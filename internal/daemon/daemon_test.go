package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"freedo/cc1101"
	"freedo/internal/cli"
	"freedo/internal/install"
	"freedo/internal/radio"
	"freedo/tado"
)

type orderedLiveBridge struct {
	mu           sync.Mutex
	result       *cc1101.AutoAckResult
	recv         int
	events       []string
	trains       [][][]byte
	cslFrames    [][]byte
	cslBootstrap []bool
	cslFollow    []bool
	cslAcks      []*cc1101.RxFrame
	cslAutoAck   bool
	cslErr       error
	cslResult    *cc1101.CSLTxResult
	resultRepeat int
	bursts       [][]byte
	burstSet     bool
	burstSent    int
	burstErr     error
	closed       bool
	entered      chan struct{}
	release      <-chan struct{}
}

type queuedCSLCall struct {
	id        uint16
	frame     []byte
	bootstrap bool
	follow    bool
}

type queuedLiveBridge struct {
	*orderedLiveBridge
	mu          sync.Mutex
	queued      []queuedCSLCall
	batches     []int
	events      []*cc1101.CSLQueueEvent
	completions []*cc1101.CSLQueueCompletion
	flushes     int
	enqueueErr  error
}

type blockingQueuedLiveBridge struct {
	*queuedLiveBridge
	entered chan struct{}
	unblock chan struct{}
	once    sync.Once
}

func (b *blockingQueuedLiveBridge) PollCSLQueue() (*cc1101.CSLQueueEvent, error) {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-b.unblock
	return nil, errors.New("bridge closed")
}

func (b *blockingQueuedLiveBridge) PollCSLQueueWithin(timeout time.Duration) (*cc1101.CSLQueueEvent, error) {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	select {
	case <-b.unblock:
		return nil, errors.New("bridge closed")
	case <-time.After(timeout):
		return nil, cc1101.ErrCSLPollTimeout
	}
}

func (b *blockingQueuedLiveBridge) Close() error {
	b.once.Do(func() { close(b.unblock) })
	return b.orderedLiveBridge.Close()
}

func (b *queuedLiveBridge) EnqueueCSLBatch(jobs []cc1101.CSLQueueRequest) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.enqueueErr != nil {
		return b.enqueueErr
	}
	b.batches = append(b.batches, len(jobs))
	for _, job := range jobs {
		b.queued = append(b.queued, queuedCSLCall{id: job.ID, frame: append([]byte(nil), job.Frame...), bootstrap: job.Bootstrap, follow: job.WaitResponse})
	}
	return nil
}

func (b *queuedLiveBridge) PollCSLQueue() (*cc1101.CSLQueueEvent, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.events) != 0 {
		event := b.events[0]
		b.events = b.events[1:]
		return event, nil
	}
	if len(b.completions) == 0 {
		return nil, nil
	}
	completion := b.completions[0]
	b.completions = b.completions[1:]
	return &cc1101.CSLQueueEvent{Completion: completion}, nil
}

func (b *queuedLiveBridge) FlushCSLQueue() error {
	b.mu.Lock()
	b.flushes++
	b.mu.Unlock()
	return nil
}

func (b *queuedLiveBridge) snapshotQueue() ([]queuedCSLCall, []int, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]queuedCSLCall(nil), b.queued...), append([]int(nil), b.batches...), b.flushes
}

type endingQueuedLiveBridge struct {
	*queuedLiveBridge
	err error
}

type failingImmediateRadio struct {
	actions []tado.Action
}

func (r *failingImmediateRadio) Frames() <-chan []byte { return nil }
func (r *failingImmediateRadio) Send([]byte) error     { return nil }
func (r *failingImmediateRadio) Close() error          { return nil }
func (r *failingImmediateRadio) SendImmediate(actions []tado.Action, _ bool) (radio.ImmediateResult, error) {
	r.actions = append([]tado.Action(nil), actions...)
	return radio.ImmediateResult{}, errors.New("first CSL transaction failed")
}

// completionRadio hands the engine the CSL completions a test sends it. It is a
// live radio to the engine, so the completion frames take the live path.
type completionRadio struct {
	frames  chan []byte
	cslDone chan radio.CSLCompletion
}

func (r *completionRadio) Frames() <-chan []byte                      { return r.frames }
func (r *completionRadio) Send([]byte) error                          { return nil }
func (r *completionRadio) Close() error                               { return nil }
func (r *completionRadio) CSLCompletions() <-chan radio.CSLCompletion { return r.cslDone }
func (r *completionRadio) SendImmediate([]tado.Action, bool) (radio.ImmediateResult, error) {
	return radio.ImmediateResult{}, errors.New("completionRadio does not transmit")
}

type immediateTestRadio struct {
	frames   chan []byte
	mu       sync.Mutex
	batches  [][]tado.Action
	submit   bool
	accept   bool
	cslQueue bool
	err      error
}

func newImmediateTestRadio(submit bool, err error) *immediateTestRadio {
	return &immediateTestRadio{frames: make(chan []byte, 1), submit: submit, err: err}
}

func (r *immediateTestRadio) Frames() <-chan []byte { return r.frames }
func (r *immediateTestRadio) Send([]byte) error     { return nil }
func (r *immediateTestRadio) Close() error          { return nil }
func (r *immediateTestRadio) SendImmediate(actions []tado.Action, _ bool) (radio.ImmediateResult, error) {
	r.mu.Lock()
	r.batches = append(r.batches, append([]tado.Action(nil), actions...))
	r.mu.Unlock()
	var result radio.ImmediateResult
	if r.submit {
		result.Submitted = append([]tado.Action(nil), actions...)
	}
	if r.accept {
		result.Accepted = append([]tado.Action(nil), actions...)
	}
	result.CSLQueue = r.cslQueue
	return result, r.err
}

func (r *immediateTestRadio) queuedAttempts() []tado.Action {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []tado.Action
	for _, batch := range r.batches {
		for _, action := range batch {
			if action.QueueAttempt {
				out = append(out, action)
			}
		}
	}
	return out
}

type manualRetryClock struct {
	started chan time.Duration
	tick    chan time.Time
}

type distinctRetryTimer struct {
	delay time.Duration
	tick  chan time.Time
}

type distinctRetryClock struct{ timers chan distinctRetryTimer }

func newDistinctRetryClock() *distinctRetryClock {
	return &distinctRetryClock{timers: make(chan distinctRetryTimer, 8)}
}

func (c *distinctRetryClock) after(delay time.Duration) <-chan time.Time {
	timer := distinctRetryTimer{delay: delay, tick: make(chan time.Time, 1)}
	c.timers <- timer
	return timer.tick
}

func (c *distinctRetryClock) wait(t *testing.T) distinctRetryTimer {
	t.Helper()
	select {
	case timer := <-c.timers:
		return timer
	case <-time.After(time.Second):
		t.Fatal("retry timer was not armed")
		return distinctRetryTimer{}
	}
}

func newManualRetryClock() *manualRetryClock {
	return &manualRetryClock{started: make(chan time.Duration, 8), tick: make(chan time.Time, 8)}
}

func (c *manualRetryClock) after(delay time.Duration) <-chan time.Time {
	c.started <- delay
	return c.tick
}

func (c *manualRetryClock) wait(t *testing.T) time.Duration {
	t.Helper()
	select {
	case delay := <-c.started:
		return delay
	case <-time.After(time.Second):
		t.Fatal("retry timer was not scheduled")
	}
	return 0
}

func (c *manualRetryClock) noTimer(t *testing.T) {
	t.Helper()
	select {
	case delay := <-c.started:
		t.Fatalf("unexpected retry timer=%s", delay)
	case <-time.After(50 * time.Millisecond):
	}
}

type indexedRetryClock struct {
	mu      sync.Mutex
	started chan time.Duration
	timers  []chan time.Time
}

func newIndexedRetryClock() *indexedRetryClock {
	return &indexedRetryClock{started: make(chan time.Duration, 8)}
}

func (c *indexedRetryClock) after(delay time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.mu.Lock()
	c.timers = append(c.timers, ch)
	c.mu.Unlock()
	c.started <- delay
	return ch
}

func (c *indexedRetryClock) wait(t *testing.T) time.Duration {
	t.Helper()
	select {
	case delay := <-c.started:
		return delay
	case <-time.After(time.Second):
		t.Fatal("retry timer was not scheduled")
	}
	return 0
}

func (c *indexedRetryClock) tick(t *testing.T, index int, at time.Time) {
	t.Helper()
	c.mu.Lock()
	if index >= len(c.timers) {
		c.mu.Unlock()
		t.Fatalf("timer %d was not scheduled", index)
	}
	ch := c.timers[index]
	c.mu.Unlock()
	ch <- at
}

func (b *endingQueuedLiveBridge) PollCSLQueue() (*cc1101.CSLQueueEvent, error) {
	queued, _, _ := b.snapshotQueue()
	if len(queued) == 0 {
		return nil, nil
	}
	return nil, b.err
}

func (b *orderedLiveBridge) RecvAutoAck(time.Duration, []byte, uint16) (*cc1101.AutoAckResult, error) {
	b.mu.Lock()
	b.events = append(b.events, "recv")
	b.recv++
	result := b.result
	seen := b.recv
	repeat := b.resultRepeat
	entered, release := b.entered, b.release
	b.mu.Unlock()
	if seen == 1 {
		if entered != nil {
			entered <- struct{}{}
		}
		if release != nil {
			<-release
		}
		return result, nil
	}
	if seen <= repeat {
		return result, nil
	}
	time.Sleep(time.Millisecond)
	return &cc1101.AutoAckResult{Status: cc1101.AutoAckNoFrame}, nil
}

func (b *orderedLiveBridge) RecvFW(time.Duration) (*cc1101.RxFrame, error) {
	b.mu.Lock()
	b.events = append(b.events, "recv-fw")
	b.mu.Unlock()
	time.Sleep(time.Millisecond)
	return nil, nil
}

func (b *orderedLiveBridge) SendTrain(frames [][]byte, _ time.Duration) (int, error) {
	copy := copyFrames(frames)
	b.mu.Lock()
	b.events = append(b.events, "train")
	b.trains = append(b.trains, copy)
	b.mu.Unlock()
	return len(frames), nil
}

func (b *orderedLiveBridge) SendCSLData(frame, _ []byte, _ time.Duration, _ time.Duration, bootstrap, follow bool, _ uint16) (*cc1101.CSLTxResult, error) {
	b.mu.Lock()
	b.events = append(b.events, "csl")
	b.cslFrames = append(b.cslFrames, append([]byte(nil), frame...))
	b.cslBootstrap = append(b.cslBootstrap, bootstrap)
	b.cslFollow = append(b.cslFollow, follow)
	err := b.cslErr
	result := b.cslResult
	var ack *cc1101.RxFrame
	if len(b.cslAcks) > 0 {
		ack = b.cslAcks[0]
		b.cslAcks = b.cslAcks[1:]
	} else if b.cslAutoAck && len(frame) >= 16 {
		dst := append(append([]byte(nil), frame[11:16]...), 0xc5, 0x1b, 0x00)
		ack = &cc1101.RxFrame{Payload: tado.BuildEnhAck(frame[2], dst, frame[3:11], 0), CRCOK: true}
	}
	b.mu.Unlock()
	if err != nil {
		return result, err
	}
	if result != nil {
		return result, nil
	}
	return &cc1101.CSLTxResult{Sent: 23, Ack: ack}, nil
}

func (b *orderedLiveBridge) SendRouterAdvertBurst(frame []byte, _ time.Duration) (int, error) {
	b.mu.Lock()
	b.events = append(b.events, "ra-burst")
	b.bursts = append(b.bursts, append([]byte(nil), frame...))
	sent, err := b.burstSent, b.burstErr
	if !b.burstSet {
		sent = 200
	}
	b.mu.Unlock()
	return sent, err
}

func (b *orderedLiveBridge) Close() error {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	return nil
}

func (b *orderedLiveBridge) snapshot() ([]string, [][][]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	events := append([]string(nil), b.events...)
	trains := make([][][]byte, len(b.trains))
	for i := range b.trains {
		trains[i] = copyFrames(b.trains[i])
	}
	return events, trains, b.closed
}

func (b *orderedLiveBridge) burstSnapshot() [][]byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return copyFrames(b.bursts)
}

func (b *orderedLiveBridge) cslSnapshot() [][]byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return copyFrames(b.cslFrames)
}

func (b *orderedLiveBridge) bootstrapSnapshot() []bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]bool(nil), b.cslBootstrap...)
}

func copyFrames(frames [][]byte) [][]byte {
	out := make([][]byte, len(frames))
	for i := range frames {
		out[i] = append([]byte(nil), frames[i]...)
	}
	return out
}

// loadFrames reads the first n on-air frames of a capture file.
func loadFrames(t *testing.T, path string, n int) [][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), 1<<16)
	var out [][]byte
	for sc.Scan() && len(out) < n {
		h, ok := replayHex(sc.Text())
		if !ok {
			continue
		}
		fr, err := hex.DecodeString(h)
		if err != nil || len(fr) < 2 {
			continue
		}
		out = append(out, fr)
	}
	return out
}

func getJSON(t *testing.T, url string, v any) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil && err != io.EOF {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp.StatusCode
}

