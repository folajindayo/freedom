package console

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"freedom/api/internal/exchange"
)

// ---------------------------------------------------------------- overview

func (s *Server) overview(ctx context.Context, r *http.Request) (any, error) {
	today, err := s.Rail.Today(ctx)
	if err != nil {
		return nil, err
	}
	date := today.BusinessDate

	counts, err := one(ctx, s.Pool, `
		SELECT (SELECT COUNT(*) FROM instruments WHERE status = 'listed')::int AS listed,
		       (SELECT COUNT(*) FROM instruments WHERE status IN ('listed','halted','suspended'))::int AS live,
		       (SELECT COALESCE(SUM(consideration_kobo),0) FROM fills WHERE session_date = $1::date AND side = 'buy')::bigint AS notional_today_kobo,
		       (SELECT COALESCE(SUM(units),0) FROM fills WHERE session_date = $1::date AND side = 'buy')::bigint AS matched_today_units,
		       (SELECT COUNT(*) FROM fills WHERE session_date = $1::date AND side = 'buy')::int AS trades_today,
		       (SELECT COUNT(*) FROM surveillance_alerts WHERE severity = 'block' AND reviewed_at IS NULL)::int AS open_block_alerts,
		       (SELECT COUNT(*) FROM surveillance_alerts WHERE state IN ('open','triaged'))::int AS open_alerts,
		       (SELECT COUNT(*) FROM trading_halts WHERE released_at IS NULL)::int AS open_halts,
		       (SELECT COUNT(*) FROM data_quality_incidents WHERE resolved_at IS NULL)::int AS open_incidents,
		       (SELECT COUNT(*) FROM disclosures WHERE published_at IS NULL)::int AS unpublished_disclosures,
		       (SELECT COUNT(*) FROM listing_applications WHERE state IN ('submitted','in_review'))::int AS pending_applications,
		       (SELECT COUNT(*) FROM lp_performance WHERE session_date = $1::date AND NOT met)::int AS mm_not_met_today,
		       (SELECT COUNT(*) FROM liquidity_providers WHERE state IN ('active','warned') AND effective @> $1::date)::int AS mm_active,
		       (SELECT COALESCE(SUM(funding_kobo),0) FROM buyback_intents WHERE state = 'pending')::bigint AS buyback_pending_kobo,
		       (SELECT COUNT(*) FROM buyback_intents WHERE state = 'pending')::int AS buyback_pending_intents,
		       (SELECT COALESCE(SUM(funding_kobo),0) FROM buyback_intents WHERE state = 'escrowed')::bigint AS buyback_escrowed_kobo,
		       (SELECT COUNT(*) FROM buyback_intents WHERE state = 'escrowed')::int AS buyback_escrowed_intents,
		       (SELECT COALESCE(SUM(funding_kobo),0) FROM buyback_intents WHERE state = 'allocated' AND batch_id IN
		            (SELECT id FROM buyback_batches WHERE session_date = $1::date))::bigint AS buyback_allocated_today_kobo,
		       (SELECT COUNT(*) FROM recon_breaks WHERE state = 'open')::int AS open_breaks,
		       (SELECT COUNT(*) FROM complaints WHERE state NOT IN ('resolved','withdrawn'))::int AS open_complaints,
		       (SELECT COUNT(*) FROM complaints WHERE state NOT IN ('resolved','withdrawn') AND respond_by < $1::date)::int AS overdue_complaints,
		       (SELECT COUNT(*) FROM members WHERE status = 'active')::int AS active_members,
		       (SELECT COUNT(*) FROM members WHERE halted)::int AS halted_members,
		       (SELECT COUNT(*) FROM instruments WHERE status = 'listed' AND carry_forward_sessions >= max_carry_forward_sessions)::int AS stale_references,
		       (SELECT COUNT(*) FROM clock_checks WHERE NOT within_bounds AND checked_at > now() - interval '1 day')::int AS clock_out_of_bounds_24h,
		       (SELECT COUNT(*) FROM presentments WHERE clearing_batch_id IS NULL)::int AS unbatched_presentments`,
		date)
	if err != nil {
		return nil, err
	}

	sessionsByState, err := collect(ctx, s.Pool, `
		SELECT state, COUNT(*)::int AS n FROM auctions WHERE session_date = $1::date GROUP BY state ORDER BY state`, date)
	if err != nil {
		return nil, err
	}

	// Participant net positions: the ledger's answer, per participant, next
	// to the latest computed settlement position.
	positions, err := collect(ctx, s.Pool, `
		SELECT p.id::text AS participant_id, p.code, p.legal_name, p.roles, p.status, p.net_debit_cap_kobo,
		       COALESCE((SELECT balance FROM account_balances b WHERE b.owner_type = 'participant' AND b.owner_id = p.id AND b.kind = 'settlement' AND b.asset_id = 'NGN'), 0)::bigint AS settlement_kobo,
		       COALESCE((SELECT balance FROM account_balances b WHERE b.owner_type = 'participant' AND b.owner_id = p.id AND b.kind = 'interchange_income' AND b.asset_id = 'NGN'), 0)::bigint AS interchange_kobo,
		       COALESCE((SELECT balance FROM account_balances b WHERE b.owner_type = 'participant' AND b.owner_id = p.id AND b.kind = 'collateral' AND b.asset_id = 'NGN'), 0)::bigint AS collateral_kobo,
		       sp.net_kobo AS last_net_kobo, sp.state AS last_state, cb.business_date::text AS last_batch_date
		  FROM participants p
		  LEFT JOIN LATERAL (SELECT * FROM settlement_positions x WHERE x.participant_id = p.id ORDER BY computed_at DESC LIMIT 1) sp ON true
		  LEFT JOIN clearing_batches cb ON cb.id = sp.batch_id
		 ORDER BY p.code`)
	if err != nil {
		return nil, err
	}

	queue, err := s.decisionQueue(ctx, date)
	if err != nil {
		return nil, err
	}

	return row{
		"business_date":     date,
		"is_trading":        today.IsTrading,
		"phase":             today.Phase,
		"now":               s.Now().UTC().Format("2006-01-02T15:04:05Z07:00"),
		"instruments":       today.Instruments,
		"counts":            counts,
		"sessions_by_state": sessionsByState,
		"positions":         positions,
		"queue":             queue,
	}, nil
}

