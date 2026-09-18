package exchange

import (
	"math"
	"testing"

	"freedom/api/internal/money"
	"freedom/api/internal/share"
)

// The exchange names the listing price: fair value over the shares in issue,
// rounded down to the tick, never below one tick.
func TestListingPrice(t *testing.T) {
	std := StandardCriteria()
	half := std
	half.RevenueMultipleBps = 5_000 // 0.5×

	cases := []struct {
		name      string
		c         Criteria
		e         Evidence
		tick      money.Kobo
		wantPrice money.Kobo
		wantFair  money.Kobo
	}{
		{
			// (₦20m + ₦60m) / 10,000,000 = ₦8.00, the brief's example.
			name: "worked example",
			c:    std,
			e: Evidence{NetAssetsKobo: money.Naira(20_000_000), RevenueKobo: money.Naira(60_000_000),
				SharesInIssue: share.Whole(10_000_000)},
			tick: 1, wantPrice: money.Naira(8), wantFair: money.Naira(80_000_000),
		},
		{
			// (₦120m + ₦280m) / 8,000,000 = ₦50.00.
			name: "mama put at fifty",
			c:    std,
			e: Evidence{NetAssetsKobo: money.Naira(120_000_000), RevenueKobo: money.Naira(280_000_000),
				SharesInIssue: share.Whole(8_000_000)},
			tick: 1, wantPrice: money.Naira(50), wantFair: money.Naira(400_000_000),
		},
		{
			// The multiple scales revenue only: ₦20m + 0.5 × ₦60m = ₦50m.
			name: "half multiple",
			c:    half,
			e: Evidence{NetAssetsKobo: money.Naira(20_000_000), RevenueKobo: money.Naira(60_000_000),
				SharesInIssue: share.Whole(10_000_000)},
			tick: 1, wantPrice: money.Naira(5), wantFair: money.Naira(50_000_000),
		},
		{
			// ₦1,000 of fair value over 1,000,000 shares is 0.1 kobo a share:
			// below a tick, so one tick.
			name: "below a tick is a tick",
			c:    std,
			e:    Evidence{NetAssetsKobo: money.Naira(1_000), SharesInIssue: share.Whole(1_000_000)},
			tick: 1, wantPrice: 1, wantFair: money.Naira(1_000),
		},
		{
			// ₦12.34 a share on a 5-kobo tick rounds DOWN to ₦12.30.
			name: "rounds down to the tick",
			c:    std,
			e:    Evidence{NetAssetsKobo: 1234 * 1_000, SharesInIssue: share.Whole(1_000)},
			tick: 5, wantPrice: 1230, wantFair: 1234 * 1_000,
		},
		{
			// No shares in issue: no price per share exists; one tick, and the
			// float criterion refuses it anyway.
			name: "no shares in issue",
			c:    std,
			e:    Evidence{NetAssetsKobo: money.Naira(1_000_000)},
			tick: 1, wantPrice: 1, wantFair: money.Naira(1_000_000),
		},
		{
			// Nothing to price: one tick, not zero.
			name: "no financials",
			c:    std,
			e:    Evidence{SharesInIssue: share.Whole(1_000)},
			tick: 1, wantPrice: 1, wantFair: 0,
		},
		{
			// A real issuer: ₦10bn of fair value over 100,000,000 shares is
			// 1e16 units, and FairValue × PerShare is 1e20 — past int64. Exact
			// arithmetic gives ₦100.00; a wrapped int64 would not.
			name: "large issuer does not wrap",
			c:    std,
			e: Evidence{NetAssetsKobo: money.Naira(4_000_000_000), RevenueKobo: money.Naira(6_000_000_000),
				SharesInIssue: share.Whole(100_000_000)},
			tick: 1, wantPrice: money.Naira(100), wantFair: money.Naira(10_000_000_000),
		},
		{
			// A zero tick is a kobo tick, not a division by zero.
			name: "zero tick means kobo",
			c:    std,
			e:    Evidence{NetAssetsKobo: money.Naira(800), SharesInIssue: share.Whole(100)},
			tick: 0, wantPrice: money.Naira(8), wantFair: money.Naira(800),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			price, fair := ListingPrice(tc.c, tc.e, tc.tick)
			if price != tc.wantPrice {
				t.Errorf("price = %s, want %s", price, tc.wantPrice)
			}
			if fair != tc.wantFair {
				t.Errorf("fair value = %s, want %s", fair, tc.wantFair)
			}
			if tc.tick > 0 && int64(price)%int64(tc.tick) != 0 {
				t.Errorf("price %s is off the %s tick", price, tc.tick)
			}
		})
	}
}

// Fair value saturates rather than wrapping when the accounts are absurd.
func TestFairValueSaturates(t *testing.T) {
	c := StandardCriteria()
	c.RevenueMultipleBps = 100_000 // 10×
	e := Evidence{NetAssetsKobo: math.MaxInt64, RevenueKobo: math.MaxInt64, SharesInIssue: share.Whole(1)}
	if got := FairValue(c, e); got != math.MaxInt64 {
		t.Errorf("fair value = %d, want saturated", got)
	}
	price, _ := ListingPrice(c, e, 1)
	if price <= 0 {
		t.Errorf("price = %s, wrapped negative", price)
	}
}

func TestMultipleString(t *testing.T) {
	for bps, want := range map[int64]string{10_000: "1.0×", 12_500: "1.25×", 5_000: "0.5×", 20_000: "2.0×"} {
		if got := multipleString(bps); got != want {
			t.Errorf("%d bps = %s, want %s", bps, got, want)
		}
	}
}
