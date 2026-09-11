// Package scheme holds the network's own rules: what time it is for clearing
// purposes, and how a transaction is identified across participants.
package scheme

import (
	"fmt"
	"time"
)

// Lagos is the scheme's clock. Nigeria does not observe daylight saving, which
// removes the single richest source of clearing-date bugs — but timestamps are
// still stored as timestamptz, because the absence of DST is a fact about
// Nigeria today rather than a property of the data model.
var Lagos = time.FixedZone("WAT", 1*60*60)

// CutoverHour is the local hour at which the business date rolls. A tap at
// 20:01 belongs to the next business date and clears in the next batch.
const CutoverHour = 20

// BusinessDate returns the scheme business date for an instant.
//
// This is the authoritative definition, and it is called exactly once per
// transaction — at the switch, on the way in — and then stamped on the record.
// Nothing downstream re-derives it: a batch replayed next week must land on the
// date it originally cleared, and a value that is recomputed is a value that
// can change.
func BusinessDate(t time.Time) string {
	local := t.In(Lagos)
	if local.Hour() >= CutoverHour {
		local = local.AddDate(0, 0, 1)
	}
	return local.Format("2006-01-02")
}

// Today is the current business date.
func Today() string { return BusinessDate(time.Now()) }

// ParseBusinessDate validates a business date string.
func ParseBusinessDate(s string) (time.Time, error) {
	d, err := time.ParseInLocation("2006-01-02", s, Lagos)
	if err != nil {
		return time.Time{}, fmt.Errorf("scheme: %q is not a business date: %w", s, err)
	}
	return d, nil
}

// ChargebackWindow is how long a cardholder has to dispute a transaction. Equity
// allocated by a buyback is locked until this window closes on the transaction
// that funded it — otherwise a fraudster taps, receives shares, sells them, and
// charges the tap back, and the network has bought shares for a thief with its
// own money.
const ChargebackWindow = 120 * 24 * time.Hour

// LockedUntil returns the date on which equity funded by a transaction on the
// given business date becomes transferable.
func LockedUntil(businessDate string) (string, error) {
	d, err := ParseBusinessDate(businessDate)
	if err != nil {
		return "", err
	}
	return d.Add(ChargebackWindow).Format("2006-01-02"), nil
}
