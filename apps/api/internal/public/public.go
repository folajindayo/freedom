// Package public is the exchange's public face: the market page anyone can
// open and the JSON it reads. No credential, nothing that identifies a
// cardholder, a merchant or a member, nothing from a book that has not
// published, no disclosure that has not been released to everyone at once.
//
// Reads are plain scheme-pool queries over the same tables the exchange API
// and the console read; the numbers here are the numbers there. Every
// response is cacheable for thirty seconds, which is the most a public
// reader can be behind a close.
package public

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"freedom/api/internal/rail"
	"freedom/api/internal/scheme"
	"freedom/api/internal/share"
)

//go:embed ui/index.html
var page []byte

// Server serves the public market.
type Server struct {
	Pool *pgxpool.Pool
	// Rail owns the "today" view: business date, trading flag, phase.
	Rail *rail.Service
	// Now is the clock, injectable for tests.
	Now func() time.Time
}

// New builds the public server. There is no token: everything here is public.
func New(pool *pgxpool.Pool, svc *rail.Service) *Server {
	return &Server{Pool: pool, Rail: svc, Now: time.Now}
}

// API is the JSON feed, to be mounted at /v1/market.
func (s *Server) API() http.Handler {
	r := chi.NewRouter()
	r.Get("/", s.json(s.market))
	r.Get("/{symbol}", s.json(s.instrument))
	return r
}

// Page is the market page, to be mounted at /market. The symbol page is the
// same file; the browser reads the path and asks the feed.
func (s *Server) Page() http.Handler {
	r := chi.NewRouter()
	serve := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=30")
		_, _ = w.Write(page)
	}
	r.Get("/", serve)
	r.Get("/{symbol}", serve)
	return r
}

// ---------------------------------------------------------------- shapes

// Instrument is one row of the market table.
type Instrument struct {
	Symbol      string `json:"symbol"`
	LegalName   string `json:"legal_name"`
	TradingName string `json:"trading_name"`
	Status      string `json:"status"`
	// Structure is auction_only, continuous, under_review or demoted.
	Structure string `json:"structure"`
	Halted    bool   `json:"halted"`
	// PriceKobo is the last published price: today's session if it has
	// published, else the current observation, else the reference.
	PriceKobo int64 `json:"price_kobo"`
	// PriceSource is auction, carry_forward, manual (a correction or a
	// re-anchor by the exchange, on the record as an observation) or
	// reference.
	PriceSource string `json:"price_source"`
	// ChangeBps is the move against the previous session's (adjusted) price;
	// null when there is no previous session.
	ChangeBps          *int64 `json:"change_bps"`
	ReferencePriceKobo int64  `json:"reference_price_kobo"`
	// MarketCapKobo is shares in issue × price, exact.
	MarketCapKobo    int64   `json:"market_cap_kobo"`
	SharesInIssue    int64   `json:"shares_in_issue_units"`
	Holders          int     `json:"holders"`
	VolumeUnitsToday int64   `json:"volume_units_today"`
	ListedAt         *string `json:"listed_at"`
}

// Index is the venue's benchmark, when one has been declared.
type Index struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	ValueBps  *int64  `json:"value_bps"`
	BaseValue int64   `json:"base_value"`
	ObsDate   *string `json:"obs_date"`
}

// Market is GET /v1/market.
type Market struct {
	BusinessDate string       `json:"business_date"`
	IsTrading    bool         `json:"is_trading"`
	Phase        string       `json:"phase"`
	AsOf         string       `json:"as_of"`
	Instruments  []Instrument `json:"instruments"`
	Index        *Index       `json:"index"`
}

// Price is one adjusted observation.
type Price struct {
	Date        string `json:"date"`
	Source      string `json:"source"`
	AdjPrice    int64  `json:"adjusted_price_kobo"`
	VolumeUnits int64  `json:"volume_units"`
	Trades      int    `json:"trades"`
}

// Session is one call auction. Price and volume are only shown once the
// session has published: the book is dark until then, to everyone.
type Session struct {
	Date         string `json:"date"`
	State        string `json:"state"`
	PriceKobo    *int64 `json:"clearing_price_kobo"`
	MatchedUnits *int64 `json:"matched_units"`
	ZeroVolume   bool   `json:"zero_volume"`
}

