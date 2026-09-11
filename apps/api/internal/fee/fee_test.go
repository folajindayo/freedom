package fee

import (
	"math/rand"
	"testing"

	"oja/api/internal/money"
)

// The invariant the whole package exists to hold.
func TestComponentsAlwaysSumToMSC(t *testing.T) {
	s := SchemeV1()
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < 100_000; i++ {
		in := Input{
			Ticket:    money.Kobo(rng.Int63n(500_000_000)), // up to ₦5m
			CoFundBps: rng.Int63n(300),
		}
		b, err := Compute(s, in)
		if err != nil {
			t.Fatalf("Compute(%s): %v", in.Ticket, err)
		}
		var sum money.Kobo
		for _, v := range b.Components {
			if v < 0 {
				t.Fatalf("ticket %s: component went negative (%s)", in.Ticket, v)
			}
			sum += v
		}
		if sum != b.MSC {
			t.Fatalf("ticket %s: components sum to %s, MSC is %s", in.Ticket, sum, b.MSC)
		}
		if b.MSC > in.Ticket {
			t.Fatalf("ticket %s: MSC %s exceeds the sale", in.Ticket, b.MSC)
		}
	}
}

func TestDeterministic(t *testing.T) {
	s := SchemeV1()
	in := Input{Ticket: money.Naira(13_337), CoFundBps: 137}
	first, err := Compute(s, in)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2_000; i++ {
		again, _ := Compute(s, in)
		for _, c := range Order {
			if first.Components[c] != again.Components[c] {
				t.Fatalf("iteration %d: %s drifted from %s to %s",
					i, c, first.Components[c], again.Components[c])
			}
		}
	}
}

// The numbers quoted in the product plan, pinned so a schedule change that
// breaks the pitch fails a test rather than a board meeting.
func TestLaunchEconomics(t *testing.T) {
	s := SchemeV1()

	t.Run("₦10,000 tap, scheme funding only", func(t *testing.T) {
		b, err := Compute(s, Input{Ticket: money.Naira(10_000)})
		if err != nil {
			t.Fatal(err)
		}
		if b.MSC != money.Naira(50) {
			t.Errorf("MSC = %s, want ₦50.00", b.MSC)
		}
		if b.BuybackFunding != money.Naira(5) {
			t.Errorf("buyback funding = %s, want ₦5.00", b.BuybackFunding)
		}
		if b.NetToMerchant() != money.Naira(9_950) {
			t.Errorf("merchant nets %s, want ₦9,950.00", b.NetToMerchant())
		}
	})

	t.Run("₦10,000 tap with 1% merchant co-funding", func(t *testing.T) {
		b, err := Compute(s, Input{Ticket: money.Naira(10_000), CoFundBps: 100})
		if err != nil {
			t.Fatal(err)
		}
		if b.BuybackFunding != money.Naira(105) {
			t.Errorf("buyback funding = %s, want ₦105.00", b.BuybackFunding)
		}
		// Co-funding must not move the issuer's revenue.
		base, _ := Compute(s, Input{Ticket: money.Naira(10_000)})
		if b.Components[Interchange] != base.Components[Interchange] {
			t.Errorf("co-funding changed interchange from %s to %s",
				base.Components[Interchange], b.Components[Interchange])
		}
	})

	t.Run("the cap makes buyback sublinear", func(t *testing.T) {
		small, _ := Compute(s, Input{Ticket: money.Naira(10_000)})
		large, _ := Compute(s, Input{Ticket: money.Naira(1_000_000)})

		if large.MSC != money.Naira(1_000) {
			t.Errorf("MSC on a ₦1m ticket = %s, want the ₦1,000.00 cap", large.MSC)
		}
		// 100× the ticket yields only 20× the equity.
		if large.BuybackFunding != money.Naira(100) {
			t.Errorf("buyback on ₦1m = %s, want ₦100.00", large.BuybackFunding)
		}
		if large.BuybackFunding >= small.BuybackFunding*100 {
			t.Error("the cap is not binding — buyback scaled linearly with the ticket")
		}
	})
}

// A capped component's overflow has to land on the named residual, not vanish.
func TestCapOverflowGoesToResidual(t *testing.T) {
	s := SchemeV1()
	b, err := Compute(s, Input{Ticket: money.Naira(1_000_000)})
	if err != nil {
		t.Fatal(err)
	}
	// Interchange wants 60% of ₦1,000 = ₦600, which is exactly its cap.
	if b.Components[Interchange] != money.Naira(600) {
		t.Fatalf("interchange = %s, want its ₦600.00 cap", b.Components[Interchange])
	}

	// Tighten the cap and the difference must appear in the scheme's share.
	tight := money.Naira(100)
	s.Rates[0].Cap = &tight
	c, err := Compute(s, Input{Ticket: money.Naira(1_000_000)})
	if err != nil {
		t.Fatal(err)
	}
	if c.Components[Interchange] != tight {
		t.Fatalf("interchange = %s, want %s", c.Components[Interchange], tight)
	}
	if got := c.Components[SchemeFee] - b.Components[SchemeFee]; got != money.Naira(500) {
		t.Fatalf("residual absorbed %s of overflow, want ₦500.00", got)
	}
	var sum money.Kobo
	for _, v := range c.Components {
		sum += v
	}
	if sum != c.MSC {
		t.Fatalf("after capping, components sum to %s but MSC is %s", sum, c.MSC)
	}
}

