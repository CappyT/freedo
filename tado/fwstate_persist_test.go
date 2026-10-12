package tado

import (
	"io"
	"slices"
	"strings"
	"testing"
	"time"
)

// officeEUIHex is VA1000000002, the device the removal test un-declares.
const officeEUIHex = "08a4defa07c51b00"

// hallThermostatEUIHex is RU1000000001, the first declared device.
const hallThermostatEUIHex = "05a2cafa07c51b00"

// putFwState feeds a d/fw/state PUT from bedroom through the frame path, so the
// node telemetry fills the way a live report fills it.
func putFwState(t *testing.T, e *EmuState, v FwVersion) {
	t.Helper()
	bedroom := mustHex(NodeByAny("bedroom").WireLE)
	raw := uplinkFrame(t, bedroom, "0.03",
		[]string{"d", "VA1000000005", "fw", "state"}, nil,
		runningFwState(v, FwSlotHasImage|FwSlotActive))
	e.OnFrame(raw)
}

func fwVersion(t *testing.T, major, minor int) FwVersion {
	t.Helper()
	v, err := NewFwVersion(major, minor)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// TestFwStateSurvivesRestoreRefillsTelemetry: the newest fw/state of a device
// persists with the desired state, and a restart refills the node fw_* telemetry
// from the stored body before the device reports again.
func TestFwStateSurvivesRestoreRefillsTelemetry(t *testing.T) {
	now := time.Unix(1000, 0)
	cfg := fixtureInstall(t).EmuConfig()
	cfg.Now = func() time.Time { return now }
	e := NewEmu(cfg, NewUnhandledLogger(io.Discard))
	v := fwVersion(t, 216, 0)
	putFwState(t, e, v)

	bedroom := NodeByAny("bedroom").WireLE
	if got := e.node(mustHex(bedroom)).Telemetry["fw_run_version"]; got != v.String() {
		t.Fatalf("before restore fw_run_version = %q, want %q", got, v.String())
	}
	snap := e.ExportDesiredState()
	if _, ok := snap.FwState[bedroomEUIHex]; !ok {
		t.Fatalf("export has no fw/state for %s: %v", bedroomEUIHex, snap.FwState)
	}

	restarted := NewEmu(cfg, NewUnhandledLogger(io.Discard))
	if err := restarted.RestoreDesiredState(snap); err != nil {
		t.Fatal(err)
	}
	if got := restarted.node(mustHex(bedroom)).Telemetry["fw_run_version"]; got != v.String() {
		t.Fatalf("after restore fw_run_version = %q, want %q", got, v.String())
	}
	put, _ := restarted.FwStateReports("bedroom")
	state, err := ParseFwState(put.Body)
	if err != nil || state.Running.Version != v {
		t.Fatalf("restored report %+v err %v, want version %s", state, err, v)
	}
}

// TestRestoredFwStateStaysStale: the restore keeps the report's time, so an
// upload still refuses a report older than its max age.
func TestRestoredFwStateStaysStale(t *testing.T) {
	now := time.Unix(1000, 0)
	cfg := fixtureInstall(t).EmuConfig()
	cfg.Now = func() time.Time { return now }
	e := NewEmu(cfg, NewUnhandledLogger(io.Discard))
	putFwState(t, e, fwVersion(t, 216, 0))

	restarted := NewEmu(cfg, NewUnhandledLogger(io.Discard))
	if err := restarted.RestoreDesiredState(e.ExportDesiredState()); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.FreshFwState("bedroom", time.Minute); err != nil {
		t.Fatalf("fresh report refused: %v", err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := restarted.FreshFwState("bedroom", time.Minute); err == nil || !strings.Contains(err.Error(), "old") {
		t.Fatalf("stale restored report accepted: %v", err)
	}
}

// TestRestoreDropsABadFwState: a stored fw/state that does not parse, or has no
// time, is dropped on its own. The rest of the desired state still restores, so
// a bad cache entry never stops the daemon start.
func TestRestoreDropsABadFwState(t *testing.T) {
	now := time.Unix(1000, 0)
	cfg := fixtureInstall(t).EmuConfig()
	cfg.Now = func() time.Time { return now }
	e := NewEmu(cfg, NewUnhandledLogger(io.Discard))
	putFwState(t, e, fwVersion(t, 216, 0))
	snap := e.ExportDesiredState()
	snap.FwState[officeEUIHex] = DesiredFwState{Body: []byte{0x01, 0xa0, 0x05}, At: now}
	snap.FwState[hallThermostatEUIHex] = DesiredFwState{Body: snap.FwState[bedroomEUIHex].Body}

	restarted := NewEmu(cfg, NewUnhandledLogger(io.Discard))
	if err := restarted.RestoreDesiredState(snap); err != nil {
		t.Fatalf("a bad fw/state entry failed the restore: %v", err)
	}
	if put, _ := restarted.FwStateReports("bedroom"); len(put.Body) == 0 {
		t.Error("the good fw/state entry was not restored")
	}
	for _, key := range []string{officeEUIHex, hallThermostatEUIHex} {
		if put, _ := restarted.FwStateReports(key); len(put.Body) > 0 {
			t.Errorf("bad fw/state entry %s was restored", key)
		}
	}
}

// TestReadFirmwareStateOncePerDevice: after start the emulator queues one
// d/fw/state read for each declared valve and wall thermostat that holds no
// report, one per device, spaced, and none for a device with a report. An
// unarmed radio queues nothing.
func TestReadFirmwareStateOncePerDevice(t *testing.T) {
	now := time.Unix(1000, 0)
	cfg := fixtureInstall(t).EmuConfig()
	cfg.Now = func() time.Time { return now }
	e := NewEmu(cfg, NewUnhandledLogger(io.Discard))
	e.SetTXArmed(func() bool { return true })
	// The wall thermostat already reported: the read must skip it.
	e.restoreFwState(map[string]DesiredFwState{
		hallThermostatEUIHex: {Body: runningFwState(fwVersion(t, 216, 0), FwSlotHasImage|FwSlotActive), At: now},
	})

	first := e.ReadFirmwareState(now)
	if first == "" || first == hallThermostatEUIHex {
		t.Fatalf("first read = %q, want a declared device without a report", first)
	}
	if q := e.Queue(first); len(q) != 1 || q[0].Desc != "get d/fw/state" {
		t.Fatalf("queued %+v, want one get d/fw/state", q)
	}
	if got := e.ReadFirmwareState(now.Add(time.Second)); got != "" {
		t.Fatalf("second read inside the spacing = %q, want none", got)
	}
	second := e.ReadFirmwareState(now.Add(fwReadSpacing))
	if second == "" || second == first {
		t.Fatalf("second read after the spacing = %q, want the next device", second)
	}
	if q := e.Queue(hallThermostatEUIHex); len(q) != 0 {
		t.Fatalf("the thermostat with a stored report was read: %+v", q)
	}

	unarmed := NewEmu(cfg, NewUnhandledLogger(io.Discard))
	if got := unarmed.ReadFirmwareState(now); got != "" {
		t.Fatalf("unarmed radio queued a read for %q", got)
	}
}

// TestRemovedDeviceLosesStoredFwState: a device that leaves the installation
// loses its stored fw/state, so the persisted copy drops it too.
func TestRemovedDeviceLosesStoredFwState(t *testing.T) {
	e, _ := fixtureEmu(t)
	e.noteFwState(e.fwPut, officeEUIHex,
		runningFwState(fwVersion(t, 216, 0), FwSlotHasImage|FwSlotActive))
	if _, ok := e.ExportDesiredState().FwState[officeEUIHex]; !ok {
		t.Fatal("the report is not in the export")
	}

	without := variantInstall(t, [2]string{officeDevice, ""}, [2]string{zone3, ""})
	_, removed, err := e.SetInstall(without)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(removed, officeEUIHex) {
		t.Fatalf("removed = %v, want %s", removed, officeEUIHex)
	}
	if _, ok := e.ExportDesiredState().FwState[officeEUIHex]; ok {
		t.Fatal("the removed device kept its stored report")
	}
}
