package radio

import (
	"errors"
	"fmt"
	"log"
	"time"

	"freedo/cc1101"
)

// Airtime is the time on air of the last hour, as the bridge firmware counts it.
// Limit says that the airtime limit is on. Without it the count informs and
// refuses nothing, and BudgetMS is 0. With it BudgetMS is the budget of the
// firmware, Deferring says that the radio holds back Router Advertisement bursts
// and CSL bootstrap trains, and OverBudget that the hour is above the budget.
// Buckets is the per-minute count of the firmware, oldest first, and At the
// instant of the reading: the daemon stores both, so that a restart of the bridge
// keeps the count.
type Airtime struct {
	UsedMS     uint32
	BudgetMS   uint32
	Share      float64
	Limit      bool
	Deferring  bool
	OverBudget bool
	Buckets    [cc1101.AirtimeBuckets]uint16
	At         time.Time
}

// Airtime returns the last reading, and whether the bridge reports one. A bridge
// without the command, and a radio that has not polled yet, report none.
func (r *Live) Airtime() (Airtime, bool) {
	r.airtimeMu.Lock()
	defer r.airtimeMu.Unlock()
	return r.airtime, !r.airtime.At.IsZero()
}

func (r *Live) airtimeDeferring() bool {
	r.airtimeMu.Lock()
	defer r.airtimeMu.Unlock()
	return r.airtime.Deferring
}

// longTXHold says why a Router Advertisement burst or a CSL bootstrap train must
// wait, or nil. The synthesizer is not locked, the airtime limit is on and the
// last hour is near the budget, or a scan step holds a register that moves the
// transmit carrier off the devices.
func (r *Live) longTXHold() error {
	switch {
	case r.synthFailed.Load():
		return errSynthHeld
	case r.airtimeLimit && r.airtimeDeferring():
		return errAirtimeDeferred
	case r.scanOffHome():
		return errScanDeferred
	}
	return nil
}

// LongTXDeferred reports that the radio refuses Router Advertisement bursts and
// CSL bootstrap trains for now. The caller asks before it builds a burst: a
// refused burst has used a frame counter.
func (r *Live) LongTXDeferred() bool { return r.longTXHold() != nil }

// idleStep runs the work that needs no transaction in flight: the receiver
// calibration, the airtime reading and the synthesizer calibration. The loop
// calls it only when no CSL job is out. It returns the error that ends the loop.
func (r *Live) idleStep() error {
	if r.tuner != nil {
		r.tuneStep()
	}
	r.pollAirtime()
	return r.calibrateSynth()
}

// pollAirtime reads the count about every liveAirtimePoll. With the airtime limit
// on, it sets the deferral with a hysteresis (on from airtimeHold of the budget,
// off under airtimeRelease) and logs a line at each change of the deferral and of
// the over-budget state. Without the limit it only keeps the count.
func (r *Live) pollAirtime() {
	br, ok := r.br.(airtimeBridge)
	if !ok {
		return
	}
	now := r.now()
	if !r.airtimeAt.IsZero() && now.Sub(r.airtimeAt) < liveAirtimePoll {
		return
	}
	r.airtimeAt = now
	reading, err := br.Airtime()
	used, budget := reading.UsedMS, reading.BudgetMS
	if err != nil {
		r.noteBridgeErr(err)
		log.Printf("airtime: %v", err)
		return
	}
	share := 0.0
	if budget > 0 {
		share = float64(used) / float64(budget)
	}
	limit := r.airtimeLimit
	if !limit && budget > 0 && !r.budgetWarned {
		// The firmware of this repository has no budget until the host sets one. A
		// bridge that reports one with the limit off runs an older v2.11 build, with a
		// fixed cap that refuses above it.
		r.budgetWarned = true
		log.Printf("airtime: the bridge reports a budget of %d ms with the airtime limit off: it runs an older v2.11 firmware with a fixed cap, flash the firmware of this release", budget)
	}
	over := limit && budget > 0 && used > budget
	r.airtimeMu.Lock()
	was, wasOver := r.airtime.Deferring, r.airtime.OverBudget
	deferring := limit && was
	if limit {
		switch {
		case share >= airtimeHold:
			deferring = true
		case share < airtimeRelease:
			deferring = false
		}
	}
	r.airtime = Airtime{UsedMS: used, BudgetMS: budget, Share: share, Limit: limit, Deferring: deferring, OverBudget: over, Buckets: reading.Buckets, At: now}
	r.airtimeMu.Unlock()
	switch {
	case deferring && !was:
		log.Printf("airtime: warning: %d of %d ms used in the last hour (%.0f%%): deferring router advertisements and CSL bootstrap trains", used, budget, 100*share)
	case !deferring && was:
		log.Printf("airtime: %d of %d ms used in the last hour (%.0f%%): no longer deferring", used, budget, 100*share)
	}
	switch {
	case over && !wasOver:
		log.Printf("airtime: warning: %d ms on air in the last hour, above the budget of %d ms (%.0f%%): the bridge refuses transmissions", used, budget, 100*share)
	case !over && wasOver:
		log.Printf("airtime: %d ms on air in the last hour, back under the budget of %d ms (%.0f%%)", used, budget, 100*share)
	}
}

