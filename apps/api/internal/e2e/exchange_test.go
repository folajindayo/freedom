package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"freedom/api/internal/acquirer"
	"freedom/api/internal/buyback"
	"freedom/api/internal/clearing"
	"freedom/api/internal/exchange"
	"freedom/api/internal/fee"
	"freedom/api/internal/issuer"
	"freedom/api/internal/ledger"
	"freedom/api/internal/money"
	"freedom/api/internal/share"
)

// tradeDate is well past the 120-day chargeback lock on the buyback lots the
// card rail creates, so shares earned by tapping are transferable by then.
const tradeDate = "2027-06-01"

// A full session: a seller offers shares earned from tapping, a buyer bids,
// the auction uncrosses, and both legs settle in one ledger transaction.
func TestExchangeSessionSettles(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)

	seller := n.cardholderID
	giveShares(t, p, n, seller, share.Whole(10), money.Naira(30))
	buyer := newCardholder(t, p, "Bola Adeyemi")
	fund(t, p, buyer, money.Naira(20_000))

	member, sellerAcct, buyerAcct := membership(t, p, n, seller, buyer)
	// The seller already holds card-funded naira, so proceeds are a delta.
	cashBefore := nairaOf(t, p, seller, ledger.KindAvailable)

	var result exchange.Result
	mustTx(t, p, func(tx pgx.Tx) error {
		e := exchange.NewEngine()
		s, err := e.Open(ctx, tx, n.instrumentID, tradeDate)
		if err != nil {
			return err
		}
		if _, err := e.Place(ctx, tx, s, exchange.OrderRequest{
			MemberID: member, ClientAccountID: sellerAcct.client, CardholderID: seller,
			AccountID: sellerAcct.ledger, Side: exchange.Sell, Type: exchange.TypeLimit,
			Limit: money.Naira(38), Qty: share.Whole(4),
		}); err != nil {
			return err
		}
		if _, err := e.Place(ctx, tx, s, exchange.OrderRequest{
			MemberID: member, ClientAccountID: buyerAcct.client, CardholderID: buyer,
			AccountID: buyerAcct.ledger, Side: exchange.Buy, Type: exchange.TypeLimit,
			Limit: money.Naira(42), Qty: share.Whole(4),
		}); err != nil {
			return err
		}
		result, err = e.RunToSettlement(ctx, tx, s)
		return err
	})

	if !result.Determined {
		t.Fatal("a crossing book did not determine a price")
	}
	if result.Exec != share.Whole(4) {
		t.Fatalf("exec = %s, want 4 shares", result.Exec)
	}

	// The seller delivered and the buyer received.
	if got := sharesOf(t, p, seller, n.instrumentID); got != share.Whole(6) {
		t.Errorf("seller holds %s, want 6 of the original 10", got)
	}
	if got := sharesOf(t, p, buyer, n.instrumentID); got != share.Whole(4) {
		t.Errorf("buyer holds %s, want 4", got)
	}

	// Cash moved the other way, net of fees.
	proceeds := nairaOf(t, p, seller, ledger.KindAvailable) - cashBefore
	gross, err := share.CostOf(result.Exec, result.Price)
	if err != nil {
		t.Fatal(err)
	}
	if proceeds <= 0 || proceeds >= gross {
		t.Fatalf("seller netted %s on a %s sale — expected something positive and below gross",
			proceeds, gross)
	}
	t.Logf("4 shares cleared at %s: the seller netted %s of %s gross", result.Price, proceeds, gross)

	// Every reservation is released. A reservation left behind is a cardholder's
	// money stranded in an account they cannot see.
	assertNoReservations(t, p)

	// The discovered price became the reference for the next session.
	var ref int64
	if err := p.QueryRow(ctx,
		`SELECT reference_price_kobo FROM instruments WHERE id = $1`, n.instrumentID).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	if money.Kobo(ref) != result.Price {
		t.Errorf("reference is %s, want the cleared price %s", money.Kobo(ref), result.Price)
	}
}

