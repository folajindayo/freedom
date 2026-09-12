package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"freedom/api/internal/clearing"
	"freedom/api/internal/exchange"
	"freedom/api/internal/money"
	"freedom/api/internal/share"
)

// A company that fails needs to know which criteria it failed. An exchange that
// says only "rejected" cannot defend the decision later.
func TestAdmissionRecordsEveryFindingSeparately(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	setup(t, p)

	var company uuid.UUID
	if err := p.QueryRow(ctx,
		`INSERT INTO companies (legal_name) VALUES ('Shoprite Kano Ltd') RETURNING id`).
		Scan(&company); err != nil {
		t.Fatal(err)
	}

	var app uuid.UUID
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		app, err = exchange.Apply(ctx, tx, company, "SHOPKANO", nil, nil)
		return err
	})

	// A young business with no float, no sponsor and no audit.
	var assessed exchange.Application
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		assessed, err = exchange.Assess(ctx, tx, app, exchange.StandardCriteria(), exchange.Evidence{
			TradingMonths: 8,
			SharesInIssue: share.Whole(1_000),
			PublicShares:  share.Whole(20), // 2%
			Holders:       4,
		})
		return err
	})

	if assessed.Passes() {
		t.Fatal("a company with 8 months of trading and a 2% float was admitted")
	}
	failed := map[string]string{}
	for _, f := range assessed.Findings {
		if !f.Met {
			failed[f.Criterion] = f.Detail
		}
	}
	for _, want := range []string{"trading_history", "free_float", "holders", "sponsor",
		"audited_accounts", "treasury_pool", "directors"} {
		if _, ok := failed[want]; !ok {
			t.Errorf("%s should have failed", want)
		}
	}
	t.Logf("refused on %d criteria; free float: %s", len(failed), failed["free_float"])

	// The criteria are published, so admitting against them would make them
	// advisory.
	if err := inTx(p, func(tx pgx.Tx) error {
		_, err := exchange.AdmitListing(ctx, tx, assessed, money.Naira(40),
			share.Whole(10_000), share.Whole(100), "listings.committee")
		return err
	}); err == nil {
		t.Fatal("a failing application was admitted")
	}

	mustTx(t, p, func(tx pgx.Tx) error {
		return exchange.Reject(ctx, tx, app, "listings.committee",
			"insufficient trading history and free float")
	})
	if err := inTx(p, func(tx pgx.Tx) error {
		return exchange.Reject(ctx, tx, app, "listings.committee", "again")
	}); err == nil {
		t.Error("a decided application was rejected again")
	}
}

// The whole point: a merchant that lists can finally be bought back.
func TestAdmissionListsAMerchantAndUnblocksItsBuyback(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)

	// A second merchant with no company, so its buybacks escrow.
	var company, merchant uuid.UUID
	if err := p.QueryRow(ctx,
		`INSERT INTO companies (legal_name) VALUES ('Buka Express Ltd') RETURNING id`).Scan(&company); err != nil {
		t.Fatal(err)
	}
	if err := p.QueryRow(ctx, `
		INSERT INTO merchants (acquirer_id, legal_name, trading_name, mcc, kyb_status)
		VALUES ($1,'Buka Express Ltd','Buka Express','5812','verified') RETURNING id`,
		n.participantID).Scan(&merchant); err != nil {
		t.Fatal(err)
	}

	sponsor, _, _ := membership(t, p, n, n.cardholderID, newCardholder(t, p, "Sponsor Rep"))

	var app uuid.UUID
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		app, err = exchange.Apply(ctx, tx, company, "BUKA", &merchant, &sponsor)
		return err
	})

	var assessed exchange.Application
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		assessed, err = exchange.Assess(ctx, tx, app, exchange.StandardCriteria(), exchange.Evidence{
			TradingMonths: 36, AuditedAccounts: true, AuditorOnList: true,
			SharesInIssue: share.Whole(10_000), PublicShares: share.Whole(1_500),
			Holders: 40, TreasuryUnits: share.Whole(2_000),
			BoardResolution: true, DirectorsClear: true,
		})
		return err
	})
	if !assessed.Passes() {
		t.Fatalf("a compliant applicant was refused: %+v", assessed.Findings)
	}

	var instrument string
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		instrument, err = exchange.AdmitListing(ctx, tx, assessed, money.Naira(25),
			share.Whole(10_000), share.Whole(100), "listings.committee")
		return err
	})
	if instrument != "EQ:BUKA" {
		t.Fatalf("listed as %s, want EQ:BUKA", instrument)
	}

	// The merchant is now linked, which is what lets a tap there buy equity.
	var linked *uuid.UUID
	if err := p.QueryRow(ctx,
		`SELECT company_id FROM merchants WHERE id = $1`, merchant).Scan(&linked); err != nil {
		t.Fatal(err)
	}
	if linked == nil || *linked != company {
		t.Fatal("the merchant was not linked to its listed company")
	}
	t.Logf("BUKA admitted at %s; its taps can now buy equity", money.Naira(25))
}

