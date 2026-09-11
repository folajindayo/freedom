package exchange

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"freedom/api/internal/money"
)

// Halts and circuit breakers.
//
// A halt is the market saying it does not currently know the price. That is a
// legitimate thing for a market to say, and saying it is far better than
// printing a number nobody believes — every downstream consumer of the price,
// the buyback most of all, is better served by "unavailable" than by "wrong".

// Halt reasons.
const (
	HaltVolatility  = "volatility"
	HaltNewsPending = "news_pending"
	HaltRegulatory  = "regulatory"
	HaltDataQuality = "data_quality"
	HaltIssuer      = "issuer_request"
)

// ErrHalted is returned by anything that will not operate on a halted symbol.
var ErrHalted = errors.New("exchange: instrument is halted")

// Halt suspends trading in an instrument.
//
// Only one halt is open at a time, enforced by a partial unique index: two
// overlapping halts would mean two people each believing they could lift it.
func Halt(ctx context.Context, tx pgx.Tx, instrumentID, reason, by string, detail map[string]any) (uuid.UUID, error) {
	switch reason {
	case HaltVolatility, HaltNewsPending, HaltRegulatory, HaltDataQuality, HaltIssuer:
	default:
		return uuid.Nil, fmt.Errorf("exchange: %q is not a halt reason", reason)
	}
	if detail == nil {
		detail = map[string]any{}
	}
	d, err := json.Marshal(detail)
	if err != nil {
		return uuid.Nil, err
	}

	var id uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO trading_halts (instrument_id, reason, raised_by, detail)
		VALUES ($1,$2,$3,$4) RETURNING id`, instrumentID, reason, by, d).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("exchange: halt %s: %w", instrumentID, err)
	}
	return id, nil
}

// Release lifts a halt. It carries the name of whoever decided the market is
// ready to price the instrument again.
func Release(ctx context.Context, tx pgx.Tx, instrumentID, by string) error {
	ct, err := tx.Exec(ctx, `
		UPDATE trading_halts SET released_at = now(), released_by = $2
		 WHERE instrument_id = $1 AND released_at IS NULL`, instrumentID, by)
	if err != nil {
		return fmt.Errorf("exchange: release halt: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("exchange: %s is not halted", instrumentID)
	}
	return nil
}

// CheckBand reports whether an uncrossed price is outside the band, and records
// an incident when it is.
//
// A price outside the band is NOT clamped into it. Clamping publishes a price
// nobody bid, which is the same failure the uncross itself refuses when it will
// not print at a band edge. The session is abandoned, the reference carries
// forward, and an incident is raised for someone to look at.
//
// It returns a flag rather than an error, deliberately. An error would abort
// the caller's transaction and take the incident record down with it — the
// breaker would trip every session, forever, and leave no trace of having done
// so. Refusing is an outcome, not a failure.
//
// Because limits are band-checked at order entry, this can only fire when the
// band moved during the session or the reference was corrected — both of which
// are exactly the circumstances where a human should be involved.
func CheckBand(ctx context.Context, tx pgx.Tx, instrumentID string, p Params, price money.Kobo) (bool, error) {
	if price >= p.BandLo && price <= p.BandHi {
		return false, nil
	}
	detail, _ := json.Marshal(map[string]any{
		"clearing_price": int64(price),
		"band_lo":        int64(p.BandLo),
		"band_hi":        int64(p.BandHi),
		"reference":      int64(p.PrevRef),
	})
	if _, err := tx.Exec(ctx, `
		INSERT INTO data_quality_incidents (instrument_id, kind, detail)
		VALUES ($1,'band_breach',$2)`, instrumentID, detail); err != nil {
		return false, fmt.Errorf("exchange: record band breach: %w", err)
	}
	return true, nil
}

// CheckVolatility trips the circuit breaker on an unusually large move.
//
// The band already rejects prices outside a hard range. This is the narrower
// question: a move that is technically in-band but far enough from the
// reference that it should stop the market rather than set tomorrow's price —
// and, through the reference, tomorrow's buyback price.
//
// A tripped breaker halts the instrument and abandons the session. Like
// CheckBand it returns a flag rather than an error, so the halt it just
// recorded survives the caller's commit.
func CheckVolatility(ctx context.Context, tx pgx.Tx, instrumentID string, ref, price money.Kobo) (bool, error) {
	var limitBps int64
	if err := tx.QueryRow(ctx,
		`SELECT halt_move_bps FROM instruments WHERE id = $1`, instrumentID).Scan(&limitBps); err != nil {
		return false, fmt.Errorf("exchange: volatility limit: %w", err)
	}
	if ref <= 0 || limitBps <= 0 {
		return false, nil
	}
	move := price - ref
	if move < 0 {
		move = -move
	}
	bps := int64(move) * 10_000 / int64(ref)
	if bps < limitBps {
		return false, nil
	}

	if _, err := Halt(ctx, tx, instrumentID, HaltVolatility, "circuit_breaker", map[string]any{
		"reference": int64(ref), "clearing_price": int64(price),
		"move_bps": bps, "limit_bps": limitBps,
	}); err != nil {
		return false, err
	}
	return true, nil
}
