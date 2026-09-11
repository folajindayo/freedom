package exchange

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"freedom/api/internal/ledger"
	"freedom/api/internal/money"
	"freedom/api/internal/share"
)

// A trading session for one instrument on one day.
//
// The session is dark: nothing about the book — no indicative price, no
// imbalance — is published while orders are being collected. On a symbol with
// six orders an indicative feed mostly tells a manipulator which order moves
// the print, and a dark call auction has no spoofing vector at all because
// there is nothing to aim at. That is a whole detection problem designed out
// rather than caught.

// Session is an auction in progress.
type Session struct {
	ID           uuid.UUID
	InstrumentID string
	Symbol       string
	CompanyID    uuid.UUID
	SessionDate  string
	State        string
	Params       Params
}

// Engine runs sessions.
type Engine struct {
	Fees FeeSchedule
}

// NewEngine builds an engine on the launch fee schedule.
func NewEngine() *Engine { return &Engine{Fees: ExchangeFeesV1()} }

// Open starts (or resumes) the session for an instrument on a date.
//
// It takes the instrument's advisory lock for the length of the transaction.
// Every order entry and the uncross itself run under that lock, which is what
// makes sequence assignment a total order and stops two concurrent entries from
// taking the same priority and committing out of order.
func (e *Engine) Open(ctx context.Context, tx pgx.Tx, instrumentID, sessionDate string) (*Session, error) {
	if err := lockInstrument(ctx, tx, instrumentID); err != nil {
		return nil, err
	}

	var s Session
	s.InstrumentID = instrumentID
	s.SessionDate = sessionDate

	var ref, tick, lot, bandBps int64
	var status string
	err := tx.QueryRow(ctx, `
		SELECT symbol, company_id, reference_price_kobo, tick_kobo, lot_units,
		       static_band_bps, status
		  FROM instruments WHERE id = $1`, instrumentID).
		Scan(&s.Symbol, &s.CompanyID, &ref, &tick, &lot, &bandBps, &status)
	if err != nil {
		return nil, fmt.Errorf("exchange: load instrument %s: %w", instrumentID, err)
	}
	if status != "listed" {
		return nil, fmt.Errorf("exchange: %s is %s, not listed", instrumentID, status)
	}
	if halted, reason, err := isHalted(ctx, tx, instrumentID); err != nil {
		return nil, err
	} else if halted {
		return nil, fmt.Errorf("%w: %s (%s)", ErrHalted, instrumentID, reason)
	}
	// The market is only open on days somebody published. An unpublished date
	// fails closed rather than opening a session no participant is present for.
	if _, err := Lookup(ctx, tx, sessionDate); err != nil {
		return nil, err
	}

	s.Params = Params{
		PrevRef: money.Kobo(ref),
		Tick:    money.Kobo(tick),
		Lot:     share.Units(lot),
		BandLo:  money.Kobo(ref - ref*bandBps/10_000),
		BandHi:  money.Kobo(ref + ref*bandBps/10_000),
	}
	if s.Params.BandLo < 1 {
		s.Params.BandLo = 1
	}

	err = tx.QueryRow(ctx, `
		INSERT INTO auctions (instrument_id, session_date, state, opens_at, freezes_at,
		                      prev_reference_kobo, engine_version)
		VALUES ($1, $2::date, 'accepting', now(), now(), $3, $4)
		ON CONFLICT (instrument_id, session_date) DO UPDATE
		  SET prev_reference_kobo = EXCLUDED.prev_reference_kobo
		RETURNING id, state`, instrumentID, sessionDate, ref, EngineVersion).
		Scan(&s.ID, &s.State)
	if err != nil {
		return nil, fmt.Errorf("exchange: open session: %w", err)
	}
	return &s, nil
}

