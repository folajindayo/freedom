// Package share represents equity quantities as integer micro-units.
//
// One share is 1e8 Units, the same scale a satoshi bears to a bitcoin. The
// scale is forced by the product: a ₦5 buyback against a ₦40 share is 0.125
// shares, and rounding that to a whole share in either direction is either
// theft or a gift. int64 at 1e8 tops out near 92 billion shares, which is two
// orders of magnitude above the largest NGX issuer's count.
//
// Units is its own type, not an alias for money.Kobo, so that the compiler
// rejects the single most likely arithmetic bug in this system: crediting a
// stock wallet with a naira figure.
package share

import (
	"fmt"
	"math/big"
	"strings"

	"oja/api/internal/money"
)

// Units is a quantity of equity in 1e-8 share units. Signed, because registry
// entries are ledger entries and ledger entries are signed.
type Units int64

// PerShare is the number of Units in one whole share.
const PerShare Units = 100_000_000

// Whole builds a quantity from a whole number of shares.
func Whole(n int64) Units { return Units(n) * PerShare }

// String renders a quantity with trailing zeros trimmed: "0.125", "1", "12.5".
// Share counts are read by people deciding whether to sell, so an exact-looking
// 8dp figure on every row is noise; the underlying integer is never truncated.
func (u Units) String() string {
	sign := ""
	v := int64(u)
	if v < 0 {
		sign, v = "−", -v
	}
	whole, frac := v/int64(PerShare), v%int64(PerShare)
	if frac == 0 {
		return fmt.Sprintf("%s%d", sign, whole)
	}
	f := strings.TrimRight(fmt.Sprintf("%08d", frac), "0")
	return fmt.Sprintf("%s%d.%s", sign, whole, f)
}

// Abs returns the magnitude of a quantity.
func (u Units) Abs() Units {
	if u < 0 {
		return -u
	}
	return u
}

// UnitsFor converts naira funding into share units at a given price.
//
// It returns the units granted, the cash those units actually consume, and the
// residual funding left behind. The two roundings pull in opposite directions
// on purpose:
//
//	units  round DOWN — we never grant equity we were not funded for
//	spent  round UP   — treasury is never short-changed by a fraction of a kobo
//
// The residual is not revenue and must not be swept. It belongs to the
// cardholder who earned it and carries forward to their next buyback; a scheme
// that quietly keeps sub-kobo change from millions of taps has invented a fee
// it never disclosed, which is precisely the thing a regulator looks for.
//
// A funding amount too small to buy a single unit yields zero units and a
// residual equal to the whole funding. Callers must treat that as deferred
// dust, never as a completed allocation.
func UnitsFor(funding, price money.Kobo) (units Units, spent, residual money.Kobo, err error) {
	if price <= 0 {
		return 0, 0, 0, fmt.Errorf("share: price must be positive, got %d kobo", int64(price))
	}
	if funding < 0 {
		return 0, 0, 0, fmt.Errorf("share: funding must not be negative, got %d kobo", int64(funding))
	}
	if funding == 0 {
		return 0, 0, 0, nil
	}

	// funding*1e8 exceeds int64 for any funding above ~₦922m, so the
	// intermediate is exact big.Int rather than a silent wrap.
	num := new(big.Int).Mul(big.NewInt(int64(funding)), big.NewInt(int64(PerShare)))
	u := new(big.Int).Quo(num, big.NewInt(int64(price)))
	if !u.IsInt64() {
		return 0, 0, 0, fmt.Errorf("share: %s at %s overflows the unit scale", funding, price)
	}
	units = Units(u.Int64())
	if units == 0 {
		return 0, 0, funding, nil
	}

	cost := new(big.Int).Mul(u, big.NewInt(int64(price)))
	q, r := new(big.Int).QuoRem(cost, big.NewInt(int64(PerShare)), new(big.Int))
	if r.Sign() != 0 {
		q.Add(q, big.NewInt(1)) // spent rounds up
	}
	spent = money.Kobo(q.Int64())
	residual = funding - spent
	if residual < 0 {
		// Rounding up the cost cannot exceed the funding that produced it;
		// if it ever does, the arithmetic is wrong and must not post.
		return 0, 0, 0, fmt.Errorf("share: %s of units cost %s, more than the %s funding",
			units, spent, funding)
	}
	return units, spent, residual, nil
}
