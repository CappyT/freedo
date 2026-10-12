package install

// install: the installation itself, in the same database as the desired
// state. The rows are the source of truth; the YAML only bootstraps the process.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"freedo/tado"
)

// installSchemaDDL adds the installation tables. Every statement is idempotent,
// so the same text serves a fresh database and the "1" -> "2" migration.
const installSchemaDDL = `
CREATE TABLE IF NOT EXISTS install (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS net_key (id INTEGER PRIMARY KEY CHECK (id = 1), key BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS device (
  eui    TEXT PRIMARY KEY,
  kind   TEXT NOT NULL,
  serial TEXT NOT NULL DEFAULT '',
  label  TEXT NOT NULL DEFAULT '',
  zone   INTEGER,
  notes  TEXT NOT NULL DEFAULT '',
  ord    INTEGER NOT NULL);
CREATE UNIQUE INDEX IF NOT EXISTS device_serial ON device(serial) WHERE serial <> '';
CREATE TABLE IF NOT EXISTS circuit (
  id          INTEGER PRIMARY KEY,
  name        TEXT NOT NULL DEFAULT '',
  heat_source TEXT NOT NULL REFERENCES device(eui));
CREATE TABLE IF NOT EXISTS zone (
  number                   INTEGER PRIMARY KEY,
  name                     TEXT NOT NULL DEFAULT '',
  kind                     TEXT NOT NULL,
  circuit                  INTEGER REFERENCES circuit(id),
  open_window_detection    INTEGER NOT NULL DEFAULT 1,
  open_window_duration_sec INTEGER NOT NULL DEFAULT 900);
CREATE TABLE IF NOT EXISTS zone_member (
  zone_number INTEGER NOT NULL REFERENCES zone(number) ON DELETE CASCADE,
  device_eui  TEXT    NOT NULL REFERENCES device(eui)  ON DELETE CASCADE,
  reporter    INTEGER NOT NULL DEFAULT 0,
  measuring   INTEGER NOT NULL DEFAULT 0,
  ord         INTEGER NOT NULL,
  PRIMARY KEY (zone_number, device_eui));
CREATE UNIQUE INDEX IF NOT EXISTS zone_one_reporter  ON zone_member(zone_number) WHERE reporter  = 1;
CREATE UNIQUE INDEX IF NOT EXISTS zone_one_measuring ON zone_member(zone_number) WHERE measuring = 1;
`

// zoneOpenWindowDDL adds the open window columns to a zone table that predates
// them. ALTER TABLE ADD COLUMN fails on a column that is already there, so it
// stays out of installSchemaDDL and runs from the migration alone.
const zoneOpenWindowDDL = `
ALTER TABLE zone ADD COLUMN open_window_detection    INTEGER NOT NULL DEFAULT 1;
ALTER TABLE zone ADD COLUMN open_window_duration_sec INTEGER NOT NULL DEFAULT 900;
`

// installTables holds every table ReplaceInstall empties, in an order the
// foreign keys accept: a referring row goes before the row it refers to.
var installTables = []string{"zone_member", "zone", "circuit", "device", "install", "net_key"}

// ErrNoInstall reports a database that carries no installation at all: neither
// the generated identity nor a single install row. It separates "not
// initialised yet" from "initialised and empty", which is a valid state before
// the first device is paired.
var ErrNoInstall = errors.New("the database holds no installation")

// NetworkKeyLen is the AES-CCM* PAN key length, in bytes.
const NetworkKeyLen = 16

