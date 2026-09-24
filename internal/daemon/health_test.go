package daemon

import (
	"context"
	"encoding/json"
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
