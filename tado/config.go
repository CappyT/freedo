package tado

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ConfigVersion is the schema version this package understands. A file that
// declares another version is rejected rather than silently misread.
const ConfigVersion = 1

// A zone or circuit id reaches the per-device tables through a 5-bit local slot
// (id & 0x1f), so 31 is the hard ceiling. The RU's zone table holds 11 entries,
// but that bounds how many zones one thermostat aggregates, not how high a zone
// number may be: zone_to_circuit_demand_update@0x2debc walks those 11 slots
// matching each slot's circuit id, never the zone number. This installation runs
// a live zone 17, so a number above 10 is worth flagging only as a count to
// watch, not as an error.
const (
	maxTableSlot = 10
	maxSlotID    = 31
)

// DeviceKind is the role a node plays on the PAN.
type DeviceKind string

const (
	KindValve      DeviceKind = "valve"      // thermostatic valve, serial VA...
	KindThermostat DeviceKind = "thermostat" // wall thermostat, boiler side, serial RU...
	KindBridge     DeviceKind = "bridge"     // internet bridge, serial IB...
)

// ZoneKind is the zone type the firmware reads from TLV 6020.
type ZoneKind string

const (
	ZoneHeating  ZoneKind = "heating"
	ZoneHotWater ZoneKind = "hotwater"
)

// TLV6020 is the byte the firmware reads for this zone kind.
func (k ZoneKind) TLV6020() byte {
	if k == ZoneHotWater {
		return 0x02
	}
	return 0x01
}

// Install describes one installation: the PAN we coordinate, the devices on it
// and the zone and circuit map the cloud used to serve them. It supersedes the
// single-install package variables NetworkKey, BridgeEUI and Inventory.
//
// Parse returns a validated Install. A hand-built one must go through Validate
// before the lookups are meaningful, because Validate is what resolves the
// network key and rejects contradictory references.
type Install struct {
	Version int    `yaml:"version"`
	HomeID  uint32 `yaml:"home_id"`

	// NetworkKeyHex is the 16-byte AES key, 32 hex digits.
	NetworkKeyHex string `yaml:"network_key"`

	Coordinator Coordinator `yaml:"coordinator"`
	Devices     []Device    `yaml:"devices"`
	Zones       []Zone      `yaml:"zones"`
	Circuits    []Circuit   `yaml:"circuits"`
	Options     Options     `yaml:"options"`

	key      []byte
	warnings []string
}

// Coordinator is our own identity on the air: the address the devices see as
// the PAN coordinator. Continuing an existing network means using that
// coordinator's EUI here, because the devices keep addressing it; a network built
// from scratch can pick any unused EUI ending in the tado suffix (EUISuffix).
type Coordinator struct {
	EUI    string `yaml:"eui"`
	Serial string `yaml:"serial"`
	Notes  string `yaml:"notes"`
}

// Device is one node of the PAN.
type Device struct {
	Kind   DeviceKind `yaml:"kind"`
	EUI    string     `yaml:"eui"`
	Serial string     `yaml:"serial"`
	Label  string     `yaml:"label"`
	// Zone is the single logical id this device answers to on "?id=N". A valve
	// accepts exactly one value (app_zone_id_validate compares the query
	// against one SRAM byte), so declaring it removes the guesswork when the
	// device is a member of more than one zone on paper.
	Zone  *int   `yaml:"zone"`
	Notes string `yaml:"notes"`
}

// Zone is one cloud-allocated zone: the N in "h/<home>/z/N/..." and in the
// "?id=N" / "?lid=N" queries. Numbering is sparse, not a dense index.
type Zone struct {
	Number          int      `yaml:"number"`
	Name            string   `yaml:"name"`
	Kind            ZoneKind `yaml:"kind"`
	Circuit         *int     `yaml:"circuit"`
	Members         []string `yaml:"members"`
	MeasuringDevice string   `yaml:"measuring_device"`
	// Reporter elects the member that publishes the zone demand (tag 40a0). The
	// election reaches the device in its d/config tag 015e record for this zone,
	// as [role bitmask][zone id]: the elected member gets role 0x0d, the others
	// 0x05. It accepts a scalar or a list, so a config that elects two devices is
	// representable and can be rejected with both names. Optional: a device
	// already commissioned holds its role in its own store.
	Reporter refList `yaml:"reporter"`
	// OpenWindowDetection is tag 60e0 of the zone configuration document and
	// OpenWindowDurationSec tag 62c0, the detection window in seconds. The app
	// changes both without a z/s write: the zone document carries them, and the
	// member reads it again after the coordinator pokes its d/config. A pointer
	// keeps "the config states nothing" apart from an explicit value, and an
	// unstated field takes the captured default.
	OpenWindowDetection   *bool `yaml:"open_window_detection"`
	OpenWindowDurationSec *int  `yaml:"open_window_duration_sec"`
}

// ZoneOpenWindowDetection and ZoneOpenWindowDurationSec are the values the
// captured zone configuration document carries, which a zone that states
// neither field gets.
const (
	ZoneOpenWindowDetection   = true
	ZoneOpenWindowDurationSec = 900
)

// zoneOpenWindowDurationMax bounds tag 62c0, a uint16 on the wire.
const zoneOpenWindowDurationMax = 0xffff

