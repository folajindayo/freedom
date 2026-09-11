package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"freedom/api/internal/exchange"
	"freedom/api/internal/exchange/httpapi"
	"freedom/api/internal/money"
	"freedom/api/internal/scheme"
	"freedom/api/internal/share"
)

// venue wraps the HTTP surface with a fixed clock and a stub credential, so the
// tests exercise the real router, the real handlers and the real engine.
type venue struct {
	srv    *httptest.Server
	member uuid.UUID
}

func newVenue(t *testing.T, p *pgxpool.Pool, member uuid.UUID) *venue {
	t.Helper()
	return newVenueAt(t, p, member, tradeDate)
}

// newVenueAt pins the venue's clock to a given business date, so a test can
// exercise a market holiday without waiting for one.
func newVenueAt(t *testing.T, p *pgxpool.Pool, member uuid.UUID, date string) *venue {
	t.Helper()
	day, err := time.ParseInLocation("2006-01-02", date, scheme.Lagos)
	if err != nil {
		t.Fatal(err)
	}
	api := httpapi.New(p, func(r *http.Request) (uuid.UUID, error) {
		if r.Header.Get("X-Member-Token") != "valid-token" {
			return uuid.Nil, fmt.Errorf("bad token")
		}
		return member, nil
	})
	// A fixed clock, so the business date is the trading day the fixture set up
	// rather than whatever day the suite happens to run.
	at := time.Date(day.Year(), day.Month(), day.Day(), 11, 0, 0, 0, scheme.Lagos)
	api.Now = func() time.Time { return at }

	srv := httptest.NewServer(api.Routes())
	t.Cleanup(srv.Close)
	return &venue{srv: srv, member: member}
}

