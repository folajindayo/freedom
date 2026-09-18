// Package rail is the door between Tapp and Freedom.
//
// Tapp is a working card rail: it holds the cardholders, the merchants and the
// credentials, charges the tap, and pays the merchant. Freedom is a working
// market: a fee split becomes a buyback intent, an allocation from treasury at
// the auction price, a locked lot, and an auction in which the holder can sell.
// This package is the contract in docs/INTEGRATION.md — neither side is
// rewritten, and every step below runs through the same production path a tap
// on Freedom's own credential would: exchange.Apply/Assess/AdmitListing,
// clearing.Run, buyback.RunSession, dispute's unwind.
//
// # One participant, both roles
//
// Tapp collected the ticket from the cardholder and paid the merchant itself,
// so in Freedom's ledger it is one scheme participant that is both issuer and
// acquirer of every tap it delivers. Its net position after clearing is exactly
// the merchant service charge it owes the scheme. That is not a shortcut, it is
// the accounting; every leg is an ordinary balanced posting.
//
// # Ids
//
// external_ref values are Tapp's own ids. Freedom stores them on the row it
// creates for them and never interprets them.
package rail

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"freedom/api/internal/buyback"
	"freedom/api/internal/exchange"
	"freedom/api/internal/fee"
	"freedom/api/internal/ledger"
	"freedom/api/internal/money"
)

// ParticipantCode marks the Tapp participant. participants.code is already
// unique, so no schema is needed to find it.
const ParticipantCode = "TAPP"

// productCode is the synthetic card product every rail cardholder carries.
const productCode = "tapp"

// MarketMakerRef is the cardholder ref of the house market maker's trading
// principal. It is a Freedom id, not a Tapp one, and cannot collide with
// Tapp's because Tapp's carry no colon.
const MarketMakerRef = "freedom:mm"

// arnPrefix and reversalPrefix key presentments to their tap. The ARN is the
// acquirer's reference and Tapp is the acquirer, so its tap id is the ARN.
const (
	arnPrefix      = "TAPP-"
	reversalPrefix = "TAPP-REV-"
)

var (
	// ErrNotFound is returned when a rail id names nothing Freedom knows.
	ErrNotFound = errors.New("rail: not found")
	// ErrInvalid marks a request the rail sent wrongly, as opposed to one a
	// rule refused.
	ErrInvalid = errors.New("rail: invalid request")
)

// AlreadyListedError refuses a listing for a company that already has a live
// instrument under another merchant. A company has one line of shares; a
// second merchant of the same company is a decision for a person, not an
// automatic second listing.
type AlreadyListedError struct {
	RCNumber string
	Symbol   string
}

func (e *AlreadyListedError) Error() string {
	return fmt.Sprintf("rail: %s is already listed as %s", e.RCNumber, e.Symbol)
}

// Service serves the rail. Every write is one database transaction opened
// here, so a failure anywhere in a step leaves nothing half-done.
type Service struct {
	Pool *pgxpool.Pool
	// Now is the clock, injectable so tests do not depend on the day they run.
	Now func() time.Time

	Schedules    fee.Schedules
	Buyback      *buyback.Engine
	Engine       *exchange.Engine
	Surveillance *exchange.Surveillance
}

// New builds a service on the launch engines and defences.
func New(pool *pgxpool.Pool) *Service {
	return &Service{
		Pool:         pool,
		Now:          time.Now,
		Schedules:    fee.StaticSchedules{1: fee.SchemeV1()},
		Buyback:      buyback.New(),
		Engine:       exchange.NewEngine(),
		Surveillance: exchange.NewSurveillance(),
	}
}

// identity is what every rail write needs to find first.
type identity struct {
	Participant uuid.UUID
	Sponsor     uuid.UUID
	// MarketMaker is the cardholder the house market maker trades as, and
	// MarketMakerClient its client account under the Sponsor member.
	MarketMaker       uuid.UUID
	MarketMakerClient uuid.UUID
}

