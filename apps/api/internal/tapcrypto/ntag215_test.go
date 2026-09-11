package tapcrypto

import (
	"encoding/hex"
	"net/url"
	"testing"
)

func tag215(uid, token []byte, ctr uint32) url.Values {
	c := []byte{byte(ctr), byte(ctr >> 8), byte(ctr >> 16)}
	return url.Values{
		"uid":   {hex.EncodeToString(uid)},
		"token": {hex.EncodeToString(token)},
		"ctr":   {hex.EncodeToString(c)},
	}
}

func tok(b byte) []byte {
	t := make([]byte, TokenBytes)
	for i := range t {
		t[i] = b
	}
	return t
}

var pilotUID = []byte{0x04, 0x99, 0x88, 0x77, 0x66, 0x55, 0x44}

// The whole pilot state machine in one table: what the issuer believes, what
// the tag presented, and what must happen. Every adversarial case in the design
// notes appears here as a row.
func TestNTAG215StateMachine(t *testing.T) {
	v := NTAG215{}

	base := CardState{
		CardID: "c1", Tech: TechNTAG215, Status: StatusActive,
		UID: pilotUID, LastCounter: 10,
		TokenCurrent: tok(0xAA), TokenPrev: tok(0xBB),
	}

	for _, tc := range []struct {
		name      string
		mutate    func(*CardState)
		token     []byte
		uid       []byte
		counter   uint32
		wantOK    bool
		wantClone bool
		wantPrev  bool
		wantWhy   DeclineReason
	}{
		{
			name:  "genuine tap with the current token",
			token: tok(0xAA), uid: pilotUID, counter: 11, wantOK: true,
		},
		{
			name:  "write-back failed last time, tag still holds the previous token",
			token: tok(0xBB), uid: pilotUID, counter: 11, wantOK: true, wantPrev: true,
		},
		{
			name:  "a token this card once held but has since burnt — a clone",
			token: tok(0xCC), uid: pilotUID, counter: 11,
			wantClone: true, wantWhy: ReasonStaleToken,
		},
		{
			name:  "counter went backwards: two tags answer to this UID",
			token: tok(0xAA), uid: pilotUID, counter: 9,
			wantClone: true, wantWhy: ReasonCounterTooOld,
		},
		{
			name:  "counter unchanged is legal — the tag was read without a tap",
			token: tok(0xAA), uid: pilotUID, counter: 10, wantOK: true,
		},
		{
			name:  "a different card's UID is a routing bug, not a clone",
			token: tok(0xAA), uid: []byte{0x04, 1, 1, 1, 1, 1, 1}, counter: 11,
			wantWhy: ReasonUIDMismatch,
		},
		{
			name:   "a frozen credential is refused before anything else is considered",
			mutate: func(s *CardState) { s.Status = StatusFrozen },
			token:  tok(0xAA), uid: pilotUID, counter: 11,
			wantWhy: ReasonCredentialFrozen,
		},
		{
			name:   "no previous token yet: only the current one is live",
			mutate: func(s *CardState) { s.TokenPrev = nil },
			token:  tok(0xBB), uid: pilotUID, counter: 11,
			wantClone: true, wantWhy: ReasonStaleToken,
		},
		{
			name:   "an all-zero token must not match an empty stored token",
			mutate: func(s *CardState) { s.TokenCurrent = nil; s.TokenPrev = nil },
			token:  make([]byte, TokenBytes), uid: pilotUID, counter: 11,
			wantClone: true, wantWhy: ReasonStaleToken,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := base
			if tc.mutate != nil {
				tc.mutate(&state)
			}
			p, err := v.Parse(tag215(tc.uid, tc.token, tc.counter))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			res := v.Verify(state, p)

			if res.OK != tc.wantOK {
				t.Fatalf("OK = %v (reason %q), want %v", res.OK, res.Reason, tc.wantOK)
			}
			if res.CloneSuspected != tc.wantClone {
				t.Errorf("CloneSuspected = %v, want %v", res.CloneSuspected, tc.wantClone)
			}
			if tc.wantWhy != "" && res.Reason != tc.wantWhy {
				t.Errorf("reason = %q, want %q", res.Reason, tc.wantWhy)
			}
			if res.UsedPrevToken != tc.wantPrev {
				t.Errorf("UsedPrevToken = %v, want %v", res.UsedPrevToken, tc.wantPrev)
			}
		})
	}
}

