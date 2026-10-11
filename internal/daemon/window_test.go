package daemon

// window_test: the zone open window engine. Frames and clocks are synthetic;
// the addresses are the ones of the test installation.

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"freedo/internal/radio"
	"freedo/tado"
)

var windowStart = time.Date(2026, 1, 5, 5, 0, 0, 0, time.UTC)

// windowInstall is the test installation with edit applied to a fresh copy.
func windowInstall(t *testing.T, edit func(*tado.Install)) *tado.Install {
	t.Helper()
	in, err := tado.Parse([]byte(fixtureInstallYAML))
	if err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(in)
	}
	return in
}

// newWindowDaemon starts a dry-run daemon on in with an injected clock and an
// optional store, and returns it with its API base URL.
func newWindowDaemon(t *testing.T, in *tado.Install, clock *testClock, store *stateStore) (*daemon, string) {
	t.Helper()
	cfg := in.EmuConfig()
	cfg.Now = clock.now
	emu := tado.NewEmu(cfg, tado.NewUnhandledLogger(io.Discard))
	if err := restoreDesiredState(store, emu); err != nil {
		t.Fatal(err)
	}
	d := newDaemon(in, emu, radio.NewMock(nil), true, "mock")
	d.retryNow = clock.now
	d.state = store
	if err := d.restoreWindows(emu); err != nil {
		t.Fatal(err)
	}
	d.applyHomeState(emu)
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)
	srv := httptest.NewServer(d.mux())
	t.Cleanup(func() {
		srv.Close()
		cancel()
	})
	return d, srv.URL
}

func wireOf(t *testing.T, in *tado.Install, ref string) string {
	t.Helper()
	dev, err := in.Resolve(ref)
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(dev.WireEUI())
}

// feed runs one uplink from ref through handleFrame, the funnel of the radio.
func feed(t *testing.T, d *daemon, in *tado.Install, ref string, contexted bool, r tado.CoAPReq) {
	t.Helper()
	frame := uplinkFrame(t, in, ref, contexted, r)
	if _, err := d.do(func(*tado.EmuState) (any, error) { d.handleFrame(frame, true); return nil, nil }); err != nil {
		t.Fatal(err)
	}
}

// uplinkFrame is the radio frame of one uplink from ref.
func uplinkFrame(t *testing.T, in *tado.Install, ref string, contexted bool, r tado.CoAPReq) []byte {
	t.Helper()
	dev, err := in.Resolve(ref)
	if err != nil {
		t.Fatal(err)
	}
	r.SrcEUI, r.DstEUI = dev.WireEUI(), in.CoordinatorEUI()
	var pkt []byte
	if contexted {
		pkt, err = tado.BuildContextCoAP(r)
	} else {
		r.SrcPort, r.DstPort = 5683, 4005
		pkt, err = tado.BuildCoAP(r)
	}
	if err != nil {
		t.Fatal(err)
	}
	frame, err := tado.BuildData(in.NetworkKey(), 0x31, in.CoordinatorEUI(), dev.WireEUI()[:5],
		tado.WrapAppPayload(pkt, 900, nil))
	if err != nil {
		t.Fatal(err)
	}
	return frame.Bytes()
}

// seedBlock makes ref report the zone block it holds, which is what lets the
// engine rebuild it, then clears whatever that report and the first schedule
// evaluation of the zone queued.
func seedBlock(t *testing.T, d *daemon, in *tado.Install, ref string, zone int, tlvs ...tado.TLV) {
	t.Helper()
	if len(tlvs) == 0 {
		tlvs = []tado.TLV{
			{Tag: "6280", Val: []byte{0x09, 0xc4}}, {Tag: "6240", Val: []byte{0x00}},
			{Tag: "6260", Val: []byte{0x01}}, {Tag: "6200", Val: []byte{0x07, 0xd0}},
			{Tag: "62e0", Val: []byte{0x00}},
		}
	}
	var body []byte
	for _, tlv := range tlvs {
		body = append(body, tlv.Encode()...)
	}
	feed(t, d, in, ref, true, tado.CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x5100 + uint16(zone), Path: []string{"z", "s"},
		Query: []string{"lid=" + strconv.Itoa(zone)}, Payload: body,
	})
	if _, err := d.do(func(e *tado.EmuState) (any, error) { d.tickHomeState(e); return nil, nil }); err != nil {
		t.Fatal(err)
	}
	members, err := in.ZoneMembers(zone)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range members {
		dropQueue(t, d, hex.EncodeToString(m.WireEUI()))
	}
}

// report sends the open window report of ref for zone.
func report(t *testing.T, d *daemon, in *tado.Install, ref string, zone int, mid uint16) {
	t.Helper()
	feed(t, d, in, ref, false, tado.CoAPReq{
		Type: "CON", Code: "0.03", MID: mid,
		Path:    []string{"h", "7654321", "z", strconv.Itoa(zone), "ow"},
		Options: []tado.CoAPOption{{Num: 12, Val: []byte{42}}},
	})
}

func pending(t *testing.T, d *daemon, target string) int {
	t.Helper()
	v, err := d.do(func(e *tado.EmuState) (any, error) { return e.Pending(target), nil })
	if err != nil {
		t.Fatal(err)
	}
	return v.(int)
}

func tick(t *testing.T, d *daemon) {
	t.Helper()
	if _, err := d.do(func(e *tado.EmuState) (any, error) { d.tickWindows(e); return nil, nil }); err != nil {
		t.Fatal(err)
	}
}

