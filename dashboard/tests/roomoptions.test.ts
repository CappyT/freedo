import { describe, expect, it } from 'vitest';
import { correctionOf, optionIssues, optionsOf, optionsPatch, parseOffset, withCorrection } from '../src/lib/roomoptions.ts';
import type { Correction, RoomOptions } from '../src/lib/roomoptions.ts';
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
	setpoint_offset_c: null,
	auto_offset: false,
	auto_offset_interval_sec: 86400,
	owner: 'device',
	away_temperature_c: null,
	...over
});

describe('the options of a room in the form', () => {
	it('read seconds as minutes and hours, and no away temperature as 5 C', () => {
		expect(optionsOf(zone())).toEqual({
			windowDetection: true,
			windowMinutes: 15,
			awayC: 5,
			endRule: 'until_next',
			endMinutes: 60,
			earlyStart: false,
			correction: 'none',
			offsetMode: 'config',
			calibrationHours: 24
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

describe('the temperature correction of a room', () => {
	const none = zone();
	const dbg2 = zone({ offset_mode: 'dbg2' });
	const shift = zone({ offset_mode: 'setpoint', setpoint_offset_c: -0.5 });
	const calibration = zone({ auto_offset: true });
	const coordinator = zone({ owner: 'coordinator' });
	const to = (from: InstallZone, correction: Correction, over: Partial<RoomOptions> = {}) =>
		optionsPatch(from, { ...withCorrection(optionsOf(from), correction), ...over });

	it('reads the correction from the offset mode, the valve calibration and the owner', () => {
		expect(correctionOf(none)).toBe('none');
		expect(correctionOf(dbg2)).toBe('none');
		expect(correctionOf(shift)).toBe('shift');
		expect(correctionOf(calibration)).toBe('calibration');
		expect(correctionOf(zone({ offset_mode: 'dbg2', auto_offset: true }))).toBe('calibration');
		expect(correctionOf(coordinator)).toBe('coordinator');
		expect(correctionOf(zone({ owner: null }))).toBe('none');
	});

	it('forces mode setpoint in the shift and keeps config or dbg2 in the others', () => {
		expect(withCorrection(optionsOf(none), 'shift').offsetMode).toBe('setpoint');
		expect(withCorrection(optionsOf(shift), 'none').offsetMode).toBe('config');
		expect(withCorrection(optionsOf(shift), 'coordinator').offsetMode).toBe('config');
		expect(withCorrection(optionsOf(dbg2), 'calibration').offsetMode).toBe('dbg2');
		expect(withCorrection(optionsOf(dbg2), 'coordinator').offsetMode).toBe('dbg2');
	});

	it('is no change when the room keeps its correction', () => {
		for (const z of [none, dbg2, shift, calibration, coordinator]) expect(to(z, correctionOf(z))).toEqual({});
	});

	it('sets the new correction and clears the old one in the same patch', () => {
		expect(to(none, 'shift')).toEqual({ offset_mode: 'setpoint' });
		expect(to(none, 'calibration', { calibrationHours: 48 })).toEqual({ auto_offset: true, auto_offset_interval_sec: 172800 });
		expect(to(none, 'coordinator')).toEqual({ owner: 'coordinator' });
		expect(to(dbg2, 'coordinator')).toEqual({ owner: 'coordinator' });
		expect(to(shift, 'none')).toEqual({ offset_mode: 'config' });
		expect(to(shift, 'calibration')).toEqual({ offset_mode: 'config', auto_offset: true });
		expect(to(shift, 'coordinator')).toEqual({ offset_mode: 'config', owner: 'coordinator' });
		expect(to(calibration, 'none')).toEqual({ auto_offset: false });
		expect(to(calibration, 'shift')).toEqual({ offset_mode: 'setpoint', auto_offset: false });
		expect(to(calibration, 'coordinator')).toEqual({ auto_offset: false, owner: 'coordinator' });
		expect(to(coordinator, 'none')).toEqual({ owner: 'device' });
		expect(to(coordinator, 'shift')).toEqual({ offset_mode: 'setpoint', owner: 'device' });
		expect(to(coordinator, 'calibration')).toEqual({ auto_offset: true, owner: 'device' });
	});

	it('sends the offset mode of the valve in the corrections that write the valve', () => {
		expect(to(none, 'none', { offsetMode: 'dbg2' })).toEqual({ offset_mode: 'dbg2' });
		expect(to(coordinator, 'coordinator', { offsetMode: 'dbg2' })).toEqual({ offset_mode: 'dbg2' });
		expect(to(shift, 'coordinator', { offsetMode: 'dbg2' })).toEqual({ offset_mode: 'dbg2', owner: 'coordinator' });
	});

	it('never sends mode setpoint outside the shift, nor another mode in it', () => {
		expect(optionsPatch(none, { ...optionsOf(none), correction: 'calibration', offsetMode: 'setpoint' })).toEqual({ auto_offset: true });
		expect(optionsPatch(none, { ...optionsOf(none), correction: 'shift', offsetMode: 'dbg2' })).toEqual({ offset_mode: 'setpoint' });
	});

	it('sends the calibration interval in the valve calibration alone', () => {
		expect(to(calibration, 'calibration', { calibrationHours: 12 })).toEqual({ auto_offset_interval_sec: 43200 });
		expect(to(none, 'coordinator', { calibrationHours: 12 })).toEqual({ owner: 'coordinator' });
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
		expect(optionIssues({ ...ok, correction: 'calibration', calibrationHours: 169 }, '')).toEqual(['calibrationHours']);
		expect(optionIssues({ ...ok, correction: 'coordinator', calibrationHours: 169 }, '')).toEqual([]);
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
