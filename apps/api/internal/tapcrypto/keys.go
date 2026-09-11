package tapcrypto

import (
	"crypto/aes"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

// KeyStore hands out the per-card AES keys an NTAG 424 credential is
// personalised with. It is an interface because the only difference between the
// pilot and a licensed scheme is where the master key lives: this package does
// not care whether a key came from a file or from a PKCS#11 partition, and the
// switch between them must not be a code change in the verification path.
//
// Implementations must never return a key for a card they cannot identify, and
// must never log key material.
type KeyStore interface {
	// MetaReadKey decrypts the PICCData mirror (SDMMetaReadKey).
	MetaReadKey(cardID string) ([]byte, error)
	// FileReadKey derives the CMAC session key (SDMFileReadKey).
	FileReadKey(cardID string) ([]byte, error)
}

// ErrNoKey is returned for a card the store does not know.
var ErrNoKey = errors.New("tapcrypto: no key for card")

// SoftwareKeyStore diversifies per-card keys from a master key held in process
// memory.
//
// This is a real implementation, not a stub: the diversification is sound and
// the pilot runs on it. What it is not is an HSM. The master key is readable by
// anything that can read this process's memory or its environment, so a host
// compromise is a compromise of every card ever issued — there is no per-card
// blast radius and no key ceremony. Before a single card is issued outside a
// controlled pilot, this is replaced by an implementation backed by a
// FIPS 140-2 Level 3 HSM under dual control, and the master key here is
// destroyed rather than migrated.
//
// PCI PIN and ANSI X9.24 additionally require that keys at rest be wrapped in
// X9.143 (formerly TR-31) key blocks with their attributes bound to the key
// material. A file of raw hex does not meet that bar and is not claimed to.
type SoftwareKeyStore struct {
	master []byte
}

// Key derivation labels. Distinct labels mean the two keys of one card are
// cryptographically unrelated, so disclosing one does not yield the other.
//
// These strings are FROZEN once a single card has been personalised. They are
// domain separators inside the KDF, so editing one — even to fix a typo or to
// follow a rename — silently changes every key derived from it, and every tag
// already in the field stops verifying. A new scheme gets a new version suffix
// and a migration, never an edit.
const (
	labelMetaRead = "freedom/sdm/meta-read/v1"
	labelFileRead = "freedom/sdm/file-read/v1"
)

// NewSoftwareKeyStore builds a store from a 16-byte master key.
func NewSoftwareKeyStore(master []byte) (*SoftwareKeyStore, error) {
	if len(master) != aes.BlockSize {
		return nil, fmt.Errorf("tapcrypto: master key is %d bytes, want %d", len(master), aes.BlockSize)
	}
	cp := make([]byte, len(master))
	copy(cp, master)
	return &SoftwareKeyStore{master: cp}, nil
}

// SoftwareKeyStoreFromEnv reads the master key from an environment variable.
//
// It deliberately has no default and no generated fallback. A card scheme that
// boots with a random master key it did not persist has issued cards it can
// never verify again; a card scheme that boots with a well-known default has
// issued cards anyone can clone. Refusing to start is the only correct
// behaviour when the key is absent.
func SoftwareKeyStoreFromEnv(name string) (*SoftwareKeyStore, error) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return nil, fmt.Errorf("tapcrypto: %s is not set; refusing to start without a card master key", name)
	}
	raw, err := hex.DecodeString(v)
	if err != nil {
		return nil, fmt.Errorf("tapcrypto: %s is not valid hex: %w", name, err)
	}
	return NewSoftwareKeyStore(raw)
}

func (s *SoftwareKeyStore) MetaReadKey(cardID string) ([]byte, error) {
	return s.derive(labelMetaRead, cardID)
}

func (s *SoftwareKeyStore) FileReadKey(cardID string) ([]byte, error) {
	return s.derive(labelFileRead, cardID)
}

// derive computes a per-card key as CMAC(master, 0x01 ‖ label ‖ 0x00 ‖ cardID).
//
// This is AN10922-shaped but is Freedom's own scheme, not NXP's exact one, and that
// is safe only because Freedom personalises its own tags and verifies them here.
// Introducing an NXP SAM or NXP personalisation tooling means adopting AN10922
// bit-for-bit instead; a near-miss would produce tags this code cannot verify.
func (s *SoftwareKeyStore) derive(label, cardID string) ([]byte, error) {
	if cardID == "" {
		return nil, ErrNoKey
	}
	msg := make([]byte, 0, 1+len(label)+1+len(cardID))
	msg = append(msg, 0x01)
	msg = append(msg, label...)
	msg = append(msg, 0x00)
	msg = append(msg, cardID...)
	return CMAC(s.master, msg)
}
