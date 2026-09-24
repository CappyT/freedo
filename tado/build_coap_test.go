package tado

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"testing"
)

func float64Ptr(v float64) *float64 { return &v }

// TestBuildCoAPRoundTrip: a packet built by BuildCoAP decodes back to the same
// fields through the ParseCoAP path used on captured traffic.
func TestBuildCoAPRoundTrip(t *testing.T) {
	dst, _ := hex.DecodeString("02a6cefa07c51b00")
	payload := concat(
		TLV{Tag: "6280", Val: tempC(21.5)}.Encode(),
		TLV{Tag: "6240", Val: []byte{0x02}}.Encode(),
	)
	extra := []CoAPOption{
		{Num: 7, Val: []byte{0xff, 0xff}},
		{Num: 12, Val: []byte{42}},
		{Num: 2048, Val: mustHex("5e5500000000000a")}, // needs the 14-form option delta
	}

	pkt, err := BuildCoAP(CoAPReq{
		Type:    "CON",
		Code:    "0.03",
		MID:     0x790c,
		Token:   []byte{0x20},
		Path:    []string{"z", "s"},
		Query:   []string{"id=3"},
		Options: extra,
		Payload: payload,
		DstEUI:  dst,
		SrcPort: 4005,
		DstPort: 5683,
	})
	if err != nil {
		t.Fatal(err)
	}

	m, err := ParseCoAP(WrapAppPayload(pkt, 4711, nil)[8:])
	if err != nil {
		t.Fatalf("built packet does not parse: %v", err)
	}
	if m.Type != "CON" || m.Code != "0.03" {
		t.Errorf("type/code = %s %s, want CON 0.03", m.Type, m.Code)
	}
	if m.MID != 0x790c || !bytes.Equal(m.Token, []byte{0x20}) {
		t.Errorf("mid=%04x token=%x, want 790c 20", m.MID, m.Token)
	}
	if m.Path != "z/s" || m.Query != "id=3" {
		t.Errorf("uri = %q, want z/s?id=3", m.URI())
	}
	if m.SrcPort != 4005 || m.DstPort != 5683 {
		t.Errorf("ports %d->%d, want 4005->5683", m.SrcPort, m.DstPort)
	}
	if len(m.Options) != len(extra) {
		t.Fatalf("got %d options, want %d", len(m.Options), len(extra))
	}
	for i, want := range extra {
		if m.Options[i].Num != want.Num || !bytes.Equal(m.Options[i].Val, want.Val) {
			t.Errorf("option %d = %d/%x, want %d/%x", i,
				m.Options[i].Num, m.Options[i].Val, want.Num, want.Val)
		}
	}
	if !bytes.Equal(m.Payload, payload) {
		t.Errorf("payload = %x, want %x", m.Payload, payload)
	}
}

// TestBuildSetpointCoAPRoundTrip: a from-scratch setpoint command frames,
// encrypts, decrypts and parses back to the TLVs that were asked for.
func TestBuildSetpointCoAPRoundTrip(t *testing.T) {
	valve := NodeByAny("camera")
	if valve == nil {
		t.Fatal("camera node not found")
	}
	dst, _ := hex.DecodeString(valve.WireLE)

	pkt, err := BuildSetpointCoAP(SetpointCoAP{
		ID:         10,
		Setpoint:   21.5,
		Overlay:    2,
		NewOverlay: true,
		DstEUI:     dst,
	})
	if err != nil {
		t.Fatal(err)
	}
	pt := WrapAppPayload(pkt, 19319, nil)
	f, err := BuildData(NetworkKey, 0x10, dst, BridgeEUI[:5], pt)
	if err != nil {
		t.Fatal(err)
	}

	back, err := ParseData(NetworkKey, f.Bytes())
	if err != nil {
		t.Fatalf("built frame does not decrypt: %v", err)
	}
	if !bytes.Equal(back.Plaintext, f.Plaintext) {
		t.Fatal("decrypted plaintext differs from what was encrypted")
	}
	app := ParseApp(back.Plaintext)
	if app == nil || app.CoAP == nil {
		t.Fatal("built command does not parse as CoAP")
	}
	if app.Counter != 19319 {
		t.Errorf("counter=%d want 19319", app.Counter)
	}
	if app.CoAP.URI() != "z/s?id=10" {
		t.Errorf("uri = %q want z/s?id=10", app.CoAP.URI())
	}
	if app.ID != "10" {
		t.Errorf("app.ID = %q want 10", app.ID)
	}
	got := map[string]string{}
	for _, tlv := range app.TLVs {
		got[tlv.Tag] = hex.EncodeToString(tlv.Val)
	}
	if got["6280"] != "0866" { // 21.5 * 100 = 2150 = 0x0866
		t.Errorf("setpoint 6280=%s want 0866", got["6280"])
	}
	if v, ok := got["6200"]; ok {
		t.Errorf("6200=%s present: the away write carries no scheduled setpoint", v)
	}
	if got["6240"] != "02" {
		t.Errorf("overlay 6240=%s want 02", got["6240"])
	}
	if got["6440"] != "0001" {
		t.Errorf("new_overlay 6440=%s want 0001", got["6440"])
	}
}

