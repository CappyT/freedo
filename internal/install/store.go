package install

// store: the freedo database file. It holds the installation and the desired
// state, and a tool that opens it for the installation alone must still create
// or migrate the whole layout, so the schema of both lives here. The desired
// state rows are internal/daemon's.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"

	"freedo/tado"

	_ "modernc.org/sqlite"
)

// SchemaVersion moves when the table layout changes. The daemon refuses a
// database it cannot read as this layout instead of starting on a guess.
// Version "1" held the desired state alone, "2" adds the installation, "3"
// the open window columns of the zone, "4" the per-device offline schedule,
// "5" the zone window state and the window writes still owed, "6" the
// interface module id each wall thermostat reported, "7" the recorded plant
// episodes and "8" the program presets and the home and away programs of
// each zone.
const (
	SchemaVersion                = "8"
	stateSchemaNoInst            = "1"
	stateSchemaNoOpenWindow      = "2"
	stateSchemaNoOfflineSchedule = "3"
	stateSchemaNoZoneWindow      = "4"
	stateSchemaNoModule          = "5"
	stateSchemaNoPlantEpisode    = "6"
	stateSchemaNoPresets         = "7"
)

const stateSchemaDDL = `
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT);
CREATE TABLE IF NOT EXISTS child_lock (key TEXT PRIMARY KEY, locked INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS temp_unit_f (key TEXT PRIMARY KEY, fahrenheit INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS flow_c (zone_id INTEGER PRIMARY KEY, celsius REAL NOT NULL);
CREATE TABLE IF NOT EXISTS config_doc (key TEXT PRIMARY KEY, body BLOB NOT NULL, etag BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS sched_doc (key TEXT PRIMARY KEY, body BLOB NOT NULL, etag BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS zone_state (key TEXT PRIMARY KEY, body BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS utc_settled (key TEXT PRIMARY KEY, value INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS boost (key TEXT PRIMARY KEY, id INTEGER NOT NULL, expires_at_ms INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS offline_schedule (key TEXT PRIMARY KEY, enabled INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS interface_module (key TEXT PRIMARY KEY, module INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS zone_window (
	zone INTEGER NOT NULL,
	source TEXT NOT NULL,
	since_ms INTEGER NOT NULL,
	deadline_ms INTEGER NOT NULL,
	PRIMARY KEY (zone, source)
);
CREATE TABLE IF NOT EXISTS zone_window_owed (
	zone INTEGER NOT NULL,
	target TEXT NOT NULL,
	PRIMARY KEY (zone, target)
);
CREATE TABLE IF NOT EXISTS plant_episode (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	zone INTEGER NOT NULL,
	kind TEXT NOT NULL,
	body TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS preset (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL UNIQUE COLLATE NOCASE
);
CREATE TABLE IF NOT EXISTS zone_program (
	preset INTEGER NOT NULL REFERENCES preset(id),
	zone INTEGER NOT NULL,
	mode TEXT NOT NULL CHECK (mode IN ('home', 'away')),
	body BLOB NOT NULL,
	PRIMARY KEY (preset, zone, mode)
);
`

// Store is the freedo database. It serves the installation and the receiver
// calibration; the daemon reads and writes the desired state through DB.
type Store struct{ db *sql.DB }

// Open opens the freedo database: the desired state and the installation. It
// creates the file and the schema when they are missing, and refuses a
// database that is not a freedo store of this layout.
func Open(path string) (*Store, error) {
	// synchronous(FULL) costs one more fsync per commit than NORMAL. Saves follow
	// API mutations, so they are rare, and a committed one must survive a power
	// cut the way the fsync-then-rename of the previous JSON writer did.
	// foreign_keys is off per connection by default, and the installation tables
	// rest on it: a zone member has to name a device that exists.
	dsn := "file:" + DSNPath(path) +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)" +
		"&_pragma=journal_size_limit(1048576)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("state database %s: %w", path, err)
	}
	// Every save replaces the whole state, so one connection is enough and it
	// keeps the writers serialised.
	db.SetMaxOpenConns(1)
	if err := initStateSchema(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("state database %s: %w", path, err)
	}
	if err := dropRoomCorrections(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("state database %s: %w", path, err)
	}
	// SQLite creates the database and the two WAL side files under the process
	// umask. A file the operator widened on purpose still works, so a refused
	// chmod is a warning, not a startup failure.
	for _, name := range []string{path, path + "-wal", path + "-shm"} {
		info, err := os.Stat(name)
		if err != nil {
			continue
		}
		if info.Mode().Perm()&^0o600 == 0 {
			continue
		}
		if err := os.Chmod(name, 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "WARNING: %s holds the network key and stays readable by others: %v\n", name, err)
		}
	}
	return &Store{db: db}, nil
}

