package institution

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"freedom/api/internal/ledger"
	"freedom/api/internal/money"
	"freedom/api/internal/scheme"
)

// Investor protection and complaints.
//
// Statutory for a Nigerian exchange, and the part of the rulebook an ordinary
// investor is most likely to ever use. Both are built the same way: a clock and
// a named decision-maker, because a claim with no deadline is a claim nobody
// has to answer and a decision with no name is a decision nobody has to defend.

// Grounds for a protection claim.
const (
	GroundsMemberDefault     = "member_default"
	GroundsUnauthorisedTrade = "unauthorised_trading"
	GroundsFailureToDeliver  = "failure_to_deliver"
	GroundsFraud             = "fraud"
)

// ClaimCap is the most any one claimant may be awarded.
//
// A fund with no per-claim cap is a fund that can be exhausted by one claimant,
// leaving nothing for everyone behind them in the queue.
var ClaimCap = money.Naira(5_000_000)

// FileClaim records a claim against the investor protection fund.
func FileClaim(ctx context.Context, tx pgx.Tx, claimant uuid.UUID, member *uuid.UUID,
	amount money.Kobo, grounds, narrative string) (uuid.UUID, error) {

	switch grounds {
	case GroundsMemberDefault, GroundsUnauthorisedTrade, GroundsFailureToDeliver, GroundsFraud:
	default:
		return uuid.Nil, fmt.Errorf("institution: %q is not grounds for a claim", grounds)
	}
	if amount <= 0 {
		return uuid.Nil, fmt.Errorf("institution: a claim must be for a positive amount")
	}
	if narrative == "" {
		return uuid.Nil, errors.New("institution: a claim needs an account of what happened")
	}

	var id uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO protection_claims (claimant, member_id, amount_kobo, grounds, narrative)
		VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		claimant, member, int64(amount), grounds, narrative).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("institution: file claim: %w", err)
	}
	return id, nil
}

// DecideClaim upholds or rejects a claim, and pays an upheld one.
//
// Payment happens in the same transaction as the decision. A claim recorded as
// upheld but unpaid is the state in which an investor is told they have won and
// still has no money, which is worse than a straight rejection.
func DecideClaim(ctx context.Context, tx pgx.Tx, claimID uuid.UUID, uphold bool,
	award money.Kobo, by, businessDate string) error {

	if by == "" {
		return errors.New("institution: a claim decision needs a name")
	}
	var claimant uuid.UUID
	var claimed money.Kobo
	var state string
	if err := tx.QueryRow(ctx, `
		SELECT claimant, amount_kobo, state FROM protection_claims
		 WHERE id = $1 FOR UPDATE`, claimID).Scan(&claimant, &claimed, &state); err != nil {
		return fmt.Errorf("institution: load claim: %w", err)
	}
	if state == "paid" || state == "rejected" {
		return fmt.Errorf("institution: claim %s is already %s", claimID, state)
	}

	if !uphold {
		_, err := tx.Exec(ctx, `
			UPDATE protection_claims SET state = 'rejected', decided_by = $2, decided_at = now()
			 WHERE id = $1`, claimID, by)
		return err
	}

	if award <= 0 || award > claimed {
		return fmt.Errorf("institution: an award of %s is not within the %s claimed", award, claimed)
	}
	if award > ClaimCap {
		// Capping protects everyone behind this claimant in the queue.
		award = ClaimCap
	}

	fund, err := ledger.Resolve(ctx, tx, ledger.Scheme(ledger.KindLossReserve, ledger.AssetNGN))
	if err != nil {
		return err
	}
	payee, err := ledger.Resolve(ctx, tx, ledger.Cardholder(claimant, ledger.KindAvailable, ledger.AssetNGN))
	if err != nil {
		return err
	}

	txID, err := ledger.Post(ctx, tx, ledger.Tx{
		EventType:      "protection.claim_paid",
		BusinessDate:   businessDate,
		IdempotencyKey: "claim|" + claimID.String(),
		CorrelationID:  &claimID,
		Entries: []ledger.Entry{
			{AccountID: fund, Amount: ledger.NGN(-award), Reason: "protection.award"},
			{AccountID: payee, Amount: ledger.NGN(award), Reason: "protection.compensation"},
		},
	})
	if err != nil && !errors.Is(err, ledger.ErrAlreadyPosted) {
		return err
	}

	_, err = tx.Exec(ctx, `
		UPDATE protection_claims
		   SET state = 'paid', awarded_kobo = $2, decided_by = $3, decided_at = now(), ledger_tx_id = $4
		 WHERE id = $1`, claimID, int64(award), by, txID)
	return err
}

