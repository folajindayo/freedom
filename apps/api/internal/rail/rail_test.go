package rail

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"freedom/api/internal/exchange"
	"freedom/api/internal/ledger"
	"freedom/api/internal/migrate"
	"freedom/api/internal/money"
	"freedom/api/internal/scheme"
	"freedom/api/internal/share"
)

// A Friday with no calendar published, so the close has to assume it.
const testDate = "2026-09-18"

func pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = "postgres://freedom_scheme:freedom@localhost:5432/freedom"
	}
	ctx := context.Background()
	p, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Skipf("no database: %v", err)
	}
	if err := p.Ping(ctx); err != nil {
		p.Close()
		t.Skipf("no database at %s: %v", url, err)
	}
	if err := migrate.Up(ctx, p); err != nil {
		p.Close()
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() {
		if imb, err := ledger.AssetImbalance(ctx, p); err == nil && len(imb) > 0 {
			t.Errorf("ledger does not balance globally: %v", imb)
		}
		p.Close()
	})
	return p
}

// reset empties every table except reference data, read from the schema so a
// new table can never leak state between tests.
func reset(t *testing.T, p *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	rows, err := p.Query(ctx, `
		SELECT tablename FROM pg_tables
		 WHERE schemaname = 'public' AND tablename NOT IN ('assets','schema_migrations')`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	rows.Close()
	if _, err := p.Exec(ctx, `TRUNCATE `+strings.Join(names, ", ")+` RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if _, err := p.Exec(ctx, `DELETE FROM assets WHERE class = 'equity'`); err != nil {
		t.Fatal(err)
	}
}

func newService(t *testing.T, p *pgxpool.Pool) *Service {
	t.Helper()
	reset(t, p)
	s := New(p)
	at := time.Date(2026, 9, 18, 11, 0, 0, 0, scheme.Lagos)
	s.Now = func() time.Time { return at }
	return s
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// symbol is random per test, as in e2e: the database is shared and the schema
// package assumes the doc's example symbol is free.
func symbol() string { return "MAMA" + strings.ToUpper(randHex(3)) }

// mamaPut is the doc's worked example.
func mamaPut(symbol, merchantRef, founderRef string) BusinessRequest {
	var r BusinessRequest
	r.MerchantRef = merchantRef
	r.CardholderRef = founderRef
	r.LegalName = "Mama Put Kitchens Ltd"
	r.TradingName = "Mama Put"
	r.RCNumber = "RC" + randHex(4)
	r.MCC = "5812"
	r.Symbol = symbol
	r.Evidence.TradingMonths = 30
	r.Evidence.AuditedAccounts = true
	r.Evidence.AuditorOnList = true
	r.Evidence.SharesInIssue = 800_000_000_000_000
	r.Evidence.PublicShares = 120_000_000_000_000
	r.Evidence.Holders = 31
	r.Evidence.TreasuryUnits = 180_000_000_000_000
	r.Evidence.BoardResolution = true
	r.Evidence.DirectorsClear = true
	// (₦80m + 1.0 × ₦240m) / 8,000,000 shares: the exchange lists it at ₦40.
	r.Evidence.NetAssetsKobo = 8_000_000_000
	r.Evidence.RevenueKobo = 24_000_000_000
	r.SharesAuthorisedUnits = 1_000_000_000_000_000
	r.DailyReleaseUnits = 50_000_000_000_000
	r.Holders = append(r.Holders, struct {
		CardholderRef string `json:"cardholder_ref"`
		Units         int64  `json:"units"`
		Label         string `json:"label"`
	}{founderRef, 50_000_000_000_000, "founder"})
	return r
}

// publishAuction records a settled session at a price, standing in for a real
// uncross where a test only needs the price that came out of one.
func publishAuction(t *testing.T, p *pgxpool.Pool, instrumentID string, price money.Kobo) {
	t.Helper()
	ctx := context.Background()
	var auctionID uuid.UUID
	if err := p.QueryRow(ctx, `
		INSERT INTO auctions (instrument_id, session_date, state, opens_at, freezes_at,
		                      prev_reference_kobo, clearing_price_kobo, matched_units,
		                      imbalance_units, zero_volume, uncrossed_at, publishes_at, rule)
		VALUES ($1,$2::date,'published',now(),now(),$3,$3,$4,0,false,now(),now(),'max_volume')
		RETURNING id`, instrumentID, testDate, int64(price), int64(share.Whole(10))).Scan(&auctionID); err != nil {
		t.Fatalf("publish auction: %v", err)
	}
	if _, err := p.Exec(ctx, `
		INSERT INTO price_observations (instrument_id, obs_date, source, price_kobo, volume_units,
		                                session_vwap_kobo, trade_count, auction_id, content_hash)
		VALUES ($1,$2::date,'auction',$3,$4,$3,1,$5,$6)`,
		instrumentID, testDate, int64(price), int64(share.Whole(10)), auctionID, "seed-"+randHex(4)); err != nil {
		t.Fatalf("publish observation: %v", err)
	}
}

func participantBalance(t *testing.T, p *pgxpool.Pool, kind string) money.Kobo {
	t.Helper()
	var id uuid.UUID
	if err := p.QueryRow(context.Background(),
		`SELECT id FROM participants WHERE code = $1`, ParticipantCode).Scan(&id); err != nil {
		t.Fatal(err)
	}
	v, err := ledger.NairaBalance(context.Background(), p, ledger.Participant(id, kind, ledger.AssetNGN))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func schemeBalance(t *testing.T, p *pgxpool.Pool, kind string) money.Kobo {
	t.Helper()
	v, err := ledger.NairaBalance(context.Background(), p, ledger.Scheme(kind, ledger.AssetNGN))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func holderBalance(t *testing.T, p *pgxpool.Pool, ref, kind, asset string) int64 {
	t.Helper()
	var id uuid.UUID
	if err := p.QueryRow(context.Background(),
		`SELECT id FROM cardholders WHERE external_ref = $1`, ref).Scan(&id); err != nil {
		t.Fatal(err)
	}
	v, err := ledger.Balance(context.Background(), p, ledger.Cardholder(id, kind, asset))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func tap(ref, merchant, holder string, amount money.Kobo) Tap {
	return Tap{TapRef: ref, MerchantRef: merchant, CardholderRef: holder,
		CardholderDisplayName: "Ada", AmountKobo: amount,
		ChargedAt: time.Date(2026, 9, 18, 10, 24, 3, 0, time.UTC)}
}

// ---------------------------------------------------------------- businesses

func TestOnboardListsAPassingBusiness(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	s := newService(t, p)

	sym := symbol()
	req := mamaPut(sym, "sp_mama", "usr_founder")
	l, created, err := s.OnboardBusiness(ctx, req)
	if err != nil {
		t.Fatalf("onboard: %v", err)
	}
	if !created || l.State != "listed" {
		t.Fatalf("state = %s, created = %v; findings %+v", l.State, created, l.Findings)
	}
	if l.InstrumentID == nil || *l.InstrumentID != "EQ:"+sym {
		t.Fatalf("instrument = %v, want EQ:%s", l.InstrumentID, sym)
	}
	for _, f := range l.Findings {
		if !f.Met {
			t.Errorf("finding %s unmet: %s", f.Criterion, f.Detail)
		}
	}
	// Treasury holds what the company reserved less the founders' allotment.
	if l.TreasuryUnits != share.Units(130_000_000_000_000) {
		t.Errorf("treasury = %s, want 1,300,000 shares", l.TreasuryUnits)
	}
	if got := holderBalance(t, p, "usr_founder", ledger.KindStockWallet, "EQ:"+sym); share.Units(got) != 50_000_000_000_000 {
		t.Errorf("founder holds %s, want 500,000 shares", share.Units(got))
	}
	var lotCost int64
	var unlock string
	if err := p.QueryRow(ctx, `
		SELECT l.cost_kobo, l.transferable_from::text FROM holding_lots l
		  JOIN accounts a ON a.id = l.account_id JOIN cardholders c ON c.id = a.owner_id
		 WHERE c.external_ref = 'usr_founder'`).Scan(&lotCost, &unlock); err != nil {
		t.Fatalf("founder lot: %v", err)
	}
	if lotCost != 0 || unlock != testDate {
		t.Errorf("founder lot cost %d unlock %s, want 0 and %s (founders' shares are not tap earnings)", lotCost, unlock, testDate)
	}
	var transfers int
	if err := p.QueryRow(ctx,
		`SELECT COUNT(*) FROM cap_table_events WHERE instrument_id = $1 AND kind = 'transfer'`, "EQ:"+sym).Scan(&transfers); err != nil {
		t.Fatal(err)
	}
	if transfers != 1 {
		t.Errorf("%d transfer events, want 1", transfers)
	}
	var daily int64
	if err := p.QueryRow(ctx, `SELECT daily_release_units FROM treasury_pools WHERE instrument_id = $1`, "EQ:"+sym).Scan(&daily); err != nil {
		t.Fatalf("no treasury pool: %v", err)
	}

	// A second call is a read.
	again, created, err := s.OnboardBusiness(ctx, req)
	if err != nil || created || again.State != "listed" || again.TreasuryUnits != l.TreasuryUnits {
		t.Fatalf("replay: %+v created=%v err=%v", again, created, err)
	}

	b, err := s.GetBusiness(ctx, "sp_mama")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if b.Holders != 1 || len(b.TopHolders) != 1 || b.TopHolders[0].CardholderRef != "usr_founder" {
		t.Errorf("cap table holders: %d %+v", b.Holders, b.TopHolders)
	}
	if b.TreasuryRemaining != l.TreasuryUnits || b.DailyReleaseUnits != 50_000_000_000_000 {
		t.Errorf("cap table: treasury %s daily %s", b.TreasuryRemaining, b.DailyReleaseUnits)
	}
	t.Logf("%s listed at %s; founder holds %s, treasury %s", b.Symbol, b.ReferencePriceKobo,
		b.TopHolders[0].Units, b.TreasuryRemaining)
}

func TestOnboardRejectsAFailingBusiness(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	s := newService(t, p)

	req := mamaPut(symbol(), "sp_young", "usr_young")
	req.Evidence.TradingMonths = 8
	l, created, err := s.OnboardBusiness(ctx, req)
	if err != nil {
		t.Fatalf("onboard: %v", err)
	}
	if !created || l.State != "rejected" || l.InstrumentID != nil {
		t.Fatalf("state = %s instrument = %v", l.State, l.InstrumentID)
	}
	unmet := map[string]bool{}
	for _, f := range l.Findings {
		if !f.Met {
			unmet[f.Criterion] = true
		}
	}
	if !unmet["trading_history"] || len(unmet) != 1 {
		t.Errorf("unmet = %v, want exactly trading_history", unmet)
	}
	var n int
	if err := p.QueryRow(ctx, `SELECT COUNT(*) FROM instruments`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("a rejected business got an instrument")
	}
	// The same evidence again is the same answer, with the reason refreshed.
	if got, _, err := s.OnboardBusiness(ctx, req); err != nil || got.State != "rejected" {
		t.Fatalf("replay of a rejection: %+v %v", got, err)
	}
	if err := p.QueryRow(ctx, `SELECT COUNT(*) FROM listing_applications WHERE merchant_id =
	    (SELECT id FROM merchants WHERE external_ref = 'sp_young')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("applications = %d, want the one re-decided, not a second", n)
	}

	// Two years later the evidence is there. The resubmission re-assesses the
	// same application and admits it: instrument, treasury, the founder's lot.
	req.Evidence.TradingMonths = 30
	l, created, err = s.OnboardBusiness(ctx, req)
	if err != nil {
		t.Fatalf("resubmit: %v", err)
	}
	if !created || l.State != "listed" || l.InstrumentID == nil {
		t.Fatalf("after resubmission state = %s instrument = %v", l.State, l.InstrumentID)
	}
	var state string
	if err := p.QueryRow(ctx, `SELECT state FROM listing_applications WHERE proposed_symbol = $1`,
		req.Symbol).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "listed" {
		t.Errorf("application state = %s, want listed", state)
	}
	h, err := s.Holdings(ctx, "usr_young")
	if err != nil || len(h.Holdings) != 1 || h.Holdings[0].Units != share.Units(req.Holders[0].Units) {
		t.Fatalf("founder holdings after resubmission: %+v %v", h, err)
	}
	// And once listed, a further call is a read again.
	if got, created, err := s.OnboardBusiness(ctx, req); err != nil || created || got.State != "listed" {
		t.Fatalf("call after listing: created=%v %+v %v", created, got, err)
	}
}

// The exchange names the price. An applicant's own number, if it still sends
// one, is ignored: ₦20m of net assets and ₦60m of revenue over 10,000,000
// shares is ₦8.00 a share whatever the founder typed.
func TestOnboardPricesTheListingFromTheAccounts(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	s := newService(t, p)

	sym := symbol()
	req := mamaPut(sym, "sp_blaze", "usr_blaze")
	req.Evidence.SharesInIssue = 1_000_000_000_000_000 // 10,000,000 shares
	req.Evidence.PublicShares = 150_000_000_000_000
	req.Evidence.NetAssetsKobo = int64(money.Naira(20_000_000))
	req.Evidence.RevenueKobo = int64(money.Naira(60_000_000))
	req.ReferencePriceKobo = 10_000_000 // ₦100,000, and nobody asked
	l, _, err := s.OnboardBusiness(ctx, req)
	if err != nil {
		t.Fatalf("onboard: %v", err)
	}
	if l.State != "listed" || l.ReferencePriceKobo != money.Naira(8) || l.FairValueKobo != money.Naira(80_000_000) {
		t.Fatalf("state %s at %s (fair value %s), want listed at ₦8.00; findings %+v",
			l.State, l.ReferencePriceKobo, l.FairValueKobo, l.Findings)
	}
	var ref int64
	if err := p.QueryRow(ctx, `SELECT reference_price_kobo FROM instruments WHERE id = $1`, "EQ:"+sym).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	if ref != 800 {
		t.Errorf("instrument reference = %d kobo, want 800", ref)
	}
	var found *exchange.Finding
	for i := range l.Findings {
		if l.Findings[i].Criterion == "listing_price" {
			found = &l.Findings[i]
		}
	}
	if found == nil || !found.Met {
		t.Fatalf("no listing_price finding on the record: %+v", l.Findings)
	}
	for _, want := range []string{"₦80,000,000.00", "₦20,000,000.00", "1.0×", "₦60,000,000.00", "10000000 shares", "listed at ₦8.00"} {
		if !strings.Contains(found.Detail, want) {
			t.Errorf("listing_price detail %q lacks %q", found.Detail, want)
		}
	}
	// The record re-read says the same.
	again, created, err := s.OnboardBusiness(ctx, req)
	if err != nil || created || again.ReferencePriceKobo != money.Naira(8) || again.FairValueKobo != money.Naira(80_000_000) {
		t.Fatalf("replay: %+v created=%v err=%v", again, created, err)
	}
	t.Logf("%s: %s", sym, found.Detail)
}

// The rules' worked example: ₦120m + ₦280m over 8,000,000 shares is ₦50.00.
func TestOnboardPricesAtFifty(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	s := newService(t, p)

	req := mamaPut(symbol(), "sp_fifty", "usr_fifty")
	req.Evidence.NetAssetsKobo = int64(money.Naira(120_000_000))
	req.Evidence.RevenueKobo = int64(money.Naira(280_000_000))
	l, _, err := s.OnboardBusiness(ctx, req)
	if err != nil || l.State != "listed" || l.ReferencePriceKobo != money.Naira(50) {
		t.Fatalf("state %s at %s, want listed at ₦50.00 (err %v)", l.State, l.ReferencePriceKobo, err)
	}
}

// A fair value below one tick a share lists at one tick, not at zero.
func TestOnboardNeverPricesBelowATick(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	s := newService(t, p)

	req := mamaPut(symbol(), "sp_tiny", "usr_tiny")
	req.Evidence.NetAssetsKobo = 1
	req.Evidence.RevenueKobo = 1
	l, _, err := s.OnboardBusiness(ctx, req)
	if err != nil || l.State != "listed" || l.ReferencePriceKobo != 1 {
		t.Fatalf("state %s at %s, want listed at one kobo (err %v)", l.State, l.ReferencePriceKobo, err)
	}
}

// No audited financials, no price, no listing — and the refusal still
// carries the price the exchange would have set, which here is a tick.
func TestOnboardRefusesAnUnpriceableBusiness(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	s := newService(t, p)

	req := mamaPut(symbol(), "sp_nofin", "usr_nofin")
	req.Evidence.NetAssetsKobo = 0
	req.Evidence.RevenueKobo = 0
	l, _, err := s.OnboardBusiness(ctx, req)
	if err != nil {
		t.Fatalf("onboard: %v", err)
	}
	if l.State != "rejected" || l.InstrumentID != nil {
		t.Fatalf("state = %s instrument = %v", l.State, l.InstrumentID)
	}
	unmet := map[string]bool{}
	for _, f := range l.Findings {
		if !f.Met {
			unmet[f.Criterion] = true
		}
	}
	if !unmet["financials"] || len(unmet) != 1 {
		t.Errorf("unmet = %v, want exactly financials", unmet)
	}
	if l.FairValueKobo != 0 || l.ReferencePriceKobo != 1 {
		t.Errorf("refusal reports fair value %s, price %s; want ₦0.00 and one tick", l.FairValueKobo, l.ReferencePriceKobo)
	}
	again, _, err := s.OnboardBusiness(ctx, req)
	if err != nil || again.State != "rejected" || again.ReferencePriceKobo != 1 {
		t.Fatalf("replay of a refusal: %+v %v", again, err)
	}
}

// ---------------------------------------------------------------- taps

func TestIngestTapAllocatesWhenPriced(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	s := newService(t, p)

	sym := symbol()
	if _, _, err := s.OnboardBusiness(ctx, mamaPut(sym, "sp_mama", "usr_founder")); err != nil {
		t.Fatal(err)
	}
	publishAuction(t, p, "EQ:"+sym, 4000)

	r, created, err := s.IngestTap(ctx, tap("tap_1", "sp_mama", "usr_ada", money.Naira(10_000)))
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if !created {
		t.Fatal("first delivery reported as a replay")
	}
	if r.FeeKobo != money.Naira(50) || r.BuybackFundingKobo != money.Naira(5) {
		t.Errorf("fee %s funding %s, want ₦50 and ₦5", r.FeeKobo, r.BuybackFundingKobo)
	}
	if r.IntentState != "allocated" || r.Symbol == nil || *r.Symbol != sym {
		t.Fatalf("intent %s symbol %v, want allocated %s", r.IntentState, r.Symbol, sym)
	}
	// ₦5 at ₦40 is 0.125 shares, exactly.
	if r.AllocatedUnits != share.PerShare/8 || r.PriceKobo != 4000 {
		t.Errorf("allocated %s at %s, want 0.125 at ₦40", r.AllocatedUnits, r.PriceKobo)
	}
	if r.LockUntil == nil || *r.LockUntil != "2027-01-16" {
		t.Errorf("lock_until = %v, want 2027-01-16 (120 days)", r.LockUntil)
	}

	// The accounting: Tapp owes exactly the MSC; the hold and the merchant's
	// receivable are both consumed.
	if got := participantBalance(t, p, ledger.KindSettlement); got != -money.Naira(50) {
		t.Errorf("participant settlement = %s, want −₦50.00", got)
	}
	if got := participantBalance(t, p, ledger.KindInterchangeIncome); got != money.Naira(30) {
		t.Errorf("interchange = %s, want ₦30.00", got)
	}
	if got := holderBalance(t, p, "usr_ada", ledger.KindHold, ledger.AssetNGN); got != 0 {
		t.Errorf("hold = %d, want 0", got)
	}
	var recv int64
	if err := p.QueryRow(ctx, `
		SELECT COALESCE(SUM(e.amount),0) FROM ledger_entries e JOIN accounts a ON a.id = e.account_id
		 WHERE a.owner_type = 'merchant' AND a.kind = 'merchant_receivable'`).Scan(&recv); err != nil {
		t.Fatal(err)
	}
	if recv != 0 {
		t.Errorf("merchant receivable = %d, want 0 (the acquirer already paid)", recv)
	}
	// 10% of the MSC funded the buyback, and the buyback spent it on treasury.
	var funding int64
	if err := p.QueryRow(ctx, `SELECT funding_kobo FROM buyback_intents`).Scan(&funding); err != nil {
		t.Fatal(err)
	}
	if money.Kobo(funding) != money.Naira(5) {
		t.Errorf("intent funding = %s, want ₦5.00", money.Kobo(funding))
	}
	var treasuryCash int64
	if err := p.QueryRow(ctx, `
		SELECT COALESCE(SUM(e.amount),0) FROM ledger_entries e JOIN accounts a ON a.id = e.account_id
		 WHERE a.owner_type = 'company' AND a.kind = 'treasury_cash'`).Scan(&treasuryCash); err != nil {
		t.Fatal(err)
	}
	if schemeBalance(t, p, ledger.KindBuybackPool)+money.Kobo(treasuryCash) != money.Naira(5) {
		t.Errorf("pool + treasury cash = %s, want ₦5.00", schemeBalance(t, p, ledger.KindBuybackPool)+money.Kobo(treasuryCash))
	}

	// What the cardholder sees.
	h, err := s.Holdings(ctx, "usr_ada")
	if err != nil {
		t.Fatalf("holdings: %v", err)
	}
	if len(h.Holdings) != 1 {
		t.Fatalf("%d holdings, want 1", len(h.Holdings))
	}
	line := h.Holdings[0]
	if line.Units != share.PerShare/8 || line.Shares != "0.125" || line.LockedUnits != line.Units || line.SellableUnits != 0 {
		t.Errorf("holding %+v", line)
	}
	if line.NextUnlock == nil || *line.NextUnlock != "2027-01-16" || line.CostKobo != 500 || line.ValueKobo != 500 || line.Lots != 1 {
		t.Errorf("holding %+v", line)
	}
	if line.LastSession == nil || line.LastSession.PriceKobo != 4000 || line.LastSession.Source != "auction" {
		t.Errorf("last session %+v", line.LastSession)
	}
	if h.TotalValueKobo != 500 || h.TotalCostKobo != 500 {
		t.Errorf("totals %+v", h)
	}

	d, err := s.Holding(ctx, "usr_ada", strings.ToLower(sym))
	if err != nil {
		t.Fatalf("holding: %v", err)
	}
	if len(d.LotLines) != 1 || d.LotLines[0].TapRef == nil || *d.LotLines[0].TapRef != "tap_1" {
		t.Errorf("lots %+v", d.LotLines)
	}
	if len(d.Prices) != 1 {
		t.Errorf("%d prices, want 1", len(d.Prices))
	}

	acts, err := s.Activity(ctx, "usr_ada", 10)
	if err != nil || len(acts) != 1 || acts[0].State != "allocated" || *acts[0].TapRef != "tap_1" {
		t.Errorf("activity %+v %v", acts, err)
	}
	t.Logf("one ₦10,000 tap at %s: %s now owns %s shares, locked until %s",
		sym, "usr_ada", line.Shares, *line.NextUnlock)
}

func TestOnboardRefusesASecondMerchantOfAListedCompany(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	s := newService(t, p)

	sym := symbol()
	first := mamaPut(sym, "sp_mama", "usr_founder")
	if l, _, err := s.OnboardBusiness(ctx, first); err != nil || l.State != "listed" {
		t.Fatalf("first: %+v %v", l, err)
	}
	// Same company (rc_number), different merchant, new symbol: refused with
	// the symbol it is already listed as, and nothing created.
	second := mamaPut(symbol(), "sp_mama_branch", "usr_founder")
	second.RCNumber = first.RCNumber
	_, created, err := s.OnboardBusiness(ctx, second)
	var listed *AlreadyListedError
	if !errors.As(err, &listed) || created || listed.Symbol != sym || listed.RCNumber != first.RCNumber {
		t.Fatalf("second merchant: created=%v err=%v", created, err)
	}
	var instruments, merchants int
	if err := p.QueryRow(ctx, `
		SELECT (SELECT COUNT(*) FROM instruments), (SELECT COUNT(*) FROM merchants WHERE external_ref = 'sp_mama_branch')`).
		Scan(&instruments, &merchants); err != nil {
		t.Fatal(err)
	}
	if instruments != 1 || merchants != 0 {
		t.Errorf("%d instruments, %d branch merchants; want 1 and 0", instruments, merchants)
	}
	// The first merchant's own replay is still a read.
	if l, created, err := s.OnboardBusiness(ctx, first); err != nil || created || l.State != "listed" {
		t.Errorf("replay: %+v %v %v", l, created, err)
	}
	// And the schema holds the line even if the service is bypassed.
	var companyID string
	if err := p.QueryRow(ctx, `SELECT company_id FROM instruments`).Scan(&companyID); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `INSERT INTO assets (id,class,scale,label) VALUES ('EQ:SECOND','equity',8,'EQ:SECOND')`); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `
		INSERT INTO instruments (id, symbol, company_id, shares_authorised_units, status)
		VALUES ('EQ:SECOND','SECOND',$1,100000000000,'listed')`, companyID); err == nil ||
		!strings.Contains(err.Error(), "instruments_one_live_per_company") {
		t.Errorf("schema allowed a second live instrument: %v", err)
	}
}