// DSNPath percent-encodes path for the file: URI. A raw ? or # would start
// the query or the fragment and open a different file.
func DSNPath(path string) string {
	parts := strings.Split(path, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

func initStateSchema(db *sql.DB) error {
	var tables int
	if err := db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`).Scan(&tables); err != nil {
		return err
	}
	// A file that already holds tables must prove it is a freedo store before the
	// DDL runs: a mistyped --state must not adopt somebody else's database.
	if tables > 0 {
		if err := migrateStateSchema(db); err != nil {
			return err
		}
		return checkStateSchema(db)
	}
	if _, err := db.Exec(stateSchemaDDL + installSchemaDDL); err != nil {
		return err
	}
	if _, err := db.Exec(`INSERT OR IGNORE INTO meta (key, value) VALUES ('schema_version', ?)`, SchemaVersion); err != nil {
		return err
	}
	return checkStateSchema(db)
}

// migrateStateSchema moves an older database forward in one transaction, so a
// crash before the commit leaves the old version to migrate again: version "1"
// gets the installation tables, version "2" the open window columns, every
// version before "4" the offline_schedule table and its schedules re-encoded,
// every version before "5" the zone_window and zone_window_owed tables, every
// version before "6" the interface_module table, every version before "7"
// the plant_episode table, and every version before "8" the preset and
// zone_program tables. A thermostat document
// stored without a learned id is dropped at load (tado.RestoreDesiredState).
// Every other desired-state row, save_seq and state_version stay as they are,
// and a zone that predates the columns takes the captured defaults. The
// installation's options.offline_schedule needs no step: an install table
// without the key reads as DefaultOfflineSchedule. The migration is one way.
func migrateStateSchema(db *sql.DB) error {
	var version string
	if err := db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&version); err != nil {
		return fmt.Errorf("not a freedo state database: %w", err)
	}
	var steps []string
	switch version {
	case stateSchemaNoInst:
		// The DDL creates the zone table with the open window columns already
		// in it, so the "2" step has nothing left to add.
		steps = append(steps, installSchemaDDL)
	case stateSchemaNoOpenWindow:
		steps = append(steps, zoneOpenWindowDDL)
	case stateSchemaNoOfflineSchedule, stateSchemaNoZoneWindow, stateSchemaNoModule, stateSchemaNoPlantEpisode, stateSchemaNoPresets:
	default:
		return nil
	}
	// Every statement of the state DDL is idempotent, so it adds the tables an
	// older version lacks and leaves the others alone.
	steps = append(steps, stateSchemaDDL)

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, ddl := range steps {
		if _, err := tx.Exec(ddl); err != nil {
			return err
		}
	}
	// A version "4" or later schedule already holds the current encoding: a
	// second re-encode would flip every enabled value back.
	if version != stateSchemaNoZoneWindow && version != stateSchemaNoModule &&
		version != stateSchemaNoPlantEpisode && version != stateSchemaNoPresets {
		if err := reencodeLegacySchedules(tx); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE meta SET value = ? WHERE key = 'schema_version'`, SchemaVersion); err != nil {
		return err
	}
	return tx.Commit()
}

// reencodeLegacySchedules rewrites every stored schedule written before
// version "4", whose entry field 3 meant "off", so each entry keeps its enabled
// value under the current encoding. The ETag is derived from the body, so it
// moves with it.
func reencodeLegacySchedules(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT key, body FROM sched_doc`)
	if err != nil {
		return err
	}
	type doc struct {
		key  string
		body []byte
	}
	var docs []doc
	for rows.Next() {
		var d doc
		if err := rows.Scan(&d.key, &d.body); err != nil {
			rows.Close()
			return err
		}
		docs = append(docs, d)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, d := range docs {
		body, etag, err := tado.ReencodeLegacySchedule(d.body)
		if err != nil {
			return fmt.Errorf("schedule %s: %w", d.key, err)
		}
		if _, err := tx.Exec(`UPDATE sched_doc SET body = ?, etag = ? WHERE key = ?`, body, etag, d.key); err != nil {
			return err
		}
	}
	return nil
}

// The values of the two room temperature corrections that freedo no longer
// runs: offset mode setpoint, which shifted the setpoints, and the automatic
// valve calibration under the install key auto_offset.<zone>.
const (
	legacyOffsetModeSetpoint = "setpoint"
	legacyAutoOffsetPrefix   = "auto_offset."
)

// ShiftReleaseKey is the meta key of the shifts, per zone in centi-degrees,
// that the valve setpoints of the zones dropRoomCorrections moved still carry.
// The daemon deletes it once it has queued and stored the writes without them.
const ShiftReleaseKey = "shift_release"

// metaIntMap decodes the meta value under key, a JSON map of zone to
// centi-degrees. A missing row, or a value that does not decode, gives nil.
func metaIntMap(ctx context.Context, tx *sql.Tx, key string) (map[int]int, error) {
	var raw string
	switch err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&raw); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, err
	}
	var m map[int]int
	if json.Unmarshal([]byte(raw), &m) != nil {
		return nil, nil
	}
	return m, nil
}

// dropRoomCorrections moves each zone in offset mode setpoint to the default
// mode config and deletes the auto_offset rows and meta room_shift and
// auto_offset, in one transaction, with one log line per changed zone. A
// changed row moves install_rev. The same transaction stores under
// ShiftReleaseKey the shift each moved zone still carries: the applied one in
// meta room_shift, else setpoint_centi. A database that holds none of the
// legacy rows stays as it is, so a second open does nothing and keeps the key.
func dropRoomCorrections(db *sql.DB) error {
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	kv, err := installValues(ctx, tx)
	if err != nil {
		return err
	}
	// A room_shift value that does not decode only loses the applied shift:
	// the zone falls back to setpoint_centi.
	roomShift, err := metaIntMap(ctx, tx, "room_shift")
	if err != nil {
		return err
	}
	var drop []string
	notes := map[int][]string{}
	released := map[int]int{}
	for key, value := range kv {
		if rest, ok := strings.CutPrefix(key, legacyAutoOffsetPrefix); ok {
			drop = append(drop, key)
			if zone, err := strconv.Atoi(rest); err == nil {
				notes[zone] = append(notes[zone], "auto_offset "+value+" removed")
			}
			continue
		}
		rest, ok := strings.CutPrefix(key, zoneOffsetPrefix)
		if !ok {
			continue
		}
		zone, err := strconv.Atoi(rest)
		if err != nil {
			continue
		}
		var off struct {
			Mode          string `json:"mode"`
			SetpointCenti int    `json:"setpoint_centi"`
		}
		if json.Unmarshal([]byte(value), &off) != nil || off.Mode != legacyOffsetModeSetpoint {
			continue
		}
		drop = append(drop, key)
		shift, on := roomShift[zone]
		if !on {
			shift = off.SetpointCenti
		}
		if shift != 0 {
			released[zone] = shift
		}
		notes[zone] = append(notes[zone],
			fmt.Sprintf("offset_mode setpoint (setpoint_centi %d, applied shift %d) is now config", off.SetpointCenti, shift))
	}
	for _, key := range drop {
		if _, err := tx.ExecContext(ctx, `DELETE FROM install WHERE key = ?`, key); err != nil {
			return err
		}
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM meta WHERE key IN ('room_shift', 'auto_offset')`)
	if err != nil {
		return err
	}
	metaDropped, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if len(drop) == 0 && metaDropped == 0 {
		return nil
	}
	if len(drop) > 0 {
		rev, _, err := ReadMetaUint(ctx, tx, "install_rev")
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO meta (key, value) VALUES ('install_rev', ?)`,
			strconv.FormatUint(rev+1, 10)); err != nil {
			return err
		}
	}
	if len(released) > 0 {
		// A release that a dead start left owed keeps its other zones.
		pending, err := metaIntMap(ctx, tx, ShiftReleaseKey)
		if err != nil {
			return err
		}
		if pending == nil {
			pending = map[int]int{}
		}
		maps.Copy(pending, released)
		raw, err := json.Marshal(pending)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO meta (key, value) VALUES (?, ?)`,
			ShiftReleaseKey, string(raw)); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, zone := range slices.Sorted(maps.Keys(notes)) {
		log.Printf("zone %d: %s: freedo no longer runs the setpoint shift and the valve calibration",
			zone, strings.Join(notes[zone], ", "))
	}
	return nil
}

