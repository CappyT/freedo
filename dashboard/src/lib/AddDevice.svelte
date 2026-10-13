<script lang="ts">
	import { onMount } from 'svelte';
	import { errorText } from './actions.ts';
	import { data } from './data.svelte.ts';
	import Icon from './Icon.svelte';
	import { t } from './i18n.svelte.ts';
	import { manage } from './install.svelte.ts';
	import { countdown, joinedSince, joinerPaired, wireEui } from './install.ts';
	import { api } from './session.ts';
	import { toasts } from './toast.svelte.ts';
	import type { PairJoiner, PairWindow } from './types.ts';
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

	interface Props {
		/** Closes the flow. Without it the flow has no Done action, as in the first setup. */
		onclose?: () => void;
	}

	let { onclose }: Props = $props();

	let pairWindow = $state<PairWindow | null>(null);
	let problem = $state<string | null>(null);
	let eui = $state('');
	let kind = $state<'valve' | 'thermostat'>('valve');
	let label = $state('');
	let room = $state('');
	let busy = $state(false);
	let pending = $state<Pending | null>(null);
	/** The device whose pairing ended. While it is set, the flow shows the end and no step. */
	let done = $state<{ short: string; name: string } | null>(null);
	let now = $state(Date.now());

	const health = $derived(data.health);
	const zones = $derived(data.zones?.declared ?? []);
	const target = $derived(wireEui(eui));
	const joiners = $derived(pairWindow?.joiners ?? []);
	// The daemon lists a declared device in the health view, by its short address.
	const declared = (short: string) => health?.devices.find((d) => d.short === short);
	const paired = (joiner: PairJoiner) => joinerPaired(joiner, data.nodes.find((n) => n.Short === joiner.short));
	const doneRooms = $derived(
		data.models.filter((m) => done !== null && m.zone.members.includes(done.short)).map((m) => zoneName(m.zone, t('zone.hotWaterName')))
	);
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
			pairWindow = await api.pairWindow();
			problem = null;
		} catch (error) {
			problem = errorText(error);
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
		done = { short: waiting.short, name: waiting.name };
		toasts.push('ok', t('pair.joined', { name: waiting.name }));
		void api.openPairWindow(0).then((view) => (pairWindow = view), () => undefined);
	});

	async function setWindow(seconds: number) {
		busy = true;
		const view = await manage(() => api.openPairWindow(seconds), seconds > 0 ? t('pair.opened') : t('pair.closed'));
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
				address = (await api.createDevice({ eui: target, kind, label: name })).device.short;
				if (zone !== null) await api.addMember(zone, target);
			}
			await api.pair(target);
			return address;
		}, t('pair.sent', { name }));
		busy = false;
		if (short === undefined) return;
		const node = data.nodes.find((n) => n.Short === short);
		pending = { eui: target, short, name, before: { serial: node?.Serial ?? '', sessionUnix: node?.SessionIssuedUnix ?? 0 } };
		eui = '';
		label = '';
	}

	async function retry() {
		const waiting = pending;
		if (!waiting) return;
		busy = true;
		await manage(() => api.pair(waiting.eui), t('pair.sent', { name: waiting.name }));
		busy = false;
	}
</script>

{#snippet windowControl()}
	{#if pairWindow?.open}
		<span class="badge tone-heating" data-testid="pair-left">{t('pair.left', { time: left ?? '-' })}</span>
		<button class="btn" type="button" disabled={busy} onclick={() => setWindow(0)}>{t('pair.close')}</button>
	{:else}
		<button class="btn primary" type="button" disabled={busy} onclick={() => setWindow(WINDOW_SEC)}>
			<Icon name="bolt" size={18} />{t('pair.open')}
		</button>
	{/if}
{/snippet}

<div class="add" data-testid="add-device">
	{#if health && !health.transmitAllowed}
		<p class="notice tone-warning" role="status"><Icon name="warning" /><span>{t('pair.noTx')}</span></p>
	{/if}

	{#if done}
		<div class="end" role="status" data-testid="pair-done">
			<h3><Icon name="check" />{t('pair.done.title', { name: done.name })}</h3>
			<p>{t('pair.done.body')}</p>
			<p>{doneRooms.length > 0 ? t('pair.done.room', { room: doneRooms.join(', ') }) : t('pair.done.noRoom')}</p>
			<div class="row fill">
				{#if onclose}<button class="btn primary" type="button" onclick={onclose}>{t('setup.done')}</button>{/if}
				<button class="btn" class:primary={!onclose} type="button" onclick={() => (done = null)}>
					<Icon name="plus" size={18} />{t('pair.another')}
				</button>
			</div>
		</div>
	{:else if pending}
		<div class="notice tone-warning progress" role="status" data-testid="pair-progress">
			<Icon name="clock" />
			<div>
				<p><strong>{t('pair.progress', { name: pending.name })}</strong></p>
				<progress aria-hidden="true"></progress>
				<p>{pairWindow?.open ? t('pair.progress.body') : t('pair.progress.closed')}</p>
			</div>
		</div>
		<div class="row fill">
			{@render windowControl()}
			<button class="btn primary" type="button" disabled={busy || !pairWindow?.open} onclick={retry}>
				<Icon name="refresh" size={18} />{t('pair.retry')}
			</button>
			<button class="btn" type="button" disabled={busy} onclick={() => (pending = null)}>{t('pair.stop')}</button>
		</div>
		{#if problem}<p class="note">{problem}</p>{/if}
	{:else}
		<ol class="steps">
			<li>
				<h3>{t('pair.step.window')}</h3>
				<p class="small muted">{t('pair.step.window.body')}</p>
				<div class="row fill">{@render windowControl()}</div>
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
								{#if device && paired(joiner)}
									<div class="joiner paired">
										<span class="mono">{joiner.eui}</span>
										<span class="small muted">{t('pair.joiner.paired', { name: device.label || device.device })}</span>
									</div>
								{:else}
									<button class="btn joiner" type="button" aria-pressed={joiner.eui === target} onclick={() => (eui = joiner.eui)}>
										<span class="mono">{joiner.eui}</span>
										<span class="small muted">{device ? t('pair.joiner.known', { name: device.label || device.device }) : t('pair.joiner.new')}</span>
									</button>
								{/if}
							</li>
						{/each}
					</ul>
				{/if}
			</li>

			<li>
				<h3>{t('pair.step.describe')}</h3>
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
			</li>
		</ol>
	{/if}
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

	.joiner.paired {
		display: flex;
		padding: 0.5rem 0.9rem;
		border: 1px dashed var(--border-strong);
		border-radius: var(--radius-md);
	}

	.progress progress {
		display: block;
		width: min(16rem, 100%);
		margin: 0.5rem 0;
		accent-color: var(--action);
	}

	.end {
		display: grid;
		gap: 0.6rem;
	}

	.end h3 {
		display: flex;
		align-items: center;
		gap: 0.5rem;
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
