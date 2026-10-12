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
	rows, active, programs, err := d.state.PresetState(ctx)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		if _, err := d.migrateDefaultPreset(ctx, e); err != nil {
			return err
		}
		if rows, active, programs, err = d.state.PresetState(ctx); err != nil {
			return err
		}
	}
	d.presets = make(map[int64]*presetRecord, len(rows))
	d.presetOrder = d.presetOrder[:0]
	for _, row := range rows {
		rec := &presetRecord{id: row.id, name: row.name, programs: programs[row.id]}
		if rec.programs == nil {
			rec.programs = map[int]storedZonePrograms{}
		}
		d.presets[row.id] = rec
		d.presetOrder = append(d.presetOrder, row.id)
		d.nextPresetID = max(d.nextPresetID, row.id+1)
	}
	if _, ok := d.presets[active]; !ok {
		// A database can lose its active id only behind the back of the daemon:
		// the active preset cannot be deleted through the API. Start on the
		// first preset instead of on none.
		log.Printf("the stored active preset %d is gone, %q takes over", active, d.presets[d.presetOrder[0]].name)
		active = d.presetOrder[0]
		if err := d.state.SetActivePreset(ctx, active); err != nil {
			return err
		}
	}
	d.activePreset = active
	return nil
}

// migrateDefaultPreset creates the Default preset on the first start with the
// program store and copies the programs the coordinator already holds into it
// as home programs: each heating zone from the served sched document of the
// first member that holds one, the hot-water zone from its daemon-side store.
// It writes only preset rows: the served documents stay as they are, so a
// restart after the migration serves the same documents and ETags as before.
func (d *daemon) migrateDefaultPreset(ctx context.Context, e *tado.EmuState) (int64, error) {
	id, err := d.state.CreatePreset(ctx, defaultPresetName, 0)
	if err != nil {
		return 0, err
	}
	if inst := d.inst.Load(); inst != nil {
		for i := range inst.Zones {
			z := &inst.Zones[i]
			if z.Kind == tado.ZoneHotWater {
				programs, err := d.state.HotWaterProgram(ctx)
				if err != nil {
					return 0, err
				}
				if programs == nil {
					continue
				}
				if err := d.state.SetZoneProgram(ctx, id, z.Number, false, programs); err != nil {
					return 0, err
				}
				log.Printf("preset %q: the hot-water program of zone %d moved in from the daemon store", defaultPresetName, z.Number)
				continue
			}
			members, err := inst.ZoneMembers(z.Number)
			if err != nil {
				continue
			}
			for _, member := range members {
				programs, set, err := e.Schedule(hex.EncodeToString(member.WireEUI()))
				if err != nil {
					return 0, err
				}
				if !set {
					continue
				}
				if err := d.state.SetZoneProgram(ctx, id, z.Number, false, programs); err != nil {
					return 0, err
				}
				log.Printf("preset %q: the home program of zone %d moved in from the sched document of %s", defaultPresetName, z.Number, member.Serial)
				break
			}
		}
	}
	if err := d.state.SetActivePreset(ctx, id); err != nil {
		return 0, err
	}
	log.Printf("program presets: migrated the current programs into the preset %q and made it active", defaultPresetName)
	return id, nil
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
