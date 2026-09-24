package tado

import (
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
)

const fullInstallPath = "testdata/install_full.yaml"

func loadFullInstall(t *testing.T) *Install {
	t.Helper()
	data, err := os.ReadFile(fullInstallPath)
	if err != nil {
		t.Fatal(err)
	}
	in, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse(%s): %v", fullInstallPath, err)
	}
	return in
}

// TestFullInstallConfigLoads is the regression fixture: the committed 33-device
// installation must stay valid. It carries the room labels an installation
// declared before the label rule holds, so the only findings are duplicates.
func TestFullInstallConfigLoads(t *testing.T) {
	in := loadFullInstall(t)
	for _, w := range in.Warnings() {
		if !strings.Contains(w, "duplicates devices[") {
			t.Errorf("warning = %q, want only duplicate room labels", w)
		}
	}
	if got := len(in.Devices); got != 33 {
		t.Errorf("devices = %d, want 33", got)
	}
	if got := len(in.DevicesByKind(KindThermostat)); got != 3 {
		t.Errorf("thermostats = %d, want 3", got)
	}
	if got := len(in.DevicesByKind(KindValve)); got != 30 {
		t.Errorf("valves = %d, want 30", got)
	}
	if got := len(in.Circuits); got != 3 {
		t.Errorf("circuits = %d, want 3", got)
	}
	if in.HomeID != 1234567 {
		t.Errorf("home_id = %d, want 1234567", in.HomeID)
	}
	if got := hex.EncodeToString(in.NetworkKey()); got != "000102030405060708090a0b0c0d0e0f" {
		t.Errorf("network key = %s", got)
	}
}

// TestFullInstallZoneShape: sparse numbering, one hot-water zone, and exactly one
// demand reporter and measuring device per heating zone.
func TestFullInstallZoneShape(t *testing.T) {
	in := loadFullInstall(t)
	wantNumbers := []int{0, 1, 2, 3, 5, 6, 8, 9, 10}
	if len(in.Zones) != len(wantNumbers) {
		t.Fatalf("zones = %d, want %d", len(in.Zones), len(wantNumbers))
	}
	for i, n := range wantNumbers {
		if in.Zones[i].Number != n {
			t.Errorf("zones[%d].number = %d, want %d", i, in.Zones[i].Number, n)
		}
	}
	dhw := 0
	for i := range in.Zones {
		z := &in.Zones[i]
		if z.Kind == ZoneHotWater {
			dhw++
			continue
		}
		rep, err := in.DemandReporter(z.Number)
		if err != nil {
			t.Errorf("zone %d reporter: %v", z.Number, err)
			continue
		}
		members, err := in.ZoneMembers(z.Number)
		if err != nil {
			t.Fatalf("zone %d members: %v", z.Number, err)
		}
		if !containsDevice(members, rep) {
			t.Errorf("zone %d reporter %s is not a member", z.Number, rep.Ref())
		}
		sensor, err := in.MeasuringDevice(z.Number)
		if err != nil {
			t.Errorf("zone %d measuring device: %v", z.Number, err)
		} else if !containsDevice(members, sensor) {
			t.Errorf("zone %d measuring device %s is not a member", z.Number, sensor.Ref())
		}
		if _, err := in.ZoneCircuit(z.Number); err != nil {
			t.Errorf("zone %d circuit: %v", z.Number, err)
		}
		if src, err := in.ZoneHeatSource(z.Number); err != nil {
			t.Errorf("zone %d heat source: %v", z.Number, err)
		} else if src.Kind != KindThermostat {
			t.Errorf("zone %d heat source %s is a %s", z.Number, src.Ref(), src.Kind)
		}
	}
	if dhw != 1 {
		t.Errorf("hot-water zones = %d, want 1", dhw)
	}
	valves := 0
	for i := range in.Zones {
		for _, ref := range in.Zones[i].Members {
			d, err := in.Resolve(ref)
			if err != nil {
				t.Fatalf("member %q: %v", ref, err)
			}
			if d.Kind == KindValve {
				valves++
			}
		}
	}
	if valves != 30 {
		t.Errorf("valve memberships = %d, want 30 (every valve in exactly one zone)", valves)
	}
}