func TestIngestForUnlistedMerchantEscrows(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	s := newService(t, p)

	r, _, err := s.IngestTap(ctx, tap("tap_u", "sp_unknown", "usr_bola", money.Naira(10_000)))
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if r.IntentState != "escrowed" || r.Symbol != nil || r.IntentID == nil {
		t.Fatalf("intent %s symbol %v", r.IntentState, r.Symbol)
	}
	// The funding is held, not lost.
	if got := schemeBalance(t, p, ledger.KindBuybackPool); got != money.Naira(5) {
		t.Errorf("buyback pool = %s, want ₦5.00", got)
	}
	if got := participantBalance(t, p, ledger.KindSettlement); got != -money.Naira(50) {
		t.Errorf("participant settlement = %s, want −₦50.00", got)
	}

	// The merchant then lists, and the escrowed funding becomes pending
	// against the new instrument.
	late := symbol()
	l, _, err := s.OnboardBusiness(ctx, mamaPut(late, "sp_unknown", "usr_owner"))
	if err != nil || l.State != "listed" {
		t.Fatalf("list later: %+v %v", l, err)
	}
	var state string
	var inst *string
	if err := p.QueryRow(ctx, `SELECT state, instrument_id FROM buyback_intents`).Scan(&state, &inst); err != nil {
		t.Fatal(err)
	}
	if state != "pending" || inst == nil || *inst != "EQ:"+late {
		t.Errorf("after listing the intent is %s on %v", state, inst)
	}
}

