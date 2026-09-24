package tado

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The emulator is exercised entirely on recorded traffic: the 370-frame golden
// capture, the two-fragment 6LoWPAN fixture, and frames synthesized with the
// package builders. No hardware is involved.

func newTestEmu(t *testing.T) (*EmuState, *UnhandledLogger, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	log := NewUnhandledLogger(buf)
	return NewEmu(EmuConfig{TxCounter: 100000, CSLPhase: 0x1234}, log), log, buf
}

// countPrefix sums the logger counts whose reason starts with kind (reasons
// carry a detail suffix, e.g. "unknown-tlv-tag=0452").
func countPrefix(counts map[string]int, kind string) int {
	n := 0
	for k, v := range counts {
		if strings.HasPrefix(k, kind) {
			n += v
		}
	}
	return n
}

func loadFrames(t *testing.T, path string) [][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	var out [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), 1<<16)
	for sc.Scan() {
		if sc.Text() == "" {
			continue
		}
		b, err := hex.DecodeString(sc.Text())
		if err != nil {
			t.Fatalf("bad hex in %s: %v", path, err)
		}
		out = append(out, b)
	}
	return out
}

// firstUplink returns the first golden frame from a device to the bridge that
// satisfies want, plus its parsed form.
func firstUplink(t *testing.T, want func(*Frame, *AppPayload) bool) ([]byte, *Frame, *AppPayload) {
	t.Helper()
	for _, raw := range loadGolden(t) {
		f, err := ParseData(NetworkKey, raw)
		if err != nil {
			continue
		}
		if bytes.Equal(f.SrcPrefix, BridgeEUI[:5]) || !bytes.Equal(f.DstEUI, BridgeEUI) {
			continue
		}
		app := ParseApp(f.Plaintext)
		if app == nil || IsFragment(f.Plaintext) {
			continue
		}
		if want(f, app) {
			return raw, f, app
		}
	}
	t.Fatal("no golden uplink matches the wanted shape")
	return nil, nil, nil
}

func findAction(actions []Action, kind ActionKind, reasonPrefix string) (Action, bool) {
	for _, a := range actions {
		if a.Kind == kind && strings.HasPrefix(a.Reason, reasonPrefix) {
			return a, true
		}
	}
	return Action{}, false
}

// TestOnFrameAcksUplink: a real device uplink to the bridge gets a MAC Enhanced
// ACK with the same seq, its telemetry is cached, and a CON on the link-local
// path also gets a CoAP ACK addressed back to the device with swapped ports.
func TestOnFrameAcksUplink(t *testing.T) {
	raw, f, app := firstUplink(t, func(_ *Frame, a *AppPayload) bool {
		if a.CoAP == nil || a.CoAP.Type != "CON" || !isDirectPath(a.CoAP) {
			return false
		}
		_, ok := a.TLV("012d") // a telemetry publish, so the cache has something to hold
		return ok
	})

	e, _, _ := newTestEmu(t)
	actions := e.OnFrame(raw)
	if len(actions) < 2 {
		t.Fatalf("got %d actions, want at least the EnhACK and the CoAP ACK", len(actions))
	}

	if actions[0].Kind != ActEnhAck {
		t.Fatalf("first action is %v, want ActEnhAck", actions[0].Kind)
	}
	ack, err := ParseEnhAck(actions[0].Bytes)
	if err != nil {
		t.Fatalf("emitted EnhACK does not parse: %v", err)
	}
	if ack.Seq != f.Seq {
		t.Errorf("EnhACK seq=%02x, want %02x (the Data frame's)", ack.Seq, f.Seq)
	}
	if !bytes.Equal(ack.DstEUI, f.SrcEUI()) {
		t.Errorf("EnhACK dst=%x, want the sender %x", ack.DstEUI, f.SrcEUI())
	}
	if !bytes.Equal(ack.SrcEUI, BridgeEUI) {
		t.Errorf("EnhACK src=%x, want the bridge %x", ack.SrcEUI, BridgeEUI)
	}
	if ack.CSLPhase != 0x1234 {
		t.Errorf("EnhACK cslPhase=%04x, want the configured 1234", ack.CSLPhase)
	}

	// Node state learned from the uplink.
	nodes := e.Nodes()
	if len(nodes) != 1 {
		t.Fatalf("got %d nodes, want 1", len(nodes))
	}
	ns := nodes[0]
	if ns.WireLE != hex.EncodeToString(f.SrcEUI()) {
		t.Errorf("node wire=%s, want %x", ns.WireLE, f.SrcEUI())
	}
	if ns.RxCounter != app.Counter {
		t.Errorf("RxCounter=%d, want %d", ns.RxCounter, app.Counter)
	}
	if ns.LastSeq != f.Seq {
		t.Errorf("LastSeq=%02x, want %02x", ns.LastSeq, f.Seq)
	}
	if ns.LastSeen.IsZero() {
		t.Error("LastSeen not set")
	}
	if ns.Telemetry["temp_C"] == "" {
		t.Errorf("temperature not cached, telemetry=%v", ns.Telemetry)
	}

	// CoAP ACK, decrypted back out of the action bytes.
	a, ok := findAction(actions, ActData, "coap-ack")
	if !ok {
		t.Fatalf("no CoAP ACK action, got %v", reasons(actions))
	}
	df, err := ParseData(NetworkKey, a.Bytes)
	if err != nil {
		t.Fatalf("CoAP ACK frame does not decrypt: %v", err)
	}
	if !bytes.Equal(df.DstEUI, f.SrcEUI()) {
		t.Errorf("ACK dst=%x, want %x", df.DstEUI, f.SrcEUI())
	}
	if !bytes.Equal(df.SrcPrefix, BridgeEUI[:5]) {
		t.Errorf("ACK src prefix=%x, want the bridge's %x", df.SrcPrefix, BridgeEUI[:5])
	}
	back := ParseApp(df.Plaintext)
	if back == nil || back.CoAP == nil {
		t.Fatal("CoAP ACK plaintext does not parse")
	}
	if back.CoAP.Type != "ACK" || back.CoAP.Code != "2.04" {
		t.Errorf("response = %s %s, want ACK 2.04", back.CoAP.Type, back.CoAP.Code)
	}
	if back.CoAP.MID != app.CoAP.MID {
		t.Errorf("ACK mid=%04x, want the request's %04x", back.CoAP.MID, app.CoAP.MID)
	}
	if !bytes.Equal(back.CoAP.Token, app.CoAP.Token) {
		t.Errorf("ACK token=%x, want %x", back.CoAP.Token, app.CoAP.Token)
	}
	if back.CoAP.SrcPort != app.CoAP.DstPort || back.CoAP.DstPort != app.CoAP.SrcPort {
		t.Errorf("ACK ports %d->%d, want the uplink's swapped %d->%d",
			back.CoAP.SrcPort, back.CoAP.DstPort, app.CoAP.DstPort, app.CoAP.SrcPort)
	}
	if back.Counter <= 100000 {
		t.Errorf("downlink counter=%d, want above the seeded 100000", back.Counter)
	}
}

// TestOnFrameLearnsCSLFromEnhAck: an Enhanced ACK from a device teaches its CSL
// phase and period.
func TestOnFrameLearnsCSLFromEnhAck(t *testing.T) {
	dev := NodeByAny("camera")
	if dev == nil {
		t.Fatal("camera not in inventory")
	}
	wire := mustB(dev.WireLE)

	e, _, _ := newTestEmu(t)
	if acts := e.OnFrame(BuildEnhAck(0x33, BridgeEUI, wire, 0x9edc)); acts != nil {
		t.Errorf("EnhACK should emit no action, got %v", reasons(acts))
	}
	nodes := e.Nodes()
	if len(nodes) != 1 {
		t.Fatalf("got %d nodes, want 1", len(nodes))
	}
	if nodes[0].CSLPhase != 0x9edc {
		t.Errorf("CSLPhase=%04x, want 9edc", nodes[0].CSLPhase)
	}
	if nodes[0].CSLPeriod != CSLPeriodUnits {
		t.Errorf("CSLPeriod=%04x, want %04x", nodes[0].CSLPeriod, CSLPeriodUnits)
	}
	if nodes[0].Short != dev.Short {
		t.Errorf("Short=%s, want %s", nodes[0].Short, dev.Short)
	}
}

// TestOnFrameReassemblesFragments: the captured 6LoWPAN fragment pair, re-framed
// as a device->bridge uplink, ACKs at the MAC layer per fragment and reassembles
// into one CoAP datagram. That datagram is block 0 of a Block2 transfer with more
// blocks to come, so its TLVs wait for the rest of the body.
func TestOnFrameReassemblesFragments(t *testing.T) {
	e, log, _ := newTestEmu(t)
	var devEUI []byte
	frames := loadFrames(t, "testdata/frames_frag.txt")
	if len(frames) != 2 {
		t.Fatalf("fixture has %d frames, want 2", len(frames))
	}
	for i, raw := range frames {
		// The fixture is a bridge->device downlink; flip the addressing so it is
		// an uplink to us, which is the direction the emulator handles.
		p, err := ParseData(NetworkKey, raw)
		if err != nil {
			t.Fatalf("fragment %d: %v", i, err)
		}
		if devEUI == nil {
			devEUI = p.DstEUI
		}
		up, err := BuildData(NetworkKey, p.Seq, BridgeEUI, devEUI[:5], p.Plaintext)
		if err != nil {
			t.Fatal(err)
		}
		actions := e.OnFrame(up.Bytes())
		if len(actions) == 0 || actions[0].Kind != ActEnhAck {
			t.Fatalf("fragment %d got %v, want a MAC EnhACK", i, reasons(actions))
		}
		a, err := ParseEnhAck(actions[0].Bytes)
		if err != nil || a.Seq != p.Seq {
			t.Errorf("fragment %d EnhACK seq mismatch: %v %v", i, a, err)
		}
	}

	st := e.Stats()
	if st.Fragments != 2 || st.Reassembled != 1 {
		t.Errorf("stats fragments=%d reassembled=%d, want 2 and 1", st.Fragments, st.Reassembled)
	}
	// The datagram is a CoAP ACK, so no CoAP response is due. It carries Block2
	// 0b, i.e. block 0 with the M bit set, and a block boundary falls anywhere:
	// the block buffers whole instead of reaching the TLV parser.
	nodes := e.Nodes()
	if len(nodes) != 1 || len(nodes[0].Telemetry) != 0 {
		t.Fatalf("a partial Block2 body reached the cache: %v", nodes)
	}
	if e.block2.Open() != 1 {
		t.Errorf("open Block2 transfers=%d, want 1", e.block2.Open())
	}
	if n := countPrefix(log.Counts(), rsnUnknownTag); n != 0 {
		t.Errorf("%d TLVs parsed out of a partial block: %s", n, log.Summary())
	}
}

// TestUnhandledLogging: frames the emulator cannot act on are logged with a
// reason, and Counts/Summary report them.
func TestUnhandledLogging(t *testing.T) {
	e, log, buf := newTestEmu(t)

	if acts := e.OnFrame(mustB("1122334455")); acts != nil {
		t.Error("an unclassified frame must produce no action")
	}
	// Foreign PAN: a real wake-up with its PAN id overwritten.
	foreign := mustB("258ccdaba205820ef800803f")
	foreign[2], foreign[3] = 0x34, 0x12
	e.OnFrame(foreign)
	// Wake-up aimed at a short address that is not in the inventory.
	e.OnFrame(BuildWakeUp(0x12, []byte{0xde, 0xad}, 500))
	// A broadcast from anyone means a second coordinator is on air.
	bc, err := BuildBroadcast(NetworkKey, 0x2d, mustB("05a2cafa07c51b00"), 7, 0x0d, mustB("c51b0000112233"))
	if err != nil {
		t.Fatal(err)
	}
	e.OnFrame(bc)

	counts := log.Counts()
	for _, kind := range []string{rsnUnclassified, rsnForeignPAN, rsnUnknownWakeTarget, rsnForeignBroadcast} {
		if countPrefix(counts, kind) != 1 {
			t.Errorf("reason %q count=%d, want 1; summary:\n%s", kind, countPrefix(counts, kind), log.Summary())
		}
	}
	if log.Total() != 4 {
		t.Errorf("logger total=%d, want 4:\n%s", log.Total(), log.Summary())
	}
	if lines := strings.Count(strings.TrimRight(buf.String(), "\n"), "\n") + 1; lines != 4 {
		t.Errorf("wrote %d lines, want 4:\n%s", lines, buf.String())
	}
	if !strings.Contains(buf.String(), "\t"+rsnUnclassified+"\t") {
		t.Errorf("log line is not tab-structured:\n%s", buf.String())
	}

	// An uplink carrying a TLV tag outside the catalog is logged too. Every tag
	// the golden capture carries is catalogued, so the frame is synthesized.
	const wantTag = "9999"
	raw := uplinkFrame(t, mustHex(NodeByAny("camera").WireLE), "0.03",
		[]string{"d", "VA1000000005", "config"}, []string{"id=10"},
		TLV{Tag: wantTag, Val: []byte{0x01}}.Encode())
	e2, log2, _ := newTestEmu(t)
	e2.OnFrame(raw)
	if n := log2.Counts()[rsnUnknownTag+"="+wantTag]; n != 1 {
		t.Errorf("unknown tag %s not logged (count=%d):\n%s", wantTag, n, log2.Summary())
	}
	if ns := e2.Nodes(); len(ns) != 1 || ns[0].Telemetry[wantTag] == "" {
		t.Errorf("unknown tag %s should still be cached raw: %v", wantTag, ns)
	}
}

// TestEnqueueDHWSetpoint: the hot-water command queues against the wall
// thermostat and rebuilds into the z/s?id=0 write the boiler path expects.
func TestEnqueueDHWSetpoint(t *testing.T) {
	e, _, _ := newTestEmu(t)
	if err := e.EnqueueDHWSetpoint("RU1000000001", 0, 53.0); err != nil {
		t.Fatal(err)
	}
	if e.Pending("RU1000000001") != 1 {
		t.Fatalf("pending=%d, want 1", e.Pending("RU1000000001"))
	}
	if err := e.EnqueueDHWSetpoint("RU1000000001", 0, 80.0); err == nil {
		t.Error("80 C is above the thermostat range and should be refused")
	}
	if err := e.EnqueueDHWSetpoint("nosuchnode", 0, 53.0); err == nil {
		t.Error("an unknown target should be refused")
	}

	actions, err := e.ScheduleDownlink("RU1000000001")
	if err != nil {
		t.Fatal(err)
	}
	last := actions[len(actions)-1]
	df, err := ParseData(NetworkKey, last.Bytes)
	if err != nil {
		t.Fatalf("DHW Data does not decrypt: %v", err)
	}
	app := ParseApp(df.Plaintext)
	if app == nil || app.CoAP == nil {
		t.Fatal("DHW downlink does not parse as CoAP")
	}
	if app.CoAP.URI() != "z/s?id=0" {
		t.Errorf("uri = %q, want z/s?id=0", app.CoAP.URI())
	}
	got := map[string]string{}
	for _, tlv := range app.TLVs {
		got[tlv.Tag] = hex.EncodeToString(tlv.Val)
	}
	if got["6280"] != "14b4" { // 53.00 * 100 = 5300
		t.Errorf("setpoint 6280=%s, want 14b4", got["6280"])
	}
	if got["6020"] != "02" {
		t.Errorf("zone kind 6020=%s, want 02", got["6020"])
	}
	if got["6260"] != "01" {
		t.Errorf("enabled 6260=%s, want 01", got["6260"])
	}
}

