import { createClient } from './api.ts';
import { auth } from './auth.svelte.ts';

/** The client of this page. It sends the stored token and reports a refusal to the auth store. */
export const api = createClient({
	token: () => auth.token,
	onUnauthorized: (sent) => auth.unauthorized(sent)
});
