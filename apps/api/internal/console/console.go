// Package console is the operations console: one embedded page and the JSON
// it reads, served by exchanged from the same process that runs the close,
// so what the page shows and what the market did are never out of step.
//
// Reads are plain scheme-pool queries over the schema. The few writes call
// the engine functions the tests already exercise — the console decides
// nothing itself, it only carries an operator's name to a function that
// wanted one.
package console

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"freedom/api/internal/exchange"
	"freedom/api/internal/rail"
	"freedom/api/internal/scheme"
)

//go:embed ui/index.html
var page []byte

// Server serves the console.
type Server struct {
	Pool *pgxpool.Pool
	// Rail owns the close and the "today" view; the console reuses both.
	Rail *rail.Service
	// Token is the bearer the page presents. Required: the console can halt
	// a symbol and close the market, so it does not mount without one.
	Token string
	// Now is the clock, injectable for tests.
	Now func() time.Time
	// Gate is the liquidity standard the gate view and graduate action use.
	Gate exchange.LiquidityTest
}

// New builds the console. An empty token is refused, like the rail's.
func New(pool *pgxpool.Pool, svc *rail.Service, token string) (*Server, error) {
	if token == "" {
		return nil, errors.New("console: CONSOLE_TOKEN is not set; refusing to mount /console")
	}
	return &Server{Pool: pool, Rail: svc, Token: token, Now: time.Now, Gate: exchange.StandardLiquidityTest()}, nil
}

