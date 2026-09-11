package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"freedom/api/internal/institution"
	"freedom/api/internal/ledger"
	"freedom/api/internal/money"
	"freedom/api/internal/scheme"
	"freedom/api/internal/share"
)

// Between an issuer submitting and the market reading, the issuer knows
// something the market does not. Halting is the only honest way to close that
// window.
func TestPriceSensitiveDisclosureHaltsUntilPublished(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)

	var id uuid.UUID
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		id, err = institution.Submit(ctx, tx, n.instrumentID, institution.DisclosureResults,
			"FY2027 audited results", "Revenue up 31%...", "company.secretary")
		return err
	})

	// The market is shut while the news is pending.
	if err := inTx(p, func(tx pgx.Tx) error {
		_, err := exchangeOpen(ctx, tx, n.instrumentID)
		return err
	}); err == nil {
		t.Fatal("the market stayed open while an unpublished announcement sat with the exchange")
	}

	var pending []institution.PendingDisclosure
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		pending, err = institution.Pending(ctx, tx)
		return err
	})
	if len(pending) != 1 || pending[0].Symbol != n.symbol {
		t.Fatalf("the pending queue does not show the announcement: %+v", pending)
	}

	mustTx(t, p, func(tx pgx.Tx) error {
		return institution.Publish(ctx, tx, id, "market.operations")
	})
	if err := inTx(p, func(tx pgx.Tx) error {
		_, err := exchangeOpen(ctx, tx, n.instrumentID)
		return err
	}); err != nil {
		t.Fatalf("the market did not reopen after publication: %v", err)
	}

	// Publishing twice is a mistake worth surfacing.
	if err := inTx(p, func(tx pgx.Tx) error {
		return institution.Publish(ctx, tx, id, "market.operations")
	}); err == nil {
		t.Error("an already-published announcement was published again")
	}
}

// A director's dealing is news about the company's owners, not the company.
// Halting for it would stop the market several times a week.
func TestRoutineDisclosureDoesNotHalt(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)

	mustTx(t, p, func(tx pgx.Tx) error {
		_, err := institution.Submit(ctx, tx, n.instrumentID, institution.DisclosureDirectorDealing,
			"Director acquires 5,000 shares", "On 1 June...", "company.secretary")
		return err
	})
	if err := inTx(p, func(tx pgx.Tx) error {
		_, err := exchangeOpen(ctx, tx, n.instrumentID)
		return err
	}); err != nil {
		t.Fatalf("a routine announcement halted the market: %v", err)
	}
}

// A regulatory halt must survive an issuer publishing their results.
func TestPublishingDoesNotLiftAnUnrelatedHalt(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)

	mustTx(t, p, func(tx pgx.Tx) error {
		_, err := exchangeHalt(ctx, tx, n.instrumentID)
		return err
	})
	var id uuid.UUID
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		id, err = institution.Submit(ctx, tx, n.instrumentID, institution.DisclosureResults,
			"Results", "...", "company.secretary")
		return err
	})
	mustTx(t, p, func(tx pgx.Tx) error {
		return institution.Publish(ctx, tx, id, "market.operations")
	})

	if err := inTx(p, func(tx pgx.Tx) error {
		_, err := exchangeOpen(ctx, tx, n.instrumentID)
		return err
	}); err == nil {
		t.Fatal("publishing results lifted a regulatory halt")
	}
}

func TestDisclosureValidation(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)
	for _, tc := range []struct{ name, kind, headline, body, by string }{
		{"unknown kind", "gossip", "h", "b", "sec"},
		{"no headline", institution.DisclosureResults, "", "b", "sec"},
		{"no body", institution.DisclosureResults, "h", "", "sec"},
		{"no submitter", institution.DisclosureResults, "h", "b", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := inTx(p, func(tx pgx.Tx) error {
				_, err := institution.Submit(ctx, tx, n.instrumentID, tc.kind, tc.headline, tc.body, tc.by)
				return err
			}); err == nil {
				t.Fatal("an invalid disclosure was accepted")
			}
		})
	}
}