// OpenWindowEnabled reports the open window detection of the zone.
func (z *Zone) OpenWindowEnabled() bool {
	if z.OpenWindowDetection == nil {
		return ZoneOpenWindowDetection
	}
	return *z.OpenWindowDetection
}

// OpenWindowSeconds reports the open window detection duration of the zone.
func (z *Zone) OpenWindowSeconds() int {
	if z.OpenWindowDurationSec == nil {
		return ZoneOpenWindowDurationSec
	}
	return *z.OpenWindowDurationSec
}

// Circuit is one heating circuit and the wall thermostat that drives it.
type Circuit struct {
	ID         int    `yaml:"id"`
	Name       string `yaml:"name"`
	HeatSource string `yaml:"heat_source"`
}

// Options are the installation knobs. The process knobs live in the bootstrap
// file, not here.
type Options struct {
	// UTCOffsetMin is TLV 02b2, signed minutes east of UTC, as a fixed value.
	// Set, it overrides Timezone. A pointer because 0 is a legitimate offset.
	UTCOffsetMin *int `yaml:"utc_offset_min"`
	// Timezone is the IANA zone tag 02b2 follows while UTCOffsetMin is unset.
	// Empty takes the host's zone. The device holds no DST rules, so the
	// coordinator serves the new offset at each transition.
	Timezone string `yaml:"timezone"`
	// UTCPushSpacingSec is the shortest time between two automatic 02b2 writes,
	// so the valves do not all home their motor at once. Unset takes
	// DefaultUTCPushSpacingSec.
	UTCPushSpacingSec *int `yaml:"utc_push_spacing_sec"`
	// OfflineSchedule is d/config tag 02b3 for every device the coordinator
	// configures: on true a device fetches its zone schedule and runs it alone
	// when the coordinator stops answering. The commercial coordinator writes
	// false. A pointer keeps "the config states nothing" apart from false, and
	// an unstated field takes DefaultOfflineSchedule.
	OfflineSchedule *bool `yaml:"offline_schedule"`
	// ExternalWindowMaxSec ends an external open window that no close follows,
	// so a lost close event cannot leave a zone at frost protection. Unset takes
	// DefaultExternalWindowMaxSec.
	ExternalWindowMaxSec *int `yaml:"external_window_max_sec"`
	// LowBatteryCellMV is the per-cell voltage under which a device reports a
	// low battery. Unset takes DefaultLowBatteryCellMV.
	LowBatteryCellMV *int `yaml:"low_battery_cell_mv"`
	// OfflineAfterSec is how long a device stays silent before it reads as
	// offline. Unset takes DefaultOfflineAfterSec.
	OfflineAfterSec *int `yaml:"offline_after_sec"`
	// FlowLimitC is the heating flow limit (c/<N>/config tag 2040) in whole
	// degrees that a circuit gets when nothing else sets one. Unset takes
	// DefaultFlowLimitC.
	FlowLimitC *int `yaml:"flow_limit_c"`
	// FlowCurves holds the weather compensation curve of a heating circuit,
	// keyed by circuit id. A circuit without an enabled curve keeps a fixed flow
	// limit.
	FlowCurves map[int]FlowCurve `yaml:"flow_curves"`
	// OutdoorMaxAgeSec is how long an outdoor temperature reading drives the
	// curves. Unset takes DefaultOutdoorMaxAgeSec.
	OutdoorMaxAgeSec *int `yaml:"outdoor_max_age_sec"`
	// FlowHysteresisC is the smallest change of a computed flow limit that is
	// served. Unset takes DefaultFlowHysteresisC.
	FlowHysteresisC *int `yaml:"flow_hysteresis_c"`
	// FlowMinIntervalSec is the shortest time between two curve changes of a
	// circuit's flow limit. Unset takes DefaultFlowMinIntervalSec.
	FlowMinIntervalSec *int `yaml:"flow_min_interval_sec"`
	// EarlyStartZones holds the heating zones whose warmer program blocks the
	// daemon starts early, from the heat-up rate it learns per zone. A zone
	// outside the map starts each block on time.
	EarlyStartZones map[int]bool `yaml:"early_start_zones"`
	// EarlyStartMaxLeadSec caps how early a block starts. Unset takes
	// DefaultEarlyStartMaxLeadSec.
	EarlyStartMaxLeadSec *int `yaml:"early_start_max_lead_sec"`
	// EarlyStartMinEpisodes is how many accepted heat-up episodes a zone needs
	// before it starts a block early. Unset takes DefaultEarlyStartMinEpisodes.
	EarlyStartMinEpisodes *int `yaml:"early_start_min_episodes"`
	// DeviceOverlayEnds holds the end rule of an overlay set on a device, per
	// heating zone. A zone outside the map takes DefaultDeviceOverlayEnd.
	DeviceOverlayEnds map[int]OverlayEnd `yaml:"device_overlay_end"`
	// ZoneOffsets holds the room temperature correction mode, per heating
	// zone. A zone outside the map takes DefaultZoneOffset.
	ZoneOffsets map[int]ZoneOffset `yaml:"zone_offset"`
	// ZonePIDs holds the zone controller gains, per heating zone. A zone
	// outside the map takes DefaultZonePID.
	ZonePIDs map[int]ZonePID `yaml:"zone_pid"`
	// AwayTemperatures holds the temperature a heating zone keeps in away, per
	// zone. A zone outside the map, or at RoomMinC, is off in away.
	AwayTemperatures map[int]float64 `yaml:"away_temperature_c"`
	// BoilerMaxPowerKW is the heat output of the boiler at 100 % modulation, in
	// kW. Without it the daemon does not estimate the boiler energy.
	BoilerMaxPowerKW *float64 `yaml:"boiler_max_power_kw"`
	// BoilerMinPowerKW is the heat output at 0 % modulation with the burner on.
	// Unset takes 0.
	BoilerMinPowerKW *float64 `yaml:"boiler_min_power_kw"`
}