// Routes returns the console, to be mounted at /console.
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Get("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(page)
	})
	r.Route("/api", func(r chi.Router) {
		r.Use(s.authenticate)
		r.Get("/overview", s.json(s.overview))
		r.Get("/instruments", s.json(s.instruments))
		r.Get("/instruments/{symbol}", s.json(s.instrument))
		r.Get("/sessions", s.json(s.sessions))
		r.Get("/orders", s.json(s.orders))
		r.Get("/fills", s.json(s.fills))
		r.Get("/alerts", s.json(s.alerts))
		r.Get("/alerts/{id}", s.json(s.alert))
		r.Get("/halts", s.json(s.halts))
		r.Get("/incidents", s.json(s.incidents))
		r.Get("/closed-periods", s.json(s.closedPeriods))
		r.Get("/listings", s.json(s.listings))
		r.Get("/gate", s.json(s.gate))
		r.Get("/providers", s.json(s.providers))
		r.Get("/buyback", s.json(s.buyback))
		r.Get("/members", s.json(s.members))
		r.Get("/corporate-actions", s.json(s.corporateActions))
		r.Get("/disclosures", s.json(s.disclosures))
		r.Get("/calendar", s.json(s.calendar))
		r.Get("/index", s.json(s.index))
		r.Get("/complaints", s.json(s.complaints))
		r.Get("/protection", s.json(s.protection))
		r.Get("/clock", s.json(s.clock))
		r.Get("/settlement", s.json(s.settlement))
		r.Get("/recon", s.json(s.recon))

		r.Post("/market/close", s.json(s.closeMarket))
		r.Post("/instruments/{symbol}/halt", s.json(s.haltInstrument))
		r.Post("/instruments/{symbol}/release", s.json(s.releaseInstrument))
		r.Post("/instruments/{symbol}/graduate", s.json(s.graduate))
		r.Post("/instruments/{symbol}/demote", s.json(s.demote))
		r.Post("/instruments/{symbol}/shares-in-issue", s.json(s.setSharesInIssue))
		r.Post("/instruments/{symbol}/reanchor", s.json(s.reanchor))
		r.Post("/alerts/{id}/triage", s.json(s.triageAlert))
		r.Post("/alerts/{id}/close", s.json(s.closeAlert))
		r.Post("/disclosures/{id}/publish", s.json(s.publishDisclosure))
		r.Post("/members/{id}/halt", s.json(s.haltMember))
		r.Post("/members/{id}/release", s.json(s.releaseMember))
	})
	return r
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		h := r.Header.Get("Authorization")
		if len(h) <= len(prefix) || h[:len(prefix)] != prefix ||
			subtle.ConstantTimeCompare([]byte(h[len(prefix):]), []byte(s.Token)) != 1 {
			writeJSON(w, http.StatusUnauthorized, errorBody{"unauthenticated", "Credentials were not accepted"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------- plumbing

type handler func(ctx context.Context, r *http.Request) (any, error)

// apiError is a refusal with the status it deserves.
type apiError struct {
	Status  int
	Code    string
	Message string
}

func (e *apiError) Error() string { return e.Message }

func invalid(format string, args ...any) error {
	return &apiError{http.StatusBadRequest, "invalid", fmt.Sprintf(format, args...)}
}

func notFound(format string, args ...any) error {
	return &apiError{http.StatusNotFound, "not_found", fmt.Sprintf(format, args...)}
}

// refused wraps an engine's refusal. The engines return plain errors whose
// text is written for the person acting, so it is passed through.
func refused(err error) error {
	msg := err.Error()
	for _, p := range []string{"exchange: ", "institution: ", "rail: "} {
		msg = strings.TrimPrefix(msg, p)
	}
	return &apiError{http.StatusConflict, "refused", capitalise(msg)}
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (s *Server) json(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		v, err := h(ctx, r)
		if err != nil {
			var ae *apiError
			switch {
			case errors.As(err, &ae):
				writeJSON(w, ae.Status, errorBody{ae.Code, ae.Message})
			case errors.Is(err, pgx.ErrNoRows):
				writeJSON(w, http.StatusNotFound, errorBody{"not_found", "No such record"})
			case errors.Is(err, exchange.ErrHalted), errors.Is(err, exchange.ErrMarketClosed):
				writeJSON(w, http.StatusConflict, errorBody{"refused", capitalise(err.Error())})
			default:
				slog.Error("console", "path", r.URL.Path, "err", err)
				writeJSON(w, http.StatusInternalServerError, errorBody{"internal", "Something went wrong; the log has the detail"})
			}
			return
		}
		writeJSON(w, http.StatusOK, v)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func capitalise(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// operator is the name typed at sign-in, carried on every request. Actions
// refuse without it: the engine functions record who decided.
func operator(r *http.Request) (string, error) {
	by := strings.TrimSpace(r.Header.Get("X-Operator"))
	if by == "" {
		return "", invalid("An operator name is required; sign in again")
	}
	if len(by) > 80 {
		return "", invalid("The operator name is too long")
	}
	return by, nil
}

func decode(r *http.Request, v any) error {
	if r.ContentLength == 0 {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(v); err != nil {
		return invalid("The request body is not valid JSON")
	}
	return nil
}

func (s *Server) today() string { return scheme.BusinessDate(s.Now()) }

// dateParam reads a ?date= (or other named) query as a business date, or
// falls back to today.
func (s *Server) dateParam(r *http.Request, name string) (string, error) {
	d := r.URL.Query().Get(name)
	if d == "" {
		return s.today(), nil
	}
	if _, err := scheme.ParseBusinessDate(d); err != nil {
		return "", invalid("%q is not a date (YYYY-MM-DD)", d)
	}
	return d, nil
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

// ---------------------------------------------------------------- rows

// querier is what collect needs: a pool or a transaction.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

type row = map[string]any

// collect runs a query and returns each row as a map keyed by column name,
// with the values a browser can read: uuids and dates as strings, timestamps
// as RFC 3339, integers as integers. Kobo and units stay integers on the
// wire; the page formats them.
func collect(ctx context.Context, q querier, sql string, args ...any) ([]row, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("console: %w", err)
	}
	defer rows.Close()
	fds := rows.FieldDescriptions()
	out := []row{}
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, err
		}
		m := make(row, len(fds))
		for i, fd := range fds {
			m[fd.Name] = norm(fd.DataTypeOID, vals[i])
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// one runs collect and returns the single row, or pgx.ErrNoRows.
func one(ctx context.Context, q querier, sql string, args ...any) (row, error) {
	rows, err := collect(ctx, q, sql, args...)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, pgx.ErrNoRows
	}
	return rows[0], nil
}

func norm(oid uint32, v any) any {
	switch x := v.(type) {
	case [16]byte:
		return uuid.UUID(x).String()
	case time.Time:
		if oid == pgtype.DateOID {
			return x.Format("2006-01-02")
		}
		return x.UTC().Format(time.RFC3339Nano)
	case pgtype.Numeric:
		f, err := x.Float64Value()
		if err != nil || !f.Valid {
			return nil
		}
		return f.Float64
	case []any:
		for i := range x {
			x[i] = norm(0, x[i])
		}
		return x
	}
	return v
}

// ---------------------------------------------------------------- paging

// paging is ?limit=&cursor=. Lists that can grow page by keyset: the cursor
// is the last row's sort key, opaque to the page.
type paging struct {
	Limit  int
	Cursor string
}

func pageParams(r *http.Request, def int) paging {
	p := paging{Limit: def, Cursor: r.URL.Query().Get("cursor")}
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n <= 500 {
		p.Limit = n
	}
	return p
}

// idCursor is the cursor for bigserial tables: the last id seen.
func (p paging) idCursor() int64 {
	n, _ := strconv.ParseInt(p.Cursor, 10, 64)
	return n
}

// tsCursor is the cursor for uuid tables ordered by time: "ts|id".
func (p paging) tsCursor() (*time.Time, *string) {
	if p.Cursor == "" {
		return nil, nil
	}
	ts, id, ok := strings.Cut(p.Cursor, "|")
	t, err := time.Parse(time.RFC3339Nano, ts)
	if !ok || err != nil {
		return nil, nil
	}
	return &t, &id
}

type listPage struct {
	Rows []row  `json:"rows"`
	Next string `json:"next_cursor,omitempty"`
	// Extra carries whatever else a list view needs beside its rows.
	Extra row `json:"extra,omitempty"`
}

// cut trims a limit+1 fetch to the page and names the cursor for the next.
func cut(rows []row, p paging, next func(last row) string) listPage {
	out := listPage{Rows: rows}
	if len(rows) > p.Limit {
		out.Rows = rows[:p.Limit]
		out.Next = next(rows[p.Limit-1])
	}
	return out
}

func tsKey(tsCol, idCol string) func(row) string {
	return func(r row) string {
		return fmt.Sprintf("%v|%v", r[tsCol], r[idCol])
	}
}

func idKey(col string) func(row) string {
	return func(r row) string { return fmt.Sprintf("%v", r[col]) }
}

func str(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%v", v)
}
