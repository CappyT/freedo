import type { GasMeterEntry } from './types.ts';

export interface MeterRow extends GasMeterEntry {
	/** What the meter counted since the reading before, null for the first reading. */
	meterDelta: number | null;
	/** What the estimate counted in the same time. */
	estimateDelta: number | null;
}

/** The readings newest first, each with what the meter and the estimate counted since the one before. */
export function meterRows(entries: GasMeterEntry[]): MeterRow[] {
	return entries
		.map((e, i) => {
			const before = entries[i - 1];
			return {
				...e,
				meterDelta: before ? e.reading - before.reading : null,
				estimateDelta: before ? e.estimate - before.estimate : null
			};
		})
		.reverse();
}

/**
 * How far the estimate is from the meter, in percent of the meter: positive when the estimate
 * counts more. The factor is meter over estimate, so 0.958 gives 4.4.
 */
export function estimateBias(factor: number): number {
	return (1 / factor - 1) * 100;
}

const pad = (n: number) => String(n).padStart(2, '0');

/** The value of a datetime-local field for a time, in the time zone of the browser. */
export function localInput(date: Date): string {
	return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

/** The RFC3339 time of a datetime-local value, or null for an empty or broken field. */
export function fromLocalInput(value: string): string | null {
	if (value === '') return null;
	const date = new Date(value);
	return Number.isNaN(date.getTime()) ? null : date.toISOString().replace(/\.\d{3}Z$/, 'Z');
}

export type ReadingIssue = 'number' | 'below' | 'future';

/** What the daemon would refuse. A reading below the last needs a replaced meter. */
export function readingIssue(reading: number | null | undefined, at: string | null, last: number | null, replaced: boolean, now = new Date()): ReadingIssue | null {
	if (reading === null || reading === undefined || reading < 0 || reading > 1e9) return 'number';
	if (at !== null && new Date(at).getTime() > now.getTime()) return 'future';
	if (!replaced && last !== null && reading < last) return 'below';
	return null;
}
