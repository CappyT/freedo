package install

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"freedo/tado"
)

// zoneSpec keeps the synthetic installation below readable: one line per zone
// instead of a page of struct literals.
type zoneSpec struct {
	number    int
	name      string
	kind      tado.ZoneKind
	circuit   int // 0 means unbound
	members   []string
	reporter  string
	measuring string
}

// syntheticInstall builds a 33-device installation: 3 wall thermostats, 30
// valves, 3 circuits, one hot-water zone and 8 heating zones of 3 to 5 members.
// It is large enough that member order, the reporter election and the measuring
// fallback all have something to lose in a round trip.
func syntheticInstall(t *testing.T) *tado.Install {
	t.Helper()
	in := &tado.Install{
		Version:       tado.ConfigVersion,
		HomeID:        1234567,
		NetworkKeyHex: "000102030405060708090a0b0c0d0e0f",
		Coordinator: tado.Coordinator{
			EUI:    "aa016a3107c51b00",
			Serial: "IB0000000000",
			Notes:  "synthetic coordinator",
		},
	}
	thermostats := []struct{ eui, serial, label string }{
		{"05a2cafa07c51b00", "RU2001000001", "living"},
		{"41c8693107c51b00", "RU2001000002", "first floor hall"},
		{"5e12693107c51b00", "RU2001000003", "office"},
	}
	for _, d := range thermostats {
		in.Devices = append(in.Devices, tado.Device{
			Kind: tado.KindThermostat, EUI: d.eui, Serial: d.serial, Label: d.label,
		})
	}
	labels := []string{
		"living", "living", "living", "living",
		"kitchen", "kitchen", "kitchen", "kitchen",
		"dining room", "dining room", "dining room", "dining room",
		"master bedroom", "master bedroom", "master bedroom", "master bedroom",
		"first floor hall", "first floor hall", "first floor hall", "first floor hall",
		"bath", "bath", "bath", "bath",
		"office", "office", "office",
		"basement", "basement", "basement",
	}
	kitchen := 2
	for i, label := range labels {
		d := tado.Device{
			Kind:   tado.KindValve,
			EUI:    fmt.Sprintf("%02x%02x6a3107c51b00", 2*i+1, 2*i+2),
			Serial: fmt.Sprintf("VA20010000%02d", i+1),
			Label:  label,
			Notes:  "synthetic",
		}
		if label == "kitchen" {
			d.Zone = &kitchen
		}
		in.Devices = append(in.Devices, d)
	}
	valve := func(n int) string { return fmt.Sprintf("VA20010000%02d", n) }
	specs := []zoneSpec{
		{number: 0, name: "hot water", kind: tado.ZoneHotWater, members: []string{"RU2001000001"}},
		{number: 1, name: "living", kind: tado.ZoneHeating, circuit: 1,
			members:  []string{"RU2001000001", valve(1), valve(2), valve(3), valve(4)},
			reporter: "RU2001000001", measuring: "RU2001000001"},
		{number: 2, name: "kitchen", kind: tado.ZoneHeating, circuit: 1,
			members:  []string{valve(5), valve(6), valve(7), valve(8)},
			reporter: valve(5), measuring: valve(6)},
		{number: 3, name: "dining room", kind: tado.ZoneHeating, circuit: 1,
			members:  []string{valve(9), valve(10), valve(11), valve(12)},
			reporter: valve(9)},
		{number: 5, name: "master bedroom", kind: tado.ZoneHeating, circuit: 2,
			members:  []string{valve(13), valve(14), valve(15), valve(16)},
			reporter: valve(13)},
		{number: 6, name: "first floor hall", kind: tado.ZoneHeating, circuit: 2,
			members:  []string{"RU2001000002", valve(17), valve(18), valve(19), valve(20)},
			reporter: "RU2001000002"},
		{number: 8, name: "bath", kind: tado.ZoneHeating, circuit: 4,
			members:  []string{valve(21), valve(22), valve(23), valve(24)},
			reporter: valve(21)},
		{number: 9, name: "office", kind: tado.ZoneHeating, circuit: 4,
			members:  []string{"RU2001000003", valve(25), valve(26), valve(27)},
			reporter: "RU2001000003"},
		{number: 10, name: "basement", kind: tado.ZoneHeating, circuit: 4,
			members:  []string{valve(28), valve(29), valve(30)},
			reporter: valve(28)},
	}
	for _, spec := range specs {
		z := tado.Zone{
			Number: spec.number, Name: spec.name, Kind: spec.kind,
			Members: spec.members, MeasuringDevice: spec.measuring,
		}
		if spec.circuit != 0 {
			c := spec.circuit
			z.Circuit = &c
		}
		if spec.reporter != "" {
			z.Reporter = append(z.Reporter, spec.reporter)
		}
		in.Zones = append(in.Zones, z)
	}
	in.Circuits = []tado.Circuit{
		{ID: 1, Name: "ground floor", HeatSource: "RU2001000001"},
		{ID: 2, Name: "first floor", HeatSource: "RU2001000002"},
		{ID: 4, Name: "basement and offices", HeatSource: "RU2001000003"},
	}
	offset := 60
	in.Options.UTCOffsetMin = &offset
	if err := in.Validate(); err != nil {
		t.Fatalf("the synthetic install is invalid: %v", err)
	}
	if len(in.Devices) != 33 {
		t.Fatalf("the synthetic install holds %d devices, want 33", len(in.Devices))
	}
	return in
}

