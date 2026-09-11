// Package alloc splits an integer total into parts that sum to exactly the total.
//
// Two places in Ọja divide an indivisible quantity among claimants, and both
// are places where a lost kobo is a settlement break that someone has to
// reconcile by hand:
//
//	fee splits          one MSC total across interchange, scheme, buyback pool
//	buyback allocation  the units a batch actually bought, across its intents
//
// Both use the largest-remainder method. Each claimant takes the floor of its
// exact share, and the kobo (or units) left over by flooring go one each to the
// claimants with the largest discarded remainders. The parts therefore sum to
// the total by construction rather than by luck.
//
// # Determinism
//
// Ties are broken by ascending index, never by map iteration. This matters more
// than it looks: a split keyed off a Go map produces a function that returns a
// different answer on maybe one run in ten, which is a test that passes in CI
// and a merchant statement that disagrees with clearing once a quarter. Callers
// therefore pass an ordered slice, and the order is part of the contract.
package alloc

import (
	"fmt"
	"math/big"
	"sort"
)

// Integer is any type whose underlying representation is int64 — money.Kobo and
// share.Units both qualify, so one implementation serves naira and equity.
type Integer interface{ ~int64 }

// Proportional splits total across len(weights) parts in proportion to weights,
// using largest remainder. The returned slice always sums to exactly total.
//
// A negative total (a reversal) is allocated on its magnitude and negated, so
// that reversing a split returns every account to its prior balance — see
// TestReversalNetsToZero. Weights must be non-negative.
func Proportional[T Integer](total T, weights []int64) ([]T, error) {
	out := make([]T, len(weights))
	if len(weights) == 0 {
		if total != 0 {
			return nil, fmt.Errorf("alloc: cannot split %d across zero claimants", int64(total))
		}
		return out, nil
	}

	neg := total < 0
	mag := total
	if neg {
		mag = -mag
	}

	var sum int64
	for i, w := range weights {
		if w < 0 {
			return nil, fmt.Errorf("alloc: weight %d is negative (%d)", i, w)
		}
		sum += w
	}
	if sum == 0 {
		if mag != 0 {
			return nil, fmt.Errorf("alloc: cannot split %d across weights that sum to zero", int64(total))
		}
		return out, nil
	}

	// total*weight overflows int64 well inside real ranges (a ₦10bn batch
	// against a 1e8-scaled weight), so the intermediate is exact big.Int.
	// This runs once per batch, not once per tap.
	bigTotal := big.NewInt(int64(mag))
	bigSum := big.NewInt(sum)

	type rem struct {
		idx int
		r   *big.Int
	}
	rems := make([]rem, len(weights))

	var allocated int64
	for i, w := range weights {
		num := new(big.Int).Mul(bigTotal, big.NewInt(w))
		q, r := new(big.Int).QuoRem(num, bigSum, new(big.Int))
		out[i] = T(q.Int64())
		allocated += q.Int64()
		rems[i] = rem{idx: i, r: r}
	}

	leftover := int64(mag) - allocated
	if leftover < 0 || leftover > int64(len(rems)) {
		// Unreachable given floor division, but this is a money path: fail
		// loudly with the numbers rather than panicking on an index.
		return nil, fmt.Errorf("alloc: leftover %d outside [0,%d] splitting %d",
			leftover, len(rems), int64(total))
	}

	// Largest remainder first; ties to the lower index. Stable ordering is the
	// whole point, so this is an explicit comparator, not a map range.
	sort.SliceStable(rems, func(a, b int) bool {
		if c := rems[a].r.Cmp(rems[b].r); c != 0 {
			return c > 0
		}
		return rems[a].idx < rems[b].idx
	})
	for i := int64(0); i < leftover; i++ {
		out[rems[i].idx]++
	}

	if neg {
		for i := range out {
			out[i] = -out[i]
		}
	}
	return out, nil
}
