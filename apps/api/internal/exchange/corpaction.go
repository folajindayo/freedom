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

// Corporate actions.
//
// Three that actually happen at Nigerian SMEs: splits, bonus issues, and cash
// dividends. Each is one balanced ledger transaction, applied at the ex-date
// session open, against a holder set snapshotted at the record date.
//
// Every one of them makes prices from before incomparable with prices after,
// and that is not merely cosmetic: the buyback's trailing-VWAP cap reads price
// history, so an unadjusted split would disable the network's main defence
// against a listing ramping its own price. Applying an action therefore always
// writes an adjustment factor, in the same transaction.

// Action kinds.
const (
	ActionSplit     = "split"
	ActionBonus     = "bonus"
	ActionDividend  = "cash_dividend"
	ActionReturnCap = "return_of_capital"
)

// ErrNotDue is returned when an action is applied out of order.
var ErrNotDue = errors.New("exchange: corporate action is not due")

// Action is a declared corporate action.
type Action struct {
	ID           uuid.UUID
	InstrumentID string
	Symbol       string
	CompanyID    uuid.UUID
	Kind         string

	// RatioNum:RatioDen is "num shares after for every den before". A 2:1 split
	// is 2/1; a one-for-four bonus issue is 5/4, because a holder of four ends
	// up with five.
	RatioNum, RatioDen int64

	DPS    money.Kobo
	WHTBps int64

	ExDate, RecordDate, PayDate string
	State                       string
}

// Declare records an action. Nothing moves until TakeRecord and Apply.
func Declare(ctx context.Context, tx pgx.Tx, a Action) (uuid.UUID, error) {
	if err := a.validate(); err != nil {
		return uuid.Nil, err
	}
	var id uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO corporate_actions
		  (instrument_id, kind, ratio_num, ratio_den, dps_kobo, wht_bps,
		   announced_on, ex_date, record_date, pay_date)
		VALUES ($1,$2,$3,$4,$5,$6,now()::date,$7::date,$8::date,$9::date)
		RETURNING id`,
		a.InstrumentID, a.Kind, nullableInt(a.RatioNum), nullableInt(a.RatioDen),
		nullableKobo(a.DPS), a.WHTBps, a.ExDate, a.RecordDate, nullableString(a.PayDate)).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("exchange: declare %s: %w", a.Kind, err)
	}
	return id, nil
}

func (a Action) validate() error {
	switch a.Kind {
	case ActionSplit, ActionBonus:
		if a.RatioNum <= 0 || a.RatioDen <= 0 {
			return fmt.Errorf("exchange: %s needs a positive ratio, got %d:%d", a.Kind, a.RatioNum, a.RatioDen)
		}
		if a.RatioNum == a.RatioDen {
			return fmt.Errorf("exchange: a 1:1 %s changes nothing", a.Kind)
		}
	case ActionDividend, ActionReturnCap:
		if a.DPS <= 0 {
			return fmt.Errorf("exchange: %s needs a positive amount per share", a.Kind)
		}
		if a.WHTBps < 0 || a.WHTBps > 10_000 {
			return fmt.Errorf("exchange: withholding of %d bps is not a rate", a.WHTBps)
		}
	default:
		return fmt.Errorf("exchange: unknown corporate action %q", a.Kind)
	}
	return nil
}

// holding is one account's position at the record date.
type holding struct {
	AccountID uuid.UUID
	Units     share.Units
}

// TakeRecord snapshots who holds what.
//
// Reserved units count: a holder who has an open sell order still owns the
// shares until they trade, and a dividend that skipped them because they were
// mid-order would be indefensible.
func TakeRecord(ctx context.Context, tx pgx.Tx, actionID uuid.UUID) (int, error) {
	a, err := loadAction(ctx, tx, actionID)
	if err != nil {
		return 0, err
	}
	if a.State != "announced" {
		return 0, fmt.Errorf("%w: %s is %s", ErrNotDue, a.Kind, a.State)
	}

	rows, err := tx.Query(ctx, `
		SELECT a.id, COALESCE(SUM(e.amount), 0)::bigint
		  FROM accounts a JOIN ledger_entries e ON e.account_id = a.id
		 WHERE a.asset_id = $1
		   AND a.kind IN ('stock_wallet','treasury','order_share_reserve')
		 GROUP BY a.id
		HAVING COALESCE(SUM(e.amount), 0) > 0`, a.InstrumentID)
	if err != nil {
		return 0, fmt.Errorf("exchange: snapshot holders: %w", err)
	}
	var holders []holding
	for rows.Next() {
		var h holding
		if err := rows.Scan(&h.AccountID, &h.Units); err != nil {
			rows.Close()
			return 0, err
		}
		holders = append(holders, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for _, h := range holders {
		if _, err := tx.Exec(ctx, `
			INSERT INTO corporate_action_entitlements (action_id, account_id, units_held)
			VALUES ($1,$2,$3) ON CONFLICT (action_id, account_id) DO UPDATE
			  SET units_held = EXCLUDED.units_held`,
			actionID, h.AccountID, int64(h.Units)); err != nil {
			return 0, fmt.Errorf("exchange: record entitlement: %w", err)
		}
	}
	if _, err := tx.Exec(ctx,
		`UPDATE corporate_actions SET state = 'record_taken' WHERE id = $1`, actionID); err != nil {
		return 0, err
	}
	return len(holders), nil
}

func loadAction(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Action, error) {
	var a Action
	var num, den, dps *int64
	var pay *string
	err := tx.QueryRow(ctx, `
		SELECT ca.id, ca.instrument_id, i.symbol, i.company_id, ca.kind,
		       ca.ratio_num, ca.ratio_den, ca.dps_kobo, ca.wht_bps,
		       ca.ex_date::text, ca.record_date::text, ca.pay_date::text, ca.state
		  FROM corporate_actions ca JOIN instruments i ON i.id = ca.instrument_id
		 WHERE ca.id = $1`, id).
		Scan(&a.ID, &a.InstrumentID, &a.Symbol, &a.CompanyID, &a.Kind,
			&num, &den, &dps, &a.WHTBps, &a.ExDate, &a.RecordDate, &pay, &a.State)
	if err != nil {
		return Action{}, fmt.Errorf("exchange: load corporate action: %w", err)
	}
	if num != nil {
		a.RatioNum, a.RatioDen = *num, *den
	}
	if dps != nil {
		a.DPS = money.Kobo(*dps)
	}
	if pay != nil {
		a.PayDate = *pay
	}
	return a, nil
}

func nullableInt(v int64) *int64 {
	if v == 0 {
		return nil
	}
	return &v
}

var _ = alloc.Proportional[share.Units]
var _ = ledger.AssetNGN
