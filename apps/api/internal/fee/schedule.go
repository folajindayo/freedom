package fee

import "freedom/api/internal/money"

// SchemeV1 is Freedom's launch pricing.
//
// The merchant service charge follows the CBN-style card-present shape: 0.50%
// of the ticket, capped. Verify the current cap against the CBN Guide to
// Charges before go-live — it is a regulated figure, not a commercial choice,
// and it moves.
//
// The cap is why buyback value is sublinear in ticket size. A ₦10,000 tap
// yields ₦50 of MSC and ₦5 of equity; a ₦1,000,000 tap yields ₦1,000 of MSC —
// not ₦5,000 — and so ₦100 of equity, a hundredth of the rate. Merchant
// co-funding, which is a share of the ticket rather than of the capped MSC, is
// the only lever that makes large baskets worth owning.
func SchemeV1() Schedule {
	mscCap := money.Naira(1_000)
	interchangeCap := money.Naira(600)

	return Schedule{
		Version:       1,
		EffectiveFrom: "2026-01-01",
		MSCRateBps:    50, // 0.50%
		MSCCap:        &mscCap,
		Rates: []Rate{
			// The issuer takes the largest share, as on any real network: it
			// carries the credit risk and funds the float.
			{Component: Interchange, ShareBps: 6_000, Cap: &interchangeCap},
			{Component: SchemeFee, ShareBps: 3_000},
			// The product. Ten percent of the fee pool, by default.
			{Component: BuybackPool, ShareBps: 1_000},
		},
		// The scheme absorbs cap overflow and rounding. It is the only party
		// here that is us: pushing drift onto the issuer or the buyback pool
		// would make someone else's revenue depend on our rounding.
		Residual: SchemeFee,
	}
}
