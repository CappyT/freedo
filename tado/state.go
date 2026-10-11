package tado

import (
	"slices"
	"sort"
	"strings"
	"time"
)

// Derived view model: the emulator's raw TLV cache turned into the numbers a UI
// or an HA bridge consumes. Nothing here is on-air state, it is a projection of
// NodeState, so a value the network never reported reads as its zero value,
// except the boiler bus values, which read nil.

// ValveView is one device as a consumer sees it.
type ValveView struct {
	Short          string            `json:"Short"`
	Serial         string            `json:"Serial"`
	Room           string            `json:"Room"`
	TempC          float64           `json:"TempC"`
	SetpointC      float64           `json:"SetpointC"`
	SchedSetpointC float64           `json:"SchedSetpointC"`
	HumidityPct    float64           `json:"HumidityPct"`
	Mode           string            `json:"Mode"`
	OpenWindow     bool              `json:"OpenWindow"`
	Telemetry      map[string]string `json:"Telemetry"`
	// HeatDemandPct comes from tag 40a0: this device's own PID demand for its
	// zone, 0..100, computed by zone_heat_demand_report@0x2b69c and reported
	// only - it does not drive the valve motor. 0 when the node never reported
	// the tag, which is also what an unelected device sends.
	HeatDemandPct int `json:"HeatDemandPct"`
	// BatteryMV is the last reading of tag 0162. BatteryPct and LowBattery read
	// the highest reading of the last BatteryWindow, so a dip under motor load
	// moves neither.
	BatteryMV  int `json:"BatteryMV"`
	BatteryPct int `json:"BatteryPct"`
	// FaultWord is tag 01a3 of d/<serial>/err, 0 when no fault or never reported.
	FaultWord uint32 `json:"FaultWord"`
	// FallbackMode is tag 0182 of d/<serial>/fallback, the device-level mode:
	// 0 none, 1 hold, 2 fixed, 3 frost, 0x7f unset. 0 when never reported.
	FallbackMode int    `json:"FallbackMode"`
	RelayState   int    `json:"RelayState"`
	RxCounter    uint32 `json:"RxCounter"`
	CSLPhase     uint16 `json:"CSLPhase"`
	CSLPeriod    uint16 `json:"CSLPeriod"`
	LastSeenUnix int64  `json:"LastSeenUnix"`
	// SessionIssued says we served this device a session id on auth/token, in
	// this run or in an earlier one the database remembers; SessionIssuedUnix is
	// when. SessionAdopted says a request of it since the start or the last
	// auth/token carried that id as option 2048.
	SessionIssued     bool  `json:"SessionIssued"`
	SessionAdopted    bool  `json:"SessionAdopted"`
	SessionIssuedUnix int64 `json:"SessionIssuedUnix"`
	// LowBattery follows the installation's low battery voltage with a
	// hysteresis of BatteryHysteresisCellMV. Fault, CommsWarnings and Offline
	// come from the daemon: the engine leaves them false and null. Fault is set
	// by the bits of FaultWord that are no communication warning, which
	// CommsWarnings names (see DecodeFaultWord), null with none.
	LowBattery    bool     `json:"LowBattery"`
	Fault         bool     `json:"Fault"`
	CommsWarnings []string `json:"CommsWarnings"`
	Offline       bool     `json:"Offline"`
	// Display is the display part of a valve's served d/config, set by the
	// daemon; null on any other device.
	Display *DisplaySettings `json:"Display"`
	// ChildLock is the d/lock state freedo serves to a valve, set by the daemon:
	// what the valve took on its last confirmed write, false with none. Null on
	// any other device.
	ChildLock *bool `json:"ChildLock"`
}

