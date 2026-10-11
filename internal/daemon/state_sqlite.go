package daemon

// state_sqlite: the durable desired state, one table per map of
// tado.DesiredState. tado.RestoreDesiredState stays the load gate: this store
// only moves rows. internal/install owns the database file and its schema.

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"freedo/internal/install"
	"freedo/tado"
)

// stateTables holds every table a save replaces, meta excluded.
var stateTables = []string{
	"child_lock", "temp_unit_f", "flow_c", "config_doc",
	"sched_doc", "zone_state", "utc_settled", "boost", "offline_schedule", "interface_module",
}

// stateStore adds the desired state to the installation store: both live in
// the same database, and db is the store's single connection.
type stateStore struct {
	*install.Store
	db *sql.DB
}

func openStateStore(path string) (*stateStore, error) {
	store, err := install.Open(path)
	if err != nil {
		return nil, err
	}
	return &stateStore{Store: store, db: store.DB()}, nil
}

// Save replaces the stored state with state in one transaction. seq orders the
// saves: a snapshot older than the stored one is skipped, not written, because
// two exporters can reach the commit out of order.
func (s *stateStore) Save(ctx context.Context, state tado.DesiredState, seq uint64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stored, found, err := install.ReadMetaUint(ctx, tx, "save_seq")
	if err != nil {
		return err
	}
	if found && seq <= stored {
		return nil
	}
	for _, table := range stateTables {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			return err
		}
	}
	for key, locked := range state.ChildLock {
		if _, err := tx.ExecContext(ctx, `INSERT INTO child_lock (key, locked) VALUES (?, ?)`, key, boolInt(locked)); err != nil {
			return err
		}
	}
	for key, fahrenheit := range state.TempUnitF {
		if _, err := tx.ExecContext(ctx, `INSERT INTO temp_unit_f (key, fahrenheit) VALUES (?, ?)`, key, boolInt(fahrenheit)); err != nil {
			return err
		}
	}
	for key, celsius := range state.FlowC {
		id, err := strconv.Atoi(key)
		if err != nil {
			return fmt.Errorf("flow circuit %q: %w", key, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO flow_c (zone_id, celsius) VALUES (?, ?)`, id, celsius); err != nil {
			return err
		}
	}
	for key, doc := range state.Config {
		if _, err := tx.ExecContext(ctx, `INSERT INTO config_doc (key, body, etag) VALUES (?, ?, ?)`,
			key, blob(doc.Body), blob(doc.ETag)); err != nil {
			return err
		}
	}
	for key, doc := range state.Schedule {
		if _, err := tx.ExecContext(ctx, `INSERT INTO sched_doc (key, body, etag) VALUES (?, ?, ?)`,
			key, blob(doc.Body), blob(doc.ETag)); err != nil {
			return err
		}
	}
	for key, body := range state.ZoneState {
		if _, err := tx.ExecContext(ctx, `INSERT INTO zone_state (key, body) VALUES (?, ?)`, key, blob(body)); err != nil {
			return err
		}
	}
	for key, value := range state.UTCSettled {
		if _, err := tx.ExecContext(ctx, `INSERT INTO utc_settled (key, value) VALUES (?, ?)`, key, value); err != nil {
			return err
		}
	}
	for key, enabled := range state.OfflineSchedule {
		if _, err := tx.ExecContext(ctx, `INSERT INTO offline_schedule (key, enabled) VALUES (?, ?)`, key, boolInt(enabled)); err != nil {
			return err
		}
	}
	for key, module := range state.InterfaceModule {
		if _, err := tx.ExecContext(ctx, `INSERT INTO interface_module (key, module) VALUES (?, ?)`, key, int64(module)); err != nil {
			return err
		}
	}
	for key, timer := range state.Boost {
		if _, err := tx.ExecContext(ctx, `INSERT INTO boost (key, id, expires_at_ms) VALUES (?, ?, ?)`,
			key, timer.ID, timer.ExpiresAt.UTC().UnixMilli()); err != nil {
			return err
		}
	}
	if err := saveBoostRules(ctx, tx, state.Boost); err != nil {
		return err
	}
	if err := saveSessions(ctx, tx, state.Session); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO meta (key, value) VALUES ('save_seq', ?)`,
		strconv.FormatUint(seq, 10)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO meta (key, value) VALUES ('state_version', ?)`,
		strconv.Itoa(tado.DesiredStateVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

// saveBoostRules stores the end rule of each timer in meta as one JSON value,
// so the boost table keeps its layout.
func saveBoostRules(ctx context.Context, tx *sql.Tx, timers map[string]tado.DesiredBoostTimer) error {
	rules := map[string]string{}
	for key, timer := range timers {
		if timer.Rule != "" {
			rules[key] = timer.Rule
		}
	}
	if len(rules) == 0 {
		_, err := tx.ExecContext(ctx, `DELETE FROM meta WHERE key = 'boost_rules'`)
		return err
	}
	raw, err := json.Marshal(rules)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO meta (key, value) VALUES ('boost_rules', ?)`, string(raw))
	return err
}

// loadBoostRules sets the stored rule on each timer of timers.
func (s *stateStore) loadBoostRules(timers map[string]tado.DesiredBoostTimer) error {
	var raw string
	switch err := s.db.QueryRow(`SELECT value FROM meta WHERE key = 'boost_rules'`).Scan(&raw); {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return err
	}
	var rules map[string]string
	if err := json.Unmarshal([]byte(raw), &rules); err != nil {
		return fmt.Errorf("meta boost_rules: %w", err)
	}
	for key, rule := range rules {
		if timer, ok := timers[key]; ok {
			timer.Rule = rule
			timers[key] = timer
		}
	}
	return nil
}

// saveSessions stores the served session ids in meta as one JSON value: the
// daemon reads them back whole at the start, so they need no table.
func saveSessions(ctx context.Context, tx *sql.Tx, sessions map[string]tado.DesiredSession) error {
	if len(sessions) == 0 {
		_, err := tx.ExecContext(ctx, `DELETE FROM meta WHERE key = 'sessions'`)
		return err
	}
	raw, err := json.Marshal(sessions)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO meta (key, value) VALUES ('sessions', ?)`, string(raw))
	return err
}

func (s *stateStore) loadSessions() (map[string]tado.DesiredSession, error) {
	out := map[string]tado.DesiredSession{}
	var raw string
	switch err := s.db.QueryRow(`SELECT value FROM meta WHERE key = 'sessions'`).Scan(&raw); {
	case errors.Is(err, sql.ErrNoRows):
		return out, nil
	case err != nil:
		return nil, err
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("meta sessions: %w", err)
	}
	return out, nil
}

// blob keeps a nil slice out of a BLOB NOT NULL column: database/sql binds a nil
// []byte as NULL.
func blob(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}

// Away returns the stored home state of the whole home. A database that holds
// none reads as home, which is the state the coordinator starts in.
func (s *stateStore) Away(ctx context.Context) (bool, error) {
	value, ok, err := install.ReadMetaUint(ctx, s.db, "away")
	if err != nil || !ok {
		return false, err
	}
	return value != 0, nil
}

// SetAway stores the home state of the whole home.
func (s *stateStore) SetAway(ctx context.Context, away bool) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO meta (key, value) VALUES ('away', ?)`,
		strconv.FormatInt(boolInt(away), 10))
	return err
}

