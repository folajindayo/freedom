package e2e

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"freedom/api/internal/issuer"
	"freedom/api/internal/ledger"
	"freedom/api/internal/money"
	"freedom/api/internal/scheme"
	"freedom/api/internal/tapcrypto"
)

// rawTap presents specific credential fields, bypassing the fixture's
// bookkeeping, so a test can lie about what the card said.
func (n *network) rawTap(ctx context.Context, tx pgx.Tx, stan string,
	token []byte, counter uint32, amount money.Kobo) (issuer.Response, error) {
	return n.auth.Authorize(ctx, tx, issuer.Request{
		TerminalID: n.terminalID,
		STAN:       stan,
		Amount:     amount,
		Tech:       tapcrypto.TechNTAG215,
		Tap: url.Values{
			"uid":   {hex.EncodeToString(n.tagUID)},
			"token": {hex.EncodeToString(token)},
			"ctr":   {hex.EncodeToString([]byte{byte(counter), byte(counter >> 8), byte(counter >> 16)})},
		},
		At: time.Date(2026, 9, 11, 12, 0, 0, 0, scheme.Lagos),
	})
}

// Two terminals tapping the same card at the same instant must approve exactly
// once. This is a database-level race and cannot be tested with a unit test:
// the guarantee comes from the row lock the authoriser takes on the credential.
func TestConcurrentTapsApproveExactlyOnce(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)

	const attempts = 8
	var wg sync.WaitGroup
	results := make([]issuer.Response, attempts)
	errs := make([]error, attempts)

	start := make(chan struct{})
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release them together
			tx, err := p.Begin(ctx)
			if err != nil {
				errs[i] = err
				return
			}
			defer tx.Rollback(ctx)

			// Every goroutine presents the SAME token at the SAME counter —
			// exactly what a cloned tag and its original would do if tapped
			// simultaneously. Only one may be approved: the losers find the
			// token rotated, and the previous-token fallback refuses them
			// because their counter has not advanced past the rotation.
			resp, err := n.rawTap(ctx, tx, fmt.Sprintf("%06d", 900+i), n.token, 1, money.Naira(1_000))
			if err != nil {
				errs[i] = err
				return
			}
			if err := tx.Commit(ctx); err != nil {
				errs[i] = err
				return
			}
			results[i] = resp
		}(i)
	}
	close(start)
	wg.Wait()

	approved := 0
	for i, r := range results {
		if errs[i] != nil {
			t.Fatalf("attempt %d errored: %v", i, errs[i])
		}
		if r.Approved {
			approved++
		}
	}
	if approved != 1 {
		t.Fatalf("%d of %d concurrent taps on one token were approved, want exactly 1", approved, attempts)
	}

	// Exactly one hold exists, so the cardholder was debited once.
	if got := n.balance(t, p, ledger.KindHold); got != money.Naira(1_000) {
		t.Fatalf("held %s after %d concurrent taps, want ₦1,000.00", got, attempts)
	}
}

// A token the card once held but has since burnt can only be in an attacker's
// hands. Presenting it must decline and must freeze the card, not merely
// decline and let the next attempt through.
func TestClonedTokenFreezesTheCard(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)

	stolen := make([]byte, len(n.token))
	copy(stolen, n.token)

	// The genuine card taps, which burns the stolen token's successor slot and
	// rotates twice so the stolen one falls outside the two-token window.
	for i := 0; i < 2; i++ {
		var resp issuer.Response
		mustTx(t, p, func(tx pgx.Tx) error {
			var err error
			resp, err = n.tap(ctx, tx, money.Naira(500))
			return err
		})
		if !resp.Approved {
			t.Fatalf("genuine tap %d declined: %s", i, resp.DeclineCode)
		}
	}

	// Now the clone taps with the token it lifted.
	var resp issuer.Response
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		resp, err = n.rawTap(ctx, tx, "009999", stolen, 9, money.Naira(500))
		return err
	})

	if resp.Approved {
		t.Fatal("a burnt token was accepted")
	}
	if resp.DeclineCode != scheme.DeclineRestrictedCard {
		t.Errorf("decline code = %s, want %s", resp.DeclineCode, scheme.DeclineRestrictedCard)
	}

	// The card and the credential are both frozen, and the reason is recorded.
	var cardStatus, credStatus, reason string
	if err := p.QueryRow(ctx, `
		SELECT c.status, cc.status, COALESCE(c.frozen_reason,'')
		  FROM cards c JOIN card_credentials cc ON cc.card_id = c.id
		 WHERE c.id = $1`, n.cardID).Scan(&cardStatus, &credStatus, &reason); err != nil {
		t.Fatal(err)
	}
	if cardStatus != "frozen" || credStatus != "frozen" {
		t.Fatalf("card %q credential %q after a clone, want both frozen", cardStatus, credStatus)
	}
	if reason == "" {
		t.Error("the freeze must record why")
	}

	// And the genuine card is now stopped too — which is correct. Two tags
	// carry one identity and the issuer cannot tell which is which.
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		resp, err = n.tap(ctx, tx, money.Naira(500))
		return err
	})
	if resp.Approved {
		t.Fatal("a frozen card kept working")
	}
}

