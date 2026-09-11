// Package buyback turns cleared fee income into equity in the cardholder's name.
//
// This is the product. Everything else on the network exists to make this step
// possible and honest.
//
// # Batching
//
// Ten thousand ₦5 intents must not become ten thousand orders. Intents for one
// instrument in one session aggregate into a single purchase, and the units
// bought are then allocated back pro-rata. That is not only an efficiency
// choice: allocating each intent independently at the batch price leaves
// Σ allocated < bought, and the difference belongs to nobody. Pro-rating the
// actual purchase is the only formulation where the treasury debit exactly
// equals the sum of cardholder credits, which is the only formulation the
// ledger's balance trigger will accept without a plug entry.
//
// # Where the price comes from
//
// The buyback is a price taker. It buys from treasury at the price a call
// auction of genuine orders discovered, and it is never itself an input to that
// auction. If it were, the network's own demand would set the price the network
// pays: the more it spent, the more each share would cost, and fee income would
// drain into company treasuries by construction.
//
// Two further defences bound what a collusive listing can extract by running a
// wash auction against itself:
//
//	trailing band   the realised price is capped at the trailing session VWAP,
//	                so a single manipulated session cannot be cashed in
//	daily release   a hard per-instrument unit cap per session
package buyback

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"oja/api/internal/alloc"
	"oja/api/internal/ledger"
	"oja/api/internal/money"
	"oja/api/internal/scheme"
	"oja/api/internal/share"
)

// Result summarises one session's allocation for one instrument.
type Result struct {
	InstrumentID string
	Intents      int
	Funding      money.Kobo
	Spent        money.Kobo
	Units        share.Units
	Price        money.Kobo
	Residual     money.Kobo
	DeferredDust int
	Capped       bool
}

type intent struct {
	ID           uuid.UUID
	CardholderID uuid.UUID
	Funding      money.Kobo
}

// Engine executes buybacks.
type Engine struct {
	// TrailingBandSessions is how many prior sessions the VWAP cap looks back
	// over. Zero disables the cap, which is only appropriate in tests.
	TrailingBandSessions int
}

// New builds an engine with the launch defences enabled.
func New() *Engine { return &Engine{TrailingBandSessions: 5} }

// RunSession allocates every pending intent for one instrument.
func (e *Engine) RunSession(ctx context.Context, tx pgx.Tx, instrumentID, sessionDate string) (Result, error) {
	res := Result{InstrumentID: instrumentID}

	price, err := e.executionPrice(ctx, tx, instrumentID, sessionDate)
	if err != nil {
		return res, err
	}
	res.Price = price

	intents, total, err := pendingIntents(ctx, tx, instrumentID)
	if err != nil {
		return res, err
	}
	if len(intents) == 0 {
		return res, nil
	}
	res.Intents = len(intents)
	res.Funding = total

	// How many units the whole batch can buy, before the release cap.
	wanted, spent, residual, err := share.UnitsFor(total, price)
	if err != nil {
		return res, err
	}
	if wanted == 0 {
		// The entire batch cannot afford one unit. Leave the intents pending
		// rather than consuming their funding for nothing.
		return res, nil
	}

	units, capped, err := applyReleaseCap(ctx, tx, instrumentID, sessionDate, wanted)
	if err != nil {
		return res, err
	}
	res.Capped = capped
	if units < wanted {
		// Re-derive the cash for the units actually released, so the treasury
		// is paid for exactly what it gave up.
		spent, residual, err = costOf(units, price, total)
		if err != nil {
			return res, err
		}
	}
	res.Units = units
	res.Spent = spent
	res.Residual = residual

	batchID, err := openBatch(ctx, tx, instrumentID, sessionDate, total, units, price)
	if err != nil {
		return res, err
	}

	// Pro-rata by funding, largest remainder. Ties break to the lower index and
	// the intents are ordered by id, so a re-run allocates identically.
	weights := make([]int64, len(intents))
	for i, in := range intents {
		weights[i] = int64(in.Funding)
	}
	perIntent, err := alloc.Proportional(units, weights)
	if err != nil {
		return res, fmt.Errorf("buyback: allocating %s units: %w", units, err)
	}

	companyID, err := companyFor(ctx, tx, instrumentID)
	if err != nil {
		return res, err
	}
	symbol, err := symbolFor(ctx, tx, instrumentID)
	if err != nil {
		return res, err
	}

	lockedUntil, err := scheme.LockedUntil(sessionDate)
	if err != nil {
		return res, err
	}

	for i, in := range intents {
		got := perIntent[i]
		if got == 0 {
			// Funded, but too little to buy a single unit at this price. It is
			// never silently dropped: the funding stays and is retried next
			// session, when either the price or the accumulated funding differs.
			if err := deferDust(ctx, tx, in.ID); err != nil {
				return res, err
			}
			res.DeferredDust++
			continue
		}
		if err := e.allocate(ctx, tx, allocation{
			IntentID:     in.ID,
			BatchID:      batchID,
			CardholderID: in.CardholderID,
			CompanyID:    companyID,
			InstrumentID: instrumentID,
			Symbol:       symbol,
			Units:        got,
			Price:        price,
			Funding:      in.Funding,
			SessionDate:  sessionDate,
			LockedUntil:  lockedUntil,
		}); err != nil {
			return res, err
		}
	}

	if err := markBatchAllocated(ctx, tx, batchID); err != nil {
		return res, err
	}
	return res, nil
}

