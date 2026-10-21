import { describe, expect, it } from 'vitest';
import {
	axisTicks,
	cost,
	monthBars,
	monthEnd,
	nearest,
	niceMax,
	runs,
	shiftDay,
	shiftMonth,
	spans,
	summarize,
	tempRange,
	yearBars
} from '../src/lib/history.ts';
import type { DayTotal, HistorySample } from '../src/lib/types.ts';

const STEP = 300_000;
const T0 = Date.UTC(2026, 9, 21, 6, 0);

function sample(i: number, fields: Partial<HistorySample> = {}): HistorySample {
	return {
		atMs: T0 + i * STEP,
		tempC: 20,
		targetC: 21,
		on: true,
		humidityPct: null,
		demandPct: 0,
		window: false,
		away: false,
		...fields
	};
}

function day(d: string, fuel: number): DayTotal {
	return { day: d, fuel, fuelEstimate: fuel, heatKWh: fuel * 10, burnerMin: 5 };
}

describe('spans', () => {
	it('joins touching samples and splits at a gap', () => {
		const samples = [sample(0, { window: true }), sample(1, { window: true }), sample(2), sample(3, { window: true })];
		expect(spans(samples, (s) => s.window, STEP)).toEqual([
			{ startMs: T0, endMs: T0 + 2 * STEP },
			{ startMs: T0 + 3 * STEP, endMs: T0 + 4 * STEP }
		]);
	});
});

describe('runs', () => {
	it('breaks a line at a missing value and at a skipped slot', () => {
		const samples = [sample(0), sample(1, { tempC: null }), sample(2), sample(3), sample(5)];
		const lines = runs(samples, (s) => s.tempC, STEP);
		expect(lines.map((l) => l.map((p) => (p.atMs - T0) / STEP))).toEqual([[0], [2, 3], [5]]);
	});
});

describe('summarize', () => {
	it('counts heating, window and away minutes and the temperatures', () => {
		const samples = [
			sample(0, { demandPct: 40, tempC: 19, humidityPct: 50 }),
			sample(1, { demandPct: 10, tempC: 20, window: true, humidityPct: 60 }),
			sample(2, { demandPct: 0, tempC: null, away: true }),
			sample(3, { demandPct: null, tempC: 21 })
		];
		expect(summarize(samples, 300)).toEqual({
			heatingMin: 10,
			meanC: 20,
			minC: 19,
			maxC: 21,
			meanHumidity: 55,
			windowMin: 5,
			awayMin: 5
		});
	});

	it('gives no temperature for a day without one', () => {
		const s = summarize([sample(0, { tempC: null })], 300);
		expect([s.meanC, s.minC, s.maxC, s.meanHumidity]).toEqual([null, null, null, null]);
	});
});

describe('nearest', () => {
	it('finds the closest sample', () => {
		const samples = [sample(0), sample(1), sample(2)];
		expect(nearest(samples, T0 + 1.4 * STEP)?.atMs).toBe(T0 + STEP);
		expect(nearest([], T0)).toBeNull();
	});
});

describe('axes', () => {
	it('pads a temperature range to whole degrees, at least 4 tall', () => {
		expect(tempRange([20.2, 21.1])).toEqual({ min: 19, max: 23 });
		expect(tempRange([15.3, 23.8])).toEqual({ min: 14, max: 25 });
		expect(tempRange([])).toEqual({ min: 16, max: 24 });
	});

	it('steps an axis at a clean interval', () => {
		expect(axisTicks(2.2)).toEqual([0, 1, 2, 3]);
		expect(axisTicks(7)).toEqual([0, 2.5, 5, 7.5]);
		expect(axisTicks(0)).toEqual([0, 1]);
		expect(axisTicks(150)).toEqual([0, 50, 100, 150]);
	});

	it('rounds an axis top up to a clean number', () => {
		expect([0, 0.8, 1, 3.2, 7.4, 12, 230].map(niceMax)).toEqual([1, 1, 1, 5, 10, 20, 250]);
	});
});

describe('dates', () => {
	it('moves days and months across their ends', () => {
		expect(shiftDay('2026-10-31', 1)).toBe('2026-11-01');
		expect(shiftDay('2026-03-01', -1)).toBe('2026-02-28');
		expect(shiftMonth('2026-01', -1)).toBe('2025-12');
		expect(shiftMonth('2025-11', 13)).toBe('2026-12');
		expect(monthEnd('2024-02')).toBe('2024-02-29');
	});
});

describe('bars', () => {
	it('gives every day of a month, null where the history has no row', () => {
		const bars = monthBars('2026-02', [day('2026-02-03', 2.5), day('2026-03-01', 9)]);
		expect(bars).toHaveLength(28);
		expect(bars[0]).toEqual({ key: '2026-02-01', total: null });
		expect(bars[2]?.total).toEqual({ fuel: 2.5, fuelEstimate: 2.5, heatKWh: 25, burnerMin: 5, days: 1 });
	});

	it('sums the days of each month up to the last one', () => {
		const bars = yearBars('2026-10', 13, [day('2025-10-04', 1), day('2026-10-01', 2), day('2026-10-02', 3)]);
		expect(bars.map((b) => b.key)).toEqual([
			'2025-10', '2025-11', '2025-12', '2026-01', '2026-02', '2026-03', '2026-04',
			'2026-05', '2026-06', '2026-07', '2026-08', '2026-09', '2026-10'
		]);
		expect(bars[0]?.total?.fuel).toBe(1);
		expect(bars[1]?.total).toBeNull();
		expect(bars[12]?.total).toEqual({ fuel: 5, fuelEstimate: 5, heatKWh: 50, burnerMin: 10, days: 2 });
	});

	it('prices fuel only with a price', () => {
		expect(cost(2, 1.25)).toBe(2.5);
		expect(cost(2, null)).toBeNull();
	});
});
