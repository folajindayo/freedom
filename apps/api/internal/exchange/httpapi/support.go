package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"freedom/api/internal/exchange"
)

// errBadRequest marks a caller mistake, as opposed to a rejection by a rule.
var errBadRequest = errors.New("bad request")

// errorBody is what a caller gets when something is refused.
//
// `code` is stable and machine-readable; `message` is written for the person
// who has to act on it and says what to do, not what went wrong internally.
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

// writeError maps a domain refusal to a status and a message the caller can act
// on.
//
// Every case here is a rule the exchange enforces, so each gets its own code —
// a member whose order was refused for insufficient funds needs to know that,
// and a member whose order was refused because they are halted needs to stop
// sending. Collapsing them all to 400 would make the API unusable.
func writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, exchange.ErrNotAMember):
		fail(w, http.StatusForbidden, "not_a_member",
			"This firm is not an active member of the exchange")
	case errors.Is(err, exchange.ErrMemberHalted):
		fail(w, http.StatusForbidden, "member_halted",
			"Order entry is suspended for this firm. Contact the exchange.")
	case errors.Is(err, exchange.ErrThrottled):
		fail(w, http.StatusTooManyRequests, "throttled",
			"This firm has exceeded its limits for today's session")
	case errors.Is(err, exchange.ErrMarketClosed):
		fail(w, http.StatusConflict, "market_closed", capitalise(err.Error()))
	case errors.Is(err, exchange.ErrHalted):
		fail(w, http.StatusConflict, "instrument_halted",
			"Trading in this instrument is halted")
	case errors.Is(err, exchange.ErrClosedPeriod):
		fail(w, http.StatusForbidden, "closed_period",
			"This account may not trade this instrument during a closed period")
	case errors.Is(err, exchange.ErrRelatedParty):
		fail(w, http.StatusForbidden, "related_party",
			"Related parties may not trade this instrument")
	case errors.Is(err, exchange.ErrInsufficientFunds):
		fail(w, http.StatusUnprocessableEntity, "insufficient_funds",
			"There is not enough available cash to cover this order")
	case errors.Is(err, exchange.ErrInsufficientShares):
		fail(w, http.StatusUnprocessableEntity, "insufficient_shares",
			"There are not enough transferable shares to cover this order. "+
				"Shares earned from spending are locked until their chargeback window closes.")
	case errors.Is(err, exchange.ErrBelowMinimum):
		fail(w, http.StatusUnprocessableEntity, "below_minimum", capitalise(err.Error()))
	case errors.Is(err, exchange.ErrFatFinger):
		fail(w, http.StatusUnprocessableEntity, "order_too_large", capitalise(err.Error()))
	case errors.Is(err, errBadRequest), errors.Is(err, errNotFound):
		if errors.Is(err, errNotFound) {
			fail(w, http.StatusNotFound, "not_found", capitalise(err.Error()))
			return
		}
		fail(w, http.StatusBadRequest, "malformed", capitalise(err.Error()))
	case errors.Is(err, pgx.ErrNoRows):
		fail(w, http.StatusNotFound, "not_found", "Not found")
	default:
		// Validation errors from the engine are the caller's problem and are
		// safe to echo; anything else is ours and is not.
		if strings.HasPrefix(err.Error(), "exchange: ") {
			fail(w, http.StatusUnprocessableEntity, "rejected", capitalise(err.Error()))
			return
		}
		fail(w, http.StatusInternalServerError, "internal",
			"The exchange could not process that request")
	}
}

var errNotFound = errors.New("not found")

func capitalise(s string) string {
	s = strings.TrimPrefix(s, "exchange: ")
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func inTx(r *http.Request, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	ctx := r.Context()
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

func resolveSymbol(r *http.Request, tx pgx.Tx, symbol string) (string, error) {
	if symbol == "" {
		return "", fmt.Errorf("%w: symbol is required", errBadRequest)
	}
	var id string
	err := tx.QueryRow(r.Context(),
		`SELECT id FROM instruments WHERE symbol = $1 AND status = 'listed'`,
		strings.ToUpper(symbol)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: no listed instrument %q", errNotFound, symbol)
	}
	return id, err
}

// resolveClient checks that the client account belongs to the member making the
// request, and returns the cardholder and the ledger account to trade from.
//
// The member scoping is the point: without it, any member could trade any
// client's account by guessing an id.
func resolveClient(r *http.Request, tx pgx.Tx, member, client uuid.UUID, instrumentID string) (
	holder uuid.UUID, ledgerAcct uuid.UUID, err error) {

	var cardholder *uuid.UUID
	var status string
	err = tx.QueryRow(r.Context(), `
		SELECT cardholder_id, status FROM client_accounts
		 WHERE id = $1 AND member_id = $2`, client, member).Scan(&cardholder, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, uuid.Nil, fmt.Errorf("%w: no such client account", errNotFound)
	}
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	if status != "active" {
		return uuid.Nil, uuid.Nil, fmt.Errorf("%w: client account is %s", errBadRequest, status)
	}
	if cardholder == nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("%w: that client account is not a cardholder", errBadRequest)
	}

	acct, err := exchange.TradingAccount(r.Context(), tx, *cardholder, instrumentID)
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	return *cardholder, acct, nil
}

type ctxKey int

const memberCtxKey ctxKey = 0

func withMember(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, memberCtxKey, id)
}

func memberFrom(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(memberCtxKey).(uuid.UUID)
	return id, ok
}
