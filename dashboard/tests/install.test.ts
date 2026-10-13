import { describe, expect, it } from 'vitest';
import { createInstallClient } from '../src/lib/api.ts';
import {
	countdown,
	describeEffect,
	joinedSince,
	movePlan,
	nextCircuitId,
	nextZoneNumber,
	setupState,
	shortOf,
	wireEui
} from '../src/lib/install.ts';
import type { Health } from '../src/lib/types.ts';
import { device } from './fixtures.ts';

describe('the number of a new zone', () => {
	it('is the lowest free number from 1 for a heated room', () => {
		expect(nextZoneNumber([], 'heating')).toBe(1);
		expect(nextZoneNumber([0, 1, 2, 5], 'heating')).toBe(3);
	});

	it('is 0 for hot water, and none when hot water exists', () => {
		expect(nextZoneNumber([1, 2], 'hotwater')).toBe(0);
		expect(nextZoneNumber([0, 1], 'hotwater')).toBeNull();
	});

	it('is none when every number to 31 is taken', () => {
		const all = Array.from({ length: 32 }, (_, i) => i);
		expect(nextZoneNumber(all, 'heating')).toBeNull();
		expect(nextCircuitId(all)).toBeNull();
		expect(nextCircuitId([1, 3])).toBe(2);
	});
});

describe('a wire EUI', () => {
	it('is 16 hexadecimal digits, in lower case', () => {
		expect(wireEui(' 04A3DEFA07C51B00 ')).toBe('04a3defa07c51b00');
		expect(wireEui('04a3defa07c51b')).toBeNull();
		expect(wireEui('04:a3:de:fa:07:c5:1b:00')).toBeNull();
		expect(wireEui('')).toBeNull();
	});
});

describe('the writes of a move', () => {
	it('leave the old room, then join the new one', () => {
		expect(movePlan(null, 2, 3)).toEqual([
			{ op: 'leave', zone: 2 },
			{ op: 'join', zone: 3 }
		]);
	});

	it('take the zone claim off before the leave and put it on the new room', () => {
		expect(movePlan(2, 2, 3)).toEqual([
			{ op: 'claim', zone: null },
			{ op: 'leave', zone: 2 },
			{ op: 'join', zone: 3 },
			{ op: 'claim', zone: 3 }
		]);
	});

	it('keep a claim on another zone', () => {
		expect(movePlan(0, 2, 3)).toEqual([
			{ op: 'leave', zone: 2 },
			{ op: 'join', zone: 3 }
		]);
	});

	it('join only from no room, leave only to no room, and nothing for the same room', () => {
		expect(movePlan(null, null, 3)).toEqual([{ op: 'join', zone: 3 }]);
		expect(movePlan(2, 2, null)).toEqual([
			{ op: 'claim', zone: null },
			{ op: 'leave', zone: 2 }
		]);
		expect(movePlan(2, 2, 2)).toEqual([]);
	});
});

describe('a removal effect', () => {
	const zoneName = (n: number) => `zone ${n}`;
	const circuitName = (id: number) => `circuit ${id}`;

	it('names the zone or the circuit it falls on', () => {
		expect(describeEffect({ effect: 'last_member', zone: 2 }, zoneName, circuitName)).toEqual({
			key: 'remove.effect.last_member',
			name: 'zone 2'
		});
		expect(describeEffect({ effect: 'member', zone: 2 }, zoneName, circuitName).key).toBe('remove.effect.member');
		expect(describeEffect({ effect: 'heat_source', circuit: 1 }, zoneName, circuitName)).toEqual({
			key: 'remove.effect.heat_source',
			name: 'circuit 1'
		});
	});
});

describe('a device after the pair command', () => {
	const before = { serial: '', sessionUnix: 100 };

	it('has not joined while the daemon reports nothing new', () => {
		expect(joinedSince(undefined, before)).toBe(false);
		expect(joinedSince({ Serial: '', SessionIssuedUnix: 100 }, before)).toBe(false);
		expect(joinedSince({ Serial: '' }, { serial: '', sessionUnix: 0 })).toBe(false);
	});

	it('has joined when the daemon served it a session after the command', () => {
		expect(joinedSince({ Serial: '', SessionIssuedUnix: 101 }, before)).toBe(true);
	});

	it('has joined when the daemon learned its serial number', () => {
		expect(joinedSince({ Serial: 'VA9000000001', SessionIssuedUnix: 100 }, before)).toBe(true);
	});

	it('has not joined on a serial number that the daemon knew before', () => {
		expect(joinedSince({ Serial: 'VA9000000001', SessionIssuedUnix: 100 }, { serial: 'VA9000000001', sessionUnix: 100 })).toBe(false);
	});
});

