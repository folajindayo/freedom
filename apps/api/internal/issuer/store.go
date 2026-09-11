package issuer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"freedom/api/internal/money"
	"freedom/api/internal/scheme"
)

type terminal struct {
	ID             uuid.UUID
	MerchantID     uuid.UUID
	AcquirerID     uuid.UUID
	IssuerDefault  uuid.UUID
	Status         string
	MerchantStatus string
	CoFundBps      int64
	Attestation    string
}

func loadTerminal(ctx context.Context, tx pgx.Tx, id uuid.UUID) (terminal, error) {
	var t terminal
	err := tx.QueryRow(ctx, `
		SELECT t.id, t.merchant_id, m.acquirer_id, t.status, m.status, m.cofund_bps,
		       t.attestation_verdict
		  FROM terminals t JOIN merchants m ON m.id = t.merchant_id
		 WHERE t.id = $1`, id).
		Scan(&t.ID, &t.MerchantID, &t.AcquirerID, &t.Status, &t.MerchantStatus,
			&t.CoFundBps, &t.Attestation)
	return t, err
}

type credential struct {
	ID           uuid.UUID
	CardID       uuid.UUID
	Tech         string
	TagUID       []byte
	LastCounter  int64
	TokenCurrent []byte
	TokenPrev    []byte
	PrevFrom     int64
	Status       string
}

// lockCredentialByUID takes a row lock for the length of the transaction.
//
// This is the serialisation point for concurrent taps of one card. Without it,
// two terminals reading the same rolling token would both see it as current and
// both approve; with it, the second waits and then finds the token burnt.
func lockCredentialByUID(ctx context.Context, tx pgx.Tx, uid []byte) (credential, error) {
	var c credential
	if len(uid) == 0 {
		return c, pgx.ErrNoRows
	}
	err := tx.QueryRow(ctx, `
		SELECT id, card_id, tech, tag_uid, last_counter, token_current, token_prev,
		       token_prev_from_counter, status
		  FROM card_credentials WHERE tag_uid = $1 FOR UPDATE`, uid).
		Scan(&c.ID, &c.CardID, &c.Tech, &c.TagUID, &c.LastCounter,
			&c.TokenCurrent, &c.TokenPrev, &c.PrevFrom, &c.Status)
	return c, err
}

type card struct {
	ID           uuid.UUID
	CardholderID uuid.UUID
	IssuerID     uuid.UUID
	Status       string
	HolderStatus string
	ExpiresOn    time.Time
	PerTxnCap    money.Kobo
	DailyCap     money.Kobo
	OnlineOnly   bool
}

func loadCard(ctx context.Context, tx pgx.Tx, id uuid.UUID) (card, error) {
	var c card
	err := tx.QueryRow(ctx, `
		SELECT c.id, c.cardholder_id, c.issuer_id, c.status, h.status, c.expires_on,
		       c.per_txn_cap_kobo, c.daily_cap_kobo, c.online_only
		  FROM cards c JOIN cardholders h ON h.id = c.cardholder_id
		 WHERE c.id = $1`, id).
		Scan(&c.ID, &c.CardholderID, &c.IssuerID, &c.Status, &c.HolderStatus,
			&c.ExpiresOn, &c.PerTxnCap, &c.DailyCap, &c.OnlineOnly)
	return c, err
}

