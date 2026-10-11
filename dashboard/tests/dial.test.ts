import { describe, expect, it } from 'vitest';
import { SPAN, START, arcPath, pointToValue, polar, snap, valueToAngle } from '../src/lib/dial.ts';

const C = { x: 100, y: 100 };
const at = (deg: number) => polar(C.x, C.y, 78, deg);

describe('dial geometry', () => {
	it('puts the minimum at the bottom left and the maximum at the bottom right', () => {
		expect(valueToAngle(5, 5, 25)).toBe(START);
		expect(valueToAngle(25, 5, 25)).toBe(START + SPAN);
		const min = at(valueToAngle(5, 5, 25));
		const max = at(valueToAngle(25, 5, 25));
		expect(min.x).toBeLessThan(C.x);
		expect(max.x).toBeGreaterThan(C.x);
		expect(min.y).toBeGreaterThan(C.y);
		expect(max.y).toBeGreaterThan(C.y);
	});

	it('puts the middle of the range at the top', () => {
		const top = at(valueToAngle(15, 5, 25));
		expect(top.x).toBeCloseTo(C.x, 6);
		expect(top.y).toBeLessThan(C.y);
	});

	it('clamps a value outside the range', () => {
		expect(valueToAngle(2, 5, 25)).toBe(START);
		expect(valueToAngle(99, 5, 25)).toBe(START + SPAN);
	});

	it('reads the value back from a point on the arc', () => {
		for (const value of [5, 12.5, 15, 20.5, 25]) {
			const point = at(valueToAngle(value, 5, 25));
			expect(pointToValue(point, C, 5, 25, 0.5)).toBe(value);
		}
	});

	it('snaps a point in the gap to the nearer end', () => {
		expect(pointToValue({ x: 60, y: 190 }, C, 5, 25, 0.5)).toBe(5);
		expect(pointToValue({ x: 140, y: 190 }, C, 5, 25, 0.5)).toBe(25);
	});

	it('rounds to the step and stays in the range', () => {
		expect(snap(20.26, 5, 25, 0.5)).toBe(20.5);
		expect(snap(20.24, 5, 25, 0.5)).toBe(20);
		expect(snap(51.2, 30, 65, 1)).toBe(51);
		expect(snap(99, 5, 25, 0.5)).toBe(25);
		expect(snap(-3, 5, 25, 0.5)).toBe(5);
	});

	it('draws a large arc past half a turn', () => {
		expect(arcPath(100, 100, 78, START, START + SPAN)).toMatch(/ 0 1 1 /);
		expect(arcPath(100, 100, 78, START, START + 90)).toMatch(/ 0 0 1 /);
	});
});
