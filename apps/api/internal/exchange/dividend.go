package exchange

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"freedom/api/internal/alloc"
	"freedom/api/internal/ledger"
	"freedom/api/internal/money"
	"freedom/api/internal/share"
)

// PayDividend distributes cash to the holders of record.
//
// This is the best retention loop the product has. A cardholder taps at a shop,
// earns a slice of it, and some months later the shop pays them — in naira that
// lands in the balance they spend at that same shop. Nothing else in the system
// makes the ownership feel real in the same way.
//
// The dividend lands in `available`, not in a segregated investment balance,
// precisely so it is spendable.
func PayDividend(ctx context.Context, tx pgx.Tx, actionID uuid.UUID, payDate string) (money.Kobo, error) {
	a, err := loadAction(ctx, tx, actionID)
	if err != nil {
		return 0, err
	}
	if a.Kind != ActionDividend && a.Kind != ActionReturnCap {
		return 0, fmt.Errorf("exchange: %s is not a cash distribution", a.Kind)
	}
	if a.State != "record_taken" {
		return 0, fmt.Errorf("%w: take the record first (state is %s)", ErrNotDue, a.State)
	}

	holders, err := entitlements(ctx, tx, actionID)
	if err != nil {
		return 0, err
	}
	if len(holders) == 0 {
		return 0, fmt.Errorf("exchange: %s has no holders of record", a.InstrumentID)
	}

	// The declared total, computed once from the aggregate holding. Each
	// holder's share is then pro-rated out of it with largest remainder, so the
	// treasury debit equals the sum of the credits exactly. Computing each
	// holder's dividend independently from their own holding and rounding would
	// leave a residue the balance trigger rejects.
	var totalUnits share.Units
	weights := make([]int64, len(holders))
	for i, h := range holders {
		totalUnits += h.Units
		weights[i] = int64(h.Units)
	}
	declared, err := share.CostOf(totalUnits, a.DPS)
	if err != nil {
		return 0, err
	}
	gross, err := alloc.Proportional(declared, weights)
	if err != nil {
		return 0, fmt.Errorf("exchange: distributing dividend: %w", err)
	}

	treasuryCash, err := ledger.Resolve(ctx, tx,
		ledger.Company(a.CompanyID, ledger.KindTreasuryCash, ledger.AssetNGN))
	if err != nil {
		return 0, err
	}
	withheld, err := ledger.Resolve(ctx, tx, ledger.Scheme(ledger.KindTaxWithheld, ledger.AssetNGN))
	if err != nil {
		return 0, err
	}

	entries := []ledger.Entry{
		{AccountID: treasuryCash, Amount: ledger.NGN(-declared), Reason: "dividend.declared"},
	}
	var whtTotal money.Kobo

	for i, h := range holders {
		if gross[i] == 0 {
			continue
		}
		owner, kind, err := ownerOf(ctx, tx, h.AccountID)
		if err != nil {
			return 0, err
		}
		// The company's own treasury holding does not pay itself a dividend;
		// its share simply is not declared out.
		if kind == ledger.KindTreasury {
			entries = append(entries, ledger.Entry{
				AccountID: treasuryCash, Amount: ledger.NGN(gross[i]), Reason: "dividend.treasury_retained",
			})
			continue
		}

		// Withholding is held separately and remitted. It is never scheme income.
		wht := money.RateOfUp(gross[i], a.WHTBps)
		if a.Kind == ActionReturnCap {
			wht = 0 // a return of capital is not income
		}
		net := gross[i] - wht
		whtTotal += wht

		cash, err := ledger.Resolve(ctx, tx, ledger.Cardholder(owner, ledger.KindAvailable, ledger.AssetNGN))
		if err != nil {
			return 0, err
		}
		if net != 0 {
			entries = append(entries, ledger.Entry{
				AccountID: cash, Amount: ledger.NGN(net), Reason: "dividend.net",
			})
		}
		if _, err := tx.Exec(ctx, `
			UPDATE corporate_action_entitlements SET gross_kobo = $3, wht_kobo = $4
			 WHERE action_id = $1 AND account_id = $2`,
			actionID, h.AccountID, int64(gross[i]), int64(wht)); err != nil {
			return 0, err
		}
	}

	if whtTotal > 0 {
		entries = append(entries, ledger.Entry{
			AccountID: withheld, Amount: ledger.NGN(whtTotal), Reason: "dividend.withholding",
		})
	}

	if _, err := ledger.Post(ctx, tx, ledger.Tx{
		EventType:      "corporate_action." + a.Kind,
		BusinessDate:   payDate,
		IdempotencyKey: "corpaction|" + actionID.String(),
		CorrelationID:  &actionID,
		Entries:        entries,
	}); err != nil {
		if isAlreadyPosted(err) {
			return declared, nil
		}
		return 0, err
	}

	// A cash dividend drops the price mechanically on the ex-date. Left
	// unadjusted, the reference is stale by the dividend per share, which shifts
	// the entire price band — and every sensible order then gets rejected as
	// out of band.
	//
	// A return of capital additionally reduces cost basis; a dividend does not.
	if _, err := tx.Exec(ctx, `
		UPDATE instruments SET reference_price_kobo = GREATEST(reference_price_kobo - $2, 1)
		 WHERE id = $1`, a.InstrumentID, int64(a.DPS)); err != nil {
		return 0, err
	}

	var prior int64
	if err := tx.QueryRow(ctx,
		`SELECT reference_price_kobo + $2 FROM instruments WHERE id = $1`,
		a.InstrumentID, int64(a.DPS)).Scan(&prior); err != nil {
		return 0, err
	}
	if prior > int64(a.DPS) {
		if _, err := tx.Exec(ctx, `
			INSERT INTO corporate_action_factors
			  (instrument_id, effective_date, action_id, price_factor, unit_factor)
			VALUES ($1,$2::date,$3, ($4::numeric - $5::numeric) / $4::numeric, 1)
			ON CONFLICT DO NOTHING`,
			a.InstrumentID, a.ExDate, actionID, prior, int64(a.DPS)); err != nil {
			return 0, fmt.Errorf("exchange: write dividend factor: %w", err)
		}
	}

	if _, err := tx.Exec(ctx,
		`UPDATE corporate_actions SET state = 'applied' WHERE id = $1`, actionID); err != nil {
		return 0, err
	}
	return declared, nil
}

// ownerOf resolves an account back to its beneficial owner and kind.
func ownerOf(ctx context.Context, tx pgx.Tx, accountID uuid.UUID) (uuid.UUID, string, error) {
	var owner *uuid.UUID
	var kind string
	if err := tx.QueryRow(ctx,
		`SELECT owner_id, kind::text FROM accounts WHERE id = $1`, accountID).Scan(&owner, &kind); err != nil {
		return uuid.Nil, "", fmt.Errorf("exchange: resolve account owner: %w", err)
	}
	if owner == nil {
		return uuid.Nil, kind, nil
	}
	return *owner, kind, nil
}
