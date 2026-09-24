package daemon

// device_health_test: the device alerts and the zone fallback in /api/health.
// Frames, values and clocks are synthetic.

import (
	"bytes"
	"log"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"freedo/tado"
)

type deviceHealth struct {
	Device       string  `json:"device"`
	Short        string  `json:"short"`
	Kind         string  `json:"kind"`
	BatteryMV    int     `json:"batteryMV"`
	LowBattery   bool    `json:"lowBattery"`
	Fault        bool    `json:"fault"`
	FaultWord    uint32  `json:"faultWord"`
	Offline      bool    `json:"offline"`
	LastSeen     *string `json:"lastSeen"`
	FallbackMode int     `json:"fallbackMode"`
}

type deviceAlert struct {
	Device string   `json:"device"`
	Alerts []string `json:"alerts"`
}

type alertHealth struct {
	Devices      []deviceHealth `json:"devices"`
	Alerts       []deviceAlert  `json:"alerts"`
	ZoneFallback []struct {
		Zone   int    `json:"zone"`
		Mode   int    `json:"mode"`
		Name   string `json:"name"`
		Source string `json:"source"`
		Since  string `json:"since"`
	} `json:"zoneFallback"`
}

func (h alertHealth) device(t *testing.T, name string) deviceHealth {
	t.Helper()
	for _, d := range h.Devices {
		if d.Device == name {
			return d
		}
	}
	t.Fatalf("device %s is missing from /api/health", name)
	return deviceHealth{}
}

func (h alertHealth) alerts() map[string][]string {
	out := map[string][]string{}
	for _, a := range h.Alerts {
		out[a.Device] = a.Alerts
	}
	return out
}

// newAlertDaemon is newWindowDaemon with the start of the daemon on the clock,
// so the silence of a device never heard counts from the test's own start.
func newAlertDaemon(t *testing.T, in *tado.Install, clock *testClock) (*daemon, string) {
	t.Helper()
	d, url := newWindowDaemon(t, in, clock, nil)
	if _, err := d.do(func(*tado.EmuState) (any, error) { d.started = clock.now(); return nil, nil }); err != nil {
		t.Fatal(err)
	}
	return d, url
}

func alertHealthOf(t *testing.T, url string) alertHealth {
	t.Helper()
	var h alertHealth
	if code := getBounded(t, url+"/api/health", &h); code != http.StatusOK {
		t.Fatalf("GET /api/health: status %d", code)
	}
	return h
}

// deviceReport sends a PUT d/<serial>/<resource> from serial with one TLV.
func deviceReport(t *testing.T, d *daemon, in *tado.Install, serial, resource string, mid uint16, tlv tado.TLV) {
	t.Helper()
	feed(t, d, in, serial, false, tado.CoAPReq{
		Type: "CON", Code: "0.03", MID: mid,
		Path:    []string{"d", serial, resource},
		Options: []tado.CoAPOption{{Num: 12, Val: []byte{42}}},
		Payload: tlv.Encode(),
	})
}

func battery(mv int) tado.TLV {
	return tado.TLV{Tag: "0162", Val: []byte{byte(mv >> 8), byte(mv)}}
}

func faultWord(w uint32) tado.TLV {
	return tado.TLV{Tag: "01a3", Val: []byte{byte(w >> 24), byte(w >> 16), byte(w >> 8), byte(w)}}
}

