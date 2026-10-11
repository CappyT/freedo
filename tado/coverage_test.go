package tado

import (
	"bytes"
	"encoding/hex"
	"testing"
	"time"
)

// Coverage tests for the resource/tag catalog and the served/active-GET/config
// handlers added to close the unhandled-frame gaps. Every frame is synthesized
// with the package builders, so no hardware or capture file is involved.

// uplinkFrame builds one on-air Data frame from a device to the bridge carrying a
// CoAP request (link-local, the path a device uses to reach the bridge).
func uplinkFrame(t *testing.T, srcEUI []byte, code string, path, query []string, payload []byte) []byte {
	t.Helper()
	pkt, err := BuildCoAP(CoAPReq{
		Type:    "CON",
		Code:    code,
		MID:     0x4321,
		Token:   []byte{0x09},
		Path:    path,
		Query:   query,
		Payload: payload,
		SrcEUI:  srcEUI,
		DstEUI:  BridgeEUI,
		SrcPort: coapPort,
		DstPort: bridgeCoAPPort,
	})
	if err != nil {
		t.Fatal(err)
	}
	fr, err := BuildData(NetworkKey, 0x31, BridgeEUI, srcEUI[:5], WrapAppPayload(pkt, 700, nil))
	if err != nil {
		t.Fatal(err)
	}
	return fr.Bytes()
}

// TestCatalogResourcesNowKnown: the resources that the baseline logged as
// unknown-coap-resource are now in the catalog.
func TestCatalogResourcesNowKnown(t *testing.T) {
	for _, p := range []string{
		"d/RU1000000001/selftest/result",
		"d/VA1000000001/mnt",
		"time",
		"d/pair",
		"d/IB1000000001/neighbors",
		"h/7654321/z/2/overlay",
	} {
		if !knownResource(p) {
			t.Errorf("knownResource(%q) = false, want true (normalized %q)", p, normalizeResource(p))
		}
	}
	// The negatives from TestResourceCatalog must still be rejected: the new
	// leaves are distinctive and must not over-match.
	for _, p := range []string{"z/quux", "d/VA1000000006/nope", "h/7654321/z/5/wat"} {
		if knownResource(p) {
			t.Errorf("knownResource(%q) = true, want false", p)
		}
	}
}

// TestCatalogTagsNowDecoded: the tags with a confirmed meaning are catalogued and
// decode without falling back to the raw-hex label.
func TestCatalogTagsNowDecoded(t *testing.T) {
	cases := map[string]struct {
		val  string
		want string
	}{
		"0140": {"ffe2", "calib_offset=-30"},                // signed
		"0143": {"01", "trace_enable=1"},                    // u8
		"02b3": {"00", "offline_schedule=0"},                // u8 flag
		"0290": {"01", "child_lock=1"},                      // u8
		"0210": {"37336239623532", "fw_build_hash=73b9b52"}, // ASCII build hash
		"0260": {"494231303030303030303031", "serial_id=IB1000000001"},
		"0007": {"0badc0de0badc0de0badc0de0badc001", "auth_token=0badc0de0badc0de0badc0de0badc001"},
		"015e": {"0d00", "zone_table=0d00"},
		"6020": {"02", "zone_kind=hot_water"},       // the hot water zone
		"6260": {"01", "zone_channel_enable=1"},     // u8
		"6160": {"02", "zone_ctl_6160=2"},           // u8 control flag
		"0265": {"073d", "valve_travel_0265=1853"},  // u16 travel counter
		"0294": {"070b", "valve_travel_0294=1803"},  // u16 travel counter
		"028d": {"0032", "valve_travel_delta=50"},   // s16, positive error
		"0273": {"0920", "valve_ctr_0273=2336"},     // u16
		"0280": {"06fb", "valve_const_0280=1787"},   // u16
		"01a3": {"00000000", "device_fault_word=0"}, // u32, no fault
		"021a": {"0000", "dcfg_const_021a=0"},       // u16 d/config constant
		"0161": {"183d", "sen_0161=6205"},           // u16, one captured sample
	}
	for tag, c := range cases {
		info, ok := tags[tag]
		if !ok {
			t.Errorf("tag %s not in catalog", tag)
			continue
		}
		got := info.name + "=" + info.decode(mustHex(c.val))
		if got != c.want {
			t.Errorf("tag %s decode = %q, want %q", tag, got, c.want)
		}
	}
	if got := tags["6020"].decode(mustHex("01")); got != "heating" {
		t.Errorf("tag 6020 decode of a room zone = %q, want heating", got)
	}
	if got := tags["028d"].decode(mustHex("ffeb")); got != "-21" {
		t.Errorf("tag 028d decode of a negative error = %q, want -21", got)
	}
}