// The documented limit of this credential, asserted rather than described: a
// reader that lifts the live token can produce a tap the issuer accepts. The
// design does not claim otherwise, and a future change that appears to "fix"
// this without moving to NTAG 424 is almost certainly wrong.
func TestNTAG215LiftedTokenIsAcceptedOnce(t *testing.T) {
	v := NTAG215{}
	state := CardState{
		CardID: "c1", Tech: TechNTAG215, Status: StatusActive, UID: pilotUID,
		LastCounter: 5, TokenCurrent: tok(0xAA),
	}
	lifted := tok(0xAA) // read off the card in a queue

	p, _ := v.Parse(tag215(pilotUID, lifted, 6))
	if res := v.Verify(state, p); !res.OK {
		t.Fatal("a lifted current token is accepted — this is the known limit of NTAG 215")
	}

	// After the issuer burns it, the real card's next tap with the same token is
	// what raises the alarm — detection after the fact, not prevention.
	state.TokenPrev = state.TokenCurrent
	state.TokenCurrent = tok(0xDD)
	state.LastCounter = 6

	p, _ = v.Parse(tag215(pilotUID, tok(0xEE), 7))
	if res := v.Verify(state, p); !res.CloneSuspected {
		t.Fatal("an unknown token after a burn must raise a clone signal")
	}
}

func TestNTAG215IssueNextIsUnpredictable(t *testing.T) {
	v := NTAG215{}
	seen := map[string]bool{}
	for i := 0; i < 1_000; i++ {
		wb, err := v.IssueNext()
		if err != nil {
			t.Fatal(err)
		}
		if len(wb.Token) != TokenBytes {
			t.Fatalf("token is %d bytes, want %d", len(wb.Token), TokenBytes)
		}
		k := hex.EncodeToString(wb.Token)
		if seen[k] {
			t.Fatal("IssueNext repeated a token")
		}
		seen[k] = true
	}
}

func TestNTAG215ParseRejectsMalformed(t *testing.T) {
	v := NTAG215{}
	for _, tc := range []struct {
		name string
		q    url.Values
	}{
		{"no uid", url.Values{"token": {hex.EncodeToString(tok(1))}, "ctr": {"000000"}}},
		{"short uid", tag215([]byte{1, 2, 3}, tok(1), 1)},
		{"short token", url.Values{"uid": {hex.EncodeToString(pilotUID)}, "token": {"aabb"}, "ctr": {"000000"}}},
		{"non-hex", url.Values{"uid": {"zzzz"}, "token": {hex.EncodeToString(tok(1))}, "ctr": {"000000"}}},
		{"no counter", url.Values{"uid": {hex.EncodeToString(pilotUID)}, "token": {hex.EncodeToString(tok(1))}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := v.Parse(tc.q); err == nil {
				t.Fatal("malformed input must be rejected at parse")
			}
		})
	}
}

// Counters are little-endian on the wire; getting it backwards only shows up
// past 255 taps, long after the pilot would have signed off.
func TestNTAG215CounterEndianness(t *testing.T) {
	v := NTAG215{}
	p, err := v.Parse(tag215(pilotUID, tok(1), 0x010203))
	if err != nil {
		t.Fatal(err)
	}
	if p.Counter != 0x010203 {
		t.Fatalf("counter parsed as %#x, want %#x", p.Counter, 0x010203)
	}
}

// A card in one technology must never be verified by the other's rules.
func TestTechMismatchIsRefused(t *testing.T) {
	v := NTAG215{}
	state := CardState{CardID: "c1", Tech: TechNTAG424, Status: StatusActive, UID: pilotUID}
	p, _ := v.Parse(tag215(pilotUID, tok(0xAA), 1))
	if res := v.Verify(state, p); res.OK || res.Reason != ReasonTechMismatch {
		t.Fatalf("a 424 card verified under 215 rules: %+v", res)
	}
}
