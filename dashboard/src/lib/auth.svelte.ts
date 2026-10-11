import { readStored, writeStored } from './storage.ts';

const TOKEN_KEY = 'freedo.token';

/**
 * The bearer token of the API. Home Assistant ingress needs none: the daemon trusts
 * the gateway, so the first answer is never 401 and the token is never asked for.
 */
class Auth {
	token = $state<string | null>(readStored(TOKEN_KEY));
	/** required: the daemon wants a token and none is stored. rejected: it refused the stored one. */
	status = $state<'unknown' | 'ok' | 'required' | 'rejected'>('unknown');

	set(token: string) {
		const clean = token.trim();
		this.token = clean === '' ? null : clean;
		writeStored(TOKEN_KEY, this.token);
		this.status = 'unknown';
	}

	clear() {
		this.set('');
	}

	unauthorized(sentToken: boolean) {
		this.status = sentToken ? 'rejected' : 'required';
	}

	accepted() {
		if (this.status !== 'ok') this.status = 'ok';
	}

	get blocked() {
		return this.status === 'required' || this.status === 'rejected';
	}
}

export const auth = new Auth();
