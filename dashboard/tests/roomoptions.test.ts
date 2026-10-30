import { describe, expect, it } from 'vitest';
import { optionIssues, optionsOf, optionsPatch, parseOffset } from '../src/lib/roomoptions.ts';
import type { InstallZone } from '../src/lib/types.ts';

const zone = (over: Partial<InstallZone> = {}): InstallZone => ({
	number: 2,
	name: 'Bath',
	kind: 'heating',
	members: [],
	circuit: 1,
	reporter: null,
	measuring_device: null,
	open_window_detection: true,
	open_window_duration_sec: 900,
	early_start: false,
	device_overlay_end: 'until_next',
	device_overlay_end_sec: null,
	offset_mode: 'config',
	owner: 'device',
	away_temperature_c: null,
	...over
});

describe('the options of a room in the form', () => {
	it('read seconds as minutes, and no away temperature as 5 C', () => {
		expect(optionsOf(zone())).toEqual({
			windowDetection: true,
			windowMinutes: 15,
			awayC: 5,
			endRule: 'until_next',
			endMinutes: 60,
			earlyStart: false,
			offsetMode: 'config'
		});
		expect(optionsOf(zone({ device_overlay_end: 'timer', device_overlay_end_sec: 5400, away_temperature_c: 16.5 }))).toMatchObject({
			endRule: 'timer',
			endMinutes: 90,
			awayC: 16.5
		});
	});
});

describe('the patch of the room options', () => {
	it('is empty when nothing changed', () => {
		expect(optionsPatch(zone(), optionsOf(zone()))).toEqual({});
	});

	it('holds the changed fields only, in the units of the daemon', () => {
		const draft = { ...optionsOf(zone()), windowDetection: false, windowMinutes: 20, awayC: 16, earlyStart: true };
		expect(optionsPatch(zone(), draft)).toEqual({
			open_window_detection: false,
			open_window_duration_sec: 1200,
			away_temperature_c: 16,
			early_start: true
		});
	});

	it('sends the duration with the rule timer alone', () => {
		expect(optionsPatch(zone(), { ...optionsOf(zone()), endRule: 'timer', endMinutes: 90 })).toEqual({
			device_overlay_end: 'timer',
			device_overlay_end_sec: 5400
		});
		expect(optionsPatch(zone(), { ...optionsOf(zone()), endRule: 'manual', endMinutes: 90 })).toEqual({ device_overlay_end: 'manual' });
		const timed = zone({ device_overlay_end: 'timer', device_overlay_end_sec: 5400 });
		expect(optionsPatch(timed, { ...optionsOf(timed), endMinutes: 30 })).toEqual({ device_overlay_end: 'timer', device_overlay_end_sec: 1800 });
	});

});

describe('the offset mode of a room', () => {
	it('reads config when the daemon sends none', () => {
		expect(optionsOf(zone({ offset_mode: null })).offsetMode).toBe('config');
		expect(optionsOf(zone({ offset_mode: 'dbg2' })).offsetMode).toBe('dbg2');
	});

	it('sends the mode when it changes, and nothing when it stays', () => {
		const dbg2 = zone({ offset_mode: 'dbg2' });
		expect(optionsPatch(zone(), { ...optionsOf(zone()), offsetMode: 'dbg2' })).toEqual({ offset_mode: 'dbg2' });
		expect(optionsPatch(dbg2, { ...optionsOf(dbg2), offsetMode: 'config' })).toEqual({ offset_mode: 'config' });
		expect(optionsPatch(dbg2, optionsOf(dbg2))).toEqual({});
	});

	it('never sends the owner: the select of the room temperature source writes it', () => {
		const coordinator = zone({ owner: 'coordinator' });
		expect(optionsPatch(coordinator, optionsOf(coordinator))).toEqual({});
	});
});

describe('the checks of the room options', () => {
	const ok = optionsOf(zone());

	it('pass the values of the daemon', () => {
		expect(optionIssues(ok, '')).toEqual([]);
		expect(optionIssues({ ...ok, awayC: 25, windowMinutes: 1092, endRule: 'timer', endMinutes: 10080 }, '-0.7')).toEqual([]);
	});

	it('keep the ranges of the daemon', () => {
		expect(optionIssues({ ...ok, windowMinutes: 0 }, '')).toEqual(['window']);
		expect(optionIssues({ ...ok, windowMinutes: 1093 }, '')).toEqual(['window']);
		expect(optionIssues({ ...ok, awayC: 4.9 }, '')).toEqual(['away']);
		expect(optionIssues({ ...ok, awayC: 25.5 }, '')).toEqual(['away']);
		expect(optionIssues({ ...ok, awayC: 16.123 }, '')).toEqual(['away']);
		expect(optionIssues({ ...ok, endRule: 'timer', endMinutes: 0 }, '')).toEqual(['endTimer']);
		expect(optionIssues({ ...ok, endRule: 'timer', endMinutes: 10081 }, '')).toEqual(['endTimer']);
		expect(optionIssues({ ...ok, endRule: 'manual', endMinutes: 0 }, '')).toEqual([]);
	});

	it('take an offset from -10 to 10 C with two decimals at most', () => {
		expect(parseOffset('')).toBeNull();
		expect(parseOffset(' -0,7 ')).toBe(-0.7);
		expect(parseOffset('10')).toBe(10);
		expect(parseOffset('10.01')).toBeUndefined();
		expect(parseOffset('0.123')).toBeUndefined();
		expect(parseOffset('warm')).toBeUndefined();
		expect(optionIssues(ok, '11')).toEqual(['offset']);
	});
});
