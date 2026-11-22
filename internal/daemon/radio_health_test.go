package daemon

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"freedo/cc1101"
	"freedo/internal/radio"
	"freedo/tado"
)

// healthRadio is a calibrating mock radio that also reports the airtime and the
// warnings of a live one.
type healthRadio struct {
	*calRadio
	airtime  radio.Airtime
	known    bool
	warnings []string
	waiting  int
	limit    int
	busy     uint64
}

func (r *healthRadio) Airtime() (radio.Airtime, bool) { return r.airtime, r.known }
func (r *healthRadio) Warnings() []string             { return r.warnings }
func (r *healthRadio) BootstrapWaiting() int          { return r.waiting }
func (r *healthRadio) BootstrapLimit() int            { return r.limit }
func (r *healthRadio) CSLBusy() uint64                { return r.busy }

type radioHealthResp struct {
	Calibrated *bool `json:"calibrated"`
	Airtime    *struct {
		UsedMs     uint32  `json:"usedMs"`
		BudgetMs   uint32  `json:"budgetMs"`
		Share      float64 `json:"share"`
		Limit      bool    `json:"limit"`
		Deferring  bool    `json:"deferring"`
		OverBudget bool    `json:"overBudget"`
	} `json:"airtime"`
	RadioWarnings []string `json:"radioWarnings"`
	Calibration   *struct {
		State    string `json:"state"`
		Residual *int   `json:"residual"`
	} `json:"calibration"`
}

// Health carries the airtime share, the residual of the calibration and the
// warnings of the live radio. A register that no round confirmed is not a
// calibration.
func TestHealthReportsAirtimeWarningsAndResidual(t *testing.T) {
	r := &healthRadio{
		calRadio: newCalRadio(radio.Calibration{State: radio.CalibrationUnsettled, Residual: 11, HasResidual: true}),
		airtime:  radio.Airtime{UsedMS: 101660, BudgetMS: 100000, Share: 1.0166, Limit: true, Deferring: true, OverBudget: true},
		known:    true,
		warnings: []string{"airtime: 101660 ms on air in the last hour, above the budget of 100000 ms (102%)"},
	}
	srv := serveCalDaemon(t, r, "live", nil)
	var health radioHealthResp
	if code := getBounded(t, srv.URL+"/api/health", &health); code != http.StatusOK {
		t.Fatalf("status=%d, want 200", code)
	}
	a := health.Airtime
	if a == nil || a.UsedMs != 101660 || a.BudgetMs != 100000 || a.Share != 1.017 || !a.Limit || !a.Deferring || !a.OverBudget {
		t.Fatalf("airtime=%+v", a)
	}
	if len(health.RadioWarnings) != 1 || !strings.Contains(health.RadioWarnings[0], "airtime") {
		t.Fatalf("radioWarnings=%v", health.RadioWarnings)
	}
	if health.Calibrated == nil || *health.Calibrated {
		t.Fatalf("calibrated=%v, want false for an unsettled register", health.Calibrated)
	}
	if c := health.Calibration; c == nil || c.State != "unsettled" {
		t.Fatalf("calibration=%+v", c)
	}
	if c := health.Calibration; c.Residual == nil || *c.Residual != 11 {
		t.Fatalf("calibration=%+v, want a residual of 11", c)
	}
}

// A radio with no reading and no warning leaves the fields out.
func TestHealthOmitsAirtimeAndWarningsWithoutThem(t *testing.T) {
	r := &healthRadio{calRadio: newCalRadio(radio.Calibration{State: radio.CalibrationStored})}
	srv := serveCalDaemon(t, r, "live", nil)
	var health radioHealthResp
	if code := getBounded(t, srv.URL+"/api/health", &health); code != http.StatusOK {
		t.Fatalf("status=%d, want 200", code)
	}
	if health.Airtime != nil || health.RadioWarnings != nil || (health.Calibration != nil && health.Calibration.Residual != nil) {
		t.Fatalf("airtime=%+v warnings=%v calibration=%+v, want none", health.Airtime, health.RadioWarnings, health.Calibration)
	}
}

// A bridge below the firmware level does not start the radio. The daemon runs on
// a mock radio in dry-run, and health answers 503 with the firmware found, the
// level required and the place of the flash steps.
func TestOldBridgeFirmwareLeavesTheDaemonRunningWithoutARadio(t *testing.T) {
	old := openBridge
	openBridge = func(string) (bridgeLink, error) {
		return nil, &cc1101.FirmwareError{Found: "v2.10", Banner: "CC1101BRIDGE v2.10 (0.3.1)"}
	}
	t.Cleanup(func() { openBridge = old })
	link, mode, fault, retry, err := openRadio(nil, "/dev/null", make([]byte, 8), 0, true, false, bridgeSettings{})
	if err != nil {
		t.Fatalf("openRadio: %v", err)
	}
	if retry {
		t.Fatal("an old firmware is retried: it needs a flash")
	}
	if mode != "mock" {
		t.Fatalf("mode=%q, want mock", mode)
	}
	for _, want := range []string{"v2.10 found", "v2.11 or later", "docs/OPERATION.md"} {
		if !strings.Contains(fault, want) {
			t.Fatalf("fault %q lacks %q", fault, want)
		}
	}

	emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	d := newDaemon(defaultInstall, emu, link, true, mode)
	d.radioFault = fault
	d.ulog = tado.NewUnhandledLogger(io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)
	srv := httptest.NewServer(d.mux())
	t.Cleanup(func() {
		srv.Close()
		cancel()
	})
	var health healthResp
	if code := getBounded(t, srv.URL+"/api/health", &health); code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", code)
	}
	if health.OK || health.RadioError != fault || health.TX || health.TXArmed {
		t.Fatalf("health=%+v, want not ok, the fault, no tx", health)
	}
}

// Any other open error stays fatal for the daemon start.
func TestOpenRadioReportsOtherErrors(t *testing.T) {
	old := openBridge
	openBridge = func(string) (bridgeLink, error) { return nil, errors.New("no such port") }
	t.Cleanup(func() { openBridge = old })
	if _, _, _, _, err := openRadio(nil, "/dev/null", make([]byte, 8), 0, false, false, bridgeSettings{}); err == nil {
		t.Fatal("a port error started the daemon")
	}
}

// Without --tx the live radio starts latched: a call site that skips the dry-run
// check still cannot transmit.
func TestDryRunRadioStartsLatched(t *testing.T) {
	store := newTestStore(t, filepath.Join(t.TempDir(), "state.db"))
	frame := []byte{0x69, 0xec, 0x10, 0x01, 0x0a, 0xce, 0xfa, 0x07, 0xc5, 0x1b, 0x00, 0x09, 0xa1, 0xce, 0xfa, 0x07}
	for _, tc := range []struct {
		autoAck bool
		blocked bool
	}{{false, true}, {true, false}} {
		bridge := newTunableBridge(cc1101.TuneDefault)
		r, err := startSeededTestRadio(store, bridge, defaultInstall.CoordinatorEUI(), 0, tc.autoAck, false)
		if err != nil {
			t.Fatal(err)
		}
		_, err = r.SendImmediate([]tado.Action{{Node: "office", Bytes: frame, CSLTx: true}}, true)
		if blocked := errors.Is(err, radio.ErrTXBlocked); blocked != tc.blocked {
			t.Errorf("autoAck=%t: SendImmediate err=%v, blocked=%t want %t", tc.autoAck, err, blocked, tc.blocked)
		}
		r.Close()
	}
}
