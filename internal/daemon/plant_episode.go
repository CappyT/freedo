package daemon

// plant_episode: the plant data a PID gain fit needs and the heat-up model does
// not keep. The recorder samples the zone chain (4060, 40a0, 4160) once per
// zone report over two kinds of episode: a heat-up, from the target rise the
// learner opens on to the settling after the crossing, and a cool-down, a
// stretch of zero demand. It only reads: no frame depends on it.

import (
	"context"
	"log"
	"math"
	"net/http"
	"time"

	"freedo/tado"
)

const (
	plantHeat = "heat"
	plantCool = "cool"
	// plantRiseC is the 4060 rise that ends the dead time: one step of the tag.
	plantRiseC = 0.1
	// A heat-up settles once 4060 holds within plantSettleBandC of the target
	// for plantSettleHold, and ends unsettled plantSettleMax after the crossing.
	plantSettleBandC = 0.2
	plantSettleHold  = 30 * time.Minute
	plantSettleMax   = 2 * time.Hour
	// plantCoolMin is the shortest cool-down kept. plantCoolMax ends a long
	// one, and the next one opens on the next tick.
	plantCoolMin = 30 * time.Minute
	plantCoolMax = 12 * time.Hour
	// plantMaxSamples bounds one episode whatever the report cadence.
	plantMaxSamples = 1440
	// plantEpisodesKept is how many episodes of each kind a zone keeps stored.
	// The onset heat-ups of an owned zone (openPlantOnset) have their own
	// plantOnsetKept slots, so they never evict a heat-up of the learner.
	plantEpisodesKept = 20
	plantOnsetKept    = 8
	// plantOnsetMinRiseC is the rise an onset heat-up needs to be stored: most
	// onsets are small corrections with no step response.
	plantOnsetMinRiseC = 0.3
	// plantEps absorbs the float error of the centi-degree tags.
	plantEps = 1e-9
)

// plantSample is the zone chain after one report. A nil field was never
// reported; outdoorC is nil without a fresh POST /api/outdoor.
type plantSample struct {
	AtMs      int64    `json:"atMs"`
	TempC     float64  `json:"tempC"`
	DemandPct *int     `json:"demandPct"`
	HeatPct   *int     `json:"heatPct"`
	OutdoorC  *float64 `json:"outdoorC"`
}

// plantEpisode is one recorded episode. A derived field is nil when the
// samples do not define it; a cool-down derives none. Owner is who ran the
// zone controller (tado.ZoneOwnerDevice or tado.ZoneOwnerCoordinator), empty
// in an episode stored before the recorder kept it.
type plantEpisode struct {
	Kind         string        `json:"kind"`
	Owner        string        `json:"owner,omitempty"`
	Onset        bool          `json:"onset,omitempty"`
	StartedMs    int64         `json:"startedMs"`
	EndedMs      int64         `json:"endedMs"`
	EndReason    string        `json:"endReason"`
	TargetC      *float64      `json:"targetC"`
	CrossedMs    *int64        `json:"crossedMs"`
	DeadTimeSec  *float64      `json:"deadTimeSec"`
	DeadTimeFrom string        `json:"deadTimeFrom,omitempty"`
	OvershootC   *float64      `json:"overshootC"`
	SettlingSec  *float64      `json:"settlingSec"`
	Samples      []plantSample `json:"samples"`
}

// plantRun is the episode a zone records now. reportAt is when the zone report
// of the last sample changed, crossedAt the first report at or above the
// target, and inBandAt the first report of the stretch within the settle band
// that holds since.
type plantRun struct {
	ep                  plantEpisode
	start, reportAt     time.Time
	crossedAt, inBandAt time.Time
	// source is the temperature source of the samples (plantSource) at the
	// start.
	source string
}

func (r *plantRun) add(s plantSample, at time.Time) {
	r.ep.Samples = append(r.ep.Samples, s)
	r.reportAt = at
	if r.ep.Kind != plantHeat {
		return
	}
	target := *r.ep.TargetC
	if r.crossedAt.IsZero() {
		if s.TempC < target-plantEps {
			return
		}
		r.crossedAt = at
	}
	switch {
	case math.Abs(s.TempC-target) > plantSettleBandC+plantEps:
		r.inBandAt = time.Time{}
	case r.inBandAt.IsZero():
		r.inBandAt = at
	}
}