// Querier is what the row readers need of a *sql.DB or a *sql.Tx. A mutation
// rebuilds the candidate installation from inside its own transaction, so every
// reader takes one of these instead of the store.
type Querier interface {
	RowQuerier
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// LoadInstall rebuilds the installation from the rows. It does not validate:
// the caller decides whether an empty or half-written installation is fatal.
func (s *Store) LoadInstall(ctx context.Context) (*tado.Install, uint64, error) {
	return ReadInstall(ctx, s.db)
}

// ReadInstall is LoadInstall on q: a mutation reads the candidate installation
// back inside its own transaction.
func ReadInstall(ctx context.Context, q Querier) (*tado.Install, uint64, error) {
	kv, err := installValues(ctx, q)
	if err != nil {
		return nil, 0, err
	}
	key, err := networkKey(ctx, q)
	if err != nil {
		return nil, 0, err
	}
	if len(kv) == 0 && key == nil {
		return nil, 0, ErrNoInstall
	}
	rev, _, err := ReadMetaUint(ctx, q, "install_rev")
	if err != nil {
		return nil, 0, err
	}

	in := &tado.Install{
		Version: tado.ConfigVersion,
		Coordinator: tado.Coordinator{
			EUI:    kv["coordinator_eui"],
			Serial: kv["coordinator_serial"],
			Notes:  kv["coordinator_notes"],
		},
	}
	if key != nil {
		in.NetworkKeyHex = hex.EncodeToString(key)
	}
	if raw, ok := kv["home_id"]; ok {
		home, err := strconv.ParseUint(raw, 10, 32)
		if err != nil {
			return nil, 0, fmt.Errorf("install home_id %q: %w", raw, err)
		}
		in.HomeID = uint32(home)
	}
	if raw, ok := kv["utc_offset_min"]; ok {
		offset, err := strconv.Atoi(raw)
		if err != nil {
			return nil, 0, fmt.Errorf("install utc_offset_min %q: %w", raw, err)
		}
		in.Options.UTCOffsetMin = &offset
	}
	in.Options.Timezone = kv["timezone"]
	in.Options.RouterGateway = kv["router_gateway"]
	in.Options.RouterDNS = kv["router_dns"]
	in.Options.FuelUnit = kv["fuel_unit"]
	if raw, ok := kv["utc_push_spacing_sec"]; ok {
		sec, err := strconv.Atoi(raw)
		if err != nil {
			return nil, 0, fmt.Errorf("install utc_push_spacing_sec %q: %w", raw, err)
		}
		in.Options.UTCPushSpacingSec = &sec
	}
	if raw, ok := kv["offline_schedule"]; ok {
		enabled, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, 0, fmt.Errorf("install offline_schedule %q: %w", raw, err)
		}
		in.Options.OfflineSchedule = &enabled
	}
	if raw, ok := kv["flow_auto_adapt"]; ok {
		enabled, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, 0, fmt.Errorf("install flow_auto_adapt %q: %w", raw, err)
		}
		in.Options.FlowAutoAdapt = &enabled
	}
	if raw, ok := kv["flow_default_curve"]; ok {
		enabled, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, 0, fmt.Errorf("install flow_default_curve %q: %w", raw, err)
		}
		in.Options.FlowDefaultCurve = &enabled
	}
	if raw, ok := kv["external_window_max_sec"]; ok {
		sec, err := strconv.Atoi(raw)
		if err != nil {
			return nil, 0, fmt.Errorf("install external_window_max_sec %q: %w", raw, err)
		}
		in.Options.ExternalWindowMaxSec = &sec
	}
	if raw, ok := kv["low_battery_cell_mv"]; ok {
		mv, err := strconv.Atoi(raw)
		if err != nil {
			return nil, 0, fmt.Errorf("install low_battery_cell_mv %q: %w", raw, err)
		}
		in.Options.LowBatteryCellMV = &mv
	}
	if raw, ok := kv["offline_after_sec"]; ok {
		sec, err := strconv.Atoi(raw)
		if err != nil {
			return nil, 0, fmt.Errorf("install offline_after_sec %q: %w", raw, err)
		}
		in.Options.OfflineAfterSec = &sec
	}
	if err := loadBoilerOptions(kv, &in.Options); err != nil {
		return nil, 0, err
	}
	if err := loadFlowOptions(kv, &in.Options); err != nil {
		return nil, 0, err
	}
	if err := loadEarlyStartOptions(kv, &in.Options); err != nil {
		return nil, 0, err
	}
	if err := loadDeviceOverlayEnds(kv, &in.Options); err != nil {
		return nil, 0, err
	}
	if err := loadZoneOffsets(kv, &in.Options); err != nil {
		return nil, 0, err
	}
	if err := loadZonePIDs(kv, &in.Options); err != nil {
		return nil, 0, err
	}
	if err := loadAutoOffsets(kv, &in.Options); err != nil {
		return nil, 0, err
	}
	if err := loadAwayTemperatures(kv, &in.Options); err != nil {
		return nil, 0, err
	}
	if err := loadDevices(ctx, q, in); err != nil {
		return nil, 0, err
	}
	if err := loadCircuits(ctx, q, in); err != nil {
		return nil, 0, err
	}
	if err := loadZones(ctx, q, in); err != nil {
		return nil, 0, err
	}
	return in, rev, nil
}

