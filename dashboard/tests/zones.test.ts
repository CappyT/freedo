import { describe, expect, it } from 'vitest';
import type { DeclaredZone, Health, HealthDevice, NodeView, ZoneDemand } from '../src/lib/types.ts';
import { buildZones, targetIsTemp, zoneName, zoneStatus, type Member } from '../src/lib/zones.ts';
import { demand, device, target, zone } from './fixtures.ts';

const member = (h: HealthDevice | undefined): Member => ({ short: '0201', label: 'bath', kind: 'valve', node: undefined, health: h });

const status = (z: DeclaredZone, d: ZoneDemand | undefined, members: Member[], away = false) =>
	zoneStatus(z, d, members, away, []);

describe('zone name', () => {
	it('shows the fixed text for the hot water zone, whatever the installation stores', () => {
		expect(zoneName(zone({ kind: 'hotwater', name: 'acqua calda sanitaria' }), 'Hot water')).toBe(
			'Hot water'
		);
	});

	it('keeps the name of a heating zone', () => {
		expect(zoneName(zone({ name: 'Bath' }), 'Hot water')).toBe('Bath');
	});
});

describe('zone state', () => {
	it('is idle with no demand and heating with demand', () => {
		expect(status(zone(), demand(), [member(device())]).state).toBe('idle');
		expect(status(zone(), demand({ DemandPct: 40 }), [member(device())]).state).toBe('heating');
	});

	it('pairs every state with a tone of the brand tokens', () => {
		const m = [member(device())];
		expect(status(zone(), demand({ DemandPct: 40 }), m).tone).toBe('heating');
		expect(status(zone(), demand(), m, true).state).toBe('away');
		expect(status(zone(), demand(), m, true).tone).toBe('away');
		expect(status(zone({ target: target({ on: false }) }), demand(), m).tone).toBe('off');
	});

	it('shows an open window before the demand', () => {
		const open = zone({ window: { open: true, source: 'device', since: null, deadline: null, undelivered: [] } });
		const result = status(open, demand({ DemandPct: 80 }), [member(device())]);
		expect(result).toMatchObject({ state: 'windowOpen', tone: 'warning' });
		expect(status(zone({ target: target({ source: 'window', targetC: null }) }), demand(), [member(device())]).state).toBe('windowOpen');
	});

	it('shows off before heating and away', () => {
		expect(status(zone({ target: target({ on: false }) }), demand({ DemandPct: 10 }), [member(device())], true).state).toBe('off');
	});

	it('is offline only when every device is offline', () => {
		const all = status(zone(), demand(), [member(device({ offline: true }))]);
		expect(all).toMatchObject({ state: 'offline', tone: 'offline' });
		const some = zoneStatus(zone({ members: ['0201', '0202'] }), demand(), [member(device({ offline: true })), member(device())], false, []);
		expect(some.state).toBe('idle');
		expect(some.alerts).toContain('deviceOffline');
	});

	it('is a fault when a device reports one, and raises the other problems as alerts', () => {
		const result = status(zone(), demand(), [member(device({ fault: true, lowBattery: true, commsWarnings: ['noNeighbour'] }))]);
		expect(result.state).toBe('fault');
		expect(result.alerts).toEqual(['deviceFault', 'lowBattery', 'commsWarning']);
	});

	it('raises a fallback of the thermostat and an undelivered window as alerts', () => {
		const z = zone({ window: { open: false, source: null, since: null, deadline: null, undelivered: ['0201'] } });
		const result = zoneStatus(z, demand(), [member(device())], false, [{ zone: 2, mode: 1, name: 'hold', source: 'x', since: '' }]);
		expect(result.alerts).toEqual(['fallback', 'windowUndelivered']);
	});

	it('reads hot water as on or off, never as heating or away', () => {
		const hot = zone({ number: 0, kind: 'hotwater' });
		expect(status(hot, demand({ DemandPct: 90 }), [member(device())], true).state).toBe('on');
		expect(status({ ...hot, target: target({ on: false }) }, undefined, [member(device())]).state).toBe('off');
	});
});

describe('zone view model', () => {
	const node = (over: Partial<NodeView>): NodeView => ({
		Short: '0201',
		Serial: 'VA9000000001',
		Room: 'bath',
		TempC: 0,
		SetpointC: 0,
		HumidityPct: 0,
		Telemetry: null,
		BatteryMV: 2500,
		BatteryPct: 61,
		LastSeenUnix: 1_700_000_000,
		LowBattery: false,
		Fault: false,
		CommsWarnings: null,
		Offline: false,
		ChildLock: false,
		...over
	});
	const health = (devices: HealthDevice[]): Health => ({ devices, zoneFallback: [] }) as unknown as Health;

	it('takes the temperature of the zone, else of the measuring device', () => {
		const withDemand = buildZones([zone()], [demand()], [node({ TempC: 19 })], health([device()]), false);
		expect(withDemand[0]?.tempC).toBe(20.5);
		const withoutReport = buildZones([zone()], [demand({ UpdatedUnix: 0, TempC: 0 })], [node({ TempC: 19 })], health([device()]), false);
		expect(withoutReport[0]?.tempC).toBe(19);
		const nothing = buildZones([zone()], [demand({ UpdatedUnix: 0, TempC: 0 })], [node({})], health([device()]), false);
		expect(nothing[0]?.tempC).toBeNull();
	});

	it('shows no temperature or humidity for hot water', () => {
		const [hot] = buildZones([zone({ kind: 'hotwater' })], [demand({ TempC: 50 })], [node({ HumidityPct: 40 })], null, false);
		expect(hot).toMatchObject({ tempC: null, humidityPct: null });
	});

	it('reads the humidity of the measuring device', () => {
		const [model] = buildZones([zone()], [demand()], [node({ HumidityPct: 43 })], health([device()]), false);
		expect(model?.humidityPct).toBe(43);
	});
});


describe('target as a number', () => {
	it('is a number while the zone is on and follows a temperature', () => {
		expect(targetIsTemp(target())).toBe(true);
	});

	it('is a reason when the zone is off, a window is open or no value is known', () => {
		expect(targetIsTemp(target({ on: false }))).toBe(false);
		expect(targetIsTemp(target({ source: 'window' }))).toBe(false);
		expect(targetIsTemp(target({ targetC: null }))).toBe(false);
	});
});
