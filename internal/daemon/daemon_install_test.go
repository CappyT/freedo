package daemon

import (
	"context"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"freedo/internal/radio"
	"freedo/tado"
)

// newTestDaemon starts a mock daemon over the install currently in
// defaultInstall and returns its API base URL.
func newTestDaemon(t *testing.T) string {
	t.Helper()
	emu := tado.NewEmu(tado.EmuConfig{TxCounter: 100000}, tado.NewUnhandledLogger(io.Discard))
	d := newDaemon(defaultInstall, emu, radio.NewMock(nil), true, "mock")
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)
	srv := httptest.NewServer(d.mux())
	t.Cleanup(func() {
		srv.Close()
		cancel()
	})
	return srv.URL
}

// TestAPIAmbiguousLabel: a room label two valves share is a usable 400 naming
// both, never a silent command to whichever one comes first.
func TestAPIAmbiguousLabel(t *testing.T) {
	url := newTestDaemon(t)
	var body struct {
		Error      string   `json:"error"`
		Candidates []string `json:"candidates"`
	}
	if code := getJSON(t, url+"/api/nodes/kitchen", &body); code != http.StatusBadRequest {
		t.Fatalf("ambiguous label: got %d %+v, want 400", code, body)
	}
	if len(body.Candidates) != 2 {
		t.Errorf("candidates = %v, want the two kitchen valves", body.Candidates)
	}

	body.Candidates = nil
	if code := postJSON(t, http.MethodPost, url+"/api/nodes/kitchen/setpoint",
		map[string]any{"tempC": 21, "id": 5}, &body); code != http.StatusBadRequest {
		t.Fatalf("ambiguous setpoint: got %d %+v, want 400", code, body)
	}
	if len(body.Candidates) != 2 {
		t.Errorf("candidates = %v, want the two kitchen valves", body.Candidates)
	}
}

func TestAPIActiveGetDeduplicatesPending(t *testing.T) {
	url := newTestDaemon(t)
	var body struct {
		Queued  bool `json:"queued"`
		Pending int  `json:"pending"`
	}
	for range 2 {
		if code := getJSON(t, url+"/api/nodes/0a06/fw", &body); code != http.StatusOK {
			t.Fatalf("active GET: got %d", code)
		}
		if !body.Queued || body.Pending != 1 {
			t.Fatalf("active GET response=%+v, want one queued request", body)
		}
	}
}

func TestAPIScheduleStoresPullDocument(t *testing.T) {
	url := newTestDaemon(t)
	var body struct {
		Queued  bool   `json:"queued"`
		Pending bool   `json:"pending"`
		Note    string `json:"note"`
	}
	if code := postJSON(t, http.MethodPut, url+"/api/nodes/VA1000000005/schedule", map[string]any{
		"dayMask": 127,
		"entries": []map[string]any{
			{"startMin": 0, "tempC": 5.0},
			{"startMin": 360, "tempC": 7.0, "enabled": false},
		},
		"id": 10,
	}, &body); code != http.StatusOK || !body.Queued || !body.Pending || body.Note != "served on the valve's next zone schedule GET" {
		t.Fatalf("schedule response: code=%d body=%+v", code, body)
	}
	var invalid struct{}
	if code := postJSON(t, http.MethodPut, url+"/api/nodes/VA1000000005/schedule", map[string]any{
		"dayMask": 127,
		"entries": []map[string]any{{"startMin": 0, "tempC": 5.001}},
		"id":      10,
	}, &invalid); code != http.StatusBadRequest {
		t.Fatalf("fractional centi schedule: got %d, want %d", code, http.StatusBadRequest)
	}
}

