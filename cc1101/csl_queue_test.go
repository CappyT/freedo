package cc1101

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"go.bug.st/serial"
)

type blockingPollPort struct {
	fakePort
	entered chan struct{}
	closed  chan struct{}
	once    sync.Once
}

type idlePollPort struct {
	fakePort
	reads int
}

func (p *idlePollPort) Read([]byte) (int, error) {
	p.reads++
	return 0, nil
}

func (p *blockingPollPort) Read([]byte) (int, error) {
	select {
	case p.entered <- struct{}{}:
	default:
	}
	<-p.closed
	return 0, errors.New("port closed")
}

func (p *blockingPollPort) Close() error {
	p.once.Do(func() { close(p.closed) })
	return nil
}

func TestCSLQueueEnqueueAndCompletion(t *testing.T) {
	data := cslTxData()
	ack := cslTxAck(data)
	completion := []byte{0, 0x00, 0x07, cslTxFlagBootstrap | cslTxFlagFollow, 23, byte(len(ack))}
	completion = append(completion, ack...)
	completion = append(completion, 0, 0)
	p := &fakePort{read: append(autoAckResponse(0, []byte{1}), autoAckResponse(0, completion)...)}
	b := &Bridge{port: p, timeout: time.Millisecond}
	if err := b.EnqueueCSLData(7, data, []byte{0x0a, 0x01}, 50*time.Millisecond, true, true, 161); err != nil {
		t.Fatal(err)
	}
	event, err := b.PollCSLQueue()
	got := event.Completion
	if err != nil || event == nil || got == nil || got.ID != 7 || got.Err != nil || got.Result == nil || got.Result.Ack == nil || got.Result.Sent != 23 {
		t.Fatalf("completion=%+v err=%v", got, err)
	}
	wantPayload := append([]byte{cslQueueEnqueue, 1, 0, 7, 0, byte(len(data) + 7), 0xa1, 0, 0, 50, cslTxFlagBootstrap | cslTxFlagFollow, 0x0a, 0x01, 0xa1, 0}, data...)
	want := append([]byte{reqMagic, cmdCSLQueue, 0, byte(len(wantPayload))}, wantPayload...)
	if len(p.writes) != 2 || !bytes.Equal(p.writes[0], want) || !bytes.Equal(p.writes[1], []byte{reqMagic, cmdCSLQueue, 0, 1, cslQueuePoll}) {
		t.Fatalf("writes=%x", p.writes)
	}
}

func TestCSLQueuePollCloseUnblocksRead(t *testing.T) {
	p := &blockingPollPort{entered: make(chan struct{}, 1), closed: make(chan struct{})}
	b := &Bridge{port: p, timeout: time.Millisecond}
	done := make(chan error, 1)
	go func() {
		_, err := b.PollCSLQueue()
		done <- err
	}()
	select {
	case <-p.entered:
	case <-time.After(time.Second):
		t.Fatal("queue poll did not enter the serial read")
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("queue poll succeeded after its port closed")
		}
	case <-time.After(time.Second):
		t.Fatal("port close did not unblock the queue poll")
	}
}

func TestCSLQueuePollWithinBoundsAnIdleSerialRead(t *testing.T) {
	p := &idlePollPort{}
	b := &Bridge{port: p, timeout: time.Second}
	started := time.Now()
	_, err := b.PollCSLQueueWithin(10 * time.Millisecond)
	if !errors.Is(err, ErrCSLPollTimeout) {
		t.Fatalf("error=%v", err)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("poll ran for %s", elapsed)
	}
	if p.reads == 0 {
		t.Fatal("poll did not attempt a serial read")
	}
}

func TestCSLQueuePollWithinBoundsRealUnixSerialRead(t *testing.T) {
	port, closePTY := silentSerialPTY(t)
	defer closePTY()
	b := &Bridge{port: port, timeout: time.Second}
	started := time.Now()
	_, err := b.PollCSLQueueWithin(30 * time.Millisecond)
	if !errors.Is(err, ErrCSLPollTimeout) {
		t.Fatalf("error=%v", err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("poll ran for %s", elapsed)
	}
}

func silentSerialPTY(t *testing.T) (serial.Port, func()) {
	t.Helper()
	if _, err := exec.LookPath("socat"); err != nil {
		t.Skip("socat is unavailable")
	}
	cmd := exec.Command("socat", "-d", "-d", "PTY,raw,echo=0", "PTY,raw,echo=0")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	cleanup := func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}
	lines := make(chan string, 4)
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()
	paths := make([]string, 0, 2)
	for len(paths) < 2 {
		select {
		case line, ok := <-lines:
			if !ok {
				cleanup()
				t.Fatal("socat ended before creating PTYs")
			}
			if at := strings.LastIndex(line, "PTY is "); at >= 0 {
				paths = append(paths, line[at+len("PTY is "):])
			}
		case <-time.After(time.Second):
			cleanup()
			t.Fatal("socat did not create PTYs")
		}
	}
	port, err := serial.Open(paths[0], &serial.Mode{BaudRate: 921600})
	if err != nil {
		cleanup()
		t.Fatal(err)
	}
	if err := port.SetReadTimeout(5 * time.Millisecond); err != nil {
		port.Close()
		cleanup()
		t.Fatal(err)
	}
	return port, func() {
		_ = port.Close()
		cleanup()
	}
}

