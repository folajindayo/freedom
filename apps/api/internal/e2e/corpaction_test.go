package e2e

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"freedom/api/internal/buyback"
	"freedom/api/internal/exchange"
	"freedom/api/internal/ledger"
	"freedom/api/internal/money"
	"freedom/api/internal/share"
)

// A split changes how many pieces a holding is in, and nothing else. Every
// invariant that could quietly break is asserted here, because each one breaks
// silently: no error, just an instrument that stops working weeks later.
func TestSplitPreservesEveryInvariant(t *testing.T) {
	p := pool(t)
	n := setup(t, p)

	// Odd unit counts, so the rounding actually has to do something.
	a := n.cardholderID
	b := newCardholder(t, p, "Bisi Alabi")
	c := newCardholder(t, p, "Segun Oduya")
	giveShares(t, p, n, a, share.PerShare*3+33_333_333, money.Naira(30))
	giveShares(t, p, n, b, share.PerShare*7+77_777_777, money.Naira(31))
	giveShares(t, p, n, c, share.PerShare+1, money.Naira(29))

	beforeA, beforeB, beforeC := sharesOf(t, p, a, n.instrumentID),
		sharesOf(t, p, b, n.instrumentID), sharesOf(t, p, c, n.instrumentID)
	beforeAuth, beforeRef := instrumentState(t, p, n)
	beforeBasis := totalBasis(t, p, n)

	// A 3:2 split — deliberately not 2:1, so nothing divides evenly.
	applySplit(t, p, n, 3, 2)

	afterA, afterB, afterC := sharesOf(t, p, a, n.instrumentID),
		sharesOf(t, p, b, n.instrumentID), sharesOf(t, p, c, n.instrumentID)

	// Everyone grew, and roughly by the ratio.
	for _, pair := range []struct {
		name          string
		before, after share.Units
	}{{"a", beforeA, afterA}, {"b", beforeB, afterB}, {"c", beforeC, afterC}} {
		want := share.Units(int64(pair.before) * 3 / 2)
		if pair.after < want-1 || pair.after > want+1 {
			t.Errorf("holder %s went from %s to %s, want about %s",
				pair.name, pair.before, pair.after, want)
		}
	}

	// Lots still reconcile to the wallet balance. If they drift, a later sell
	// either cannot find the units or finds units the ledger does not have.
	for _, holder := range []uuid.UUID{a, b, c} {
		assertLotsMatchWallet(t, p, n, holder)
	}

	// Cost basis is untouched: a split changes the price per share, not what
	// anyone paid.
	if got := totalBasis(t, p, n); got != beforeBasis {
		t.Errorf("total cost basis moved from %s to %s across a split", beforeBasis, got)
	}

	// The authorised ceiling scaled with the share count. Without this the next
	// buyback finds issued above authorised and stops forever — silently,
	// because "the cap bound" is a normal outcome.
	afterAuth, afterRef := instrumentState(t, p, n)
	if afterAuth != beforeAuth*3/2 {
		t.Errorf("authorised units went %s → %s, want %s",
			beforeAuth, afterAuth, beforeAuth*3/2)
	}
	// And the reference price fell by the same ratio, or the whole price band
	// shifts and every sensible order is rejected as out of band.
	if afterRef != beforeRef*2/3 {
		t.Errorf("reference price went %s → %s, want %s", beforeRef, afterRef, beforeRef*2/3)
	}
}

// The exploit the adjustment factors exist to close.
//
// The buyback caps what it pays at the trailing volume-weighted average. Read
// raw, every session before a 2:1 split shows twice the real price — so the cap
// sits at twice the market and stops binding. An issuer who wanted to ramp
// would simply split the week before and then print whatever they liked.
func TestSplitDoesNotDisableTheVWAPCap(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)

	// Honest history at ₦40 on real volume.
	for _, d := range []string{"2026-09-04", "2026-09-07", "2026-09-08", "2026-09-09", "2026-09-10"} {
		publishAuction(t, p, n, d, money.Naira(40), share.Whole(50))
	}

	// The company splits 2:1. Fair value is now ₦20 a share.
	giveShares(t, p, n, n.cardholderID, share.Whole(10), money.Naira(40))
	applySplit(t, p, n, 2, 1)

	// In adjusted terms the trailing VWAP is ₦20, not ₦40.
	var adj int64
	if err := p.QueryRow(ctx, `
		SELECT (SUM(adj_price_kobo::numeric * adj_volume_units::numeric)
		        / SUM(adj_volume_units::numeric))::bigint
		  FROM price_observations_adjusted
		 WHERE instrument_id = $1 AND volume_units > 0`, n.instrumentID).Scan(&adj); err != nil {
		t.Fatal(err)
	}
	if money.Kobo(adj) != money.Naira(20) {
		t.Fatalf("adjusted trailing VWAP is %s, want ₦20.00 — history is not comparable across the split",
			money.Kobo(adj))
	}

	// Now the ramp: today's session prints at ₦40, double fair value.
	publishAuction(t, p, n, sessionDate, money.Naira(40), share.Whole(1))
	tap(t, p, n)

	var b buyback.Result
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		e := buyback.New()
		b, err = e.RunSession(ctx, tx, n.instrumentID, sessionDate)
		return err
	})

	if b.Price != money.Naira(20) {
		t.Fatalf("the buyback paid %s after a split-then-ramp; the cap should have held it at ₦20.00",
			b.Price)
	}
	t.Logf("split 2:1 then printed ₦40.00; buyback paid %s", b.Price)
}

