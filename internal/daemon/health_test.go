package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"freedo/internal/radio"
	"freedo/tado"
)

// healthResp is the whole GET /api/health answer. The pointer fields carry a
// zero value in a fast test, so only a pointer proves the field was sent.
type healthResp struct {
	OK              bool   `json:"ok"`
	Mode            string `json:"mode"`
	TX              bool   `json:"tx"`
	Engine          string `json:"engine"`
	BridgePresent   bool   `json:"bridgePresent"`
	TXArmed         bool   `json:"txArmed"`
	TransmitAllowed bool   `json:"transmitAllowed"`
	RadioError      string `json:"radioError"`
	FSCTRL0         string `json:"fsctrl0"`
	SerialErrors    *int   `json:"serialErrors"`
	Calibrated      *bool  `json:"calibrated"`
	Calibration     *struct {
		State    string `json:"state"`
		Readings *int   `json:"readings"`
		Want     *int   `json:"want"`
	} `json:"calibration"`
	Version   string `json:"version"`
	StartedAt string `json:"startedAt"`
	UptimeSec *int   `json:"uptimeSec"`
	Unhandled *int   `json:"unhandled"`
	Frames    int    `json:"frames"`
	MICFails  int    `json:"micFails"`
	RXQueue   *int   `json:"rxQueue"`
	Taps      *int   `json:"taps"`
	Downlink  struct {
		Sent      int `json:"sent"`
		Delivered int `json:"delivered"`
		Dropped   int `json:"dropped"`
	} `json:"downlink"`
	Queue struct {
		Pending     *int `json:"pending"`
		AwaitingCSL *int `json:"awaitingCSL"`
		FlowPending *int `json:"flowPending"`
		ZoneProxies *int `json:"zoneProxies"`
	} `json:"queue"`
}

// Health must answer even while the engine is stuck: it is the container health
// check, so it reports the engine as busy instead of waiting for it.
func TestHealthReportsBusyEngine(t *testing.T) {
	emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	d := newDaemon(defaultInstall, emu, radio.NewMock(nil), true, "mock")
	d.ulog = tado.NewUnhandledLogger(io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)
	srv := httptest.NewServer(d.mux())
	t.Cleanup(func() {
		srv.Close()
		cancel()
	})

	release := make(chan struct{})
	busy := make(chan struct{})
	go func() {
		_, _ = d.do(func(*tado.EmuState) (any, error) {
			close(busy)
			<-release
			return nil, nil
		})
	}()
	<-busy

	start := time.Now()
	var health healthResp
	code := getBounded(t, srv.URL+"/api/health", &health)
	elapsed := time.Since(start)
	close(release)

	if code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", code)
	}
	if health.OK || health.Engine != "busy" {
		t.Fatalf("health=%+v", health)
	}
	if health.Mode != "mock" || health.Version == "" || health.UptimeSec == nil || health.Unhandled == nil ||
		health.RXQueue == nil || health.Taps == nil {
		t.Fatalf("the non-engine fields are missing: %+v", health)
	}
	if health.Queue.Pending != nil {
		t.Fatalf("the busy answer carries engine fields: %+v", health)
	}
	if elapsed > healthEngineBudget+time.Second {
		t.Fatalf("health waited %s on the busy engine, want at most the %s budget", elapsed, healthEngineBudget)
	}
}

// getBounded is getJSON on a client with a timeout, so a budget regression
// fails here instead of hanging the package.
func getBounded(t *testing.T, url string, v any) int {
	t.Helper()
	resp, err := boundedClient.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil && err != io.EOF {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp.StatusCode
}

// registerRadio is a mock radio that also reports its frequency-offset register,
// like the live one.
type registerRadio struct {
	*radio.Mock
	fsctrl0 byte
}

func (r *registerRadio) FSCTRL0() byte { return r.fsctrl0 }

// Health carries the register of a live radio, and nothing in the other modes:
// an operator reads there the offset the receiver runs on.
func TestHealthReportsTheRegister(t *testing.T) {
	tests := []struct {
		name    string
		radio   radio.Radio
		fsctrl0 string
	}{
		{name: "mock radio", radio: radio.NewMock(nil)},
		{
			name:    "radio with a register",
			radio:   &registerRadio{Mock: radio.NewMock(nil), fsctrl0: 0x33},
			fsctrl0: "0x33",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
			d := newDaemon(defaultInstall, emu, tt.radio, true, "mock")
			d.ulog = tado.NewUnhandledLogger(io.Discard)
			ctx, cancel := context.WithCancel(context.Background())
			go d.run(ctx)
			srv := httptest.NewServer(d.mux())
			t.Cleanup(func() {
				srv.Close()
				cancel()
			})
			var health healthResp
			if code := getBounded(t, srv.URL+"/api/health", &health); code != http.StatusOK {
				t.Fatalf("status=%d, want 200", code)
			}
			if health.FSCTRL0 != tt.fsctrl0 {
				t.Errorf("fsctrl0=%q want %q", health.FSCTRL0, tt.fsctrl0)
			}
		})
	}
}

