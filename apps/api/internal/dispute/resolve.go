package dispute

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"freedom/api/internal/fee"
	"freedom/api/internal/ledger"
	"freedom/api/internal/money"
	"freedom/api/internal/share"
)

// Resolve closes a dispute and moves the money.
//
// When the cardholder wins, three things happen in one transaction: the sale is
// reversed, the merchant is debited, and the equity the buyback bought with
// that transaction's fees is unwound. Doing any of them without the others
// leaves the network having paid for a transaction that did not happen.
func Resolve(ctx context.Context, tx pgx.Tx, disputeID uuid.UUID,
	outcome, by, note, businessDate string, byClock bool) error {

	if outcome != WonCardholder && outcome != WonMerchant && outcome != Withdrawn {
		return fmt.Errorf("dispute: %q is not an outcome", outcome)
	}
	if by == "" {
		return errors.New("dispute: a resolution needs a name")
	}

	var d Dispute
	var state string
	err := tx.QueryRow(ctx, `
		SELECT id, presentment_id, cardholder_id, merchant_id, reason_code,
		       amount_kobo, state, provisional_credit
		  FROM disputes WHERE id = $1 FOR UPDATE`, disputeID).
		Scan(&d.ID, &d.PresentmentID, &d.Cardholder, &d.Merchant, &d.ReasonCode,
			&d.Amount, &state, &d.Provisional)
	if err != nil {
		return fmt.Errorf("dispute: load: %w", err)
	}
	if state == WonCardholder || state == WonMerchant || state == Withdrawn {
		return fmt.Errorf("dispute: %s is already resolved as %s", disputeID, state)
	}

	var resolutionTx *uuid.UUID
	switch outcome {
	case WonCardholder:
		txID, err := settleForCardholder(ctx, tx, d, businessDate)
		if err != nil {
			return err
		}
		resolutionTx = &txID
		// The equity comes back too. A chargeback that reversed the payment but
		// left the shares in place would mean the network bought a cardholder
		// equity out of fees on a transaction that, in the end, never happened.
		if err := Unwind(ctx, tx, d, businessDate); err != nil {
			return err
		}
	case WonMerchant:
		if d.Provisional {
			// The provisional credit was a loan against the outcome, and the
			// outcome went the other way.
			txID, err := reverseProvisional(ctx, tx, d, businessDate)
			if err != nil {
				return err
			}
			resolutionTx = &txID
		}
	}

	if _, err := tx.Exec(ctx, `
		UPDATE disputes SET state = $2, resolved_at = now(), resolved_by = $3,
		       outcome_note = $4, resolution_tx_id = $5
		 WHERE id = $1`, disputeID, outcome, by, note, resolutionTx); err != nil {
		return fmt.Errorf("dispute: resolve: %w", err)
	}
	return event(ctx, tx, disputeID, state, outcome, byClock, by, note)
}

// originalFees recomputes the fee split exactly as it was priced, from the
// schedule version pinned on the presentment.
func originalFees(ctx context.Context, tx pgx.Tx, d Dispute) (fee.Breakdown, uuid.UUID, error) {
	var version int
	var cofundBps int64
	var issuer uuid.UUID
	err := tx.QueryRow(ctx, `
		SELECT p.fee_schedule_version, m.cofund_bps, c.issuer_id
		  FROM presentments p
		  JOIN merchants m ON m.id = p.merchant_id
		  JOIN cards c     ON c.id = p.card_id
		 WHERE p.id = $1`, d.PresentmentID).Scan(&version, &cofundBps, &issuer)
	if err != nil {
		return fee.Breakdown{}, uuid.Nil, fmt.Errorf("dispute: load fee context: %w", err)
	}
	sched, err := Schedules.Version(version)
	if err != nil {
		return fee.Breakdown{}, uuid.Nil, err
	}
	bd, err := fee.Compute(sched, fee.Input{Ticket: d.Amount, CoFundBps: cofundBps})
	if err != nil {
		return fee.Breakdown{}, uuid.Nil, err
	}
	return bd, issuer, nil
}

