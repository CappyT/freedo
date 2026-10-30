package install

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"freedo/tado"
)

func newTestStore(t *testing.T, path string) *Store {
	t.Helper()
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// A --state typo that lands on somebody else's database must stop startup, and
// must leave that database alone.
func TestOpenStateStoreRefusesAForeignDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "other.db")
	foreign, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := foreign.Exec(`CREATE TABLE states (entity_id TEXT, state TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := foreign.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path)
	if err == nil {
		store.Close()
		t.Fatal("a foreign database should be refused")
	}

	reopened, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	rows, err := reopened.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "states" {
		t.Errorf("the foreign database holds %v, want only states", names)
	}
}

// The tune value survives a restart, so the radio starts near the module
// crystal offset instead of the register default.
func TestStateStoreFSCTRL0RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	ctx := context.Background()
	store := newTestStore(t, path)
	if _, ok, err := store.FSCTRL0(ctx); err != nil || ok {
		t.Fatalf("a fresh database holds a tune: ok=%t err=%v", ok, err)
	}
	if err := store.SetFSCTRL0(ctx, 0x3b); err != nil {
		t.Fatal(err)
	}
	if err := store.SetFSCTRL0(ctx, 0x41); err != nil {
		t.Fatal(err)
	}
	got, ok, err := newTestStore(t, path).FSCTRL0(ctx)
	if err != nil || !ok || got != 0x41 {
		t.Fatalf("FSCTRL0=%#02x,%t,%v want 0x41,true,nil", got, ok, err)
	}
}

// shiftRelease reads ShiftReleaseKey, nil without the row.
func shiftRelease(t *testing.T, s *Store) map[int]int {
	t.Helper()
	var raw string
	switch err := s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, ShiftReleaseKey).Scan(&raw); {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		t.Fatal(err)
	}
	var m map[int]int
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// TestOpenDropsTheRoomCorrections: a database that holds offset mode setpoint
// and the valve calibration opens with every setpoint zone in mode config and
// without the auto_offset rows, meta room_shift and meta auto_offset. A dbg2
// zone keeps its mode. The open moves install_rev once and stores under
// ShiftReleaseKey the shift the valves of each zone still carry: the applied
// one, else setpoint_centi, and none for a zero shift. A second open changes
// nothing and keeps the key.
func TestOpenDropsTheRoomCorrections(t *testing.T) {
	ctx := context.Background()
	store, path := storeWithInstall(t, syntheticInstall(t))
	for _, stmt := range []string{
		`INSERT INTO install (key, value) VALUES ('zone_offset.3', '{"mode":"setpoint","setpoint_centi":-70}')`,
		`INSERT INTO install (key, value) VALUES ('zone_offset.5', '{"mode":"setpoint","setpoint_centi":50}')`,
		`INSERT INTO install (key, value) VALUES ('zone_offset.6', '{"mode":"setpoint"}')`,
		`INSERT INTO install (key, value) VALUES ('zone_offset.8', '{"mode":"dbg2"}')`,
		`INSERT INTO install (key, value) VALUES ('auto_offset.9', '{"enabled":true,"interval_sec":86400}')`,
		`INSERT INTO meta (key, value) VALUES ('room_shift', '{"3":-120}')`,
		`INSERT INTO meta (key, value) VALUES ('auto_offset', '{"9":{"centi":30,"wroteMs":0}}')`,
	} {
		if _, err := store.db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}

	migrated, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]int{3: -120, 5: 50}
	if got := shiftRelease(t, migrated); !reflect.DeepEqual(got, want) {
		t.Errorf("shift release = %v, want %v", got, want)
	}
	in, rev, err := migrated.LoadInstall(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rev != 2 {
		t.Errorf("install_rev = %d, want 2", rev)
	}
	for zone, want := range map[int]string{3: tado.OffsetModeConfig, 5: tado.OffsetModeConfig, 6: tado.OffsetModeConfig, 8: tado.OffsetModeDbg2} {
		if got := in.Options.ZoneOffset(zone).Mode; got != want {
			t.Errorf("zone %d offset mode = %s, want %s", zone, got, want)
		}
	}
	var left int
	if err := migrated.db.QueryRow(`SELECT COUNT(*) FROM install WHERE key LIKE 'auto_offset.%' OR key IN ('zone_offset.3', 'zone_offset.5', 'zone_offset.6')`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d install rows of the corrections left, want none", left)
	}
	if err := migrated.db.QueryRow(`SELECT COUNT(*) FROM meta WHERE key IN ('room_shift', 'auto_offset')`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d meta rows of the corrections left, want none", left)
	}
	if err := migrated.Close(); err != nil {
		t.Fatal(err)
	}

	again := newTestStore(t, path)
	if got := shiftRelease(t, again); !reflect.DeepEqual(got, want) {
		t.Errorf("shift release after the second open = %v, want %v", got, want)
	}
	if _, rev, err := again.LoadInstall(ctx); err != nil || rev != 2 {
		t.Errorf("install_rev on the second open = %d (%v), want 2", rev, err)
	}
}
