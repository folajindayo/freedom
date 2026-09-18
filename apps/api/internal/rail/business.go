package rail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"freedom/api/internal/exchange"
	"freedom/api/internal/ledger"
	"freedom/api/internal/money"
	"freedom/api/internal/scheme"
	"freedom/api/internal/share"
)

// Onboarding is listing. A merchant registering a business in the Tapp app is
// a company applying to list on Freedom, and it is measured against the same
// rulebook as anyone else — the card relationship makes a listing useful, not
// appropriate.

// BusinessRequest is what Tapp sends when a merchant registers a business.
type BusinessRequest struct {
	MerchantRef   string `json:"merchant_ref"`
	CardholderRef string `json:"cardholder_ref"`
	LegalName     string `json:"legal_name"`
	TradingName   string `json:"trading_name"`
	RCNumber      string `json:"rc_number"`
	MCC           string `json:"mcc"`
	Symbol        string `json:"symbol"`
	Evidence      struct {
		TradingMonths   int   `json:"trading_months"`
		AuditedAccounts bool  `json:"audited_accounts"`
		AuditorOnList   bool  `json:"auditor_on_list"`
		SharesInIssue   int64 `json:"shares_in_issue"`
		PublicShares    int64 `json:"public_shares"`
		Holders         int   `json:"holders"`
		TreasuryUnits   int64 `json:"treasury_units"`
		BoardResolution bool  `json:"board_resolution"`
		DirectorsClear  bool  `json:"directors_clear"`
	} `json:"evidence"`
	ReferencePriceKobo    int64 `json:"reference_price_kobo"`
	SharesAuthorisedUnits int64 `json:"shares_authorised_units"`
	DailyReleaseUnits     int64 `json:"daily_release_units"`
	CoFundBps             int64 `json:"cofund_bps"`
	Holders               []struct {
		CardholderRef string `json:"cardholder_ref"`
		Units         int64  `json:"units"`
		Label         string `json:"label"`
	} `json:"holders"`
}

// Listing is the outcome of an application.
type Listing struct {
	MerchantRef        string             `json:"merchant_ref"`
	Symbol             string             `json:"symbol"`
	InstrumentID       *string            `json:"instrument_id"`
	State              string             `json:"state"`
	Findings           []exchange.Finding `json:"findings"`
	ReferencePriceKobo money.Kobo         `json:"reference_price_kobo"`
	TreasuryUnits      share.Units        `json:"treasury_units"`
}

