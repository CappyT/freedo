import type { EndRule } from './types.ts';

/** The rules of the API, in the order the dashboard offers them. */
export const END_RULES: EndRule[] = ['until_next', 'until_auto_change', 'timer', 'manual'];

/** The daemon accepts a timer of 1 second to 7 days. */
export const MAX_TIMER_SEC = 604_800;

export function timerSeconds(hours: number, minutes: number): number {
	return Math.round(hours) * 3600 + Math.round(minutes) * 60;
}

export function validTimer(seconds: number): boolean {
	return Number.isFinite(seconds) && seconds >= 60 && seconds <= MAX_TIMER_SEC;
}