// decisionQueue is everything waiting on a person, each row pointing at the
// record it is about.
func (s *Server) decisionQueue(ctx context.Context, date string) ([]row, error) {
	queue := []row{}
	add := func(rows []row, err error) error {
		if err != nil {
			return err
		}
		queue = append(queue, rows...)
		return nil
	}
	if err := add(collect(ctx, s.Pool, `
		SELECT 'alert' AS kind, a.severity, i.symbol, a.id::text AS id,
		       a.detection || ' · ' || COALESCE(a.subject_group, '') AS title,
		       a.state || ' · ' || COALESCE(a.session_date::text, '') AS detail,
		       '#/surveillance/' || a.id AS href, a.raised_at AS at
		  FROM surveillance_alerts a JOIN instruments i ON i.id = a.instrument_id
		 WHERE a.severity = 'block' AND a.reviewed_at IS NULL ORDER BY a.raised_at`)); err != nil {
		return nil, err
	}
	if err := add(collect(ctx, s.Pool, `
		SELECT 'halt' AS kind, 'block' AS severity, i.symbol, h.id::text AS id,
		       'halted: ' || h.reason AS title, 'raised by ' || h.raised_by AS detail,
		       '#/instruments/' || i.symbol AS href, h.halted_at AS at
		  FROM trading_halts h JOIN instruments i ON i.id = h.instrument_id
		 WHERE h.released_at IS NULL ORDER BY h.halted_at`)); err != nil {
		return nil, err
	}
	if err := add(collect(ctx, s.Pool, `
		SELECT 'disclosure' AS kind, 'warn' AS severity, i.symbol, d.id::text AS id,
		       d.kind || ': ' || d.headline AS title,
		       CASE WHEN d.halt_id IS NULL THEN 'unpublished' ELSE 'unpublished · holding a halt' END AS detail,
		       '#/disclosures/' || d.id AS href, d.submitted_at AS at
		  FROM disclosures d JOIN instruments i ON i.id = d.instrument_id
		 WHERE d.published_at IS NULL ORDER BY d.submitted_at`)); err != nil {
		return nil, err
	}
	if err := add(collect(ctx, s.Pool, `
		SELECT 'stale_reference' AS kind, 'warn' AS severity, i.symbol, i.id AS id,
		       'reference carried ' || i.carry_forward_sessions || ' sessions' AS title,
		       'limit ' || i.max_carry_forward_sessions || '; the buyback refuses this price' AS detail,
		       '#/instruments/' || i.symbol AS href, now() AS at
		  FROM instruments i WHERE i.status = 'listed' AND i.carry_forward_sessions >= i.max_carry_forward_sessions
		 ORDER BY i.symbol`)); err != nil {
		return nil, err
	}
	if err := add(collect(ctx, s.Pool, `
		SELECT 'incident' AS kind, 'warn' AS severity, i.symbol, d.id::text AS id,
		       d.kind AS title, 'data-quality incident, unresolved' AS detail,
		       '#/halts' AS href, d.raised_at AS at
		  FROM data_quality_incidents d LEFT JOIN instruments i ON i.id = d.instrument_id
		 WHERE d.resolved_at IS NULL ORDER BY d.raised_at`)); err != nil {
		return nil, err
	}
	if err := add(collect(ctx, s.Pool, `
		SELECT 'mm_breach' AS kind, 'warn' AS severity, i.symbol, lp.id::text AS id,
		       m.code || ' missed its obligation' AS title, COALESCE(p.shortfall, 'not met') AS detail,
		       '#/market-makers' AS href, now() AS at
		  FROM lp_performance p JOIN liquidity_providers lp ON lp.id = p.provider_id
		  JOIN members m ON m.id = lp.member_id JOIN instruments i ON i.id = lp.instrument_id
		 WHERE p.session_date = $1::date AND NOT p.met ORDER BY i.symbol`, date)); err != nil {
		return nil, err
	}
	if err := add(collect(ctx, s.Pool, `
		SELECT 'application' AS kind, 'info' AS severity, a.proposed_symbol AS symbol, a.id::text AS id,
		       c.legal_name || ' · ' || a.state AS title, 'listing application awaiting a decision' AS detail,
		       '#/listings/' || a.id AS href, a.created_at AS at
		  FROM listing_applications a JOIN companies c ON c.id = a.company_id
		 WHERE a.state IN ('submitted','in_review') ORDER BY a.created_at`)); err != nil {
		return nil, err
	}
	if err := add(collect(ctx, s.Pool, `
		SELECT 'break' AS kind, 'warn' AS severity, NULL::text AS symbol, b.id::text AS id,
		       b.kind || ' · ' || b.subject AS title, 'reconciliation break, open' AS detail,
		       '#/reconciliation' AS href, r.ran_at AS at
		  FROM recon_breaks b JOIN recon_runs r ON r.id = b.run_id
		 WHERE b.state = 'open' ORDER BY r.ran_at`)); err != nil {
		return nil, err
	}

	// Gate-eligible symbols: the liquidity standard passes and the symbol is
	// still auction-only, so graduation is a decision someone can make.
	eligible, err := s.gateRows(ctx, date)
	if err != nil {
		return nil, err
	}
	for _, g := range eligible {
		if g["passes"] == true && g["clob_enabled"] != true {
			queue = append(queue, row{
				"kind": "gate", "severity": "info", "symbol": g["symbol"], "id": g["instrument_id"],
				"title": "meets the liquidity standard", "detail": "eligible to graduate to continuous trading",
				"href": fmt.Sprintf("#/instruments/%v", g["symbol"]), "at": s.Now().UTC().Format("2006-01-02T15:04:05Z07:00"),
			})
		}
	}
	return queue, nil
}

// ---------------------------------------------------------------- instruments

const instrumentSelect = `
	SELECT i.id AS instrument_id, i.symbol, i.status, i.clob_enabled, i.clob_review_state,
	       i.clob_enabled_at, i.clob_disabled_at, i.listed_at, i.created_at,
	       i.reference_price_kobo, i.static_band_bps, i.dynamic_band_bps, i.halt_move_bps,
	       i.tick_kobo, i.lot_units, i.min_order_notional_kobo, i.max_order_notional_kobo, i.max_order_units,
	       i.carry_forward_sessions, i.max_carry_forward_sessions, i.min_vwap_volume_units,
	       i.shares_authorised_units, i.company_id::text AS company_id, c.legal_name, c.rc_number,
	       m.trading_name, m.mcc, m.external_ref AS merchant_ref, m.cofund_bps,
	       tp.daily_release_units,
	       COALESCE((SELECT balance FROM account_balances b WHERE b.account_id = tp.account_id), 0)::bigint AS treasury_units,
	       COALESCE((SELECT units FROM treasury_releases tr WHERE tr.instrument_id = i.id AND tr.session_date = $1::date), 0)::bigint AS released_today_units,
	       (SELECT COALESCE(SUM(l.units_open),0) FROM holding_lots l JOIN accounts a ON a.id = l.account_id
	         WHERE l.instrument_id = i.id AND a.kind = 'stock_wallet')::bigint AS in_issue_units,
	       (SELECT COUNT(DISTINCT l.account_id) FROM holding_lots l JOIN accounts a ON a.id = l.account_id
	         WHERE l.instrument_id = i.id AND a.kind = 'stock_wallet' AND l.units_open > 0)::int AS holders,
	       h.reason AS halt_reason, h.halted_at, h.raised_by AS halted_by, (h.id IS NOT NULL) AS halted,
	       a.state AS session_state, a.clearing_price_kobo AS session_price_kobo, a.matched_units AS session_matched_units,
	       (SELECT COUNT(*) FROM surveillance_alerts x WHERE x.instrument_id = i.id AND x.severity = 'block' AND x.reviewed_at IS NULL)::int AS open_block_alerts,
	       (SELECT COUNT(*) FROM data_quality_incidents x WHERE x.instrument_id = i.id AND x.resolved_at IS NULL)::int AS open_incidents,
	       (SELECT COUNT(*) FROM disclosures x WHERE x.instrument_id = i.id AND x.published_at IS NULL)::int AS unpublished_disclosures,
	       po.obs_date::text AS last_obs_date, po.source AS last_obs_source, po.price_kobo AS last_obs_price_kobo
	  FROM instruments i
	  JOIN companies c ON c.id = i.company_id
	  LEFT JOIN merchants m ON m.company_id = c.id
	  LEFT JOIN treasury_pools tp ON tp.instrument_id = i.id
	  LEFT JOIN trading_halts h ON h.instrument_id = i.id AND h.released_at IS NULL
	  LEFT JOIN auctions a ON a.instrument_id = i.id AND a.session_date = $1::date
	  LEFT JOIN price_observations_current po ON po.instrument_id = i.id`