// BoilerView is the single heat source, assembled from whichever node reports
// the boiler and wall-thermostat tags. The boiler values come from the bus
// catalogue (hvacTags) the wall thermostat relays, read-only with no set path.
// A boiler value is null until the boiler reports it, and while the boiler
// lists it as unsupported in tag 0481.
type BoilerView struct {
	Present     bool   `json:"Present"`
	SourceShort string `json:"SourceShort"`
	// BusType is the boiler bus family that InterfaceModule, the tag 015d the
	// wall thermostat reported, selects. Empty and null until it reports one.
	BusType         string  `json:"BusType"`
	InterfaceModule *uint16 `json:"InterfaceModule"`
	// BusDetect is tag 0167 of hvac/det/bus, a detect flag only eBus B sends.
	BusDetect     *int     `json:"BusDetect"`
	FlowC         *float64 `json:"FlowC"`
	DHWC          *float64 `json:"DHWC"`
	DHWSetpointC  *float64 `json:"DHWSetpointC"` // tag 045b: reported by the thermostat, never set by the daemon
	ModulationPct *int     `json:"ModulationPct"`
	Active        bool     `json:"Active"`
	HeatActive    bool     `json:"HeatActive"`
	// CircuitDemandPct is tag 4080: the heating circuit's aggregated demand,
	// the max over the zones bound to it, which is what drives the boiler.
	CircuitDemandPct int `json:"CircuitDemandPct"`
	// LeadZoneTempC (tag 4040) and LeadZoneTargetC (tag 4000) come from
	// h/<home>/c/<id>/act. They report the zone that drives the circuit, so they
	// follow whichever zone leads and are not the wall thermostat's own readings.
	LeadZoneTempC   float64 `json:"LeadZoneTempC"`
	LeadZoneTargetC float64 `json:"LeadZoneTargetC"`

	ReturnC          *float64 `json:"ReturnC"`
	OutsideC         *float64 `json:"OutsideC"`
	ControlSetpointC *float64 `json:"ControlSetpointC"`
	MaxChSetpointC   *float64 `json:"MaxChSetpointC"`
	DhwFlowLmin      *float64 `json:"DhwFlowLmin"`
	ChPressureBar    *float64 `json:"ChPressureBar"`
	BurnerStarts     *uint32  `json:"BurnerStarts"`
	BurnerHoursMin   *uint32  `json:"BurnerHoursMin"`
	// FaultCodes are the 0460 codes of the last hvac/codes report, null before
	// one. FaultCode is the first of them, 0 with none.
	FaultCodes []uint32 `json:"FaultCodes"`
	FaultCode  int      `json:"FaultCode"`
	// Unsupported names, sorted, the attributes the boiler lists in tag 0481.
	Unsupported []string `json:"Unsupported"`

	// Every catalogue attribute the boiler reported or lists as unsupported,
	// by bus resource and keyed by its catalogue name.
	Mon            map[string]*float64 `json:"Mon"`            // hvac/mon
	DHW            map[string]*float64 `json:"DHW"`            // hvac/mon/dhw
	HeatPump       map[string]*float64 `json:"HeatPump"`       // hvac/mon/hp
	HeatingCircuit map[string]*float64 `json:"HeatingCircuit"` // hvac/mon/hc
	Maintenance    map[string]*float64 `json:"Maintenance"`    // hvac/maint
	Config         map[string]*float64 `json:"Config"`         // hvac/config
}

type CircuitView struct {
	ID         int        `json:"ID"`
	Name       string     `json:"Name"`
	HeatSource string     `json:"HeatSource"`
	Boiler     BoilerView `json:"Boiler"`
	// DemandPct is tag 4080 and HeatActive tag 2090, both read from
	// h/<home>/c/<id>/act: the wall thermostat aggregates the zones bound to the
	// circuit and publishes the result there. Zero until the air reports it.
	DemandPct  int  `json:"DemandPct"`
	HeatActive bool `json:"HeatActive"`
	// Source is the short address of the device that published the last report.
	Source      string `json:"Source"`
	UpdatedUnix int64  `json:"UpdatedUnix"`
}

// ZoneView is one zone of the heat-demand chain. The values are reports the
// coordinator observed, never values it computed.
type ZoneView struct {
	Number  int    `json:"Number"`
	Name    string `json:"Name"`
	Kind    string `json:"Kind"`
	Circuit int    `json:"Circuit"` // 0 when the installation binds the zone to no circuit
	// DemandPct is tag 40a0, the demand the zone's own device computes and
	// publishes on h/<home>/z/<n>/act and in the 2.05 we relay to the thermostat.
	DemandPct int `json:"DemandPct"`
	// HeatPct is tag 4160, pushed back per zone on z/cpe?lid=<n>. It is the
	// circuit aggregate, so every zone of one circuit reads the same number.
	HeatPct int `json:"HeatPct"`
	// TempC is tag 4060, the zone temperature published next to the demand.
	TempC float64 `json:"TempC"`
	// Reporter is the short address of the device whose d/config 015e record
	// elects it for this zone. Empty when no device holds the role.
	Reporter string `json:"Reporter"`
	// Source is the short address of the device that published the last report.
	Source      string `json:"Source"`
	UpdatedUnix int64  `json:"UpdatedUnix"`
	// FallbackMode is tag 6460 of h/<home>/z/<n>/fallback, the wall
	// thermostat's fallback for the zone: 0 none, 1 hold, 2 fixed, 3 frost.
	// FallbackSinceUnix is when the mode last changed, 0 when never reported.
	FallbackMode      int    `json:"FallbackMode"`
	Fallback          string `json:"Fallback"`
	FallbackSinceUnix int64  `json:"FallbackSinceUnix"`
}

