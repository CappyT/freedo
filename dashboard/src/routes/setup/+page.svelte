<script lang="ts">
	import { resolve } from '$app/paths';
	import { onMount } from 'svelte';
	import { errorText } from '#lib/actions.ts';
	import AddDevice from '#lib/AddDevice.svelte';
	import CircuitForm from '#lib/CircuitForm.svelte';
	import ConfirmDialog from '#lib/ConfirmDialog.svelte';
	import { refusal } from '#lib/backup.ts';
	import { data } from '#lib/data.svelte.ts';
	import { t } from '#lib/i18n.svelte.ts';
	import Icon from '#lib/Icon.svelte';
	import { install, manage } from '#lib/install.svelte.ts';
	import { setupState, type StepState } from '#lib/install.ts';
	import RoomForm from '#lib/RoomForm.svelte';
	import { api, calibrateAgain } from '#lib/session.ts';
	import { toasts } from '#lib/toast.svelte.ts';
	import { zoneName } from '#lib/zones.ts';

	const health = $derived(data.health);
	const zones = $derived(data.zones?.declared ?? []);
	const steps = $derived(setupState(health, zones));
	const allDone = $derived(
		[steps.radio, steps.timezone, steps.rooms, steps.transmit, steps.devices, steps.circuits].every((s) => s === 'done')
	);

	// The zone in the field. It starts on the zone of the installation, else on the zone of the browser.
	let zoneDraft = $state<string | null>(null);
	const timezone = $derived(zoneDraft ?? (install.info?.timezone || Intl.DateTimeFormat().resolvedOptions().timeZone));
	// The daemon names a set utc_offset_min "fixed offset", in English.
	const zoneLabel = $derived(health?.timezone?.name === 'fixed offset' ? t('settings.timezone.fixed') : (health?.timezone?.name ?? ''));
	let busy = $state(false);
	let calibrateBusy = $state(false);
	let calibrateDialog: ConfirmDialog | undefined = $state();

	onMount(() => void install.load());

	async function saveTimezone(event: SubmitEvent) {
		event.preventDefault();
		const name = timezone.trim();
		if (name === '') return;
		busy = true;
		const done = await manage(() => api.setTimezone(name), t('setup.timezone.saved'));
		busy = false;
		if (done) zoneDraft = null;
	}

	// A refusal is a state of this step, not of the page: the token gate stays shut.
	let calibrateError = $state<string | null>(null);

	async function calibrate() {
		calibrateBusy = true;
		calibrateError = null;
		try {
			await calibrateAgain();
			toasts.push('ok', t('setup.radio.again.started'));
		} catch (error) {
			const why = refusal(error);
			if (why === 'admin') calibrateError = t('setup.radio.again.adminOnly');
			else if (why === 'token') calibrateError = t('setup.radio.again.needsToken');
			else toasts.push('error', errorText(error));
		}
		await Promise.all([data.refresh(), install.load()]);
		calibrateBusy = false;
	}
</script>

