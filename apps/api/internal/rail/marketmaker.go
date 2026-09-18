package rail

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"freedom/api/internal/exchange"
	"freedom/api/internal/institution"
	"freedom/api/internal/ledger"
	"freedom/api/internal/money"
	"freedom/api/internal/scheme"
	"freedom/api/internal/share"
)

// The house market maker's three needs, in the order it needs them: capital,
// inventory, and a quote every session. Capital comes from the scheme's
// float. Inventory comes from a placement — a block sold out of the
// company's treasury at the listing price, or at the reference later. The
// quote is the exchange's quoting engine, run once a session by the same
// scheduler that runs the close.

// Placement is a block of treasury shares sold to the market maker.
type Placement struct {
	Symbol    string      `json:"symbol"`
	Units     share.Units `json:"units"`
	PriceKobo money.Kobo  `json:"price_kobo"`
	CostKobo  money.Kobo  `json:"cost_kobo"`
	LedgerTx  uuid.UUID   `json:"ledger_tx_id"`
	// Held is the market maker's inventory after the placement.
	Held share.Units `json:"held_units"`
}

// PlaceWithMarketMaker sells units from the company's treasury to the house
// market maker at price, as one balanced ledger transaction: the market
// maker's cash to the company's treasury cash, the treasury's shares to the
// market maker's wallet. The lot is dated today and transferable at once —
// it is not tap-earned, so the chargeback lock has nothing to protect. The
// cap table records the transfer and a listing particulars notice publishes
// it, so the block's existence is public before the first quote.
//
// Refused when the treasury does not hold the block or the market maker
// cannot pay for it. `by` names who decided; `reason` says why.
func (s *Service) PlaceWithMarketMaker(ctx context.Context, tx pgx.Tx, instrumentID string,
	units share.Units, price money.Kobo, today, by, reason string) (Placement, error) {

	var out Placement
	if units <= 0 {
		return out, fmt.Errorf("%w: placement units must be positive", ErrInvalid)
	}
	if price <= 0 {
		return out, fmt.Errorf("%w: placement needs a price", ErrInvalid)
	}
	id, err := ensure(ctx, tx)
	if err != nil {
		return out, err
	}
	var companyID uuid.UUID
	var symbol, status string
	if err := tx.QueryRow(ctx, `SELECT company_id, symbol, status FROM instruments WHERE id = $1 FOR UPDATE`,
		instrumentID).Scan(&companyID, &symbol, &status); err != nil {
		return out, fmt.Errorf("rail: load instrument: %w", err)
	}
	if status != "listed" {
		return out, fmt.Errorf("%w: %s is %s, not listed", ErrInvalid, symbol, status)
	}
	cost, err := share.CostOf(units, price)
	if err != nil {
		return out, err
	}

	treasury, err := ledger.Resolve(ctx, tx, ledger.Company(companyID, ledger.KindTreasury, instrumentID))
	if err != nil {
		return out, err
	}
	treasuryCash, err := ledger.Resolve(ctx, tx, ledger.Company(companyID, ledger.KindTreasuryCash, ledger.AssetNGN))
	if err != nil {
		return out, err
	}
	wallet, err := ledger.Resolve(ctx, tx, ledger.Cardholder(id.MarketMaker, ledger.KindStockWallet, instrumentID))
	if err != nil {
		return out, err
	}
	cash, err := ledger.Resolve(ctx, tx, ledger.Cardholder(id.MarketMaker, ledger.KindAvailable, ledger.AssetNGN))
	if err != nil {
		return out, err
	}

	inTreasury, err := ledger.ShareBalance(ctx, tx, ledger.Company(companyID, ledger.KindTreasury, instrumentID))
	if err != nil {
		return out, err
	}
	if inTreasury < units {
		return out, fmt.Errorf("%w: placement of %s exceeds the %s in treasury", ErrInvalid, units, inTreasury)
	}
	available, err := ledger.NairaBalance(ctx, tx, ledger.Cardholder(id.MarketMaker, ledger.KindAvailable, ledger.AssetNGN))
	if err != nil {
		return out, err
	}
	if available < cost {
		return out, fmt.Errorf("%w: the market maker has %s available and the block costs %s; fund it first",
			ErrInvalid, available, cost)
	}

	// The key carries what was placed, so the same block twice is a no-op
	// and a different block is a second placement.
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%d|%s|%s|%s", instrumentID, int64(units), int64(price), today, by, reason)))
	key := "rail|mm_placement|" + hex.EncodeToString(sum[:8])
	txID, err := ledger.Post(ctx, tx, ledger.Tx{
		EventType:      "rail.mm_placement",
		BusinessDate:   today,
		IdempotencyKey: key,
		Entries: []ledger.Entry{
			{AccountID: cash, Amount: ledger.NGN(-cost), Reason: "placement.paid"},
			{AccountID: treasuryCash, Amount: ledger.NGN(cost), Reason: "placement.proceeds"},
			{AccountID: treasury, Amount: ledger.Equity(symbol, -units), Reason: "placement.release"},
			{AccountID: wallet, Amount: ledger.Equity(symbol, units), Reason: "placement.receive"},
		},
	})
	out = Placement{Symbol: symbol, Units: units, PriceKobo: price, CostKobo: cost, LedgerTx: txID}
	if errors.Is(err, ledger.ErrAlreadyPosted) {
		out.Held, err = ledger.ShareBalance(ctx, tx, ledger.Cardholder(id.MarketMaker, ledger.KindStockWallet, instrumentID))
		return out, err
	}
	if err != nil {
		return out, err
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO holding_lots (account_id, instrument_id, units, units_open, cost_kobo,
		                          cost_open_kobo, transferable_from, ledger_tx_id)
		VALUES ($1, $2, $3, $3, $4, $4, $5::date, $6)`,
		wallet, instrumentID, int64(units), int64(cost), today, txID); err != nil {
		return out, fmt.Errorf("rail: placement lot: %w", err)
	}
	note := fmt.Sprintf("placed with market maker %s at %s", ParticipantCode, price)
	if by != "" {
		note += " by " + by
	}
	if reason != "" {
		note += ": " + reason
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO cap_table_events (instrument_id, kind, units_delta, ledger_tx_id, note)
		VALUES ($1, 'transfer', $2, $3, $4)`, instrumentID, int64(units), txID, note); err != nil {
		return out, fmt.Errorf("rail: cap table event: %w", err)
	}

	// Listing particulars are not price-sensitive, so Submit does not halt;
	// the notice is published in the same transaction.
	body := fmt.Sprintf("%s of %s were placed with the designated market maker (%s) from the company's treasury at %s a share, for %s. "+
		"The block is the market maker's inventory for quoting; it is not tap-earned and is transferable from %s.",
		units, symbol, ParticipantCode, price, cost, today)
	submitter := by
	if submitter == "" {
		submitter = "rail"
	}
	discID, err := institution.Submit(ctx, tx, instrumentID, institution.DisclosureParticulars,
		"Market maker placement", body, submitter)
	if err != nil {
		return out, err
	}
	if err := institution.Publish(ctx, tx, discID, submitter); err != nil {
		return out, err
	}

	// The block is the inventory the quoting engine steers towards.
	if _, err := tx.Exec(ctx, `
		UPDATE liquidity_providers SET target_units = target_units + $3
		 WHERE instrument_id = $1 AND member_id = $2 AND state IN ('active','warned')`,
		instrumentID, id.Sponsor, int64(units)); err != nil {
		return out, fmt.Errorf("rail: provider target: %w", err)
	}
	out.Held, err = ledger.ShareBalance(ctx, tx, ledger.Cardholder(id.MarketMaker, ledger.KindStockWallet, instrumentID))
	return out, err
}

