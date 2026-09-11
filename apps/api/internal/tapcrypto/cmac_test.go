package tapcrypto

import (
	"encoding/hex"
	"testing"
)

// The RFC 4493 §4 test vectors. If these drift, every NTAG 424 tap in the field
// stops verifying at once, so they are pinned exactly as published.
func TestCMACRFC4493(t *testing.T) {
	key, _ := hex.DecodeString("2b7e151628aed2a6abf7158809cf4f3c")
	msg, _ := hex.DecodeString(
		"6bc1bee22e409f96e93d7e117393172a" +
			"ae2d8a571e03ac9c9eb76fac45af8e51" +
			"30c81c46a35ce411e5fbc1191a0a52ef" +
			"f69f2445df4f9b17ad2b417be66c3710")

	for _, tc := range []struct {
		name string
		n    int
		want string
	}{
		{"empty", 0, "bb1d6929e95937287fa37d129b756746"},
		{"one block", 16, "070a16b46b4d4144f79bdd9dd04a287c"},
		{"partial block", 40, "dfa66747de9ae63030ca32611497c827"},
		{"four blocks", 64, "51f0bebf7e3b9d92fc49741779363cfe"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CMAC(key, msg[:tc.n])
			if err != nil {
				t.Fatal(err)
			}
			if hex.EncodeToString(got) != tc.want {
				t.Errorf("CMAC over %d bytes = %s, want %s", tc.n, hex.EncodeToString(got), tc.want)
			}
		})
	}
}

func TestCMACRejectsBadKeyLength(t *testing.T) {
	if _, err := CMAC([]byte{1, 2, 3}, nil); err == nil {
		t.Fatal("a 3-byte key must be rejected")
	}
}

// NXP truncation keeps the odd-indexed bytes. Taking the first eight is the
// classic implementation bug and it fails closed on every real tag.
func TestTruncateNXPKeepsOddBytes(t *testing.T) {
	full, _ := hex.DecodeString("000102030405060708090a0b0c0d0e0f")
	got := hex.EncodeToString(truncateNXP(full))
	if want := "01030507090b0d0f"; got != want {
		t.Errorf("truncateNXP = %s, want %s", got, want)
	}
}

func TestEqualCT(t *testing.T) {
	if !equalCT([]byte("abcd"), []byte("abcd")) {
		t.Error("identical slices must compare equal")
	}
	if equalCT([]byte("abcd"), []byte("abce")) {
		t.Error("differing slices must not compare equal")
	}
	if equalCT([]byte("abcd"), []byte("abc")) {
		t.Error("different lengths must not compare equal")
	}
}
