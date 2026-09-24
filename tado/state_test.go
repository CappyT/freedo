package tado

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// TestSnapshot: a real telemetry uplink lands in the derived view model with a
// plausible temperature, a battery reading and the running counters.
func TestSnapshot(t *testing.T) {
	raw, f, app := firstUplink(t, func(_ *Frame, a *AppPayload) bool {
		_, ok := a.TLV("012d")
		return ok
	})

	e, log, _ := newTestEmu(t)
	e.OnFrame(raw)

	s := e.Snapshot()
	if len(s.Valves) != 1 {
		t.Fatalf("got %d valves, want 1", len(s.Valves))
	}
	v := s.Valves[0]
	ns := e.nodes[hexEUI(f.SrcEUI())]
	ns.CSLPhase, ns.CSLPeriod = 123, 456
	ns.Telemetry["test"] = "value"
	v = e.Snapshot().Valves[0]
	if v.Short != ShortFromWire(f.SrcEUI()) {
		t.Errorf("short=%q, want %q", v.Short, ShortFromWire(f.SrcEUI()))
	}
	if v.CSLPhase != 123 || v.CSLPeriod != 456 {
		t.Errorf("CSL phase/period = %d/%d, want 123/456", v.CSLPhase, v.CSLPeriod)
	}
	if v.Telemetry["test"] != "value" {
		t.Errorf("Telemetry = %v, want copied test value", v.Telemetry)
	}
	v.Telemetry["test"] = "changed"
	if ns.Telemetry["test"] != "value" {
		t.Error("ValveView telemetry aliases node state")
	}
	if v.TempC <= 0 || v.TempC > 50 {
		t.Errorf("TempC=%v, want a plausible room temperature", v.TempC)
	}
	if v.RxCounter != app.Counter {
		t.Errorf("RxCounter=%d, want %d", v.RxCounter, app.Counter)
	}
	if v.LastSeenUnix == 0 {
		t.Error("LastSeenUnix not set")
	}
	if _, ok := app.TLV("0162"); ok {
		if v.BatteryMV <= 0 {
			t.Errorf("BatteryMV=%d, want the reported voltage", v.BatteryMV)
		}
		if v.BatteryPct < 0 || v.BatteryPct > 100 {
			t.Errorf("BatteryPct=%d, want 0..100", v.BatteryPct)
		}
	}
	if id, ok := queryID(app.CoAP.Query); ok {
		if got := e.Nodes()[0].LastID; got != id {
			t.Errorf("LastID=%d, want the uplink's %d", got, id)
		}
	}

	if s.Stats != e.Stats() {
		t.Errorf("snapshot stats %+v, want %+v", s.Stats, e.Stats())
	}
	if s.Stats.Frames != 1 || s.Stats.Data != 1 {
		t.Errorf("stats frames=%d data=%d, want 1 and 1", s.Stats.Frames, s.Stats.Data)
	}
	total := 0
	for _, n := range s.Unhandled {
		total += n
	}
	if total != log.Total() {
		t.Errorf("unhandled total=%d, want the logger's %d", total, log.Total())
	}
}

// TestSnapshotEmpty: with no traffic the snapshot is empty, not nil-mapped.
func TestSnapshotEmpty(t *testing.T) {
	e, _, _ := newTestEmu(t)
	s := e.Snapshot()
	if len(s.Valves) != 0 {
		t.Errorf("valves=%d, want 0", len(s.Valves))
	}
	if s.Boiler.Present {
		t.Error("boiler reported present with no traffic")
	}
	if s.Unhandled == nil {
		t.Error("Unhandled must be a map, not nil")
	}
}

func isPtr[T comparable](p *T, want T) bool { return p != nil && *p == want }

