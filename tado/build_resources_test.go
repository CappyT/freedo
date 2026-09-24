package tado

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// parseBuilt decodes a freshly built CoAP datagram through the capture path.
func parseBuilt(t *testing.T, pkt []byte) *CoAPMessage {
	t.Helper()
	m, err := ParseCoAP(WrapAppPayload(pkt, 1, nil)[8:])
	if err != nil {
		t.Fatalf("built packet does not parse: %v", err)
	}
	return m
}

func testDstEUI(t *testing.T) []byte {
	t.Helper()
	dst, err := hex.DecodeString("02a6cefa07c51b00")
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

// TestBuildChildLockCoAP pins the d/lock write to the captured shape: the bare
// path, no query, Uri-Port ffff and Content-Format 42 before the 0290 payload.
func TestBuildChildLockCoAP(t *testing.T) {
	dst := testDstEUI(t)
	pkt, err := BuildChildLockCoAP(true, 0x1234, []byte{0x20}, BridgeEUI, dst)
	if err != nil {
		t.Fatal(err)
	}
	m := parseBuilt(t, pkt)
	if m.Type != "CON" || m.Code != "0.03" {
		t.Errorf("type/code = %s %s, want CON 0.03", m.Type, m.Code)
	}
	if m.Path != "d/lock" || m.Query != "" {
		t.Errorf("uri = %q, want d/lock with no query", m.URI())
	}
	if m.SrcPort != 4005 || m.DstPort != 5683 {
		t.Errorf("ports %d->%d, want 4005->5683", m.SrcPort, m.DstPort)
	}
	if o, ok := m.Option(12); !ok || !bytes.Equal(o.Val, []byte{42}) {
		t.Errorf("content-format = %v/%x, want 12/2a", ok, o.Val)
	}
	if o, ok := m.Option(7); !ok || !bytes.Equal(o.Val, []byte{0xff, 0xff}) {
		t.Errorf("uri-port = %v/%x, want 7/ffff", ok, o.Val)
	}
	if want := []byte{0x02, 0x90, 0x01, 0x01}; !bytes.Equal(m.Payload, want) {
		t.Errorf("payload = %x, want %x", m.Payload, want)
	}
	want := strings.Join([]string{
		"41031234", "20", // CON 0.03, MID, 1-byte token
		"72ffff",         // 7 Uri-Port ffff
		"4164",           // 11 Uri-Path "d"
		"046c6f636b",     // 11 Uri-Path "lock"
		"112a",           // 12 Content-Format 42
		"ff", "02900101", // marker, 0290=01
	}, "")
	if got := hex.EncodeToString(pkt[9:]); got != want {
		t.Errorf("coap = %s, want %s", got, want)
	}
	if err := checkInlineUDP(BridgeEUI, dst, pkt); err != nil {
		t.Error(err)
	}

	off, err := BuildChildLockCoAP(false, 0x1234, []byte{0x20}, BridgeEUI, dst)
	if err != nil {
		t.Fatal(err)
	}
	if want := []byte{0x02, 0x90, 0x01, 0x00}; !bytes.Equal(parseBuilt(t, off).Payload, want) {
		t.Errorf("lock off payload = %x, want %x", parseBuilt(t, off).Payload, want)
	}
}

func TestBuildIdentifyRebootNetcmd(t *testing.T) {
	dst := testDstEUI(t)

	pkt, err := BuildIdentifyCoAP(0x0101, []byte{0x01}, BridgeEUI, dst)
	if err != nil {
		t.Fatal(err)
	}
	m := parseBuilt(t, pkt)
	if m.Path != "d/identify" || m.Query != "" {
		t.Errorf("uri = %q, want d/identify with no query", m.URI())
	}
	if len(m.Payload) != 0 {
		t.Errorf("identify payload = %x, want empty", m.Payload)
	}
	if o, ok := m.Option(12); !ok || !bytes.Equal(o.Val, []byte{42}) {
		t.Errorf("identify content-format = %v/%x, want 12/2a", ok, o.Val)
	}
	// RFC 7252 3: an empty payload ends the message after the last option, so
	// the 0xff marker must stay out.
	wantIdentify := strings.Join([]string{
		"41030101", "01", // CON 0.03, MID, 1-byte token
		"72ffff",             // 7 Uri-Port ffff
		"4164",               // 11 Uri-Path "d"
		"086964656e74696679", // 11 Uri-Path "identify"
		"112a",               // 12 Content-Format 42
	}, "")
	if got := hex.EncodeToString(pkt[9:]); got != wantIdentify {
		t.Errorf("identify coap = %s, want %s", got, wantIdentify)
	}

	pkt, err = BuildCalibrateCoAP(0x0104, []byte{0x04}, BridgeEUI, dst)
	if err != nil {
		t.Fatal(err)
	}
	wantCalibrate := strings.Join([]string{
		"41030104", "04", // CON 0.03, MID, 1-byte token
		"72ffff",               // 7 Uri-Port ffff
		"4164",                 // 11 Uri-Path "d"
		"0963616c696272617465", // 11 Uri-Path "calibrate"
		"112a",                 // 12 Content-Format 42
	}, "")
	if got := hex.EncodeToString(pkt[9:]); got != wantCalibrate {
		t.Errorf("calibrate coap = %s, want %s", got, wantCalibrate)
	}

	pkt, err = BuildRebootCoAP(5, 0x0102, []byte{0x02}, BridgeEUI, dst)
	if err != nil {
		t.Fatal(err)
	}
	m = parseBuilt(t, pkt)
	if m.Type != "CON" || m.Code != "0.02" {
		t.Errorf("reboot type/code = %s %s, want CON 0.02", m.Type, m.Code)
	}
	if m.Path != "d/reboot" || m.Query != "id=5" {
		t.Errorf("uri = %q, want d/reboot?id=5", m.URI())
	}
	if m.MID != 0x0102 || !bytes.Equal(m.Token, []byte{0x02}) {
		t.Errorf("reboot MID/token = %04x/%x, want 0102/02", m.MID, m.Token)
	}
	if len(m.Payload) != 0 {
		t.Errorf("reboot payload = %x, want empty", m.Payload)
	}
	if _, ok := m.Option(12); ok {
		t.Error("reboot should carry no content-format")
	}
	if o, ok := m.Option(7); !ok || !bytes.Equal(o.Val, []byte{0xff, 0xff}) {
		t.Errorf("reboot uri-port = %v/%x, want 7/ffff", ok, o.Val)
	}

	pkt, err = BuildNetcmdCoAP(1007, 0x0103, []byte{0x03}, BridgeEUI, dst)
	if err != nil {
		t.Fatal(err)
	}
	m = parseBuilt(t, pkt)
	if m.Type != "CON" || m.Code != "0.03" {
		t.Errorf("netcmd type/code = %s %s, want CON 0.03", m.Type, m.Code)
	}
	if m.Path != "d/test/v1" || m.Query != "t=1007" {
		t.Errorf("uri = %q, want d/test/v1?t=1007", m.URI())
	}
	if len(m.Payload) != 0 {
		t.Errorf("netcmd payload = %x, want empty", m.Payload)
	}
	if err := checkInlineUDP(BridgeEUI, dst, pkt); err != nil {
		t.Error(err)
	}
}

// TestBuildPairCoAP pins the d/pair write to the coordinator shape: a CON POST
// with an empty token, Content-Format 42 and option 2048 in ascending order, no
// Uri-Port, and the plaintext key as tag8 0x12 with a one-byte length.
func TestBuildPairCoAP(t *testing.T) {
	dst := testDstEUI(t)
	key := make([]byte, 16)
	for i := range key {
		key[i] = byte(i)
	}

	pkt, err := BuildPairCoAP(key, 0x2222, BridgeEUI, dst)
	if err != nil {
		t.Fatal(err)
	}
	m := parseBuilt(t, pkt)
	if m.Type != "CON" || m.Code != "0.02" {
		t.Errorf("type/code = %s %s, want CON 0.02", m.Type, m.Code)
	}
	if m.Path != "d/pair" || m.Query != "" {
		t.Errorf("uri = %q, want d/pair with no query", m.URI())
	}
	if len(m.Token) != 0 {
		t.Errorf("token = %x, want empty", m.Token)
	}
	var session []byte
	for _, o := range m.Options {
		switch o.Num {
		case 7:
			t.Errorf("Uri-Port = %x, the coordinator sends none", o.Val)
		case coapSessionOption:
			session = o.Val
		}
	}
	if len(session) != pairSessionLen {
		t.Fatalf("option 2048 = %x, want %d bytes", session, pairSessionLen)
	}

	// Option 2048 follows option 12, so its delta is 2036 and needs the 16-bit
	// extension: nibble 14 and 2036-269 = 0x06e7.
	want := []byte{0x40, 0x02, 0x22, 0x22} // ver 1, CON, no token; POST; MID
	want = append(want, 0xb1, 'd')         // 11 Uri-Path
	want = append(want, 0x04, 'p', 'a', 'i', 'r')
	want = append(want, 0x11, 42) // 12 Content-Format
	want = append(want, 0xe8, 0x06, 0xe7)
	want = append(want, session...)
	want = append(want, 0xff, 0x12, 0x10)
	want = append(want, key...)
	if !bytes.Equal(m.Raw, want) {
		t.Errorf("message = %x, want %x", m.Raw, want)
	}
	if len(m.Payload) != 18 {
		t.Errorf("payload is %d bytes, want 18", len(m.Payload))
	}
	if m.SrcPort != 5683 || m.DstPort != 5683 {
		t.Errorf("ports %d->%d, want 5683->5683 as captured", m.SrcPort, m.DstPort)
	}
	if err := checkInlineUDP(BridgeEUI, dst, pkt); err != nil {
		t.Error(err)
	}
	if len(PairingKey) != 16 {
		t.Errorf("PairingKey is %d bytes, want 16", len(PairingKey))
	}

	again, err := BuildPairCoAP(key, 0x2222, BridgeEUI, dst)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(parseBuilt(t, again).Raw, m.Raw) {
		t.Error("option 2048 must carry a new value on each request")
	}

	if _, err := BuildPairCoAP(key[:15], 0x2222, BridgeEUI, dst); err == nil {
		t.Error("a 15-byte network key should be rejected")
	}
}

func TestBuildBoostCoAP(t *testing.T) {
	dst := testDstEUI(t)
	for _, tc := range []struct {
		name    string
		enabled bool
		payload string
		tags    []string
	}{
		{
			name: "start", enabled: true,
			payload: "61600102618001006020010161e00100624001036260010162800209c462e001006440020001",
			tags:    []string{"6160", "6180", "6020", "61e0", "6240", "6260", "6280", "62e0", "6440"},
		},
		{
			name:    "restore",
			payload: "61600102618001006020010161e001006240010062e001006440020001",
			tags:    []string{"6160", "6180", "6020", "61e0", "6240", "62e0", "6440"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pkt, err := BuildBoostCoAP(BoostCoAP{
				ID: 7, Enabled: tc.enabled, NewOverlay: true, MID: 0x1234,
				Token: []byte{0x20}, SrcEUI: BridgeEUI, DstEUI: dst,
			})
			if err != nil {
				t.Fatal(err)
			}
			m := parseBuilt(t, pkt)
			if m.Type != "CON" || m.Code != "0.03" {
				t.Errorf("type/code = %s %s, want CON 0.03", m.Type, m.Code)
			}
			if m.Path != "z/s" || m.Query != "id=7" {
				t.Errorf("uri = %q, want z/s?id=7", m.URI())
			}
			if o, ok := m.Option(7); !ok || !bytes.Equal(o.Val, []byte{0xff, 0xff}) {
				t.Errorf("uri-port = %v/%x, want 7/ffff", ok, o.Val)
			}
			if o, ok := m.Option(12); !ok || !bytes.Equal(o.Val, []byte{42}) {
				t.Errorf("content-format = %v/%x, want 12/2a", ok, o.Val)
			}
			want, err := hex.DecodeString(tc.payload)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(m.Payload, want) {
				t.Errorf("payload = %x, want %x", m.Payload, want)
			}
			var tags []string
			for _, tlv := range parseTLVList(m.Payload) {
				tags = append(tags, tlv.Tag)
			}
			if strings.Join(tags, " ") != strings.Join(tc.tags, " ") {
				t.Errorf("tag order = %v, want %v", tags, tc.tags)
			}
			if err := checkInlineUDP(BridgeEUI, dst, pkt); err != nil {
				t.Error(err)
			}
		})
	}
}

// capturedFlowConfig is the real bridge -> RU wall thermostat answer to the
// thermostat's GET of "h/7654321/c/1/config" (tado_capture/coap_sniff_1786609624.log,
// MID 9cc2): a link-local CoAP 2.05 Content carrying 2040=12c0 = 48.00 C, the CH
// flow setpoint. The value is constant across both captures.
const capturedFlowConfig = "69eca805a2cafa07c51b0009a1cefa07780b25dbac10179e0d97b05fdfa436d1" +
	"8dbe9ebe6d1ea81537bc29d13d3538644907c4a5d761d7fbbfd1ebb048"

func TestBuildFlowSetpointCoAP(t *testing.T) {
	// The captured reply proves the tag, resource and centi-degree encoding.
	capf, err := ParseData(NetworkKey, mustHex(capturedFlowConfig))
	if err != nil {
		t.Fatalf("captured flow config does not decrypt: %v", err)
	}
	capApp := ParseApp(capf.Plaintext)
	if capApp == nil || capApp.CoAP == nil {
		t.Fatal("captured flow config does not parse as CoAP")
	}
	if capApp.CoAP.Type != "ACK" || capApp.CoAP.Code != "2.05" {
		t.Errorf("capture type/code = %s %s, want ACK 2.05", capApp.CoAP.Type, capApp.CoAP.Code)
	}
	if got := hex.EncodeToString(capApp.CoAP.Payload); got != "20400212c0" {
		t.Fatalf("capture payload = %s, want 20400212c0 (2040=48.00C)", got)
	}
	if o, ok := capApp.CoAP.Option(4); !ok || hex.EncodeToString(o.Val) != "06c75b32eed9d547" {
		t.Errorf("capture ETag = %v/%x, want 4/06c75b32eed9d547", ok, o.Val)
	}
	if o, ok := capApp.CoAP.Option(23); !ok || !bytes.Equal(o.Val, []byte{0x03}) {
		t.Errorf("capture Block2 = %v/%x, want 23/03", ok, o.Val)
	}

	dst := capf.DstEUI // the RU is the destination of our reply
	pkt, err := BuildFlowSetpointCoAP(1, 48.0, capApp.CoAP.MID, capApp.CoAP.Token, BridgeEUI, dst)
	if err != nil {
		t.Fatal(err)
	}
	m := parseBuilt(t, pkt)
	if m.Type != "ACK" || m.Code != "2.05" {
		t.Errorf("type/code = %s %s, want ACK 2.05", m.Type, m.Code)
	}
	if m.SrcPort != 4005 || m.DstPort != 5683 {
		t.Errorf("ports %d->%d, want 4005->5683", m.SrcPort, m.DstPort)
	}
	if got := hex.EncodeToString(m.Payload); got != "20400212c0" {
		t.Errorf("payload = %s, want 20400212c0 (2040=48.00C)", got)
	}
	if o, ok := m.Option(4); !ok || !bytes.Equal(o.Val, FlowSetpointETag(m.Payload)) {
		t.Errorf("ETag = %v/%x, want 4/%x", ok, o.Val, FlowSetpointETag(m.Payload))
	}
	if got, want := optionNums(m), optionNums(capApp.CoAP); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("options %v, want the captured %v", got, want)
	}
	if o, ok := m.Option(23); !ok || !bytes.Equal(o.Val, []byte{0x03}) {
		t.Errorf("Block2 = %v/%x, want 23/03", ok, o.Val)
	}
	if err := checkInlineUDP(BridgeEUI, dst, pkt); err != nil {
		t.Error(err)
	}

	// A different setpoint re-encodes the centi value; 30..75 C are accepted.
	pkt, err = BuildFlowSetpointCoAP(1, 55.5, 0x1234, []byte{0x40}, BridgeEUI, dst)
	if err != nil {
		t.Fatal(err)
	}
	updated := parseBuilt(t, pkt)
	if got := hex.EncodeToString(updated.Payload); got != "20400215ae" { // 5550 = 0x15ae
		t.Errorf("55.5C payload = %s, want 20400215ae", got)
	}
	if bytes.Equal(FlowSetpointETag(m.Payload), FlowSetpointETag(updated.Payload)) {
		t.Error("a different flow representation must have a different ETag")
	}
	for _, tc := range []struct {
		temp float64
		ok   bool
	}{{29.9, false}, {30.0, true}, {75.0, true}, {75.1, false}} {
		_, err := BuildFlowSetpointCoAP(1, tc.temp, 0x1, []byte{0x1}, BridgeEUI, dst)
		if (err == nil) != tc.ok {
			t.Errorf("%.1fC: err=%v, want ok=%t", tc.temp, err, tc.ok)
		}
	}
}

