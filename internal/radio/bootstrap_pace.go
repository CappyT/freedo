package radio

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"freedo/cc1101"
)

// A CSL bootstrap train is about one second on air. After a restart of the
// daemon every device lacks a sample anchor, so every job asks for one, and the
// trains would fill the air for minutes. With the airtime limit on, the radio
// starts at most liveBootstrapPaceMax trains in any liveBootstrapPaceWindow. A
// job over the limit waits in its queue: nothing is dropped. With the limit off
// there is no count and no limit. In both cases the radio serves a job that
// carries a user command before a refresh.
//
// A job that asks for a train books a slot when it starts. The firmware sends a
// train of about 1 s only when the device has no anchor. A job that ran on an
// anchor sent a few wake frames, and its slot returns when it completes.
const (
	liveBootstrapPaceMax    = 4
	liveBootstrapPaceWindow = time.Minute
	// liveBootstrapTrainFrames is the wake frame count from which a job counts as a
	// train: half of the 253 frames of a train of one period. A job on an anchor
	// sends about 22.
	liveBootstrapTrainFrames = 127
	// liveBootstrapWaitGrace is how long a waiting job may be late for its retry.
	// A job that does not ask again by then left its queue, and no longer holds
	// the refreshes back.
	liveBootstrapWaitGrace = 5 * time.Second
	// liveBootstrapYield is the retry of a refresh that leaves a free slot to a
	// user command that waits.
	liveBootstrapYield = 2 * time.Second
	// liveBootstrapPaceSlack keeps a retry timer from firing just before its slot.
	liveBootstrapPaceSlack = 250 * time.Millisecond
)

// BootstrapLimit is the number of trains that start in a minute at most, for
// health. It is 0 when the airtime limit is off: the pace then has no limit.
func (r *Live) BootstrapLimit() int {
	if r.airtimeLimit {
		return liveBootstrapPaceMax
	}
	return 0
}

// BootstrapPaced says that the job has no slot for its bootstrap train now. The
// job stays queued and asks again after RetryAfter. It is not an airtime
// refusal: the firmware has taken nothing.
type BootstrapPaced struct {
	RetryAfter time.Duration
	// Waiting is the number of jobs that wait for a slot, this one included.
	Waiting int
	// Cause says why the job waits. The zero value gives a text with no cause.
	Cause PaceCause
}

// PaceCause is the reason that the pace holds a job.
type PaceCause uint8

const (
	// PaceLimit: the slots of the minute are taken (the airtime limit is on).
	PaceLimit PaceCause = iota + 1
	// PaceCooldown: the last train started less than the global cooldown ago.
	PaceCooldown
	// PaceYield: a user command waits for a slot, and a refresh gives way.
	PaceYield
)

func (e *BootstrapPaced) Error() string {
	wait := fmt.Sprintf("%d waiting, retry in %s", e.Waiting, e.RetryAfter.Round(time.Second))
	switch e.Cause {
	case PaceLimit:
		return fmt.Sprintf("CSL bootstrap train paced: at most %d a minute, %s", liveBootstrapPaceMax, wait)
	case PaceCooldown:
		return fmt.Sprintf("CSL bootstrap train paced: the last train started less than %s ago, %s", liveCSLBootstrapGlobalCooldown, wait)
	case PaceYield:
		return fmt.Sprintf("CSL bootstrap train paced: a user command waits for a slot, %s", wait)
	}
	return "CSL bootstrap train paced: " + wait
}

type paceStart struct {
	at    time.Time
	short string
	// settled says that the job completed and sent a train. A start that is not
	// settled is a job in flight: its slot returns if it turns out to be short.
	settled bool
}

type paceWaiter struct {
	user bool
	// until is the retry that the waiter was told, with a grace.
	until time.Time
}

type bootstrapPace struct {
	mu      sync.Mutex
	starts  []paceStart
	waiters map[string]paceWaiter
}

// prune drops the starts older than the window and the waiters that did not ask
// again. The caller holds the lock.
func (p *bootstrapPace) prune(now time.Time) {
	keep := p.starts[:0]
	for _, s := range p.starts {
		if now.Sub(s.at) < liveBootstrapPaceWindow {
			keep = append(keep, s)
		}
	}
	p.starts = keep
	for short, w := range p.waiters {
		if now.After(w.until) {
			delete(p.waiters, short)
		}
	}
}

