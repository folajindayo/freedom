package exchange

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"freedom/api/internal/money"
)

// Designated market makers.
//
// An SME order book has buyers arriving automatically — the buyback, every
// session — and almost no natural sellers. Without somebody obliged to quote,
// most sessions uncross at zero volume, the reference carries forward for
// weeks, and the buyback spends against a price nobody traded at.
//
// # The obligation is per session, not per second
//
// A symbol that trades once a day cannot be quoted continuously. So the duty is
// to place a two-sided quote into every auction: at least this much size, no
// wider than this spread, in at least this share of sessions. That is
// measurable on an auction-only venue, which continuous-quoting obligations
// are not.
//
// # And it is measured
//
// An obligation nobody measures is a claim in a prospectus that cannot be
// defended. A market maker who quoted on the days it suited them and stood
// aside on the days the price was moving has met no obligation at all, and only
// a per-session record shows that.
//
// # Independence
//
// A provider must not be a related party of the issuer. An obligation to quote,
// held by somebody who benefits from the price, is the manipulation vector
// wearing a badge.

// ErrNotIndependent is returned when a proposed provider is connected to the
// issuer.
var ErrNotIndependent = errors.New("exchange: a market maker must be independent of the issuer")

// Obligation is what a provider commits to.
type Obligation struct {
	InstrumentID string
	MemberID     uuid.UUID
	MinQuote     money.Kobo
	MaxSpreadBps int64
	MinUptimeBps int64
	RebateBps    int64
	From, To     string
}

// StandardObligation is the launch commitment: ₦50,000 a side, no wider than
// 5%, present in at least 80% of sessions.
func StandardObligation(instrumentID string, member uuid.UUID, from, to string) Obligation {
	return Obligation{
		InstrumentID: instrumentID, MemberID: member,
		MinQuote: money.Naira(50_000), MaxSpreadBps: 500, MinUptimeBps: 8000,
		RebateBps: 2500, From: from, To: to,
	}
}

// Appoint registers a market maker against a symbol.
func Appoint(ctx context.Context, tx pgx.Tx, o Obligation) (uuid.UUID, error) {
	if o.MinQuote <= 0 || o.MaxSpreadBps <= 0 {
		return uuid.Nil, fmt.Errorf("exchange: an obligation needs a size and a spread")
	}

	// Independence, checked against every account the member trades for.
	var related bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM related_parties rp
		   JOIN accounts a ON a.id = rp.account_id
		   JOIN client_accounts ca ON ca.cardholder_id = a.owner_id
		  WHERE rp.instrument_id = $1 AND ca.member_id = $2)`,
		o.InstrumentID, o.MemberID).Scan(&related); err != nil {
		return uuid.Nil, fmt.Errorf("exchange: independence check: %w", err)
	}
	if related {
		return uuid.Nil, fmt.Errorf("%w: %s", ErrNotIndependent, o.MemberID)
	}

	var isMM bool
	if err := tx.QueryRow(ctx,
		`SELECT 'market_maker' = ANY(roles) FROM members WHERE id = $1`, o.MemberID).Scan(&isMM); err != nil {
		return uuid.Nil, fmt.Errorf("exchange: load member: %w", err)
	}
	if !isMM {
		return uuid.Nil, fmt.Errorf("exchange: member %s is not registered as a market maker", o.MemberID)
	}

	var id uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO liquidity_providers
		  (instrument_id, member_id, min_quote_kobo, max_spread_bps, min_uptime_bps, rebate_bps, effective)
		VALUES ($1,$2,$3,$4,$5,$6,daterange($7::date,$8::date,'[]'))
		RETURNING id`,
		o.InstrumentID, o.MemberID, int64(o.MinQuote), o.MaxSpreadBps,
		o.MinUptimeBps, o.RebateBps, o.From, o.To).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("exchange: appoint market maker: %w", err)
	}
	return id, nil
}

// SessionPerformance is one provider's showing in one session.
type SessionPerformance struct {
	ProviderID uuid.UUID
	Quoted     bool
	TwoSided   bool
	Bid, Ask   money.Kobo
	SpreadBps  int64
	Size       money.Kobo
	Met        bool
	Shortfall  string
}

