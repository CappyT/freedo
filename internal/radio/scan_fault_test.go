package radio

import (
	"fmt"
	"testing"
	"time"

	"freedo/cc1101"
)

// lostReplyCarrier models cc1101.Bridge.setFSCTRL0 when the link loses the reply
// of the register write: the chip takes the value, the write returns an error
// before the driver records it, so FSCTRL0() of the bridge still reports the old
// one.
type lostReplyCarrier struct {
	*carrierBridge
	n      int
	lose   map[int]bool
	cached byte
}

func (b *lostReplyCarrier) FSCTRL0() byte { return b.cached }

func (b *lostReplyCarrier) SetFSCTRL0(v byte) error {
	b.n++
	_ = b.carrierBridge.SetFSCTRL0(v)
	if b.lose[b.n] {
		return fmt.Errorf("%w: reply lost", cc1101.ErrLinkFault)
	}
	b.cached = v
	return nil
}

func (b *lostReplyCarrier) chip() byte { return b.carrierBridge.FSCTRL0() }

func newLostReplyPlant(lose map[int]bool) (*Live, *lostReplyCarrier, *stepClock) {
	const home = 0x34
	br := &lostReplyCarrier{carrierBridge: newCarrierBridge(home, 0), lose: lose, cached: home}
	clock := newStepClock()
	r := &Live{tuner: br, calResults: make(chan CalibrationResult, 1), clock: clock.read}
	r.fsctrl0.Store(uint32(home))
	r.cal.state, r.cal.stored, r.cal.hasStored = CalibrationStored, home, true
	r.cal.quietSince = clock.read()
	r.SetPlant(&testPlant{sources: []string{"devic"}})
	return r, br, clock
}

// The reply of the first scan step is lost: the chip holds the step and the host
// believes the register is home. The scan ends, and the restore writes home with
// no comparison against the host value. Until it works the radio counts as off
// home.
func TestLostReplyOfTheFirstScanStepIsRestored(t *testing.T) {
	const home = 0x34
	r, br, clock := newLostReplyPlant(map[int]bool{1: true})
	r.tuneStep()
	clock.advance(calibrationQuietBefore)
	r.tuneStep() // scan step 1: the write lands, its reply is lost
	if br.chip() == home {
		t.Fatalf("chip %#02x after the first step, want the step", br.chip())
	}
	if !r.LongTXDeferred() {
		t.Fatal("long transmissions allowed while the chip holds an unknown register")
	}
	r.tuneStep() // the restore
	if br.chip() != home || r.FSCTRL0() != home || r.LongTXDeferred() {
		t.Fatalf("chip %#02x host %#02x deferred=%v, want home and no deferral", br.chip(), r.FSCTRL0(), r.LongTXDeferred())
	}
	before := br.n
	clock.advance(calibrationScanHold - time.Minute)
	r.tuneStep()
	if br.n != before {
		t.Fatal("a scan step started inside the hold")
	}
}

// A pair window that opens while the scan holds a step lost by the link ends the
// scan too, and the chip goes home.
func TestPairWindowRestoresALostFirstStep(t *testing.T) {
	const home = 0x34
	r, br, clock := newLostReplyPlant(map[int]bool{1: true})
	r.tuneStep()
	clock.advance(calibrationQuietBefore)
	r.tuneStep()
	clock.advance(time.Minute)
	r.pairWindowChanged(10 * time.Minute)
	for range 20 {
		r.tuneStep()
		if br.chip() != home && !r.LongTXDeferred() {
			t.Fatalf("chip %#02x off home with long transmissions allowed", br.chip())
		}
		clock.advance(time.Minute)
	}
	if br.chip() != home {
		t.Fatalf("chip %#02x after the pair window, want home", br.chip())
	}
}

