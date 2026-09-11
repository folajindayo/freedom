package buyback

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

// executionPrice is what the buyback will pay per share this session.
//
// It is the current reference price, capped at the trailing VWAP. The cap is
// the defence against a listing that runs a wash auction against itself on a
// thin book: doubling your own print does not double what the network pays you,
// because the trailing window has not moved.
func (e *Engine) executionPrice(ctx context.Context, tx pgx.Tx, instrumentID, sessionDate string) (money.Kobo, error) {
	var ref int64
	err := tx.QueryRow(ctx, `
		SELECT price_kobo FROM price_observations
		 WHERE instrument_id = $1 AND obs_date <= $2::date
		 ORDER BY obs_date DESC, id DESC LIMIT 1`, instrumentID, sessionDate).Scan(&ref)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("buyback: %s has no reference price for %s", instrumentID, sessionDate)
	}
	if err != nil {
		return 0, fmt.Errorf("buyback: reference price: %w", err)
	}
	price := money.Kobo(ref)

	if e.TrailingBandSessions <= 0 {
		return price, nil
	}

	// Volume-weighted average of the trailing sessions. Sessions with no volume
	// contribute nothing, so a run of empty carry-forward days cannot anchor
	// the cap at a price nobody traded at.
	var num, den int64
	err = tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(price_kobo * volume_units), 0), COALESCE(SUM(volume_units), 0)
		  FROM (SELECT price_kobo, volume_units FROM price_observations
		         WHERE instrument_id = $1 AND obs_date < $2::date AND volume_units > 0
		         ORDER BY obs_date DESC, id DESC LIMIT $3) recent`,
		instrumentID, sessionDate, e.TrailingBandSessions).Scan(&num, &den)
	if err != nil {
		return 0, fmt.Errorf("buyback: trailing vwap: %w", err)
	}
	if den == 0 {
		// No traded history yet. The reference stands: on a brand-new listing
		// there is nothing to compare against, which is exactly why the daily
		// release cap is the binding defence in the first sessions.
		return price, nil
	}
	vwap := money.Kobo(num / den)
	if price > vwap {
		return vwap, nil
	}
	return price, nil
}

func pendingIntents(ctx context.Context, tx pgx.Tx, instrumentID string) ([]intent, money.Kobo, error) {
	// Ordered by id so that a re-run allocates identically: largest-remainder
	// ties break by position, and position must not depend on the planner.
	rows, err := tx.Query(ctx, `
		SELECT id, cardholder_id, funding_kobo
		  FROM buyback_intents
		 WHERE instrument_id = $1 AND state = 'pending'
		 ORDER BY id`, instrumentID)
	if err != nil {
		return nil, 0, fmt.Errorf("buyback: pending intents: %w", err)
	}
	defer rows.Close()

	var out []intent
	var total money.Kobo
	for rows.Next() {
		var in intent
		if err := rows.Scan(&in.ID, &in.CardholderID, &in.Funding); err != nil {
			return nil, 0, err
		}
		out = append(out, in)
		total += in.Funding
	}
	return out, total, rows.Err()
}

// applyReleaseCap bounds how much treasury an instrument may release in one
// session, and reserves it atomically.
//
// The cap is per instrument per day and it is the hard stop on value extraction:
// whatever a collusive listing does to its own price, it cannot sell the network
// more than this many units today.
func applyReleaseCap(ctx context.Context, tx pgx.Tx, instrumentID, sessionDate string, want share.Units) (share.Units, bool, error) {
	var daily, released int64
	err := tx.QueryRow(ctx, `
		UPDATE treasury_pools
		   SET released_units = CASE WHEN release_date = $2::date THEN released_units ELSE 0 END,
		       release_date   = $2::date
		 WHERE instrument_id = $1
		RETURNING daily_release_units, released_units`, instrumentID, sessionDate).
		Scan(&daily, &released)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, fmt.Errorf("buyback: %s has no treasury pool", instrumentID)
	}
	if err != nil {
		return 0, false, fmt.Errorf("buyback: release cap: %w", err)
	}

	headroom := share.Units(daily - released)
	if headroom <= 0 {
		return 0, true, nil
	}
	grant := want
	capped := false
	if grant > headroom {
		grant, capped = headroom, true
	}

	// The authorised ceiling is the other hard stop: every buyback is issuance,
	// and issuance cannot exceed what the company authorised.
	var authorised, issued int64
	if err := tx.QueryRow(ctx, `
		SELECT i.shares_authorised_units,
		       COALESCE((SELECT SUM(units_delta) FROM cap_table_events
		                  WHERE instrument_id = i.id AND kind = 'treasury_release'), 0)
		  FROM instruments i WHERE i.id = $1`, instrumentID).Scan(&authorised, &issued); err != nil {
		return 0, false, fmt.Errorf("buyback: authorised check: %w", err)
	}
	if remaining := share.Units(authorised - issued); grant > remaining {
		grant, capped = remaining, true
	}
	if grant <= 0 {
		return 0, true, nil
	}

	if _, err := tx.Exec(ctx, `
		UPDATE treasury_pools SET released_units = released_units + $2
		 WHERE instrument_id = $1`, instrumentID, int64(grant)); err != nil {
		return 0, false, fmt.Errorf("buyback: reserve release: %w", err)
	}
	return grant, capped, nil
}

func openBatch(ctx context.Context, tx pgx.Tx, instrumentID, sessionDate string,
	funding money.Kobo, units share.Units, price money.Kobo) (uuid.UUID, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO buyback_batches
		  (instrument_id, session_date, funding_kobo, units_bought, price_kobo, source, state)
		VALUES ($1,$2::date,$3,$4,$5,'treasury','executed')
		ON CONFLICT (instrument_id, session_date) DO UPDATE
		  SET funding_kobo = EXCLUDED.funding_kobo,
		      units_bought = EXCLUDED.units_bought,
		      price_kobo   = EXCLUDED.price_kobo
		RETURNING id`,
		instrumentID, sessionDate, int64(funding), int64(units), int64(price)).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("buyback: open batch: %w", err)
	}
	return id, nil
}