// The two "z/s?id=10" downlink bodies of capture-heatdemand-20260827.log,
// decrypted. The bridge set the camera valve to 25.00 C at t=1787837637.098 and
// returned it to its program at t=1787837960.105.
const (
	capturedSetpointManualBody = "61600102" + // 6160=02, the same value on every zone
		"61800100" +
		"60200101" + // room zone
		"61e00100" +
		"62400102" + // manual
		"62600101" +
		"62800209c4" + // 25.00 C
		"62e00100" +
		"6440020001"
	capturedSetpointAutoBody = "61600102" +
		"61800100" +
		"60200101" +
		"61e00100" +
		"62400100" + // auto: 6260 and 6280 both go away
		"62e00100" +
		"6440020001"
)

// TestBuildSetpointCoAPMatchesCaptureBlock pins the TLV block against the two
// captured downlinks. Neither carries 6200.
func TestBuildSetpointCoAPMatchesCaptureBlock(t *testing.T) {
	dst := mustHex(NodeByAny("camera").WireLE)
	for _, tc := range []struct {
		name string
		c    SetpointCoAP
		want string
	}{
		{"manual", SetpointCoAP{ID: 10, Setpoint: 25.0, Overlay: 2, NewOverlay: true, DstEUI: dst}, capturedSetpointManualBody},
		{"return to schedule", SetpointCoAP{ID: 10, Setpoint: 25.0, Overlay: 0, NewOverlay: true, DstEUI: dst}, capturedSetpointAutoBody},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pkt, err := BuildSetpointCoAP(tc.c)
			if err != nil {
				t.Fatal(err)
			}
			app := ParseApp(WrapAppPayload(pkt, 1, nil))
			if app == nil || app.CoAP == nil {
				t.Fatal("built downlink does not parse")
			}
			if app.CoAP.URI() != "z/s?id=10" {
				t.Errorf("uri=%q, want z/s?id=10", app.CoAP.URI())
			}
			if got := hex.EncodeToString(app.CoAP.Payload); got != tc.want {
				t.Errorf("body=%s\n    want=%s", got, tc.want)
			}
		})
	}
}

