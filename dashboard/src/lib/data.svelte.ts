import { ApiError } from './api.ts';
import { auth } from './auth.svelte.ts';
import { coalesce } from './coalesce.ts';
import { secondsLeft, startOverdue, waitEnd } from './importwait.ts';
import { api } from './session.ts';
import type { Boiler, Circuit, Health, Home, NodeView, Preset, Zones } from './types.ts';
import { buildZones } from './zones.ts';

const POLL_MS = 5000;

/** The state of the daemon as the pages show it. One timer refreshes it while the tab is visible. */
class Data {
	health = $state<Health | null>(null);
	zones = $state<Zones | null>(null);
	nodes = $state<NodeView[]>([]);
	home = $state<Home | null>(null);
	boiler = $state<Boiler | null>(null);
	circuits = $state<Circuit[]>([]);
	presets = $state<Preset[]>([]);
	/** False while no request reaches the daemon. */
	reachable = $state(true);
	/** True while the daemon restarts on purpose: no answer is expected, not a fault. */
	restarting = $state(false);
	/** When the wait after an import ends, on the clock of this page. Null while the daemon does not wait. */
	waitEnd = $state<number | null>(null);
	#now = $state(Date.now());
	/** The seconds of the wait that are left. Zero while the daemon starts after the wait. */
	waitSec = $derived(this.waitEnd === null ? null : secondsLeft(this.waitEnd, this.#now));
	loaded = $state(false);
	/** The message of the last failed request, shown when nothing loaded. */
	problem = $state<string | null>(null);

	away = $derived(this.home?.away ?? this.health?.away ?? false);
	models = $derived(
		buildZones(this.zones?.declared ?? [], this.zones?.demand ?? [], this.nodes, this.health, this.away)
	);
	heating = $derived(this.models.filter((m) => m.zone.kind === 'heating'));
	hotWater = $derived(this.models.find((m) => m.zone.kind === 'hotwater'));

	#timer: ReturnType<typeof setInterval> | undefined;
	#clock: ReturnType<typeof setInterval> | undefined;
	#reads = coalesce(() => this.#read());

	start() {
		if (this.#timer !== undefined) return;
		void this.refresh();
		// A tick while a read is in flight is skipped: the read in flight is as fresh as the tick.
		this.#timer = setInterval(() => {
			if (!this.#reads.busy()) void this.refresh();
		}, POLL_MS);
		this.#clock = setInterval(() => {
			if (this.waitEnd === null) return;
			this.#now = Date.now();
			// The daemon starts at the end of the wait: ask each second, so the page comes back at once.
			if (this.waitSec === 0 && !this.#reads.busy()) void this.refresh();
		}, 1000);
		document.addEventListener('visibilitychange', this.#visible);
	}

	stop() {
		clearInterval(this.#timer);
		clearInterval(this.#clock);
		this.#timer = undefined;
		document.removeEventListener('visibilitychange', this.#visible);
	}

	#visible = () => {
		if (!document.hidden) void this.refresh();
	};

	/** Reads every view, and resolves once a read that started after this call ends. */
	refresh(): Promise<void> {
		if (document.hidden || auth.blocked) return Promise.resolve();
		return this.#reads.run();
	}

	/** A view that fails keeps its last value. */
	async #read() {
		if (this.waitEnd !== null) {
			// A daemon that waits answers 503 to every other read.
			try {
				const end = waitEnd(await api.health(), Date.now(), this.waitEnd);
				if (end !== null) {
					this.waitEnd = end;
					return;
				}
			} catch {
				if (!startOverdue(this.waitEnd, Date.now())) return;
			}
			this.waitEnd = null;
		}
		const [health, zones, nodes, home, boiler, circuits, presets] = await Promise.allSettled([
			api.health(),
			api.zones(),
			api.nodes(),
			api.home(),
			api.boiler(),
			api.circuits(),
			api.presets()
		]);
		if (health.status === 'fulfilled') {
			const end = waitEnd(health.value, Date.now());
			if (end !== null) {
				auth.accepted();
				this.#now = Date.now();
				this.waitEnd = end;
				this.reachable = true;
				this.problem = null;
				return;
			}
			this.health = health.value;
		}
		if (zones.status === 'fulfilled') this.zones = zones.value;
		if (nodes.status === 'fulfilled') this.nodes = nodes.value;
		if (home.status === 'fulfilled') this.home = home.value;
		if (boiler.status === 'fulfilled') this.boiler = boiler.value;
		// An installation with no circuit answers null.
		if (circuits.status === 'fulfilled') this.circuits = circuits.value ?? [];
		if (presets.status === 'fulfilled') this.presets = presets.value.presets;
		const all = [health, zones, nodes, home, boiler, circuits, presets];
		const failed = all.filter((r): r is PromiseRejectedResult => r.status === 'rejected');
		if (failed.length < all.length) auth.accepted();
		this.reachable = !(failed.length === all.length && failed.every((r) => r.reason instanceof ApiError && r.reason.status === 0));
		const reason = failed.find((r) => !(r.reason instanceof ApiError && r.reason.status === 401))?.reason;
		this.problem = reason instanceof Error ? reason.message : null;
		this.loaded = true;
	}
}

export const data = new Data();