// OnboardBusiness applies, assesses, admits and seeds — or records the refusal.
//
// created is false when the merchant already has a record, which is returned
// unchanged: the rail retries, and a retry must not lodge a second application.
func (s *Service) OnboardBusiness(ctx context.Context, req BusinessRequest) (out Listing, created bool, err error) {
	req.MerchantRef = strings.TrimSpace(req.MerchantRef)
	if req.MerchantRef == "" {
		return out, false, fmt.Errorf("%w: merchant_ref is required", ErrInvalid)
	}
	if req.LegalName == "" || req.TradingName == "" {
		return out, false, fmt.Errorf("%w: legal_name and trading_name are required", ErrInvalid)
	}
	if len(req.MCC) != 4 {
		return out, false, fmt.Errorf("%w: mcc must be four digits", ErrInvalid)
	}
	if req.ReferencePriceKobo <= 0 || req.SharesAuthorisedUnits <= 0 || req.DailyReleaseUnits <= 0 {
		return out, false, fmt.Errorf("%w: reference_price_kobo, shares_authorised_units and daily_release_units must be positive", ErrInvalid)
	}
	if req.CoFundBps < 0 || req.CoFundBps > 300 {
		return out, false, fmt.Errorf("%w: cofund_bps must be between 0 and 300", ErrInvalid)
	}
	var distributed share.Units
	seen := map[string]bool{}
	for _, h := range req.Holders {
		if h.CardholderRef == "" || h.Units <= 0 {
			return out, false, fmt.Errorf("%w: every holder needs a cardholder_ref and positive units", ErrInvalid)
		}
		if seen[h.CardholderRef] {
			return out, false, fmt.Errorf("%w: holder %s is listed twice", ErrInvalid, h.CardholderRef)
		}
		seen[h.CardholderRef] = true
		distributed += share.Units(h.Units)
	}
	// Founders' shares come out of the treasury the company reserved. Handing
	// out more than was reserved would issue shares nobody authorised.
	if distributed > share.Units(req.Evidence.TreasuryUnits) {
		return out, false, fmt.Errorf("%w: holders receive %s but only %s is in treasury",
			ErrInvalid, distributed, share.Units(req.Evidence.TreasuryUnits))
	}

	today := scheme.BusinessDate(s.Now())
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		id, err := ensure(ctx, tx)
		if err != nil {
			return err
		}
		if err := lockRef(ctx, tx, "merchant", req.MerchantRef); err != nil {
			return err
		}

		// A merchant that already applied gets its record back, whatever the
		// outcome was. The doc's contract: a second call is a read.
		existing, err := s.listingFor(ctx, tx, req.MerchantRef)
		if err == nil {
			out = existing
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}

		// The CAC number identifies the company. If it is already listed, this
		// is a second merchant of a listed company, not a new applicant: refuse
		// and name the symbol. The schema enforces the same invariant
		// (instruments_one_live_per_company); this is the readable answer.
		if rc := strings.TrimSpace(req.RCNumber); rc != "" {
			var symbol string
			err := tx.QueryRow(ctx, `
				SELECT i.symbol FROM instruments i JOIN companies c ON c.id = i.company_id
				 WHERE c.rc_number = $1 AND i.status IN ('listed','halted','suspended')`, rc).Scan(&symbol)
			if err == nil {
				return &AlreadyListedError{RCNumber: rc, Symbol: symbol}
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("rail: company lookup: %w", err)
			}
		}

		companyID, merchantID, err := s.createBusiness(ctx, tx, id, req)
		if err != nil {
			return err
		}

		app, err := exchange.Apply(ctx, tx, companyID, req.Symbol, &merchantID, &id.Sponsor)
		if err != nil {
			return err
		}
		assessed, err := exchange.Assess(ctx, tx, app, exchange.StandardCriteria(), exchange.Evidence{
			TradingMonths:   req.Evidence.TradingMonths,
			AuditedAccounts: req.Evidence.AuditedAccounts,
			AuditorOnList:   req.Evidence.AuditorOnList,
			SharesInIssue:   share.Units(req.Evidence.SharesInIssue),
			PublicShares:    share.Units(req.Evidence.PublicShares),
			Holders:         req.Evidence.Holders,
			TreasuryUnits:   share.Units(req.Evidence.TreasuryUnits),
			BoardResolution: req.Evidence.BoardResolution,
			DirectorsClear:  req.Evidence.DirectorsClear,
		})
		if err != nil {
			return err
		}
		out = Listing{
			MerchantRef:        req.MerchantRef,
			Symbol:             assessed.ProposedSymbol,
			Findings:           assessed.Findings,
			ReferencePriceKobo: money.Kobo(req.ReferencePriceKobo),
		}
		created = true

		if !assessed.Passes() {
			var short []string
			for _, f := range assessed.Findings {
				if !f.Met {
					short = append(short, f.Criterion)
				}
			}
			if err := exchange.Reject(ctx, tx, app, "rail", "unmet: "+strings.Join(short, ", ")); err != nil {
				return err
			}
			out.State = "rejected"
			return nil
		}

		instrumentID, err := exchange.AdmitListing(ctx, tx, assessed, money.Kobo(req.ReferencePriceKobo),
			share.Units(req.SharesAuthorisedUnits), share.Units(req.DailyReleaseUnits), "rail")
		if err != nil {
			return err
		}
		if err := s.seedTreasury(ctx, tx, id, companyID, instrumentID, assessed.ProposedSymbol,
			share.Units(req.Evidence.TreasuryUnits), share.Units(req.DailyReleaseUnits), today); err != nil {
			return err
		}
		for _, h := range req.Holders {
			if err := s.distributeFounder(ctx, tx, id, companyID, instrumentID, assessed.ProposedSymbol,
				h.CardholderRef, share.Units(h.Units), h.Label, today); err != nil {
				return err
			}
		}

		// The listings pipeline pays out: funding that accrued against this
		// merchant while it was unlisted now has an instrument to buy, and
		// the next session's buyback picks it up like any other intent.
		if _, err := tx.Exec(ctx, `
			UPDATE buyback_intents SET instrument_id = $2, state = 'pending'
			 WHERE merchant_id = $1 AND instrument_id IS NULL AND state = 'escrowed'`,
			merchantID, instrumentID); err != nil {
			return fmt.Errorf("rail: adopt escrowed intents: %w", err)
		}

		out.State = "listed"
		out.InstrumentID = &instrumentID
		out.TreasuryUnits = share.Units(req.Evidence.TreasuryUnits) - distributed
		return nil
	})
	return out, created, err
}