// The "z/s?id=N" shapes the app capture holds, with synthetic zone ids and
// temperatures. Home writes 6160=1, 61e0=1 and the scheduled setpoint 6200;
// away writes 6160=2, 61e0=0 and no 6200. Tag 6440 marks the start or the end
// of an overlay, so a write that updates a running overlay drops it.
func TestBuildZoneWriteHomeAndAwayShapes(t *testing.T) {
	room := mustHex("3000000000c51b00")
	water := mustHex("2000000000c51b00")
	for _, tc := range []struct {
		name  string
		build func() ([]byte, error)
		want  string
	}{{
		// A manual set until the next block, from a zone that follows its program.
		name: "home until_next start",
		build: func() ([]byte, error) {
			return BuildSetpointCoAP(SetpointCoAP{
				ID: 4, Setpoint: 18.0, Overlay: 3,
				Home: true, SchedC: 20.0, NewOverlay: true, DstEUI: room,
			})
		},
		want: "61600101" + "61800100" + "60200101" + "61e00101" +
			"62000207d0" + "62400103" + "62600101" + "6280020708" + "62e00100" + "6440020001",
	}, {
		// The same write on a zone that already runs an overlay: a timer re-arm,
		// and the captured change of a running until_next overlay into a timer.
		name: "home timer re-arm",
		build: func() ([]byte, error) {
			return BuildSetpointCoAP(SetpointCoAP{
				ID: 4, Setpoint: 18.0, Overlay: 3,
				Home: true, SchedC: 20.0, DstEUI: room,
			})
		},
		want: "61600101" + "61800100" + "60200101" + "61e00101" +
			"62000207d0" + "62400103" + "62600101" + "6280020708" + "62e00100",
	}, {
		// Also the captured write of a program block turned back on at 20 C.
		name: "home return to schedule",
		build: func() ([]byte, error) {
			return BuildSetpointCoAP(SetpointCoAP{
				ID: 4, Overlay: 0,
				Home: true, SchedC: 20.0, NewOverlay: true, DstEUI: room,
			})
		},
		want: "61600101" + "61800100" + "60200101" + "61e00101" +
			"62000207d0" + "62400100" + "62e00100" + "6440020001",
	}, {
		// The captured program block set to off: 61e0=0 and no 6200.
		name: "home room program off",
		build: func() ([]byte, error) {
			return BuildSetpointCoAP(SetpointCoAP{
				ID: 4, Overlay: 0, ProgramOff: true,
				Home: true, SchedC: 20.0, NewOverlay: true, DstEUI: room,
			})
		},
		want: "61600101" + "61800100" + "60200101" + "61e00100" +
			"62400100" + "62e00100" + "6440020001",
	}, {
		// The captured end of a coordinator-kept timer: no 6440.
		name: "home room timer expiry",
		build: func() ([]byte, error) {
			return BuildBoostCoAP(BoostCoAP{ID: 4, Home: true, SchedC: 20.0, DstEUI: room})
		},
		want: "61600101" + "61800100" + "60200101" + "61e00101" +
			"62000207d0" + "62400100" + "62e00100",
	}, {
		// The room off overlay until the next block: 6260=0 and no 6280, the
		// captured tag sequence with a synthetic zone.
		name: "home room off until_next",
		build: func() ([]byte, error) {
			return BuildSetpointCoAP(SetpointCoAP{
				ID: 4, Setpoint: 21.5, Overlay: 3, Off: true,
				Home: true, SchedC: 20.0, NewOverlay: true, DstEUI: room,
			})
		},
		want: "61600101" + "61800100" + "60200101" + "61e00101" +
			"62000207d0" + "62400103" + "62600100" + "62e00100" + "6440020001",
	}, {
		name: "home room off manual re-arm",
		build: func() ([]byte, error) {
			return BuildSetpointCoAP(SetpointCoAP{
				ID: 4, Overlay: 2, Off: true, Home: true, SchedC: 20.0, DstEUI: room,
			})
		},
		want: "61600101" + "61800100" + "60200101" + "61e00101" +
			"62000207d0" + "62400102" + "62600100" + "62e00100",
	}, {
		name: "home hot water manual",
		build: func() ([]byte, error) {
			return BuildDHWSetpointCoAP(DHWSetpointCoAP{
				ID: 0, Setpoint: float64Ptr(54.0), Overlay: 2,
				Home: true, SchedC: 55.0, NewOverlay: true, DstEUI: water,
			})
		},
		want: "61600101" + "61800100" + "60200102" + "61e00101" +
			"620002157c" + "62400102" + "62600101" + "6280021518" + "62e00100" + "6440020001",
	}, {
		name: "home hot water return to schedule",
		build: func() ([]byte, error) {
			return BuildDHWSetpointCoAP(DHWSetpointCoAP{
				ID: 0, Overlay: 0,
				Home: true, SchedC: 55.0, NewOverlay: true, DstEUI: water,
			})
		},
		want: "61600101" + "61800100" + "60200102" + "61e00101" +
			"620002157c" + "62400100" + "62e00100" + "6440020001",
	}, {
		// No capture shows a hot-water block that is off, so 6200 stays.
		name: "home hot water program off",
		build: func() ([]byte, error) {
			return BuildDHWSetpointCoAP(DHWSetpointCoAP{
				ID: 0, Overlay: 0, ProgramOff: true,
				Home: true, SchedC: 55.0, NewOverlay: true, DstEUI: water,
			})
		},
		want: "61600101" + "61800100" + "60200102" + "61e00100" +
			"620002157c" + "62400100" + "62e00100" + "6440020001",
	}, {
		// One write of the away burst: the zone keeps its overlay, so no 6440.
		name: "away burst write",
		build: func() ([]byte, error) {
			return BuildSetpointCoAP(SetpointCoAP{
				ID: 4, Setpoint: 5.0, Overlay: 2, DstEUI: room,
			})
		},
		want: "61600102" + "61800100" + "60200101" + "61e00100" +
			"62400102" + "62600101" + "62800201f4" + "62e00100",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			pkt, err := tc.build()
			if err != nil {
				t.Fatal(err)
			}
			app := ParseApp(WrapAppPayload(pkt, 1, nil))
			if app == nil || app.CoAP == nil {
				t.Fatal("built downlink does not parse")
			}
			if got := hex.EncodeToString(app.CoAP.Payload); got != tc.want {
				t.Errorf("body=%s\n    want=%s", got, tc.want)
			}
		})
	}
}

// A hot-water return to schedule carries no setpoint and no enabled flag.
func TestBuildDHWReturnToScheduleRejectsOverlayTags(t *testing.T) {
	water := mustHex("2000000000c51b00")
	enabled := true
	if _, err := BuildDHWSetpointCoAP(DHWSetpointCoAP{
		ID: 0, Setpoint: float64Ptr(54.0), Overlay: 0, DstEUI: water,
	}); err == nil {
		t.Error("a return to schedule with a setpoint was accepted")
	}
	if _, err := BuildDHWSetpointCoAP(DHWSetpointCoAP{
		ID: 0, Enabled: &enabled, Overlay: 0, DstEUI: water,
	}); err == nil {
		t.Error("a return to schedule with an enabled flag was accepted")
	}
}

// A room off write needs an overlay: the return to schedule has no 6260.
func TestBuildSetpointOffRejectsReturnToSchedule(t *testing.T) {
	if _, err := BuildSetpointCoAP(SetpointCoAP{
		ID: 4, Overlay: 0, Off: true, DstEUI: mustHex("3000000000c51b00"),
	}); err == nil {
		t.Error("an off write with overlay 0 was accepted")
	}
}

