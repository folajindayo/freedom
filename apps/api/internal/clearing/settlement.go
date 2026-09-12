package clearing

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"freedom/api/internal/money"
)

// Net settlement.
//
// A clearing batch produces one number per participant, and that number is what
// actually moves between banks. Netting is the whole reason a scheme sits
// between two of them: a member that acquired ₦40m and issued ₦38m settles ₦2m,
// not two gross flows in opposite directions.
//
// # Net debit caps
//
// A participant's position is bounded by what they have posted. The scheme
// being able to refuse to settle an over-cap position is the control that stops
// one member's failure from becoming every member's loss — and it only works if
// the refusal happens BEFORE the instruction goes out, which is why the cap is
// checked at computation rather than at payment.

// Position is one participant's net obligation for a batch.
type Position struct {
	ParticipantID uuid.UUID
	Code          string
	Net           money.Kobo
	Acquired      money.Kobo
	Issued        money.Kobo
	Interchange   money.Kobo
	Fees          money.Kobo
	State         string
	CapBreach     money.Kobo
}

// Owes reports whether the participant owes the network.
func (p Position) Owes() bool { return p.Net < 0 }

// ErrPositionsUnbalanced is returned when the net positions do not sum to zero.
//
// They always must: every naira one participant owes is a naira another is
// owed. A non-zero sum means the batch invented or destroyed money, and no
// instruction should be sent on the strength of it.
var ErrPositionsUnbalanced = errors.New("clearing: net settlement positions do not sum to zero")

// ComputePositions nets a finalised batch into one figure per participant.
func ComputePositions(ctx context.Context, tx pgx.Tx, b Batch) ([]Position, error) {
	rows, err := tx.Query(ctx, `
		WITH cleared AS (
		  SELECT p.id, p.amount_kobo, m.acquirer_id, c.issuer_id
		    FROM presentments p
		    JOIN merchants m ON m.id = p.merchant_id
		    JOIN cards c     ON c.id = p.card_id
		   WHERE p.clearing_batch_id = $1),
		acquiring AS (
		  SELECT acquirer_id AS participant, SUM(amount_kobo) AS gross
		    FROM cleared GROUP BY acquirer_id),
		issuing AS (
		  SELECT issuer_id AS participant, SUM(amount_kobo) AS gross
		    FROM cleared GROUP BY issuer_id)
		SELECT pa.id, pa.code,
		       COALESCE(a.gross, 0)::bigint,
		       COALESCE(i.gross, 0)::bigint
		  FROM participants pa
		  LEFT JOIN acquiring a ON a.participant = pa.id
		  LEFT JOIN issuing   i ON i.participant = pa.id
		 WHERE a.participant IS NOT NULL OR i.participant IS NOT NULL
		 ORDER BY pa.code`, b.ID)
	if err != nil {
		return nil, fmt.Errorf("clearing: net positions: %w", err)
	}
	defer rows.Close()

	var out []Position
	var sum money.Kobo
	for rows.Next() {
		var p Position
		if err := rows.Scan(&p.ParticipantID, &p.Code, &p.Acquired, &p.Issued); err != nil {
			return nil, err
		}
		// An acquirer is owed what its merchants sold; an issuer owes what its
		// cardholders spent. A participant wearing both roles nets them.
		p.Net = p.Acquired - p.Issued
		sum += p.Net
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if sum != 0 {
		return nil, fmt.Errorf("%w: they sum to %s", ErrPositionsUnbalanced, sum)
	}

	for i := range out {
		if err := checkCap(ctx, tx, &out[i]); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO settlement_positions
			  (batch_id, participant_id, net_kobo, acquired_kobo, issued_kobo, state, cap_breach_kobo)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT (batch_id, participant_id) DO UPDATE
			  SET net_kobo = EXCLUDED.net_kobo, acquired_kobo = EXCLUDED.acquired_kobo,
			      issued_kobo = EXCLUDED.issued_kobo, state = EXCLUDED.state,
			      cap_breach_kobo = EXCLUDED.cap_breach_kobo`,
			b.ID, out[i].ParticipantID, int64(out[i].Net), int64(out[i].Acquired),
			int64(out[i].Issued), out[i].State, nullableKobo(out[i].CapBreach)); err != nil {
			return nil, fmt.Errorf("clearing: record position: %w", err)
		}
	}
	return out, nil
}

// checkCap marks a position that exceeds what the participant has posted.
//
// Only a debit position can breach: being owed money is not an exposure the
// network needs protecting from.
func checkCap(ctx context.Context, tx pgx.Tx, p *Position) error {
	p.State = "computed"
	if p.Net >= 0 {
		return nil
	}
	var cap money.Kobo
	if err := tx.QueryRow(ctx,
		`SELECT net_debit_cap_kobo FROM participants WHERE id = $1`, p.ParticipantID).Scan(&cap); err != nil {
		return fmt.Errorf("clearing: net debit cap: %w", err)
	}
	if owed := -p.Net; owed > cap {
		p.State = "capped"
		p.CapBreach = owed - cap
	}
	return nil
}

// Instruct queues the cash movements for a batch's positions.
//
// A capped position is deliberately not instructed. The scheme refusing to
// settle is the control, and instructing anyway while recording a breach would
// be theatre — the money would already have moved.
func Instruct(ctx context.Context, tx pgx.Tx, b Batch) (instructed, held int, err error) {
	rows, err := tx.Query(ctx, `
		SELECT id, participant_id, net_kobo, state FROM settlement_positions
		 WHERE batch_id = $1 AND state IN ('computed','capped')
		 ORDER BY participant_id`, b.ID)
	if err != nil {
		return 0, 0, fmt.Errorf("clearing: load positions: %w", err)
	}
	type row struct {
		id          uuid.UUID
		participant uuid.UUID
		net         money.Kobo
		state       string
	}
	var positions []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.participant, &r.net, &r.state); err != nil {
			rows.Close()
			return 0, 0, err
		}
		positions = append(positions, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}

	for _, r := range positions {
		if r.state == "capped" {
			held++
			continue
		}
		if r.net == 0 {
			if _, err := tx.Exec(ctx,
				`UPDATE settlement_positions SET state = 'settled', settled_at = now() WHERE id = $1`,
				r.id); err != nil {
				return instructed, held, err
			}
			continue
		}
		if _, err := tx.Exec(ctx,
			`UPDATE settlement_positions SET state = 'instructed' WHERE id = $1`, r.id); err != nil {
			return instructed, held, fmt.Errorf("clearing: instruct position: %w", err)
		}
		instructed++
	}
	return instructed, held, nil
}

// ReleaseCap lifts a cap breach once a participant posts more collateral.
func ReleaseCap(ctx context.Context, tx pgx.Tx, positionID uuid.UUID, by string) error {
	if by == "" {
		return errors.New("clearing: releasing a capped position requires a name")
	}
	ct, err := tx.Exec(ctx, `
		UPDATE settlement_positions SET state = 'computed', cap_breach_kobo = NULL
		 WHERE id = $1 AND state = 'capped'`, positionID)
	if err != nil {
		return fmt.Errorf("clearing: release cap: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("clearing: position %s is not capped", positionID)
	}
	return nil
}

func nullableKobo(v money.Kobo) *int64 {
	if v == 0 {
		return nil
	}
	n := int64(v)
	return &n
}
