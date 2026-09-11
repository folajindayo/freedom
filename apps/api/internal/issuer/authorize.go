// Package issuer decides whether a tap is approved.
//
// This is the latency-critical path in the whole system. A tap that takes more
// than about a second and a half stops feeling like a tap, and a card that does
// not feel instant does not get used twice, so everything here runs in one
// database transaction with no external calls.
//
// # What an authorisation does to money
//
// It moves value sideways, not out: the ticket amount goes from the cardholder's
// available balance into their hold balance. Nothing is paid to anyone until
// clearing. That is what makes a reversal cheap — the money never left — and it
// is why the buyback fires at clearing rather than here.
package issuer

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"oja/api/internal/ledger"
	"oja/api/internal/money"
	"oja/api/internal/scheme"
	"oja/api/internal/tapcrypto"
)

// Request is an authorisation request as it arrives from an acquirer.
type Request struct {
	TerminalID uuid.UUID
	// STAN is the terminal's own transaction number. A terminal retrying a
	// timed-out tap sends the same one, and the switch must resolve that to a
	// single authorisation.
	STAN   string
	Amount money.Kobo
	// Tap carries the fields the PoS read off the credential.
	Tech tapcrypto.Tech
	Tap  url.Values
	At   time.Time
}

// Response is what the terminal is told, plus anything it must do next.
type Response struct {
	Approved     bool
	AuthID       uuid.UUID
	AuthCode     string
	DeclineCode  scheme.DeclineCode
	Message      string
	BusinessDate string

	// WriteBack is the next rolling token, present only for NTAG 215 and only
	// on approval. The PoS must write it to the tag and acknowledge; if it does
	// not, the previous token remains live for one more tap.
	WriteBack *tapcrypto.WriteBack
}

// Authorizer holds the verifiers and the clock. It has no state of its own: a
// tap's outcome depends on the database and on the message, never on what this
// process happens to remember.
type Authorizer struct {
	Verifiers map[tapcrypto.Tech]tapcrypto.Verifier
	Now       func() time.Time
}

// New builds an Authorizer for both credential technologies.
func New(keys tapcrypto.KeyStore) *Authorizer {
	return &Authorizer{
		Verifiers: map[tapcrypto.Tech]tapcrypto.Verifier{
			tapcrypto.TechNTAG215: tapcrypto.NTAG215{},
			tapcrypto.TechNTAG424: &tapcrypto.NTAG424{Keys: keys},
		},
		Now: time.Now,
	}
}

// AuthorizationLifetime is how long a hold survives without a capture. Real
// networks release stale holds; a card whose balance is eaten by a week-old
// abandoned authorisation is a support call the cardholder always wins.
const AuthorizationLifetime = 7 * 24 * time.Hour

func decline(code scheme.DeclineCode) Response {
	return Response{Approved: false, DeclineCode: code, Message: code.Message()}
}

