// Package dispute runs the card chargeback lifecycle.
//
// The defining feature is that every stage has a deadline AND a default
// outcome. A dispute clock with no default does not expire, it simply sits: the
// side that benefits from silence stays silent, and on a card network that side
// is whoever is currently holding the money. So the clocks here decide, and the
// decision is recorded as having been made by the clock rather than by a person.
//
// Which way a deadline falls depends on whose turn it is, not on who filed.
// Before the merchant answers, silence means the cardholder wins; after they
// answer, silence means the cardholder accepted the answer.
package dispute

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"freedom/api/internal/money"
	"freedom/api/internal/scheme"
)

// Reason groups. The group sets how long the cardholder has to file and how
// much weight the merchant's evidence has to carry.
const (
	ReasonFraud           = "fraud"
	ReasonProcessingError = "processing_error"
	ReasonAuthorisation   = "authorisation"
	ReasonConsumerDispute = "consumer_dispute"
)

// States.
const (
	StateInitiated         = "initiated"
	StateEvidenceRequested = "evidence_requested"
	StateRepresented       = "represented"
	StatePreArbitration    = "pre_arbitration"
	StateArbitration       = "arbitration"
	WonCardholder          = "won_cardholder"
	WonMerchant            = "won_merchant"
	Withdrawn              = "withdrawn"
)

// The clocks. These mirror the windows a real scheme runs, and the important
// property is that each is shorter than the one before: a dispute that
// escalates has to converge rather than ratchet outward forever.
const (
	// FilingWindow matches the chargeback lock on buyback shares exactly. If
	// the two ever diverge, equity becomes sellable while a dispute against the
	// transaction that funded it is still possible.
	FilingWindow       = 120 * 24 * time.Hour
	RepresentmentDays  = 45
	PreArbitrationDays = 30
	ArbitrationDays    = 30
)

var (
	// ErrTooLate is returned for a dispute filed after the window closed.
	ErrTooLate = errors.New("dispute: the filing window has closed")
	// ErrAlreadyDisputed guards against charging one transaction back twice.
	ErrAlreadyDisputed = errors.New("dispute: this transaction is already disputed")
)

// Dispute is a live case.
type Dispute struct {
	ID            uuid.UUID
	PresentmentID uuid.UUID
	Cardholder    uuid.UUID
	Merchant      uuid.UUID
	ReasonCode    string
	Amount        money.Kobo
	State         string
	RespondBy     string
	Provisional   bool
}

// File opens a dispute against a cleared transaction.
//
// Fraud disputes carry a provisional credit: the cardholder gets their money
// back immediately and the merchant carries the exposure while it is
// investigated. That is the regulatory expectation everywhere, and it is also
// the right default — the person who says their card was used without them
// should not fund the investigation.
func File(ctx context.Context, tx pgx.Tx, presentmentID uuid.UUID,
	reasonCode, detail string, now time.Time) (Dispute, error) {

	switch reasonCode {
	case ReasonFraud, ReasonProcessingError, ReasonAuthorisation, ReasonConsumerDispute:
	default:
		return Dispute{}, fmt.Errorf("dispute: %q is not a chargeback reason", reasonCode)
	}
	if detail == "" {
		return Dispute{}, errors.New("dispute: a chargeback needs a description")
	}

	var d Dispute
	d.PresentmentID = presentmentID
	d.ReasonCode = reasonCode

	var businessDate string
	var kind string
	err := tx.QueryRow(ctx, `
		SELECT p.merchant_id, c.cardholder_id, p.amount_kobo, p.kind,
		       COALESCE(p.business_date::text, p.received_at::date::text)
		  FROM presentments p JOIN cards c ON c.id = p.card_id
		 WHERE p.id = $1`, presentmentID).
		Scan(&d.Merchant, &d.Cardholder, &d.Amount, &kind, &businessDate)
	if errors.Is(err, pgx.ErrNoRows) {
		return Dispute{}, fmt.Errorf("dispute: no such transaction")
	}
	if err != nil {
		return Dispute{}, fmt.Errorf("dispute: load transaction: %w", err)
	}
	if kind != "first" {
		return Dispute{}, fmt.Errorf("dispute: %s presentments cannot be charged back", kind)
	}
	if d.Amount <= 0 {
		return Dispute{}, fmt.Errorf("dispute: cannot charge back a %s presentment", d.Amount)
	}

	cleared, err := scheme.ParseBusinessDate(businessDate)
	if err != nil {
		return Dispute{}, err
	}
	if now.Sub(cleared) > FilingWindow {
		return Dispute{}, fmt.Errorf("%w: %s cleared on %s", ErrTooLate, presentmentID, businessDate)
	}

	d.State = StateEvidenceRequested
	d.RespondBy = scheme.BusinessDate(now.AddDate(0, 0, RepresentmentDays))
	d.Provisional = reasonCode == ReasonFraud

	err = tx.QueryRow(ctx, `
		INSERT INTO disputes
		  (presentment_id, cardholder_id, merchant_id, reason_code, reason_detail,
		   amount_kobo, state, respond_by, filed_on, provisional_credit)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8::date,$9::date,$10)
		ON CONFLICT (presentment_id) DO NOTHING
		RETURNING id`,
		presentmentID, d.Cardholder, d.Merchant, reasonCode, detail,
		int64(d.Amount), d.State, d.RespondBy, scheme.BusinessDate(now), d.Provisional).Scan(&d.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Dispute{}, ErrAlreadyDisputed
	}
	if err != nil {
		return Dispute{}, fmt.Errorf("dispute: file: %w", err)
	}

	if err := event(ctx, tx, d.ID, StateInitiated, d.State, false, "cardholder", detail); err != nil {
		return Dispute{}, err
	}
	if d.Provisional {
		if err := provisionalCredit(ctx, tx, d, scheme.BusinessDate(now)); err != nil {
			return Dispute{}, err
		}
	}
	return d, nil
}

