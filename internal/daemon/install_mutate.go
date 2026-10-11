package daemon

// install_mutate: one store method per management-API mutation. They all run
// the same transaction: apply the rows, rebuild the installation from the rows
// inside the transaction, gate it, then bump install_rev and commit. Nothing
// touches the database again before the commit, because the store keeps a
// single connection.

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"freedo/internal/install"
	"freedo/tado"
)

// querier is install.Querier under the name the mutations already use.
type querier = install.Querier

// installResult is what one mutation hands back: the installation the rows now
// describe, its revision and the warnings the gate collected.
type installResult struct {
	Install  *tado.Install
	Rev      uint64
	Warnings []string
}

// badRequestError is input the caller can fix: an unknown reference, an
// unusable EUI, a value the installation refuses.
type badRequestError struct{ msg string }

func (e *badRequestError) Error() string { return e.msg }

func badf(format string, a ...any) error { return &badRequestError{msg: fmt.Sprintf(format, a...)} }

// notFoundError names an object the rows do not hold.
type notFoundError struct{ msg string }

func (e *notFoundError) Error() string { return e.msg }

func notFoundf(format string, a ...any) error { return &notFoundError{msg: fmt.Sprintf(format, a...)} }

// conflictError is a mutation the stored rows refuse: a stale revision, a
// duplicate key, or a row another one still names. referrers lists every row
// that holds the reference, so one round trip tells the caller what to undo
// first.
type conflictError struct {
	msg       string
	referrers []string
	rev       uint64
}

func (e *conflictError) Error() string { return e.msg }

func conflictf(format string, a ...any) error { return &conflictError{msg: fmt.Sprintf(format, a...)} }

// optInt separates an absent JSON field from an explicit null: "leave it" from
// "clear it". A plain *int cannot say both.
type optInt struct {
	set   bool
	value *int
}

// UnmarshalJSON implements json.Unmarshaler.
func (o *optInt) UnmarshalJSON(b []byte) error {
	o.set = true
	if string(bytes.TrimSpace(b)) == "null" {
		o.value = nil
		return nil
	}
	var v int
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	o.value = &v
	return nil
}

// optFloat is optInt for a number with a fraction.
type optFloat struct {
	set   bool
	value *float64
}

// UnmarshalJSON implements json.Unmarshaler.
func (o *optFloat) UnmarshalJSON(b []byte) error {
	o.set = true
	if string(bytes.TrimSpace(b)) == "null" {
		o.value = nil
		return nil
	}
	var v float64
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	o.value = &v
	return nil
}

// optPID is optInt for the gains of a zone.
type optPID struct {
	set   bool
	value *pidPatch
}

type pidPatch struct {
	Kp *int `json:"kp"`
	Ki *int `json:"ki"`
	Kd *int `json:"kd"`
}

