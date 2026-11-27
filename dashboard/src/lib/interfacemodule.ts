import type { InterfaceModule, InterfaceModuleState } from './types.ts';

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

export interface ModuleCalls {
	put: (ref: string, module: number, current?: number) => Promise<InterfaceModuleState>;
	get: (ref: string) => Promise<InterfaceModule>;
}

/**
 * Writes the id and reads the view again. The PUT answers with the id alone: the list of known ids comes
 * from the GET, and the select needs it.
 */
export async function setAndRead(ref: string, module: number, current: number | undefined, calls: ModuleCalls): Promise<InterfaceModule> {
	await calls.put(ref, module, current);
	return calls.get(ref);
}
