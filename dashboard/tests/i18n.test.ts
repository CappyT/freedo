import { describe, expect, it } from 'vitest';
import * as format from '../src/lib/format.ts';
import { de } from '../src/lib/i18n/de.ts';
import { en } from '../src/lib/i18n/en.ts';
import { fr } from '../src/lib/i18n/fr.ts';
import { it as italian } from '../src/lib/i18n/it.ts';

const placeholders = (text: string) => [...text.matchAll(/\{(\w+)\}/g)].map((m) => m[1]).sort();

describe('dictionaries', () => {
	for (const [name, dictionary] of Object.entries({ it: italian, de, fr })) {
		it(`${name} has the keys of English and no other`, () => {
			expect(Object.keys(dictionary).sort()).toEqual(Object.keys(en).sort());
		});

		it(`${name} fills the same values as English`, () => {
			for (const [key, text] of Object.entries(en)) {
				expect(placeholders((dictionary as Record<string, string>)[key] ?? ''), key).toEqual(placeholders(text));
			}
		});

		it(`${name} leaves no text empty`, () => {
			for (const [key, text] of Object.entries(dictionary)) expect(text.trim(), key).not.toBe('');
		});
	}
});

describe('formats', () => {
	it('write a temperature and a percentage in the language', () => {
		expect(format.temp(21.5, 'en')).toBe('21.5 °C');
		expect(format.temp(21.5, 'de')).toBe('21,5 °C');
		expect(format.temp(null, 'en')).toBe('-');
		expect(format.percent(43, 'en')).toBe('43 %');
	});

	it('write the short forms of a tile and a room screen', () => {
		expect(format.degrees(21, 'it')).toBe('21°');
		expect(format.degrees(20.5, 'en')).toBe('20.5°');
		expect(format.degrees(null, 'en')).toBe('-');
		expect(format.pct(64, 'it')).toBe('64%');
		expect(format.numberParts(20.4, 'it')).toEqual({ whole: '20', fraction: ',4' });
		expect(format.numberParts(19, 'en')).toEqual({ whole: '19', fraction: '.0' });
		expect(format.numberParts(-2.5, 'de')).toEqual({ whole: '-2', fraction: ',5' });
	});

	it('write a day and a time, with the year only for another year', () => {
		const now = new Date(2026, 9, 21, 16, 0);
		expect(format.dateTime(new Date(2026, 9, 21, 15, 52), 'en', now)).toBe('Oct 21, 03:52 PM');
		expect(format.dateTime(new Date(2025, 0, 2, 9, 5), 'en', now)).toBe('Jan 2, 2025, 09:05 AM');
	});

	it('write a duration', () => {
		expect(format.duration(5400)).toBe('1 h 30 min');
		expect(format.duration(1800)).toBe('30 min');
		expect(format.duration(7200)).toBe('2 h');
	});

	it('name the days from Monday', () => {
		expect(format.dayName('mon', 'en')).toBe('Mon');
		expect(format.dayName('sun', 'en', 'long')).toBe('Sunday');
		expect(format.dayName('mon', 'it', 'long')).toBe('lunedì');
	});

	it('say how long ago a time was', () => {
		const now = 1_700_000_000_000;
		expect(format.ago(0, 'en', now)).toBe('-');
		expect(format.ago(now / 1000 - 600, 'en', now)).toBe('10 minutes ago');
		expect(format.ago(now / 1000 - 7200, 'en', now)).toBe('2 hours ago');
	});
});