// UnmarshalJSON implements json.Unmarshaler. The body decoder's strictness
// does not reach a custom unmarshaler, and encoding/json matches field names
// without regard to case, so the keys are checked here against their exact
// spelling before the strict decode.
func (o *optPID) UnmarshalJSON(b []byte) error {
	o.set = true
	if string(bytes.TrimSpace(b)) == "null" {
		o.value = nil
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return err
	}
	var unknown []string
	for key := range fields {
		if key != "kp" && key != "ki" && key != "kd" {
			unknown = append(unknown, strconv.Quote(key))
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("pid: unknown field %s, want kp, ki or kd", strings.Join(unknown, ", "))
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	o.value = &pidPatch{}
	return dec.Decode(o.value)
}

// optString is optInt for a string field.
type optString struct {
	set   bool
	value *string
}

// UnmarshalJSON implements json.Unmarshaler.
func (o *optString) UnmarshalJSON(b []byte) error {
	o.set = true
	if string(bytes.TrimSpace(b)) == "null" {
		o.value = nil
		return nil
	}
	var v string
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	o.value = &v
	return nil
}

// mutate is the discipline every mutation follows. cur is the installation the
// rows described when the transaction opened, which is what a reference in the
// request resolves against; the candidate comes back out of the rows apply left
// behind. On any error the transaction rolls back and the database is untouched.
func (s *stateStore) mutate(ctx context.Context, expect *uint64,
	apply func(ctx context.Context, tx querier, cur *tado.Install) error) (installResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return installResult{}, err
	}
	defer tx.Rollback()
	cur, _, err := install.ReadInstall(ctx, tx)
	if err != nil {
		return installResult{}, err
	}
	if err := apply(ctx, tx, cur); err != nil {
		return installResult{}, withCurrentRev(ctx, tx, err)
	}
	candidate, rev, err := install.ReadInstall(ctx, tx)
	if err != nil {
		return installResult{}, err
	}
	if err := candidate.ValidateCommissionable(true); err != nil {
		return installResult{}, &badRequestError{msg: err.Error()}
	}
	if expect != nil && *expect != rev {
		return installResult{}, &conflictError{
			msg: fmt.Sprintf("the installation is at revision %d, the request carries %d", rev, *expect),
			rev: rev,
		}
	}
	rev++
	if _, err := tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO meta (key, value) VALUES ('install_rev', ?)`,
		strconv.FormatUint(rev, 10)); err != nil {
		return installResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return installResult{}, err
	}
	return installResult{Install: candidate, Rev: rev, Warnings: candidate.Warnings()}, nil
}

// withCurrentRev stamps the revision a caller has to retry against onto a
// conflict the rows raised, so one answer carries both the reason and the
// token.
func withCurrentRev(ctx context.Context, q querier, err error) error {
	var conflict *conflictError
	if !errors.As(err, &conflict) || conflict.rev != 0 {
		return err
	}
	rev, _, readErr := install.ReadMetaUint(ctx, q, "install_rev")
	if readErr != nil {
		return err
	}
	conflict.rev = rev
	return err
}

// --- shared row helpers ---

// resolveRef maps a caller's device reference to the wire EUI hex the rows key
// on. An ambiguous room label travels out as it is, so the handler can answer
// with every candidate.
func resolveRef(in *tado.Install, ref string) (string, *tado.Device, error) {
	dev, err := in.Resolve(ref)
	if err != nil {
		var amb *tado.AmbiguousRefError
		if errors.As(err, &amb) {
			return "", nil, err
		}
		return "", nil, &badRequestError{msg: err.Error()}
	}
	wire := dev.WireEUI()
	if len(wire) != 8 {
		return "", nil, badf("device %s carries an unusable EUI %q", dev.Ref(), dev.EUI)
	}
	return hex.EncodeToString(wire), dev, nil
}

// checkLabelFree refuses a room label another device already carries and names
// that device. The label is what the dissector and every log line print, so one
// name for two devices makes a capture ambiguous. except is the wire EUI of the
// device under write, which never collides with itself.
func checkLabelFree(cur *tado.Install, label, except string) error {
	if strings.TrimSpace(label) == "" {
		return nil
	}
	for _, dev := range cur.DevicesByLabel(label) {
		if hexRef(dev) == except {
			continue
		}
		return conflictf("label %q is already declared by device %s", label, dev.Ref())
	}
	return nil
}

func deviceExists(ctx context.Context, q querier, wire string) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM device WHERE eui = ?`, wire).Scan(&n)
	return n > 0, err
}

func zoneExists(ctx context.Context, q querier, number int) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM zone WHERE number = ?`, number).Scan(&n)
	return n > 0, err
}

func circuitExists(ctx context.Context, q querier, id int) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM circuit WHERE id = ?`, id).Scan(&n)
	return n > 0, err
}

