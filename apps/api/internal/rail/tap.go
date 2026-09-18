package rail

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"freedom/api/internal/clearing"
	"freedom/api/internal/dispute"
	"freedom/api/internal/ledger"
	"freedom/api/internal/money"
	"freedom/api/internal/scheme"
	"freedom/api/internal/share"
)

// A tap Tapp has already charged. By the time it reaches Freedom the
// cardholder has been debited and the merchant credited on Tapp's own ledger,
// so there is nothing to authorise: the tap arrives as a presentment, funded
// by the participant that already collected it.

// Tap is one charged tap as the rail delivers it.
type Tap struct {
	TapRef                string     `json:"tap_ref"`
	MerchantRef           string     `json:"merchant_ref"`
	CardholderRef         string     `json:"cardholder_ref"`
	CardholderDisplayName string     `json:"cardholder_display_name"`
	AmountKobo            money.Kobo `json:"amount_kobo"`
	ChargedAt             time.Time  `json:"charged_at"`
}

// TapResult is what the rail learns about a tap: what it cost, what it funded,
// and what — if anything yet — it bought.
type TapResult struct {
	TapRef             string      `json:"tap_ref"`
	PresentmentID      uuid.UUID   `json:"presentment_id"`
	FeeKobo            money.Kobo  `json:"fee_kobo"`
	BuybackFundingKobo money.Kobo  `json:"buyback_funding_kobo"`
	IntentID           *uuid.UUID  `json:"intent_id"`
	IntentState        string      `json:"intent_state"`
	AllocatedUnits     share.Units `json:"allocated_units"`
	PriceKobo          money.Kobo  `json:"price_kobo"`
	Symbol             *string     `json:"symbol"`
	LockUntil          *string     `json:"lock_until"`
	Reversed           bool        `json:"reversed"`
}

// IngestTap runs one tap through clearing and, when a price exists today,
// the buyback. Idempotent on tap_ref: created reports whether this call did
// the work or found it done.
func (s *Service) IngestTap(ctx context.Context, t Tap) (out TapResult, created bool, err error) {
	t.TapRef = strings.TrimSpace(t.TapRef)
	if t.TapRef == "" {
		return out, false, fmt.Errorf("%w: tap_ref is required", ErrInvalid)
	}
	if t.AmountKobo <= 0 {
		return out, false, fmt.Errorf("%w: amount_kobo must be positive", ErrInvalid)
	}
	today := scheme.BusinessDate(s.Now())

	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockRef(ctx, tx, "tap", t.TapRef); err != nil {
			return err
		}
		id, err := ensure(ctx, tx)
		if err != nil {
			return err
		}

		// Replay. The outbox will deliver a tap more than once, and the
		// second delivery is a read.
		existing, err := s.tapView(ctx, tx, t.TapRef)
		if err == nil {
			out = existing
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}

		holder, card, err := upsertCardholder(ctx, tx, id, t.CardholderRef, t.CardholderDisplayName)
		if err != nil {
			return err
		}
		m, err := upsertMerchant(ctx, tx, id, t.MerchantRef)
		if err != nil {
			return err
		}

		// The hold an authorisation would have placed, funded by the
		// participant who already collected the ticket from the cardholder.
		// Clearing then consumes the hold exactly as it would any other.
		if err := s.postTapFunded(ctx, tx, id, holder, t.TapRef, t.AmountKobo, today); err != nil {
			return err
		}

		version, err := currentSchedule(ctx, tx, today)
		if err != nil {
			return err
		}
		var presentmentID uuid.UUID
		if err := tx.QueryRow(ctx, `
			INSERT INTO presentments (merchant_id, card_id, kind, amount_kobo, arn, fee_schedule_version)
			VALUES ($1, $2, 'first', $3, $4, $5) RETURNING id`,
			m.ID, card, int64(t.AmountKobo), arnPrefix+t.TapRef, version).Scan(&presentmentID); err != nil {
			return fmt.Errorf("rail: presentment: %w", err)
		}

		if err := s.clear(ctx, tx, today); err != nil {
			return err
		}

		// The acquirer already paid the merchant. What clearing credited to
		// the merchant's receivable is Tapp's to collect back.
		bd, err := s.breakdownFor(ctx, tx, presentmentID)
		if err != nil {
			return err
		}
		if err := s.postMerchantPaid(ctx, tx, id, m.ID, t.TapRef, bd.Ticket-bd.MerchantPays(), today); err != nil {
			return err
		}

		if err := s.allocateIfPriced(ctx, tx, presentmentID, today); err != nil {
			return err
		}

		out, err = s.tapView(ctx, tx, t.TapRef)
		created = true
		return err
	})
	return out, created, err
}

