package exchange

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"freedom/api/internal/money"
	"freedom/api/internal/share"
)

// The liquidity gate.
//
// Graduating to continuous trading benefits the issuer — a live book looks like
// a real market and flatters a valuation — so the criteria have to be hard to
// buy. Every test below is either a count of DISTINCT UNRELATED participants or
// a concentration limit, because those are the ones an issuer cannot satisfy by
// trading with itself.
//
// Graduation is reversible. A one-way door removes the issuer's incentive to
// maintain liquidity the moment they are through it.

// LiquidityTest is the standard a symbol must meet, measured over a window of
// recent sessions.
type LiquidityTest struct {
	Window             int        // sessions looked at
	MinTradedSessions  int        // of those, how many must have crossed
	MinMedianNotional  money.Kobo // typical session size
	MinDistinctBuyers  int        // unrelated accounts
	MinDistinctSellers int        // unrelated accounts
	MaxAccountShareBps int64      // no single account above this share of volume
	MaxGroupShareBps   int64      // no single related-party group above this
	MaxSessionMoveBps  int64      // stability
	MinSessionsListed  int        // cooling-off since listing
}

// StandardLiquidityTest is the launch standard.
func StandardLiquidityTest() LiquidityTest {
	return LiquidityTest{
		Window:             20,
		MinTradedSessions:  15,
		MinMedianNotional:  money.Naira(500_000),
		MinDistinctBuyers:  20,
		MinDistinctSellers: 10,
		MaxAccountShareBps: 2500, // 25%
		MaxGroupShareBps:   4000, // 40%
		MaxSessionMoveBps:  2000, // 20%
		MinSessionsListed:  60,
	}
}

// LiquidityReport is the evidence behind a graduation decision.
type LiquidityReport struct {
	InstrumentID    string
	Passes          bool
	Failures        []string
	TradedSessions  int
	MedianNotional  money.Kobo
	DistinctBuyers  int
	DistinctSellers int
	TopAccountBps   int64
	TopGroupBps     int64
	MaxMoveBps      int64
	SessionsListed  int
}

// Assess measures a symbol against the standard.
func (lt LiquidityTest) Assess(ctx context.Context, tx pgx.Tx, instrumentID, asOf string) (LiquidityReport, error) {
	r := LiquidityReport{InstrumentID: instrumentID}

	err := tx.QueryRow(ctx, `
		WITH recent AS (
		  SELECT obs_date, price_kobo, volume_units
		    FROM price_observations
		   WHERE instrument_id = $1 AND obs_date <= $2::date AND source = 'auction'
		   ORDER BY obs_date DESC LIMIT $3)
		SELECT COUNT(*) FILTER (WHERE volume_units > 0),
		       COALESCE(percentile_disc(0.5) WITHIN GROUP (
		           ORDER BY (price_kobo * volume_units) / 100000000), 0)::bigint
		  FROM recent`,
		instrumentID, asOf, lt.Window).Scan(&r.TradedSessions, &r.MedianNotional)
	if err != nil {
		return r, fmt.Errorf("exchange: liquidity sessions: %w", err)
	}

	// Distinct UNRELATED participants. This is the criterion that actually
	// bites: an issuer can manufacture volume, but not counterparties.
	err = tx.QueryRow(ctx, `
		SELECT COUNT(DISTINCT a.owner_id) FILTER (WHERE f.side = 'buy'),
		       COUNT(DISTINCT a.owner_id) FILTER (WHERE f.side = 'sell')
		  FROM fills f
		  JOIN orders o   ON o.id = f.order_id
		  JOIN accounts a ON a.id = o.account_id
		 WHERE f.instrument_id = $1 AND f.session_date <= $2::date
		   AND o.owner_key IS NULL`,
		instrumentID, asOf).Scan(&r.DistinctBuyers, &r.DistinctSellers)
	if err != nil {
		return r, fmt.Errorf("exchange: liquidity participants: %w", err)
	}

	var topAcct, topGroup, total *int64
	err = tx.QueryRow(ctx, `
		WITH v AS (
		  SELECT a.owner_id, COALESCE(o.owner_key, 'account:' || a.owner_id::text) AS grp, f.units
		    FROM fills f JOIN orders o ON o.id = f.order_id JOIN accounts a ON a.id = o.account_id
		   WHERE f.instrument_id = $1 AND f.session_date <= $2::date)
		SELECT (SELECT MAX(u) FROM (SELECT SUM(units) u FROM v GROUP BY owner_id) x),
		       (SELECT MAX(u) FROM (SELECT SUM(units) u FROM v GROUP BY grp) y),
		       (SELECT SUM(units) FROM v)`,
		instrumentID, asOf).Scan(&topAcct, &topGroup, &total)
	if err != nil {
		return r, fmt.Errorf("exchange: liquidity concentration: %w", err)
	}
	if total != nil && *total > 0 {
		if topAcct != nil {
			r.TopAccountBps = *topAcct * 10_000 / *total
		}
		if topGroup != nil {
			r.TopGroupBps = *topGroup * 10_000 / *total
		}
	}

	var maxMove *int64
	err = tx.QueryRow(ctx, `
		WITH recent AS (
		  SELECT price_kobo, LAG(price_kobo) OVER (ORDER BY obs_date) AS prev
		    FROM price_observations
		   WHERE instrument_id = $1 AND obs_date <= $2::date AND source = 'auction'
		   ORDER BY obs_date DESC LIMIT $3)
		SELECT MAX(ABS(price_kobo - prev) * 10000 / NULLIF(prev, 0)) FROM recent WHERE prev IS NOT NULL`,
		instrumentID, asOf, lt.Window).Scan(&maxMove)
	if err != nil {
		return r, fmt.Errorf("exchange: liquidity stability: %w", err)
	}
	if maxMove != nil {
		r.MaxMoveBps = *maxMove
	}

	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM trading_calendar
		 WHERE is_trading AND session_date > (SELECT listed_at::date FROM instruments WHERE id = $1)
		   AND session_date <= $2::date`, instrumentID, asOf).Scan(&r.SessionsListed); err != nil {
		return r, fmt.Errorf("exchange: sessions since listing: %w", err)
	}

	// Anything unresolved and blocking disqualifies outright — a symbol under
	// suspicion does not get a more permissive market.
	var blocked bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM surveillance_alerts
		                WHERE instrument_id = $1 AND severity = 'block' AND reviewed_at IS NULL)
		    OR EXISTS (SELECT 1 FROM data_quality_incidents
		                WHERE instrument_id = $1 AND resolved_at IS NULL)`,
		instrumentID).Scan(&blocked); err != nil {
		return r, fmt.Errorf("exchange: outstanding issues: %w", err)
	}

	check := func(ok bool, msg string, args ...any) {
		if !ok {
			r.Failures = append(r.Failures, fmt.Sprintf(msg, args...))
		}
	}
	check(r.TradedSessions >= lt.MinTradedSessions,
		"traded in %d of the last %d sessions, needs %d", r.TradedSessions, lt.Window, lt.MinTradedSessions)
	check(r.MedianNotional >= lt.MinMedianNotional,
		"median session notional %s, needs %s", r.MedianNotional, lt.MinMedianNotional)
	check(r.DistinctBuyers >= lt.MinDistinctBuyers,
		"%d distinct unrelated buyers, needs %d", r.DistinctBuyers, lt.MinDistinctBuyers)
	check(r.DistinctSellers >= lt.MinDistinctSellers,
		"%d distinct unrelated sellers, needs %d", r.DistinctSellers, lt.MinDistinctSellers)
	check(r.TopAccountBps <= lt.MaxAccountShareBps,
		"largest account holds %d bps of volume, limit %d", r.TopAccountBps, lt.MaxAccountShareBps)
	check(r.TopGroupBps <= lt.MaxGroupShareBps,
		"largest related group holds %d bps of volume, limit %d", r.TopGroupBps, lt.MaxGroupShareBps)
	check(r.MaxMoveBps <= lt.MaxSessionMoveBps,
		"largest session move %d bps, limit %d", r.MaxMoveBps, lt.MaxSessionMoveBps)
	check(r.SessionsListed >= lt.MinSessionsListed,
		"listed for %d sessions, needs %d", r.SessionsListed, lt.MinSessionsListed)
	check(!blocked, "unresolved blocking alerts or data-quality incidents")

	r.Passes = len(r.Failures) == 0
	return r, nil
}

