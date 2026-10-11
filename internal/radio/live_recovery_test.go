package radio

import (
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"freedo/cc1101"
	"freedo/tado"
)

// flakyPollBridge fails its first CSL queue polls with err, like a poll whose
// reply the serial link cut. It counts the failures like the cc1101 bridge.
type flakyPollBridge struct {
	*queuedLiveBridge
	fails atomic.Int32
	polls atomic.Int32
	lost  atomic.Int32
	err   error
}

func (b *flakyPollBridge) PollCSLQueue() (*cc1101.CSLQueueEvent, error) {
	b.polls.Add(1)
	if b.fails.Add(-1) >= 0 {
		b.lost.Add(1)
		return nil, b.err
	}
	return b.queuedLiveBridge.PollCSLQueue()
}

func (b *flakyPollBridge) SerialErrors() uint64 { return uint64(b.lost.Load()) }

var errTruncatedReply = fmt.Errorf("%w: reply truncated", cc1101.ErrLinkFault)

func (b *orderedLiveBridge) receives() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.recv
}

// A bridge that reboots after an enqueue loses the job and its completion, and
// every later poll answers empty. The owner has to give up on the job by its
// firmware guard, report it as failed and go back to receiving.
func TestLiveRadioFailsQueuedCSLWhoseCompletionIsLost(t *testing.T) {
	frame := []byte{0x69, 0xec, 0x10, 0x01, 0x0a, 0xce, 0xfa, 0x07, 0xc5, 0x1b, 0x00, 0x09, 0xa1, 0xce, 0xfa, 0x07}
	bridge := &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}}
	radio, err := buildLiveRadio(bridge, defaultInstall.CoordinatorEUI(), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	var skew atomic.Int64
	radio.clock = func() time.Time { return time.Now().Add(time.Duration(skew.Load())) }
	go radio.loop()
	defer radio.Close()

	result, err := radio.SendImmediate([]tado.Action{{Node: "office", Bytes: frame, CSLTx: true, QueueAttempt: true}}, true)
	if err != nil || len(result.Accepted) != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	// Inside the guard of the job the owner keeps waiting for its completion.
	skew.Store(int64(time.Second))
	select {
	case completion := <-radio.CSLCompletions():
		t.Fatalf("completion before the guard: %+v", completion)
	case <-time.After(50 * time.Millisecond):
	}
	received := bridge.receives()

	skew.Store(int64(time.Minute))
	select {
	case completion := <-radio.CSLCompletions():
		if completion.Result != nil || !errors.Is(completion.Err, errCSLCompletionLost) {
			t.Fatalf("completion=%+v", completion)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a lost CSL completion kept the job pending")
	}
	deadline := time.Now().Add(time.Second)
	for bridge.receives() == received {
		if time.Now().After(deadline) {
			t.Fatal("receive did not resume after the lost completion")
		}
		time.Sleep(time.Millisecond)
	}
	if _, _, flushes := bridge.snapshotQueue(); flushes < 2 {
		t.Fatalf("flushes=%d, want the start flush and the expiry flush", flushes)
	}
	if err := radio.RadioError(); err != nil {
		t.Fatalf("radio error=%v", err)
	}
}

// The deadline of the pending jobs is the sum of their firmware guards, since
// the bridge runs one job at a time, plus the time an immediate batch holds it.
func TestLiveRadioCSLDeadlineAddsJobGuards(t *testing.T) {
	now := time.Unix(1000, 0)
	radio := &Live{clock: func() time.Time { return now }, cslPending: make(map[uint16]liveCSLPending)}
	jobs := []cc1101.CSLQueueRequest{
		{AckTimeout: liveCSLTxAckTimeout, Bootstrap: true},
		{AckTimeout: liveCSLTxAckTimeout, WaitResponse: true},
	}
	radio.armCSLDeadline(jobs)
	// Wake window: one period, two widest guards and the longest listen.
	const plain = 2007808*time.Microsecond + liveCSLTxAckTimeout + 400*time.Millisecond
	want := now.Add(plain + plain + 500*time.Millisecond)
	if !radio.cslDeadline.Equal(want) {
		t.Fatalf("deadline=%s want %s", radio.cslDeadline.Sub(now), want.Sub(now))
	}
	radio.cslPending[1] = liveCSLPending{}
	start := now
	now = now.Add(300 * time.Millisecond)
	radio.holdCSLDeadline(start)
	radio.armCSLDeadline(jobs[:1])
	want = want.Add(300*time.Millisecond + plain)
	if !radio.cslDeadline.Equal(want) {
		t.Fatalf("deadline=%s want %s", radio.cslDeadline.Sub(now), want.Sub(now))
	}
	now = want.Add(liveCSLCompletionSlack)
	if radio.expireCSL() {
		t.Fatal("expired at the deadline plus slack")
	}
}

// A reply that the serial link cut or lost must not end the owner, however
// often it happens: the owner polls again and the lost event stays lost.
func TestLiveRadioSurvivesLostCSLQueuePolls(t *testing.T) {
	bridge := &flakyPollBridge{queuedLiveBridge: &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}}, err: errTruncatedReply}
	bridge.fails.Store(5)
	radio, err := StartLive(bridge, defaultInstall.CoordinatorEUI(), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	defer radio.Close()
	deadline := time.Now().Add(3 * time.Second)
	for bridge.polls.Load() < 8 || bridge.receives() < 2 {
		select {
		case <-radio.ended:
			t.Fatalf("owner ended on lost polls: %v", radio.RadioError())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("polls=%d receives=%d", bridge.polls.Load(), bridge.receives())
		}
		time.Sleep(time.Millisecond)
	}
	if err := radio.RadioError(); err != nil {
		t.Fatalf("radio error=%v", err)
	}
	if got := radio.SerialErrors(); got != 5 {
		t.Fatalf("serial errors=%d, want 5", got)
	}
}