// Cash must conserve exactly: the pool is rounded once and split both ways, so
// what buyers pay is what sellers and the exchange receive, to the kobo.
func TestSettlementConservesCash(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)

	seller := n.cardholderID
	giveShares(t, p, n, seller, share.Whole(10), money.Naira(30))
	buyer := newCardholder(t, p, "Chidi Nwosu")
	fund(t, p, buyer, money.Naira(50_000))
	member, sa, ba := membership(t, p, n, seller, buyer)

	// A price and quantity chosen so the consideration does not divide evenly.
	mustTx(t, p, func(tx pgx.Tx) error {
		e := exchange.NewEngine()
		s, err := e.Open(ctx, tx, n.instrumentID, tradeDate)
		if err != nil {
			return err
		}
		if _, err := e.Place(ctx, tx, s, exchange.OrderRequest{
			MemberID: member, ClientAccountID: sa.client, CardholderID: seller,
			AccountID: sa.ledger, Side: exchange.Sell, Type: exchange.TypeLimit,
			Limit: money.Kobo(3_733), Qty: share.PerShare*3 + 7_777_777,
		}); err != nil {
			return err
		}
		if _, err := e.Place(ctx, tx, s, exchange.OrderRequest{
			MemberID: member, ClientAccountID: ba.client, CardholderID: buyer,
			AccountID: ba.ledger, Side: exchange.Buy, Type: exchange.TypeLimit,
			Limit: money.Kobo(4_111), Qty: share.PerShare*3 + 7_777_777,
		}); err != nil {
			return err
		}
		_, err = e.RunToSettlement(ctx, tx, s)
		return err
	})

	// The ledger's own per-asset invariant is checked in pool()'s cleanup; here
	// we check the exchange's books directly.
	var considerationBuy, considerationSell, fees int64
	if err := p.QueryRow(ctx, `
		SELECT COALESCE(SUM(consideration_kobo) FILTER (WHERE side='buy'), 0),
		       COALESCE(SUM(consideration_kobo) FILTER (WHERE side='sell'), 0),
		       COALESCE(SUM(fee_kobo), 0)
		  FROM fills`).Scan(&considerationBuy, &considerationSell, &fees); err != nil {
		t.Fatal(err)
	}
	if considerationBuy != considerationSell {
		t.Fatalf("buyers paid %s but sellers were credited %s — the pool did not conserve",
			money.Kobo(considerationBuy), money.Kobo(considerationSell))
	}
	if fees <= 0 {
		t.Error("no exchange fee was charged")
	}
	assertNoReservations(t, p)
}

// The chargeback lock, enforced where it matters: shares are visibly owned and
// not sellable. Without it a fraudster taps, takes the equity, sells it, and
// charges the tap back.
func TestBuybackSharesAreLockedFromSale(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)
	holder := n.cardholderID
	giveShares(t, p, n, holder, share.Whole(5), money.Naira(30))

	// Lock them the way a buyback allocation does.
	mustExec(t, p, `UPDATE holding_lots SET transferable_from = '2099-01-01'
	                 WHERE instrument_id = $1`, n.instrumentID)

	owned := sharesOf(t, p, holder, n.instrumentID)
	if owned != share.Whole(5) {
		t.Fatalf("wallet shows %s, want 5", owned)
	}
	sellable, err := exchange.Sellable(ctx, p, holder, n.instrumentID, tradeDate)
	if err != nil {
		t.Fatal(err)
	}
	if sellable != 0 {
		t.Fatalf("sellable is %s, want nothing — the lot is inside its chargeback window", sellable)
	}

	member, sa, _ := membership(t, p, n, holder, newCardholder(t, p, "Ngozi Eze"))
	err = inTx(p, func(tx pgx.Tx) error {
		e := exchange.NewEngine()
		s, err := e.Open(ctx, tx, n.instrumentID, tradeDate)
		if err != nil {
			return err
		}
		_, err = e.Place(ctx, tx, s, exchange.OrderRequest{
			MemberID: member, ClientAccountID: sa.client, CardholderID: holder,
			AccountID: sa.ledger, Side: exchange.Sell, Type: exchange.TypeLimit,
			Limit: money.Naira(38), Qty: share.Whole(3),
		})
		return err
	})
	if err == nil {
		t.Fatal("a locked lot was sold")
	}
	if !strings.Contains(err.Error(), "transferable") {
		t.Fatalf("want an insufficient-transferable-shares error, got: %v", err)
	}
}

