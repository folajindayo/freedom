package exchange

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"freedom/api/internal/money"
	"freedom/api/internal/share"
)

// Market surveillance.
//
// On a thin SME book one participant can move the print for pocket change, so
// this matters more here than on a liquid market, not less. Every detection
// below runs off data the exchange already keeps — the order event log and the
// fill record — rather than off a statistical model that would need tuning
// nobody can defend to a regulator.
//
// The most valuable of them is only possible because the matching engine is a
// pure function. See MarkTheAuction.

// Detection names, recorded on the alert.
const (
	DetectMarkTheAuction = "mark_the_auction"
	DetectWashTrading    = "wash_trading"
	DetectRamping        = "ramping"
	DetectOrderToTrade   = "order_to_trade_ratio"
	DetectClosedPeriod   = "closed_period_trade"
)

// Alert severities. Only `block` stops the buyback from pricing against the
// instrument; the others are for a human to work through.
const (
	SevInfo  = "info"
	SevWarn  = "warn"
	SevBlock = "block"
)

// Alert is a raised suspicion.
type Alert struct {
	ID           int64
	InstrumentID string
	SessionDate  string
	Detection    string
	Severity     string
	SubjectGroup string
	Evidence     map[string]any
}

// Surveillance runs detections over a settled session.
type Surveillance struct {
	// MarkBps is how far a single account may move the clearing price by its
	// presence before it is treated as marking. 300bp on a thin book.
	MarkBps int64
	// MarkMaxVolumeShare is the volume share below which an account moving the
	// price that far is suspicious rather than simply large. An account
	// supplying most of the volume SHOULD move the price — that is a market
	// working, not an abuse.
	MarkMaxVolumeShareBps int64
	// WashNetBps is how close to flat a participant's net position must be,
	// relative to their gross activity, to look like a wash.
	WashNetBps int64
	// RampBps is how far above the adjusted trailing average a print may sit
	// before it is treated as a ramp.
	RampBps int64
}

// NewSurveillance returns the launch thresholds.
func NewSurveillance() *Surveillance {
	return &Surveillance{
		MarkBps:               300,
		MarkMaxVolumeShareBps: 1000, // 10%
		WashNetBps:            500,  // net within 5% of gross
		RampBps:               2000, // 20% above the trailing average
	}
}

// RunSession runs every detection over one settled auction and raises alerts.
func (s *Surveillance) RunSession(ctx context.Context, tx pgx.Tx, instrumentID, sessionDate string) ([]Alert, error) {
	var raised []Alert

	marks, err := s.MarkTheAuction(ctx, tx, instrumentID, sessionDate)
	if err != nil {
		return nil, err
	}
	raised = append(raised, marks...)

	washes, err := s.WashTrading(ctx, tx, instrumentID, sessionDate)
	if err != nil {
		return nil, err
	}
	raised = append(raised, washes...)

	ramps, err := s.Ramping(ctx, tx, instrumentID, sessionDate)
	if err != nil {
		return nil, err
	}
	raised = append(raised, ramps...)

	for i := range raised {
		id, err := raise(ctx, tx, raised[i])
		if err != nil {
			return nil, err
		}
		raised[i].ID = id
	}
	return raised, nil
}

