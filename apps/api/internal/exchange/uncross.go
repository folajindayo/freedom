package exchange

import (
	"fmt"
	"sort"

	"freedom/api/internal/money"
	"freedom/api/internal/share"
)

// EngineVersion is the version of the rules below.
//
// It is recorded on every session, and replay dispatches on it. The first time
// a real auction hits a case these rules did not anticipate they will change,
// and sessions that already settled must continue to replay under the rules
// they settled under — otherwise the audit trail says a price was wrong when
// what actually happened is that the rules moved.
const EngineVersion = 1

// Tie-break rungs, recorded on the session so that "why was the price ₦104?"
// has an answer a cardholder can be given.
const (
	RuleMaxVolume   = "max_volume"   // one price cleared the most
	RuleMinSurplus  = "min_surplus"  // tied on volume, least left unfilled
	RuleSurplusSide = "surplus_side" // tied on both; the surplus side pays
	RuleReference   = "reference"    // tied and balanced; the reference decides
)

// Params are the instrument's state and rules for one uncross.
type Params struct {
	// PrevRef is the previous session's reference price. Never zero: a listing
	// mandates one, which is what removes the "no price determinable" branch
	// from the ladder below.
	PrevRef money.Kobo
	Tick    money.Kobo
	Lot     share.Units
	BandLo  money.Kobo // inclusive
	BandHi  money.Kobo // inclusive
}

// Fill is one order's allocation.
type Fill struct {
	Seq   int64
	Side  Side
	Units share.Units
}

// Result is the outcome of an uncross.
type Result struct {
	// Determined is false when nothing crossed. The caller then carries the
	// reference forward rather than inventing a price.
	Determined bool
	Price      money.Kobo
	Exec       share.Units
	// Imbalance is D−S at the clearing price; positive is a buy surplus.
	Imbalance share.Units
	Rule      string
	Fills     []Fill

	// Abandoned is set when the session uncrossed but the result was refused —
	// a band breach or a tripped circuit breaker. The price is discarded, the
	// reference carries forward, and Reason says which gate refused it.
	Abandoned bool
	Reason    string
}

