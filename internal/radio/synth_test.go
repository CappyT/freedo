package radio

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"freedo/cc1101"
	"freedo/tado"
)

var synthTestFrame = []byte{0x69, 0xec, 0x10, 0x06, 0x0a, 0xce, 0xfa, 0x07, 0xc5, 0x1b, 0x00, 0x09, 0xa1, 0xce, 0xfa, 0x07}

// loseSynth runs the hourly calibration on a chip that does not lock.
func loseSynth(t *testing.T) (*Live, *budgetBridge, *stepClock) {
	t.Helper()
	r, bridge, clock := newBudgetRadio(t)
	bridge.synthErr = cc1101.ErrNoSynthLock
	r.synthAt = clock.read()
	clock.advance(liveSynthEvery)
	if err := r.calibrateSynth(); err != nil {
		t.Fatalf("the first failed calibration ended the loop: %v", err)
	}
	if !r.synthFailed.Load() {
		t.Fatal("no synth failure recorded")
	}
	return r, bridge, clock
}

// A chip that does not lock cannot hold the carrier: every transmission waits
// until a calibration locks again, and the refusal reads as a deferral.
func TestNoSynthLockHoldsEveryTransmission(t *testing.T) {
	r, bridge, clock := loseSynth(t)
	if !r.LongTXDeferred() {
		t.Error("a Router Advertisement burst is not deferred")
	}
	cases := map[string]func() error{
		"bootstrap CSL job": func() error {
			_, err := r.sendActions([]tado.Action{{CSLTx: true, HasCSLBatch: true, Bytes: synthTestFrame}})
			return err
		},
		"CSL job on the anchor": func() error {
			_, err := r.sendActions([]tado.Action{{CSLTx: true, Bytes: synthTestFrame}})
			return err
		},
		"Enhanced ACK or beacon train": func() error {
			_, err := r.sendActions([]tado.Action{{Bytes: synthTestFrame}})
			return err
		},
		"async frames": func() error {
			_, err := r.sendFrames([][]byte{synthTestFrame})
			return err
		},
		"Router Advertisement burst": func() error {
			_, err := r.sendFrames(routerAdvertTestFrames())
			return err
		},
		"pair window": func() error {
			req := livePairWindow{window: cc1101.PairWindow{Duration: time.Minute, Key: make([]byte, 16), Coordinator: make([]byte, 8), Count: 3}, done: make(chan livePairResult, 1)}
			r.finishPairWindow(req)
			return (<-req.done).err
		},
	}
	for name, send := range cases {
		err := send()
		if !errors.Is(err, errSynthHeld) || !errors.Is(err, cc1101.ErrAirtimeBudget) {
			t.Errorf("%s: err=%v, want errSynthHeld as a deferral", name, err)
		}
	}
	if a, _ := r.Airtime(); a.OverBudget {
		t.Error("the hold set the over-budget flag")
	}
	queued, _, _ := bridge.snapshotQueue()
	_, trains, _ := bridge.snapshot()
	if len(queued) != 0 || len(trains) != 0 || len(bridge.burstSnapshot()) != 0 || bridge.pairOpens != 0 {
		t.Fatalf("the bridge got queued=%d trains=%d bursts=%d pairOpens=%d", len(queued), len(trains), len(bridge.burstSnapshot()), bridge.pairOpens)
	}

	bridge.synthErr = nil
	clock.advance(liveSynthRetry)
	if err := r.calibrateSynth(); err != nil {
		t.Fatal(err)
	}
	if r.synthFailed.Load() || r.LongTXDeferred() {
		t.Fatal("the radio stays held after a lock")
	}
	if _, err := r.sendActions([]tado.Action{{CSLTx: true, HasCSLBatch: true, Bytes: synthTestFrame}}); err != nil {
		t.Fatalf("after a lock: %v", err)
	}
	if queued, _, _ := bridge.snapshotQueue(); len(queued) != 1 {
		t.Fatalf("queued=%d after a lock, want 1", len(queued))
	}
}

// The retry runs every minute. The third failed retry ends the loop with an
// error that restarts the daemon, and a lock in between clears the count.
func TestSynthRetriesEndTheLoopAfterThreeFailures(t *testing.T) {
	r, bridge, clock := loseSynth(t)
	for try := 1; try <= liveSynthRetries; try++ {
		clock.advance(liveSynthRetry - time.Second)
		if err := r.calibrateSynth(); err != nil || bridge.synthCalls != try {
			t.Fatalf("try %d ran before the minute: calls=%d err=%v", try, bridge.synthCalls, err)
		}
		clock.advance(time.Second)
		err := r.calibrateSynth()
		if try < liveSynthRetries && err != nil {
			t.Fatalf("retry %d ended the loop: %v", try, err)
		}
		if try == liveSynthRetries && !errors.Is(err, errSynthLost) {
			t.Fatalf("retry %d: err=%v, want errSynthLost", try, err)
		}
	}

	r, bridge, clock = loseSynth(t)
	clock.advance(liveSynthRetry)
	_ = r.calibrateSynth()
	bridge.synthErr = nil
	clock.advance(liveSynthRetry)
	_ = r.calibrateSynth()
	bridge.synthErr = cc1101.ErrNoSynthLock
	clock.advance(liveSynthEvery)
	for range liveSynthRetries {
		if err := r.calibrateSynth(); err != nil && !errors.Is(err, errSynthLost) {
			t.Fatal(err)
		}
		clock.advance(liveSynthRetry)
	}
	if err := r.calibrateSynth(); !errors.Is(err, errSynthLost) {
		t.Fatalf("err=%v, want the count to start again after a lock", err)
	}
}