// Represent records the merchant's answer and hands the clock back to the
// cardholder.
func Represent(ctx context.Context, tx pgx.Tx, disputeID uuid.UUID, evidence string, now time.Time) error {
	if evidence == "" {
		return errors.New("dispute: representment requires evidence")
	}
	return advance(ctx, tx, disputeID, []string{StateEvidenceRequested}, StateRepresented,
		scheme.BusinessDate(now.AddDate(0, 0, PreArbitrationDays)), "merchant", evidence)
}

// Escalate moves a dispute to the next stage when the losing side will not
// accept the last one.
func Escalate(ctx context.Context, tx pgx.Tx, disputeID uuid.UUID, by, note string, now time.Time) error {
	var state string
	if err := tx.QueryRow(ctx,
		`SELECT state FROM disputes WHERE id = $1`, disputeID).Scan(&state); err != nil {
		return fmt.Errorf("dispute: load: %w", err)
	}
	switch state {
	case StateRepresented:
		return advance(ctx, tx, disputeID, []string{StateRepresented}, StatePreArbitration,
			scheme.BusinessDate(now.AddDate(0, 0, PreArbitrationDays)), by, note)
	case StatePreArbitration:
		return advance(ctx, tx, disputeID, []string{StatePreArbitration}, StateArbitration,
			scheme.BusinessDate(now.AddDate(0, 0, ArbitrationDays)), by, note)
	default:
		return fmt.Errorf("dispute: cannot escalate from %s", state)
	}
}

func advance(ctx context.Context, tx pgx.Tx, disputeID uuid.UUID, from []string,
	to, respondBy, actor, note string) error {

	var current string
	if err := tx.QueryRow(ctx,
		`SELECT state FROM disputes WHERE id = $1 FOR UPDATE`, disputeID).Scan(&current); err != nil {
		return fmt.Errorf("dispute: load: %w", err)
	}
	ok := false
	for _, f := range from {
		if current == f {
			ok = true
		}
	}
	if !ok {
		return fmt.Errorf("dispute: cannot move from %s to %s", current, to)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE disputes SET state = $2, respond_by = $3::date WHERE id = $1`,
		disputeID, to, respondBy); err != nil {
		return fmt.Errorf("dispute: advance: %w", err)
	}
	return event(ctx, tx, disputeID, current, to, false, actor, note)
}

func event(ctx context.Context, tx pgx.Tx, disputeID uuid.UUID,
	from, to string, byDefault bool, actor, note string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO dispute_events (dispute_id, from_state, to_state, by_default, actor, note)
		VALUES ($1,$2,$3,$4,$5,$6)`, disputeID, from, to, byDefault, actor, note)
	if err != nil {
		return fmt.Errorf("dispute: record event: %w", err)
	}
	return nil
}

// DefaultOutcome is what happens when a deadline passes at a given stage.
//
// Whose turn it is decides, not who filed. Before the merchant answers, silence
// means they have no answer and the cardholder wins. After they answer, silence
// means the cardholder accepted it. Arbitration never defaults — somebody has
// to actually decide, and letting a clock settle it would make the scheme's own
// inaction a ruling.
func DefaultOutcome(state string) (string, bool) {
	switch state {
	case StateInitiated, StateEvidenceRequested:
		return WonCardholder, true
	case StateRepresented, StatePreArbitration:
		return WonMerchant, true
	default:
		return "", false
	}
}

// RunClocks resolves every dispute whose deadline has passed.
//
// This is the job that makes the deadlines real. Without it they are decoration.
func RunClocks(ctx context.Context, tx pgx.Tx, asOf string) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, state FROM disputes
		 WHERE respond_by < $1::date
		   AND state NOT IN ('won_cardholder','won_merchant','withdrawn','arbitration')
		 ORDER BY respond_by, id`, asOf)
	if err != nil {
		return nil, fmt.Errorf("dispute: scan expired: %w", err)
	}
	type expired struct {
		id    uuid.UUID
		state string
	}
	var due []expired
	for rows.Next() {
		var e expired
		if err := rows.Scan(&e.id, &e.state); err != nil {
			rows.Close()
			return nil, err
		}
		due = append(due, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var resolved []uuid.UUID
	for _, e := range due {
		outcome, ok := DefaultOutcome(e.state)
		if !ok {
			continue
		}
		if err := Resolve(ctx, tx, e.id, outcome, "clock", "deadline passed at "+e.state, asOf, true); err != nil {
			return resolved, err
		}
		resolved = append(resolved, e.id)
	}
	return resolved, nil
}
