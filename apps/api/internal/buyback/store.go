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

// Refusal is why a session cannot be priced.
//
// A refusal is not a failure: the intents escrow and are retried, rather than
// failing the whole clearing batch. Escrowing is always available because the
// funding is already held — what is missing is a price we are willing to stand
// behind.
type Refusal string

const (
	RefusalNoSession    Refusal = "no_session"       // the auction has not published
	RefusalHalted       Refusal = "halted"           // trading is suspended
	RefusalStale        Refusal = "stale_reference"  // too many carry-forward sessions
	RefusalThinVWAP     Refusal = "thin_vwap_window" // not enough traded volume to cap against
	RefusalSurveillance Refusal = "surveillance_block"
)

// PriceRefused carries a refusal up to the caller.
type PriceRefused struct{ Reason Refusal }

func (e PriceRefused) Error() string { return "buyback: no execution price: " + string(e.Reason) }

// executionPrice is what the buyback will pay per share this session.
//
// It consumes a PUBLISHED AUCTION rather than reading price observations
// directly. The difference matters: the previous version took whatever
// observation was newest on or before the session date, which meant it would
// happily pay a price that no auction produced, or a price from ninety days ago
// on an instrument nobody had traded since.
//
// The checks run in order, and each has its own refusal so support can tell a
// merchant why their customers' shares did not arrive.
func (e *Engine) executionPrice(ctx context.Context, tx pgx.Tx, instrumentID, sessionDate string) (money.Kobo, error) {
	// 1. Trading must not be suspended. A halted instrument must not accrue a
	//    price, and must certainly not have one paid against it.
	var halted bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM trading_halts
		                WHERE instrument_id = $1 AND released_at IS NULL)`,
		instrumentID).Scan(&halted); err != nil {
		return 0, fmt.Errorf("buyback: halt check: %w", err)
	}
	if halted {
		return 0, PriceRefused{RefusalHalted}
	}

	// 2. No unresolved blocking surveillance alert. If we suspect the price was
	//    manipulated, spending scheme money against it is the harm.
	var blocked bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM surveillance_alerts
		                WHERE instrument_id = $1 AND severity = 'block' AND reviewed_at IS NULL)`,
		instrumentID).Scan(&blocked); err != nil {
		return 0, fmt.Errorf("buyback: surveillance check: %w", err)
	}
	if blocked {
		return 0, PriceRefused{RefusalSurveillance}
	}

	// 3. If this date has a session, it must have published: the buyback must
	//    never run ahead of the market it takes its price from. A date with
	//    no session at all — a weekend, a holiday, the hours after the 20:00
	//    cutover — is a different case: there is no market today to wait
	//    for, and the price the next session would carry forward is the
	//    reference we already hold. A Friday-evening tap should not wait
	//    until Monday for shares the market would price identically.
	var zeroVolume bool
	var clearing *int64
	var state string
	err := tx.QueryRow(ctx, `
		SELECT state, zero_volume, clearing_price_kobo FROM auctions
		 WHERE instrument_id = $1 AND session_date = $2::date`,
		instrumentID, sessionDate).Scan(&state, &zeroVolume, &clearing)
	noSession := errors.Is(err, pgx.ErrNoRows)
	if err != nil && !noSession {
		return 0, fmt.Errorf("buyback: load session: %w", err)
	}
	if !noSession && state != "published" {
		return 0, PriceRefused{RefusalNoSession}
	}

	var sessionPrice money.Kobo
	if !noSession && !zeroVolume && clearing != nil {
		sessionPrice = money.Kobo(*clearing)
	} else {
		// 4. Nothing crossed, or no session today. The carried-forward
		//    reference is usable, but only while it is still recent enough
		//    to mean anything.
		var carried, maxCarried int
		var ref int64
		if err := tx.QueryRow(ctx, `
			SELECT carry_forward_sessions, max_carry_forward_sessions, reference_price_kobo
			  FROM instruments WHERE id = $1`, instrumentID).Scan(&carried, &maxCarried, &ref); err != nil {
			return 0, fmt.Errorf("buyback: staleness check: %w", err)
		}
		if carried > maxCarried {
			return 0, PriceRefused{RefusalStale}
		}
		sessionPrice = money.Kobo(ref)
	}
	if sessionPrice <= 0 {
		return 0, PriceRefused{RefusalNoSession}
	}

	if e.TrailingBandSessions <= 0 {
		return sessionPrice, nil
	}

	// 5. Cap at the trailing volume-weighted average.
	vwap, status, err := e.trailingVWAP(ctx, tx, instrumentID, sessionDate)
	if err != nil {
		return 0, err
	}
	switch status {
	case vwapThin:
		// A cap computed from one dust trade is not a control; it is a control
		// that looks like one, which is worse. Refuse and escrow instead.
		return 0, PriceRefused{RefusalThinVWAP}
	case vwapNoHistory:
		// A brand-new listing has nothing to compare against. The daily release
		// cap is the binding defence for these first sessions.
		return sessionPrice, nil
	}
	if sessionPrice > vwap {
		return vwap, nil
	}
	return sessionPrice, nil
}

// vwapStatus distinguishes "there is no history to cap against" from "the cap
// is zero", which are the same value and very different decisions.
type vwapStatus int

const (
	vwapOK vwapStatus = iota
	vwapNoHistory
	vwapThin
)

