package daemon

// early_start: the heat-up model of each heating zone and the early start of
// the program blocks it drives. No device firmware starts a block early: the
// commercial cloud did. The daemon learns how fast a zone warms from the
// episodes it observes after a target rise, and pushes the 6200 of a warmer
// block early enough for the zone to reach it when the block starts.

import (
	"cmp"
	"context"
	"log"
	"math"
	"net/http"
	"slices"
	"strconv"
	"time"

	"freedo/tado"
)

const (
	// thermalMinRiseC is the smallest gap between target and zone temperature
	// that opens an episode: under it the 0.1 C steps of tag 4060 dominate.
	thermalMinRiseC = 0.5
	// thermalMaxEpisode ends an episode that never reaches its target.
	thermalMaxEpisode = 6 * time.Hour
	// thermalMinRate and thermalMaxRate bound an accepted rate, in C per hour.
	thermalMinRate = 0.1
	thermalMaxRate = 10.0
)

// thermalEpisode is one accepted heat-up.
type thermalEpisode struct {
	EndedMs  int64    `json:"endedMs"`
	StartC   float64  `json:"startC"`
	TargetC  float64  `json:"targetC"`
	Minutes  float64  `json:"minutes"`
	Rate     float64  `json:"rateCPerHour"`
	OutdoorC *float64 `json:"outdoorC,omitempty"`
}

// thermalModel is the heat-up model of one zone: its last accepted episodes,
// oldest first.
type thermalModel struct {
	Episodes []thermalEpisode `json:"episodes"`
}

func (m *thermalModel) add(ep thermalEpisode) {
	m.Episodes = append(m.Episodes, ep)
	if n := len(m.Episodes) - tado.EarlyStartEpisodesKept; n > 0 {
		m.Episodes = slices.Clone(m.Episodes[n:])
	}
}

// rate is the median rate of the episodes. With an outdoor temperature and at
// least k episodes that recorded one, it is the median of the k episodes
// recorded nearest to it.
func (m *thermalModel) rate(outdoorC *float64, k int) (float64, bool) {
	episodes := m.Episodes
	if outdoorC != nil {
		var near []thermalEpisode
		for _, ep := range episodes {
			if ep.OutdoorC != nil {
				near = append(near, ep)
			}
		}
		if len(near) >= k {
			slices.SortStableFunc(near, func(a, b thermalEpisode) int {
				return cmp.Compare(math.Abs(*a.OutdoorC-*outdoorC), math.Abs(*b.OutdoorC-*outdoorC))
			})
			episodes = near[:k]
		}
	}
	if len(episodes) == 0 {
		return 0, false
	}
	rates := make([]float64, len(episodes))
	for i, ep := range episodes {
		rates[i] = ep.Rate
	}
	slices.Sort(rates)
	mid := len(rates) / 2
	if len(rates)%2 == 0 {
		return (rates[mid-1] + rates[mid]) / 2, true
	}
	return rates[mid], true
}

// heatEpisode is the heat-up a zone runs now. lastAt and lastC are the last
// point below the target, which the crossing is interpolated from; reportAt is
// when the zone report last changed.
type heatEpisode struct {
	start    time.Time
	startC   float64
	targetC  float64
	outdoorC *float64
	lastAt   time.Time
	lastC    float64
	reportAt time.Time
	demanded bool
}

// earlyStart is a block a zone runs before the program reaches it at boundary.
// It is stored with the program blocks: the stored block of the zone is this
// one, and a restart that forgot it would write the zone back to the colder
// block before the boundary.
type earlyStart struct {
	at, boundary time.Time
	block        scheduledBlock
}

// freshOutdoor returns the outdoor temperature while it is fresh.
func (d *daemon) freshOutdoor(o tado.Options, now time.Time) *float64 {
	if d.outdoor == nil || now.Sub(d.outdoor.at) > o.OutdoorMaxAge() {
		return nil
	}
	c := d.outdoor.tempC
	return &c
}

// nodeOverlays maps the short address of each node to whether it runs an
// overlay.
func nodeOverlays(e *tado.EmuState) map[string]bool {
	out := make(map[string]bool)
	for _, n := range e.Nodes() {
		out[n.Short] = n.LastOverlay != overlayAutoMode
	}
	return out
}