// plantIdle reports zero demand: 40a0 reported 0, and on a zone the daemon
// does not own 4160 not above 0. On an owned zone 4160 is the circuit demand
// of the other zones: the valves of the zone follow its 40a0 of 0.
func plantIdle(s plantSample, owned bool) bool {
	return s.DemandPct != nil && *s.DemandPct == 0 && (owned || s.HeatPct == nil || *s.HeatPct == 0)
}

// plantOwnerGap names why the samples of an owned zone stop describing the
// plant, empty while they do and on a zone the daemon does not own. The
// samples come from the controller step, which runs on with no member valve
// on air (the demand moves no valve) and falls back to the device sensor when
// the external reading goes stale. seen maps a node to its last frame.
func (d *daemon) plantOwnerGap(inst *tado.Install, zone int, now time.Time, seen map[string]time.Time) string {
	if !inst.Options.CoordinatorOwns(zone) {
		return ""
	}
	if x := d.roomTemps[zone]; x != nil && !x.at.IsZero() && !roomTempFresh(inst, x, now) {
		return "no zone report"
	}
	members, err := inst.ZoneMembers(zone)
	if err != nil {
		return "no zone report"
	}
	for _, m := range members {
		if t, ok := seen[m.Short()]; ok && now.Sub(t) <= inst.Options.OfflineAfter() {
			return ""
		}
	}
	return "no zone report"
}

// plantSource names the temperature source of the samples of zone: the
// source the controller of an owned zone read at its last step, empty on a
// zone the daemon does not own (4060 alone).
func (d *daemon) plantSource(inst *tado.Install, zone int) string {
	if x := d.zoneOwners[zone]; inst.Options.CoordinatorOwns(zone) && x != nil && x.haveOut {
		return x.source
	}
	return ""
}

// plantSample takes 4060 on the corrected scale of the heat-up target
// (zone_offset), so the crossing and the band compare like with like. On an
// owned zone it takes the temperature and the demand of the daemon's own
// controller step: the members echo pushed measures minutes late, between
// fresh ones, so the zone report would interleave two time bases. Before the
// controller has a temperature it takes no sample.
func (d *daemon) plantSample(e *tado.EmuState, inst *tado.Install, zone int, now time.Time) (plantSample, time.Time, bool) {
	tempC, demand, heat, updated, ok := e.ZoneChain(zone)
	if inst.Options.CoordinatorOwns(zone) {
		x := d.zoneOwners[zone]
		if x == nil || !x.haveOut || x.lost {
			return plantSample{}, time.Time{}, false
		}
		pct := x.pct
		return plantSample{
			AtMs: x.evalAt.UnixMilli(), TempC: x.tempC, DemandPct: &pct, HeatPct: heat,
			OutdoorC: d.freshOutdoor(inst.Options, now),
		}, x.evalAt, true
	}
	if !ok {
		return plantSample{}, time.Time{}, false
	}
	return plantSample{
		AtMs: updated.UnixMilli(), TempC: d.roomTempC(inst, zone, tempC), DemandPct: demand, HeatPct: heat,
		OutdoorC: d.freshOutdoor(inst.Options, now),
	}, updated, true
}

// openPlantHeat starts the heat-up record of zone on the target rise the
// learner opens its episode on. A record still running ends there.
func (d *daemon) openPlantHeat(e *tado.EmuState, inst *tado.Install, zone int, targetC float64, now time.Time) {
	if run := d.plant[zone]; run != nil {
		reason := "heat-up"
		if run.ep.Kind == plantHeat {
			reason = "target changed"
		}
		d.endPlant(zone, reason, now)
	}
	s, updated, ok := d.plantSample(e, inst, zone, now)
	if !ok {
		return
	}
	d.plant[zone] = &plantRun{
		ep: plantEpisode{
			Kind: plantHeat, Owner: inst.Options.ZoneOwner(zone), StartedMs: now.UnixMilli(), TargetC: &targetC,
			Samples: []plantSample{s},
		},
		start: now, reportAt: updated, source: d.plantSource(inst, zone),
	}
}

