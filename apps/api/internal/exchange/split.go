package exchange

import (
	"context"
	"fmt"
	"math/big"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"freedom/api/internal/alloc"
	"freedom/api/internal/ledger"
	"freedom/api/internal/share"
)

// ApplySplit executes a split or bonus issue.
//
// Everything below happens in one transaction because the pieces are only
// correct together. A split that scaled holdings without scaling the authorised
// ceiling, or without scaling the reference price, leaves a working-looking
// instrument that is quietly broken in ways nobody notices until the next
// buyback or the next order.
func ApplySplit(ctx context.Context, tx pgx.Tx, actionID uuid.UUID, effectiveDate string) error {
	a, err := loadAction(ctx, tx, actionID)
	if err != nil {
		return err
	}
	if a.Kind != ActionSplit && a.Kind != ActionBonus {
		return fmt.Errorf("exchange: %s is not a share-count action", a.Kind)
	}
	if a.State != "record_taken" {
		return fmt.Errorf("%w: take the record first (state is %s)", ErrNotDue, a.State)
	}
	if err := lockInstrument(ctx, tx, a.InstrumentID); err != nil {
		return err
	}

	holders, err := entitlements(ctx, tx, actionID)
	if err != nil {
		return err
	}
	if len(holders) == 0 {
		return fmt.Errorf("exchange: %s has no holders to split", a.InstrumentID)
	}

	var before share.Units
	weights := make([]int64, len(holders))
	for i, h := range holders {
		before += h.Units
		weights[i] = int64(h.Units)
	}

	// The target total, computed once from the total rather than as a sum of
	// per-account roundings. Then distributed by largest remainder, so the parts
	// sum to the target exactly and no holder is systematically shorted by the
	// rounding — which, repeated over a decade of actions, is a real transfer.
	after := share.Units(mulDiv(int64(before), a.RatioNum, a.RatioDen))
	newBalances, err := alloc.Proportional(after, weights)
	if err != nil {
		return fmt.Errorf("exchange: distributing split: %w", err)
	}

	external, err := ledger.Resolve(ctx, tx, ledger.Scheme(ledger.KindExternal, a.InstrumentID))
	if err != nil {
		return err
	}

	entries := []ledger.Entry{}
	var issued share.Units
	for i, h := range holders {
		delta := newBalances[i] - h.Units
		if delta == 0 {
			continue
		}
		entries = append(entries, ledger.Entry{
			AccountID: h.AccountID,
			Amount:    ledger.Equity(a.Symbol, delta),
			Reason:    a.Kind + ".adjust",
		})
		issued += delta
		if _, err := tx.Exec(ctx, `
			UPDATE corporate_action_entitlements SET units_delta = $3
			 WHERE action_id = $1 AND account_id = $2`,
			actionID, h.AccountID, int64(delta)); err != nil {
			return err
		}
	}
	if issued == 0 {
		return fmt.Errorf("exchange: %s at %d:%d moved nothing", a.Kind, a.RatioNum, a.RatioDen)
	}
	entries = append(entries, ledger.Entry{
		AccountID: external,
		Amount:    ledger.Equity(a.Symbol, -issued),
		Reason:    a.Kind + ".issue",
	})

	txID, err := ledger.Post(ctx, tx, ledger.Tx{
		EventType:      "corporate_action." + a.Kind,
		BusinessDate:   effectiveDate,
		IdempotencyKey: "corpaction|" + actionID.String(),
		CorrelationID:  &actionID,
		Entries:        entries,
	})
	if err != nil {
		if isAlreadyPosted(err) {
			return nil
		}
		return err
	}

	// Lots follow their account's new balance, distributed the same way. Basis
	// is NOT touched: a 2:1 split halves the cost per share and leaves the total
	// cost exactly where it was, which is also what the tax treatment expects.
	for i, h := range holders {
		if err := rescaleLots(ctx, tx, h.AccountID, a.InstrumentID, newBalances[i]); err != nil {
			return err
		}
	}

	// The authorised ceiling scales with everything else.
	//
	// Without this the first buyback after a 2:1 split finds issued units above
	// the authorised count, applyReleaseCap returns zero headroom, and the
	// buyback stops for that instrument permanently — with no error, because
	// "the cap bound" is a normal outcome.
	if _, err := tx.Exec(ctx, `
		UPDATE instruments
		   SET shares_authorised_units = shares_authorised_units * $2 / $3,
		       reference_price_kobo    = GREATEST(reference_price_kobo * $3 / $2, 1)
		 WHERE id = $1`, a.InstrumentID, a.RatioNum, a.RatioDen); err != nil {
		return fmt.Errorf("exchange: rescale instrument: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO cap_table_events (instrument_id, kind, units_delta, ledger_tx_id, note)
		VALUES ($1,'split',$2,$3,$4)`,
		a.InstrumentID, int64(issued), txID,
		fmt.Sprintf("%s %d:%d", a.Kind, a.RatioNum, a.RatioDen)); err != nil {
		return err
	}

	// The adjustment factor. Price falls by the same ratio the share count
	// rises, so history stays comparable — and the buyback's VWAP cap keeps
	// working across the action instead of sitting at twice the real price for
	// the length of its window.
	if _, err := tx.Exec(ctx, `
		INSERT INTO corporate_action_factors
		  (instrument_id, effective_date, action_id, price_factor, unit_factor)
		VALUES ($1,$2::date,$3, $5::numeric / $4::numeric, $4::numeric / $5::numeric)
		ON CONFLICT DO NOTHING`,
		a.InstrumentID, effectiveDate, actionID, a.RatioNum, a.RatioDen); err != nil {
		return fmt.Errorf("exchange: write adjustment factor: %w", err)
	}

	_, err = tx.Exec(ctx, `
		UPDATE corporate_actions SET state = 'applied', ledger_tx_id = $2 WHERE id = $1`,
		actionID, txID)
	return err
}

// rescaleLots redistributes an account's lots to sum to its new balance.
//
// Cost basis per lot is untouched. A split does not change what anyone paid; it
// changes how many pieces they hold it in.
func rescaleLots(ctx context.Context, tx pgx.Tx, accountID uuid.UUID,
	instrumentID string, target share.Units) error {

	rows, err := tx.Query(ctx, `
		SELECT id, units_open FROM holding_lots
		 WHERE account_id = $1 AND instrument_id = $2 AND units_open > 0
		 ORDER BY acquired_at, id FOR UPDATE`, accountID, instrumentID)
	if err != nil {
		return fmt.Errorf("exchange: load lots for rescale: %w", err)
	}
	type lot struct {
		id    int64
		units share.Units
	}
	var lots []lot
	var total share.Units
	for rows.Next() {
		var l lot
		if err := rows.Scan(&l.id, &l.units); err != nil {
			rows.Close()
			return err
		}
		lots = append(lots, l)
		total += l.units
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(lots) == 0 || total == 0 {
		// A treasury or reserve account holds a balance without lots; only
		// cardholder wallets carry cost basis.
		return nil
	}

	weights := make([]int64, len(lots))
	for i, l := range lots {
		weights[i] = int64(l.units)
	}
	parts, err := alloc.Proportional(target, weights)
	if err != nil {
		return fmt.Errorf("exchange: distributing lots: %w", err)
	}
	for i, l := range lots {
		if _, err := tx.Exec(ctx, `
			UPDATE holding_lots SET units = units + ($2 - units_open), units_open = $2
			 WHERE id = $1`, l.id, int64(parts[i])); err != nil {
			return fmt.Errorf("exchange: rescale lot %d: %w", l.id, err)
		}
	}
	return nil
}

func entitlements(ctx context.Context, tx pgx.Tx, actionID uuid.UUID) ([]holding, error) {
	rows, err := tx.Query(ctx, `
		SELECT account_id, units_held FROM corporate_action_entitlements
		 WHERE action_id = $1 ORDER BY account_id`, actionID)
	if err != nil {
		return nil, fmt.Errorf("exchange: load entitlements: %w", err)
	}
	defer rows.Close()
	var out []holding
	for rows.Next() {
		var h holding
		if err := rows.Scan(&h.AccountID, &h.Units); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// mulDiv computes floor(a*num/den) exactly.
//
// The intermediate is big.Int rather than a 128-bit hand-roll: share counts at
// 1e-8 scale against a split ratio overflow int64 well inside realistic
// numbers, and this runs once per corporate action, not once per tap.
func mulDiv(a, num, den int64) int64 {
	n := new(big.Int).Mul(big.NewInt(a), big.NewInt(num))
	return new(big.Int).Quo(n, big.NewInt(den)).Int64()
}
