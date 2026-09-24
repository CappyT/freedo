package radio

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"freedo/cc1101"
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

type blockingFlushBridge struct {
	entered chan struct{}
	release chan struct{}
	err     error
	closed  atomic.Int32
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

func (b *blockingFlushBridge) RecvAutoAck(time.Duration, []byte, uint16) (*cc1101.AutoAckResult, error) {
	return nil, b.err
}
func (b *blockingFlushBridge) RecvFW(time.Duration) (*cc1101.RxFrame, error) { return nil, b.err }
func (b *blockingFlushBridge) SendTrain(frames [][]byte, _ time.Duration) (int, error) {
	return len(frames), nil
}
func (b *blockingFlushBridge) SendCSLData([]byte, []byte, time.Duration, time.Duration, bool, bool, uint16) (*cc1101.CSLTxResult, error) {
	return &cc1101.CSLTxResult{}, nil
}
func (b *blockingFlushBridge) EnqueueCSLBatch([]cc1101.CSLQueueRequest) error { return nil }
func (b *blockingFlushBridge) PollCSLQueue() (*cc1101.CSLQueueEvent, error)   { return nil, nil }
func (b *blockingFlushBridge) FlushCSLQueue() error {
	b.entered <- struct{}{}
	<-b.release
	return nil
}
func (b *blockingFlushBridge) Close() error {
	b.closed.Add(1)
	return nil
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

type waitingLiveBridge struct {
	started chan struct{}
	closed  bool
}

type endingLiveBridge struct {
	*orderedLiveBridge
	entered chan int
	release chan struct{}
	err     error
	mu      sync.Mutex
	recv    int
}

type failingTrainBridge struct {
	calls int
	sent  int
	err   error
}

func (b *failingTrainBridge) RecvAutoAck(time.Duration, []byte, uint16) (*cc1101.AutoAckResult, error) {
	return nil, nil
}
func (b *failingTrainBridge) RecvFW(time.Duration) (*cc1101.RxFrame, error) { return nil, nil }
func (b *failingTrainBridge) SendTrain([][]byte, time.Duration) (int, error) {
	b.calls++
	return b.sent, b.err
}
func (b *failingTrainBridge) SendCSLData([]byte, []byte, time.Duration, time.Duration, bool, bool, uint16) (*cc1101.CSLTxResult, error) {
	return nil, b.err
}
func (b *failingTrainBridge) Close() error { return nil }

func (b *waitingLiveBridge) RecvAutoAck(wait time.Duration, _ []byte, _ uint16) (*cc1101.AutoAckResult, error) {
	select {
	case b.started <- struct{}{}:
	default:
	}
	time.Sleep(wait)
	return &cc1101.AutoAckResult{Status: cc1101.AutoAckNoFrame}, nil
}

func (b *waitingLiveBridge) RecvFW(time.Duration) (*cc1101.RxFrame, error) { return nil, nil }
func (b *waitingLiveBridge) SendTrain(frames [][]byte, _ time.Duration) (int, error) {
	return len(frames), nil
}
func (b *waitingLiveBridge) SendCSLData([]byte, []byte, time.Duration, time.Duration, bool, bool, uint16) (*cc1101.CSLTxResult, error) {
	return &cc1101.CSLTxResult{Sent: 3}, nil
}
func (b *waitingLiveBridge) Close() error { b.closed = true; return nil }

func (b *endingLiveBridge) RecvAutoAck(time.Duration, []byte, uint16) (*cc1101.AutoAckResult, error) {
	b.mu.Lock()
	b.recv++
	recv := b.recv
	b.mu.Unlock()
	if recv == 1 {
		b.entered <- recv
		<-b.release
		return &cc1101.AutoAckResult{Status: cc1101.AutoAckNoFrame}, nil
	}
	if recv == 2 {
		b.entered <- recv
		<-b.release
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

func (b *orderedLiveBridge) followSnapshot() []bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]bool(nil), b.cslFollow...)
}

func routerAdvertTestFrames() [][]byte {
	ra := []byte{0x49, 0xe8, 0x42}
	return append(tado.BuildRouterAdvertWakeUpBurst(ra[2]), ra)
}

func TestLiveRadioHandlesImmediateBetweenReceives(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	bridge := &orderedLiveBridge{
		result:  &cc1101.AutoAckResult{Status: cc1101.AutoAckNoFrame},
		entered: entered,
		release: release,
	}
	radio := &Live{
		br: bridge, coordinator: defaultInstall.CoordinatorEUI(), autoAck: true, batch: true,
		frames: make(chan []byte, 1), immediate: make(chan liveBatch), async: make(chan liveAsyncBatch, 1),
		stop: make(chan struct{}), ended: make(chan struct{}),
	}
	go radio.loop()
	t.Cleanup(func() { _ = radio.Close() })
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("owner did not enter receive")
	}
	done := make(chan error, 1)
	go func() {
		_, err := radio.SendImmediate([]tado.Action{{Kind: tado.ActBroadcast, Bytes: []byte{0x49, 0xe8, 0x01}}}, true)
		done <- err
	}()
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("immediate batch was not handled between receives")
	}
	events, trains, _ := bridge.snapshot()
	if len(events) == 0 || events[0] != "recv" {
		t.Fatalf("events=%v, want receive first", events)
	}
	for _, event := range events {
		if event != "recv" && event != "train" {
			t.Fatalf("events=%v, want only receive timeouts and train", events)
		}
	}
	if len(trains) != 1 || len(trains[0]) != 1 || !bytes.Equal(trains[0][0], []byte{0x49, 0xe8, 0x01}) {
		t.Fatalf("trains=%x", trains)
	}
}