type windowState struct {
	Open     bool    `json:"open"`
	Source   *string `json:"source"`
	Since    *string `json:"since"`
	Deadline *string `json:"deadline"`
}

func (w windowState) source() string {
	if w.Source == nil {
		return ""
	}
	return *w.Source
}

func (w windowState) deadline() string {
	if w.Deadline == nil {
		return ""
	}
	return *w.Deadline
}

// zoneWindowOf reads the window of zone through GET /api/zones and checks that
// /api/health agrees.
func zoneWindowOf(t *testing.T, url string, zone int) windowState {
	t.Helper()
	var zones struct {
		Declared []struct {
			Number int          `json:"number"`
			Window *windowState `json:"window"`
		} `json:"declared"`
	}
	if code := getJSON(t, url+"/api/zones", &zones); code != http.StatusOK {
		t.Fatalf("zones: code=%d", code)
	}
	var got *windowState
	for _, z := range zones.Declared {
		if z.Number == zone {
			got = z.Window
		}
	}
	if got == nil {
		t.Fatalf("zone %d carries no window", zone)
	}
	var health struct {
		Windows map[string]windowState `json:"windows"`
	}
	if code := getJSON(t, url+"/api/health", &health); code != http.StatusOK {
		t.Fatalf("health: code=%d", code)
	}
	if h := health.Windows[strconv.Itoa(zone)]; h.Open != got.Open || h.source() != got.source() || h.deadline() != got.deadline() {
		t.Errorf("health window %+v disagrees with the zone window %+v", h, *got)
	}
	return *got
}

func postWindow(t *testing.T, url string, zone int, open bool) (windowState, int) {
	t.Helper()
	var w windowState
	code := postJSON(t, http.MethodPost, url+"/api/zones/"+strconv.Itoa(zone)+"/window", map[string]any{"open": open}, &w)
	return w, code
}

func wantWindow(t *testing.T, what string, got windowState, open bool, source string, deadline time.Time) {
	t.Helper()
	wantDeadline := ""
	if open {
		wantDeadline = deadline.UTC().Format(time.RFC3339)
	}
	if got.Open != open || got.source() != source || got.deadline() != wantDeadline {
		t.Errorf("%s: window open=%t source=%q deadline=%q, want open=%t source=%q deadline=%q",
			what, got.Open, got.source(), got.deadline(), open, source, wantDeadline)
	}
}

// deliver sends the head of the queue of target and feeds back the 2.04 ACK
// of the device, which confirms the write. It returns the TLVs of the write.
func deliver(t *testing.T, d *daemon, in *tado.Install, target string) map[string]string {
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
	if n := pending(t, d, target); n != 0 {
		t.Fatalf("%s still holds %d queued writes after the ACK", target, n)
	}
	return tags
}

// wantWindowWrites checks that every target holds exactly one queued write
// carrying 62e0 = value and no 6440, and delivers it.
func wantWindowWrites(t *testing.T, what string, d *daemon, in *tado.Install, value string, targets ...string) {
	t.Helper()
	for _, target := range targets {
		if n := pending(t, d, target); n != 1 {
			t.Fatalf("%s: %s holds %d queued writes, want 1", what, target, n)
		}
		wantTags(t, what+" on "+target, deliver(t, d, in, target), map[string]string{"62e0": value}, "6440")
	}
}

func wantNoWrites(t *testing.T, what string, d *daemon, targets ...string) {
	t.Helper()
	for _, target := range targets {
		if n := pending(t, d, target); n != 0 {
			t.Errorf("%s: %s holds %d queued writes, want none", what, target, n)
		}
	}
}

// TestWindowDeviceReportOpensZone: a member report opens the window of the
// whole zone, the zone writer takes 62e0 = 1 and the other member takes none,
// a repeated report restarts nothing, and the zone closes after its 62c0.
func TestWindowDeviceReportOpensZone(t *testing.T) {
	in := windowInstall(t, nil)
	clock := newTestClock(windowStart)
	d, url := newWindowDaemon(t, in, clock, nil)
	a, b := wireOf(t, in, "VA1000000006"), wireOf(t, in, "VA1000000001")
	seedBlock(t, d, in, "VA1000000006", 5)
	seedBlock(t, d, in, "VA1000000001", 5)

	report(t, d, in, "VA1000000006", 5, 0x6001)
	deadline := windowStart.Add(900 * time.Second)
	wantWindowWrites(t, "the device open", d, in, "01", a)
	wantNoWrites(t, "the device open on the relayed member", d, b)
	wantWindow(t, "after the report", zoneWindowOf(t, url, 5), true, "device", deadline)

	clock.advance(100 * time.Second)
	report(t, d, in, "VA1000000001", 5, 0x6002)
	wantNoWrites(t, "a repeated report", d, a, b)
	wantWindow(t, "after a repeated report", zoneWindowOf(t, url, 5), true, "device", deadline)

	clock.advance(799 * time.Second)
	tick(t, d)
	wantNoWrites(t, "one second before 62c0", d, a, b)

	clock.advance(time.Second)
	tick(t, d)
	wantWindowWrites(t, "the device close", d, in, "00", a)
	wantNoWrites(t, "the device close on the relayed member", d, b)
	wantWindow(t, "after 62c0", zoneWindowOf(t, url, 5), false, "", time.Time{})
}

