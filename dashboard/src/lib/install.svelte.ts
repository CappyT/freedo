import { errorText } from './actions.ts';
import { data } from './data.svelte.ts';
import { t } from './i18n.svelte.ts';
import { api } from './session.ts';
import { toasts } from './toast.svelte.ts';
import type { InstallInfo } from './types.ts';

/** What the management pages know about the installation beyond the polled views. */
class Install {
	info = $state<InstallInfo | null>(null);

	/** Reads the installation. A failed read keeps the last value. */
	async load() {
		try {
			const info = await api.installInfo();
			// The daemon prefixes each finding with the name of its checker.
			this.info = { ...info, warnings: info.warnings.map((w) => w.replace(/^tado: config: /, '')) };
		} catch {
			// The banners and the token gate report a daemon that does not answer.
		}
	}
}

export const install = new Install();

/**
 * Runs a change of the installation, tells the user what happened and reads the state again.
 * It returns the answer, or undefined when the daemon refused. After a refusal the page also reads
 * the state again, because a change made of several writes can stop half way.
 */
export async function manage<T>(command: () => Promise<T>, done?: string): Promise<T | undefined> {
	let result: T | undefined;
	try {
		result = await command();
		toasts.push('ok', done ?? t('toast.done'));
	} catch (error) {
		toasts.push('error', errorText(error));
	}
	await Promise.all([data.refresh(), install.load()]);
	return result;
}
