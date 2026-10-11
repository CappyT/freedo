package tado

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"slices"
	"strings"
	"testing"
	"time"
)

// Regression tests for CoAP and MAC edge cases. Every frame
// is synthesized with the package builders.

// midZeroUplink is deviceUplink with the Message ID forced to 0 and the UDP
// checksum recomputed over the patched message.
func midZeroUplink(t *testing.T, dev []byte, r CoAPReq) []byte {
	t.Helper()
	r.MID = 1
	r.SrcEUI, r.DstEUI = dev, BridgeEUI
	r.SrcPort, r.DstPort = coapPort, bridgeCoAPPort
	pkt, err := BuildCoAP(r)
	if err != nil {
		t.Fatal(err)
	}
	const coapAt = 9 // IPHC 7e33, UDP-NHC f0, both ports and the checksum
	pkt[coapAt+2], pkt[coapAt+3] = 0, 0
	ck := udpChecksumV6(linkLocalFromWire(dev), linkLocalFromWire(BridgeEUI), coapPort, bridgeCoAPPort, pkt[coapAt:])
	binary.BigEndian.PutUint16(pkt[7:coapAt], ck)
	f, err := BuildData(NetworkKey, 0x23, BridgeEUI, dev[:5], WrapAppPayload(pkt, 701, nil))
	if err != nil {
		t.Fatal(err)
	}
	return f.Bytes()
}

// TestAnswersConWithMIDZero: MID 0 is a valid Message ID, and every device
// wraps through it once per 65536 CONs.
func TestAnswersConWithMIDZero(t *testing.T) {
	dev := mustHex("02a6cefa07c51b00")
	e, _, _ := newTestEmu(t)
	raw := midZeroUplink(t, dev, CoAPReq{Type: "CON", Code: "0.01", Token: []byte{0x31}, Path: []string{"d", "lock"}})
	m := servedAnswer(t, e.OnFrame(raw), 0)
	if m.Type != "ACK" || m.Code != "2.05" || !bytes.Equal(m.Token, []byte{0x31}) {
		t.Errorf("answer=%s %s tok=%x, want ACK 2.05 tok=31", m.Type, m.Code, m.Token)
	}
}

// TestAnswerBuildersAcceptMIDZero: the builders that echo a request's MID take
// 0 like any other value.
func TestAnswerBuildersAcceptMIDZero(t *testing.T) {
	dev := mustHex("02a6cefa07c51b00")
	builds := map[string]func() ([]byte, error){
		"BuildCoAPAck": func() ([]byte, error) {
			return BuildCoAPAck("2.04", 0, nil, nil, BridgeEUI, dev, bridgeCoAPPort, coapPort)
		},
		"BuildTimeCoAP": func() ([]byte, error) {
			return BuildTimeCoAP(time.Unix(1_700_000_000, 0), 0, nil, BridgeEUI, dev, bridgeCoAPPort, coapPort)
		},
		"BuildFlowSetpointCoAP": func() ([]byte, error) {
			return BuildFlowSetpointCoAP(1, 45, 0, nil, BridgeEUI, dev)
		},
		"BuildContextAck": func() ([]byte, error) {
			return BuildContextAck("2.04", 0, nil, BridgeEUI, dev, coapPort, coapPort)
		},
		"BuildContextAckBody": func() ([]byte, error) {
			return BuildContextAckBody("2.05", 0, nil, []byte{0x01}, BridgeEUI, dev, coapPort, coapPort)
		},
	}
	for name, build := range builds {
		pkt, err := build()
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		m, err := parseCoAPCore(pkt)
		if err != nil || m.MID != 0 {
			t.Errorf("%s: mid=%v err=%v, want 0", name, m, err)
		}
	}
}

// TestRequestBuildersKeepMIDZero: a caller that passes MID 0 gets MID 0 on air,
// so the queue that tracks the MID matches the device's answer.
func TestRequestBuildersKeepMIDZero(t *testing.T) {
	pkt, err := BuildSetpointCoAP(SetpointCoAP{ID: 3, Setpoint: 21, Overlay: 2, MID: 0, Token: []byte{1}, DstEUI: mustHex("02a6cefa07c51b00")})
	if err != nil {
		t.Fatal(err)
	}
	m, err := parseCoAPCore(pkt)
	if err != nil || m.MID != 0 {
		t.Errorf("mid=%04x err=%v, want 0000", m.MID, err)
	}
}

