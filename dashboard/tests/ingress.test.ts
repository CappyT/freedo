import { describe, expect, it } from 'vitest';
import { createClient, type Client } from '../src/lib/api.ts';
import routes from '../../internal/daemon/ingress_routes.json';

// internal/daemon/ingress_routes.json lists the calls that the daemon accepts without a token
// from the Home Assistant gateway. The client must call nothing else, and the list must hold
// nothing that the client does not call.

type Call = (client: Client) => Promise<unknown>;

// One call for each method of the client. Add a line here when the client gets a method.
const calls: Record<string, Call> = {
	health: (c) => c.health(),
	zones: (c) => c.zones(),
	nodes: (c) => c.nodes(),
	home: (c) => c.home(),
	boiler: (c) => c.boiler(),
	circuits: (c) => c.circuits(),
	schedule: (c) => c.schedule(3),
	putSchedule: (c) => c.putSchedule(3, []),
	deleteSchedule: (c) => c.deleteSchedule(3),
	presets: (c) => c.presets(),
	createPreset: (c) => c.createPreset('Winter'),
	renamePreset: (c) => c.renamePreset(1, 'Winter'),
	deletePreset: (c) => c.deletePreset(1),
	activatePreset: (c) => c.activatePreset(1),
	presetSchedules: (c) => c.presetSchedules(1),
	setpoint: (c) => c.setpoint(3, 20, { mode: 'manual' }),
	off: (c) => c.off(3, { mode: 'manual' }),
	auto: (c) => c.auto(3),
	boost: (c) => c.boost(3, true),
	window: (c) => c.window(3, true),
	setAway: (c) => c.setAway(true),
	renameDevice: (c) => c.renameDevice('0201', 'bath'),
	flow: (c) => c.flow(55, 1),
	childLock: (c) => c.childLock('0201', true),
	identify: (c) => c.identify('0201'),
	installInfo: (c) => c.installInfo(),
	setTimezone: (c) => c.setTimezone('Europe/Rome'),
	installDevices: (c) => c.installDevices(),
	installDevice: (c) => c.installDevice('0201'),
	createDevice: (c) => c.createDevice({ eui: '0102030405060708', kind: 'valve', label: 'bath' }),
	claimZone: (c) => c.claimZone('0201', null),
	removal: (c) => c.removal('0201'),
	deleteDevice: (c) => c.deleteDevice('0201', true),
	installZones: (c) => c.installZones(),
	createZone: (c) => c.createZone({ number: 3, name: 'Bath', kind: 'heating' }),
	patchZone: (c) => c.patchZone(3, { name: 'Bath' }),
	deleteZone: (c) => c.deleteZone(3),
	addMember: (c) => c.addMember(3, '0201'),
	removeMember: (c) => c.removeMember(3, '0201'),
	setRole: async (c) => {
		await c.setRole(3, 'reporter', '0201');
		await c.setRole(3, 'reporter', null);
		await c.setRole(3, 'measuring', '0201');
		await c.setRole(3, 'measuring', null);
	},
	installCircuits: (c) => c.installCircuits(),
	createCircuit: (c) => c.createCircuit({ id: 1, name: 'House', heat_source: '0201' }),
	patchCircuit: (c) => c.patchCircuit(1, { name: 'House' }),
	deleteCircuit: (c) => c.deleteCircuit(1),
	pairWindow: (c) => c.pairWindow(),
	openPairWindow: (c) => c.openPairWindow(600),
	pair: (c) => c.pair('0102030405060708')
};