// Company is who the instrument is a share of.
type Company struct {
	LegalName   string  `json:"legal_name"`
	RCNumber    *string `json:"rc_number"`
	TradingName string  `json:"trading_name"`
	MCC         string  `json:"mcc"`
}

// CapTable is the share count as the register sees it.
type CapTable struct {
	Authorised   int64 `json:"shares_authorised_units"`
	InIssue      int64 `json:"shares_in_issue_units"`
	OnRegister   int64 `json:"on_register_units"`
	Treasury     int64 `json:"treasury_units"`
	DailyRelease int64 `json:"daily_release_units"`
}

// CorporateAction is a split, bonus or dividend that has been announced or
// applied. Cancelled ones are not shown.
type CorporateAction struct {
	Kind     string `json:"kind"`
	State    string `json:"state"`
	RatioNum *int64 `json:"ratio_num"`
	RatioDen *int64 `json:"ratio_den"`
	DPSKobo  *int64 `json:"dps_kobo"`
	ExDate   string `json:"ex_date"`
}

// Disclosure is one published disclosure. Unpublished ones do not exist here.
type Disclosure struct {
	Kind        string `json:"kind"`
	Headline    string `json:"headline"`
	PublishedAt string `json:"published_at"`
}

// Detail is GET /v1/market/{symbol}.
type Detail struct {
	Instrument
	Prices           []Price           `json:"prices"`
	Sessions         []Session         `json:"sessions"`
	Company          Company           `json:"company"`
	CapTable         CapTable          `json:"cap_table"`
	CorporateActions []CorporateAction `json:"corporate_actions"`
	Disclosures      []Disclosure      `json:"disclosures"`
}

// ---------------------------------------------------------------- reads

// instrumentSelect is the row behind both endpoints. $1 is the business date.
//
// The price is decided in SQL so the list and the record cannot disagree:
// today's published clearing price, else the current observation (a real
// print beats a carry-forward on the same date, as the view orders it), else
// the reference. The previous session's price is the newest adjusted
// observation before the date of the one that gave the price.
const instrumentSelect = `
	SELECT i.symbol, c.legal_name, COALESCE(m.trading_name, ''), i.status, i.clob_review_state,
	       EXISTS (SELECT 1 FROM trading_halts h WHERE h.instrument_id = i.id AND h.released_at IS NULL),
	       i.reference_price_kobo, i.shares_in_issue_units, i.listed_at,
	       CASE WHEN a.state = 'published' AND a.clearing_price_kobo IS NOT NULL
	                 AND NOT (po.source = 'manual' AND po.obs_date >= a.session_date) THEN a.clearing_price_kobo
	            ELSE po.price_kobo END,
	       CASE WHEN a.state = 'published' AND a.clearing_price_kobo IS NOT NULL
	                 AND NOT (po.source = 'manual' AND po.obs_date >= a.session_date) THEN 'auction'
	            ELSE po.source END,
	       prev.adj_price_kobo,
	       (SELECT COUNT(DISTINCT l.account_id) FROM holding_lots l JOIN accounts acc ON acc.id = l.account_id
	         WHERE l.instrument_id = i.id AND acc.kind = 'stock_wallet' AND l.units_open > 0)::int,
	       (SELECT COALESCE(SUM(f.units), 0) FROM fills f
	         WHERE f.instrument_id = i.id AND f.session_date = $1::date AND f.side = 'buy')::bigint
	  FROM instruments i
	  JOIN companies c ON c.id = i.company_id
	  LEFT JOIN merchants m ON m.company_id = c.id
	  LEFT JOIN auctions a ON a.instrument_id = i.id AND a.session_date = $1::date
	  LEFT JOIN price_observations_current po ON po.instrument_id = i.id
	  LEFT JOIN LATERAL (
	       SELECT x.adj_price_kobo FROM price_observations_adjusted x
	        WHERE x.instrument_id = i.id AND x.obs_date < COALESCE(po.obs_date, $1::date)
	        ORDER BY x.obs_date DESC,
	                 CASE x.source WHEN 'manual' THEN 0 WHEN 'auction' THEN 1 WHEN 'clob' THEN 2 ELSE 3 END,
	                 x.id DESC LIMIT 1) prev ON true
	 WHERE i.status <> 'draft'`

