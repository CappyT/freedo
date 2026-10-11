package daemon

// api_zone: the zone-level commands and the zone target view. A zone command
// goes to one member: the app capture shows the commercial coordinator
// writing z/s?id=N to the elected demand reporter alone, the wall thermostat
// included when it holds the role, for a setpoint, a timer, a return to
// schedule, a boost and the home or away burst. The other members take the
// block from the reporter's z/s?lid=N echo, which the coordinator relays.

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"freedo/tado"
)

const (
	// zoneMinC and zoneMaxC bound a room setpoint: the range of the tado app,
	// whose captured writes span 5.00 C (away) to 25.00 C (boost).
	zoneMinC        = tado.RoomMinC
	zoneMaxC        = tado.RoomMaxC
	zoneMaxDuration = tado.OverlayMaxDuration
)

// roomSetpointErr refuses a room setpoint outside zoneMinC..zoneMaxC. The wire
// carries centi-degrees in a u16, so a value past the range wraps: 700 C
// reads 44.64 C.
func roomSetpointErr(name string, c float64) error {
	if c < zoneMinC || c > zoneMaxC {
		return fmt.Errorf("%s must be within %g..%g C", name, zoneMinC, zoneMaxC)
	}
	return nil
}

// errNoZoneWriter marks a zone that elects no demand reporter and has several
// members: no one member is the writer.
var errNoZoneWriter = errors.New("zone elects no demand reporter")

// zoneWriter returns the member the coordinator writes a zone command to: the
// wall thermostat of the hot-water zone, else the demand reporter, else the
// only member of the zone.
func zoneWriter(inst *tado.Install, z *tado.Zone) (*tado.Device, error) {
	if z.Kind == tado.ZoneHotWater {
		return inst.HotWaterTarget()
	}
	dev, err := inst.DemandReporter(z.Number)
	if err != nil {
		members, merr := inst.ZoneMembers(z.Number)
		if merr != nil {
			return nil, merr
		}
		if len(members) != 1 {
			return nil, fmt.Errorf("zone %d has %d members and %w: "+
				"declare its reporter, the member the coordinator writes the zone to", z.Number, len(members), errNoZoneWriter)
		}
		dev = members[0]
	}
	if dev.Zone != nil && *dev.Zone != z.Number {
		return nil, fmt.Errorf("zone %d writer %s answers to id %d", z.Number, dev.Ref(), *dev.Zone)
	}
	return dev, nil
}

// zoneWriteTargets lists the wire EUIs a zone-wide write goes to: the zone
// writer alone, as the commercial coordinator does. Every member takes the
// write when the zone has no writer or its writer is offline: the other members
// take the block from the writer's echo, and a writer that is not on the air
// echoes nothing.
func (d *daemon) zoneWriteTargets(e *tado.EmuState, zone int) []string {
	inst := d.inst.Load()
	if inst == nil {
		return nil
	}
	z, err := inst.ZoneByNumber(zone)
	if err != nil {
		return nil
	}
	if dev, err := zoneWriter(inst, z); err == nil && !d.deviceOffline(e, dev) {
		return []string{hex.EncodeToString(dev.WireEUI())}
	}
	members, err := inst.ZoneMembers(zone)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(members))
	for _, member := range members {
		out = append(out, hex.EncodeToString(member.WireEUI()))
	}
	return out
}

// deviceOffline reports the offline flag of /api/health for dev.
func (d *daemon) deviceOffline(e *tado.EmuState, dev *tado.Device) bool {
	vv := tado.ValveView{Short: dev.Short()}
	for _, v := range e.NodeViews() {
		if v.Short == vv.Short {
			vv = v
			break
		}
	}
	d.applyAlerts(&vv, dev.Kind, d.inst.Load().Options, d.retryNow())
	return vv.Offline
}

// pathZoneWriters resolves the {number} path value to the zone and the devices
// that take its command: the zone writer, or every member while the zone has
// none, as zoneWriteTargets does.
func (d *daemon) pathZoneWriters(w http.ResponseWriter, r *http.Request) (*tado.Zone, []*tado.Device, bool) {
	inst := d.inst.Load()
	if inst == nil {
		writeResolveErr(w, errNoInstall)
		return nil, nil, false
	}
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid zone number")
		return nil, nil, false
	}
	z, err := inst.ZoneByNumber(number)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return nil, nil, false
	}
	dev, err := zoneWriter(inst, z)
	if errors.Is(err, errNoZoneWriter) {
		members, merr := inst.ZoneMembers(z.Number)
		if merr != nil {
			writeErr(w, http.StatusBadRequest, merr.Error())
			return nil, nil, false
		}
		return z, members, true
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return nil, nil, false
	}
	return z, []*tado.Device{dev}, true
}

