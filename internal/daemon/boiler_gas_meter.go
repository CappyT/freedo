package daemon

// boiler_gas_meter: the real gas meter the user reads now and then, recorded
// against the fuel counter of boiler_energy.go. The reading is in fuel_unit,
// the unit of energy.gas.totalM3. The daemon keeps the last readings and serves
// an estimate of the meter value that follows the fuel counter between
// readings. Two readings learn a factor that corrects the meter value only:
// energy.gas.totalM3 and flowM3H stay the raw estimate, so the Home Assistant
// energy and gas sensors do not change.
//
// The meter differs from the estimate for reasons the estimate cannot see:
// other gas users on the same meter (a hob), the efficiency the fuel counter
// does not model and the modulation the counter references. That is what the
// learned factor absorbs.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"time"

	"freedo/tado"
)

const (
	// gasMeterKey holds the readings in the meta table, as JSON.
	gasMeterKey = "boiler_gas_meter"
	// gasMeterMaxReadings is how many readings the daemon keeps: enough to see
	// the history of a replaceable meter, small enough for one meta row.
	gasMeterMaxReadings = 20
	// gasMeterMaxReading bounds a reading: no household meter is above it, so a
	// larger value is a typo.
	gasMeterMaxReading = 1e9
)

// errNoGasReading answers a DELETE with nothing to remove.
var errNoGasReading = errors.New("no meter reading is stored")

// meterSaveError marks a write of the readings that failed, for the handler to
// answer 500 instead of 400.
type meterSaveError struct{ err error }

func (e *meterSaveError) Error() string { return "persist the gas meter: " + e.err.Error() }
func (e *meterSaveError) Unwrap() error { return e.err }

// gasReading is one meter reading. At is the time the user took it, stored for
// display only. Estimate is the fuel counter at that moment, the baseline the
// meter value is extrapolated from.
type gasReading struct {
	At       time.Time `json:"at"`
	Reading  float64   `json:"reading"`
	Estimate float64   `json:"estimate"`
}

func (m *energyMeter) lastGasReading() *gasReading {
	if len(m.gasReadings) == 0 {
		return nil
	}
	return &m.gasReadings[len(m.gasReadings)-1]
}

// addGasReading appends a reading and drops the oldest beyond the cap.
func (m *energyMeter) addGasReading(r gasReading) {
	m.gasReadings = append(m.gasReadings, r)
	if len(m.gasReadings) > gasMeterMaxReadings {
		m.gasReadings = m.gasReadings[len(m.gasReadings)-gasMeterMaxReadings:]
	}
}

// gasFactor learns the correction from the two most recent readings whose
// estimate is at least one unit apart: factor = meter delta / estimate delta.
// It returns 1 and 0 readings while there is no such pair or while the factor
// is implausible, so a bad pair never corrects the meter.
func gasFactor(rs []gasReading) (float64, int) {
	if len(rs) < 2 {
		return 1, 0
	}
	last := rs[len(rs)-1]
	for i := len(rs) - 2; i >= 0; i-- {
		delta := last.Estimate - rs[i].Estimate
		if delta < 1 {
			continue
		}
		factor := (last.Reading - rs[i].Reading) / delta
		if factor >= 0.5 && factor <= 2.0 {
			return factor, 2
		}
		return 1, 0
	}
	return 1, 0
}

// gasMeterView renders energy.gas.meter: the estimated meter value that follows
// the fuel counter from the last reading, the last reading and its time, the
// count of readings kept, and the learned factor. It is null while no reading
// is stored.
func (d *daemon) gasMeterView() map[string]any {
	rs := d.energy.gasReadings
	if len(rs) == 0 {
		return nil
	}
	last := rs[len(rs)-1]
	factor, n := gasFactor(rs)
	return map[string]any{
		"value":          round3(last.Reading + factor*(d.energy.totalM3-last.Estimate)),
		"lastReading":    last.Reading,
		"lastReadingAt":  last.At.UTC().Format(time.RFC3339),
		"readings":       len(rs),
		"factor":         factor,
		"factorReadings": n,
	}
}

// saveGasMeter writes the readings now. The reading is written at once, unlike
// the fuel counter, because the user typed it and must see it kept.
func (d *daemon) saveGasMeter() error {
	if d.state == nil {
		return nil
	}
	return d.state.SetGasMeterReadings(context.Background(), d.energy.gasReadings)
}

