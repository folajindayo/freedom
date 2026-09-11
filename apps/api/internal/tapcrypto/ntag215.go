package tapcrypto

import (
	"net/url"
	"time"
)

// NTAG 215, the pilot credential.
//
// The tag computes nothing, so the server does all the work against two signals
// the tag does provide:
//
//	a rolling token   16 random bytes the issuer wrote into user memory. The PoS
//	                  reads it, the issuer burns it, and the PoS writes the next
//	                  one back. Presenting a burnt token means two cards carry
//	                  the same secret.
//	the read counter  NTAG21x increments a 24-bit one-way counter on the first
//	                  valid READ after entering a field. A clone cannot make the
//	                  original count backwards, so the two drift apart and the
//	                  regression is visible.
//
// # The two-token window
//
// A single expected token is wrong in practice. The customer lifts the card as
// soon as the terminal beeps, the write-back never lands, and the tag still
// holds token N while the issuer expects N+1. The next genuine tap then looks
// exactly like a clone and freezes a real cardholder mid-queue.
//
// So two tokens are live at once and both burn on use. That costs a little
// detection sharpness — a thief who captures a token has one extra tap of room
// — and buys the pilot a false-positive rate low enough to run.
//
// # What this does not do
//
// It does not authenticate. A reader held near a card in a queue can lift the
// current token and write it to a blank tag, and if that clone taps before the
// real card does, it is approved and the real card is the one that gets frozen.
// Only NTAG 424 DNA closes that, because only NTAG 424 keeps a secret the
// reader never sees.
type NTAG215 struct{}

func (NTAG215) Tech() Tech { return TechNTAG215 }

// Parse reads what the PoS lifted off the tag.
func (NTAG215) Parse(q url.Values) (Presentment, error) {
	uid, err := hexField(q, "uid", 7)
	if err != nil {
		return Presentment{}, err
	}
	token, err := hexField(q, "token", TokenBytes)
	if err != nil {
		return Presentment{}, err
	}
	ctr, err := hexField(q, "ctr", 3)
	if err != nil {
		return Presentment{}, err
	}
	// NTAG21x reports its counter least significant byte first.
	counter := uint32(ctr[0]) | uint32(ctr[1])<<8 | uint32(ctr[2])<<16

	return Presentment{
		Tech:    TechNTAG215,
		UID:     uid,
		Token:   token,
		Counter: counter,
	}, nil
}

func (NTAG215) Verify(state CardState, p Presentment) Result {
	if p.Tech != TechNTAG215 || state.Tech != TechNTAG215 {
		return declined(ReasonTechMismatch)
	}
	if state.Status != StatusActive {
		return declined(ReasonCredentialFrozen)
	}
	if len(p.UID) != 7 || len(p.Token) != TokenBytes {
		return declined(ReasonMalformed)
	}
	if !equalCT(p.UID, state.UID) {
		// The PoS read a different card than the one this state describes. A
		// routing bug looks like this too, so it is a decline, not a freeze.
		return declined(ReasonUIDMismatch)
	}

	// A counter that has gone backwards is physically impossible for one tag:
	// there are two tags answering to this UID.
	if p.Counter < state.LastCounter {
		return clone(ReasonCounterTooOld)
	}

	switch {
	case equalCT(p.Token, state.TokenCurrent):
		return Result{OK: true, Counter: p.Counter, UID: p.UID}

	case len(state.TokenPrev) == TokenBytes && equalCT(p.Token, state.TokenPrev):
		// The previous write-back did not land. Accept once more and re-issue —
		// but only if the tag has actually been read again since. A counter that
		// has not advanced means this is the same tap presented twice, which is
		// a replay rather than a retry.
		if p.Counter <= state.TokenPrevFromCounter {
			return clone(ReasonStaleToken)
		}
		return Result{OK: true, Counter: p.Counter, UID: p.UID, UsedPrevToken: true}

	default:
		// The token is well-formed, the UID matches, and the secret is neither
		// of the two live ones. The only way to hold a token this card once had
		// is to have copied it.
		return clone(ReasonStaleToken)
	}
}

// IssueNext mints the token the PoS must write back after an approval.
func (NTAG215) IssueNext() (*WriteBack, error) {
	tok, err := NewToken()
	if err != nil {
		return nil, err
	}
	return &WriteBack{Token: tok, IssuedAt: time.Now().UTC()}, nil
}

// Compile-time proof that each technology implements exactly what it should:
// NTAG 215 rotates a secret held in tag memory, NTAG 424 has no secret to
// rotate and must not pretend to.
var (
	_ Verifier = (*NTAG215)(nil)
	_ Rotator  = (*NTAG215)(nil)
	_ Verifier = (*NTAG424)(nil)
)