// closeReport sends the DELETE of the open window resource of ref for zone:
// the 62e0 of the zone dropped to 0 on the device from a source other than the
// coordinator.
func closeReport(t *testing.T, d *daemon, in *tado.Install, ref string, zone int, mid uint16) {
	t.Helper()
	feed(t, d, in, ref, false, tado.CoAPReq{
		Type: "CON", Code: "0.04", MID: mid,
		Path: []string{"h", "7654321", "z", strconv.Itoa(zone), "ow"},
	})
}

// TestWindowDeviceCloseEndsHold: a member's DELETE of z/N/ow drops the device
// hold before its 62c0 runs out, and the zone writer owes 62e0 = 0 at once. An
// external hold keeps the zone open.
func TestWindowDeviceCloseEndsHold(t *testing.T) {
	in := windowInstall(t, nil)
	clock := newTestClock(windowStart)
	d, url := newWindowDaemon(t, in, clock, nil)
	a, b := wireOf(t, in, "VA1000000006"), wireOf(t, in, "VA1000000001")
	seedBlock(t, d, in, "VA1000000006", 5)
	seedBlock(t, d, in, "VA1000000001", 5)

	report(t, d, in, "VA1000000006", 5, 0x6011)
	wantWindowWrites(t, "the device open", d, in, "01", a)

	clock.advance(60 * time.Second)
	closeReport(t, d, in, "VA1000000001", 5, 0x6012)
	wantWindowWrites(t, "the device close", d, in, "00", a)
	wantNoWrites(t, "the device close on the relayed member", d, b)
	wantWindow(t, "after the device close", zoneWindowOf(t, url, 5), false, "", time.Time{})

	clock.advance(time.Second)
	report(t, d, in, "VA1000000006", 5, 0x6013)
	wantWindowWrites(t, "the second device open", d, in, "01", a)
	externalEnd := clock.now().Add(2 * time.Hour)
	if _, code := postWindow(t, url, 5, true); code != http.StatusOK {
		t.Fatalf("external open: code=%d", code)
	}
	closeReport(t, d, in, "VA1000000006", 5, 0x6014)
	wantNoWrites(t, "a device close under an external hold", d, a, b)
	wantWindow(t, "a device close under an external hold", zoneWindowOf(t, url, 5), true, "external", externalEnd)
}

// TestWindowDeviceReportHonoursZoneConfig: a zone with the detection off takes
// no write, a zone duration other than the default sets the deadline, and a
// device outside the zone opens nothing.
func TestWindowDeviceReportHonoursZoneConfig(t *testing.T) {
	off, short := false, 600
	in := windowInstall(t, func(in *tado.Install) {
		for i := range in.Zones {
			switch in.Zones[i].Number {
			case 5:
				in.Zones[i].OpenWindowDetection = &off
			case 10:
				in.Zones[i].OpenWindowDurationSec = &short
			}
		}
	})
	clock := newTestClock(windowStart)
	d, url := newWindowDaemon(t, in, clock, nil)
	a, b := wireOf(t, in, "VA1000000006"), wireOf(t, in, "VA1000000001")
	bedroom := wireOf(t, in, "VA1000000005")
	seedBlock(t, d, in, "VA1000000006", 5)
	seedBlock(t, d, in, "VA1000000001", 5)
	seedBlock(t, d, in, "VA1000000005", 10)

	report(t, d, in, "VA1000000006", 5, 0x6101)
	wantNoWrites(t, "detection off", d, a, b)
	wantWindow(t, "detection off", zoneWindowOf(t, url, 5), false, "", time.Time{})

	report(t, d, in, "VA1000000006", 10, 0x6102)
	wantNoWrites(t, "a report from outside the zone", d, bedroom)

	report(t, d, in, "VA1000000005", 10, 0x6103)
	wantWindowWrites(t, "the 600 s zone", d, in, "01", bedroom)
	wantWindow(t, "the 600 s zone", zoneWindowOf(t, url, 10), true, "device", windowStart.Add(600*time.Second))
}

// TestWindowExternalOpenClose: the zone API opens and closes the external
// window, and a lost close ends at the installation's cap.
func TestWindowExternalOpenClose(t *testing.T) {
	maxSec := 1800
	in := windowInstall(t, func(in *tado.Install) { in.Options.ExternalWindowMaxSec = &maxSec })
	clock := newTestClock(windowStart)
	d, url := newWindowDaemon(t, in, clock, nil)
	a, b := wireOf(t, in, "VA1000000006"), wireOf(t, in, "VA1000000001")
	seedBlock(t, d, in, "VA1000000006", 5)
	seedBlock(t, d, in, "VA1000000001", 5)

	w, code := postWindow(t, url, 5, true)
	if code != http.StatusOK {
		t.Fatalf("open: code=%d", code)
	}
	wantWindow(t, "the open answer", w, true, "external", windowStart.Add(30*time.Minute))
	wantWindowWrites(t, "the external open", d, in, "01", a)
	wantNoWrites(t, "the external open on the relayed member", d, b)

	clock.advance(time.Minute)
	if _, code := postWindow(t, url, 5, true); code != http.StatusOK {
		t.Fatalf("second open: code=%d", code)
	}
	wantNoWrites(t, "a repeated open", d, a, b)

	w, code = postWindow(t, url, 5, false)
	if code != http.StatusOK {
		t.Fatalf("close: code=%d", code)
	}
	wantWindow(t, "the close answer", w, false, "", time.Time{})
	wantWindowWrites(t, "the external close", d, in, "00", a)
	wantNoWrites(t, "the external close on the relayed member", d, b)
	if _, code := postWindow(t, url, 5, false); code != http.StatusOK {
		t.Fatalf("second close: code=%d", code)
	}
	wantNoWrites(t, "a repeated close", d, a, b)

	// The cap: no close follows this open.
	if _, code := postWindow(t, url, 5, true); code != http.StatusOK {
		t.Fatalf("reopen: code=%d", code)
	}
	wantWindowWrites(t, "the reopen", d, in, "01", a)
	wantNoWrites(t, "the reopen on the relayed member", d, b)
	clock.advance(30*time.Minute - time.Second)
	tick(t, d)
	wantNoWrites(t, "before the cap", d, a, b)
	clock.advance(time.Second)
	tick(t, d)
	wantWindowWrites(t, "the cap", d, in, "00", a)
	wantNoWrites(t, "the cap on the relayed member", d, b)
	wantWindow(t, "after the cap", zoneWindowOf(t, url, 5), false, "", time.Time{})

	var bad map[string]any
	if code := postJSON(t, http.MethodPost, url+"/api/zones/5/window", map[string]any{}, &bad); code != http.StatusBadRequest {
		t.Errorf("a body without open: code=%d, want 400", code)
	}
	if code := postJSON(t, http.MethodPost, url+"/api/zones/4/window", map[string]any{"open": true}, &bad); code != http.StatusNotFound {
		t.Errorf("an unknown zone: code=%d, want 404", code)
	}
}