// clear runs today's batch over every unbatched presentment, which is what a
// clearing run is. Nothing narrower would match production.
func (s *Service) clear(ctx context.Context, tx pgx.Tx, today string) error {
	batch, err := clearing.OpenBatch(ctx, tx, today)
	if err != nil {
		return err
	}
	c := &clearing.Clearer{Schedules: s.Schedules}
	_, err = c.Run(ctx, tx, batch)
	return err
}

// allocateIfPriced runs the buyback for the tap's instrument unless today's
// session exists and has not yet published — then the intent stays pending
// for the close, because RunSession would escrow it for want of a price. A
// date with no session at all (a weekend, the evening after cutover) is
// priced by the engine at the carried reference, so the buyback runs now.
func (s *Service) allocateIfPriced(ctx context.Context, tx pgx.Tx, presentmentID uuid.UUID, today string) error {
	var instrumentID *string
	err := tx.QueryRow(ctx,
		`SELECT instrument_id FROM buyback_intents WHERE presentment_id = $1`, presentmentID).Scan(&instrumentID)
	if errors.Is(err, pgx.ErrNoRows) || instrumentID == nil {
		return nil // no intent (dust ticket), or an unlisted merchant
	}
	if err != nil {
		return fmt.Errorf("rail: load intent: %w", err)
	}
	var forming bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM auctions
		                WHERE instrument_id = $1 AND session_date = $2::date
		                  AND state NOT IN ('published','halted','cancelled'))`,
		*instrumentID, today).Scan(&forming); err != nil {
		return fmt.Errorf("rail: session check: %w", err)
	}
	if forming {
		return nil
	}
	_, err = s.Buyback.RunSession(ctx, tx, *instrumentID, today)
	return err
}

func (s *Service) postTapFunded(ctx context.Context, tx pgx.Tx, id identity, holder uuid.UUID,
	tapRef string, ticket money.Kobo, today string) error {

	settlement, err := ledger.Resolve(ctx, tx, ledger.Participant(id.Participant, ledger.KindSettlement, ledger.AssetNGN))
	if err != nil {
		return err
	}
	hold, err := ledger.Resolve(ctx, tx, ledger.Cardholder(holder, ledger.KindHold, ledger.AssetNGN))
	if err != nil {
		return err
	}
	_, err = ledger.Post(ctx, tx, ledger.Tx{
		EventType:      "rail.tap_funded",
		BusinessDate:   today,
		IdempotencyKey: "rail|tap|" + tapRef,
		Entries: []ledger.Entry{
			{AccountID: settlement, Amount: ledger.NGN(-ticket), Reason: "rail.tap_funded"},
			{AccountID: hold, Amount: ledger.NGN(ticket), Reason: "rail.hold"},
		},
	})
	if err != nil && !errors.Is(err, ledger.ErrAlreadyPosted) {
		return err
	}
	return nil
}

func (s *Service) postMerchantPaid(ctx context.Context, tx pgx.Tx, id identity, merchantID uuid.UUID,
	tapRef string, net money.Kobo, today string) error {

	if net <= 0 {
		return nil // the fee consumed the whole ticket; nothing was owed
	}
	settlement, err := ledger.Resolve(ctx, tx, ledger.Participant(id.Participant, ledger.KindSettlement, ledger.AssetNGN))
	if err != nil {
		return err
	}
	recv, err := ledger.Resolve(ctx, tx, ledger.Merchant(merchantID, ledger.KindMerchantReceivable, ledger.AssetNGN))
	if err != nil {
		return err
	}
	_, err = ledger.Post(ctx, tx, ledger.Tx{
		EventType:      "rail.merchant_paid",
		BusinessDate:   today,
		IdempotencyKey: "rail|paid|" + tapRef,
		Entries: []ledger.Entry{
			{AccountID: recv, Amount: ledger.NGN(-net), Reason: "rail.merchant_paid"},
			{AccountID: settlement, Amount: ledger.NGN(net), Reason: "rail.acquirer_reimbursed"},
		},
	})
	if err != nil && !errors.Is(err, ledger.ErrAlreadyPosted) {
		return err
	}
	return nil
}

// tapView assembles the result for a tap from what the database holds, so a
// fresh ingest and a replay answer identically.
func (s *Service) tapView(ctx context.Context, q ledger.Querier, tapRef string) (TapResult, error) {
	out := TapResult{TapRef: tapRef}
	var intentID *uuid.UUID
	var state *string
	var units, price *int64
	err := q.QueryRow(ctx, `
		SELECT p.id, bi.id, bi.state, bi.allocated_units, bi.price_kobo, i.symbol,
		       EXISTS (SELECT 1 FROM presentments r WHERE r.arn = $2)
		  FROM presentments p
		  LEFT JOIN buyback_intents bi ON bi.presentment_id = p.id
		  LEFT JOIN instruments i ON i.id = bi.instrument_id
		 WHERE p.arn = $1`, arnPrefix+tapRef, reversalPrefix+tapRef).
		Scan(&out.PresentmentID, &intentID, &state, &units, &price, &out.Symbol, &out.Reversed)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, fmt.Errorf("%w: tap %q", ErrNotFound, tapRef)
	}
	if err != nil {
		return out, fmt.Errorf("rail: load tap: %w", err)
	}

	bd, err := s.breakdownFor(ctx, q, out.PresentmentID)
	if err != nil {
		return out, err
	}
	out.FeeKobo = bd.MSC
	out.BuybackFundingKobo = bd.BuybackFunding

	if intentID == nil {
		// A ticket too small to carry any buyback funding. Nothing is owed and
		// nothing is pending; "escrowed" would promise something that never
		// existed.
		out.IntentState = "none"
		return out, nil
	}
	out.IntentID = intentID
	out.IntentState = *state
	if units != nil {
		out.AllocatedUnits = share.Units(*units)
	}
	if price != nil {
		out.PriceKobo = money.Kobo(*price)
	}
	if *state == "allocated" {
		// The lot the allocation opened carries the lock.
		var until string
		err := q.QueryRow(ctx, `
			SELECT l.transferable_from::text
			  FROM holding_lots l JOIN ledger_tx t ON t.id = l.ledger_tx_id
			 WHERE t.idempotency_key = 'buyback|' || $1::text
			 ORDER BY l.id LIMIT 1`, intentID.String()).Scan(&until)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return out, fmt.Errorf("rail: lot lock: %w", err)
		}
		if err == nil {
			out.LockUntil = &until
		}
	}
	return out, nil
}

// ReverseResult is what a reversal reports back.
type ReverseResult struct {
	TapRef       string      `json:"tap_ref"`
	State        string      `json:"state"`
	UnwoundUnits share.Units `json:"unwound_units"`
}

// ReverseTap undoes a tap: a full_reversal presentment clears with the negated
// split, the rail postings are mirrored, and any equity the tap bought comes
// back through the same unwind a chargeback uses.
//
// Idempotent on tap_ref, and the reversal carries no reason of its own on the
// presentment because the presentment kinds are the scheme's vocabulary; the
// rail's reason is logged with the unwind note.
func (s *Service) ReverseTap(ctx context.Context, tapRef, reason string) (ReverseResult, error) {
	tapRef = strings.TrimSpace(tapRef)
	out := ReverseResult{TapRef: tapRef, State: "reversed"}
	if tapRef == "" {
		return out, fmt.Errorf("%w: tap_ref is required", ErrInvalid)
	}
	today := scheme.BusinessDate(s.Now())

	return out, s.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockRef(ctx, tx, "tap", tapRef); err != nil {
			return err
		}
		id, err := ensure(ctx, tx)
		if err != nil {
			return err
		}

		var original, merchantID, card, holder uuid.UUID
		var amount, version int64
		err = tx.QueryRow(ctx, `
			SELECT p.id, p.merchant_id, p.card_id, c.cardholder_id, p.amount_kobo, p.fee_schedule_version
			  FROM presentments p JOIN cards c ON c.id = p.card_id
			 WHERE p.arn = $1 AND p.kind = 'first'`, arnPrefix+tapRef).
			Scan(&original, &merchantID, &card, &holder, &amount, &version)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: tap %q", ErrNotFound, tapRef)
		}
		if err != nil {
			return fmt.Errorf("rail: load tap: %w", err)
		}

		var reversal uuid.UUID
		err = tx.QueryRow(ctx, `SELECT id FROM presentments WHERE arn = $1`, reversalPrefix+tapRef).Scan(&reversal)
		switch {
		case err == nil:
			// Already reversed; report what the unwind did.
			return unwoundUnits(ctx, tx, original, &out.UnwoundUnits)
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("rail: reversal check: %w", err)
		}

		if err := tx.QueryRow(ctx, `
			INSERT INTO presentments (merchant_id, card_id, kind, amount_kobo, arn, fee_schedule_version)
			VALUES ($1, $2, 'full_reversal', $3, $4, $5) RETURNING id`,
			merchantID, card, -amount, reversalPrefix+tapRef, version).Scan(&reversal); err != nil {
			return fmt.Errorf("rail: reversal presentment: %w", err)
		}

		// Clearing negates the split: the hold is restored, the merchant's
		// receivable and every fee account are returned to where they were.
		if err := s.clear(ctx, tx, today); err != nil {
			return err
		}

		// The rail postings mirror the ingest ones so the participant's
		// position and the cardholder's hold both return to zero.
		bd, err := s.breakdownFor(ctx, tx, original)
		if err != nil {
			return err
		}
		if err := s.postTapReversed(ctx, tx, id, holder, merchantID, tapRef, bd.Ticket, bd.Ticket-bd.MerchantPays(), today); err != nil {
			return err
		}

		// The equity. Allocated intents unwind through the chargeback path;
		// an intent that never allocated is closed so the close does not buy
		// shares for a sale that did not happen.
		if err := dispute.UnwindReversal(ctx, tx, original, reversal, holder, today); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE buyback_intents SET state = 'unwound'
			 WHERE presentment_id = $1 AND state IN ('pending','escrowed','deferred_dust','batched')`,
			original); err != nil {
			return fmt.Errorf("rail: close intent: %w", err)
		}
		_ = reason // recorded by the rail; Freedom's record is the presentment kind
		return unwoundUnits(ctx, tx, original, &out.UnwoundUnits)
	})
}

