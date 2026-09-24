package daemon

import (
	"context"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"freedo/internal/install"
	"freedo/internal/radio"
	"freedo/tado"
)

// stateClock is millisecond aligned: the boost expiry is stored as unix
// milliseconds, so a finer clock cannot survive a round trip.
var stateClock = time.Date(2026, 8, 27, 10, 30, 0, 123_000_000, time.UTC)

func newTestStore(t *testing.T, path string) *stateStore {
	t.Helper()
	store, err := openStateStore(path)
	if err != nil {
		t.Fatalf("openStateStore(%s): %v", path, err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// populatedDesiredState fills every table of one snapshot. The documents come
// from the emulator, so they pass the validation RestoreDesiredState applies.
func populatedDesiredState(t *testing.T) tado.DesiredState {
	t.Helper()
	cfg := defaultInstall.EmuConfig()
	cfg.Now = func() time.Time { return stateClock }
	emu := tado.NewEmu(cfg, tado.NewUnhandledLogger(io.Discard))
	camera, err := defaultInstall.Resolve("VA1000000005")
	if err != nil {
		t.Fatal(err)
	}
	boiler, err := defaultInstall.Resolve(dhwNode)
	if err != nil {
		t.Fatal(err)
	}
	cameraKey := hex.EncodeToString(camera.WireEUI())
	if err := emu.EnqueueChildLock(cameraKey, 10, true); err != nil {
		t.Fatal(err)
	}
	fahrenheit := true
	if err := emu.EnqueueConfig(cameraKey, 10, tado.ConfigWrite{TempUnitF: &fahrenheit}); err != nil {
		t.Fatal(err)
	}
	offlineSchedule := false
	if err := emu.EnqueueConfig(cameraKey, 10, tado.ConfigWrite{OfflineScheduleEnabled: &offlineSchedule}); err != nil {
		t.Fatal(err)
	}
	if err := emu.EnqueueFlowSetpoint(hex.EncodeToString(boiler.WireEUI()), flowCircuitID, 48); err != nil {
		t.Fatal(err)
	}
	if err := emu.SetSchedule(cameraKey, []tado.ScheduleProgram{{DayMask: 0x7f, Entries: []tado.ScheduleEntry{
		{Minute: 0, SetpointCenti: 500, Enabled: true},
		{Minute: 360, SetpointCenti: 700, Enabled: true},
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := emu.EnqueueBoost(cameraKey, 10, true); err != nil {
		t.Fatal(err)
	}
	state := emu.ExportDesiredState()
	// The emulator fills these two from received traffic, which no test frame
	// here carries.
	state.ZoneState["10|z/s"] = []byte{1, 2, 3}
	state.UTCSettled[cameraKey] = 60
	state.InterfaceModule[hex.EncodeToString(boiler.WireEUI())] = 71

	gate := tado.NewEmu(cfg, tado.NewUnhandledLogger(io.Discard))
	if err := gate.RestoreDesiredState(state); err != nil {
		t.Fatalf("the fixture snapshot is invalid: %v", err)
	}
	return gate.ExportDesiredState()
}

func TestStateStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	want := populatedDesiredState(t)
	for _, m := range []int{
		len(want.ChildLock), len(want.TempUnitF), len(want.FlowC), len(want.Config),
		len(want.Schedule), len(want.ZoneState), len(want.UTCSettled), len(want.Boost),
		len(want.OfflineSchedule), len(want.InterfaceModule),
	} {
		if m == 0 {
			t.Fatalf("the fixture snapshot leaves a table empty: %+v", want)
		}
	}
	store := newTestStore(t, path)
	if err := store.Save(context.Background(), want, 1); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{path, path + "-wal", path + "-shm"} {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode=%#o, want 0600", name, info.Mode().Perm())
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := newTestStore(t, path)
	got, err := reopened.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("loaded state differs\n got: %#v\nwant: %#v", got, want)
	}
	for key, timer := range want.Boost {
		if got.Boost[key].ExpiresAt != timer.ExpiresAt {
			t.Errorf("boost %s expiry=%s, want %s", key, got.Boost[key].ExpiresAt, timer.ExpiresAt)
		}
	}

	restored := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	if err := restored.RestoreDesiredState(got); err != nil {
		t.Fatal(err)
	}
	if exported := restored.ExportDesiredState(); !reflect.DeepEqual(exported, want) {
		t.Errorf("restored state differs\n got: %#v\nwant: %#v", exported, want)
	}
}

func TestStateStoreRoundTripEmptyState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	want := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard)).ExportDesiredState()
	store := newTestStore(t, path)
	if err := store.Save(context.Background(), want, 1); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("loaded state differs\n got: %#v\nwant: %#v", got, want)
	}
}

// A save replaces the whole state, so a key the new snapshot leaves out is gone
// from the database.
func TestStateStoreSaveReplacesEverything(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store := newTestStore(t, path)
	first := populatedDesiredState(t)
	if err := store.Save(context.Background(), first, 1); err != nil {
		t.Fatal(err)
	}
	second := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard)).ExportDesiredState()
	second.FlowC["1"] = 55
	if err := store.Save(context.Background(), second, 2); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, second) {
		t.Errorf("loaded state differs\n got: %#v\nwant: %#v", got, second)
	}
}

