// Package schema tests the invariants the database enforces on its own.
//
// A constraint that is never exercised is a constraint nobody knows is missing.
// Each test here corresponds to a failure mode that is expensive or impossible
// to fix after the network carries real traffic.
package schema

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"freedom/api/internal/migrate"
)

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
	return p
}

func participant(t *testing.T, p *pgxpool.Pool, code string) string {
	t.Helper()
	var id string
	err := p.QueryRow(context.Background(), `
		INSERT INTO participants (code, legal_name, roles, status)
		VALUES ($1, $1, ARRAY['issuer'], 'active') RETURNING id`, code).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// Two issuers claiming the same PAN prefix is a routine incident on real
// networks: traffic then routes by whichever row the planner returned.
func TestOverlappingBINRangesAreImpossible(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	a := participant(t, p, "BINA"+randSuffix())
	b := participant(t, p, "BINB"+randSuffix())

	pad := func(s string) string { return s + strings.Repeat("0", 19-len(s)) }

	_, err := p.Exec(ctx, `
		INSERT INTO bin_ranges (low, high, issuer_id, product_code, effective)
		VALUES ($1, $2, $3, 'classic', daterange('2026-01-01','2030-01-01'))`,
		pad("5061"), pad("5062"), a)
	if err != nil {
		t.Fatal(err)
	}

	// An overlapping range for a different issuer, same dates.
	_, err = p.Exec(ctx, `
		INSERT INTO bin_ranges (low, high, issuer_id, product_code, effective)
		VALUES ($1, $2, $3, 'classic', daterange('2026-01-01','2030-01-01'))`,
		pad("50615"), pad("50616"), b)
	if err == nil {
		t.Fatal("an overlapping BIN range was accepted")
	}
	if !strings.Contains(err.Error(), "exclusion") && !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("want an exclusion-constraint violation, got %v", err)
	}

	// The same range in a non-overlapping period is legitimate: this is how a
	// BIN is transferred between issuers.
	if _, err := p.Exec(ctx, `
		INSERT INTO bin_ranges (low, high, issuer_id, product_code, effective)
		VALUES ($1, $2, $3, 'classic', daterange('2030-01-01','2035-01-01'))`,
		pad("5061"), pad("5062"), b); err != nil {
		t.Fatalf("a later, non-overlapping period must be allowed: %v", err)
	}
}

// Equity may not be allocated to someone who has not been verified and has not
// accepted the risk disclosure. Retrofitting this means unwinding shares
// already granted to people who were never eligible.
func TestEquityAllocationGate(t *testing.T) {
	p := pool(t)
	ctx := context.Background()

	var id string
	if err := p.QueryRow(ctx, `
		INSERT INTO cardholders (phone, display_name) VALUES ($1, 'Test Holder') RETURNING id`,
		"+234"+randSuffix()).Scan(&id); err != nil {
		t.Fatal(err)
	}

	check := func() bool {
		var ok bool
		if err := p.QueryRow(ctx, `SELECT equity_allocation_permitted($1)`, id).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}

	if check() {
		t.Fatal("a brand-new cardholder must not be eligible for equity")
	}

	for _, step := range []struct {
		name string
		sql  string
		want bool
	}{
		{"KYC tier alone is not enough", `UPDATE cardholders SET kyc_tier = 1 WHERE id = $1`, false},
		{"BVN alone is not enough", `UPDATE cardholders SET bvn_verified_at = now() WHERE id = $1`, false},
		{"disclosure completes it", `UPDATE cardholders SET disclosure_accepted_at = now(), disclosure_version = 'v1' WHERE id = $1`, true},
		{"freezing revokes it", `UPDATE cardholders SET status = 'frozen' WHERE id = $1`, false},
	} {
		if _, err := p.Exec(ctx, step.sql, id); err != nil {
			t.Fatal(err)
		}
		if got := check(); got != step.want {
			t.Errorf("%s: eligible = %v, want %v", step.name, got, step.want)
		}
	}
}

