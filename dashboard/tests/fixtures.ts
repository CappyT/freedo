import type { DeclaredZone, HealthDevice, ZoneDemand } from '../src/lib/types.ts';

export const target = (over: Partial<DeclaredZone['target']> = {}): DeclaredZone['target'] => ({
	targetC: 20,
	source: 'program',
	overlay: null,
	programC: 20,
	programOff: false,
	boost: false,
	known: true,
	on: true,
	...over
});

export const zone = (over: Partial<DeclaredZone> = {}): DeclaredZone => ({
	number: 2,
	name: 'Bath',
	kind: 'heating',
	members: ['0201'],
	measuringDevice: '0201',
	target: target(),
	window: { open: false, source: null, since: null, deadline: null, undelivered: [] },
	...over
});

export const demand = (over: Partial<ZoneDemand> = {}): ZoneDemand => ({
	Number: 2,
	Name: 'Bath',
	Kind: 'heating',
	Circuit: 1,
	DemandPct: 0,
	HeatPct: 0,
	TempC: 20.5,
	UpdatedUnix: 1_700_000_000,
	FallbackMode: 0,
	Fallback: 'none',
	...over
});

export const device = (over: Partial<HealthDevice> = {}): HealthDevice => ({
	device: 'VA9000000001',
	short: '0201',
	kind: 'valve',
	label: 'bath',
	batteryMV: 2500,
	lowBattery: false,
	fault: false,
	faultWord: 0,
	commsWarnings: null,
	offline: false,
	lastSeen: null,
	fallbackMode: 0,
	...over
});
