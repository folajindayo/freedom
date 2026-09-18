package exchange

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"freedom/api/internal/money"
	"freedom/api/internal/share"
)

// Listing admission.
//
// The criteria are in docs/LISTING-RULES.md. This measures a specific company
// against them and records each finding separately rather than collapsing them
// into a yes or a no — a company that fails on two counts needs to know which
// two, and an exchange that says only "rejected" cannot defend the decision
// later.
//
// Accepting the Freedom card does not entitle a business to list. The card
// relationship is what makes a listing useful; it is not what makes it
// appropriate.

// Criteria are the admission standards, held as data so a rulebook change is a
// configuration change rather than a deployment.
type Criteria struct {
	MinTradingMonths int
	MinFreeFloatBps  int64
	MinHolders       int
	RequireAudited   bool
	RequireSponsor   bool
	RequireTreasury  bool
	MinTreasuryUnits share.Units
}

// StandardCriteria is the published standard.
func StandardCriteria() Criteria {
	return Criteria{
		MinTradingMonths: 24,
		MinFreeFloatBps:  1000, // 10%
		MinHolders:       25,
		RequireAudited:   true,
		RequireSponsor:   true,
		RequireTreasury:  true,
		MinTreasuryUnits: share.Whole(1_000),
	}
}

// Finding is one criterion's verdict.
type Finding struct {
	Criterion string `json:"criterion"`
	Met       bool   `json:"met"`
	Detail    string `json:"detail"`
}

// Application is a company asking to list.
type Application struct {
	ID             uuid.UUID
	CompanyID      uuid.UUID
	MerchantID     *uuid.UUID
	ProposedSymbol string
	SponsorMember  *uuid.UUID
	State          string
	Findings       []Finding
}

// Passes reports whether every criterion was met.
func (a Application) Passes() bool {
	for _, f := range a.Findings {
		if !f.Met {
			return false
		}
	}
	return len(a.Findings) > 0
}

// Apply lodges a listing application.
func Apply(ctx context.Context, tx pgx.Tx, companyID uuid.UUID, symbol string,
	merchant, sponsor *uuid.UUID) (uuid.UUID, error) {

	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if !validSymbol(symbol) {
		return uuid.Nil, fmt.Errorf("exchange: %q is not a valid symbol", symbol)
	}
	var taken bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM instruments WHERE symbol = $1)`, symbol).Scan(&taken); err != nil {
		return uuid.Nil, err
	}
	if taken {
		return uuid.Nil, fmt.Errorf("exchange: %s is already listed", symbol)
	}

	var id uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO listing_applications (company_id, merchant_id, proposed_symbol, sponsor_member)
		VALUES ($1,$2,$3,$4) RETURNING id`, companyID, merchant, symbol, sponsor).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("exchange: lodge application: %w", err)
	}
	return id, nil
}

