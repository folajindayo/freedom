// Package clearing turns a day of taps into money that has actually moved.
//
// Authorisation only reserves value. Clearing is where the cardholder is
// debited for real, the merchant becomes owed, the fee is split among the
// participants, and — because the buyback is funded out of that split — where
// the promise of equity becomes a concrete intent to buy it.
//
// # Why the buyback fires here and not at authorisation
//
// An authorisation can be reversed, and a reversed tap must not have bought
// anyone shares. Waiting until clearing costs the cardholder a day and saves
// the network from unwinding equity purchases against transactions that never
// happened.
//
// # Re-runnability
//
// A batch job of this size will die halfway at some point. Every write here is
// keyed: the ledger by idempotency key, the buyback intent by a unique
// constraint on its presentment. Re-running a half-finished batch completes it
// rather than double-posting it.
package clearing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"freedom/api/internal/fee"
	"freedom/api/internal/ledger"
	"freedom/api/internal/money"
)

// Batch is a clearing cycle for one business date.
type Batch struct {
	ID           uuid.UUID
	BusinessDate string
	Cycle        int16
	State        string
}

// Result summarises what a run did.
type Result struct {
	BatchID        uuid.UUID
	Presentments   int
	Cleared        money.Kobo
	Fees           money.Kobo
	BuybackFunding money.Kobo
	Intents        int
	Escrowed       int
}

// Schedules resolves a pinned fee schedule version. Clearing must price with
// the version stamped on the transaction, never with whichever is live now.
type Schedules interface {
	Version(v int) (fee.Schedule, error)
}

// StaticSchedules serves a fixed set of versions.
type StaticSchedules map[int]fee.Schedule

func (s StaticSchedules) Version(v int) (fee.Schedule, error) {
	sc, ok := s[v]
	if !ok {
		return fee.Schedule{}, fmt.Errorf("clearing: fee schedule v%d is not published", v)
	}
	return sc, nil
}

// Clearer runs batches.
type Clearer struct {
	Schedules Schedules
}

// OpenBatch returns the open batch for a business date, creating it if needed.
func OpenBatch(ctx context.Context, tx pgx.Tx, businessDate string) (Batch, error) {
	var b Batch
	err := tx.QueryRow(ctx, `
		INSERT INTO clearing_batches (business_date, cutoff_at, state)
		VALUES ($1::date, now(), 'open')
		ON CONFLICT (business_date, cycle) DO UPDATE SET business_date = EXCLUDED.business_date
		RETURNING id, business_date::text, cycle, state`, businessDate).
		Scan(&b.ID, &b.BusinessDate, &b.Cycle, &b.State)
	if err != nil {
		return Batch{}, fmt.Errorf("clearing: open batch: %w", err)
	}
	return b, nil
}

type presentment struct {
	ID              uuid.UUID
	AuthorizationID *uuid.UUID
	MerchantID      uuid.UUID
	CardID          uuid.UUID
	CardholderID    uuid.UUID
	Kind            string
	Amount          money.Kobo
	ScheduleVersion int
	CoFundBps       int64
	AcquirerID      uuid.UUID
	IssuerID        uuid.UUID
	CompanyID       *uuid.UUID
	InstrumentID    *string
}