// pollAirtimeFinal reads the firmware once more after the loop stopped, so that
// the daemon stores the time on air of the whole run. A loop that ended on an
// error leaves no link to trust, and gets no read. Only Close calls it, after
// the loop goroutine ended.
func (r *Live) pollAirtimeFinal() {
	if r.RadioError() != nil {
		return
	}
	r.airtimeAt = time.Time{}
	r.pollAirtime()
}

// noteAirtimeRefusal handles a refusal of the firmware: the budget is used up,
// so the deferral starts before the next reading. The firmware refuses only with
// a budget, which the host sets with the airtime limit: without it the answer is
// not expected and only gets a log line.
func (r *Live) noteAirtimeRefusal(what string) {
	if !r.airtimeLimit {
		log.Printf("airtime: the bridge refused a %s for the hourly budget, but the airtime limit is off", what)
		return
	}
	r.airtimeMu.Lock()
	r.airtime.Deferring = true
	r.airtimeMu.Unlock()
	log.Printf("airtime: the bridge refused a %s, the hourly budget is used up: deferred", what)
}

// logDeferral records a transmission the loop holds back. One line per kind and
// minute is enough: the engine asks again.
func (r *Live) logDeferral(what string, why error) {
	now := r.now()
	if r.deferLogAt == nil {
		r.deferLogAt = make(map[string]time.Time)
	}
	if last := r.deferLogAt[what]; !last.IsZero() && now.Sub(last) < time.Minute {
		return
	}
	r.deferLogAt[what] = now
	var paced *BootstrapPaced
	if errors.As(why, &paced) {
		log.Printf("%s deferred: %v", what, paced)
		return
	}
	if errors.Is(why, errSynthHeld) {
		log.Printf("synthesizer: %s held while the synthesizer is not locked", what)
		return
	}
	if errors.Is(why, errScanDeferred) {
		log.Printf("receiver calibration: %s deferred while the FSCTRL0 scan holds another register", what)
		return
	}
	log.Printf("airtime: %s deferred, the last hour is above %.0f%% of the budget", what, 100*airtimeHold)
}

// noteBridgeErr turns an ST_UNCONFIGURED answer of any request into the end of
// the loop, so the daemon restarts and configures the chip at once. It also
// logs a refusal for the airtime budget. Other errors are the caller's.
func (r *Live) noteBridgeErr(err error) {
	switch {
	case errors.Is(err, cc1101.ErrUnconfigured):
		r.unconfigured.Store(true)
	case errors.Is(err, cc1101.ErrAirtimeBudget) && !errors.Is(err, errAirtimeDeferred) && !errors.Is(err, errScanDeferred) && !errors.Is(err, errSynthHeld):
		r.noteAirtimeRefusal("request")
	}
}

