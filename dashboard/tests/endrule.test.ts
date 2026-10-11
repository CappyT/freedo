import { describe, expect, it } from 'vitest';
import { END_RULES, timerSeconds, validTimer } from '../src/lib/endrule.ts';

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