// postGasMeter records a meter reading: {"reading": number} in fuel_unit, with
// an optional "estimate" (the fuel counter when the reading was taken, for a
// reading from a while ago) and an optional "at" (RFC3339, display only). A
// reading below the last one is refused, unless "replaced": true marks a new
// meter: it drops every stored reading first and starts the factor over.
func (d *daemon) postGasMeter(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Reading  *float64 `json:"reading"`
		Estimate *float64 `json:"estimate"`
		At       *string  `json:"at"`
		Replaced bool     `json:"replaced"`
	}
	if !decodeBody(w, r, &b) {
		return
	}
	if b.Reading == nil {
		writeErr(w, http.StatusBadRequest, "reading is required")
		return
	}
	reading := *b.Reading
	if math.IsNaN(reading) || math.IsInf(reading, 0) || reading < 0 || reading > gasMeterMaxReading {
		writeErr(w, http.StatusBadRequest, "reading must be a number from 0 to 1e9")
		return
	}
	var at time.Time
	if b.At != nil {
		ts, err := time.Parse(time.RFC3339, *b.At)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "at must be an RFC3339 timestamp")
			return
		}
		at = ts
	}
	var o tado.Options
	if in := d.inst.Load(); in != nil {
		o = in.Options
	}
	if _, _, ok := o.BoilerGas(); !ok {
		writeErr(w, http.StatusServiceUnavailable,
			"the fuel counter does not run: set boiler_max_gas_m3h and boiler_min_gas_m3h")
		return
	}
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		now := d.retryNow()
		when := at
		if when.IsZero() {
			when = now
		}
		if when.After(now) {
			return nil, errors.New("at is in the future")
		}
		counter := d.energy.totalM3
		estimate := counter
		if b.Estimate != nil {
			estimate = *b.Estimate
			if math.IsNaN(estimate) || math.IsInf(estimate, 0) || estimate < 0 || estimate > counter {
				return nil, fmt.Errorf("estimate must be from 0 up to the current counter (%.3f)", counter)
			}
		}
		if b.Replaced {
			d.energy.gasReadings = nil
		}
		if last := d.energy.lastGasReading(); last != nil && reading < last.Reading {
			return nil, fmt.Errorf("reading %.3f is below the previous reading %.3f: pass \"replaced\": true for a replaced meter", reading, last.Reading)
		}
		d.energy.addGasReading(gasReading{At: when.UTC(), Reading: reading, Estimate: estimate})
		if err := d.saveGasMeter(); err != nil {
			return nil, &meterSaveError{err}
		}
		return d.gasMeterView(), nil
	})
	if err != nil {
		var se *meterSaveError
		if errors.As(err, &se) {
			writeErr(w, http.StatusInternalServerError, se.Error())
			return
		}
		if writeHandlerErr(w, err) {
			return
		}
	}
	writeJSON(w, http.StatusOK, v)
}

// deleteGasMeter removes the last reading, to undo a typo.
func (d *daemon) deleteGasMeter(w http.ResponseWriter, r *http.Request) {
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		if len(d.energy.gasReadings) == 0 {
			return nil, errNoGasReading
		}
		d.energy.gasReadings = d.energy.gasReadings[:len(d.energy.gasReadings)-1]
		if err := d.saveGasMeter(); err != nil {
			return nil, &meterSaveError{err}
		}
		return d.gasMeterView(), nil
	})
	if errors.Is(err, errNoGasReading) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		var se *meterSaveError
		if errors.As(err, &se) {
			writeErr(w, http.StatusInternalServerError, se.Error())
			return
		}
		if writeHandlerErr(w, err) {
			return
		}
	}
	writeJSON(w, http.StatusOK, v)
}

// GasMeterReadings returns the stored meter readings, nil while none is stored.
func (s *stateStore) GasMeterReadings(ctx context.Context) ([]gasReading, error) {
	var raw string
	switch err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, gasMeterKey).Scan(&raw); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, err
	}
	var rs []gasReading
	if err := json.Unmarshal([]byte(raw), &rs); err != nil {
		return nil, fmt.Errorf("meta %s: %w", gasMeterKey, err)
	}
	return rs, nil
}

// SetGasMeterReadings stores the meter readings as one JSON row.
func (s *stateStore) SetGasMeterReadings(ctx context.Context, rs []gasReading) error {
	b, err := json.Marshal(rs)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT OR REPLACE INTO meta (key, value) VALUES (?, ?)`, gasMeterKey, string(b))
	return err
}
