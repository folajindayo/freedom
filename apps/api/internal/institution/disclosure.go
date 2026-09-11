// Package institution is the half of an exchange that is not a matching engine.
//
// Disclosure, an index, investor protection, complaints and the clock. None of
// it matches an order and all of it is what makes the venue an exchange rather
// than a piece of software that pairs buyers with sellers.
package institution

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"freedom/api/internal/exchange"
)

// Disclosure kinds.
const (
	DisclosureResults         = "results"
	DisclosureCorporateAction = "corporate_action"
	DisclosureMaterialEvent   = "material_event"
	DisclosureDirectorDealing = "director_dealing"
	DisclosureShareholding    = "shareholding"
	DisclosureParticulars     = "listing_particulars"
	DisclosureSuspension      = "suspension"
)

// Submit lodges an announcement and halts trading until it is published.
//
// The halt is the point. Between an issuer submitting and the market reading,
// the issuer knows something the market does not, and anybody who trades in
// that window trades against people who cannot know what they know. Halting is
// not caution; it is the only way the window can be closed honestly.
func Submit(ctx context.Context, tx pgx.Tx, instrumentID, kind, headline, body, by string) (uuid.UUID, error) {
	switch kind {
	case DisclosureResults, DisclosureCorporateAction, DisclosureMaterialEvent,
		DisclosureDirectorDealing, DisclosureShareholding, DisclosureParticulars,
		DisclosureSuspension:
	default:
		return uuid.Nil, fmt.Errorf("institution: %q is not a disclosure kind", kind)
	}
	if headline == "" || body == "" {
		return uuid.Nil, errors.New("institution: a disclosure needs a headline and a body")
	}
	if by == "" {
		return uuid.Nil, errors.New("institution: a disclosure needs a submitter")
	}

	// A price-sensitive announcement halts the market; a routine one does not.
	var haltID *uuid.UUID
	if priceSensitive(kind) {
		if halted, _, err := alreadyHalted(ctx, tx, instrumentID); err != nil {
			return uuid.Nil, err
		} else if !halted {
			id, err := exchange.Halt(ctx, tx, instrumentID, exchange.HaltNewsPending, by,
				map[string]any{"awaiting": kind, "headline": headline})
			if err != nil {
				return uuid.Nil, err
			}
			haltID = &id
		}
	}

	var id uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO disclosures (instrument_id, kind, headline, body, submitted_by, halt_id)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
		instrumentID, kind, headline, body, by, haltID).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("institution: submit disclosure: %w", err)
	}
	return id, nil
}

// priceSensitive reports whether an announcement should stop the market until
// everyone has seen it.
func priceSensitive(kind string) bool {
	switch kind {
	case DisclosureResults, DisclosureCorporateAction, DisclosureMaterialEvent, DisclosureSuspension:
		return true
	default:
		// A director's dealing or a shareholding notice is published for the
		// record. It is news about the company's owners rather than about the
		// company, and halting for it would stop the market several times a week.
		return false
	}
}

// Publish releases an announcement to everyone at one instant and lifts the
// halt it caused.
func Publish(ctx context.Context, tx pgx.Tx, disclosureID uuid.UUID, by string) error {
	var instrumentID string
	var published *time.Time
	var haltID *uuid.UUID
	err := tx.QueryRow(ctx, `
		SELECT instrument_id, published_at, halt_id FROM disclosures
		 WHERE id = $1 FOR UPDATE`, disclosureID).Scan(&instrumentID, &published, &haltID)
	if err != nil {
		return fmt.Errorf("institution: load disclosure: %w", err)
	}
	if published != nil {
		return fmt.Errorf("institution: disclosure %s was already published", disclosureID)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE disclosures SET published_at = now() WHERE id = $1`, disclosureID); err != nil {
		return fmt.Errorf("institution: publish: %w", err)
	}
	// Only lift the halt this disclosure raised. A separate regulatory halt
	// must survive an issuer publishing their results.
	if haltID != nil {
		if err := exchange.Release(ctx, tx, instrumentID, by); err != nil {
			return err
		}
	}
	return nil
}

// Pending lists announcements submitted but not yet published, which is the
// queue somebody has to be watching: every one of them is holding a market shut.
func Pending(ctx context.Context, tx pgx.Tx) ([]PendingDisclosure, error) {
	rows, err := tx.Query(ctx, `
		SELECT d.id, i.symbol, d.kind, d.headline, d.submitted_at
		  FROM disclosures d JOIN instruments i ON i.id = d.instrument_id
		 WHERE d.published_at IS NULL ORDER BY d.submitted_at`)
	if err != nil {
		return nil, fmt.Errorf("institution: pending disclosures: %w", err)
	}
	defer rows.Close()

	var out []PendingDisclosure
	for rows.Next() {
		var p PendingDisclosure
		if err := rows.Scan(&p.ID, &p.Symbol, &p.Kind, &p.Headline, &p.SubmittedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PendingDisclosure is an unpublished announcement.
type PendingDisclosure struct {
	ID          uuid.UUID
	Symbol      string
	Kind        string
	Headline    string
	SubmittedAt time.Time
}

func alreadyHalted(ctx context.Context, tx pgx.Tx, instrumentID string) (bool, string, error) {
	var reason string
	err := tx.QueryRow(ctx, `
		SELECT reason FROM trading_halts
		 WHERE instrument_id = $1 AND released_at IS NULL LIMIT 1`, instrumentID).Scan(&reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	return true, reason, nil
}
