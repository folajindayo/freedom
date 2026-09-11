// Package e2e drives the whole rail: a tap on a real credential travels through
// acceptance, authorisation, clearing and buyback, and the cardholder ends up
// owning a piece of the shop.
package e2e

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"oja/api/internal/acquirer"
	"oja/api/internal/buyback"
	"oja/api/internal/clearing"
	"oja/api/internal/fee"
	"oja/api/internal/issuer"
	"oja/api/internal/ledger"
	"oja/api/internal/migrate"
	"oja/api/internal/money"
	"oja/api/internal/share"
	"oja/api/internal/tapcrypto"
)

const sessionDate = "2026-09-11"

func pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = "postgres://oja_scheme:oja@localhost:5432/oja"
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
	t.Cleanup(func() {
		// The whole-ledger invariant, checked after every end-to-end run. If a
		// single kobo or share unit was created or destroyed anywhere in the
		// rail, this is what says so.
		if imb, err := ledger.AssetImbalance(ctx, p); err == nil && len(imb) > 0 {
			t.Errorf("ledger does not balance globally: %v", imb)
		}
		p.Close()
	})
	return p
}

func TestGoldenPath(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)

	// ---------------------------------------------------------------- the tap
	var auth issuer.Response
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		auth, err = n.tap(ctx, tx, money.Naira(10_000))
		return err
	})
	if !auth.Approved {
		t.Fatalf("tap declined: %s (%s)", auth.DeclineCode, auth.Message)
	}
	if auth.WriteBack == nil {
		t.Fatal("an NTAG215 approval must return the next rolling token for the PoS to write")
	}

	// The hold moved value sideways, not out.
	if got := n.balance(t, p, ledger.KindAvailable); got != money.Naira(40_000) {
		t.Errorf("available after the hold = %s, want ₦40,000.00", got)
	}
	if got := n.balance(t, p, ledger.KindHold); got != money.Naira(10_000) {
		t.Errorf("held = %s, want ₦10,000.00", got)
	}

	// ------------------------------------------------------------- capture
	mustTx(t, p, func(tx pgx.Tx) error {
		_, err := acquirer.Capture(ctx, tx, auth.AuthID, money.Naira(10_000))
		return err
	})

	// ------------------------------------------------------------- clearing
	var cleared clearing.Result
	mustTx(t, p, func(tx pgx.Tx) error {
		batch, err := clearing.OpenBatch(ctx, tx, sessionDate)
		if err != nil {
			return err
		}
		c := &clearing.Clearer{Schedules: clearing.StaticSchedules{1: fee.SchemeV1()}}
		cleared, err = c.Run(ctx, tx, batch)
		return err
	})

	if cleared.Presentments != 1 {
		t.Fatalf("cleared %d presentments, want 1", cleared.Presentments)
	}
	if cleared.Fees != money.Naira(50) {
		t.Errorf("fees = %s, want ₦50.00 (0.5%% of ₦10,000)", cleared.Fees)
	}
	if cleared.BuybackFunding != money.Naira(5) {
		t.Errorf("buyback funding = %s, want ₦5.00", cleared.BuybackFunding)
	}

	// The hold is consumed and the merchant is owed the net.
	if got := n.balance(t, p, ledger.KindHold); got != 0 {
		t.Errorf("hold after clearing = %s, want nothing", got)
	}
	if got := n.merchantBalance(t, p); got != money.Naira(9_950) {
		t.Errorf("merchant receivable = %s, want ₦9,950.00", got)
	}

	// ------------------------------------------------------------- buyback
	var b buyback.Result
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		// The trailing band is disabled here: a brand-new listing has no traded
		// history, and the daily release cap is the binding defence. The band
		// is exercised in TestWashTradingIsCapped.
		e := &buyback.Engine{TrailingBandSessions: 0}
		b, err = e.RunSession(ctx, tx, n.instrumentID, sessionDate)
		return err
	})

	if b.Intents != 1 {
		t.Fatalf("buyback saw %d intents, want 1", b.Intents)
	}
	if b.Price != money.Naira(40) {
		t.Errorf("execution price = %s, want ₦40.00", b.Price)
	}

	// ₦5 at ₦40 a share is 0.125 shares, exactly.
	want := share.PerShare / 8
	if b.Units != want {
		t.Errorf("bought %s units, want %s", b.Units, want)
	}

	// ---------------------------------------------------- the thing itself
	held := n.shares(t, p)
	if held != want {
		t.Fatalf("the cardholder owns %s of %s, want %s", held, n.symbol, want)
	}
	t.Logf("one ₦10,000 tap at %s: the cardholder now owns %s shares", n.symbol, held)

	// The treasury gave up exactly what the wallet gained.
	if treas := n.treasuryShares(t, p); treas != share.Whole(1_000)-want {
		t.Errorf("treasury holds %s, want %s", treas, share.Whole(1_000)-want)
	}

	// And a holding lot exists with its cost basis and its chargeback lock.
	var units, cost int64
	var lockedUntil string
	if err := p.QueryRow(ctx, `
		SELECT l.units, l.cost_kobo, l.transferable_from::text
		  FROM holding_lots l JOIN accounts a ON a.id = l.account_id
		 WHERE a.owner_id = $1 AND l.instrument_id = $2`,
		n.cardholderID, n.instrumentID).Scan(&units, &cost, &lockedUntil); err != nil {
		t.Fatalf("no holding lot was opened: %v", err)
	}
	if share.Units(units) != want {
		t.Errorf("lot holds %s units, want %s", share.Units(units), want)
	}
	if money.Kobo(cost) != money.Naira(5) {
		t.Errorf("lot cost basis = %s, want ₦5.00", money.Kobo(cost))
	}
	if lockedUntil != "2027-01-09" {
		t.Errorf("lot unlocks %s, want 2027-01-09 (the 120-day chargeback window)", lockedUntil)
	}
}

