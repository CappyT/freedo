import { describe, expect, it } from 'vitest';
import { COORDINATOR_SOURCE, electedDevice, readingUnix, roomTempState, sourceNote, sourceValue, sourceWrites } from '../src/lib/roomtemp.ts';
import type { ExternalTemp, InstallZone } from '../src/lib/types.ts';

const none: ExternalTemp = {
	tempC: null, at: null, ageSec: null, source: null, stale: false, inUse: false, deviceC: null, refused: null
};
const fresh: ExternalTemp = { ...none, tempC: 20.4, at: '2026-01-01T10:00:00Z', ageSec: 120, source: 'Kitchen', inUse: true };
const old: ExternalTemp = { ...fresh, ageSec: 4000, stale: true, inUse: false };
const refused: ExternalTemp = { ...none, refused: { source: 'Kitchen', at: '2026-01-01T10:00:00Z', ageSec: 30 } };

describe('roomTempState', () => {
	it('is none for hot water and for a device-owned zone with no refusal', () => {
		expect(roomTempState('device', undefined)).toBe('none');
		expect(roomTempState(null, fresh)).toBe('none');
		expect(roomTempState('device', null)).toBe('none');
		expect(roomTempState('device', none)).toBe('none');
	});

	it('is refused for a device-owned zone with a refused push, also on a daemon that sends no field', () => {
		expect(roomTempState('device', refused)).toBe('refused');
		const older = { ...none };
		delete older.refused;
		expect(roomTempState('device', older)).toBe('none');
	});

	it('ignores a refusal once the thermometer owns the zone', () => {
		expect(roomTempState('coordinator', refused)).toBe('no_reading');
	});

	it('is active for a fresh reading', () => {
		expect(roomTempState('coordinator', fresh)).toBe('active');
	});

	it('is stale for a reading older than the limit', () => {
		expect(roomTempState('coordinator', old)).toBe('stale');
		expect(roomTempState('coordinator', { ...old, inUse: false, stale: false })).toBe('stale');
	});

	it('is no_reading when the thermometer owns the zone and none arrived', () => {
		expect(roomTempState('coordinator', none)).toBe('no_reading');
	});
});

describe('readingUnix', () => {
	it('moves back by the age on the clock of the daemon', () => {
		expect(readingUnix(120, 1_000_000_000)).toBe(1_000_000 - 120);
	});
});

type Source = Pick<InstallZone, 'members' | 'reporter' | 'measuring_device' | 'owner'>;
const room = (over: Partial<Source> = {}): Source => ({
	members: ['aa', 'bb'],
	reporter: null,
	measuring_device: null,
	owner: 'device',
	...over
});

describe('the elected device', () => {
	it('is the stored measuring device first', () => {
		expect(electedDevice(room({ measuring_device: 'bb', reporter: 'aa' }))).toBe('bb');
	});

	it('is the reporter when no measuring device is stored', () => {
		expect(electedDevice(room({ reporter: 'aa' }))).toBe('aa');
	});

	it('is the only member when nothing else is set', () => {
		expect(electedDevice(room({ members: ['aa'] }))).toBe('aa');
	});

	it('is none with several members and no election, and with no member', () => {
		expect(electedDevice(room())).toBeNull();
		expect(electedDevice(room({ members: [] }))).toBeNull();
	});
});

describe('the option of the select', () => {
	it('is the thermometer when it owns the zone, whatever the devices are', () => {
		expect(sourceValue(room({ owner: 'coordinator', measuring_device: 'bb' }))).toBe(COORDINATOR_SOURCE);
	});

	it('is the elected device when a device owns the zone', () => {
		expect(sourceValue(room({ measuring_device: 'bb' }))).toBe('bb');
		expect(sourceValue(room({ reporter: 'aa' }))).toBe('aa');
		expect(sourceValue(room({ members: ['aa'] }))).toBe('aa');
	});

	it('is empty with several members and no election', () => {
		expect(sourceValue(room())).toBe('');
	});
});

describe('the writes of a choice', () => {
	it('sets the owner alone for the thermometer, and keeps the measuring device', () => {
		expect(sourceWrites(room({ measuring_device: 'aa' }), COORDINATOR_SOURCE)).toEqual({ owner: 'coordinator' });
		expect(sourceWrites(room({ owner: 'coordinator' }), COORDINATOR_SOURCE)).toEqual({});
	});

	it('sets the measuring device for a device', () => {
		expect(sourceWrites(room(), 'bb')).toEqual({ measuring: 'bb' });
		expect(sourceWrites(room({ measuring_device: 'aa' }), 'bb')).toEqual({ measuring: 'bb' });
	});

	it('stores an elected device that no one stored yet', () => {
		expect(sourceWrites(room({ members: ['aa'] }), 'aa')).toEqual({ measuring: 'aa' });
	});

	it('gives the zone back to a device when the thermometer owned it', () => {
		expect(sourceWrites(room({ owner: 'coordinator', measuring_device: 'aa' }), 'aa')).toEqual({ owner: 'device' });
		expect(sourceWrites(room({ owner: 'coordinator', measuring_device: 'aa' }), 'bb')).toEqual({ measuring: 'bb', owner: 'device' });
	});

	it('writes nothing for a device that already measures a device-owned zone', () => {
		expect(sourceWrites(room({ measuring_device: 'aa' }), 'aa')).toEqual({});
	});
});

describe('the note under the select', () => {
	it('asks for a choice with several members and no election', () => {
		expect(sourceNote(room(), undefined)).toBe('choose');
	});

	it('explains the device when one is elected or the room has one member or none', () => {
		expect(sourceNote(room({ measuring_device: 'aa' }), fresh)).toBe('device');
		expect(sourceNote(room({ reporter: 'bb' }), none)).toBe('device');
		expect(sourceNote(room({ members: ['aa'] }), undefined)).toBe('device');
		expect(sourceNote(room({ members: [] }), undefined)).toBe('device');
	});

	it('gives the three states of the thermometer', () => {
		const ha = room({ owner: 'coordinator' });
		expect(sourceNote(ha, fresh)).toBe('fresh');
		expect(sourceNote(ha, old)).toBe('stale');
		expect(sourceNote(ha, none)).toBe('no_reading');
	});

	it('says no reading when the daemon sends no external temperature, and ignores a refused push', () => {
		const ha = room({ owner: 'coordinator' });
		expect(sourceNote(ha, undefined)).toBe('no_reading');
		expect(sourceNote(ha, refused)).toBe('no_reading');
	});

	it('says thermostat when the elected device is a wall thermostat, and device for a valve', () => {
		expect(sourceNote(room({ measuring_device: 'aa' }), fresh, 'thermostat')).toBe('thermostat');
		expect(sourceNote(room({ measuring_device: 'aa' }), fresh, 'valve')).toBe('device');
		expect(sourceNote(room({ measuring_device: 'aa' }), fresh)).toBe('device');
		expect(sourceNote(room({ owner: 'coordinator' }), fresh, 'thermostat')).toBe('fresh');
		expect(sourceNote(room(), undefined, 'thermostat')).toBe('choose');
	});

	it('does not ask for a choice while the thermometer owns the zone', () => {
		expect(sourceNote(room({ owner: 'coordinator' }), fresh)).toBe('fresh');
	});
});
