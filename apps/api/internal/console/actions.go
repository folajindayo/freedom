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
	"freedom/api/internal/money"
	"freedom/api/internal/rail"
	"freedom/api/internal/scheme"
	"freedom/api/internal/share"
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

// reanchor re-prices a listing admitted before the pricing rule
// (LISTING-RULES §2.4) under it. The exchange decides the number; the
// console carries the audited figures, the operator's name and the reason.
func (s *Server) reanchor(ctx context.Context, r *http.Request) (any, error) {
	by, err := operator(r)
	if err != nil {
		return nil, err
	}
	var body struct {
		NetAssetsKobo int64  `json:"net_assets_kobo"`
		RevenueKobo   int64  `json:"revenue_kobo"`
		Reason        string `json:"reason"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	if body.NetAssetsKobo <= 0 || body.RevenueKobo <= 0 {
		return nil, invalid("net assets and revenue must both be positive, in kobo, from the audited accounts")
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
		_, err = exchange.Reanchor(ctx, tx, id, s.today(), exchange.StandardCriteria(),
			money.Kobo(body.NetAssetsKobo), money.Kobo(body.RevenueKobo), by, body.Reason)
		switch {
		case errors.Is(err, exchange.ErrNoSharesInIssue):
			return &apiError{http.StatusUnprocessableEntity, "unpriceable", "Set shares in issue first"}
		case errors.Is(err, exchange.ErrSessionOpen):
			return &apiError{http.StatusConflict, "refused", "Re-anchor after the close"}
		case err != nil:
			return refused(err)
		}
		out, err = s.instrumentRow(ctx, tx, symbol)
		return err
	})
	return out, err
}

// ---------------------------------------------------------------- market maker

// quoteMarket runs the quoting engine for a date, the way the 10:05
// scheduler does. Re-running it is a no-op for quotes already in the book.
func (s *Server) quoteMarket(ctx context.Context, r *http.Request) (any, error) {
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
	rows, err := s.Rail.QuoteMarket(ctx, body.SessionDate)
	if err != nil {
		if errors.Is(err, rail.ErrInvalid) {
			return nil, invalid("%s", strings.TrimPrefix(err.Error(), "rail: invalid request: "))
		}
		return nil, err
	}
	date := body.SessionDate
	if date == "" {
		date = s.today()
	}
	return row{"by": by, "session_date": date, "quotes": rows}, nil
}

// fundMember moves capital from the scheme's float to a member's
// market-making account. Cash, so a ledger transaction and nothing else.
func (s *Server) fundMember(ctx context.Context, r *http.Request) (any, error) {
	by, err := operator(r)
	if err != nil {
		return nil, err
	}
	var body struct {
		AmountKobo int64  `json:"amount_kobo"`
		Reason     string `json:"reason"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	if body.AmountKobo <= 0 {
		return nil, invalid("amount_kobo must be positive")
	}
	if strings.TrimSpace(body.Reason) == "" {
		return nil, invalid("a reason is required")
	}
	code := strings.ToUpper(chi.URLParam(r, "code"))
	var out rail.Funding
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		out, err = s.Rail.FundMarketMaker(ctx, tx, code, money.Kobo(body.AmountKobo), s.today(), by, body.Reason)
		switch {
		case errors.Is(err, rail.ErrNotFound):
			return notFound("%s", capitalise(strings.TrimPrefix(err.Error(), "rail: not found: ")))
		case errors.Is(err, rail.ErrInvalid):
			return invalid("%s", strings.TrimPrefix(err.Error(), "rail: invalid request: "))
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return row{"by": by, "member_code": out.MemberCode, "amount_kobo": int64(out.AmountKobo),
		"ledger_tx_id": out.LedgerTx.String(), "available_kobo": int64(out.Available)}, nil
}

// placeWithMarketMaker sells a block from the company's treasury to the
// house market maker at the current reference.
func (s *Server) placeWithMarketMaker(ctx context.Context, r *http.Request) (any, error) {
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
		return nil, invalid("units must be a positive number of share units")
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
		var ref int64
		if err := tx.QueryRow(ctx, `SELECT reference_price_kobo FROM instruments WHERE id = $1`, id).Scan(&ref); err != nil {
			return err
		}
		placed, err := s.Rail.PlaceWithMarketMaker(ctx, tx, id, share.Units(body.Units), money.Kobo(ref), s.today(), by, body.Reason)
		if errors.Is(err, rail.ErrInvalid) {
			return &apiError{http.StatusConflict, "refused", capitalise(strings.TrimPrefix(err.Error(), "rail: invalid request: "))}
		}
		if err != nil {
			return err
		}
		inst, err := s.instrumentRow(ctx, tx, symbol)
		if err != nil {
			return err
		}
		out = row{"instrument": inst, "placement": row{
			"symbol": placed.Symbol, "units": int64(placed.Units), "price_kobo": int64(placed.PriceKobo),
			"cost_kobo": int64(placed.CostKobo), "ledger_tx_id": placed.LedgerTx.String(), "held_units": int64(placed.Held)}}
		return nil
	})
	return out, err
}

