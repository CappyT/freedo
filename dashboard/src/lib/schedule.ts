// Logic of the weekly schedule editor. The rules mirror the daemon: tado.ValidateSchedule,
// validateScheduleRange and the size limit of one day program. The daemon stays the judge.

import type { ScheduleView } from './types.ts';

export const DAYS = ['mon', 'tue', 'wed', 'thu', 'fri', 'sat', 'sun'] as const;
export type Day = (typeof DAYS)[number];

/** The two programs of a zone: the one it follows at home, the one it follows while away. */
export type Mode = 'home' | 'away';

/**
 * The program a zone runs now. The away program is in force only while the whole home is
 * away and the zone has one; otherwise the home program is in force.
 */
export function programInForce(away: boolean, hasAwayProgram: boolean): Mode {
	return away && hasAwayProgram ? 'away' : 'home';
}

export interface Block {
	minute: number;
	setpointC: number;
	enabled: boolean;
}

/** Days that share one list of blocks: one day program of the API. */
export interface Group {
	key: number;
	days: Day[];
	blocks: Block[];
}

export interface Range {
	min: number;
	max: number;
	step: number;
}

export const HEATING_RANGE: Range = { min: 5, max: 25, step: 0.5 };
export const HOT_WATER_RANGE: Range = { min: 30, max: 65, step: 1 };

export const LAST_MINUTE = 1439;
/** One day program is at most 255 bytes on the device. */
export const MAX_PROGRAM_BYTES = 255;

export type IssueCode =
	| 'noGroups'
	| 'groupNoDays'
	| 'daysMissing'
	| 'noBlocks'
	| 'firstNotMidnight'
	| 'notIncreasing'
	| 'temperatureRange'
	| 'temperatureDecimals'
	| 'tooLarge';

export interface Issue {
	code: IssueCode;
	/** Index of the group in the list, when the issue belongs to one. */
	group?: number;
	/** Index of the block in the group. */
	block?: number;
	/** Days that no group covers. */
	days?: Day[];
}

export function fromView(view: ScheduleView): Group[] {
	return view.programs.map((program, key) => ({
		key,
		days: DAYS.filter((day) => program.days.includes(day)),
		blocks: program.entries.map((e) => ({ minute: e.minute, setpointC: e.setpoint_c, enabled: e.enabled }))
	}));
}

/** The request body of PUT /api/zones/{n}/schedule. */
export function toPrograms(groups: Group[]) {
	return groups.map((group) => ({
		days: sortDays(group.days),
		entries: group.blocks.map((b) => ({ minute: b.minute, setpoint_c: b.setpointC, enabled: b.enabled }))
	}));
}

/** A new schedule: the same three blocks every day. */
export function defaultDraft(range: Range, hotWater: boolean): Group[] {
	const high = hotWater ? 50 : 20;
	const low = hotWater ? range.min : 17;
	return [
		{
			key: 0,
			days: [...DAYS],
			blocks: [
				{ minute: 0, setpointC: low, enabled: !hotWater },
				{ minute: 6 * 60 + 30, setpointC: high, enabled: true },
				{ minute: 22 * 60, setpointC: low, enabled: !hotWater }
			]
		}
	];
}

export function sortDays(days: Day[]): Day[] {
	return DAYS.filter((d) => days.includes(d));
}

export function dayMask(days: Day[]): number {
	return DAYS.reduce((mask, day, bit) => (days.includes(day) ? mask | (1 << bit) : mask), 0);
}

function varintSize(value: number): number {
	let size = 1;
	for (let v = value; v >= 0x80; v = Math.floor(v / 0x80)) size++;
	return size;
}

/** Bytes of one day program as the daemon encodes it. */
export function programBytes(blocks: Block[]): number {
	let size = 2; // the day mask field: a tag and a one byte mask
	for (const b of blocks) {
		const entry = 1 + varintSize(b.minute) + 1 + varintSize(Math.round(b.setpointC * 100)) + 2;
		size += 1 + varintSize(entry) + entry;
	}
	return size;
}