// TestAmbiguousLabelResolution: a shared room label names several devices, and
// resolving it must name them all instead of picking one.
func TestAmbiguousLabelResolution(t *testing.T) {
	in := loadFullInstall(t)
	_, err := in.Resolve("soggiorno")
	var amb *AmbiguousRefError
	if !errors.As(err, &amb) {
		t.Fatalf("Resolve(soggiorno) error = %v, want *AmbiguousRefError", err)
	}
	if len(amb.Candidates) != 5 {
		t.Errorf("candidates = %v, want 5", amb.Candidates)
	}
	for _, want := range []string{"RU2001000001", "VA2001000004"} {
		if !strings.Contains(amb.Error(), want) {
			t.Errorf("error %q does not name %s", amb.Error(), want)
		}
	}
	if got := len(in.DevicesByLabel("bagno")); got != 4 {
		t.Errorf("DevicesByLabel(bagno) = %d, want 4", got)
	}
}

// TestDuplicateLabelWarns: the management API keeps a room label unique, but an
// installation declared before that rule has to stay loadable. The finding
// travels as a warning, so the daemon starts and the operator reads what to
// rename. The comparison ignores case, the way Resolve reads a label.
func TestDuplicateLabelWarns(t *testing.T) {
	in := base()
	in.Devices[2].Label = "Cucina"
	if err := in.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	got := strings.Join(in.Warnings(), "\n")
	if !strings.Contains(got, `devices[2].label: "Cucina" duplicates devices[1]`) {
		t.Errorf("warnings = %v, want the duplicate label reported", in.Warnings())
	}
}

func TestResolveIdentifierForms(t *testing.T) {
	in := loadFullInstall(t)
	for _, ref := range []string{
		"VA2001000001",
		"va2001000001",
		"01026a3107c51b00",
		"00:1b:c5:07:31:6a:02:01",
		"0201",
		"taverna e uffici", // not a device label
	} {
		d, err := in.Resolve(ref)
		if ref == "taverna e uffici" {
			if err == nil {
				t.Errorf("Resolve(%q) resolved to %s, want unknown", ref, d.Ref())
			}
			continue
		}
		if err != nil {
			t.Errorf("Resolve(%q): %v", ref, err)
			continue
		}
		if d.Serial != "VA2001000001" {
			t.Errorf("Resolve(%q) = %s, want VA2001000001", ref, d.Serial)
		}
	}
	if _, err := in.Resolve(""); err == nil {
		t.Error("Resolve(\"\") = nil error, want one")
	}
}

func TestFullInstallLookups(t *testing.T) {
	in := loadFullInstall(t)
	dhw, err := in.HotWaterTarget()
	if err != nil {
		t.Fatalf("HotWaterTarget: %v", err)
	}
	if dhw.Serial != "RU2001000001" {
		t.Errorf("HotWaterTarget = %s, want RU2001000001", dhw.Serial)
	}
	z, ok := in.HotWaterZone()
	if !ok || z.Number != 0 {
		t.Errorf("HotWaterZone = %v, %v, want zone 0", z, ok)
	}
	src, err := in.HeatSource(4)
	if err != nil || src.Serial != "RU2001000003" {
		t.Errorf("HeatSource(4) = %v, %v, want RU2001000003", src, err)
	}
	if _, err := in.HeatSource(3); err == nil {
		t.Error("HeatSource(3) = nil error, want no such circuit")
	}
	if _, err := in.ZoneByNumber(4); err == nil {
		t.Error("ZoneByNumber(4) = nil error, want no such zone")
	}
	c, err := in.ZoneCircuit(9)
	if err != nil || c.ID != 4 {
		t.Errorf("ZoneCircuit(9) = %v, %v, want circuit 4", c, err)
	}
	if got := in.CircuitsFor(src); len(got) != 1 || got[0].ID != 4 {
		t.Errorf("CircuitsFor(RU2001000003) = %v, want circuit 4", got)
	}
	valve, err := in.Resolve("VA2001000030")
	if err != nil {
		t.Fatal(err)
	}
	if got := in.CircuitsFor(valve); got != nil {
		t.Errorf("CircuitsFor(a valve) = %v, want none", got)
	}
	if in.Options.UTCOffsetMin == nil || *in.Options.UTCOffsetMin != 60 {
		t.Errorf("utc_offset_min = %v, want 60", in.Options.UTCOffsetMin)
	}
}

