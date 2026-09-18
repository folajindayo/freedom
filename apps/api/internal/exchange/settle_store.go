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

// releaseUnfilled returns reservations held by orders that did not fill, or did
// not fill completely.
//
// The invariant this exists to hold: after a session is published, every
// participant's reserve balances are exactly zero. A reservation left behind is
// a cardholder's money stranded in an account they cannot see.
func (e *Engine) releaseUnfilled(ctx context.Context, tx pgx.Tx, s *Session, book []Order,
	index map[int64]bookOrder, r Result) ([]ledger.Entry, error) {

	filled := map[int64]share.Units{}
	for _, f := range r.Fills {
		filled[f.Seq] += f.Units
	}

	var entries []ledger.Entry
	for _, o := range book {
		if _, traded := filled[o.Seq]; traded {
			continue // handled in settle, which knows the consideration
		}
		bo := index[o.Seq]
		if o.Side == Buy {
			if bo.ReservedKobo == 0 {
				continue
			}
			reserve, err := ledger.Resolve(ctx, tx,
				ledger.Cardholder(bo.CardholderID, ledger.KindOrderCashReserve, ledger.AssetNGN))
			if err != nil {
				return nil, err
			}
			avail, err := ledger.Resolve(ctx, tx,
				ledger.Cardholder(bo.CardholderID, ledger.KindAvailable, ledger.AssetNGN))
			if err != nil {
				return nil, err
			}
			entries = append(entries,
				ledger.Entry{AccountID: reserve, Amount: ledger.NGN(-bo.ReservedKobo), Reason: "auction.unfilled_release"},
				ledger.Entry{AccountID: avail, Amount: ledger.NGN(bo.ReservedKobo), Reason: "auction.unfilled_release"})
			continue
		}

		if bo.Qty == 0 {
			continue
		}
		reserve, err := ledger.Resolve(ctx, tx,
			ledger.Cardholder(bo.CardholderID, ledger.KindOrderShareReserve, s.InstrumentID))
		if err != nil {
			return nil, err
		}
		wallet, err := ledger.Resolve(ctx, tx,
			ledger.Cardholder(bo.CardholderID, ledger.KindStockWallet, s.InstrumentID))
		if err != nil {
			return nil, err
		}
		entries = append(entries,
			ledger.Entry{AccountID: reserve, Amount: ledger.Equity(s.Symbol, -bo.Qty), Reason: "auction.unfilled_release"},
			ledger.Entry{AccountID: wallet, Amount: ledger.Equity(s.Symbol, bo.Qty), Reason: "auction.unfilled_release"})
	}

	// Lot reservations come back too, whether or not the order traded.
	if _, err := tx.Exec(ctx, `
		UPDATE holding_lots l
		   SET units_reserved = l.units_reserved - x.units
		  FROM (SELECT r.lot_id, SUM(r.units) AS units
		          FROM order_lot_reservations r JOIN orders o ON o.id = r.order_id
		         WHERE o.auction_id = $1 AND NOT r.released
		         GROUP BY r.lot_id) x
		 WHERE l.id = x.lot_id`, s.ID); err != nil {
		return nil, fmt.Errorf("exchange: release lot reservations: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE order_lot_reservations r SET released = true
		  FROM orders o WHERE o.id = r.order_id AND o.auction_id = $1`, s.ID); err != nil {
		return nil, err
	}
	return entries, nil
}

func recordFill(ctx context.Context, tx pgx.Tx, s *Session, bo bookOrder, f Fill,
	price, consideration, fee money.Kobo, txID uuid.UUID) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx, `
		INSERT INTO fills (instrument_id, venue, auction_id, order_id, side, units,
		                   price_kobo, consideration_kobo, fee_kobo, ledger_tx_id, session_date)
		VALUES ($1,'auction',$2,$3,$4,$5,$6,$7,$8,$9,$10::date)
		ON CONFLICT (auction_id, order_id) DO UPDATE SET units = EXCLUDED.units
		RETURNING id`,
		s.InstrumentID, s.ID, bo.OrderID, string(f.Side), int64(f.Units),
		int64(price), int64(consideration), int64(fee), txID, s.SessionDate).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("exchange: record fill: %w", err)
	}
	return id, nil
}

// openLot records equity bought on the exchange.
//
// Its cost basis is the consideration plus the fee — what the buyer actually
// paid to own it — and it is transferable immediately: unlike a buyback
// allocation, nothing funded it that could later be charged back.
func openLot(ctx context.Context, tx pgx.Tx, s *Session, bo bookOrder,
	units share.Units, cost money.Kobo, txID uuid.UUID) error {
	wallet, err := ledger.Resolve(ctx, tx,
		ledger.Cardholder(bo.CardholderID, ledger.KindStockWallet, s.InstrumentID))
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO holding_lots (account_id, instrument_id, units, units_open,
		                          cost_kobo, cost_open_kobo, transferable_from, ledger_tx_id)
		VALUES ($1,$2,$3,$3,$4,$4,$5::date,$6)`,
		wallet, s.InstrumentID, int64(units), int64(cost), s.SessionDate, txID)
	if err != nil {
		return fmt.Errorf("exchange: open lot: %w", err)
	}
	return nil
}

// disposeLots consumes the lots a sell reserved, oldest first, and records the
// basis and realised gain of each.
//
// The basis of a partial disposal is apportioned with largest remainder rather
// than by multiplying a ratio, so that consuming a lot completely leaves its
// open basis at exactly zero instead of a stray kobo that accumulates across a
// year of partial sales.
//
// Realised P/L is derived here and never posted to the ledger: the ledger has
// already moved the cash, and a "realised gains" account would count it twice.
func disposeLots(ctx context.Context, tx pgx.Tx, bo bookOrder, units share.Units,
	proceeds, fee money.Kobo, fillID int64) error {

	rows, err := tx.Query(ctx, `
		SELECT r.lot_id, r.units, l.units_open, l.cost_open_kobo
		  FROM order_lot_reservations r JOIN holding_lots l ON l.id = r.lot_id
		 WHERE r.order_id = $1
		 ORDER BY l.acquired_at, l.id`, bo.OrderID)
	if err != nil {
		return fmt.Errorf("exchange: load reserved lots: %w", err)
	}
	type lotRow struct {
		id        int64
		reserved  share.Units
		open      share.Units
		costOpen  money.Kobo
		consuming share.Units
	}
	var lots []lotRow
	for rows.Next() {
		var l lotRow
		if err := rows.Scan(&l.id, &l.reserved, &l.open, &l.costOpen); err != nil {
			rows.Close()
			return err
		}
		lots = append(lots, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	remaining := units
	for i := range lots {
		if remaining <= 0 {
			break
		}
		take := lots[i].reserved
		if take > remaining {
			take = remaining
		}
		lots[i].consuming = take
		remaining -= take
	}
	if remaining > 0 {
		return fmt.Errorf("exchange: sold %s but only %s was reserved across lots", units, units-remaining)
	}

	weights := make([]int64, len(lots))
	for i, l := range lots {
		weights[i] = int64(l.consuming)
	}
	proceedShare, err := alloc.Proportional(proceeds, weights)
	if err != nil {
		return err
	}
	feeShare, err := alloc.Proportional(fee, weights)
	if err != nil {
		return err
	}

	for i, l := range lots {
		if l.consuming == 0 {
			continue
		}
		basisParts, err := alloc.Proportional(l.costOpen,
			[]int64{int64(l.consuming), int64(l.open - l.consuming)})
		if err != nil {
			return err
		}
		basis := basisParts[0]

		if _, err := tx.Exec(ctx, `
			UPDATE holding_lots
			   SET units_open = units_open - $2, cost_open_kobo = cost_open_kobo - $3
			 WHERE id = $1`, l.id, int64(l.consuming), int64(basis)); err != nil {
			return fmt.Errorf("exchange: consume lot %d: %w", l.id, err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO lot_disposals (fill_id, lot_id, units, basis_kobo, proceeds_kobo, fee_kobo, realised_kobo)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT (fill_id, lot_id) DO NOTHING`,
			fillID, l.id, int64(l.consuming), int64(basis),
			int64(proceedShare[i]), int64(feeShare[i]),
			int64(proceedShare[i]-basis-feeShare[i])); err != nil {
			return fmt.Errorf("exchange: record disposal: %w", err)
		}
	}
	return nil
}

func markOrdersSettled(ctx context.Context, tx pgx.Tx, s *Session, r Result) error {
	filled := map[int64]share.Units{}
	for _, f := range r.Fills {
		filled[f.Seq] += f.Units
	}
	for seq, units := range filled {
		if _, err := tx.Exec(ctx, `
			UPDATE orders SET filled_units = $3,
			       state = CASE WHEN $3 >= COALESCE(qty_units, $3) THEN 'filled' ELSE 'partial' END
			 WHERE auction_id = $1 AND auction_seq = $2`, s.ID, seq, int64(units)); err != nil {
			return err
		}
	}
	// Auction orders are DAY by construction, so anything unfilled expires with
	// the session rather than resting into tomorrow with a live reservation.
	if _, err := tx.Exec(ctx, `
		UPDATE orders SET state = 'expired'
		 WHERE auction_id = $1 AND state = 'open'`, s.ID); err != nil {
		return err
	}
	return nil
}

// publish records the discovered price and closes the session.
func (e *Engine) publish(ctx context.Context, tx pgx.Tx, s *Session, r Result, resultHash string) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO price_observations (instrument_id, obs_date, source, price_kobo,
		                                volume_units, session_vwap_kobo, trade_count,
		                                auction_id, content_hash)
		VALUES ($1,$2::date,'auction',$3,$4,$3,$5,$6,$7)
		ON CONFLICT (instrument_id, obs_date, source, content_hash) DO NOTHING`,
		s.InstrumentID, s.SessionDate, int64(r.Price), int64(r.Exec),
		len(r.Fills), s.ID, resultHash); err != nil {
		return fmt.Errorf("exchange: publish price: %w", err)
	}

	// The discovered price becomes the reference, and the staleness counter
	// resets because a real trade happened.
	if _, err := tx.Exec(ctx, `
		UPDATE instruments SET reference_price_kobo = $2, carry_forward_sessions = 0
		 WHERE id = $1`, s.InstrumentID, int64(r.Price)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE auctions SET state = 'published', publishes_at = now() WHERE id = $1`, s.ID); err != nil {
		return fmt.Errorf("exchange: publish session: %w", err)
	}
	s.State = "published"
	return nil
}

// publishCarryForward closes a session in which nothing crossed.
//
// Two outcomes. When a designated market maker's firm two-sided quote stood
// in the frozen book, the session publishes its mid: a price somebody named
// and was obliged to trade at, so the reference moves to it and the
// staleness counter resets. Otherwise the reference is carried forward and
// the counter increments, so that a symbol nobody has quoted or traded for
// weeks is visibly stale rather than quietly pricing a buyback off a number
// from another month. The clearing price stays NULL either way: nothing
// cleared.
//
// The quote is read from lp_performance, which the close writes by calling
// MeasureSession before it settles. A caller that settles without measuring
// gets the carry-forward.
func (e *Engine) publishCarryForward(ctx context.Context, tx pgx.Tx, s *Session, resultHash string) error {
	book, index, err := e.loadBook(ctx, tx, s)
	if err != nil {
		return err
	}

	mid, err := firmQuoteMid(ctx, tx, s, index)
	if err != nil {
		return err
	}
	if mid > 0 {
		if _, err := tx.Exec(ctx, `
			INSERT INTO price_observations (instrument_id, obs_date, source, price_kobo,
			                                volume_units, trade_count, auction_id, content_hash)
			VALUES ($1,$2::date,'quote',$3,0,0,$4,$5)
			ON CONFLICT (instrument_id, obs_date, source, content_hash) DO NOTHING`,
			s.InstrumentID, s.SessionDate, int64(mid), s.ID, resultHash); err != nil {
			return fmt.Errorf("exchange: publish quote: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE instruments SET reference_price_kobo = $2, carry_forward_sessions = 0
			 WHERE id = $1`, s.InstrumentID, int64(mid)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE auctions SET rule = 'quote' WHERE id = $1`, s.ID); err != nil {
			return err
		}
	} else {
		if _, err := tx.Exec(ctx, `
			INSERT INTO price_observations (instrument_id, obs_date, source, price_kobo,
			                                volume_units, trade_count, auction_id, content_hash)
			VALUES ($1,$2::date,'carry_forward',$3,0,0,$4,$5)
			ON CONFLICT (instrument_id, obs_date, source, content_hash) DO NOTHING`,
			s.InstrumentID, s.SessionDate, int64(s.Params.PrevRef), s.ID, resultHash); err != nil {
			return fmt.Errorf("exchange: carry forward: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE instruments SET carry_forward_sessions = carry_forward_sessions + 1
			 WHERE id = $1`, s.InstrumentID); err != nil {
			return err
		}
	}

	entries, err := e.releaseUnfilled(ctx, tx, s, book, index, Result{})
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		if _, err := ledger.Post(ctx, tx, ledger.Tx{
			EventType:      "auction.no_trade",
			BusinessDate:   s.SessionDate,
			IdempotencyKey: "auction|" + s.ID.String(),
			CorrelationID:  &s.ID,
			Entries:        entries,
		}); err != nil && !isAlreadyPosted(err) {
			return err
		}
	}
	if err := markOrdersSettled(ctx, tx, s, Result{}); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE auctions SET state = 'published', publishes_at = now() WHERE id = $1`, s.ID); err != nil {
		return err
	}
	s.State = "published"
	return nil
}

func isAlreadyPosted(err error) bool {
	return errors.Is(err, ledger.ErrAlreadyPosted)
}
