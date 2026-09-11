package exchange

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"freedom/api/internal/alloc"
	"freedom/api/internal/ledger"
	"freedom/api/internal/money"
	"freedom/api/internal/share"
)

// bookOrder is an order as loaded for crossing, with the identity the engine
// deliberately does not carry.
type bookOrder struct {
	Order
	OrderID      uuid.UUID
	CardholderID uuid.UUID
	ReservedKobo money.Kobo
}

func (e *Engine) loadBook(ctx context.Context, tx pgx.Tx, s *Session) ([]Order, map[int64]bookOrder, error) {
	rows, err := tx.Query(ctx, `
		SELECT o.id, o.auction_seq, o.side, o.type, o.limit_kobo, o.qty_units,
		       o.notional_kobo, o.reserved_kobo, o.owner_key, a.owner_id
		  FROM orders o JOIN accounts a ON a.id = o.account_id
		 WHERE o.auction_id = $1 AND o.state = 'open'
		 ORDER BY o.auction_seq`, s.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("exchange: load book: %w", err)
	}
	defer rows.Close()

	var book []Order
	index := map[int64]bookOrder{}
	for rows.Next() {
		var bo bookOrder
		var side string
		var limit, qty, notional *int64
		var ownerKey *string
		if err := rows.Scan(&bo.OrderID, &bo.Seq, &side, &bo.Type, &limit, &qty,
			&notional, &bo.ReservedKobo, &ownerKey, &bo.CardholderID); err != nil {
			return nil, nil, err
		}
		bo.ID = bo.OrderID
		bo.Side = Side(side)
		if limit != nil {
			bo.Limit = money.Kobo(*limit)
		}
		if qty != nil {
			bo.Qty = share.Units(*qty)
		}
		if notional != nil {
			bo.Notional = money.Kobo(*notional)
		}
		if ownerKey != nil {
			bo.OwnerKey = *ownerKey
		}
		book = append(book, bo.Order)
		index[bo.Seq] = bo
	}
	return book, index, rows.Err()
}

