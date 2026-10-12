package daemon

// room_temp: the room temperature an external sensor measures for a heating
// zone in offset mode setpoint, pushed over HTTP. The valve regulates on its
// own sensor and the daemon cannot change what it measures, so the daemon moves
// the setpoint it writes: the zone shift (zone_offset.go) becomes the external
// temperature minus the one the zone reports, and the zone reports the external
// temperature as its own while the reading is fresh. Mode config and dbg2 keep
// the correction in tag 0140 on the valve, so they take no reading. Mode config
// also makes a valve home its motor on each write. Mode dbg2 does not.
//
// It is a slow outer correction on the regulation of the valve:
//   - the wanted shift is that difference, within roomShiftMaxC of the manual
//     offset, smoothed by a first order filter of time constant roomShiftTau;
//   - the applied shift moves by one roomShiftStepC toward it, and only when
//     the two differ by more than roomShiftHystC;
//   - each move is a zone rewrite on air to battery devices, so two moves of
//     one zone are roomShiftMinInterval apart. The engine start opens the
//     interval, as it does for a flow limit.
//
// The applied shift replaces the manual offset while it holds. A reading older
// than options.room_temp_max_age_sec ends the correction: the wanted shift
// goes back to the manual offset through the same filter and the applied shift
// follows, one step at a time. The reading is not stored. The applied shift is,
// because the setpoints on the valves carry it.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"freedo/tado"
)

const (
	// roomTempMinC and roomTempMaxC bound a pushed room temperature to what a
	// room sensor measures in a heated home.
	roomTempMinC = -10
	roomTempMaxC = 50
	// roomTempSourceMaxLen bounds the label of the sensor that sends a reading,
	// in bytes.
	roomTempSourceMaxLen = 255
	// roomShiftMaxC bounds the correction around the manual offset. A valve
	// sensor next to a radiator reads a few degrees off the room, and a larger
	// gap is more likely a bad external value than a real one: it would send a
	// zone to the end of its setpoint range.
	roomShiftMaxC = 5.0
	// roomShiftTau is long against the heating cycle of a valve next to a
	// radiator, whose sensor swings by degrees over tens of minutes: a swing of
	// 2 C peak to peak that flips every 30 minutes leaves a ripple of 0.12 C
	// around the mean on the filter, inside roomShiftHystC. A correction of 2 C
	// is within a step after about four hours, and the release takes as long.
	roomShiftTau = 2 * time.Hour
	// roomShiftStepC is how far one move shifts the setpoint of a zone, above
	// the accuracy of the sensor of a valve.
	roomShiftStepC = 0.2
	// roomShiftHystC is below a step, so the applied shift ends within it of the
	// wanted one, and above the ripple the filter leaves, so the shift does not
	// move back and forth.
	roomShiftHystC = 0.15
	// roomShiftMinInterval bounds the zone rewrites to four an hour.
	roomShiftMinInterval = 15 * time.Minute
	// roomTempEvery paces the filter of each zone.
	roomTempEvery = time.Minute
)

var (
	errRoomTempZone = errors.New("only a heating zone takes an external room temperature")
	errRoomTempMode = errors.New("an external room temperature needs offset_mode setpoint or auto_offset: " +
		"the other modes keep the correction in tag 0140 on the valve, and mode config also makes a valve home its motor")
)

// roomTemp is the external room temperature of one zone and the correction it
// drives. at is zero without a reading. source labels the sensor of the reading
// for display, empty when the push named none. on says that shiftC, the shift applied
// to the zone, replaces the manual offset. wantedC is the filtered shift, evalAt
// the last filter step and wroteAt the last change of shiftC. All belong to the
// engine goroutine.
type roomTemp struct {
	tempC   float64
	source  string
	at      time.Time
	on      bool
	shiftC  float64
	wantedC float64
	evalAt  time.Time
	wroteAt time.Time
}