func TestReplayDoesNotDoublePost(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	s := newService(t, p)

	sym := symbol()
	if _, _, err := s.OnboardBusiness(ctx, mamaPut(sym, "sp_mama", "usr_founder")); err != nil {
		t.Fatal(err)
	}
	publishAuction(t, p, "EQ:"+sym, 4000)

	first, _, err := s.IngestTap(ctx, tap("tap_r", "sp_mama", "usr_ada", money.Naira(10_000)))
	if err != nil {
		t.Fatal(err)
	}
	second, created, err := s.IngestTap(ctx, tap("tap_r", "sp_mama", "usr_ada", money.Naira(10_000)))
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("the replay reported as a new tap")
	}
	if second.PresentmentID != first.PresentmentID || second.AllocatedUnits != first.AllocatedUnits {
		t.Errorf("replay answered differently: %+v vs %+v", first, second)
	}
	var presentments, postings int
	if err := p.QueryRow(ctx, `SELECT COUNT(*) FROM presentments`).Scan(&presentments); err != nil {
		t.Fatal(err)
	}
	if err := p.QueryRow(ctx, `SELECT COUNT(*) FROM ledger_tx WHERE idempotency_key LIKE 'rail|%|tap_r'`).Scan(&postings); err != nil {
		t.Fatal(err)
	}
	if presentments != 1 || postings != 2 {
		t.Errorf("%d presentments, %d rail postings after a replay; want 1 and 2", presentments, postings)
	}
	if got := participantBalance(t, p, ledger.KindSettlement); got != -money.Naira(50) {
		t.Errorf("participant settlement = %s after replay, want −₦50.00", got)
	}
}

