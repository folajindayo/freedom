package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"freedom/api/internal/dispute"
	"freedom/api/internal/ledger"
	"freedom/api/internal/money"
	"freedom/api/internal/scheme"
	"freedom/api/internal/share"
)

var filedAt = time.Date(2026, 9, 12, 10, 0, 0, 0, scheme.Lagos)

// The whole reason the chargeback lock exists.
//
// A cardholder taps, the fee pool buys them equity, and the tap is later
// charged back. The payment reverses AND the equity comes back — a chargeback
// that refunded the money but left the shares in place would mean the network
// bought somebody equity out of fees on a transaction that never happened.
func TestChargebackUnwindsTheBuyback(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)

	tap(t, p, n)
	runBuyback(t, p, n, 0)

	holder := n.cardholderID
	earned := sharesOf(t, p, holder, n.instrumentID)
	if earned == 0 {
		t.Fatal("the tap earned no equity, so there is nothing to unwind")
	}
	cashBefore := nairaOf(t, p, holder, ledger.KindAvailable)
	poolBefore := schemeBalance(t, p, ledger.KindBuybackPool)

	presentment := firstPresentment(t, p)

	var d dispute.Dispute
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		d, err = dispute.File(ctx, tx, presentment, dispute.ReasonFraud,
			"Card used at a shop I have never visited", filedAt)
		return err
	})

	// Fraud carries a provisional credit: the person who says their card was
	// used without them should not fund the investigation.
	if !d.Provisional {
		t.Fatal("a fraud dispute did not carry a provisional credit")
	}
	if got := nairaOf(t, p, holder, ledger.KindAvailable) - cashBefore; got != money.Naira(10_000) {
		t.Fatalf("provisional credit was %s, want ₦10,000.00", got)
	}

	mustTx(t, p, func(tx pgx.Tx) error {
		return dispute.Resolve(ctx, tx, d.ID, dispute.WonCardholder,
			"disputes.analyst", "terminal was not where the cardholder was", sessionDate, false)
	})

	// The equity is gone from the wallet and back in treasury.
	if got := sharesOf(t, p, holder, n.instrumentID); got != 0 {
		t.Fatalf("the cardholder still holds %s after a successful chargeback", got)
	}
	// The fee pool is whole again.
	if got := schemeBalance(t, p, ledger.KindBuybackPool); got != poolBefore {
		t.Errorf("the buyback pool is %s, was %s before the tap", got, poolBefore)
	}
	// And the cardholder keeps the refund, not two refunds.
	if got := nairaOf(t, p, holder, ledger.KindAvailable) - cashBefore; got != money.Naira(10_000) {
		t.Fatalf("the cardholder ended up with %s, want one ₦10,000.00 refund", got)
	}

	var method string
	var clawed int64
	if err := p.QueryRow(ctx,
		`SELECT method, units_clawed FROM buyback_unwinds`).Scan(&method, &clawed); err != nil {
		t.Fatal(err)
	}
	if method != "shares" || share.Units(clawed) != earned {
		t.Fatalf("unwound by %s for %s units, want all %s in shares",
			method, share.Units(clawed), earned)
	}
	t.Logf("chargeback unwound %s of %s in shares; pool restored", share.Units(clawed), n.symbol)

	// Issuance reverses, or the instrument's authorised headroom never recovers
	// and the buyback slowly strangles itself.
	var unwindEvents int
	if err := p.QueryRow(ctx, `
		SELECT COUNT(*) FROM cap_table_events
		 WHERE instrument_id = $1 AND kind = 'buyback_unwind'`, n.instrumentID).Scan(&unwindEvents); err != nil {
		t.Fatal(err)
	}
	if unwindEvents != 1 {
		t.Errorf("%d unwind events in the cap table, want 1", unwindEvents)
	}
}