// TestBuildTimeCoAP: the "time" answer is the cloud's wire form - a piggybacked
// ACK 2.05 with no options at all, echoing the request MID and its empty token,
// carrying the 5-byte nanopb epoch.
func TestBuildTimeCoAP(t *testing.T) {
	dst := testDstEUI(t)
	// The captured cloud answer, payload 0df7a57d6a = epoch 1786619383.
	now := time.Unix(1786619383, 0)
	if got := hex.EncodeToString(EncodeTimePayload(now)); got != "0df7a57d6a" {
		t.Fatalf("payload = %s, want 0df7a57d6a", got)
	}

	pkt, err := BuildTimeCoAP(now, 0x1234, nil, BridgeEUI, dst, bridgeCoAPPort, coapPort)
	if err != nil {
		t.Fatal(err)
	}
	m := parseBuilt(t, pkt)
	if m.Type != "ACK" || m.Code != "2.05" {
		t.Errorf("type/code = %s %s, want ACK 2.05", m.Type, m.Code)
	}
	if m.MID != 0x1234 {
		t.Errorf("mid = %04x, want 1234 (the request's)", m.MID)
	}
	if len(m.Token) != 0 {
		t.Errorf("token = %x, want the request's empty one", m.Token)
	}
	if len(m.Options) != 0 {
		t.Errorf("options = %v, want none: the cloud sends not even a Content-Format", m.Options)
	}
	if got := hex.EncodeToString(m.Payload); got != "0df7a57d6a" {
		t.Errorf("payload = %s, want 0df7a57d6a", got)
	}
	if m.SrcPort != 4005 || m.DstPort != 5683 {
		t.Errorf("ports %d->%d, want 4005->5683", m.SrcPort, m.DstPort)
	}
	if err := checkInlineUDP(BridgeEUI, dst, pkt); err != nil {
		t.Error(err)
	}

	// A second later re-encodes the little-endian epoch.
	if got := hex.EncodeToString(EncodeTimePayload(now.Add(time.Second))); got != "0df8a57d6a" {
		t.Errorf("epoch+1 payload = %s, want 0df8a57d6a", got)
	}
}

