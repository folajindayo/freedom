package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"freedom/api/internal/exchange"
	"freedom/api/internal/money"
	"freedom/api/internal/share"
)

// An obligation to quote, held by somebody who benefits from the price, is the
// manipulation vector wearing a badge.
func TestMarketMakerMustBeIndependentOfTheIssuer(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)

	insider := newCardholder(t, p, "Bayo Ilori")
	member, _, _ := membership(t, p, n, n.cardholderID, insider)
	ia := clientFor(t, p, n, member, insider)
	mustExec(t, p, `UPDATE members SET roles = ARRAY['broker','market_maker'] WHERE id = $1`, member)

	// Tie one of the member's clients to the issuer.
	mustExec(t, p, `
		INSERT INTO related_parties (instrument_id, account_id, group_key, relation, effective)
		VALUES ($1,$2,'issuer-group','affiliate',daterange('2020-01-01','2099-01-01'))`,
		n.instrumentID, ia.ledger)

	err := inTx(p, func(tx pgx.Tx) error {
		_, err := exchange.Appoint(ctx, tx,
			exchange.StandardObligation(n.instrumentID, member, "2027-01-01", "2028-01-01"))
		return err
	})
	if err == nil {
		t.Fatal("an issuer affiliate was appointed as market maker")
	}
	if !strings.Contains(err.Error(), "independent") {
		t.Fatalf("want an independence refusal, got: %v", err)
	}
}

func TestOnlyRegisteredMarketMakersAreAppointed(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)
	member, _, _ := membership(t, p, n, n.cardholderID, newCardholder(t, p, "Tola Ade"))

	// A plain broker, not registered for the role.
	if err := inTx(p, func(tx pgx.Tx) error {
		_, err := exchange.Appoint(ctx, tx,
			exchange.StandardObligation(n.instrumentID, member, "2027-01-01", "2028-01-01"))
		return err
	}); err == nil {
		t.Fatal("a member without the market_maker role was appointed")
	}
}

// A market maker who quoted on the days it suited them and stood aside when the
// price was moving has met no obligation at all. Only a per-session record
// shows that.
func TestObligationIsMeasuredEverySession(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)

	maker := newCardholder(t, p, "Liquidity Partners")
	fund(t, p, maker, money.Naira(5_000_000))
	giveShares(t, p, n, maker, share.Whole(5_000), money.Naira(30))
	member, _, ma := membership(t, p, n, n.cardholderID, maker)
	mustExec(t, p, `UPDATE members SET roles = ARRAY['broker','market_maker'] WHERE id = $1`, member)

	var provider uuid.UUID
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		provider, err = exchange.Appoint(ctx, tx,
			exchange.StandardObligation(n.instrumentID, member, "2027-01-01", "2028-01-01"))
		return err
	})

	// Each sub-test uses its own session date rather than tearing the previous
	// one down: five consecutive trading days, which is also what makes the
	// uptime figure at the end mean something.
	quote := func(date string, bid, ask money.Kobo, units share.Units) []exchange.SessionPerformance {
		var perf []exchange.SessionPerformance
		mustTx(t, p, func(tx pgx.Tx) error {
			e := exchange.NewEngine()
			s, err := e.Open(ctx, tx, n.instrumentID, date)
			if err != nil {
				return err
			}
			if bid > 0 {
				if _, err := e.Place(ctx, tx, s, order(member, ma, maker, exchange.Buy, bid, units)); err != nil {
					return err
				}
			}
			if ask > 0 {
				if _, err := e.Place(ctx, tx, s, order(member, ma, maker, exchange.Sell, ask, units)); err != nil {
					return err
				}
			}
			perf, err = exchange.MeasureSession(ctx, tx, n.instrumentID, date)
			return err
		})
		return perf
	}

	t.Run("a compliant two-sided quote meets the obligation", func(t *testing.T) {
		// ₦39 / ₦41 around a ₦40 reference is a 500bp spread, at the limit, on
		// 2,000 shares — comfortably above the ₦50,000 minimum.
		perf := quote("2027-06-01", money.Naira(39), money.Naira(41), share.Whole(2_000))
		if len(perf) != 1 {
			t.Fatalf("measured %d providers, want 1", len(perf))
		}
		if !perf[0].Met {
			t.Fatalf("a compliant quote was marked short: %s (spread %d bps, size %s)",
				perf[0].Shortfall, perf[0].SpreadBps, perf[0].Size)
		}
		t.Logf("met: %s / %s, spread %d bps, size %s",
			perf[0].Bid, perf[0].Ask, perf[0].SpreadBps, perf[0].Size)
	})

	t.Run("a one-sided quote does not", func(t *testing.T) {
		perf := quote("2027-06-02", money.Naira(39), 0, share.Whole(2_000))
		if perf[0].Met || perf[0].TwoSided {
			t.Fatal("a one-sided quote met a two-sided obligation")
		}
		if !strings.Contains(perf[0].Shortfall, "one side") {
			t.Errorf("shortfall = %q", perf[0].Shortfall)
		}
	})

	t.Run("a quote too wide does not", func(t *testing.T) {
		perf := quote("2027-06-03", money.Naira(34), money.Naira(46), share.Whole(2_000))
		if perf[0].Met {
			t.Fatal("a 30% spread met a 5% obligation")
		}
		if !strings.Contains(perf[0].Shortfall, "spread") {
			t.Errorf("shortfall = %q", perf[0].Shortfall)
		}
	})

	t.Run("a quote too small does not", func(t *testing.T) {
		perf := quote("2027-06-04", money.Naira(39), money.Naira(41), share.Whole(10))
		if perf[0].Met {
			t.Fatal("a ₦400 quote met a ₦50,000 obligation")
		}
		if !strings.Contains(perf[0].Shortfall, "size") {
			t.Errorf("shortfall = %q", perf[0].Shortfall)
		}
	})

	t.Run("absence is the clearest breach of all", func(t *testing.T) {
		perf := quote("2027-06-07", 0, 0, 0)
		if perf[0].Quoted || perf[0].Met {
			t.Fatal("a provider who did not quote was marked present")
		}
		if perf[0].Shortfall != "did not quote" {
			t.Errorf("shortfall = %q", perf[0].Shortfall)
		}
	})

	// One met out of five: well under the 80% obligation.
	var uptime int64
	var sessions int
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		uptime, sessions, err = exchange.Uptime(ctx, tx, provider, 20)
		return err
	})
	if sessions != 5 {
		t.Fatalf("%d sessions recorded, want 5", sessions)
	}
	if uptime != 2000 {
		t.Fatalf("uptime %d bps over %d sessions, want 2000", uptime, sessions)
	}
	t.Logf("uptime %d bps over %d sessions — well short of the 8000 bps obligation", uptime, sessions)
}

