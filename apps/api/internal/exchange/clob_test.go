package exchange

import (
	"math/rand"
	"testing"

	"freedom/api/internal/money"
	"freedom/api/internal/share"
)

func resting(seq int64, side Side, price money.Kobo, qty share.Units) Resting {
	return Resting{Order: lim(seq, side, price, qty), Remaining: qty}
}

func book(orders ...Resting) *Book {
	b := &Book{}
	for _, o := range orders {
		b.Insert(o)
	}
	return b
}

// The trade prints at the RESTING order's price, never the aggressor's. A
// taker's limit is a worst-case bound; getting this backwards silently hands
// the spread to whoever crossed.
func TestTradePrintsAtTheRestingPrice(t *testing.T) {
	b := book(resting(1, Sell, money.Naira(40), share.Whole(10)))

	trades, rest, err := Match(b, lim(2, Buy, money.Naira(45), share.Whole(10)), DefaultRules())
	if err != nil {
		t.Fatal(err)
	}
	if len(trades) != 1 {
		t.Fatalf("got %d trades, want 1", len(trades))
	}
	if trades[0].Price != money.Naira(40) {
		t.Fatalf("printed at %s; the resting ask was ₦40.00 and the taker bid ₦45.00", trades[0].Price)
	}
	if rest.Remaining != 0 {
		t.Fatalf("%s left unfilled against sufficient depth", rest.Remaining)
	}
}

// Price beats time; among equal prices, time decides.
func TestPriceThenTimePriorityOnTheBook(t *testing.T) {
	b := book(
		resting(1, Sell, money.Naira(42), share.Whole(5)), // worse price, earlier
		resting(2, Sell, money.Naira(40), share.Whole(5)), // best price
		resting(3, Sell, money.Naira(40), share.Whole(5)), // same price, later
	)
	trades, _, err := Match(b, lim(4, Buy, money.Naira(45), share.Whole(12)), DefaultRules())
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		maker int64
		price money.Kobo
		units share.Units
	}{
		{2, money.Naira(40), share.Whole(5)},
		{3, money.Naira(40), share.Whole(5)},
		{1, money.Naira(42), share.Whole(2)},
	}
	if len(trades) != len(want) {
		t.Fatalf("got %d trades, want %d: %+v", len(trades), len(want), trades)
	}
	for i, w := range want {
		if trades[i].MakerSeq != w.maker || trades[i].Price != w.price || trades[i].Units != w.units {
			t.Errorf("trade %d = maker %d at %s for %s, want maker %d at %s for %s",
				i, trades[i].MakerSeq, trades[i].Price, trades[i].Units, w.maker, w.price, w.units)
		}
	}
}

// An aggressor never trades through its own limit, however deep the book.
func TestAggressorNeverTradesThroughItsLimit(t *testing.T) {
	b := book(
		resting(1, Sell, money.Naira(40), share.Whole(5)),
		resting(2, Sell, money.Naira(50), share.Whole(5)),
	)
	trades, rest, err := Match(b, lim(3, Buy, money.Naira(45), share.Whole(10)), DefaultRules())
	if err != nil {
		t.Fatal(err)
	}
	if len(trades) != 1 || trades[0].Price != money.Naira(40) {
		t.Fatalf("expected one fill at ₦40.00, got %+v", trades)
	}
	if rest.Remaining != share.Whole(5) {
		t.Fatalf("%s remaining, want 5 — the ₦50 ask is through the ₦45 limit", rest.Remaining)
	}
}

func TestMarketOrderSweepsAndDoesNotRest(t *testing.T) {
	b := book(resting(1, Sell, money.Naira(40), share.Whole(3)))
	trades, rest, err := Match(b, mkt(2, Buy, share.Whole(10)), DefaultRules())
	if err != nil {
		t.Fatal(err)
	}
	if len(trades) != 1 || trades[0].Units != share.Whole(3) {
		t.Fatalf("got %+v", trades)
	}
	b.Insert(rest)
	if len(b.Bids) != 0 {
		t.Fatal("an unfilled market order rested on the book")
	}
}