// DefaultOfflineSchedule is tag 02b3 for an installation that states no
// options.offline_schedule.
const DefaultOfflineSchedule = true

// DefaultExternalWindowMaxSec is the external open window cap of an
// installation that states no options.external_window_max_sec.
const DefaultExternalWindowMaxSec = 2 * 60 * 60

// DefaultLowBatteryCellMV is 2200 mV on a valve and 3300 mV on a wall
// thermostat. The captures hold valves at 2381..2626 mV in service, one fresh
// pair at 3039..3060 mV and the thermostat at 3701..3735 mV.
const DefaultLowBatteryCellMV = 1100

// DefaultOfflineAfterSec is three periods of the periodic sensor report, which
// the captures show every 900..1190 s on valves and thermostat alike.
const DefaultOfflineAfterSec = 60 * 60

// batteryCells is the cell count of each battery-powered kind: two AA in a
// valve, three AAA in the wall thermostat.
var batteryCells = map[DeviceKind]int{KindValve: 2, KindThermostat: 3}

// LowBatteryMV returns the low battery threshold of a device kind, 0 for a kind
// that runs on no battery.
func (o Options) LowBatteryMV(k DeviceKind) int {
	return batteryCells[k] * o.LowBatteryCell()
}

// FullBatteryCellMV is the per-cell voltage that reads as a full battery. The
// captures hold a fresh valve pair at 3039..3060 mV, 1520..1530 mV per cell.
const FullBatteryCellMV = 1500

// BatteryHysteresisCellMV is how far above the low battery voltage, per cell, a
// device must read before a raised low battery clears. A dead cell recovers some
// voltage at rest, and that recovery must not clear the alert.
const BatteryHysteresisCellMV = 50

// BatteryPct maps a battery voltage of a device kind to a charge percentage:
// linear per cell from LowBatteryCell at 0 % to FullBatteryCellMV at 100 %. The
// low battery alert fires as the percentage reaches 0. It returns 0 for a kind
// that runs on no battery.
func (o Options) BatteryPct(k DeviceKind, mV int) int {
	cells := batteryCells[k]
	if cells == 0 || mV <= 0 {
		return 0
	}
	empty, span := cells*o.LowBatteryCell(), cells*(FullBatteryCellMV-o.LowBatteryCell())
	if span <= 0 {
		if mV >= empty {
			return 100
		}
		return 0
	}
	return max(0, min(100, (mV-empty)*100/span))
}

// lowBatteryClearMV is the voltage a device with a raised low battery must reach
// before the alert clears.
func (o Options) lowBatteryClearMV(k DeviceKind) int {
	return batteryCells[k] * (o.LowBatteryCell() + BatteryHysteresisCellMV)
}

// LowBatteryCell returns the per-cell low battery voltage in mV.
func (o Options) LowBatteryCell() int {
	if o.LowBatteryCellMV != nil {
		return *o.LowBatteryCellMV
	}
	return DefaultLowBatteryCellMV
}

// OfflineAfter reports how long a silent device takes to read as offline.
func (o Options) OfflineAfter() time.Duration {
	sec := DefaultOfflineAfterSec
	if o.OfflineAfterSec != nil {
		sec = *o.OfflineAfterSec
	}
	return time.Duration(sec) * time.Second
}

// ExternalWindowMax reports how long an external open window lasts without a
// close.
func (o Options) ExternalWindowMax() time.Duration {
	sec := DefaultExternalWindowMaxSec
	if o.ExternalWindowMaxSec != nil {
		sec = *o.ExternalWindowMaxSec
	}
	return time.Duration(sec) * time.Second
}

// OfflineScheduleEnabled reports the installation's tag 02b3.
func (o Options) OfflineScheduleEnabled() bool {
	if o.OfflineSchedule == nil {
		return DefaultOfflineSchedule
	}
	return *o.OfflineSchedule
}

// refList accepts both "reporter: VA1" and "reporter: [VA1, VA2]".
type refList []string

// UnmarshalYAML implements yaml.Unmarshaler.
func (r *refList) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		if n.Tag == "!!null" {
			*r = nil
			return nil
		}
		*r = refList{n.Value}
		return nil
	}
	var s []string
	if err := n.Decode(&s); err != nil {
		return err
	}
	*r = refList(s)
	return nil
}

// AmbiguousRefError reports a reference that matches several devices, naming
// every candidate. An installation declared before the management API kept
// labels unique still holds duplicates, so resolving a room label can land
// here.
type AmbiguousRefError struct {
	Ref        string
	Candidates []string
}

func (e *AmbiguousRefError) Error() string {
	return fmt.Sprintf("tado: config: %q matches %d devices (%s): use a serial or an EUI",
		e.Ref, len(e.Candidates), strings.Join(e.Candidates, ", "))
}

