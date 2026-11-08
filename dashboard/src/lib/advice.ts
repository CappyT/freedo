// The consumption advisor: ranges, clock times, links and texts of GET /api/history/advice.

import type { Key } from './i18n/en.ts';
import type { Advice, AdviceConfidence, AdviceKind, AdviceSplit, Estimate } from './types.ts';

/** The windows of the advisor, in days. */
export const ADVICE_WINDOWS = [7, 28, 90] as const;
export type AdviceWindow = (typeof ADVICE_WINDOWS)[number];
export const DEFAULT_ADVICE_WINDOW: AdviceWindow = 28;

/** Days of valid data that the model needs before the savings show. The daemon decides; this is only the text. */
export const MODEL_DAYS = 14;

/** The comfort limit of the daemon: 16 to 25 C. */
export const COMFORT_MAX_C = { min: 16, max: 25 };

/** Minutes of the day as HH:MM. 1440 gives 00:00, the end of the day. */
export function clockOf(minutes: number): string {
	const m = ((Math.round(minutes) % 1440) + 1440) % 1440;
	return `${String(Math.floor(m / 60)).padStart(2, '0')}:${String(m % 60).padStart(2, '0')}`;
}

/** An estimate as "low-high", or one number when both ends read the same. */
export function rangeText(e: Estimate, format: (v: number) => string): string {
	const low = format(e.low);
	const high = format(e.high);
	return low === high ? low : `${low}-${high}`;
}

/** The page that holds the setting behind an advice, as the route and the parameters that resolve() takes. */
export type AdviceLink = { route: '/' | '/programs' | '/boiler' } | { route: '/rooms/[n]'; params: { n: string } };

export function adviceLink(a: Pick<Advice, 'kind' | 'zones'>): AdviceLink {
	switch (a.kind) {
		case 'setpoint':
		case 'setback':
			return { route: '/programs' };
		case 'short_cycle':
			return { route: '/boiler' };
		default:
			return a.zones.length > 0 ? { route: '/rooms/[n]', params: { n: String(a.zones[0]) } } : { route: '/' };
	}
}

export const confidenceTone: Record<AdviceConfidence, 'idle' | 'warning' | 'off'> = {
	high: 'idle',
	medium: 'warning',
	low: 'off'
};

const TEXT: Record<AdviceKind, { title: Key; body: Key }> = {
	window: { title: 'advice.window.title', body: 'advice.window.body' },
	vent: { title: 'advice.vent.title', body: 'advice.vent.body' },
	away: { title: 'advice.away.title', body: 'advice.away.body' },
	setpoint: { title: 'advice.setpoint.title', body: 'advice.setpoint.body' },
	setback: { title: 'advice.setback.title', body: 'advice.setback.body' },
	unreachable: { title: 'advice.unreachable.title', body: 'advice.unreachable.body' },
	short_cycle: { title: 'advice.short_cycle.title', body: 'advice.short_cycle.body' }
};

/** The title key and the body key of a kind. */
export const adviceKeys = (kind: AdviceKind) => TEXT[kind];

/** What the texts need from the page: the language and the room names. */
export interface TextContext {
	/** The name of a room. */
	name: (zone: number) => string;
	/** The names of the rooms as one text: "Bath, Hall and Study". */
	list: (names: string[]) => string;
	number: (v: number, digits: number) => string;
	temp: (v: number) => string;
}

/** The values of {name} in the body of an advice, from its evidence. */
export function adviceParams(a: Advice, c: TextContext): Record<string, string | number> {
	const e = a.evidence;
	const out: Record<string, string | number> = {
		zones: c.list(a.zones.map(c.name)),
		hours: c.number(a.hours, a.hours < 10 ? 1 : 0)
	};
	if (a.kind === 'vent') {
		const periods = e.periods ?? [];
		out.count = periods.length;
		out.pct = c.number(Math.max(0, ...periods.map((p) => p.humidityDropPct)), 0);
	}
	if (a.kind === 'setpoint') {
		out.from = clockOf(e.fromMin ?? 0);
		out.to = clockOf(e.toMin ?? 0);
		out.target = c.temp(e.targetC ?? 0);
		out.threshold = c.temp(e.thresholdC ?? 0);
	}
	if (a.kind === 'setback') {
		out.comfort = c.temp(e.comfortC ?? 0);
		out.setback = c.temp(e.setbackC ?? 0);
		out.proposed = c.temp(e.proposedC ?? 0);
		out.perDay = c.number(e.hoursPerDay ?? 0, 1);
	}
	if (a.kind === 'unreachable') out.gap = c.temp(e.gapC ?? 0);
	if (a.kind === 'short_cycle') {
		out.days = e.days ?? 0;
		out.starts = c.number(e.startsPerBurnerHour ?? 0, 1);
	}
	return out;
}

export type SplitRowKind = 'zone' | 'hotWater' | 'unattributed';

export interface SplitRow {
	kind: SplitRowKind;
	/** The room number of a zone row. */
	zone?: number;
	fuel: number;
	/** The part of all the fuel of the window, 0 to 1. */
	fraction: number;
}

/**
 * The rows of "Where it goes": the rooms by fuel, highest first, then the hot water and the fuel that
 * no room or hot water run explains. A row with no fuel is left out.
 */
export function splitRows(split: AdviceSplit): SplitRow[] {
	const part = (fuel: number) => (split.fuel > 0 ? Math.min(1, Math.max(0, fuel / split.fuel)) : 0);
	const rows: SplitRow[] = [...split.zones]
		.sort((a, b) => b.fuel - a.fuel || a.zone - b.zone)
		.map((z) => ({ kind: 'zone', zone: z.zone, fuel: z.fuel, fraction: part(z.fuel) }));
	if (split.hotWater > 0) rows.push({ kind: 'hotWater', fuel: split.hotWater, fraction: part(split.hotWater) });
	if (split.unattributed > 0) rows.push({ kind: 'unattributed', fuel: split.unattributed, fraction: part(split.unattributed) });
	return rows;
}