// boilerOptions maps the install keys of the boiler output and gas flow
// options to their fields.
func boilerOptions(o *tado.Options) map[string]**float64 {
	return map[string]**float64{
		"boiler_max_power_kw": &o.BoilerMaxPowerKW,
		"boiler_min_power_kw": &o.BoilerMinPowerKW,
		"boiler_max_gas_m3h":  &o.BoilerMaxGasM3H,
		"boiler_min_gas_m3h":  &o.BoilerMinGasM3H,
	}
}

func loadBoilerOptions(kv map[string]string, o *tado.Options) error {
	for key, field := range boilerOptions(o) {
		raw, ok := kv[key]
		if !ok {
			continue
		}
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return fmt.Errorf("install %s %q: %w", key, raw, err)
		}
		*field = &v
	}
	return nil
}

// flowIntOptions maps the install keys of the integer flow limit options, and
// of the age limit of the temperatures pushed over HTTP, to their fields.
func flowIntOptions(o *tado.Options) map[string]**int {
	return map[string]**int{
		"flow_limit_c":          &o.FlowLimitC,
		"outdoor_max_age_sec":   &o.OutdoorMaxAgeSec,
		"room_temp_max_age_sec": &o.RoomTempMaxAgeSec,
		"flow_hysteresis_c":     &o.FlowHysteresisC,
		"flow_min_interval_sec": &o.FlowMinIntervalSec,
	}
}

// flowCurvePrefix starts the install key of a circuit's weather compensation
// curve, stored as JSON.
const flowCurvePrefix = "flow_curve."

func FlowCurveKey(id int) string { return flowCurvePrefix + strconv.Itoa(id) }

func loadFlowOptions(kv map[string]string, o *tado.Options) error {
	for key, field := range flowIntOptions(o) {
		raw, ok := kv[key]
		if !ok {
			continue
		}
		v, err := strconv.Atoi(raw)
		if err != nil {
			return fmt.Errorf("install %s %q: %w", key, raw, err)
		}
		*field = &v
	}
	for key, raw := range kv {
		rest, ok := strings.CutPrefix(key, flowCurvePrefix)
		if !ok {
			continue
		}
		id, err := strconv.Atoi(rest)
		if err != nil {
			return fmt.Errorf("install %s: %w", key, err)
		}
		var curve tado.FlowCurve
		if err := json.Unmarshal([]byte(raw), &curve); err != nil {
			return fmt.Errorf("install %s: %w", key, err)
		}
		if o.FlowCurves == nil {
			o.FlowCurves = map[int]tado.FlowCurve{}
		}
		o.FlowCurves[id] = curve
	}
	return nil
}

// earlyStartIntOptions maps the install keys of the integer early start
// options to their fields.
func earlyStartIntOptions(o *tado.Options) map[string]**int {
	return map[string]**int{
		"early_start_max_lead_sec": &o.EarlyStartMaxLeadSec,
		"early_start_min_episodes": &o.EarlyStartMinEpisodes,
	}
}

// earlyStartPrefix starts the install key of a zone whose early start is on.
// The key exists only while the switch is on.
const earlyStartPrefix = "early_start."

func EarlyStartKey(zone int) string { return earlyStartPrefix + strconv.Itoa(zone) }

func loadEarlyStartOptions(kv map[string]string, o *tado.Options) error {
	for key, field := range earlyStartIntOptions(o) {
		raw, ok := kv[key]
		if !ok {
			continue
		}
		v, err := strconv.Atoi(raw)
		if err != nil {
			return fmt.Errorf("install %s %q: %w", key, raw, err)
		}
		*field = &v
	}
	for key := range kv {
		rest, ok := strings.CutPrefix(key, earlyStartPrefix)
		if !ok {
			continue
		}
		zone, err := strconv.Atoi(rest)
		if err != nil {
			return fmt.Errorf("install %s: %w", key, err)
		}
		if o.EarlyStartZones == nil {
			o.EarlyStartZones = map[int]bool{}
		}
		o.EarlyStartZones[zone] = true
	}
	return nil
}

