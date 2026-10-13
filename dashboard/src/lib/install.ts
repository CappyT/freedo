import type { DeviceKind, Health, RemovalEffect, ZoneKind } from './types.ts';

// The daemon refuses a zone number or a circuit id above 31. Hot water is zone 0 on the radio.
const MAX_ID = 31;
const HOT_WATER_ZONE = 0;

/** The number of a new zone: 0 for hot water, else the lowest free number from 1. Null when none is free. */
export function nextZoneNumber(used: number[], kind: ZoneKind): number | null {
	if (kind === 'hotwater') return used.includes(HOT_WATER_ZONE) ? null : HOT_WATER_ZONE;
	return lowestFree(used);
}

/** The id of a new circuit: the lowest free id from 1. Null when none is free. */
export function nextCircuitId(used: number[]): number | null {
	return lowestFree(used);
}

function lowestFree(used: number[]): number | null {
	for (let n = 1; n <= MAX_ID; n++) if (!used.includes(n)) return n;
	return null;
}

/** A wire EUI as the daemon prints it: 16 hexadecimal digits. */
export function wireEui(text: string): string | null {
	const clean = text.trim().toLowerCase();
	return /^[0-9a-f]{16}$/.test(clean) ? clean : null;
}

export type MoveStep =
	| { op: 'claim'; zone: number | null }
	| { op: 'leave'; zone: number }
	| { op: 'join'; zone: number };

/**
 * The writes that move a device from one zone to another, in an order the daemon accepts.
 * The daemon refuses a device that claims a zone it is not a member of, so a claim on the zone
 * the device leaves goes first and comes back on the zone it joins. Null means no zone.
 */
export function movePlan(claim: number | null, from: number | null, to: number | null): MoveStep[] {
	if (from === to) return [];
	const steps: MoveStep[] = [];
	const claimed = claim !== null && claim === from;
	if (claimed) steps.push({ op: 'claim', zone: null });
	if (from !== null) steps.push({ op: 'leave', zone: from });
	if (to !== null) steps.push({ op: 'join', zone: to });
	if (claimed && to !== null) steps.push({ op: 'claim', zone: to });
	return steps;
}

export type EffectKey =
	| 'remove.effect.member'
	| 'remove.effect.hot_water'
	| 'remove.effect.last_member'
	| 'remove.effect.reporter'
	| 'remove.effect.measuring_device'
	| 'remove.effect.heat_source';

/** The text key of a removal effect and the name it falls on: a zone or a circuit. */
export function describeEffect(
	effect: RemovalEffect,
	zoneName: (n: number) => string,
	circuitName: (id: number) => string
): { key: EffectKey; name: string } {
	const name =
		effect.circuit !== undefined ? circuitName(effect.circuit) : effect.zone !== undefined ? zoneName(effect.zone) : '';
	return { key: `remove.effect.${effect.effect}`, name };
}

export type StepState = 'done' | 'todo';

export interface SetupState {
	radio: StepState;
	/** Why the radio step is open: no live radio, or a radio that is not calibrated. */
	radioProblem: 'none' | 'noRadio' | 'notCalibrated';
	timezone: StepState;
	rooms: StepState;
	transmit: StepState;
	/** Why the transmit step is open: the other coordinator is on air, or the daemon runs in dry run. */
	transmitProblem: 'none' | 'bridge' | 'dryRun';
	devices: StepState;
	circuits: StepState;
}

/**
 * The state of each step of the first setup, from what the daemon reports. A heating room with no
 * circuit keeps the circuit step open, because its valves then get no heat source.
 */
export function setupState(health: Health | null, zones: { kind: ZoneKind; circuit?: number }[]): SetupState {
	const heating = zones.filter((z) => z.kind === 'heating');
	const radioProblem = health?.mode !== 'live' ? 'noRadio' : health.calibrated === false ? 'notCalibrated' : 'none';
	const transmitProblem = health?.bridgePresent ? 'bridge' : health?.transmitAllowed ? 'none' : 'dryRun';
	const done = (ok: boolean): StepState => (ok ? 'done' : 'todo');
	return {
		radio: done(radioProblem === 'none'),
		radioProblem,
		timezone: done(health?.timezone !== undefined && !health.timezone.fallback),
		rooms: done(heating.length > 0),
		transmit: done(transmitProblem === 'none'),
		transmitProblem,
		devices: done((health?.devices.length ?? 0) > 0),
		circuits: done(heating.length > 0 && heating.every((z) => z.circuit !== undefined))
	};
}

