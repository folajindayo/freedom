package tapcrypto

import (
	"encoding/hex"
	"fmt"
	"net/url"
	"testing"
)

func testKeys(t *testing.T) *SoftwareKeyStore {
	t.Helper()
	master, _ := hex.DecodeString("000102030405060708090a0b0c0d0e0f")
	ks, err := NewSoftwareKeyStore(master)
	if err != nil {
		t.Fatal(err)
	}
	return ks
}

// tag424 simulates a personalised tag: it produces exactly the URL a real one
// would mirror. Without an independent producer, a verifier that accepts
// nothing would pass every negative test.
func tag424(t *testing.T, ks *SoftwareKeyStore, cardID string, uid []byte, ctr uint32) url.Values {
	t.Helper()
	metaKey, err := ks.MetaReadKey(cardID)
	if err != nil {
		t.Fatal(err)
	}
	fileKey, err := ks.FileReadKey(cardID)
	if err != nil {
		t.Fatal(err)
	}
	picc, err := EncodePICCData(metaKey, uid, ctr)
	if err != nil {
		t.Fatal(err)
	}
	mac, err := SignSDM(fileKey, uid, ctr, nil)
	if err != nil {
		t.Fatal(err)
	}
	return url.Values{
		"picc": {hex.EncodeToString(picc)},
		"cmac": {hex.EncodeToString(mac)},
	}
}

func TestNTAG424AcceptsGenuineTap(t *testing.T) {
	ks := testKeys(t)
	v := &NTAG424{Keys: ks}
	uid := []byte{0x04, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66}
	state := CardState{CardID: "card-1", Tech: TechNTAG424, Status: StatusActive, UID: uid, LastCounter: 7}

	p, err := v.Parse(tag424(t, ks, "card-1", uid, 8))
	if err != nil {
		t.Fatal(err)
	}
	res := v.Verify(state, p)
	if !res.OK {
		t.Fatalf("genuine tap declined: %s", res.Reason)
	}
	if res.Counter != 8 {
		t.Fatalf("counter %d, want 8", res.Counter)
	}
	// Parse must not have trusted the UID before decryption.
	if len(p.UID) != 0 {
		t.Error("Parse exposed a UID it had not yet decrypted")
	}
}

// A captured URL replayed later presents a counter already seen. This is the
// attack the counter exists to stop, and it must freeze the card, not merely
// decline.
func TestNTAG424ReplayIsDetectedAsClone(t *testing.T) {
	ks := testKeys(t)
	v := &NTAG424{Keys: ks}
	uid := []byte{0x04, 1, 2, 3, 4, 5, 6}
	captured := tag424(t, ks, "card-1", uid, 42)

	for _, last := range []uint32{42, 43, 100} {
		state := CardState{CardID: "card-1", Tech: TechNTAG424, Status: StatusActive, UID: uid, LastCounter: last}
		p, _ := v.Parse(captured)
		res := v.Verify(state, p)
		if res.OK {
			t.Fatalf("replay at counter 42 accepted when last seen was %d", last)
		}
		if !res.CloneSuspected {
			t.Errorf("replay at last=%d should be a clone signal, got %s", last, res.Reason)
		}
	}
}

// Every bit of the message is covered by the CMAC. Flipping any of it must fail.
func TestNTAG424RejectsTamperedFields(t *testing.T) {
	ks := testKeys(t)
	v := &NTAG424{Keys: ks}
	uid := []byte{0x04, 1, 2, 3, 4, 5, 6}
	state := CardState{CardID: "card-1", Tech: TechNTAG424, Status: StatusActive, UID: uid, LastCounter: 0}

	for _, field := range []string{"picc", "cmac"} {
		t.Run(field, func(t *testing.T) {
			q := tag424(t, ks, "card-1", uid, 9)
			raw, _ := hex.DecodeString(q.Get(field))
			raw[0] ^= 0x01
			q.Set(field, hex.EncodeToString(raw))

			p, err := v.Parse(q)
			if err != nil {
				return // a malformed blob rejected at parse is also a pass
			}
			if res := v.Verify(state, p); res.OK {
				t.Fatalf("a tampered %s was accepted", field)
			}
		})
	}
}

// A tag personalised for another card must not verify here, even though the
// blob is structurally perfect and signed by a real key.
func TestNTAG424RejectsAnotherCardsTag(t *testing.T) {
	ks := testKeys(t)
	v := &NTAG424{Keys: ks}
	uidA := []byte{0x04, 1, 2, 3, 4, 5, 6}

	q := tag424(t, ks, "card-2", uidA, 5)
	state := CardState{CardID: "card-1", Tech: TechNTAG424, Status: StatusActive, UID: uidA, LastCounter: 0}
	p, _ := v.Parse(q)
	if res := v.Verify(state, p); res.OK {
		t.Fatal("a tag keyed to card-2 verified against card-1")
	}
}

