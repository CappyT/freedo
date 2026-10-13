import { describe, expect, it } from 'vitest';
import { burnerHours, burnerState, canSetFlow, flowDrivers, flowInRange } from '../src/lib/boiler.ts';
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

describe('the burner state', () => {
	const boiler = (flag: number | null | undefined, active: boolean, present = true) => ({
		Present: present,
		Active: active,
		Mon: flag === undefined ? null : { boiler_flag: flag }
	});

	it('follows the burner flag of the boiler', () => {
		expect(burnerState(boiler(1, true))).toBe('on');
		expect(burnerState(boiler(0, false))).toBe('off');
	});

	it('is on for hot water, while the heating circuit does not call for heat', () => {
		// The values of a plant that heats hot water: Active true, HeatActive false.
		expect(burnerState({ ...boiler(1, true), HeatActive: false } as Parameters<typeof burnerState>[0])).toBe('on');
	});

	it('is unknown while the boiler reports no flag or has not reported', () => {
		expect(burnerState(boiler(undefined, false))).toBe('unknown');
		expect(burnerState(boiler(null, false))).toBe('unknown');
		expect(burnerState({ Present: true, Active: false })).toBe('unknown');
		expect(burnerState(boiler(1, true, false))).toBe('unknown');
	});
});