func zoneOverlay(inst *tado.Install, zone int, overlays map[string]bool) bool {
	members, err := inst.ZoneMembers(zone)
	if err != nil {
		return false
	}
	for _, m := range members {
		if overlays[m.Short()] {
			return true
		}
	}
	return false
}

// heatUpBlocker names what keeps zone off its program block, empty when the
// zone follows it.
func (d *daemon) heatUpBlocker(inst *tado.Install, zone int, overlays map[string]bool) string {
	block, known := d.zoneSched[zone]
	switch {
	case d.away:
		return "away"
	case !known:
		return "no program block"
	case block.off:
		return "program block off"
	case d.windows[zone].open():
		return "open window"
	case zoneOverlay(inst, zone, overlays):
		return "overlay"
	}
	return ""
}

// observeHeatUps runs the learner on every heating zone. An episode opens when
// the target of a zone that follows its program rises at least thermalMinRiseC
// above the zone temperature, and ends when a report reaches the target.
func (d *daemon) observeHeatUps(e *tado.EmuState) {
	inst := d.inst.Load()
	if inst == nil {
		return
	}
	now := d.retryNow()
	overlays := nodeOverlays(e)
	heating := make(map[int]bool)
	for i := range inst.Zones {
		if z := &inst.Zones[i]; z.Kind == tado.ZoneHeating {
			heating[z.Number] = true
			d.observeHeatUp(e, inst, z.Number, now, overlays)
			d.recordPlant(e, inst, z.Number, now, overlays)
		}
	}
	for zone := range d.heating {
		if !heating[zone] {
			delete(d.heating, zone)
		}
	}
	for zone := range d.plant {
		if !heating[zone] {
			delete(d.plant, zone)
		}
	}
	for zone := range d.thermalTarget {
		if !heating[zone] {
			delete(d.thermalTarget, zone)
		}
	}
}

// forgetRemovedZones drops what the engine keeps about each zone before
// declares and in does not: the heat-up model, the episodes running, the early
// start, the stored plant episodes and the program block. A zone later declared
// under the same number starts from nothing. It runs on the engine goroutine,
// which is the one that stores a model or an episode, so no late save brings
// one back.
func (d *daemon) forgetRemovedZones(before, in *tado.Install) {
	for i := range before.Zones {
		zone := before.Zones[i].Number
		if _, err := in.ZoneByNumber(zone); err == nil {
			continue
		}
		delete(d.thermal, zone)
		delete(d.heating, zone)
		delete(d.thermalTarget, zone)
		if _, ok := d.early[zone]; ok {
			delete(d.early, zone)
			d.schedUnsaved = true
		}
		delete(d.plant, zone)
		delete(d.zoneSched, zone)
		if _, ok := d.schedPushed[zone]; ok {
			delete(d.schedPushed, zone)
			d.schedUnsaved = true
		}
		if d.state != nil {
			if err := d.state.DeleteZoneLearning(context.Background(), zone); err != nil {
				log.Printf("zone %d removed, its heat-up model and plant episodes stay stored: %v", zone, err)
			}
		}
	}
	d.saveProgramBlocks()
}

func (d *daemon) observeHeatUp(e *tado.EmuState, inst *tado.Install, zone int, now time.Time, overlays map[string]bool) {
	blocker := d.heatUpBlocker(inst, zone, overlays)
	target := d.zoneSched[zone].c
	prev, hadPrev := d.thermalTarget[zone]
	if blocker == "" {
		d.thermalTarget[zone] = target
	} else {
		delete(d.thermalTarget, zone)
	}
	tempC, demand, updated, reported := d.zoneReport(e, zone)
	maxGap := inst.Options.OfflineAfter()
	if ep := d.heating[zone]; ep != nil {
		switch {
		case blocker != "":
			d.rejectHeatUp(zone, blocker)
		case target != ep.targetC:
			d.rejectHeatUp(zone, "target changed")
		case reported && updated.After(ep.reportAt):
			ep.reportAt = updated
			ep.demanded = ep.demanded || demand > 0
			if tempC >= ep.targetC {
				d.finishHeatUp(zone, ep, tempC, updated)
			} else if tempC > ep.lastC {
				ep.lastAt, ep.lastC = updated, tempC
			}
		case now.Sub(ep.reportAt) > maxGap:
			d.rejectHeatUp(zone, "no zone report")
		case now.Sub(ep.start) > thermalMaxEpisode:
			d.rejectHeatUp(zone, "target not reached")
		}
		return
	}
	if blocker != "" || !hadPrev || target <= prev || !reported || now.Sub(updated) > maxGap ||
		target-tempC < thermalMinRiseC {
		return
	}
	d.heating[zone] = &heatEpisode{
		start: now, startC: tempC, targetC: target, outdoorC: d.freshOutdoor(inst.Options, now),
		lastAt: now, lastC: tempC, reportAt: updated, demanded: demand > 0,
	}
	d.openPlantHeat(e, inst, zone, target, now)
}