func deviceCount(ctx context.Context, q querier) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM device`).Scan(&n)
	return n, err
}

// nextOrd returns the append position of an ordered table.
func nextOrd(ctx context.Context, q querier, query string, args ...any) (int, error) {
	var next int
	err := q.QueryRowContext(ctx, query, args...).Scan(&next)
	return next, err
}

func intsFrom(ctx context.Context, q querier, query string, args ...any) ([]int, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func stringsFrom(ctx context.Context, q querier, query string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func setInstallValue(ctx context.Context, q querier, key, value string) error {
	_, err := q.ExecContext(ctx, `INSERT OR REPLACE INTO install (key, value) VALUES (?, ?)`, key, value)
	return err
}

func clearInstallValue(ctx context.Context, q querier, key string) error {
	_, err := q.ExecContext(ctx, `DELETE FROM install WHERE key = ?`, key)
	return err
}

func checkDeviceKind(kind string) error {
	switch tado.DeviceKind(kind) {
	case tado.KindValve, tado.KindThermostat, tado.KindBridge:
		return nil
	}
	return badf("unknown device kind %q: one of valve, thermostat, bridge", kind)
}

func checkZoneKind(kind string) error {
	switch tado.ZoneKind(kind) {
	case tado.ZoneHeating, tado.ZoneHotWater:
		return nil
	}
	return badf("unknown zone kind %q: one of heating, hotwater", kind)
}

// --- the installation row ---

func (s *stateStore) PatchInstall(ctx context.Context, p installPatch) (installResult, error) {
	return s.mutate(ctx, p.Rev, func(ctx context.Context, tx querier, cur *tado.Install) error {
		identity := p.HomeID != nil || (p.Coordinator != nil && p.Coordinator.EUI != nil)
		if identity {
			// Adopting a live tado network means taking over its home id and
			// coordinator address. Once a device is declared, both are what the
			// devices already answer to and a rewrite would orphan every row.
			n, err := deviceCount(ctx, tx)
			if err != nil {
				return err
			}
			if n > 0 {
				return &conflictError{msg: fmt.Sprintf(
					"home_id and coordinator_eui are writable only while no device is declared, and %d are", n)}
			}
		}
		if p.HomeID != nil {
			if err := setInstallValue(ctx, tx, "home_id", strconv.FormatUint(uint64(*p.HomeID), 10)); err != nil {
				return err
			}
		}
		if c := p.Coordinator; c != nil {
			if c.EUI != nil {
				wire, err := tado.ParseEUI(*c.EUI)
				if err != nil {
					return badf("coordinator eui: %v", err)
				}
				if err := setInstallValue(ctx, tx, "coordinator_eui", hex.EncodeToString(wire)); err != nil {
					return err
				}
			}
			if c.Serial != nil {
				if err := setInstallValue(ctx, tx, "coordinator_serial", *c.Serial); err != nil {
					return err
				}
			}
			if c.Notes != nil {
				if err := setInstallValue(ctx, tx, "coordinator_notes", *c.Notes); err != nil {
					return err
				}
			}
		}
		if p.OfflineSchedule != nil {
			if err := setInstallValue(ctx, tx, "offline_schedule", strconv.FormatInt(boolInt(*p.OfflineSchedule), 10)); err != nil {
				return err
			}
		}
		if p.ExternalWindowMaxSec != nil {
			if err := setInstallValue(ctx, tx, "external_window_max_sec", strconv.Itoa(*p.ExternalWindowMaxSec)); err != nil {
				return err
			}
		}
		if p.LowBatteryCellMV != nil {
			if err := setInstallValue(ctx, tx, "low_battery_cell_mv", strconv.Itoa(*p.LowBatteryCellMV)); err != nil {
				return err
			}
		}
		if p.OfflineAfterSec != nil {
			if err := setInstallValue(ctx, tx, "offline_after_sec", strconv.Itoa(*p.OfflineAfterSec)); err != nil {
				return err
			}
		}
		for key, v := range map[string]optFloat{
			"boiler_max_power_kw": p.BoilerMaxPowerKW,
			"boiler_min_power_kw": p.BoilerMinPowerKW,
		} {
			if !v.set {
				continue
			}
			var err error
			if v.value == nil {
				err = clearInstallValue(ctx, tx, key)
			} else {
				err = setInstallValue(ctx, tx, key, strconv.FormatFloat(*v.value, 'g', -1, 64))
			}
			if err != nil {
				return err
			}
		}
		if err := patchFlowOptions(ctx, tx, p); err != nil {
			return err
		}
		for key, v := range map[string]*int{
			"early_start_max_lead_sec": p.EarlyStartMaxLeadSec,
			"early_start_min_episodes": p.EarlyStartMinEpisodes,
		} {
			if v == nil {
				continue
			}
			if err := setInstallValue(ctx, tx, key, strconv.Itoa(*v)); err != nil {
				return err
			}
		}
		if p.UTCPushSpacingSec != nil {
			if err := setInstallValue(ctx, tx, "utc_push_spacing_sec", strconv.Itoa(*p.UTCPushSpacingSec)); err != nil {
				return err
			}
		}
		if p.Timezone.set {
			var err error
			if p.Timezone.value == nil || *p.Timezone.value == "" {
				err = clearInstallValue(ctx, tx, "timezone")
			} else {
				err = setInstallValue(ctx, tx, "timezone", *p.Timezone.value)
			}
			if err != nil {
				return err
			}
		}
		if p.UTCOffsetMin.set {
			if p.UTCOffsetMin.value == nil {
				return clearInstallValue(ctx, tx, "utc_offset_min")
			}
			return setInstallValue(ctx, tx, "utc_offset_min", strconv.Itoa(*p.UTCOffsetMin.value))
		}
		return nil
	})
}

// patchFlowOptions writes the flow limit options a patch carries. A null curve
// removes the circuit's curve.
func patchFlowOptions(ctx context.Context, tx querier, p installPatch) error {
	for key, v := range map[string]*int{
		"flow_limit_c":          p.FlowLimitC,
		"outdoor_max_age_sec":   p.OutdoorMaxAgeSec,
		"flow_hysteresis_c":     p.FlowHysteresisC,
		"flow_min_interval_sec": p.FlowMinIntervalSec,
	} {
		if v == nil {
			continue
		}
		if err := setInstallValue(ctx, tx, key, strconv.Itoa(*v)); err != nil {
			return err
		}
	}
	for id, curve := range p.FlowCurves {
		if curve == nil {
			if err := clearInstallValue(ctx, tx, install.FlowCurveKey(id)); err != nil {
				return err
			}
			continue
		}
		raw, err := json.Marshal(curve)
		if err != nil {
			return err
		}
		if err := setInstallValue(ctx, tx, install.FlowCurveKey(id), string(raw)); err != nil {
			return err
		}
	}
	return nil
}

// SetNetworkKey writes the PAN key the coordinator protects every frame with.
func (s *stateStore) SetNetworkKey(ctx context.Context, p networkKeyPut) (installResult, error) {
	return s.mutate(ctx, p.Rev, func(ctx context.Context, tx querier, cur *tado.Install) error {
		key, err := parseNetworkKey(p.Key)
		if err != nil {
			return err
		}
		// A device holds the key it was paired under. A rewrite would leave
		// every one of them deaf, so the key freezes with the first device.
		n, err := deviceCount(ctx, tx)
		if err != nil {
			return err
		}
		if n > 0 {
			return &conflictError{msg: fmt.Sprintf(
				"the network key is writable only while no device is declared, and %d are", n)}
		}
		_, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO net_key (id, key) VALUES (1, ?)`, key)
		return err
	})
}

