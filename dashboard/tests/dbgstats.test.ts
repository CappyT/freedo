import { describe, expect, it } from 'vitest';
import { deltaValue, isTicks, lastRead, refusal, statValue } from '../src/lib/dbgstats.ts';
import type { DebugStats } from '../src/lib/types.ts';

const counters: DebugStats = {
	f: 11,
	pending: false,
	code: '2.05',
	received_at: '2026-11-07T10:00:00Z',
	stats: [
		{ tag: '023b', name: 'rx_crc_fail', value: 4 },
		{ tag: '023d', name: 'rssi_dBm', value: -75 },
		{ tag: '0241', value: 0 }
	]
};

describe('valve debug statistics', () => {
	it('read a named value, or null when the answer lacks it', () => {
		expect(statValue(counters, 'rx_crc_fail')).toBe(4);
		expect(statValue(counters, 'rssi_dBm')).toBe(-75);
		expect(statValue(counters, 'mic_fail')).toBeNull();
		expect(statValue(null, 'rx_crc_fail')).toBeNull();
		expect(statValue({ f: 11, pending: true }, 'rx_crc_fail')).toBeNull();
	});

	it('read how much a counter moved between the last two answers', () => {
		const moved: DebugStats = { ...counters, delta: { interval_s: 3600, stats: [{ tag: '023b', name: 'rx_crc_fail', value: 2 }] } };
		expect(deltaValue(moved, 'rx_crc_fail')).toBe(2);
		expect(deltaValue(moved, 'mic_fail')).toBeNull();
		expect(deltaValue(counters, 'rx_crc_fail')).toBeNull();
		expect(deltaValue(null, 'rx_crc_fail')).toBeNull();
	});

	it('tell the RTC tick values apart', () => {
		expect(isTicks({ tag: '0223', name: 'awake_ticks', value: 1 })).toBe(true);
		expect(isTicks({ tag: '023b', name: 'rx_crc_fail', value: 1 })).toBe(false);
		expect(isTicks({ tag: '0241', value: 1 })).toBe(false);
	});

	it('take the newest answer time of the two functions', () => {
		const states: DebugStats = { f: 10, pending: false, received_at: '2026-11-07T10:05:00Z' };
		expect(lastRead([states, counters])).toBe(Date.parse('2026-11-07T10:05:00Z') / 1000);
		expect(lastRead([null, { f: 11, pending: true }])).toBeNull();
	});

	it('report an answer other than 2.05', () => {
		expect(refusal(counters)).toBeNull();
		expect(refusal({ f: 10, pending: false, code: '4.04' })).toBe('4.04');
		expect(refusal(null)).toBeNull();
	});
});
