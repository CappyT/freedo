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
	LastSeenUnix: number;
	LowBattery: boolean;
	Fault: boolean;
	CommsWarnings: string[] | null;
	Offline: boolean;
	ChildLock: boolean | null;
}

export interface Home {
	away: boolean;
	scheduled: Record<string, number>;
	scheduledOff: Record<string, boolean>;
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
	ModulationPct: number | null;
	Active: boolean;
	HeatActive: boolean;
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

export interface ScheduleView {
	zone: number;
	members: string[];
	programs: ScheduleProgram[];
}

export interface Queued {
	queued: boolean;
	pending: number;
}
