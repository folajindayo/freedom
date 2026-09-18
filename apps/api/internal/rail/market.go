package rail

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"freedom/api/internal/exchange"
	"freedom/api/internal/money"
	"freedom/api/internal/scheme"
	"freedom/api/internal/share"
)

// The market close, as one routine: for every listed instrument, open the
// session if nobody has, run it to settlement, run surveillance over the
// result, and let the buyback take the price. Each instrument closes in its
// own transaction so a halted symbol does not hold every other symbol's
// buyback hostage.

// CloseRow is one instrument's close.
type CloseRow struct {
	Symbol       string       `json:"symbol"`
	InstrumentID string       `json:"instrument_id"`
	State        string       `json:"state"`
	PriceKobo    money.Kobo   `json:"price_kobo"`
	MatchedUnits share.Units  `json:"matched_units"`
	Buyback      CloseBuyback `json:"buyback"`
	Error        string       `json:"error,omitempty"`
}

// CloseBuyback is what the buyback did after the session.
type CloseBuyback struct {
	Intents int         `json:"intents"`
	Units   share.Units `json:"units"`
	// Retried is how many earlier-escrowed intents this close put back in
	// front of the engine; they end up in Intents or Escrowed.
	Retried  int    `json:"retried"`
	Escrowed int    `json:"escrowed"`
	Refusal  string `json:"refusal,omitempty"`
}