func TestLiveRadioOwnerExitReleasesPendingSenders(t *testing.T) {
	boom := errors.New("receive failed")
	bridge := &endingLiveBridge{
		orderedLiveBridge: &orderedLiveBridge{},
		entered:           make(chan int, 2),
		release:           make(chan struct{}),
		err:               boom,
	}
	radio := &Live{
		br: bridge, coordinator: defaultInstall.CoordinatorEUI(), autoAck: true, batch: true,
		frames: make(chan []byte, 1), immediate: make(chan liveBatch, 1), async: make(chan liveAsyncBatch, 1),
		stop: make(chan struct{}), ended: make(chan struct{}), rxErrorLimit: 1,
	}
	go radio.loop()
	t.Cleanup(func() { _ = radio.Close() })

	select {
	case recv := <-bridge.entered:
		if recv != 1 {
			t.Fatalf("first receive=%d", recv)
		}
	case <-time.After(time.Second):
		t.Fatal("owner did not enter first receive")
	}
	first := make(chan error, 1)
	go func() {
		_, err := radio.SendImmediate([]tado.Action{{Kind: tado.ActBroadcast, Bytes: []byte{0x49, 0xe8, 0x01}}}, true)
		first <- err
	}()
	deadline := time.Now().Add(time.Second)
	for len(radio.immediate) != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(radio.immediate) != 1 {
		t.Fatal("first immediate was not queued")
	}
	bridge.release <- struct{}{}
	select {
	case err := <-first:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("first immediate did not complete")
	}

	select {
	case recv := <-bridge.entered:
		if recv != 2 {
			t.Fatalf("second receive=%d", recv)
		}
	case <-time.After(time.Second):
		t.Fatal("owner did not enter second receive")
	}
	batchDone := make(chan error, 1)
	go func() { batchDone <- radio.SendBatch([][]byte{{0x49, 0xe8, 0x02}}) }()
	immediateDone := make(chan error, 1)
	go func() {
		_, err := radio.SendImmediate([]tado.Action{{Kind: tado.ActBroadcast, Bytes: []byte{0x49, 0xe8, 0x03}}}, true)
		immediateDone <- err
	}()
	deadline = time.Now().Add(time.Second)
	for (len(radio.async) != 1 || len(radio.immediate) != 1) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(radio.async) != 1 || len(radio.immediate) != 1 {
		t.Fatalf("pending async=%d immediate=%d, want 1/1", len(radio.async), len(radio.immediate))
	}
	bridge.release <- struct{}{}

	for _, done := range []<-chan error{batchDone, immediateDone} {
		select {
		case err := <-done:
			if !errors.Is(err, errLiveEnded) || !strings.Contains(err.Error(), boom.Error()) {
				t.Fatalf("sender error=%v, want owner failure", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("sender remained blocked after owner exit")
		}
	}
	if err := radio.Send([]byte{0x49, 0xe8, 0x04}); !errors.Is(err, errLiveEnded) {
		t.Fatalf("Send after owner exit=%v, want owner failure", err)
	}
}

func TestLiveRadioRXErrorLimitDefaultsToProduction(t *testing.T) {
	if got := (&Live{}).receiveErrorLimit(); got != liveRXErrorLimit {
		t.Fatalf("receive error limit=%d, want %d", got, liveRXErrorLimit)
	}
}

func TestLiveRadioGateDominatesBufferedImmediate(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	bridge := &orderedLiveBridge{
		result:  &cc1101.AutoAckResult{Status: cc1101.AutoAckNoFrame},
		entered: entered,
		release: release,
	}
	radio := &Live{
		br: bridge, coordinator: defaultInstall.CoordinatorEUI(), autoAck: true, batch: true,
		frames: make(chan []byte, 1), immediate: make(chan liveBatch, 1), async: make(chan liveAsyncBatch, 1),
		stop: make(chan struct{}), ended: make(chan struct{}),
	}
	go radio.loop()
	t.Cleanup(func() { _ = radio.Close() })
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("owner did not enter receive")
	}
	pending := make(chan error, 1)
	go func() {
		_, err := radio.SendImmediate([]tado.Action{{Kind: tado.ActBroadcast, Bytes: []byte{0x49, 0xe8, 0x01}}}, true)
		pending <- err
	}()
	deadline := time.Now().Add(time.Second)
	for len(radio.immediate) != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(radio.immediate) != 1 {
		t.Fatal("pending immediate did not fill the buffer")
	}
	if _, err := radio.SendImmediate(nil, false); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case err := <-pending:
		if !errors.Is(err, ErrTXBlocked) {
			t.Fatalf("pending immediate error=%v, want %v", err, ErrTXBlocked)
		}
	case <-time.After(time.Second):
		t.Fatal("pending immediate was not released")
	}
	deadline = time.Now().Add(time.Second)
	for {
		events, trains, _ := bridge.snapshot()
		if len(events) >= 2 && events[0] == "recv" && events[1] == "recv-fw" {
			if len(trains) != 0 {
				t.Fatalf("gate sent trains=%x", trains)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("owner did not enter passive receive: events=%v", events)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestLiveRadioSendsEachDataInSeparateCSLTransaction(t *testing.T) {
	bridge := &orderedLiveBridge{}
	radio := &Live{
		br: bridge, immediate: make(chan liveBatch), async: make(chan liveAsyncBatch, 1), stop: make(chan struct{}),
	}
	frames := []tado.Action{
		{Bytes: []byte{0x69, 0xec, 0x10, 0x01, 0x0a, 0xce, 0xfa, 0x07, 0xc5, 0x1b, 0x00, 0x09, 0xa1, 0xce, 0xfa, 0x07}, CSLTx: true},
		{Bytes: []byte{0x79, 0xec, 0x11, 0x01, 0x0a, 0xce, 0xfa, 0x07, 0xc5, 0x1b, 0x00, 0x09, 0xa1, 0xce, 0xfa, 0x07}, CSLTx: true},
	}
	finished := make(chan bool, 1)
	go func() { finished <- radio.awaitImmediate() }()
	if _, err := radio.SendImmediate(frames, true); err != nil {
		t.Fatal(err)
	}
	if !<-finished {
		t.Fatal("owner did not resume after CSL batch")
	}
	events, trains, _ := bridge.snapshot()
	if len(events) != 2 || events[0] != "csl" || events[1] != "csl" || len(trains) != 0 {
		t.Fatalf("events=%v generic trains=%d", events, len(trains))
	}
	if got := bridge.cslSnapshot(); len(got) != 2 || !bytes.Equal(got[0], frames[0].Bytes) || !bytes.Equal(got[1], frames[1].Bytes) {
		t.Fatalf("CSL frames=%x", got)
	}
	if got := bridge.followSnapshot(); len(got) != 2 || got[0] || got[1] {
		t.Fatalf("follow flags=%v, want [false false]", got)
	}
}

func TestLiveRadioReinjectsActiveGetFollowResponse(t *testing.T) {
	device, err := defaultInstall.Resolve("VA1000000005")
	if err != nil {
		t.Fatal(err)
	}
	emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	if err := emu.EnqueueGet(device.Serial, "d/fw/state", []string{"d", "fw", "state"}, nil); err != nil {
		t.Fatal(err)
	}
	queue := emu.Queue(device.Serial)
	if len(queue) != 1 {
		t.Fatalf("queue=%+v", queue)
	}
	response, err := tado.BuildCoAP(tado.CoAPReq{
		Type: "ACK", Code: "2.05", MID: queue[0].MID, Token: []byte{0x21}, Payload: []byte{0xaa},
		SrcEUI: device.WireEUI(), DstEUI: defaultInstall.CoordinatorEUI(), SrcPort: 5683, DstPort: 4005,
	})
	if err != nil {
		t.Fatal(err)
	}
	follow, err := tado.BuildData(defaultInstall.NetworkKey(), 0x72, defaultInstall.CoordinatorEUI(), device.WireEUI()[:5], tado.WrapAppPayload(response, 701, nil))
	if err != nil {
		t.Fatal(err)
	}
	uplink, err := tado.BuildCoAP(tado.CoAPReq{
		Type: "NON", Code: "0.01", MID: 0x7001, Token: []byte{0x22}, Path: []string{"h", "7654321", "d", "time"},
		SrcEUI: device.WireEUI(), DstEUI: defaultInstall.CoordinatorEUI(), SrcPort: 5683, DstPort: 4005,
	})
	if err != nil {
		t.Fatal(err)
	}
	trigger, err := tado.BuildData(defaultInstall.NetworkKey(), 0x71, defaultInstall.CoordinatorEUI(), device.WireEUI()[:5], tado.WrapAppPayload(uplink, 700, nil))
	if err != nil {
		t.Fatal(err)
	}
	actions := emu.OnFrame(trigger.Bytes())
	var queued tado.Action
	for _, action := range actions {
		if action.QueueAttempt {
			queued = action
		}
	}
	if !queued.WaitResponse {
		t.Fatal("active GET did not request follow response")
	}
	dst := append(append([]byte(nil), queued.Bytes[11:16]...), 0xc5, 0x1b, 0)
	bridge := &orderedLiveBridge{cslResult: &cc1101.CSLTxResult{
		Sent:   23,
		Ack:    &cc1101.RxFrame{Payload: tado.BuildEnhAck(queued.Bytes[2], dst, queued.Bytes[3:11], 0), CRCOK: true},
		Follow: []*cc1101.RxFrame{{Payload: follow.Bytes(), CRCOK: true}},
	}}
	radio := &Live{br: bridge}
	result, err := radio.sendActions([]tado.Action{queued})
	if err != nil {
		t.Fatal(err)
	}
	if got := bridge.followSnapshot(); len(got) != 1 || !got[0] {
		t.Fatalf("follow flags=%v", got)
	}
	if len(result.Acks) != 2 {
		t.Fatalf("reinject=%d, want ACK then follow", len(result.Acks))
	}
	if result.Acks[0][0] != 0x42 || !bytes.Equal(result.Acks[1], follow.Bytes()) {
		t.Fatalf("reinject order=%x / %x", result.Acks[0], result.Acks[1])
	}
	for _, raw := range result.Acks {
		emu.OnFrame(raw)
	}
	code, body, ok := emu.ActiveGetResponse(device.Serial, "d/fw/state")
	if !ok || code != "2.05" || !bytes.Equal(body, []byte{0xaa}) || emu.Pending(device.Serial) != 0 {
		t.Fatalf("response=%q/%x/%t pending=%d", code, body, ok, emu.Pending(device.Serial))
	}
}

func TestLiveRadioCountsNoAckCSLTxAsSubmitted(t *testing.T) {
	bridge := &orderedLiveBridge{}
	radio := &Live{br: bridge}
	action := tado.Action{
		Bytes:        []byte{0x69, 0xec, 0x10, 0x01, 0x0a, 0xce, 0xfa, 0x07, 0xc5, 0x1b, 0x00, 0x09, 0xa1, 0xce, 0xfa, 0x07},
		CSLTx:        true,
		QueueAttempt: true,
	}
	result, err := radio.sendActions([]tado.Action{action})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Submitted) != 1 || !result.Submitted[0].QueueAttempt || len(result.Acks) != 0 {
		t.Fatalf("result=%+v, want submitted no-ACK action", result)
	}
}

func TestLiveRadioCountsFailedCSLTxAsSubmitted(t *testing.T) {
	bridge := &orderedLiveBridge{
		cslErr:    errors.New("CSLTX received invalid Enhanced ACK"),
		cslResult: &cc1101.CSLTxResult{Sent: 23},
	}
	radio := &Live{br: bridge}
	action := tado.Action{
		Bytes:        []byte{0x69, 0xec, 0x10, 0x01, 0x0a, 0xce, 0xfa, 0x07, 0xc5, 0x1b, 0x00, 0x09, 0xa1, 0xce, 0xfa, 0x07},
		CSLTx:        true,
		QueueAttempt: true,
	}
	result, err := radio.sendActions([]tado.Action{action})
	if err == nil {
		t.Fatal("invalid ACK accepted")
	}
	if len(result.Submitted) != 1 || !result.Submitted[0].QueueAttempt {
		t.Fatalf("result=%+v, want submitted failed action", result)
	}
}

func TestLiveRadioMarksCSLQueueAdmissionFailure(t *testing.T) {
	bridge := &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}, enqueueErr: cc1101.ErrCSLQueueFull}
	radio := &Live{br: bridge, cslPending: make(map[uint16]liveCSLPending)}
	action := tado.Action{Bytes: []byte{0x69, 0xec, 0x10, 0x01, 0x0a, 0xce, 0xfa, 0x07, 0xc5, 0x1b, 0x00, 0x09, 0xa1, 0xce, 0xfa, 0x07}, CSLTx: true, QueueAttempt: true}
	result, err := radio.sendActions([]tado.Action{action})
	if !errors.Is(err, cc1101.ErrCSLQueueFull) || !result.CSLQueue || len(result.Accepted) != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestLiveRadioQueuesAnchorsAndReinjectsCSLCompletion(t *testing.T) {
	first := []byte{0x69, 0xec, 0x10, 0x01, 0x0a, 0xce, 0xfa, 0x07, 0xc5, 0x1b, 0x00, 0x09, 0xa1, 0xce, 0xfa, 0x07}
	second := []byte{0x69, 0xec, 0x11, 0x03, 0x0a, 0xce, 0xfa, 0x08, 0xc5, 0x1b, 0x00, 0x09, 0xa1, 0xce, 0xfa, 0x08}
	firstAck := &cc1101.RxFrame{Payload: tado.BuildEnhAck(first[2], append(append([]byte(nil), first[11:16]...), 0xc5, 0x1b, 0), first[3:11], 0), CRCOK: true}
	follow := &cc1101.RxFrame{Payload: append([]byte(nil), second...), CRCOK: true}
	bridge := &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}}
	radio := &Live{br: bridge, cslPending: make(map[uint16]liveCSLPending), cslGroups: make(map[uint16]*liveCSLGroup), cslDone: make(chan CSLCompletion, 1)}
	actions := []tado.Action{
		{Node: "office", Bytes: first, CSLTx: true, QueueAttempt: true},
		{Node: "hall", Bytes: second, CSLTx: true, QueueAttempt: true, WaitResponse: true},
	}
	result, err := radio.sendActions(actions)
	if err != nil || len(result.Accepted) != 2 || len(result.Submitted) != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	queued, _, _ := bridge.snapshotQueue()
	if len(queued) != 2 || queued[0].frame[3:5][0] != 0x01 || queued[1].frame[3:5][0] != 0x03 || queued[0].follow || !queued[1].follow {
		t.Fatalf("queued=%+v", queued)
	}
	bridge.mu.Lock()
	bridge.completions = []*cc1101.CSLQueueCompletion{
		{ID: queued[0].id, Result: &cc1101.CSLTxResult{Sent: 23, Ack: firstAck}},
		{ID: queued[1].id, Result: &cc1101.CSLTxResult{Sent: 23, Follow: []*cc1101.RxFrame{follow}}},
	}
	bridge.mu.Unlock()
	radio.drainCSL()
	radio.drainCSL()
	completion := <-radio.cslDone
	if len(completion.Frames) != 1 || !bytes.Equal(completion.Frames[0], firstAck.Payload) {
		t.Fatalf("completion=%+v", completion)
	}
	radio.drainCSLCompletions()
	completion = <-radio.cslDone
	if len(completion.Frames) != 1 || !bytes.Equal(completion.Frames[0], follow.Payload) {
		t.Fatalf("completion=%+v", completion)
	}
	if len(radio.cslPending) != 0 {
		t.Fatalf("pending=%d", len(radio.cslPending))
	}
}