// Parse decodes an installation config and validates it.
func Parse(data []byte) (*Install, error) {
	in := &Install{}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(in); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("tado: config: empty document")
		}
		return nil, fmt.Errorf("tado: config: %w", err)
	}
	if err := in.Validate(); err != nil {
		return nil, err
	}
	return in, nil
}

// issues collects the two severities of a validation pass.
type issues struct {
	errs  []error
	warns []string
}

func (c *issues) errf(path, format string, a ...any) {
	c.errs = append(c.errs, fmt.Errorf("tado: config: %s: %s", path, fmt.Sprintf(format, a...)))
}

func (c *issues) warnf(path, format string, a ...any) {
	c.warns = append(c.warns, fmt.Sprintf("tado: config: %s: %s", path, fmt.Sprintf(format, a...)))
}

// Validate checks the whole config and resolves the network key. It returns
// every hard error joined together, and leaves the warnings for Warnings().
// Re-running it is safe: warnings are recomputed, not appended.
func (in *Install) Validate() error { return in.ValidateCommissionable(false) }

// ValidateCommissionable is Validate with a commissioning escape hatch. With
// allowEmpty an installation that declares no device passes with a warning
// instead of the "no device declared" error, which is what a daemon on a fresh
// database has before the first device is paired and what the management API
// has between "create the zone" and "pair the first member". The identity
// checks stay mandatory in both modes: without a key, a home id and a
// coordinator address nothing can be commissioned at all.
func (in *Install) ValidateCommissionable(allowEmpty bool) error {
	c := &issues{}
	in.warnings = nil

	if in.Version != ConfigVersion {
		c.errf("version", "unsupported schema version %d, this build reads %d", in.Version, ConfigVersion)
	}
	if in.HomeID == 0 {
		c.errf("home_id", "missing: the resource paths are h/<home_id>/... and the devices echo it")
	}
	in.checkKey(c)
	in.checkCoordinator(c)
	if allowEmpty && in.isEmpty() {
		c.warnf("devices", "installation empty, waiting for commissioning")
	} else {
		in.checkDevices(c, allowEmpty)
		in.checkZones(c)
		in.checkCircuits(c)
	}
	in.checkOptions(c)

	in.warnings = c.warns
	return errors.Join(c.errs...)
}

// isEmpty reports an installation that declares nothing to coordinate.
func (in *Install) isEmpty() bool {
	return len(in.Devices) == 0 && len(in.Zones) == 0 && len(in.Circuits) == 0
}

// Warnings returns the non-fatal findings of the last Validate, for the daemon
// to print at startup.
func (in *Install) Warnings() []string { return in.warnings }

func (in *Install) checkKey(c *issues) {
	in.key = nil
	if in.NetworkKeyHex == "" {
		c.errf("network_key", "missing: the 16-byte PAN key is required to validate protected frames")
		return
	}
	raw := strings.TrimSpace(in.NetworkKeyHex)
	k, err := hex.DecodeString(raw)
	if err != nil {
		c.errf("network_key", "not hex: %q", elide(raw))
		return
	}
	if len(k) != 16 {
		c.errf("network_key", "key must be 16 bytes (32 hex digits), got %d", len(k))
		return
	}
	in.key = k
}

func (in *Install) checkCoordinator(c *issues) {
	if in.Coordinator.EUI == "" {
		c.errf("coordinator.eui", "missing: this is the address the devices see as PAN coordinator")
		return
	}
	b, err := ParseEUI(in.Coordinator.EUI)
	if err != nil {
		c.errf("coordinator.eui", "%v", err)
		return
	}
	if !bytes.Equal(b[len(b)-len(EUISuffix):], EUISuffix) {
		c.warnf("coordinator.eui", "%q does not end in the tado suffix %s: devices filter on it",
			in.Coordinator.EUI, hex.EncodeToString(EUISuffix))
	}
}

func (in *Install) checkDevices(c *issues, allowEmpty bool) {
	if len(in.Devices) == 0 {
		// A zone or a circuit can already exist here: the management API builds
		// the installation one row at a time.
		if allowEmpty {
			c.warnf("devices", "no device declared, waiting for commissioning")
			return
		}
		c.errf("devices", "no device declared")
		return
	}
	coord, _ := ParseEUI(in.Coordinator.EUI)
	seenEUI := map[string]int{}
	seenSerial := map[string]int{}
	seenLabel := map[string]int{}
	for i := range in.Devices {
		d := &in.Devices[i]
		p := fmt.Sprintf("devices[%d]", i)
		switch d.Kind {
		case KindValve, KindThermostat, KindBridge:
		case "":
			c.errf(p+".kind", "missing: one of valve, thermostat, bridge")
		default:
			c.errf(p+".kind", "unknown kind %q: one of valve, thermostat, bridge", d.Kind)
		}
		b, err := ParseEUI(d.EUI)
		if err != nil {
			c.errf(p+".eui", "%v", err)
		} else {
			h := hex.EncodeToString(b)
			if j, dup := seenEUI[h]; dup {
				c.errf(p+".eui", "%q duplicates devices[%d]", d.EUI, j)
			} else {
				seenEUI[h] = i
			}
			if coord != nil && bytes.Equal(b, coord) {
				c.warnf(p+".eui", "%q is the coordinator EUI: we answer for that address ourselves", d.EUI)
			}
		}
		if d.Serial != "" {
			s := strings.ToLower(d.Serial)
			if j, dup := seenSerial[s]; dup {
				c.errf(p+".serial", "%q duplicates devices[%d]", d.Serial, j)
			} else {
				seenSerial[s] = i
			}
		}
		if d.Label != "" {
			// A warning, not an error: an installation declared before the
			// management API held the rule still has to load.
			l := strings.ToLower(d.Label)
			if j, dup := seenLabel[l]; dup {
				c.warnf(p+".label", "%q duplicates devices[%d]: two devices print the same name", d.Label, j)
			} else {
				seenLabel[l] = i
			}
		}
	}
	in.checkDeviceZones(c)
}

