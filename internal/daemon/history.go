package daemon

// history: the optional statistics. Every historyEvery the engine writes one
// sample per zone and one of the boiler, adds the fuel and the heat of the
// step to the total of the local day, and keeps the summary of each finished
// heat-up episode. The rows live in a SQLite file of their own beside the
// state database: the backup export copies the state database and stays
// small, the history holds no network key, and deleting it removes one file.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"freedo/internal/install"
	"freedo/tado"

	_ "modernc.org/sqlite"
)

const (
	historyEvery = 5 * time.Minute
	// historySampleDays bounds the samples and the episodes. historyDayMonths
	// bounds the daily totals: the current calendar month and the 12 before.
	historySampleDays = 90
	historyDayMonths  = 13
	// historyGapMax is the longest step whose counter delta still goes to a
	// day. The counters run while the history is off, so the step after a
	// longer gap holds fuel that belongs to no day the history saw.
	historyGapMax      = time.Hour
	historyWriteBudget = 2 * time.Second
	historySchema      = "1"
	historyMaxPrice    = 1000.0

	historyEnabledKey      = "history_enabled"
	historyPriceKey        = "history_fuel_price"
	historyCurrencyKey     = "history_currency"
	historyDefaultCurrency = "EUR"
)

var (
	currencyCode       = regexp.MustCompile(`^[A-Z]{3}$`)
	errHistoryOff      = errors.New("the statistics are off: turn them on with PUT /api/history")
	errHistoryNoDB     = errors.New("the statistics need the state database (--state)")
	errHistoryDeleteOn = errors.New("delete needs the statistics off: send \"enabled\": false with it")
)

const historyDDL = `
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS zone_sample (
	zone INTEGER NOT NULL,
	at INTEGER NOT NULL,
	temp_c REAL,
	target_c REAL,
	is_on INTEGER NOT NULL,
	humidity_pct REAL,
	demand_pct INTEGER,
	window_open INTEGER NOT NULL,
	away INTEGER NOT NULL,
	PRIMARY KEY (zone, at)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS boiler_sample (
	at INTEGER PRIMARY KEY,
	flow_c REAL,
	modulation_pct INTEGER,
	burner INTEGER,
	fuel REAL NOT NULL,
	heat_kwh REAL NOT NULL
);
CREATE TABLE IF NOT EXISTS day_total (
	day TEXT PRIMARY KEY,
	fuel REAL NOT NULL,
	fuel_estimate REAL NOT NULL,
	heat_kwh REAL NOT NULL,
	burner_min INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS heat_episode (
	zone INTEGER NOT NULL,
	started_ms INTEGER NOT NULL,
	ended_ms INTEGER NOT NULL,
	start_c REAL NOT NULL,
	target_c REAL,
	crossed_ms INTEGER,
	dead_time_sec REAL,
	overshoot_c REAL,
	settling_sec REAL,
	end_reason TEXT NOT NULL,
	PRIMARY KEY (zone, started_ms)
);
`

// historySettings are the switch and the fuel price. They live in the state
// database, so they outlive a deleted history file and travel with a backup.
type historySettings struct {
	Enabled   bool
	FuelPrice *float64
	Currency  string
}

// zoneSample is one zone at one sample time. A nil field had no value: no
// fresh zone report, no humidity sensor, no number target.
type zoneSample struct {
	zone        int
	AtMs        int64    `json:"atMs"`
	TempC       *float64 `json:"tempC"`
	TargetC     *float64 `json:"targetC"`
	On          bool     `json:"on"`
	HumidityPct *float64 `json:"humidityPct"`
	DemandPct   *int     `json:"demandPct"`
	Window      bool     `json:"window"`
	Away        bool     `json:"away"`
}

// boilerSample is the boiler at one sample time, with the running counters of
// boiler_energy.go. Burner is nil for a boiler that reports no burner flag.
type boilerSample struct {
	FlowC         *float64
	ModulationPct *int
	Burner        *bool
	Fuel, HeatKWh float64
}

// heatSummary is a finished heat-up episode of plant_episode.go without its
// samples.
type heatSummary struct {
	StartedMs   int64    `json:"startedMs"`
	EndedMs     int64    `json:"endedMs"`
	StartC      float64  `json:"startC"`
	TargetC     *float64 `json:"targetC"`
	CrossedMs   *int64   `json:"crossedMs"`
	DeadTimeSec *float64 `json:"deadTimeSec"`
	OvershootC  *float64 `json:"overshootC"`
	SettlingSec *float64 `json:"settlingSec"`
	EndReason   string   `json:"endReason"`
}