func TestLiveRadioQueuesColdBootstrapNoAckWithTrainCount(t *testing.T) {
	frame := []byte{0x69, 0xec, 0x10, 0x01, 0x0a, 0xce, 0xfa, 0x07, 0xc5, 0x1b, 0x00, 0x09, 0xa1, 0xce, 0xfa, 0x07}
	bridge := &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}}
	radio := &Live{br: bridge, cslPending: make(map[uint16]liveCSLPending), cslGroups: make(map[uint16]*liveCSLGroup), cslDone: make(chan CSLCompletion, 1)}
	if _, err := radio.sendActions([]tado.Action{{Node: "office", Bytes: frame, CSLTx: true, QueueAttempt: true}}); err != nil {
		t.Fatal(err)
	}
	queued, _, _ := bridge.snapshotQueue()
	bridge.completions = []*cc1101.CSLQueueCompletion{{
		ID:     queued[0].id,
		Result: &cc1101.CSLTxResult{Sent: 255},
	}}
	radio.drainCSL()
	completion := <-radio.cslDone
	if completion.Result == nil || completion.Result.Sent != 255 || completion.Result.Ack != nil || completion.Err != nil {
		t.Fatalf("completion=%+v", completion)
	}
}

func TestLiveRadioQueuesFragmentedCSLBatchWithSharedBootstrap(t *testing.T) {
	first := []byte{0x79, 0xec, 0x10, 0x01, 0x0a, 0xce, 0xfa, 0x07, 0xc5, 0x1b, 0x00, 0x09, 0xa1, 0xce, 0xfa, 0x07}
	second := []byte{0x79, 0xec, 0x11, 0x01, 0x0a, 0xce, 0xfa, 0x07, 0xc5, 0x1b, 0x00, 0x09, 0xa1, 0xce, 0xfa, 0x07}
	bridge := &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}}
	radio := &Live{
		br: bridge, cslPending: make(map[uint16]liveCSLPending), cslGroups: make(map[uint16]*liveCSLGroup),
		bootstrapAttemptAt: make(map[string]time.Time), bootstrapConfirmedAt: make(map[string]time.Time),
	}
	actions := []tado.Action{
		{Bytes: first, CSLTx: true, QueueAttempt: true, QueueMID: 0x7101, QueueTry: 1, WaitResponse: true},
		{Bytes: second, CSLTx: true, QueueAttempt: true, QueueMID: 0x7101, QueueTry: 1, WaitResponse: true},
	}
	if _, err := radio.sendActions(actions); err != nil {
		t.Fatal(err)
	}
	queued, batches, _ := bridge.snapshotQueue()
	if len(batches) != 1 || batches[0] != 2 || len(queued) != 2 {
		t.Fatalf("batches=%v queued=%+v", batches, queued)
	}
	if !queued[0].bootstrap || !queued[1].bootstrap {
		t.Fatalf("fragment bootstrap=%t/%t, want shared cold start", queued[0].bootstrap, queued[1].bootstrap)
	}
}

