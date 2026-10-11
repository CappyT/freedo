package daemon

// zone_echo_test: the z/s?lid=N reports of a zone member after a coordinator
// write. Frames and clocks are synthetic.

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"freedo/tado"
)

const camera = "VA1000000005" // the only member and writer of zone 10

// cameraBlock is a z/s block of zone 10: a manual overlay at centiC with the
// valve's own 6200 of 24.10 C.
func cameraBlock(centiC uint16) []tado.TLV {
	return []tado.TLV{
		{Tag: "6280", Val: []byte{byte(centiC >> 8), byte(centiC)}}, {Tag: "6240", Val: []byte{0x02}},
		{Tag: "6260", Val: []byte{0x01}}, {Tag: "6200", Val: []byte{0x09, 0x6a}},
		{Tag: "61e0", Val: []byte{0x01}}, {Tag: "62e0", Val: []byte{0x00}},
	}
}

// echoZone sends the z/s?lid=N report of ref.
func echoZone(t *testing.T, d *daemon, in *tado.Install, ref string, zone int, mid uint16, tlvs []tado.TLV) {
	t.Helper()
	var body []byte
	for _, tlv := range tlvs {
		body = append(body, tlv.Encode()...)
	}
	feed(t, d, in, ref, true, tado.CoAPReq{
		Type: "CON", Code: "0.03", MID: mid, Path: []string{"z", "s"},
		Query: []string{"lid=" + strconv.Itoa(zone)}, Payload: body,
	})
}

func wantZoneTarget(t *testing.T, what, url string, zone int, setpointC float64) {
	t.Helper()
	if target := mustTarget(t, url, zone); target["source"] != "overlay" || target["targetC"] != setpointC {
		t.Errorf("%s: target = %v, want an overlay at %v", what, target, setpointC)
	}
}

// TestStaleZoneEchoKeepsNewerSetpoint: the valve echoes the block of a write
// the newer setpoint already replaced. The zone keeps the newer setpoint and a
// later rewrite restates it. A wheel change reported after it still wins.
func TestStaleZoneEchoKeepsNewerSetpoint(t *testing.T) {
	in := windowInstall(t, nil)
	d, url := newWindowDaemon(t, in, newTestClock(windowStart), nil)
	writer := wireOf(t, in, camera)
	seedBlock(t, d, in, camera, 10, cameraBlock(2460)...)
	wantZoneTarget(t, "seeded", url, 10, 24.6)

	if _, code := postZone(t, url, 10, "setpoint", map[string]any{"setpointC": 25}); code != http.StatusOK {
		t.Fatalf("setpoint: code=%d", code)
	}
	wantTags(t, "the setpoint", deliver(t, d, in, writer), map[string]string{"6280": "09c4", "6240": "02"})

	echoZone(t, d, in, camera, 10, 0x9812, cameraBlock(2460))
	wantZoneTarget(t, "after the stale echo", url, 10, 25)
	if _, err := d.do(func(e *tado.EmuState) (any, error) { return nil, e.EnqueueZoneRewrite(writer, 10) }); err != nil {
		t.Fatal(err)
	}
	wantTags(t, "the rewrite after the stale echo", deliver(t, d, in, writer), map[string]string{"6280": "09c4", "6240": "02"})

	echoZone(t, d, in, camera, 10, 0x9813, cameraBlock(2500))
	wantZoneTarget(t, "after the current echo", url, 10, 25)

	overlayFromDevice(t, d, in, camera, 10, 2460, 0x9814)
	echoZone(t, d, in, camera, 10, 0x9815, cameraBlock(2460))
	wantZoneTarget(t, "after the wheel", url, 10, 24.6)
}

// TestStaleZoneEchoExpires: past the echo window, a report equal to the
// replaced block is a change made on the device and the zone takes it.
func TestStaleZoneEchoExpires(t *testing.T) {
	in := windowInstall(t, nil)
	clock := newTestClock(windowStart)
	d, url := newWindowDaemon(t, in, clock, nil)
	writer := wireOf(t, in, camera)
	seedBlock(t, d, in, camera, 10, cameraBlock(2460)...)
	if _, code := postZone(t, url, 10, "setpoint", map[string]any{"setpointC": 25}); code != http.StatusOK {
		t.Fatalf("setpoint: code=%d", code)
	}
	deliver(t, d, in, writer)

	clock.advance(2 * time.Minute)
	echoZone(t, d, in, camera, 10, 0x9812, cameraBlock(2460))
	wantZoneTarget(t, "after the window", url, 10, 24.6)
}

