import { describe, expect, it } from 'vitest';
import { burnerHours, canSetFlow, flowDrivers, flowInRange } from '../src/lib/boiler.ts';
import type { FlowCircuit } from '../src/lib/types.ts';

describe('flow limit drivers', () => {
	const circuit = (over: Partial<FlowCircuit> = {}): FlowCircuit => ({
		id: 1,
		curve: false,
		defaultCurve: false,
		adapt: false,
		computedC: null,
		servedC: 55,
		minC: 30,
		maxC: 75,
		changedAt: null,
		fetched: true,
		adaptedC: null,
		adaptReason: null,
		reductionC: null,
		...over
	});

	it('is manual when nothing else sets it', () => {
		expect(flowDrivers(circuit())).toEqual(['manual']);
		expect(canSetFlow(circuit())).toBe(true);
	});

	it('names the curve, the default curve and the adaptation, and refuses a manual value', () => {
		expect(flowDrivers(circuit({ curve: true }))).toEqual(['curve']);
		expect(flowDrivers(circuit({ defaultCurve: true }))).toEqual(['defaultCurve']);
		expect(flowDrivers(circuit({ adapt: true }))).toEqual(['adapt']);
		expect(flowDrivers(circuit({ curve: true, adapt: true }))).toEqual(['curve', 'adapt']);
		for (const over of [{ curve: true }, { defaultCurve: true }, { adapt: true }]) {
			expect(canSetFlow(circuit(over))).toBe(false);
		}
	});

	it('keeps a manual value inside the bounds of the circuit', () => {
		const c = circuit({ minC: 30, maxC: 70 });
		expect(flowInRange(c, 30)).toBe(true);
		expect(flowInRange(c, 70)).toBe(true);
		expect(flowInRange(c, 29)).toBe(false);
		expect(flowInRange(c, 71)).toBe(false);
		expect(flowInRange(c, Number.NaN)).toBe(false);
	});

	it('shows burner minutes as hours', () => {
		expect(burnerHours(90)).toBe(1.5);
		expect(burnerHours(null)).toBeNull();
	});
});
