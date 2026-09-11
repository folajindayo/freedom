package exchange

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"freedom/api/internal/money"
	"freedom/api/internal/share"
)

// The certification harness.
//
// Freedom is a venue, which means other people integrate against it. A member
// firm that has never been made to handle a rejection will discover its first
// one in production, on a real customer's order — so before a firm goes live it
// runs this scripted conversation and has to get every answer right.
//
// The cases are chosen for what they teach rather than for coverage: each one
// is a refusal a live member WILL meet, and each has a distinct code they must
// handle differently. A firm that treats every non-200 the same passes none of
// them.

// CertCase is one scripted exchange between a candidate member and the venue.
type CertCase struct {
	Name string
	// Why this case exists — shown in the report, because a firm that
	// understands the rule fixes its handling rather than its retry loop.
	Teaches string
	// Expect names the refusal code the member must handle, or "accepted".
	Expect string
}

// CertificationSuite is what a member must pass before going live.
func CertificationSuite() []CertCase {
	return []CertCase{
		{"place a valid limit order", "the happy path, and the order id to reconcile against", "accepted"},
		{"resend the same client order id", "a retry is not a second order; the venue returns the original", "accepted"},
		{"sell more than the account holds", "position checks are the venue's, not yours", "insufficient_shares"},
		{"sell shares still inside their chargeback lock", "owned is not the same as sellable", "insufficient_shares"},
		{"buy with insufficient cash", "orders are covered at entry; there is no credit", "insufficient_funds"},
		{"order below the minimum notional", "dust is refused, not rounded", "below_minimum"},
		{"order far above the maximum size", "the venue catches decimal points before they trade", "order_too_large"},
		{"limit price off the tick", "prices are refused, never silently rounded to a tick", "rejected"},
		{"limit price outside the band", "the band is published; price inside it", "rejected"},
		{"trade a halted instrument", "a halt means the venue does not know the price", "instrument_halted"},
		{"trade on a market holiday", "the calendar is published ahead; do not guess it", "market_closed"},
		{"trade while the firm is halted", "the kill switch stops order entry immediately", "member_halted"},
		{"exceed the order-to-trade ratio", "unfilled order storms are throttled", "throttled"},
		{"cancel a live order", "cancels release the reservation; confirm before assuming", "accepted"},
		{"cancel the same order twice", "a second cancel is a conflict, not a success", "not_cancellable"},
		{"cancel another member's order", "other members' orders read as absent, never as forbidden", "not_found"},
		{"send with a bad credential", "authentication failures say nothing about why", "unauthenticated"},
	}
}

// CertificationRun is a candidate's attempt.
type CertificationRun struct {
	MemberCode string
	Results    []CertResult
}

// CertResult is one case's outcome.
type CertResult struct {
	Case   CertCase
	Got    string
	Passed bool
	Detail string
}

// Passed reports whether every case was handled correctly.
func (r CertificationRun) Passed() bool {
	for _, x := range r.Results {
		if !x.Passed {
			return false
		}
	}
	return len(r.Results) > 0
}

// Report renders the run for the candidate, naming what each failure teaches
// rather than only that it failed.
func (r CertificationRun) Report() string {
	out := fmt.Sprintf("Certification: %s\n", r.MemberCode)
	var failed int
	for _, x := range r.Results {
		mark := "pass"
		if !x.Passed {
			mark, failed = "FAIL", failed+1
		}
		out += fmt.Sprintf("  [%s] %-42s expected %-20s got %s\n",
			mark, x.Case.Name, x.Case.Expect, x.Got)
		if !x.Passed {
			out += fmt.Sprintf("         %s\n", x.Case.Teaches)
		}
	}
	if failed == 0 {
		out += fmt.Sprintf("  %d of %d — certified\n", len(r.Results), len(r.Results))
	} else {
		out += fmt.Sprintf("  %d of %d failed — not certified\n", failed, len(r.Results))
	}
	return out
}

// SandboxInstrument provisions a symbol that exists only for certification.
//
// Candidates must not certify against a live symbol: their deliberate failures
// would land in real surveillance, and their successful orders would land in a
// real price.
func SandboxInstrument(ctx context.Context, tx pgx.Tx, companyID any, symbol string) (string, error) {
	assetID := "EQ:" + symbol
	if _, err := tx.Exec(ctx, `
		INSERT INTO assets (id, class, scale, label) VALUES ($1,'equity',8,$2)
		ON CONFLICT (id) DO NOTHING`, assetID, symbol); err != nil {
		return "", fmt.Errorf("exchange: sandbox asset: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO instruments (id, symbol, company_id, shares_authorised_units,
		                         reference_price_kobo, status, listed_at,
		                         min_order_notional_kobo, max_order_units)
		VALUES ($1,$2,$3,$4,$5,'listed',now(),$6,$7)
		ON CONFLICT (id) DO NOTHING`,
		assetID, symbol, companyID, int64(share.Whole(1_000_000)),
		int64(money.Naira(40)), int64(money.Naira(100)), int64(share.Whole(1_000))); err != nil {
		return "", fmt.Errorf("exchange: sandbox instrument: %w", err)
	}
	return assetID, nil
}