// createBusiness makes the company and merchant rows, or completes a merchant
// row a tap created before the merchant onboarded.
func (s *Service) createBusiness(ctx context.Context, tx pgx.Tx, id identity, req BusinessRequest) (company, merchant uuid.UUID, err error) {
	rc := strings.TrimSpace(req.RCNumber)
	if rc != "" {
		// The CAC number is the company's identity. A second merchant of the
		// same company shares it rather than creating a duplicate issuer.
		err = tx.QueryRow(ctx, `
			INSERT INTO companies (legal_name, rc_number) VALUES ($1, $2)
			ON CONFLICT (rc_number) DO UPDATE SET legal_name = EXCLUDED.legal_name
			RETURNING id`, req.LegalName, rc).Scan(&company)
	} else {
		err = tx.QueryRow(ctx, `INSERT INTO companies (legal_name) VALUES ($1) RETURNING id`,
			req.LegalName).Scan(&company)
	}
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("rail: company: %w", err)
	}

	// kyb_status is verified because Tapp, as the acquirer, holds the KYB and
	// vouches for the merchant by submitting it.
	err = tx.QueryRow(ctx, `
		INSERT INTO merchants (acquirer_id, legal_name, trading_name, mcc, kyb_status,
		                       cofund_bps, company_id, external_ref)
		VALUES ($1, $2, $3, $4, 'verified', $5, $6, $7)
		ON CONFLICT (external_ref) DO UPDATE
		  SET legal_name = EXCLUDED.legal_name, trading_name = EXCLUDED.trading_name,
		      mcc = EXCLUDED.mcc, kyb_status = EXCLUDED.kyb_status,
		      cofund_bps = EXCLUDED.cofund_bps, company_id = EXCLUDED.company_id
		RETURNING id`,
		id.Participant, req.LegalName, req.TradingName, req.MCC, req.CoFundBps, company, req.MerchantRef).
		Scan(&merchant)
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("rail: merchant: %w", err)
	}
	return company, merchant, nil
}