func TestNoCrossLeavesBookIntact(t *testing.T) {
	b := book(resting(1, Sell, money.Naira(45), share.Whole(5)))
	trades, rest, err := Match(b, lim(2, Buy, money.Naira(40), share.Whole(5)), DefaultRules())
	if err != nil {
		t.Fatal(err)
	}
	if len(trades) != 0 {
		t.Fatalf("a ₦40 bid traded against a ₦45 ask: %+v", trades)
	}
	b.Insert(rest)
	bid, ask := b.Top()
	if bid != money.Naira(40) || ask != money.Naira(45) {
		t.Fatalf("top of book %s / %s, want ₦40.00 / ₦45.00", bid, ask)
	}
}

func TestCancelRemovesFromEitherSide(t *testing.T) {
	b := book(
		resting(1, Buy, money.Naira(39), share.Whole(5)),
		resting(2, Sell, money.Naira(41), share.Whole(5)),
	)
	if !b.Cancel(1) || !b.Cancel(2) {
		t.Fatal("cancel did not find a resting order")
	}
	if bid, ask := b.Top(); bid != 0 || ask != 0 {
		t.Fatalf("book is not empty: %s / %s", bid, ask)
	}
	if b.Cancel(99) {
		t.Error("cancelling an unknown order reported success")
	}
}

// Conservation under random flow: every unit that leaves the book arrives in a
// trade, and nothing is created.
func TestMatchingConservesUnits(t *testing.T) {
	rng := rand.New(rand.NewSource(17))
	rules := DefaultRules()

	for iter := 0; iter < 2_000; iter++ {
		b := &Book{}
		var seq int64
		var resting share.Units

		for i := 0; i < 8; i++ {
			seq++
			side := Buy
			price := money.Naira(30 + rng.Int63n(10))
			if rng.Intn(2) == 0 {
				side = Sell
				price = money.Naira(40 + rng.Int63n(10))
			}
			qty := share.Units(rng.Int63n(int64(share.Whole(10))) + 1)
			o := lim(seq, side, price, qty)
			trades, rest, err := Match(b, o, rules)
			if err != nil {
				t.Fatal(err)
			}
			var traded share.Units
			for _, tr := range trades {
				if tr.Units <= 0 {
					t.Fatalf("non-positive trade size %s", tr.Units)
				}
				traded += tr.Units
			}
			if traded+rest.Remaining != qty {
				t.Fatalf("order %d: traded %s + remaining %s != %s", seq, traded, rest.Remaining, qty)
			}
			resting = resting - traded + rest.Remaining
			b.Insert(rest)
		}

		var onBook share.Units
		for _, r := range append(append([]Resting{}, b.Bids...), b.Asks...) {
			onBook += r.Remaining
		}
		if onBook != resting {
			t.Fatalf("book holds %s, expected %s", onBook, resting)
		}
	}
}

// The book is never crossed after a match: a bid above the best ask would mean
// a trade that should have happened and did not.
func TestBookIsNeverCrossed(t *testing.T) {
	rng := rand.New(rand.NewSource(23))
	rules := DefaultRules()
	b := &Book{}

	for seq := int64(1); seq <= 600; seq++ {
		side := Buy
		if rng.Intn(2) == 0 {
			side = Sell
		}
		o := lim(seq, side, money.Naira(35+rng.Int63n(11)),
			share.Units(rng.Int63n(int64(share.Whole(5)))+1))
		_, rest, err := Match(b, o, rules)
		if err != nil {
			t.Fatal(err)
		}
		b.Insert(rest)

		if bid, ask := b.Top(); bid != 0 && ask != 0 && bid >= ask {
			t.Fatalf("after order %d the book is crossed: bid %s >= ask %s", seq, bid, ask)
		}
	}
}

func TestCashDenominatedOrdersAreAuctionOnly(t *testing.T) {
	b := &Book{}
	o := Order{Seq: 1, Side: Buy, Type: TypeLimit, Limit: money.Naira(40), Notional: money.Naira(500)}
	if _, _, err := Match(b, o, DefaultRules()); err == nil {
		t.Fatal("a cash-denominated order was accepted onto the continuous book")
	}
}
