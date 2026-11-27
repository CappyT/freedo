import type { InterfaceModule } from './types.ts';

export interface FamilyGroup {
	family: string;
	ids: number[];
}

/** The ids the daemon accepts, grouped by boiler bus family. Families and ids are in ascending order. */
export function groupByFamily(known: InterfaceModule['known']): FamilyGroup[] {
	const groups = new Map<string, number[]>();
	for (const { module, family } of known) groups.set(family, [...(groups.get(family) ?? []), module]);
	return [...groups]
		.map(([family, ids]) => ({ family, ids: ids.sort((a, b) => a - b) }))
		.sort((a, b) => a.family.localeCompare(b.family));
}