// deviceOverlayEndPrefix starts the install key of a zone's device overlay
// end rule, stored as JSON. The key exists only while the zone states a rule.
const deviceOverlayEndPrefix = "device_overlay_end."

func DeviceOverlayEndKey(zone int) string { return deviceOverlayEndPrefix + strconv.Itoa(zone) }

func loadDeviceOverlayEnds(kv map[string]string, o *tado.Options) error {
	for key, raw := range kv {
		rest, ok := strings.CutPrefix(key, deviceOverlayEndPrefix)
		if !ok {
			continue
		}
		zone, err := strconv.Atoi(rest)
		if err != nil {
			return fmt.Errorf("install %s: %w", key, err)
		}
		var end tado.OverlayEnd
		if err := json.Unmarshal([]byte(raw), &end); err != nil {
			return fmt.Errorf("install %s: %w", key, err)
		}
		if o.DeviceOverlayEnds == nil {
			o.DeviceOverlayEnds = map[int]tado.OverlayEnd{}
		}
		o.DeviceOverlayEnds[zone] = end
	}
	return nil
}

// zoneOffsetPrefix starts the install key of a zone's temperature correction
// mode, stored as JSON. The key exists only while the zone states a mode.
const zoneOffsetPrefix = "zone_offset."

func ZoneOffsetKey(zone int) string { return zoneOffsetPrefix + strconv.Itoa(zone) }

func loadZoneOffsets(kv map[string]string, o *tado.Options) error {
	for key, raw := range kv {
		rest, ok := strings.CutPrefix(key, zoneOffsetPrefix)
		if !ok {
			continue
		}
		zone, err := strconv.Atoi(rest)
		if err != nil {
			return fmt.Errorf("install %s: %w", key, err)
		}
		var off tado.ZoneOffset
		if err := json.Unmarshal([]byte(raw), &off); err != nil {
			return fmt.Errorf("install %s: %w", key, err)
		}
		if o.ZoneOffsets == nil {
			o.ZoneOffsets = map[int]tado.ZoneOffset{}
		}
		o.ZoneOffsets[zone] = off
	}
	return nil
}

// autoOffsetPrefix starts the install key of a zone's automatic temperature
// calibration, stored as JSON. The key exists only while the zone states one.
const autoOffsetPrefix = "auto_offset."

func AutoOffsetKey(zone int) string { return autoOffsetPrefix + strconv.Itoa(zone) }

func loadAutoOffsets(kv map[string]string, o *tado.Options) error {
	for key, raw := range kv {
		rest, ok := strings.CutPrefix(key, autoOffsetPrefix)
		if !ok {
			continue
		}
		zone, err := strconv.Atoi(rest)
		if err != nil {
			return fmt.Errorf("install %s: %w", key, err)
		}
		var a tado.AutoOffset
		if err := json.Unmarshal([]byte(raw), &a); err != nil {
			return fmt.Errorf("install %s: %w", key, err)
		}
		if o.AutoOffsets == nil {
			o.AutoOffsets = map[int]tado.AutoOffset{}
		}
		o.AutoOffsets[zone] = a
	}
	return nil
}

// zonePIDPrefix starts the install key of a zone's controller gains, stored as
// JSON. The key exists only while the zone states gains other than the default.
const zonePIDPrefix = "zone_pid."

func ZonePIDKey(zone int) string { return zonePIDPrefix + strconv.Itoa(zone) }

func loadZonePIDs(kv map[string]string, o *tado.Options) error {
	for key, raw := range kv {
		rest, ok := strings.CutPrefix(key, zonePIDPrefix)
		if !ok {
			continue
		}
		zone, err := strconv.Atoi(rest)
		if err != nil {
			return fmt.Errorf("install %s: %w", key, err)
		}
		var pid tado.ZonePID
		if err := json.Unmarshal([]byte(raw), &pid); err != nil {
			return fmt.Errorf("install %s: %w", key, err)
		}
		if o.ZonePIDs == nil {
			o.ZonePIDs = map[int]tado.ZonePID{}
		}
		o.ZonePIDs[zone] = pid
	}
	return nil
}

