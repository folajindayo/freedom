// Package db owns the connection pools and the rules about which one may run
// which query.
//
// # Two roles, on purpose
//
// Row-level security is the mechanism that stops one acquirer on the network
// seeing another's traffic, and it is worth nothing if every binary connects as
// the same Postgres role. Ọja therefore has two:
//
//	oja_participant  RLS enforced. The acquirer API and the member portal.
//	oja_scheme       BYPASSRLS. The switch, clearing, and the auction engine,
//	                 which are inherently cross-participant and cannot work
//	                 through a policy that hides half the network from them.
//
// This is a deployment decision as much as a code one — the two roles need two
// connection strings — and it has to be made before any participant data exists.
// Retrofitting it onto a live network means re-issuing every credential.
package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type DB struct {
	// Scheme is the BYPASSRLS pool. Every cross-participant job uses it.
	Scheme *pgxpool.Pool
	// Participant is the RLS-enforced pool. Every request carrying a
	// participant identity uses it, through WithParticipantTx.
	Participant *pgxpool.Pool
}

// Open connects both pools. schemeURL and participantURL may point at the same
// database — they must not authenticate as the same role.
func Open(ctx context.Context, schemeURL, participantURL string) (*DB, error) {
	scheme, err := open(ctx, schemeURL)
	if err != nil {
		return nil, fmt.Errorf("scheme pool: %w", err)
	}
	participant, err := open(ctx, participantURL)
	if err != nil {
		scheme.Close()
		return nil, fmt.Errorf("participant pool: %w", err)
	}
	return &DB{Scheme: scheme, Participant: participant}, nil
}

func open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}

func (d *DB) Close() {
	if d.Participant != nil {
		d.Participant.Close()
	}
	if d.Scheme != nil {
		d.Scheme.Close()
	}
}

// WithParticipantTx opens a transaction on the RLS-enforced pool and pins
// app.current_participant for its duration. This is the only approved way to
// run a query on behalf of a member bank.
//
// SET LOCAL is connection-safe because it resets when the transaction ends,
// which is what makes this correct against a pool rather than a dedicated
// connection.
func (d *DB) WithParticipantTx(ctx context.Context, participantID string, fn func(pgx.Tx) error) error {
	return withTx(ctx, d.Participant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`SELECT set_config('app.current_participant', $1, true)`, participantID); err != nil {
			return fmt.Errorf("set participant: %w", err)
		}
		return fn(tx)
	})
}

// WithSchemeTx opens a transaction on the BYPASSRLS pool. Use it for the
// switch, clearing, settlement and the auction engine — never for a request
// whose authority comes from a participant's own credentials.
func (d *DB) WithSchemeTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return withTx(ctx, d.Scheme, fn)
}

func withTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