/**
 * The short address of the device that a view names. The engine views name a device by its
 * serial number, and by its short address while it has none.
 */
export function shortOf(devices: { short: string; device: string }[], ref: string): string {
	return devices.find((d) => d.short === ref || d.device === ref)?.short ?? '';
}

/** What the daemon reports of a device when the pair command goes out. */
export interface NodeMark {
	serial: string;
	rxCounter: number;
}

export function nodeMark(node: { Serial: string; RxCounter: number } | undefined): NodeMark {
	return { serial: node?.Serial ?? '', rxCounter: node?.RxCounter ?? 0 };
}

/**
 * A device joined after a pair command when the daemon read a frame of it under the network key
 * since: the frame counter of the device moved, or the daemon learned its serial number. The answer
 * of the pair command only says that it is queued. The time of the last session does not tell: the
 * daemon answers a repeated request of the device with the session it holds, and the time stays.
 */
export function joinedSince(node: { Serial: string; RxCounter: number } | undefined, before: NodeMark): boolean {
	if (!node) return false;
	return node.RxCounter !== before.rxCounter || (before.serial === '' && node.Serial !== '');
}

/**
 * A device that the pair window heard is paired when the daemon heard it again after its last
 * request to join, and a request of it carried the session of the daemon. The daemon keeps the
 * device in the list of the window after the device joins and after the window closes, so the list
 * alone does not tell a waiting device from a paired one. An acknowledgement of the pair command
 * also moves the last seen time, so that time alone is no proof.
 */
export function joinerPaired(
	joiner: { lastSeen: string },
	node: { LastSeenUnix: number; SessionAdopted: boolean } | undefined
): boolean {
	const heard = Date.parse(joiner.lastSeen);
	if (!node || Number.isNaN(heard)) return false;
	return node.SessionAdopted && node.LastSeenUnix > Math.floor(heard / 1000);
}

/** Seconds as m:ss, for the time a pair window stays open. */
export function countdown(seconds: number): string {
	const left = Math.max(0, Math.round(seconds));
	return `${Math.floor(left / 60)}:${String(left % 60).padStart(2, '0')}`;
}

export interface RoomRef {
	number: number;
	kind: ZoneKind;
}

/**
 * Whether a device can join a room, given the rooms it is in now. A device belongs to one heated
 * room. The wall thermostat is the exception the plant needs: it can be in the hot water room and
 * in one heated room at once. So a heated room takes a device in no room, or a wall thermostat that
 * is in the hot water room only. The hot water room takes a wall thermostat only.
 */
export function canJoin(kind: DeviceKind, memberOf: RoomRef[], room: RoomRef): boolean {
	if (memberOf.some((z) => z.number === room.number)) return false;
	if (room.kind === 'hotwater') return kind === 'thermostat';
	if (kind === 'bridge') return false;
	if (memberOf.length === 0) return true;
	return kind === 'thermostat' && memberOf.every((z) => z.kind === 'hotwater');
}

/**
 * The rooms a device can move to from the room `from`, or from no room when `from` is null. The
 * device leaves `from`, so that room does not count against it. A device of the hot water room has
 * no target: taking the thermostat out of hot water is the only move there.
 */
export function moveTargets<R extends RoomRef>(kind: DeviceKind, memberOf: RoomRef[], from: number | null, rooms: R[]): R[] {
	const current = memberOf.find((z) => z.number === from);
	if (current?.kind === 'hotwater') return [];
	const rest = memberOf.filter((z) => z.number !== from);
	return rooms.filter((room) => room.number !== from && (from === null || room.kind === 'heating') && canJoin(kind, rest, room));
}
