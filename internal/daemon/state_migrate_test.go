package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"freedo/internal/install"
	"freedo/tado"
)

// legacyDatabase writes a database of an older schema version at path: the
// current layout with drops applied, stamped with version. The caller adds its
// rows through the returned handle and closes it.
func legacyDatabase(t *testing.T, path, version string, drops ...string) *sql.DB {
	t.Helper()
	store, err := install.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range drops {
		if _, err := old.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := old.Exec(`UPDATE meta SET value = ? WHERE key = 'schema_version'`, version); err != nil {
		t.Fatal(err)
	}
	return old
}

// A version "1" database holds desired state and nothing else. Opening it adds
// the installation tables and keeps every stored row.
func TestOpenStateStoreMigratesFromVersionOne(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	old := legacyDatabase(t, path, "1",
		`DROP TABLE zone_member`, `DROP TABLE zone`, `DROP TABLE circuit`,
		`DROP TABLE device`, `DROP TABLE install`, `DROP TABLE net_key`)
	for _, stmt := range []string{
		`INSERT INTO meta (key, value) VALUES ('save_seq', '7')`,
		`INSERT INTO flow_c (zone_id, celsius) VALUES (1, 48)`,
		`INSERT INTO child_lock (key, locked) VALUES ('02a6cefa07c51b00|10', 1)`,
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
	if version != install.SchemaVersion {
		t.Errorf("schema version = %q, want %q", version, install.SchemaVersion)
	}
	seq, found, err := install.ReadMetaUint(context.Background(), store.db, "save_seq")
	if err != nil {
		t.Fatal(err)
	}
	if !found || seq != 7 {
		t.Errorf("save_seq = %d (found %v), want 7", seq, found)
	}
	state, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := state.FlowC["1"]; !ok || got != 48 {
		t.Errorf("flow setpoint = %v (found %v), want 48", got, ok)
	}
	if !state.ChildLock["02a6cefa07c51b00|10"] {
		t.Error("the migration lost the child lock row")
	}
	if _, _, err := store.LoadInstall(context.Background()); !errors.Is(err, install.ErrNoInstall) {
		t.Errorf("LoadInstall on a migrated database: %v, want ErrNoInstall", err)
	}
}

// A version "3" database has no offline_schedule table, no
// options.offline_schedule row and schedules in the retired field 3 encoding.
// Opening it adds the table, re-encodes the schedules with their enabled values
// kept, and the stored installation reads the offline schedule as on.
func TestOpenStateStoreMigratesOfflineSchedule(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	old := legacyDatabase(t, path, "3", `DROP TABLE offline_schedule`)
	if _, err := old.Exec(`INSERT INTO install (key, value) VALUES ('home_id', '1234')`); err != nil {
		t.Fatal(err)
	}
	// A schedule the version "3" encoder wrote: field 3 is "off", so 00 is
	// enabled, 01 disabled, and an entry without field 3 is enabled.
	legacy := mustDecodeHex(t, "121d"+"087f"+
		"1207"+"0800"+"10b410"+"1800"+ // 00:00 21.00 C enabled
		"1208"+"088603"+"10c00c"+"1801"+ // 06:30 16.00 C disabled
		"1206"+"08a80a"+"10880e") // 22:00 18.00 C, no field 3: enabled
	const schedKey = "02a6cefa07c51b00"
	if _, err := old.Exec(`INSERT INTO sched_doc (key, body, etag) VALUES (?, ?, ?)`,
		schedKey, legacy, []byte{1, 2, 3, 4, 5, 6, 7, 8}); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	store := newTestStore(t, path)
	var version string
	if err := store.db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != install.SchemaVersion {
		t.Errorf("schema version = %q, want %q", version, install.SchemaVersion)
	}
	state, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(state.OfflineSchedule) != 0 {
		t.Errorf("migrated offline schedule overrides = %v, want none", state.OfflineSchedule)
	}
	want := mustDecodeHex(t, "121f"+"087f"+
		"1207"+"0800"+"10b410"+"1801"+
		"1208"+"088603"+"10c00c"+"1800"+
		"1208"+"08a80a"+"10880e"+"1801")
	doc := state.Schedule[schedKey]
	if !bytes.Equal(doc.Body, want) {
		t.Errorf("migrated schedule = %x, want %x", doc.Body, want)
	}
	programs, err := tado.DecodeSchedule(doc.Body)
	if err != nil {
		t.Fatal(err)
	}
	var enabled []bool
	for _, entry := range programs[0].Entries {
		enabled = append(enabled, entry.Enabled)
	}
	if fmt.Sprint(enabled) != "[true false true]" {
		t.Errorf("migrated enabled = %v, want [true false true]", enabled)
	}
	// The ETag moved with the body: the restore gate recomputes and compares it.
	if err := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard)).
		RestoreDesiredState(state); err != nil {
		t.Errorf("the migrated state does not restore: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := newTestStore(t, path).Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := again.Schedule[schedKey]; !bytes.Equal(got.Body, want) || !bytes.Equal(got.ETag, doc.ETag) {
		t.Errorf("a second open rewrote the schedule: %x/%x, want %x/%x", got.Body, got.ETag, want, doc.ETag)
	}

	in, _, err := newTestStore(t, path).LoadInstall(context.Background())
	if err != nil {
		t.Fatalf("LoadInstall: %v", err)
	}
	if in.Options.OfflineSchedule != nil || !in.Options.OfflineScheduleEnabled() {
		t.Errorf("migrated offline_schedule = %v/%t, want unset and on",
			in.Options.OfflineSchedule, in.Options.OfflineScheduleEnabled())
	}
}

func mustDecodeHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestOpenStateStoreMigratesZoneWindow: a version "4" database gets the
// zone_window table and keeps its schedules as they are, because they already
// hold the current encoding.
func TestOpenStateStoreMigratesZoneWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	old := legacyDatabase(t, path, "4", `DROP TABLE zone_window`)
	current := tado.EncodeSchedulePrograms([]tado.ScheduleProgram{{DayMask: 0x7f, Entries: []tado.ScheduleEntry{
		{Minute: 0, SetpointCenti: 2100, Enabled: true},
		{Minute: 390, SetpointCenti: 1600, Enabled: false},
	}}})
	const schedKey = "02a6cefa07c51b00"
	if _, err := old.Exec(`INSERT INTO sched_doc (key, body, etag) VALUES (?, ?, ?)`,
		schedKey, current, []byte{1, 2, 3, 4, 5, 6, 7, 8}); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	store := newTestStore(t, path)
	var version string
	if err := store.db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != install.SchemaVersion {
		t.Errorf("schema version = %q, want %q", version, install.SchemaVersion)
	}
	state, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := state.Schedule[schedKey].Body; !bytes.Equal(got, current) {
		t.Errorf("migrated schedule = %x, want it unchanged %x", got, current)
	}

	since := time.Date(2026, 1, 5, 5, 0, 0, 0, time.UTC)
	want := map[int]zoneWindow{
		3: {windowExternal: {since: since, deadline: since.Add(2 * time.Hour)}},
		5: {
			windowDevice:   {since: since, deadline: since.Add(15 * time.Minute)},
			windowExternal: {since: since.Add(time.Minute), deadline: since.Add(time.Hour)},
		},
	}
	wantOwed := []windowWrite{{zone: 3, target: "08a4defa07c51b00"}, {zone: 5, target: "010acefa07c51b00"}}
	if err := store.SetZoneWindows(context.Background(), want, wantOwed); err != nil {
		t.Fatal(err)
	}
	got, owed, err := store.ZoneWindows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) || fmt.Sprint(owed) != fmt.Sprint(wantOwed) {
		t.Errorf("stored windows = %v owed %v, want %v owed %v", got, owed, want, wantOwed)
	}
}

