// Package httpapi is the exchange's network surface.
//
// Until this existed, everything the exchange could do was reachable only from
// Go — no app, no console, and no possibility of a second member. The API is
// JSON over HTTP rather than FIX: FIX is what a member bank's existing order
// management system speaks and is the right answer once one shows up, but it is
// a large protocol to implement for a venue whose only client today is a phone.
// The message shapes below stay close to FIX's field semantics so that adding
// a FIX gateway later is a translation rather than a redesign.
package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"freedom/api/internal/exchange"
	"freedom/api/internal/money"
	"freedom/api/internal/scheme"
	"freedom/api/internal/share"
)

// API serves order entry and market data.
type API struct {
	Pool   *pgxpool.Pool
	Engine *exchange.Engine
	// Now is the clock, injectable so tests do not depend on the day they run.
	Now func() time.Time
	// Auth resolves a request to a member. Returning an error rejects the call.
	Auth func(r *http.Request) (uuid.UUID, error)
}

// New builds an API on the launch engine.
func New(pool *pgxpool.Pool, auth func(*http.Request) (uuid.UUID, error)) *API {
	return &API{Pool: pool, Engine: exchange.NewEngine(), Now: time.Now, Auth: auth}
}

// Routes returns the exchange's HTTP surface.
func (a *API) Routes() http.Handler {
	r := chi.NewRouter()
	r.Get("/v1/health", a.health)

	r.Route("/v1/instruments", func(r chi.Router) {
		r.Get("/", a.listInstruments)
		r.Get("/{symbol}", a.getInstrument)
		r.Get("/{symbol}/market-data", a.marketData)
	})

	r.Route("/v1/orders", func(r chi.Router) {
		r.Use(a.authenticate)
		r.Post("/", a.placeOrder)
		r.Get("/{id}", a.getOrder)
		r.Delete("/{id}", a.cancelOrder)
	})

	r.Route("/v1/sessions", func(r chi.Router) {
		r.Use(a.authenticate)
		r.Get("/{symbol}/{date}", a.getSession)
	})
	return r
}

type memberKey struct{}

func (a *API) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := a.Auth(r)
		if err != nil {
			// Deliberately uninformative. Telling an unauthenticated caller
			// which half of their credential was wrong is an oracle.
			fail(w, http.StatusUnauthorized, "unauthenticated", "Credentials were not accepted")
			return
		}
		next.ServeHTTP(w, r.WithContext(withMember(r.Context(), id)))
	})
}

// ---------------------------------------------------------------- orders

type placeRequest struct {
	Symbol        string `json:"symbol"`
	Side          string `json:"side"`
	Type          string `json:"type"`
	LimitKobo     int64  `json:"limit_kobo,omitempty"`
	Units         int64  `json:"units,omitempty"`
	NotionalKobo  int64  `json:"notional_kobo,omitempty"`
	ClientOrderID string `json:"client_order_id,omitempty"`
	ClientAccount string `json:"client_account_id"`
}

type placeResponse struct {
	OrderID       string `json:"order_id"`
	ClientOrderID string `json:"client_order_id,omitempty"`
	Status        string `json:"status"`
	SessionDate   string `json:"session_date"`
}

func (a *API) placeOrder(w http.ResponseWriter, r *http.Request) {
	member, ok := memberFrom(r.Context())
	if !ok {
		fail(w, http.StatusUnauthorized, "unauthenticated", "Credentials were not accepted")
		return
	}
	var req placeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "malformed", "The request body is not valid JSON")
		return
	}

	sessionDate := scheme.BusinessDate(a.Now())
	var orderID uuid.UUID

	err := inTx(r, a.Pool, func(tx pgx.Tx) error {
		// The gateway check comes first, deliberately. Resolving the symbol or
		// the client account before it would let a suspended firm distinguish a
		// real client id from a guess by the shape of the refusal.
		if err := exchange.CheckMember(r.Context(), tx, member); err != nil {
			return err
		}
		instrumentID, err := resolveSymbol(r, tx, req.Symbol)
		if err != nil {
			return err
		}
		clientID, err := uuid.Parse(req.ClientAccount)
		if err != nil {
			return fmt.Errorf("%w: client_account_id is not a valid id", errBadRequest)
		}
		holder, ledgerAcct, err := resolveClient(r, tx, member, clientID, instrumentID)
		if err != nil {
			return err
		}

		s, err := a.Engine.Open(r.Context(), tx, instrumentID, sessionDate)
		if err != nil {
			return err
		}
		orderID, err = a.Engine.Place(r.Context(), tx, s, exchange.OrderRequest{
			MemberID: member, ClientAccountID: clientID, CardholderID: holder,
			AccountID:     ledgerAcct,
			Side:          exchange.Side(strings.ToLower(req.Side)),
			Type:          strings.ToLower(req.Type),
			Limit:         money.Kobo(req.LimitKobo),
			Qty:           share.Units(req.Units),
			Notional:      money.Kobo(req.NotionalKobo),
			ClientOrderID: req.ClientOrderID,
		})
		return err
	})
	if err != nil {
		writeError(w, err)
		return
	}

	ok2 := placeResponse{
		OrderID: orderID.String(), ClientOrderID: req.ClientOrderID,
		Status: "accepted", SessionDate: sessionDate,
	}
	respond(w, http.StatusCreated, ok2)
}