func TestEmuBridging(t *testing.T) {
	in := loadFullInstall(t)
	zones := in.EmuZones()
	if len(zones) != 9 {
		t.Fatalf("EmuZones = %d zones, want 9", len(zones))
	}
	if got := zones[1]; len(got) != 5 || got[0] != "05a2cafa07c51b00" {
		t.Errorf("EmuZones[1] = %v", got)
	}
	cfg := in.EmuConfig()
	if len(cfg.Key) != 16 {
		t.Errorf("EmuConfig.Key = %d bytes, want 16", len(cfg.Key))
	}
	if got := hex.EncodeToString(cfg.SelfEUI); got != "aa016a3107c51b00" {
		t.Errorf("EmuConfig.SelfEUI = %s", got)
	}
	if cfg.UTCOffsetMin == nil || *cfg.UTCOffsetMin != 60 {
		t.Errorf("EmuConfig.UTCOffsetMin = %v, want the option's 60", cfg.UTCOffsetMin)
	}
}

func TestParseEUI(t *testing.T) {
	tests := []struct {
		in, want, err string
	}{
		{in: "05a2cafa07c51b00", want: "05a2cafa07c51b00"},
		{in: " 05A2CAFA07C51B00 ", want: "05a2cafa07c51b00"},
		{in: "00:1b:c5:07:fa:ca:a2:05", want: "05a2cafa07c51b00"},
		{in: "00-1b-c5-07-fa-ca-a2-05", want: "05a2cafa07c51b00"},
		{in: "", err: "missing EUI"},
		{in: "05a2cafa", err: "8 bytes"},
		{in: "05a2cafa07c51b0000", err: "8 bytes"},
		{in: "zza2cafa07c51b00", err: "not hex"},
	}
	for _, tt := range tests {
		b, err := ParseEUI(tt.in)
		if tt.err != "" {
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Errorf("ParseEUI(%q) error = %v, want %q", tt.in, err, tt.err)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseEUI(%q): %v", tt.in, err)
			continue
		}
		if got := hex.EncodeToString(b); got != tt.want {
			t.Errorf("ParseEUI(%q) = %s, want %s", tt.in, got, tt.want)
		}
	}
}

// base is the smallest valid install: one thermostat driving one circuit, one
// heating zone with a valve reporter, one hot-water zone.
func base() *Install {
	circuit := 1
	return &Install{
		Version:       ConfigVersion,
		HomeID:        1234567,
		NetworkKeyHex: "000102030405060708090a0b0c0d0e0f",
		Coordinator:   Coordinator{EUI: "aa016a3107c51b00"},
		Devices: []Device{
			{Kind: KindThermostat, EUI: "05a2cafa07c51b00", Serial: "RU2001000001", Label: "corridoio"},
			{Kind: KindValve, EUI: "01026a3107c51b00", Serial: "VA2001000001", Label: "cucina"},
			{Kind: KindValve, EUI: "03046a3107c51b00", Serial: "VA2001000002", Label: "sala da pranzo"},
		},
		Zones: []Zone{
			{Number: 0, Kind: ZoneHotWater, Members: []string{"RU2001000001"}},
			{
				Number:   1,
				Kind:     ZoneHeating,
				Circuit:  &circuit,
				Members:  []string{"VA2001000001", "VA2001000002"},
				Reporter: refList{"VA2001000001"},
			},
		},
		Circuits: []Circuit{{ID: 1, HeatSource: "RU2001000001"}},
	}
}

func TestBaseIsValid(t *testing.T) {
	in := base()
	if err := in.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if w := in.Warnings(); len(w) != 0 {
		t.Errorf("warnings = %v, want none", w)
	}
	// Validate is what resolves the key, and re-running must not accumulate.
	if len(in.NetworkKey()) != 16 {
		t.Errorf("NetworkKey = %d bytes, want 16", len(in.NetworkKey()))
	}
	if err := in.Validate(); err != nil {
		t.Fatalf("second Validate: %v", err)
	}
	if w := in.Warnings(); len(w) != 0 {
		t.Errorf("warnings after second run = %v, want none", w)
	}
}

