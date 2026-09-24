package radio

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"freedo/cc1101"
	"freedo/tado"
)

// flakyPollBridge fails its first CSL queue polls, like a poll that read the
// late reply of an earlier request.
type flakyPollBridge struct {
	*queuedLiveBridge
	fails atomic.Int32
	polls atomic.Int32
}

func (b *flakyPollBridge) PollCSLQueue() (*cc1101.CSLQueueEvent, error) {
	b.polls.Add(1)
	if b.fails.Add(-1) >= 0 {
		return nil, errors.New("CSL queue event is malformed")
	}
	return b.queuedLiveBridge.PollCSLQueue()
}

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
	want := now.Add(1530*time.Millisecond + 2030*time.Millisecond)
	if !radio.cslDeadline.Equal(want) {
		t.Fatalf("deadline=%s want %s", radio.cslDeadline.Sub(now), want.Sub(now))
	}
	radio.cslPending[1] = liveCSLPending{}
	start := now
	now = now.Add(300 * time.Millisecond)
	radio.holdCSLDeadline(start)
	radio.armCSLDeadline(jobs[:1])
	want = want.Add(300*time.Millisecond + 1530*time.Millisecond)
	if !radio.cslDeadline.Equal(want) {
		t.Fatalf("deadline=%s want %s", radio.cslDeadline.Sub(now), want.Sub(now))
	}
	now = want.Add(liveCSLCompletionSlack)
	if radio.expireCSL() {
		t.Fatal("expired at the deadline plus slack")
	}
}

// One failed poll, a late reply or a malformed event, must not end the owner:
// it retries once after the bridge resynced its input.
func TestLiveRadioRetriesOneFailedCSLQueuePoll(t *testing.T) {
	bridge := &flakyPollBridge{queuedLiveBridge: &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}}}
	bridge.fails.Store(1)
	radio, err := StartLive(bridge, defaultInstall.CoordinatorEUI(), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	defer radio.Close()
	deadline := time.Now().Add(time.Second)
	for bridge.polls.Load() < 3 || bridge.receives() < 2 {
		select {
		case <-radio.ended:
			t.Fatalf("owner ended on one failed poll: %v", radio.RadioError())
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
}

// Two failed polls in a row still end the owner.
func TestLiveRadioStopsAfterTwoFailedCSLQueuePolls(t *testing.T) {
	bridge := &flakyPollBridge{queuedLiveBridge: &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}}}
	bridge.fails.Store(2)
	radio, err := StartLive(bridge, defaultInstall.CoordinatorEUI(), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	defer radio.Close()
	select {
	case <-radio.ended:
	case <-time.After(time.Second):
		t.Fatal("owner kept running after two failed polls")
	}
	if !errors.Is(radio.RadioError(), errLiveEnded) {
		t.Fatalf("radio error=%v", radio.RadioError())
	}
}
