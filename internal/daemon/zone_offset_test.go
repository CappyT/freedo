package daemon

// zone_offset_test: the three correction modes of POST /api/zones/{n}/offset.
// Frames and installation are synthetic.

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/http"
	"testing"

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
// record 0140 under the ETag already served, setpoint no device write at all
// but a zone rewrite and a zone setpoint shifted by +0.7 C.
func TestZoneOffsetModes(t *testing.T) {
	for _, mode := range []string{tado.OffsetModeConfig, tado.OffsetModeDbg2, tado.OffsetModeSetpoint} {
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
			if mode == tado.OffsetModeSetpoint {
				var view map[string]any
				if code := postJSON(t, http.MethodPut, a.base+"/api/zones/3/schedule",
					allDayProgram(map[string]any{"minute": 0, "setpoint_c": 19.5}), &view); code != http.StatusOK {
					t.Fatalf("schedule: code=%d %v", code, view)
				}
				seedBlock(t, d, in, "VA1000000002", 3)
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
			case tado.OffsetModeSetpoint:
				if res["zone"].(map[string]any)["setpoint_offset_c"] != -0.7 {
					t.Errorf("offset answer = %v, want setpoint_offset_c -0.7", res)
				}
				if uri != "z/s?id=3" || tags["6200"] != "07e4" {
					t.Errorf("setpoint restate = %s %v, want z/s with 6200=07e4 (19.5 + 0.7)", uri, tags)
				}
				if string(after.ETag) != string(before.ETag) || served0140(after) != served0140(before) {
					t.Errorf("served d/config moved: %x -> %x", before.Body, after.Body)
				}
				if _, code := postZone(t, a.base, 3, "setpoint", map[string]any{"setpointC": 21.0}); code != http.StatusOK {
					t.Fatalf("zone setpoint: code=%d", code)
				}
				wantTags(t, "shifted setpoint", deliverAck(t, d, in, valve),
					map[string]string{"6240": "02", "6280": "087a", "6200": "07e4"})
				target := mustTarget(t, a.base, 3)
				if target["targetC"] != 21.0 || target["programC"] != 19.5 {
					t.Errorf("target = %v, want 21 on the corrected scale and program 19.5", target)
				}
			}
			if n := pending(t, d, valve); n != 0 {
				t.Errorf("%d more writes queued, want none", n)
			}
		})
	}
}

// deliverAck is deliverRequest with the TLVs alone.
func deliverAck(t *testing.T, d *daemon, in *tado.Install, target string) map[string]string {
	t.Helper()
	_, tags := deliverRequest(t, d, in, target)
	return tags
}

// TestZoneOffsetModePersists: the mode and the coordinator's own correction
// live in the installation rows, so a daemon started on them keeps both, and
// leaving mode setpoint drops the correction and restates the zone unshifted.
func TestZoneOffsetModePersists(t *testing.T) {
	a := newInstallAPIOn(t, windowInstall(t, nil))
	a.mutate(http.MethodPatch, "/api/install/zones/3", map[string]any{"offset_mode": "setpoint"})
	if code, res := a.call(http.MethodPost, "/api/zones/3/offset", map[string]any{"offsetC": 1.5}); code != http.StatusOK {
		t.Fatalf("offset: code=%d %v", code, res)
	}
	stored, _, err := a.store.LoadInstall(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := stored.ValidateCommissionable(true); err != nil {
		t.Fatal(err)
	}
	if got := stored.Options.ZoneOffset(3); got != (tado.ZoneOffset{Mode: tado.OffsetModeSetpoint, SetpointCenti: 150}) {
		t.Errorf("stored zone 3 offset = %+v, want setpoint 150", got)
	}

	d, url := newWindowDaemon(t, stored, newTestClock(windowStart), a.store)
	var zone map[string]any
	if code := getJSON(t, url+"/api/install/zones/3", &zone); code != http.StatusOK {
		t.Fatalf("zone after restart: code=%d", code)
	}
	if z := zone["zone"].(map[string]any); z["offset_mode"] != "setpoint" || z["setpoint_offset_c"] != 1.5 {
		t.Errorf("zone after restart = %v, want setpoint 1.5", z)
	}

	valve := wireOf(t, stored, "VA1000000002")
	seedBlock(t, d, stored, "VA1000000002", 3)
	var res map[string]any
	if code := postJSON(t, http.MethodPatch, url+"/api/install/zones/3", map[string]any{"offset_mode": "dbg2"}, &res); code != http.StatusOK {
		t.Fatalf("dbg2: code=%d %v", code, res)
	}
	if z := res["zone"].(map[string]any); z["offset_mode"] != "dbg2" || z["setpoint_offset_c"] != nil {
		t.Errorf("zone in dbg2 = %v, want no setpoint offset", z)
	}
	if uri, tags := deliverRequest(t, d, stored, valve); uri != "z/s?id=3" || tags["6200"] != "07d0" {
		t.Errorf("leaving setpoint writes %s %v, want the z/s restate with the wire 6200=07d0", uri, tags)
	}
	if code := postJSON(t, http.MethodPost, url+"/api/zones/3/offset", map[string]any{"offsetC": -0.7}, &res); code != http.StatusOK {
		t.Fatalf("offset: code=%d %v", code, res)
	}
	if uri, tags := deliverRequest(t, d, stored, valve); uri != "d/dbg2/tlvs" || tags["0140"] != "ffba" {
		t.Errorf("dbg2 offset after restart = %s %v, want d/dbg2/tlvs 0140=ffba", uri, tags)
	}

	if code := postJSON(t, http.MethodPatch, url+"/api/install/zones/3", map[string]any{"offset_mode": "cloud"}, &res); code != http.StatusBadRequest {
		t.Errorf("bad mode: code=%d %v, want 400", code, res)
	}
}

// TestSetpointShiftScale: a target on the corrected scale goes out as target -
// shift inside the room range, and a reported temperature comes back + shift.
func TestSetpointShiftScale(t *testing.T) {
	in := windowInstall(t, func(in *tado.Install) {
		in.Options.ZoneOffsets = map[int]tado.ZoneOffset{3: {Mode: tado.OffsetModeSetpoint, SetpointCenti: -70}}
	})
	for _, tc := range []struct {
		zone     int
		c, raw   float64
		reported float64
	}{
		{3, 21, 21.7, 20.3},
		{3, 24.5, 25, 23.8},
		{5, 21, 21, 21},
	} {
		if got := rawSetpointC(in, tc.zone, tc.c); got != tc.raw {
			t.Errorf("zone %d: raw of %v = %v, want %v", tc.zone, tc.c, got, tc.raw)
		}
		if got := correctedC(in, tc.zone, tc.c); got != tc.reported {
			t.Errorf("zone %d: corrected of %v = %v, want %v", tc.zone, tc.c, got, tc.reported)
		}
	}
	views := correctedZoneViews(in, []tado.ZoneView{{Number: 3, TempC: 22}, {Number: 5, TempC: 22}})
	if views[0].TempC != 21.3 || views[1].TempC != 22 {
		t.Errorf("zone views = %+v, want 21.3 on zone 3 and 22 on zone 5", views)
	}
}