func TestEnqueueDHWControlDisabled(t *testing.T) {
	e, _, _ := newTestEmu(t)
	if err := e.EnqueueDHWControl("RU1000000001", 0, 53.0, false); err != nil {
		t.Fatal(err)
	}
	actions, err := e.ScheduleDownlink("RU1000000001")
	if err != nil {
		t.Fatal(err)
	}
	df, err := ParseData(NetworkKey, actions[len(actions)-1].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	app := ParseApp(df.Plaintext)
	if app == nil || app.CoAP == nil {
		t.Fatal("DHW downlink does not parse as CoAP")
	}
	if got, want := hex.EncodeToString(app.CoAP.Payload), "61600102618001006020010261e00100624001026260010062e001006440020001"; got != want {
		t.Errorf("disabled DHW payload=%s, want %s", got, want)
	}
	for _, tlv := range app.TLVs {
		if tlv.Tag == "6280" {
			t.Error("disabled DHW control must omit 6280")
		}
	}
	wire, _, err := e.resolve("RU1000000001")
	if err != nil {
		t.Fatal(err)
	}
	e.confirmByMID(wire, &CoAPMessage{Type: "ACK", Code: "2.04", MID: app.CoAP.MID, Token: app.CoAP.Token})
	if e.Pending("RU1000000001") != 0 {
		t.Error("CoAP ACK did not close disabled DHW control")
	}
	history := e.QueueHistory("RU1000000001")
	if len(history) != 1 || history[0].Reason != "coap-ack" {
		t.Errorf("history=%+v, want delivered/coap-ack", history)
	}
}

func TestEnqueueDHWControlRequiresCoAP204(t *testing.T) {
	e, _, _ := newTestEmu(t)
	if err := e.EnqueueDHWControl("RU1000000001", 0, 53.0, false); err != nil {
		t.Fatal(err)
	}
	wire, label, err := e.resolve("RU1000000001")
	if err != nil {
		t.Fatal(err)
	}
	first, ok := e.nextDownlink(wire, label)
	if !ok || len(first) != 1 || !first[0].WaitResponse || !first[0].CoAPTransaction {
		t.Fatalf("first=%+v ok=%t, want CoAP-confirmed DHW write", first, ok)
	}
	firstData, err := ParseData(NetworkKey, first[0].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	firstReq := ParseApp(firstData.Plaintext).CoAP
	e.OnFrame(BuildEnhAck(firstData.Seq, BridgeEUI, wire, 0))
	if e.Pending("RU1000000001") != 1 {
		t.Fatal("Enhanced ACK completed DHW write")
	}

	second, ok := e.nextDownlink(wire, label)
	if !ok || len(second) != 1 || !second[0].WaitResponse || !second[0].CoAPTransaction {
		t.Fatalf("second=%+v ok=%t, want DHW CoAP retry", second, ok)
	}
	secondData, err := ParseData(NetworkKey, second[0].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	secondReq := ParseApp(secondData.Plaintext).CoAP
	if secondReq.MID != firstReq.MID || !bytes.Equal(secondReq.Token, firstReq.Token) {
		t.Fatalf("retry MID/token=%04x/%x, want %04x/%x", secondReq.MID, secondReq.Token, firstReq.MID, firstReq.Token)
	}
	if secondData.Seq == firstData.Seq || bytes.Equal(secondData.Plaintext[4:8], firstData.Plaintext[4:8]) {
		t.Fatal("DHW retry reused MAC sequence number or nonce counter")
	}

	feed := func(src []byte, code string, mid uint16, token []byte, seq byte) {
		t.Helper()
		ack, err := BuildCoAPAck(code, mid, token, nil, src, BridgeEUI, coapPort, bridgeCoAPPort)
		if err != nil {
			t.Fatal(err)
		}
		frame, err := BuildData(NetworkKey, seq, BridgeEUI, src[:5], WrapAppPayload(ack, 900+uint32(seq), nil))
		if err != nil {
			t.Fatal(err)
		}
		e.OnFrame(frame.Bytes())
	}
	other := []byte{0x67, 0x45, 0x23, 0x31, 0x07, 0xc5, 0x1b, 0x00}
	feed(other, "2.04", firstReq.MID, firstReq.Token, 0x41)
	feed(wire, "2.04", firstReq.MID+1, firstReq.Token, 0x42)
	feed(wire, "2.04", firstReq.MID, []byte{0}, 0x43)
	if e.Pending("RU1000000001") != 1 {
		t.Fatal("wrong source, MID, or token completed DHW write")
	}
	feed(wire, "4.00", firstReq.MID, firstReq.Token, 0x44)
	if e.Pending("RU1000000001") != 0 {
		t.Fatal("matching DHW 4.xx left the write pending")
	}
	history := e.QueueHistory("RU1000000001")
	if len(history) != 1 || history[0].Result != "dropped" || history[0].Reason != "coap-4.00" {
		t.Fatalf("4.xx history=%+v, want dropped/coap-4.00", history)
	}

	if err := e.EnqueueDHWControl("RU1000000001", 0, 53.0, true); err != nil {
		t.Fatal(err)
	}
	terminal, ok := e.nextDownlink(wire, label)
	if !ok || len(terminal) != 1 {
		t.Fatalf("terminal=%+v ok=%t", terminal, ok)
	}
	terminalData, err := ParseData(NetworkKey, terminal[0].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	terminalReq := ParseApp(terminalData.Plaintext).CoAP
	feed(wire, "2.04", terminalReq.MID, terminalReq.Token, 0x45)
	if e.Pending("RU1000000001") != 0 {
		t.Fatal("matching DHW 2.04 did not complete the write")
	}
	history = e.QueueHistory("RU1000000001")
	if len(history) != 2 || history[0].Result != "delivered" || history[0].Reason != "coap-ack" {
		t.Fatalf("2.04 history=%+v, want delivered/coap-ack", history)
	}
	wantState := "61600102618001006020010261e00100624001026260010162800214b462e00100"
	if got := hex.EncodeToString(e.zoneState[zoneKey(0, "z/s")]); got != wantState {
		t.Errorf("confirmed DHW state=%s, want %s", got, wantState)
	}
}

func TestEnqueueBoostRequiresMatchingCoAP204(t *testing.T) {
	e, _, _ := newTestEmu(t)
	if err := e.EnqueueBoost("camera", 3, true); err != nil {
		t.Fatal(err)
	}
	wire, label, err := e.resolve("camera")
	if err != nil {
		t.Fatal(err)
	}
	actions, ok := e.nextDownlink(wire, label)
	if !ok || len(actions) != 1 || !actions[0].WaitResponse || !actions[0].CoAPTransaction {
		t.Fatalf("actions=%+v ok=%t, want CoAP-confirmed boost", actions, ok)
	}
	data, err := ParseData(NetworkKey, actions[0].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	request := ParseApp(data.Plaintext).CoAP
	e.OnFrame(BuildEnhAck(data.Seq, BridgeEUI, wire, 0))
	if e.Pending("camera") != 1 {
		t.Fatal("Enhanced ACK completed boost")
	}

	feed := func(code string, token []byte, seq byte) {
		t.Helper()
		ack, err := BuildCoAPAck(code, request.MID, token, nil, wire, BridgeEUI, coapPort, bridgeCoAPPort)
		if err != nil {
			t.Fatal(err)
		}
		frame, err := BuildData(NetworkKey, seq, BridgeEUI, wire[:5], WrapAppPayload(ack, 900+uint32(seq), nil))
		if err != nil {
			t.Fatal(err)
		}
		e.OnFrame(frame.Bytes())
	}
	feed("2.05", request.Token, 0x41)
	feed("2.04", []byte{0}, 0x42)
	if e.Pending("camera") != 1 {
		t.Fatal("wrong CoAP code or token completed boost")
	}
	feed("4.04", request.Token, 0x43)
	if e.Pending("camera") != 0 {
		t.Fatalf("matching 4.04 left boost pending=%d", e.Pending("camera"))
	}
	history := e.QueueHistory("camera")
	if len(history) != 1 || history[0].Result != "dropped" || history[0].Reason != "coap-4.04" {
		t.Fatalf("4.04 history=%+v, want dropped/coap-4.04", history)
	}

	if err := e.EnqueueBoost("camera", 3, true); err != nil {
		t.Fatal(err)
	}
	actions, ok = e.nextDownlink(wire, label)
	if !ok || len(actions) != 1 {
		t.Fatalf("retry actions=%+v ok=%t", actions, ok)
	}
	data, err = ParseData(NetworkKey, actions[0].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	request = ParseApp(data.Plaintext).CoAP
	feed("2.04", request.Token, 0x43)
	if e.Pending("camera") != 0 {
		t.Fatalf("matching 2.04 left boost pending=%d", e.Pending("camera"))
	}
	history = e.QueueHistory("camera")
	if len(history) != 2 || history[0].Result != "delivered" || history[0].Reason != "coap-ack" {
		t.Fatalf("history=%+v, want delivered/coap-ack", history)
	}

	if err := e.EnqueueBoost("camera", 3, true); err != nil {
		t.Fatal(err)
	}
	actions, ok = e.nextDownlink(wire, label)
	if !ok || len(actions) != 1 {
		t.Fatalf("4.05 actions=%+v ok=%t", actions, ok)
	}
	data, err = ParseData(NetworkKey, actions[0].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	request = ParseApp(data.Plaintext).CoAP
	feed("4.05", request.Token, 0x44)
	if e.Pending("camera") != 0 {
		t.Fatalf("matching 4.05 left boost pending=%d", e.Pending("camera"))
	}
	history = e.QueueHistory("camera")
	if len(history) != 3 || history[0].Result != "dropped" || history[0].Reason != "coap-4.05" {
		t.Fatalf("4.05 history=%+v, want dropped/coap-4.05", history)
	}
}

func TestBoostTimerQueuesCapturedRestore(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	cfg := fixtureInstall(t).EmuConfig()
	cfg.Now = func() time.Time { return now }
	e := NewEmu(cfg, nil)
	if err := e.EnqueueBoost("camera", 10, true); err != nil {
		t.Fatal(err)
	}
	if due, err := e.ExpireBoosts(now.Add(BoostDuration)); err != nil || len(due) != 0 {
		t.Fatalf("expiry while the boost waits in the queue: due=%v err=%v", due, err)
	}
	answerQueued(t, e, mustHex(cameraEUIHex), "2.04")
	if due, err := e.ExpireBoosts(now.Add(BoostDuration - time.Second)); err != nil || len(due) != 0 {
		t.Fatalf("early expiry due=%v err=%v", due, err)
	}
	due, err := e.ExpireBoosts(now.Add(BoostDuration))
	if err != nil || len(due) != 1 {
		t.Fatalf("expiry due=%v err=%v", due, err)
	}
	wire, _, err := e.resolve("camera")
	if err != nil {
		t.Fatal(err)
	}
	queue := e.queue[hexEUI(wire)]
	if len(queue) != 1 || queue[0].kind != "boost" {
		t.Fatalf("queue=%+v, want the restore", queue)
	}
	msg, err := ParseCoAP(WrapAppPayload(queue[0].coap, 1, nil)[8:])
	if err != nil {
		t.Fatal(err)
	}
	// The captured return to schedule without 6440: a timer that expires on
	// its own ends the overlay with no marker.
	want := strings.TrimSuffix(capturedSetpointAutoBody, "6440020001")
	if msg.Path != "z/s" || msg.Query != "id=10" || hex.EncodeToString(msg.Payload) != want {
		t.Fatalf("restore=%s %s payload=%x, want %s", msg.Code, msg.URI(), msg.Payload, want)
	}
	if len(e.ExportDesiredState().Boost) != 1 {
		t.Fatal("the queued restore dropped the stored timer before the valve took it")
	}
	answerQueued(t, e, wire, "2.04")
	if len(e.ExportDesiredState().Boost) != 0 {
		t.Fatal("expired timer remained persistent after the restore")
	}
}

// TestEnqueueFlowSetpoint: the CH flow command validates its target and range and
// registers the served value; it is not a queued downlink.
func TestEnqueueFlowSetpoint(t *testing.T) {
	e, _, _ := newTestEmu(t)
	if err := e.EnqueueFlowSetpoint("RU1000000001", 1, 48.0); err != nil {
		t.Fatal(err)
	}
	if e.FlowPending() != 1 {
		t.Fatalf("flow pending=%d, want 1", e.FlowPending())
	}
	if e.Pending("RU1000000001") != 0 {
		t.Errorf("flow is served, not queued: downlink pending=%d, want 0", e.Pending("RU1000000001"))
	}
	if err := e.EnqueueFlowSetpoint("RU1000000001", 1, 90.0); err == nil {
		t.Error("90 C is above the flow range and should be refused")
	}
	if err := e.EnqueueFlowSetpoint("nosuchnode", 1, 48.0); err == nil {
		t.Error("an unknown target should be refused")
	}
}

// TestServedFlowSetpoint: with a flow setpoint registered, the wall thermostat's
// GET of "h/<home>/c/1/config" is answered with a Data frame carrying tag 2040,
// and the pending count clears once fetched.
func TestServedFlowSetpoint(t *testing.T) {
	ru := NodeByAny("RU1000000001")
	if ru == nil {
		t.Fatal("RU wall thermostat not in inventory")
	}
	ruEUI := mustHex(ru.WireLE)

	// Synthesize the thermostat's circuit-config GET (device 5683 -> bridge 4005).
	get, err := BuildCoAP(CoAPReq{
		Type:    "CON",
		Code:    "0.01",
		MID:     0xabcd,
		Token:   []byte{0x07},
		Path:    []string{"h", "7654321", "c", "1", "config"},
		SrcEUI:  ruEUI,
		DstEUI:  BridgeEUI,
		SrcPort: coapPort,
		DstPort: bridgeCoAPPort,
	})
	if err != nil {
		t.Fatal(err)
	}
	frame, err := BuildData(NetworkKey, 0x11, BridgeEUI, ruEUI[:5], WrapAppPayload(get, 500, nil))
	if err != nil {
		t.Fatal(err)
	}

	e, _, _ := newTestEmu(t)
	if err := e.EnqueueFlowSetpoint("RU1000000001", 1, 48.0); err != nil {
		t.Fatal(err)
	}
	actions := e.OnFrame(frame.Bytes())

	var served *AppPayload
	for _, a := range actions {
		if a.Kind != ActData {
			continue
		}
		df, err := ParseData(NetworkKey, a.Bytes)
		if err != nil {
			continue
		}
		if app := ParseApp(df.Plaintext); app != nil && app.CoAP != nil && app.CoAP.MID == 0xabcd {
			served = app
		}
	}
	if served == nil {
		t.Fatal("no served answer to the c/1/config GET")
	}
	if served.CoAP.Code != "2.05" {
		t.Errorf("served code = %s, want 2.05", served.CoAP.Code)
	}
	if got := hex.EncodeToString(served.CoAP.Payload); got != "20400212c0" {
		t.Errorf("served payload = %s, want 20400212c0 (2040=48.00C)", got)
	}
	if e.FlowPending() != 0 {
		t.Errorf("flow pending=%d after fetch, want 0", e.FlowPending())
	}
}

func TestServedFlowSetpointETag(t *testing.T) {
	ru := mustHex("05a2cafa07c51b00")
	e, _, _ := newTestEmu(t)
	if err := e.EnqueueFlowSetpoint("RU1000000001", 1, 48.0); err != nil {
		t.Fatal(err)
	}
	etag := FlowSetpointETag(flowSetpointPayload(48.0))
	raw := deviceUplink(t, ru, CoAPReq{
		Type: "CON", Code: "0.01", MID: 0xabce, Token: []byte{0x08},
		Path:    []string{"h", "7654321", "c", "1", "config"},
		Options: []CoAPOption{{Num: 4, Val: etag}},
	})
	m := servedAnswer(t, e.OnFrame(raw), 0xabce)
	if m.Code != "2.03" || len(m.Payload) != 0 {
		t.Errorf("answer = %s payload=%x, want 2.03 without body", m.Code, m.Payload)
	}
}

func TestServedZoneConfigBlock2AndETag(t *testing.T) {
	in := fixtureInstall(t)
	dev := mustHex("08a4defa07c51b00")
	e := NewEmu(in.EmuConfig(), nil)
	doc, err := BuildZoneConfigDocument(in, 3)
	if err != nil {
		t.Fatal(err)
	}

	var body []byte
	for block := 0; ; block++ {
		value := byte(block<<4 | 3)
		raw := deviceUplink(t, dev, CoAPReq{
			Type: "CON", Code: "0.01", MID: uint16(0xb000 + block), Token: []byte{0x09},
			Path:    []string{"h", "7654321", "z", "3", "config"},
			Options: []CoAPOption{{Num: 23, Val: []byte{value}}},
		})
		actions := e.OnFrame(raw)
		m := servedBlockAnswer(t, actions, uint16(0xb000+block))
		if m.Code != "2.05" {
			t.Fatalf("block %d code=%s, want 2.05", block, m.Code)
		}
		etag, ok := m.Option(4)
		if !ok || !bytes.Equal(etag.Val, doc.ETag) {
			t.Fatalf("block %d ETag=%x, want %x", block, etag.Val, doc.ETag)
		}
		bo, ok := m.Option(23)
		if !ok {
			t.Fatalf("block %d has no Block2 option", block)
		}
		v := decodeCoAPUint(bo.Val)
		if got := int(v >> 4); got != block {
			t.Fatalf("response block=%d, want %d", got, block)
		}
		if got := byte(v & 7); got != 3 {
			t.Fatalf("response SZX=%d, want 3", got)
		}
		body = append(body, m.Payload...)
		dataFrames, fragmented := 0, false
		var fcfs []byte
		for _, a := range actions {
			if a.Kind == ActData && len(a.Bytes) > 127 {
				t.Errorf("block %d frame is %d B, exceeds PHY limit", block, len(a.Bytes))
			}
			if a.Kind == ActData {
				dataFrames++
				f, err := ParseData(NetworkKey, a.Bytes)
				if err != nil {
					t.Fatalf("block %d fragment: %v", block, err)
				}
				if IsFragment(f.Plaintext) {
					fragmented = true
					fcfs = append(fcfs, a.Bytes[0])
				}
			}
		}
		// Every fragment but the last carries Frame Pending: nothing is queued.
		for i, fcf := range fcfs {
			want := fcfDataPending[0]
			if i == len(fcfs)-1 {
				want = fcfData[0]
			}
			if fcf != want {
				t.Fatalf("block %d fragment %d FCF=%02x, want %02x", block, i, fcf, want)
			}
		}
		if len(m.Payload) == 128 && (!fragmented || dataFrames < 2) {
			t.Fatalf("block %d response has %d data frames, want fragmentation", block, dataFrames)
		}
		if v&0x08 == 0 {
			break
		}
	}
	if !bytes.Equal(body, doc.Body) {
		t.Errorf("reassembled body length=%d, want %d", len(body), len(doc.Body))
	}

	raw := deviceUplink(t, dev, CoAPReq{
		Type: "CON", Code: "0.01", MID: 0xb100, Token: []byte{0x0a},
		Path:    []string{"h", "7654321", "z", "3", "config"},
		Options: []CoAPOption{{Num: 4, Val: doc.ETag}, {Num: 23, Val: []byte{0x03}}},
	})
	m := servedAnswer(t, e.OnFrame(raw), 0xb100)
	if m.Code != "2.03" || len(m.Payload) != 0 {
		t.Errorf("conditional answer=%s payload=%x, want empty 2.03", m.Code, m.Payload)
	}
	// The captured 2.03 Valid echoes the ETag the request validated.
	if etag, ok := m.Option(4); !ok || !bytes.Equal(etag.Val, doc.ETag) {
		t.Errorf("2.03 ETag=%x present=%v, want %x", etag.Val, ok, doc.ETag)
	}

	// A stale ETag gets the document back, not a 2.03.
	raw = deviceUplink(t, dev, CoAPReq{
		Type: "CON", Code: "0.01", MID: 0xb101, Token: []byte{0x0b},
		Path:    []string{"h", "7654321", "z", "3", "config"},
		Options: []CoAPOption{{Num: 4, Val: []byte{1, 2, 3, 4, 5, 6, 7, 8}}, {Num: 23, Val: []byte{0x03}}},
	})
	if m := servedBlockAnswer(t, e.OnFrame(raw), 0xb101); m.Code != "2.05" {
		t.Errorf("stale etag answer=%s, want 2.05", m.Code)
	}
}

func TestServedSchedulePullBlock2AndETag(t *testing.T) {
	dev := mustHex("02a6cefa07c51b00")
	e, _, _ := newTestEmu(t)
	entries := make([]ScheduleEntry, 0, 24)
	for minute := 0; minute < 24*60; minute += 60 {
		entries = append(entries, ScheduleEntry{Minute: minute, SetpointCenti: 500 + minute, Enabled: true})
	}
	if err := e.SetSchedule("camera", []ScheduleProgram{{DayMask: 0x7f, Entries: entries}}); err != nil {
		t.Fatal(err)
	}
	e.inst = &Install{HomeID: 7654321}
	want := EncodeSchedule(EncodeScheduleDayProgram(0x7f, entries...))

	var body, etag []byte
	for block := 0; ; block++ {
		raw := deviceUplink(t, dev, CoAPReq{
			Type: "CON", Code: "0.01", MID: uint16(0xb300 + block), Token: []byte{0x0c},
			Path:    []string{"h", "7654321", "z", "3", "sched"},
			Options: []CoAPOption{{Num: 23, Val: []byte{byte(block<<4 | 3)}}},
		})
		m := servedBlockAnswer(t, e.OnFrame(raw), uint16(0xb300+block))
		if m.Code != "2.05" {
			t.Fatalf("block %d code=%s, want 2.05", block, m.Code)
		}
		gotETag, ok := m.Option(4)
		if !ok || len(gotETag.Val) != 8 {
			t.Fatalf("block %d ETag=%x, want 8 bytes", block, gotETag.Val)
		}
		if etag == nil {
			etag = append([]byte(nil), gotETag.Val...)
		} else if !bytes.Equal(etag, gotETag.Val) {
			t.Fatalf("block %d ETag=%x, want %x", block, gotETag.Val, etag)
		}
		blockOpt, ok := m.Option(23)
		if !ok || decodeCoAPUint(blockOpt.Val)>>4 != uint32(block) || byte(decodeCoAPUint(blockOpt.Val)&7) != 3 {
			t.Fatalf("block %d option=%x, want num=%d szx=3", block, blockOpt.Val, block)
		}
		body = append(body, m.Payload...)
		if decodeCoAPUint(blockOpt.Val)&0x08 == 0 {
			break
		}
	}
	if !bytes.Equal(body, want) {
		t.Errorf("schedule body=%x, want %x", body, want)
	}

	raw := deviceUplink(t, dev, CoAPReq{
		Type: "CON", Code: "0.01", MID: 0xb400, Token: []byte{0x0d},
		Path: []string{"d", "sched"}, Query: []string{"id=3"},
		Options: []CoAPOption{{Num: 4, Val: etag}, {Num: 23, Val: []byte{0x03}}},
	})
	m := servedAnswer(t, e.OnFrame(raw), 0xb400)
	if m.Code != "2.03" || len(m.Payload) != 0 {
		t.Errorf("conditional answer=%s payload=%x, want empty 2.03", m.Code, m.Payload)
	}
}

// TestServedScheduleWithoutDocumentIsValid: a device with no schedule document
// gets 2.03, never a 2.05 whose empty body would disable its stored program.
func TestServedScheduleWithoutDocumentIsValid(t *testing.T) {
	dev := mustHex("02a6cefa07c51b00")
	e, _, _ := newTestEmu(t)
	e.inst = &Install{HomeID: 7654321}
	zeroETag := make([]byte, 8)

	cases := []struct {
		name  string
		path  []string
		query []string
		opts  []CoAPOption
	}{
		{"zone path with etag", []string{"h", "7654321", "z", "10", "sched"}, nil,
			[]CoAPOption{{Num: 4, Val: zeroETag}, {Num: 23, Val: []byte{0x03}}}},
		{"device path with etag", []string{"d", "sched"}, []string{"id=3"},
			[]CoAPOption{{Num: 4, Val: zeroETag}, {Num: 23, Val: []byte{0x03}}}},
		{"zone path without etag", []string{"h", "7654321", "z", "10", "sched"}, nil,
			[]CoAPOption{{Num: 23, Val: []byte{0x03}}}},
	}
	for i, c := range cases {
		mid := uint16(0xb500 + i)
		raw := deviceUplink(t, dev, CoAPReq{
			Type: "CON", Code: "0.01", MID: mid, Token: []byte{0x0e},
			Path: c.path, Query: c.query, Options: c.opts,
		})
		m := servedAnswer(t, e.OnFrame(raw), mid)
		if m.Code != "2.03" || len(m.Payload) != 0 {
			t.Errorf("%s: answer=%s payload=%x, want empty 2.03", c.name, m.Code, m.Payload)
		}
		reqETag, sent := (&CoAPMessage{Options: c.opts}).Option(4)
		gotETag, echoed := m.Option(4)
		if echoed != sent || (sent && !bytes.Equal(gotETag.Val, reqETag.Val)) {
			t.Errorf("%s: ETag=%x present=%v, want the presented %x", c.name, gotETag.Val, echoed, reqETag.Val)
		}
	}
}

// TestSetScheduleRequiresCompleteOrderedWeek: the day programs must cover the
// week exactly once and every program must run from minute 0 upwards.
func TestSetScheduleRequiresCompleteOrderedWeek(t *testing.T) {
	e, _, _ := newTestEmu(t)
	day := []ScheduleEntry{{Minute: 0, SetpointCenti: 500, Enabled: true}}
	for _, tc := range []struct {
		name     string
		programs []ScheduleProgram
	}{
		{"no program", nil},
		{"empty mask", []ScheduleProgram{{DayMask: 0, Entries: day}}},
		{"partial week", []ScheduleProgram{{DayMask: 0x01, Entries: day}}},
		{"missing day", []ScheduleProgram{{DayMask: 0x1f, Entries: day}, {DayMask: 0x40, Entries: day}}},
		{"overlapping masks", []ScheduleProgram{{DayMask: 0x3f, Entries: day}, {DayMask: 0x60, Entries: day}}},
		{"first entry after 0", []ScheduleProgram{{DayMask: 0x7f, Entries: []ScheduleEntry{{Minute: 1, SetpointCenti: 500, Enabled: true}}}}},
		{"repeated minute", []ScheduleProgram{{DayMask: 0x7f, Entries: []ScheduleEntry{{Minute: 0, SetpointCenti: 500}, {Minute: 0, SetpointCenti: 700}}}}},
		{"decreasing minutes", []ScheduleProgram{{DayMask: 0x7f, Entries: []ScheduleEntry{{Minute: 0, SetpointCenti: 500}, {Minute: 360, SetpointCenti: 700}, {Minute: 120, SetpointCenti: 900}}}}},
		{"minute past the day", []ScheduleProgram{{DayMask: 0x7f, Entries: []ScheduleEntry{{Minute: 0, SetpointCenti: 500}, {Minute: 1440, SetpointCenti: 700}}}}},
		{"second program empty", []ScheduleProgram{{DayMask: 0x1f, Entries: day}, {DayMask: 0x60}}},
	} {
		if err := e.SetSchedule("camera", tc.programs); err == nil {
			t.Errorf("SetSchedule(%s) accepted %+v", tc.name, tc.programs)
		}
	}
}

// TestSetScheduleDayBlobBoundary: the device stores each day program in a slot
// of at most 255 bytes, so a program that encodes to 255 bytes passes and one
// that encodes to 256 is refused.
func TestSetScheduleDayBlobBoundary(t *testing.T) {
	// Minute 0 and minutes below 128 take a one-byte varint (9-byte field),
	// later minutes two bytes (10-byte field); the day mask field takes 2.
	day := func(short, long int) []ScheduleEntry {
		out := []ScheduleEntry{{Minute: 0, SetpointCenti: 2100, Enabled: true}}
		for m := 1; m <= short; m++ {
			out = append(out, ScheduleEntry{Minute: m, SetpointCenti: 2100, Enabled: true})
		}
		for i := 0; i < long; i++ {
			out = append(out, ScheduleEntry{Minute: 128 + i, SetpointCenti: 2100, Enabled: true})
		}
		return out
	}
	e, _, _ := newTestEmu(t)
	for _, tc := range []struct {
		entries []ScheduleEntry
		size    int
		ok      bool
	}{
		{day(6, 19), 255, true},
		{day(5, 20), 256, false},
	} {
		if n := len(EncodeScheduleDayProgram(0x01, tc.entries...)); n != tc.size {
			t.Fatalf("fixture encodes to %d bytes, want %d", n, tc.size)
		}
		err := e.SetSchedule("camera", []ScheduleProgram{{DayMask: 0x7f, Entries: tc.entries}})
		if (err == nil) != tc.ok {
			t.Errorf("SetSchedule(%d-byte day) err = %v, want ok=%t", tc.size, err, tc.ok)
		}
	}
}

// TestSetScheduleAcceptsSeveralDayPrograms: a working week and a weekend
// program are one document, and the API reads the programs back from it.
func TestSetScheduleAcceptsSeveralDayPrograms(t *testing.T) {
	e, _, _ := newTestEmu(t)
	programs := []ScheduleProgram{
		{DayMask: 0x1f, Entries: []ScheduleEntry{
			{Minute: 0, SetpointCenti: 1600, Enabled: true},
			{Minute: 390, SetpointCenti: 2100, Enabled: true},
			{Minute: 1380, SetpointCenti: 1600, Enabled: false},
		}},
		{DayMask: 0x60, Entries: []ScheduleEntry{
			{Minute: 0, SetpointCenti: 1750, Enabled: true},
			{Minute: 1439, SetpointCenti: -50, Enabled: true},
		}},
	}
	if err := e.SetSchedule("camera", programs); err != nil {
		t.Fatal(err)
	}
	got, set, err := e.Schedule("camera")
	if err != nil || !set {
		t.Fatalf("Schedule = %v, %t, %v", got, set, err)
	}
	if !reflect.DeepEqual(got, programs) {
		t.Errorf("programs = %+v, want %+v", got, programs)
	}
	if _, set, err := e.Schedule("04a3defa07c51b00"); err != nil || set {
		t.Errorf("Schedule of a device with no document = %t, %v", set, err)
	}
}

func servedBlockAnswer(t *testing.T, actions []Action, mid uint16) *CoAPMessage {
	t.Helper()
	reasm := NewReassembler()
	for _, a := range actions {
		if a.Kind != ActData {
			continue
		}
		frame, err := ParseData(NetworkKey, a.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		if app := ParseApp(frame.Plaintext); app != nil && app.CoAP != nil && app.CoAP.MID == mid {
			return app.CoAP
		}
		msg, done := reasm.Add(frame)
		if done && msg.MID == mid {
			return msg
		}
	}
	t.Fatalf("no Block2 answer with mid=%04x among %v", mid, reasons(actions))
	return nil
}

func TestServedVolatileSettings(t *testing.T) {
	dev := mustHex("02a6cefa07c51b00")
	e, _, _ := newTestEmu(t)
	if err := e.EnqueueChildLock("camera", 10, true); err != nil {
		t.Fatal(err)
	}
	// The served lock is the one the valve took.
	actions, ok, err := e.NextQueuedDownlink("camera")
	if err != nil || !ok {
		t.Fatalf("no queued child lock: ok=%v err=%v", ok, err)
	}
	df, err := ParseData(NetworkKey, actions[0].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	e.OnFrame(BuildEnhAck(df.Seq, BridgeEUI, dev, 0))
	unitF := true
	if err := e.EnqueueConfig("camera", 10, ConfigWrite{TempUnitF: &unitF}); err != nil {
		t.Fatal(err)
	}
	for i, tc := range []struct {
		path string
		want string
	}{{"d/lock", "02900101"}, {"d/dispsettings", "013f0101"}} {
		raw := deviceUplink(t, dev, CoAPReq{
			Type: "CON", Code: "0.01", MID: uint16(0xb200 + i), Token: []byte{0x0b},
			Path: strings.Split(tc.path, "/"),
		})
		m := servedAnswer(t, e.OnFrame(raw), uint16(0xb200+i))
		if got := hex.EncodeToString(m.Payload); got != tc.want {
			t.Errorf("GET %s payload=%s, want %s", tc.path, got, tc.want)
		}
	}
}

func TestOnFrameRecognizesUnicastICMPv6(t *testing.T) {
	plaintext := mustHex("c51b0004dbd300007a333a810009d80e44")
	dev := mustHex("05a2cafa07c51b00")
	frame, err := BuildData(NetworkKey, 0x31, BridgeEUI, dev[:5], plaintext)
	if err != nil {
		t.Fatal(err)
	}
	e, log, _ := newTestEmu(t)
	actions := e.OnFrame(frame.Bytes())
	if typ, ok := UnicastICMPv6Type(plaintext); !ok || typ != 129 {
		t.Fatalf("ICMPv6 = %d/%v, want echo reply type 129", typ, ok)
	}
	if e.Stats().ICMPv6 != 1 {
		t.Errorf("ICMPv6 counter=%d, want 1", e.Stats().ICMPv6)
	}
	if len(actions) != 1 || actions[0].Kind != ActEnhAck {
		t.Errorf("actions=%v, want only Enhanced ACK", reasons(actions))
	}
	if got := e.TakeAcceptedInboundIPv6(); !bytes.Equal(got, dev) {
		t.Errorf("accepted source=%x, want %x", got, dev)
	}
	if log.Counts()[rsnAppParse] != 0 || log.Counts()[rsnNoCoAP] != 0 {
		t.Errorf("ICMPv6 logged as app error: %v", log.Counts())
	}
}

func TestAcceptedInboundIPv6FollowsFragmentReassembly(t *testing.T) {
	dev := mustHex("02a6cefa07c51b00")
	e, _, _ := newTestEmu(t)
	pkt, err := BuildCoAP(CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x7310, Token: []byte{0x73},
		Path: []string{"d", "config"}, Query: []string{"id=10"},
		Payload: bytes.Repeat([]byte{0x73}, 260),
		SrcEUI:  dev, DstEUI: BridgeEUI, SrcPort: coapPort, DstPort: bridgeCoAPPort,
	})
	if err != nil {
		t.Fatal(err)
	}
	fragments, err := Fragment6LoWPANDatagram(pkt, 0x7310)
	if err != nil {
		t.Fatal(err)
	}
	for i, fragment := range fragments {
		frame, err := BuildDataPending(NetworkKey, byte(i), BridgeEUI, dev[:5], WrapAppPayload(fragment, uint32(i+1), nil))
		if err != nil {
			t.Fatal(err)
		}
		e.OnFrame(frame.Bytes())
		got := e.TakeAcceptedInboundIPv6()
		if i != len(fragments)-1 && got != nil {
			t.Fatalf("fragment %d accepted source=%x before reassembly", i, got)
		}
		if i == len(fragments)-1 && !bytes.Equal(got, dev) {
			t.Fatalf("completed source=%x, want %x", got, dev)
		}
	}
	if got := e.TakeAcceptedInboundIPv6(); got != nil {
		t.Fatalf("event was not consumed: %x", got)
	}
}

// deviceUplink synthesizes a device-to-bridge Data frame carrying one CoAP
// request: the addressing and ports are the uplink's, the caller fills the rest.
func deviceUplink(t *testing.T, dev []byte, r CoAPReq) []byte {
	t.Helper()
	r.SrcEUI, r.DstEUI = dev, BridgeEUI
	r.SrcPort, r.DstPort = coapPort, bridgeCoAPPort
	pkt, err := BuildCoAP(r)
	if err != nil {
		t.Fatal(err)
	}
	f, err := BuildData(NetworkKey, 0x21, BridgeEUI, dev[:5], WrapAppPayload(pkt, 700, nil))
	if err != nil {
		t.Fatal(err)
	}
	return f.Bytes()
}

// servedAnswer returns the emulator's CoAP answer carrying mid.
func servedAnswer(t *testing.T, actions []Action, mid uint16) *CoAPMessage {
	t.Helper()
	for _, a := range actions {
		if a.Kind != ActData {
			continue
		}
		df, err := ParseData(NetworkKey, a.Bytes)
		if err != nil {
			continue
		}
		if app := ParseApp(df.Plaintext); app != nil && app.CoAP != nil && app.CoAP.MID == mid {
			return app.CoAP
		}
	}
	t.Fatalf("no answer with mid=%04x among %v", mid, reasons(actions))
	return nil
}

// timeGET is a device's GET of the "time" resource.
func timeGET(t *testing.T, dev []byte, mid uint16) []byte {
	t.Helper()
	return deviceUplink(t, dev, CoAPReq{
		Type: "CON", Code: "0.01", MID: mid, Token: []byte{},
		Path: []string{"time"}, Options: []CoAPOption{optOctets},
	})
}

// TestFaultSuppressTime: with the injection on, a GET of "time" gets the bare
// ack the resource catalogue falls through to and no clock at all. The device is
// then left with rt_time_now_get returning -3 and no weekly schedule, which is
// the second half of test F12.
func TestFaultSuppressTime(t *testing.T) {
	dev := mustHex("02a6cefa07c51b00")
	log := NewUnhandledLogger(nil)
	e := NewEmu(EmuConfig{TxCounter: 100000, SuppressTime: true}, log)

	m := servedAnswer(t, e.OnFrame(timeGET(t, dev, 0x0501)), 0x0501)
	if len(m.Payload) != 0 {
		t.Errorf("payload = %x with time suppressed, want no clock at all", m.Payload)
	}
	if st := e.Stats(); st.TimeSuppressed != 1 {
		t.Errorf("TimeSuppressed=%d, want 1", st.TimeSuppressed)
	}
	if n := countPrefix(log.Counts(), rsnTimeSuppressed); n != 1 {
		t.Errorf("%s logged %d times, want 1: a suppressed answer must be on the record", rsnTimeSuppressed, n)
	}
}

// TestFaultDelayTime: with the delay on, the answer is still built correctly but
// carries NotBefore, so whoever drives the radio holds it past the device's 5 s
// discard deadline (coap_rx_timed_response_dispatch@0x22748). F12, first half.
func TestFaultDelayTime(t *testing.T) {
	dev := mustHex("02a6cefa07c51b00")
	pinned := time.Unix(1786619383, 0)
	log := NewUnhandledLogger(nil)
	e := NewEmu(EmuConfig{
		TxCounter: 100000,
		TimeDelay: 7 * time.Second,
		Now:       func() time.Time { return pinned },
	}, log)

	actions := e.OnFrame(timeGET(t, dev, 0x0502))
	m := servedAnswer(t, actions, 0x0502)
	if got := hex.EncodeToString(m.Payload); got != "0df7a57d6a" {
		t.Errorf("payload = %s, want the normal clock body: the delay must not corrupt it", got)
	}
	var held *Action
	for i := range actions {
		if !actions[i].NotBefore.IsZero() {
			held = &actions[i]
		}
	}
	if held == nil {
		t.Fatal("no action carries NotBefore: the answer would go out on time")
	}
	if want := pinned.Add(7 * time.Second); !held.NotBefore.Equal(want) {
		t.Errorf("NotBefore = %v, want %v", held.NotBefore, want)
	}
	if st := e.Stats(); st.TimeDelayed != 1 {
		t.Errorf("TimeDelayed=%d, want 1", st.TimeDelayed)
	}
	if n := countPrefix(log.Counts(), rsnTimeDelayed); n != 1 {
		t.Errorf("%s logged %d times, want 1", rsnTimeDelayed, n)
	}
}

// TestFaultTimeOffByDefault: the zero EmuConfig answers "time" normally and
// immediately. The injections have to be asked for.
func TestFaultTimeOffByDefault(t *testing.T) {
	dev := mustHex("02a6cefa07c51b00")
	e := NewEmu(EmuConfig{TxCounter: 100000, Now: func() time.Time { return time.Unix(1786619383, 0) }}, nil)
	actions := e.OnFrame(timeGET(t, dev, 0x0503))
	if got := hex.EncodeToString(servedAnswer(t, actions, 0x0503).Payload); got != "0df7a57d6a" {
		t.Errorf("payload = %s, want the clock body", got)
	}
	for _, a := range actions {
		if !a.NotBefore.IsZero() {
			t.Errorf("[%s] carries NotBefore=%v with no injection configured", a.Kind, a.NotBefore)
		}
	}
	if st := e.Stats(); st.TimeSuppressed != 0 || st.TimeDelayed != 0 {
		t.Errorf("fault counters moved on a clean run: suppressed=%d delayed=%d",
			st.TimeSuppressed, st.TimeDelayed)
	}
}

// TestFaultDelayOnlyTouchesTime: the delay is scoped to the "time" resource.
// Holding an unrelated answer back would break the CSL rendezvous it belongs to.
func TestFaultDelayOnlyTouchesTime(t *testing.T) {
	dev := mustHex("02a6cefa07c51b00")
	e := NewEmu(EmuConfig{TxCounter: 100000, TimeDelay: 7 * time.Second}, nil)
	actions := e.OnFrame(deviceUplink(t, dev, CoAPReq{
		Type: "CON", Code: "0.01", MID: 0x0504, Token: []byte{1},
		Path: []string{"d", "lock"}, Options: []CoAPOption{optOctets},
	}))
	if len(servedAnswer(t, actions, 0x0504).Payload) == 0 {
		t.Error("d/lock lost its body")
	}
	for _, a := range actions {
		if !a.NotBefore.IsZero() {
			t.Errorf("[%s] %s was held back, but only \"time\" may be", a.Kind, a.Reason)
		}
	}
	if st := e.Stats(); st.TimeDelayed != 0 {
		t.Errorf("TimeDelayed=%d on a request that is not for time", st.TimeDelayed)
	}
}

// TestServedTimeResource: a device's GET of "time" is answered synchronously
// with the cloud's wire form - a 2.05 echoing the MID, carrying no options at
// all and the 5-byte nanopb epoch of the emulator's clock.
func TestServedTimeResource(t *testing.T) {
	dev := mustHex("02a6cefa07c51b00")
	raw := deviceUplink(t, dev, CoAPReq{
		Type:  "CON",
		Code:  "0.01",
		MID:   0x04d2,
		Token: []byte{}, // the device sends TKL=0
		Path:  []string{"time"},
		Options: []CoAPOption{
			optOctets,
			{Num: 2048, Val: mustHex("5e55000000000001")}, // its session cookie
		},
	})

	pinned := time.Unix(1786619383, 0)
	log := NewUnhandledLogger(nil)
	e := NewEmu(EmuConfig{TxCounter: 100000, Now: func() time.Time { return pinned }}, log)
	m := servedAnswer(t, e.OnFrame(raw), 0x04d2)

	if m.Type != "ACK" || m.Code != "2.05" {
		t.Errorf("type/code = %s %s, want ACK 2.05", m.Type, m.Code)
	}
	if len(m.Token) != 0 {
		t.Errorf("token = %x, want the request's empty one", m.Token)
	}
	if len(m.Options) != 0 {
		t.Errorf("options = %v, want none: the cloud sends not even a Content-Format", m.Options)
	}
	if got := hex.EncodeToString(m.Payload); got != "0df7a57d6a" {
		t.Errorf("payload = %s, want 0df7a57d6a (epoch 1786619383 LE)", got)
	}

	// The option-2048 value the device presented is filed against the node.
	ns := e.Nodes()
	if len(ns) != 1 {
		t.Fatalf("got %d nodes, want 1", len(ns))
	}
	if got := hex.EncodeToString(ns[0].Option2048); got != "5e55000000000001" {
		t.Errorf("observed option 2048 = %s, want 5e55000000000001", got)
	}
	if n := countPrefix(log.Counts(), rsnSessionMismatch); n != 0 {
		t.Errorf("mismatch logged %d times with no session served yet, want 0", n)
	}
}

// TestServedAuthToken: auth/token carries the session id (025e) and the session
// lifetime (025f). The id is derived from the device wire EUI, so it survives an
// emulator restart, and a device echoing a different option 2048 is flagged.
func TestServedAuthToken(t *testing.T) {
	dev := mustHex("02a6cefa07c51b00")
	// fnv64a of the device wire EUI.
	const wantSession = "f72c98fb64b76e7a"
	raw := deviceUplink(t, dev, CoAPReq{
		Type:    "CON",
		Code:    "0.02",
		MID:     0x0301,
		Token:   []byte{0x09},
		Path:    []string{"auth", "token"},
		Options: []CoAPOption{optOctets},
		Payload: mustHex("0102"),
	})

	e, log, _ := newTestEmu(t)
	m := servedAnswer(t, e.OnFrame(raw), 0x0301)
	if m.Code != "2.05" {
		t.Errorf("code = %s, want 2.05", m.Code)
	}
	if got, want := hex.EncodeToString(m.Payload), "025e08"+wantSession+"025f0205a0"; got != want {
		t.Errorf("payload = %s, want %s", got, want)
	}

	// Deterministic: a fresh emulator serves the same device the same id.
	e2, _, _ := newTestEmu(t)
	if m2 := servedAnswer(t, e2.OnFrame(raw), 0x0301); !bytes.Equal(m2.Payload, m.Payload) {
		t.Errorf("restart changed the session id: %x then %x", m.Payload, m2.Payload)
	}

	// The device is meant to echo that id verbatim as option 2048; anything else
	// means our answer never landed.
	bad := deviceUplink(t, dev, CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x0302, Token: []byte{0x0a},
		Path:    []string{"d", "VA1000000005", "sen"},
		Options: []CoAPOption{{Num: 2048, Val: mustHex("5e5500000000000a")}},
	})
	e.OnFrame(bad)
	if n := countPrefix(log.Counts(), rsnSessionMismatch); n != 1 {
		t.Errorf("session mismatch logged %d times, want 1:\n%s", n, log.Summary())
	}
	ns := e.Nodes()
	if len(ns) != 1 {
		t.Fatalf("got %d nodes, want 1", len(ns))
	}
	if got := hex.EncodeToString(ns[0].SessionID); got != wantSession {
		t.Errorf("served session id = %s, want %s", got, wantSession)
	}
	if got := hex.EncodeToString(ns[0].Option2048); got != "5e5500000000000a" {
		t.Errorf("observed option 2048 = %s, want the echoed 5e5500000000000a", got)
	}
}

// TestServedAuthKey: auth/key, the bootstrap a device runs while its keystore
// id7 is unprovisioned, is answered with the same session pair as auth/token.
// The challenge it presents cannot be validated without that device's id2, and
// an empty 2.05 would leave it with no session id to echo as option 2048.
func TestServedAuthKey(t *testing.T) {
	dev := mustHex("02a6cefa07c51b00")
	// fnv64a of the device wire EUI, the same value auth/token derives.
	const wantSession = "f72c98fb64b76e7a"
	const wantBody = "025e08" + wantSession + "025f0205a0"
	raw := deviceUplink(t, dev, CoAPReq{
		Type: "CON", Code: "0.02", MID: 0x0401, Token: []byte{0x0b},
		Path: []string{"auth", "key"}, Options: []CoAPOption{optOctets},
		Payload: concat(
			TLV{Tag: "0260", Val: []byte("VA1000000005")}.Encode(),
			TLV{Tag: "0262", Val: mustHex("000102030405060708090a0b0c0d0e0f")}.Encode(),
		),
	})

	e, _, _ := newTestEmu(t)
	m := servedAnswer(t, e.OnFrame(raw), 0x0401)
	if m.Code != "2.05" {
		t.Errorf("code = %s, want 2.05", m.Code)
	}
	if got := hex.EncodeToString(m.Payload); got != wantBody {
		t.Fatalf("payload = %s, want %s", got, wantBody)
	}
	tlvs := parseTLVList(m.Payload)
	if len(tlvs) != 2 {
		t.Fatalf("body holds %d TLVs, want the session id and its lifetime", len(tlvs))
	}
	// 0261 is the permanent id7 grant: the device would decrypt it with its id2
	// and persist the plaintext forever, so serving one blind bricks its auth.
	if tlvs[0].Tag != "025e" || tlvs[1].Tag != "025f" {
		t.Fatalf("tags = %s %s, want 025e 025f", tlvs[0].Tag, tlvs[1].Tag)
	}
	if got := hex.EncodeToString(tlvs[0].Val); got != wantSession {
		t.Errorf("session id = %s, want %s", got, wantSession)
	}

	// Same device, same session: the id is derived from its wire EUI, so the
	// two join paths cannot hand out different ones.
	tok := deviceUplink(t, dev, CoAPReq{
		Type: "CON", Code: "0.02", MID: 0x0402, Token: []byte{0x0c},
		Path: []string{"auth", "token"}, Options: []CoAPOption{optOctets},
	})
	e2, _, _ := newTestEmu(t)
	if got := servedAnswer(t, e2.OnFrame(tok), 0x0402); !bytes.Equal(got.Payload, m.Payload) {
		t.Errorf("auth/token payload %x differs from auth/key %x", got.Payload, m.Payload)
	}
	ns := e.Nodes()
	if len(ns) != 1 || hex.EncodeToString(ns[0].SessionID) != wantSession {
		t.Errorf("served session id = %x, want %s filed against the node", ns[0].SessionID, wantSession)
	}
}

// authAttempt returns the queued auth answer among actions and its parsed frame.
func authAttempt(t *testing.T, actions []Action, mid uint16) (Action, *Frame, *CoAPMessage) {
	t.Helper()
	for _, a := range actions {
		if a.Kind != ActData {
			continue
		}
		df, err := ParseData(NetworkKey, a.Bytes)
		if err != nil {
			continue
		}
		if app := ParseApp(df.Plaintext); app != nil && app.CoAP != nil && app.CoAP.MID == mid {
			return a, df, app.CoAP
		}
	}
	t.Fatalf("no answer with mid=%04x among %v", mid, reasons(actions))
	return Action{}, nil, nil
}

// TestAuthReplyRetriesAsSameAnswer: the auth/token answer rides the downlink
// queue with the bootstrap train permitted and a MAC-level retry. A
// retransmission of the same MID gets the same CoAP answer and serves no new
// session, before and after the Enhanced ACK of an earlier attempt.
func TestAuthReplyRetriesAsSameAnswer(t *testing.T) {
	dev := mustHex("02a6cefa07c51b00")
	raw := deviceUplink(t, dev, CoAPReq{
		Type: "CON", Code: "0.02", MID: 0x0311, Token: []byte{0x11},
		Path: []string{"auth", "token"}, Options: []CoAPOption{optOctets},
	})
	e, log, _ := newTestEmu(t)
	target := hexEUI(dev)

	first, firstFrame, firstMsg := authAttempt(t, e.OnFrame(raw), 0x0311)
	if !first.CSLTx || !first.QueueAttempt || !first.CSLBootstrapAlways || !first.ResponseRetry || first.CoAPTransaction {
		t.Fatalf("auth answer flags = %+v, want a queued CSL response with bootstrap always", first)
	}
	if n := e.Pending(target); n != 1 {
		t.Fatalf("pending = %d, want the auth answer", n)
	}

	second, secondFrame, secondMsg := authAttempt(t, e.OnFrame(reframe(t, raw, 0x22, 701)), 0x0311)
	if !second.QueueAttempt || second.QueueTry != 2 {
		t.Fatalf("retransmission attempt = %+v, want try 2 of the same entry", second)
	}
	if secondFrame.Seq != firstFrame.Seq {
		t.Errorf("retry MAC seq = %02x, want %02x", secondFrame.Seq, firstFrame.Seq)
	}
	if !bytes.Equal(secondMsg.Raw, firstMsg.Raw) {
		t.Errorf("retry answer %x differs from %x", secondMsg.Raw, firstMsg.Raw)
	}
	if n := e.Pending(target); n != 1 {
		t.Errorf("pending = %d after a retransmission, want 1", n)
	}

	e.OnFrame(BuildEnhAck(secondFrame.Seq, BridgeEUI, dev, 0))
	if n := e.Pending(target); n != 0 {
		t.Fatalf("pending = %d after the Enhanced ACK, want 0", n)
	}
	_, _, thirdMsg := authAttempt(t, e.OnFrame(reframe(t, raw, 0x23, 702)), 0x0311)
	if !bytes.Equal(thirdMsg.Raw, firstMsg.Raw) {
		t.Errorf("answer after delivery %x differs from %x", thirdMsg.Raw, firstMsg.Raw)
	}
	if n := countPrefix(log.Counts(), rsnAuthAccepted); n != 1 {
		t.Errorf("auth accepted %d times for one MID, want 1", n)
	}

	next := deviceUplink(t, dev, CoAPReq{
		Type: "CON", Code: "0.02", MID: 0x0312, Token: []byte{0x12},
		Path: []string{"auth", "token"}, Options: []CoAPOption{optOctets},
	})
	authAttempt(t, e.OnFrame(next), 0x0312)
	if n := countPrefix(log.Counts(), rsnAuthAccepted); n != 2 {
		t.Errorf("auth accepted %d times for two MIDs, want 2", n)
	}
	for _, q := range e.Queue(target) {
		if q.MID == 0x0311 {
			t.Errorf("answer to the abandoned MID is still queued: %+v", q)
		}
	}
}

// TestSessionAdoptionFromLaterUplink: a request carrying the served session id
// as option 2048 marks the node adopted and logs it once. A later request
// without the option is logged once as a drift. A response is not a request
// and carries no session id.
func TestSessionAdoptionFromLaterUplink(t *testing.T) {
	dev := mustHex("02a6cefa07c51b00")
	const session = "f72c98fb64b76e7a"
	e, log, _ := newTestEmu(t)
	authAttempt(t, e.OnFrame(deviceUplink(t, dev, CoAPReq{
		Type: "CON", Code: "0.02", MID: 0x0321, Token: []byte{0x21},
		Path: []string{"auth", "token"}, Options: []CoAPOption{optOctets},
	})), 0x0321)
	view := func() ValveView {
		t.Helper()
		valves := e.Snapshot().Valves
		if len(valves) != 1 {
			t.Fatalf("got %d valves, want 1", len(valves))
		}
		return valves[0]
	}
	if v := view(); !v.SessionIssued || v.SessionAdopted {
		t.Fatalf("after auth issued=%t adopted=%t, want true false", v.SessionIssued, v.SessionAdopted)
	}

	for mid := uint16(0x0322); mid <= 0x0323; mid++ {
		e.OnFrame(deviceUplink(t, dev, CoAPReq{
			Type: "CON", Code: "0.01", MID: mid, Token: []byte{},
			Path: []string{"time"}, Options: []CoAPOption{optOctets, {Num: 2048, Val: mustHex(session)}},
		}))
	}
	if v := view(); !v.SessionIssued || !v.SessionAdopted {
		t.Fatalf("after echo issued=%t adopted=%t, want true true", v.SessionIssued, v.SessionAdopted)
	}
	if n := countPrefix(log.Counts(), rsnSessionAdopted); n != 1 {
		t.Errorf("adoption logged %d times, want 1:\n%s", n, log.Summary())
	}

	e.OnFrame(deviceUplink(t, dev, CoAPReq{
		Type: "NON", Code: "2.05", MID: 0x0324, Token: []byte{0x24},
	}))
	if v := view(); !v.SessionAdopted {
		t.Fatal("a response without option 2048 cleared the adoption")
	}
	for mid := uint16(0x0325); mid <= 0x0326; mid++ {
		e.OnFrame(deviceUplink(t, dev, CoAPReq{
			Type: "CON", Code: "0.03", MID: mid, Token: []byte{byte(mid)},
			Path: []string{"d", "VA1000000005", "sen"},
		}))
	}
	if v := view(); v.SessionAdopted {
		t.Fatal("requests without option 2048 left the node adopted")
	}
	if n := countPrefix(log.Counts(), rsnSessionMissing); n != 1 {
		t.Errorf("missing session logged %d times, want 1:\n%s", n, log.Summary())
	}
	if n := countPrefix(log.Counts(), rsnSessionMismatch); n != 0 {
		t.Errorf("mismatch logged %d times, want 0", n)
	}
}

// TestPiggybackSetpoint requires the valve's correlated application response.
func TestPiggybackSetpoint(t *testing.T) {
	camera := NodeByAny("camera")
	if camera == nil {
		t.Fatal("camera not in inventory")
	}
	raw, f, _ := firstUplink(t, func(fr *Frame, _ *AppPayload) bool {
		return hex.EncodeToString(fr.SrcEUI()) == camera.WireLE
	})

	e, _, _ := newTestEmu(t)
	if err := e.EnqueueSetpoint("camera", SetpointParams{
		ID: 10, Setpoint: 21.5, Sched: 18.0, Overlay: 2,
	}); err != nil {
		t.Fatal(err)
	}
	if e.Pending("camera") != 1 {
		t.Fatalf("pending=%d, want 1", e.Pending("camera"))
	}

	actions := e.OnFrame(raw)
	if actions[0].Kind != ActEnhAck {
		t.Fatalf("first action %v, want the EnhACK before the downlink", actions[0].Kind)
	}
	dl, ok := findAction(actions, ActData, "downlink setpoint")
	if !ok {
		t.Fatalf("no piggybacked downlink, got %v", reasons(actions))
	}
	if !dl.CSLTx {
		t.Fatalf("setpoint CSL plan=%+v", dl)
	}
	if dl.Node != "camera" {
		t.Errorf("action node=%q, want camera", dl.Node)
	}

	df, err := ParseData(NetworkKey, dl.Bytes)
	if err != nil {
		t.Fatalf("downlink does not decrypt: %v", err)
	}
	if !bytes.Equal(df.DstEUI, f.SrcEUI()) {
		t.Errorf("downlink dst=%x, want the camera %x", df.DstEUI, f.SrcEUI())
	}
	app := ParseApp(df.Plaintext)
	if app == nil || app.CoAP == nil {
		t.Fatal("downlink plaintext does not parse")
	}
	if app.CoAP.Type != "CON" || app.CoAP.Code != "0.03" {
		t.Errorf("downlink = %s %s, want CON 0.03", app.CoAP.Type, app.CoAP.Code)
	}
	if app.CoAP.URI() != "z/s?id=10" {
		t.Errorf("uri=%q, want z/s?id=10", app.CoAP.URI())
	}
	if app.CoAP.SrcPort != 4005 || app.CoAP.DstPort != 5683 {
		t.Errorf("ports %d->%d, want 4005->5683", app.CoAP.SrcPort, app.CoAP.DstPort)
	}
	want := map[string]string{"6280": "0866", "6240": "02", "6440": "0001"}
	got := map[string]string{}
	for _, tl := range app.TLVs {
		got[tl.Tag] = hex.EncodeToString(tl.Val)
	}
	for tag, v := range want {
		if got[tag] != v {
			t.Errorf("TLV %s=%s, want %s (all: %v)", tag, got[tag], v, got)
		}
	}
	if v, ok := got["6200"]; ok {
		t.Errorf("downlink carries 6200=%s; no captured plane does", v)
	}

	if st := e.Stats(); st.DownlinksSent != 1 || st.DownlinksDelivered != 0 {
		t.Errorf("stats sent=%d delivered=%d, want 1 and 0", st.DownlinksSent, st.DownlinksDelivered)
	}
	// MAC acknowledgement refreshes the link but does not prove application.
	e.OnFrame(BuildEnhAck(df.Seq, BridgeEUI, f.SrcEUI(), 0x0101))
	if e.Pending("camera") != 1 {
		t.Errorf("pending=%d after the device EnhACK, want 1", e.Pending("camera"))
	}
	ack, err := BuildCoAPAck("2.04", app.CoAP.MID, app.CoAP.Token, nil, f.SrcEUI(), BridgeEUI, coapPort, bridgeCoAPPort)
	if err != nil {
		t.Fatal(err)
	}
	ackFrame, err := BuildData(NetworkKey, 0x42, BridgeEUI, f.SrcEUI()[:5], WrapAppPayload(ack, 0x0102, nil))
	if err != nil {
		t.Fatal(err)
	}
	e.OnFrame(ackFrame.Bytes())
	if e.Pending("camera") != 0 {
		t.Errorf("pending=%d after the device CoAP response, want 0", e.Pending("camera"))
	}
	if st := e.Stats(); st.DownlinksDelivered != 1 {
		t.Errorf("delivered=%d, want 1", st.DownlinksDelivered)
	}
	cached := map[string]string{}
	for _, tlv := range parseTLVList(e.zoneState[zoneKey(10, "z/s")]) {
		cached[tlv.Tag] = hex.EncodeToString(tlv.Val)
	}
	wantCached := map[string]string{
		"6160": "02", "6180": "00", "6020": "01", "61e0": "00",
		"6240": "02", "6260": "01", "6280": "0866", "62e0": "00",
	}
	if !reflect.DeepEqual(cached, wantCached) {
		t.Errorf("cached zone state=%v, want complete confirmed block %v", cached, wantCached)
	}
}

func TestEnqueueSetpointRequiresCorrelatedApplicationResponse(t *testing.T) {
	feed := func(t *testing.T, e *EmuState, src []byte, code string, mid uint16, token []byte, seq byte) {
		t.Helper()
		ack, err := BuildCoAPAck(code, mid, token, nil, src, BridgeEUI, coapPort, bridgeCoAPPort)
		if err != nil {
			t.Fatal(err)
		}
		frame, err := BuildData(NetworkKey, seq, BridgeEUI, src[:5], WrapAppPayload(ack, 0x700+uint32(seq), nil))
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := ParseData(NetworkKey, frame.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		msg := ParseApp(parsed.Plaintext)
		if msg == nil || msg.CoAP == nil || msg.CoAP.Code != code {
			t.Fatalf("response code=%+v, want %s", msg, code)
		}
		e.OnFrame(frame.Bytes())
	}

	for _, accepted := range []string{"2.04"} {
		t.Run(accepted, func(t *testing.T) {
			e, _, _ := newTestEmu(t)
			if err := e.EnqueueSetpoint("camera", SetpointParams{ID: 4, Setpoint: 5, Sched: 5, Overlay: 2}); err != nil {
				t.Fatal(err)
			}
			wire, label, err := e.resolve("camera")
			if err != nil {
				t.Fatal(err)
			}
			actions, ok := e.nextDownlink(wire, label)
			if !ok || len(actions) != 1 || !actions[0].WaitResponse || !actions[0].CoAPTransaction {
				t.Fatalf("actions=%+v ok=%t, want CoAP-confirmed setpoint", actions, ok)
			}
			data, err := ParseData(NetworkKey, actions[0].Bytes)
			if err != nil {
				t.Fatal(err)
			}
			request := ParseApp(data.Plaintext).CoAP
			e.OnFrame(BuildEnhAck(data.Seq, BridgeEUI, wire, 0))
			if e.Pending("camera") != 1 {
				t.Fatal("Enhanced ACK completed setpoint")
			}

			other := []byte{0x68, 0x45, 0x23, 0x31, 0x07, 0xc5, 0x1b, 0x00}
			e.confirmByMID(other, &CoAPMessage{Type: "ACK", Code: accepted, MID: request.MID, Token: request.Token})
			if e.Pending("camera") != 1 {
				t.Fatal("wrong source completed setpoint")
			}
			e.confirmByMID(wire, &CoAPMessage{Type: "ACK", Code: accepted, MID: request.MID + 1, Token: request.Token})
			if e.Pending("camera") != 1 {
				t.Fatal("wrong MID completed setpoint")
			}
			e.confirmByMID(wire, &CoAPMessage{Type: "ACK", Code: accepted, MID: request.MID, Token: []byte{0}})
			if e.Pending("camera") != 1 {
				t.Fatal("wrong token completed setpoint")
			}
			e.confirmByMID(wire, &CoAPMessage{Type: "ACK", Code: "2.01", MID: request.MID, Token: request.Token})
			if e.Pending("camera") != 1 {
				t.Fatal("wrong 2.01 code completed setpoint")
			}
			e.confirmByMID(wire, &CoAPMessage{Type: "ACK", Code: "2.05", MID: request.MID, Token: request.Token})
			if e.Pending("camera") != 1 {
				t.Fatal("wrong 2.05 code completed setpoint")
			}
			feed(t, e, wire, accepted, request.MID, request.Token, 0x46)
			if e.Pending("camera") != 0 {
				t.Fatal("matching CoAP response did not complete setpoint")
			}
			history := e.QueueHistory("camera")
			if len(history) != 1 || history[0].Result != "delivered" || history[0].Reason != "coap-ack" {
				t.Fatalf("history=%+v, want delivered/coap-ack", history)
			}
		})
	}

	e, _, _ := newTestEmu(t)
	if err := e.EnqueueSetpoint("camera", SetpointParams{ID: 4, Setpoint: 5, Sched: 5, Overlay: 2}); err != nil {
		t.Fatal(err)
	}
	wire, label, err := e.resolve("camera")
	if err != nil {
		t.Fatal(err)
	}
	actions, ok := e.nextDownlink(wire, label)
	if !ok || len(actions) != 1 {
		t.Fatalf("actions=%+v ok=%t", actions, ok)
	}
	data, err := ParseData(NetworkKey, actions[0].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	request := ParseApp(data.Plaintext).CoAP
	feed(t, e, wire, "4.05", request.MID, request.Token, 0x47)
	if e.Pending("camera") != 0 {
		t.Fatal("matching 4.xx left setpoint pending")
	}
	history := e.QueueHistory("camera")
	if len(history) != 1 || history[0].Result != "dropped" || history[0].Reason != "coap-4.05" {
		t.Fatalf("history=%+v, want dropped/coap-4.05", history)
	}
}

// TestDropQueue: withdrawing the queue for one device leaves the other devices
// and the delivery counters alone. The cold-start harness relies on it to keep
// an undelivered attempt from riding out as a piggyback during the next one.
func TestDropQueue(t *testing.T) {
	e, _, _ := newTestEmu(t)
	if n := e.DropQueue("bagno"); n != 0 {
		t.Errorf("DropQueue on an empty queue returned %d, want 0", n)
	}
	if n := e.DropQueue("nosuchnode"); n != 0 {
		t.Errorf("DropQueue on an unknown target returned %d, want 0", n)
	}
	for _, node := range []string{"bagno", "camera"} {
		if err := e.EnqueueSetpoint(node, SetpointParams{ID: 3, Setpoint: 5.0, Sched: 5.0, Overlay: 2}); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.EnqueueIdentify("bagno", 3); err != nil {
		t.Fatal(err)
	}
	queue := e.Queue("bagno")
	if len(queue) != 2 {
		t.Fatalf("queue length=%d, want 2", len(queue))
	}
	if queue[0].Kind != "setpoint" || queue[0].Desc == "" || queue[0].MID == 0 || queue[0].Tries != 0 || queue[0].InFlight {
		t.Errorf("first queue entry = %+v, want a fresh setpoint", queue[0])
	}
	queue[0].Kind = "changed"
	if got := e.Queue("bagno")[0].Kind; got != "setpoint" {
		t.Errorf("Queue returned mutable state: kind=%q", got)
	}
	before := e.Stats()
	if n := e.DropQueue("bagno"); n != 2 {
		t.Errorf("dropped %d downlinks, want the 2 queued for bagno", n)
	}
	if n := e.Pending("bagno"); n != 0 {
		t.Errorf("bagno still holds %d downlinks", n)
	}
	if n := e.Pending("camera"); n != 1 {
		t.Errorf("camera lost its own queue (%d pending, want 1)", n)
	}
	after := e.Stats()
	if after.DownlinksDelivered != before.DownlinksDelivered || after.DownlinksDropped != before.DownlinksDropped {
		t.Errorf("withdrawing moved the delivery counters: delivered %d->%d dropped %d->%d",
			before.DownlinksDelivered, after.DownlinksDelivered,
			before.DownlinksDropped, after.DownlinksDropped)
	}
	if _, err := e.ScheduleDownlink("bagno"); err == nil {
		t.Error("scheduling after a withdrawal should fail: nothing is queued")
	}
}

func TestQueueHistoryRecordsTerminalOutcomes(t *testing.T) {
	e, _, _ := newTestEmu(t)
	wire, label, err := e.resolve("bagno")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.EnqueueSetpoint("bagno", SetpointParams{ID: 3, Setpoint: 5, Sched: 5, Overlay: 2}); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.nextDownlink(wire, label); !ok {
		t.Fatal("no downlink attempt")
	}
	q := e.queue[hexEUI(wire)][0]
	e.confirmByMID(wire, &CoAPMessage{Type: "ACK", Code: "2.04", MID: q.mid, Token: q.responseToken})

	if err := e.EnqueueIdentify("bagno", 3); err != nil {
		t.Fatal(err)
	}
	e.DropQueue("bagno")

	if err := e.EnqueueSetpoint("bagno", SetpointParams{ID: 3, Setpoint: 5, Sched: 5, Overlay: 2}); err != nil {
		t.Fatal(err)
	}
	for range maxDownlinkTries {
		if _, ok := e.nextDownlink(wire, label); !ok {
			t.Fatal("retry attempt missing")
		}
	}
	if _, ok := e.nextDownlink(wire, label); ok {
		t.Fatal("retry-limit downlink was sent")
	}

	history := e.QueueHistory("bagno")
	if len(history) != 3 {
		t.Fatalf("history length=%d, want 3", len(history))
	}
	got := [][2]string{{history[0].Result, history[0].Reason}, {history[1].Result, history[1].Reason}, {history[2].Result, history[2].Reason}}
	want := [][2]string{{"dropped", "retry-limit"}, {"withdrawn", "operator"}, {"delivered", "coap-ack"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("history outcomes=%v, want %v", got, want)
	}
	if history[0].Tries != maxDownlinkTries || history[2].Tries != 1 || history[2].Completed.IsZero() {
		t.Errorf("history=%+v, want terminal attempts and timestamps", history)
	}
	history[0].Result = "changed"
	if got := e.QueueHistory("bagno")[0].Result; got != "dropped" {
		t.Errorf("QueueHistory returned mutable state: result=%q", got)
	}
}

func TestQueueHistoryRetainsNewest32Outcomes(t *testing.T) {
	e, _, _ := newTestEmu(t)
	wire, _, err := e.resolve("bagno")
	if err != nil {
		t.Fatal(err)
	}
	key := hexEUI(wire)
	for i := 0; i < maxDownlinkHistory+1; i++ {
		e.recordDownlinkOutcome(key, &queuedDownlink{kind: "setpoint", desc: strconv.Itoa(i)}, "withdrawn", "operator")
	}
	history := e.QueueHistory("bagno")
	if len(history) != maxDownlinkHistory {
		t.Fatalf("history length=%d, want %d", len(history), maxDownlinkHistory)
	}
	if got := history[0].Desc; got != strconv.Itoa(maxDownlinkHistory) {
		t.Errorf("newest desc=%q, want %d", got, maxDownlinkHistory)
	}
	if got := history[len(history)-1].Desc; got != "1" {
		t.Errorf("oldest retained desc=%q, want 1", got)
	}
}

func TestQueueZoneHopRecordsSupersededOutcome(t *testing.T) {
	e, _, _ := newTestEmu(t)
	wire, _, err := e.resolve("bagno")
	if err != nil {
		t.Fatal(err)
	}
	origin := mustHex(NodeByAny("0a01").WireLE)
	older := &CoAPMessage{Type: "CON", Code: "0.03", MID: 0x4411, Path: "z/s", Query: "lid=3"}
	newer := &CoAPMessage{Type: "CON", Code: "0.03", MID: 0x4412, Path: "z/s", Query: "lid=3"}
	e.queueZoneHop(wire, older, []byte{1}, "zone old", origin)
	e.queueZoneHop(wire, newer, []byte{2}, "zone new", origin)

	queue := e.Queue("bagno")
	if len(queue) != 1 || queue[0].Desc != "zone new" {
		t.Fatalf("queue=%+v, want only the newer zone hop", queue)
	}
	history := e.QueueHistory("bagno")
	if len(history) != 1 || history[0].Result != "superseded" || history[0].Reason != "newer-zone-state" {
		t.Fatalf("history=%+v, want superseded newer-zone-state", history)
	}
}

func queueDescs(e *EmuState, target string) []string {
	var out []string
	for _, d := range e.Queue(target) {
		out = append(out, d.Desc)
	}
	return out
}

// TestZoneRelayCollapsesRetransmissions: a CON the origin retransmits under the
// same MID rides the queued copy, also while that copy is on the air.
func TestZoneRelayCollapsesRetransmissions(t *testing.T) {
	sender := mustHex(NodeByAny("0a01").WireLE)
	peer := mustHex(NodeByAny("a205").WireLE)
	target := hex.EncodeToString(peer)
	e, log, _ := newTestEmu(t)
	get := CoAPReq{
		Type: "CON", Code: "0.01", MID: 0x5501, Token: []byte{0x55},
		Path: []string{"z", "p"}, Query: []string{"lid=5"}, Options: []CoAPOption{optOctets},
	}
	raw := zoneUplink(t, sender, peer, get)
	e.OnFrame(raw)
	e.OnFrame(reframe(t, raw, 0x23, 801))
	if n := e.Pending(target); n != 1 {
		t.Fatalf("pending=%d, want 1:\n%s", n, log.Summary())
	}
	if _, ok, err := e.NextQueuedDownlink(target); err != nil || !ok {
		t.Fatalf("dispatch ok=%t err=%v", ok, err)
	}
	e.OnFrame(reframe(t, raw, 0x24, 802))
	queue := e.Queue(target)
	if len(queue) != 1 || !queue[0].InFlight {
		t.Fatalf("queue=%+v, want the one copy in flight", queue)
	}
	if got := e.Stats().ZoneRelaysCollapsed; got != 2 {
		t.Errorf("ZoneRelaysCollapsed=%d, want 2", got)
	}
}

// TestZoneHopRouteKeysByOriginEUI: two devices that share a label push the same
// resource; neither push replaces the other.
func TestZoneHopRouteKeysByOriginEUI(t *testing.T) {
	e, _, _ := newTestEmu(t)
	wire, _, err := e.resolve("bagno")
	if err != nil {
		t.Fatal(err)
	}
	first := mustHex(NodeByAny("0a01").WireLE)
	second := append([]byte{0x11, 0x22, 0x33, 0x44, 0x55}, EUISuffix...)
	e.queueZoneHop(wire, &CoAPMessage{Type: "CON", Code: "0.03", MID: 0x0101, Path: "z/p", Query: "lid=5"}, []byte{1}, "first", first)
	e.queueZoneHop(wire, &CoAPMessage{Type: "CON", Code: "0.03", MID: 0x0201, Path: "z/p", Query: "lid=5"}, []byte{2}, "second", second)
	if got := queueDescs(e, "bagno"); len(got) != 2 {
		t.Fatalf("queue=%v, want both pushes", got)
	}
}

// TestZoneHopNotificationSupersedes: a newer NON observe notification replaces a
// waiting one of the same observation. A CON notification and an ACK never do.
func TestZoneHopNotificationSupersedes(t *testing.T) {
	e, _, _ := newTestEmu(t)
	wire, _, err := e.resolve("bagno")
	if err != nil {
		t.Fatal(err)
	}
	origin := mustHex(NodeByAny("0a01").WireLE)
	notify := func(typ string, mid uint16, token byte) *CoAPMessage {
		return &CoAPMessage{Type: typ, Code: "2.05", MID: mid, Token: []byte{token},
			Options: []CoAPOption{{Num: 6, Val: []byte{byte(mid)}}}}
	}
	e.queueZoneHop(wire, notify("NON", 0x0001, 0x0a), []byte{1}, "non-1", origin)
	e.queueZoneHop(wire, notify("NON", 0x0002, 0x0b), []byte{2}, "other-token", origin)
	e.queueZoneHop(wire, notify("NON", 0x0003, 0x0a), []byte{3}, "non-3", origin)
	e.queueZoneHop(wire, notify("CON", 0x0004, 0x0c), []byte{4}, "con-4", origin)
	e.queueZoneHop(wire, notify("CON", 0x0005, 0x0c), []byte{5}, "con-5", origin)
	want := []string{"other-token", "non-3", "con-4", "con-5"}
	if got := queueDescs(e, "bagno"); !slices.Equal(got, want) {
		t.Fatalf("queue=%v, want %v", got, want)
	}
}

// TestZoneHopAckGoesFirst: an ACK closing the target's CON passes the waiting
// datagrams, after the one on the air and after the ACKs already waiting.
func TestZoneHopAckGoesFirst(t *testing.T) {
	e, _, _ := newTestEmu(t)
	wire, _, err := e.resolve("bagno")
	if err != nil {
		t.Fatal(err)
	}
	origin := mustHex(NodeByAny("0a01").WireLE)
	e.queueZoneHop(wire, &CoAPMessage{Type: "CON", Code: "0.03", MID: 0x0101, Path: "z/p", Query: "lid=5"}, []byte{1}, "push-1", origin)
	if _, ok, err := e.NextQueuedDownlink("bagno"); err != nil || !ok {
		t.Fatalf("dispatch ok=%t err=%v", ok, err)
	}
	e.queueZoneHop(wire, &CoAPMessage{Type: "CON", Code: "0.03", MID: 0x0102, Path: "z/p", Query: "lid=8"}, []byte{2}, "push-2", origin)
	e.queueZoneHop(wire, &CoAPMessage{Type: "ACK", Code: "2.05", MID: 0x0c01}, []byte{3}, "ack-1", origin)
	e.queueZoneHop(wire, &CoAPMessage{Type: "ACK", Code: "2.04", MID: 0x0c02}, []byte{4}, "ack-2", origin)
	want := []string{"push-1", "ack-1", "ack-2", "push-2"}
	if got := queueDescs(e, "bagno"); !slices.Equal(got, want) {
		t.Fatalf("queue=%v, want %v", got, want)
	}
}

// TestZoneHopBacklogIsBounded: past maxZoneHopsPerTarget the oldest waiting
// datagram that is not an ACK is dropped and recorded.
func TestZoneHopBacklogIsBounded(t *testing.T) {
	e, _, _ := newTestEmu(t)
	wire, _, err := e.resolve("bagno")
	if err != nil {
		t.Fatal(err)
	}
	origin := mustHex(NodeByAny("0a01").WireLE)
	e.queueZoneHop(wire, &CoAPMessage{Type: "ACK", Code: "2.05", MID: 0x0c00}, []byte{0}, "ack", origin)
	for i := range maxZoneHopsPerTarget + 2 {
		e.queueZoneHop(wire, &CoAPMessage{Type: "CON", Code: "0.03", MID: uint16(0x0100 + i), Path: "z/p", Query: "lid=" + strconv.Itoa(i)},
			[]byte{byte(i)}, "push-"+strconv.Itoa(i), origin)
	}
	got := queueDescs(e, "bagno")
	if len(got) != maxZoneHopsPerTarget || got[0] != "ack" || got[1] != "push-3" {
		t.Fatalf("queue=%v, want the ACK then push-3 onwards", got)
	}
	history := e.QueueHistory("bagno")
	if len(history) != 3 || history[0].Result != "dropped" || history[0].Reason != "zone-backlog" || history[2].Desc != "push-0" {
		t.Fatalf("history=%+v, want push-0..2 dropped for zone-backlog", history)
	}
	actions, ok, err := e.NextQueuedDownlink("bagno")
	if err != nil || !ok || len(actions) != 1 || !actions[0].ZoneRelay {
		t.Fatalf("actions=%+v ok=%t err=%v, want one zone relay attempt", actions, ok, err)
	}
}

// TestScheduleDownlink: the cold-start list is a wake-up train followed by the
// Data frame, all sharing one sequence number.
func TestScheduleDownlink(t *testing.T) {
	e, _, _ := newTestEmu(t)
	if _, err := e.ScheduleDownlink("bagno"); err == nil {
		t.Error("scheduling with an empty queue should fail")
	}
	if _, err := e.ScheduleDownlink("nosuchnode"); err == nil {
		t.Error("scheduling for an unknown target should fail")
	}
	if err := e.EnqueueSetpoint("bagno", SetpointParams{ID: 3, Setpoint: 19.0, Sched: 19.0, Overlay: 2}); err != nil {
		t.Fatal(err)
	}
	actions, err := e.ScheduleDownlink("bagno")
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != wakeUpTrainLen+1 {
		t.Fatalf("got %d actions, want %d wake-ups plus the Data frame", len(actions), wakeUpTrainLen+1)
	}

	bagno := NodeByAny("bagno")
	prev := uint16(0xffff)
	for i, a := range actions[:len(actions)-1] {
		if a.Kind != ActWakeUp {
			t.Fatalf("action %d is %v, want ActWakeUp", i, a.Kind)
		}
		w, err := ParseWakeUp(a.Bytes)
		if err != nil {
			t.Fatalf("wake-up %d does not parse: %v", i, err)
		}
		if w.DstShort != bagno.Short {
			t.Errorf("wake-up %d dst=%s, want %s", i, w.DstShort, bagno.Short)
		}
		if w.PANID != PANID {
			t.Errorf("wake-up %d pan=%04x, want %04x", i, w.PANID, PANID)
		}
		if w.RendezvousTime > prev {
			t.Errorf("wake-up %d rendezvous=%d rose above %d", i, w.RendezvousTime, prev)
		}
		prev = w.RendezvousTime
	}

	last := actions[len(actions)-1]
	if last.Kind != ActData {
		t.Fatalf("last action is %v, want ActData", last.Kind)
	}
	df, err := ParseData(NetworkKey, last.Bytes)
	if err != nil {
		t.Fatalf("cold-start Data does not decrypt: %v", err)
	}
	if hex.EncodeToString(df.DstEUI) != bagno.WireLE {
		t.Errorf("Data dst=%x, want %s", df.DstEUI, bagno.WireLE)
	}
	w, _ := ParseWakeUp(actions[0].Bytes)
	if w.Seq != df.Seq {
		t.Errorf("train seq=%02x, Data seq=%02x: one transaction should share it", w.Seq, df.Seq)
	}
}

// TestMICFailNotAcked: a corrupted Data frame is logged and never acknowledged.
// A MAC ACK for a frame we could not authenticate would let a forged frame pass.
func TestMICFailNotAcked(t *testing.T) {
	raw := append([]byte{}, loadGolden(t)[0]...)
	raw[headerLen] ^= 0xff // first ciphertext byte

	e, log, _ := newTestEmu(t)
	if acts := e.OnFrame(raw); acts != nil {
		t.Fatalf("MIC failure produced %v, want no action", reasons(acts))
	}
	if n := log.Counts()[rsnMICFail]; n != 1 {
		t.Errorf("mic-fail count=%d, want 1:\n%s", n, log.Summary())
	}
	if st := e.Stats(); st.MICFails != 1 || st.EnhAcks != 0 {
		t.Errorf("stats micFails=%d enhAcks=%d, want 1 and 0", st.MICFails, st.EnhAcks)
	}
	if len(e.Nodes()) != 0 {
		t.Error("a frame that failed the MIC must not create node state")
	}
}

// TestGoldenSweep replays the whole capture through OnFrame: nothing panics,
// every device-to-bridge frame is answered, and the resource catalog covers
// every CoAP path the capture carries.
func TestGoldenSweep(t *testing.T) {
	e, log, _ := newTestEmu(t)
	frames := loadGolden(t)
	uplinks, acks := 0, 0
	for _, raw := range frames {
		f, err := ParseData(NetworkKey, raw)
		if err != nil {
			t.Fatal(err)
		}
		up := !bytes.Equal(f.SrcPrefix, BridgeEUI[:5])
		if up {
			uplinks++
		}
		for _, a := range e.OnFrame(raw) {
			if a.Kind == ActEnhAck {
				acks++
			}
		}
	}
	if acks != uplinks {
		t.Errorf("acked %d of %d uplinks", acks, uplinks)
	}
	counts := log.Counts()
	if n := countPrefix(counts, rsnUnknownRes); n != 0 {
		t.Errorf("%d unknown CoAP resources; extend knownResources:\n%s", n, log.Summary())
	}
	for _, kind := range []string{rsnBuildFail, rsnUnclassified, rsnMICFail, rsnAppParse, rsnUnhandledMethod} {
		if n := countPrefix(counts, kind); n != 0 {
			t.Errorf("unexpected %q x%d:\n%s", kind, n, log.Summary())
		}
	}
	// The captured downlinks are the real bridge talking, which is exactly the
	// sole-coordinator violation the emulator must flag and never answer.
	if n := counts[rsnBridgeSrc]; n != len(frames)-uplinks {
		t.Errorf("bridge-sourced frames logged=%d, want %d", n, len(frames)-uplinks)
	}
	// Zone traffic is answerable now, so nothing should be turned away for its
	// addressing mode. What stays unanswered is only what is not ours to answer.
	if n := countPrefix(counts, rsnUnknownAddrMode); n != 0 {
		t.Errorf("unexpected %q x%d:\n%s", rsnUnknownAddrMode, n, log.Summary())
	}
	t.Logf("swept %d frames (%d uplinks), unhandled summary:\n%s", len(frames), uplinks, log.Summary())
}

// TestOnFrameAnswersZoneUplink: a lid= uplink addressed to the bridge is answered
// with a context-compressed CoAP ACK whose UDP checksum verifies under
// Context0Prefix. That path used to be received and logged only.
func TestOnFrameAnswersZoneUplink(t *testing.T) {
	raw, f, app := firstUplink(t, func(fr *Frame, a *AppPayload) bool {
		if a.CoAP == nil || a.CoAP.Type != "CON" || !a.CoAP.Contexted() {
			return false
		}
		if !strings.HasPrefix(a.CoAP.Query, "lid=") {
			return false
		}
		dst := a.CoAP.Dst.IPv6(fr.DstEUI)
		return dst != nil && bytes.Equal(dst[8:], modifiedEUI64(BridgeEUI))
	})

	e, log, _ := newTestEmu(t)
	act, ok := findAction(e.OnFrame(raw), ActData, "coap-ack")
	if !ok {
		t.Fatalf("no CoAP ACK for %s; unhandled:\n%s", app.CoAP.URI(), log.Summary())
	}

	back, err := ParseData(NetworkKey, act.Bytes)
	if err != nil {
		t.Fatalf("the answer does not decrypt: %v", err)
	}
	if !bytes.Equal(back.DstEUI, f.SrcEUI()) {
		t.Errorf("answer addressed to %x, want the sender %x", back.DstEUI, f.SrcEUI())
	}
	m, err := ParseCoAP(back.Plaintext[8:])
	if err != nil {
		t.Fatalf("the answer does not parse: %v", err)
	}
	if !m.Contexted() {
		t.Error("the answer left the zone addressing mode")
	}
	if m.Type != "ACK" || m.MID != app.CoAP.MID {
		t.Errorf("answer is %s mid=%04x, want ACK mid=%04x", m.Type, m.MID, app.CoAP.MID)
	}
	// The receiving device drops a bad checksum, so this is the assertion that
	// makes the whole zone downlink path real.
	ck, okCk := m.RecomputeChecksum(back.SrcEUI(), back.DstEUI)
	if !okCk || ck != m.Checksum {
		t.Errorf("answer checksum %04x does not verify (recomputed %04x, ok=%v)", m.Checksum, ck, okCk)
	}
	if n := countPrefix(log.Counts(), rsnRelayedPath); n != 0 {
		t.Errorf("a uplink addressed to us was treated as relayed:\n%s", log.Summary())
	}
}

// TestZoneUplinkRelayed: a zone uplink whose IPv6 destination is another device
// is forwarded to that device, never answered - answering as ourselves would
// construct a relay reply for the peer. The re-emitted frame is the bridge's own relay of
// the same datagram, byte for byte, as the capture recorded it.
func TestZoneUplinkRelayed(t *testing.T) {
	pair := zoneRelayPairs(t)[0]
	up, err := ParseData(NetworkKey, pair[0])
	if err != nil {
		t.Fatal(err)
	}
	down, err := ParseData(NetworkKey, pair[1])
	if err != nil {
		t.Fatal(err)
	}
	m := ParseApp(up.Plaintext).CoAP
	peer := hex.EncodeToString(down.DstEUI)

	e, log, _ := newTestEmu(t)
	actions := e.OnFrame(pair[0])
	if _, ok := findAction(actions, ActData, "coap-ack"); ok {
		t.Errorf("answered %s, which is addressed to %x not to us", m.URI(), m.Dst.IID)
	}
	if _, ok := findAction(actions, ActEnhAck, ""); !ok {
		t.Error("the MAC Enhanced ACK is still due: we did receive the frame")
	}
	if n := e.Pending(peer); n != 1 {
		t.Fatalf("pending for the addressed peer %s = %d, want 1:\n%s", peer, n, log.Summary())
	}
	if st := e.Stats(); st.ZoneRelays != 1 {
		t.Errorf("ZoneRelays=%d, want 1", st.ZoneRelays)
	}

	out, err := e.ScheduleDownlink(peer)
	if err != nil {
		t.Fatal(err)
	}
	relay := out[len(out)-1]
	rf, err := ParseData(NetworkKey, relay.Bytes)
	if err != nil {
		t.Fatalf("the relay does not decrypt: %v", err)
	}
	if !bytes.Equal(rf.DstEUI, down.DstEUI) {
		t.Errorf("relay addressed to %x, want the peer %x", rf.DstEUI, down.DstEUI)
	}
	got := rf.Plaintext[8 : len(rf.Plaintext)-2]
	want := down.Plaintext[8 : len(down.Plaintext)-2]
	if !bytes.Equal(got, want) {
		t.Errorf("relayed packet\n got =%s\n want=%s", hex.EncodeToString(got), hex.EncodeToString(want))
	}
}

// TestZoneRelayForwardsRequestVerbatim: a relayed request keeps the CoAP bytes
// its author sent. The commercial bridge never adds option 2048 to a relayed
// request (1444 relayed pairs in capture-app-20260907 and ota-watch-20260923/24,
// every one identical on both hops), also for an author that echoes a session
// id on its own uplinks.
func TestZoneRelayForwardsRequestVerbatim(t *testing.T) {
	sender := mustHex(NodeByAny("0a01").WireLE)
	peer := mustHex(NodeByAny("a205").WireLE)

	for _, echo := range [][]byte{nil, mustHex("5e55000000000001")} {
		e, log, _ := newTestEmu(t)
		if echo != nil {
			e.OnFrame(deviceUplink(t, sender, CoAPReq{
				Type: "CON", Code: "0.03", MID: 0x3200, Token: []byte{0x12},
				Path: []string{"d", "config"}, Query: []string{"id=10"},
				Options: []CoAPOption{optOctets, {Num: coapSessionOption, Val: echo}},
				Payload: TLV{Tag: "0140", Val: []byte{0x00, 0x00}}.Encode(),
			}))
		}
		up := zoneUplink(t, sender, peer, CoAPReq{
			Type: "CON", Code: "0.03", MID: 0x3101, Token: []byte{0x11},
			Path: []string{"z", "s"}, Query: []string{"lid=10"}, Options: []CoAPOption{optOctets},
			Payload: TLV{Tag: "40a0", Val: []byte{56}}.Encode(),
		})
		uf, err := ParseData(NetworkKey, up)
		if err != nil {
			t.Fatal(err)
		}
		sent := ParseApp(uf.Plaintext).CoAP
		e.OnFrame(up)
		out, err := e.ScheduleDownlink(hex.EncodeToString(peer))
		if err != nil {
			t.Fatalf("%v; unhandled:\n%s", err, log.Summary())
		}
		f, err := ParseData(NetworkKey, out[len(out)-1].Bytes)
		if err != nil {
			t.Fatal(err)
		}
		relayed := ParseApp(f.Plaintext).CoAP
		if !bytes.Equal(relayed.Raw, sent.Raw) {
			t.Errorf("echo=%x: relayed CoAP\n got =%x\n want=%x", echo, relayed.Raw, sent.Raw)
		}
	}
}

// TestZoneRelayAnswerCarriesNoSession: a relayed 2.xx keeps the shape the capture
// recorded, which carries no session id.
func TestZoneRelayAnswerCarriesNoSession(t *testing.T) {
	sender := mustHex(NodeByAny("0a01").WireLE)
	peer := mustHex(NodeByAny("a205").WireLE)
	e, log, _ := newTestEmu(t)
	e.OnFrame(zoneUplink(t, sender, peer, CoAPReq{
		Type: "NON", Code: "2.05", MID: 0x1e04, Token: []byte{0x1e},
		Payload: TLV{Tag: "40a0", Val: []byte{56}}.Encode(),
	}))
	out, err := e.ScheduleDownlink(hex.EncodeToString(peer))
	if err != nil {
		t.Fatalf("%v; unhandled:\n%s", err, log.Summary())
	}
	f, err := ParseData(NetworkKey, out[len(out)-1].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if o, ok := ParseApp(f.Plaintext).CoAP.Option(coapSessionOption); ok {
		t.Errorf("relayed answer carries option 2048 = %x", o.Val)
	}
}

// optionValue returns an option value, nil when the message carries none.
func optionValue(m *CoAPMessage, num int) []byte {
	o, ok := m.Option(num)
	if !ok {
		return nil
	}
	return o.Val
}

// This exercises only the builder retry identity. The daemon test covers the
// completion path that schedules the retry after CSLTX_NOACK.
func TestZoneRelayResponseRetryBuilderKeepsCapturedMACIdentity(t *testing.T) {
	now := time.Unix(100, 0)
	e := NewEmu(EmuConfig{
		TxCounter: 100000,
		CSLPhase:  0x1234,
		Now:       func() time.Time { return now },
	}, NewUnhandledLogger(io.Discard))
	sender := mustHex(NodeByAny("0a01").WireLE)
	peer := mustHex(NodeByAny("a205").WireLE)
	target := hex.EncodeToString(peer)
	if actions := e.OnFrame(zoneUplink(t, sender, peer, CoAPReq{
		Type: "NON", Code: "2.05", MID: 0x1e04, Token: []byte{0x1e},
		Payload: TLV{Tag: "40a0", Val: []byte{56}}.Encode(),
	})); len(actions) == 0 {
		t.Fatal("zone response was not accepted")
	}
	first, ok, err := e.NextQueuedDownlink(target)
	if err != nil || !ok || len(first) != 1 || !first[0].ResponseRetry {
		t.Fatalf("first=%+v ok=%t err=%v", first, ok, err)
	}
	now = now.Add(time.Second)
	second, ok, err := e.NextQueuedDownlink(target)
	if err != nil || !ok || len(second) != 1 || !second[0].ResponseRetry {
		t.Fatalf("second=%+v ok=%t err=%v", second, ok, err)
	}
	firstData, err := ParseData(NetworkKey, first[0].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	secondData, err := ParseData(NetworkKey, second[0].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	firstCoAP := ParseApp(firstData.Plaintext).CoAP
	secondCoAP := ParseApp(secondData.Plaintext).CoAP
	if firstCoAP == nil || secondCoAP == nil || firstCoAP.MID != 0x1e04 || secondCoAP.MID != firstCoAP.MID || !bytes.Equal(secondCoAP.Raw, firstCoAP.Raw) {
		t.Fatalf("CoAP identity changed: first=%+v second=%+v", firstCoAP, secondCoAP)
	}
	if secondData.Seq != firstData.Seq {
		t.Fatalf("MAC seq=%02x, want captured retry seq=%02x", secondData.Seq, firstData.Seq)
	}
	if bytes.Equal(secondData.Plaintext[4:8], firstData.Plaintext[4:8]) {
		t.Fatal("relay retry reused the anti-replay counter")
	}
	e.confirmBySeq(peer, secondData.Seq)
	if err := e.EnqueueChildLock(target, 5, true); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	next, ok, err := e.NextQueuedDownlink(target)
	if err != nil || !ok || len(next) != 1 {
		t.Fatalf("next=%+v ok=%t err=%v", next, ok, err)
	}
	nextData, err := ParseData(NetworkKey, next[0].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if nextData.Seq != firstData.Seq+1 {
		t.Fatalf("next MAC seq=%02x, want %02x", nextData.Seq, firstData.Seq+1)
	}
}

func TestNoAutoQueueSuppressesZoneQueues(t *testing.T) {
	t.Run("relay", func(t *testing.T) {
		pair := zoneRelayPairs(t)[0]
		down, err := ParseData(NetworkKey, pair[1])
		if err != nil {
			t.Fatal(err)
		}
		e := NewEmu(EmuConfig{TxCounter: 100000, NoAutoQueue: true}, nil)
		actions := e.OnFrame(pair[0])
		if _, ok := findAction(actions, ActEnhAck, ""); !ok {
			t.Fatal("no-auto-queue suppressed the MAC Enhanced ACK")
		}
		if n := e.Pending(hex.EncodeToString(down.DstEUI)); n != 0 {
			t.Fatalf("pending relay=%d, want 0", n)
		}
		if st := e.Stats(); st.ZoneRelays != 0 {
			t.Fatalf("ZoneRelays=%d, want 0", st.ZoneRelays)
		}
	})

	t.Run("fanout", func(t *testing.T) {
		sender := mustHex(NodeByAny("a803").WireLE)
		members := []string{"a70a", "a205"}
		e := NewEmu(EmuConfig{
			TxCounter: 100000, NoAutoQueue: true,
			Zones: map[int][]string{8: append(members, "a803")},
		}, nil)
		actions := e.OnFrame(zoneUplink(t, sender, BridgeEUI, CoAPReq{
			Type: "CON", Code: "0.03", MID: 0x9d06, Token: []byte{0x12},
			Path: []string{"z", "s"}, Query: []string{"lid=8"}, Options: []CoAPOption{optOctets},
			Payload: TLV{Tag: "40a0", Val: []byte{60}}.Encode(),
		}))
		if m := servedAnswer(t, actions, 0x9d06); m.Type != "ACK" {
			t.Fatalf("CoAP response=%s, want ACK", m.Type)
		}
		for _, member := range members {
			if n := e.Pending(member); n != 0 {
				t.Fatalf("pending fanout to %s=%d, want 0", member, n)
			}
		}
		if st := e.Stats(); st.ZoneFanouts != 0 {
			t.Fatalf("ZoneFanouts=%d, want 0", st.ZoneFanouts)
		}
	})

	t.Run("proxy", func(t *testing.T) {
		ru := mustHex(NodeByAny("RU1000000001").WireLE)
		members := []string{"a70a", "a803"}
		e := NewEmu(EmuConfig{
			TxCounter: 100000, NoAutoQueue: true, ZoneGetProxy: true,
			Zones: map[int][]string{8: {"a205", "a70a", "a803"}},
		}, nil)
		actions := e.OnFrame(zoneUplink(t, ru, BridgeEUI, CoAPReq{
			Type: "CON", Code: "0.01", MID: 0x7a02, Token: []byte{0x78},
			Path: []string{"z", "s"}, Query: []string{"lid=8"},
		}))
		if m := servedAnswer(t, actions, 0x7a02); m.Type != "ACK" {
			t.Fatalf("CoAP response=%s, want ACK", m.Type)
		}
		for _, member := range members {
			if n := e.Pending(member); n != 0 {
				t.Fatalf("pending proxy to %s=%d, want 0", member, n)
			}
		}
		if st := e.Stats(); st.ZoneProxies != 0 {
			t.Fatalf("ZoneProxies=%d, want 0", st.ZoneProxies)
		}
	})
}

// TestZoneDirectoryLearned: the zone directory is declared in the config and
// learned from the traffic. A device that publishes on "lid=N" is a member of N,
// and so is the peer it addresses.
func TestZoneDirectoryLearned(t *testing.T) {
	camera := mustHex(NodeByAny("camera").WireLE)
	ru := mustHex(NodeByAny("RU1000000001").WireLE)

	// Declared by short address: a room name resolves to one device, and the two
	// cucina valves share it.
	e := NewEmu(EmuConfig{TxCounter: 100000, Zones: map[int][]string{5: {"0a06", "0a01"}}}, nil)
	if got := e.ZoneMembers(5); !reflect.DeepEqual(got, []string{"0a01", "0a06"}) {
		t.Errorf("declared zone 5 = %v, want [0a01 0a06]", got)
	}
	if got := e.ZoneMembers(10); len(got) != 0 {
		t.Errorf("zone 10 = %v before any traffic, want empty", got)
	}
	if err := e.AddZoneMember(10, "nosuchnode"); err == nil {
		t.Error("an unresolvable member should be refused")
	}

	// A zone publish from the camera, addressed to the wall thermostat: both are
	// members of zone 10, and the bridge itself never is.
	e.OnFrame(zoneUplink(t, camera, ru, CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x5101, Token: []byte{2},
		Path: []string{"z", "s"}, Query: []string{"lid=10"}, Options: []CoAPOption{optOctets},
		Payload: TLV{Tag: "6280", Val: tempC(21.0)}.Encode(),
	}))
	if got := e.ZoneMembers(10); !reflect.DeepEqual(got, []string{"a205", "a602"}) {
		t.Errorf("learned zone 10 = %v, want [a205 a602]", got)
	}
	if _, ok := e.ZoneDirectory()[5]; !ok {
		t.Errorf("directory = %v, want the declared zone 5 too", e.ZoneDirectory())
	}
}

// TestZoneFanOut: a zone PUT addressed to the bridge is acknowledged to its
// sender and re-emitted to every other member of the zone, each carrying the
// original Message-ID, token and payload.
func TestZoneFanOut(t *testing.T) {
	sender := mustHex(NodeByAny("a803").WireLE)
	members := []string{"a70a", "a205"}
	body := concat(
		TLV{Tag: "6280", Val: tempC(21.0)}.Encode(),
		TLV{Tag: "40a0", Val: []byte{60}}.Encode(),
	)

	e := NewEmu(EmuConfig{TxCounter: 100000, Zones: map[int][]string{8: append(members, "a803")}}, nil)
	actions := e.OnFrame(zoneUplink(t, sender, BridgeEUI, CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x9d04, Token: []byte{0x11},
		Path: []string{"z", "s"}, Query: []string{"lid=8"}, Options: []CoAPOption{optOctets},
		Payload: body,
	}))

	// The sender still gets its ACK, on the zone addressing mode.
	ack := servedAnswer(t, actions, 0x9d04)
	if ack.Type != "ACK" || !ack.Contexted() {
		t.Errorf("sender answered with %s (contexted=%v), want a zone ACK", ack.Type, ack.Contexted())
	}
	if st := e.Stats(); st.ZoneFanouts != len(members) {
		t.Errorf("ZoneFanouts=%d, want %d", st.ZoneFanouts, len(members))
	}
	if n := e.Pending(hex.EncodeToString(sender)); n != 0 {
		t.Errorf("the sender got %d fanned-out copies of its own frame, want 0", n)
	}

	for _, member := range members {
		if n := e.Pending(member); n != 1 {
			t.Fatalf("pending for %s = %d, want 1", member, n)
		}
		out, err := e.ScheduleDownlink(member)
		if err != nil {
			t.Fatal(err)
		}
		df, err := ParseData(NetworkKey, out[len(out)-1].Bytes)
		if err != nil {
			t.Fatalf("%s: fan-out does not decrypt: %v", member, err)
		}
		if hex.EncodeToString(df.DstEUI) != NodeByAny(member).WireLE {
			t.Errorf("%s: fan-out addressed to %x", member, df.DstEUI)
		}
		fm, err := ParseCoAP(df.Plaintext[8:])
		if err != nil {
			t.Fatalf("%s: fan-out does not parse: %v", member, err)
		}
		if fm.MID != 0x9d04 || !bytes.Equal(fm.Token, []byte{0x11}) {
			t.Errorf("%s: mid=%04x token=%x, want the sender's 9d04/11", member, fm.MID, fm.Token)
		}
		if fm.URI() != "z/s?lid=8" || !bytes.Equal(fm.Payload, body) {
			t.Errorf("%s: %s payload=%x, want z/s?lid=8 %x", member, fm.URI(), fm.Payload, body)
		}
		// The publishing device stays the IPv6 source: the receiver has to see
		// whose zone state this is, and the checksum must verify against it.
		if !bytes.Equal(wireFromIID(fm.Src.IID), sender) {
			t.Errorf("%s: source %x, want the publisher %x", member, fm.Src.IID, sender)
		}
		if ck, ok := fm.RecomputeChecksum(sender, df.DstEUI); !ok || ck != fm.Checksum {
			t.Errorf("%s: checksum %04x does not verify (recomputed %04x, ok=%v)", member, fm.Checksum, ck, ok)
		}
	}

	// The gate turns it off without touching anything else.
	off := NewEmu(EmuConfig{TxCounter: 100000, NoZoneFanOut: true, Zones: map[int][]string{8: append(members, "a803")}}, nil)
	off.OnFrame(zoneUplink(t, sender, BridgeEUI, CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x9d05, Token: []byte{0x11},
		Path: []string{"z", "s"}, Query: []string{"lid=8"}, Payload: body,
	}))
	if n := off.Pending("a70a"); n != 0 {
		t.Errorf("NoZoneFanOut still queued %d frames", n)
	}
}

// TestZoneGetServedFromCache: the state a zone published is answered back on a
// GET of the same resource. Without it the wall thermostat's poll of a zone
// returns an empty 2.05 and no demand ever reaches the boiler.
func TestZoneGetServedFromCache(t *testing.T) {
	valve := mustHex(NodeByAny("a304").WireLE)
	ru := mustHex(NodeByAny("RU1000000001").WireLE)
	body := concat(TLV{Tag: "40a0", Val: []byte{45}}.Encode(), TLV{Tag: "6240", Val: []byte{2}}.Encode())

	e := NewEmu(EmuConfig{TxCounter: 100000, NoZoneFanOut: true}, nil)
	e.OnFrame(zoneUplink(t, valve, BridgeEUI, CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x1b40, Token: []byte{3},
		Path: []string{"z", "p"}, Query: []string{"lid=2"}, Options: []CoAPOption{optOctets},
		Payload: body,
	}))
	e.OnFrame(zoneUplink(t, valve, BridgeEUI, CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x1b43, Token: []byte{6},
		Path: []string{"z", "p"}, Query: []string{"lid=2"}, Options: []CoAPOption{optOctets},
		Payload: TLV{Tag: "6240", Val: []byte{0}}.Encode(),
	}))
	body = concat(TLV{Tag: "40a0", Val: []byte{45}}.Encode(), TLV{Tag: "6240", Val: []byte{0}}.Encode())

	get := zoneUplink(t, ru, BridgeEUI, CoAPReq{
		Type: "CON", Code: "0.01", MID: 0x1b41, Token: []byte{4},
		Path: []string{"z", "p"}, Query: []string{"lid=2"},
	})
	m := servedAnswer(t, e.OnFrame(get), 0x1b41)
	if m.Code != "2.05" {
		t.Errorf("code = %s, want 2.05", m.Code)
	}
	if !bytes.Equal(m.Payload, body) {
		t.Errorf("payload = %x, want the zone's last publish %x", m.Payload, body)
	}

	// A zone we hold nothing for still gets the bare answer, not a stale one.
	other := zoneUplink(t, ru, BridgeEUI, CoAPReq{
		Type: "CON", Code: "0.01", MID: 0x1b42, Token: []byte{5},
		Path: []string{"z", "p"}, Query: []string{"lid=3"},
	})
	if m := servedAnswer(t, e.OnFrame(other), 0x1b42); len(m.Payload) != 0 {
		t.Errorf("unknown zone answered %x, want an empty body", m.Payload)
	}
}

// TestZoneMeasurePutAnswer: the captured bridge answers the z/p copy a device
// addresses to it with an empty 2.04. The 2.05 62a0=00 in the capture is the
// zone member's own answer to the copy addressed to it, which the bridge only
// relays; a 62a0 with bit0 set would zero the valve's demand.
func TestZoneMeasurePutAnswer(t *testing.T) {
	valve := mustHex(NodeByAny("a304").WireLE)
	e := NewEmu(EmuConfig{TxCounter: 100000, NoZoneFanOut: true}, nil)
	put := zoneUplink(t, valve, BridgeEUI, CoAPReq{
		Type: "CON", Code: "0.03", MID: 0xaea4, Token: []byte{},
		Path: []string{"z", "p"}, Query: []string{"lid=5"},
		Options: []CoAPOption{optOctets, optZoneMeasureAge, {Num: coapSessionOption, Val: mustHex("0011223344556677")}},
		Payload: concat(TLV{Tag: "4060", Val: []byte{0x0a, 0x7a}}.Encode(), TLV{Tag: "40a0", Val: []byte{0}}.Encode()),
	})
	m := servedAnswer(t, e.OnFrame(put), 0xaea4)
	if m.Type != "ACK" || m.Code != "2.04" || !m.Contexted() || len(m.Token) != 0 {
		t.Errorf("answer = %s %s contexted=%t token=%x, want a zone ACK 2.04 with the empty token",
			m.Type, m.Code, m.Contexted(), m.Token)
	}
	if len(m.Options) != 1 || m.Options[0].Num != optOctets.Num || !bytes.Equal(m.Options[0].Val, optOctets.Val) {
		t.Errorf("options = %+v, want only Content-Format 42", m.Options)
	}
	if len(m.Payload) != 0 {
		t.Errorf("payload = %x, want none", m.Payload)
	}
}

// TestZoneMeasurePeerAnswerRelayed: a zone member's 2.05 62a0=00 to a z/p PUT
// reaches the sender as the member sent it.
func TestZoneMeasurePeerAnswerRelayed(t *testing.T) {
	member := mustHex(NodeByAny("a205").WireLE)
	sender := mustHex(NodeByAny("0a01").WireLE)
	answer := TLV{Tag: "62a0", Val: []byte{0}}.Encode()
	e, log, _ := newTestEmu(t)
	actions := e.OnFrame(zoneUplink(t, member, sender, CoAPReq{
		Type: "ACK", Code: "2.05", MID: 0xa8db, Token: []byte{},
		Options: []CoAPOption{optOctets}, Payload: answer,
	}))
	if _, ok := findAction(actions, ActData, "coap-ack"); ok {
		t.Error("answered a member's ACK addressed to another device")
	}
	out, err := e.ScheduleDownlink(hex.EncodeToString(sender))
	if err != nil {
		t.Fatalf("%v; unhandled:\n%s", err, log.Summary())
	}
	f, err := ParseData(NetworkKey, out[len(out)-1].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	m := ParseApp(f.Plaintext).CoAP
	if m.Type != "ACK" || m.Code != "2.05" || m.MID != 0xa8db || !bytes.Equal(m.Payload, answer) {
		t.Errorf("relayed %s %s mid=%04x payload=%x, want ACK 2.05 mid=a8db %x", m.Type, m.Code, m.MID, m.Payload, answer)
	}
	if len(m.Options) != 1 || m.Options[0].Num != optOctets.Num {
		t.Errorf("relayed options = %+v, want only Content-Format 42", m.Options)
	}
}

func TestDirectZoneStateGetServedFromCache(t *testing.T) {
	dev := mustHex("02a6cefa07c51b00")
	e, _, _ := newTestEmu(t)
	e.inst = &Install{HomeID: 7654321}
	body := concat(
		TLV{Tag: "6280", Val: []byte{0x01, 0xf4}}.Encode(),
		TLV{Tag: "6200", Val: []byte{0x01, 0xf4}}.Encode(),
		TLV{Tag: "6240", Val: []byte{0}}.Encode(),
	)
	e.zoneState[zoneKey(3, "z/s")] = body

	raw := deviceUplink(t, dev, CoAPReq{
		Type: "CON", Code: "0.01", MID: 0x1b44, Token: []byte{7},
		Path: []string{"h", "7654321", "z", "3", "s"},
	})
	m := servedAnswer(t, e.OnFrame(raw), 0x1b44)
	// No home state pushed: the head leaves out 6160, 61e0 and 6200, and an
	// auto overlay carries no 6260 or 6280.
	want := "61800100" + "60200101" + "62400100" + "62e00100"
	if got := hex.EncodeToString(m.Payload); m.Code != "2.05" || got != want {
		t.Errorf("answer=%s payload=%s, want 2.05 %s", m.Code, got, want)
	}
}

func TestDirectZoneStateUsesThermostatReadShape(t *testing.T) {
	in := fixtureInstall(t)
	e := NewEmu(in.EmuConfig(), nil)
	body := concat(
		TLV{Tag: "6160", Val: []byte{1}}.Encode(),
		TLV{Tag: "6180", Val: []byte{0}}.Encode(),
		TLV{Tag: "6020", Val: []byte{1}}.Encode(),
		TLV{Tag: "61e0", Val: []byte{1}}.Encode(),
		TLV{Tag: "6200", Val: []byte{0x02, 0xbc}}.Encode(),
		TLV{Tag: "6240", Val: []byte{2}}.Encode(),
		TLV{Tag: "6260", Val: []byte{1}}.Encode(),
		TLV{Tag: "6280", Val: []byte{0x01, 0xf4}}.Encode(),
		TLV{Tag: "62e0", Val: []byte{0}}.Encode(),
	)
	e.zoneState[zoneKey(1, "z/s")] = body
	e.SetHomeState(true, map[int]float64{1: 20})

	ru, err := in.Resolve("RU1000000001")
	if err != nil {
		t.Fatal(err)
	}
	raw := deviceUplink(t, ru.WireEUI(), CoAPReq{
		Type: "CON", Code: "0.01", MID: 0x1b45, Token: []byte{8},
		Path: []string{"h", "7654321", "z", "1", "s"},
	})
	m := servedAnswer(t, e.OnFrame(raw), 0x1b45)
	want := "61600101618001006020010161e0010162000207d0624001026260010162800201f462e00100"
	if got := hex.EncodeToString(m.Payload); m.Code != "2.05" || got != want {
		t.Errorf("thermostat answer=%s payload=%s, want 2.05 %s", m.Code, got, want)
	}

	valve, err := in.Resolve("VA1000000003")
	if err != nil {
		t.Fatal(err)
	}
	raw = deviceUplink(t, valve.WireEUI(), CoAPReq{
		Type: "CON", Code: "0.01", MID: 0x1b46, Token: []byte{9},
		Path: []string{"h", "7654321", "z", "1", "s"},
	})
	m = servedAnswer(t, e.OnFrame(raw), 0x1b46)
	if got := hex.EncodeToString(m.Payload); m.Code != "2.05" || got != want {
		t.Errorf("valve answer=%s payload=%s, want the thermostat's 2.05 %s", m.Code, got, want)
	}
}

// TestZoneGetProxied: with the proxy on, a zone GET addressed to us is forwarded
// to another member of the zone and we answer nothing ourselves. Two replies
// would carry the same MID and the requester would keep whichever landed first,
// which is ours, so the proxy and the cached answer exclude each other.
func TestZoneGetProxied(t *testing.T) {
	ru := mustHex(NodeByAny("RU1000000001").WireLE)
	others := []string{"a70a", "a803"}

	e := NewEmu(EmuConfig{
		TxCounter:    100000,
		ZoneGetProxy: true,
		Zones:        map[int][]string{8: {"a205", "a70a", "a803"}},
	}, nil)
	actions := e.OnFrame(zoneUplink(t, ru, BridgeEUI, CoAPReq{
		Type: "CON", Code: "0.01", MID: 0x7a01, Token: []byte{0x77},
		Path: []string{"z", "s"}, Query: []string{"lid=8"},
	}))
	if _, ok := findAction(actions, ActData, "coap-ack"); ok {
		t.Error("answered the GET as well: the requester dedups on mid=7a01 and keeps ours")
	}
	if _, ok := findAction(actions, ActEnhAck, ""); !ok {
		t.Error("the MAC Enhanced ACK is still due: we did receive the frame")
	}
	if st := e.Stats(); st.ZoneProxies != 1 {
		t.Errorf("ZoneProxies=%d, want 1", st.ZoneProxies)
	}
	if n := e.Pending(hex.EncodeToString(ru)); n != 0 {
		t.Errorf("the requester got %d frames of its own request back", n)
	}

	asked, queued := "", 0
	for _, member := range others {
		if n := e.Pending(member); n > 0 {
			asked, queued = member, queued+n
		}
	}
	if queued != 1 {
		t.Fatalf("%d members queued the GET, want exactly one asked", queued)
	}

	out, err := e.ScheduleDownlink(asked)
	if err != nil {
		t.Fatal(err)
	}
	df, err := ParseData(NetworkKey, out[len(out)-1].Bytes)
	if err != nil {
		t.Fatalf("the proxied GET does not decrypt: %v", err)
	}
	if hex.EncodeToString(df.DstEUI) != NodeByAny(asked).WireLE {
		t.Errorf("proxied to %x, want the member %s", df.DstEUI, asked)
	}
	fm, err := ParseCoAP(df.Plaintext[8:])
	if err != nil {
		t.Fatalf("the proxied GET does not parse: %v", err)
	}
	if fm.MID != 0x7a01 || !bytes.Equal(fm.Token, []byte{0x77}) {
		t.Errorf("mid=%04x token=%x, want the requester's 7a01/77", fm.MID, fm.Token)
	}
	if fm.Code != "0.01" || fm.URI() != "z/s?lid=8" {
		t.Errorf("%s %s, want 0.01 z/s?lid=8", fm.Code, fm.URI())
	}
	// The requester stays the IPv6 source, so the member answers it directly and
	// the checksum has to verify against that pair, not against ours.
	if !bytes.Equal(wireFromIID(fm.Src.IID), ru) {
		t.Errorf("source %x, want the requester %x", fm.Src.IID, ru)
	}
	if ck, ok := fm.RecomputeChecksum(ru, df.DstEUI); !ok || ck != fm.Checksum {
		t.Errorf("checksum %04x does not verify (recomputed %04x, ok=%v)", fm.Checksum, ck, ok)
	}
}

// TestZoneGetProxyFallsBackToCache: the proxy has nobody to ask when the
// requester is the only member it knows, and then the cached answer stands
// rather than the request going unanswered.
func TestZoneGetProxyFallsBackToCache(t *testing.T) {
	valve := mustHex(NodeByAny("a304").WireLE)
	body := TLV{Tag: "40a0", Val: []byte{45}}.Encode()

	e := NewEmu(EmuConfig{TxCounter: 100000, ZoneGetProxy: true, NoZoneFanOut: true}, nil)
	e.OnFrame(zoneUplink(t, valve, BridgeEUI, CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x7b01, Token: []byte{3},
		Path: []string{"z", "p"}, Query: []string{"lid=2"}, Options: []CoAPOption{optOctets},
		Payload: body,
	}))
	m := servedAnswer(t, e.OnFrame(zoneUplink(t, valve, BridgeEUI, CoAPReq{
		Type: "CON", Code: "0.01", MID: 0x7b02, Token: []byte{4},
		Path: []string{"z", "p"}, Query: []string{"lid=2"},
	})), 0x7b02)
	if !bytes.Equal(m.Payload, body) {
		t.Errorf("payload = %x, want the zone's last publish %x", m.Payload, body)
	}
	if st := e.Stats(); st.ZoneProxies != 0 {
		t.Errorf("ZoneProxies=%d with no other member, want 0", st.ZoneProxies)
	}
}

// TestZoneProxyPicksFreshestMember: given a choice, the proxy asks the member
// heard from most recently. A member that has never been heard from is not asked
// at all while a live one is available, because it is the likelier to be asleep.
func TestZoneProxyPicksFreshestMember(t *testing.T) {
	ru := mustHex(NodeByAny("RU1000000001").WireLE)
	stale := mustHex(NodeByAny("a70a").WireLE)
	fresh := mustHex(NodeByAny("a803").WireLE)

	e := NewEmu(EmuConfig{
		TxCounter:    100000,
		ZoneGetProxy: true,
		NoZoneFanOut: true,
		Zones:        map[int][]string{8: {"a205", "a70a", "a803", "a408"}},
	}, nil)
	// a70a reports, then a803 reports later: a803 is the freshest. a408 never
	// says anything at all.
	for _, dev := range [][]byte{stale, fresh} {
		e.OnFrame(zoneUplink(t, dev, BridgeEUI, CoAPReq{
			Type: "CON", Code: "0.03", MID: 0x7c00, Token: []byte{1},
			Path: []string{"z", "p"}, Query: []string{"lid=8"},
			Options: []CoAPOption{optOctets}, Payload: TLV{Tag: "40a0", Val: []byte{10}}.Encode(),
		}))
	}
	e.OnFrame(zoneUplink(t, ru, BridgeEUI, CoAPReq{
		Type: "CON", Code: "0.01", MID: 0x7c01, Token: []byte{2},
		Path: []string{"z", "s"}, Query: []string{"lid=8"},
	}))
	if n := e.Pending(hex.EncodeToString(fresh)); n != 1 {
		t.Errorf("the freshest member queued %d proxied GETs, want 1", n)
	}
	if n := e.Pending(hex.EncodeToString(stale)); n != 0 {
		t.Errorf("the older member was asked as well (%d queued)", n)
	}
	if n := e.Pending(NodeByAny("a408").WireLE); n != 0 {
		t.Errorf("a member never heard from was asked (%d queued)", n)
	}
}

// TestZoneProxyTimeoutAnswersRequester: a member that takes a proxied GET and
// never answers no longer leaves the requester hanging. Past the timeout we
// answer it ourselves, under the original MID and token, from the zone cache.
func TestZoneProxyTimeoutAnswersRequester(t *testing.T) {
	ru := mustHex(NodeByAny("RU1000000001").WireLE)
	member := mustHex(NodeByAny("a70a").WireLE)
	body := TLV{Tag: "40a0", Val: []byte{33}}.Encode()

	now := time.Unix(1_700_000_000, 0)
	e := NewEmu(EmuConfig{
		TxCounter:        100000,
		ZoneGetProxy:     true,
		NoZoneFanOut:     true,
		ZoneProxyTimeout: 10 * time.Second,
		Now:              func() time.Time { return now },
		Zones:            map[int][]string{8: {"a205", "a70a"}},
	}, nil)
	// The member publishes the zone measures, so the fallback has a body to
	// serve. A z/s block is rebuilt instead, see TestZoneReadsRestateTheHomeState.
	e.OnFrame(zoneUplink(t, member, BridgeEUI, CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x7d00, Token: []byte{1},
		Path: []string{"z", "p"}, Query: []string{"lid=8"},
		Options: []CoAPOption{optOctets}, Payload: body,
	}))
	if got := e.OnFrame(zoneUplink(t, ru, BridgeEUI, CoAPReq{
		Type: "CON", Code: "0.01", MID: 0x7d01, Token: []byte{0x42},
		Path: []string{"z", "p"}, Query: []string{"lid=8"},
	})); len(got) == 0 {
		t.Fatal("the proxied GET produced no action at all")
	}
	if n := e.PendingZoneProxies(); n != 1 {
		t.Fatalf("PendingZoneProxies=%d after proxying, want 1", n)
	}
	// Before the deadline nothing is due: the member may still answer.
	if a := e.ExpireZoneProxies(); len(a) != 0 {
		t.Errorf("expired %d proxies before the deadline", len(a))
	}
	now = now.Add(11 * time.Second)
	actions := e.ExpireZoneProxies()
	if len(actions) != 1 {
		t.Fatalf("expired %d proxies past the deadline, want 1", len(actions))
	}
	if e.PendingZoneProxies() != 0 {
		t.Error("the expired proxy is still pending")
	}
	if st := e.Stats(); st.ZoneProxyTimeouts != 1 {
		t.Errorf("ZoneProxyTimeouts=%d, want 1", st.ZoneProxyTimeouts)
	}
	m := servedAnswer(t, actions, 0x7d01)
	if m.Code != "2.05" {
		t.Errorf("code %s, want 2.05", m.Code)
	}
	if !bytes.Equal(m.Token, []byte{0x42}) {
		t.Errorf("token %x, want the requester's 42", m.Token)
	}
	if !bytes.Equal(m.Payload, body) {
		t.Errorf("payload %x, want the zone's cached state %x", m.Payload, body)
	}
	df, err := ParseData(NetworkKey, actions[0].Bytes)
	if err != nil {
		t.Fatalf("the fallback answer does not decrypt: %v", err)
	}
	if !bytes.Equal(df.DstEUI, ru) {
		t.Errorf("answered %x, want the requester %x", df.DstEUI, ru)
	}
}

// TestZoneProxyAnsweredMemberSuppressesTimeout: the member's own 2.05 clears the
// pending proxy, so the requester never gets a second answer under the same MID.
func TestZoneProxyAnsweredMemberSuppressesTimeout(t *testing.T) {
	ru := mustHex(NodeByAny("RU1000000001").WireLE)
	member := mustHex(NodeByAny("a70a").WireLE)

	now := time.Unix(1_700_000_000, 0)
	e := NewEmu(EmuConfig{
		TxCounter:        100000,
		ZoneGetProxy:     true,
		NoZoneFanOut:     true,
		ZoneProxyTimeout: 10 * time.Second,
		Now:              func() time.Time { return now },
		Zones:            map[int][]string{8: {"a205", "a70a"}},
	}, nil)
	e.OnFrame(zoneUplink(t, ru, BridgeEUI, CoAPReq{
		Type: "CON", Code: "0.01", MID: 0x7e01, Token: []byte{7},
		Path: []string{"z", "s"}, Query: []string{"lid=8"},
	}))
	if e.PendingZoneProxies() != 1 {
		t.Fatal("the GET was not proxied")
	}
	// The member answers the requester directly: the reply reaches us as an L2
	// hop addressed to the requester, and we relay it.
	e.OnFrame(zoneUplink(t, member, ru, CoAPReq{
		Type: "ACK", Code: "2.05", MID: 0x7e01, Token: []byte{7},
		Options: []CoAPOption{optOctets}, Payload: TLV{Tag: "40a0", Val: []byte{9}}.Encode(),
	}))
	if n := e.PendingZoneProxies(); n != 0 {
		t.Errorf("PendingZoneProxies=%d after the member answered, want 0", n)
	}
	now = now.Add(30 * time.Second)
	if a := e.ExpireZoneProxies(); len(a) != 0 {
		t.Errorf("answered again after the member already had (%d actions)", len(a))
	}
	if st := e.Stats(); st.ZoneProxyTimeouts != 0 {
		t.Errorf("ZoneProxyTimeouts=%d, want 0", st.ZoneProxyTimeouts)
	}
}

// TestZoneProxyTimeoutDisabled: a negative timeout keeps the pre-timeout
// behaviour, waiting for the member indefinitely.
func TestZoneProxyTimeoutDisabled(t *testing.T) {
	ru := mustHex(NodeByAny("RU1000000001").WireLE)
	now := time.Unix(1_700_000_000, 0)
	e := NewEmu(EmuConfig{
		TxCounter:        100000,
		ZoneGetProxy:     true,
		NoZoneFanOut:     true,
		ZoneProxyTimeout: -1,
		Now:              func() time.Time { return now },
		Zones:            map[int][]string{8: {"a205", "a70a"}},
	}, nil)
	e.OnFrame(zoneUplink(t, ru, BridgeEUI, CoAPReq{
		Type: "CON", Code: "0.01", MID: 0x7f01, Token: []byte{8},
		Path: []string{"z", "s"}, Query: []string{"lid=8"},
	}))
	if st := e.Stats(); st.ZoneProxies != 1 {
		t.Fatalf("ZoneProxies=%d, want the GET proxied", st.ZoneProxies)
	}
	if n := e.PendingZoneProxies(); n != 0 {
		t.Errorf("PendingZoneProxies=%d with the timeout off, want nothing tracked", n)
	}
	now = now.Add(time.Hour)
	if a := e.ExpireZoneProxies(); len(a) != 0 {
		t.Errorf("expired %d proxies with the timeout off", len(a))
	}
}

// zoneUplink synthesizes a context-0 ("lid=") Data frame from dev, carried to us
// at L2 and addressed at the IPv6 layer to peer: peer == BridgeEUI is the copy a
// device sends to the bridge, anything else is a datagram we are meant to
// forward.
func zoneUplink(t *testing.T, dev, peer []byte, r CoAPReq) []byte {
	t.Helper()
	r.SrcEUI, r.DstEUI = dev, peer
	pkt, err := BuildContextCoAP(r) // 7ef7: both addresses from L2, i.e. to us
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(peer, BridgeEUI) {
		// Peer-directed: the source stays elided (it is the L2 sender) and the
		// destination is carried inline, IPHC 7ef5, as every zone publish in
		// the capture does.
		msg, err := buildCoAPMessage(r)
		if err != nil {
			t.Fatal(err)
		}
		out := append([]byte{0x7e, 0xf5, cidBothCtx0}, modifiedEUI64(peer)...)
		out = append(out, nhcUDPPortsIn)
		out = binary.BigEndian.AppendUint16(out, coapPort)
		out = binary.BigEndian.AppendUint16(out, coapPort)
		ck := udpChecksumV6(contextAddr(dev), contextAddr(peer), coapPort, coapPort, msg)
		pkt = append(binary.BigEndian.AppendUint16(out, ck), msg...)
	}
	f, err := BuildData(NetworkKey, 0x22, BridgeEUI, dev[:5], WrapAppPayload(pkt, 800, nil))
	if err != nil {
		t.Fatal(err)
	}
	return f.Bytes()
}

// TestBridgeSeen: a Data frame carrying our own source prefix means the real
// bridge is transmitting. It trips the coexistence guard and teaches the highest
// anti-replay counter the bridge is using.
func TestBridgeSeen(t *testing.T) {
	e, log, _ := newTestEmu(t)
	if e.BridgeSeen() || e.Snapshot().BridgePresent {
		t.Fatal("the guard is set before any traffic")
	}
	if e.MaxBridgeCounter() != 0 {
		t.Fatalf("MaxBridgeCounter=%d before any traffic, want 0", e.MaxBridgeCounter())
	}

	camera := mustB(NodeByAny("camera").WireLE)
	pkt, err := BuildCoAP(CoAPReq{
		Code: "0.03", Path: []string{"d", "info"},
		SrcEUI: BridgeEUI, DstEUI: camera, SrcPort: 4005,
	})
	if err != nil {
		t.Fatal(err)
	}
	const ctr = 123456
	down := func(seq byte, c uint32) []byte {
		f, err := BuildData(NetworkKey, seq, camera, BridgeEUI[:5], WrapAppPayload(pkt, c, nil))
		if err != nil {
			t.Fatal(err)
		}
		return f.Bytes()
	}

	if acts := e.OnFrame(down(0x11, ctr)); acts != nil {
		t.Errorf("a bridge-sourced frame must produce no action, got %v", reasons(acts))
	}
	if !e.BridgeSeen() || !e.Snapshot().BridgePresent {
		t.Error("bridge presence not detected")
	}
	if e.MaxBridgeCounter() != ctr {
		t.Errorf("MaxBridgeCounter=%d, want %d", e.MaxBridgeCounter(), ctr)
	}
	// An older frame never lowers the maximum.
	e.OnFrame(down(0x12, ctr-1000))
	if e.MaxBridgeCounter() != ctr {
		t.Errorf("MaxBridgeCounter=%d after an older frame, want %d", e.MaxBridgeCounter(), ctr)
	}
	if n := log.Counts()[rsnBridgeSrc]; n != 2 {
		t.Errorf("%s count=%d, want 2:\n%s", rsnBridgeSrc, n, log.Summary())
	}
	if len(e.Nodes()) != 0 {
		t.Error("a bridge-sourced frame must not create node state")
	}
}

// capturedRA is a real 0x49e8 broadcast of the bridge, from
// tado_capture/coap_sniff_1786609624.log: the Router Advertisement that carries
// the network configuration of the PAN.
const capturedRA = "49e8c1ffff09a1cefa07c51b0004f02bfd84800bc515da1c5f5bacc22474b591" +
	"0a46eb30e5011c14af362cf3534250148a5c4cec23bd5b241cb6adc994bfdb5c04091ae40a" +
	"db4516e230f44a941cea5b6134fd1503ed18436b4fd296e5fd5dda360de31f9bcb5d01c8a6e3e6"

const capturedRS = "49e800ffff060acefa07c51b00040c0073de984b9d395b733123579c5303fb2d1a0fa3211905d4cf90d9a0593726d51c3f91bc3e"

// TestParseBroadcastCaptured: the captured broadcast decrypts under the network
// key and reads back as the bridge's Router Advertisement.
func TestParseBroadcastCaptured(t *testing.T) {
	m, err := ParseBroadcast(NetworkKey, mustB(capturedRA))
	if err != nil {
		t.Fatalf("captured broadcast does not decrypt: %v", err)
	}
	if m.Seq != 0xc1 || m.SecCtrl != 0x04 || m.FrameCounter != 11248 {
		t.Errorf("seq=%02x secCtrl=%02x ctr=%d, want c1 04 11248", m.Seq, m.SecCtrl, m.FrameCounter)
	}
	if !bytes.Equal(m.SrcEUI, BridgeEUI) {
		t.Errorf("src=%x, want the bridge %x", m.SrcEUI, BridgeEUI)
	}
	if m.ICMPType != icmpRouterAdvert || m.RA == nil {
		t.Fatalf("icmp type=%d ra=%v, want a Router Advertisement", m.ICMPType, m.RA)
	}
	ra := m.RA
	if ra.RouterLifetime != 1800 || ra.MTU != 1500 || ra.Netmask != 0xffffffff {
		t.Errorf("lifetime=%d mtu=%d mask=%08x, want 1800 1500 ffffffff",
			ra.RouterLifetime, ra.MTU, ra.Netmask)
	}
	if hex.EncodeToString(ra.SLLAO) != "001bc507facea109" {
		t.Errorf("SLLAO=%x, want the bridge EUI-64 001bc507facea109", ra.SLLAO)
	}
	if !bytes.Equal(ra.Gateway, []byte{192, 168, 5, 1}) || !bytes.Equal(ra.DNS, []byte{8, 8, 8, 8}) {
		t.Errorf("gateway=%v dns=%v, want 192.168.5.1 and 8.8.8.8", ra.Gateway, ra.DNS)
	}

	if _, err := ParseBroadcast(PairingKey, mustB(capturedRA)); !errors.Is(err, ErrMICFail) {
		t.Errorf("wrong key gave %v, want ErrMICFail", err)
	}

	// A device broadcast is a Router Solicitation: decoded, but no RA to report.
	s, err := ParseBroadcast(NetworkKey, mustB(capturedRS))
	if err != nil {
		t.Fatalf("captured solicitation does not decrypt: %v", err)
	}
	if s.ICMPType != 133 || s.RA != nil {
		t.Errorf("icmp type=%d ra=%v, want 133 and no RA", s.ICMPType, s.RA)
	}
}

// TestBeaconRoundTrip checks the rebuilt beacon against the captured frame.
func TestBeaconRoundTrip(t *testing.T) {
	want, err := ParseBroadcast(NetworkKey, mustB(capturedRA))
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := BuildBroadcast(NetworkKey, want.Seq, want.SrcEUI, uint16(want.FrameCounter), want.SecCtrl, want.Plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rebuilt, mustB(capturedRA)) {
		t.Errorf("beacon\n got %x\nwant %s", rebuilt, capturedRA)
	}
}

// TestBeacon: the periodic broadcast classifies as 0x49e8 and decrypts back.
func TestBeacon(t *testing.T) {
	e, _, _ := newTestEmu(t)
	a, err := e.Beacon()
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != ActBroadcast {
		t.Fatalf("kind=%v, want ActBroadcast", a.Kind)
	}
	if Classify(a.Bytes) != ClassBroadcast {
		t.Fatalf("Classify=%v, want ClassBroadcast", Classify(a.Bytes))
	}
	if !bytes.Equal(a.Bytes[5:13], BridgeEUI) {
		t.Errorf("broadcast src=%x, want %x", a.Bytes[5:13], BridgeEUI)
	}
	second, err := e.Beacon()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a.Bytes, second.Bytes) {
		t.Error("two beacons must differ: the frame counter and seq advance")
	}
}

func TestBeaconCounterCarry(t *testing.T) {
	e := NewEmu(EmuConfig{TxCounter: 0xffff}, nil)
	a, err := e.Beacon()
	if err != nil {
		t.Fatal(err)
	}
	m, err := ParseBroadcast(NetworkKey, a.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if m.FrameCounter != 0x10000 {
		t.Errorf("FrameCounter=%#x, want 0x10000", m.FrameCounter)
	}
	if got := binary.LittleEndian.Uint16(a.Bytes[14:16]); got != 0 {
		t.Errorf("counter low=%#x, want 0", got)
	}
	if got := binary.LittleEndian.Uint16(m.Plaintext[:2]); got != 1 {
		t.Errorf("counter high=%#x, want 1", got)
	}
}

func TestBeaconCounterExhausted(t *testing.T) {
	e := NewEmu(EmuConfig{TxCounter: ^uint32(0) - 1}, nil)
	a, err := e.Beacon()
	if err != nil {
		t.Fatal(err)
	}
	m, err := ParseBroadcast(NetworkKey, a.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if m.FrameCounter != ^uint32(0) {
		t.Errorf("FrameCounter=%#x, want %#x", m.FrameCounter, ^uint32(0))
	}
	for i := 0; i < 2; i++ {
		if _, err := e.Beacon(); !errors.Is(err, ErrTxCounterExhausted) {
			t.Errorf("Beacon error=%v, want ErrTxCounterExhausted", err)
		}
	}
}

func TestUnicastCounterExhausted(t *testing.T) {
	e := NewEmu(EmuConfig{TxCounter: ^uint32(0) - 1}, nil)
	ctr, err := e.nextTx()
	if err != nil || ctr != ^uint32(0) {
		t.Fatalf("next counter=%#x err=%v, want %#x nil", ctr, err, ^uint32(0))
	}
	if _, err := e.nextTx(); !errors.Is(err, ErrTxCounterExhausted) {
		t.Fatalf("counter exhaustion error=%v", err)
	}
	if got := e.TxCounter(); got != ^uint32(0) {
		t.Errorf("counter wrapped to %#x", got)
	}

	d := &queuedDownlink{coap: []byte{0x60, 0x00}, key: NetworkKey}
	if _, _, err := e.buildFrames(testDstEUI(t), d); !errors.Is(err, ErrTxCounterExhausted) {
		t.Fatalf("unfragmented build error=%v", err)
	}
}

func TestRouterSolicitationAnswerRateLimit(t *testing.T) {
	now := time.Unix(100, 0)
	log := NewUnhandledLogger(io.Discard)
	e := NewEmu(EmuConfig{
		Now:                  func() time.Time { return now },
		RouterAdvertInterval: 30 * time.Second,
	}, log)

	// An unarmed transmitter answers nothing and logs the solicitation.
	if actions := e.OnFrame(mustB(capturedRS)); actions != nil {
		t.Fatalf("unarmed RS actions=%+v, want nil", actions)
	}
	if got := log.Counts()[rsnRouterSolicit+" suppressed=tx-not-armed"]; got != 1 {
		t.Fatalf("suppressed solicitations=%d, want 1", got)
	}
	if got := e.Stats().RouterSolicitationsAnswered; got != 0 {
		t.Fatalf("answered=%d, want 0 while unarmed", got)
	}

	e.SetTXArmed(func() bool { return true })
	first := e.OnFrame(mustB(capturedRS))
	if len(first) != RouterAdvertWakeUpCount+1 {
		t.Fatalf("first RS actions=%d, want %d", len(first), RouterAdvertWakeUpCount+1)
	}
	for i := range first[:RouterAdvertWakeUpCount] {
		if first[i].Kind != ActWakeUp || first[i].Bytes[1] != first[len(first)-1].Bytes[2] {
			t.Fatalf("wake-up %d does not share the RA sequence", i)
		}
	}
	if first[len(first)-1].Kind != ActBroadcast {
		t.Fatal("last action is not the router advertisement")
	}
	m, err := ParseBroadcast(NetworkKey, first[len(first)-1].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if m.FrameCounter != 1 {
		t.Fatalf("first counter=%d, want 1", m.FrameCounter)
	}
	e.CompleteBeacon(true)

	// Ten more solicitations inside the window: the burst on air answers them all.
	for i := 0; i < 10; i++ {
		now = now.Add(100 * time.Millisecond)
		if actions := e.OnFrame(mustB(capturedRS)); actions != nil {
			t.Fatalf("solicitation %d inside the window built %d actions", i, len(actions))
		}
	}
	if got := e.Stats().RouterSolicitationsAnswered; got != 11 {
		t.Errorf("answered=%d, want 11", got)
	}
	if got := log.Total(); got != 1 {
		t.Errorf("logged events=%d, want 1: an answered solicitation is not unhandled", got)
	}

	now = now.Add(routerSolicitAnswerWindow)
	second := e.OnFrame(mustB(capturedRS))
	if len(second) != RouterAdvertWakeUpCount+1 {
		t.Fatalf("RS after the window actions=%d, want %d", len(second), RouterAdvertWakeUpCount+1)
	}
	m, err = ParseBroadcast(NetworkKey, second[len(second)-1].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if m.FrameCounter != 2 {
		t.Fatalf("second counter=%d, want 2", m.FrameCounter)
	}
	e.CompleteBeacon(true)

	// The periodic beacon keeps its own interval, and the burst it puts on air
	// covers a solicitation that follows it.
	if _, due, err := e.MaybeBeacon(); err != nil || due {
		t.Fatalf("ticker beacon due=%v err=%v, want false nil", due, err)
	}
	now = now.Add(30 * time.Second)
	if _, due, err := e.MaybeBeacon(); err != nil || !due {
		t.Fatalf("ticker beacon due=%v err=%v, want true nil", due, err)
	}
	e.CompleteBeacon(true)
	if actions := e.OnFrame(mustB(capturedRS)); actions != nil {
		t.Fatalf("RS after the ticker beacon built %d actions", len(actions))
	}
	if got := e.Stats().RouterAdvertisementsSent; got != 3 {
		t.Errorf("RouterAdvertisementsSent=%d, want 3", got)
	}
}

func TestRouterAdvertRetriesAfterFailedTransmit(t *testing.T) {
	e := NewEmu(EmuConfig{}, nil)
	e.SetTXArmed(func() bool { return true })
	if actions := e.OnFrame(mustB(capturedRS)); len(actions) != RouterAdvertWakeUpCount+1 {
		t.Fatalf("first actions=%d, want %d", len(actions), RouterAdvertWakeUpCount+1)
	}
	e.CompleteBeacon(false)
	if actions := e.OnFrame(mustB(capturedRS)); len(actions) != RouterAdvertWakeUpCount+1 {
		t.Fatalf("retry actions=%d, want %d", len(actions), RouterAdvertWakeUpCount+1)
	}
	if got := e.Stats().RouterAdvertisementsSent; got != 0 {
		t.Errorf("RouterAdvertisementsSent=%d, want 0 before confirmation", got)
	}
	e.CompleteBeacon(true)
	if got := e.Stats().RouterAdvertisementsSent; got != 1 {
		t.Errorf("RouterAdvertisementsSent=%d, want 1", got)
	}
}

func TestOnBroadcastClassifiesICMPv6(t *testing.T) {
	e, log, _ := newTestEmu(t)
	if actions := e.OnFrame(mustB(capturedRA)); actions != nil {
		t.Fatalf("RA actions=%v, want nil", actions)
	}
	if actions := e.OnFrame(mustB(capturedRS)); actions != nil {
		t.Fatalf("RS actions=%v, want nil", actions)
	}
	nsPacket := []byte{iphcMcast0, iphcMcast1, nhICMPv6, 0x01, icmpNeighborSolicit, 0, 0, 0}
	ns, err := BuildBroadcast(NetworkKey, 0x21, mustB("060acefa07c51b00"), 9, bcastSecCtrl,
		WrapBroadcastPayload(nsPacket, 0))
	if err != nil {
		t.Fatal(err)
	}
	if actions := e.OnFrame(ns); actions != nil {
		t.Fatalf("NS actions=%v, want nil", actions)
	}
	st := e.Stats()
	if st.Broadcasts != 3 || st.ICMPv6 != 3 || st.RouterAdvertisements != 1 ||
		st.RouterSolicitations != 1 || st.NeighborSolicitations != 1 {
		t.Errorf("broadcast stats=%+v", st)
	}
	if st.RouterAdvertisementsSent != 0 {
		t.Errorf("RouterAdvertisementsSent=%d, want 0 with the default interval", st.RouterAdvertisementsSent)
	}
	for _, reason := range []string{rsnRouterAdvert, rsnRouterSolicit, rsnNeighborSolicit} {
		if log.Counts()[reason] != 1 {
			t.Errorf("%s count=%d, want 1", reason, log.Counts()[reason])
		}
	}
}

func TestOnBroadcastMICFailureLatchesBridge(t *testing.T) {
	e := NewEmu(EmuConfig{RouterAdvertInterval: time.Minute}, NewUnhandledLogger(io.Discard))
	bad := mustB(capturedRA)
	bad[len(bad)-1] ^= 1
	if actions := e.OnFrame(bad); actions != nil {
		t.Fatalf("bad broadcast actions=%+v, want nil", actions)
	}
	if !e.BridgeSeen() {
		t.Error("bridge source was not latched before broadcast MIC verification")
	}
	if st := e.Stats(); st.MICFails != 1 || st.Broadcasts != 1 {
		t.Errorf("broadcast stats=%+v, want one MIC failure", st)
	}
}

func TestRouterSolicitationSkipsCoordinator(t *testing.T) {
	e := NewEmu(EmuConfig{RouterAdvertInterval: time.Minute}, nil)
	rsPacket := []byte{iphcMcast0, iphcMcast1, nhICMPv6, 0x01, icmpRouterSolicit, 0, 0, 0}
	bridgeRS, err := BuildBroadcast(NetworkKey, 0x21, BridgeEUI, 9, bcastSecCtrl,
		WrapBroadcastPayload(rsPacket, 0))
	if err != nil {
		t.Fatal(err)
	}
	if actions := e.OnFrame(bridgeRS); actions != nil {
		t.Fatalf("bridge RS actions=%+v, want nil", actions)
	}
	if !e.BridgeSeen() {
		t.Fatal("bridge source was not latched")
	}
	if _, due, err := e.MaybeBeacon(); err != nil || due {
		t.Fatalf("bridge ticker due=%v err=%v, want false nil", due, err)
	}
}

// TestResourceCatalog covers the normalization that lets one catalog entry match
// every instance of a resource.
func TestResourceCatalog(t *testing.T) {
	known := []string{
		"z/s", "z/p", "h/7654321/z/5/config", "h/7654321/z/2/s", "h/7654321/c/1/act",
		"h/7654321/hvac/mon", "h/7654321/hvac/mon/dhw", "h/7654321/z/2/ow",
		"d/VA1000000006/config", "d/VA1000000006/lock", "d/VA1000000001/sen",
		"d/RU1000000001/fw/state", "auth/key", "netcmd",
	}
	for _, p := range known {
		if !knownResource(p) {
			t.Errorf("knownResource(%q) = false, want true (normalized %q)", p, normalizeResource(p))
		}
	}
	for _, p := range []string{"", "z/quux", "d/VA1000000006/nope", "h/7654321/z/5/wat"} {
		if knownResource(p) {
			t.Errorf("knownResource(%q) = true, want false", p)
		}
	}
	if got := normalizeResource("h/7654321/z/5/config"); got != "z/config" {
		t.Errorf("normalizeResource = %q, want z/config", got)
	}
}

func reasons(actions []Action) []string {
	out := make([]string, len(actions))
	for i, a := range actions {
		out[i] = a.Kind.String() + ":" + a.Reason
	}
	return out
}

// TestConfigEtagSource: the document carries exactly one tag 015a record. It is
// the caller's ETag when ConfigWrite.Etag is set - the emulator must not stamp a
// second one, which the device's last-wins parse would resolve in its favour -
// and the emulator's own hash otherwise. Either way the recorded ETag is the one
// that went on the wire, so a conditional GET validates against it.
func TestConfigEtagSource(t *testing.T) {
	const target = "VA1000000007" // bagno valve
	node := NodeByAny(target)
	if node == nil {
		t.Fatal("valve not in inventory")
	}
	devEUI := mustHex(node.WireLE)
	caller := []byte{0xde, 0xad, 0xbe, 0xef, 0x00, 0x11, 0x22, 0x33}

	tests := []struct {
		name string
		etag []byte
		want []byte // nil: whatever the emulator stamped
	}{
		{"caller supplied", caller, caller},
		{"emulator stamped", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, _, _ := newTestEmu(t)
			off := 50
			if err := e.EnqueueConfig(target, 3, ConfigWrite{CalibOffset: &off, Etag: tt.etag}); err != nil {
				t.Fatal(err)
			}
			snap, ok := e.cfgDoc[hexEUI(devEUI)]
			if !ok {
				t.Fatal("EnqueueConfig recorded no config document")
			}
			var onWire [][]byte
			for _, tlv := range parseTLVList(snap.body) {
				if tlv.Tag == configEtagTag {
					onWire = append(onWire, tlv.Val)
				}
			}
			if len(onWire) != 1 {
				t.Fatalf("body carries %d %s records, want 1", len(onWire), configEtagTag)
			}
			if !bytes.Equal(onWire[0], snap.etag) {
				t.Errorf("on-wire etag %x != recorded etag %x", onWire[0], snap.etag)
			}
			switch {
			case tt.want != nil:
				if !bytes.Equal(snap.etag, tt.want) {
					t.Errorf("etag = %x, want the caller's %x", snap.etag, tt.want)
				}
			case len(snap.etag) != 8:
				t.Errorf("stamped etag len=%d, want 8", len(snap.etag))
			}
		})
	}
}

// TestConfigEtagValidation: EnqueueConfig stamps the d/config document with its
// ETag (tag 015a), and the emulator validates a device's conditional GET of
// d/config against it - 2.03 Valid on a match, 2.05 Content with the document and
// its ETag on a stale/absent one - the way the real bridge does.
func TestConfigEtagValidation(t *testing.T) {
	const target = "VA1000000007" // bagno valve
	node := NodeByAny(target)
	if node == nil {
		t.Fatal("valve not in inventory")
	}
	devEUI := mustHex(node.WireLE)

	e, _, _ := newTestEmu(t)
	off := 50
	if err := e.EnqueueConfig(target, 3, ConfigWrite{CalibOffset: &off}); err != nil {
		t.Fatal(err)
	}
	snap, ok := e.cfgDoc[hexEUI(devEUI)]
	if !ok {
		t.Fatal("EnqueueConfig recorded no config document")
	}
	if len(snap.etag) != 8 {
		t.Fatalf("etag len=%d, want 8", len(snap.etag))
	}
	// The pushed document must carry the ETag as TLV 015a.
	if tlvs := parseTLVList(snap.body); len(tlvs) == 0 || tlvs[len(tlvs)-1].Tag != configEtagTag ||
		!bytes.Equal(tlvs[len(tlvs)-1].Val, snap.etag) {
		t.Fatalf("document does not end with ETag TLV %s=%x", configEtagTag, snap.etag)
	}

	get := func(mid uint16, etag []byte) *AppPayload {
		var opts []CoAPOption
		if etag != nil {
			opts = []CoAPOption{{Num: 4, Val: etag}}
		}
		req, err := BuildCoAP(CoAPReq{
			Type: "CON", Code: "0.01", MID: mid, Token: []byte{0x09},
			Path: []string{"d", "config"}, Options: opts,
			SrcEUI: devEUI, DstEUI: BridgeEUI, SrcPort: coapPort, DstPort: bridgeCoAPPort,
		})
		if err != nil {
			t.Fatal(err)
		}
		frame, err := BuildData(NetworkKey, 0x21, BridgeEUI, devEUI[:5], WrapAppPayload(req, 600, nil))
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range e.OnFrame(frame.Bytes()) {
			if a.Kind != ActData {
				continue
			}
			df, err := ParseData(NetworkKey, a.Bytes)
			if err != nil {
				continue
			}
			if app := ParseApp(df.Plaintext); app != nil && app.CoAP != nil && app.CoAP.MID == mid {
				return app
			}
		}
		return nil
	}

	// A stale ETag receives the document and learns the ETag for the next GET.
	var servedETag []byte
	if app := get(0xbe01, []byte{1, 2, 3, 4, 5, 6, 7, 8}); app == nil {
		t.Fatal("no answer to stale-etag GET")
	} else {
		if app.CoAP.Code != "2.05" {
			t.Errorf("stale etag: code=%s, want 2.05", app.CoAP.Code)
		}
		if want := servedConfigBody(snap); !bytes.Equal(app.CoAP.Payload, want) {
			t.Errorf("2.05 payload = %x, want stored body without 015a %x", app.CoAP.Payload, want)
		}
		if et, ok := app.CoAP.Option(4); !ok || !bytes.Equal(et.Val, snap.etag) {
			t.Errorf("2.05 must carry ETag option = %x (ok=%v got %x)", snap.etag, ok, et.Val)
		} else {
			servedETag = append([]byte(nil), et.Val...)
		}
	}

	if app := get(0xbe02, servedETag); app == nil {
		t.Fatal("no answer to matching-etag GET")
	} else if app.CoAP.Code != "2.03" {
		t.Errorf("matching etag: code=%s, want 2.03", app.CoAP.Code)
	} else if len(app.CoAP.Payload) != 0 {
		t.Errorf("2.03 Valid must be empty, got %x", app.CoAP.Payload)
	}
}

// TestServedConfigCarriesETagInOptionOnly: the 2.05 body of d/config leaves out
// tag 015a, as the cloud's does, and the ETag rides in option 4. The stored and
// persisted document keeps its 015a, so a restart serves the same ETag and the
// valve's conditional GET still earns 2.03.
func TestServedConfigCarriesETagInOptionOnly(t *testing.T) {
	e, in := fixtureEmu(t)
	get := func(e *EmuState, mid uint16, etag []byte) *CoAPMessage {
		opts := []CoAPOption{{Num: 23, Val: []byte{0x03}}}
		if etag != nil {
			opts = append([]CoAPOption{{Num: 4, Val: etag}}, opts...)
		}
		raw := deviceUplink(t, mustHex(cameraEUIHex), CoAPReq{
			Type: "CON", Code: "0.01", MID: mid, Token: []byte{0x0e},
			Path: []string{"d", "config"}, Options: opts,
		})
		return servedAnswer(t, e.OnFrame(raw), mid)
	}

	m := get(e, 0xc101, nil)
	etag, ok := m.Option(4)
	if m.Code != "2.05" || !ok || !bytes.Equal(etag.Val, e.cfgDoc[cameraEUIHex].etag) {
		t.Fatalf("d/config = %s, ETag option %x, want 2.05 and %x", m.Code, etag.Val, e.cfgDoc[cameraEUIHex].etag)
	}
	for _, tlv := range parseTLVList(m.Payload) {
		if tlv.Tag == configEtagTag {
			t.Fatalf("2.05 body carries %s=%x", configEtagTag, tlv.Val)
		}
	}
	if len(parseTLVList(m.Payload)) == 0 {
		t.Fatal("2.05 body is empty")
	}

	restarted, _ := fixtureEmu(t)
	if err := restarted.RestoreDesiredState(e.ExportDesiredState()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := restarted.SetInstall(in); err != nil {
		t.Fatal(err)
	}
	if got := restarted.cfgDoc[cameraEUIHex].etag; !bytes.Equal(got, etag.Val) {
		t.Fatalf("ETag after restart = %x, want %x", got, etag.Val)
	}
	if m := get(restarted, 0xc102, etag.Val); m.Code != "2.03" {
		t.Fatalf("conditional GET after restart = %s, want 2.03", m.Code)
	}
}

// stm32FlowCircuit mirrors cfg_attr_list_validate@0x08000548 of the thermostat
// STM32: the first slot with kind (6020) 1 selects the circuit, from 6060 when
// the role (6340) has bit 0x02, else 9 when it has bit 0x04. 0x7f = none.
func stm32FlowCircuit(recs []NodeTableRec, kind, circuit func(zone int) byte) byte {
	for _, r := range recs {
		if kind(int(r.Node)) != 1 {
			continue
		}
		switch {
		case r.Role&0x02 != 0:
			return circuit(int(r.Node))
		case r.Role&0x04 != 0:
			return 9
		}
	}
	return 0x7f
}

// TestThermostatNodeTableSelectsFlowCircuit: the commercial bridge serves the
// wall thermostat 015e = 0d00 0b01 0202 0203 0205 0208 020a
// (capture-app-20260907, d/<RU>/config 2.05), and after the next c/1/config
// the boiler read back MaxTSet = 48 C (hvac/config 046f = 12c1). The STM32
// reaches tag 2040 only through the 0x02 bit of its own heating zone.
func TestThermostatNodeTableSelectsFlowCircuit(t *testing.T) {
	in := fixtureInstall(t)
	ru, err := in.Resolve("RU1000000001")
	if err != nil {
		t.Fatal(err)
	}
	var wire []byte
	for _, r := range initialNodeTable(in, ru) {
		wire = append(wire, r.Role, r.Node)
	}
	if got, want := hex.EncodeToString(wire), "0d000b010202020302050208020a"; got != want {
		t.Errorf("thermostat 015e records = %s, want the captured %s", got, want)
	}
	kind := func(zone int) byte {
		z, err := in.ZoneByNumber(zone)
		if err != nil || z.Kind != ZoneHeating {
			return 2
		}
		return 1
	}
	circuit := func(zone int) byte {
		z, err := in.ZoneByNumber(zone)
		if err != nil || z.Circuit == nil {
			return 0x7f
		}
		return byte(*z.Circuit)
	}
	if got := stm32FlowCircuit(initialNodeTable(in, ru), kind, circuit); got != 1 {
		t.Errorf("STM32 flow circuit = %d, want 1: 2040|1 must reach OpenTherm MaxTSet", got)
	}
}

func TestInitialConfigDocumentFromInstall(t *testing.T) {
	in := fixtureInstall(t)
	e := NewEmu(in.EmuConfig(), nil)
	camera := mustHex("02a6cefa07c51b00")
	snap, ok := e.cfgDoc[hexEUI(camera)]
	if !ok {
		t.Fatal("camera has no initial d/config document")
	}
	var records []NodeTableRec
	for _, tlv := range parseTLVList(snap.body) {
		if tlv.Tag == "015e" && len(tlv.Val) == 2 {
			records = append(records, NodeTableRec{Role: tlv.Val[0], Node: tlv.Val[1]})
		}
	}
	if want := []NodeTableRec{{Role: 0x0d, Node: 10}}; !reflect.DeepEqual(records, want) {
		t.Errorf("camera node table=%v, want %v", records, want)
	}
	reporter, err := in.Resolve("VA1000000006")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := initialNodeTable(in, reporter), []NodeTableRec{{Role: 0x0d, Node: 5}}; !reflect.DeepEqual(got, want) {
		t.Errorf("reporter node table=%v, want %v", got, want)
	}
	nonReporter, err := in.Resolve("VA1000000001")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := initialNodeTable(in, nonReporter), []NodeTableRec{{Role: 0x05, Node: 5}}; !reflect.DeepEqual(got, want) {
		t.Errorf("non-reporter node table=%v, want %v", got, want)
	}

	ru, err := in.Resolve("RU1000000001")
	if err != nil {
		t.Fatal(err)
	}
	wantRU := []NodeTableRec{
		{Role: 0x0d, Node: 0}, {Role: 0x0b, Node: 1},
		{Role: 0x02, Node: 2}, {Role: 0x02, Node: 3}, {Role: 0x02, Node: 5},
		{Role: 0x02, Node: 8}, {Role: 0x02, Node: 10},
	}
	if got := initialNodeTable(in, ru); !reflect.DeepEqual(got, wantRU) {
		t.Errorf("thermostat node table=%v, want %v", got, wantRU)
	}

	raw := deviceUplink(t, camera, CoAPReq{
		Type: "CON", Code: "0.01", MID: 0xc001, Token: []byte{0x0c},
		Path: []string{"d", "config"}, Options: []CoAPOption{{Num: 23, Val: []byte{0x03}}},
	})
	m := servedAnswer(t, e.OnFrame(raw), 0xc001)
	if want := servedConfigBody(snap); m.Code != "2.05" || !bytes.Equal(m.Payload, want) {
		t.Errorf("initial d/config=%s payload %d B, want 2.05 and %d B", m.Code, len(m.Payload), len(want))
	}
	if etag, present := m.Option(4); !present || !bytes.Equal(etag.Val, snap.etag) {
		t.Errorf("initial ETag=%x, want %x", etag.Val, snap.etag)
	}

	calib := 50
	if err := e.EnqueueConfig(hexEUI(camera), 10, ConfigWrite{CalibOffset: &calib}); err != nil {
		t.Fatal(err)
	}
	updated := e.cfgDoc[hexEUI(camera)]
	raw = deviceUplink(t, camera, CoAPReq{
		Type: "CON", Code: "0.01", MID: 0xc002, Token: []byte{0x0d},
		Path: []string{"d", "config"}, Options: []CoAPOption{{Num: 4, Val: snap.etag}},
	})
	m = servedAnswer(t, e.OnFrame(raw), 0xc002)
	if m.Code != "2.05" {
		t.Fatalf("stale initial ETag answer=%s, want 2.05", m.Code)
	}
	if etag, present := m.Option(4); !present || !bytes.Equal(etag.Val, updated.etag) {
		t.Errorf("updated ETag=%x, want %x", etag.Val, updated.etag)
	}
	got := map[string][][]byte{}
	for _, tlv := range parseTLVList(m.Payload) {
		got[tlv.Tag] = append(got[tlv.Tag], tlv.Val)
	}
	if v := got["015c"]; len(v) != 1 || !bytes.Equal(v[0], []byte{0x00, 0x74, 0xcb, 0xb1}) {
		t.Errorf("home id=%x, want 0074cbb1", v)
	}
	if v := got["015d"]; len(v) != 1 || !bytes.Equal(v[0], []byte{0x00, 0x70}) {
		t.Errorf("report interval=%x, want 0070", v)
	}
	if v := got["02b2"]; len(v) != 1 || !bytes.Equal(v[0], []byte{0x00, 0x3c}) {
		t.Errorf("UTC offset=%x, want 003c", v)
	}
	if v := got["015e"]; len(v) != 1 || !bytes.Equal(v[0], []byte{0x0d, 0x0a}) {
		t.Errorf("node table=%x, want 0d0a", v)
	}
	if v := got["0140"]; len(v) != 1 || !bytes.Equal(v[0], []byte{0x00, 0x32}) {
		t.Errorf("calibration=%x, want 0032", v)
	}
}

func TestEnqueueConfigFragmentsOversizedNodeTable(t *testing.T) {
	e, _, _ := newTestEmu(t)
	table := make([]NodeTableRec, 20)
	for i := range table {
		table[i] = NodeTableRec{Role: 0x0b, Node: uint8(i)}
	}
	if err := e.EnqueueConfig("camera", 10, ConfigWrite{NodeTable: table}); err != nil {
		t.Fatal(err)
	}
	if e.Pending("camera") != 1 {
		t.Fatalf("pending=%d, want 1", e.Pending("camera"))
	}
	wire, label, err := e.resolve("camera")
	if err != nil {
		t.Fatal(err)
	}
	actions, ok := e.nextDownlink(wire, label)
	if !ok || len(actions) < 2 {
		t.Fatalf("fragmented downlink actions=%d ok=%t, want at least 2", len(actions), ok)
	}

	ra := NewReassembler()
	var msg *CoAPMessage
	for i, a := range actions {
		f, err := ParseData(NetworkKey, a.Bytes)
		if err != nil {
			t.Fatalf("fragment %d: %v", i, err)
		}
		// Only the last fragment of the only queued downlink clears Frame Pending.
		want := fcfDataPending
		if i == len(actions)-1 {
			want = fcfData
		}
		if a.Bytes[0] != want[0] || a.Bytes[1] != want[1] {
			t.Fatalf("fragment %d FCF=%02x%02x, want %02x%02x", i, a.Bytes[0], a.Bytes[1], want[0], want[1])
		}
		if len(a.Bytes) > 127 {
			t.Fatalf("fragment %d is %d B, exceeds PHY limit", i, len(a.Bytes))
		}
		if m, done := ra.Add(f); done {
			msg = m
		}
	}
	if msg == nil {
		t.Fatal("fragments did not reassemble")
	}
	if msg.URI() != "d/config" {
		t.Errorf("uri=%q, want d/config", msg.URI())
	}
	var got []NodeTableRec
	for _, tlv := range parseTLVList(msg.Payload) {
		if tlv.Tag == "015e" && len(tlv.Val) == 2 {
			got = append(got, NodeTableRec{Role: tlv.Val[0], Node: tlv.Val[1]})
		}
	}
	if !reflect.DeepEqual(got, table) {
		t.Errorf("node table=%v, want %v", got, table)
	}
	retry, ok := e.nextDownlink(wire, label)
	if !ok || len(retry) != len(actions) {
		t.Fatalf("retry actions=%d ok=%t, want %d fragmented frames", len(retry), ok, len(actions))
	}
	first, err := ParseData(NetworkKey, actions[0].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ParseData(NetworkKey, retry[0].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	firstTag := binary.BigEndian.Uint16(first.Plaintext[8+2 : 8+4])
	secondTag := binary.BigEndian.Uint16(second.Plaintext[8+2 : 8+4])
	if firstTag == secondTag {
		t.Fatalf("fragment retry reused tag=%04x", firstTag)
	}

	last, err := ParseData(NetworkKey, actions[len(actions)-1].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	e.confirmBySeq(wire, last.Seq)
	if e.Pending("camera") != 1 {
		t.Errorf("MAC ACK removed fragmented downlink: pending=%d, want 1", e.Pending("camera"))
	}
	e.confirmByMID(wire, &CoAPMessage{Type: "ACK", Code: "2.04", MID: msg.MID, Token: msg.Token})
	if e.Pending("camera") != 0 {
		t.Errorf("CoAP ACK did not remove fragmented downlink: pending=%d", e.Pending("camera"))
	}
}

func TestEmuFragmentTagSeedAndRetry(t *testing.T) {
	e := NewEmu(EmuConfig{
		TxCounter:   100000,
		FragTagSeed: func() uint16 { return 0xfffe },
	}, nil)
	if got := e.nextFragTag(); got != 0xffff {
		t.Fatalf("first tag=%04x, want ffff", got)
	}
	if got := e.nextFragTag(); got != 0 {
		t.Fatalf("retry tag=%04x, want 0000", got)
	}
}

// legionellaRequest is a device request on z/legionella under the h/<home>/z/<n>
// prefix the resource is addressed with.
func legionellaRequest(t *testing.T, dev []byte, code, query string, mid uint16, payload []byte) []byte {
	t.Helper()
	r := CoAPReq{
		Type: "CON", Code: code, MID: mid, Token: []byte{0x44},
		Path: []string{"h", "7654321", "z", "10", "legionella"}, Payload: payload,
	}
	if query != "" {
		r.Query = []string{query}
	}
	if len(payload) > 0 {
		r.Options = []CoAPOption{optOctets}
	}
	return deviceUplink(t, dev, r)
}

// TestServedLegionellaPost: a POST naming the device's own zone is answered
// 2.01 and its single 63c0 record is filed against the node.
func TestServedLegionellaPost(t *testing.T) {
	dev := mustHex("02a6cefa07c51b00") // VA1000000005, the only member of zone 10
	in := fixtureInstall(t)
	e := NewEmu(in.EmuConfig(), NewUnhandledLogger(io.Discard))

	m := servedAnswer(t, e.OnFrame(legionellaRequest(t, dev, "0.02", "id=10", 0x2201, mustHex("63c00101"))), 0x2201)
	if m.Code != "2.01" {
		t.Fatalf("code = %s, want 2.01", m.Code)
	}
	if len(m.Payload) != 0 {
		t.Errorf("answer carries a body %x, want none", m.Payload)
	}
	var got []byte
	for _, n := range e.Nodes() {
		if n.WireLE == hex.EncodeToString(dev) {
			got = n.Legionella
		}
	}
	if !bytes.Equal(got, []byte{0x01}) {
		t.Errorf("node legionella value = %x, want 01", got)
	}
}

// TestServedLegionellaRefusals: only the POST slot exists on the device, the id
// has to name the zone the device answers to, and the value caps at 3 bytes.
func TestServedLegionellaRefusals(t *testing.T) {
	dev := mustHex("02a6cefa07c51b00")
	cases := []struct {
		name, code, query, want string
		payload                 []byte
	}{
		{"get", "0.01", "id=10", "4.05", nil},
		{"put", "0.03", "id=10", "4.05", mustHex("63c00101")},
		{"delete", "0.04", "id=10", "4.05", nil},
		{"no id", "0.02", "", "4.00", mustHex("63c00101")},
		{"other zone", "0.02", "id=3", "4.00", mustHex("63c00101")},
		{"value over 3 bytes", "0.02", "id=10", "4.00", mustHex("63c00401020304")},
	}
	in := fixtureInstall(t)
	for i, c := range cases {
		e := NewEmu(in.EmuConfig(), NewUnhandledLogger(io.Discard))
		mid := uint16(0x2301 + i)
		m := servedAnswer(t, e.OnFrame(legionellaRequest(t, dev, c.code, c.query, mid, c.payload)), mid)
		if m.Code != c.want {
			t.Errorf("%s: code = %s, want %s", c.name, m.Code, c.want)
		}
		if len(m.Payload) != 0 {
			t.Errorf("%s: answer carries a body %x, want none", c.name, m.Payload)
		}
	}
}

// TestServedLegionellaUnknownZone: with no zone knowledge for the device the id
// cannot be checked, so the POST is served and the gap is logged.
func TestServedLegionellaUnknownZone(t *testing.T) {
	dev := mustHex("02a6cefa07c51b00")
	e, log, _ := newTestEmu(t)
	m := servedAnswer(t, e.OnFrame(legionellaRequest(t, dev, "0.02", "id=7", 0x2401, mustHex("63c00100"))), 0x2401)
	if m.Code != "2.01" {
		t.Errorf("code = %s, want 2.01", m.Code)
	}
	if n := countPrefix(log.Counts(), rsnZoneIDUnverified); n != 1 {
		t.Errorf("unverified zone id logged %d times, want 1:\n%s", n, log.Summary())
	}
}

// TestCalibrateOutcome: the valve closes d/calibrate with a 2.xx, a 4.05
// drops it, and the d/<serial>/mnt report carries the calibration state.
func TestCalibrateOutcome(t *testing.T) {
	in := synthInstall(t)
	e := NewEmu(in.EmuConfig(), nil)
	va, err := in.Resolve("VA0000000001")
	if err != nil {
		t.Fatal(err)
	}
	wire := va.WireEUI()
	for _, code := range []string{"2.05", "2.04", "4.05"} {
		if err := e.EnqueueCalibrate("VA0000000001"); err != nil {
			t.Fatal(err)
		}
		if _, ok := e.nextDownlink(wire, "study"); !ok {
			t.Fatal("no calibrate attempt")
		}
		q := e.queue[hexEUI(wire)][0]
		e.confirmByMID(wire, &CoAPMessage{Type: "ACK", Code: code, MID: q.mid, Token: q.responseToken})
		if n := e.Pending("VA0000000001"); n != 0 {
			t.Fatalf("%s left %d downlinks queued, want 0", code, n)
		}
		want := [2]string{"delivered", "coap-" + code}
		if code == "4.05" {
			want[0] = "dropped"
		}
		h := e.QueueHistory("VA0000000001")[0]
		if got := [2]string{h.Result, h.Reason}; h.Kind != "calibrate" || got != want {
			t.Errorf("%s outcome=%s %v, want calibrate %v", code, h.Kind, got, want)
		}
	}

	raw := synthDeviceUplink(t, in, wire, CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x7401, Path: []string{"d", "VA0000000001", "mnt"},
		Payload: encodeTLVs([]TLV{
			{Tag: "01b5", Val: []byte{0x00, 0x10}}, {Tag: "01b6", Val: []byte{0x0d, 0xac}},
			{Tag: "01fa", Val: []byte{2}}, {Tag: "01fb", Val: []byte{0}},
		}),
	})
	if m := synthAnswer(t, in, e.OnFrame(raw), 0x7401); m.Code != "2.04" {
		t.Errorf("mnt answer=%s, want 2.04", m.Code)
	}
	for _, n := range e.Nodes() {
		if n.WireLE != hex.EncodeToString(wire) {
			continue
		}
		if n.Telemetry["calib_state"] != "2" || n.Telemetry["calib_error"] != "0" || n.Telemetry["ctrl_1b6"] != "3500" {
			t.Errorf("telemetry=%v, want calib_state=2 calib_error=0 ctrl_1b6=3500", n.Telemetry)
		}
		return
	}
	t.Fatal("the valve has no node state")
}
