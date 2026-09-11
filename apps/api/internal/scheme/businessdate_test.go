package scheme

import (
	"testing"
	"time"
)

func TestBusinessDateCutover(t *testing.T) {
	at := func(s string) time.Time {
		ts, err := time.ParseInLocation("2006-01-02 15:04", s, Lagos)
		if err != nil {
			t.Fatal(err)
		}
		return ts
	}
	for _, tc := range []struct {
		when string
		want string
	}{
		{"2026-09-11 00:01", "2026-09-11"},
		{"2026-09-11 19:59", "2026-09-11"},
		{"2026-09-11 20:00", "2026-09-12"}, // the cutover instant itself rolls
		{"2026-09-11 23:59", "2026-09-12"},
		{"2026-12-31 21:00", "2027-01-01"}, // and across a year boundary
	} {
		if got := BusinessDate(at(tc.when)); got != tc.want {
			t.Errorf("BusinessDate(%s) = %s, want %s", tc.when, got, tc.want)
		}
	}
}

// A tap is stamped with Lagos's business date regardless of where the server
// runs. A UTC host must not shift the whole network's clearing day.
func TestBusinessDateIsZoneIndependent(t *testing.T) {
	instant := time.Date(2026, 9, 11, 19, 30, 0, 0, Lagos)
	want := BusinessDate(instant)
	for _, loc := range []*time.Location{time.UTC, time.FixedZone("PST", -8*3600), time.FixedZone("JST", 9*3600)} {
		if got := BusinessDate(instant.In(loc)); got != want {
			t.Errorf("the same instant read in %v gave %s, want %s", loc, got, want)
		}
	}
}

func TestLockedUntilSpansTheChargebackWindow(t *testing.T) {
	got, err := LockedUntil("2026-09-11")
	if err != nil {
		t.Fatal(err)
	}
	if got != "2027-01-09" {
		t.Errorf("equity funded on 2026-09-11 unlocks %s, want 2027-01-09 (120 days)", got)
	}
	if _, err := LockedUntil("not-a-date"); err == nil {
		t.Error("a malformed business date must be rejected")
	}
}