// Merchant co-funding is the lever that makes the buyback material. Same
// ticket, one percent opted in, twenty-one times the equity.
func TestMerchantCoFundingMultipliesTheBuyback(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)

	mustExec(t, p, `UPDATE merchants SET cofund_bps = 100 WHERE id = $1`, n.merchantID)

	var auth issuer.Response
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		auth, err = n.tap(ctx, tx, money.Naira(10_000))
		return err
	})
	if !auth.Approved {
		t.Fatalf("declined: %s", auth.DeclineCode)
	}
	mustTx(t, p, func(tx pgx.Tx) error {
		_, err := acquirer.Capture(ctx, tx, auth.AuthID, money.Naira(10_000))
		return err
	})

	var cleared clearing.Result
	mustTx(t, p, func(tx pgx.Tx) error {
		batch, err := clearing.OpenBatch(ctx, tx, sessionDate)
		if err != nil {
			return err
		}
		c := &clearing.Clearer{Schedules: clearing.StaticSchedules{1: fee.SchemeV1()}}
		cleared, err = c.Run(ctx, tx, batch)
		return err
	})

	if cleared.BuybackFunding != money.Naira(105) {
		t.Fatalf("buyback funding with 1%% co-funding = %s, want ₦105.00", cleared.BuybackFunding)
	}
	// The merchant pays the co-funding; the issuer's interchange is untouched.
	if got := n.merchantBalance(t, p); got != money.Naira(9_850) {
		t.Errorf("merchant receivable = %s, want ₦9,850.00 (₦9,950 less ₦100 co-funding)", got)
	}

	mustTx(t, p, func(tx pgx.Tx) error {
		e := &buyback.Engine{TrailingBandSessions: 0}
		_, err := e.RunSession(ctx, tx, n.instrumentID, sessionDate)
		return err
	})

	// ₦105 at ₦40 = 2.625 shares, against 0.125 on scheme fees alone.
	want := share.Units(2_62_500_000)
	if got := n.shares(t, p); got != want {
		t.Fatalf("co-funded tap bought %s shares, want %s", got, want)
	}
}