func TestDecodeScheduleEntries(t *testing.T) {
	entries, err := decodeScheduleEntries([]scheduleEntryBody{{StartMin: 0, TempC: 7.25}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := entries[0], (tado.ScheduleEntry{Minute: 0, SetpointCenti: 725, Enabled: true}); got != want {
		t.Fatalf("entry = %+v, want %+v", got, want)
	}
	if _, err := decodeScheduleEntries([]scheduleEntryBody{{StartMin: 0, TempC: 7.251}}); err == nil {
		t.Fatal("accepted non-centi temperature")
	}
}

func TestResolveIDFallsBackToInstall(t *testing.T) {
	in, err := tado.Parse([]byte(fixtureInstallYAML))
	if err != nil {
		t.Fatal(err)
	}
	emu := tado.NewEmu(in.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	bedroom, err := in.Resolve("VA1000000005")
	if err != nil {
		t.Fatal(err)
	}
	explicit := 7
	if id, err := resolveID(emu, in, bedroom.Short(), &explicit); err != nil || id != 7 {
		t.Errorf("explicit id = %d, %v, want 7", id, err)
	}
	if id, err := resolveID(emu, in, bedroom.Short(), nil); err != nil || id != 10 {
		t.Errorf("membership id = %d, %v, want 10", id, err)
	}

	declaredText := strings.Replace(fixtureInstallYAML,
		"serial: VA1000000005, label: bedroom}", "serial: VA1000000005, label: bedroom, zone: 10}", 1)
	declared, err := tado.Parse([]byte(declaredText))
	if err != nil {
		t.Fatal(err)
	}
	declaredEmu := tado.NewEmu(declared.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	if id, err := resolveID(declaredEmu, declared, bedroom.Short(), nil); err != nil || id != 10 {
		t.Errorf("declared zone id = %d, %v, want 10", id, err)
	}

	thermostat, err := in.Resolve("RU1000000001")
	if err != nil {
		t.Fatal(err)
	}
	zero := 0
	if id, err := resolveID(emu, in, thermostat.Short(), &zero); err != nil || id != 0 {
		t.Errorf("explicit zero id = %d, %v, want 0", id, err)
	}
	if _, err := resolveID(emu, in, thermostat.Short(), nil); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("multi-zone id error = %v, want ambiguity", err)
	}

}

// TestAPINodesListsInstall: /api/nodes reports the configured devices, not just
// the ones the engine has heard.
func TestAPINodesListsInstall(t *testing.T) {
	url := newTestDaemon(t)
	var valves []tado.ValveView
	if code := getJSON(t, url+"/api/nodes", &valves); code != http.StatusOK {
		t.Fatalf("nodes: got %d", code)
	}
	if len(valves) != len(defaultInstall.Devices) {
		t.Fatalf("nodes = %d, want the %d configured devices", len(valves), len(defaultInstall.Devices))
	}
	rooms := map[string]bool{}
	for _, v := range valves {
		rooms[v.Room] = true
	}
	for _, want := range []string{"bedroom", "kitchen", "hall", "office"} {
		if !rooms[want] {
			t.Errorf("no node labelled %q in %+v", want, valves)
		}
	}

	// A device that has never been heard is still addressable by serial.
	if code := getJSON(t, url+"/api/nodes/VA1000000002", new(tado.ValveView)); code != http.StatusOK {
		t.Errorf("configured but unheard node: got %d, want 200", code)
	}
}

// TestAPIZonesReportsInstall: /api/zones carries the declared zone map next to
// what the engine learned.
func TestAPIZonesReportsInstall(t *testing.T) {
	url := newTestDaemon(t)
	var body struct {
		Declared []struct {
			Number          int      `json:"number"`
			Kind            string   `json:"kind"`
			Members         []string `json:"members"`
			Reporter        string   `json:"reporter"`
			MeasuringDevice string   `json:"measuringDevice"`
		} `json:"declared"`
	}
	if code := getJSON(t, url+"/api/zones", &body); code != http.StatusOK {
		t.Fatalf("zones: got %d", code)
	}
	if len(body.Declared) != len(defaultInstall.Zones) {
		t.Fatalf("declared zones = %d, want %d", len(body.Declared), len(defaultInstall.Zones))
	}
	for _, z := range body.Declared {
		if z.Number == 5 {
			if len(z.Members) != 2 || z.Reporter == "" || z.MeasuringDevice == "" {
				t.Errorf("zone 5 = %+v, want two members, a reporter and a measuring device", z)
			}
			return
		}
	}
	t.Error("zone 5 missing from the declared map")
}

// TestAPIZonesReportsDemand: the heat-demand chain reaches the state view.
// The frame is the zone-demand publish of the capture, h/<home>/z/10/act with
// tag 40a0.
func TestAPIZonesReportsDemand(t *testing.T) {
	emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	bedroom, err := defaultInstall.Resolve("VA1000000005")
	if err != nil {
		t.Fatal(err)
	}
	put, err := tado.BuildCoAP(tado.CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x7f01, Token: []byte{0x7f},
		Path:    []string{"h", "7654321", "z", "10", "act"},
		Payload: tado.TLV{Tag: "40a0", Val: []byte{100}}.Encode(),
		SrcEUI:  bedroom.WireEUI(), DstEUI: defaultInstall.CoordinatorEUI(),
	})
	if err != nil {
		t.Fatal(err)
	}
	frame, err := tado.BuildData(defaultInstall.NetworkKey(), 0x7f, defaultInstall.CoordinatorEUI(),
		bedroom.WireEUI()[:5], tado.WrapAppPayload(put, 702, nil))
	if err != nil {
		t.Fatal(err)
	}
	emu.OnFrame(frame.Bytes())

	d := newDaemon(defaultInstall, emu, radio.NewMock(nil), true, "mock")
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)
	srv := httptest.NewServer(d.mux())
	t.Cleanup(func() {
		srv.Close()
		cancel()
	})

	var body struct {
		Demand []tado.ZoneView `json:"demand"`
	}
	if code := getJSON(t, srv.URL+"/api/zones", &body); code != http.StatusOK {
		t.Fatalf("zones: got %d", code)
	}
	for _, z := range body.Demand {
		if z.Number != 10 {
			continue
		}
		if z.DemandPct != 100 || z.Reporter != bedroom.Short() || z.Source != bedroom.Short() {
			t.Errorf("zone 10 = %+v, want demand 100 reported by %s", z, bedroom.Short())
		}
		return
	}
	t.Errorf("zone 10 missing from the demand block: %+v", body.Demand)
}

// TestZoneOffsetTargetsMeasuringDevice: the room-level API translates to one
// d/config write carrying 0140 for the elected measuring device.
func TestZoneOffsetTargetsMeasuringDevice(t *testing.T) {
	emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	d := newDaemon(defaultInstall, emu, radio.NewMock(nil), true, "mock")
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)
	srv := httptest.NewServer(d.mux())
	t.Cleanup(func() {
		srv.Close()
		cancel()
	})

	var response struct {
		Queued  bool    `json:"queued"`
		Zone    int     `json:"zone"`
		Target  string  `json:"target"`
		OffsetC float64 `json:"offsetC"`
	}
	if code := postJSON(t, http.MethodPost, srv.URL+"/api/zones/3/offset",
		map[string]any{"offsetC": -0.7}, &response); code != http.StatusOK {
		t.Fatalf("zone offset status=%d response=%+v", code, response)
	}
	if !response.Queued || response.Zone != 3 || response.Target != "VA1000000002" || response.OffsetC != -0.7 {
		t.Fatalf("zone offset response=%+v", response)
	}

	v, err := d.do(func(e *tado.EmuState) (any, error) { return e.ScheduleDownlink("a408") })
	if err != nil {
		t.Fatal(err)
	}
	actions := v.([]tado.Action)
	if len(actions) == 0 {
		t.Fatal("no config downlink for the measuring device")
	}
	frame, err := tado.ParseData(defaultInstall.NetworkKey(), actions[len(actions)-1].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	app := tado.ParseApp(frame.Plaintext)
	if app == nil || app.CoAP == nil || app.CoAP.URI() != "d/config" {
		t.Fatalf("zone offset app=%+v", app)
	}
	found := false
	for _, tlv := range app.TLVs {
		if tlv.Tag == "0140" {
			found = true
			if got := hex.EncodeToString(tlv.Val); got != "ffba" {
				t.Errorf("0140=%s, want ffba", got)
			}
		}
	}
	if !found {
		t.Errorf("0140 missing from %+v", app.TLVs)
	}

	// Zone 1 measures with the wall thermostat, which is served no d/config
	// document until it reports its interface module id, so the offset has
	// nowhere to go (TestThermostatOffsetServed covers a known id).
	var refused struct {
		Queued bool   `json:"queued"`
		Error  string `json:"error"`
	}
	code := postJSON(t, http.MethodPost, srv.URL+"/api/zones/1/offset",
		map[string]any{"offsetC": -0.7}, &refused)
	if code != http.StatusBadRequest || refused.Queued || !strings.Contains(refused.Error, "d/config") {
		t.Errorf("zone 1 offset = %d %+v, want 400 naming the refused d/config write", code, refused)
	}

	for name, body := range map[string]map[string]any{
		"missing offset":   {},
		"excess precision": {"offsetC": -0.701},
		"outside range":    {"offsetC": 10.01},
	} {
		var bad map[string]any
		if code := postJSON(t, http.MethodPost, srv.URL+"/api/zones/3/offset", body, &bad); code != http.StatusBadRequest {
			t.Errorf("%s: status=%d body=%+v", name, code, bad)
		}
	}
}

// TestAPIWithoutHotWaterZone: an installation with no hot-water zone refuses
// the hot-water endpoint, and the heat-source capabilities and the CH flow
// default to the heat source of its first circuit.
func TestAPIWithoutHotWaterZone(t *testing.T) {
	_, err := tado.Parse([]byte(`
version: 1
home_id: 4711
network_key: "000102030405060708090a0b0c0d0e0f"
coordinator: {eui: "aa016a3107c51b00"}
devices:
  - {kind: valve, eui: "01026a3107c51b00", serial: VA2001000001, label: kitchen}
zones:
  - {number: 1, name: kitchen, kind: heating, circuit: 1, members: [VA2001000001], reporter: VA2001000001}
circuits: []
`))
	if err == nil {
		t.Fatal("a circuit-less config validated; adjust the fixture")
	}

	in, err := tado.Parse([]byte(`
version: 1
home_id: 4711
network_key: "000102030405060708090a0b0c0d0e0f"
coordinator: {eui: "aa016a3107c51b00"}
devices:
  - {kind: thermostat, eui: "03046a3107c51b00", serial: RU2001000001, label: kitchen}
  - {kind: valve, eui: "01026a3107c51b00", serial: VA2001000001, label: kitchen}
zones:
  - {number: 1, name: kitchen, kind: heating, circuit: 1, members: [RU2001000001, VA2001000001], reporter: VA2001000001}
circuits:
  - {id: 1, name: house, heat_source: RU2001000001}
`))
	if err != nil {
		t.Fatal(err)
	}

	saved := defaultInstall
	defaultInstall = in
	t.Cleanup(func() { defaultInstall = saved })

	url := newTestDaemon(t)
	var body struct {
		Error string `json:"error"`
	}
	if code := postJSON(t, http.MethodPost, url+"/api/boiler/dhw",
		map[string]any{"tempC": 50}, &body); code != http.StatusServiceUnavailable || body.Error == "" {
		t.Fatalf("dhw without a hot-water zone: got %d %+v", code, body)
	}

	var set struct {
		Queued bool   `json:"queued"`
		Target string `json:"target"`
		ID     int    `json:"id"`
	}
	if code := getJSON(t, url+"/api/heatsource/capabilities", &set); code != http.StatusOK || !set.Queued || set.Target != "RU2001000001" {
		t.Fatalf("heatsource caps without a hot-water zone: got %d %+v, want queued to RU2001000001", code, set)
	}
	for name, body := range map[string]map[string]any{
		"the default target": {"tempC": 48},
		"an explicit target": {"tempC": 48, "target": "RU2001000001"},
	} {
		set.Queued, set.Target, set.ID = false, "", 0
		if code := postJSON(t, http.MethodPost, url+"/api/boiler/flow", body, &set); code != http.StatusOK || !set.Queued {
			t.Fatalf("flow with %s: got %d %+v", name, code, set)
		}
		if set.Target != "RU2001000001" || set.ID != 1 {
			t.Errorf("flow with %s: target=%q id=%d, want RU2001000001 on circuit 1", name, set.Target, set.ID)
		}
	}
}

// TestDaemonConfigUTCOffsetInherited: a d/config write that omits utcOffsetMin
// takes options.utc_offset_min, else the offset of options.timezone, and an
// explicit value in the body wins over it.
// The device holds no DST rules of its own, so the offset has to come from the
// installation unless the caller names another.
func TestDaemonConfigUTCOffsetInherited(t *testing.T) {
	const short = "a602" // the valve of zone 10

	var set struct {
		Queued bool   `json:"queued"`
		Error  string `json:"error"`
	}

	// The fixture install declares utc_offset_min: 0, so a body that sets no
	// field of its own still has one to write.
	url := newTestDaemon(t)
	if code := postJSON(t, http.MethodPost, url+"/api/nodes/"+short+"/config",
		map[string]any{"id": 10}, &set); code != http.StatusOK || !set.Queued {
		t.Fatalf("inherited offset: got %d %+v, want it queued", code, set)
	}

	// The body's own value is the one validated and sent: a value the config
	// would have replaced with a valid 0 is still refused.
	set.Queued, set.Error = false, ""
	if code := postJSON(t, http.MethodPost, url+"/api/nodes/"+short+"/config",
		map[string]any{"utcOffsetMin": 1441, "id": 10}, &set); code != http.StatusBadRequest || set.Error == "" {
		t.Fatalf("explicit out-of-range offset: got %d %+v, want 400", code, set)
	}

	// With no fixed offset the body inherits the offset of options.timezone.
	in, err := tado.Parse([]byte(strings.Replace(fixtureInstallYAML,
		"utc_offset_min: 0\n", "timezone: UTC\n", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if in.Options.UTCOffsetMin != nil {
		t.Fatal("the offset-less fixture still declares utc_offset_min")
	}
	saved := defaultInstall
	defaultInstall = in
	t.Cleanup(func() { defaultInstall = saved })

	set.Queued, set.Error = false, ""
	if code := postJSON(t, http.MethodPost, newTestDaemon(t)+"/api/nodes/"+short+"/config",
		map[string]any{"id": 10}, &set); code != http.StatusOK || !set.Queued {
		t.Fatalf("timezone offset: got %d %+v, want it queued", code, set)
	}
}
