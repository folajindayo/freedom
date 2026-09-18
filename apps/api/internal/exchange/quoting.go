package exchange

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"freedom/api/internal/ledger"
	"freedom/api/internal/money"
	"freedom/api/internal/share"
)

// The house quoting engine.
//
// marketmaker.go gives a designated market maker an obligation and measures
// it. This is what meets it: once a session, for every provider, compute a
// two-sided quote and put it into the book as two ordinary limit orders from
// the provider's own client account. It has no privilege the book does not
// give everyone — the orders reserve cash and shares like any other, and the
// uncross does not know they are a market maker's.
//
// # Centre, skew, spread
//
// The centre is the exchange's own fair value per share (LISTING-RULES §2.4)
// when the company has one on record, else the reference. Fair value is the
// anchor because it is the one number nobody in the market chose: the
// reference is where the last session ended, which on a thin book is where
// the last participant left it.
//
// Inventory steers the quote. The provider is placed a block at listing and
// told to hold about that much: below target (the public has been buying) the
// quote shifts up, above it (the public has been selling) it shifts down, by
// at most MaxSkewBps. That is the whole of the strategy, and it is deliberately
// a formula rather than a judgement so the record can say why a quote was
// where it was.
//
// # It never crosses itself
//
// The bid is always at least one tick under the ask, and both are clamped
// into the session's band. A quote that cannot fit two sides inside the band
// — a fair value far from the reference — quotes the side that fits and says
// so in its note, and the band moves one step towards fair value per session,
// which is exactly what the band is for.

// ErrProviderInactive is returned when the provider is not currently obliged
// to quote: suspended, terminated, or outside its effective dates.
var ErrProviderInactive = errors.New("exchange: provider is not active for this session")

// ErrSessionNotAccepting is returned when the session has already frozen.
var ErrSessionNotAccepting = errors.New("exchange: session is no longer accepting orders")

// QuoteModel is how the engine prices a quote.
type QuoteModel struct {
	// TargetSpreadBps is the spread the engine aims for, capped by each
	// provider's obligation.
	TargetSpreadBps int64
	// MaxSkewBps is how far inventory may push the quote off centre.
	MaxSkewBps int64
}

// DefaultQuoteModel is the launch model: 3% wide, steered by up to 5%.
func DefaultQuoteModel() QuoteModel { return QuoteModel{TargetSpreadBps: 300, MaxSkewBps: 500} }

// Quote is what the engine put into one session for one provider.
type Quote struct {
	ProviderID   uuid.UUID
	InstrumentID string
	Symbol       string
	SessionDate  string

	Centre       money.Kobo
	CentreSource string // "fair_value" or "reference"
	SkewBps      int64
	SpreadBps    int64
	Held, Target share.Units

	Bid, Ask               money.Kobo
	BidUnits, AskUnits     share.Units
	BidOrderID, AskOrderID uuid.UUID
	// Note says what the engine could not do and why: no inventory, no
	// capital, one side outside the band.
	Note string
}

// TwoSided reports whether both sides were placed.
func (q Quote) TwoSided() bool { return q.BidOrderID != uuid.Nil && q.AskOrderID != uuid.Nil }