// dayTotal is what one local day counted. Fuel is the estimate times the
// meter factor in force when the step was counted.
type dayTotal struct {
	Day          string  `json:"day"`
	Fuel         float64 `json:"fuel"`
	FuelEstimate float64 `json:"fuelEstimate"`
	HeatKWh      float64 `json:"heatKWh"`
	BurnerMin    int     `json:"burnerMin"`
}

// historyCounters are the counters of the last boiler sample written.
type historyCounters struct {
	at         time.Time
	fuel, heat float64
	ok         bool
}

type historyStore struct {
	db   *sql.DB
	path string
}

// historyPath names the history file beside the state database:
// freedo-state.db gives freedo-state-history.db.
func historyPath(statePath string) string {
	return strings.TrimSuffix(statePath, filepath.Ext(statePath)) + "-history.db"
}

func openHistory(path string) (*historyStore, error) {
	// synchronous(NORMAL): a power cut can lose the last samples, which the
	// next ones follow. The state database keeps FULL for what the user wrote.
	dsn := "file:" + install.DSNPath(path) +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)" +
		"&_pragma=journal_size_limit(1048576)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("history %s: %w", path, err)
	}
	if err := initHistory(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("history %s: %w", path, err)
	}
	return &historyStore{db: db, path: path}, nil
}

func initHistory(db *sql.DB) error {
	if _, err := db.Exec(historyDDL); err != nil {
		return err
	}
	var v string
	switch err := db.QueryRow(`SELECT value FROM meta WHERE key = 'schema'`).Scan(&v); {
	case errors.Is(err, sql.ErrNoRows):
		_, err = db.Exec(`INSERT INTO meta (key, value) VALUES ('schema', ?)`, historySchema)
		return err
	case err != nil:
		return err
	case v != historySchema:
		return fmt.Errorf("schema %q, this daemon reads %q", v, historySchema)
	}
	return nil
}

func (h *historyStore) Close() error { return h.db.Close() }

// removeHistory deletes the history file and its WAL side files.
func removeHistory(path string) error {
	for _, name := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// historySize is the size on disk of the history file and its WAL side files.
func historySize(path string) int64 {
	var size int64
	for _, name := range []string{path, path + "-wal", path + "-shm"} {
		if info, err := os.Stat(name); err == nil {
			size += info.Size()
		}
	}
	return size
}

// add writes the samples of one time and adds day to its daily total.
func (h *historyStore) add(ctx context.Context, at time.Time, zones []zoneSample, b boilerSample, day dayTotal) error {
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	sec := at.Unix()
	for _, z := range zones {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO zone_sample
			(zone, at, temp_c, target_c, is_on, humidity_pct, demand_pct, window_open, away)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			z.zone, sec, z.TempC, z.TargetC, z.On, z.HumidityPct, z.DemandPct, z.Window, z.Away); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO boiler_sample
		(at, flow_c, modulation_pct, burner, fuel, heat_kwh) VALUES (?, ?, ?, ?, ?, ?)`,
		sec, b.FlowC, b.ModulationPct, b.Burner, b.Fuel, b.HeatKWh); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO day_total (day, fuel, fuel_estimate, heat_kwh, burner_min)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (day) DO UPDATE SET fuel = fuel + excluded.fuel,
			fuel_estimate = fuel_estimate + excluded.fuel_estimate,
			heat_kwh = heat_kwh + excluded.heat_kwh, burner_min = burner_min + excluded.burner_min`,
		day.Day, day.Fuel, day.FuelEstimate, day.HeatKWh, day.BurnerMin); err != nil {
		return err
	}
	return tx.Commit()
}