// TestZoneEchoWithoutProgramQueuesNoRewrite: a zone the daemon holds no
// program for takes no rewrite when its member echoes the 6200 a delivered
// write dropped from the zone cache: that is no block change.
func TestZoneEchoWithoutProgramQueuesNoRewrite(t *testing.T) {
	in := windowInstall(t, nil)
	d, url := newWindowDaemon(t, in, newTestClock(windowStart), nil)
	writer := wireOf(t, in, camera)
	seedBlock(t, d, in, camera, 10, cameraBlock(2460)...)
	if _, code := postZone(t, url, 10, "setpoint", map[string]any{"setpointC": 25}); code != http.StatusOK {
		t.Fatalf("setpoint: code=%d", code)
	}
	deliver(t, d, in, writer)
	tickHome(t, d)

	for i := range 3 {
		echoZone(t, d, in, camera, 10, 0x9820+uint16(i), cameraBlock(2500))
		tickHome(t, d)
		wantNoWrites(t, "the echo", d, writer)
	}
}

// TestRewriteBehindSetpointRestatesIt: a rewrite queued behind a setpoint the
// valve has not taken yet restates that setpoint, not the older block. The
// setpoint changes a running overlay and carries no 6440, so the rewrite
// replaces it in the queue.
func TestRewriteBehindSetpointRestatesIt(t *testing.T) {
	in := windowInstall(t, nil)
	d, url := newWindowDaemon(t, in, newTestClock(windowStart), nil)
	writer := wireOf(t, in, camera)
	seedBlock(t, d, in, camera, 10, cameraBlock(2460)...)
	if _, code := postZone(t, url, 10, "setpoint", map[string]any{"setpointC": 25}); code != http.StatusOK {
		t.Fatalf("setpoint: code=%d", code)
	}
	if _, err := d.do(func(e *tado.EmuState) (any, error) { return nil, e.EnqueueZoneRewrite(writer, 10) }); err != nil {
		t.Fatal(err)
	}
	wantTags(t, "the rewrite", deliverAck(t, d, in, writer), map[string]string{"6280": "09c4", "6240": "02"}, "6440")
	wantNoWrites(t, "the replaced setpoint", d, writer)
	wantZoneTarget(t, "after the rewrite", url, 10, 25)
}

// TestRewriteBehindReturnFromOffReachesZone: the room runs the off overlay
// and its return to schedule is still queued. The away switch restates that
// return to schedule, not the cached off overlay, so the zone takes the away
// block instead of being skipped. The switch carries the 6440 of the return, so
// it replaces it in the queue.
func TestRewriteBehindReturnFromOffReachesZone(t *testing.T) {
	in := windowInstall(t, nil)
	d, url := newWindowDaemon(t, in, newTestClock(windowStart), nil)
	writer := wireOf(t, in, camera)
	seedBlock(t, d, in, camera, 10, cameraBlock(2460)...)
	if _, code := postZone(t, url, 10, "off", map[string]any{"mode": "manual"}); code != http.StatusOK {
		t.Fatalf("off: code=%d", code)
	}
	wantTags(t, "the off overlay", deliverAck(t, d, in, writer), map[string]string{"6240": "02", "6260": "00"}, "6280")
	if _, code := postZone(t, url, 10, "auto", map[string]any{}); code != http.StatusOK {
		t.Fatalf("auto: code=%d", code)
	}
	var res struct {
		Away   bool `json:"away"`
		Queued int  `json:"queued"`
	}
	if code := postJSON(t, http.MethodPatch, url+"/api/home", map[string]any{"away": true}, &res); code != http.StatusOK || !res.Away {
		t.Fatalf("away: code=%d %+v", code, res)
	}
	if n := pending(t, d, writer); n != 1 {
		t.Fatalf("%s holds %d queued writes, want the away rewrite alone", writer, n)
	}
	wantTags(t, "the away rewrite", deliverAck(t, d, in, writer),
		map[string]string{"6160": "02", "61e0": "00", "6240": "00", "6440": "0001"}, "6260", "6280")
}

// TestSetpointBehindReturnStartsOverlay: a setpoint queued behind a return to
// schedule reaches a valve that runs no overlay any more, so it marks the start
// of one with 6440. Both writes then mark an event, and the newer one replaces
// the return in the queue.
func TestSetpointBehindReturnStartsOverlay(t *testing.T) {
	in := windowInstall(t, nil)
	d, url := newWindowDaemon(t, in, newTestClock(windowStart), nil)
	writer := wireOf(t, in, camera)
	seedBlock(t, d, in, camera, 10, cameraBlock(2460)...)
	if _, code := postZone(t, url, 10, "auto", map[string]any{}); code != http.StatusOK {
		t.Fatalf("auto: code=%d", code)
	}
	if _, code := postZone(t, url, 10, "setpoint", map[string]any{"setpointC": 22}); code != http.StatusOK {
		t.Fatalf("setpoint: code=%d", code)
	}
	var last map[string]string
	for pending(t, d, writer) > 0 {
		last = deliverAck(t, d, in, writer)
	}
	wantTags(t, "the setpoint", last, map[string]string{"6240": "02", "6280": "0898", "6440": "0001"})
}