// TestBuildAuthTokenBody: the auth/token body is the session id (025e) plus the
// session lifetime (025f) as u16 big-endian minutes.
func TestBuildAuthTokenBody(t *testing.T) {
	body, err := BuildAuthTokenBody(mustHex("0011223344556677"))
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(body); got != "025e080011223344556677025f0205a0" {
		t.Errorf("body = %s, want 025e080011223344556677025f0205a0", got)
	}
	tlvs := parseTLVList(body)
	if len(tlvs) != 2 {
		t.Fatalf("body holds %d TLVs, want 2", len(tlvs))
	}
	if tlvs[0].Tag != sessionIDTag || len(tlvs[0].Val) != 8 {
		t.Errorf("first TLV = %s/%x, want %s with 8 bytes", tlvs[0].Tag, tlvs[0].Val, sessionIDTag)
	}
	if tlvs[1].Tag != sessionLifetimeTag || be(tlvs[1].Val) != sessionLifetimeMin {
		t.Errorf("second TLV = %s/%x, want %s = %d minutes big-endian",
			tlvs[1].Tag, tlvs[1].Val, sessionLifetimeTag, sessionLifetimeMin)
	}
	// The firmware branch at 0x1e5e2 accepts a session id of 4..8 bytes only.
	for _, n := range []int{0, 3, 9} {
		if _, err := BuildAuthTokenBody(make([]byte, n)); err == nil {
			t.Errorf("session id of %d bytes should be refused", n)
		}
	}
}

