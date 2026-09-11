package alloc

import (
	"math/rand"
	"testing"
)

type kobo int64

// The defining property: a split never creates or destroys value.
func TestProportionalSumsToTotal(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 20_000; i++ {
		n := 1 + rng.Intn(8)
		weights := make([]int64, n)
		for j := range weights {
			weights[j] = rng.Int63n(10_000)
		}
		total := kobo(rng.Int63n(1_000_000_000_000))
		if rng.Intn(2) == 0 {
			total = -total
		}

		parts, err := Proportional(total, weights)
		if err != nil {
			continue // zero-weight vectors are a documented error, not a failure
		}
		var sum kobo
		for _, p := range parts {
			sum += p
		}
		if sum != total {
			t.Fatalf("split %d across %v summed to %d", total, weights, sum)
		}
	}
}

// Every part must lie within one unit of its exact proportional share, or the
// method has degenerated into "give it all to the first claimant".
func TestProportionalIsFair(t *testing.T) {
	parts, err := Proportional(kobo(100), []int64{1, 1, 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range parts {
		if p < 33 || p > 34 {
			t.Fatalf("equal thirds of 100 should be 33 or 34, got %v", parts)
		}
	}
}

// The bug this package exists to prevent: an allocator that is right nine runs
// in ten because it ranged over a map.
func TestProportionalIsDeterministic(t *testing.T) {
	weights := []int64{7, 11, 13, 17, 19, 23}
	first, err := Proportional(kobo(1_000_003), weights)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1_000; i++ {
		again, err := Proportional(kobo(1_000_003), weights)
		if err != nil {
			t.Fatal(err)
		}
		for j := range first {
			if first[j] != again[j] {
				t.Fatalf("iteration %d diverged at index %d: %v vs %v", i, j, first, again)
			}
		}
	}
}

// Ties go to the lower index, and that has to be observable rather than
// incidental: 10 across four equal claimants gives the first two the extra.
func TestProportionalTiesGoToLowerIndex(t *testing.T) {
	parts, err := Proportional(kobo(10), []int64{1, 1, 1, 1})
	if err != nil {
		t.Fatal(err)
	}
	want := []kobo{3, 3, 2, 2}
	for i := range want {
		if parts[i] != want[i] {
			t.Fatalf("got %v, want %v", parts, want)
		}
	}
}

// Reversing a fee split must return every account to its prior balance. If the
// negative path rounded independently, a full reversal would leave dust behind
// in some accounts and overdraw others.
func TestReversalNetsToZero(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 10_000; i++ {
		weights := []int64{rng.Int63n(500) + 1, rng.Int63n(500) + 1, rng.Int63n(500) + 1}
		total := kobo(rng.Int63n(1_000_000))

		fwd, err := Proportional(total, weights)
		if err != nil {
			t.Fatal(err)
		}
		rev, err := Proportional(-total, weights)
		if err != nil {
			t.Fatal(err)
		}
		for j := range fwd {
			if fwd[j]+rev[j] != 0 {
				t.Fatalf("component %d: %d forward + %d reversed = %d, want 0",
					j, fwd[j], rev[j], fwd[j]+rev[j])
			}
		}
	}
}

// A weight of zero earns nothing; it must not absorb a remainder unit.
func TestZeroWeightGetsNothing(t *testing.T) {
	parts, err := Proportional(kobo(101), []int64{0, 1, 1})
	if err != nil {
		t.Fatal(err)
	}
	if parts[0] != 0 {
		t.Fatalf("zero weight took %d", parts[0])
	}
	if parts[1]+parts[2] != 101 {
		t.Fatalf("remaining claimants got %d, want 101", parts[1]+parts[2])
	}
}

// Intermediates must not wrap: ₦10bn against 1e8-scale weights overflows int64
// long before it overflows the big.Int path.
func TestNoOverflowOnLargeTotals(t *testing.T) {
	parts, err := Proportional(kobo(1_000_000_000_000), []int64{100_000_000, 100_000_000, 1})
	if err != nil {
		t.Fatal(err)
	}
	var sum kobo
	for _, p := range parts {
		if p < 0 {
			t.Fatalf("negative part %d from a positive total — intermediate wrapped: %v", p, parts)
		}
		sum += p
	}
	if sum != 1_000_000_000_000 {
		t.Fatalf("sum %d", sum)
	}
}

func TestErrors(t *testing.T) {
	if _, err := Proportional(kobo(5), []int64{0, 0}); err == nil {
		t.Fatal("splitting a non-zero total across zero weights must error, not divide by zero")
	}
	if _, err := Proportional(kobo(5), []int64{-1, 2}); err == nil {
		t.Fatal("negative weights must error")
	}
	if parts, err := Proportional(kobo(0), []int64{0, 0}); err != nil || parts[0] != 0 {
		t.Fatalf("zero across zero weights is a no-op, got %v %v", parts, err)
	}
}