// enqueueEach runs enqueue on each of devs, dispatches what it queued and
// returns the downlinks pending on them.
func (d *daemon) enqueueEach(e *tado.EmuState, devs []*tado.Device, enqueue func(dev *tado.Device, target string) error) (int, error) {
	pending := 0
	for _, dev := range devs {
		target := hex.EncodeToString(dev.WireEUI())
		if err := enqueue(dev, target); err != nil {
			return 0, err
		}
		d.dispatchQueued(e, target)
		pending += e.Pending(target)
	}
	return pending, nil
}

// targetNames is targetName for each of devs, joined by commas.
func targetNames(devs []*tado.Device) string {
	names := make([]string, len(devs))
	for i, dev := range devs {
		names[i] = targetName(dev)
	}
	return strings.Join(names, ",")
}

// zoneOverlayBody is the overlay part of a zone setpoint or off request.
type zoneOverlayBody struct {
	Mode        string `json:"mode"`
	DurationSec *int   `json:"durationSec"`
}

// overlay maps the request to the 6240 value and the timer it arms: manual
// (2) runs until the next command, until_next (3) until the next block of the
// program, timer (3) for durationSec, until_auto_change (1) until the next
// block of the program or the next switch between home and away, whichever
// comes first. The 1 of the last one is the app's TADO_MODE as captured.
func (b zoneOverlayBody) overlay() (byte, time.Duration, error) {
	switch b.Mode {
	case "manual", "", "until_next", tado.OverlayEndUntilAutoChange:
		if b.DurationSec != nil {
			return 0, 0, errors.New("durationSec needs mode timer")
		}
		switch b.Mode {
		case "until_next":
			return 3, 0, nil
		case tado.OverlayEndUntilAutoChange:
			return 1, 0, nil
		}
		return 2, 0, nil
	case "timer":
		if b.DurationSec == nil || *b.DurationSec <= 0 {
			return 0, 0, errors.New("mode timer needs a positive durationSec")
		}
		d, err := overlaySeconds(*b.DurationSec)
		if err != nil {
			return 0, 0, err
		}
		return 3, d, nil
	}
	return 0, 0, errors.New("bad mode " + b.Mode + ": want manual, until_next, timer or " + tado.OverlayEndUntilAutoChange)
}

// endRule is the rule the daemon keeps with the overlay the request sets. A
// manual overlay has none.
func (b zoneOverlayBody) endRule() string {
	switch b.Mode {
	case tado.OverlayEndUntilNext, tado.OverlayEndTimer, tado.OverlayEndUntilAutoChange:
		return b.Mode
	}
	return ""
}

// overlaySeconds is the timer a durationSec arms, within zoneMaxDuration. The
// bound is checked in seconds: past it the product with time.Second overflows
// and wraps to a tiny or negative timer, which leaves the overlay on for good.
func overlaySeconds(sec int) (time.Duration, error) {
	maxSec := int(zoneMaxDuration / time.Second)
	switch {
	case sec <= 0:
		return 0, errors.New("durationSec must be positive")
	case sec > maxSec:
		return 0, fmt.Errorf("durationSec exceeds %d", maxSec)
	}
	return time.Duration(sec) * time.Second, nil
}

// untilNext returns how long an until_next overlay on zone lasts: up to the
// next block of its program. ok is false when the daemon holds no program.
func (d *daemon) untilNext(e *tado.EmuState, zone int) (time.Duration, bool) {
	inst := d.inst.Load()
	if inst == nil {
		return 0, false
	}
	now := d.retryNow()
	next, ok := d.nextZoneBlock(e, inst, zone, now)
	if !ok || !next.After(now) {
		return 0, false
	}
	return next.Sub(now), true
}

// overlayDuration is the coordinator timer of a zone overlay: an until_next or
// until_auto_change overlay lasts up to the next block of the program, which a
// hot-water until_next overlay needs. Without a program until_auto_change has
// no deadline. Every other mode keeps duration.
func (d *daemon) overlayDuration(e *tado.EmuState, z *tado.Zone, mode string, duration time.Duration) (time.Duration, error) {
	if mode != "until_next" && mode != tado.OverlayEndUntilAutoChange {
		return duration, nil
	}
	next, known := d.untilNext(e, z.Number)
	switch {
	case known:
		return next, nil
	case z.Kind == tado.ZoneHotWater && mode == "until_next":
		return 0, errors.New("mode until_next needs a hot-water program")
	}
	return duration, nil
}