// CloseMarket runs the close for a session date (today when empty).
//
// Idempotent: a session already published is not re-run, and the buyback
// re-runs harmlessly because every allocation is keyed by intent.
func (s *Service) CloseMarket(ctx context.Context, sessionDate string) ([]CloseRow, error) {
	if sessionDate == "" {
		sessionDate = scheme.BusinessDate(s.Now())
	}
	if _, err := scheme.ParseBusinessDate(sessionDate); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := s.inTx(ctx, func(tx pgx.Tx) error {
		return ensureCalendarDay(ctx, tx, sessionDate)
	}); err != nil {
		return nil, err
	}

	type listed struct{ id, symbol string }
	var instruments []listed
	rows, err := s.Pool.Query(ctx, `SELECT id, symbol FROM instruments WHERE status = 'listed' ORDER BY symbol`)
	if err != nil {
		return nil, fmt.Errorf("rail: listed instruments: %w", err)
	}
	for rows.Next() {
		var l listed
		if err := rows.Scan(&l.id, &l.symbol); err != nil {
			rows.Close()
			return nil, err
		}
		instruments = append(instruments, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := []CloseRow{}
	for _, in := range instruments {
		row := CloseRow{Symbol: in.symbol, InstrumentID: in.id}
		err := s.inTx(ctx, func(tx pgx.Tx) error {
			return s.closeOne(ctx, tx, &row, sessionDate)
		})
		if err != nil {
			// One symbol's failure is recorded on its row, not thrown at the
			// caller: the other symbols still closed.
			row.Error = err.Error()
			slog.Warn("market close failed for instrument", "symbol", in.symbol, "date", sessionDate, "err", err)
		}
		out = append(out, row)
	}
	return out, nil
}

func (s *Service) closeOne(ctx context.Context, tx pgx.Tx, row *CloseRow, sessionDate string) error {
	var state string
	err := tx.QueryRow(ctx, `
		SELECT state FROM auctions WHERE instrument_id = $1 AND session_date = $2::date`,
		row.InstrumentID, sessionDate).Scan(&state)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("rail: session state: %w", err)
	}

	switch state {
	case "published":
		// Already closed; only the buyback re-runs.
	case "halted", "cancelled":
		// A session that ended without a price. Nothing to settle; the
		// buyback will refuse and escrow, which is the recorded outcome.
	default:
		sess, err := s.Engine.Open(ctx, tx, row.InstrumentID, sessionDate)
		if err != nil {
			return err
		}
		if _, err := s.Engine.RunToSettlement(ctx, tx, sess); err != nil {
			return err
		}
		if _, err := s.Surveillance.RunSession(ctx, tx, row.InstrumentID, sessionDate); err != nil {
			return err
		}
	}

	var price, matched *int64
	if err := tx.QueryRow(ctx, `
		SELECT state, clearing_price_kobo, matched_units FROM auctions
		 WHERE instrument_id = $1 AND session_date = $2::date`,
		row.InstrumentID, sessionDate).Scan(&row.State, &price, &matched); err != nil {
		return fmt.Errorf("rail: session result: %w", err)
	}
	if price != nil {
		row.PriceKobo = money.Kobo(*price)
	}
	if matched != nil {
		row.MatchedUnits = share.Units(*matched)
	}

	// Escrowed intents are funding already held for a price we refused to
	// stand behind (halt, thin VWAP, band). The engine escrows but nothing in
	// it retries, so the close is the retry: put them back to pending and let
	// RunSession price them or escrow them again against today's reason.
	// Only the state flips; the intents table has no reason column, and the
	// refusal is reported on this row instead.
	ct, err := tx.Exec(ctx, `
		UPDATE buyback_intents SET state = 'pending'
		 WHERE instrument_id = $1 AND state = 'escrowed'`, row.InstrumentID)
	if err != nil {
		return fmt.Errorf("rail: retry escrowed intents: %w", err)
	}
	res, err := s.Buyback.RunSession(ctx, tx, row.InstrumentID, sessionDate)
	if err != nil {
		return err
	}
	row.Buyback = CloseBuyback{Intents: res.Intents, Units: res.Units, Retried: int(ct.RowsAffected()),
		Escrowed: res.Escrowed, Refusal: string(res.Refusal)}
	if res.Price > 0 && row.PriceKobo == 0 {
		// A zero-volume session: the buyback paid the carried reference.
		row.PriceKobo = res.Price
	}
	return nil
}

// ensureCalendarDay makes a session date openable when nobody published a
// calendar for it.
//
// The exchange fails closed on an unpublished date, and that is right for a
// venue whose members are told when to show up. The rail's close is a
// scheduled job with no members to notify, so a weekday nobody entered is
// treated as a trading day and said so in the log — a moveable holiday that
// was never published will be closed on, and the log line is how that gets
// noticed.
func ensureCalendarDay(ctx context.Context, tx pgx.Tx, date string) error {
	_, err := exchange.Lookup(ctx, tx, date)
	if err == nil || !errors.Is(err, exchange.ErrMarketClosed) {
		return err
	}
	if !strings.Contains(err.Error(), "not on the published calendar") {
		return nil // a published holiday; Open will refuse with the reason
	}
	day, err := scheme.ParseBusinessDate(date)
	if err != nil {
		return err
	}
	if wd := day.Weekday(); wd == time.Saturday || wd == time.Sunday {
		return nil
	}
	opens := time.Date(day.Year(), day.Month(), day.Day(), exchange.OpenHour, 0, 0, 0, scheme.Lagos)
	freezes := time.Date(day.Year(), day.Month(), day.Day(), exchange.FreezeHour, 0, 0, 0, scheme.Lagos)
	if _, err := tx.Exec(ctx, `
		INSERT INTO trading_calendar (session_date, is_trading, opens_at, freezes_at, note)
		VALUES ($1::date, true, $2, $3, 'assumed trading: no calendar published for this date')
		ON CONFLICT (session_date) DO NOTHING`, date, opens, freezes); err != nil {
		return fmt.Errorf("rail: assume trading day: %w", err)
	}
	slog.Warn("no trading calendar published for date; treating weekday as a trading day", "date", date)
	return nil
}

// TodayView is the market as it stands now.
type TodayView struct {
	BusinessDate string            `json:"business_date"`
	IsTrading    bool              `json:"is_trading"`
	Phase        string            `json:"phase"`
	Instruments  []TodayInstrument `json:"instruments"`
}

// TodayInstrument is one symbol's standing today.
type TodayInstrument struct {
	Symbol             string     `json:"symbol"`
	SessionState       string     `json:"session_state"`
	ReferencePriceKobo money.Kobo `json:"reference_price_kobo"`
	Halted             bool       `json:"halted"`
}

// Today reports the business date, whether it trades, the session phase and
// each listed instrument's state.
func (s *Service) Today(ctx context.Context) (TodayView, error) {
	now := s.Now().In(scheme.Lagos)
	v := TodayView{BusinessDate: scheme.BusinessDate(now), Instruments: []TodayInstrument{}}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		_, err := exchange.Lookup(ctx, tx, v.BusinessDate)
		switch {
		case err == nil:
			v.IsTrading = true
		case errors.Is(err, exchange.ErrMarketClosed):
			if strings.Contains(err.Error(), "not on the published calendar") {
				wd := now.Weekday()
				v.IsTrading = wd != time.Saturday && wd != time.Sunday
			}
		default:
			return err
		}
		v.Phase = phase(now, v.IsTrading)

		rows, err := tx.Query(ctx, `
			SELECT i.symbol, COALESCE(a.state, 'scheduled'), i.reference_price_kobo,
			       EXISTS (SELECT 1 FROM trading_halts h WHERE h.instrument_id = i.id AND h.released_at IS NULL)
			  FROM instruments i
			  LEFT JOIN auctions a ON a.instrument_id = i.id AND a.session_date = $1::date
			 WHERE i.status = 'listed' ORDER BY i.symbol`, v.BusinessDate)
		if err != nil {
			return fmt.Errorf("rail: instruments today: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var t TodayInstrument
			if err := rows.Scan(&t.Symbol, &t.SessionState, &t.ReferencePriceKobo, &t.Halted); err != nil {
				return err
			}
			v.Instruments = append(v.Instruments, t)
		}
		return rows.Err()
	})
	return v, err
}