// Every pair that needs no token under ingress. A Home Assistant user who opens the page can make
// each of these calls, so a new line here is a decision about access, not a detail of the client.
const ALLOWED = [
	'GET /api/health',
	'GET /api/zones',
	'GET /api/nodes',
	'GET /api/home',
	'GET /api/boiler',
	'GET /api/circuits',
	'GET /api/zones/{n}/schedule',
	'PUT /api/zones/{n}/schedule',
	'DELETE /api/zones/{n}/schedule',
	'GET /api/presets',
	'POST /api/presets',
	'PATCH /api/presets/{id}',
	'DELETE /api/presets/{id}',
	'POST /api/presets/{id}/activate',
	'GET /api/presets/{id}/schedules',
	'POST /api/zones/{n}/setpoint',
	'POST /api/zones/{n}/off',
	'POST /api/zones/{n}/auto',
	'POST /api/zones/{n}/boost',
	'POST /api/zones/{n}/window',
	'PATCH /api/home',
	'POST /api/boiler/flow',
	'POST /api/nodes/{id}/childlock',
	'POST /api/nodes/{id}/identify',
	'PUT /api/install/devices/{ref}/label',
	'GET /api/install',
	'PUT /api/install/timezone',
	'GET /api/install/devices',
	'POST /api/install/devices',
	'GET /api/install/devices/{ref}',
	'PUT /api/install/devices/{ref}/zone',
	'GET /api/install/devices/{ref}/removal',
	'DELETE /api/install/devices/{ref}',
	'GET /api/install/zones',
	'POST /api/install/zones',
	'PATCH /api/install/zones/{n}',
	'DELETE /api/install/zones/{n}',
	'POST /api/install/zones/{n}/members',
	'DELETE /api/install/zones/{n}/members/{ref}',
	'PUT /api/install/zones/{n}/reporter',
	'DELETE /api/install/zones/{n}/reporter',
	'PUT /api/install/zones/{n}/measuring',
	'DELETE /api/install/zones/{n}/measuring',
	'GET /api/install/circuits',
	'POST /api/install/circuits',
	'PATCH /api/install/circuits/{id}',
	'DELETE /api/install/circuits/{id}',
	'GET /api/pair/window',
	'POST /api/pair/window',
	'POST /api/pair/devices/{ref}'
];

// What stays behind the token under ingress, whatever the client does: the identity of the network,
// the network key, a pairing with a key of the caller, reset, firmware and the debug routes.
const TOKEN_ONLY = [
	'PATCH /api/install',
	'PUT /api/install/network_key',
	'PATCH /api/install/devices/0201',
	'PUT /api/install/zones/3/members',
	'POST /api/pair',
	'POST /api/nodes/0201/factoryreset',
	'POST /api/nodes/0201/reboot',
	'POST /api/nodes/0201/fw',
	'POST /api/nodes/0201/config',
	'POST /api/nodes/0201/netcmd',
	'GET /api/debug/rx',
	'POST /api/debug/decrypt'
];

/** The daemon matches a segment in braces against one non-empty segment. */
function matches(pattern: string, method: string, path: string): boolean {
	const [m, template] = pattern.split(' ') as [string, string];
	if (m !== method) return false;
	const want = template.split('/');
	const got = path.split('/');
	return want.length === got.length && want.every((s, i) => (s.startsWith('{') ? got[i] !== '' : s === got[i]));
}

async function record(call: Call): Promise<string[]> {
	const seen: string[] = [];
	const fetchStub = (async (url: string | URL | Request, init?: RequestInit) => {
		seen.push(`${init?.method ?? 'GET'} ${new URL(String(url)).pathname}`);
		return new Response('{}', { status: 200 });
	}) as unknown as typeof fetch;
	await call(createClient({ base: 'http://daemon/', fetch: fetchStub })).catch(() => undefined);
	return seen;
}

describe('the routes that need no token under ingress', () => {
	it('are called through one test entry per client method', () => {
		const methods = Object.keys(createClient({ base: 'http://daemon/' })).filter((k) => k !== 'url');
		expect(Object.keys(calls).sort()).toEqual(methods.sort());
	});

	it('hold every call of the client', async () => {
		for (const [name, call] of Object.entries(calls)) {
			const seen = await record(call);
			expect(seen.length, name).toBeGreaterThan(0);
			for (const pair of seen) {
				const [method, path] = pair.split(' ') as [string, string];
				expect(
					routes.some((r) => matches(r, method, path)),
					`${name} calls ${pair}, which is not in internal/daemon/ingress_routes.json`
				).toBe(true);
			}
		}
	});

	it('are exactly the allowed set', () => {
		expect([...routes].sort()).toEqual([...ALLOWED].sort());
	});

	it('leave the identity, the network key, reset, firmware and debug behind the token', () => {
		for (const pair of TOKEN_ONLY) {
			const [method, path] = pair.split(' ') as [string, string];
			expect(routes.filter((r) => matches(r, method, path)), pair).toEqual([]);
		}
	});

	it('hold nothing that the client does not call', async () => {
		const used = new Set<string>();
		for (const call of Object.values(calls)) {
			for (const pair of await record(call)) {
				const [method, path] = pair.split(' ') as [string, string];
				routes.filter((r) => matches(r, method, path)).forEach((r) => used.add(r));
			}
		}
		expect(routes.filter((r) => !used.has(r))).toEqual([]);
	});
});