// linkRadio is a mock radio that also counts the serial replies it lost, like
// the live one.
type linkRadio struct {
	*radio.Mock
	lost uint64
}

func (r *linkRadio) SerialErrors() uint64 { return r.lost }

// Health carries the serial error counter of a live radio, and nothing in the
// other modes: an operator reads there a link that loses replies without
// reading the log.
func TestHealthReportsSerialErrors(t *testing.T) {
	tests := []struct {
		name  string
		radio radio.Radio
		want  *int
	}{
		{name: "mock radio", radio: radio.NewMock(nil)},
		{name: "radio that lost replies", radio: &linkRadio{Mock: radio.NewMock(nil), lost: 7}, want: new(7)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
			d := newDaemon(defaultInstall, emu, tt.radio, true, "mock")
			d.ulog = tado.NewUnhandledLogger(io.Discard)
			ctx, cancel := context.WithCancel(context.Background())
			go d.run(ctx)
			srv := httptest.NewServer(d.mux())
			t.Cleanup(func() {
				srv.Close()
				cancel()
			})
			var health healthResp
			if code := getBounded(t, srv.URL+"/api/health", &health); code != http.StatusOK {
				t.Fatalf("status=%d, want 200", code)
			}
			if (health.SerialErrors == nil) != (tt.want == nil) || (tt.want != nil && *health.SerialErrors != *tt.want) {
				t.Errorf("serialErrors=%v want %v", health.SerialErrors, tt.want)
			}
		})
	}
}

// calRadio is a live radio with a receiver calibration the test sets.
type calRadio struct {
	*radio.Mock
	mu      sync.Mutex
	cal     radio.Calibration
	results chan radio.CalibrationResult
	recals  int
}

func newCalRadio(cal radio.Calibration) *calRadio {
	return &calRadio{Mock: radio.NewMock(nil), cal: cal, results: make(chan radio.CalibrationResult, 1)}
}

func (r *calRadio) Calibration() radio.Calibration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cal
}

func (r *calRadio) Recalibrate() radio.Calibration {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recals++
	r.cal = radio.Calibration{State: radio.CalibrationMeasuring, Want: 20, Session: r.cal.Session + 1}
	return r.cal
}

func (r *calRadio) CalibrationResults() <-chan radio.CalibrationResult { return r.results }

func serveCalDaemon(t *testing.T, r radio.Radio, mode string, store *stateStore) *httptest.Server {
	t.Helper()
	emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	d := newDaemon(defaultInstall, emu, r, true, mode)
	d.ulog = tado.NewUnhandledLogger(io.Discard)
	if err := d.attachState(store); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)
	go d.storeCalibrations(ctx)
	srv := httptest.NewServer(d.mux())
	t.Cleanup(func() {
		srv.Close()
		cancel()
	})
	return srv
}

// Health reports where the register comes from, and the progress of a
// measurement while it runs.
func TestHealthReportsTheCalibration(t *testing.T) {
	tests := []struct {
		state      radio.CalibrationState
		readings   int
		calibrated bool
	}{
		{state: radio.CalibrationStored, calibrated: true},
		{state: radio.CalibrationMeasuring, readings: 7},
		{state: radio.CalibrationMeasured, calibrated: true},
	}
	for _, tt := range tests {
		t.Run(string(tt.state), func(t *testing.T) {
			cal := radio.Calibration{State: tt.state}
			if tt.state == radio.CalibrationMeasuring {
				cal.Readings, cal.Want = tt.readings, 20
			}
			srv := serveCalDaemon(t, newCalRadio(cal), "live", nil)
			var health healthResp
			if code := getBounded(t, srv.URL+"/api/health", &health); code != http.StatusOK {
				t.Fatalf("status=%d, want 200", code)
			}
			if health.Calibrated == nil || *health.Calibrated != tt.calibrated {
				t.Fatalf("calibrated=%v want %t", health.Calibrated, tt.calibrated)
			}
			c := health.Calibration
			if c == nil || c.State != string(tt.state) {
				t.Fatalf("calibration=%+v, want state %s", c, tt.state)
			}
			measuring := tt.state == radio.CalibrationMeasuring
			if (c.Readings != nil) != measuring || (c.Want != nil) != measuring {
				t.Fatalf("readings=%v want=%v, want both only in measuring", c.Readings, c.Want)
			}
			if measuring && (*c.Readings != tt.readings || *c.Want != 20) {
				t.Errorf("readings=%d want=%d, want %d of 20", *c.Readings, *c.Want, tt.readings)
			}
		})
	}
}

