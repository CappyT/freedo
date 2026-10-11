import type { EndRule, ZoneKind } from './types.ts';

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

/**
 * The rule that the page selects first. A change that ends at the next block never ends in a
 * zone with no weekly program, so such a zone starts on "until I end it", which says what
 * happens. hasProgram is the field target.hasProgram of GET /api/zones. A daemon that does
 * not send it leaves it unknown: a room then starts on the next block, as it always did, and hot
 * water starts on "until I end it", because the daemon refuses the next block without a program.
 */
export function defaultEndRule(kind: ZoneKind, hasProgram: boolean | undefined): EndRule {
	if (hasProgram === true) return 'until_next';
	if (hasProgram === false) return 'manual';
	return kind === 'hotwater' ? 'manual' : 'until_next';
}
