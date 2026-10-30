import type { DeviceKind, ExternalTemp, InstallZone, ZoneOwner } from './types.ts';

/**
 * What the room screen says about the room temperature that Home Assistant pushes:
 * - none: nothing to say, a device of the room measures it;
 * - refused: Home Assistant pushes, and the zone belongs to a device, which drops the push;
 * - active: a fresh reading drives the room;
 * - stale: the last reading is too old to drive it;
 * - no_reading: the zone waits for a reading and none arrived.
 */
export type RoomTempState = 'none' | 'refused' | 'active' | 'stale' | 'no_reading';

/** The state of a heating zone. A zone with no externalTemp or no owner, as hot water, is `none`. */
export function roomTempState(owner: ZoneOwner | null, ext: ExternalTemp | null | undefined): RoomTempState {
	if (!ext || owner === null) return 'none';
	// A refusal outlives a change of owner until the next push, so it counts only while a device owns the zone.
	if (owner === 'device') return ext.refused ? 'refused' : 'none';
	if (ext.inUse) return 'active';
	if (ext.tempC !== null) return 'stale';
	return 'no_reading';
}

/** The Unix time of a reading from its age on the clock of the daemon, for the relative time format. */
export function readingUnix(ageSec: number, nowMs: number = Date.now()): number {
	return Math.round(nowMs / 1000 - ageSec);
}

type SourceZone = Pick<InstallZone, 'members' | 'reporter' | 'measuring_device' | 'owner'>;

/** The value of the option of the Home Assistant thermometer. A device option holds the EUI of the device. */
export const COORDINATOR_SOURCE = 'coordinator';

/** The device that measures the room: the one stored, else the reporter, else the only member. The daemon elects the same way. */
export function electedDevice(zone: SourceZone): string | null {
	if (zone.measuring_device) return zone.measuring_device;
	if (zone.reporter) return zone.reporter;
	return zone.members.length === 1 ? (zone.members[0] ?? null) : null;
}

/** The option the select shows: the thermometer, the elected device, or '' for no choice. */
export function sourceValue(zone: SourceZone): string {
	return zone.owner === 'coordinator' ? COORDINATOR_SOURCE : (electedDevice(zone) ?? '');
}

/** The writes that make a choice of the select. Each field is absent when the zone already holds it. */
export interface SourceWrites {
	measuring?: string;
	owner?: ZoneOwner;
}

export function sourceWrites(zone: SourceZone, choice: string): SourceWrites {
	if (choice === COORDINATOR_SOURCE) return zone.owner === 'coordinator' ? {} : { owner: 'coordinator' };
	const writes: SourceWrites = {};
	if (choice !== zone.measuring_device) writes.measuring = choice;
	if (zone.owner === 'coordinator') writes.owner = 'device';
	return writes;
}

/**
 * The note under the select:
 * - choose: a device owns the zone, several devices are members and none is elected;
 * - thermostat: a device owns the zone and the elected device is a wall thermostat;
 * - device: a device owns the zone;
 * - fresh, no_reading, stale: the thermometer owns the zone, by the state of its reading.
 */
export type SourceNote = 'choose' | 'thermostat' | 'device' | 'fresh' | 'no_reading' | 'stale';

/** `electedKind` is the kind of the elected device, null when the device is unknown. */
export function sourceNote(zone: SourceZone, ext: ExternalTemp | null | undefined, electedKind: DeviceKind | null = null): SourceNote {
	if (zone.owner !== 'coordinator') {
		if (electedDevice(zone) === null && zone.members.length > 1) return 'choose';
		return electedKind === 'thermostat' ? 'thermostat' : 'device';
	}
	const state = roomTempState('coordinator', ext);
	return state === 'active' ? 'fresh' : state === 'stale' ? 'stale' : 'no_reading';
}
