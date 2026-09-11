package exchange

import (
	"fmt"
	"sort"

	"freedom/api/internal/money"
	"freedom/api/internal/share"
)

// Continuous matching.
//
// A symbol earns this. Auction-only is the default because a continuous book on
// an illiquid name does not discover a price, it manufactures one: a single
// small trade becomes the last print, and every valuation, buyback and holder
// statement inherits it until somebody trades again — which on an SME symbol
// might be next week.
//
// Once a symbol graduates, the auction does not go away. The CLOB runs through
// the day and a CLOSING auction still sets the daily reference, because the
// last continuous print is exactly the thing one participant can move for
// pocket change at 15:59.

// Resting is an order sitting on the book.
type Resting struct {
	Order
	Remaining share.Units
}

// Book is one instrument's resting orders, kept in price-time priority.
type Book struct {
	Bids []Resting // descending price, then ascending seq
	Asks []Resting // ascending price, then ascending seq
}

// Match applies an incoming order against a book and returns the trades.
//
// Three rules that implementations routinely get wrong, all of them here:
//
//   - The trade prints at the RESTING order's price, never the aggressor's. A
//     taker's limit is a worst-case bound, not a price. Getting this backwards
//     silently transfers the spread to whoever crossed.
//   - Price beats time, and time is a total order. Two orders can share a
//     microsecond, so priority is a sequence number rather than a timestamp.
//   - The aggressor never trades through its own limit, however deep the book.
func Match(b *Book, in Order, rules Rules) ([]Trade, Resting, error) {
	if err := rules.Validate(in); err != nil {
		return nil, Resting{}, err
	}
	if in.Notional > 0 {
		return nil, Resting{}, fmt.Errorf("exchange: cash-denominated orders are auction-only")
	}

	rest := Resting{Order: in, Remaining: in.Qty}
	opposite := &b.Asks
	if in.Side == Sell {
		opposite = &b.Bids
	}

	var trades []Trade
	for rest.Remaining > 0 && len(*opposite) > 0 {
		top := (*opposite)[0]
		if !crosses(in, top) {
			break
		}
		qty := rest.Remaining
		if top.Remaining < qty {
			qty = top.Remaining
		}
		if rules.Lot > 1 {
			qty -= qty % rules.Lot
			if qty == 0 {
				break
			}
		}

		trades = append(trades, Trade{
			Price:        top.Limit, // the resting order's price, always
			Units:        qty,
			TakerSeq:     in.Seq,
			MakerSeq:     top.Seq,
			TakerSide:    in.Side,
			MakerOrderID: top.ID,
			TakerOrderID: in.ID,
		})

		rest.Remaining -= qty
		top.Remaining -= qty
		if top.Remaining == 0 {
			*opposite = (*opposite)[1:]
		} else {
			(*opposite)[0] = top
		}
	}
	return trades, rest, nil
}

// Trade is one continuous match.
type Trade struct {
	Price                      money.Kobo
	Units                      share.Units
	TakerSeq, MakerSeq         int64
	TakerSide                  Side
	MakerOrderID, TakerOrderID interface{ String() string }
}

// crosses reports whether an incoming order can trade with a resting one.
func crosses(in Order, top Resting) bool {
	// A resting market order is not a thing: anything unfilled at entry either
	// rests with a limit or is cancelled, so every resting order has a price.
	if in.IsMarket() {
		return true
	}
	if in.Side == Buy {
		return in.Limit >= top.Limit
	}
	return in.Limit <= top.Limit
}

// Insert places a resting order in price-time priority.
func (b *Book) Insert(r Resting) {
	if r.Remaining <= 0 || r.IsMarket() {
		return // IOC/market remainders never rest
	}
	side := &b.Asks
	if r.Side == Buy {
		side = &b.Bids
	}
	*side = append(*side, r)
	sortSide(*side, r.Side)
}

func sortSide(s []Resting, side Side) {
	sort.SliceStable(s, func(i, j int) bool {
		if s[i].Limit != s[j].Limit {
			if side == Buy {
				return s[i].Limit > s[j].Limit // best bid is the highest
			}
			return s[i].Limit < s[j].Limit // best ask is the lowest
		}
		return s[i].Seq < s[j].Seq
	})
}

// Cancel removes an order by sequence.
func (b *Book) Cancel(seq int64) bool {
	for _, side := range []*[]Resting{&b.Bids, &b.Asks} {
		for i, r := range *side {
			if r.Seq == seq {
				*side = append((*side)[:i], (*side)[i+1:]...)
				return true
			}
		}
	}
	return false
}

// Top returns the best bid and offer. A side with no orders reports zero, which
// callers must read as "no market", not as a price of nothing.
func (b *Book) Top() (bid, ask money.Kobo) {
	if len(b.Bids) > 0 {
		bid = b.Bids[0].Limit
	}
	if len(b.Asks) > 0 {
		ask = b.Asks[0].Limit
	}
	return bid, ask
}

// Depth totals resting quantity at or better than a price.
func (b *Book) Depth(side Side, price money.Kobo) share.Units {
	var total share.Units
	src := b.Bids
	if side == Sell {
		src = b.Asks
	}
	for _, r := range src {
		if side == Buy && r.Limit >= price {
			total += r.Remaining
		}
		if side == Sell && r.Limit <= price {
			total += r.Remaining
		}
	}
	return total
}