// HotWaterProgram returns the stored hot-water program, nil when none is
// stored. It lives in meta as the hex of the weekly document: the daemon keeps
// it and never serves it, so it has no place among the served documents.
func (s *stateStore) HotWaterProgram(ctx context.Context) ([]tado.ScheduleProgram, error) {
	var raw string
	switch err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'hot_water_program'`).Scan(&raw); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, err
	}
	body, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("meta hot_water_program: %w", err)
	}
	programs, err := tado.DecodeSchedule(body)
	if err != nil {
		return nil, fmt.Errorf("meta hot_water_program: %w", err)
	}
	if err := validateHotWaterSchedule(programs); err != nil {
		return nil, fmt.Errorf("meta hot_water_program: %w", err)
	}
	return programs, nil
}

// SetHotWaterProgram stores the hot-water program.
func (s *stateStore) SetHotWaterProgram(ctx context.Context, programs []tado.ScheduleProgram) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO meta (key, value) VALUES ('hot_water_program', ?)`,
		hex.EncodeToString(tado.EncodeSchedulePrograms(programs)))
	return err
}

// storedBlock is the JSON of one scheduledBlock in meta program_blocks.
type storedBlock struct {
	C   float64 `json:"c"`
	Off bool    `json:"off,omitempty"`
}

