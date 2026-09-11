package exchange

import (
	"freedom/api/internal/money"
)

// Fees charged on an exchange fill.
//
// Both sides pay. The fee is a separate ledger entry and never folded into the
// price: a fee inside the price would corrupt price_observations, the buyback's
// trailing-VWAP cap, and every holder's cost basis at the same time, and none
// of the three would be recoverable afterwards.
type FeeSchedule struct {
	Version int
	// TradeBps is charged on consideration, to each side.
	TradeBps int64
	// MinFee stops a dust trade from costing more to process than it moves.
	MinFee money.Kobo
	// MaxFee caps the charge on a large fill.
	MaxFee *money.Kobo
}

// ExchangeFeesV1 is the launch schedule: 25bp a side, floored at ₦5, capped at
// ₦2,500.
func ExchangeFeesV1() FeeSchedule {
	cap := money.Naira(2_500)
	return FeeSchedule{Version: 1, TradeBps: 25, MinFee: money.Naira(5), MaxFee: &cap}
}

// Fee computes the charge on a consideration.
//
// It rounds UP: the fee is money the exchange collects, and the house is never
// worse off for a rounding direction it did not choose deliberately.
func (f FeeSchedule) Fee(consideration money.Kobo) money.Kobo {
	if consideration <= 0 {
		return 0
	}
	return money.Clamp(money.RateOfUp(consideration, f.TradeBps), f.MinFee, f.MaxFee)
}

// FeeOnSale is the fee a seller pays, never more than their proceeds.
//
// Without the clamp a sale small enough that the minimum fee exceeds its
// proceeds would drive the seller's cash negative — which the ledger would
// permit, since an account may go negative, and which the seller would
// experience as being charged to sell something.
func (f FeeSchedule) FeeOnSale(proceeds money.Kobo) money.Kobo {
	fee := f.Fee(proceeds)
	if fee > proceeds {
		return proceeds
	}
	return fee
}

// MaxFeeFor is the most a buy of this notional could be charged, used to size a
// reservation. A reservation that is short by a kobo is a fill that cannot
// settle.
func (f FeeSchedule) MaxFeeFor(notional money.Kobo) money.Kobo {
	return f.Fee(notional)
}