// Funding is capital moved to a market maker.
type Funding struct {
	MemberCode string     `json:"member_code"`
	AmountKobo money.Kobo `json:"amount_kobo"`
	LedgerTx   uuid.UUID  `json:"ledger_tx_id"`
	// Available is the market maker's cash after the transfer.
	Available money.Kobo `json:"available_kobo"`
}

// FundMarketMaker moves capital from the scheme's float to a member's
// market-making account. It is cash, not equity, so it is a ledger
// transaction and nothing else: event mm.capital, the reason on every leg.
// The house member's account exists from Ensure; another member must have a
// client account labelled market_maker.
//
// The idempotency key is the member, the date, the amount and the reason:
// the same funding twice on one day is a no-op, and a second tranche needs
// its own reason.
func (s *Service) FundMarketMaker(ctx context.Context, tx pgx.Tx, memberCode string,
	amount money.Kobo, today, by, reason string) (Funding, error) {

	var out Funding
	if amount <= 0 {
		return out, fmt.Errorf("%w: amount must be positive", ErrInvalid)
	}
	if strings.TrimSpace(reason) == "" {
		return out, fmt.Errorf("%w: a reason is required", ErrInvalid)
	}
	if _, err := ensure(ctx, tx); err != nil {
		return out, err
	}
	var memberID, cardholder uuid.UUID
	err := tx.QueryRow(ctx, `
		SELECT m.id, ca.cardholder_id FROM members m
		  JOIN client_accounts ca ON ca.member_id = m.id AND ca.label = 'market_maker' AND ca.cardholder_id IS NOT NULL
		 WHERE m.code = $1 AND 'market_maker' = ANY(m.roles)
		 ORDER BY ca.created_at LIMIT 1`, memberCode).Scan(&memberID, &cardholder)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, fmt.Errorf("%w: member %q is not a market maker with a market-making account", ErrNotFound, memberCode)
	}
	if err != nil {
		return out, fmt.Errorf("rail: find market maker: %w", err)
	}

	float, err := ledger.Resolve(ctx, tx, ledger.Scheme(ledger.KindFloat, ledger.AssetNGN))
	if err != nil {
		return out, err
	}
	cash, err := ledger.Resolve(ctx, tx, ledger.Cardholder(cardholder, ledger.KindAvailable, ledger.AssetNGN))
	if err != nil {
		return out, err
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d|%s", memberCode, today, int64(amount), reason)))
	why := "mm.capital: " + reason
	if by != "" {
		why += " — by " + by
	}
	txID, err := ledger.Post(ctx, tx, ledger.Tx{
		EventType:      "mm.capital",
		BusinessDate:   today,
		IdempotencyKey: "mm.capital|" + memberCode + "|" + hex.EncodeToString(sum[:8]),
		Entries: []ledger.Entry{
			{AccountID: float, Amount: ledger.NGN(-amount), Reason: why},
			{AccountID: cash, Amount: ledger.NGN(amount), Reason: why},
		},
	})
	if err != nil && !errors.Is(err, ledger.ErrAlreadyPosted) {
		return out, err
	}
	out = Funding{MemberCode: memberCode, AmountKobo: amount, LedgerTx: txID}
	out.Available, err = ledger.NairaBalance(ctx, tx, ledger.Cardholder(cardholder, ledger.KindAvailable, ledger.AssetNGN))
	return out, err
}

