package radio

import (
	"errors"
	"fmt"
	"log"
	"time"

	"freedo/cc1101"
)

// A module that is far off the carrier decodes nothing, and then no reading can
// ever move its register. The radio then scans: it steps the register around its
// home value, and the first step that decodes a frame of a device becomes the
// candidate: the normal measurement runs on it for at most
// calibrationCandidateMax. The scan moves the transmit carrier with the receive
// one, so it runs only on a plant that has a device, and only after the devices
// were silent for calibrationQuietBefore. Each step dwells for
// calibrationScanDwell. A pass that decodes nothing goes back to the home
// register, and the next pass waits for calibrationScanHold. A candidate that
// fails goes home and waits the same.
const (
	calibrationQuietBefore = 10 * time.Minute
	calibrationScanDwell   = 3 * time.Minute
	calibrationScanHold    = time.Hour
	// calibrationCandidateMax bounds the measurement on a candidate register. A
	// register that no round settled by then goes back home.
	calibrationCandidateMax = 30 * time.Minute
	// calibrationRestoreRetry spaces the tries of a restore write that failed.
	calibrationRestoreRetry = 5 * time.Second
)

// calibrationScanStep is the distance between two registers of the scan: about
// 48 kHz, inside the receive filter that is 135 kHz wide.
const calibrationScanStep = 30

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
	// CalibrationUnsettled: the rounds ended with a register that no round
	// confirmed. The radio keeps it, and nothing is stored.
	CalibrationUnsettled CalibrationState = "unsettled"
)

// Calibration is the receiver calibration state at one instant. Readings counts
// the usable readings of the round in progress. Session changes at every
// Recalibrate, so a result of an older session is told apart. Residual is the
// offset the frames still read after the calibration, in register units, when
// enough of them arrived.
type Calibration struct {
	// Candidate: a scan step decoded a frame of a device and the measurement
	// runs on that register. The register is stored only if the measurement
	// settles inside the band.
	Candidate   bool
	State       CalibrationState
	Readings    int
	Want        int
	Session     uint64
	Residual    int
	HasResidual bool
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
	// plant tells the devices of the installation. Nil: no device, no scan.
	plant Plant
	// stored is the register the database holds, when hasStored. The scan and a
	// pair window go back to it, or to cc1101.TuneDefault.
	stored    byte
	hasStored bool
	// quietSince starts the silence of the devices: the start, the last frame
	// of a device, the first device of the plant, the end of a pair window.
	quietSince time.Time
	hadDevices bool
	// scanning: the register steps around the home value, and since is when the
	// scan last wrote it. scanFailed: the last pass decoded nothing. scanHold: no
	// pass starts before it. restore: the loop writes the home register at its
	// next idle step, after a pair window stopped the scan, and again every
	// calibrationRestoreRetry until the write works. pairUntil: no scan starts
	// before it.
	scanning   bool
	scanFailed bool
	scanSteps  int
	scanHold   time.Time
	restore    bool
	restoreAt  time.Time
	// regUnknown: the last register write failed, and the chip can hold the new
	// value although the host does not know it. The next write is not skipped
	// for an equal host value, and the radio counts as off home until it works.
	regUnknown bool
	// retryReg: a correction write of the measurement got no reply, so the chip
	// can hold it. The restore writes it again instead of the home register,
	// and the round collects nothing until the write works.
	retryReg    byte
	retryRegSet bool
	pairUntil   time.Time
	since       time.Time
	// candidate: a frame of a device decoded on a scan step and the measurement
	// runs on that register, which is not the home one. candidateAt starts it,
	// and candidateFrom is the state to go back to when it fails.
	candidate     bool
	candidateAt   time.Time
	candidateFrom CalibrationState
	// resid holds the readings of the frames heard after the measurement, per
	// source, for the residual of the register. The register write clears it.
	resid map[string][]int
}

