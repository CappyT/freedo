import { de } from './i18n/de.ts';
import { en, type Key } from './i18n/en.ts';
import { fr } from './i18n/fr.ts';
import { it } from './i18n/it.ts';
import * as format from './format.ts';
import { prefs, type Locale } from './prefs.svelte.ts';
import type { Day } from './schedule.ts';

export type { Key };

const dictionaries: Record<Locale, Record<Key, string>> = { en, it, de, fr };

/** Replaces {name} in a text with the value of the parameter. */
export function interpolate(text: string, params?: Record<string, string | number>): string {
	if (!params) return text;
	return text.replace(/\{(\w+)\}/g, (whole, name: string) => (name in params ? String(params[name]) : whole));
}

/** Looks up a text in the language of the page. It reads the language, so a template that calls it updates. */
export function t(key: Key, params?: Record<string, string | number>): string {
	return interpolate(dictionaries[prefs.locale][key] ?? en[key], params);
}

/** The formats of the language of the page. */
export const fmt = {
	number: (v: number | null | undefined, digits = 1, minDigits = 0) => format.number(v, prefs.locale, digits, minDigits),
	temp: (v: number | null | undefined) => format.temp(v, prefs.locale),
	degrees: (v: number | null | undefined) => format.degrees(v, prefs.locale),
	parts: (v: number) => format.numberParts(v, prefs.locale),
	percent: (v: number | null | undefined) => format.percent(v, prefs.locale),
	pct: (v: number | null | undefined) => format.pct(v, prefs.locale),
	clock: (v: string | number | Date) => format.clock(v, prefs.locale),
	when: (v: string | number | Date) => format.when(v, prefs.locale),
	dateTime: (v: string | number | Date) => format.dateTime(v, prefs.locale),
	decimal: () => format.decimalSeparator(prefs.locale),
	ago: (unixSeconds: number) => format.ago(unixSeconds, prefs.locale),
	duration: format.duration,
	uptime: format.uptime,
	day: (d: Day, width: 'short' | 'long' = 'short') => format.dayName(d, prefs.locale, width),
	longDay: (day: string) => format.longDay(day, prefs.locale),
	shortDay: (day: string) => format.shortDay(day, prefs.locale),
	month: (month: string, width: 'long' | 'short' = 'long') => format.monthName(month, prefs.locale, width),
	money: (v: number | null, currency: string) => format.money(v, currency, prefs.locale),
	fuel: (v: number | null, unit: string) => format.fuel(v, unit, prefs.locale),
	fuelUnit: format.fuelUnit,
	size: (bytes: number) => format.size(bytes, prefs.locale)
};