// MarkTheAuction detects a participant setting the price rather than taking it.
//
// This is the detection the engine's purity was worth paying for. Instead of
// asking a model whether an order "looks" manipulative, it asks a question with
// an exact answer: re-run the uncross with that account's orders removed and
// see whether the price moves. An account whose presence moves the clearing
// price materially while supplying only a small share of the volume is, by
// definition, setting the price — there is no interpretation involved.
//
// The volume-share test is what keeps it fair. A participant supplying most of
// a session's liquidity SHOULD move the price; that is a market working.
func (s *Surveillance) MarkTheAuction(ctx context.Context, tx pgx.Tx, instrumentID, sessionDate string) ([]Alert, error) {
	sess, book, orders, err := loadSettledSession(ctx, tx, instrumentID, sessionDate)
	if err != nil {
		return nil, err
	}
	if sess == nil || len(book) == 0 {
		return nil, nil
	}

	actual, err := Cross(book, sess.Params)
	if err != nil {
		return nil, err
	}
	if !actual.Determined || actual.Exec == 0 {
		return nil, nil
	}

	// Group by beneficial owner, so splitting an order across two accounts of
	// the same group does not evade the test.
	byGroup := map[string][]int64{}
	volume := map[string]share.Units{}
	for _, o := range orders {
		key := o.OwnerKey
		if key == "" {
			key = "account:" + o.CardholderID.String()
		}
		byGroup[key] = append(byGroup[key], o.Seq)
	}
	for _, f := range actual.Fills {
		o, ok := orders[f.Seq]
		if !ok {
			continue
		}
		key := o.OwnerKey
		if key == "" {
			key = "account:" + o.CardholderID.String()
		}
		volume[key] += f.Units
	}

	var out []Alert
	for group, seqs := range byGroup {
		counter := make([]Order, 0, len(book))
		drop := map[int64]bool{}
		for _, q := range seqs {
			drop[q] = true
		}
		for _, o := range book {
			if !drop[o.Seq] {
				counter = append(counter, o)
			}
		}

		without, err := Cross(counter, sess.Params)
		if err != nil {
			return nil, err
		}

		moved := priceMoveBps(actual.Price, without.Price, without.Determined)
		if moved < s.MarkBps {
			continue
		}
		// Did they earn that influence with volume?
		shareBps := int64(0)
		if actual.Exec > 0 {
			shareBps = int64(volume[group]) * 10_000 / int64(actual.Exec)
		}
		if shareBps > s.MarkMaxVolumeShareBps {
			continue
		}

		sev := SevWarn
		if moved >= s.MarkBps*3 {
			sev = SevBlock
		}
		out = append(out, Alert{
			InstrumentID: instrumentID, SessionDate: sessionDate,
			Detection: DetectMarkTheAuction, Severity: sev, SubjectGroup: group,
			Evidence: map[string]any{
				"price_with":         int64(actual.Price),
				"price_without":      int64(without.Price),
				"determined_without": without.Determined,
				"moved_bps":          moved,
				"volume_share_bps":   shareBps,
				"orders":             len(seqs),
			},
		})
	}
	return out, nil
}

// priceMoveBps measures how far the price moved when a participant was removed.
//
// A participant whose removal means no price forms at all is the most extreme
// case of setting the price, so it is reported at the top of the scale rather
// than skipped for having no comparison.
func priceMoveBps(with, without money.Kobo, determined bool) int64 {
	if !determined {
		return 1 << 30
	}
	if with == 0 {
		return 0
	}
	d := with - without
	if d < 0 {
		d = -d
	}
	return int64(d) * 10_000 / int64(with)
}

// WashTrading detects activity that changes no beneficial ownership.
//
// Gross activity high, net position near flat, inside one related-party group.
// It catches the crude version, which is the version that actually happens on a
// venue this size.
func (s *Surveillance) WashTrading(ctx context.Context, tx pgx.Tx, instrumentID, sessionDate string) ([]Alert, error) {
	rows, err := tx.Query(ctx, `
		SELECT COALESCE(o.owner_key, 'account:' || a.owner_id::text) AS grp,
		       COALESCE(SUM(f.units) FILTER (WHERE f.side = 'buy'), 0)  AS bought,
		       COALESCE(SUM(f.units) FILTER (WHERE f.side = 'sell'), 0) AS sold
		  FROM fills f
		  JOIN orders o   ON o.id = f.order_id
		  JOIN accounts a ON a.id = o.account_id
		 WHERE f.instrument_id = $1 AND f.session_date = $2::date
		 GROUP BY 1`, instrumentID, sessionDate)
	if err != nil {
		return nil, fmt.Errorf("exchange: wash scan: %w", err)
	}
	defer rows.Close()

	var out []Alert
	for rows.Next() {
		var group string
		var bought, sold share.Units
		if err := rows.Scan(&group, &bought, &sold); err != nil {
			return nil, err
		}
		gross := bought + sold
		if gross == 0 {
			continue
		}
		net := bought - sold
		if net < 0 {
			net = -net
		}
		// Both sides present, and the net barely moved.
		if bought == 0 || sold == 0 {
			continue
		}
		if int64(net)*10_000/int64(gross) > s.WashNetBps {
			continue
		}
		out = append(out, Alert{
			InstrumentID: instrumentID, SessionDate: sessionDate,
			Detection: DetectWashTrading, Severity: SevBlock, SubjectGroup: group,
			Evidence: map[string]any{
				"bought_units": int64(bought),
				"sold_units":   int64(sold),
				"net_units":    int64(net),
				"net_bps":      int64(net) * 10_000 / int64(gross),
			},
		})
	}
	return out, rows.Err()
}

