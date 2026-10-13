import { describe, expect, it } from 'vitest';
import { ApiError } from '../src/lib/api.ts';
import { exportState, fileName, importState, waitForRestart } from '../src/lib/backup.ts';
import routes from '../../internal/daemon/ingress_routes.json';

interface Seen {
	method: string;
	url: string;
	authorization: string | null;
	type: string | null;
	body: unknown;
}

function stub(answer: () => Response) {
	const seen: Seen[] = [];
	const fetchStub = (async (url: string | URL | Request, init?: RequestInit) => {
		const headers = new Headers(init?.headers);
		seen.push({
			method: init?.method ?? 'GET',
			url: String(url),
			authorization: headers.get('Authorization'),
			type: headers.get('Content-Type'),
			body: init?.body
		});
		return answer();
	}) as unknown as typeof fetch;
	return { seen, fetch: fetchStub };
}

const prefix = 'http://ha.example/api/hassio_ingress/abc/';

describe('the backup calls', () => {
	it('download the export relative to the page, with the token', async () => {
		const { seen, fetch } = stub(
			() =>
				new Response('SQLite format 3', {
					headers: { 'Content-Disposition': 'attachment; filename="freedo-state-0.7.1-20260301T100000Z.db"' }
				})
		);
		const file = await exportState({ base: prefix, fetch, token: () => 'secret' });
		expect(seen).toEqual([
			{ method: 'GET', url: prefix + 'api/state/export', authorization: 'Bearer secret', type: null, body: undefined }
		]);
		expect(file.name).toBe('freedo-state-0.7.1-20260301T100000Z.db');
		expect(await file.blob.text()).toBe('SQLite format 3');
	});

	it('upload the file as the body, with the confirm field', async () => {
		const { seen, fetch } = stub(() => Response.json({ schema: '8', restart: true, waitSec: 250 }, { status: 202 }));
		const file = new Blob(['SQLite format 3']);
		const result = await importState(file, { base: prefix, fetch });
		expect(seen).toHaveLength(1);
		expect(seen[0]).toMatchObject({
			method: 'POST',
			url: prefix + 'api/state/import?confirm=true',
			authorization: null,
			type: 'application/octet-stream'
		});
		expect(seen[0]?.body).toBe(file);
		expect(result).toEqual({ schema: '8', restart: true, waitSec: 250 });
	});

	it('give the status and the message of a refusal', async () => {
		const { fetch } = stub(() => Response.json({ error: 'missing bearer token' }, { status: 401 }));
		const error = await exportState({ base: prefix, fetch }).catch((e: unknown) => e);
		expect(error).toBeInstanceOf(ApiError);
		expect(error).toMatchObject({ status: 401, message: 'missing bearer token' });
		const none = stub(() => new Response('gateway', { status: 502, statusText: 'Bad Gateway' }));
		await expect(importState(new Blob(['x']), { base: prefix, fetch: none.fetch })).rejects.toMatchObject({
			status: 502,
			message: 'Bad Gateway'
		});
	});

	it('are not in the list that ingress serves without the token', () => {
		expect(routes.filter((r) => r.includes('/api/state'))).toEqual([]);
	});
});

describe('the name of the downloaded file', () => {
	it('comes from the header, without a folder', () => {
		expect(fileName('attachment; filename="freedo-state-0.7.1-20260301T100000Z.db"')).toBe('freedo-state-0.7.1-20260301T100000Z.db');
		expect(fileName('attachment; filename="../../etc/state.db"')).toBe('state.db');
		expect(fileName(null)).toBe('freedo-state.db');
		expect(fileName('attachment')).toBe('freedo-state.db');
	});
});

describe('the wait for the restart', () => {
	// Each entry is the uptime the daemon answers, or null for no answer.
	const run = async (answers: (number | null)[], before: number, limitMs = 60_000) => {
		let clock = 0;
		let calls = 0;
		const done = await waitForRestart(
			async () => {
				const answer = answers[Math.min(calls++, answers.length - 1)];
				if (answer === null || answer === undefined) throw new Error('down');
				return answer;
			},
			{ before, limitMs, sleep: async (ms) => void (clock += ms), now: () => clock }
		);
		return { done, calls };
	};

	it('ends when the uptime starts again', async () => {
		expect(await run([120, 121, 0], 119)).toEqual({ done: true, calls: 3 });
	});

	it('ends on the first answer after a time with no answer', async () => {
		expect(await run([300, null, null, 301], 299)).toEqual({ done: true, calls: 4 });
	});

	it('gives up at the limit while the daemon is away', async () => {
		expect(await run([null], 10, 5000)).toEqual({ done: false, calls: 5 });
	});

	it('gives up at the limit while the old daemon still answers', async () => {
		expect(await run([50, 51, 52, 53], 49, 3000)).toEqual({ done: false, calls: 3 });
	});
});
