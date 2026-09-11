// Package exchange is Freedom's private stock market.
//
// It exists because the card cannot keep its promise without it: almost no
// Nigerian SME retailer is listed anywhere, so there is no market in which to
// buy the shares every tap is supposed to earn. This package is that market.
//
// # Why a call auction first
//
// A continuous order book on an illiquid name does not discover a price, it
// manufactures one. A single 10-unit trade at a silly level becomes the last
// print, and every valuation, every buyback, and every holder's statement
// inherits it until someone trades again — which on an SME symbol might be
// next week.
//
// So every symbol begins auction-only: orders accumulate through the session
// and cross once, at the single price that maximises executable volume. That
// price is the product of everyone who wanted to trade that day rather than of
// whoever traded last. A symbol earns a continuous book only after it clears a
// liquidity threshold, and even then the auction sets the daily reference.
package exchange

import (
	"fmt"

	"github.com/google/uuid"

	"freedom/api/internal/money"
	"freedom/api/internal/share"
)

// Side of an order.
type Side string

const (
	Buy  Side = "buy"
	Sell Side = "sell"
)

// Opposite returns the other side.
func (s Side) Opposite() Side {
	if s == Buy {
		return Sell
	}
	return Buy
}

// Order type.
const (
	TypeLimit  = "limit"
	TypeMarket = "market"
)

// Venue an order was entered into.
const (
	VenueAuction = "auction"
	VenueCLOB    = "clob"
)

// Order is one participant's intention, as the matching engine sees it.
//
// It carries no pointers and no maps: the matcher is a pure function over a
// slice of these, which is what makes an auction replayable byte-for-byte from
// its recorded orders. A session that cannot be replayed cannot be audited, and
// an exchange that cannot be audited is not one.
type Order struct {
	ID   uuid.UUID
	Side Side
	Type string

	// Limit is the worst price the order will accept. Zero for a market order,
	// which accepts whatever the uncross produces.
	Limit money.Kobo

	// Exactly one of Qty and Notional is set. Qty sizes the order in share
	// units; Notional sizes it in naira (see QtyAt).
	Qty      share.Units
	Notional money.Kobo

	// Seq is time priority. A monotonic sequence rather than a timestamp,
	// because two orders can share a microsecond and priority must be total.
	Seq int64

	AccountID uuid.UUID

	// OwnerKey groups accounts that must not trade with each other — a listed
	// company and the accounts on its own cap table. Self-trade prevention uses
	// it to keep a wash trade from setting the price the buyback then pays.
	// Empty means the order is unrelated to anyone.
	OwnerKey string
}

// IsMarket reports whether the order accepts any price.
func (o Order) IsMarket() bool { return o.Type == TypeMarket }

// Executable reports whether the order would trade at price p.
//
// A market order always executes. A limit buy executes at or below its limit; a
// limit sell at or above. This single predicate defines the demand and supply
// curves the uncross searches over.
func (o Order) Executable(p money.Kobo) bool {
	if o.IsMarket() {
		return true
	}
	if o.Side == Buy {
		return o.Limit >= p
	}
	return o.Limit <= p
}

// Rules are an instrument's trading constraints.
type Rules struct {
	// Tick is the minimum price increment. Limits off-tick are rejected at
	// entry rather than silently rounded — rounding someone's order is deciding
	// what they meant.
	Tick money.Kobo
	// Lot is the minimum tradable quantity and the granularity above it.
	Lot share.Units
	// MinPrice and MaxPrice bound an order's limit. Zero MaxPrice means no
	// ceiling.
	MinPrice money.Kobo
	MaxPrice money.Kobo
}

// DefaultRules are what a new listing starts with: kobo ticks and no lot
// constraint, because buyback allocations are fractions of a share by design.
func DefaultRules() Rules {
	return Rules{Tick: 1, Lot: 1, MinPrice: 1}
}

// Validate checks an order against the instrument's rules.
func (r Rules) Validate(o Order) error {
	switch {
	case o.Qty > 0 && o.Notional > 0:
		return fmt.Errorf("exchange: an order is sized in units or in naira, never both")
	case o.Qty < 0 || o.Notional < 0:
		return fmt.Errorf("exchange: negative size")
	case o.Qty == 0 && o.Notional == 0:
		return fmt.Errorf("exchange: an order needs a quantity or a notional")
	case o.Notional > 0 && o.Side == Sell:
		// Selling "₦5,000 worth" is ambiguous until the price is known, and the
		// units would have to be reserved before then. Sells are unit-sized.
		return fmt.Errorf("exchange: a sell order must be sized in units, not naira")
	}
	if o.Qty > 0 && r.Lot > 1 && int64(o.Qty)%int64(r.Lot) != 0 {
		return fmt.Errorf("exchange: quantity %s is not a multiple of the %s lot", o.Qty, r.Lot)
	}
	if o.Side != Buy && o.Side != Sell {
		return fmt.Errorf("exchange: unknown side %q", o.Side)
	}

	switch o.Type {
	case TypeMarket:
		if o.Limit != 0 {
			return fmt.Errorf("exchange: a market order must not carry a limit (%s)", o.Limit)
		}
	case TypeLimit:
		if o.Limit <= 0 {
			return fmt.Errorf("exchange: a limit order needs a positive price, got %s", o.Limit)
		}
		if r.Tick > 1 && int64(o.Limit)%int64(r.Tick) != 0 {
			return fmt.Errorf("exchange: price %s is not on the %s tick", o.Limit, r.Tick)
		}
		if r.MinPrice > 0 && o.Limit < r.MinPrice {
			return fmt.Errorf("exchange: price %s is below the %s minimum", o.Limit, r.MinPrice)
		}
		if r.MaxPrice > 0 && o.Limit > r.MaxPrice {
			return fmt.Errorf("exchange: price %s is above the %s maximum", o.Limit, r.MaxPrice)
		}
	default:
		return fmt.Errorf("exchange: unknown order type %q", o.Type)
	}
	return nil
}

// Notional support.
//
// A market buy cannot be reserved, because it has no upper bound on what it
// will spend. Rather than offer an order type whose risk cannot be computed,
// Freedom offers the cash-denominated buy — "spend ₦5,000 on MAMAPUT" — whose
// reservation is exactly the notional plus the maximum fee. It is also the
// shape the product already speaks: a ₦5 tap buys 0.125 shares, not a share
// count.
//
// Crucially, floor(Notional / p) is non-increasing in p, so demand stays
// monotone and the uncross's quasi-concavity argument survives intact.

// QtyAt returns the order's quantity at price p, floored to the lot.
//
// For a unit-denominated order this is just its quantity. For a cash-denominated
// one it is what the notional buys at p.
func (o Order) QtyAt(p money.Kobo, lot share.Units) share.Units {
	q := o.Qty
	if o.Notional > 0 {
		if p <= 0 {
			return 0
		}
		u, _, _, err := share.UnitsFor(o.Notional, p)
		if err != nil {
			return 0
		}
		q = u
	}
	if lot > 1 {
		q = q - q%lot
	}
	if q < 0 {
		return 0
	}
	return q
}

// IsCashDenominated reports whether the order is sized in naira.
func (o Order) IsCashDenominated() bool { return o.Notional > 0 }