// awayTemperaturePrefix starts the install key of a zone's away temperature, in
// degrees C. The key exists only while the zone states one.
const awayTemperaturePrefix = "away_temperature_c."

func AwayTemperatureKey(zone int) string { return awayTemperaturePrefix + strconv.Itoa(zone) }

func loadAwayTemperatures(kv map[string]string, o *tado.Options) error {
	for key, raw := range kv {
		rest, ok := strings.CutPrefix(key, awayTemperaturePrefix)
		if !ok {
			continue
		}
		zone, err := strconv.Atoi(rest)
		if err != nil {
			return fmt.Errorf("install %s: %w", key, err)
		}
		c, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return fmt.Errorf("install %s %q: %w", key, raw, err)
		}
		if o.AwayTemperatures == nil {
			o.AwayTemperatures = map[int]float64{}
		}
		o.AwayTemperatures[zone] = c
	}
	return nil
}

func installValues(ctx context.Context, q Querier) (map[string]string, error) {
	out := map[string]string{}
	rows, err := q.QueryContext(ctx, `SELECT key, value FROM install`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		out[key] = value
	}
	return out, rows.Err()
}

// networkKey returns the stored PAN key, or nil when the identity is missing.
func networkKey(ctx context.Context, q Querier) ([]byte, error) {
	var key []byte
	switch err := q.QueryRowContext(ctx, `SELECT key FROM net_key WHERE id = 1`).Scan(&key); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, err
	}
	if len(key) != NetworkKeyLen {
		return nil, fmt.Errorf("net_key holds %d bytes, want %d", len(key), NetworkKeyLen)
	}
	return key, nil
}

func loadDevices(ctx context.Context, q Querier, in *tado.Install) error {
	rows, err := q.QueryContext(ctx, `SELECT eui, kind, serial, label, zone, notes FROM device ORDER BY ord`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var d tado.Device
		var kind string
		var zone sql.NullInt64
		if err := rows.Scan(&d.EUI, &kind, &d.Serial, &d.Label, &zone, &d.Notes); err != nil {
			return err
		}
		d.Kind = tado.DeviceKind(kind)
		if zone.Valid {
			n := int(zone.Int64)
			d.Zone = &n
		}
		in.Devices = append(in.Devices, d)
	}
	return rows.Err()
}