// storeWithInstall writes in to a fresh database and returns the store.
func storeWithInstall(t *testing.T, in *tado.Install) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	store := newTestStore(t, path)
	rev, err := store.ReplaceInstall(context.Background(), in)
	if err != nil {
		t.Fatalf("ReplaceInstall: %v", err)
	}
	if rev != 1 {
		t.Fatalf("first ReplaceInstall returned rev %d, want 1", rev)
	}
	return store, path
}

func TestInstallRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   func(t *testing.T) *tado.Install
	}{
		{"capture fixture", func(t *testing.T) *tado.Install { return defaultInstall }},
		{"33 devices", syntheticInstall},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := tc.in(t)
			store, _ := storeWithInstall(t, want)
			got, rev, err := store.LoadInstall(context.Background())
			if err != nil {
				t.Fatalf("LoadInstall: %v", err)
			}
			if rev != 1 {
				t.Errorf("rev = %d, want 1", rev)
			}
			if err := got.Validate(); err != nil {
				t.Fatalf("the loaded install does not validate: %v", err)
			}
			compareInstalls(t, got, want)
		})
	}
}

// TestInstallRoundTripKeepsAwayTemperatures: the away temperature of a zone
// survives the database, as a number with its fraction, and a zone that states
// none loads none.
func TestInstallRoundTripKeepsAwayTemperatures(t *testing.T) {
	in, err := tado.Parse([]byte(fixtureInstallYAML + "  away_temperature_c: {1: 12.5, 3: 18}\n"))
	if err != nil {
		t.Fatal(err)
	}
	store, _ := storeWithInstall(t, in)
	got, _, err := store.LoadInstall(context.Background())
	if err != nil {
		t.Fatalf("LoadInstall: %v", err)
	}
	if want := (map[int]float64{1: 12.5, 3: 18}); !reflect.DeepEqual(got.Options.AwayTemperatures, want) {
		t.Errorf("loaded away temperatures = %v, want %v", got.Options.AwayTemperatures, want)
	}
	bare, _ := storeWithInstall(t, defaultInstall)
	got, _, err = bare.LoadInstall(context.Background())
	if err != nil {
		t.Fatalf("LoadInstall: %v", err)
	}
	if len(got.Options.AwayTemperatures) != 0 {
		t.Errorf("an installation without away temperatures loaded %v", got.Options.AwayTemperatures)
	}
}

