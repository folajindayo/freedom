package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"

	"github.com/jackc/pgx/v5"
)

// databaseURL resolves where exchanged keeps its data.
//
// On a hosted Postgres shared with another system (Tapp's API shares the
// Railway instance), DATABASE_URL points at that system's database, and
// both codebases create tables with the same names — `ledger_entries`,
// `accounts` — so they cannot share a schema. DATABASE_NAME names a separate
// database on the same instance; exchanged creates it on first boot if it is
// missing and connects there. The alternative is a manual `CREATE DATABASE`
// that somebody has to remember before the first deploy, which is exactly the
// kind of step that gets skipped at 3am.
//
// Unset, the URL is used as given, which is what local development wants.
func databaseURL(ctx context.Context, raw, name string) (string, error) {
	if name == "" {
		return raw, nil
	}
	if !validDatabaseName(name) {
		return "", fmt.Errorf("DATABASE_NAME %q: use letters, digits and underscores only", name)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("DATABASE_URL: %w", err)
	}
	if err := ensureDatabase(ctx, raw, name); err != nil {
		return "", err
	}
	u.Path = "/" + name
	return u.String(), nil
}

var databaseNameRE = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

func validDatabaseName(s string) bool { return databaseNameRE.MatchString(s) }

// ensureDatabase creates `name` on the instance behind `admin` if absent.
// CREATE DATABASE cannot run inside a transaction, and two replicas booting
// at once could race it, so a duplicate is treated as success.
func ensureDatabase(ctx context.Context, admin, name string) error {
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		return fmt.Errorf("connect to create %s: %w", name, err)
	}
	defer conn.Close(ctx)

	var exists bool
	if err := conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, name).Scan(&exists); err != nil {
		return fmt.Errorf("check database %s: %w", name, err)
	}
	if exists {
		return nil
	}
	// The name is validated above, so quoting it as an identifier is safe.
	_, err = conn.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s`, pgx.Identifier{name}.Sanitize()))
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) && pgErr.SQLState() == "42P04" { // duplicate_database
		return nil
	}
	if err != nil {
		return fmt.Errorf("create database %s: %w", name, err)
	}
	return nil
}