// TestSnapshotBoiler: the boiler tags of a synthetic uplink fill the single
// BoilerView, whichever node reports them.
func TestSnapshotBoiler(t *testing.T) {
	e, _, _ := newTestEmu(t)
	ns := e.node(mustB("05a2cafa07c51b00"))
	e.cacheTLVs(ns, []TLV{
		{Tag: "044c", Val: []byte{0x0f, 0xa0}}, // 40.00 C
		{Tag: "045a", Val: []byte{0x13, 0x88}}, // 50.00 C
		{Tag: "0452", Val: []byte{0x00, 35}},
		{Tag: "0457", Val: []byte{1}},
		{Tag: "0167", Val: []byte{1}},
		{Tag: "2090", Val: []byte{1}},
		{Tag: "4080", Val: []byte{60}},         // circuit demand 60%
		{Tag: "4000", Val: []byte{0x08, 0x34}}, // 21.00 C
		{Tag: "4040", Val: []byte{0x07, 0xd0}}, // 20.00 C
		{Tag: "044d", Val: []byte{0x0b, 0xb8}}, // return 30.00 C
		{Tag: "044f", Val: []byte{0xfe, 0x0c}}, // outside -5.00 C
		{Tag: "0450", Val: []byte{0x11, 0x94}}, // control setpoint 45.00 C
		{Tag: "046f", Val: []byte{0x16, 0x2e}}, // max CH setpoint 56.78 C
		{Tag: "045e", Val: []byte{0x0a}},       // 1.0 l/min
		{Tag: "0462", Val: []byte{0x0f}},       // 1.5 bar
		{Tag: "0463", Val: []byte{0x00, 0x00, 0x01, 0x2c}},
		{Tag: "0466", Val: []byte{0x00, 0x00, 0x0e, 0x10}},
	}, "", ClassData, "corridoio", "bridge", "")

	b := e.Snapshot().Boiler
	if !b.Present {
		t.Fatal("boiler not marked present")
	}
	if !isPtr(b.FlowC, 40) || !isPtr(b.DHWC, 50) {
		t.Errorf("flow=%v dhw=%v, want 40 and 50", b.FlowC, b.DHWC)
	}
	if !isPtr(b.ModulationPct, 35) || !b.Active || !isPtr(b.BusDetect, 1) {
		t.Errorf("mod=%v active=%v detect=%v, want 35 true 1", b.ModulationPct, b.Active, b.BusDetect)
	}
	if b.BusType != "" || b.InterfaceModule != nil {
		t.Errorf("bus=%q module=%v, want none: the thermostat reported no 015d", b.BusType, b.InterfaceModule)
	}
	if !b.HeatActive || b.CircuitDemandPct != 60 {
		t.Errorf("heatActive=%v circuitDemand=%d, want true and 60", b.HeatActive, b.CircuitDemandPct)
	}
	if b.LeadZoneTargetC != 21 || b.LeadZoneTempC != 20 {
		t.Errorf("leadZoneTarget=%v leadZoneTemp=%v, want 21 and 20", b.LeadZoneTargetC, b.LeadZoneTempC)
	}
	if !isPtr(b.ReturnC, 30) || !isPtr(b.OutsideC, -5) {
		t.Errorf("return=%v outside=%v, want 30 and -5", b.ReturnC, b.OutsideC)
	}
	if !isPtr(b.ControlSetpointC, 45) || !isPtr(b.MaxChSetpointC, 56.78) {
		t.Errorf("ctlSetpoint=%v maxCh=%v, want 45 and 56.78", b.ControlSetpointC, b.MaxChSetpointC)
	}
	if !isPtr(b.DhwFlowLmin, 1) || !isPtr(b.ChPressureBar, 1.5) {
		t.Errorf("dhwFlow=%v pressure=%v, want 1 and 1.5", b.DhwFlowLmin, b.ChPressureBar)
	}
	if !isPtr(b.BurnerStarts, 300) || !isPtr(b.BurnerHoursMin, 3600) {
		t.Errorf("starts=%v minutes=%v, want 300 and 3600", b.BurnerStarts, b.BurnerHoursMin)
	}
	if b.SourceShort != "a205" || b.FaultCodes != nil || b.FaultCode != 0 {
		t.Errorf("source=%s faults=%v fault=%d, want a205 and no codes report", b.SourceShort, b.FaultCodes, b.FaultCode)
	}
	if !isPtr(b.Mon["boiler_flow_C"], 40) || !isPtr(b.Maintenance["burner_starts"], 300) {
		t.Errorf("mon=%v maint=%v, want the groups to carry the flat values", b.Mon, b.Maintenance)
	}
}

func TestSnapshotCircuitAttribution(t *testing.T) {
	in := fixtureInstall(t)
	e := NewEmu(in.EmuConfig(), nil)
	ns := e.node(mustB("05a2cafa07c51b00"))
	e.cacheTLVs(ns, []TLV{{Tag: "044c", Val: []byte{0x0f, 0xa0}}}, "", ClassData, "corridoio", "bridge", "")
	circuits := e.Snapshot().Circuits
	if len(circuits) != 1 {
		t.Fatalf("circuits=%d, want 1", len(circuits))
	}
	c := circuits[0]
	if c.ID != 1 || c.HeatSource != "RU1000000001" || c.Boiler.SourceShort != "a205" || !isPtr(c.Boiler.FlowC, 40) {
		t.Errorf("circuit=%+v", c)
	}
}

// TestSnapshotBoilerIgnoresReportInterval: 015d is the d/config report interval,
// so a valve echoing it must not surface as a boiler.
func TestSnapshotBoilerIgnoresReportInterval(t *testing.T) {
	e, _, _ := newTestEmu(t)
	ns := e.node(mustB("02a6cefa07c51b00"))
	e.cacheTLVs(ns, []TLV{{Tag: "015d", Val: []byte{0x00, 0x70}}}, "", ClassData, "camera", "bridge", "")

	if b := e.Snapshot().Boiler; b.Present || b.FaultCode != 0 {
		t.Errorf("present=%v fault=%d, want false and 0", b.Present, b.FaultCode)
	}
}

// TestHeatDemand: tag 40a0 maps to HeatDemandPct, and a node that never
// reported it reads 0.
func TestHeatDemand(t *testing.T) {
	e, _, _ := newTestEmu(t)
	with := e.node(mustB("02a6cefa07c51b00"))
	e.cacheTLVs(with, []TLV{{Tag: "40a0", Val: []byte{0x4b}}}, "", ClassData, "camera", "bridge", "")
	e.node(mustB("04a3defa07c51b00")) // no telemetry at all

	got := map[string]int{}
	for _, v := range e.Snapshot().Valves {
		got[v.Short] = v.HeatDemandPct
	}
	if got["a602"] != 75 {
		t.Errorf("HeatDemandPct=%d, want 75", got["a602"])
	}
	if got["a304"] != 0 {
		t.Errorf("HeatDemandPct=%d without tag 40a0, want 0", got["a304"])
	}
}

