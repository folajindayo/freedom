package e2e

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"freedom/api/internal/exchange"
	"freedom/api/internal/money"
	"freedom/api/internal/share"
)

// The certification harness, run against the real HTTP surface.
//
// This is what a candidate member does before going live. Each case is a
// refusal a live member WILL meet, with a distinct code they have to handle
// differently — a firm that treats every non-200 the same passes none of them.
func TestCertificationSuite(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)

	holder := n.cardholderID
	giveShares(t, p, n, holder, share.Whole(30), money.Naira(30))
	fund(t, p, holder, money.Naira(10_000))
	member, ha, _ := membership(t, p, n, holder, newCardholder(t, p, "Ngozi Bright"))
	v := newVenue(t, p, member)

	// Candidates certify against a sandbox symbol: a deliberate failure must
	// not land in real surveillance, and a successful order must not land in a
	// real price.
	var sandbox string
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		sandbox, err = exchange.SandboxInstrument(ctx, tx, "CERTX")
		return err
	})
	if sandbox == "" {
		t.Fatal("no sandbox instrument was provisioned")
	}

	mustExec(t, p, `UPDATE instruments SET max_order_units = $2 WHERE id = $1`,
		n.instrumentID, int64(share.Whole(100)))

	order := func(body map[string]any, auth bool) (int, map[string]any) {
		base := map[string]any{
			"symbol": n.symbol, "side": "sell", "type": "limit",
			"limit_kobo": int64(money.Naira(38)), "units": int64(share.Whole(5)),
			"client_account_id": ha.client.String(),
		}
		for k, val := range body {
			base[k] = val
		}
		return v.do(t, "POST", "/v1/orders", base, auth)
	}

	run := exchange.CertificationRun{MemberCode: "FSEC"}
	record := func(name, got, detail string) {
		for _, c := range exchange.CertificationSuite() {
			if c.Name == name {
				run.Results = append(run.Results, exchange.CertResult{
					Case: c, Got: got, Passed: got == c.Expect, Detail: detail,
				})
				return
			}
		}
		t.Fatalf("no certification case named %q", name)
	}
	code := func(status int, body map[string]any) string {
		if status < 300 {
			return "accepted"
		}
		s, _ := body["code"].(string)
		return s
	}

	// --- the happy path, and the retry ---------------------------------
	status, body := order(map[string]any{"client_order_id": "cert-1"}, true)
	record("place a valid limit order", code(status, body), "")
	liveOrder, _ := body["order_id"].(string)

	_, retry := order(map[string]any{"client_order_id": "cert-1"}, true)
	got := "accepted"
	if retry["order_id"] != liveOrder {
		got = "duplicated"
	}
	record("resend the same client order id", got, "")

	// --- position and funding ------------------------------------------
	// Above the 30 shares held, but below the 100-share fat-finger ceiling, so
	// this exercises the position check rather than the size check.
	status, body = order(map[string]any{"units": int64(share.Whole(50))}, true)
	record("sell more than the account holds", code(status, body), "")

	locked := newCardholder(t, p, "Chinedu Eze")
	la := clientFor(t, p, n, member, locked)
	giveShares(t, p, n, locked, share.Whole(10), money.Naira(30))
	mustExec(t, p, `
		UPDATE holding_lots SET transferable_from = '2099-01-01'
		 WHERE account_id = (SELECT id FROM accounts WHERE owner_id = $1 AND kind='stock_wallet')`, locked)
	status, body = order(map[string]any{"client_account_id": la.client.String()}, true)
	record("sell shares still inside their chargeback lock", code(status, body), "")

	poor := newCardholder(t, p, "Bunmi Alade")
	pa := clientFor(t, p, n, member, poor)
	status, body = order(map[string]any{
		"client_account_id": pa.client.String(), "side": "buy",
		"limit_kobo": int64(money.Naira(40)), "units": int64(share.Whole(50)),
	}, true)
	record("buy with insufficient cash", code(status, body), "")

	// --- size and price -------------------------------------------------
	status, body = order(map[string]any{"units": int64(share.PerShare / 100)}, true)
	record("order below the minimum notional", code(status, body), "")

	status, body = order(map[string]any{"units": int64(share.Whole(500))}, true)
	record("order far above the maximum size", code(status, body), "")

	mustExec(t, p, `UPDATE instruments SET tick_kobo = 25 WHERE id = $1`, n.instrumentID)
	status, body = order(map[string]any{"limit_kobo": int64(money.Naira(38)) + 7}, true)
	record("limit price off the tick", code(status, body), "")
	mustExec(t, p, `UPDATE instruments SET tick_kobo = 1 WHERE id = $1`, n.instrumentID)

	status, body = order(map[string]any{"limit_kobo": int64(money.Naira(500))}, true)
	record("limit price outside the band", code(status, body), "")

	// --- venue state -----------------------------------------------------
	mustTx(t, p, func(tx pgx.Tx) error {
		_, err := exchange.Halt(ctx, tx, n.instrumentID, exchange.HaltNewsPending, "cert", nil)
		return err
	})
	status, body = order(nil, true)
	record("trade a halted instrument", code(status, body), "")
	mustTx(t, p, func(tx pgx.Tx) error { return exchange.Release(ctx, tx, n.instrumentID, "cert") })

	// A Saturday: the calendar is published ahead and must not be guessed.
	closedVenue := newVenueAt(t, p, member, "2027-06-05")
	status, body = closedVenue.do(t, "POST", "/v1/orders", map[string]any{
		"symbol": n.symbol, "side": "sell", "type": "limit",
		"limit_kobo": int64(money.Naira(38)), "units": int64(share.Whole(5)),
		"client_account_id": ha.client.String(),
	}, true)
	record("trade on a market holiday", code(status, body), "")

	mustTx(t, p, func(tx pgx.Tx) error {
		return exchange.HaltMember(ctx, tx, member, "certification")
	})
	status, body = order(nil, true)
	record("trade while the firm is halted", code(status, body), "")
	mustTx(t, p, func(tx pgx.Tx) error { return exchange.ResumeMember(ctx, tx, member) })

	// --- throttles --------------------------------------------------------
	throttled := "not_throttled"
	for i := 0; i < 60; i++ {
		status, body := order(map[string]any{
			"side": "buy", "limit_kobo": int64(money.Naira(34)),
			"units": int64(share.Whole(3)), "client_account_id": ha.client.String(),
		}, true)
		if status == http.StatusTooManyRequests {
			throttled, _ = body["code"].(string)
			break
		}
	}
	record("exceed the order-to-trade ratio", throttled, "")
	mustExec(t, p, `DELETE FROM member_activity WHERE member_id = $1`, member)

	// --- cancels ----------------------------------------------------------
	status, _ = v.do(t, "DELETE", "/v1/orders/"+liveOrder, nil, true)
	record("cancel a live order", map[bool]string{true: "accepted", false: "failed"}[status < 300], "")

	status, body = v.do(t, "DELETE", "/v1/orders/"+liveOrder, nil, true)
	record("cancel the same order twice", code(status, body), "")

	var rival uuid.UUID
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		rival, err = exchange.Admit(ctx, tx, "CERTRIVAL", "Rival Ltd", nil)
		if err != nil {
			return err
		}
		return exchange.Activate(ctx, tx, rival, "cert")
	})
	status, body = newVenue(t, p, rival).do(t, "DELETE", "/v1/orders/"+liveOrder, nil, true)
	record("cancel another member's order", code(status, body), "")

	// --- credentials -------------------------------------------------------
	status, body = order(nil, false)
	record("send with a bad credential", code(status, body), "")

	t.Log("\n" + run.Report())
	if !run.Passed() {
		t.Fatal("the venue did not behave as its own certification suite requires")
	}
	if len(run.Results) != len(exchange.CertificationSuite()) {
		t.Fatalf("ran %d of %d cases", len(run.Results), len(exchange.CertificationSuite()))
	}
}

// A failing run names what each failure teaches, so a candidate fixes its
// handling rather than its retry loop.
func TestCertificationReportExplainsFailures(t *testing.T) {
	run := exchange.CertificationRun{
		MemberCode: "BADCO",
		Results: []exchange.CertResult{{
			Case: exchange.CertCase{
				Name: "sell more than the account holds", Expect: "insufficient_shares",
				Teaches: "position checks are the venue's, not yours",
			},
			Got: "accepted", Passed: false,
		}},
	}
	if run.Passed() {
		t.Fatal("a run with a failure reported as passed")
	}
	report := run.Report()
	if !strings.Contains(report, "not certified") {
		t.Error("the report does not say the candidate failed")
	}
	if !strings.Contains(report, "position checks are the venue's") {
		t.Error("the report does not explain what the failure teaches")
	}
	if (exchange.CertificationRun{}).Passed() {
		t.Error("an empty run must not count as certified")
	}
}