// TestInstallRoundTripKeepsBoilerPower: the output and the gas flow of the
// boiler survive the database, and an installation that states none loads none.
func TestInstallRoundTripKeepsBoilerPower(t *testing.T) {
	in, err := tado.Parse([]byte(fixtureInstallYAML +
		"  boiler_max_power_kw: 24.5\n  boiler_min_power_kw: 2.5\n  boiler_max_gas_m3h: 2.65\n  boiler_min_gas_m3h: 0.3\n"))
	if err != nil {
		t.Fatal(err)
	}
	store, _ := storeWithInstall(t, in)
	got, _, err := store.LoadInstall(context.Background())
	if err != nil {
		t.Fatalf("LoadInstall: %v", err)
	}
	if maxKW, minKW, ok := got.Options.BoilerPower(); !ok || maxKW != 24.5 || minKW != 2.5 {
		t.Errorf("loaded output = %v, %v, %v, want 24.5, 2.5, true", maxKW, minKW, ok)
	}
	if maxM3H, minM3H, ok := got.Options.BoilerGas(); !ok || maxM3H != 2.65 || minM3H != 0.3 {
		t.Errorf("loaded gas flow = %v, %v, %v, want 2.65, 0.3, true", maxM3H, minM3H, ok)
	}
	bare, _ := storeWithInstall(t, defaultInstall)
	got, _, err = bare.LoadInstall(context.Background())
	if err != nil {
		t.Fatalf("LoadInstall: %v", err)
	}
	if _, _, ok := got.Options.BoilerPower(); ok {
		t.Error("an installation without a boiler output loaded one")
	}
	if _, _, ok := got.Options.BoilerGas(); ok {
		t.Error("an installation without a gas flow loaded one")
	}
}

// TestInstallRoundTripKeepsRouterIPv4: the router advertisement addresses
// survive the database, and an installation that states none loads none.
func TestInstallRoundTripKeepsRouterIPv4(t *testing.T) {
	in, err := tado.Parse([]byte(fixtureInstallYAML + "  router_gateway: 10.3.2.1\n  router_dns: 10.3.2.2\n"))
	if err != nil {
		t.Fatal(err)
	}
	store, _ := storeWithInstall(t, in)
	got, _, err := store.LoadInstall(context.Background())
	if err != nil {
		t.Fatalf("LoadInstall: %v", err)
	}
	if got.Options.RouterGateway != "10.3.2.1" || got.Options.RouterDNS != "10.3.2.2" {
		t.Errorf("loaded = %q, %q, want 10.3.2.1 and 10.3.2.2", got.Options.RouterGateway, got.Options.RouterDNS)
	}
	bare, _ := storeWithInstall(t, defaultInstall)
	got, _, err = bare.LoadInstall(context.Background())
	if err != nil {
		t.Fatalf("LoadInstall: %v", err)
	}
	if got.Options.RouterGateway != "" || got.Options.RouterDNS != "" {
		t.Errorf("an installation without router addresses loaded %q, %q", got.Options.RouterGateway, got.Options.RouterDNS)
	}
}

// TestInstallRoundTripKeepsFuelUnit: the fuel unit survives the database, and
// an installation that states none loads none and reads m3.
func TestInstallRoundTripKeepsFuelUnit(t *testing.T) {
	in, err := tado.Parse([]byte(fixtureInstallYAML + "  fuel_unit: l\n"))
	if err != nil {
		t.Fatal(err)
	}
	store, _ := storeWithInstall(t, in)
	got, _, err := store.LoadInstall(context.Background())
	if err != nil || got.Options.Fuel() != "l" {
		t.Errorf("loaded unit = %q (%v), want l", got.Options.Fuel(), err)
	}
	bare, _ := storeWithInstall(t, defaultInstall)
	got, _, err = bare.LoadInstall(context.Background())
	if err != nil || got.Options.FuelUnit != "" || got.Options.Fuel() != "m3" {
		t.Errorf("an installation without a unit loaded %q (%v), want none and m3", got.Options.FuelUnit, err)
	}
}