func TestLiveRadioBatchesServedZoneConfigBlock2Fragments(t *testing.T) {
	device, err := defaultInstall.Resolve("ufficio")
	if err != nil {
		t.Fatal(err)
	}
	emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	block2Actions := func(seq byte, counter uint32, mid uint16) []tado.Action {
		t.Helper()
		pkt, err := tado.BuildCoAP(tado.CoAPReq{
			Type: "CON", Code: "0.01", MID: mid, Token: []byte{seq},
			Path:    []string{"h", "7654321", "z", "3", "config"},
			Options: []tado.CoAPOption{{Num: 23, Val: []byte{0x03}}},
			SrcEUI:  device.WireEUI(), DstEUI: defaultInstall.CoordinatorEUI(), SrcPort: 5683, DstPort: 4005,
		})
		if err != nil {
			t.Fatal(err)
		}
		uplink, err := tado.BuildData(defaultInstall.NetworkKey(), seq, defaultInstall.CoordinatorEUI(), device.WireEUI()[:5], tado.WrapAppPayload(pkt, counter, nil))
		if err != nil {
			t.Fatal(err)
		}
		var actions []tado.Action
		for _, action := range emu.OnFrame(uplink.Bytes()) {
			if action.CSLTx {
				actions = append(actions, action)
			}
		}
		return actions
	}
	actions := block2Actions(0x8b, 0x8b03, 0x8b03)
	if len(actions) < 2 {
		t.Fatalf("Block2 actions=%d, want fragments", len(actions))
	}
	batchID := actions[0].CSLBatchTag
	for _, action := range actions {
		if action.QueueAttempt || !action.HasCSLBatch || action.CSLBatchTag != batchID {
			t.Fatalf("action=%+v, want synchronous Block2 fragment", action)
		}
	}
	nextActions := block2Actions(0x8c, 0x8b04, 0x8b04)
	if len(nextActions) != len(actions) || !nextActions[0].HasCSLBatch || nextActions[0].CSLBatchTag == batchID {
		t.Fatalf("distinct Block2 group=%x/%x actions=%d/%d", batchID, nextActions[0].CSLBatchTag, len(actions), len(nextActions))
	}

	now := time.Unix(200, 0)
	bridge := &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}}
	radio := &Live{
		br: bridge, clock: func() time.Time { return now },
		cslPending: make(map[uint16]liveCSLPending), cslGroups: make(map[uint16]*liveCSLGroup),
		bootstrapConfirmedAt: map[string]time.Time{cslShort(actions[0].Bytes): now},
	}
	result, err := radio.sendActions(actions)
	if err != nil || len(result.Accepted) != len(actions) || len(result.Submitted) != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	queued, batches, _ := bridge.snapshotQueue()
	if !reflect.DeepEqual(batches, []int{len(actions)}) || len(queued) != len(actions) {
		t.Fatalf("batches=%v queued=%d", batches, len(queued))
	}
	for _, job := range queued {
		if !job.bootstrap {
			t.Fatalf("Block2 job=%+v lacks warm-anchor fallback", job)
		}
	}
	if !queued[1].bootstrap {
		t.Fatal("fragment 2 lacks bootstrap fallback after warm fragment 1")
	}
	if len(radio.cslGroups) != 0 {
		t.Fatalf("synchronous Block2 created queue accounting groups=%d", len(radio.cslGroups))
	}

	bridge.completions = make([]*cc1101.CSLQueueCompletion, 0, len(queued))
	for i, job := range queued {
		completion := &cc1101.CSLQueueCompletion{ID: job.id, Result: &cc1101.CSLTxResult{Sent: 255}}
		if i == 0 {
			completion.Result.Sent = 14
		} else if i == 1 {
			src := append(append([]byte(nil), job.frame[11:16]...), tado.EUISuffix...)
			completion.Result.Ack = &cc1101.RxFrame{Payload: tado.BuildEnhAck(job.frame[2], src, job.frame[3:11], 0), CRCOK: true}
		}
		bridge.completions = append(bridge.completions, completion)
	}
	radio.drainCSL()
	if _, ok := radio.bootstrapConfirmedAt[cslShort(actions[0].Bytes)]; !ok {
		t.Fatal("fourth warm miss changed the host confirmation before no-anchor")
	}
	for range queued[1:] {
		radio.drainCSL()
	}
	if len(radio.cslPending) != 0 {
		t.Fatalf("pending=%d", len(radio.cslPending))
	}
	if _, err := radio.sendActions(append(actions, nextActions...)); err != nil {
		t.Fatal(err)
	}
	queued, batches, _ = bridge.snapshotQueue()
	if !reflect.DeepEqual(batches, []int{len(actions), len(actions), len(nextActions)}) || len(queued) != 2*len(actions)+len(nextActions) {
		t.Fatalf("batches=%v queued=%d", batches, len(queued))
	}
	for _, job := range queued[len(actions):] {
		if !job.bootstrap {
			t.Fatalf("explicit fragment group job=%+v did not bootstrap", job)
		}
	}
}

func TestSameCSLBatchUsesRFC4944TagIncludingZero(t *testing.T) {
	first := tado.Action{CSLTx: true, HasCSLBatch: true, CSLBatchTag: 0, Bytes: []byte{0x69, 0xec, 0x10, 0x06, 0x0a}}
	if !sameCSLBatch(first, first) {
		t.Fatal("zero RFC 4944 tag did not identify one group")
	}
	other := first
	other.CSLBatchTag = 1
	if sameCSLBatch(first, other) {
		t.Fatal("distinct RFC 4944 tags coalesced")
	}
	other = first
	other.Bytes = append([]byte(nil), first.Bytes...)
	other.Bytes[3] = 0x6e
	if sameCSLBatch(first, other) {
		t.Fatal("different short addresses coalesced")
	}
}

func TestLiveRadioDefersCSLFramesWhileServingImmediate(t *testing.T) {
	frame := []byte{0x69, 0xec, 0x10, 0x01, 0x0a, 0xce, 0xfa, 0x07, 0xc5, 0x1b, 0x00, 0x09, 0xa1, 0xce, 0xfa, 0x07}
	ack := []byte{0x42, 0xee, 0x10}
	follow1 := []byte{0x69, 0xec, 0x11}
	follow2 := []byte{0x69, 0xec, 0x12}
	bridge := &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}}
	radio := &Live{
		br:         bridge,
		frames:     make(chan []byte, 1),
		cslDone:    make(chan CSLCompletion, 1),
		immediate:  make(chan liveBatch, 1),
		async:      make(chan liveAsyncBatch, 1),
		cslPending: map[uint16]liveCSLPending{1: {action: tado.Action{Node: "office", Bytes: frame}}, 2: {action: tado.Action{Node: "hall", Bytes: frame}}},
		cslGroups:  make(map[uint16]*liveCSLGroup),
		stop:       make(chan struct{}),
		ended:      make(chan struct{}),
	}
	bridge.completions = []*cc1101.CSLQueueCompletion{{
		ID: 1,
		Result: &cc1101.CSLTxResult{
			Sent:   255,
			Ack:    &cc1101.RxFrame{Payload: ack, CRCOK: true},
			Follow: []*cc1101.RxFrame{{Payload: follow1, CRCOK: true}, {Payload: follow2, CRCOK: true}},
		},
	}}
	go radio.loop()
	t.Cleanup(func() { _ = radio.Close() })

	var completion CSLCompletion
	select {
	case completion = <-radio.CSLCompletions():
		if completion.Result == nil || completion.Result.Sent != 255 {
			t.Fatalf("completion=%+v", completion)
		}
	case <-time.After(time.Second):
		t.Fatal("engine did not receive CSL completion")
	}
	for i, want := range [][]byte{ack, follow1, follow2} {
		if i >= len(completion.Frames) || !bytes.Equal(completion.Frames[i], want) {
			t.Fatalf("completion frames=%x want=%x", completion.Frames, want)
		}
	}
	immediateDone := make(chan error, 1)
	go func() {
		_, err := radio.SendImmediate(nil, true)
		immediateDone <- err
	}()
	select {
	case err := <-immediateDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("owner did not serve immediate while CSL remained pending")
	}
}

func TestLiveRadioDefersInboundFramesInOrder(t *testing.T) {
	radio := &Live{frames: make(chan []byte, 1)}
	frames := [][]byte{{1}, {2}, {3}}
	radio.publishAcks(frames)
	if got := <-radio.Frames(); !bytes.Equal(got, frames[0]) {
		t.Fatalf("first=%x", got)
	}
	radio.drainFrames()
	if got := <-radio.Frames(); !bytes.Equal(got, frames[1]) {
		t.Fatalf("second=%x", got)
	}
	radio.drainFrames()
	if got := <-radio.Frames(); !bytes.Equal(got, frames[2]) {
		t.Fatalf("third=%x", got)
	}
}

func TestLiveRadioFlushesQueuedCSLAtBridgeGate(t *testing.T) {
	bridge := &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}}
	radio := &Live{br: bridge, cslPending: map[uint16]liveCSLPending{1: {action: tado.Action{Node: "office"}}}}
	radio.dropCSL(ErrStopped)
	_, _, flushes := bridge.snapshotQueue()
	if flushes != 1 || len(radio.cslPending) != 0 {
		t.Fatalf("flushes=%d pending=%d", flushes, len(radio.cslPending))
	}
}

func TestLiveRadioFlushesCSLQueueBeforeOwnerStarts(t *testing.T) {
	bridge := &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}}
	radio, err := StartLive(bridge, defaultInstall.CoordinatorEUI(), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := radio.Close(); err != nil {
		t.Fatal(err)
	}
	_, _, flushes := bridge.snapshotQueue()
	if flushes < 1 {
		t.Fatalf("flushes=%d", flushes)
	}
}

