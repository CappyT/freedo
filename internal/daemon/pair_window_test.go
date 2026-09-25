package daemon

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"freedo/cc1101"
	"freedo/internal/radio"
	"freedo/tado"
)

// pairRadio is a radio whose bridge takes the pair window command. A close
// reports sent Enhanced Beacons.
type pairRadio struct {
	*radio.Mock
	mu      sync.Mutex
	windows []cc1101.PairWindow
	err     error
	sent    int
}

func (r *pairRadio) SetPairWindow(w cc1101.PairWindow) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return 0, r.err
	}
	r.windows = append(r.windows, w)
	if w.Duration == 0 {
		return r.sent, nil
	}
	return 0, nil
}

func (r *pairRadio) snapshot() []cc1101.PairWindow {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]cc1101.PairWindow(nil), r.windows...)
}

func pairRadioDaemon(t *testing.T, rad *pairRadio, dryRun bool) (*daemon, string, *testClock) {
	t.Helper()
	clock := newTestClock(time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC))
	cfg := defaultInstall.EmuConfig()
	cfg.Now = clock.now
	d := newDaemon(defaultInstall, tado.NewEmu(cfg, tado.NewUnhandledLogger(io.Discard)), rad, dryRun, "mock")
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)
	srv := httptest.NewServer(d.mux())
	t.Cleanup(func() {
		srv.Close()
		cancel()
	})
	return d, srv.URL, clock
}

// TestPairWindowArmsBridgeResponder: with an armed transmitter the window
// hands the bridge firmware the pairing key, the coordinator and a block of
// seq and frame counter, so the bridge answers Enhanced Beacon Requests on
// its own. Closing the window closes it on the bridge too.
func TestPairWindowArmsBridgeResponder(t *testing.T) {
	rad := &pairRadio{Mock: radio.NewMock(nil)}
	d, url, _ := pairRadioDaemon(t, rad, false)
	before := txCounter(t, d)

	var view tado.PairWindowView
	if code := postJSON(t, http.MethodPost, url+"/api/pair/window", map[string]any{"seconds": 120}, &view); code != http.StatusOK || !view.Open || !view.Responder {
		t.Fatalf("open window: code=%d %+v", code, view)
	}
	got := rad.snapshot()
	if len(got) != 1 {
		t.Fatalf("bridge windows %+v, want one", got)
	}
	w := got[0]
	if w.Duration != 2*time.Minute || !bytes.Equal(w.Key, tado.PairingKey) ||
		!bytes.Equal(w.Coordinator, defaultInstall.CoordinatorEUI()) || w.Count != pairBeaconBlock || w.Counter != before+1 {
		t.Errorf("bridge window %+v", w)
	}
	if after := txCounter(t, d); after != before+pairBeaconBlock {
		t.Errorf("frame counter %d after the window, want %d: the block is the bridge's", after, before+pairBeaconBlock)
	}

	if code := postJSON(t, http.MethodPost, url+"/api/pair/window", map[string]any{"seconds": 0}, &view); code != http.StatusOK || view.Open || view.Responder {
		t.Fatalf("close window: code=%d %+v", code, view)
	}
	if got := rad.snapshot(); len(got) != 2 || got[1].Duration != 0 {
		t.Fatalf("bridge windows %+v, want a close", got)
	}

	// Firmware before v2.9: the window still opens, the host answers.
	rad.mu.Lock()
	rad.err = cc1101.ErrPairWindowUnsupported
	rad.mu.Unlock()
	if code := postJSON(t, http.MethodPost, url+"/api/pair/window", map[string]any{"seconds": 60}, &view); code != http.StatusOK || !view.Open || view.Responder {
		t.Fatalf("v2.8 bridge: code=%d %+v", code, view)
	}
}

// TestPairWindowDryRunLeavesBridge: a dry-run transmits nothing, so the bridge
// window stays closed.
func TestPairWindowDryRunLeavesBridge(t *testing.T) {
	rad := &pairRadio{Mock: radio.NewMock(nil)}
	_, url, _ := pairRadioDaemon(t, rad, true)
	var view tado.PairWindowView
	if code := postJSON(t, http.MethodPost, url+"/api/pair/window", map[string]any{"seconds": 120}, &view); code != http.StatusOK || !view.Open || view.Responder {
		t.Fatalf("open window: code=%d %+v", code, view)
	}
	if got := rad.snapshot(); len(got) != 0 {
		t.Errorf("dry-run armed the bridge: %+v", got)
	}
}

