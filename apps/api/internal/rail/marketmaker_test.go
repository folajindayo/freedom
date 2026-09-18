package rail

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"freedom/api/internal/exchange"
	"freedom/api/internal/ledger"
	"freedom/api/internal/money"
	"freedom/api/internal/share"
)

// The house market maker: capital from the float, inventory from a placement,
// a quote every session, and — when nothing crosses — its mid as the price.

// fundMM puts capital on the house market maker, through the same function
// the console calls.
func fundMM(t *testing.T, s *Service, amount money.Kobo, reason string) Funding {
	t.Helper()
	var f Funding
	if err := s.inTx(context.Background(), func(tx pgx.Tx) error {
		var err error
		f, err = s.FundMarketMaker(context.Background(), tx, ParticipantCode, amount, testDate, "ops", reason)
		return err
	}); err != nil {
		t.Fatalf("fund: %v", err)
	}
	return f
}

func mmBalance(t *testing.T, s *Service, kind, asset string) int64 {
	t.Helper()
	return holderBalance(t, s.Pool, MarketMakerRef, kind, asset)
}

func TestFundMarketMakerPostsABalancedTransaction(t *testing.T) {
	p := pool(t)
	s := newService(t, p)

	f := fundMM(t, s, money.Naira(200_000), "launch capital")
	if f.Available != money.Naira(200_000) || f.LedgerTx == uuid.Nil {
		t.Fatalf("funding %+v", f)
	}
	if got := schemeBalance(t, p, ledger.KindFloat); got != -money.Naira(200_000) {
		t.Errorf("float = %s, want −₦200,000", got)
	}
	if got := mmBalance(t, s, ledger.KindAvailable, ledger.AssetNGN); money.Kobo(got) != money.Naira(200_000) {
		t.Errorf("market maker available = %s, want ₦200,000", money.Kobo(got))
	}
	var eventType string
	if err := p.QueryRow(context.Background(), `SELECT event_type FROM ledger_tx WHERE id = $1`, f.LedgerTx).Scan(&eventType); err != nil {
		t.Fatal(err)
	}
	if eventType != "mm.capital" {
		t.Errorf("event_type = %s, want mm.capital", eventType)
	}

	// The same funding on the same day with the same reason is a no-op.
	again := fundMM(t, s, money.Naira(200_000), "launch capital")
	if again.LedgerTx != f.LedgerTx || again.Available != money.Naira(200_000) {
		t.Errorf("replay: %+v", again)
	}
	// A second tranche needs its own reason.
	more := fundMM(t, s, money.Naira(200_000), "second tranche")
	if more.Available != money.Naira(400_000) {
		t.Errorf("after the second tranche: %s", more.Available)
	}

	// The member exists with the role and one market-making account.
	var roles []string
	var accounts int
	if err := p.QueryRow(context.Background(), `
		SELECT m.roles, (SELECT COUNT(*) FROM client_accounts ca WHERE ca.member_id = m.id AND ca.label = 'market_maker')
		  FROM members m WHERE m.code = $1`, ParticipantCode).Scan(&roles, &accounts); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(roles, ","), "market_maker") || accounts != 1 {
		t.Errorf("member roles %v, %d market-making accounts", roles, accounts)
	}

	// An unknown member is not found; nothing posts.
	err := s.inTx(context.Background(), func(tx pgx.Tx) error {
		_, err := s.FundMarketMaker(context.Background(), tx, "NOPE", money.Naira(1), testDate, "ops", "x")
		return err
	})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("funding an unknown member: %v", err)
	}
}

