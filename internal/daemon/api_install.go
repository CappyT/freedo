package daemon

// api_install: the management view of the installation. /api/nodes and
// /api/zones stay the runtime view; these routes are what writes the rows.
// The network key is in no response: one route writes it, and only while the
// installation declares no device, because rotation needs every device
// re-paired.

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"freedo/internal/install"
	"freedo/tado"
)

// --- request payloads ---

type coordinatorPatch struct {
	EUI    *string `json:"eui"`
	Serial *string `json:"serial"`
	Notes  *string `json:"notes"`
}

type installPatch struct {
	Rev          *uint64           `json:"rev"`
	HomeID       *uint32           `json:"home_id"`
	Coordinator  *coordinatorPatch `json:"coordinator"`
	UTCOffsetMin optInt            `json:"utc_offset_min"`
	// Timezone is the IANA zone 02b2 follows; null or "" takes the host's.
	Timezone          optString `json:"timezone"`
	UTCPushSpacingSec *int      `json:"utc_push_spacing_sec"`
	// OfflineSchedule is d/config tag 02b3 for every device without a
	// per-device value. false is the commercial coordinator's behaviour.
	OfflineSchedule *bool `json:"offline_schedule"`
	// ExternalWindowMaxSec ends an external open window that no close follows.
	ExternalWindowMaxSec *int `json:"external_window_max_sec"`
	// RoomTempMaxAgeSec ends the correction a room temperature pushed to
	// POST /api/zones/{n}/temperature drives, when no new reading comes.
	RoomTempMaxAgeSec *int `json:"room_temp_max_age_sec"`
	// LowBatteryCellMV and OfflineAfterSec are the thresholds of the device
	// alerts in /api/health.
	LowBatteryCellMV   *int `json:"low_battery_cell_mv"`
	OfflineAfterSec    *int `json:"offline_after_sec"`
	FlowLimitC         *int `json:"flow_limit_c"`
	OutdoorMaxAgeSec   *int `json:"outdoor_max_age_sec"`
	FlowHysteresisC    *int `json:"flow_hysteresis_c"`
	FlowMinIntervalSec *int `json:"flow_min_interval_sec"`
	// FlowAutoAdapt switches the adaptation of the flow limit to the room
	// feedback, for every circuit.
	FlowAutoAdapt *bool `json:"flow_auto_adapt"`
	// FlowDefaultCurve switches the default weather curve on for every circuit
	// without a curve of its own. It needs flow_limit_c.
	FlowDefaultCurve *bool `json:"flow_default_curve"`
	// FlowCurves sets the weather compensation curve per circuit id; null
	// removes it.
	FlowCurves map[int]*tado.FlowCurve `json:"flow_curves"`
	// EarlyStartMaxLeadSec and EarlyStartMinEpisodes tune the early start of a
	// program block. The per-zone switch is the zone's early_start.
	EarlyStartMaxLeadSec  *int `json:"early_start_max_lead_sec"`
	EarlyStartMinEpisodes *int `json:"early_start_min_episodes"`
	// BoilerMaxPowerKW and BoilerMinPowerKW size the boiler energy estimate,
	// BoilerMaxGasM3H and BoilerMinGasM3H the fuel count in FuelUnit per hour;
	// null removes a value.
	BoilerMaxPowerKW optFloat `json:"boiler_max_power_kw"`
	BoilerMinPowerKW optFloat `json:"boiler_min_power_kw"`
	BoilerMaxGasM3H  optFloat `json:"boiler_max_gas_m3h"`
	BoilerMinGasM3H  optFloat `json:"boiler_min_gas_m3h"`
	// RouterGateway and RouterDNS are the IPv4 addresses of the router
	// advertisement; null or "" takes the unspecified address.
	RouterGateway optString `json:"router_gateway"`
	RouterDNS     optString `json:"router_dns"`
	// FuelUnit is m3, l or kg; null or "" takes m3.
	FuelUnit optString `json:"fuel_unit"`
}

// networkKeyPut carries the PAN key as 32 hex characters. The field travels in
// only: no read route answers with it.
type networkKeyPut struct {
	Rev *uint64 `json:"rev"`
	Key string  `json:"key"`
}

