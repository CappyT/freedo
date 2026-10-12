export interface Coalesced {
	/** Resolves after a read that started after this call. */
	run(): Promise<void>;
	/** True while a read is in flight. */
	busy(): boolean;
}

/**
 * Runs one read at a time. A call while a read is in flight queues one more read after it, because
 * the read in flight can have started before the change the caller wants to see. Calls during the
 * same read share the queued read.
 */
export function coalesce(read: () => Promise<void>): Coalesced {
	let current: Promise<void> | null = null;
	let next: Promise<void> | null = null;

	function run(): Promise<void> {
		if (current === null) {
			current = read().finally(() => {
				current = null;
			});
			return current;
		}
		next ??= current
			.catch(() => {})
			.then(() => {
				next = null;
				return run();
			});
		return next;
	}

	return { run, busy: () => current !== null };
}