// Place enters an order into an accepting session.
func (e *Engine) Place(ctx context.Context, tx pgx.Tx, s *Session, req OrderRequest) (uuid.UUID, error) {
	if s.State != "accepting" {
		return uuid.Nil, fmt.Errorf("exchange: session is %s, not accepting orders", s.State)
	}

	// The gateway checks, before anything touches the book. A halted member is
	// stopped in one action rather than talked through a withdrawal.
	if err := checkMember(ctx, tx, req.MemberID); err != nil {
		return uuid.Nil, err
	}
	if err := checkThrottles(ctx, tx, req.MemberID, s.SessionDate); err != nil {
		return uuid.Nil, err
	}
	if err := checkClosedPeriod(ctx, tx, s.InstrumentID, req.AccountID, s.SessionDate); err != nil {
		return uuid.Nil, err
	}

	rules := Rules{
		Tick: s.Params.Tick, Lot: s.Params.Lot,
		MinPrice: s.Params.BandLo, MaxPrice: s.Params.BandHi,
	}
	o := Order{
		ID: uuid.New(), Side: req.Side, Type: req.Type,
		Limit: req.Limit, Qty: req.Qty, Notional: req.Notional,
		AccountID: req.CardholderID,
	}
	if err := rules.Validate(o); err != nil {
		return uuid.Nil, err
	}

	// Related parties may not trade their own symbol at all. The check is on
	// the trading account, which is what related_parties records — an insider
	// with three accounts must have all three listed, and listing them is the
	// compliance job that admits the listing in the first place.
	key, err := assertNotRelatedParty(ctx, tx, s.InstrumentID, req.AccountID, s.SessionDate)
	if err != nil {
		return uuid.Nil, err
	}
	o.OwnerKey = key

	// Size bounds. The floor is dust control — a fill whose proceeds are smaller
	// than the fee to process it wastes everyone's time and can leave a seller
	// net negative. The ceilings are fat-finger control: the most common large
	// error in trading is a decimal point, and it is far cheaper to reject the
	// order than to unwind the trade.
	var minNotional int64
	var maxNotional, maxUnits *int64
	if err := tx.QueryRow(ctx, `
		SELECT min_order_notional_kobo, max_order_notional_kobo, max_order_units
		  FROM instruments WHERE id = $1`, s.InstrumentID).
		Scan(&minNotional, &maxNotional, &maxUnits); err != nil {
		return uuid.Nil, err
	}
	notional := o.notionalAt(s.Params.PrevRef)
	if notional < money.Kobo(minNotional) {
		return uuid.Nil, fmt.Errorf("%w: %s is below the %s minimum",
			ErrBelowMinimum, notional, money.Kobo(minNotional))
	}
	if maxNotional != nil && notional > money.Kobo(*maxNotional) {
		return uuid.Nil, fmt.Errorf("%w: %s exceeds the %s maximum order size",
			ErrFatFinger, notional, money.Kobo(*maxNotional))
	}
	if maxUnits != nil && o.Qty > share.Units(*maxUnits) {
		return uuid.Nil, fmt.Errorf("%w: %s exceeds the %s maximum order quantity",
			ErrFatFinger, o.Qty, share.Units(*maxUnits))
	}

	// A repeated client order id is a retry, not a second order. Without this a
	// terminal that times out and resends doubles the customer's position.
	if req.ClientOrderID != "" {
		var existing uuid.UUID
		err := tx.QueryRow(ctx, `
			SELECT id FROM orders WHERE member_id = $1 AND client_order_id = $2`,
			req.MemberID, req.ClientOrderID).Scan(&existing)
		if err == nil {
			return existing, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, fmt.Errorf("exchange: duplicate check: %w", err)
		}
	}

	// Dense, per-auction priority, assigned under the instrument lock taken in
	// Open. A global sequence would make a replay on a fresh database produce
	// different numbers and therefore a different hash, so no session could be
	// audited by replaying it.
	var seq int64
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(auction_seq), 0) + 1 FROM orders WHERE auction_id = $1`, s.ID).
		Scan(&seq); err != nil {
		return uuid.Nil, err
	}
	o.Seq = seq

	if _, err := tx.Exec(ctx, `
		INSERT INTO orders (id, instrument_id, account_id, side, type, limit_kobo,
		                    qty_units, notional_kobo, venue, auction_id, auction_seq,
		                    tif, owner_key, member_id, client_account_id, state)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'auction',$9,$10,'day',$11,$12,$13,'open')`,
		o.ID, s.InstrumentID, req.AccountID, string(o.Side), o.Type,
		nullableKobo(o.Limit), nullableUnits(o.Qty), nullableKobo(o.Notional),
		s.ID, seq, nullableString(key), req.MemberID, req.ClientAccountID); err != nil {
		return uuid.Nil, fmt.Errorf("exchange: insert order: %w", err)
	}
	if req.ClientOrderID != "" {
		if _, err := tx.Exec(ctx,
			`UPDATE orders SET client_order_id = $2 WHERE id = $1`, o.ID, req.ClientOrderID); err != nil {
			return uuid.Nil, err
		}
	}
	if err := recordOrderSent(ctx, tx, req.MemberID, s.SessionDate); err != nil {
		return uuid.Nil, err
	}

	// Cover it before it joins the book.
	var reserved money.Kobo
	if o.Side == Buy {
		reserved, err = reserveBuy(ctx, tx, o, req.CardholderID, e.Fees, s.SessionDate)
	} else {
		_, err = reserveSell(ctx, tx, o, req.CardholderID, s.InstrumentID, s.Symbol, s.SessionDate)
	}
	if err != nil {
		return uuid.Nil, err
	}

	if _, err := tx.Exec(ctx, `
		UPDATE orders SET reserved_kobo = $2, reserved_units = $3 WHERE id = $1`,
		o.ID, int64(reserved), int64(o.Qty)); err != nil {
		return uuid.Nil, err
	}
	if err := recordEvent(ctx, tx, s, o, "new", ""); err != nil {
		return uuid.Nil, err
	}
	return o.ID, nil
}

// OrderRequest is an order as it arrives from a member.
type OrderRequest struct {
	MemberID        uuid.UUID
	ClientAccountID uuid.UUID
	CardholderID    uuid.UUID
	AccountID       uuid.UUID // the ledger account the order trades from
	Side            Side
	Type            string
	Limit           money.Kobo
	Qty             share.Units
	Notional        money.Kobo
	// ClientOrderID is the member's own reference. Resending one is a retry,
	// not a second order.
	ClientOrderID string
}

// RunToSettlement freezes the book, uncrosses it, and settles the result.
//
// The three steps are one transaction on purpose: a session that uncrossed but
// did not settle would have a published price and no trades behind it, and the
// buyback would then pay a price that never actually moved any shares.
func (e *Engine) RunToSettlement(ctx context.Context, tx pgx.Tx, s *Session) (Result, error) {
	book, orderBySeq, err := e.loadBook(ctx, tx, s)
	if err != nil {
		return Result{}, err
	}

	bookHash := hashBook(book, s.Params)
	if _, err := tx.Exec(ctx, `
		UPDATE auctions SET state = 'frozen', frozen_at = now(), book_hash = $2 WHERE id = $1`,
		s.ID, bookHash); err != nil {
		return Result{}, fmt.Errorf("exchange: freeze: %w", err)
	}
	s.State = "frozen"

	result, err := Cross(book, s.Params)
	if err != nil {
		return Result{}, err
	}
	resultHash := hashResult(book, s.Params, result)

	// Two gates between uncrossing and publishing. Neither adjusts the price:
	// a price the market did not produce is worse than no price at all, because
	// everything downstream will believe it.
	if result.Determined {
		breached, err := CheckBand(ctx, tx, s.InstrumentID, s.Params, result.Price)
		if err != nil {
			return Result{}, err
		}
		tripped := false
		if !breached {
			tripped, err = CheckVolatility(ctx, tx, s.InstrumentID, s.Params.PrevRef, result.Price)
			if err != nil {
				return Result{}, err
			}
		}
		if breached || tripped {
			reason := "band_breach"
			if tripped {
				reason = "circuit_breaker"
			}
			return e.abandon(ctx, tx, s, book, orderBySeq, reason)
		}
	}

	if _, err := tx.Exec(ctx, `
		UPDATE auctions
		   SET state = 'uncrossed', uncrossed_at = now(), clearing_price_kobo = $2,
		       matched_units = $3, imbalance_units = $4, imbalance_side = $5,
		       zero_volume = $6, rule = $7, result_hash = $8
		 WHERE id = $1`,
		s.ID, nullableKobo(result.Price), nullableUnits(result.Exec),
		int64(result.Imbalance), imbalanceSide(result.Imbalance),
		!result.Determined, nullableString(result.Rule), resultHash); err != nil {
		return Result{}, fmt.Errorf("exchange: record uncross: %w", err)
	}
	s.State = "uncrossed"

	if err := e.settle(ctx, tx, s, book, orderBySeq, result, resultHash); err != nil {
		return Result{}, err
	}
	return result, nil
}

// abandon discards an uncrossed price that failed a publication gate.
//
// Reservations come back, orders expire, and the session ends in `halted` —
// never `published`, so nothing downstream ever sees the price. Crucially this
// all commits: the incident and the halt that caused the abandonment must
// survive, or the breaker trips again tomorrow with no record of why.
func (e *Engine) abandon(ctx context.Context, tx pgx.Tx, s *Session, book []Order,
	index map[int64]bookOrder, reason string) (Result, error) {

	entries, err := e.releaseUnfilled(ctx, tx, s, book, index, Result{})
	if err != nil {
		return Result{}, err
	}
	if len(entries) > 0 {
		if _, err := ledger.Post(ctx, tx, ledger.Tx{
			EventType:      "auction.abandoned",
			BusinessDate:   s.SessionDate,
			IdempotencyKey: "auction|" + s.ID.String(),
			CorrelationID:  &s.ID,
			Entries:        entries,
		}); err != nil && !isAlreadyPosted(err) {
			return Result{}, err
		}
	}
	if err := markOrdersSettled(ctx, tx, s, Result{}); err != nil {
		return Result{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE auctions SET state = 'halted', clearing_price_kobo = NULL, rule = $2
		 WHERE id = $1`, s.ID, reason); err != nil {
		return Result{}, fmt.Errorf("exchange: abandon session: %w", err)
	}
	s.State = "halted"
	return Result{Abandoned: true, Reason: reason}, nil
}