// Schedules resolves a pinned fee schedule version. It is a package variable so
// that a historical schedule can be registered without threading it through
// every dispute call — a chargeback may reference a version retired years ago.
var Schedules fee.Schedules = fee.StaticSchedules{1: fee.SchemeV1()}

// provisionalCredit funds a fraud claimant while the case is investigated.
func provisionalCredit(ctx context.Context, tx pgx.Tx, d Dispute, businessDate string) error {
	holder, err := ledger.Resolve(ctx, tx, ledger.Cardholder(d.Cardholder, ledger.KindAvailable, ledger.AssetNGN))
	if err != nil {
		return err
	}
	suspense, err := ledger.Resolve(ctx, tx, ledger.Scheme(ledger.KindSuspense, ledger.AssetNGN))
	if err != nil {
		return err
	}
	txID, err := ledger.Post(ctx, tx, ledger.Tx{
		EventType:      "dispute.provisional_credit",
		BusinessDate:   businessDate,
		IdempotencyKey: "dispute.provisional|" + d.ID.String(),
		CorrelationID:  &d.ID,
		Entries: []ledger.Entry{
			{AccountID: suspense, Amount: ledger.NGN(-d.Amount), Reason: "dispute.provisional"},
			{AccountID: holder, Amount: ledger.NGN(d.Amount), Reason: "dispute.provisional_credit"},
		},
	})
	if err != nil && !errors.Is(err, ledger.ErrAlreadyPosted) {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE disputes SET provisional_tx_id = $2 WHERE id = $1`, d.ID, txID)
	return err
}

func reverseProvisional(ctx context.Context, tx pgx.Tx, d Dispute, businessDate string) (uuid.UUID, error) {
	holder, err := ledger.Resolve(ctx, tx, ledger.Cardholder(d.Cardholder, ledger.KindAvailable, ledger.AssetNGN))
	if err != nil {
		return uuid.Nil, err
	}
	suspense, err := ledger.Resolve(ctx, tx, ledger.Scheme(ledger.KindSuspense, ledger.AssetNGN))
	if err != nil {
		return uuid.Nil, err
	}
	return ledger.Post(ctx, tx, ledger.Tx{
		EventType:      "dispute.provisional_reversed",
		BusinessDate:   businessDate,
		IdempotencyKey: "dispute.reverse|" + d.ID.String(),
		CorrelationID:  &d.ID,
		Entries: []ledger.Entry{
			{AccountID: holder, Amount: ledger.NGN(-d.Amount), Reason: "dispute.provisional_reversed"},
			{AccountID: suspense, Amount: ledger.NGN(d.Amount), Reason: "dispute.provisional_reversed"},
		},
	})
}

// settleForCardholder reverses the sale in full: the cardholder is made whole,
// the merchant is debited, and every fee the transaction generated is unwound.
//
// Reversing only the ticket would leave the issuer holding interchange, the
// scheme holding its fee and the buyback pool holding its share — all earned on
// a sale that, in the end, did not happen. The merchant would also still have
// paid a merchant service charge on a transaction they are being debited for,
// which is charging them twice.
//
// The fees are recomputed from the schedule version PINNED at authorisation and
// negated, never recomputed at today's rates. A chargeback nine weeks later
// must reverse exactly what was charged, not what would be charged now.
//
// The merchant's receivable goes negative if they have already been paid out.
// That is correct and deliberate: it is a debt they owe the network, and hiding
// it by refusing to post would mean the network absorbed the chargeback
// silently.
func settleForCardholder(ctx context.Context, tx pgx.Tx, d Dispute, businessDate string) (uuid.UUID, error) {
	holder, err := ledger.Resolve(ctx, tx, ledger.Cardholder(d.Cardholder, ledger.KindAvailable, ledger.AssetNGN))
	if err != nil {
		return uuid.Nil, err
	}
	merchant, err := ledger.Resolve(ctx, tx,
		ledger.Merchant(d.Merchant, ledger.KindMerchantReceivable, ledger.AssetNGN))
	if err != nil {
		return uuid.Nil, err
	}
	suspense, err := ledger.Resolve(ctx, tx, ledger.Scheme(ledger.KindSuspense, ledger.AssetNGN))
	if err != nil {
		return uuid.Nil, err
	}

	// The fee split, as it was priced at authorisation.
	bd, issuer, err := originalFees(ctx, tx, d)
	if err != nil {
		return uuid.Nil, err
	}
	reversal := bd.Negate()

	interchange, err := ledger.Resolve(ctx, tx,
		ledger.Participant(issuer, ledger.KindInterchangeIncome, ledger.AssetNGN))
	if err != nil {
		return uuid.Nil, err
	}
	revenue, err := ledger.Resolve(ctx, tx, ledger.Scheme(ledger.KindRevenue, ledger.AssetNGN))
	if err != nil {
		return uuid.Nil, err
	}
	pool, err := ledger.Resolve(ctx, tx, ledger.Scheme(ledger.KindBuybackPool, ledger.AssetNGN))
	if err != nil {
		return uuid.Nil, err
	}

	// The merchant gets back what they netted, not the ticket: they never
	// received the fee portion, so they must not be debited for it.
	entries := []ledger.Entry{
		{AccountID: merchant, Amount: ledger.NGN(-(d.Amount - bd.MerchantPays())), Reason: "chargeback.merchant_debit"},
	}
	add := func(acct uuid.UUID, amt money.Kobo, reason string) {
		if amt != 0 {
			entries = append(entries, ledger.Entry{AccountID: acct, Amount: ledger.NGN(amt), Reason: reason})
		}
	}
	add(interchange, reversal.Components[fee.Interchange], "chargeback.interchange_reversed")
	add(revenue, reversal.Components[fee.SchemeFee], "chargeback.scheme_fee_reversed")
	add(pool, reversal.Components[fee.BuybackPool]+reversal.CoFund, "chargeback.buyback_pool_reversed")

	if d.Provisional {
		// Already credited. The provisional advance is settled rather than the
		// cardholder being paid twice.
		entries = append(entries,
			ledger.Entry{AccountID: suspense, Amount: ledger.NGN(d.Amount), Reason: "chargeback.provisional_settled"})
	} else {
		entries = append(entries,
			ledger.Entry{AccountID: holder, Amount: ledger.NGN(d.Amount), Reason: "chargeback.refund"})
	}

	return ledger.Post(ctx, tx, ledger.Tx{
		EventType:      "dispute.chargeback",
		BusinessDate:   businessDate,
		IdempotencyKey: "dispute.chargeback|" + d.ID.String(),
		CorrelationID:  &d.ID,
		Entries:        entries,
	})
}

// Unwind claws back the equity a buyback bought against a charged-back sale.
//
// The chargeback lock exists precisely so that this is usually possible in
// shares: a buyback lot cannot be sold until the filing window has closed, so
// while a dispute is live the shares are still there. Three cases, in order of
// how much the network recovers:
//
//	shares      the lot is intact. Units go back to treasury, cash back to the
//	            fee pool, and nobody is out of pocket.
//	mixed       part of the lot is gone — possible after a split reshaped it, or
//	            a late dispute. What remains goes back; the rest is charged to
//	            the cardholder in cash at the price they received it at.
//	written_off the cardholder cannot cover the shortfall. The loss lands in a
//	            named reserve rather than quietly unbalancing something.
//
// Charging the shortfall at the ORIGINAL price, not today's, is deliberate: the
// cardholder did not choose to receive the shares and must not end up owing
// more than the network spent because the price rose.
func Unwind(ctx context.Context, tx pgx.Tx, d Dispute, businessDate string) error {
	var intentID uuid.UUID
	var instrumentID, symbol string
	var companyID uuid.UUID
	var allocated share.Units
	var price money.Kobo
	err := tx.QueryRow(ctx, `
		SELECT b.id, b.instrument_id, i.symbol, i.company_id,
		       COALESCE(b.allocated_units, 0), COALESCE(b.price_kobo, 0)
		  FROM buyback_intents b
		  JOIN instruments i ON i.id = b.instrument_id
		 WHERE b.presentment_id = $1 AND b.state = 'allocated'
		 FOR UPDATE OF b`, d.PresentmentID).
		Scan(&intentID, &instrumentID, &symbol, &companyID, &allocated, &price)
	if errors.Is(err, pgx.ErrNoRows) {
		// No equity was ever allocated — the merchant was unlisted, or the
		// intent escrowed. Nothing to claw back.
		return nil
	}
	if err != nil {
		return fmt.Errorf("dispute: load buyback intent: %w", err)
	}
	if allocated <= 0 {
		return nil
	}

	wallet, err := ledger.Resolve(ctx, tx, ledger.Cardholder(d.Cardholder, ledger.KindStockWallet, instrumentID))
	if err != nil {
		return err
	}

	// What is still there to take back.
	var held share.Units
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(units_open - units_reserved), 0)::bigint
		  FROM holding_lots WHERE account_id = $1 AND instrument_id = $2`,
		wallet, instrumentID).Scan(&held); err != nil {
		return fmt.Errorf("dispute: holdings for unwind: %w", err)
	}

	clawable := allocated
	if held < clawable {
		clawable = held
	}
	short := allocated - clawable

	cost, err := share.CostOf(clawable, price)
	if err != nil {
		return err
	}
	shortCash, err := share.CostOf(short, price)
	if err != nil {
		return err
	}

	treasury, err := ledger.Resolve(ctx, tx, ledger.Company(companyID, ledger.KindTreasury, instrumentID))
	if err != nil {
		return err
	}
	treasuryCash, err := ledger.Resolve(ctx, tx, ledger.Company(companyID, ledger.KindTreasuryCash, ledger.AssetNGN))
	if err != nil {
		return err
	}
	pool, err := ledger.Resolve(ctx, tx, ledger.Scheme(ledger.KindBuybackPool, ledger.AssetNGN))
	if err != nil {
		return err
	}

	var entries []ledger.Entry
	method := "shares"

	if clawable > 0 {
		entries = append(entries,
			ledger.Entry{AccountID: wallet, Amount: ledger.Equity(symbol, -clawable), Reason: "unwind.claw_back"},
			ledger.Entry{AccountID: treasury, Amount: ledger.Equity(symbol, clawable), Reason: "unwind.to_treasury"},
			ledger.Entry{AccountID: treasuryCash, Amount: ledger.NGN(-cost), Reason: "unwind.refund"},
			ledger.Entry{AccountID: pool, Amount: ledger.NGN(cost), Reason: "unwind.pool_restored"})
		if err := consumeLots(ctx, tx, wallet, instrumentID, clawable); err != nil {
			return err
		}
	}

	var loss money.Kobo
	var recovered money.Kobo = cost
	if short > 0 {
		method = "mixed"
		if clawable == 0 {
			method = "cash"
		}
		available, err := ledger.NairaBalance(ctx, tx,
			ledger.Cardholder(d.Cardholder, ledger.KindAvailable, ledger.AssetNGN))
		if err != nil {
			return err
		}
		cash := shortCash
		if available < cash {
			cash = available
			if cash < 0 {
				cash = 0
			}
		}
		if cash > 0 {
			holderAcct, err := ledger.Resolve(ctx, tx,
				ledger.Cardholder(d.Cardholder, ledger.KindAvailable, ledger.AssetNGN))
			if err != nil {
				return err
			}
			entries = append(entries,
				ledger.Entry{AccountID: holderAcct, Amount: ledger.NGN(-cash), Reason: "unwind.cash_recovery"},
				ledger.Entry{AccountID: pool, Amount: ledger.NGN(cash), Reason: "unwind.pool_restored"})
			recovered += cash
		}
		if loss = shortCash - cash; loss > 0 {
			reserve, err := ledger.Resolve(ctx, tx,
				ledger.Scheme(ledger.KindBuybackLossReserve, ledger.AssetNGN))
			if err != nil {
				return err
			}
			entries = append(entries,
				ledger.Entry{AccountID: reserve, Amount: ledger.NGN(-loss), Reason: "unwind.shortfall"},
				ledger.Entry{AccountID: pool, Amount: ledger.NGN(loss), Reason: "unwind.pool_restored"})
			if clawable == 0 && cash == 0 {
				method = "written_off"
			}
		}
	}

	if len(entries) == 0 {
		return nil
	}

	txID, err := ledger.Post(ctx, tx, ledger.Tx{
		EventType:      "buyback.unwound",
		BusinessDate:   businessDate,
		IdempotencyKey: "unwind|" + intentID.String(),
		CorrelationID:  &intentID,
		Entries:        entries,
	})
	if err != nil {
		if errors.Is(err, ledger.ErrAlreadyPosted) {
			return nil
		}
		return err
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO buyback_unwinds
		  (intent_id, dispute_id, units_clawed, units_short, cash_recovered_kobo, loss_kobo, method, ledger_tx_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (intent_id) DO NOTHING`,
		intentID, d.ID, int64(clawable), int64(short), int64(recovered), int64(loss), method, txID); err != nil {
		return fmt.Errorf("dispute: record unwind: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE buyback_intents SET state = 'unwound' WHERE id = $1`, intentID); err != nil {
		return err
	}
	// Issuance reverses too, or the instrument's authorised headroom never
	// recovers from a chargeback and slowly strangles its own buyback.
	_, err = tx.Exec(ctx, `
		INSERT INTO cap_table_events (instrument_id, kind, units_delta, ledger_tx_id, note)
		VALUES ($1,'buyback_unwind',$2,$3,$4)`,
		instrumentID, -int64(clawable), txID, "chargeback "+d.ID.String())
	return err
}

// consumeLots takes units back out of a cardholder's lots, newest first.
//
// Newest first, not FIFO: the lot being clawed back is the one the buyback just
// created, and taking the oldest would leave the cardholder holding the new lot
// and short an older one they had actually paid for.
func consumeLots(ctx context.Context, tx pgx.Tx, wallet uuid.UUID, instrumentID string, units share.Units) error {
	rows, err := tx.Query(ctx, `
		SELECT id, units_open - units_reserved, cost_open_kobo, units_open
		  FROM holding_lots
		 WHERE account_id = $1 AND instrument_id = $2 AND units_open > units_reserved
		 ORDER BY acquired_at DESC, id DESC FOR UPDATE`, wallet, instrumentID)
	if err != nil {
		return fmt.Errorf("dispute: lots for unwind: %w", err)
	}
	type lot struct {
		id   int64
		free share.Units
		cost money.Kobo
		open share.Units
	}
	var lots []lot
	for rows.Next() {
		var l lot
		if err := rows.Scan(&l.id, &l.free, &l.cost, &l.open); err != nil {
			rows.Close()
			return err
		}
		lots = append(lots, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	remaining := units
	for _, l := range lots {
		if remaining <= 0 {
			break
		}
		take := l.free
		if take > remaining {
			take = remaining
		}
		basis := money.Kobo(int64(l.cost) * int64(take) / int64(l.open))
		if take == l.open {
			basis = l.cost
		}
		if _, err := tx.Exec(ctx, `
			UPDATE holding_lots SET units_open = units_open - $2, cost_open_kobo = cost_open_kobo - $3
			 WHERE id = $1`, l.id, int64(take), int64(basis)); err != nil {
			return fmt.Errorf("dispute: consume lot %d: %w", l.id, err)
		}
		remaining -= take
	}
	if remaining > 0 {
		return fmt.Errorf("dispute: %s short when clawing back %s", remaining, units)
	}
	return nil
}
