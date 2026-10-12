package daemon

// boiler_energy: an estimate of the heat the boiler delivers and of the fuel it
// burns, integrated from the hvac/mon reports of the wall thermostat. Home
// Assistant turns the counters into an energy sensor and a gas sensor and
// prices them. The estimate has three error sources: the modulation is
// relative to the boiler's own range, the heat counter does not model the
// efficiency (it is heat output, not gas energy), and the reports are sparse,
// so a short burn or a change between two reports is approximated. The gas
// counter reads the gas flow range of the boiler data sheet and needs no
// efficiency.
//
// The thermostat reports only the attributes that changed. The estimate holds
// the modulation and the burner flag of a report until the next report, and it
// drops a stretch the thermostat left unreported for longer than energyHold.
// The boiler status flags that split heating from hot water are not reported:
// the counters are totals.

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
	boilerGasKey    = "boiler_gas_m3"
)

// Reasons the estimate does not run, as the API names them.
const (
	energyNoMaxPower   = "noMaxPower"
	energyNoReport     = "noReport"
	energyStale        = "stale"
	energyNoBurnerFlag = "noBurnerFlag"
	energyNoModulation = "noModulation"
	energyNoGasFlow    = "noGasFlow"
)

// energyMeter integrates the heat output and the gas flow between two reports.
// It belongs to the engine goroutine.
type energyMeter struct {
	// totalKWh and totalM3 count every integrated stretch. shownKWh and shownM3
	// are the part the database holds, and the only part the API serves: a
	// restart then never reads a counter below one a client has seen.
	totalKWh, shownKWh float64
	totalM3, shownM3   float64
	// prev is the last sample taken. updated is the time of the last stretch
	// integrated, savedAt the time of the last write attempt.
	prev           tado.BoilerSample
	updated, saved time.Time
	// gasReadings holds the meter readings the user took, oldest first. See
	// boiler_gas_meter.go.
	gasReadings []gasReading
}

// boilerRate returns the rate a sample implies on the range lo..hi of the
// boiler: the output in kW or the gas flow in m3/h. why names what is missing
// when the rate is unknown, and is empty otherwise. The modulation spans the
// boiler's range from its minimum to its maximum, so it is no plain share of
// the maximum: the captures show the burner on at 0 % and 3 %.
func boilerRate(s tado.BoilerSample, hi, lo float64) (rate float64, why string) {
	switch {
	case s.Burner == nil:
		return 0, energyNoBurnerFlag
	case !*s.Burner:
		return 0, ""
	case s.ModulationPct == nil:
		return 0, energyNoModulation
	}
	mod := float64(min(max(*s.ModulationPct, 0), 100))
	return lo + (hi-lo)*mod/100, ""
}

// boilerKW returns the heat output a sample implies, in kW.
func boilerKW(o tado.Options, s tado.BoilerSample) (kw float64, why string) {
	maxKW, minKW, ok := o.BoilerPower()
	if !ok {
		return 0, energyNoMaxPower
	}
	return boilerRate(s, maxKW, minKW)
}

