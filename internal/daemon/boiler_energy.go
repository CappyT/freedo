package daemon

// boiler_energy: an estimate of the heat the boiler delivers, integrated from
// the hvac/mon reports of the wall thermostat. Home Assistant turns the counter
// into an energy sensor and prices it. The estimate has three error sources:
// the modulation is relative to the boiler's own range, the efficiency is not
// modelled (the counter is heat output, not gas energy), and the reports are
// sparse, so a short burn or a change between two reports is approximated.
//
// The thermostat reports only the attributes that changed. The estimate holds
// the modulation and the burner flag of a report until the next report, and it
// drops a stretch the thermostat left unreported for longer than energyHold.
// The boiler status flags that split heating from hot water are not reported:
// the counter is a total.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"strconv"
	"time"

	"freedo/tado"
)

const (
	// energyHold is the longest stretch one report is held. While the burner
	// runs the thermostat sends an hvac/mon frame every 4..48 s (17 frames in
	// 223 s), and the modulation alone stayed unreported for up to 120 s. The
	// hold is 2.5 times that longest quiet stretch. A longer silence is a lost
	// link or a stopped thermostat, and the stretch adds nothing.
	energyHold = 5 * time.Minute
	// energySaveEvery bounds the writes of the counter to the database.
	energySaveEvery = time.Minute

	boilerEnergyKey = "boiler_energy_kwh"
)

// Reasons the estimate does not run, as the API names them.
const (
	energyNoMaxPower   = "noMaxPower"
	energyNoReport     = "noReport"
	energyStale        = "stale"
	energyNoBurnerFlag = "noBurnerFlag"
	energyNoModulation = "noModulation"
)

// energyMeter integrates the heat output between two reports. It belongs to the
// engine goroutine.
type energyMeter struct {
	// totalKWh counts every integrated stretch. shownKWh is the part the
	// database holds, and the only part the API serves: a restart then never
	// reads a counter below one a client has seen.
	totalKWh, shownKWh float64
	// prev is the last sample taken. updated is the time of the last stretch
	// integrated, savedAt the time of the last write attempt.
	prev           tado.BoilerSample
	updated, saved time.Time
}

// boilerKW returns the heat output a sample implies. why names what is missing
// when the output is unknown, and is empty otherwise. The modulation spans the
// boiler's range from its minimum to its maximum output, so it is no plain
// share of the maximum: the captures show the burner on at 0 % and 3 %.
func boilerKW(o tado.Options, s tado.BoilerSample) (kw float64, why string) {
	maxKW, minKW, ok := o.BoilerPower()
	switch {
	case !ok:
		return 0, energyNoMaxPower
	case s.Burner == nil:
		return 0, energyNoBurnerFlag
	case !*s.Burner:
		return 0, ""
	case s.ModulationPct == nil:
		return 0, energyNoModulation
	}
	mod := float64(min(max(*s.ModulationPct, 0), 100))
	return minKW + (maxKW-minKW)*mod/100, ""
}

// observe takes the sample a frame left in the engine. The stretch since the
// previous sample runs at the previous sample's output. A sample at the time of
// the previous one replaces it, and an older one is ignored, so a repeat or a
// clock that steps back never adds energy.
func (m *energyMeter) observe(s tado.BoilerSample, o tado.Options) {
	if s.At.IsZero() || s.At.Before(m.prev.At) {
		return
	}
	if dt := s.At.Sub(m.prev.At); !m.prev.At.IsZero() && dt > 0 && dt <= energyHold {
		if kw, why := boilerKW(o, m.prev); why == "" {
			m.totalKWh += kw * dt.Hours()
			m.updated = s.At
		}
	}
	m.prev = s
}

// noteBoiler feeds the meter with the boiler state of the engine after a frame.
func (d *daemon) noteBoiler(e *tado.EmuState) {
	var o tado.Options
	if in := d.inst.Load(); in != nil {
		o = in.Options
	}
	d.energy.observe(e.BoilerSample(), o)
}