// trailingVWAP is the volume-weighted average of recent traded sessions.
//
// Three things it does that the first version did not.
//
// It reads the CORPORATE-ACTION-ADJUSTED series. This is not tidiness: after a
// 2:1 split every prior session reads at twice the real price, so an unadjusted
// cap sits at twice the current price and stops binding for the length of the
// window. An issuer who wanted to ramp would simply split the week before.
//
// It requires a minimum traded volume across the window, because a window
// containing a single 1e-8 trade at a manipulated price would otherwise BE the
// cap — a control that looks like one while providing nothing.
//
// And the arithmetic runs in numeric rather than bigint: price times volume at
// realistic scales is around 1e17 per row, and summing five of those sits
// uncomfortably close to the int64 ceiling.
func (e *Engine) trailingVWAP(ctx context.Context, tx pgx.Tx, instrumentID, sessionDate string) (money.Kobo, vwapStatus, error) {
	var minVolume int64
	if err := tx.QueryRow(ctx,
		`SELECT min_vwap_volume_units FROM instruments WHERE id = $1`, instrumentID).Scan(&minVolume); err != nil {
		return 0, vwapThin, fmt.Errorf("buyback: vwap floor: %w", err)
	}

	var vwap, volume *int64
	err := tx.QueryRow(ctx, `
		SELECT CASE WHEN SUM(volume_units) > 0
		            THEN (SUM(price_kobo::numeric * volume_units::numeric)
		                  / SUM(volume_units::numeric))::bigint
		       END,
		       SUM(volume_units)::bigint
		  FROM (SELECT adj_price_kobo AS price_kobo, adj_volume_units AS volume_units
		          FROM price_observations_adjusted
		         WHERE instrument_id = $1 AND obs_date < $2::date AND volume_units > 0
		         ORDER BY obs_date DESC, id DESC LIMIT $3) recent`,
		instrumentID, sessionDate, e.TrailingBandSessions).Scan(&vwap, &volume)
	if err != nil {
		return 0, vwapThin, fmt.Errorf("buyback: trailing vwap: %w", err)
	}
	if vwap == nil || volume == nil {
		return 0, vwapNoHistory, nil
	}
	if *volume < minVolume {
		return 0, vwapThin, nil
	}
	return money.Kobo(*vwap), vwapOK, nil
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
// The cap is per instrument per day and it is the hard stop on value
// extraction: whatever a collusive listing does to its own price, it cannot
// sell the network more than this many units today.
//
// Releases are recorded in an append-only table keyed by session date. The
// previous shape kept a running total on treasury_pools and reset it whenever
// the stored release_date differed from the session being processed — so
// replaying session D after D+1 had already run silently restored the whole
// day's cap, on exactly the path most likely to be re-run.
func applyReleaseCap(ctx context.Context, tx pgx.Tx, instrumentID, sessionDate string, want share.Units) (share.Units, bool, error) {
	var daily int64
	err := tx.QueryRow(ctx,
		`SELECT daily_release_units FROM treasury_pools WHERE instrument_id = $1`,
		instrumentID).Scan(&daily)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, fmt.Errorf("buyback: %s has no treasury pool", instrumentID)
	}
	if err != nil {
		return 0, false, fmt.Errorf("buyback: release cap: %w", err)
	}

	var released int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO treasury_releases (instrument_id, session_date, units)
		VALUES ($1, $2::date, 0)
		ON CONFLICT (instrument_id, session_date) DO UPDATE
		  SET units = treasury_releases.units
		RETURNING units`, instrumentID, sessionDate).Scan(&released); err != nil {
		return 0, false, fmt.Errorf("buyback: read release history: %w", err)
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
		UPDATE treasury_releases SET units = units + $3
		 WHERE instrument_id = $1 AND session_date = $2::date`,
		instrumentID, sessionDate, int64(grant)); err != nil {
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
		  -- A session's buyback can run more than once: the rail runs it as
		  -- taps arrive once the day has a price, and the close runs it again
		  -- for anything still pending. The batch is the day's total, so each
		  -- run adds to it rather than replacing it. The price is the same
		  -- within a day by construction (one published session).
		  SET funding_kobo = buyback_batches.funding_kobo + EXCLUDED.funding_kobo,
		      units_bought = buyback_batches.units_bought + EXCLUDED.units_bought,
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
	// cost_open_kobo starts equal to the cost: it is what a later disposal
	// decrements, and a lot opened with zero open basis reports its entire
	// proceeds as gain the first time it is sold.
	if _, err := tx.Exec(ctx, `
		INSERT INTO holding_lots
		  (account_id, instrument_id, units, units_open, cost_kobo, cost_open_kobo,
		   transferable_from, ledger_tx_id)
		VALUES ($1,$2,$3,$3,$4,$4,$5::date,$6)`,
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

// escrowPending parks every pending intent for an instrument that cannot be
// priced. The funding is not lost; it is retried next session.
func escrowPending(ctx context.Context, tx pgx.Tx, instrumentID, reason string) (int, error) {
	ct, err := tx.Exec(ctx, `
		UPDATE buyback_intents SET state = 'escrowed'
		 WHERE instrument_id = $1 AND state = 'pending'`, instrumentID)
	if err != nil {
		return 0, fmt.Errorf("buyback: escrow pending (%s): %w", reason, err)
	}
	return int(ct.RowsAffected()), nil
}
