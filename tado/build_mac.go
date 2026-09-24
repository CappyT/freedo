package tado

import (
	"crypto/aes"
	"encoding/binary"
	"errors"
)

// MAC-layer frame builders for the bridge emulator: the CSL wake-up train, the
// Enhanced ACK answered at the rendezvous, and the broadcast Data frame. The
// unicast Data frame is built by BuildData in frame.go.
//
// Every frame here is produced WITHOUT the on-air FCS: the CC1101 appends the
// CRC-16 in hardware.

// fcfBroadcast is the Frame Control of the 0x49e8 broadcast Data frame, which
// carries an aux security header on the wire instead of the implicit one.
var fcfBroadcast = [2]byte{0x49, 0xe8}

var (
	ieRendezBytes = mustHex(ieRendezvousTime)
	ieCSLBytes    = mustHex(ieCSL)
	ieTermBytes   = mustHex(ieHeaderTerm)
)

const RouterAdvertWakeUpCount = 254

// BuildWakeUp builds a 12-byte Multipurpose CSL wake-up frame:
//
//	MPFC(1) Seq(1) PANID(2 LE) DstShort(2) RendezIE(2) RendezTime(2 LE) TermIE(2)
//
// dstShort is written verbatim in inventory-short order (ffff = broadcast).
func BuildWakeUp(seq byte, dstShort []byte, rendezTime uint16) []byte {
	if len(dstShort) != 2 {
		panic("tado: dstShort must be 2 bytes")
	}
	f := make([]byte, 0, 12)
	f = append(f, fcfShort, seq)
	f = binary.LittleEndian.AppendUint16(f, PANID)
	f = append(f, dstShort...)
	f = append(f, ieRendezBytes...)
	f = binary.LittleEndian.AppendUint16(f, rendezTime)
	return append(f, ieTermBytes...)
}

// BuildWakeUpTrain builds a descending-RendezvousTime wake-up burst: count
// frames, all sharing seq and dstShort, RendezvousTime stepping from
// startRendez down toward 0 by step (clamped at 0, stops at 0).
func BuildWakeUpTrain(seq byte, dstShort []byte, count int, startRendez, step uint16) [][]byte {
	if count <= 0 {
		return nil
	}
	out := make([][]byte, 0, count)
	r := startRendez
	for i := 0; i < count; i++ {
		out = append(out, BuildWakeUp(seq, dstShort, r))
		if r < step {
			r = 0
		} else {
			r -= step
		}
	}
	return out
}

func BuildRouterAdvertWakeUpBurst(seq byte) [][]byte {
	out := make([][]byte, 0, RouterAdvertWakeUpCount)
	for i := 0; i < RouterAdvertWakeUpCount; i++ {
		rendez := uint16(0)
		if i < 253 {
			rendez = uint16(1 + uint32(CSLPeriodUnits-1)*uint32(252-i)/252)
		}
		out = append(out, BuildWakeUp(seq, []byte{0xff, 0xff}, rendez))
	}
	return out
}

// BuildEnhAck builds a 29-byte Enhanced ACK (unsecured):
//
//	FCF(2) Seq(1) Dst(8) Src(8) CSL-IE(2) Phase(2 LE) Period(2 LE) TermIE(2) FCS(2)
//
// CSL-Period is fixed to CSLPeriodUnits. The last two bytes are the software MAC
// FCS over the preceding 27; the valve drops the ACK when it mismatches.
func BuildEnhAck(seq byte, dstEUI, srcEUI []byte, cslPhase uint16) []byte {
	if len(dstEUI) != 8 || len(srcEUI) != 8 {
		panic("tado: dstEUI and srcEUI must be 8 bytes")
	}
	f := make([]byte, 0, 29)
	f = append(f, fcfEnhAck[0], fcfEnhAck[1], seq)
	f = append(f, dstEUI...)
	f = append(f, srcEUI...)
	f = append(f, ieCSLBytes...)
	f = binary.LittleEndian.AppendUint16(f, cslPhase)
	f = binary.LittleEndian.AppendUint16(f, CSLPeriodUnits)
	f = append(f, ieTermBytes...)
	return append(f, macFCS(f)...)
}

// macFCS is the 802.15.4 MAC FCS the Enhanced ACK carries in software:
// CRC-16/CCITT reflected (poly 0x8408), init 0x0000, no final XOR, emitted LE.
func macFCS(b []byte) []byte {
	crc := uint16(0x0000)
	for _, x := range b {
		crc ^= uint16(x)
		for i := 0; i < 8; i++ {
			if crc&1 != 0 {
				crc = (crc >> 1) ^ 0x8408
			} else {
				crc >>= 1
			}
		}
	}
	return []byte{byte(crc), byte(crc >> 8)}
}

func setMACFCS(header, plaintext []byte) ([]byte, error) {
	if len(plaintext) < 2 {
		return nil, errors.New("tado: plaintext must reserve a 2-byte MAC FCS")
	}
	out := append([]byte(nil), plaintext...)
	input := make([]byte, 0, len(header)+len(out)-2)
	input = append(input, header...)
	input = append(input, out[:len(out)-2]...)
	copy(out[len(out)-2:], macFCS(input))
	return out, nil
}

// BuildBroadcast builds a 0x49e8 broadcast Data frame:
//
//	FCF(2) Seq(1) DstShort(2=ffff) Src(8) SecCtrl(1) FrameCounter(2 LE) ct MIC(4)
//
// As for unicast Data the security inputs are slices of the header itself:
// nonce = header[0:13], aad = header[0:16].
func BuildBroadcast(key []byte, seq byte, srcEUI []byte, frameCounter uint16, secCtrl byte, plaintext []byte) ([]byte, error) {
	if len(srcEUI) != 8 {
		return nil, errors.New("tado: srcEUI must be 8 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	header := make([]byte, 0, headerLen)
	header = append(header, fcfBroadcast[0], fcfBroadcast[1], seq, 0xff, 0xff)
	header = append(header, srcEUI...)
	header = append(header, secCtrl)
	header = binary.LittleEndian.AppendUint16(header, frameCounter)
	plaintext, err = setMACFCS(header, plaintext)
	if err != nil {
		return nil, err
	}
	ct, tag := ccmEncrypt(block, header[:nonceLen], header, plaintext)
	return append(append(append([]byte{}, header...), ct...), tag...), nil
}
