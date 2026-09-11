// Package fee computes what a transaction costs and who receives it.
//
// This is the buyback's funding source, which is why it lives at the
// foundation rather than in the clearing service: the whole product thesis is
// that a slice of this split becomes equity, and a split that does not
// reconcile to the kobo is a product that cannot be audited.
//
// # Shape of a charge
//
// A tap generates one merchant service charge and, optionally, one merchant
// co-funding contribution:
//
//	MSC       what the merchant pays the network. Distributed among the issuer
//	          (interchange), the scheme, and the buyback pool.
//	co-fund   an opt-in amount the merchant pays on top, all of which buys their
//	          own shares for the cardholder. Kept out of the MSC distribution
//	          entirely: folding it in would silently inflate interchange, and a
//	          member bank's revenue must not move because a merchant ran a
//	          loyalty campaign.
//
// # Versioning
//
// A schedule is pinned on the authorisation and clearing uses that version, not
// whichever is live at clearing time. A mid-day rate change would otherwise make
// T+1 clearing disagree with the quote the merchant was shown, and the
// reconciliation never balances again. This is the most common fee bug in real
// networks and it is a data-model decision, not a code one.
package fee

import (
	"fmt"

	"freedom/api/internal/alloc"
	"freedom/api/internal/money"
)

// Component names a party's slice of the merchant service charge.
type Component string

const (
	// Interchange goes to the issuer of the card.
	Interchange Component = "interchange"
	// SchemeFee is the network's own revenue.
	SchemeFee Component = "scheme"
	// BuybackPool funds equity purchases for the cardholder.
	BuybackPool Component = "buyback_pool"
	// AcquirerMarkup is the acquirer's margin.
	AcquirerMarkup Component = "acquirer_markup"
)

// Order fixes the component sequence for every deterministic operation in this
// package. Remainder ties break toward the front of this list, so changing it
// changes money: it is part of the schedule's published behaviour.
var Order = []Component{Interchange, SchemeFee, BuybackPool, AcquirerMarkup}

// Rate describes one component's claim on the merchant service charge.
type Rate struct {
	Component Component
	// ShareBps is the component's share of the MSC in basis points. The shares
	// across all components should total 10,000; when they do not, the
	// difference lands on the residual component rather than vanishing.
	ShareBps int64
	// Floor and Cap bound the component in absolute terms. A cap is what makes
	// interchange predictable on large tickets; Cap nil means uncapped.
	Floor money.Kobo
	Cap   *money.Kobo
}

// Schedule is a versioned, dated set of pricing rules.
type Schedule struct {
	Version int
	// Effective bounds, as YYYY-MM-DD. EffectiveTo empty means open-ended.
	EffectiveFrom string
	EffectiveTo   string

	// The merchant service charge itself.
	MSCRateBps int64
	MSCFlat    money.Kobo
	MSCFloor   money.Kobo
	MSCCap     *money.Kobo

	// How the MSC is divided.
	Rates []Rate

	// Residual receives whatever a cap took away from another component, and
	// absorbs the rounding remainder. Naming it is mandatory: a capped
	// component's overflow has to go somewhere, and leaving it implicit means
	// each reader assumes a different somewhere.
	Residual Component
}

// Breakdown is the computed result. Components always sums to MSC exactly.
type Breakdown struct {
	ScheduleVersion int
	Ticket          money.Kobo
	MSC             money.Kobo
	Components      map[Component]money.Kobo
	// CoFund is the merchant's opt-in contribution, entirely additional to MSC.
	CoFund money.Kobo
	// BuybackFunding is what the buyback engine actually receives: the pool
	// component plus any co-funding. This is the number the product promises.
	BuybackFunding money.Kobo
}

// MerchantPays is the merchant's total cost of accepting the transaction.
func (b Breakdown) MerchantPays() money.Kobo { return b.MSC + b.CoFund }

// NetToMerchant is what the merchant is owed for the sale.
func (b Breakdown) NetToMerchant() money.Kobo { return b.Ticket - b.MerchantPays() }

// Input is the transaction being priced.
type Input struct {
	Ticket money.Kobo
	// CoFundBps is the merchant's opt-in equity rebate, in basis points of the
	// ticket. Zero for a merchant who has not opted in.
	CoFundBps int64
}