// TestWindowExternalCapDefault: an installation that states no cap closes an
// external window after two hours.
func TestWindowExternalCapDefault(t *testing.T) {
	in := windowInstall(t, nil)
	clock := newTestClock(windowStart)
	_, url := newWindowDaemon(t, in, clock, nil)
	w, code := postWindow(t, url, 3, true)
	if code != http.StatusOK {
		t.Fatalf("open: code=%d", code)
	}
	wantWindow(t, "the default cap", w, true, "external", windowStart.Add(2*time.Hour))
}

// TestWindowSourcesOverlap: with both sources holding the zone, it closes only
// when the device timer has run out and the external source has closed,
// whichever comes last.
func TestWindowSourcesOverlap(t *testing.T) {
	in := windowInstall(t, nil)
	clock := newTestClock(windowStart)
	d, url := newWindowDaemon(t, in, clock, nil)
	a, b := wireOf(t, in, "VA1000000006"), wireOf(t, in, "VA1000000001")
	seedBlock(t, d, in, "VA1000000006", 5)
	seedBlock(t, d, in, "VA1000000001", 5)

	// The device timer runs out first.
	report(t, d, in, "VA1000000006", 5, 0x6201)
	wantWindowWrites(t, "the device open", d, in, "01", a)
	wantNoWrites(t, "the device open on the relayed member", d, b)
	clock.advance(100 * time.Second)
	if _, code := postWindow(t, url, 5, true); code != http.StatusOK {
		t.Fatalf("open: code=%d", code)
	}
	wantNoWrites(t, "the external open on an open zone", d, a, b)
	externalEnd := windowStart.Add(100*time.Second + 2*time.Hour)
	wantWindow(t, "both", zoneWindowOf(t, url, 5), true, "both", externalEnd)
	clock.advance(800 * time.Second)
	tick(t, d)
	wantNoWrites(t, "the device timer under an external hold", d, a, b)
	wantWindow(t, "the external hold alone", zoneWindowOf(t, url, 5), true, "external", externalEnd)
	if _, code := postWindow(t, url, 5, false); code != http.StatusOK {
		t.Fatalf("close: code=%d", code)
	}
	wantWindowWrites(t, "the last release", d, in, "00", a)
	wantNoWrites(t, "the last release on the relayed member", d, b)

	// The external source closes first.
	start := clock.now()
	if _, code := postWindow(t, url, 5, true); code != http.StatusOK {
		t.Fatalf("open: code=%d", code)
	}
	wantWindowWrites(t, "the external open", d, in, "01", a)
	wantNoWrites(t, "the external open on the relayed member", d, b)
	report(t, d, in, "VA1000000001", 5, 0x6202)
	wantNoWrites(t, "the device open on an open zone", d, a, b)
	if _, code := postWindow(t, url, 5, false); code != http.StatusOK {
		t.Fatalf("close: code=%d", code)
	}
	wantNoWrites(t, "the external close under a device hold", d, a, b)
	wantWindow(t, "the device hold alone", zoneWindowOf(t, url, 5), true, "device", start.Add(900*time.Second))
	clock.advance(900 * time.Second)
	tick(t, d)
	wantWindowWrites(t, "the device timer, last", d, in, "00", a)
	wantNoWrites(t, "the device timer, last on the relayed member", d, b)
}

