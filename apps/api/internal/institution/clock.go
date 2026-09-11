package institution

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Clock integrity.
//
// Every audit-trail claim the exchange makes rests on its clock. Sequencing is
// the entire case in a manipulation investigation — who placed what, and in
// which order — and a venue that cannot show its time was traceable cannot
// defend a sequence of events at all.
//
// The check is deliberately simple and deliberately recorded: a drifting clock
// that nobody wrote down is indistinguishable afterwards from a clock that was
// fine.

// MaxDrift is the tolerance. Beyond this the venue should not be sequencing
// orders, because two events a few milliseconds apart can no longer be ordered
// with confidence.
const MaxDrift = 100 * time.Millisecond

// CheckClock records the offset between the venue's clock and a reference.
func CheckClock(ctx context.Context, tx pgx.Tx, source string, offset time.Duration) (bool, error) {
	if source == "" {
		return false, fmt.Errorf("institution: a clock check needs a named source")
	}
	within := offset < MaxDrift && offset > -MaxDrift
	_, err := tx.Exec(ctx, `
		INSERT INTO clock_checks (source, offset_ms, within_bounds)
		VALUES ($1,$2,$3)`, source, offset.Milliseconds(), within)
	if err != nil {
		return false, fmt.Errorf("institution: record clock check: %w", err)
	}
	return within, nil
}

// ClockHealthy reports whether the most recent check was within tolerance.
//
// A venue that has never checked is not healthy. Absence of evidence is not
// evidence of a good clock, and defaulting to "fine" is how a silent failure
// stays silent.
func ClockHealthy(ctx context.Context, tx pgx.Tx) (bool, error) {
	var within bool
	err := tx.QueryRow(ctx, `
		SELECT within_bounds FROM clock_checks ORDER BY checked_at DESC, id DESC LIMIT 1`).
		Scan(&within)
	if err != nil {
		return false, nil
	}
	return within, nil
}