type deviceCreate struct {
	Rev    *uint64 `json:"rev"`
	Kind   string  `json:"kind"`
	EUI    string  `json:"eui"`
	Serial string  `json:"serial"`
	Label  string  `json:"label"`
	Zone   optInt  `json:"zone"`
	Notes  string  `json:"notes"`
}

type devicePatch struct {
	Rev    *uint64 `json:"rev"`
	Kind   *string `json:"kind"`
	Serial *string `json:"serial"`
	Label  *string `json:"label"`
	Zone   optInt  `json:"zone"`
	Notes  *string `json:"notes"`
}

type zoneCreate struct {
	Rev     *uint64 `json:"rev"`
	Number  *int    `json:"number"`
	Name    string  `json:"name"`
	Kind    string  `json:"kind"`
	Circuit optInt  `json:"circuit"`
}

type zonePatch struct {
	Rev                 *uint64 `json:"rev"`
	Name                *string `json:"name"`
	Kind                *string `json:"kind"`
	Circuit             optInt  `json:"circuit"`
	OpenWindowDetection *bool   `json:"open_window_detection"`
	OpenWindowDuration  *int    `json:"open_window_duration_sec"`
	EarlyStart          *bool   `json:"early_start"`
	DeviceOverlayEnd    *string `json:"device_overlay_end"`
	DeviceOverlayEndSec *int    `json:"device_overlay_end_sec"`
	OffsetMode          *string `json:"offset_mode"`
	// PID sets the controller gains kp, ki and kd; null returns the zone to
	// the default ones.
	PID optPID `json:"pid"`
	// AwayTemperatureC is the temperature a heating zone keeps in away, in
	// degrees C; null clears it. Unset and 5 C both read as off in away.
	AwayTemperatureC optFloat `json:"away_temperature_c"`
	// SetpointOffsetCenti is set by POST /api/zones/{number}/offset alone.
	SetpointOffsetCenti *int `json:"-"`
}

type membersPut struct {
	Rev     *uint64  `json:"rev"`
	Members []string `json:"members"`
}

// memberPost carries one device reference: the appended member, the elected
// reporter or the measuring device.
type memberPost struct {
	Rev *uint64 `json:"rev"`
	Ref string  `json:"ref"`
}

type circuitCreate struct {
	Rev        *uint64 `json:"rev"`
	ID         *int    `json:"id"`
	Name       string  `json:"name"`
	HeatSource string  `json:"heat_source"`
}

type circuitPatch struct {
	Rev        *uint64 `json:"rev"`
	Name       *string `json:"name"`
	HeatSource *string `json:"heat_source"`
}

// --- responses ---

// hexRef spells a device the way the rows do, so a caller can feed an answer
// straight back into the next request.
func hexRef(dev *tado.Device) string { return hex.EncodeToString(dev.WireEUI()) }

func deviceView(dev *tado.Device) map[string]any {
	out := map[string]any{
		"eui":    hexRef(dev),
		"short":  dev.Short(),
		"kind":   string(dev.Kind),
		"serial": dev.Serial,
		"label":  dev.Label,
		"notes":  dev.Notes,
		"zone":   nil,
	}
	if dev.Zone != nil {
		out["zone"] = *dev.Zone
	}
	return out
}

func zoneView(in *tado.Install, z *tado.Zone) map[string]any {
	out := map[string]any{
		"number":                   z.Number,
		"name":                     z.Name,
		"kind":                     string(z.Kind),
		"members":                  zoneMemberRefs(in, z),
		"circuit":                  nil,
		"reporter":                 nil,
		"measuring_device":         nil,
		"open_window_detection":    z.OpenWindowEnabled(),
		"open_window_duration_sec": z.OpenWindowSeconds(),
		"early_start":              in.Options.EarlyStart(z.Number),
		"device_overlay_end":       nil,
		"device_overlay_end_sec":   nil,
		"offset_mode":              nil,
		"setpoint_offset_c":        nil,
		"pid":                      nil,
		"away_temperature_c":       nil,
	}
	if z.Kind == tado.ZoneHeating {
		end := in.Options.DeviceOverlayEnd(z.Number)
		out["device_overlay_end"] = end.Mode
		if end.Mode == tado.OverlayEndTimer {
			out["device_overlay_end_sec"] = end.DurationSec
		}
		off := in.Options.ZoneOffset(z.Number)
		out["offset_mode"] = off.Mode
		if off.Mode == tado.OffsetModeSetpoint {
			out["setpoint_offset_c"] = in.Options.SetpointShiftC(z.Number)
		}
		out["pid"] = in.Options.ZonePID(z.Number)
		if c, ok := in.Options.AwayTemperatures[z.Number]; ok {
			out["away_temperature_c"] = c
		}
	}
	if z.Circuit != nil {
		out["circuit"] = *z.Circuit
	}
	if len(z.Reporter) == 1 {
		out["reporter"] = normalizeRef(in, z.Reporter[0])
	}
	if z.MeasuringDevice != "" {
		out["measuring_device"] = normalizeRef(in, z.MeasuringDevice)
	}
	return out
}

