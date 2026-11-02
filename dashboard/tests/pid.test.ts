import { describe, expect, it } from 'vitest';
import { createClient } from '../src/lib/api.ts';
import { DEFAULT_PID, gainsOf, parsePid, samePid, spanText, suggestionView } from '../src/lib/pid.ts';
import type { PidSuggestion } from '../src/lib/types.ts';

function stub(body: unknown) {
	const calls: { url: string; method: string; body: unknown }[] = [];
	const fetchStub = (async (url: string | URL | Request, init?: RequestInit) => {
		calls.push({ url: String(url), method: init?.method ?? 'GET', body: init?.body === undefined ? undefined : JSON.parse(String(init.body)) });
		return new Response(JSON.stringify(body), { status: 200 });
	}) as unknown as typeof fetch;
	return { calls, client: createClient({ base: 'http://h/', fetch: fetchStub }) };
}

const suggestion = (over: Partial<PidSuggestion> = {}): PidSuggestion => ({
	zone: 2,
	minEpisodes: 3,
	minR2: 0.8,
	heatUsed: 3,
	coolUsed: 1,
	confident: true,
	model: { gainCPerPct: 0.05, tauSec: 7800, deadTimeSec: 300, rmseC: 0.1, r2: 0.91 },
	lossPerHour: 0.2,
	suggested: { kp: 812.4, ki: 120.6, kd: 0, tcSec: 100, kcPctPerC: 8, tiSec: 600, tdSec: 0 },
	current: DEFAULT_PID,
	episodes: [{ kind: 'heat', startedMs: 1, used: true }],
	...over
});

describe('the client calls of the gains', () => {
	it('reads the suggestion of a zone with a GET', async () => {
		const { calls, client } = stub(suggestion());
		expect((await client.pidSuggestion(2)).zone).toBe(2);
		expect(calls[0]).toMatchObject({ url: 'http://h/api/zones/2/pid-suggestion', method: 'GET', body: undefined });
	});

	it('sets the gains with a PATCH and restores the defaults with a null', async () => {
		const { calls, client } = stub({ zone: {} });
		await client.patchZone(2, { pid: { kp: 900, ki: 100, kd: 0 } });
		await client.patchZone(2, { pid: null });
		expect(calls.map((c) => [c.url, c.method, c.body])).toEqual([
			['http://h/api/install/zones/2', 'PATCH', { pid: { kp: 900, ki: 100, kd: 0 } }],
			['http://h/api/install/zones/2', 'PATCH', { pid: null }]
		]);
	});
});

describe('the gains in the fields', () => {
	it('read whole numbers from 0 to 65535', () => {
		expect(parsePid({ kp: '1000', ki: ' 500 ', kd: '0' })).toEqual({ pid: { kp: 1000, ki: 500, kd: 0 } });
		expect(parsePid({ kp: '65536', ki: '1', kd: '1' })).toEqual({ issue: 'range' });
		expect(parsePid({ kp: '', ki: '1', kd: '1' })).toEqual({ issue: 'range' });
		expect(parsePid({ kp: '1.5', ki: '1', kd: '1' })).toEqual({ issue: 'range' });
		expect(parsePid({ kp: '-1', ki: '1', kd: '1' })).toEqual({ issue: 'range' });
	});

	it('refuse kp and ki both 0', () => {
		expect(parsePid({ kp: '0', ki: '0', kd: '5' })).toEqual({ issue: 'zero' });
		expect(parsePid({ kp: '0', ki: '1', kd: '0' })).toEqual({ pid: { kp: 0, ki: 1, kd: 0 } });
	});

	it('take the defaults for a zone with no stored gains', () => {
		expect(samePid(gainsOf(null), DEFAULT_PID)).toBe(true);
		expect(samePid(gainsOf({ kp: 1, ki: 2, kd: 3 }), DEFAULT_PID)).toBe(false);
	});
});

describe('the seconds of the model', () => {
	it.each([
		[30, '30 s'],
		[300, '5 min'],
		[3600, '1 h'],
		[7800, '2 h 10 min']
	])('write %i s as %s', (seconds, text) => {
		expect(spanText(seconds)).toBe(text);
	});
});

describe('the states of the suggestion', () => {
	it('is none without suggested gains, and says when no heat-up exists', () => {
		expect(suggestionView(suggestion({ suggested: null, model: null, heatUsed: 1 }))).toEqual({ state: 'none', heatUsed: 1, minEpisodes: 3, noEpisodes: false });
		expect(suggestionView(suggestion({ suggested: null, model: null, heatUsed: 0, episodes: [] }))).toMatchObject({ state: 'none', noEpisodes: true });
	});

	it('counts the heat-ups alone for the empty message, not the cool-downs', () => {
		const cool = [{ kind: 'cool' as const, startedMs: 1, used: true }];
		expect(suggestionView(suggestion({ suggested: null, model: null, heatUsed: 0, episodes: cool }))).toMatchObject({ noEpisodes: true });
	});

	it('is weak with the reason of the daemon, even when the fit is confident', () => {
		const view = suggestionView(suggestion({ reason: 'the valve controller period is not verified' }));
		expect(view).toMatchObject({ state: 'weak', reason: 'the valve controller period is not verified', gains: { kp: 812, ki: 121, kd: 0 } });
	});

	it('is weak with gains that are not confident, rounded to whole numbers', () => {
		expect(suggestionView(suggestion({ confident: false, heatUsed: 2 }))).toEqual({
			state: 'weak',
			gains: { kp: 812, ki: 121, kd: 0 },
			heatUsed: 2,
			minEpisodes: 3,
			r2: 0.91,
			reason: null
		});
	});

	it('is confident with the fit R2, and keeps the gains in the range of the daemon', () => {
		const view = suggestionView(suggestion({ suggested: { kp: 70000, ki: -3, kd: 5, tcSec: 1, kcPctPerC: 1, tiSec: 1, tdSec: 1 } }));
		expect(view).toMatchObject({ state: 'confident', gains: { kp: 65535, ki: 0, kd: 5 }, r2: 0.91 });
	});
});