func TestLiveRadioFlushesUnknownCSLCompletion(t *testing.T) {
	bridge := &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}}
	bridge.events = []*cc1101.CSLQueueEvent{{Completion: &cc1101.CSLQueueCompletion{ID: 2, Result: &cc1101.CSLTxResult{Sent: 23}}}}
	radio := &Live{br: bridge, cslPending: map[uint16]liveCSLPending{1: {action: tado.Action{Node: "office"}}}}
	radio.drainCSL()
	_, _, flushes := bridge.snapshotQueue()
	if flushes != 1 || len(radio.cslPending) != 0 {
		t.Fatalf("flushes=%d pending=%d", flushes, len(radio.cslPending))
	}
}

func TestLiveRadioReinjectsInboundFromQueuedCSL(t *testing.T) {
	frame := []byte{0x69, 0xec, 0x10, 0x01, 0x0a, 0xce, 0xfa, 0x07, 0xc5, 0x1b, 0x00, 0x09, 0xa1, 0xce, 0xfa, 0x07}
	bridge := &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}}
	bridge.events = []*cc1101.CSLQueueEvent{{Inbound: &cc1101.RxFrame{Payload: frame, CRCOK: true}}}
	radio := &Live{br: bridge, cslPending: map[uint16]liveCSLPending{1: {action: tado.Action{Node: "office"}}}, frames: make(chan []byte, 1)}
	radio.drainCSL()
	if got := <-radio.frames; !bytes.Equal(got, frame) {
		t.Fatalf("inbound=%x", got)
	}
}

func TestLiveRadioGroupsFragmentedCSLAccounting(t *testing.T) {
	first := []byte{0x69, 0xec, 0x10, 0x01, 0x0a, 0xce, 0xfa, 0x07, 0xc5, 0x1b, 0x00, 0x09, 0xa1, 0xce, 0xfa, 0x07}
	second := append([]byte(nil), first...)
	second[2] = 0x11
	bridge := &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}}
	radio := &Live{br: bridge, cslPending: make(map[uint16]liveCSLPending), cslGroups: make(map[uint16]*liveCSLGroup), cslDone: make(chan CSLCompletion, 2)}
	result, err := radio.sendActions([]tado.Action{{Node: "office", Bytes: first, CSLTx: true, QueueAttempt: true}, {Node: "office", Bytes: second, CSLTx: true, QueueAttempt: true}})
	if err != nil || len(result.Accepted) != 2 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	queued, _, _ := bridge.snapshotQueue()
	bridge.events = []*cc1101.CSLQueueEvent{
		{Completion: &cc1101.CSLQueueCompletion{ID: queued[0].id, Result: &cc1101.CSLTxResult{Sent: 23}}},
		{Completion: &cc1101.CSLQueueCompletion{ID: queued[1].id, Result: &cc1101.CSLTxResult{Sent: 0}}},
	}
	radio.drainCSL()
	radio.drainCSL()
	completion := <-radio.cslDone
	if completion.Result == nil || completion.Result.Sent == 0 {
		t.Fatalf("completion=%+v", completion)
	}
	select {
	case extra := <-radio.cslDone:
		t.Fatalf("extra completion=%+v", extra)
	default:
	}
}

func TestLiveRadioSeparatesResponseFromFragmentAdmission(t *testing.T) {
	response := []byte{0x69, 0xec, 0x20, 0x01, 0x0a, 0xce, 0xfa, 0x07, 0xc5, 0x1b, 0x00, 0x09, 0xa1, 0xce, 0xfa, 0x07}
	first := append([]byte(nil), response...)
	first[2] = 0x21
	second := append([]byte(nil), response...)
	second[2] = 0x22
	bridge := &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}}
	radio := &Live{br: bridge, cslPending: make(map[uint16]liveCSLPending), cslGroups: make(map[uint16]*liveCSLGroup)}
	_, err := radio.sendActions([]tado.Action{
		{Node: "office", Bytes: response, CSLTx: true},
		{Node: "office", Bytes: first, CSLTx: true, QueueAttempt: true, QueueMID: 41, QueueTry: 1},
		{Node: "office", Bytes: second, CSLTx: true, QueueAttempt: true, QueueMID: 41, QueueTry: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, batches, _ := bridge.snapshotQueue()
	if !reflect.DeepEqual(batches, []int{1, 2}) {
		t.Fatalf("batches=%v", batches)
	}
}

func TestLiveRadioGroupsAllZeroCSLAccounting(t *testing.T) {
	frame := []byte{0x69, 0xec, 0x10, 0x01, 0x0a, 0xce, 0xfa, 0x07, 0xc5, 0x1b, 0x00, 0x09, 0xa1, 0xce, 0xfa, 0x07}
	second := append([]byte(nil), frame...)
	second[2] = 0x11
	bridge := &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}}
	radio := &Live{br: bridge, cslPending: make(map[uint16]liveCSLPending), cslGroups: make(map[uint16]*liveCSLGroup), cslDone: make(chan CSLCompletion, 2)}
	if _, err := radio.sendActions([]tado.Action{{Node: "office", Bytes: frame, CSLTx: true, QueueAttempt: true}, {Node: "office", Bytes: second, CSLTx: true, QueueAttempt: true}}); err != nil {
		t.Fatal(err)
	}
	queued, _, _ := bridge.snapshotQueue()
	bridge.events = []*cc1101.CSLQueueEvent{
		{Completion: &cc1101.CSLQueueCompletion{ID: queued[0].id, Result: &cc1101.CSLTxResult{Sent: 0}}},
		{Completion: &cc1101.CSLQueueCompletion{ID: queued[1].id, Result: &cc1101.CSLTxResult{Sent: 0}}},
	}
	radio.drainCSL()
	radio.drainCSL()
	completion := <-radio.cslDone
	if completion.Result != nil {
		t.Fatalf("completion=%+v", completion)
	}
}

// TestLiveRadioSetsNoHostCSLNeighborCap: the host caps no CSL neighbor count of
// its own, so five neighbors all reach the bridge. The neighbor table is the
// bridge firmware's (CSL_NEIGHBORS = 16 in cc1101_bridge_v2.ino), which answers
// a full table with ErrCSLQueueNeighborFull; this fake bridge does not model
// that limit.
func TestLiveRadioSetsNoHostCSLNeighborCap(t *testing.T) {
	bridge := &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}}
	radio := &Live{br: bridge, cslPending: make(map[uint16]liveCSLPending), cslGroups: make(map[uint16]*liveCSLGroup)}
	actions := make([]tado.Action, 0, 5)
	for short := byte(1); short <= 5; short++ {
		frame := []byte{0x69, 0xec, short, short, 0x04, 0x6a, 0x31, short, 0xc5, 0x1b, 0x00, 0x09, 0xa1, 0xce, 0xfa, short}
		actions = append(actions, tado.Action{Node: fmt.Sprintf("node-%d", short), Bytes: frame, CSLTx: true})
	}
	result, err := radio.sendActions(actions)
	if err != nil || len(result.Accepted) != 5 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	queued, _, _ := bridge.snapshotQueue()
	if len(queued) != 5 {
		t.Fatalf("queued=%d, want 5", len(queued))
	}
	for i, call := range queued {
		if call.frame[3] != byte(i+1) || call.frame[4] != 0x04 {
			t.Fatalf("neighbor %d=%x", i, call.frame[3:5])
		}
	}
}

func TestLogCSLTxIncludesActionMetadata(t *testing.T) {
	got := formatCSLTx(tado.Action{Node: "cucina", Bytes: []byte{0x69, 0xec, 0, 0x06, 0x0a}, QueueAttempt: true, Reason: "downlink active GET d/fw/state seq=d0 try=1"}, true,
		&cc1101.CSLTxResult{Sent: 255}, errors.New("CSLTX has no target sample anchor"))

	for _, want := range []string{
		"node=cucina", "short=0a06", "queue=true", "bootstrap=true", "sent=255", "ack=false",
		`reason="downlink active GET d/fw/state seq=d0 try=1"`, "error=CSLTX has no target sample anchor",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary=%q, missing %q", got, want)
		}
	}
}