// Calibration returns the receiver calibration state. A radio that cannot move
// its register reports an empty state.
func (r *Live) Calibration() Calibration {
	r.calMu.Lock()
	defer r.calMu.Unlock()
	return r.calibrationLocked()
}

func (r *Live) calibrationLocked() Calibration {
	c := Calibration{State: r.cal.state, Session: r.cal.session, Candidate: r.cal.candidate}
	if c.State == CalibrationMeasuring {
		c.Readings, c.Want = r.cal.usable, cc1101.TuneWant
	} else {
		c.Residual, c.HasResidual = r.residualLocked()
	}
	return c
}

// residualLocked is the correction the frames heard since the last register
// write still ask for. It needs TuneMin usable readings, or readings that clip.
func (r *Live) residualLocked() (int, bool) {
	correction, usable, clipped := cc1101.TuneCorrection(r.cal.resid)
	if !clipped && usable < cc1101.TuneMin {
		return 0, false
	}
	return correction, true
}

// calibrationWarningsLocked lists what a person must look at.
func (r *Live) calibrationWarningsLocked() []string {
	var out []string
	switch {
	case r.cal.candidate:
		out = append(out, fmt.Sprintf("receiver calibration: a frame of a device decoded on FSCTRL0 %#02x, home is %#02x: measuring on this register, it is stored only if the measurement settles inside +-%d units, else FSCTRL0 goes back to %#02x by %s",
			r.FSCTRL0(), r.homeLocked(), cc1101.TuneAccept, r.homeLocked(), r.cal.candidateAt.Add(calibrationCandidateMax).Format("15:04:05")))
	case r.cal.scanning:
		out = append(out, fmt.Sprintf("receiver calibration: no frame from a device for %s, scanning FSCTRL0 around %#02x, now %#02x, step %d of %d: check that devices transmit, the antenna and the module",
			calibrationQuietBefore, r.homeLocked(), r.FSCTRL0(), r.cal.scanSteps, len(scanRegisters(r.homeLocked()))))
	case r.cal.scanFailed:
		out = append(out, fmt.Sprintf("receiver calibration: the scan decoded no frame from a device on any register, FSCTRL0 is %#02x (home %#02x) and the next scan starts at %s: check that devices transmit, the antenna and the module",
			r.FSCTRL0(), r.homeLocked(), r.cal.scanHold.Format("15:04:05")))
	}
	if r.cal.restore && !r.cal.retryRegSet && r.FSCTRL0() != r.homeLocked() {
		out = append(out, fmt.Sprintf("receiver calibration: FSCTRL0 is %#02x, not the home value %#02x: the write that restores it did not work, the radio tries again every %s",
			r.FSCTRL0(), r.homeLocked(), calibrationRestoreRetry))
	}
	if r.cal.regUnknown {
		out = append(out, fmt.Sprintf("receiver calibration: the last FSCTRL0 write got no confirmed reply, so the chip can hold another register than %#02x: the long transmissions wait and the radio writes it again every %s",
			r.FSCTRL0(), calibrationRestoreRetry))
	}
	if r.cal.state == CalibrationUnsettled {
		out = append(out, fmt.Sprintf("receiver calibration: no register was confirmed in %d rounds, nothing is stored: POST /api/radio/calibrate measures again", cc1101.TuneRounds+1))
	}
	if r.cal.state != CalibrationMeasuring {
		if residual, ok := r.residualLocked(); ok && (residual < -cc1101.TuneAccept || residual > cc1101.TuneAccept) {
			out = append(out, fmt.Sprintf("receiver calibration: the frames read %+d units off the register, over the band of %d: the module drifted, POST /api/radio/calibrate measures again", residual, cc1101.TuneAccept))
		}
	}
	return out
}

// Plant tells the live radio which frames come from the devices of the
// installation.
type Plant interface {
	// Devices is the number of devices of the installation.
	Devices() int
	// HasSource reports whether a frame source, as cc1101.TuneSample labels it,
	// is a device of the installation.
	HasSource(source string) bool
}