func (in *Install) checkDeviceZones(c *issues) {
	for i := range in.Devices {
		d := &in.Devices[i]
		p := fmt.Sprintf("devices[%d].zone", i)
		member := in.zonesOf(d)
		if d.Zone == nil {
			// Only a valve is degenerate this way: it accepts a single logical
			// id. A thermostat indexes a real table and sits in several zones.
			if d.Kind == KindValve && len(member) > 1 {
				c.warnf(p, "%s is a member of zones %s but declares no zone: a valve accepts only one logical id",
					d.Ref(), joinInts(member))
			}
			continue
		}
		if _, err := in.ZoneByNumber(*d.Zone); err != nil {
			c.errf(p, "%s claims zone %d, which is not declared", d.Ref(), *d.Zone)
			continue
		}
		if !containsInt(member, *d.Zone) {
			c.errf(p, "%s claims zone %d but is not among zones[number=%d].members",
				d.Ref(), *d.Zone, *d.Zone)
		}
	}
}

func (in *Install) checkZones(c *issues) {
	seen := map[int]int{}
	dhw := 0
	for i := range in.Zones {
		z := &in.Zones[i]
		p := fmt.Sprintf("zones[%d]", i)
		switch {
		case z.Number > maxSlotID:
			c.errf(p+".number", "%d is out of range: the per-zone attribute id carries 5 bits (id & 0x1f)", z.Number)
		case z.Number < 0:
			c.errf(p+".number", "%d is negative", z.Number)
		case z.Number > maxTableSlot:
			c.warnf(p+".number", "%d is above %d: fine on air (zone 17 runs on this installation), but one thermostat aggregates at most %d zones per circuit",
				z.Number, maxTableSlot, maxTableSlot+1)
		}
		if j, dup := seen[z.Number]; dup {
			c.errf(p+".number", "%d duplicates zones[%d]", z.Number, j)
		} else {
			seen[z.Number] = i
		}
		switch z.Kind {
		case ZoneHeating:
		case ZoneHotWater:
			dhw++
		case "":
			c.errf(p+".kind", "missing: one of heating, hotwater")
		default:
			c.errf(p+".kind", "unknown kind %q: one of heating, hotwater", z.Kind)
		}
		members := in.checkMembers(c, p, z)
		in.checkReporter(c, p, z, members)
		in.checkMeasuringDevice(c, p, z, members)
		in.checkZoneCircuit(c, p, z)
		checkZoneOpenWindow(c, p, z)
	}
	if dhw > 1 {
		c.errf("zones", "%d hot-water zones declared, the boiler serves at most one", dhw)
	}
}

func checkZoneOpenWindow(c *issues, p string, z *Zone) {
	if d := z.OpenWindowDurationSec; d != nil && (*d < 0 || *d > zoneOpenWindowDurationMax) {
		c.errf(p+".open_window_duration_sec", "%d is out of range: tag 62c0 carries a uint16", *d)
	}
}

func (in *Install) checkMeasuringDevice(c *issues, p string, z *Zone, members []*Device) {
	if z.Kind != ZoneHeating || z.MeasuringDevice == "" {
		return
	}
	d, err := in.Resolve(z.MeasuringDevice)
	if err != nil {
		c.errf(p+".measuring_device", "%v", err)
		return
	}
	if !containsDevice(members, d) {
		c.errf(p+".measuring_device", "%s is not a member of zone %d", d.Ref(), z.Number)
	}
}

func (in *Install) checkMembers(c *issues, p string, z *Zone) []*Device {
	if len(z.Members) == 0 {
		c.warnf(p+".members", "zone %d has no member: nothing serves or reports it", z.Number)
		return nil
	}
	out := make([]*Device, 0, len(z.Members))
	for j, ref := range z.Members {
		d, err := in.Resolve(ref)
		if err != nil {
			c.errf(fmt.Sprintf("%s.members[%d]", p, j), "%v", err)
			continue
		}
		out = append(out, d)
	}
	if z.Kind == ZoneHotWater && !hasKind(out, KindThermostat) {
		c.warnf(p+".members", "hot-water zone %d has no thermostat member: only the wall thermostat is wired to the boiler", z.Number)
	}
	return out
}

