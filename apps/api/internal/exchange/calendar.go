package exchange

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"freedom/api/internal/scheme"
)

// The trading calendar.
//
// Nigerian public holidays are announced, not computable. Eid al-Fitr and Eid
// al-Kabir move with the lunar calendar and their dates are declared by the
// government, sometimes days ahead; Democracy Day moved from May to June by
// proclamation in 2019. An algorithm that derives them closes the market on the
// wrong day, which is worse than not having one, so the calendar is data that
// somebody publishes ahead of time.
//
// Fixed-date holidays can be generated. The moveable ones must be entered.

// ErrMarketClosed is returned when a session is opened on a non-trading day.
var ErrMarketClosed = errors.New("exchange: the market is closed")

// SessionDay describes one date on the calendar.
type SessionDay struct {
	Date      string
	IsTrading bool
	OpensAt   time.Time
	FreezesAt time.Time
	Note      string
}

// Session hours, local to Lagos. The auction collects orders through the
// morning and freezes at midday, leaving the afternoon for settlement,
// surveillance and the buyback run — all of which must finish before the
// scheme's 20:00 business-date cutover.
const (
	OpenHour   = 10
	FreezeHour = 12
)

// Lookup returns the calendar entry for a date, or ErrMarketClosed.
func Lookup(ctx context.Context, tx pgx.Tx, date string) (SessionDay, error) {
	var d SessionDay
	var opens, freezes *time.Time
	var note *string
	err := tx.QueryRow(ctx, `
		SELECT session_date::text, is_trading, opens_at, freezes_at, note
		  FROM trading_calendar WHERE session_date = $1::date`, date).
		Scan(&d.Date, &d.IsTrading, &opens, &freezes, &note)
	if errors.Is(err, pgx.ErrNoRows) {
		// An unpublished date is not a trading day. Failing closed is right
		// here: a market that opens on a date nobody scheduled is a market
		// whose participants are not there.
		return SessionDay{Date: date}, fmt.Errorf("%w: %s is not on the published calendar", ErrMarketClosed, date)
	}
	if err != nil {
		return d, fmt.Errorf("exchange: calendar lookup: %w", err)
	}
	if !d.IsTrading {
		reason := "a scheduled holiday"
		if note != nil {
			reason = *note
		}
		return d, fmt.Errorf("%w on %s: %s", ErrMarketClosed, date, reason)
	}
	if opens != nil {
		d.OpensAt = *opens
	}
	if freezes != nil {
		d.FreezesAt = *freezes
	}
	if note != nil {
		d.Note = *note
	}
	return d, nil
}

// Holiday is a non-trading day to be published.
type Holiday struct {
	Date string
	Name string
}

// FixedHolidays returns the Nigerian public holidays whose dates are the same
// every year. The moveable feasts are deliberately absent — see Publish.
func FixedHolidays(year int) []Holiday {
	y := fmt.Sprint(year)
	return []Holiday{
		{y + "-01-01", "New Year's Day"},
		{y + "-05-01", "Workers' Day"},
		{y + "-06-12", "Democracy Day"},
		{y + "-10-01", "Independence Day"},
		{y + "-12-25", "Christmas Day"},
		{y + "-12-26", "Boxing Day"},
	}
}

// Publish writes a year of calendar entries.
//
// Weekends and the fixed holidays are closed automatically. The moveable
// holidays — the two Eids, Good Friday and Easter Monday — must be passed in,
// because they are announced rather than derived. Passing none is legal and
// produces a calendar that is wrong on those days, which is why Publish reports
// how many moveable dates it was given.
func Publish(ctx context.Context, tx pgx.Tx, year int, moveable []Holiday) (trading int, err error) {
	closed := map[string]string{}
	for _, h := range append(FixedHolidays(year), moveable...) {
		closed[h.Date] = h.Name
	}

	start := time.Date(year, 1, 1, 0, 0, 0, 0, scheme.Lagos)
	for d := start; d.Year() == year; d = d.AddDate(0, 0, 1) {
		date := d.Format("2006-01-02")
		note, isHoliday := closed[date]
		weekend := d.Weekday() == time.Saturday || d.Weekday() == time.Sunday
		isTrading := !weekend && !isHoliday
		if weekend && !isHoliday {
			note = "weekend"
		}

		var opens, freezes *time.Time
		if isTrading {
			o := time.Date(year, d.Month(), d.Day(), OpenHour, 0, 0, 0, scheme.Lagos)
			f := time.Date(year, d.Month(), d.Day(), FreezeHour, 0, 0, 0, scheme.Lagos)
			opens, freezes = &o, &f
			trading++
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO trading_calendar (session_date, is_trading, opens_at, freezes_at, note)
			VALUES ($1::date,$2,$3,$4,$5)
			ON CONFLICT (session_date) DO UPDATE
			  SET is_trading = EXCLUDED.is_trading, opens_at = EXCLUDED.opens_at,
			      freezes_at = EXCLUDED.freezes_at, note = EXCLUDED.note`,
			date, isTrading, opens, freezes, nullableString(note)); err != nil {
			return 0, fmt.Errorf("exchange: publish calendar: %w", err)
		}
	}
	return trading, nil
}

// NextTradingDay returns the first trading date on or after a given date.
func NextTradingDay(ctx context.Context, tx pgx.Tx, from string) (string, error) {
	var d string
	err := tx.QueryRow(ctx, `
		SELECT session_date::text FROM trading_calendar
		 WHERE session_date >= $1::date AND is_trading
		 ORDER BY session_date LIMIT 1`, from).Scan(&d)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: no trading day published on or after %s", ErrMarketClosed, from)
	}
	return d, err
}