// The customer lifts the card before the PoS writes the new token back. The
// next genuine tap must work: freezing a real cardholder mid-queue over a
// failed write is the failure mode that would kill the pilot.
func TestFailedWriteBackDoesNotLockOutCardholder(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)

	held := make([]byte, len(n.token))
	copy(held, n.token) // what the tag still holds

	var resp issuer.Response
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		resp, err = n.rawTap(ctx, tx, "000001", held, 1, money.Naira(1_000))
		return err
	})
	if !resp.Approved {
		t.Fatalf("first tap declined: %s", resp.DeclineCode)
	}
	// The write-back never lands, so the tag still holds the old token.

	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		resp, err = n.rawTap(ctx, tx, "000002", held, 2, money.Naira(1_000))
		return err
	})
	if !resp.Approved {
		t.Fatalf("the two-token window did not cover a failed write-back: %s", resp.DeclineCode)
	}

	// It covers exactly one retry. A third presentation of the same token is
	// outside the window and is treated as a clone.
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		resp, err = n.rawTap(ctx, tx, "000003", held, 3, money.Naira(1_000))
		return err
	})
	if resp.Approved {
		t.Fatal("the two-token window must not extend indefinitely")
	}
}

// The pilot rails are the fraud-loss budget for a credential that cannot
// authenticate. They are asserted, not assumed.
func TestPilotRails(t *testing.T) {
	p := pool(t)
	ctx := context.Background()

	t.Run("per-transaction cap", func(t *testing.T) {
		n := setup(t, p)
		mustExec(t, p, `UPDATE cards SET per_txn_cap_kobo = $2 WHERE id = $1`,
			n.cardID, int64(money.Naira(2_000)))

		var resp issuer.Response
		mustTx(t, p, func(tx pgx.Tx) error {
			var err error
			resp, err = n.tap(ctx, tx, money.Naira(2_001))
			return err
		})
		if resp.Approved || resp.DeclineCode != scheme.DeclineExceedsLimit {
			t.Fatalf("over-cap tap: approved=%v code=%s", resp.Approved, resp.DeclineCode)
		}
	})

	t.Run("daily cap across several taps", func(t *testing.T) {
		n := setup(t, p)
		mustExec(t, p, `UPDATE cards SET daily_cap_kobo = $2, per_txn_cap_kobo = $3 WHERE id = $1`,
			n.cardID, int64(money.Naira(3_000)), int64(money.Naira(3_000)))

		for i := 0; i < 3; i++ {
			var resp issuer.Response
			mustTx(t, p, func(tx pgx.Tx) error {
				var err error
				resp, err = n.tap(ctx, tx, money.Naira(1_000))
				return err
			})
			if !resp.Approved {
				t.Fatalf("tap %d of 3 within the daily cap declined: %s", i+1, resp.DeclineCode)
			}
		}
		var resp issuer.Response
		mustTx(t, p, func(tx pgx.Tx) error {
			var err error
			resp, err = n.tap(ctx, tx, money.Naira(1))
			return err
		})
		if resp.Approved || resp.DeclineCode != scheme.DeclineVelocityExceeded {
			t.Fatalf("the fourth tap breached the daily cap: approved=%v code=%s",
				resp.Approved, resp.DeclineCode)
		}
	})

	t.Run("insufficient funds", func(t *testing.T) {
		n := setup(t, p)
		// Raise the per-transaction cap above the balance, so this test
		// exercises the funds check rather than the cap check.
		mustExec(t, p, `UPDATE cards SET per_txn_cap_kobo = $2 WHERE id = $1`,
			n.cardID, int64(money.Naira(100_000)))
		var resp issuer.Response
		mustTx(t, p, func(tx pgx.Tx) error {
			var err error
			resp, err = n.tap(ctx, tx, money.Naira(60_000)) // funded with ₦50,000
			return err
		})
		if resp.Approved || resp.DeclineCode != scheme.DeclineInsufficientFunds {
			t.Fatalf("approved=%v code=%s", resp.Approved, resp.DeclineCode)
		}
	})

	t.Run("a frozen card is refused", func(t *testing.T) {
		n := setup(t, p)
		mustExec(t, p, `UPDATE cards SET status = 'frozen' WHERE id = $1`, n.cardID)
		var resp issuer.Response
		mustTx(t, p, func(tx pgx.Tx) error {
			var err error
			resp, err = n.tap(ctx, tx, money.Naira(100))
			return err
		})
		if resp.Approved || resp.DeclineCode != scheme.DeclineRestrictedCard {
			t.Fatalf("approved=%v code=%s", resp.Approved, resp.DeclineCode)
		}
	})
}

// A terminal retrying a timed-out tap must not charge the customer twice.
func TestTerminalRetryIsIdempotent(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	n := setup(t, p)

	tok := make([]byte, len(n.token))
	copy(tok, n.token)

	var first, retry issuer.Response
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		first, err = n.rawTap(ctx, tx, "000042", tok, 1, money.Naira(2_500))
		return err
	})
	if !first.Approved {
		t.Fatalf("declined: %s", first.DeclineCode)
	}

	// The same STAN, resent because the terminal never saw the response. The
	// tag now holds the rotated token, but the switch must recognise the retry
	// before any of that matters.
	mustTx(t, p, func(tx pgx.Tx) error {
		var err error
		retry, err = n.rawTap(ctx, tx, "000042", first.WriteBack.Token, 2, money.Naira(2_500))
		return err
	})
	if !retry.Approved {
		t.Fatalf("a retried STAN was declined: %s", retry.DeclineCode)
	}
	if retry.AuthID != first.AuthID {
		t.Errorf("retry produced a new authorisation %s, want the original %s", retry.AuthID, first.AuthID)
	}
	if got := n.balance(t, p, ledger.KindHold); got != money.Naira(2_500) {
		t.Fatalf("held %s after a retry, want ₦2,500.00 — the customer was charged twice", got)
	}
}

var _ = pgxpool.Pool{}
