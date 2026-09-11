package share

import (
	"math/rand"
	"testing"

	"freedom/api/internal/money"
)

func TestUnitsForConservesFunding(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	for i := 0; i < 50_000; i++ {
		funding := money.Kobo(rng.Int63n(10_000_000)) // up to ₦100k per tap
		price := money.Kobo(rng.Int63n(5_000_000) + 1)

		units, spent, residual, err := UnitsFor(funding, price)
		if err != nil {
			t.Fatalf("UnitsFor(%s, %s): %v", funding, price, err)
		}
		if spent+residual != funding {
			t.Fatalf("UnitsFor(%s, %s): spent %s + residual %s != funding",
				funding, price, spent, residual)
		}
		if spent > funding {
			t.Fatalf("UnitsFor(%s, %s) spent %s, more than funded", funding, price, spent)
		}
		if residual < 0 {
			t.Fatalf("UnitsFor(%s, %s) left a negative residual %s", funding, price, residual)
		}
		if units == 0 && residual != funding {
			t.Fatalf("UnitsFor(%s, %s) granted nothing but kept %s", funding, price, spent)
		}
		if units > 0 && spent <= 0 {
			t.Fatalf("UnitsFor(%s, %s) granted %s units for nothing", funding, price, units)
		}
	}
}

// The residual must be smaller than the price of a single unit — otherwise we
// under-allocated and the cardholder is owed equity we did not grant.
func TestResidualIsAlwaysSubUnit(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	for i := 0; i < 50_000; i++ {
		funding := money.Kobo(rng.Int63n(10_000_000))
		price := money.Kobo(rng.Int63n(1_000_000) + 1)

		units, _, residual, err := UnitsFor(funding, price)
		if err != nil {
			t.Fatal(err)
		}
		if units == 0 {
			continue // deferred dust, handled by the caller
		}
		// One more unit would have cost at least ceil(price/1e8) kobo more.
		_, moreSpent, _, err := UnitsFor(funding, price)
		if err != nil {
			t.Fatal(err)
		}
		if residual >= price {
			t.Fatalf("UnitsFor(%s, %s) left %s residual, enough for another whole share (spent %s)",
				funding, price, residual, moreSpent)
		}
	}
}

// Exact division must leave nothing behind: ₦5 of a ₦40 share is precisely
// 0.125 shares and no change.
func TestUnitsForExactDivision(t *testing.T) {
	units, spent, residual, err := UnitsFor(money.Naira(5), money.Naira(40))
	if err != nil {
		t.Fatal(err)
	}
	if units != PerShare/8 {
		t.Fatalf("got %s shares, want 0.125", units)
	}
	if spent != money.Naira(5) || residual != 0 {
		t.Fatalf("spent %s residual %s, want ₦5.00 and nothing", spent, residual)
	}
}

// A share priced above the entire funding yields dust, not a free share and not
// a silent zero-with-cash-taken.
func TestDustBelowOneUnit(t *testing.T) {
	units, spent, residual, err := UnitsFor(money.Kobo(1), money.Kobo(500_000_000))
	if err != nil {
		t.Fatal(err)
	}
	if units != 0 || spent != 0 || residual != 1 {
		t.Fatalf("units %s spent %s residual %s: dust must return the funding untouched",
			units, spent, residual)
	}
}

func TestUnitsForRejectsBadInput(t *testing.T) {
	if _, _, _, err := UnitsFor(money.Naira(1), 0); err == nil {
		t.Fatal("a zero price must error rather than divide by zero")
	}
	if _, _, _, err := UnitsFor(money.Naira(1), -100); err == nil {
		t.Fatal("a negative price must error")
	}
	if _, _, _, err := UnitsFor(-1, money.Naira(1)); err == nil {
		t.Fatal("negative funding must error")
	}
}

func TestUnitsString(t *testing.T) {
	for _, tc := range []struct {
		in   Units
		want string
	}{
		{0, "0"},
		{PerShare, "1"},
		{PerShare / 8, "0.125"},
		{PerShare*12 + PerShare/2, "12.5"},
		{1, "0.00000001"},
		{-PerShare / 4, "−0.25"},
	} {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("Units(%d).String() = %q, want %q", int64(tc.in), got, tc.want)
		}
	}
}
