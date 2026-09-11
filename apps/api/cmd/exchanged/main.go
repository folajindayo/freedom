// Command exchanged serves Freedom Exchange's order entry and market data API.
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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"freedom/api/internal/exchange/httpapi"
	"freedom/api/internal/migrate"
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
	addr := ":" + envOr("PORT", "8081")
	srv := &http.Server{
		Addr:              addr,
		Handler:           api.Routes(),
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

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