type allocation struct {
	IntentID     uuid.UUID
	BatchID      uuid.UUID
	CardholderID uuid.UUID
	CompanyID    uuid.UUID
	InstrumentID string
	Symbol       string
	Units        share.Units
	Price        money.Kobo
	Funding      money.Kobo
	SessionDate  string
	LockedUntil  string
}

// allocate posts one cardholder's share of the batch.
//
// Both legs ride one ledger transaction: naira moves from the buyback pool to
// the company's treasury cash, and equity moves from treasury to the
// cardholder's wallet. They balance independently, per asset.
func (e *Engine) allocate(ctx context.Context, tx pgx.Tx, a allocation) error {
	// Equity may only be allocated to a verified cardholder who has accepted
	// the disclosure. Checked here rather than trusted from upstream, because
	// this is the last point before shares exist in someone's name.
	var permitted bool
	if err := tx.QueryRow(ctx,
		`SELECT equity_allocation_permitted($1)`, a.CardholderID).Scan(&permitted); err != nil {
		return fmt.Errorf("buyback: eligibility check: %w", err)
	}
	if !permitted {
		// Hold the funding rather than granting equity to someone who may not
		// hold it. It stays claimable once they complete verification.
		return holdForIneligible(ctx, tx, a.IntentID)
	}

	// The cash the treasury actually receives for these units, rounded up.
	cost, residual, err := costOf(a.Units, a.Price, a.Funding)
	if err != nil {
		return err
	}

	pool, err := ledger.Resolve(ctx, tx, ledger.Scheme(ledger.KindBuybackPool, ledger.AssetNGN))
	if err != nil {
		return err
	}

	treasuryCash, err := ledger.Resolve(ctx, tx,
		ledger.Company(a.CompanyID, ledger.KindTreasuryCash, ledger.AssetNGN))
	if err != nil {
		return err
	}
	treasury, err := ledger.Resolve(ctx, tx,
		ledger.Company(a.CompanyID, ledger.KindTreasury, a.InstrumentID))
	if err != nil {
		return err
	}
	wallet, err := ledger.Resolve(ctx, tx,
		ledger.Cardholder(a.CardholderID, ledger.KindStockWallet, a.InstrumentID))
	if err != nil {
		return err
	}

	entries := []ledger.Entry{
		{AccountID: pool, Amount: ledger.NGN(-cost), Reason: "buyback.funding"},
		{AccountID: treasuryCash, Amount: ledger.NGN(cost), Reason: "buyback.treasury_proceeds"},
		{AccountID: treasury, Amount: ledger.Equity(a.Symbol, -a.Units), Reason: "buyback.treasury_release"},
		{AccountID: wallet, Amount: ledger.Equity(a.Symbol, a.Units), Reason: "buyback.allocation"},
	}

	// The residual is the cardholder's, not the network's. It moves to their
	// own carry-forward account so the next buyback starts from it.
	if residual > 0 {
		carry, err := ledger.Resolve(ctx, tx,
			ledger.Cardholder(a.CardholderID, ledger.KindBuybackResidual, ledger.AssetNGN))
		if err != nil {
			return err
		}
		entries = append(entries,
			ledger.Entry{AccountID: pool, Amount: ledger.NGN(-residual), Reason: "buyback.residual"},
			ledger.Entry{AccountID: carry, Amount: ledger.NGN(residual), Reason: "buyback.residual_carry"})
	}

	txID, err := ledger.Post(ctx, tx, ledger.Tx{
		EventType:      "buyback.allocated",
		BusinessDate:   a.SessionDate,
		IdempotencyKey: "buyback|" + a.IntentID.String(),
		CorrelationID:  &a.IntentID,
		Entries:        entries,
	})
	if err != nil {
		if errors.Is(err, ledger.ErrAlreadyPosted) {
			return nil // a re-run; the allocation already happened
		}
		return err
	}

	return finishIntent(ctx, tx, a, txID, cost, residual)
}

// costOf is the cash a quantity of units consumes out of a funding pot, and
// what is left over. Capped at the funding: rounding up must never spend money
// the batch does not have.
func costOf(u share.Units, price, funding money.Kobo) (spent, residual money.Kobo, err error) {
	spent, err = share.CostOf(u, price)
	if err != nil {
		return 0, 0, err
	}
	if spent > funding {
		spent = funding
	}
	return spent, funding - spent, nil
}
