// Package cashrail moves naira across the boundary of the system.
//
// Everything else in Freedom settles atomically, because Freedom controls both
// legs. This does not: once an instruction leaves for NIBSS, the outcome is
// somebody else's to report, and it may arrive late, twice, or not at all.
//
// So this package is built around the three things that are actually true of an
// external rail: an instruction may be sent more than once and must only move
// money once; a confirmation may arrive twice and must only be believed once;
// and a payment may fail after being accepted, which is a return rather than an
// error.
package cashrail

import (
	"context"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"freedom/api/internal/ledger"
	"freedom/api/internal/money"
)

// Instruction states. A payment that has been sent is not a payment that has
// settled, and the gap between them is where the risk lives.
const (
	StatePending  = "pending"
	StateSent     = "sent"
	StateSettled  = "settled"
	StateFailed   = "failed"
	StateReturned = "returned"
)

// ErrUnknownInstruction is returned for a confirmation about something we never
// sent — which is either a provider bug or an attack, and must never be treated
// as a reason to credit an account.
var ErrUnknownInstruction = errors.New("cashrail: no such instruction")

// Deposit credits a cardholder for money that arrived from a bank.
//
// Keyed on the provider's own reference so a webhook delivered twice credits
// once. This is the single most important property in the package: a duplicate
// deposit is free money, and providers retry.
func Deposit(ctx context.Context, tx pgx.Tx, holder uuid.UUID, amount money.Kobo,
	externalRef, businessDate string) (uuid.UUID, error) {

	if amount <= 0 {
		return uuid.Nil, fmt.Errorf("cashrail: deposit must be positive, got %s", amount)
	}
	if externalRef == "" {
		return uuid.Nil, fmt.Errorf("cashrail: a deposit needs the provider's reference")
	}

	account, err := ledger.Resolve(ctx, tx, ledger.Cardholder(holder, ledger.KindAvailable, ledger.AssetNGN))
	if err != nil {
		return uuid.Nil, err
	}
	external, err := ledger.Resolve(ctx, tx, ledger.Scheme(ledger.KindExternal, ledger.AssetNGN))
	if err != nil {
		return uuid.Nil, err
	}

	key := "deposit|" + externalRef
	var id uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO settlement_instructions
		  (direction, account_id, amount_kobo, rail, external_ref, state, idempotency_key)
		VALUES ('in',$1,$2,'nibss_nip',$3,'settled',$4)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING id`, account, int64(amount), externalRef, key).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		// Already credited. The provider retried; that is normal, not an error.
		return existingInstruction(ctx, tx, key)
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("cashrail: record deposit: %w", err)
	}

	txID, err := ledger.Post(ctx, tx, ledger.Tx{
		EventType:      "cashrail.deposit",
		BusinessDate:   businessDate,
		IdempotencyKey: key,
		CorrelationID:  &id,
		Entries: []ledger.Entry{
			{AccountID: external, Amount: ledger.NGN(-amount), Reason: "deposit.received"},
			{AccountID: account, Amount: ledger.NGN(amount), Reason: "deposit.credit"},
		},
	})
	if err != nil && !errors.Is(err, ledger.ErrAlreadyPosted) {
		return uuid.Nil, err
	}
	_, err = tx.Exec(ctx,
		`UPDATE settlement_instructions SET ledger_tx_id = $2 WHERE id = $1`, id, txID)
	return id, err
}

// RequestWithdrawal debits a cardholder and queues an instruction.
//
// The money leaves the cardholder's balance immediately and sits in a payable
// account until the rail confirms. Debiting on confirmation instead would let
// the same balance be withdrawn twice while the first payment was in flight.
func RequestWithdrawal(ctx context.Context, tx pgx.Tx, holder uuid.UUID, amount money.Kobo,
	clientRef, businessDate string) (uuid.UUID, error) {

	if amount <= 0 {
		return uuid.Nil, fmt.Errorf("cashrail: withdrawal must be positive, got %s", amount)
	}
	available, err := ledger.NairaBalance(ctx, tx, ledger.Cardholder(holder, ledger.KindAvailable, ledger.AssetNGN))
	if err != nil {
		return uuid.Nil, err
	}
	if available < amount {
		return uuid.Nil, fmt.Errorf("cashrail: %s available, %s requested", available, amount)
	}

	account, err := ledger.Resolve(ctx, tx, ledger.Cardholder(holder, ledger.KindAvailable, ledger.AssetNGN))
	if err != nil {
		return uuid.Nil, err
	}
	payable, err := ledger.Resolve(ctx, tx, ledger.Scheme(ledger.KindSuspense, ledger.AssetNGN))
	if err != nil {
		return uuid.Nil, err
	}

	key := "withdrawal|" + clientRef
	var id uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO settlement_instructions
		  (direction, account_id, amount_kobo, rail, state, idempotency_key)
		VALUES ('out',$1,$2,'nibss_nip','pending',$3)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING id`, account, int64(amount), key).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return existingInstruction(ctx, tx, key)
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("cashrail: record withdrawal: %w", err)
	}

	txID, err := ledger.Post(ctx, tx, ledger.Tx{
		EventType:      "cashrail.withdrawal_requested",
		BusinessDate:   businessDate,
		IdempotencyKey: key,
		CorrelationID:  &id,
		Entries: []ledger.Entry{
			{AccountID: account, Amount: ledger.NGN(-amount), Reason: "withdrawal.debit"},
			{AccountID: payable, Amount: ledger.NGN(amount), Reason: "withdrawal.payable"},
		},
	})
	if err != nil && !errors.Is(err, ledger.ErrAlreadyPosted) {
		return uuid.Nil, err
	}
	_, err = tx.Exec(ctx,
		`UPDATE settlement_instructions SET ledger_tx_id = $2 WHERE id = $1`, id, txID)
	return id, err
}

