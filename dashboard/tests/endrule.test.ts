import { describe, expect, it } from 'vitest';
import { END_RULES, defaultEndRule, timerSeconds, validTimer } from '../src/lib/endrule.ts';

describe('the rule offered first', () => {
	it('is the next block where the zone has a program', () => {
		expect(defaultEndRule('heating', true)).toBe('until_next');
		expect(defaultEndRule('hotwater', true)).toBe('until_next');
	});

	it('is "until I end it" where the zone has none, because the next block never comes', () => {
		expect(defaultEndRule('heating', false)).toBe('manual');
		expect(defaultEndRule('hotwater', false)).toBe('manual');
	});

	it('behaves as before when the daemon does not say', () => {
		expect(defaultEndRule('heating', undefined)).toBe('until_next');
		expect(defaultEndRule('hotwater', undefined)).toBe('manual');
	});
});

describe('end rules', () => {
	it('offer the four rule names of the API', () => {
		expect([...END_RULES].sort()).toEqual(['manual', 'timer', 'until_auto_change', 'until_next']);
	});

	it('accept a timer from one minute to seven days', () => {
		expect(timerSeconds(1, 30)).toBe(5400);
		expect(validTimer(59)).toBe(false);
		expect(validTimer(60)).toBe(true);
		expect(validTimer(604_800)).toBe(true);
		expect(validTimer(604_801)).toBe(false);
		expect(validTimer(Number.NaN)).toBe(false);
	});
});