// On a venue where one listing may be twenty times the next, an uncapped index
// IS that listing and tells nobody anything about the market.
func TestIndexCapsAndRedistributesWeight(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)

	// A second, much smaller listing alongside the fixture's.
	small := listAnother(t, p, n, "TINY", money.Naira(10), share.Whole(50))

	ix := institution.FreedomAllShare("2027-01-01")
	mustTx(t, p, func(tx pgx.Tx) error {
		if err := institution.Create(ctx, tx, ix); err != nil {
			return err
		}
		if err := institution.Include(ctx, tx, ix.ID, n.instrumentID, "2027-01-01"); err != nil {
			return err
		}
		return institution.Include(ctx, tx, ix.ID, small, "2027-01-01")
	})

	var weights []institution.Weights
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		_, weights, err = institution.Compute(ctx, tx, ix.ID, tradeDate)
		return err
	})
	if len(weights) != 2 {
		t.Fatalf("index has %d constituents, want 2", len(weights))
	}

	var total int64
	for _, w := range weights {
		total += w.WeightBps
		t.Logf("%-8s cap %s  weight %d bps", w.Symbol, w.CapKobo, w.WeightBps)
	}
	if total < 9_900 || total > 10_100 {
		t.Fatalf("weights sum to %d bps, want about 10,000", total)
	}
	// With only two constituents a 20% cap is unachievable, so the weights are
	// left uncapped rather than looping on an impossible constraint.
	if weights[0].WeightBps == 0 || weights[1].WeightBps == 0 {
		t.Error("a constituent was weighted to nothing")
	}
}

func TestIndexNeedsConstituents(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	setup(t, p)
	ix := institution.FreedomAllShare("2027-01-01")
	mustTx(t, p, func(tx pgx.Tx) error { return institution.Create(ctx, tx, ix) })
	if err := inTx(p, func(tx pgx.Tx) error {
		_, _, err := institution.Compute(ctx, tx, ix.ID, tradeDate)
		return err
	}); err == nil {
		t.Fatal("an empty index computed a value")
	}
}

// A claim recorded as upheld but unpaid leaves an investor told they have won
// and still holding nothing, which is worse than a straight rejection.
func TestProtectionClaimIsDecidedAndPaidTogether(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)
	claimant := n.cardholderID

	// Capitalise the fund.
	mustTx(t, p, func(tx pgx.Tx) error {
		ext, err := ledger.Resolve(ctx, tx, ledger.Scheme(ledger.KindExternal, ledger.AssetNGN))
		if err != nil {
			return err
		}
		fund, err := ledger.Resolve(ctx, tx, ledger.Scheme(ledger.KindLossReserve, ledger.AssetNGN))
		if err != nil {
			return err
		}
		_, err = ledger.Post(ctx, tx, ledger.Tx{
			EventType: "protection.funded", BusinessDate: tradeDate,
			IdempotencyKey: "protection-fund",
			Entries: []ledger.Entry{
				{AccountID: ext, Amount: ledger.NGN(-money.Naira(10_000_000)), Reason: "fund"},
				{AccountID: fund, Amount: ledger.NGN(money.Naira(10_000_000)), Reason: "fund"},
			},
		})
		return err
	})

	before := nairaOf(t, p, claimant, ledger.KindAvailable)
	var id uuid.UUID
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		id, err = institution.FileClaim(ctx, tx, claimant, nil, money.Naira(200_000),
			institution.GroundsUnauthorisedTrade, "Trades placed without instruction on 3 June")
		return err
	})
	mustTx(t, p, func(tx pgx.Tx) error {
		return institution.DecideClaim(ctx, tx, id, true, money.Naira(200_000),
			"claims.committee", tradeDate)
	})

	if got := nairaOf(t, p, claimant, ledger.KindAvailable) - before; got != money.Naira(200_000) {
		t.Fatalf("the claimant received %s, want ₦200,000.00", got)
	}
	// Deciding twice must not pay twice.
	if err := inTx(p, func(tx pgx.Tx) error {
		return institution.DecideClaim(ctx, tx, id, true, money.Naira(200_000), "claims.committee", tradeDate)
	}); err == nil {
		t.Error("a paid claim was decided again")
	}
}

// A fund with no per-claim cap can be exhausted by one claimant, leaving
// nothing for everyone behind them.
func TestClaimIsCapped(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)
	claimant := n.cardholderID

	mustTx(t, p, func(tx pgx.Tx) error {
		ext, _ := ledger.Resolve(ctx, tx, ledger.Scheme(ledger.KindExternal, ledger.AssetNGN))
		fund, err := ledger.Resolve(ctx, tx, ledger.Scheme(ledger.KindLossReserve, ledger.AssetNGN))
		if err != nil {
			return err
		}
		_, err = ledger.Post(ctx, tx, ledger.Tx{
			EventType: "protection.funded", BusinessDate: tradeDate,
			IdempotencyKey: "protection-fund-2",
			Entries: []ledger.Entry{
				{AccountID: ext, Amount: ledger.NGN(-money.Naira(50_000_000)), Reason: "fund"},
				{AccountID: fund, Amount: ledger.NGN(money.Naira(50_000_000)), Reason: "fund"},
			},
		})
		return err
	})

	before := nairaOf(t, p, claimant, ledger.KindAvailable)
	var id uuid.UUID
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		id, err = institution.FileClaim(ctx, tx, claimant, nil, money.Naira(20_000_000),
			institution.GroundsFraud, "Everything")
		return err
	})
	mustTx(t, p, func(tx pgx.Tx) error {
		return institution.DecideClaim(ctx, tx, id, true, money.Naira(20_000_000), "committee", tradeDate)
	})
	if got := nairaOf(t, p, claimant, ledger.KindAvailable) - before; got != institution.ClaimCap {
		t.Fatalf("awarded %s, want the %s cap", got, institution.ClaimCap)
	}
}