func TestValidationErrors(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*Install)
		want string
	}{
		{"bad version", func(in *Install) { in.Version = 99 }, "unsupported schema version 99"},
		{"no home id", func(in *Install) { in.HomeID = 0 }, "home_id"},
		{"no key", func(in *Install) { in.NetworkKeyHex = "" }, "network_key: missing"},
		{"key not hex", func(in *Install) { in.NetworkKeyHex = "zz0102" }, "not hex"},
		{"key too short", func(in *Install) { in.NetworkKeyHex = "0001020304" }, "must be 16 bytes"},
		{"no coordinator", func(in *Install) { in.Coordinator.EUI = "" }, "coordinator.eui: missing"},
		{"bad coordinator eui", func(in *Install) { in.Coordinator.EUI = "aa01" }, "must be 8 bytes"},
		{"no devices", func(in *Install) { in.Devices = nil }, "no device declared"},
		{"bad device eui", func(in *Install) { in.Devices[1].EUI = "0102" }, "devices[1].eui"},
		{"unknown device kind", func(in *Install) { in.Devices[1].Kind = "boiler" }, `unknown kind "boiler"`},
		{"missing device kind", func(in *Install) { in.Devices[1].Kind = "" }, "devices[1].kind: missing"},
		{"duplicate eui", func(in *Install) {
			in.Devices[2].EUI = in.Devices[1].EUI
		}, "duplicates devices[1]"},
		{"duplicate serial", func(in *Install) {
			in.Devices[2].Serial = in.Devices[1].Serial
		}, "devices[2].serial"},
		{"unknown member", func(in *Install) {
			in.Zones[1].Members = append(in.Zones[1].Members, "VA9999999999")
		}, `unknown device "VA9999999999"`},
		{"ambiguous member", func(in *Install) {
			in.Devices[2].Label = in.Devices[1].Label
			in.Zones[1].Members = []string{"cucina"}
		}, `"cucina" matches 2 devices`},
		{"device claims missing zone", func(in *Install) {
			n := 7
			in.Devices[1].Zone = &n
		}, "claims zone 7, which is not declared"},
		{"device claims foreign zone", func(in *Install) {
			n := 0
			in.Devices[1].Zone = &n
		}, "is not among zones[number=0].members"},
		{"zone number too large", func(in *Install) { in.Zones[1].Number = 32 }, "out of range"},
		{"negative zone number", func(in *Install) { in.Zones[1].Number = -1 }, "is negative"},
		{"duplicate zone number", func(in *Install) { in.Zones[1].Number = 0 }, "duplicates zones[0]"},
		{"unknown zone kind", func(in *Install) { in.Zones[1].Kind = "cooling" }, `unknown kind "cooling"`},
		{"missing zone kind", func(in *Install) { in.Zones[1].Kind = "" }, "zones[1].kind: missing"},
		{"two reporters", func(in *Install) {
			in.Zones[1].Reporter = refList{"VA2001000001", "VA2001000002"}
		}, "elects 2 demand reporters"},
		{"reporter not a member", func(in *Install) {
			in.Zones[1].Reporter = refList{"RU2001000001"}
		}, "is not a member of zone 1"},
		{"measuring device not a member", func(in *Install) {
			in.Zones[1].MeasuringDevice = "RU2001000001"
		}, "is not a member of zone 1"},
		{"zone binds unknown circuit", func(in *Install) {
			n := 3
			in.Zones[1].Circuit = &n
		}, "circuit 3, which is not declared"},
		{"two hot-water zones", func(in *Install) {
			in.Zones = append(in.Zones, Zone{Number: 2, Kind: ZoneHotWater, Members: []string{"RU2001000001"}})
		}, "2 hot-water zones declared"},
		{"circuit without heat source", func(in *Install) {
			in.Circuits[0].HeatSource = ""
		}, "has no heat source"},
		{"heat source is a valve", func(in *Install) {
			in.Circuits[0].HeatSource = "VA2001000001"
		}, "is a valve"},
		{"unknown heat source", func(in *Install) {
			in.Circuits[0].HeatSource = "RU0000000000"
		}, `unknown device "RU0000000000"`},
		{"duplicate circuit id", func(in *Install) {
			in.Circuits = append(in.Circuits, Circuit{ID: 1, HeatSource: "RU2001000001"})
		}, "duplicates circuits[0]"},
		{"circuit id out of range", func(in *Install) {
			in.Circuits[0].ID = 40
			n := 40
			in.Zones[1].Circuit = &n
		}, "circuits[0].id: 40 is out of range"},
		{"utc offset out of range", func(in *Install) {
			in.Options.UTCOffsetMin = cfgPtr(2000)
		}, "out of range -1440..1440"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := base()
			tt.mut(in)
			err := in.Validate()
			if err == nil {
				t.Fatalf("Validate = nil, want error containing %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Validate = %q, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestValidationWarnings(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*Install)
		want string
	}{
		// 17 warns but is legitimate: this installation runs a live zone 17.
		{"risky zone number", func(in *Install) { in.Zones[1].Number = 17 }, "is above 10"},
		{"risky circuit id", func(in *Install) {
			in.Circuits[0].ID = 12
			n := 12
			in.Zones[1].Circuit = &n
		}, "circuit table holds 11 entries"},
		{"unbound heating zone", func(in *Install) { in.Zones[1].Circuit = nil }, "falls back to circuit 9"},
		// The user's own installation elects no reporter and heats. The role is
		// already in each device's store; the coordinator only writes 015e.
		{"no reporter", func(in *Install) { in.Zones[1].Reporter = nil }, "elects no demand reporter"},
		{"coordinator without tado suffix", func(in *Install) {
			in.Coordinator.EUI = "aa016a3107c51b01"
		}, "does not end in the tado suffix"},
		{"device reuses coordinator eui", func(in *Install) {
			in.Devices[1].EUI = in.Coordinator.EUI
		}, "is the coordinator EUI"},
		{"empty zone", func(in *Install) { in.Zones[0].Members = nil }, "has no member"},
		{"hot water without thermostat", func(in *Install) {
			in.Zones[0].Members = []string{"VA2001000001"}
		}, "no thermostat member"},
		{"hot water with reporter", func(in *Install) {
			in.Zones[0].Reporter = refList{"RU2001000001"}
		}, "elects a demand reporter"},
		{"hot water bound to a circuit", func(in *Install) {
			n := 1
			in.Zones[0].Circuit = &n
		}, "publishes over the inter-MCU link"},
		{"valve in two zones", func(in *Install) {
			in.Zones[0].Kind = ZoneHeating
			in.Zones[0].Members = []string{"VA2001000001"}
			in.Zones[0].Reporter = refList{"VA2001000001"}
			in.Zones[0].Circuit = in.Zones[1].Circuit
		}, "accepts only one logical id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := base()
			tt.mut(in)
			if err := in.Validate(); err != nil {
				t.Fatalf("Validate = %v, want a warning and no error", err)
			}
			if !strings.Contains(strings.Join(in.Warnings(), "\n"), tt.want) {
				t.Errorf("warnings = %v, want one containing %q", in.Warnings(), tt.want)
			}
		})
	}
}

func TestZeroValueInstall(t *testing.T) {
	in := &Install{}
	err := in.Validate()
	if err == nil {
		t.Fatal("Validate = nil, want errors")
	}
	for _, want := range []string{"version", "home_id", "network_key", "coordinator.eui", "no device declared"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	// The lookups must not panic on an unusable config.
	if _, err := in.Resolve("anything"); err == nil {
		t.Error("Resolve on a zero Install = nil error")
	}
	if _, ok := in.HotWaterZone(); ok {
		t.Error("HotWaterZone on a zero Install = ok")
	}
	if _, err := in.HotWaterTarget(); err == nil {
		t.Error("HotWaterTarget on a zero Install = nil error")
	}
	if got := in.EmuZones(); len(got) != 0 {
		t.Errorf("EmuZones = %v, want empty", got)
	}
	if in.CoordinatorEUI() != nil || in.NetworkKey() != nil {
		t.Error("CoordinatorEUI/NetworkKey on a zero Install are not nil")
	}
}

// TestValidateCommissionable: a daemon on a fresh database holds a key, a
// coordinator address and nothing else. allowEmpty turns that into a warning
// and leaves every identity check in place.
func TestValidateCommissionable(t *testing.T) {
	empty := base()
	empty.Devices, empty.Zones, empty.Circuits = nil, nil, nil

	if err := empty.ValidateCommissionable(true); err != nil {
		t.Fatalf("ValidateCommissionable(true) on an empty install = %v, want a warning", err)
	}
	if got := strings.Join(empty.Warnings(), "\n"); !strings.Contains(got, "installation empty, waiting for commissioning") {
		t.Errorf("warnings = %v, want the empty-install warning", empty.Warnings())
	}
	if err := empty.ValidateCommissionable(false); err == nil || !strings.Contains(err.Error(), "no device declared") {
		t.Errorf("ValidateCommissionable(false) = %v, want the no-device error", err)
	}
	if err := empty.Validate(); err == nil {
		t.Error("Validate on an empty install = nil error")
	}

	identity := []struct {
		name string
		mut  func(*Install)
		want string
	}{
		{"no key", func(in *Install) { in.NetworkKeyHex = "" }, "network_key: missing"},
		{"key too short", func(in *Install) { in.NetworkKeyHex = "0001020304" }, "must be 16 bytes"},
		{"no coordinator", func(in *Install) { in.Coordinator.EUI = "" }, "coordinator.eui: missing"},
		{"bad coordinator eui", func(in *Install) { in.Coordinator.EUI = "aa01" }, "must be 8 bytes"},
		{"no home id", func(in *Install) { in.HomeID = 0 }, "home_id"},
		{"bad version", func(in *Install) { in.Version = 99 }, "unsupported schema version"},
	}
	for _, tt := range identity {
		t.Run(tt.name, func(t *testing.T) {
			in := base()
			in.Devices, in.Zones, in.Circuits = nil, nil, nil
			tt.mut(in)
			err := in.ValidateCommissionable(true)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("ValidateCommissionable(true) = %v, want it to contain %q", err, tt.want)
			}
		})
	}

	// Half declared is not empty: the zone references still have to resolve.
	half := base()
	half.Devices = nil
	if err := half.ValidateCommissionable(true); err == nil {
		t.Error("ValidateCommissionable(true) accepted zones without devices")
	}

	// The management API declares a zone before it pairs the first member, so a
	// zone with nothing in it yet is a warning while commissioning.
	starting := base()
	starting.Devices, starting.Circuits = nil, nil
	starting.Zones = []Zone{{Number: 1, Name: "sala", Kind: ZoneHeating}}
	if err := starting.ValidateCommissionable(true); err != nil {
		t.Fatalf("ValidateCommissionable(true) on a zone without devices = %v, want a warning", err)
	}
	if got := strings.Join(starting.Warnings(), "\n"); !strings.Contains(got, "no device declared, waiting for commissioning") {
		t.Errorf("warnings = %v, want the no-device warning", starting.Warnings())
	}
	if err := starting.ValidateCommissionable(false); err == nil || !strings.Contains(err.Error(), "no device declared") {
		t.Errorf("ValidateCommissionable(false) = %v, want the no-device error", err)
	}

	full := base()
	if err := full.ValidateCommissionable(true); err != nil {
		t.Errorf("ValidateCommissionable(true) on a full install = %v", err)
	}
	if w := full.Warnings(); len(w) != 0 {
		t.Errorf("warnings = %v, want none", w)
	}
}

// TestValidateCommissionableIntermediateStates: every step the management API
// leaves behind between two complete installations has to pass. The gate keeps
// them as warnings, so a half-built installation is serveable and the operator
// still reads what is missing.
func TestValidateCommissionableIntermediateStates(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*Install)
		want string
	}{
		{"a device in no zone", func(in *Install) {
			in.Zones, in.Circuits = nil, nil
		}, ""},
		{"a zone with no member", func(in *Install) {
			in.Zones[1].Members, in.Zones[1].Reporter = nil, nil
		}, "zone 1 has no member"},
		{"members but no reporter", func(in *Install) {
			in.Zones[1].Reporter = nil
		}, "elects no demand reporter"},
		{"a heat source in no zone", func(in *Install) {
			in.Zones = nil
			in.Zones = append(in.Zones, Zone{Number: 1, Kind: ZoneHeating, Members: []string{"VA2001000001"},
				Reporter: refList{"VA2001000001"}})
		}, ""},
		{"no circuit at all", func(in *Install) {
			in.Circuits = nil
			in.Zones[1].Circuit = nil
		}, "binds to no circuit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := base()
			tc.mut(in)
			if err := in.ValidateCommissionable(true); err != nil {
				t.Fatalf("ValidateCommissionable(true) = %v, want it accepted", err)
			}
			if tc.want == "" {
				return
			}
			if got := strings.Join(in.Warnings(), "\n"); !strings.Contains(got, tc.want) {
				t.Errorf("warnings = %v, want one about %q", in.Warnings(), tc.want)
			}
		})
	}
}

