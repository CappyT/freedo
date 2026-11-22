package radio

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"freedo/cc1101"
)

// lockLossBridge is a bridge with a register, a pair window and a synthesizer
// that stops locking on demand, as cc1101.Bridge.setFSCTRL0 reports it: the
// register is written and the SCAL after it finds no lock.
type lockLossBridge struct {
	*carrierBridge
	noLock bool
	pair   []time.Duration
}

func (b *lockLossBridge) SetFSCTRL0(v byte) error {
	_ = b.carrierBridge.SetFSCTRL0(v)
	if b.noLock {
		return fmt.Errorf("%w: 3 attempts", cc1101.ErrNoSynthLock)
	}
	return nil
}

func (b *lockLossBridge) SetPairWindow(w cc1101.PairWindow) (int, error) {
	b.pair = append(b.pair, w.Duration)
	return 0, nil
}

// A scan step is out and a pair window opens with no job out, so the restore
// runs before PAIRWIN. The SCAL of that write finds no lock: the window must
// stay closed, and the caller gets the synthesizer error.
func TestPairWindowStaysClosedWhenTheRestoreLosesTheLock(t *testing.T) {
	const home = 0x34
	br := &lockLossBridge{carrierBridge: newCarrierBridge(home, 0)}
	clock := newStepClock()
	r := &Live{br: br, tuner: br, calResults: make(chan CalibrationResult, 1), clock: clock.read}
	r.fsctrl0.Store(uint32(home))
	r.cal.state, r.cal.stored, r.cal.hasStored = CalibrationStored, home, true
	r.cal.quietSince = clock.read()
	r.SetPlant(&testPlant{sources: []string{"devic"}})
	r.tuneStep()
	clock.advance(calibrationQuietBefore)
	r.tuneStep() // scan step 1, the synthesizer locks
	if r.FSCTRL0() == home {
		t.Fatal("no scan step")
	}
	br.noLock = true
	req := livePairWindow{window: cc1101.PairWindow{Duration: 10 * time.Minute}, done: make(chan livePairResult, 1)}
	r.finishPairWindow(req)
	res := <-req.done
	if !errors.Is(res.err, errSynthHeld) {
		t.Errorf("open with a restore that lost the lock: err=%v, want %v", res.err, errSynthHeld)
	}
	if !r.synthFailed.Load() {
		t.Error("synthFailed is not set after the restore lost the lock")
	}
	for i, d := range br.pair {
		if d != 0 {
			t.Errorf("PAIRWIN %d opens a window of %s while the synthesizer is not locked", i, d)
		}
	}
	r.calMu.Lock()
	open := !r.cal.pairUntil.IsZero()
	r.calMu.Unlock()
	if open {
		t.Error("the host believes a pair window is open")
	}
}
