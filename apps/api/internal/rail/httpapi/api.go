// Package httpapi is the rail's network surface: /v1/rail/*, service to
// service, one bearer token.
//
// It is deliberately a thin translation of package rail. The contract lives
// in docs/INTEGRATION.md and the semantics live in rail; this file decides
// only status codes and error shapes, in the same voice as the exchange API
// so a caller integrating both sees one product.
package httpapi

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"freedom/api/internal/exchange"
	"freedom/api/internal/rail"
)

// API serves the rail.
type API struct {
	Service *rail.Service
	// Token is the shared secret. The server refuses to mount the rail
	// without one; an unauthenticated rail would let anyone mint equity.
	Token string
}

// New builds the rail API. It returns an error rather than a handler when the
// token is empty, so main can refuse to start.
func New(s *rail.Service, token string) (*API, error) {
	if token == "" {
		return nil, errors.New("rail: RAIL_TOKEN is not set; refusing to mount /v1/rail")
	}
	return &API{Service: s, Token: token}, nil
}

// Routes returns the rail's HTTP surface, to be mounted at /v1/rail.
func (a *API) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(a.authenticate)

	r.Post("/businesses", a.onboardBusiness)
	r.Get("/businesses/{merchant_ref}", a.getBusiness)
	r.Get("/businesses/{merchant_ref}/holders", a.listHolders)

	r.Post("/taps", a.ingestTap)
	r.Post("/taps/{tap_ref}/reverse", a.reverseTap)

	r.Get("/cardholders/{cardholder_ref}/holdings", a.holdings)
	r.Get("/cardholders/{cardholder_ref}/holdings/{symbol}", a.holding)
	r.Get("/cardholders/{cardholder_ref}/activity", a.activity)

	r.Post("/market/close", a.closeMarket)
	r.Get("/market/today", a.today)
	return r
}

func (a *API) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		h := r.Header.Get("Authorization")
		// Constant time, so the comparison's duration says nothing about how
		// many leading bytes were right.
		if len(h) <= len(prefix) || h[:len(prefix)] != prefix ||
			subtle.ConstantTimeCompare([]byte(h[len(prefix):]), []byte(a.Token)) != 1 {
			fail(w, http.StatusUnauthorized, "unauthenticated", "Credentials were not accepted")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------- businesses

func (a *API) onboardBusiness(w http.ResponseWriter, r *http.Request) {
	var req rail.BusinessRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "malformed", "The request body is not valid JSON")
		return
	}
	out, created, err := a.Service.OnboardBusiness(r.Context(), req)
	if err != nil {
		writeError(w, err)
		return
	}
	// A rejection is an answer, not an error: the merchant needs the
	// findings. Only a fresh listing is a 201.
	status := http.StatusOK
	if created && out.State == "listed" {
		status = http.StatusCreated
	}
	respond(w, status, out)
}

func (a *API) getBusiness(w http.ResponseWriter, r *http.Request) {
	out, err := a.Service.GetBusiness(r.Context(), chi.URLParam(r, "merchant_ref"))
	if err != nil {
		writeError(w, err)
		return
	}
	respond(w, http.StatusOK, out)
}

func (a *API) listHolders(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	holders, next, err := a.Service.ListHolders(r.Context(), chi.URLParam(r, "merchant_ref"),
		r.URL.Query().Get("cursor"), limit)
	if err != nil {
		writeError(w, err)
		return
	}
	respond(w, http.StatusOK, map[string]any{"holders": holders, "next_cursor": next})
}

// ---------------------------------------------------------------- taps

func (a *API) ingestTap(w http.ResponseWriter, r *http.Request) {
	var t rail.Tap
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&t); err != nil {
		fail(w, http.StatusBadRequest, "malformed", "The request body is not valid JSON")
		return
	}
	out, created, err := a.Service.IngestTap(r.Context(), t)
	if err != nil {
		writeError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	respond(w, status, out)
}

