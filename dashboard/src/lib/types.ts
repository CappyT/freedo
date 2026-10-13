// Shapes of the daemon answers. docs/API.md names the fields and the Go views define them.

export type EndRule = 'manual' | 'timer' | 'until_next' | 'until_auto_change';
export type ZoneKind = 'heating' | 'hotwater';
export type DeviceKind = 'valve' | 'thermostat' | 'bridge';

export interface TimeZoneInfo {
	name: string;
	offsetMin: number;
	fallback: boolean;
}

export interface HealthDevice {
	device: string;
	short: string;
	kind: DeviceKind;
	label: string;
	batteryMV: number;
	lowBattery: boolean;
	fault: boolean;
	faultWord: number;
	/** The names of the faults in the fault word. `fault` is true when there is one. */
	faults: string[] | null;
	/** The names of the valve states that are a warning and no fault. */
	deviceWarnings: string[] | null;
	commsWarnings: string[] | null;
	offline: boolean;
	lastSeen: string | null;
	fallbackMode: number;
}

export interface ZoneFallback {
	zone: number;
	mode: number;
	name: string;
	source: string;
	since: string;
}

export interface Health {
	ok: boolean;
	engine?: string;
	mode: 'live' | 'replay' | 'mock';
	tx: boolean;
	txArmed: boolean;
	transmitAllowed: boolean;
	bridgePresent: boolean;
	startedAt: string;
	uptimeSec: number;
	away: boolean;
	unhandled: number;
	rxQueue: number;
	radioError?: string;
	fsctrl0?: string;
	/** Serial replies of the bridge lost since the start. Live mode only. */
	serialErrors?: number;
	calibrated?: boolean;
	warning?: string;
	frames: number;
	micFails: number;
	routerAdvertsSent: number;
	downlink: { sent: number; delivered: number; dropped: number };
	queue: { pending: number; awaitingCSL: number; flowPending: number; zoneProxies: number };
	devices: HealthDevice[];
	alerts: { device: string; short: string; label: string; alerts: string[] }[];
	zoneFallback: ZoneFallback[];
	timezone?: TimeZoneInfo;
	interfaceModuleUnknown?: string[];
}

export interface WindowView {
	open: boolean;
	source: string | null;
	since: string | null;
	deadline: string | null;
	undelivered: string[];
}

export interface Overlay {
	kind: string;
	rule: EndRule;
	on: boolean;
	setpointC: number | null;
	endsAt: string | null;
}

export interface Target {
	targetC: number | null;
	source: 'program' | 'overlay' | 'boost' | 'window' | 'away' | null;
	overlay: Overlay | null;
	programC: number | null;
	programOff: boolean;
	boost: boolean;
	known: boolean;
	on: boolean;
	/** The daemon holds a weekly program for the zone. A daemon without the field leaves it undefined. */
	hasProgram?: boolean;
}

export interface DeclaredZone {
	number: number;
	name: string;
	kind: ZoneKind;
	members: string[];
	circuit?: number;
	reporter?: string;
	measuringDevice?: string;
	writer?: string;
	window?: WindowView;
	target: Target;
}

export interface ZoneDemand {
	Number: number;
	Name: string;
	Kind: ZoneKind;
	Circuit: number;
	DemandPct: number;
	HeatPct: number;
	TempC: number;
	UpdatedUnix: number;
	FallbackMode: number;
	Fallback: string;
}

export interface Zones {
	declared: DeclaredZone[];
	demand: ZoneDemand[];
}

export interface DisplaySettings {
	orientation: string;
	notification: boolean;
}

export interface NodeView {
	Short: string;
	Serial: string;
	Room: string;
	TempC: number;
	SetpointC: number;
	HumidityPct: number;
	Telemetry: Record<string, string> | null;
	BatteryMV: number;
	BatteryPct: number;
	/** When the daemon last read a frame or an acknowledgement of the device, in Unix seconds. 0 before the first one. */
	LastSeenUnix: number;
	/** The frame counter of the last frame of the device that the daemon read under the network key. */
	RxCounter: number;
	/** A request of the device carried the session that the daemon served it. */
	SessionAdopted: boolean;
	LowBattery: boolean;
	Fault: boolean;
	Faults: string[] | null;
	DeviceWarnings: string[] | null;
	CommsWarnings: string[] | null;
	Offline: boolean;
	ChildLock: boolean | null;
}

export interface Home {
	away: boolean;
	scheduled: Record<string, number>;
	scheduledOff: Record<string, boolean>;
	/** The active program preset. A daemon before schema 8 leaves it absent. */
	preset?: { id: number; name: string };
}

/** One program preset: the home and away programs of every zone. */
export interface Preset {
	id: number;
	name: string;
	active: boolean;
}

export interface FlowCircuit {
	id: number;
	curve: boolean;
	defaultCurve: boolean;
	adapt: boolean;
	computedC: number | null;
	servedC: number | null;
	minC: number;
	maxC: number;
	changedAt: string | null;
	fetched: boolean;
	adaptedC: number | null;
	adaptReason: string | null;
	reductionC: number | null;
}

export interface GasEstimate {
	running: boolean;
	reason: string | null;
	flowM3H: number;
	totalM3: number;
	unit: string;
}

export interface EnergyEstimate {
	running: boolean;
	reason: string | null;
	powerKW: number;
	totalKWh: number;
	maxPowerKW: number | null;
	reportedAt: string | null;
	gas: GasEstimate;
}