func TestOnboardPlacesABlockWithTheMarketMaker(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	s := newService(t, p)
	fundMM(t, s, money.Naira(200_000), "launch capital")

	sym := symbol()
	req := mamaPut(sym, "sp_mama", "usr_founder")
	req.MarketMakerPlacementUnits = int64(share.Whole(1_000)) // ₦40,000 at the ₦40 listing price
	l, _, err := s.OnboardBusiness(ctx, req)
	if err != nil {
		t.Fatalf("onboard: %v", err)
	}
	if l.State != "listed" {
		t.Fatalf("state %s: %+v", l.State, l.Findings)
	}
	inst := "EQ:" + sym

	// Treasury: reserved less founders less the placement.
	if l.TreasuryUnits != share.Units(130_000_000_000_000)-share.Whole(1_000) {
		t.Errorf("treasury = %s", l.TreasuryUnits)
	}
	// The block sits in the market maker's wallet, paid for at the listing price.
	if got := mmBalance(t, s, ledger.KindStockWallet, inst); share.Units(got) != share.Whole(1_000) {
		t.Errorf("market maker holds %s, want 1,000 shares", share.Units(got))
	}
	if got := mmBalance(t, s, ledger.KindAvailable, ledger.AssetNGN); money.Kobo(got) != money.Naira(160_000) {
		t.Errorf("market maker cash = %s, want ₦160,000", money.Kobo(got))
	}
	var treasuryCash int64
	if err := p.QueryRow(ctx, `
		SELECT COALESCE(SUM(e.amount),0) FROM ledger_entries e JOIN accounts a ON a.id = e.account_id
		 WHERE a.owner_type = 'company' AND a.kind = 'treasury_cash'`).Scan(&treasuryCash); err != nil {
		t.Fatal(err)
	}
	if money.Kobo(treasuryCash) != money.Naira(40_000) {
		t.Errorf("treasury cash = %s, want ₦40,000", money.Kobo(treasuryCash))
	}
	// One balanced transaction, both assets.
	var legs int
	if err := p.QueryRow(ctx, `
		SELECT COUNT(*) FROM ledger_entries e JOIN ledger_tx x ON x.id = e.tx_id WHERE x.event_type = 'rail.mm_placement'`).Scan(&legs); err != nil {
		t.Fatal(err)
	}
	if legs != 4 {
		t.Errorf("%d placement legs, want 4", legs)
	}

	// The lot: cost at the listing price, transferable today (not tap-earned).
	var lotCost int64
	var unlock string
	if err := p.QueryRow(ctx, `
		SELECT l.cost_kobo, l.transferable_from::text FROM holding_lots l
		  JOIN accounts a ON a.id = l.account_id JOIN cardholders c ON c.id = a.owner_id
		 WHERE c.external_ref = $1 AND l.instrument_id = $2`, MarketMakerRef, inst).Scan(&lotCost, &unlock); err != nil {
		t.Fatalf("placement lot: %v", err)
	}
	if money.Kobo(lotCost) != money.Naira(40_000) || unlock != testDate {
		t.Errorf("lot cost %s unlock %s", money.Kobo(lotCost), unlock)
	}
	// On the cap table and disclosed.
	var note string
	if err := p.QueryRow(ctx, `
		SELECT note FROM cap_table_events WHERE instrument_id = $1 AND kind = 'transfer' AND note LIKE 'placed with market maker%'`, inst).Scan(&note); err != nil {
		t.Fatalf("cap table event: %v", err)
	}
	if !strings.Contains(note, "placed with market maker TAPP at ₦40.00") {
		t.Errorf("note = %q", note)
	}
	var headline string
	var published *string
	if err := p.QueryRow(ctx, `
		SELECT headline, published_at::text FROM disclosures WHERE instrument_id = $1 AND kind = 'listing_particulars'`, inst).Scan(&headline, &published); err != nil {
		t.Fatalf("disclosure: %v", err)
	}
	if headline != "Market maker placement" || published == nil {
		t.Errorf("disclosure %q published %v", headline, published)
	}
	var halted bool
	if err := p.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM trading_halts WHERE instrument_id = $1 AND released_at IS NULL)`, inst).Scan(&halted); err != nil {
		t.Fatal(err)
	}
	if halted {
		t.Error("the placement notice halted trading")
	}
	t.Logf("%s: 1,000 shares placed with %s at %s; treasury %s", sym, ParticipantCode, l.ReferencePriceKobo, l.TreasuryUnits)
}

func TestOnboardRefusesAPlacementTheMarketMakerCannotPay(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	s := newService(t, p)
	// ₦10,000 of capital against a ₦40,000 block.
	fundMM(t, s, money.Naira(10_000), "thin")

	req := mamaPut(symbol(), "sp_mama", "usr_founder")
	req.MarketMakerPlacementUnits = int64(share.Whole(1_000))
	_, _, err := s.OnboardBusiness(ctx, req)
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "fund it first") {
		t.Fatalf("want an ErrInvalid refusal naming the shortfall, got %v", err)
	}
	// Nothing listed: the refusal rolled the admission back with it.
	var n int
	if err := p.QueryRow(ctx, `SELECT COUNT(*) FROM instruments`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d instruments after a refused placement, want 0", n)
	}

	// More than the treasury holds after founders.
	req.MarketMakerPlacementUnits = 130_000_000_000_001
	_, _, err = s.OnboardBusiness(ctx, req)
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("want an ErrInvalid refusal on treasury, got %v", err)
	}
}

// The whole day: quote at 10:05, close at 12:00, nothing crosses, the
// session publishes the market maker's mid, and the next tap buys at it.
func TestQuoteThenCloseZeroVolumePublishesTheMid(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	s := newService(t, p)
	fundMM(t, s, money.Naira(200_000), "launch capital")

	sym := symbol()
	req := mamaPut(sym, "sp_mama", "usr_founder")
	req.MarketMakerPlacementUnits = int64(share.Whole(2_000)) // ₦80,000 of the ₦200,000
	if _, _, err := s.OnboardBusiness(ctx, req); err != nil {
		t.Fatal(err)
	}
	inst := "EQ:" + sym

	// Appointed on the standard obligation, told to hold 4,000 shares while
	// it holds 2,000: it is short of inventory, so its quote leans up.
	var provider uuid.UUID
	if err := s.inTx(ctx, func(tx pgx.Tx) error {
		var member uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM members WHERE code = $1`, ParticipantCode).Scan(&member); err != nil {
			return err
		}
		o := exchange.StandardObligation(inst, member, "2026-01-01", "2027-12-31")
		o.TargetUnits = share.Whole(4_000)
		var err error
		provider, err = exchange.Appoint(ctx, tx, o)
		return err
	}); err != nil {
		t.Fatalf("appoint: %v", err)
	}

	quotes, err := s.QuoteMarket(ctx, testDate)
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	if len(quotes) != 1 || quotes[0].Error != "" || !quotes[0].TwoSided {
		t.Fatalf("quotes: %+v", quotes)
	}
	q := quotes[0]
	// Centre ₦40 (fair value over shares in issue), skew +250 bps (half
	// short of target), spread 300 bps: bid 40 × 1.010 = ₦40.40, ask 40 ×
	// 1.040 = ₦41.60.
	if q.Centre != 4000 || q.SkewBps != 250 || q.SpreadBps != 300 || q.Bid != 4040 || q.Ask != 4160 {
		t.Fatalf("quote: %+v", q)
	}
	// Sizes cover the ₦50,000 obligation at each side's own price.
	if bidNotional := int64(q.Bid) * int64(q.BidUnits) / int64(share.PerShare); bidNotional < int64(money.Naira(50_000)) {
		t.Errorf("bid covers %s, below the obligation", money.Kobo(bidNotional))
	}
	if askNotional := int64(q.Ask) * int64(q.AskUnits) / int64(share.PerShare); askNotional < int64(money.Naira(50_000)) {
		t.Errorf("ask covers %s, below the obligation", money.Kobo(askNotional))
	}

	// Re-running the quote is a no-op: still two orders.
	if again, err := s.QuoteMarket(ctx, testDate); err != nil || again[0].Error != "" || again[0].Bid != q.Bid {
		t.Fatalf("re-quote: %+v %v", again, err)
	}
	var orders int
	if err := p.QueryRow(ctx, `SELECT COUNT(*) FROM orders WHERE instrument_id = $1`, inst).Scan(&orders); err != nil {
		t.Fatal(err)
	}
	if orders != 2 {
		t.Fatalf("%d orders after two quoting runs, want 2", orders)
	}

	// A tap before the close waits: today's session has not published.
	before, _, err := s.IngestTap(ctx, tap("tap_q", "sp_mama", "usr_ada", money.Naira(10_000)))
	if err != nil || before.IntentState != "pending" {
		t.Fatalf("before the close: %+v %v", before, err)
	}

	rows, err := s.CloseMarket(ctx, testDate)
	if err != nil || len(rows) != 1 || rows[0].Error != "" {
		t.Fatalf("close: %+v %v", rows, err)
	}
	row := rows[0]
	if row.State != "published" || row.MatchedUnits != 0 {
		t.Fatalf("row %+v", row)
	}

	// Nothing crossed — the market maker does not trade with itself — so
	// the session published the mid of its quote as the price.
	var source string
	var price int64
	var zeroVolume bool
	var clearing *int64
	var rule *string
	if err := p.QueryRow(ctx, `
		SELECT po.source, po.price_kobo, a.zero_volume, a.clearing_price_kobo, a.rule
		  FROM price_observations_current po JOIN auctions a ON a.instrument_id = po.instrument_id AND a.session_date = po.obs_date
		 WHERE po.instrument_id = $1`, inst).Scan(&source, &price, &zeroVolume, &clearing, &rule); err != nil {
		t.Fatal(err)
	}
	if source != "quote" || price != 4100 || !zeroVolume || clearing != nil || rule == nil || *rule != "quote" {
		t.Errorf("published: source %s price %d zero_volume %v clearing %v rule %v; want quote at ₦41.00 with no clearing price",
			source, price, zeroVolume, clearing, rule)
	}
	var ref int64
	var carried int
	if err := p.QueryRow(ctx, `SELECT reference_price_kobo, carry_forward_sessions FROM instruments WHERE id = $1`, inst).Scan(&ref, &carried); err != nil {
		t.Fatal(err)
	}
	if ref != 4100 || carried != 0 {
		t.Errorf("reference %d carried %d, want 4100 and 0", ref, carried)
	}
	// The obligation was measured as met.
	var met, twoSided bool
	if err := p.QueryRow(ctx, `SELECT met, two_sided FROM lp_performance WHERE provider_id = $1 AND session_date = $2::date`, provider, testDate).Scan(&met, &twoSided); err != nil {
		t.Fatalf("performance: %v", err)
	}
	if !met || !twoSided {
		t.Error("the quote was not measured as meeting the obligation")
	}
	// No false surveillance alert on a session with no fills.
	var alerts int
	if err := p.QueryRow(ctx, `SELECT COUNT(*) FROM surveillance_alerts WHERE instrument_id = $1`, inst).Scan(&alerts); err != nil {
		t.Fatal(err)
	}
	if alerts != 0 {
		t.Errorf("%d surveillance alerts on a quote-only session", alerts)
	}
	// Reservations released.
	var reserves int64
	if err := p.QueryRow(ctx, `
		SELECT COALESCE(SUM(e.amount),0) FROM ledger_entries e JOIN accounts a ON a.id = e.account_id
		 WHERE a.kind IN ('order_cash_reserve','order_share_reserve')`).Scan(&reserves); err != nil {
		t.Fatal(err)
	}
	if reserves != 0 {
		t.Errorf("reservations left behind: %d", reserves)
	}

	// The buyback paid the mid: ₦5 at ₦41 is 0.12195121 shares.
	if row.Buyback.Intents != 1 || row.Buyback.Refusal != "" || row.PriceKobo != 4100 {
		t.Errorf("buyback %+v at %s", row.Buyback, row.PriceKobo)
	}
	after, _, err := s.IngestTap(ctx, tap("tap_q", "sp_mama", "usr_ada", money.Naira(10_000)))
	if err != nil || after.IntentState != "allocated" || after.PriceKobo != 4100 || after.AllocatedUnits != 12195121 {
		t.Errorf("after the close: %+v %v", after, err)
	}
	// The merchant and the public see where the price came from.
	b, err := s.GetBusiness(ctx, "sp_mama")
	if err != nil || b.LastSession == nil || b.LastSession.Source != "quote" || b.LastSession.PriceKobo != 4100 || b.PriceKobo != 4100 {
		t.Errorf("business view: %+v %v", b.LastSession, err)
	}
	t.Logf("%s: quoted %s / %s, nothing crossed, published %s (%s); tap bought %s at %s",
		sym, q.Bid, q.Ask, money.Kobo(price), source, after.AllocatedUnits, after.PriceKobo)
}

