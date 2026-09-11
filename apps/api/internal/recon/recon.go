// Package recon finds the places where two records of the same fact disagree.
//
// Freedom is correct today because its tests say so, not because anything would
// notice if it stopped being. This is the thing that notices.
//
// Every check below compares two independently maintained representations of
// one truth — the ledger against the lot register, fills against their
// postings, reservations against the orders holding them. They are maintained
// by different code paths on purpose, because a check that reads the same row
// twice proves nothing.
//
// Breaks are recorded in BOTH directions: present here and missing there, and
// the reverse. A one-directional check finds half the breaks and is worse than
// no check at all, because it looks like coverage.
package recon

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Kinds of reconciliation.
const (
	KindLedgerVsLots  = "ledger_vs_lots"
	KindFillsVsLedger = "fills_vs_ledger"
	KindReserves      = "reserves"
	KindTreasury      = "treasury"
	KindCashRail      = "cash_rail"
)

// Break is one disagreement.
type Break struct {
	Kind     string
	Subject  string
	Expected *int64
	Actual   *int64
	Detail   map[string]any
}

// Run is the outcome of one reconciliation.
type Run struct {
	ID      uuid.UUID
	Kind    string
	Checked int
	Breaks  []Break
}

// Clean reports whether the run found nothing.
func (r Run) Clean() bool { return len(r.Breaks) == 0 }

// RunAll executes every reconciliation for a business date.
func RunAll(ctx context.Context, tx pgx.Tx, businessDate string) ([]Run, error) {
	var out []Run
	for _, fn := range []func(context.Context, pgx.Tx, string) (Run, error){
		LedgerVsLots, FillsVsLedger, Reserves, Treasury,
	} {
		r, err := fn(ctx, tx, businessDate)
		if err != nil {
			return out, err
		}
		if err := record(ctx, tx, businessDate, &r); err != nil {
			return out, err
		}
		out = append(out, r)
	}
	return out, nil
}