// checkReporter validates the demand-reporter election. The election is
// optional: a live installation runs with none declared, because the role is
// already in each device's own store and reaches it in d/config tag 015e. What
// is declared still has to name one resolvable member.
func (in *Install) checkReporter(c *issues, p string, z *Zone, members []*Device) {
	if z.Kind != ZoneHeating {
		if len(z.Reporter) > 0 {
			c.warnf(p+".reporter", "hot-water zone %d elects a demand reporter: the firmware elects one only for kind heating", z.Number)
		}
		return
	}
	switch len(z.Reporter) {
	case 1:
	case 0:
		c.warnf(p+".reporter", "heating zone %d elects no demand reporter: its d/config 015e records carry role 0x05 for every member, so the coordinator writes no primary bit", z.Number)
		return
	default:
		c.errf(p+".reporter", "heating zone %d elects %d demand reporters (%s): the 015e primary bit admits exactly one",
			z.Number, len(z.Reporter), strings.Join(z.Reporter, ", "))
		return
	}
	d, err := in.Resolve(z.Reporter[0])
	if err != nil {
		c.errf(p+".reporter", "%v", err)
		return
	}
	if !containsDevice(members, d) {
		c.errf(p+".reporter", "%s is not a member of zone %d", d.Ref(), z.Number)
	}
}

func (in *Install) checkZoneCircuit(c *issues, p string, z *Zone) {
	if z.Circuit == nil {
		if z.Kind == ZoneHeating {
			c.warnf(p+".circuit", "heating zone %d binds to no circuit: it falls back to circuit 9, whose demand nobody publishes", z.Number)
		}
		return
	}
	if z.Kind == ZoneHotWater {
		c.warnf(p+".circuit", "hot-water zone %d binds to a circuit: the DHW slot publishes over the inter-MCU link instead", z.Number)
	}
	if _, err := in.CircuitByID(*z.Circuit); err != nil {
		c.errf(p+".circuit", "zone %d binds to circuit %d, which is not declared", z.Number, *z.Circuit)
	}
}

func (in *Install) checkCircuits(c *issues) {
	seen := map[int]int{}
	for i := range in.Circuits {
		cir := &in.Circuits[i]
		p := fmt.Sprintf("circuits[%d]", i)
		switch {
		case cir.ID > maxSlotID:
			c.errf(p+".id", "%d is out of range: the circuit id composes into the published attribute id (0x4080|c)", cir.ID)
		case cir.ID < 0:
			c.errf(p+".id", "%d is negative", cir.ID)
		case cir.ID > maxTableSlot:
			c.warnf(p+".id", "%d indexes a slot above %d: the firmware circuit table holds 11 entries", cir.ID, maxTableSlot)
		}
		if j, dup := seen[cir.ID]; dup {
			c.errf(p+".id", "%d duplicates circuits[%d]", cir.ID, j)
		} else {
			seen[cir.ID] = i
		}
		if cir.HeatSource == "" {
			c.errf(p+".heat_source", "circuit %d has no heat source: name the wall thermostat that drives it", cir.ID)
			continue
		}
		d, err := in.Resolve(cir.HeatSource)
		if err != nil {
			c.errf(p+".heat_source", "%v", err)
			continue
		}
		if d.Kind != KindThermostat {
			c.errf(p+".heat_source", "%s is a %s: only a wall thermostat aggregates the circuit demand and drives OpenTherm",
				d.Ref(), d.Kind)
		}
	}
}

func (in *Install) checkOptions(c *issues) {
	// The bound is ConfigWrite.Validate's, since this value ends up in a
	// ConfigWrite: reuse the check instead of restating it.
	if o := in.Options.UTCOffsetMin; o != nil {
		if err := (ConfigWrite{UTCOffsetMin: o}).Validate(); err != nil {
			c.errf("options.utc_offset_min", "%s", strings.TrimPrefix(err.Error(), "tado: "))
		}
	}
	if tz := in.Options.Timezone; tz != "" {
		if _, err := LoadTimezone(tz); err != nil {
			c.errf("options.timezone", "%v", err)
		}
	}
	if o := in.Options.UTCPushSpacingSec; o != nil && *o <= 0 {
		c.errf("options.utc_push_spacing_sec", "%d is not a positive number of seconds", *o)
	}
	if o := in.Options.ExternalWindowMaxSec; o != nil && *o <= 0 {
		c.errf("options.external_window_max_sec", "%d is not a positive number of seconds", *o)
	}
	if o := in.Options.LowBatteryCellMV; o != nil && *o <= 0 {
		c.errf("options.low_battery_cell_mv", "%d is not a positive voltage", *o)
	}
	if o := in.Options.OfflineAfterSec; o != nil && *o <= 0 {
		c.errf("options.offline_after_sec", "%d is not a positive number of seconds", *o)
	}
	in.checkFlowOptions(c)
	in.checkEarlyStartOptions(c)
	in.checkBoilerPower(c)
	in.checkDeviceOverlayEnds(c)
	in.checkZoneOffsets(c)
	in.checkZonePIDs(c)
	in.checkAwayTemperatures(c)
}

// NetworkKey returns the resolved 16-byte PAN key. It is nil until Validate ran.
func (in *Install) NetworkKey() []byte { return in.key }

// CoordinatorEUI returns our own 8-byte wire EUI.
func (in *Install) CoordinatorEUI() []byte {
	b, err := ParseEUI(in.Coordinator.EUI)
	if err != nil {
		return nil
	}
	return b
}