// ProgramBlocks returns the block each zone took at the last tick or zone
// rewrite, and false when none is stored.
func (s *stateStore) ProgramBlocks(ctx context.Context) (map[int]scheduledBlock, bool, error) {
	var raw string
	switch err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'program_blocks'`).Scan(&raw); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, false, nil
	case err != nil:
		return nil, false, err
	}
	var stored map[int]storedBlock
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return nil, false, fmt.Errorf("meta program_blocks: %w", err)
	}
	out := make(map[int]scheduledBlock, len(stored))
	for zone, b := range stored {
		out[zone] = scheduledBlock{c: b.C, off: b.Off}
	}
	return out, true, nil
}

// SetCounterResume stores the counter resume points, keyed by meta key, in one
// transaction.
func (s *stateStore) SetCounterResume(ctx context.Context, resume map[string]uint64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for key, v := range resume {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO meta (key, value) VALUES (?, ?)`,
			key, strconv.FormatUint(v, 10)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// storedProgramWrite is the JSON of one programWrite in meta program_owed.
type storedProgramWrite struct {
	Zone   int    `json:"zone"`
	Target string `json:"target"`
}

// storedEarlyStart is the JSON of one earlyStart in meta early_starts.
type storedEarlyStart struct {
	AtMs       int64   `json:"atMs"`
	BoundaryMs int64   `json:"boundaryMs"`
	C          float64 `json:"c"`
	Off        bool    `json:"off,omitempty"`
}

// ProgramOwed returns the zone rewrites queued and not confirmed at the last
// save.
func (s *stateStore) ProgramOwed(ctx context.Context) ([]programWrite, error) {
	var stored []storedProgramWrite
	if err := s.metaJSON(ctx, "program_owed", &stored); err != nil {
		return nil, err
	}
	out := make([]programWrite, 0, len(stored))
	for _, w := range stored {
		out = append(out, programWrite{zone: w.Zone, target: w.Target})
	}
	return out, nil
}

// EarlyStarts returns the early starts running at the last save.
func (s *stateStore) EarlyStarts(ctx context.Context) (map[int]earlyStart, error) {
	var stored map[int]storedEarlyStart
	if err := s.metaJSON(ctx, "early_starts", &stored); err != nil {
		return nil, err
	}
	out := make(map[int]earlyStart, len(stored))
	for zone, es := range stored {
		out[zone] = earlyStart{
			at: time.UnixMilli(es.AtMs).UTC(), boundary: time.UnixMilli(es.BoundaryMs).UTC(),
			block: scheduledBlock{c: es.C, off: es.Off},
		}
	}
	return out, nil
}

// metaJSON decodes the JSON meta value of key into v, and leaves v alone when
// the key is not stored.
func (s *stateStore) metaJSON(ctx context.Context, key string, v any) error {
	var raw string
	switch err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&raw); {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return err
	}
	if err := json.Unmarshal([]byte(raw), v); err != nil {
		return fmt.Errorf("meta %s: %w", key, err)
	}
	return nil
}