// SetPlant gives the radio the installation. Until it is set the plant counts
// as empty, and the register never scans.
func (r *Live) SetPlant(p Plant) {
	r.calMu.Lock()
	r.cal.plant = p
	r.calMu.Unlock()
}

// homeLocked is the register the scan and a pair window go back to: the stored
// calibration, or cc1101.TuneDefault.
func (r *Live) homeLocked() byte {
	if r.cal.hasStored {
		return r.cal.stored
	}
	return cc1101.TuneDefault
}

// scanOffHome reports that a scan step, a candidate register, a restore that
// did not work yet or a reset write that waits holds a register other than the
// home one. The transmit carrier follows the register, so the long
// transmissions wait.
func (r *Live) scanOffHome() bool {
	r.calMu.Lock()
	defer r.calMu.Unlock()
	return (r.cal.scanning || r.cal.candidate || r.cal.restore || r.cal.reset) && (r.cal.regUnknown || r.FSCTRL0() != r.homeLocked())
}

// pairWindowChanged tells the radio that a pair window opened for d, or closed
// when d is zero. An open window stops the scan and a candidate measurement,
// sends the register back to its home value and holds the scan back until the
// window ends.
func (r *Live) pairWindowChanged(d time.Duration) {
	r.calMu.Lock()
	defer r.calMu.Unlock()
	now := r.now()
	if d <= 0 {
		r.cal.pairUntil = time.Time{}
		r.cal.quietSince = now
		return
	}
	r.cal.pairUntil = now.Add(d)
	r.cal.quietSince = r.cal.pairUntil
	if r.cal.scanning || r.cal.candidate {
		r.cal.scanning = false
		r.endCandidateLocked()
		r.cal.restore = true
		log.Printf("receiver calibration: pair window open, the FSCTRL0 scan stops and the register goes back to %#02x", r.homeLocked())
	}
}

// endCandidateLocked drops a candidate that did not settle, and goes back to the
// state before it with an empty round. The caller sends the register home.
func (r *Live) endCandidateLocked() {
	r.cal.retryRegSet = false
	if !r.cal.candidate {
		return
	}
	r.cal.candidate = false
	r.cal.state = r.cal.candidateFrom
	r.cal.round = 0
	r.cal.resid = nil
	r.clearRoundLocked()
}

// heardLocked notes a frame that the receiver decoded. A frame of a device ends
// the silence, and the scan step that decoded one becomes the candidate: the
// measurement runs on that register.
func (r *Live) heardLocked(source string) {
	if r.cal.plant == nil || !r.cal.plant.HasSource(source) {
		return
	}
	r.cal.quietSince = r.now()
	r.cal.scanFailed = false
	if r.cal.scanning {
		log.Printf("receiver calibration: the scan decoded a frame of a device on FSCTRL0 %#02x: measuring from there, home is %#02x", r.FSCTRL0(), r.homeLocked())
		from := r.cal.state
		r.startMeasureLocked()
		r.cal.candidate, r.cal.candidateAt, r.cal.candidateFrom = true, r.now(), from
	}
}

// noteHeard notes a frame that carries no reading, such as a CSL inbound frame.
func (r *Live) noteHeard(frame *cc1101.RxFrame) {
	if frame == nil {
		return
	}
	source, ok := cc1101.TuneSample(frame)
	if !ok {
		return
	}
	r.calMu.Lock()
	r.heardLocked(source)
	r.calMu.Unlock()
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
		// The daemon forgets the stored value with this call.
		r.cal.hasStored = false
		r.cal.reset = true
	}
	return r.calibrationLocked()
}

