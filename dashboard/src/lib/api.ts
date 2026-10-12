import type {
	Boiler,
	Circuit,
	EndRule,
	Health,
	Home,
	NodeView,
	Preset,
	Queued,
	ScheduleView,
	WindowView,
	Zones
} from './types.ts';

export class ApiError extends Error {
	constructor(
		readonly status: number,
		message: string,
		readonly body?: unknown
	) {
		super(message);
	}
}

export interface ClientOptions {
	/** Document the API paths are relative to. The default is the base of the page. */
	base?: string;
	fetch?: typeof fetch;
	token?: () => string | null;
	/** Called on a 401 answer, with whether the request carried a token. */
	onUnauthorized?: (sentToken: boolean) => void;
	timeoutMs?: number;
}

/** How a setpoint or an off command ends: the rule names of the API. */
export interface EndSpec {
	mode: EndRule;
	durationSec?: number;
}

/** Which program of which preset a schedule route reads or writes. Both default to home and the active preset. */
export interface ScheduleQuery {
	mode?: 'home' | 'away';
	preset?: number;
}

export interface Command extends Queued {
	zone?: number;
	target?: string;
}

/**
 * API calls are relative to the document, so the same build works at the root of
 * the daemon and under a prefix such as the Home Assistant ingress path.
 */
export function createClient(options: ClientOptions = {}) {
	const doFetch = options.fetch ?? ((...args: Parameters<typeof fetch>) => fetch(...args));
	const timeoutMs = options.timeoutMs ?? 15_000;

	function url(path: string): string {
		const base = options.base ?? (typeof document === 'undefined' ? 'http://localhost/' : document.baseURI);
		return new URL('api/' + path, base).href;
	}

	async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
		const headers: Record<string, string> = { Accept: 'application/json' };
		const token = options.token?.() ?? null;
		if (token) headers.Authorization = 'Bearer ' + token;
		if (body !== undefined) headers['Content-Type'] = 'application/json';
		let response: Response;
		try {
			response = await doFetch(url(path), {
				method,
				headers,
				body: body === undefined ? undefined : JSON.stringify(body),
				signal: AbortSignal.timeout(timeoutMs)
			});
		} catch (error) {
			throw new ApiError(0, error instanceof Error ? error.message : 'network error');
		}
		const text = await response.text();
		let parsed: unknown;
		try {
			parsed = text === '' ? undefined : JSON.parse(text);
		} catch {
			parsed = undefined;
		}
		if (response.status === 401) options.onUnauthorized?.(token !== null);
		if (!response.ok) {
			const message =
				typeof parsed === 'object' && parsed !== null && 'error' in parsed
					? String((parsed as { error: unknown }).error)
					: response.statusText || 'HTTP ' + response.status;
			throw new ApiError(response.status, message, parsed);
		}
		return parsed as T;
	}

	const zone = (n: number) => 'zones/' + n;

	return {
		url,
		health: async (): Promise<Health> => {
			try {
				return await request<Health>('GET', 'health');
			} catch (error) {
				// A busy engine or a lost radio answers 503 with the health body.
				if (error instanceof ApiError && error.status === 503 && isHealth(error.body)) return error.body;
				throw error;
			}
		},
		zones: () => request<Zones>('GET', 'zones'),
		nodes: () => request<NodeView[]>('GET', 'nodes'),
		home: () => request<Home>('GET', 'home'),
		boiler: () => request<Boiler>('GET', 'boiler'),
		circuits: () => request<Circuit[]>('GET', 'circuits'),
		/** The programs of a zone, or null while the named mode holds none. */
		schedule: async (n: number, query: ScheduleQuery = {}): Promise<ScheduleView | null> => {
			try {
				return await request<ScheduleView>('GET', zone(n) + '/schedule' + scheduleQuery(query));
			} catch (error) {
				if (error instanceof ApiError && error.status === 404) return null;
				throw error;
			}
		},
		putSchedule: (n: number, programs: unknown, query: ScheduleQuery = {}) =>
			request<ScheduleView>('PUT', zone(n) + '/schedule' + scheduleQuery(query), { programs }),
		deleteSchedule: (n: number, query: ScheduleQuery = {}) =>
			request<ScheduleView>('DELETE', zone(n) + '/schedule' + scheduleQuery(query)),
		setpoint: (n: number, setpointC: number, end: EndSpec) =>
			request<Command>('POST', zone(n) + '/setpoint', { setpointC, ...endBody(end) }),
		off: (n: number, end: EndSpec) => request<Command>('POST', zone(n) + '/off', endBody(end)),
		auto: (n: number) => request<Command>('POST', zone(n) + '/auto', {}),
		boost: (n: number, on: boolean) => request<Command>('POST', zone(n) + '/boost', { on }),
		window: (n: number, open: boolean) => request<WindowView>('POST', zone(n) + '/window', { open }),
		setAway: (away: boolean) => request<Home & { queued: number }>('PATCH', 'home', { away }),
		/** The program presets, in creation order. Exactly one carries active. */
		presets: () => request<{ presets: Preset[] }>('GET', 'presets'),
		/** Creates a preset with a copy of the active one, or of copyFrom. */
		createPreset: (name: string, copyFrom?: number) =>
			request<Preset>('POST', 'presets', copyFrom === undefined ? { name } : { name, copy_from: copyFrom }),
		renamePreset: (id: number, name: string) => request<Preset>('PATCH', 'presets/' + id, { name }),
		/** Removes a preset and its programs. The active preset answers 409. */
		deletePreset: (id: number) => request<{ dropped: number }>('DELETE', 'presets/' + id),
		activatePreset: (id: number) =>
			request<{ id: number; name: string; active: boolean; queued: number }>('POST', 'presets/' + id + '/activate'),
		/** The label of a device is unique in the installation, compared without case. */
		renameDevice: (ref: string, label: string) =>
			request<{ rev: number; device: unknown }>(
				'PUT',
				'install/devices/' + encodeURIComponent(ref) + '/label',
				{ label }
			),
		flow: (tempC: number, circuit?: number) =>
			request<Command>('POST', 'boiler/flow', circuit === undefined ? { tempC } : { tempC, id: circuit }),
		childLock: (device: string, on: boolean) =>
			request<Command>('POST', 'nodes/' + encodeURIComponent(device) + '/childlock', { on }),
		identify: (device: string) => request<Command>('POST', 'nodes/' + encodeURIComponent(device) + '/identify', {})
	};
}

export type Client = ReturnType<typeof createClient>;

/** timer carries durationSec; the other rules carry none. */
function endBody(end: EndSpec): { mode: EndRule; durationSec?: number } {
	return end.mode === 'timer' ? { mode: 'timer', durationSec: end.durationSec } : { mode: end.mode };
}

/** The query string of a schedule route. An absent field keeps the daemon default. */
function scheduleQuery(query: ScheduleQuery): string {
	const params = new URLSearchParams();
	if (query.mode !== undefined) params.set('mode', query.mode);
	if (query.preset !== undefined) params.set('preset', String(query.preset));
	const text = params.toString();
	return text === '' ? '' : '?' + text;
}

function isHealth(value: unknown): value is Health {
	return typeof value === 'object' && value !== null && 'ok' in value && 'mode' in value;
}
