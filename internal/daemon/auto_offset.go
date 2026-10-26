package daemon

// auto_offset: the automatic calibration of a heating zone's temperature
// correction (tag 0140 on the valve) from an external room temperature, in the
// modes config and dbg2. Mode setpoint corrects continuously from the reading
// instead (room_temp.go), so it refuses the option.
//
// The daemon filters the difference between the external reading and the
// calibrated temperature 012d of the measuring device, the device it writes the
// correction to. It samples while the reading is fresh, the device sends
// frames and has reported 012d since the last change of the correction, the
// zone window is closed and no heat-up episode runs. At most once per interval
// it writes the current correction plus that difference to the valve, through
// the same path as POST /api/zones/{n}/offset. A write moves 012d by the
// written delta, so the filter restarts after it. When the filter next holds
// enough samples and the difference has not shrunk, the daemon stops the
// automatic writes of that zone until the option is switched off and on. The
// tracked correction, the last write time, the pending check and the stop are
// stored, so a restart does not write again before the interval ends.

import (
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"math"
	"slices"
	"time"

	"freedo/tado"
)

const (
	// autoOffsetMinSamples is how much sampled time the filter needs before it
	// may write, twice the time constant of the filter.
	autoOffsetMinSamples = 2 * time.Hour
	// autoOffsetMaxAbsC bounds the correction the calibration writes, as the
	// setpoint release does.
	autoOffsetMaxAbsC = 5.0
	// autoOffsetDeadbandC is the smallest change of the correction worth a
	// write: a smaller one is inside the noise of the difference.
	autoOffsetDeadbandC = 0.3
	// autoOffsetShrinkC is how much the filtered difference must shrink after a
	// write: half the smallest write, and above the ripple the filter leaves on
	// a valve swing (see roomShiftTau).
	autoOffsetShrinkC = autoOffsetDeadbandC / 2
)

// autoOffset is the calibration of one zone. corr is the correction the daemon
// believes the valve holds, wroteAt the last automatic write, diffC the filtered
// difference and sampledFor the sampled time since the filter restarted.
// movedAt is the last change of the correction: a 012d report before it
// predates the change. checkDue says that checkDiffC, the difference at the
// last write, waits for the check, and stop is why the automatic writes
// stopped. All belong to the engine goroutine.
type autoOffset struct {
	corr       float64
	wroteAt    time.Time
	diffC      float64
	sampled    bool
	sampledFor time.Duration
	evalAt     time.Time
	movedAt    time.Time
	checkDue   bool
	checkDiffC float64
	stop       string
}

// storedAutoOffset is the JSON of one zone's calibration in meta auto_offset.
type storedAutoOffset struct {
	Centi      int    `json:"centi"`
	WroteMs    int64  `json:"wroteMs"`
	CheckCenti *int   `json:"checkCenti,omitempty"`
	Stop       string `json:"stop,omitempty"`
}

// learnerReadsExternal reports whether the learners of zone read the external
// room temperature now: in mode setpoint, with auto_offset on or with the
// coordinator as owner, while the reading is fresh. The reading does not move when the
// correction is written, so a write does not disturb a running episode then.
func (d *daemon) learnerReadsExternal(inst *tado.Install, zone int) bool {
	if inst == nil {
		return false
	}
	if _, ok := d.freshRoomTemp(inst, zone); !ok {
		return false
	}
	return zoneTakesReading(inst, zone)
}

// autoEligible reports whether zone may carry an automatic calibration: a
// heating zone in mode config or dbg2 with the option on.
func autoEligible(inst *tado.Install, zone int) bool {
	z, err := inst.ZoneByNumber(zone)
	if err != nil || z.Kind != tado.ZoneHeating {
		return false
	}
	if inst.Options.ZoneOffset(zone).Mode == tado.OffsetModeSetpoint {
		return false
	}
	return inst.Options.AutoOffset(zone).Enabled
}

// tickAutoOffsets steps the calibration of every eligible zone and stores the
// ones that moved.
func (d *daemon) tickAutoOffsets(e *tado.EmuState) {
	inst := d.inst.Load()
	if inst == nil {
		return
	}
	now := d.retryNow()
	d.dropAutoOffsets(inst)
	zones := make([]int, 0, len(inst.Zones))
	for i := range inst.Zones {
		zone := inst.Zones[i].Number
		if !autoEligible(inst, zone) {
			continue
		}
		zones = append(zones, zone)
		if d.autoOffsets[zone] == nil {
			d.autoOffsets[zone] = &autoOffset{evalAt: now}
		}
	}
	slices.Sort(zones)
	for _, zone := range zones {
		d.stepAutoOffset(e, inst, zone, d.autoOffsets[zone], now)
	}
	d.saveAutoOffsets()
}

// dropAutoOffsets forgets the calibration of the zones that may no longer
// carry one. setInstall calls it too, so the option switched off and on clears
// a stop even within one engine tick.
func (d *daemon) dropAutoOffsets(inst *tado.Install) {
	for zone := range d.autoOffsets {
		if !autoEligible(inst, zone) {
			delete(d.autoOffsets, zone)
			d.autoUnsaved = true
		}
	}
	d.saveAutoOffsets()
}

