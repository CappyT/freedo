package daemon

// window: the open window state of each heating zone. Two sources hold a zone
// open: the detector of a member, through its z/ow report, and an external one,
// through the API. The zone writer owes a z/s write with 62e0 each time the
// zone itself opens or closes, until it confirms the value.

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"time"

	"freedo/tado"
)

// windowSource names who holds a zone window open.
type windowSource string

const (
	windowDevice   windowSource = "device"
	windowExternal windowSource = "external"
)

// windowHold is one source holding a zone open until deadline. The device hold
// lasts the 62c0 of the zone; the external one lasts until its close, capped by
// options.external_window_max_sec.
type windowHold struct {
	since, deadline time.Time
}

// zoneWindow holds the sources that keep one zone open.
type zoneWindow map[windowSource]windowHold

// open is the precedence rule: the zone stays open while any source holds it,
// so it closes only when the device timer has run out and the external source
// has closed.
func (w zoneWindow) open() bool { return len(w) > 0 }

// windowWrite names one member of a zone the engine owes a 62e0 write.
type windowWrite struct {
	zone   int
	target string
}

// windowOwed is the retry state of one owed write. The owed set itself is
// durable, so a write the queue lost to a restart goes out again; the attempt
// count is not.
type windowOwed struct {
	attempts int
	nextAt   time.Time
	gaveUp   bool
	// blockLogged holds that the log already names the missing block.
	blockLogged bool
}

const (
	// windowRetryEvery is the cadence of the other proactive writes.
	windowRetryEvery = proactiveDownlinkRetryBackoff
	// windowRetryLimit spans 20 min, more than one 900 s zone refresh, which is
	// when a valve the daemon has not heard since a restart reports its block.
	windowRetryLimit = 40
)

var errWindowZone = errors.New("the zone has no open window")

// windowZone returns the zone when the engine drives its window: a declared
// heating zone. The hot water zone has none.
func (d *daemon) windowZone(number int) (*tado.Zone, error) {
	inst := d.inst.Load()
	if inst == nil {
		return nil, errNoInstall
	}
	z, err := inst.ZoneByNumber(number)
	if err != nil {
		return nil, err
	}
	if z.Kind != tado.ZoneHeating {
		return nil, fmt.Errorf("zone %d: %w", number, errWindowZone)
	}
	return z, nil
}

// setWindow applies the open or the close of one source to zone. When the
// zone itself changes state its zoneWriteTargets owe the new 62e0. A source
// that already holds the zone keeps its hold: a repeated open restarts
// nothing. It reports whether the stored state changed.
func (d *daemon) setWindow(e *tado.EmuState, zone int, src windowSource, open bool, hold time.Duration, dispatch func(string)) bool {
	next, changed := d.nextWindow(zone, src, open, hold)
	if !changed {
		return false
	}
	d.putWindow(e, zone, next, dispatch)
	return true
}

// nextWindow returns, as a new map, the sources that hold zone once src opens
// or closes, and whether they differ from the current ones. It changes nothing.
func (d *daemon) nextWindow(zone int, src windowSource, open bool, hold time.Duration) (zoneWindow, bool) {
	w := d.windows[zone]
	if _, held := w[src]; open == held {
		return w, false
	}
	next := make(zoneWindow, len(w)+1)
	for s, h := range w {
		next[s] = h
	}
	if open {
		now := d.retryNow().UTC()
		next[src] = windowHold{since: now, deadline: now.Add(hold)}
	} else {
		delete(next, src)
	}
	return next, true
}

// putWindow makes next the sources that hold zone. When the zone itself opens
// or closes, its zoneWriteTargets owe the new 62e0.
func (d *daemon) putWindow(e *tado.EmuState, zone int, next zoneWindow, dispatch func(string)) {
	was := d.windows[zone].open()
	if len(next) == 0 {
		delete(d.windows, zone)
	} else {
		d.windows[zone] = next
	}
	if next.open() != was {
		d.oweZone(e, zone, dispatch)
	}
}

