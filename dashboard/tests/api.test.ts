import { describe, expect, it, vi } from 'vitest';
import { ApiError, createClient } from '../src/lib/api.ts';

interface Call {
	url: string;
	method: string;
	headers: Record<string, string>;
	body: unknown;
}

/** A fetch that records the request and answers with the given status and JSON. */
function stub(status: number, body: unknown) {
	const calls: Call[] = [];
	const fetchStub = vi.fn(async (url: string | URL | Request, init?: RequestInit) => {
		calls.push({
			url: String(url),
			method: init?.method ?? 'GET',
			headers: (init?.headers ?? {}) as Record<string, string>,
			body: init?.body === undefined ? undefined : JSON.parse(String(init.body))
		});
		return new Response(body === undefined ? '' : JSON.stringify(body), { status });
	}) as unknown as typeof fetch;
	return { calls, fetch: fetchStub };
}

describe('API URLs are relative to the document', () => {
	it.each([
		['the daemon root', 'http://daemon:8080/', 'http://daemon:8080/api/zones'],
		['the index file', 'http://daemon:8080/index.html', 'http://daemon:8080/api/zones'],
		[
			'a Home Assistant ingress prefix',
			'https://ha.example/api/hassio_ingress/Zx9Y_token/',
			'https://ha.example/api/hassio_ingress/Zx9Y_token/api/zones'
		],
		[
			'a prefix and a fragment route',
			'https://ha.example/api/hassio_ingress/abc/#/zones/2',
			'https://ha.example/api/hassio_ingress/abc/api/zones'
		],
		['a query string', 'http://daemon:8080/?theme=dark', 'http://daemon:8080/api/zones']
	])('under %s', async (_name, base, want) => {
		const { calls, fetch } = stub(200, { declared: [], demand: [] });
		await createClient({ base, fetch }).zones();
		expect(calls[0]?.url).toBe(want);
	});
});

describe('the bearer token', () => {
	it('is sent when the page has one', async () => {
		const { calls, fetch } = stub(200, []);
		await createClient({ base: 'http://h/', fetch, token: () => 'secret' }).nodes();
		expect(calls[0]?.headers.Authorization).toBe('Bearer secret');
	});

	it('is not sent when the page has none', async () => {
		const { calls, fetch } = stub(200, []);
		await createClient({ base: 'http://h/', fetch, token: () => null }).nodes();
		expect(calls[0]?.headers.Authorization).toBeUndefined();
	});

	it('tells the page when the daemon refuses it', async () => {
		const { fetch } = stub(401, { error: 'invalid bearer token' });
		const onUnauthorized = vi.fn();
		const client = createClient({ base: 'http://h/', fetch, token: () => 'wrong', onUnauthorized });
		await expect(client.zones()).rejects.toMatchObject({ status: 401, message: 'invalid bearer token' });
		expect(onUnauthorized).toHaveBeenCalledWith(true);
	});

	it('says that no token was sent when the daemon asks for one', async () => {
		const { fetch } = stub(401, { error: 'missing bearer token' });
		const onUnauthorized = vi.fn();
		await createClient({ base: 'http://h/', fetch, onUnauthorized })
			.zones()
			.catch(() => undefined);
		expect(onUnauthorized).toHaveBeenCalledWith(false);
	});
});

describe('commands', () => {
	it('send the rule names of the API for a setpoint', async () => {
		const { calls, fetch } = stub(200, { queued: true, pending: 1 });
		const client = createClient({ base: 'http://h/', fetch });
		await client.setpoint(2, 21.5, { mode: 'until_auto_change' });
		await client.setpoint(2, 21, { mode: 'timer', durationSec: 5400 });
		await client.setpoint(2, 20, { mode: 'manual', durationSec: 5400 });
		expect(calls.map((c) => [c.method, c.url, c.body])).toEqual([
			['POST', 'http://h/api/zones/2/setpoint', { setpointC: 21.5, mode: 'until_auto_change' }],
			['POST', 'http://h/api/zones/2/setpoint', { setpointC: 21, mode: 'timer', durationSec: 5400 }],
			// Only a timer carries a duration: the daemon refuses it on the other rules.
			['POST', 'http://h/api/zones/2/setpoint', { setpointC: 20, mode: 'manual' }]
		]);
	});

	it('use the routes of docs/API.md', async () => {
		const { calls, fetch } = stub(200, {});
		const client = createClient({ base: 'http://h/', fetch });
		await client.off(3, { mode: 'until_next' });
		await client.auto(3);
		await client.boost(3, true);
		await client.window(3, false);
		await client.setAway(true);
		await client.renameDevice('0201', 'bath');
		await client.flow(55, 1);
		await client.childLock('0201', true);
		await client.identify('0201');
		expect(calls.map((c) => `${c.method} ${new URL(c.url).pathname}`)).toEqual([
			'POST /api/zones/3/off',
			'POST /api/zones/3/auto',
			'POST /api/zones/3/boost',
			'POST /api/zones/3/window',
			'PATCH /api/home',
			'PATCH /api/install/devices/0201',
			'POST /api/boiler/flow',
			'POST /api/nodes/0201/childlock',
			'POST /api/nodes/0201/identify'
		]);
		expect(calls[5]?.body).toEqual({ label: 'bath' });
		expect(calls[6]?.body).toEqual({ tempC: 55, id: 1 });
		expect(calls[4]?.body).toEqual({ away: true });
	});

	it('write the schedule with one request', async () => {
		const { calls, fetch } = stub(200, { zone: 2, members: [], programs: [] });
		const programs = [{ days: ['mon'], entries: [{ minute: 0, setpoint_c: 18, enabled: true }] }];
		await createClient({ base: 'http://h/', fetch }).putSchedule(2, programs);
		expect(calls).toHaveLength(1);
		expect(calls[0]).toMatchObject({ method: 'PUT', body: { programs } });
	});
});

describe('errors', () => {
	it('carry the message of the daemon', async () => {
		const { fetch } = stub(400, { error: 'setpointC must be within 5..25' });
		const error = await createClient({ base: 'http://h/', fetch })
			.setpoint(1, 40, { mode: 'manual' })
			.catch((e: unknown) => e);
		expect(error).toBeInstanceOf(ApiError);
		expect(error).toMatchObject({ status: 400, message: 'setpointC must be within 5..25' });
	});

	it('report a daemon that does not answer as status 0', async () => {
		const fetch = (async () => {
			throw new TypeError('Failed to fetch');
		}) as unknown as typeof globalThis.fetch;
		await expect(createClient({ base: 'http://h/', fetch }).health()).rejects.toMatchObject({ status: 0 });
	});

	it('keep the health body of a busy engine', async () => {
		const { fetch } = stub(503, { ok: false, engine: 'busy', mode: 'live' });
		const health = await createClient({ base: 'http://h/', fetch }).health();
		expect(health).toMatchObject({ ok: false, engine: 'busy' });
	});

	it('read a missing schedule as null', async () => {
		const { fetch } = stub(404, { error: 'zone 3 has no schedule' });
		expect(await createClient({ base: 'http://h/', fetch }).schedule(3)).toBeNull();
	});
});
