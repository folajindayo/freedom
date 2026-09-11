package exchange

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"freedom/api/internal/ledger"
	"freedom/api/internal/money"
	"freedom/api/internal/share"
)

// Pre-trade risk.
//
// Freedom settles T+0 against fully prefunded accounts, so every order is
// covered before it enters the book: cash and shares move to reservation
// accounts at entry and are swapped atomically at the fill. There is no
// unsettled window, therefore no counterparty risk, therefore no clearing
// house, no netting, no margin and no guarantee fund — an entire category of
// system that only exists to manage a window we do not have.
//
// The cost is that nobody trades on credit. For a market whose clients are
// prepaid cardholders that is not a real constraint.

// ErrInsufficientFunds and friends are refusals, not failures: they are the
// normal answer to an order that cannot be covered.
var (
	ErrInsufficientFunds  = errors.New("exchange: insufficient available balance")
	ErrInsufficientShares = errors.New("exchange: insufficient transferable shares")
	ErrRelatedParty       = errors.New("exchange: related parties may not trade this instrument")
	ErrBelowMinimum       = errors.New("exchange: order is below the instrument's minimum")
)

// reserveBuy moves cash from available into the order's reserve.
//
// A limit buy can only ever clear at or below its limit, so reserving the limit
// price plus the maximum fee is sufficient and the surplus comes back at
// settlement. A cash-denominated buy reserves its notional exactly.
//
// A market buy is refused at validation rather than reserved: it has no upper
// bound, so there is no amount that could cover it. That is why cash
// denomination exists.
func reserveBuy(ctx context.Context, tx pgx.Tx, o Order, cardholder uuid.UUID,
	fees FeeSchedule, businessDate string) (money.Kobo, error) {

	var principal money.Kobo
	switch {
	case o.IsCashDenominated():
		principal = o.Notional
	case o.IsMarket():
		return 0, fmt.Errorf("exchange: a market buy cannot be reserved; size it in naira instead")
	default:
		c, err := share.CostOf(o.Qty, o.Limit)
		if err != nil {
			return 0, err
		}
		principal = c
	}
	total := principal + fees.MaxFeeFor(principal)

	available, err := ledger.NairaBalance(ctx, tx,
		ledger.Cardholder(cardholder, ledger.KindAvailable, ledger.AssetNGN))
	if err != nil {
		return 0, err
	}
	if available < total {
		return 0, fmt.Errorf("%w: %s available, %s needed", ErrInsufficientFunds, available, total)
	}

	from, err := ledger.Resolve(ctx, tx, ledger.Cardholder(cardholder, ledger.KindAvailable, ledger.AssetNGN))
	if err != nil {
		return 0, err
	}
	to, err := ledger.Resolve(ctx, tx, ledger.Cardholder(cardholder, ledger.KindOrderCashReserve, ledger.AssetNGN))
	if err != nil {
		return 0, err
	}
	if _, err := ledger.Post(ctx, tx, ledger.Tx{
		EventType:      "order.reserve_cash",
		BusinessDate:   businessDate,
		IdempotencyKey: "reserve|" + o.ID.String(),
		CorrelationID:  &o.ID,
		Entries: []ledger.Entry{
			{AccountID: from, Amount: ledger.NGN(-total), Reason: "order.reserve"},
			{AccountID: to, Amount: ledger.NGN(total), Reason: "order.reserve"},
		},
	}); err != nil && !errors.Is(err, ledger.ErrAlreadyPosted) {
		return 0, err
	}
	return total, nil
}

// lotPick is one lot earmarked for a sell order.
type lotPick struct {
	LotID uuid.UUID
	ID    int64
	Units share.Units
}