// A market maker having a bad month is far more common than one abandoning its
// obligation, so a breach warns before it suspends. Suspending on the first
// miss would cost the symbol its only liquidity exactly when it needs it most.
func TestProviderIsWarnedBeforeSuspended(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)

	maker := newCardholder(t, p, "Thin Quotes Ltd")
	member, _, _ := membership(t, p, n, n.cardholderID, maker)
	mustExec(t, p, `UPDATE members SET roles = ARRAY['broker','market_maker'] WHERE id = $1`, member)

	var provider uuid.UUID
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		provider, err = exchange.Appoint(ctx, tx,
			exchange.StandardObligation(n.instrumentID, member, "2027-01-01", "2028-01-01"))
		return err
	})

	// Ten sessions, all missed.
	for i := 1; i <= 10; i++ {
		mustExec(t, p, `
			INSERT INTO lp_performance (provider_id, session_date, quoted, met, shortfall)
			VALUES ($1, date '2027-06-01' + $2::int, false, false, 'did not quote')`, provider, i)
	}

	review := func() (int, int, int) {
		var w, s, r int
		mustTx(t, p, func(tx pgx.Tx) error {
			var err error
			w, s, r, err = exchange.ReviewProviders(ctx, tx, 20, 5)
			return err
		})
		return w, s, r
	}

	if w, s, _ := review(); w != 1 || s != 0 {
		t.Fatalf("first review warned %d suspended %d, want 1 and 0", w, s)
	}
	if got := providerState(t, p, provider); got != "warned" {
		t.Fatalf("state = %s, want warned", got)
	}
	if _, s, _ := review(); s != 1 {
		t.Fatal("a second failing review did not suspend")
	}
	if got := providerState(t, p, provider); got != "suspended" {
		t.Fatalf("state = %s, want suspended", got)
	}
}

// A provider appointed last week has not failed; there is simply not enough
// evidence yet.
func TestProviderNotJudgedOnTooFewSessions(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)

	maker := newCardholder(t, p, "New Quotes Ltd")
	member, _, _ := membership(t, p, n, n.cardholderID, maker)
	mustExec(t, p, `UPDATE members SET roles = ARRAY['broker','market_maker'] WHERE id = $1`, member)

	var provider uuid.UUID
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		provider, err = exchange.Appoint(ctx, tx,
			exchange.StandardObligation(n.instrumentID, member, "2027-01-01", "2028-01-01"))
		return err
	})
	for i := 1; i <= 2; i++ {
		mustExec(t, p, `
			INSERT INTO lp_performance (provider_id, session_date, quoted, met)
			VALUES ($1, date '2027-06-01' + $2::int, false, false)`, provider, i)
	}

	var warned int
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		warned, _, _, err = exchange.ReviewProviders(ctx, tx, 20, 5)
		return err
	})
	if warned != 0 {
		t.Fatal("a provider was warned on two sessions of evidence")
	}
	if got := providerState(t, p, provider); got != "active" {
		t.Fatalf("state = %s, want active", got)
	}
}

func providerState(t *testing.T, p interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, id uuid.UUID) string {
	t.Helper()
	var s string
	if err := p.QueryRow(context.Background(),
		`SELECT state FROM liquidity_providers WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}