func scanInstrument(rows pgx.Rows) (Instrument, error) {
	var v Instrument
	var listedAt *time.Time
	var price *int64
	var source *string
	var prev *int64
	if err := rows.Scan(&v.Symbol, &v.LegalName, &v.TradingName, &v.Status, &v.Structure, &v.Halted,
		&v.ReferencePriceKobo, &v.SharesInIssue, &listedAt, &price, &source, &prev,
		&v.Holders, &v.VolumeUnitsToday); err != nil {
		return v, err
	}
	if listedAt != nil {
		t := listedAt.UTC().Format(time.RFC3339)
		v.ListedAt = &t
	}
	switch {
	case price != nil && *price > 0:
		v.PriceKobo = *price
		v.PriceSource = "auction"
		if source != nil && (*source == "carry_forward" || *source == "manual") {
			v.PriceSource = *source
		}
	default:
		v.PriceKobo = v.ReferencePriceKobo
		v.PriceSource = "reference"
	}
	if prev != nil && *prev > 0 && v.PriceSource != "reference" {
		bps := (v.PriceKobo - *prev) * 10_000 / *prev
		v.ChangeBps = &bps
	}
	v.MarketCapKobo = valueOf(v.SharesInIssue, v.PriceKobo)
	return v, nil
}

// valueOf is units at a price, in kobo: units × price / 1e8, exact in the
// intermediate — the same arithmetic the rail reports to the merchant.
func valueOf(units, price int64) int64 {
	if units <= 0 || price <= 0 {
		return 0
	}
	n := new(big.Int).Mul(big.NewInt(units), big.NewInt(price))
	return n.Quo(n, big.NewInt(int64(share.PerShare))).Int64()
}

func (s *Server) market(ctx context.Context, _ *http.Request) (any, error) {
	today, err := s.Rail.Today(ctx)
	if err != nil {
		return nil, err
	}
	out := Market{
		BusinessDate: today.BusinessDate, IsTrading: today.IsTrading, Phase: today.Phase,
		AsOf: s.Now().UTC().Format(time.RFC3339), Instruments: []Instrument{},
	}
	rows, err := s.Pool.Query(ctx, instrumentSelect+` ORDER BY i.symbol`, today.BusinessDate)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		v, err := scanInstrument(rows)
		if err != nil {
			return nil, err
		}
		out.Instruments = append(out.Instruments, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	var ix Index
	var obsDate *time.Time
	err = s.Pool.QueryRow(ctx, `
		SELECT i.id, i.name, i.base_value, v.value_bps, v.obs_date
		  FROM indices i
		  LEFT JOIN LATERAL (SELECT value_bps, obs_date FROM index_values x
		                      WHERE x.index_id = i.id ORDER BY obs_date DESC LIMIT 1) v ON true
		 ORDER BY i.id LIMIT 1`).Scan(&ix.ID, &ix.Name, &ix.BaseValue, &ix.ValueBps, &obsDate)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return nil, err
	default:
		if obsDate != nil {
			d := obsDate.Format("2006-01-02")
			ix.ObsDate = &d
		}
		out.Index = &ix
	}
	return out, nil
}