// TestPairWindowExpiryReportsBeacons: a window that runs out closes on the
// bridge too, and the log gives the Enhanced Beacons the bridge sent in it, as
// an explicit close does. The bridge keeps that count after its own window
// ends, until the next pair window command.
func TestPairWindowExpiryReportsBeacons(t *testing.T) {
	logs := captureLog(t)
	rad := &pairRadio{Mock: radio.NewMock(nil), sent: 3}
	d, url, clock := pairRadioDaemon(t, rad, false)
	var view tado.PairWindowView
	if code := postJSON(t, http.MethodPost, url+"/api/pair/window", map[string]any{"seconds": 60}, &view); code != http.StatusOK || !view.Responder {
		t.Fatalf("open window: code=%d %+v", code, view)
	}
	clock.advance(61 * time.Second)
	deadline := time.Now().Add(3 * time.Second)
	for len(rad.snapshot()) < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	got := rad.snapshot()
	if len(got) != 2 || got[1].Duration != 0 {
		t.Fatalf("bridge windows %+v, want the open and a close at expiry", got)
	}
	// The engine logs before the next command runs.
	if _, err := d.do(func(*tado.EmuState) (any, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if want := "pair window: expired, the bridge sent 3 Enhanced Beacons"; !strings.Contains(logs.String(), want) {
		t.Errorf("log %q, want %q", logs.String(), want)
	}
	time.Sleep(1200 * time.Millisecond)
	if got := rad.snapshot(); len(got) != 2 {
		t.Errorf("bridge windows %+v, want a single close", got)
	}
}

// TestPairDispatchesAfterUnansweredAttempt: the attempt the radio held ends
// without an Enhanced ACK while d/pair waits behind it. d/pair goes at once and
// does not wait for the 30 s retry of that attempt.
func TestPairDispatchesAfterUnansweredAttempt(t *testing.T) {
	now := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	cfg := defaultInstall.EmuConfig()
	cfg.Now = func() time.Time { return now }
	emu := tado.NewEmu(cfg, tado.NewUnhandledLogger(io.Discard))
	rad := newImmediateTestRadio(false, nil)
	rad.cslQueue, rad.accept = true, true
	d := newDaemon(defaultInstall, emu, rad, false, "live")
	clock := newManualRetryClock()
	d.retryAfter, d.retryNow = clock.after, func() time.Time { return now }
	t.Cleanup(func() { close(d.done) })

	if err := emu.EnqueueNetcmd(dhwNode, 1); err != nil {
		t.Fatal(err)
	}
	d.dispatchQueued(emu, dhwNode)
	attempts := rad.queuedAttempts()
	if len(attempts) != 1 || !emu.QueuedDownlinkAwaitingCSL(dhwNode) {
		t.Fatalf("attempts %v, want the netcmd on the radio", attempts)
	}
	if err := emu.EnqueuePair(dhwNode, defaultInstall.NetworkKey()); err != nil {
		t.Fatal(err)
	}
	completion := radio.CSLCompletion{Action: attempts[0], Result: &cc1101.CSLTxResult{Sent: 23}, Err: errors.New("CSLTX got no Enhanced ACK")}
	d.handleCSLCompletion(completion)
	d.chainQueuedDispatch(completion)
	if !d.dispatchDue[dhwNode] {
		t.Fatal("d/pair waits for the retry of the unanswered attempt")
	}
	d.dispatchQueued(emu, dhwNode)
	attempts = rad.queuedAttempts()
	if last := attempts[len(attempts)-1]; !strings.HasPrefix(last.Reason, "downlink pair") {
		t.Errorf("attempt after the completion %q, want d/pair", last.Reason)
	}
}

func txCounter(t *testing.T, d *daemon) uint32 {
	t.Helper()
	v, err := d.do(func(e *tado.EmuState) (any, error) { return e.TxCounter(), nil })
	if err != nil {
		t.Fatal(err)
	}
	return v.(uint32)
}

// TestPairWindowAPI: POST /api/pair/window opens the window, a joining device
// heard under the pairing key shows in GET /api/pair/window, and POST /api/pair
// with no target queues d/pair to it.
func TestPairWindowAPI(t *testing.T) {
	clock := newTestClock(time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC))
	d, url := newHomeDaemon(t, clock)
	joiner := []byte{0x0b, 0xb1, 0xce, 0xfa, 0x07, 0xc5, 0x1b, 0x00}
	target := hex.EncodeToString(joiner)

	var res map[string]any
	if code := postJSON(t, http.MethodPost, url+"/api/pair", map[string]any{}, &res); code != http.StatusBadRequest {
		t.Fatalf("pair with no window: code=%d %v, want 400", code, res)
	}
	if code := postJSON(t, http.MethodPost, url+"/api/pair/window", map[string]any{"seconds": 3600}, &res); code != http.StatusBadRequest {
		t.Fatalf("an hour-long window: code=%d %v, want 400", code, res)
	}
	var view tado.PairWindowView
	if code := postJSON(t, http.MethodPost, url+"/api/pair/window", map[string]any{"seconds": 120}, &view); code != http.StatusOK || !view.Open {
		t.Fatalf("open window: code=%d %+v", code, view)
	}

	rs := []byte{0x7b, 0x3b, 58, 0x02, 133, 0, 0, 0}
	frame, err := tado.BuildBroadcast(tado.PairingKey, 0x13, joiner, 25, 0x04, tado.WrapBroadcastPayload(rs, 0))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.do(func(*tado.EmuState) (any, error) { d.handleFrame(frame, true); return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if code := getJSON(t, url+"/api/pair/window", &view); code != http.StatusOK || len(view.Joiners) != 1 || view.Joiners[0].EUI != target {
		t.Fatalf("window view: code=%d %+v", code, view)
	}

	if code := postJSON(t, http.MethodPost, url+"/api/pair", map[string]any{}, &res); code != http.StatusOK || res["target"] != target {
		t.Fatalf("auto-targeted pair: code=%d %v", code, res)
	}
	if n := pending(t, d, target); n != 1 {
		t.Errorf("joiner holds %d queued writes, want the d/pair", n)
	}
	if code := postJSON(t, http.MethodPost, url+"/api/pair", map[string]any{"target": "b10b"}, &res); code != http.StatusOK || res["target"] != target {
		t.Errorf("pair by the joiner's short address: code=%d %v", code, res)
	}

	if code := postJSON(t, http.MethodPost, url+"/api/pair/window", map[string]any{"seconds": 0}, &view); code != http.StatusOK || view.Open {
		t.Errorf("close window: code=%d %+v", code, view)
	}
	if code := postJSON(t, http.MethodPost, url+"/api/pair", map[string]any{"target": "b10b"}, &res); code != http.StatusNotFound {
		t.Errorf("a joiner of a closed window: code=%d %v, want 404", code, res)
	}
}