// phase names where the day is against the session hours.
func phase(now time.Time, trading bool) string {
	if !trading {
		return "closed"
	}
	switch h := now.Hour(); {
	case h < exchange.OpenHour:
		return "pre_open"
	case h < exchange.FreezeHour:
		return "accepting"
	default:
		return "closed"
	}
}

// Scheduler runs CloseMarket once a day at a Lagos wall-clock time, on
// trading days. It is the same routine the HTTP endpoint runs, so a missed
// run is recovered by calling the endpoint.
//
// at is "HH:MM"; "off" disables it. Blocks until ctx is done.
func (s *Service) Scheduler(ctx context.Context, at string) error {
	if at == "off" {
		slog.Info("market close scheduler disabled")
		<-ctx.Done()
		return nil
	}
	target, err := time.Parse("15:04", at)
	if err != nil {
		return fmt.Errorf("rail: MARKET_CLOSE_AT %q is not HH:MM", at)
	}
	slog.Info("market close scheduled", "at", at, "zone", "Africa/Lagos")

	for {
		now := s.Now().In(scheme.Lagos)
		next := time.Date(now.Year(), now.Month(), now.Day(), target.Hour(), target.Minute(), 0, 0, scheme.Lagos)
		if !next.After(now) {
			next = next.AddDate(0, 0, 1)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(next.Sub(now)):
		}

		date := next.Format("2006-01-02")
		if !s.tradingDay(ctx, date) {
			slog.Info("market close skipped: not a trading day", "date", date)
			continue
		}
		rows, err := s.CloseMarket(ctx, date)
		if err != nil {
			slog.Error("market close failed", "date", date, "err", err)
			continue
		}
		for _, r := range rows {
			slog.Info("market closed", "symbol", r.Symbol, "state", r.State, "price", r.PriceKobo,
				"intents", r.Buyback.Intents, "units", r.Buyback.Units, "refusal", r.Buyback.Refusal, "err", r.Error)
		}
	}
}

// tradingDay is the calendar's answer, falling back to weekdays when the date
// was never published.
func (s *Service) tradingDay(ctx context.Context, date string) bool {
	trading := false
	_ = s.inTx(ctx, func(tx pgx.Tx) error {
		_, err := exchange.Lookup(ctx, tx, date)
		if err == nil {
			trading = true
			return nil
		}
		if strings.Contains(err.Error(), "not on the published calendar") {
			d, _ := scheme.ParseBusinessDate(date)
			wd := d.Weekday()
			trading = wd != time.Saturday && wd != time.Sunday
		}
		return nil
	})
	return trading
}