func (s *Server) instrument(ctx context.Context, r *http.Request) (any, error) {
	symbol := strings.ToUpper(chi.URLParam(r, "symbol"))
	today := scheme.BusinessDate(s.Now())
	var d Detail
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, instrumentSelect+` AND i.symbol = $2`, today, symbol)
		if err != nil {
			return err
		}
		if !rows.Next() {
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			return pgx.ErrNoRows
		}
		d.Instrument, err = scanInstrument(rows)
		rows.Close()
		if err != nil {
			return err
		}

		var id string
		if err := tx.QueryRow(ctx, `
			SELECT i.id, c.legal_name, c.rc_number, COALESCE(m.trading_name, ''), COALESCE(m.mcc, ''),
			       i.shares_authorised_units, i.shares_in_issue_units,
			       (SELECT COALESCE(SUM(l.units_open), 0) FROM holding_lots l JOIN accounts acc ON acc.id = l.account_id
			         WHERE l.instrument_id = i.id AND acc.kind = 'stock_wallet')::bigint,
			       COALESCE((SELECT balance FROM account_balances b WHERE b.account_id = tp.account_id), 0)::bigint,
			       COALESCE(tp.daily_release_units, 0)
			  FROM instruments i
			  JOIN companies c ON c.id = i.company_id
			  LEFT JOIN merchants m ON m.company_id = c.id
			  LEFT JOIN treasury_pools tp ON tp.instrument_id = i.id
			 WHERE i.symbol = $1`, symbol).
			Scan(&id, &d.Company.LegalName, &d.Company.RCNumber, &d.Company.TradingName, &d.Company.MCC,
				&d.CapTable.Authorised, &d.CapTable.InIssue, &d.CapTable.OnRegister,
				&d.CapTable.Treasury, &d.CapTable.DailyRelease); err != nil {
			return err
		}
		d.Company.MCC = strings.TrimSpace(d.Company.MCC)

		d.Prices = []Price{}
		if err := each(ctx, tx, `
			SELECT * FROM (
			  SELECT DISTINCT ON (obs_date) obs_date::text, source, adj_price_kobo, volume_units, trade_count
			    FROM price_observations_adjusted
			   WHERE instrument_id = $1
			   ORDER BY obs_date DESC, CASE source WHEN 'manual' THEN 0 WHEN 'auction' THEN 1 WHEN 'clob' THEN 2 ELSE 3 END, id DESC
			   LIMIT 90) p ORDER BY obs_date`, func(rows pgx.Rows) error {
			var p Price
			if err := rows.Scan(&p.Date, &p.Source, &p.AdjPrice, &p.VolumeUnits, &p.Trades); err != nil {
				return err
			}
			d.Prices = append(d.Prices, p)
			return nil
		}, id); err != nil {
			return err
		}

		d.Sessions = []Session{}
		if err := each(ctx, tx, `
			SELECT session_date::text, state,
			       CASE WHEN state = 'published' THEN clearing_price_kobo END,
			       CASE WHEN state = 'published' THEN matched_units END,
			       zero_volume
			  FROM auctions WHERE instrument_id = $1 ORDER BY session_date DESC LIMIT 20`, func(rows pgx.Rows) error {
			var x Session
			if err := rows.Scan(&x.Date, &x.State, &x.PriceKobo, &x.MatchedUnits, &x.ZeroVolume); err != nil {
				return err
			}
			d.Sessions = append(d.Sessions, x)
			return nil
		}, id); err != nil {
			return err
		}

		d.CorporateActions = []CorporateAction{}
		if err := each(ctx, tx, `
			SELECT kind, state, ratio_num, ratio_den, dps_kobo, ex_date::text
			  FROM corporate_actions WHERE instrument_id = $1 AND state <> 'cancelled'
			 ORDER BY ex_date DESC LIMIT 50`, func(rows pgx.Rows) error {
			var x CorporateAction
			if err := rows.Scan(&x.Kind, &x.State, &x.RatioNum, &x.RatioDen, &x.DPSKobo, &x.ExDate); err != nil {
				return err
			}
			d.CorporateActions = append(d.CorporateActions, x)
			return nil
		}, id); err != nil {
			return err
		}

		d.Disclosures = []Disclosure{}
		return each(ctx, tx, `
			SELECT kind, headline, published_at
			  FROM disclosures WHERE instrument_id = $1 AND published_at IS NOT NULL
			 ORDER BY published_at DESC LIMIT 50`, func(rows pgx.Rows) error {
			var x Disclosure
			var at time.Time
			if err := rows.Scan(&x.Kind, &x.Headline, &at); err != nil {
				return err
			}
			x.PublishedAt = at.UTC().Format(time.RFC3339)
			d.Disclosures = append(d.Disclosures, x)
			return nil
		}, id)
	})
	if err != nil {
		return nil, err
	}
	return d, nil
}

// ---------------------------------------------------------------- plumbing

type handler func(ctx context.Context, r *http.Request) (any, error)

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (s *Server) json(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		v, err := h(ctx, r)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeJSON(w, http.StatusNotFound, "no-store", errorBody{"not_found", "No such instrument"})
				return
			}
			slog.Error("public", "path", r.URL.Path, "err", err)
			writeJSON(w, http.StatusInternalServerError, "no-store", errorBody{"internal", "Something went wrong"})
			return
		}
		writeJSON(w, http.StatusOK, "public, max-age=30", v)
	}
}

func writeJSON(w http.ResponseWriter, status int, cache string, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", cache)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// each runs a query and calls fn once per row.
func each(ctx context.Context, tx pgx.Tx, sql string, fn func(pgx.Rows) error, args ...any) error {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}