// Compute prices a transaction under a schedule.
//
// The order of operations is the part that matters, and it is the order most
// implementations get wrong:
//
//  1. compute the MSC, applying its floor and cap
//  2. give each component its share of that MSC, floored
//  3. apply each component's own floor and cap
//  4. re-derive the residual so the parts sum to the MSC again
//
// Applying caps after distributing the remainder — the natural way to write it —
// breaks the sum, because a cap can only ever take money away and nothing puts
// it back.
func Compute(s Schedule, in Input) (Breakdown, error) {
	if in.Ticket < 0 {
		return Breakdown{}, fmt.Errorf("fee: ticket must not be negative, got %s", in.Ticket)
	}
	if s.Residual == "" {
		return Breakdown{}, fmt.Errorf("fee: schedule v%d names no residual component", s.Version)
	}
	if err := s.validate(); err != nil {
		return Breakdown{}, err
	}

	// A reversal is priced by negating the forward charge rather than by
	// recomputing on a negative ticket, so that reversing always returns every
	// account to the balance it had. See Negate.
	msc := money.Clamp(money.RateOf(in.Ticket, s.MSCRateBps)+s.MSCFlat, s.MSCFloor, s.MSCCap)
	if msc > in.Ticket {
		// The charge can never exceed the sale; a flat fee on a tiny ticket
		// would otherwise make the merchant pay to be paid.
		msc = in.Ticket
	}

	// Step 2: distribute by share, using largest remainder so the parts sum to
	// the MSC before caps are considered.
	weights := make([]int64, len(Order))
	byComponent := map[Component]Rate{}
	for _, r := range s.Rates {
		byComponent[r.Component] = r
	}
	for i, c := range Order {
		weights[i] = byComponent[c].ShareBps
	}
	parts, err := alloc.Proportional(msc, weights)
	if err != nil {
		return Breakdown{}, fmt.Errorf("fee: distributing %s under schedule v%d: %w", msc, s.Version, err)
	}

	// Step 3: caps and floors, tracking what they moved.
	out := map[Component]money.Kobo{}
	for i, c := range Order {
		r := byComponent[c]
		if r.Component == "" && parts[i] == 0 {
			continue // component not present in this schedule
		}
		out[c] = money.Clamp(parts[i], r.Floor, r.Cap)
	}

	// Step 4: the residual absorbs both the cap overflow and the rounding.
	var sum money.Kobo
	for _, v := range out {
		sum += v
	}
	if drift := msc - sum; drift != 0 {
		out[s.Residual] += drift
		// A residual driven negative means the schedule's floors alone exceed
		// the MSC. That is an unpriceable schedule, not a transaction to post.
		if out[s.Residual] < 0 {
			return Breakdown{}, fmt.Errorf(
				"fee: schedule v%d floors exceed the %s MSC on a %s ticket; %s would owe %s",
				s.Version, msc, in.Ticket, s.Residual, out[s.Residual])
		}
	}

	coFund := money.RateOf(in.Ticket, in.CoFundBps)

	return Breakdown{
		ScheduleVersion: s.Version,
		Ticket:          in.Ticket,
		MSC:             msc,
		Components:      out,
		CoFund:          coFund,
		BuybackFunding:  out[BuybackPool] + coFund,
	}, nil
}

// Negate returns the exact inverse of a breakdown, for a reversal or a refund.
//
// It negates rather than recomputes on purpose. Recomputing a refund from the
// refunded amount leaves fee dust behind on every partial refund — the parts of
// a split of X and a split of Y never sum to the split of X+Y — and that dust
// accumulates in accounts nobody owns until someone reconciles it by hand.
func (b Breakdown) Negate() Breakdown {
	out := make(map[Component]money.Kobo, len(b.Components))
	for c, v := range b.Components {
		out[c] = -v
	}
	return Breakdown{
		ScheduleVersion: b.ScheduleVersion,
		Ticket:          -b.Ticket,
		MSC:             -b.MSC,
		Components:      out,
		CoFund:          -b.CoFund,
		BuybackFunding:  -b.BuybackFunding,
	}
}

