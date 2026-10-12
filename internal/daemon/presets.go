package daemon

// presets: the program store of the daemon. A preset holds the home program
// and the optional away program of every zone, the hot-water zone included.
// Exactly one preset is active: its home programs are the sched documents the
// heating members are served and what every zone follows at home, and its away
// programs what a zone with one follows in away. A schedule write without
// preset=<id> names the active preset. The registry lives in memory on the
// engine goroutine and behind it in the preset and zone_program tables: every
// write lands in the database first, so a failed save changes nothing.

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"

	"freedo/tado"
)

// defaultPresetName is the preset the first start with the program store
// builds out of the programs the coordinator already holds.
const defaultPresetName = "Default"

// presetRecord is one program preset: its id, its name and the programs of
// every zone.
type presetRecord struct {
	id       int64
	name     string
	programs map[int]storedZonePrograms
}

// ensurePresets seeds the in-memory Default preset of a storeless daemon: a
// stored one holds it from restorePresets.
func (d *daemon) ensurePresets() {
	if len(d.presets) > 0 {
		return
	}
	d.presets = map[int64]*presetRecord{
		1: {id: 1, name: defaultPresetName, programs: map[int]storedZonePrograms{}},
	}
	d.presetOrder = []int64{1}
	d.activePreset = 1
	d.nextPresetID = 2
}

// restorePresets loads the presets and the active one. A database that holds
// none gets the Default preset of migrateDefaultPreset. It runs at start,
// after the desired state reached the emulator, whose served documents are
// what the migration copies.
func (d *daemon) restorePresets(ctx context.Context, e *tado.EmuState) error {
	if d.state == nil {
		return nil
	}
	st, err := d.state.PresetState(ctx)
	if err != nil {
		return err
	}
	if len(st.rows) == 0 {
		if err := d.migrateDefaultPreset(ctx, e); err != nil {
			return err
		}
		if st, err = d.state.PresetState(ctx); err != nil {
			return err
		}
		if len(st.rows) == 0 {
			return errors.New("program presets: the migration stored no preset")
		}
	}
	d.presets = make(map[int64]*presetRecord, len(st.rows))
	d.presetOrder = d.presetOrder[:0]
	for _, row := range st.rows {
		rec := &presetRecord{id: row.id, name: row.name, programs: st.programs[row.id]}
		if rec.programs == nil {
			rec.programs = map[int]storedZonePrograms{}
		}
		d.presets[row.id] = rec
		d.presetOrder = append(d.presetOrder, row.id)
		d.nextPresetID = max(d.nextPresetID, row.id+1)
	}
	active := st.active
	if _, ok := d.presets[active]; !ok {
		// The migration stores the active id with the first preset, and the API
		// cannot delete the active preset: only a database edited or damaged
		// behind the back of the daemon gets here. The first preset takes over,
		// so the daemon runs on a known preset.
		first := d.presets[d.presetOrder[0]]
		if st.activeFound {
			log.Printf("WARNING: program presets: inconsistent database: the stored active preset %d does not exist, %q (id %d) takes over",
				active, first.name, first.id)
		} else {
			log.Printf("WARNING: program presets: inconsistent database: %d presets and no stored active preset, %q (id %d) takes over",
				len(d.presets), first.name, first.id)
		}
		active = first.id
		if err := d.state.SetActivePreset(ctx, active); err != nil {
			return err
		}
	}
	d.activePreset = active
	d.adoptHotWaterProgram(ctx)
	return nil
}

// adoptHotWaterProgram moves the hot-water program a daemon before the program
// presets kept in meta into every preset that holds no home program for the
// hot-water zone. It runs at start and at each new installation, so a program
// stored while the installation declared no hot-water zone moves in once one
// is declared. A program that does not decode stays in meta, logged.
func (d *daemon) adoptHotWaterProgram(ctx context.Context) {
	inst := d.inst.Load()
	if d.state == nil || inst == nil || len(d.presets) == 0 {
		return
	}
	z, ok := inst.HotWaterZone()
	if !ok {
		return
	}
	programs, ids, err := d.state.AdoptHotWaterProgram(ctx, z.Number)
	if err != nil {
		log.Printf("the hot-water program of the daemon store stays out of the presets: %v", err)
		return
	}
	for _, id := range ids {
		if rec := d.presets[id]; rec != nil {
			zp := rec.programs[z.Number]
			zp.home = programs
			rec.programs[z.Number] = zp
		}
	}
	if programs != nil {
		log.Printf("the hot-water program of zone %d moved in from the daemon store into %d presets", z.Number, len(ids))
	}
}