type orderView struct {
	OrderID     string `json:"order_id"`
	Symbol      string `json:"symbol"`
	Side        string `json:"side"`
	Type        string `json:"type"`
	LimitKobo   *int64 `json:"limit_kobo,omitempty"`
	Units       *int64 `json:"units,omitempty"`
	FilledUnits int64  `json:"filled_units"`
	State       string `json:"state"`
}

func (a *API) getOrder(w http.ResponseWriter, r *http.Request) {
	member, _ := memberFrom(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		fail(w, http.StatusBadRequest, "malformed", "That is not a valid order id")
		return
	}
	var v orderView
	err = inTx(r, a.Pool, func(tx pgx.Tx) error {
		// Scoped to the member: one member must never see another's orders, and
		// an order belonging to somebody else reads as absent rather than
		// forbidden — existence is itself information.
		return tx.QueryRow(r.Context(), `
			SELECT o.id::text, i.symbol, o.side, o.type, o.limit_kobo, o.qty_units,
			       o.filled_units, o.state
			  FROM orders o JOIN instruments i ON i.id = o.instrument_id
			 WHERE o.id = $1 AND o.member_id = $2`, id, member).
			Scan(&v.OrderID, &v.Symbol, &v.Side, &v.Type, &v.LimitKobo, &v.Units,
				&v.FilledUnits, &v.State)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, http.StatusNotFound, "not_found", "No such order")
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	respond(w, http.StatusOK, v)
}

func (a *API) cancelOrder(w http.ResponseWriter, r *http.Request) {
	member, _ := memberFrom(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		fail(w, http.StatusBadRequest, "malformed", "That is not a valid order id")
		return
	}
	var found, cancelled bool
	err = inTx(r, a.Pool, func(tx pgx.Tx) error {
		var err error
		found, cancelled, err = exchange.CancelOrder(r.Context(), tx, id, member)
		return err
	})
	if err != nil {
		writeError(w, err)
		return
	}
	if !found {
		// Another member's order, or none at all. Both read as absent: saying
		// "you may not cancel that" would confirm it exists.
		fail(w, http.StatusNotFound, "not_found", "No such order")
		return
	}
	if !cancelled {
		fail(w, http.StatusConflict, "not_cancellable",
			"That order has already filled, expired or been cancelled")
		return
	}
	respond(w, http.StatusOK, map[string]string{"order_id": id.String(), "status": "cancelled"})
}

// ---------------------------------------------------------------- market data

func (a *API) listInstruments(w http.ResponseWriter, r *http.Request) {
	type row struct {
		Symbol        string `json:"symbol"`
		Status        string `json:"status"`
		ReferenceKobo int64  `json:"reference_kobo"`
		Continuous    bool   `json:"continuous"`
		Halted        bool   `json:"halted"`
	}
	var out []row
	err := inTx(r, a.Pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(), `
			SELECT i.symbol, i.status, i.reference_price_kobo, i.clob_enabled,
			       EXISTS (SELECT 1 FROM trading_halts h
			                WHERE h.instrument_id = i.id AND h.released_at IS NULL)
			  FROM instruments i WHERE i.status <> 'draft' ORDER BY i.symbol`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var x row
			if err := rows.Scan(&x.Symbol, &x.Status, &x.ReferenceKobo, &x.Continuous, &x.Halted); err != nil {
				return err
			}
			out = append(out, x)
		}
		return rows.Err()
	})
	if err != nil {
		writeError(w, err)
		return
	}
	if out == nil {
		out = []row{}
	}
	respond(w, http.StatusOK, map[string]any{"instruments": out})
}

