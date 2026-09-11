package exchange

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Membership and the controls that sit in front of order entry.
//
// A member is a firm that takes responsibility for the orders it sends. On a
// public exchange this is most of the commercial relationship; here there is
// one member at launch, but the model carries a member from the first row —
// retrofitting membership onto live trades means touching every order, fill and
// settlement record and backfilling an isolation boundary onto data that was
// never separated.

var (
	// ErrNotAMember is returned for an unknown or inactive firm.
	ErrNotAMember = errors.New("exchange: not an active member")
	// ErrMemberHalted is the kill switch.
	ErrMemberHalted = errors.New("exchange: member is halted")
	// ErrThrottled is returned when a member exceeds its limits for the session.
	ErrThrottled = errors.New("exchange: member throttled")
)

// Member roles.
const (
	RoleBroker      = "broker"
	RoleMarketMaker = "market_maker"
	RoleIssuerAgent = "issuer_agent"
)

// Member is a firm admitted to the exchange.
type Member struct {
	ID     uuid.UUID
	Code   string
	Name   string
	Roles  []string
	Status string
	Halted bool
}

// Admit registers a firm. It starts pending: admission is a decision somebody
// makes after vetting, not a side effect of filling in a form.
func Admit(ctx context.Context, tx pgx.Tx, code, legalName string, roles []string) (uuid.UUID, error) {
	if code == "" || legalName == "" {
		return uuid.Nil, fmt.Errorf("exchange: a member needs a code and a legal name")
	}
	for _, r := range roles {
		switch r {
		case RoleBroker, RoleMarketMaker, RoleIssuerAgent:
		default:
			return uuid.Nil, fmt.Errorf("exchange: %q is not a member role", r)
		}
	}
	if len(roles) == 0 {
		roles = []string{RoleBroker}
	}
	var id uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO members (code, legal_name, roles, status)
		VALUES ($1,$2,$3,'pending') RETURNING id`, code, legalName, roles).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("exchange: admit member: %w", err)
	}
	return id, nil
}

// Activate lets a member start trading, once whatever vetting the rulebook
// requires has actually happened.
func Activate(ctx context.Context, tx pgx.Tx, memberID uuid.UUID, by string) error {
	ct, err := tx.Exec(ctx, `
		UPDATE members SET status = 'active' WHERE id = $1 AND status = 'pending'`, memberID)
	if err != nil {
		return fmt.Errorf("exchange: activate member: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("exchange: member %s is not pending activation", memberID)
	}
	return nil
}

// HaltMember is the kill switch.
//
// Both the exchange and the member can reach it, and it takes effect at the
// gateway rather than by unwinding anything: existing orders stand, nothing new
// is accepted. A member whose system is malfunctioning needs to be stopped in
// one action, not talked through a withdrawal.
func HaltMember(ctx context.Context, tx pgx.Tx, memberID uuid.UUID, reason string) error {
	if reason == "" {
		return fmt.Errorf("exchange: halting a member requires a reason")
	}
	_, err := tx.Exec(ctx, `
		UPDATE members SET halted = true, halted_reason = $2 WHERE id = $1`, memberID, reason)
	if err != nil {
		return fmt.Errorf("exchange: halt member: %w", err)
	}
	return nil
}

// ResumeMember lifts the kill switch.
func ResumeMember(ctx context.Context, tx pgx.Tx, memberID uuid.UUID) error {
	_, err := tx.Exec(ctx, `
		UPDATE members SET halted = false, halted_reason = NULL WHERE id = $1`, memberID)
	return err
}

// CheckMember is the gateway check. It runs before ANYTHING else on order
// entry — before the symbol is resolved, before the client account is looked
// up — so that a firm which is not an active member cannot use refusal codes to
// probe for valid symbols or client account ids.
func CheckMember(ctx context.Context, tx pgx.Tx, memberID uuid.UUID) error {
	return checkMember(ctx, tx, memberID)
}

func checkMember(ctx context.Context, tx pgx.Tx, memberID uuid.UUID) error {
	var status string
	var halted bool
	var reason *string
	err := tx.QueryRow(ctx,
		`SELECT status, halted, halted_reason FROM members WHERE id = $1`, memberID).
		Scan(&status, &halted, &reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotAMember
	}
	if err != nil {
		return fmt.Errorf("exchange: member check: %w", err)
	}
	if status != "active" {
		return fmt.Errorf("%w: status is %s", ErrNotAMember, status)
	}
	if halted {
		why := "halted"
		if reason != nil {
			why = *reason
		}
		return fmt.Errorf("%w: %s", ErrMemberHalted, why)
	}
	return nil
}

// checkThrottles enforces per-session message limits.
//
// The order-to-trade ratio is the one that matters. A member sending thousands
// of orders and trading almost none is either malfunctioning or layering, and
// both call for the same response: stop taking their orders. It only applies
// once there is enough traffic to mean anything — a member whose first order of
// the day has not filled yet has an infinite ratio and has done nothing wrong.
func checkThrottles(ctx context.Context, tx pgx.Tx, memberID uuid.UUID, sessionDate string) error {
	var maxOrders, maxRatio int
	if err := tx.QueryRow(ctx,
		`SELECT max_orders_per_session, max_order_to_trade_ratio FROM members WHERE id = $1`,
		memberID).Scan(&maxOrders, &maxRatio); err != nil {
		return fmt.Errorf("exchange: throttle limits: %w", err)
	}

	var sent, filled int
	err := tx.QueryRow(ctx, `
		INSERT INTO member_activity (member_id, session_date, orders_sent)
		VALUES ($1, $2::date, 0)
		ON CONFLICT (member_id, session_date) DO UPDATE SET orders_sent = member_activity.orders_sent
		RETURNING orders_sent, orders_filled`, memberID, sessionDate).Scan(&sent, &filled)
	if err != nil {
		return fmt.Errorf("exchange: read member activity: %w", err)
	}

	if sent >= maxOrders {
		return fmt.Errorf("%w: %d orders this session, limit %d", ErrThrottled, sent, maxOrders)
	}
	// Below the floor the ratio is noise, not a signal.
	const ratioFloor = 20
	if sent >= ratioFloor && filled == 0 {
		return fmt.Errorf("%w: %d orders and no fills this session", ErrThrottled, sent)
	}
	if filled > 0 && sent/filled > maxRatio {
		return fmt.Errorf("%w: order-to-trade ratio %d exceeds %d", ErrThrottled, sent/filled, maxRatio)
	}
	return nil
}

// recordOrderSent counts an accepted order against the member's session budget.
func recordOrderSent(ctx context.Context, tx pgx.Tx, memberID uuid.UUID, sessionDate string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO member_activity (member_id, session_date, orders_sent)
		VALUES ($1,$2::date,1)
		ON CONFLICT (member_id, session_date) DO UPDATE
		  SET orders_sent = member_activity.orders_sent + 1`, memberID, sessionDate)
	return err
}

// recordFills credits a member's fills, which is what keeps their
// order-to-trade ratio honest.
func recordFills(ctx context.Context, tx pgx.Tx, auctionID uuid.UUID, sessionDate string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO member_activity (member_id, session_date, orders_filled)
		SELECT o.member_id, $2::date, COUNT(*)
		  FROM fills f JOIN orders o ON o.id = f.order_id
		 WHERE f.auction_id = $1 AND o.member_id IS NOT NULL
		 GROUP BY o.member_id
		ON CONFLICT (member_id, session_date) DO UPDATE
		  SET orders_filled = member_activity.orders_filled + EXCLUDED.orders_filled`,
		auctionID, sessionDate)
	if err != nil {
		return fmt.Errorf("exchange: record member fills: %w", err)
	}
	return nil
}