// TestWindowWriteKeepsZoneState: the window write resends the block the member
// holds, home state, scheduled 6200 and running overlay included, and flips
// 62e0 alone.
func TestWindowWriteKeepsZoneState(t *testing.T) {
	in := windowInstall(t, nil)
	clock := newTestClock(windowStart)
	d, url := newWindowDaemon(t, in, clock, nil)
	bedroom := wireOf(t, in, "VA1000000005")
	var view map[string]any
	if code := postJSON(t, http.MethodPut, url+"/api/zones/10/schedule",
		allDayProgram(map[string]any{"minute": 0, "setpoint_c": 19.5}), &view); code != http.StatusOK {
		t.Fatalf("schedule: code=%d %v", code, view)
	}
	seedBlock(t, d, in, "VA1000000005", 10,
		tado.TLV{Tag: "6280", Val: []byte{0x08, 0x34}}, tado.TLV{Tag: "6240", Val: []byte{0x02}},
		tado.TLV{Tag: "6260", Val: []byte{0x01}}, tado.TLV{Tag: "6200", Val: []byte{0x07, 0x9e}},
		tado.TLV{Tag: "62e0", Val: []byte{0x00}})
	dropQueue(t, d, bedroom)

	if _, code := postWindow(t, url, 10, true); code != http.StatusOK {
		t.Fatalf("open: code=%d", code)
	}
	want := map[string]string{
		"6160": "01", "6180": "00", "6020": "01", "61e0": "01", "6200": "079e",
		"6240": "02", "6260": "01", "6280": "0834", "62e0": "01",
	}
	tags := nextZoneWrite(t, d, bedroom)
	wantTags(t, "the window write", tags, want, "6440")
	if len(tags) != len(want) {
		t.Errorf("the window write carries %v, want exactly %v", tags, want)
	}
}

// TestWindowRetriesUnwritableMember: at home, a zone with no program and no
// member block has no 6200 to restate. Its writer retries on the 30 s cadence,
// not on every tick, and the log names it once. The other member's report
// gives the zone its 6200, so the writer takes its write on the next retry.
func TestWindowRetriesUnwritableMember(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	in := windowInstall(t, nil)
	clock := newTestClock(windowStart)
	d, url := newWindowDaemon(t, in, clock, nil)
	a, b := wireOf(t, in, "VA1000000006"), wireOf(t, in, "VA1000000001")

	if _, code := postWindow(t, url, 5, true); code != http.StatusOK {
		t.Fatalf("open: code=%d", code)
	}
	wantNoWrites(t, "the unknown writer", d, a, b)
	for i := 0; i < 3; i++ {
		clock.advance(windowRetryEvery)
		tick(t, d)
	}
	wantNoWrites(t, "the retried unknown writer", d, a, b)
	if n := strings.Count(logs.String(), tado.ErrHeatingBlockUnavailable.Error()); n != 1 {
		t.Errorf("the log names the missing block %d times, want once:\n%s", n, logs.String())
	}

	seedBlock(t, d, in, "VA1000000001", 5)
	clock.advance(windowRetryEvery - time.Second)
	tick(t, d)
	wantNoWrites(t, "before the retry cadence", d, a, b)
	clock.advance(time.Second)
	tick(t, d)
	wantWindowWrites(t, "the retried writer", d, in, "01", a)
	wantNoWrites(t, "the retried writer on the relayed member", d, b)
	clock.advance(windowRetryEvery)
	tick(t, d)
	wantNoWrites(t, "the confirmed writer", d, a, b)
}

// TestWindowWritesThermostatWriter: the wall thermostat writes zone 1, so it
// takes the window write, as in the captured zone 1 report; the valve of the
// zone takes the block from the thermostat's relayed echo.
func TestWindowWritesThermostatWriter(t *testing.T) {
	in := windowInstall(t, nil)
	clock := newTestClock(windowStart)
	d, url := newWindowDaemon(t, in, clock, nil)
	ru, valve := wireOf(t, in, "RU1000000001"), wireOf(t, in, "VA1000000003")
	seedBlock(t, d, in, "RU1000000001", 1)
	seedBlock(t, d, in, "VA1000000003", 1)

	if _, code := postWindow(t, url, 1, true); code != http.StatusOK {
		t.Fatalf("open: code=%d", code)
	}
	wantWindowWrites(t, "the thermostat", d, in, "01", ru)
	wantNoWrites(t, "the valve", d, valve)
	for i := 0; i < 3; i++ {
		clock.advance(windowRetryEvery)
		tick(t, d)
	}
	wantNoWrites(t, "the thermostat after the retries", d, ru, valve)
	if _, code := postWindow(t, url, 1, false); code != http.StatusOK {
		t.Fatalf("close: code=%d", code)
	}
	wantWindowWrites(t, "the thermostat close", d, in, "00", ru)
	wantNoWrites(t, "the valve close", d, valve)
}

// TestWindowWriterOfflineWritesEveryMember: an offline writer echoes nothing to
// the other members, so every member takes the window write. Once the writer is
// heard again, it takes the next write alone.
func TestWindowWriterOfflineWritesEveryMember(t *testing.T) {
	offline := 600
	in := windowInstall(t, func(in *tado.Install) { in.Options.OfflineAfterSec = &offline })
	clock := newTestClock(windowStart)
	d, url := newWindowDaemon(t, in, clock, nil)
	a, b := wireOf(t, in, "VA1000000006"), wireOf(t, in, "VA1000000001")
	seedBlock(t, d, in, "VA1000000006", 5)
	clock.advance(11 * time.Minute)
	seedBlock(t, d, in, "VA1000000001", 5)

	if _, code := postWindow(t, url, 5, true); code != http.StatusOK {
		t.Fatalf("open: code=%d", code)
	}
	wantWindowWrites(t, "the open with the writer offline", d, in, "01", a, b)
	tick(t, d)

	seedBlock(t, d, in, "VA1000000006", 5,
		tado.TLV{Tag: "6280", Val: []byte{0x09, 0xc4}}, tado.TLV{Tag: "6240", Val: []byte{0x00}},
		tado.TLV{Tag: "6260", Val: []byte{0x01}}, tado.TLV{Tag: "6200", Val: []byte{0x07, 0xd0}},
		tado.TLV{Tag: "62e0", Val: []byte{0x01}})
	if _, code := postWindow(t, url, 5, false); code != http.StatusOK {
		t.Fatalf("close: code=%d", code)
	}
	wantWindowWrites(t, "the close with the writer back", d, in, "00", a)
	wantNoWrites(t, "the close on the relayed member", d, b)
}

