import { ApiError } from './api.ts';

/**
 * The export and the import of the whole state of the daemon. The two calls are not in the client of
 * api.ts: they need the API token also under Home Assistant ingress, because the file holds the
 * network key and an import replaces the installation.
 */
export interface BackupOptions {
	/** Document the API paths are relative to. The default is the base of the page. */
	base?: string;
	fetch?: typeof fetch;
	token?: () => string | null;
}

export interface BackupFile {
	name: string;
	blob: Blob;
}

/** The answer of an accepted import. The daemon restarts, and waitSec is how long it then waits before it starts. */
export interface ImportResult {
	schema: string;
	exportedBy?: string;
	exportedAt?: string;
	restart: boolean;
	waitSec: number;
}

const FALLBACK_NAME = 'freedo-state.db';
const TRANSFER_TIMEOUT_MS = 60_000;

/** The file name of a Content-Disposition header, without any folder part. */
export function fileName(disposition: string | null): string {
	const match = /filename="([^"]+)"/.exec(disposition ?? '');
	const name = match?.[1]?.split(/[\\/]/).pop()?.trim();
	return name ? name : FALLBACK_NAME;
}

async function call(path: string, init: RequestInit, options: BackupOptions): Promise<Response> {
	const doFetch = options.fetch ?? ((...args: Parameters<typeof fetch>) => fetch(...args));
	const base = options.base ?? (typeof document === 'undefined' ? 'http://localhost/' : document.baseURI);
	const headers: Record<string, string> = { ...(init.headers as Record<string, string> | undefined) };
	const token = options.token?.() ?? null;
	if (token) headers.Authorization = 'Bearer ' + token;
	let response: Response;
	try {
		response = await doFetch(new URL('api/' + path, base).href, {
			...init,
			headers,
			signal: AbortSignal.timeout(TRANSFER_TIMEOUT_MS)
		});
	} catch (error) {
		throw new ApiError(0, error instanceof Error ? error.message : 'network error');
	}
	if (response.ok) return response;
	let message = response.statusText || 'HTTP ' + response.status;
	try {
		const parsed: unknown = JSON.parse(await response.text());
		if (typeof parsed === 'object' && parsed !== null && 'error' in parsed) message = String(parsed.error);
	} catch {
		// The status text stands.
	}
	throw new ApiError(response.status, message);
}

/** Downloads a snapshot of the state database. The file holds the network key. */
export async function exportState(options: BackupOptions = {}): Promise<BackupFile> {
	const response = await call('state/export', { method: 'GET' }, options);
	return { name: fileName(response.headers.get('Content-Disposition')), blob: await response.blob() };
}

/** Sends a backup file. The daemon checks it, keeps its current state as a copy and restarts on the file. */
export async function importState(file: Blob, options: BackupOptions = {}): Promise<ImportResult> {
	const response = await call(
		'state/import?confirm=true',
		{ method: 'POST', headers: { 'Content-Type': 'application/octet-stream' }, body: file },
		options
	);
	return (await response.json()) as ImportResult;
}

export interface RestartWait {
	/** The uptime of the daemon before the restart, in seconds. */
	before: number;
	limitMs: number;
	stepMs?: number;
	sleep?: (ms: number) => Promise<void>;
	now?: () => number;
}

/**
 * Waits until the daemon answers again after a restart: its uptime is lower than before, or it answers
 * after a time with no answer. Resolves false when the limit passes first.
 */
export async function waitForRestart(uptime: () => Promise<number>, wait: RestartWait): Promise<boolean> {
	const sleep = wait.sleep ?? ((ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms)));
	const now = wait.now ?? Date.now;
	const end = now() + wait.limitMs;
	let down = false;
	while (now() < end) {
		await sleep(wait.stepMs ?? 1000);
		try {
			const current = await uptime();
			if (down || current < wait.before) return true;
		} catch {
			down = true;
		}
	}
	return false;
}
