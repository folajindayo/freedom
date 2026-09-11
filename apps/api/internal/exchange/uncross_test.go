package exchange

import (
	"math/rand"
	"testing"

	"freedom/api/internal/money"
	"freedom/api/internal/share"
)

func params(ref money.Kobo) Params {
	return Params{
		PrevRef: ref,
		Tick:    1,
		Lot:     1,
		BandLo:  ref - ref*2000/10_000, // ±20%
		BandHi:  ref + ref*2000/10_000,
	}
}

func lim(seq int64, side Side, price money.Kobo, qty share.Units) Order {
	return Order{Seq: seq, Side: side, Type: TypeLimit, Limit: price, Qty: qty}
}

func mkt(seq int64, side Side, qty share.Units) Order {
	return Order{Seq: seq, Side: side, Type: TypeMarket, Qty: qty}
}

func TestSimpleCross(t *testing.T) {
	p := params(money.Naira(40))
	book := []Order{
		lim(1, Buy, money.Naira(41), share.Whole(10)),
		lim(2, Sell, money.Naira(39), share.Whole(10)),
	}
	r, err := Cross(book, p)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Determined {
		t.Fatal("a crossing book must determine a price")
	}
	if r.Exec != share.Whole(10) {
		t.Fatalf("exec = %s, want 10", r.Exec)
	}
	assertInvariants(t, book, p, r)
}

// The disaster the original ladder selected.
//
// Market orders on both sides and no limits: executable volume is positive and
// identical at every price, so the tied interval is the whole band and the
// surplus-side rung would take its top edge. One 0.01-share trade would print
// at reference × 1.20 and become tomorrow's buyback price.
func TestMarketOrdersBothSidesPrintAtTheReference(t *testing.T) {
	ref := money.Naira(40)
	p := params(ref)
	book := []Order{
		mkt(1, Buy, share.PerShare/100),
		mkt(2, Sell, share.PerShare/100),
	}
	r, err := Cross(book, p)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Determined {
		t.Fatal("two crossing market orders must trade")
	}
	if r.Price != ref {
		t.Fatalf("price = %s, want the reference %s — a band edge is a price nobody named", r.Price, ref)
	}
	if r.Price == p.BandHi || r.Price == p.BandLo {
		t.Fatal("the auction printed at a band edge")
	}
	assertInvariants(t, book, p, r)
}

// When the reference lies strictly inside the tied interval it IS the price.
// Picking the nearest tied endpoint manufactures a jump to the edge of the
// interval on a day when nothing happened.
func TestReferenceInsideTiedIntervalWins(t *testing.T) {
	ref := money.Kobo(10_400) // ₦104.00
	p := params(ref)
	// A buyer at ₦110 and a seller at ₦100 cross. Every price in [100,110]
	// clears the same volume with zero imbalance.
	book := []Order{
		lim(1, Buy, money.Naira(110), share.Whole(5)),
		lim(2, Sell, money.Naira(100), share.Whole(5)),
	}
	r, err := Cross(book, p)
	if err != nil {
		t.Fatal(err)
	}
	if r.Price != ref {
		t.Fatalf("price = %s, want the reference %s", r.Price, ref)
	}
	if r.Rule != RuleReference {
		t.Errorf("rule = %s, want %s", r.Rule, RuleReference)
	}
	assertInvariants(t, book, p, r)
}