// TestStampSessionID: the relay path adds the option only where it is missing.
// A message that already names its author goes on untouched, which is what keeps
// a relayed device request byte-identical to the bridge's own second hop.
func TestStampSessionID(t *testing.T) {
	mine := mustHex("a1b2c3d4e5f60718")
	theirs := mustHex("5e5500000000000a")
	base := CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x790c, Token: []byte{0x20},
		Path: []string{"z", "s"}, Query: []string{"lid=3"},
		Options: []CoAPOption{{Num: 12, Val: []byte{42}}},
		Payload: mustHex("6280020866"),
	}
	bare, err := buildCoAPMessage(base)
	if err != nil {
		t.Fatal(err)
	}

	if got, err := StampSessionID(bare, nil); err != nil || !bytes.Equal(got, bare) {
		t.Errorf("empty id rewrote the message: %x, %v", got, err)
	}

	stamped, err := StampSessionID(bare, mine)
	if err != nil {
		t.Fatal(err)
	}
	m, err := parseCoAPMessage(stamped)
	if err != nil {
		t.Fatal(err)
	}
	if o, ok := m.Option(2048); !ok || !bytes.Equal(o.Val, mine) {
		t.Errorf("option 2048 = %x, ok=%t, want %x", o.Val, ok, mine)
	}
	if m.MID != base.MID || !bytes.Equal(m.Token, base.Token) || m.URI() != "z/s?lid=3" ||
		m.Code != base.Code || m.Type != base.Type || !bytes.Equal(m.Payload, base.Payload) {
		t.Errorf("the stamp changed the message identity: %+v", m)
	}

	kept, err := StampSessionID(stamped, theirs)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(kept, stamped) {
		t.Errorf("a message that already names its author was rewritten: %x", kept)
	}
}

// capturedDHWSetpoint is the real bridge -> RU wall thermostat downlink the app
// produced when it set the boiler hot-water setpoint to 53.00 C
// (tado_capture/coap_sniff_1786609624.log, t=1786610002.738). The device echoed
// 045b=14b4 on "h/7654321/hvac/mon/dhw" 19 s later.
const capturedDHWSetpoint = "69ecdf05a2cafa07c51b0009a1cefa079e26414fc883187909ffb88b97851d0e" +
	"03e689e7a6aac8c803cee273b52fdb842fe12728d794ccddd9e2eef356e3c707e" +
	"10222cb24ad3622abf8997eb9fbf2eae409ff87990f36b6f82d62732ed81cc0aa" +
	"256973afa70f"

const capturedDHWDisable = "69ec9d05a2cafa07c51b0009a1cefa07c748061944f189e9e4057945ffceded7d4f18cdfcff49ea136998b870b18b3fa3f3a7c584e84fdac2a372a164fe6c4423b5be513d11752300eca64eb8ad424ffcefa8cf6c11b2f46692bbb65d3e5"