// roomOverlay is the zone write of a room overlay: the program setpoint the
// block restates, else fallback, and whether the zone already runs an overlay.
func (d *daemon) roomOverlay(e *tado.EmuState, z *tado.Zone, dev *tado.Device, overlay byte, duration time.Duration, fallback float64) tado.SetpointParams {
	sched := fallback
	if block, ok := d.zoneSched[z.Number]; ok {
		sched = d.rawSetpointC(d.inst.Load(), z.Number, block.c)
	}
	return tado.SetpointParams{
		ID: z.Number, Sched: sched, Overlay: overlay, Duration: duration,
		RunningOverlay: runningOverlay(e, dev.Short(), z.Number),
	}
}

// postZoneSetpoint sets the target of a zone: a room overlay on the zone
// writer, or the hot-water overlay on the wall thermostat.
func (d *daemon) postZoneSetpoint(w http.ResponseWriter, r *http.Request) {
	z, devs, ok := d.pathZoneWriters(w, r)
	if !ok {
		return
	}
	var b struct {
		SetpointC *float64 `json:"setpointC"`
		zoneOverlayBody
	}
	if !decodeBody(w, r, &b) {
		return
	}
	overlay, duration, err := b.overlay()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if b.SetpointC == nil {
		writeErr(w, http.StatusBadRequest, "setpointC is required")
		return
	}
	c := *b.SetpointC
	if _, err := centiDegrees(c); err != nil {
		writeErr(w, http.StatusBadRequest, "setpointC: "+err.Error())
		return
	}
	lo, hi := zoneMinC, zoneMaxC
	if z.Kind == tado.ZoneHotWater {
		lo, hi = tado.DHWMinC, tado.DHWMaxC
	}
	if c < lo || c > hi {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("setpointC must be within %g..%g", lo, hi))
		return
	}
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		d.applyHomeState(e)
		duration, err := d.overlayDuration(e, z, b.Mode, duration)
		if err != nil {
			return nil, err
		}
		return d.enqueueEach(e, devs, func(dev *tado.Device, target string) error {
			if z.Kind == tado.ZoneHotWater {
				return e.EnqueueDHWOverlay(target, z.Number, c, true, duration, b.endRule())
			}
			raw := d.rawSetpointC(d.inst.Load(), z.Number, c)
			sp := d.roomOverlay(e, z, dev, overlay, duration, raw)
			sp.Setpoint = raw
			sp.EndRule = b.endRule()
			return e.EnqueueSetpoint(target, sp)
		})
	})
	queued(w, v, d.persist(err), map[string]any{"zone": z.Number, "target": targetNames(devs)})
}

// postZoneAuto returns a zone to its program.
func (d *daemon) postZoneAuto(w http.ResponseWriter, r *http.Request) {
	z, devs, ok := d.pathZoneWriters(w, r)
	if !ok {
		return
	}
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		d.applyHomeState(e)
		return d.enqueueEach(e, devs, func(_ *tado.Device, target string) error {
			if z.Kind == tado.ZoneHotWater {
				return e.EnqueueDHWSchedule(target, z.Number)
			}
			return e.EnqueueSchedule(target, z.Number)
		})
	})
	queued(w, v, d.persist(err), map[string]any{"zone": z.Number, "target": targetNames(devs)})
}

// postZoneOff turns a zone off with an overlay, 6260=0 and no 6280: the
// hot water on the wall thermostat, a room on its writer. The room block is
// the captured one of the app's "off until the next time block".
func (d *daemon) postZoneOff(w http.ResponseWriter, r *http.Request) {
	z, devs, ok := d.pathZoneWriters(w, r)
	if !ok {
		return
	}
	var b zoneOverlayBody
	if !decodeBody(w, r, &b) {
		return
	}
	overlay, duration, err := b.overlay()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		d.applyHomeState(e)
		duration, err := d.overlayDuration(e, z, b.Mode, duration)
		if err != nil {
			return nil, err
		}
		return d.enqueueEach(e, devs, func(dev *tado.Device, target string) error {
			if z.Kind == tado.ZoneHotWater {
				return e.EnqueueDHWOverlay(target, z.Number, 0, false, duration, b.endRule())
			}
			sp := d.roomOverlay(e, z, dev, overlay, duration, 0)
			sp.Off = true
			sp.EndRule = b.endRule()
			return e.EnqueueSetpoint(target, sp)
		})
	})
	queued(w, v, d.persist(err), map[string]any{"zone": z.Number, "target": targetNames(devs)})
}