func imbalanceSide(imb share.Units) *string {
	if imb == 0 {
		return nil
	}
	s := "sell"
	if imb > 0 {
		s = "buy"
	}
	return &s
}

func lockInstrument(ctx context.Context, tx pgx.Tx, instrumentID string) error {
	// Serialises every order entry and the uncross for one instrument. Row
	// locks on the book would deadlock under crossing flow and would not
	// produce a total order over arrivals.
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext('book:' || $1))`, instrumentID); err != nil {
		return fmt.Errorf("exchange: lock instrument %s: %w", instrumentID, err)
	}
	return nil
}

func isHalted(ctx context.Context, tx pgx.Tx, instrumentID string) (bool, string, error) {
	var reason string
	err := tx.QueryRow(ctx, `
		SELECT reason FROM trading_halts
		 WHERE instrument_id = $1 AND released_at IS NULL LIMIT 1`, instrumentID).Scan(&reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	return true, reason, nil
}

// notionalAt is the naira size of an order at a price, for minimum-size checks.
func (o Order) notionalAt(p money.Kobo) money.Kobo {
	if o.IsCashDenominated() {
		return o.Notional
	}
	price := o.Limit
	if price <= 0 {
		price = p
	}
	c, err := share.CostOf(o.Qty, price)
	if err != nil {
		return 0
	}
	return c
}

func recordEvent(ctx context.Context, tx pgx.Tx, s *Session, o Order, kind, reason string) error {
	payload, err := json.Marshal(map[string]any{
		"side": o.Side, "type": o.Type, "limit": int64(o.Limit),
		"qty": int64(o.Qty), "notional": int64(o.Notional), "owner_key": o.OwnerKey,
	})
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO order_events (instrument_id, auction_id, order_id, kind, reason, payload, auction_seq)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		s.InstrumentID, s.ID, o.ID, kind, nullableString(reason), payload, o.Seq)
	return err
}

// hashBook seals what was crossed. hashResult seals what came out. Together
// with engine_version they are the audit trail: anyone holding the order events
// can rebuild the book, re-run Cross, and check both.
func hashBook(book []Order, p Params) string {
	var b strings.Builder
	fmt.Fprintf(&b, "v%d\nparams %d %d %d %d %d\n",
		EngineVersion, int64(p.PrevRef), int64(p.Tick), int64(p.Lot), int64(p.BandLo), int64(p.BandHi))
	for _, o := range book {
		fmt.Fprintf(&b, "o %s %s %d %d %d %d\n",
			o.Side, o.Type, int64(o.Limit), int64(o.Qty), int64(o.Notional), o.Seq)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func hashResult(book []Order, p Params, r Result) string {
	var b strings.Builder
	b.WriteString(hashBook(book, p))
	fmt.Fprintf(&b, "\nprice %d\nexec %d\nimb %d\nrule %s\n",
		int64(r.Price), int64(r.Exec), int64(r.Imbalance), r.Rule)
	for _, f := range r.Fills {
		fmt.Fprintf(&b, "f %s %d %d\n", f.Side, f.Seq, int64(f.Units))
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func nullableKobo(v money.Kobo) *int64 {
	if v == 0 {
		return nil
	}
	n := int64(v)
	return &n
}

func nullableUnits(v share.Units) *int64 {
	if v == 0 {
		return nil
	}
	n := int64(v)
	return &n
}

func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// CancelOrder withdraws a live order and releases everything it reserved.
//
// It reports found and cancelled separately, and the distinction matters at the
// API boundary. An order that belongs to another member must read as ABSENT,
// not as present-but-refused: "you may not cancel that" confirms the order
// exists, which is information the caller has no right to. An order that is
// genuinely theirs but already filled reports found-but-not-cancelled, because
// a member who believes they cancelled a fill will act on a position they do
// not have.
func CancelOrder(ctx context.Context, tx pgx.Tx, orderID, memberID uuid.UUID) (found, cancelled bool, err error) {
	var instrumentID, state string
	var auctionID *uuid.UUID
	err = tx.QueryRow(ctx, `
		SELECT instrument_id, state, auction_id FROM orders
		 WHERE id = $1 AND member_id = $2 FOR UPDATE`, orderID, memberID).
		Scan(&instrumentID, &state, &auctionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("exchange: load order: %w", err)
	}
	if state != "open" && state != "partial" {
		return true, false, nil
	}
	if err := lockInstrument(ctx, tx, instrumentID); err != nil {
		return true, false, err
	}

	if _, err := tx.Exec(ctx, `
		UPDATE orders SET state = 'cancelled' WHERE id = $1`, orderID); err != nil {
		return true, false, fmt.Errorf("exchange: cancel order: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO order_events (instrument_id, auction_id, order_id, kind, payload)
		VALUES ($1,$2,$3,'cancel','{}'::jsonb)`, instrumentID, auctionID, orderID); err != nil {
		return true, false, err
	}
	if err := releaseOrderReservations(ctx, tx, orderID); err != nil {
		return true, false, err
	}
	return true, true, nil
}