// TestBuildDHWSetpointCoAPMatchesCapture: the TLV block BuildDHWSetpointCoAP
// emits is byte-identical to the captured hot-water write, so the resource
// (z/s?id=0), the tags and the centi-degree encoding are the real ones.
func TestBuildDHWSetpointCoAPMatchesCapture(t *testing.T) {
	p, err := ParseData(NetworkKey, mustHex(capturedDHWSetpoint))
	if err != nil {
		t.Fatalf("captured DHW downlink does not decrypt: %v", err)
	}
	want := ParseApp(p.Plaintext)
	if want == nil || want.CoAP == nil {
		t.Fatal("captured DHW downlink does not parse as CoAP")
	}
	if want.CoAP.URI() != "z/s?id=0" {
		t.Fatalf("capture uri = %q, want z/s?id=0", want.CoAP.URI())
	}

	pkt, err := BuildDHWSetpointCoAP(DHWSetpointCoAP{
		ID:         0,
		Setpoint:   float64Ptr(53.0),
		Overlay:    2,
		NewOverlay: true,
		MID:        want.CoAP.MID,
		Token:      want.CoAP.Token,
		DstEUI:     p.DstEUI,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := ParseApp(WrapAppPayload(pkt, want.Counter, nil))
	if got == nil || got.CoAP == nil {
		t.Fatal("built DHW downlink does not parse as CoAP")
	}
	if got.CoAP.URI() != want.CoAP.URI() {
		t.Errorf("uri = %q, want %q", got.CoAP.URI(), want.CoAP.URI())
	}
	if !bytes.Equal(got.CoAP.Payload, want.CoAP.Payload) {
		t.Errorf("TLV block mismatch\n got  = %s\n want = %s",
			hex.EncodeToString(got.CoAP.Payload), hex.EncodeToString(want.CoAP.Payload))
	}
	// The checksum must be valid or the thermostat's UDP stack drops it.
	if err := checkInlineUDP(BridgeEUI, p.DstEUI, pkt); err != nil {
		t.Error(err)
	}
}

func TestBuildDHWDisableMatchesOfficialCapture(t *testing.T) {
	p, err := ParseData(NetworkKey, mustHex(capturedDHWDisable))
	if err != nil {
		t.Fatalf("captured DHW disable does not decrypt: %v", err)
	}
	want := ParseApp(p.Plaintext)
	if want == nil || want.CoAP == nil {
		t.Fatal("captured DHW disable does not parse as CoAP")
	}
	enabled := false
	pkt, err := BuildDHWSetpointCoAP(DHWSetpointCoAP{
		ID:         0,
		Enabled:    &enabled,
		Overlay:    2,
		NewOverlay: true,
		MID:        want.CoAP.MID,
		Token:      want.CoAP.Token,
		DstEUI:     p.DstEUI,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := ParseApp(WrapAppPayload(pkt, want.Counter, nil))
	if got == nil || got.CoAP == nil {
		t.Fatal("built DHW disable does not parse as CoAP")
	}
	if !bytes.Equal(got.CoAP.Raw, want.CoAP.Raw) {
		t.Errorf("CoAP mismatch\n got  = %s\n want = %s", hex.EncodeToString(got.CoAP.Raw), hex.EncodeToString(want.CoAP.Raw))
	}
	for _, tlv := range got.TLVs {
		if tlv.Tag == "6280" {
			t.Error("official disabled DHW write must omit 6280")
		}
	}
	if err := checkInlineUDP(BridgeEUI, p.DstEUI, pkt); err != nil {
		t.Error(err)
	}
}

// TestBuildDHWSetpointCoAPBlock: the hot-water block carries the heat-source
// channel (6160=02), the hot-water zone kind (6020=02) and no scheduled
// setpoint (6200), which is what tells the thermostat to drive TdhwSet.
func TestBuildDHWSetpointCoAPBlock(t *testing.T) {
	ru := NodeByAny("RU1000000001")
	if ru == nil {
		t.Fatal("RU wall thermostat not in the inventory")
	}
	pkt, err := BuildDHWSetpointCoAP(DHWSetpointCoAP{
		ID:         0,
		Setpoint:   float64Ptr(48.0),
		Overlay:    2,
		NewOverlay: true,
		DstEUI:     mustHex(ru.WireLE),
	})
	if err != nil {
		t.Fatal(err)
	}
	app := ParseApp(WrapAppPayload(pkt, 1, nil))
	if app == nil || app.CoAP == nil {
		t.Fatal("built DHW downlink does not parse as CoAP")
	}
	if app.CoAP.URI() != "z/s?id=0" {
		t.Errorf("uri = %q, want z/s?id=0", app.CoAP.URI())
	}
	got := map[string]string{}
	for _, tlv := range app.TLVs {
		got[tlv.Tag] = hex.EncodeToString(tlv.Val)
	}
	for tag, want := range map[string]string{
		"6160": "02",
		"6020": "02",
		"61e0": "00",
		"6240": "02",
		"6260": "01",
		"6280": "12c0", // 48.00 * 100 = 4800 = 0x12c0
		"62e0": "00",
		"6440": "0001",
	} {
		if got[tag] != want {
			t.Errorf("%s=%s want %s", tag, got[tag], want)
		}
	}
	if v, ok := got["6200"]; ok {
		t.Errorf("6200=%s present: a hot-water zone carries no scheduled setpoint", v)
	}
}

func TestBuildDHWSetpointCoAPDisabled(t *testing.T) {
	ru := NodeByAny("RU1000000001")
	if ru == nil {
		t.Fatal("RU wall thermostat not in the inventory")
	}
	enabled := false
	pkt, err := BuildDHWSetpointCoAP(DHWSetpointCoAP{
		ID:      0,
		Enabled: &enabled,
		Overlay: 2,
		DstEUI:  mustHex(ru.WireLE),
	})
	if err != nil {
		t.Fatal(err)
	}
	app := ParseApp(WrapAppPayload(pkt, 1, nil))
	if app == nil {
		t.Fatal("built DHW downlink does not parse as CoAP")
	}
	for _, tlv := range app.TLVs {
		if tlv.Tag == "6260" && hex.EncodeToString(tlv.Val) == "00" {
			continue
		}
		if tlv.Tag == "6280" {
			t.Error("disabled DHW write contains 6280")
		}
	}
	if got := hex.EncodeToString(app.CoAP.Payload); !strings.Contains(got, "62600100") {
		t.Error("6260=00 missing from disabled DHW write")
	}
}

func TestBuildDHWSetpointCoAPDisabledRejectsSetpoint(t *testing.T) {
	ru := NodeByAny("RU1000000001")
	if ru == nil {
		t.Fatal("RU wall thermostat not in the inventory")
	}
	enabled := false
	_, err := BuildDHWSetpointCoAP(DHWSetpointCoAP{
		ID:       0,
		Setpoint: float64Ptr(48),
		Enabled:  &enabled,
		Overlay:  2,
		DstEUI:   mustHex(ru.WireLE),
	})
	if err == nil || !strings.Contains(err.Error(), "must omit setpoint") {
		t.Fatalf("disabled setpoint error = %v, want omission error", err)
	}
}

// TestBuildDHWSetpointCoAPRange: the thermostat clamps the heat-source setpoint
// to 30..65 C, so the builder refuses anything outside it rather than sending a
// value the device silently rewrites.
func TestBuildDHWSetpointCoAPRange(t *testing.T) {
	dst := mustHex("05a2cafa07c51b00")
	for _, tc := range []struct {
		temp float64
		ok   bool
	}{{29.9, false}, {30.0, true}, {65.0, true}, {65.1, false}} {
		_, err := BuildDHWSetpointCoAP(DHWSetpointCoAP{ID: 0, Setpoint: float64Ptr(tc.temp), Overlay: 2, DstEUI: dst})
		if (err == nil) != tc.ok {
			t.Errorf("%.1fC: err=%v, want ok=%t", tc.temp, err, tc.ok)
		}
	}
}

// TestUDPChecksumAgainstCapture: udpChecksumV6 reproduces the inline checksum of
// every captured link-local (IPHC 7e33) frame, the real "z/s?id=N" downlinks
// included. That is the acceptance proof a rebuilt downlink passes the valve's
// UDP stack. The context-compressed modes (7ef5/7ef7/7cd7) are skipped: their
// IPv6 addresses come from 6LoWPAN context 0, not from the L2 header.
func TestUDPChecksumAgainstCapture(t *testing.T) {
	checked, downlinks := 0, 0
	for i, fr := range loadGolden(t) {
		p, err := ParseData(NetworkKey, fr)
		if err != nil {
			t.Fatal(err)
		}
		core, off, ok := linkLocalCore(p.Plaintext)
		if !ok {
			continue
		}
		udp, after, err := parseUDPNHC(core, off)
		if err != nil || core[off]&0x04 != 0 { // C=1 would have no inline checksum
			continue
		}
		inline := binary.BigEndian.Uint16(core[after-2 : after])
		got := udpChecksumV6(linkLocalFromWire(p.SrcEUI()), linkLocalFromWire(p.DstEUI),
			udp[0], udp[1], core[after:])
		if got != inline {
			t.Errorf("frame %d: checksum %04x, capture has %04x", i, got, inline)
		}
		checked++
		if m, err := parseCoAPMessage(core[after:]); err == nil &&
			m.Path == "z/s" && strings.HasPrefix(m.Query, "id=") {
			downlinks++
		}
	}
	if checked == 0 || downlinks == 0 {
		t.Fatalf("nothing verified: %d link-local frames, %d z/s?id downlinks", checked, downlinks)
	}
	t.Logf("checksum matches on %d captured link-local frames (%d z/s?id downlinks)", checked, downlinks)
}

// TestBuildCoAPMatchesCapture: re-encoding a captured "z/s?id=N" downlink from
// its parsed fields reproduces the on-air bytes exactly - IPHC, UDP-NHC,
// checksum, option encoding and all.
func TestBuildCoAPMatchesCapture(t *testing.T) {
	rebuilt := 0
	for i, fr := range loadGolden(t) {
		p, err := ParseData(NetworkKey, fr)
		if err != nil {
			t.Fatal(err)
		}
		core, off, ok := linkLocalCore(p.Plaintext)
		if !ok || !bytes.Equal(p.SrcPrefix, BridgeEUI[:5]) {
			continue
		}
		udp, after, err := parseUDPNHC(core, off)
		if err != nil {
			continue
		}
		m, err := parseCoAPMessage(core[after:])
		if err != nil || m.Path != "z/s" || !strings.HasPrefix(m.Query, "id=") {
			continue
		}
		pkt, err := BuildCoAP(CoAPReq{
			Type:    m.Type,
			Code:    m.Code,
			MID:     m.MID,
			Token:   m.Token,
			Path:    strings.Split(m.Path, "/"),
			Query:   strings.Split(m.Query, "&"),
			Options: m.Options,
			Payload: m.Payload,
			SrcEUI:  p.SrcEUI(),
			DstEUI:  p.DstEUI,
			SrcPort: udp[0],
			DstPort: udp[1],
		})
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if !bytes.Equal(pkt, core) {
			t.Errorf("frame %d mismatch\n orig=%s\n got =%s", i,
				hex.EncodeToString(core), hex.EncodeToString(pkt))
		}
		rebuilt++
	}
	if rebuilt == 0 {
		t.Fatal("no captured z/s?id downlink found to rebuild")
	}
	t.Logf("rebuilt %d captured z/s?id downlinks byte-for-byte", rebuilt)
}

// zoneRelayPairs returns every zone datagram the capture carries twice: once
// from the device that addressed a zone peer, once from the bridge on the hop
// towards that peer. The pairing is exact - same CoAP bytes, the uplink's IPv6
// destination as the relay's L2 destination, the uplink's L2 sender as the
// relay's IPv6 source - so a match cannot be a coincidence of Message-IDs.
func zoneRelayPairs(t *testing.T) [][2][]byte {
	t.Helper()
	type zf struct {
		f *Frame
		m *CoAPMessage
	}
	var ups, downs []zf
	for _, raw := range loadGolden(t) {
		f, err := ParseData(NetworkKey, raw)
		if err != nil {
			continue
		}
		app := ParseApp(f.Plaintext)
		if app == nil || app.CoAP == nil || !app.CoAP.Contexted() {
			continue
		}
		if bytes.Equal(f.SrcPrefix, BridgeEUI[:5]) {
			downs = append(downs, zf{f, app.CoAP})
		} else {
			ups = append(ups, zf{f, app.CoAP})
		}
	}
	var out [][2][]byte
	for _, u := range ups {
		peer := wireFromIID(u.m.Dst.IID)
		if peer == nil {
			continue
		}
		for _, d := range downs {
			if d.m.MID != u.m.MID || !bytes.Equal(d.m.Raw, u.m.Raw) {
				continue
			}
			if !bytes.Equal(d.f.DstEUI, peer) || !bytes.Equal(wireFromIID(d.m.Src.IID), u.f.SrcEUI()) {
				continue
			}
			out = append(out, [2][]byte{u.f.Bytes(), d.f.Bytes()})
			break
		}
	}
	if len(out) == 0 {
		t.Fatal("no relayed zone pair in the capture")
	}
	return out
}

// TestBuildZoneRelayMatchesCapture: re-emitting a captured zone datagram towards
// the peer it is addressed to reproduces the bridge's own second hop byte for
// byte - IPHC, the inline source IID, the decremented hop limit, the ports and
// the checksum the sender computed.
func TestBuildZoneRelayMatchesCapture(t *testing.T) {
	pairs := zoneRelayPairs(t)
	for i, p := range pairs {
		up, err := ParseData(NetworkKey, p[0])
		if err != nil {
			t.Fatal(err)
		}
		down, err := ParseData(NetworkKey, p[1])
		if err != nil {
			t.Fatal(err)
		}
		m := ParseApp(up.Plaintext).CoAP
		got, err := BuildZoneRelay(m.Raw, modifiedEUI64(up.SrcEUI()), down.DstEUI, m.SrcPort, m.DstPort)
		if err != nil {
			t.Fatalf("pair %d: %v", i, err)
		}
		want := down.Plaintext[8 : len(down.Plaintext)-2]
		if !bytes.Equal(got, want) {
			t.Errorf("pair %d mismatch\n got =%s\n want=%s", i,
				hex.EncodeToString(got), hex.EncodeToString(want))
		}
	}
	t.Logf("rebuilt %d captured zone relays byte-for-byte", len(pairs))
}

// TestBuildSetpointCoAPZoneTagsOptIn: the three per-zone config tags appear only
// when the caller sets them. The default block is what the capture pins, so a
// leak here would break TestBuildCoAPMatchesCapture.
func TestBuildSetpointCoAPZoneTagsOptIn(t *testing.T) {
	dst := mustHex(NodeByAny("camera").WireLE)
	base := SetpointCoAP{ID: 10, Setpoint: 21.5, Overlay: 2, MID: 0x0101, Token: []byte{1}, DstEUI: dst}

	tlvs := func(c SetpointCoAP) map[string]string {
		pkt, err := BuildSetpointCoAP(c)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for _, tl := range ParseApp(WrapAppPayload(pkt, 1, nil)).TLVs {
			got[tl.Tag] = hex.EncodeToString(tl.Val)
		}
		return got
	}

	def := tlvs(base)
	for _, tag := range []string{"6060", "6340", "6400"} {
		if v, ok := def[tag]; ok {
			t.Errorf("default block carries %s=%s; it must stay absent", tag, v)
		}
	}

	flags := byte(ZoneFlagCircuit | ZoneFlagPrimary)
	circuit, enable := byte(1), byte(0)
	with := base
	with.ZoneFlags, with.Circuit, with.CtrlEnable = &flags, &circuit, &enable
	got := tlvs(with)
	for tag, want := range map[string]string{"6060": "01", "6340": "06", "6400": "00"} {
		if got[tag] != want {
			t.Errorf("%s=%q, want %q (all: %v)", tag, got[tag], want, got)
		}
	}
	// The elected reporter needs bit2 set and bit3 clear: zone_primary_select
	// tests (flags & 0x0c) == 4.
	if flags&0x0c != ZoneFlagPrimary {
		t.Errorf("flags %02x fail the zone_primary_select mask", flags)
	}
	// Everything the default block carries is still there, unchanged.
	for tag, want := range def {
		if got[tag] != want {
			t.Errorf("opt-in changed %s: %q, want %q", tag, got[tag], want)
		}
	}
}

// TestCoAPCodecTraps pins the two message-level traps the codec must survive:
// the proprietary option 2048, which no CoAP definition covers and which the
// bridge emits last, right before the payload marker, and the sentinel Uri-Port
// ffff, which goes out verbatim instead of being folded into the default port.
// The wire form is asserted first, then parsed back and re-encoded, so a codec
// that merely round-trips its own output cannot pass.
func TestCoAPCodecTraps(t *testing.T) {
	const wire = "4103790c20" + // CON 0.03, mid 790c, token 20
		"72ffff" + // 7  Uri-Port ffff
		"417a0173" + // 11 "z", 11 "s"
		"112a" + // 12 Content-Format 42
		"3469643d33" + // 15 "id=3"
		"e806e45e5500000000000a" + // 2048, 14-form delta
		"ff6280020866" // payload marker + TLV
	req := CoAPReq{
		Type:  "CON",
		Code:  "0.03",
		MID:   0x790c,
		Token: []byte{0x20},
		Path:  []string{"z", "s"},
		Query: []string{"id=3"},
		Options: []CoAPOption{
			{Num: 2048, Val: mustHex("5e5500000000000a")},
			{Num: 7, Val: []byte{0xff, 0xff}},
			{Num: 12, Val: []byte{42}},
		},
		Payload: mustHex("6280020866"),
	}

	msg, err := buildCoAPMessage(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(msg); got != wire {
		t.Fatalf("built %s\nwant  %s", got, wire)
	}

	m, err := parseCoAPMessage(msg)
	if err != nil {
		t.Fatalf("built message does not parse: %v", err)
	}
	if o, ok := m.Option(2048); !ok || !bytes.Equal(o.Val, mustHex("5e5500000000000a")) {
		t.Errorf("option 2048 = %x, ok=%t", o.Val, ok)
	}
	if o, ok := m.Option(7); !ok || !bytes.Equal(o.Val, []byte{0xff, 0xff}) {
		t.Errorf("Uri-Port = %x, ok=%t", o.Val, ok)
	}

	req.Options = m.Options
	req.Path = strings.Split(m.Path, "/")
	req.Query = strings.Split(m.Query, "&")
	req.Payload = m.Payload
	again, err := buildCoAPMessage(req)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, msg) {
		t.Fatalf("re-encode %s\nwant      %s", hex.EncodeToString(again), wire)
	}
}

// TestBuildCoAPAck: an ACK echoes the MID and token of the request it answers.
func TestBuildCoAPAck(t *testing.T) {
	dev, _ := hex.DecodeString("04a3defa07c51b00")
	token := []byte{0xde, 0xad, 0xbe, 0xef}

	pkt, err := BuildCoAPAck("2.04", 0x2721, token, nil, BridgeEUI, dev, 5683, 5683)
	if err != nil {
		t.Fatal(err)
	}
	m, err := ParseCoAP(WrapAppPayload(pkt, 1, nil)[8:])
	if err != nil {
		t.Fatalf("ACK does not parse: %v", err)
	}
	if m.Type != "ACK" {
		t.Errorf("type=%s want ACK", m.Type)
	}
	if m.Code != "2.04" {
		t.Errorf("code=%s want 2.04", m.Code)
	}
	if m.MID != 0x2721 {
		t.Errorf("mid=%04x want 2721", m.MID)
	}
	if !bytes.Equal(m.Token, token) {
		t.Errorf("token=%x want %x", m.Token, token)
	}
	if len(m.Payload) != 0 {
		t.Errorf("payload=%x want empty", m.Payload)
	}
	// The inline checksum must be valid for the device to accept it.
	if err := checkInlineUDP(BridgeEUI, dev, pkt); err != nil {
		t.Error(err)
	}
}

// TestCoAPCode covers the dotted-code mapping and its rejections.
func TestCoAPCode(t *testing.T) {
	for _, c := range []struct {
		in   string
		want byte
	}{{"0.01", 0x01}, {"0.03", 0x03}, {"2.03", 0x43}, {"2.04", 0x44}, {"2.05", 0x45}, {"4.04", 0x84}} {
		got, err := CoAPCode(c.in)
		if err != nil || got != c.want {
			t.Errorf("CoAPCode(%q) = %02x, %v; want %02x", c.in, got, err, c.want)
		}
		if s := formatCode(c.want); s != c.in {
			t.Errorf("formatCode(%02x) = %q, want %q", c.want, s, c.in)
		}
	}
	for _, bad := range []string{"", "2.4", "x.04", "2.0y", "8.01", "2.32"} {
		if _, err := CoAPCode(bad); err == nil {
			t.Errorf("CoAPCode(%q) should fail", bad)
		}
	}
}

// TestBuildCoAPElideChecksum: C=1 drops the inline checksum and still parses.
func TestBuildCoAPElideChecksum(t *testing.T) {
	pkt, err := BuildCoAP(CoAPReq{
		Code: "0.03", MID: 1, Token: []byte{7},
		Path: []string{"z", "s"}, ElideChecksum: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pkt[2] != nhcUDPElideCk {
		t.Errorf("UDP-NHC = %02x, want %02x", pkt[2], nhcUDPElideCk)
	}
	if want := 2 + 1 + 4 + 4 + 1 + 2 + 2; len(pkt) != want { // no 2-byte checksum in between
		t.Errorf("length %d, want %d for %x", len(pkt), want, pkt)
	}
	m, err := ParseCoAP(WrapAppPayload(pkt, 1, nil)[8:])
	if err != nil || m.Path != "z/s" {
		t.Fatalf("elided-checksum packet does not parse: %v", err)
	}
}

// linkLocalCore returns the IPHC packet of a plaintext and the UDP-NHC offset,
// but only for the link-local mode this builder emits.
func linkLocalCore(pt []byte) ([]byte, int, bool) {
	if len(pt) < 12 {
		return nil, 0, false
	}
	core := pt[8 : len(pt)-2]
	if core[0] != iphcLinkLocal0 || core[1] != iphcLinkLocal1 {
		return nil, 0, false
	}
	off, err := skipIPHC(core)
	if err != nil || off >= len(core) {
		return nil, 0, false
	}
	return core, off, true
}

// checkInlineUDP recomputes the inline checksum of a built packet.
func checkInlineUDP(srcEUI, dstEUI, pkt []byte) error {
	udp, after, err := parseUDPNHC(pkt, 2)
	if err != nil {
		return err
	}
	inline := binary.BigEndian.Uint16(pkt[after-2 : after])
	got := udpChecksumV6(linkLocalFromWire(srcEUI), linkLocalFromWire(dstEUI), udp[0], udp[1], pkt[after:])
	if got != inline {
		return errCk{inline, got}
	}
	return nil
}

type errCk struct{ inline, got uint16 }

func (e errCk) Error() string {
	return "inline checksum " + hex.EncodeToString([]byte{byte(e.inline >> 8), byte(e.inline)}) +
		" != recomputed " + hex.EncodeToString([]byte{byte(e.got >> 8), byte(e.got)})
}