// Snapshot is the whole derived state at one instant.
type Snapshot struct {
	Valves    []ValveView
	Boiler    BoilerView
	Circuits  []CircuitView
	Zones     []ZoneView
	Stats     EmuStats
	Unhandled map[string]int
	// BridgePresent means the real bridge is on air, so transmitting would put
	// two coordinators on the PAN.
	BridgePresent bool
}

// Snapshot projects the learned node state into the view model.
func (e *EmuState) Snapshot() Snapshot {
	nodes := e.sortedNodes()
	s := Snapshot{
		Valves:        make([]ValveView, 0, len(nodes)),
		Stats:         e.stats,
		Unhandled:     e.log.Counts(),
		BridgePresent: e.BridgeSeen(),
	}
	byShort := make(map[string]*NodeState, len(nodes))
	for _, ns := range nodes {
		byShort[ns.Short] = ns
		s.Valves = append(s.Valves, e.valveView(ns))
		e.fillBoiler(&s.Boiler, ns)
	}
	if e.inst != nil {
		for i := range e.inst.Circuits {
			c := &e.inst.Circuits[i]
			view := CircuitView{ID: c.ID, Name: c.Name, HeatSource: c.HeatSource}
			if dev, err := e.inst.HeatSource(c.ID); err == nil {
				view.HeatSource = dev.Serial
				if view.HeatSource == "" {
					view.HeatSource = dev.Short()
				}
				if ns := byShort[dev.Short()]; ns != nil {
					e.fillBoiler(&view.Boiler, ns)
				}
			}
			fillCircuitDemand(&view, e.circuitRep[c.ID])
			s.Circuits = append(s.Circuits, view)
		}
	}
	// A circuit the air reports but the installation does not declare is kept:
	// the coordinator stores what the devices send rather than dropping it.
	for _, id := range e.undeclaredCircuits() {
		view := CircuitView{ID: id}
		fillCircuitDemand(&view, e.circuitRep[id])
		s.Circuits = append(s.Circuits, view)
	}
	s.Zones = e.zoneViews()
	return s
}

// NodeViews projects the learned nodes alone, in short-address order. It is
// Snapshot().Valves without the cost of the rest of the snapshot.
func (e *EmuState) NodeViews() []ValveView {
	nodes := e.sortedNodes()
	out := make([]ValveView, 0, len(nodes))
	for _, ns := range nodes {
		out = append(out, e.valveView(ns))
	}
	return out
}

func (e *EmuState) sortedNodes() []*NodeState {
	nodes := make([]*NodeState, 0, len(e.nodes))
	for _, ns := range e.nodes {
		nodes = append(nodes, ns)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Short < nodes[j].Short })
	return nodes
}

// zoneViews projects the demand chain, one entry per known zone.
func (e *EmuState) zoneViews() []ZoneView {
	reporters := e.zoneReporters()
	numbers := e.demandZones()
	out := make([]ZoneView, 0, len(numbers))
	for _, n := range numbers {
		v := ZoneView{Number: n, Reporter: reporters[n], Fallback: FallbackName(FallbackNone)}
		if e.inst != nil {
			if z, err := e.inst.ZoneByNumber(n); err == nil {
				v.Name, v.Kind = z.Name, string(z.Kind)
				if z.Circuit != nil {
					v.Circuit = *z.Circuit
				}
			}
		}
		if rep := e.zoneRep[n]; rep != nil {
			v.DemandPct = rawInt(rep.raw["40a0"])
			v.HeatPct = rawInt(rep.raw["4160"])
			v.TempC = centiC(rep.raw["4060"])
			v.Source = rep.source
			if !rep.updated.IsZero() {
				v.UpdatedUnix = rep.updated.Unix()
			}
		}
		if fb := e.zoneFallback[n]; fb != nil {
			v.FallbackMode, v.Fallback = fb.mode, FallbackName(fb.mode)
			v.FallbackSinceUnix = fb.since.Unix()
		}
		out = append(out, v)
	}
	return out
}

func fillCircuitDemand(v *CircuitView, rep *demandReport) {
	if rep == nil {
		return
	}
	v.DemandPct = rawInt(rep.raw["4080"])
	v.HeatActive = be(rep.raw["2090"]) != 0
	v.Source = rep.source
	if !rep.updated.IsZero() {
		v.UpdatedUnix = rep.updated.Unix()
	}
}

