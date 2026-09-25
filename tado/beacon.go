package tado

import (
	"crypto/aes"
	"encoding/binary"
	"errors"
)

// A joining valve looks for a coordinator with Enhanced Beacon Requests under
// PairingKey, one on this channel every 8 s (coap_sniff_1786619225 lines 2900,
// 3539, 34935, 35538). The commercial bridge answers each with an Enhanced
// Beacon unicast to the valve under the same key, with no wake-up train: the
// beacon ends 9 to 14 ms after the request (lines 3540, 34936, 35539), so it
// starts 2.4 to 7.4 ms after it at 50 kBaud. After a beacon the valve sends its
// Router Solicitation under the pairing key.
//
//	request  FCF 0be8(2) Seq(1) DstPAN ffff(2) Dst ffff(2) Src(8) SecCtrl 04(1) | Ctr(4 LE) Cmd 07(1) FCS(2) | MIC(4)
//	beacon   FCF 18ec(2) Seq(1) DstPAN abcd(2 LE) Dst(8) Src(8) SecCtrl 04(1) Ctr(4 LE) FCS(2) | MIC(4)
//
// As in a Data frame the security inputs are slices of the frame: nonce =
// frame[0:13], aad = frame[0:16], and the bytes from 16 up to the MIC are the
// ciphertext. The command id of the request sits in the ciphertext. The beacon
// carries no IE and no payload; its seq and frame counter are the next ones of
// the bridge.
const (
	beaconRequestLen = 27
	enhBeaconLen     = 32
	macCmdBeaconReq  = 0x07
)

var (
	fcfBeaconRequest = [2]byte{0x0b, 0xe8}
	fcfEnhBeacon     = [2]byte{0x18, 0xec}
)

// BeaconRequest is a verified Enhanced Beacon Request.
type BeaconRequest struct {
	Seq          byte
	SrcEUI       []byte // 8-byte wire EUI of the device that looks for a coordinator
	FrameCounter uint32
}

// ParseBeaconRequest verifies an Enhanced Beacon Request under key. It returns
// ErrMICFail when the MIC does not verify.
func ParseBeaconRequest(key, frame []byte) (*BeaconRequest, error) {
	if len(frame) != beaconRequestLen || frame[0] != fcfBeaconRequest[0] || frame[1] != fcfBeaconRequest[1] {
		return nil, errors.New("tado: not a 27-byte Enhanced Beacon Request")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	pt, ok := ccmDecrypt(block, frame[:nonceLen], frame[:headerLen], frame[headerLen:len(frame)-micLen], frame[len(frame)-micLen:])
	if !ok {
		return nil, ErrMICFail
	}
	if !validMACFCS(frame[:headerLen], pt) {
		return nil, ErrMACFCS
	}
	if pt[4] != macCmdBeaconReq {
		return nil, errors.New("tado: MAC command is not an Enhanced Beacon Request")
	}
	return &BeaconRequest{
		Seq:          frame[2],
		SrcEUI:       append([]byte{}, frame[7:15]...),
		FrameCounter: binary.LittleEndian.Uint32(pt[:4]),
	}, nil
}

// BuildEnhancedBeacon builds the 32-byte Enhanced Beacon that answers a Beacon
// Request of dstEUI, from srcEUI, under key.
func BuildEnhancedBeacon(key []byte, seq byte, dstEUI, srcEUI []byte, frameCounter uint32) ([]byte, error) {
	if len(dstEUI) != 8 || len(srcEUI) != 8 {
		return nil, errors.New("tado: dstEUI and srcEUI must be 8 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	f := make([]byte, 0, enhBeaconLen)
	f = append(f, fcfEnhBeacon[0], fcfEnhBeacon[1], seq)
	f = binary.LittleEndian.AppendUint16(f, PANID)
	f = append(f, dstEUI...)
	f = append(f, srcEUI...)
	f = append(f, bcastSecCtrl)
	f = binary.LittleEndian.AppendUint32(f, frameCounter)
	pt, err := setMACFCS(f[:headerLen], append(append([]byte{}, f[headerLen:]...), 0, 0))
	if err != nil {
		return nil, err
	}
	ct, tag := ccmEncrypt(block, f[:nonceLen], f[:headerLen], pt)
	return append(append(f[:headerLen:headerLen], ct...), tag...), nil
}
