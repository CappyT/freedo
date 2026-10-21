// The statistics pages: spans, lines and totals derived from the stored samples.

import type { DayTotal, HistorySample } from './types.ts';

/** A stretch of time in milliseconds. */
export interface Span {
	startMs: number;
	endMs: number;
}

/** The runs of samples where flag holds. A sample covers its interval. */
export function spans(samples: HistorySample[], flag: (s: HistorySample) => boolean, intervalMs: number): Span[] {
	const out: Span[] = [];
	for (const s of samples) {
		if (!flag(s)) continue;
		const last = out.at(-1);
		if (last && last.endMs === s.atMs) last.endMs = s.atMs + intervalMs;
		else out.push({ startMs: s.atMs, endMs: s.atMs + intervalMs });
	}
	return out;
}

export interface Point {
	atMs: number;
	v: number;
}

/**
 * The points of one value, split where the value is missing or where the samples skip a slot: a
 * line never bridges a time the daemon did not see.
 */
export function runs(samples: HistorySample[], value: (s: HistorySample) => number | null, intervalMs: number): Point[][] {
	const out: Point[][] = [];
	let run: Point[] | null = null;
	let prevMs: number | null = null;
	for (const s of samples) {
		const v = value(s);
		if (v === null) {
			run = null;
		} else {
			if (run === null || prevMs === null || s.atMs - prevMs > intervalMs * 1.5) {
				run = [];
				out.push(run);
			}
			run.push({ atMs: s.atMs, v });
		}
		prevMs = s.atMs;
	}
	return out;
}

export interface DaySummary {
	/** Minutes with a heating demand above zero. */
	heatingMin: number;
	meanC: number | null;
	minC: number | null;
	maxC: number | null;
	meanHumidity: number | null;
	windowMin: number;
	awayMin: number;
}

const mean = (values: number[]) => (values.length === 0 ? null : values.reduce((a, b) => a + b, 0) / values.length);

export function summarize(samples: HistorySample[], intervalSec: number): DaySummary {
	const step = intervalSec / 60;
	const temps = samples.flatMap((s) => (s.tempC === null ? [] : [s.tempC]));
	const humidity = samples.flatMap((s) => (s.humidityPct === null ? [] : [s.humidityPct]));
	return {
		heatingMin: samples.filter((s) => (s.demandPct ?? 0) > 0).length * step,
		meanC: mean(temps),
		minC: temps.length > 0 ? Math.min(...temps) : null,
		maxC: temps.length > 0 ? Math.max(...temps) : null,
		meanHumidity: mean(humidity),
		windowMin: samples.filter((s) => s.window).length * step,
		awayMin: samples.filter((s) => s.away).length * step
	};
}

/** The sample nearest to atMs, null for no sample. */
export function nearest(samples: HistorySample[], atMs: number): HistorySample | null {
	let best: HistorySample | null = null;
	for (const s of samples) {
		if (best === null || Math.abs(s.atMs - atMs) < Math.abs(best.atMs - atMs)) best = s;
	}
	return best;
}

/** A temperature axis in whole degrees around the values, at least 4 degrees tall. */
export function tempRange(values: number[]): { min: number; max: number } {
	if (values.length === 0) return { min: 16, max: 24 };
	let min = Math.floor(Math.min(...values) - 0.5);
	let max = Math.ceil(Math.max(...values) + 0.5);
	if (max - min < 4) {
		const pad = 4 - (max - min);
		min -= Math.floor(pad / 2);
		max += Math.ceil(pad / 2);
	}
	return { min, max };
}

/** A clean axis top at or above max: 1, 2, 2.5 or 5 times a power of ten. */
export function niceMax(max: number): number {
	if (!(max > 0)) return 1;
	const exp = 10 ** Math.floor(Math.log10(max));
	for (const step of [1, 2, 2.5, 5, 10]) {
		if (step * exp >= max - 1e-9) return step * exp;
	}
	return 10 * exp;
}

/** Ticks from 0 to a clean top at or above max, at a clean step: about count steps. */
export function axisTicks(max: number, count = 3): number[] {
	const step = niceMax(max / count);
	const top = Math.max(step, Math.ceil(max / step - 1e-9) * step);
	const out: number[] = [];
	for (let v = 0; v <= top + 1e-9; v += step) out.push(Math.round(v * 1000) / 1000);
	return out;
}

/** YYYY-MM-DD moved by n days. */
export function shiftDay(day: string, n: number): string {
	const [y = 0, m = 1, d = 1] = day.split('-').map(Number);
	return new Date(Date.UTC(y, m - 1, d + n)).toISOString().slice(0, 10);
}

/** YYYY-MM moved by n months. */
export function shiftMonth(month: string, n: number): string {
	const [y = 0, m = 1] = month.split('-').map(Number);
	return new Date(Date.UTC(y, m - 1 + n, 1)).toISOString().slice(0, 7);
}

/** The last day of a month, YYYY-MM-DD. */
export function monthEnd(month: string): string {
	return shiftDay(shiftMonth(month, 1) + '-01', -1);
}

/** The local date of a time, YYYY-MM-DD. */
export function localDay(date: Date): string {
	const pad = (n: number) => String(n).padStart(2, '0');
	return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`;
}

export interface Total {
	fuel: number;
	fuelEstimate: number;
	heatKWh: number;
	burnerMin: number;
	/** The days the history holds a row for. */
	days: number;
}

export function total(days: DayTotal[]): Total {
	const out: Total = { fuel: 0, fuelEstimate: 0, heatKWh: 0, burnerMin: 0, days: days.length };
	for (const d of days) {
		out.fuel += d.fuel;
		out.fuelEstimate += d.fuelEstimate;
		out.heatKWh += d.heatKWh;
		out.burnerMin += d.burnerMin;
	}
	return out;
}

export interface Bar {
	/** YYYY-MM-DD for a day, YYYY-MM for a month. */
	key: string;
	/** Null where the history holds nothing. */
	total: Total | null;
}

/** One bar per day of the month, null for a day with no row. */
export function monthBars(month: string, days: DayTotal[]): Bar[] {
	const byDay = new Map(days.map((d) => [d.day, d]));
	const out: Bar[] = [];
	for (let day = month + '-01'; day.startsWith(month); day = shiftDay(day, 1)) {
		const row = byDay.get(day);
		out.push({ key: day, total: row ? total([row]) : null });
	}
	return out;
}

/** One bar per month, count months up to last, null for a month with no row. */
export function yearBars(last: string, count: number, days: DayTotal[]): Bar[] {
	const out: Bar[] = [];
	for (let i = count - 1; i >= 0; i--) {
		const month = shiftMonth(last, -i);
		const rows = days.filter((d) => d.day.startsWith(month));
		out.push({ key: month, total: rows.length > 0 ? total(rows) : null });
	}
	return out;
}

/** The cost of an amount of fuel, null without a price. */
export function cost(fuel: number, price: number | null): number | null {
	return price === null ? null : fuel * price;
}