func (s *Server) instruments(ctx context.Context, r *http.Request) (any, error) {
	p := pageParams(r, 100)
	rows, err := collect(ctx, s.Pool, instrumentSelect+`
		 WHERE ($2 = '' OR i.symbol > $2) AND ($3 = '' OR i.status = $3)
		 ORDER BY i.symbol LIMIT $4`, s.today(), p.Cursor, r.URL.Query().Get("status"), p.Limit+1)
	if err != nil {
		return nil, err
	}
	return cut(rows, p, idKey("symbol")), nil
}

func (s *Server) instrument(ctx context.Context, r *http.Request) (any, error) {
	symbol := strings.ToUpper(chi.URLParam(r, "symbol"))
	today := s.today()
	var out row
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		inst, err := one(ctx, tx, instrumentSelect+` WHERE i.symbol = $2`, today, symbol)
		if errors.Is(err, pgx.ErrNoRows) {
			return notFound("No instrument %s", symbol)
		}
		if err != nil {
			return err
		}
		id := str(inst["instrument_id"])
		out = row{"instrument": inst}

		if out["sessions"], err = collect(ctx, tx, sessionSelect+`
			 WHERE a.instrument_id = $1 ORDER BY a.session_date DESC LIMIT 40`, id); err != nil {
			return err
		}
		darken(out["sessions"].([]row))

		if out["prices"], err = collect(ctx, tx, `
			SELECT * FROM (
			  SELECT DISTINCT ON (obs_date) obs_date::text AS obs_date, source, price_kobo, adj_price_kobo,
			         volume_units, adj_volume_units, trade_count
			    FROM price_observations_adjusted
			   WHERE instrument_id = $1
			   ORDER BY obs_date DESC, CASE source WHEN 'manual' THEN 0 WHEN 'auction' THEN 1 WHEN 'clob' THEN 2 ELSE 3 END, id DESC
			   LIMIT 90) p ORDER BY obs_date`, id); err != nil {
			return err
		}

		report, err := s.Gate.Assess(ctx, tx, id, today)
		if err != nil {
			return err
		}
		out["liquidity"] = s.gateReport(report, inst["clob_enabled"] == true)

		if out["related_parties"], err = collect(ctx, tx, `
			SELECT rp.account_id::text AS account_id, rp.group_key, rp.relation, rp.effective::text AS effective,
			       a.owner_type, a.owner_id::text AS owner_id, a.kind AS account_kind,
			       COALESCE(ch.display_name, co.legal_name, mm.trading_name) AS owner_name
			  FROM related_parties rp JOIN accounts a ON a.id = rp.account_id
			  LEFT JOIN cardholders ch ON a.owner_type = 'cardholder' AND ch.id = a.owner_id
			  LEFT JOIN companies co ON a.owner_type = 'company' AND co.id = a.owner_id
			  LEFT JOIN merchants mm ON a.owner_type = 'merchant' AND mm.id = a.owner_id
			 WHERE rp.instrument_id = $1 ORDER BY rp.group_key, rp.relation`, id); err != nil {
			return err
		}

		providers, err := s.providerRows(ctx, tx, id, today)
		if err != nil {
			return err
		}
		for _, pr := range providers {
			if pr["recent"], err = collect(ctx, tx, `
				SELECT session_date::text AS session_date, quoted, two_sided, bid_kobo, ask_kobo, spread_bps, size_kobo, met, shortfall
				  FROM lp_performance WHERE provider_id = $1 ORDER BY session_date DESC LIMIT 20`, pr["provider_id"]); err != nil {
				return err
			}
		}
		out["providers"] = providers

		if out["corporate_actions"], err = collect(ctx, tx, corporateActionSelect+` WHERE ca.instrument_id = $1 ORDER BY ca.ex_date DESC`, id); err != nil {
			return err
		}
		if out["disclosures"], err = collect(ctx, tx, disclosureSelect+` WHERE d.instrument_id = $1 ORDER BY d.submitted_at DESC LIMIT 50`, id); err != nil {
			return err
		}
		if out["holders"], err = collect(ctx, tx, `
			SELECT COALESCE(c.external_ref, c.id::text) AS cardholder_ref, c.display_name,
			       SUM(l.units_open)::bigint AS units, SUM(l.cost_open_kobo)::bigint AS cost_kobo,
			       MIN(l.acquired_at)::date::text AS first_acquired,
			       COALESCE(SUM(l.units_open) FILTER (WHERE l.transferable_from > $2::date), 0)::bigint AS locked_units,
			       COUNT(*)::int AS lots,
			       EXISTS (SELECT 1 FROM related_parties rp WHERE rp.instrument_id = $1 AND rp.account_id = a.id) AS related
			  FROM holding_lots l JOIN accounts a ON a.id = l.account_id JOIN cardholders c ON c.id = a.owner_id
			 WHERE l.instrument_id = $1 AND a.kind = 'stock_wallet' AND l.units_open > 0
			 GROUP BY a.id, c.id ORDER BY units DESC LIMIT 25`, id, today); err != nil {
			return err
		}
		if out["cap_table_events"], err = collect(ctx, tx, `
			SELECT id, kind, units_delta, ledger_tx_id::text AS ledger_tx_id, note, occurred_at
			  FROM cap_table_events WHERE instrument_id = $1 ORDER BY id DESC LIMIT 50`, id); err != nil {
			return err
		}
		if out["halts"], err = collect(ctx, tx, haltSelect+` WHERE h.instrument_id = $1 ORDER BY h.halted_at DESC LIMIT 50`, id); err != nil {
			return err
		}
		if out["alerts"], err = collect(ctx, tx, alertSelect+` WHERE a.instrument_id = $1 ORDER BY a.id DESC LIMIT 20`, id); err != nil {
			return err
		}
		if out["closed_periods"], err = collect(ctx, tx, `
			SELECT id::text AS id, reason, period::text AS period, declared_by, created_at
			  FROM closed_periods WHERE instrument_id = $1 ORDER BY lower(period) DESC LIMIT 20`, id); err != nil {
			return err
		}
		if out["buyback"], err = collect(ctx, tx, `
			SELECT session_date::text AS session_date, funding_kobo, units_bought, price_kobo, source, state
			  FROM buyback_batches WHERE instrument_id = $1 ORDER BY session_date DESC LIMIT 20`, id); err != nil {
			return err
		}
		return nil
	})
	return out, err
}