// parseNetworkKey reads the 32 hex characters of a PAN key. No message it
// returns repeats the value it refused.
func parseNetworkKey(raw string) ([]byte, error) {
	key, err := hex.DecodeString(strings.TrimSpace(raw))
	if err != nil {
		return nil, badf("key: want %d hex characters, the value is not hex", 2*install.NetworkKeyLen)
	}
	if len(key) != install.NetworkKeyLen {
		return nil, badf("key: want %d hex characters for %d bytes, the value holds %d bytes",
			2*install.NetworkKeyLen, install.NetworkKeyLen, len(key))
	}
	return key, nil
}

// --- devices ---

func (s *stateStore) CreateDevice(ctx context.Context, c deviceCreate) (installResult, error) {
	return s.mutate(ctx, c.Rev, func(ctx context.Context, tx querier, cur *tado.Install) error {
		wire, err := tado.ParseEUI(c.EUI)
		if err != nil {
			return badf("eui: %v", err)
		}
		if err := checkDeviceKind(c.Kind); err != nil {
			return err
		}
		key := hex.EncodeToString(wire)
		switch found, err := deviceExists(ctx, tx, key); {
		case err != nil:
			return err
		case found:
			return conflictf("device %s is already declared", key)
		}
		if c.Serial != "" {
			var n int
			if err := tx.QueryRowContext(ctx,
				`SELECT count(*) FROM device WHERE serial = ?`, c.Serial).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				return conflictf("serial %s is already declared", c.Serial)
			}
		}
		if err := checkLabelFree(cur, c.Label, key); err != nil {
			return err
		}
		ord, err := nextOrd(ctx, tx, `SELECT coalesce(max(ord), -1) + 1 FROM device`)
		if err != nil {
			return err
		}
		var zone any
		if c.Zone.value != nil {
			zone = *c.Zone.value
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO device (eui, kind, serial, label, zone, notes, ord) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			key, c.Kind, c.Serial, c.Label, zone, c.Notes, ord)
		return err
	})
}

// PatchDevice also returns the wire EUI it patched: a patch that moves the
// serial or the label leaves the reference the caller used naming nothing.
func (s *stateStore) PatchDevice(ctx context.Context, ref string, p devicePatch) (installResult, string, error) {
	var patched string
	res, err := s.mutate(ctx, p.Rev, func(ctx context.Context, tx querier, cur *tado.Install) error {
		key, _, err := resolveRef(cur, ref)
		if err != nil {
			return err
		}
		patched = key
		if p.Kind != nil {
			if err := checkDeviceKind(*p.Kind); err != nil {
				return err
			}
			if err := updateDeviceColumn(ctx, tx, key, "kind", *p.Kind); err != nil {
				return err
			}
		}
		if p.Serial != nil {
			var n int
			if err := tx.QueryRowContext(ctx,
				`SELECT count(*) FROM device WHERE serial = ? AND eui <> ?`, *p.Serial, key).Scan(&n); err != nil {
				return err
			}
			if n > 0 && *p.Serial != "" {
				return conflictf("serial %s is already declared", *p.Serial)
			}
			if err := updateDeviceColumn(ctx, tx, key, "serial", *p.Serial); err != nil {
				return err
			}
		}
		if p.Label != nil {
			if err := checkLabelFree(cur, *p.Label, key); err != nil {
				return err
			}
			if err := updateDeviceColumn(ctx, tx, key, "label", *p.Label); err != nil {
				return err
			}
		}
		if p.Notes != nil {
			if err := updateDeviceColumn(ctx, tx, key, "notes", *p.Notes); err != nil {
				return err
			}
		}
		if p.Zone.set {
			var zone any
			if p.Zone.value != nil {
				zone = *p.Zone.value
			}
			if err := updateDeviceColumn(ctx, tx, key, "zone", zone); err != nil {
				return err
			}
		}
		return nil
	})
	return res, patched, err
}