// Resolve finds the device a target string names: serial, wire EUI (either
// spelling), short address or room label, case-insensitive. Unique identifiers
// win over labels. A label shared by several devices returns an
// *AmbiguousRefError naming all of them instead of picking one, which is where
// this differs from NodeByAny.
func (in *Install) Resolve(ref string) (*Device, error) {
	s := strings.ToLower(strings.TrimSpace(ref))
	if s == "" {
		return nil, errors.New("tado: config: empty device reference")
	}
	var wire string
	if b, err := ParseEUI(s); err == nil {
		wire = hex.EncodeToString(b)
	}
	for i := range in.Devices {
		d := &in.Devices[i]
		if strings.ToLower(d.Serial) == s || d.Short() == s || (wire != "" && d.wireHex() == wire) {
			return d, nil
		}
	}
	var m []*Device
	for i := range in.Devices {
		if d := &in.Devices[i]; d.Label != "" && strings.ToLower(d.Label) == s {
			m = append(m, d)
		}
	}
	switch len(m) {
	case 1:
		return m[0], nil
	case 0:
		return nil, fmt.Errorf("tado: config: unknown device %q", ref)
	}
	cand := make([]string, len(m))
	for i, d := range m {
		cand[i] = d.Ref()
	}
	return nil, &AmbiguousRefError{Ref: ref, Candidates: cand}
}

// DevicesByLabel returns every device carrying that room label. It answers a
// list because an older installation can still hold the same label twice.
func (in *Install) DevicesByLabel(label string) []*Device {
	s := strings.ToLower(strings.TrimSpace(label))
	var out []*Device
	for i := range in.Devices {
		if d := &in.Devices[i]; d.Label != "" && strings.ToLower(d.Label) == s {
			out = append(out, d)
		}
	}
	return out
}

// DevicesByKind returns every device of one kind, in declaration order.
func (in *Install) DevicesByKind(k DeviceKind) []*Device {
	var out []*Device
	for i := range in.Devices {
		if d := &in.Devices[i]; d.Kind == k {
			out = append(out, d)
		}
	}
	return out
}

// ZoneByNumber returns the zone with that number.
func (in *Install) ZoneByNumber(n int) (*Zone, error) {
	for i := range in.Zones {
		if in.Zones[i].Number == n {
			return &in.Zones[i], nil
		}
	}
	return nil, fmt.Errorf("tado: config: no zone %d", n)
}

