package console

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"freedom/api/internal/exchange"
	"freedom/api/internal/institution"
	"freedom/api/internal/scheme"
)

// Actions. Each is one transaction around one engine function, and each
// returns the row it changed so the page can show the result rather than
// guess it. The operator's name travels as `by` wherever the engine keeps one.

func (s *Server) closeMarket(ctx context.Context, r *http.Request) (any, error) {
	by, err := operator(r)
	if err != nil {
		return nil, err
	}
	var body struct {
		SessionDate string `json:"session_date"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	if body.SessionDate != "" {
		if _, err := scheme.ParseBusinessDate(body.SessionDate); err != nil {
			return nil, invalid("%q is not a date (YYYY-MM-DD)", body.SessionDate)
		}
	}
	// The close is the rail's routine: one transaction per instrument, so
	// one symbol's failure is a row, not a rollback of the others.
	rows, err := s.Rail.CloseMarket(ctx, body.SessionDate)
	if err != nil {
		if errors.Is(err, exchange.ErrMarketClosed) {
			return nil, refused(err)
		}
		return nil, err
	}
	return row{"by": by, "session_date": body.SessionDate, "instruments": rows}, nil
}

// instrumentID resolves a symbol inside the transaction, so a symbol that
// vanished mid-request is a 404 rather than a foreign-key error.
func instrumentID(ctx context.Context, tx pgx.Tx, symbol string) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT id FROM instruments WHERE symbol = $1 FOR UPDATE`, symbol).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", notFound("No instrument %s", symbol)
	}
	return id, err
}

func (s *Server) instrumentRow(ctx context.Context, tx pgx.Tx, symbol string) (row, error) {
	return one(ctx, tx, instrumentSelect+` WHERE i.symbol = $2`, s.today(), symbol)
}

func (s *Server) haltInstrument(ctx context.Context, r *http.Request) (any, error) {
	by, err := operator(r)
	if err != nil {
		return nil, err
	}
	var body struct {
		Reason string `json:"reason"`
		Detail string `json:"detail"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	symbol := strings.ToUpper(chi.URLParam(r, "symbol"))
	var out row
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		id, err := instrumentID(ctx, tx, symbol)
		if err != nil {
			return err
		}
		detail := map[string]any{}
		if body.Detail != "" {
			detail["note"] = body.Detail
		}
		if _, err := exchange.Halt(ctx, tx, id, body.Reason, by, detail); err != nil {
			if strings.Contains(err.Error(), "not a halt reason") {
				return invalid("%q is not a halt reason", body.Reason)
			}
			if strings.Contains(err.Error(), "trading_halts_one_open") {
				return &apiError{http.StatusConflict, "refused", symbol + " is already halted"}
			}
			return refused(err)
		}
		out, err = s.instrumentRow(ctx, tx, symbol)
		return err
	})
	return out, err
}

func (s *Server) releaseInstrument(ctx context.Context, r *http.Request) (any, error) {
	by, err := operator(r)
	if err != nil {
		return nil, err
	}
	symbol := strings.ToUpper(chi.URLParam(r, "symbol"))
	var out row
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		id, err := instrumentID(ctx, tx, symbol)
		if err != nil {
			return err
		}
		if err := exchange.Release(ctx, tx, id, by); err != nil {
			return refused(err)
		}
		out, err = s.instrumentRow(ctx, tx, symbol)
		return err
	})
	return out, err
}

// graduate and demote both assess first. Graduate refuses with the failures
// when the report says no; demote records the report it was decided on.
func (s *Server) graduate(ctx context.Context, r *http.Request) (any, error) {
	return s.review(ctx, r, true)
}

func (s *Server) demote(ctx context.Context, r *http.Request) (any, error) {
	return s.review(ctx, r, false)
}

func (s *Server) review(ctx context.Context, r *http.Request, up bool) (any, error) {
	by, err := operator(r)
	if err != nil {
		return nil, err
	}
	symbol := strings.ToUpper(chi.URLParam(r, "symbol"))
	var out row
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		id, err := instrumentID(ctx, tx, symbol)
		if err != nil {
			return err
		}
		report, err := s.Gate.Assess(ctx, tx, id, s.today())
		if err != nil {
			return err
		}
		if up {
			if !report.Passes {
				return &apiError{http.StatusUnprocessableEntity, "gate_failed",
					symbol + " does not meet the liquidity standard: " + strings.Join(report.Failures, "; ")}
			}
			if err := exchange.Graduate(ctx, tx, report, by); err != nil {
				return refused(err)
			}
		} else if err := exchange.Demote(ctx, tx, report, by); err != nil {
			return refused(err)
		}
		inst, err := s.instrumentRow(ctx, tx, symbol)
		if err != nil {
			return err
		}
		out = row{"instrument": inst, "liquidity": s.gateReport(report, inst["clob_enabled"] == true)}
		return nil
	})
	return out, err
}

func (s *Server) triageAlert(ctx context.Context, r *http.Request) (any, error) {
	by, err := operator(r)
	if err != nil {
		return nil, err
	}
	id, err := alertID(r)
	if err != nil {
		return nil, err
	}
	var body struct {
		Assignee string `json:"assignee"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	if body.Assignee == "" {
		body.Assignee = by
	}
	var out row
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if err := exchange.Triage(ctx, tx, id, body.Assignee); err != nil {
			return refused(err)
		}
		var err error
		out, err = one(ctx, tx, alertSelect+` WHERE a.id = $1`, id)
		return err
	})
	return out, err
}