func updateDeviceColumn(ctx context.Context, q querier, key, column string, value any) error {
	_, err := q.ExecContext(ctx, "UPDATE device SET "+column+" = ? WHERE eui = ?", value, key)
	return err
}

func (s *stateStore) DeleteDevice(ctx context.Context, ref string, expect *uint64) (installResult, *tado.Device, error) {
	var gone *tado.Device
	res, err := s.mutate(ctx, expect, func(ctx context.Context, tx querier, cur *tado.Install) error {
		key, dev, err := resolveRef(cur, ref)
		if err != nil {
			return err
		}
		zones, err := intsFrom(ctx, tx,
			`SELECT zone_number FROM zone_member WHERE device_eui = ? ORDER BY zone_number`, key)
		if err != nil {
			return err
		}
		circuits, err := intsFrom(ctx, tx,
			`SELECT id FROM circuit WHERE heat_source = ? ORDER BY id`, key)
		if err != nil {
			return err
		}
		var referrers []string
		for _, zone := range zones {
			referrers = append(referrers, fmt.Sprintf("zone %d members", zone))
		}
		for _, id := range circuits {
			referrers = append(referrers, fmt.Sprintf("circuit %d heat_source", id))
		}
		if len(referrers) > 0 {
			return &conflictError{
				msg:       fmt.Sprintf("device %s is still named by %s", dev.Ref(), strings.Join(referrers, ", ")),
				referrers: referrers,
			}
		}
		copied := *dev
		gone = &copied
		_, err = tx.ExecContext(ctx, `DELETE FROM device WHERE eui = ?`, key)
		return err
	})
	return res, gone, err
}

// --- zones ---