func markBatchAllocated(ctx context.Context, tx pgx.Tx, batchID uuid.UUID) error {
	_, err := tx.Exec(ctx,
		`UPDATE buyback_batches SET state = 'allocated' WHERE id = $1`, batchID)
	return err
}

func companyFor(ctx context.Context, tx pgx.Tx, instrumentID string) (uuid.UUID, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx,
		`SELECT company_id FROM instruments WHERE id = $1`, instrumentID).Scan(&id)
	return id, err
}

func symbolFor(ctx context.Context, tx pgx.Tx, instrumentID string) (string, error) {
	var s string
	err := tx.QueryRow(ctx,
		`SELECT symbol FROM instruments WHERE id = $1`, instrumentID).Scan(&s)
	return s, err
}

// deferDust leaves a funded intent pending when it cannot buy a whole unit.
// It is never closed out silently: the cardholder paid for this and is owed it.
func deferDust(ctx context.Context, tx pgx.Tx, intentID uuid.UUID) error {
	_, err := tx.Exec(ctx,
		`UPDATE buyback_intents SET state = 'deferred_dust' WHERE id = $1 AND state = 'pending'`,
		intentID)
	return err
}

// holdForIneligible parks an intent whose cardholder is not yet permitted to
// hold equity. The funding is not lost; it becomes claimable on verification.
func holdForIneligible(ctx context.Context, tx pgx.Tx, intentID uuid.UUID) error {
	_, err := tx.Exec(ctx,
		`UPDATE buyback_intents SET state = 'escrowed' WHERE id = $1 AND state = 'pending'`,
		intentID)
	return err
}

// finishIntent records the allocation and opens the holding lot.
func finishIntent(ctx context.Context, tx pgx.Tx, a allocation, txID uuid.UUID,
	cost, residual money.Kobo) error {

	if _, err := tx.Exec(ctx, `
		UPDATE buyback_intents
		   SET state = 'allocated', batch_id = $2, allocated_units = $3,
		       price_kobo = $4, residual_kobo = $5
		 WHERE id = $1`,
		a.IntentID, a.BatchID, int64(a.Units), int64(a.Price), int64(residual)); err != nil {
		return fmt.Errorf("buyback: finish intent: %w", err)
	}

	wallet, err := ledger.Resolve(ctx, tx,
		ledger.Cardholder(a.CardholderID, ledger.KindStockWallet, a.InstrumentID))
	if err != nil {
		return err
	}

	// A lot, not a running total: cost basis cannot be reconstructed later, and
	// transferable_from is what makes a chargeback unwind possible at all.
	if _, err := tx.Exec(ctx, `
		INSERT INTO holding_lots
		  (account_id, instrument_id, units, units_open, cost_kobo, transferable_from, ledger_tx_id)
		VALUES ($1,$2,$3,$3,$4,$5::date,$6)`,
		wallet, a.InstrumentID, int64(a.Units), int64(cost), a.LockedUntil, txID); err != nil {
		return fmt.Errorf("buyback: open holding lot: %w", err)
	}

	// Issuance is an event, so dilution can always be reconstructed.
	if _, err := tx.Exec(ctx, `
		INSERT INTO cap_table_events (instrument_id, kind, units_delta, ledger_tx_id, note)
		VALUES ($1,'treasury_release',$2,$3,$4)`,
		a.InstrumentID, int64(a.Units), txID, "buyback intent "+a.IntentID.String()); err != nil {
		return fmt.Errorf("buyback: cap table event: %w", err)
	}
	return nil
}
