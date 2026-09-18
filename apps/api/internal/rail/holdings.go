package rail

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"freedom/api/internal/ledger"
	"freedom/api/internal/money"
	"freedom/api/internal/scheme"
	"freedom/api/internal/share"
)

// What a cardholder owns, read from the lots. Lots are the holding: the wallet
// balance is the same number less whatever an open order has reserved, and the
// lock, the cost basis and the acquisition date all live on the lot.

// HoldingLine is one instrument in a cardholder's portfolio.
type HoldingLine struct {
	Symbol             string       `json:"symbol"`
	LegalName          string       `json:"legal_name"`
	TradingName        string       `json:"trading_name"`
	Units              share.Units  `json:"units"`
	Shares             string       `json:"shares"`
	SellableUnits      share.Units  `json:"sellable_units"`
	LockedUnits        share.Units  `json:"locked_units"`
	NextUnlock         *string      `json:"next_unlock"`
	CostKobo           money.Kobo   `json:"cost_kobo"`
	ReferencePriceKobo money.Kobo   `json:"reference_price_kobo"`
	ValueKobo          money.Kobo   `json:"value_kobo"`
	ChangeBps          int64        `json:"change_bps"`
	Lots               int          `json:"lots"`
	LastSession        *LastSession `json:"last_session"`
}

// LastSession is the latest price observation for an instrument.
type LastSession struct {
	Date      string     `json:"date"`
	PriceKobo money.Kobo `json:"price_kobo"`
	Source    string     `json:"source"`
}

// Portfolio is everything a cardholder holds, valued at the reference.
type Portfolio struct {
	CardholderRef  string        `json:"cardholder_ref"`
	AsOf           string        `json:"as_of"`
	TotalValueKobo money.Kobo    `json:"total_value_kobo"`
	TotalCostKobo  money.Kobo    `json:"total_cost_kobo"`
	Holdings       []HoldingLine `json:"holdings"`
}

// Holdings values every instrument a cardholder holds.
func (s *Service) Holdings(ctx context.Context, cardholderRef string) (Portfolio, error) {
	asOf := scheme.BusinessDate(s.Now())
	p := Portfolio{CardholderRef: cardholderRef, AsOf: asOf, Holdings: []HoldingLine{}}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		holder, err := cardholderByRef(ctx, tx, cardholderRef)
		if err != nil {
			return err
		}
		lines, err := s.holdingLines(ctx, tx, holder, asOf, "")
		if err != nil {
			return err
		}
		p.Holdings = lines
		for _, l := range lines {
			p.TotalValueKobo += l.ValueKobo
			p.TotalCostKobo += l.CostKobo
		}
		return nil
	})
	return p, err
}

// HoldingDetail is one holding with its lots and price history. The outer
// `lots` is the list; it shadows the line's count on the wire, as the contract
// asks.
type HoldingDetail struct {
	HoldingLine
	CardholderRef string       `json:"cardholder_ref"`
	AsOf          string       `json:"as_of"`
	LotLines      []Lot        `json:"lots"`
	Prices        []PricePoint `json:"prices"`
}

// Lot is one acquisition, as it stands today.
type Lot struct {
	Units            share.Units `json:"units"`
	CostKobo         money.Kobo  `json:"cost_kobo"`
	AcquiredAt       string      `json:"acquired_at"`
	TransferableFrom string      `json:"transferable_from"`
	TapRef           *string     `json:"tap_ref"`
}

// PricePoint mirrors /v1/instruments/{symbol}/market-data.
type PricePoint struct {
	Date         string `json:"date"`
	Source       string `json:"source"`
	PriceKobo    int64  `json:"price_kobo"`
	AdjPriceKobo int64  `json:"adjusted_price_kobo"`
	VolumeUnits  int64  `json:"volume_units"`
	Trades       int    `json:"trades"`
}