// Ramping detects a print pushed well above where the instrument has been
// trading, with the buying concentrated in one group.
//
// This is the attack the buyback's trailing-VWAP cap limits the damage from.
// The cap is a loss limiter; this is the detection that says it happened, so
// somebody can act on the cause rather than just absorbing it every session.
func (s *Surveillance) Ramping(ctx context.Context, tx pgx.Tx, instrumentID, sessionDate string) ([]Alert, error) {
	var price, trailing *int64
	err := tx.QueryRow(ctx, `
		SELECT (SELECT clearing_price_kobo FROM auctions
		         WHERE instrument_id = $1 AND session_date = $2::date AND state = 'published'),
		       (SELECT (SUM(adj_price_kobo::numeric * adj_volume_units::numeric)
		                / NULLIF(SUM(adj_volume_units::numeric), 0))::bigint
		          FROM price_observations_adjusted
		         WHERE instrument_id = $1 AND obs_date < $2::date AND volume_units > 0)`,
		instrumentID, sessionDate).Scan(&price, &trailing)
	if err != nil {
		return nil, fmt.Errorf("exchange: ramp scan: %w", err)
	}
	if price == nil || trailing == nil || *trailing <= 0 {
		return nil, nil
	}
	over := (*price - *trailing) * 10_000 / *trailing
	if over < s.RampBps {
		return nil, nil
	}

	// Who did the buying?
	var group *string
	var groupUnits, totalUnits *int64
	if err := tx.QueryRow(ctx, `
		WITH buys AS (
		  SELECT COALESCE(o.owner_key, 'account:' || a.owner_id::text) AS grp, SUM(f.units) AS u
		    FROM fills f JOIN orders o ON o.id = f.order_id JOIN accounts a ON a.id = o.account_id
		   WHERE f.instrument_id = $1 AND f.session_date = $2::date AND f.side = 'buy'
		   GROUP BY 1)
		SELECT grp, u, (SELECT SUM(u) FROM buys) FROM buys ORDER BY u DESC LIMIT 1`,
		instrumentID, sessionDate).Scan(&group, &groupUnits, &totalUnits); err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("exchange: ramp attribution: %w", err)
	}
	if group == nil || totalUnits == nil || *totalUnits == 0 {
		return nil, nil
	}

	return []Alert{{
		InstrumentID: instrumentID, SessionDate: sessionDate,
		Detection: DetectRamping, Severity: SevBlock, SubjectGroup: *group,
		Evidence: map[string]any{
			"clearing_price":  *price,
			"trailing_vwap":   *trailing,
			"over_bps":        over,
			"top_buyer_share": *groupUnits * 10_000 / *totalUnits,
		},
	}}, nil
}

func raise(ctx context.Context, tx pgx.Tx, a Alert) (int64, error) {
	ev, err := json.Marshal(a.Evidence)
	if err != nil {
		return 0, err
	}
	var id int64
	err = tx.QueryRow(ctx, `
		INSERT INTO surveillance_alerts
		  (instrument_id, session_date, detection, severity, subject_group, evidence)
		VALUES ($1,$2::date,$3,$4,$5,$6)
		RETURNING id`,
		a.InstrumentID, a.SessionDate, a.Detection, a.Severity, a.SubjectGroup, ev).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("exchange: raise alert: %w", err)
	}
	return id, nil
}