export interface Boiler {
	Present: boolean;
	BusType: string;
	FlowC: number | null;
	DHWC: number | null;
	/** The hot water setpoint that the thermostat reports. The daemon does not set it. */
	DHWSetpointC?: number | null;
	ModulationPct: number | null;
	/** The burner flag of the boiler. It is false also while the boiler reports no flag: see Mon. */
	Active: boolean;
	/** The heating circuit calls for heat (tag 2090). It does not say that the burner runs. */
	HeatActive: boolean;
	/** The monitor values of the boiler by name. boiler_flag is absent while the boiler reports no burner state. */
	Mon?: Record<string, number | null> | null;
	CircuitDemandPct: number;
	ReturnC: number | null;
	OutsideC: number | null;
	ControlSetpointC: number | null;
	MaxChSetpointC: number | null;
	ChPressureBar: number | null;
	BurnerStarts: number | null;
	BurnerHoursMin: number | null;
	FaultCodes: number[] | null;
	FaultCode: number;
	flowLimit: {
		defaultC: number | null;
		outdoor: { tempC: number; ageSec: number; stale: boolean } | null;
		circuits: FlowCircuit[];
	};
	energy: EnergyEstimate;
}

export interface Circuit {
	ID: number;
	Name: string;
	HeatSource: string;
}

export interface ScheduleEntry {
	minute: number;
	setpoint_c: number;
	enabled: boolean;
}

export interface ScheduleProgram {
	days: string[];
	mask: number;
	entries: ScheduleEntry[];
}

/** One zone of GET /api/presets/{id}/schedules. A mode without a program is null. */
export interface PresetZoneSchedules {
	zone: number;
	name: string;
	kind: ZoneKind;
	home: ScheduleProgram[] | null;
	away: ScheduleProgram[] | null;
	/** The block the preset holds now in the current home state, null when none is in force. */
	inForce: { mode: 'home' | 'away'; setpointC: number; off: boolean } | null;
}

export interface PresetSchedules {
	preset: Preset;
	zones: PresetZoneSchedules[];
}

export interface ScheduleView {
	zone: number;
	members: string[];
	programs: ScheduleProgram[];
	/** The mode and the preset the answer belongs to. A daemon before schema 8 leaves them absent. */
	mode?: 'home' | 'away';
	preset?: number;
}

export interface Queued {
	queued: boolean;
	pending: number;
}

/** GET /api/install: the fields the dashboard reads. */
export interface InstallInfo {
	rev: number;
	/** The IANA zone of the installation, empty while the daemon follows its host. */
	timezone: string;
	utc_offset_min: number | null;
	warnings: string[];
	counts: { devices: number; zones: number; circuits: number };
}

/** A device row of the installation. eui is the wire EUI in hexadecimal. */
export interface InstallDevice {
	eui: string;
	short: string;
	kind: DeviceKind;
	serial: string;
	label: string;
	/** The one zone number the device answers to, null when the installation states none. */
	zone: number | null;
}

/** A zone row of the installation. The references are wire EUIs. */
export interface InstallZone {
	number: number;
	name: string;
	kind: ZoneKind;
	members: string[];
	circuit: number | null;
	reporter: string | null;
	measuring_device: string | null;
	open_window_detection: boolean;
	open_window_duration_sec: number;
	early_start: boolean;
	/** The fields below are null on the hot water zone, which has none of these options. */
	device_overlay_end: 'manual' | 'until_next' | 'timer' | null;
	device_overlay_end_sec: number | null;
	offset_mode: 'config' | 'dbg2' | 'setpoint' | null;
	/** The correction the daemon holds in mode setpoint. Null in the other modes: the valve holds it there. */
	setpoint_offset_c: number | null;
	auto_offset: boolean | null;
	auto_offset_interval_sec: number | null;
	away_temperature_c: number | null;
}

/** The option fields of PATCH /api/install/zones/{n} that the dashboard writes. */
export interface ZoneOptionsPatch {
	open_window_detection?: boolean;
	open_window_duration_sec?: number;
	early_start?: boolean;
	device_overlay_end?: 'manual' | 'until_next' | 'timer';
	device_overlay_end_sec?: number;
	offset_mode?: 'config' | 'dbg2' | 'setpoint';
	auto_offset?: boolean;
	auto_offset_interval_sec?: number;
	away_temperature_c?: number;
}

export interface InstallCircuit {
	id: number;
	name: string;
	heat_source: string;
}

export type RemovalEffectName = 'member' | 'hot_water' | 'last_member' | 'reporter' | 'measuring_device' | 'heat_source';

export interface RemovalEffect {
	effect: RemovalEffectName;
	zone?: number;
	circuit?: number;
}

/** GET /api/install/devices/{ref}/removal. forceable is false while the device is a heat source. */
export interface Removal {
	device: InstallDevice;
	effects: RemovalEffect[];
	forceable: boolean;
}

/** A device that the open pair window heard under the pairing key. */
export interface PairJoiner {
	eui: string;
	short: string;
	/** The label of the device when the installation declares it, else empty. */
	label: string;
	/** When the daemon last heard the device ask to join, as an RFC 3339 time. */
	lastSeen: string;
	frames: number;
}

export interface PairWindow {
	open: boolean;
	until?: string;
	/** The bridge answers the beacon requests of the window by itself (firmware v2.9 or later). */
	responder: boolean;
	joiners: PairJoiner[];
}