// A dividend moves cash from the company to the people who own it, and lands
// where they can spend it.
func TestCashDividendPaysHolders(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)

	a := n.cardholderID
	b := newCardholder(t, p, "Kemi Balogun")
	giveShares(t, p, n, a, share.Whole(30), money.Naira(30))
	giveShares(t, p, n, b, share.Whole(10), money.Naira(30))
	fundTreasury(t, p, n, money.Naira(10_000))

	cashA := nairaOf(t, p, a, ledger.KindAvailable)
	cashB := nairaOf(t, p, b, ledger.KindAvailable)

	// ₦2.00 a share, 10% withholding.
	var declared money.Kobo
	mustTx(t, p, func(tx pgx.Tx) error {
		id, err := exchange.Declare(ctx, tx, exchange.Action{
			InstrumentID: n.instrumentID, Kind: exchange.ActionDividend,
			DPS: money.Naira(2), WHTBps: 1000,
			ExDate: "2027-06-01", RecordDate: "2027-06-01", PayDate: "2027-06-15",
		})
		if err != nil {
			return err
		}
		if _, err := exchange.TakeRecord(ctx, tx, id); err != nil {
			return err
		}
		declared, err = exchange.PayDividend(ctx, tx, id, "2027-06-15")
		return err
	})

	// A holds 30 shares → ₦60 gross, ₦54 net after 10% withholding.
	if got := nairaOf(t, p, a, ledger.KindAvailable) - cashA; got != money.Naira(54) {
		t.Errorf("holder A received %s, want ₦54.00 (₦60.00 less 10%%)", got)
	}
	if got := nairaOf(t, p, b, ledger.KindAvailable) - cashB; got != money.Naira(18) {
		t.Errorf("holder B received %s, want ₦18.00", got)
	}

	// Withholding is held separately — it is the taxman's, never the scheme's.
	wht, err := ledger.NairaBalance(ctx, p, ledger.Scheme(ledger.KindTaxWithheld, ledger.AssetNGN))
	if err != nil {
		t.Fatal(err)
	}
	if wht != money.Naira(8) {
		t.Errorf("withheld %s, want ₦8.00", wht)
	}
	t.Logf("declared %s across holders; %s withheld", declared, wht)

	// And the reference price dropped by the dividend, or the price band would
	// be stale by ₦2.00 and every sensible order rejected as out of band.
	_, ref := instrumentState(t, p, n)
	if ref != money.Naira(38) {
		t.Errorf("reference price is %s after a ₦2.00 dividend on a ₦40.00 share, want ₦38.00", ref)
	}
}

// Corporate actions are applied by jobs that will be re-run.
func TestCorporateActionsAreIdempotent(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)
	holder := n.cardholderID
	giveShares(t, p, n, holder, share.Whole(10), money.Naira(30))

	var actionID uuid.UUID
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		actionID, err = exchange.Declare(ctx, tx, exchange.Action{
			InstrumentID: n.instrumentID, Kind: exchange.ActionSplit,
			RatioNum: 2, RatioDen: 1,
			ExDate: "2027-06-01", RecordDate: "2027-06-01",
		})
		if err != nil {
			return err
		}
		_, err = exchange.TakeRecord(ctx, tx, actionID)
		return err
	})
	mustTx(t, p, func(tx pgx.Tx) error {
		return exchange.ApplySplit(ctx, tx, actionID, "2027-06-01")
	})

	after := sharesOf(t, p, holder, n.instrumentID)

	// A second run must not double them again. The action is already applied,
	// so it refuses outright.
	err := inTx(p, func(tx pgx.Tx) error {
		return exchange.ApplySplit(ctx, tx, actionID, "2027-06-01")
	})
	if err == nil {
		t.Fatal("a split was applied twice")
	}
	if again := sharesOf(t, p, holder, n.instrumentID); again != after {
		t.Fatalf("holding moved from %s to %s on a re-run", after, again)
	}
}