// compareInstalls checks the resolved values, not the reference spellings: the
// database keeps a wire EUI where the config held a serial or a label.
func compareInstalls(t *testing.T, got, want *tado.Install) {
	t.Helper()
	if got.HomeID != want.HomeID {
		t.Errorf("home id = %d, want %d", got.HomeID, want.HomeID)
	}
	if !bytes.Equal(got.NetworkKey(), want.NetworkKey()) {
		t.Errorf("the network key changed in the round trip")
	}
	if !bytes.Equal(got.CoordinatorEUI(), want.CoordinatorEUI()) {
		t.Errorf("coordinator EUI = %x, want %x", got.CoordinatorEUI(), want.CoordinatorEUI())
	}
	if got.Coordinator.Serial != want.Coordinator.Serial || got.Coordinator.Notes != want.Coordinator.Notes {
		t.Errorf("coordinator = %+v, want %+v", got.Coordinator, want.Coordinator)
	}
	switch a, b := got.Options.UTCOffsetMin, want.Options.UTCOffsetMin; {
	case a == nil && b == nil:
	case a == nil || b == nil:
		t.Errorf("utc offset = %v, want %v", a, b)
	case *a != *b:
		t.Errorf("utc offset = %d, want %d", *a, *b)
	}

	if len(got.Devices) != len(want.Devices) {
		t.Fatalf("devices = %d, want %d", len(got.Devices), len(want.Devices))
	}
	for i := range want.Devices {
		g, w := &got.Devices[i], &want.Devices[i]
		if !bytes.Equal(g.WireEUI(), w.WireEUI()) {
			t.Errorf("devices[%d] eui = %x, want %x", i, g.WireEUI(), w.WireEUI())
		}
		if g.Kind != w.Kind || g.Serial != w.Serial || g.Label != w.Label || g.Notes != w.Notes {
			t.Errorf("devices[%d] = %+v, want %+v", i, *g, *w)
		}
		switch {
		case g.Zone == nil && w.Zone == nil:
		case g.Zone == nil || w.Zone == nil:
			t.Errorf("devices[%d] zone = %v, want %v", i, g.Zone, w.Zone)
		case *g.Zone != *w.Zone:
			t.Errorf("devices[%d] zone = %d, want %d", i, *g.Zone, *w.Zone)
		}
	}

	if len(got.Circuits) != len(want.Circuits) {
		t.Fatalf("circuits = %d, want %d", len(got.Circuits), len(want.Circuits))
	}
	for i := range want.Circuits {
		g, w := &got.Circuits[i], &want.Circuits[i]
		if g.ID != w.ID || g.Name != w.Name {
			t.Errorf("circuits[%d] = %+v, want %+v", i, *g, *w)
		}
		gs, err := got.HeatSource(g.ID)
		if err != nil {
			t.Fatalf("circuit %d heat source: %v", g.ID, err)
		}
		ws, err := want.HeatSource(w.ID)
		if err != nil {
			t.Fatalf("circuit %d heat source: %v", w.ID, err)
		}
		if !bytes.Equal(gs.WireEUI(), ws.WireEUI()) {
			t.Errorf("circuits[%d] heat source = %x, want %x", i, gs.WireEUI(), ws.WireEUI())
		}
	}

	if len(got.Zones) != len(want.Zones) {
		t.Fatalf("zones = %d, want %d", len(got.Zones), len(want.Zones))
	}
	for i := range want.Zones {
		g, w := &got.Zones[i], &want.Zones[i]
		if g.Number != w.Number || g.Name != w.Name || g.Kind != w.Kind {
			t.Errorf("zones[%d] = %+v, want %+v", i, *g, *w)
			continue
		}
		switch {
		case g.Circuit == nil && w.Circuit == nil:
		case g.Circuit == nil || w.Circuit == nil:
			t.Errorf("zone %d circuit = %v, want %v", w.Number, g.Circuit, w.Circuit)
		case *g.Circuit != *w.Circuit:
			t.Errorf("zone %d circuit = %d, want %d", w.Number, *g.Circuit, *w.Circuit)
		}
		compareMembers(t, got, want, w.Number)
	}
}