func (s *stateStore) CreateZone(ctx context.Context, c zoneCreate) (installResult, error) {
	return s.mutate(ctx, c.Rev, func(ctx context.Context, tx querier, cur *tado.Install) error {
		if c.Number == nil {
			return badf("number is required")
		}
		if err := checkZoneKind(c.Kind); err != nil {
			return err
		}
		switch found, err := zoneExists(ctx, tx, *c.Number); {
		case err != nil:
			return err
		case found:
			return conflictf("zone %d is already declared", *c.Number)
		}
		circuit, err := zoneCircuitValue(ctx, tx, c.Circuit)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO zone (number, name, kind, circuit) VALUES (?, ?, ?, ?)`,
			*c.Number, c.Name, c.Kind, circuit)
		return err
	})
}

// zoneCircuitValue turns the circuit field of a request into the column value,
// after checking the circuit exists: the foreign key would report it as a bare
// constraint failure.
func zoneCircuitValue(ctx context.Context, q querier, circuit optInt) (any, error) {
	if circuit.value == nil {
		return nil, nil
	}
	switch found, err := circuitExists(ctx, q, *circuit.value); {
	case err != nil:
		return nil, err
	case !found:
		return nil, badf("circuit %d is not declared", *circuit.value)
	}
	return *circuit.value, nil
}

func (s *stateStore) PatchZone(ctx context.Context, number int, p zonePatch) (installResult, error) {
	return s.mutate(ctx, p.Rev, func(ctx context.Context, tx querier, cur *tado.Install) error {
		switch found, err := zoneExists(ctx, tx, number); {
		case err != nil:
			return err
		case !found:
			return notFoundf("no zone %d", number)
		}
		if p.Name != nil {
			if _, err := tx.ExecContext(ctx, `UPDATE zone SET name = ? WHERE number = ?`, *p.Name, number); err != nil {
				return err
			}
		}
		if p.Kind != nil {
			if err := checkZoneKind(*p.Kind); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE zone SET kind = ? WHERE number = ?`, *p.Kind, number); err != nil {
				return err
			}
		}
		if p.Circuit.set {
			circuit, err := zoneCircuitValue(ctx, tx, p.Circuit)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE zone SET circuit = ? WHERE number = ?`, circuit, number); err != nil {
				return err
			}
		}
		if p.OpenWindowDetection != nil {
			if _, err := tx.ExecContext(ctx,
				`UPDATE zone SET open_window_detection = ? WHERE number = ?`,
				boolInt(*p.OpenWindowDetection), number); err != nil {
				return err
			}
		}
		if p.OpenWindowDuration != nil {
			if _, err := tx.ExecContext(ctx,
				`UPDATE zone SET open_window_duration_sec = ? WHERE number = ?`,
				*p.OpenWindowDuration, number); err != nil {
				return err
			}
		}
		if err := patchDeviceOverlayEnd(ctx, tx, cur, number, p); err != nil {
			return err
		}
		if err := patchZoneOffset(ctx, tx, cur, number, p); err != nil {
			return err
		}
		if err := patchZonePID(ctx, tx, cur, number, p.PID); err != nil {
			return err
		}
		switch {
		case p.EarlyStart == nil:
		case *p.EarlyStart:
			return setInstallValue(ctx, tx, install.EarlyStartKey(number), "1")
		default:
			return clearInstallValue(ctx, tx, install.EarlyStartKey(number))
		}
		return nil
	})
}

// patchDeviceOverlayEnd applies the end rule fields of p on the rule the zone
// holds. A mode other than timer drops the duration. A rule equal to the
// default is not stored.
func patchDeviceOverlayEnd(ctx context.Context, tx querier, cur *tado.Install, number int, p zonePatch) error {
	if p.DeviceOverlayEnd == nil && p.DeviceOverlayEndSec == nil {
		return nil
	}
	end := cur.Options.DeviceOverlayEnd(number)
	if p.DeviceOverlayEnd != nil {
		end.Mode = *p.DeviceOverlayEnd
		if end.Mode != tado.OverlayEndTimer {
			end.DurationSec = 0
		}
	}
	if p.DeviceOverlayEndSec != nil {
		end.DurationSec = *p.DeviceOverlayEndSec
	}
	if msg := end.Check(); msg != "" {
		return badf("device_overlay_end: %s", msg)
	}
	if end == tado.DefaultDeviceOverlayEnd {
		return clearInstallValue(ctx, tx, install.DeviceOverlayEndKey(number))
	}
	raw, err := json.Marshal(end)
	if err != nil {
		return err
	}
	return setInstallValue(ctx, tx, install.DeviceOverlayEndKey(number), string(raw))
}

// patchZoneOffset applies the correction fields of p on the mode the zone
// holds. A mode other than setpoint drops the coordinator's own correction, so
// it cannot come back unseen with a later switch. The default is not stored.
func patchZoneOffset(ctx context.Context, tx querier, cur *tado.Install, number int, p zonePatch) error {
	if p.OffsetMode == nil && p.SetpointOffsetCenti == nil {
		return nil
	}
	off := cur.Options.ZoneOffset(number)
	if p.OffsetMode != nil {
		off.Mode = *p.OffsetMode
		if off.Mode != tado.OffsetModeSetpoint {
			off.SetpointCenti = 0
		}
	}
	if p.SetpointOffsetCenti != nil {
		off.SetpointCenti = *p.SetpointOffsetCenti
	}
	if msg := off.Check(); msg != "" {
		return badf("offset_mode: %s", msg)
	}
	if off == tado.DefaultZoneOffset {
		return clearInstallValue(ctx, tx, install.ZoneOffsetKey(number))
	}
	raw, err := json.Marshal(off)
	if err != nil {
		return err
	}
	return setInstallValue(ctx, tx, install.ZoneOffsetKey(number), string(raw))
}

// patchZonePID applies the gains of p on the ones the zone holds: a gain the
// request leaves out keeps its value, null returns the zone to the default.
// The default is not stored, so the zone document keeps its ETag.
func patchZonePID(ctx context.Context, tx querier, cur *tado.Install, number int, p optPID) error {
	if !p.set {
		return nil
	}
	pid := tado.DefaultZonePID
	if p.value != nil {
		pid = cur.Options.ZonePID(number)
		for _, g := range []struct {
			in  *int
			out *int
		}{{p.value.Kp, &pid.Kp}, {p.value.Ki, &pid.Ki}, {p.value.Kd, &pid.Kd}} {
			if g.in != nil {
				*g.out = *g.in
			}
		}
		if msg := pid.Check(); msg != "" {
			return badf("pid: %s", msg)
		}
	}
	if pid == tado.DefaultZonePID {
		return clearInstallValue(ctx, tx, install.ZonePIDKey(number))
	}
	raw, err := json.Marshal(pid)
	if err != nil {
		return err
	}
	return setInstallValue(ctx, tx, install.ZonePIDKey(number), string(raw))
}

func (s *stateStore) DeleteZone(ctx context.Context, number int, expect *uint64) (installResult, *tado.Zone, error) {
	var gone *tado.Zone
	res, err := s.mutate(ctx, expect, func(ctx context.Context, tx querier, cur *tado.Install) error {
		zone, err := cur.ZoneByNumber(number)
		if err != nil {
			return notFoundf("no zone %d", number)
		}
		// The member rows cascade, but a device that answers on this zone id
		// would be left claiming a zone nobody declares.
		claimants, err := stringsFrom(ctx, tx, `SELECT eui FROM device WHERE zone = ? ORDER BY ord`, number)
		if err != nil {
			return err
		}
		if len(claimants) > 0 {
			referrers := make([]string, len(claimants))
			for i, eui := range claimants {
				referrers[i] = "device " + eui + " zone"
			}
			return &conflictError{
				msg:       fmt.Sprintf("zone %d is still claimed by %s", number, strings.Join(referrers, ", ")),
				referrers: referrers,
			}
		}
		copied := *zone
		gone = &copied
		if err := clearInstallValue(ctx, tx, install.EarlyStartKey(number)); err != nil {
			return err
		}
		if err := clearInstallValue(ctx, tx, install.DeviceOverlayEndKey(number)); err != nil {
			return err
		}
		if err := clearInstallValue(ctx, tx, install.ZoneOffsetKey(number)); err != nil {
			return err
		}
		if err := clearInstallValue(ctx, tx, install.ZonePIDKey(number)); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM zone WHERE number = ?`, number)
		return err
	})
	return res, gone, err
}

