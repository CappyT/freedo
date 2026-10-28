import type { Correction } from './roomoptions.ts';
import type { ExternalTemp } from './types.ts';

/**
 * What the room screen says about the room temperature that Home Assistant pushes:
 * - none: nothing to say, the valves regulate on their own sensor;
 * - refused: Home Assistant pushes, and the method None drops the push;
 * - active: a fresh reading drives the room;
 * - stale: the last reading is too old to drive it;
 * - no_reading: the method needs a reading and none arrived.
 */
export type RoomTempState = 'none' | 'refused' | 'active' | 'stale' | 'no_reading';

/** The state of a heating zone. A zone with no externalTemp, as hot water, is `none`. */
export function roomTempState(method: Correction, ext: ExternalTemp | null | undefined): RoomTempState {
	if (!ext) return 'none';
	// A refusal outlives a change of method until the next push, so it counts only while the method is None.
	if (method === 'none') return ext.refused ? 'refused' : 'none';
	if (ext.inUse) return 'active';
	if (ext.tempC !== null) return 'stale';
	return 'no_reading';
}

/** The Unix time of a reading from its age on the clock of the daemon, for the relative time format. */
export function readingUnix(ageSec: number, nowMs: number = Date.now()): number {
	return Math.round(nowMs / 1000 - ageSec);
}