func compareMembers(t *testing.T, got, want *tado.Install, zone int) {
	t.Helper()
	gm, err := got.ZoneMembers(zone)
	if err != nil {
		t.Fatalf("zone %d members: %v", zone, err)
	}
	wm, err := want.ZoneMembers(zone)
	if err != nil {
		t.Fatalf("zone %d members: %v", zone, err)
	}
	if len(gm) != len(wm) {
		t.Fatalf("zone %d has %d members, want %d", zone, len(gm), len(wm))
	}
	for i := range wm {
		if !bytes.Equal(gm[i].WireEUI(), wm[i].WireEUI()) {
			t.Errorf("zone %d member %d = %x, want %x", zone, i, gm[i].WireEUI(), wm[i].WireEUI())
		}
	}
	compareElection(t, "reporter", zone, got.DemandReporter, want.DemandReporter)
	compareElection(t, "measuring device", zone, got.MeasuringDevice, want.MeasuringDevice)
}

func compareElection(t *testing.T, role string, zone int, got, want func(int) (*tado.Device, error)) {
	t.Helper()
	g, gerr := got(zone)
	w, werr := want(zone)
	if (gerr == nil) != (werr == nil) {
		t.Errorf("zone %d %s: got %v, want %v", zone, role, gerr, werr)
		return
	}
	if werr != nil {
		return
	}
	if !bytes.Equal(g.WireEUI(), w.WireEUI()) {
		t.Errorf("zone %d %s = %x, want %x", zone, role, g.WireEUI(), w.WireEUI())
	}
}

// The zone configuration document is built from the member list in order, and
// TestZoneConfigDocumentMatchesCapture pins the zone 10 bytes to the air. A
// round trip that reordered the members would show up here first.
func TestInstallRoundTripKeepsZoneConfigDocuments(t *testing.T) {
	for _, want := range []*tado.Install{defaultInstall, syntheticInstall(t)} {
		store, _ := storeWithInstall(t, want)
		got, _, err := store.LoadInstall(context.Background())
		if err != nil {
			t.Fatalf("LoadInstall: %v", err)
		}
		if err := got.Validate(); err != nil {
			t.Fatalf("the loaded install does not validate: %v", err)
		}
		for i := range want.Zones {
			zone := want.Zones[i].Number
			wantDoc, err := tado.BuildZoneConfigDocument(want, zone)
			if err != nil {
				t.Fatalf("zone %d: %v", zone, err)
			}
			gotDoc, err := tado.BuildZoneConfigDocument(got, zone)
			if err != nil {
				t.Fatalf("zone %d: %v", zone, err)
			}
			if !bytes.Equal(gotDoc.Body, wantDoc.Body) {
				t.Errorf("zone %d body\n got: %x\nwant: %x", zone, gotDoc.Body, wantDoc.Body)
			}
			if !bytes.Equal(gotDoc.ETag, wantDoc.ETag) {
				t.Errorf("zone %d etag = %x, want %x", zone, gotDoc.ETag, wantDoc.ETag)
			}
		}
	}
}

func TestEnsureIdentityIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store := newTestStore(t, path)
	ctx := context.Background()
	created, err := store.EnsureIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("the first EnsureIdentity reported no identity created")
	}
	first, rev, err := store.LoadInstall(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rev != 1 {
		t.Errorf("install_rev = %d, want 1", rev)
	}
	if first.HomeID == 0 {
		t.Error("home id = 0, which no resource path accepts")
	}
	eui, err := tado.ParseEUI(first.Coordinator.EUI)
	if err != nil {
		t.Fatalf("coordinator eui %q: %v", first.Coordinator.EUI, err)
	}
	if !bytes.Equal(eui[len(eui)-len(tado.EUISuffix):], tado.EUISuffix) {
		t.Errorf("coordinator eui = %x, want the tado suffix %x at the end", eui, tado.EUISuffix)
	}
	if key, err := hex.DecodeString(first.NetworkKeyHex); err != nil || len(key) != 16 {
		t.Errorf("network key is %q, want 16 bytes of hex (%v)", first.NetworkKeyHex, err)
	}

	created, err = store.EnsureIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("the second EnsureIdentity generated a new identity")
	}
	second, _, err := store.LoadInstall(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second.NetworkKeyHex != first.NetworkKeyHex ||
		second.Coordinator.EUI != first.Coordinator.EUI ||
		second.HomeID != first.HomeID {
		t.Errorf("the identity changed: %+v, want %+v", second.Coordinator, first.Coordinator)
	}

	other := newTestStore(t, filepath.Join(t.TempDir(), "other.db"))
	if _, err := other.EnsureIdentity(ctx); err != nil {
		t.Fatal(err)
	}
	elsewhere, _, err := other.LoadInstall(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if elsewhere.NetworkKeyHex == first.NetworkKeyHex {
		t.Error("two databases generated the same network key")
	}
	if elsewhere.Coordinator.EUI == first.Coordinator.EUI {
		t.Error("two databases generated the same coordinator EUI")
	}
}

// The zone member rows rest on the foreign keys, which sqlite leaves off unless
// the DSN turns them on.
func TestInstallForeignKeysAreEnforced(t *testing.T) {
	store, _ := storeWithInstall(t, defaultInstall)
	_, err := store.db.Exec(
		`INSERT INTO zone_member (zone_number, device_eui, ord) VALUES (10, 'ffffffffffffffff', 99)`)
	if err == nil {
		t.Fatal("a member row naming no device was accepted")
	}
}

func TestPackageLoadInstall(t *testing.T) {
	store, path := storeWithInstall(t, defaultInstall)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	in, err := LoadInstall(path)
	if err != nil {
		t.Fatalf("LoadInstall(%s): %v", path, err)
	}
	compareInstalls(t, in, defaultInstall)

	empty := filepath.Join(t.TempDir(), "empty.db")
	newTestStore(t, empty).Close()
	if _, err := LoadInstall(empty); !errors.Is(err, ErrNoInstall) {
		t.Errorf("LoadInstall on an empty database: %v, want ErrNoInstall", err)
	}
}

// A version "2" database holds zones without the open window columns. Opening
// it adds them, and every stored zone takes the captured defaults.
func TestOpenStateStoreMigratesZoneOpenWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	noOpenWindow := `
CREATE TABLE zone (
  number  INTEGER PRIMARY KEY,
  name    TEXT NOT NULL DEFAULT '',
  kind    TEXT NOT NULL,
  circuit INTEGER);`
	for _, stmt := range []string{
		stateSchemaDDL,
		noOpenWindow,
		`CREATE TABLE install (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`INSERT INTO meta (key, value) VALUES ('schema_version', '2')`,
		`INSERT INTO zone (number, name, kind) VALUES (4, 'study', 'heating')`,
	} {
		if _, err := old.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	store := newTestStore(t, path)
	var version string
	if err := store.db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion {
		t.Errorf("schema version = %q, want %q", version, SchemaVersion)
	}
	var detection, duration int
	if err := store.db.QueryRow(
		`SELECT open_window_detection, open_window_duration_sec FROM zone WHERE number = 4`).
		Scan(&detection, &duration); err != nil {
		t.Fatal(err)
	}
	if detection != 1 || duration != tado.ZoneOpenWindowDurationSec {
		t.Errorf("zone 4 = %d/%d, want 1/%d", detection, duration, tado.ZoneOpenWindowDurationSec)
	}
}
