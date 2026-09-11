package tapcrypto

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"fmt"
	"net/url"
)

// NTAG 424 DNA Secure Dynamic Messaging, per NXP AN12196.
//
// The tag holds two AES-128 keys and, on every read, mirrors into its own NDEF
// URL an encrypted PICCData blob (its UID and a monotonic read counter) plus a
// CMAC computed under a session key derived from that counter. Verification is
// therefore: decrypt, derive, re-compute, compare, and require the counter to
// have advanced.
//
// The counter is what makes a captured URL worthless. Replaying yesterday's tap
// presents a counter the issuer has already seen, and a tag cannot be made to
// count backwards.

// sv2Prefix is the fixed head of the session-key derivation vector
// (AN12196 §Session key generation). The full vector is
//
//	3C C3 00 01 00 80 ‖ UID(7) ‖ SDMReadCtr(3, little-endian)
//
// zero-padded to the AES block size.
var sv2Prefix = []byte{0x3C, 0xC3, 0x00, 0x01, 0x00, 0x80}

// piccDataTagUID and piccDataTagCtr are the flag bits in the first byte of
// decrypted PICCData. The common personalisation mirrors both, giving 0xC7:
// UID present, counter present, UID seven bytes long.
const (
	piccDataTagUID = 0x80
	piccDataTagCtr = 0x40
	piccUIDLen     = 7
)

// NTAG424 verifies SDM presentments. Keys are fetched per card through a
// KeyStore so that an HSM-backed implementation is a constructor change.
type NTAG424 struct {
	Keys KeyStore
}

func (n *NTAG424) Tech() Tech { return TechNTAG424 }

// Parse reads the SDM mirror fields out of the tag's URL.
//
// The UID and counter are deliberately left empty: they are inside the
// encrypted PICCData and are not known — and must not be guessed at — until
// Verify has decrypted it under a key only the issuer holds.
func (n *NTAG424) Parse(q url.Values) (Presentment, error) {
	picc, err := hexField(q, "picc", 16)
	if err != nil {
		return Presentment{}, err
	}
	mac, err := hexField(q, "cmac", 8)
	if err != nil {
		return Presentment{}, err
	}
	// Optional encrypted file data; when the personalisation mirrors none, the
	// CMAC input is the empty string and this stays nil.
	var macInput []byte
	if raw := q.Get("enc"); raw != "" {
		macInput, err = hexField(q, "enc", 0)
		if err != nil {
			return Presentment{}, err
		}
	}
	return Presentment{
		Tech:     TechNTAG424,
		PICCData: picc,
		CMAC:     mac,
		MACInput: macInput,
	}, nil
}

func (n *NTAG424) Verify(state CardState, p Presentment) Result {
	if p.Tech != TechNTAG424 || state.Tech != TechNTAG424 {
		return declined(ReasonTechMismatch)
	}
	if state.Status != StatusActive {
		return declined(ReasonCredentialFrozen)
	}
	if len(p.PICCData) != aes.BlockSize || len(p.CMAC) != 8 {
		return declined(ReasonMalformed)
	}

	metaKey, err := n.Keys.MetaReadKey(state.CardID)
	if err != nil {
		return declined(ReasonKeyUnavailable)
	}
	uid, counter, err := decryptPICCData(metaKey, p.PICCData)
	if err != nil {
		return declined(ReasonMalformed)
	}

	// A structurally valid blob decrypted under this card's key can still belong
	// to a different card if an attacker swapped personalisation data.
	if !equalCT(uid, state.UID) {
		return clone(ReasonUIDMismatch)
	}

	fileKey, err := n.Keys.FileReadKey(state.CardID)
	if err != nil {
		return declined(ReasonKeyUnavailable)
	}
	sessionKey, err := sdmSessionKey(fileKey, uid, counter)
	if err != nil {
		return declined(ReasonKeyUnavailable)
	}
	full, err := CMAC(sessionKey, p.MACInput)
	if err != nil {
		return declined(ReasonKeyUnavailable)
	}
	if !equalCT(truncateNXP(full), p.CMAC) {
		// A bad CMAC is not a clone signal: it is far more often a
		// personalisation or key-rotation mistake, and freezing cards on it
		// would turn one bad batch into a mass lockout.
		return declined(ReasonBadCMAC)
	}

	// The counter is the replay defence. Equality means the exact same tap is
	// being presented twice, which is what a captured URL looks like.
	if counter <= state.LastCounter {
		return clone(ReasonCounterReplay)
	}

	return Result{OK: true, Counter: counter, UID: uid}
}