// Authorize runs the full decision inside one transaction.
func (a *Authorizer) Authorize(ctx context.Context, tx pgx.Tx, req Request) (Response, error) {
	now := a.Now()
	if req.At.IsZero() {
		req.At = now
	}
	businessDate := scheme.BusinessDate(req.At)

	if req.Amount <= 0 {
		return decline(scheme.DeclineFormatError), nil
	}

	verifier, ok := a.Verifiers[req.Tech]
	if !ok {
		return decline(scheme.DeclineFormatError), nil
	}
	presentment, err := verifier.Parse(req.Tap)
	if err != nil {
		// A malformed presentment is a read error at the terminal, not a
		// judgement about the card.
		return decline(scheme.DeclineFormatError), nil
	}

	term, err := loadTerminal(ctx, tx, req.TerminalID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return decline(scheme.DeclineInvalidTerminal), nil
		}
		return Response{}, err
	}
	if term.Status != "active" || term.MerchantStatus != "active" {
		return decline(scheme.DeclineInvalidTerminal), nil
	}

	// Lock the credential for the duration. Two terminals tapping the same card
	// at the same instant must not both read the pre-rotation token and both
	// approve; the second waits here and then sees a token that has been burnt.
	cred, err := lockCredentialByUID(ctx, tx, presentmentUID(presentment, req.Tap))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return decline(scheme.DeclineInvalidCard), nil
		}
		return Response{}, err
	}

	state := tapcrypto.CardState{
		CardID:       cred.CardID.String(),
		Tech:         tapcrypto.Tech(cred.Tech),
		Status:       cred.Status,
		UID:          cred.TagUID,
		LastCounter:  uint32(cred.LastCounter),
		TokenCurrent: cred.TokenCurrent,
		TokenPrev:    cred.TokenPrev,
	}
	result := verifier.Verify(state, presentment)

	if result.CloneSuspected {
		// Freeze first, answer second. A suspected clone means two tags carry
		// one identity and every further tap on either is a loss.
		if err := freezeForClone(ctx, tx, cred, string(result.Reason), now); err != nil {
			return Response{}, err
		}
		return decline(scheme.DeclineRestrictedCard), nil
	}
	if !result.OK {
		return decline(declineFor(result.Reason)), nil
	}

	card, err := loadCard(ctx, tx, cred.CardID)
	if err != nil {
		return Response{}, err
	}
	switch {
	case card.Status == "frozen", card.Status == "cancelled":
		return decline(scheme.DeclineRestrictedCard), nil
	case card.Status == "expired" || !card.ExpiresOn.After(req.At):
		return decline(scheme.DeclineExpiredCard), nil
	case card.HolderStatus != "active":
		return decline(scheme.DeclineRestrictedCard), nil
	}

	// The pilot rails. These are the fraud-loss budget for a credential that
	// cannot authenticate, not advisory guidance.
	if req.Amount > card.PerTxnCap {
		return decline(scheme.DeclineExceedsLimit), nil
	}
	spentToday, err := authorisedToday(ctx, tx, card.ID, businessDate)
	if err != nil {
		return Response{}, err
	}
	if spentToday+req.Amount > card.DailyCap {
		return decline(scheme.DeclineVelocityExceeded), nil
	}

	available, err := ledger.NairaBalance(ctx, tx,
		ledger.Cardholder(card.CardholderID, ledger.KindAvailable, ledger.AssetNGN))
	if err != nil {
		return Response{}, err
	}
	if available < req.Amount {
		return decline(scheme.DeclineInsufficientFunds), nil
	}

	// Advance the counter conditionally. Read-then-write would let two
	// concurrent taps accept the same counter value; this cannot.
	if err := advanceCounter(ctx, tx, cred.ID, int64(result.Counter)); err != nil {
		return Response{}, err
	}

	// Place the hold.
	availAcct, err := ledger.Resolve(ctx, tx,
		ledger.Cardholder(card.CardholderID, ledger.KindAvailable, ledger.AssetNGN))
	if err != nil {
		return Response{}, err
	}
	holdAcct, err := ledger.Resolve(ctx, tx,
		ledger.Cardholder(card.CardholderID, ledger.KindHold, ledger.AssetNGN))
	if err != nil {
		return Response{}, err
	}
	holdTx, err := ledger.Post(ctx, tx, ledger.Tx{
		EventType:    "authorization.hold",
		BusinessDate: businessDate,
		// The natural key a switch always has. A retried tap posts nothing.
		IdempotencyKey: fmt.Sprintf("auth|%s|%s|%s|%s",
			term.AcquirerID, businessDate, term.ID, req.STAN),
		Entries: []ledger.Entry{
			{AccountID: availAcct, Amount: ledger.NGN(-req.Amount), Reason: "authorization.hold"},
			{AccountID: holdAcct, Amount: ledger.NGN(req.Amount), Reason: "authorization.hold"},
		},
	})
	if err != nil {
		if errors.Is(err, ledger.ErrAlreadyPosted) {
			// The terminal retried a tap that already succeeded. Return the
			// original decision rather than declining a good transaction.
			return a.replay(ctx, tx, term, req, businessDate)
		}
		return Response{}, err
	}

	authID, authCode, err := recordAuthorization(ctx, tx, authRow{
		CardID:       card.ID,
		CredentialID: cred.ID,
		Terminal:     term,
		STAN:         req.STAN,
		Amount:       req.Amount,
		Counter:      int64(result.Counter),
		BusinessDate: businessDate,
		HoldTxID:     holdTx,
		ExpiresAt:    now.Add(AuthorizationLifetime),
	})
	if err != nil {
		return Response{}, err
	}

	resp := Response{
		Approved:     true,
		AuthID:       authID,
		AuthCode:     authCode,
		BusinessDate: businessDate,
	}

	// Rotate the rolling token. Only NTAG 215 rotates; NTAG 424 holds a key it
	// never reveals and has nothing to write back.
	if rot, ok := verifier.(tapcrypto.Rotator); ok {
		wb, err := rot.IssueNext()
		if err != nil {
			return Response{}, err
		}
		if err := rotateToken(ctx, tx, cred.ID, cred.TokenCurrent, wb.Token, now); err != nil {
			return Response{}, err
		}
		resp.WriteBack = wb
	}
	return resp, nil
}

// declineFor maps an internal verification reason to what the counter is told.
func declineFor(r tapcrypto.DeclineReason) scheme.DeclineCode {
	switch r {
	case tapcrypto.ReasonCredentialFrozen:
		return scheme.DeclineRestrictedCard
	case tapcrypto.ReasonMalformed, tapcrypto.ReasonTechMismatch:
		return scheme.DeclineFormatError
	case tapcrypto.ReasonUIDMismatch, tapcrypto.ReasonUnknownCard:
		return scheme.DeclineInvalidCard
	case tapcrypto.ReasonKeyUnavailable:
		return scheme.DeclineSystemError
	default:
		// Cryptographic failures are reported as a plain decline. Telling a
		// probing attacker which check failed hands them an oracle.
		return scheme.DeclineSuspectedFraud
	}
}

// presentmentUID returns the UID to look the credential up by. NTAG 424 hides
// its UID inside encrypted PICCData, so the PoS sends the plain UID it read
// from the tag's anticollision response purely as a lookup hint — it is never
// trusted, because Verify re-derives the real one from the ciphertext and
// compares.
func presentmentUID(p tapcrypto.Presentment, q url.Values) []byte {
	if len(p.UID) > 0 {
		return p.UID
	}
	uid, err := decodeHex(q.Get("uid"))
	if err != nil {
		return nil
	}
	return uid
}