// --- zone members ---

func (s *stateStore) PutZoneMembers(ctx context.Context, number int, p membersPut) (installResult, error) {
	return s.mutate(ctx, p.Rev, func(ctx context.Context, tx querier, cur *tado.Install) error {
		switch found, err := zoneExists(ctx, tx, number); {
		case err != nil:
			return err
		case !found:
			return notFoundf("no zone %d", number)
		}
		keys := make([]string, 0, len(p.Members))
		seen := map[string]bool{}
		for _, ref := range p.Members {
			key, _, err := resolveRef(cur, ref)
			if err != nil {
				return err
			}
			if seen[key] {
				return badf("device %s appears twice in the member list", key)
			}
			seen[key] = true
			keys = append(keys, key)
		}
		// The two roles are per member, so a member the list keeps keeps them.
		roles, err := memberRoles(ctx, tx, number)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM zone_member WHERE zone_number = ?`, number); err != nil {
			return err
		}
		for i, key := range keys {
			role := roles[key]
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO zone_member (zone_number, device_eui, reporter, measuring, ord) VALUES (?, ?, ?, ?, ?)`,
				number, key, role[0], role[1], i); err != nil {
				return err
			}
		}
		return nil
	})
}

// memberRoles reads the reporter and measuring flags of one zone, keyed by wire
// EUI hex.
func memberRoles(ctx context.Context, q querier, number int) (map[string][2]int, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT device_eui, reporter, measuring FROM zone_member WHERE zone_number = ?`, number)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][2]int{}
	for rows.Next() {
		var key string
		var reporter, measuring int
		if err := rows.Scan(&key, &reporter, &measuring); err != nil {
			return nil, err
		}
		out[key] = [2]int{reporter, measuring}
	}
	return out, rows.Err()
}

func (s *stateStore) AppendZoneMember(ctx context.Context, number int, p memberPost) (installResult, error) {
	return s.mutate(ctx, p.Rev, func(ctx context.Context, tx querier, cur *tado.Install) error {
		switch found, err := zoneExists(ctx, tx, number); {
		case err != nil:
			return err
		case !found:
			return notFoundf("no zone %d", number)
		}
		key, dev, err := resolveRef(cur, p.Ref)
		if err != nil {
			return err
		}
		switch member, err := isMember(ctx, tx, number, key); {
		case err != nil:
			return err
		case member:
			return conflictf("%s is already a member of zone %d", dev.Ref(), number)
		}
		ord, err := nextOrd(ctx, tx,
			`SELECT coalesce(max(ord), -1) + 1 FROM zone_member WHERE zone_number = ?`, number)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO zone_member (zone_number, device_eui, ord) VALUES (?, ?, ?)`, number, key, ord)
		return err
	})
}

