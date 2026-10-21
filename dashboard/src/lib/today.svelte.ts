// The program of today on the clock of the installation, and the words of a zone state.

import { data } from './data.svelte.ts';
import { fmt, t } from './i18n.svelte.ts';
import type { IconName } from './Icon.svelte';
import { formatTime, localNow, nextChange, type Change, type Day, type Group } from './schedule.ts';
import type { StateKey } from './zones.ts';

export const stateIcons: Record<StateKey, IconName> = {
	heating: 'flame',
	idle: 'idle',
	on: 'idle',
	away: 'away',
	off: 'power',
	windowOpen: 'window',
	offline: 'offline',
	fault: 'fault'
};

// A screen left open keeps the time of now current: the marker moves and the next change follows.
let now = $state(Date.now());
if (typeof window !== 'undefined') setInterval(() => (now = Date.now()), 30_000);

export interface Today {
	groups: Group[];
	day: Day;
	minute: number;
	next: Change | null;
}

/** The weekday and the minute on the installation clock from the daemon; without it, the browser clock. */
export function clockNow(): { day: Day; minute: number } {
	return localNow(now, data.health?.timezone?.offsetMin ?? -new Date(now).getTimezoneOffset());
}

/** Today in the program in force. */
export function todayOf(groups: Group[] | null): Today | null {
	if (!groups || groups.length === 0) return null;
	const at = clockNow();
	return { groups, day: at.day, minute: at.minute, next: nextChange(groups, at) };
}

/** "at 22:30 it goes to 18°", or null when nothing changes. */
export function nextText(change: Change | null): string | null {
	if (!change) return null;
	const time = formatTime(change.minute);
	const when = change.tomorrow ? t('when.tomorrow', { time }) : t('when.today', { time });
	return change.enabled ? t('zone.nextAt', { when, temp: fmt.degrees(change.setpointC) }) : t('zone.nextOffAt', { when });
}