{#snippet badge(step: StepState, outside = false)}
	<span class="badge" class:tone-idle={step === 'done'} class:tone-warning={step === 'todo'}>
		{step === 'done' ? t('setup.done') : t('setup.todo')}
	</span>
	{#if outside}<span class="small muted">{t('setup.outside')}</span>{/if}
{/snippet}

<section class="page">
	<div class="page-head">
		<h1>{t('setup.title')}</h1>
		<a class="btn" href={resolve('/devices')}><Icon name="devices" size={18} />{t('devices.title')}</a>
	</div>
	<p class="lead">{t('setup.lead')}</p>

	{#if !data.loaded}
		<p class="muted">{t('state.loading')}</p>
	{:else}
		<ol class="steps">
			<li class="panel" data-step="radio" data-state={steps.radio}>
				<header><h2>{t('setup.radio')}</h2><div class="row">{@render badge(steps.radio, true)}</div></header>
				{#if steps.radioProblem === 'noRadio'}
					<p>{t('setup.radio.none')}</p>
				{:else}
					{#if steps.calibration === 'measuring'}
						<p>{t('setup.radio.measuring', { readings: health?.calibration?.readings ?? 0, want: health?.calibration?.want ?? 0 })}</p>
						<p class="small muted">{t('setup.radio.measuring.hint')}</p>
					{:else}
						<p>{t('setup.radio.ok')}</p>
					{/if}
					<div class="row">
						<button class="btn" type="button" disabled={calibrateBusy} onclick={() => calibrateDialog?.show()}>
							<Icon name="refresh" size={18} />{t('setup.radio.again')}
						</button>
					</div>
					{#if calibrateError}
						<p class="notice tone-fault" role="alert" data-testid="calibrate-error"><Icon name="fault" /><span>{calibrateError}</span></p>
					{/if}
				{/if}
			</li>

			<li class="panel" data-step="timezone" data-state={steps.timezone}>
				<header><h2>{t('settings.timezone')}</h2><div class="row">{@render badge(steps.timezone)}</div></header>
				<p>{steps.timezone === 'done' ? t('setup.timezone.ok', { name: zoneLabel }) : t('setup.timezone.body')}</p>
				<form class="row fill" onsubmit={saveTimezone}>
					<label class="field grow">
						<span>{t('setup.timezone.name')}</span>
						<input class="input" value={timezone} oninput={(e) => (zoneDraft = e.currentTarget.value)} placeholder="Europe/Rome" autocomplete="off" spellcheck="false" />
					</label>
					<button class="btn primary" type="submit" disabled={busy || timezone.trim() === ''}>
						<Icon name="check" size={18} />{t('setup.timezone.save')}
					</button>
				</form>
			</li>

			<li class="panel" data-step="rooms" data-state={steps.rooms}>
				<header><h2>{t('setup.rooms')}</h2><div class="row">{@render badge(steps.rooms)}</div></header>
				<p>{t('setup.rooms.body')}</p>
				{#if zones.length > 0}
					<ul class="tags">
						{#each zones as zone (zone.number)}<li class="badge tone-off">{zoneName(zone, t('zone.hotWaterName'))}</li>{/each}
					</ul>
				{/if}
				<RoomForm />
			</li>

			<li class="panel" data-step="transmit" data-state={steps.transmit}>
				<header><h2>{t('setup.transmit')}</h2><div class="row">{@render badge(steps.transmit, true)}</div></header>
				{#if steps.transmitProblem === 'bridge'}
					<p>{t('setup.transmit.bridge')}</p>
				{:else if steps.transmitProblem === 'dryRun'}
					<p>{t('setup.transmit.dryRun')}</p>
				{:else}
					<p>{t('setup.transmit.ok')}</p>
				{/if}
			</li>

			<li class="panel" data-step="devices" data-state={steps.devices}>
				<header><h2>{t('setup.devices')}</h2><div class="row">{@render badge(steps.devices)}</div></header>
				<p>{t('setup.devices.body', { n: health?.devices.length ?? 0 })}</p>
				<AddDevice />
			</li>

			<li class="panel" data-step="circuits" data-state={steps.circuits}>
				<header><h2>{t('circuits.title')}</h2><div class="row">{@render badge(steps.circuits)}</div></header>
				<p>{t('circuits.note')}</p>
				{#if data.circuits.length > 0}
					<ul class="tags">
						{#each data.circuits as circuit (circuit.ID)}<li class="badge tone-off">{circuit.Name || t('boiler.circuit', { id: circuit.ID })}</li>{/each}
					</ul>
				{/if}
				<CircuitForm />
			</li>
		</ol>

		<section class="panel" data-step="finish">
			<header><h2>{t('setup.finish')}</h2></header>
			<p>{allDone ? t('setup.finish.done') : t('setup.finish.open')}</p>
			{#if (install.info?.warnings.length ?? 0) > 0}
				<p class="note">{t('install.checks.note')}</p>
				<ul class="checks small">
					{#each install.info?.warnings ?? [] as warning (warning)}<li>{warning}</li>{/each}
				</ul>
			{/if}
			<div class="row fill">
				<a class="btn primary" href={resolve('/')}><Icon name="home" size={18} />{t('setup.finish.home')}</a>
				<a class="btn" href={resolve('/devices')}><Icon name="devices" size={18} />{t('devices.title')}</a>
			</div>
		</section>
	{/if}
</section>

<ConfirmDialog
	bind:this={calibrateDialog}
	title={t('setup.radio.again.title')}
	body={t('setup.radio.again.body')}
	confirm={t('setup.radio.again')}
	onconfirm={() => void calibrate()}
/>

<style>
	.lead {
		max-width: 46rem;
	}

	.steps {
		list-style: none;
		counter-reset: step;
		margin: 0;
		padding: 0;
		display: grid;
		gap: 1.5rem;
	}

	.steps > li {
		counter-increment: step;
	}

	.steps > li > header h2::before {
		content: counter(step) '. ';
		color: var(--text-muted);
	}

	.steps > li > header {
		align-items: center;
	}

	.tags {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-wrap: wrap;
		gap: 0.4rem;
	}

	.checks {
		margin: 0;
		padding-left: 1.25rem;
		display: grid;
		gap: 0.35rem;
		overflow-wrap: anywhere;
	}

	.grow {
		flex: 1 1 14rem;
	}
</style>