func (e *EmuState) valveView(ns *NodeState) ValveView {
	v := ValveView{
		Short:          ns.Short,
		TempC:          centiC(ns.raw["012d"]),
		SetpointC:      centiC(ns.raw["6280"]),
		SchedSetpointC: centiC(ns.raw["6200"]),
		HumidityPct:    deciV(ns.raw["0135"]),
		OpenWindow:     ns.LastOpenWindow,
		BatteryMV:      rawInt(ns.raw["0162"]),
		RelayState:     rawInt(ns.raw["0164"]),
		HeatDemandPct:  rawInt(ns.raw["40a0"]),
		FaultWord:      uint32(be(ns.raw["01a3"])),
		RxCounter:      ns.RxCounter,
		CSLPhase:       ns.CSLPhase,
		CSLPeriod:      ns.CSLPeriod,
		Telemetry:      cloneTelemetry(ns.Telemetry),
		SessionIssued:  len(ns.SessionID) > 0,
		SessionAdopted: ns.SessionAdopted,
	}
	if e.inst != nil {
		if d, err := e.inst.Resolve(ns.Short); err == nil {
			v.Serial, v.Room = d.Serial, d.Label
		}
	} else if n := NodeByAny(ns.Short); n != nil {
		v.Serial, v.Room = n.Serial, n.Room
	}
	if len(ns.raw["6240"]) > 0 {
		v.Mode = modeString(ns.LastOverlay)
	}
	if level := ns.batteryLevel(); level > 0 {
		opts, kind := e.batteryModel(ns.Short)
		v.BatteryPct = opts.BatteryPct(kind, level)
	}
	v.LowBattery = ns.lowBattery
	if !ns.LastSeen.IsZero() {
		v.LastSeenUnix = ns.LastSeen.Unix()
	}
	if s, ok := e.sessions[ns.WireLE]; ok && v.SessionIssued {
		v.SessionIssuedUnix = s.at.Unix()
	}
	if ns.fallback != nil {
		v.FallbackMode = ns.fallback.mode
	}
	return v
}

// cloneTelemetry copies the Telemetry map of a node for a view, without the
// keys of the secret tags.
func cloneTelemetry(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		if !secretTelemetry[key] {
			out[key] = value
		}
	}
	return out
}

// fillBoiler folds one node's boiler and circuit tags into the shared
// heat-source view. Nodes are visited in short-address order, so the last writer
// of a tag is deterministic when two nodes report it. An attribute the boiler
// lists as unsupported reads null even when an earlier report carried it.
func (e *EmuState) fillBoiler(b *BoilerView, ns *NodeState) {
	if v, ok := e.moduleID[ns.WireLE]; ok {
		b.BusType, b.InterfaceModule = InterfaceModuleFamily(v), &v
	}
	present := false
	for tag, val := range ns.raw {
		if len(val) == 0 {
			continue
		}
		switch tag {
		case "4080":
			b.CircuitDemandPct = rawInt(val)
		case "2090":
			b.HeatActive = be(val) != 0
		case "4000":
			b.LeadZoneTargetC = centiC(val)
		case "4040":
			b.LeadZoneTempC = centiC(val)
		case "0167":
			detect := rawInt(val)
			b.BusDetect = &detect
		default:
			h, ok := hvacTagByTag[tag]
			if !ok {
				continue
			}
			var p *float64
			if x, ok := h.value(val); ok {
				p = &x
			}
			b.setAttr(h.res, h.name, p)
		}
		present = true
	}
	for _, tags := range ns.unsupported {
		for _, tag := range tags {
			name := tag
			if h, ok := hvacTagByTag[tag]; ok {
				name = h.name
				b.setAttr(h.res, h.name, nil)
			}
			if !slices.Contains(b.Unsupported, name) {
				b.Unsupported = append(b.Unsupported, name)
			}
			present = true
		}
	}
	sort.Strings(b.Unsupported)
	if ns.faultCodes != nil {
		b.FaultCodes = slices.Clone(ns.faultCodes)
		present = true
	}
	if present {
		b.Present = true
		b.SourceShort = ns.Short
	}
	b.deriveLegacy()
}