// seedTreasury puts the reserved shares into the company's treasury and opens
// the release policy the buyback reads. AdmitListing creates the instrument;
// the shares themselves arrive from outside the system, as every issuance does.
func (s *Service) seedTreasury(ctx context.Context, tx pgx.Tx, id identity, companyID uuid.UUID,
	instrumentID, symbol string, units, dailyRelease share.Units, today string) error {

	ext, err := ledger.Resolve(ctx, tx, ledger.Scheme(ledger.KindExternal, instrumentID))
	if err != nil {
		return err
	}
	treasury, err := ledger.Resolve(ctx, tx, ledger.Company(companyID, ledger.KindTreasury, instrumentID))
	if err != nil {
		return err
	}
	if units > 0 {
		if _, err := ledger.Post(ctx, tx, ledger.Tx{
			EventType:      "rail.treasury_seeded",
			BusinessDate:   today,
			IdempotencyKey: "rail|treasury|" + instrumentID,
			Entries: []ledger.Entry{
				{AccountID: ext, Amount: ledger.Equity(symbol, -units), Reason: "listing.authorised"},
				{AccountID: treasury, Amount: ledger.Equity(symbol, units), Reason: "listing.treasury"},
			},
		}); err != nil && !errors.Is(err, ledger.ErrAlreadyPosted) {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO treasury_pools (instrument_id, account_id, daily_release_units)
		VALUES ($1, $2, $3) ON CONFLICT (instrument_id) DO NOTHING`,
		instrumentID, treasury, int64(dailyRelease)); err != nil {
		return fmt.Errorf("rail: treasury pool: %w", err)
	}
	return nil
}

// distributeFounder moves founders' shares from treasury into a holder's wallet
// as a zero-cost lot dated today and unlocked. These are not tap earnings, so
// the chargeback lock has nothing to protect and does not apply.
func (s *Service) distributeFounder(ctx context.Context, tx pgx.Tx, id identity, companyID uuid.UUID,
	instrumentID, symbol, cardholderRef string, units share.Units, label, today string) error {

	holder, _, err := upsertCardholder(ctx, tx, id, cardholderRef, "")
	if err != nil {
		return err
	}
	treasury, err := ledger.Resolve(ctx, tx, ledger.Company(companyID, ledger.KindTreasury, instrumentID))
	if err != nil {
		return err
	}
	wallet, err := ledger.Resolve(ctx, tx, ledger.Cardholder(holder, ledger.KindStockWallet, instrumentID))
	if err != nil {
		return err
	}
	txID, err := ledger.Post(ctx, tx, ledger.Tx{
		EventType:      "rail.founder_allotment",
		BusinessDate:   today,
		IdempotencyKey: "rail|founder|" + instrumentID + "|" + cardholderRef,
		Entries: []ledger.Entry{
			{AccountID: treasury, Amount: ledger.Equity(symbol, -units), Reason: "listing.founder_release"},
			{AccountID: wallet, Amount: ledger.Equity(symbol, units), Reason: "listing.founder_allotment"},
		},
	})
	if err != nil {
		if errors.Is(err, ledger.ErrAlreadyPosted) {
			return nil
		}
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO holding_lots (account_id, instrument_id, units, units_open, cost_kobo,
		                          cost_open_kobo, transferable_from, ledger_tx_id)
		VALUES ($1, $2, $3, $3, 0, 0, $4::date, $5)`,
		wallet, instrumentID, int64(units), today, txID); err != nil {
		return fmt.Errorf("rail: founder lot: %w", err)
	}
	note := "founder allotment to " + cardholderRef
	if label != "" {
		note += " (" + label + ")"
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO cap_table_events (instrument_id, kind, units_delta, ledger_tx_id, note)
		VALUES ($1, 'transfer', $2, $3, $4)`, instrumentID, int64(units), txID, note); err != nil {
		return fmt.Errorf("rail: cap table event: %w", err)
	}
	return nil
}

// listingFor reads the recorded outcome for a merchant, without the cap table.
func (s *Service) listingFor(ctx context.Context, q ledger.Querier, merchantRef string) (Listing, error) {
	var l Listing
	var findings []byte
	var instrumentID *string
	var refPrice *int64
	var appState string
	err := q.QueryRow(ctx, `
		SELECT a.proposed_symbol, a.state, a.findings, i.id, i.reference_price_kobo
		  FROM merchants m
		  JOIN listing_applications a ON a.merchant_id = m.id
		  LEFT JOIN instruments i ON i.company_id = a.company_id
		                         AND i.status IN ('listed','halted','suspended')
		 WHERE m.external_ref = $1
		 ORDER BY a.created_at DESC LIMIT 1`, merchantRef).
		Scan(&l.Symbol, &appState, &findings, &instrumentID, &refPrice)
	if errors.Is(err, pgx.ErrNoRows) {
		return l, fmt.Errorf("%w: no business registered for merchant %q", ErrNotFound, merchantRef)
	}
	if err != nil {
		return l, fmt.Errorf("rail: load listing: %w", err)
	}
	l.MerchantRef = merchantRef
	if err := json.Unmarshal(findings, &l.Findings); err != nil {
		return l, fmt.Errorf("rail: decode findings: %w", err)
	}
	if l.Findings == nil {
		l.Findings = []exchange.Finding{}
	}
	l.State = appState
	l.InstrumentID = instrumentID
	if refPrice != nil {
		l.ReferencePriceKobo = money.Kobo(*refPrice)
	}
	if instrumentID != nil {
		var companyID uuid.UUID
		if err := q.QueryRow(ctx, `SELECT company_id FROM instruments WHERE id = $1`, *instrumentID).Scan(&companyID); err != nil {
			return l, err
		}
		treasury, err := ledger.ShareBalance(ctx, q, ledger.Company(companyID, ledger.KindTreasury, *instrumentID))
		if err != nil {
			return l, err
		}
		l.TreasuryUnits = treasury
	}
	return l, nil
}

// Business is a listing with its cap table.
type Business struct {
	Listing
	SharesAuthorised    share.Units     `json:"shares_authorised"`
	InIssue             share.Units     `json:"in_issue"`
	TreasuryRemaining   share.Units     `json:"treasury_remaining"`
	ReleasedToday       share.Units     `json:"released_today"`
	DailyReleaseUnits   share.Units     `json:"daily_release_units"`
	Holders             int             `json:"holders"`
	TopHolders          []Holder        `json:"top_holders"`
	PendingFundingKobo  money.Kobo      `json:"pending_funding_kobo"`
	EscrowedFundingKobo money.Kobo      `json:"escrowed_funding_kobo"`
	LastSession         *SessionSummary `json:"last_session"`
	Halted              HaltView        `json:"halted"`
}

// Holder is one line of the cap table.
type Holder struct {
	CardholderRef string      `json:"cardholder_ref"`
	Units         share.Units `json:"units"`
	CostKobo      money.Kobo  `json:"cost_kobo"`
	FirstAcquired string      `json:"first_acquired"`
	LockedUnits   share.Units `json:"locked_units"`
}

// SessionSummary is the last auction, as the merchant sees it.
type SessionSummary struct {
	Date      string     `json:"date"`
	State     string     `json:"state"`
	PriceKobo money.Kobo `json:"price_kobo"`
	// Source says where the price came from: 'auction' when the session
	// crossed, 'carry_forward' when nothing traded and the reference stood.
	// A zero-volume session has no clearing price, but it still has a price
	// the buyback paid, and the merchant should see that one, not a zero.
	Source       string      `json:"source,omitempty"`
	VolumeUnits  share.Units `json:"volume_units"`
	MatchedUnits share.Units `json:"matched_units,omitempty"`
}

// HaltView says whether trading is suspended and why.
type HaltView struct {
	Halted bool   `json:"halted"`
	Reason string `json:"reason,omitempty"`
}

// GetBusiness returns the record and its live cap table.
func (s *Service) GetBusiness(ctx context.Context, merchantRef string) (Business, error) {
	var b Business
	today := scheme.BusinessDate(s.Now())
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		l, err := s.listingFor(ctx, tx, merchantRef)
		if err != nil {
			return err
		}
		b.Listing = l
		b.TopHolders = []Holder{}

		m, err := findMerchant(ctx, tx, merchantRef)
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(SUM(funding_kobo) FILTER (WHERE state = 'pending'), 0)::bigint,
			       COALESCE(SUM(funding_kobo) FILTER (WHERE state = 'escrowed'), 0)::bigint
			  FROM buyback_intents WHERE merchant_id = $1`, m.ID).
			Scan(&b.PendingFundingKobo, &b.EscrowedFundingKobo); err != nil {
			return fmt.Errorf("rail: intent funding: %w", err)
		}
		if l.InstrumentID == nil {
			return nil
		}
		inst := *l.InstrumentID

		var daily *int64
		if err := tx.QueryRow(ctx, `
			SELECT i.shares_authorised_units, p.daily_release_units,
			       COALESCE((SELECT units FROM treasury_releases
			                  WHERE instrument_id = i.id AND session_date = $2::date), 0)
			  FROM instruments i LEFT JOIN treasury_pools p ON p.instrument_id = i.id
			 WHERE i.id = $1`, inst, today).
			Scan(&b.SharesAuthorised, &daily, &b.ReleasedToday); err != nil {
			return fmt.Errorf("rail: instrument policy: %w", err)
		}
		if daily != nil {
			b.DailyReleaseUnits = share.Units(*daily)
		}
		b.TreasuryRemaining = l.TreasuryUnits

		// In issue means held by someone other than the treasury: what the
		// lots say, which is also what the wallets say.
		holders, err := s.holdersOf(ctx, tx, inst, today, "", 10, true)
		if err != nil {
			return err
		}
		b.TopHolders = holders
		if err := tx.QueryRow(ctx, `
			SELECT COUNT(DISTINCT l.account_id), COALESCE(SUM(l.units_open), 0)::bigint
			  FROM holding_lots l JOIN accounts a ON a.id = l.account_id
			 WHERE l.instrument_id = $1 AND a.kind = 'stock_wallet' AND l.units_open > 0`, inst).
			Scan(&b.Holders, &b.InIssue); err != nil {
			return fmt.Errorf("rail: holder count: %w", err)
		}

		b.LastSession, err = lastSession(ctx, tx, inst)
		if err != nil {
			return err
		}
		b.Halted, err = haltOf(ctx, tx, inst)
		return err
	})
	return b, err
}