func (s *stateStore) RemoveZoneMember(ctx context.Context, number int, ref string, expect *uint64) (installResult, error) {
	return s.mutate(ctx, expect, func(ctx context.Context, tx querier, cur *tado.Install) error {
		switch found, err := zoneExists(ctx, tx, number); {
		case err != nil:
			return err
		case !found:
			return notFoundf("no zone %d", number)
		}
		key, dev, err := resolveRef(cur, ref)
		if err != nil {
			return err
		}
		switch member, err := isMember(ctx, tx, number, key); {
		case err != nil:
			return err
		case !member:
			return notFoundf("%s is not a member of zone %d", dev.Ref(), number)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM zone_member WHERE zone_number = ? AND device_eui = ?`, number, key); err != nil {
			return err
		}
		return renumberMembers(ctx, tx, number)
	})
}

func isMember(ctx context.Context, q querier, number int, key string) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx,
		`SELECT count(*) FROM zone_member WHERE zone_number = ? AND device_eui = ?`, number, key).Scan(&n)
	return n > 0, err
}

// renumberMembers closes the gap a removal left, so ord stays the dense
// position the zone config document is built from.
func renumberMembers(ctx context.Context, q querier, number int) error {
	keys, err := stringsFrom(ctx, q,
		`SELECT device_eui FROM zone_member WHERE zone_number = ? ORDER BY ord`, number)
	if err != nil {
		return err
	}
	for i, key := range keys {
		if _, err := q.ExecContext(ctx,
			`UPDATE zone_member SET ord = ? WHERE zone_number = ? AND device_eui = ?`, i, number, key); err != nil {
			return err
		}
	}
	return nil
}

// SetZoneRole elects the member that carries one of the two per-zone roles,
// "reporter" or "measuring". The partial unique indexes admit one holder, so
// the previous one is cleared in the same statement pair.
func (s *stateStore) SetZoneRole(ctx context.Context, number int, column string, p memberPost) (installResult, error) {
	return s.mutate(ctx, p.Rev, func(ctx context.Context, tx querier, cur *tado.Install) error {
		switch found, err := zoneExists(ctx, tx, number); {
		case err != nil:
			return err
		case !found:
			return notFoundf("no zone %d", number)
		}
		key, dev, err := resolveRef(cur, p.Ref)
		if err != nil {
			return err
		}
		switch member, err := isMember(ctx, tx, number, key); {
		case err != nil:
			return err
		case !member:
			return badf("%s is not a member of zone %d", dev.Ref(), number)
		}
		if err := clearZoneRole(ctx, tx, number, column); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			"UPDATE zone_member SET "+column+" = 1 WHERE zone_number = ? AND device_eui = ?", number, key)
		return err
	})
}

func (s *stateStore) ClearZoneRole(ctx context.Context, number int, column string, expect *uint64) (installResult, error) {
	return s.mutate(ctx, expect, func(ctx context.Context, tx querier, cur *tado.Install) error {
		switch found, err := zoneExists(ctx, tx, number); {
		case err != nil:
			return err
		case !found:
			return notFoundf("no zone %d", number)
		}
		return clearZoneRole(ctx, tx, number, column)
	})
}

func clearZoneRole(ctx context.Context, q querier, number int, column string) error {
	_, err := q.ExecContext(ctx, "UPDATE zone_member SET "+column+" = 0 WHERE zone_number = ?", number)
	return err
}

// --- circuits ---

func (s *stateStore) CreateCircuit(ctx context.Context, c circuitCreate) (installResult, error) {
	return s.mutate(ctx, c.Rev, func(ctx context.Context, tx querier, cur *tado.Install) error {
		if c.ID == nil {
			return badf("id is required")
		}
		switch found, err := circuitExists(ctx, tx, *c.ID); {
		case err != nil:
			return err
		case found:
			return conflictf("circuit %d is already declared", *c.ID)
		}
		if c.HeatSource == "" {
			return badf("heat_source is required: name the wall thermostat that drives the circuit")
		}
		key, _, err := resolveRef(cur, c.HeatSource)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO circuit (id, name, heat_source) VALUES (?, ?, ?)`,
			*c.ID, c.Name, key)
		return err
	})
}

func (s *stateStore) PatchCircuit(ctx context.Context, id int, p circuitPatch) (installResult, error) {
	return s.mutate(ctx, p.Rev, func(ctx context.Context, tx querier, cur *tado.Install) error {
		switch found, err := circuitExists(ctx, tx, id); {
		case err != nil:
			return err
		case !found:
			return notFoundf("no circuit %d", id)
		}
		if p.Name != nil {
			if _, err := tx.ExecContext(ctx, `UPDATE circuit SET name = ? WHERE id = ?`, *p.Name, id); err != nil {
				return err
			}
		}
		if p.HeatSource != nil {
			key, _, err := resolveRef(cur, *p.HeatSource)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE circuit SET heat_source = ? WHERE id = ?`, key, id); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *stateStore) DeleteCircuit(ctx context.Context, id int, expect *uint64) (installResult, *tado.Circuit, error) {
	var gone *tado.Circuit
	res, err := s.mutate(ctx, expect, func(ctx context.Context, tx querier, cur *tado.Install) error {
		circuit, err := cur.CircuitByID(id)
		if err != nil {
			return notFoundf("no circuit %d", id)
		}
		zones, err := intsFrom(ctx, tx, `SELECT number FROM zone WHERE circuit = ? ORDER BY number`, id)
		if err != nil {
			return err
		}
		if len(zones) > 0 {
			referrers := make([]string, len(zones))
			for i, zone := range zones {
				referrers[i] = fmt.Sprintf("zone %d circuit", zone)
			}
			return &conflictError{
				msg:       fmt.Sprintf("circuit %d is still bound to %s", id, strings.Join(referrers, ", ")),
				referrers: referrers,
			}
		}
		copied := *circuit
		gone = &copied
		if err := clearInstallValue(ctx, tx, install.FlowCurveKey(id)); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM circuit WHERE id = ?`, id)
		return err
	})
	return res, gone, err
}
