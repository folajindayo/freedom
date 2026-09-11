// Package registrar is the keeper of who owns what.
//
// Not a record of trades — the authoritative register of ownership, and the
// functions that touch it outside of trading: off-market transfers, and the
// statements a holder disputes against.
//
// In Nigeria this is a separately licensed capital market function. Whether
// Freedom holds that licence itself or appoints an existing registrar and keeps
// this as a sub-register under a nominee, the code is the same: somebody has to
// know, exactly, who owns what and how they came to own it.
package registrar

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"freedom/api/internal/ledger"
	"freedom/api/internal/money"
	"freedom/api/internal/share"
)

// Reasons a holding moves without a trade.
const (
	TransferInheritance    = "inheritance"
	TransferCourtOrder     = "court_order"
	TransferGift           = "gift"
	TransferCorrection     = "correction"
	TransferAccountClosure = "account_closure"
)

// RequestTransfer records an intention to move shares off-market.
//
// It does not move anything. Every off-market transfer needs a documented
// reason and a second person's approval, because this is the one path that
// changes ownership with no counterparty, no price and no market to witness it.
// It is the table a regulator reads first.
func RequestTransfer(ctx context.Context, tx pgx.Tx, t Transfer) (uuid.UUID, error) {
	switch t.Reason {
	case TransferInheritance, TransferCourtOrder, TransferGift,
		TransferCorrection, TransferAccountClosure:
	default:
		return uuid.Nil, fmt.Errorf("registrar: %q is not a transfer reason", t.Reason)
	}
	if t.Units <= 0 {
		return uuid.Nil, fmt.Errorf("registrar: transfer must be positive, got %s", t.Units)
	}
	if t.Reference == "" {
		return uuid.Nil, fmt.Errorf(
			"registrar: a %s transfer needs a reference — a probate number, a court reference, a case id",
			t.Reason)
	}
	if t.RequestedBy == "" {
		return uuid.Nil, fmt.Errorf("registrar: a transfer needs a requester")
	}

	var id uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO share_transfers
		  (instrument_id, from_account, to_account, units, reason, reference, requested_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		t.InstrumentID, t.From, t.To, int64(t.Units), t.Reason, t.Reference, t.RequestedBy).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("registrar: request transfer: %w", err)
	}
	return id, nil
}

// Transfer is an off-market movement.
type Transfer struct {
	InstrumentID string
	Symbol       string
	From, To     uuid.UUID
	Units        share.Units
	Reason       string
	Reference    string
	RequestedBy  string
}

// Approve authorises a requested transfer.
//
// The approver may not be the requester. One person moving someone else's
// shares on their own signature is the control failure this exists to prevent,
// and it is worth an explicit check rather than a policy nobody enforces.
func Approve(ctx context.Context, tx pgx.Tx, transferID uuid.UUID, approver string) error {
	if approver == "" {
		return fmt.Errorf("registrar: approval requires a name")
	}
	var requester string
	if err := tx.QueryRow(ctx,
		`SELECT requested_by FROM share_transfers WHERE id = $1 AND state = 'requested'`,
		transferID).Scan(&requester); err != nil {
		return fmt.Errorf("registrar: load transfer: %w", err)
	}
	if requester == approver {
		return fmt.Errorf("registrar: %s requested this transfer and may not also approve it", approver)
	}
	_, err := tx.Exec(ctx, `
		UPDATE share_transfers SET state = 'approved', approved_by = $2 WHERE id = $1`,
		transferID, approver)
	return err
}