// postZoneBoost starts or cancels the boost of a heating zone: 25 C for
// tado.BoostDuration, then the return to schedule.
func (d *daemon) postZoneBoost(w http.ResponseWriter, r *http.Request) {
	z, devs, ok := d.pathZoneWriters(w, r)
	if !ok {
		return
	}
	if z.Kind != tado.ZoneHeating {
		writeErr(w, http.StatusBadRequest, "boost requires a heating zone")
		return
	}
	var b struct {
		On *bool `json:"on"`
	}
	if !decodeBody(w, r, &b) {
		return
	}
	if b.On == nil {
		writeErr(w, http.StatusBadRequest, "on is required")
		return
	}
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		d.applyHomeState(e)
		return d.enqueueEach(e, devs, func(_ *tado.Device, target string) error {
			return e.EnqueueBoost(target, z.Number, *b.On)
		})
	})
	queued(w, v, d.persist(err), map[string]any{"zone": z.Number, "target": targetNames(devs), "on": *b.On})
}

// overlayRule names the end rule a zone overlay runs under: the one it was set
// with, else the one its 6240 and its timer imply. A timer with no stored rule
// reads as until_next, as it did before the rule was kept.
func overlayRule(overlay byte, timer tado.OverlayTimer, timed bool) string {
	switch {
	case timed && timer.Rule != "":
		return timer.Rule
	case overlay == 1:
		return tado.OverlayEndUntilAutoChange
	case timed || overlay == 3:
		return tado.OverlayEndUntilNext
	}
	return tado.OverlayEndManual
}

// overlayKind names a 6240 overlay.
func overlayKind(overlay byte) string {
	switch overlay {
	case 1:
		return tado.OverlayEndUntilAutoChange
	case 2:
		return "manual"
	case 3:
		return "until_next"
	}
	return "overlay " + strconv.Itoa(int(overlay))
}

// zoneTargetView is what zone z targets now, from the z/s block the daemon
// holds, its program, its window and the home state. targetC is null when the
// device falls back to a value of its own: an open window, the away state of a
// zone with no away temperature, an off overlay or a program block that is off.
// Hot water follows the away state like the cloud: with no overlay it is off. On air a timer is an until_next
// overlay: kind follows the 6240, rule is the end rule the overlay was set
// with (manual, timer, until_next or until_auto_change) and endsAt is the
// deadline the coordinator keeps, null while the rule has none.
//
// hasProgram tells whether the daemon holds a weekly program for the zone.
// source is "program" without one too: the zone then runs the block its device
// reports, and programC is the 6200 of the zone cache, null when it holds none.
func (d *daemon) zoneTargetView(e *tado.EmuState, z *tado.Zone) map[string]any {
	hotWater := z.Kind == tado.ZoneHotWater
	_, held := d.zonePrograms(e, d.inst.Load(), z.Number)
	view := map[string]any{
		"targetC": nil, "source": nil, "overlay": nil,
		"programC": nil, "programOff": false, "hasProgram": held, "boost": false, "known": false,
	}
	program, hasProgram := d.zoneSched[z.Number]
	if hasProgram {
		view["programC"], view["programOff"] = program.c, program.off
	}
	block, known := e.ZoneBlock(z.Number)
	view["known"] = known
	timer, timed := e.ZoneOverlayTimer(z.Number)
	boost := !hotWater && timed && timer.Boost && known && block.Overlay != 0
	view["boost"] = boost
	on := !hasProgram || !program.off
	if known && block.Overlay != 0 {
		on = block.Enabled == nil || *block.Enabled
		kind := overlayKind(block.Overlay)
		if boost {
			kind = "boost"
		}
		overlay := map[string]any{
			"kind": kind, "rule": overlayRule(block.Overlay, timer, timed), "on": on, "setpointC": nil, "endsAt": nil,
		}
		if on && block.SetpointC != nil {
			overlay["setpointC"] = d.correctedC(d.inst.Load(), z.Number, *block.SetpointC)
		}
		if timed && !timer.ExpiresAt.IsZero() {
			overlay["endsAt"] = timer.ExpiresAt.UTC().Format(time.RFC3339)
		}
		view["overlay"] = overlay
	}
	view["on"] = on
	switch {
	case !hotWater && (d.windows[z.Number].open() || known && block.OpenWindow):
		view["source"] = "window"
	case known && block.Overlay != 0:
		view["source"] = "overlay"
		if boost {
			view["source"] = "boost"
		}
		view["targetC"] = view["overlay"].(map[string]any)["setpointC"]
	case d.away:
		view["source"] = "away"
		// Away, 61e0=0 is the hot-water enable of a zone with no overlay.
		if hotWater {
			view["on"] = false
		} else if c, ok := d.inst.Load().Options.AwayTemperature(z.Number); ok {
			view["targetC"] = c
		}
	case known:
		view["source"] = "program"
		if hasProgram && !program.off {
			view["targetC"] = program.c
		}
	}
	return view
}
