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
	Warning         string `json:"warning"`
	StartedAt       string `json:"startedAt"`
	UptimeSec       *int   `json:"uptimeSec"`
	Unhandled       *int   `json:"unhandled"`
	Frames          int    `json:"frames"`
	MICFails        int    `json:"micFails"`
	RXQueue         *int   `json:"rxQueue"`
	Taps            *int   `json:"taps"`
	Downlink        struct {
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
	if health.Mode != "mock" || health.UptimeSec == nil || health.Unhandled == nil ||
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

// Health tells the operator that the receiver holds no measured offset, and
// names the command that measures one. A database with a stored register keeps
// the warning out.
func TestHealthReportsTheCalibration(t *testing.T) {
	tests := []struct {
		name   string
		stored bool
	}{
		{name: "no calibration"},
		{name: "stored calibration", stored: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStore(t, filepath.Join(t.TempDir(), "state.db"))
			if tt.stored {
				if err := store.SetFSCTRL0(context.Background(), 0x33); err != nil {
					t.Fatal(err)
				}
			}
			emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
			d := newDaemon(defaultInstall, emu, radio.NewMock(nil), true, "live")
			d.ulog = tado.NewUnhandledLogger(io.Discard)
			d.setCalibration(store, "/dev/ttyUSB0")
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
			if health.Calibrated == nil || *health.Calibrated != tt.stored {
				t.Fatalf("calibrated=%v want %t", health.Calibrated, tt.stored)
			}
			switch {
			case tt.stored && health.Warning != "":
				t.Errorf("warning=%q, want none", health.Warning)
			case !tt.stored && !strings.Contains(health.Warning, "freedo-dev autotune -p /dev/ttyUSB0"):
				t.Errorf("warning=%q, want the autotune command with the port", health.Warning)
			}
		})
	}
}

// The other modes have no receiver of their own, so they carry no calibration.
func TestHealthOmitsTheCalibrationOffLive(t *testing.T) {
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
	var health healthResp
	if code := getBounded(t, srv.URL+"/api/health", &health); code != http.StatusOK {
		t.Fatalf("status=%d, want 200", code)
	}
	if health.Calibrated != nil || health.Warning != "" {
		t.Fatalf("calibrated=%v warning=%q, want both absent", health.Calibrated, health.Warning)
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
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = host })
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
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
	time.Local = time.FixedZone("CET", 3600)
	if w := utcZoneWarning(tado.Options{}, now); w != "" {
		t.Errorf("a host zone of +01:00 warns: %q", w)
	}
}