// TestOpenStateStoreMigratesInterfaceModule: a version "5" database gets the
// interface_module table and keeps its schedules as they are, because they
// already hold the current encoding.
func TestOpenStateStoreMigratesInterfaceModule(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	old := legacyDatabase(t, path, "5", `DROP TABLE interface_module`)
	current := tado.EncodeSchedulePrograms([]tado.ScheduleProgram{{DayMask: 0x7f, Entries: []tado.ScheduleEntry{
		{Minute: 0, SetpointCenti: 2100, Enabled: true},
		{Minute: 390, SetpointCenti: 1600, Enabled: false},
	}}})
	const schedKey = "02a6cefa07c51b00"
	if _, err := old.Exec(`INSERT INTO sched_doc (key, body, etag) VALUES (?, ?, ?)`,
		schedKey, current, []byte{1, 2, 3, 4, 5, 6, 7, 8}); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	store := newTestStore(t, path)
	var version string
	if err := store.db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != install.SchemaVersion {
		t.Errorf("schema version = %q, want %q", version, install.SchemaVersion)
	}
	state, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := state.Schedule[schedKey].Body; !bytes.Equal(got, current) {
		t.Errorf("migrated schedule = %x, want it unchanged %x", got, current)
	}
	state.InterfaceModule["05a2cafa07c51b00"] = 71
	if err := store.Save(context.Background(), state, 1); err != nil {
		t.Fatal(err)
	}
	reloaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.InterfaceModule["05a2cafa07c51b00"]; got != 71 {
		t.Errorf("stored interface module = %d, want 71", got)
	}
}

