import { describe, expect, it } from 'vitest';
import { typedNumber } from '../src/lib/numeric.ts';
import { HEATING_RANGE, setBlock, validate, type Group } from '../src/lib/schedule.ts';

// What a number field reads after each key. "21." is not a complete number: the browser gives NaN.
const keys = [2, 21, Number.NaN, 21.5];

describe('a number typed into a field', () => {
	it('keeps 21.5 when the digits come one by one', () => {
		let stored = 17;
		for (const read of keys) stored = typedNumber(read, stored, false);
		expect(stored).toBe(21.5);
	});

	it('keeps the last whole value at the incomplete step instead of clearing the field', () => {
		const seen: number[] = [];
		let stored = 17;
		for (const read of keys) {
			stored = typedNumber(read, stored, false);
			seen.push(stored);
		}
		expect(seen).toEqual([2, 21, 21, 21.5]);
	});

	it('stores an empty or broken field when the person leaves it, so that validation names it', () => {
		expect(typedNumber(Number.NaN, 21, true)).toBeNaN();
		expect(typedNumber(21.5, 21, true)).toBe(21.5);
	});

	it('reaches the schedule draft as 21.5, and the draft passes validation', () => {
		let groups: Group[] = [
			{ key: 1, days: ['mon', 'tue', 'wed', 'thu', 'fri', 'sat', 'sun'], blocks: [{ minute: 0, setpointC: 17, enabled: true }] }
		];
		for (const read of keys) {
			const stored = typedNumber(read, groups[0]!.blocks[0]!.setpointC, false);
			groups = setBlock(groups, 1, 0, { setpointC: stored });
			// The draft never holds NaN while the person types, so it is never a wrong value.
			expect(Number.isNaN(groups[0]!.blocks[0]!.setpointC)).toBe(false);
		}
		expect(groups[0]!.blocks[0]!.setpointC).toBe(21.5);
		expect(validate(groups, HEATING_RANGE)).toEqual([]);
	});
});
