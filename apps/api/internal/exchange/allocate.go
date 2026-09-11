package exchange

import (
	"fmt"
	"sort"

	"freedom/api/internal/alloc"
	"freedom/api/internal/money"
	"freedom/api/internal/share"
)

// allocate distributes exec units across each side of the book at price p.
//
// Both sides are rationed by the same walk. Rationing only "the surplus side"
// is almost right and breaks as soon as a lot size exists: flooring executable
// volume to a whole lot can leave BOTH sides holding more than exec, and a walk
// that only rations one of them produces two sides that do not sum to the same
// number — which the ledger's per-asset balance trigger then rejects at commit,
// after the whole session has been computed.
func allocate(book []Order, p money.Kobo, exec share.Units, lot share.Units) ([]Fill, error) {
	fills := make([]Fill, 0, len(book))
	for _, side := range []Side{Buy, Sell} {
		side := side
		got, err := allocateSide(book, side, p, exec, lot)
		if err != nil {
			return nil, err
		}
		fills = append(fills, got...)
	}
	sort.Slice(fills, func(i, j int) bool {
		if fills[i].Side != fills[j].Side {
			return fills[i].Side < fills[j].Side
		}
		return fills[i].Seq < fills[j].Seq
	})
	return fills, nil
}

// priority level within one side at the clearing price.
type level struct {
	orders []Order
	total  share.Units
}

// allocateSide walks one side's orders in price-time priority, filling whole
// levels until one cannot be filled whole, then pro-rating within that level.
//
// The levels are: market orders, then limits strictly better than the clearing
// price ordered by aggressiveness, then limits exactly at it. A naive version
// that fills "market orders, then better limits, then pro-rates the rest" has
// no answer when the market orders alone exceed the executable volume — a
// thousand market buys against a hundred limit sells — and that is not an edge
// case, it is what a retail order flow looks like against a thin book.
func allocateSide(book []Order, side Side, p money.Kobo, exec share.Units, lot share.Units) ([]Fill, error) {
	type sized struct {
		o Order
		q share.Units
	}
	var live []sized
	for _, o := range book {
		if o.Side != side || !o.Executable(p) {
			continue
		}
		if q := o.QtyAt(p, lot); q > 0 {
			live = append(live, sized{o, q})
		}
	}
	if len(live) == 0 {
		if exec != 0 {
			return nil, fmt.Errorf("exchange: %s side has no executable orders but %s to fill", side, exec)
		}
		return nil, nil
	}

	// Price-time priority. Market orders first — they accepted any price, so
	// nothing outranks them. Then limits by aggressiveness: a buyer willing to
	// pay more, or a seller willing to take less, is served first. Ties by
	// sequence, which is a total order by construction.
	sort.Slice(live, func(i, j int) bool {
		a, b := live[i].o, live[j].o
		if a.IsMarket() != b.IsMarket() {
			return a.IsMarket()
		}
		if !a.IsMarket() && a.Limit != b.Limit {
			if side == Buy {
				return a.Limit > b.Limit
			}
			return a.Limit < b.Limit
		}
		return a.Seq < b.Seq
	})

	// Group into levels of equal priority. Orders within a level are
	// indistinguishable on price, so when a level cannot be filled whole its
	// members share what is left pro-rata rather than by arrival.
	var levels []level
	for i := 0; i < len(live); {
		j := i
		var lv level
		for ; j < len(live); j++ {
			same := live[i].o.IsMarket() == live[j].o.IsMarket() &&
				(live[i].o.IsMarket() || live[i].o.Limit == live[j].o.Limit)
			if !same {
				break
			}
			lv.orders = append(lv.orders, live[j].o)
			lv.total += live[j].q
		}
		levels = append(levels, lv)
		i = j
	}

	fills := make([]Fill, 0, len(live))
	remaining := exec
	for _, lv := range levels {
		if remaining <= 0 {
			break
		}
		if remaining >= lv.total {
			for _, o := range lv.orders {
				fills = append(fills, Fill{Seq: o.Seq, Side: side, Units: o.QtyAt(p, lot)})
			}
			remaining -= lv.total
			continue
		}

		// The marginal level. Pro-rate by quantity with largest remainder so
		// the parts sum to exactly what is left, then stop: nothing below the
		// marginal price level fills at all.
		weights := make([]int64, len(lv.orders))
		for i, o := range lv.orders {
			weights[i] = int64(o.QtyAt(p, lot))
		}
		toShare := remaining
		if lot > 1 {
			// Pro-rate in whole lots, then scale back, so no allocation is a
			// fraction of a tradable unit.
			for i := range weights {
				weights[i] /= int64(lot)
			}
			toShare /= lot
		}
		parts, err := alloc.Proportional(toShare, weights)
		if err != nil {
			return nil, fmt.Errorf("exchange: rationing the marginal level: %w", err)
		}
		for i, o := range lv.orders {
			u := parts[i]
			if lot > 1 {
				u *= lot
			}
			if u == 0 {
				continue
			}
			fills = append(fills, Fill{Seq: o.Seq, Side: side, Units: u})
		}
		remaining = 0
		break
	}

	if remaining != 0 {
		return nil, fmt.Errorf(
			"exchange: %s side allocated %s of %s at %s — the walk did not exhaust the executable volume",
			side, exec-remaining, exec, p)
	}
	return fills, nil
}
