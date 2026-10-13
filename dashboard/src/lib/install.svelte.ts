import { ApiError } from './api.ts';
import { auth } from './auth.svelte.ts';
import { data } from './data.svelte.ts';
import { t } from './i18n.svelte.ts';
import { errorText } from './actions.ts';
import { install as installApi } from './session.ts';
import { toasts } from './toast.svelte.ts';
import type { InstallInfo } from './types.ts';

/** What the management pages know about the installation beyond the polled views. */
class Install {
	info = $state<InstallInfo | null>(null);
	/** The daemon refused a management call for lack of a token: Home Assistant ingress covers none of them. */
	needsToken = $state(false);

	/** Reads the installation. A refusal keeps the last value. */
	async load() {
		try {
			const info = await installApi.info();
			// The daemon prefixes each finding with the name of its checker.
			this.info = { ...info, warnings: info.warnings.map((w) => w.replace(/^tado: config: /, '')) };
			this.needsToken = false;
		} catch (error) {
			this.refused(error);
		}
	}

	/** Notes a refusal for lack of a token. It returns whether the error was one. */
	refused(error: unknown): boolean {
		if (!(error instanceof ApiError) || error.status !== 401) return false;
		this.needsToken = true;
		return true;
	}

	setToken(token: string) {
		auth.set(token);
		this.needsToken = false;
		void this.load();
		void data.refresh();
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
		toasts.push('error', install.refused(error) ? t('manage.token.needed') : errorText(error));
	}
	await Promise.all([data.refresh(), install.load()]);
	return result;
}
