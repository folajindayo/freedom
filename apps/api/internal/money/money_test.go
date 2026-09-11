package money

import "testing"

func TestKoboString(t *testing.T) {
	for _, tc := range []struct {
		in   Kobo
		want string
	}{
		{0, "₦0.00"},
		{5, "₦0.05"},
		{Naira(20_000), "₦20,000.00"},
		{Naira(1_000_000) + 45, "₦1,000,000.45"},
		{-Naira(250), "−₦250.00"},
	} {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("Kobo(%d).String() = %q, want %q", int64(tc.in), got, tc.want)
		}
	}
}

// The two rounding directions must actually differ, and each must be the one
// its name promises. A test that only checks exact division proves nothing.
func TestRateRoundingDirection(t *testing.T) {
	amount := Naira(100) // ₦100.00 = 10,000 kobo
	const bps = 33       // 0.33% = 33 kobo exactly

	if got := RateOf(amount, bps); got != 33 {
		t.Fatalf("RateOf exact case = %s", got)
	}

	// 10,001 kobo at 33bps = 33.0033 kobo: down is 33, up is 34.
	amount = Kobo(10_001)
	if got := RateOf(amount, bps); got != 33 {
		t.Errorf("RateOf(%s, %d) = %s, want ₦0.33 (rounds toward zero)", amount, bps, got)
	}
	if got := RateOfUp(amount, bps); got != 34 {
		t.Errorf("RateOfUp(%s, %d) = %s, want ₦0.34 (rounds away from zero)", amount, bps, got)
	}
}

// Reversals carry negative amounts through the same rate functions, and "round
// down" must mean toward zero in both directions or a reversal will not undo
// the fee it is reversing.
func TestRateRoundingIsSymmetricAboutZero(t *testing.T) {
	a := Kobo(10_001)
	if RateOf(a, 33) != -RateOf(-a, 33) {
		t.Errorf("RateOf is not symmetric: %s vs %s", RateOf(a, 33), RateOf(-a, 33))
	}
	if RateOfUp(a, 33) != -RateOfUp(-a, 33) {
		t.Errorf("RateOfUp is not symmetric: %s vs %s", RateOfUp(a, 33), RateOfUp(-a, 33))
	}
}

// The CBN-style card-present schedule: 0.50% capped at ₦1,000. The cap is what
// makes buyback value sublinear on large tickets, so it is pinned by a test.
func TestCappedMerchantServiceCharge(t *testing.T) {
	cap := Naira(1_000)
	for _, tc := range []struct {
		ticket Kobo
		want   Kobo
	}{
		{Naira(10_000), Naira(50)},       // 0.5% uncapped
		{Naira(200_000), Naira(1_000)},   // exactly at the cap
		{Naira(1_000_000), Naira(1_000)}, // capped: not ₦5,000
	} {
		got := Clamp(RateOf(tc.ticket, 50), 0, &cap)
		if got != tc.want {
			t.Errorf("MSC on %s = %s, want %s", tc.ticket, got, tc.want)
		}
	}
}

func TestClampFloorBeatsCap(t *testing.T) {
	cap := Kobo(10)
	if got := Clamp(Kobo(5), Kobo(50), &cap); got != 10 {
		t.Errorf("a cap below the floor must win, got %s", got)
	}
	if got := Clamp(Kobo(5), 0, nil); got != 5 {
		t.Errorf("a nil cap means uncapped, got %s", got)
	}
}