func TestApplicationValidation(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)

	var company uuid.UUID
	if err := p.QueryRow(ctx,
		`INSERT INTO companies (legal_name) VALUES ('Test Ltd') RETURNING id`).Scan(&company); err != nil {
		t.Fatal(err)
	}
	for _, sym := range []string{"AB", "9START", "HAS SPACE", "WAYTOOLONGSYMBOL"} {
		if err := inTx(p, func(tx pgx.Tx) error {
			_, err := exchange.Apply(ctx, tx, company, sym, nil, nil)
			return err
		}); err == nil {
			t.Errorf("symbol %q was accepted", sym)
		}
	}

	// Case is normalised rather than refused. Tickers are conventionally
	// uppercase and accepting "buka" for BUKA changes nothing about what the
	// applicant meant — unlike a price, where rounding decides it for them.
	var app uuid.UUID
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		app, err = exchange.Apply(ctx, tx, company, "  buka2  ", nil, nil)
		return err
	})
	var stored string
	if err := p.QueryRow(ctx,
		`SELECT proposed_symbol FROM listing_applications WHERE id = $1`, app).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "BUKA2" {
		t.Errorf("symbol stored as %q, want BUKA2", stored)
	}
	// An already-listed symbol cannot be applied for.
	if err := inTx(p, func(tx pgx.Tx) error {
		_, err := exchange.Apply(ctx, tx, company, n.symbol, nil, nil)
		return err
	}); err == nil {
		t.Error("a listed symbol was accepted for a new application")
	}
}

// Netting is the whole reason a scheme sits between two banks: a member that
// acquired ₦40m and issued ₦38m settles ₦2m, not two gross flows.
func TestNetSettlementPositions(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)
	tap(t, p, n)

	var batch clearing.Batch
	var positions []clearing.Position
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		batch, err = clearing.OpenBatch(ctx, tx, sessionDate)
		if err != nil {
			return err
		}
		positions, err = clearing.ComputePositions(ctx, tx, batch)
		return err
	})

	if len(positions) != 1 {
		t.Fatalf("%d positions, want 1 — Freedom is both acquirer and issuer", len(positions))
	}
	// Wearing both roles, the position nets to nothing.
	if positions[0].Net != 0 {
		t.Fatalf("net position %s, want zero when one participant is both sides", positions[0].Net)
	}
	if positions[0].Acquired != money.Naira(10_000) || positions[0].Issued != money.Naira(10_000) {
		t.Errorf("gross: acquired %s, issued %s", positions[0].Acquired, positions[0].Issued)
	}
	t.Logf("%s: acquired %s, issued %s, net %s",
		positions[0].Code, positions[0].Acquired, positions[0].Issued, positions[0].Net)

	var instructed, held int
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		instructed, held, err = clearing.Instruct(ctx, tx, batch)
		return err
	})
	// A zero position needs no instruction; it is already settled.
	if instructed != 0 || held != 0 {
		t.Errorf("instructed %d, held %d — a net-zero position needs neither", instructed, held)
	}
}

