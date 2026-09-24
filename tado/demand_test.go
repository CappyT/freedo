package tado

import (
	"io"
	"reflect"
	"strings"
	"testing"
)

// Heat-demand chain tests. The bodies below are the ones the capture of
// 2026-08-27 carries; the value widths follow from the frame lengths, which the
// CCM* cipher preserves exactly:
//
//	h/7654321/hvac/mon with one 2-byte attribute      81 B
//	h/7654321/hvac/mon with 044c, 0452, 0457          90 B  -> 0457 holds 1 byte
//	h/7654321/z/10/act with 40a0 alone                80 B  -> body 4 B
//	h/7654321/c/1/act with 4040 4000 4080 2090        93 B  -> body 18 B
//
// The c/1/act path is one byte shorter than the hvac/mon one and one byte
// shorter than the z/10/act one, so the three bodies are 5, 4 and 18 bytes. With
// 4040 and 4000 holding centi-degrees in 2 bytes, 4080 and 40a0 sharing a width
// and 2090 holding a flag, the only solution is a 1-byte value for 40a0, 4080
// and 2090. Tag 4160 takes the same width as the other percent attributes.

const (
	capCamera    = "02a6cefa07c51b00" // valve VA1000000005, zone 10
	capCorridoio = "05a2cafa07c51b00" // wall thermostat RU1000000001, circuit 1
)

func demandEmu(t *testing.T) *EmuState {
	t.Helper()
	return NewEmu(fixtureInstall(t).EmuConfig(), NewUnhandledLogger(io.Discard))
}

func zoneOf(t *testing.T, e *EmuState, n int) ZoneView {
	t.Helper()
	for _, z := range e.Snapshot().Zones {
		if z.Number == n {
			return z
		}
	}
	t.Fatalf("zone %d is missing from the snapshot", n)
	return ZoneView{}
}

func circuitOf(t *testing.T, e *EmuState, id int) CircuitView {
	t.Helper()
	for _, c := range e.Snapshot().Circuits {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("circuit %d is missing from the snapshot", id)
	return CircuitView{}
}

// TestDemandChainPopulatesState walks the whole chain the capture shows, in the
// order it happens, and reads the result from the state view. No frame carries
// tag 6340, 6060, 6400 or 2040.
func TestDemandChainPopulatesState(t *testing.T) {
	e := demandEmu(t)
	camera, corridoio := mustHex(capCamera), mustHex(capCorridoio)

	// 1. The valve publishes the zone demand on the home plane.
	e.OnFrame(uplinkFrame(t, camera, "0.03",
		[]string{"h", "7654321", "z", "10", "act"}, nil,
		TLV{Tag: "40a0", Val: []byte{100}}.Encode()))
	if got := zoneOf(t, e, 10); got.DemandPct != 100 {
		t.Errorf("zone 10 demand = %d, want 100 (%+v)", got.DemandPct, got)
	}

	// 2. The same demand reaches the wall thermostat as a 2.05 we relay. It
	// carries no path and no query, so the zone comes from the publisher.
	e.OnFrame(zoneUplink(t, camera, corridoio, CoAPReq{
		Type: "NON", Code: "2.05", MID: 0x7801, Token: []byte{0x78},
		Payload: concat(
			TLV{Tag: "4060", Val: []byte{0x06, 0xd4}}.Encode(), // 17.48 C
			TLV{Tag: "40a0", Val: []byte{100}}.Encode(),
			TLV{Tag: "0135", Val: []byte{0x02, 0x86}}.Encode(), // 64.60 %
		),
	}))
	got := zoneOf(t, e, 10)
	if got.DemandPct != 100 || got.TempC != 17.48 {
		t.Errorf("zone 10 after the relayed 2.05 = %+v, want demand 100 and 17.48 C", got)
	}
	if got.Source != "a602" {
		t.Errorf("zone 10 source = %q, want the publishing valve a602", got.Source)
	}

	// 3. The thermostat aggregates the circuit.
	e.OnFrame(uplinkFrame(t, corridoio, "0.03",
		[]string{"h", "7654321", "c", "1", "act"}, nil,
		concat(
			TLV{Tag: "4040", Val: []byte{0x06, 0xd4}}.Encode(), // 17.48 C
			TLV{Tag: "4000", Val: []byte{0x09, 0xc4}}.Encode(), // 25.00 C
			TLV{Tag: "4080", Val: []byte{100}}.Encode(),
			TLV{Tag: "2090", Val: []byte{1}}.Encode(),
		)))
	if c := circuitOf(t, e, 1); c.DemandPct != 100 || !c.HeatActive || c.Source != "a205" {
		t.Errorf("circuit 1 = %+v, want demand 100, heat active, source a205", c)
	}

	// 4. The thermostat pushes the circuit heat percent back to the zone. The
	// datagram is addressed to the valve, so we only route it.
	e.OnFrame(zoneUplink(t, corridoio, camera, CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x3a01, Token: []byte{0x3a},
		Path: []string{"z", "cpe"}, Query: []string{"lid=10"},
		Payload: TLV{Tag: "4160", Val: []byte{100}}.Encode(),
	}))
	got = zoneOf(t, e, 10)
	if got.HeatPct != 100 || got.DemandPct != 100 {
		t.Errorf("zone 10 after z/cpe = %+v, want heat 100 and demand 100", got)
	}

	// The role that elects the publisher is the d/config 015e record, 0d0a in
	// the capture: role 0x0d, zone 10.
	if got.Reporter != "a602" {
		t.Errorf("zone 10 reporter = %q, want a602 from the 015e record", got.Reporter)
	}
	recs, err := e.NodeRoles("VA1000000005")
	if err != nil {
		t.Fatal(err)
	}
	if want := []NodeTableRec{{Role: 0x0d, Node: 10}}; !reflect.DeepEqual(recs, want) {
		t.Errorf("camera 015e records = %v, want %v", recs, want)
	}

	// Nothing in the chain needed the phantom tags.
	for _, ns := range e.nodes {
		for _, tag := range []string{"6340", "6060", "6400", "2040"} {
			if _, ok := ns.raw[tag]; ok {
				t.Errorf("node %s cached %s, which never appears on air", ns.Short, tag)
			}
		}
	}
	// Every frame of the chain is a resource and a tag the coordinator knows.
	if counts := e.log.Counts(); len(counts) != 0 {
		t.Errorf("the chain logged unhandled traffic: %v", counts)
	}
}

