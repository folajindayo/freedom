// Package tapcrypto verifies that a tap came from a card Ọja issued.
//
// Two credential technologies sit behind one interface, and the gap between
// them is the single most important security fact about the pilot:
//
//	NTAG 424 DNA  Cryptographic. The tag computes AES-CMAC over a monotonic
//	              counter with a key that never leaves it. Functionally an EMV
//	              cryptogram: a clone requires the key, and the key is not
//	              readable. This is the production credential.
//
//	NTAG 215      Not cryptographic. It has no key and computes nothing. The UID
//	              is readable and copyable and user memory can be dumped. It
//	              cannot prove it is itself.
//
// The NTAG 215 path is therefore not authentication and this package does not
// pretend otherwise. What it does is make cloning loud: a server-issued rolling
// token that burns on use, plus the tag's own one-way read counter, so that a
// copy and its original fall out of step and the mismatch surfaces as a decline
// and a freeze. An attacker who reads a token and writes a blank tag before the
// real card taps again still wins that round. That is a priced fraud loss, not
// a defect to be argued away, and it is why the 215 path ships online-only,
// under low caps and tight velocity limits, to a closed pilot cohort.
//
// # Separation of concerns
//
// Nothing here touches a database. Verifiers are pure functions over a CardState
// the caller loaded and a Presentment the caller parsed, which is what lets the
// whole state machine — including the adversarial cases — be table-tested
// without a fixture.
package tapcrypto

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"time"
)

// Tech identifies a credential technology.
type Tech string

const (
	TechNTAG215 Tech = "ntag215"
	TechNTAG424 Tech = "ntag424"
)

// TokenBytes is the length of an NTAG 215 rolling token. 16 bytes is well
// inside the tag's 504-byte user memory and far beyond guessing.
const TokenBytes = 16

// DeclineReason explains a refusal. These map to scheme decline codes in
// package protocol; they are kept separate from the wire codes so that adding a
// reason does not silently change what a terminal prints.
type DeclineReason string

const (
	ReasonNone             DeclineReason = ""
	ReasonUnknownCard      DeclineReason = "unknown_card"
	ReasonCardNotActive    DeclineReason = "card_not_active"
	ReasonUIDMismatch      DeclineReason = "uid_mismatch"
	ReasonBadCMAC          DeclineReason = "bad_cmac"
	ReasonCounterReplay    DeclineReason = "counter_replay"
	ReasonStaleToken       DeclineReason = "stale_token"
	ReasonUnknownToken     DeclineReason = "unknown_token"
	ReasonMalformed        DeclineReason = "malformed_presentment"
	ReasonTechMismatch     DeclineReason = "tech_mismatch"
	ReasonKeyUnavailable   DeclineReason = "key_unavailable"
	ReasonCounterTooOld    DeclineReason = "counter_regressed"
	ReasonCredentialFrozen DeclineReason = "credential_frozen"
)

// Credential status values.
const (
	StatusActive = "active"
	StatusFrozen = "frozen"
)

// Presentment is what arrived from the tag: parsed, well-formed, and not yet
// trusted in any way.
type Presentment struct {
	Tech Tech

	// Present for both technologies.
	UID     []byte
	Counter uint32

	// NTAG 215: the rolling token read out of user memory.
	Token []byte

	// NTAG 424: the SDM fields mirrored into the tag's own NDEF URL. PICCData
	// is still encrypted; the UID and Counter above are only populated after
	// Verify decrypts it, so a caller must not trust them before then.
	PICCData []byte
	CMAC     []byte
	MACInput []byte
}

// CardState is what the issuer knows about this credential, loaded by the
// caller. Verifiers never read or write it.
type CardState struct {
	CardID string
	Tech   Tech
	Status string

	// UID as personalised. For NTAG 424 this is checked against the decrypted
	// PICCData, so a tag replaying another card's data is caught.
	UID []byte

	// LastCounter is the highest counter value already accepted. Both
	// technologies require strict increase.
	LastCounter uint32

	// NTAG 215 rolling tokens. Two are live at once so that a write that fails
	// after approval — the customer lifted the card early — does not lock a
	// real cardholder out on their next tap. Both burn on use.
	TokenCurrent []byte
	TokenPrev    []byte
}

// Result is the verdict. A Result with OK false and CloneSuspected true is the
// signal to freeze the credential and raise a case, not merely to decline.
type Result struct {
	OK             bool
	Reason         DeclineReason
	Counter        uint32
	UID            []byte
	CloneSuspected bool

	// UsedPrevToken records that the fallback token was accepted, meaning the
	// previous write-back did not land. Worth a metric: a rising rate means the
	// PoS is losing the field too early, which degrades the clone signal.
	UsedPrevToken bool
}

func declined(r DeclineReason) Result { return Result{OK: false, Reason: r} }

func clone(r DeclineReason) Result {
	return Result{OK: false, Reason: r, CloneSuspected: true}
}

// WriteBack is a new rolling token for the PoS to write into the tag. It is
// returned only after an approval, and only for NTAG 215.
type WriteBack struct {
	Token    []byte
	IssuedAt time.Time
}

// Verifier checks a presentment against known state.
type Verifier interface {
	Tech() Tech

	// Parse decodes the transport form into a Presentment without trusting any
	// of it. For NTAG 424 the values are the tag's own SDM URL query; for
	// NTAG 215 they are what the PoS read out of the tag.
	Parse(q url.Values) (Presentment, error)

	// Verify is a pure decision over caller-supplied state.
	Verify(state CardState, p Presentment) Result
}

// Rotator is implemented by credentials whose secret lives in rewritable tag
// memory. NTAG 424 does not implement it: its key never changes and never
// leaves the tag.
type Rotator interface {
	IssueNext() (*WriteBack, error)
}

// NewToken mints a rolling token from the system CSPRNG.
func NewToken() ([]byte, error) {
	b := make([]byte, TokenBytes)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("tapcrypto: mint token: %w", err)
	}
	return b, nil
}

// hexField pulls a hex-encoded parameter of an exact byte length.
func hexField(q url.Values, name string, want int) ([]byte, error) {
	v := q.Get(name)
	if v == "" {
		return nil, fmt.Errorf("tapcrypto: %s missing", name)
	}
	b, err := hex.DecodeString(v)
	if err != nil {
		return nil, fmt.Errorf("tapcrypto: %s is not hex: %w", name, err)
	}
	if want > 0 && len(b) != want {
		return nil, fmt.Errorf("tapcrypto: %s is %d bytes, want %d", name, len(b), want)
	}
	return b, nil
}

var errNotRotatable = errors.New("tapcrypto: credential does not rotate")