// MeasureSession records whether each provider met its obligation.
//
// Run after the book freezes and before the uncross, because the obligation is
// about what was quoted — a provider whose quote was never going to trade still
// met their duty by being there, and one who pulled their quote at the last
// moment did not, however the session turned out.
func MeasureSession(ctx context.Context, tx pgx.Tx, instrumentID, sessionDate string) ([]SessionPerformance, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, member_id, min_quote_kobo, max_spread_bps
		  FROM liquidity_providers
		 WHERE instrument_id = $1 AND effective @> $2::date AND state IN ('active','warned')`,
		instrumentID, sessionDate)
	if err != nil {
		return nil, fmt.Errorf("exchange: load providers: %w", err)
	}
	type provider struct {
		id        uuid.UUID
		member    uuid.UUID
		minQuote  money.Kobo
		maxSpread int64
	}
	var providers []provider
	for rows.Next() {
		var p provider
		if err := rows.Scan(&p.id, &p.member, &p.minQuote, &p.maxSpread); err != nil {
			rows.Close()
			return nil, err
		}
		providers = append(providers, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var out []SessionPerformance
	for _, p := range providers {
		perf := SessionPerformance{ProviderID: p.id}

		// The best two-sided quote this member put into the session.
		var bid, ask, bidSize, askSize *int64
		err := tx.QueryRow(ctx, `
			SELECT MAX(o.limit_kobo) FILTER (WHERE o.side = 'buy'),
			       MIN(o.limit_kobo) FILTER (WHERE o.side = 'sell'),
			       SUM(o.limit_kobo * o.qty_units / 100000000) FILTER (WHERE o.side = 'buy'),
			       SUM(o.limit_kobo * o.qty_units / 100000000) FILTER (WHERE o.side = 'sell')
			  FROM orders o JOIN auctions a ON a.id = o.auction_id
			 WHERE a.instrument_id = $1 AND a.session_date = $2::date
			   AND o.member_id = $3 AND o.type = 'limit'
			   AND o.state NOT IN ('rejected','cancelled')`,
			instrumentID, sessionDate, p.member).Scan(&bid, &ask, &bidSize, &askSize)
		if err != nil {
			return nil, fmt.Errorf("exchange: measure provider: %w", err)
		}

		switch {
		case bid == nil && ask == nil:
			perf.Shortfall = "did not quote"
		case bid == nil || ask == nil:
			perf.Quoted = true
			perf.Shortfall = "quoted one side only"
		default:
			perf.Quoted, perf.TwoSided = true, true
			perf.Bid, perf.Ask = money.Kobo(*bid), money.Kobo(*ask)
			mid := (*bid + *ask) / 2
			if mid > 0 {
				perf.SpreadBps = (*ask - *bid) * 10_000 / mid
			}
			smaller := *bidSize
			if askSize != nil && *askSize < smaller {
				smaller = *askSize
			}
			perf.Size = money.Kobo(smaller)

			switch {
			case perf.SpreadBps > p.maxSpread:
				perf.Shortfall = fmt.Sprintf("spread %d bps exceeds %d", perf.SpreadBps, p.maxSpread)
			case perf.Size < p.minQuote:
				perf.Shortfall = fmt.Sprintf("size %s below the %s minimum", perf.Size, p.minQuote)
			default:
				perf.Met = true
			}
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO lp_performance
			  (provider_id, session_date, quoted, two_sided, bid_kobo, ask_kobo,
			   spread_bps, size_kobo, met, shortfall)
			VALUES ($1,$2::date,$3,$4,$5,$6,$7,$8,$9,$10)
			ON CONFLICT (provider_id, session_date) DO UPDATE
			  SET quoted = EXCLUDED.quoted, two_sided = EXCLUDED.two_sided,
			      bid_kobo = EXCLUDED.bid_kobo, ask_kobo = EXCLUDED.ask_kobo,
			      spread_bps = EXCLUDED.spread_bps, size_kobo = EXCLUDED.size_kobo,
			      met = EXCLUDED.met, shortfall = EXCLUDED.shortfall`,
			p.id, sessionDate, perf.Quoted, perf.TwoSided,
			nullableKobo(perf.Bid), nullableKobo(perf.Ask),
			nullableInt(perf.SpreadBps), nullableKobo(perf.Size),
			perf.Met, nullableString(perf.Shortfall)); err != nil {
			return nil, fmt.Errorf("exchange: record performance: %w", err)
		}
		out = append(out, perf)
	}
	return out, nil
}

// Uptime is the share of recent sessions in which a provider met its
// obligation, in basis points.
func Uptime(ctx context.Context, tx pgx.Tx, providerID uuid.UUID, window int) (int64, int, error) {
	var met, total int
	err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FILTER (WHERE met), COUNT(*)
		  FROM (SELECT met FROM lp_performance
		         WHERE provider_id = $1 ORDER BY session_date DESC LIMIT $2) recent`,
		providerID, window).Scan(&met, &total)
	if err != nil {
		return 0, 0, fmt.Errorf("exchange: uptime: %w", err)
	}
	if total == 0 {
		return 0, 0, nil
	}
	return int64(met) * 10_000 / int64(total), total, nil
}

// ReviewProviders escalates or restores providers based on recent uptime.
//
// Warned before suspended, deliberately. A market maker having a bad month is
// far more common than one abandoning its obligation, and suspending on the
// first breach would cost the symbol its only liquidity at exactly the moment
// it needs it most.
func ReviewProviders(ctx context.Context, tx pgx.Tx, window, minSessions int) (warned, suspended, restored int, err error) {
	rows, err := tx.Query(ctx, `
		SELECT id, min_uptime_bps, state FROM liquidity_providers
		 WHERE state IN ('active','warned')`)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("exchange: load providers for review: %w", err)
	}
	type row struct {
		id    uuid.UUID
		min   int64
		state string
	}
	var providers []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.min, &r.state); err != nil {
			rows.Close()
			return 0, 0, 0, err
		}
		providers = append(providers, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, 0, err
	}

	for _, r := range providers {
		uptime, sessions, err := Uptime(ctx, tx, r.id, window)
		if err != nil {
			return warned, suspended, restored, err
		}
		// Too few sessions to judge. A provider appointed last week has not
		// failed; there simply is not enough evidence yet.
		if sessions < minSessions {
			continue
		}

		var next string
		switch {
		case uptime >= r.min && r.state == "warned":
			next, restored = "active", restored+1
		case uptime < r.min && r.state == "active":
			next, warned = "warned", warned+1
		case uptime < r.min && r.state == "warned":
			next, suspended = "suspended", suspended+1
		default:
			continue
		}
		if _, err := tx.Exec(ctx,
			`UPDATE liquidity_providers SET state = $2 WHERE id = $1`, r.id, next); err != nil {
			return warned, suspended, restored, fmt.Errorf("exchange: update provider state: %w", err)
		}
	}
	return warned, suspended, restored, nil
}