// TestDemandReportsMergeAsDeltas: every report resource sends only the
// attributes that changed, so an omitted one keeps its last value.
func TestDemandReportsMergeAsDeltas(t *testing.T) {
	e := demandEmu(t)
	camera, corridoio := mustHex(capCamera), mustHex(capCorridoio)

	circuit := func(tlvs ...TLV) {
		t.Helper()
		var body []byte
		for _, tl := range tlvs {
			body = append(body, tl.Encode()...)
		}
		e.OnFrame(uplinkFrame(t, corridoio, "0.03",
			[]string{"h", "7654321", "c", "1", "act"}, nil, body))
	}

	circuit(
		TLV{Tag: "4080", Val: []byte{100}},
		TLV{Tag: "2090", Val: []byte{1}},
	)
	// The capture's own transition: the demand drops to 0 while heat_active
	// stays 1 for another 44 s, then 2090 alone goes to 0.
	circuit(TLV{Tag: "4080", Val: []byte{0}})
	if c := circuitOf(t, e, 1); c.DemandPct != 0 || !c.HeatActive {
		t.Errorf("circuit 1 = %+v, want demand 0 with heat still active", c)
	}
	circuit(TLV{Tag: "2090", Val: []byte{0}})
	if c := circuitOf(t, e, 1); c.HeatActive {
		t.Errorf("circuit 1 = %+v, want heat inactive", c)
	}

	// The zone report has two publishers: the valve writes 40a0, the thermostat
	// writes 4160. Neither may clear the other.
	e.OnFrame(uplinkFrame(t, camera, "0.03",
		[]string{"h", "7654321", "z", "10", "act"}, nil,
		TLV{Tag: "40a0", Val: []byte{100}}.Encode()))
	e.OnFrame(zoneUplink(t, corridoio, camera, CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x3a02, Token: []byte{0x3a},
		Path: []string{"z", "cpe"}, Query: []string{"lid=10"},
		Payload: TLV{Tag: "4160", Val: []byte{0}}.Encode(),
	}))
	if got := zoneOf(t, e, 10); got.DemandPct != 100 || got.HeatPct != 0 {
		t.Errorf("zone 10 = %+v, want demand 100 kept and heat 0", got)
	}
}

// TestBoilerReportMergesAsDelta: hvac/mon carries one or two attributes per
// frame. The boiler view must keep the ones a later frame omits.
func TestBoilerReportMergesAsDelta(t *testing.T) {
	e := demandEmu(t)
	corridoio := mustHex(capCorridoio)
	mon := func(body []byte) {
		t.Helper()
		e.OnFrame(uplinkFrame(t, corridoio, "0.03",
			[]string{"h", "7654321", "hvac", "mon"}, nil, body))
	}

	// 1787837678.326 control_setpoint_C=40.00C boiler_0458=5496
	mon(concat(
		TLV{Tag: "0450", Val: []byte{0x0f, 0xa0}}.Encode(),
		TLV{Tag: "0458", Val: []byte{0x15, 0x78}}.Encode(),
	))
	// 1787837682.325 boiler_mod=53 boiler_flag=1
	mon(concat(
		TLV{Tag: "0452", Val: []byte{0x00, 0x35}}.Encode(),
		TLV{Tag: "0457", Val: []byte{0x01}}.Encode(),
	))
	// 1787837693.326 boiler_mod=37, alone
	mon(TLV{Tag: "0452", Val: []byte{0x00, 0x25}}.Encode())

	b := e.Snapshot().Boiler
	if !isPtr(b.ControlSetpointC, 40.0) {
		t.Errorf("control setpoint = %v, want 40 kept across the deltas", b.ControlSetpointC)
	}
	if !isPtr(b.ModulationPct, 37) || !b.Active {
		t.Errorf("boiler = %+v, want modulation 37 and the burner flag kept", b)
	}
}

