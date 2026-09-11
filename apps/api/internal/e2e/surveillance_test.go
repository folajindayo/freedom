package e2e

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"freedom/api/internal/exchange"
	"freedom/api/internal/money"
	"freedom/api/internal/share"
)

// The detection the engine's purity was worth paying for.
//
// A small participant who moves the clearing price by their mere presence is
// setting the price rather than taking it — and because Cross is a pure
// function, that is not a judgement call. Remove their orders, re-run the
// uncross, and read the difference.
func TestMarkTheAuctionIsDetectedExactly(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)

	seller := n.cardholderID
	giveShares(t, p, n, seller, share.Whole(100), money.Naira(30))
	honest := newCardholder(t, p, "Uche Nnamdi")
	marker := newCardholder(t, p, "Dapo Martins")
	fund(t, p, honest, money.Naira(50_000))
	fund(t, p, marker, money.Naira(50_000))

	member, sa, ha := membership(t, p, n, seller, honest)
	ma := clientFor(t, p, n, member, marker)

	// A book that clears 50 units at ₦38 on genuine interest alone: the seller
	// offers 50 at ₦38 and 50 more at ₦44, and one real buyer bids 50 at ₦44.
	// Both prices clear 50, and the smaller imbalance picks ₦38.
	//
	// Three units from the marker tip the balance: at ₦44 the book now clears
	// 53, which beats ₦38's 50 outright, and the print jumps six naira on 5.7%
	// of the volume.
	mustTx(t, p, func(tx pgx.Tx) error {
		e := exchange.NewEngine()
		s, err := e.Open(ctx, tx, n.instrumentID, tradeDate)
		if err != nil {
			return err
		}
		if _, err := e.Place(ctx, tx, s, order(member, sa, seller, exchange.Sell, money.Naira(38), share.Whole(50))); err != nil {
			return err
		}
		if _, err := e.Place(ctx, tx, s, order(member, sa, seller, exchange.Sell, money.Naira(44), share.Whole(50))); err != nil {
			return err
		}
		if _, err := e.Place(ctx, tx, s, order(member, ha, honest, exchange.Buy, money.Naira(44), share.Whole(50))); err != nil {
			return err
		}
		if _, err := e.Place(ctx, tx, s, order(member, ma, marker, exchange.Buy, money.Naira(44), share.Whole(3))); err != nil {
			return err
		}
		r, err := e.RunToSettlement(ctx, tx, s)
		if err != nil {
			return err
		}
		if r.Price != money.Naira(44) {
			t.Fatalf("the session cleared at %s; the marking scenario needs ₦44.00", r.Price)
		}
		return nil
	})

	var alerts []exchange.Alert
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		alerts, err = exchange.NewSurveillance().MarkTheAuction(ctx, tx, n.instrumentID, tradeDate)
		return err
	})

	var found *exchange.Alert
	for i := range alerts {
		if alerts[i].SubjectGroup == "account:"+marker.String() {
			found = &alerts[i]
		}
	}
	if found == nil {
		t.Fatalf("the marker was not detected; alerts raised: %+v", alerts)
	}
	t.Logf("marking detected: price moved %v bps on %v bps of volume",
		found.Evidence["moved_bps"], found.Evidence["volume_share_bps"])

	// And the honest counterparties, who supplied the volume, are not flagged —
	// a participant supplying most of a session's liquidity SHOULD move the
	// price. That is a market working.
	for _, a := range alerts {
		if a.SubjectGroup == "account:"+honest.String() || a.SubjectGroup == "account:"+seller.String() {
			t.Errorf("a genuine liquidity provider was flagged as marking: %+v", a.Evidence)
		}
	}
}

// Two accounts of one group trading with each other change no beneficial
// ownership. Gross activity high, net position flat.
func TestWashTradingIsDetected(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)

	washer := n.cardholderID
	giveShares(t, p, n, washer, share.Whole(20), money.Naira(30))
	fund(t, p, washer, money.Naira(50_000))
	other := newCardholder(t, p, "Femi Sanni")
	member, wa, _ := membership(t, p, n, washer, other)

	// The same account on both sides of the book at the same price.
	mustTx(t, p, func(tx pgx.Tx) error {
		e := exchange.NewEngine()
		s, err := e.Open(ctx, tx, n.instrumentID, tradeDate)
		if err != nil {
			return err
		}
		if _, err := e.Place(ctx, tx, s, order(member, wa, washer, exchange.Sell, money.Naira(40), share.Whole(10))); err != nil {
			return err
		}
		if _, err := e.Place(ctx, tx, s, order(member, wa, washer, exchange.Buy, money.Naira(40), share.Whole(10))); err != nil {
			return err
		}
		_, err = e.RunToSettlement(ctx, tx, s)
		return err
	})

	var alerts []exchange.Alert
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		alerts, err = exchange.NewSurveillance().WashTrading(ctx, tx, n.instrumentID, tradeDate)
		return err
	})
	if len(alerts) == 0 {
		t.Fatal("a self-crossing account was not detected as washing")
	}
	if alerts[0].Severity != exchange.SevBlock {
		t.Errorf("severity = %s, want block", alerts[0].Severity)
	}
	t.Logf("wash detected: bought %v, sold %v, net %v bps of gross",
		alerts[0].Evidence["bought_units"], alerts[0].Evidence["sold_units"],
		alerts[0].Evidence["net_bps"])
}

