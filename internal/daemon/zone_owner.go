package daemon

// zone_owner: the daemon as the owner of a heating zone. The zone document
// names the coordinator as owner and measuring endpoint (tado/zone_owner.go),
// so the members take the zone measure from us instead of from the elected
// valve: the daemon runs the controller of the valve on the external room
// temperature, serves the measure to a GET z/p?lid=N and pushes it to every
// member. The valve next to the radiator no longer regulates on its own
// sensor, so no offset is needed.
//
// The controller is the one of the valve, ctrl_pct_update@0x31970, with the
// gains of the zone document (tado.ZonePID), in centi-degrees and percent:
//
//	Kp*(SP-T)/2000 + Ki*integ/30000 - Kd*(T-Tref)/2000, clamped to 0..100
//
// integ sums the error once per step and stays within 0..3000000/Ki. Tref is
// the temperature ownerTrefAge earlier. The valve runs the controller on its
// own schedule, which no capture shows: the daemon steps it once per minute.
//
// Without an external reading the controller reads the calibrated temperature
// of the measuring device, so the zone keeps regulating on the valve sensor as
// it does with the device as owner. Without a target (an open window, an off
// block, away without a temperature) the demand is 0 and the integrator
// restarts. The state is not stored: a restart starts the controller from
// zero, as a valve does after each read of the zone document.
//
// The observe registrations are stored (meta zone_observers): after a restart
// the wall thermostat keeps its registration and ignores a notification under
// an unknown token or with a lower sequence, so without them it would hold its
// last demand until its own deadline, up to 30 minutes.
//
// Unverified on air: no capture shows a member with the coordinator as owner.
// The daemon logs every zone-plane request a member addresses to us for an
// owned zone, so the on-air check can see what the members ask.

import (
	"context"
	"encoding/hex"
	"log"
	"math"
	"slices"
	"strings"
	"time"

	"freedo/tado"
)

const (
	// ownerStep paces the controller of each owned zone.
	ownerStep = time.Minute
	// ownerTrefAge is how far back the derivative term looks: "about 10
	// minutes" in the valve.
	ownerTrefAge = 10 * time.Minute
	// ownerPushEvery bounds the time between two pushes of an unchanged
	// measure. A valve that owns a zone pushes its measure about once a minute,
	// and a member seen on air went back to its own sensor between pushes 3 to
	// 5 minutes apart.
	ownerPushEvery = ownerStep
)

// ownerSample is one temperature the controller read, for Tref.
type ownerSample struct {
	at time.Time
	c  float64
}

// zoneOwner is the controller of one owned zone. All belong to the engine
// goroutine.
type zoneOwner struct {
	integ    float64
	hist     []ownerSample
	tempC    float64
	source   string // "external" or "device"
	pct      int
	haveOut  bool
	evalAt   time.Time
	pushedAt time.Time
	lost     bool // no temperature at the last step, logged once
}

// update steps the controller with the setpoint sp and the temperature t, both
// in degrees, and returns the demand in percent.
func (x *zoneOwner) update(pid tado.ZonePID, spC, tC float64, now time.Time) int {
	errCenti := (spC - tC) * 100
	if pid.Ki > 0 {
		x.integ = min(max(x.integ+errCenti, 0), 3000000/float64(pid.Ki))
	} else {
		x.integ = 0
	}
	x.hist = append(x.hist, ownerSample{at: now, c: tC})
	// Keep the samples that may still serve as Tref: the newest at or older
	// than ownerTrefAge, and everything after it.
	cut := 0
	for i, s := range x.hist {
		if now.Sub(s.at) >= ownerTrefAge {
			cut = i
		}
	}
	x.hist = slices.Clone(x.hist[cut:])
	tref := x.hist[0].c
	out := float64(pid.Kp)*errCenti/2000 + float64(pid.Ki)*x.integ/30000 - float64(pid.Kd)*(tC-tref)*100/2000
	return int(math.Round(min(max(out, 0), 100)))
}

// reset zeroes the controller state, as the valve does when it reads the zone
// document.
func (x *zoneOwner) reset() {
	x.integ, x.hist = 0, nil
}