// restoreEnergy loads the stored counter before the engine starts.
func (d *daemon) restoreEnergy() error {
	if d.state == nil {
		return nil
	}
	kwh, err := d.state.BoilerEnergy(context.Background())
	if err != nil {
		return err
	}
	d.energy.totalKWh, d.energy.shownKWh = kwh, kwh
	return nil
}

// tickEnergy writes the counter when it moved and energySaveEvery has passed
// since the last write attempt. Without a database there is nothing to bound.
func (d *daemon) tickEnergy() {
	m := &d.energy
	if m.totalKWh == m.shownKWh || d.state != nil && d.retryNow().Sub(m.saved) < energySaveEvery {
		return
	}
	if err := d.saveEnergy(); err != nil {
		log.Printf("persist boiler energy: %v", err)
	}
}

// saveEnergy writes the counter now.
func (d *daemon) saveEnergy() error {
	m := &d.energy
	m.saved = d.retryNow()
	if d.state != nil {
		if err := d.state.SetBoilerEnergy(context.Background(), m.totalKWh); err != nil {
			return err
		}
	}
	m.shownKWh = m.totalKWh
	return nil
}

// saveEnergyBefore writes the counter on the engine goroutine, for the shutdown.
func (d *daemon) saveEnergyBefore(deadline time.Time) error {
	timeout := time.Until(deadline)
	if timeout <= 0 {
		return errEngineBusy
	}
	_, err := d.doWithin(timeout, func(*tado.EmuState) (any, error) { return nil, d.saveEnergy() })
	return err
}

// energyView renders the estimate for GET /api/boiler: whether it runs and, if
// not, why; the output now; the counter; and the times of the last report and
// of the last stretch integrated.
func (d *daemon) energyView(now time.Time) map[string]any {
	m := &d.energy
	var o tado.Options
	if in := d.inst.Load(); in != nil {
		o = in.Options
	}
	maxKW, minKW, hasMax := o.BoilerPower()
	var kw float64
	var why string
	switch {
	case !hasMax:
		why = energyNoMaxPower
	case m.prev.At.IsZero():
		why = energyNoReport
	case now.Sub(m.prev.At) > energyHold:
		why = energyStale
	default:
		kw, why = boilerKW(o, m.prev)
	}
	view := map[string]any{
		"running": why == "", "reason": nil, "powerKW": round3(kw), "totalKWh": round3(m.shownKWh),
		"maxPowerKW": nil, "minPowerKW": minKW, "holdSec": int(energyHold / time.Second),
		"reportedAt": nil, "updatedAt": nil,
	}
	if why != "" {
		view["reason"] = why
	}
	if hasMax {
		view["maxPowerKW"] = maxKW
	}
	if !m.prev.At.IsZero() {
		view["reportedAt"] = m.prev.At.UTC().Format(time.RFC3339)
	}
	if !m.updated.IsZero() {
		view["updatedAt"] = m.updated.UTC().Format(time.RFC3339)
	}
	return view
}

// round3 rounds to the Wh. A rounded counter stays monotonic.
func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

// BoilerEnergy returns the stored counter in kWh, 0 when none is stored.
func (s *stateStore) BoilerEnergy(ctx context.Context) (float64, error) {
	var raw string
	switch err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, boilerEnergyKey).Scan(&raw); {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, err
	}
	kwh, err := strconv.ParseFloat(raw, 64)
	if err != nil || kwh < 0 || math.IsNaN(kwh) || math.IsInf(kwh, 0) {
		return 0, fmt.Errorf("meta %s: %q is not a counter", boilerEnergyKey, raw)
	}
	return kwh, nil
}

// SetBoilerEnergy stores the counter.
func (s *stateStore) SetBoilerEnergy(ctx context.Context, kwh float64) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO meta (key, value) VALUES (?, ?)`,
		boilerEnergyKey, strconv.FormatFloat(kwh, 'f', -1, 64))
	return err
}