// heatUpRunning reports whether zone runs a heat-up episode or a plant heat
// record now.
func (d *daemon) heatUpRunning(zone int) bool {
	if d.heating[zone] != nil {
		return true
	}
	run := d.plant[zone]
	return run != nil && run.ep.Kind == plantHeat
}

// stepAutoOffset takes a sample when it may, checks that the last write moved
// the difference, and writes a new correction once the samples and the
// interval allow it.
func (d *daemon) stepAutoOffset(e *tado.EmuState, inst *tado.Install, zone int, x *autoOffset, now time.Time) {
	a := inst.Options.AutoOffset(zone)
	dt := now.Sub(x.evalAt)
	if x.evalAt.IsZero() || dt < 0 {
		x.evalAt, dt = now, 0
	}
	// The engine ticks every second: evalAt moves only with a sample, or dt
	// never reaches roomTempEvery.
	if dt >= roomTempEvery {
		x.evalAt = now
		if ext, ok := d.freshRoomTemp(inst, zone); ok && !d.windows[zone].open() && !d.heatUpRunning(zone) {
			if devC, ok := measuredC(e, inst, zone, x.movedAt, now); ok {
				diff := ext - devC
				switch {
				case !x.sampled:
					x.diffC, x.sampled = diff, true
				default:
					x.diffC += (diff - x.diffC) * (1 - math.Exp(-dt.Seconds()/roomShiftTau.Seconds()))
				}
				x.sampledFor += dt
			}
		}
	}
	if x.sampledFor < autoOffsetMinSamples {
		return
	}
	if x.checkDue {
		x.checkDue, d.autoUnsaved = false, true
		if math.Abs(x.diffC) > math.Abs(x.checkDiffC)-autoOffsetShrinkC {
			x.stop = fmt.Sprintf("the filtered difference did not shrink after the last write: %+.2f C before it, %+.2f C after", x.checkDiffC, x.diffC)
			log.Printf("zone %d auto offset: stopped, %s", zone, x.stop)
		}
	}
	// A write that does not move the temperature the filter reads would grow
	// the correction up to the bound, and in mode config home the motor each
	// time.
	if x.stop != "" {
		return
	}
	// Never write over a heat-up: the offset moves 4060 and the learner would
	// read the step as the plant.
	if d.heatUpRunning(zone) {
		return
	}
	if now.Sub(d.autoOffsetBase(x)) < time.Duration(a.IntervalSec)*time.Second {
		return
	}
	// The served document holds the correction the valve runs, also one set
	// before the calibration was on: start from it.
	if dev, err := inst.MeasuringDevice(zone); err == nil {
		if centi, ok := e.ServedCalibOffset(hex.EncodeToString(dev.WireEUI())); ok {
			x.corr = float64(centi) / 100
		}
	}
	target := math.Round((x.corr+x.diffC)*10) / 10
	target = min(max(target, -autoOffsetMaxAbsC), autoOffsetMaxAbsC)
	if math.Abs(target-x.corr) < autoOffsetDeadbandC {
		return
	}
	if err := d.writeAutoOffset(e, inst, zone, target); err != nil {
		log.Printf("zone %d auto offset: %v", zone, err)
		x.wroteAt, d.autoUnsaved = now, true
		return
	}
	log.Printf("zone %d auto offset: correction %+.1f -> %+.1f C (filtered difference %+.2f C)",
		zone, x.corr, target, x.diffC)
	x.corr, x.wroteAt, x.movedAt, d.autoUnsaved = target, now, now, true
	// The served d/config document changed: save it, or a restart would serve
	// the old one and the valve would take back the old correction.
	d.unsaved = true
	// The write moves 012d by the delta: restart the filter, and check the
	// difference it builds against this one.
	x.checkDiffC, x.checkDue = x.diffC, true
	x.diffC, x.sampledFor, x.sampled = 0, 0, false
}

// measuredC is the calibrated temperature 012d of the measuring device of zone,
// from a report no older than movedAt, while that device sends frames. The
// calibration writes 0140 to that device alone, so 012d moves by the written
// delta. The zone temperature 4060 comes from the member that pushed it last,
// which a write to another member does not move.
func measuredC(e *tado.EmuState, inst *tado.Install, zone int, movedAt, now time.Time) (float64, bool) {
	dev, err := inst.MeasuringDevice(zone)
	if err != nil {
		return 0, false
	}
	c, reported, seen, ok := e.DeviceTemp(hex.EncodeToString(dev.WireEUI()))
	if !ok || reported.Before(movedAt) || now.Sub(seen) > inst.Options.OfflineAfter() {
		return 0, false
	}
	return c, true
}

// autoOffsetBase is where the interval of x starts: the last write, or the
// engine start before the first one. A restart does not move it, or a daemon
// that restarts more often than the interval would never calibrate.
func (d *daemon) autoOffsetBase(x *autoOffset) time.Time {
	if x.wroteAt.IsZero() {
		return d.flowEpoch
	}
	return x.wroteAt
}

