import { describe, expect, it } from 'vitest';
import { adviceKeys, adviceLink, adviceParams, clockOf, confidenceTone, rangeText, splitRows, type TextContext } from '../src/lib/advice.ts';
import type { Advice, AdviceSplit } from '../src/lib/types.ts';

const est = (value: number, low = value, high = value) => ({ value, low, high });
const advice = (over: Partial<Advice> = {}): Advice => ({
	id: 'a',
	kind: 'window',
	zones: [3, 5],
	hours: 6.4,
	fuel: est(1, 0.5, 1.5),
	cost: null,
	confidence: 'medium',
	evidence: {},
	...over
});

const ctx: TextContext = {
	name: (n) => ({ 3: 'Bath', 5: 'Study' })[n] ?? `Room ${n}`,
	list: (names) => names.join(' and '),
	number: (v, d) => v.toFixed(d),
	temp: (v) => `${v.toFixed(1)} C`
};

describe('clockOf', () => {
	it('pads hours and minutes', () => {
		expect(clockOf(0)).toBe('00:00');
		expect(clockOf(6 * 60 + 5)).toBe('06:05');
		expect(clockOf(22 * 60 + 30)).toBe('22:30');
	});

	it('reads the end of the day as 00:00', () => {
		expect(clockOf(1440)).toBe('00:00');
	});
});

describe('rangeText', () => {
	const f = (v: number) => v.toFixed(2);
	it('joins low and high', () => {
		expect(rangeText(est(1, 0.4, 0.9), f)).toBe('0.40-0.90');
	});

	it('gives one number when both ends read the same', () => {
		expect(rangeText(est(1, 0.501, 0.499), f)).toBe('0.50');
	});
});

describe('adviceLink', () => {
	it('sends a setpoint and a setback to the programs', () => {
		expect(adviceLink(advice({ kind: 'setpoint' }))).toEqual({ route: '/programs' });
		expect(adviceLink(advice({ kind: 'setback' }))).toEqual({ route: '/programs' });
	});

	it('sends the other room kinds to the first room', () => {
		for (const kind of ['window', 'vent', 'unreachable', 'away'] as const) expect(adviceLink(advice({ kind }))).toEqual({ route: '/rooms/[n]', params: { n: '3' } });
	});

	it('sends short cycles to the boiler', () => {
		expect(adviceLink(advice({ kind: 'short_cycle', zones: [] }))).toEqual({ route: '/boiler' });
	});

	it('falls back to the home page for a room kind with no room', () => {
		expect(adviceLink(advice({ kind: 'window', zones: [] }))).toEqual({ route: '/' });
	});
});

describe('the texts of an advice', () => {
	it('have a title and a body key for every kind', () => {
		for (const kind of ['window', 'vent', 'away', 'setpoint', 'setback', 'unreachable', 'short_cycle'] as const) {
			expect(adviceKeys(kind)).toEqual({ title: `advice.${kind}.title`, body: `advice.${kind}.body` });
		}
	});

	it('join the rooms by name', () => {
		expect(adviceParams(advice(), ctx)).toMatchObject({ zones: 'Bath and Study', hours: '6.4' });
	});

	it('read the setpoint block as clock times', () => {
		const a = advice({ kind: 'setpoint', evidence: { targetC: 21.5, thresholdC: 21, fromMin: 390, toMin: 1320 } });
		expect(adviceParams(a, ctx)).toMatchObject({ from: '06:30', to: '22:00', target: '21.5 C', threshold: '21.0 C' });
	});

	it('count the vent periods and take the largest humidity drop', () => {
		const periods = [
			{ startMs: 0, endMs: 1, riseC: 0.5, humidityDropPct: 4 },
			{ startMs: 2, endMs: 3, riseC: 0.2, humidityDropPct: 9 }
		];
		expect(adviceParams(advice({ kind: 'vent', evidence: { periods } }), ctx)).toMatchObject({ count: 2, pct: '9' });
	});

	it('hold the setback, unreachable and short cycle evidence', () => {
		expect(adviceParams(advice({ kind: 'setback', evidence: { comfortC: 20, setbackC: 18, proposedC: 17, hoursPerDay: 8 } }), ctx)).toMatchObject({
			comfort: '20.0 C',
			setback: '18.0 C',
			proposed: '17.0 C',
			perDay: '8.0'
		});
		expect(adviceParams(advice({ kind: 'unreachable', evidence: { gapC: 2.5 } }), ctx)).toMatchObject({ gap: '2.5 C' });
		expect(adviceParams(advice({ kind: 'short_cycle', evidence: { days: 5, startsPerBurnerHour: 6.2 } }), ctx)).toMatchObject({ days: 5, starts: '6.2' });
	});
});

describe('confidenceTone', () => {
	it('maps each level to a status tone', () => {
		expect(confidenceTone).toEqual({ high: 'idle', medium: 'warning', low: 'off' });
	});
});

describe('splitRows', () => {
	const split = (over: Partial<AdviceSplit> = {}): AdviceSplit => ({
		fuel: 10,
		heating: 8,
		hotWater: 1.5,
		unattributed: 0.5,
		weighted: true,
		zones: [
			{ zone: 5, fuel: 2, share: 0.25, fuelPerDegreeHour: null },
			{ zone: 3, fuel: 6, share: 0.75, fuelPerDegreeHour: 0.1 }
		],
		...over
	});

	it('puts the rooms first by fuel, then the hot water and the unattributed fuel', () => {
		expect(splitRows(split()).map((r) => [r.kind, r.zone, r.fuel, r.fraction])).toEqual([
			['zone', 3, 6, 0.6],
			['zone', 5, 2, 0.2],
			['hotWater', undefined, 1.5, 0.15],
			['unattributed', undefined, 0.5, 0.05]
		]);
	});

	it('leaves out a row with no fuel', () => {
		expect(splitRows(split({ hotWater: 0, unattributed: 0 })).map((r) => r.kind)).toEqual(['zone', 'zone']);
	});

	it('gives a zero fraction when the window holds no fuel', () => {
		expect(splitRows(split({ fuel: 0 })).every((r) => r.fraction === 0)).toBe(true);
	});
});
