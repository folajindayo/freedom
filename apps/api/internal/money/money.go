// Package money represents naira amounts as integer kobo.
//
// Every monetary value in Ọja is an int64 count of kobo (1 naira = 100 kobo).
// Floating point never touches money, at rest or in transit: a clearing run
// spanning a day of taps must produce byte-identical results on every machine,
// and float drift is indistinguishable from a real settlement break.
//
// Shares live in a separate package with a separate type (see internal/share).
// That is deliberate. The buyback allocator holds naira and share quantities in
// the same function, and a bare int64 for both would let the compiler watch us
// add one to the other.
package money

import (
	"fmt"
	"strings"
)

// Kobo is an amount in kobo. Signed, because ledger entries are signed.
type Kobo int64

const NairaInKobo Kobo = 100

// Naira builds an amount from whole naira.
func Naira(n int64) Kobo { return Kobo(n) * NairaInKobo }

// Whole returns the whole-naira part, truncated toward zero.
func (k Kobo) Whole() int64 { return int64(k) / int64(NairaInKobo) }

// String renders an amount the way a Nigerian reader expects it: ₦20,000.00.
// Negative amounts use U+2212 MINUS, not a hyphen, so statements line up.
func (k Kobo) String() string {
	sign := ""
	v := int64(k)
	if v < 0 {
		sign, v = "−", -v
	}
	whole, frac := v/100, v%100

	digits := fmt.Sprintf("%d", whole)
	var b strings.Builder
	for i, r := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return fmt.Sprintf("%s₦%s.%02d", sign, b.String(), frac)
}

// Abs returns the magnitude of an amount.
func (k Kobo) Abs() Kobo {
	if k < 0 {
		return -k
	}
	return k
}

// RateOf applies a basis-point rate to an amount, truncating toward zero.
//
// Truncation is the wrong default for exactly half of the call sites, which is
// why callers that care use RateOfUp instead. Fee components take the floor —
// the network never charges a kobo it cannot justify from the schedule — and
// the shortfall is redistributed deterministically by alloc.Proportional.
func RateOf(amount Kobo, bps int64) Kobo {
	if amount == 0 || bps == 0 {
		return 0
	}
	neg := amount < 0
	if neg {
		amount = -amount
	}
	v := Kobo(int64(amount) * bps / 10_000)
	if neg {
		return -v
	}
	return v
}

// RateOfUp applies a basis-point rate rounding away from zero.
//
// Direction here is a risk decision, not a formatting one, and each call site
// picks it deliberately: round down for money we grant (a rebate, a buyback
// allocation) and up for money we collect or measure exposure against (a
// merchant's cash due to treasury, a net debit position). The house is never
// worse off for a rounding choice it did not make on purpose.
func RateOfUp(amount Kobo, bps int64) Kobo {
	if amount == 0 || bps == 0 {
		return 0
	}
	neg := amount < 0
	if neg {
		amount = -amount
	}
	n := int64(amount) * bps
	v := Kobo(n / 10_000)
	if n%10_000 != 0 {
		v++
	}
	if neg {
		return -v
	}
	return v
}

// Clamp applies an optional floor and cap to a fee component. A nil cap means
// uncapped; the floor is always applied first so a cap below the floor wins,
// which is the behaviour a schedule author expects when they typo one of them.
func Clamp(v Kobo, floor Kobo, cap *Kobo) Kobo {
	if v < floor {
		v = floor
	}
	if cap != nil && v > *cap {
		v = *cap
	}
	return v
}