func TestLiveRadioStopsBeforeQueuedActionAfterFailedCSLTx(t *testing.T) {
	bridge := &orderedLiveBridge{
		cslErr:    errors.New("CSLTX received invalid Enhanced ACK"),
		cslResult: &cc1101.CSLTxResult{Sent: 23},
	}
	radio := &Live{br: bridge}
	actions := []tado.Action{
		{Bytes: []byte{0x69, 0xec, 0x10, 0x01, 0x0a, 0xce, 0xfa, 0x07, 0xc5, 0x1b, 0x00, 0x09, 0xa1, 0xce, 0xfa, 0x07}, CSLTx: true},
		{Bytes: []byte{0x69, 0xec, 0x11, 0x01, 0x0a, 0xce, 0xfa, 0x07, 0xc5, 0x1b, 0x00, 0x09, 0xa1, 0xce, 0xfa, 0x07}, CSLTx: true, QueueAttempt: true},
	}
	result, err := radio.sendActions(actions)
	if err == nil {
		t.Fatal("invalid ACK accepted")
	}
	if len(result.Submitted) != 1 || result.Submitted[0].QueueAttempt {
		t.Fatalf("result=%+v, want only preceding action", result)
	}
	if got := len(bridge.cslSnapshot()); got != 1 {
		t.Fatalf("CSL sends=%d, want 1", got)
	}
}

func TestLiveRadioCSLBootstrapGlobalCooldown(t *testing.T) {
	now := time.Unix(100, 0)
	radio := &Live{clock: func() time.Time { return now }}
	frame := func(short byte) []byte { return []byte{0x69, 0xec, 0, short, 0x04} }
	if !radio.allowCSLBootstrap(frame(1), false) {
		t.Fatal("first target did not bootstrap")
	}
	if radio.allowCSLBootstrap(frame(2), false) {
		t.Fatal("second target bypassed global cooldown")
	}
	now = now.Add(liveCSLBootstrapGlobalCooldown)
	if !radio.allowCSLBootstrap(frame(2), false) {
		t.Fatal("second target did not bootstrap after global cooldown")
	}
}

func TestLiveRadioCSLBootstrapConfirmationStartsTargetCooldown(t *testing.T) {
	now := time.Unix(100, 0)
	radio := &Live{clock: func() time.Time { return now }}
	frame := []byte{0x69, 0xec, 0, 0x06, 0x0a}
	if !radio.allowCSLBootstrap(frame, false) {
		t.Fatal("first target did not bootstrap")
	}
	radio.confirmCSLBootstrap(frame, true, &cc1101.CSLTxResult{Ack: &cc1101.RxFrame{}}, nil)
	now = now.Add(liveCSLBootstrapTargetCooldown - time.Second)
	if radio.allowCSLBootstrap(frame, false) {
		t.Fatal("confirmed target bypassed cooldown")
	}
	now = now.Add(time.Second)
	if !radio.allowCSLBootstrap(frame, false) {
		t.Fatal("target did not bootstrap after target cooldown")
	}
}

func TestLiveRadioNoAnchorInvalidatesConfirmedBootstrap(t *testing.T) {
	now := time.Unix(100, 0)
	bridge := &orderedLiveBridge{result: &cc1101.AutoAckResult{Status: cc1101.AutoAckNoFrame}}
	radio := &Live{br: bridge, clock: func() time.Time { return now }}
	action := tado.Action{Bytes: []byte{0x69, 0xec, 0, 0x06, 0x0a}, CSLTx: true, QueueAttempt: true}

	bridge.cslResult = &cc1101.CSLTxResult{Sent: 255, Ack: &cc1101.RxFrame{}}
	if _, err := radio.sendActions([]tado.Action{action}); err != nil {
		t.Fatalf("cold bootstrap: %v", err)
	}
	for range 4 {
		now = now.Add(30 * time.Second)
		bridge.cslResult = &cc1101.CSLTxResult{Sent: 23}
		bridge.cslErr = errors.New("CSLTX no ACK")
		if _, err := radio.sendActions([]tado.Action{action}); err == nil {
			t.Fatal("warm miss succeeded")
		}
	}
	now = now.Add(30 * time.Second)
	bridge.cslResult = &cc1101.CSLTxResult{}
	bridge.cslErr = cc1101.ErrCSLTxNoAnchor
	if _, err := radio.sendActions([]tado.Action{action}); !errors.Is(err, cc1101.ErrCSLTxNoAnchor) {
		t.Fatalf("no-anchor error=%v", err)
	}
	if _, ok := radio.bootstrapConfirmedAt[cslShort(action.Bytes)]; ok {
		t.Fatal("no-anchor retained confirmed bootstrap")
	}
	now = now.Add(liveCSLBootstrapGlobalCooldown)
	if _, err := radio.sendActions([]tado.Action{action}); !errors.Is(err, cc1101.ErrCSLTxNoAnchor) {
		t.Fatalf("cold retry error=%v", err)
	}
	if got := bridge.bootstrapSnapshot(); !reflect.DeepEqual(got, []bool{true, false, false, false, false, false, true}) {
		t.Fatalf("bootstrap flags=%v", got)
	}
}

func TestLiveRadioQueuedNoAnchorInvalidatesConfirmedBootstrap(t *testing.T) {
	now := time.Unix(100, 0)
	bridge := &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{result: &cc1101.AutoAckResult{Status: cc1101.AutoAckNoFrame}}}
	radio := &Live{
		br: bridge, clock: func() time.Time { return now },
		bootstrapConfirmedAt: map[string]time.Time{"0a06": now},
		cslPending:           map[uint16]liveCSLPending{1: {action: tado.Action{Bytes: []byte{0x69, 0xec, 0, 0x06, 0x0a}}, bootstrap: false}},
	}
	bridge.completions = []*cc1101.CSLQueueCompletion{{ID: 1, Result: &cc1101.CSLTxResult{}, Err: cc1101.ErrCSLTxNoAnchor}}
	radio.drainCSL()
	if _, ok := radio.bootstrapConfirmedAt["0a06"]; ok {
		t.Fatal("queued no-anchor retained confirmed bootstrap")
	}
	now = now.Add(liveCSLBootstrapGlobalCooldown)
	if !radio.allowCSLBootstrap([]byte{0x69, 0xec, 0, 0x06, 0x0a}, false) {
		t.Fatal("queued no-anchor did not permit cold bootstrap")
	}
}

func TestLiveRadioCSLBootstrapFailureRetriesAfterBackoff(t *testing.T) {
	now := time.Unix(100, 0)
	radio := &Live{clock: func() time.Time { return now }}
	frame := []byte{0x69, 0xec, 0, 0x06, 0x0a}
	if !radio.allowCSLBootstrap(frame, false) {
		t.Fatal("first target did not bootstrap")
	}
	now = now.Add(liveCSLBootstrapRetryCooldown - time.Second)
	if radio.allowCSLBootstrap(frame, false) {
		t.Fatal("failed bootstrap bypassed retry cooldown")
	}
	now = now.Add(time.Second)
	if !radio.allowCSLBootstrap(frame, false) {
		t.Fatal("failed bootstrap did not retry after backoff")
	}
}

// coapRetryMin is the first CoAP retry delay of the daemon.
const coapRetryMin = 0x501 * time.Millisecond

func TestLiveRadioIdentifyRetryBypassesBootstrapCooldown(t *testing.T) {
	now := time.Unix(100, 0)
	bridge := &orderedLiveBridge{
		cslErr:    errors.New("CSLTX received invalid Enhanced ACK"),
		cslResult: &cc1101.CSLTxResult{Sent: 255},
	}
	radio := &Live{br: bridge, clock: func() time.Time { return now }}
	for try := 1; try <= 2; try++ {
		action := tado.Action{
			Bytes:                     []byte{0x69, 0xec, 0x10, 0x06, 0x0a},
			CSLTx:                     true,
			QueueAttempt:              true,
			QueueTry:                  try,
			IdentifyCSLBootstrapRetry: true,
			Reason:                    "downlink identify id=3",
		}
		if _, err := radio.sendActions([]tado.Action{action}); err == nil {
			t.Fatalf("try %d bootstrap error = nil", try)
		}
		now = now.Add(coapRetryMin)
	}
	if got := bridge.bootstrapSnapshot(); len(got) != 2 || !got[0] || !got[1] {
		t.Fatalf("bootstrap flags=%v, want [true true]", got)
	}
}