// decryptPICCData recovers the UID and read counter from the encrypted mirror.
//
// AES-128-CBC with an all-zero IV is what the tag uses; it is safe here only
// because the plaintext is a single block containing a counter that never
// repeats, so there is no second block to leak a relationship to.
func decryptPICCData(key, blob []byte) (uid []byte, counter uint32, err error) {
	if len(blob) != aes.BlockSize {
		return nil, 0, fmt.Errorf("tapcrypto: PICCData is %d bytes, want %d", len(blob), aes.BlockSize)
	}
	c, err := aes.NewCipher(key)
	if err != nil {
		return nil, 0, err
	}
	out := make([]byte, aes.BlockSize)
	cipher.NewCBCDecrypter(c, make([]byte, aes.BlockSize)).CryptBlocks(out, blob)

	tag := out[0]
	if tag&piccDataTagUID == 0 || tag&piccDataTagCtr == 0 {
		return nil, 0, fmt.Errorf("tapcrypto: PICCData tag %#02x does not mirror both UID and counter", tag)
	}
	if int(tag&0x0f) != piccUIDLen {
		return nil, 0, fmt.Errorf("tapcrypto: PICCData declares a %d-byte UID", tag&0x0f)
	}
	uid = make([]byte, piccUIDLen)
	copy(uid, out[1:1+piccUIDLen])

	// SDMReadCtr is three bytes, least significant first.
	counter = uint32(out[8]) | uint32(out[9])<<8 | uint32(out[10])<<16
	return uid, counter, nil
}

// sdmSessionKey derives KSesSDMFileReadMAC (AN12196).
func sdmSessionKey(fileReadKey, uid []byte, counter uint32) ([]byte, error) {
	if len(uid) != piccUIDLen {
		return nil, fmt.Errorf("tapcrypto: session key needs a %d-byte UID", piccUIDLen)
	}
	sv2 := make([]byte, 0, aes.BlockSize)
	sv2 = append(sv2, sv2Prefix...)
	sv2 = append(sv2, uid...)
	var ctr [4]byte
	binary.LittleEndian.PutUint32(ctr[:], counter)
	sv2 = append(sv2, ctr[0], ctr[1], ctr[2])
	for len(sv2) < aes.BlockSize {
		sv2 = append(sv2, 0x00)
	}
	return CMAC(fileReadKey, sv2)
}

// EncodePICCData builds the encrypted mirror a tag would emit. It exists for
// personalisation and for tests: the verifier above is only trustworthy if
// something independent can produce input it should accept.
func EncodePICCData(key, uid []byte, counter uint32) ([]byte, error) {
	if len(uid) != piccUIDLen {
		return nil, fmt.Errorf("tapcrypto: UID must be %d bytes", piccUIDLen)
	}
	if counter > 0xFFFFFF {
		return nil, fmt.Errorf("tapcrypto: counter %d exceeds the tag's 24-bit range", counter)
	}
	plain := make([]byte, aes.BlockSize)
	plain[0] = piccDataTagUID | piccDataTagCtr | piccUIDLen
	copy(plain[1:], uid)
	plain[8] = byte(counter)
	plain[9] = byte(counter >> 8)
	plain[10] = byte(counter >> 16)
	// Bytes 11..15 are the tag's zero padding.

	c, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, aes.BlockSize)
	cipher.NewCBCEncrypter(c, make([]byte, aes.BlockSize)).CryptBlocks(out, plain)
	return out, nil
}

// SignSDM produces the 8-byte CMAC a tag would transmit for this tap.
func SignSDM(fileReadKey, uid []byte, counter uint32, macInput []byte) ([]byte, error) {
	sk, err := sdmSessionKey(fileReadKey, uid, counter)
	if err != nil {
		return nil, err
	}
	full, err := CMAC(sk, macInput)
	if err != nil {
		return nil, err
	}
	return truncateNXP(full), nil
}
