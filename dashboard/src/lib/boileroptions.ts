import type { BoilerOptions, BoilerPatch, FlowCurve, FuelUnit } from './types.ts';

// The limits of the daemon: docs/API.md, "Installation options" and "Flow limit".
export const POWER_KW = { max: 500 };
export const FUEL_FLOW = { max: 60 };
export const FLOW_C = { min: 30, max: 75 };
/** The limit a curve or the adaptation falls back to while flow_limit_c is unset. */
export const FALLBACK_FLOW_C = 48;
export const FUEL_UNITS: FuelUnit[] = ['m3', 'l', 'kg'];

/** Go rounds a half away from zero, and the preview must give the value the daemon serves. */
function round(value: number): number {
	return Math.sign(value) * Math.round(Math.abs(value));
}

const clamp = (value: number, lo: number, hi: number) => Math.min(hi, Math.max(lo, value));

/** The limit the default curve gives under the manual maximum limitC. Mirrors tado.DefaultFlowAt. */
export function defaultFlowAt(limitC: number, outdoorC: number): number {
	const reduction = clamp(round(3.2 + 0.33 * outdoorC), 0, 8);
	return Math.max(limitC - reduction, FLOW_C.min);
}

/** The limit a curve of a circuit gives. Mirrors tado.FlowCurve.FlowAt. */
export function curveFlowAt(curve: FlowCurve, outdoorC: number): number {
	const [p, q] = curve.points;
	const slope = (q.flow_c - p.flow_c) / (q.outdoor_c - p.outdoor_c);
	return clamp(round(p.flow_c + slope * (outdoorC - p.outdoor_c)), curve.min_c, curve.max_c);
}

/**
 * A number the person typed: the value, null for an empty field, or undefined for a text that is
 * not a number. A comma counts as the decimal separator.
 */
export function parseNumber(text: string): number | null | undefined {
	const clean = text.trim().replace(',', '.');
	if (clean === '') return null;
	const value = Number(clean);
	return Number.isFinite(value) ? value : undefined;
}

/** A value for a text field, with the decimal separator of the language: 20.5 gives "20,5" in Italian. */
const text = (value: number | null, decimal = '.') => (value === null ? '' : String(value).replace('.', decimal));

/** The energy data as the form holds them: text, so that a field can be empty. */
export interface EnergyForm {
	fuel: FuelUnit;
	maxKW: string;
	minKW: string;
	maxFlow: string;
	minFlow: string;
}

export function energyOf(o: BoilerOptions, decimal = '.'): EnergyForm {
	return {
		fuel: o.fuel_unit,
		maxKW: text(o.boiler_max_power_kw, decimal),
		// The daemon reports 0 for an unset minimum power, and 0 is a valid minimum.
		minKW: o.boiler_max_power_kw === null && o.boiler_min_power_kw === 0 ? '' : text(o.boiler_min_power_kw, decimal),
		maxFlow: text(o.boiler_max_gas_m3h, decimal),
		minFlow: text(o.boiler_min_gas_m3h, decimal)
	};
}

export type EnergyIssue = 'maxKW' | 'minKW' | 'kwOrder' | 'maxFlow' | 'minFlow' | 'flowOrder' | 'flowPair';

/** What the daemon would refuse: a maximum above 0 and within the limit, a minimum below it. */
export function energyIssues(f: EnergyForm): EnergyIssue[] {
	const issues: EnergyIssue[] = [];
	const maxKW = parseNumber(f.maxKW);
	const minKW = parseNumber(f.minKW);
	const maxFlow = parseNumber(f.maxFlow);
	const minFlow = parseNumber(f.minFlow);
	if (maxKW === undefined || (maxKW !== null && !(maxKW > 0 && maxKW <= POWER_KW.max))) issues.push('maxKW');
	if (minKW === undefined || (minKW !== null && !(minKW >= 0 && minKW <= POWER_KW.max))) issues.push('minKW');
	if (typeof maxKW === 'number' && typeof minKW === 'number' && minKW >= maxKW) issues.push('kwOrder');
	if (maxFlow === undefined || (maxFlow !== null && !(maxFlow > 0 && maxFlow <= FUEL_FLOW.max))) issues.push('maxFlow');
	if (minFlow === undefined || (minFlow !== null && !(minFlow >= 0 && minFlow <= FUEL_FLOW.max))) issues.push('minFlow');
	if (typeof maxFlow === 'number' && typeof minFlow === 'number' && minFlow >= maxFlow) issues.push('flowOrder');
	// The fuel counter needs both flows: one alone counts nothing.
	if ((maxFlow === null) !== (minFlow === null)) issues.push('flowPair');
	return issues;
}

/** The fields of PUT /api/install/boiler that differ from what the daemon holds. */
export function energyPatch(o: BoilerOptions, f: EnergyForm, decimal = '.'): BoilerPatch {
	const now = energyOf(o, decimal);
	const patch: BoilerPatch = {};
	if (f.fuel !== now.fuel) patch.fuel_unit = f.fuel;
	if (f.maxKW.trim() !== now.maxKW) patch.boiler_max_power_kw = parseNumber(f.maxKW) ?? null;
	if (f.minKW.trim() !== now.minKW) patch.boiler_min_power_kw = parseNumber(f.minKW) ?? null;
	if (f.maxFlow.trim() !== now.maxFlow) patch.boiler_max_gas_m3h = parseNumber(f.maxFlow) ?? null;
	if (f.minFlow.trim() !== now.minFlow) patch.boiler_min_gas_m3h = parseNumber(f.minFlow) ?? null;
	return patch;
}