// The surplus side pays: unfilled buyers bid it up, unfilled sellers push down.
func TestSurplusSidePays(t *testing.T) {
	p := params(money.Naira(40))

	// A buyer for 50 at ₦44 against sellers of 5 at ₦42 and 5 at ₦43. Both ₦43
	// and ₦44 clear all 10 units with the same surplus of 40, so rung 2 does not
	// separate them and rung 3 decides: with buyers left unfilled, competition
	// among them carries the price to the top of the tied range and the buyer
	// pays their full bid.
	t.Run("buy surplus takes the higher price", func(t *testing.T) {
		book := []Order{
			lim(1, Buy, money.Naira(44), share.Whole(50)),
			lim(2, Sell, money.Naira(42), share.Whole(5)),
			lim(3, Sell, money.Naira(43), share.Whole(5)),
		}
		r, err := Cross(book, p)
		if err != nil {
			t.Fatal(err)
		}
		if r.Imbalance <= 0 {
			t.Fatalf("expected a buy surplus, got imbalance %s", r.Imbalance)
		}
		if r.Exec != share.Whole(10) {
			t.Fatalf("exec = %s, want 10 — both sellers fill", r.Exec)
		}
		if r.Price != money.Naira(44) {
			t.Fatalf("price = %s, want ₦44.00", r.Price)
		}
		if r.Rule != RuleSurplusSide {
			t.Errorf("rule = %s, want %s", r.Rule, RuleSurplusSide)
		}
		assertInvariants(t, book, p, r)
	})

	t.Run("sell surplus takes the lower price", func(t *testing.T) {
		book := []Order{
			lim(1, Sell, money.Naira(36), share.Whole(50)),
			lim(2, Buy, money.Naira(38), share.Whole(5)),
			lim(3, Buy, money.Naira(37), share.Whole(5)),
		}
		r, err := Cross(book, p)
		if err != nil {
			t.Fatal(err)
		}
		if r.Imbalance >= 0 {
			t.Fatalf("expected a sell surplus, got imbalance %s", r.Imbalance)
		}
		if r.Exec != share.Whole(10) {
			t.Fatalf("exec = %s, want 10 — both buyers fill", r.Exec)
		}
		// Mirror image: unfilled sellers undercut each other down to the bottom
		// of the tied range.
		if r.Price != money.Naira(36) {
			t.Fatalf("price = %s, want ₦36.00", r.Price)
		}
		if r.Rule != RuleSurplusSide {
			t.Errorf("rule = %s, want %s", r.Rule, RuleSurplusSide)
		}
		assertInvariants(t, book, p, r)
	})
}

// Most sessions on a thin symbol look like this, and the honest answer is to
// carry the reference forward rather than invent a price.
func TestZeroVolumeSession(t *testing.T) {
	p := params(money.Naira(40))
	for _, tc := range []struct {
		name string
		book []Order
	}{
		{"empty book", nil},
		{"no crossing", []Order{
			lim(1, Buy, money.Naira(38), share.Whole(5)),
			lim(2, Sell, money.Naira(42), share.Whole(5)),
		}},
		{"only buyers", []Order{lim(1, Buy, money.Naira(41), share.Whole(5))}},
		{"market order with no opposing liquidity", []Order{mkt(1, Buy, share.Whole(5))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Cross(tc.book, p)
			if err != nil {
				t.Fatal(err)
			}
			if r.Determined {
				t.Fatalf("nothing crossed but a price of %s was published", r.Price)
			}
			if len(r.Fills) != 0 {
				t.Fatal("a zero-volume session produced fills")
			}
		})
	}
}

// Market orders alone can exceed the executable volume, so the walk must ration
// them rather than assume they all fill.
func TestSuperiorOrdersExceedExecutableVolume(t *testing.T) {
	p := params(money.Naira(40))
	book := []Order{
		mkt(1, Buy, share.Whole(1_000)),
		mkt(2, Buy, share.Whole(1_000)),
		lim(3, Sell, money.Naira(40), share.Whole(100)),
	}
	r, err := Cross(book, p)
	if err != nil {
		t.Fatal(err)
	}
	if r.Exec != share.Whole(100) {
		t.Fatalf("exec = %s, want 100 — limited by the only seller", r.Exec)
	}
	// Two equal market buys share the 100 available equally.
	for _, f := range r.Fills {
		if f.Side == Buy && f.Units != share.Whole(50) {
			t.Errorf("market buy %d got %s, want 50 (pro-rata of the marginal level)", f.Seq, f.Units)
		}
	}
	assertInvariants(t, book, p, r)
}

// Price-time priority: a more aggressive order fills before a less aggressive
// one, and among equals the earlier sequence wins nothing extra — they share.
func TestPriceThenTimePriority(t *testing.T) {
	p := params(money.Naira(40))
	book := []Order{
		lim(1, Buy, money.Naira(41), share.Whole(10)), // less aggressive
		lim(2, Buy, money.Naira(44), share.Whole(10)), // more aggressive, later
		lim(3, Sell, money.Naira(40), share.Whole(10)),
	}
	r, err := Cross(book, p)
	if err != nil {
		t.Fatal(err)
	}
	var filled map[int64]share.Units = map[int64]share.Units{}
	for _, f := range r.Fills {
		filled[f.Seq] = f.Units
	}
	if filled[2] != share.Whole(10) {
		t.Errorf("the ₦44 buyer got %s, want 10 — price beats time", filled[2])
	}
	if filled[1] != 0 {
		t.Errorf("the ₦41 buyer got %s, want nothing", filled[1])
	}
	assertInvariants(t, book, p, r)
}