func TestLoadDesiredStateFreshInstall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	store, err := loadDesiredState(path, emu)
	if err != nil {
		t.Fatalf("loadDesiredState(%s): %v", path, err)
	}
	defer store.Close()
	empty := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard)).ExportDesiredState()
	if got := emu.ExportDesiredState(); !reflect.DeepEqual(got, empty) {
		t.Errorf("a fresh install started with %#v", got)
	}
}

// A database the daemon cannot read stops startup. Nothing is moved aside: the
// operator keeps the file as it is.
func TestLoadDesiredStateCorruptDatabaseFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	if err := os.WriteFile(path, []byte("this is not a database at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	store, err := loadDesiredState(path, emu)
	if err == nil {
		store.Close()
		t.Fatal("a corrupt database should stop startup")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "state.db" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the state directory holds %v, want only state.db", names)
	}
}

// A zone body the emulator exports empty arrives as a nil slice. It must not
// reach the BLOB NOT NULL column as NULL: that would fail every later save.
func TestStateStoreSavesEmptyBlobs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store := newTestStore(t, path)
	want := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard)).ExportDesiredState()
	want.ZoneState["10|z/s"] = nil
	if err := store.Save(context.Background(), want, 1); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	body, ok := got.ZoneState["10|z/s"]
	if !ok {
		t.Fatalf("the empty zone body is gone: %#v", got.ZoneState)
	}
	if len(body) != 0 {
		t.Errorf("zone body=%x, want empty", body)
	}
	// The store must stay writable: the first save is the one that used to break.
	if err := store.Save(context.Background(), want, 2); err != nil {
		t.Fatal(err)
	}
}