/** The curve of one circuit as the form holds it. */
export interface CurveForm {
	enabled: boolean;
	coldOutdoor: number;
	coldFlow: number;
	mildOutdoor: number;
	mildFlow: number;
	minC: number;
	maxC: number;
}

/** A curve for a circuit that has none: 55 C at -5 C outdoors, 35 C at 15 C, off. */
export const NEW_CURVE: CurveForm = { enabled: false, coldOutdoor: -5, coldFlow: 55, mildOutdoor: 15, mildFlow: 35, minC: 30, maxC: 60 };

export function curveOf(curve: FlowCurve | undefined): CurveForm {
	if (!curve) return { ...NEW_CURVE };
	const [p, q] = curve.points;
	return { enabled: curve.enabled, coldOutdoor: p.outdoor_c, coldFlow: p.flow_c, mildOutdoor: q.outdoor_c, mildFlow: q.flow_c, minC: curve.min_c, maxC: curve.max_c };
}

export function curveFrom(f: CurveForm): FlowCurve {
	return {
		enabled: f.enabled,
		points: [
			{ outdoor_c: f.coldOutdoor, flow_c: f.coldFlow },
			{ outdoor_c: f.mildOutdoor, flow_c: f.mildFlow }
		],
		min_c: f.minC,
		max_c: f.maxC
	};
}

/** The flow limit, the curves and the adaptation as the form holds them. Timing is in minutes. */
export interface FlowForm {
	limit: string;
	defaultCurve: boolean;
	autoAdapt: boolean;
	/** Keyed by circuit id. */
	curves: Record<string, CurveForm>;
	outdoorMinutes: number;
	hysteresisC: number;
	intervalMinutes: number;
}

export function flowOf(o: BoilerOptions, circuitIds: number[]): FlowForm {
	return {
		limit: text(o.flow_limit_c),
		defaultCurve: o.flow_default_curve,
		autoAdapt: o.flow_auto_adapt,
		curves: Object.fromEntries(circuitIds.map((id) => [String(id), curveOf(o.flow_curves[String(id)])])),
		outdoorMinutes: Math.round(o.outdoor_max_age_sec / 60),
		hysteresisC: o.flow_hysteresis_c,
		intervalMinutes: Math.round(o.flow_min_interval_sec / 60)
	};
}

export type FlowIssue = 'limit' | 'defaultNeedsLimit' | 'curve' | 'outdoorMinutes' | 'hysteresis' | 'interval';

const flowValue = (v: number) => Number.isInteger(v) && v >= FLOW_C.min && v <= FLOW_C.max;
const positive = (v: number) => Number.isInteger(v) && v > 0;

/** The circuit ids whose enabled curve the daemon would refuse. */
export function badCurves(f: FlowForm): string[] {
	return Object.entries(f.curves)
		.filter(([, c]) => c.enabled)
		.filter(
			([, c]) =>
				!Number.isFinite(c.coldOutdoor) ||
				!Number.isFinite(c.mildOutdoor) ||
				c.coldOutdoor === c.mildOutdoor ||
				![c.coldFlow, c.mildFlow, c.minC, c.maxC].every(flowValue) ||
				c.minC > c.maxC
		)
		.map(([id]) => id);
}

export function flowIssues(f: FlowForm): FlowIssue[] {
	const issues: FlowIssue[] = [];
	const limit = parseNumber(f.limit);
	if (limit === undefined || (limit !== null && !flowValue(limit))) issues.push('limit');
	if (f.defaultCurve && limit === null) issues.push('defaultNeedsLimit');
	if (badCurves(f).length > 0) issues.push('curve');
	if (!positive(f.outdoorMinutes)) issues.push('outdoorMinutes');
	if (!positive(f.hysteresisC)) issues.push('hysteresis');
	if (!positive(f.intervalMinutes)) issues.push('interval');
	return issues;
}

const sameCurve = (a: FlowCurve | undefined, b: FlowCurve) => a !== undefined && JSON.stringify(a) === JSON.stringify(b);

/** The fields that differ from what the daemon holds. A curve left off that the daemon never held is not sent. */
export function flowPatch(o: BoilerOptions, f: FlowForm): BoilerPatch {
	const patch: BoilerPatch = {};
	const limit = parseNumber(f.limit) ?? null;
	if (limit !== o.flow_limit_c) patch.flow_limit_c = limit;
	if (f.defaultCurve !== o.flow_default_curve) patch.flow_default_curve = f.defaultCurve;
	if (f.autoAdapt !== o.flow_auto_adapt) patch.flow_auto_adapt = f.autoAdapt;
	const curves: Record<string, FlowCurve> = {};
	for (const [id, form] of Object.entries(f.curves)) {
		const held = o.flow_curves[id];
		const next = curveFrom(form);
		if (!held && !form.enabled) continue;
		if (!sameCurve(held, next)) curves[id] = next;
	}
	if (Object.keys(curves).length > 0) patch.flow_curves = curves;
	if (f.outdoorMinutes * 60 !== o.outdoor_max_age_sec) patch.outdoor_max_age_sec = f.outdoorMinutes * 60;
	if (f.hysteresisC !== o.flow_hysteresis_c) patch.flow_hysteresis_c = f.hysteresisC;
	if (f.intervalMinutes * 60 !== o.flow_min_interval_sec) patch.flow_min_interval_sec = f.intervalMinutes * 60;
	return patch;
}

/** The points of a preview line from coldest to mildest outdoor temperature, one per degree. */
export function previewLine(at: (outdoorC: number) => number, from = -15, to = 25): { outdoorC: number; flowC: number }[] {
	const points = [];
	for (let x = from; x <= to; x++) points.push({ outdoorC: x, flowC: at(x) });
	return points;
}
