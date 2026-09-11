// Package acquirer is the merchant's side of the network: it takes the tap from
// the terminal, submits it for authorisation, and presents it for clearing.
package acquirer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"oja/api/internal/money"
)

// Capture submits an approved authorisation for clearing.
//
// Card-present transactions capture immediately — the goods have left the
// counter — so this normally runs straight after an approval. It is separate
// from authorisation anyway, because the two fail independently and because a
// refund or a reversal is a presentment with no new authorisation behind it.
func Capture(ctx context.Context, tx pgx.Tx, authID uuid.UUID, amount money.Kobo) (uuid.UUID, error) {
	if amount <= 0 {
		return uuid.Nil, fmt.Errorf("acquirer: capture amount must be positive, got %s", amount)
	}

	var outstanding money.Kobo
	var cardID, merchantID uuid.UUID
	var version int
	err := tx.QueryRow(ctx, `
		SELECT outstanding_kobo, card_id, merchant_id, fee_schedule_version
		  FROM authorizations WHERE id = $1 FOR UPDATE`, authID).
		Scan(&outstanding, &cardID, &merchantID, &version)
	if err != nil {
		return uuid.Nil, fmt.Errorf("acquirer: load authorization: %w", err)
	}
	if amount > outstanding {
		// Capturing more than was authorised is how a merchant accidentally
		// bills a customer twice. The network refuses rather than trusting the
		// terminal's arithmetic.
		return uuid.Nil, fmt.Errorf(
			"acquirer: capture of %s exceeds the %s outstanding on authorization %s",
			amount, outstanding, authID)
	}

	var id uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO presentments
		  (authorization_id, merchant_id, card_id, kind, amount_kobo, arn, fee_schedule_version)
		VALUES ($1,$2,$3,'first',$4,$5,$6)
		RETURNING id`,
		authID, merchantID, cardID, int64(amount), newARN(), version).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("acquirer: present: %w", err)
	}
	return id, nil
}

// Refund presents a negative amount against an original sale.
func Refund(ctx context.Context, tx pgx.Tx, authID uuid.UUID, amount money.Kobo) (uuid.UUID, error) {
	if amount <= 0 {
		return uuid.Nil, fmt.Errorf("acquirer: refund amount must be positive, got %s", amount)
	}
	var cardID, merchantID uuid.UUID
	var version int
	if err := tx.QueryRow(ctx, `
		SELECT card_id, merchant_id, fee_schedule_version FROM authorizations WHERE id = $1`,
		authID).Scan(&cardID, &merchantID, &version); err != nil {
		return uuid.Nil, fmt.Errorf("acquirer: load authorization: %w", err)
	}

	var id uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO presentments
		  (authorization_id, merchant_id, card_id, kind, amount_kobo, arn, fee_schedule_version)
		VALUES ($1,$2,$3,'refund',$4,$5,$6)
		RETURNING id`,
		authID, merchantID, cardID, -int64(amount), newARN(), version).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("acquirer: present refund: %w", err)
	}
	return id, nil
}

// newARN builds an acquirer reference number, the identifier a merchant quotes
// to support and the key a dispute is filed against.
func newARN() string {
	b := make([]byte, 9)
	if _, err := rand.Read(b); err != nil {
		panic("acquirer: system entropy unavailable: " + err.Error())
	}
	return "ARN" + hex.EncodeToString(b)
}
