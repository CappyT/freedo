package daemon

// topology_test: most daemon tests run on one installation layout. This table
// runs small synthetic layouts through the same engine checks, so a plant with
// no circuit, no hot water or two boilers fails here and not on site. Every
// identifier is synthetic.

import (
	"net/http"
	"strconv"
	"testing"

	"freedo/tado"
)

const topologyHeader = `
version: 1
home_id: 4711
network_key: "000102030405060708090a0b0c0d0e0f"
coordinator: {eui: "aa016a3107c51b00", serial: IB3000000001}
`

var topologies = []struct {
	name string
	yaml string
	// boiler says whether the layout has a thermostat wired to the boiler: the
	// heat-source capabilities route answers 503 without one.
	boiler bool
}{
	{"valves only, no circuit", `
devices:
  - {kind: valve, eui: "01016a3107c51b00", serial: VA3000000001, label: north}
  - {kind: valve, eui: "02016a3107c51b00", serial: VA3000000002, label: south}
  - {kind: valve, eui: "03016a3107c51b00", serial: VA3000000003, label: south}
zones:
  - {number: 1, name: north, kind: heating, members: [VA3000000001], reporter: VA3000000001}
  - {number: 2, name: south, kind: heating, members: [VA3000000002, VA3000000003]}
`, false},
	{"one thermostat, a valve zone, no hot water", `
devices:
  - {kind: thermostat, eui: "05016a3107c51b00", serial: RU3000000001, label: hall}
  - {kind: valve, eui: "01016a3107c51b00", serial: VA3000000001, label: north}
zones:
  - {number: 1, name: north, kind: heating, circuit: 1, members: [VA3000000001], reporter: VA3000000001}
circuits:
  - {id: 1, name: house, heat_source: RU3000000001}
`, true},
	{"two circuits, two thermostats", `
devices:
  - {kind: thermostat, eui: "05016a3107c51b00", serial: RU3000000001, label: ground}
  - {kind: thermostat, eui: "06016a3107c51b00", serial: RU3000000002, label: upper}
  - {kind: valve, eui: "01016a3107c51b00", serial: VA3000000001, label: north}
  - {kind: valve, eui: "02016a3107c51b00", serial: VA3000000002, label: south}
zones:
  - {number: 0, name: water, kind: hotwater, members: [RU3000000001]}
  - {number: 1, name: north, kind: heating, circuit: 1, members: [VA3000000001], reporter: VA3000000001}
  - {number: 2, name: south, kind: heating, circuit: 2, members: [VA3000000002], reporter: VA3000000002}
circuits:
  - {id: 1, name: ground, heat_source: RU3000000001}
  - {id: 2, name: upper, heat_source: RU3000000002}
`, true},
	{"a thermostat that is not a heat source", `
devices:
  - {kind: thermostat, eui: "05016a3107c51b00", serial: RU3000000001, label: hall}
  - {kind: thermostat, eui: "06016a3107c51b00", serial: RU3000000002, label: study}
  - {kind: valve, eui: "01016a3107c51b00", serial: VA3000000001, label: north}
zones:
  - {number: 1, name: north, kind: heating, circuit: 1, members: [VA3000000001], reporter: VA3000000001}
  - {number: 2, name: study, kind: heating, circuit: 1, members: [RU3000000002], reporter: RU3000000002}
circuits:
  - {id: 1, name: house, heat_source: RU3000000001}
`, true},
	{"hot water only", `
devices:
  - {kind: thermostat, eui: "05016a3107c51b00", serial: RU3000000001, label: hall}
zones:
  - {number: 0, name: water, kind: hotwater, members: [RU3000000001]}
`, true},
}