// gateReport turns the engine's report into the checklist the page draws:
// nine criteria, each with what was measured and what is required.
func (s *Server) gateReport(r exchange.LiquidityReport, clob bool) row {
	lt := s.Gate
	type c struct {
		Criterion string `json:"criterion"`
		Measured  any    `json:"measured"`
		Required  any    `json:"required"`
		// Kind says how to format both numbers: count, kobo or bps. Op is
		// the direction of the test, and Unit the noun the count is of.
		Kind string `json:"kind"`
		Op   string `json:"op"`
		Unit string `json:"unit"`
		Met  bool   `json:"met"`
	}
	blocked := false
	for _, f := range r.Failures {
		if strings.HasPrefix(f, "unresolved blocking") {
			blocked = true
		}
	}
	criteria := []c{
		{"Sessions that crossed", r.TradedSessions, lt.MinTradedSessions, "count", ">=", fmt.Sprintf("of %d", lt.Window), r.TradedSessions >= lt.MinTradedSessions},
		{"Median session notional", int64(r.MedianNotional), int64(lt.MinMedianNotional), "kobo", ">=", "", r.MedianNotional >= lt.MinMedianNotional},
		{"Distinct unrelated buyers", r.DistinctBuyers, lt.MinDistinctBuyers, "count", ">=", "accounts", r.DistinctBuyers >= lt.MinDistinctBuyers},
		{"Distinct unrelated sellers", r.DistinctSellers, lt.MinDistinctSellers, "count", ">=", "accounts", r.DistinctSellers >= lt.MinDistinctSellers},
		{"Largest single account", r.TopAccountBps, lt.MaxAccountShareBps, "bps", "<=", "of volume", r.TopAccountBps <= lt.MaxAccountShareBps},
		{"Largest related-party group", r.TopGroupBps, lt.MaxGroupShareBps, "bps", "<=", "of volume", r.TopGroupBps <= lt.MaxGroupShareBps},
		{"Largest session-over-session move", r.MaxMoveBps, lt.MaxSessionMoveBps, "bps", "<=", "", r.MaxMoveBps <= lt.MaxSessionMoveBps},
		{"Sessions since listing", r.SessionsListed, lt.MinSessionsListed, "count", ">=", "sessions", r.SessionsListed >= lt.MinSessionsListed},
		{"Open blocking alerts or incidents", map[bool]string{true: "some", false: "none"}[blocked], "none", "text", "=", "", !blocked},
	}
	return row{"passes": r.Passes, "failures": r.Failures, "clob_enabled": clob, "criteria": criteria,
		"as_of": s.today(), "window": lt.Window}
}

// ---------------------------------------------------------------- sessions

const sessionSelect = `
	SELECT a.id::text AS auction_id, i.symbol, a.instrument_id, a.session_date::text AS session_date, a.state,
	       a.opens_at, a.freezes_at, a.frozen_at, a.uncrossed_at, a.publishes_at,
	       a.prev_reference_kobo, a.clearing_price_kobo, a.matched_units, a.imbalance_units, a.imbalance_side,
	       a.zero_volume, a.rule, a.book_hash, a.result_hash, a.engine_version,
	       (SELECT COUNT(*) FROM orders o WHERE o.auction_id = a.id)::int AS orders,
	       (SELECT COUNT(*) FROM orders o WHERE o.auction_id = a.id AND o.state IN ('open','partial'))::int AS orders_open,
	       (SELECT COUNT(*) FROM fills f WHERE f.auction_id = a.id AND f.side = 'buy')::int AS trades,
	       (SELECT COALESCE(SUM(f.consideration_kobo),0) FROM fills f WHERE f.auction_id = a.id AND f.side = 'buy')::bigint AS notional_kobo,
	       bb.id::text AS buyback_batch_id, bb.funding_kobo AS buyback_funding_kobo, bb.units_bought AS buyback_units,
	       bb.price_kobo AS buyback_price_kobo, bb.state AS buyback_state, bb.source AS buyback_source,
	       (SELECT COUNT(*) FROM surveillance_alerts x WHERE x.instrument_id = a.instrument_id AND x.session_date = a.session_date)::int AS alerts
	  FROM auctions a JOIN instruments i ON i.id = a.instrument_id
	  LEFT JOIN buyback_batches bb ON bb.instrument_id = a.instrument_id AND bb.session_date = a.session_date`

// darken blanks everything but the order count on a session still
// accepting orders. The book is dark by design: nobody, including the
// operator, sees the shape of the book before the freeze.
func darken(rows []row) {
	for _, r := range rows {
		if r["state"] != "accepting" {
			r["dark"] = false
			continue
		}
		r["dark"] = true
		for _, k := range []string{"orders_open", "prev_reference_kobo", "clearing_price_kobo", "matched_units",
			"imbalance_units", "imbalance_side", "rule", "book_hash", "result_hash", "trades", "notional_kobo"} {
			r[k] = nil
		}
	}
}

func (s *Server) sessions(ctx context.Context, r *http.Request) (any, error) {
	date, err := s.dateParam(r, "date")
	if err != nil {
		return nil, err
	}
	p := pageParams(r, 200)
	rows, err := collect(ctx, s.Pool, sessionSelect+`
		 WHERE a.session_date = $1::date AND ($2 = '' OR i.symbol > $2) ORDER BY i.symbol LIMIT $3`, date, p.Cursor, p.Limit+1)
	if err != nil {
		return nil, err
	}
	darken(rows)
	out := cut(rows, p, idKey("symbol"))
	out.Extra = row{"date": date}
	return out, nil
}

func (s *Server) orders(ctx context.Context, r *http.Request) (any, error) {
	q := r.URL.Query()
	date, err := s.dateParam(r, "date")
	if err != nil {
		return nil, err
	}
	p := pageParams(r, 100)
	ts, id := p.tsCursor()
	symbol, state := strings.ToUpper(q.Get("symbol")), q.Get("state")

	// Orders in a session still accepting are counted, never listed.
	dark, err := one(ctx, s.Pool, `
		SELECT COALESCE(SUM((SELECT COUNT(*) FROM orders o WHERE o.auction_id = a.id)), 0)::int AS dark_orders,
		       COUNT(*)::int AS dark_sessions
		  FROM auctions a JOIN instruments i ON i.id = a.instrument_id
		 WHERE a.state = 'accepting' AND a.session_date = $1::date AND ($2 = '' OR i.symbol = $2)`, date, symbol)
	if err != nil {
		return nil, err
	}
	rows, err := collect(ctx, s.Pool, `
		SELECT o.id::text AS id, i.symbol, o.side, o.type, o.tif, o.venue, o.state, o.limit_kobo, o.qty_units, o.filled_units,
		       o.notional_kobo, o.reserved_kobo, o.reserved_units, o.auction_seq, o.client_order_id, o.owner_key,
		       o.reject_reason, o.expires_on::text AS expires_on, o.created_at,
		       m.code AS member, ca.label AS client_account, a.session_date::text AS session_date, a.state AS session_state,
		       COALESCE(o.reserved_kobo, 0) AS _r
		  FROM orders o JOIN instruments i ON i.id = o.instrument_id
		  LEFT JOIN auctions a ON a.id = o.auction_id
		  LEFT JOIN members m ON m.id = o.member_id
		  LEFT JOIN client_accounts ca ON ca.id = o.client_account_id
		 WHERE (a.session_date = $1::date OR (o.auction_id IS NULL AND o.created_at::date = $1::date))
		   AND (a.state IS NULL OR a.state <> 'accepting')
		   AND ($2 = '' OR i.symbol = $2) AND ($3 = '' OR o.state = $3)
		   AND ($4::timestamptz IS NULL OR (o.created_at, o.id::text) < ($4, $5))
		 ORDER BY o.created_at DESC, o.id::text DESC LIMIT $6`, date, symbol, state, ts, id, p.Limit+1)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		delete(r, "_r")
	}
	out := cut(rows, p, tsKey("created_at", "id"))
	out.Extra = row{"date": date, "dark_orders": dark["dark_orders"], "dark_sessions": dark["dark_sessions"]}
	return out, nil
}