// Run clears every unbatched presentment into the given batch.
func (c *Clearer) Run(ctx context.Context, tx pgx.Tx, b Batch) (Result, error) {
	res := Result{BatchID: b.ID}

	rows, err := tx.Query(ctx, `
		SELECT p.id, p.authorization_id, p.merchant_id, p.card_id, cd.cardholder_id,
		       p.kind, p.amount_kobo, p.fee_schedule_version, m.cofund_bps,
		       m.acquirer_id, cd.issuer_id, m.company_id, i.id
		  FROM presentments p
		  JOIN cards cd ON cd.id = p.card_id
		  JOIN cardholders c ON c.id = cd.cardholder_id
		  JOIN merchants m ON m.id = p.merchant_id
		  LEFT JOIN instruments i ON i.company_id = m.company_id AND i.status = 'listed'
		 WHERE p.clearing_batch_id IS NULL
		 ORDER BY p.received_at, p.id`)
	if err != nil {
		return res, fmt.Errorf("clearing: select presentments: %w", err)
	}

	var batch []presentment
	for rows.Next() {
		var p presentment
		if err := rows.Scan(&p.ID, &p.AuthorizationID, &p.MerchantID, &p.CardID, &p.CardholderID,
			&p.Kind, &p.Amount, &p.ScheduleVersion, &p.CoFundBps,
			&p.AcquirerID, &p.IssuerID, &p.CompanyID, &p.InstrumentID); err != nil {
			rows.Close()
			return res, err
		}
		batch = append(batch, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}

	for _, p := range batch {
		n, err := c.clearOne(ctx, tx, b, p)
		if err != nil {
			return res, fmt.Errorf("clearing: presentment %s: %w", p.ID, err)
		}
		res.Presentments++
		res.Cleared += p.Amount
		res.Fees += n.fees
		res.BuybackFunding += n.buyback
		if n.intent {
			res.Intents++
		}
		if n.escrowed {
			res.Escrowed++
		}
	}
	return res, nil
}

type oneResult struct {
	fees     money.Kobo
	buyback  money.Kobo
	intent   bool
	escrowed bool
}

func (c *Clearer) clearOne(ctx context.Context, tx pgx.Tx, b Batch, p presentment) (oneResult, error) {
	var out oneResult

	sched, err := c.Schedules.Version(p.ScheduleVersion)
	if err != nil {
		return out, err
	}
	// Reversals and refunds carry a negative amount and are priced by negating
	// the forward split, so that they return every account to its prior balance.
	forward, err := fee.Compute(sched, fee.Input{Ticket: p.Amount.Abs(), CoFundBps: p.CoFundBps})
	if err != nil {
		return out, err
	}
	bd := forward
	if p.Amount < 0 {
		bd = forward.Negate()
	}

	entries, err := buildEntries(ctx, tx, p, bd)
	if err != nil {
		return out, err
	}

	_, err = ledger.Post(ctx, tx, ledger.Tx{
		EventType:      "clearing." + p.Kind,
		BusinessDate:   b.BusinessDate,
		IdempotencyKey: "clearing|" + p.ID.String(),
		CorrelationID:  &p.ID,
		Entries:        entries,
	})
	if err != nil && !errors.Is(err, ledger.ErrAlreadyPosted) {
		return out, err
	}

	if _, err := tx.Exec(ctx, `
		UPDATE presentments SET clearing_batch_id = $2, business_date = $3::date
		 WHERE id = $1 AND clearing_batch_id IS NULL`,
		p.ID, b.ID, b.BusinessDate); err != nil {
		return out, fmt.Errorf("clearing: mark presentment: %w", err)
	}

	// Close out the authorisation this presentment settles.
	if p.AuthorizationID != nil {
		if _, err := tx.Exec(ctx, `
			UPDATE authorizations
			   SET outstanding_kobo = GREATEST(outstanding_kobo - $2, 0)
			 WHERE id = $1`, *p.AuthorizationID, int64(p.Amount.Abs())); err != nil {
			return out, fmt.Errorf("clearing: close authorization: %w", err)
		}
	}

	out.fees = bd.MSC

	// Only a forward sale funds a buyback. A refund's negative funding is
	// handled by the unwind path, which has to decide what to do about shares
	// that may already have been sold — not by writing a negative intent.
	if p.Kind == "first" && bd.BuybackFunding > 0 {
		created, escrowed, err := recordIntent(ctx, tx, b, p, bd)
		if err != nil {
			return out, err
		}
		out.buyback = bd.BuybackFunding
		out.intent = created
		out.escrowed = escrowed
	}
	return out, nil
}

// buildEntries turns a fee breakdown into balanced ledger legs.
//
// The shape is the same for a sale and a reversal because every amount is
// signed: reversing simply posts the negatives.
func buildEntries(ctx context.Context, tx pgx.Tx, p presentment, bd fee.Breakdown) ([]ledger.Entry, error) {
	resolve := func(ref ledger.AccountRef) (uuid.UUID, error) { return ledger.Resolve(ctx, tx, ref) }

	hold, err := resolve(ledger.Cardholder(p.CardholderID, ledger.KindHold, ledger.AssetNGN))
	if err != nil {
		return nil, err
	}
	recv, err := resolve(ledger.Merchant(p.MerchantID, ledger.KindMerchantReceivable, ledger.AssetNGN))
	if err != nil {
		return nil, err
	}
	interchange, err := resolve(ledger.Participant(p.IssuerID, ledger.KindInterchangeIncome, ledger.AssetNGN))
	if err != nil {
		return nil, err
	}
	revenue, err := resolve(ledger.Scheme(ledger.KindRevenue, ledger.AssetNGN))
	if err != nil {
		return nil, err
	}
	pool, err := resolve(ledger.Scheme(ledger.KindBuybackPool, ledger.AssetNGN))
	if err != nil {
		return nil, err
	}

	ticket := bd.Ticket
	entries := []ledger.Entry{
		// The cardholder's hold is consumed.
		{AccountID: hold, Amount: ledger.NGN(-ticket), Reason: "clearing.hold_released"},
		// The merchant is owed the sale less everything they pay.
		{AccountID: recv, Amount: ledger.NGN(ticket - bd.MerchantPays()), Reason: "clearing.merchant_net"},
	}

	add := func(acct uuid.UUID, amt money.Kobo, reason string) {
		if amt != 0 {
			entries = append(entries, ledger.Entry{AccountID: acct, Amount: ledger.NGN(amt), Reason: reason})
		}
	}
	add(interchange, bd.Components[fee.Interchange], "clearing.interchange")
	add(revenue, bd.Components[fee.SchemeFee], "clearing.scheme_fee")
	// The buyback pool receives its share of the MSC plus the merchant's
	// co-funding, which is additional to the MSC and passes through untouched.
	add(pool, bd.Components[fee.BuybackPool]+bd.CoFund, "clearing.buyback_pool")

	return entries, nil
}

// recordIntent books the promise of equity.
//
// A merchant who has not listed is not an error: the funding accrues against
// them and the merchant portal shows them how much stock their own customers
// are already waiting to own. That is the listings pipeline.
func recordIntent(ctx context.Context, tx pgx.Tx, b Batch, p presentment, bd fee.Breakdown) (created, escrowed bool, err error) {
	breakdown, err := json.Marshal(map[string]int64{
		"scheme":   int64(bd.Components[fee.BuybackPool]),
		"merchant": int64(bd.CoFund),
		"promo":    0,
	})
	if err != nil {
		return false, false, err
	}

	state := "pending"
	if p.InstrumentID == nil {
		state = "escrowed"
	}

	ct, err := tx.Exec(ctx, `
		INSERT INTO buyback_intents
		  (presentment_id, clearing_batch_id, merchant_id, cardholder_id, instrument_id,
		   funding_kobo, funding_breakdown, state)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (presentment_id) DO NOTHING`,
		p.ID, b.ID, p.MerchantID, p.CardholderID, p.InstrumentID,
		int64(bd.BuybackFunding), breakdown, state)
	if err != nil {
		return false, false, fmt.Errorf("clearing: record buyback intent: %w", err)
	}
	// A zero row count means a re-run found the intent already there, which is
	// the correct outcome, not a failure.
	return ct.RowsAffected() > 0, state == "escrowed", nil
}

// Finalise closes a batch. After this point the batch is immutable and any
// adjustment is a new entry in a later cycle — settlement finality is what lets
// a participant reconcile a statement that will not change under them.
func Finalise(ctx context.Context, tx pgx.Tx, b Batch) error {
	ct, err := tx.Exec(ctx, `
		UPDATE clearing_batches SET state = 'final', finalised_at = now()
		 WHERE id = $1 AND state IN ('open','cutoff','computing')`, b.ID)
	if err != nil {
		return fmt.Errorf("clearing: finalise: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("clearing: batch %s is already final", b.ID)
	}
	return nil
}