// A cash-denominated buy sizes itself at the clearing price, and its demand
// still falls as price rises — which is what keeps the uncross well-behaved.
func TestCashDenominatedBuy(t *testing.T) {
	p := params(money.Naira(40))
	book := []Order{
		{Seq: 1, Side: Buy, Type: TypeLimit, Limit: money.Naira(40), Notional: money.Naira(200)},
		lim(2, Sell, money.Naira(40), share.Whole(10)),
	}
	r, err := Cross(book, p)
	if err != nil {
		t.Fatal(err)
	}
	// ₦200 at ₦40 is 5 shares.
	if r.Exec != share.Whole(5) {
		t.Fatalf("exec = %s, want 5 shares (₦200 at ₦40)", r.Exec)
	}
	assertInvariants(t, book, p, r)
}

func TestParamsValidation(t *testing.T) {
	book := []Order{lim(1, Buy, money.Naira(40), share.Whole(1))}
	for _, tc := range []struct {
		name string
		p    Params
	}{
		{"no reference price", Params{Tick: 1, Lot: 1, BandLo: 1, BandHi: 100}},
		{"zero tick", Params{PrevRef: 100, Lot: 1, BandLo: 1, BandHi: 100}},
		{"zero lot", Params{PrevRef: 100, Tick: 1, BandLo: 1, BandHi: 100}},
		{"inverted band", Params{PrevRef: 100, Tick: 1, Lot: 1, BandLo: 200, BandHi: 100}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Cross(book, tc.p); err == nil {
				t.Fatal("invalid params must be rejected")
			}
		})
	}
}

// Identical input, identical output — every time. A matching engine that is
// right nine runs in ten is a market that cannot be audited.
func TestCrossIsDeterministic(t *testing.T) {
	p := params(money.Naira(40))
	rng := rand.New(rand.NewSource(11))
	book := randomBook(rng, p, 40)

	first, err := Cross(book, p)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 500; i++ {
		again, err := Cross(book, p)
		if err != nil {
			t.Fatal(err)
		}
		if again.Price != first.Price || again.Exec != first.Exec || again.Rule != first.Rule {
			t.Fatalf("run %d diverged: %+v vs %+v", i, first, again)
		}
		if len(again.Fills) != len(first.Fills) {
			t.Fatalf("run %d produced %d fills, want %d", i, len(again.Fills), len(first.Fills))
		}
		for j := range first.Fills {
			if again.Fills[j] != first.Fills[j] {
				t.Fatalf("run %d fill %d diverged: %+v vs %+v", i, j, first.Fills[j], again.Fills[j])
			}
		}
	}
}

// The candidate set must never miss the true maximum. This is the check the
// brute-force oracle exists for.
func TestCrossMatchesBruteForceOracle(t *testing.T) {
	rng := rand.New(rand.NewSource(2027))
	p := params(money.Naira(40))

	for i := 0; i < 3_000; i++ {
		book := randomBook(rng, p, 1+rng.Intn(12))
		r, err := Cross(book, p)
		if err != nil {
			t.Fatalf("book %d: %v", i, err)
		}
		wantExec, _ := bruteForce(book, p)

		if r.Exec != wantExec {
			t.Fatalf("book %d: Cross found %s executable, brute force found %s at some tick\nbook: %+v",
				i, r.Exec, wantExec, book)
		}
		if wantExec == 0 && r.Determined {
			t.Fatalf("book %d: nothing was executable but a price was published", i)
		}
		if r.Determined {
			assertInvariants(t, book, p, r)
		}
	}
}

func randomBook(rng *rand.Rand, p Params, n int) []Order {
	book := make([]Order, 0, n)
	span := int64(p.BandHi - p.BandLo)
	for i := 0; i < n; i++ {
		o := Order{Seq: int64(i + 1), Qty: share.Units(rng.Int63n(int64(share.Whole(20))) + 1)}
		if rng.Intn(2) == 0 {
			o.Side = Buy
		} else {
			o.Side = Sell
		}
		if rng.Intn(5) == 0 {
			o.Type = TypeMarket
		} else {
			o.Type = TypeLimit
			o.Limit = p.BandLo + money.Kobo(rng.Int63n(span+1))
		}
		book = append(book, o)
	}
	return book
}