func (r *Live) startMeasureLocked() {
	r.cal.session++
	r.cal.state = CalibrationMeasuring
	r.cal.round = 0
	r.cal.reset = false
	r.cal.quietSince = r.now()
	r.cal.scanning, r.cal.scanFailed, r.cal.restore, r.cal.candidate = false, false, false, false
	r.cal.scanSteps = 0
	r.cal.resid = nil
	r.cal.retryRegSet = false
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
	r.heardLocked(source)
	if r.cal.state != CalibrationMeasuring {
		r.noteResidualLocked(source, frame.FreqEst)
		return
	}
	if r.cal.due || r.cal.reset || r.cal.restore {
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

// noteResidualLocked keeps the reading of a frame heard after the measurement.
func (r *Live) noteResidualLocked(source string, freqEst int) {
	if r.cal.resid == nil {
		r.cal.resid = make(map[string][]int)
	}
	values := append(r.cal.resid[source], freqEst)
	if len(values) > tuneKeep {
		values = append(values[:0], values[1:]...)
	}
	r.cal.resid[source] = values
}

// scanRegisters lists the registers a silent measurement tries, from base:
// base+30, base-30, base+60, base-60, and on, as far as the int8 range reaches.
func scanRegisters(base byte) []byte {
	var out []byte
	for k := 1; ; k++ {
		up, down := int(int8(base))+k*calibrationScanStep, int(int8(base))-k*calibrationScanStep
		if up > 127 && down < -128 {
			return out
		}
		if up <= 127 {
			out = append(out, byte(int8(up)))
		}
		if down >= -128 {
			out = append(out, byte(int8(down)))
		}
	}
}

// failCandidateLocked ends a candidate that did not settle: the register goes
// home and the scan waits for calibrationScanHold, as after a pass that decoded
// nothing. Without the hold, a plant whose devices send every 10 to 15 minutes
// would spend most of its time one step off home. The caller sends the register
// home.
func (r *Live) failCandidateLocked(now time.Time) {
	r.endCandidateLocked()
	r.cal.quietSince, r.cal.restore = now, true
	r.cal.scanHold = now.Add(calibrationScanHold)
}

// scanTickLocked runs the scan of the register. It returns true when the step is
// over: it wrote the register, or it ended a pass, and the caller gets calMu back
// unlocked. On false the caller still holds calMu.
func (r *Live) scanTickLocked() bool {
	c := &r.cal
	now := r.now()
	if c.candidate && now.Sub(c.candidateAt) >= calibrationCandidateMax {
		log.Printf("receiver calibration: no round settled on FSCTRL0 %#02x in %s: back to %#02x, next scan after %s", r.FSCTRL0(), calibrationCandidateMax, r.homeLocked(), calibrationScanHold)
		r.failCandidateLocked(now)
	}
	if c.plant == nil || c.plant.Devices() == 0 {
		// An empty plant has nothing to hear: it stays on its home register.
		c.hadDevices = false
		if c.scanning || c.candidate {
			c.scanning = false
			r.endCandidateLocked()
			c.restore = true
		}
	}
	if c.restore {
		if now.Before(c.restoreAt) {
			return false
		}
		r.calMu.Unlock()
		r.restoreHome()
		return true
	}
	if c.plant == nil || c.plant.Devices() == 0 {
		return false
	}
	if !c.hadDevices || c.quietSince.IsZero() {
		c.hadDevices, c.quietSince = true, now
	}
	if c.scanning {
		if now.Sub(c.since) < calibrationScanDwell {
			return false
		}
		return r.scanAdvanceLocked(now)
	}
	if c.candidate || now.Sub(c.quietSince) < calibrationQuietBefore || now.Before(c.scanHold) || now.Before(c.pairUntil) {
		return false
	}
	c.scanning, c.scanSteps = true, 0
	log.Printf("receiver calibration: no frame from a device for %s: FSCTRL0 scan around %#02x", calibrationQuietBefore, r.homeLocked())
	return r.scanAdvanceLocked(now)
}

// scanAdvanceLocked writes the next register of the scan. A pass that decoded
// nothing ends instead: the register goes back home and the next pass waits for
// calibrationScanHold. The caller holds calMu and gets it back unlocked.
func (r *Live) scanAdvanceLocked(now time.Time) bool {
	c := &r.cal
	list := scanRegisters(r.homeLocked())
	if c.scanSteps >= len(list) {
		c.scanning, c.scanFailed, c.restore = false, true, true
		c.scanHold = now.Add(calibrationScanHold)
		log.Printf("receiver calibration: the scan decoded no frame from a device on any of %d registers: back to FSCTRL0 %#02x, next scan after %s", len(list), r.homeLocked(), calibrationScanHold)
		r.calMu.Unlock()
		r.restoreHome()
		return true
	}
	next := list[c.scanSteps]
	c.scanSteps++
	steps := c.scanSteps
	c.since = now
	r.clearRoundLocked()
	r.calMu.Unlock()
	log.Printf("receiver calibration: scan step %d of %d, FSCTRL0 %#02x -> %#02x", steps, len(list), r.FSCTRL0(), next)
	if err := r.setRegister(next); err != nil {
		log.Printf("receiver calibration: FSCTRL0=%#02x: %v: the scan ends, the register goes back to %#02x, next scan after %s", next, err, r.homeLocked(), calibrationScanHold)
		r.calMu.Lock()
		// The chip can hold the step although the host does not know it, and a
		// frame decoded on it must not start a candidate on the wrong register.
		c.scanning, c.restore = false, true
		c.scanHold = now.Add(calibrationScanHold)
		r.calMu.Unlock()
	}
	return true
}

// restoreIfDue runs the restore that a pair window or a failed write asked for,
// when its time has come. Only the loop goroutine calls it, with no job out.
func (r *Live) restoreIfDue() {
	r.calMu.Lock()
	due := r.cal.restore && !r.now().Before(r.cal.restoreAt)
	r.calMu.Unlock()
	if due {
		r.restoreHome()
	}
}

// restoreHome writes the home register when the radio holds another one, or the
// correction whose write got no reply. The
// restore flag stays set until the write works, so the loop tries again, and the
// radio counts as off home until then. The caller sets the flag.
func (r *Live) restoreHome() {
	r.calMu.Lock()
	target, session, unknown := r.homeLocked(), r.cal.session, r.cal.regUnknown
	if r.cal.retryRegSet {
		target = r.cal.retryReg
	}
	r.clearRoundLocked()
	r.calMu.Unlock()
	var err error
	if unknown || r.FSCTRL0() != target {
		if err = r.setRegister(target); err != nil {
			log.Printf("receiver calibration: FSCTRL0=%#02x: %v", target, err)
		}
	}
	r.calMu.Lock()
	defer r.calMu.Unlock()
	// A new measurement started meanwhile and owns the flag.
	if r.cal.session != session {
		return
	}
	r.cal.restore = err != nil
	if err == nil {
		r.cal.retryRegSet = false
	} else {
		r.cal.restoreAt = r.now().Add(calibrationRestoreRetry)
	}
}

// setRegister writes FSCTRL0 and clears the readings that belong to the old
// register. A synthesizer that does not lock keeps the register written, and the
// hourly check runs again after a minute.
func (r *Live) setRegister(v byte) error {
	err := r.tuner.SetFSCTRL0(v)
	if errors.Is(err, cc1101.ErrNoSynthLock) {
		log.Printf("WARNING: receiver calibration: FSCTRL0=%#02x written: %v", v, err)
		r.synthLost()
		r.synthAt = r.now()
		err = nil
	}
	r.noteBridgeErr(err)
	if err != nil {
		// The chip can hold the new value although the SCAL after it failed, or
		// although the reply of the write was lost.
		r.calMu.Lock()
		r.cal.regUnknown = true
		if held := r.tuner.FSCTRL0(); held != r.FSCTRL0() {
			r.fsctrl0.Store(uint32(held))
			r.cal.resid = nil
		}
		r.calMu.Unlock()
		return err
	}
	r.fsctrl0.Store(uint32(v))
	r.calMu.Lock()
	r.cal.resid = nil
	r.cal.regUnknown = false
	r.calMu.Unlock()
	return nil
}

// tuneStep evaluates a full round, the way Bridge.Autotune does, with the live
// band cc1101.TuneAccept: inside it the register is right, else the loop writes
// the correction and measures one more round, at most TuneRounds writes. The
// round after the last write only confirms: a register that no round found
// inside the band is never a result, so the daemon stores nothing for it. A
// plant whose devices are silent steps the register instead, see
// calibrationQuietBefore. The loop
// calls it only when no CSL job is out and no transmission waits, so a write
// never lands inside a transaction.
func (r *Live) tuneStep() {
	r.calMu.Lock()
	if r.cal.reset {
		r.cal.reset = false
		r.calMu.Unlock()
		r.resetRegister()
		return
	}
	if r.scanTickLocked() {
		return
	}
	if !r.cal.due || r.cal.restore || r.cal.regUnknown {
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
	next := cc1101.TuneTarget(reg, correction)
	inside := correction >= -cc1101.TuneAccept && correction <= cc1101.TuneAccept
	// A register at its limit cannot move further.
	if !inside && next != reg && round <= cc1101.TuneRounds {
		if err := r.setRegister(next); err != nil {
			log.Printf("receiver calibration: FSCTRL0=%#02x: %v", next, err)
			r.calMu.Lock()
			if r.cal.session == session {
				// The chip can hold the correction although the reply was lost.
				// The restore writes it again before the next round collects:
				// the result must never name a register that the chip does not hold.
				r.cal.retryReg, r.cal.retryRegSet = next, true
				r.cal.restore, r.cal.restoreAt = true, r.now().Add(calibrationRestoreRetry)
				r.clearRoundLocked()
			}
			r.calMu.Unlock()
			return
		}
		log.Printf("receiver calibration: round %d: correction %+d on %d frames of %d sources, FSCTRL0 %#02x -> %#02x",
			round, correction, usable, sources, reg, next)
		r.calMu.Lock()
		if r.cal.session == session {
			r.clearRoundLocked()
		}
		r.calMu.Unlock()
		return
	}

	r.calMu.Lock()
	defer r.calMu.Unlock()
	if r.cal.session != session {
		return
	}
	r.clearRoundLocked()
	if !inside && r.cal.candidate {
		// The candidate register is a guess: it is neither kept nor stored.
		log.Printf("receiver calibration: round %d: correction %+d on %d frames of %d sources at the candidate FSCTRL0 %#02x: not settled, back to %#02x, next scan after %s",
			round, correction, usable, sources, reg, r.homeLocked(), calibrationScanHold)
		r.failCandidateLocked(r.now())
		return
	}
	if !inside {
		r.cal.state = CalibrationUnsettled
		log.Printf("receiver calibration: round %d: correction %+d on %d frames of %d sources at FSCTRL0 %#02x: no register confirmed, nothing stored",
			round, correction, usable, sources, reg)
		return
	}
	r.cal.state = CalibrationMeasured
	r.cal.candidate = false
	// The daemon stores this register, and a later scan goes back to it.
	r.cal.stored, r.cal.hasStored = reg, true
	result := CalibrationResult{FSCTRL0: reg, Frames: usable, Sources: sources, Session: session}
	select {
	case <-r.calResults:
	default:
	}
	r.calResults <- result
}

// resetRegister writes cc1101.TuneDefault for a new measurement. A write that
// fails is tried again every calibrationRestoreRetry, and the measurement waits
// for it: it must not run on a scan step.
func (r *Live) resetRegister() {
	err := r.setRegister(cc1101.TuneDefault)
	if err == nil {
		return
	}
	log.Printf("receiver calibration: FSCTRL0=%#02x: %v", cc1101.TuneDefault, err)
	r.calMu.Lock()
	r.cal.restore, r.cal.restoreAt = true, r.now().Add(calibrationRestoreRetry)
	r.calMu.Unlock()
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
