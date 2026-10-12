import { describe, expect, it } from 'vitest';
import { coalesce } from '../src/lib/coalesce.ts';

/** A read that ends when the test says so. */
function reads() {
	const pending: { resolve: () => void; reject: (e: Error) => void }[] = [];
	const read = () => new Promise<void>((resolve, reject) => pending.push({ resolve, reject }));
	const nth = (i: number) => {
		const p = pending[i];
		if (!p) throw new Error(`read ${i} did not start`);
		return p;
	};
	return { read, pending, nth };
}

const settled = () => new Promise((r) => setTimeout(r, 0));

describe('coalesced reads', () => {
	it('answer a call during a read in flight with a read that starts after it', async () => {
		const { read, pending, nth } = reads();
		const c = coalesce(read);
		const first = c.run();
		// A rename lands here: the read in flight may hold the old label.
		let done = false;
		const second = c.run().then(() => (done = true));
		expect(pending).toHaveLength(1);
		nth(0).resolve();
		await first;
		await settled();
		expect(pending).toHaveLength(2);
		expect(done).toBe(false);
		nth(1).resolve();
		await second;
		expect(done).toBe(true);
		expect(c.busy()).toBe(false);
	});

	it('share one queued read among the calls during the same read', async () => {
		const { read, pending, nth } = reads();
		const c = coalesce(read);
		void c.run();
		const queued = [c.run(), c.run(), c.run()];
		nth(0).resolve();
		await settled();
		expect(pending).toHaveLength(2);
		nth(1).resolve();
		await Promise.all(queued);
		expect(pending).toHaveLength(2);
	});

	it('run the queued read after a failed read', async () => {
		const { read, pending, nth } = reads();
		const c = coalesce(read);
		const first = c.run().catch(() => 'failed');
		const second = c.run();
		nth(0).reject(new Error('down'));
		expect(await first).toBe('failed');
		await settled();
		expect(pending).toHaveLength(2);
		nth(1).resolve();
		await second;
	});
});