// QuotableProviders lists the providers obliged to quote on a date whose
// instrument is listed and not halted.
func QuotableProviders(ctx context.Context, q ledger.Querier, sessionDate string) ([]uuid.UUID, error) {
	rows, err := q.Query(ctx, `
		SELECT lp.id FROM liquidity_providers lp JOIN instruments i ON i.id = lp.instrument_id
		 WHERE lp.state IN ('active','warned') AND lp.effective @> $1::date AND i.status = 'listed'
		   AND NOT EXISTS (SELECT 1 FROM trading_halts h WHERE h.instrument_id = i.id AND h.released_at IS NULL)
		 ORDER BY i.symbol, lp.created_at`, sessionDate)
	if err != nil {
		return nil, fmt.Errorf("exchange: quotable providers: %w", err)
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// QuoteAll quotes every quotable provider for a date, in one transaction.
func (e *Engine) QuoteAll(ctx context.Context, tx pgx.Tx, sessionDate string, model QuoteModel) ([]Quote, error) {
	ids, err := QuotableProviders(ctx, tx, sessionDate)
	if err != nil {
		return nil, err
	}
	var out []Quote
	for _, id := range ids {
		q, err := e.QuoteSession(ctx, tx, id, sessionDate, model)
		if err != nil {
			return out, err
		}
		out = append(out, q)
	}
	return out, nil
}

// QuoteSession computes and places one provider's quote for a session,
// opening the session if nobody has.
//
// Idempotent: the orders carry a client order id of "mm|<symbol>|<date>|<side>",
// so a re-run finds the orders already in the book and records them rather
// than doubling the quote. A side that could not be placed the first time
// (no inventory yet) is placed on the re-run if it can be now.
func (e *Engine) QuoteSession(ctx context.Context, tx pgx.Tx, providerID uuid.UUID, sessionDate string, model QuoteModel) (Quote, error) {
	if model.TargetSpreadBps <= 0 {
		model = DefaultQuoteModel()
	}
	q := Quote{ProviderID: providerID, SessionDate: sessionDate}

	var memberID uuid.UUID
	var minQuote money.Kobo
	var maxSpread int64
	var state string
	var effective bool
	err := tx.QueryRow(ctx, `
		SELECT instrument_id, member_id, min_quote_kobo, max_spread_bps, target_units, state,
		       effective @> $2::date
		  FROM liquidity_providers WHERE id = $1`, providerID, sessionDate).
		Scan(&q.InstrumentID, &memberID, &minQuote, &maxSpread, &q.Target, &state, &effective)
	if errors.Is(err, pgx.ErrNoRows) {
		return q, fmt.Errorf("exchange: no provider %s", providerID)
	}
	if err != nil {
		return q, fmt.Errorf("exchange: load provider: %w", err)
	}
	if !effective || (state != "active" && state != "warned") {
		return q, fmt.Errorf("%w: %s is %s", ErrProviderInactive, providerID, state)
	}

	clientAccount, cardholder, err := providerAccount(ctx, tx, memberID)
	if err != nil {
		return q, err
	}

	s, err := e.Open(ctx, tx, q.InstrumentID, sessionDate)
	if err != nil {
		return q, err
	}
	if s.State != "accepting" {
		return q, fmt.Errorf("%w: %s on %s is %s", ErrSessionNotAccepting, s.Symbol, sessionDate, s.State)
	}
	q.Symbol = s.Symbol

	// Centre.
	q.Centre, q.CentreSource, err = quoteCentre(ctx, tx, s)
	if err != nil {
		return q, err
	}

	// Inventory and skew.
	q.Held, err = heldUnits(ctx, tx, cardholder, q.InstrumentID)
	if err != nil {
		return q, err
	}
	q.SkewBps = inventorySkew(q.Held, q.Target, model.MaxSkewBps)
	q.SpreadBps = model.TargetSpreadBps
	if maxSpread < q.SpreadBps {
		q.SpreadBps = maxSpread
	}

	// Prices: centre shifted by the skew, then half the spread each way,
	// on the tick, inside the band, never crossed.
	bid := priceAt(q.Centre, 10_000+q.SkewBps-q.SpreadBps/2, s.Params.Tick)
	ask := priceAt(q.Centre, 10_000+q.SkewBps+q.SpreadBps/2, s.Params.Tick)
	bid, ask, note := fitToBand(bid, ask, s.Params)
	q.Bid, q.Ask = bid, ask
	q.addNote(note)

	// Sizes. The obligation is a naira size per side; the engine quotes at
	// least that, rounded up so integer arithmetic on the record never
	// reads a kobo short, and no more than the provider can cover.
	var minNotional int64
	var maxNotional, maxUnits *int64
	if err := tx.QueryRow(ctx, `
		SELECT min_order_notional_kobo, max_order_notional_kobo, max_order_units
		  FROM instruments WHERE id = $1`, q.InstrumentID).
		Scan(&minNotional, &maxNotional, &maxUnits); err != nil {
		return q, err
	}
	capUnits := func(units share.Units, price money.Kobo) share.Units {
		if maxUnits != nil && units > share.Units(*maxUnits) {
			units = share.Units(*maxUnits)
		}
		if maxNotional != nil {
			if u, _, _, err := share.UnitsFor(money.Kobo(*maxNotional), price); err == nil && units > u {
				units = u
			}
		}
		return units
	}

	if q.Ask > 0 {
		want := unitsCovering(minQuote, q.Ask, s.Params.Lot)
		sellable, err := Sellable(ctx, tx, cardholder, q.InstrumentID, sessionDate)
		if err != nil {
			return q, err
		}
		units := capUnits(minUnits(want, sellable), q.Ask)
		switch {
		case sellable == 0:
			q.addNote("no inventory: ask withheld")
		case units < want:
			q.addNote(fmt.Sprintf("inventory %s is below the %s obligation", sellable, want))
		}
		if cost, _ := share.CostOf(units, q.Ask); units > 0 && cost < money.Kobo(minNotional) {
			q.addNote(fmt.Sprintf("inventory worth %s is below the %s order minimum: ask withheld", cost, money.Kobo(minNotional)))
			units = 0
		}
		q.AskUnits = units
	}
	if q.Bid > 0 {
		want := unitsCovering(minQuote, q.Bid, s.Params.Lot)
		available, err := ledger.NairaBalance(ctx, tx, ledger.Cardholder(cardholder, ledger.KindAvailable, ledger.AssetNGN))
		if err != nil {
			return q, err
		}
		affordable := affordableUnits(available, q.Bid, s.Params.Lot, e.Fees)
		units := capUnits(minUnits(want, affordable), q.Bid)
		switch {
		case affordable == 0:
			q.addNote(fmt.Sprintf("no capital (%s available): bid withheld", available))
		case units < want:
			q.addNote(fmt.Sprintf("capital covers %s of the %s obligation", units, want))
		}
		if cost, _ := share.CostOf(units, q.Bid); units > 0 && cost < money.Kobo(minNotional) {
			q.addNote(fmt.Sprintf("capital worth %s is below the %s order minimum: bid withheld", cost, money.Kobo(minNotional)))
			units = 0
		}
		q.BidUnits = units
	}

	wallet, err := ledger.Resolve(ctx, tx, ledger.Cardholder(cardholder, ledger.KindStockWallet, q.InstrumentID))
	if err != nil {
		return q, err
	}
	place := func(side Side, limit money.Kobo, units share.Units) (uuid.UUID, money.Kobo, share.Units, error) {
		if limit <= 0 || units <= 0 {
			return uuid.Nil, limit, units, nil
		}
		id, err := e.Place(ctx, tx, s, OrderRequest{
			MemberID: memberID, ClientAccountID: clientAccount, CardholderID: cardholder, AccountID: wallet,
			Side: side, Type: TypeLimit, Limit: limit, Qty: units,
			ClientOrderID: fmt.Sprintf("mm|%s|%s|%s", s.Symbol, sessionDate, side),
		})
		if err != nil {
			return uuid.Nil, 0, 0, fmt.Errorf("exchange: place %s quote: %w", side, err)
		}
		// What actually stands in the book: on a re-run Place hands back the
		// order already there, and the record must describe that one.
		var l, u int64
		if err := tx.QueryRow(ctx, `SELECT limit_kobo, qty_units FROM orders WHERE id = $1`, id).Scan(&l, &u); err != nil {
			return uuid.Nil, 0, 0, err
		}
		return id, money.Kobo(l), share.Units(u), nil
	}
	if q.BidOrderID, q.Bid, q.BidUnits, err = place(Buy, q.Bid, q.BidUnits); err != nil {
		return q, err
	}
	if q.AskOrderID, q.Ask, q.AskUnits, err = place(Sell, q.Ask, q.AskUnits); err != nil {
		return q, err
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO mm_quotes (provider_id, session_date, instrument_id, centre_kobo, skew_bps, spread_bps,
		                       held_units, target_units, bid_kobo, ask_kobo, bid_units, ask_units,
		                       bid_order_id, ask_order_id, note, quoted_at)
		VALUES ($1,$2::date,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,now())
		ON CONFLICT (provider_id, session_date) DO UPDATE
		  SET centre_kobo = EXCLUDED.centre_kobo, skew_bps = EXCLUDED.skew_bps, spread_bps = EXCLUDED.spread_bps,
		      held_units = EXCLUDED.held_units, target_units = EXCLUDED.target_units,
		      bid_kobo = EXCLUDED.bid_kobo, ask_kobo = EXCLUDED.ask_kobo,
		      bid_units = EXCLUDED.bid_units, ask_units = EXCLUDED.ask_units,
		      bid_order_id = EXCLUDED.bid_order_id, ask_order_id = EXCLUDED.ask_order_id,
		      note = EXCLUDED.note, quoted_at = now()`,
		providerID, sessionDate, q.InstrumentID, int64(q.Centre), q.SkewBps, q.SpreadBps,
		int64(q.Held), int64(q.Target), nullableKobo(q.Bid), nullableKobo(q.Ask),
		int64(q.BidUnits), int64(q.AskUnits), nullableUUID(q.BidOrderID), nullableUUID(q.AskOrderID),
		nullableString(q.Note)); err != nil {
		return q, fmt.Errorf("exchange: record quote: %w", err)
	}
	return q, nil
}

func (q *Quote) addNote(s string) {
	if s == "" {
		return
	}
	if q.Note != "" {
		q.Note += "; "
	}
	q.Note += s
}

// providerAccount is the client account a member quotes from: the one
// labelled market_maker, or its only one. A member with several unlabelled
// client accounts has not said which is the house account, and the engine
// does not guess with somebody's money.
func providerAccount(ctx context.Context, tx pgx.Tx, memberID uuid.UUID) (client, cardholder uuid.UUID, err error) {
	err = tx.QueryRow(ctx, `
		SELECT id, cardholder_id FROM client_accounts
		 WHERE member_id = $1 AND cardholder_id IS NOT NULL AND status = 'active' AND label = 'market_maker'
		 ORDER BY created_at LIMIT 1`, memberID).Scan(&client, &cardholder)
	if err == nil {
		return client, cardholder, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, uuid.Nil, fmt.Errorf("exchange: provider account: %w", err)
	}
	rows, err := tx.Query(ctx, `
		SELECT id, cardholder_id FROM client_accounts
		 WHERE member_id = $1 AND cardholder_id IS NOT NULL AND status = 'active' LIMIT 2`, memberID)
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("exchange: provider account: %w", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
		if n == 1 {
			if err := rows.Scan(&client, &cardholder); err != nil {
				return uuid.Nil, uuid.Nil, err
			}
		}
	}
	switch {
	case rows.Err() != nil:
		return uuid.Nil, uuid.Nil, rows.Err()
	case n == 0:
		return uuid.Nil, uuid.Nil, fmt.Errorf("exchange: member %s has no client account to quote from", memberID)
	case n > 1:
		return uuid.Nil, uuid.Nil, fmt.Errorf("exchange: member %s has several client accounts and none labelled market_maker", memberID)
	}
	return client, cardholder, nil
}

// quoteCentre is fair value per share from the company's listing record when
// there is one, rounded down to the tick like the listing price; else the
// reference.
func quoteCentre(ctx context.Context, tx pgx.Tx, s *Session) (money.Kobo, string, error) {
	var fair, shares int64
	err := tx.QueryRow(ctx, `
		SELECT COALESCE((SELECT a.fair_value_kobo FROM listing_applications a
		                  WHERE a.company_id = i.company_id AND a.state = 'listed'
		                  ORDER BY a.created_at DESC LIMIT 1), 0),
		       i.shares_in_issue_units
		  FROM instruments i WHERE i.id = $1`, s.InstrumentID).Scan(&fair, &shares)
	if err != nil {
		return 0, "", fmt.Errorf("exchange: quote centre: %w", err)
	}
	if fair <= 0 || shares <= 0 {
		return s.Params.PrevRef, "reference", nil
	}
	n := new(big.Int).Mul(big.NewInt(fair), big.NewInt(int64(share.PerShare)))
	n.Quo(n, big.NewInt(shares))
	tick := big.NewInt(int64(s.Params.Tick))
	n.Quo(n, tick).Mul(n, tick)
	if !n.IsInt64() || n.Sign() <= 0 {
		return s.Params.PrevRef, "reference", nil
	}
	c := money.Kobo(n.Int64())
	if c < s.Params.Tick {
		c = s.Params.Tick
	}
	return c, "fair_value", nil
}

// heldUnits is the provider's whole position in the instrument: every open
// lot, locked or not, because inventory is inventory whether or not it can be
// sold today.
func heldUnits(ctx context.Context, tx pgx.Tx, cardholder uuid.UUID, instrumentID string) (share.Units, error) {
	var v int64
	err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(l.units_open), 0)
		  FROM holding_lots l JOIN accounts a ON a.id = l.account_id
		 WHERE a.owner_type = 'cardholder' AND a.owner_id = $1
		   AND a.kind = 'stock_wallet' AND l.instrument_id = $2`, cardholder, instrumentID).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("exchange: held inventory: %w", err)
	}
	return share.Units(v), nil
}

// inventorySkew is (target − held) / target × max, clamped to ±max, in basis
// points. Positive when the provider is short of its target, so the quote
// rises to buy inventory back; negative when long, so it falls to shed it.
func inventorySkew(held, target share.Units, maxBps int64) int64 {
	if target <= 0 || maxBps <= 0 {
		return 0
	}
	n := new(big.Int).Sub(big.NewInt(int64(target)), big.NewInt(int64(held)))
	n.Mul(n, big.NewInt(maxBps))
	n.Quo(n, big.NewInt(int64(target)))
	if n.Cmp(big.NewInt(maxBps)) > 0 {
		return maxBps
	}
	if n.Cmp(big.NewInt(-maxBps)) < 0 {
		return -maxBps
	}
	return n.Int64()
}

// priceAt is centre × factorBps / 10,000, rounded to the nearest tick.
func priceAt(centre money.Kobo, factorBps int64, tick money.Kobo) money.Kobo {
	if tick <= 0 {
		tick = 1
	}
	n := new(big.Int).Mul(big.NewInt(int64(centre)), big.NewInt(factorBps))
	n.Quo(n, big.NewInt(10_000))
	return roundToTick(money.Kobo(n.Int64()), tick)
}

func roundToTick(p, tick money.Kobo) money.Kobo {
	if tick <= 1 {
		return p
	}
	r := p % tick
	p -= r
	if r*2 >= tick {
		p += tick
	}
	return p
}

// fitToBand clamps a quote into the session's band and keeps the bid at
// least a tick under the ask. When only one side fits, the other is dropped
// and the note says which.
func fitToBand(bid, ask money.Kobo, p Params) (money.Kobo, money.Kobo, string) {
	clamp := func(v money.Kobo) money.Kobo {
		if v < p.BandLo {
			return p.BandLo
		}
		if v > p.BandHi {
			return p.BandHi
		}
		return v
	}
	bid, ask = clamp(bid), clamp(ask)
	if bid < ask {
		return bid, ask, ""
	}
	// Both sides landed on the same band edge. Step one inside the other.
	switch {
	case ask-p.Tick >= p.BandLo:
		return ask - p.Tick, ask, "quote clamped to the band"
	case bid+p.Tick <= p.BandHi:
		return bid, bid + p.Tick, "quote clamped to the band"
	}
	// A one-tick band. Quote the bid only.
	return bid, 0, "band too narrow for two sides: ask withheld"
}

// unitsCovering is the fewest units whose notional at price is at least the
// obligation, rounded up to the lot.
func unitsCovering(notional, price money.Kobo, lot share.Units) share.Units {
	if price <= 0 {
		return 0
	}
	n := new(big.Int).Mul(big.NewInt(int64(notional)), big.NewInt(int64(share.PerShare)))
	q, r := new(big.Int).QuoRem(n, big.NewInt(int64(price)), new(big.Int))
	if r.Sign() != 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() {
		return 0
	}
	u := share.Units(q.Int64())
	if lot > 1 {
		if rem := u % lot; rem != 0 {
			u += lot - rem
		}
	}
	return u
}

// affordableUnits is the most units a buy at price can be covered for out of
// available cash, fee included, in whole lots.
func affordableUnits(available, price money.Kobo, lot share.Units, fees FeeSchedule) share.Units {
	if available <= 0 || price <= 0 {
		return 0
	}
	// The fee on the whole balance bounds the fee on any part of it.
	principal := available - fees.MaxFeeFor(available)
	if principal <= 0 {
		return 0
	}
	u, _, _, err := share.UnitsFor(principal, price)
	if err != nil {
		return 0
	}
	if lot > 1 {
		u -= u % lot
	}
	// Rounding up the cost by a kobo can tip the total over; step down.
	for u > 0 {
		cost, err := share.CostOf(u, price)
		if err != nil {
			return 0
		}
		if cost+fees.MaxFeeFor(cost) <= available {
			break
		}
		if lot > 1 {
			u -= lot
		} else {
			u--
		}
	}
	return u
}

func nullableUUID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

// firmQuoteMid is the mid of the best two-sided quote that stood at the
// freeze of a session in which nothing crossed, or 0 when there was none.
//
// Firm means: the provider's obligation for the session was measured as met
// (two-sided, inside its spread, at least its size — see MeasureSession), and
// both orders were still live in the frozen book. A quote pulled before the
// freeze, or one that was never wide enough to count, sets no price.
func firmQuoteMid(ctx context.Context, tx pgx.Tx, s *Session, index map[int64]bookOrder) (money.Kobo, error) {
	live := make(map[uuid.UUID]bool, len(index))
	for _, bo := range index {
		live[bo.OrderID] = true
	}
	rows, err := tx.Query(ctx, `
		SELECT q.bid_kobo, q.ask_kobo, q.bid_order_id, q.ask_order_id
		  FROM mm_quotes q
		  JOIN lp_performance p ON p.provider_id = q.provider_id AND p.session_date = q.session_date
		 WHERE q.instrument_id = $1 AND q.session_date = $2::date
		   AND p.two_sided AND p.met
		   AND q.bid_order_id IS NOT NULL AND q.ask_order_id IS NOT NULL`,
		s.InstrumentID, s.SessionDate)
	if err != nil {
		return 0, fmt.Errorf("exchange: firm quotes: %w", err)
	}
	defer rows.Close()
	var bestBid, bestAsk money.Kobo
	for rows.Next() {
		var bid, ask int64
		var bidID, askID uuid.UUID
		if err := rows.Scan(&bid, &ask, &bidID, &askID); err != nil {
			return 0, err
		}
		if !live[bidID] || !live[askID] {
			continue
		}
		b, a := money.Kobo(bid), money.Kobo(ask)
		if b < s.Params.BandLo || a > s.Params.BandHi || b >= a {
			continue
		}
		if b > bestBid {
			bestBid = b
		}
		if bestAsk == 0 || a < bestAsk {
			bestAsk = a
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if bestBid == 0 || bestAsk == 0 {
		return 0, nil
	}
	return roundToTick((bestBid+bestAsk)/2, s.Params.Tick), nil
}