func (d *daemon) rejectHeatUp(zone int, reason string) {
	log.Printf("zone %d heat-up rejected: %s", zone, reason)
	delete(d.heating, zone)
}

// finishHeatUp accepts the episode a report at or above the target ends. The
// crossing is interpolated between the last point below the target and this
// report, so the report period does not round the duration up.
func (d *daemon) finishHeatUp(zone int, ep *heatEpisode, tempC float64, at time.Time) {
	cross := at
	if tempC > ep.lastC && at.After(ep.lastAt) {
		frac := (ep.targetC - ep.lastC) / (tempC - ep.lastC)
		cross = ep.lastAt.Add(time.Duration(frac * float64(at.Sub(ep.lastAt))))
	}
	elapsed := cross.Sub(ep.start)
	if !ep.demanded {
		d.rejectHeatUp(zone, "no heat demand")
		return
	}
	if elapsed <= 0 {
		d.rejectHeatUp(zone, "target reached at the start")
		return
	}
	rate := (ep.targetC - ep.startC) / elapsed.Hours()
	if rate < thermalMinRate || rate > thermalMaxRate {
		d.rejectHeatUp(zone, "rate "+strconv.FormatFloat(rate, 'f', 2, 64)+" C/h out of range")
		return
	}
	delete(d.heating, zone)
	model := d.thermal[zone]
	if model == nil {
		model = &thermalModel{}
		d.thermal[zone] = model
	}
	model.add(thermalEpisode{
		EndedMs: cross.UnixMilli(), StartC: ep.startC, TargetC: ep.targetC,
		Minutes: math.Round(elapsed.Minutes()*10) / 10, Rate: math.Round(rate*100) / 100, OutdoorC: ep.outdoorC,
	})
	log.Printf("zone %d heat-up %.1f -> %.1f C in %s: %.2f C/h", zone, ep.startC, ep.targetC, elapsed.Round(time.Minute), rate)
	if d.state != nil {
		if err := d.state.SetThermalModel(context.Background(), zone, model); err != nil {
			log.Printf("persist zone %d heat-up model: %v", zone, err)
		}
	}
}

// earlyPlan returns when zone starts its next block early: at home, with no
// overlay and no open window, with enough episodes, and only when the next
// block is warmer and the zone is below it. The lead is the time the model
// needs for the gap, capped by options.early_start_max_lead_sec.
func (d *daemon) earlyPlan(e *tado.EmuState, inst *tado.Install, zone int, now time.Time, overlays map[string]bool) (earlyStart, bool) {
	z, err := inst.ZoneByNumber(zone)
	if err != nil || z.Kind != tado.ZoneHeating || !inst.Options.EarlyStart(zone) ||
		d.away || d.windows[zone].open() || zoneOverlay(inst, zone, overlays) {
		return earlyStart{}, false
	}
	minEpisodes := inst.Options.EarlyStartMinEpisodeCount()
	model := d.thermal[zone]
	if model == nil || len(model.Episodes) < minEpisodes {
		return earlyStart{}, false
	}
	programs, ok := d.zonePrograms(e, inst, zone)
	if !ok {
		return earlyStart{}, false
	}
	loc := d.programClock(inst)
	cur, ok := scheduledBlockAt(programs, now, loc)
	if !ok {
		return earlyStart{}, false
	}
	boundary, ok := nextScheduleChange(programs, now, loc)
	if !ok {
		return earlyStart{}, false
	}
	next, ok := scheduledBlockAt(programs, boundary, loc)
	if !ok || next.off || (!cur.off && next.c <= cur.c) {
		return earlyStart{}, false
	}
	tempC, _, updated, ok := d.zoneReport(e, zone)
	if !ok || now.Sub(updated) > inst.Options.OfflineAfter() || tempC >= next.c {
		return earlyStart{}, false
	}
	rate, ok := model.rate(d.freshOutdoor(inst.Options, now), minEpisodes)
	if !ok || rate <= 0 {
		return earlyStart{}, false
	}
	lead := min(time.Duration((next.c-tempC)/rate*float64(time.Hour)), inst.Options.EarlyStartMaxLead())
	return earlyStart{at: boundary.Add(-lead), boundary: boundary, block: next}, true
}