// settle posts the session's fills and releases every reservation.
//
// # One ledger transaction for the whole session
//
// Not one per fill. The balance trigger fires once per header rather than once
// per entry, the session becomes atomic — there is no such thing as half an
// uncross — and the idempotency key is natural, so a re-run completes the
// session instead of double-posting it.
//
// # Cash conservation
//
// There is no buyer-and-seller pair in a call auction: there is a set of buys
// totalling Exec and a set of sells totalling Exec. Computing each side's
// consideration independently with share.CostOf — which rounds up — makes
// buyers pay more than sellers receive, by up to a kobo each, and the deferred
// balance trigger then rejects the entire session at commit, after all the work
// is done. So there is ONE pool consideration, rounded once, and both sides are
// pro-rated out of it with largest remainder.
func (e *Engine) settle(ctx context.Context, tx pgx.Tx, s *Session, book []Order,
	index map[int64]bookOrder, r Result, resultHash string) error {

	if !r.Determined {
		return e.publishCarryForward(ctx, tx, s, resultHash)
	}

	pool, err := share.CostOf(r.Exec, r.Price)
	if err != nil {
		return err
	}

	var buys, sells []Fill
	for _, f := range r.Fills {
		if f.Side == Buy {
			buys = append(buys, f)
		} else {
			sells = append(sells, f)
		}
	}

	buyShares, err := considerations(pool, buys)
	if err != nil {
		return err
	}
	sellShares, err := considerations(pool, sells)
	if err != nil {
		return err
	}

	var entries []ledger.Entry
	feeAcct, err := ledger.Resolve(ctx, tx, ledger.Scheme(ledger.KindExchangeFeeIncome, ledger.AssetNGN))
	if err != nil {
		return err
	}
	var feeTotal money.Kobo

	type pending struct {
		fill          Fill
		consideration money.Kobo
		fee           money.Kobo
	}
	var toRecord []pending

	// Buyers: consume consideration and fee out of the reservation, and hand
	// back whatever the reservation over-covered. A limit buy that cleared
	// below its limit gets the difference returned here.
	for i, f := range buys {
		bo := index[f.Seq]
		n := buyShares[i]
		fee := e.Fees.Fee(n)
		reserve, err := ledger.Resolve(ctx, tx,
			ledger.Cardholder(bo.CardholderID, ledger.KindOrderCashReserve, ledger.AssetNGN))
		if err != nil {
			return err
		}
		avail, err := ledger.Resolve(ctx, tx,
			ledger.Cardholder(bo.CardholderID, ledger.KindAvailable, ledger.AssetNGN))
		if err != nil {
			return err
		}
		wallet, err := ledger.Resolve(ctx, tx,
			ledger.Cardholder(bo.CardholderID, ledger.KindStockWallet, s.InstrumentID))
		if err != nil {
			return err
		}

		entries = append(entries,
			ledger.Entry{AccountID: reserve, Amount: ledger.NGN(-bo.ReservedKobo), Reason: "auction.reserve_release"},
			ledger.Entry{AccountID: wallet, Amount: ledger.Equity(s.Symbol, f.Units), Reason: "auction.receive"})
		if refund := bo.ReservedKobo - n - fee; refund != 0 {
			entries = append(entries,
				ledger.Entry{AccountID: avail, Amount: ledger.NGN(refund), Reason: "auction.reserve_refund"})
		}
		feeTotal += fee
		toRecord = append(toRecord, pending{f, n, fee})
	}

	// Sellers: deliver the units, release any unfilled remainder back to the
	// wallet, and receive proceeds net of fee.
	for i, f := range sells {
		bo := index[f.Seq]
		g := sellShares[i]
		fee := e.Fees.FeeOnSale(g)
		reserve, err := ledger.Resolve(ctx, tx,
			ledger.Cardholder(bo.CardholderID, ledger.KindOrderShareReserve, s.InstrumentID))
		if err != nil {
			return err
		}
		wallet, err := ledger.Resolve(ctx, tx,
			ledger.Cardholder(bo.CardholderID, ledger.KindStockWallet, s.InstrumentID))
		if err != nil {
			return err
		}
		avail, err := ledger.Resolve(ctx, tx,
			ledger.Cardholder(bo.CardholderID, ledger.KindAvailable, ledger.AssetNGN))
		if err != nil {
			return err
		}

		entries = append(entries,
			ledger.Entry{AccountID: reserve, Amount: ledger.Equity(s.Symbol, -bo.Qty), Reason: "auction.deliver"},
			ledger.Entry{AccountID: avail, Amount: ledger.NGN(g - fee), Reason: "auction.sell_proceeds_net"})
		if residual := bo.Qty - f.Units; residual > 0 {
			entries = append(entries,
				ledger.Entry{AccountID: wallet, Amount: ledger.Equity(s.Symbol, residual), Reason: "auction.reserve_refund"})
		}
		feeTotal += fee
		toRecord = append(toRecord, pending{f, g, fee})
	}

	if feeTotal > 0 {
		entries = append(entries,
			ledger.Entry{AccountID: feeAcct, Amount: ledger.NGN(feeTotal), Reason: "auction.fee"})
	}

	// Unfilled orders still hold reservations; release them here so that after
	// a published session every reserve balance is exactly zero.
	rel, err := e.releaseUnfilled(ctx, tx, s, book, index, r)
	if err != nil {
		return err
	}
	entries = append(entries, rel...)

	txID, err := ledger.Post(ctx, tx, ledger.Tx{
		EventType: "auction.settled",
		// The session's own date, never the wall clock: a session that froze at
		// 14:00 and settles at 21:00 would otherwise land on tomorrow, because
		// the scheme's cutover is 20:00.
		BusinessDate:   s.SessionDate,
		IdempotencyKey: "auction|" + s.ID.String(),
		CorrelationID:  &s.ID,
		Entries:        entries,
	})
	if err != nil {
		if errors.Is(err, ledger.ErrAlreadyPosted) {
			return nil // a re-run; the session already settled
		}
		return err
	}

	for _, p := range toRecord {
		bo := index[p.fill.Seq]
		fillID, err := recordFill(ctx, tx, s, bo, p.fill, r.Price, p.consideration, p.fee, txID)
		if err != nil {
			return err
		}
		if p.fill.Side == Sell {
			if err := disposeLots(ctx, tx, bo, p.fill.Units, p.consideration, p.fee, fillID); err != nil {
				return err
			}
		} else {
			if err := openLot(ctx, tx, s, bo, p.fill.Units, p.consideration+p.fee, txID); err != nil {
				return err
			}
		}
	}

	if err := markOrdersSettled(ctx, tx, s, r); err != nil {
		return err
	}
	return e.publish(ctx, tx, s, r, resultHash)
}

// considerations splits the pool across one side's fills, in proportion to
// units, so that the two sides sum to exactly the same total.
func considerations(pool money.Kobo, fills []Fill) ([]money.Kobo, error) {
	if len(fills) == 0 {
		return nil, nil
	}
	weights := make([]int64, len(fills))
	for i, f := range fills {
		weights[i] = int64(f.Units)
	}
	parts, err := alloc.Proportional(pool, weights)
	if err != nil {
		return nil, fmt.Errorf("exchange: splitting consideration: %w", err)
	}
	return parts, nil
}