func (v *venue) do(t *testing.T, method, path string, body any, auth bool) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, v.srv.URL+path, &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if auth {
		req.Header.Set("X-Member-Token", "valid-token")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestAPIPlacesAndCancelsAnOrder(t *testing.T) {
	p := pool(t)
	n := setup(t, p)
	holder := n.cardholderID
	giveShares(t, p, n, holder, share.Whole(20), money.Naira(30))
	member, sa, _ := membership(t, p, n, holder, newCardholder(t, p, "Ola Fashina"))
	v := newVenue(t, p, member)

	status, body := v.do(t, "POST", "/v1/orders", map[string]any{
		"symbol": n.symbol, "side": "sell", "type": "limit",
		"limit_kobo": int64(money.Naira(38)), "units": int64(share.Whole(5)),
		"client_account_id": sa.client.String(), "client_order_id": "cl-001",
	}, true)
	if status != http.StatusCreated {
		t.Fatalf("place returned %d: %v", status, body)
	}
	orderID, _ := body["order_id"].(string)
	if orderID == "" {
		t.Fatalf("no order id in %v", body)
	}

	// Resending the same client order id is a retry, not a second order.
	// Without this a terminal that times out doubles the customer's position.
	status, again := v.do(t, "POST", "/v1/orders", map[string]any{
		"symbol": n.symbol, "side": "sell", "type": "limit",
		"limit_kobo": int64(money.Naira(38)), "units": int64(share.Whole(5)),
		"client_account_id": sa.client.String(), "client_order_id": "cl-001",
	}, true)
	if status != http.StatusCreated || again["order_id"] != orderID {
		t.Fatalf("a repeated client order id created a second order: %v", again)
	}

	status, view := v.do(t, "GET", "/v1/orders/"+orderID, nil, true)
	if status != http.StatusOK || view["state"] != "open" {
		t.Fatalf("get order returned %d: %v", status, view)
	}

	status, _ = v.do(t, "DELETE", "/v1/orders/"+orderID, nil, true)
	if status != http.StatusOK {
		t.Fatalf("cancel returned %d", status)
	}

	// Cancelling releases the reservation, and the shares are sellable again.
	assertNoReservations(t, p)
	sellable, err := exchange.Sellable(context.Background(), p, holder, n.instrumentID, tradeDate)
	if err != nil {
		t.Fatal(err)
	}
	if sellable != share.Whole(20) {
		t.Fatalf("%s sellable after a cancel, want all 20 back", sellable)
	}

	// Cancelling twice reports honestly rather than claiming success — a member
	// who believes they cancelled a fill will act on a position they do not have.
	if status, _ := v.do(t, "DELETE", "/v1/orders/"+orderID, nil, true); status != http.StatusConflict {
		t.Errorf("second cancel returned %d, want 409", status)
	}
}

// Every rule the exchange enforces gets its own code. Collapsing them to 400
// would make the API unusable: a member refused for insufficient funds needs to
// know that, and one refused because they are halted needs to stop sending.
func TestAPIRefusalsAreDistinguishable(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)
	holder := n.cardholderID
	member, sa, _ := membership(t, p, n, holder, newCardholder(t, p, "Ada Nwoke"))
	v := newVenue(t, p, member)

	sell := func(limit money.Kobo, units share.Units) (int, map[string]any) {
		return v.do(t, "POST", "/v1/orders", map[string]any{
			"symbol": n.symbol, "side": "sell", "type": "limit",
			"limit_kobo": int64(limit), "units": int64(units),
			"client_account_id": sa.client.String(),
		}, true)
	}

	t.Run("no shares to sell", func(t *testing.T) {
		status, body := sell(money.Naira(38), share.Whole(5))
		if status != http.StatusUnprocessableEntity || body["code"] != "insufficient_shares" {
			t.Fatalf("got %d %v", status, body)
		}
		// The message explains the chargeback lock, because that is the most
		// likely reason a cardholder's shares are not sellable.
		if msg, _ := body["message"].(string); !bytes.Contains([]byte(msg), []byte("chargeback")) {
			t.Errorf("message does not explain the lock: %q", msg)
		}
	})

	giveShares(t, p, n, holder, share.Whole(20), money.Naira(30))

	t.Run("below the minimum order size", func(t *testing.T) {
		status, body := sell(money.Naira(38), share.PerShare/100)
		if status != http.StatusUnprocessableEntity || body["code"] != "below_minimum" {
			t.Fatalf("got %d %v", status, body)
		}
	})

	t.Run("fat finger", func(t *testing.T) {
		mustExec(t, p, `UPDATE instruments SET max_order_units = $2 WHERE id = $1`,
			n.instrumentID, int64(share.Whole(10)))
		status, body := sell(money.Naira(38), share.Whole(15))
		if status != http.StatusUnprocessableEntity || body["code"] != "order_too_large" {
			t.Fatalf("got %d %v", status, body)
		}
		mustExec(t, p, `UPDATE instruments SET max_order_units = NULL WHERE id = $1`, n.instrumentID)
	})

	t.Run("instrument halted", func(t *testing.T) {
		mustTx(t, p, func(tx pgx.Tx) error {
			_, err := exchange.Halt(ctx, tx, n.instrumentID, exchange.HaltRegulatory, "sec", nil)
			return err
		})
		status, body := sell(money.Naira(38), share.Whole(5))
		if status != http.StatusConflict || body["code"] != "instrument_halted" {
			t.Fatalf("got %d %v", status, body)
		}
		mustTx(t, p, func(tx pgx.Tx) error {
			return exchange.Release(ctx, tx, n.instrumentID, "sec")
		})
	})

	t.Run("member kill switch", func(t *testing.T) {
		mustTx(t, p, func(tx pgx.Tx) error {
			return exchange.HaltMember(ctx, tx, member, "risk system offline")
		})
		status, body := sell(money.Naira(38), share.Whole(5))
		if status != http.StatusForbidden || body["code"] != "member_halted" {
			t.Fatalf("got %d %v", status, body)
		}
		mustTx(t, p, func(tx pgx.Tx) error { return exchange.ResumeMember(ctx, tx, member) })
	})

	t.Run("unauthenticated", func(t *testing.T) {
		status, body := v.do(t, "POST", "/v1/orders", map[string]any{"symbol": n.symbol}, false)
		if status != http.StatusUnauthorized {
			t.Fatalf("got %d %v", status, body)
		}
		// Deliberately uninformative: telling a caller which half of their
		// credential was wrong is an oracle.
		if body["message"] != "Credentials were not accepted" {
			t.Errorf("message leaks detail: %v", body["message"])
		}
	})
}

