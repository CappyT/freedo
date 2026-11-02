package radio

import (
	"log"
	"math"

	"freedo/cc1101"
)

// tuneKeep bounds the readings a round keeps per source; a source drops its
// oldest beyond it. Twice TuneWant lets a source with half its readings clipped
// still complete a round on its own.
const tuneKeep = 2 * cc1101.TuneWant

// CalibrationState says where the frequency-offset register of a live radio
// comes from.
type CalibrationState string

const (
	// CalibrationStored: the daemon seeded the register from the database.
	CalibrationStored CalibrationState = "stored"
	// CalibrationMeasuring: the radio collects FREQEST readings to set it.
	CalibrationMeasuring CalibrationState = "measuring"
	// CalibrationMeasured: the radio measured it in this session.
	CalibrationMeasured CalibrationState = "measured"
)

// Calibration is the receiver calibration state at one instant. Readings counts
// the usable readings of the round in progress. Session changes at every
// Recalibrate, so a result of an older session is told apart.
type Calibration struct {
	State    CalibrationState
	Readings int
	Want     int
	Session  uint64
}

// CalibrationResult is one finished measurement: the register the radio holds
// now, and the usable frames and the sources of the last round.
type CalibrationResult struct {
	FSCTRL0 byte
	Frames  int
	Sources int
	Session uint64
}

// liveCalibration is the measurement state. calMu guards it: the loop goroutine
// collects and evaluates, the API goroutines read it and restart it. usable and
// total count the readings held in samples.
type liveCalibration struct {
	state   CalibrationState
	samples map[string][]int
	usable  int
	total   int
	round   int
	reset   bool // the loop writes TuneDefault before the measurement samples
	due     bool // the round is complete; the loop evaluates it at its next idle step
	session uint64
}

// Calibration returns the receiver calibration state. A radio that cannot move
// its register reports an empty state.
func (r *Live) Calibration() Calibration {
	r.calMu.Lock()
	defer r.calMu.Unlock()
	return r.calibrationLocked()
}

func (r *Live) calibrationLocked() Calibration {
	c := Calibration{State: r.cal.state, Session: r.cal.session}
	if c.State == CalibrationMeasuring {
		c.Readings, c.Want = r.cal.usable, cc1101.TuneWant
	}
	return c
}

// CalibrationResults delivers each finished measurement. The channel keeps the
// last result only.
func (r *Live) CalibrationResults() <-chan CalibrationResult { return r.calResults }

// Recalibrate drops the readings of the current measurement, if any, and starts
// a new one from cc1101.TuneDefault: the loop writes that register at its next
// idle step, and samples only after the write. A radio that cannot move its
// register stays as it is.
func (r *Live) Recalibrate() Calibration {
	r.calMu.Lock()
	defer r.calMu.Unlock()
	if r.tuner != nil {
		r.startMeasureLocked()
		r.cal.reset = true
	}
	return r.calibrationLocked()
}

func (r *Live) startMeasureLocked() {
	r.cal.session++
	r.cal.state = CalibrationMeasuring
	r.cal.round = 0
	r.cal.reset = false
	r.clearRoundLocked()
}

func (r *Live) clearRoundLocked() {
	r.cal.samples = make(map[string][]int)
	r.cal.usable, r.cal.total = 0, 0
	r.cal.due = false
}