// ZoneMembers resolves the members of a zone, in declaration order.
func (in *Install) ZoneMembers(n int) ([]*Device, error) {
	z, err := in.ZoneByNumber(n)
	if err != nil {
		return nil, err
	}
	out := make([]*Device, 0, len(z.Members))
	for _, ref := range z.Members {
		d, err := in.Resolve(ref)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

// DemandReporter returns the member elected to publish the zone demand.
func (in *Install) DemandReporter(n int) (*Device, error) {
	z, err := in.ZoneByNumber(n)
	if err != nil {
		return nil, err
	}
	if len(z.Reporter) != 1 {
		return nil, fmt.Errorf("tado: config: zone %d elects %d demand reporters, want exactly 1", n, len(z.Reporter))
	}
	return in.Resolve(z.Reporter[0])
}

// MeasuringDevice returns the member whose temperature represents the zone.
// A config that names none falls back to the demand reporter, then to the only
// member of a single-member zone; both match the usual single-sensor layout and
// stay explicit in the rendered API.
func (in *Install) MeasuringDevice(n int) (*Device, error) {
	z, err := in.ZoneByNumber(n)
	if err != nil {
		return nil, err
	}
	if z.Kind != ZoneHeating {
		return nil, fmt.Errorf("tado: config: zone %d is not a heating zone", n)
	}
	if z.MeasuringDevice != "" {
		return in.Resolve(z.MeasuringDevice)
	}
	if d, err := in.DemandReporter(n); err == nil {
		return d, nil
	}
	members, err := in.ZoneMembers(n)
	if err != nil {
		return nil, err
	}
	if len(members) == 1 {
		return members[0], nil
	}
	return nil, fmt.Errorf("tado: config: zone %d names no measuring device and has %d members", n, len(members))
}

// HotWaterZone returns the single hot-water zone, if the install has one.
func (in *Install) HotWaterZone() (*Zone, bool) {
	for i := range in.Zones {
		if in.Zones[i].Kind == ZoneHotWater {
			return &in.Zones[i], true
		}
	}
	return nil, false
}

// HotWaterTarget returns the thermostat that serves the hot-water zone: the
// per-install answer to the former dhwNode constant.
func (in *Install) HotWaterTarget() (*Device, error) {
	z, ok := in.HotWaterZone()
	if !ok {
		return nil, errors.New("tado: config: no hot-water zone declared")
	}
	members, err := in.ZoneMembers(z.Number)
	if err != nil {
		return nil, err
	}
	var out []*Device
	for _, d := range members {
		if d.Kind == KindThermostat {
			out = append(out, d)
		}
	}
	switch len(out) {
	case 1:
		return out[0], nil
	case 0:
		return nil, fmt.Errorf("tado: config: hot-water zone %d has no thermostat member", z.Number)
	}
	return nil, fmt.Errorf("tado: config: hot-water zone %d has %d thermostat members", z.Number, len(out))
}

// CircuitByID returns the circuit with that id.
func (in *Install) CircuitByID(id int) (*Circuit, error) {
	for i := range in.Circuits {
		if in.Circuits[i].ID == id {
			return &in.Circuits[i], nil
		}
	}
	return nil, fmt.Errorf("tado: config: no circuit %d", id)
}

// CircuitsFor returns the circuits a thermostat is the heat source of, in
// declaration order. A thermostat normally drives exactly one.
func (in *Install) CircuitsFor(d *Device) []*Circuit {
	var out []*Circuit
	for i := range in.Circuits {
		if src, err := in.Resolve(in.Circuits[i].HeatSource); err == nil && src == d {
			out = append(out, &in.Circuits[i])
		}
	}
	return out
}

// ZoneCircuit returns the circuit a zone binds to.
func (in *Install) ZoneCircuit(n int) (*Circuit, error) {
	z, err := in.ZoneByNumber(n)
	if err != nil {
		return nil, err
	}
	if z.Circuit == nil {
		return nil, fmt.Errorf("tado: config: zone %d binds to no circuit", n)
	}
	return in.CircuitByID(*z.Circuit)
}

// HeatSource returns the wall thermostat that drives a circuit.
func (in *Install) HeatSource(circuitID int) (*Device, error) {
	c, err := in.CircuitByID(circuitID)
	if err != nil {
		return nil, err
	}
	return in.Resolve(c.HeatSource)
}

// ZoneHeatSource returns the wall thermostat the zone's demand ends up at.
func (in *Install) ZoneHeatSource(n int) (*Device, error) {
	c, err := in.ZoneCircuit(n)
	if err != nil {
		return nil, err
	}
	return in.Resolve(c.HeatSource)
}

// EmuZones renders the zone map as EmuConfig.Zones expects it: zone number to
// member references, given as wire EUIs so nothing has to be resolved again.
func (in *Install) EmuZones() map[int][]string {
	out := make(map[int][]string, len(in.Zones))
	for i := range in.Zones {
		z := &in.Zones[i]
		refs := make([]string, 0, len(z.Members))
		for _, ref := range z.Members {
			d, err := in.Resolve(ref)
			if err != nil {
				continue
			}
			refs = append(refs, d.wireHex())
		}
		out[z.Number] = refs
	}
	return out
}

// EmuConfig fills the emulator fields this config owns. The caller still sets
// Now, the bootstrap knobs and the No* gates.
func (in *Install) EmuConfig() EmuConfig {
	return EmuConfig{
		Install:      in,
		Key:          in.NetworkKey(),
		SelfEUI:      in.CoordinatorEUI(),
		Zones:        in.EmuZones(),
		UTCOffsetMin: in.Options.UTCOffsetMin,
	}
}

// WireEUI returns the 8-byte little-endian address as seen on air, or nil if
// the config spelling is unusable.
func (d *Device) WireEUI() []byte {
	b, err := ParseEUI(d.EUI)
	if err != nil {
		return nil
	}
	return b
}

// Short returns the 16-bit short address, hex.
func (d *Device) Short() string { return ShortFromWire(d.WireEUI()) }

// Ref is the most specific identifier of the device, for messages.
func (d *Device) Ref() string {
	switch {
	case d.Serial != "" && d.Label != "":
		return d.Serial + " (" + d.Label + ")"
	case d.Serial != "":
		return d.Serial
	case d.EUI != "" && d.Label != "":
		return d.EUI + " (" + d.Label + ")"
	case d.EUI != "":
		return d.EUI
	}
	return d.Label
}

func (d *Device) wireHex() string {
	b := d.WireEUI()
	if b == nil {
		return ""
	}
	return hex.EncodeToString(b)
}

// ParseEUI accepts the two spellings of a node address and always returns the
// 8-byte wire (little-endian) form: bare hex is read as wire order, the colon
// or dash form as the canonical big-endian EUI-64 and reversed.
func ParseEUI(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("missing EUI")
	}
	canonical := strings.ContainsAny(s, ":-")
	b, err := hex.DecodeString(strings.ToLower(strings.NewReplacer(":", "", "-", "").Replace(s)))
	if err != nil {
		return nil, fmt.Errorf("%q is not hex", s)
	}
	if len(b) != 8 {
		return nil, fmt.Errorf("%q must be 8 bytes (16 hex digits), got %d", s, len(b))
	}
	if canonical {
		for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
			b[i], b[j] = b[j], b[i]
		}
	}
	return b, nil
}

// zonesOf lists the zone numbers a device is a member of.
func (in *Install) zonesOf(d *Device) []int {
	var out []int
	for i := range in.Zones {
		z := &in.Zones[i]
		for _, ref := range z.Members {
			if m, err := in.Resolve(ref); err == nil && m == d {
				out = append(out, z.Number)
				break
			}
		}
	}
	return out
}

func hasKind(ds []*Device, k DeviceKind) bool {
	for _, d := range ds {
		if d.Kind == k {
			return true
		}
	}
	return false
}

func containsDevice(ds []*Device, want *Device) bool {
	for _, d := range ds {
		if d == want {
			return true
		}
	}
	return false
}

func containsInt(xs []int, want int) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func joinInts(xs []int) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = fmt.Sprint(x)
	}
	return strings.Join(parts, ", ")
}

func elide(s string) string {
	if len(s) > 24 {
		return s[:24] + "..."
	}
	return s
}