// oweZone makes the zoneWriteTargets of zone owe its current 62e0 and writes
// them. The captured zone 1 report puts 62e0 on the wall thermostat, its
// writer, and the valve takes it from the relayed echo. The emulator learns the
// new state first, so a schedule rewrite the home state queues on the way
// carries it too.
func (d *daemon) oweZone(e *tado.EmuState, zone int, dispatch func(string)) {
	for _, target := range d.zoneWriteTargets(e, zone) {
		d.windowOwed[windowWrite{zone: zone, target: target}] = &windowOwed{}
	}
	e.SetOpenWindows(d.windowDesired())
	d.tickHomeState(e)
	d.settleWindows(e, dispatch)
}

// windowDesired is the 62e0 the emulator stamps per zone: 1 on an open zone,
// 0 on a closed one that still owes a close.
func (d *daemon) windowDesired() map[int]bool {
	out := make(map[int]bool, len(d.windows))
	for w := range d.windowOwed {
		out[w.zone] = false
	}
	for zone, w := range d.windows {
		out[zone] = w.open()
	}
	return out
}

// settleWindows drops every owed write the member now confirms and writes the
// others that are due. A member that cannot take the write retries every
// windowRetryEvery; after windowRetryLimit attempts the engine stops writing
// it and flags it, and the stamp of its next z/s write settles it. A heating
// write already in flight is not an attempt: its outcome decides. It reports
// whether the owed set changed.
func (d *daemon) settleWindows(e *tado.EmuState, dispatch func(string)) bool {
	owed := make([]windowWrite, 0, len(d.windowOwed))
	for w := range d.windowOwed {
		owed = append(owed, w)
	}
	sort.Slice(owed, func(i, j int) bool {
		if owed[i].zone != owed[j].zone {
			return owed[i].zone < owed[j].zone
		}
		return owed[i].target < owed[j].target
	})
	now := d.retryNow()
	changed := false
	for _, w := range owed {
		o := d.windowOwed[w]
		if !d.windowTarget(w) {
			delete(d.windowOwed, w)
			changed = true
			continue
		}
		want := d.windows[w.zone].open()
		if open, ok := e.ConfirmedOpenWindow(w.target); ok && open == want {
			delete(d.windowOwed, w)
			changed = true
			continue
		}
		if o.gaveUp || now.Before(o.nextAt) {
			continue
		}
		o.nextAt = now.Add(windowRetryEvery)
		err := e.EnqueueOpenWindow(w.target, w.zone, want)
		switch {
		case errors.Is(err, tado.ErrHeatingBlockPending):
			continue
		case err == nil:
			dispatch(w.target)
		case errors.Is(err, tado.ErrHeatingBlockUnavailable):
			if !o.blockLogged {
				o.blockLogged = true
				log.Printf("zone %d window on %s: %v; retrying every %s until the member reports its block",
					w.zone, w.target, err, windowRetryEvery)
			}
		default:
			log.Printf("zone %d window on %s: %v", w.zone, w.target, err)
		}
		o.attempts++
		if o.attempts >= windowRetryLimit {
			o.gaveUp = true
			log.Printf("zone %d window on %s: no confirmed 62e0=%d after %d attempts; its next z/s write carries it",
				w.zone, w.target, boolInt(want), o.attempts)
		}
	}
	if changed {
		e.SetOpenWindows(d.windowDesired())
	}
	return changed
}

// windowTarget reports whether w still names a member of a heating zone. Any
// member qualifies, so an owed write outlives a change of the writer's offline
// flag.
func (d *daemon) windowTarget(w windowWrite) bool {
	if _, err := d.windowZone(w.zone); err != nil {
		return false
	}
	return d.zoneMember(w.zone, w.target)
}