func (s *Service) postTapReversed(ctx context.Context, tx pgx.Tx, id identity, holder, merchantID uuid.UUID,
	tapRef string, ticket, net money.Kobo, today string) error {

	settlement, err := ledger.Resolve(ctx, tx, ledger.Participant(id.Participant, ledger.KindSettlement, ledger.AssetNGN))
	if err != nil {
		return err
	}
	hold, err := ledger.Resolve(ctx, tx, ledger.Cardholder(holder, ledger.KindHold, ledger.AssetNGN))
	if err != nil {
		return err
	}
	_, err = ledger.Post(ctx, tx, ledger.Tx{
		EventType:      "rail.tap_reversed",
		BusinessDate:   today,
		IdempotencyKey: "rail|rev|" + tapRef,
		Entries: []ledger.Entry{
			{AccountID: hold, Amount: ledger.NGN(-ticket), Reason: "rail.hold_returned"},
			{AccountID: settlement, Amount: ledger.NGN(ticket), Reason: "rail.tap_reversed"},
		},
	})
	if err != nil && !errors.Is(err, ledger.ErrAlreadyPosted) {
		return err
	}
	if net <= 0 {
		return nil
	}
	recv, err := ledger.Resolve(ctx, tx, ledger.Merchant(merchantID, ledger.KindMerchantReceivable, ledger.AssetNGN))
	if err != nil {
		return err
	}
	_, err = ledger.Post(ctx, tx, ledger.Tx{
		EventType:      "rail.merchant_repaid",
		BusinessDate:   today,
		IdempotencyKey: "rail|repaid|" + tapRef,
		Entries: []ledger.Entry{
			{AccountID: recv, Amount: ledger.NGN(net), Reason: "rail.merchant_repaid"},
			{AccountID: settlement, Amount: ledger.NGN(-net), Reason: "rail.acquirer_charged"},
		},
	})
	if err != nil && !errors.Is(err, ledger.ErrAlreadyPosted) {
		return err
	}
	return nil
}

func unwoundUnits(ctx context.Context, q ledger.Querier, presentmentID uuid.UUID, dest *share.Units) error {
	err := q.QueryRow(ctx, `
		SELECT COALESCE(u.units_clawed, 0)
		  FROM buyback_intents bi LEFT JOIN buyback_unwinds u ON u.intent_id = bi.id
		 WHERE bi.presentment_id = $1`, presentmentID).Scan(dest)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}