func zoneMemberRefs(in *tado.Install, z *tado.Zone) []string {
	out := make([]string, 0, len(z.Members))
	for _, ref := range z.Members {
		out = append(out, normalizeRef(in, ref))
	}
	return out
}

// normalizeRef spells a stored reference as a wire EUI. The rows already hold
// one; a reference that resolves to nothing travels back unchanged rather than
// disappearing from the answer.
func normalizeRef(in *tado.Install, ref string) string {
	if dev, err := in.Resolve(ref); err == nil {
		return hexRef(dev)
	}
	return ref
}

func circuitView(in *tado.Install, c *tado.Circuit) map[string]any {
	return map[string]any{
		"id":          c.ID,
		"name":        c.Name,
		"heat_source": normalizeRef(in, c.HeatSource),
	}
}

func installView(in *tado.Install, rev uint64) map[string]any {
	gateway, dns := in.Options.RouterIPv4()
	out := map[string]any{
		"rev":     rev,
		"home_id": in.HomeID,
		"coordinator": map[string]any{
			"eui":    hex.EncodeToString(in.CoordinatorEUI()),
			"serial": in.Coordinator.Serial,
			"notes":  in.Coordinator.Notes,
		},
		"network_key":              map[string]any{"present": len(in.NetworkKey()) == install.NetworkKeyLen},
		"warnings":                 warningList(in.Warnings()),
		"utc_offset_min":           nil,
		"timezone":                 in.Options.Timezone,
		"utc_push_spacing_sec":     int(in.Options.UTCPushSpacing() / time.Second),
		"offline_schedule":         in.Options.OfflineScheduleEnabled(),
		"external_window_max_sec":  int(in.Options.ExternalWindowMax() / time.Second),
		"room_temp_max_age_sec":    int(in.Options.RoomTempMaxAge() / time.Second),
		"low_battery_cell_mv":      in.Options.LowBatteryCell(),
		"offline_after_sec":        int(in.Options.OfflineAfter() / time.Second),
		"flow_limit_c":             nil,
		"outdoor_max_age_sec":      int(in.Options.OutdoorMaxAge() / time.Second),
		"flow_hysteresis_c":        in.Options.FlowHysteresis(),
		"flow_min_interval_sec":    int(in.Options.FlowMinInterval() / time.Second),
		"flow_auto_adapt":          in.Options.FlowAutoAdaptEnabled(),
		"flow_default_curve":       in.Options.FlowDefaultCurveEnabled(),
		"flow_curves":              flowCurvesView(in.Options.FlowCurves),
		"early_start_max_lead_sec": int(in.Options.EarlyStartMaxLead() / time.Second),
		"early_start_min_episodes": in.Options.EarlyStartMinEpisodeCount(),
		"boiler_max_power_kw":      nil,
		"boiler_min_power_kw":      0.0,
		"boiler_max_gas_m3h":       nil,
		"boiler_min_gas_m3h":       nil,
		"fuel_unit":                in.Options.Fuel(),
		"router_gateway":           gateway.String(),
		"router_dns":               dns.String(),
		"counts": map[string]any{
			"devices":  len(in.Devices),
			"zones":    len(in.Zones),
			"circuits": len(in.Circuits),
		},
	}
	if o := in.Options.UTCOffsetMin; o != nil {
		out["utc_offset_min"] = *o
	}
	if maxKW, minKW, ok := in.Options.BoilerPower(); ok {
		out["boiler_max_power_kw"], out["boiler_min_power_kw"] = maxKW, minKW
	}
	if limit, ok := in.Options.FlowLimit(); ok {
		out["flow_limit_c"] = limit
	}
	if v := in.Options.BoilerMaxGasM3H; v != nil {
		out["boiler_max_gas_m3h"] = *v
	}
	if v := in.Options.BoilerMinGasM3H; v != nil {
		out["boiler_min_gas_m3h"] = *v
	}
	return out
}