// Ensure makes the rail's fixed rows exist: the Tapp participant, the member
// that sponsors its listings and makes its markets, the house market maker's
// trading account, and the launch fee schedule. Idempotent, and cheap enough
// to call at the start of every write — reading first so a steady state never
// writes.
func Ensure(ctx context.Context, tx pgx.Tx) error {
	_, err := ensure(ctx, tx)
	return err
}

func ensure(ctx context.Context, tx pgx.Tx) (identity, error) {
	var id identity
	err := tx.QueryRow(ctx, `SELECT id FROM participants WHERE code = $1`, ParticipantCode).Scan(&id.Participant)
	if errors.Is(err, pgx.ErrNoRows) {
		// Both roles: Tapp issues the credential and acquires the merchant. The
		// net debit cap stays at zero because its settlement position always
		// nets to nothing — it is both sides of every tap — and its real
		// exposure is the MSC it owes, which is a treasury matter, not a cap.
		err = tx.QueryRow(ctx, `
			INSERT INTO participants (code, legal_name, roles, status)
			VALUES ($1, 'Tapp', ARRAY['issuer','acquirer'], 'active')
			ON CONFLICT (code) DO UPDATE SET code = EXCLUDED.code
			RETURNING id`, ParticipantCode).Scan(&id.Participant)
	}
	if err != nil {
		return id, fmt.Errorf("rail: participant: %w", err)
	}

	// The listing standard requires a sponsor and the rail request carries
	// none: Tapp is the sponsor of record for the businesses it brings, as an
	// issuer agent. It is also the house market maker: the one member whose
	// orders the venue itself enters, from one client account, on the quoting
	// engine's formula and nobody's discretion (exchange/quoting.go). The
	// conflict in sponsoring a listing and quoting it is LISTING-RULES §7's,
	// and is managed the same way — on the record, not waved away.
	var roles []string
	err = tx.QueryRow(ctx, `SELECT id, roles FROM members WHERE code = $1`, ParticipantCode).Scan(&id.Sponsor, &roles)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `
			INSERT INTO members (code, legal_name, roles, status)
			VALUES ($1, 'Tapp', ARRAY['issuer_agent','market_maker'], 'active')
			ON CONFLICT (code) DO UPDATE SET code = EXCLUDED.code
			RETURNING id`, ParticipantCode).Scan(&id.Sponsor)
		roles = []string{exchange.RoleIssuerAgent, exchange.RoleMarketMaker}
	}
	if err != nil {
		return id, fmt.Errorf("rail: sponsor member: %w", err)
	}
	if !slices.Contains(roles, exchange.RoleMarketMaker) {
		if _, err := tx.Exec(ctx, `
			UPDATE members SET roles = array_append(roles, 'market_maker') WHERE id = $1`, id.Sponsor); err != nil {
			return id, fmt.Errorf("rail: market maker role: %w", err)
		}
	}

	// The house market maker trades as a cardholder because that is the
	// identity the book reserves against and settles to: every order on the
	// venue trades from a cardholder's wallet and available cash. It is a
	// principal, not a person, and its ref says so.
	err = tx.QueryRow(ctx, `SELECT id FROM cardholders WHERE external_ref = $1`, MarketMakerRef).Scan(&id.MarketMaker)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `
			INSERT INTO cardholders (phone, display_name, external_ref, kyc_tier,
			                         bvn_verified_at, disclosure_accepted_at, disclosure_version)
			VALUES ($1, 'Freedom Market Making', $1, 3, now(), now(), 'house-v1')
			ON CONFLICT (external_ref) DO UPDATE SET external_ref = EXCLUDED.external_ref
			RETURNING id`, MarketMakerRef).Scan(&id.MarketMaker)
	}
	if err != nil {
		return id, fmt.Errorf("rail: market maker: %w", err)
	}
	err = tx.QueryRow(ctx, `SELECT id FROM client_accounts WHERE cardholder_id = $1`, id.MarketMaker).Scan(&id.MarketMakerClient)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `
			INSERT INTO client_accounts (member_id, cardholder_id, label)
			VALUES ($1, $2, 'market_maker') RETURNING id`, id.Sponsor, id.MarketMaker).Scan(&id.MarketMakerClient)
	}
	if err != nil {
		return id, fmt.Errorf("rail: market maker account: %w", err)
	}

	var have bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM fee_schedules WHERE version = 1)`).Scan(&have); err != nil {
		return id, fmt.Errorf("rail: fee schedule: %w", err)
	}
	if !have {
		if _, err := tx.Exec(ctx, `
			INSERT INTO fee_schedules (version, effective_from, definition)
			VALUES (1, '2026-01-01', '{"name":"SchemeV1"}'::jsonb)
			ON CONFLICT (version) DO NOTHING`); err != nil {
			return id, fmt.Errorf("rail: fee schedule: %w", err)
		}
	}
	return id, nil
}

// upsertCardholder finds or creates the Freedom cardholder for a rail user, and
// the one synthetic card that lets presentments reference them.
//
// The KYC and disclosure fields are set at creation because the buyback will
// not allocate to a cardholder without them (equity_allocation_permitted).
// Tapp holds the person's KYC and presents the risk disclosure; Freedom records
// the rail's attestation rather than re-collecting it. The disclosure version
// names the rail so the attestation is distinguishable from Freedom's own.
func upsertCardholder(ctx context.Context, tx pgx.Tx, id identity, ref, displayName string) (holder, card uuid.UUID, err error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return uuid.Nil, uuid.Nil, fmt.Errorf("%w: cardholder_ref is required", ErrInvalid)
	}
	var current string
	err = tx.QueryRow(ctx, `SELECT id, display_name FROM cardholders WHERE external_ref = $1`, ref).
		Scan(&holder, &current)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		name := strings.TrimSpace(displayName)
		if name == "" {
			name = ref
		}
		err = tx.QueryRow(ctx, `
			INSERT INTO cardholders (phone, display_name, external_ref, kyc_tier,
			                         bvn_verified_at, disclosure_accepted_at, disclosure_version)
			VALUES ('tapp:' || $1, $2, $1, 1, now(), now(), 'tapp-rail-v1')
			ON CONFLICT (external_ref) DO UPDATE SET external_ref = EXCLUDED.external_ref
			RETURNING id`, ref, name).Scan(&holder)
		if err != nil {
			return uuid.Nil, uuid.Nil, fmt.Errorf("rail: create cardholder: %w", err)
		}
	case err != nil:
		return uuid.Nil, uuid.Nil, fmt.Errorf("rail: find cardholder: %w", err)
	default:
		if name := strings.TrimSpace(displayName); name != "" && name != current {
			if _, err := tx.Exec(ctx, `UPDATE cardholders SET display_name = $2 WHERE id = $1`, holder, name); err != nil {
				return uuid.Nil, uuid.Nil, fmt.Errorf("rail: rename cardholder: %w", err)
			}
		}
	}

	// This row exists only because presentments reference cards. The
	// credential lives in Tapp: there is no PAN, no tag, and nothing here can
	// authorise anything. The token is a plain hash of the rail id rather than
	// the keyed HMAC a real PAN needs, because the input is not a PAN and the
	// keyed hash exists to protect the small PAN space from brute force.
	err = tx.QueryRow(ctx, `SELECT id FROM cards WHERE cardholder_id = $1 AND product_code = $2`,
		holder, productCode).Scan(&card)
	if errors.Is(err, pgx.ErrNoRows) {
		token := sha256.Sum256([]byte("tapp:" + ref))
		err = tx.QueryRow(ctx, `
			INSERT INTO cards (cardholder_id, issuer_id, product_code, pan_token, pan_last4,
			                   pan_bin, expires_on, online_only)
			VALUES ($1, $2, $3, $4, '0000', '99999999', '2099-12-31', true)
			RETURNING id`, holder, id.Participant, productCode, token[:]).Scan(&card)
	}
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("rail: card: %w", err)
	}
	return holder, card, nil
}

// merchant is a rail merchant as Freedom knows it.
type merchant struct {
	ID        uuid.UUID
	CompanyID *uuid.UUID
	CoFundBps int64
}

// findMerchant looks a merchant up by its rail id.
func findMerchant(ctx context.Context, q ledger.Querier, ref string) (merchant, error) {
	var m merchant
	err := q.QueryRow(ctx, `SELECT id, company_id, cofund_bps FROM merchants WHERE external_ref = $1`, ref).
		Scan(&m.ID, &m.CompanyID, &m.CoFundBps)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, fmt.Errorf("%w: merchant %q", ErrNotFound, ref)
	}
	if err != nil {
		return m, fmt.Errorf("rail: find merchant: %w", err)
	}
	return m, nil
}

// upsertMerchant finds or creates the merchant a tap names.
//
// A tap can arrive for a merchant that has never registered a business: Tapp
// delivers every tap, and the funding for an unlisted merchant accrues rather
// than failing. Until the merchant onboards, Freedom knows it only by its rail
// id, so the names are the id and the MCC is the unassigned code. Onboarding
// replaces them.
func upsertMerchant(ctx context.Context, tx pgx.Tx, id identity, ref string) (merchant, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return merchant{}, fmt.Errorf("%w: merchant_ref is required", ErrInvalid)
	}
	m, err := findMerchant(ctx, tx, ref)
	if err == nil {
		return m, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return m, err
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO merchants (acquirer_id, legal_name, trading_name, mcc, kyb_status, external_ref)
		VALUES ($1, $2, $2, '0000', 'pending', $2)
		ON CONFLICT (external_ref) DO UPDATE SET external_ref = EXCLUDED.external_ref
		RETURNING id, company_id, cofund_bps`, id.Participant, ref).Scan(&m.ID, &m.CompanyID, &m.CoFundBps)
	if err != nil {
		return m, fmt.Errorf("rail: create merchant: %w", err)
	}
	return m, nil
}