// recordPlant samples zone on a new report and ends its record when the
// episode is over. With no record running and no heat-up learned, a fresh
// report of zero demand opens a cool-down. On an owned zone the demand that
// ends a cool-down opens a heat-up (openPlantOnset).
func (d *daemon) recordPlant(e *tado.EmuState, inst *tado.Install, zone int, now time.Time, overlays map[string]bool, seen map[string]time.Time) {
	s, updated, reported := d.plantSample(e, inst, zone, now)
	owned := inst.Options.CoordinatorOwns(zone)
	run := d.plant[zone]
	if run == nil {
		if reported && now.Sub(updated) <= inst.Options.OfflineAfter() && d.heating[zone] == nil &&
			!d.windows[zone].open() && plantIdle(s, owned) && d.plantOwnerGap(inst, zone, now, seen) == "" {
			d.plant[zone] = &plantRun{
				ep: plantEpisode{
					Kind: plantCool, Owner: inst.Options.ZoneOwner(zone), StartedMs: now.UnixMilli(),
					Samples: []plantSample{s},
				},
				start: now, reportAt: updated, source: d.plantSource(inst, zone),
			}
		}
		return
	}
	if reported && updated.After(run.reportAt) {
		if run.ep.Kind == plantCool && !plantIdle(s, owned) {
			d.endPlant(zone, "demand", now)
			if owned {
				d.openPlantOnset(inst, zone, run.ep.Samples[len(run.ep.Samples)-1], s, updated, now, overlays, seen)
			}
			return
		}
		run.add(s, updated)
	}
	if reason := d.plantEnd(inst, zone, run, now, overlays, seen); reason != "" {
		d.endPlant(zone, reason, now)
	}
}

// openPlantOnset opens the heat-up record of an owned zone on the controller
// step whose demand leaves 0 while the target is above the room, whatever the
// rise: the daemon runs the controller, so it knows the onset. The record
// starts at the last idle sample, so the fit sees the onset. The learner opens
// its own heat-ups first in the same pass, so one onset makes one record.
// endPlant drops the record when 4060 rises less than plantOnsetMinRiseC.
func (d *daemon) openPlantOnset(inst *tado.Install, zone int, idle, s plantSample, updated, now time.Time, overlays map[string]bool, seen map[string]time.Time) {
	target := d.zoneSched[zone].c
	if d.heating[zone] != nil || d.heatUpBlocker(inst, zone, overlays) != "" || target <= s.TempC+plantEps ||
		d.plantOwnerGap(inst, zone, now, seen) != "" {
		return
	}
	run := &plantRun{
		ep: plantEpisode{
			Kind: plantHeat, Owner: tado.ZoneOwnerCoordinator, Onset: true, StartedMs: now.UnixMilli(),
			TargetC: &target, Samples: []plantSample{idle},
		},
		start: now, source: d.plantSource(inst, zone),
	}
	run.add(s, updated)
	d.plant[zone] = run
}

// plantEnd names why the record of zone ends now, empty while it runs.
func (d *daemon) plantEnd(inst *tado.Install, zone int, run *plantRun, now time.Time, overlays map[string]bool, seen map[string]time.Time) string {
	switch {
	case len(run.ep.Samples) >= plantMaxSamples:
		return "sample limit"
	case now.Sub(run.reportAt) > inst.Options.OfflineAfter():
		return "no zone report"
	}
	if inst.Options.ZoneOwner(zone) != run.ep.Owner {
		return "owner changed"
	}
	if gap := d.plantOwnerGap(inst, zone, now, seen); gap != "" {
		return gap
	}
	if d.plantSource(inst, zone) != run.source {
		return "source changed"
	}
	switch {
	case run.ep.Kind == plantCool && d.windows[zone].open():
		return "open window"
	case run.ep.Kind == plantCool && now.Sub(run.start) >= plantCoolMax:
		return "max duration"
	case run.ep.Kind == plantCool:
		return ""
	}
	if blocker := d.heatUpBlocker(inst, zone, overlays); blocker != "" {
		return blocker
	}
	switch {
	case d.zoneSched[zone].c != *run.ep.TargetC:
		return "target changed"
	case !run.inBandAt.IsZero() && now.Sub(run.inBandAt) >= plantSettleHold:
		return "settled"
	case run.crossedAt.IsZero() && now.Sub(run.start) > thermalMaxEpisode:
		return "target not reached"
	case !run.crossedAt.IsZero() && now.Sub(run.crossedAt) > plantSettleMax:
		return "not settled"
	}
	return ""
}