// Holding returns one instrument's position with its lots and prices.
func (s *Service) Holding(ctx context.Context, cardholderRef, symbol string) (HoldingDetail, error) {
	asOf := scheme.BusinessDate(s.Now())
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	d := HoldingDetail{CardholderRef: cardholderRef, AsOf: asOf, LotLines: []Lot{}, Prices: []PricePoint{}}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		holder, err := cardholderByRef(ctx, tx, cardholderRef)
		if err != nil {
			return err
		}
		lines, err := s.holdingLines(ctx, tx, holder, asOf, symbol)
		if err != nil {
			return err
		}
		if len(lines) == 0 {
			return fmt.Errorf("%w: %s holds no %s", ErrNotFound, cardholderRef, symbol)
		}
		d.HoldingLine = lines[0]

		rows, err := tx.Query(ctx, `
			SELECT l.units_open, l.cost_open_kobo, l.acquired_at::text, l.transferable_from::text, p.arn
			  FROM holding_lots l
			  JOIN accounts a ON a.id = l.account_id
			  JOIN instruments i ON i.id = l.instrument_id
			  LEFT JOIN ledger_tx t ON t.id = l.ledger_tx_id
			  LEFT JOIN buyback_intents bi ON t.idempotency_key = 'buyback|' || bi.id::text
			  LEFT JOIN presentments p ON p.id = bi.presentment_id
			 WHERE a.owner_type = 'cardholder' AND a.owner_id = $1 AND a.kind = 'stock_wallet'
			   AND i.symbol = $2 AND l.units_open > 0
			 ORDER BY l.acquired_at, l.id`, holder, symbol)
		if err != nil {
			return fmt.Errorf("rail: lots: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var l Lot
			var arn *string
			if err := rows.Scan(&l.Units, &l.CostKobo, &l.AcquiredAt, &l.TransferableFrom, &arn); err != nil {
				return err
			}
			if arn != nil {
				l.TapRef = tapRefOf(*arn)
			}
			d.LotLines = append(d.LotLines, l)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()

		d.Prices, err = prices(ctx, tx, symbol)
		return err
	})
	return d, err
}

// holdingLines aggregates a cardholder's lots per instrument, optionally for
// one symbol.
func (s *Service) holdingLines(ctx context.Context, q ledger.Querier, holder uuid.UUID, asOf, symbol string) ([]HoldingLine, error) {
	rows, err := q.Query(ctx, `
		SELECT i.symbol, c.legal_name,
		       COALESCE((SELECT trading_name FROM merchants WHERE company_id = c.id ORDER BY created_at LIMIT 1), c.legal_name),
		       SUM(l.units_open)::bigint,
		       COALESCE(SUM(l.units_open - l.units_reserved) FILTER (WHERE l.transferable_from <= $2::date), 0)::bigint,
		       COALESCE(SUM(l.units_open) FILTER (WHERE l.transferable_from > $2::date), 0)::bigint,
		       MIN(l.transferable_from::text) FILTER (WHERE l.transferable_from > $2::date),
		       SUM(l.cost_open_kobo)::bigint,
		       i.reference_price_kobo,
		       COUNT(*),
		       po.obs_date::text, po.price_kobo, po.source
		  FROM holding_lots l
		  JOIN accounts a ON a.id = l.account_id
		  JOIN instruments i ON i.id = l.instrument_id
		  JOIN companies c ON c.id = i.company_id
		  LEFT JOIN price_observations_current po ON po.instrument_id = i.id
		 WHERE a.owner_type = 'cardholder' AND a.owner_id = $1 AND a.kind = 'stock_wallet'
		   AND l.units_open > 0 AND ($3 = '' OR i.symbol = $3)
		 GROUP BY i.id, c.id, po.obs_date, po.price_kobo, po.source
		 ORDER BY i.symbol`, holder, asOf, symbol)
	if err != nil {
		return nil, fmt.Errorf("rail: holdings: %w", err)
	}
	defer rows.Close()

	var out []HoldingLine
	for rows.Next() {
		var h HoldingLine
		var obsDate, obsSource *string
		var obsPrice *int64
		if err := rows.Scan(&h.Symbol, &h.LegalName, &h.TradingName, &h.Units, &h.SellableUnits,
			&h.LockedUnits, &h.NextUnlock, &h.CostKobo, &h.ReferencePriceKobo, &h.Lots,
			&obsDate, &obsPrice, &obsSource); err != nil {
			return nil, err
		}
		h.Shares = h.Units.String()
		// Valued the way a statement values it, so the two never disagree.
		v, err := share.CostOf(h.Units, h.ReferencePriceKobo)
		if err != nil {
			return nil, err
		}
		h.ValueKobo = v
		if h.CostKobo > 0 {
			h.ChangeBps = int64(h.ValueKobo-h.CostKobo) * 10_000 / int64(h.CostKobo)
		}
		if obsDate != nil && obsPrice != nil && obsSource != nil {
			h.LastSession = &LastSession{Date: *obsDate, PriceKobo: money.Kobo(*obsPrice), Source: *obsSource}
		}
		out = append(out, h)
	}
	if out == nil {
		out = []HoldingLine{}
	}
	return out, rows.Err()
}