// breakdownFor re-prices a presentment exactly as clearing priced it: the
// schedule version pinned on the presentment and the merchant's co-funding.
// fee.Compute is deterministic, so this is the breakdown clearing used, not an
// estimate of it.
func (s *Service) breakdownFor(ctx context.Context, q ledger.Querier, presentmentID uuid.UUID) (fee.Breakdown, error) {
	var version int
	var cofund, amount int64
	err := q.QueryRow(ctx, `
		SELECT p.fee_schedule_version, m.cofund_bps, p.amount_kobo
		  FROM presentments p JOIN merchants m ON m.id = p.merchant_id
		 WHERE p.id = $1`, presentmentID).Scan(&version, &cofund, &amount)
	if err != nil {
		return fee.Breakdown{}, fmt.Errorf("rail: load presentment %s: %w", presentmentID, err)
	}
	sched, err := s.Schedules.Version(version)
	if err != nil {
		return fee.Breakdown{}, err
	}
	return fee.Compute(sched, fee.Input{Ticket: money.Kobo(amount).Abs(), CoFundBps: cofund})
}

// currentSchedule is the fee schedule version in force on a business date.
func currentSchedule(ctx context.Context, q ledger.Querier, businessDate string) (int, error) {
	var v int
	err := q.QueryRow(ctx, `
		SELECT version FROM fee_schedules
		 WHERE effective_from <= $1::date AND (effective_to IS NULL OR effective_to > $1::date)
		 ORDER BY version DESC LIMIT 1`, businessDate).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("rail: no fee schedule is in force on %s", businessDate)
	}
	return v, err
}

// lockRef serialises every write about one rail id. Tapp's outbox retries, and
// two deliveries of the same tap racing each other must not both get past the
// replay check.
func lockRef(ctx context.Context, tx pgx.Tx, kind, ref string) error {
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext('rail:' || $1 || ':' || $2))`, kind, ref); err != nil {
		return fmt.Errorf("rail: lock %s %s: %w", kind, ref, err)
	}
	return nil
}

// inTx runs fn in one transaction and commits it.
func (s *Service) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// tapRefOf recovers the rail's tap id from a presentment ARN.
func tapRefOf(arn string) *string {
	if strings.HasPrefix(arn, arnPrefix) && !strings.HasPrefix(arn, reversalPrefix) {
		r := strings.TrimPrefix(arn, arnPrefix)
		return &r
	}
	return nil
}