// endPlant closes the record of zone and stores it when it holds data: two
// samples at least, and a cool-down plantCoolMin long at least.
func (d *daemon) endPlant(zone int, reason string, now time.Time) {
	run := d.plant[zone]
	delete(d.plant, zone)
	ep := run.ep
	ep.EndedMs, ep.EndReason = now.UnixMilli(), reason
	if len(ep.Samples) < 2 || (ep.Kind == plantCool && now.Sub(run.start) < plantCoolMin) {
		return
	}
	if ep.Onset {
		temps := make([]float64, len(ep.Samples))
		for i, s := range ep.Samples {
			temps[i] = s.TempC
		}
		if rise, _ := plantSpanC(temps); rise < plantOnsetMinRiseC-plantEps {
			log.Printf("zone %d onset heat episode dropped: %.2f C rise in %d samples, %s", zone, rise, len(ep.Samples), reason)
			return
		}
	}
	if ep.Kind == plantHeat {
		run.derive(&ep)
		// An onset heat-up follows no program block: the history lists the
		// heat-ups of the program.
		if !ep.Onset {
			d.historyEpisode(zone, &ep)
		}
	}
	log.Printf("zone %d %s episode recorded: %d samples, %s", zone, ep.Kind, len(ep.Samples), reason)
	if d.state != nil {
		if err := d.state.AddPlantEpisode(context.Background(), zone, &ep); err != nil {
			log.Printf("persist zone %d plant episode: %v", zone, err)
		}
	}
}

// derive fills the dead time, and after a crossing the overshoot, the peak of
// 4060 over the target, and the settling time, from the crossing to the start
// of the stretch within the band that settled the zone.
func (r *plantRun) derive(ep *plantEpisode) {
	if sec, from, ok := plantDeadTime(ep.Samples, ep.Owner); ok {
		ep.DeadTimeSec, ep.DeadTimeFrom = &sec, from
	}
	if r.crossedAt.IsZero() {
		return
	}
	crossed := r.crossedAt.UnixMilli()
	ep.CrossedMs = &crossed
	peak := math.Inf(-1)
	for _, s := range ep.Samples {
		if s.AtMs >= crossed {
			peak = max(peak, s.TempC)
		}
	}
	over := math.Round((peak-*ep.TargetC)*100) / 100
	ep.OvershootC = &over
	if ep.EndReason == "settled" {
		sec := r.inBandAt.Sub(r.crossedAt).Seconds()
		ep.SettlingSec = &sec
	}
}

// plantDeadTime returns the delay from the demand onset to the first report of
// 4060 plantRiseC above its value at the onset. The onset is the first report
// with the plant input of plantHeatInput above 0 after one without.
func plantDeadTime(samples []plantSample, owner string) (float64, string, bool) {
	from, pct := plantHeatInput(samples, owner)
	on := func(s plantSample) bool { p := pct(s); return p != nil && *p > 0 }
	for i := 1; i < len(samples); i++ {
		if !on(samples[i]) || on(samples[i-1]) {
			continue
		}
		onset := samples[i]
		for _, s := range samples[i+1:] {
			if s.TempC >= onset.TempC+plantRiseC-plantEps {
				return float64(s.AtMs-onset.AtMs) / 1000, from, true
			}
		}
		break
	}
	return 0, "", false
}

// getPlantEpisodes returns the stored plant episodes of a heating zone, oldest
// first.
func (d *daemon) getPlantEpisodes(w http.ResponseWriter, r *http.Request) {
	_, zone, ok := d.earlyStartZone(w, r)
	if !ok {
		return
	}
	store, ok := d.installStore(w)
	if !ok {
		return
	}
	episodes, err := store.PlantEpisodes(r.Context(), zone)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"zone": zone, "settleBandC": plantSettleBandC, "settleHoldSec": int(plantSettleHold / time.Second),
		"episodes": episodes,
	})
}
