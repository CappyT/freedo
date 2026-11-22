package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"freedo/cc1101"
	"freedo/internal/radio"
	"freedo/tado"
)

// configBridge is a bridge whose configuration the test scripts.
type configBridge struct {
	*tunableLiveBridge
	configureErr error
	configured   atomic.Int32
	closed       atomic.Int32
}

func (b *configBridge) Banner() string { return "CC1101BRIDGE v2.11" }
func (b *configBridge) Baud() int      { return 921600 }

func (b *configBridge) Configure(...cc1101.ConfigureOption) error {
	b.configured.Add(1)
	return b.configureErr
}

func (b *configBridge) Close() error {
	b.closed.Add(1)
	return nil
}

func stubBridge(t *testing.T, br bridgeLink) {
	t.Helper()
	old := openBridge
	openBridge = func(string) (bridgeLink, error) { return br, nil }
	t.Cleanup(func() { openBridge = old })
}

func shortRetry(t *testing.T, every time.Duration) {
	t.Helper()
	old := radioRetryEvery
	radioRetryEvery = every
	t.Cleanup(func() { radioRetryEvery = old })
}

// A chip that cannot be configured, here a synthesizer that does not lock, does
// not stop the daemon. It runs on a mock radio in dry-run, health names the
// cause, and the caller is told to try again.
func TestUnconfiguredBridgeLeavesTheDaemonRunningAndRetries(t *testing.T) {
	br := &configBridge{
		tunableLiveBridge: newTunableBridge(cc1101.TuneDefault),
		configureErr:      errors.Join(errors.New("configure"), cc1101.ErrNoSynthLock),
	}
	stubBridge(t, br)
	link, mode, fault, retry, err := openRadio(nil, "/dev/null", make([]byte, 8), 0, true, false, bridgeSettings{})
	if err != nil {
		t.Fatalf("openRadio: %v", err)
	}
	if mode != "mock" || !retry {
		t.Fatalf("mode=%q retry=%t, want a mock radio and a retry", mode, retry)
	}
	if br.closed.Load() != 1 {
		t.Fatalf("the failed bridge was closed %d times, want 1", br.closed.Load())
	}
	for _, want := range []string{"synthesizer did not lock", "tries again every"} {
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

// The seed write runs SCAL again. A chip that locks for Configure and not for the
// seed must not start live: it would transmit with no lock and no hold. The
// daemon runs on the mock radio and retries, as for a Configure failure.
func TestSeedWithoutSynthLockLeavesTheDaemonRunningAndRetries(t *testing.T) {
	br := &configBridge{tunableLiveBridge: newTunableBridge(cc1101.TuneDefault)}
	br.err = fmt.Errorf("%w: 3 attempts", cc1101.ErrNoSynthLock)
	stubBridge(t, br)
	link, mode, fault, retry, err := openRadio(nil, "/dev/null", make([]byte, 8), 0, true, false, bridgeSettings{})
	if err != nil {
		t.Fatalf("openRadio: %v", err)
	}
	if mode != "mock" || !retry {
		t.Fatalf("mode=%q retry=%t, want a mock radio and a retry", mode, retry)
	}
	if _, ok := link.(*radio.Live); ok {
		t.Fatal("a live radio started on a seed that did not lock")
	}
	if br.closed.Load() != 1 {
		t.Fatalf("the failed bridge was closed %d times, want 1", br.closed.Load())
	}
	for _, want := range []string{"seed FSCTRL0", "synthesizer did not lock", "tries again every"} {
		if !strings.Contains(fault, want) {
			t.Fatalf("fault %q lacks %q", fault, want)
		}
	}
}

// A v2.11 build whose AIRTIME is the old one fails the airtime load of the
// configuration. Only a flash helps, so the daemon runs on the mock radio with a
// radioError that says to flash, and it does not retry.
func TestOldAirtimeFirmwareAsksForAFlashAndDoesNotRetry(t *testing.T) {
	old := &cc1101.FirmwareError{Found: "v2.11", Banner: "CC1101BRIDGE v2.11 (0.4.0)", Contract: "has the old AIRTIME command (no per-minute buckets, no preload)"}
	br := &configBridge{
		tunableLiveBridge: newTunableBridge(cc1101.TuneDefault),
		configureErr:      fmt.Errorf("configure: airtime preload: %w", old),
	}
	stubBridge(t, br)
	_, mode, fault, retry, err := openRadio(nil, "/dev/null", make([]byte, 8), 0, true, false, bridgeSettings{})
	if err != nil {
		t.Fatalf("openRadio: %v", err)
	}
	if mode != "mock" || retry {
		t.Fatalf("mode=%q retry=%t, want a mock radio and no retry", mode, retry)
	}
	for _, want := range []string{"AIRTIME", "flash firmware/cc1101_bridge"} {
		if !strings.Contains(fault, want) {
			t.Fatalf("fault %q lacks %q", fault, want)
		}
	}
	if strings.Contains(fault, "tries again") {
		t.Fatalf("fault %q promises a retry", fault)
	}
	if br.closed.Load() != 1 {
		t.Fatalf("the bridge was closed %d times, want 1", br.closed.Load())
	}
}

// The retry opens and configures the bridge again every period. The fault of
// health follows the last cause, and a try that works asks main to restart.
func TestRetryRadioRestartsWhenTheBridgeConfigures(t *testing.T) {
	shortRetry(t, time.Millisecond)
	d := newDaemon(defaultInstall, nil, nil, true, "mock")
	d.radioFault = "start"
	var calls atomic.Int32
	probe := func() error {
		switch calls.Add(1) {
		case 1:
			return errors.New("no such port")
		case 2:
			return errors.Join(errRadioConfigure, cc1101.ErrNoSynthLock)
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		d.retryRadio(ctx, probe)
		close(done)
	}()
	select {
	case <-d.radioReady:
	case <-time.After(5 * time.Second):
		t.Fatalf("no restart after %d tries", calls.Load())
	}
	<-done
	if calls.Load() != 3 {
		t.Fatalf("tries=%d, want 3", calls.Load())
	}
	if !strings.Contains(d.fault(), "synthesizer did not lock") {
		t.Fatalf("fault=%q, want the cause of the last failed try", d.fault())
	}
}

// A bridge that turns up with old firmware stops the retry: it needs a flash.
func TestRetryRadioStopsOnOldFirmware(t *testing.T) {
	shortRetry(t, time.Millisecond)
	d := newDaemon(defaultInstall, nil, nil, true, "mock")
	var calls atomic.Int32
	probe := func() error {
		calls.Add(1)
		return &cc1101.FirmwareError{Found: "v2.10", Banner: "CC1101BRIDGE v2.10 (0.3.1)"}
	}
	done := make(chan struct{})
	go func() {
		d.retryRadio(context.Background(), probe)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the retry goes on with an old firmware")
	}
	if calls.Load() != 1 || !strings.Contains(d.fault(), "v2.10 found") {
		t.Fatalf("tries=%d fault=%q", calls.Load(), d.fault())
	}
	select {
	case <-d.radioReady:
		t.Fatal("an old firmware asked for a restart")
	default:
	}
}

// The retry ends with the daemon.
func TestRetryRadioStopsWithTheContext(t *testing.T) {
	shortRetry(t, time.Hour)
	d := newDaemon(defaultInstall, nil, nil, true, "mock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		d.retryRadio(ctx, func() error {
			t.Error("probe ran")
			return nil
		})
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the retry outlives its context")
	}
}

// The probe is the whole start check: it opens, configures with the airtime of
// the store, writes the calibration seed with its lock check, and releases the
// port.
func TestProbeBridgeConfiguresAndCloses(t *testing.T) {
	br := &configBridge{tunableLiveBridge: newTunableBridge(cc1101.TuneDefault)}
	stubBridge(t, br)
	if err := probeBridge(nil, "/dev/null", bridgeSettings{}); err != nil {
		t.Fatal(err)
	}
	if br.configured.Load() != 1 || br.closed.Load() != 1 {
		t.Fatalf("configured=%d closed=%d, want 1 and 1", br.configured.Load(), br.closed.Load())
	}
	if !bytes.Equal(br.writes, []byte{cc1101.TuneDefault}) {
		t.Fatalf("the probe wrote FSCTRL0 %x, want the seed %x", br.writes, cc1101.TuneDefault)
	}
	br.configureErr = cc1101.ErrNoSynthLock
	err := probeBridge(nil, "/dev/null", bridgeSettings{})
	if !errors.Is(err, errRadioConfigure) || !errors.Is(err, cc1101.ErrNoSynthLock) || br.closed.Load() != 2 {
		t.Fatalf("err=%v closed=%d", err, br.closed.Load())
	}
}

// The retry probe runs the whole start check. A seed whose SCAL does not lock
// fails the probe, so the daemon does not re-execute at each retry period for as
// long as the seed fails.
func TestRetryProbeFailsOnASeedThatDoesNotLock(t *testing.T) {
	br := &configBridge{tunableLiveBridge: newTunableBridge(cc1101.TuneDefault)}
	br.err = fmt.Errorf("%w: 3 attempts", cc1101.ErrNoSynthLock)
	stubBridge(t, br)
	if err := probeBridge(nil, "/dev/null", bridgeSettings{}); !errors.Is(err, cc1101.ErrNoSynthLock) {
		t.Fatalf("probe = %v, want the seed lock error", err)
	}
	shortRetry(t, 10*time.Millisecond)
	d := &daemon{radioReady: make(chan struct{}, 1)}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	d.retryRadio(ctx, func() error { return probeBridge(nil, "/dev/null", bridgeSettings{}) })
	select {
	case <-d.radioReady:
		t.Fatal("the daemon asked for a restart on a probe whose seed does not lock")
	default:
	}
	if f := d.fault(); !strings.Contains(f, "seed FSCTRL0") {
		t.Fatalf("fault %q does not name the seed", f)
	}
}

// A seed write that fails on a link fault leaves the register changed with no
// calibration result. The radio does not start live: it runs on the mock radio
// and retries.
func TestSeedLinkFaultDoesNotStartTheLiveRadio(t *testing.T) {
	br := &configBridge{tunableLiveBridge: newTunableBridge(cc1101.TuneDefault)}
	br.err = fmt.Errorf("%w: deadline exceeded", cc1101.ErrLinkFault)
	stubBridge(t, br)
	link, mode, fault, retry, err := openRadio(nil, "/dev/null", make([]byte, 8), 0, true, false, bridgeSettings{})
	if err != nil {
		t.Fatalf("openRadio: %v", err)
	}
	if _, live := link.(*radio.Live); live || mode != "mock" || !retry {
		t.Fatalf("mode=%q retry=%t live=%t, want a mock radio and a retry", mode, retry, live)
	}
	if !strings.Contains(fault, "seed FSCTRL0") {
		t.Fatalf("fault %q does not name the seed", fault)
	}
	if br.closed.Load() != 1 {
		t.Fatalf("the failed bridge was closed %d times, want 1", br.closed.Load())
	}
}