// Cross runs a call auction over a frozen book.
//
// It is pure: no clock, no database, no map iteration, no floating point, no
// generated identifiers. Everything it needs is in its arguments, which is what
// makes a session replayable byte-for-byte from its recorded order events — and
// a session that cannot be replayed cannot be audited.
//
// That purity also buys the best surveillance detection in the system: to ask
// whether an account marked the auction, re-run Cross with that account's
// orders removed and see whether the price moves. No heuristic, no model.
func Cross(book []Order, p Params) (Result, error) {
	if err := p.validate(); err != nil {
		return Result{}, err
	}

	// The admissible price set.
	//
	// This is the invariant that matters most in the whole engine: an auction
	// price may only be a limit price someone actually named, or the reference
	// clamped into the tied interval. Never a band edge.
	//
	// Without it, a book containing only market orders on both sides has every
	// price tied on volume with a constant imbalance, and the surplus-side rung
	// selects the top of the band: one market buy of 0.01 shares against one
	// market sell prints at reference × 1.20, becomes the day's reference, and
	// the buyback pays it tomorrow.
	candidates := p.admissible(book)
	if len(candidates) == 0 {
		return Result{Determined: false}, nil
	}

	type point struct {
		price money.Kobo
		exec  share.Units
		imb   share.Units
	}
	points := make([]point, 0, len(candidates))
	var best share.Units
	for _, c := range candidates {
		d, s := depth(book, c, p.Lot)
		e := minUnits(d, s)
		if p.Lot > 1 {
			e -= e % p.Lot
		}
		points = append(points, point{price: c, exec: e, imb: d - s})
		if e > best {
			best = e
		}
	}

	// Nothing crosses. Not an error: most sessions on a thin symbol look like
	// this, and the honest response is to carry the reference forward and say
	// so, not to manufacture a price.
	if best == 0 {
		return Result{Determined: false}, nil
	}

	// Rung 1: maximum executable volume.
	tied := points[:0:0]
	for _, pt := range points {
		if pt.exec == best {
			tied = append(tied, pt)
		}
	}
	rule := RuleMaxVolume

	// Rung 2: minimum surplus. This comes before the direction rule — Xetra,
	// Nasdaq and NYSE all order it this way.
	if len(tied) > 1 {
		minImb := absUnits(tied[0].imb)
		for _, pt := range tied[1:] {
			if a := absUnits(pt.imb); a < minImb {
				minImb = a
			}
		}
		kept := tied[:0:0]
		for _, pt := range tied {
			if absUnits(pt.imb) == minImb {
				kept = append(kept, pt)
			}
		}
		if len(kept) < len(tied) {
			rule = RuleMinSurplus
		}
		tied = kept
	}

	chosen := tied[0]
	if len(tied) > 1 {
		lo, hi := tied[0], tied[0]
		allPos, allNeg := true, true
		for _, pt := range tied {
			if pt.price < lo.price {
				lo = pt
			}
			if pt.price > hi.price {
				hi = pt
			}
			if pt.imb <= 0 {
				allPos = false
			}
			if pt.imb >= 0 {
				allNeg = false
			}
		}
		switch {
		// Rung 3: the surplus side pays. Unfilled buyers bid the price up;
		// unfilled sellers push it down.
		//
		// This is not subsumed by rung 2: two adjacent breakpoints can carry
		// exactly equal imbalance, because a buy limit priced at b changes
		// demand above b but not at b.
		case allPos:
			chosen, rule = hi, RuleSurplusSide
		case allNeg:
			chosen, rule = lo, RuleSurplusSide

		// Rung 4: the surplus changes sign across the tied range, or is zero
		// throughout. The reference decides — and when it lies strictly inside
		// the tied interval, the reference ITSELF is the price.
		//
		// Picking the nearest tied endpoint instead, which is the intuitive
		// rule, manufactures a price jump to the edge of the interval on a day
		// when nothing happened. Executable volume is quasi-concave, so every
		// price between two tied maxima is itself a maximum: the reference is a
		// legitimate clearing price here, not a compromise.
		default:
			rule = RuleReference
			switch {
			case p.PrevRef >= hi.price:
				chosen = hi
			case p.PrevRef <= lo.price:
				chosen = lo
			default:
				d, s := depth(book, p.PrevRef, p.Lot)
				e := minUnits(d, s)
				if p.Lot > 1 {
					e -= e % p.Lot
				}
				chosen = point{price: p.PrevRef, exec: e, imb: d - s}
			}
		}
	}

	fills, err := allocate(book, chosen.price, chosen.exec, p.Lot)
	if err != nil {
		return Result{}, err
	}

	return Result{
		Determined: true,
		Price:      chosen.price,
		Exec:       chosen.exec,
		Imbalance:  chosen.imb,
		Rule:       rule,
		Fills:      fills,
	}, nil
}

func (p Params) validate() error {
	switch {
	case p.PrevRef <= 0:
		return fmt.Errorf("exchange: a reference price is required; a listing without one has no band and no tie-break")
	case p.Tick <= 0:
		return fmt.Errorf("exchange: tick must be positive, got %s", p.Tick)
	case p.Lot <= 0:
		return fmt.Errorf("exchange: lot must be positive, got %s", p.Lot)
	case p.BandLo <= 0 || p.BandHi < p.BandLo:
		return fmt.Errorf("exchange: band [%s, %s] is not a range", p.BandLo, p.BandHi)
	}
	return nil
}

// admissible builds the candidate price set: limit prices inside the band, plus
// the reference clamped into it. Sorted and deduplicated so the enumeration
// below is deterministic without depending on the book's order.
func (p Params) admissible(book []Order) []money.Kobo {
	seen := make([]money.Kobo, 0, len(book)+1)
	for _, o := range book {
		if o.IsMarket() {
			continue
		}
		if o.Limit >= p.BandLo && o.Limit <= p.BandHi {
			seen = append(seen, o.Limit)
		}
	}
	ref := p.PrevRef
	if ref < p.BandLo {
		ref = p.BandLo
	}
	if ref > p.BandHi {
		ref = p.BandHi
	}
	seen = append(seen, ref)

	sort.Slice(seen, func(i, j int) bool { return seen[i] < seen[j] })
	out := seen[:0]
	for i, v := range seen {
		if i == 0 || v != seen[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// depth returns demand and supply at a price.
func depth(book []Order, price money.Kobo, lot share.Units) (d, s share.Units) {
	for _, o := range book {
		if !o.Executable(price) {
			continue
		}
		q := o.QtyAt(price, lot)
		if q <= 0 {
			continue
		}
		if o.Side == Buy {
			d += q
		} else {
			s += q
		}
	}
	return d, s
}

func minUnits(a, b share.Units) share.Units {
	if a < b {
		return a
	}
	return b
}

func absUnits(v share.Units) share.Units {
	if v < 0 {
		return -v
	}
	return v
}