// A blocking alert must stop the buyback from spending against that price. The
// whole point of detecting a ramp is not to then pay it.
func TestBlockingAlertStopsTheBuyback(t *testing.T) {
	p := pool(t)
	n := setup(t, p)
	tap(t, p, n)

	mustExec(t, p, `
		INSERT INTO surveillance_alerts (instrument_id, session_date, detection, severity, subject_group, evidence)
		VALUES ($1,$2::date,'ramping','block','issuer-group','{}'::jsonb)`,
		n.instrumentID, sessionDate)

	b := runBuyback(t, p, n, 0)
	if b.Refusal != "surveillance_block" {
		t.Fatalf("refusal = %q, want surveillance_block", b.Refusal)
	}
	if b.Escrowed == 0 {
		t.Fatal("intents must escrow while an instrument is under a blocking alert")
	}
}

// An alert is worked by a person, and closing one is what releases the block.
// So it carries a name.
func TestAlertWorkflow(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)

	var id int64
	if err := p.QueryRow(ctx, `
		INSERT INTO surveillance_alerts (instrument_id, session_date, detection, severity, subject_group, evidence)
		VALUES ($1,$2::date,'wash_trading','block','grp','{}'::jsonb) RETURNING id`,
		n.instrumentID, sessionDate).Scan(&id); err != nil {
		t.Fatal(err)
	}

	mustTx(t, p, func(tx pgx.Tx) error { return exchange.Triage(ctx, tx, id, "surveillance.analyst") })

	// Triaging twice is a mistake worth surfacing, not a silent no-op.
	if err := inTx(p, func(tx pgx.Tx) error {
		return exchange.Triage(ctx, tx, id, "someone.else")
	}); err == nil {
		t.Error("an already-triaged alert was triaged again")
	}

	mustTx(t, p, func(tx pgx.Tx) error {
		return exchange.Close(ctx, tx, id, "false_positive", "head.of.surveillance", "market maker quoting wide")
	})

	var state, outcome, by string
	if err := p.QueryRow(ctx,
		`SELECT state, outcome, closed_by FROM surveillance_alerts WHERE id = $1`, id).
		Scan(&state, &outcome, &by); err != nil {
		t.Fatal(err)
	}
	if state != "false_positive" || by != "head.of.surveillance" {
		t.Fatalf("state %q closed by %q", state, by)
	}

	// And with the block lifted, the buyback can price the instrument again.
	tap(t, p, n)
	if b := runBuyback(t, p, n, 0); b.Refusal != "" {
		t.Fatalf("buyback still refused after the alert was closed: %q", b.Refusal)
	}
}

func TestCloseRejectsUnknownOutcome(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)
	var id int64
	if err := p.QueryRow(ctx, `
		INSERT INTO surveillance_alerts (instrument_id, session_date, detection, severity, subject_group, evidence)
		VALUES ($1,$2::date,'ramping','warn','grp','{}'::jsonb) RETURNING id`,
		n.instrumentID, sessionDate).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := inTx(p, func(tx pgx.Tx) error {
		return exchange.Close(ctx, tx, id, "looked fine to me", "someone", "")
	}); err == nil {
		t.Fatal("an unrecognised outcome must be rejected")
	}
}

// ---------------------------------------------------------------- helpers

func order(member uuid.UUID, a acct, holder uuid.UUID, side exchange.Side,
	limit money.Kobo, qty share.Units) exchange.OrderRequest {
	return exchange.OrderRequest{
		MemberID: member, ClientAccountID: a.client, CardholderID: holder,
		AccountID: a.ledger, Side: side, Type: exchange.TypeLimit,
		Limit: limit, Qty: qty,
	}
}
