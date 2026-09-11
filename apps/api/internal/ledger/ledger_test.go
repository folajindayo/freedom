package ledger

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"oja/api/internal/migrate"
	"oja/api/internal/money"
	"oja/api/internal/share"
)

const businessDate = "2026-09-11"

// testPool connects to the scheme role and applies migrations. DB-backed tests
// skip rather than fail when there is no database, so `go test ./...` stays
// useful on a machine that has not run docker compose.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = "postgres://oja_scheme:oja@localhost:5432/oja"
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Skipf("no database: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("no database at %s: %v", url, err)
	}
	if err := migrate.Up(ctx, pool); err != nil {
		pool.Close()
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() {
		// Every ledger test ends by proving the whole ledger still balances.
		// This is the check that catches what no unit test will.
		if imb, err := AssetImbalance(ctx, pool); err == nil && len(imb) > 0 {
			t.Errorf("ledger does not balance globally: %v", imb)
		}
		pool.Close()
	})
	return pool
}

func key(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("%s|%s", t.Name(), uuid.NewString())
}

func TestPostBalancedNaira(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	holder := uuid.New()
	avail, err := Resolve(ctx, pool, Cardholder(holder, KindAvailable, AssetNGN))
	if err != nil {
		t.Fatal(err)
	}
	ext, err := Resolve(ctx, pool, Scheme(KindExternal, AssetNGN))
	if err != nil {
		t.Fatal(err)
	}

	// Fund the cardholder: value enters the system from a bank rail.
	if _, err := Post(ctx, pool, Tx{
		EventType:      "funding.received",
		BusinessDate:   businessDate,
		IdempotencyKey: key(t),
		Entries: []Entry{
			{ext, NGN(-money.Naira(50_000)), "funding.external"},
			{avail, NGN(money.Naira(50_000)), "funding.credit"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	bal, err := NairaBalance(ctx, pool, Cardholder(holder, KindAvailable, AssetNGN))
	if err != nil {
		t.Fatal(err)
	}
	if bal != money.Naira(50_000) {
		t.Fatalf("balance %s, want ₦50,000.00", bal)
	}
}

// The defining feature of the engine: one event, two assets, each balancing on
// its own. This is a buyback.
func TestPostMultiAssetBuyback(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	symbol := "MAMAPUT" + fmt.Sprint(rand.Intn(1_000_000))
	eq := EquityAsset(symbol)
	if _, err := pool.Exec(ctx,
		`INSERT INTO assets (id, class, scale, label) VALUES ($1,'equity',8,$2)`,
		eq, symbol); err != nil {
		t.Fatal(err)
	}

	holder, company := uuid.New(), uuid.New()
	pool2, _ := Resolve(ctx, pool, Scheme(KindBuybackPool, AssetNGN))
	tcash, _ := Resolve(ctx, pool, Company(company, KindTreasuryCash, AssetNGN))
	treas, _ := Resolve(ctx, pool, Company(company, KindTreasury, eq))
	wallet, _ := Resolve(ctx, pool, Cardholder(holder, KindStockWallet, eq))
	ext, _ := Resolve(ctx, pool, Scheme(KindExternal, AssetNGN))
	extEq, err := Resolve(ctx, pool, Scheme(KindExternal, eq))
	if err != nil {
		t.Fatal(err)
	}

	// Seed: fee income into the pool, and authorised shares into treasury.
	if _, err := Post(ctx, pool, Tx{
		EventType: "seed", BusinessDate: businessDate, IdempotencyKey: key(t),
		Entries: []Entry{
			{ext, NGN(-money.Naira(5)), "seed.fees"},
			{pool2, NGN(money.Naira(5)), "seed.fees"},
			{extEq, Equity(symbol, -share.Whole(1_000)), "seed.authorised"},
			{treas, Equity(symbol, share.Whole(1_000)), "seed.authorised"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// The buyback: ₦5 of pool money buys 0.125 shares at ₦40.
	units, spent, residual, err := share.UnitsFor(money.Naira(5), money.Naira(40))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Post(ctx, pool, Tx{
		EventType: "buyback.allocated", BusinessDate: businessDate, IdempotencyKey: key(t),
		Entries: []Entry{
			{pool2, NGN(-spent), "buyback.funding"},
			{tcash, NGN(spent), "buyback.proceeds"},
			{treas, Equity(symbol, -units), "buyback.treasury_release"},
			{wallet, Equity(symbol, units), "buyback.allocation"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := ShareBalance(ctx, pool, Cardholder(holder, KindStockWallet, eq))
	if err != nil {
		t.Fatal(err)
	}
	if got != share.PerShare/8 {
		t.Fatalf("stock wallet holds %s, want 0.125", got)
	}
	if residual != 0 {
		t.Fatalf("residual %s on an exact division", residual)
	}
}

// An event that balances in naira but not in equity must be rejected. Summing
// the two together would have let this through.
func TestPerAssetBalanceIsEnforced(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	symbol := "SKEW" + fmt.Sprint(rand.Intn(1_000_000))
	eq := EquityAsset(symbol)
	if _, err := pool.Exec(ctx,
		`INSERT INTO assets (id, class, scale, label) VALUES ($1,'equity',8,$2)`, eq, symbol); err != nil {
		t.Fatal(err)
	}
	a, _ := Resolve(ctx, pool, Scheme(KindBuybackPool, AssetNGN))
	b, _ := Resolve(ctx, pool, Scheme(KindRevenue, AssetNGN))
	c, err := Resolve(ctx, pool, Scheme(KindSuspense, eq))
	if err != nil {
		t.Fatal(err)
	}

	_, err = Post(ctx, pool, Tx{
		EventType: "bad", BusinessDate: businessDate, IdempotencyKey: key(t),
		Entries: []Entry{
			{a, NGN(-100), "x"},
			{b, NGN(100), "x"},
			{c, Equity(symbol, 5), "orphan equity leg"}, // balances in NGN, not in equity
		},
	})
	if err == nil {
		t.Fatal("an equity leg that does not balance must be rejected")
	}
	if !strings.Contains(err.Error(), eq) {
		t.Fatalf("error should name the offending asset, got: %v", err)
	}
}

// Replays are the normal case on a switch, not an exception.
func TestIdempotencyKeyIsEnforced(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	a, _ := Resolve(ctx, pool, Scheme(KindFloat, AssetNGN))
	b, err := Resolve(ctx, pool, Scheme(KindExternal, AssetNGN))
	if err != nil {
		t.Fatal(err)
	}
	// Scheme accounts are singletons that outlive any one test, so the
	// assertion is on the movement rather than on the absolute balance.
	before, err := NairaBalance(ctx, pool, Scheme(KindFloat, AssetNGN))
	if err != nil {
		t.Fatal(err)
	}

	tx := Tx{
		EventType: "replay", BusinessDate: businessDate, IdempotencyKey: key(t),
		Entries: []Entry{{a, NGN(500), "in"}, {b, NGN(-500), "out"}},
	}

	if _, err := Post(ctx, pool, tx); err != nil {
		t.Fatal(err)
	}
	_, err = Post(ctx, pool, tx)
	if err == nil {
		t.Fatal("a replayed idempotency key must not post twice")
	}
	if !strings.Contains(err.Error(), ErrAlreadyPosted.Error()) {
		t.Fatalf("want ErrAlreadyPosted, got %v", err)
	}

	after, _ := NairaBalance(ctx, pool, Scheme(KindFloat, AssetNGN))
	if after-before != 500 {
		t.Fatalf("float moved by %s across a replayed post, want ₦5.00 — the money moved twice",
			after-before)
	}
}

// Settlement finality: a committed transaction can never be amended, so a
// correction has to be a new transaction with its own audit trail.
func TestCommittedTransactionIsClosed(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	a, _ := Resolve(ctx, pool, Scheme(KindFloat, AssetNGN))
	b, err := Resolve(ctx, pool, Scheme(KindExternal, AssetNGN))
	if err != nil {
		t.Fatal(err)
	}
	txID, err := Post(ctx, pool, Tx{
		EventType: "final", BusinessDate: businessDate, IdempotencyKey: key(t),
		Entries: []Entry{{a, NGN(100), "in"}, {b, NGN(-100), "out"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = pool.Exec(ctx, `
		INSERT INTO ledger_entries (tx_id, account_id, asset_id, amount, reason)
		VALUES ($1, $2, 'NGN', 999999, 'backdated theft')`, txID, a)
	if err == nil {
		t.Fatal("appending an entry to a committed transaction must be rejected")
	}
	if !strings.Contains(err.Error(), "closed") {
		t.Fatalf("want a closed-transaction error, got %v", err)
	}
}

// The Go-side check must fire before the write, so the caller gets an error
// naming the asset rather than a deferred trigger failure with no context.
func TestUnbalancedIsRejectedBeforeWriting(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	a, _ := Resolve(ctx, pool, Scheme(KindFloat, AssetNGN))
	b, err := Resolve(ctx, pool, Scheme(KindExternal, AssetNGN))
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name    string
		entries []Entry
		want    string
	}{
		{"unbalanced", []Entry{{a, NGN(100), "in"}, {b, NGN(-99), "out"}}, "unbalanced"},
		{"single entry", []Entry{{a, NGN(100), "in"}}, "at least two"},
		{"zero amount", []Entry{{a, NGN(0), "in"}, {b, NGN(0), "out"}}, "not a movement"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Post(ctx, pool, Tx{
				EventType: "bad", BusinessDate: businessDate, IdempotencyKey: key(t),
				Entries: tc.entries,
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error containing %q, got %v", tc.want, err)
			}
		})
	}
}

// A hold is not spendable. Authorisation moves value sideways rather than out,
// so that a reversal can put it back without needing to find it again.
func TestHoldIsNotSpendable(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	holder := uuid.New()

	avail, _ := Resolve(ctx, pool, Cardholder(holder, KindAvailable, AssetNGN))
	hold, _ := Resolve(ctx, pool, Cardholder(holder, KindHold, AssetNGN))
	ext, err := Resolve(ctx, pool, Scheme(KindExternal, AssetNGN))
	if err != nil {
		t.Fatal(err)
	}

	mustPost(t, pool, Tx{EventType: "fund", BusinessDate: businessDate, IdempotencyKey: key(t),
		Entries: []Entry{{ext, NGN(-money.Naira(10_000)), "f"}, {avail, NGN(money.Naira(10_000)), "f"}}})
	mustPost(t, pool, Tx{EventType: "auth.hold", BusinessDate: businessDate, IdempotencyKey: key(t),
		Entries: []Entry{
			{avail, NGN(-money.Naira(2_500)), "auth.hold"},
			{hold, NGN(money.Naira(2_500)), "auth.hold"}}})

	a, _ := NairaBalance(ctx, pool, Cardholder(holder, KindAvailable, AssetNGN))
	h, _ := NairaBalance(ctx, pool, Cardholder(holder, KindHold, AssetNGN))
	if a != money.Naira(7_500) || h != money.Naira(2_500) {
		t.Fatalf("available %s, held %s; want ₦7,500.00 and ₦2,500.00", a, h)
	}
}

func mustPost(t *testing.T, q Querier, tx Tx) uuid.UUID {
	t.Helper()
	id, err := Post(context.Background(), q, tx)
	if err != nil {
		t.Fatalf("post %s: %v", tx.EventType, err)
	}
	return id
}

func init() { rand.Seed(time.Now().UnixNano()) }