// If the merchant proves the sale, the provisional credit was a loan against an
// outcome that went the other way.
func TestMerchantWinsReversesTheProvisionalCredit(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)
	tap(t, p, n)
	runBuyback(t, p, n, 0)

	holder := n.cardholderID
	before := nairaOf(t, p, holder, ledger.KindAvailable)
	earned := sharesOf(t, p, holder, n.instrumentID)

	var d dispute.Dispute
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		d, err = dispute.File(ctx, tx, firstPresentment(t, p), dispute.ReasonFraud,
			"I did not make this purchase", filedAt)
		return err
	})
	mustTx(t, p, func(tx pgx.Tx) error {
		return dispute.Represent(ctx, tx, d.ID, "signed delivery note and CCTV", filedAt)
	})
	mustTx(t, p, func(tx pgx.Tx) error {
		return dispute.Resolve(ctx, tx, d.ID, dispute.WonMerchant,
			"disputes.analyst", "evidence accepted", sessionDate, false)
	})

	if got := nairaOf(t, p, holder, ledger.KindAvailable); got != before {
		t.Fatalf("balance is %s, want the original %s — the provisional credit was not reversed", got, before)
	}
	// And the equity stays, because the transaction stands.
	if got := sharesOf(t, p, holder, n.instrumentID); got != earned {
		t.Fatalf("the cardholder holds %s, want the %s they earned", got, earned)
	}
}

// Every deadline has a default outcome, and which way it falls depends on whose
// turn it is — not on who filed.
func TestClocksDecideWhenNobodyDoes(t *testing.T) {
	p := pool(t)
	ctx := context.Background()

	t.Run("merchant silence hands it to the cardholder", func(t *testing.T) {
		n := setup(t, p)
		tap(t, p, n)
		runBuyback(t, p, n, 0)

		var d dispute.Dispute
		mustTx(t, p, func(tx pgx.Tx) error {
			var err error
			d, err = dispute.File(ctx, tx, firstPresentment(t, p), dispute.ReasonProcessingError,
				"Charged twice for one meal", filedAt)
			return err
		})

		var resolved []uuid.UUID
		mustTx(t, p, func(tx pgx.Tx) error {
			var err error
			// Well past the 45-day representment window.
			resolved, err = dispute.RunClocks(ctx, tx, "2027-01-01")
			return err
		})
		if len(resolved) != 1 || resolved[0] != d.ID {
			t.Fatalf("the clock did not resolve the dispute: %v", resolved)
		}
		if got := disputeState(t, p, d.ID); got != dispute.WonCardholder {
			t.Fatalf("state = %s, want the cardholder to win on merchant silence", got)
		}
		// And the unwind ran, because the cardholder won.
		if got := sharesOf(t, p, n.cardholderID, n.instrumentID); got != 0 {
			t.Errorf("equity survived a default judgement: %s", got)
		}
		// The resolution is recorded as the clock's, not a person's.
		var byDefault bool
		if err := p.QueryRow(ctx, `
			SELECT by_default FROM dispute_events
			 WHERE dispute_id = $1 ORDER BY id DESC LIMIT 1`, d.ID).Scan(&byDefault); err != nil {
			t.Fatal(err)
		}
		if !byDefault {
			t.Error("a clock resolution was recorded as if a person made it")
		}
	})

	t.Run("cardholder silence after representment hands it to the merchant", func(t *testing.T) {
		n := setup(t, p)
		tap(t, p, n)
		runBuyback(t, p, n, 0)
		earned := sharesOf(t, p, n.cardholderID, n.instrumentID)

		var d dispute.Dispute
		mustTx(t, p, func(tx pgx.Tx) error {
			var err error
			d, err = dispute.File(ctx, tx, firstPresentment(t, p), dispute.ReasonConsumerDispute,
				"Goods not as described", filedAt)
			return err
		})
		mustTx(t, p, func(tx pgx.Tx) error {
			return dispute.Represent(ctx, tx, d.ID, "photographs of the item as sold", filedAt)
		})
		mustTx(t, p, func(tx pgx.Tx) error {
			_, err := dispute.RunClocks(ctx, tx, "2027-01-01")
			return err
		})
		if got := disputeState(t, p, d.ID); got != dispute.WonMerchant {
			t.Fatalf("state = %s, want the merchant to win on cardholder silence", got)
		}
		if got := sharesOf(t, p, n.cardholderID, n.instrumentID); got != earned {
			t.Errorf("equity was clawed back from a dispute the cardholder lost")
		}
	})

	// Arbitration never defaults. Letting a clock settle it would make the
	// scheme's own inaction a ruling.
	t.Run("arbitration does not default", func(t *testing.T) {
		if _, ok := dispute.DefaultOutcome(dispute.StateArbitration); ok {
			t.Fatal("arbitration has a default outcome")
		}
	})
}