// TestCacheTLVs62c0ByResource: the telemetry cache keys 62c0 by the resource
// that carried it. The two bodies are synthetic.
func TestCacheTLVs62c0ByResource(t *testing.T) {
	e, _, _ := newTestEmu(t)
	plane := e.node(mustB("02a6cefa07c51b00"))
	e.cacheTLVs(plane, []TLV{{Tag: "62c0", Val: []byte{0x01}}},
		"h/7654321/z/10/s?id=10", ClassData, "camera", "bridge", "")
	if got := plane.Telemetry["open_window_zone"]; got != "1" {
		t.Errorf("zone plane: open_window_zone=%q, want 1: %v", got, plane.Telemetry)
	}

	cfg := e.node(mustB("04a3defa07c51b00"))
	e.cacheTLVs(cfg, []TLV{
		{Tag: "63e0", Val: []byte{0x01}},
		{Tag: "62c0", Val: []byte{0x03, 0x84}},
		{Tag: "6380", Val: []byte{0x03, 0x84}},
	}, "h/7654321/z/10/config", ClassData, "cucina", "bridge", "")
	if got := cfg.Telemetry["zone_open_ivl_sec"]; got != "900" {
		t.Errorf("zone config: zone_open_ivl_sec=%q, want 900: %v", got, cfg.Telemetry)
	}
	if _, ok := cfg.Telemetry["open_window_zone"]; ok {
		t.Errorf("zone config: 62c0 cached as open_window_zone: %v", cfg.Telemetry)
	}
}

// TestBoilerTagsKnown: the relayed OpenTherm attributes decode through the
// catalog instead of falling through as unknown tags.
func TestBoilerTagsKnown(t *testing.T) {
	cases := []struct {
		tag  string
		val  []byte
		name string
		want string
	}{
		{"044d", []byte{0x0b, 0xb8}, "return_water_C", "30.00C"},
		{"0450", []byte{0x11, 0x94}, "control_setpoint_C", "45.00C"},
		{"045e", []byte{0x0a}, "dhw_flow_lmin", "1.00l/min"},
		{"0462", []byte{0x0f}, "ch_pressure_bar", "1.50bar"},
		{"0463", []byte{0x00, 0x00, 0x01, 0x2c}, "burner_starts", "300"},
		{"046e", []byte{0x00, 0x2a}, "boiler_046e", "42"},
		{"0470", []byte{0x01, 0x00}, "boiler_0470", "256"},
	}
	for _, c := range cases {
		info, ok := tags[c.tag]
		if !ok {
			t.Errorf("tag %s missing from the catalog", c.tag)
			continue
		}
		if info.name != c.name {
			t.Errorf("tag %s name=%q, want %q", c.tag, info.name, c.name)
		}
		if got := info.decode(c.val); got != c.want {
			t.Errorf("tag %s decode=%q, want %q", c.tag, got, c.want)
		}
	}
}

// The percentage is linear per cell from the low battery voltage to
// FullBatteryCellMV, so the alert threshold reads 0 %.
func TestBatteryPct(t *testing.T) {
	cell := 1200
	custom := Options{LowBatteryCellMV: &cell}
	for _, c := range []struct {
		opts Options
		kind DeviceKind
		mV   int
		want int
	}{
		{Options{}, KindValve, 2000, 0},
		{Options{}, KindValve, 2200, 0},
		{Options{}, KindValve, 2520, 40},
		{Options{}, KindValve, 2600, 50},
		{Options{}, KindValve, 3000, 100},
		{Options{}, KindValve, 3060, 100},
		{Options{}, KindThermostat, 3300, 0},
		{Options{}, KindThermostat, 3720, 35},
		{Options{}, KindThermostat, 4500, 100},
		{Options{}, KindBridge, 3000, 0},
		{Options{}, "", 3000, 0},
		{custom, KindValve, 2400, 0},
		{custom, KindValve, 2700, 50},
	} {
		if got := c.opts.BatteryPct(c.kind, c.mV); got != c.want {
			t.Errorf("BatteryPct(%s, %d mV) = %d, want %d", c.kind, c.mV, got, c.want)
		}
	}
	full := FullBatteryCellMV + 10
	over := Options{LowBatteryCellMV: &full}
	if got := over.BatteryPct(KindValve, 3000); got != 0 {
		t.Errorf("a threshold above full: BatteryPct(3000) = %d, want 0", got)
	}
	if got := over.BatteryPct(KindValve, 3100); got != 100 {
		t.Errorf("a threshold above full: BatteryPct(3100) = %d, want 100", got)
	}
}

func TestModeString(t *testing.T) {
	for _, c := range []struct {
		v    byte
		want string
	}{{0, "auto"}, {2, "manual"}, {3, "until_next"}, {9, "unknown"}} {
		if got := modeString(c.v); got != c.want {
			t.Errorf("modeString(%d)=%q, want %q", c.v, got, c.want)
		}
	}
}

// downlinkFor queues one command for the camera, replays its first golden uplink
// and returns the piggybacked Data action.
func downlinkFor(t *testing.T, reasonPrefix string, queue func(*EmuState) error) Action {
	t.Helper()
	camera := NodeByAny("camera")
	if camera == nil {
		t.Fatal("camera not in inventory")
	}
	raw, _, _ := firstUplink(t, func(fr *Frame, _ *AppPayload) bool {
		return hex.EncodeToString(fr.SrcEUI()) == camera.WireLE
	})
	e, _, _ := newTestEmu(t)
	if err := queue(e); err != nil {
		t.Fatal(err)
	}
	a, ok := findAction(e.OnFrame(raw), ActData, reasonPrefix)
	if !ok {
		t.Fatalf("no %q action piggybacked", reasonPrefix)
	}
	return a
}