func loadCircuits(ctx context.Context, q Querier, in *tado.Install) error {
	rows, err := q.QueryContext(ctx, `SELECT id, name, heat_source FROM circuit ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var c tado.Circuit
		if err := rows.Scan(&c.ID, &c.Name, &c.HeatSource); err != nil {
			return err
		}
		in.Circuits = append(in.Circuits, c)
	}
	return rows.Err()
}

func loadZones(ctx context.Context, q Querier, in *tado.Install) error {
	rows, err := q.QueryContext(ctx, `SELECT number, name, kind, circuit,
	  open_window_detection, open_window_duration_sec FROM zone ORDER BY number`)
	if err != nil {
		return err
	}
	defer rows.Close()
	at := map[int]int{}
	for rows.Next() {
		var z tado.Zone
		var kind string
		var circuit sql.NullInt64
		var detection int
		var duration int
		if err := rows.Scan(&z.Number, &z.Name, &kind, &circuit, &detection, &duration); err != nil {
			return err
		}
		z.Kind = tado.ZoneKind(kind)
		if circuit.Valid {
			c := int(circuit.Int64)
			z.Circuit = &c
		}
		enabled := detection != 0
		z.OpenWindowDetection = &enabled
		z.OpenWindowDurationSec = &duration
		at[z.Number] = len(in.Zones)
		in.Zones = append(in.Zones, z)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return loadZoneMembers(ctx, q, in, at)
}

func loadZoneMembers(ctx context.Context, q Querier, in *tado.Install, at map[int]int) error {
	rows, err := q.QueryContext(ctx,
		`SELECT zone_number, device_eui, reporter, measuring FROM zone_member ORDER BY zone_number, ord`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var number int
		var eui string
		var reporter, measuring int
		if err := rows.Scan(&number, &eui, &reporter, &measuring); err != nil {
			return err
		}
		i, ok := at[number]
		if !ok {
			return fmt.Errorf("zone_member names zone %d, which no row declares", number)
		}
		z := &in.Zones[i]
		z.Members = append(z.Members, eui)
		if reporter != 0 {
			z.Reporter = append(z.Reporter, eui)
		}
		if measuring != 0 {
			z.MeasuringDevice = eui
		}
	}
	return rows.Err()
}

// ReplaceInstall writes in over every installation row in one transaction and
// returns the new revision. It leaves schema_version, state_version and
// save_seq alone: the desired state has its own sequence.
//
// SetMaxOpenConns(1) means this transaction must commit before anything else
// touches the database, a state save included, or the caller deadlocks.
func (s *Store) ReplaceInstall(ctx context.Context, in *tado.Install) (uint64, error) {
	if in == nil {
		return 0, errors.New("installation: nil install")
	}
	key := in.NetworkKey()
	if len(key) != NetworkKeyLen {
		return 0, fmt.Errorf("installation: the network key is %d bytes, want %d: Validate resolves it",
			len(key), NetworkKeyLen)
	}
	coordinator := in.CoordinatorEUI()
	if len(coordinator) != 8 {
		return 0, fmt.Errorf("installation: coordinator EUI %q is unusable", in.Coordinator.EUI)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for _, table := range installTables {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			return 0, err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO net_key (id, key) VALUES (1, ?)`, key); err != nil {
		return 0, err
	}
	values := [][2]string{
		{"home_id", strconv.FormatUint(uint64(in.HomeID), 10)},
		{"coordinator_eui", hex.EncodeToString(coordinator)},
		{"coordinator_serial", in.Coordinator.Serial},
		{"coordinator_notes", in.Coordinator.Notes},
	}
	if o := in.Options.UTCOffsetMin; o != nil {
		values = append(values, [2]string{"utc_offset_min", strconv.Itoa(*o)})
	}
	if tz := in.Options.Timezone; tz != "" {
		values = append(values, [2]string{"timezone", tz})
	}
	if o := in.Options.RouterGateway; o != "" {
		values = append(values, [2]string{"router_gateway", o})
	}
	if o := in.Options.RouterDNS; o != "" {
		values = append(values, [2]string{"router_dns", o})
	}
	if o := in.Options.FuelUnit; o != "" {
		values = append(values, [2]string{"fuel_unit", o})
	}
	if o := in.Options.UTCPushSpacingSec; o != nil {
		values = append(values, [2]string{"utc_push_spacing_sec", strconv.Itoa(*o)})
	}
	if o := in.Options.OfflineSchedule; o != nil {
		values = append(values, [2]string{"offline_schedule", strconv.FormatInt(boolInt(*o), 10)})
	}
	if o := in.Options.FlowAutoAdapt; o != nil {
		values = append(values, [2]string{"flow_auto_adapt", strconv.FormatInt(boolInt(*o), 10)})
	}
	if o := in.Options.FlowDefaultCurve; o != nil {
		values = append(values, [2]string{"flow_default_curve", strconv.FormatInt(boolInt(*o), 10)})
	}
	if o := in.Options.ExternalWindowMaxSec; o != nil {
		values = append(values, [2]string{"external_window_max_sec", strconv.Itoa(*o)})
	}
	if o := in.Options.LowBatteryCellMV; o != nil {
		values = append(values, [2]string{"low_battery_cell_mv", strconv.Itoa(*o)})
	}
	if o := in.Options.OfflineAfterSec; o != nil {
		values = append(values, [2]string{"offline_after_sec", strconv.Itoa(*o)})
	}
	for key, field := range boilerOptions(&in.Options) {
		if *field != nil {
			values = append(values, [2]string{key, strconv.FormatFloat(**field, 'g', -1, 64)})
		}
	}
	for key, field := range flowIntOptions(&in.Options) {
		if *field != nil {
			values = append(values, [2]string{key, strconv.Itoa(**field)})
		}
	}
	for id, curve := range in.Options.FlowCurves {
		raw, err := json.Marshal(curve)
		if err != nil {
			return 0, err
		}
		values = append(values, [2]string{FlowCurveKey(id), string(raw)})
	}
	for key, field := range earlyStartIntOptions(&in.Options) {
		if *field != nil {
			values = append(values, [2]string{key, strconv.Itoa(**field)})
		}
	}
	for zone, on := range in.Options.EarlyStartZones {
		if on {
			values = append(values, [2]string{EarlyStartKey(zone), "1"})
		}
	}
	for zone, end := range in.Options.DeviceOverlayEnds {
		raw, err := json.Marshal(end)
		if err != nil {
			return 0, err
		}
		values = append(values, [2]string{DeviceOverlayEndKey(zone), string(raw)})
	}
	for zone, off := range in.Options.ZoneOffsets {
		raw, err := json.Marshal(off)
		if err != nil {
			return 0, err
		}
		values = append(values, [2]string{ZoneOffsetKey(zone), string(raw)})
	}
	for zone, pid := range in.Options.ZonePIDs {
		raw, err := json.Marshal(pid)
		if err != nil {
			return 0, err
		}
		values = append(values, [2]string{ZonePIDKey(zone), string(raw)})
	}
	for zone, a := range in.Options.AutoOffsets {
		raw, err := json.Marshal(a)
		if err != nil {
			return 0, err
		}
		values = append(values, [2]string{AutoOffsetKey(zone), string(raw)})
	}
	for zone, c := range in.Options.AwayTemperatures {
		values = append(values, [2]string{AwayTemperatureKey(zone), strconv.FormatFloat(c, 'g', -1, 64)})
	}
	for _, kv := range values {
		if _, err := tx.ExecContext(ctx, `INSERT INTO install (key, value) VALUES (?, ?)`, kv[0], kv[1]); err != nil {
			return 0, err
		}
	}
	if err := writeDevices(ctx, tx, in); err != nil {
		return 0, err
	}
	if err := writeCircuits(ctx, tx, in); err != nil {
		return 0, err
	}
	if err := writeZones(ctx, tx, in); err != nil {
		return 0, err
	}
	rev, _, err := ReadMetaUint(ctx, tx, "install_rev")
	if err != nil {
		return 0, err
	}
	rev++
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO meta (key, value) VALUES ('install_rev', ?)`,
		strconv.FormatUint(rev, 10)); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return rev, nil
}

func writeDevices(ctx context.Context, tx Querier, in *tado.Install) error {
	for i := range in.Devices {
		d := &in.Devices[i]
		wire := d.WireEUI()
		if len(wire) != 8 {
			return fmt.Errorf("installation: devices[%d].eui %q is unusable", i, d.EUI)
		}
		var zone any
		if d.Zone != nil {
			zone = *d.Zone
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO device (eui, kind, serial, label, zone, notes, ord) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			hex.EncodeToString(wire), string(d.Kind), d.Serial, d.Label, zone, d.Notes, i); err != nil {
			return err
		}
	}
	return nil
}

func writeCircuits(ctx context.Context, tx Querier, in *tado.Install) error {
	for i := range in.Circuits {
		c := &in.Circuits[i]
		source, err := in.Resolve(c.HeatSource)
		if err != nil {
			return fmt.Errorf("installation: circuits[%d].heat_source: %w", i, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO circuit (id, name, heat_source) VALUES (?, ?, ?)`,
			c.ID, c.Name, hex.EncodeToString(source.WireEUI())); err != nil {
			return err
		}
	}
	return nil
}

