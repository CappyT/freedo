import { ApiError } from './api.ts';
import { data } from './data.svelte.ts';
import { t } from './i18n.svelte.ts';
import { toasts } from './toast.svelte.ts';

/** The text of a failed request. The daemon explains a refused value in its own words. */
export function errorText(error: unknown): string {
	if (error instanceof ApiError) {
		if (error.status === 0) return t('error.unreachable');
		if (error.status === 401) return t('error.token');
		return error.message;
	}
	return error instanceof Error ? error.message : String(error);
}

/**
 * Runs a command, tells the user what happened and reads the state again.
 * It returns the answer once the page shows the state after the command, or undefined when the daemon refused.
 */
export async function act<T>(command: () => Promise<T>, done?: string): Promise<T | undefined> {
	try {
		const result = await command();
		const answer = result as { pending?: unknown; queued?: unknown } | undefined;
		// Most commands answer with the messages waiting for the device. PATCH /api/home counts the writes it queued.
		const count = answer?.pending ?? answer?.queued;
		const queued = typeof count === 'number' ? count : null;
		const dryRun = data.health !== null && !data.health.tx;
		const text = queued === null ? (done ?? t('toast.done')) : t('toast.queued', { n: queued });
		toasts.push('ok', dryRun ? `${text} ${t('toast.dryRun')}` : text);
		await data.refresh();
		return result;
	} catch (error) {
		toasts.push('error', errorText(error));
		return undefined;
	}
}

/** Runs one command for each zone and reports once: how many worked and the first refusal. */
export async function actMany(commands: (() => Promise<unknown>)[]): Promise<void> {
	const results = await Promise.allSettled(commands.map((run) => run()));
	const failed = results.filter((r): r is PromiseRejectedResult => r.status === 'rejected');
	const ok = results.length - failed.length;
	if (ok > 0) {
		const dryRun = data.health !== null && !data.health.tx;
		const text = t('toast.sentMany', { n: ok });
		toasts.push('ok', dryRun ? `${text} ${t('toast.dryRun')}` : text);
	}
	if (failed[0]) toasts.push('error', errorText(failed[0].reason));
	await data.refresh();
}