// SetProgramState stores in one transaction the block each zone took, the
// zone rewrites still owed and the early starts running.
func (s *stateStore) SetProgramState(ctx context.Context, blocks map[int]scheduledBlock, owed []programWrite, early map[int]earlyStart) error {
	storedBlocks := make(map[int]storedBlock, len(blocks))
	for zone, b := range blocks {
		storedBlocks[zone] = storedBlock{C: b.c, Off: b.off}
	}
	storedOwed := make([]storedProgramWrite, 0, len(owed))
	for _, w := range owed {
		storedOwed = append(storedOwed, storedProgramWrite{Zone: w.zone, Target: w.target})
	}
	slices.SortFunc(storedOwed, func(a, b storedProgramWrite) int {
		return cmp.Or(cmp.Compare(a.Zone, b.Zone), strings.Compare(a.Target, b.Target))
	})
	storedEarly := make(map[int]storedEarlyStart, len(early))
	for zone, es := range early {
		storedEarly[zone] = storedEarlyStart{
			AtMs: es.at.UnixMilli(), BoundaryMs: es.boundary.UnixMilli(), C: es.block.c, Off: es.block.off,
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for key, v := range map[string]any{"program_blocks": storedBlocks, "program_owed": storedOwed, "early_starts": storedEarly} {
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO meta (key, value) VALUES (?, ?)`, key, string(raw)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// thermalModelPrefix starts the meta key of the heat-up model of a zone, stored
// as JSON. The daemon learns it and serves it to no device, like the hot-water
// program, so it needs no table of its own.
const thermalModelPrefix = "thermal_model."

// ThermalModels returns the stored heat-up model of every zone.
func (s *stateStore) ThermalModels(ctx context.Context) (map[int]*thermalModel, error) {
	out := make(map[int]*thermalModel)
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM meta WHERE key LIKE 'thermal\_model.%' ESCAPE '\'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key, raw string
		if err := rows.Scan(&key, &raw); err != nil {
			return nil, err
		}
		zone, err := strconv.Atoi(strings.TrimPrefix(key, thermalModelPrefix))
		if err != nil {
			return nil, fmt.Errorf("meta %s: %w", key, err)
		}
		var m thermalModel
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			return nil, fmt.Errorf("meta %s: %w", key, err)
		}
		out[zone] = &m
	}
	return out, rows.Err()
}

// SetThermalModel stores the heat-up model of zone.
func (s *stateStore) SetThermalModel(ctx context.Context, zone int, m *thermalModel) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT OR REPLACE INTO meta (key, value) VALUES (?, ?)`,
		thermalModelPrefix+strconv.Itoa(zone), string(raw))
	return err
}

// DeleteThermalModel forgets the heat-up model of zone.
func (s *stateStore) DeleteThermalModel(ctx context.Context, zone int) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM meta WHERE key = ?`, thermalModelPrefix+strconv.Itoa(zone))
	return err
}

// DeleteZoneLearning drops the heat-up model and the plant episodes of zone.
func (s *stateStore) DeleteZoneLearning(ctx context.Context, zone int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM meta WHERE key = ?`, thermalModelPrefix+strconv.Itoa(zone)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM plant_episode WHERE zone = ?`, zone); err != nil {
		return err
	}
	return tx.Commit()
}

// AddPlantEpisode stores one recorded episode of zone and drops the oldest of
// its kind past plantEpisodesKept, so the table stays bounded.
func (s *stateStore) AddPlantEpisode(ctx context.Context, zone int, ep *plantEpisode) error {
	raw, err := json.Marshal(ep)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO plant_episode (zone, kind, body) VALUES (?, ?, ?)`,
		zone, ep.Kind, string(raw)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM plant_episode WHERE zone = ? AND kind = ? AND id NOT IN
		(SELECT id FROM plant_episode WHERE zone = ? AND kind = ? ORDER BY id DESC LIMIT ?)`,
		zone, ep.Kind, zone, ep.Kind, plantEpisodesKept); err != nil {
		return err
	}
	return tx.Commit()
}

// PlantEpisodes returns the stored episodes of zone, oldest first.
func (s *stateStore) PlantEpisodes(ctx context.Context, zone int) ([]plantEpisode, error) {
	out := []plantEpisode{}
	rows, err := s.db.QueryContext(ctx, `SELECT id, body FROM plant_episode WHERE zone = ? ORDER BY id`, zone)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var raw string
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		var ep plantEpisode
		if err := json.Unmarshal([]byte(raw), &ep); err != nil {
			return nil, fmt.Errorf("plant episode %d: %w", id, err)
		}
		out = append(out, ep)
	}
	return out, rows.Err()
}

// ZoneWindows returns the stored zone window holds and the valve writes still
// owed. An unknown source is an error: the daemon must not guess who holds a
// zone open.
func (s *stateStore) ZoneWindows(ctx context.Context) (map[int]zoneWindow, []windowWrite, error) {
	out := make(map[int]zoneWindow)
	err := s.scan(`SELECT zone, source, since_ms, deadline_ms FROM zone_window`, func(rows *sql.Rows) error {
		var zone int
		var source string
		var since, deadline int64
		if err := rows.Scan(&zone, &source, &since, &deadline); err != nil {
			return err
		}
		src := windowSource(source)
		if src != windowDevice && src != windowExternal {
			return fmt.Errorf("zone %d window source %q is unknown", zone, source)
		}
		if out[zone] == nil {
			out[zone] = make(zoneWindow)
		}
		out[zone][src] = windowHold{since: time.UnixMilli(since).UTC(), deadline: time.UnixMilli(deadline).UTC()}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	var owed []windowWrite
	err = s.scan(`SELECT zone, target FROM zone_window_owed ORDER BY zone, target`, func(rows *sql.Rows) error {
		var w windowWrite
		if err := rows.Scan(&w.zone, &w.target); err != nil {
			return err
		}
		owed = append(owed, w)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return out, owed, nil
}

// SetZoneWindows replaces the stored zone window holds and owed writes in one
// transaction.
func (s *stateStore) SetZoneWindows(ctx context.Context, windows map[int]zoneWindow, owed []windowWrite) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, table := range []string{"zone_window", "zone_window_owed"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			return err
		}
	}
	for _, w := range owed {
		if _, err := tx.ExecContext(ctx, `INSERT INTO zone_window_owed (zone, target) VALUES (?, ?)`,
			w.zone, w.target); err != nil {
			return err
		}
	}
	for zone, w := range windows {
		for src, hold := range w {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO zone_window (zone, source, since_ms, deadline_ms) VALUES (?, ?, ?, ?)`,
				zone, string(src), hold.since.UTC().UnixMilli(), hold.deadline.UTC().UnixMilli()); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// Load reads the stored state. A database with no rows loads as an empty
// snapshot; any read failure is reported, because the daemon must not serve a
// state it could not read in full.
func (s *stateStore) Load() (tado.DesiredState, error) {
	if err := s.CheckSchema(); err != nil {
		return tado.DesiredState{}, err
	}
	version, err := s.stateVersion()
	if err != nil {
		return tado.DesiredState{}, err
	}
	out := tado.DesiredState{
		Version:         version,
		ChildLock:       map[string]bool{},
		TempUnitF:       map[string]bool{},
		FlowC:           map[string]float64{},
		Config:          map[string]tado.DesiredConfigDocument{},
		Schedule:        map[string]tado.DesiredScheduleDocument{},
		ZoneState:       map[string][]byte{},
		UTCSettled:      map[string]int{},
		Boost:           map[string]tado.DesiredBoostTimer{},
		OfflineSchedule: map[string]bool{},
		InterfaceModule: map[string]uint16{},
	}
	if out.Session, err = s.loadSessions(); err != nil {
		return tado.DesiredState{}, err
	}
	if err := s.scan(`SELECT key, locked FROM child_lock`, func(rows *sql.Rows) error {
		var key string
		var locked int64
		if err := rows.Scan(&key, &locked); err != nil {
			return err
		}
		out.ChildLock[key] = locked != 0
		return nil
	}); err != nil {
		return tado.DesiredState{}, err
	}
	if err := s.scan(`SELECT key, fahrenheit FROM temp_unit_f`, func(rows *sql.Rows) error {
		var key string
		var fahrenheit int64
		if err := rows.Scan(&key, &fahrenheit); err != nil {
			return err
		}
		out.TempUnitF[key] = fahrenheit != 0
		return nil
	}); err != nil {
		return tado.DesiredState{}, err
	}
	if err := s.scan(`SELECT zone_id, celsius FROM flow_c`, func(rows *sql.Rows) error {
		var id int
		var celsius float64
		if err := rows.Scan(&id, &celsius); err != nil {
			return err
		}
		out.FlowC[strconv.Itoa(id)] = celsius
		return nil
	}); err != nil {
		return tado.DesiredState{}, err
	}
	if err := s.scan(`SELECT key, body, etag FROM config_doc`, func(rows *sql.Rows) error {
		var key string
		var body, etag []byte
		if err := rows.Scan(&key, &body, &etag); err != nil {
			return err
		}
		out.Config[key] = tado.DesiredConfigDocument{Body: body, ETag: etag}
		return nil
	}); err != nil {
		return tado.DesiredState{}, err
	}
	if err := s.scan(`SELECT key, body, etag FROM sched_doc`, func(rows *sql.Rows) error {
		var key string
		var body, etag []byte
		if err := rows.Scan(&key, &body, &etag); err != nil {
			return err
		}
		out.Schedule[key] = tado.DesiredScheduleDocument{Body: body, ETag: etag}
		return nil
	}); err != nil {
		return tado.DesiredState{}, err
	}
	if err := s.scan(`SELECT key, body FROM zone_state`, func(rows *sql.Rows) error {
		var key string
		var body []byte
		if err := rows.Scan(&key, &body); err != nil {
			return err
		}
		out.ZoneState[key] = body
		return nil
	}); err != nil {
		return tado.DesiredState{}, err
	}
	if err := s.scan(`SELECT key, value FROM utc_settled`, func(rows *sql.Rows) error {
		var key string
		var value int
		if err := rows.Scan(&key, &value); err != nil {
			return err
		}
		out.UTCSettled[key] = value
		return nil
	}); err != nil {
		return tado.DesiredState{}, err
	}
	if err := s.scan(`SELECT key, enabled FROM offline_schedule`, func(rows *sql.Rows) error {
		var key string
		var enabled int64
		if err := rows.Scan(&key, &enabled); err != nil {
			return err
		}
		out.OfflineSchedule[key] = enabled != 0
		return nil
	}); err != nil {
		return tado.DesiredState{}, err
	}
	if err := s.scan(`SELECT key, module FROM interface_module`, func(rows *sql.Rows) error {
		var key string
		var module int64
		if err := rows.Scan(&key, &module); err != nil {
			return err
		}
		if module < 0 || module > 0xffff {
			return fmt.Errorf("interface module %s: %d is not a u16", key, module)
		}
		out.InterfaceModule[key] = uint16(module)
		return nil
	}); err != nil {
		return tado.DesiredState{}, err
	}
	if err := s.scan(`SELECT key, id, expires_at_ms FROM boost`, func(rows *sql.Rows) error {
		var key string
		var id int
		var expires int64
		if err := rows.Scan(&key, &id, &expires); err != nil {
			return err
		}
		out.Boost[key] = tado.DesiredBoostTimer{ID: id, ExpiresAt: time.UnixMilli(expires).UTC()}
		return nil
	}); err != nil {
		return tado.DesiredState{}, err
	}
	if err := s.loadBoostRules(out.Boost); err != nil {
		return tado.DesiredState{}, err
	}
	return out, nil
}

// stateVersion returns the snapshot version the last save stored. A database no
// save has touched yet carries the current version.
func (s *stateStore) stateVersion() (int, error) {
	var raw string
	switch err := s.db.QueryRow(`SELECT value FROM meta WHERE key = 'state_version'`).Scan(&raw); {
	case errors.Is(err, sql.ErrNoRows):
		return tado.DesiredStateVersion, nil
	case err != nil:
		return 0, err
	}
	version, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("meta state_version %q: %w", raw, err)
	}
	return version, nil
}

func (s *stateStore) scan(query string, row func(*sql.Rows) error) error {
	rows, err := s.db.Query(query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := row(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func boolInt(v bool) int64 {
	if v {
		return 1
	}
	return 0
}