// tickZoneOwners steps the controller of every owned zone and logs the
// zone-plane requests the members addressed to us.
func (d *daemon) tickZoneOwners(e *tado.EmuState) {
	for _, r := range e.TakeOwnerRequests() {
		body := ""
		if r.Body != "" {
			body = " " + r.Body
		}
		log.Printf("zone %d owner: %s asked %s %s observe=%t opts=[%s]%s", r.Zone, r.Device, r.Method, r.URI, r.Observe, r.Options, body)
		if r.Registered {
			path, _, _ := strings.Cut(r.URI, "?")
			log.Printf("zone %d owner: %s observes %s tok=%s", r.Zone, r.Device, path, r.Token)
		}
	}
	inst := d.inst.Load()
	if inst == nil {
		return
	}
	defer d.saveZoneObservers(e, inst)
	d.dropZoneOwners(e, inst)
	now := d.retryNow()
	for i := range inst.Zones {
		z := &inst.Zones[i]
		if z.Kind != tado.ZoneHeating || !inst.Options.CoordinatorOwns(z.Number) {
			continue
		}
		x := d.zoneOwners[z.Number]
		if x == nil {
			x = &zoneOwner{}
			d.zoneOwners[z.Number] = x
		}
		d.stepZoneOwner(e, inst, z, x, now)
	}
}

// dropZoneOwners forgets the controller of the zones the coordinator no longer
// owns and stops serving their measure.
func (d *daemon) dropZoneOwners(e *tado.EmuState, inst *tado.Install) {
	for zone := range d.zoneOwners {
		if z, err := inst.ZoneByNumber(zone); err == nil && z.Kind == tado.ZoneHeating && inst.Options.CoordinatorOwns(zone) {
			continue
		}
		delete(d.zoneOwners, zone)
		e.ClearZoneMeasure(zone)
	}
}

// ownerTempC is the temperature the controller of zone reads: the external
// reading while it is fresh, else the calibrated temperature of the measuring
// device.
func (d *daemon) ownerTempC(e *tado.EmuState, inst *tado.Install, zone int, now time.Time) (float64, string, bool) {
	if c, ok := d.freshRoomTemp(inst, zone); ok {
		return c, "external", true
	}
	if c, ok := measuredC(e, inst, zone, now); ok {
		return c, "device", true
	}
	return 0, "", false
}

// measuredC is the calibrated temperature 012d of the measuring device of
// zone, while that device sends frames.
func measuredC(e *tado.EmuState, inst *tado.Install, zone int, now time.Time) (float64, bool) {
	dev, err := inst.MeasuringDevice(zone)
	if err != nil {
		return 0, false
	}
	c, _, seen, ok := e.DeviceTemp(hex.EncodeToString(dev.WireEUI()))
	if !ok || now.Sub(seen) > inst.Options.OfflineAfter() {
		return 0, false
	}
	return c, true
}

// ownerHumidityDeci is the humidity the measure carries in tag 0135, in tenths
// of a percent: the last reading of the measuring device, 0 when it has none.
// Every captured measure carries the tag; the wall thermostat took no measure
// without it on the plant.
func (d *daemon) ownerHumidityDeci(e *tado.EmuState, inst *tado.Install, zone int) int {
	dev, err := inst.MeasuringDevice(zone)
	if err != nil {
		return 0
	}
	h, _ := e.DeviceHumidityDeci(hex.EncodeToString(dev.WireEUI()))
	return h
}

// stepZoneOwner runs one step of the controller of zone when a step is due,
// serves the measure and pushes it to the members when it changed or
// ownerPushEvery passed.
func (d *daemon) stepZoneOwner(e *tado.EmuState, inst *tado.Install, z *tado.Zone, x *zoneOwner, now time.Time) {
	if !x.evalAt.IsZero() && now.Sub(x.evalAt) < ownerStep {
		return
	}
	x.evalAt = now
	tC, source, ok := d.ownerTempC(e, inst, z.Number, now)
	if !ok {
		if !x.lost {
			log.Printf("zone %d owner: no room temperature, the members get no measure", z.Number)
		}
		x.lost = true
		e.ClearZoneMeasure(z.Number)
		return
	}
	x.lost = false
	if x.haveOut && source != x.source {
		// The two sensors read the same room with a different error: a
		// derivative or an integral across the switch is a sensor step, not a
		// room change.
		x.reset()
	}
	pid := inst.Options.ZonePID(z.Number)
	pct := 0
	if spC, has := d.zoneTargetC(e, z); has {
		pct = x.update(pid, spC, tC, now)
	} else {
		x.reset()
	}
	changed := !x.haveOut || pct != x.pct || source != x.source
	x.tempC, x.source, x.pct, x.haveOut = tC, source, pct, true
	humidity := d.ownerHumidityDeci(e, inst, z.Number)
	e.SetZoneMeasure(z.Number, tado.ZoneMeasureBody(tC, pct, humidity))
	for _, o := range e.NotifyZoneObservers(z.Number) {
		d.requestQueuedDispatch(o.Target)
		log.Printf("zone %d owner: notify %s tok=%s obs=%d %.2f C demand %d%%", z.Number, o.Device, o.Token, o.Seq, tC, pct)
	}
	if !changed && now.Sub(x.pushedAt) < ownerPushEvery {
		return
	}
	members, err := inst.ZoneMembers(z.Number)
	if err != nil {
		log.Printf("zone %d owner: %v", z.Number, err)
		return
	}
	for _, m := range members {
		target := hex.EncodeToString(m.WireEUI())
		if err := e.EnqueueZoneMeasure(target, z.Number, tC, pct, humidity); err != nil {
			log.Printf("zone %d owner: measure to %s: %v", z.Number, m.Short(), err)
			continue
		}
		d.requestQueuedDispatch(target)
	}
	x.pushedAt = now
	if changed {
		log.Printf("zone %d owner: %.2f C (%s) demand %d%%", z.Number, tC, source, pct)
	}
}