// TestResourceDownlinks: every queued resource rides out on the rendezvous and
// decrypts back to the expected CoAP request.
func TestResourceDownlinks(t *testing.T) {
	cases := []struct {
		name   string
		prefix string
		key    []byte
		path   string
		query  []string
		queue  func(*EmuState) error
	}{
		{"childlock", "downlink childlock", NetworkKey, "d/lock", nil,
			func(e *EmuState) error { return e.EnqueueChildLock("camera", 10, true) }},
		{"identify", "downlink identify", NetworkKey, "d/identify", nil,
			func(e *EmuState) error { return e.EnqueueIdentify("camera", 5) }},
		{"calibrate", "downlink calibrate", NetworkKey, "d/calibrate", nil,
			func(e *EmuState) error { return e.EnqueueCalibrate("camera") }},
		{"reboot", "downlink reboot", NetworkKey, "d/reboot", []string{"id=5"},
			func(e *EmuState) error { return e.EnqueueReboot("camera", 5) }},
		{"netcmd", "downlink netcmd", NetworkKey, "d/test/v1", []string{"t=1007"},
			func(e *EmuState) error { return e.EnqueueNetcmd("camera", 1007) }},
		{"mode", "downlink mode", NetworkKey, "z/s", []string{"id=4"},
			func(e *EmuState) error {
				seedHeatingBlock(e, mustB(NodeByAny("camera").WireLE))
				return e.EnqueueMode("camera", 4, 2)
			}},
		{"openwindow", "downlink openwindow", NetworkKey, "z/s", []string{"id=4"},
			func(e *EmuState) error {
				seedHeatingBlock(e, mustB(NodeByAny("camera").WireLE))
				return e.EnqueueOpenWindow("camera", 4, true)
			}},
		{"pair", "downlink pair", PairingKey, "d/pair", nil,
			func(e *EmuState) error { return e.EnqueuePair("camera", NetworkKey) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := downlinkFor(t, c.prefix, c.queue)
			df, err := ParseData(c.key, a.Bytes)
			if err != nil {
				t.Fatalf("downlink does not decrypt: %v", err)
			}
			app := ParseApp(df.Plaintext)
			if app == nil || app.CoAP == nil {
				t.Fatal("downlink plaintext does not parse")
			}
			m := app.CoAP
			wantCode := "0.03"
			if c.name == "reboot" || c.name == "pair" {
				wantCode = "0.02"
			}
			if m.Type != "CON" || m.Code != wantCode {
				t.Errorf("%s %s, want CON %s", m.Type, m.Code, wantCode)
			}
			if m.Path != c.path {
				t.Errorf("path=%q, want %q", m.Path, c.path)
			}
			if c.query == nil && m.Query != "" {
				t.Errorf("query=%q, want the bare path", m.Query)
			}
			for _, q := range c.query {
				if !strings.Contains(m.Query, q) {
					t.Errorf("query=%q, want it to contain %q", m.Query, q)
				}
			}
			wantSrc := uint16(4005)
			if c.name == "pair" {
				wantSrc = 5683 // every captured d/pair leaves from 5683
			}
			if m.SrcPort != wantSrc || m.DstPort != 5683 {
				t.Errorf("ports %d->%d, want %d->5683", m.SrcPort, m.DstPort, wantSrc)
			}
		})
	}
}

func seedHeatingBlock(e *EmuState, wire []byte) {
	ns := e.node(wire)
	e.cacheTLVs(ns, []TLV{
		{Tag: "6280", Val: []byte{0x08, 0x66}},
		{Tag: "6200", Val: []byte{0x07, 0x08}},
		{Tag: "6240", Val: []byte{0x00}},
		{Tag: "62e0", Val: []byte{0x01}},
	}, "", ClassData, "camera", "bridge", "")
}

// TestPairKeySeparation: the d/pair frame is secured with PairingKey, so the
// network key must not open it.
func TestPairKeySeparation(t *testing.T) {
	a := downlinkFor(t, "downlink pair", func(e *EmuState) error {
		return e.EnqueuePair("camera", NetworkKey)
	})
	if _, err := ParseData(NetworkKey, a.Bytes); err == nil {
		t.Error("the pair frame decrypted under the network key")
	}
	e, _, _ := newTestEmu(t)
	if err := e.EnqueuePair("camera", []byte("short")); err == nil {
		t.Error("a network key that is not 16 bytes should be rejected")
	}
	if e.Pending("camera") != 0 {
		t.Error("a rejected key must not queue anything")
	}
}

// TestChildLockPayload: the queued d/lock write carries 0290=01.
func TestChildLockPayload(t *testing.T) {
	a := downlinkFor(t, "downlink childlock", func(e *EmuState) error {
		return e.EnqueueChildLock("camera", 10, true)
	})
	df, err := ParseData(NetworkKey, a.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	app := ParseApp(df.Plaintext)
	if app == nil {
		t.Fatal("plaintext does not parse")
	}
	tl, ok := app.TLV("0290")
	if !ok || hex.EncodeToString(tl.Val) != "01" {
		t.Errorf("TLV 0290 = %v (present=%v), want 01", tl.Val, ok)
	}
}

func TestBoostPayload(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target string
	}{
		{name: "valve", target: "camera"},
		{name: "thermostat", target: "RU1000000001"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _, _ := newTestEmu(t)
			if err := e.EnqueueBoost(tc.target, 10, true); err != nil {
				t.Fatal(err)
			}
			actions, err := e.ScheduleDownlink(tc.target)
			if err != nil {
				t.Fatal(err)
			}
			a, ok := findAction(actions, ActData, "downlink boost")
			if !ok {
				t.Fatal("no boost downlink")
			}
			df, err := ParseData(NetworkKey, a.Bytes)
			if err != nil {
				t.Fatal(err)
			}
			app := ParseApp(df.Plaintext)
			if app == nil || app.CoAP == nil {
				t.Fatal("boost downlink does not parse")
			}
			if app.CoAP.Path != "z/s" || app.CoAP.Query != "id=10" || app.CoAP.Code != "0.03" {
				t.Errorf("request=%s %s, want 0.03 z/s?id=10", app.CoAP.Code, app.CoAP.URI())
			}
			if tlv, ok := app.TLV("6240"); !ok || !bytes.Equal(tlv.Val, []byte{3}) {
				t.Errorf("overlay TLV=%v/%x, want 6240=03", ok, tlv.Val)
			}
			if tlv, ok := app.TLV("6280"); !ok || !bytes.Equal(tlv.Val, []byte{0x09, 0xc4}) {
				t.Errorf("setpoint TLV=%v/%x, want 6280=09c4", ok, tlv.Val)
			}
		})
	}
}