// QuoteRow is one provider's quote in a quoting run.
type QuoteRow struct {
	Symbol     string     `json:"symbol"`
	ProviderID uuid.UUID  `json:"provider_id"`
	Centre     money.Kobo `json:"centre_kobo"`
	// CentreSource is fair_value or reference.
	CentreSource string      `json:"centre_source"`
	Held         share.Units `json:"held_units"`
	Target       share.Units `json:"target_units"`
	SkewBps      int64       `json:"skew_bps"`
	SpreadBps    int64       `json:"spread_bps"`
	Bid          money.Kobo  `json:"bid_kobo"`
	Ask          money.Kobo  `json:"ask_kobo"`
	BidUnits     share.Units `json:"bid_units"`
	AskUnits     share.Units `json:"ask_units"`
	TwoSided     bool        `json:"two_sided"`
	Note         string      `json:"note,omitempty"`
	Error        string      `json:"error,omitempty"`
}

// QuoteMarket runs the quoting engine for every quotable provider on a date
// (today when empty), each in its own transaction so one symbol's refusal is
// a row rather than a rollback of the others. It opens sessions that nobody
// has, which on a scheduled day means the 10:05 quote opens the book the
// 12:00 close will freeze.
func (s *Service) QuoteMarket(ctx context.Context, sessionDate string) ([]QuoteRow, error) {
	if sessionDate == "" {
		sessionDate = scheme.BusinessDate(s.Now())
	}
	if _, err := scheme.ParseBusinessDate(sessionDate); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := s.inTx(ctx, func(tx pgx.Tx) error {
		return ensureCalendarDay(ctx, tx, sessionDate)
	}); err != nil {
		return nil, err
	}
	ids, err := exchange.QuotableProviders(ctx, s.Pool, sessionDate)
	if err != nil {
		return nil, err
	}
	out := []QuoteRow{}
	for _, id := range ids {
		row := QuoteRow{ProviderID: id}
		err := s.inTx(ctx, func(tx pgx.Tx) error {
			q, err := s.Engine.QuoteSession(ctx, tx, id, sessionDate, exchange.DefaultQuoteModel())
			row.Symbol = q.Symbol
			if err != nil {
				return err
			}
			row.Centre, row.CentreSource, row.Held, row.Target = q.Centre, q.CentreSource, q.Held, q.Target
			row.SkewBps, row.SpreadBps = q.SkewBps, q.SpreadBps
			row.Bid, row.Ask, row.BidUnits, row.AskUnits = q.Bid, q.Ask, q.BidUnits, q.AskUnits
			row.TwoSided, row.Note = q.TwoSided(), q.Note
			return nil
		})
		if err != nil {
			row.Error = err.Error()
			slog.Warn("market maker quote failed", "provider", id, "symbol", row.Symbol, "date", sessionDate, "err", err)
		}
		out = append(out, row)
	}
	return out, nil
}

// QuoteScheduler runs QuoteMarket once a day at a Lagos wall-clock time, on
// trading days. at is "HH:MM"; "off" disables it.
func (s *Service) QuoteScheduler(ctx context.Context, at string) error {
	return s.daily(ctx, "market maker quote", "MM_QUOTE_AT", at, func(date string) {
		rows, err := s.QuoteMarket(ctx, date)
		if err != nil {
			slog.Error("market maker quote failed", "date", date, "err", err)
			return
		}
		for _, r := range rows {
			slog.Info("market maker quoted", "symbol", r.Symbol, "bid", r.Bid, "ask", r.Ask,
				"bid_units", r.BidUnits, "ask_units", r.AskUnits, "skew_bps", r.SkewBps, "note", r.Note, "err", r.Error)
		}
	})
}
