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
	identify: (c) => c.identify('0201')
};

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
