// Package migrate applies schema migrations from inside the binary.
//
// Embedding them means the schema travels with the code that expects it, and a
// deployment cannot start against a database it predates. Applying on boot is
// safe because it is guarded by an advisory lock: when several instances start
// at once, one applies and the others wait and then find nothing to do.
//
// Forked from Tender, with two corrections that matter more here than there —
// see the comments on errNotApplied and on the recorded INSERT below. A card
// scheme that wedges its own boot sequence is an outage across every terminal
// on the network at once.
package migrate

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed sql/*.sql
var files embed.FS

// lockID is arbitrary; it only has to be the same in every instance.
const lockID = 0x0A_1E_D6E4

// Up applies every migration that has not been applied yet, in filename order.
func Up(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	// Serialise across instances. Two containers booting together would
	// otherwise both try to add the same column.
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, lockID); err != nil {
		return fmt.Errorf("take migration lock: %w", err)
	}
	defer conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, lockID)

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			filename   text PRIMARY KEY,
			applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("create migrations table: %w", err)
	}

	entries, err := files.ReadDir("sql")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		var one int
		err := conn.QueryRow(ctx,
			`SELECT 1 FROM schema_migrations WHERE filename = $1`, name).Scan(&one)
		switch {
		case err == nil:
			continue // already applied
		case errors.Is(err, pgx.ErrNoRows):
			// Not applied yet. Fall through and apply it.
		default:
			// Anything else — a connection blip, a permission error — must stop
			// the boot. Treating it as "not applied" re-runs the file, which
			// then fails on an object that already exists and wedges every
			// future boot until someone edits the database by hand.
			return fmt.Errorf("check migration %s: %w", name, err)
		}

		body, err := files.ReadFile("sql/" + name)
		if err != nil {
			return err
		}
		slog.Info("applying migration", "file", name)

		// The bookkeeping INSERT is appended to the migration body so that it
		// commits inside the file's own BEGIN/COMMIT. Issued separately, a crash
		// in the gap between them leaves a schema that is applied but unrecorded,
		// and the next boot re-runs it and fails forever.
		//
		// Deliberately not wrapped in one transaction here: some migrations use
		// ALTER TYPE ... ADD VALUE, which Postgres refuses inside a transaction
		// block that later uses the new value. Each file manages its own.
		sql := string(body) + fmt.Sprintf(
			"\nINSERT INTO schema_migrations (filename) VALUES (%s);\n", quote(name))
		if _, err := conn.Exec(ctx, sql); err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
	}
	return nil
}

// quote renders a SQL string literal. Migration filenames are compiled into the
// binary by //go:embed and cannot carry user input, but the statement is built
// by concatenation, so it does not get to rely on that.
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