// A bad CMAC is usually a personalisation error, not an attack. Freezing on it
// would turn one mis-keyed batch into a mass lockout.
func TestNTAG424BadCMACDoesNotFreeze(t *testing.T) {
	ks := testKeys(t)
	v := &NTAG424{Keys: ks}
	uid := []byte{0x04, 1, 2, 3, 4, 5, 6}
	q := tag424(t, ks, "card-1", uid, 5)
	q.Set("cmac", "0000000000000000")

	p, _ := v.Parse(q)
	res := v.Verify(CardState{CardID: "card-1", Tech: TechNTAG424, Status: StatusActive, UID: uid}, p)
	if res.OK {
		t.Fatal("a zero CMAC was accepted")
	}
	if res.Reason != ReasonBadCMAC {
		t.Fatalf("reason %s, want %s", res.Reason, ReasonBadCMAC)
	}
	if res.CloneSuspected {
		t.Error("a bad CMAC must not freeze the card")
	}
}

// Per-card diversification: two cards under one master must not share a key,
// and the two keys of one card must be unrelated.
func TestKeyDiversification(t *testing.T) {
	ks := testKeys(t)
	a1, _ := ks.MetaReadKey("card-1")
	a2, _ := ks.FileReadKey("card-1")
	b1, _ := ks.MetaReadKey("card-2")

	if equalCT(a1, a2) {
		t.Error("a card's meta-read and file-read keys are identical")
	}
	if equalCT(a1, b1) {
		t.Error("two cards share a meta-read key")
	}
	if len(a1) != 16 {
		t.Fatalf("derived key is %d bytes, want 16", len(a1))
	}
	// Stable across calls, or cards stop verifying after a restart.
	again, _ := ks.MetaReadKey("card-1")
	if !equalCT(a1, again) {
		t.Error("key derivation is not deterministic")
	}
}

func TestKeyStoreRefusesWithoutMaster(t *testing.T) {
	t.Setenv("OJA_TEST_MASTER", "")
	if _, err := SoftwareKeyStoreFromEnv("OJA_TEST_MASTER"); err == nil {
		t.Fatal("a missing master key must refuse to start, not invent one")
	}
	t.Setenv("OJA_TEST_MASTER", "zzzz")
	if _, err := SoftwareKeyStoreFromEnv("OJA_TEST_MASTER"); err == nil {
		t.Fatal("a non-hex master key must be rejected")
	}
	if _, err := NewSoftwareKeyStore(make([]byte, 8)); err == nil {
		t.Fatal("an 8-byte master key must be rejected")
	}
}

// The counter is 24 bits on the tag. Asking for more must fail loudly at
// personalisation rather than silently wrapping in the field.
func TestPICCCounterRangeIsEnforced(t *testing.T) {
	ks := testKeys(t)
	key, _ := ks.MetaReadKey("card-1")
	uid := []byte{0x04, 1, 2, 3, 4, 5, 6}
	if _, err := EncodePICCData(key, uid, 0xFFFFFF); err != nil {
		t.Fatalf("the maximum 24-bit counter must encode: %v", err)
	}
	if _, err := EncodePICCData(key, uid, 0x1000000); err == nil {
		t.Fatal("a counter beyond 24 bits must be rejected")
	}
}

// Round-trip every counter boundary: the little-endian packing is easy to get
// backwards and the failure only shows up past 255 taps.
func TestPICCDataRoundTrip(t *testing.T) {
	ks := testKeys(t)
	key, _ := ks.MetaReadKey("card-1")
	uid := []byte{0x04, 0xAB, 0xCD, 0xEF, 0x11, 0x22, 0x33}

	for _, ctr := range []uint32{0, 1, 255, 256, 65535, 65536, 0xFFFFFE, 0xFFFFFF} {
		t.Run(fmt.Sprint(ctr), func(t *testing.T) {
			blob, err := EncodePICCData(key, uid, ctr)
			if err != nil {
				t.Fatal(err)
			}
			gotUID, gotCtr, err := decryptPICCData(key, blob)
			if err != nil {
				t.Fatal(err)
			}
			if gotCtr != ctr {
				t.Errorf("counter round-tripped to %d, want %d", gotCtr, ctr)
			}
			if !equalCT(gotUID, uid) {
				t.Errorf("UID round-tripped to %x, want %x", gotUID, uid)
			}
		})
	}
}