// reserveSell earmarks specific lots and moves their units into the reserve.
//
// Reserving by lot IDENTITY rather than by quantity is what makes the
// chargeback lock enforceable. A quantity check against the wallet balance
// would happily sell a lot that is still inside its 120-day lock as long as the
// account's total looked sufficient; selecting the lots means a locked lot
// cannot be reserved because it was never in the result set.
//
// FIFO within the eligible set, by acquisition then id — not by unlock date,
// which drifts from acquisition order and produces a cost basis nobody can
// reconcile against their statement.
func reserveSell(ctx context.Context, tx pgx.Tx, o Order, cardholder uuid.UUID,
	instrumentID, symbol, businessDate string) ([]lotPick, error) {

	walletRef := ledger.Cardholder(cardholder, ledger.KindStockWallet, instrumentID)
	wallet, err := ledger.Resolve(ctx, tx, walletRef)
	if err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx, `
		SELECT id, units_open - units_reserved
		  FROM holding_lots
		 WHERE account_id = $1 AND instrument_id = $2
		   AND transferable_from <= $3::date
		   AND units_open > units_reserved
		 ORDER BY acquired_at, id
		 FOR UPDATE`, wallet, instrumentID, businessDate)
	if err != nil {
		return nil, fmt.Errorf("exchange: select sellable lots: %w", err)
	}

	type candidate struct {
		id        int64
		available share.Units
	}
	var cands []candidate
	var sellable share.Units
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.available); err != nil {
			rows.Close()
			return nil, err
		}
		cands = append(cands, c)
		sellable += c.available
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if sellable < o.Qty {
		return nil, fmt.Errorf("%w: %s transferable, %s requested", ErrInsufficientShares, sellable, o.Qty)
	}

	var picks []lotPick
	remaining := o.Qty
	for _, c := range cands {
		if remaining <= 0 {
			break
		}
		take := c.available
		if take > remaining {
			take = remaining
		}
		if _, err := tx.Exec(ctx, `
			UPDATE holding_lots SET units_reserved = units_reserved + $2 WHERE id = $1`,
			c.id, int64(take)); err != nil {
			return nil, fmt.Errorf("exchange: reserve lot %d: %w", c.id, err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO order_lot_reservations (order_id, lot_id, units) VALUES ($1,$2,$3)
			ON CONFLICT (order_id, lot_id) DO UPDATE SET units = order_lot_reservations.units + EXCLUDED.units`,
			o.ID, c.id, int64(take)); err != nil {
			return nil, fmt.Errorf("exchange: record lot reservation: %w", err)
		}
		picks = append(picks, lotPick{ID: c.id, Units: take})
		remaining -= take
	}

	to, err := ledger.Resolve(ctx, tx, ledger.Cardholder(cardholder, ledger.KindOrderShareReserve, instrumentID))
	if err != nil {
		return nil, err
	}
	if _, err := ledger.Post(ctx, tx, ledger.Tx{
		EventType:      "order.reserve_shares",
		BusinessDate:   businessDate,
		IdempotencyKey: "reserve|" + o.ID.String(),
		CorrelationID:  &o.ID,
		Entries: []ledger.Entry{
			{AccountID: wallet, Amount: ledger.Equity(symbol, -o.Qty), Reason: "order.reserve"},
			{AccountID: to, Amount: ledger.Equity(symbol, o.Qty), Reason: "order.reserve"},
		},
	}); err != nil && !errors.Is(err, ledger.ErrAlreadyPosted) {
		return nil, err
	}
	return picks, nil
}

// Sellable returns the units an account may actually sell today.
//
// This is strictly less than the ledger's stock_wallet balance whenever any lot
// is still inside its chargeback lock, and the difference is the whole point:
// a fraudster must not be able to tap, receive equity, sell it and charge the
// tap back.
func Sellable(ctx context.Context, q ledger.Querier, cardholder uuid.UUID,
	instrumentID, businessDate string) (share.Units, error) {
	var v int64
	err := q.QueryRow(ctx, `
		SELECT COALESCE(SUM(l.units_open - l.units_reserved), 0)
		  FROM holding_lots l JOIN accounts a ON a.id = l.account_id
		 WHERE a.owner_type = 'cardholder' AND a.owner_id = $1
		   AND a.kind = 'stock_wallet' AND l.instrument_id = $2
		   AND l.transferable_from <= $3::date`,
		cardholder, instrumentID, businessDate).Scan(&v)
	return share.Units(v), err
}

// assertNotRelatedParty refuses an order from the issuer, its treasury, its
// directors, staff, affiliates, or the merchant itself.
//
// On an auction-only symbol the book is thin enough that one participant can
// set the price, and the issuer is the party with both the motive and the
// treasury to do it. Their legitimate channels — the treasury release that
// funds the buyback, and a bid of last resort — are price takers by
// construction and never enter the auction that sets the price.
func assertNotRelatedParty(ctx context.Context, tx pgx.Tx, instrumentID string, accountID uuid.UUID,
	businessDate string) (ownerKey string, err error) {
	var key string
	err = tx.QueryRow(ctx, `
		SELECT group_key FROM related_parties
		 WHERE instrument_id = $1 AND account_id = $2 AND effective @> $3::date
		 LIMIT 1`, instrumentID, accountID, businessDate).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("exchange: related-party check: %w", err)
	}
	return key, fmt.Errorf("%w (%s)", ErrRelatedParty, key)
}
