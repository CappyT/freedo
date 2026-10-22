import { describe, expect, it } from 'vitest';
import { boilerFaults, burnerHours, burnerState, canSetFlow, flowDrivers, flowInRange, outsideTemp } from '../src/lib/boiler.ts';
import type { Boiler, FlowCircuit } from '../src/lib/types.ts';

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

describe('the fault codes of the boiler', () => {
	it('are unknown before the boiler view and before the first fault report', () => {
		expect(boilerFaults(null)).toEqual({ state: 'unknown', codes: [] });
		expect(boilerFaults({ FaultCodes: null, FaultCode: 0 })).toEqual({ state: 'unknown', codes: [] });
	});

	it('are none after a report with no code', () => {
		expect(boilerFaults({ FaultCodes: [], FaultCode: 0 })).toEqual({ state: 'none', codes: [] });
	});

	it('are the numbers the boiler reports, with no meaning added', () => {
		expect(boilerFaults({ FaultCodes: [133, 28], FaultCode: 133 })).toEqual({ state: 'fault', codes: [133, 28] });
		expect(boilerFaults({ FaultCodes: null, FaultCode: 133 })).toEqual({ state: 'fault', codes: [133] });
	});
});

describe('the outdoor temperature', () => {
	const boiler = (OutsideC: number | null, outdoor: Boiler['flowLimit']['outdoor']) =>
		({ OutsideC, flowLimit: { outdoor } }) as Pick<Boiler, 'OutsideC' | 'flowLimit'>;

	it('takes the sensor of the boiler first', () => {
		expect(outsideTemp(boiler(8.5, { tempC: 24.7, ageSec: 60, stale: false }))).toBe(8.5);
	});

	it('takes the fresh reading from Home Assistant when the boiler has no sensor', () => {
		expect(outsideTemp(boiler(null, { tempC: 24.7, ageSec: 60, stale: false }))).toBe(24.7);
	});

	it('is unknown with a stale reading or none', () => {
		expect(outsideTemp(boiler(null, { tempC: 24.7, ageSec: 7200, stale: true }))).toBeNull();
		expect(outsideTemp(boiler(null, null))).toBeNull();
	});
});
