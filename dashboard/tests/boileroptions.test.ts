import { describe, expect, it } from 'vitest';
import {
	curveFlowAt,
	defaultFlowAt,
	energyIssues,
	energyOf,
	energyPatch,
	flowIssues,
	flowOf,
	flowPatch,
	parseNumber
} from '../src/lib/boileroptions.ts';
import type { BoilerOptions, FlowCurve } from '../src/lib/types.ts';

const options = (over: Partial<BoilerOptions> = {}): BoilerOptions => ({
	boiler_max_power_kw: 20.5,
	boiler_min_power_kw: 3,
	boiler_max_gas_m3h: 2.25,
	boiler_min_gas_m3h: 0.32,
	fuel_unit: 'm3',
	flow_limit_c: 56,
	flow_default_curve: true,
	flow_auto_adapt: false,
	flow_curves: {},
	outdoor_max_age_sec: 3600,
	flow_hysteresis_c: 1,
	flow_min_interval_sec: 1800,
	...over
});

describe('the default weather curve', () => {
	// docs/API.md: for flow_limit_c 56 C, 54 C at -5 C, 52 C at 3 C, 49 C at 10 C, 48 C from 16 C.
	it('gives the values the daemon documents', () => {
		expect([-5, 3, 10, 16, 22].map((x) => defaultFlowAt(56, x))).toEqual([54, 52, 49, 48, 48]);
	});

	it('gives the manual maximum on a cold day and never goes under 30 C', () => {
		expect(defaultFlowAt(56, -12)).toBe(56);
		expect(defaultFlowAt(32, 20)).toBe(30);
	});
});

describe('a curve of a circuit', () => {
	const curve: FlowCurve = {
		enabled: true,
		points: [
			{ outdoor_c: -5, flow_c: 55 },
			{ outdoor_c: 15, flow_c: 35 }
		],
		min_c: 38,
		max_c: 52
	};

	it('follows the line between its points and stays inside its bounds', () => {
		expect(curveFlowAt(curve, 5)).toBe(45);
		expect(curveFlowAt(curve, -10)).toBe(52);
		expect(curveFlowAt(curve, 20)).toBe(38);
	});
});

describe('the energy data in the form', () => {
	it('read the options and send only what changed', () => {
		const o = options();
		const form = energyOf(o);
		expect(form).toEqual({ fuel: 'm3', maxKW: '20.5', minKW: '3', maxFlow: '2.25', minFlow: '0.32' });
		expect(energyPatch(o, form)).toEqual({});
		expect(energyPatch(o, { ...form, maxKW: '24', minFlow: '0,3' })).toEqual({ boiler_max_power_kw: 24, boiler_min_gas_m3h: 0.3 });
	});

	it('write the decimals with the separator of the language', () => {
		const o = options();
		const form = energyOf(o, ',');
		expect(form.maxKW).toBe('20,5');
		expect(energyPatch(o, form, ',')).toEqual({});
	});

	it('send null for a field the person emptied', () => {
		const o = options();
		expect(energyPatch(o, { ...energyOf(o), maxFlow: '', minFlow: '' })).toEqual({ boiler_max_gas_m3h: null, boiler_min_gas_m3h: null });
	});

	it('show an unset minimum power as empty', () => {
		expect(energyOf(options({ boiler_max_power_kw: null, boiler_min_power_kw: 0 })).minKW).toBe('');
	});

	it('name what the daemon would refuse', () => {
		const ok = energyOf(options());
		expect(energyIssues(ok)).toEqual([]);
		expect(energyIssues({ ...ok, maxKW: '0' })).toContain('maxKW');
		expect(energyIssues({ ...ok, minKW: '25' })).toContain('kwOrder');
		expect(energyIssues({ ...ok, maxFlow: 'abc' })).toContain('maxFlow');
		expect(energyIssues({ ...ok, minFlow: '' })).toContain('flowPair');
		expect(energyIssues({ ...ok, maxFlow: '61' })).toContain('maxFlow');
	});
});

describe('the flow settings in the form', () => {
	it('read seconds as minutes and send only what changed', () => {
		const o = options();
		const form = flowOf(o, [1]);
		expect(form.outdoorMinutes).toBe(60);
		expect(form.intervalMinutes).toBe(30);
		expect(flowPatch(o, form)).toEqual({});
		expect(flowPatch(o, { ...form, limit: '50', intervalMinutes: 20 })).toEqual({ flow_limit_c: 50, flow_min_interval_sec: 1200 });
	});

	it('send no curve for a circuit that has none and keeps it off', () => {
		const o = options();
		expect(flowPatch(o, flowOf(o, [1, 2])).flow_curves).toBeUndefined();
	});

	it('send the curve of a circuit once it is switched on or changed', () => {
		const o = options();
		const form = flowOf(o, [1]);
		form.curves['1'] = { ...form.curves['1']!, enabled: true };
		expect(flowPatch(o, form).flow_curves).toEqual({
			'1': { enabled: true, points: [{ outdoor_c: -5, flow_c: 55 }, { outdoor_c: 15, flow_c: 35 }], min_c: 30, max_c: 60 }
		});
	});

	it('name what the daemon would refuse', () => {
		const form = flowOf(options(), [1]);
		expect(flowIssues(form)).toEqual([]);
		expect(flowIssues({ ...form, limit: '80' })).toContain('limit');
		expect(flowIssues({ ...form, limit: '' })).toContain('defaultNeedsLimit');
		expect(flowIssues({ ...form, hysteresisC: 0 })).toContain('hysteresis');
		const bad = flowOf(options(), [1]);
		bad.curves['1'] = { ...bad.curves['1']!, enabled: true, mildOutdoor: -5 };
		expect(flowIssues(bad)).toContain('curve');
	});
});

describe('a number the person typed', () => {
	it('takes a comma, reads an empty field as null and a word as undefined', () => {
		expect(parseNumber(' 2,63 ')).toBe(2.63);
		expect(parseNumber('')).toBeNull();
		expect(parseNumber('due')).toBeUndefined();
	});
});