func writeZones(ctx context.Context, tx Querier, in *tado.Install) error {
	for i := range in.Zones {
		z := &in.Zones[i]
		var circuit any
		if z.Circuit != nil {
			circuit = *z.Circuit
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO zone
		  (number, name, kind, circuit, open_window_detection, open_window_duration_sec)
		  VALUES (?, ?, ?, ?, ?, ?)`,
			z.Number, z.Name, string(z.Kind), circuit,
			boolInt(z.OpenWindowEnabled()), z.OpenWindowSeconds()); err != nil {
			return err
		}
		for j, ref := range z.Members {
			d, err := in.Resolve(ref)
			if err != nil {
				return fmt.Errorf("installation: zones[%d].members[%d]: %w", i, j, err)
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO zone_member (zone_number, device_eui, ord) VALUES (?, ?, ?)`,
				z.Number, hex.EncodeToString(d.WireEUI()), j); err != nil {
				return err
			}
		}
		// The two roles are written from the resolved answer, so the config
		// fallbacks of MeasuringDevice end up spelled out in the rows.
		if d, err := in.DemandReporter(z.Number); err == nil {
			if err := markMember(ctx, tx, "reporter", z.Number, d); err != nil {
				return err
			}
		}
		if d, err := in.MeasuringDevice(z.Number); err == nil {
			if err := markMember(ctx, tx, "measuring", z.Number, d); err != nil {
				return err
			}
		}
	}
	return nil
}