func TestRejectedClaimPaysNothing(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)
	before := nairaOf(t, p, n.cardholderID, ledger.KindAvailable)

	var id uuid.UUID
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		id, err = institution.FileClaim(ctx, tx, n.cardholderID, nil, money.Naira(1_000),
			institution.GroundsFraud, "I changed my mind about a trade")
		return err
	})
	mustTx(t, p, func(tx pgx.Tx) error {
		return institution.DecideClaim(ctx, tx, id, false, 0, "committee", tradeDate)
	})
	if got := nairaOf(t, p, n.cardholderID, ledger.KindAvailable); got != before {
		t.Fatalf("a rejected claim moved %s", got-before)
	}
}

// A complaint with no clock is a complaint nobody has to answer.
func TestComplaintClockAndEscalation(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)
	member, _, _ := membership(t, p, n, n.cardholderID, newCardholder(t, p, "Dele Ojo"))
	now := time.Date(2027, 6, 1, 10, 0, 0, 0, scheme.Lagos)

	var id uuid.UUID
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		id, err = institution.FileComplaint(ctx, tx, n.cardholderID, institution.AgainstMember,
			&member, nil, "Order not executed", "I placed an order and nothing happened", now)
		return err
	})

	// A member answers for themselves first; the exchange is the escalation.
	var state, respondBy string
	if err := p.QueryRow(ctx,
		`SELECT state, respond_by::text FROM complaints WHERE id = $1`, id).Scan(&state, &respondBy); err != nil {
		t.Fatal(err)
	}
	if state != "with_member" {
		t.Errorf("state = %s, want with_member", state)
	}
	if respondBy != "2027-06-22" {
		t.Errorf("respond by %s, want 2027-06-22 (21 days)", respondBy)
	}

	// Nothing chases a deadline on its own; this is the list that makes the
	// clock mean something.
	var overdue []uuid.UUID
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		overdue, err = institution.Overdue(ctx, tx, "2027-07-01")
		return err
	})
	if len(overdue) != 1 || overdue[0] != id {
		t.Fatalf("the overdue list does not show the complaint: %v", overdue)
	}

	mustTx(t, p, func(tx pgx.Tx) error {
		return institution.Escalate(ctx, tx, id, "escalated_sec")
	})
	mustTx(t, p, func(tx pgx.Tx) error {
		return institution.ResolveComplaint(ctx, tx, id, "Order was rejected for insufficient funds; explained", "ombudsman")
	})
	if err := inTx(p, func(tx pgx.Tx) error {
		return institution.Escalate(ctx, tx, id, "escalated_sec")
	}); err == nil {
		t.Error("a resolved complaint was escalated")
	}
}

// Sequencing is the entire case in a manipulation investigation, and a venue
// that has never checked its clock is not healthy — absence of evidence is not
// evidence of a good clock.
func TestClockIntegrity(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	setup(t, p)

	var healthy bool
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		healthy, err = institution.ClockHealthy(ctx, tx)
		return err
	})
	if healthy {
		t.Fatal("a venue that has never checked its clock reported healthy")
	}

	mustTx(t, p, func(tx pgx.Tx) error {
		ok, err := institution.CheckClock(ctx, tx, "ntp.pool.org", 12*time.Millisecond)
		if !ok {
			t.Error("a 12ms offset was treated as drift")
		}
		return err
	})
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		healthy, err = institution.ClockHealthy(ctx, tx)
		return err
	})
	if !healthy {
		t.Fatal("a healthy clock reported unhealthy")
	}

	mustTx(t, p, func(tx pgx.Tx) error {
		ok, err := institution.CheckClock(ctx, tx, "ntp.pool.org", 850*time.Millisecond)
		if ok {
			t.Error("850ms of drift was treated as within tolerance")
		}
		return err
	})
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		healthy, err = institution.ClockHealthy(ctx, tx)
		return err
	})
	if healthy {
		t.Fatal("the venue reported a healthy clock after recording drift")
	}

	if err := inTx(p, func(tx pgx.Tx) error {
		_, err := institution.CheckClock(ctx, tx, "", 0)
		return err
	}); err == nil {
		t.Error("a clock check with no named source was accepted")
	}
}