// The loop ends at the third failed retry, the fourth failed calibration, 3
// minutes after the first. The warning and the error say retries.
func TestSynthLoopEndsAtTheFourthFailedCalibrationAndSaysRetries(t *testing.T) {
	r, bridge, clock := loseSynth(t)
	first := clock.read()
	var err error
	for err == nil {
		clock.advance(liveSynthRetry)
		err = r.calibrateSynth()
	}
	if !errors.Is(err, errSynthLost) || bridge.synthCalls != 4 || clock.read().Sub(first) != 3*time.Minute {
		t.Fatalf("err=%v after %d calibrations and %s, want errSynthLost at the fourth, 3 minutes after the first", err, bridge.synthCalls, clock.read().Sub(first))
	}
	if !strings.Contains(err.Error(), "3 retries") {
		t.Errorf("error %q does not name the 3 retries", err)
	}
	if w := strings.Join(r.Warnings(), "\n"); !strings.Contains(w, "after 3 failed retries") {
		t.Errorf("warning %q does not name the 3 failed retries", w)
	}
}

// A hold of the scan or of the synthesizer is a deferral for the callers, like a
// refusal for the airtime budget, but its text must not say that the budget is
// used up: the log of the daemon shows it.
func TestScanAndSynthHoldsDoNotClaimTheBudgetIsUsedUp(t *testing.T) {
	for name, err := range map[string]error{"scan": errScanDeferred, "synthesizer": errSynthHeld} {
		if !errors.Is(err, cc1101.ErrAirtimeBudget) {
			t.Errorf("%s hold is not a deferral", name)
		}
		if strings.Contains(err.Error(), "budget") || strings.Contains(err.Error(), "airtime") {
			t.Errorf("%s hold reads %q", name, err)
		}
		wrapped := fmt.Errorf("csl job: %w", err)
		if !errors.Is(wrapped, cc1101.ErrAirtimeBudget) || strings.Contains(wrapped.Error(), "budget") {
			t.Errorf("%s hold wrapped reads %q", name, wrapped)
		}
	}
}

// A pair window the bridge holds open answers Enhanced Beacon Requests on its
// own: losing the lock closes it.
func TestSynthLossClosesAnOpenPairWindow(t *testing.T) {
	r, bridge, clock := newBudgetRadio(t)
	r.pairWindowChanged(time.Minute)
	bridge.synthErr = cc1101.ErrNoSynthLock
	r.synthAt = clock.read()
	clock.advance(liveSynthEvery)
	if err := r.calibrateSynth(); err != nil {
		t.Fatal(err)
	}
	if bridge.pairCloses != 1 {
		t.Fatalf("pair window closes=%d, want 1", bridge.pairCloses)
	}
	clock.advance(liveSynthRetry)
	_ = r.calibrateSynth()
	if bridge.pairCloses != 1 {
		t.Fatalf("pair window closes=%d after a retry, want 1", bridge.pairCloses)
	}
}

// atomicClock is a clock that the test moves while the loop reads it.
type atomicClock struct{ ns atomic.Int64 }

func (c *atomicClock) read() time.Time         { return time.Unix(0, c.ns.Load()) }
func (c *atomicClock) advance(d time.Duration) { c.ns.Add(int64(d)) }

// In the loop, a chip out of lock only listens: the receive stops answering
// with an Enhanced ACK, jobs queued before are flushed, and the loop ends after
// the retries so that the daemon restarts.
func TestLiveLoopListensOnlyWithoutSynthLockAndEnds(t *testing.T) {
	bridge := &budgetBridge{queuedLiveBridge: &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}}, budget: 100000}
	bridge.synthErr = cc1101.ErrNoSynthLock
	r, err := buildLiveRadio(bridge, defaultInstall.CoordinatorEUI(), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	clock := &atomicClock{}
	clock.ns.Store(time.Unix(1_700_000_000, 0).UnixNano())
	r.clock = clock.read
	go r.loop()
	defer r.Close()

	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("timeout: %s", what)
			}
			time.Sleep(time.Millisecond)
		}
	}
	waitFor("autoack receives", func() bool { return bridge.receives() > 2 })
	clock.advance(liveSynthEvery)
	waitFor("synth failure", func() bool { return r.synthFailed.Load() })
	autoAcks := bridge.receives()
	waitFor("passive receives", func() bool {
		events, _, _ := bridge.snapshot()
		n := 0
		for _, e := range events {
			if e == "recv-fw" {
				n++
			}
		}
		return n > 2
	})
	if got := bridge.receives(); got != autoAcks {
		t.Fatalf("auto-ack receives went from %d to %d while the synthesizer was out of lock", autoAcks, got)
	}

	for try := 1; try <= liveSynthRetries; try++ {
		select {
		case <-r.ended:
			t.Fatalf("the loop ended before retry %d", try)
		default:
		}
		clock.advance(liveSynthRetry)
		want := 1 + try
		waitFor("retry", func() bool {
			bridge.bmu.Lock()
			defer bridge.bmu.Unlock()
			return bridge.synthCalls >= want
		})
	}
	waitFor("the loop end", func() bool {
		select {
		case <-r.ended:
			return true
		default:
			return false
		}
	})
	if err := r.RadioError(); !errors.Is(err, errLiveEnded) || !errors.Is(err, errSynthLost) || !errors.Is(err, cc1101.ErrNoSynthLock) {
		t.Fatalf("radio error=%v, want errSynthLost", err)
	}
}