// paceBootstrap books a train slot for the device, or returns the refusal. A
// refresh gives way while a user command waits for a slot. A job that can ask
// again (queued) joins the waiters; a response does not.
func (r *Live) paceBootstrap(short string, user, queued bool) *BootstrapPaced {
	p := &r.pace
	p.mu.Lock()
	defer p.mu.Unlock()
	now := r.now()
	p.prune(now)
	yield := false
	if !user {
		for other, w := range p.waiters {
			if other != short && w.user {
				yield = true
				break
			}
		}
	}
	free := liveBootstrapPaceMax - len(p.starts)
	if !r.airtimeLimit {
		free = liveBootstrapPaceMax
	}
	if free > 0 && !yield {
		if r.airtimeLimit {
			p.starts = append(p.starts, paceStart{at: now, short: short})
		}
		delete(p.waiters, short)
		return nil
	}
	retry, cause := liveBootstrapYield, PaceYield
	if free <= 0 {
		cause = PaceLimit
		// The oldest train leaves the window at its time. A job in flight may free
		// its slot earlier, when it completes short: look again soon.
		if oldest, ok := p.oldestSettled(); ok && p.settled() >= liveBootstrapPaceMax {
			retry = oldest.Add(liveBootstrapPaceWindow).Sub(now) + liveBootstrapPaceSlack
		}
	}
	return p.wait(short, user, queued, now, retry, cause)
}

// settled is the number of starts that sent a train.
func (p *bootstrapPace) settled() int {
	n := 0
	for _, s := range p.starts {
		if s.settled {
			n++
		}
	}
	return n
}

// oldestSettled is the time of the oldest start that sent a train.
func (p *bootstrapPace) oldestSettled() (time.Time, bool) {
	for _, s := range p.starts {
		if s.settled {
			return s.at, true
		}
	}
	return time.Time{}, false
}

// wait records a job that asks again after retry, and returns the refusal. A
// job that cannot ask again (a response) joins no queue of waiters. The caller
// holds the lock.
func (p *bootstrapPace) wait(short string, user, queued bool, now time.Time, retry time.Duration, cause PaceCause) *BootstrapPaced {
	if queued {
		if p.waiters == nil {
			p.waiters = make(map[string]paceWaiter)
		}
		w := p.waiters[short]
		p.waiters[short] = paceWaiter{user: w.user || user, until: now.Add(retry + liveBootstrapWaitGrace)}
	}
	return &BootstrapPaced{RetryAfter: retry, Waiting: len(p.waiters), Cause: cause}
}

// paceCooldown holds a queued job that wants a train for the rest of the global
// cooldown of the last train. It waits like a job without a slot, so the user
// priority and the order of the pace apply to it.
func (r *Live) paceCooldown(short string, user bool, wait time.Duration) *BootstrapPaced {
	p := &r.pace
	p.mu.Lock()
	defer p.mu.Unlock()
	now := r.now()
	p.prune(now)
	return p.wait(short, user, true, now, wait+liveBootstrapPaceSlack, PaceCooldown)
}

// settleBootstrapSlot closes the slot that a job booked, when the job completes.
// A job that sent a train keeps its slot for the window. A job that never reached
// the air, or that ran on an anchor, gives it back.
func (r *Live) settleBootstrapSlot(short string, result *cc1101.CSLTxResult) {
	p := &r.pace
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := len(p.starts) - 1; i >= 0; i-- {
		if p.starts[i].short != short || p.starts[i].settled {
			continue
		}
		if result != nil && result.Sent >= liveBootstrapTrainFrames {
			p.starts[i].settled = true
		} else {
			p.starts = append(p.starts[:i], p.starts[i+1:]...)
		}
		return
	}
}

// releaseBootstrapWait removes a device from the waiters: its job no longer needs a train.
func (r *Live) releaseBootstrapWait(short string) {
	p := &r.pace
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.waiters, short)
}

// BootstrapWaiting is the number of devices whose queued job waits for a slot of
// the bootstrap pace.
func (r *Live) BootstrapWaiting() int {
	p := &r.pace
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prune(r.now())
	return len(p.waiters)
}

// CSLBusy is the number of jobs and bursts that found the channel busy.
func (r *Live) CSLBusy() uint64 { return r.cslBusy.Load() }

func (r *Live) noteCSLBusy(err error) {
	if errors.Is(err, cc1101.ErrCSLTxBusy) {
		r.cslBusy.Add(1)
	}
}