// The scheme refusing to settle an over-cap position is what stops one member's
// failure from becoming every member's loss — and it only works if the refusal
// happens before the instruction goes out.
func TestNetDebitCapHoldsSettlement(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)

	// A second participant issuing the card, so the position does not net out.
	var issuer uuid.UUID
	if err := p.QueryRow(ctx, `
		INSERT INTO participants (code, legal_name, roles, status, net_debit_cap_kobo)
		VALUES ($1,'Small Bank Plc',ARRAY['issuer'],'active',$2) RETURNING id`,
		"SMALL"+randHex(3), int64(money.Naira(1_000))).Scan(&issuer); err != nil {
		t.Fatal(err)
	}
	mustExec(t, p, `UPDATE cards SET issuer_id = $2 WHERE id = $1`, n.cardID, issuer)

	tap(t, p, n)

	var batch clearing.Batch
	var positions []clearing.Position
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		batch, err = clearing.OpenBatch(ctx, tx, sessionDate)
		if err != nil {
			return err
		}
		positions, err = clearing.ComputePositions(ctx, tx, batch)
		return err
	})

	if len(positions) != 2 {
		t.Fatalf("%d positions, want 2", len(positions))
	}
	var sum money.Kobo
	var capped *clearing.Position
	for i := range positions {
		sum += positions[i].Net
		if positions[i].State == "capped" {
			capped = &positions[i]
		}
	}
	// Every naira one participant owes is a naira another is owed.
	if sum != 0 {
		t.Fatalf("positions sum to %s, want zero", sum)
	}
	if capped == nil {
		t.Fatal("a ₦10,000 debit against a ₦1,000 cap was not held")
	}
	if capped.CapBreach != money.Naira(9_000) {
		t.Errorf("breach = %s, want ₦9,000.00", capped.CapBreach)
	}
	t.Logf("%s owes %s against a ₦1,000.00 cap — held, breach %s",
		capped.Code, -capped.Net, capped.CapBreach)

	var instructed, held int
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		instructed, held, err = clearing.Instruct(ctx, tx, batch)
		return err
	})
	if held != 1 {
		t.Fatalf("held %d positions, want 1 — instructing anyway would be theatre", held)
	}
	if instructed != 1 {
		t.Errorf("instructed %d, want the uncapped counterparty", instructed)
	}

	// Releasing takes a name, because somebody decided the collateral is there.
	var positionID uuid.UUID
	if err := p.QueryRow(ctx,
		`SELECT id FROM settlement_positions WHERE state = 'capped'`).Scan(&positionID); err != nil {
		t.Fatal(err)
	}
	if err := inTx(p, func(tx pgx.Tx) error {
		return clearing.ReleaseCap(ctx, tx, positionID, "")
	}); err == nil {
		t.Error("a capped position was released with no name attached")
	}
	mustTx(t, p, func(tx pgx.Tx) error {
		return clearing.ReleaseCap(ctx, tx, positionID, "risk.committee")
	})
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		instructed, held, err = clearing.Instruct(ctx, tx, batch)
		return err
	})
	if held != 0 || instructed != 1 {
		t.Fatalf("after release: instructed %d, held %d", instructed, held)
	}
}

func TestPositionsMustSumToZero(t *testing.T) {
	// Not a database test: it is the invariant itself that matters. Every naira
	// one participant owes is a naira another is owed, and a non-zero sum means
	// the batch invented or destroyed money.
	if !strings.Contains(clearing.ErrPositionsUnbalanced.Error(), "sum to zero") {
		t.Fatal("the unbalanced-positions error does not say what is wrong")
	}
}