// appointMarketMaker registers a member as the symbol's designated market
// maker on the standard obligation, with the numbers overridable. The
// engine refuses a related party.
func (s *Server) appointMarketMaker(ctx context.Context, r *http.Request) (any, error) {
	by, err := operator(r)
	if err != nil {
		return nil, err
	}
	var body struct {
		MemberCode   string `json:"member_code"`
		MinQuoteKobo int64  `json:"min_quote_kobo"`
		MaxSpreadBps int64  `json:"max_spread_bps"`
		TargetUnits  int64  `json:"target_units"`
		From         string `json:"from"`
		To           string `json:"to"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	code := strings.ToUpper(strings.TrimSpace(body.MemberCode))
	if code == "" {
		code = rail.ParticipantCode
	}
	if body.MinQuoteKobo < 0 || body.MaxSpreadBps < 0 || body.TargetUnits < 0 {
		return nil, invalid("min_quote_kobo, max_spread_bps and target_units must not be negative")
	}
	from := body.From
	if from == "" {
		from = s.today()
	}
	to := body.To
	if to == "" {
		d, _ := scheme.ParseBusinessDate(from)
		to = d.AddDate(1, 0, 0).Format("2006-01-02")
	}
	for _, d := range []string{from, to} {
		if _, err := scheme.ParseBusinessDate(d); err != nil {
			return nil, invalid("%q is not a date (YYYY-MM-DD)", d)
		}
	}
	symbol := strings.ToUpper(chi.URLParam(r, "symbol"))
	var out row
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if err := rail.Ensure(ctx, tx); err != nil {
			return err
		}
		id, err := instrumentID(ctx, tx, symbol)
		if err != nil {
			return err
		}
		var memberID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM members WHERE code = $1`, code).Scan(&memberID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return notFound("No member %s", code)
			}
			return err
		}
		o := exchange.StandardObligation(id, memberID, from, to)
		if body.MinQuoteKobo > 0 {
			o.MinQuote = money.Kobo(body.MinQuoteKobo)
		}
		if body.MaxSpreadBps > 0 {
			o.MaxSpreadBps = body.MaxSpreadBps
		}
		o.TargetUnits = share.Units(body.TargetUnits)
		providerID, err := exchange.Appoint(ctx, tx, o)
		if err != nil {
			return refused(err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO cap_table_events (instrument_id, kind, units_delta, note)
			VALUES ($1, 'transfer', 0, $2)`, id,
			fmt.Sprintf("market maker %s appointed by %s: %s a side, spread ≤ %d bps, %s to %s",
				code, by, o.MinQuote, o.MaxSpreadBps, from, to)); err != nil {
			return err
		}
		rows, err := s.providerRows(ctx, tx, id, s.today())
		if err != nil {
			return err
		}
		for _, pr := range rows {
			if pr["provider_id"] == providerID.String() {
				out = pr
			}
		}
		return nil
	})
	return out, err
}

// terminateMarketMaker ends every live appointment of a member on a symbol.
func (s *Server) terminateMarketMaker(ctx context.Context, r *http.Request) (any, error) {
	by, err := operator(r)
	if err != nil {
		return nil, err
	}
	var body struct {
		MemberCode string `json:"member_code"`
		Reason     string `json:"reason"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	code := strings.ToUpper(strings.TrimSpace(body.MemberCode))
	if code == "" {
		code = rail.ParticipantCode
	}
	symbol := strings.ToUpper(chi.URLParam(r, "symbol"))
	var out []row
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		id, err := instrumentID(ctx, tx, symbol)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT lp.id FROM liquidity_providers lp JOIN members m ON m.id = lp.member_id
			 WHERE lp.instrument_id = $1 AND m.code = $2 AND lp.state <> 'terminated'`, id, code)
		if err != nil {
			return err
		}
		var ids []uuid.UUID
		for rows.Next() {
			var pid uuid.UUID
			if err := rows.Scan(&pid); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, pid)
		}
		rows.Close()
		if len(ids) == 0 {
			return notFound("%s has no live appointment on %s", code, symbol)
		}
		for _, pid := range ids {
			if err := exchange.TerminateProvider(ctx, tx, pid, s.today()); err != nil {
				return refused(err)
			}
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO cap_table_events (instrument_id, kind, units_delta, note)
			VALUES ($1, 'transfer', 0, $2)`, id,
			fmt.Sprintf("market maker %s terminated by %s: %s", code, by, body.Reason)); err != nil {
			return err
		}
		out, err = s.providerRows(ctx, tx, id, s.today())
		return err
	})
	return out, err
}