// A pair window with no job out sends the register home before it opens.
func TestPairWindowRestoresHomeBeforeTheWindowOpens(t *testing.T) {
	r, br, clock := newLostReplyPlant(nil)
	r.tuneStep()
	clock.advance(calibrationQuietBefore)
	r.tuneStep()
	if br.chip() == 0x34 {
		t.Fatal("no scan step")
	}
	r.finishPairWindow(livePairWindow{
		window: cc1101.PairWindow{Duration: 5 * time.Minute, Key: make([]byte, 16), Coordinator: make([]byte, 8), Count: 1},
		done:   make(chan livePairResult, 1),
	})
	if br.chip() != 0x34 {
		t.Fatalf("chip %#02x when the window opened, want home", br.chip())
	}
}

// POST /api/radio/calibrate during a scan: the reset write to TuneDefault fails
// once. The radio tries it again, and the measurement waits for it.
func TestRecalibrateDuringAScanRetriesAFailedResetWrite(t *testing.T) {
	br := newCarrierBridge(cc1101.TuneDefault, 0)
	fb := &failingCarrier{carrierBridge: br, fail: map[int]bool{2: true}}
	clock := newStepClock()
	r := &Live{tuner: fb, calResults: make(chan CalibrationResult, 1), clock: clock.read}
	r.fsctrl0.Store(uint32(br.FSCTRL0()))
	r.cal.state, r.cal.stored, r.cal.hasStored = CalibrationStored, cc1101.TuneDefault, true
	r.cal.quietSince = clock.read()
	r.SetPlant(&testPlant{sources: []string{"devic"}})
	r.tuneStep()
	clock.advance(calibrationQuietBefore)
	r.tuneStep() // scan step 1
	r.Recalibrate()
	r.tuneStep() // the reset write fails
	if r.FSCTRL0() == cc1101.TuneDefault || !r.LongTXDeferred() {
		t.Fatalf("FSCTRL0 %#02x deferred=%v after the failed reset, want the scan step and the deferral", r.FSCTRL0(), r.LongTXDeferred())
	}
	clock.advance(calibrationRestoreRetry)
	r.tuneStep()
	if r.FSCTRL0() != cc1101.TuneDefault || r.LongTXDeferred() {
		t.Fatalf("FSCTRL0 %#02x deferred=%v after the retry, want TuneDefault and no deferral", r.FSCTRL0(), r.LongTXDeferred())
	}
}

// A plant with one device that sends every few minutes: a frame decodes within
// 30 units of the carrier and home is right. A candidate that does not settle
// holds the scan for an hour, so the radio is not off home most of the time.
func TestSparseDeviceStaysMostlyOnHome(t *testing.T) {
	for _, period := range []int{5, 9, 11, 15, 30, 60} {
		const home = 0x34
		br := newCarrierBridge(home, 0)
		clock := newStepClock()
		r := &Live{tuner: br, calResults: make(chan CalibrationResult, 1), clock: clock.read}
		r.fsctrl0.Store(uint32(home))
		r.cal.state, r.cal.stored, r.cal.hasStored = CalibrationStored, home, true
		r.cal.quietSince = clock.read()
		r.SetPlant(&testPlant{sources: []string{"devic"}})
		var offHome, offAllowed int
		const total = 24 * 60
		for minute := 0; minute < total; minute++ {
			if minute%period == 0 {
				offset := int(int8(home)) - int(int8(r.FSCTRL0()))
				if offset >= -30 && offset <= 30 {
					r.collectTune(tuneDataFrame("devic", max(min(offset, 21), -21)))
				}
			}
			r.tuneStep()
			if r.FSCTRL0() != home {
				offHome++
				if !r.LongTXDeferred() {
					offAllowed++
				}
			}
			clock.advance(time.Minute)
		}
		t.Logf("uplink every %2d min: off home %d of %d min (%d%%)", period, offHome, total, 100*offHome/total)
		if offAllowed != 0 {
			t.Errorf("uplink every %d min: %d minutes off home with long transmissions allowed", period, offAllowed)
		}
		if share := 100 * offHome / total; share > 40 {
			t.Errorf("uplink every %d min: off home %d%% of the day, want at most 40%%", period, share)
		}
	}
}
