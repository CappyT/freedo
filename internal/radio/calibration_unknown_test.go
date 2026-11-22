package radio

import (
	"strings"
	"testing"
	"time"

	"freedo/cc1101"
)

// A measurement round asks for a correction and the reply of that register write
// is lost. The chip holds the new register, the host the old one. The round
// collects nothing until the restore wrote the correction again, and the result
// names the register that the chip holds.
func TestLostReplyOfACorrectionWriteIsWrittenAgainBeforeTheResult(t *testing.T) {
	const start = cc1101.TuneDefault
	carrier := int(int8(start)) + 15
	br := &lostReplyCarrier{carrierBridge: newCarrierBridge(start, carrier), lose: map[int]bool{1: true}, cached: start}
	clock := newStepClock()
	r := &Live{tuner: br, calResults: make(chan CalibrationResult, 1), clock: clock.read}
	r.fsctrl0.Store(uint32(start))
	r.calMu.Lock()
	r.startMeasureLocked()
	r.calMu.Unlock()
	r.SetPlant(&testPlant{sources: []string{"devic"}})
	collect := func() {
		for i := 0; i < cc1101.TuneWant; i++ {
			r.collectTune(tuneDataFrame("devic", carrier-int(int8(br.chip()))))
			clock.advance(time.Second)
		}
	}
	collect()
	r.tuneStep() // the correction write lands, its reply is lost
	if br.chip() == start || r.FSCTRL0() != start {
		t.Fatalf("chip %d host %d, want the chip on the correction and the host on the old register", int8(br.chip()), int8(r.FSCTRL0()))
	}
	if !r.LongTXDeferred() {
		t.Error("long transmissions allowed while the chip holds a register that the host does not know")
	}
	if !strings.Contains(strings.Join(r.Warnings(), "\n"), "no confirmed reply") {
		t.Errorf("no warning while the register is unknown: %q", r.Warnings())
	}
	collect() // frames on the unknown register
	r.tuneStep()
	select {
	case res := <-r.calResults:
		t.Fatalf("result FSCTRL0=%d while the register is unknown, chip %d", int8(res.FSCTRL0), int8(br.chip()))
	default:
	}
	clock.advance(calibrationRestoreRetry)
	r.tuneStep() // the restore writes the correction again
	if br.chip() != r.FSCTRL0() || r.LongTXDeferred() {
		t.Fatalf("chip %d host %d deferred=%v, want the same register and no deferral", int8(br.chip()), int8(r.FSCTRL0()), r.LongTXDeferred())
	}
	for round := 0; round < 4; round++ {
		collect()
		r.tuneStep()
		select {
		case res := <-r.calResults:
			if res.FSCTRL0 != br.chip() {
				t.Fatalf("result FSCTRL0=%d, the chip holds %d", int8(res.FSCTRL0), int8(br.chip()))
			}
			return
		default:
		}
	}
	t.Fatal("no result")
}

// The reply of the first scan step is lost and every restore write after it
// fails too. The host value equals home, the chip holds the step: health says so.
func TestUnknownRegisterWithAFailingRestoreHasAWarning(t *testing.T) {
	lose := map[int]bool{}
	for i := 1; i <= 50; i++ {
		lose[i] = true
	}
	r, _, clock := newLostReplyPlant(lose)
	r.tuneStep()
	clock.advance(calibrationQuietBefore)
	r.tuneStep()
	for i := 0; i < 6; i++ {
		clock.advance(calibrationRestoreRetry)
		r.tuneStep()
	}
	if !r.LongTXDeferred() {
		t.Fatal("long transmissions allowed for a register that the host does not know")
	}
	if !strings.Contains(strings.Join(r.Warnings(), "\n"), "no confirmed reply") {
		t.Errorf("the long transmissions wait and health has no warning line: %q", r.Warnings())
	}
}

// POST /api/radio/calibrate while a scan step is out. Recalibrate clears the scan
// flags at once, and the loop writes TuneDefault at its next idle step: until
// then the register is the scan step, and the long transmissions wait.
func TestRecalibrateDuringAScanDefersLongTXAtOnce(t *testing.T) {
	const home = 0x34
	br := newCarrierBridge(home, 0)
	clock := newStepClock()
	r := &Live{br: br, tuner: br, calResults: make(chan CalibrationResult, 1), clock: clock.read}
	r.fsctrl0.Store(uint32(home))
	r.cal.state, r.cal.stored, r.cal.hasStored = CalibrationStored, home, true
	r.cal.quietSince = clock.read()
	r.SetPlant(&testPlant{sources: []string{"devic"}})
	r.tuneStep()
	clock.advance(calibrationQuietBefore)
	r.tuneStep()
	if !r.LongTXDeferred() {
		t.Fatal("long transmissions allowed on a scan step")
	}
	r.Recalibrate()
	if r.FSCTRL0() == cc1101.TuneDefault {
		t.Fatal("the reset write ran before the idle step")
	}
	if !r.LongTXDeferred() {
		t.Errorf("a long transmission is allowed on the scan step %d until the reset write", int8(r.FSCTRL0()))
	}
	r.tuneStep()
	if r.FSCTRL0() != cc1101.TuneDefault || r.LongTXDeferred() {
		t.Errorf("FSCTRL0 %d deferred=%v after the reset write, want TuneDefault and no deferral", int8(r.FSCTRL0()), r.LongTXDeferred())
	}
}
