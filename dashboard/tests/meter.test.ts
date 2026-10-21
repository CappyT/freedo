import { describe, expect, it } from 'vitest';
import { estimateBias, fromLocalInput, localInput, meterRows, readingIssue } from '../src/lib/meter.ts';

describe('the gas meter readings', () => {
	const entries = [
		{ at: '2026-10-13T12:59:00Z', reading: 8786.236, estimate: 0.392 },
		{ at: '2026-10-21T13:52:00Z', reading: 8793.31, estimate: 7.774 }
	];

	it('list the newest first, with what the meter and the estimate counted', () => {
		const rows = meterRows(entries);
		expect(rows.map((r) => r.reading)).toEqual([8793.31, 8786.236]);
		expect(rows[0]!.meterDelta).toBeCloseTo(7.074, 6);
		expect(rows[0]!.estimateDelta).toBeCloseTo(7.382, 6);
		expect(rows[1]!.meterDelta).toBeNull();
	});

	it('say how far the estimate is from the meter', () => {
		expect(estimateBias(0.958)).toBeCloseTo(4.38, 2);
		expect(estimateBias(1.1)).toBeCloseTo(-9.09, 2);
	});
});

describe('the time of a reading', () => {
	it('goes through the datetime-local field in the time zone of the browser', () => {
		const date = new Date(2026, 9, 21, 15, 52);
		expect(localInput(date)).toBe('2026-10-21T15:52');
		expect(fromLocalInput(localInput(date))).toBe(new Date(2026, 9, 21, 15, 52).toISOString().replace('.000Z', 'Z'));
		expect(fromLocalInput('')).toBeNull();
	});
});

describe('a reading the person typed', () => {
	const now = new Date('2026-10-21T14:00:00Z');

	it('is refused below the last reading unless the meter was replaced', () => {
		expect(readingIssue(8793, null, 8793.31, false, now)).toBe('below');
		expect(readingIssue(12, null, 8793.31, true, now)).toBeNull();
		expect(readingIssue(8800, null, 8793.31, false, now)).toBeNull();
	});

	it('is refused when it is not a number or lies in the future', () => {
		expect(readingIssue(undefined, null, null, false, now)).toBe('number');
		expect(readingIssue(-1, null, null, false, now)).toBe('number');
		expect(readingIssue(1, '2026-10-21T15:00:00Z', null, false, now)).toBe('future');
	});
});