// The issuer and its affiliates may not trade their own symbol. On a book this
// thin they are the party with both the motive and the treasury to set the
// price the buyback then pays.
func TestRelatedPartyCannotTrade(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)
	holder := n.cardholderID
	giveShares(t, p, n, holder, share.Whole(5), money.Naira(30))
	member, sa, _ := membership(t, p, n, holder, newCardholder(t, p, "Tunde Bakare"))

	mustExec(t, p, `
		INSERT INTO related_parties (instrument_id, account_id, group_key, relation, effective)
		VALUES ($1, $2, 'issuer-group', 'director', daterange('2020-01-01','2099-01-01'))`,
		n.instrumentID, sa.ledger)

	err := inTx(p, func(tx pgx.Tx) error {
		e := exchange.NewEngine()
		s, err := e.Open(ctx, tx, n.instrumentID, tradeDate)
		if err != nil {
			return err
		}
		_, err = e.Place(ctx, tx, s, exchange.OrderRequest{
			MemberID: member, ClientAccountID: sa.client, CardholderID: holder,
			AccountID: sa.ledger, Side: exchange.Sell, Type: exchange.TypeLimit,
			Limit: money.Naira(38), Qty: share.Whole(3),
		})
		return err
	})
	if err == nil {
		t.Fatal("a related party placed an order")
	}
	if !strings.Contains(err.Error(), "related parties") {
		t.Fatalf("want a related-party refusal, got: %v", err)
	}
}

// Nothing crossed. The reference carries forward and the staleness counter
// moves, so a symbol nobody trades is visibly stale rather than quietly
// pricing a buyback off a number from another month.
func TestZeroVolumeSessionCarriesForward(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)
	holder := n.cardholderID
	giveShares(t, p, n, holder, share.Whole(5), money.Naira(30))
	member, sa, _ := membership(t, p, n, holder, newCardholder(t, p, "Amara Obi"))

	var result exchange.Result
	mustTx(t, p, func(tx pgx.Tx) error {
		e := exchange.NewEngine()
		s, err := e.Open(ctx, tx, n.instrumentID, tradeDate)
		if err != nil {
			return err
		}
		// An ask nobody meets.
		if _, err := e.Place(ctx, tx, s, exchange.OrderRequest{
			MemberID: member, ClientAccountID: sa.client, CardholderID: holder,
			AccountID: sa.ledger, Side: exchange.Sell, Type: exchange.TypeLimit,
			Limit: money.Naira(47), Qty: share.Whole(3),
		}); err != nil {
			return err
		}
		result, err = e.RunToSettlement(ctx, tx, s)
		return err
	})

	if result.Determined {
		t.Fatal("nothing crossed but a price was published")
	}

	var source string
	var carried int
	if err := p.QueryRow(ctx, `
		SELECT po.source, i.carry_forward_sessions
		  FROM price_observations po JOIN instruments i ON i.id = po.instrument_id
		 WHERE po.instrument_id = $1 ORDER BY po.id DESC LIMIT 1`,
		n.instrumentID).Scan(&source, &carried); err != nil {
		t.Fatal(err)
	}
	if source != "carry_forward" {
		t.Errorf("observation source = %s, want carry_forward", source)
	}
	if carried != 1 {
		t.Errorf("carry_forward_sessions = %d, want 1", carried)
	}

	// And the unfilled seller got their shares back.
	if got := sharesOf(t, p, holder, n.instrumentID); got != share.Whole(5) {
		t.Errorf("holder has %s after an unfilled sell, want 5", got)
	}
	assertNoReservations(t, p)
}