// planEarlyStarts opens the early start of every zone whose plan is due. The
// block then holds until the program reaches it, so the block change itself
// finds nothing to push.
func (d *daemon) planEarlyStarts(e *tado.EmuState) {
	inst := d.inst.Load()
	if inst == nil || len(inst.Options.EarlyStartZones) == 0 {
		return
	}
	now := d.retryNow()
	overlays := nodeOverlays(e)
	for zone, on := range inst.Options.EarlyStartZones {
		if _, running := d.early[zone]; running || !on {
			continue
		}
		plan, ok := d.earlyPlan(e, inst, zone, now, overlays)
		if !ok || now.Before(plan.at) || !now.Before(plan.boundary) {
			continue
		}
		d.early[zone] = plan
		d.schedUnsaved = true
		log.Printf("zone %d early start: %g C, %s before the block", zone, plan.block.c, plan.boundary.Sub(now).Round(time.Minute))
	}
}

// earlyBlock returns the block zone holds now: the one it started early, until
// the program reaches it. Away, a disabled switch or a program that no longer
// holds that block at that instant ends the early start.
func (d *daemon) earlyBlock(e *tado.EmuState, inst *tado.Install, zone int, block scheduledBlock, now time.Time) scheduledBlock {
	es, ok := d.early[zone]
	if !ok {
		return block
	}
	if d.away || !inst.Options.EarlyStart(zone) || !now.Before(es.boundary) || !d.stillPlanned(e, inst, zone, es, now) {
		delete(d.early, zone)
		d.schedUnsaved = true
		return block
	}
	return es.block
}

func (d *daemon) stillPlanned(e *tado.EmuState, inst *tado.Install, zone int, es earlyStart, now time.Time) bool {
	programs, ok := d.zonePrograms(e, inst, zone)
	if !ok {
		return false
	}
	loc := d.programClock(inst)
	boundary, ok := nextScheduleChange(programs, now, loc)
	if !ok || !boundary.Equal(es.boundary) {
		return false
	}
	next, ok := scheduledBlockAt(programs, boundary, loc)
	return ok && next == es.block
}

// --- API ---

func (d *daemon) earlyStartView(e *tado.EmuState, inst *tado.Install, zone int, overlays map[string]bool) map[string]any {
	now := d.retryNow()
	o := inst.Options
	out := map[string]any{
		"zone": zone, "enabled": o.EarlyStart(zone),
		"minEpisodes": o.EarlyStartMinEpisodeCount(), "maxLeadSec": int(o.EarlyStartMaxLead() / time.Second),
		"episodes": 0, "rateCPerHour": nil, "updatedAt": nil, "history": []thermalEpisode{},
		"heatUp": nil, "active": nil, "next": nil,
	}
	if model := d.thermal[zone]; model != nil && len(model.Episodes) > 0 {
		rate, _ := model.rate(d.freshOutdoor(o, now), o.EarlyStartMinEpisodeCount())
		out["episodes"] = len(model.Episodes)
		out["rateCPerHour"] = math.Round(rate*100) / 100
		out["updatedAt"] = time.UnixMilli(model.Episodes[len(model.Episodes)-1].EndedMs).UTC().Format(time.RFC3339)
		out["history"] = model.Episodes
	}
	if ep := d.heating[zone]; ep != nil {
		out["heatUp"] = map[string]any{
			"since": ep.start.UTC().Format(time.RFC3339), "startC": ep.startC, "targetC": ep.targetC,
		}
	}
	if es, ok := d.early[zone]; ok {
		out["active"] = earlyStartPlanView(es)
	} else if plan, ok := d.earlyPlan(e, inst, zone, now, overlays); ok {
		out["next"] = earlyStartPlanView(plan)
	}
	return out
}