// assertInvariants checks everything that must be true of any uncross, whatever
// the book. Called from every positive test above and from the fuzzer.
func assertInvariants(t *testing.T, book []Order, p Params, r Result) {
	t.Helper()
	if !r.Determined {
		return
	}

	if r.Price < p.BandLo || r.Price > p.BandHi {
		t.Errorf("price %s is outside the band [%s, %s]", r.Price, p.BandLo, p.BandHi)
	}
	if p.Tick > 1 && int64(r.Price)%int64(p.Tick) != 0 {
		t.Errorf("price %s is off the %s tick", r.Price, p.Tick)
	}
	if !isAdmissible(book, p, r.Price) {
		t.Errorf("price %s was named by nobody — not a limit in the book, not the reference", r.Price)
	}

	byOrder := map[int64]share.Units{}
	var buy, sell share.Units
	for _, f := range r.Fills {
		if f.Units <= 0 {
			t.Errorf("fill for seq %d is %s", f.Seq, f.Units)
		}
		byOrder[f.Seq] += f.Units
		if f.Side == Buy {
			buy += f.Units
		} else {
			sell += f.Units
		}
	}
	if buy != r.Exec {
		t.Errorf("buy fills sum to %s, executable volume is %s", buy, r.Exec)
	}
	if sell != r.Exec {
		t.Errorf("sell fills sum to %s, executable volume is %s", sell, r.Exec)
	}

	for _, o := range book {
		got := byOrder[o.Seq]
		if got == 0 {
			continue
		}
		if !o.Executable(r.Price) {
			t.Errorf("order %d filled at %s but its limit is %s (%s)", o.Seq, r.Price, o.Limit, o.Side)
		}
		if max := o.QtyAt(r.Price, p.Lot); got > max {
			t.Errorf("order %d filled %s, more than its %s", o.Seq, got, max)
		}
	}
}

// Lot sizes. Flooring executable volume to a whole lot has to happen while
// choosing the price, not while allocating: floor each side's allocation
// afterwards and the two sides stop summing to the same number, which the
// ledger's balance trigger then rejects after the whole session is computed.
func TestLotSizeKeepsBothSidesEqual(t *testing.T) {
	p := params(money.Naira(40))
	p.Lot = share.PerShare // whole shares only

	book := []Order{
		// 10.5 shares wanted, 10.5 offered: only 10 whole lots may trade.
		{Seq: 1, Side: Buy, Type: TypeLimit, Limit: money.Naira(41), Qty: share.Whole(10) + share.PerShare/2},
		{Seq: 2, Side: Sell, Type: TypeLimit, Limit: money.Naira(39), Qty: share.Whole(10) + share.PerShare/2},
	}
	r, err := Cross(book, p)
	if err != nil {
		t.Fatal(err)
	}
	if r.Exec != share.Whole(10) {
		t.Fatalf("exec = %s, want exactly 10 whole shares", r.Exec)
	}
	if int64(r.Exec)%int64(p.Lot) != 0 {
		t.Fatalf("exec %s is not a whole number of lots", r.Exec)
	}
	assertInvariants(t, book, p, r)
}

// Pro-rata within the marginal level must also respect the lot, or a rationed
// allocation becomes a fraction of an untradable unit.
func TestLotSizeInProRata(t *testing.T) {
	p := params(money.Naira(40))
	p.Lot = share.PerShare

	book := []Order{
		lim(1, Buy, money.Naira(40), share.Whole(7)),
		lim(2, Buy, money.Naira(40), share.Whole(7)),
		lim(3, Sell, money.Naira(40), share.Whole(9)),
	}
	r, err := Cross(book, p)
	if err != nil {
		t.Fatal(err)
	}
	if r.Exec != share.Whole(9) {
		t.Fatalf("exec = %s, want 9", r.Exec)
	}
	for _, f := range r.Fills {
		if int64(f.Units)%int64(p.Lot) != 0 {
			t.Errorf("fill for seq %d is %s — not a whole lot", f.Seq, f.Units)
		}
	}
	assertInvariants(t, book, p, r)
}