func TestReverseUnwinds(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	s := newService(t, p)

	sym := symbol()
	if _, _, err := s.OnboardBusiness(ctx, mamaPut(sym, "sp_mama", "usr_founder")); err != nil {
		t.Fatal(err)
	}
	publishAuction(t, p, "EQ:"+sym, 4000)
	r, _, err := s.IngestTap(ctx, tap("tap_x", "sp_mama", "usr_ada", money.Naira(10_000)))
	if err != nil || r.IntentState != "allocated" {
		t.Fatalf("ingest: %+v %v", r, err)
	}
	poolBefore := schemeBalance(t, p, ledger.KindBuybackPool)

	rev, err := s.ReverseTap(ctx, "tap_x", "merchant_reversal")
	if err != nil {
		t.Fatalf("reverse: %v", err)
	}
	if rev.State != "reversed" || rev.UnwoundUnits != share.PerShare/8 {
		t.Fatalf("reverse = %+v", rev)
	}
	// Everything is back where it was: no shares, no position, no hold, and
	// the pool made whole.
	if got := holderBalance(t, p, "usr_ada", ledger.KindStockWallet, "EQ:"+sym); got != 0 {
		t.Errorf("wallet = %d after reversal", got)
	}
	if got := participantBalance(t, p, ledger.KindSettlement); got != 0 {
		t.Errorf("participant settlement = %s, want zero", got)
	}
	if got := participantBalance(t, p, ledger.KindInterchangeIncome); got != 0 {
		t.Errorf("interchange = %s, want zero", got)
	}
	if got := holderBalance(t, p, "usr_ada", ledger.KindHold, ledger.AssetNGN); got != 0 {
		t.Errorf("hold = %d, want 0", got)
	}
	if got := schemeBalance(t, p, ledger.KindBuybackPool); got != poolBefore+money.Naira(5)-money.Naira(5) {
		t.Errorf("pool = %s, want %s", got, poolBefore)
	}
	var state string
	if err := p.QueryRow(ctx, `SELECT state FROM buyback_intents`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "unwound" {
		t.Errorf("intent is %s, want unwound", state)
	}
	var cause *uuid.UUID
	if err := p.QueryRow(ctx, `SELECT reversal_presentment_id FROM buyback_unwinds`).Scan(&cause); err != nil || cause == nil {
		t.Errorf("unwind has no reversal cause: %v", err)
	}
	view, _, err := s.IngestTap(ctx, tap("tap_x", "sp_mama", "usr_ada", money.Naira(10_000)))
	if err != nil || !view.Reversed {
		t.Errorf("a reversed tap reads back as %+v", view)
	}
	if again, err := s.ReverseTap(ctx, "tap_x", "merchant_reversal"); err != nil || again.UnwoundUnits != rev.UnwoundUnits {
		t.Errorf("reverse replay: %+v %v", again, err)
	}
	h, err := s.Holdings(ctx, "usr_ada")
	if err != nil || len(h.Holdings) != 0 {
		t.Errorf("holdings after reversal: %+v %v", h, err)
	}
}

// ---------------------------------------------------------------- the close

func TestCloseMarketAllocatesPendingIntents(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	s := newService(t, p)

	sym := symbol()
	if _, _, err := s.OnboardBusiness(ctx, mamaPut(sym, "sp_mama", "usr_founder")); err != nil {
		t.Fatal(err)
	}
	// No session has published yet, so the tap's intent waits for the close.
	r, _, err := s.IngestTap(ctx, tap("tap_c", "sp_mama", "usr_ada", money.Naira(10_000)))
	if err != nil {
		t.Fatal(err)
	}
	if r.IntentState != "pending" || r.AllocatedUnits != 0 {
		t.Fatalf("before the close: %+v", r)
	}
	if got := schemeBalance(t, p, ledger.KindBuybackPool); got != money.Naira(5) {
		t.Errorf("pool = %s before the close, want ₦5.00", got)
	}

	rows, err := s.CloseMarket(ctx, testDate)
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("%d rows, want 1", len(rows))
	}
	row := rows[0]
	if row.Error != "" {
		t.Fatalf("close failed: %s", row.Error)
	}
	// Nothing traded, so the session carried the reference forward and the
	// buyback paid that.
	if row.State != "published" || row.MatchedUnits != 0 || row.PriceKobo != 4000 {
		t.Errorf("row %+v", row)
	}
	if row.Buyback.Intents != 1 || row.Buyback.Units != share.PerShare/8 || row.Buyback.Refusal != "" {
		t.Errorf("buyback %+v", row.Buyback)
	}
	var source string
	if err := p.QueryRow(ctx, `SELECT source FROM price_observations ORDER BY id DESC LIMIT 1`).Scan(&source); err != nil {
		t.Fatal(err)
	}
	if source != "carry_forward" {
		t.Errorf("observation source = %s, want carry_forward", source)
	}
	after, _, err := s.IngestTap(ctx, tap("tap_c", "sp_mama", "usr_ada", money.Naira(10_000)))
	if err != nil || after.IntentState != "allocated" || after.AllocatedUnits != share.PerShare/8 {
		t.Errorf("after the close: %+v %v", after, err)
	}

	// The close is idempotent.
	again, err := s.CloseMarket(ctx, testDate)
	if err != nil || again[0].Error != "" || again[0].Buyback.Intents != 0 {
		t.Errorf("second close: %+v %v", again, err)
	}
	if got := holderBalance(t, p, "usr_ada", ledger.KindStockWallet, "EQ:"+sym); share.Units(got) != share.PerShare/8 {
		t.Errorf("wallet = %s after two closes, want 0.125", share.Units(got))
	}

	today, err := s.Today(ctx)
	if err != nil || !today.IsTrading || today.BusinessDate != testDate || len(today.Instruments) != 1 ||
		today.Instruments[0].SessionState != "published" {
		t.Errorf("today: %+v %v", today, err)
	}
	t.Logf("close on %s: %s carried %s, buyback allocated %s shares", testDate, row.Symbol, row.PriceKobo, row.Buyback.Units)
}

