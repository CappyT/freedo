package daemon

// api_zone: the zone-level commands and the zone target view. A zone command
// goes to one member: capture-app-20260907 shows the commercial coordinator
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
	"time"

	"freedo/tado"
)

const (
	// zoneMinC and zoneMaxC bound a room setpoint: the range of the tado app,
	// whose captured writes span 5.00 C (away) to 25.00 C (boost).
	zoneMinC        = 5.0
	zoneMaxC        = 25.0
	zoneMaxDuration = tado.OverlayMaxDuration
)

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
			return nil, fmt.Errorf("zone %d elects no demand reporter and has %d members: "+
				"declare its reporter, the member the coordinator writes the zone to", z.Number, len(members))
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

// pathZoneWriter resolves the {number} path value to the zone and its writer.
func (d *daemon) pathZoneWriter(w http.ResponseWriter, r *http.Request) (*tado.Zone, *tado.Device, bool) {
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
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return nil, nil, false
	}
	return z, dev, true
}

// zoneOverlayBody is the overlay part of a zone setpoint or off request.
type zoneOverlayBody struct {
	Mode        string `json:"mode"`
	DurationSec *int   `json:"durationSec"`
}

// overlay maps the request to the 6240 value and the timer it arms: manual
// (2) runs until the next command, until_next (3) until the next block of the
// program, timer (3) for durationSec.
func (b zoneOverlayBody) overlay() (byte, time.Duration, error) {
	switch b.Mode {
	case "manual", "", "until_next":
		if b.DurationSec != nil {
			return 0, 0, errors.New("durationSec needs mode timer")
		}
		if b.Mode == "until_next" {
			return 3, 0, nil
		}
		return 2, 0, nil
	case "timer":
		if b.DurationSec == nil || *b.DurationSec <= 0 {
			return 0, 0, errors.New("mode timer needs a positive durationSec")
		}
		d := time.Duration(*b.DurationSec) * time.Second
		if d > zoneMaxDuration {
			return 0, 0, fmt.Errorf("durationSec exceeds %d", int(zoneMaxDuration.Seconds()))
		}
		return 3, d, nil
	}
	return 0, 0, errors.New("bad mode " + b.Mode + ": want manual, until_next or timer")
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

// overlayDuration is the coordinator timer of a zone overlay: an until_next
// overlay lasts up to the next block of the program, which a hot-water overlay
// needs. Every other mode keeps duration.
func (d *daemon) overlayDuration(e *tado.EmuState, z *tado.Zone, mode string, duration time.Duration) (time.Duration, error) {
	if mode != "until_next" {
		return duration, nil
	}
	next, known := d.untilNext(e, z.Number)
	switch {
	case known:
		return next, nil
	case z.Kind == tado.ZoneHotWater:
		return 0, errors.New("mode until_next needs a hot-water program")
	}
	return duration, nil
}

// roomOverlay is the zone write of a room overlay: the program setpoint the
// block restates, else fallback, and whether the zone already runs an overlay.
func (d *daemon) roomOverlay(e *tado.EmuState, z *tado.Zone, dev *tado.Device, overlay byte, duration time.Duration, fallback float64) tado.SetpointParams {
	sched := fallback
	if block, ok := d.zoneSched[z.Number]; ok {
		sched = rawSetpointC(d.inst.Load(), z.Number, block.c)
	}
	return tado.SetpointParams{
		ID: z.Number, Sched: sched, Overlay: overlay, Duration: duration,
		RunningOverlay: runningOverlay(e, dev.Short()),
	}
}

// postZoneSetpoint sets the target of a zone: a room overlay on the zone
// writer, or the hot-water overlay on the wall thermostat.
func (d *daemon) postZoneSetpoint(w http.ResponseWriter, r *http.Request) {
	z, dev, ok := d.pathZoneWriter(w, r)
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
	target := hex.EncodeToString(dev.WireEUI())
	v, err := d.do(func(e *tado.EmuState) (any, error) {
		d.applyHomeState(e)
		duration, err := d.overlayDuration(e, z, b.Mode, duration)
		if err != nil {
			return nil, err
		}
		if z.Kind == tado.ZoneHotWater {
			err = e.EnqueueDHWOverlay(target, z.Number, c, true, duration)
		} else {
			raw := rawSetpointC(d.inst.Load(), z.Number, c)
			sp := d.roomOverlay(e, z, dev, overlay, duration, raw)
			sp.Setpoint = raw
			err = e.EnqueueSetpoint(target, sp)
		}
		if err != nil {
			return nil, err
		}
		d.dispatchQueued(e, target)
		return e.Pending(target), nil
	})
	queued(w, v, d.persist(err), map[string]any{"zone": z.Number, "target": targetName(dev)})
}

// postZoneAuto returns a zone to its program.
func (d *daemon) postZoneAuto(w http.ResponseWriter, r *http.Request) {
	z, dev, ok := d.pathZoneWriter(w, r)
	if !ok {
		return
	}
	target := hex.EncodeToString(dev.WireEUI())
	v, err := d.do(func(e *tado.EmuState) (any, error) {
		d.applyHomeState(e)
		var err error
		if z.Kind == tado.ZoneHotWater {
			err = e.EnqueueDHWSchedule(target, z.Number)
		} else {
			err = e.EnqueueSchedule(target, z.Number)
		}
		if err != nil {
			return nil, err
		}
		d.dispatchQueued(e, target)
		return e.Pending(target), nil
	})
	queued(w, v, d.persist(err), map[string]any{"zone": z.Number, "target": targetName(dev)})
}

// postZoneOff turns a zone off with an overlay, 6260=0 and no 6280: the
// hot water on the wall thermostat, a room on its writer. The room block is
// the captured one of the app's "off until the next time block".
func (d *daemon) postZoneOff(w http.ResponseWriter, r *http.Request) {
	z, dev, ok := d.pathZoneWriter(w, r)
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
	target := hex.EncodeToString(dev.WireEUI())
	v, err := d.do(func(e *tado.EmuState) (any, error) {
		d.applyHomeState(e)
		duration, err := d.overlayDuration(e, z, b.Mode, duration)
		if err != nil {
			return nil, err
		}
		if z.Kind == tado.ZoneHotWater {
			err = e.EnqueueDHWOverlay(target, z.Number, 0, false, duration)
		} else {
			sp := d.roomOverlay(e, z, dev, overlay, duration, 0)
			sp.Off = true
			err = e.EnqueueSetpoint(target, sp)
		}
		if err != nil {
			return nil, err
		}
		d.dispatchQueued(e, target)
		return e.Pending(target), nil
	})
	queued(w, v, d.persist(err), map[string]any{"zone": z.Number, "target": targetName(dev)})
}

// postZoneBoost starts or cancels the boost of a heating zone: 25 C for
// tado.BoostDuration, then the return to schedule.
func (d *daemon) postZoneBoost(w http.ResponseWriter, r *http.Request) {
	z, dev, ok := d.pathZoneWriter(w, r)
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
	target := hex.EncodeToString(dev.WireEUI())
	v, err := d.do(func(e *tado.EmuState) (any, error) {
		d.applyHomeState(e)
		if err := e.EnqueueBoost(target, z.Number, *b.On); err != nil {
			return nil, err
		}
		d.dispatchQueued(e, target)
		return e.Pending(target), nil
	})
	queued(w, v, d.persist(err), map[string]any{"zone": z.Number, "target": targetName(dev), "on": *b.On})
}

// overlayKind names a 6240 overlay.
func overlayKind(overlay byte) string {
	switch overlay {
	case 2:
		return "manual"
	case 3:
		return "until_next"
	}
	return "overlay " + strconv.Itoa(int(overlay))
}

// zoneTargetView is what zone z targets now, from the z/s block the daemon
// holds, its program, its window and the home state. targetC is null when the
// device falls back to a value of its own: an open window, the away state, an
// off overlay or a program block that is off. Hot water follows the away state
// like the cloud: with no overlay it is off. On air a timer is an until_next
// overlay; endsAt is the deadline the coordinator keeps.
func (d *daemon) zoneTargetView(e *tado.EmuState, z *tado.Zone) map[string]any {
	hotWater := z.Kind == tado.ZoneHotWater
	view := map[string]any{
		"targetC": nil, "source": nil, "overlay": nil,
		"programC": nil, "programOff": false, "boost": false, "known": false,
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
		overlay := map[string]any{"kind": kind, "on": on, "setpointC": nil, "endsAt": nil}
		if on && block.SetpointC != nil {
			overlay["setpointC"] = correctedC(d.inst.Load(), z.Number, *block.SetpointC)
		}
		if timed {
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
		}
	case known:
		view["source"] = "program"
		if hasProgram && !program.off {
			view["targetC"] = program.c
		}
	}
	return view
}
