import type { Boiler, FlowCircuit } from './types.ts';

export type FlowDriver = 'manual' | 'curve' | 'defaultCurve' | 'adapt';

/**
 * What sets the flow limit of a circuit. A weather curve and the adaptation can act
 * together: the curve gives the ceiling and the adaptation trims below it.
 */
export function flowDrivers(c: FlowCircuit): FlowDriver[] {
	const drivers: FlowDriver[] = [];
	if (c.curve) drivers.push('curve');
	else if (c.defaultCurve) drivers.push('defaultCurve');
	if (c.adapt) drivers.push('adapt');
	return drivers.length > 0 ? drivers : ['manual'];
}

/** The daemon refuses a manual limit while a curve or the adaptation owns the circuit. */
export function canSetFlow(c: FlowCircuit): boolean {
	return flowDrivers(c).includes('manual');
}

/** A whole degree inside the bounds the circuit accepts. */
export function flowInRange(c: FlowCircuit, value: number): boolean {
	return Number.isFinite(value) && value >= c.minC && value <= c.maxC;
}

export function burnerHours(minutes: number | null): number | null {
	return minutes === null ? null : Math.round((minutes / 60) * 10) / 10;
}

export type BurnerState = 'on' | 'off' | 'unknown';

/**
 * Whether the burner runs. Only the burner flag of the boiler tells it. The call for heat of the
 * heating circuit does not: the burner also runs for hot water while no room asks for heat, and
 * it can be off while a room asks. A boiler that reports no flag has an unknown state.
 */
export function burnerState(boiler: Pick<Boiler, 'Present' | 'Active' | 'Mon'>): BurnerState {
	if (!boiler.Present || typeof boiler.Mon?.boiler_flag !== 'number') return 'unknown';
	return boiler.Active ? 'on' : 'off';
}
