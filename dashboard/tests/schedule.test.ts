import { describe, expect, it } from 'vitest';
import {
	DAYS,
	HEATING_RANGE,
	HOT_WATER_RANGE,
	addBlock,
	addGroup,
	dayMask,
	defaultDraft,
	formatTime,
	fromView,
	localNow,
	nextChange,
	parseTime,
	programBytes,
	programInForce,
	removeBlock,
	removeGroup,
	sameSchedule,
	setBlock,
	toPrograms,
	toggleDay,
	validate,
	weekSegments,
	type Group
} from '../src/lib/schedule.ts';

const week = (): Group[] => [
	{
		key: 1,
		days: ['mon', 'tue', 'wed', 'thu', 'fri'],
		blocks: [
			{ minute: 0, setpointC: 17, enabled: true },
			{ minute: 390, setpointC: 20.5, enabled: true },
			{ minute: 1320, setpointC: 17, enabled: false }
		]
	},
	{ key: 2, days: ['sat', 'sun'], blocks: [{ minute: 0, setpointC: 19, enabled: true }] }
];

const codes = (groups: Group[], range = HEATING_RANGE) => validate(groups, range).map((i) => i.code);

describe('API view to draft and back', () => {
	it('keeps the programs of the API', () => {
		const view = {
			zone: 2,
			members: ['0201'],
			programs: [
				{ days: ['mon', 'tue', 'wed', 'thu', 'fri'], mask: 31, entries: [{ minute: 0, setpoint_c: 18, enabled: true }] },
				{ days: ['sat', 'sun'], mask: 96, entries: [{ minute: 0, setpoint_c: 20, enabled: false }] }
			]
		};
		const groups = fromView(view);
		expect(groups.map((g) => g.days)).toEqual([['mon', 'tue', 'wed', 'thu', 'fri'], ['sat', 'sun']]);
		expect(toPrograms(groups)).toEqual(view.programs.map(({ days, entries }) => ({ days, entries })));
	});

	it('computes the day mask like the daemon: bit 0 is Monday', () => {
		expect(dayMask(['mon'])).toBe(1);
		expect(dayMask(['sun'])).toBe(64);
		expect(dayMask([...DAYS])).toBe(0x7f);
	});

	it('detects a changed draft', () => {
		const a = week();
		expect(sameSchedule(a, week())).toBe(true);
		expect(sameSchedule(a, setBlock(week(), 1, 1, { setpointC: 21 }))).toBe(false);
	});
});

describe('validation follows the rules of the API', () => {
	it('accepts a complete week', () => {
		expect(validate(week(), HEATING_RANGE)).toEqual([]);
	});

	it('accepts the default draft for a room and for hot water', () => {
		expect(validate(defaultDraft(HEATING_RANGE, false), HEATING_RANGE)).toEqual([]);
		expect(validate(defaultDraft(HOT_WATER_RANGE, true), HOT_WATER_RANGE)).toEqual([]);
	});

	it('needs a program', () => {
		expect(codes([])).toEqual(['noGroups']);
	});

	it('needs all seven days and names the missing ones', () => {
		const groups = week().slice(0, 1);
		const issue = validate(groups, HEATING_RANGE).find((i) => i.code === 'daysMissing');
		expect(issue?.days).toEqual(['sat', 'sun']);
	});

	it('refuses a group with no day', () => {
		expect(codes([...week(), { key: 3, days: [], blocks: [{ minute: 0, setpointC: 18, enabled: true }] }])).toContain('groupNoDays');
	});

	it('needs the first block at 00:00', () => {
		const groups = week();
		groups[1]!.blocks[0]!.minute = 60;
		expect(codes(groups)).toContain('firstNotMidnight');
	});

	it('needs strictly increasing minutes up to 1439', () => {
		const same = week();
		same[0]!.blocks[1]!.minute = 0;
		expect(codes(same)).toContain('notIncreasing');
		const late = week();
		late[0]!.blocks[2]!.minute = 1440;
		expect(codes(late)).toContain('notIncreasing');
		const edge = week();
		edge[0]!.blocks[2]!.minute = 1439;
		expect(codes(edge)).toEqual([]);
	});

	it('keeps a room between 5 and 25 degrees, also in an off block', () => {
		for (const bad of [4.5, 25.5, Number.NaN]) {
			const groups = week();
			groups[0]!.blocks[2]!.setpointC = bad; // the block that is off
			expect(codes(groups)).toContain('temperatureRange');
		}
		const edges = week();
		edges[0]!.blocks[0]!.setpointC = 5;
		edges[0]!.blocks[1]!.setpointC = 25;
		expect(codes(edges)).toEqual([]);
	});

	it('keeps hot water between 30 and 65 degrees', () => {
		const groups = defaultDraft(HOT_WATER_RANGE, true);
		expect(codes(groups, HEATING_RANGE)).toContain('temperatureRange');
		groups[0]!.blocks[1]!.setpointC = 66;
		expect(codes(groups, HOT_WATER_RANGE)).toContain('temperatureRange');
	});

	it('allows two decimals and no more', () => {
		const groups = week();
		groups[1]!.blocks[0]!.setpointC = 19.25;
		expect(codes(groups)).toEqual([]);
		groups[1]!.blocks[0]!.setpointC = 19.255;
		expect(codes(groups)).toContain('temperatureDecimals');
	});

	it('refuses a program that the device cannot store', () => {
		const blocks = Array.from({ length: 30 }, (_, i) => ({ minute: i * 40, setpointC: 18, enabled: true }));
		expect(programBytes(blocks)).toBeGreaterThan(255);
		const groups: Group[] = [{ key: 1, days: [...DAYS], blocks }];
		expect(codes(groups)).toContain('tooLarge');
		expect(programBytes(blocks.slice(0, 20))).toBeLessThanOrEqual(255);
	});

	it('counts bytes like tado.EncodeScheduleDayProgram', () => {
		// mask field 2 bytes, then per entry: tag, length, and 0x08 minute 0x10 centi 0x18 flag
		expect(programBytes([{ minute: 0, setpointC: 18, enabled: true }])).toBe(2 + 1 + 1 + 7);
		expect(programBytes([{ minute: 390, setpointC: 20.5, enabled: true }])).toBe(2 + 1 + 1 + 8);
	});
});