// One member must never reach another's orders, and an order belonging to
// somebody else reads as absent rather than forbidden — existence is itself
// information.
func TestAPIScopesOrdersToTheMember(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)
	holder := n.cardholderID
	giveShares(t, p, n, holder, share.Whole(20), money.Naira(30))
	member, sa, _ := membership(t, p, n, holder, newCardholder(t, p, "Ifeanyi Obi"))
	v := newVenue(t, p, member)

	status, body := v.do(t, "POST", "/v1/orders", map[string]any{
		"symbol": n.symbol, "side": "sell", "type": "limit",
		"limit_kobo": int64(money.Naira(38)), "units": int64(share.Whole(5)),
		"client_account_id": sa.client.String(),
	}, true)
	if status != http.StatusCreated {
		t.Fatalf("place returned %d: %v", status, body)
	}
	orderID := body["order_id"].(string)

	// A second member, who must see nothing.
	var rival uuid.UUID
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		rival, err = exchange.Admit(ctx, tx, "RIVAL", "Rival Securities", nil)
		if err != nil {
			return err
		}
		return exchange.Activate(ctx, tx, rival, "listings")
	})
	rv := newVenue(t, p, rival)

	if status, _ := rv.do(t, "GET", "/v1/orders/"+orderID, nil, true); status != http.StatusNotFound {
		t.Fatalf("another member read the order: status %d", status)
	}
	if status, _ := rv.do(t, "DELETE", "/v1/orders/"+orderID, nil, true); status != http.StatusNotFound {
		t.Fatalf("another member cancelled the order: status %d", status)
	}
}

// Market data is public: a price nobody can look up is not a market.
func TestAPIPublishesMarketData(t *testing.T) {
	p := pool(t)
	n := setup(t, p)
	member, _, _ := membership(t, p, n, n.cardholderID, newCardholder(t, p, "Tayo Ade"))
	v := newVenue(t, p, member)

	status, body := v.do(t, "GET", "/v1/instruments", nil, false)
	if status != http.StatusOK {
		t.Fatalf("list returned %d", status)
	}
	if len(body["instruments"].([]any)) == 0 {
		t.Fatal("no instruments listed")
	}

	status, inst := v.do(t, "GET", "/v1/instruments/"+n.symbol, nil, false)
	if status != http.StatusOK {
		t.Fatalf("instrument returned %d: %v", status, inst)
	}
	if inst["reference_kobo"].(float64) != float64(money.Naira(40)) {
		t.Errorf("reference = %v, want 4000", inst["reference_kobo"])
	}
	// The band is published, so a member can price an order that will be accepted.
	if inst["band_lo_kobo"].(float64) >= inst["band_hi_kobo"].(float64) {
		t.Error("band is inverted")
	}

	status, md := v.do(t, "GET", "/v1/instruments/"+n.symbol+"/market-data", nil, false)
	if status != http.StatusOK || len(md["observations"].([]any)) == 0 {
		t.Fatalf("market data returned %d: %v", status, md)
	}

	if status, _ := v.do(t, "GET", "/v1/instruments/NOSUCH", nil, false); status != http.StatusNotFound {
		t.Error("an unknown symbol did not 404")
	}
}

func TestAPIHealth(t *testing.T) {
	p := pool(t)
	n := setup(t, p)
	member, _, _ := membership(t, p, n, n.cardholderID, newCardholder(t, p, "Nkechi Udo"))
	v := newVenue(t, p, member)

	status, body := v.do(t, "GET", "/v1/health", nil, false)
	if status != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("health returned %d: %v", status, body)
	}
	if body["business_date"] != tradeDate {
		t.Errorf("business date = %v, want %s", body["business_date"], tradeDate)
	}
}