// writeAutoOffset queues the correction of zone on the valve, through the path
// of POST /api/zones/{n}/offset for the mode the zone holds.
func (d *daemon) writeAutoOffset(e *tado.EmuState, inst *tado.Install, zone int, corrC float64) error {
	dev, err := inst.MeasuringDevice(zone)
	if err != nil {
		return err
	}
	target := hex.EncodeToString(dev.WireEUI())
	centi := int(math.Round(corrC * 100))
	if inst.Options.ZoneOffset(zone).Mode == tado.OffsetModeDbg2 {
		err = e.EnqueueCalibOffsetDbg2(target, zone, centi)
	} else {
		err = e.EnqueueConfig(target, zone, tado.ConfigWrite{CalibOffset: &centi})
	}
	if err != nil {
		return err
	}
	d.requestQueuedDispatch(target)
	return nil
}

// noteOffsetChange records that the correction of zone moved to corrCenti by a
// manual or automatic write, on the engine goroutine. Unless the learners read
// the external reading, it rejects a running heat-up episode and ends a running
// plant heat episode: the write moves 4060 by the delta, so an episode that
// spans it would read a step that is not from the plant. The automatic filter
// restarts for the same reason, and drops the check of the last automatic
// write: the next difference no longer measures that write alone.
func (d *daemon) noteOffsetChange(inst *tado.Install, zone, corrCenti int) {
	now := d.retryNow()
	if x := d.autoOffsets[zone]; x != nil {
		x.corr, d.autoUnsaved = float64(corrCenti)/100, true
		x.diffC, x.sampledFor, x.sampled = 0, 0, false
		x.checkDue = false
		x.evalAt, x.movedAt = now, now
	}
	if d.learnerReadsExternal(inst, zone) {
		return
	}
	if d.heating[zone] != nil {
		d.rejectHeatUp(zone, "offset changed")
	}
	if run := d.plant[zone]; run != nil && run.ep.Kind == plantHeat {
		d.endPlant(zone, "offset changed", now)
	}
}

// saveAutoOffsets stores the tracked corrections and the last write times once
// they moved. A failed save stays owed to the next tick.
func (d *daemon) saveAutoOffsets() {
	if !d.autoUnsaved || d.state == nil {
		return
	}
	stored := make(map[int]storedAutoOffset, len(d.autoOffsets))
	for zone, x := range d.autoOffsets {
		s := storedAutoOffset{Centi: int(math.Round(x.corr * 100)), Stop: x.stop}
		if !x.wroteAt.IsZero() {
			s.WroteMs = x.wroteAt.UnixMilli()
		}
		if x.checkDue {
			check := int(math.Round(x.checkDiffC * 100))
			s.CheckCenti = &check
		}
		stored[zone] = s
	}
	if err := d.state.SetAutoOffsets(context.Background(), stored); err != nil {
		log.Printf("persist the auto offsets: %v", err)
		return
	}
	d.autoUnsaved = false
}

// restoreAutoOffsets loads the stored calibrations before the engine starts.
// The filter restarts empty.
func (d *daemon) restoreAutoOffsets() error {
	if d.state == nil {
		return nil
	}
	stored, err := d.state.AutoOffsets(context.Background())
	if err != nil {
		return err
	}
	for zone, s := range stored {
		x := &autoOffset{corr: float64(s.Centi) / 100, evalAt: d.retryNow(), stop: s.Stop}
		if s.WroteMs > 0 {
			x.wroteAt = time.UnixMilli(s.WroteMs).UTC()
		}
		if s.CheckCenti != nil {
			x.checkDue, x.checkDiffC = true, float64(*s.CheckCenti)/100
		}
		d.autoOffsets[zone] = x
	}
	return nil
}

// autoOffsetView renders the automatic calibration of one heating zone:
// enabled, the interval, the last write time, the next earliest calibration,
// the filtered difference, the sampled span and why the automatic writes
// stopped. A stopped zone has no next calibration.
func (d *daemon) autoOffsetView(inst *tado.Install, zone int) map[string]any {
	a := inst.Options.AutoOffset(zone)
	view := map[string]any{
		"enabled": a.Enabled, "intervalSec": a.IntervalSec,
		"lastWriteAt": nil, "nextAt": nil, "filteredDiffC": nil, "sampleSpanSec": 0,
		"stopReason": nil,
	}
	x := d.autoOffsets[zone]
	if x == nil {
		return view
	}
	if x.stop != "" {
		view["stopReason"] = x.stop
	} else {
		view["nextAt"] = d.autoOffsetBase(x).Add(time.Duration(a.IntervalSec) * time.Second).UTC().Format(time.RFC3339)
	}
	view["sampleSpanSec"] = int(x.sampledFor / time.Second)
	if !x.wroteAt.IsZero() {
		view["lastWriteAt"] = x.wroteAt.UTC().Format(time.RFC3339)
	}
	if x.sampled {
		view["filteredDiffC"] = roundCenti(x.diffC)
	}
	return view
}
