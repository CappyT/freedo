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

export interface Written {
	/** What the PUT answered: the id is stored. */
	written: InterfaceModuleState;
	/** The view the GET read after it, null when that read failed. */
	view: InterfaceModule | null;
}

/**
 * Writes the id and reads the view again. The PUT answers with the id alone: the list of known ids comes
 * from the GET, and the select needs it. A failed PUT throws. A failed GET does not: the id is stored, and
 * the caller shows it.
 */
export async function setAndRead(ref: string, module: number, current: number | undefined, calls: ModuleCalls): Promise<Written> {
	const written = await calls.put(ref, module, current);
	try {
		return { written, view: await calls.get(ref) };
	} catch {
		return { written, view: null };
	}
}
