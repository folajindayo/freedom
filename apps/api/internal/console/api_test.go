package console

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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"freedom/api/internal/institution"
	"freedom/api/internal/migrate"
	"freedom/api/internal/money"
	"freedom/api/internal/public"
	"freedom/api/internal/rail"
	"freedom/api/internal/scheme"
)

// The console over a real database: seed one listed business through the
// rail, tap it, close the market, and read what the console shows about it.
// Then the action round trips.

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
	s   *Server
}

func newClient(t *testing.T, p *pgxpool.Pool) *client {
	t.Helper()
	svc := rail.New(p)
	at := time.Date(2026, 9, 18, 11, 0, 0, 0, scheme.Lagos)
	svc.Now = func() time.Time { return at }
	s, err := New(p, svc, "console-secret")
	if err != nil {
		t.Fatal(err)
	}
	s.Now = svc.Now
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	return &client{srv: srv, svc: svc, s: s}
}

func (c *client) do(t *testing.T, method, path string, body any, token, by string) (int, map[string]any) {
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
	if by != "" {
		req.Header.Set("X-Operator", by)
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

func (c *client) get(t *testing.T, path string) map[string]any {
	t.Helper()
	status, body := c.do(t, "GET", path, nil, "console-secret", "")
	if status != http.StatusOK {
		t.Fatalf("GET %s: %d %v", path, status, body)
	}
	return body
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
	// (₦80m + 1.0 × ₦240m) / 8,000,000 shares: the exchange lists it at ₦40.
	r.Evidence.NetAssetsKobo, r.Evidence.RevenueKobo = 8_000_000_000, 24_000_000_000
	r.SharesAuthorisedUnits, r.DailyReleaseUnits = 1_000_000_000_000_000, 50_000_000_000_000
	r.Holders = append(r.Holders, struct {
		CardholderRef string `json:"cardholder_ref"`
		Units         int64  `json:"units"`
		Label         string `json:"label"`
	}{"usr_owner", 50_000_000_000_000, "founder"})
	return r
}

// seed onboards one business, taps it once and closes the market.
func seed(t *testing.T, c *client) string {
	t.Helper()
	ctx := context.Background()
	sym := "MAMA" + strings.ToUpper(randHex(3))
	out, _, err := c.svc.OnboardBusiness(ctx, mamaPut(sym))
	if err != nil || out.State != "listed" {
		t.Fatalf("onboard: %v %+v", err, out)
	}
	if _, _, err := c.svc.IngestTap(ctx, rail.Tap{TapRef: "tap_1", MerchantRef: "sp_1", CardholderRef: "usr_ada",
		CardholderDisplayName: "Ada", AmountKobo: money.Naira(10_000),
		ChargedAt: time.Date(2026, 9, 18, 10, 24, 3, 0, time.UTC)}); err != nil {
		t.Fatalf("tap: %v", err)
	}
	rows, err := c.svc.CloseMarket(ctx, testDate)
	if err != nil || len(rows) != 1 || rows[0].State != "published" {
		t.Fatalf("close: %v %+v", err, rows)
	}
	return sym
}

func TestConsoleRequiresTheToken(t *testing.T) {
	if _, err := New(nil, nil, ""); err == nil {
		t.Fatal("the console mounted without a token")
	}
	c := newClient(t, pool(t))
	if status, _ := c.do(t, "GET", "/api/overview", nil, "", ""); status != http.StatusUnauthorized {
		t.Errorf("no token: %d, want 401", status)
	}
	if status, _ := c.do(t, "GET", "/api/overview", nil, "wrong", ""); status != http.StatusUnauthorized {
		t.Errorf("wrong token: %d, want 401", status)
	}
	// The page itself is public; it is the sign-in card.
	resp, err := http.Get(c.srv.URL + "/")
	if err != nil || resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Errorf("page: %v %v", err, resp)
	}
}

func TestConsoleReadsTheMarket(t *testing.T) {
	c := newClient(t, pool(t))
	sym := seed(t, c)

	ov := c.get(t, "/api/overview")
	if ov["business_date"] != testDate || ov["phase"] != "accepting" {
		t.Errorf("overview date/phase: %v %v", ov["business_date"], ov["phase"])
	}
	counts := ov["counts"].(map[string]any)
	if counts["listed"] != float64(1) || counts["open_halts"] != float64(0) {
		t.Errorf("overview counts: %v", counts)
	}
	if counts["buyback_allocated_today_kobo"] != float64(500) {
		t.Errorf("buyback allocated today: %v, want 500", counts["buyback_allocated_today_kobo"])
	}
	positions := ov["positions"].([]any)
	if len(positions) != 1 || positions[0].(map[string]any)["settlement_kobo"] != float64(-5000) {
		t.Errorf("participant positions: %v", positions)
	}
	if _, ok := ov["queue"].([]any); !ok {
		t.Errorf("queue missing: %v", ov["queue"])
	}

	list := c.get(t, "/api/instruments")
	rows := list["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("instruments: %v", rows)
	}
	first := rows[0].(map[string]any)
	if first["symbol"] != sym || first["status"] != "listed" || first["halted"] != false ||
		first["treasury_units"] != float64(130_000_000_000_000-12_500_000) || first["holders"] != float64(2) {
		t.Errorf("instrument row: %v", first)
	}

	rec := c.get(t, "/api/instruments/"+sym)
	inst := rec["instrument"].(map[string]any)
	if inst["symbol"] != sym || inst["daily_release_units"] != float64(50_000_000_000_000) {
		t.Errorf("record: %v", inst)
	}
	liq := rec["liquidity"].(map[string]any)
	if liq["passes"] != false || len(liq["criteria"].([]any)) != 9 {
		t.Errorf("liquidity: %v", liq)
	}
	if n := len(rec["prices"].([]any)); n != 1 {
		t.Errorf("prices: %d rows, want 1", n)
	}
	if n := len(rec["holders"].([]any)); n != 2 {
		t.Errorf("holders: %d rows, want 2", n)
	}
	if status, _ := c.do(t, "GET", "/api/instruments/NOPE", nil, "console-secret", ""); status != http.StatusNotFound {
		t.Errorf("unknown instrument: %d, want 404", status)
	}

	sess := c.get(t, "/api/sessions?date="+testDate)
	srows := sess["rows"].([]any)
	if len(srows) != 1 {
		t.Fatalf("sessions: %v", srows)
	}
	sr := srows[0].(map[string]any)
	if sr["symbol"] != sym || sr["state"] != "published" || sr["zero_volume"] != true || sr["dark"] != false ||
		sr["engine_version"] != float64(1) || sr["book_hash"] == nil || sr["buyback_state"] == nil {
		t.Errorf("session row: %v", sr)
	}

	al := c.get(t, "/api/alerts")
	if _, ok := al["rows"].([]any); !ok {
		t.Errorf("alerts shape: %v", al)
	}
	if status, _ := c.do(t, "GET", "/api/alerts/999", nil, "console-secret", ""); status != http.StatusNotFound {
		t.Errorf("unknown alert: %d, want 404", status)
	}

	for _, path := range []string{"/api/orders", "/api/fills", "/api/halts", "/api/incidents", "/api/closed-periods",
		"/api/listings", "/api/gate", "/api/providers", "/api/buyback", "/api/members", "/api/corporate-actions",
		"/api/disclosures", "/api/calendar", "/api/index", "/api/complaints", "/api/protection", "/api/clock",
		"/api/settlement", "/api/recon"} {
		c.get(t, path)
	}
	bb := c.get(t, "/api/buyback?date="+testDate)
	if b := bb["batches"].([]any); len(b) != 1 || b[0].(map[string]any)["units_bought"] != float64(12_500_000) {
		t.Errorf("buyback batches: %v", b)
	}
	if g := c.get(t, "/api/gate")["rows"].([]any); len(g) != 1 || g[0].(map[string]any)["passes"] != false {
		t.Errorf("gate: %v", g)
	}
	if l := c.get(t, "/api/listings")["rows"].([]any); len(l) != 1 || l[0].(map[string]any)["state"] != "listed" {
		t.Errorf("listings: %v", l)
	}
}

func TestConsoleActions(t *testing.T) {
	p := pool(t)
	c := newClient(t, p)
	sym := seed(t, c)
	const tok = "console-secret"

	// Every action needs a name.
	if status, body := c.do(t, "POST", "/api/instruments/"+sym+"/halt", map[string]any{"reason": "regulatory"}, tok, ""); status != http.StatusBadRequest {
		t.Errorf("halt without operator: %d %v", status, body)
	}

	// Halt → release.
	status, body := c.do(t, "POST", "/api/instruments/"+sym+"/halt", map[string]any{"reason": "regulatory", "detail": "SEC query"}, tok, "ngozi")
	if status != http.StatusOK || body["halted"] != true || body["halt_reason"] != "regulatory" || body["halted_by"] != "ngozi" {
		t.Fatalf("halt: %d %v", status, body)
	}
	if status, body = c.do(t, "POST", "/api/instruments/"+sym+"/halt", map[string]any{"reason": "regulatory"}, tok, "ngozi"); status != http.StatusConflict {
		t.Errorf("second halt: %d %v", status, body)
	}
	if status, body = c.do(t, "POST", "/api/instruments/"+sym+"/halt", map[string]any{"reason": "because"}, tok, "ngozi"); status != http.StatusBadRequest {
		t.Errorf("bad reason: %d %v", status, body)
	}
	if ov := c.get(t, "/api/overview"); ov["counts"].(map[string]any)["open_halts"] != float64(1) {
		t.Errorf("open halts after halt: %v", ov["counts"])
	}
	h := c.get(t, "/api/halts")
	if open := h["extra"].(map[string]any)["open"].([]any); len(open) != 1 {
		t.Errorf("open halts list: %v", open)
	}
	status, body = c.do(t, "POST", "/api/instruments/"+sym+"/release", nil, tok, "ngozi")
	if status != http.StatusOK || body["halted"] != false {
		t.Fatalf("release: %d %v", status, body)
	}
	if status, body = c.do(t, "POST", "/api/instruments/"+sym+"/release", nil, tok, "ngozi"); status != http.StatusConflict {
		t.Errorf("release when not halted: %d %v", status, body)
	}
	var releasedBy string
	if err := p.QueryRow(context.Background(), `SELECT released_by FROM trading_halts LIMIT 1`).Scan(&releasedBy); err != nil || releasedBy != "ngozi" {
		t.Errorf("released_by: %q %v", releasedBy, err)
	}

	// Graduate refuses with the failures; demote refuses when auction-only.
	status, body = c.do(t, "POST", "/api/instruments/"+sym+"/graduate", nil, tok, "ngozi")
	if status != http.StatusUnprocessableEntity || body["code"] != "gate_failed" {
		t.Errorf("graduate: %d %v", status, body)
	}
	if status, body = c.do(t, "POST", "/api/instruments/"+sym+"/demote", nil, tok, "ngozi"); status != http.StatusConflict {
		t.Errorf("demote auction-only: %d %v", status, body)
	}

	// A disclosure halts the symbol; publishing lifts it.
	var instrumentID string
	if err := p.QueryRow(context.Background(), `SELECT id FROM instruments WHERE symbol = $1`, sym).Scan(&instrumentID); err != nil {
		t.Fatal(err)
	}
	tx, err := p.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	dID, err := institution.Submit(context.Background(), tx, instrumentID, institution.DisclosureResults, "H1 results", "Revenue up.", "secretary")
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := c.get(t, "/api/disclosures")["rows"].([]any); len(d) != 1 || d[0].(map[string]any)["holding_halt"] != true {
		t.Errorf("disclosures: %v", d)
	}
	status, body = c.do(t, "POST", "/api/disclosures/"+dID.String()+"/publish", nil, tok, "ngozi")
	if status != http.StatusOK || body["published_at"] == nil || body["pending"] != false {
		t.Fatalf("publish: %d %v", status, body)
	}
	if status, _ = c.do(t, "POST", "/api/disclosures/"+dID.String()+"/publish", nil, tok, "ngozi"); status != http.StatusConflict {
		t.Errorf("publish twice: %d", status)
	}
	if ov := c.get(t, "/api/overview"); ov["counts"].(map[string]any)["open_halts"] != float64(0) {
		t.Errorf("halt survived publish: %v", ov["counts"])
	}

	// An alert: triage then close.
	var alertID int64
	if err := p.QueryRow(context.Background(), `
		INSERT INTO surveillance_alerts (instrument_id, session_date, detection, severity, subject_group, evidence)
		VALUES ($1, $2::date, 'wash_trading', 'block', 'account:x', '{"net_bps": 12}') RETURNING id`, instrumentID, testDate).Scan(&alertID); err != nil {
		t.Fatal(err)
	}
	if ov := c.get(t, "/api/overview"); ov["counts"].(map[string]any)["open_block_alerts"] != float64(1) {
		t.Errorf("open block alerts: %v", ov["counts"])
	}
	status, body = c.do(t, "POST", "/api/alerts/1/triage", map[string]any{"assignee": "femi"}, tok, "ngozi")
	if status != http.StatusOK || body["state"] != "triaged" || body["assigned_to"] != "femi" {
		t.Fatalf("triage: %d %v", status, body)
	}
	if status, _ = c.do(t, "POST", "/api/alerts/1/close", map[string]any{"outcome": "whatever"}, tok, "ngozi"); status != http.StatusBadRequest {
		t.Errorf("bad outcome: %d", status)
	}
	status, body = c.do(t, "POST", "/api/alerts/1/close", map[string]any{"outcome": "false_positive", "notes": "same owner, two wallets"}, tok, "ngozi")
	if status != http.StatusOK || body["state"] != "false_positive" || body["closed_by"] != "ngozi" || body["evidence"] == nil {
		t.Fatalf("close alert: %d %v", status, body)
	}
	a := c.get(t, "/api/alerts/1")
	if a["notes"] != "same owner, two wallets" || a["evidence"].(map[string]any)["net_bps"] != float64(12) {
		t.Errorf("alert detail: %v", a)
	}

	// The member kill switch.
	var memberID string
	if err := p.QueryRow(context.Background(), `SELECT id FROM members LIMIT 1`).Scan(&memberID); err != nil {
		t.Fatal(err)
	}
	if status, body = c.do(t, "POST", "/api/members/"+memberID+"/halt", nil, tok, "ngozi"); status != http.StatusBadRequest {
		t.Errorf("member halt without reason: %d %v", status, body)
	}
	status, body = c.do(t, "POST", "/api/members/"+memberID+"/halt", map[string]any{"reason": "runaway algo"}, tok, "ngozi")
	if status != http.StatusOK || body["halted"] != true || !strings.Contains(body["halted_reason"].(string), "ngozi") {
		t.Fatalf("member halt: %d %v", status, body)
	}
	if m := c.get(t, "/api/members")["rows"].([]any); m[0].(map[string]any)["halted"] != true {
		t.Errorf("members list: %v", m)
	}
	status, body = c.do(t, "POST", "/api/members/"+memberID+"/release", nil, tok, "ngozi")
	if status != http.StatusOK || body["halted"] != false {
		t.Fatalf("member release: %d %v", status, body)
	}

	// The close, again: idempotent, one row per listed instrument.
	status, body = c.do(t, "POST", "/api/market/close", map[string]any{"session_date": testDate}, tok, "ngozi")
	if status != http.StatusOK {
		t.Fatalf("close: %d %v", status, body)
	}
	if rows := body["instruments"].([]any); len(rows) != 1 || rows[0].(map[string]any)["state"] != "published" {
		t.Errorf("close rows: %v", rows)
	}
	if status, body = c.do(t, "POST", "/api/market/close", map[string]any{"session_date": "yesterday"}, tok, "ngozi"); status != http.StatusBadRequest {
		t.Errorf("close bad date: %d %v", status, body)
	}
}

// A session still accepting shows only its order count.
func TestConsoleKeepsTheBookDark(t *testing.T) {
	p := pool(t)
	c := newClient(t, p)
	sym := seed(t, c)
	ctx := context.Background()
	var instrumentID string
	if err := p.QueryRow(ctx, `SELECT id FROM instruments WHERE symbol = $1`, sym).Scan(&instrumentID); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `
		INSERT INTO trading_calendar (session_date, is_trading, opens_at, freezes_at) VALUES ('2026-09-21', true, now(), now())`); err != nil {
		t.Fatal(err)
	}
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.svc.Engine.Open(ctx, tx, instrumentID, "2026-09-21"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	sess := c.get(t, "/api/sessions?date=2026-09-21")
	rows := sess["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("sessions: %v", rows)
	}
	r := rows[0].(map[string]any)
	if r["state"] != "accepting" || r["dark"] != true || r["orders"] != float64(0) || r["book_hash"] != nil || r["prev_reference_kobo"] != nil {
		t.Errorf("accepting session leaked: %v", r)
	}
	o := c.get(t, "/api/orders?date=2026-09-21")
	if o["extra"].(map[string]any)["dark_sessions"] != float64(1) {
		t.Errorf("orders extra: %v", o["extra"])
	}
	_ = pgx.ErrNoRows
}

// The declared share count is what the company is valued on; correcting it
// is an operator action that leaves a cap table event with a name on it.
func TestConsoleSetsSharesInIssue(t *testing.T) {
	p := pool(t)
	c := newClient(t, p)
	sym := seed(t, c)
	const tok = "console-secret"

	if status, _ := c.do(t, "POST", "/api/instruments/"+sym+"/shares-in-issue",
		map[string]any{"units": 12_000_000_000_000_000, "reason": ""}, tok, "ngozi"); status != http.StatusBadRequest {
		t.Fatalf("no reason: status %d, want 400", status)
	}
	status, body := c.do(t, "POST", "/api/instruments/"+sym+"/shares-in-issue",
		map[string]any{"units": 12_000_000_000_000_000, "reason": "declared at admission"}, tok, "ngozi")
	if status != http.StatusOK {
		t.Fatalf("status %d: %v", status, body)
	}
	// 120,000,000 shares at the ₦40 reference: ₦4.8bn, exactly.
	if got := body["market_cap_kobo"]; got != float64(480_000_000_000) {
		t.Errorf("market_cap_kobo = %v, want 480000000000", got)
	}
	var note string
	if err := p.QueryRow(context.Background(), `
		SELECT note FROM cap_table_events WHERE instrument_id = $1 ORDER BY id DESC LIMIT 1`,
		"EQ:"+sym).Scan(&note); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(note, "ngozi") || !strings.Contains(note, "declared at admission") {
		t.Errorf("cap table event note = %q, want the operator and the reason", note)
	}
}

// A listing admitted before the pricing rule is re-anchored under it: the
// reference becomes fair value over the shares in issue, as a manual
// observation, on the application, and in what the public sees.
func TestConsoleReanchorsTheListingPrice(t *testing.T) {
	p := pool(t)
	c := newClient(t, p)
	sym := seed(t, c)
	const tok = "console-secret"
	ctx := context.Background()
	body := map[string]any{"net_assets_kobo": 2_000_000_000, "revenue_kobo": 6_000_000_000, "reason": "listed before the pricing rule"}

	if status, _ := c.do(t, "POST", "/api/instruments/"+sym+"/reanchor",
		map[string]any{"net_assets_kobo": 2_000_000_000, "revenue_kobo": 6_000_000_000, "reason": ""}, tok, "ngozi"); status != http.StatusBadRequest {
		t.Fatalf("no reason: status %d, want 400", status)
	}
	// The seeded listing declared 8,000,000 shares; clear it to prove the
	// refusal, then set the count the example is priced on.
	if _, err := p.Exec(ctx, `UPDATE instruments SET shares_in_issue_units = 0 WHERE symbol = $1`, sym); err != nil {
		t.Fatal(err)
	}
	if status, b := c.do(t, "POST", "/api/instruments/"+sym+"/reanchor", body, tok, "ngozi"); status != http.StatusUnprocessableEntity {
		t.Fatalf("no shares in issue: status %d %v, want 422", status, b)
	}
	if status, b := c.do(t, "POST", "/api/instruments/"+sym+"/shares-in-issue",
		map[string]any{"units": 1_000_000_000_000_000, "reason": "declared at admission"}, tok, "ngozi"); status != http.StatusOK {
		t.Fatalf("shares in issue: %d %v", status, b)
	}

	status, row := c.do(t, "POST", "/api/instruments/"+sym+"/reanchor", body, tok, "ngozi")
	if status != http.StatusOK || row["reference_price_kobo"] != float64(800) || row["carry_forward_sessions"] != float64(0) {
		t.Fatalf("re-anchor: %d %v", status, row)
	}
	var source string
	var price, volume int64
	if err := p.QueryRow(ctx, `
		SELECT source, price_kobo, volume_units FROM price_observations
		 WHERE instrument_id = $1 AND source = 'manual' AND obs_date = $2::date`, "EQ:"+sym, testDate).
		Scan(&source, &price, &volume); err != nil {
		t.Fatalf("manual observation: %v", err)
	}
	if price != 800 || volume != 0 {
		t.Errorf("manual observation %s %d kobo %d units, want 800 and 0", source, price, volume)
	}
	var fair, listed int64
	var findings string
	if err := p.QueryRow(ctx, `
		SELECT fair_value_kobo, listing_price_kobo, findings::text FROM listing_applications
		 WHERE proposed_symbol = $1`, sym).Scan(&fair, &listed, &findings); err != nil {
		t.Fatal(err)
	}
	if fair != 8_000_000_000 || listed != 800 || !strings.Contains(findings, "re-anchored by ngozi: listed before the pricing rule") ||
		!strings.Contains(findings, "→ ₦8.00") {
		t.Errorf("application: fair %d listed %d findings %s", fair, listed, findings)
	}

	// The public sees the new price at once, and where it came from.
	ps := public.New(p, c.svc)
	ps.Now = c.s.Now
	pub := httptest.NewServer(ps.API())
	defer pub.Close()
	resp, err := http.Get(pub.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	resp.Body.Close()
	list, _ := m["instruments"].([]any)
	if len(list) != 1 {
		t.Fatalf("public market: %v", m)
	}
	if r := list[0].(map[string]any); r["price_kobo"] != float64(800) || r["price_source"] != "manual" || r["reference_price_kobo"] != float64(800) {
		t.Errorf("public row after re-anchor: %v", r)
	}

	// Not while a session is open: Monday's session is accepting.
	const monday = "2026-09-21"
	c.s.Now = func() time.Time { return time.Date(2026, 9, 21, 11, 0, 0, 0, scheme.Lagos) }
	if _, err := p.Exec(ctx, `
		INSERT INTO auctions (instrument_id, session_date, state, opens_at, freezes_at, prev_reference_kobo, rule)
		VALUES ($1, $2::date, 'accepting', now(), now() + interval '1 hour', 800, 'max_volume')`, "EQ:"+sym, monday); err != nil {
		t.Fatal(err)
	}
	if status, b := c.do(t, "POST", "/api/instruments/"+sym+"/reanchor", body, tok, "ngozi"); status != http.StatusConflict {
		t.Errorf("during accepting: status %d %v, want 409", status, b)
	}
}

// The market maker from the console: fund, place, appoint, quote, read the
// quote, close, read the price, terminate.
func TestConsoleRunsTheMarketMaker(t *testing.T) {
	p := pool(t)
	c := newClient(t, p)
	ctx := context.Background()
	const tok = "console-secret"

	sym := "MAMA" + strings.ToUpper(randHex(3))
	out, _, err := c.svc.OnboardBusiness(ctx, mamaPut(sym))
	if err != nil || out.State != "listed" {
		t.Fatalf("onboard: %v %+v", err, out)
	}

	// Capital: a balanced posting from the float.
	status, body := c.do(t, "POST", "/api/members/TAPP/fund", map[string]any{"amount_kobo": 20_000_000, "reason": "launch capital"}, tok, "ngozi")
	if status != http.StatusOK || body["available_kobo"] != float64(20_000_000) || body["ledger_tx_id"] == nil {
		t.Fatalf("fund: %d %v", status, body)
	}
	if status, body = c.do(t, "POST", "/api/members/TAPP/fund", map[string]any{"amount_kobo": 1000}, tok, "ngozi"); status != http.StatusBadRequest {
		t.Errorf("fund without a reason: %d %v", status, body)
	}
	if status, body = c.do(t, "POST", "/api/members/NOPE/fund", map[string]any{"amount_kobo": 1000, "reason": "x"}, tok, "ngozi"); status != http.StatusNotFound {
		t.Errorf("fund an unknown member: %d %v", status, body)
	}

	// Inventory: a block at the reference, refused before it is affordable.
	status, body = c.do(t, "POST", "/api/instruments/"+sym+"/place-with-market-maker", map[string]any{"units": 10_000_00000000, "reason": "too much"}, tok, "ngozi")
	if status != http.StatusConflict {
		t.Errorf("unaffordable placement: %d %v", status, body)
	}
	status, body = c.do(t, "POST", "/api/instruments/"+sym+"/place-with-market-maker", map[string]any{"units": 2_000_00000000, "reason": "launch inventory"}, tok, "ngozi")
	if status != http.StatusOK {
		t.Fatalf("place: %d %v", status, body)
	}
	placed := body["placement"].(map[string]any)
	if placed["price_kobo"] != float64(4000) || placed["cost_kobo"] != float64(8_000_000) || placed["held_units"] != float64(2_000_00000000) {
		t.Errorf("placement: %v", placed)
	}
	inst := body["instrument"].(map[string]any)
	if inst["treasury_units"] != float64(130_000_000_000_000-2_000_00000000) {
		t.Errorf("treasury after placement: %v", inst["treasury_units"])
	}

	// Appointment refuses a related party: mark the house account as one.
	if _, err := p.Exec(ctx, `
		INSERT INTO related_parties (instrument_id, account_id, group_key, relation, effective)
		SELECT $1, a.id, 'issuer-group', 'affiliate', daterange('2020-01-01','2099-01-01')
		  FROM accounts a JOIN cardholders ch ON ch.id = a.owner_id
		 WHERE ch.external_ref = $2 AND a.kind = 'stock_wallet' AND a.asset_id = $1`, "EQ:"+sym, rail.MarketMakerRef); err != nil {
		t.Fatal(err)
	}
	status, body = c.do(t, "POST", "/api/instruments/"+sym+"/market-maker", map[string]any{"member_code": "TAPP"}, tok, "ngozi")
	if status != http.StatusConflict || !strings.Contains(str(body["message"]), "independent") {
		t.Fatalf("appointing a related party: %d %v", status, body)
	}
	if _, err := p.Exec(ctx, `DELETE FROM related_parties`); err != nil {
		t.Fatal(err)
	}
	status, body = c.do(t, "POST", "/api/instruments/"+sym+"/market-maker", map[string]any{"member_code": "TAPP", "target_units": 4_000_00000000}, tok, "ngozi")
	if status != http.StatusOK || body["state"] != "active" || body["min_quote_kobo"] != float64(5_000_000) || body["member"] != "TAPP" {
		t.Fatalf("appoint: %d %v", status, body)
	}
	if status, body = c.do(t, "POST", "/api/instruments/"+sym+"/market-maker", map[string]any{"member_code": "TAPP"}, tok, "ngozi"); status != http.StatusConflict {
		t.Errorf("appointing twice: %d %v", status, body)
	}

	// The quote.
	status, body = c.do(t, "POST", "/api/market/quote", nil, tok, "ngozi")
	if status != http.StatusOK {
		t.Fatalf("quote: %d %v", status, body)
	}
	quotes := body["quotes"].([]any)
	if len(quotes) != 1 {
		t.Fatalf("quotes: %v", quotes)
	}
	q := quotes[0].(map[string]any)
	if q["symbol"] != sym || q["two_sided"] != true || q["bid_kobo"] != float64(4040) || q["ask_kobo"] != float64(4160) || q["skew_bps"] != float64(250) {
		t.Errorf("quote: %v", q)
	}
	rd := c.get(t, "/api/quotes?date="+testDate)
	rows := rd["rows"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["bid_state"] != "open" || rows[0].(map[string]any)["ask_state"] != "open" {
		t.Errorf("quotes read: %v", rows)
	}
	rec := c.get(t, "/api/instruments/"+sym)
	if today := rec["quote_today"].([]any); len(today) != 1 || today[0].(map[string]any)["centre_kobo"] != float64(4000) {
		t.Errorf("record quote_today: %v", rec["quote_today"])
	}
	// Close: nothing crosses, the mid is the price.
	status, body = c.do(t, "POST", "/api/market/close", nil, tok, "ngozi")
	if status != http.StatusOK {
		t.Fatalf("close: %d %v", status, body)
	}
	rec = c.get(t, "/api/instruments/"+sym)
	inst = rec["instrument"].(map[string]any)
	if inst["reference_price_kobo"] != float64(4100) || inst["last_obs_source"] != "quote" || inst["carry_forward_sessions"] != float64(0) {
		t.Errorf("after the close: ref %v source %v carried %v", inst["reference_price_kobo"], inst["last_obs_source"], inst["carry_forward_sessions"])
	}
	rows = c.get(t, "/api/quotes?date="+testDate)["rows"].([]any)
	if r := rows[0].(map[string]any); r["met"] != true || r["price_source"] != "quote" || r["session_price_kobo"] != float64(4100) || r["session_rule"] != "quote" {
		t.Errorf("quote after the close: %v", r)
	}
	// The public market row says where the price came from.
	pub := public.New(p, c.svc)
	pub.Now = c.svc.Now
	srv := httptest.NewServer(pub.API())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	var mkt map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&mkt)
	resp.Body.Close()
	list, _ := mkt["instruments"].([]any)
	if len(list) != 1 {
		t.Fatalf("public market: %v", mkt)
	}
	if r := list[0].(map[string]any); r["price_source"] != "quote" || r["price_kobo"] != float64(4100) {
		t.Errorf("public: %v %v", r["price_source"], r["price_kobo"])
	}

	// Terminate.
	status, body = c.do(t, "DELETE", "/api/instruments/"+sym+"/market-maker", map[string]any{"member_code": "TAPP", "reason": "pilot over"}, tok, "ngozi")
	if status != http.StatusOK {
		t.Fatalf("terminate: %d %v", status, body)
	}
	var state string
	if err := p.QueryRow(ctx, `SELECT state FROM liquidity_providers`).Scan(&state); err != nil || state != "terminated" {
		t.Errorf("provider state %s %v", state, err)
	}
	if status, body = c.do(t, "DELETE", "/api/instruments/"+sym+"/market-maker", map[string]any{"member_code": "TAPP"}, tok, "ngozi"); status != http.StatusNotFound {
		t.Errorf("terminating twice: %d %v", status, body)
	}
	// And re-appointable: the terminated period no longer overlaps.
	if status, body = c.do(t, "POST", "/api/instruments/"+sym+"/market-maker", map[string]any{"member_code": "TAPP", "from": "2026-09-19"}, tok, "ngozi"); status != http.StatusOK {
		t.Errorf("re-appoint: %d %v", status, body)
	}
}
