package exchange

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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
	// RequireFinancials demands audited net assets and trailing revenue. They
	// are what the listing price is set from, so an applicant without them
	// cannot be priced and so cannot list.
	RequireFinancials bool
	// RevenueMultipleBps is the multiple of trailing revenue added to net
	// assets to reach fair value, in basis points: 10_000 is 1.0×.
	RevenueMultipleBps int64
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
		// ⚠ placeholder: docs/LISTING-RULES.md §2.4.
		RequireFinancials:  true,
		RevenueMultipleBps: 10_000, // 1.0×
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
	// NetAssetsKobo and RevenueKobo are from the audited accounts: net assets
	// at the balance sheet date and revenue for the trailing twelve months.
	// The listing price is set from them (ListingPrice).
	NetAssetsKobo money.Kobo
	RevenueKobo   money.Kobo
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

	// The exchange sets the listing price from these figures. Without them
	// there is no price, and a listing without a price is not a listing.
	if c.RequireFinancials {
		add("financials", e.NetAssetsKobo > 0 && e.RevenueKobo > 0 && e.AuditedAccounts,
			"net assets %s, revenue %s (12 months), audited: %v",
			e.NetAssetsKobo, e.RevenueKobo, e.AuditedAccounts)
	}

	if err := saveFindings(ctx, tx, applicationID, "in_review", a.Findings); err != nil {
		return a, err
	}
	a.State = "in_review"
	return a, nil
}

func saveFindings(ctx context.Context, tx pgx.Tx, applicationID uuid.UUID, state string, fs []Finding) error {
	findings, err := json.Marshal(fs)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE listing_applications SET state = $2, findings = $3 WHERE id = $1`,
		applicationID, state, findings); err != nil {
		return fmt.Errorf("exchange: record findings: %w", err)
	}
	return nil
}

// FairValue is what the exchange values the company at, in kobo:
//
//	FairValue = NetAssets + RevenueMultiple × Revenue
//
// The intermediate is exact; a result beyond the kobo range saturates rather
// than wraps, and is then plainly wrong instead of quietly negative.
func FairValue(c Criteria, e Evidence) money.Kobo {
	fv := fairValue(c, e)
	if !fv.IsInt64() {
		return money.Kobo(math.MaxInt64)
	}
	return money.Kobo(fv.Int64())
}

func fairValue(c Criteria, e Evidence) *big.Int {
	rev := new(big.Int).Mul(big.NewInt(int64(e.RevenueKobo)), big.NewInt(c.RevenueMultipleBps))
	rev.Quo(rev, big.NewInt(10_000))
	fv := new(big.Int).Add(big.NewInt(int64(e.NetAssetsKobo)), rev)
	if fv.Sign() < 0 {
		return new(big.Int)
	}
	return fv
}

// ListingPrice is the price the exchange lists a company at. The applicant
// does not name it: it is fair value over the shares in issue —
//
//	ListingPrice = FairValue / SharesInIssue
//
// in kobo per share, rounded DOWN to the tick and never below one tick.
// Rounding down means the price never overstates the accounts; the floor
// means a company whose fair value is below a kobo a share still has a price
// the auction can move from. Quantities are units (1e8 per share), so
// FairValue × PerShare is exact in big.Int: a ₦1bn company with a hundred
// million shares in issue is 1e19 in the intermediate, past int64.
//
// A company with no shares in issue has no price per share; it gets one
// tick, and fails the float criterion anyway.
func ListingPrice(c Criteria, e Evidence, tick money.Kobo) (price, fair money.Kobo) {
	if tick <= 0 {
		tick = 1
	}
	fair = FairValue(c, e)
	if e.SharesInIssue <= 0 {
		return tick, fair
	}
	n := new(big.Int).Mul(fairValue(c, e), big.NewInt(int64(share.PerShare)))
	n.Quo(n, big.NewInt(int64(e.SharesInIssue)))
	n.Quo(n, big.NewInt(int64(tick)))
	n.Mul(n, big.NewInt(int64(tick)))
	if !n.IsInt64() {
		// The kobo range, on the tick. Nothing real gets here.
		n.SetInt64(math.MaxInt64)
		n.Quo(n, big.NewInt(int64(tick))).Mul(n, big.NewInt(int64(tick)))
	}
	price = money.Kobo(n.Int64())
	if price < tick {
		price = tick
	}
	return price, fair
}

// multipleString renders basis points as a multiple: 10_000 → "1.0×",
// 12_500 → "1.25×".
func multipleString(bps int64) string {
	frac := strings.TrimRight(fmt.Sprintf("%04d", bps%10_000), "0")
	if frac == "" {
		frac = "0"
	}
	return fmt.Sprintf("%d.%s×", bps/10_000, frac)
}

// RecordListingPrice puts the price decision on the application beside the
// other findings. It is always met — the price is the exchange's, and the
// exchange does not refuse its own number — but the record shows how it
// was reached, which is what a merchant asking "why ₦8?" needs to see.
func RecordListingPrice(ctx context.Context, tx pgx.Tx, a *Application, c Criteria, e Evidence,
	price, fair money.Kobo) error {

	multiple := multipleString(c.RevenueMultipleBps)
	a.Findings = append(a.Findings, Finding{
		Criterion: "listing_price",
		Met:       true,
		Detail: fmt.Sprintf("fair value %s (net assets %s + %s revenue %s) over %s shares → listed at %s",
			fair, e.NetAssetsKobo, multiple, e.RevenueKobo, e.SharesInIssue, price),
	})
	if err := saveFindings(ctx, tx, a.ID, a.State, a.Findings); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE listing_applications SET fair_value_kobo = $2, listing_price_kobo = $3 WHERE id = $1`,
		a.ID, int64(fair), int64(price)); err != nil {
		return fmt.Errorf("exchange: record listing price: %w", err)
	}
	return nil
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