// TestObservedNodeRolesSupersedeServedOnes: a device that publishes its own
// d/config document reassigns the role, because 015e is where the role lives.
func TestObservedNodeRolesSupersedeServedOnes(t *testing.T) {
	e := demandEmu(t)
	bagno := mustHex("04a3defa07c51b00") // VA1000000007, zone 2

	if got := zoneOf(t, e, 10).Reporter; got != "a602" {
		t.Fatalf("zone 10 reporter = %q before any observation, want a602", got)
	}
	e.OnFrame(uplinkFrame(t, bagno, "0.03",
		[]string{"d", "VA1000000007", "config"}, nil,
		concat(
			TLV{Tag: "015c", Val: []byte{0x00, 0x74, 0xcb, 0xb1}}.Encode(),
			TLV{Tag: "015e", Val: []byte{0x0d, 0x0a}}.Encode(),
		)))
	recs, err := e.NodeRoles("VA1000000007")
	if err != nil {
		t.Fatal(err)
	}
	if want := []NodeTableRec{{Role: 0x0d, Node: 10}}; !reflect.DeepEqual(recs, want) {
		t.Fatalf("bagno 015e records = %v, want %v", recs, want)
	}
	// Two devices now claim zone 10; the tie resolves on the short address.
	if got := zoneOf(t, e, 10).Reporter; got != "a304" {
		t.Errorf("zone 10 reporter = %q, want a304", got)
	}
}

// TestInstallWithoutReporterRuns: the user's own installation elects no demand
// reporter and heats. Validation accepts it, the d/config 015e records still go
// out with the membership role, and the zone document still builds.
func TestInstallWithoutReporterRuns(t *testing.T) {
	yaml := strings.ReplaceAll(fixtureInstallYAML, ", reporter: VA1000000005", "")
	in, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("an installation that elects no reporter for zone 10 must load: %v", err)
	}
	if !strings.Contains(strings.Join(in.Warnings(), "\n"), "elects no demand reporter") {
		t.Errorf("warnings = %v, want one about the missing election", in.Warnings())
	}

	camera, err := in.Resolve("VA1000000005")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := initialNodeTable(in, camera), []NodeTableRec{{Role: 0x05, Node: 10}}; !reflect.DeepEqual(got, want) {
		t.Errorf("camera node table = %v, want the plain membership role %v", got, want)
	}
	if _, err := BuildZoneConfigDocument(in, 10); err != nil {
		t.Errorf("zone 10 config document: %v", err)
	}
	// The single member answers for the zone temperature, so the offset write
	// path keeps a target.
	if d, err := in.MeasuringDevice(10); err != nil || d != camera {
		t.Errorf("MeasuringDevice(10) = %v, %v, want the only member", d, err)
	}

	// The chain still fills the state view: nothing in it needs an election.
	e := NewEmu(in.EmuConfig(), NewUnhandledLogger(io.Discard))
	e.OnFrame(uplinkFrame(t, mustHex(capCamera), "0.03",
		[]string{"h", "7654321", "z", "10", "act"}, nil,
		TLV{Tag: "40a0", Val: []byte{100}}.Encode()))
	got := zoneOf(t, e, 10)
	if got.DemandPct != 100 {
		t.Errorf("zone 10 demand = %d, want 100", got.DemandPct)
	}
	if got.Reporter != "" {
		t.Errorf("zone 10 reporter = %q, want none: no 015e record elects a member", got.Reporter)
	}
}

// TestDemandPathParsing pins the two path readers against the capture's paths.
func TestDemandPathParsing(t *testing.T) {
	zone := []struct {
		path string
		want int
		ok   bool
	}{
		{"h/7654321/z/10/act", 10, true},
		{"h/7654321/z/0/s", 0, true},
		{"z/s", 0, false},
		{"z/cpe", 0, false},
		{"h/7654321/c/1/act", 0, false},
	}
	for _, tt := range zone {
		got, ok := zoneReportPath(tt.path)
		if got != tt.want || ok != tt.ok {
			t.Errorf("zoneReportPath(%q) = %d, %v, want %d, %v", tt.path, got, ok, tt.want, tt.ok)
		}
	}
	circuit := []struct {
		path string
		want int
		ok   bool
	}{
		{"h/7654321/c/1/act", 1, true},
		{"h/7654321/c/1/config", 0, false}, // the flow-setpoint document
		{"h/7654321/z/10/act", 0, false},
		{"h/7654321/hvac/mon", 0, false},
	}
	for _, tt := range circuit {
		got, ok := circuitReportPath(tt.path)
		if got != tt.want || ok != tt.ok {
			t.Errorf("circuitReportPath(%q) = %d, %v, want %d, %v", tt.path, got, ok, tt.want, tt.ok)
		}
	}
}
