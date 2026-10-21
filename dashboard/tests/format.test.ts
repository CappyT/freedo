import { describe, expect, it } from 'vitest';
import { fuel, number } from '../src/lib/format.ts';

describe('a number in a table column', () => {
	it('keeps the same decimals on every row, so the column lines up', () => {
		expect(fuel(1.6, 'm3', 'it-IT')).toBe('1,60 m³');
		expect(fuel(2, 'm3', 'it-IT')).toBe('2,00 m³');
		expect(number(20, 'it-IT', 1, 1)).toBe('20,0');
	});

	it('drops trailing zeros when no minimum is asked', () => {
		expect(number(20, 'it-IT', 1)).toBe('20');
	});
});