// ErrNoSharesInIssue is returned when a listing cannot be priced per share
// because the instrument's share count has never been set.
var ErrNoSharesInIssue = errors.New("exchange: set shares in issue first")

// ErrSessionOpen is returned when a re-anchor is attempted while today's
// session is still accepting or frozen: the price it would change is the one
// that session is about to discover from.
var ErrSessionOpen = errors.New("exchange: re-anchor after the close")

// Reanchored is what a re-anchor decided.
type Reanchored struct {
	Price     money.Kobo
	FairValue money.Kobo
	Shares    share.Units
}

// Reanchor re-prices a listing admitted before the pricing rule under it.
//
// It is the exchange's existing correction mechanism — a correction is a new
// price observation with source 'manual', never an update to an old one —
// applied to the reference: the instrument's reference becomes fair value
// over the shares in issue, the staleness counter resets because the price
// is fresh, and the observation carries the operator's name and reason in
// its hash so the row can be explained later. The decision is also written
// to the company's latest application beside the admission findings, where
// the listing price belongs.
func Reanchor(ctx context.Context, tx pgx.Tx, instrumentID, sessionDate string, c Criteria,
	netAssets, revenue money.Kobo, by, reason string) (Reanchored, error) {

	var out Reanchored
	if by == "" || strings.TrimSpace(reason) == "" {
		return out, fmt.Errorf("exchange: a re-anchor needs a name and a reason")
	}
	if netAssets <= 0 || revenue <= 0 {
		return out, fmt.Errorf("exchange: a re-anchor needs positive audited net assets and revenue")
	}
	var symbol string
	var companyID uuid.UUID
	var shares, tick int64
	if err := tx.QueryRow(ctx, `
		SELECT symbol, company_id, shares_in_issue_units, tick_kobo
		  FROM instruments WHERE id = $1 FOR UPDATE`, instrumentID).
		Scan(&symbol, &companyID, &shares, &tick); err != nil {
		return out, fmt.Errorf("exchange: load instrument: %w", err)
	}
	if shares <= 0 {
		return out, ErrNoSharesInIssue
	}
	var open bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM auctions WHERE instrument_id = $1 AND session_date = $2::date
		                  AND state IN ('accepting','frozen'))`, instrumentID, sessionDate).Scan(&open); err != nil {
		return out, err
	}
	if open {
		return out, ErrSessionOpen
	}

	e := Evidence{NetAssetsKobo: netAssets, RevenueKobo: revenue, SharesInIssue: share.Units(shares)}
	out.Price, out.FairValue = ListingPrice(c, e, money.Kobo(tick))
	out.Shares = e.SharesInIssue

	if _, err := tx.Exec(ctx, `
		UPDATE instruments SET reference_price_kobo = $2, carry_forward_sessions = 0 WHERE id = $1`,
		instrumentID, int64(out.Price)); err != nil {
		return out, fmt.Errorf("exchange: re-anchor reference: %w", err)
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d|%s|%s", symbol, sessionDate, int64(out.Price), by, reason)))
	if _, err := tx.Exec(ctx, `
		INSERT INTO price_observations (instrument_id, obs_date, source, price_kobo, volume_units,
		                                trade_count, content_hash)
		VALUES ($1,$2::date,'manual',$3,0,0,$4)
		ON CONFLICT (instrument_id, obs_date, source, content_hash) DO NOTHING`,
		instrumentID, sessionDate, int64(out.Price), hex.EncodeToString(sum[:])); err != nil {
		return out, fmt.Errorf("exchange: record correction: %w", err)
	}

	// On the application, beside the admission findings. A listing seeded
	// without one (tests, migrations) has nowhere to write; that is not an
	// error, the instrument and the observation are the record.
	var a Application
	var findings []byte
	err := tx.QueryRow(ctx, `
		SELECT id, state, findings FROM listing_applications
		 WHERE company_id = $1 ORDER BY created_at DESC LIMIT 1 FOR UPDATE`, companyID).
		Scan(&a.ID, &a.State, &findings)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, fmt.Errorf("exchange: load application: %w", err)
	}
	if err := json.Unmarshal(findings, &a.Findings); err != nil {
		return out, fmt.Errorf("exchange: decode findings: %w", err)
	}
	a.Findings = append(a.Findings, Finding{
		Criterion: "listing_price",
		Met:       true,
		Detail: fmt.Sprintf("re-anchored by %s: %s; fair value %s (net assets %s + %s revenue %s) over %s shares → %s",
			by, reason, out.FairValue, netAssets, multipleString(c.RevenueMultipleBps), revenue, e.SharesInIssue, out.Price),
	})
	if err := saveFindings(ctx, tx, a.ID, a.State, a.Findings); err != nil {
		return out, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE listing_applications SET fair_value_kobo = $2, listing_price_kobo = $3 WHERE id = $1`,
		a.ID, int64(out.FairValue), int64(out.Price)); err != nil {
		return out, fmt.Errorf("exchange: record re-anchor: %w", err)
	}
	return out, nil
}
