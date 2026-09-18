package public

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"freedom/api/internal/migrate"
	"freedom/api/internal/money"
	"freedom/api/internal/rail"
	"freedom/api/internal/scheme"
)

// The public market over a real database: seed one listed business through
// the rail, close the market, tap it, and read what anyone can see.

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
	t.Cleanup(p.Close)

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
	return p
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

type client struct {
	srv *httptest.Server
	svc *rail.Service
}

func newClient(t *testing.T, p *pgxpool.Pool) *client {
	t.Helper()
	svc := rail.New(p)
	at := time.Date(2026, 9, 18, 11, 0, 0, 0, scheme.Lagos)
	svc.Now = func() time.Time { return at }
	s := New(p, svc)
	s.Now = svc.Now
	root := chi.NewRouter()
	root.Mount("/v1/market", s.API())
	root.Mount("/market", s.Page())
	srv := httptest.NewServer(root)
	t.Cleanup(srv.Close)
	return &client{srv: srv, svc: svc}
}

// get reads a path with no credential of any kind.
func (c *client) get(t *testing.T, path string) (int, http.Header, map[string]any) {
	t.Helper()
	resp, err := http.Get(c.srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, resp.Header, out
}

func mamaPut(symbol string) rail.BusinessRequest {
	var r rail.BusinessRequest
	r.MerchantRef, r.CardholderRef = "sp_1", "usr_owner"
	r.LegalName, r.TradingName = "Mama Put Kitchens Ltd", "Mama Put"
	r.RCNumber, r.MCC, r.Symbol = "RC"+randHex(4), "5812", symbol
	r.Evidence.TradingMonths = 30
	r.Evidence.AuditedAccounts, r.Evidence.AuditorOnList = true, true
	r.Evidence.SharesInIssue, r.Evidence.PublicShares = 800_000_000_000_000, 120_000_000_000_000
	r.Evidence.Holders, r.Evidence.TreasuryUnits = 31, 180_000_000_000_000
	r.Evidence.BoardResolution, r.Evidence.DirectorsClear = true, true
	r.ReferencePriceKobo, r.SharesAuthorisedUnits, r.DailyReleaseUnits = 4000, 1_000_000_000_000_000, 50_000_000_000_000
	r.Holders = append(r.Holders, struct {
		CardholderRef string `json:"cardholder_ref"`
		Units         int64  `json:"units"`
		Label         string `json:"label"`
	}{"usr_owner", 50_000_000_000_000, "founder"})
	return r
}

// seed onboards one business, closes the market and taps it: the tap lands
// after the close, so it allocates at once against the carried reference.
func seed(t *testing.T, c *client) string {
	t.Helper()
	ctx := context.Background()
	sym := "MAMA" + strings.ToUpper(randHex(3))
	out, _, err := c.svc.OnboardBusiness(ctx, mamaPut(sym))
	if err != nil || out.State != "listed" {
		t.Fatalf("onboard: %v %+v", err, out)
	}
	rows, err := c.svc.CloseMarket(ctx, testDate)
	if err != nil || len(rows) != 1 || rows[0].State != "published" {
		t.Fatalf("close: %v %+v", err, rows)
	}
	if _, _, err := c.svc.IngestTap(ctx, rail.Tap{TapRef: "tap_1", MerchantRef: "sp_1", CardholderRef: "usr_ada",
		CardholderDisplayName: "Ada", AmountKobo: money.Naira(10_000),
		ChargedAt: time.Date(2026, 9, 18, 10, 24, 3, 0, time.UTC)}); err != nil {
		t.Fatalf("tap: %v", err)
	}
	return sym
}

func TestPublicMarket(t *testing.T) {
	c := newClient(t, pool(t))
	sym := seed(t, c)

	status, hdr, m := c.get(t, "/v1/market")
	if status != http.StatusOK {
		t.Fatalf("GET /v1/market: %d %v", status, m)
	}
	if cc := hdr.Get("Cache-Control"); cc != "public, max-age=30" {
		t.Errorf("cache-control: %q", cc)
	}
	if m["business_date"] != testDate || m["phase"] != "accepting" || m["is_trading"] != true || m["as_of"] == nil {
		t.Errorf("market header: %v", m)
	}
	if _, ok := m["index"]; !ok {
		t.Errorf("index key missing: %v", m)
	}
	list, _ := m["instruments"].([]any)
	if len(list) != 1 {
		t.Fatalf("instruments: %v", m["instruments"])
	}
	row := list[0].(map[string]any)
	// 8e14 units × 4000 kobo / 1e8 units per share.
	const wantCap = float64(800_000_000_000_000 * 4000 / 100_000_000)
	if row["symbol"] != sym || row["legal_name"] != "Mama Put Kitchens Ltd" || row["trading_name"] != "Mama Put" ||
		row["status"] != "listed" || row["structure"] != "auction_only" || row["halted"] != false ||
		row["price_kobo"] != float64(4000) || row["price_source"] != "carry_forward" ||
		row["reference_price_kobo"] != float64(4000) || row["market_cap_kobo"] != wantCap ||
		row["shares_in_issue_units"] != float64(800_000_000_000_000) || row["holders"] != float64(2) ||
		row["listed_at"] == nil {
		t.Errorf("instrument row: %v", row)
	}
	if row["change_bps"] != nil {
		t.Errorf("change_bps with one session: %v, want null", row["change_bps"])
	}
	for _, k := range []string{"merchant_ref", "cardholder_ref", "member_id", "instrument_id", "orders"} {
		if _, ok := row[k]; ok {
			t.Errorf("row exposes %s", k)
		}
	}

	status, hdr, d := c.get(t, "/v1/market/"+strings.ToLower(sym))
	if status != http.StatusOK {
		t.Fatalf("GET /v1/market/%s: %d %v", sym, status, d)
	}
	if cc := hdr.Get("Cache-Control"); cc != "public, max-age=30" {
		t.Errorf("cache-control: %q", cc)
	}
	if d["symbol"] != sym || d["market_cap_kobo"] != wantCap || d["holders"] != float64(2) {
		t.Errorf("record: %v", d)
	}
	if n := len(d["sessions"].([]any)); n != 1 {
		t.Errorf("sessions: %d, want 1", n)
	} else if s := d["sessions"].([]any)[0].(map[string]any); s["date"] != testDate || s["state"] != "published" || s["zero_volume"] != true {
		t.Errorf("session: %v", s)
	}
	if n := len(d["prices"].([]any)); n < 1 {
		t.Errorf("prices: %d, want at least 1", n)
	} else if p := d["prices"].([]any)[0].(map[string]any); p["adjusted_price_kobo"] != float64(4000) || p["source"] != "carry_forward" {
		t.Errorf("price: %v", p)
	}
	co := d["company"].(map[string]any)
	if co["legal_name"] != "Mama Put Kitchens Ltd" || co["trading_name"] != "Mama Put" || co["mcc"] != "5812" || co["rc_number"] == nil {
		t.Errorf("company: %v", co)
	}
	ct := d["cap_table"].(map[string]any)
	if ct["shares_authorised_units"] != float64(1_000_000_000_000_000) || ct["shares_in_issue_units"] != float64(800_000_000_000_000) ||
		ct["daily_release_units"] != float64(50_000_000_000_000) || ct["treasury_units"] != float64(130_000_000_000_000-12_500_000) ||
		ct["on_register_units"] != float64(50_000_000_000_000+12_500_000) {
		t.Errorf("cap table: %v", ct)
	}
	if _, ok := d["corporate_actions"].([]any); !ok {
		t.Errorf("corporate_actions: %v", d["corporate_actions"])
	}
	if _, ok := d["disclosures"].([]any); !ok {
		t.Errorf("disclosures: %v", d["disclosures"])
	}

	if status, _, _ := c.get(t, "/v1/market/NOPE"); status != http.StatusNotFound {
		t.Errorf("unknown symbol: %d, want 404", status)
	}

	// The page, at both paths, with no credential.
	for _, path := range []string{"/market", "/market/" + sym} {
		resp, err := http.Get(c.srv.URL + path)
		if err != nil || resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
			t.Errorf("page %s: %v %v", path, err, resp)
		}
	}
}

func TestPublicMarketHidesUnpublished(t *testing.T) {
	p := pool(t)
	c := newClient(t, p)
	sym := seed(t, c)
	ctx := context.Background()
	if _, err := p.Exec(ctx, `
		INSERT INTO disclosures (instrument_id, kind, headline, body, submitted_by, submitted_at, published_at)
		SELECT id, 'results', 'Half-year results', 'body', 'cfo', now() - interval '1 hour', now() FROM instruments WHERE symbol = $1`, sym); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `
		INSERT INTO disclosures (instrument_id, kind, headline, body, submitted_by, submitted_at)
		SELECT id, 'material_event', 'Not yet public', 'body', 'cfo', now() FROM instruments WHERE symbol = $1`, sym); err != nil {
		t.Fatal(err)
	}
	_, _, d := c.get(t, "/v1/market/"+sym)
	ds := d["disclosures"].([]any)
	if len(ds) != 1 || ds[0].(map[string]any)["headline"] != "Half-year results" {
		t.Errorf("disclosures: %v, want only the published one", ds)
	}
	if _, ok := ds[0].(map[string]any)["body"]; ok {
		t.Errorf("disclosure exposes its body")
	}
}
