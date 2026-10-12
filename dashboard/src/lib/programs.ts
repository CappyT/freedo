import type { PresetSchedules, PresetZoneSchedules } from './types.ts';

type InForce = PresetZoneSchedules['inForce'];

export interface ZoneChange {
	zone: number;
	name: string;
	kind: PresetZoneSchedules['kind'];
	from: InForce;
	to: InForce;
}

const sameBlock = (a: InForce, b: InForce) =>
	a === b || (a !== null && b !== null && a.off === b.off && (a.off || a.setpointC === b.setpointC));

/**
 * The zones whose block in force moves when target becomes the active preset: the zones the
 * daemon writes at once on the activation. Two off blocks are the same block whatever their
 * stored temperature.
 */
export function changedZones(active: PresetSchedules, target: PresetSchedules): ZoneChange[] {
	const before = new Map(active.zones.map((z) => [z.zone, z.inForce]));
	return target.zones
		.filter((z) => !sameBlock(before.get(z.zone) ?? null, z.inForce))
		.map((z) => ({ zone: z.zone, name: z.name, kind: z.kind, from: before.get(z.zone) ?? null, to: z.inForce }));
}