// The kill switch and the throttles, which are the controls that matter when a
// member's own system starts misbehaving.
func TestMemberControls(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)

	t.Run("admission is a decision, not a form", func(t *testing.T) {
		var id uuid.UUID
		mustTx(t, p, func(tx pgx.Tx) error {
			var err error
			id, err = exchange.Admit(ctx, tx, "NEWCO", "Newco Securities", nil)
			return err
		})
		// A firm starts pending. Vetting happens before trading, not after.
		v := newVenue(t, p, id)
		status, body := v.do(t, "POST", "/v1/orders", map[string]any{
			"symbol": n.symbol, "side": "buy", "type": "limit",
			"limit_kobo": int64(money.Naira(40)), "units": int64(share.Whole(1)),
			"client_account_id": uuid.New().String(),
		}, true)
		if status != http.StatusForbidden || body["code"] != "not_a_member" {
			t.Fatalf("a pending firm traded: %d %v", status, body)
		}

		mustTx(t, p, func(tx pgx.Tx) error { return exchange.Activate(ctx, tx, id, "listings") })
		if err := inTx(p, func(tx pgx.Tx) error {
			return exchange.Activate(ctx, tx, id, "listings")
		}); err == nil {
			t.Error("activating an already-active member must be an error")
		}
	})

	t.Run("halting a member requires a reason", func(t *testing.T) {
		member, _, _ := membership(t, p, n, n.cardholderID, newCardholder(t, p, "Sade Coker"))
		if err := inTx(p, func(tx pgx.Tx) error {
			return exchange.HaltMember(ctx, tx, member, "")
		}); err == nil {
			t.Fatal("a member was halted with no reason recorded")
		}
	})

	t.Run("unknown role is refused", func(t *testing.T) {
		if err := inTx(p, func(tx pgx.Tx) error {
			_, err := exchange.Admit(ctx, tx, "BADROLE", "Bad Role Ltd", []string{"kingmaker"})
			return err
		}); err == nil {
			t.Fatal("an unknown member role was accepted")
		}
	})

	// An order-to-trade ratio this lopsided is either a malfunction or layering,
	// and both call for the same response: stop taking the orders.
	t.Run("order-to-trade ratio throttles a member", func(t *testing.T) {
		holder := newCardholder(t, p, "Kunle Bello")
		fund(t, p, holder, money.Naira(500_000))
		member, ha, _ := membership(t, p, n, holder, newCardholder(t, p, "Remi Dada"))
		v := newVenue(t, p, member)

		place := func() (int, map[string]any) {
			return v.do(t, "POST", "/v1/orders", map[string]any{
				"symbol": n.symbol, "side": "buy", "type": "limit",
				"limit_kobo": int64(money.Naira(34)), "units": int64(share.Whole(3)),
				"client_account_id": ha.client.String(),
			}, true)
		}

		var throttled bool
		for i := 0; i < 40; i++ {
			status, body := place()
			if status == http.StatusTooManyRequests {
				throttled = true
				if body["code"] != "throttled" {
					t.Errorf("code = %v, want throttled", body["code"])
				}
				break
			}
			if status != http.StatusCreated {
				t.Fatalf("order %d returned %d: %v", i, status, body)
			}
		}
		if !throttled {
			t.Fatal("a member sending 40 unfilled orders was never throttled")
		}
	})
}

// A closed period restricts the people who know first, not the market.
func TestClosedPeriodBlocksInsidersOnly(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)

	insider := n.cardholderID
	outsider := newCardholder(t, p, "Halima Sule")
	giveShares(t, p, n, insider, share.Whole(20), money.Naira(30))
	giveShares(t, p, n, outsider, share.Whole(20), money.Naira(30))
	member, ia, oa := membership(t, p, n, insider, outsider)

	mustExec(t, p, `
		INSERT INTO related_parties (instrument_id, account_id, group_key, relation, effective)
		VALUES ($1,$2,'issuer-group','director',daterange('2020-01-01','2099-01-01'))`,
		n.instrumentID, ia.ledger)
	mustTx(t, p, func(tx pgx.Tx) error {
		return exchange.DeclareClosedPeriod(ctx, tx, n.instrumentID, "results",
			"2027-05-25", "2027-06-10", "company.secretary")
	})

	v := newVenue(t, p, member)
	sell := func(a acct) (int, map[string]any) {
		return v.do(t, "POST", "/v1/orders", map[string]any{
			"symbol": n.symbol, "side": "sell", "type": "limit",
			"limit_kobo": int64(money.Naira(38)), "units": int64(share.Whole(5)),
			"client_account_id": a.client.String(),
		}, true)
	}

	status, body := sell(ia)
	// The insider is a related party, so they are refused either way — but the
	// closed period is what would stop them once the symbol graduates to
	// continuous trading and loses the blanket ban.
	if status != http.StatusForbidden {
		t.Fatalf("an insider traded during a closed period: %d %v", status, body)
	}

	if status, body := sell(oa); status != http.StatusCreated {
		t.Fatalf("an ordinary holder was blocked by a closed period: %d %v", status, body)
	}
}

func TestDeclareClosedPeriodValidatesReason(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)
	if err := inTx(p, func(tx pgx.Tx) error {
		return exchange.DeclareClosedPeriod(ctx, tx, n.instrumentID, "vibes",
			"2027-05-25", "2027-06-10", "someone")
	}); err == nil {
		t.Fatal("an unrecognised closed-period reason was accepted")
	}
}
