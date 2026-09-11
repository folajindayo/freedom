package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"freedom/api/internal/exchange"
	"freedom/api/internal/money"
	"freedom/api/internal/share"
)

// The market opens on days somebody published, and not on others.
func TestCalendarGovernsSessions(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)

	for _, tc := range []struct {
		name, date, want string
	}{
		{"a weekday is open", "2027-06-01", ""},
		{"Saturday is closed", "2027-06-05", "weekend"},
		{"Sunday is closed", "2027-06-06", "weekend"},
		{"Independence Day is closed", "2027-10-01", "Independence Day"},
		{"Christmas is closed", "2027-12-25", "Christmas"},
		{"an unpublished year fails closed", "2031-03-04", "not on the published calendar"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := inTx(p, func(tx pgx.Tx) error {
				_, err := exchange.NewEngine().Open(ctx, tx, n.instrumentID, tc.date)
				return err
			})
			if tc.want == "" {
				if err != nil {
					t.Fatalf("a trading day was refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("a session opened on %s", tc.date)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error mentioning %q, got: %v", tc.want, err)
			}
		})
	}
}

// The moveable feasts are announced, not computed. A calendar published without
// them is wrong on those days, and the only fix is data.
func TestMoveableHolidaysMustBeSupplied(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)

	eid := "2027-03-09" // a Tuesday; the real date is declared each year
	if err := inTx(p, func(tx pgx.Tx) error {
		_, err := exchange.NewEngine().Open(ctx, tx, n.instrumentID, eid)
		return err
	}); err != nil {
		t.Fatalf("before the holiday is published the market is open: %v", err)
	}

	mustTx(t, p, func(tx pgx.Tx) error {
		_, err := exchange.Publish(ctx, tx, 2027, []exchange.Holiday{{Date: eid, Name: "Eid al-Fitr"}})
		return err
	})

	err := inTx(p, func(tx pgx.Tx) error {
		_, err := exchange.NewEngine().Open(ctx, tx, n.instrumentID, eid)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "Eid al-Fitr") {
		t.Fatalf("want the market closed for Eid, got: %v", err)
	}
}

func TestNextTradingDaySkipsClosures(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	setup(t, p)

	for _, tc := range []struct{ from, want string }{
		{"2027-06-05", "2027-06-07"}, // Saturday → Monday
		{"2027-10-01", "2027-10-04"}, // Independence Day (Friday) → Monday
		{"2027-06-01", "2027-06-01"}, // already a trading day
	} {
		var got string
		mustTx(t, p, func(tx pgx.Tx) error {
			var err error
			got, err = exchange.NextTradingDay(ctx, tx, tc.from)
			return err
		})
		if got != tc.want {
			t.Errorf("next trading day from %s = %s, want %s", tc.from, got, tc.want)
		}
	}
}

// A halt means the market does not currently know the price, which is a
// legitimate thing to say and far better than printing a number nobody believes.
func TestHaltStopsTradingAndBuyback(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)

	mustTx(t, p, func(tx pgx.Tx) error {
		_, err := exchange.Halt(ctx, tx, n.instrumentID, exchange.HaltNewsPending,
			"listings.officer", map[string]any{"awaiting": "FY results"})
		return err
	})

	if err := inTx(p, func(tx pgx.Tx) error {
		_, err := exchange.NewEngine().Open(ctx, tx, n.instrumentID, tradeDate)
		return err
	}); err == nil {
		t.Fatal("a session opened on a halted instrument")
	}

	tap(t, p, n)
	if b := runBuyback(t, p, n, 0); b.Refusal != "halted" {
		t.Fatalf("buyback refusal = %q, want halted", b.Refusal)
	}

	// Only one halt may be open at a time — two would mean two people each
	// believing they could lift it.
	if err := inTx(p, func(tx pgx.Tx) error {
		_, err := exchange.Halt(ctx, tx, n.instrumentID, exchange.HaltRegulatory, "someone", nil)
		return err
	}); err == nil {
		t.Error("a second concurrent halt was accepted")
	}

	mustTx(t, p, func(tx pgx.Tx) error {
		return exchange.Release(ctx, tx, n.instrumentID, "listings.officer")
	})
	if err := inTx(p, func(tx pgx.Tx) error {
		_, err := exchange.NewEngine().Open(ctx, tx, n.instrumentID, tradeDate)
		return err
	}); err != nil {
		t.Fatalf("the instrument did not resume after release: %v", err)
	}

	if err := inTx(p, func(tx pgx.Tx) error {
		return exchange.Release(ctx, tx, n.instrumentID, "listings.officer")
	}); err == nil {
		t.Error("releasing an instrument that is not halted must be an error")
	}
}