// The filing window and the chargeback lock are the same 120 days on purpose.
// If they diverged, equity would become sellable while a dispute against the
// transaction that funded it was still possible.
func TestFilingWindowMatchesTheChargebackLock(t *testing.T) {
	if dispute.FilingWindow != scheme.ChargebackWindow {
		t.Fatalf("the filing window is %v and the share lock is %v; they must be identical",
			dispute.FilingWindow, scheme.ChargebackWindow)
	}

	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)
	tap(t, p, n)

	// A dispute filed a year later is out of time.
	err := inTx(p, func(tx pgx.Tx) error {
		_, err := dispute.File(ctx, tx, firstPresentment(t, p), dispute.ReasonFraud,
			"Only just noticed", filedAt.AddDate(1, 0, 0))
		return err
	})
	if err == nil {
		t.Fatal("a dispute was filed a year after the transaction cleared")
	}
}

func TestOneDisputePerTransaction(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)
	tap(t, p, n)
	presentment := firstPresentment(t, p)

	mustTx(t, p, func(tx pgx.Tx) error {
		_, err := dispute.File(ctx, tx, presentment, dispute.ReasonFraud, "Not mine", filedAt)
		return err
	})
	if err := inTx(p, func(tx pgx.Tx) error {
		_, err := dispute.File(ctx, tx, presentment, dispute.ReasonFraud, "Still not mine", filedAt)
		return err
	}); err == nil {
		t.Fatal("one transaction was charged back twice")
	}
}

func TestDisputeValidation(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)
	tap(t, p, n)
	presentment := firstPresentment(t, p)

	for _, tc := range []struct{ name, reason, detail string }{
		{"unknown reason", "i changed my mind", "..."},
		{"no description", dispute.ReasonFraud, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := inTx(p, func(tx pgx.Tx) error {
				_, err := dispute.File(ctx, tx, presentment, tc.reason, tc.detail, filedAt)
				return err
			}); err == nil {
				t.Fatal("an invalid dispute was accepted")
			}
		})
	}

	var d dispute.Dispute
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		d, err = dispute.File(ctx, tx, presentment, dispute.ReasonFraud, "Not mine", filedAt)
		return err
	})
	if err := inTx(p, func(tx pgx.Tx) error {
		return dispute.Resolve(ctx, tx, d.ID, dispute.WonCardholder, "", "", sessionDate, false)
	}); err == nil {
		t.Error("a dispute was resolved with no name attached")
	}
	mustTx(t, p, func(tx pgx.Tx) error {
		return dispute.Resolve(ctx, tx, d.ID, dispute.WonMerchant, "analyst", "ok", sessionDate, false)
	})
	if err := inTx(p, func(tx pgx.Tx) error {
		return dispute.Resolve(ctx, tx, d.ID, dispute.WonCardholder, "analyst", "again", sessionDate, false)
	}); err == nil {
		t.Error("a resolved dispute was resolved again")
	}
}