export function validate(groups: Group[], range: Range): Issue[] {
	const issues: Issue[] = [];
	if (groups.length === 0) return [{ code: 'noGroups' }];
	const covered = new Set<Day>();
	groups.forEach((group, g) => {
		if (group.days.length === 0) issues.push({ code: 'groupNoDays', group: g });
		group.days.forEach((d) => covered.add(d));
		if (group.blocks.length === 0) {
			issues.push({ code: 'noBlocks', group: g });
			return;
		}
		if (group.blocks[0]?.minute !== 0) issues.push({ code: 'firstNotMidnight', group: g, block: 0 });
		let previous = -1;
		group.blocks.forEach((b, i) => {
			if (!Number.isInteger(b.minute) || b.minute < 0 || b.minute > LAST_MINUTE || b.minute <= previous) {
				issues.push({ code: 'notIncreasing', group: g, block: i });
			}
			previous = b.minute;
			// A block that is off still carries a temperature, in the same range.
			if (!(b.setpointC >= range.min && b.setpointC <= range.max)) {
				issues.push({ code: 'temperatureRange', group: g, block: i });
			} else if (Math.abs(b.setpointC * 100 - Math.round(b.setpointC * 100)) > 1e-6) {
				issues.push({ code: 'temperatureDecimals', group: g, block: i });
			}
		});
		if (programBytes(group.blocks) > MAX_PROGRAM_BYTES) issues.push({ code: 'tooLarge', group: g });
	});
	const missing = DAYS.filter((d) => !covered.has(d));
	if (missing.length > 0) issues.push({ code: 'daysMissing', days: missing });
	return issues;
}

let nextKey = 1000;
const freshKey = () => nextKey++;

/** Puts the day in the group and takes it out of every other group. A group left empty is dropped. */
export function toggleDay(groups: Group[], key: number, day: Day): Group[] {
	const target = groups.find((g) => g.key === key);
	if (!target) return groups;
	if (target.days.includes(day)) {
		return groups.map((g) => (g.key === key ? { ...g, days: g.days.filter((d) => d !== day) } : g));
	}
	return groups
		.map((g) => {
			if (g.key === key) return { ...g, days: sortDays([...g.days, day]) };
			return { ...g, days: g.days.filter((d) => d !== day) };
		})
		.filter((g) => g.key === key || g.days.length > 0);
}

/** A new group with no day. It starts from the blocks of the first group. */
export function addGroup(groups: Group[]): Group[] {
	const blocks = groups[0]?.blocks.map((b) => ({ ...b })) ?? [{ minute: 0, setpointC: 18, enabled: true }];
	return [...groups, { key: freshKey(), days: [], blocks }];
}

export function removeGroup(groups: Group[], key: number): Group[] {
	return groups.filter((g) => g.key !== key);
}

/** Adds a block an hour after the last one, or in the middle of the last gap. */
export function addBlock(groups: Group[], key: number): Group[] {
	return groups.map((g) => {
		if (g.key !== key) return g;
		const last = g.blocks.at(-1);
		if (!last || last.minute >= LAST_MINUTE) return g;
		const minute = last.minute + 60 <= LAST_MINUTE ? last.minute + 60 : Math.ceil((last.minute + LAST_MINUTE) / 2);
		return { ...g, blocks: [...g.blocks, { ...last, minute }] };
	});
}

export function removeBlock(groups: Group[], key: number, index: number): Group[] {
	return groups.map((g) => (g.key === key && index > 0 ? { ...g, blocks: g.blocks.filter((_, i) => i !== index) } : g));
}

/** Changes one block. A change of the time keeps the blocks in time order. */
export function setBlock(groups: Group[], key: number, index: number, patch: Partial<Block>): Group[] {
	return groups.map((g) => {
		if (g.key !== key) return g;
		const blocks = g.blocks.map((b, i) => (i === index ? { ...b, ...patch } : b));
		if (patch.minute !== undefined) blocks.sort((a, b) => a.minute - b.minute);
		return { ...g, blocks };
	});
}

export function parseTime(text: string): number | null {
	const m = /^(\d{1,2}):(\d{2})$/.exec(text.trim());
	if (!m) return null;
	const minute = Number(m[1]) * 60 + Number(m[2]);
	return Number(m[2]) < 60 && minute <= LAST_MINUTE ? minute : null;
}

export function formatTime(minute: number): string {
	return String(Math.floor(minute / 60)).padStart(2, '0') + ':' + String(minute % 60).padStart(2, '0');
}

export interface Segment {
	from: number;
	to: number;
	setpointC: number;
	enabled: boolean;
}

/** The blocks of each day as segments that cover the 24 hours. The day ends at minute 1440. */
export function weekSegments(groups: Group[]): Record<Day, Segment[]> {
	const week = Object.fromEntries(DAYS.map((d) => [d, [] as Segment[]])) as Record<Day, Segment[]>;
	for (const group of groups) {
		const segments = group.blocks.map((b, i) => ({
			from: b.minute,
			to: group.blocks[i + 1]?.minute ?? 1440,
			setpointC: b.setpointC,
			enabled: b.enabled
		}));
		for (const day of group.days) week[day] = segments;
	}
	return week;
}

/** Whether two drafts send the same request. */
export function sameSchedule(a: Group[], b: Group[]): boolean {
	return JSON.stringify(toPrograms(a)) === JSON.stringify(toPrograms(b));
}