func TestLiveRadioIdentifyFirstTryBypassesPreviousBootstrapFailure(t *testing.T) {
	now := time.Unix(100, 0)
	bridge := &orderedLiveBridge{
		cslErr:    errors.New("CSLTX received invalid Enhanced ACK"),
		cslResult: &cc1101.CSLTxResult{Sent: 255},
	}
	radio := &Live{br: bridge, clock: func() time.Time { return now }}
	frame := []byte{0x69, 0xec, 0x10, 0x06, 0x0a}
	if _, err := radio.sendActions([]tado.Action{{Bytes: frame, CSLTx: true, QueueAttempt: true}}); err == nil {
		t.Fatal("preceding bootstrap error = nil")
	}
	identify := tado.Action{
		Bytes:                     frame,
		CSLTx:                     true,
		QueueAttempt:              true,
		QueueTry:                  1,
		IdentifyCSLBootstrapRetry: true,
		Reason:                    "downlink identify id=3",
	}
	if _, err := radio.sendActions([]tado.Action{identify}); err == nil {
		t.Fatal("identify bootstrap error = nil")
	}
	if got := bridge.bootstrapSnapshot(); len(got) != 2 || !got[0] || !got[1] {
		t.Fatalf("bootstrap flags=%v, want [true true]", got)
	}
}

func TestLiveRadioQueueGetRetriesBootstrapAfterFailure(t *testing.T) {
	now := time.Unix(100, 0)
	bridge := &orderedLiveBridge{
		cslErr:    errors.New("CSLTX received invalid Enhanced ACK"),
		cslResult: &cc1101.CSLTxResult{Sent: 255},
	}
	radio := &Live{br: bridge, clock: func() time.Time { return now }}
	action := tado.Action{
		Bytes:        []byte{0x69, 0xec, 0x10, 0x06, 0x0a},
		CSLTx:        true,
		QueueAttempt: true,
		Reason:       "downlink active GET d/fw/state seq=1d try=1",
	}
	if _, err := radio.sendActions([]tado.Action{action}); err == nil {
		t.Fatal("first bootstrap error = nil")
	}
	now = now.Add(liveCSLBootstrapRetryCooldown)
	if _, err := radio.sendActions([]tado.Action{action}); err == nil {
		t.Fatal("retry bootstrap error = nil")
	}
	if got := bridge.bootstrapSnapshot(); len(got) != 2 || !got[0] || !got[1] {
		t.Fatalf("bootstrap flags=%v, want [true true] after failed queue GET", got)
	}
}

func TestLiveRadioBatchesCoAPAndQueuedData(t *testing.T) {
	bridge := &orderedLiveBridge{}
	radio := &Live{br: bridge, immediate: make(chan liveBatch), async: make(chan liveAsyncBatch, 1), stop: make(chan struct{})}
	actions := []tado.Action{
		{Bytes: []byte{0x69, 0xec, 1, 1, 4, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}, CSLTx: true},
		{Bytes: []byte{0x69, 0xec, 2, 1, 4, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}, CSLTx: true},
	}
	finished := make(chan bool, 1)
	go func() { finished <- radio.awaitImmediate() }()
	if _, err := radio.SendImmediate(actions, true); err != nil {
		t.Fatal(err)
	}
	if !<-finished {
		t.Fatal("owner did not finish the response batch")
	}
	if got := bridge.cslSnapshot(); len(got) != 2 {
		t.Fatalf("CSL transactions=%d, want 2", len(got))
	}
	if got := bridge.bootstrapSnapshot(); len(got) != 2 || !got[0] || got[1] {
		t.Fatalf("bootstrap flags=%v, want [true false]", got)
	}
}

func TestLiveRadioRoutesRouterAdvertBurst(t *testing.T) {
	bridge := &orderedLiveBridge{}
	radio := &Live{br: bridge}
	if err := radio.sendTrain(routerAdvertTestFrames()); err != nil {
		t.Fatal(err)
	}
	events, trains, _ := bridge.snapshot()
	bursts := bridge.burstSnapshot()
	if len(events) != 1 || events[0] != "ra-burst" || len(trains) != 0 || len(bursts) != 1 {
		t.Fatalf("events=%v trains=%d bursts=%d", events, len(trains), len(bursts))
	}
	if !bytes.Equal(bursts[0], []byte{0x49, 0xe8, 0x42}) {
		t.Fatalf("router advertisement=%x", bursts[0])
	}
}

func TestLiveRadioRouterAdvertBurstFailsWithoutFallback(t *testing.T) {
	boom := errors.New("burst failed")
	for _, bridge := range []*orderedLiveBridge{
		{burstSet: true, burstSent: 2},
		{burstSet: true, burstErr: boom},
	} {
		radio := &Live{br: bridge}
		if err := radio.sendTrain(routerAdvertTestFrames()); err == nil {
			t.Fatal("router advertisement burst succeeded")
		}
		_, trains, _ := bridge.snapshot()
		if len(trains) != 0 {
			t.Fatalf("generic train fallback=%d", len(trains))
		}
	}
}

func TestLiveRadioChunksTrain(t *testing.T) {
	bridge := &orderedLiveBridge{}
	radio := &Live{br: bridge}
	frames := make([][]byte, tado.RouterAdvertWakeUpCount+1)
	for i := range frames {
		frames[i] = []byte{byte(i)}
	}
	if err := radio.sendTrain(frames); err != nil {
		t.Fatal(err)
	}
	_, trains, _ := bridge.snapshot()
	want := []int{48, 48, 48, 48, 48, 15}
	if len(trains) != len(want) {
		t.Fatalf("trains=%d, want %d", len(trains), len(want))
	}
	i := 0
	for n, train := range trains {
		if len(train) != want[n] {
			t.Fatalf("train %d len=%d, want %d", n, len(train), want[n])
		}
		for _, frame := range train {
			if !bytes.Equal(frame, []byte{byte(i)}) {
				t.Fatalf("frame %d=%x, want %x", i, frame, byte(i))
			}
			i++
		}
	}
}

func TestLiveRadioChunksTrainBySerialPayload(t *testing.T) {
	bridge := &orderedLiveBridge{}
	radio := &Live{br: bridge}
	frames := make([][]byte, 5)
	for i := range frames {
		frames[i] = make([]byte, 400)
		frames[i][0] = byte(i)
	}
	if err := radio.sendTrain(frames); err != nil {
		t.Fatal(err)
	}
	_, trains, _ := bridge.snapshot()
	if len(trains) != 3 || len(trains[0]) != 2 || len(trains[1]) != 2 || len(trains[2]) != 1 {
		t.Fatalf("payload chunks=%v", []int{len(trains[0]), len(trains[1]), len(trains[2])})
	}
}

func TestLiveRadioStopsTrainAfterFirstFailure(t *testing.T) {
	boom := errors.New("send failed")
	for _, b := range []*failingTrainBridge{
		{err: boom},
		{sent: 1},
	} {
		radio := &Live{br: b}
		if err := radio.sendTrain(make([][]byte, 255)); err == nil {
			t.Fatal("sendTrain succeeded after a failed chunk")
		}
		if b.calls != 1 {
			t.Fatalf("SendTrain calls=%d, want 1", b.calls)
		}
	}
}

func TestLiveRadioAsyncBatchAcceptsRouterAdvertBurst(t *testing.T) {
	radio := &Live{async: make(chan liveAsyncBatch, 1), stop: make(chan struct{})}
	frames := make([][]byte, tado.RouterAdvertWakeUpCount+1)
	for i := range frames {
		frames[i] = []byte{byte(i)}
	}
	done := make(chan [][]byte, 1)
	go func() {
		batch := <-radio.async
		done <- batch.frames
		batch.done <- nil
	}()
	if err := radio.SendBatch(frames); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if len(got) != len(frames) {
		t.Fatalf("async batch=%d frames, want %d", len(got), len(frames))
	}
}

func TestLiveRadioDropsPendingBatchAfterBridgeLatch(t *testing.T) {
	radio := &Live{async: make(chan liveAsyncBatch, 1)}
	radio.txBlocked.Store(true)
	done := make(chan error, 1)
	radio.async <- liveAsyncBatch{frames: make([][]byte, tado.RouterAdvertWakeUpCount+1), done: done}
	radio.drainAsync()
	if err := <-done; !errors.Is(err, ErrTXBlocked) {
		t.Fatalf("pending batch error=%v, want %v", err, ErrTXBlocked)
	}
}

func TestLiveRadioRejectsSyncBatchWhileAwaitingImmediate(t *testing.T) {
	radio := &Live{
		immediate: make(chan liveBatch),
		async:     make(chan liveAsyncBatch, 1),
		stop:      make(chan struct{}),
	}
	finished := make(chan bool, 1)
	go func() { finished <- radio.awaitImmediate() }()
	errCh := make(chan error, 1)
	go func() { errCh <- radio.SendBatch([][]byte{{1}}) }()
	select {
	case err := <-errCh:
		if !errors.Is(err, errLiveBusy) {
			t.Fatalf("SendBatch error=%v, want %v", err, errLiveBusy)
		}
	case <-time.After(time.Second):
		t.Fatal("SendBatch blocked while the owner awaited immediate work")
	}
	immediate := liveBatch{autoAck: true, done: make(chan liveBatchResult, 1)}
	radio.immediate <- immediate
	if result := <-immediate.done; result.err != nil {
		t.Fatal(result.err)
	}
	if !<-finished {
		t.Fatal("owner did not resume after immediate work")
	}
}