// Graduate opens continuous trading for a symbol that passed.
func Graduate(ctx context.Context, tx pgx.Tx, r LiquidityReport, by string) error {
	if !r.Passes {
		return fmt.Errorf("exchange: %s does not meet the liquidity standard: %v", r.InstrumentID, r.Failures)
	}
	ct, err := tx.Exec(ctx, `
		UPDATE instruments
		   SET clob_enabled = true, clob_enabled_at = now(), clob_review_state = 'continuous'
		 WHERE id = $1 AND NOT clob_enabled`, r.InstrumentID)
	if err != nil {
		return fmt.Errorf("exchange: graduate: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("exchange: %s already trades continuously", r.InstrumentID)
	}
	return recordReview(ctx, tx, r, "graduated", by)
}

// Demote returns a symbol to auction-only.
//
// Reversibility is the point. A symbol whose liquidity was built for the
// graduation test and abandoned afterwards goes back to a market structure that
// suits how it actually trades.
func Demote(ctx context.Context, tx pgx.Tx, r LiquidityReport, by string) error {
	ct, err := tx.Exec(ctx, `
		UPDATE instruments
		   SET clob_enabled = false, clob_disabled_at = now(), clob_review_state = 'demoted'
		 WHERE id = $1 AND clob_enabled`, r.InstrumentID)
	if err != nil {
		return fmt.Errorf("exchange: demote: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("exchange: %s is already auction-only", r.InstrumentID)
	}
	// Resting orders cannot survive the change of market structure: there is no
	// continuous book left for them to rest on, and their reservations would be
	// stranded.
	if _, err := tx.Exec(ctx, `
		UPDATE orders SET state = 'cancelled', reject_reason = 'clob_demoted'
		 WHERE instrument_id = $1 AND venue = 'clob' AND state IN ('open','partial')`,
		r.InstrumentID); err != nil {
		return fmt.Errorf("exchange: cancel resting orders: %w", err)
	}
	return recordReview(ctx, tx, r, "demoted", by)
}

func recordReview(ctx context.Context, tx pgx.Tx, r LiquidityReport, outcome, by string) error {
	ev, err := json.Marshal(map[string]any{
		"outcome":          outcome,
		"by":               by,
		"traded_sessions":  r.TradedSessions,
		"median_notional":  int64(r.MedianNotional),
		"distinct_buyers":  r.DistinctBuyers,
		"distinct_sellers": r.DistinctSellers,
		"top_account_bps":  r.TopAccountBps,
		"top_group_bps":    r.TopGroupBps,
		"max_move_bps":     r.MaxMoveBps,
		"failures":         r.Failures,
	})
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO data_quality_incidents (instrument_id, kind, detail, resolved_at)
		VALUES ($1, 'liquidity_review', $2, now())`, r.InstrumentID, ev)
	return err
}

var _ = share.Units(0)