describe('editing', () => {
	it('moves a day to the group that takes it and drops the group it empties', () => {
		const groups = toggleDay(week(), 2, 'fri');
		expect(groups.find((g) => g.key === 1)?.days).toEqual(['mon', 'tue', 'wed', 'thu']);
		expect(groups.find((g) => g.key === 2)?.days).toEqual(['fri', 'sat', 'sun']);
		const emptied = toggleDay(toggleDay(week(), 1, 'sat'), 1, 'sun');
		expect(emptied.map((g) => g.key)).toEqual([1]);
		expect(validate(emptied, HEATING_RANGE)).toEqual([]);
	});

	it('leaves a day unassigned when it is switched off, and says so', () => {
		const groups = toggleDay(week(), 1, 'mon');
		expect(codes(groups)).toEqual(['daysMissing']);
	});

	it('adds a group without a day, with the blocks of the first one', () => {
		const groups = addGroup(week());
		expect(groups).toHaveLength(3);
		expect(groups[2]?.days).toEqual([]);
		expect(groups[2]?.blocks).toEqual(week()[0]?.blocks);
		expect(removeGroup(groups, groups[2]!.key)).toHaveLength(2);
	});

	it('adds a block an hour after the last and stops at the end of the day', () => {
		const added = addBlock(week(), 2);
		expect(added[1]?.blocks.map((b) => b.minute)).toEqual([0, 60]);
		let groups = week();
		for (let i = 0; i < 40; i++) groups = addBlock(groups, 2);
		const minutes = groups[1]!.blocks.map((b) => b.minute);
		expect(minutes.at(-1)).toBeLessThanOrEqual(1439);
		expect(new Set(minutes).size).toBe(minutes.length);
	});

	it('never removes the block at midnight', () => {
		expect(removeBlock(week(), 1, 0)[0]?.blocks).toHaveLength(3);
		expect(removeBlock(week(), 1, 2)[0]?.blocks).toHaveLength(2);
	});

	it('keeps the blocks in time order when one moves', () => {
		const groups = setBlock(week(), 1, 1, { minute: 1400 });
		expect(groups[0]?.blocks.map((b) => b.minute)).toEqual([0, 1320, 1400]);
	});
});

describe('times', () => {
	it('parse and format a time of day', () => {
		expect(parseTime('06:30')).toBe(390);
		expect(parseTime('6:05')).toBe(365);
		expect(parseTime('23:59')).toBe(1439);
		expect(parseTime('24:00')).toBeNull();
		expect(parseTime('12:60')).toBeNull();
		expect(parseTime('noon')).toBeNull();
		expect(formatTime(390)).toBe('06:30');
		expect(formatTime(0)).toBe('00:00');
	});
});

describe('the week view', () => {
	it('covers every day from 00:00 to 24:00', () => {
		const segments = weekSegments(week());
		for (const day of DAYS) {
			const list = segments[day];
			expect(list[0]?.from).toBe(0);
			expect(list.at(-1)?.to).toBe(1440);
			for (let i = 1; i < list.length; i++) expect(list[i]?.from).toBe(list[i - 1]?.to);
		}
		expect(segments.mon[2]).toMatchObject({ from: 1320, to: 1440, enabled: false });
		expect(segments.sat).toHaveLength(1);
	});
});

describe('the next change', () => {
	it('reads the clock of the installation, not the clock of the browser', () => {
		// 2024-01-01 is a Monday. 23:30 UTC is 00:30 on Tuesday at UTC+1.
		const at = localNow(Date.UTC(2024, 0, 1, 23, 30), 60);
		expect(at).toEqual({ day: 'tue', minute: 30 });
	});

	it('finds the next block of today that changes the value', () => {
		expect(nextChange(week(), { day: 'mon', minute: 100 })).toEqual({ tomorrow: false, minute: 390, setpointC: 20.5, enabled: true });
		expect(nextChange(week(), { day: 'mon', minute: 400 })).toEqual({ tomorrow: false, minute: 1320, setpointC: 17, enabled: false });
	});

	it('looks at tomorrow when today holds no change, and skips a block with the same value', () => {
		expect(nextChange(week(), { day: 'mon', minute: 1400 })).toEqual({ tomorrow: true, minute: 0, setpointC: 17, enabled: true });
		const same: Group[] = [
			{ key: 1, days: [...DAYS], blocks: [{ minute: 0, setpointC: 18, enabled: true }, { minute: 600, setpointC: 18, enabled: true }, { minute: 1200, setpointC: 21, enabled: true }] }
		];
		expect(nextChange(same, { day: 'wed', minute: 300 })).toEqual({ tomorrow: false, minute: 1200, setpointC: 21, enabled: true });
	});

	it('is null when the program holds one value through tomorrow', () => {
		expect(nextChange(week(), { day: 'sat', minute: 600 })).toBeNull();
		expect(nextChange([], { day: 'sat', minute: 600 })).toBeNull();
	});
});

describe('the program in force', () => {
	it('is the away program only while the home is away and the zone has one', () => {
		expect(programInForce(false, true)).toBe('home');
		expect(programInForce(false, false)).toBe('home');
		expect(programInForce(true, false)).toBe('home');
		expect(programInForce(true, true)).toBe('away');
	});
});