// Clearing will die halfway at some point. Re-running it must complete the work,
// not double it.
func TestClearingIsIdempotent(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)

	var auth issuer.Response
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		auth, err = n.tap(ctx, tx, money.Naira(10_000))
		return err
	})
	mustTx(t, p, func(tx pgx.Tx) error {
		_, err := acquirer.Capture(ctx, tx, auth.AuthID, money.Naira(10_000))
		return err
	})

	run := func() clearing.Result {
		var r clearing.Result
		mustTx(t, p, func(tx pgx.Tx) error {
			batch, err := clearing.OpenBatch(ctx, tx, sessionDate)
			if err != nil {
				return err
			}
			c := &clearing.Clearer{Schedules: clearing.StaticSchedules{1: fee.SchemeV1()}}
			r, err = c.Run(ctx, tx, batch)
			return err
		})
		return r
	}

	first := run()
	if first.Presentments != 1 {
		t.Fatalf("first run cleared %d, want 1", first.Presentments)
	}
	second := run()
	if second.Presentments != 0 {
		t.Fatalf("re-running clearing picked up %d presentments again", second.Presentments)
	}
	if got := n.merchantBalance(t, p); got != money.Naira(9_950) {
		t.Fatalf("merchant receivable = %s after two runs, want ₦9,950.00", got)
	}

	// And the buyback session is equally re-runnable.
	runBuyback := func() buyback.Result {
		var r buyback.Result
		mustTx(t, p, func(tx pgx.Tx) error {
			var err error
			e := &buyback.Engine{TrailingBandSessions: 0}
			r, err = e.RunSession(ctx, tx, n.instrumentID, sessionDate)
			return err
		})
		return r
	}
	runBuyback()
	after := n.shares(t, p)
	runBuyback()
	if again := n.shares(t, p); again != after {
		t.Fatalf("re-running the buyback allocated again: %s then %s", after, again)
	}
}

// The daily release cap is the hard stop on what a listing can extract from the
// fee pool in one session, whatever it does to its own price.
func TestTreasuryReleaseCapBinds(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)

	// Allow only a hundredth of a share to be released today.
	mustExec(t, p, `UPDATE treasury_pools SET daily_release_units = $2 WHERE instrument_id = $1`,
		n.instrumentID, int64(share.PerShare/100))

	var auth issuer.Response
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		auth, err = n.tap(ctx, tx, money.Naira(10_000))
		return err
	})
	mustTx(t, p, func(tx pgx.Tx) error {
		_, err := acquirer.Capture(ctx, tx, auth.AuthID, money.Naira(10_000))
		return err
	})
	mustTx(t, p, func(tx pgx.Tx) error {
		batch, err := clearing.OpenBatch(ctx, tx, sessionDate)
		if err != nil {
			return err
		}
		c := &clearing.Clearer{Schedules: clearing.StaticSchedules{1: fee.SchemeV1()}}
		_, err = c.Run(ctx, tx, batch)
		return err
	})

	var b buyback.Result
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		e := &buyback.Engine{TrailingBandSessions: 0}
		b, err = e.RunSession(ctx, tx, n.instrumentID, sessionDate)
		return err
	})

	if !b.Capped {
		t.Fatal("the release cap should have bound")
	}
	if b.Units != share.PerShare/100 {
		t.Fatalf("released %s units, want the %s cap", b.Units, share.PerShare/100)
	}
	// The cardholder is charged only for what treasury actually released.
	if b.Spent > b.Funding {
		t.Fatalf("spent %s against %s of funding", b.Spent, b.Funding)
	}
}

func mustTx(t *testing.T, p *pgxpool.Pool, fn func(pgx.Tx) error) {
	t.Helper()
	ctx := context.Background()
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := fn(tx); err != nil {
		t.Fatalf("transaction: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func mustExec(t *testing.T, p *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := p.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

var _ = uuid.Nil
var _ = tapcrypto.TechNTAG215
