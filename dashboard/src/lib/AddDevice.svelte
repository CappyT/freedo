<script lang="ts">
	import { onMount } from 'svelte';
	import { errorText } from './actions.ts';
	import { data } from './data.svelte.ts';
	import Icon from './Icon.svelte';
	import { t } from './i18n.svelte.ts';
	import { install as installState, manage } from './install.svelte.ts';
	import { countdown, joinedSince, wireEui } from './install.ts';
	import { install } from './session.ts';
	import { toasts } from './toast.svelte.ts';
	import type { PairWindow } from './types.ts';
	import { zoneName } from './zones.ts';

	// The longest window the daemon accepts. The page closes it when the pairing ends.
	const WINDOW_SEC = 600;
	const POLL_MS = 2000;

	/** A device that got the pair command and has not joined yet. */
	interface Pending {
		eui: string;
		short: string;
		name: string;
		/** What the daemon knew of the device before the command. */
		before: { serial: string; sessionUnix: number };
	}

	let pairWindow = $state<PairWindow | null>(null);
	let problem = $state<string | null>(null);
	let eui = $state('');
	let kind = $state<'valve' | 'thermostat'>('valve');
	let label = $state('');
	let room = $state('');
	let busy = $state(false);
	let pending = $state<Pending | null>(null);
	/** The name of the last device that joined. */
	let joined = $state<string | null>(null);
	let now = $state(Date.now());

	const health = $derived(data.health);
	const zones = $derived(data.zones?.declared ?? []);
	const target = $derived(wireEui(eui));
	const joiners = $derived(pairWindow?.joiners ?? []);
	// The daemon lists a declared device in the health view, by its short address.
	const declared = (short: string) => health?.devices.find((d) => d.short === short);
	const known = $derived.by(() => {
		const joiner = joiners.find((j) => j.eui === target);
		return joiner ? declared(joiner.short) : undefined;
	});
	// Hot water takes the wall thermostat only.
	const rooms = $derived(zones.filter((z) => z.kind === 'heating' || kind === 'thermostat'));
	const ready = $derived(target !== null && (known !== undefined || label.trim() !== ''));
	const left = $derived(pairWindow?.open && pairWindow.until ? countdown((Date.parse(pairWindow.until) - now) / 1000) : null);

	async function read() {
		try {
			pairWindow = await install.pairWindow();
			problem = null;
		} catch (error) {
			if (!installState.refused(error)) problem = errorText(error);
		}
	}

	onMount(() => {
		void read();
		const poll = setInterval(() => {
			if (document.hidden) return;
			if (pairWindow?.open) void read();
			// The node view tells when the device joined. The shared poll alone would show it up to 5 seconds late.
			if (pending) void data.refresh();
		}, POLL_MS);
		const clock = setInterval(() => (now = Date.now()), 1000);
		return () => {
			clearInterval(poll);
			clearInterval(clock);
		};
	});

	// The pairing ends when the daemon reports that the device joined. The window closes then.
	$effect(() => {
		const waiting = pending;
		if (!waiting) return;
		if (!joinedSince(data.nodes.find((n) => n.Short === waiting.short), waiting.before)) return;
		pending = null;
		joined = waiting.name;
		toasts.push('ok', t('pair.joined', { name: waiting.name }));
		void install.openPairWindow(0).then((view) => (pairWindow = view), () => undefined);
	});

	async function setWindow(seconds: number) {
		busy = true;
		const view = await manage(() => install.openPairWindow(seconds), seconds > 0 ? t('pair.opened') : t('pair.closed'));
		busy = false;
		if (view) pairWindow = view;
	}

	async function submit(event: SubmitEvent) {
		event.preventDefault();
		if (target === null || !ready) return;
		const name = known ? known.label || known.device : label.trim();
		const zone = room === '' ? null : Number(room);
		const existing = known;
		busy = true;
		const short = await manage(async () => {
			let address = existing?.short;
			if (address === undefined) {
				address = (await install.createDevice({ eui: target, kind, label: name })).device.short;
				if (zone !== null) await install.addMember(zone, target);
			}
			await install.pair(target);
			return address;
		}, t('pair.sent', { name }));
		busy = false;
		if (short === undefined) return;
		const node = data.nodes.find((n) => n.Short === short);
		pending = { eui: target, short, name, before: { serial: node?.Serial ?? '', sessionUnix: node?.SessionIssuedUnix ?? 0 } };
		joined = null;
		eui = '';
		label = '';
	}

	async function retry() {
		const waiting = pending;
		if (!waiting) return;
		busy = true;
		await manage(() => install.pair(waiting.eui), t('pair.sent', { name: waiting.name }));
		busy = false;
	}
</script>