// calibrateSynth runs SCAL once per hour. MCSM0 turns the automatic calibration
// off, so a chip that drifts in temperature loses its lock and goes quiet
// without a sign. A calibration that does not lock holds every transmission and
// runs again after a minute. The third failed retry in a row returns errSynthLost,
// which ends the loop.
func (r *Live) calibrateSynth() error {
	br, ok := r.br.(synthBridge)
	if !ok {
		return nil
	}
	every := liveSynthEvery
	if r.synthFailed.Load() {
		every = liveSynthRetry
	}
	now := r.now()
	if now.Sub(r.synthAt) < every {
		return nil
	}
	r.synthAt = now
	switch err := br.CalibrateSynth(); {
	case err == nil:
		r.synthTries = 0
		if r.synthFailed.Swap(false) {
			log.Printf("synthesizer calibration: locked again")
		}
	case errors.Is(err, cc1101.ErrNoSynthLock):
		if !r.synthLost() {
			r.synthTries++
		}
		if r.synthTries >= liveSynthRetries {
			log.Printf("synthesizer calibration: %v: the bridge restarts", err)
			return errSynthLost
		}
		log.Printf("WARNING: synthesizer calibration: %v: no transmission until it locks, trying again in %s", err, liveSynthRetry)
	case isLinkFault(err):
		log.Printf("synthesizer calibration: %v", err)
	default:
		r.noteBridgeErr(err)
		log.Printf("synthesizer calibration: %v", err)
	}
	return nil
}

// synthLost records that the synthesizer is not locked, and reports whether it
// was locked before. The bridge answers Enhanced Beacon Requests on its own
// while a pair window is open, so the window closes at once.
func (r *Live) synthLost() bool {
	if r.synthFailed.Swap(true) {
		return false
	}
	r.synthTries = 0
	r.calMu.Lock()
	open := !r.cal.pairUntil.IsZero()
	r.calMu.Unlock()
	if !open {
		return true
	}
	r.pairWindowChanged(0)
	if br, ok := r.br.(pairWindowBridge); ok {
		if _, err := br.SetPairWindow(cc1101.PairWindow{}); err != nil {
			r.noteBridgeErr(err)
			log.Printf("synthesizer: close the pair window: %v", err)
		}
	}
	return true
}

// synthHold refuses a transmission while the synthesizer is not locked.
func (r *Live) synthHold(what string) error {
	if !r.synthFailed.Load() {
		return nil
	}
	r.logDeferral(what, errSynthHeld)
	return errSynthHeld
}

// Warnings lists the conditions of the radio that need a person: a calibration
// that found no frame or no stable register, a register that drifted, a
// synthesizer that does not lock, and an airtime budget that is nearly spent.
func (r *Live) Warnings() []string {
	var out []string
	r.calMu.Lock()
	out = append(out, r.calibrationWarningsLocked()...)
	r.calMu.Unlock()
	if r.synthFailed.Load() {
		out = append(out, fmt.Sprintf("the synthesizer is not locked: the radio transmits nothing and may hear nothing; it calibrates again every %s and restarts the bridge after %d failed retries", liveSynthRetry, liveSynthRetries))
	}
	if a, ok := r.Airtime(); ok && a.OverBudget {
		out = append(out, fmt.Sprintf("airtime: %d ms on air in the last hour, above the budget of %d ms (%.0f%%): the bridge refuses transmissions and the radio defers router advertisements and CSL bootstrap trains", a.UsedMS, a.BudgetMS, 100*a.Share))
	} else if ok && a.Deferring {
		out = append(out, fmt.Sprintf("airtime: %.0f%% of the hourly budget used: router advertisements and CSL bootstrap trains are deferred", 100*a.Share))
	}
	return out
}