func TestParseYAMLForms(t *testing.T) {
	const good = `
version: 1
home_id: 1234567
network_key: "000102030405060708090a0b0c0d0e0f"
coordinator:
  eui: aa016a3107c51b00
devices:
  - kind: thermostat
    eui: 05a2cafa07c51b00
    serial: RU2001000001
  - kind: valve
    eui: 01026a3107c51b00
    serial: VA2001000001
zones:
  - number: 1
    kind: heating
    circuit: 1
    members: [VA2001000001]
    reporter: VA2001000001
circuits:
  - id: 1
    heat_source: RU2001000001
`
	in, err := Parse([]byte(good))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if in.Options.UTCOffsetMin != nil {
		t.Errorf("utc_offset_min = %v with no options block, want unset", in.Options.UTCOffsetMin)
	}

	// reporter also takes a list, so a config that elects two is rejected
	// rather than silently truncated.
	twoReporters := strings.Replace(good,
		"reporter: VA2001000001", "reporter: [VA2001000001, RU2001000001]", 1)
	if _, err := Parse([]byte(twoReporters)); err == nil ||
		!strings.Contains(err.Error(), "elects 2 demand reporters") {
		t.Errorf("two reporters: err = %v", err)
	}

	// A typo in a key is an error, not a silently ignored field.
	typo := strings.Replace(good, "home_id:", "home_di:", 1)
	if _, err := Parse([]byte(typo)); err == nil || !strings.Contains(err.Error(), "home_di") {
		t.Errorf("unknown field: err = %v", err)
	}

	if _, err := Parse(nil); err == nil || !strings.Contains(err.Error(), "empty document") {
		t.Errorf("empty document: err = %v", err)
	}
	if _, err := Parse([]byte("version: [1,2\n")); err == nil {
		t.Error("malformed yaml: err = nil")
	}

	// The keys that moved to the bootstrap file or into the database are out of
	// the schema: a document that still carries one is refused, not ignored.
	for _, moved := range []string{
		"network_key_file: net.key\n",
		"options:\n  pan_id: 0xabcd\n",
	} {
		if _, err := Parse([]byte(good + moved)); err == nil ||
			!strings.Contains(err.Error(), "not found in type tado.") {
			t.Errorf("%q: err = %v, want an unknown-field error", moved, err)
		}
	}
}

