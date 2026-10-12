package daemon

// api_schedule: the weekly program of a zone, in the two modes of a program
// preset: "home" (the default) is what the zone follows at home, and what the
// heating members of the active preset are served as their sched document;
// "away" is what the zone follows when the whole home is away, and a zone
// without one keeps the fixed away temperature or the off of the away
// fallback. The query of every route names the mode (mode=home|away, home by
// default) and the preset (preset=<id>, the active one by default). PUT
// replaces the whole program of a mode; DELETE removes it: the away program
// returns the zone to the away fallback, the home program leaves the zone
// without a program, whose members read 2.03 and keep the program they run, as
// a zone that never had one. The hot-water zone gets the same two modes,
// daemon-side only: its program is never served on sched, because the
// thermostat's offline engine would run it on its heating zone.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"freedo/tado"
)

// dayNames maps the day-mask bits, bit 0 first.
var dayNames = [7]string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

// dayMaskBit is the day-mask bit of t. Bit 0 is Monday.
func dayMaskBit(t time.Time) byte {
	return 1 << ((int(t.Weekday()) + 6) % 7)
}

// scheduledBlock is what the program entry in force holds: the setpoint for
// 6200 and entry field 3, which a disabled block writes as 61e0=0.
type scheduledBlock struct {
	c   float64
	off bool
}

// programLocation is the wall clock the weekly programs run on: the device
// clock, UTC plus the 02b2 the coordinator serves. options.utc_offset_min fixes
// it, else options.timezone, else the host's zone. The device offline engine
// runs the same program on that clock, so the two agree on every block.
func programLocation(o tado.Options) *time.Location {
	if o.UTCOffsetMin != nil {
		return time.FixedZone("", *o.UTCOffsetMin*60)
	}
	loc, _ := o.Location()
	return loc
}

// programClock is programLocation of inst, resolved once per installation: the
// engine evaluates the programs every tick. It belongs to the engine goroutine.
func (d *daemon) programClock(inst *tado.Install) *time.Location {
	if d.progLocOf != inst {
		d.progLocOf, d.progLoc = inst, programLocation(inst.Options)
	}
	return d.progLoc
}

// scheduledBlockAt returns the block the weekly programs hold at t on the wall
// clock of loc, and whether one is in force. The entry in force is the last one
// whose minute has passed.
func scheduledBlockAt(programs []tado.ScheduleProgram, t time.Time, loc *time.Location) (scheduledBlock, bool) {
	t = t.In(loc)
	bit := dayMaskBit(t)
	minute := t.Hour()*60 + t.Minute()
	for _, program := range programs {
		if program.DayMask&bit == 0 {
			continue
		}
		found := false
		var entry tado.ScheduleEntry
		for _, e := range program.Entries {
			if e.Minute > minute {
				break
			}
			entry, found = e, true
		}
		if !found {
			return scheduledBlock{}, false
		}
		return scheduledBlock{c: float64(entry.SetpointCenti) / 100, off: !entry.Enabled}, true
	}
	return scheduledBlock{}, false
}

// nextScheduleChange returns the instant the weekly programs move to their next
// entry on the wall clock of loc. A day whose entries are all in the past
// changes at the next midnight. An entry minute is a wall-clock time, not an
// offset from midnight: on a DST day the two differ by an hour.
func nextScheduleChange(programs []tado.ScheduleProgram, t time.Time, loc *time.Location) (time.Time, bool) {
	t = t.In(loc)
	bit := dayMaskBit(t)
	minute := t.Hour()*60 + t.Minute()
	for _, program := range programs {
		if program.DayMask&bit == 0 {
			continue
		}
		for _, e := range program.Entries {
			if e.Minute > minute {
				return time.Date(t.Year(), t.Month(), t.Day(), e.Minute/60, e.Minute%60, 0, 0, loc), true
			}
		}
		return time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc), true
	}
	return time.Time{}, false
}

type schedulePlanBody struct {
	Programs []scheduleProgramBody `json:"programs"`
}

// scheduleProgramBody takes the day selection as a day-name list or as the
// 7-bit mask itself.
type scheduleProgramBody struct {
	Days    json.RawMessage     `json:"days"`
	Entries []schedulePointBody `json:"entries"`
}

type schedulePointBody struct {
	Minute    *int     `json:"minute"`
	SetpointC *float64 `json:"setpoint_c"`
	Enabled   *bool    `json:"enabled"`
}

