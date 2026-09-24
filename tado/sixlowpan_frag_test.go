package tado

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"time"
)

func fragmentedTestPacket(t *testing.T, payload []byte) []byte {
	t.Helper()
	pkt, err := BuildCoAP(CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x7101, Token: []byte{0x71},
		Path: []string{"d", "config"}, Query: []string{"id=10"},
		Options: []CoAPOption{optUriPort, optOctets}, Payload: payload,
		SrcEUI: BridgeEUI, DstEUI: testDstEUI(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	return pkt
}

func parsedFragments(t *testing.T, fragments [][]byte) []*Frame {
	t.Helper()
	dst := testDstEUI(t)
	out := make([]*Frame, 0, len(fragments))
	for i, fragment := range fragments {
		f, err := BuildDataPending(NetworkKey, byte(i), dst, BridgeEUI[:5], WrapAppPayload(fragment, uint32(i+1), nil))
		if err != nil {
			t.Fatalf("fragment %d: %v", i, err)
		}
		parsed, err := ParseData(NetworkKey, f.Bytes())
		if err != nil {
			t.Fatalf("parse fragment %d: %v", i, err)
		}
		out = append(out, parsed)
	}
	return out
}

func fragmentedFrames(t *testing.T, payload []byte, tag uint16) []*Frame {
	t.Helper()
	fragments, err := Fragment6LoWPANDatagram(fragmentedTestPacket(t, payload), tag)
	if err != nil {
		t.Fatal(err)
	}
	return parsedFragments(t, fragments)
}

func completeFragments(t *testing.T, ra *Reassembler, frames []*Frame) *CoAPMessage {
	t.Helper()
	var got *CoAPMessage
	for _, f := range frames {
		if m, done := ra.Add(f); done {
			got = m
		}
	}
	return got
}

func TestFragment6LoWPANDatagramReassemblesOutOfOrder(t *testing.T) {
	dst := testDstEUI(t)
	payload := bytes.Repeat([]byte{0xa5}, 260)
	pkt, err := BuildCoAP(CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x7101, Token: []byte{0x71},
		Path: []string{"d", "config"}, Query: []string{"id=10"},
		Options: []CoAPOption{optUriPort, optOctets}, Payload: payload,
		SrcEUI: BridgeEUI, DstEUI: dst,
	})
	if err != nil {
		t.Fatal(err)
	}
	fragments, err := Fragment6LoWPANDatagram(pkt, 0x7788)
	if err != nil {
		t.Fatal(err)
	}
	if len(fragments) < 3 {
		t.Fatalf("fragments=%d, want at least 3", len(fragments))
	}
	if got := fragments[0][4]; got != iphcLinkLocal0 {
		t.Errorf("FRAG1 starts %02x, want IPHC %02x", got, iphcLinkLocal0)
	}
	if got := fragments[1][4]; got != 16 {
		t.Errorf("first FRAGN offset=%d, want 16", got)
	}

	ra := NewReassembler()
	var got *CoAPMessage
	for i := len(fragments) - 1; i >= 0; i-- {
		f, err := BuildDataPending(NetworkKey, byte(i), dst, BridgeEUI[:5], WrapAppPayload(fragments[i], uint32(i+1), nil))
		if err != nil {
			t.Fatalf("fragment %d: %v", i, err)
		}
		parsed, err := ParseData(NetworkKey, f.Bytes())
		if err != nil {
			t.Fatalf("parse fragment %d: %v", i, err)
		}
		if m, done := ra.Add(parsed); done {
			got = m
		}
	}
	if got == nil {
		t.Fatal("out-of-order fragments did not reassemble")
	}
	if !bytes.Equal(got.Payload, payload) {
		t.Errorf("payload length=%d, want %d", len(got.Payload), len(payload))
	}
}

func TestFragment6LoWPANDatagramRespectsValveMTU(t *testing.T) {
	packet := make([]byte, maxFragmentIPv6Datagram-ipv6UDPHeaderLen+9)
	packet[0] = iphcLinkLocal0
	packet[1] = iphcLinkLocal1
	packet[2] = nhcUDPPortsIn
	fragments, err := Fragment6LoWPANDatagram(packet, 0x1200)
	if err != nil {
		t.Fatal(err)
	}
	if got := int(binary.BigEndian.Uint16(fragments[0][:2]) & 0x07ff); got != maxFragmentIPv6Datagram {
		t.Fatalf("datagram size=%d, want %d", got, maxFragmentIPv6Datagram)
	}

	packet = append(packet, 0)
	if _, err := Fragment6LoWPANDatagram(packet, 0x1201); err == nil || !strings.Contains(err.Error(), "1280") {
		t.Fatalf("oversized datagram error=%v, want 1280-byte limit", err)
	}
}

func TestFragmentedDownlinkDoesNotPartiallyConsumeCounter(t *testing.T) {
	e := NewEmu(EmuConfig{TxCounter: ^uint32(0) - 1}, nil)
	d := &queuedDownlink{
		fragmented: true,
		coap:       fragmentedTestPacket(t, bytes.Repeat([]byte{0xa5}, 260)),
		key:        NetworkKey,
	}
	if _, _, err := e.buildFrames(testDstEUI(t), d); !errors.Is(err, ErrTxCounterExhausted) {
		t.Fatalf("fragmented build error=%v", err)
	}
	if got := e.TxCounter(); got != ^uint32(0)-1 {
		t.Errorf("counter=%#x, want %#x", got, ^uint32(0)-1)
	}
}

func TestReassemblerDoesNotCompleteAcrossGap(t *testing.T) {
	dst := testDstEUI(t)
	payload := bytes.Repeat([]byte{0x5a}, 260)
	pkt, err := BuildCoAP(CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x7102, Token: []byte{0x72},
		Path: []string{"d", "config"}, Query: []string{"id=10"},
		Options: []CoAPOption{optUriPort, optOctets}, Payload: payload,
		SrcEUI: BridgeEUI, DstEUI: dst,
	})
	if err != nil {
		t.Fatal(err)
	}
	fragments, err := Fragment6LoWPANDatagram(pkt, 0x7789)
	if err != nil {
		t.Fatal(err)
	}
	if len(fragments) < 3 {
		t.Fatalf("fragments=%d, want at least 3", len(fragments))
	}
	ra := NewReassembler()
	for _, i := range []int{0, len(fragments) - 1} {
		f, err := BuildDataPending(NetworkKey, byte(i), dst, BridgeEUI[:5], WrapAppPayload(fragments[i], uint32(i+1), nil))
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := ParseData(NetworkKey, f.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		if _, done := ra.Add(parsed); done {
			t.Fatal("reassembler completed with a missing middle fragment")
		}
	}
}

func TestReassemblerSeparatesSameTagFlows(t *testing.T) {
	t.Run("destination", func(t *testing.T) {
		first := fragmentedFrames(t, bytes.Repeat([]byte{0x11}, 260), 0x778a)
		second := fragmentedFrames(t, bytes.Repeat([]byte{0x22}, 260), 0x778a)
		second[0].DstEUI = append([]byte{}, []byte{8, 7, 6, 5, 4, 3, 2, 1}...)
		for _, f := range second[1:] {
			f.DstEUI = append([]byte{}, second[0].DstEUI...)
		}
		ra := NewReassembler()
		var got [][]byte
		for i := range first {
			if m, done := ra.Add(first[i]); done {
				got = append(got, m.Payload)
			}
			if m, done := ra.Add(second[i]); done {
				got = append(got, m.Payload)
			}
		}
		if len(got) != 2 || !bytes.Equal(got[0], bytes.Repeat([]byte{0x11}, 260)) || !bytes.Equal(got[1], bytes.Repeat([]byte{0x22}, 260)) {
			t.Fatalf("payloads=%x", got)
		}
	})

	t.Run("size", func(t *testing.T) {
		first := fragmentedFrames(t, bytes.Repeat([]byte{0x33}, 260), 0x778b)
		second := fragmentedFrames(t, bytes.Repeat([]byte{0x44}, 340), 0x778b)
		ra := NewReassembler()
		var got [][]byte
		for i := 0; i < len(second) || i < len(first); i++ {
			if i < len(first) {
				if m, done := ra.Add(first[i]); done {
					got = append(got, m.Payload)
				}
			}
			if i < len(second) {
				if m, done := ra.Add(second[i]); done {
					got = append(got, m.Payload)
				}
			}
		}
		if len(got) != 2 || !bytes.Equal(got[0], bytes.Repeat([]byte{0x33}, 260)) || !bytes.Equal(got[1], bytes.Repeat([]byte{0x44}, 340)) {
			t.Fatalf("payloads=%x", got)
		}
	})

	t.Run("source", func(t *testing.T) {
		first := fragmentedFrames(t, bytes.Repeat([]byte{0x55}, 260), 0x778e)
		second := fragmentedFrames(t, bytes.Repeat([]byte{0x66}, 260), 0x778e)
		for _, f := range second {
			f.SrcPrefix = []byte{1, 2, 3, 4, 5}
		}
		ra := NewReassembler()
		var got [][]byte
		for i := range first {
			if m, done := ra.Add(first[i]); done {
				got = append(got, m.Payload)
			}
			if m, done := ra.Add(second[i]); done {
				got = append(got, m.Payload)
			}
		}
		if len(got) != 2 || !bytes.Equal(got[0], bytes.Repeat([]byte{0x55}, 260)) || !bytes.Equal(got[1], bytes.Repeat([]byte{0x66}, 260)) {
			t.Fatalf("payloads=%x", got)
		}
	})
}

func TestReassemblerDropsOverlappingFragment(t *testing.T) {
	frames := fragmentedFrames(t, bytes.Repeat([]byte{0x5b}, 260), 0x778c)
	ra := NewReassembler()
	if _, done := ra.Add(frames[0]); done {
		t.Fatal("first fragment completed")
	}
	if _, done := ra.Add(frames[1]); done {
		t.Fatal("second fragment completed")
	}
	duplicate := *frames[1]
	duplicate.Plaintext = append([]byte{}, frames[1].Plaintext...)
	if _, done := ra.Add(&duplicate); done {
		t.Fatal("exact duplicate completed")
	}
	overlap := *frames[1]
	overlap.Plaintext = append([]byte{}, frames[1].Plaintext...)
	overlap.Plaintext[8+lowpanFragNHeaderLen] ^= 0xff
	if _, done := ra.Add(&overlap); done {
		t.Fatal("overlapping fragment completed")
	}
	if len(ra.m) != 0 {
		t.Fatalf("buffers=%d, want reset", len(ra.m))
	}
	if got := completeFragments(t, ra, frames); got == nil || !bytes.Equal(got.Payload, bytes.Repeat([]byte{0x5b}, 260)) {
		t.Fatal("fresh transfer did not complete after overlap reset")
	}
}

func TestReassemblerExpiresIncompleteDatagram(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	ra := NewReassemblerWithClock(func() time.Time { return now }, 10*time.Second)
	frames := fragmentedFrames(t, bytes.Repeat([]byte{0x6c}, 260), 0x778d)
	if _, done := ra.Add(frames[0]); done {
		t.Fatal("first fragment completed")
	}
	now = now.Add(9 * time.Second)
	if _, done := ra.Add(frames[1]); done {
		t.Fatal("second fragment completed")
	}
	now = now.Add(time.Second)
	if got := ra.Expire(); got != 1 {
		t.Fatalf("expired=%d, want 1", got)
	}
	if got := completeFragments(t, ra, frames[1:]); got != nil {
		t.Fatal("expired transfer completed from continuation fragments")
	}
}

// fragmentRoundTrip fragments one link-local datagram carrying r and returns
// the message the reassembler rebuilds from the fragments.
func fragmentRoundTrip(t *testing.T, r CoAPReq) *CoAPMessage {
	t.Helper()
	r.SrcEUI, r.DstEUI = BridgeEUI, testDstEUI(t)
	pkt, err := BuildCoAP(r)
	if err != nil {
		t.Fatal(err)
	}
	fragments, err := Fragment6LoWPANDatagram(pkt, 0x4242)
	if err != nil {
		t.Fatal(err)
	}
	got := completeFragments(t, NewReassembler(), parsedFragments(t, fragments))
	if got == nil {
		t.Fatal("the fragments did not reassemble")
	}
	return got
}

// TestReassemblyOptionsPastFrag1Cut: an option that crosses the end of FRAG1
// cannot be parsed from FRAG1 alone. The datagram still reassembles, because
// the CoAP message is parsed once, whole.
func TestReassemblyOptionsPastFrag1Cut(t *testing.T) {
	seg := strings.Repeat("p", 100)
	payload := bytes.Repeat([]byte{0x5a}, 30)
	got := fragmentRoundTrip(t, CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x7201, Token: []byte{0x72},
		Path: []string{seg}, Options: []CoAPOption{optOctets}, Payload: payload,
	})
	if got.Path != seg || got.MID != 0x7201 || !bytes.Equal(got.Payload, payload) {
		t.Errorf("path=%d bytes mid=%04x payload=%x, want %d bytes, 7201, %x", len(got.Path), got.MID, got.Payload, len(seg), payload)
	}
	if ck, ok := got.RecomputeChecksum(BridgeEUI, testDstEUI(t)); !ok || ck != got.Checksum {
		t.Errorf("checksum %04x does not cover the reassembled message (recomputed %04x)", got.Checksum, ck)
	}
}

// TestReassemblyOptionsEndAtFrag1Cut: header and options that end exactly at
// the FRAG1 cut leave the payload marker as the first FRAGN byte. The payload
// must not start with that marker.
func TestReassemblyOptionsEndAtFrag1Cut(t *testing.T) {
	// 4-byte header, 1-byte token, 2-byte option header and 73 bytes of value:
	// the 80 CoAP bytes a link-local FRAG1 carries.
	seg := strings.Repeat("q", 73)
	payload := bytes.Repeat([]byte{0xa5}, 40)
	got := fragmentRoundTrip(t, CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x7202, Token: []byte{0x73},
		Path: []string{seg}, Payload: payload,
	})
	if got.Path != seg || !bytes.Equal(got.Payload, payload) {
		t.Errorf("path=%d bytes payload=%x, want %d bytes, %x", len(got.Path), got.Payload, len(seg), payload)
	}
}