// authorisedToday is the velocity check. It counts what is still outstanding
// plus what has already cleared today, so that reversing a transaction frees
// the headroom it consumed.
func authorisedToday(ctx context.Context, tx pgx.Tx, cardID uuid.UUID, businessDate string) (money.Kobo, error) {
	var total int64
	err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(approved_kobo), 0)
		  FROM authorizations
		 WHERE card_id = $1 AND business_date = $2::date
		   AND result IN ('approved','partial')`, cardID, businessDate).Scan(&total)
	return money.Kobo(total), err
}

func advanceCounter(ctx context.Context, tx pgx.Tx, credID uuid.UUID, counter int64) error {
	// Conditional: the counter may only ever move forward. A concurrent tap
	// that lost the race cannot rewind it.
	_, err := tx.Exec(ctx, `
		UPDATE card_credentials SET last_counter = $2
		 WHERE id = $1 AND last_counter <= $2`, credID, counter)
	if err != nil {
		return fmt.Errorf("issuer: advance counter: %w", err)
	}
	return nil
}

// rotateToken burns the presented token and installs the next one. The token
// that was current becomes previous, so a write-back that fails in the field
// leaves the cardholder one working tap rather than a frozen card.
func rotateToken(ctx context.Context, tx pgx.Tx, credID uuid.UUID, prev, next []byte,
	atCounter int64, now time.Time) error {
	_, err := tx.Exec(ctx, `
		UPDATE card_credentials
		   SET token_prev = $2, token_current = $3, token_prev_from_counter = $5,
		       token_seq = token_seq + 1, token_rotated_at = $4
		 WHERE id = $1`, credID, prev, next, now, atCounter)
	if err != nil {
		return fmt.Errorf("issuer: rotate token: %w", err)
	}
	return nil
}

// freezeForClone stops both the credential and the card it belongs to.
//
// Freezing the credential alone would leave a replacement tag working on a card
// whose identity is known to be copied, so the card goes too and a human decides
// when it comes back.
func freezeForClone(ctx context.Context, tx pgx.Tx, c credential, reason string, now time.Time) error {
	if _, err := tx.Exec(ctx, `
		UPDATE card_credentials
		   SET status = 'frozen', clone_suspected_at = $2
		 WHERE id = $1`, c.ID, now); err != nil {
		return fmt.Errorf("issuer: freeze credential: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE cards SET status = 'frozen', frozen_reason = $2, frozen_at = $3
		 WHERE id = $1 AND status <> 'cancelled'`, c.CardID, "clone_suspected:"+reason, now); err != nil {
		return fmt.Errorf("issuer: freeze card: %w", err)
	}
	return nil
}

type authRow struct {
	CardID       uuid.UUID
	CredentialID uuid.UUID
	Terminal     terminal
	STAN         string
	Amount       money.Kobo
	Counter      int64
	BusinessDate string
	HoldTxID     uuid.UUID
	ExpiresAt    time.Time
}

func recordAuthorization(ctx context.Context, tx pgx.Tx, r authRow) (uuid.UUID, string, error) {
	rrn := newRRN()
	var id uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO authorizations
		  (card_id, credential_id, terminal_id, merchant_id, acquirer_id, issuer_id,
		   stan, rrn, requested_kobo, approved_kobo, outstanding_kobo, result,
		   fee_schedule_version, counter, business_date, hold_tx_id, expires_at)
		SELECT $1, $2, $3, $4, $5, c.issuer_id, $6, $7, $8, $8, $8, 'approved',
		       $9, $10, $11::date, $12, $13
		  FROM cards c WHERE c.id = $1
		RETURNING id`,
		r.CardID, r.CredentialID, r.Terminal.ID, r.Terminal.MerchantID, r.Terminal.AcquirerID,
		r.STAN, rrn, int64(r.Amount), currentScheduleVersion, r.Counter,
		r.BusinessDate, r.HoldTxID, r.ExpiresAt).Scan(&id)
	if err != nil {
		return uuid.Nil, "", fmt.Errorf("issuer: record authorization: %w", err)
	}
	return id, rrn[:6], nil
}

// currentScheduleVersion is pinned onto every authorisation so that clearing
// prices with the schedule that was live when the cardholder tapped, not
// whichever is live when the batch runs.
const currentScheduleVersion = 1

// replay returns the decision already recorded for a retried STAN.
func (a *Authorizer) replay(ctx context.Context, tx pgx.Tx, term terminal, req Request, businessDate string) (Response, error) {
	var id uuid.UUID
	var rrn, result string
	err := tx.QueryRow(ctx, `
		SELECT id, rrn, result FROM authorizations
		 WHERE acquirer_id = $1 AND business_date = $2::date AND terminal_id = $3 AND stan = $4`,
		term.AcquirerID, businessDate, term.ID, req.STAN).Scan(&id, &rrn, &result)
	if err != nil {
		return Response{}, fmt.Errorf("issuer: replay lookup: %w", err)
	}
	return Response{
		Approved:     result == "approved" || result == "partial",
		AuthID:       id,
		AuthCode:     rrn[:6],
		BusinessDate: businessDate,
	}, nil
}

// newRRN builds a retrieval reference number. Twelve characters, unique enough
// that a merchant quoting one to support identifies exactly one transaction.
func newRRN() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		// The CSPRNG failing is not a condition to paper over with a weaker
		// source; the caller's transaction will roll back.
		panic("issuer: system entropy unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}

func decodeHex(s string) ([]byte, error) { return hex.DecodeString(s) }

var _ = scheme.DeclineNone