func TestHaltRejectsUnknownReason(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)
	if err := inTx(p, func(tx pgx.Tx) error {
		_, err := exchange.Halt(ctx, tx, n.instrumentID, "felt_wrong", "someone", nil)
		return err
	}); err == nil {
		t.Fatal("an unrecognised halt reason must be rejected")
	}
}

// A violent move trips the breaker: the instrument halts and the session is
// abandoned rather than published. The alternative is that tomorrow's buyback
// pays a price the market itself could not stand behind.
func TestCircuitBreakerHaltsOnViolentMove(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)

	seller := n.cardholderID
	giveShares(t, p, n, seller, share.Whole(20), money.Naira(30))
	buyer := newCardholder(t, p, "Zainab Yusuf")
	fund(t, p, buyer, money.Naira(50_000))
	member, sa, ba := membership(t, p, n, seller, buyer)

	// Widen the band so the order passes entry, but leave the breaker at its
	// default 30% — this is the in-band-but-implausible case the breaker exists
	// for, distinct from the band rejection at entry.
	mustExec(t, p, `UPDATE instruments SET static_band_bps = 6000 WHERE id = $1`, n.instrumentID)

	var result exchange.Result
	mustTx(t, p, func(tx pgx.Tx) error {
		e := exchange.NewEngine()
		s, err := e.Open(ctx, tx, n.instrumentID, tradeDate)
		if err != nil {
			return err
		}
		// Reference is ₦40; this clears around ₦60, a 50% move.
		if _, err := e.Place(ctx, tx, s, order(member, sa, seller, exchange.Sell, money.Naira(60), share.Whole(5))); err != nil {
			return err
		}
		if _, err := e.Place(ctx, tx, s, order(member, ba, buyer, exchange.Buy, money.Naira(60), share.Whole(5))); err != nil {
			return err
		}
		result, err = e.RunToSettlement(ctx, tx, s)
		return err
	})
	if !result.Abandoned {
		t.Fatal("a 50% move was published")
	}
	if result.Reason != "circuit_breaker" {
		t.Fatalf("reason = %q, want circuit_breaker", result.Reason)
	}

	// The instrument is now halted, and the reference is untouched — the
	// session never published, so nothing downstream learned the bad price.
	var halted bool
	if err := p.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM trading_halts
		                WHERE instrument_id = $1 AND released_at IS NULL)`,
		n.instrumentID).Scan(&halted); err != nil {
		t.Fatal(err)
	}
	if !halted {
		t.Error("the breaker tripped but left no halt")
	}
	if _, ref := instrumentState(t, p, n); ref != money.Naira(40) {
		t.Errorf("reference moved to %s despite the session being abandoned", ref)
	}
}

// A price outside the band is never clamped into it. Clamping publishes a price
// nobody bid.
func TestBandBreachIsRefusedNotClamped(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)

	params := exchange.Params{
		PrevRef: money.Naira(40), Tick: 1, Lot: 1,
		BandLo: money.Naira(32), BandHi: money.Naira(48),
	}
	var breached bool
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		breached, err = exchange.CheckBand(ctx, tx, n.instrumentID, params, money.Naira(55))
		return err
	})
	if !breached {
		t.Fatal("an out-of-band price was allowed to publish")
	}

	var kind string
	if err := p.QueryRow(ctx, `
		SELECT kind FROM data_quality_incidents WHERE instrument_id = $1 ORDER BY id DESC LIMIT 1`,
		n.instrumentID).Scan(&kind); err != nil {
		t.Fatal(err)
	}
	if kind != "band_breach" {
		t.Errorf("incident kind = %s, want band_breach", kind)
	}
}