func (a *API) reverseTap(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Reason string `json:"reason"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
			fail(w, http.StatusBadRequest, "malformed", "The request body is not valid JSON")
			return
		}
	}
	out, err := a.Service.ReverseTap(r.Context(), chi.URLParam(r, "tap_ref"), body.Reason)
	if err != nil {
		writeError(w, err)
		return
	}
	respond(w, http.StatusOK, out)
}

// ---------------------------------------------------------------- cardholders

func (a *API) holdings(w http.ResponseWriter, r *http.Request) {
	out, err := a.Service.Holdings(r.Context(), chi.URLParam(r, "cardholder_ref"))
	if err != nil {
		writeError(w, err)
		return
	}
	respond(w, http.StatusOK, out)
}

func (a *API) holding(w http.ResponseWriter, r *http.Request) {
	out, err := a.Service.Holding(r.Context(), chi.URLParam(r, "cardholder_ref"), chi.URLParam(r, "symbol"))
	if err != nil {
		writeError(w, err)
		return
	}
	respond(w, http.StatusOK, out)
}

func (a *API) activity(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	out, err := a.Service.Activity(r.Context(), chi.URLParam(r, "cardholder_ref"), limit)
	if err != nil {
		writeError(w, err)
		return
	}
	respond(w, http.StatusOK, map[string]any{"activity": out})
}

// ---------------------------------------------------------------- market

func (a *API) closeMarket(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SessionDate string `json:"session_date"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
			fail(w, http.StatusBadRequest, "malformed", "The request body is not valid JSON")
			return
		}
	}
	rows, err := a.Service.CloseMarket(r.Context(), body.SessionDate)
	if err != nil {
		writeError(w, err)
		return
	}
	respond(w, http.StatusOK, map[string]any{"instruments": rows})
}

func (a *API) today(w http.ResponseWriter, r *http.Request) {
	out, err := a.Service.Today(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	respond(w, http.StatusOK, out)
}

// ---------------------------------------------------------------- support

// errorBody matches the exchange API's shape: a stable code and a message
// written for the person who has to act on it.
type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func respond(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, code, message string) {
	respond(w, status, errorBody{Code: code, Message: message})
}

// writeError maps a refusal to a status the rail can act on. Duplicated from
// the exchange API rather than shared: the two surfaces refuse for different
// reasons and should be free to diverge.
func writeError(w http.ResponseWriter, err error) {
	var listed *rail.AlreadyListedError
	if errors.As(err, &listed) {
		// The refusal carries the symbol so the caller can show the merchant
		// which listing they already belong to.
		respond(w, http.StatusConflict, map[string]string{
			"error":   "already_listed",
			"message": listed.RCNumber + " is already listed as " + listed.Symbol,
			"symbol":  listed.Symbol,
		})
		return
	}
	switch {
	case errors.Is(err, rail.ErrInvalid):
		fail(w, http.StatusBadRequest, "malformed", capitalise(strings.TrimPrefix(err.Error(), "rail: invalid request: ")))
	case errors.Is(err, rail.ErrNotFound), errors.Is(err, pgx.ErrNoRows):
		fail(w, http.StatusNotFound, "not_found", capitalise(strings.TrimPrefix(err.Error(), "rail: not found: ")))
	case errors.Is(err, exchange.ErrMarketClosed):
		fail(w, http.StatusConflict, "market_closed", capitalise(err.Error()))
	case errors.Is(err, exchange.ErrHalted):
		fail(w, http.StatusConflict, "instrument_halted", "Trading in this instrument is halted")
	default:
		// A rule the exchange enforces is the caller's to hear; anything else
		// is ours and is not.
		if strings.HasPrefix(err.Error(), "exchange: ") {
			fail(w, http.StatusUnprocessableEntity, "rejected", capitalise(err.Error()))
			return
		}
		fail(w, http.StatusInternalServerError, "internal", "The rail could not process that request")
	}
}

func capitalise(s string) string {
	s = strings.TrimPrefix(s, "exchange: ")
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