// TestReplayResourcesHandled: replay a handful of frames carrying resources that
// used to be logged as unknown, and assert the analyzer logs no unknown resource
// or tag now.
func TestReplayResourcesHandled(t *testing.T) {
	valve := mustHex(NodeByAny("bedroom").WireLE)
	ru := mustHex(NodeByAny("RU1000000001").WireLE)

	// TLV payload built only from newly-catalogued tags: none should log.
	tlvPayload := concat(
		TLV{Tag: "0140", Val: []byte{0xff, 0xe2}}.Encode(),
		TLV{Tag: "0143", Val: []byte{0x01}}.Encode(),
		TLV{Tag: "02b3", Val: []byte{0x00}}.Encode(),
		TLV{Tag: "0290", Val: []byte{0x01}}.Encode(),
	)
	// The motor telemetry and fault bodies the valve reports after a stroke.
	actPayload := concat(
		TLV{Tag: "0265", Val: []byte{0x07, 0x3d}}.Encode(),
		TLV{Tag: "0294", Val: []byte{0x07, 0x0b}}.Encode(),
		TLV{Tag: "028d", Val: []byte{0x00, 0x32}}.Encode(),
		TLV{Tag: "0273", Val: []byte{0x09, 0x20}}.Encode(),
		TLV{Tag: "027c", Val: []byte{0x09, 0x27}}.Encode(),
		TLV{Tag: "0280", Val: []byte{0x06, 0xfb}}.Encode(),
	)
	errPayload := TLV{Tag: "01a3", Val: []byte{0x00, 0x00, 0x00, 0x00}}.Encode()

	frames := [][]byte{
		uplinkFrame(t, valve, "0.01", []string{"time"}, nil, nil),
		uplinkFrame(t, valve, "0.02", []string{"d", "VA1000000001", "mnt"}, nil, nil),
		uplinkFrame(t, ru, "0.02", []string{"d", "RU1000000001", "selftest", "result"}, nil, nil),
		uplinkFrame(t, valve, "0.03", []string{"d", "VA1000000005", "config"}, []string{"id=10"}, tlvPayload),
		uplinkFrame(t, valve, "0.03", []string{"d", "VA1000000005", "act"}, nil, actPayload),
		uplinkFrame(t, valve, "0.03", []string{"d", "VA1000000005", "err"}, nil, errPayload),
	}

	log := NewUnhandledLogger(&bytes.Buffer{})
	an := NewAnalyzer(log)
	for _, fr := range frames {
		an.Add(fr)
	}
	counts := log.Counts()
	if n := countPrefix(counts, rsnUnknownRes); n != 0 {
		t.Errorf("%d unknown resources after replay:\n%s", n, log.Summary())
	}
	if n := countPrefix(counts, rsnUnknownTag); n != 0 {
		t.Errorf("%d unknown tags after replay:\n%s", n, log.Summary())
	}
}