// TestGenerateMIDIsSequential: a generated MID comes from one counter that
// wraps through 0, and GenerateMID outranks the MID field.
func TestGenerateMIDIsSequential(t *testing.T) {
	saved := midCounter.Load()
	defer midCounter.Store(saved)
	midCounter.Store(0xfffe)
	var got []uint16
	for i := 0; i < 3; i++ {
		msg, err := buildCoAPMessage(CoAPReq{Type: "CON", Code: "0.01", MID: 0x1234, GenerateMID: true, Token: []byte{1}})
		if err != nil {
			t.Fatal(err)
		}
		m, err := parseCoAPMessage(msg)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, m.MID)
	}
	if want := []uint16{0xffff, 0x0000, 0x0001}; !slices.Equal(got, want) {
		t.Errorf("MIDs=%04x, want %04x", got, want)
	}
}

// TestActiveGetKeyedByDevice: two devices whose active GETs share a MID keep
// their own exchanges.
func TestActiveGetKeyedByDevice(t *testing.T) {
	e, _, _ := newTestEmu(t)
	saved := midCounter.Load()
	defer midCounter.Store(saved)
	for _, target := range []string{"camera", "bagno"} {
		midCounter.Store(0x7000)
		if err := e.EnqueueGet(target, "d/fw/state", []string{"d", "fw", "state"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(e.pendGet) != 2 {
		t.Errorf("active GETs=%d, want 2", len(e.pendGet))
	}
}

// illegalOptions are options go-coap's decoder skips for their length: a 0 and
// a 9-byte ETag, a 3-byte Content-Format and a 4-byte Block2.
var illegalOptions = []CoAPOption{
	{Num: 4, Val: []byte{}},
	{Num: 4, Val: bytes.Repeat([]byte{0xe7}, 9)},
	{Num: 12, Val: []byte{0x00, 0x00, 0x2a}},
	{Num: 23, Val: []byte{0x00, 0x00, 0x00, 0x13}},
}

func illegalOptionsMessage(t *testing.T) []byte {
	t.Helper()
	msg, err := buildCoAPMessage(CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x7401, Token: []byte{0x74},
		Path: []string{"z", "s"}, Query: []string{"lid=2"},
		Options: append([]CoAPOption{}, illegalOptions...),
		Payload: []byte{0x62, 0x40, 0x01, 0x00},
	})
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

// TestParseCoAPMessageKeepsIllegalOptionLengths: the parser keeps every option
// value opaque, whatever its length.
func TestParseCoAPMessageKeepsIllegalOptionLengths(t *testing.T) {
	m, err := parseCoAPMessage(illegalOptionsMessage(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Options) != len(illegalOptions) {
		t.Fatalf("options=%v, want %v", m.Options, illegalOptions)
	}
	for i, o := range illegalOptions {
		if m.Options[i].Num != o.Num || !bytes.Equal(m.Options[i].Val, o.Val) {
			t.Errorf("option %d = %d:%x, want %d:%x", i, m.Options[i].Num, m.Options[i].Val, o.Num, o.Val)
		}
	}
}

// TestServedBlock2RefusesMalformedOption: a Block2 longer than 3 bytes is an
// error, not a request for block 0.
func TestServedBlock2RefusesMalformedOption(t *testing.T) {
	in := fixtureInstall(t)
	buf := &bytes.Buffer{}
	e := NewEmu(in.EmuConfig(), NewUnhandledLogger(buf))
	raw := deviceUplink(t, mustHex("08a4defa07c51b00"), CoAPReq{
		Type: "CON", Code: "0.01", MID: 0x7402, Token: []byte{0x75},
		Path:    []string{"h", "7654321", "z", "3", "config"},
		Options: []CoAPOption{{Num: 23, Val: []byte{0x00, 0x00, 0x00, 0x03}}},
	})
	for _, a := range e.OnFrame(raw) {
		if a.Kind == ActData {
			t.Errorf("a malformed Block2 was answered: %s", a.Reason)
		}
	}
	if !strings.Contains(buf.String(), "Block2 option is longer than 3 bytes") {
		t.Errorf("log does not name the malformed Block2:\n%s", buf.String())
	}
}

// TestParseEnhAckChecksFCS: every captured Enhanced ACK passes the MAC FCS
// check, and one corrupted byte fails it.
func TestParseEnhAckChecksFCS(t *testing.T) {
	for _, c := range realEnhAcks {
		frame := mustB(c.want)
		if _, err := ParseEnhAck(frame); err != nil {
			t.Errorf("captured %s: %v", c.want[:8], err)
		}
		frame[22] ^= 0x01
		if _, err := ParseEnhAck(frame); err == nil {
			t.Errorf("corrupted %s parsed", c.want[:8])
		}
	}
}

// TestEnhAckForAnotherNodeConfirmsNothing: an Enhanced ACK addressed to
// another node never closes our in-flight downlink, even when the sequence
// number matches.
func TestEnhAckForAnotherNodeConfirmsNothing(t *testing.T) {
	camera := mustHex(NodeByAny("camera").WireLE)
	other := []byte{0x67, 0x45, 0x23, 0x31, 0x07, 0xc5, 0x1b, 0x00}
	e, _, _ := newTestEmu(t)
	if err := e.EnqueueChildLock("camera", 10, true); err != nil {
		t.Fatal(err)
	}
	wire, label, err := e.resolve("camera")
	if err != nil {
		t.Fatal(err)
	}
	out, ok := e.nextDownlink(wire, label)
	if !ok || len(out) != 1 {
		t.Fatalf("downlink=%v ok=%t", out, ok)
	}
	df, err := ParseData(NetworkKey, out[0].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	e.OnFrame(BuildEnhAck(df.Seq, other, camera, 0x0101))
	if n := e.Pending("camera"); n != 1 {
		t.Fatalf("an Enhanced ACK to another node closed the downlink: pending=%d", n)
	}
	e.OnFrame(BuildEnhAck(df.Seq, BridgeEUI, camera, 0x0101))
	if n := e.Pending("camera"); n != 0 {
		t.Errorf("our own Enhanced ACK left pending=%d, want 0", n)
	}
}

// TestZoneRelayRefusedBySize: a zone hop that cannot fit one Data frame is
// refused when it is relayed, with its own reason, instead of being queued and
// failing at send time. The hop carries the source IID inline and the hop
// limit where the uplink carries the destination IID, so a 127-byte uplink
// relays as 128 bytes.
func TestZoneRelayRefusedBySize(t *testing.T) {
	dev := mustHex(NodeByAny("a304").WireLE)
	peer := mustHex(NodeByAny("a70a").WireLE)
	buf := &bytes.Buffer{}
	e := NewEmu(EmuConfig{TxCounter: 100000}, NewUnhandledLogger(buf))
	var raw []byte
	for n := 1; ; n++ {
		raw = zoneUplink(t, dev, peer, CoAPReq{
			Type: "NON", Code: "0.03", MID: 0x7403, Token: []byte{0x76},
			Path: []string{"z", "s"}, Query: []string{"lid=2"}, Options: []CoAPOption{optOctets},
			Payload: bytes.Repeat([]byte{0x11}, n),
		})
		if len(raw) >= 127 {
			break
		}
	}
	if len(raw) > 127 {
		t.Fatalf("the uplink itself is %d bytes", len(raw))
	}
	e.OnFrame(raw)
	if n := e.Pending(hex.EncodeToString(peer)); n != 0 {
		t.Errorf("an oversized hop was queued: pending=%d", n)
	}
	if !strings.Contains(buf.String(), "zone-relay-too-large") {
		t.Errorf("log does not name the oversized hop:\n%s", buf.String())
	}
}

// TestDuplicateConAnsweredFromCache: a retransmitted CON gets the answer it got
// the first time and is not processed again.
func TestDuplicateConAnsweredFromCache(t *testing.T) {
	in := synthInstall(t)
	e := NewEmu(in.EmuConfig(), nil)
	va, err := in.Resolve("VA0000000001")
	if err != nil {
		t.Fatal(err)
	}
	ow := synthDeviceUplink(t, in, va.WireEUI(), CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x7404, Token: []byte{0x77}, Path: homePath(in, "z", "4", "ow"),
		Options: []CoAPOption{optOctets},
	})
	first := synthAnswer(t, in, e.OnFrame(ow), 0x7404)
	again := synthAnswer(t, in, e.OnFrame(ow), 0x7404)
	if !bytes.Equal(first.Raw, again.Raw) {
		t.Errorf("the duplicate got %x, want the first answer %x", again.Raw, first.Raw)
	}
	if got := e.TakeOpenWindowReports(); len(got) != 1 {
		t.Errorf("open window reports=%d, want 1", len(got))
	}

	zs := synthZoneUplink(t, in, va.WireEUI(), CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x7405, Token: []byte{0x78},
		Path: []string{"z", "s"}, Query: []string{"lid=4"}, Options: []CoAPOption{optOctets},
		Payload: TLV{Tag: "6240", Val: []byte{0x00}}.Encode(),
	})
	synthAnswer(t, in, e.OnFrame(zs), 0x7405)
	synthAnswer(t, in, e.OnFrame(zs), 0x7405)
	if got := e.TakeZoneStateReports(); len(got) != 1 {
		t.Errorf("zone state reports=%d, want 1", len(got))
	}

	// The same MID with another token is another request.
	ow2 := synthDeviceUplink(t, in, va.WireEUI(), CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x7404, Token: []byte{0x79}, Path: homePath(in, "z", "4", "ow"),
		Options: []CoAPOption{optOctets},
	})
	synthAnswer(t, in, e.OnFrame(ow2), 0x7404)
	if got := e.TakeOpenWindowReports(); len(got) != 1 {
		t.Errorf("open window reports after a new token=%d, want 1", len(got))
	}
}

// TestZoneProxyKeyedByRequester: two requesters that pick the same MID wait
// for their own answers.
func TestZoneProxyKeyedByRequester(t *testing.T) {
	ru := mustHex(NodeByAny("RU1000000001").WireLE)
	a803 := mustHex(NodeByAny("a803").WireLE)
	member := mustHex(NodeByAny("a70a").WireLE)
	e := NewEmu(EmuConfig{
		TxCounter: 100000, ZoneGetProxy: true, NoZoneFanOut: true,
		Zones: map[int][]string{8: {"RU1000000001", "a70a", "a803"}},
	}, nil)
	e.OnFrame(zoneUplink(t, member, BridgeEUI, CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x7406, Token: []byte{1},
		Path: []string{"z", "p"}, Query: []string{"lid=8"},
		Options: []CoAPOption{optOctets}, Payload: TLV{Tag: "40a0", Val: []byte{10}}.Encode(),
	}))
	for _, dev := range [][]byte{ru, a803} {
		e.OnFrame(zoneUplink(t, dev, BridgeEUI, CoAPReq{
			Type: "CON", Code: "0.01", MID: 0x7407, Token: []byte{2},
			Path: []string{"z", "s"}, Query: []string{"lid=8"},
		}))
	}
	if n := e.PendingZoneProxies(); n != 2 {
		t.Errorf("pending proxies=%d, want 2", n)
	}
}

// scheduleEntries is a day program long enough for a multi-block transfer.
func scheduleEntries(base int) []ScheduleEntry {
	entries := make([]ScheduleEntry, 0, 24)
	for minute := 0; minute < 24*60; minute += 60 {
		entries = append(entries, ScheduleEntry{Minute: minute, SetpointCenti: base + minute, Enabled: true})
	}
	return entries
}

func scheduleBlockGET(t *testing.T, dev []byte, block int) []byte {
	t.Helper()
	return deviceUplink(t, dev, CoAPReq{
		Type: "CON", Code: "0.01", MID: uint16(0x7500 + block), Token: []byte{0x7a},
		Path:    []string{"h", "7654321", "z", "3", "sched"},
		Options: []CoAPOption{{Num: 23, Val: []byte{byte(block<<4 | 3)}}},
	})
}

// TestBlock2TransferServesOneDocument: a document that changes during a
// block-wise transfer does not leak into it. The blocks after block 0 come
// from the document block 0 came from.
func TestBlock2TransferServesOneDocument(t *testing.T) {
	dev := mustHex("02a6cefa07c51b00")
	e, _, _ := newTestEmu(t)
	e.inst = &Install{HomeID: 7654321}
	if err := e.SetSchedule("camera", []ScheduleProgram{{DayMask: 0x7f, Entries: scheduleEntries(500)}}); err != nil {
		t.Fatal(err)
	}
	old := EncodeSchedule(EncodeScheduleDayProgram(0x7f, scheduleEntries(500)...))

	var body, etag []byte
	for block := 0; ; block++ {
		m := servedBlockAnswer(t, e.OnFrame(scheduleBlockGET(t, dev, block)), uint16(0x7500+block))
		if m.Code != "2.05" {
			t.Fatalf("block %d code=%s, want 2.05", block, m.Code)
		}
		got, _ := m.Option(4)
		if etag == nil {
			etag = append([]byte(nil), got.Val...)
			if err := e.SetSchedule("camera", []ScheduleProgram{{DayMask: 0x7f, Entries: scheduleEntries(600)}}); err != nil {
				t.Fatal(err)
			}
		} else if !bytes.Equal(got.Val, etag) {
			t.Fatalf("block %d ETag=%x, want the block 0 ETag %x", block, got.Val, etag)
		}
		body = append(body, m.Payload...)
		bo, _ := m.Option(23)
		if decodeCoAPUint(bo.Val)&0x08 == 0 {
			break
		}
	}
	if !bytes.Equal(body, old) {
		t.Errorf("body mixes documents:\n got %x\nwant %x", body, old)
	}
}

// TestBlock2WithoutTransferRefused: a block past 0 with no transfer behind it
// is refused, so the device restarts at block 0.
func TestBlock2WithoutTransferRefused(t *testing.T) {
	dev := mustHex("02a6cefa07c51b00")
	e, _, _ := newTestEmu(t)
	e.inst = &Install{HomeID: 7654321}
	if err := e.SetSchedule("camera", []ScheduleProgram{{DayMask: 0x7f, Entries: scheduleEntries(500)}}); err != nil {
		t.Fatal(err)
	}
	m := servedAnswer(t, e.OnFrame(scheduleBlockGET(t, dev, 1)), 0x7501)
	if m.Code != "5.03" || len(m.Payload) != 0 {
		t.Errorf("answer=%s payload=%x, want an empty 5.03", m.Code, m.Payload)
	}
}
