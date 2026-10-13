import { createClient, createInstallClient } from './api.ts';
import { auth } from './auth.svelte.ts';

/** The client of this page. It sends the stored token and reports a refusal to the auth store. */
export const api = createClient({
	token: () => auth.token,
	onUnauthorized: (sent) => auth.unauthorized(sent)
});

/**
 * The client of the installation pages. A refusal does not reach the auth store: under Home
 * Assistant ingress the rest of the page works without a token, and only these pages ask for one.
 */
export const install = createInstallClient({ token: () => auth.token });