// The other modes have no receiver of their own, so they carry no calibration.
func TestHealthOmitsTheCalibrationOffLive(t *testing.T) {
	srv := serveCalDaemon(t, newCalRadio(radio.Calibration{State: radio.CalibrationStored}), "mock", nil)
	var health healthResp
	if code := getBounded(t, srv.URL+"/api/health", &health); code != http.StatusOK {
		t.Fatalf("status=%d, want 200", code)
	}
	if health.Calibrated != nil || health.Calibration != nil {
		t.Fatalf("calibrated=%v calibration=%+v, want both absent", health.Calibrated, health.Calibration)
	}
}

// A live radio with no register to move has no calibration: health omits it
// and the route refuses.
func TestHealthOmitsTheCalibrationWithoutARegister(t *testing.T) {
	store := newTestStore(t, filepath.Join(t.TempDir(), "state.db"))
	if err := store.SetFSCTRL0(context.Background(), 0x33); err != nil {
		t.Fatal(err)
	}
	srv := serveCalDaemon(t, newCalRadio(radio.Calibration{}), "live", store)
	var health healthResp
	if code := getBounded(t, srv.URL+"/api/health", &health); code != http.StatusOK {
		t.Fatalf("status=%d, want 200", code)
	}
	if health.Calibrated != nil || health.Calibration != nil {
		t.Fatalf("calibrated=%v calibration=%+v, want both absent", health.Calibrated, health.Calibration)
	}
	resp, err := boundedClient.Post(srv.URL+"/api/radio/calibrate", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status=%d, want 409", resp.StatusCode)
	}
	if _, ok, _ := store.FSCTRL0(context.Background()); !ok {
		t.Error("the refused route forgot the stored calibration")
	}
}

// The route forgets the stored value and starts a new measurement.
func TestRadioCalibrateRoute(t *testing.T) {
	store := newTestStore(t, filepath.Join(t.TempDir(), "state.db"))
	if err := store.SetFSCTRL0(context.Background(), 0x33); err != nil {
		t.Fatal(err)
	}
	r := newCalRadio(radio.Calibration{State: radio.CalibrationStored})
	srv := serveCalDaemon(t, r, "live", store)

	resp, err := boundedClient.Post(srv.URL+"/api/radio/calibrate", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status=%d body=%v, want 202", resp.StatusCode, body)
	}
	if body["state"] != "measuring" || body["readings"] != float64(0) || body["want"] != float64(20) {
		t.Errorf("body=%v, want measuring 0 of 20", body)
	}
	if _, ok, err := store.FSCTRL0(context.Background()); err != nil || ok {
		t.Errorf("stored FSCTRL0 ok=%t err=%v, want it forgotten", ok, err)
	}
	if r.recals != 1 {
		t.Errorf("Recalibrate calls=%d, want 1", r.recals)
	}
}