func (s *Server) fills(ctx context.Context, r *http.Request) (any, error) {
	date, err := s.dateParam(r, "date")
	if err != nil {
		return nil, err
	}
	p := pageParams(r, 100)
	rows, err := collect(ctx, s.Pool, `
		SELECT f.id, i.symbol, f.venue, f.side, f.units, f.price_kobo, f.consideration_kobo, f.fee_kobo,
		       f.order_id::text AS order_id, f.contra_order_id::text AS contra_order_id, f.ledger_tx_id::text AS ledger_tx_id,
		       f.session_date::text AS session_date, f.created_at, m.code AS member, o.owner_key
		  FROM fills f JOIN instruments i ON i.id = f.instrument_id
		  JOIN orders o ON o.id = f.order_id LEFT JOIN members m ON m.id = o.member_id
		 WHERE f.session_date = $1::date AND ($2 = '' OR i.symbol = $2) AND ($3 = 0 OR f.id < $3)
		 ORDER BY f.id DESC LIMIT $4`, date, strings.ToUpper(r.URL.Query().Get("symbol")), p.idCursor(), p.Limit+1)
	if err != nil {
		return nil, err
	}
	out := cut(rows, p, idKey("id"))
	out.Extra = row{"date": date}
	return out, nil
}

// ---------------------------------------------------------------- integrity

const alertSelect = `
	SELECT a.id, i.symbol, a.instrument_id, a.session_date::text AS session_date, a.detection, a.severity,
	       a.subject_group, a.state, a.assigned_to, a.raised_at, a.triaged_at, a.reviewed_at, a.closed_by,
	       a.outcome, a.notes, a.evidence
	  FROM surveillance_alerts a JOIN instruments i ON i.id = a.instrument_id`

func (s *Server) alerts(ctx context.Context, r *http.Request) (any, error) {
	q := r.URL.Query()
	p := pageParams(r, 100)
	rows, err := collect(ctx, s.Pool, alertSelect+`
		 WHERE ($1 = '' OR a.state = $1) AND ($2 = '' OR a.severity = $2) AND ($3 = '' OR i.symbol = $3)
		   AND ($4 = 0 OR a.id < $4)
		 ORDER BY a.id DESC LIMIT $5`, q.Get("state"), q.Get("severity"), strings.ToUpper(q.Get("symbol")), p.idCursor(), p.Limit+1)
	if err != nil {
		return nil, err
	}
	return cut(rows, p, idKey("id")), nil
}

func (s *Server) alert(ctx context.Context, r *http.Request) (any, error) {
	id, err := alertID(r)
	if err != nil {
		return nil, err
	}
	a, err := one(ctx, s.Pool, alertSelect+` WHERE a.id = $1`, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, notFound("No alert %d", id)
	}
	return a, err
}

func alertID(r *http.Request) (int64, error) {
	id, err := parseInt(chi.URLParam(r, "id"))
	if err != nil {
		return 0, invalid("Alert ids are integers")
	}
	return id, nil
}

func parseInt(s string) (int64, error) { return strconv.ParseInt(s, 10, 64) }

const haltSelect = `
	SELECT h.id::text AS id, i.symbol, h.instrument_id, h.reason, h.halted_at, h.released_at, h.raised_by, h.released_by, h.detail,
	       (h.released_at IS NULL) AS open
	  FROM trading_halts h JOIN instruments i ON i.id = h.instrument_id`

func (s *Server) halts(ctx context.Context, r *http.Request) (any, error) {
	p := pageParams(r, 100)
	ts, id := p.tsCursor()
	open, err := collect(ctx, s.Pool, haltSelect+` WHERE h.released_at IS NULL ORDER BY h.halted_at`)
	if err != nil {
		return nil, err
	}
	history, err := collect(ctx, s.Pool, haltSelect+`
		 WHERE h.released_at IS NOT NULL AND ($1::timestamptz IS NULL OR (h.halted_at, h.id::text) < ($1, $2))
		 ORDER BY h.halted_at DESC, h.id::text DESC LIMIT $3`, ts, id, p.Limit+1)
	if err != nil {
		return nil, err
	}
	out := cut(history, p, tsKey("halted_at", "id"))
	out.Extra = row{"open": open}
	return out, nil
}

func (s *Server) incidents(ctx context.Context, r *http.Request) (any, error) {
	p := pageParams(r, 100)
	state := r.URL.Query().Get("state")
	rows, err := collect(ctx, s.Pool, `
		SELECT d.id, i.symbol, d.instrument_id, d.kind, d.detail, d.raised_at, d.resolved_at, (d.resolved_at IS NULL) AS open
		  FROM data_quality_incidents d LEFT JOIN instruments i ON i.id = d.instrument_id
		 WHERE ($1 = 0 OR d.id < $1)
		   AND ($2 = '' OR ($2 = 'open' AND d.resolved_at IS NULL) OR ($2 = 'resolved' AND d.resolved_at IS NOT NULL))
		 ORDER BY d.id DESC LIMIT $3`, p.idCursor(), state, p.Limit+1)
	if err != nil {
		return nil, err
	}
	return cut(rows, p, idKey("id")), nil
}

func (s *Server) closedPeriods(ctx context.Context, r *http.Request) (any, error) {
	p := pageParams(r, 100)
	rows, err := collect(ctx, s.Pool, `
		SELECT cp.id::text AS id, i.symbol, cp.reason, cp.period::text AS period,
		       lower(cp.period)::text AS from_date, upper(cp.period)::text AS to_date,
		       (cp.period @> $1::date) AS active, cp.declared_by, cp.created_at
		  FROM closed_periods cp JOIN instruments i ON i.id = cp.instrument_id
		 ORDER BY lower(cp.period) DESC, i.symbol LIMIT $2`, s.today(), p.Limit)
	if err != nil {
		return nil, err
	}
	return listPage{Rows: rows}, nil
}

// ---------------------------------------------------------------- records