// The default thresholds: 1100 mV per cell, so 2200 mV on a valve and 3300 mV
// on the wall thermostat, and one hour of silence.
func TestHealthDeviceAlerts(t *testing.T) {
	clock := newTestClock(windowStart)
	in := windowInstall(t, nil)
	d, url := newAlertDaemon(t, in, clock)

	deviceReport(t, d, in, "VA1000000005", "sen", 0x6101, battery(2100))
	deviceReport(t, d, in, "VA1000000007", "sen", 0x6102, battery(2300))
	deviceReport(t, d, in, "RU1000000001", "sen", 0x6103, battery(3200))
	deviceReport(t, d, in, "VA1000000002", "err", 0x6104, faultWord(4))
	deviceReport(t, d, in, "VA1000000006", "err", 0x6105, faultWord(0))

	h := alertHealthOf(t, url)
	if got := h.device(t, "VA1000000005"); !got.LowBattery || got.BatteryMV != 2100 || got.Offline || got.LastSeen == nil {
		t.Errorf("valve at 2100 mV = %+v, want low battery, online, seen", got)
	}
	if got := h.device(t, "VA1000000007"); got.LowBattery {
		t.Errorf("valve at 2300 mV = %+v, want no low battery", got)
	}
	if got := h.device(t, "RU1000000001"); !got.LowBattery || got.Kind != "thermostat" {
		t.Errorf("thermostat at 3200 mV = %+v, want low battery", got)
	}
	if got := h.device(t, "VA1000000002"); !got.Fault || got.FaultWord != 4 {
		t.Errorf("valve with fault word 4 = %+v, want fault 4", got)
	}
	if got := h.device(t, "VA1000000006"); got.Fault {
		t.Errorf("valve with fault word 0 = %+v, want no fault", got)
	}
	if got := h.device(t, "VA1000000004"); got.LowBattery || got.Offline || got.LastSeen != nil {
		t.Errorf("a device never heard, at the start = %+v, want no alert", got)
	}
	want := map[string][]string{
		"VA1000000005": {"lowBattery"},
		"RU1000000001": {"lowBattery"},
		"VA1000000002": {"fault"},
	}
	if got := h.alerts(); !reflect.DeepEqual(got, want) {
		t.Errorf("alerts = %v, want %v", got, want)
	}

	// 70 minutes on, only the valve that reported 20 minutes ago is online.
	clock.advance(50 * time.Minute)
	deviceReport(t, d, in, "VA1000000007", "sen", 0x6106, battery(2300))
	clock.advance(20 * time.Minute)
	h = alertHealthOf(t, url)
	if got := h.device(t, "VA1000000007"); got.Offline {
		t.Errorf("valve heard 20 minutes ago = %+v, want online", got)
	}
	for _, name := range []string{"VA1000000005", "RU1000000001", "VA1000000004"} {
		if got := h.device(t, name); !got.Offline {
			t.Errorf("%s silent for 70 minutes = %+v, want offline", name, got)
		}
	}
	alerts := h.alerts()
	if _, ok := alerts["VA1000000007"]; ok {
		t.Errorf("the online valve raised %v", alerts["VA1000000007"])
	}
	if got := alerts["VA1000000005"]; !reflect.DeepEqual(got, []string{"lowBattery", "offline"}) {
		t.Errorf("alerts of the low valve = %v, want lowBattery and offline", got)
	}

	// The node API carries the same flags.
	var node struct {
		LowBattery, Fault, Offline bool
		FaultWord                  uint32
	}
	if code := getJSON(t, url+"/api/nodes/VA1000000005", &node); code != http.StatusOK {
		t.Fatalf("GET node: status %d", code)
	}
	if !node.LowBattery || !node.Offline || node.Fault {
		t.Errorf("node API = %+v, want low battery and offline", node)
	}
	if code := getJSON(t, url+"/api/nodes/VA1000000002", &node); code != http.StatusOK || !node.Fault || node.FaultWord != 4 {
		t.Errorf("node API of the faulty valve = %d %+v, want fault 4", code, node)
	}
}

func TestHealthAlertThresholdOptions(t *testing.T) {
	cell, offline := 1200, 600
	in := windowInstall(t, func(in *tado.Install) {
		in.Options.LowBatteryCellMV, in.Options.OfflineAfterSec = &cell, &offline
	})
	clock := newTestClock(windowStart)
	d, url := newAlertDaemon(t, in, clock)

	deviceReport(t, d, in, "VA1000000005", "sen", 0x6201, battery(2300))
	h := alertHealthOf(t, url)
	if got := h.device(t, "VA1000000005"); !got.LowBattery || got.Offline {
		t.Errorf("valve at 2300 mV under a 2400 mV limit = %+v, want low battery only", got)
	}
	clock.advance(11 * time.Minute)
	if got := alertHealthOf(t, url).device(t, "VA1000000005"); !got.Offline {
		t.Errorf("valve silent for 11 minutes under a 10 minute limit = %+v, want offline", got)
	}
}