// takeWindowReports opens the window of every zone whose member reported one,
// when the zone has its detection on. The hold lasts the 62c0 of the zone. A
// close report drops the device hold at once: the member's 62e0 went to 0 from
// a source other than the coordinator, so restating 1 would reopen it.
func (d *daemon) takeWindowReports(e *tado.EmuState) {
	for _, r := range e.TakeOpenWindowReports() {
		z, err := d.windowZone(r.Zone)
		if err != nil {
			log.Printf("open window report from %s: %v", r.Device, err)
			continue
		}
		if !d.zoneMember(r.Zone, r.Device) {
			log.Printf("open window report from %s: not a member of zone %d", r.Device, r.Zone)
			continue
		}
		if !r.Close && !z.OpenWindowEnabled() {
			continue
		}
		hold := time.Duration(z.OpenWindowSeconds()) * time.Second
		if d.setWindow(e, r.Zone, windowDevice, !r.Close, hold, d.requestQueuedDispatch) {
			if err := d.saveWindows(); err != nil {
				log.Printf("persist zone %d window: %v", r.Zone, err)
			}
		}
	}
}

func (d *daemon) zoneMember(zone int, target string) bool {
	members, err := d.inst.Load().ZoneMembers(zone)
	if err != nil {
		return false
	}
	for _, member := range members {
		if hex.EncodeToString(member.WireEUI()) == target {
			return true
		}
	}
	return false
}

// tickWindows ends every hold past its deadline, drops the state of a zone
// that no longer has a window and settles the owed writes.
func (d *daemon) tickWindows(e *tado.EmuState) {
	now := d.retryNow()
	changed := false
	for _, zone := range sortedZones(d.windows) {
		if _, err := d.windowZone(zone); err != nil {
			delete(d.windows, zone)
			changed = true
			continue
		}
		for src, hold := range d.windows[zone] {
			if !now.Before(hold.deadline) {
				if d.setWindow(e, zone, src, false, 0, d.requestQueuedDispatch) {
					changed = true
				}
			}
		}
	}
	if d.settleWindows(e, d.requestQueuedDispatch) {
		changed = true
	}
	if changed {
		e.SetOpenWindows(d.windowDesired())
		if err := d.saveWindows(); err != nil {
			log.Printf("persist zone windows: %v", err)
		}
	}
}

func sortedZones(windows map[int]zoneWindow) []int {
	out := make([]int, 0, len(windows))
	for zone := range windows {
		out = append(out, zone)
	}
	sort.Ints(out)
	return out
}

// restoreWindows loads the stored zone windows before the engine starts. A
// hold keeps its stored deadline, so the time the daemon was down counts, and
// every write still owed goes out again on the first tick.
func (d *daemon) restoreWindows(e *tado.EmuState) error {
	if d.state == nil {
		return nil
	}
	windows, owed, err := d.state.ZoneWindows(context.Background())
	if err != nil {
		return err
	}
	d.windows = windows
	for _, w := range owed {
		d.windowOwed[w] = &windowOwed{}
	}
	e.SetOpenWindows(d.windowDesired())
	return nil
}

// saveWindows writes the zone window state to the database.
func (d *daemon) saveWindows() error {
	return d.saveWindowState(d.windows, d.owedWrites(nil))
}

func (d *daemon) saveWindowState(windows map[int]zoneWindow, owed []windowWrite) error {
	if d.state == nil {
		return nil
	}
	if err := d.state.SetZoneWindows(context.Background(), windows, owed); err != nil {
		return &stateSaveError{err: err}
	}
	return nil
}

// owedWrites lists the owed window writes plus extra, sorted and without
// repeats.
func (d *daemon) owedWrites(extra []windowWrite) []windowWrite {
	set := make(map[windowWrite]bool, len(d.windowOwed)+len(extra))
	for w := range d.windowOwed {
		set[w] = true
	}
	for _, w := range extra {
		set[w] = true
	}
	out := make([]windowWrite, 0, len(set))
	for w := range set {
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].zone != out[j].zone {
			return out[i].zone < out[j].zone
		}
		return out[i].target < out[j].target
	})
	return out
}