// With no inventory the engine quotes the bid alone, says so, and the
// session — one-sided — carries the reference forward as before.
func TestQuoteWithoutInventoryIsBidOnly(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	s := newService(t, p)
	fundMM(t, s, money.Naira(200_000), "launch capital")

	sym := symbol()
	if _, _, err := s.OnboardBusiness(ctx, mamaPut(sym, "sp_mama", "usr_founder")); err != nil {
		t.Fatal(err)
	}
	inst := "EQ:" + sym
	if err := s.inTx(ctx, func(tx pgx.Tx) error {
		var member uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM members WHERE code = $1`, ParticipantCode).Scan(&member); err != nil {
			return err
		}
		_, err := exchange.Appoint(ctx, tx, exchange.StandardObligation(inst, member, "2026-01-01", "2027-12-31"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	quotes, err := s.QuoteMarket(ctx, testDate)
	if err != nil || len(quotes) != 1 || quotes[0].Error != "" {
		t.Fatalf("quote: %+v %v", quotes, err)
	}
	q := quotes[0]
	if q.TwoSided || q.BidUnits == 0 || q.AskUnits != 0 || !strings.Contains(q.Note, "no inventory") {
		t.Fatalf("quote: %+v", q)
	}
	if q.SkewBps != 0 || q.Bid != 3940 {
		t.Errorf("no target means no skew: %+v", q)
	}

	rows, err := s.CloseMarket(ctx, testDate)
	if err != nil || rows[0].Error != "" {
		t.Fatalf("close: %+v %v", rows, err)
	}
	var source string
	if err := p.QueryRow(ctx, `SELECT source FROM price_observations_current WHERE instrument_id = $1`, inst).Scan(&source); err != nil {
		t.Fatal(err)
	}
	if source != "carry_forward" {
		t.Errorf("a one-sided quote set the price: source %s", source)
	}
	var shortfall string
	if err := p.QueryRow(ctx, `SELECT shortfall FROM lp_performance WHERE session_date = $1::date`, testDate).Scan(&shortfall); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(shortfall, "one side") {
		t.Errorf("shortfall = %q", shortfall)
	}

	// A placement after the close is fine — it is inventory for tomorrow —
	// but today's session has published, so quoting it again is refused
	// on its row rather than silently skipped.
	if err := s.inTx(ctx, func(tx pgx.Tx) error {
		_, err := s.PlaceWithMarketMaker(ctx, tx, inst, share.Whole(100), 4000, testDate, "ops", "inventory")
		return err
	}); err != nil {
		t.Fatalf("placement after the close: %v", err)
	}
	late, err := s.QuoteMarket(ctx, testDate)
	if err != nil || len(late) != 1 || !strings.Contains(late[0].Error, "no longer accepting") {
		t.Errorf("quoting a published session: %+v %v", late, err)
	}
	t.Logf("%s: bid only at %s (%s); session carried forward", sym, q.Bid, q.Note)
}

// Inventory steers the quote: short of target it leans up, long of target
// it leans down, never past the skew limit.
func TestQuoteSkewFollowsInventory(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	s := newService(t, p)
	fundMM(t, s, money.Naira(1_000_000), "launch capital")

	sym := symbol()
	req := mamaPut(sym, "sp_mama", "usr_founder")
	req.MarketMakerPlacementUnits = int64(share.Whole(3_000)) // ₦120,000
	if _, _, err := s.OnboardBusiness(ctx, req); err != nil {
		t.Fatal(err)
	}
	inst := "EQ:" + sym
	if err := s.inTx(ctx, func(tx pgx.Tx) error {
		var member uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM members WHERE code = $1`, ParticipantCode).Scan(&member); err != nil {
			return err
		}
		o := exchange.StandardObligation(inst, member, "2026-01-01", "2027-12-31")
		o.TargetUnits = share.Whole(1_000) // holds 3,000: three times long
		_, err := exchange.Appoint(ctx, tx, o)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	quotes, err := s.QuoteMarket(ctx, testDate)
	if err != nil || len(quotes) != 1 || quotes[0].Error != "" {
		t.Fatalf("quote: %+v %v", quotes, err)
	}
	q := quotes[0]
	// (1,000 − 3,000) / 1,000 × 500 = −1000, clamped to −500: bid 40 × 0.935
	// = ₦37.40, ask 40 × 0.965 = ₦38.60.
	if q.SkewBps != -500 || q.Bid != 3740 || q.Ask != 3860 || !q.TwoSided {
		t.Errorf("long of target: %+v", q)
	}
	if q.Held != share.Whole(3_000) || q.Target != share.Whole(1_000) || q.CentreSource != "fair_value" {
		t.Errorf("inventory: held %s target %s centre from %s", q.Held, q.Target, q.CentreSource)
	}
	t.Logf("%s: held %s against a %s target → skew %d bps, %s / %s", sym, q.Held, q.Target, q.SkewBps, q.Bid, q.Ask)
}