// LedgerVsLots checks that the share registry and the lot register agree.
//
// These are the two truths most likely to drift: the ledger is written by the
// posting path and the lots by the disposal and corporate-action paths, and a
// bug in either shows up here before it shows up in a customer's statement.
//
// Wallet balance must equal open lot units plus whatever is reserved against
// open orders.
func LedgerVsLots(ctx context.Context, tx pgx.Tx, businessDate string) (Run, error) {
	r := Run{Kind: KindLedgerVsLots}
	rows, err := tx.Query(ctx, `
		WITH wallet AS (
		  SELECT a.id, a.asset_id, a.owner_id, COALESCE(SUM(e.amount), 0)::bigint AS balance
		    FROM accounts a LEFT JOIN ledger_entries e ON e.account_id = a.id
		   WHERE a.kind = 'stock_wallet'
		   GROUP BY a.id),
		lots AS (
		  SELECT account_id, instrument_id, COALESCE(SUM(units_open), 0)::bigint AS units
		    FROM holding_lots GROUP BY account_id, instrument_id),
		reserved AS (
		  SELECT a.owner_id, a.asset_id, COALESCE(SUM(e.amount), 0)::bigint AS units
		    FROM accounts a LEFT JOIN ledger_entries e ON e.account_id = a.id
		   WHERE a.kind = 'order_share_reserve'
		   GROUP BY a.owner_id, a.asset_id)
		SELECT w.id::text, w.asset_id, w.balance,
		       COALESCE(l.units, 0), COALESCE(rs.units, 0)
		  FROM wallet w
		  LEFT JOIN lots l      ON l.account_id = w.id AND l.instrument_id = w.asset_id
		  LEFT JOIN reserved rs ON rs.owner_id = w.owner_id AND rs.asset_id = w.asset_id
		 WHERE w.balance <> 0 OR COALESCE(l.units,0) <> 0`)
	if err != nil {
		return r, fmt.Errorf("recon: ledger vs lots: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var acct, asset string
		var balance, lots, reserved int64
		if err := rows.Scan(&acct, &asset, &balance, &lots, &reserved); err != nil {
			return r, err
		}
		r.Checked++
		// The wallet holds what is neither reserved nor disposed; lots record
		// everything still open, reserved or not.
		if want := lots - reserved; balance != want {
			exp, act := want, balance
			r.Breaks = append(r.Breaks, Break{
				Kind: KindLedgerVsLots, Subject: acct + " " + asset,
				Expected: &exp, Actual: &act,
				Detail: map[string]any{"lots_open": lots, "reserved": reserved},
			})
		}
	}
	return r, rows.Err()
}

// FillsVsLedger checks that every fill has a posting behind it, and that every
// posting has a fill.
//
// The second direction is the one that matters: a fill with no ledger entry is
// a trade that never settled, and a settlement with no fill is money that moved
// for no recorded reason.
func FillsVsLedger(ctx context.Context, tx pgx.Tx, businessDate string) (Run, error) {
	r := Run{Kind: KindFillsVsLedger}

	rows, err := tx.Query(ctx, `
		SELECT f.id::text, f.ledger_tx_id::text
		  FROM fills f
		  LEFT JOIN ledger_tx t ON t.id = f.ledger_tx_id
		 WHERE f.session_date = $1::date AND t.id IS NULL`, businessDate)
	if err != nil {
		return r, fmt.Errorf("recon: fills without postings: %w", err)
	}
	for rows.Next() {
		var fill, txID string
		if err := rows.Scan(&fill, &txID); err != nil {
			rows.Close()
			return r, err
		}
		r.Breaks = append(r.Breaks, Break{
			Kind: KindFillsVsLedger, Subject: "fill " + fill,
			Detail: map[string]any{"missing": "ledger transaction", "ledger_tx_id": txID},
		})
	}
	rows.Close()

	// And the other way: a settlement transaction with no fills behind it.
	rows, err = tx.Query(ctx, `
		SELECT t.id::text, t.event_type
		  FROM ledger_tx t
		  LEFT JOIN fills f ON f.ledger_tx_id = t.id
		 WHERE t.business_date = $1::date AND t.event_type = 'auction.settled'
		   AND f.id IS NULL`, businessDate)
	if err != nil {
		return r, fmt.Errorf("recon: postings without fills: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var txID, kind string
		if err := rows.Scan(&txID, &kind); err != nil {
			return r, err
		}
		r.Breaks = append(r.Breaks, Break{
			Kind: KindFillsVsLedger, Subject: "ledger_tx " + txID,
			Detail: map[string]any{"missing": "fills", "event_type": kind},
		})
	}

	if err := tx.QueryRow(ctx,
		`SELECT COUNT(*) FROM fills WHERE session_date = $1::date`, businessDate).
		Scan(&r.Checked); err != nil {
		return r, err
	}
	return r, rows.Err()
}

// Reserves checks that no reservation outlives the order holding it.
//
// A reservation left behind is a cardholder's money sitting in an account they
// cannot see and cannot spend, and nothing else in the system would ever
// mention it.
func Reserves(ctx context.Context, tx pgx.Tx, businessDate string) (Run, error) {
	r := Run{Kind: KindReserves}
	rows, err := tx.Query(ctx, `
		WITH held AS (
		  SELECT a.id, a.kind::text AS kind, a.asset_id, a.owner_id,
		         COALESCE(SUM(e.amount), 0)::bigint AS balance
		    FROM accounts a LEFT JOIN ledger_entries e ON e.account_id = a.id
		   WHERE a.kind IN ('order_cash_reserve','order_share_reserve')
		   GROUP BY a.id),
		live AS (
		  SELECT a.owner_id, o.side,
		         SUM(o.reserved_kobo)  AS kobo,
		         SUM(o.reserved_units) AS units
		    FROM orders o JOIN accounts a ON a.id = o.account_id
		   WHERE o.state IN ('open','partial')
		   GROUP BY a.owner_id, o.side)
		SELECT h.id::text, h.kind, h.asset_id, h.balance,
		       COALESCE((SELECT CASE WHEN h.kind = 'order_cash_reserve'
		                             THEN SUM(kobo) ELSE SUM(units) END
		                   FROM live l WHERE l.owner_id = h.owner_id), 0)
		  FROM held h WHERE h.balance <> 0`)
	if err != nil {
		return r, fmt.Errorf("recon: reserves: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var acct, kind, asset string
		var balance, expected int64
		if err := rows.Scan(&acct, &kind, &asset, &balance, &expected); err != nil {
			return r, err
		}
		r.Checked++
		if balance != expected {
			exp, act := expected, balance
			r.Breaks = append(r.Breaks, Break{
				Kind: KindReserves, Subject: acct + " " + kind + " " + asset,
				Expected: &exp, Actual: &act,
				Detail: map[string]any{"held_with_no_live_order": balance - expected},
			})
		}
	}
	return r, rows.Err()
}

// Treasury checks that issuance never exceeded what was authorised.
//
// The release cap is enforced at the point of issue; this is the independent
// check that it was, and it reads the cap table rather than the counter the
// cap itself maintains.
func Treasury(ctx context.Context, tx pgx.Tx, businessDate string) (Run, error) {
	r := Run{Kind: KindTreasury}
	rows, err := tx.Query(ctx, `
		SELECT i.id, i.shares_authorised_units,
		       COALESCE((SELECT SUM(units_delta) FROM cap_table_events c
		                  WHERE c.instrument_id = i.id AND c.kind IN ('treasury_release','split','bonus')), 0)
		  FROM instruments i WHERE i.status <> 'draft'`)
	if err != nil {
		return r, fmt.Errorf("recon: treasury: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var instrument string
		var authorised, issued int64
		if err := rows.Scan(&instrument, &authorised, &issued); err != nil {
			return r, err
		}
		r.Checked++
		if issued > authorised {
			exp, act := authorised, issued
			r.Breaks = append(r.Breaks, Break{
				Kind: KindTreasury, Subject: instrument,
				Expected: &exp, Actual: &act,
				Detail: map[string]any{"over_issued_by": issued - authorised},
			})
		}
	}
	return r, rows.Err()
}

func record(ctx context.Context, tx pgx.Tx, businessDate string, r *Run) error {
	err := tx.QueryRow(ctx, `
		INSERT INTO recon_runs (business_date, kind, checked, breaks)
		VALUES ($1::date,$2,$3,$4) RETURNING id`,
		businessDate, r.Kind, r.Checked, len(r.Breaks)).Scan(&r.ID)
	if err != nil {
		return fmt.Errorf("recon: record run: %w", err)
	}
	for _, b := range r.Breaks {
		detail, err := json.Marshal(b.Detail)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO recon_breaks (run_id, kind, subject, expected, actual, detail)
			VALUES ($1,$2,$3,$4,$5,$6)`,
			r.ID, b.Kind, b.Subject, b.Expected, b.Actual, detail); err != nil {
			return fmt.Errorf("recon: record break: %w", err)
		}
	}
	return nil
}

// Resolve closes a break with a decision and the name of whoever made it.
func Resolve(ctx context.Context, tx pgx.Tx, breakID int64, state, by, note string) error {
	switch state {
	case "investigating", "resolved", "written_off":
	default:
		return fmt.Errorf("recon: %q is not a break state", state)
	}
	if by == "" {
		return fmt.Errorf("recon: resolving a break requires a name")
	}
	ct, err := tx.Exec(ctx, `
		UPDATE recon_breaks SET state = $2, resolved_by = $3, note = $4,
		       resolved_at = CASE WHEN $2 = 'investigating' THEN NULL ELSE now() END
		 WHERE id = $1 AND state <> 'resolved'`, breakID, state, by, note)
	if err != nil {
		return fmt.Errorf("recon: resolve break: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("recon: break %d is already resolved", breakID)
	}
	return nil
}

// OpenBreaks counts unresolved breaks, which is the number that belongs on an
// operations dashboard.
func OpenBreaks(ctx context.Context, tx pgx.Tx) (int, error) {
	var n int
	err := tx.QueryRow(ctx,
		`SELECT COUNT(*) FROM recon_breaks WHERE state IN ('open','investigating')`).Scan(&n)
	return n, err
}