// flowCurvesView keys the curves by circuit id, and is an object even when no
// curve is set.
func flowCurvesView(curves map[int]tado.FlowCurve) map[string]tado.FlowCurve {
	out := make(map[string]tado.FlowCurve, len(curves))
	for id, c := range curves {
		out[strconv.Itoa(id)] = c
	}
	return out
}

// --- plumbing ---

// installStore is the database the management routes write. A daemon without
// one serves the installation it was given and refuses every mutation.
func (d *daemon) installStore(w http.ResponseWriter) (*stateStore, bool) {
	if d.state == nil {
		writeErr(w, http.StatusServiceUnavailable, "the daemon runs without a state database")
		return nil, false
	}
	return d.state, true
}

// installNow returns the installation the daemon serves and the revision the
// database holds for it.
func (d *daemon) installNow(w http.ResponseWriter, r *http.Request) (*tado.Install, uint64, bool) {
	in := d.inst.Load()
	if in == nil {
		writeResolveErr(w, errNoInstall)
		return nil, 0, false
	}
	var rev uint64
	if d.state != nil {
		got, err := d.state.InstallRev(r.Context())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return nil, 0, false
		}
		rev = got
	}
	return in, rev, true
}

// writeInstallErr maps a mutation failure to its status. The taxonomy is the
// store's, so every route answers the same way.
func writeInstallErr(w http.ResponseWriter, err error) {
	var amb *tado.AmbiguousRefError
	var bad *badRequestError
	var missing *notFoundError
	var conflict *conflictError
	switch {
	case errors.As(err, &amb):
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": amb.Error(), "candidates": amb.Candidates})
	case errors.As(err, &bad):
		writeErr(w, http.StatusBadRequest, bad.Error())
	case errors.As(err, &missing):
		writeErr(w, http.StatusNotFound, missing.Error())
	case errors.As(err, &conflict):
		body := map[string]any{"error": conflict.Error()}
		if conflict.rev != 0 {
			body["rev"] = conflict.rev
		}
		if len(conflict.referrers) > 0 {
			body["referrers"] = conflict.referrers
		}
		if conflict.effects != nil {
			body["effects"] = conflict.effects
		}
		writeJSON(w, http.StatusConflict, body)
	case errors.Is(err, install.ErrNoInstall):
		writeErr(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, errStopped):
		writeErr(w, http.StatusServiceUnavailable, err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
}

// mutateInstall is the write half of every management route: one store
// mutation under the installation lock, then the engine hop and the state save.
//
// The lock serialises two HTTP writers, so no second transaction opens while
// the first one still holds the single connection, and no second reload
// overtakes the first. run has committed by the time it returns: with
// SetMaxOpenConns(1) the engine hop and the state save that follow would
// otherwise wait on a connection the open transaction still holds.
func (d *daemon) mutateInstall(w http.ResponseWriter, key string,
	run func(*stateStore) (installResult, any, error)) {
	store, ok := d.installStore(w)
	if !ok {
		return
	}
	d.installMu.Lock()
	defer d.installMu.Unlock()
	res, object, err := run(store)
	if err != nil {
		writeInstallErr(w, err)
		return
	}
	body := map[string]any{"rev": res.Rev, "warnings": warningList(res.Warnings)}
	if key != "" {
		body[key] = object
	}
	if err := d.applyInstall(res.Install); err != nil {
		body["error"] = fmt.Sprintf(
			"the database holds revision %d, the running daemon did not take it: %v", res.Rev, err)
		writeJSON(w, http.StatusInternalServerError, body)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// applyInstall hands the committed installation to the engine and saves the
// desired state the reload may have moved.
func (d *daemon) applyInstall(in *tado.Install) error {
	if _, err := d.do(func(*tado.EmuState) (any, error) {
		_, _, err := d.setInstall(in)
		return nil, err
	}); err != nil {
		return err
	}
	return d.persist(nil)
}

// warningList keeps the warnings field an array in JSON, never null.
func warningList(warnings []string) []string {
	if warnings == nil {
		return []string{}
	}
	return warnings
}

// queryRev reads the optional ?rev= concurrency token of a DELETE.
func queryRev(r *http.Request) (*uint64, error) {
	raw := r.URL.Query().Get("rev")
	if raw == "" {
		return nil, nil
	}
	rev, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return nil, badf("bad rev %q: want a decimal revision", raw)
	}
	return &rev, nil
}

// queryForce reads the optional ?force= flag of a DELETE. An absent or empty
// value is false.
func queryForce(r *http.Request) (bool, error) {
	raw := r.URL.Query().Get("force")
	if raw == "" {
		return false, nil
	}
	force, err := strconv.ParseBool(raw)
	if err != nil {
		return false, badf("bad force %q: want true or false", raw)
	}
	return force, nil
}

// pathInt reads a numeric path value.
func pathInt(w http.ResponseWriter, r *http.Request, name string) (int, bool) {
	raw := r.PathValue(name)
	v, err := strconv.Atoi(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad "+name+" "+strconv.Quote(raw))
		return 0, false
	}
	return v, true
}

// --- read handlers ---

func (d *daemon) getInstall(w http.ResponseWriter, r *http.Request) {
	in, rev, ok := d.installNow(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, installView(in, rev))
}

func (d *daemon) getInstallDevices(w http.ResponseWriter, r *http.Request) {
	in, rev, ok := d.installNow(w, r)
	if !ok {
		return
	}
	devices := make([]map[string]any, 0, len(in.Devices))
	for i := range in.Devices {
		devices = append(devices, deviceView(&in.Devices[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"rev": rev, "devices": devices})
}

func (d *daemon) getInstallDevice(w http.ResponseWriter, r *http.Request) {
	in, rev, ok := d.installNow(w, r)
	if !ok {
		return
	}
	dev, err := in.Resolve(r.PathValue("ref"))
	if err != nil {
		writeResolveErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rev": rev, "device": deviceView(dev)})
}

func (d *daemon) getInstallZones(w http.ResponseWriter, r *http.Request) {
	in, rev, ok := d.installNow(w, r)
	if !ok {
		return
	}
	zones := make([]map[string]any, 0, len(in.Zones))
	for i := range in.Zones {
		zones = append(zones, zoneView(in, &in.Zones[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"rev": rev, "zones": zones})
}

func (d *daemon) getInstallZone(w http.ResponseWriter, r *http.Request) {
	in, rev, ok := d.installNow(w, r)
	if !ok {
		return
	}
	number, ok := pathInt(w, r, "number")
	if !ok {
		return
	}
	zone, err := in.ZoneByNumber(number)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rev": rev, "zone": zoneView(in, zone)})
}

func (d *daemon) getInstallCircuits(w http.ResponseWriter, r *http.Request) {
	in, rev, ok := d.installNow(w, r)
	if !ok {
		return
	}
	circuits := make([]map[string]any, 0, len(in.Circuits))
	for i := range in.Circuits {
		circuits = append(circuits, circuitView(in, &in.Circuits[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"rev": rev, "circuits": circuits})
}

func (d *daemon) getInstallCircuit(w http.ResponseWriter, r *http.Request) {
	in, rev, ok := d.installNow(w, r)
	if !ok {
		return
	}
	id, ok := pathInt(w, r, "id")
	if !ok {
		return
	}
	circuit, err := in.CircuitByID(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rev": rev, "circuit": circuitView(in, circuit)})
}

// --- write handlers ---

func (d *daemon) patchInstall(w http.ResponseWriter, r *http.Request) {
	var p installPatch
	if !decodeBody(w, r, &p) {
		return
	}
	d.mutateInstall(w, "install", func(store *stateStore) (installResult, any, error) {
		res, err := store.PatchInstall(r.Context(), p)
		if err != nil {
			return res, nil, err
		}
		return res, installView(res.Install, res.Rev), nil
	})
}

func (d *daemon) putInstallNetworkKey(w http.ResponseWriter, r *http.Request) {
	var p networkKeyPut
	if !decodeBody(w, r, &p) {
		return
	}
	d.mutateInstall(w, "install", func(store *stateStore) (installResult, any, error) {
		res, err := store.SetNetworkKey(r.Context(), p)
		if err != nil {
			return res, nil, err
		}
		return res, installView(res.Install, res.Rev), nil
	})
}

func (d *daemon) postInstallDevice(w http.ResponseWriter, r *http.Request) {
	var c deviceCreate
	if !decodeBody(w, r, &c) {
		return
	}
	d.mutateInstall(w, "device", func(store *stateStore) (installResult, any, error) {
		res, err := store.CreateDevice(r.Context(), c)
		if err != nil {
			return res, nil, err
		}
		return res, viewDeviceOf(res.Install, c.EUI), nil
	})
}

func (d *daemon) patchInstallDevice(w http.ResponseWriter, r *http.Request) {
	var p devicePatch
	if !decodeBody(w, r, &p) {
		return
	}
	ref := r.PathValue("ref")
	d.mutateInstall(w, "device", func(store *stateStore) (installResult, any, error) {
		res, key, err := store.PatchDevice(r.Context(), ref, p)
		if err != nil {
			return res, nil, err
		}
		// A patch can move the serial and the label, so the answer is read back
		// by the EUI, which no patch changes.
		return res, viewDeviceOf(res.Install, key), nil
	})
}

// viewDeviceOf renders the device a mutation left behind. A reference the new
// installation cannot resolve renders as nothing rather than failing the answer
// to a mutation that already committed.
func viewDeviceOf(in *tado.Install, ref string) any {
	dev, err := in.Resolve(ref)
	if err != nil {
		return nil
	}
	return deviceView(dev)
}

func (d *daemon) deleteInstallDevice(w http.ResponseWriter, r *http.Request) {
	rev, err := queryRev(r)
	if err != nil {
		writeInstallErr(w, err)
		return
	}
	force, err := queryForce(r)
	if err != nil {
		writeInstallErr(w, err)
		return
	}
	ref := r.PathValue("ref")
	d.mutateInstall(w, "device", func(store *stateStore) (installResult, any, error) {
		res, gone, err := store.DeleteDevice(r.Context(), ref, rev, force)
		if err != nil {
			return res, nil, err
		}
		return res, deviceView(gone), nil
	})
}

func (d *daemon) postInstallZone(w http.ResponseWriter, r *http.Request) {
	var c zoneCreate
	if !decodeBody(w, r, &c) {
		return
	}
	d.mutateInstall(w, "zone", func(store *stateStore) (installResult, any, error) {
		res, err := store.CreateZone(r.Context(), c)
		if err != nil {
			return res, nil, err
		}
		return res, viewZoneOf(res.Install, *c.Number), nil
	})
}

func (d *daemon) patchInstallZone(w http.ResponseWriter, r *http.Request) {
	number, ok := pathInt(w, r, "number")
	if !ok {
		return
	}
	var p zonePatch
	if !decodeBody(w, r, &p) {
		return
	}
	d.mutateInstall(w, "zone", func(store *stateStore) (installResult, any, error) {
		res, err := store.PatchZone(r.Context(), number, p)
		if err != nil {
			return res, nil, err
		}
		return res, viewZoneOf(res.Install, number), nil
	})
}

func viewZoneOf(in *tado.Install, number int) any {
	zone, err := in.ZoneByNumber(number)
	if err != nil {
		return nil
	}
	return zoneView(in, zone)
}

func (d *daemon) deleteInstallZone(w http.ResponseWriter, r *http.Request) {
	number, ok := pathInt(w, r, "number")
	if !ok {
		return
	}
	rev, err := queryRev(r)
	if err != nil {
		writeInstallErr(w, err)
		return
	}
	d.mutateInstall(w, "zone", func(store *stateStore) (installResult, any, error) {
		res, gone, err := store.DeleteZone(r.Context(), number, rev)
		if err != nil {
			return res, nil, err
		}
		return res, zoneView(res.Install, gone), nil
	})
}

func (d *daemon) putInstallZoneMembers(w http.ResponseWriter, r *http.Request) {
	number, ok := pathInt(w, r, "number")
	if !ok {
		return
	}
	var p membersPut
	if !decodeBody(w, r, &p) {
		return
	}
	d.mutateInstall(w, "members", func(store *stateStore) (installResult, any, error) {
		res, err := store.PutZoneMembers(r.Context(), number, p)
		if err != nil {
			return res, nil, err
		}
		return res, memberRefs(res.Install, number), nil
	})
}

func (d *daemon) postInstallZoneMember(w http.ResponseWriter, r *http.Request) {
	number, ok := pathInt(w, r, "number")
	if !ok {
		return
	}
	var p memberPost
	if !decodeBody(w, r, &p) {
		return
	}
	d.mutateInstall(w, "members", func(store *stateStore) (installResult, any, error) {
		res, err := store.AppendZoneMember(r.Context(), number, p)
		if err != nil {
			return res, nil, err
		}
		return res, memberRefs(res.Install, number), nil
	})
}

func (d *daemon) deleteInstallZoneMember(w http.ResponseWriter, r *http.Request) {
	number, ok := pathInt(w, r, "number")
	if !ok {
		return
	}
	rev, err := queryRev(r)
	if err != nil {
		writeInstallErr(w, err)
		return
	}
	ref := r.PathValue("ref")
	d.mutateInstall(w, "members", func(store *stateStore) (installResult, any, error) {
		res, err := store.RemoveZoneMember(r.Context(), number, ref, rev)
		if err != nil {
			return res, nil, err
		}
		return res, memberRefs(res.Install, number), nil
	})
}

func memberRefs(in *tado.Install, number int) []string {
	zone, err := in.ZoneByNumber(number)
	if err != nil {
		return []string{}
	}
	return zoneMemberRefs(in, zone)
}

func (d *daemon) putInstallZoneReporter(w http.ResponseWriter, r *http.Request) {
	d.putZoneRole(w, r, "reporter")
}

func (d *daemon) deleteInstallZoneReporter(w http.ResponseWriter, r *http.Request) {
	d.deleteZoneRole(w, r, "reporter")
}

func (d *daemon) putInstallZoneMeasuring(w http.ResponseWriter, r *http.Request) {
	d.putZoneRole(w, r, "measuring")
}

func (d *daemon) deleteInstallZoneMeasuring(w http.ResponseWriter, r *http.Request) {
	d.deleteZoneRole(w, r, "measuring")
}

// roleKeys maps the zone_member column to the response field of its role.
var roleKeys = map[string]string{"reporter": "reporter", "measuring": "measuring_device"}

func (d *daemon) putZoneRole(w http.ResponseWriter, r *http.Request, column string) {
	number, ok := pathInt(w, r, "number")
	if !ok {
		return
	}
	var p memberPost
	if !decodeBody(w, r, &p) {
		return
	}
	d.mutateInstall(w, roleKeys[column], func(store *stateStore) (installResult, any, error) {
		res, err := store.SetZoneRole(r.Context(), number, column, p)
		if err != nil {
			return res, nil, err
		}
		return res, zoneRoleRef(res.Install, number, column), nil
	})
}

func (d *daemon) deleteZoneRole(w http.ResponseWriter, r *http.Request, column string) {
	number, ok := pathInt(w, r, "number")
	if !ok {
		return
	}
	rev, err := queryRev(r)
	if err != nil {
		writeInstallErr(w, err)
		return
	}
	d.mutateInstall(w, roleKeys[column], func(store *stateStore) (installResult, any, error) {
		res, err := store.ClearZoneRole(r.Context(), number, column, rev)
		if err != nil {
			return res, nil, err
		}
		return res, zoneRoleRef(res.Install, number, column), nil
	})
}

// zoneRoleRef reads back the role the mutation wrote, straight off the rows the
// candidate was rebuilt from.
func zoneRoleRef(in *tado.Install, number int, column string) any {
	zone, err := in.ZoneByNumber(number)
	if err != nil {
		return nil
	}
	if column == "reporter" {
		if len(zone.Reporter) == 1 {
			return normalizeRef(in, zone.Reporter[0])
		}
		return nil
	}
	if zone.MeasuringDevice == "" {
		return nil
	}
	return normalizeRef(in, zone.MeasuringDevice)
}

func (d *daemon) postInstallCircuit(w http.ResponseWriter, r *http.Request) {
	var c circuitCreate
	if !decodeBody(w, r, &c) {
		return
	}
	d.mutateInstall(w, "circuit", func(store *stateStore) (installResult, any, error) {
		res, err := store.CreateCircuit(r.Context(), c)
		if err != nil {
			return res, nil, err
		}
		return res, viewCircuitOf(res.Install, *c.ID), nil
	})
}

func (d *daemon) patchInstallCircuit(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(w, r, "id")
	if !ok {
		return
	}
	var p circuitPatch
	if !decodeBody(w, r, &p) {
		return
	}
	d.mutateInstall(w, "circuit", func(store *stateStore) (installResult, any, error) {
		res, err := store.PatchCircuit(r.Context(), id, p)
		if err != nil {
			return res, nil, err
		}
		return res, viewCircuitOf(res.Install, id), nil
	})
}

func viewCircuitOf(in *tado.Install, id int) any {
	circuit, err := in.CircuitByID(id)
	if err != nil {
		return nil
	}
	return circuitView(in, circuit)
}

func (d *daemon) deleteInstallCircuit(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(w, r, "id")
	if !ok {
		return
	}
	rev, err := queryRev(r)
	if err != nil {
		writeInstallErr(w, err)
		return
	}
	d.mutateInstall(w, "circuit", func(store *stateStore) (installResult, any, error) {
		res, gone, err := store.DeleteCircuit(r.Context(), id, rev)
		if err != nil {
			return res, nil, err
		}
		return res, circuitView(res.Install, gone), nil
	})
}

// installRoutes registers the management surface on the daemon mux.
func (d *daemon) installRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/install", d.getInstall)
	m.HandleFunc("PATCH /api/install", d.patchInstall)
	m.HandleFunc("PUT /api/install/network_key", d.putInstallNetworkKey)
	m.HandleFunc("GET /api/install/devices", d.getInstallDevices)
	m.HandleFunc("POST /api/install/devices", d.postInstallDevice)
	m.HandleFunc("GET /api/install/devices/{ref}", d.getInstallDevice)
	m.HandleFunc("PATCH /api/install/devices/{ref}", d.patchInstallDevice)
	m.HandleFunc("GET /api/install/devices/{ref}/removal", d.getInstallDeviceRemoval)
	m.HandleFunc("DELETE /api/install/devices/{ref}", d.deleteInstallDevice)
	m.HandleFunc("GET /api/install/zones", d.getInstallZones)
	m.HandleFunc("POST /api/install/zones", d.postInstallZone)
	m.HandleFunc("GET /api/install/zones/{number}", d.getInstallZone)
	m.HandleFunc("PATCH /api/install/zones/{number}", d.patchInstallZone)
	m.HandleFunc("DELETE /api/install/zones/{number}", d.deleteInstallZone)
	m.HandleFunc("PUT /api/install/zones/{number}/members", d.putInstallZoneMembers)
	m.HandleFunc("POST /api/install/zones/{number}/members", d.postInstallZoneMember)
	m.HandleFunc("DELETE /api/install/zones/{number}/members/{ref}", d.deleteInstallZoneMember)
	m.HandleFunc("PUT /api/install/zones/{number}/reporter", d.putInstallZoneReporter)
	m.HandleFunc("DELETE /api/install/zones/{number}/reporter", d.deleteInstallZoneReporter)
	m.HandleFunc("PUT /api/install/zones/{number}/measuring", d.putInstallZoneMeasuring)
	m.HandleFunc("DELETE /api/install/zones/{number}/measuring", d.deleteInstallZoneMeasuring)
	m.HandleFunc("GET /api/install/circuits", d.getInstallCircuits)
	m.HandleFunc("POST /api/install/circuits", d.postInstallCircuit)
	m.HandleFunc("GET /api/install/circuits/{id}", d.getInstallCircuit)
	m.HandleFunc("PATCH /api/install/circuits/{id}", d.patchInstallCircuit)
	m.HandleFunc("DELETE /api/install/circuits/{id}", d.deleteInstallCircuit)
}