// saveNextWindow stores the zone window state putWindow is about to reach:
// next as the holds of zone and, when the zone opens or closes, a write owed to
// each of its zoneWriteTargets. It returns the owed writes it stored.
func (d *daemon) saveNextWindow(e *tado.EmuState, zone int, next zoneWindow) ([]windowWrite, error) {
	windows := make(map[int]zoneWindow, len(d.windows)+1)
	for z, w := range d.windows {
		windows[z] = w
	}
	delete(windows, zone)
	if len(next) > 0 {
		windows[zone] = next
	}
	var extra []windowWrite
	if next.open() != d.windows[zone].open() {
		for _, target := range d.zoneWriteTargets(e, zone) {
			extra = append(extra, windowWrite{zone: zone, target: target})
		}
	}
	owed := d.owedWrites(extra)
	return owed, d.saveWindowState(windows, owed)
}

// windowView renders the window state of one zone. since is when the zone
// opened and deadline when it closes unless a source acts again. undelivered
// lists the members the engine gave up writing.
func (d *daemon) windowView(zone int) map[string]any {
	w := d.windows[zone]
	undelivered := []string{}
	for ww, o := range d.windowOwed {
		if ww.zone == zone && o.gaveUp {
			undelivered = append(undelivered, ww.target)
		}
	}
	sort.Strings(undelivered)
	view := map[string]any{"open": w.open(), "source": nil, "since": nil, "deadline": nil, "undelivered": undelivered}
	if !w.open() {
		return view
	}
	var since, deadline time.Time
	for _, hold := range w {
		if since.IsZero() || hold.since.Before(since) {
			since = hold.since
		}
		if hold.deadline.After(deadline) {
			deadline = hold.deadline
		}
	}
	source := "both"
	if len(w) == 1 {
		for src := range w {
			source = string(src)
		}
	}
	view["source"] = source
	view["since"] = since.UTC().Format(time.RFC3339)
	view["deadline"] = deadline.UTC().Format(time.RFC3339)
	return view
}

// windowViews renders the window state of every heating zone, keyed by number.
func (d *daemon) windowViews() map[string]any {
	out := map[string]any{}
	inst := d.inst.Load()
	if inst == nil {
		return out
	}
	for i := range inst.Zones {
		if inst.Zones[i].Kind == tado.ZoneHeating {
			out[strconv.Itoa(inst.Zones[i].Number)] = d.windowView(inst.Zones[i].Number)
		}
	}
	return out
}

// postZoneWindow opens or closes the external window of a zone. The zone stays
// open while its own detector still holds it.
func (d *daemon) postZoneWindow(w http.ResponseWriter, r *http.Request) {
	zone, _, ok := d.pathZoneMembers(w, r)
	if !ok {
		return
	}
	var b struct {
		Open *bool `json:"open"`
	}
	if !decodeBody(w, r, &b) {
		return
	}
	if b.Open == nil {
		writeErr(w, http.StatusBadRequest, "open is required")
		return
	}
	z, err := d.windowZone(zone)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	hold := d.inst.Load().Options.ExternalWindowMax()
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		next, changed := d.nextWindow(z.Number, windowExternal, *b.Open, hold)
		if !changed {
			return d.windowView(z.Number), nil
		}
		// Saved before the engine moves, so a failed save changes nothing.
		saved, err := d.saveNextWindow(e, z.Number, next)
		if err != nil {
			return nil, err
		}
		d.putWindow(e, z.Number, next, func(target string) { d.dispatchQueued(e, target) })
		// The writes the members already confirm are settled by now: store the
		// smaller set too. The window itself is stored, so this save only trims.
		if !slices.Equal(saved, d.owedWrites(nil)) {
			if err := d.saveWindows(); err != nil {
				log.Printf("persist zone %d window: %v", z.Number, err)
			}
		}
		return d.windowView(z.Number), nil
	})
	if writeHandlerErr(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, v)
}