// Outside live mode there is no receiver to calibrate.
func TestRadioCalibrateRouteRefusesOffLive(t *testing.T) {
	store := newTestStore(t, filepath.Join(t.TempDir(), "state.db"))
	if err := store.SetFSCTRL0(context.Background(), 0x33); err != nil {
		t.Fatal(err)
	}
	srv := serveCalDaemon(t, radio.NewMock(nil), "mock", store)
	resp, err := boundedClient.Post(srv.URL+"/api/radio/calibrate", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusConflict || body["error"] == "" {
		t.Fatalf("status=%d body=%v, want 409 with an error", resp.StatusCode, body)
	}
	if v, ok, _ := store.FSCTRL0(context.Background()); !ok || v != 0x33 {
		t.Errorf("stored FSCTRL0=%#02x,%t, want 0x33 untouched", v, ok)
	}
}

// The daemon stores a measured calibration and logs it. A result of a session
// that a re-calibration replaced stays out of the store.
func TestDaemonStoresTheMeasuredCalibration(t *testing.T) {
	logs := captureLog(t)
	store := newTestStore(t, filepath.Join(t.TempDir(), "state.db"))
	r := newCalRadio(radio.Calibration{State: radio.CalibrationMeasured, Session: 2})
	d := newDaemon(defaultInstall, nil, r, true, "live")
	if err := d.attachState(store); err != nil {
		t.Fatal(err)
	}

	d.storeCalibration(r, radio.CalibrationResult{FSCTRL0: 0x44, Frames: 20, Sources: 1, Session: 1})
	if _, ok, _ := store.FSCTRL0(context.Background()); ok {
		t.Fatal("the result of a replaced session was stored")
	}
	d.storeCalibration(r, radio.CalibrationResult{FSCTRL0: 0x33, Frames: 20, Sources: 4, Session: 2})
	if v, ok, err := store.FSCTRL0(context.Background()); err != nil || !ok || v != 0x33 {
		t.Fatalf("stored FSCTRL0=%#02x,%t,%v, want 0x33", v, ok, err)
	}
	want := "receiver calibration: FSCTRL0 0x33 measured on 20 frames of 4 sources, stored\n"
	if !strings.HasSuffix(logs.String(), want) || strings.Count(logs.String(), "receiver calibration") != 1 {
		t.Errorf("log=%q, want only %q", logs.String(), want)
	}
}

// endedRadio is a live radio whose frame channel is already closed, with the
// error its loop ended on.
type endedRadio struct {
	*radio.Mock
	err error
}

func (r *endedRadio) TXArmed() bool     { return false }
func (r *endedRadio) RadioError() error { return r.err }

// Only a live radio loop that ended on an error fails health and makes Main
// leave. A finished replay, a mock or a live radio stopped on purpose do not.
func TestHealthAndExitOnEndedRadio(t *testing.T) {
	boom := errors.New("CSL queue poll: bridge gone")
	tests := []struct {
		name  string
		radio radio.Radio
		lost  bool
	}{
		{name: "finished mock", radio: radio.NewMock(nil)},
		{name: "stopped live radio", radio: &endedRadio{Mock: radio.NewMock(nil)}},
		{name: "live radio ended on an error", radio: &endedRadio{Mock: radio.NewMock(nil), err: boom}, lost: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
			d := newDaemon(defaultInstall, emu, tt.radio, true, "live")
			d.ulog = tado.NewUnhandledLogger(io.Discard)
			ctx, cancel := context.WithCancel(context.Background())
			go d.run(ctx)
			srv := httptest.NewServer(d.mux())
			t.Cleanup(func() {
				srv.Close()
				cancel()
			})
			var lost error
			select {
			case lost = <-d.radioLost:
			case <-time.After(200 * time.Millisecond):
			}
			if tt.lost != (lost != nil) || (lost != nil && !errors.Is(lost, boom)) {
				t.Fatalf("radio lost=%v, want lost=%t", lost, tt.lost)
			}
			var health healthResp
			code := getBounded(t, srv.URL+"/api/health", &health)
			wantCode := http.StatusOK
			if tt.lost {
				wantCode = http.StatusServiceUnavailable
			}
			if code != wantCode || health.OK == tt.lost {
				t.Fatalf("status=%d ok=%t, want %d", code, health.OK, wantCode)
			}
			if tt.lost && health.RadioError != boom.Error() {
				t.Fatalf("radioError=%q", health.RadioError)
			}
		})
	}
}

// TestHealthReportsTimezone: GET /api/health names the zone tag 02b2 follows
// and says whether it is the host's, and the start warning fires for a UTC host
// with no zone and no offset set, and for nothing else.
func TestHealthReportsTimezone(t *testing.T) {
	host := time.Local
	t.Cleanup(func() { time.Local = host })
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	// The host zone is set before any server runs: a write to time.Local races
	// with a live HTTP server, whose connection goroutines call time.Now.
	time.Local = time.FixedZone("CET", 3600)
	if w := utcZoneWarning(tado.Options{}, now); w != "" {
		t.Errorf("a host zone of +01:00 warns: %q", w)
	}
	time.Local = time.UTC
	for name, tc := range map[string]struct {
		edit    func(*tado.Install)
		want    tado.UTCZone
		warning bool
	}{
		"fixed offset": {func(in *tado.Install) {}, tado.UTCZone{Name: "fixed offset"}, false},
		"zone": {func(in *tado.Install) { in.Options.UTCOffsetMin, in.Options.Timezone = nil, "Europe/Rome" },
			tado.UTCZone{Name: "Europe/Rome", OffsetMin: 60}, false},
		"host UTC": {func(in *tado.Install) { in.Options.UTCOffsetMin = nil },
			tado.UTCZone{Name: "UTC", Fallback: true}, true},
	} {
		in := windowInstall(t, tc.edit)
		if got := utcZoneWarning(in.Options, now) != ""; got != tc.warning {
			t.Errorf("%s: warning = %v, want %v", name, got, tc.warning)
		}
		_, url := newWindowDaemon(t, in, newTestClock(now), nil)
		var health struct {
			Timezone tado.UTCZone `json:"timezone"`
		}
		if code := getJSON(t, url+"/api/health", &health); code != http.StatusOK || health.Timezone != tc.want {
			t.Errorf("%s: health timezone = %+v (code %d), want %+v", name, health.Timezone, code, tc.want)
		}
	}
}
