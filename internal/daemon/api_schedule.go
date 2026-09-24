package daemon

// api_schedule: the weekly schedule of a zone. The device pulls the document
// from its own resource, so a write fans the same document out to every zone
// member and a read takes it back from the first member that holds one. The hot
// water program is the exception: the daemon keeps it and never serves it,
// because the thermostat's offline engine would run it on its heating zone.

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

// scheduledBlockAt returns the block the weekly programs hold at t, and whether
// one is in force. The entry in force is the last one whose minute has passed.
func scheduledBlockAt(programs []tado.ScheduleProgram, t time.Time) (scheduledBlock, bool) {
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
// entry. A day whose entries are all in the past changes at the next midnight.
func nextScheduleChange(programs []tado.ScheduleProgram, t time.Time) (time.Time, bool) {
	bit := dayMaskBit(t)
	minute := t.Hour()*60 + t.Minute()
	midnight := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	for _, program := range programs {
		if program.DayMask&bit == 0 {
			continue
		}
		for _, e := range program.Entries {
			if e.Minute > minute {
				return midnight.Add(time.Duration(e.Minute) * time.Minute), true
			}
		}
		return midnight.AddDate(0, 0, 1), true
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

// putZoneSchedule stores the weekly schedule of a zone. The answer carries the
// stored programs; a rejected rule is a 400 naming it.
func (d *daemon) putZoneSchedule(w http.ResponseWriter, r *http.Request) {
	zone, members, ok := d.pathZoneMembers(w, r)
	if !ok {
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
	if d.isHotWaterZone(zone) {
		d.putHotWaterSchedule(w, zone, members, programs)
		return
	}
	v, err := d.do(func(e *tado.EmuState) (any, error) {
		for _, member := range members {
			if err := e.SetSchedule(hex.EncodeToString(member.WireEUI()), programs); err != nil {
				return nil, err
			}
		}
		return newScheduleView(zone, members, programs), nil
	})
	if writeHandlerErr(w, d.persist(err)) {
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// getZoneSchedule returns the stored programs of a zone, or 404 when no member
// holds a document.
func (d *daemon) getZoneSchedule(w http.ResponseWriter, r *http.Request) {
	zone, members, ok := d.pathZoneMembers(w, r)
	if !ok {
		return
	}
	hotWater := d.isHotWaterZone(zone)
	v, err := d.do(func(e *tado.EmuState) (any, error) {
		if hotWater {
			if d.dhwProgram == nil {
				return nil, nil
			}
			return newScheduleView(zone, members, d.dhwProgram), nil
		}
		for _, member := range members {
			programs, set, err := e.Schedule(hex.EncodeToString(member.WireEUI()))
			if err != nil {
				return nil, err
			}
			if set {
				return newScheduleView(zone, members, programs), nil
			}
		}
		return nil, nil
	})
	if writeHandlerErr(w, err) {
		return
	}
	if v == nil {
		writeErr(w, http.StatusNotFound, "zone "+strconv.Itoa(zone)+" has no schedule")
		return
	}
	writeJSON(w, http.StatusOK, v)
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

// putHotWaterSchedule stores the hot-water program daemon-side. The engine
// evaluates it and writes each block change to the thermostat on the hot-water
// zone; nothing reaches a sched resource.
func (d *daemon) putHotWaterSchedule(w http.ResponseWriter, zone int, members []*tado.Device, programs []tado.ScheduleProgram) {
	if err := validateHotWaterSchedule(programs); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	v, err := d.do(func(e *tado.EmuState) (any, error) {
		d.dhwProgram = programs
		return newScheduleView(zone, members, programs), nil
	})
	if writeHandlerErr(w, err) {
		return
	}
	if d.state != nil {
		if err := d.state.SetHotWaterProgram(context.Background(), programs); err != nil {
			writeHandlerErr(w, &stateSaveError{err: err})
			return
		}
	}
	writeJSON(w, http.StatusOK, v)
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
// setpoint range the thermostat clamps hot water to. A disabled entry carries
// its setpoint in 6200 too, so it keeps to the same range.
func validateHotWaterSchedule(programs []tado.ScheduleProgram) error {
	if err := tado.ValidateSchedule(programs); err != nil {
		return err
	}
	for i, program := range programs {
		for j, entry := range program.Entries {
			c := float64(entry.SetpointCenti) / 100
			if c < tado.DHWMinC || c > tado.DHWMaxC {
				return fmt.Errorf("programs[%d].entries[%d].setpoint_c must be within %g..%g C for hot water",
					i, j, tado.DHWMinC, tado.DHWMaxC)
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

func newScheduleView(zone int, members []*tado.Device, programs []tado.ScheduleProgram) scheduleView {
	out := scheduleView{
		Zone:     zone,
		Members:  make([]string, 0, len(members)),
		Programs: make([]scheduleProgramView, 0, len(programs)),
	}
	for _, member := range members {
		out.Members = append(out.Members, member.Short())
	}
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
		out.Programs = append(out.Programs, view)
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
