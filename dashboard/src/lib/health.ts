import type { Health } from './types.ts';

export type SerialLink = 'none' | 'ok' | 'warning';

/**
 * The serial link counts the replies of the bridge that it lost since the daemon started. Only a
 * live radio has a link, so the field is absent in the other modes. Each lost reply is a radio
 * frame that never reached the daemon, so a count above zero is a warning.
 */
export function serialLink(health: Health | null): SerialLink {
	if (health?.serialErrors === undefined) return 'none';
	return health.serialErrors > 0 ? 'warning' : 'ok';
}