func (s *Server) listings(ctx context.Context, r *http.Request) (any, error) {
	p := pageParams(r, 100)
	ts, id := p.tsCursor()
	rows, err := collect(ctx, s.Pool, `
		SELECT a.id::text AS id, a.proposed_symbol AS symbol, a.state, a.findings, a.decided_by, a.decided_at, a.note, a.created_at,
		       c.id::text AS company_id, c.legal_name, c.rc_number, m.trading_name, m.external_ref AS merchant_ref, m.mcc,
		       sm.code AS sponsor, i.status AS instrument_status,
		       (SELECT COUNT(*) FROM jsonb_array_elements(a.findings) f WHERE (f->>'met')::boolean)::int AS met,
		       jsonb_array_length(a.findings)::int AS criteria,
		       COALESCE((SELECT SUM(funding_kobo) FROM buyback_intents bi WHERE bi.merchant_id = m.id AND bi.state = 'escrowed'), 0)::bigint AS escrowed_kobo
		  FROM listing_applications a JOIN companies c ON c.id = a.company_id
		  LEFT JOIN merchants m ON m.id = a.merchant_id
		  LEFT JOIN members sm ON sm.id = a.sponsor_member
		  LEFT JOIN instruments i ON i.symbol = a.proposed_symbol
		 WHERE ($1 = '' OR a.state = $1) AND ($2::timestamptz IS NULL OR (a.created_at, a.id::text) < ($2, $3))
		 ORDER BY a.created_at DESC, a.id::text DESC LIMIT $4`, r.URL.Query().Get("state"), ts, id, p.Limit+1)
	if err != nil {
		return nil, err
	}
	return cut(rows, p, tsKey("created_at", "id")), nil
}

// gateRows assesses every listed instrument against the standard.
func (s *Server) gateRows(ctx context.Context, date string) ([]row, error) {
	var out []row
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		insts, err := collect(ctx, tx, `
			SELECT id AS instrument_id, symbol, clob_enabled, clob_review_state, listed_at
			  FROM instruments WHERE status = 'listed' ORDER BY symbol`)
		if err != nil {
			return err
		}
		out = []row{}
		for _, in := range insts {
			rep, err := s.Gate.Assess(ctx, tx, str(in["instrument_id"]), date)
			if err != nil {
				return err
			}
			g := s.gateReport(rep, in["clob_enabled"] == true)
			for k, v := range in {
				g[k] = v
			}
			g["failing"] = len(rep.Failures)
			out = append(out, g)
		}
		return nil
	})
	return out, err
}

func (s *Server) gate(ctx context.Context, r *http.Request) (any, error) {
	rows, err := s.gateRows(ctx, s.today())
	if err != nil {
		return nil, err
	}
	return listPage{Rows: rows, Extra: row{"as_of": s.today(), "standard": s.Gate}}, nil
}

const providerSelect = `
	SELECT lp.id::text AS provider_id, i.symbol, lp.instrument_id, m.id::text AS member_id, m.code AS member, m.legal_name AS member_name,
	       lp.min_quote_kobo, lp.max_spread_bps, lp.min_uptime_bps, lp.rebate_bps, lp.effective::text AS effective,
	       lower(lp.effective)::text AS from_date, upper(lp.effective)::text AS to_date, lp.state, lp.created_at,
	       t.quoted AS today_quoted, t.two_sided AS today_two_sided, t.bid_kobo AS today_bid_kobo, t.ask_kobo AS today_ask_kobo,
	       t.spread_bps AS today_spread_bps, t.size_kobo AS today_size_kobo, t.met AS today_met, t.shortfall AS today_shortfall
	  FROM liquidity_providers lp JOIN instruments i ON i.id = lp.instrument_id JOIN members m ON m.id = lp.member_id
	  LEFT JOIN lp_performance t ON t.provider_id = lp.id AND t.session_date = $1::date`

func (s *Server) providerRows(ctx context.Context, q pgx.Tx, instrumentID, date string) ([]row, error) {
	rows, err := collect(ctx, q, providerSelect+`
		 WHERE ($2 = '' OR lp.instrument_id = $2) ORDER BY i.symbol, m.code`, date, instrumentID)
	if err != nil {
		return nil, err
	}
	for _, pr := range rows {
		id, err := uuid.Parse(str(pr["provider_id"]))
		if err != nil {
			return nil, err
		}
		bps, n, err := exchange.Uptime(ctx, q, id, 20)
		if err != nil {
			return nil, err
		}
		pr["uptime_bps"], pr["uptime_sessions"] = bps, n
	}
	return rows, nil
}

func (s *Server) providers(ctx context.Context, r *http.Request) (any, error) {
	var rows []row
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		rows, err = s.providerRows(ctx, tx, "", s.today())
		return err
	})
	if err != nil {
		return nil, err
	}
	return listPage{Rows: rows, Extra: row{"date": s.today()}}, nil
}

func (s *Server) members(ctx context.Context, r *http.Request) (any, error) {
	p := pageParams(r, 100)
	rows, err := collect(ctx, s.Pool, `
		SELECT m.id::text AS id, m.code, m.legal_name, m.roles, m.status, m.halted, m.halted_reason,
		       m.max_orders_per_session, m.max_order_to_trade_ratio, m.created_at,
		       COALESCE(ma.orders_sent, 0) AS orders_sent, COALESCE(ma.orders_filled, 0) AS orders_filled,
		       COALESCE(ma.cancels, 0) AS cancels, COALESCE(ma.rejects, 0) AS rejects,
		       (SELECT COUNT(*) FROM client_accounts ca WHERE ca.member_id = m.id)::int AS client_accounts,
		       (SELECT COUNT(*) FROM liquidity_providers lp WHERE lp.member_id = m.id AND lp.state IN ('active','warned'))::int AS mm_appointments
		  FROM members m LEFT JOIN member_activity ma ON ma.member_id = m.id AND ma.session_date = $1::date
		 WHERE ($2 = '' OR m.code > $2) ORDER BY m.code LIMIT $3`, s.today(), p.Cursor, p.Limit+1)
	if err != nil {
		return nil, err
	}
	out := cut(rows, p, idKey("code"))
	out.Extra = row{"date": s.today()}
	return out, nil
}

// ---------------------------------------------------------------- post-trade