// ---------------------------------------------------------------- helpers

type acct struct {
	client uuid.UUID
	ledger uuid.UUID
}

func membership(t *testing.T, p *pgxpool.Pool, n *network, a, b uuid.UUID) (uuid.UUID, acct, acct) {
	t.Helper()
	ctx := context.Background()
	var member uuid.UUID
	if err := p.QueryRow(ctx, `
		INSERT INTO members (code, legal_name, status) VALUES ($1,'Freedom Securities','active')
		ON CONFLICT (code) DO UPDATE SET status = 'active' RETURNING id`,
		"FSEC").Scan(&member); err != nil {
		t.Fatal(err)
	}
	return member, clientFor(t, p, n, member, a), clientFor(t, p, n, member, b)
}

func clientFor(t *testing.T, p *pgxpool.Pool, n *network, member, cardholder uuid.UUID) acct {
	t.Helper()
	ctx := context.Background()
	var out acct
	if err := p.QueryRow(ctx, `
		INSERT INTO client_accounts (member_id, cardholder_id, label) VALUES ($1,$2,'retail')
		ON CONFLICT (cardholder_id) WHERE cardholder_id IS NOT NULL
		  DO UPDATE SET label = 'retail' RETURNING id`,
		member, cardholder).Scan(&out.client); err != nil {
		t.Fatal(err)
	}
	// Orders trade from an account the cardholder owns; the stock wallet for
	// this instrument is the natural one and Resolve creates it on first use.
	mustTx(t, p, func(tx pgx.Tx) error {
		id, err := ledger.Resolve(ctx, tx,
			ledger.Cardholder(cardholder, ledger.KindStockWallet, n.instrumentID))
		out.ledger = id
		return err
	})
	return out
}