func (h *historyStore) addEpisode(ctx context.Context, zone int, s heatSummary) error {
	_, err := h.db.ExecContext(ctx, `INSERT OR REPLACE INTO heat_episode
		(zone, started_ms, ended_ms, start_c, target_c, crossed_ms, dead_time_sec, overshoot_c, settling_sec, end_reason)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		zone, s.StartedMs, s.EndedMs, s.StartC, s.TargetC, s.CrossedMs, s.DeadTimeSec, s.OvershootC, s.SettlingSec, s.EndReason)
	return err
}

// prune drops the samples and the episodes older than historySampleDays and
// the daily totals before the first month historyDayMonths keeps.
func (h *historyStore) prune(ctx context.Context, now time.Time, loc *time.Location) error {
	cut := now.AddDate(0, 0, -historySampleDays)
	local := now.In(loc)
	firstDay := time.Date(local.Year(), local.Month()-(historyDayMonths-1), 1, 0, 0, 0, 0, loc).Format(time.DateOnly)
	for _, q := range []struct {
		sql string
		arg any
	}{
		{`DELETE FROM zone_sample WHERE at < ?`, cut.Unix()},
		{`DELETE FROM boiler_sample WHERE at < ?`, cut.Unix()},
		{`DELETE FROM heat_episode WHERE started_ms < ?`, cut.UnixMilli()},
		{`DELETE FROM day_total WHERE day < ?`, firstDay},
	} {
		if _, err := h.db.ExecContext(ctx, q.sql, q.arg); err != nil {
			return err
		}
	}
	return nil
}

// lastBoiler returns the counters of the newest boiler sample.
func (h *historyStore) lastBoiler(ctx context.Context) (historyCounters, error) {
	var sec int64
	var c historyCounters
	err := h.db.QueryRowContext(ctx, `SELECT at, fuel, heat_kwh FROM boiler_sample ORDER BY at DESC LIMIT 1`).
		Scan(&sec, &c.fuel, &c.heat)
	if errors.Is(err, sql.ErrNoRows) {
		return historyCounters{}, nil
	}
	if err != nil {
		return historyCounters{}, err
	}
	c.at, c.ok = time.Unix(sec, 0), true
	return c, nil
}

// first returns the time of the oldest sample, of one zone when zone > 0.
func (h *historyStore) first(ctx context.Context, zone int) (time.Time, bool, error) {
	var sec sql.NullInt64
	var err error
	if zone > 0 {
		err = h.db.QueryRowContext(ctx, `SELECT MIN(at) FROM zone_sample WHERE zone = ?`, zone).Scan(&sec)
	} else {
		err = h.db.QueryRowContext(ctx, `SELECT MIN(at) FROM boiler_sample`).Scan(&sec)
	}
	if err != nil || !sec.Valid {
		return time.Time{}, false, err
	}
	return time.Unix(sec.Int64, 0), true, nil
}

func (h *historyStore) zoneSamples(ctx context.Context, zone int, from, to time.Time) ([]zoneSample, error) {
	rows, err := h.db.QueryContext(ctx, `SELECT at, temp_c, target_c, is_on, humidity_pct, demand_pct, window_open, away
		FROM zone_sample WHERE zone = ? AND at >= ? AND at < ? ORDER BY at`, zone, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []zoneSample{}
	for rows.Next() {
		var sec int64
		var temp, target, humidity sql.NullFloat64
		var demand sql.NullInt64
		s := zoneSample{zone: zone}
		if err := rows.Scan(&sec, &temp, &target, &s.On, &humidity, &demand, &s.Window, &s.Away); err != nil {
			return nil, err
		}
		s.AtMs = sec * 1000
		s.TempC, s.TargetC, s.HumidityPct = nullFloat(temp), nullFloat(target), nullFloat(humidity)
		if demand.Valid {
			v := int(demand.Int64)
			s.DemandPct = &v
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (h *historyStore) episodes(ctx context.Context, zone int, from, to time.Time) ([]heatSummary, error) {
	rows, err := h.db.QueryContext(ctx, `SELECT started_ms, ended_ms, start_c, target_c, crossed_ms, dead_time_sec,
		overshoot_c, settling_sec, end_reason FROM heat_episode
		WHERE zone = ? AND started_ms >= ? AND started_ms < ? ORDER BY started_ms`, zone, from.UnixMilli(), to.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []heatSummary{}
	for rows.Next() {
		var s heatSummary
		var target, dead, overshoot, settling sql.NullFloat64
		var crossed sql.NullInt64
		if err := rows.Scan(&s.StartedMs, &s.EndedMs, &s.StartC, &target, &crossed, &dead, &overshoot, &settling,
			&s.EndReason); err != nil {
			return nil, err
		}
		s.TargetC, s.DeadTimeSec, s.OvershootC, s.SettlingSec =
			nullFloat(target), nullFloat(dead), nullFloat(overshoot), nullFloat(settling)
		if crossed.Valid {
			s.CrossedMs = &crossed.Int64
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (h *historyStore) days(ctx context.Context, from, to string) ([]dayTotal, error) {
	rows, err := h.db.QueryContext(ctx, `SELECT day, fuel, fuel_estimate, heat_kwh, burner_min FROM day_total
		WHERE day >= ? AND day <= ? ORDER BY day`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []dayTotal{}
	for rows.Next() {
		var t dayTotal
		if err := rows.Scan(&t.Day, &t.Fuel, &t.FuelEstimate, &t.HeatKWh, &t.BurnerMin); err != nil {
			return nil, err
		}
		t.Fuel, t.FuelEstimate, t.HeatKWh = round3(t.Fuel), round3(t.FuelEstimate), round3(t.HeatKWh)
		out = append(out, t)
	}
	return out, rows.Err()
}

func nullFloat(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	return &v.Float64
}

// HistorySettings reads the statistics settings, off with the default currency
// when none are stored.
func (s *stateStore) HistorySettings(ctx context.Context) (historySettings, error) {
	out := historySettings{Currency: historyDefaultCurrency}
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM meta WHERE key IN (?, ?, ?)`,
		historyEnabledKey, historyPriceKey, historyCurrencyKey)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return out, err
		}
		switch key {
		case historyEnabledKey:
			out.Enabled = value == "1"
		case historyPriceKey:
			v, err := strconv.ParseFloat(value, 64)
			if err != nil || !validPrice(v) {
				return out, fmt.Errorf("meta %s: %q is not a price", key, value)
			}
			out.FuelPrice = &v
		case historyCurrencyKey:
			if !currencyCode.MatchString(value) {
				return out, fmt.Errorf("meta %s: %q is not a currency code", key, value)
			}
			out.Currency = value
		}
	}
	return out, rows.Err()
}