// TestConfigWriteUTCOffset: tag 02b2 goes out as a signed 16-bit big-endian
// minute count, and a value the device could not read as an offset is refused
// rather than truncated.
func TestConfigWriteUTCOffset(t *testing.T) {
	for _, tc := range []struct {
		min  int
		want string
	}{
		{120, "02b2020078"}, // CEST
		{60, "02b202003c"},  // CET
		{-60, "02b202ffc4"}, // one hour west of UTC
		{-1440, "02b202fa60"},
	} {
		w := ConfigWrite{UTCOffsetMin: &tc.min}
		if err := w.Validate(); err != nil {
			t.Fatalf("offset %d: %v", tc.min, err)
		}
		tlvs := w.TLVs()
		if len(tlvs) != 1 {
			t.Fatalf("offset %d renders %d TLVs, want 1", tc.min, len(tlvs))
		}
		if got := hex.EncodeToString(tlvs[0].Encode()); got != tc.want {
			t.Errorf("offset %d = %s, want %s", tc.min, got, tc.want)
		}
	}
	// Out of range, 0x7fff (the store's "unset" sentinel) included.
	for _, min := range []int{1441, -1441, 0x7fff} {
		w := ConfigWrite{UTCOffsetMin: &min}
		if err := w.Validate(); err == nil {
			t.Errorf("offset %d should be refused", min)
		}
	}
	// The tag sits between 015d and 02b3 in the emitted order.
	off, offlineScheduleEnabled := 60, true
	module := uint16(112)
	w := ConfigWrite{InterfaceModuleID: &module, UTCOffsetMin: &off, OfflineScheduleEnabled: &offlineScheduleEnabled}
	tags := []string{}
	for _, tl := range w.TLVs() {
		tags = append(tags, tl.Tag)
	}
	if len(tags) != 3 || tags[0] != "015d" || tags[1] != "02b2" || tags[2] != "02b3" {
		t.Errorf("tag order = %v, want [015d 02b2 02b3]", tags)
	}
}

