/**
 * The wait of a daemon that transmits after an import of the state. During the wait GET /api/health
 * carries importWait and every other call answers 503.
 */

/** After the wait the daemon opens the radio and gives no answer for a moment. Longer than this is a fault. */
const START_GRACE_MS = 60_000;

/**
 * When the wait ends on the clock of the page, or null when the health answer reports no wait.
 * leftSec is rounded up and the answer takes time to arrive, so the earliest end is the best one.
 */
export function waitEnd(health: { importWait?: { leftSec: number } }, nowMs: number, current: number | null = null): number | null {
	const left = health.importWait?.leftSec;
	if (typeof left !== 'number') return null;
	const end = nowMs + Math.max(0, left) * 1000;
	return current === null ? end : Math.min(current, end);
}

/** The whole seconds left to show. */
export function secondsLeft(endMs: number, nowMs: number): number {
	return Math.max(0, Math.ceil((endMs - nowMs) / 1000));
}

/** True when a daemon that gives no answer is no longer the expected start after the wait. */
export function startOverdue(endMs: number, nowMs: number): boolean {
	return nowMs > endMs + START_GRACE_MS;
}