// The stored snapshot version reaches RestoreDesiredState, so a database written
// by a future layout stops startup.
func TestStateStoreStateVersionStopsStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store := newTestStore(t, path)
	if err := store.Save(context.Background(), populatedDesiredState(t), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('state_version', '99')`); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 99 {
		t.Fatalf("loaded version=%d, want the stored 99", got.Version)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	if reopened, err := loadDesiredState(path, emu); err == nil {
		reopened.Close()
		t.Error("startup on an unknown state version should fail")
	}
}

// Two exporters can reach the commit out of order. The older snapshot is
// skipped, and skipping is not an error.
// TestZoneStateSavesAreBounded: a stream of z/s reports saves at most once per
// zoneStateSaveEvery, and the tick saves the last change the bound held back.
func TestZoneStateSavesAreBounded(t *testing.T) {
	store := newTestStore(t, filepath.Join(t.TempDir(), "state.db"))
	in := windowInstall(t, nil)
	clock := newTestClock(windowStart)
	cfg := in.EmuConfig()
	cfg.Now = clock.now
	emu := tado.NewEmu(cfg, tado.NewUnhandledLogger(io.Discard))
	d := newDaemon(in, emu, radio.NewMock(nil), true, "mock")
	d.retryNow = clock.now
	d.state = store
	saves := func() uint64 {
		t.Helper()
		seq, _, err := install.ReadMetaUint(context.Background(), store.db, "save_seq")
		if err != nil {
			t.Fatal(err)
		}
		return seq
	}
	const reports = 20
	for i := range reports {
		body := tado.TLV{Tag: "6280", Val: []byte{0x08, byte(i)}}.Encode()
		d.handleFrame(uplinkFrame(t, in, "VA1000000002", true, tado.CoAPReq{
			Type: "CON", Code: "0.03", MID: 0x5200 + uint16(i), Path: []string{"z", "s"},
			Query: []string{"lid=3"}, Payload: body,
		}), true)
		clock.advance(time.Second)
	}
	if got, want := saves(), uint64(reports*time.Second/zoneStateSaveEvery); got != want {
		t.Errorf("%d reports over %s made %d saves, want %d", reports, reports*time.Second, got, want)
	}
	clock.advance(zoneStateSaveEvery)
	d.flushUnsaved(context.Background(), emu)
	d.flushUnsaved(context.Background(), emu)
	if got, want := saves(), uint64(reports*time.Second/zoneStateSaveEvery)+1; got != want {
		t.Errorf("after the tick: %d saves, want %d", got, want)
	}
	state, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	want := hex.EncodeToString(tado.TLV{Tag: "6280", Val: []byte{0x08, reports - 1}}.Encode())
	if got := hex.EncodeToString(state.ZoneState["3|z/s"]); !strings.Contains(got, want) {
		t.Errorf("stored z/s %s, want the last 6280 %s", got, want)
	}
}

func TestStateStoreSkipsAStaleSaveSequence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store := newTestStore(t, path)
	newer := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard)).ExportDesiredState()
	newer.FlowC["1"] = 55
	older := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard)).ExportDesiredState()
	older.FlowC["1"] = 40
	if err := store.Save(context.Background(), newer, 2); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), older, 1); err != nil {
		t.Fatalf("a stale save should be skipped, not fail: %v", err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, newer) {
		t.Errorf("loaded state differs\n got: %#v\nwant: %#v", got, newer)
	}
}

// The shutdown save is bounded: a context already over its deadline stops the
// write instead of blocking on the disk.
func TestStateStoreSaveHonoursTheContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store := newTestStore(t, path)
	want := populatedDesiredState(t)
	if err := store.Save(context.Background(), want, 1); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	changed := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard)).ExportDesiredState()
	changed.FlowC["1"] = 55
	if err := store.Save(ctx, changed, 2); err == nil {
		t.Fatal("a save on a cancelled context should fail")
	}
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the cancelled save changed the database\n got: %#v\nwant: %#v", got, want)
	}
}

func TestStateStoreSchemaVersionMismatchFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store := newTestStore(t, path)
	if _, err := store.db.Exec(`UPDATE meta SET value = '99' WHERE key = 'schema_version'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); err == nil {
		t.Error("a load on an unknown schema version should fail")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	if reopened, err := loadDesiredState(path, emu); err == nil {
		reopened.Close()
		t.Error("startup on an unknown schema version should fail")
	}
}

// A save replaces the state tables, not meta, so it must not drop the tune.
func TestStateStoreSaveKeepsFSCTRL0(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	ctx := context.Background()
	store := newTestStore(t, path)
	if err := store.SetFSCTRL0(ctx, 0x2f); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, populatedDesiredState(t), 1); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.FSCTRL0(ctx)
	if err != nil || !ok || got != 0x2f {
		t.Fatalf("FSCTRL0=%#02x,%t,%v want 0x2f,true,nil", got, ok, err)
	}
}

// The installation revision is the API concurrency token; the desired state
// keeps its own counters and must not move with it.
func TestReplaceInstallBumpsOnlyTheInstallRevision(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	store := newTestStore(t, path)
	if err := store.Save(ctx, populatedDesiredState(t), 4); err != nil {
		t.Fatal(err)
	}
	for want := uint64(1); want <= 2; want++ {
		rev, err := store.ReplaceInstall(ctx, defaultInstall)
		if err != nil {
			t.Fatalf("ReplaceInstall: %v", err)
		}
		if rev != want {
			t.Errorf("ReplaceInstall returned rev %d, want %d", rev, want)
		}
		got, err := store.InstallRev(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("InstallRev = %d, want %d", got, want)
		}
	}
	seq, found, err := install.ReadMetaUint(ctx, store.db, "save_seq")
	if err != nil {
		t.Fatal(err)
	}
	if !found || seq != 4 {
		t.Errorf("save_seq = %d (found %v), want 4", seq, found)
	}
	version, err := store.stateVersion()
	if err != nil {
		t.Fatal(err)
	}
	if version != tado.DesiredStateVersion {
		t.Errorf("state_version = %d, want %d", version, tado.DesiredStateVersion)
	}
	state, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Config) == 0 {
		t.Error("the installation write dropped the desired state")
	}
}