// TestActiveGetCorrelation: an enqueued bridge GET rides out on the device's next
// frame, and the device's 2.05 response is filed under the queried resource.
func TestActiveGetCorrelation(t *testing.T) {
	bedroom := NodeByAny("bedroom")
	bedroomEUI := mustHex(bedroom.WireLE)
	raw, _, _ := firstUplink(t, func(fr *Frame, _ *AppPayload) bool {
		return hex.EncodeToString(fr.SrcEUI()) == bedroom.WireLE
	})

	e, _, _ := newTestEmu(t)
	if err := e.EnqueueGet("bedroom", "d/fw/state", []string{"d", "fw", "state"}, nil); err != nil {
		t.Fatal(err)
	}

	// The GET piggybacks on the next frame from the bedroom.
	get, ok := findAction(e.OnFrame(raw), ActData, "downlink get")
	if !ok {
		t.Fatal("the queued GET did not piggyback")
	}
	gf, err := ParseData(NetworkKey, get.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	gm := ParseApp(gf.Plaintext).CoAP
	if gm.Code != "0.01" {
		t.Errorf("active GET code = %s, want 0.01", gm.Code)
	}
	if gm.URI() != "d/fw/state" {
		t.Errorf("active GET URI = %q, want d/fw/state", gm.URI())
	}
	e.OnFrame(BuildEnhAck(gf.Seq, BridgeEUI, bedroomEUI, 0x0101))
	if e.Pending("bedroom") != 1 || !e.Queue("bedroom")[0].InFlight {
		t.Fatalf("Enhanced ACK closed active GET: queue=%+v", e.Queue("bedroom"))
	}
	if len(e.QueueHistory("bedroom")) != 0 {
		t.Fatalf("Enhanced ACK recorded active GET history: %+v", e.QueueHistory("bedroom"))
	}
	if err := e.EnqueueGet("bedroom", "d/fw/state", []string{"d", "fw", "state"}, nil); err != nil {
		t.Fatal(err)
	}
	if e.Pending("bedroom") != 1 {
		t.Fatalf("duplicate active GET pending=%d, want 1", e.Pending("bedroom"))
	}

	// Synthesize the device's 2.05 response with the same MID and feed it back.
	body := TLV{Tag: "0140", Val: []byte{0x00, 0x2a}}.Encode()
	wrong, err := BuildCoAPAck("2.05", gm.MID+1, gm.Token, body, bedroomEUI, BridgeEUI, coapPort, bridgeCoAPPort)
	if err != nil {
		t.Fatal(err)
	}
	wrongFrame, err := BuildData(NetworkKey, 0x3f, BridgeEUI, bedroomEUI[:5], WrapAppPayload(wrong, 899, nil))
	if err != nil {
		t.Fatal(err)
	}
	e.OnFrame(wrongFrame.Bytes())
	if e.Pending("bedroom") != 1 {
		t.Fatalf("wrong MID closed active GET: pending=%d", e.Pending("bedroom"))
	}
	resp, err := BuildCoAPAck("2.05", gm.MID, gm.Token, body, bedroomEUI, BridgeEUI, coapPort, bridgeCoAPPort)
	if err != nil {
		t.Fatal(err)
	}
	rf, err := BuildData(NetworkKey, 0x40, BridgeEUI, bedroomEUI[:5], WrapAppPayload(resp, 900, nil))
	if err != nil {
		t.Fatal(err)
	}
	e.OnFrame(rf.Bytes())

	got, ok := e.ActiveGetResult("bedroom", "d/fw/state")
	if !ok {
		t.Fatal("no result recorded for the active GET")
	}
	if !bytes.Equal(got, body) {
		t.Errorf("active GET result = %x, want %x", got, body)
	}
	if code, _, ok := e.ActiveGetResponse("bedroom", "d/fw/state"); !ok || code != "2.05" {
		t.Fatalf("active GET response code=%q ok=%t, want 2.05/true", code, ok)
	}
	if e.Pending("bedroom") != 0 || len(e.pendGet) != 0 {
		t.Fatalf("matching 2.05 left active GET pending=%d correlations=%v", e.Pending("bedroom"), e.pendGet)
	}
	history := e.QueueHistory("bedroom")
	if len(history) != 1 || history[0].Result != "delivered" || history[0].Reason != "coap-ack" {
		t.Fatalf("active GET history=%+v, want delivered/coap-ack", history)
	}
}

func TestActiveGetCurrentlogBlock2Reassembly(t *testing.T) {
	bedroom := NodeByAny("bedroom")
	bedroomEUI := mustHex(bedroom.WireLE)
	raw, _, _ := firstUplink(t, func(fr *Frame, _ *AppPayload) bool {
		return hex.EncodeToString(fr.SrcEUI()) == bedroom.WireLE
	})
	e, _, _ := newTestEmu(t)
	if err := e.EnqueueGet("bedroom", "d/currentlog", []string{"d", "currentlog"}, nil); err != nil {
		t.Fatal(err)
	}
	get, ok := findAction(e.OnFrame(raw), ActData, "downlink get")
	if !ok {
		t.Fatal("the queued GET did not piggyback")
	}
	first := activeGetMessage(t, get.Bytes)
	if first.URI() != "d/currentlog" {
		t.Fatalf("first URI=%q, want d/currentlog", first.URI())
	}

	body := bytes.Repeat([]byte{0xa5}, 260)
	second := activeGetBlockResponse(t, e, bedroomEUI, first.MID, first.Token, 0x0b, body[:128], 0x7a01)
	if second.MID == first.MID || !bytes.Equal(second.Token, first.Token) {
		t.Fatalf("block GET MID/token=%04x/%x, want new MID and %x", second.MID, second.Token, first.Token)
	}
	if block, ok := second.Option(23); !ok || !bytes.Equal(block.Val, []byte{0x13}) {
		t.Fatalf("second GET Block2=%v, want 13", block)
	}
	third := activeGetBlockResponse(t, e, bedroomEUI, second.MID, second.Token, 0x1b, body[128:256], 0x7a02)
	if block, ok := third.Option(23); !ok || !bytes.Equal(block.Val, []byte{0x23}) {
		t.Fatalf("third GET Block2=%v, want 23", block)
	}
	activeGetBlockResponse(t, e, bedroomEUI, third.MID, third.Token, 0x23, body[256:], 0x7a03)

	code, got, ok := e.ActiveGetResponse("bedroom", "d/currentlog")
	if !ok || code != "2.05" || !bytes.Equal(got, body) {
		t.Fatalf("active Block2 response=%q/%d/%t, want 2.05/%d/true", code, len(got), ok, len(body))
	}
	if e.Pending("bedroom") != 0 || len(e.pendGet) != 0 {
		t.Fatalf("Block2 result left pending=%d correlations=%v", e.Pending("bedroom"), e.pendGet)
	}
	history := e.QueueHistory("bedroom")
	if len(history) != 1 || history[0].Result != "delivered" || history[0].Reason != "coap-ack" || history[0].Tries != 1 {
		t.Fatalf("Block2 history=%+v, want one delivered/coap-ack attempt", history)
	}
}

// straddlingDoc is a synthetic TLV document of 299 bytes. The 63 leading 4-byte
// TLVs put the 43-byte one at offset 252, so it runs to 294 and crosses the
// 256-byte boundary between block 1 and block 2 of a 128-byte block-wise
// transfer: block 1 ends in the middle of it.
func straddlingDoc(value []byte) []byte {
	var doc []byte
	for i := 0; i < 63; i++ {
		doc = append(doc, TLV{Tag: "0143", Val: []byte{0x01}}.Encode()...)
	}
	doc = append(doc, TLV{Tag: "0210", Val: value}.Encode()...)
	return append(doc, TLV{Tag: "02b3", Val: []byte{0x00}}.Encode()...)
}

// TestBlock2CachesOnlyTheWholeBody: a block-wise response reaches the TLV cache
// once, reassembled. Parsing a single block would file the halves of the TLV the
// block boundary cuts.
func TestBlock2CachesOnlyTheWholeBody(t *testing.T) {
	bedroom := NodeByAny("bedroom")
	bedroomEUI := mustHex(bedroom.WireLE)
	raw, _, _ := firstUplink(t, func(fr *Frame, _ *AppPayload) bool {
		return hex.EncodeToString(fr.SrcEUI()) == bedroom.WireLE
	})
	e, log, _ := newTestEmu(t)
	if err := e.EnqueueGet("bedroom", "d/currentlog", []string{"d", "currentlog"}, nil); err != nil {
		t.Fatal(err)
	}
	get, ok := findAction(e.OnFrame(raw), ActData, "downlink get")
	if !ok {
		t.Fatal("the queued GET did not piggyback")
	}
	first := activeGetMessage(t, get.Bytes)

	value := bytes.Repeat([]byte{'a'}, 40)
	doc := straddlingDoc(value)
	if len(doc) != 299 {
		t.Fatalf("document is %d bytes, want 299", len(doc))
	}
	ns := e.nodes[hexEUI(bedroomEUI)]

	second := activeGetBlockResponse(t, e, bedroomEUI, first.MID, first.Token, 0x0b, doc[:128], 0x7b01)
	if len(ns.raw["0210"]) != 0 || e.block2.Open() != 1 {
		t.Fatalf("block 0 cached %x with %d transfers open", ns.raw["0210"], e.block2.Open())
	}
	third := activeGetBlockResponse(t, e, bedroomEUI, second.MID, second.Token, 0x1b, doc[128:256], 0x7b02)
	if len(ns.raw["0210"]) != 0 {
		t.Fatalf("block 1 cached the cut TLV as %x", ns.raw["0210"])
	}
	activeGetBlockResponse(t, e, bedroomEUI, third.MID, third.Token, 0x23, doc[256:], 0x7b03)

	if !bytes.Equal(ns.raw["0210"], value) {
		t.Errorf("cached 0210 = %x, want %x", ns.raw["0210"], value)
	}
	if e.block2.Open() != 0 {
		t.Errorf("open Block2 transfers=%d after the last block, want 0", e.block2.Open())
	}
	if n := countPrefix(log.Counts(), rsnBlock2Dropped); n != 0 {
		t.Errorf("%d blocks dropped in an ordered transfer: %s", n, log.Summary())
	}
	if code, got, ok := e.ActiveGetResponse("bedroom", "d/currentlog"); !ok || code != "2.05" || !bytes.Equal(got, doc) {
		t.Errorf("active GET response=%q/%d/%t, want 2.05/%d/true", code, len(got), ok, len(doc))
	}
}

// TestBlock2OrphanBlockDropped: a block that arrives without its predecessors is
// logged and discarded, never parsed.
func TestBlock2OrphanBlockDropped(t *testing.T) {
	bedroomEUI := mustHex(NodeByAny("bedroom").WireLE)
	e, log, _ := newTestEmu(t)
	pkt, err := BuildCoAP(CoAPReq{
		Type: "ACK", Code: "2.05", MID: 0x4242, Token: []byte{0x55},
		Options: []CoAPOption{{Num: 23, Val: []byte{0x10}}}, // block 1, last, szx 0
		Payload: TLV{Tag: "0143", Val: []byte{0x01}}.Encode(),
		SrcEUI:  bedroomEUI, DstEUI: BridgeEUI, SrcPort: coapPort, DstPort: bridgeCoAPPort,
	})
	if err != nil {
		t.Fatal(err)
	}
	frame, err := BuildData(NetworkKey, 0x51, BridgeEUI, bedroomEUI[:5], WrapAppPayload(pkt, 901, nil))
	if err != nil {
		t.Fatal(err)
	}
	e.OnFrame(frame.Bytes())

	if v := e.nodes[hexEUI(bedroomEUI)].raw["0143"]; len(v) != 0 {
		t.Errorf("orphan block cached 0143=%x", v)
	}
	if n := countPrefix(log.Counts(), rsnBlock2Dropped); n != 1 {
		t.Errorf("orphan block logged %d times, want 1: %s", n, log.Summary())
	}
	if e.block2.Open() != 0 {
		t.Errorf("orphan block opened %d transfers, want 0", e.block2.Open())
	}
}

func activeGetBlockResponse(t *testing.T, e *EmuState, dev []byte, mid uint16, token []byte, block byte, payload []byte, tag uint16) *CoAPMessage {
	t.Helper()
	pkt, err := BuildCoAP(CoAPReq{
		Type: "ACK", Code: "2.05", MID: mid, Token: token,
		Options: []CoAPOption{{Num: 23, Val: []byte{block}}}, Payload: payload,
		SrcEUI: dev, DstEUI: BridgeEUI, SrcPort: coapPort, DstPort: bridgeCoAPPort,
	})
	if err != nil {
		t.Fatal(err)
	}
	fragments := [][]byte{pkt}
	if headerLen+len(WrapAppPayload(pkt, 0, nil))+micLen > 127 {
		fragments, err = Fragment6LoWPANDatagram(pkt, tag)
		if err != nil {
			t.Fatal(err)
		}
	}
	var actions []Action
	for i, fragment := range fragments {
		frame, err := BuildDataPending(NetworkKey, byte(0x70+i), BridgeEUI, dev[:5], WrapAppPayload(fragment, uint32(tag)+uint32(i), nil))
		if err != nil {
			t.Fatal(err)
		}
		actions = e.OnFrame(frame.Bytes())
		if i != len(fragments)-1 && e.Pending("bedroom") != 1 {
			t.Fatalf("fragment %d changed active GET pending=%d", i, e.Pending("bedroom"))
		}
	}
	if block&8 == 0 {
		return nil
	}
	a, ok := findAction(actions, ActData, "downlink get")
	if !ok {
		t.Fatalf("block %02x did not queue the follow-up GET: %v", block, reasons(actions))
	}
	return activeGetMessage(t, a.Bytes)
}

func activeGetMessage(t *testing.T, raw []byte) *CoAPMessage {
	t.Helper()
	f, err := ParseData(NetworkKey, raw)
	if err != nil {
		t.Fatal(err)
	}
	app := ParseApp(f.Plaintext)
	if app == nil || app.CoAP == nil {
		t.Fatalf("active GET frame has no CoAP: %x", raw)
	}
	return app.CoAP
}

func TestActiveGetErrorResponseCorrelation(t *testing.T) {
	bedroom := NodeByAny("bedroom")
	bedroomEUI := mustHex(bedroom.WireLE)
	raw, _, _ := firstUplink(t, func(fr *Frame, _ *AppPayload) bool {
		return hex.EncodeToString(fr.SrcEUI()) == bedroom.WireLE
	})
	e, _, _ := newTestEmu(t)
	if err := e.EnqueueGet("bedroom", "d/currentlog", []string{"d", "currentlog"}, nil); err != nil {
		t.Fatal(err)
	}
	get, ok := findAction(e.OnFrame(raw), ActData, "downlink get")
	if !ok {
		t.Fatal("the queued GET did not piggyback")
	}
	gf, err := ParseData(NetworkKey, get.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	gm := ParseApp(gf.Plaintext).CoAP
	resp, err := BuildCoAPAck("4.03", gm.MID, gm.Token, nil, bedroomEUI, BridgeEUI, coapPort, bridgeCoAPPort)
	if err != nil {
		t.Fatal(err)
	}
	rf, err := BuildData(NetworkKey, 0x40, BridgeEUI, bedroomEUI[:5], WrapAppPayload(resp, 900, nil))
	if err != nil {
		t.Fatal(err)
	}
	e.OnFrame(rf.Bytes())
	code, body, ok := e.ActiveGetResponse("bedroom", "d/currentlog")
	if !ok || code != "4.03" || len(body) != 0 {
		t.Fatalf("active GET response=%q/%x ok=%t, want 4.03/empty/true", code, body, ok)
	}
	if e.Pending("bedroom") != 0 || len(e.pendGet) != 0 {
		t.Fatalf("4.03 left active GET pending=%d correlations=%v", e.Pending("bedroom"), e.pendGet)
	}
	history := e.QueueHistory("bedroom")
	if len(history) != 1 || history[0].Result != "delivered" || history[0].Reason != "coap-ack" {
		t.Fatalf("active GET history=%+v, want delivered/coap-ack", history)
	}
}

func TestActiveGetRejectsMismatchedResponse(t *testing.T) {
	bedroom := NodeByAny("bedroom")
	other := NodeByAny("bath")
	bedroomEUI := mustHex(bedroom.WireLE)
	otherEUI := mustHex(other.WireLE)
	raw, _, _ := firstUplink(t, func(fr *Frame, _ *AppPayload) bool {
		return hex.EncodeToString(fr.SrcEUI()) == bedroom.WireLE
	})
	e, _, _ := newTestEmu(t)
	if err := e.EnqueueGet("bedroom", "d/fw/state", []string{"d", "fw", "state"}, nil); err != nil {
		t.Fatal(err)
	}
	get, ok := findAction(e.OnFrame(raw), ActData, "downlink get")
	if !ok {
		t.Fatal("the queued GET did not piggyback")
	}
	gf, err := ParseData(NetworkKey, get.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	gm := ParseApp(gf.Plaintext).CoAP
	cases := []struct {
		name string
		body []byte
		src  []byte
		mid  uint16
		typ  string
	}{
		{name: "source", src: otherEUI, mid: gm.MID, typ: "ACK"},
		{name: "mid", src: bedroomEUI, mid: gm.MID + 1, typ: "ACK"},
		{name: "type", src: bedroomEUI, mid: gm.MID, typ: "CON"},
	}
	for i, tc := range cases {
		pkt, err := BuildCoAP(CoAPReq{Type: tc.typ, Code: "4.03", MID: tc.mid, Token: gm.Token, Payload: tc.body, SrcEUI: tc.src, DstEUI: BridgeEUI, SrcPort: coapPort, DstPort: bridgeCoAPPort})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		frame, err := BuildData(NetworkKey, byte(0x40+i), BridgeEUI, tc.src[:5], WrapAppPayload(pkt, uint32(900+i), nil))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		e.OnFrame(frame.Bytes())
		if e.Pending("bedroom") != 1 || len(e.pendGet) != 1 {
			t.Fatalf("%s closed active GET: pending=%d correlations=%v", tc.name, e.Pending("bedroom"), e.pendGet)
		}
	}
}

func TestEnqueueGetDeduplicatesPending(t *testing.T) {
	e, _, _ := newTestEmu(t)
	for range 2 {
		if err := e.EnqueueGet("bedroom", "d/fw/state", []string{"d", "fw", "state"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := e.Pending("bedroom"); got != 1 {
		t.Fatalf("pending=%d, want 1", got)
	}
}

func TestActiveGetRetryLimitClearsCorrelation(t *testing.T) {
	e, _, _ := newTestEmu(t)
	now := time.Unix(100, 0)
	e.cfg.Now = func() time.Time { return now }
	wire, label, err := e.resolve("bedroom")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.EnqueueGet("bedroom", "d/fw/state", []string{"d", "fw", "state"}, nil); err != nil {
		t.Fatal(err)
	}
	oldMID := e.Queue("bedroom")[0].MID
	for range maxDownlinkTries {
		if _, ok := e.nextDownlink(wire, label); !ok {
			t.Fatal("active GET retry missing")
		}
		now = now.Add(activeGetRetryBackoff)
	}
	if _, ok := e.nextDownlink(wire, label); ok {
		t.Fatal("retry-limit active GET produced action")
	}
	if e.Pending("bedroom") != 0 || len(e.pendGet) != 0 {
		t.Fatalf("retry-limit left active GET pending=%d correlations=%v", e.Pending("bedroom"), e.pendGet)
	}
	if err := e.EnqueueGet("bedroom", "d/fw/state", []string{"d", "fw", "state"}, nil); err != nil {
		t.Fatal(err)
	}
	if e.Pending("bedroom") != 1 || e.Queue("bedroom")[0].MID == oldMID {
		t.Fatalf("new active GET queue=%+v, want a new request", e.Queue("bedroom"))
	}
}

func TestActiveGetRetryIsPacedAndKeepsMID(t *testing.T) {
	e, _, _ := newTestEmu(t)
	now := time.Unix(100, 0)
	e.cfg.Now = func() time.Time { return now }
	wire, label, err := e.resolve("bedroom")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.EnqueueGet("bedroom", "d/fw/state", []string{"d", "fw", "state"}, nil); err != nil {
		t.Fatal(err)
	}
	first, ok := e.nextDownlink(wire, label)
	if !ok || len(first) != 1 || !first[0].WaitResponse {
		t.Fatalf("first=%+v ok=%t", first, ok)
	}
	mid := e.Queue("bedroom")[0].MID
	now = now.Add(activeGetRetryBackoff - time.Nanosecond)
	if _, ok := e.nextDownlink(wire, label); ok {
		t.Fatal("active GET retried before backoff")
	}
	now = now.Add(time.Nanosecond)
	second, ok := e.nextDownlink(wire, label)
	if !ok || len(second) != 1 || !second[0].WaitResponse {
		t.Fatalf("second=%+v ok=%t", second, ok)
	}
	if got := e.Queue("bedroom")[0].MID; got != mid {
		t.Fatalf("MID=%04x, want %04x", got, mid)
	}
}

func TestRebootRequiresMatchingCoAPResponse(t *testing.T) {
	bedroom := NodeByAny("bedroom")
	bedroomEUI := mustHex(bedroom.WireLE)
	otherEUI := []byte{0x67, 0x45, 0x23, 0x31, 0x07, 0xc5, 0x1b, 0x00}
	e, _, _ := newTestEmu(t)
	wire, label, err := e.resolve("bedroom")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.EnqueueReboot("bedroom", 5); err != nil {
		t.Fatal(err)
	}
	first, ok := e.nextDownlink(wire, label)
	if !ok || len(first) != 1 || !first[0].WaitResponse {
		t.Fatalf("first=%+v ok=%t", first, ok)
	}
	firstData, err := ParseData(NetworkKey, first[0].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	firstReq := ParseApp(firstData.Plaintext).CoAP
	e.OnFrame(BuildEnhAck(firstData.Seq, BridgeEUI, bedroomEUI, 0x0101))
	if e.Pending("bedroom") != 1 {
		t.Fatalf("Enhanced ACK completed reboot: pending=%d", e.Pending("bedroom"))
	}

	second, ok := e.nextDownlink(wire, label)
	if !ok || len(second) != 1 || !second[0].WaitResponse {
		t.Fatalf("second=%+v ok=%t", second, ok)
	}
	secondData, err := ParseData(NetworkKey, second[0].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	secondReq := ParseApp(secondData.Plaintext).CoAP
	if secondReq.MID != firstReq.MID || !bytes.Equal(secondReq.Token, firstReq.Token) {
		t.Fatalf("retry MID/token=%04x/%x, want %04x/%x", secondReq.MID, secondReq.Token, firstReq.MID, firstReq.Token)
	}
	e.HoldQueuedDownlink("bedroom", time.Hour)

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
	feed(otherEUI, "2.04", firstReq.MID, firstReq.Token, 0x40)
	if e.Pending("bedroom") != 1 {
		t.Fatal("wrong source completed reboot")
	}
	feed(bedroomEUI, "2.04", firstReq.MID+1, firstReq.Token, 0x41)
	if e.Pending("bedroom") != 1 {
		t.Fatal("wrong MID completed reboot")
	}
	feed(bedroomEUI, "2.05", firstReq.MID, firstReq.Token, 0x42)
	if e.Pending("bedroom") != 1 {
		t.Fatal("wrong code completed reboot")
	}
	if d := e.queue[hexEUI(bedroomEUI)][0]; !d.requireCoAP || !bytes.Equal(d.responseToken, firstReq.Token) {
		t.Fatalf("reboot confirmation=%+v token=%x, want required/%x", d, d.responseToken, firstReq.Token)
	}
	wrongToken := []byte{0x00}
	feed(bedroomEUI, "2.04", firstReq.MID, wrongToken, 0x43)
	if e.Pending("bedroom") != 1 {
		t.Fatalf("wrong reboot response completed queue: pending=%d", e.Pending("bedroom"))
	}
	feed(bedroomEUI, "2.04", firstReq.MID, firstReq.Token, 0x44)
	if e.Pending("bedroom") != 0 {
		t.Fatalf("matching 2.04 left reboot pending=%d", e.Pending("bedroom"))
	}
	history := e.QueueHistory("bedroom")
	if len(history) != 1 || history[0].Result != "delivered" || history[0].Reason != "coap-ack" {
		t.Fatalf("history=%+v, want delivered coap-ack", history)
	}

	retry, _, _ := newTestEmu(t)
	if err := retry.EnqueueReboot("bedroom", 5); err != nil {
		t.Fatal(err)
	}
	for range maxDownlinkTries {
		if _, ok := retry.nextDownlink(wire, label); !ok {
			t.Fatal("reboot retry missing")
		}
	}
	if _, ok := retry.nextDownlink(wire, label); ok || retry.Pending("bedroom") != 0 {
		t.Fatalf("retry-limit left reboot queue=%+v", retry.Queue("bedroom"))
	}
	history = retry.QueueHistory("bedroom")
	if len(history) != 1 || history[0].Result != "dropped" || history[0].Reason != "retry-limit" {
		t.Fatalf("retry history=%+v, want dropped retry-limit", history)
	}
}

// TestEnqueueConfigWrite: a d/config store write carries the set fields as their
// store TLVs, on a CoAP 0.03 PUT to d/config.
func TestEnqueueConfigWrite(t *testing.T) {
	bedroom := NodeByAny("bedroom")
	raw, _, _ := firstUplink(t, func(fr *Frame, _ *AppPayload) bool {
		return hex.EncodeToString(fr.SrcEUI()) == bedroom.WireLE
	})

	e, _, _ := newTestEmu(t)
	trace, offlineScheduleEnabled := uint8(1), false
	if err := e.EnqueueConfig("bedroom", 10, ConfigWrite{TraceEnable: &trace, OfflineScheduleEnabled: &offlineScheduleEnabled}); err != nil {
		t.Fatal(err)
	}

	dl, ok := findAction(e.OnFrame(raw), ActData, "downlink config")
	if !ok {
		t.Fatal("the queued config write did not piggyback")
	}
	df, err := ParseData(NetworkKey, dl.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	app := ParseApp(df.Plaintext)
	if app.CoAP.Type != "CON" || app.CoAP.Code != "0.03" {
		t.Errorf("config write = %s %s, want CON 0.03", app.CoAP.Type, app.CoAP.Code)
	}
	if app.CoAP.URI() != "d/config" {
		t.Errorf("uri = %q, want d/config: the write carries no query", app.CoAP.URI())
	}
	got := map[string]string{}
	for _, tl := range app.TLVs {
		got[tl.Tag] = hex.EncodeToString(tl.Val)
	}
	for tag, want := range map[string]string{"0143": "01", "02b3": "00"} {
		if got[tag] != want {
			t.Errorf("config TLV %s = %q, want %q (all: %v)", tag, got[tag], want, got)
		}
	}
}

// TestConfigWriteRequiresMatchingCoAP204 covers the unfragmented H11 shape:
// the MAC ACK refreshes the link only; d/config completes on its final CoAP ACK.
func TestConfigWriteRequiresMatchingCoAP204(t *testing.T) {
	device := NodeByAny("0a06")
	if device == nil {
		t.Fatal("0a06 not in inventory")
	}
	deviceEUI := mustHex(device.WireLE)
	otherEUI := []byte{0x67, 0x45, 0x23, 0x31, 0x07, 0xc5, 0x1b, 0x00}
	e, _, _ := newTestEmu(t)
	offset := 120
	if err := e.EnqueueConfig("0a06", 5, ConfigWrite{
		UTCOffsetMin: &offset,
		NodeTable:    []NodeTableRec{{Role: 5, Node: 6}},
	}); err != nil {
		t.Fatal(err)
	}
	wire, label, err := e.resolve("0a06")
	if err != nil {
		t.Fatal(err)
	}
	first, ok := e.nextDownlink(wire, label)
	if !ok || len(first) != 1 || !first[0].WaitResponse || !first[0].CoAPTransaction {
		t.Fatalf("first=%+v ok=%t", first, ok)
	}
	firstData, err := ParseData(NetworkKey, first[0].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	firstReq := ParseApp(firstData.Plaintext).CoAP
	if firstReq.Type != "CON" || firstReq.Code != "0.03" || firstReq.URI() != "d/config" {
		t.Fatalf("first request=%+v", firstReq)
	}
	got := map[string]string{}
	for _, tlv := range parseTLVList(firstReq.Payload) {
		got[tlv.Tag] = hex.EncodeToString(tlv.Val)
	}
	if got["015e"] != "0506" || got["02b2"] != "0078" || len(mustHex(got["015a"])) != 8 {
		t.Fatalf("H11 config TLVs=%v, want 015e=0506 02b2=0078 generated 015a", got)
	}
	e.OnFrame(BuildEnhAck(firstData.Seq, BridgeEUI, deviceEUI, 0x0101))
	if e.Pending("0a06") != 1 {
		t.Fatalf("Enhanced ACK completed config: pending=%d", e.Pending("0a06"))
	}

	second, ok := e.nextDownlink(wire, label)
	if !ok || len(second) != 1 || !second[0].WaitResponse {
		t.Fatalf("second=%+v ok=%t", second, ok)
	}
	secondData, err := ParseData(NetworkKey, second[0].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	secondReq := ParseApp(secondData.Plaintext).CoAP
	if secondReq.MID != firstReq.MID || !bytes.Equal(secondReq.Token, firstReq.Token) {
		t.Fatalf("retry MID/token=%04x/%x, want %04x/%x", secondReq.MID, secondReq.Token, firstReq.MID, firstReq.Token)
	}
	e.HoldQueuedDownlink("0a06", time.Hour)

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
	feed(otherEUI, "2.04", firstReq.MID, firstReq.Token, 0x40)
	if e.Pending("0a06") != 1 {
		t.Fatal("wrong source completed config")
	}
	feed(deviceEUI, "2.04", firstReq.MID+1, firstReq.Token, 0x41)
	if e.Pending("0a06") != 1 {
		t.Fatal("wrong MID completed config")
	}
	feed(deviceEUI, "2.04", firstReq.MID, []byte{0x00}, 0x42)
	if e.Pending("0a06") != 1 {
		t.Fatal("wrong token completed config")
	}
	feed(deviceEUI, "2.04", firstReq.MID, firstReq.Token, 0x43)
	if e.Pending("0a06") != 0 {
		t.Fatalf("matching 2.04 left config pending=%d", e.Pending("0a06"))
	}
	history := e.QueueHistory("0a06")
	if len(history) != 1 || history[0].Result != "delivered" || history[0].Reason != "coap-ack" {
		t.Fatalf("history=%+v, want delivered coap-ack", history)
	}

	if err := e.EnqueueConfig("0a06", 5, ConfigWrite{NodeTable: []NodeTableRec{{Role: 5, Node: 5}}}); err != nil {
		t.Fatal(err)
	}
	third, ok := e.nextDownlink(wire, label)
	if !ok || len(third) != 1 {
		t.Fatalf("third=%+v ok=%t", third, ok)
	}
	thirdData, err := ParseData(NetworkKey, third[0].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	thirdReq := ParseApp(thirdData.Plaintext).CoAP
	feed(deviceEUI, "4.00", thirdReq.MID, thirdReq.Token, 0x44)
	if e.Pending("0a06") != 0 {
		t.Fatalf("4.xx left config pending=%d", e.Pending("0a06"))
	}
	history = e.QueueHistory("0a06")
	if len(history) != 2 || history[0].Result != "dropped" || history[0].Reason != "coap-4.00" {
		t.Fatalf("4.xx history=%+v, want dropped coap-4.00", history)
	}
}
