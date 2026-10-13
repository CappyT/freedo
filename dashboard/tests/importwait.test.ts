import { describe, expect, it } from 'vitest';
import { uptimeOf } from '../src/lib/backup.ts';
import { secondsLeft, startOverdue, waitEnd } from '../src/lib/importwait.ts';
import type { Health } from '../src/lib/types.ts';

const now = 1_700_000_000_000;

describe('the wait after an import', () => {
	it('is absent in the health of a daemon that runs', () => {
		expect(waitEnd({}, now)).toBeNull();
		expect(waitEnd({}, now, now + 5000)).toBeNull();
	});

	it('ends leftSec after the answer', () => {
		expect(waitEnd({ importWait: { leftSec: 227 } }, now)).toBe(now + 227_000);
		expect(waitEnd({ importWait: { leftSec: -3 } }, now)).toBe(now);
	});

	it('never moves the end later, so the countdown does not go up', () => {
		const first = waitEnd({ importWait: { leftSec: 100 } }, now);
		expect(waitEnd({ importWait: { leftSec: 96 } }, now + 5000, first)).toBe(first);
		expect(waitEnd({ importWait: { leftSec: 94 } }, now + 5000, first)).toBe(now + 99_000);
	});

	it('counts whole seconds down to zero', () => {
		const end = now + 3000;
		expect(secondsLeft(end, now)).toBe(3);
		expect(secondsLeft(end, now + 1)).toBe(3);
		expect(secondsLeft(end, now + 2000)).toBe(1);
		expect(secondsLeft(end, now + 2999)).toBe(1);
		expect(secondsLeft(end, now + 3000)).toBe(0);
		expect(secondsLeft(end, now + 60_000)).toBe(0);
	});

	it('expects no answer for one minute after the end, while the daemon starts', () => {
		const end = now + 3000;
		expect(startOverdue(end, now)).toBe(false);
		expect(startOverdue(end, end + 60_000)).toBe(false);
		expect(startOverdue(end, end + 60_001)).toBe(true);
	});

	it('is not a daemon that started again', () => {
		expect(() => uptimeOf({ ok: true, importWait: { leftSec: 9 } } as Health)).toThrow();
		expect(uptimeOf({ ok: true, uptimeSec: 4 } as Health)).toBe(4);
	});
});