// Prorate scales a breakdown down to a partial amount.
//
// It returns only the part. The complement must be obtained with Split or by
// subtraction, never by calling Prorate again on the remaining amount: two
// independent prorations of the same breakdown can each round the same kobo
// their own way, and then the parts sum to one kobo more than the whole. That
// is not a hypothetical — it is what the reconstruction test catches.
func (b Breakdown) Prorate(part money.Kobo) (Breakdown, error) {
	if b.Ticket == 0 {
		return Breakdown{}, fmt.Errorf("fee: cannot prorate a zero-ticket breakdown")
	}
	if part < 0 || part > b.Ticket {
		return Breakdown{}, fmt.Errorf("fee: partial %s is outside the %s ticket", part, b.Ticket)
	}

	// Each component is split between the part and its complement by the same
	// largest-remainder rule used everywhere else, and the part takes index 0.
	scale := func(v money.Kobo) (money.Kobo, error) {
		parts, err := alloc.Proportional(v, []int64{int64(part), int64(b.Ticket - part)})
		if err != nil {
			return 0, err
		}
		return parts[0], nil
	}

	out := make(map[Component]money.Kobo, len(b.Components))
	var msc money.Kobo
	for _, c := range Order {
		v, ok := b.Components[c]
		if !ok {
			continue
		}
		scaled, err := scale(v)
		if err != nil {
			return Breakdown{}, fmt.Errorf("fee: prorating %s: %w", c, err)
		}
		out[c] = scaled
		msc += scaled
	}
	// MSC is the sum of the scaled components rather than a separately scaled
	// figure, so the breakdown is internally consistent by construction instead
	// of by a drift adjustment.
	coFund, err := scale(b.CoFund)
	if err != nil {
		return Breakdown{}, fmt.Errorf("fee: prorating co-funding: %w", err)
	}

	return Breakdown{
		ScheduleVersion: b.ScheduleVersion,
		Ticket:          part,
		MSC:             msc,
		Components:      out,
		CoFund:          coFund,
		BuybackFunding:  out[BuybackPool] + coFund,
	}, nil
}

// Split divides a breakdown into a part and the remainder.
//
// The remainder is derived by subtraction, which is what makes the two
// reconstruct the original exactly no matter how the rounding fell. This is the
// method clearing uses to apply a partial reversal against an outstanding
// authorisation.
func (b Breakdown) Split(part money.Kobo) (taken, left Breakdown, err error) {
	taken, err = b.Prorate(part)
	if err != nil {
		return Breakdown{}, Breakdown{}, err
	}
	return taken, b.Sub(taken), nil
}

// Sub subtracts one breakdown from another, component by component.
func (b Breakdown) Sub(o Breakdown) Breakdown {
	out := make(map[Component]money.Kobo, len(b.Components))
	for _, c := range Order {
		if v, ok := b.Components[c]; ok {
			out[c] = v - o.Components[c]
		}
	}
	return Breakdown{
		ScheduleVersion: b.ScheduleVersion,
		Ticket:          b.Ticket - o.Ticket,
		MSC:             b.MSC - o.MSC,
		Components:      out,
		CoFund:          b.CoFund - o.CoFund,
		BuybackFunding:  b.BuybackFunding - o.BuybackFunding,
	}
}

func (s Schedule) validate() error {
	seen := map[Component]bool{}
	for _, r := range s.Rates {
		if seen[r.Component] {
			return fmt.Errorf("fee: schedule v%d lists %s twice", s.Version, r.Component)
		}
		seen[r.Component] = true
		if r.ShareBps < 0 {
			return fmt.Errorf("fee: schedule v%d gives %s a negative share", s.Version, r.Component)
		}
		if r.Cap != nil && *r.Cap < 0 {
			return fmt.Errorf("fee: schedule v%d caps %s below zero", s.Version, r.Component)
		}
		if !validComponent(r.Component) {
			return fmt.Errorf("fee: schedule v%d names unknown component %q", s.Version, r.Component)
		}
	}
	if !seen[s.Residual] {
		return fmt.Errorf("fee: schedule v%d names %s as residual but does not price it",
			s.Version, s.Residual)
	}
	return nil
}

func validComponent(c Component) bool {
	for _, k := range Order {
		if k == c {
			return true
		}
	}
	return false
}

// Components returns the breakdown's components in Order, for stable rendering
// and stable ledger entry ordering.
func (b Breakdown) Sorted() []struct {
	Component Component
	Amount    money.Kobo
} {
	out := make([]struct {
		Component Component
		Amount    money.Kobo
	}, 0, len(b.Components))
	for _, c := range Order {
		if v, ok := b.Components[c]; ok {
			out = append(out, struct {
				Component Component
				Amount    money.Kobo
			}{c, v})
		}
	}
	return out
}