// FundBalance is what the protection fund currently holds.
func FundBalance(ctx context.Context, q ledger.Querier) (money.Kobo, error) {
	return ledger.NairaBalance(ctx, q, ledger.Scheme(ledger.KindLossReserve, ledger.AssetNGN))
}

// ---------------------------------------------------------------- complaints

// ComplaintWindow is how long the respondent has. A complaint with no clock is
// a complaint nobody has to answer.
const ComplaintWindow = 21 * 24 * time.Hour

// Complaint targets.
const (
	AgainstMember   = "member"
	AgainstIssuer   = "issuer"
	AgainstExchange = "exchange"
)

// FileComplaint opens a complaint and starts its clock.
func FileComplaint(ctx context.Context, tx pgx.Tx, complainant uuid.UUID, against string,
	member *uuid.UUID, instrument *string, subject, narrative string, now time.Time) (uuid.UUID, error) {

	switch against {
	case AgainstMember, AgainstIssuer, AgainstExchange:
	default:
		return uuid.Nil, fmt.Errorf("institution: a complaint cannot be against %q", against)
	}
	if subject == "" || narrative == "" {
		return uuid.Nil, errors.New("institution: a complaint needs a subject and an account of what happened")
	}

	state := "with_exchange"
	if against == AgainstMember {
		// A member answers for themselves first. The exchange is the escalation,
		// not the first responder.
		state = "with_member"
	}
	respondBy := scheme.BusinessDate(now.Add(ComplaintWindow))

	var id uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO complaints
		  (complainant, against, member_id, instrument_id, subject, narrative, state, respond_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8::date) RETURNING id`,
		complainant, against, member, instrument, subject, narrative, state, respondBy).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("institution: file complaint: %w", err)
	}
	return id, nil
}

// Escalate moves a complaint up. An investor who cannot get an answer must have
// somewhere to go that is not the person who gave them the answer.
func Escalate(ctx context.Context, tx pgx.Tx, complaintID uuid.UUID, to string) error {
	switch to {
	case "with_exchange", "escalated_sec":
	default:
		return fmt.Errorf("institution: cannot escalate to %q", to)
	}
	ct, err := tx.Exec(ctx, `
		UPDATE complaints SET state = $2
		 WHERE id = $1 AND state NOT IN ('resolved','withdrawn')`, complaintID, to)
	if err != nil {
		return fmt.Errorf("institution: escalate: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("institution: complaint %s is closed", complaintID)
	}
	return nil
}

// ResolveComplaint closes one with an outcome and a name.
func ResolveComplaint(ctx context.Context, tx pgx.Tx, complaintID uuid.UUID, resolution, by string) error {
	if resolution == "" || by == "" {
		return errors.New("institution: resolving a complaint requires an outcome and a name")
	}
	ct, err := tx.Exec(ctx, `
		UPDATE complaints SET state = 'resolved', resolution = $2, handled_by = $3, resolved_at = now()
		 WHERE id = $1 AND state <> 'resolved'`, complaintID, resolution, by)
	if err != nil {
		return fmt.Errorf("institution: resolve complaint: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("institution: complaint %s is already resolved", complaintID)
	}
	return nil
}

// Overdue lists complaints past their deadline — the list that makes the clock
// mean something, because nothing else in the system will chase them.
func Overdue(ctx context.Context, tx pgx.Tx, asOf string) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx, `
		SELECT id FROM complaints
		 WHERE respond_by < $1::date AND state NOT IN ('resolved','withdrawn')
		 ORDER BY respond_by`, asOf)
	if err != nil {
		return nil, fmt.Errorf("institution: overdue complaints: %w", err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
