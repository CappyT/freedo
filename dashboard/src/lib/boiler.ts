import type { FlowCircuit } from './types.ts';

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
