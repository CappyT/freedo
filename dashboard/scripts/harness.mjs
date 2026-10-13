// Starts a daemon for the checks in this directory: no radio, a temporary database, a free
// loopback port. Never a real database, never a serial port.
import { spawn } from 'node:child_process';
import { mkdtempSync, rmSync } from 'node:fs';
import net from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

export function freePort() {
	return new Promise((resolve, reject) => {
		const server = net.createServer();
		server.listen(0, '127.0.0.1', () => {
			const { port } = server.address();
			server.close(() => resolve(port));
		});
		server.on('error', reject);
	});
}

/**
 * The answer of a daemon that waits after an import, as internal/daemon/state_transfer.go gives it.
 * The daemon of the checks has no radio, so it never waits: the checks put this answer in its place.
 */
export function waitingAnswer(path, leftSec) {
	const health = path.endsWith('/api/health');
	return {
		status: health ? 200 : 503,
		headers: health ? { 'Content-Type': 'application/json' } : { 'Content-Type': 'application/json', 'Retry-After': String(leftSec) },
		body: JSON.stringify(
			health
				? { ok: true, mode: 'live', tx: true, txArmed: false, transmitAllowed: false, importWait: { leftSec } }
				: { error: `freedo starts in ${leftSec} s: it waits after the import of a state` }
		)
	};
}

/** Starts the daemon binary. token sets FREEDO_API_TOKEN, and none leaves the API open on loopback. */
export async function startDaemon(bin, { token } = {}) {
	const work = mkdtempSync(join(tmpdir(), 'freedo-check-'));
	const port = await freePort();
	const env = { ...process.env };
	delete env.FREEDO_API_TOKEN;
	if (token) env.FREEDO_API_TOKEN = token;
	const child = spawn(
		bin,
		['--addr', `127.0.0.1:${port}`, '--state', join(work, 'state.db'), '--unhandled', join(work, 'unhandled.log')],
		// The daemon logs every queued write. CHECK_VERBOSE=1 shows the log.
		{ env, stdio: ['ignore', 'ignore', process.env.CHECK_VERBOSE ? 'inherit' : 'ignore'] }
	);
	const base = `http://127.0.0.1:${port}`;
	for (let i = 0; ; i++) {
		try {
			await fetch(base + '/api/health');
			break;
		} catch {
			if (i > 100) throw new Error('the daemon did not start');
			await new Promise((r) => setTimeout(r, 100));
		}
	}
	return {
		base,
		port,
		async stop() {
			child.kill('SIGTERM');
			await new Promise((resolve) => {
				child.once('exit', resolve);
				setTimeout(resolve, 5000);
			});
			rmSync(work, { recursive: true, force: true });
		}
	};
}