func validSymbol(s string) bool {
	if len(s) < 3 || len(s) > 12 {
		return false
	}
	if s[0] < 'A' || s[0] > 'Z' {
		return false
	}
	for _, r := range s {
		if !(r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// Evidence is what the applicant has supplied, gathered outside the system by
// the listings team and entered here.
type Evidence struct {
	TradingMonths   int
	AuditedAccounts bool
	AuditorOnList   bool
	SharesInIssue   share.Units
	PublicShares    share.Units
	Holders         int
	TreasuryUnits   share.Units
	BoardResolution bool
	DirectorsClear  bool
}

// Assess measures an application against the criteria and records each finding.
//
// It also reopens a rejected application: new evidence puts it back into
// review with fresh findings, and only then can it be admitted or rejected
// again. A decision is never revisited without something new to look at.
func Assess(ctx context.Context, tx pgx.Tx, applicationID uuid.UUID, c Criteria, e Evidence) (Application, error) {
	var a Application
	a.ID = applicationID
	err := tx.QueryRow(ctx, `
		SELECT company_id, merchant_id, proposed_symbol, sponsor_member, state
		  FROM listing_applications WHERE id = $1 FOR UPDATE`, applicationID).
		Scan(&a.CompanyID, &a.MerchantID, &a.ProposedSymbol, &a.SponsorMember, &a.State)
	if err != nil {
		return a, fmt.Errorf("exchange: load application: %w", err)
	}
	if a.State == "approved" || a.State == "listed" {
		return a, fmt.Errorf("exchange: application %s is already %s", applicationID, a.State)
	}

	add := func(name string, met bool, format string, args ...any) {
		a.Findings = append(a.Findings, Finding{name, met, fmt.Sprintf(format, args...)})
	}

	// A business with no trading history has no basis for a price, and the
	// auction would be discovering a number rather than a value.
	add("trading_history", e.TradingMonths >= c.MinTradingMonths,
		"%d months of trading, needs %d", e.TradingMonths, c.MinTradingMonths)

	if c.RequireAudited {
		add("audited_accounts", e.AuditedAccounts && e.AuditorOnList,
			"audited: %v, auditor on the SEC list: %v", e.AuditedAccounts, e.AuditorOnList)
	}

	// The buyback needs somewhere for a price to come from. A symbol whose only
	// holders are the founder and the scheme has no market, and its auction
	// would be the scheme trading with the issuer.
	//
	// Quantities are units (1e8 per share), so a real issuer's count times
	// 10,000 does not fit in an int64: a company with a hundred million
	// shares in issue overflowed here and read as a negative float. The
	// intermediate is exact.
	var floatBps int64
	if e.SharesInIssue > 0 {
		num := new(big.Int).Mul(big.NewInt(int64(e.PublicShares)), big.NewInt(10_000))
		floatBps = new(big.Int).Quo(num, big.NewInt(int64(e.SharesInIssue))).Int64()
	}
	add("free_float", floatBps >= c.MinFreeFloatBps,
		"%d bps in public hands, needs %d", floatBps, c.MinFreeFloatBps)
	add("holders", e.Holders >= c.MinHolders,
		"%d unrelated holders, needs %d", e.Holders, c.MinHolders)

	if c.RequireTreasury {
		add("treasury_pool", e.TreasuryUnits >= c.MinTreasuryUnits && e.BoardResolution,
			"%s reserved (needs %s), board resolution: %v",
			e.TreasuryUnits, c.MinTreasuryUnits, e.BoardResolution)
	}
	if c.RequireSponsor {
		add("sponsor", a.SponsorMember != nil, "sponsor appointed: %v", a.SponsorMember != nil)
	}
	add("directors", e.DirectorsClear,
		"no director disqualified, bankrupt or under SEC action: %v", e.DirectorsClear)

	findings, err := json.Marshal(a.Findings)
	if err != nil {
		return a, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE listing_applications SET state = 'in_review', findings = $2 WHERE id = $1`,
		applicationID, findings); err != nil {
		return a, fmt.Errorf("exchange: record findings: %w", err)
	}
	a.State = "in_review"
	return a, nil
}

// AdmitListing lists a company that passed, creating the instrument and its
// treasury.
//
// A listing that fails is not admitted by decision: the criteria are published,
// and admitting against them would make them advisory. Waiving one is a change
// to the criteria, which is a decision somebody makes in the open.
func AdmitListing(ctx context.Context, tx pgx.Tx, a Application, reference money.Kobo,
	authorised, dailyRelease share.Units, by string) (string, error) {

	if !a.Passes() {
		var short []string
		for _, f := range a.Findings {
			if !f.Met {
				short = append(short, f.Criterion)
			}
		}
		return "", fmt.Errorf("exchange: %s does not meet the listing standard: %s",
			a.ProposedSymbol, strings.Join(short, ", "))
	}
	if by == "" {
		return "", fmt.Errorf("exchange: admission requires a name")
	}
	if reference <= 0 {
		return "", fmt.Errorf("exchange: a listing needs a reference price")
	}

	assetID := "EQ:" + a.ProposedSymbol
	if _, err := tx.Exec(ctx, `
		INSERT INTO assets (id, class, scale, label) VALUES ($1,'equity',8,$2)
		ON CONFLICT (id) DO NOTHING`, assetID, a.ProposedSymbol); err != nil {
		return "", fmt.Errorf("exchange: create asset: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO instruments (id, symbol, company_id, shares_authorised_units,
		                         reference_price_kobo, status, listed_at)
		VALUES ($1,$2,$3,$4,$5,'listed',now())`,
		assetID, a.ProposedSymbol, a.CompanyID, int64(authorised), int64(reference)); err != nil {
		return "", fmt.Errorf("exchange: create instrument: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO cap_table_events (instrument_id, kind, units_delta, note)
		VALUES ($1,'authorised',$2,$3)`, assetID, int64(authorised), "admitted by "+by); err != nil {
		return "", err
	}

	// The merchant's buybacks can now find an instrument, which is the point of
	// the whole exercise.
	if a.MerchantID != nil {
		if _, err := tx.Exec(ctx,
			`UPDATE merchants SET company_id = $2 WHERE id = $1`, *a.MerchantID, a.CompanyID); err != nil {
			return "", fmt.Errorf("exchange: link merchant: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE listing_applications SET state = 'listed', decided_by = $2, decided_at = now()
		 WHERE id = $1`, a.ID, by); err != nil {
		return "", err
	}
	_ = dailyRelease
	return assetID, nil
}

// Reject closes an application with reasons.
func Reject(ctx context.Context, tx pgx.Tx, applicationID uuid.UUID, by, note string) error {
	if by == "" || note == "" {
		return fmt.Errorf("exchange: a rejection needs a name and a reason")
	}
	ct, err := tx.Exec(ctx, `
		UPDATE listing_applications SET state = 'rejected', decided_by = $2, decided_at = now(), note = $3
		 WHERE id = $1 AND state NOT IN ('listed','rejected')`, applicationID, by, note)
	if err != nil {
		return fmt.Errorf("exchange: reject application: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("exchange: application %s is already decided", applicationID)
	}
	return nil
}