func (b *BoilerView) setAttr(res, name string, v *float64) {
	var group *map[string]*float64
	switch res {
	case resHVACMon:
		group = &b.Mon
	case resHVACDHW:
		group = &b.DHW
	case resHVACHP:
		group = &b.HeatPump
	case resHVACHC:
		group = &b.HeatingCircuit
	case resHVACMaint:
		group = &b.Maintenance
	case resHVACConfig:
		group = &b.Config
	default:
		return
	}
	if *group == nil {
		*group = map[string]*float64{}
	}
	(*group)[name] = v
}

// deriveLegacy fills the flat fields from the catalogue groups.
func (b *BoilerView) deriveLegacy() {
	b.FlowC = b.Mon["boiler_flow_C"]
	b.ReturnC = b.Mon["return_water_C"]
	b.OutsideC = b.Mon["outside_C"]
	b.ControlSetpointC = b.Mon["control_setpoint_C"]
	b.ModulationPct = intAttr(b.Mon["boiler_mod"])
	b.Active = b.Mon["boiler_flag"] != nil && *b.Mon["boiler_flag"] != 0
	b.DHWC = b.DHW["dhw_temp_C"]
	b.DHWSetpointC = b.DHW["dhw_setpoint_C"]
	b.DhwFlowLmin = b.DHW["dhw_flow_lmin"]
	b.ChPressureBar = b.Maintenance["ch_pressure_bar"]
	b.BurnerStarts = u32Attr(b.Maintenance["burner_starts"])
	b.BurnerHoursMin = u32Attr(b.Maintenance["burner_hours_min"])
	b.MaxChSetpointC = b.Config["max_ch_setpoint_C"]
	b.FaultCode = 0
	if len(b.FaultCodes) > 0 {
		b.FaultCode = int(b.FaultCodes[0])
	}
}

func intAttr(v *float64) *int {
	if v == nil {
		return nil
	}
	n := int(*v)
	return &n
}

func u32Attr(v *float64) *uint32 {
	if v == nil {
		return nil
	}
	n := uint32(*v)
	return &n
}

// hvacResources are the boiler bus resources of the wall thermostat.
var hvacResources = map[string]bool{
	resHVACMon: true, resHVACDHW: true, resHVACHP: true, resHVACHC: true,
	resHVACMaint: true, resHVACConfig: true, resHVACCodes: true,
}

// noteHVAC keeps what the tag-keyed cache cannot hold of a boiler bus report:
// the 0481 list of each resource, which every resource carries under the same
// tag, and the repeated 0460 fault codes. Each hvac/codes report replaces the
// fault list, so a report without 0460 clears it. A body with no request URI
// takes the resource of the attributes its 0481 names. at stamps an hvac/mon
// report, even one with no attribute the boiler view reads: it shows the
// thermostat alive.
func (ns *NodeState) noteHVAC(tlvs []TLV, uri string, at time.Time) {
	path, _, _ := strings.Cut(uri, "?")
	res := normalizeResource(path)
	if res == resHVACMon {
		ns.hvacMonAt = at
	}
	var unsupported []string
	manifest, codes := false, res == resHVACCodes
	for _, t := range tlvs {
		switch t.Tag {
		case "0481":
			manifest = true
			unsupported = append(unsupported, unsupportedTags(t.Val)...)
		case "0460":
			codes = true
		}
	}
	if manifest && !hvacResources[res] {
		res = ""
		for _, tag := range unsupported {
			if h, ok := hvacTagByTag[tag]; ok {
				res = h.res
				break
			}
		}
	}
	if manifest && res != "" {
		if ns.unsupported == nil {
			ns.unsupported = map[string][]string{}
		}
		ns.unsupported[res] = unsupported
	}
	if !codes {
		return
	}
	faults := []uint32{}
	for _, t := range tlvs {
		if t.Tag != "0460" || len(t.Val) == 0 || len(t.Val) > 4 {
			continue
		}
		if c := uint32(be(t.Val)); c != 0xffffffff && c != 0x7fffffff {
			faults = append(faults, c)
		}
	}
	ns.faultCodes = faults
}

func modeString(overlay byte) string {
	switch overlay {
	case 0:
		return "auto"
	case overlayAutoChange:
		return OverlayEndUntilAutoChange
	case 2:
		return "manual"
	case 3:
		return "until_next"
	}
	return "unknown"
}

func centiC(v []byte) float64 {
	if len(v) == 0 {
		return 0
	}
	return float64(be(v)) / 100.0
}

func deciV(v []byte) float64 {
	if len(v) == 0 {
		return 0
	}
	return float64(be(v)) / 10.0
}

func rawInt(v []byte) int {
	if len(v) == 0 {
		return 0
	}
	return int(be(v))
}
