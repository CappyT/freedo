import { describe, expect, it } from 'vitest';
import { serialLink } from '../src/lib/health.ts';
import type { Health } from '../src/lib/types.ts';

const health = (over: Partial<Health>) => ({ ok: true, mode: 'live', ...over }) as Health;

describe('serial link state', () => {
	it('is absent without a live radio, where the daemon omits the field', () => {
		expect(serialLink(null)).toBe('none');
		expect(serialLink(health({ mode: 'mock' }))).toBe('none');
	});

	it('is fine at zero lost replies', () => {
		expect(serialLink(health({ serialErrors: 0 }))).toBe('ok');
	});

	it('warns from the first lost reply', () => {
		expect(serialLink(health({ serialErrors: 1 }))).toBe('warning');
		expect(serialLink(health({ serialErrors: 4096 }))).toBe('warning');
	});
});