// TestWindowNoWriterWritesEveryMember: a zone of two members that elects no
// demand reporter has no writer, so every member takes the window write.
func TestWindowNoWriterWritesEveryMember(t *testing.T) {
	in := windowInstall(t, func(in *tado.Install) {
		for i := range in.Zones {
			if in.Zones[i].Number == 5 {
				in.Zones[i].Reporter = nil
			}
		}
	})
	clock := newTestClock(windowStart)
	d, url := newWindowDaemon(t, in, clock, nil)
	a, b := wireOf(t, in, "VA1000000006"), wireOf(t, in, "VA1000000001")
	seedBlock(t, d, in, "VA1000000006", 5)
	seedBlock(t, d, in, "VA1000000001", 5)

	if _, code := postWindow(t, url, 5, true); code != http.StatusOK {
		t.Fatalf("open: code=%d", code)
	}
	wantWindowWrites(t, "the open with no writer", d, in, "01", a, b)
}

// TestWindowGivesUpAndConverges: a member that never becomes writable is
// flagged after windowRetryLimit attempts and written no more, and the stamp of
// its next setpoint write settles it.
func TestWindowGivesUpAndConverges(t *testing.T) {
	in := windowInstall(t, nil)
	clock := newTestClock(windowStart)
	d, url := newWindowDaemon(t, in, clock, nil)
	b := wireOf(t, in, "VA1000000002")
	if _, code := postWindow(t, url, 3, true); code != http.StatusOK {
		t.Fatalf("open: code=%d", code)
	}
	wantNoWrites(t, "the unknown member", d, b)
	for i := 1; i < windowRetryLimit; i++ {
		clock.advance(windowRetryEvery)
		tick(t, d)
	}
	undelivered := func() []any {
		var zones struct {
			Declared []struct {
				Number int            `json:"number"`
				Window map[string]any `json:"window"`
			} `json:"declared"`
		}
		if code := getJSON(t, url+"/api/zones", &zones); code != http.StatusOK {
			t.Fatalf("zones: code=%d", code)
		}
		for _, z := range zones.Declared {
			if z.Number == 3 {
				list, _ := z.Window["undelivered"].([]any)
				return list
			}
		}
		t.Fatal("zone 3 missing")
		return nil
	}
	if got := undelivered(); len(got) != 1 || got[0] != b {
		t.Fatalf("undelivered = %v, want [%s]", got, b)
	}

	// The block arrives after the engine gave up: it writes nothing more.
	seedBlock(t, d, in, "VA1000000002", 3)
	clock.advance(windowRetryEvery)
	tick(t, d)
	wantNoWrites(t, "after giving up", d, b)

	var res map[string]any
	if code := postJSON(t, http.MethodPost, url+"/api/nodes/VA1000000002/setpoint",
		map[string]any{"tempC": 21.0, "mode": "manual", "id": 3}, &res); code != http.StatusOK {
		t.Fatalf("setpoint: code=%d %v", code, res)
	}
	wantTags(t, "the setpoint stamp", deliver(t, d, in, b), map[string]string{"62e0": "01", "6240": "02"})
	tick(t, d)
	if got := undelivered(); len(got) != 0 {
		t.Errorf("undelivered after the setpoint = %v, want none", got)
	}
}

// TestWindowSurvivesRestart: an open window and its deadline come back from
// the database, and the deadline keeps counting across the downtime.
func TestWindowSurvivesRestart(t *testing.T) {
	path := t.TempDir() + "/state.db"
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	in := windowInstall(t, nil)
	clock := newTestClock(windowStart)
	d, url := newWindowDaemon(t, in, clock, store)
	office := wireOf(t, in, "VA1000000002")
	seedBlock(t, d, in, "VA1000000002", 3)
	if _, code := postWindow(t, url, 3, true); code != http.StatusOK {
		t.Fatalf("open: code=%d", code)
	}
	wantWindowWrites(t, "the open", d, in, "01", office)
	tick(t, d)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	clock.advance(time.Hour)
	d, url = newWindowDaemon(t, in, clock, reopened)
	seedBlock(t, d, in, "VA1000000002", 3, openBlock(0x01)...)
	wantWindow(t, "after the restart", zoneWindowOf(t, url, 3), true, "external", windowStart.Add(2*time.Hour))
	tick(t, d)
	wantNoWrites(t, "a confirmed open after the restart", d, office)

	clock.advance(time.Hour)
	tick(t, d)
	wantWindowWrites(t, "the stored deadline", d, in, "00", office)
	tick(t, d)
	windows, owed, err := reopened.ZoneWindows(context.Background())
	if err != nil || len(windows) != 0 || len(owed) != 0 {
		t.Errorf("stored after the close: windows %v owed %v err=%v, want none", windows, owed, err)
	}
}