func cfgPtr[T any](v T) *T { return &v }

// TestConfigWriteFieldWireBytes pins the wire encoding of every typed d/config
// field: the literal [tag:2][len:1][value] bytes the device's record dispatcher
// expects, spelled out rather than round-tripped.
func TestConfigWriteFieldWireBytes(t *testing.T) {
	for _, tc := range []struct {
		name string
		w    ConfigWrite
		want string
	}{
		{"013f fahrenheit", ConfigWrite{TempUnitF: cfgPtr(true)}, "013f0101"},
		{"013f celsius", ConfigWrite{TempUnitF: cfgPtr(false)}, "013f0100"},
		{"0140 positive", ConfigWrite{CalibOffset: cfgPtr(180)}, "01400200b4"},
		{"0140 negative", ConfigWrite{CalibOffset: cfgPtr(-20)}, "014002ffec"},
		{"0140 lower bound", ConfigWrite{CalibOffset: cfgPtr(-calibOffsetAbs)}, "014002fc18"},
		{"0140 upper bound", ConfigWrite{CalibOffset: cfgPtr(calibOffsetAbs)}, "01400203e8"},
		// The dispatcher hands the byte to the setter unnormalized, so a value
		// that is neither 0 nor 1 has to survive the encoder intact.
		{"0143 raw byte", ConfigWrite{TraceEnable: cfgPtr(uint8(0xff))}, "014301ff"},
		{"0143 zero", ConfigWrite{TraceEnable: cfgPtr(uint8(0))}, "01430100"},
		{"0149 lcd variant", ConfigWrite{Orientation: cfgPtr(uint8(1))}, "01490101"},
		{"0158 state bit 9", ConfigWrite{ChangeNotify: cfgPtr(uint16(0x0200))}, "0158020200"},
		{"0158 cleared", ConfigWrite{ChangeNotify: cfgPtr(uint16(0))}, "0158020000"},
		{"015a etag 8B", ConfigWrite{Etag: []byte{0, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77}},
			"015a080011223344556677"},
		{"015a etag 1B", ConfigWrite{Etag: []byte{0xa5}}, "015a01a5"},
		{"015c home id", ConfigWrite{HomeID: cfgPtr(uint32(0x0074cbb1))}, "015c040074cbb1"},
		{"015d valve module", ConfigWrite{InterfaceModuleID: cfgPtr(uint16(112))}, "015d020070"},
		{"015d thermostat module", ConfigWrite{InterfaceModuleID: cfgPtr(uint16(71))}, "015d020047"},
		// One TLV per record, in the given order: the device keys each record's
		// slot off its ordinal in the body.
		{"015e node table", ConfigWrite{NodeTable: []NodeTableRec{
			{Role: 0x0d, Node: 0x00}, {Role: 0x0b, Node: 0x01}, {Role: 0x02, Node: 0x02},
		}}, "015e020d00015e020b01015e020202"},
		{"015e none", ConfigWrite{NodeTable: []NodeTableRec{{Role: 0x02, Node: 0x7f}}}, "015e02027f"},
		{"02b3 offline schedule disabled", ConfigWrite{OfflineScheduleEnabled: cfgPtr(false)}, "02b30100"},
		{"02b3 offline schedule enabled", ConfigWrite{OfflineScheduleEnabled: cfgPtr(true)}, "02b30101"},
	} {
		if err := tc.w.Validate(); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := hex.EncodeToString(encodeTLVs(tc.w.TLVs())); got != tc.want {
			t.Errorf("%s = %s, want %s", tc.name, got, tc.want)
		}
	}
}

// TestConfigWriteTagOrder: the fields render in ascending tag order, and the
// 015e records keep the order they were given inside it.
func TestConfigWriteTagOrder(t *testing.T) {
	w := ConfigWrite{
		OfflineScheduleEnabled: cfgPtr(false),
		NodeTable:              []NodeTableRec{{Role: 1, Node: 1}, {Role: 2, Node: 2}},
		HomeID:                 cfgPtr(uint32(7)),
		TempUnitF:              cfgPtr(false),
		UTCOffsetMin:           cfgPtr(60),
		ChangeNotify:           cfgPtr(uint16(0)),
		Etag:                   []byte{1},
		Orientation:            cfgPtr(uint8(0)),
		TraceEnable:            cfgPtr(uint8(0)),
		CalibOffset:            cfgPtr(0),
		InterfaceModuleID:      cfgPtr(uint16(112)),
	}
	var got []string
	for _, tl := range w.TLVs() {
		got = append(got, tl.Tag)
	}
	want := []string{"013f", "0140", "0143", "0149", "0158", "015a", "015c", "015d",
		"015e", "015e", "02b2", "02b3"}
	if len(got) != len(want) {
		t.Fatalf("tags = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tags = %v, want %v", got, want)
		}
	}
}