func (s *Server) closeAlert(ctx context.Context, r *http.Request) (any, error) {
	by, err := operator(r)
	if err != nil {
		return nil, err
	}
	id, err := alertID(r)
	if err != nil {
		return nil, err
	}
	var body struct {
		Outcome string `json:"outcome"`
		Notes   string `json:"notes"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	var out row
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if err := exchange.Close(ctx, tx, id, body.Outcome, by, body.Notes); err != nil {
			if strings.Contains(err.Error(), "not an alert outcome") {
				return invalid("%q is not an alert outcome", body.Outcome)
			}
			return refused(err)
		}
		var err error
		out, err = one(ctx, tx, alertSelect+` WHERE a.id = $1`, id)
		return err
	})
	return out, err
}

func (s *Server) publishDisclosure(ctx context.Context, r *http.Request) (any, error) {
	by, err := operator(r)
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		return nil, invalid("Disclosure ids are uuids")
	}
	var out row
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if err := institution.Publish(ctx, tx, id, by); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return notFound("No disclosure %s", id)
			}
			return refused(err)
		}
		var err error
		out, err = one(ctx, tx, disclosureSelect+` WHERE d.id = $1`, id)
		return err
	})
	return out, err
}

func (s *Server) haltMember(ctx context.Context, r *http.Request) (any, error) {
	return s.memberSwitch(ctx, r, true)
}

func (s *Server) releaseMember(ctx context.Context, r *http.Request) (any, error) {
	return s.memberSwitch(ctx, r, false)
}

// memberSwitch is the kill switch. The engine's HaltMember/ResumeMember do
// not record who threw it, so the reason carries the operator's name.
func (s *Server) memberSwitch(ctx context.Context, r *http.Request, halt bool) (any, error) {
	by, err := operator(r)
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		return nil, invalid("Member ids are uuids")
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	var out row
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM members WHERE id = $1 FOR UPDATE)`, id).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return notFound("No member %s", id)
		}
		if halt {
			if strings.TrimSpace(body.Reason) == "" {
				return invalid("Halting a member requires a reason")
			}
			if err := exchange.HaltMember(ctx, tx, id, body.Reason+" — by "+by); err != nil {
				return refused(err)
			}
		} else if err := exchange.ResumeMember(ctx, tx, id); err != nil {
			return refused(err)
		}
		var err error
		out, err = one(ctx, tx, `
			SELECT m.id::text AS id, m.code, m.legal_name, m.roles, m.status, m.halted, m.halted_reason,
			       m.max_orders_per_session, m.max_order_to_trade_ratio, m.created_at
			  FROM members m WHERE m.id = $1`, id)
		return err
	})
	return out, err
}

// setSharesInIssue corrects the company's declared share count.
//
// The count is what the company is valued on. It is declared at admission
// and changes with issuance, so an operator may correct it — but never
// silently: the change is a cap table event carrying the operator's name
// and the reason, like every other movement in the register.
func (s *Server) setSharesInIssue(ctx context.Context, r *http.Request) (any, error) {
	by, err := operator(r)
	if err != nil {
		return nil, err
	}
	var body struct {
		Units  int64  `json:"units"`
		Reason string `json:"reason"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	if body.Units <= 0 {
		return nil, invalid("shares in issue must be a positive number of units")
	}
	if strings.TrimSpace(body.Reason) == "" {
		return nil, invalid("a reason is required")
	}
	symbol := strings.ToUpper(chi.URLParam(r, "symbol"))
	var out row
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		id, err := instrumentID(ctx, tx, symbol)
		if err != nil {
			return err
		}
		var before int64
		if err := tx.QueryRow(ctx, `
			UPDATE instruments SET shares_in_issue_units = $2 WHERE id = $1
			RETURNING (SELECT shares_in_issue_units FROM instruments WHERE id = $1)`, id, body.Units).
			Scan(&before); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO cap_table_events (instrument_id, kind, units_delta, note)
			VALUES ($1, 'authorised', $2, $3)`,
			id, body.Units-before, fmt.Sprintf("shares in issue set to %d by %s: %s", body.Units, by, body.Reason)); err != nil {
			return err
		}
		out, err = s.instrumentRow(ctx, tx, symbol)
		return err
	})
	return out, err
}
