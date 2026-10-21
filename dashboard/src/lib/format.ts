// Number, time and name formats. Every function takes the locale, so a test can pin it.

import type { Day } from './schedule.ts';

export const NONE = '-';

export function number(value: number | null | undefined, locale: string, maxDigits = 1, minDigits = 0): string {
	if (value === null || value === undefined || Number.isNaN(value)) return NONE;
	return new Intl.NumberFormat(locale, { maximumFractionDigits: maxDigits, minimumFractionDigits: minDigits }).format(value);
}

export function temp(value: number | null | undefined, locale: string): string {
	return value === null || value === undefined ? NONE : number(value, locale, 1) + ' °C';
}

/** A short temperature with no unit letter, for a line under a large number: 21 gives "21°". */
export function degrees(value: number | null | undefined, locale: string): string {
	return value === null || value === undefined ? NONE : number(value, locale, 1) + '°';
}

/** The whole part and the decimal part with its separator, always with one decimal: 20 gives "20" and ",0" in Italian. */
export function numberParts(value: number, locale: string): { whole: string; fraction: string } {
	const parts = new Intl.NumberFormat(locale, { minimumFractionDigits: 1, maximumFractionDigits: 1 }).formatToParts(value);
	const at = parts.findIndex((p) => p.type === 'decimal');
	const text = (list: Intl.NumberFormatPart[]) => list.map((p) => p.value).join('');
	return at < 0 ? { whole: text(parts), fraction: '' } : { whole: text(parts.slice(0, at)), fraction: text(parts.slice(at)) };
}

/** A percentage with no space, for a short fact beside a label: 64 gives "64%". */
export function pct(value: number | null | undefined, locale: string): string {
	return value === null || value === undefined ? NONE : number(value, locale, 0) + '%';
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

/** The decimal separator of a language: "," in Italian, "." in English. */
export function decimalSeparator(locale: string): string {
	return new Intl.NumberFormat(locale).formatToParts(1.5).find((p) => p.type === 'decimal')?.value ?? '.';
}

/** A day and a time, with the year only when it is not the current one: "21 ott, 15:52". */
export function dateTime(value: string | number | Date, locale: string, now: Date = new Date()): string {
	const date = toDate(value);
	const year = date.getFullYear() === now.getFullYear() ? undefined : 'numeric';
	return new Intl.DateTimeFormat(locale, { day: 'numeric', month: 'short', year, hour: '2-digit', minute: '2-digit' }).format(date);
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

/** A local date YYYY-MM-DD as a long day: "martedì 21 ottobre", with the year only for another year. */
export function longDay(day: string, locale: string, now: Date = new Date()): string {
	const [y = 0, m = 1, d = 1] = day.split('-').map(Number);
	const year = y === now.getFullYear() ? undefined : 'numeric';
	return new Intl.DateTimeFormat(locale, { weekday: 'long', day: 'numeric', month: 'long', year, timeZone: 'UTC' }).format(
		Date.UTC(y, m - 1, d)
	);
}

/** A local date YYYY-MM-DD as a short day: "21 ott". */
export function shortDay(day: string, locale: string): string {
	const [y = 0, m = 1, d = 1] = day.split('-').map(Number);
	return new Intl.DateTimeFormat(locale, { day: 'numeric', month: 'short', timeZone: 'UTC' }).format(Date.UTC(y, m - 1, d));
}

/** A month YYYY-MM: "ottobre 2026" long, "ott" short. */
export function monthName(month: string, locale: string, width: 'long' | 'short'): string {
	const [y = 0, m = 1] = month.split('-').map(Number);
	const options: Intl.DateTimeFormatOptions = width === 'long' ? { month: 'long', year: 'numeric' } : { month: 'short' };
	return new Intl.DateTimeFormat(locale, { ...options, timeZone: 'UTC' }).format(Date.UTC(y, m - 1, 1));
}

/** An amount of money in an ISO 4217 currency: 4.1 EUR gives "4,10 €" in Italian. */
export function money(value: number | null, currency: string, locale: string): string {
	if (value === null) return NONE;
	return new Intl.NumberFormat(locale, { style: 'currency', currency }).format(value);
}

/** The fuel unit of the daemon as text: m3 gives m³. */
export function fuelUnit(unit: string): string {
	return unit === 'm3' ? 'm³' : unit;
}

/** An amount of fuel with its unit, to two decimals: "3,25 m³". */
export function fuel(value: number | null, unit: string, locale: string): string {
	return value === null ? NONE : `${number(value, locale, 2, 2)} ${fuelUnit(unit)}`;
}

/** A file size: "1,2 MB". */
export function size(bytes: number, locale: string): string {
	if (bytes < 1024) return `${number(bytes, locale, 0)} B`;
	if (bytes < 1024 * 1024) return `${number(bytes / 1024, locale, 0)} kB`;
	return `${number(bytes / (1024 * 1024), locale, 1)} MB`;
}