// TestConfigWriteValidation: every value the device would read as something
// other than what the caller meant is refused before it can be encoded.
func TestConfigWriteValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		w    ConfigWrite
	}{
		{"0140 above +10.0 C", ConfigWrite{CalibOffset: cfgPtr(calibOffsetAbs + 1)}},
		{"0140 below -10.0 C", ConfigWrite{CalibOffset: cfgPtr(-calibOffsetAbs - 1)}},
		// A record longer than the device's 8-byte reassembly buffer aborts the
		// rest of the body, so the etag is bounded at the source too.
		{"015a empty", ConfigWrite{Etag: []byte{}}},
		{"015a 9 bytes", ConfigWrite{Etag: make([]byte, configTLVMaxLen+1)}},
		// A zero home id makes the device log diag 0x3b/0x100 and apply nothing.
		{"015c zero home id", ConfigWrite{HomeID: cfgPtr(uint32(0))}},
		// 0 selects the firmware default image, 0x7fff tears the controller down.
		{"015d zero module", ConfigWrite{InterfaceModuleID: cfgPtr(uint16(0))}},
		{"015d teardown", ConfigWrite{InterfaceModuleID: cfgPtr(interfaceModuleTeardown)}},
		{"015e over 255 records", ConfigWrite{NodeTable: make([]NodeTableRec, nodeTableMax+1)}},
		{"02b2 out of range", ConfigWrite{UTCOffsetMin: cfgPtr(utcOffsetMinAbs + 1)}},
	} {
		if err := tc.w.Validate(); err == nil {
			t.Errorf("%s should be refused", tc.name)
		}
	}
	// The module ids the installed hardware carries.
	for _, module := range []uint16{71, 112} {
		if err := (ConfigWrite{InterfaceModuleID: &module}).Validate(); err != nil {
			t.Errorf("interface module %d: %v", module, err)
		}
	}
}

// TestBuildConfigWriteRejectsOversizedTLV: the encoder refuses a value longer
// than the device's 8-byte record buffer. A record declaring more returns -5 on
// the device and drops every tag behind it, so the arbitrary-TLV path is guarded
// as well as the typed one.
func TestBuildConfigWriteRejectsOversizedTLV(t *testing.T) {
	dst := testDstEUI(t)
	src := make([]byte, 8)
	good := []TLV{{Tag: "015a", Val: make([]byte, configTLVMaxLen)}}
	if _, err := BuildConfigWriteCoAP(good, 1, []byte{1}, src, dst); err != nil {
		t.Fatalf("a %d-byte value should be accepted: %v", configTLVMaxLen, err)
	}
	bad := []TLV{
		{Tag: "0140", Val: []byte{0, 1}},
		{Tag: "7777", Val: make([]byte, configTLVMaxLen+1)},
		{Tag: "02b3", Val: []byte{0}},
	}
	if _, err := BuildConfigWriteCoAP(bad, 1, []byte{1}, src, dst); err == nil {
		t.Fatalf("a %d-byte value should be refused", configTLVMaxLen+1)
	}
	if _, err := BuildConfigWriteCoAP(nil, 1, []byte{1}, src, dst); err == nil {
		t.Error("an empty config write should be refused")
	}
}

// captureConfigBody is a real cloud d/config write, decrypted off the air.
const captureConfigBody = "0143010001400200b4015d020070015c040074cbb102b30100021a020000" +
	"01490100015e020d030158020200"

// TestConfigWriteReproducesCaptureBody: the typed fields render the same tag set
// and the same values as a real cloud write. ConfigWrite.TLVs renders the set
// fields alone, in ascending tag order; the document that reaches the wire is
// assembled by configDocument and pinned by TestConfigDocumentMatchesCapture.
func TestConfigWriteReproducesCaptureBody(t *testing.T) {
	raw, err := hex.DecodeString(captureConfigBody)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{}
	var wantNodes []string
	for _, tl := range parseTLVList(raw) {
		if tl.Tag == "015e" {
			wantNodes = append(wantNodes, hex.EncodeToString(tl.Val))
			continue
		}
		want[tl.Tag] = hex.EncodeToString(tl.Val)
	}
	delete(want, "021a") // a constant of the document, not a field of the write

	w := ConfigWrite{
		TraceEnable:            cfgPtr(uint8(0)),
		CalibOffset:            cfgPtr(180),
		InterfaceModuleID:      cfgPtr(uint16(112)),
		HomeID:                 cfgPtr(uint32(0x0074cbb1)),
		OfflineScheduleEnabled: cfgPtr(false),
		Orientation:            cfgPtr(uint8(0)),
		NodeTable:              []NodeTableRec{{Role: 0x0d, Node: 0x03}},
		ChangeNotify:           cfgPtr(uint16(0x0200)),
	}
	if err := w.Validate(); err != nil {
		t.Fatal(err)
	}
	const wantBody = "01400200b401430100014901000158020200" +
		"015c040074cbb1015d020070015e020d0302b30100"
	if got := hex.EncodeToString(encodeTLVs(w.TLVs())); got != wantBody {
		t.Errorf("body = %s, want %s", got, wantBody)
	}

	got := map[string]string{}
	var gotNodes []string
	for _, tl := range w.TLVs() {
		if tl.Tag == "015e" {
			gotNodes = append(gotNodes, hex.EncodeToString(tl.Val))
			continue
		}
		got[tl.Tag] = hex.EncodeToString(tl.Val)
	}
	for tag, v := range want {
		if got[tag] != v {
			t.Errorf("tag %s = %q, want %q from the capture", tag, got[tag], v)
		}
	}
	for tag := range got {
		if _, ok := want[tag]; !ok {
			t.Errorf("tag %s is not in the captured write", tag)
		}
	}
	if len(gotNodes) != len(wantNodes) || (len(wantNodes) > 0 && gotNodes[0] != wantNodes[0]) {
		t.Errorf("015e records = %v, want %v", gotNodes, wantNodes)
	}
}