// Execute moves the shares and carries the cost basis with them.
//
// Basis travels with the lot on purpose. An inherited holding that arrives with
// a zero basis would show the beneficiary an enormous fictitious gain the first
// time they sold, and there would be no way to reconstruct the real figure.
func Execute(ctx context.Context, tx pgx.Tx, transferID uuid.UUID, businessDate string) error {
	var t Transfer
	var state string
	err := tx.QueryRow(ctx, `
		SELECT s.instrument_id, i.symbol, s.from_account, s.to_account, s.units, s.reason, s.state
		  FROM share_transfers s JOIN instruments i ON i.id = s.instrument_id
		 WHERE s.id = $1 FOR UPDATE`, transferID).
		Scan(&t.InstrumentID, &t.Symbol, &t.From, &t.To, &t.Units, &t.Reason, &state)
	if err != nil {
		return fmt.Errorf("registrar: load transfer: %w", err)
	}
	if state != "approved" {
		return fmt.Errorf("registrar: transfer is %s, not approved", state)
	}

	txID, err := ledger.Post(ctx, tx, ledger.Tx{
		EventType:      "registrar.transfer",
		BusinessDate:   businessDate,
		IdempotencyKey: "transfer|" + transferID.String(),
		CorrelationID:  &transferID,
		Entries: []ledger.Entry{
			{AccountID: t.From, Amount: ledger.Equity(t.Symbol, -t.Units), Reason: "transfer." + t.Reason},
			{AccountID: t.To, Amount: ledger.Equity(t.Symbol, t.Units), Reason: "transfer." + t.Reason},
		},
	})
	if err != nil {
		return err
	}

	if err := moveLots(ctx, tx, t, txID, businessDate); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		UPDATE share_transfers SET state = 'executed', executed_at = now(), ledger_tx_id = $2
		 WHERE id = $1`, transferID, txID)
	return err
}

// moveLots consumes the sender's oldest transferable lots and opens matching
// ones for the recipient, carrying basis and the original acquisition date.
func moveLots(ctx context.Context, tx pgx.Tx, t Transfer, txID uuid.UUID, businessDate string) error {
	rows, err := tx.Query(ctx, `
		SELECT id, units_open, cost_open_kobo, acquired_at, transferable_from
		  FROM holding_lots
		 WHERE account_id = $1 AND instrument_id = $2
		   AND units_open > units_reserved AND transferable_from <= $3::date
		 ORDER BY acquired_at, id FOR UPDATE`, t.From, t.InstrumentID, businessDate)
	if err != nil {
		return fmt.Errorf("registrar: load lots: %w", err)
	}
	type lot struct {
		id       int64
		open     share.Units
		cost     money.Kobo
		acquired time.Time
		unlock   time.Time
	}
	var lots []lot
	var available share.Units
	for rows.Next() {
		var l lot
		if err := rows.Scan(&l.id, &l.open, &l.cost, &l.acquired, &l.unlock); err != nil {
			rows.Close()
			return err
		}
		lots = append(lots, l)
		available += l.open
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if available < t.Units {
		return fmt.Errorf("registrar: %s transferable, %s requested", available, t.Units)
	}

	remaining := t.Units
	for _, l := range lots {
		if remaining <= 0 {
			break
		}
		take := l.open
		if take > remaining {
			take = remaining
		}
		// Basis travels proportionally, and a fully consumed lot lands on zero.
		basis := money.Kobo(int64(l.cost) * int64(take) / int64(l.open))
		if take == l.open {
			basis = l.cost
		}
		if _, err := tx.Exec(ctx, `
			UPDATE holding_lots SET units_open = units_open - $2, cost_open_kobo = cost_open_kobo - $3
			 WHERE id = $1`, l.id, int64(take), int64(basis)); err != nil {
			return fmt.Errorf("registrar: consume lot %d: %w", l.id, err)
		}
		// The recipient inherits the acquisition date and the unlock date too:
		// a transfer must not be a way to launder a locked holding into a
		// sellable one.
		if _, err := tx.Exec(ctx, `
			INSERT INTO holding_lots (account_id, instrument_id, units, units_open,
			                          cost_kobo, cost_open_kobo, transferable_from,
			                          acquired_at, ledger_tx_id)
			VALUES ($1,$2,$3,$3,$4,$4,$5,$6,$7)`,
			t.To, t.InstrumentID, int64(take), int64(basis), l.unlock, l.acquired, txID); err != nil {
			return fmt.Errorf("registrar: open recipient lot: %w", err)
		}
		remaining -= take
	}
	return nil
}

// Statement is what a holder is sent and disputes against.
type Statement struct {
	AccountID   uuid.UUID
	PeriodStart string
	PeriodEnd   string
	Holdings    []StatementLine
}

// StatementLine is one instrument on a statement.
type StatementLine struct {
	Symbol      string      `json:"symbol"`
	Units       share.Units `json:"units"`
	CostKobo    money.Kobo  `json:"cost_kobo"`
	ValueKobo   money.Kobo  `json:"value_kobo"`
	PriceKobo   money.Kobo  `json:"price_kobo"`
	LockedUnits share.Units `json:"locked_units"`
}

// Generate builds and stores a statement.
//
// Stored, not rendered on demand. A holder disputes what they were SENT, so
// what they were sent has to still exist — regenerating it later from current
// data would answer a different question, and would quietly change its answer
// every time the price moved.
func Generate(ctx context.Context, tx pgx.Tx, accountOwner uuid.UUID,
	periodStart, periodEnd string) (Statement, error) {

	s := Statement{PeriodStart: periodStart, PeriodEnd: periodEnd}

	rows, err := tx.Query(ctx, `
		SELECT i.symbol,
		       COALESCE(SUM(l.units_open), 0)::bigint,
		       COALESCE(SUM(l.cost_open_kobo), 0)::bigint,
		       COALESCE(SUM(l.units_open) FILTER (WHERE l.transferable_from > $2::date), 0)::bigint,
		       i.reference_price_kobo,
		       MIN(a.id::text)
		  FROM holding_lots l
		  JOIN accounts a    ON a.id = l.account_id
		  JOIN instruments i ON i.id = l.instrument_id
		 WHERE a.owner_id = $1 AND a.kind = 'stock_wallet' AND l.units_open > 0
		 GROUP BY i.symbol, i.reference_price_kobo
		 ORDER BY i.symbol`, accountOwner, periodEnd)
	if err != nil {
		return s, fmt.Errorf("registrar: statement holdings: %w", err)
	}
	defer rows.Close()

	var anyAccount string
	for rows.Next() {
		var line StatementLine
		var acct string
		if err := rows.Scan(&line.Symbol, &line.Units, &line.CostKobo,
			&line.LockedUnits, &line.PriceKobo, &acct); err != nil {
			return s, err
		}
		v, err := share.CostOf(line.Units, line.PriceKobo)
		if err != nil {
			return s, err
		}
		line.ValueKobo = v
		s.Holdings = append(s.Holdings, line)
		anyAccount = acct
	}
	if err := rows.Err(); err != nil {
		return s, err
	}
	if anyAccount == "" {
		return s, nil // nothing held; no statement to store
	}
	if err := s.AccountID.UnmarshalText([]byte(anyAccount)); err != nil {
		return s, err
	}

	content, err := json.Marshal(map[string]any{
		"period_start": periodStart, "period_end": periodEnd, "holdings": s.Holdings,
	})
	if err != nil {
		return s, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO holding_statements (account_id, period_start, period_end, content)
		VALUES ($1,$2::date,$3::date,$4)
		ON CONFLICT (account_id, period_end) DO NOTHING`,
		s.AccountID, periodStart, periodEnd, content); err != nil {
		return s, fmt.Errorf("registrar: store statement: %w", err)
	}
	return s, nil
}