func markMember(ctx context.Context, tx Querier, column string, zone int, d *tado.Device) error {
	_, err := tx.ExecContext(ctx,
		"UPDATE zone_member SET "+column+" = 1 WHERE zone_number = ? AND device_eui = ?",
		zone, hex.EncodeToString(d.WireEUI()))
	return err
}

// EnsureIdentity generates the coordinator identity on a database that has
// none: the PAN key, our own EUI and the home id. It is idempotent and never
// prints the key.
func (s *Store) EnsureIdentity(ctx context.Context) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var keys int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM net_key`).Scan(&keys); err != nil {
		return false, err
	}
	if keys > 0 {
		return false, nil
	}
	key := make([]byte, NetworkKeyLen)
	if _, err := rand.Read(key); err != nil {
		return false, err
	}
	eui, err := randomCoordinatorEUI()
	if err != nil {
		return false, err
	}
	home, err := randomHomeID()
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO net_key (id, key) VALUES (1, ?)`, key); err != nil {
		return false, err
	}
	for _, kv := range [][2]string{
		{"home_id", strconv.FormatUint(uint64(home), 10)},
		{"coordinator_eui", hex.EncodeToString(eui)},
	} {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO install (key, value) VALUES (?, ?)`,
			kv[0], kv[1]); err != nil {
			return false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO meta (key, value) VALUES ('install_rev', '1')`); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// randomCoordinatorEUI draws the 5 free bytes of a wire address. The devices
// filter on the tado suffix, which sits at the end of the wire spelling.
func randomCoordinatorEUI() ([]byte, error) {
	eui := make([]byte, 8-len(tado.EUISuffix), 8)
	if _, err := rand.Read(eui); err != nil {
		return nil, err
	}
	return append(eui, tado.EUISuffix...), nil
}

// randomHomeID never returns 0: the resource paths carry it and Validate
// refuses a zero home.
func randomHomeID() (uint32, error) {
	var b [4]byte
	for {
		if _, err := rand.Read(b[:]); err != nil {
			return 0, err
		}
		if home := binary.LittleEndian.Uint32(b[:]); home != 0 {
			return home, nil
		}
	}
}

// InstallRev returns the revision of the stored installation. A database no
// installation write has touched yet answers 0.
func (s *Store) InstallRev(ctx context.Context) (uint64, error) {
	rev, _, err := ReadMetaUint(ctx, s.db, "install_rev")
	return rev, err
}

// LoadInstall reads a validated installation from the database at path. It
// generates nothing: a database without an identity is an error here, because
// only the daemon commissions one.
func LoadInstall(path string) (*tado.Install, error) {
	store, err := Open(path)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	in, _, err := store.LoadInstall(context.Background())
	if err != nil {
		return nil, fmt.Errorf("state database %s: %w", path, err)
	}
	if err := in.Validate(); err != nil {
		return nil, err
	}
	return in, nil
}

// WriteInstall replaces the whole installation in the database at path,
// creating the file and the schema when they are missing. It is the way in for
// the development tools and the one-off seed script. The desired state in the
// same database is left alone.
func WriteInstall(path string, in *tado.Install) error {
	store, err := Open(path)
	if err != nil {
		return err
	}
	defer store.Close()
	if _, err := store.ReplaceInstall(context.Background(), in); err != nil {
		return fmt.Errorf("state database %s: %w", path, err)
	}
	return nil
}