func (s *Server) buyback(ctx context.Context, r *http.Request) (any, error) {
	date, err := s.dateParam(r, "date")
	if err != nil {
		return nil, err
	}
	batches, err := collect(ctx, s.Pool, `
		SELECT i.symbol, i.id AS instrument_id, bb.id::text AS batch_id, bb.session_date::text AS session_date,
		       bb.funding_kobo, bb.units_bought, bb.price_kobo, bb.source, bb.state, bb.created_at,
		       (SELECT COUNT(*) FROM buyback_intents bi WHERE bi.batch_id = bb.id)::int AS intents,
		       (SELECT COALESCE(SUM(residual_kobo),0) FROM buyback_intents bi WHERE bi.batch_id = bb.id)::bigint AS residual_kobo,
		       COALESCE(tr.units, 0) AS released_units, tp.daily_release_units, i.reference_price_kobo,
		       a.state AS session_state, a.clearing_price_kobo AS session_price_kobo
		  FROM instruments i
		  LEFT JOIN buyback_batches bb ON bb.instrument_id = i.id AND bb.session_date = $1::date
		  LEFT JOIN treasury_releases tr ON tr.instrument_id = i.id AND tr.session_date = $1::date
		  LEFT JOIN treasury_pools tp ON tp.instrument_id = i.id
		  LEFT JOIN auctions a ON a.instrument_id = i.id AND a.session_date = $1::date
		 WHERE i.status IN ('listed','halted','suspended') ORDER BY i.symbol`, date)
	if err != nil {
		return nil, err
	}
	intents, err := collect(ctx, s.Pool, `
		SELECT COALESCE(i.symbol, '—') AS symbol, bi.state, COUNT(*)::int AS intents,
		       SUM(bi.funding_kobo)::bigint AS funding_kobo, COALESCE(SUM(bi.allocated_units),0)::bigint AS units,
		       COALESCE(SUM(bi.residual_kobo),0)::bigint AS residual_kobo
		  FROM buyback_intents bi LEFT JOIN instruments i ON i.id = bi.instrument_id
		 GROUP BY 1, 2 ORDER BY 1, 2`)
	if err != nil {
		return nil, err
	}
	pipeline, err := collect(ctx, s.Pool, `
		SELECT m.id::text AS merchant_id, m.trading_name, m.legal_name, m.external_ref AS merchant_ref, m.mcc, m.kyb_status,
		       COUNT(*)::int AS intents, SUM(bi.funding_kobo)::bigint AS escrowed_kobo, MIN(bi.created_at) AS since,
		       la.state AS application_state, la.proposed_symbol
		  FROM buyback_intents bi JOIN merchants m ON m.id = bi.merchant_id
		  LEFT JOIN listing_applications la ON la.merchant_id = m.id
		 WHERE bi.state = 'escrowed' AND bi.instrument_id IS NULL
		 GROUP BY m.id, la.state, la.proposed_symbol ORDER BY escrowed_kobo DESC`)
	if err != nil {
		return nil, err
	}
	pool, err := one(ctx, s.Pool, `
		SELECT COALESCE((SELECT balance FROM account_balances WHERE owner_type = 'scheme' AND kind = 'buyback_pool' AND asset_id = 'NGN'), 0)::bigint AS pool_kobo,
		       COALESCE((SELECT balance FROM account_balances WHERE owner_type = 'scheme' AND kind = 'buyback_loss_reserve' AND asset_id = 'NGN'), 0)::bigint AS loss_reserve_kobo`)
	if err != nil {
		return nil, err
	}
	return row{"date": date, "batches": batches, "intents": intents, "pipeline": pipeline, "pool": pool}, nil
}

func (s *Server) settlement(ctx context.Context, r *http.Request) (any, error) {
	batch, err := one(ctx, s.Pool, `
		SELECT cb.id::text AS batch_id, cb.business_date::text AS business_date, cb.cycle, cb.state, cb.cutoff_at, cb.finalised_at,
		       (SELECT COUNT(*) FROM presentments p WHERE p.clearing_batch_id = cb.id)::int AS presentments,
		       (SELECT COALESCE(SUM(amount_kobo),0) FROM presentments p WHERE p.clearing_batch_id = cb.id)::bigint AS gross_kobo
		  FROM clearing_batches cb ORDER BY cb.business_date DESC, cb.cycle DESC LIMIT 1`)
	if errors.Is(err, pgx.ErrNoRows) {
		return row{"batch": nil, "positions": []row{}, "instructions": []row{}}, nil
	}
	if err != nil {
		return nil, err
	}
	positions, err := collect(ctx, s.Pool, `
		SELECT sp.id::text AS id, p.code, p.legal_name, p.roles, sp.net_kobo, sp.acquired_kobo, sp.issued_kobo,
		       sp.interchange_kobo, sp.fees_kobo, sp.state, sp.cap_breach_kobo, p.net_debit_cap_kobo,
		       sp.instruction_id::text AS instruction_id, sp.computed_at, sp.settled_at
		  FROM settlement_positions sp JOIN participants p ON p.id = sp.participant_id
		 WHERE sp.batch_id = $1 ORDER BY p.code`, batch["batch_id"])
	if err != nil {
		return nil, err
	}
	instructions, err := collect(ctx, s.Pool, `
		SELECT si.id::text AS id, si.direction, si.amount_kobo, si.rail, si.external_ref, si.state, si.created_at,
		       a.owner_type, a.kind AS account_kind
		  FROM settlement_instructions si JOIN accounts a ON a.id = si.account_id
		 ORDER BY si.created_at DESC LIMIT 50`)
	if err != nil {
		return nil, err
	}
	return row{"batch": batch, "positions": positions, "instructions": instructions}, nil
}

func (s *Server) recon(ctx context.Context, r *http.Request) (any, error) {
	runs, err := collect(ctx, s.Pool, `
		SELECT id::text AS id, business_date::text AS business_date, kind, checked, breaks, ran_at
		  FROM recon_runs ORDER BY ran_at DESC LIMIT 50`)
	if err != nil {
		return nil, err
	}
	breaks, err := collect(ctx, s.Pool, `
		SELECT b.id, b.run_id::text AS run_id, r.business_date::text AS business_date, r.kind AS run_kind, b.kind, b.subject,
		       b.expected, b.actual, b.detail, b.state, b.resolved_by, b.resolved_at, b.note, r.ran_at
		  FROM recon_breaks b JOIN recon_runs r ON r.id = b.run_id
		 ORDER BY (b.state = 'open') DESC, b.id DESC LIMIT 200`)
	if err != nil {
		return nil, err
	}
	return row{"runs": runs, "breaks": breaks}, nil
}

// ---------------------------------------------------------------- institution

const corporateActionSelect = `
	SELECT ca.id::text AS id, i.symbol, ca.instrument_id, ca.kind, ca.ratio_num, ca.ratio_den, ca.dps_kobo, ca.wht_bps,
	       ca.announced_on::text AS announced_on, ca.ex_date::text AS ex_date, ca.record_date::text AS record_date,
	       ca.pay_date::text AS pay_date, ca.state, ca.ledger_tx_id::text AS ledger_tx_id, ca.created_at,
	       (SELECT COUNT(*) FROM corporate_action_entitlements e WHERE e.action_id = ca.id)::int AS entitlements,
	       (SELECT COALESCE(SUM(gross_kobo),0) FROM corporate_action_entitlements e WHERE e.action_id = ca.id)::bigint AS gross_kobo,
	       (SELECT COALESCE(SUM(wht_kobo),0) FROM corporate_action_entitlements e WHERE e.action_id = ca.id)::bigint AS wht_kobo
	  FROM corporate_actions ca JOIN instruments i ON i.id = ca.instrument_id`

func (s *Server) corporateActions(ctx context.Context, r *http.Request) (any, error) {
	p := pageParams(r, 100)
	ts, id := p.tsCursor()
	rows, err := collect(ctx, s.Pool, corporateActionSelect+`
		 WHERE ($1::timestamptz IS NULL OR (ca.created_at, ca.id::text) < ($1, $2))
		 ORDER BY ca.created_at DESC, ca.id::text DESC LIMIT $3`, ts, id, p.Limit+1)
	if err != nil {
		return nil, err
	}
	return cut(rows, p, tsKey("created_at", "id")), nil
}

