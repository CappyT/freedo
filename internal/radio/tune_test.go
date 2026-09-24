package radio

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"freedo/cc1101"
	"freedo/tado"
)

// tunableLiveBridge is a live bridge with a frequency-offset register, like the
// real one. It records every register write.
type tunableLiveBridge struct {
	*orderedLiveBridge
	reg    byte
	writes []byte
	err    error
}

func (b *tunableLiveBridge) FSCTRL0() byte { return b.reg }

func (b *tunableLiveBridge) SetFSCTRL0(value byte) error {
	if b.err != nil {
		return b.err
	}
	b.reg = value
	b.writes = append(b.writes, value)
	return nil
}

func newTunableBridge(reg byte) *tunableLiveBridge {
	return &tunableLiveBridge{
		orderedLiveBridge: &orderedLiveBridge{result: &cc1101.AutoAckResult{Status: cc1101.AutoAckNoFrame}},
		reg:               reg,
	}
}

// tuneDataFrame is a synthetic coordinator Data frame with one FREQEST reading.
// freqSource labels it by its source address.
func tuneDataFrame(source string, freqEst int) *cc1101.RxFrame {
	payload := make([]byte, 64)
	payload[0], payload[1] = 0x69, 0xec
	copy(payload[11:16], source)
	return &cc1101.RxFrame{Payload: payload, CRCOK: true, FreqEst: freqEst, HasFreqEst: true}
}

// captureLog sends the daemon log into a buffer for the length of one test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &logs
}

// The debug line carries the fields that tell the readings of one source apart:
// the receive path, the RSSI, the reading, the time since the previous frame of
// that source and since our own last transmission.
func TestLiveRadioTuneDebugLogsEveryFrame(t *testing.T) {
	now := time.Unix(100, 0)
	bridge := newTunableBridge(0x40)
	radio := &Live{br: bridge, tuneDebug: true, clock: func() time.Time { return now }}

	logs := captureLog(t)

	if _, err := radio.sendFrames([][]byte{{0x61, 0xdc, 0x01}}); err != nil {
		t.Fatalf("sendFrames: %v", err)
	}
	now = now.Add(200 * time.Millisecond)
	first := tuneDataFrame("abcde", -13)
	first.RSSIdBm = -42.5
	radio.tuneDebugFrame(first, "autoack")
	now = now.Add(20 * time.Millisecond)
	radio.tuneDebugFrame(tuneDataFrame("abcde", 7), "autoack")

	want := []string{
		"tune frame src=6162636465 path=autoack rssi=-42.5 len=64 fe=-13 since_src=-1 since_tx=200\n",
		"tune frame src=6162636465 path=autoack rssi=0.0 len=64 fe=7 since_src=20 since_tx=220\n",
	}
	for _, line := range want {
		if !strings.Contains(logs.String(), line) {
			t.Errorf("log=%q, want it to hold %q", logs.String(), line)
		}
	}
	if got := strings.Count(logs.String(), "tune frame "); got != len(want) {
		t.Errorf("two frames gave %d debug lines, want %d", got, len(want))
	}
}

// A CSL inbound frame gets a line too, and the frame still reaches the engine.
func TestLiveRadioTuneDebugLogsCSLInboundFrames(t *testing.T) {
	now := time.Unix(100, 0)
	bridge := &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}}
	bridge.events = append(bridge.events, &cc1101.CSLQueueEvent{Inbound: tuneDataFrame("abcde", 15)})
	radio := &Live{
		br:         bridge,
		tuneDebug:  true,
		cslPending: map[uint16]liveCSLPending{1: {action: tado.Action{Node: "office"}}},
		frames:     make(chan []byte, 1),
		clock:      func() time.Time { return now },
	}

	logs := captureLog(t)

	if _, err := radio.drainCSL(); err != nil {
		t.Fatalf("drainCSL: %v", err)
	}
	want := "tune frame src=6162636465 path=csl rssi=0.0 len=64 fe=15 since_src=-1 since_tx=-1\n"
	if !strings.Contains(logs.String(), want) {
		t.Errorf("log=%q, want it to hold %q", logs.String(), want)
	}
	if len(radio.frames) != 1 {
		t.Errorf("the engine got %d frames, want 1", len(radio.frames))
	}
}

func TestLiveRadioTuneDebugOffLogsNothing(t *testing.T) {
	now := time.Unix(100, 0)
	radio := &Live{br: newTunableBridge(0x40), clock: func() time.Time { return now }}

	logs := captureLog(t)

	radio.tuneDebugFrame(tuneDataFrame("abcde", -13), "recv")
	if strings.Contains(logs.String(), "tune frame ") {
		t.Errorf("log=%q, want no debug line", logs.String())
	}
}

// With no CSL job out the loop still polls the queue, so a frame the bridge
// answered between two receive windows reaches the engine.
func TestLiveRadioLoopPollsCSLQueueWithoutPendingJob(t *testing.T) {
	inbound := tuneDataFrame("abcde", 15)
	bridge := &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}}
	bridge.events = append(bridge.events, &cc1101.CSLQueueEvent{Inbound: inbound})
	radio := &Live{
		br: bridge, coordinator: defaultInstall.CoordinatorEUI(), autoAck: true,
		frames: make(chan []byte, 1), immediate: make(chan liveBatch, 1), async: make(chan liveAsyncBatch, 1),
		cslPending: make(map[uint16]liveCSLPending), cslGroups: make(map[uint16]*liveCSLGroup),
		cslDone: make(chan CSLCompletion, 1), stop: make(chan struct{}), ended: make(chan struct{}),
	}
	go radio.loop()
	defer func() {
		close(radio.stop)
		<-radio.ended
	}()
	select {
	case frame := <-radio.frames:
		if !bytes.Equal(frame, inbound.Payload) {
			t.Fatalf("frame=%x, want %x", frame, inbound.Payload)
		}
	case <-time.After(time.Second):
		t.Fatal("the idle inbound frame did not reach the engine")
	}
}