// TestWindowCloseResentAfterRestart: a close the queue still held when the
// daemon stopped goes out again after the restart.
func TestWindowCloseResentAfterRestart(t *testing.T) {
	path := t.TempDir() + "/state.db"
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	in := windowInstall(t, nil)
	clock := newTestClock(windowStart)
	d, url := newWindowDaemon(t, in, clock, store)
	office := wireOf(t, in, "VA1000000002")
	seedBlock(t, d, in, "VA1000000002", 3)
	if _, code := postWindow(t, url, 3, true); code != http.StatusOK {
		t.Fatalf("open: code=%d", code)
	}
	wantWindowWrites(t, "the open", d, in, "01", office)
	if _, code := postWindow(t, url, 3, false); code != http.StatusOK {
		t.Fatalf("close: code=%d", code)
	}
	if n := pending(t, d, office); n != 1 {
		t.Fatalf("the close is not queued: %d writes", n)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	d, url = newWindowDaemon(t, in, clock, reopened)
	wantWindow(t, "after the restart", zoneWindowOf(t, url, 3), false, "", time.Time{})
	// The valve still reports the open window the lost close should have ended.
	seedBlock(t, d, in, "VA1000000002", 3, openBlock(0x01)...)
	clock.advance(windowRetryEvery)
	tick(t, d)
	wantWindowWrites(t, "the resent close", d, in, "00", office)
	clock.advance(windowRetryEvery)
	tick(t, d)
	wantNoWrites(t, "the confirmed close", d, office)
}

// restartAfterOpen opens the window of zone 3 on a valve that reports its
// block, delivers the open and restarts the daemon from the same store. On the
// first daemon, edit runs before the valve reports and seeded after.
func restartAfterOpen(t *testing.T, edit, seeded func(d *daemon, in *tado.Install, url string)) (*daemon, string, *tado.Install, *stateStore, *testClock) {
	t.Helper()
	path := t.TempDir() + "/state.db"
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	in := windowInstall(t, nil)
	clock := newTestClock(windowStart)
	d, url := newWindowDaemon(t, in, clock, store)
	office := wireOf(t, in, "VA1000000002")
	if edit != nil {
		edit(d, in, url)
	}
	seedBlock(t, d, in, "VA1000000002", 3)
	if seeded != nil {
		seeded(d, in, url)
	}
	if _, code := postWindow(t, url, 3, true); code != http.StatusOK {
		t.Fatalf("open: code=%d", code)
	}
	wantWindowWrites(t, "the open", d, in, "01", office)
	tick(t, d)
	// The shutdown save: a restart within zoneStateSaveEvery of the open would
	// otherwise keep the zone cache from before it.
	if err := d.saveDesiredState(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	clock.advance(time.Minute)
	d, url = newWindowDaemon(t, in, clock, reopened)
	tick(t, d)
	wantNoWrites(t, "a confirmed open after the restart", d, office)
	return d, url, in, reopened, clock
}

// wantNothingOwed checks that the store holds no window and no owed write.
func wantNothingOwed(t *testing.T, store *stateStore) {
	t.Helper()
	windows, owed, err := store.ZoneWindows(context.Background())
	if err != nil || len(windows) != 0 || len(owed) != 0 {
		t.Errorf("stored after the close: windows %v owed %v err=%v, want none", windows, owed, err)
	}
}

// TestWindowCloseAfterRestartWithoutBlock: after a restart the emulator holds
// no block for the valve. The close goes out at once in the block a zone
// rewrite restates, from the flat program of the zone, and its 2.04 settles
// the owed write.
func TestWindowCloseAfterRestartWithoutBlock(t *testing.T) {
	d, url, in, store, clock := restartAfterOpen(t, putFlatProgram(t), nil)
	office := wireOf(t, in, "VA1000000002")

	if _, code := postWindow(t, url, 3, false); code != http.StatusOK {
		t.Fatalf("close: code=%d", code)
	}
	if n := pending(t, d, office); n != 1 {
		t.Fatalf("the close: %d queued writes, want 1", n)
	}
	want := map[string]string{
		"6160": "01", "6180": "00", "6020": "01", "61e0": "01", "6200": "079e", "6240": "00", "62e0": "00",
	}
	tags := deliver(t, d, in, office)
	wantTags(t, "the close", tags, want, "6440")
	if len(tags) != len(want) {
		t.Errorf("the close carries %v, want exactly %v", tags, want)
	}
	tick(t, d)
	wantNothingOwed(t, store)
	for i := 0; i < 3; i++ {
		clock.advance(windowRetryEvery)
		tick(t, d)
	}
	wantNoWrites(t, "the confirmed close", d, office)
	wantWindow(t, "after the close", zoneWindowOf(t, url, 3), false, "", time.Time{})
}

// putFlatProgram gives zone 3 an all-day program at 19.5 C.
func putFlatProgram(t *testing.T) func(*daemon, *tado.Install, string) {
	return func(_ *daemon, _ *tado.Install, url string) {
		var view map[string]any
		if code := postJSON(t, http.MethodPut, url+"/api/zones/3/schedule",
			allDayProgram(map[string]any{"minute": 0, "setpoint_c": 19.5}), &view); code != http.StatusOK {
			t.Fatalf("schedule: code=%d %v", code, view)
		}
	}
}

// TestWindowCloseAfterRestartKeepsManualOverlay: the close that follows a
// restart takes the overlay from the stored zone cache, so a manual 21 C
// overlay stays in place.
func TestWindowCloseAfterRestartKeepsManualOverlay(t *testing.T) {
	d, url, in, _, _ := restartAfterOpen(t, putFlatProgram(t), func(d *daemon, in *tado.Install, url string) {
		var res map[string]any
		if code := postJSON(t, http.MethodPost, url+"/api/nodes/VA1000000002/setpoint",
			map[string]any{"tempC": 21.0, "mode": "manual", "id": 3}, &res); code != http.StatusOK {
			t.Fatalf("setpoint: code=%d %v", code, res)
		}
		wantTags(t, "the manual setpoint", deliver(t, d, in, wireOf(t, in, "VA1000000002")),
			map[string]string{"6240": "02", "6280": "0834"})
		if err := d.saveDesiredState(); err != nil {
			t.Fatal(err)
		}
	})
	office := wireOf(t, in, "VA1000000002")

	if _, code := postWindow(t, url, 3, false); code != http.StatusOK {
		t.Fatalf("close: code=%d", code)
	}
	want := map[string]string{
		"6160": "01", "6180": "00", "6020": "01", "61e0": "01", "6200": "079e",
		"6240": "02", "6260": "01", "6280": "0834", "62e0": "00",
	}
	tags := deliver(t, d, in, office)
	wantTags(t, "the close", tags, want, "6440")
	if len(tags) != len(want) {
		t.Errorf("the close carries %v, want exactly %v", tags, want)
	}
}

// TestWindowCloseAfterCrashKeepsDeliveredOverlay: a delivered manual overlay
// reaches the database without any other save, so the close after a restart
// that skipped the shutdown save still restates it.
func TestWindowCloseAfterCrashKeepsDeliveredOverlay(t *testing.T) {
	path := t.TempDir() + "/state.db"
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	in := windowInstall(t, nil)
	clock := newTestClock(windowStart)
	d, url := newWindowDaemon(t, in, clock, store)
	office := wireOf(t, in, "VA1000000002")
	putFlatProgram(t)(d, in, url)
	seedBlock(t, d, in, "VA1000000002", 3)
	var res map[string]any
	if code := postJSON(t, http.MethodPost, url+"/api/nodes/VA1000000002/setpoint",
		map[string]any{"tempC": 21.0, "mode": "manual", "id": 3}, &res); code != http.StatusOK {
		t.Fatalf("setpoint: code=%d %v", code, res)
	}
	wantTags(t, "the manual setpoint", deliver(t, d, in, office), map[string]string{"6240": "02", "6280": "0834"})
	if _, code := postWindow(t, url, 3, true); code != http.StatusOK {
		t.Fatalf("open: code=%d", code)
	}
	wantWindowWrites(t, "the open", d, in, "01", office)
	tick(t, d)
	clock.advance(zoneStateSaveEvery)
	if _, err := d.do(func(e *tado.EmuState) (any, error) { d.flushUnsaved(context.Background(), e); return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	clock.advance(time.Minute)
	d, url = newWindowDaemon(t, in, clock, reopened)
	tick(t, d)
	wantNoWrites(t, "a confirmed open after the restart", d, office)
	if _, code := postWindow(t, url, 3, false); code != http.StatusOK {
		t.Fatalf("close: code=%d", code)
	}
	wantTags(t, "the close", deliver(t, d, in, office),
		map[string]string{"6240": "02", "6260": "01", "6280": "0834", "62e0": "00"}, "6440")
}

// TestWindowCloseAfterRestartWithoutProgram: at home a zone with no program
// restates the 6200 of the zone cache. The delivered open carried the reported
// 6200, so the cache keeps it through a restart that drops the valve's block,
// and the close goes out at once with it.
func TestWindowCloseAfterRestartWithoutProgram(t *testing.T) {
	d, url, in, store, _ := restartAfterOpen(t, nil, nil)
	office := wireOf(t, in, "VA1000000002")

	if _, code := postWindow(t, url, 3, false); code != http.StatusOK {
		t.Fatalf("close: code=%d", code)
	}
	if n := pending(t, d, office); n != 1 {
		t.Fatalf("the close: %s holds %d queued writes, want 1", office, n)
	}
	wantTags(t, "the close", deliver(t, d, in, office), map[string]string{"6200": "07d0", "62e0": "00"}, "6440")
	tick(t, d)
	wantNothingOwed(t, store)
}

// openBlock is the default seeded block with 62e0 = value.
func openBlock(value byte) []tado.TLV {
	return []tado.TLV{
		{Tag: "6280", Val: []byte{0x09, 0xc4}}, {Tag: "6240", Val: []byte{0x00}},
		{Tag: "6260", Val: []byte{0x01}}, {Tag: "6200", Val: []byte{0x07, 0xd0}},
		{Tag: "62e0", Val: []byte{value}},
	}
}

// TestWindowIgnoresHotWater: the hot water zone has no window, from either
// source.
func TestWindowIgnoresHotWater(t *testing.T) {
	in := windowInstall(t, nil)
	clock := newTestClock(windowStart)
	d, url := newWindowDaemon(t, in, clock, nil)
	ru := wireOf(t, in, "RU1000000001")

	var res map[string]any
	if code := postJSON(t, http.MethodPost, url+"/api/zones/0/window", map[string]any{"open": true}, &res); code != http.StatusBadRequest {
		t.Errorf("hot water open: code=%d %v, want 400", code, res)
	}
	report(t, d, in, "RU1000000001", 0, 0x6301)
	wantNoWrites(t, "a hot water report", d, ru)
	var health struct {
		Windows map[string]windowState `json:"windows"`
	}
	if code := getJSON(t, url+"/api/health", &health); code != http.StatusOK {
		t.Fatalf("health: code=%d", code)
	}
	if _, ok := health.Windows["0"]; ok {
		t.Error("health carries a window for the hot water zone")
	}
	v, err := d.do(func(*tado.EmuState) (any, error) { return len(d.windows), nil })
	if err != nil || v.(int) != 0 {
		t.Errorf("window state holds %v zones, want none", v)
	}
}