func TestCSLQueueFIFOAndSaturationContract(t *testing.T) {
	data := cslTxData()
	var responses []byte
	for id := 1; id <= 32; id++ {
		responses = append(responses, autoAckResponse(0, []byte{1})...)
	}
	responses = append(responses, autoAckResponse(cslQueueFull, nil)...)
	p := &fakePort{read: responses}
	b := &Bridge{port: p, timeout: time.Millisecond}
	for id := 1; id <= 32; id++ {
		if err := b.EnqueueCSLData(uint16(id), data, []byte{0x0a, 0x01}, time.Millisecond, false, false, 0); err != nil {
			t.Fatalf("enqueue %d: %v", id, err)
		}
	}
	if err := b.EnqueueCSLData(33, data, []byte{0x0a, 0x01}, time.Millisecond, false, false, 0); !errors.Is(err, ErrCSLQueueFull) || !strings.Contains(err.Error(), "full") {
		t.Fatalf("saturation error=%v", err)
	}
	for id, write := range p.writes[:32] {
		if got := binary.BigEndian.Uint16(write[6:8]); got != uint16(id+1) {
			t.Fatalf("enqueue order id=%d, want %d", got, id+1)
		}
	}
}

func TestCSLQueueCompletionKeepsCoAPFollow(t *testing.T) {
	data := cslTxData()
	ack := cslTxAck(data)
	follow := cslTxAck(data)
	body := []byte{0, 0x00, 0x09, cslTxFlagFollow, 23, byte(len(ack))}
	body = append(body, ack...)
	body = append(body, 1, byte(len(follow)))
	body = append(body, follow...)
	body = append(body, 0)
	b := &Bridge{port: &fakePort{read: autoAckResponse(0, body)}, timeout: time.Millisecond}
	event, err := b.PollCSLQueue()
	got := event.Completion
	if err != nil || event == nil || got == nil || got.Err != nil || len(got.Result.Follow) != 1 {
		t.Fatalf("completion=%+v err=%v", got, err)
	}
}

func TestCSLQueueColdBootstrapNoAckKeepsTrainCount(t *testing.T) {
	body := []byte{0, 0x00, 0x2a, cslTxFlagBootstrap, 255}
	b := &Bridge{port: &fakePort{read: autoAckResponse(cslTxNoAck, body)}, timeout: time.Millisecond}
	event, err := b.PollCSLQueue()
	if err != nil || event == nil || event.Completion == nil {
		t.Fatalf("event=%+v err=%v", event, err)
	}
	completion := event.Completion
	if completion.Result == nil || completion.Result.Sent != 255 || completion.Result.Ack != nil || completion.Err != nil {
		t.Fatalf("completion=%+v", completion)
	}
}

func TestCSLQueueBatchIsAtomicWhenNeighborHasOneSlot(t *testing.T) {
	data := cslTxData()
	p := &fakePort{read: autoAckResponse(cslQueueFull, nil)}
	b := &Bridge{port: p, timeout: time.Millisecond}
	jobs := []CSLQueueRequest{
		{ID: 1, Frame: data, DstShort: []byte{0x0a, 0x01}, AckTimeout: time.Millisecond},
		{ID: 2, Frame: data, DstShort: []byte{0x0a, 0x01}, AckTimeout: time.Millisecond},
	}
	if err := b.EnqueueCSLBatch(jobs); err == nil || !strings.Contains(err.Error(), "full") {
		t.Fatalf("batch error=%v", err)
	}
	if len(p.writes) != 1 || p.writes[0][4] != cslQueueEnqueue || p.writes[0][5] != 2 {
		t.Fatalf("writes=%x", p.writes)
	}
}

func TestCSLQueueNeighborLimitIsDistinct(t *testing.T) {
	data := cslTxData()
	b := &Bridge{port: &fakePort{read: autoAckResponse(cslQueueNeighborFull, nil)}, timeout: time.Millisecond}
	if err := b.EnqueueCSLData(17, data, []byte{0x0a, 0x01}, time.Millisecond, false, false, 0); !errors.Is(err, ErrCSLQueueNeighborFull) || !strings.Contains(err.Error(), "neighbor table") {
		t.Fatalf("neighbor limit error=%v", err)
	}
}

func TestCSLQueuePollPreservesFollowOverflowUplink(t *testing.T) {
	data := cslTxData()
	raw := append([]byte{byte(len(data))}, data...)
	raw = append(raw, 0, 0x80)
	b := &Bridge{port: &fakePort{read: autoAckResponse(0, append([]byte{1}, raw...))}, timeout: time.Millisecond}
	event, err := b.PollCSLQueue()
	if err != nil || event == nil || event.Inbound == nil || !bytes.Equal(event.Inbound.Payload, data) {
		t.Fatalf("event=%+v err=%v", event, err)
	}
	if event.Inbound.HasFreqEst {
		t.Error("a v2 inbound record carries no FREQEST")
	}
}