type scheduleView struct {
	Zone     int                   `json:"zone"`
	Members  []string              `json:"members"`
	Programs []scheduleProgramView `json:"programs"`
	Mode     string                `json:"mode"`
	Preset   int64                 `json:"preset"`
}

type scheduleProgramView struct {
	Days    []string            `json:"days"`
	Mask    int                 `json:"mask"`
	Entries []schedulePointView `json:"entries"`
}

type schedulePointView struct {
	Minute    int     `json:"minute"`
	SetpointC float64 `json:"setpoint_c"`
	Enabled   bool    `json:"enabled"`
}

// putZoneSchedule stores the program of the named mode and preset of the zone
// (setZoneProgram). On the home mode of the active preset the document also
// fans out to the sched of every heating member; every other program stays
// daemon-side: no device pulls an away program or the program of a preset
// that is not active, and the hot-water program is never served. The answer
// carries the stored programs; a rejected rule is a 400 naming it.
func (d *daemon) putZoneSchedule(w http.ResponseWriter, r *http.Request) {
	zone, members, ok := d.pathZoneMembers(w, r)
	if !ok {
		return
	}
	away, mode, err := scheduleMode(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var b schedulePlanBody
	if !decodeBody(w, r, &b) {
		return
	}
	programs, err := decodeSchedulePrograms(b.Programs)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	hotWater := d.isHotWaterZone(zone)
	if hotWater {
		if err := validateHotWaterSchedule(programs); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	} else {
		if err := tado.ValidateSchedule(programs); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := validateScheduleRange(programs, zoneMinC, zoneMaxC, ""); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		d.ensurePresets()
		preset, err := d.resolvePresetParam(r)
		if err != nil {
			return nil, err
		}
		if err := d.setZoneProgram(e, preset, zone, away, programs, !hotWater); err != nil {
			return nil, err
		}
		return newScheduleView(zone, members, programs, mode, preset), nil
	})
	if writeAPIErr(w, d.persist(err)) {
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// getZoneSchedule returns the program of the named mode and preset of the
// zone from the program store, or 404 when the zone holds none. The served
// documents of the heating members derive from the home programs of the
// active preset, so the store is what the members are served.
func (d *daemon) getZoneSchedule(w http.ResponseWriter, r *http.Request) {
	zone, members, ok := d.pathZoneMembers(w, r)
	if !ok {
		return
	}
	away, mode, err := scheduleMode(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		d.ensurePresets()
		preset, err := d.resolvePresetParam(r)
		if err != nil {
			return nil, err
		}
		zp := d.presetZonePrograms(preset, zone)
		programs := zp.home
		if away {
			programs = zp.away
		}
		if programs == nil {
			return nil, zoneScheduleErr(zone, away)
		}
		return newScheduleView(zone, members, programs, mode, preset), nil
	})
	if writeAPIErr(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// deleteZoneSchedule removes the program of the named mode and preset of the
// zone. Deleting the away program returns the zone to the away fallback.
// Deleting the home program of the active preset leaves the zone without a
// program: the daemon stops writing its block changes, and the served sched
// document of its members is dropped, so their next fetch gets the 2.03 of a
// zone that never had a program and they keep the program they run.
func (d *daemon) deleteZoneSchedule(w http.ResponseWriter, r *http.Request) {
	zone, members, ok := d.pathZoneMembers(w, r)
	if !ok {
		return
	}
	away, mode, err := scheduleMode(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	hotWater := d.isHotWaterZone(zone)
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		d.ensurePresets()
		preset, err := d.resolvePresetParam(r)
		if err != nil {
			return nil, err
		}
		zp := d.presetZonePrograms(preset, zone)
		if (away && zp.away == nil) || (!away && zp.home == nil) {
			return nil, zoneScheduleErr(zone, away)
		}
		if d.state != nil {
			if err := d.state.DeleteZoneProgram(context.Background(), preset, zone, away); err != nil {
				return nil, &stateSaveError{err: err}
			}
		}
		d.dropZoneProgram(preset, zone, away)
		if !away && !hotWater && preset == d.activePreset {
			for _, member := range members {
				target := hex.EncodeToString(member.WireEUI())
				dropped, err := e.DropSchedule(target)
				if err != nil {
					return nil, err
				}
				if dropped {
					log.Printf("dropped the sched document of %s: zone %d has no home program", member.Serial, zone)
				}
			}
		}
		return newScheduleView(zone, members, nil, mode, preset), nil
	})
	if writeAPIErr(w, d.persist(err)) {
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// setZoneProgram stores the program of the named mode of zone in preset, and
// on the home mode of the active preset of a zone whose members are served
// (served) fans the document out to every member. The program goes to the
// program store before the engine learns it and before any member is served
// it: a failed save changes nothing. The caller checks the program with the
// rules the fan-out applies, so the fan-out cannot refuse a stored program.
func (d *daemon) setZoneProgram(e *tado.EmuState, preset int64, zone int, away bool, programs []tado.ScheduleProgram, served bool) error {
	if d.state != nil {
		if err := d.state.SetZoneProgram(context.Background(), preset, zone, away, programs); err != nil {
			return &stateSaveError{err: err}
		}
	}
	d.storeZoneProgram(preset, zone, away, programs)
	if away || !served || preset != d.activePreset {
		return nil
	}
	members, err := d.inst.Load().ZoneMembers(zone)
	if err != nil {
		return err
	}
	for _, member := range members {
		target := hex.EncodeToString(member.WireEUI())
		if err := e.SetSchedule(target, programs); err != nil {
			return err
		}
		d.dispatchQueued(e, target)
	}
	return nil
}

// scheduleMode parses ?mode= of r, home by default.
func scheduleMode(r *http.Request) (away bool, mode string, err error) {
	switch mode = r.URL.Query().Get("mode"); mode {
	case "", "home":
		return false, "home", nil
	case "away":
		return true, "away", nil
	default:
		return false, "", fmt.Errorf("mode %q is unknown: want home or away", mode)
	}
}

// resolvePresetParam reads preset=<id> from the query of r, the active preset
// when absent. It runs on the engine goroutine, which owns the registry.
func (d *daemon) resolvePresetParam(r *http.Request) (int64, error) {
	raw := r.URL.Query().Get("preset")
	if raw == "" {
		return d.activePreset, nil
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 1 {
		return 0, apiErrf(http.StatusBadRequest, "preset %q is not a preset id", raw)
	}
	if d.presets[id] == nil {
		return 0, apiErrf(http.StatusNotFound, "preset %d is not declared", id)
	}
	return id, nil
}

// zoneScheduleErr is the 404 of a mode the zone holds no program for.
func zoneScheduleErr(zone int, away bool) error {
	if away {
		return apiErrf(http.StatusNotFound, "zone %d has no away program", zone)
	}
	return apiErrf(http.StatusNotFound, "zone %d has no schedule", zone)
}

// isHotWaterZone reports whether zone is the hot-water zone of the installation.
func (d *daemon) isHotWaterZone(zone int) bool {
	inst := d.inst.Load()
	if inst == nil {
		return false
	}
	z, ok := inst.HotWaterZone()
	return ok && z.Number == zone
}

// purgeHotWaterSchedules drops at the start the hot-water program an earlier
// version stored as the thermostat's sched document, logs each drop and saves
// the desired state without it.
func (d *daemon) purgeHotWaterSchedules(e *tado.EmuState) error {
	dropped := e.DropHotWaterScheduleLeftovers()
	if len(dropped) == 0 {
		return nil
	}
	for _, serial := range dropped {
		log.Printf("dropped the sched document of %s: a hot-water program the daemon now runs itself", serial)
	}
	if d.state == nil {
		return nil
	}
	d.saveSeq++
	return d.saveDesiredStateSnapshot(context.Background(), e.ExportDesiredState(), d.saveSeq)
}

// validateHotWaterSchedule applies the week rules of a served document and the
// setpoint range the thermostat clamps hot water to.
func validateHotWaterSchedule(programs []tado.ScheduleProgram) error {
	if err := tado.ValidateSchedule(programs); err != nil {
		return err
	}
	return validateScheduleRange(programs, tado.DHWMinC, tado.DHWMaxC, " for hot water")
}

// validateScheduleRange keeps every entry within lo..hi C. A disabled entry
// carries its setpoint on the wire too, so it keeps to the same range.
func validateScheduleRange(programs []tado.ScheduleProgram, lo, hi float64, what string) error {
	for i, program := range programs {
		for j, entry := range program.Entries {
			c := float64(entry.SetpointCenti) / 100
			if c < lo || c > hi {
				return fmt.Errorf("programs[%d].entries[%d].setpoint_c must be within %g..%g C%s", i, j, lo, hi, what)
			}
		}
	}
	return nil
}

// pathZoneMembers resolves the zone number of the path to its members.
func (d *daemon) pathZoneMembers(w http.ResponseWriter, r *http.Request) (int, []*tado.Device, bool) {
	inst := d.inst.Load()
	if inst == nil {
		writeResolveErr(w, errNoInstall)
		return 0, nil, false
	}
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid zone number")
		return 0, nil, false
	}
	members, err := inst.ZoneMembers(number)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return 0, nil, false
	}
	if len(members) == 0 {
		writeErr(w, http.StatusBadRequest, "zone "+strconv.Itoa(number)+" declares no member")
		return 0, nil, false
	}
	return number, members, true
}

func decodeSchedulePrograms(in []scheduleProgramBody) ([]tado.ScheduleProgram, error) {
	if len(in) == 0 {
		return nil, errors.New("programs is required")
	}
	out := make([]tado.ScheduleProgram, 0, len(in))
	for i, program := range in {
		mask, err := decodeScheduleDays(program.Days)
		if err != nil {
			return nil, fmt.Errorf("programs[%d].days: %w", i, err)
		}
		entries := make([]tado.ScheduleEntry, 0, len(program.Entries))
		for j, point := range program.Entries {
			if point.Minute == nil || point.SetpointC == nil {
				return nil, fmt.Errorf("programs[%d].entries[%d] requires minute and setpoint_c", i, j)
			}
			centi, err := centiDegrees(*point.SetpointC)
			if err != nil {
				return nil, fmt.Errorf("programs[%d].entries[%d].setpoint_c must have at most two decimal places", i, j)
			}
			enabled := true
			if point.Enabled != nil {
				enabled = *point.Enabled
			}
			entries = append(entries, tado.ScheduleEntry{Minute: *point.Minute, SetpointCenti: centi, Enabled: enabled})
		}
		out = append(out, tado.ScheduleProgram{DayMask: mask, Entries: entries})
	}
	return out, nil
}

func decodeScheduleDays(raw json.RawMessage) (byte, error) {
	invalid := errors.New("a day-name list or a 7-bit mask is required")
	if len(raw) == 0 {
		return 0, invalid
	}
	var names []string
	if err := json.Unmarshal(raw, &names); err == nil {
		var mask byte
		for _, name := range names {
			bit, ok := dayBit(name)
			if !ok {
				return 0, fmt.Errorf("%q is not a day name", name)
			}
			mask |= bit
		}
		return mask, nil
	}
	var mask int
	if err := json.Unmarshal(raw, &mask); err != nil || mask < 0 || mask > 0x7f {
		return 0, invalid
	}
	return byte(mask), nil
}

func dayBit(name string) (byte, bool) {
	want := strings.ToLower(strings.TrimSpace(name))
	for i, day := range dayNames {
		if day == want {
			return 1 << i, true
		}
	}
	return 0, false
}

func newScheduleView(zone int, members []*tado.Device, programs []tado.ScheduleProgram, mode string, preset int64) scheduleView {
	out := scheduleView{
		Zone:     zone,
		Members:  make([]string, 0, len(members)),
		Programs: make([]scheduleProgramView, 0, len(programs)),
		Mode:     mode,
		Preset:   preset,
	}
	for _, member := range members {
		out.Members = append(out.Members, member.Short())
	}
	out.Programs = append(out.Programs, programViews(programs)...)
	return out
}

// programViews renders programs for the API. A nil list stays nil, so a mode
// without a program reads null.
func programViews(programs []tado.ScheduleProgram) []scheduleProgramView {
	if programs == nil {
		return nil
	}
	out := make([]scheduleProgramView, 0, len(programs))
	for _, program := range programs {
		view := scheduleProgramView{
			Days:    daysOf(program.DayMask),
			Mask:    int(program.DayMask),
			Entries: make([]schedulePointView, 0, len(program.Entries)),
		}
		for _, entry := range program.Entries {
			view.Entries = append(view.Entries, schedulePointView{
				Minute:    entry.Minute,
				SetpointC: float64(entry.SetpointCenti) / 100,
				Enabled:   entry.Enabled,
			})
		}
		out = append(out, view)
	}
	return out
}

func daysOf(mask byte) []string {
	out := make([]string, 0, len(dayNames))
	for i, day := range dayNames {
		if mask&(1<<i) != 0 {
			out = append(out, day)
		}
	}
	return out
}