func postJSON(t *testing.T, method, url string, body, v any) int {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(method, url, bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil && err != io.EOF {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp.StatusCode
}

func newImmediateDaemon(t *testing.T, cfg tado.EmuConfig, radio radio.Radio, dryRun bool) (*daemon, string) {
	return newImmediateDaemonWithRetry(t, cfg, radio, dryRun, nil)
}

// newImmediateDaemonWithRetry starts a daemon on a test radio. setup runs
// before the engine starts: the engine reads the clock fields from its own
// goroutine, so a test must not set them afterwards.
func newImmediateDaemonWithRetry(t *testing.T, cfg tado.EmuConfig, radio radio.Radio, dryRun bool, retryAfter func(time.Duration) <-chan time.Time, setup ...func(*daemon)) (*daemon, string) {
	t.Helper()
	emu := tado.NewEmu(cfg, tado.NewUnhandledLogger(io.Discard))
	d := newDaemon(defaultInstall, emu, radio, dryRun, "live")
	if retryAfter != nil {
		d.retryAfter = retryAfter
	}
	for _, fn := range setup {
		fn(d)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)
	srv := httptest.NewServer(d.mux())
	t.Cleanup(func() {
		srv.Close()
		cancel()
		_ = radio.Close()
	})
	return d, srv.URL
}

// fixedRetryClock makes the daemon read its retry clock from *now and draw no
// retry jitter. The test moves *now only before it hands the engine a tick.
func fixedRetryClock(now *time.Time) func(*daemon) {
	return func(d *daemon) {
		d.retryNow = func() time.Time { return *now }
		d.retryRand = func() uint16 { return 0 }
	}
}

func waitForQueuedAttempts(t *testing.T, radio *immediateTestRadio, want int) []tado.Action {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if got := radio.queuedAttempts(); len(got) >= want {
			return got
		}
		time.Sleep(time.Millisecond)
	}
	return radio.queuedAttempts()
}

func queuedCoAP(t *testing.T, raw []byte) *tado.CoAPMessage {
	t.Helper()
	frame, err := tado.ParseData(defaultInstall.NetworkKey(), raw)
	if err != nil {
		t.Fatal(err)
	}
	app := tado.ParseApp(frame.Plaintext)
	if app == nil || app.CoAP == nil {
		t.Fatalf("queued frame has no CoAP: %x", raw)
	}
	return app.CoAP
}

type statsResp struct {
	Stats     tado.EmuStats  `json:"stats"`
	Nodes     int            `json:"nodes"`
	Unhandled map[string]int `json:"unhandled"`
}

const daemonNoAutoQueueHelper = "FREEDO_TEST_DAEMON_NO_AUTO_QUEUE_HELPER"
const daemonZoneFanOutHelper = "FREEDO_TEST_DAEMON_ZONE_FAN_OUT_HELPER"

// bootstrapYAML is the whole config file the subprocess helpers get: the
// installation is in the database, and every other setting is on their flags.
const bootstrapYAML = "version: 2\n"

// daemonHelperArgs reads the argument list of a subprocess helper. An unset
// variable is the no-argument start, not a single empty argument.
func daemonHelperArgs() []string {
	raw := os.Getenv("FREEDO_TEST_DAEMON_ARGS")
	if raw == "" {
		return []string{}
	}
	return strings.Split(raw, "\n")
}

// seedInstall writes the bootstrap file and a database holding in, the way an
// operator seeds one before the first start.
func seedInstall(t *testing.T, dir string, in *tado.Install) (config, state string) {
	t.Helper()
	config = filepath.Join(dir, "freedo.yaml")
	state = filepath.Join(dir, "state.db")
	if err := os.WriteFile(config, []byte(bootstrapYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := install.WriteInstall(state, in); err != nil {
		t.Fatalf("seed %s: %v", state, err)
	}
	return config, state
}

func TestDaemonBroadcastCounterCLISeed(t *testing.T) {
	for _, tt := range []struct {
		name      string
		tx        uint64
		broadcast uint64
		want      uint32
		wantErr   bool
	}{
		{name: "txctr alone", tx: 65535, want: 65535},
		{name: "bcastctr is a floor", tx: 7, broadcast: 65535, want: 65535},
		{name: "txctr above bcastctr", tx: 65535, broadcast: 7, want: 65535},
		{name: "tx overflow", tx: uint64(^uint32(0)) + 1, wantErr: true},
		{name: "broadcast overflow", broadcast: uint64(^uint32(0)) + 1, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := daemonCounterSeed(tt.tx, tt.broadcast)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error=%v, wantErr=%v", err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Errorf("seed=%d, want %d", got, tt.want)
			}
		})
	}
}

func TestDaemonNoAutoQueueCLI(t *testing.T) {
	if os.Getenv(daemonNoAutoQueueHelper) == "1" {
		Main(daemonHelperArgs())
		return
	}

	bedroom, err := defaultInstall.Resolve("bedroom")
	if err != nil {
		t.Fatal(err)
	}
	pkt, err := tado.BuildCoAP(tado.CoAPReq{
		Type: "NON", Code: "0.03", MID: 0x6110, Token: []byte{0x51},
		Path: []string{"h", "7654321", "z", "10", "s"}, Query: []string{"id=10"},
		Payload: tado.TLV{Tag: "6280", Val: []byte{0x08, 0x34}}.Encode(),
		SrcEUI:  bedroom.WireEUI(), DstEUI: defaultInstall.CoordinatorEUI(),
		SrcPort: 5683, DstPort: 4005,
	})
	if err != nil {
		t.Fatal(err)
	}
	frame, err := tado.BuildData(defaultInstall.NetworkKey(), 0x21, defaultInstall.CoordinatorEUI(),
		bedroom.WireEUI()[:5], tado.WrapAppPayload(pkt, 700, nil))
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	config, state := seedInstall(t, dir, defaultInstall)
	replay := filepath.Join(dir, "utc.log")
	unhandled := filepath.Join(dir, "unhandled.log")
	if err := os.WriteFile(replay, []byte(frame.Hex()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	args := []string{
		"--config", config, "--replay", replay, "--state", state, "--unhandled", unhandled, "--addr", addr,
		"--no-auto-queue", "--duration", "500ms",
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestDaemonNoAutoQueueCLI$")
	cmd.Env = append(os.Environ(), daemonNoAutoQueueHelper+"=1", "FREEDO_TEST_DAEMON_ARGS="+strings.Join(args, "\n"))
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	seenFrame := false
	deadline := time.Now().Add(time.Second)
	client := &http.Client{Timeout: 20 * time.Millisecond}
	for time.Now().Before(deadline) {
		var stats statsResp
		resp, err := client.Get("http://" + addr + "/api/stats")
		if err == nil {
			err = json.NewDecoder(resp.Body).Decode(&stats)
			resp.Body.Close()
		}
		if err == nil && stats.Stats.Frames == 1 {
			seenFrame = true
			var queue struct {
				Pending int `json:"pending"`
			}
			if code := getJSON(t, "http://"+addr+"/api/queue/"+bedroom.Serial, &queue); code != http.StatusOK {
				t.Errorf("queue: got %d", code)
			} else if queue.Pending != 0 {
				t.Errorf("CLI --no-auto-queue left %d automatic downlinks", queue.Pending)
			}
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("daemon helper: %v\n%s", err, output.String())
	}
	if !seenFrame {
		t.Fatalf("daemon helper did not replay the UTC uplink\n%s", output.String())
	}
}

func TestDaemonRelaysZoneTrafficWithoutFanOutCLI(t *testing.T) {
	if os.Getenv(daemonZoneFanOutHelper) == "1" {
		Main(daemonHelperArgs())
		return
	}

	var relay []byte
	for _, raw := range loadFrames(t, "../../tado/testdata/frames_69ec.txt", 10000) {
		probe := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
		probe.OnFrame(raw)
		if probe.Stats().ZoneRelays == 1 && probe.Pending("a70a") == 1 {
			relay = raw
			break
		}
	}
	if relay == nil {
		t.Fatal("capture has no zone relay to a70a")
	}

	sender, err := defaultInstall.Resolve("a803")
	if err != nil {
		t.Fatal(err)
	}
	put, err := tado.BuildContextCoAP(tado.CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x9d07, Token: []byte{0x13},
		Path: []string{"z", "s"}, Query: []string{"lid=8"},
		Payload: tado.TLV{Tag: "40a0", Val: []byte{60}}.Encode(),
		SrcEUI:  sender.WireEUI(), DstEUI: defaultInstall.CoordinatorEUI(),
	})
	if err != nil {
		t.Fatal(err)
	}
	fanout, err := tado.BuildData(defaultInstall.NetworkKey(), 0x25, defaultInstall.CoordinatorEUI(),
		sender.WireEUI()[:5], tado.WrapAppPayload(put, 701, nil))
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	config, state := seedInstall(t, dir, defaultInstall)
	replay := filepath.Join(dir, "zone.log")
	unhandled := filepath.Join(dir, "unhandled.log")
	if err := os.WriteFile(replay, []byte(hex.EncodeToString(relay)+"\n"+fanout.Hex()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	args := []string{
		"--config", config, "--replay", replay, "--state", state, "--unhandled", unhandled, "--addr", addr,
		"--duration", "500ms",
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestDaemonRelaysZoneTrafficWithoutFanOutCLI$")
	cmd.Env = append(os.Environ(), daemonZoneFanOutHelper+"=1", "FREEDO_TEST_DAEMON_ARGS="+strings.Join(args, "\n"))
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	seenFrames := false
	deadline := time.Now().Add(time.Second)
	client := &http.Client{Timeout: 20 * time.Millisecond}
	for time.Now().Before(deadline) {
		var stats statsResp
		resp, err := client.Get("http://" + addr + "/api/stats")
		if err == nil {
			err = json.NewDecoder(resp.Body).Decode(&stats)
			resp.Body.Close()
		}
		if err == nil && stats.Stats.Frames == 2 {
			seenFrames = true
			if stats.Stats.ZoneRelays != 1 || stats.Stats.ZoneFanouts != 0 {
				t.Errorf("zone stats=%+v, want one relay and no fanout", stats.Stats)
			}
			if stats.Stats.DownlinksSent != 0 {
				t.Errorf("replay sent %d downlinks, want 0", stats.Stats.DownlinksSent)
			}
			var queue struct {
				Pending int `json:"pending"`
			}
			if code := getJSON(t, "http://"+addr+"/api/queue/a70a", &queue); code != http.StatusOK {
				t.Errorf("relay queue: got %d", code)
			} else if queue.Pending != 1 {
				t.Errorf("relay queue pending=%d, want 1", queue.Pending)
			}
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("daemon helper: %v\n%s", err, output.String())
	}
	if !seenFrames {
		t.Fatalf("daemon helper did not replay zone frames\n%s", output.String())
	}
}

func TestZoneRelayResponseRearmsAtCSLNoAckCompletion(t *testing.T) {
	now := time.Date(2026, 8, 24, 19, 8, 55, 0, time.UTC)
	cfg := defaultInstall.EmuConfig()
	cfg.Now = func() time.Time { return now }
	emu := tado.NewEmu(cfg, tado.NewUnhandledLogger(io.Discard))
	sender, err := defaultInstall.Resolve("0a01")
	if err != nil {
		t.Fatal(err)
	}
	ru, err := defaultInstall.Resolve(dhwNode)
	if err != nil {
		t.Fatal(err)
	}
	response, err := tado.BuildContextCoAP(tado.CoAPReq{
		Type: "NON", Code: "2.05", MID: 0x1e04, Token: []byte{0x1e},
		Payload: tado.TLV{Tag: "40a0", Val: []byte{56}}.Encode(),
		SrcEUI:  sender.WireEUI(), DstEUI: ru.WireEUI(),
	})
	if err != nil {
		t.Fatal(err)
	}
	iid := append([]byte(nil), ru.WireEUI()...)
	for i := range iid[:4] {
		iid[i], iid[len(iid)-1-i] = iid[len(iid)-1-i], iid[i]
	}
	iid[0] ^= 0x02
	peerPacket := append([]byte{0x7e, 0xf5, 0x00}, iid...)
	peerPacket = append(peerPacket, response[3:]...)
	uplink, err := tado.BuildData(defaultInstall.NetworkKey(), 0x30, defaultInstall.CoordinatorEUI(), sender.WireEUI()[:5], tado.WrapAppPayload(peerPacket, 800, nil))
	if err != nil {
		t.Fatal(err)
	}
	emu.OnFrame(uplink.Bytes())
	first, ok, err := emu.NextQueuedDownlink(dhwNode)
	if err != nil || !ok || len(first) != 1 || !first[0].ResponseRetry || first[0].CoAPTransaction {
		t.Fatalf("first=%+v ok=%t err=%v", first, ok, err)
	}

	immediate := newImmediateTestRadio(true, nil)
	d := newDaemon(defaultInstall, emu, immediate, false, "live")
	clock := newManualRetryClock()
	d.retryAfter, d.retryNow = clock.after, func() time.Time { return now }
	t.Cleanup(func() { close(d.done) })
	d.handleCSLCompletion(radio.CSLCompletion{Action: first[0], Result: &cc1101.CSLTxResult{Sent: 23}})
	clock.noTimer(t)
	if len(d.dispatchQ) != 1 || !d.dispatchDue[dhwNode] {
		t.Fatalf("retry dispatch due=%v queue=%v", d.dispatchDue, d.dispatchQ)
	}
	d.dispatchQ = d.dispatchQ[1:]
	delete(d.dispatchDue, dhwNode)
	d.dispatchQueued(emu, dhwNode)
	attempts := immediate.queuedAttempts()
	if len(attempts) != 1 || !attempts[0].ResponseRetry {
		t.Fatalf("retry attempts=%+v", attempts)
	}
	firstData, err := tado.ParseData(defaultInstall.NetworkKey(), first[0].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	secondData, err := tado.ParseData(defaultInstall.NetworkKey(), attempts[0].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if secondData.Seq != firstData.Seq {
		t.Fatalf("relay MAC seq=%02x, want %02x", secondData.Seq, firstData.Seq)
	}
	if bytes.Equal(secondData.Plaintext[4:8], firstData.Plaintext[4:8]) {
		t.Fatal("relay retry reused the anti-replay counter")
	}
}

func deviceRequestUplink(t *testing.T, dev *tado.Device, req tado.CoAPReq, seq byte) []byte {
	t.Helper()
	req.SrcEUI, req.DstEUI = dev.WireEUI(), defaultInstall.CoordinatorEUI()
	req.SrcPort, req.DstPort = 5683, 4005
	pkt, err := tado.BuildCoAP(req)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := tado.BuildData(defaultInstall.NetworkKey(), seq, defaultInstall.CoordinatorEUI(), dev.WireEUI()[:5], tado.WrapAppPayload(pkt, 900+uint32(seq), nil))
	if err != nil {
		t.Fatal(err)
	}
	return frame.Bytes()
}

func authTokenRequest(mid uint16) tado.CoAPReq {
	return tado.CoAPReq{Type: "CON", Code: "0.02", MID: mid, Token: []byte{byte(mid)}, Path: []string{"auth", "token"}}
}

func TestAuthReplyBootstrapsAndRetriesAfterMissedAck(t *testing.T) {
	now := time.Date(2026, 8, 20, 13, 27, 47, 0, time.UTC)
	cfg := defaultInstall.EmuConfig()
	cfg.Now = func() time.Time { return now }
	emu := tado.NewEmu(cfg, tado.NewUnhandledLogger(io.Discard))
	dev, err := defaultInstall.Resolve("0a01")
	if err != nil {
		t.Fatal(err)
	}
	var reply tado.Action
	for _, action := range emu.OnFrame(deviceRequestUplink(t, dev, authTokenRequest(0x2b8c), 0x40)) {
		if action.QueueAttempt {
			reply = action
		}
	}
	if !reply.CSLTx || !reply.CSLBootstrapAlways || !reply.ResponseRetry {
		t.Fatalf("auth reply = %+v, want a queued CSL response with bootstrap always", reply)
	}

	// A preceding reply spends the target's bootstrap: an ordinary reply inside
	// the cooldown gets no train, the auth reply still does.
	bridge := &orderedLiveBridge{cslResult: &cc1101.CSLTxResult{Sent: 255}}
	live, err := radio.StartLive(bridge, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = live.Close() })
	plain := tado.Action{Kind: tado.ActData, Bytes: reply.Bytes, CSLTx: true, Reason: "coap-ack 2.04"}
	for _, action := range []tado.Action{plain, reply, plain} {
		if _, err := live.SendImmediate([]tado.Action{action}, true); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Second)
	}
	if got := bridge.bootstrapSnapshot(); len(got) != 3 || !got[0] || !got[1] || got[2] {
		t.Fatalf("bootstrap flags=%v, want [true true false]", got)
	}

	immediate := newImmediateTestRadio(true, nil)
	d := newDaemon(defaultInstall, emu, immediate, false, "live")
	clock := newManualRetryClock()
	d.retryAfter, d.retryNow = clock.after, func() time.Time { return now }
	t.Cleanup(func() { close(d.done) })
	d.handleCSLCompletion(radio.CSLCompletion{Action: reply, Result: &cc1101.CSLTxResult{Sent: 255}})
	clock.noTimer(t)
	if len(d.dispatchQ) != 1 || !d.dispatchDue[dev.Serial] {
		t.Fatalf("retry dispatch due=%v queue=%v", d.dispatchDue, d.dispatchQ)
	}
	d.dispatchQ = d.dispatchQ[1:]
	delete(d.dispatchDue, dev.Serial)
	d.dispatchQueued(emu, dev.Serial)
	attempts := immediate.queuedAttempts()
	if len(attempts) != 1 || !attempts[0].CSLBootstrapAlways || !attempts[0].ResponseRetry {
		t.Fatalf("retry attempts=%+v", attempts)
	}
	first, err := tado.ParseData(defaultInstall.NetworkKey(), reply.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := tado.ParseData(defaultInstall.NetworkKey(), attempts[0].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if retry.Seq != first.Seq {
		t.Errorf("retry MAC seq=%02x, want %02x", retry.Seq, first.Seq)
	}
	firstApp, retryApp := tado.ParseApp(first.Plaintext), tado.ParseApp(retry.Plaintext)
	if firstApp == nil || retryApp == nil || firstApp.CoAP == nil || retryApp.CoAP == nil || !bytes.Equal(retryApp.CoAP.Raw, firstApp.CoAP.Raw) {
		t.Fatal("retry does not carry the same CoAP answer")
	}
}

func TestNodeAPIShowsSessionAdoption(t *testing.T) {
	emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	dev, err := defaultInstall.Resolve("0a01")
	if err != nil {
		t.Fatal(err)
	}
	emu.OnFrame(deviceRequestUplink(t, dev, authTokenRequest(0x2b90), 0x41))
	var session []byte
	for _, ns := range emu.Nodes() {
		if ns.Short == dev.Short() {
			session = ns.SessionID
		}
	}
	if len(session) == 0 {
		t.Fatal("auth/token served no session id")
	}
	emu.OnFrame(deviceRequestUplink(t, dev, tado.CoAPReq{
		Type: "CON", Code: "0.01", MID: 0x2b91, Path: []string{"time"},
		Options: []tado.CoAPOption{{Num: 2048, Val: session}},
	}, 0x42))
	d := newDaemon(defaultInstall, emu, radio.NewMock(nil), true, "mock")
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)
	srv := httptest.NewServer(d.mux())
	t.Cleanup(func() {
		srv.Close()
		cancel()
	})
	var node map[string]any
	if code := getJSON(t, srv.URL+"/api/nodes/"+dev.Short(), &node); code != http.StatusOK || node["SessionIssued"] != true || node["SessionAdopted"] != true {
		t.Fatalf("got %d issued=%v adopted=%v, want both true", code, node["SessionIssued"], node["SessionAdopted"])
	}
}

// A served session id survives a restart: the new daemon reads the device as
// issued, and adopted on its first request that echoes the id.
func TestSessionSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	clock := newTestClock(windowStart)
	in := windowInstall(t, nil)
	const valve = "VA1000000005"
	dev, err := in.Resolve(valve)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := newWindowDaemon(t, in, clock, newTestStore(t, path))
	feed(t, d, in, valve, false, authTokenRequest(0x2c01))
	var session []byte
	if _, err := d.do(func(e *tado.EmuState) (any, error) {
		for _, ns := range e.Nodes() {
			if ns.Short == dev.Short() {
				session = ns.SessionID
			}
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(session) == 0 {
		t.Fatal("auth/token served no session id")
	}

	clock.advance(time.Hour)
	store := newTestStore(t, path)
	cfg := in.EmuConfig()
	cfg.Now = clock.now
	emu := tado.NewEmu(cfg, tado.NewUnhandledLogger(io.Discard))
	if err := restoreDesiredState(store, emu); err != nil {
		t.Fatalf("restore: %v", err)
	}
	next := newDaemon(in, emu, radio.NewMock(nil), true, "mock")
	next.retryNow = clock.now
	if err := next.attachState(store); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go next.run(ctx)
	srv := httptest.NewServer(next.mux())
	t.Cleanup(func() {
		srv.Close()
		cancel()
	})
	node := func() map[string]any {
		t.Helper()
		var node map[string]any
		if code := getJSON(t, srv.URL+"/api/nodes/"+valve, &node); code != http.StatusOK {
			t.Fatalf("GET node: status %d", code)
		}
		return node
	}
	if got := node(); got["SessionIssued"] != true || got["SessionAdopted"] != false ||
		got["SessionIssuedUnix"] != float64(windowStart.Unix()) {
		t.Errorf("after the restart = issued %v adopted %v at %v, want issued at %d, not adopted",
			got["SessionIssued"], got["SessionAdopted"], got["SessionIssuedUnix"], windowStart.Unix())
	}
	feed(t, next, in, valve, false, tado.CoAPReq{
		Type: "CON", Code: "0.01", MID: 0x2c02, Path: []string{"time"},
		Options: []tado.CoAPOption{{Num: 2048, Val: session}},
	})
	if got := node(); got["SessionIssued"] != true || got["SessionAdopted"] != true {
		t.Errorf("after the echo = issued %v adopted %v, want both true", got["SessionIssued"], got["SessionAdopted"])
	}

	// The new run continues the stored save sequence, so its first save lands.
	clock.advance(time.Hour)
	feed(t, next, in, valve, false, authTokenRequest(0x2c03))
	saved, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := saved.Session[hex.EncodeToString(dev.WireEUI())].IssuedAt; !got.Equal(clock.now()) {
		t.Errorf("stored session issued at %s, want the second run's %s", got, clock.now())
	}
}

func TestDaemonNoAutoQueueAllowsExplicitAPI(t *testing.T) {
	emu := tado.NewEmu(tado.EmuConfig{TxCounter: 100000, NoAutoQueue: true}, tado.NewUnhandledLogger(io.Discard))
	d := newDaemon(defaultInstall, emu, radio.NewMock(nil), true, "mock")
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)
	srv := httptest.NewServer(d.mux())
	t.Cleanup(func() {
		srv.Close()
		cancel()
	})

	var response struct {
		Queued  bool `json:"queued"`
		Pending int  `json:"pending"`
	}
	if code := postJSON(t, http.MethodPost, srv.URL+"/api/nodes/VA1000000005/setpoint",
		map[string]any{"tempC": 5, "mode": "manual", "id": 10}, &response); code != http.StatusOK || !response.Queued || response.Pending != 1 {
		t.Fatalf("explicit setpoint: got %d %+v", code, response)
	}
}

func TestDaemonProactivelyDispatchesIdentifyAndRebootWithoutUplink(t *testing.T) {
	// identify writes the bare path, reboot keeps its query.
	for _, tc := range []struct{ endpoint, uri string }{
		{"identify", "d/identify"},
		{"reboot", "d/reboot?id=3"},
	} {
		t.Run(tc.endpoint, func(t *testing.T) {
			radio := newImmediateTestRadio(true, nil)
			d, url := newImmediateDaemon(t, tado.EmuConfig{TxCounter: 100000, NoAutoQueue: true}, radio, false)
			var response struct {
				Queued  bool `json:"queued"`
				Pending int  `json:"pending"`
			}
			if code := postJSON(t, http.MethodPost, url+"/api/nodes/a408/"+tc.endpoint,
				map[string]any{"id": 3}, &response); code != http.StatusOK || !response.Queued || response.Pending != 1 {
				t.Fatalf("%s: got %d %+v", tc.endpoint, code, response)
			}
			attempts := waitForQueuedAttempts(t, radio, 1)
			if len(attempts) != 1 || !attempts[0].CSLTx {
				t.Fatalf("attempts=%+v, want one CSL queued request", attempts)
			}
			f, err := tado.ParseData(defaultInstall.NetworkKey(), attempts[0].Bytes)
			if err != nil {
				t.Fatal(err)
			}
			app := tado.ParseApp(f.Plaintext)
			if app == nil || app.CoAP == nil || app.CoAP.URI() != tc.uri {
				t.Fatalf("%s request=%+v", tc.endpoint, app)
			}
			v, err := d.do(func(e *tado.EmuState) (any, error) { return e.Queue("a408"), nil })
			if err != nil {
				t.Fatal(err)
			}
			queue := v.([]tado.QueueEntry)
			if len(queue) != 1 || queue[0].Tries != 1 || !queue[0].InFlight {
				t.Fatalf("queue=%+v, want submitted in-flight request", queue)
			}
		})
	}
}

func TestDaemonCalibrateQueuesValveOnly(t *testing.T) {
	radio := newImmediateTestRadio(true, nil)
	_, url := newImmediateDaemon(t, tado.EmuConfig{TxCounter: 100000, NoAutoQueue: true}, radio, false)
	var response struct {
		Queued  bool   `json:"queued"`
		Pending int    `json:"pending"`
		Error   string `json:"error"`
	}
	if code := postJSON(t, http.MethodPost, url+"/api/nodes/RU1000000001/calibrate", nil, &response); code != http.StatusBadRequest || response.Queued {
		t.Fatalf("thermostat calibrate: got %d %+v, want 400", code, response)
	}
	if code := postJSON(t, http.MethodPost, url+"/api/nodes/a408/calibrate", nil, &response); code != http.StatusOK || !response.Queued || response.Pending != 1 {
		t.Fatalf("valve calibrate: got %d %+v", code, response)
	}
	attempts := waitForQueuedAttempts(t, radio, 1)
	if len(attempts) != 1 || !attempts[0].CSLTx || !attempts[0].WaitResponse || !attempts[0].CoAPTransaction {
		t.Fatalf("attempts=%+v, want one CoAP-confirmed CSL request", attempts)
	}
	m := queuedCoAP(t, attempts[0].Bytes)
	if m.Type != "CON" || m.Code != "0.03" || m.URI() != "d/calibrate" || len(m.Payload) != 0 {
		t.Fatalf("calibrate request = %s %s %s payload=%x, want CON 0.03 d/calibrate with no payload", m.Type, m.Code, m.URI(), m.Payload)
	}
}

func TestDaemonExtuiActiveGet(t *testing.T) {
	radio := newImmediateTestRadio(true, nil)
	_, url := newImmediateDaemon(t, tado.EmuConfig{TxCounter: 100000, NoAutoQueue: true}, radio, false)
	var queued struct {
		Resource string `json:"resource"`
		Queued   bool   `json:"queued"`
		Pending  int    `json:"pending"`
	}
	getURL := url + "/api/nodes/a408/resource?path=z%2Fextui&query=lid%3Dpzh"
	if code := getJSON(t, getURL, &queued); code != http.StatusOK || !queued.Queued || queued.Resource != "z/extui?lid=pzh" || queued.Pending != 1 {
		t.Fatalf("active extui GET: got %d %+v", code, queued)
	}
	attempts := waitForQueuedAttempts(t, radio, 1)
	if len(attempts) != 1 {
		t.Fatalf("attempts=%+v", attempts)
	}
	data, err := tado.ParseData(defaultInstall.NetworkKey(), attempts[0].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	request := tado.ParseApp(data.Plaintext).CoAP
	if request == nil || request.Type != "CON" || request.Code != "0.01" || request.URI() != "z/extui?lid=pzh" {
		t.Fatalf("extui GET request=%+v", request)
	}
	device, err := defaultInstall.Resolve("a408")
	if err != nil {
		t.Fatal(err)
	}
	body := tado.BuildExtuiBody("[fd00::1]/z/s", "[fd00::1]/z/p")
	response, err := tado.BuildCoAPAck("2.05", request.MID, request.Token, body, device.WireEUI(), defaultInstall.CoordinatorEUI(), 5683, 4005)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := tado.BuildData(defaultInstall.NetworkKey(), 0x71, defaultInstall.CoordinatorEUI(), device.WireEUI()[:5], tado.WrapAppPayload(response, 100001, nil))
	if err != nil {
		t.Fatal(err)
	}
	radio.frames <- frame.Bytes()

	var got struct {
		Resource string `json:"resource"`
		Code     string `json:"code"`
		Result   string `json:"result"`
	}
	want := hex.EncodeToString(body)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if code := getJSON(t, getURL, &got); code == http.StatusOK && got.Code == "2.05" {
			if got.Resource != "z/extui?lid=pzh" || got.Result != want {
				t.Fatalf("active extui result=%+v, want body %s", got, want)
			}
			break
		}
		time.Sleep(time.Millisecond)
	}
	if got.Code != "2.05" {
		t.Fatal("device extui response was not correlated")
	}

}

func TestDaemonMemActiveGetNormalizesQueryAndRefreshes(t *testing.T) {
	radio := newImmediateTestRadio(true, nil)
	_, url := newImmediateDaemon(t, tado.EmuConfig{TxCounter: 100000, NoAutoQueue: true}, radio, false)
	const memURL = "/api/nodes/0a06/dbg/mem?adr=20007b40&len=16"
	var queued struct {
		Resource string `json:"resource"`
		Queued   bool   `json:"queued"`
		Pending  int    `json:"pending"`
	}
	if code := getJSON(t, url+memURL, &queued); code != http.StatusOK || !queued.Queued || queued.Resource != "d/dbg/m" || queued.Pending != 1 {
		t.Fatalf("initial dbg/mem GET: got %d %+v", code, queued)
	}
	attempts := waitForQueuedAttempts(t, radio, 1)
	if len(attempts) != 1 {
		t.Fatalf("attempts=%+v", attempts)
	}
	request := queuedCoAP(t, attempts[0].Bytes)
	if request.URI() != "d/dbg/m?adr=536902464&len=16" {
		t.Fatalf("dbg/mem wire URI=%q", request.URI())
	}
	firstMID := request.MID

	if code := getJSON(t, url+memURL, &queued); code != http.StatusOK || !queued.Queued || queued.Pending != 1 {
		t.Fatalf("poll while pending: got %d %+v", code, queued)
	}
	if got := len(waitForQueuedAttempts(t, radio, 1)); got != 1 {
		t.Fatalf("pending poll emitted %d attempts", got)
	}

	device, err := defaultInstall.Resolve("0a06")
	if err != nil {
		t.Fatal(err)
	}
	sendMemResponse := func(mid uint16, token, body []byte, counter uint32) {
		t.Helper()
		response, err := tado.BuildCoAPAck("2.05", mid, token, body, device.WireEUI(), defaultInstall.CoordinatorEUI(), 5683, 4005)
		if err != nil {
			t.Fatal(err)
		}
		frame, err := tado.BuildData(defaultInstall.NetworkKey(), byte(counter), defaultInstall.CoordinatorEUI(), device.WireEUI()[:5], tado.WrapAppPayload(response, counter, nil))
		if err != nil {
			t.Fatal(err)
		}
		radio.frames <- frame.Bytes()
	}
	sendMemResponse(firstMID, request.Token, []byte{0x11}, 100001)

	var result struct {
		Resource string `json:"resource"`
		Code     string `json:"code"`
		Result   string `json:"result"`
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if code := getJSON(t, url+memURL, &result); code == http.StatusOK && result.Code == "2.05" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if result.Resource != "d/dbg/m" || result.Code != "2.05" || result.Result != "11" {
		t.Fatalf("cached dbg/mem result=%+v", result)
	}

	if code := getJSON(t, url+memURL+"&refresh=1", &queued); code != http.StatusOK || !queued.Queued || queued.Pending != 1 {
		t.Fatalf("dbg/mem refresh: got %d %+v", code, queued)
	}
	attempts = waitForQueuedAttempts(t, radio, 2)
	if len(attempts) != 2 {
		t.Fatalf("refresh attempts=%+v", attempts)
	}
	refresh := queuedCoAP(t, attempts[1].Bytes)
	if refresh.MID == firstMID || refresh.URI() != request.URI() {
		t.Fatalf("refresh request=%+v, want new MID and URI %q", refresh, request.URI())
	}
	sendMemResponse(refresh.MID, refresh.Token, []byte{0x22}, 100002)

	if code := getJSON(t, url+"/api/nodes/0a06/dbg/mem?adr=20007b41&len=16", &queued); code != http.StatusOK || !queued.Queued || queued.Pending != 1 {
		t.Fatalf("different dbg/mem query: got %d %+v", code, queued)
	}
	attempts = waitForQueuedAttempts(t, radio, 3)
	if len(attempts) != 3 {
		t.Fatalf("different-query attempts=%+v", attempts)
	}
	different := queuedCoAP(t, attempts[2].Bytes)
	if different.URI() != "d/dbg/m?adr=536902465&len=16" {
		t.Fatalf("different dbg/mem URI=%q", different.URI())
	}
}

func TestDaemonMemActiveGetValidatesQuery(t *testing.T) {
	radio := newImmediateTestRadio(true, nil)
	_, url := newImmediateDaemon(t, tado.EmuConfig{TxCounter: 100000, NoAutoQueue: true}, radio, false)
	for _, query := range []string{
		"adr=0x20007b40&len=16",
		"adr=20007b40&len=0",
		"adr=20007b40&len=256",
		"adr=20007b40&len=16&refresh=0",
		"adr=20007b40&len=16&other=1",
	} {
		var response struct {
			Error string `json:"error"`
		}
		if code := getJSON(t, url+"/api/nodes/0a06/dbg/mem?"+query, &response); code != http.StatusBadRequest || response.Error == "" {
			t.Fatalf("query %q: got %d %+v", query, code, response)
		}
	}
}

func TestDaemonExtuiActiveGetValidatesLid(t *testing.T) {
	emu := tado.NewEmu(tado.EmuConfig{TxCounter: 100000, NoAutoQueue: true}, tado.NewUnhandledLogger(io.Discard))
	d := newDaemon(defaultInstall, emu, radio.NewMock(nil), true, "mock")
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)
	srv := httptest.NewServer(d.mux())
	t.Cleanup(func() {
		srv.Close()
		cancel()
	})
	for _, query := range []string{"", "lid=3", "lid=pzx", "lid=pzh%26x=1"} {
		var response struct {
			Error string `json:"error"`
		}
		url := srv.URL + "/api/nodes/a408/resource?path=z%2Fextui"
		if query != "" {
			url += "&query=" + query
		}
		if code := getJSON(t, url, &response); code != http.StatusBadRequest {
			t.Fatalf("query %q: got %d %+v, want 400", query, code, response)
		}
	}
}

func TestDaemonExtuiPZHWrite(t *testing.T) {
	radio := newImmediateTestRadio(true, nil)
	d, url := newImmediateDaemon(t, tado.EmuConfig{TxCounter: 100000, NoAutoQueue: true}, radio, false)
	var queued struct {
		Lid     string `json:"lid"`
		Queued  bool   `json:"queued"`
		Pending int    `json:"pending"`
	}
	if code := postJSON(t, http.MethodPost, url+"/api/nodes/a408/extui", map[string]any{}, &queued); code != http.StatusOK || !queued.Queued || queued.Lid != "pzh" || queued.Pending != 1 {
		t.Fatalf("extui PUT: got %d %+v", code, queued)
	}
	attempts := waitForQueuedAttempts(t, radio, 2)
	if len(attempts) < 2 || !attempts[0].WaitResponse {
		t.Fatalf("attempts=%+v", attempts)
	}
	reasm := tado.NewReassembler()
	var request *tado.CoAPMessage
	var lastSeq byte
	for i, attempt := range attempts {
		data, err := tado.ParseData(defaultInstall.NetworkKey(), attempt.Bytes)
		if err != nil {
			t.Fatalf("fragment %d: %v", i, err)
		}
		lastSeq = data.Seq
		if msg, done := reasm.Add(data); done {
			request = msg
		}
	}
	if request == nil || request.Code != "0.03" || request.URI() != "z/extui?lid=pzh" {
		t.Fatalf("extui request=%+v", request)
	}
	if _, ok := request.Option(7); ok {
		t.Fatal("extui PUT has Uri-Port")
	}
	if cf, ok := request.Option(12); !ok || !bytes.Equal(cf.Val, []byte{42}) {
		t.Fatalf("Content-Format=%+v", cf)
	}
	device, err := defaultInstall.Resolve("a408")
	if err != nil {
		t.Fatal(err)
	}
	radio.frames <- tado.BuildEnhAck(lastSeq, defaultInstall.CoordinatorEUI(), device.WireEUI(), 0)
	ack, err := tado.BuildCoAPAck("2.01", request.MID, request.Token, nil, device.WireEUI(), defaultInstall.CoordinatorEUI(), 5683, 4005)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := tado.BuildData(defaultInstall.NetworkKey(), 0x72, defaultInstall.CoordinatorEUI(), device.WireEUI()[:5], tado.WrapAppPayload(ack, 100001, nil))
	if err != nil {
		t.Fatal(err)
	}
	radio.frames <- frame.Bytes()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		v, err := d.do(func(e *tado.EmuState) (any, error) { return e.QueueHistory("a408"), nil })
		if err != nil {
			t.Fatal(err)
		}
		history := v.([]tado.DownlinkOutcome)
		if len(history) != 0 {
			if history[0].Kind != "extui" || history[0].Reason != "coap-ack" {
				t.Fatalf("history=%+v", history)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("extui PUT did not complete from 2.01")
}

func TestDaemonRebootCompletesOnlyAfterCoAP204(t *testing.T) {
	radio := newImmediateTestRadio(true, nil)
	d, url := newImmediateDaemon(t, tado.EmuConfig{TxCounter: 100000, NoAutoQueue: true}, radio, false)
	var response struct {
		Queued  bool `json:"queued"`
		Pending int  `json:"pending"`
	}
	if code := postJSON(t, http.MethodPost, url+"/api/nodes/a408/reboot", map[string]any{"id": 3}, &response); code != http.StatusOK || !response.Queued || response.Pending != 1 {
		t.Fatalf("reboot: got %d %+v", code, response)
	}
	attempts := waitForQueuedAttempts(t, radio, 1)
	if len(attempts) != 1 || !attempts[0].WaitResponse {
		t.Fatalf("attempts=%+v, want one response-waiting reboot", attempts)
	}
	data, err := tado.ParseData(defaultInstall.NetworkKey(), attempts[0].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	request := tado.ParseApp(data.Plaintext).CoAP
	device, err := defaultInstall.Resolve("a408")
	if err != nil {
		t.Fatal(err)
	}
	wire := device.WireEUI()
	radio.frames <- tado.BuildEnhAck(data.Seq, defaultInstall.CoordinatorEUI(), wire, 0)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		v, err := d.do(func(e *tado.EmuState) (any, error) { return e.Stats().EnhAcks, nil })
		if err != nil {
			t.Fatal(err)
		}
		if v.(int) > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	v, err := d.do(func(e *tado.EmuState) (any, error) { return e.Pending("a408"), nil })
	if err != nil {
		t.Fatal(err)
	}
	if v.(int) != 1 {
		t.Fatalf("Enhanced ACK completed reboot: pending=%d", v.(int))
	}

	ack, err := tado.BuildCoAPAck("2.04", request.MID, request.Token, nil,
		wire, defaultInstall.CoordinatorEUI(), 5683, 4005)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := tado.BuildData(defaultInstall.NetworkKey(), 0x61, defaultInstall.CoordinatorEUI(), wire[:5], tado.WrapAppPayload(ack, 701, nil))
	if err != nil {
		t.Fatal(err)
	}
	radio.frames <- frame.Bytes()
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		v, err := d.do(func(e *tado.EmuState) (any, error) { return e.Pending("a408"), nil })
		if err != nil {
			t.Fatal(err)
		}
		if v.(int) == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("matching 2.04 did not complete reboot")
}

func TestDaemonH11ConfigCompletesOnlyAfterCoAP204(t *testing.T) {
	clock := newManualRetryClock()
	radio := newImmediateTestRadio(true, nil)
	now := time.Unix(100, 0)
	d, url := newImmediateDaemonWithRetry(t, tado.EmuConfig{
		TxCounter: 100000, NoAutoQueue: true, Now: func() time.Time { return now },
	}, radio, false, clock.after, fixedRetryClock(&now))

	var queued struct {
		Queued  bool `json:"queued"`
		Pending int  `json:"pending"`
	}
	if code := postJSON(t, http.MethodPost, url+"/api/nodes/0a06/config", map[string]any{
		"id": 5, "utcOffsetMin": 120, "nodeTable": []map[string]any{{"role": 5, "node": 6}},
	}, &queued); code != http.StatusOK || !queued.Queued || queued.Pending != 1 {
		t.Fatalf("H11 config: got %d %+v", code, queued)
	}
	attempts := waitForQueuedAttempts(t, radio, 1)
	if len(attempts) != 1 || !attempts[0].WaitResponse || !attempts[0].CoAPTransaction {
		t.Fatalf("attempts=%+v, want one CoAP-confirmed config", attempts)
	}
	first := queuedCoAP(t, attempts[0].Bytes)
	if first.URI() != "d/config" {
		t.Fatalf("config URI=%q, want the bare d/config", first.URI())
	}
	// The write carries the whole document, so the two fields the request set
	// are records inside it and the generated 015a closes it.
	document := hex.EncodeToString(first.Payload)
	if !strings.HasPrefix(document, "0143") || !strings.Contains(document, "015e020506") ||
		!strings.Contains(document, "02b2020078") ||
		len(document) < 22 || !strings.HasPrefix(document[len(document)-22:], "015a08") {
		t.Fatalf("config document=%s, want 015e=0506 02b2=0078 and a generated 015a last", document)
	}
	device, err := defaultInstall.Resolve("0a06")
	if err != nil {
		t.Fatal(err)
	}
	wire := device.WireEUI()
	radio.frames <- tado.BuildEnhAck(attempts[0].Bytes[2], defaultInstall.CoordinatorEUI(), wire, 0)
	if got := clock.wait(t); got != coapRetryMin {
		t.Fatalf("config retry delay=%s, want %s", got, coapRetryMin)
	}
	v, err := d.do(func(e *tado.EmuState) (any, error) { return e.Pending("0a06"), nil })
	if err != nil {
		t.Fatal(err)
	}
	if v.(int) != 1 {
		t.Fatalf("Enhanced ACK completed H11 config: pending=%d", v.(int))
	}
	now = now.Add(coapRetryMin)
	clock.tick <- now
	attempts = waitForQueuedAttempts(t, radio, 2)
	if len(attempts) != 2 {
		t.Fatalf("config attempts=%+v", attempts)
	}
	retry := queuedCoAP(t, attempts[1].Bytes)
	if retry.MID != first.MID || !bytes.Equal(retry.Token, first.Token) {
		t.Fatalf("config retry MID/token=%04x/%x, want %04x/%x", retry.MID, retry.Token, first.MID, first.Token)
	}
	wrong, err := tado.BuildCoAPAck("2.04", first.MID, []byte{0}, nil, wire,
		defaultInstall.CoordinatorEUI(), 5683, 4005)
	if err != nil {
		t.Fatal(err)
	}
	wrongFrame, err := tado.BuildData(defaultInstall.NetworkKey(), 0x71, defaultInstall.CoordinatorEUI(), wire[:5], tado.WrapAppPayload(wrong, 100001, nil))
	if err != nil {
		t.Fatal(err)
	}
	radio.frames <- wrongFrame.Bytes()
	v, err = d.do(func(e *tado.EmuState) (any, error) { return e.Pending("0a06"), nil })
	if err != nil {
		t.Fatal(err)
	}
	if v.(int) != 1 {
		t.Fatalf("wrong token completed H11 config: pending=%d", v.(int))
	}
	ack, err := tado.BuildCoAPAck("2.04", first.MID, first.Token, nil, wire,
		defaultInstall.CoordinatorEUI(), 5683, 4005)
	if err != nil {
		t.Fatal(err)
	}
	ackFrame, err := tado.BuildData(defaultInstall.NetworkKey(), 0x72, defaultInstall.CoordinatorEUI(), wire[:5], tado.WrapAppPayload(ack, 100002, nil))
	if err != nil {
		t.Fatal(err)
	}
	radio.frames <- ackFrame.Bytes()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		v, err = d.do(func(e *tado.EmuState) (any, error) { return e.QueueHistory("0a06"), nil })
		if err != nil {
			t.Fatal(err)
		}
		history := v.([]tado.DownlinkOutcome)
		if len(history) != 0 {
			if history[0].Result != "delivered" || history[0].Reason != "coap-ack" {
				t.Fatalf("H11 history=%+v", history)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("matching H11 config 2.04 did not complete")
}

func TestDaemonProactiveDispatchRollsBackWhenCSLHasNoAnchor(t *testing.T) {
	bridge := &orderedLiveBridge{
		result:    &cc1101.AutoAckResult{Status: cc1101.AutoAckNoFrame},
		cslErr:    errors.New("CSLTX has no target sample anchor"),
		cslResult: &cc1101.CSLTxResult{},
	}
	live, err := radio.StartLive(bridge, defaultInstall.CoordinatorEUI(), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	d, url := newImmediateDaemon(t, tado.EmuConfig{TxCounter: 100000}, live, false)

	var response struct {
		Queued bool `json:"queued"`
	}
	if code := postJSON(t, http.MethodPost, url+"/api/nodes/a408/identify", map[string]any{"id": 3}, &response); code != http.StatusOK || !response.Queued {
		t.Fatalf("identify: got %d %+v", code, response)
	}
	deadline := time.Now().Add(time.Second)
	for len(bridge.cslSnapshot()) < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := bridge.bootstrapSnapshot(); len(got) != 1 || !got[0] {
		t.Fatalf("first bootstrap flags=%v, want [true]", got)
	}
	v, err := d.do(func(e *tado.EmuState) (any, error) { return e.Queue("a408"), nil })
	if err != nil {
		t.Fatal(err)
	}
	queue := v.([]tado.QueueEntry)
	if len(queue) != 1 || queue[0].Tries != 0 || queue[0].InFlight {
		t.Fatalf("queue=%+v, want rolled-back request", queue)
	}

	if code := postJSON(t, http.MethodPost, url+"/api/nodes/a408/reboot", map[string]any{"id": 3}, &response); code != http.StatusOK || !response.Queued {
		t.Fatalf("reboot: got %d %+v", code, response)
	}
	deadline = time.Now().Add(time.Second)
	for len(bridge.cslSnapshot()) < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := bridge.bootstrapSnapshot(); len(got) != 2 || !got[0] || !got[1] {
		t.Fatalf("bootstrap flags=%v, want [true true]", got)
	}
}

func TestDaemonProactiveNoAnchorRetriesWithoutUplink(t *testing.T) {
	clock := newManualRetryClock()
	radio := newImmediateTestRadio(false, errors.New("CSLTX has no target sample anchor"))
	_, url := newImmediateDaemonWithRetry(t, tado.EmuConfig{TxCounter: 100000}, radio, false, clock.after)
	var response struct {
		Queued bool `json:"queued"`
	}
	if code := postJSON(t, http.MethodPost, url+"/api/nodes/a408/identify", map[string]any{"id": 3}, &response); code != http.StatusOK || !response.Queued {
		t.Fatalf("identify: got %d %+v", code, response)
	}
	if attempts := waitForQueuedAttempts(t, radio, 1); len(attempts) != 1 {
		t.Fatalf("initial attempts=%d, want 1", len(attempts))
	}
	clock.wait(t)
	clock.tick <- time.Now()
	if attempts := waitForQueuedAttempts(t, radio, 2); len(attempts) != 2 {
		t.Fatalf("retry attempts=%d, want 2", len(attempts))
	}
}

func TestDaemonIdentifyCoAPRetransmitParity(t *testing.T) {
	clock := newManualRetryClock()
	radio := newImmediateTestRadio(true, nil)
	now := time.Unix(100, 0)
	d, url := newImmediateDaemonWithRetry(t, tado.EmuConfig{TxCounter: 100000, Now: func() time.Time { return now }}, radio, false, clock.after, fixedRetryClock(&now))

	var response struct {
		Queued bool `json:"queued"`
	}
	if code := postJSON(t, http.MethodPost, url+"/api/nodes/a408/identify", map[string]any{"id": 3}, &response); code != http.StatusOK || !response.Queued {
		t.Fatalf("identify: got %d %+v", code, response)
	}
	if attempts := waitForQueuedAttempts(t, radio, 1); len(attempts) != 1 || !attempts[0].WaitResponse || !attempts[0].CoAPTransaction {
		t.Fatalf("first attempts=%+v, want one CoAP-confirmed identify", attempts)
	}

	base := coapRetryMin
	for try := 1; try <= 4; try++ {
		want := base << (try - 1)
		if got := clock.wait(t); got != want {
			t.Fatalf("try %d retry delay=%s, want %s", try, got, want)
		}
		now = now.Add(want)
		clock.tick <- now
		if try < 4 {
			if attempts := waitForQueuedAttempts(t, radio, try+1); len(attempts) != try+1 {
				t.Fatalf("try %d attempts=%d, want %d", try+1, len(attempts), try+1)
			}
		}
	}

	attempts := radio.queuedAttempts()
	if len(attempts) != 4 {
		t.Fatalf("attempts=%d, want 4", len(attempts))
	}
	first, err := tado.ParseData(defaultInstall.NetworkKey(), attempts[0].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	firstApp := tado.ParseApp(first.Plaintext)
	if firstApp == nil || firstApp.CoAP == nil {
		t.Fatal("first identify has no CoAP")
	}
	for i, attempt := range attempts[1:] {
		frame, err := tado.ParseData(defaultInstall.NetworkKey(), attempt.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		app := tado.ParseApp(frame.Plaintext)
		if app == nil || app.CoAP == nil || app.CoAP.MID != firstApp.CoAP.MID || !bytes.Equal(app.CoAP.Token, firstApp.CoAP.Token) {
			t.Fatalf("attempt %d CoAP identity mismatch", i+2)
		}
		if frame.Seq == first.Seq || bytes.Equal(frame.Plaintext[4:8], first.Plaintext[4:8]) {
			t.Fatalf("attempt %d reused MAC seq or CCM counter", i+2)
		}
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		v, err := d.do(func(e *tado.EmuState) (any, error) { return e.Pending("a408"), nil })
		if err != nil {
			t.Fatal(err)
		}
		if v.(int) == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("expired transaction remained pending")
}

func TestLinkMaintenanceBootstrapsAndResetsAfterFourProbes(t *testing.T) {
	radio := newImmediateTestRadio(true, nil)
	emu := tado.NewEmu(tado.EmuConfig{TxCounter: 100000}, tado.NewUnhandledLogger(io.Discard))
	d := newDaemon(defaultInstall, emu, radio, false, "live")
	d.initLinkMaintenance()
	dev, err := defaultInstall.Resolve("a408")
	if err != nil {
		t.Fatal(err)
	}
	key := hex.EncodeToString(dev.WireEUI())
	d.linkMaint = map[string]linkMaintEntry{key: {state: 1, retry: -1}}
	d.linkMaintOrder = []string{key}
	firstAt := 0
	for tick := 1; len(radio.batches) < 4; tick++ {
		d.linkMaintenanceTick(emu)
		if len(radio.batches) == 1 && firstAt == 0 {
			firstAt = tick
		}
		if tick > 2000 {
			t.Fatal("maintenance did not finish four probes")
		}
	}
	if want := linkMaintStartupTicks + linkMaintIdleTicks + linkMaintRetryTicks - 2; firstAt != want {
		t.Fatalf("first Echo tick=%d, want %d", firstAt, want)
	}
	if len(radio.batches) != 4 {
		t.Fatalf("maintenance sends=%d, want 4", len(radio.batches))
	}
	for i, batch := range radio.batches {
		if len(batch) != 1 || !batch[0].CSLTx || !batch[0].CSLBootstrapAlways || !batch[0].WaitResponse || batch[0].QueueAttempt {
			t.Fatalf("send %d action=%+v", i+1, batch)
		}
	}
	for range linkMaintPostSendTicks + 2 {
		d.linkMaintenanceTick(emu)
	}
	entry := d.linkMaint[key]
	if entry.state != 2 || entry.retry != -1 {
		t.Fatalf("terminal state=%+v, want state 2 retry -1", entry)
	}
}

func TestLinkMaintenanceSelectsOneReadyTargetInOrder(t *testing.T) {
	radio := newImmediateTestRadio(true, nil)
	emu := tado.NewEmu(tado.EmuConfig{TxCounter: 100000}, tado.NewUnhandledLogger(io.Discard))
	d := newDaemon(defaultInstall, emu, radio, false, "live")
	first, err := defaultInstall.Resolve("a408")
	if err != nil {
		t.Fatal(err)
	}
	second, err := defaultInstall.Resolve("0a06")
	if err != nil {
		t.Fatal(err)
	}
	firstKey, secondKey := hex.EncodeToString(first.WireEUI()), hex.EncodeToString(second.WireEUI())
	d.linkMaintTick = linkMaintStartupTicks
	d.linkMaint = map[string]linkMaintEntry{
		firstKey:  {state: 1, retry: 0},
		secondKey: {state: 1, retry: 0},
	}
	d.linkMaintOrder = []string{firstKey, secondKey}
	d.linkMaintenanceTick(emu)
	if len(radio.batches) != 1 || d.linkMaint[firstKey].retry != 1 || d.linkMaint[secondKey].retry != 0 {
		t.Fatalf("first transition batches=%d entries=%+v", len(radio.batches), d.linkMaint)
	}
	for range linkMaintPostSendTicks {
		d.linkMaintenanceTick(emu)
	}
	if len(radio.batches) != 2 || d.linkMaint[secondKey].retry != 1 {
		t.Fatalf("second transition batches=%d entries=%+v", len(radio.batches), d.linkMaint)
	}
}

func TestLinkMaintenanceHonorsNoAutoQueue(t *testing.T) {
	radio := newImmediateTestRadio(true, nil)
	emu := tado.NewEmu(tado.EmuConfig{TxCounter: 100000, NoAutoQueue: true}, tado.NewUnhandledLogger(io.Discard))
	d := newDaemon(defaultInstall, emu, radio, false, "live")
	d.linkMaintTick = linkMaintStartupTicks
	d.linkMaint = map[string]linkMaintEntry{"a408": {state: 1, retry: 0}}
	d.linkMaintOrder = []string{"a408"}
	d.linkMaintenanceTick(emu)
	if len(radio.batches) != 0 {
		t.Fatalf("no-auto-queue sent %d maintenance batches", len(radio.batches))
	}
}

func TestLinkMaintenanceAcceptsOnlyCompleteFragmentedIPv6(t *testing.T) {
	dev, err := defaultInstall.Resolve("a408")
	if err != nil {
		t.Fatal(err)
	}
	pkt, err := tado.BuildCoAP(tado.CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x7410, Token: []byte{0x74},
		Path: []string{"d", "config"}, Query: []string{"id=3"},
		Payload: bytes.Repeat([]byte{0x74}, 260),
		SrcEUI:  dev.WireEUI(), DstEUI: defaultInstall.CoordinatorEUI(), SrcPort: 5683, DstPort: 4005,
	})
	if err != nil {
		t.Fatal(err)
	}
	fragments, err := tado.Fragment6LoWPANDatagram(pkt, 0x7410)
	if err != nil {
		t.Fatal(err)
	}
	build := func(t *testing.T, bodies [][]byte) [][]byte {
		t.Helper()
		frames := make([][]byte, 0, len(bodies))
		for i, body := range bodies {
			frame, err := tado.BuildDataPending(defaultInstall.NetworkKey(), byte(i), defaultInstall.CoordinatorEUI(), dev.WireEUI()[:5], tado.WrapAppPayload(body, uint32(i+1), nil))
			if err != nil {
				t.Fatal(err)
			}
			frames = append(frames, frame.Bytes())
		}
		return frames
	}
	key := hex.EncodeToString(dev.WireEUI())
	initial := linkMaintEntry{state: 2, retry: 3, age: 9, wait: 4}
	assertUnchanged := func(t *testing.T, frames [][]byte) {
		t.Helper()
		emu := tado.NewEmu(tado.EmuConfig{TxCounter: 100000}, tado.NewUnhandledLogger(io.Discard))
		d := newDaemon(defaultInstall, emu, newImmediateTestRadio(true, nil), false, "live")
		d.linkMaint[key] = initial
		for _, frame := range frames {
			d.handleFrame(frame, true)
		}
		if got := d.linkMaint[key]; got != initial {
			t.Fatalf("entry=%+v, want unchanged %+v", got, initial)
		}
	}

	assertUnchanged(t, build(t, fragments[:1]))
	assertUnchanged(t, build(t, [][]byte{fragments[0], fragments[len(fragments)-1]}))
	overlap := append([]byte{}, fragments[1]...)
	overlap[len(overlap)-1] ^= 0xff
	assertUnchanged(t, build(t, [][]byte{fragments[0], fragments[1], overlap}))
	assertUnchanged(t, build(t, [][]byte{{0xe0, 0x00, 0x00, 0x00, 0x00}}))

	emu := tado.NewEmu(tado.EmuConfig{TxCounter: 100000}, tado.NewUnhandledLogger(io.Discard))
	d := newDaemon(defaultInstall, emu, newImmediateTestRadio(true, nil), false, "live")
	d.linkMaint[key] = initial
	for i, frame := range build(t, fragments) {
		d.handleFrame(frame, true)
		if i != len(fragments)-1 && d.linkMaint[key] != initial {
			t.Fatalf("fragment %d reset entry early: %+v", i, d.linkMaint[key])
		}
	}
	if got := d.linkMaint[key]; got != (linkMaintEntry{state: 1, retry: -1}) {
		t.Fatalf("completed entry=%+v, want state 1 retry -1", got)
	}
}

func TestDaemonRebootUsesGenericCoAPRetransmit(t *testing.T) {
	clock := newManualRetryClock()
	radio := newImmediateTestRadio(true, nil)
	now := time.Unix(100, 0)
	d, url := newImmediateDaemonWithRetry(t, tado.EmuConfig{TxCounter: 100000, Now: func() time.Time { return now }}, radio, false, clock.after, fixedRetryClock(&now))

	var response struct {
		Queued bool `json:"queued"`
	}
	if code := postJSON(t, http.MethodPost, url+"/api/nodes/a408/reboot", map[string]any{"id": 3}, &response); code != http.StatusOK || !response.Queued {
		t.Fatalf("reboot: got %d %+v", code, response)
	}
	attempts := waitForQueuedAttempts(t, radio, 1)
	if len(attempts) != 1 || !attempts[0].CoAPTransaction || !attempts[0].WaitResponse {
		t.Fatalf("first attempts=%+v, want one CoAP transaction", attempts)
	}

	for try := 1; try <= 3; try++ {
		want := coapRetryMin << (try - 1)
		if got := clock.wait(t); got != want {
			t.Fatalf("retry %d delay=%s, want %s", try, got, want)
		}
		now = now.Add(want)
		clock.tick <- now
		if attempts = waitForQueuedAttempts(t, radio, try+1); len(attempts) != try+1 {
			t.Fatalf("attempts=%d, want %d", len(attempts), try+1)
		}
	}

	first, err := tado.ParseData(defaultInstall.NetworkKey(), attempts[0].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	firstApp := tado.ParseApp(first.Plaintext)
	if firstApp == nil || firstApp.CoAP == nil || firstApp.CoAP.URI() != "d/reboot?id=3" {
		t.Fatalf("first reboot=%+v", firstApp)
	}
	for i, attempt := range attempts[1:] {
		frame, err := tado.ParseData(defaultInstall.NetworkKey(), attempt.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		app := tado.ParseApp(frame.Plaintext)
		if app == nil || app.CoAP == nil || app.CoAP.MID != firstApp.CoAP.MID || !bytes.Equal(app.CoAP.Token, firstApp.CoAP.Token) {
			t.Fatalf("attempt %d CoAP identity mismatch", i+2)
		}
		if frame.Seq == first.Seq || bytes.Equal(frame.Plaintext[4:8], first.Plaintext[4:8]) {
			t.Fatalf("attempt %d reused MAC seq or CCM counter", i+2)
		}
	}

	if got := clock.wait(t); got != coapRetryMin<<3 {
		t.Fatalf("terminal delay=%s, want %s", got, coapRetryMin<<3)
	}
	now = now.Add(coapRetryMin << 3)
	clock.tick <- now
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		v, err := d.do(func(e *tado.EmuState) (any, error) { return e.Pending("a408"), nil })
		if err != nil {
			t.Fatal(err)
		}
		if v.(int) == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("expired reboot transaction remained pending")
}

func TestDaemonIdentifyCoAPAckCancelsRetry(t *testing.T) {
	clock := newManualRetryClock()
	radio := newImmediateTestRadio(true, nil)
	now := time.Unix(100, 0)
	d, url := newImmediateDaemonWithRetry(t, tado.EmuConfig{TxCounter: 100000, Now: func() time.Time { return now }}, radio, false, clock.after, fixedRetryClock(&now))

	var response struct {
		Queued bool `json:"queued"`
	}
	if code := postJSON(t, http.MethodPost, url+"/api/nodes/a408/identify", map[string]any{"id": 3}, &response); code != http.StatusOK || !response.Queued {
		t.Fatalf("identify: got %d %+v", code, response)
	}
	attempts := waitForQueuedAttempts(t, radio, 1)
	if len(attempts) != 1 {
		t.Fatalf("attempts=%d, want 1", len(attempts))
	}
	if got := clock.wait(t); got != coapRetryMin {
		t.Fatalf("retry delay=%s, want %s", got, coapRetryMin)
	}
	request, err := tado.ParseData(defaultInstall.NetworkKey(), attempts[0].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	app := tado.ParseApp(request.Plaintext)
	if app == nil || app.CoAP == nil {
		t.Fatal("identify has no CoAP")
	}
	device, err := defaultInstall.Resolve("a408")
	if err != nil {
		t.Fatal(err)
	}
	ack, err := tado.BuildCoAPAck("2.04", app.CoAP.MID, app.CoAP.Token, nil, device.WireEUI(), defaultInstall.CoordinatorEUI(), 5683, 4005)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := tado.BuildData(defaultInstall.NetworkKey(), 0x61, defaultInstall.CoordinatorEUI(), device.WireEUI()[:5], tado.WrapAppPayload(ack, 701, nil))
	if err != nil {
		t.Fatal(err)
	}
	radio.frames <- frame.Bytes()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		v, err := d.do(func(e *tado.EmuState) (any, error) { return e.Pending("a408"), nil })
		if err != nil {
			t.Fatal(err)
		}
		if v.(int) == 0 {
			now = now.Add(coapRetryMin)
			clock.tick <- now
			time.Sleep(10 * time.Millisecond)
			if got := len(radio.queuedAttempts()); got != 1 {
				t.Fatalf("CoAP ACK left %d attempts, want 1", got)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("matching identify 2.04 did not complete queue")
}

func TestDaemonIdentifyNewTransactionReplacesStaleRetryTimer(t *testing.T) {
	clock := newIndexedRetryClock()
	radio := newImmediateTestRadio(true, nil)
	started := time.Unix(100, 0)
	now := started
	d, url := newImmediateDaemonWithRetry(t, tado.EmuConfig{TxCounter: 100000, Now: func() time.Time { return now }}, radio, false, clock.after, fixedRetryClock(&now))

	var response struct {
		Queued bool `json:"queued"`
	}
	if code := postJSON(t, http.MethodPost, url+"/api/nodes/a408/identify", map[string]any{"id": 3}, &response); code != http.StatusOK || !response.Queued {
		t.Fatalf("first identify: got %d %+v", code, response)
	}
	attempts := waitForQueuedAttempts(t, radio, 1)
	if got := clock.wait(t); got != coapRetryMin {
		t.Fatalf("first retry delay=%s, want %s", got, coapRetryMin)
	}
	request, err := tado.ParseData(defaultInstall.NetworkKey(), attempts[0].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	app := tado.ParseApp(request.Plaintext)
	if app == nil || app.CoAP == nil {
		t.Fatal("first identify has no CoAP")
	}
	device, err := defaultInstall.Resolve("a408")
	if err != nil {
		t.Fatal(err)
	}
	ack, err := tado.BuildCoAPAck("2.04", app.CoAP.MID, app.CoAP.Token, nil, device.WireEUI(), defaultInstall.CoordinatorEUI(), 5683, 4005)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := tado.BuildData(defaultInstall.NetworkKey(), 0x61, defaultInstall.CoordinatorEUI(), device.WireEUI()[:5], tado.WrapAppPayload(ack, 701, nil))
	if err != nil {
		t.Fatal(err)
	}
	radio.frames <- frame.Bytes()
	completed := false
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		v, err := d.do(func(e *tado.EmuState) (any, error) { return e.Pending("a408"), nil })
		if err != nil {
			t.Fatal(err)
		}
		if v.(int) == 0 {
			completed = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !completed {
		t.Fatal("matching identify 2.04 did not complete queue")
	}
	now = now.Add(500 * time.Millisecond)
	if code := postJSON(t, http.MethodPost, url+"/api/nodes/a408/identify", map[string]any{"id": 3}, &response); code != http.StatusOK || !response.Queued {
		t.Fatalf("second identify: got %d %+v", code, response)
	}
	if attempts = waitForQueuedAttempts(t, radio, 2); len(attempts) != 2 {
		t.Fatalf("attempts=%d, want 2", len(attempts))
	}
	if got := clock.wait(t); got != coapRetryMin {
		t.Fatalf("second retry delay=%s, want %s", got, coapRetryMin)
	}
	clock.tick(t, 0, started.Add(coapRetryMin))
	time.Sleep(10 * time.Millisecond)
	if got := len(radio.queuedAttempts()); got != 2 {
		t.Fatalf("stale timer dispatched %d attempts, want 2", got)
	}
	now = now.Add(coapRetryMin)
	clock.tick(t, 1, now)
	if attempts = waitForQueuedAttempts(t, radio, 3); len(attempts) != 3 {
		t.Fatalf("new retry attempts=%d, want 3", len(attempts))
	}
}

func TestDaemonIdentifyKeepsCONTimerWhenFIFOCommandIsQueued(t *testing.T) {
	clock := newManualRetryClock()
	radio := newImmediateTestRadio(true, nil)
	now := time.Unix(100, 0)
	d, url := newImmediateDaemonWithRetry(t, tado.EmuConfig{TxCounter: 100000, Now: func() time.Time { return now }}, radio, false, clock.after, fixedRetryClock(&now))

	var response struct {
		Queued bool `json:"queued"`
	}
	if code := postJSON(t, http.MethodPost, url+"/api/nodes/a408/identify", map[string]any{"id": 3}, &response); code != http.StatusOK || !response.Queued {
		t.Fatalf("identify: got %d %+v", code, response)
	}
	if attempts := waitForQueuedAttempts(t, radio, 1); len(attempts) != 1 {
		t.Fatalf("identify attempts=%d, want 1", len(attempts))
	}
	if got := clock.wait(t); got != coapRetryMin {
		t.Fatalf("identify retry delay=%s, want %s", got, coapRetryMin)
	}
	if code := postJSON(t, http.MethodPost, url+"/api/nodes/a408/reboot", map[string]any{"id": 3}, &response); code != http.StatusOK || !response.Queued {
		t.Fatalf("reboot: got %d %+v", code, response)
	}
	clock.noTimer(t)
	now = now.Add(coapRetryMin)
	clock.tick <- now
	attempts := waitForQueuedAttempts(t, radio, 2)
	if len(attempts) != 2 {
		t.Fatalf("attempts=%d, want 2", len(attempts))
	}
	frame, err := tado.ParseData(defaultInstall.NetworkKey(), attempts[1].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	app := tado.ParseApp(frame.Plaintext)
	if app == nil || app.CoAP == nil || app.CoAP.URI() != "d/identify" {
		t.Fatalf("second attempt=%+v, want identify retry", app)
	}
	v, err := d.do(func(e *tado.EmuState) (any, error) { return e.Queue("a408"), nil })
	if err != nil {
		t.Fatal(err)
	}
	queue := v.([]tado.QueueEntry)
	if len(queue) != 2 || queue[0].Kind != "identify" || queue[0].Tries != 2 || !queue[0].InFlight || queue[1].Kind != "reboot" || queue[1].Tries != 0 {
		t.Fatalf("queue=%+v, want identify retry before pending reboot", queue)
	}
}

func TestDaemonIdentifyMACACKStaysPendingForCoAP(t *testing.T) {
	bridge := &orderedLiveBridge{result: &cc1101.AutoAckResult{Status: cc1101.AutoAckNoFrame}, cslAutoAck: true}
	live, err := radio.StartLive(bridge, defaultInstall.CoordinatorEUI(), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := newImmediateDaemon(t, tado.EmuConfig{TxCounter: 100000}, live, false)
	if _, err := d.do(func(e *tado.EmuState) (any, error) {
		if err := e.EnqueueIdentify("a408", 3); err != nil {
			return nil, err
		}
		if err := e.EnqueueReboot("a408", 3); err != nil {
			return nil, err
		}
		d.dispatchQueued(e, "a408")
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for len(bridge.cslSnapshot()) < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	attempts := bridge.cslSnapshot()
	if len(attempts) != 1 {
		t.Fatalf("attempts=%d, want only identify before CoAP ACK", len(attempts))
	}
	f, err := tado.ParseData(defaultInstall.NetworkKey(), attempts[0])
	if err != nil {
		t.Fatal(err)
	}
	if app := tado.ParseApp(f.Plaintext); app == nil || app.CoAP == nil || app.CoAP.URI() != "d/identify" {
		t.Fatalf("attempt=%+v, want identify", app)
	}
	v, err := d.do(func(e *tado.EmuState) (any, error) { return e.Queue("a408"), nil })
	if err != nil {
		t.Fatal(err)
	}
	queue := v.([]tado.QueueEntry)
	if len(queue) != 2 || queue[0].Kind != "identify" || queue[0].Tries != 1 || !queue[0].InFlight || queue[1].Kind != "reboot" || queue[1].Tries != 0 {
		t.Fatalf("queue=%+v, want in-flight identify followed by reboot", queue)
	}
}

func TestDaemonProactiveRetryDoesNotDuplicateBeforeDue(t *testing.T) {
	clock := newManualRetryClock()
	radio := newImmediateTestRadio(true, nil)
	d, url := newImmediateDaemonWithRetry(t, tado.EmuConfig{TxCounter: 100000}, radio, false, clock.after)
	var response struct {
		Queued bool `json:"queued"`
	}
	if code := postJSON(t, http.MethodPost, url+"/api/nodes/a408/identify", map[string]any{"id": 3}, &response); code != http.StatusOK || !response.Queued {
		t.Fatalf("identify: got %d %+v", code, response)
	}
	clock.wait(t)
	clock.tick <- time.Now()
	clock.wait(t)
	if attempts := radio.queuedAttempts(); len(attempts) != 1 {
		t.Fatalf("early retry dispatched %d queued attempts, want 1", len(attempts))
	}
	if _, err := d.do(func(e *tado.EmuState) (any, error) { return e.Pending("a408"), nil }); err != nil {
		t.Fatal(err)
	}
}

func TestDaemonProactiveRetryStopsForBridgeLatchAndShutdown(t *testing.T) {
	for _, mode := range []string{"bridge", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			clock := newManualRetryClock()
			radio := newImmediateTestRadio(false, errors.New("CSLTX has no target sample anchor"))
			emu := tado.NewEmu(tado.EmuConfig{TxCounter: 100000}, tado.NewUnhandledLogger(io.Discard))
			d := newDaemon(defaultInstall, emu, radio, false, "live")
			d.retryAfter = clock.after
			ctx, cancel := context.WithCancel(context.Background())
			go d.run(ctx)
			t.Cleanup(cancel)
			if _, err := d.do(func(e *tado.EmuState) (any, error) {
				if err := e.EnqueueIdentify("a408", 3); err != nil {
					return nil, err
				}
				d.dispatchQueued(e, "a408")
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			clock.wait(t)
			if mode == "bridge" {
				bridge, _ := goldenPair(t)
				if _, err := d.do(func(e *tado.EmuState) (any, error) {
					e.OnFrame(bridge)
					return nil, nil
				}); err != nil {
					t.Fatal(err)
				}
			} else {
				cancel()
				select {
				case <-d.done:
				case <-time.After(time.Second):
					t.Fatal("daemon did not stop")
				}
			}
			clock.tick <- time.Now()
			time.Sleep(10 * time.Millisecond)
			if attempts := radio.queuedAttempts(); len(attempts) != 1 {
				t.Fatalf("%s retry dispatched %d attempts, want 1", mode, len(attempts))
			}
		})
	}
}

func TestDaemonProactiveDispatchRespectsBridgeLatch(t *testing.T) {
	radio := newImmediateTestRadio(true, nil)
	d, url := newImmediateDaemon(t, tado.EmuConfig{TxCounter: 100000}, radio, false)
	bridge, _ := goldenPair(t)
	if _, err := d.do(func(e *tado.EmuState) (any, error) {
		e.OnFrame(bridge)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	var response struct {
		Queued bool `json:"queued"`
	}
	if code := postJSON(t, http.MethodPost, url+"/api/nodes/a408/identify", map[string]any{"id": 3}, &response); code != http.StatusOK || !response.Queued {
		t.Fatalf("identify: got %d %+v", code, response)
	}
	if got := radio.queuedAttempts(); len(got) != 0 {
		t.Fatalf("bridge latch dispatched %d queued attempts", len(got))
	}
	v, err := d.do(func(e *tado.EmuState) (any, error) { return e.Queue("a408"), nil })
	if err != nil {
		t.Fatal(err)
	}
	queue := v.([]tado.QueueEntry)
	if len(queue) != 1 || queue[0].Tries != 0 || queue[0].InFlight {
		t.Fatalf("queue=%+v, want unsent request", queue)
	}
}

func TestDaemonProactiveDispatchKeepsFIFOAndAvoidsRacingUplinkDuplicate(t *testing.T) {
	radio := newImmediateTestRadio(true, nil)
	d, url := newImmediateDaemon(t, tado.EmuConfig{TxCounter: 100000}, radio, false)
	var response struct {
		Queued bool `json:"queued"`
	}
	for _, endpoint := range []string{"identify", "reboot"} {
		if code := postJSON(t, http.MethodPost, url+"/api/nodes/a408/"+endpoint, map[string]any{"id": 3}, &response); code != http.StatusOK || !response.Queued {
			t.Fatalf("%s: got %d %+v", endpoint, code, response)
		}
	}
	if attempts := waitForQueuedAttempts(t, radio, 1); len(attempts) != 1 {
		t.Fatalf("queued attempts=%d, want only FIFO head", len(attempts))
	}

	device, err := defaultInstall.Resolve("a408")
	if err != nil {
		t.Fatal(err)
	}
	pkt, err := tado.BuildCoAP(tado.CoAPReq{
		Type: "NON", Code: "0.01", MID: 0x6110, Token: []byte{0x51},
		Path:   []string{"h", "7654321", "d", "time"},
		SrcEUI: device.WireEUI(), DstEUI: defaultInstall.CoordinatorEUI(), SrcPort: 5683, DstPort: 4005,
	})
	if err != nil {
		t.Fatal(err)
	}
	uplink, err := tado.BuildData(defaultInstall.NetworkKey(), 0x21, defaultInstall.CoordinatorEUI(),
		device.WireEUI()[:5], tado.WrapAppPayload(pkt, 700, nil))
	if err != nil {
		t.Fatal(err)
	}
	radio.frames <- uplink.Bytes()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		v, err := d.do(func(e *tado.EmuState) (any, error) { return e.Stats().Frames, nil })
		if err != nil {
			t.Fatal(err)
		}
		if v.(int) > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if got := radio.queuedAttempts(); len(got) != 1 {
		t.Fatalf("racing uplink dispatched %d queued attempts, want 1", len(got))
	}
	v, err := d.do(func(e *tado.EmuState) (any, error) { return e.Queue("a408"), nil })
	if err != nil {
		t.Fatal(err)
	}
	queue := v.([]tado.QueueEntry)
	if len(queue) != 2 || queue[0].Tries != 1 || !queue[0].InFlight || queue[1].Tries != 0 {
		t.Fatalf("queue=%+v, want in-flight identify followed by unsent reboot", queue)
	}
}

func TestDaemonModeAndOpenWindowNeedHeatingBlock(t *testing.T) {
	emu := tado.NewEmu(tado.EmuConfig{TxCounter: 100000}, tado.NewUnhandledLogger(io.Discard))
	d := newDaemon(defaultInstall, emu, radio.NewMock(nil), true, "mock")
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)
	srv := httptest.NewServer(d.mux())
	t.Cleanup(func() {
		srv.Close()
		cancel()
	})

	for path, body := range map[string]any{
		"mode":       map[string]any{"mode": "manual", "id": 5},
		"openwindow": map[string]any{"open": true, "id": 5},
	} {
		var response struct {
			Error string `json:"error"`
		}
		if code := postJSON(t, http.MethodPost, srv.URL+"/api/nodes/0a06/"+path, body, &response); code != http.StatusBadRequest || response.Error != tado.ErrHeatingBlockUnavailable.Error() {
			t.Fatalf("%s: got %d %+v", path, code, response)
		}
	}
	if got := emu.Pending("0a06"); got != 0 {
		t.Fatalf("pending=%d, want 0", got)
	}

	var bootstrap struct {
		Queued bool `json:"queued"`
	}
	if code := postJSON(t, http.MethodPost, srv.URL+"/api/nodes/0a06/setpoint",
		map[string]any{"tempC": 5, "sched": 5, "mode": "manual", "id": 5}, &bootstrap); code != http.StatusOK || !bootstrap.Queued {
		t.Fatalf("setpoint bootstrap: got %d %+v", code, bootstrap)
	}
	device, err := defaultInstall.Resolve("0a06")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.do(func(e *tado.EmuState) (any, error) {
		actions, ok, err := e.NextQueuedDownlink("0a06")
		if err != nil {
			return nil, err
		}
		if !ok || len(actions) != 1 {
			return nil, errors.New("setpoint bootstrap has no Data action")
		}
		action := actions[0]
		data, err := tado.ParseData(defaultInstall.NetworkKey(), action.Bytes)
		if err != nil {
			return nil, err
		}
		app := tado.ParseApp(data.Plaintext)
		if app == nil || app.CoAP == nil {
			return nil, errors.New("setpoint bootstrap has no CoAP request")
		}
		e.OnFrame(tado.BuildEnhAck(data.Seq, defaultInstall.CoordinatorEUI(), device.WireEUI(), 0))
		ack, err := tado.BuildCoAPAck("2.04", app.CoAP.MID, app.CoAP.Token, nil,
			device.WireEUI(), defaultInstall.CoordinatorEUI(), 5683, 4005)
		if err != nil {
			return nil, err
		}
		frame, err := tado.BuildData(defaultInstall.NetworkKey(), 0x61, defaultInstall.CoordinatorEUI(),
			device.WireEUI()[:5], tado.WrapAppPayload(ack, 100001, nil))
		if err != nil {
			return nil, err
		}
		e.OnFrame(frame.Bytes())
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	var queued struct {
		Queued bool `json:"queued"`
	}
	if code := postJSON(t, http.MethodPost, srv.URL+"/api/nodes/0a06/mode",
		map[string]any{"mode": "manual", "id": 5}, &queued); code != http.StatusOK || !queued.Queued {
		t.Fatalf("mode after confirmed bootstrap: got %d %+v", code, queued)
	}
}

// TestChildLockRefusedOnThermostat: the wall thermostat serves no d/lock
// resource (its firmware holds no d/lock path and it answers 4.04), so the
// child lock route answers 400 and queues nothing.
func TestChildLockRefusedOnThermostat(t *testing.T) {
	in := windowInstall(t, nil)
	d, url := newWindowDaemon(t, in, newTestClock(windowStart), nil)
	var response struct {
		Queued bool   `json:"queued"`
		Error  string `json:"error"`
	}
	code := postJSON(t, http.MethodPost, url+"/api/nodes/"+dhwNode+"/childlock", map[string]any{"on": true, "id": 1}, &response)
	if code != http.StatusBadRequest || response.Queued || !strings.Contains(response.Error, "d/lock") {
		t.Errorf("child lock on the thermostat: got %d %+v, want 400 naming d/lock", code, response)
	}
	if n := pending(t, d, wireOf(t, in, dhwNode)); n != 0 {
		t.Errorf("thermostat pending = %d, want 0", n)
	}
	response.Error = ""
	code = postJSON(t, http.MethodPost, url+"/api/nodes/"+dhwNode+"/childlock", map[string]any{"on": true}, &response)
	if code != http.StatusBadRequest || !strings.Contains(response.Error, "only a valve has a child lock") {
		t.Errorf("child lock on the thermostat without an id: got %d %+v, want 400 naming the device kind", code, response)
	}
}

// TestNodeViewChildLock: a valve reads its served d/lock state in /api/nodes
// and in the per-node route, off until the valve takes the write. A wall
// thermostat reads null.
func TestNodeViewChildLock(t *testing.T) {
	in := windowInstall(t, nil)
	d, url := newWindowDaemon(t, in, newTestClock(windowStart), nil)
	lock := func(ref string) *bool {
		t.Helper()
		var one tado.ValveView
		if code := getJSON(t, url+"/api/nodes/"+ref, &one); code != http.StatusOK {
			t.Fatalf("GET node %s: %d", ref, code)
		}
		var all []tado.ValveView
		if code := getJSON(t, url+"/api/nodes", &all); code != http.StatusOK {
			t.Fatalf("GET nodes: %d", code)
		}
		for _, v := range all {
			if v.Serial == ref {
				if (v.ChildLock == nil) != (one.ChildLock == nil) || v.ChildLock != nil && *v.ChildLock != *one.ChildLock {
					t.Fatalf("%s: list reads %v, node route reads %v", ref, v.ChildLock, one.ChildLock)
				}
				return v.ChildLock
			}
		}
		t.Fatalf("%s is missing from /api/nodes", ref)
		return nil
	}
	setLock := func(on bool) {
		t.Helper()
		var response struct {
			Queued bool `json:"queued"`
		}
		if code := postJSON(t, http.MethodPost, url+"/api/nodes/VA1000000005/childlock", map[string]any{"on": on, "id": 10}, &response); code != http.StatusOK || !response.Queued {
			t.Fatalf("child lock %t: got %d %+v", on, code, response)
		}
		deliver(t, d, in, "VA1000000005")
	}

	if got := lock("VA1000000005"); got == nil || *got {
		t.Fatalf("valve child lock before any write = %v, want false", got)
	}
	if got := lock(dhwNode); got != nil {
		t.Fatalf("thermostat child lock = %v, want null", *got)
	}
	setLock(true)
	if got := lock("VA1000000005"); got == nil || !*got {
		t.Fatalf("valve child lock after the lock write = %v, want true", got)
	}
	if got := lock("VA1000000007"); got == nil || *got {
		t.Fatalf("other valve child lock = %v, want false", got)
	}
	setLock(false)
	if got := lock("VA1000000005"); got == nil || *got {
		t.Fatalf("valve child lock after the unlock write = %v, want false", got)
	}
}

func TestDaemonPersistsDesiredState(t *testing.T) {
	in := defaultInstall
	emu := tado.NewEmu(in.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	d := newDaemon(defaultInstall, emu, radio.NewMock(nil), true, "mock")
	path := filepath.Join(t.TempDir(), "state.db")
	d.state = newTestStore(t, path)
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)
	srv := httptest.NewServer(d.mux())
	t.Cleanup(func() {
		srv.Close()
		cancel()
	})

	var response struct {
		Queued bool   `json:"queued"`
		Error  string `json:"error"`
	}
	if code := postJSON(t, http.MethodPost, srv.URL+"/api/nodes/VA1000000005/childlock",
		map[string]any{"on": true, "id": 10}, &response); code != http.StatusOK || !response.Queued {
		t.Fatalf("child lock: got %d %+v", code, response)
	}
	// The child lock is desired state once the valve takes it.
	deliver(t, d, in, "VA1000000005")
	if code := postJSON(t, http.MethodPost, srv.URL+"/api/nodes/VA1000000005/config",
		map[string]any{"tempUnitF": true, "id": 10}, &response); code != http.StatusOK || !response.Queued {
		t.Fatalf("config: got %d %+v", code, response)
	}
	if code := postJSON(t, http.MethodPost, srv.URL+"/api/boiler/flow",
		map[string]any{"tempC": 48}, &response); code != http.StatusOK || !response.Queued {
		t.Fatalf("flow: got %d %+v", code, response)
	}

	var storedVersion string
	if err := d.state.db.QueryRow(`SELECT value FROM meta WHERE key = 'state_version'`).Scan(&storedVersion); err != nil {
		t.Fatalf("stored state version: %v", err)
	}
	if want := strconv.Itoa(tado.DesiredStateVersion); storedVersion != want {
		t.Fatalf("stored state version=%s, want %s", storedVersion, want)
	}
	saved, err := d.state.Load()
	if err != nil {
		t.Fatal(err)
	}
	if saved.Version != tado.DesiredStateVersion {
		t.Fatalf("state version=%d", saved.Version)
	}

	restarted := tado.NewEmu(in.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	store, err := loadDesiredState(path, restarted)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	got := restarted.ExportDesiredState()
	bedroom, err := in.Resolve("VA1000000005")
	if err != nil {
		t.Fatal(err)
	}
	key := hex.EncodeToString(bedroom.WireEUI())
	if !got.ChildLock[key] || !got.TempUnitF[key] || got.FlowC["1"] != 48 {
		t.Errorf("restored desired state=%+v", got)
	}
	if doc, ok := got.Config[key]; !ok || len(doc.Body) == 0 || len(doc.ETag) == 0 {
		t.Errorf("restored config=%+v", doc)
	}
}

// TestDaemonDisplayEndpoint: GET reads a valve's display settings, PUT queues
// one d/config write, or a d/dbg2/tlvs write on "via": "dbg2", and answers the
// new values, and /api/nodes carries them.
func TestDaemonDisplayEndpoint(t *testing.T) {
	emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	d := newDaemon(defaultInstall, emu, radio.NewMock(nil), true, "mock")
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)
	srv := httptest.NewServer(d.mux())
	t.Cleanup(func() {
		srv.Close()
		cancel()
	})
	url := srv.URL + "/api/nodes/VA1000000005/display"

	var display tado.DisplaySettings
	if code := getJSON(t, url, &display); code != http.StatusOK ||
		display != (tado.DisplaySettings{Orientation: "horizontal", Notification: true}) {
		t.Fatalf("GET: got %d %+v", code, display)
	}
	var put struct {
		Queued  bool                 `json:"queued"`
		Pending int                  `json:"pending"`
		Display tado.DisplaySettings `json:"display"`
		Error   string               `json:"error"`
	}
	if code := postJSON(t, http.MethodPut, url, map[string]any{"orientation": "vertical", "id": 10}, &put); code != http.StatusOK ||
		!put.Queued || put.Pending != 1 || put.Display != (tado.DisplaySettings{Orientation: "vertical", Notification: true}) {
		t.Fatalf("PUT: got %d %+v", code, put)
	}
	if code := postJSON(t, http.MethodPut, url, map[string]any{"orientation": "upside", "id": 10}, &put); code != http.StatusBadRequest {
		t.Fatalf("PUT bad orientation: got %d %+v", code, put)
	}
	// A d/dbg2 write reaches the served document on the valve's 2.04 only.
	if code := postJSON(t, http.MethodPut, url, map[string]any{"notification": false, "via": "dbg2", "id": 10}, &put); code != http.StatusOK ||
		!put.Queued || put.Display != (tado.DisplaySettings{Orientation: "vertical", Notification: true}) {
		t.Fatalf("PUT via dbg2: got %d %+v", code, put)
	}
	for _, want := range []string{"d/config", "d/dbg2/tlvs"} {
		if uri, _ := deliverRequest(t, d, defaultInstall, "VA1000000005"); uri != want {
			t.Fatalf("delivered %s, want %s", uri, want)
		}
	}
	if code := getJSON(t, url, &display); code != http.StatusOK ||
		display != (tado.DisplaySettings{Orientation: "vertical", Notification: false}) {
		t.Fatalf("GET after the valve took both writes: got %d %+v", code, display)
	}
	if code := postJSON(t, http.MethodPut, url, map[string]any{"notification": true, "via": "cloud", "id": 10}, &put); code != http.StatusBadRequest {
		t.Fatalf("PUT bad via: got %d %+v", code, put)
	}
	var nodes []tado.ValveView
	if code := getJSON(t, srv.URL+"/api/nodes", &nodes); code != http.StatusOK {
		t.Fatalf("GET nodes: %d", code)
	}
	for _, n := range nodes {
		if n.Serial == "VA1000000005" {
			if n.Display == nil || n.Display.Orientation != "vertical" {
				t.Fatalf("nodes view display = %+v", n.Display)
			}
			return
		}
	}
	t.Fatal("the valve is missing from /api/nodes")
}

func TestDaemonAPI(t *testing.T) {
	frames := loadFrames(t, "../../tado/testdata/frames_69ec.txt", 10)
	if len(frames) == 0 {
		t.Fatal("no frames in the capture")
	}

	cfg := defaultInstall.EmuConfig()
	cfg.TxCounter = 100000
	emu := tado.NewEmu(cfg, tado.NewUnhandledLogger(io.Discard))
	d := newDaemon(defaultInstall, emu, radio.NewMock(frames), true, "mock")
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)
	srv := httptest.NewServer(d.mux())
	t.Cleanup(func() {
		srv.Close()
		cancel()
	})

	// The mock closes its channel, so the engine drains every frame promptly.
	var stats statsResp
	deadline := time.Now().Add(time.Second)
	for {
		getJSON(t, srv.URL+"/api/stats", &stats)
		if stats.Stats.Frames >= len(frames) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("engine consumed %d of %d frames", stats.Stats.Frames, len(frames))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if stats.Stats.Frames == 0 {
		t.Fatal("stats reports no frames")
	}
	var circuits []tado.CircuitView
	if code := getJSON(t, srv.URL+"/api/circuits", &circuits); code != http.StatusOK || len(circuits) != len(defaultInstall.Circuits) {
		t.Fatalf("circuits: got %d %+v", code, circuits)
	}

	var health healthResp
	if code := getJSON(t, srv.URL+"/api/health", &health); code != http.StatusOK {
		t.Fatalf("health: got %d", code)
	}
	if !health.OK || health.Mode != "mock" || health.TX || health.TXArmed || health.TransmitAllowed {
		t.Fatalf("health: got %+v", health)
	}
	if health.UptimeSec == nil || health.Unhandled == nil || health.Queue.Pending == nil {
		t.Fatalf("health lacks the counters: %+v", health)
	}
	if health.StartedAt == "" || health.Frames != stats.Stats.Frames {
		t.Fatalf("health: got %+v, want frames=%d", health, stats.Stats.Frames)
	}

	var valves []tado.ValveView
	if code := getJSON(t, srv.URL+"/api/nodes", &valves); code != http.StatusOK {
		t.Fatalf("nodes: got %d", code)
	}
	if len(valves) == 0 {
		t.Fatal("nodes: no valve learned from the capture")
	}
	short := ""
	for _, v := range valves {
		if tado.NodeByAny(v.Short) != nil {
			short = v.Short
			break
		}
	}
	if short == "" {
		t.Fatalf("no learned node is in the inventory: %+v", valves)
	}

	if code := getJSON(t, srv.URL+"/api/nodes/"+short, new(tado.ValveView)); code != http.StatusOK {
		t.Fatalf("nodes/%s: got %d", short, code)
	}

	var set struct {
		Queued  bool   `json:"queued"`
		Pending int    `json:"pending"`
		Error   string `json:"error"`
	}
	code := postJSON(t, http.MethodPost, srv.URL+"/api/nodes/"+short+"/setpoint",
		map[string]any{"tempC": 21, "mode": "manual", "id": 10}, &set)
	if code != http.StatusOK || !set.Queued {
		t.Fatalf("setpoint: got %d %+v", code, set)
	}

	var queue struct {
		Pending int               `json:"pending"`
		Entries []tado.QueueEntry `json:"queue"`
	}
	if code := getJSON(t, srv.URL+"/api/queue/"+short, &queue); code != http.StatusOK {
		t.Fatalf("queue: got %d", code)
	}
	if queue.Pending != 1 || len(queue.Entries) != 1 || queue.Entries[0].Kind != "setpoint" || queue.Entries[0].Desc == "" || queue.Entries[0].MID == 0 {
		t.Fatalf("queue: got %+v, want one described setpoint", queue)
	}
	deleteReq, err := http.NewRequest(http.MethodDelete, srv.URL+"/api/queue/"+short, nil)
	if err != nil {
		t.Fatal(err)
	}
	deleteResp, err := http.DefaultClient.Do(deleteReq)
	if err != nil {
		t.Fatal(err)
	}
	defer deleteResp.Body.Close()
	var deleted struct {
		Dropped int `json:"dropped"`
	}
	if err := json.NewDecoder(deleteResp.Body).Decode(&deleted); err != nil {
		t.Fatal(err)
	}
	if deleteResp.StatusCode != http.StatusOK || deleted.Dropped != 1 {
		t.Fatalf("delete queue: got %d %+v", deleteResp.StatusCode, deleted)
	}
	var afterDelete struct {
		Pending int                    `json:"pending"`
		History []tado.DownlinkOutcome `json:"history"`
	}
	if code := getJSON(t, srv.URL+"/api/queue/"+short, &afterDelete); code != http.StatusOK {
		t.Fatalf("queue after delete: got %d", code)
	}
	if afterDelete.Pending != 0 || len(afterDelete.History) != 1 ||
		afterDelete.History[0].Result != "withdrawn" || afterDelete.History[0].Reason != "operator" {
		t.Fatalf("queue history after delete: got %+v", afterDelete)
	}

	bedroom := tado.NodeByAny("bedroom")
	var boostBefore struct {
		Pending int `json:"pending"`
	}
	if code := getJSON(t, srv.URL+"/api/queue/"+bedroom.Short, &boostBefore); code != http.StatusOK {
		t.Fatalf("boost queue: got %d", code)
	}
	var boost struct {
		Queued  bool `json:"queued"`
		Pending int  `json:"pending"`
	}
	code = postJSON(t, http.MethodPost, srv.URL+"/api/nodes/"+bedroom.Short+"/boost",
		map[string]any{"on": true, "id": 10}, &boost)
	if code != http.StatusOK || !boost.Queued || boost.Pending != boostBefore.Pending+1 {
		t.Fatalf("boost: got %d %+v", code, boost)
	}
	var boostAfter struct {
		Pending int `json:"pending"`
	}
	if code := getJSON(t, srv.URL+"/api/queue/"+bedroom.Short, &boostAfter); code != http.StatusOK || boostAfter.Pending != boostBefore.Pending+1 {
		t.Fatalf("boost queue: got %d %+v", code, boostAfter)
	}
}

func TestDaemonBeaconTicker(t *testing.T) {
	emu := tado.NewEmu(tado.EmuConfig{
		TxCounter:            100000,
		RouterAdvertInterval: 5 * time.Millisecond,
	}, tado.NewUnhandledLogger(io.Discard))
	radio := radio.NewMock(nil)
	d := newDaemon(defaultInstall, emu, radio, false, "mock")
	d.beaconEvery = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go d.run(ctx)
	want := tado.RouterAdvertWakeUpCount + 1
	deadline := time.Now().Add(5 * time.Second)
	for len(radio.Sent()) < want && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	sent := radio.Sent()
	if len(sent) < want {
		t.Fatalf("daemon emitted %d frames, want at least %d", len(sent), want)
	}
	if got := tado.Classify(sent[tado.RouterAdvertWakeUpCount]); got != tado.ClassBroadcast {
		t.Errorf("beacon class=%v, want broadcast", got)
	}
}

func TestDaemonDryRunReleasesRouterAdvert(t *testing.T) {
	emu := tado.NewEmu(tado.EmuConfig{RouterAdvertInterval: time.Minute}, tado.NewUnhandledLogger(io.Discard))
	d := newDaemon(defaultInstall, emu, radio.NewMock(nil), true, "mock")
	device, err := defaultInstall.Resolve("VA1000000005")
	if err != nil {
		t.Fatal(err)
	}
	frame, err := tado.BuildBroadcast(defaultInstall.NetworkKey(), 0x21, device.WireEUI(), 9, 0x04,
		tado.WrapBroadcastPayload([]byte{0x7b, 0x3b, 58, 0x01, 133, 0, 0, 0}, 0))
	if err != nil {
		t.Fatal(err)
	}
	d.frame(frame)
	actions, due, err := emu.MaybeBeacon()
	if err != nil || !due || len(actions) != tado.RouterAdvertWakeUpCount+1 {
		t.Fatalf("dry-run left beacon pending: actions=%d due=%v err=%v", len(actions), due, err)
	}
	emu.CompleteBeacon(false)
	if got := emu.Stats().RouterAdvertisementsSent; got != 0 {
		t.Errorf("RouterAdvertisementsSent=%d, want 0 in dry-run", got)
	}
}

func TestDaemonBoostExplicitZeroID(t *testing.T) {
	emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	radio := radio.NewMock(nil)
	d := newDaemon(defaultInstall, emu, radio, true, "mock")
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)
	srv := httptest.NewServer(d.mux())
	t.Cleanup(func() {
		srv.Close()
		cancel()
	})

	ru, err := defaultInstall.Resolve(dhwNode)
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Error string `json:"error"`
	}
	if code := postJSON(t, http.MethodPost, srv.URL+"/api/nodes/"+ru.Short()+"/boost",
		map[string]any{"on": true, "id": 0}, &response); code != http.StatusBadRequest || response.Error != "boost requires a heating zone" {
		t.Fatalf("boost id=0: got %d %+v", code, response)
	}
	if pending := emu.Pending(ru.Short()); pending != 0 {
		t.Fatalf("boost queued %d requests", pending)
	}
	if sent := len(radio.Sent()); sent != 0 {
		t.Fatalf("boost transmitted %d frames", sent)
	}
}

func TestDaemonBoostQueuesValveWithoutTXInDryRun(t *testing.T) {
	emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	radio := radio.NewMock(nil)
	d := newDaemon(defaultInstall, emu, radio, true, "mock")
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)
	srv := httptest.NewServer(d.mux())
	t.Cleanup(func() {
		srv.Close()
		cancel()
	})

	valve, err := defaultInstall.Resolve("0a06")
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Error string `json:"error"`
	}
	if code := postJSON(t, http.MethodPost, srv.URL+"/api/nodes/"+valve.Short()+"/boost",
		map[string]any{"on": true, "id": 5}, &response); code != http.StatusOK || response.Error != "" {
		t.Fatalf("valve boost: got %d %+v", code, response)
	}
	if pending := emu.Pending(valve.Short()); pending != 1 {
		t.Fatalf("valve boost queued %d requests", pending)
	}
	if sent := len(radio.Sent()); sent != 0 {
		t.Fatalf("valve boost transmitted %d frames", sent)
	}
}

// TestDaemonBoilerDHW: the hot-water endpoint queues against the wall
// thermostat without needing a node in the path, and rejects an out-of-range
// setpoint instead of letting the device silently clamp it.
func TestDaemonBoilerDHW(t *testing.T) {
	emu := tado.NewEmu(tado.EmuConfig{TxCounter: 100000}, tado.NewUnhandledLogger(io.Discard))
	d := newDaemon(defaultInstall, emu, radio.NewMock(nil), true, "mock")
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)
	srv := httptest.NewServer(d.mux())
	t.Cleanup(func() {
		srv.Close()
		cancel()
	})

	var set struct {
		Queued  bool   `json:"queued"`
		Pending int    `json:"pending"`
		Target  string `json:"target"`
		ID      int    `json:"id"`
		On      bool   `json:"on"`
		Error   string `json:"error"`
	}
	code := postJSON(t, http.MethodPost, srv.URL+"/api/boiler/dhw",
		map[string]any{"tempC": 53}, &set)
	if code != http.StatusOK || !set.Queued {
		t.Fatalf("boiler/dhw: got %d %+v", code, set)
	}
	if set.Target != dhwNode || set.ID != dhwZoneID {
		t.Errorf("boiler/dhw: target=%q id=%d, want %q %d", set.Target, set.ID, dhwNode, dhwZoneID)
	}
	if set.Pending != 1 {
		t.Errorf("boiler/dhw: pending=%d, want 1", set.Pending)
	}
	if !set.On {
		t.Error("boiler/dhw default on=false, want true")
	}

	set = struct {
		Queued  bool   `json:"queued"`
		Pending int    `json:"pending"`
		Target  string `json:"target"`
		ID      int    `json:"id"`
		On      bool   `json:"on"`
		Error   string `json:"error"`
	}{}
	code = postJSON(t, http.MethodPost, srv.URL+"/api/boiler/dhw",
		map[string]any{"tempC": 53, "id": 0}, &set)
	if code != http.StatusOK || !set.Queued || set.ID != 0 {
		t.Fatalf("boiler/dhw explicit id 0: got %d %+v", code, set)
	}

	set = struct {
		Queued  bool   `json:"queued"`
		Pending int    `json:"pending"`
		Target  string `json:"target"`
		ID      int    `json:"id"`
		On      bool   `json:"on"`
		Error   string `json:"error"`
	}{}
	code = postJSON(t, http.MethodPost, srv.URL+"/api/boiler/dhw",
		map[string]any{"tempC": 53, "id": 1}, &set)
	if code != http.StatusBadRequest || set.Error == "" {
		t.Fatalf("boiler/dhw wrong zone: got %d %+v", code, set)
	}

	set = struct {
		Queued  bool   `json:"queued"`
		Pending int    `json:"pending"`
		Target  string `json:"target"`
		ID      int    `json:"id"`
		On      bool   `json:"on"`
		Error   string `json:"error"`
	}{}
	code = postJSON(t, http.MethodPost, srv.URL+"/api/boiler/dhw",
		map[string]any{"on": false}, &set)
	if code != http.StatusOK || !set.Queued || set.On {
		t.Fatalf("boiler/dhw off: got %d %+v", code, set)
	}

	set = struct {
		Queued  bool   `json:"queued"`
		Pending int    `json:"pending"`
		Target  string `json:"target"`
		ID      int    `json:"id"`
		On      bool   `json:"on"`
		Error   string `json:"error"`
	}{}
	code = postJSON(t, http.MethodPost, srv.URL+"/api/boiler/dhw",
		map[string]any{"on": true}, &set)
	if code != http.StatusBadRequest || set.Error == "" {
		t.Fatalf("boiler/dhw enabled without tempC: got %d %+v", code, set)
	}

	set = struct {
		Queued  bool   `json:"queued"`
		Pending int    `json:"pending"`
		Target  string `json:"target"`
		ID      int    `json:"id"`
		On      bool   `json:"on"`
		Error   string `json:"error"`
	}{}
	code = postJSON(t, http.MethodPost, srv.URL+"/api/boiler/dhw",
		map[string]any{"tempC": 90}, &set)
	if code != http.StatusBadRequest || set.Error == "" {
		t.Fatalf("90 C should be refused, got %d %+v", code, set)
	}
}

func TestDaemonDHWUsesCoAPTerminalDelivery(t *testing.T) {
	clock := newManualRetryClock()
	radio := newImmediateTestRadio(true, nil)
	now := time.Unix(100, 0)
	d, url := newImmediateDaemonWithRetry(t, tado.EmuConfig{
		TxCounter: 100000, NoAutoQueue: true, Now: func() time.Time { return now },
	}, radio, false, clock.after, fixedRetryClock(&now))

	var response struct {
		Queued  bool `json:"queued"`
		Pending int  `json:"pending"`
	}
	if code := postJSON(t, http.MethodPost, url+"/api/boiler/dhw", map[string]any{"on": false}, &response); code != http.StatusOK || !response.Queued || response.Pending != 1 {
		t.Fatalf("DHW OFF: got %d %+v", code, response)
	}
	attempts := waitForQueuedAttempts(t, radio, 1)
	if len(attempts) != 1 || !attempts[0].WaitResponse || !attempts[0].CoAPTransaction {
		t.Fatalf("attempts=%+v, want one CoAP-confirmed DHW write", attempts)
	}
	first, err := tado.ParseData(defaultInstall.NetworkKey(), attempts[0].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	firstReq := tado.ParseApp(first.Plaintext).CoAP
	if firstReq == nil || firstReq.URI() != "z/s?id=0" {
		t.Fatalf("DHW request=%+v, want z/s?id=0", firstReq)
	}
	ru, err := defaultInstall.Resolve(dhwNode)
	if err != nil {
		t.Fatal(err)
	}
	radio.frames <- tado.BuildEnhAck(first.Seq, defaultInstall.CoordinatorEUI(), ru.WireEUI(), 0)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		v, err := d.do(func(e *tado.EmuState) (any, error) { return e.Stats().EnhAcks, nil })
		if err != nil {
			t.Fatal(err)
		}
		if v.(int) != 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	v, err := d.do(func(e *tado.EmuState) (any, error) { return e.Pending(dhwNode), nil })
	if err != nil {
		t.Fatal(err)
	}
	if v.(int) != 1 {
		t.Fatalf("Enhanced ACK completed DHW write: pending=%d", v.(int))
	}
	if got := clock.wait(t); got != coapRetryMin {
		t.Fatalf("DHW retry delay=%s, want %s", got, coapRetryMin)
	}
	now = now.Add(coapRetryMin)
	clock.tick <- now
	attempts = waitForQueuedAttempts(t, radio, 2)
	second, err := tado.ParseData(defaultInstall.NetworkKey(), attempts[1].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	secondReq := tado.ParseApp(second.Plaintext).CoAP
	if secondReq == nil || secondReq.MID != firstReq.MID || !bytes.Equal(secondReq.Token, firstReq.Token) {
		t.Fatalf("DHW retry=%+v, want MID/token %04x/%x", secondReq, firstReq.MID, firstReq.Token)
	}
	if second.Seq == first.Seq || bytes.Equal(second.Plaintext[4:8], first.Plaintext[4:8]) {
		t.Fatal("DHW retry reused MAC sequence number or nonce counter")
	}
	ack, err := tado.BuildCoAPAck("2.04", firstReq.MID, firstReq.Token, nil,
		ru.WireEUI(), defaultInstall.CoordinatorEUI(), 5683, 4005)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := tado.BuildData(defaultInstall.NetworkKey(), 0x61, defaultInstall.CoordinatorEUI(), ru.WireEUI()[:5], tado.WrapAppPayload(ack, 701, nil))
	if err != nil {
		t.Fatal(err)
	}
	radio.frames <- frame.Bytes()
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		v, err = d.do(func(e *tado.EmuState) (any, error) { return e.Pending(dhwNode), nil })
		if err != nil {
			t.Fatal(err)
		}
		if v.(int) == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("matching DHW 2.04 did not complete queue")
}

// TestDaemonBoilerFlow: the CH flow endpoint registers the served setpoint on the
// wall thermostat (default circuit 1) and rejects an out-of-range value.
func TestDaemonBoilerFlow(t *testing.T) {
	emu := tado.NewEmu(tado.EmuConfig{TxCounter: 100000}, tado.NewUnhandledLogger(io.Discard))
	d := newDaemon(defaultInstall, emu, radio.NewMock(nil), true, "mock")
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)
	srv := httptest.NewServer(d.mux())
	t.Cleanup(func() {
		srv.Close()
		cancel()
	})

	var set struct {
		Queued  bool   `json:"queued"`
		Pending int    `json:"pending"`
		Target  string `json:"target"`
		ID      int    `json:"id"`
		Error   string `json:"error"`
	}
	code := postJSON(t, http.MethodPost, srv.URL+"/api/boiler/flow",
		map[string]any{"tempC": 48}, &set)
	if code != http.StatusOK || !set.Queued {
		t.Fatalf("boiler/flow: got %d %+v", code, set)
	}
	if set.Target != dhwNode || set.ID != flowCircuitID {
		t.Errorf("boiler/flow: target=%q id=%d, want %q %d", set.Target, set.ID, dhwNode, flowCircuitID)
	}
	if set.Pending != 1 {
		t.Errorf("boiler/flow: pending=%d, want 1", set.Pending)
	}

	set.Error = ""
	code = postJSON(t, http.MethodPost, srv.URL+"/api/boiler/flow",
		map[string]any{"tempC": 90}, &set)
	if code != http.StatusBadRequest || set.Error == "" {
		t.Fatalf("90 C should be refused, got %d %+v", code, set)
	}
}

// TestDaemonHoldsDelayedAction: an action carrying NotBefore does not go on air
// until that instant, and the engine keeps running while it waits. This is what
// turns EmuConfig.TimeDelay into an answer the device actually receives late
// (test F12) rather than one it never receives.
func TestDaemonHoldsDelayedAction(t *testing.T) {
	emu := tado.NewEmu(tado.EmuConfig{TxCounter: 100000}, tado.NewUnhandledLogger(io.Discard))
	radio := radio.NewMock(nil)
	d := newDaemon(defaultInstall, emu, radio, false, "mock")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go d.run(ctx)

	hold := 150 * time.Millisecond
	d.do(func(*tado.EmuState) (any, error) {
		d.tx(tado.Action{
			Kind: tado.ActData, Bytes: []byte{1, 2, 3}, Reason: "held",
			NotBefore: time.Now().Add(hold),
		})
		// A second action decided right after must not queue behind the first:
		// holding one answer must not stall the rendezvous of everything else.
		d.tx(tado.Action{Kind: tado.ActData, Bytes: []byte{4, 5, 6}, Reason: "immediate"})
		return nil, nil
	})

	sent := radio.Sent()
	if len(sent) != 1 || !bytes.Equal(sent[0], []byte{4, 5, 6}) {
		t.Fatalf("before the deadline the radio saw %v, want only the immediate frame", sent)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(radio.Sent()) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the held action never went out")
		}
		time.Sleep(5 * time.Millisecond)
	}
	sent = radio.Sent()
	if !bytes.Equal(sent[1], []byte{1, 2, 3}) {
		t.Errorf("second frame %x, want the held one", sent[1])
	}
}

func TestLiveRadioBatchesImmediateAndAsyncTX(t *testing.T) {
	device, err := defaultInstall.Resolve("VA1000000005")
	if err != nil {
		t.Fatal(err)
	}
	pkt, err := tado.BuildCoAP(tado.CoAPReq{
		Type: "CON", Code: "0.01", MID: 0x6110, Token: []byte{0x51},
		Path:   []string{"h", "7654321", "d", "time"},
		SrcEUI: device.WireEUI(), DstEUI: defaultInstall.CoordinatorEUI(),
		SrcPort: 5683, DstPort: 4005,
	})
	if err != nil {
		t.Fatal(err)
	}
	uplink, err := tado.BuildData(defaultInstall.NetworkKey(), 0x21, defaultInstall.CoordinatorEUI(),
		device.WireEUI()[:5], tado.WrapAppPayload(pkt, 700, nil))
	if err != nil {
		t.Fatal(err)
	}
	bridge := &orderedLiveBridge{result: &cc1101.AutoAckResult{
		Status: cc1101.AutoAckSent,
		Frame:  &cc1101.RxFrame{Payload: uplink.Bytes(), CRCOK: true},
	}}
	live, err := radio.StartLive(bridge, defaultInstall.CoordinatorEUI(), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	d := newDaemon(defaultInstall, emu, live, false, "live")
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)
	t.Cleanup(func() {
		cancel()
		if err := live.Close(); err != nil {
			t.Error(err)
		}
	})

	deadline := time.Now().Add(time.Second)
	for {
		events, trains, _ := bridge.snapshot()
		csl := bridge.cslSnapshot()
		if len(events) >= 3 && len(csl) == 1 {
			if events[0] != "recv" || events[1] != "csl" || events[2] != "recv" {
				t.Fatalf("order = %v, want recv csl recv", events[:3])
			}
			if len(trains) != 0 {
				t.Fatalf("generic immediate trains=%d, want 0", len(trains))
			}
			if tado.Classify(csl[0]) != tado.ClassData {
				t.Fatalf("CSL frame class=%v, want Data", tado.Classify(csl[0]))
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("owner did not send CSL immediate TX: events=%v trains=%d", events, len(trains))
		}
		time.Sleep(time.Millisecond)
	}

	if err := live.Send([]byte{0x49, 0xe8, 0x01}); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(time.Second)
	for {
		_, trains, _ := bridge.snapshot()
		if len(trains) == 1 {
			if got := trains[0]; len(got) != 1 || !bytes.Equal(got[0], []byte{0x49, 0xe8, 0x01}) {
				t.Fatalf("async batch = %x", got)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("owner did not serialize async TX")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestLiveRadioReinjectsCSLEnhancedAck(t *testing.T) {
	device, err := defaultInstall.Resolve("VA1000000005")
	if err != nil {
		t.Fatal(err)
	}
	pkt, err := tado.BuildCoAP(tado.CoAPReq{
		Type: "NON", Code: "0.01", MID: 0x6110, Token: []byte{0x51},
		Path:   []string{"h", "7654321", "d", "time"},
		SrcEUI: device.WireEUI(), DstEUI: defaultInstall.CoordinatorEUI(),
		SrcPort: 5683, DstPort: 4005,
	})
	if err != nil {
		t.Fatal(err)
	}
	uplink, err := tado.BuildData(defaultInstall.NetworkKey(), 0x21, defaultInstall.CoordinatorEUI(),
		device.WireEUI()[:5], tado.WrapAppPayload(pkt, 700, nil))
	if err != nil {
		t.Fatal(err)
	}
	bridge := &orderedLiveBridge{cslAutoAck: true, result: &cc1101.AutoAckResult{
		Status: cc1101.AutoAckSent,
		Frame:  &cc1101.RxFrame{Payload: uplink.Bytes(), CRCOK: true},
	}}
	live, err := radio.StartLive(bridge, defaultInstall.CoordinatorEUI(), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	cfg := defaultInstall.EmuConfig()
	cfg.NoCoAPAck = true
	emu := tado.NewEmu(cfg, tado.NewUnhandledLogger(io.Discard))
	if err := emu.EnqueueSetpoint(device.Serial, tado.SetpointParams{ID: 10, Setpoint: 5, Sched: 5, Overlay: 2}); err != nil {
		t.Fatal(err)
	}
	d := newDaemon(defaultInstall, emu, live, false, "live")
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)
	t.Cleanup(func() {
		cancel()
		_ = live.Close()
	})
	deadline := time.Now().Add(time.Second)
	for len(bridge.cslSnapshot()) != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := len(bridge.cslSnapshot()); got != 1 {
		t.Fatalf("CSL sends=%d, want 1", got)
	}
	deadline = time.Now().Add(time.Second)
	for {
		events, trains, _ := bridge.snapshot()
		if len(events) >= 3 && events[0] == "recv" && events[1] == "csl" && events[2] == "recv" {
			if len(trains) != 0 {
				t.Fatalf("reinject sent generic trains=%x", trains)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reinject stalled owner: events=%v", events)
		}
		time.Sleep(time.Millisecond)
	}
	v, err := d.do(func(e *tado.EmuState) (any, error) {
		return struct {
			stats   tado.EmuStats
			pending int
		}{e.Stats(), e.Pending(device.Serial)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got := v.(struct {
		stats   tado.EmuStats
		pending int
	})
	if got.stats.EnhAcks != 2 || got.stats.DownlinksDelivered != 0 || got.pending != 1 {
		t.Fatalf("ack=%d delivery=%d pending=%d, want 2/0/1", got.stats.EnhAcks, got.stats.DownlinksDelivered, got.pending)
	}
}

func TestLiveRadioTopLevelEmptyFrameCompletesReceiveBatch(t *testing.T) {
	device, err := defaultInstall.Resolve("VA1000000005")
	if err != nil {
		t.Fatal(err)
	}
	// A Neighbor Solicitation is decoded and answered by nothing, so the batch
	// this frame opens stays empty.
	ns, err := tado.BuildBroadcast(defaultInstall.NetworkKey(), 0x21, device.WireEUI(), 9, 0x04,
		tado.WrapBroadcastPayload([]byte{0x7b, 0x3b, 58, 0x01, 135, 0, 0, 0}, 0))
	if err != nil {
		t.Fatal(err)
	}
	bridge := &orderedLiveBridge{result: &cc1101.AutoAckResult{
		Status: cc1101.AutoAckFrameNoAck,
		Frame:  &cc1101.RxFrame{Payload: ns, CRCOK: true},
	}}
	live, err := radio.StartLive(bridge, defaultInstall.CoordinatorEUI(), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	d := newDaemon(defaultInstall, emu, live, false, "live")
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)
	t.Cleanup(func() {
		cancel()
		_ = live.Close()
	})
	deadline := time.Now().Add(time.Second)
	for {
		events, trains, _ := bridge.snapshot()
		if len(events) >= 2 && events[0] == "recv" && events[1] == "recv" {
			if len(trains) != 0 {
				t.Fatalf("empty frame sent trains=%x", trains)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("owner did not resume receive: events=%v", events)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestDaemonRecoversQueuedCSLWhenLiveOwnerEnds(t *testing.T) {
	boom := errors.New("receive failed")
	bridge := &endingQueuedLiveBridge{
		queuedLiveBridge: &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}},
		err:              boom,
	}
	live, err := radio.StartLive(bridge, defaultInstall.CoordinatorEUI(), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	d, url := newImmediateDaemon(t, tado.EmuConfig{TxCounter: 100000, NoAutoQueue: true}, live, false)

	var queued struct {
		Queued bool `json:"queued"`
	}
	if code := postJSON(t, http.MethodPost, url+"/api/nodes/a408/identify", map[string]any{"id": 3}, &queued); code != http.StatusOK || !queued.Queued {
		t.Fatalf("identify: got %d %+v", code, queued)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		v, err := d.do(func(e *tado.EmuState) (any, error) {
			return struct {
				queue    []tado.QueueEntry
				awaiting bool
			}{e.Queue("a408"), e.QueuedDownlinkAwaitingCSL("a408")}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		state := v.(struct {
			queue    []tado.QueueEntry
			awaiting bool
		})
		if len(state.queue) == 1 && state.queue[0].Tries == 0 && state.queue[0].InFlight && !state.awaiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queue remained stranded: %+v awaiting=%t", state.queue, state.awaiting)
		}
		time.Sleep(time.Millisecond)
	}

	// A dead radio loop fails the container health check, and Main leaves
	// non-zero so a supervisor restarts the process.
	var health struct {
		OK              bool   `json:"ok"`
		TXArmed         bool   `json:"txArmed"`
		TransmitAllowed bool   `json:"transmitAllowed"`
		RadioError      string `json:"radioError"`
	}
	if code := getJSON(t, url+"/api/health", &health); code != http.StatusServiceUnavailable {
		t.Fatalf("health status=%d", code)
	}
	if health.OK || health.TXArmed || health.TransmitAllowed || !strings.Contains(health.RadioError, boom.Error()) {
		t.Fatalf("health=%+v", health)
	}
	select {
	case err := <-d.radioLost:
		if !strings.Contains(err.Error(), boom.Error()) {
			t.Fatalf("radio lost=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the engine did not report the ended radio loop to Main")
	}
}

func TestLiveRadioHandlesNonemptyFollowBetweenReceives(t *testing.T) {
	device, err := defaultInstall.Resolve("VA1000000005")
	if err != nil {
		t.Fatal(err)
	}
	pkt, err := tado.BuildCoAP(tado.CoAPReq{
		Type: "CON", Code: "0.01", MID: 0x6110, Token: []byte{0x51},
		Path:   []string{"h", "7654321", "d", "time"},
		SrcEUI: device.WireEUI(), DstEUI: defaultInstall.CoordinatorEUI(), SrcPort: 5683, DstPort: 4005,
	})
	if err != nil {
		t.Fatal(err)
	}
	follow, err := tado.BuildData(defaultInstall.NetworkKey(), 0x21, defaultInstall.CoordinatorEUI(), device.WireEUI()[:5], tado.WrapAppPayload(pkt, 700, nil))
	if err != nil {
		t.Fatal(err)
	}
	// The bubble lets the test release the receive only once the follow action
	// waits in the radio queue.
	synctest.Test(t, func(t *testing.T) {
		entered := make(chan struct{}, 1)
		release := make(chan struct{})
		bridge := &orderedLiveBridge{result: &cc1101.AutoAckResult{Status: cc1101.AutoAckNoFrame}, entered: entered, release: release}
		live, err := radio.StartLive(bridge, defaultInstall.CoordinatorEUI(), 0, true)
		if err != nil {
			t.Fatal(err)
		}
		cfg := defaultInstall.EmuConfig()
		cfg.NoAutoQueue = true
		d := newDaemon(defaultInstall, tado.NewEmu(cfg, tado.NewUnhandledLogger(io.Discard)), live, false, "live")
		t.Cleanup(func() { _ = live.Close() })
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("owner did not enter receive")
		}
		done := make(chan struct{}, 1)
		go func() {
			d.handleFrame(follow.Bytes(), false)
			done <- struct{}{}
		}()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("follow action was not queued")
		default:
		}
		close(release)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("follow action did not complete")
		}
		deadline := time.Now().Add(time.Second)
		for {
			events, _, _ := bridge.snapshot()
			if len(events) >= 3 && events[0] == "recv" && events[1] == "csl" && events[2] == "recv" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("follow action order=%v", events)
			}
			time.Sleep(time.Millisecond)
		}
	})
}

func TestLiveRadioBridgeFollowEmptyArmsGate(t *testing.T) {
	bridgeFrame, _ := goldenPair(t)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	bridge := &orderedLiveBridge{result: &cc1101.AutoAckResult{Status: cc1101.AutoAckNoFrame}, entered: entered, release: release}
	live, err := radio.StartLive(bridge, defaultInstall.CoordinatorEUI(), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	d := newDaemon(defaultInstall, tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard)), live, false, "live")
	t.Cleanup(func() { _ = live.Close() })
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("owner did not enter receive")
	}
	d.handleFrame(bridgeFrame, false)
	close(release)
	deadline := time.Now().Add(time.Second)
	for {
		events, trains, _ := bridge.snapshot()
		if len(events) >= 2 && events[0] == "recv" && events[1] == "recv-fw" {
			if len(trains) != 0 {
				t.Fatalf("bridge follow sent trains=%x", trains)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("bridge follow did not arm passive receive: events=%v", events)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestFrameRevertsQueuedDownlinkWhenEarlierCSLTxFails(t *testing.T) {
	device, err := defaultInstall.Resolve("VA1000000005")
	if err != nil {
		t.Fatal(err)
	}
	pkt, err := tado.BuildCoAP(tado.CoAPReq{
		Type: "CON", Code: "0.01", MID: 0x6110, Token: []byte{0x51},
		Path:   []string{"h", "7654321", "d", "time"},
		SrcEUI: device.WireEUI(), DstEUI: defaultInstall.CoordinatorEUI(),
		SrcPort: 5683, DstPort: 4005,
	})
	if err != nil {
		t.Fatal(err)
	}
	uplink, err := tado.BuildData(defaultInstall.NetworkKey(), 0x21, defaultInstall.CoordinatorEUI(),
		device.WireEUI()[:5], tado.WrapAppPayload(pkt, 700, nil))
	if err != nil {
		t.Fatal(err)
	}
	emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	if err := emu.EnqueueGet(device.Serial, "d/fw/state", []string{"d", "fw", "state"}, nil); err != nil {
		t.Fatal(err)
	}
	radio := &failingImmediateRadio{}
	d := newDaemon(defaultInstall, emu, radio, false, "live")
	d.frame(uplink.Bytes())
	if len(radio.actions) != 2 || radio.actions[0].QueueAttempt || !radio.actions[1].QueueAttempt {
		t.Fatalf("actions=%+v, want CoAP response then queued downlink", radio.actions)
	}
	queued := emu.Queue(device.Serial)
	if len(queued) != 1 || queued[0].Tries != 0 || queued[0].InFlight {
		t.Fatalf("queue=%+v, want unsubmitted GET", queued)
	}
	if got := emu.Stats().DownlinksSent; got != 0 {
		t.Fatalf("downlinks sent=%d, want 0", got)
	}
}

func TestQueuedCSLCompletionAccountsOnlySentFrames(t *testing.T) {
	device, err := defaultInstall.Resolve("VA1000000005")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 20, 10, 47, 56, 0, time.UTC)
	cfg := defaultInstall.EmuConfig()
	cfg.Now = func() time.Time { return now }
	emu := tado.NewEmu(cfg, tado.NewUnhandledLogger(io.Discard))
	if err := emu.EnqueueGet(device.Serial, "d/fw/state", []string{"d", "fw", "state"}, nil); err != nil {
		t.Fatal(err)
	}
	actions, ok, err := emu.NextQueuedDownlink(device.Serial)
	if err != nil || !ok || len(actions) != 1 {
		t.Fatalf("actions=%+v ok=%t err=%v", actions, ok, err)
	}
	d := newDaemon(defaultInstall, emu, &immediateTestRadio{}, false, "live")
	clock := newManualRetryClock()
	d.retryAfter, d.retryNow = clock.after, func() time.Time { return now }
	t.Cleanup(func() { close(d.done) })
	d.handleCSLCompletion(radio.CSLCompletion{Action: actions[0], Result: &cc1101.CSLTxResult{Sent: 0}})
	queued := emu.Queue(device.Serial)
	if len(queued) != 1 || queued[0].Tries != 0 || !queued[0].InFlight {
		t.Fatalf("zero-send queue=%+v", queued)
	}
	now = now.Add(proactiveDownlinkRetryBackoff)
	actions, ok, err = emu.NextQueuedDownlink(device.Serial)
	if err != nil || !ok || len(actions) != 1 {
		t.Fatalf("retry actions=%+v ok=%t err=%v", actions, ok, err)
	}
	d.handleCSLCompletion(radio.CSLCompletion{Action: actions[0], Result: &cc1101.CSLTxResult{Sent: 23}})
	queued = emu.Queue(device.Serial)
	if len(queued) != 1 || queued[0].Tries != 1 || !queued[0].InFlight {
		t.Fatalf("sent queue=%+v", queued)
	}
}

func TestDaemonCSLCompletionAccountsBeforeItsEnhancedAck(t *testing.T) {
	device, err := defaultInstall.Resolve("VA1000000005")
	if err != nil {
		t.Fatal(err)
	}
	emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	if err := emu.EnqueueChildLock(device.Serial, 5, true); err != nil {
		t.Fatal(err)
	}
	if err := emu.EnqueueChildLock(device.Serial, 5, false); err != nil {
		t.Fatal(err)
	}
	first, ok, err := emu.NextQueuedDownlink(device.Serial)
	if err != nil || !ok || len(first) == 0 {
		t.Fatalf("first=%+v ok=%t err=%v", first, ok, err)
	}
	link := &completionRadio{frames: make(chan []byte, 1), cslDone: make(chan radio.CSLCompletion, 1)}
	d := newDaemon(defaultInstall, emu, link, false, "live")
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)
	t.Cleanup(cancel)
	ack := tado.BuildEnhAck(first[0].Bytes[2], defaultInstall.CoordinatorEUI(), device.WireEUI(), 0)
	link.cslDone <- radio.CSLCompletion{
		Action: first[0], Result: &cc1101.CSLTxResult{Sent: 23},
		Frames: [][]byte{ack, {0x69, 0xec, 0x7f}},
	}

	deadline := time.Now().Add(time.Second)
	for {
		v, err := d.do(func(e *tado.EmuState) (any, error) { return e.Queue(device.Serial), nil })
		if err != nil {
			t.Fatal(err)
		}
		queue := v.([]tado.QueueEntry)
		if len(queue) == 1 && queue[0].Tries == 0 && !queue[0].InFlight {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queue after completion/ACK=%+v", queue)
		}
		time.Sleep(time.Millisecond)
	}
	type nextResult struct {
		actions []tado.Action
		ok      bool
	}
	v, err := d.do(func(e *tado.EmuState) (any, error) {
		actions, ok, err := e.NextQueuedDownlink(device.Serial)
		return nextResult{actions: actions, ok: ok}, err
	})
	if err != nil {
		t.Fatal(err)
	}
	second := v.(nextResult)
	if !second.ok || len(second.actions) == 0 || second.actions[0].QueueTry != 1 {
		t.Fatalf("second=%+v", second)
	}
	if d.retryMID[device.Serial] == second.actions[0].QueueMID {
		t.Fatalf("stale retry targets q2 mid=%d", second.actions[0].QueueMID)
	}
}

func TestQueuedCSLNoAnchorWaitsForRetryDeadline(t *testing.T) {
	device, err := defaultInstall.Resolve("VA1000000005")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 20, 10, 47, 56, 0, time.UTC)
	cfg := defaultInstall.EmuConfig()
	cfg.Now = func() time.Time { return now }
	emu := tado.NewEmu(cfg, tado.NewUnhandledLogger(io.Discard))
	if err := emu.EnqueueChildLock(device.Serial, 5, true); err != nil {
		t.Fatal(err)
	}
	immediate := newImmediateTestRadio(false, nil)
	immediate.cslQueue, immediate.accept = true, true
	d := newDaemon(defaultInstall, emu, immediate, false, "live")
	clock := newManualRetryClock()
	d.retryAfter, d.retryNow = clock.after, func() time.Time { return now }
	t.Cleanup(func() { close(d.done) })

	d.dispatchQueued(emu, device.Serial)
	attempts := immediate.queuedAttempts()
	if len(attempts) != 1 || attempts[0].QueueTry != 1 {
		t.Fatalf("initial attempts=%+v", attempts)
	}
	firstSeq, firstCounter := attempts[0].Bytes[2], emu.TxCounter()
	for range 3 {
		d.requestQueuedDispatch(device.Serial)
		d.dispatchQ = d.dispatchQ[1:]
		delete(d.dispatchDue, device.Serial)
		d.dispatchQueued(emu, device.Serial)
		if actions, ok, err := emu.NextQueuedDownlink(device.Serial); err != nil || ok || len(actions) != 0 {
			t.Fatalf("inbound before completion actions=%+v ok=%t err=%v", actions, ok, err)
		}
	}
	if got := immediate.queuedAttempts(); len(got) != 1 || got[0].Bytes[2] != firstSeq || emu.TxCounter() != firstCounter {
		t.Fatalf("admission redispatched attempts=%+v counter=%d", got, emu.TxCounter())
	}
	d.handleCSLCompletion(radio.CSLCompletion{Action: attempts[0], Result: &cc1101.CSLTxResult{Sent: 0}})
	if got := clock.wait(t); got != proactiveDownlinkRetryBackoff {
		t.Fatalf("retry delay=%s, want %s", got, proactiveDownlinkRetryBackoff)
	}
	if len(immediate.queuedAttempts()) != 1 || len(d.dispatchQ) != 0 || d.dispatchDue[device.Serial] {
		t.Fatalf("no-anchor redispatched: attempts=%d due=%v queue=%v", len(immediate.queuedAttempts()), d.dispatchDue, d.dispatchQ)
	}
	if queued := emu.Queue(device.Serial); len(queued) != 1 || queued[0].Tries != 0 || !queued[0].InFlight {
		t.Fatalf("no-anchor queue=%+v", queued)
	}
	if got := emu.TxCounter(); got != firstCounter {
		t.Fatalf("counter advanced before retry: got=%d want=%d", got, firstCounter)
	}

	d.requestQueuedDispatch(device.Serial)
	d.dispatchQ = d.dispatchQ[1:]
	delete(d.dispatchDue, device.Serial)
	d.dispatchQueued(emu, device.Serial)
	if len(immediate.queuedAttempts()) != 1 || len(d.dispatchQ) != 0 || d.dispatchDue[device.Serial] {
		t.Fatalf("early dispatch submitted: attempts=%d due=%v queue=%v", len(immediate.queuedAttempts()), d.dispatchDue, d.dispatchQ)
	}
	if got := emu.TxCounter(); got != firstCounter {
		t.Fatalf("counter advanced by early dispatch: got=%d want=%d", got, firstCounter)
	}

	d.handleCSLCompletion(radio.CSLCompletion{Action: attempts[0], Result: &cc1101.CSLTxResult{Sent: 0}})
	clock.noTimer(t)
	if len(immediate.queuedAttempts()) != 1 {
		t.Fatalf("duplicate completion submitted=%d", len(immediate.queuedAttempts()))
	}

	clock.tick <- now.Add(proactiveDownlinkRetryBackoff)
	now = now.Add(proactiveDownlinkRetryBackoff)
	select {
	case fn := <-d.cmds:
		fn()
	case <-time.After(time.Second):
		t.Fatal("retry deadline did not dispatch")
	}
	if len(d.dispatchQ) != 1 || !d.dispatchDue[device.Serial] {
		t.Fatalf("retry dispatch due=%v queue=%v", d.dispatchDue, d.dispatchQ)
	}
	d.dispatchQ = d.dispatchQ[1:]
	delete(d.dispatchDue, device.Serial)
	d.dispatchQueued(emu, device.Serial)
	attempts = immediate.queuedAttempts()
	if len(attempts) != 2 || attempts[1].Bytes[2] == firstSeq || emu.TxCounter() <= firstCounter {
		t.Fatalf("retry attempts=%+v counter=%d", attempts, emu.TxCounter())
	}
}

// A job that found the channel busy put nothing on the air: it counts no
// attempt and retries on the scale of a MAC retransmission, not after 30 s.
func TestQueuedCSLBusyRetriesSoon(t *testing.T) {
	device, err := defaultInstall.Resolve("VA1000000005")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 20, 10, 47, 56, 0, time.UTC)
	cfg := defaultInstall.EmuConfig()
	cfg.Now = func() time.Time { return now }
	emu := tado.NewEmu(cfg, tado.NewUnhandledLogger(io.Discard))
	if err := emu.EnqueueChildLock(device.Serial, 5, true); err != nil {
		t.Fatal(err)
	}
	immediate := newImmediateTestRadio(false, nil)
	immediate.cslQueue, immediate.accept = true, true
	d := newDaemon(defaultInstall, emu, immediate, false, "live")
	clock := newManualRetryClock()
	d.retryAfter, d.retryNow = clock.after, func() time.Time { return now }
	t.Cleanup(func() { close(d.done) })

	d.dispatchQueued(emu, device.Serial)
	attempts := immediate.queuedAttempts()
	if len(attempts) != 1 {
		t.Fatalf("initial attempts=%+v", attempts)
	}
	d.handleCSLCompletion(radio.CSLCompletion{Action: attempts[0], Result: &cc1101.CSLTxResult{Sent: 0}, Err: cc1101.ErrCSLTxBusy})
	if got := clock.wait(t); got != cslBusyRetryBackoff {
		t.Fatalf("busy retry delay=%s, want %s", got, cslBusyRetryBackoff)
	}
	if queued := emu.Queue(device.Serial); len(queued) != 1 || queued[0].Tries != 0 {
		t.Fatalf("busy counted an attempt: queue=%+v", queued)
	}
}

func TestQueuedCSLConfigNoAnchorReplacesAdmissionT0(t *testing.T) {
	device, err := defaultInstall.Resolve("0a06")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 20, 13, 29, 5, 0, time.UTC)
	cfg := defaultInstall.EmuConfig()
	cfg.Now = func() time.Time { return now }
	emu := tado.NewEmu(cfg, tado.NewUnhandledLogger(io.Discard))
	offset := 120
	if err := emu.EnqueueConfig(device.Serial, 5, tado.ConfigWrite{UTCOffsetMin: &offset}); err != nil {
		t.Fatal(err)
	}
	immediate := newImmediateTestRadio(false, nil)
	immediate.cslQueue, immediate.accept = true, true
	d := newDaemon(defaultInstall, emu, immediate, false, "live")
	clock := newDistinctRetryClock()
	d.retryAfter, d.retryNow, d.retryRand = clock.after, func() time.Time { return now }, func() uint16 { return 0 }
	t.Cleanup(func() { close(d.done) })

	d.dispatchQueued(emu, device.Serial)
	attempts := immediate.queuedAttempts()
	if len(attempts) != 1 || !attempts[0].CoAPTransaction {
		t.Fatalf("admission attempts=%+v", attempts)
	}
	firstSeq, firstCounter := attempts[0].Bytes[2], emu.TxCounter()
	admission := clock.wait(t)
	if admission.delay != coapRetryMin {
		t.Fatalf("admission T0=%s, want %s", admission.delay, coapRetryMin)
	}

	pkt, err := tado.BuildCoAP(tado.CoAPReq{
		Type: "CON", Code: "0.01", MID: 0x5111, Token: []byte{1}, Path: []string{"time"},
		SrcEUI: device.WireEUI(), DstEUI: defaultInstall.CoordinatorEUI(), SrcPort: 5683, DstPort: 4005,
	})
	if err != nil {
		t.Fatal(err)
	}
	uplink, err := tado.BuildData(defaultInstall.NetworkKey(), 0x71, defaultInstall.CoordinatorEUI(), device.WireEUI()[:5], tado.WrapAppPayload(pkt, 701, nil))
	if err != nil {
		t.Fatal(err)
	}
	if actions := emu.OnFrame(uplink.Bytes()); len(actions) == 0 {
		t.Fatal("uplink was not accepted")
	}
	beforeExternalCounter := emu.TxCounter()
	for range 3 {
		d.requestQueuedDispatch(device.Serial)
		d.dispatchQ = d.dispatchQ[1:]
		delete(d.dispatchDue, device.Serial)
		d.dispatchQueued(emu, device.Serial)
	}
	if got := immediate.queuedAttempts(); len(got) != 1 || got[0].Bytes[2] != firstSeq || emu.TxCounter() != beforeExternalCounter {
		t.Fatalf("pre-completion redispatch attempts=%+v counter=%d", got, emu.TxCounter())
	}

	d.handleCSLCompletion(radio.CSLCompletion{Action: attempts[0], Result: &cc1101.CSLTxResult{Sent: 0}, Err: cc1101.ErrCSLTxNoAnchor})
	retry := clock.wait(t)
	if retry.delay != proactiveDownlinkRetryBackoff {
		t.Fatalf("no-anchor retry=%s, want %s", retry.delay, proactiveDownlinkRetryBackoff)
	}
	if d.retryDue[device.Serial] < 2 {
		t.Fatalf("T0 generation was not replaced: %d", d.retryDue[device.Serial])
	}
	admission.tick <- now.Add(coapRetryMin)
	select {
	case fn := <-d.cmds:
		fn()
		if len(d.dispatchQ) != 0 || d.retryDue[device.Serial] < 2 {
			t.Fatalf("stale T0 changed retry state due=%v queue=%v", d.retryDue, d.dispatchQ)
		}
	case <-time.After(20 * time.Millisecond):
	}
	if got := immediate.queuedAttempts(); len(got) != 1 || emu.TxCounter() != beforeExternalCounter {
		t.Fatalf("stale T0 advanced attempts=%+v counter=%d", got, emu.TxCounter())
	}

	now = now.Add(proactiveDownlinkRetryBackoff)
	retry.tick <- now
	select {
	case fn := <-d.cmds:
		fn()
	case <-time.After(time.Second):
		t.Fatal("30s retry did not dispatch")
	}
	d.dispatchQ = d.dispatchQ[1:]
	delete(d.dispatchDue, device.Serial)
	d.dispatchQueued(emu, device.Serial)
	attempts = immediate.queuedAttempts()
	if len(attempts) != 2 || attempts[1].Bytes[2] == firstSeq || emu.TxCounter() <= firstCounter {
		t.Fatalf("30s retry attempts=%+v counter=%d", attempts, emu.TxCounter())
	}
}

func TestQueuedCSLConfigAdmissionT0WaitsForCompletion(t *testing.T) {
	device, err := defaultInstall.Resolve("0a06")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 20, 13, 29, 5, 0, time.UTC)
	cfg := defaultInstall.EmuConfig()
	cfg.Now = func() time.Time { return now }
	emu := tado.NewEmu(cfg, tado.NewUnhandledLogger(io.Discard))
	offset := 120
	if err := emu.EnqueueConfig(device.Serial, 5, tado.ConfigWrite{UTCOffsetMin: &offset}); err != nil {
		t.Fatal(err)
	}
	immediate := newImmediateTestRadio(false, nil)
	immediate.cslQueue, immediate.accept = true, true
	d := newDaemon(defaultInstall, emu, immediate, false, "live")
	clock := newDistinctRetryClock()
	d.retryAfter, d.retryNow, d.retryRand = clock.after, func() time.Time { return now }, func() uint16 { return 0 }
	t.Cleanup(func() { close(d.done) })

	d.dispatchQueued(emu, device.Serial)
	attempt := immediate.queuedAttempts()[0]
	timer := clock.wait(t)
	now = now.Add(timer.delay)
	timer.tick <- now
	select {
	case fn := <-d.cmds:
		fn()
	case <-time.After(time.Second):
		t.Fatal("T0 callback did not arrive")
	}
	if d.retryDue[device.Serial] != 1 || len(d.dispatchQ) != 0 {
		t.Fatalf("T0 during active CSL due=%v queue=%v", d.retryDue, d.dispatchQ)
	}
	d.handleCSLCompletion(radio.CSLCompletion{Action: attempt, Result: &cc1101.CSLTxResult{Sent: 23}})
	if len(d.dispatchQ) != 1 || !d.dispatchDue[device.Serial] {
		t.Fatalf("completion after T0 did not dispatch retry due=%v queue=%v", d.retryDue, d.dispatchQ)
	}
}

func TestQueuedCSLDuplicateNoAnchorTryTwoKeepsRetryState(t *testing.T) {
	for _, tc := range []struct {
		name  string
		queue func(*tado.EmuState, string) error
		want  time.Duration
	}{
		{
			name: "coap",
			queue: func(e *tado.EmuState, target string) error {
				return e.EnqueueIdentify(target, 3)
			},
			want: proactiveDownlinkRetryBackoff,
		},
		{
			name: "non-coap",
			queue: func(e *tado.EmuState, target string) error {
				return e.EnqueueChildLock(target, 5, true)
			},
			want: proactiveDownlinkRetryBackoff,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			device, err := defaultInstall.Resolve("VA1000000005")
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 8, 20, 10, 47, 56, 0, time.UTC)
			cfg := defaultInstall.EmuConfig()
			cfg.Now = func() time.Time { return now }
			emu := tado.NewEmu(cfg, tado.NewUnhandledLogger(io.Discard))
			if err := tc.queue(emu, device.Serial); err != nil {
				t.Fatal(err)
			}
			_, ok, err := emu.NextQueuedDownlink(device.Serial)
			if err != nil || !ok {
				t.Fatalf("try one ok=%t err=%v", ok, err)
			}
			actions, ok, err := emu.NextQueuedDownlink(device.Serial)
			if err != nil || !ok || len(actions) == 0 || actions[0].QueueTry != 2 {
				t.Fatalf("try two actions=%+v ok=%t err=%v", actions, ok, err)
			}
			d := newDaemon(defaultInstall, emu, &immediateTestRadio{}, false, "live")
			clock := newManualRetryClock()
			d.retryAfter, d.retryNow, d.retryRand = clock.after, func() time.Time { return now }, func() uint16 { return 0 }
			t.Cleanup(func() { close(d.done) })
			counter := emu.TxCounter()
			d.handleCSLCompletion(radio.CSLCompletion{Action: actions[0], Result: &cc1101.CSLTxResult{Sent: 0}})
			if got := clock.wait(t); got != tc.want {
				t.Fatalf("retry delay=%s want=%s", got, tc.want)
			}
			d.handleCSLCompletion(radio.CSLCompletion{Action: actions[0], Result: &cc1101.CSLTxResult{Sent: 0}})
			clock.noTimer(t)
			queued := emu.Queue(device.Serial)
			if len(queued) != 1 || queued[0].Tries != 1 || !queued[0].InFlight || emu.TxCounter() != counter {
				t.Fatalf("duplicate no-anchor queue=%+v counter=%d", queued, emu.TxCounter())
			}
		})
	}
}

func TestQueuedCSLReplacesStaleRetryForNextFIFOHead(t *testing.T) {
	device, err := defaultInstall.Resolve("VA1000000005")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 20, 10, 47, 56, 0, time.UTC)
	cfg := defaultInstall.EmuConfig()
	cfg.Now = func() time.Time { return now }
	emu := tado.NewEmu(cfg, tado.NewUnhandledLogger(io.Discard))
	if err := emu.EnqueueChildLock(device.Serial, 5, true); err != nil {
		t.Fatal(err)
	}
	if err := emu.EnqueueChildLock(device.Serial, 5, false); err != nil {
		t.Fatal(err)
	}
	first, ok, err := emu.NextQueuedDownlink(device.Serial)
	if err != nil || !ok || len(first) == 0 {
		t.Fatalf("first=%+v ok=%t err=%v", first, ok, err)
	}
	d := newDaemon(defaultInstall, emu, &immediateTestRadio{}, false, "live")
	clock := newIndexedRetryClock()
	d.retryAfter, d.retryNow = clock.after, func() time.Time { return now }
	t.Cleanup(func() { close(d.done) })
	d.handleCSLCompletion(radio.CSLCompletion{Action: first[0], Result: &cc1101.CSLTxResult{Sent: 23}})
	if got := clock.wait(t); got != proactiveDownlinkRetryBackoff {
		t.Fatalf("first retry=%s", got)
	}
	emu.OnFrame(tado.BuildEnhAck(first[0].Bytes[2], defaultInstall.CoordinatorEUI(), device.WireEUI(), 0))
	second, ok, err := emu.NextQueuedDownlink(device.Serial)
	if err != nil || !ok || len(second) == 0 {
		t.Fatalf("second=%+v ok=%t err=%v", second, ok, err)
	}
	d.handleCSLCompletion(radio.CSLCompletion{Action: second[0], Result: &cc1101.CSLTxResult{Sent: 0}})
	if got := clock.wait(t); got != proactiveDownlinkRetryBackoff {
		t.Fatalf("second retry=%s", got)
	}
	clock.tick(t, 0, now.Add(proactiveDownlinkRetryBackoff))
	select {
	case fn := <-d.cmds:
		fn()
	case <-time.After(50 * time.Millisecond):
	}
	if len(d.dispatchQ) != 0 || d.dispatchDue[device.Serial] {
		t.Fatalf("stale retry dispatched: due=%v queue=%v", d.dispatchDue, d.dispatchQ)
	}
	clock.tick(t, 1, now.Add(proactiveDownlinkRetryBackoff))
	select {
	case fn := <-d.cmds:
		fn()
	case <-time.After(time.Second):
		t.Fatal("second retry did not dispatch")
	}
	if len(d.dispatchQ) != 1 || !d.dispatchDue[device.Serial] {
		t.Fatalf("second retry due=%v queue=%v", d.dispatchDue, d.dispatchQ)
	}
}

func TestQueuedCoAPStartsT0OnlyAfterAdmission(t *testing.T) {
	device, err := defaultInstall.Resolve("VA1000000005")
	if err != nil {
		t.Fatal(err)
	}
	emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	if err := emu.EnqueueIdentify(device.Serial, 3); err != nil {
		t.Fatal(err)
	}
	immediate := newImmediateTestRadio(false, cc1101.ErrCSLQueueFull)
	immediate.cslQueue = true
	d := newDaemon(defaultInstall, emu, immediate, false, "live")
	d.dispatchQueued(emu, device.Serial)
	_, waited := d.cslAdmissionWait[device.Serial]
	if len(d.coapRetryBase) != 0 || len(d.retryDue) != 0 || !waited {
		t.Fatalf("full admission state: base=%v due=%v wait=%v", d.coapRetryBase, d.retryDue, d.cslAdmissionWait)
	}
	queued := immediate.queuedAttempts()
	if len(queued) != 1 {
		t.Fatalf("attempts=%d, want 1", len(queued))
	}
	d.handleCSLCompletion(radio.CSLCompletion{Action: queued[0], Result: &cc1101.CSLTxResult{Sent: 23}})
	if !d.dispatchDue[device.Serial] || len(d.dispatchQ) != 1 {
		t.Fatalf("completion did not release admission wait: action=%+v due=%v queue=%v", queued[0], d.dispatchDue, d.dispatchQ)
	}
	immediate.err = nil
	immediate.accept = true
	delete(d.dispatchDue, device.Serial)
	d.dispatchQ = nil
	d.dispatchQueued(emu, device.Serial)
	if _, ok := d.coapRetryBase[device.Serial]; !ok || d.retryDue[device.Serial] == 0 {
		t.Fatalf("accepted admission did not arm T0: base=%v due=%v", d.coapRetryBase, d.retryDue)
	}
}

func TestRevertUnsubmittedKeepsPartialLogicalAttempt(t *testing.T) {
	device, err := defaultInstall.Resolve("VA1000000005")
	if err != nil {
		t.Fatal(err)
	}
	emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	table := make([]tado.NodeTableRec, 20)
	for i := range table {
		table[i] = tado.NodeTableRec{Role: 0x0b, Node: uint8(i)}
	}
	if err := emu.EnqueueConfig(device.Serial, 10, tado.ConfigWrite{NodeTable: table}); err != nil {
		t.Fatal(err)
	}
	pkt, err := tado.BuildCoAP(tado.CoAPReq{
		Type: "NON", Code: "0.01", MID: 0x6110, Token: []byte{0x51},
		Path:   []string{"h", "7654321", "d", "time"},
		SrcEUI: device.WireEUI(), DstEUI: defaultInstall.CoordinatorEUI(),
		SrcPort: 5683, DstPort: 4005,
	})
	if err != nil {
		t.Fatal(err)
	}
	uplink, err := tado.BuildData(defaultInstall.NetworkKey(), 0x21, defaultInstall.CoordinatorEUI(),
		device.WireEUI()[:5], tado.WrapAppPayload(pkt, 700, nil))
	if err != nil {
		t.Fatal(err)
	}
	var queuedActions []tado.Action
	for _, action := range emu.OnFrame(uplink.Bytes()) {
		if action.QueueAttempt {
			queuedActions = append(queuedActions, action)
		}
	}
	if len(queuedActions) < 2 {
		t.Fatalf("queued fragments=%d, want at least 2", len(queuedActions))
	}
	d := newDaemon(defaultInstall, emu, &radio.Mock{}, false, "live")
	d.revertUnsubmitted(queuedActions, []tado.Action{queuedActions[0]})
	queued := emu.Queue(device.Serial)
	if len(queued) != 1 || queued[0].Tries != 1 || !queued[0].InFlight {
		t.Fatalf("queue=%+v, want retained submitted attempt", queued)
	}
	if got := emu.Stats().DownlinksSent; got != 1 {
		t.Fatalf("downlinks sent=%d, want 1", got)
	}
}

func TestQueuedCSLFragmentsRevertByLastSequence(t *testing.T) {
	device, err := defaultInstall.Resolve("VA1000000005")
	if err != nil {
		t.Fatal(err)
	}
	emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	table := make([]tado.NodeTableRec, 20)
	if err := emu.EnqueueConfig(device.Serial, 10, tado.ConfigWrite{NodeTable: table}); err != nil {
		t.Fatal(err)
	}
	pkt, err := tado.BuildCoAP(tado.CoAPReq{Type: "NON", Code: "0.01", MID: 0x6111, Token: []byte{0x52}, Path: []string{"h", "7654321", "d", "time"}, SrcEUI: device.WireEUI(), DstEUI: defaultInstall.CoordinatorEUI(), SrcPort: 5683, DstPort: 4005})
	if err != nil {
		t.Fatal(err)
	}
	uplink, err := tado.BuildData(defaultInstall.NetworkKey(), 0x22, defaultInstall.CoordinatorEUI(), device.WireEUI()[:5], tado.WrapAppPayload(pkt, 701, nil))
	if err != nil {
		t.Fatal(err)
	}
	var fragments []tado.Action
	for _, action := range emu.OnFrame(uplink.Bytes()) {
		if action.QueueAttempt {
			fragments = append(fragments, action)
		}
	}
	if len(fragments) < 2 {
		t.Fatalf("fragments=%d", len(fragments))
	}
	d := newDaemon(defaultInstall, emu, &immediateTestRadio{}, false, "live")
	clock := newManualRetryClock()
	d.retryAfter = clock.after
	t.Cleanup(func() { close(d.done) })
	d.handleCSLCompletion(radio.CSLCompletion{Action: fragments[len(fragments)-1], Result: &cc1101.CSLTxResult{Sent: 0}})
	queued := emu.Queue(device.Serial)
	if len(queued) != 1 || queued[0].Tries != 0 || !queued[0].InFlight || emu.Stats().DownlinksSent != 0 {
		t.Fatalf("queue=%+v sent=%d", queued, emu.Stats().DownlinksSent)
	}
}

func TestFormatCSLFollowReportsCoAPCorrelationWithoutToken(t *testing.T) {
	device, err := defaultInstall.Resolve("VA1000000005")
	if err != nil {
		t.Fatal(err)
	}
	emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	if err := emu.EnqueueBoost(device.Serial, 10, true); err != nil {
		t.Fatal(err)
	}
	actions, ok, err := emu.NextQueuedDownlink(device.Serial)
	if err != nil || !ok || len(actions) != 1 {
		t.Fatalf("actions=%+v ok=%t err=%v", actions, ok, err)
	}
	requestData, err := tado.ParseData(defaultInstall.NetworkKey(), actions[0].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	request := tado.ParseApp(requestData.Plaintext).CoAP
	if request == nil {
		t.Fatal("boost request did not parse")
	}
	ack, err := tado.BuildCoAPAck("2.05", request.MID, request.Token,
		tado.TLV{Tag: "62a0", Val: []byte{1}}.Encode(), device.WireEUI(), defaultInstall.CoordinatorEUI(), 5683, 4005)
	if err != nil {
		t.Fatal(err)
	}
	follow, err := tado.BuildData(defaultInstall.NetworkKey(), 0x72, defaultInstall.CoordinatorEUI(), device.WireEUI()[:5], tado.WrapAppPayload(ack, 701, nil))
	if err != nil {
		t.Fatal(err)
	}
	got := formatCSLFollow(actions[0], defaultInstall.NetworkKey(), follow.Bytes())
	for _, want := range []string{"request=z/s?id=10", "code=2.05", "mid-match=true", "token-match=true", "tlv=62a0=01"} {
		if !strings.Contains(got, want) {
			t.Errorf("follow=%q missing %q", got, want)
		}
	}
	if strings.Contains(got, hex.EncodeToString(request.Token)) {
		t.Fatalf("follow log exposed token: %q", got)
	}
	nonCoAP := formatCSLFollow(actions[0], defaultInstall.NetworkKey(), tado.BuildEnhAck(requestData.Seq, defaultInstall.CoordinatorEUI(), device.WireEUI(), 0))
	if !strings.Contains(nonCoAP, "non-coap class=EnhACK") {
		t.Fatalf("non-CoAP follow=%q", nonCoAP)
	}
	malformed := formatCSLFollow(actions[0], defaultInstall.NetworkKey(), []byte{0x69, 0xec})
	if !strings.Contains(malformed, "non-coap parse=") {
		t.Fatalf("malformed follow=%q", malformed)
	}
}

// The API answers while the live radio holds a queued CSL transaction with no
// completion yet.
func TestDaemonAPIAnswersWhileLiveCSLPending(t *testing.T) {
	frame := []byte{0x69, 0xec, 0x10, 0x01, 0x0a, 0xce, 0xfa, 0x07, 0xc5, 0x1b, 0x00, 0x09, 0xa1, 0xce, 0xfa, 0x07}
	bridge := &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}}
	live, err := radio.StartLive(bridge, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = live.Close() })
	result, err := live.SendImmediate([]tado.Action{{Node: "hall", Bytes: frame, CSLTx: true}}, true)
	if err != nil || !result.CSLQueue || len(result.Accepted) != 1 {
		t.Fatalf("queued CSL result=%+v err=%v", result, err)
	}
	d := newDaemon(defaultInstall, tado.NewEmu(tado.EmuConfig{TxCounter: 100000}, tado.NewUnhandledLogger(io.Discard)), live, false, "live")
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)
	t.Cleanup(cancel)
	srv := httptest.NewServer(d.mux())
	t.Cleanup(srv.Close)
	for _, path := range []string{"/api/health", "/api/queue/a408"} {
		var response any
		if code := getJSON(t, srv.URL+path, &response); code != http.StatusOK {
			t.Fatalf("%s status=%d", path, code)
		}
	}
}

// routerSolicitationFrame is the broadcast a device sends when it has lost the
// router: ICMPv6 type 133, 6LoWPAN-compressed to ff02::2.
func routerSolicitationFrame(t *testing.T) []byte {
	t.Helper()
	device, err := defaultInstall.Resolve("VA1000000005")
	if err != nil {
		t.Fatal(err)
	}
	rs, err := tado.BuildBroadcast(defaultInstall.NetworkKey(), 0x21, device.WireEUI(), 9, 0x04,
		tado.WrapBroadcastPayload([]byte{0x7b, 0x3b, 58, 0x01, 133, 0, 0, 0}, 0))
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

// solicitationDaemon runs the engine on a mock bridge that answers every
// receive with the same solicitation, repeat times.
func solicitationDaemon(t *testing.T, repeat int, dryRun bool) (*daemon, *orderedLiveBridge, *tado.UnhandledLogger) {
	t.Helper()
	bridge := &orderedLiveBridge{
		resultRepeat: repeat,
		result: &cc1101.AutoAckResult{
			Status: cc1101.AutoAckFrameNoAck,
			Frame:  &cc1101.RxFrame{Payload: routerSolicitationFrame(t), CRCOK: true},
		},
	}
	live, err := radio.StartLive(bridge, defaultInstall.CoordinatorEUI(), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	cfg := defaultInstall.EmuConfig()
	// A fixed clock keeps every solicitation inside the answer window.
	now := time.Unix(1000, 0)
	cfg.Now = func() time.Time { return now }
	ulog := tado.NewUnhandledLogger(io.Discard)
	d := newDaemon(defaultInstall, tado.NewEmu(cfg, ulog), live, dryRun, "live")
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)
	t.Cleanup(func() {
		cancel()
		_ = live.Close()
	})
	return d, bridge, ulog
}

// emuStats reads the engine counters from the test goroutine.
func emuStats(t *testing.T, d *daemon) tado.EmuStats {
	t.Helper()
	v, err := d.do(func(e *tado.EmuState) (any, error) { return e.Stats(), nil })
	if err != nil {
		t.Fatal(err)
	}
	return v.(tado.EmuStats)
}

// The answer to a solicitation does not wait for the periodic beacon: this
// daemon has none.
func TestLiveRadioBatchesRouterAdvertAfterSolicitation(t *testing.T) {
	d, bridge, _ := solicitationDaemon(t, 1, false)
	deadline := time.Now().Add(time.Second)
	for {
		events, trains, _ := bridge.snapshot()
		if len(events) >= 3 && len(bridge.burstSnapshot()) == 1 {
			if events[0] != "recv" || events[1] != "ra-burst" || events[2] != "recv" {
				t.Fatalf("order=%v, want recv ra-burst recv", events[:3])
			}
			if len(trains) != 0 {
				t.Fatalf("RA generic trains=%d, want 0", len(trains))
			}
			burst := bridge.burstSnapshot()[0]
			if tado.Classify(burst) != tado.ClassBroadcast {
				t.Fatalf("RA burst frame=%x", burst)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("owner did not send the RS response: events=%v trains=%d", events, len(trains))
		}
		time.Sleep(time.Millisecond)
	}
	stats := emuStats(t, d)
	if stats.RouterAdvertisementsSent != 1 || stats.RouterSolicitationsAnswered != 1 {
		t.Errorf("sent=%d answered=%d, want 1 and 1",
			stats.RouterAdvertisementsSent, stats.RouterSolicitationsAnswered)
	}
}

// Ten solicitations inside the window get one burst: the advertisement is a
// broadcast and every device that asked hears it.
func TestLiveRadioAnswersTenSolicitationsWithOneBurst(t *testing.T) {
	d, bridge, ulog := solicitationDaemon(t, 10, false)
	deadline := time.Now().Add(2 * time.Second)
	for {
		stats := emuStats(t, d)
		if stats.RouterSolicitations >= 10 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the engine saw %d solicitations, want 10", stats.RouterSolicitations)
		}
		time.Sleep(time.Millisecond)
	}
	stats := emuStats(t, d)
	if got := len(bridge.burstSnapshot()); got != 1 {
		t.Errorf("bursts=%d, want 1", got)
	}
	if stats.RouterAdvertisementsSent != 1 || stats.RouterSolicitationsAnswered != 10 {
		t.Errorf("sent=%d answered=%d, want 1 and 10",
			stats.RouterAdvertisementsSent, stats.RouterSolicitationsAnswered)
	}
	if got := ulog.Total(); got != 0 {
		t.Errorf("unhandled events=%d, want 0: an answered solicitation is handled", got)
	}
}

// A dry-run answers nothing and says so in the unhandled log.
func TestDryRunLogsSolicitationWithoutAnswer(t *testing.T) {
	d, bridge, ulog := solicitationDaemon(t, 1, true)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if emuStats(t, d).RouterSolicitations >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the engine never saw the solicitation")
		}
		time.Sleep(time.Millisecond)
	}
	stats := emuStats(t, d)
	if got := len(bridge.burstSnapshot()); got != 0 {
		t.Errorf("bursts=%d, want 0 in a dry-run", got)
	}
	if stats.RouterAdvertisementsSent != 0 || stats.RouterSolicitationsAnswered != 0 {
		t.Errorf("sent=%d answered=%d, want 0 and 0",
			stats.RouterAdvertisementsSent, stats.RouterSolicitationsAnswered)
	}
	logged := ""
	for reason := range ulog.Counts() {
		logged = reason
	}
	if !strings.Contains(logged, "router-solicitation") || !strings.Contains(logged, "suppressed") {
		t.Errorf("unhandled reason=%q, want the suppressed solicitation", logged)
	}
}

// The beacon runs at the longest interval of the commercial coordinator.
func TestBeaconDefaultInterval(t *testing.T) {
	if defaultBeaconInterval != 600*time.Second {
		t.Errorf("defaultBeaconInterval=%v, want 600s", defaultBeaconInterval)
	}
	var b cli.Bootstrap
	if got := b.BeaconInterval(); got != defaultBeaconInterval {
		t.Errorf("bootstrap beacon_sec default=%v, want %v", got, defaultBeaconInterval)
	}
}

func TestDaemonAPICloseUnblocksColdCSLQueue(t *testing.T) {
	bridge := &blockingQueuedLiveBridge{
		queuedLiveBridge: &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}},
		entered:          make(chan struct{}, 1),
		unblock:          make(chan struct{}),
	}
	live, err := radio.StartLive(bridge, defaultInstall.CoordinatorEUI(), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-bridge.entered:
	case <-time.After(time.Second):
		t.Fatal("owner did not enter pending queue poll")
	}
	d := newDaemon(defaultInstall, tado.NewEmu(tado.EmuConfig{TxCounter: 100000}, tado.NewUnhandledLogger(io.Discard)), live, false, "live")
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)
	srv := httptest.NewServer(d.mux())
	defer srv.Close()
	defer cancel()
	defer live.Close()

	finished := make(chan error, 1)
	go func() {
		body := bytes.NewBufferString(`{"id":3}`)
		resp, err := http.Post(srv.URL+"/api/nodes/a408/identify", "application/json", body)
		if err == nil {
			resp.Body.Close()
		}
		finished <- err
	}()
	time.Sleep(10 * time.Millisecond)
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("API handler remained blocked after live close")
	}
}

func TestLiveRadioBridgeGateDisablesAutoAck(t *testing.T) {
	bridgeFrame, _ := goldenPair(t)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	bridge := &orderedLiveBridge{result: &cc1101.AutoAckResult{
		Status: cc1101.AutoAckFrameNoAck,
		Frame:  &cc1101.RxFrame{Payload: bridgeFrame, CRCOK: true},
	}, entered: entered, release: release}
	live, err := radio.StartLive(bridge, defaultInstall.CoordinatorEUI(), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	d := newDaemon(defaultInstall, emu, live, false, "live")
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)
	t.Cleanup(func() {
		cancel()
		_ = live.Close()
	})

	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("owner did not enter receive")
	}
	if err := live.Send([]byte{0x49, 0xe8, 0x01}); err != nil {
		t.Fatal(err)
	}
	close(release)

	deadline := time.Now().Add(time.Second)
	for {
		events, trains, _ := bridge.snapshot()
		if len(events) >= 3 && events[0] == "recv" && events[1] == "recv-fw" {
			if len(trains) != 0 {
				t.Fatalf("bridge gate sent %d trains", len(trains))
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("auto ACK did not stop after bridge detection: events=%v", events)
		}
		time.Sleep(time.Millisecond)
	}
	if err := live.Send([]byte{0x49, 0xe8, 0x02}); !errors.Is(err, errTXBlocked) {
		t.Fatalf("Send after bridge detection = %v, want %v", err, errTXBlocked)
	}
}

// runEngine drives an armed daemon over frames until the engine has consumed
// them all, then returns it with its mock radio.
func runEngine(t *testing.T, frames [][]byte) (*daemon, *radio.Mock) {
	t.Helper()
	emu := tado.NewEmu(tado.EmuConfig{TxCounter: 0}, tado.NewUnhandledLogger(io.Discard))
	radio := radio.NewMock(frames)
	d := newDaemon(defaultInstall, emu, radio, false, "mock")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go d.run(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for {
		v, err := d.do(func(e *tado.EmuState) (any, error) { return e.Stats().Frames, nil })
		if err != nil {
			t.Fatal(err)
		}
		if v.(int) >= len(frames) {
			return d, radio
		}
		if time.Now().After(deadline) {
			t.Fatalf("engine consumed %d of %d frames", v.(int), len(frames))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// goldenPair returns one bridge-sourced downlink and one device uplink from the
// golden capture.
func goldenPair(t *testing.T) (bridge, uplink []byte) {
	t.Helper()
	for _, fr := range loadFrames(t, "../../tado/testdata/frames_69ec.txt", 200) {
		f, err := tado.ParseData(tado.NetworkKey, fr)
		if err != nil {
			continue
		}
		switch {
		case bytes.Equal(f.SrcPrefix, tado.BridgeEUI[:5]):
			if bridge == nil {
				bridge = fr
			}
		case uplink == nil && bytes.Equal(f.DstEUI, tado.BridgeEUI):
			uplink = fr
		}
	}
	if bridge == nil || uplink == nil {
		t.Fatal("capture has no bridge/device frame pair")
	}
	return bridge, uplink
}

// TestDaemonRefusesTxWithBridgePresent: with the real bridge on air the armed
// engine decides the same frames but puts none of them on the radio.
func TestDaemonRefusesTxWithBridgePresent(t *testing.T) {
	bridge, uplink := goldenPair(t)

	_, control := runEngine(t, [][]byte{uplink})
	if len(control.Sent()) == 0 {
		t.Fatal("armed engine sent nothing for a device uplink")
	}

	d, radio := runEngine(t, [][]byte{bridge, uplink})
	if n := len(radio.Sent()); n != 0 {
		t.Errorf("sent %d frames with the bridge present, want 0", n)
	}
	v, err := d.do(func(e *tado.EmuState) (any, error) { return e.Snapshot().BridgePresent, nil })
	if err != nil {
		t.Fatal(err)
	}
	if !v.(bool) {
		t.Error("BridgePresent is false after a bridge-sourced frame")
	}
}

func TestDaemonCounterIgnoresObservedBridge(t *testing.T) {
	bridge, _ := goldenPair(t)
	d, _ := runEngine(t, [][]byte{bridge})
	v, err := d.do(func(e *tado.EmuState) (any, error) {
		return [2]uint32{e.TxCounter(), e.MaxBridgeCounter()}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got := v.([2]uint32)
	if got[1] == 0 {
		t.Fatal("bridge frame counter was not observed")
	}
	if got[0] != 0 {
		t.Errorf("tx counter=%d, want startup value 0", got[0])
	}
}

// TestDaemonConfigAndActiveGet covers the config write and known device GETs.
func TestDaemonConfigAndActiveGet(t *testing.T) {
	emu := tado.NewEmu(tado.EmuConfig{TxCounter: 100000}, tado.NewUnhandledLogger(io.Discard))
	d := newDaemon(defaultInstall, emu, radio.NewMock(nil), true, "mock")
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)
	srv := httptest.NewServer(d.mux())
	t.Cleanup(func() {
		srv.Close()
		cancel()
	})

	const short = "a602" // bedroom, in the inventory

	var set struct {
		Queued bool   `json:"queued"`
		Error  string `json:"error"`
	}
	if code := postJSON(t, http.MethodPost, srv.URL+"/api/nodes/"+short+"/config",
		map[string]any{"traceEnable": 1, "offlineScheduleEnabled": false, "id": 10}, &set); code != http.StatusOK || !set.Queued {
		t.Fatalf("config: got %d %+v", code, set)
	}

	resp, err := http.Get(srv.URL + "/api/nodes/" + short + "/config")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("config GET: got %d, want 405", resp.StatusCode)
	}

	var active struct {
		Resource string `json:"resource"`
		Queued   bool   `json:"queued"`
	}
	if code := getJSON(t, srv.URL+"/api/nodes/"+short+"/fw", &active); code != http.StatusOK || !active.Queued || active.Resource != "d/fw/state" {
		t.Fatalf("firmware active get: got %d %+v", code, active)
	}
	resp, err = http.Get(srv.URL + "/api/nodes/" + short + "/info")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("info GET: got %d, want 404", resp.StatusCode)
	}

	var ov struct {
		Error string `json:"error"`
	}
	if code := postJSON(t, http.MethodPost, srv.URL+"/api/nodes/"+short+"/overlay",
		map[string]any{"mode": "auto", "id": 10}, &ov); code != http.StatusBadRequest || ov.Error != tado.ErrHeatingBlockUnavailable.Error() {
		t.Fatalf("overlay without heating block: got %d %+v", code, ov)
	}
}

// answeredActiveGetServer queues one active GET of resource on a valve, feeds
// the device answer code+payload back, and serves the daemon API.
func answeredActiveGetServer(t *testing.T, resource string, path []string, code string, payload []byte) string {
	t.Helper()
	device, err := defaultInstall.Resolve("VA1000000005")
	if err != nil {
		t.Fatal(err)
	}
	emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	if err := emu.EnqueueGet(device.Serial, resource, path, nil); err != nil {
		t.Fatal(err)
	}
	uplinkCoAP, err := tado.BuildCoAP(tado.CoAPReq{
		Type: "CON", Code: "0.01", MID: 0x6110, Token: []byte{0x51},
		Path:   []string{"h", "7654321", "d", "time"},
		SrcEUI: device.WireEUI(), DstEUI: defaultInstall.CoordinatorEUI(),
		SrcPort: 5683, DstPort: 4005,
	})
	if err != nil {
		t.Fatal(err)
	}
	uplink, err := tado.BuildData(defaultInstall.NetworkKey(), 0x21, defaultInstall.CoordinatorEUI(),
		device.WireEUI()[:5], tado.WrapAppPayload(uplinkCoAP, 700, nil))
	if err != nil {
		t.Fatal(err)
	}
	var get tado.Action
	for _, action := range emu.OnFrame(uplink.Bytes()) {
		if action.QueueAttempt {
			get = action
			break
		}
	}
	if len(get.Bytes) == 0 {
		t.Fatal("active GET action missing")
	}
	data, err := tado.ParseData(defaultInstall.NetworkKey(), get.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	request := tado.ParseApp(data.Plaintext).CoAP
	response, err := tado.BuildCoAPAck(code, request.MID, request.Token, payload,
		device.WireEUI(), defaultInstall.CoordinatorEUI(), 5683, 4005)
	if err != nil {
		t.Fatal(err)
	}
	responseFrame, err := tado.BuildData(defaultInstall.NetworkKey(), 0x22, defaultInstall.CoordinatorEUI(),
		device.WireEUI()[:5], tado.WrapAppPayload(response, 701, nil))
	if err != nil {
		t.Fatal(err)
	}
	emu.OnFrame(responseFrame.Bytes())

	d := newDaemon(defaultInstall, emu, radio.NewMock(nil), true, "mock")
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)
	srv := httptest.NewServer(d.mux())
	t.Cleanup(func() {
		srv.Close()
		cancel()
	})
	return srv.URL
}

func TestActiveGetAPIReportsCoAPCode(t *testing.T) {
	url := answeredActiveGetServer(t, "d/currentlog", []string{"d", "currentlog"}, "4.05", nil)
	var body struct {
		Resource string `json:"resource"`
		Code     string `json:"code"`
		Result   string `json:"result"`
	}
	if status := getJSON(t, url+"/api/nodes/a602/currentlog", &body); status != http.StatusOK {
		t.Fatalf("active GET status=%d body=%+v", status, body)
	}
	if body.Resource != "d/currentlog" || body.Code != "4.05" || body.Result != "" {
		t.Fatalf("active GET response=%+v, want d/currentlog/4.05/empty", body)
	}
}

// TestNodeFwAPIDecodesState: a 2.05 d/fw/state answer comes back decoded.
func TestNodeFwAPIDecodesState(t *testing.T) {
	// synthetic: type 8, running 215.1 image+active+confirmed, ext 0x10 204.1 image+confirmed
	payload := []byte{
		0x01, 0xa0, 0x01, 0x08,
		0x00, 0x3a, 0x02, 0x35, 0xc1, 0x00, 0x3b, 0x01, 0x0e,
		0x00, 0x35, 0x02, 0x33, 0x01, 0x00, 0x36, 0x01, 0x0a,
		0x02, 0x10, 0x07, 'a', 'b', 'c', '1', '2', '3', '4',
	}
	url := answeredActiveGetServer(t, "d/fw/state", []string{"d", "fw", "state"}, "2.05", payload)
	var body struct {
		Code    string           `json:"code"`
		Decoded tado.FwStateView `json:"decoded"`
	}
	if status := getJSON(t, url+"/api/nodes/a602/fw", &body); status != http.StatusOK || body.Code != "2.05" {
		t.Fatalf("fw GET status=%d body=%+v", status, body)
	}
	v := body.Decoded
	if v.DeviceType != 8 || v.Running.Version != "215.1" || !v.Confirmed || v.Ext10 == nil || v.Ext10.Version != "204.1" ||
		v.Ext10.State != "image+confirmed" || v.Ext11 != nil || v.BuildHash != "abc1234" {
		t.Fatalf("decoded %+v", v)
	}
}

// TestDaemonConfigFields: the d/config endpoint takes every typed store field,
// and a value the device would misread is answered with the validation message
// instead of being queued.
func TestDaemonConfigFields(t *testing.T) {
	emu := tado.NewEmu(tado.EmuConfig{TxCounter: 100000}, tado.NewUnhandledLogger(io.Discard))
	d := newDaemon(defaultInstall, emu, radio.NewMock(nil), true, "mock")
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)
	srv := httptest.NewServer(d.mux())
	t.Cleanup(func() {
		srv.Close()
		cancel()
	})

	const short = "a602"
	url := srv.URL + "/api/nodes/" + short + "/config"

	var set struct {
		Queued bool   `json:"queued"`
		Error  string `json:"error"`
	}
	full := map[string]any{
		"tempUnitF": true, "calibOffset": 180, "traceEnable": 0, "lcdVariant": 1,
		"stateBits": 512, "etagHex": "0011223344556677", "homeId": 7654321,
		"utcOffsetMin": 60, "offlineScheduleEnabled": true,
		"nodeTable": []map[string]any{{"role": 13, "node": 0}},
		"id":        10,
	}
	if code := postJSON(t, http.MethodPost, url, full, &set); code != http.StatusOK || !set.Queued {
		t.Fatalf("full config: got %d %+v", code, set)
	}
	v, err := d.do(func(e *tado.EmuState) (any, error) {
		return e.ScheduleDownlink(short)
	})
	if err != nil {
		t.Fatal(err)
	}
	actions := v.([]tado.Action)
	frame, err := tado.ParseData(defaultInstall.NetworkKey(), actions[len(actions)-1].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	app := tado.ParseApp(frame.Plaintext)
	if app == nil {
		t.Fatal("missing app payload")
	}
	found := false
	for _, tlv := range app.TLVs {
		if tlv.Tag == "02b3" {
			found = true
			if got := hex.EncodeToString(tlv.Val); got != "01" {
				t.Errorf("02b3 = %s, want 01", got)
			}
			break
		}
	}
	if !found {
		t.Errorf("02b3 missing from config TLVs: %#v", app.TLVs)
	}

	for name, body := range map[string]map[string]any{
		"calibration offset beyond +-10.0 C": {"calibOffset": 1001, "id": 10},
		"zero home id blocks the apply path": {"homeId": 0, "id": 10},
		"etag longer than the record buffer": {"etagHex": "001122334455667788", "id": 10},
		"utc offset beyond a day":            {"utcOffsetMin": 1441, "id": 10},
		"malformed etag hex":                 {"etagHex": "zz", "id": 10},
		// traceEnable is a byte on the wire: a bool no longer decodes.
		"trace enable as a bool": {"traceEnable": true, "id": 10},
	} {
		var bad struct {
			Queued bool   `json:"queued"`
			Error  string `json:"error"`
		}
		code := postJSON(t, http.MethodPost, url, body, &bad)
		if code != http.StatusBadRequest || bad.Queued || bad.Error == "" {
			t.Errorf("%s: got %d %+v, want 400 with a message", name, code, bad)
		}
	}
}

// TestSetpointZoneBody: the zone block maps to the three z/s bytes, the binding
// bit comes along with the circuit id, and the two combinations the firmware
// would silently ignore are refused.
func TestSetpointZoneBody(t *testing.T) {
	circuit, enabled := 1, true
	z := &zoneBody{
		Circuit:           &circuit,
		Flags:             &zoneFlagsBody{Active: true, Primary: true},
		ControllerEnabled: &enabled,
	}
	var sp tado.SetpointParams
	if err := z.apply(&sp); err != nil {
		t.Fatal(err)
	}
	// active | binding | primary; the binding bit is implied by the circuit id.
	if want := byte(tado.ZoneFlagActive | tado.ZoneFlagCircuit | tado.ZoneFlagPrimary); sp.ZoneFlags == nil || *sp.ZoneFlags != want {
		t.Errorf("flags = %v, want %#02x", sp.ZoneFlags, want)
	}
	if sp.Circuit == nil || *sp.Circuit != 1 {
		t.Errorf("circuit = %v, want 1", sp.Circuit)
	}
	if sp.CtrlEnable == nil || *sp.CtrlEnable != 0 {
		t.Errorf("ctrlEnable = %v, want 0 (enabled)", sp.CtrlEnable)
	}

	disabled := false
	sp = tado.SetpointParams{}
	if err := (&zoneBody{ControllerEnabled: &disabled}).apply(&sp); err != nil {
		t.Fatal(err)
	}
	if sp.CtrlEnable == nil || *sp.CtrlEnable != 1 {
		t.Errorf("ctrlEnable = %v, want 1 (disabled)", sp.CtrlEnable)
	}
	if sp.ZoneFlags != nil || sp.Circuit != nil {
		t.Errorf("an omitted field must stay untouched: flags=%v circuit=%v", sp.ZoneFlags, sp.Circuit)
	}

	// A nil block leaves the captured setpoint downlink alone.
	sp = tado.SetpointParams{}
	if err := (*zoneBody)(nil).apply(&sp); err != nil || sp.ZoneFlags != nil || sp.Circuit != nil || sp.CtrlEnable != nil {
		t.Errorf("nil zone block changed the setpoint: %+v (%v)", sp, err)
	}

	if err := (&zoneBody{Circuit: &circuit}).apply(&tado.SetpointParams{}); err == nil {
		t.Error("a circuit id without flags should be refused")
	}
	if err := (&zoneBody{Flags: &zoneFlagsBody{Primary: true, Member: true}}).apply(&tado.SetpointParams{}); err == nil {
		t.Error("primary together with member should be refused")
	}
}

func TestDaemonUnknownNode(t *testing.T) {
	emu := tado.NewEmu(tado.EmuConfig{}, tado.NewUnhandledLogger(io.Discard))
	d := newDaemon(defaultInstall, emu, radio.NewMock(nil), true, "mock")
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)
	srv := httptest.NewServer(d.mux())
	t.Cleanup(func() {
		srv.Close()
		cancel()
	})

	var body struct {
		Error string `json:"error"`
	}
	if code := getJSON(t, srv.URL+"/api/nodes/nosuchnode", &body); code != http.StatusNotFound {
		t.Fatalf("unknown node: got %d %+v", code, body)
	}
}

// --- durable desired state ---

// The shutdown save is the last writer of the state database, and it stops
// writing once the engine is gone.
func TestDaemonShutdownSaveHoldsTheLastMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	d := newDaemon(defaultInstall, emu, radio.NewMock(nil), true, "mock")
	d.state = newTestStore(t, path)
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)

	boiler, err := defaultInstall.Resolve(dhwNode)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.do(func(e *tado.EmuState) (any, error) {
		return nil, e.EnqueueFlowSetpoint(hex.EncodeToString(boiler.WireEUI()), flowCircuitID, 44)
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.saveDesiredState(); err != nil {
		t.Fatal(err)
	}
	cancel()
	<-d.done

	saved, err := d.state.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := saved.FlowC["1"]; !ok || got != 44 {
		t.Errorf("flowC=%v (present %v), want 44", got, ok)
	}
	if err := d.saveDesiredState(); !errors.Is(err, errStopped) {
		t.Errorf("save after shutdown: %v, want %v", err, errStopped)
	}
	again, err := d.state.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again, saved) {
		t.Errorf("the state changed after shutdown\n got: %#v\nwant: %#v", again, saved)
	}
}

const daemonStateHelper = "FREEDO_TEST_DAEMON_STATE_HELPER"

// The whole CLI shutdown order: the API mutation is on disk once the process is
// gone, so the final save runs before the engine and the store are closed.
func TestDaemonShutdownSavePersistsAcrossTheCLI(t *testing.T) {
	if os.Getenv(daemonStateHelper) == "1" {
		Main(daemonHelperArgs())
		return
	}

	dir := t.TempDir()
	config, state := seedInstall(t, dir, defaultInstall)
	unhandled := filepath.Join(dir, "unhandled.log")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	// The child stops itself on this timer: it is the stop signal of this test.
	const childRun = 2 * time.Second
	args := []string{
		"--config", config, "--state", state, "--unhandled", unhandled, "--addr", addr,
		"--duration", childRun.String(), "--debug-api",
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestDaemonShutdownSavePersistsAcrossTheCLI$")
	cmd.Env = append(os.Environ(), daemonStateHelper+"=1", "FREEDO_TEST_DAEMON_ARGS="+strings.Join(args, "\n"))
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	var response struct {
		Queued bool   `json:"queued"`
		Error  string `json:"error"`
	}
	// serving is when the API first answers, which is also when Main arms the
	// --duration timer: the deadlines below measure the stop, not the child boot.
	var serving time.Time
	mutated := false
	deadline := time.Now().Add(5 * time.Second)
	client := &http.Client{Timeout: 100 * time.Millisecond}
	for time.Now().Before(deadline) {
		if resp, err := client.Get("http://" + addr + "/api/stats"); err == nil {
			resp.Body.Close()
			serving = time.Now()
			if code := postJSON(t, http.MethodPost, "http://"+addr+"/api/nodes/VA1000000005/config",
				map[string]any{"tempUnitF": true, "id": 10}, &response); code != http.StatusOK || !response.Queued {
				t.Fatalf("config: got %d %+v", code, response)
			}
			mutated = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !mutated {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("the daemon helper never served the API\n%s", output.String())
	}

	// An rx tap held open over the shutdown: without the closeTaps hook Main wires
	// on the server, it would hold the whole HTTP drain budget and the child would
	// miss the deadline below.
	tapCtx, cancelTap := context.WithCancel(context.Background())
	defer cancelTap()
	tapReq, err := http.NewRequestWithContext(tapCtx, http.MethodGet, "http://"+addr+"/api/debug/rx", nil)
	if err != nil {
		t.Fatal(err)
	}
	tapResp, err := (&http.Client{Timeout: 5 * time.Second}).Do(tapReq)
	if err != nil {
		t.Fatalf("open the rx tap: %v\n%s", err, output.String())
	}
	defer tapResp.Body.Close()
	if tapResp.StatusCode != http.StatusOK {
		t.Fatalf("rx tap: status=%d\n%s", tapResp.StatusCode, output.String())
	}

	// The whole shutdown path is budgeted, so the child must be gone well before
	// the shutdownBudget backstop that would otherwise os.Exit(1) it.
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("daemon helper: %v\n%s", err, output.String())
		}
		// A tap the shutdown does not release holds the drain for the whole
		// httpDrainBudget, which this margin would not survive.
		if elapsed := time.Since(serving); elapsed > childRun+httpDrainBudget*3/4 {
			t.Errorf("the child took %s to stop with one open rx tap\n%s", elapsed, output.String())
		}
	case <-time.After(time.Until(serving.Add(childRun + 8*time.Second))):
		_ = cmd.Process.Kill()
		<-waited
		t.Fatalf("the daemon helper did not exit within 8s of its stop signal\n%s", output.String())
	}

	store := newTestStore(t, state)
	saved, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	bedroom, err := defaultInstall.Resolve("VA1000000005")
	if err != nil {
		t.Fatal(err)
	}
	if key := hex.EncodeToString(bedroom.WireEUI()); !saved.TempUnitF[key] {
		t.Errorf("the shutdown save lost the temperature unit: %+v\n%s", saved.TempUnitF, output.String())
	}
	// The handler save took sequence 1, so a second one proves the shutdown save
	// ran before the engine and the store were closed.
	var seq uint64
	if err := store.db.QueryRow(`SELECT value FROM meta WHERE key = 'save_seq'`).Scan(&seq); err != nil {
		t.Fatalf("stored save sequence: %v", err)
	}
	if seq < 2 {
		t.Errorf("save sequence=%d, want the shutdown save on top of the handler save\n%s", seq, output.String())
	}
}

const daemonNoArgsHelper = "FREEDO_TEST_DAEMON_NOARGS_HELPER"
const daemonOldYAMLHelper = "FREEDO_TEST_DAEMON_OLD_YAML_HELPER"

// The first start: an empty directory, no bootstrap file and no database. The
// daemon commissions its own identity, serves the API and leaves a database
// behind holding a key, a coordinator address and a home id, with no device
// yet. --addr is the only argument, so the test binds no fixed port.
func TestDaemonBootsWithoutArguments(t *testing.T) {
	if os.Getenv(daemonNoArgsHelper) == "1" {
		Main(daemonHelperArgs())
		return
	}

	dir := t.TempDir()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestDaemonBootsWithoutArguments$")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), daemonNoArgsHelper+"=1",
		"FREEDO_TEST_DAEMON_ARGS="+strings.Join([]string{"--addr", addr}, "\n"))
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	booted := false
	client := &http.Client{Timeout: 100 * time.Millisecond}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := client.Get("http://" + addr + "/api/stats"); err == nil {
			resp.Body.Close()
			booted = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !booted {
		cmd.Process.Kill()
		<-done
		t.Fatalf("the daemon did not serve the API on a fresh directory\n%s", output.String())
	}
	// The signal handler is armed just after the banner, so let the child reach
	// it before the only SIGTERM this test sends.
	time.Sleep(250 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the daemon exited with %v\n%s", err, output.String())
		}
	case <-time.After(20 * time.Second):
		cmd.Process.Kill()
		<-done
		t.Fatalf("the daemon did not stop on SIGTERM\n%s", output.String())
	}

	got := output.String()
	for _, want := range []string{
		"daemon: mode=mock", "coordinator=", "short=",
		"the installation is empty", "fails the MIC",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the first-start banner does not carry %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "flags:") {
		t.Errorf("the daemon printed its usage instead of booting:\n%s", got)
	}

	store := newTestStore(t, filepath.Join(dir, "freedo-state.db"))
	in, rev, err := store.LoadInstall(context.Background())
	if err != nil {
		t.Fatalf("the first start left no installation: %v\n%s", err, got)
	}
	if err := in.ValidateCommissionable(true); err != nil {
		t.Fatalf("the generated installation does not validate: %v", err)
	}
	if rev != 1 {
		t.Errorf("install_rev = %d, want 1", rev)
	}
	if key := in.NetworkKey(); len(key) != 16 {
		t.Errorf("generated network key = %d bytes, want 16", len(key))
	}
	eui := in.CoordinatorEUI()
	if len(eui) != 8 || !bytes.HasSuffix(eui, tado.EUISuffix) {
		t.Errorf("generated coordinator EUI = %x, want 8 bytes ending in %x", eui, tado.EUISuffix)
	}
	if in.HomeID == 0 {
		t.Error("generated home id = 0")
	}
	if len(in.Devices) != 0 {
		t.Errorf("devices = %d, want none before the first pairing", len(in.Devices))
	}
}

// A version-1 installation config is refused, never migrated: a daemon that
// quietly started on a generated key while the real one sat in the YAML would
// look like a total radio failure.
func TestDaemonRefusesTheInstallationYAML(t *testing.T) {
	if os.Getenv(daemonOldYAMLHelper) == "1" {
		Main(daemonHelperArgs())
		return
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "freedo.yaml"), []byte(fixtureInstallYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestDaemonRefusesTheInstallationYAML$")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), daemonOldYAMLHelper+"=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the daemon started on a version-1 installation config\n%s", out)
	}
	for _, want := range []string{
		"home_id", "network_key", "coordinator", "devices", "zones", "circuits",
		"options", "version: 1", "freedo-state.db", "/api/install", "utc_offset_min", "pan_id",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("the refusal does not name %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "freedo-state.db")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refusal created a database: %v", err)
	}
}

// TestBootFollowsTheStoredInstallation: RestoreDesiredState brings back the
// d/config documents of the last run, so a restart on a changed installation
// has to end with the boot-time SetInstall or the stale 015e roles would win.
func TestBootFollowsTheStoredInstallation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	if err := install.WriteInstall(path, defaultInstall); err != nil {
		t.Fatal(err)
	}
	// The same installation with zone 1 reporting through its valve:
	// the 015e role bytes of the thermostat move with the election.
	stale, err := tado.Parse([]byte(strings.Replace(fixtureInstallYAML,
		"reporter: RU1000000001}", "reporter: VA1000000003}", 1)))
	if err != nil {
		t.Fatal(err)
	}
	// The thermostat is served a document, and so carries roles, only once its
	// interface module id is known.
	staleEmu := tado.NewEmu(stale.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	setThermostatModule(t, staleEmu, dhwNode, 71)
	staleRoles, err := staleEmu.NodeRoles(dhwNode)
	if err != nil {
		t.Fatal(err)
	}
	store := newTestStore(t, path)
	if err := store.Save(context.Background(), staleEmu.ExportDesiredState(), 1); err != nil {
		t.Fatal(err)
	}

	// Main's boot order, minus the radio and the HTTP server.
	in, _, err := store.LoadInstall(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := in.ValidateCommissionable(true); err != nil {
		t.Fatal(err)
	}
	emu := tado.NewEmu(in.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	if err := restoreDesiredState(store, emu); err != nil {
		t.Fatal(err)
	}
	if _, _, err := emu.SetInstall(in); err != nil {
		t.Fatal(err)
	}

	fresh := tado.NewEmu(in.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	setThermostatModule(t, fresh, dhwNode, 71)
	want, err := fresh.NodeRoles(dhwNode)
	if err != nil {
		t.Fatal(err)
	}
	if len(want) == 0 {
		t.Fatal("the installation derives no 015e records; adjust the fixture")
	}
	if reflect.DeepEqual(staleRoles, want) {
		t.Fatalf("the stale state carries the same 015e roles (%v); adjust the fixture", want)
	}
	got, err := emu.NodeRoles(dhwNode)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the served 015e roles follow the stored state: got %v, want the database's %v", got, want)
	}
}

// bootEmuFromStore repeats Main's boot order on store, minus the radio and the
// HTTP server: the installation, then the desired state, then the reload.
func bootEmuFromStore(t *testing.T, store *stateStore) *tado.EmuState {
	t.Helper()
	in, _, err := store.LoadInstall(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := in.ValidateCommissionable(true); err != nil {
		t.Fatal(err)
	}
	emu := tado.NewEmu(in.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	if err := restoreDesiredState(store, emu); err != nil {
		t.Fatal(err)
	}
	if _, _, err := emu.SetInstall(in); err != nil {
		t.Fatal(err)
	}
	return emu
}

// deviceConfigGET sends one conditional GET of d/<serial>/config from wire and
// returns the answer. etag is the ETag the device cached, nil for none.
func deviceConfigGET(t *testing.T, emu *tado.EmuState, wire []byte, mid uint16, etag []byte) *tado.CoAPMessage {
	t.Helper()
	var options []tado.CoAPOption
	if etag != nil {
		options = []tado.CoAPOption{{Num: 4, Val: etag}}
	}
	request, err := tado.BuildCoAP(tado.CoAPReq{
		Type: "CON", Code: "0.01", MID: mid, Token: []byte{0x21},
		Path: []string{"d", "VA1000000005", "config"}, Options: options,
		SrcEUI: wire, DstEUI: defaultInstall.CoordinatorEUI(),
		SrcPort: 5683, DstPort: 4005,
	})
	if err != nil {
		t.Fatal(err)
	}
	frame, err := tado.BuildData(defaultInstall.NetworkKey(), byte(mid), defaultInstall.CoordinatorEUI(),
		wire[:5], tado.WrapAppPayload(request, uint32(mid), nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range emu.OnFrame(frame.Bytes()) {
		if action.Kind != tado.ActData {
			continue
		}
		data, err := tado.ParseData(defaultInstall.NetworkKey(), action.Bytes)
		if err != nil {
			continue
		}
		if app := tado.ParseApp(data.Plaintext); app != nil && app.CoAP != nil && app.CoAP.MID == mid {
			return app.CoAP
		}
	}
	t.Fatalf("no answer to the d/config GET mid=%04x", mid)
	return nil
}

// TestBootServesTheStoredConfigDocument: a device that rebooted presents the
// ETag it cached, and the document that ETag stamps lives in the store. A daemon
// that comes back on that database answers 2.03 Valid; a document that moved
// after the boot gets 2.05 and a new ETag.
func TestBootServesTheStoredConfigDocument(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	if err := install.WriteInstall(path, defaultInstall); err != nil {
		t.Fatal(err)
	}
	store := newTestStore(t, path)
	bedroom, err := defaultInstall.Resolve("VA1000000005")
	if err != nil {
		t.Fatal(err)
	}
	wire := bedroom.WireEUI()

	// The run before the restart pushes a field the installation does not own, so
	// the served document stops being reproducible from the installation alone.
	first := bootEmuFromStore(t, store)
	fahrenheit := true
	if err := first.EnqueueConfig(hex.EncodeToString(wire), 10, tado.ConfigWrite{TempUnitF: &fahrenheit}); err != nil {
		t.Fatal(err)
	}
	served := deviceConfigGET(t, first, wire, 0x9001, nil)
	if served.Code != "2.05" {
		t.Fatalf("the first GET = %s, want 2.05", served.Code)
	}
	etag, present := served.Option(4)
	if !present || len(etag.Val) == 0 {
		t.Fatal("the 2.05 carries no ETag option")
	}
	cached := append([]byte(nil), etag.Val...)
	if err := store.Save(context.Background(), first.ExportDesiredState(), 1); err != nil {
		t.Fatal(err)
	}

	restarted := bootEmuFromStore(t, store)
	valid := deviceConfigGET(t, restarted, wire, 0x9002, cached)
	if valid.Code != "2.03" {
		t.Errorf("the cached ETag after a restart = %s, want 2.03", valid.Code)
	}
	if len(valid.Payload) != 0 {
		t.Errorf("2.03 Valid carries %d bytes, want none", len(valid.Payload))
	}

	offset := 50
	if err := restarted.EnqueueConfig(hex.EncodeToString(wire), 10, tado.ConfigWrite{CalibOffset: &offset}); err != nil {
		t.Fatal(err)
	}
	changed := deviceConfigGET(t, restarted, wire, 0x9003, cached)
	if changed.Code != "2.05" {
		t.Fatalf("a changed document = %s, want 2.05", changed.Code)
	}
	fresh, present := changed.Option(4)
	if !present || bytes.Equal(fresh.Val, cached) {
		t.Errorf("the changed document keeps the ETag %x, want a new one", cached)
	}
	if len(changed.Payload) == 0 {
		t.Error("the 2.05 carries no document")
	}
}