// Firmware v2.1 appends FREQEST to the inbound record, which is how the daemon
// tunes on a CSL uplink.
func TestCSLQueuePollReadsInboundFreqEst(t *testing.T) {
	data := cslTxData()
	raw := append([]byte{byte(len(data))}, data...)
	raw = append(raw, 0, 0x80, 0xf3)
	b := &Bridge{port: &fakePort{read: autoAckResponse(0, append([]byte{1}, raw...))}, timeout: time.Millisecond}
	event, err := b.PollCSLQueue()
	if err != nil || event == nil || event.Inbound == nil || !bytes.Equal(event.Inbound.Payload, data) {
		t.Fatalf("event=%+v err=%v", event, err)
	}
	if !event.Inbound.HasFreqEst || event.Inbound.FreqEst != -13 {
		t.Errorf("freqEst=%d,%t want -13", event.Inbound.FreqEst, event.Inbound.HasFreqEst)
	}
}

type timedReply struct {
	delay time.Duration
	data  []byte
}

type pendingReply struct {
	at   time.Time
	data []byte
}

// scriptedPort answers each request with its scripted replies, each one after
// its delay, and honors the read timeout like a real port.
type scriptedPort struct {
	fakePort
	script  [][]timedReply
	pending []pendingReply
	timeout time.Duration
}

func (p *scriptedPort) Write(data []byte) (int, error) {
	n, err := p.fakePort.Write(data)
	if i := len(p.writes) - 1; i < len(p.script) {
		for _, reply := range p.script[i] {
			p.pending = append(p.pending, pendingReply{at: time.Now().Add(reply.delay), data: reply.data})
		}
	}
	return n, err
}

func (p *scriptedPort) Read(dst []byte) (int, error) {
	end := time.Now().Add(p.timeout)
	for {
		for len(p.pending) != 0 && !time.Now().Before(p.pending[0].at) {
			p.read = append(p.read, p.pending[0].data...)
			p.pending = p.pending[1:]
		}
		if len(p.read) != 0 {
			return p.fakePort.Read(dst)
		}
		if !time.Now().Before(end) {
			return 0, nil
		}
		time.Sleep(time.Millisecond)
	}
}

func (p *scriptedPort) SetReadTimeout(timeout time.Duration) error {
	p.timeout = timeout
	return nil
}

func cslQueueNoAckReply(id uint16) []byte {
	return autoAckResponse(cslTxNoAck, []byte{0, byte(id >> 8), byte(id), 0, cslTxMinSent})
}

func pollCompletionID(t *testing.T, b *Bridge) uint16 {
	t.Helper()
	event, err := b.PollCSLQueueWithin(time.Second)
	if err != nil || event == nil || event.Completion == nil {
		t.Fatalf("event=%+v err=%v", event, err)
	}
	return event.Completion.ID
}

// A reply carries no command echo. A reply that lands after the poll deadline
// must not answer the next poll.
func TestCSLQueuePollDropsLateReplyAfterDeadline(t *testing.T) {
	p := &scriptedPort{timeout: 5 * time.Millisecond, script: [][]timedReply{
		{{delay: 40 * time.Millisecond, data: cslQueueNoAckReply(7)}},
		{{data: cslQueueNoAckReply(8)}},
		{{data: autoAckResponse(0, nil)}},
	}}
	b := &Bridge{port: p, timeout: time.Second}
	if _, err := b.PollCSLQueueWithin(20 * time.Millisecond); !errors.Is(err, ErrCSLPollTimeout) {
		t.Fatalf("error=%v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if id := pollCompletionID(t, b); id != 8 {
		t.Fatalf("poll read completion %d, the late reply of the previous poll", id)
	}
	if event, err := b.PollCSLQueueWithin(time.Second); err != nil || event != nil {
		t.Fatalf("event=%+v err=%v, want the empty reply of this poll", event, err)
	}
}

// A poll that reads the late reply of another request finds it malformed. The
// reply to that poll is still in the input, so the next poll has to skip it.
func TestCSLQueuePollResyncsAfterMalformedReply(t *testing.T) {
	p := &scriptedPort{timeout: 5 * time.Millisecond, script: [][]timedReply{
		{{data: autoAckResponse(0, []byte{1})}, {data: cslQueueNoAckReply(7)}},
		{{data: cslQueueNoAckReply(8)}},
	}}
	b := &Bridge{port: p, timeout: time.Second}
	if _, err := b.PollCSLQueueWithin(time.Second); err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("error=%v", err)
	}
	if id := pollCompletionID(t, b); id != 8 {
		t.Fatalf("poll read completion %d, the reply of the previous poll", id)
	}
}