func TestCloseMarketRetriesEscrowedIntents(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	s := newService(t, p)

	sym := symbol()
	if _, _, err := s.OnboardBusiness(ctx, mamaPut(sym, "sp_mama", "usr_founder")); err != nil {
		t.Fatal(err)
	}
	publishAuction(t, p, "EQ:"+sym, 4000)
	if err := s.inTx(ctx, func(tx pgx.Tx) error {
		_, err := exchange.Halt(ctx, tx, "EQ:"+sym, exchange.HaltIssuer, "surveillance", nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// Priced session, but halted: the engine refuses and escrows.
	r, _, err := s.IngestTap(ctx, tap("tap_h", "sp_mama", "usr_ada", money.Naira(10_000)))
	if err != nil {
		t.Fatal(err)
	}
	if r.IntentState != "escrowed" {
		t.Fatalf("under a halt: intent %s, want escrowed", r.IntentState)
	}
	// A close under the halt retries and escrows again, with the reason.
	rows, err := s.CloseMarket(ctx, testDate)
	if err != nil || rows[0].Error != "" {
		t.Fatalf("close under halt: %+v %v", rows, err)
	}
	if b := rows[0].Buyback; b.Retried != 1 || b.Escrowed != 1 || b.Intents != 0 || b.Refusal != "halted" {
		t.Errorf("under halt: %+v", b)
	}

	if err := s.inTx(ctx, func(tx pgx.Tx) error {
		return exchange.Release(ctx, tx, "EQ:"+sym, "surveillance")
	}); err != nil {
		t.Fatal(err)
	}
	rows, err = s.CloseMarket(ctx, testDate)
	if err != nil || rows[0].Error != "" {
		t.Fatalf("close after release: %+v %v", rows, err)
	}
	if b := rows[0].Buyback; b.Retried != 1 || b.Intents != 1 || b.Escrowed != 0 || b.Units != share.PerShare/8 {
		t.Errorf("after release: %+v", b)
	}
	after, _, err := s.IngestTap(ctx, tap("tap_h", "sp_mama", "usr_ada", money.Naira(10_000)))
	if err != nil || after.IntentState != "allocated" {
		t.Errorf("after the close: %+v %v", after, err)
	}
	if got := holderBalance(t, p, "usr_ada", ledger.KindStockWallet, "EQ:"+sym); share.Units(got) != share.PerShare/8 {
		t.Errorf("wallet = %s, want 0.125", share.Units(got))
	}
}