// boilerGas returns the gas flow a sample implies, in m3/h.
func boilerGas(o tado.Options, s tado.BoilerSample) (flow float64, why string) {
	maxM3H, minM3H, ok := o.BoilerGas()
	if !ok {
		return 0, energyNoGasFlow
	}
	return boilerRate(s, maxM3H, minM3H)
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
		if flow, why := boilerGas(o, m.prev); why == "" {
			m.totalM3 += flow * dt.Hours()
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

// restoreEnergy loads the stored counters before the engine starts.
func (d *daemon) restoreEnergy() error {
	if d.state == nil {
		return nil
	}
	kwh, err := d.state.BoilerEnergy(context.Background())
	if err != nil {
		return err
	}
	m3, err := d.state.BoilerGas(context.Background())
	if err != nil {
		return err
	}
	readings, err := d.state.GasMeterReadings(context.Background())
	if err != nil {
		return err
	}
	d.energy.totalKWh, d.energy.shownKWh = kwh, kwh
	d.energy.totalM3, d.energy.shownM3 = m3, m3
	d.energy.gasReadings = readings
	return nil
}

// tickEnergy writes the counters when one moved and energySaveEvery has passed
// since the last write attempt. Without a database there is nothing to bound.
func (d *daemon) tickEnergy() {
	m := &d.energy
	if m.totalKWh == m.shownKWh && m.totalM3 == m.shownM3 || d.state != nil && d.retryNow().Sub(m.saved) < energySaveEvery {
		return
	}
	if err := d.saveEnergy(); err != nil {
		log.Printf("persist boiler energy: %v", err)
	}
}

// saveEnergy writes the counters now.
func (d *daemon) saveEnergy() error {
	m := &d.energy
	m.saved = d.retryNow()
	if d.state != nil {
		if err := d.state.SetBoilerEnergy(context.Background(), m.totalKWh); err != nil {
			return err
		}
		if err := d.state.SetBoilerGas(context.Background(), m.totalM3); err != nil {
			return err
		}
	}
	m.shownKWh, m.shownM3 = m.totalKWh, m.totalM3
	return nil
}

// saveEnergyBefore writes the counters on the engine goroutine, for the shutdown.
func (d *daemon) saveEnergyBefore(deadline time.Time) error {
	timeout := time.Until(deadline)
	if timeout <= 0 {
		return errEngineBusy
	}
	_, err := d.doWithin(timeout, func(*tado.EmuState) (any, error) { return nil, d.saveEnergy() })
	return err
}

// rateAt returns the rate the meter shows at now: the rate of the last sample,
// or why it is unknown. unset is the reason of: an option the rate needs is
// missing, which outranks the state of the reports.
func (m *energyMeter) rateAt(now time.Time, o tado.Options, of func(tado.Options, tado.BoilerSample) (float64, string), unset string) (float64, string) {
	rate, why := of(o, m.prev)
	switch {
	case why == unset:
	case m.prev.At.IsZero():
		return 0, energyNoReport
	case now.Sub(m.prev.At) > energyHold:
		return 0, energyStale
	}
	return rate, why
}

// energyView renders the estimate for GET /api/boiler: whether it runs and, if
// not, why; the output now; the counter; and the times of the last report and
// of the last stretch integrated. The gas count sits in its own object, with
// the same fields, because it runs on its own settings. Its field names keep
// the m3 of the first unit: the flow is in unit per hour and the total in unit,
// the unit being options.fuel_unit.
func (d *daemon) energyView(now time.Time) map[string]any {
	m := &d.energy
	var o tado.Options
	if in := d.inst.Load(); in != nil {
		o = in.Options
	}
	maxKW, minKW, hasMax := o.BoilerPower()
	kw, why := m.rateAt(now, o, boilerKW, energyNoMaxPower)
	flow, gasWhy := m.rateAt(now, o, boilerGas, energyNoGasFlow)
	gas := map[string]any{
		"running": gasWhy == "", "reason": nil, "flowM3H": round3(flow), "totalM3": round3(m.shownM3),
		"maxFlowM3H": nil, "minFlowM3H": nil, "unit": o.Fuel(), "meter": d.gasMeterView(),
	}
	if gasWhy != "" {
		gas["reason"] = gasWhy
	}
	if v := o.BoilerMaxGasM3H; v != nil {
		gas["maxFlowM3H"] = *v
	}
	if v := o.BoilerMinGasM3H; v != nil {
		gas["minFlowM3H"] = *v
	}
	view := map[string]any{
		"running": why == "", "reason": nil, "powerKW": round3(kw), "totalKWh": round3(m.shownKWh),
		"maxPowerKW": nil, "minPowerKW": minKW, "holdSec": int(energyHold / time.Second),
		"reportedAt": nil, "updatedAt": nil, "gas": gas,
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

// round3 rounds to the Wh or the litre. A rounded counter stays monotonic.
func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

// BoilerEnergy returns the stored heat counter in kWh, 0 when none is stored.
func (s *stateStore) BoilerEnergy(ctx context.Context) (float64, error) {
	return s.boilerCounter(ctx, boilerEnergyKey)
}

// SetBoilerEnergy stores the heat counter.
func (s *stateStore) SetBoilerEnergy(ctx context.Context, kwh float64) error {
	return s.setBoilerCounter(ctx, boilerEnergyKey, kwh)
}

// BoilerGas returns the stored gas volume counter in m3, 0 when none is stored.
func (s *stateStore) BoilerGas(ctx context.Context) (float64, error) {
	return s.boilerCounter(ctx, boilerGasKey)
}

// SetBoilerGas stores the gas volume counter.
func (s *stateStore) SetBoilerGas(ctx context.Context, m3 float64) error {
	return s.setBoilerCounter(ctx, boilerGasKey, m3)
}

func (s *stateStore) boilerCounter(ctx context.Context, key string) (float64, error) {
	var raw string
	switch err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&raw); {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, err
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("meta %s: %q is not a counter", key, raw)
	}
	return v, nil
}

func (s *stateStore) setBoilerCounter(ctx context.Context, key string, v float64) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO meta (key, value) VALUES (?, ?)`,
		key, strconv.FormatFloat(v, 'f', -1, 64))
	return err
}
