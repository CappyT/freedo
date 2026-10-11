// Number, time and name formats. Every function takes the locale, so a test can pin it.

import type { Day } from './schedule.ts';

export const NONE = '-';

export function number(value: number | null | undefined, locale: string, maxDigits = 1): string {
	if (value === null || value === undefined || Number.isNaN(value)) return NONE;
	return new Intl.NumberFormat(locale, { maximumFractionDigits: maxDigits, minimumFractionDigits: 0 }).format(value);
}

export function temp(value: number | null | undefined, locale: string): string {
	return value === null || value === undefined ? NONE : number(value, locale, 1) + ' °C';
}

export function percent(value: number | null | undefined, locale: string): string {
	return value === null || value === undefined ? NONE : number(value, locale, 0) + ' %';
}

function toDate(value: string | number | Date): Date {
	return value instanceof Date ? value : new Date(value);
}

export function clock(value: string | number | Date, locale: string): string {
	return new Intl.DateTimeFormat(locale, { hour: '2-digit', minute: '2-digit' }).format(toDate(value));
}

/** The time of day, with the weekday when the date is not today. */
export function when(value: string | number | Date, locale: string, now: Date = new Date()): string {
	const date = toDate(value);
	if (date.toDateString() === now.toDateString()) return clock(date, locale);
	return new Intl.DateTimeFormat(locale, { weekday: 'short', hour: '2-digit', minute: '2-digit' }).format(date);
}

/** How long ago a Unix time was. The result is "-" for a time that never happened. */
export function ago(unixSeconds: number, locale: string, nowMs: number = Date.now()): string {
	if (!unixSeconds) return NONE;
	const seconds = Math.round(unixSeconds - nowMs / 1000);
	const formatter = new Intl.RelativeTimeFormat(locale, { numeric: 'auto' });
	const abs = Math.abs(seconds);
	if (abs < 60) return formatter.format(Math.trunc(seconds / 5) * 5, 'second');
	if (abs < 3600) return formatter.format(Math.round(seconds / 60), 'minute');
	if (abs < 86400) return formatter.format(Math.round(seconds / 3600), 'hour');
	return formatter.format(Math.round(seconds / 86400), 'day');
}

/** 5400 gives "1 h 30 min". */
export function duration(seconds: number): string {
	const total = Math.max(0, Math.round(seconds / 60));
	const h = Math.floor(total / 60);
	const m = total % 60;
	if (h === 0) return `${m} min`;
	return m === 0 ? `${h} h` : `${h} h ${m} min`;
}

const MONDAY = Date.UTC(2024, 0, 1);
const DAY_INDEX: Record<Day, number> = { mon: 0, tue: 1, wed: 2, thu: 3, fri: 4, sat: 5, sun: 6 };

export function dayName(day: Day, locale: string, width: 'short' | 'long' = 'short'): string {
	return new Intl.DateTimeFormat(locale, { weekday: width, timeZone: 'UTC' }).format(
		MONDAY + DAY_INDEX[day] * 86_400_000
	);
}

export function uptime(seconds: number): string {
	const d = Math.floor(seconds / 86400);
	const rest = seconds % 86400;
	return d > 0 ? `${d} d ${duration(rest)}` : duration(rest) === '0 min' ? '<1 min' : duration(rest);
}
