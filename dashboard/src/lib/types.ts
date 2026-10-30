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
	/** The version of the daemon, "dev" on a build with no release stamp. */
	version?: string;
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
	/** Present while a daemon that transmits waits after an import. Such an answer carries no state. */
	importWait?: { leftSec: number };
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

/** A push of a room temperature that a zone refused because a device owns the zone. */
export interface RefusedReading {
	source: string | null;
	at: string;
	ageSec: number;
}

/** The room temperature that an outside sensor gives a heating zone. docs/API.md, "External room temperature". */
export interface ExternalTemp {
	tempC: number | null;
	at: string | null;
	ageSec: number | null;
	source: string | null;
	/** The reading is older than the limit and no longer drives the zone. */
	stale: boolean;
	inUse: boolean;
	deviceC: number | null;
	/** The last push the zone refused, null when there is none or it is old. Absent on a daemon without the field. */
	refused?: RefusedReading | null;
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
	/** Heating zones only. */
	externalTemp?: ExternalTemp;
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

export interface GasMeterEntry {
	/** RFC3339, the time the user took the reading. */
	at: string;
	reading: number;
	/** The fuel counter when the reading was taken. */
	estimate: number;
}

/** The real meter the user reads, with the value the daemon extrapolates between readings. */
export interface GasMeter {
	value: number;
	lastReading: number;
	lastReadingAt: string;
	readings: number;
	factor: number;
	/** 2 when a factor was learned, else 0. */
	factorReadings: number;
	/** Oldest first. */
	entries: GasMeterEntry[];
}

export interface GasEstimate {
	running: boolean;
	reason: string | null;
	flowM3H: number;
	totalM3: number;
	unit: string;
	/** Null while no reading is stored. */
	meter: GasMeter | null;
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
export type FuelUnit = 'm3' | 'l' | 'kg';

/** A weather curve of one circuit: two points from outdoor temperature to flow limit, clamped to min_c and max_c. */
export interface FlowCurve {
	enabled: boolean;
	points: [{ outdoor_c: number; flow_c: number }, { outdoor_c: number; flow_c: number }];
	min_c: number;
	max_c: number;
}

/** The options of the boiler and of its flow limit. PUT /api/install/boiler writes them; null removes a value. */
export interface BoilerOptions {
	boiler_max_power_kw: number | null;
	boiler_min_power_kw: number;
	boiler_max_gas_m3h: number | null;
	boiler_min_gas_m3h: number | null;
	fuel_unit: FuelUnit;
	flow_limit_c: number | null;
	flow_default_curve: boolean;
	flow_auto_adapt: boolean;
	/** Keyed by circuit id. */
	flow_curves: Record<string, FlowCurve>;
	outdoor_max_age_sec: number;
	flow_hysteresis_c: number;
	flow_min_interval_sec: number;
}

/** What PUT /api/install/boiler accepts. A curve set to null removes it. */
export type BoilerPatch = Partial<Omit<BoilerOptions, 'boiler_min_power_kw' | 'flow_curves'>> & {
	boiler_min_power_kw?: number | null;
	flow_curves?: Record<string, FlowCurve | null>;
};

export interface InstallInfo extends BoilerOptions {
	rev: number;
	/** Age at which a room temperature from Home Assistant expires. */
	room_temp_max_age_sec?: number;
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
	/** How a manual offset reaches the valve. */
	offset_mode: 'config' | 'dbg2' | null;
	/** Null on the hot water zone. */
	owner: ZoneOwner | null;
	away_temperature_c: number | null;
}

/** Who owns a heating zone: the elected demand reporter, or freedo on the room temperature of Home Assistant. */
export type ZoneOwner = 'device' | 'coordinator';

/** The option fields of PATCH /api/install/zones/{n} that the dashboard writes. */
export interface ZoneOptionsPatch {
	open_window_detection?: boolean;
	open_window_duration_sec?: number;
	early_start?: boolean;
	device_overlay_end?: 'manual' | 'until_next' | 'timer';
	device_overlay_end_sec?: number;
	offset_mode?: 'config' | 'dbg2';
	owner?: ZoneOwner;
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

/** GET /api/history: the switch of the statistics, what they hold and the fuel price. */
export interface HistoryInfo {
	/** False without a state database: the statistics need one. */
	available: boolean;
	enabled: boolean;
	/** Why an enabled history has no file, else null. */
	error: string | null;
	/** The time of the oldest sample, null before the first. */
	since: string | null;
	sizeBytes: number;
	intervalSec: number;
	sampleDays: number;
	dayMonths: number;
	/** The price of one fuel unit, null while unset. */
	fuelPrice: number | null;
	/** An ISO 4217 code. */
	currency: string;
}

export interface HistoryPatch {
	enabled?: boolean;
	/** Removes the stored statistics. It needs enabled false. */
	delete?: boolean;
	fuelPrice?: number | null;
	currency?: string;
}

/** One zone at one sample time. A null field had no value then. */
export interface HistorySample {
	atMs: number;
	tempC: number | null;
	targetC: number | null;
	on: boolean;
	humidityPct: number | null;
	demandPct: number | null;
	window: boolean;
	away: boolean;
}

/** A finished heat-up: a rise of the program target, from the start to the settling. */
export interface HeatUp {
	startedMs: number;
	endedMs: number;
	startC: number;
	targetC: number | null;
	/** When the room reached the target, null when it never did. */
	crossedMs: number | null;
	deadTimeSec: number | null;
	overshootC: number | null;
	settlingSec: number | null;
	endReason: string;
}

/** GET /api/history/zones/{n}: one local day of one zone. */
export interface ZoneHistory {
	zone: number;
	date: string;
	from: string;
	to: string;
	intervalSec: number;
	/** The local date of the oldest sample of the zone, null before the first. */
	first: string | null;
	samples: HistorySample[];
	episodes: HeatUp[];
}

/** What one local day counted. fuel is the estimate times the meter factor of that day. */
export interface DayTotal {
	day: string;
	fuel: number;
	fuelEstimate: number;
	heatKWh: number;
	burnerMin: number;
}

/** GET /api/history/energy. */
export interface EnergyHistory {
	from: string;
	to: string;
	unit: string;
	fuelPrice: number | null;
	currency: string;
	days: DayTotal[];
}