func TestLiveRadioCloseWaitsForReceiveWindow(t *testing.T) {
	bridge := &waitingLiveBridge{started: make(chan struct{}, 1)}
	radio := &Live{
		br:          bridge,
		coordinator: make([]byte, 8),
		autoAck:     true,
		frames:      make(chan []byte, 1),
		immediate:   make(chan liveBatch),
		async:       make(chan liveAsyncBatch, 1),
		stop:        make(chan struct{}),
		ended:       make(chan struct{}),
	}
	go radio.loop()
	select {
	case <-bridge.started:
	case <-time.After(time.Second):
		t.Fatal("owner did not start receive")
	}
	started := time.Now()
	if err := radio.Close(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > liveReceiveWindow+50*time.Millisecond {
		t.Fatalf("close took %s, want at most one receive window", elapsed)
	}
	if !bridge.closed {
		t.Fatal("bridge did not close after the owner stopped")
	}
}

func TestLiveRadioCloseUnblocksPendingCSLQueuePoll(t *testing.T) {
	bridge := &blockingQueuedLiveBridge{
		queuedLiveBridge: &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}},
		entered:          make(chan struct{}, 1),
		unblock:          make(chan struct{}),
	}
	radio := &Live{
		br:         bridge,
		frames:     make(chan []byte, 1),
		immediate:  make(chan liveBatch, 1),
		async:      make(chan liveAsyncBatch, 1),
		cslPending: map[uint16]liveCSLPending{1: {action: tado.Action{Node: "office"}}},
		cslGroups:  make(map[uint16]*liveCSLGroup),
		stop:       make(chan struct{}),
		ended:      make(chan struct{}),
	}
	go radio.loop()
	select {
	case <-bridge.entered:
	case <-time.After(time.Second):
		t.Fatal("owner did not poll pending CSL queue")
	}
	started := time.Now()
	if err := radio.Close(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > liveCloseGrace+100*time.Millisecond {
		t.Fatalf("close took %s", elapsed)
	}
	if !bridge.closed {
		t.Fatal("bridge was not closed to unblock pending poll")
	}
}

func TestLiveRadioStopsAfterStalledCSLQueuePoll(t *testing.T) {
	bridge := &blockingQueuedLiveBridge{
		queuedLiveBridge: &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}},
		entered:          make(chan struct{}, 1),
		unblock:          make(chan struct{}),
	}
	radio := &Live{
		br:        bridge,
		frames:    make(chan []byte, 1),
		immediate: make(chan liveBatch, 1),
		async:     make(chan liveAsyncBatch, 1),
		cslPending: map[uint16]liveCSLPending{1: {action: tado.Action{
			Node: "office", QueueAttempt: true,
			Bytes: []byte{0x69, 0xec, 0x10, 0x06, 0x0a, 0xce, 0xfa, 0x07, 0xc5, 0x1b, 0x00, 0x09, 0xa1, 0xce, 0xfa, 0x07},
		}}},
		cslGroups:      make(map[uint16]*liveCSLGroup),
		cslDone:        make(chan CSLCompletion, 1),
		cslPollTimeout: 10 * time.Millisecond,
		stop:           make(chan struct{}),
		ended:          make(chan struct{}),
	}
	go radio.loop()
	select {
	case <-bridge.entered:
	case <-time.After(time.Second):
		t.Fatal("owner did not enter pending queue poll")
	}
	select {
	case <-radio.ended:
	case <-time.After(time.Second):
		t.Fatal("stalled queue poll did not end the owner")
	}
	select {
	case completion := <-radio.cslDone:
		if completion.Result != nil || !errors.Is(completion.Err, errCSLPollStalled) {
			t.Fatalf("completion=%+v", completion)
		}
	case <-time.After(time.Second):
		t.Fatal("stalled queue poll did not release its pending CSL action")
	}
	if !errors.Is(radio.RadioError(), errLiveEnded) || !errors.Is(radio.RadioError(), errCSLPollStalled) {
		t.Fatalf("radio error=%v", radio.RadioError())
	}
	if err := radio.Close(); err != nil {
		t.Fatal(err)
	}
	if !bridge.closed {
		t.Fatal("bridge was not closed after the owner ended")
	}
}

func TestLiveRadioCloseWaitsForOwnerCleanup(t *testing.T) {
	bridge := &blockingFlushBridge{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
		err:     errors.New("receive failed"),
	}
	radio := &Live{
		br: bridge, cslPending: map[uint16]liveCSLPending{1: {action: tado.Action{Node: "office"}}},
		cslGroups: make(map[uint16]*liveCSLGroup), frames: make(chan []byte), stop: make(chan struct{}), ended: make(chan struct{}),
	}
	loopErr := errors.New("owner failed")
	go radio.finishLoop(&loopErr)
	select {
	case <-bridge.entered:
	case <-time.After(time.Second):
		t.Fatal("owner did not enter CSL cleanup")
	}
	closed := make(chan error, 1)
	go func() { closed <- radio.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned before cleanup: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if got := bridge.closed.Load(); got != 0 {
		t.Fatalf("bridge Close calls=%d before cleanup", got)
	}
	close(bridge.release)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not return after cleanup")
	}
	if got := bridge.closed.Load(); got != 1 {
		t.Fatalf("bridge Close calls=%d, want 1", got)
	}
}

func TestLiveRadioDefersCSLCompletionsWhenConsumerIsFull(t *testing.T) {
	radio := &Live{cslDone: make(chan CSLCompletion, 32)}
	for i := 0; i < 33; i++ {
		radio.publishCSLCompletion(CSLCompletion{Action: tado.Action{Node: fmt.Sprintf("node-%d", i)}})
	}
	if len(radio.cslDeferred) != 1 {
		t.Fatalf("deferred=%d", len(radio.cslDeferred))
	}
	if got := <-radio.cslDone; got.Action.Node != "node-0" {
		t.Fatalf("first=%+v", got)
	}
	radio.drainCSLCompletions()
	for len(radio.cslDone) > 1 {
		<-radio.cslDone
	}
	if got := <-radio.cslDone; got.Action.Node != "node-32" {
		t.Fatalf("second=%+v", got)
	}
}

// TestLiveRadioDropsCRCFailedCSLFrames: a frame the chip received with a failed
// CRC reaches neither the emulator nor the completion frames.
func TestLiveRadioDropsCRCFailedCSLFrames(t *testing.T) {
	good := []byte{0x69, 0xec, 0x10, 0x01, 0x0a, 0xce, 0xfa, 0x07, 0xc5, 0x1b, 0x00, 0x09, 0xa1, 0xce, 0xfa, 0x07}
	bad := append([]byte(nil), good...)
	bad[6] ^= 0x10
	ack := &cc1101.RxFrame{Payload: tado.BuildEnhAck(0x10, defaultInstall.CoordinatorEUI(), defaultInstall.CoordinatorEUI(), 0), CRCOK: true}
	bridge := &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}}
	bridge.events = []*cc1101.CSLQueueEvent{
		{Inbound: &cc1101.RxFrame{Payload: bad}},
		{Completion: &cc1101.CSLQueueCompletion{ID: 1, Result: &cc1101.CSLTxResult{
			Sent: 23, Ack: ack, Follow: []*cc1101.RxFrame{{Payload: bad}, {Payload: good, CRCOK: true}},
		}}},
	}
	radio := &Live{
		br:         bridge,
		cslPending: map[uint16]liveCSLPending{1: {action: tado.Action{Node: "office"}}},
		frames:     make(chan []byte, 1),
		cslDone:    make(chan CSLCompletion, 1),
	}
	radio.drainCSL()
	select {
	case frame := <-radio.frames:
		t.Fatalf("CRC-failed inbound frame published: %x", frame)
	default:
	}
	radio.drainCSL()
	completion := <-radio.cslDone
	if len(completion.Frames) != 2 || !bytes.Equal(completion.Frames[0], ack.Payload) || !bytes.Equal(completion.Frames[1], good) {
		t.Fatalf("frames=%x, want the ACK and the CRC-OK follow frame", completion.Frames)
	}
}
