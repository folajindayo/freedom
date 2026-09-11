package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"freedom/api/internal/ledger"
	"freedom/api/internal/money"
	"freedom/api/internal/recon"
	"freedom/api/internal/registrar"
	"freedom/api/internal/share"
)

// A clean system reconciles. This is the baseline: if it ever fails, one of the
// checks below is wrong rather than the system.
func TestReconciliationIsCleanAfterNormalTrading(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)

	// A tap that earns equity, so there are lots and postings to reconcile
	// rather than an empty system that trivially agrees with itself.
	tap(t, p, n)
	runBuyback(t, p, n, 0)
	giveShares(t, p, n, newCardholder(t, p, "Yemi Coker"), share.Whole(5), money.Naira(30))

	var runs []recon.Run
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		runs, err = recon.RunAll(ctx, tx, tradeDate)
		return err
	})
	if len(runs) != 4 {
		t.Fatalf("ran %d reconciliations, want 4", len(runs))
	}
	for _, r := range runs {
		if !r.Clean() {
			t.Errorf("%s found %d breaks: %+v", r.Kind, len(r.Breaks), r.Breaks[0])
		}
		t.Logf("%-16s checked %d, clean", r.Kind, r.Checked)
	}
}

// And it notices when they disagree. Corrupting the lot register behind the
// ledger's back is exactly the drift this exists to catch.
func TestReconciliationCatchesDrift(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)
	giveShares(t, p, n, n.cardholderID, share.Whole(10), money.Naira(30))

	// Someone's bad migration, or a bug in a disposal path.
	mustExec(t, p, `UPDATE holding_lots SET units_open = units_open - $2
	                 WHERE instrument_id = $1`, n.instrumentID, int64(share.Whole(3)))

	var run recon.Run
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		run, err = recon.LedgerVsLots(ctx, tx, tradeDate)
		return err
	})
	if run.Clean() {
		t.Fatal("three shares vanished from the lot register and nothing noticed")
	}
	b := run.Breaks[0]
	if b.Expected == nil || b.Actual == nil || *b.Actual-*b.Expected != int64(share.Whole(3)) {
		t.Fatalf("break does not describe the drift: %+v", b)
	}
	t.Logf("caught: %s expected %d, ledger holds %d", b.Subject, *b.Expected, *b.Actual)
}

// A reservation outliving its order is a cardholder's money in an account they
// cannot see, and nothing else in the system would ever mention it.
func TestReconciliationCatchesStrandedReservations(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)
	holder := n.cardholderID
	fund(t, p, holder, money.Naira(10_000))

	// Move cash into a reserve with no order behind it.
	mustTx(t, p, func(tx pgx.Tx) error {
		avail, err := ledger.Resolve(ctx, tx, ledger.Cardholder(holder, ledger.KindAvailable, ledger.AssetNGN))
		if err != nil {
			return err
		}
		reserve, err := ledger.Resolve(ctx, tx,
			ledger.Cardholder(holder, ledger.KindOrderCashReserve, ledger.AssetNGN))
		if err != nil {
			return err
		}
		_, err = ledger.Post(ctx, tx, ledger.Tx{
			EventType: "test.strand", BusinessDate: tradeDate,
			IdempotencyKey: "strand|" + holder.String(),
			Entries: []ledger.Entry{
				{AccountID: avail, Amount: ledger.NGN(-money.Naira(500)), Reason: "strand"},
				{AccountID: reserve, Amount: ledger.NGN(money.Naira(500)), Reason: "strand"},
			},
		})
		return err
	})

	var run recon.Run
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		run, err = recon.Reserves(ctx, tx, tradeDate)
		return err
	})
	if run.Clean() {
		t.Fatal("a stranded reservation went unnoticed")
	}
}