// Confirm records the rail's verdict on an outbound payment.
//
// Settled clears the payable. Returned puts the money back in the cardholder's
// hands, which is the outcome people forget: a payment can be accepted and then
// bounce days later, and the cardholder must not be left short in the meantime.
func Confirm(ctx context.Context, tx pgx.Tx, instructionID uuid.UUID, outcome, externalRef, businessDate string) error {
	switch outcome {
	case StateSettled, StateFailed, StateReturned:
	default:
		return fmt.Errorf("cashrail: %q is not a settlement outcome", outcome)
	}

	var state, direction string
	var amount money.Kobo
	var account uuid.UUID
	err := tx.QueryRow(ctx, `
		SELECT state, direction, amount_kobo, account_id FROM settlement_instructions
		 WHERE id = $1 FOR UPDATE`, instructionID).Scan(&state, &direction, &amount, &account)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrUnknownInstruction
	}
	if err != nil {
		return fmt.Errorf("cashrail: load instruction: %w", err)
	}
	if state == StateSettled || state == StateReturned {
		return nil // already confirmed; providers deliver twice
	}
	if direction != "out" {
		return fmt.Errorf("cashrail: instruction %s is inbound and needs no confirmation", instructionID)
	}

	payable, err := ledger.Resolve(ctx, tx, ledger.Scheme(ledger.KindSuspense, ledger.AssetNGN))
	if err != nil {
		return err
	}
	external, err := ledger.Resolve(ctx, tx, ledger.Scheme(ledger.KindExternal, ledger.AssetNGN))
	if err != nil {
		return err
	}

	var entries []ledger.Entry
	switch outcome {
	case StateSettled:
		entries = []ledger.Entry{
			{AccountID: payable, Amount: ledger.NGN(-amount), Reason: "withdrawal.settled"},
			{AccountID: external, Amount: ledger.NGN(amount), Reason: "withdrawal.paid"},
		}
	case StateFailed, StateReturned:
		entries = []ledger.Entry{
			{AccountID: payable, Amount: ledger.NGN(-amount), Reason: "withdrawal.returned"},
			{AccountID: account, Amount: ledger.NGN(amount), Reason: "withdrawal.refunded"},
		}
	}

	if _, err := ledger.Post(ctx, tx, ledger.Tx{
		EventType:      "cashrail.withdrawal_" + outcome,
		BusinessDate:   businessDate,
		IdempotencyKey: "confirm|" + instructionID.String(),
		CorrelationID:  &instructionID,
		Entries:        entries,
	}); err != nil && !errors.Is(err, ledger.ErrAlreadyPosted) {
		return err
	}

	_, err = tx.Exec(ctx, `
		UPDATE settlement_instructions SET state = $2, external_ref = COALESCE($3, external_ref)
		 WHERE id = $1`, instructionID, outcome, nullable(externalRef))
	return err
}

func existingInstruction(ctx context.Context, tx pgx.Tx, key string) (uuid.UUID, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx,
		`SELECT id FROM settlement_instructions WHERE idempotency_key = $1`, key).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("cashrail: locate instruction %q: %w", key, err)
	}
	return id, nil
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// VerifyWebhook authenticates a provider callback.
//
// The signature is computed over the RAW request bytes. Re-marshalling the JSON
// first and signing that can never match: key order, whitespace and numeric
// formatting all differ, and the failure looks like a wrong secret rather than
// a wrong input — which is a whole afternoon of debugging the wrong thing.
func VerifyWebhook(secret, signature string, body []byte) error {
	if secret == "" {
		return errors.New("cashrail: no webhook secret configured; refusing to accept callbacks")
	}
	mac := hmac.New(sha512.New, []byte(secret))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(strings.ToLower(signature)), []byte(want)) {
		return errors.New("cashrail: webhook signature does not match")
	}
	return nil
}

// ParseEvent reads a provider callback tolerantly.
//
// Providers rename fields between versions and between products, so the parser
// accepts the shapes seen in the wild rather than one canonical spelling. An
// unrecognised payload is refused rather than guessed at.
func ParseEvent(body []byte) (Event, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return Event{}, fmt.Errorf("cashrail: malformed callback: %w", err)
	}
	e := Event{
		Reference: pick(raw, "reference", "transactionReference", "transaction_reference", "ref"),
		Status:    strings.ToLower(pick(raw, "status", "transactionStatus", "state")),
	}
	if e.Reference == "" {
		return Event{}, errors.New("cashrail: callback carries no reference")
	}
	return e, nil
}

// Event is a provider callback, normalised.
type Event struct {
	Reference string
	Status    string
}

// Outcome maps a provider status onto a settlement outcome.
func (e Event) Outcome() (string, error) {
	switch e.Status {
	case "success", "successful", "completed", "settled", "paid":
		return StateSettled, nil
	case "failed", "declined", "rejected":
		return StateFailed, nil
	case "returned", "reversed", "refunded":
		return StateReturned, nil
	default:
		// Never guess. A status we do not recognise must not be turned into a
		// money movement on the assumption it probably meant success.
		return "", fmt.Errorf("cashrail: unrecognised provider status %q", e.Status)
	}
}

func pick(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}