func prices(ctx context.Context, q ledger.Querier, symbol string) ([]PricePoint, error) {
	// One price per date. A date can hold several observations — a carried
	// reference and, later, a manual correction — and a series that shows
	// both draws a vertical line and lets a chart pick the wrong one. The
	// precedence is the one price_observations_current uses: a correction
	// beats a print beats a carry-forward.
	rows, err := q.Query(ctx, `
		SELECT DISTINCT ON (p.obs_date)
		       p.obs_date::text, p.source, p.price_kobo, p.adj_price_kobo, p.volume_units, p.trade_count
		  FROM price_observations_adjusted p
		  JOIN instruments i ON i.id = p.instrument_id
		 WHERE i.symbol = $1
		 ORDER BY p.obs_date DESC,
		          CASE p.source WHEN 'manual' THEN 0 WHEN 'auction' THEN 1 WHEN 'clob' THEN 2 ELSE 3 END,
		          p.id DESC
		 LIMIT 90`, symbol)
	if err != nil {
		return nil, fmt.Errorf("rail: prices: %w", err)
	}
	defer rows.Close()
	out := []PricePoint{}
	for rows.Next() {
		var x PricePoint
		if err := rows.Scan(&x.Date, &x.Source, &x.PriceKobo, &x.AdjPriceKobo, &x.VolumeUnits, &x.Trades); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// Activity is one buyback intent as the cardholder sees it: where the card
// was spent, for how much, and what that tap bought.
type Activity struct {
	TapRef        *string     `json:"tap_ref"`
	MerchantRef   *string     `json:"merchant_ref"`
	MerchantName  string      `json:"merchant_name"`
	Symbol        *string     `json:"symbol"`
	TapAmountKobo money.Kobo  `json:"tap_amount_kobo"`
	FundingKobo   money.Kobo  `json:"funding_kobo"`
	State         string      `json:"state"`
	Units         share.Units `json:"units"`
	PriceKobo     money.Kobo  `json:"price_kobo"`
	At            string      `json:"at"`
}

// Activity lists a cardholder's intents, newest first. Every state is shown
// with its state named — an intent that escrowed or unwound is still something
// the cardholder will ask about.
func (s *Service) Activity(ctx context.Context, cardholderRef string, limit int) ([]Activity, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	out := []Activity{}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		holder, err := cardholderByRef(ctx, tx, cardholderRef)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT p.arn, m.external_ref, COALESCE(NULLIF(m.trading_name, ''), m.legal_name),
			       i.symbol, p.amount_kobo, bi.funding_kobo, bi.state,
			       COALESCE(bi.allocated_units, 0), COALESCE(bi.price_kobo, 0), bi.created_at::text
			  FROM buyback_intents bi
			  JOIN presentments p ON p.id = bi.presentment_id
			  JOIN merchants m ON m.id = bi.merchant_id
			  LEFT JOIN instruments i ON i.id = bi.instrument_id
			 WHERE bi.cardholder_id = $1
			 ORDER BY bi.created_at DESC, bi.id DESC LIMIT $2`, holder, limit)
		if err != nil {
			return fmt.Errorf("rail: activity: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var a Activity
			var arn string
			if err := rows.Scan(&arn, &a.MerchantRef, &a.MerchantName, &a.Symbol, &a.TapAmountKobo,
				&a.FundingKobo, &a.State, &a.Units, &a.PriceKobo, &a.At); err != nil {
				return err
			}
			a.TapRef = tapRefOf(arn)
			out = append(out, a)
		}
		return rows.Err()
	})
	return out, err
}

func cardholderByRef(ctx context.Context, q ledger.Querier, ref string) (uuid.UUID, error) {
	var id uuid.UUID
	err := q.QueryRow(ctx, `SELECT id FROM cardholders WHERE external_ref = $1`, strings.TrimSpace(ref)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, fmt.Errorf("%w: cardholder %q", ErrNotFound, ref)
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("rail: find cardholder: %w", err)
	}
	return id, nil
}