// A reversal must return every account to the balance it had.
func TestReversalNetsToZero(t *testing.T) {
	s := SchemeV1()
	rng := rand.New(rand.NewSource(9))
	for i := 0; i < 20_000; i++ {
		in := Input{Ticket: money.Kobo(rng.Int63n(100_000_000)), CoFundBps: rng.Int63n(300)}
		fwd, err := Compute(s, in)
		if err != nil {
			t.Fatal(err)
		}
		rev := fwd.Negate()
		for _, c := range Order {
			if fwd.Components[c]+rev.Components[c] != 0 {
				t.Fatalf("%s: %s + %s != 0", c, fwd.Components[c], rev.Components[c])
			}
		}
		if fwd.BuybackFunding+rev.BuybackFunding != 0 {
			t.Fatal("buyback funding does not reverse cleanly")
		}
	}
}

// A partial reversal and its complement must reconstruct the original exactly,
// or a sequence of partials leaves dust in accounts nobody owns.
func TestPartialReversalsReconstructTheWhole(t *testing.T) {
	s := SchemeV1()
	rng := rand.New(rand.NewSource(21))
	for i := 0; i < 20_000; i++ {
		ticket := money.Kobo(rng.Int63n(50_000_000) + 1)
		full, err := Compute(s, Input{Ticket: ticket, CoFundBps: rng.Int63n(300)})
		if err != nil {
			t.Fatal(err)
		}
		part := money.Kobo(rng.Int63n(int64(ticket) + 1))

		a, b, err := full.Split(part)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range Order {
			if got := a.Components[c] + b.Components[c]; got != full.Components[c] {
				t.Fatalf("ticket %s split at %s: %s parts sum to %s, want %s",
					ticket, part, c, got, full.Components[c])
			}
		}
		if a.MSC+b.MSC != full.MSC {
			t.Fatalf("MSC parts %s + %s != %s", a.MSC, b.MSC, full.MSC)
		}
		if a.CoFund+b.CoFund != full.CoFund {
			t.Fatalf("co-fund parts do not reconstruct")
		}
		if a.Ticket+b.Ticket != full.Ticket {
			t.Fatalf("ticket parts %s + %s != %s", a.Ticket, b.Ticket, full.Ticket)
		}
		// Each side must be internally consistent too: a breakdown whose
		// components do not sum to its own MSC cannot be posted to the ledger.
		for _, side := range []Breakdown{a, b} {
			var sum money.Kobo
			for _, v := range side.Components {
				sum += v
			}
			if sum != side.MSC {
				t.Fatalf("split side has components summing to %s but MSC %s", sum, side.MSC)
			}
		}
	}
}

func TestScheduleValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    Schedule
	}{
		{"no residual named", Schedule{Version: 9, Rates: []Rate{{Component: SchemeFee, ShareBps: 10_000}}}},
		{"residual not priced", Schedule{Version: 9, Residual: BuybackPool,
			Rates: []Rate{{Component: SchemeFee, ShareBps: 10_000}}}},
		{"duplicate component", Schedule{Version: 9, Residual: SchemeFee,
			Rates: []Rate{{Component: SchemeFee, ShareBps: 5_000}, {Component: SchemeFee, ShareBps: 5_000}}}},
		{"unknown component", Schedule{Version: 9, Residual: SchemeFee,
			Rates: []Rate{{Component: "kickback", ShareBps: 10_000}, {Component: SchemeFee}}}},
		{"negative share", Schedule{Version: 9, Residual: SchemeFee,
			Rates: []Rate{{Component: SchemeFee, ShareBps: -1}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Compute(tc.s, Input{Ticket: money.Naira(100)}); err == nil {
				t.Fatal("an invalid schedule must be rejected")
			}
		})
	}
}

// A schedule whose floors exceed the MSC is unpriceable and must say so rather
// than post a negative component.
func TestImpossibleFloorsAreRejected(t *testing.T) {
	s := Schedule{
		Version: 9, MSCRateBps: 50, Residual: SchemeFee,
		Rates: []Rate{
			{Component: Interchange, ShareBps: 5_000, Floor: money.Naira(100)},
			{Component: SchemeFee, ShareBps: 5_000},
		},
	}
	if _, err := Compute(s, Input{Ticket: money.Naira(10)}); err == nil {
		t.Fatal("floors above the MSC must be rejected, not netted into a negative residual")
	}
}

// Tiny tickets are where flat fees and floors misbehave.
func TestSmallTickets(t *testing.T) {
	s := SchemeV1()
	for _, ticket := range []money.Kobo{0, 1, 2, 50, 99, 100} {
		b, err := Compute(s, Input{Ticket: ticket})
		if err != nil {
			t.Fatalf("ticket %s: %v", ticket, err)
		}
		var sum money.Kobo
		for _, v := range b.Components {
			sum += v
		}
		if sum != b.MSC {
			t.Errorf("ticket %s: components %s != MSC %s", ticket, sum, b.MSC)
		}
		if b.NetToMerchant() < 0 {
			t.Errorf("ticket %s: merchant nets %s — paid to be paid", ticket, b.NetToMerchant())
		}
	}
}