// TestOpenStateStoreMigratesPresets: a version "7" database gets the preset
// and zone_program tables, empty, and keeps its served documents and its
// hot-water program as they are. The first daemon start then builds the
// Default preset from them: a member document that does not decode stays out
// and does not fail the start.
func TestOpenStateStoreMigratesPresets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	in := presetInstall(t, nil)
	old := legacyDatabase(t, path, "7", `DROP TABLE zone_program`, `DROP TABLE preset`)
	officePrograms := weekPrograms(
		tado.ScheduleEntry{Minute: 0, SetpointCenti: 1900, Enabled: true},
		tado.ScheduleEntry{Minute: 420, SetpointCenti: 1600, Enabled: true},
	)
	hwPrograms := weekPrograms(tado.ScheduleEntry{Minute: 0, SetpointCenti: 4500, Enabled: true})
	office := tado.EncodeSchedulePrograms(officePrograms)
	broken := []byte{0xff}
	docs := map[string][]byte{wireOf(t, in, "VA1000000002"): office, wireOf(t, in, "VA1000000005"): broken}
	for key, body := range docs {
		h := fnv.New64a()
		h.Write(body)
		if _, err := old.Exec(`INSERT INTO sched_doc (key, body, etag) VALUES (?, ?, ?)`, key, body, h.Sum(nil)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := old.Exec(`INSERT INTO meta (key, value) VALUES ('hot_water_program', ?)`,
		hex.EncodeToString(tado.EncodeSchedulePrograms(hwPrograms))); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	store := newTestStore(t, path)
	var version string
	if err := store.db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != "8" {
		t.Errorf("schema version = %q, want %q", version, "8")
	}
	for _, table := range []string{"preset", "zone_program"} {
		var n int
		if err := store.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s after the schema migration: %d rows, err %v, want an empty table", table, n, err)
		}
	}
	state, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for key, body := range docs {
		if got := state.Schedule[key].Body; !bytes.Equal(got, body) {
			t.Errorf("migrated schedule %s = %x, want it unchanged %x", key, got, body)
		}
	}

	clock := newTestClock(time.Date(2026, 1, 5, 5, 0, 0, 0, time.UTC))
	d, url, _ := startStoredDaemon(t, in, clock, store)
	if _, name := presetHome(t, url); name != "Default" {
		t.Errorf("the active preset is %q, want Default", name)
	}
	got := engineValue(t, d, func() map[int][]tado.ScheduleProgram {
		out := map[int][]tado.ScheduleProgram{}
		for _, zone := range []int{0, 3, 10} {
			if home, ok := d.zonePrograms(zone); ok {
				out[zone] = home
			}
		}
		return out
	})
	if want := map[int][]tado.ScheduleProgram{0: hwPrograms, 3: officePrograms}; !reflect.DeepEqual(got, want) {
		t.Errorf("the Default home programs are %v, want %v", got, want)
	}
}