// Triage assigns an alert to someone. An alert nobody owns is not being worked.
func Triage(ctx context.Context, tx pgx.Tx, alertID int64, assignee string) error {
	ct, err := tx.Exec(ctx, `
		UPDATE surveillance_alerts SET state = 'triaged', assigned_to = $2, triaged_at = now()
		 WHERE id = $1 AND state = 'open'`, alertID, assignee)
	if err != nil {
		return fmt.Errorf("exchange: triage: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("exchange: alert %d is not open", alertID)
	}
	return nil
}

// Close records a decision and who made it.
//
// Closing an alert is what releases a `block` severity, and therefore what lets
// the buyback price the instrument again — so it carries a name.
func Close(ctx context.Context, tx pgx.Tx, alertID int64, outcome, by, notes string) error {
	state := "closed"
	switch outcome {
	case "false_positive":
		state = "false_positive"
	case "referred":
		state = "referred"
	case "escalated":
		state = "escalated"
	case "actioned", "closed":
	default:
		return fmt.Errorf("exchange: %q is not an alert outcome", outcome)
	}
	ct, err := tx.Exec(ctx, `
		UPDATE surveillance_alerts
		   SET state = $2, outcome = $3, closed_by = $4, notes = $5, reviewed_at = now()
		 WHERE id = $1 AND state <> 'closed'`, alertID, state, outcome, by, nullableString(notes))
	if err != nil {
		return fmt.Errorf("exchange: close alert: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("exchange: alert %d is already closed", alertID)
	}
	return nil
}

// settledSession is what the replay path needs to rebuild an auction.
type settledSession struct {
	ID     uuid.UUID
	Params Params
}

func loadSettledSession(ctx context.Context, tx pgx.Tx, instrumentID, sessionDate string) (
	*settledSession, []Order, map[int64]bookOrder, error) {

	var s settledSession
	var ref int64
	var tick, lot, bandBps int64
	err := tx.QueryRow(ctx, `
		SELECT a.id, a.prev_reference_kobo, i.tick_kobo, i.lot_units, i.static_band_bps
		  FROM auctions a JOIN instruments i ON i.id = a.instrument_id
		 WHERE a.instrument_id = $1 AND a.session_date = $2::date AND a.state = 'published'`,
		instrumentID, sessionDate).Scan(&s.ID, &ref, &tick, &lot, &bandBps)
	if err == pgx.ErrNoRows {
		return nil, nil, nil, nil
	}
	if err != nil {
		return nil, nil, nil, fmt.Errorf("exchange: load settled session: %w", err)
	}
	s.Params = Params{
		PrevRef: money.Kobo(ref), Tick: money.Kobo(tick), Lot: share.Units(lot),
		BandLo: money.Kobo(ref - ref*bandBps/10_000), BandHi: money.Kobo(ref + ref*bandBps/10_000),
	}
	if s.Params.BandLo < 1 {
		s.Params.BandLo = 1
	}

	book, index, err := loadSessionBook(ctx, tx, s.ID)
	if err != nil {
		return nil, nil, nil, err
	}
	return &s, book, index, nil
}

// loadSessionBook rebuilds the crossed book from the orders of a settled
// session, including the ones that filled — loadBook's `state = 'open'` filter
// is right during a session and wrong afterwards.
func loadSessionBook(ctx context.Context, tx pgx.Tx, auctionID uuid.UUID) ([]Order, map[int64]bookOrder, error) {
	rows, err := tx.Query(ctx, `
		SELECT o.id, o.auction_seq, o.side, o.type, o.limit_kobo, o.qty_units,
		       o.notional_kobo, o.reserved_kobo, o.owner_key, a.owner_id
		  FROM orders o JOIN accounts a ON a.id = o.account_id
		 WHERE o.auction_id = $1 AND o.state <> 'rejected'
		 ORDER BY o.auction_seq`, auctionID)
	if err != nil {
		return nil, nil, fmt.Errorf("exchange: rebuild book: %w", err)
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
		bo.ID, bo.Side = bo.OrderID, Side(side)
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
