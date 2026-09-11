package exchange

import (
	"testing"

	"freedom/api/internal/money"
	"freedom/api/internal/share"
)

// FuzzCross drives the engine from raw bytes and checks every invariant plus
// agreement with the brute-force oracle.
//
// The oracle comparison is the valuable half: it evaluates executable volume at
// every tick in the band, so any book where the candidate set misses the true
// maximum is a failure the fuzzer can find on its own.
func FuzzCross(f *testing.F) {
	f.Add([]byte{1, 0, 40, 10, 2, 1, 38, 10})
	f.Add([]byte{0, 1, 0, 5, 0, 0, 0, 5})
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		p := Params{
			PrevRef: money.Naira(40),
			Tick:    1,
			Lot:     1,
			BandLo:  money.Naira(32),
			BandHi:  money.Naira(48),
		}

		// Four bytes per order: side, type, price offset, quantity.
		var book []Order
		for i := 0; i+3 < len(data) && len(book) < 24; i += 4 {
			o := Order{Seq: int64(len(book) + 1)}
			if data[i]&1 == 0 {
				o.Side = Buy
			} else {
				o.Side = Sell
			}
			if data[i+1]&3 == 0 {
				o.Type = TypeMarket
			} else {
				o.Type = TypeLimit
				o.Limit = p.BandLo + money.Kobo(data[i+2])*money.Kobo(p.Tick)*8
				if o.Limit > p.BandHi {
					o.Limit = p.BandHi
				}
			}
			o.Qty = share.Units(int64(data[i+3])+1) * (share.PerShare / 100)
			book = append(book, o)
		}

		r, err := Cross(book, p)
		if err != nil {
			t.Fatalf("Cross returned an error on a well-formed book: %v", err)
		}

		wantExec, _ := bruteForce(book, p)
		if r.Exec != wantExec {
			t.Fatalf("Cross found %s executable; brute force found %s\nbook: %+v", r.Exec, wantExec, book)
		}
		if wantExec == 0 {
			if r.Determined {
				t.Fatal("nothing crossed but a price was published")
			}
			return
		}
		assertInvariants(t, book, p, r)
	})
}