// collectTune adds the FREQEST reading of one received frame to the round in
// progress. Only the loop goroutine calls it, and only for frames that are not
// the answer to its own transmission. Like Bridge.Autotune, a round completes
// on TuneWant usable readings, or on TuneWant readings that clip on one side:
// a register far off pins most readings at the FOC limit, and only the clip
// value can move it.
func (r *Live) collectTune(frame *cc1101.RxFrame) {
	if frame == nil {
		return
	}
	source, ok := cc1101.TuneSample(frame)
	if !ok {
		return
	}
	r.calMu.Lock()
	defer r.calMu.Unlock()
	if r.cal.state != CalibrationMeasuring || r.cal.due || r.cal.reset {
		return
	}
	values := append(r.cal.samples[source], frame.FreqEst)
	r.cal.total++
	if cc1101.TuneUsable(frame.FreqEst) {
		r.cal.usable++
	}
	if len(values) > tuneKeep {
		r.cal.total--
		if cc1101.TuneUsable(values[0]) {
			r.cal.usable--
		}
		values = append(values[:0], values[1:]...)
	}
	r.cal.samples[source] = values
	switch {
	case r.cal.usable >= cc1101.TuneWant:
		r.cal.due = true
	case r.cal.total >= cc1101.TuneWant:
		_, _, clipped := cc1101.TuneCorrection(r.cal.samples)
		r.cal.due = clipped
	}
}

// tuneTarget applies a correction to the register. FSCTRL0 is a signed byte:
// the result saturates at its limits instead of wrapping to the other end.
func tuneTarget(reg byte, correction int) byte {
	v := min(max(int(int8(reg))+correction, math.MinInt8), math.MaxInt8)
	return byte(int8(v))
}

// tuneStep evaluates a full round, the way Bridge.Autotune does, with the live
// band cc1101.TuneAccept: inside it the register is right, else the loop writes
// the correction and measures one more round, at most TuneRounds rounds. The
// loop calls it only when no CSL job is out and no transmission waits, so a
// write never lands inside a transaction.
func (r *Live) tuneStep() {
	r.calMu.Lock()
	if r.cal.reset {
		r.cal.reset = false
		r.calMu.Unlock()
		r.resetRegister()
		return
	}
	if !r.cal.due {
		r.calMu.Unlock()
		return
	}
	session := r.cal.session
	correction, usable, _ := cc1101.TuneCorrection(r.cal.samples)
	sources := tuneSources(r.cal.samples)
	r.cal.round++
	round := r.cal.round
	r.calMu.Unlock()

	reg := r.FSCTRL0()
	next := tuneTarget(reg, correction)
	// A register at its limit cannot move further: it is the result.
	if next != reg && (correction < -cc1101.TuneAccept || correction > cc1101.TuneAccept) {
		if err := r.tuner.SetFSCTRL0(next); err != nil {
			log.Printf("receiver calibration: FSCTRL0=%#02x: %v", next, err)
			r.calMu.Lock()
			if r.cal.session == session {
				r.cal.round--
				r.clearRoundLocked()
			}
			r.calMu.Unlock()
			return
		}
		r.fsctrl0.Store(uint32(next))
		log.Printf("receiver calibration: round %d: correction %+d on %d frames of %d sources, FSCTRL0 %#02x -> %#02x",
			round, correction, usable, sources, reg, next)
		reg = next
		if round < cc1101.TuneRounds {
			r.calMu.Lock()
			if r.cal.session == session {
				r.clearRoundLocked()
			}
			r.calMu.Unlock()
			return
		}
	}

	r.calMu.Lock()
	defer r.calMu.Unlock()
	if r.cal.session != session {
		return
	}
	r.cal.state = CalibrationMeasured
	r.clearRoundLocked()
	result := CalibrationResult{FSCTRL0: reg, Frames: usable, Sources: sources, Session: session}
	select {
	case <-r.calResults:
	default:
	}
	r.calResults <- result
}

// resetRegister writes cc1101.TuneDefault for a new measurement. A write the
// radio refuses is a log line: the measurement then starts from the register
// as it stands.
func (r *Live) resetRegister() {
	if err := r.tuner.SetFSCTRL0(cc1101.TuneDefault); err != nil {
		log.Printf("receiver calibration: FSCTRL0=%#02x: %v", cc1101.TuneDefault, err)
		return
	}
	r.fsctrl0.Store(uint32(cc1101.TuneDefault))
}

// tuneSources counts the sources with at least one usable reading.
func tuneSources(samples map[string][]int) int {
	n := 0
	for _, values := range samples {
		for _, v := range values {
			if cc1101.TuneUsable(v) {
				n++
				break
			}
		}
	}
	return n
}