// Breaks are worked and closed by a person, and closing one carries a name.
func TestBreakWorkflow(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)
	giveShares(t, p, n, n.cardholderID, share.Whole(10), money.Naira(30))
	mustExec(t, p, `UPDATE holding_lots SET units_open = units_open - $2
	                 WHERE instrument_id = $1`, n.instrumentID, int64(share.Whole(1)))

	mustTx(t, p, func(tx pgx.Tx) error {
		_, err := recon.RunAll(ctx, tx, tradeDate)
		return err
	})

	var open int
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		open, err = recon.OpenBreaks(ctx, tx)
		return err
	})
	if open == 0 {
		t.Fatal("no open breaks after a deliberate drift")
	}

	var id int64
	if err := p.QueryRow(ctx,
		`SELECT id FROM recon_breaks WHERE state = 'open' ORDER BY id LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	mustTx(t, p, func(tx pgx.Tx) error {
		return recon.Resolve(ctx, tx, id, "investigating", "ops.analyst", "chasing the disposal path")
	})
	mustTx(t, p, func(tx pgx.Tx) error {
		return recon.Resolve(ctx, tx, id, "resolved", "head.of.ops", "bad migration, corrected")
	})
	if err := inTx(p, func(tx pgx.Tx) error {
		return recon.Resolve(ctx, tx, id, "resolved", "someone", "")
	}); err == nil {
		t.Error("a resolved break was resolved again")
	}
	if err := inTx(p, func(tx pgx.Tx) error {
		return recon.Resolve(ctx, tx, id, "resolved", "", "")
	}); err == nil {
		t.Error("a break was resolved with no name attached")
	}
}

// An off-market transfer changes ownership with no counterparty, no price and
// no market to witness it — so it needs a documented reason and a second
// person's signature.
func TestOffMarketTransferRequiresReasonAndSecondApprover(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)

	deceased := n.cardholderID
	beneficiary := newCardholder(t, p, "Adaeze Okonkwo")
	giveShares(t, p, n, deceased, share.Whole(10), money.Naira(30))

	from := walletOf(t, p, deceased, n.instrumentID)
	to := walletOf(t, p, beneficiary, n.instrumentID)

	t.Run("a transfer needs a documented reference", func(t *testing.T) {
		if err := inTx(p, func(tx pgx.Tx) error {
			_, err := registrar.RequestTransfer(ctx, tx, registrar.Transfer{
				InstrumentID: n.instrumentID, From: from, To: to,
				Units: share.Whole(10), Reason: registrar.TransferInheritance,
				RequestedBy: "registry.clerk",
			})
			return err
		}); err == nil {
			t.Fatal("an inheritance transfer was accepted with no probate reference")
		}
	})

	var id uuid.UUID
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		id, err = registrar.RequestTransfer(ctx, tx, registrar.Transfer{
			InstrumentID: n.instrumentID, From: from, To: to,
			Units: share.Whole(10), Reason: registrar.TransferInheritance,
			Reference: "PROBATE/LA/2027/4412", RequestedBy: "registry.clerk",
		})
		return err
	})

	t.Run("the requester may not also approve", func(t *testing.T) {
		if err := inTx(p, func(tx pgx.Tx) error {
			return registrar.Approve(ctx, tx, id, "registry.clerk")
		}); err == nil {
			t.Fatal("one person moved someone else's shares on their own signature")
		}
	})

	t.Run("an unapproved transfer does not execute", func(t *testing.T) {
		if err := inTx(p, func(tx pgx.Tx) error {
			return registrar.Execute(ctx, tx, id, tradeDate)
		}); err == nil {
			t.Fatal("an unapproved transfer executed")
		}
	})

	mustTx(t, p, func(tx pgx.Tx) error {
		return registrar.Approve(ctx, tx, id, "head.of.registry")
	})
	mustTx(t, p, func(tx pgx.Tx) error {
		return registrar.Execute(ctx, tx, id, tradeDate)
	})

	if got := sharesOf(t, p, deceased, n.instrumentID); got != 0 {
		t.Errorf("the estate still holds %s", got)
	}
	if got := sharesOf(t, p, beneficiary, n.instrumentID); got != share.Whole(10) {
		t.Errorf("the beneficiary holds %s, want 10", got)
	}

	// Basis travelled. Without it the beneficiary would show an enormous
	// fictitious gain the first time they sold, with no way to reconstruct the
	// real figure.
	var basis int64
	if err := p.QueryRow(ctx, `
		SELECT COALESCE(SUM(cost_open_kobo), 0) FROM holding_lots
		 WHERE account_id = $1 AND instrument_id = $2`, to, n.instrumentID).Scan(&basis); err != nil {
		t.Fatal(err)
	}
	if money.Kobo(basis) != money.Naira(300) {
		t.Errorf("inherited basis = %s, want ₦300.00", money.Kobo(basis))
	}
}

// A transfer must not be a way to launder a locked holding into a sellable one.
func TestTransferCarriesTheChargebackLock(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)

	giver := n.cardholderID
	receiver := newCardholder(t, p, "Emeka Obi")
	giveShares(t, p, n, giver, share.Whole(10), money.Naira(30))
	mustExec(t, p, `UPDATE holding_lots SET transferable_from = '2028-01-01'
	                 WHERE instrument_id = $1`, n.instrumentID)

	from := walletOf(t, p, giver, n.instrumentID)
	to := walletOf(t, p, receiver, n.instrumentID)

	var id uuid.UUID
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		id, err = registrar.RequestTransfer(ctx, tx, registrar.Transfer{
			InstrumentID: n.instrumentID, From: from, To: to,
			Units: share.Whole(5), Reason: registrar.TransferGift,
			Reference: "GIFT/2027/1", RequestedBy: "clerk",
		})
		return err
	})
	mustTx(t, p, func(tx pgx.Tx) error { return registrar.Approve(ctx, tx, id, "supervisor") })

	// The lots are locked, so there is nothing transferable to move.
	if err := inTx(p, func(tx pgx.Tx) error {
		return registrar.Execute(ctx, tx, id, tradeDate)
	}); err == nil {
		t.Fatal("locked shares were transferred out")
	}
}

// A holder disputes what they were SENT, so what they were sent has to still
// exist — regenerating it later would answer a different question, and would
// quietly change its answer every time the price moved.
func TestHoldingStatement(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)
	holder := n.cardholderID
	giveShares(t, p, n, holder, share.Whole(12), money.Naira(30))

	var st registrar.Statement
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		st, err = registrar.Generate(ctx, tx, holder, "2027-05-01", "2027-05-31")
		return err
	})
	if len(st.Holdings) != 1 {
		t.Fatalf("statement has %d lines, want 1", len(st.Holdings))
	}
	line := st.Holdings[0]
	if line.Units != share.Whole(12) {
		t.Errorf("units = %s, want 12", line.Units)
	}
	if line.CostKobo != money.Naira(360) {
		t.Errorf("cost = %s, want ₦360.00", line.CostKobo)
	}
	if line.ValueKobo != money.Naira(480) {
		t.Errorf("value = %s, want ₦480.00 at the ₦40.00 reference", line.ValueKobo)
	}
	t.Logf("%s: %s units, cost %s, value %s", line.Symbol, line.Units, line.CostKobo, line.ValueKobo)

	// It is stored, and the stored copy is what was sent.
	var stored string
	if err := p.QueryRow(ctx, `
		SELECT content::text FROM holding_statements
		 WHERE account_id = $1 AND period_end = '2027-05-31'`, st.AccountID).Scan(&stored); err != nil {
		t.Fatalf("the statement was not kept: %v", err)
	}
	if !strings.Contains(stored, n.symbol) {
		t.Error("the stored statement does not name the instrument")
	}
}

// The locked portion is shown separately, because "you own 12 but can sell 0"
// is the single most confusing thing about this product and the statement is
// where it has to be explained.
func TestStatementShowsLockedUnitsSeparately(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)
	holder := n.cardholderID
	giveShares(t, p, n, holder, share.Whole(12), money.Naira(30))
	mustExec(t, p, `UPDATE holding_lots SET transferable_from = '2099-01-01'
	                 WHERE instrument_id = $1`, n.instrumentID)

	var st registrar.Statement
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		st, err = registrar.Generate(ctx, tx, holder, "2027-05-01", "2027-05-31")
		return err
	})
	if st.Holdings[0].LockedUnits != share.Whole(12) {
		t.Fatalf("locked units = %s, want all 12", st.Holdings[0].LockedUnits)
	}
}

// walletOf resolves a holder's stock wallet, creating it on first use — a
// recipient who has never held this instrument has no account until something
// puts one there.
func walletOf(t *testing.T, p *pgxpool.Pool, holder uuid.UUID, instrumentID string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		id, err = ledger.Resolve(context.Background(), tx,
			ledger.Cardholder(holder, ledger.KindStockWallet, instrumentID))
		return err
	})
	return id
}
