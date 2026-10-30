package daemon

// zone_offset_test: the two correction modes of POST /api/zones/{n}/offset,
// what a manual offset does to the learners, and the release of the setpoint
// shift a zone carried in offset mode setpoint. Frames and installation are
// synthetic.

import (
	"context"
	"encoding/hex"
	"fmt"
	"math"
	"net/http"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"freedo/tado"
)

// deliverRequest pulls the head of the queue of target, acknowledges it with
// 2.04 and returns its URI and TLVs, keyed by tag.
func deliverRequest(t *testing.T, d *daemon, in *tado.Install, target string) (string, map[string]string) {
	t.Helper()
	v, err := d.do(func(e *tado.EmuState) (any, error) {
		actions, ok, err := e.NextQueuedDownlink(target)
		if err != nil || !ok || len(actions) == 0 {
			return nil, fmt.Errorf("no queued downlink for %s (err=%v)", target, err)
		}
		return actions[0].Bytes, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	frame, err := tado.ParseData(in.NetworkKey(), v.([]byte))
	if err != nil {
		t.Fatal(err)
	}
	m := tado.ParseApp(frame.Plaintext).CoAP
	tags := map[string]string{}
	for _, tlv := range tado.ParseTLVs(m.Payload) {
		tags[tlv.Tag] = hex.EncodeToString(tlv.Val)
	}
	feed(t, d, in, target, false, tado.CoAPReq{Type: "ACK", Code: "2.04", MID: m.MID, Token: m.Token})
	return m.URI(), tags
}

// configDocOf returns the d/config document the daemon serves to target.
func configDocOf(t *testing.T, d *daemon, target string) tado.DesiredConfigDocument {
	t.Helper()
	v, err := d.do(func(e *tado.EmuState) (any, error) { return e.ExportDesiredState().Config[target], nil })
	if err != nil {
		t.Fatal(err)
	}
	return v.(tado.DesiredConfigDocument)
}

func served0140(doc tado.DesiredConfigDocument) string {
	for _, tlv := range tado.ParseTLVs(doc.Body) {
		if tlv.Tag == "0140" {
			return hex.EncodeToString(tlv.Val)
		}
	}
	return ""
}

// TestZoneOffsetModes: zone 3 starts in mode config. Each mode turns one
// offset of -0.7 C into its own writes on the zone's only valve: config one
// full PUT d/config with a new 015a, dbg2 one PUT d/dbg2/tlvs with the single
// record 0140 under the ETag already served.
func TestZoneOffsetModes(t *testing.T) {
	for _, mode := range []string{tado.OffsetModeConfig, tado.OffsetModeDbg2} {
		t.Run(mode, func(t *testing.T) {
			a := newInstallAPIOn(t, windowInstall(t, nil))
			d := a.d
			in := d.inst.Load()
			valve := wireOf(t, in, "VA1000000002")
			if got := a.get("/api/install/zones/3")["zone"].(map[string]any)["offset_mode"]; got != tado.OffsetModeConfig {
				t.Fatalf("default offset_mode = %v, want config", got)
			}
			if mode != tado.OffsetModeConfig {
				a.mutate(http.MethodPatch, "/api/install/zones/3", map[string]any{"offset_mode": mode})
			}
			dropQueue(t, d, valve)
			before := configDocOf(t, d, valve)

			var res map[string]any
			if code := postJSON(t, http.MethodPost, a.base+"/api/zones/3/offset",
				map[string]any{"offsetC": -0.7}, &res); code != http.StatusOK {
				t.Fatalf("offset: code=%d %v", code, res)
			}
			if n := pending(t, d, valve); n != 1 {
				t.Fatalf("%d queued writes, want one", n)
			}
			uri, tags := deliverRequest(t, d, in, valve)
			after := configDocOf(t, d, valve)
			switch mode {
			case tado.OffsetModeConfig:
				if uri != "d/config" || tags["0140"] != "ffba" || tags["015c"] == "" {
					t.Errorf("config write = %s %v, want the full d/config with 0140=ffba", uri, tags)
				}
				if tags["015a"] == hex.EncodeToString(before.ETag) || tags["015a"] != hex.EncodeToString(after.ETag) {
					t.Errorf("015a = %s, want the new ETag %x in place of %x", tags["015a"], after.ETag, before.ETag)
				}
			case tado.OffsetModeDbg2:
				wantExactTags(t, "dbg2 write", tags, map[string]string{"0140": "ffba"})
				if uri != "d/dbg2/tlvs" {
					t.Errorf("dbg2 write goes to %s, want d/dbg2/tlvs", uri)
				}
				if string(after.ETag) != string(before.ETag) || served0140(after) != "ffba" {
					t.Errorf("served d/config: etag %x 0140 %q, want %x and ffba", after.ETag, served0140(after), before.ETag)
				}
			}
			if n := pending(t, d, valve); n != 0 {
				t.Errorf("%d more writes queued, want none", n)
			}
		})
	}
}

// TestThermostatOffsetServed: zone 1 measures with the wall thermostat, which
// drops the document a d/config write carries. In mode config the offset goes into the
// document it is served, under a new ETag, is stored, and queues only the
// empty PUT d/config that makes the device GET it at once
// (TestThermostatCalibOffsetServed follows the device's GET). The node
// config API takes calibOffset the same way and refuses any other tag. Mode
// dbg2 stays refused: d/dbg2/tlvs is a valve resource.
func TestThermostatOffsetServed(t *testing.T) {
	a := newInstallAPIOn(t, windowInstall(t, nil))
	d := a.d
	in := d.inst.Load()
	thermostat := wireOf(t, in, dhwNode)
	frame := thermostatModuleReport(t, in, thermostat, 71, 0x5e01)
	if _, err := d.do(func(*tado.EmuState) (any, error) {
		d.handleFrame(frame, true)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	dropQueue(t, d, thermostat)
	before := configDocOf(t, d, thermostat)
	if len(before.ETag) == 0 {
		t.Fatal("no thermostat document after the module report")
	}

	var res map[string]any
	if code := postJSON(t, http.MethodPost, a.base+"/api/zones/1/offset",
		map[string]any{"offsetC": -0.7}, &res); code != http.StatusOK {
		t.Fatalf("zone 1 offset: code=%d %v", code, res)
	}
	if res["queued"] != false || res["served"] != true || res["reread"] != true ||
		res["target"] != dhwNode || res["mode"] != tado.OffsetModeConfig {
		t.Errorf("zone 1 offset answer = %v, want served to %s with a re-read trigger, not queued", res, dhwNode)
	}
	if n := pending(t, d, thermostat); n != 1 {
		t.Errorf("%d downlinks queued to the thermostat, want the one re-read trigger", n)
	}
	if uri, tags := deliverRequest(t, d, in, thermostat); uri != "d/config" || len(tags) != 0 {
		t.Errorf("trigger = %s %v, want an empty PUT d/config", uri, tags)
	}
	after := configDocOf(t, d, thermostat)
	if served0140(after) != "ffba" || string(after.ETag) == string(before.ETag) {
		t.Errorf("served d/config: 0140 %q etag %x, want ffba under a new ETag (was %x)", served0140(after), after.ETag, before.ETag)
	}
	stored, err := a.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if doc := stored.Config[thermostat]; served0140(doc) != "ffba" || string(doc.ETag) != string(after.ETag) {
		t.Errorf("stored d/config: 0140 %q etag %x, want ffba and %x", served0140(doc), doc.ETag, after.ETag)
	}

	if code := postJSON(t, http.MethodPost, a.base+"/api/nodes/"+dhwNode+"/config",
		map[string]any{"calibOffset": 0}, &res); code != http.StatusOK {
		t.Fatalf("node config: code=%d %v", code, res)
	}
	if res["queued"] != false || res["served"] != true || res["reread"] != true {
		t.Errorf("node config answer = %v, want served with a re-read trigger, not queued", res)
	}
	if doc := configDocOf(t, d, thermostat); served0140(doc) != "0000" {
		t.Errorf("served 0140 after calibOffset 0 = %q, want 0000", served0140(doc))
	}
	if code := postJSON(t, http.MethodPost, a.base+"/api/nodes/"+dhwNode+"/config",
		map[string]any{"utcOffsetMin": 90}, &res); code != http.StatusBadRequest {
		t.Errorf("utcOffsetMin to the thermostat: code=%d %v, want 400", code, res)
	}

	a.mutate(http.MethodPatch, "/api/install/zones/1", map[string]any{"offset_mode": tado.OffsetModeDbg2})
	dropQueue(t, d, thermostat)
	kept := configDocOf(t, d, thermostat)
	if code := postJSON(t, http.MethodPost, a.base+"/api/zones/1/offset",
		map[string]any{"offsetC": -0.7}, &res); code != http.StatusBadRequest {
		t.Errorf("zone 1 offset in mode dbg2: code=%d %v, want 400", code, res)
	}
	if n := pending(t, d, thermostat); n != 0 {
		t.Errorf("%d downlinks queued to the thermostat in mode dbg2, want none", n)
	}
	if doc := configDocOf(t, d, thermostat); string(doc.Body) != string(kept.Body) {
		t.Error("the refused dbg2 write moved the served document")
	}
}

// deliverAck is deliverRequest with the TLVs alone.
func deliverAck(t *testing.T, d *daemon, in *tado.Install, target string) map[string]string {
	t.Helper()
	_, tags := deliverRequest(t, d, in, target)
	return tags
}

// TestZoneOffsetModePersists: the mode lives in the installation rows, so a
// daemon started on them keeps it. The rows carry no field of the setpoint
// shift or of the valve calibration, offset_mode takes config and dbg2 alone,
// and auto_offset and auto_offset_interval_sec are unknown fields.
func TestZoneOffsetModePersists(t *testing.T) {
	a := newInstallAPIOn(t, windowInstall(t, nil))
	a.mutate(http.MethodPatch, "/api/install/zones/3", map[string]any{"offset_mode": "dbg2"})
	stored := loadStored(t, a)
	if got := stored.Options.ZoneOffset(3); got != (tado.ZoneOffset{Mode: tado.OffsetModeDbg2}) {
		t.Errorf("stored zone 3 offset = %+v, want dbg2", got)
	}

	d, url := newWindowDaemon(t, stored, newTestClock(windowStart), a.store)
	var zone map[string]any
	if code := getJSON(t, url+"/api/install/zones/3", &zone); code != http.StatusOK {
		t.Fatalf("zone after restart: code=%d", code)
	}
	z := zone["zone"].(map[string]any)
	if z["offset_mode"] != "dbg2" {
		t.Errorf("zone after restart = %v, want dbg2", z)
	}
	for _, key := range []string{"setpoint_offset_c", "auto_offset", "auto_offset_interval_sec"} {
		if _, present := z[key]; present {
			t.Errorf("zone row carries %s: %v", key, z)
		}
	}

	valve := wireOf(t, stored, "VA1000000002")
	var res map[string]any
	if code := postJSON(t, http.MethodPost, url+"/api/zones/3/offset", map[string]any{"offsetC": -0.7}, &res); code != http.StatusOK {
		t.Fatalf("offset: code=%d %v", code, res)
	}
	if uri, tags := deliverRequest(t, d, stored, valve); uri != "d/dbg2/tlvs" || tags["0140"] != "ffba" {
		t.Errorf("dbg2 offset after restart = %s %v, want d/dbg2/tlvs 0140=ffba", uri, tags)
	}

	for _, body := range []map[string]any{
		{"offset_mode": "setpoint"},
		{"offset_mode": "cloud"},
		{"auto_offset": true},
		{"auto_offset_interval_sec": 3600},
	} {
		if code := postJSON(t, http.MethodPatch, url+"/api/install/zones/3", body, &res); code != http.StatusBadRequest {
			t.Errorf("%v: code=%d %v, want 400", body, code, res)
		}
	}
	if got := loadStored(t, a).Options.ZoneOffset(3).Mode; got != tado.OffsetModeDbg2 {
		t.Errorf("mode after the refused patches = %s, want dbg2", got)
	}
}

// TestShiftReleaseKeepsQueuedReturn: a shift is released while the return to
// schedule of the zone still waits in the queue. The release rewrites that
// return, not the overlay the valve still runs, whether the return is the
// expiry of the timer (no 6440) or a cancel (6440). The deadline ends with the
// delivered return.
func TestShiftReleaseKeepsQueuedReturn(t *testing.T) {
	for _, tc := range []struct {
		name  string
		queue func(t *testing.T, d *daemon, url string, clock *testClock)
	}{
		{"expiry", func(t *testing.T, d *daemon, _ string, clock *testClock) {
			clock.advance(15 * time.Minute)
			expireOverlays(t, d)
		}},
		{"cancel", func(t *testing.T, _ *daemon, url string, _ *testClock) {
			if _, code := postZone(t, url, 3, "auto", map[string]any{}); code != http.StatusOK {
				t.Fatalf("auto: code=%d", code)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := windowInstall(t, nil)
			clock := newTestClock(windowStart)
			d, url := newWindowDaemon(t, in, clock, nil)
			valve := wireOf(t, in, "VA1000000002")
			seedBlock(t, d, in, "VA1000000002", 3)
			if _, code := postZone(t, url, 3, "setpoint", map[string]any{
				"setpointC": 22, "mode": "timer", "durationSec": 900,
			}); code != http.StatusOK {
				t.Fatalf("timer: code=%d", code)
			}
			deliverAck(t, d, in, valve)
			tc.queue(t, d, url, clock)
			if _, err := d.do(func(e *tado.EmuState) (any, error) { return nil, d.releaseShift(e, 3, valve, 0.5) }); err != nil {
				t.Fatal(err)
			}
			var last map[string]string
			for pending(t, d, valve) > 0 {
				last = deliverAck(t, d, in, valve)
			}
			wantTags(t, "the last write", last, map[string]string{"6240": "00"}, "6260", "6280")
			v, err := d.do(func(e *tado.EmuState) (any, error) {
				_, timed := e.ZoneOverlayTimer(3)
				return timed, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if v.(bool) {
				t.Error("the delivered return left the deadline")
			}
		})
	}
}

// heatUpRunning reports whether zone runs a heat-up episode now.
func heatUpRunning(t *testing.T, d *daemon, zone int) bool {
	t.Helper()
	v, err := d.do(func(*tado.EmuState) (any, error) { return d.heating[zone] != nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	return v.(bool)
}

// heatRig starts a daemon whose zone 3 runs 17 C then 21 C, with the store
// backing the management routes.
func heatRig(t *testing.T) (*daemon, string, *testClock, *tado.Install, *installAPI) {
	t.Helper()
	a := newInstallAPIOn(t, windowInstall(t, func(in *tado.Install) {
		in.Options.EarlyStartZones = map[int]bool{3: true}
	}))
	stored := loadStored(t, a)
	clock := newTestClock(windowStart)
	d, url := newWindowDaemon(t, stored, clock, a.store)
	var view map[string]any
	if code := postJSON(t, http.MethodPut, url+"/api/zones/3/schedule", allDayProgram(
		map[string]any{"minute": 0, "setpoint_c": 17.0},
		map[string]any{"minute": 420, "setpoint_c": 21.0},
	), &view); code != http.StatusOK {
		t.Fatalf("schedule: code=%d %v", code, view)
	}
	seedBlock(t, d, stored, earlyValve, 3)
	return d, url, clock, stored, a
}

// TestManualOffsetRejectsHeatUp: a manual offset in mode config moves 4060 and
// rejects the running heat-up episode when the learner reads 4060.
func TestManualOffsetRejectsHeatUp(t *testing.T) {
	d, url, clock, in, _ := heatRig(t)
	at(t, d, in, clock, "06:50", 17.0, 0)
	at(t, d, in, clock, "07:00", math.NaN(), 0)
	at(t, d, in, clock, "07:30", 18.0, 80)
	if !heatUpRunning(t, d, 3) {
		t.Fatal("no heat-up episode to reject")
	}
	var res map[string]any
	if code := postJSON(t, http.MethodPost, url+"/api/zones/3/offset", map[string]any{"offsetC": 0.5}, &res); code != http.StatusOK {
		t.Fatalf("offset: code=%d %v", code, res)
	}
	if heatUpRunning(t, d, 3) {
		t.Error("the manual offset did not reject the heat-up episode")
	}
}

// TestManualOffsetKeepsHeatUp: with the coordinator as owner and a fresh
// reading, the learner reads the external temperature and the offset does not
// reject the running episode.
func TestManualOffsetKeepsHeatUp(t *testing.T) {
	d, url, clock, in, _ := heatRig(t)
	var res map[string]any
	if code := postJSON(t, http.MethodPatch, url+"/api/install/zones/3", map[string]any{"owner": tado.ZoneOwnerCoordinator}, &res); code != http.StatusOK {
		t.Fatalf("owner: code=%d %v", code, res)
	}
	at(t, d, in, clock, "06:50", 17.0, 0)
	at(t, d, in, clock, "07:00", math.NaN(), 0)
	at(t, d, in, clock, "07:30", 18.0, 80)
	if !heatUpRunning(t, d, 3) {
		t.Fatal("no heat-up episode to keep")
	}
	if code := postJSON(t, http.MethodPost, url+"/api/zones/3/temperature", map[string]any{"tempC": 20}, &res); code != http.StatusOK {
		t.Fatalf("temperature: code=%d %v", code, res)
	}
	if code := postJSON(t, http.MethodPost, url+"/api/zones/3/offset", map[string]any{"offsetC": 0.5}, &res); code != http.StatusOK {
		t.Fatalf("offset: code=%d %v", code, res)
	}
	if !heatUpRunning(t, d, 3) {
		t.Error("a fresh external reading must keep the heat-up episode")
	}
}

// legacyStateFile writes a state database that holds zone 3 in offset mode
// setpoint at -0.7 C with an applied shift of -1.2 C, as the version before the
// migration left it, and returns its path.
func legacyStateFile(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	legacy, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.EnsureIdentity(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.ReplaceInstall(ctx, windowInstall(t, nil)); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`INSERT INTO install (key, value) VALUES ('zone_offset.3', '{"mode":"setpoint","setpoint_centi":-70}')`,
		`INSERT INTO meta (key, value) VALUES ('room_shift', '{"3":-120}')`,
	} {
		if _, err := legacy.db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// shiftReleaseOf reads install.ShiftReleaseKey from store.
func shiftReleaseOf(t *testing.T, store *stateStore) map[int]int {
	t.Helper()
	released, err := store.ShiftRelease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return released
}

// startReleased starts a daemon on the state database at path, with an all-day
// program at 19.5 C and the valve block known. The store holds the release of
// zone 3 at -1.2 C.
func startReleased(t *testing.T, path string) (*daemon, string, *tado.Install, *stateStore, string) {
	t.Helper()
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if got := shiftReleaseOf(t, store); len(got) != 1 || got[3] != -120 {
		t.Fatalf("shift release = %v, want zone 3 at -120", got)
	}
	in, _, err := store.LoadInstall(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := in.ValidateCommissionable(true); err != nil {
		t.Fatal(err)
	}
	d, url := newWindowDaemon(t, in, newTestClock(windowStart), store)
	var view map[string]any
	if code := postJSON(t, http.MethodPut, url+"/api/zones/3/schedule",
		allDayProgram(map[string]any{"minute": 0, "setpoint_c": 19.5}), &view); code != http.StatusOK {
		t.Fatalf("schedule: code=%d %v", code, view)
	}
	valve := wireOf(t, in, "VA1000000002")
	seedBlock(t, d, in, "VA1000000002", 3)
	dropQueue(t, d, valve)
	return d, url, in, store, valve
}

// TestReleaseShifts: a start that transmits writes a zone the store moved off
// offset mode setpoint again, without the shift its valve still holds. The
// program goes out as it is, and the 23.2 C a running overlay of 22 C carried
// on the wire moves back to 22 C. The owed write is stored and the release key
// is gone before the engine runs. A start that dies before the release, or one
// that does not transmit, leaves the key, and the next start releases.
func TestReleaseShifts(t *testing.T) {
	release := func(t *testing.T, d *daemon, transmit bool) error {
		t.Helper()
		_, err := d.do(func(e *tado.EmuState) (any, error) { return nil, d.releaseShifts(e, transmit) })
		return err
	}
	released := func(t *testing.T, d *daemon, store *stateStore, valve string) {
		t.Helper()
		if err := release(t, d, true); err != nil {
			t.Fatal(err)
		}
		owed, err := store.ProgramOwed(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(owed, programWrite{zone: 3, target: valve}) {
			t.Errorf("stored owed writes = %v, want zone 3 on %s", owed, valve)
		}
		if got := shiftReleaseOf(t, store); got != nil {
			t.Errorf("shift release after the release = %v, want none", got)
		}
	}
	wantProgram := func(t *testing.T, d *daemon, in *tado.Install, valve string) {
		t.Helper()
		if uri, tags := deliverRequest(t, d, in, valve); uri != "z/s?id=3" || tags["6200"] != "079e" {
			t.Errorf("release = %s %v, want z/s with the program 6200=079e", uri, tags)
		}
	}
	t.Run("program", func(t *testing.T) {
		d, _, in, store, valve := startReleased(t, legacyStateFile(t))
		released(t, d, store, valve)
		wantProgram(t, d, in, valve)
	})
	t.Run("overlay", func(t *testing.T) {
		d, url, in, store, valve := startReleased(t, legacyStateFile(t))
		if _, code := postZone(t, url, 3, "setpoint", map[string]any{"setpointC": 23.2}); code != http.StatusOK {
			t.Fatalf("overlay: code=%d", code)
		}
		deliverAck(t, d, in, valve)
		released(t, d, store, valve)
		wantTags(t, "the release", deliverAck(t, d, in, valve), map[string]string{"6280": "0898", "6200": "079e"})
	})
	t.Run("a start that dies before the release", func(t *testing.T) {
		path := legacyStateFile(t)
		dead, err := openStateStore(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := dead.Close(); err != nil {
			t.Fatal(err)
		}
		d, _, in, store, valve := startReleased(t, path)
		released(t, d, store, valve)
		wantProgram(t, d, in, valve)
	})
	t.Run("a start that does not transmit", func(t *testing.T) {
		path := legacyStateFile(t)
		d, _, _, store, valve := startReleased(t, path)
		if err := release(t, d, false); err != nil {
			t.Fatal(err)
		}
		pending, err := d.do(func(e *tado.EmuState) (any, error) { return e.Pending(valve), nil })
		if err != nil {
			t.Fatal(err)
		}
		if pending != 0 {
			t.Errorf("%v writes queued without transmit, want none", pending)
		}
		if got := shiftReleaseOf(t, store); len(got) != 1 || got[3] != -120 {
			t.Errorf("shift release without transmit = %v, want zone 3 at -120", got)
		}
	})
}

// TestImportReleasesNoShift: an import migrates the staged file and drops its
// release, because the valves hold what the running daemon wrote.
func TestImportReleasesNoShift(t *testing.T) {
	path := legacyStateFile(t)
	if _, err := checkImport(path); err != nil {
		t.Fatal(err)
	}
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if got := shiftReleaseOf(t, store); got != nil {
		t.Errorf("shift release of the imported file = %v, want none", got)
	}
	in, _, err := store.LoadInstall(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := in.Options.ZoneOffset(3).Mode; got != tado.OffsetModeConfig {
		t.Errorf("imported zone 3 offset mode = %s, want config", got)
	}
}