// A link that loses every reply ends the owner once nothing worked for
// linkDeadAfter, and not before.
func TestLiveRadioStopsWhenTheLinkStaysDead(t *testing.T) {
	bridge := &flakyPollBridge{queuedLiveBridge: &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}}, err: errTruncatedReply}
	bridge.fails.Store(1 << 20)
	radio, err := buildLiveRadio(bridge, defaultInstall.CoordinatorEUI(), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	radio.linkDeadAfter = 400 * time.Millisecond
	started := time.Now()
	go radio.loop()
	defer radio.Close()
	select {
	case <-radio.ended:
	case <-time.After(5 * time.Second):
		t.Fatal("owner kept running on a dead link")
	}
	if elapsed := time.Since(started); elapsed < radio.linkDeadAfter {
		t.Fatalf("owner ended after %s, before linkDeadAfter", elapsed)
	}
	if err := radio.RadioError(); !errors.Is(err, errLiveEnded) || !errors.Is(err, cc1101.ErrLinkFault) {
		t.Fatalf("radio error=%v", err)
	}
}

// deadRXBridge loses the reply of every receive window.
type deadRXBridge struct{ *queuedLiveBridge }

func (b *deadRXBridge) RecvAutoAck(time.Duration, []byte, uint16) (*cc1101.AutoAckResult, error) {
	return nil, errTruncatedReply
}

func TestLiveRadioStopsWhenReceiveWindowsStayDead(t *testing.T) {
	bridge := &deadRXBridge{&queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}}}
	radio, err := buildLiveRadio(bridge, defaultInstall.CoordinatorEUI(), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	radio.linkDeadAfter = 400 * time.Millisecond
	started := time.Now()
	go radio.loop()
	defer radio.Close()
	select {
	case <-radio.ended:
	case <-time.After(5 * time.Second):
		t.Fatal("owner kept running with every receive window lost")
	}
	if elapsed := time.Since(started); elapsed < radio.linkDeadAfter {
		t.Fatalf("owner ended after %s, before linkDeadAfter", elapsed)
	}
	if err := radio.RadioError(); !errors.Is(err, errLiveEnded) || !errors.Is(err, cc1101.ErrLinkFault) {
		t.Fatalf("radio error=%v", err)
	}
}

// An error that is not a lost reply, a port that failed, still ends the owner
// at once.
func TestLiveRadioStopsOnPortErrorOfCSLQueuePoll(t *testing.T) {
	bridge := &flakyPollBridge{queuedLiveBridge: &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}}, err: errors.New("input/output error")}
	bridge.fails.Store(1)
	radio, err := StartLive(bridge, defaultInstall.CoordinatorEUI(), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	defer radio.Close()
	select {
	case <-radio.ended:
	case <-time.After(time.Second):
		t.Fatal("owner kept running after a port error")
	}
	if !errors.Is(radio.RadioError(), errLiveEnded) {
		t.Fatalf("radio error=%v", radio.RadioError())
	}
}

// configBridge reports whether the chip still holds the register set, like the
// IOCFG0 read back of cc1101.Bridge.Configured.
type configBridge struct {
	*queuedLiveBridge
	configured atomic.Bool
	checks     atomic.Int32
}

func (b *configBridge) Configured() (bool, error) {
	b.checks.Add(1)
	return b.configured.Load(), nil
}

// A bridge that rebooted answers every request, but the chip listens on reset
// defaults: every poll and every window is empty. The owner reads the register
// set back after a run of empty windows and ends, so the daemon restarts and
// configures the chip again.
func TestLiveRadioEndsWhenQuietBridgeLostRegisters(t *testing.T) {
	bridge := &configBridge{queuedLiveBridge: &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}}}
	radio, err := StartLive(bridge, defaultInstall.CoordinatorEUI(), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	defer radio.Close()
	select {
	case <-radio.ended:
	case <-time.After(2 * time.Second):
		t.Fatalf("owner kept running on a chip that lost its registers, checks=%d", bridge.checks.Load())
	}
	if err := radio.RadioError(); !errors.Is(err, errLiveEnded) || !errors.Is(err, errBridgeUnconfigured) {
		t.Fatalf("radio error=%v", radio.RadioError())
	}
}

