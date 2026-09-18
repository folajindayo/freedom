package httpapi

import (
	"bytes"
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

	"github.com/jackc/pgx/v5/pgxpool"

	"freedom/api/internal/migrate"
	"freedom/api/internal/rail"
	"freedom/api/internal/scheme"
)

// The wire: the token gate, the status codes the contract names, and that the
// JSON shapes carry the fields Tapp reads. The semantics are tested in rail.

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
}

func newClient(t *testing.T, p *pgxpool.Pool) *client {
	t.Helper()
	svc := rail.New(p)
	at := time.Date(2026, 9, 18, 11, 0, 0, 0, scheme.Lagos)
	svc.Now = func() time.Time { return at }
	api, err := New(svc, "secret-token")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.Routes())
	t.Cleanup(srv.Close)
	return &client{srv: srv}
}

func (c *client) do(t *testing.T, method, path string, body any, token string) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, c.srv.URL+path, &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// The symbol is random, as in e2e: the database is shared and the schema
// package assumes the doc's example symbol is free.
func business(symbol string) map[string]any {
	return map[string]any{
		"merchant_ref": "sp_1", "cardholder_ref": "usr_owner",
		"legal_name": "Mama Put Kitchens Ltd", "trading_name": "Mama Put",
		"rc_number": "RC1483920", "mcc": "5812", "symbol": symbol,
		"evidence": map[string]any{
			"trading_months": 30, "audited_accounts": true, "auditor_on_list": true,
			"shares_in_issue": 800000000000000, "public_shares": 120000000000000,
			"holders": 31, "treasury_units": 180000000000000,
			"board_resolution": true, "directors_clear": true,
		},
		"reference_price_kobo": 4000, "shares_authorised_units": 1000000000000000,
		"daily_release_units": 50000000000000, "cofund_bps": 0,
		"holders": []map[string]any{{"cardholder_ref": "usr_owner", "units": 50000000000000, "label": "founder"}},
	}
}

func TestRailRequiresTheToken(t *testing.T) {
	if _, err := New(rail.New(nil), ""); err == nil {
		t.Fatal("the rail mounted without a token")
	}
	c := newClient(t, pool(t))
	if status, _ := c.do(t, "GET", "/market/today", nil, ""); status != http.StatusUnauthorized {
		t.Errorf("no token: %d, want 401", status)
	}
	if status, _ := c.do(t, "GET", "/market/today", nil, "wrong"); status != http.StatusUnauthorized {
		t.Errorf("wrong token: %d, want 401", status)
	}
	if status, _ := c.do(t, "GET", "/market/today", nil, "secret-token"); status != http.StatusOK {
		t.Errorf("right token: %d, want 200", status)
	}
}

func TestRailStatusCodes(t *testing.T) {
	c := newClient(t, pool(t))
	const tok = "secret-token"
	sym := "MAMA" + strings.ToUpper(randHex(3))

	status, body := c.do(t, "POST", "/businesses", business(sym), tok)
	if status != http.StatusCreated || body["state"] != "listed" || body["instrument_id"] != "EQ:"+sym {
		t.Fatalf("onboard: %d %v", status, body)
	}
	if status, body = c.do(t, "POST", "/businesses", business(sym), tok); status != http.StatusOK || body["state"] != "listed" {
		t.Errorf("onboard replay: %d %v", status, body)
	}

	branch := business("BRANCH" + strings.ToUpper(randHex(2)))
	branch["merchant_ref"] = "sp_1_branch"
	if status, body = c.do(t, "POST", "/businesses", branch, tok); status != http.StatusConflict ||
		body["error"] != "already_listed" || body["symbol"] != sym {
		t.Errorf("second merchant of a listed company: %d %v", status, body)
	}

	young := business("YOUNG" + strings.ToUpper(randHex(2)))
	young["merchant_ref"], young["rc_number"] = "sp_2", "RC"+randHex(4)
	young["evidence"].(map[string]any)["trading_months"] = 3
	if status, body = c.do(t, "POST", "/businesses", young, tok); status != http.StatusOK || body["state"] != "rejected" {
		t.Errorf("rejection: %d %v", status, body)
	}

	if status, _ = c.do(t, "GET", "/businesses/sp_nobody", nil, tok); status != http.StatusNotFound {
		t.Errorf("unknown business: %d, want 404", status)
	}
	if status, body = c.do(t, "GET", "/businesses/sp_1", nil, tok); status != http.StatusOK || body["holders"] != float64(1) {
		t.Errorf("get business: %d %v", status, body)
	}

	tap := map[string]any{"tap_ref": "tap_1", "merchant_ref": "sp_1", "cardholder_ref": "usr_ada",
		"cardholder_display_name": "Ada", "amount_kobo": 1000000, "charged_at": "2026-09-18T11:24:03Z"}
	status, body = c.do(t, "POST", "/taps", tap, tok)
	if status != http.StatusCreated || body["intent_state"] != "pending" || body["fee_kobo"] != float64(5000) ||
		body["buyback_funding_kobo"] != float64(500) || body["symbol"] != sym {
		t.Fatalf("tap: %d %v", status, body)
	}
	if status, _ = c.do(t, "POST", "/taps", tap, tok); status != http.StatusOK {
		t.Errorf("tap replay: %d, want 200", status)
	}
	if status, _ = c.do(t, "POST", "/taps", map[string]any{"tap_ref": "", "amount_kobo": 5}, tok); status != http.StatusBadRequest {
		t.Errorf("bad tap: %d, want 400", status)
	}

	status, body = c.do(t, "POST", "/market/close", map[string]any{"session_date": "2026-09-18"}, tok)
	if status != http.StatusOK {
		t.Fatalf("close: %d %v", status, body)
	}
	rows := body["instruments"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["state"] != "published" {
		t.Errorf("close rows: %v", rows)
	}

	status, body = c.do(t, "GET", "/cardholders/usr_ada/holdings", nil, tok)
	if status != http.StatusOK || body["total_value_kobo"] != float64(500) {
		t.Errorf("holdings: %d %v", status, body)
	}
	if status, _ = c.do(t, "GET", "/cardholders/usr_ada/holdings/"+sym, nil, tok); status != http.StatusOK {
		t.Errorf("holding: %d", status)
	}
	if status, _ = c.do(t, "GET", "/cardholders/usr_ada/holdings/NOPE", nil, tok); status != http.StatusNotFound {
		t.Errorf("unknown holding: %d, want 404", status)
	}
	if status, _ = c.do(t, "GET", "/cardholders/usr_nobody/holdings", nil, tok); status != http.StatusNotFound {
		t.Errorf("unknown cardholder: %d, want 404", status)
	}
	if status, body = c.do(t, "GET", "/cardholders/usr_ada/activity?limit=5", nil, tok); status != http.StatusOK || len(body["activity"].([]any)) != 1 {
		t.Errorf("activity: %d %v", status, body)
	}
	if status, body = c.do(t, "GET", "/businesses/sp_1/holders?limit=1", nil, tok); status != http.StatusOK || body["next_cursor"] == "" {
		t.Errorf("holders page: %d %v", status, body)
	}

	status, body = c.do(t, "POST", "/taps/tap_1/reverse", map[string]any{"reason": "merchant_reversal"}, tok)
	if status != http.StatusOK || body["state"] != "reversed" || body["unwound_units"] != float64(12500000) {
		t.Errorf("reverse: %d %v", status, body)
	}
	if status, _ = c.do(t, "POST", "/taps/tap_none/reverse", nil, tok); status != http.StatusNotFound {
		t.Errorf("reverse unknown: %d, want 404", status)
	}
}