func checkStateSchema(db *sql.DB) error {
	var got string
	if err := db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&got); err != nil {
		return fmt.Errorf("not a freedo state database: %w", err)
	}
	if got != SchemaVersion {
		return fmt.Errorf("schema version %q is unsupported, want %q", got, SchemaVersion)
	}
	return nil
}

// CheckSchema reports a database whose layout is not SchemaVersion.
func (s *Store) CheckSchema() error { return checkStateSchema(s.db) }

// SchemaStatus classifies a state database against the schema this binary
// reads.
type SchemaStatus int

const (
	// SchemaMissing: no file at the path.
	SchemaMissing SchemaStatus = iota
	// SchemaReadable: this binary reads the file, now or after a migration.
	SchemaReadable
	// SchemaNewer: the file holds a schema newer than this binary.
	SchemaNewer
)

// InspectSchema opens the database at path read-only and classifies its schema
// version: the file is missing, this binary reads it (the stored version is this
// one or older, and an older one migrates), or the stored version is newer than
// this binary. It returns the version as stored. It creates and migrates
// nothing.
func InspectSchema(path string) (SchemaStatus, string, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return SchemaMissing, "", nil
		}
		return SchemaMissing, "", err
	}
	db, err := sql.Open("sqlite", "file:"+DSNPath(path)+"?mode=ro")
	if err != nil {
		return SchemaMissing, "", err
	}
	defer db.Close()
	var got string
	if err := db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&got); err != nil {
		return SchemaMissing, "", fmt.Errorf("not a freedo state database: %w", err)
	}
	stored, err := strconv.Atoi(got)
	if err != nil {
		return SchemaMissing, got, fmt.Errorf("schema version %q is not a number", got)
	}
	want, err := strconv.Atoi(SchemaVersion)
	if err != nil {
		return SchemaMissing, got, err
	}
	if stored > want {
		return SchemaNewer, got, nil
	}
	return SchemaReadable, got, nil
}

