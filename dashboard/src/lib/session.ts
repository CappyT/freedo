import { createClient, recalibrate } from './api.ts';
import { auth } from './auth.svelte.ts';

/** The client of this page. It sends the stored token and reports a refusal to the auth store. */
export const api = createClient({
	token: () => auth.token,
	onUnauthorized: (sent) => auth.unauthorized(sent)
});

/**
 * Starts the receiver calibration again. It sends the stored token. A refusal stays with the caller:
 * it never reaches the auth store, so the token gate does not replace the page.
 */
export const calibrateAgain = () => recalibrate({ token: () => auth.token });
