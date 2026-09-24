package daemon

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"freedo/internal/radio"
	"freedo/tado"
)

// boundedClient bounds every HTTP request of the tap and health tests. On the
// default client a budget regression hangs the package until the go test
// timeout instead of failing where it broke.
var boundedClient = &http.Client{Timeout: 5 * time.Second}

type rxTapLine struct {
	Type    string `json:"type"`
	Mode    string `json:"mode"`
	T       string `json:"t"`
	Class   string `json:"class"`
	Hex     string `json:"hex"`
	Dissect string `json:"dissect"`
	Dropped uint64 `json:"dropped"`
}

// newTapServer serves the API on a real http.Server, the way Main does, so a
// test can drain it with Shutdown.
func newTapServer(t *testing.T) (*daemon, *httptest.Server) {
	t.Helper()
	emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	d := newDaemon(defaultInstall, emu, radio.NewMock(nil), true, "mock")
	d.debugAPI = true
	srv := httptest.NewUnstartedServer(d.mux())
	srv.Config.RegisterOnShutdown(d.closeTaps)
	srv.Start()
	t.Cleanup(srv.Close)
	return d, srv
}

// openTap starts the stream and reads the hello line, so the tap is registered
// before the caller injects a frame.
func openTap(t *testing.T, url string) (*bufio.Scanner, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	resp, err := boundedClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		resp.Body.Close()
		cancel()
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/x-ndjson" {
		t.Fatalf("content-type=%q", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("cache-control=%q", got)
	}
	if got := resp.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("x-accel-buffering=%q", got)
	}
	sc := bufio.NewScanner(resp.Body)
	line := readTapLine(t, sc)
	if line.Type != "hello" || line.Mode != "mock" {
		t.Fatalf("hello=%+v", line)
	}
	return sc, cancel
}

func readTapLine(t *testing.T, sc *bufio.Scanner) rxTapLine {
	t.Helper()
	if !sc.Scan() {
		t.Fatalf("stream ended: %v", sc.Err())
	}
	var line rxTapLine
	if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
		t.Fatalf("bad ndjson line %q: %v", sc.Text(), err)
	}
	return line
}

// waitTaps waits for the registry to reach n, which the HTTP goroutine updates.
func waitTaps(t *testing.T, d *daemon, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for d.tapCount() != n {
		if time.Now().After(deadline) {
			t.Fatalf("taps=%d, want %d", d.tapCount(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRxTapStreamsOneFrame(t *testing.T) {
	d, srv := newTapServer(t)
	sc, _ := openTap(t, srv.URL+"/api/debug/rx")
	waitTaps(t, d, 1)

	frame := debugDataFrame(t, 0x44, 55, []byte{0x62, 0x80, 0x02, 0x07, 0xd0})
	d.handleFrame(frame, true)

	line := readTapLine(t, sc)
	if line.Type != "frame" || line.Hex != hex.EncodeToString(frame) {
		t.Fatalf("line=%+v", line)
	}
	if line.Class != "Data" || line.Dissect == "" || line.Dropped != 0 {
		t.Fatalf("line=%+v", line)
	}
	if _, err := time.Parse(time.RFC3339Nano, line.T); err != nil {
		t.Fatalf("t=%q: %v", line.T, err)
	}
}

func TestRxTapWithoutDissect(t *testing.T) {
	d, srv := newTapServer(t)
	sc, _ := openTap(t, srv.URL+"/api/debug/rx?dissect=0")
	waitTaps(t, d, 1)

	frame := debugDataFrame(t, 0x45, 56, nil)
	d.handleFrame(frame, true)

	line := readTapLine(t, sc)
	if line.Hex != hex.EncodeToString(frame) || line.Class != "Data" {
		t.Fatalf("line=%+v", line)
	}
	if line.Dissect != "" {
		t.Fatalf("dissect=%q, want none", line.Dissect)
	}
}

// A client that never reads must not stop the radio path: the frame is counted
// as dropped and handleFrame returns.
func TestRxTapDropsInsteadOfBlocking(t *testing.T) {
	emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	d := newDaemon(defaultInstall, emu, radio.NewMock(nil), true, "mock")
	tap, ok := d.addTap()
	if !ok {
		t.Fatal("the first tap was refused")
	}
	frame := debugDataFrame(t, 0x46, 57, nil)

	done := make(chan struct{})
	go func() {
		for i := 0; i < tapBuffer+1; i++ {
			d.handleFrame(frame, true)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handleFrame blocked on a full tap")
	}
	if got := tap.dropped.Load(); got != 1 {
		t.Fatalf("dropped=%d, want 1", got)
	}
	if len(tap.c) != tapBuffer {
		t.Fatalf("buffered=%d, want %d", len(tap.c), tapBuffer)
	}
}

func TestRxTapCancelUnregisters(t *testing.T) {
	d, srv := newTapServer(t)
	_, cancel := openTap(t, srv.URL+"/api/debug/rx")
	waitTaps(t, d, 1)
	cancel()
	waitTaps(t, d, 0)
}

func TestRxTapLimitsConcurrentClients(t *testing.T) {
	d, srv := newTapServer(t)
	for i := 0; i < maxTaps; i++ {
		openTap(t, srv.URL+"/api/debug/rx")
	}
	waitTaps(t, d, maxTaps)

	resp, err := boundedClient.Get(srv.URL + "/api/debug/rx")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status=%d, want 429", resp.StatusCode)
	}
}

// stalledWriter is a ResponseWriter that answers every write with the deadline
// error a stalled TCP connection reports once SetWriteDeadline expires.
type stalledWriter struct {
	header   http.Header
	deadline time.Time
}

func (w *stalledWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}

func (w *stalledWriter) WriteHeader(int) {}

func (w *stalledWriter) Write([]byte) (int, error) {
	if w.deadline.IsZero() {
		return 0, errors.New("the tap wrote without a deadline")
	}
	return 0, os.ErrDeadlineExceeded
}

func (w *stalledWriter) SetWriteDeadline(t time.Time) error {
	w.deadline = t
	return nil
}

// A client that stops reading must lose its slot instead of holding one of
// maxTaps until TCP gives up: the write deadline ends the handler.
func TestRxTapWriteDeadlineEndsTheHandler(t *testing.T) {
	emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	d := newDaemon(defaultInstall, emu, radio.NewMock(nil), true, "mock")

	w := &stalledWriter{}
	start := time.Now()
	done := make(chan struct{})
	go func() {
		d.getDebugRx(w, httptest.NewRequest(http.MethodGet, "/api/debug/rx", nil))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the tap handler kept its slot on a stalled write")
	}
	if got := d.tapCount(); got != 0 {
		t.Errorf("taps=%d after the handler returned, want 0", got)
	}
	if w.deadline.Before(start) || w.deadline.After(start.Add(tapWriteBudget+time.Second)) {
		t.Errorf("write deadline=%s, want it within %s of %s", w.deadline, tapWriteBudget, start)
	}
}

// An open tap must not hold the HTTP drain: RegisterOnShutdown releases it.
func TestRxTapShutdownReturnsFast(t *testing.T) {
	d, srv := newTapServer(t)
	openTap(t, srv.URL+"/api/debug/rx")
	waitTaps(t, d, 1)

	ctx, cancel := context.WithTimeout(context.Background(), httpDrainBudget)
	defer cancel()
	start := time.Now()
	if err := srv.Config.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("shutdown took %s with one open tap", elapsed)
	}
}