func newCardholder(t *testing.T, p *pgxpool.Pool, name string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := p.QueryRow(context.Background(), `
		INSERT INTO cardholders (phone, display_name, kyc_tier, bvn_verified_at,
		                         disclosure_accepted_at, disclosure_version)
		VALUES ($1,$2,2,now(),now(),'risk-disclosure-v1') RETURNING id`,
		"+234"+randHex(5), name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func fund(t *testing.T, p *pgxpool.Pool, holder uuid.UUID, amount money.Kobo) {
	t.Helper()
	ctx := context.Background()
	mustTx(t, p, func(tx pgx.Tx) error {
		ext, err := ledger.Resolve(ctx, tx, ledger.Scheme(ledger.KindExternal, ledger.AssetNGN))
		if err != nil {
			return err
		}
		avail, err := ledger.Resolve(ctx, tx, ledger.Cardholder(holder, ledger.KindAvailable, ledger.AssetNGN))
		if err != nil {
			return err
		}
		_, err = ledger.Post(ctx, tx, ledger.Tx{
			EventType: "funding.received", BusinessDate: tradeDate,
			IdempotencyKey: "fund|" + holder.String(),
			Entries: []ledger.Entry{
				{AccountID: ext, Amount: ledger.NGN(-amount), Reason: "funding"},
				{AccountID: avail, Amount: ledger.NGN(amount), Reason: "funding"},
			},
		})
		return err
	})
}

// giveShares puts transferable equity in a wallet, with a cost basis, the way a
// settled purchase would.
func giveShares(t *testing.T, p *pgxpool.Pool, n *network, holder uuid.UUID,
	units share.Units, price money.Kobo) {
	t.Helper()
	ctx := context.Background()
	cost, err := share.CostOf(units, price)
	if err != nil {
		t.Fatal(err)
	}
	var wallet uuid.UUID
	mustTx(t, p, func(tx pgx.Tx) error {
		treas, err := ledger.Resolve(ctx, tx, ledger.Company(n.companyID, ledger.KindTreasury, n.instrumentID))
		if err != nil {
			return err
		}
		wallet, err = ledger.Resolve(ctx, tx, ledger.Cardholder(holder, ledger.KindStockWallet, n.instrumentID))
		if err != nil {
			return err
		}
		txID, err := ledger.Post(ctx, tx, ledger.Tx{
			EventType: "seed.holding", BusinessDate: tradeDate,
			IdempotencyKey: "seed|" + holder.String() + "|" + n.instrumentID,
			Entries: []ledger.Entry{
				{AccountID: treas, Amount: ledger.Equity(n.symbol, -units), Reason: "seed"},
				{AccountID: wallet, Amount: ledger.Equity(n.symbol, units), Reason: "seed"},
			},
		})
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO holding_lots (account_id, instrument_id, units, units_open,
			                          cost_kobo, cost_open_kobo, transferable_from, ledger_tx_id)
			VALUES ($1,$2,$3,$3,$4,$4,'2026-01-01',$5)`,
			wallet, n.instrumentID, int64(units), int64(cost), txID)
		return err
	})
}

func sharesOf(t *testing.T, p *pgxpool.Pool, holder uuid.UUID, instrumentID string) share.Units {
	t.Helper()
	v, err := ledger.ShareBalance(context.Background(), p,
		ledger.Cardholder(holder, ledger.KindStockWallet, instrumentID))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func nairaOf(t *testing.T, p *pgxpool.Pool, holder uuid.UUID, kind string) money.Kobo {
	t.Helper()
	v, err := ledger.NairaBalance(context.Background(), p,
		ledger.Cardholder(holder, kind, ledger.AssetNGN))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// assertNoReservations is the leak check: after a published session every
// reserve account is empty and no lot is still earmarked.
func assertNoReservations(t *testing.T, p *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	rows, err := p.Query(ctx, `
		SELECT a.kind, a.asset_id, COALESCE(SUM(e.amount), 0)
		  FROM accounts a LEFT JOIN ledger_entries e ON e.account_id = a.id
		 WHERE a.kind IN ('order_cash_reserve','order_share_reserve')
		 GROUP BY a.id, a.kind, a.asset_id
		HAVING COALESCE(SUM(e.amount), 0) <> 0`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind, asset string
		var bal int64
		if err := rows.Scan(&kind, &asset, &bal); err != nil {
			t.Fatal(err)
		}
		t.Errorf("reservation left behind: %s holds %d of %s", kind, bal, asset)
	}

	var stuck int
	if err := p.QueryRow(ctx,
		`SELECT COUNT(*) FROM holding_lots WHERE units_reserved <> 0`).Scan(&stuck); err != nil {
		t.Fatal(err)
	}
	if stuck > 0 {
		t.Errorf("%d holding lots are still earmarked after settlement", stuck)
	}
}

func inTx(p *pgxpool.Pool, fn func(pgx.Tx) error) error {
	ctx := context.Background()
	tx, err := p.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// The attack the whole price-integrity design exists to resist.
//
// A listing with two accounts trades with itself on a thin book, doubling its
// own reference price, and then expects the buyback to spend scheme money at
// twice the going rate. The trailing volume-weighted average is what stops the
// extraction: one manipulated session cannot be cashed in, because the window
// behind it has not moved.
func TestWashTradingIsCapped(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)

	// Five honest sessions at ₦40 on real volume.
	for _, d := range []string{"2026-09-04", "2026-09-07", "2026-09-08", "2026-09-09", "2026-09-10"} {
		publishAuction(t, p, n, d, money.Naira(40), share.Whole(50))
	}
	// Then today's session prints at ₦80 — the ramp.
	publishAuction(t, p, n, sessionDate, money.Naira(80), share.Whole(1))

	tap(t, p, n)

	var b buyback.Result
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		e := buyback.New() // trailing band ON
		b, err = e.RunSession(ctx, tx, n.instrumentID, sessionDate)
		return err
	})

	if b.Price != money.Naira(40) {
		t.Fatalf("the buyback paid %s; the ramp to ₦80.00 should have been capped at the ₦40.00 trailing VWAP",
			b.Price)
	}
	t.Logf("session printed ₦80.00, buyback paid %s", b.Price)
}

// A trailing window containing one dust trade is not a control — it is a
// control that looks like one. The buyback must refuse rather than cap against
// a number nobody really traded at.
func TestThinVWAPWindowRefusesRatherThanCapping(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)

	// One prior session, traded in dust, far below the instrument's floor.
	publishAuction(t, p, n, "2026-09-10", money.Naira(40), 1)
	publishAuction(t, p, n, sessionDate, money.Naira(80), share.Whole(1))

	tap(t, p, n)

	var b buyback.Result
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		e := buyback.New()
		b, err = e.RunSession(ctx, tx, n.instrumentID, sessionDate)
		return err
	})

	if b.Refusal != buyback.RefusalThinVWAP {
		t.Fatalf("refusal = %q, want %q", b.Refusal, buyback.RefusalThinVWAP)
	}
	if b.Escrowed == 0 {
		t.Fatal("the funding must escrow, not vanish")
	}
	if got := n.shares(t, p); got != 0 {
		t.Fatalf("shares were allocated at an uncapped price: %s", got)
	}
}

// The buyback must not reach back to an older session's price when today's
// auction has not run. Doing so would let it pay a price from before whatever
// news moved the market.
func TestBuybackRefusesWithoutAPublishedAuction(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)

	// Unwind today's session entirely, so the instrument has a reference price
	// but no auction to take one from. ('published' is deliberately terminal,
	// so it cannot simply be cancelled.)
	mustExec(t, p, `DELETE FROM price_observations WHERE instrument_id = $1`, n.instrumentID)
	mustExec(t, p, `DELETE FROM auctions WHERE instrument_id = $1`, n.instrumentID)
	tap(t, p, n)

	var b buyback.Result
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		e := &buyback.Engine{TrailingBandSessions: 0}
		b, err = e.RunSession(ctx, tx, n.instrumentID, sessionDate)
		return err
	})

	if b.Refusal != buyback.RefusalNoSession {
		t.Fatalf("refusal = %q, want %q", b.Refusal, buyback.RefusalNoSession)
	}
	if b.Escrowed == 0 {
		t.Fatal("intents must escrow when there is no session to price against")
	}
}

// A halted instrument must not accrue a price, and must certainly not have one
// paid against it.
func TestBuybackRefusesOnHaltedInstrument(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)

	mustExec(t, p, `
		INSERT INTO trading_halts (instrument_id, reason, raised_by)
		VALUES ($1,'news_pending','surveillance')`, n.instrumentID)
	tap(t, p, n)

	var b buyback.Result
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		e := &buyback.Engine{TrailingBandSessions: 0}
		b, err = e.RunSession(ctx, tx, n.instrumentID, sessionDate)
		return err
	})
	if b.Refusal != buyback.RefusalHalted {
		t.Fatalf("refusal = %q, want %q", b.Refusal, buyback.RefusalHalted)
	}
}

// tap runs one ₦10,000 purchase all the way to a pending buyback intent.
func tap(t *testing.T, p *pgxpool.Pool, n *network) {
	t.Helper()
	ctx := context.Background()
	var auth issuer.Response
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		auth, err = n.tap(ctx, tx, money.Naira(10_000))
		return err
	})
	if !auth.Approved {
		t.Fatalf("tap declined: %s", auth.DeclineCode)
	}
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
}