// SetHistorySettings stores the statistics settings in one transaction.
func (s *stateStore) SetHistorySettings(ctx context.Context, h historySettings) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	enabled := "0"
	if h.Enabled {
		enabled = "1"
	}
	set := `INSERT OR REPLACE INTO meta (key, value) VALUES (?, ?)`
	if _, err := tx.ExecContext(ctx, set, historyEnabledKey, enabled); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, set, historyCurrencyKey, h.Currency); err != nil {
		return err
	}
	if h.FuelPrice == nil {
		_, err = tx.ExecContext(ctx, `DELETE FROM meta WHERE key = ?`, historyPriceKey)
	} else {
		_, err = tx.ExecContext(ctx, set, historyPriceKey, strconv.FormatFloat(*h.FuelPrice, 'f', -1, 64))
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func validPrice(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= historyMaxPrice
}

// restoreHistory loads the settings and opens the file of an enabled history
// before the engine starts.
func (d *daemon) restoreHistory() error {
	if d.state == nil {
		return nil
	}
	cfg, err := d.state.HistorySettings(context.Background())
	if err != nil {
		return err
	}
	d.historyCfg = cfg
	if cfg.Enabled {
		d.openHistory()
	}
	return nil
}

// openHistory opens the file of an enabled history. A file it cannot open
// leaves the statistics without samples, not the daemon without a start: the
// reason shows in GET /api/history.
func (d *daemon) openHistory() {
	h, err := openHistory(historyPath(d.state.path))
	if err != nil {
		d.historyErr = err.Error()
		log.Printf("WARNING: statistics: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), historyWriteBudget)
	defer cancel()
	prev, err := h.lastBoiler(ctx)
	if err != nil {
		log.Printf("statistics: read the last sample: %v", err)
	}
	d.history, d.historyErr, d.historyPrev, d.historyDay = h, "", prev, ""
}

func (d *daemon) closeHistory() {
	if d.history != nil {
		if err := d.history.Close(); err != nil {
			log.Printf("statistics: close: %v", err)
		}
	}
	d.history, d.historyErr = nil, ""
}

// closeHistoryBefore closes the history on the engine goroutine, for the
// shutdown.
func (d *daemon) closeHistoryBefore(deadline time.Time) error {
	timeout := time.Until(deadline)
	if timeout <= 0 {
		return errEngineBusy
	}
	_, err := d.doWithin(timeout, func(*tado.EmuState) (any, error) { d.closeHistory(); return nil, nil })
	return err
}

// tickHistory writes the samples of the time slot that began last, once.
func (d *daemon) tickHistory(e *tado.EmuState) {
	h, inst := d.history, d.inst.Load()
	if h == nil || inst == nil {
		return
	}
	now := d.retryNow()
	slot := now.Truncate(historyEvery)
	if !slot.After(d.historySlot) {
		return
	}
	d.historySlot = slot
	snap := e.Snapshot()
	zones := d.historyZones(e, inst, snap, now)
	b := historyBoiler(snap.Boiler, d.energy.totalM3, d.energy.totalKWh)
	loc := d.programClock(inst)
	day := dayTotal{Day: slot.In(loc).Format(time.DateOnly)}
	if p := d.historyPrev; p.ok && slot.Sub(p.at) <= historyGapMax {
		factor, _ := gasFactor(d.energy.gasReadings)
		fuel := max(0, b.Fuel-p.fuel)
		day.FuelEstimate, day.Fuel, day.HeatKWh = fuel, fuel*factor, max(0, b.HeatKWh-p.heat)
	}
	if b.Burner != nil && *b.Burner {
		day.BurnerMin = int(historyEvery / time.Minute)
	}
	ctx, cancel := context.WithTimeout(context.Background(), historyWriteBudget)
	defer cancel()
	if err := h.add(ctx, slot, zones, b, day); err != nil {
		log.Printf("statistics: write the sample: %v", err)
		return
	}
	d.historyPrev = historyCounters{at: slot, fuel: b.Fuel, heat: b.HeatKWh, ok: true}
	if day.Day != d.historyDay {
		if err := h.prune(ctx, now, loc); err != nil {
			log.Printf("statistics: drop the old rows: %v", err)
			return
		}
		d.historyDay = day.Day
	}
}

// historyZones samples every zone the way the dashboard shows it: the room
// temperature of a fresh zone report, else of the measuring device of a zone
// that never reported; the humidity of the measuring device, else of the first
// member that reports one; the target of zoneTargetView.
func (d *daemon) historyZones(e *tado.EmuState, inst *tado.Install, snap tado.Snapshot, now time.Time) []zoneSample {
	reports := make(map[int]tado.ZoneView, len(snap.Zones))
	for _, z := range d.roomZoneViews(snap.Zones) {
		reports[z.Number] = z
	}
	valves := make(map[string]tado.ValveView, len(snap.Valves))
	for _, v := range snap.Valves {
		valves[v.Short] = v
	}
	out := make([]zoneSample, 0, len(inst.Zones))
	for i := range inst.Zones {
		z := &inst.Zones[i]
		view := d.zoneTargetView(e, z)
		s := zoneSample{zone: z.Number, Away: d.away, Window: view["source"] == "window"}
		s.On, _ = view["on"].(bool)
		if c, ok := view["targetC"].(float64); ok && s.On {
			s.TargetC = &c
		}
		switch z.Kind {
		case tado.ZoneHotWater:
			if t := snap.Boiler.DHWC; t != nil {
				c := *t
				s.TempC = &c
			}
		case tado.ZoneHeating:
			if rep, ok := reports[z.Number]; ok && rep.UpdatedUnix > 0 {
				if now.Sub(time.Unix(rep.UpdatedUnix, 0)) <= inst.Options.OfflineAfter() {
					if rep.TempC > 0 {
						c := rep.TempC
						s.TempC = &c
					}
					demand := rep.DemandPct
					s.DemandPct = &demand
				}
			} else if sensor, err := inst.MeasuringDevice(z.Number); err == nil {
				if v, ok := valves[sensor.Short()]; ok && v.TempC > 0 {
					c := v.TempC
					s.TempC = &c
				}
			}
			s.HumidityPct = historyHumidity(inst, z.Number, valves)
		}
		out = append(out, s)
	}
	return out
}

func historyHumidity(inst *tado.Install, zone int, valves map[string]tado.ValveView) *float64 {
	var shorts []string
	if sensor, err := inst.MeasuringDevice(zone); err == nil {
		shorts = append(shorts, sensor.Short())
	}
	if members, err := inst.ZoneMembers(zone); err == nil {
		for _, m := range members {
			shorts = append(shorts, m.Short())
		}
	}
	for _, short := range shorts {
		if v, ok := valves[short]; ok && v.HumidityPct > 0 {
			h := v.HumidityPct
			return &h
		}
	}
	return nil
}

func historyBoiler(b tado.BoilerView, fuel, heat float64) boilerSample {
	s := boilerSample{FlowC: b.FlowC, ModulationPct: b.ModulationPct, Fuel: fuel, HeatKWh: heat}
	if b.Present && b.Mon["boiler_flag"] != nil {
		on := b.Active
		s.Burner = &on
	}
	return s
}

// historyEpisode copies a finished heat-up episode into the history.
func (d *daemon) historyEpisode(zone int, ep *plantEpisode) {
	if d.history == nil || len(ep.Samples) == 0 {
		return
	}
	s := heatSummary{
		StartedMs: ep.StartedMs, EndedMs: ep.EndedMs, StartC: ep.Samples[0].TempC, TargetC: ep.TargetC,
		CrossedMs: ep.CrossedMs, DeadTimeSec: ep.DeadTimeSec, OvershootC: ep.OvershootC, SettlingSec: ep.SettlingSec,
		EndReason: ep.EndReason,
	}
	ctx, cancel := context.WithTimeout(context.Background(), historyWriteBudget)
	defer cancel()
	if err := d.history.addEpisode(ctx, zone, s); err != nil {
		log.Printf("statistics: write the zone %d heat-up: %v", zone, err)
	}
}

// historyView renders GET /api/history. It runs on the engine goroutine.
func (d *daemon) historyView() map[string]any {
	view := map[string]any{
		"available": d.state != nil, "enabled": d.historyCfg.Enabled, "error": nil, "since": nil, "sizeBytes": 0,
		"intervalSec": int(historyEvery / time.Second), "sampleDays": historySampleDays,
		"dayMonths": historyDayMonths, "fuelPrice": d.historyCfg.FuelPrice, "currency": d.historyCfg.Currency,
	}
	if d.historyCfg.Currency == "" {
		view["currency"] = historyDefaultCurrency
	}
	if d.historyErr != "" {
		view["error"] = d.historyErr
	}
	if d.state != nil {
		view["sizeBytes"] = historySize(historyPath(d.state.path))
	}
	if d.history != nil {
		ctx, cancel := context.WithTimeout(context.Background(), historyWriteBudget)
		defer cancel()
		if at, ok, err := d.history.first(ctx, 0); err == nil && ok {
			view["since"] = at.UTC().Format(time.RFC3339)
		}
	}
	return view
}

func (d *daemon) getHistory(w http.ResponseWriter, r *http.Request) {
	v, err := d.doReq(r, func(*tado.EmuState) (any, error) { return d.historyView(), nil })
	write(w, v, err)
}

// historyPut is the body of PUT /api/history. Each field is optional; delete
// removes the file and needs the statistics off.
type historyPut struct {
	Enabled   *bool    `json:"enabled"`
	Delete    bool     `json:"delete"`
	FuelPrice optFloat `json:"fuelPrice"`
	Currency  *string  `json:"currency"`
}

func (d *daemon) putHistory(w http.ResponseWriter, r *http.Request) {
	var b historyPut
	if !decodeBody(w, r, &b) {
		return
	}
	if b.Enabled == nil && !b.Delete && !b.FuelPrice.set && b.Currency == nil {
		writeErr(w, http.StatusBadRequest, "no statistics option given")
		return
	}
	if v := b.FuelPrice.value; v != nil && !validPrice(*v) {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("fuelPrice must be a number from 0 to %g, or null", historyMaxPrice))
		return
	}
	if b.Currency != nil && !currencyCode.MatchString(*b.Currency) {
		writeErr(w, http.StatusBadRequest, "currency must be an ISO 4217 code such as EUR")
		return
	}
	if d.state == nil {
		writeErr(w, http.StatusConflict, errHistoryNoDB.Error())
		return
	}
	v, err := d.doReq(r, func(*tado.EmuState) (any, error) {
		cfg := d.historyCfg
		if b.Enabled != nil {
			cfg.Enabled = *b.Enabled
		}
		if b.FuelPrice.set {
			cfg.FuelPrice = b.FuelPrice.value
		}
		if b.Currency != nil {
			cfg.Currency = *b.Currency
		}
		if cfg.Currency == "" {
			cfg.Currency = historyDefaultCurrency
		}
		if b.Delete && cfg.Enabled {
			return nil, errHistoryDeleteOn
		}
		ctx, cancel := context.WithTimeout(context.Background(), historyWriteBudget)
		defer cancel()
		if err := d.state.SetHistorySettings(ctx, cfg); err != nil {
			return nil, &stateSaveError{err}
		}
		d.historyCfg = cfg
		switch {
		case cfg.Enabled && d.history == nil:
			d.openHistory()
		case !cfg.Enabled:
			d.closeHistory()
		}
		if b.Delete {
			if err := removeHistory(historyPath(d.state.path)); err != nil {
				return nil, &stateSaveError{err}
			}
		}
		return d.historyView(), nil
	})
	if writeHandlerErr(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// historyHandle is what a read route takes from the engine: the open store,
// nil while off, and the settings. The query runs off the engine goroutine.
type historyHandle struct {
	store *historyStore
	cfg   historySettings
}

func (d *daemon) historyHandle(w http.ResponseWriter, r *http.Request) (historyHandle, bool) {
	v, err := d.doReq(r, func(*tado.EmuState) (any, error) {
		return historyHandle{store: d.history, cfg: d.historyCfg}, nil
	})
	if writeHandlerErr(w, err) {
		return historyHandle{}, false
	}
	hh := v.(historyHandle)
	if hh.store == nil {
		writeErr(w, http.StatusConflict, errHistoryOff.Error())
		return historyHandle{}, false
	}
	return hh, true
}

// historyDate parses a local day, today when raw is empty.
func historyDate(raw string, now time.Time, loc *time.Location) (time.Time, error) {
	if raw == "" {
		local := now.In(loc)
		return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc), nil
	}
	day, err := time.ParseInLocation(time.DateOnly, raw, loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("date %q is not YYYY-MM-DD", raw)
	}
	return day, nil
}

// getHistoryZone returns the samples and the heat-up episodes of one zone over
// one local day: ?date=YYYY-MM-DD, today by default.
func (d *daemon) getHistoryZone(w http.ResponseWriter, r *http.Request) {
	zone, ok := pathInt(w, r, "number")
	if !ok {
		return
	}
	inst := d.inst.Load()
	if inst == nil {
		writeResolveErr(w, errNoInstall)
		return
	}
	if _, err := inst.ZoneByNumber(zone); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	loc := programLocation(inst.Options)
	from, err := historyDate(r.URL.Query().Get("date"), d.retryNow(), loc)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	to := from.AddDate(0, 0, 1)
	hh, ok := d.historyHandle(w, r)
	if !ok {
		return
	}
	samples, err := hh.store.zoneSamples(r.Context(), zone, from, to)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	episodes, err := hh.store.episodes(r.Context(), zone, from, to)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var first any
	if at, ok, err := hh.store.first(r.Context(), zone); err == nil && ok {
		first = at.In(loc).Format(time.DateOnly)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"zone": zone, "date": from.Format(time.DateOnly), "from": from.UTC().Format(time.RFC3339),
		"to": to.UTC().Format(time.RFC3339), "intervalSec": int(historyEvery / time.Second), "first": first,
		"samples": samples, "episodes": episodes,
	})
}

// getHistoryEnergy returns the daily totals from ?from= to ?to= (YYYY-MM-DD,
// both included), by default the months historyDayMonths keeps.
func (d *daemon) getHistoryEnergy(w http.ResponseWriter, r *http.Request) {
	inst := d.inst.Load()
	var o tado.Options
	if inst != nil {
		o = inst.Options
	}
	loc := programLocation(o)
	now := d.retryNow().In(loc)
	from := time.Date(now.Year(), now.Month()-(historyDayMonths-1), 1, 0, 0, 0, 0, loc).Format(time.DateOnly)
	to := now.Format(time.DateOnly)
	q := r.URL.Query()
	for _, p := range []struct {
		name string
		dst  *string
	}{{"from", &from}, {"to", &to}} {
		raw := q.Get(p.name)
		if raw == "" {
			continue
		}
		if _, err := time.Parse(time.DateOnly, raw); err != nil {
			writeErr(w, http.StatusBadRequest, p.name+" "+strconv.Quote(raw)+" is not YYYY-MM-DD")
			return
		}
		*p.dst = raw
	}
	if from > to {
		writeErr(w, http.StatusBadRequest, "from is after to")
		return
	}
	hh, ok := d.historyHandle(w, r)
	if !ok {
		return
	}
	days, err := hh.store.days(r.Context(), from, to)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"from": from, "to": to, "unit": o.Fuel(), "fuelPrice": hh.cfg.FuelPrice, "currency": hh.cfg.Currency,
		"days": days,
	})
}