func (d *daemon) roomTempZone(number int) (*tado.Zone, error) {
	inst := d.inst.Load()
	if inst == nil {
		return nil, errNoInstall
	}
	z, err := inst.ZoneByNumber(number)
	if err != nil {
		return nil, err
	}
	if z.Kind != tado.ZoneHeating {
		return nil, fmt.Errorf("zone %d: %w", number, errRoomTempZone)
	}
	return z, nil
}

// roomTempFresh reports whether the reading of x still drives its zone.
func roomTempFresh(inst *tado.Install, x *roomTemp, now time.Time) bool {
	return !x.at.IsZero() && now.Sub(x.at) <= inst.Options.RoomTempMaxAge()
}

// freshRoomTemp returns the external room temperature of zone while it is fresh.
func (d *daemon) freshRoomTemp(inst *tado.Install, zone int) (float64, bool) {
	x := d.roomTemps[zone]
	if x == nil || !roomTempFresh(inst, x, d.retryNow()) {
		return 0, false
	}
	return x.tempC, true
}

// roomTempC is the room temperature of zone on the corrected scale: the
// external one while the learners read it, else the wire temperature c with the
// shift.
func (d *daemon) roomTempC(inst *tado.Install, zone int, c float64) float64 {
	if d.learnerReadsExternal(inst, zone) {
		t, _ := d.freshRoomTemp(inst, zone)
		return t
	}
	return d.correctedC(inst, zone, c)
}

