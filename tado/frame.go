package tado

import (
	"crypto/aes"
	"encoding/hex"
	"errors"
	"fmt"
)

// A Data frame on air (0x69ec), with encrypted MAC FCS and without PHY CRC.
//
//	FCF(2) | seq(1) | dstEUI(8) | srcPrefix(5) | ciphertext | MIC(4)
//
// The security inputs are literally slices of the frame header:
//
//	nonce = frame[0:13]   aad = frame[0:16]   ct = frame[16:len-4]   MIC = frame[len-4:]
//
// The 4-byte internal frame counter is inside the plaintext (LE, offset 4:8),
// not in the nonce. See docs "MAC Layer" and "Chiavi e Cifratura".
const (
	headerLen = 16 // FCF(2)+seq(1)+dst(8)+srcPrefix(5)
	nonceLen  = 13
)

// Frame is a parsed/assembled Data frame.
type Frame struct {
	Seq       byte
	DstEUI    []byte // 8 bytes, wire (little-endian)
	SrcPrefix []byte // 5 bytes; full src EUI = SrcPrefix || EUISuffix
	Plaintext []byte // decrypted app payload (nil until decrypted)
	raw       []byte // full on-air bytes without PHY CRC
}

// SrcEUI returns the full 8-byte wire source address (prefix + shared suffix).
func (f *Frame) SrcEUI() []byte { return append(append([]byte{}, f.SrcPrefix...), EUISuffix...) }

// Bytes returns the on-air frame without PHY CRC.
func (f *Frame) Bytes() []byte { return f.raw }

// Hex returns the on-air frame as a hex string.
func (f *Frame) Hex() string { return hex.EncodeToString(f.raw) }

// BuildData assembles and encrypts a Data frame from app plaintext.
func BuildData(key []byte, seq byte, dstEUI, srcPrefix, plaintext []byte) (*Frame, error) {
	return buildData(fcfData, key, seq, dstEUI, srcPrefix, plaintext)
}

// BuildDataPending assembles a secured Data frame with the Frame Pending bit,
// which tells the peer that more frames wait for it.
func BuildDataPending(key []byte, seq byte, dstEUI, srcPrefix, plaintext []byte) (*Frame, error) {
	return buildData(fcfDataPending, key, seq, dstEUI, srcPrefix, plaintext)
}

func buildData(fcf [2]byte, key []byte, seq byte, dstEUI, srcPrefix, plaintext []byte) (*Frame, error) {
	if len(dstEUI) != 8 || len(srcPrefix) != 5 {
		return nil, errors.New("tado: dstEUI must be 8 bytes and srcPrefix 5 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	header := make([]byte, 0, headerLen)
	header = append(header, fcf[0], fcf[1], seq)
	header = append(header, dstEUI...)
	header = append(header, srcPrefix...)
	plaintext, err = setMACFCS(header, plaintext)
	if err != nil {
		return nil, err
	}
	ct, tag := ccmEncrypt(block, header[:nonceLen], header, plaintext)
	raw := append(append(append([]byte{}, header...), ct...), tag...)
	if len(raw) > 127 {
		return nil, fmt.Errorf("tado: Data frame is %d bytes, exceeds PHY limit 127", len(raw))
	}
	return &Frame{Seq: seq, DstEUI: dstEUI, SrcPrefix: srcPrefix, Plaintext: plaintext, raw: raw}, nil
}

// ParseData decrypts and parses an on-air Data frame without PHY CRC. It
// returns ErrMICFail when the MIC does not verify under key.
func ParseData(key, frame []byte) (*Frame, error) {
	if len(frame) < headerLen+micLen+1 {
		return nil, errors.New("tado: frame too short")
	}
	// Accept both Data (0x69ec) and Data-pending (0x79ec): same CCM* (the nonce is
	// frame[0:13], so the real FCF bytes are already in it). Data-pending carries
	// the standard 802.15.4 Frame Pending bit, which says the sender has more
	// frames queued for the same peer. It does not mark a 6LoWPAN fragment: only
	// the plaintext dispatch does, which IsFragment reads.
	if !(frame[0] == fcfData[0] && frame[1] == fcfData[1]) &&
		!(frame[0] == fcfDataPending[0] && frame[1] == fcfDataPending[1]) {
		return nil, errors.New("tado: not a Data (0x69ec/0x79ec) frame")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	nonce := frame[0:nonceLen]
	aad := frame[0:headerLen]
	ct := frame[headerLen : len(frame)-micLen]
	tag := frame[len(frame)-micLen:]
	pt, ok := ccmDecrypt(block, nonce, aad, ct, tag)
	if !ok {
		return nil, ErrMICFail
	}
	if !validMACFCS(aad, pt) {
		return nil, ErrMACFCS
	}
	return &Frame{
		Seq:       frame[2],
		DstEUI:    append([]byte{}, frame[3:11]...),
		SrcPrefix: append([]byte{}, frame[11:16]...),
		Plaintext: pt,
		raw:       append([]byte{}, frame...),
	}, nil
}

// ErrMICFail means the CCM* tag did not verify (wrong key or corrupt frame).
var ErrMICFail = errors.New("tado: MIC verification failed")

var ErrMACFCS = errors.New("tado: MAC FCS verification failed")

func validMACFCS(header, plaintext []byte) bool {
	if len(plaintext) < 2 {
		return false
	}
	input := make([]byte, 0, len(header)+len(plaintext)-2)
	input = append(input, header...)
	input = append(input, plaintext[:len(plaintext)-2]...)
	want := macFCS(input)
	return plaintext[len(plaintext)-2] == want[0] && plaintext[len(plaintext)-1] == want[1]
}
