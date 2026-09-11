package e2e

import (
	"context"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"freedom/api/internal/cashrail"
	"freedom/api/internal/ledger"
	"freedom/api/internal/money"
)

// A duplicate deposit is free money, and providers retry. This is the single
// most important property in the package.
func TestDepositCreditsOnceHoweverManyTimesItArrives(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)
	holder := n.cardholderID
	before := nairaOf(t, p, holder, ledger.KindAvailable)

	var first uuid.UUID
	for i := 0; i < 4; i++ {
		mustTx(t, p, func(tx pgx.Tx) error {
			id, err := cashrail.Deposit(ctx, tx, holder, money.Naira(25_000), "NIP/2027/998877", tradeDate)
			if i == 0 {
				first = id
			} else if id != first {
				t.Fatalf("retry %d produced a different instruction", i)
			}
			return err
		})
	}

	if got := nairaOf(t, p, holder, ledger.KindAvailable) - before; got != money.Naira(25_000) {
		t.Fatalf("four deliveries of one deposit credited %s, want ₦25,000.00", got)
	}
}

// The money leaves the balance immediately. Debiting on confirmation instead
// would let the same balance be withdrawn twice while the first payment was
// still in flight.
func TestWithdrawalDebitsImmediatelyAndSettles(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)
	holder := n.cardholderID
	fund(t, p, holder, money.Naira(30_000))
	before := nairaOf(t, p, holder, ledger.KindAvailable)

	var id uuid.UUID
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		id, err = cashrail.RequestWithdrawal(ctx, tx, holder, money.Naira(10_000), "wd-1", tradeDate)
		return err
	})
	if got := before - nairaOf(t, p, holder, ledger.KindAvailable); got != money.Naira(10_000) {
		t.Fatalf("balance fell by %s on request, want ₦10,000.00", got)
	}

	mustTx(t, p, func(tx pgx.Tx) error {
		return cashrail.Confirm(ctx, tx, id, cashrail.StateSettled, "NIP/OUT/1", tradeDate)
	})
	// Confirming twice is normal: providers deliver twice.
	mustTx(t, p, func(tx pgx.Tx) error {
		return cashrail.Confirm(ctx, tx, id, cashrail.StateSettled, "NIP/OUT/1", tradeDate)
	})
	if got := before - nairaOf(t, p, holder, ledger.KindAvailable); got != money.Naira(10_000) {
		t.Fatalf("balance moved again on a repeated confirmation: %s", got)
	}

	// And the payable is cleared, so nothing is stranded in suspense.
	susp, err := ledger.NairaBalance(ctx, p, ledger.Scheme(ledger.KindSuspense, ledger.AssetNGN))
	if err != nil {
		t.Fatal(err)
	}
	if susp != 0 {
		t.Errorf("suspense holds %s after settlement", susp)
	}
}

// A payment can be accepted and bounce days later. The cardholder must not be
// left short in the meantime, and must get it back when it returns.
func TestReturnedWithdrawalGoesBackToTheCardholder(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)
	holder := n.cardholderID
	fund(t, p, holder, money.Naira(30_000))
	before := nairaOf(t, p, holder, ledger.KindAvailable)

	var id uuid.UUID
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		id, err = cashrail.RequestWithdrawal(ctx, tx, holder, money.Naira(8_000), "wd-2", tradeDate)
		return err
	})
	mustTx(t, p, func(tx pgx.Tx) error {
		return cashrail.Confirm(ctx, tx, id, cashrail.StateReturned, "NIP/RET/1", tradeDate)
	})

	if got := nairaOf(t, p, holder, ledger.KindAvailable); got != before {
		t.Fatalf("after a return the balance is %s, want the original %s", got, before)
	}
	susp, _ := ledger.NairaBalance(ctx, p, ledger.Scheme(ledger.KindSuspense, ledger.AssetNGN))
	if susp != 0 {
		t.Errorf("suspense holds %s after a return", susp)
	}
}

func TestWithdrawalRefusedWithoutFunds(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)
	if err := inTx(p, func(tx pgx.Tx) error {
		_, err := cashrail.RequestWithdrawal(ctx, tx, n.cardholderID, money.Naira(1_000_000), "wd-3", tradeDate)
		return err
	}); err == nil {
		t.Fatal("a withdrawal larger than the balance was queued")
	}
}

// A confirmation about something we never sent is either a provider bug or an
// attack, and must never be a reason to move money.
func TestConfirmationForUnknownInstructionIsRefused(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	setup(t, p)
	err := inTx(p, func(tx pgx.Tx) error {
		return cashrail.Confirm(ctx, tx, uuid.New(), cashrail.StateSettled, "ghost", tradeDate)
	})
	if err == nil {
		t.Fatal("a confirmation for an unknown instruction was accepted")
	}
}

// The signature covers the RAW bytes. Re-marshalling first can never match, and
// the failure looks like a wrong secret rather than a wrong input.
func TestWebhookSignature(t *testing.T) {
	const secret = "shhh"
	body := []byte(`{"reference":"NIP/1","status":"SUCCESS","amount":1000}`)
	mac := hmac.New(sha512.New, []byte(secret))
	mac.Write(body)
	sig := hex.EncodeToString(mac.Sum(nil))

	if err := cashrail.VerifyWebhook(secret, sig, body); err != nil {
		t.Fatalf("a valid signature was rejected: %v", err)
	}
	if err := cashrail.VerifyWebhook(secret, sig, append(body, ' ')); err == nil {
		t.Error("a single trailing space did not change the signature")
	}
	if err := cashrail.VerifyWebhook(secret, "deadbeef", body); err == nil {
		t.Error("a wrong signature was accepted")
	}
	// Refusing to start without a secret beats accepting everything.
	if err := cashrail.VerifyWebhook("", sig, body); err == nil {
		t.Error("callbacks were accepted with no secret configured")
	}
}

// Providers rename fields between versions. The parser accepts the shapes seen
// in the wild and refuses the ones it does not recognise rather than guessing.
func TestEventParsingAndOutcomes(t *testing.T) {
	for _, tc := range []struct {
		body string
		want string
	}{
		{`{"reference":"a","status":"SUCCESS"}`, cashrail.StateSettled},
		{`{"transactionReference":"b","transactionStatus":"completed"}`, cashrail.StateSettled},
		{`{"ref":"c","state":"reversed"}`, cashrail.StateReturned},
		{`{"reference":"d","status":"declined"}`, cashrail.StateFailed},
	} {
		e, err := cashrail.ParseEvent([]byte(tc.body))
		if err != nil {
			t.Fatalf("%s: %v", tc.body, err)
		}
		got, err := e.Outcome()
		if err != nil || got != tc.want {
			t.Errorf("%s → %q (%v), want %q", tc.body, got, err, tc.want)
		}
	}

	if _, err := cashrail.ParseEvent([]byte(`{"status":"SUCCESS"}`)); err == nil {
		t.Error("a callback with no reference was accepted")
	}
	if _, err := cashrail.ParseEvent([]byte(`not json`)); err == nil {
		t.Error("malformed JSON was accepted")
	}

	// A status nobody recognises must not become a money movement on the
	// assumption it probably meant success.
	e, _ := cashrail.ParseEvent([]byte(`{"reference":"x","status":"probably_fine"}`))
	if _, err := e.Outcome(); err == nil {
		t.Error("an unrecognised provider status was turned into an outcome")
	}
}
