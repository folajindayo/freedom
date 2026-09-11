package exchange

import (
	"freedom/api/internal/money"
	"freedom/api/internal/share"
)

// bruteForce is a deliberately slow reference implementation of the uncross.
//
// It evaluates executable volume at EVERY tick in the price band rather than at
// the candidate prices Cross enumerates. That makes it an independent check of
// the thing most likely to be wrong: not the arithmetic, but whether the
// candidate set can miss the true maximum. If Cross's candidates are ever
// incomplete, the two disagree and the fuzzer says so.
//
// It is the most valuable test artefact in this package, because it turns
// "is the ladder right?" from an argument into a mechanical question.
func bruteForce(book []Order, p Params) (execMax share.Units, maximisers []money.Kobo) {
	for price := p.BandLo; price <= p.BandHi; price += p.Tick {
		d, s := depth(book, price, p.Lot)
		e := minUnits(d, s)
		if p.Lot > 1 {
			e -= e % p.Lot
		}
		switch {
		case e > execMax:
			execMax = e
			maximisers = []money.Kobo{price}
		case e == execMax && e > 0:
			maximisers = append(maximisers, price)
		}
	}
	return execMax, maximisers
}

// isAdmissible reports whether a price is one someone actually named, or the
// reference clamped into the band. This is the invariant that keeps an auction
// from ever printing at a band edge nobody bid.
func isAdmissible(book []Order, p Params, price money.Kobo) bool {
	ref := p.PrevRef
	if ref < p.BandLo {
		ref = p.BandLo
	}
	if ref > p.BandHi {
		ref = p.BandHi
	}
	if price == ref {
		return true
	}
	for _, o := range book {
		if !o.IsMarket() && o.Limit == price && price >= p.BandLo && price <= p.BandHi {
			return true
		}
	}
	return false
}