// When the shares are gone, the shortfall is charged at the price the
// cardholder received them at — not today's. They did not choose to receive
// them and must not owe more than the network spent because the price rose.
func TestUnwindFallsBackToCashWhenSharesAreGone(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)
	tap(t, p, n)
	runBuyback(t, p, n, 0)

	holder := n.cardholderID
	earned := sharesOf(t, p, holder, n.instrumentID)
	fund(t, p, holder, money.Naira(1_000))

	// Simulate the shares having left the wallet — a late dispute after the
	// lock expired and the holder sold.
	mustTx(t, p, func(tx pgx.Tx) error {
		wallet, err := ledger.Resolve(ctx, tx, ledger.Cardholder(holder, ledger.KindStockWallet, n.instrumentID))
		if err != nil {
			return err
		}
		ext, err := ledger.Resolve(ctx, tx, ledger.Scheme(ledger.KindExternal, n.instrumentID))
		if err != nil {
			return err
		}
		if _, err := ledger.Post(ctx, tx, ledger.Tx{
			EventType: "test.sold", BusinessDate: sessionDate,
			IdempotencyKey: "sold|" + holder.String(),
			Entries: []ledger.Entry{
				{AccountID: wallet, Amount: ledger.Equity(n.symbol, -earned), Reason: "sold"},
				{AccountID: ext, Amount: ledger.Equity(n.symbol, earned), Reason: "sold"},
			},
		}); err != nil {
			return err
		}
		_, err = tx.Exec(ctx,
			`UPDATE holding_lots SET units_open = 0, cost_open_kobo = 0 WHERE account_id = $1`, wallet)
		return err
	})

	cashBefore := nairaOf(t, p, holder, ledger.KindAvailable)
	var d dispute.Dispute
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		d, err = dispute.File(ctx, tx, firstPresentment(t, p), dispute.ReasonProcessingError,
			"Double charged", filedAt)
		return err
	})
	mustTx(t, p, func(tx pgx.Tx) error {
		return dispute.Resolve(ctx, tx, d.ID, dispute.WonCardholder, "analyst", "upheld", sessionDate, false)
	})

	var method string
	var short, recovered, loss int64
	if err := p.QueryRow(ctx,
		`SELECT method, units_short, cash_recovered_kobo, loss_kobo FROM buyback_unwinds`).
		Scan(&method, &short, &recovered, &loss); err != nil {
		t.Fatal(err)
	}
	if method != "cash" {
		t.Fatalf("method = %s, want cash — no shares were left to claw, but the cardholder covered it", method)
	}
	if share.Units(short) != earned {
		t.Errorf("short by %s, want the whole %s", share.Units(short), earned)
	}
	if loss != 0 {
		t.Errorf("wrote off %s despite the cardholder having cash", money.Kobo(loss))
	}
	// Recovered in cash, at the price they received the shares at.
	if money.Kobo(recovered) != money.Naira(5) {
		t.Errorf("recovered %s, want the ₦5.00 the pool spent", money.Kobo(recovered))
	}
	// The refund arrived and the ₦5 recovery came out of it.
	if got := nairaOf(t, p, holder, ledger.KindAvailable) - cashBefore; got != money.Naira(10_000)-money.Naira(5) {
		t.Errorf("net cash movement %s, want ₦9,995.00", got)
	}
	t.Logf("shares gone: recovered %s in cash, wrote off %s", money.Kobo(recovered), money.Kobo(loss))
}

// A merchant who was never listed earned the cardholder nothing, so there is
// nothing to unwind and the chargeback must still work.
func TestChargebackWithNoBuybackIsFine(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)
	mustExec(t, p, `UPDATE merchants SET company_id = NULL WHERE id = $1`, n.merchantID)
	tap(t, p, n)

	var d dispute.Dispute
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		d, err = dispute.File(ctx, tx, firstPresentment(t, p), dispute.ReasonFraud, "Not mine", filedAt)
		return err
	})
	mustTx(t, p, func(tx pgx.Tx) error {
		return dispute.Resolve(ctx, tx, d.ID, dispute.WonCardholder, "analyst", "upheld", sessionDate, false)
	})

	var unwinds int
	if err := p.QueryRow(ctx, `SELECT COUNT(*) FROM buyback_unwinds`).Scan(&unwinds); err != nil {
		t.Fatal(err)
	}
	if unwinds != 0 {
		t.Fatalf("%d unwinds recorded for a merchant that never listed", unwinds)
	}
}

func firstPresentment(t *testing.T, p *pgxpool.Pool) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := p.QueryRow(context.Background(),
		`SELECT id FROM presentments WHERE kind = 'first' ORDER BY received_at LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func disputeState(t *testing.T, p *pgxpool.Pool, id uuid.UUID) string {
	t.Helper()
	var s string
	if err := p.QueryRow(context.Background(),
		`SELECT state FROM disputes WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func schemeBalance(t *testing.T, p *pgxpool.Pool, kind string) money.Kobo {
	t.Helper()
	v, err := ledger.NairaBalance(context.Background(), p, ledger.Scheme(kind, ledger.AssetNGN))
	if err != nil {
		t.Fatal(err)
	}
	return v
}
