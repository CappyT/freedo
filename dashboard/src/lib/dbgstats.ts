import type { DebugStat, DebugStats } from './types.ts';

/** The RTC rate of the function 10 state times, in ticks per second. */
export const TICK_HZ = 32768;

/** The value of the named statistic in the last answer, or null when the answer lacks it. */
export function statValue(view: DebugStats | null, name: string): number | null {
	return view?.stats?.find((s) => s.name === name)?.value ?? null;
}

/** A function 10 value counts RTC ticks. */
export const isTicks = (stat: DebugStat): boolean => stat.name?.endsWith('_ticks') ?? false;

/** The time of the newest answer, in Unix seconds, or null before the first answer. */
export function lastRead(views: (DebugStats | null)[]): number | null {
	const times = views.map((v) => (v?.received_at ? Date.parse(v.received_at) : NaN)).filter((ms) => !Number.isNaN(ms));
	return times.length === 0 ? null : Math.max(...times) / 1000;
}

/** The code of a last answer that is not a 2.05, or null. */
export function refusal(view: DebugStats | null): string | null {
	return view?.code && view.code !== '2.05' ? view.code : null;
}