// A configured chip keeps the owner running, and the check stays rare: one
// read back per run of empty windows.
func TestLiveRadioKeepsRunningWhenRegistersHold(t *testing.T) {
	bridge := &configBridge{queuedLiveBridge: &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}}}
	bridge.configured.Store(true)
	radio, err := StartLive(bridge, defaultInstall.CoordinatorEUI(), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	defer radio.Close()
	deadline := time.Now().Add(2 * time.Second)
	for bridge.receives() < 100 {
		if time.Now().After(deadline) {
			t.Fatalf("receives=%d", bridge.receives())
		}
		time.Sleep(time.Millisecond)
	}
	if err := radio.RadioError(); err != nil {
		t.Fatalf("radio error=%v", err)
	}
	checks := int(bridge.checks.Load())
	if checks == 0 || checks > bridge.receives()/10 {
		t.Fatalf("checks=%d over %d receives", checks, bridge.receives())
	}
}

// A job whose completion never came is the first sign of a reboot. The owner
// reads the register set back right after it expires the job, without waiting
// for a run of empty windows.
func TestLiveRadioChecksRegistersAfterLostCompletion(t *testing.T) {
	frame := []byte{0x69, 0xec, 0x10, 0x01, 0x0a, 0xce, 0xfa, 0x07, 0xc5, 0x1b, 0x00, 0x09, 0xa1, 0xce, 0xfa, 0x07}
	bridge := &configBridge{queuedLiveBridge: &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}}}
	bridge.configured.Store(true)
	radio, err := buildLiveRadio(bridge, defaultInstall.CoordinatorEUI(), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	var skew atomic.Int64
	radio.clock = func() time.Time { return time.Now().Add(time.Duration(skew.Load())) }
	go radio.loop()
	defer radio.Close()
	if _, err := radio.SendImmediate([]tado.Action{{Node: "office", Bytes: frame, CSLTx: true, QueueAttempt: true}}, true); err != nil {
		t.Fatal(err)
	}
	bridge.configured.Store(false)
	received := bridge.receives()
	skew.Store(int64(time.Minute))
	select {
	case <-radio.ended:
	case <-time.After(2 * time.Second):
		t.Fatal("owner kept running after a lost completion on a chip that lost its registers")
	}
	if n := bridge.receives() - received; n >= 10 {
		t.Fatalf("owner ended after %d more windows, want the check at the expiry", n)
	}
}

// slowTrainBridge moves the owner clock while it sends a train, like a long
// train that holds the bridge scheduler.
type slowTrainBridge struct {
	*queuedLiveBridge
	skew *atomic.Int64
	hold time.Duration
}

func (b *slowTrainBridge) SendTrain(frames [][]byte, timeout time.Duration) (int, error) {
	b.skew.Add(int64(b.hold))
	return b.queuedLiveBridge.SendTrain(frames, timeout)
}

// A batch that answers a received frame runs inside awaitImmediate. A train it
// sends after a queued job holds the bridge, so the job deadline moves by the
// time the batch took, as it does for a batch the loop takes between windows.
func TestLiveRadioAwaitedBatchHoldsCSLDeadline(t *testing.T) {
	frame := []byte{0x69, 0xec, 0x10, 0x01, 0x0a, 0xce, 0xfa, 0x07, 0xc5, 0x1b, 0x00, 0x09, 0xa1, 0xce, 0xfa, 0x07}
	uplink := &cc1101.RxFrame{Payload: []byte{0x69, 0xec, 0x11}, CRCOK: true}
	var skew atomic.Int64
	bridge := &slowTrainBridge{
		queuedLiveBridge: &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{result: &cc1101.AutoAckResult{Status: cc1101.AutoAckSent, Frame: uplink}}},
		skew:             &skew,
		hold:             5 * time.Second,
	}
	radio, err := buildLiveRadio(bridge, defaultInstall.CoordinatorEUI(), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	radio.clock = func() time.Time { return time.Now().Add(time.Duration(skew.Load())) }
	go radio.loop()
	defer radio.Close()
	select {
	case <-radio.Frames():
	case <-time.After(time.Second):
		t.Fatal("no uplink")
	}
	actions := []tado.Action{
		{Node: "office", Bytes: frame, CSLTx: true, QueueAttempt: true},
		{Node: "office", Bytes: []byte{0x41, 0xd8, 0x01, 0xcd, 0xab}},
	}
	if result, err := radio.SendImmediate(actions, true); err != nil || len(result.Accepted) != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	select {
	case completion := <-radio.CSLCompletions():
		t.Fatalf("job expired while the train held the bridge: %+v", completion)
	case <-time.After(100 * time.Millisecond):
	}
	skew.Add(int64(time.Minute))
	select {
	case completion := <-radio.CSLCompletions():
		if !errors.Is(completion.Err, errCSLCompletionLost) {
			t.Fatalf("completion=%+v", completion)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the job never expired")
	}
}
