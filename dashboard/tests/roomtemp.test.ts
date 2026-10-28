import { describe, expect, it } from 'vitest';
import { roomTempState, readingUnix } from '../src/lib/roomtemp.ts';
import type { ExternalTemp } from '../src/lib/types.ts';

const none: ExternalTemp = {
	tempC: null, at: null, ageSec: null, source: null, stale: false, inUse: false, learnInUse: false,
	deviceC: null, wantedC: null, shiftC: 0, offsetC: 0, refused: null
};
const fresh: ExternalTemp = { ...none, tempC: 20.4, at: '2026-01-01T10:00:00Z', ageSec: 120, source: 'Kitchen', inUse: true };
const old: ExternalTemp = { ...fresh, ageSec: 4000, stale: true, inUse: false };
const refused: ExternalTemp = { ...none, refused: { source: 'Kitchen', at: '2026-01-01T10:00:00Z', ageSec: 30 } };

describe('roomTempState', () => {
	it('is none for hot water and for the method None with no refusal', () => {
		expect(roomTempState('none', undefined)).toBe('none');
		expect(roomTempState('shift', null)).toBe('none');
		expect(roomTempState('none', none)).toBe('none');
	});

	it('is refused for the method None with a refused push, also on a daemon that sends no field', () => {
		expect(roomTempState('none', refused)).toBe('refused');
		const older = { ...none };
		delete older.refused;
		expect(roomTempState('none', older)).toBe('none');
	});

	it('ignores a refusal once the method takes a reading', () => {
		for (const method of ['shift', 'calibration', 'coordinator'] as const) {
			expect(roomTempState(method, refused)).toBe('no_reading');
		}
	});

	it('is active for a fresh reading in each method that takes one', () => {
		for (const method of ['shift', 'calibration', 'coordinator'] as const) {
			expect(roomTempState(method, fresh)).toBe('active');
		}
	});

	it('is stale for a reading older than the limit', () => {
		expect(roomTempState('shift', old)).toBe('stale');
		expect(roomTempState('coordinator', { ...old, inUse: false, stale: false })).toBe('stale');
	});

	it('is no_reading when the method needs one and none arrived', () => {
		expect(roomTempState('shift', none)).toBe('no_reading');
		expect(roomTempState('calibration', none)).toBe('no_reading');
	});
});

describe('readingUnix', () => {
	it('moves back by the age on the clock of the daemon', () => {
		expect(readingUnix(120, 1_000_000_000)).toBe(1_000_000 - 120);
	});
});
