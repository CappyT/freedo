package daemon

// zone_program_test: target.hasProgram tells a zone that follows a weekly
// program of the daemon from one that has none, and a zone with no program
// receives no block change write.

import (
	"encoding/hex"
	"fmt"
	"net/http"
	"testing"
	"time"

	"freedo/tado"
)

// wantProgramView checks the program fields of a zone target. A nil programC
// means the view carries none.
func wantProgramView(t *testing.T, what string, target map[string]any, hasProgram bool, programC, targetC any) {
	t.Helper()
	if target["hasProgram"] != hasProgram || target["programC"] != programC || target["targetC"] != targetC ||
		target["source"] != "program" || target["known"] != true || target["on"] != true {
		t.Errorf("%s: target = %v, want hasProgram=%t programC=%v targetC=%v on the program", what, target, hasProgram, programC, targetC)
	}
}

// deliverZones sends every write queued to target in turn, feeds back the 2.04
// of the device and returns the TLVs of each write keyed by its zone kind, 6020.
func deliverZones(t *testing.T, d *daemon, in *tado.Install, target string) map[string]map[string]string {
	t.Helper()
	out := map[string]map[string]string{}
	for n := pending(t, d, target); n > 0; n = pending(t, d, target) {
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
		out[tags["6020"]] = tags
		feed(t, d, in, target, false, tado.CoAPReq{Type: "ACK", Code: "2.04", MID: m.MID, Token: m.Token})
		if pending(t, d, target) >= n {
			t.Fatalf("%s still holds %d queued writes after the ACK", target, n)
		}
	}
	return out
}

// TestZoneTargetHasProgram: hasProgram is true while the daemon holds a weekly
// program for the zone, hot water included. Without one the source stays
// "program", and programC is the 6200 of the zone cache, null when it holds none.
func TestZoneTargetHasProgram(t *testing.T) {
	in := windowInstall(t, nil)
	d, url := newWindowDaemon(t, in, newTestClock(windowStart), nil)
	// The block a device reports on a zone that has no 6200: it follows its
	// program, and the daemon holds none.
	bare := []tado.TLV{{Tag: "6240", Val: []byte{0x00}}, {Tag: "62e0", Val: []byte{0x00}}}
	seedBlock(t, d, in, "RU1000000001", 0, bare...)
	seedBlock(t, d, in, "VA1000000002", 3, bare...)
	// The default block carries 6200 = 20 C.
	seedBlock(t, d, in, "VA1000000001", 5)

	wantProgramView(t, "hot water without a program", mustTarget(t, url, 0), false, nil, nil)
	wantProgramView(t, "a room without a program", mustTarget(t, url, 3), false, nil, nil)
	wantProgramView(t, "a room with a 6200 in the zone cache", mustTarget(t, url, 5), false, 20.0, 20.0)

	var view map[string]any
	if code := postJSON(t, http.MethodPut, url+"/api/zones/3/schedule",
		allDayProgram(map[string]any{"minute": 0, "setpoint_c": 19.0}), &view); code != http.StatusOK {
		t.Fatalf("room schedule: code=%d %v", code, view)
	}
	putHotWaterWeek(t, url)
	tickHome(t, d)

	wantProgramView(t, "a room with a program", mustTarget(t, url, 3), true, 19.0, 19.0)
	wantProgramView(t, "hot water with a program", mustTarget(t, url, 0), true, 45.0, 45.0)
	wantProgramView(t, "a room that still has no program", mustTarget(t, url, 5), false, 20.0, 20.0)
}

// TestZoneWithoutProgramWrites: a zone with no program and no 6200 gets no write
// at a block change or at the start. Only the switch between home and away
// writes it, and neither write carries 6200, so the device keeps its own.
func TestZoneWithoutProgramWrites(t *testing.T) {
	in := windowInstall(t, nil)
	clock := newTestClock(windowStart)
	d, url := newWindowDaemon(t, in, clock, nil)
	thermostat, valve := wireOf(t, in, "RU1000000001"), wireOf(t, in, "VA1000000002")
	bare := []tado.TLV{{Tag: "6240", Val: []byte{0x00}}, {Tag: "62e0", Val: []byte{0x00}}}
	seedBlock(t, d, in, "RU1000000001", 0, bare...)
	seedBlock(t, d, in, "VA1000000002", 3, bare...)

	clock.advance(24 * time.Hour)
	tickHome(t, d)
	wantNoWrites(t, "a day without a program", d, thermostat, valve)

	var home map[string]any
	if code := postJSON(t, http.MethodPatch, url+"/api/home", map[string]any{"away": true}, &home); code != http.StatusOK {
		t.Fatalf("away: code=%d %v", code, home)
	}
	// The thermostat takes the hot water write and the write of the hall zone.
	wantTags(t, "hot water away", deliverZones(t, d, in, thermostat)["02"],
		map[string]string{"6020": "02", "6160": "02", "61e0": "00", "6240": "00"}, "6200", "6260", "6280", "6440")
	wantTags(t, "room away", deliverZones(t, d, in, valve)["01"],
		map[string]string{"6020": "01", "6160": "02", "61e0": "00", "6240": "00"}, "6200", "6260", "6280")

	if code := postJSON(t, http.MethodPatch, url+"/api/home", map[string]any{"away": false}, &home); code != http.StatusOK {
		t.Fatalf("home: code=%d %v", code, home)
	}
	wantTags(t, "hot water home", deliverZones(t, d, in, thermostat)["02"],
		map[string]string{"6020": "02", "6160": "01", "61e0": "01", "6240": "00"}, "6200", "6260", "6280", "6440")
	wantTags(t, "room home", deliverZones(t, d, in, valve)["01"],
		map[string]string{"6020": "01", "6160": "01", "61e0": "01", "6240": "00"}, "6200", "6260", "6280")
}