func TestCorporateActionValidation(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)

	for _, tc := range []struct {
		name string
		a    exchange.Action
	}{
		{"split with no ratio", exchange.Action{Kind: exchange.ActionSplit}},
		{"a 1:1 split changes nothing", exchange.Action{Kind: exchange.ActionSplit, RatioNum: 1, RatioDen: 1}},
		{"dividend with no amount", exchange.Action{Kind: exchange.ActionDividend}},
		{"withholding above 100%", exchange.Action{Kind: exchange.ActionDividend,
			DPS: money.Naira(1), WHTBps: 20_000}},
		{"unknown kind", exchange.Action{Kind: "reverse_merger"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.a.InstrumentID = n.instrumentID
			tc.a.ExDate, tc.a.RecordDate = "2027-06-01", "2027-06-01"
			err := inTx(p, func(tx pgx.Tx) error {
				_, err := exchange.Declare(ctx, tx, tc.a)
				return err
			})
			if err == nil {
				t.Fatal("an invalid corporate action was accepted")
			}
		})
	}
}

// ---------------------------------------------------------------- helpers

func applySplit(t *testing.T, p *pgxpool.Pool, n *network, num, den int64) {
	t.Helper()
	ctx := context.Background()
	mustTx(t, p, func(tx pgx.Tx) error {
		id, err := exchange.Declare(ctx, tx, exchange.Action{
			InstrumentID: n.instrumentID, Kind: exchange.ActionSplit,
			RatioNum: num, RatioDen: den,
			ExDate: "2027-06-01", RecordDate: "2027-06-01",
		})
		if err != nil {
			return err
		}
		if _, err := exchange.TakeRecord(ctx, tx, id); err != nil {
			return err
		}
		return exchange.ApplySplit(ctx, tx, id, "2027-06-01")
	})
}

func instrumentState(t *testing.T, p *pgxpool.Pool, n *network) (share.Units, money.Kobo) {
	t.Helper()
	var auth, ref int64
	if err := p.QueryRow(context.Background(),
		`SELECT shares_authorised_units, reference_price_kobo FROM instruments WHERE id = $1`,
		n.instrumentID).Scan(&auth, &ref); err != nil {
		t.Fatal(err)
	}
	return share.Units(auth), money.Kobo(ref)
}

func totalBasis(t *testing.T, p *pgxpool.Pool, n *network) money.Kobo {
	t.Helper()
	var v int64
	if err := p.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(cost_open_kobo), 0) FROM holding_lots WHERE instrument_id = $1`,
		n.instrumentID).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return money.Kobo(v)
}

// assertLotsMatchWallet is the two-truths check: holding_lots and the ledger
// must agree, or a later sell either cannot find units it should have or finds
// units the ledger never issued.
func assertLotsMatchWallet(t *testing.T, p *pgxpool.Pool, n *network, holder uuid.UUID) {
	t.Helper()
	wallet := sharesOf(t, p, holder, n.instrumentID)
	var lots int64
	if err := p.QueryRow(context.Background(), `
		SELECT COALESCE(SUM(l.units_open), 0)
		  FROM holding_lots l JOIN accounts a ON a.id = l.account_id
		 WHERE a.owner_id = $1 AND a.kind = 'stock_wallet' AND l.instrument_id = $2`,
		holder, n.instrumentID).Scan(&lots); err != nil {
		t.Fatal(err)
	}
	if share.Units(lots) != wallet {
		t.Errorf("lots sum to %s but the wallet holds %s", share.Units(lots), wallet)
	}
}

func fundTreasury(t *testing.T, p *pgxpool.Pool, n *network, amount money.Kobo) {
	t.Helper()
	ctx := context.Background()
	mustTx(t, p, func(tx pgx.Tx) error {
		ext, err := ledger.Resolve(ctx, tx, ledger.Scheme(ledger.KindExternal, ledger.AssetNGN))
		if err != nil {
			return err
		}
		cash, err := ledger.Resolve(ctx, tx, ledger.Company(n.companyID, ledger.KindTreasuryCash, ledger.AssetNGN))
		if err != nil {
			return err
		}
		_, err = ledger.Post(ctx, tx, ledger.Tx{
			EventType: "treasury.funded", BusinessDate: tradeDate,
			IdempotencyKey: "treasury|" + n.companyID.String(),
			Entries: []ledger.Entry{
				{AccountID: ext, Amount: ledger.NGN(-amount), Reason: "treasury.funding"},
				{AccountID: cash, Amount: ledger.NGN(amount), Reason: "treasury.funding"},
			},
		})
		return err
	})
}