func TestZoneKindTLV(t *testing.T) {
	if got := ZoneHeating.TLV6020(); got != 0x01 {
		t.Errorf("heating = %#02x, want 0x01", got)
	}
	if got := ZoneHotWater.TLV6020(); got != 0x02 {
		t.Errorf("hotwater = %#02x, want 0x02", got)
	}
}

func TestDeviceAccessors(t *testing.T) {
	d := Device{Kind: KindValve, EUI: "01026a3107c51b00", Serial: "VA2001000001", Label: "cucina"}
	if got := hex.EncodeToString(d.WireEUI()); got != "01026a3107c51b00" {
		t.Errorf("WireEUI = %s", got)
	}
	if got := d.Short(); got != "0201" {
		t.Errorf("Short = %s, want 0201", got)
	}
	if got := d.Ref(); got != "VA2001000001 (cucina)" {
		t.Errorf("Ref = %q", got)
	}
	if got := (&Device{EUI: "01026a3107c51b00"}).Ref(); got != "01026a3107c51b00" {
		t.Errorf("Ref without serial = %q", got)
	}
	if got := (&Device{Label: "cucina"}).Ref(); got != "cucina" {
		t.Errorf("Ref without serial or EUI = %q", got)
	}
	if bad := (&Device{EUI: "nope"}); bad.WireEUI() != nil || bad.Short() != "" {
		t.Error("accessors on an unusable EUI are not empty")
	}
}
