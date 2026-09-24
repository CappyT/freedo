package tado

import "crypto/cipher"

// CCM* (IEEE 802.15.4 security) as the Tado link uses it: AES-128, MIC-32,
// L=2, B0 flags 0x49 (Adata=1, M=4, L=2). Encryption is AES-CTR and the MIC is
// a CBC-MAC over B0 | encoded-AAD | plaintext, tag = X[:4] XOR E(A0).
const (
	micLen  = 4    // M
	lenSize = 2    // L
	b0Flags = 0x49 // Adata=1, M=4, L=2
	ctrFlag = lenSize - 1
)

// keystream returns the first n bytes of the CTR keystream (counter blocks A1..).
// A_i = [ctrFlag][nonce(13)][i(2 BE)], i starting at 1.
func keystream(block cipher.Block, nonce []byte, n int) []byte {
	out := make([]byte, 0, (n+15)/16*16)
	var a [16]byte
	a[0] = ctrFlag
	copy(a[1:14], nonce)
	for i := 1; len(out) < n; i++ {
		a[14] = byte(i >> 8)
		a[15] = byte(i)
		var ks [16]byte
		block.Encrypt(ks[:], a[:])
		out = append(out, ks[:]...)
	}
	return out[:n]
}

// mic computes the 4-byte CCM* tag over (nonce, aad, pt).
func mic(block cipher.Block, nonce, aad, pt []byte) []byte {
	var data []byte
	// B0 = flags | nonce(13) | len(pt) as L bytes BE
	b0 := make([]byte, 16)
	b0[0] = b0Flags
	copy(b0[1:14], nonce)
	b0[14] = byte(len(pt) >> 8)
	b0[15] = byte(len(pt))
	data = append(data, b0...)
	// AAD block: 2-byte BE length prefix then aad, zero-padded to a 16-byte boundary.
	a := make([]byte, 0, 16)
	a = append(a, byte(len(aad)>>8), byte(len(aad)))
	a = append(a, aad...)
	data = append(data, padTo16(a)...)
	// Plaintext, zero-padded to a 16-byte boundary.
	data = append(data, padTo16(pt)...)

	var x [16]byte
	var tmp [16]byte
	for j := 0; j < len(data); j += 16 {
		for k := 0; k < 16; k++ {
			tmp[k] = x[k] ^ data[j+k]
		}
		block.Encrypt(x[:], tmp[:])
	}
	// S0 = E([ctrFlag][nonce][0]) ; tag = X[:4] XOR S0[:4]
	var a0 [16]byte
	a0[0] = ctrFlag
	copy(a0[1:14], nonce)
	var s0 [16]byte
	block.Encrypt(s0[:], a0[:])
	out := make([]byte, micLen)
	for i := 0; i < micLen; i++ {
		out[i] = x[i] ^ s0[i]
	}
	return out
}

// ccmEncrypt returns (ciphertext, tag) for pt under (nonce, aad).
func ccmEncrypt(block cipher.Block, nonce, aad, pt []byte) (ct, tag []byte) {
	tag = mic(block, nonce, aad, pt)
	ks := keystream(block, nonce, len(pt))
	ct = make([]byte, len(pt))
	for i := range pt {
		ct[i] = pt[i] ^ ks[i]
	}
	return ct, tag
}

// ccmDecrypt returns (plaintext, ok) for ct under (nonce, aad); ok is false on
// MIC mismatch (wrong key or corrupt frame).
func ccmDecrypt(block cipher.Block, nonce, aad, ct, wantTag []byte) (pt []byte, ok bool) {
	ks := keystream(block, nonce, len(ct))
	pt = make([]byte, len(ct))
	for i := range ct {
		pt[i] = ct[i] ^ ks[i]
	}
	return pt, bytesEqual(mic(block, nonce, aad, pt), wantTag)
}

func padTo16(b []byte) []byte {
	if r := len(b) % 16; r != 0 {
		return append(b, make([]byte, 16-r)...)
	}
	return b
}

// bytesEqual is a plain compare; the MIC is not a secret so constant time is not
// required here.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