// migrateDefaultPreset creates the Default preset on the first start with the
// program store and copies the programs the coordinator already holds into it
// as home programs: each heating zone from the served sched document of the
// first member whose document decodes and keeps the week rules. The hot-water
// program follows in adoptHotWaterProgram. It reads everything first and then writes
// in one transaction, so a failed or interrupted start leaves no preset and
// the next start migrates again. It writes only preset rows: the served
// documents stay as they are, so a restart after the migration serves the same
// documents and ETags as before.
func (d *daemon) migrateDefaultPreset(ctx context.Context, e *tado.EmuState) error {
	home := make(map[int][]tado.ScheduleProgram)
	var moved []string
	if inst := d.inst.Load(); inst != nil {
		for i := range inst.Zones {
			z := &inst.Zones[i]
			if z.Kind != tado.ZoneHeating {
				continue
			}
			members, err := inst.ZoneMembers(z.Number)
			if err != nil {
				continue
			}
			for _, member := range members {
				programs, set, err := e.Schedule(hex.EncodeToString(member.WireEUI()))
				if err == nil && set {
					err = tado.ValidateSchedule(programs)
				}
				if err != nil {
					log.Printf("preset %q: the sched document of %s stays out of zone %d: %v", defaultPresetName, member.Serial, z.Number, err)
					continue
				}
				if !set {
					continue
				}
				home[z.Number] = programs
				moved = append(moved, fmt.Sprintf("the home program of zone %d moved in from the sched document of %s", z.Number, member.Serial))
				break
			}
		}
	}
	if err := d.state.MigrateDefaultPreset(ctx, defaultPresetName, home); err != nil {
		return err
	}
	for _, line := range moved {
		log.Printf("preset %q: %s", defaultPresetName, line)
	}
	log.Printf("program presets: migrated the current programs into the preset %q and made it active", defaultPresetName)
	return nil
}

// zonePrograms returns the home program of zone in the active preset: what the
// zone follows at home and what the heating members are served.
func (d *daemon) zonePrograms(zone int) ([]tado.ScheduleProgram, bool) {
	if p := d.presets[d.activePreset]; p != nil {
		if zp := p.programs[zone]; zp.home != nil {
			return zp.home, true
		}
	}
	return nil, false
}

// awayProgramOf returns the away program of zone in the active preset, and
// false when the zone has none, which is the zone that keeps the fixed away
// temperature or the off of the away fallback.
func (d *daemon) awayProgramOf(zone int) ([]tado.ScheduleProgram, bool) {
	if p := d.presets[d.activePreset]; p != nil {
		if zp := p.programs[zone]; zp.away != nil {
			return zp.away, true
		}
	}
	return nil, false
}

// programInForce returns the program zone follows now: its away program while
// the home is away and the zone holds one, else its home program.
func (d *daemon) programInForce(zone int) ([]tado.ScheduleProgram, bool) {
	if d.away {
		if away, ok := d.awayProgramOf(zone); ok {
			return away, true
		}
	}
	return d.zonePrograms(zone)
}

// presetZonePrograms returns what preset holds for zone, home and away.
func (d *daemon) presetZonePrograms(preset int64, zone int) storedZonePrograms {
	if rec := d.presets[preset]; rec != nil {
		return rec.programs[zone]
	}
	return storedZonePrograms{}
}

// storeZoneProgram records a program the database already took.
func (d *daemon) storeZoneProgram(preset int64, zone int, away bool, programs []tado.ScheduleProgram) {
	rec := d.presets[preset]
	zp := rec.programs[zone]
	if away {
		zp.away = programs
	} else {
		zp.home = programs
	}
	rec.programs[zone] = zp
}

// dropZoneProgram forgets a program the database already dropped.
func (d *daemon) dropZoneProgram(preset int64, zone int, away bool) {
	rec := d.presets[preset]
	zp := rec.programs[zone]
	if away {
		zp.away = nil
	} else {
		zp.home = nil
	}
	rec.programs[zone] = zp
}

// presetNamed reports whether a preset holds name, without case.
func (d *daemon) presetNamed(name string) bool {
	for _, p := range d.presets {
		if strings.EqualFold(p.name, name) {
			return true
		}
	}
	return false
}

// serveHomePrograms fans the home program of the active preset out to the
// sched document of every zone member, and drops the document of a zone
// without one: its members then read 2.03 like a zone that never had a
// program, and keep the program they run. A document whose bytes do not move
// takes no fetch trigger, so serving an unchanged preset is silent.
func (d *daemon) serveHomePrograms(e *tado.EmuState) error {
	inst := d.inst.Load()
	if inst == nil {
		return nil
	}
	for i := range inst.Zones {
		z := &inst.Zones[i]
		if z.Kind != tado.ZoneHeating {
			continue
		}
		home, held := d.zonePrograms(z.Number)
		members, err := inst.ZoneMembers(z.Number)
		if err != nil {
			continue
		}
		for _, member := range members {
			target := hex.EncodeToString(member.WireEUI())
			if !held {
				dropped, err := e.DropSchedule(target)
				if err != nil {
					return err
				}
				if dropped {
					log.Printf("dropped the sched document of %s: zone %d has no home program in the active preset",
						member.Serial, z.Number)
				}
				continue
			}
			if err := e.SetSchedule(target, home); err != nil {
				return err
			}
			d.dispatchQueued(e, target)
		}
	}
	return nil
}