const disclosureSelect = `
	SELECT d.id::text AS id, i.symbol, d.instrument_id, d.kind, d.headline, d.body, d.submitted_by, d.submitted_at,
	       d.published_at, d.halt_id::text AS halt_id, (d.published_at IS NULL) AS pending,
	       (h.id IS NOT NULL AND h.released_at IS NULL) AS holding_halt
	  FROM disclosures d JOIN instruments i ON i.id = d.instrument_id
	  LEFT JOIN trading_halts h ON h.id = d.halt_id`

func (s *Server) disclosures(ctx context.Context, r *http.Request) (any, error) {
	p := pageParams(r, 100)
	ts, id := p.tsCursor()
	rows, err := collect(ctx, s.Pool, disclosureSelect+`
		 WHERE ($1::timestamptz IS NULL OR (d.submitted_at, d.id::text) < ($1, $2))
		 ORDER BY d.submitted_at DESC, d.id::text DESC LIMIT $3`, ts, id, p.Limit+1)
	if err != nil {
		return nil, err
	}
	return cut(rows, p, tsKey("submitted_at", "id")), nil
}

func (s *Server) calendar(ctx context.Context, r *http.Request) (any, error) {
	q := r.URL.Query()
	from, to := q.Get("from"), q.Get("to")
	today := s.today()
	if from == "" {
		from = today[:8] + "01"
	}
	if to == "" {
		to = today[:4] + "-12-31"
	}
	rows, err := collect(ctx, s.Pool, `
		SELECT tc.session_date::text AS session_date, tc.is_trading, tc.opens_at, tc.freezes_at, tc.note,
		       to_char(tc.session_date, 'Dy') AS weekday,
		       (SELECT COUNT(*) FROM auctions a WHERE a.session_date = tc.session_date)::int AS sessions,
		       (SELECT COUNT(*) FROM auctions a WHERE a.session_date = tc.session_date AND a.state = 'published')::int AS published,
		       (SELECT COUNT(*) FROM auctions a WHERE a.session_date = tc.session_date AND a.state IN ('halted','cancelled'))::int AS abandoned,
		       (SELECT COUNT(*) FROM closed_periods cp WHERE cp.period @> tc.session_date)::int AS closed_periods
		  FROM trading_calendar tc
		 WHERE tc.session_date BETWEEN $1::date AND $2::date ORDER BY tc.session_date`, from, to)
	if err != nil {
		return nil, err
	}
	return listPage{Rows: rows, Extra: row{"from": from, "to": to, "today": today}}, nil
}

func (s *Server) index(ctx context.Context, r *http.Request) (any, error) {
	indices, err := collect(ctx, s.Pool, `
		SELECT id, name, base_date::text AS base_date, base_value, method, max_weight_bps, created_at,
		       (SELECT COUNT(*) FROM index_constituents c WHERE c.index_id = i.id AND c.removed_on IS NULL)::int AS constituents,
		       (SELECT value_bps FROM index_values v WHERE v.index_id = i.id ORDER BY obs_date DESC LIMIT 1) AS latest_bps,
		       (SELECT obs_date::text FROM index_values v WHERE v.index_id = i.id ORDER BY obs_date DESC LIMIT 1) AS latest_date
		  FROM indices i ORDER BY id`)
	if err != nil {
		return nil, err
	}
	for _, ix := range indices {
		if ix["values"], err = collect(ctx, s.Pool, `
			SELECT * FROM (SELECT obs_date::text AS obs_date, value_bps, constituents, computed_at
			  FROM index_values WHERE index_id = $1 ORDER BY obs_date DESC LIMIT 90) v ORDER BY obs_date`, ix["id"]); err != nil {
			return nil, err
		}
		if ix["members"], err = collect(ctx, s.Pool, `
			SELECT i.symbol, c.added_on::text AS added_on, c.removed_on::text AS removed_on, i.reference_price_kobo, i.status
			  FROM index_constituents c JOIN instruments i ON i.id = c.instrument_id
			 WHERE c.index_id = $1 ORDER BY c.removed_on IS NOT NULL, i.symbol`, ix["id"]); err != nil {
			return nil, err
		}
	}
	return row{"indices": indices}, nil
}

func (s *Server) complaints(ctx context.Context, r *http.Request) (any, error) {
	p := pageParams(r, 100)
	ts, id := p.tsCursor()
	rows, err := collect(ctx, s.Pool, `
		SELECT c.id::text AS id, ch.display_name AS complainant, COALESCE(ch.external_ref, ch.id::text) AS complainant_ref,
		       c.against, m.code AS member, i.symbol, c.subject, c.narrative, c.state, c.respond_by::text AS respond_by,
		       (c.respond_by < $1::date AND c.state NOT IN ('resolved','withdrawn')) AS overdue,
		       c.resolved_at, c.resolution, c.handled_by, c.created_at
		  FROM complaints c LEFT JOIN cardholders ch ON ch.id = c.complainant
		  LEFT JOIN members m ON m.id = c.member_id LEFT JOIN instruments i ON i.id = c.instrument_id
		 WHERE ($2::timestamptz IS NULL OR (c.created_at, c.id::text) < ($2, $3))
		 ORDER BY c.created_at DESC, c.id::text DESC LIMIT $4`, s.today(), ts, id, p.Limit+1)
	if err != nil {
		return nil, err
	}
	return cut(rows, p, tsKey("created_at", "id")), nil
}

func (s *Server) protection(ctx context.Context, r *http.Request) (any, error) {
	p := pageParams(r, 100)
	ts, id := p.tsCursor()
	rows, err := collect(ctx, s.Pool, `
		SELECT pc.id::text AS id, ch.display_name AS claimant, COALESCE(ch.external_ref, ch.id::text) AS claimant_ref,
		       m.code AS member, pc.amount_kobo, pc.awarded_kobo, pc.grounds, pc.narrative, pc.state,
		       pc.decided_by, pc.decided_at, pc.ledger_tx_id::text AS ledger_tx_id, pc.created_at
		  FROM protection_claims pc JOIN cardholders ch ON ch.id = pc.claimant LEFT JOIN members m ON m.id = pc.member_id
		 WHERE ($1::timestamptz IS NULL OR (pc.created_at, pc.id::text) < ($1, $2))
		 ORDER BY pc.created_at DESC, pc.id::text DESC LIMIT $3`, ts, id, p.Limit+1)
	if err != nil {
		return nil, err
	}
	fund, err := one(ctx, s.Pool, `
		SELECT COALESCE((SELECT balance FROM account_balances WHERE owner_type = 'scheme' AND kind = 'loss_reserve' AND asset_id = 'NGN'), 0)::bigint AS fund_kobo`)
	if err != nil {
		return nil, err
	}
	out := cut(rows, p, tsKey("created_at", "id"))
	out.Extra = fund
	return out, nil
}

func (s *Server) clock(ctx context.Context, r *http.Request) (any, error) {
	rows, err := collect(ctx, s.Pool, `
		SELECT id, checked_at, source, offset_ms, within_bounds FROM clock_checks ORDER BY id DESC LIMIT 50`)
	if err != nil {
		return nil, err
	}
	return listPage{Rows: rows, Extra: row{"server_now": s.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00")}}, nil
}
