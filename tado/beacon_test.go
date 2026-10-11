package tado

import (
	"bytes"
	"crypto/aes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"testing"
)

// beaconRequest builds an Enhanced Beacon Request as a joining valve sends it
// (the pairing capture): a secured MAC command to
// the broadcast address whose command id sits in the ciphertext.
func beaconRequest(t *testing.T, key []byte, seq byte, src []byte, ctr uint32, cmd byte) []byte {
	t.Helper()
	header := []byte{0x0b, 0xe8, seq, 0xff, 0xff, 0xff, 0xff}
	header = append(header, src...)
	header = append(header, 0x04)
	pt := binary.LittleEndian.AppendUint32(nil, ctr)
	pt, err := setMACFCS(header, append(pt, cmd, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	ct, tag := ccmEncrypt(block, header[:nonceLen], header, pt)
	return append(append(header, ct...), tag...)
}

func TestClassifyJoinFrames(t *testing.T) {
	cases := []struct {
		hex  string
		want FrameClass
		name string
	}{
		{"0be813ffffffff", ClassBeaconRequest, "BeaconReq"},
		{"18eceecdab", ClassBeacon, "EnhBeacon"},
		{"59e8f1ffff", ClassBroadcast, "Broadcast"},
	}
	for _, c := range cases {
		got := Classify(mustB(c.hex))
		if got != c.want || got.String() != c.name {
			t.Errorf("Classify(%s)=%v (%s), want %v (%s)", c.hex, got, got, c.want, c.name)
		}
	}
}

// TestParseBeaconRequest: the request verifies under the key it was sealed
// with, names its sender and its counter, and is refused under another key or
// with another command.
func TestParseBeaconRequest(t *testing.T) {
	frame := beaconRequest(t, PairingKey, 0x7b, joinerEUI, 16, 0x07)
	// Also the request of the firmware v2.9 test vector (TestBuildEnhancedBeacon).
	if got := hex.EncodeToString(frame); got != "0be87bffffffff0bb1cefa07c51b0004581463f90147ed74caf1bd" {
		t.Fatalf("request %s", got)
	}
	r, err := ParseBeaconRequest(PairingKey, frame)
	if err != nil {
		t.Fatal(err)
	}
	if r.Seq != 0x7b || !bytes.Equal(r.SrcEUI, joinerEUI) || r.FrameCounter != 16 {
		t.Errorf("request %+v", r)
	}
	if _, err := ParseBeaconRequest(NetworkKey, frame); !errors.Is(err, ErrMICFail) {
		t.Errorf("wrong key: %v, want ErrMICFail", err)
	}
	if _, err := ParseBeaconRequest(PairingKey, beaconRequest(t, PairingKey, 0x7b, joinerEUI, 16, 0x04)); err == nil {
		t.Error("a MAC command other than 0x07 parsed as a beacon request")
	}
	if _, err := ParseBeaconRequest(PairingKey, frame[:26]); err == nil {
		t.Error("a short request parsed")
	}
}

// TestBuildEnhancedBeacon pins the answer layout of the capture
// (the pairing capture), rebuilt with a synthetic
// identity: FCF 18ec, the bridge seq, PAN abcd, the requester, the bridge, then
// the security control, the frame counter and the MAC FCS, sealed from byte 16
// on. No IE and no payload. With the real identity this builder gives the
// three captured beacons byte for byte. The hex is also the test vector of the
// beacon builder in bridge firmware v2.9.
func TestBuildEnhancedBeacon(t *testing.T) {
	eb, err := BuildEnhancedBeacon(PairingKey, 0x59, joinerEUI, BridgeEUI, 13537)
	if err != nil {
		t.Fatal(err)
	}
	const want = "18ec59cdab0bb1cefa07c51b0009a1ce085a0864b7719c19e34285c9cded7ec2"
	if got := hex.EncodeToString(eb); got != want {
		t.Errorf("beacon\n got %s\nwant %s", got, want)
	}
	if len(eb) != 32 || Classify(eb) != ClassBeacon {
		t.Fatalf("beacon %x: %d bytes, class %v", eb, len(eb), Classify(eb))
	}
	if eb[2] != 0x59 || binary.LittleEndian.Uint16(eb[3:5]) != PANID ||
		!bytes.Equal(eb[5:13], joinerEUI) || !bytes.Equal(eb[13:16], BridgeEUI[:3]) {
		t.Errorf("clear header %x", eb[:16])
	}
	block, err := aes.NewCipher(PairingKey)
	if err != nil {
		t.Fatal(err)
	}
	pt, ok := ccmDecrypt(block, eb[:nonceLen], eb[:headerLen], eb[headerLen:len(eb)-micLen], eb[len(eb)-micLen:])
	if !ok {
		t.Fatal("MIC does not verify under the pairing key")
	}
	if !bytes.Equal(pt[:5], BridgeEUI[3:]) || pt[5] != 0x04 || binary.LittleEndian.Uint32(pt[6:10]) != 13537 {
		t.Errorf("sealed part %x", pt)
	}
	if !validMACFCS(eb[:headerLen], pt) {
		t.Error("MAC FCS does not verify")
	}
	if _, err := BuildEnhancedBeacon(PairingKey, 1, joinerEUI[:5], BridgeEUI, 1); err == nil {
		t.Error("a short destination built")
	}
}

// TestBuildBroadcastPending: the Frame Pending broadcast carries FCF 59e8, the
// same payload, and parses like the plain one.
func TestBuildBroadcastPending(t *testing.T) {
	payload := WrapBroadcastPayload([]byte{0x7b, 0x3b, nhICMPv6, 0x01, icmpRouterAdvert, 0, 0, 0}, 0)
	plain, err := BuildBroadcast(NetworkKey, 0xf1, BridgeEUI, 12910, bcastSecCtrl, payload)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := BuildBroadcastPending(NetworkKey, 0xf1, BridgeEUI, 12910, bcastSecCtrl, payload)
	if err != nil {
		t.Fatal(err)
	}
	if pending[0] != 0x59 || pending[1] != 0xe8 || !bytes.Equal(pending[2:headerLen], plain[2:headerLen]) {
		t.Fatalf("pending header %x, plain %x", pending[:headerLen], plain[:headerLen])
	}
	a, err := ParseBroadcast(NetworkKey, plain)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseBroadcast(NetworkKey, pending)
	if err != nil {
		t.Fatalf("pending broadcast does not parse: %v", err)
	}
	if !bytes.Equal(a.Plaintext[:len(a.Plaintext)-2], b.Plaintext[:len(b.Plaintext)-2]) {
		t.Errorf("payload differs: %x vs %x", a.Plaintext, b.Plaintext)
	}
}