// zoneOwnerView is the owner of a heating zone for GET /api/zones: the mode,
// and for an owned zone the temperature the controller read, its source, the
// demand, the last push and the observers of the measure.
func (d *daemon) zoneOwnerView(e *tado.EmuState, inst *tado.Install, zone int) map[string]any {
	out := map[string]any{"mode": inst.Options.ZoneOwner(zone)}
	x := d.zoneOwners[zone]
	if x == nil || !inst.Options.CoordinatorOwns(zone) {
		return out
	}
	out["tempC"], out["source"], out["demandPct"], out["pushedAt"] = nil, nil, nil, nil
	if x.haveOut {
		out["tempC"], out["source"], out["demandPct"] = x.tempC, x.source, x.pct
	}
	if !x.pushedAt.IsZero() {
		out["pushedAt"] = x.pushedAt.UTC().Format(time.RFC3339)
	}
	observers := []map[string]any{}
	for _, o := range e.ZoneObservers(zone) {
		observers = append(observers, map[string]any{
			"device": o.Device, "token": o.Token, "seq": o.Seq,
			"notifiedAt": o.NotifiedAt.UTC().Format(time.RFC3339),
		})
	}
	out["observers"] = observers
	return out
}

// storedZoneObserver is the JSON of one observe registration in meta
// zone_observers. The measure last sent is not stored: the restored
// registration notifies at the first step, as after a change of the measure.
type storedZoneObserver struct {
	Zone       int    `json:"zone"`
	Target     string `json:"target"` // wire EUI hex of the requester
	Token      string `json:"token"`  // hex
	Seq        uint32 `json:"seq"`
	NotifiedMs int64  `json:"notifiedMs"`
}

// zoneObserverRows lists the observe registrations of every heating zone, in
// zone order.
func zoneObserverRows(e *tado.EmuState, inst *tado.Install) []storedZoneObserver {
	out := []storedZoneObserver{}
	zones := make([]int, 0, len(inst.Zones))
	for i := range inst.Zones {
		if inst.Zones[i].Kind == tado.ZoneHeating {
			zones = append(zones, inst.Zones[i].Number)
		}
	}
	slices.Sort(zones)
	for _, zone := range zones {
		for _, o := range e.ZoneObservers(zone) {
			out = append(out, storedZoneObserver{
				Zone: zone, Target: o.Target, Token: o.Token, Seq: o.Seq, NotifiedMs: o.NotifiedAt.UnixMilli(),
			})
		}
	}
	return out
}

// saveZoneObservers stores the observe registrations once they moved: a new
// registration, a notification (its sequence must keep rising across a
// restart) or a registration dropped. A failed save stays owed to the next
// tick.
func (d *daemon) saveZoneObservers(e *tado.EmuState, inst *tado.Install) {
	if d.state == nil {
		return
	}
	rows := zoneObserverRows(e, inst)
	if d.zoneObsSaved != nil && slices.Equal(rows, d.zoneObsSaved) {
		return
	}
	if err := d.state.SetZoneObservers(context.Background(), rows); err != nil {
		log.Printf("persist the zone observers: %v", err)
		return
	}
	d.zoneObsSaved = rows
}

// restoreZoneObservers loads the stored observe registrations before the
// engine starts. A registration of a zone the coordinator no longer owns, or
// of a requester that left the zone, is dropped.
func (d *daemon) restoreZoneObservers(e *tado.EmuState) error {
	if d.state == nil {
		return nil
	}
	stored, err := d.state.ZoneObservers(context.Background())
	if err != nil {
		return err
	}
	for _, s := range stored {
		requester, err1 := hex.DecodeString(s.Target)
		token, err2 := hex.DecodeString(s.Token)
		if err1 != nil || err2 != nil {
			log.Printf("zone %d owner: stored observer %s tok=%s is not hex, dropped", s.Zone, s.Target, s.Token)
			continue
		}
		if e.RestoreZoneObserver(s.Zone, requester, token, s.Seq, time.UnixMilli(s.NotifiedMs).UTC()) {
			log.Printf("zone %d owner: %s observes the measure tok=%s obs=%d, restored", s.Zone, tado.ShortFromWire(requester), s.Token, s.Seq)
		}
	}
	return nil
}