<div class="add" data-testid="add-device">
	{#if health && !health.transmitAllowed}
		<p class="notice tone-warning" role="status"><Icon name="warning" /><span>{t('pair.noTx')}</span></p>
	{/if}

	<ol class="steps">
		<li>
			<h3>{t('pair.step.window')}</h3>
			<p class="small muted">{t('pair.step.window.body')}</p>
			<div class="row fill">
				{#if pairWindow?.open}
					<span class="badge tone-heating" data-testid="pair-left">{t('pair.left', { time: left ?? '-' })}</span>
					<button class="btn" type="button" disabled={busy} onclick={() => setWindow(0)}>{t('pair.close')}</button>
				{:else}
					<button class="btn primary" type="button" disabled={busy} onclick={() => setWindow(WINDOW_SEC)}>
						<Icon name="bolt" size={18} />{t('pair.open')}
					</button>
				{/if}
			</div>
			{#if pairWindow?.open && health?.transmitAllowed && !pairWindow.responder}
				<p class="note">{t('pair.noResponder')}</p>
			{/if}
			{#if problem}<p class="note">{problem}</p>{/if}
		</li>

		<li>
			<h3>{t('pair.step.button')}</h3>
			<p class="small muted">{t('pair.step.button.body')}</p>
			{#if joiners.length === 0}
				<p class="small waiting" role="status">{pairWindow?.open ? t('pair.waiting') : t('pair.noJoiners')}</p>
			{:else}
				<ul class="joiners">
					{#each joiners as joiner (joiner.eui)}
						{@const device = declared(joiner.short)}
						<li>
							<button class="btn joiner" type="button" aria-pressed={joiner.eui === target} disabled={pending !== null} onclick={() => (eui = joiner.eui)}>
								<span class="mono">{joiner.eui}</span>
								<span class="small muted">{device ? t('pair.joiner.known', { name: device.label || device.device }) : t('pair.joiner.new')}</span>
							</button>
						</li>
					{/each}
				</ul>
			{/if}
		</li>

		<li>
			<h3>{t('pair.step.describe')}</h3>
			{#if pending}
				<div class="notice tone-warning progress" role="status" data-testid="pair-progress">
					<Icon name="clock" />
					<div>
						<p><strong>{t('pair.progress', { name: pending.name })}</strong></p>
						<p>{pairWindow?.open ? t('pair.progress.body') : t('pair.progress.closed')}</p>
					</div>
				</div>
				<div class="row fill">
					<button class="btn primary" type="button" disabled={busy || !pairWindow?.open} onclick={retry}>
						<Icon name="refresh" size={18} />{t('pair.retry')}
					</button>
					<button class="btn" type="button" disabled={busy} onclick={() => (pending = null)}>{t('pair.stop')}</button>
				</div>
			{:else}
				{#if joined}
					<p class="notice tone-idle" role="status" data-testid="pair-joined"><Icon name="check" /><span>{t('pair.joined', { name: joined })}</span></p>
				{/if}
				<form class="row fill" onsubmit={submit}>
					<label class="field">
						<span>{t('pair.eui')}</span>
						<input class="input mono eui" bind:value={eui} placeholder="0000000000000000" maxlength="16" autocomplete="off" spellcheck="false" aria-invalid={eui.trim() !== '' && target === null} />
					</label>
					{#if !known}
						<label class="field">
							<span>{t('pair.kind')}</span>
							<select class="input" bind:value={kind}>
								<option value="valve">{t('kind.valve')}</option>
								<option value="thermostat">{t('kind.thermostat')}</option>
							</select>
						</label>
						<label class="field grow">
							<span>{t('devices.name')}</span>
							<input class="input" bind:value={label} placeholder={t('pair.nameHint')} autocomplete="off" />
						</label>
						<label class="field">
							<span>{t('devices.zone')}</span>
							<select class="input" bind:value={room}>
								<option value="">{t('devices.noRoom')}</option>
								{#each rooms as z (z.number)}<option value={String(z.number)}>{zoneName(z, t('zone.hotWaterName'))}</option>{/each}
							</select>
						</label>
					{/if}
					<button class="btn primary" type="submit" disabled={busy || !ready}>
						<Icon name="check" size={18} />{known ? t('pair.again') : t('pair.add')}
					</button>
				</form>
				{#if known}<p class="note">{t('pair.known', { name: known.label || known.device })}</p>{/if}
			{/if}
		</li>
	</ol>
</div>

<style>
	.add {
		display: grid;
		gap: 1rem;
	}

	.steps {
		margin: 0;
		padding-left: 1.5rem;
		display: grid;
		gap: 1.25rem;
	}

	.steps > li {
		padding-left: 0.25rem;
	}

	.steps > li > * + * {
		margin-top: 0.6rem;
	}

	.steps > li::marker {
		font-family: var(--font-display);
		font-weight: var(--weight-semibold);
	}

	.joiners {
		list-style: none;
		margin: 0.6rem 0 0;
		padding: 0;
		display: flex;
		flex-wrap: wrap;
		gap: 0.5rem;
	}

	.joiner {
		flex-direction: column;
		align-items: flex-start;
		gap: 0.1rem;
	}

	.joiner[aria-pressed='true'] {
		border-color: var(--action);
		box-shadow: inset 0 0 0 1px var(--action);
	}

	.progress p + p {
		margin-top: 0.25rem;
	}

	.eui {
		width: 12.5rem;
		max-width: 100%;
	}

	.grow {
		flex: 1 1 10rem;
	}

	@media (max-width: 640px) {
		form .field {
			flex: 1 1 100%;
		}

		.eui {
			width: 100%;
		}
	}
</style>