func TestBoostTelemetryCachesRawAndDecodedValue(t *testing.T) {
	device := NodeByAny("camera")
	if device == nil {
		t.Fatal("camera not in inventory")
	}
	e, _, _ := newTestEmu(t)
	wire := mustHex(device.WireLE)
	if err := e.EnqueueBoost("camera", 3, true); err != nil {
		t.Fatal(err)
	}
	_, label, err := e.resolve("camera")
	if err != nil {
		t.Fatal(err)
	}
	actions, ok := e.nextDownlink(wire, label)
	if !ok || len(actions) != 1 {
		t.Fatalf("actions=%+v ok=%t, want one queued boost", actions, ok)
	}
	requestData, err := ParseData(NetworkKey, actions[0].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	request := ParseApp(requestData.Plaintext).CoAP
	if request == nil {
		t.Fatal("boost request does not parse as CoAP")
	}
	ack, err := BuildCoAPAck("2.04", request.MID, request.Token, TLV{Tag: "62a0", Val: []byte{1}}.Encode(), wire, BridgeEUI, coapPort, bridgeCoAPPort)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := BuildData(NetworkKey, 0x31, BridgeEUI, wire[:5], WrapAppPayload(ack, 700, nil))
	if err != nil {
		t.Fatal(err)
	}
	e.OnFrame(frame.Bytes())
	nodes := e.Nodes()
	if len(nodes) != 1 {
		t.Fatalf("nodes=%d, want 1", len(nodes))
	}
	if got := hex.EncodeToString(nodes[0].raw["62a0"]); got != "01" {
		t.Errorf("raw 62a0=%q, want 01", got)
	}
	if got := nodes[0].Telemetry["zmeas_ack"]; got != "1" {
		t.Errorf("decoded zmeas_ack=%q, want 1", got)
	}
}

// TestModeReusesLastKnown: EnqueueMode resends the node's cached block and only
// moves the overlay.
func TestModeReusesLastKnown(t *testing.T) {
	camera := NodeByAny("camera")
	raw, _, _ := firstUplink(t, func(fr *Frame, _ *AppPayload) bool {
		return hex.EncodeToString(fr.SrcEUI()) == camera.WireLE
	})
	e, _, _ := newTestEmu(t)
	ns := e.node(mustB(camera.WireLE))
	seedHeatingBlock(e, mustB(camera.WireLE))
	if ns.LastSetpointC != 21.5 || ns.LastSchedC != 18 || ns.LastOverlay != 0 || !ns.LastOpenWindow {
		t.Fatalf("last-known block = %v %v %v %v", ns.LastSetpointC, ns.LastSchedC, ns.LastOverlay, ns.LastOpenWindow)
	}
	if err := e.EnqueueMode("camera", 4, 3); err != nil {
		t.Fatal(err)
	}
	a, ok := findAction(e.OnFrame(raw), ActData, "downlink mode")
	if !ok {
		t.Fatal("no mode downlink")
	}
	df, err := ParseData(NetworkKey, a.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	app := ParseApp(df.Plaintext)
	got := map[string]string{}
	for _, tl := range app.TLVs {
		got[tl.Tag] = hex.EncodeToString(tl.Val)
	}
	want := map[string]string{"6280": "0866", "6240": "03", "62e0": "01"}
	for tag, v := range want {
		if got[tag] != v {
			t.Errorf("TLV %s=%s, want %s (all: %v)", tag, got[tag], v, got)
		}
	}
	if v, ok := got["6200"]; ok {
		t.Errorf("mode downlink carries 6200=%s; no captured plane does", v)
	}
}

// TestOpenWindowWithoutBlockRestatesRewrite: a node whose block is unknown
// takes the open window write in the shape of the zone rewrite, and its 2.04
// confirms the 62e0 alone, so a mode write still waits for the block.
func TestOpenWindowWithoutBlockRestatesRewrite(t *testing.T) {
	camera := NodeByAny("camera")
	if camera == nil {
		t.Fatal("camera not in inventory")
	}
	body := func(e *EmuState) (*queuedDownlink, string) {
		t.Helper()
		wire, label, err := e.resolve("camera")
		if err != nil {
			t.Fatal(err)
		}
		actions, ok := e.nextDownlink(wire, label)
		if !ok {
			t.Fatal("downlink missing")
		}
		data, err := ParseData(NetworkKey, actions[0].Bytes)
		if err != nil {
			t.Fatal(err)
		}
		app := ParseApp(data.Plaintext)
		if app == nil || app.CoAP == nil {
			t.Fatal("downlink does not parse as CoAP")
		}
		return e.queue[hexEUI(wire)][0], hex.EncodeToString(app.CoAP.Payload)
	}

	rewrite, _, _ := newTestEmu(t)
	rewrite.SetHomeState(true, map[int]float64{4: 19.5})
	rewrite.SetOpenWindows(map[int]bool{4: false})
	if err := rewrite.EnqueueZoneRewrite("camera", 4); err != nil {
		t.Fatal(err)
	}
	_, want := body(rewrite)

	e, _, _ := newTestEmu(t)
	e.SetHomeState(true, map[int]float64{4: 19.5})
	e.SetOpenWindows(map[int]bool{4: false})
	if err := e.EnqueueOpenWindow("camera", 4, false); err != nil {
		t.Fatal(err)
	}
	head, got := body(e)
	if got != want {
		t.Errorf("body=%s, want the rewrite body %s", got, want)
	}
	if want := "61600101" + "61800100" + "60200101" + "61e00101" + "620002079e" + "62400100" + "62e00100"; got != want {
		t.Errorf("body=%s, want %s", got, want)
	}
	if _, confirmed := e.ConfirmedOpenWindow("camera"); confirmed {
		t.Error("the write in flight already confirms 62e0")
	}
	if err := e.EnqueueOpenWindow("camera", 4, false); !errors.Is(err, ErrHeatingBlockPending) {
		t.Errorf("second write error = %v, want ErrHeatingBlockPending", err)
	}
	wire := mustB(camera.WireLE)
	e.confirmByMID(wire, &CoAPMessage{Type: "ACK", Code: "2.04", MID: head.mid, Token: head.responseToken})
	if open, confirmed := e.ConfirmedOpenWindow("camera"); open || !confirmed {
		t.Errorf("confirmed 62e0 = %t %t, want false true", open, confirmed)
	}
	if err := e.EnqueueMode("camera", 4, 2); !errors.Is(err, ErrHeatingBlockUnavailable) {
		t.Errorf("mode error = %v, want ErrHeatingBlockUnavailable", err)
	}

	away, _, _ := newTestEmu(t)
	if err := away.EnqueueOpenWindow("camera", 4, true); err != nil {
		t.Errorf("away write error = %v, want none: an away write carries no 6200", err)
	}
}

// TestModeCarries6440OpenWindowDoesNot: a mode write starts or ends an
// overlay, so it ends with 6440=0001. The captured 62e0 write that answers an
// open window report keeps the running overlay and carries no 6440.
func TestModeCarries6440OpenWindowDoesNot(t *testing.T) {
	camera := NodeByAny("camera")
	if camera == nil {
		t.Fatal("camera not in inventory")
	}
	for _, tc := range []struct {
		name    string
		enqueue func(*EmuState) error
		want    string
	}{
		{"mode", func(e *EmuState) error { return e.EnqueueMode("camera", 4, 2) }, "6440020001"},
		// 6160 6180 6020 61e0 6200 6240 62e0, home at 20.00 C on the program.
		{"openwindow", func(e *EmuState) error {
			e.SetHomeState(true, map[int]float64{4: 20})
			return e.EnqueueOpenWindow("camera", 4, true)
		}, "61600101" + "61800100" + "60200101" + "61e00101" + "62000207d0" + "62400100" + "62e00101"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _, _ := newTestEmu(t)
			seedHeatingBlock(e, mustB(camera.WireLE))
			if err := tc.enqueue(e); err != nil {
				t.Fatal(err)
			}
			wire, label, err := e.resolve("camera")
			if err != nil {
				t.Fatal(err)
			}
			actions, ok := e.nextDownlink(wire, label)
			if !ok {
				t.Fatal("downlink missing")
			}
			data, err := ParseData(NetworkKey, actions[0].Bytes)
			if err != nil {
				t.Fatal(err)
			}
			app := ParseApp(data.Plaintext)
			if app == nil || app.CoAP == nil {
				t.Fatal("downlink does not parse as CoAP")
			}
			if got := hex.EncodeToString(app.CoAP.Payload); !strings.HasSuffix(got, tc.want) {
				t.Errorf("body=%s, want the %s suffix", got, tc.want)
			}
		})
	}
}

// TestZoneWriteLeavesUnknownSchedOut: at home, a write to a zone with no
// program pushed and no 6200 in the zone cache leaves 6200 out, so the device
// keeps its own. It never takes the node's last 6200: the wall thermostat sits
// in the hot-water zone and in a heating zone, so that value may be the other
// zone's.
func TestZoneWriteLeavesUnknownSchedOut(t *testing.T) {
	queuedTags := func(t *testing.T, e *EmuState, wire []byte) map[string]string {
		t.Helper()
		q := e.queue[hexEUI(wire)]
		if len(q) == 0 {
			t.Fatal("no queued write")
		}
		tags := make(map[string]string)
		for _, tlv := range parseTLVList(parseBuilt(t, q[len(q)-1].coap).Payload) {
			tags[tlv.Tag] = hex.EncodeToString(tlv.Val)
		}
		return tags
	}
	const ru = "RU1000000001" // hot water in zone 0, the writer of heating zone 1
	for _, tc := range []struct {
		name    string
		zone    int
		enqueue func(*EmuState) error
	}{
		{"setpoint", 1, func(e *EmuState) error {
			return e.EnqueueSetpoint(ru, SetpointParams{ID: 1, Setpoint: 21, Sched: 21, Overlay: 2})
		}},
		{"off", 1, func(e *EmuState) error {
			return e.EnqueueSetpoint(ru, SetpointParams{ID: 1, Overlay: 2, Off: true})
		}},
		{"return to schedule", 1, func(e *EmuState) error { return e.EnqueueSchedule(ru, 1) }},
		{"boost", 1, func(e *EmuState) error { return e.EnqueueBoost(ru, 1, true) }},
		{"rewrite", 1, func(e *EmuState) error { return e.EnqueueZoneRewrite(ru, 1) }},
		{"hot water overlay", 0, func(e *EmuState) error { return e.EnqueueDHWOverlay(ru, 0, 48, true, 0) }},
		{"hot water off", 0, func(e *EmuState) error { return e.EnqueueDHWOverlay(ru, 0, 0, false, 0) }},
		{"hot water return to schedule", 0, func(e *EmuState) error { return e.EnqueueDHWSchedule(ru, 0) }},
		{"hot water rewrite", 0, func(e *EmuState) error { return e.EnqueueZoneRewrite(ru, 0) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, in := fixtureEmu(t)
			dev, err := in.Resolve(ru)
			if err != nil {
				t.Fatal(err)
			}
			other := 1 - tc.zone
			e.SetHomeState(true, map[int]float64{other: 55})
			e.node(dev.WireEUI()).LastSchedC = 55
			if err := tc.enqueue(e); err != nil {
				t.Fatal(err)
			}
			tags := queuedTags(t, e, dev.WireEUI())
			if tags["6160"] != "01" || tags["61e0"] != "01" {
				t.Errorf("head 6160=%s 61e0=%s, want the home state", tags["6160"], tags["61e0"])
			}
			if v, ok := tags["6200"]; ok {
				t.Errorf("6200=%s, want no 6200", v)
			}
		})
	}

	t.Run("window", func(t *testing.T) {
		e, in := fixtureEmu(t)
		dev, err := in.Resolve(ru)
		if err != nil {
			t.Fatal(err)
		}
		e.SetHomeState(true, map[int]float64{0: 55})
		e.node(dev.WireEUI()).LastSchedC = 55
		if err := e.EnqueueOpenWindow(ru, 1, true); !errors.Is(err, ErrHeatingBlockUnavailable) {
			t.Fatalf("window with no 6200 known: err=%v, want ErrHeatingBlockUnavailable", err)
		}
		// A member report caches the zone's own 6200: the writes restate it.
		e.zoneState[zoneKey(1, "z/s")] = encodeTLVs([]TLV{{Tag: "6200", Val: []byte{0x08, 0x02}}, {Tag: "6240", Val: []byte{0x00}}})
		if err := e.EnqueueOpenWindow(ru, 1, true); err != nil {
			t.Fatal(err)
		}
		if tags := queuedTags(t, e, dev.WireEUI()); tags["6200"] != "0802" || tags["62e0"] != "01" {
			t.Errorf("window write 6200=%s 62e0=%s, want the zone cache 0802 and 01", tags["6200"], tags["62e0"])
		}
	})
}

func TestModeAndOpenWindowNeedCompleteHeatingBlock(t *testing.T) {
	e, _, _ := newTestEmu(t)
	// Home with no program: the open window write has no 6200 to restate.
	e.SetHomeState(true, nil)
	for name, enqueue := range map[string]func() error{
		"mode":       func() error { return e.EnqueueMode("camera", 4, 2) },
		"openwindow": func() error { return e.EnqueueOpenWindow("camera", 4, true) },
	} {
		if err := enqueue(); !errors.Is(err, ErrHeatingBlockUnavailable) {
			t.Errorf("%s error = %v, want ErrHeatingBlockUnavailable", name, err)
		}
	}
	if got := e.Pending("camera"); got != 0 {
		t.Fatalf("pending=%d, want 0", got)
	}
	e.SetHomeState(false, nil)

	camera := NodeByAny("camera")
	ns := e.node(mustB(camera.WireLE))
	e.cacheTLVs(ns, []TLV{
		{Tag: "6280", Val: []byte{0x08, 0x66}},
		{Tag: "6200", Val: []byte{0x07, 0x08}},
		{Tag: "6240", Val: []byte{0x00}},
	}, "", ClassData, "camera", "bridge", "")
	if err := e.EnqueueMode("camera", 4, 2); !errors.Is(err, ErrHeatingBlockUnavailable) {
		t.Errorf("partial block error = %v, want ErrHeatingBlockUnavailable", err)
	}

	confirmed, _, _ := newTestEmu(t)
	seedHeatingBlock(confirmed, mustB(camera.WireLE))
	if err := confirmed.EnqueueMode("camera", 4, 2); err != nil {
		t.Fatal(err)
	}
	if err := confirmed.EnqueueOpenWindow("camera", 4, true); !errors.Is(err, ErrHeatingBlockPending) {
		t.Errorf("pending block error = %v, want ErrHeatingBlockPending", err)
	}
	pendingWire, pendingLabel, err := confirmed.resolve("camera")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := confirmed.nextDownlink(pendingWire, pendingLabel); !ok {
		t.Fatal("pending mode downlink missing")
	}
	confirmedHead := confirmed.queue[hexEUI(pendingWire)][0]
	confirmed.confirmByMID(pendingWire, &CoAPMessage{Type: "ACK", Code: "2.04", MID: confirmedHead.mid, Token: confirmedHead.responseToken})
	if err := confirmed.EnqueueOpenWindow("camera", 4, true); err != nil {
		t.Errorf("2.04 confirmation error = %v", err)
	}

	failed, _, _ := newTestEmu(t)
	failed.SetHomeState(true, nil)
	seedHeatingBlock(failed, mustB(camera.WireLE))
	if err := failed.EnqueueMode("camera", 4, 2); err != nil {
		t.Fatal(err)
	}
	failedWire, failedLabel, err := failed.resolve("camera")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := failed.nextDownlink(failedWire, failedLabel); !ok {
		t.Fatal("failed mode downlink missing")
	}
	failedHead := failed.queue[hexEUI(failedWire)][0]
	failed.confirmByMID(failedWire, &CoAPMessage{Type: "ACK", Code: "4.05", MID: failedHead.mid, Token: failedHead.responseToken})
	if err := failed.EnqueueOpenWindow("camera", 4, true); !errors.Is(err, ErrHeatingBlockUnavailable) {
		t.Errorf("4.xx confirmation error = %v, want ErrHeatingBlockUnavailable", err)
	}
	failedNode := failed.node(failedWire)
	if failedNode.LastSetpointC != 0 || failedNode.LastSchedC != 0 || failedNode.LastOverlay != 0 || failedNode.LastOpenWindow {
		t.Errorf("invalidated typed block = %v %v %v %v", failedNode.LastSetpointC, failedNode.LastSchedC, failedNode.LastOverlay, failedNode.LastOpenWindow)
	}

	if err := e.EnqueueSetpoint("camera", SetpointParams{ID: 4, Setpoint: 5, Sched: 18, Overlay: 2, OpenWindow: 0}); err != nil {
		t.Fatal(err)
	}
	if err := e.EnqueueMode("camera", 4, 3); !errors.Is(err, ErrHeatingBlockUnavailable) {
		t.Errorf("unconfirmed bootstrap error = %v, want ErrHeatingBlockUnavailable", err)
	}
	wire, label, err := e.resolve("camera")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := e.nextDownlink(wire, label); !ok {
		t.Fatal("bootstrap downlink missing")
	}
	seq := e.Queue("camera")[0].Seq
	e.OnFrame(BuildEnhAck(seq, BridgeEUI, wire, 0))
	q := e.queue[hexEUI(wire)][0]
	e.confirmByMID(wire, &CoAPMessage{Type: "ACK", Code: "2.04", MID: q.mid, Token: q.responseToken})
	if err := e.EnqueueMode("camera", 4, 3); err != nil {
		t.Fatalf("confirmed bootstrap mode: %v", err)
	}
	ns = e.node(wire)
	if ns.LastSetpointC != 5 || ns.LastSchedC != 18 || ns.LastOverlay != 2 || ns.LastOpenWindow {
		t.Errorf("promoted block = %v %v %v %v", ns.LastSetpointC, ns.LastSchedC, ns.LastOverlay, ns.LastOpenWindow)
	}

	ackHead := func() {
		t.Helper()
		if _, ok := e.nextDownlink(wire, label); !ok {
			t.Fatal("downlink missing")
		}
		seq := e.Queue("camera")[0].Seq
		e.OnFrame(BuildEnhAck(seq, BridgeEUI, wire, 0))
	}
	ackHead()
	if ns.LastOverlay != 2 || ns.LastOpenWindow {
		t.Errorf("mode promoted before CoAP ACK = overlay %d openWindow %t", ns.LastOverlay, ns.LastOpenWindow)
	}
	modeHead := e.queue[hexEUI(wire)][0]
	e.confirmByMID(wire, &CoAPMessage{Type: "ACK", Code: "2.04", MID: modeHead.mid, Token: []byte{0xff}})
	e.confirmByMID(wire, &CoAPMessage{Type: "ACK", Code: "2.05", MID: modeHead.mid, Token: modeHead.responseToken})
	if got := e.Pending("camera"); got != 1 {
		t.Fatalf("mode pending after mismatched CoAP responses = %d, want 1", got)
	}
	e.confirmByMID(wire, &CoAPMessage{Type: "ACK", Code: "2.04", MID: modeHead.mid, Token: modeHead.responseToken})
	if ns.LastOverlay != 3 || ns.LastOpenWindow {
		t.Errorf("mode promotion = overlay %d openWindow %t", ns.LastOverlay, ns.LastOpenWindow)
	}
	if err := e.EnqueueOpenWindow("camera", 4, true); err != nil {
		t.Fatal(err)
	}
	ackHead()
	if ns.LastOverlay != 3 || ns.LastOpenWindow {
		t.Errorf("open-window promoted before CoAP ACK = overlay %d openWindow %t", ns.LastOverlay, ns.LastOpenWindow)
	}
	openWindowHead := e.queue[hexEUI(wire)][0]
	e.confirmByMID(wire, &CoAPMessage{Type: "ACK", Code: "2.04", MID: openWindowHead.mid, Token: openWindowHead.responseToken})
	if ns.LastOverlay != 3 || !ns.LastOpenWindow {
		t.Errorf("open-window promotion = overlay %d openWindow %t", ns.LastOverlay, ns.LastOpenWindow)
	}
	if err := e.EnqueueMode("camera", 4, 0); err != nil {
		t.Fatal(err)
	}
	actions, ok := e.nextDownlink(wire, label)
	if !ok {
		t.Fatal("final mode downlink missing")
	}
	data, err := ParseData(NetworkKey, actions[0].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	app := ParseApp(data.Plaintext)
	if overlay, ok := app.TLV("6240"); !ok || !bytes.Equal(overlay.Val, []byte{0}) {
		t.Errorf("final overlay=%v/%x, want 6240=00", ok, overlay.Val)
	}
	// The captured return to schedule, open window instead of 62e0=00.
	wantAuto := strings.Replace(capturedSetpointAutoBody, "62e00100", "62e00101", 1)
	if got := hex.EncodeToString(app.CoAP.Payload); got != wantAuto {
		t.Errorf("final body=%s\n         want=%s", got, wantAuto)
	}
	if open, ok := app.TLV("62e0"); !ok || !bytes.Equal(open.Val, []byte{1}) {
		t.Errorf("final open-window=%v/%x, want 62e0=01", ok, open.Val)
	}
}
