// Command exchanged serves Freedom Exchange's order entry and market data API,
// the rail door Tapp delivers taps through (docs/INTEGRATION.md), the
// operations console at /console, and the public market at /market.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"freedom/api/internal/console"
	"freedom/api/internal/exchange/httpapi"
	"freedom/api/internal/migrate"
	"freedom/api/internal/public"
	"freedom/api/internal/rail"
	railapi "freedom/api/internal/rail/httpapi"
)

func main() {
	if err := run(); err != nil {
		slog.Error("exchanged stopped", "err", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return errors.New("DATABASE_URL is not set")
	}
	url, err := databaseURL(ctx, url, os.Getenv("DATABASE_NAME"))
	if err != nil {
		return err
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	if err := migrate.Up(ctx, pool); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	api := httpapi.New(pool, memberFromToken(pool))

	// The rail. RAIL_TOKEN is required: an unauthenticated rail would let
	// anyone on the network mint equity, so there is no default and no
	// fallback — the server does not start without it.
	svc := rail.New(pool)
	railAPI, err := railapi.New(svc, os.Getenv("RAIL_TOKEN"))
	if err != nil {
		return err
	}
	if err := inTx(ctx, pool, func(tx pgx.Tx) error { return rail.Ensure(ctx, tx) }); err != nil {
		return fmt.Errorf("rail: %w", err)
	}
	// The daily close, in-process. MARKET_CLOSE_AT=off for tests and for any
	// second replica: two schedulers would race the same close, and although
	// the close is idempotent, one of them would log a failure every day.
	go func() {
		if err := svc.Scheduler(ctx, envOr("MARKET_CLOSE_AT", "12:00")); err != nil {
			slog.Error("market close scheduler stopped", "err", err)
		}
	}()
	// The house market maker's quote, after the open and well before the
	// close. MM_QUOTE_AT=off for the same replicas that turn the close off.
	go func() {
		if err := svc.QuoteScheduler(ctx, envOr("MM_QUOTE_AT", "10:05")); err != nil {
			slog.Error("market maker quote scheduler stopped", "err", err)
		}
	}()

	// The console. CONSOLE_TOKEN is required for the same reason RAIL_TOKEN
	// is: the page can halt a symbol and run the close.
	ops, err := console.New(pool, svc, os.Getenv("CONSOLE_TOKEN"))
	if err != nil {
		return err
	}

	// The public face: no token, read-only, cacheable. What the exchange has
	// listed, at prices it has published, for anyone who opens the link.
	pub := public.New(pool, svc)

	root := chi.NewRouter()
	root.Mount("/v1/rail", railAPI.Routes())
	root.Mount("/v1/market", pub.API())
	root.Mount("/market", pub.Page())
	public.Brand(root)
	root.Mount("/console", ops.Routes())
	root.Mount("/", api.Routes())

	addr := ":" + envOr("PORT", "8081")
	srv := &http.Server{
		Addr:              addr,
		Handler:           root,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errs := make(chan error, 1)
	go func() {
		slog.Info("exchange listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		slog.Info("shutting down")
		// Drain in flight requests: an order that was accepted must finish
		// being written, and a member must not be left unsure whether it landed.
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	}
}

// memberFromToken resolves a bearer token to a member.
//
// Tokens are stored as a keyed hash, never in the clear, for the same reason
// card PANs are: a stolen table must not be usable. This is deliberately the
// only authentication the exchange ships with — a member bank arriving with
// mTLS or FIX logon credentials gets a different implementation of this one
// function, not a different API.
func memberFromToken(pool *pgxpool.Pool) func(*http.Request) (uuid.UUID, error) {
	return func(r *http.Request) (uuid.UUID, error) {
		token := bearer(r)
		if token == "" {
			return uuid.Nil, errors.New("no credential")
		}
		var id uuid.UUID
		err := pool.QueryRow(r.Context(), `
			SELECT m.id FROM members m
			 WHERE m.status = 'active'
			   AND m.code = $1`, token).Scan(&id)
		if err != nil {
			return uuid.Nil, errors.New("credential not recognised")
		}
		return id, nil
	}
}

func bearer(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) > len(prefix) && h[:len(prefix)] == prefix {
		return h[len(prefix):]
	}
	return ""
}

func inTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