// postZoneTemperature stores the external room temperature of a zone,
// {"tempC": number, "source": string}. source is optional and only labels the
// sensor in the view. The correction follows on the next steps of the filter.
func (d *daemon) postZoneTemperature(w http.ResponseWriter, r *http.Request) {
	zone, _, ok := d.pathZoneMembers(w, r)
	if !ok {
		return
	}
	var b struct {
		TempC  *float64 `json:"tempC"`
		Source string   `json:"source"`
	}
	if !decodeBody(w, r, &b) {
		return
	}
	if b.TempC == nil || !(*b.TempC >= roomTempMinC && *b.TempC <= roomTempMaxC) {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("tempC is required, within %d..%d", roomTempMinC, roomTempMaxC))
		return
	}
	if msg := checkRoomTempSource(b.Source); msg != "" {
		writeErr(w, http.StatusBadRequest, "source: "+msg)
		return
	}
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		z, err := d.roomTempZone(zone)
		if err != nil {
			return nil, err
		}
		inst := d.inst.Load()
		if inst.Options.ZoneOffset(z.Number).Mode != tado.OffsetModeSetpoint && !inst.Options.AutoOffset(z.Number).Enabled {
			return nil, fmt.Errorf("zone %d: %w", z.Number, errRoomTempMode)
		}
		now := d.retryNow()
		x := d.roomTemps[z.Number]
		if x == nil {
			manual := inst.Options.SetpointShiftC(z.Number)
			x = &roomTemp{shiftC: manual, wantedC: manual, evalAt: now}
			d.roomTemps[z.Number] = x
		}
		x.tempC, x.at, x.source = roundCenti(*b.TempC), now, b.Source
		return d.roomTempView(e, z.Number), nil
	})
	if errors.Is(err, errRoomTempMode) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if writeHandlerErr(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// checkRoomTempSource returns a problem with the source label of a reading, or
// "" when it is valid. An empty label is valid: the push names no sensor.
func checkRoomTempSource(source string) string {
	switch {
	case len(source) > roomTempSourceMaxLen:
		return fmt.Sprintf("longer than %d bytes", roomTempSourceMaxLen)
	case !utf8.ValidString(source):
		return "not valid UTF-8"
	case strings.ContainsFunc(source, unicode.IsControl):
		return "holds a control character"
	}
	return ""
}

// deleteZoneTemperature drops the external room temperature of a zone. The
// correction it holds is released as it is when the reading goes stale.
func (d *daemon) deleteZoneTemperature(w http.ResponseWriter, r *http.Request) {
	zone, _, ok := d.pathZoneMembers(w, r)
	if !ok {
		return
	}
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		z, err := d.roomTempZone(zone)
		if err != nil {
			return nil, err
		}
		if x := d.roomTemps[z.Number]; x != nil {
			x.at = time.Time{}
		}
		return d.roomTempView(e, z.Number), nil
	})
	if writeHandlerErr(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// tickRoomTemps steps the correction of every zone with a reading and stores
// the applied shifts that moved.
func (d *daemon) tickRoomTemps(e *tado.EmuState) {
	inst := d.inst.Load()
	if inst == nil {
		return
	}
	now := d.retryNow()
	zones := make([]int, 0, len(d.roomTemps))
	for zone := range d.roomTemps {
		zones = append(zones, zone)
	}
	slices.Sort(zones)
	for _, zone := range zones {
		x := d.roomTemps[zone]
		if x.at.IsZero() && !x.on {
			delete(d.roomTemps, zone)
			continue
		}
		d.stepRoomTemp(e, inst, zone, x, now)
	}
	d.saveRoomShifts()
}

// stepRoomTemp advances the filter of zone and, when the applied shift is more
// than roomShiftHystC from the wanted one and roomShiftMinInterval has passed
// since its last move, moves it by a step and restates the zone. With no fresh
// reading the shift ends on the manual offset.
func (d *daemon) stepRoomTemp(e *tado.EmuState, inst *tado.Install, zone int, x *roomTemp, now time.Time) {
	// Only mode setpoint shifts the setpoints. A reading in mode config or dbg2
	// with auto_offset on drives the learners alone.
	if inst.Options.ZoneOffset(zone).Mode != tado.OffsetModeSetpoint {
		return
	}
	manual := inst.Options.SetpointShiftC(zone)
	if !x.on {
		x.shiftC = manual
	}
	dt := now.Sub(x.evalAt)
	if dt < roomTempEvery {
		return
	}
	x.evalAt = now
	fresh := roomTempFresh(inst, x, now)
	var wanted float64
	switch {
	case fresh:
		wire, _, updated, ok := e.ZoneReport(zone)
		if !ok || now.Sub(updated) > inst.Options.OfflineAfter() {
			return
		}
		wanted = manual + min(max(x.tempC-wire-manual, -roomShiftMaxC), roomShiftMaxC)
	case x.on:
		if x.shiftC == manual {
			x.on, x.wantedC = false, manual
			d.roomUnsaved = true
			return
		}
		// The release never goes farther from the manual offset than the
		// applied shift is.
		x.wantedC = min(max(x.wantedC, min(x.shiftC, manual)), max(x.shiftC, manual))
		wanted = manual
	default:
		x.wantedC = manual
		return
	}
	x.wantedC += (wanted - x.wantedC) * (1 - math.Exp(-dt.Seconds()/roomShiftTau.Seconds()))

	next := x.shiftC
	switch {
	case !fresh && math.Abs(x.shiftC-manual) <= roomShiftStepC:
		next = manual
	case x.wantedC-x.shiftC > roomShiftHystC:
		next += roomShiftStepC
	case x.shiftC-x.wantedC > roomShiftHystC:
		next -= roomShiftStepC
	}
	next = roundCenti(next)
	if next == x.shiftC {
		return
	}
	last := x.wroteAt
	if last.IsZero() {
		last = d.flowEpoch
	}
	if now.Sub(last) < roomShiftMinInterval {
		return
	}
	delta := roundCenti(next - x.shiftC)
	prev := x.shiftC
	x.shiftC, x.wroteAt, x.on = next, now, fresh || next != manual
	d.roomUnsaved = true
	d.restateZoneShift(e, inst, zone, delta)
	log.Printf("zone %d room temperature %.2f C: shift %+.2f -> %+.2f C", zone, x.tempC, prev, next)
}

// dropRoomTemps forgets the zones the installation no longer lets take an
// external room temperature: removed, no longer heating, in mode config or dbg2
// without auto_offset, or no longer in mode setpoint. restateShiftedZones has
// restated them before.
func (d *daemon) dropRoomTemps(inst *tado.Install) {
	for zone, x := range d.roomTemps {
		z, err := inst.ZoneByNumber(zone)
		kept := err == nil && z.Kind == tado.ZoneHeating &&
			(inst.Options.ZoneOffset(zone).Mode == tado.OffsetModeSetpoint || inst.Options.AutoOffset(zone).Enabled)
		if kept {
			continue
		}
		delete(d.roomTemps, zone)
		d.roomUnsaved = d.roomUnsaved || x.on
	}
	d.saveRoomShifts()
}

// saveRoomShifts stores the applied shifts once they moved. A failed save stays
// owed to the next tick.
func (d *daemon) saveRoomShifts() {
	if !d.roomUnsaved {
		return
	}
	if d.state != nil {
		shifts := make(map[int]int, len(d.roomTemps))
		for zone, x := range d.roomTemps {
			if x.on {
				shifts[zone] = int(math.Round(x.shiftC * 100))
			}
		}
		if err := d.state.SetRoomShifts(context.Background(), shifts); err != nil {
			log.Printf("persist the room shifts: %v", err)
			return
		}
	}
	d.roomUnsaved = false
}

// restoreRoomShifts loads the stored shifts before the engine starts. The
// readings are not stored, so each shift is released from the start, unless a
// new reading comes first. A shift of a zone that cannot take one any more is
// dropped.
func (d *daemon) restoreRoomShifts() error {
	inst := d.inst.Load()
	if d.state == nil || inst == nil {
		return nil
	}
	stored, err := d.state.RoomShifts(context.Background())
	if err != nil {
		return err
	}
	for zone, centi := range stored {
		z, err := inst.ZoneByNumber(zone)
		if err != nil || z.Kind != tado.ZoneHeating || inst.Options.ZoneOffset(zone).Mode != tado.OffsetModeSetpoint {
			d.roomUnsaved = true
			continue
		}
		shift := float64(centi) / 100
		d.roomTemps[zone] = &roomTemp{on: true, shiftC: shift, wantedC: shift, evalAt: d.retryNow()}
	}
	return nil
}

// roomTempView renders the external room temperature of one heating zone.
// tempC, at, ageSec and source describe the last reading and stale says it is too old
// to drive the zone; inUse says that it does, and then the zone reports tempC as
// its temperature. learnInUse says that the early start, the plant episodes and
// the flow adaptation read tempC: the same as inUse in mode setpoint, and with
// auto_offset on in the modes config and dbg2. deviceC is the temperature the
// zone reports on the wire scale and wantedC the filtered shift. shiftC is the
// shift applied to the zone, and offsetC the manual one it returns to.
func (d *daemon) roomTempView(e *tado.EmuState, zone int) map[string]any {
	inst := d.inst.Load()
	view := map[string]any{
		"tempC": nil, "at": nil, "ageSec": nil, "source": nil, "stale": false, "inUse": false, "learnInUse": false,
		"deviceC": nil, "wantedC": nil,
		"shiftC": d.shiftC(inst, zone), "offsetC": inst.Options.SetpointShiftC(zone),
	}
	if deviceC, _, _, ok := e.ZoneReport(zone); ok {
		view["deviceC"] = deviceC
	}
	x := d.roomTemps[zone]
	if x == nil {
		return view
	}
	if !x.at.IsZero() {
		age := d.retryNow().Sub(x.at)
		fresh := roomTempFresh(inst, x, d.retryNow())
		view["tempC"], view["at"] = x.tempC, x.at.UTC().Format(time.RFC3339)
		if x.source != "" {
			view["source"] = x.source
		}
		view["ageSec"], view["stale"], view["inUse"] = int(age/time.Second), !fresh, fresh
	}
	view["learnInUse"] = d.learnerReadsExternal(inst, zone)
	if x.on || view["inUse"] == true {
		view["wantedC"] = roundCenti(x.wantedC)
	}
	return view
}