describe('the time a pair window stays open', () => {
	it('reads as minutes and seconds, never below zero', () => {
		expect(countdown(600)).toBe('10:00');
		expect(countdown(61.4)).toBe('1:01');
		expect(countdown(-3)).toBe('0:00');
	});
});

describe('the steps of the first setup', () => {
	const health = (over: Partial<Health> = {}) =>
		({
			mode: 'live',
			calibrated: true,
			bridgePresent: false,
			transmitAllowed: true,
			timezone: { name: 'Europe/Rome', offsetMin: 60, fallback: false },
			devices: [device()],
			...over
		}) as Health;

	it('are all open before the daemon answers', () => {
		expect(setupState(null, [])).toMatchObject({
			radio: 'todo',
			radioProblem: 'noRadio',
			timezone: 'todo',
			rooms: 'todo',
			transmit: 'todo',
			transmitProblem: 'dryRun',
			devices: 'todo',
			circuits: 'todo'
		});
	});

	it('are all done on a plant that runs', () => {
		const state = setupState(health(), [{ kind: 'heating', circuit: 1 }, { kind: 'hotwater' }]);
		expect(Object.values(state).filter((v) => v === 'todo')).toEqual([]);
	});

	it('name what keeps the radio and the transmit steps open', () => {
		expect(setupState(health({ mode: 'mock' }), []).radioProblem).toBe('noRadio');
		expect(setupState(health({ calibrated: false }), []).radioProblem).toBe('notCalibrated');
		expect(setupState(health({ bridgePresent: true }), []).transmitProblem).toBe('bridge');
		expect(setupState(health({ transmitAllowed: false }), []).transmitProblem).toBe('dryRun');
	});

	it('keep the time zone open while the daemon follows its host', () => {
		expect(setupState(health({ timezone: { name: 'UTC', offsetMin: 0, fallback: true } }), []).timezone).toBe('todo');
	});

	it('keep the circuits open while a heated room has none', () => {
		expect(setupState(health(), [{ kind: 'heating', circuit: 1 }, { kind: 'heating' }]).circuits).toBe('todo');
		expect(setupState(health(), [{ kind: 'hotwater' }])).toMatchObject({ rooms: 'todo', circuits: 'todo' });
	});
});

describe('the device a view names', () => {
	it('is found by serial number or by short address', () => {
		const devices = [device({ short: 'a205', device: 'RU9000000001' })];
		expect(shortOf(devices, 'RU9000000001')).toBe('a205');
		expect(shortOf(devices, 'a205')).toBe('a205');
		expect(shortOf(devices, 'VA9000000009')).toBe('');
	});
});

describe('the installation client', () => {
	async function record(call: (client: ReturnType<typeof createInstallClient>) => Promise<unknown>) {
		const seen: { method: string; path: string; body: unknown }[] = [];
		const fetchStub = (async (url: string | URL | Request, init?: RequestInit) => {
			const { pathname, search } = new URL(String(url));
			seen.push({ method: init?.method ?? 'GET', path: pathname + search, body: init?.body ? JSON.parse(String(init.body)) : undefined });
			return new Response('{"devices":[],"zones":[],"circuits":[]}', { status: 200 });
		}) as unknown as typeof fetch;
		await call(createInstallClient({ base: 'http://daemon/', fetch: fetchStub }));
		return seen;
	}

	it('forces a delete only when the caller asks', async () => {
		expect(await record((c) => c.deleteDevice('a304', false))).toEqual([{ method: 'DELETE', path: '/api/install/devices/a304', body: undefined }]);
		expect((await record((c) => c.deleteDevice('a304', true)))[0]?.path).toBe('/api/install/devices/a304?force=true');
	});

	it('sets a role with PUT and clears it with DELETE', async () => {
		expect(await record((c) => c.setRole(2, 'measuring', 'a304'))).toEqual([
			{ method: 'PUT', path: '/api/install/zones/2/measuring', body: { ref: 'a304' } }
		]);
		expect(await record((c) => c.setRole(2, 'reporter', null))).toEqual([
			{ method: 'DELETE', path: '/api/install/zones/2/reporter', body: undefined }
		]);
	});

	it('pairs one named device and opens the window for a number of seconds', async () => {
		expect(await record((c) => c.pair('04a3defa07c51b00'))).toEqual([
			{ method: 'POST', path: '/api/pair', body: { target: '04a3defa07c51b00' } }
		]);
		expect(await record((c) => c.openPairWindow(600))).toEqual([
			{ method: 'POST', path: '/api/pair/window', body: { seconds: 600 } }
		]);
	});

	it('removes the zone claim of a device with null', async () => {
		expect(await record((c) => c.claimZone('a304', null))).toEqual([
			{ method: 'PATCH', path: '/api/install/devices/a304', body: { zone: null } }
		]);
	});
});