// ListHolders pages every holder of a merchant's instrument, by cardholder_ref.
func (s *Service) ListHolders(ctx context.Context, merchantRef, cursor string, limit int) ([]Holder, string, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var out []Holder
	var next string
	today := scheme.BusinessDate(s.Now())
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		l, err := s.listingFor(ctx, tx, merchantRef)
		if err != nil {
			return err
		}
		out = []Holder{}
		if l.InstrumentID == nil {
			return nil
		}
		// One past the page tells us whether there is a next page without a
		// count query.
		rows, err := s.holdersOf(ctx, tx, *l.InstrumentID, today, cursor, limit+1, false)
		if err != nil {
			return err
		}
		if len(rows) > limit {
			rows = rows[:limit]
			next = rows[limit-1].CardholderRef
		}
		out = rows
		return nil
	})
	return out, next, err
}

// holdersOf aggregates lots per holder: by units for the cap table's top
// holders, by cardholder_ref for a stable page cursor.
func (s *Service) holdersOf(ctx context.Context, q ledger.Querier, instrumentID, asOf, cursor string,
	limit int, byUnits bool) ([]Holder, error) {

	order := "c.external_ref"
	if byUnits {
		order = "units DESC, c.external_ref"
	}
	rows, err := q.Query(ctx, `
		SELECT COALESCE(c.external_ref, c.id::text),
		       SUM(l.units_open)::bigint AS units,
		       SUM(l.cost_open_kobo)::bigint,
		       MIN(l.acquired_at)::date::text,
		       COALESCE(SUM(l.units_open) FILTER (WHERE l.transferable_from > $2::date), 0)::bigint
		  FROM holding_lots l
		  JOIN accounts a     ON a.id = l.account_id
		  JOIN cardholders c  ON c.id = a.owner_id
		 WHERE l.instrument_id = $1 AND a.owner_type = 'cardholder' AND a.kind = 'stock_wallet'
		   AND l.units_open > 0 AND COALESCE(c.external_ref, c.id::text) > $3
		 GROUP BY c.id
		 ORDER BY `+order+`
		 LIMIT $4`, instrumentID, asOf, cursor, limit)
	if err != nil {
		return nil, fmt.Errorf("rail: holders: %w", err)
	}
	defer rows.Close()
	out := []Holder{}
	for rows.Next() {
		var h Holder
		if err := rows.Scan(&h.CardholderRef, &h.Units, &h.CostKobo, &h.FirstAcquired, &h.LockedUnits); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func lastSession(ctx context.Context, q ledger.Querier, instrumentID string) (*SessionSummary, error) {
	var ss SessionSummary
	var price, matched *int64
	var observed int64
	err := q.QueryRow(ctx, `
		SELECT a.session_date::text, a.state, a.clearing_price_kobo, a.matched_units,
		       COALESCE(p.price_kobo, 0), COALESCE(p.source, '')
		  FROM auctions a
		  LEFT JOIN price_observations_current p
		         ON p.instrument_id = a.instrument_id AND p.obs_date = a.session_date
		 WHERE a.instrument_id = $1
		 ORDER BY a.session_date DESC LIMIT 1`, instrumentID).
		Scan(&ss.Date, &ss.State, &price, &matched, &observed, &ss.Source)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("rail: last session: %w", err)
	}
	if price != nil {
		ss.PriceKobo = money.Kobo(*price)
	} else if observed > 0 {
		ss.PriceKobo = money.Kobo(observed)
	}
	if matched != nil {
		ss.VolumeUnits = share.Units(*matched)
		ss.MatchedUnits = ss.VolumeUnits
	}
	return &ss, nil
}

func haltOf(ctx context.Context, q ledger.Querier, instrumentID string) (HaltView, error) {
	var h HaltView
	err := q.QueryRow(ctx, `
		SELECT reason FROM trading_halts WHERE instrument_id = $1 AND released_at IS NULL LIMIT 1`,
		instrumentID).Scan(&h.Reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return h, nil
	}
	if err != nil {
		return h, fmt.Errorf("rail: halt: %w", err)
	}
	h.Halted = true
	return h, nil
}
