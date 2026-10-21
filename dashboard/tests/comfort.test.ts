import { describe, expect, it } from 'vitest';
import { comfortOf, humidityVerdict, tempVerdict } from '../src/lib/comfort.ts';

describe('the air comfort of a room', () => {
	it('takes 20 to 24 C as comfortable', () => {
		expect([19.9, 20, 24, 24.1].map(tempVerdict)).toEqual(['cold', 'ok', 'ok', 'warm']);
	});

	it('takes 30 to 60 % as comfortable and 70 % up as a mould risk', () => {
		expect([29, 30, 60, 61, 69, 70].map(humidityVerdict)).toEqual(['dry', 'ok', 'ok', 'humid', 'humid', 'mould']);
	});

	it('names the humidity before the temperature', () => {
		expect(comfortOf(21.5, 45)?.verdict).toBe('pleasant');
		expect(comfortOf(18, 45)?.verdict).toBe('cold');
		expect(comfortOf(18, 72)?.verdict).toBe('mould');
		expect(comfortOf(25, 25)?.verdict).toBe('dry');
	});

	it('judges a room with one value, and none with no value', () => {
		expect(comfortOf(21, null)).toEqual({ temp: 'ok', humidity: null, verdict: 'pleasant' });
		expect(comfortOf(null, 65)?.verdict).toBe('humid');
		expect(comfortOf(null, null)).toBeNull();
	});
});