// TestConfigDocumentMatchesCapture pins the d/config write to the two captured
// documents of one valve: the whole document every time, in the bridge tag
// order, 015a last, and no Uri-Query. The values are the captured ones, with a
// synthetic home id and synthetic ETags.
//
// Lengths, against the captured frame: the document is 55 bytes and the app
// plaintext 99, which is 8 (shim) + 9 (IPHC and UDP) + 4 (CoAP header) + 6
// (token) + 14 (options) + 1 (payload marker) + 55 + 2 (trailer). The
// coordinator's own token is 4 bytes, so its frame is 2 bytes shorter.
func TestConfigDocumentMatchesCapture(t *testing.T) {
	const (
		homeID    = 0x00123456
		firstETag = "1122334455667788"
		nextETag  = "8877665544332211"
		document  = "01430100" + "%s" + "015d020070" + "015c0400123456" + "02b30100" +
			"021a020000" + "01490100" + "015e020d0a" + "0158020200" + "015a08" + "%s"
	)
	// The cached document is the first captured write: calibration -990.
	cached := ConfigWrite{
		TraceEnable:            cfgPtr(uint8(0)),
		CalibOffset:            cfgPtr(-990),
		InterfaceModuleID:      cfgPtr(uint16(112)),
		HomeID:                 cfgPtr(uint32(homeID)),
		OfflineScheduleEnabled: cfgPtr(false),
		Orientation:            cfgPtr(uint8(0)),
		NodeTable:              []NodeTableRec{{Role: 0x0d, Node: 0x0a}},
		ChangeNotify:           cfgPtr(uint16(0x0200)),
		Etag:                   mustHex(firstETag),
	}
	first, snap, err := mergeConfigDocument(cfgSnapshot{}, cached, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf(document, "014002fc22", firstETag)
	if got := hex.EncodeToString(encodeTLVs(first)); got != want {
		t.Errorf("first write = %s, want %s", got, want)
	}
	if got := hex.EncodeToString(snap.body); got != want {
		t.Errorf("remembered document = %s, want the document written %s", got, want)
	}
	if len(snap.body) != 55 {
		t.Errorf("document = %d bytes, want the captured 55", len(snap.body))
	}

	// The second captured write moves the calibration alone and resends the
	// whole document, every other tag unchanged. -70 centi-degrees is ffba; the
	// analysis prose reads ffb6, which is -74, and its own decoded line says -70.
	second, _, err := mergeConfigDocument(snap, ConfigWrite{
		CalibOffset: cfgPtr(-70), Etag: mustHex(nextETag),
	}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	want = fmt.Sprintf(document, "014002ffba", nextETag)
	if got := hex.EncodeToString(encodeTLVs(second)); got != want {
		t.Errorf("second write = %s, want %s", got, want)
	}

	// A field the coordinator has never seen goes out with the captured default,
	// so an operator writing one tag still sends a whole document. Tag 015d has
	// no default: only the device kind decides it.
	bare, _, err := mergeConfigDocument(cfgSnapshot{}, ConfigWrite{
		CalibOffset: cfgPtr(-70), Etag: mustHex(nextETag),
	}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	want = "01430100014002ffba02b30100021a020000" +
		"014901000158020200015a08" + nextETag
	if got := hex.EncodeToString(encodeTLVs(bare)); got != want {
		t.Errorf("write with no cached document = %s, want %s", got, want)
	}

	// The request itself: CoAP 0.03 to the bare d/config, Uri-Port ffff and
	// Content-Format 42, and a 6-byte token to reproduce the captured length.
	pkt, err := BuildConfigWriteCoAP(first, 0x1234, make([]byte, 6),
		mustHex("0102030405c51b00"), mustHex("1112131415c51b00"))
	if err != nil {
		t.Fatal(err)
	}
	app := ParseApp(WrapAppPayload(pkt, 10009, nil))
	if app == nil || app.CoAP == nil {
		t.Fatal("the config write does not parse")
	}
	if app.CoAP.Type != "CON" || app.CoAP.Code != "0.03" || app.CoAP.URI() != "d/config" {
		t.Errorf("request = %s %s %q, want CON 0.03 d/config", app.CoAP.Type, app.CoAP.Code, app.CoAP.URI())
	}
	if port, ok := app.CoAP.Option(7); !ok || !bytes.Equal(port.Val, []byte{0xff, 0xff}) {
		t.Errorf("Uri-Port = %x (present %v), want ffff", port.Val, ok)
	}
	if format, ok := app.CoAP.Option(12); !ok || !bytes.Equal(format.Val, []byte{42}) {
		t.Errorf("Content-Format = %x (present %v), want 42", format.Val, ok)
	}
	if n := len(WrapAppPayload(pkt, 10009, nil)); n != 99 {
		t.Errorf("app plaintext = %d bytes, want the captured 99", n)
	}
}

func TestEncodeTLV8(t *testing.T) {
	got := encodeTLV8(0x12, []byte{0xaa, 0xbb, 0xcc})
	if want := []byte{0x12, 0x03, 0xaa, 0xbb, 0xcc}; !bytes.Equal(got, want) {
		t.Errorf("encodeTLV8 = %x, want %x", got, want)
	}
}

func TestEncodeScheduleEntry(t *testing.T) {
	// 06:30 = minute 390 -> uvarint 86 03; 21.00 C = 2100 -> uvarint b4 10.
	got := EncodeScheduleEntry(390, 2100, true)
	want := []byte{0x08, 0x86, 0x03, 0x10, 0xb4, 0x10, 0x18, 0x01}
	if !bytes.Equal(got, want) {
		t.Errorf("entry = %x, want %x", got, want)
	}
	if got := EncodeScheduleEntry(390, 2100, false); got[len(got)-1] != 0x00 {
		t.Errorf("disabled entry = %x, want field3 = 0", got)
	}
	// Signed field2 is two's complement, not zigzag: -50 takes 10 bytes.
	neg := EncodeScheduleEntry(0, -50, false)
	wantNeg := []byte{0x08, 0x00, 0x10, 0xce, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01, 0x18, 0x00}
	if !bytes.Equal(neg, wantNeg) {
		t.Errorf("negative entry = %x, want %x", neg, wantNeg)
	}
}

func TestEncodeScheduleDayProgram(t *testing.T) {
	// field1 day_mask 0x7f, then one entry on field2:
	// minute 360 -> e8 02, 21.00 C = 2100 -> b4 10, enabled true (8-byte entry).
	got := EncodeScheduleDayProgram(0x7f, ScheduleEntry{Minute: 360, SetpointCenti: 2100, Enabled: true})
	want := []byte{
		0x08, 0x7f, // day_mask
		0x12, 0x08, 0x08, 0xe8, 0x02, 0x10, 0xb4, 0x10, 0x18, 0x01, // entry
	}
	if !bytes.Equal(got, want) {
		t.Errorf("day program = %x, want %x", got, want)
	}
	if got := EncodeScheduleDayProgram(0x01); !bytes.Equal(got, []byte{0x08, 0x01}) {
		t.Errorf("empty program = %x, want 0801 (mask only)", got)
	}
}

// TestScheduleEntryField3IsEnabled: field 3 is the device's power-on flag (zone
// tag 61e0), so 1 decodes as enabled, 0 as disabled, and an absent field as the
// protobuf default, disabled.
func TestScheduleEntryField3IsEnabled(t *testing.T) {
	for _, tc := range []struct {
		body []byte
		want bool
	}{
		{[]byte{0x08, 0x00, 0x10, 0xb4, 0x10, 0x18, 0x01}, true},
		{[]byte{0x08, 0x00, 0x10, 0xb4, 0x10, 0x18, 0x00}, false},
		{[]byte{0x08, 0x00, 0x10, 0xb4, 0x10}, false},
	} {
		got, err := decodeScheduleEntry(tc.body, false)
		if err != nil {
			t.Fatal(err)
		}
		if got.Enabled != tc.want {
			t.Errorf("decode(%x).Enabled = %t, want %t", tc.body, got.Enabled, tc.want)
		}
	}
	for _, enabled := range []bool{true, false} {
		got, err := decodeScheduleEntry(EncodeScheduleEntry(0, 2100, enabled), false)
		if err != nil {
			t.Fatal(err)
		}
		if got.Enabled != enabled {
			t.Errorf("round trip of enabled=%t = %t", enabled, got.Enabled)
		}
	}
}

func TestEncodeSchedule(t *testing.T) {
	prog := EncodeScheduleDayProgram(0x7f, ScheduleEntry{Minute: 360, SetpointCenti: 2100, Enabled: true})
	got := EncodeSchedule(prog)
	want := append([]byte{0x12, byte(len(prog))}, prog...) // field2 length-delimited
	if !bytes.Equal(got, want) {
		t.Errorf("schedule = %x, want %x", got, want)
	}
	if len(EncodeSchedule()) != 0 {
		t.Error("empty schedule should encode to no bytes")
	}
}

// TestSchedulePayloadRoundTrip: a working week and a weekend program travel in
// one document and come back unchanged, negative setpoints and disabled entries
// included.
func TestSchedulePayloadRoundTrip(t *testing.T) {
	programs := []ScheduleProgram{
		{DayMask: 0x1f, Entries: []ScheduleEntry{
			{Minute: 0, SetpointCenti: 1600, Enabled: true},
			{Minute: 390, SetpointCenti: 2100, Enabled: false},
		}},
		{DayMask: 0x60, Entries: []ScheduleEntry{
			{Minute: 0, SetpointCenti: -50, Enabled: true},
			{Minute: 1439, SetpointCenti: 500, Enabled: true},
		}},
	}
	body := EncodeSchedulePrograms(programs)
	want := EncodeSchedule(
		EncodeScheduleDayProgram(0x1f, programs[0].Entries...),
		EncodeScheduleDayProgram(0x60, programs[1].Entries...),
	)
	if !bytes.Equal(body, want) {
		t.Fatalf("body = %x, want %x", body, want)
	}
	got, err := DecodeSchedule(body)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, programs) {
		t.Errorf("programs = %+v, want %+v", got, programs)
	}
	for _, bad := range [][]byte{{0x08, 0x01}, {0x12}, {0x12, 0x05, 0x08}, {0x12, 0x02, 0x08, 0x80}} {
		if _, err := DecodeSchedule(bad); err == nil {
			t.Errorf("DecodeSchedule(%x) accepted", bad)
		}
	}
}