// topologyGET sends the GET of the zone configuration document from dev and
// returns the answer.
func topologyGET(t *testing.T, a *installAPI, dev *tado.Device, zone int, mid uint16) *tado.CoAPMessage {
	t.Helper()
	in := a.d.inst.Load()
	frame := uplinkFrame(t, in, dev.Serial, false, tado.CoAPReq{
		Type: "CON", Code: "0.01", MID: mid, Token: []byte{0x0c},
		Path:    []string{"h", strconv.FormatUint(uint64(in.HomeID), 10), "z", strconv.Itoa(zone), "config"},
		Options: []tado.CoAPOption{{Num: 12, Val: []byte{42}}, {Num: 23, Val: []byte{0x02}}},
	})
	v, err := a.d.do(func(e *tado.EmuState) (any, error) { return e.OnFrame(frame), nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, act := range v.([]tado.Action) {
		if act.Kind != tado.ActData {
			continue
		}
		data, err := tado.ParseData(in.NetworkKey(), act.Bytes)
		if err != nil {
			continue
		}
		if app := tado.ParseApp(data.Plaintext); app != nil && app.CoAP != nil && app.CoAP.MID == mid {
			return app.CoAP
		}
	}
	t.Fatalf("no answer with mid=%04x to the GET of zone %d from %s", mid, zone, dev.Serial)
	return nil
}

// TestTopologies: each layout is declared and stored, serves the configuration
// document of each zone, takes a setpoint write on each zone, runs the periodic
// work of the engine, switches to away and back, and renders its views.
func TestTopologies(t *testing.T) {
	for _, tc := range topologies {
		t.Run(tc.name, func(t *testing.T) {
			in, err := tado.Parse([]byte(topologyHeader + tc.yaml))
			if err != nil {
				t.Fatalf("declare: %v", err)
			}
			if err := in.ValidateCommissionable(true); err != nil {
				t.Fatalf("declare: %v", err)
			}
			a := newInstallAPIOn(t, in)
			stored := a.d.inst.Load()

			for i := range stored.Zones {
				z := &stored.Zones[i]
				members, err := stored.ZoneMembers(z.Number)
				if err != nil || len(members) == 0 {
					t.Fatalf("zone %d members: %v", z.Number, err)
				}
				if m := topologyGET(t, a, members[0], z.Number, uint16(0x7a00+i)); m.Code != "2.05" || len(m.Payload) == 0 {
					t.Errorf("zone %d config: %s with %d body bytes, want a 2.05 document", z.Number, m.Code, len(m.Payload))
				}
				setpoint := 21.0
				if z.Kind == tado.ZoneHotWater {
					setpoint = 50
				}
				body := map[string]any{"setpointC": setpoint}
				if code, resp := a.call(http.MethodPost, "/api/zones/"+strconv.Itoa(z.Number)+"/setpoint", body); code != http.StatusOK || resp["queued"] != true {
					t.Errorf("zone %d setpoint: code=%d %v, want 200 queued", z.Number, code, resp)
				}
			}

			// The periodic work of the engine runs on the layout without a panic.
			if _, err := a.d.do(func(e *tado.EmuState) (any, error) {
				d := a.d
				d.tickCounters(e)
				d.tickHomeState(e)
				d.tickWindows(e)
				d.tickPairWindow(e)
				d.tickEnergy()
				d.tickFlowLimits(e)
				d.tickUTCOffset(e)
				_, err := e.ExpireBoosts(d.retryNow())
				return nil, err
			}); err != nil {
				t.Errorf("tick: %v", err)
			}

			for _, away := range []bool{true, false} {
				if code, resp := a.call(http.MethodPatch, "/api/home", map[string]any{"away": away}); code != http.StatusOK {
					t.Errorf("away=%v: code=%d %v, want 200", away, code, resp)
				}
			}

			for _, path := range []string{
				"/api/health", "/api/home", "/api/zones", "/api/nodes", "/api/boiler",
				"/api/install", "/api/install/zones", "/api/install/circuits",
			} {
				if code, resp := a.call(http.MethodGet, path, nil); code != http.StatusOK {
					t.Errorf("GET %s: code=%d %v, want 200", path, code, resp)
				}
			}
			want := http.StatusServiceUnavailable
			if tc.boiler {
				want = http.StatusOK
			}
			if code, resp := a.call(http.MethodGet, "/api/heatsource/capabilities", nil); code != want {
				t.Errorf("GET /api/heatsource/capabilities: code=%d %v, want %d", code, resp, want)
			}
		})
	}
}