// DB is the connection the desired state goes through.
func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) Close() error { return s.db.Close() }

// RowQuerier is what ReadMetaUint needs of a *sql.DB or a *sql.Tx: the
// installation reads the counter outside a transaction too.
type RowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// ReadMetaUint reads the meta counter key, and whether the database holds it.
func ReadMetaUint(ctx context.Context, q RowQuerier, key string) (uint64, bool, error) {
	var raw string
	switch err := q.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&raw); {
	case errors.Is(err, sql.ErrNoRows):
		return 0, false, nil
	case err != nil:
		return 0, false, err
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("meta %s %q: %w", key, raw, err)
	}
	return value, true, nil
}

// FSCTRL0 returns the passive receiver calibration, and whether the database
// holds one.
func (s *Store) FSCTRL0(ctx context.Context) (byte, bool, error) {
	value, ok, err := ReadMetaUint(ctx, s.db, "fsctrl0")
	if err != nil || !ok {
		return 0, false, err
	}
	if value > 0xFF {
		return 0, false, fmt.Errorf("meta fsctrl0 %d is not a register value", value)
	}
	return byte(value), true, nil
}

// SetFSCTRL0 stores the passive receiver calibration, so the next start seeds
// the radio with it.
func (s *Store) SetFSCTRL0(ctx context.Context, value byte) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO meta (key, value) VALUES ('fsctrl0', ?)`,
		strconv.FormatUint(uint64(value), 10))
	return err
}

// StoreFSCTRL0 writes the passive receiver calibration into the database at
// path, through the store code the daemon seeds from. A path that holds no
// database is refused: creating one here would put the calibration in a file
// the daemon never reads.
func StoreFSCTRL0(path string, value byte) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("state database %s: %w", path, err)
	}
	store, err := Open(path)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.SetFSCTRL0(context.Background(), value); err != nil {
		return fmt.Errorf("state database %s: %w", path, err)
	}
	return nil
}

func boolInt(v bool) int64 {
	if v {
		return 1
	}
	return 0
}