func (a *API) getInstrument(w http.ResponseWriter, r *http.Request) {
	type view struct {
		Symbol         string `json:"symbol"`
		Status         string `json:"status"`
		ReferenceKobo  int64  `json:"reference_kobo"`
		BandLoKobo     int64  `json:"band_lo_kobo"`
		BandHiKobo     int64  `json:"band_hi_kobo"`
		TickKobo       int64  `json:"tick_kobo"`
		LotUnits       int64  `json:"lot_units"`
		MinOrderKobo   int64  `json:"min_order_notional_kobo"`
		Continuous     bool   `json:"continuous"`
		Halted         bool   `json:"halted"`
		CarriedForward int    `json:"carry_forward_sessions"`
	}
	var v view
	err := inTx(r, a.Pool, func(tx pgx.Tx) error {
		var bandBps int64
		err := tx.QueryRow(r.Context(), `
			SELECT i.symbol, i.status, i.reference_price_kobo, i.static_band_bps,
			       i.tick_kobo, i.lot_units, i.min_order_notional_kobo,
			       i.clob_enabled, i.carry_forward_sessions,
			       EXISTS (SELECT 1 FROM trading_halts h
			                WHERE h.instrument_id = i.id AND h.released_at IS NULL)
			  FROM instruments i WHERE i.symbol = $1`, strings.ToUpper(chi.URLParam(r, "symbol"))).
			Scan(&v.Symbol, &v.Status, &v.ReferenceKobo, &bandBps, &v.TickKobo, &v.LotUnits,
				&v.MinOrderKobo, &v.Continuous, &v.CarriedForward, &v.Halted)
		if err != nil {
			return err
		}
		v.BandLoKobo = v.ReferenceKobo - v.ReferenceKobo*bandBps/10_000
		v.BandHiKobo = v.ReferenceKobo + v.ReferenceKobo*bandBps/10_000
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, http.StatusNotFound, "not_found", "No such instrument")
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	respond(w, http.StatusOK, v)
}

func (a *API) marketData(w http.ResponseWriter, r *http.Request) {
	type point struct {
		Date         string `json:"date"`
		Source       string `json:"source"`
		PriceKobo    int64  `json:"price_kobo"`
		AdjPriceKobo int64  `json:"adjusted_price_kobo"`
		VolumeUnits  int64  `json:"volume_units"`
		Trades       int    `json:"trades"`
	}
	var out []point
	err := inTx(r, a.Pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(), `
			SELECT p.obs_date::text, p.source, p.price_kobo, p.adj_price_kobo,
			       p.volume_units, p.trade_count
			  FROM price_observations_adjusted p
			  JOIN instruments i ON i.id = p.instrument_id
			 WHERE i.symbol = $1
			 ORDER BY p.obs_date DESC LIMIT 90`, strings.ToUpper(chi.URLParam(r, "symbol")))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var x point
			if err := rows.Scan(&x.Date, &x.Source, &x.PriceKobo, &x.AdjPriceKobo,
				&x.VolumeUnits, &x.Trades); err != nil {
				return err
			}
			out = append(out, x)
		}
		return rows.Err()
	})
	if err != nil {
		writeError(w, err)
		return
	}
	if out == nil {
		out = []point{}
	}
	respond(w, http.StatusOK, map[string]any{"observations": out})
}

func (a *API) getSession(w http.ResponseWriter, r *http.Request) {
	type view struct {
		Symbol       string  `json:"symbol"`
		SessionDate  string  `json:"session_date"`
		State        string  `json:"state"`
		ZeroVolume   bool    `json:"zero_volume"`
		PriceKobo    *int64  `json:"clearing_price_kobo,omitempty"`
		MatchedUnits *int64  `json:"matched_units,omitempty"`
		Rule         *string `json:"tie_break_rule,omitempty"`
		ResultHash   *string `json:"result_hash,omitempty"`
	}
	var v view
	err := inTx(r, a.Pool, func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(), `
			SELECT i.symbol, a.session_date::text, a.state, a.zero_volume,
			       a.clearing_price_kobo, a.matched_units, a.rule, a.result_hash
			  FROM auctions a JOIN instruments i ON i.id = a.instrument_id
			 WHERE i.symbol = $1 AND a.session_date = $2::date`,
			strings.ToUpper(chi.URLParam(r, "symbol")), chi.URLParam(r, "date")).
			Scan(&v.Symbol, &v.SessionDate, &v.State, &v.ZeroVolume, &v.PriceKobo,
				&v.MatchedUnits, &v.Rule, &v.ResultHash)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, http.StatusNotFound, "not_found", "No session on that date")
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	respond(w, http.StatusOK, v)
}

func (a *API) health(w http.ResponseWriter, r *http.Request) {
	if err := a.Pool.Ping(r.Context()); err != nil {
		fail(w, http.StatusServiceUnavailable, "unavailable", "The exchange is not accepting requests")
		return
	}
	respond(w, http.StatusOK, map[string]any{
		"status":        "ok",
		"business_date": scheme.BusinessDate(a.Now()),
	})
}

var _ = slog.Default