// A terminal retrying a timed-out tap resends the same STAN. That must resolve
// to one authorisation, not two.
func TestDuplicateSTANIsRejected(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	f := newFixture(t, p)

	insert := func(stan string) error {
		_, err := p.Exec(ctx, `
			INSERT INTO authorizations
			  (card_id, credential_id, terminal_id, merchant_id, acquirer_id, issuer_id,
			   stan, rrn, requested_kobo, approved_kobo, outstanding_kobo, result,
			   fee_schedule_version, counter, business_date, expires_at)
			VALUES ($1,$2,$3,$4,$5,$5,$6,'RRN000000001',100000,100000,100000,'approved',
			        1,1,'2026-09-11', now() + interval '7 days')`,
			f.cardID, f.credentialID, f.terminalID, f.merchantID, f.participantID, stan)
		return err
	}

	if err := insert("000123"); err != nil {
		t.Fatal(err)
	}
	if err := insert("000123"); err == nil {
		t.Fatal("a replayed STAN created a second authorisation")
	}
	if err := insert("000124"); err != nil {
		t.Fatalf("a different STAN must be accepted: %v", err)
	}
}

// A presentment may only fund one buyback intent, no matter how many times a
// half-finished clearing run is retried.
func TestBuybackIntentIsExactlyOncePerPresentment(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	f := newFixture(t, p)

	var batchID string
	if err := p.QueryRow(ctx, `
		INSERT INTO clearing_batches (business_date, cutoff_at) VALUES ('2026-09-11', now())
		ON CONFLICT (business_date, cycle) DO UPDATE SET cutoff_at = EXCLUDED.cutoff_at
		RETURNING id`).Scan(&batchID); err != nil {
		t.Fatal(err)
	}

	var presentmentID string
	if err := p.QueryRow(ctx, `
		INSERT INTO presentments (merchant_id, card_id, kind, amount_kobo, arn, fee_schedule_version)
		VALUES ($1,$2,'first',1000000,$3,1) RETURNING id`,
		f.merchantID, f.cardID, "ARN"+randSuffix()).Scan(&presentmentID); err != nil {
		t.Fatal(err)
	}

	insert := func() error {
		_, err := p.Exec(ctx, `
			INSERT INTO buyback_intents
			  (presentment_id, clearing_batch_id, merchant_id, cardholder_id, funding_kobo, funding_breakdown)
			VALUES ($1,$2,$3,$4,500,'{"scheme":500}'::jsonb)`,
			presentmentID, batchID, f.merchantID, f.cardholderID)
		return err
	}
	if err := insert(); err != nil {
		t.Fatal(err)
	}
	if err := insert(); err == nil {
		t.Fatal("a re-run clearing batch created a second buyback intent for one presentment")
	}
}

// An instrument's symbol is a public identifier and must not be free text.
func TestInstrumentSymbolFormat(t *testing.T) {
	p := pool(t)
	ctx := context.Background()

	var companyID string
	if err := p.QueryRow(ctx,
		`INSERT INTO companies (legal_name) VALUES ('Test Co') RETURNING id`).Scan(&companyID); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		symbol string
		ok     bool
	}{
		{"MAMAPUT", true},
		{"ABC", true},
		{"AB", false},        // too short
		{"lowercase", false}, // must be upper
		{"9START", false},    // must begin with a letter
		{"HAS SPACE", false},
	} {
		asset := "EQ:" + tc.symbol
		_, _ = p.Exec(ctx, `INSERT INTO assets (id,class,scale,label) VALUES ($1,'equity',8,$1)
			ON CONFLICT DO NOTHING`, asset)
		_, err := p.Exec(ctx, `
			INSERT INTO instruments (id, symbol, company_id, shares_authorised_units)
			VALUES ($1,$2,$3,100000000000)`, asset, tc.symbol, companyID)
		if tc.ok && err != nil {
			t.Errorf("symbol %q should be valid: %v", tc.symbol, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("symbol %q should be rejected", tc.symbol)
		}
		_, _ = p.Exec(ctx, `DELETE FROM instruments WHERE id = $1`, asset)
	}
}
