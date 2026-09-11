package tapcrypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/subtle"
	"fmt"
)

// AES-CMAC, RFC 4493.
//
// The standard library has no CMAC, and NTAG 424 DNA's entire security model is
// CMAC over a session key derived by CMAC. Rather than pull a dependency into
// the one package where a supply-chain compromise would be indistinguishable
// from a working card, it is implemented here and pinned against the RFC's own
// test vectors in cmac_test.go.

// rb is the constant for the 128-bit block size (RFC 4493 §2.3).
const rb = 0x87

// cmacSubkeys derives K1 and K2 from the block cipher.
func cmacSubkeys(c cipher.Block) (k1, k2 []byte) {
	l := make([]byte, aes.BlockSize)
	c.Encrypt(l, make([]byte, aes.BlockSize))
	k1 = shiftLeft1(l)
	k2 = shiftLeft1(k1)
	return k1, k2
}

// shiftLeft1 shifts a 16-byte block left by one bit, XOR-ing in Rb when the bit
// shifted out was set. Written branch-free over the data so it does not leak the
// key's high bit through timing.
func shiftLeft1(in []byte) []byte {
	out := make([]byte, len(in))
	var carry byte
	for i := len(in) - 1; i >= 0; i-- {
		out[i] = in[i]<<1 | carry
		carry = in[i] >> 7
	}
	// carry is the bit shifted off the top: mask Rb in when it is 1.
	out[len(out)-1] ^= rb & -carry
	return out
}

// CMAC returns the full 16-byte AES-CMAC of msg under key.
func CMAC(key, msg []byte) ([]byte, error) {
	c, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("tapcrypto: cmac cipher: %w", err)
	}
	k1, k2 := cmacSubkeys(c)

	n := (len(msg) + aes.BlockSize - 1) / aes.BlockSize
	complete := n > 0 && len(msg)%aes.BlockSize == 0
	if n == 0 {
		n = 1
	}

	// The last block is the message tail XOR K1 when it is a whole block, or
	// padded with 0x80 00... and XOR K2 when it is not.
	last := make([]byte, aes.BlockSize)
	tail := msg[(n-1)*aes.BlockSize:]
	if complete {
		copy(last, tail)
		xorInto(last, k1)
	} else {
		copy(last, tail)
		last[len(tail)] = 0x80
		xorInto(last, k2)
	}

	x := make([]byte, aes.BlockSize)
	y := make([]byte, aes.BlockSize)
	for i := 0; i < n-1; i++ {
		copy(y, msg[i*aes.BlockSize:(i+1)*aes.BlockSize])
		xorInto(y, x)
		c.Encrypt(x, y)
	}
	xorInto(last, x)
	c.Encrypt(x, last)
	return x, nil
}

func xorInto(dst, src []byte) {
	for i := range dst {
		dst[i] ^= src[i]
	}
}

// truncateNXP reduces a 16-byte CMAC to the 8 bytes a tag actually transmits.
//
// NXP keeps the odd-indexed bytes — 1, 3, 5, 7, 9, 11, 13, 15 — rather than the
// leading half. Implementations that take the first 8 bytes verify nothing and
// reject every genuine tag, which is a confusing enough failure to be worth
// stating here.
func truncateNXP(mac []byte) []byte {
	out := make([]byte, 8)
	for i := 0; i < 8; i++ {
		out[i] = mac[2*i+1]
	}
	return out
}

// equalCT reports whether two byte slices match, in constant time.
func equalCT(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}
