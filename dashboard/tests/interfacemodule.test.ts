import { describe, expect, it } from 'vitest';
import { ApiError, createClient, setInterfaceModule } from '../src/lib/api.ts';
import { refusal } from '../src/lib/backup.ts';
import { buildDevices } from '../src/lib/devices.ts';
import { groupByFamily } from '../src/lib/interfacemodule.ts';
import type { Health } from '../src/lib/types.ts';
import { device } from './fixtures.ts';

describe('the ids of the interface module select', () => {
	it('group by family, with the families and the ids in order', () => {
		expect(
			groupByFamily([
				{ module: 71, family: 'opentherm' },
				{ module: 5, family: 'ebus_a' },
				{ module: 121, family: 'ebus_b' },
				{ module: 1, family: 'ebus_a' }
			])
		).toEqual([
			{ family: 'ebus_a', ids: [1, 5] },
			{ family: 'ebus_b', ids: [121] },
			{ family: 'opentherm', ids: [71] }
		]);
		expect(groupByFamily([])).toEqual([]);
	});
});

describe('the interface module calls', () => {
	it('read the id and the known list of one device', async () => {
		const seen: string[] = [];
		const fetchStub = (async (url: string | URL | Request, init?: RequestInit) => {
			seen.push(`${init?.method ?? 'GET'} ${new URL(String(url)).pathname}`);
			return Response.json({ module: null, family: 'unknown', source: null, known: [{ module: 71, family: 'opentherm' }] });
		}) as unknown as typeof fetch;
		const view = await createClient({ base: 'http://daemon/', fetch: fetchStub }).interfaceModule('0201');
		expect(seen).toEqual(['GET /api/install/devices/0201/interface_module']);
		expect(view.known).toEqual([{ module: 71, family: 'opentherm' }]);
	});

	it('send the id as the module field, with the token', async () => {
		const seen: { method?: string; body?: unknown; authorization: string | null }[] = [];
		const fetchStub = (async (_url: string | URL | Request, init?: RequestInit) => {
			seen.push({ method: init?.method, body: init?.body, authorization: new Headers(init?.headers).get('Authorization') });
			return Response.json({ module: 80, family: 'ebus_a', source: 'operator', known: [] });
		}) as unknown as typeof fetch;
		await setInterfaceModule('0201', 80, { base: 'http://daemon/', fetch: fetchStub, token: () => 'secret' });
		expect(seen).toEqual([{ method: 'PUT', body: '{"module":80}', authorization: 'Bearer secret' }]);
	});

	it('tell a user who is not an administrator from a rejected id', async () => {
		const fetchStub = (async () =>
			Response.json({ error: 'only a Home Assistant administrator can call PUT' }, { status: 403 })) as unknown as typeof fetch;
		const error = await setInterfaceModule('0201', 80, { base: 'http://daemon/', fetch: fetchStub }).catch((e: unknown) => e);
		expect(error).toBeInstanceOf(ApiError);
		expect(refusal(error)).toBe('admin');
		expect(refusal(new ApiError(400, 'tado: interface module id 112 is the valve'))).toBeNull();
	});
});

describe('the alert of a thermostat with no interface module id', () => {
	it('shows on the row of the device that the daemon names', () => {
		const health = {
			devices: [device({ short: '0301', kind: 'thermostat' }), device({ short: '0201' })],
			alerts: [{ device: 'RU9000000001', short: '0301', label: 'hall', alerts: ['setInterfaceModule'] }]
		} as unknown as Health;
		const rows = buildDevices([], health, []);
		expect(rows[0]?.alerts).toEqual(['setInterfaceModule']);
		expect(rows[1]?.alerts).toEqual([]);
	});
});
