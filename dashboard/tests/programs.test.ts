import { describe, expect, it } from 'vitest';
import { changedZones } from '../src/lib/programs.ts';
import type { PresetSchedules, PresetZoneSchedules } from '../src/lib/types.ts';

const zone = (number: number, inForce: PresetZoneSchedules['inForce']): PresetZoneSchedules => ({
	zone: number,
	name: `zone ${number}`,
	kind: 'heating',
	home: null,
	away: null,
	inForce
});
const preset = (id: number, zones: PresetZoneSchedules[]): PresetSchedules => ({
	preset: { id, name: `p${id}`, active: id === 1 },
	zones
});
const block = (setpointC: number, off = false) => ({ mode: 'home' as const, setpointC, off });

describe('the zones an activation changes', () => {
	it('lists the zones whose block in force moves, and no other', () => {
		const active = preset(1, [zone(1, block(20)), zone(2, block(18)), zone(3, null), zone(4, block(21))]);
		const target = preset(2, [zone(1, block(20)), zone(2, block(21)), zone(3, block(19)), zone(4, null)]);
		expect(changedZones(active, target).map((c) => [c.zone, c.from?.setpointC ?? null, c.to?.setpointC ?? null])).toEqual([
			[2, 18, 21],
			[3, null, 19],
			[4, 21, null]
		]);
	});

	it('treats two off blocks as the same block', () => {
		const active = preset(1, [zone(1, block(15, true)), zone(2, block(15, true))]);
		const target = preset(2, [zone(1, block(5, true)), zone(2, block(15))]);
		expect(changedZones(active, target).map((c) => c.zone)).toEqual([2]);
	});

	it('finds nothing when the target is the active preset', () => {
		const active = preset(1, [zone(1, block(20)), zone(2, null)]);
		expect(changedZones(active, active)).toEqual([]);
	});
});