func earlyStartPlanView(es earlyStart) map[string]any {
	return map[string]any{
		"at": es.at.UTC().Format(time.RFC3339), "blockAt": es.boundary.UTC().Format(time.RFC3339),
		"leadSec": int(es.boundary.Sub(es.at) / time.Second), "setpointC": es.block.c,
	}
}

// getEarlyStarts returns the early start state of every heating zone.
func (d *daemon) getEarlyStarts(w http.ResponseWriter, r *http.Request) {
	inst := d.inst.Load()
	if inst == nil {
		writeResolveErr(w, errNoInstall)
		return
	}
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		overlays := nodeOverlays(e)
		out := []map[string]any{}
		for i := range inst.Zones {
			if z := &inst.Zones[i]; z.Kind == tado.ZoneHeating {
				out = append(out, d.earlyStartView(e, inst, z.Number, overlays))
			}
		}
		return map[string]any{"zones": out}, nil
	})
	write(w, v, err)
}

// earlyStartZone resolves the path zone to a declared heating zone.
func (d *daemon) earlyStartZone(w http.ResponseWriter, r *http.Request) (*tado.Install, int, bool) {
	zone, ok := pathInt(w, r, "number")
	if !ok {
		return nil, 0, false
	}
	inst := d.inst.Load()
	if inst == nil {
		writeResolveErr(w, errNoInstall)
		return nil, 0, false
	}
	z, err := inst.ZoneByNumber(zone)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return nil, 0, false
	}
	if z.Kind != tado.ZoneHeating {
		writeErr(w, http.StatusBadRequest, "zone "+strconv.Itoa(zone)+" is not a heating zone")
		return nil, 0, false
	}
	return inst, zone, true
}

func (d *daemon) getEarlyStart(w http.ResponseWriter, r *http.Request) {
	inst, zone, ok := d.earlyStartZone(w, r)
	if !ok {
		return
	}
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		return d.earlyStartView(e, inst, zone, nodeOverlays(e)), nil
	})
	write(w, v, err)
}

// putEarlyStart switches the early start of a zone, {"enabled": bool}. The
// switch is an installation option, so it takes the management path.
func (d *daemon) putEarlyStart(w http.ResponseWriter, r *http.Request) {
	zone, ok := pathInt(w, r, "number")
	if !ok {
		return
	}
	var b struct {
		Enabled *bool `json:"enabled"`
	}
	if !decodeBody(w, r, &b) {
		return
	}
	if b.Enabled == nil {
		writeErr(w, http.StatusBadRequest, "enabled is required")
		return
	}
	d.mutateInstall(w, "zone", func(store *stateStore) (installResult, any, error) {
		res, err := store.PatchZone(r.Context(), zone, zonePatch{EarlyStart: b.Enabled})
		if err != nil {
			return res, nil, err
		}
		return res, viewZoneOf(res.Install, zone), nil
	})
}

// deleteEarlyStartModel forgets the heat-up model of a zone and the episode it
// runs. A block already started early keeps running to its start.
func (d *daemon) deleteEarlyStartModel(w http.ResponseWriter, r *http.Request) {
	inst, zone, ok := d.earlyStartZone(w, r)
	if !ok {
		return
	}
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		delete(d.thermal, zone)
		delete(d.heating, zone)
		// Deleted on the engine goroutine, where finishHeatUp stores a model: a
		// model learned after this request stays stored.
		if d.state != nil {
			if err := d.state.DeleteThermalModel(context.Background(), zone); err != nil {
				return nil, &stateSaveError{err: err}
			}
		}
		return d.earlyStartView(e, inst, zone, nodeOverlays(e)), nil
	})
	if writeHandlerErr(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, v)
}