// The battery level is the highest reading of the last three hours: a dip under
// load neither raises the alert nor moves the percentage, a reading just past
// the threshold does not clear a raised alert, and fresh cells do.
func TestHealthLowBatteryWindow(t *testing.T) {
	clock := newTestClock(windowStart)
	in := windowInstall(t, nil)
	d, url := newAlertDaemon(t, in, clock)
	const valve = "VA1000000005"
	var mid uint16 = 0x6401
	step := func(after time.Duration, mV int, wantLow bool, wantPct int) {
		t.Helper()
		clock.advance(after)
		deviceReport(t, d, in, valve, "sen", mid, battery(mV))
		mid++
		var node struct {
			BatteryMV, BatteryPct int
			LowBattery            bool
		}
		if code := getJSON(t, url+"/api/nodes/"+valve, &node); code != http.StatusOK {
			t.Fatalf("GET node: status %d", code)
		}
		if node.BatteryMV != mV || node.LowBattery != wantLow || node.BatteryPct != wantPct {
			t.Errorf("after a %d mV reading: %+v, want low battery %v at %d %%", mV, node, wantLow, wantPct)
		}
		if got := alertHealthOf(t, url).device(t, valve); got.LowBattery != wantLow {
			t.Errorf("after a %d mV reading: health low battery %v, want %v", mV, got.LowBattery, wantLow)
		}
	}
	step(0, 2520, false, 40)
	step(10*time.Minute, 2100, false, 40)
	// The 2520 mV reading leaves the window; the 2100 mV dip is still in it.
	step(2*time.Hour+55*time.Minute, 2150, true, 0)
	step(20*time.Minute, 2250, true, 6)
	step(20*time.Minute, 3040, false, 100)
}

// Lowering the threshold clears a raised alert before the next reading.
func TestHealthLowBatteryThresholdChange(t *testing.T) {
	clock := newTestClock(windowStart)
	in := windowInstall(t, nil)
	d, url := newAlertDaemon(t, in, clock)
	deviceReport(t, d, in, "VA1000000005", "sen", 0x6501, battery(2100))
	if got := alertHealthOf(t, url).device(t, "VA1000000005"); !got.LowBattery {
		t.Fatalf("valve at 2100 mV = %+v, want low battery", got)
	}
	cell := 1000
	next := windowInstall(t, func(in *tado.Install) { in.Options.LowBatteryCellMV = &cell })
	if _, err := d.do(func(*tado.EmuState) (any, error) {
		_, _, err := d.setInstall(next)
		return nil, err
	}); err != nil {
		t.Fatal(err)
	}
	if got := alertHealthOf(t, url).device(t, "VA1000000005"); got.LowBattery {
		t.Errorf("valve at 2100 mV under a 2000 mV limit = %+v, want no low battery", got)
	}
}

// The zone fallback reaches /api/zones and /api/health, and its entry and exit
// reach the log.
func TestHealthZoneFallback(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	clock := newTestClock(windowStart)
	in := windowInstall(t, nil)
	d, url := newAlertDaemon(t, in, clock)
	zoneFallback := func(mid uint16, mode byte) {
		feed(t, d, in, "RU1000000001", false, tado.CoAPReq{
			Type: "CON", Code: "0.03", MID: mid,
			Path:    []string{"h", "7654321", "z", "2", "fallback"},
			Options: []tado.CoAPOption{{Num: 12, Val: []byte{42}}},
			Payload: tado.TLV{Tag: "6460", Val: []byte{mode}}.Encode(),
		})
	}

	zoneFallback(0x6301, tado.FallbackFrost)
	deviceReport(t, d, in, "RU1000000001", "fallback", 0x6302, tado.TLV{Tag: "0182", Val: []byte{tado.FallbackFrost}})

	h := alertHealthOf(t, url)
	if len(h.ZoneFallback) != 1 || h.ZoneFallback[0].Zone != 2 || h.ZoneFallback[0].Mode != 3 ||
		h.ZoneFallback[0].Name != "frost" || h.ZoneFallback[0].Since != windowStart.Format(time.RFC3339) {
		t.Fatalf("zoneFallback = %+v, want zone 2 in frost since the start", h.ZoneFallback)
	}
	if got := h.device(t, "RU1000000001"); got.FallbackMode != tado.FallbackFrost {
		t.Errorf("thermostat device fallback = %d, want 3", got.FallbackMode)
	}
	var zones struct {
		Demand []tado.ZoneView `json:"demand"`
	}
	if code := getJSON(t, url+"/api/zones", &zones); code != http.StatusOK {
		t.Fatalf("GET /api/zones: status %d", code)
	}
	modes := map[int]string{}
	for _, z := range zones.Demand {
		modes[z.Number] = z.Fallback
	}
	if modes[2] != "frost" || modes[3] != "none" {
		t.Errorf("zone fallbacks in /api/zones = %v, want zone 2 frost and zone 3 none", modes)
	}

	zoneFallback(0x6303, tado.FallbackNone)
	if h := alertHealthOf(t, url); len(h.ZoneFallback) != 0 {
		t.Errorf("zoneFallback after the exit = %+v, want empty", h.ZoneFallback)
	}
	for _, want := range []string{"zone 2 (reported by a205) entered fallback 3 (frost)", "device a205 entered fallback 3 (frost)",
		"zone 2 (reported by a205) left fallback 3 (frost)"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log misses %q:\n%s", want, logs.String())
		}
	}
}
