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
	"slices"
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
	plantEpisodesKept = 20
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
// samples do not define it; a cool-down derives none.
type plantEpisode struct {
	Kind         string        `json:"kind"`
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

// plantIdle reports zero demand: 40a0 reported 0 and 4160 not above 0.
func plantIdle(s plantSample) bool {
	return s.DemandPct != nil && *s.DemandPct == 0 && (s.HeatPct == nil || *s.HeatPct == 0)
}

// plantSample takes 4060 on the corrected scale of the heat-up target
// (zone_offset), so the crossing and the band compare like with like.
func (d *daemon) plantSample(e *tado.EmuState, inst *tado.Install, zone int, now time.Time) (plantSample, time.Time, bool) {
	tempC, demand, heat, updated, ok := e.ZoneChain(zone)
	if !ok {
		return plantSample{}, time.Time{}, false
	}
	return plantSample{
		AtMs: updated.UnixMilli(), TempC: correctedC(inst, zone, tempC), DemandPct: demand, HeatPct: heat,
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
		ep:    plantEpisode{Kind: plantHeat, StartedMs: now.UnixMilli(), TargetC: &targetC, Samples: []plantSample{s}},
		start: now, reportAt: updated,
	}
}

// recordPlant samples zone on a new report and ends its record when the
// episode is over. With no record running and no heat-up learned, a fresh
// report of zero demand opens a cool-down.
func (d *daemon) recordPlant(e *tado.EmuState, inst *tado.Install, zone int, now time.Time, overlays map[string]bool) {
	s, updated, reported := d.plantSample(e, inst, zone, now)
	run := d.plant[zone]
	if run == nil {
		if reported && now.Sub(updated) <= inst.Options.OfflineAfter() && d.heating[zone] == nil &&
			!d.windows[zone].open() && plantIdle(s) {
			d.plant[zone] = &plantRun{
				ep:    plantEpisode{Kind: plantCool, StartedMs: now.UnixMilli(), Samples: []plantSample{s}},
				start: now, reportAt: updated,
			}
		}
		return
	}
	if reported && updated.After(run.reportAt) {
		if run.ep.Kind == plantCool && !plantIdle(s) {
			d.endPlant(zone, "demand", now)
			return
		}
		run.add(s, updated)
	}
	if reason := d.plantEnd(inst, zone, run, now, overlays); reason != "" {
		d.endPlant(zone, reason, now)
	}
}

// plantEnd names why the record of zone ends now, empty while it runs.
func (d *daemon) plantEnd(inst *tado.Install, zone int, run *plantRun, now time.Time, overlays map[string]bool) string {
	switch {
	case len(run.ep.Samples) >= plantMaxSamples:
		return "sample limit"
	case now.Sub(run.reportAt) > inst.Options.OfflineAfter():
		return "no zone report"
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
	if ep.Kind == plantHeat {
		run.derive(&ep)
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
	if sec, from, ok := plantDeadTime(ep.Samples); ok {
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
// with 4160 above 0 after one without: the valve motor follows 4160. A zone
// that never reported 4160 takes the onset of 40a0 instead.
func plantDeadTime(samples []plantSample) (float64, string, bool) {
	from, pct := "40a0", func(s plantSample) *int { return s.DemandPct }
	if slices.ContainsFunc(samples, func(s plantSample) bool { return s.HeatPct != nil }) {
		from, pct = "4160", func(s plantSample) *int { return s.HeatPct }
	}
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
