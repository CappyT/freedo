<script lang="ts">
	import { actMany, act } from '#lib/actions.ts';
	import ConfirmDialog from '#lib/ConfirmDialog.svelte';
	import { data } from '#lib/data.svelte.ts';
	import { t } from '#lib/i18n.svelte.ts';
	import Icon from '#lib/Icon.svelte';
	import { api } from '#lib/session.ts';
	import ZoneCard from '#lib/ZoneCard.svelte';

	let confirmBoost: ConfirmDialog | undefined = $state();
	let busy = $state(false);
	let homeRadio: HTMLInputElement | undefined = $state();
	let awayRadio: HTMLInputElement | undefined = $state();
	let presetSelect: HTMLSelectElement | undefined = $state();

	const zones = $derived(data.models);
	const heating = $derived(data.heating);
	const presets = $derived(data.presets);
	const activePreset = $derived(presets.find((preset) => preset.active));

	/** Shows the presence that the daemon reports. A radio keeps the click when the request fails. */
	function showPresence() {
		if (homeRadio) homeRadio.checked = !data.away;
		if (awayRadio) awayRadio.checked = data.away;
	}

	async function setAway(value: boolean) {
		if (value === data.away || busy) return;
		busy = true;
		const done = await act(() => api.setAway(value));
		busy = false;
		if (!done) showPresence();
	}

	/** Switches the active preset. The select returns to the active one when the request fails. */
	async function activate() {
		if (!presetSelect || busy) return;
		const id = Number(presetSelect.value);
		if (id === activePreset?.id) return;
		busy = true;
		const done = await act(() => api.activatePreset(id));
		busy = false;
		if (!done && presetSelect) presetSelect.value = String(activePreset?.id ?? '');
	}

	const boostAll = () => actMany(heating.map((m) => () => api.boost(m.zone.number, true)));
	const resumeAll = () => actMany(zones.map((m) => () => api.auto(m.zone.number)));
</script>

<section class="page" aria-labelledby="title">
	<div class="page-head">
		<h1 id="title">{t('home.title')}</h1>
		<div class="row actions fill">
			{#if presets.length > 0}
				<label class="field inline preset">
					<span>{t('preset.active')}</span>
					<select class="input" bind:this={presetSelect} value={String(activePreset?.id ?? '')} onchange={activate} disabled={busy}>
						{#each presets as preset (preset.id)}
							<option value={String(preset.id)}>{preset.name}</option>
						{/each}
					</select>
				</label>
			{/if}
			<fieldset class="segmented" disabled={busy || !data.home}>
				<legend class="sr-only">{t('home.presence')}</legend>
				<label>
					<input type="radio" name="presence" bind:this={homeRadio} checked={!data.away} onchange={() => setAway(false)} />
					<Icon name="home" size={18} />{t('home.atHome')}
				</label>
				<label>
					<input type="radio" name="presence" bind:this={awayRadio} checked={data.away} onchange={() => setAway(true)} />
					<Icon name="away" size={18} />{t('home.away')}
				</label>
			</fieldset>
			<button class="btn" onclick={() => confirmBoost?.show()} disabled={heating.length === 0}>
				<Icon name="bolt" size={18} />{t('home.boostAll')}
			</button>
			<button class="btn" onclick={resumeAll} disabled={zones.length === 0}>
				<Icon name="calendar" size={18} />{t('home.resumeAll')}
			</button>
		</div>
	</div>

	{#if !data.loaded}
		<p class="muted">{t('state.loading')}</p>
	{:else if zones.length === 0}
		<div class="panel">
			<h2>{t('home.empty.title')}</h2>
			<p>{t('home.empty.body')}</p>
			{#if data.problem}<p class="note mono">{data.problem}</p>{/if}
		</div>
	{:else}
		<ul class="grid">
			{#each zones as model (model.zone.number)}
				<li><ZoneCard {model} /></li>
			{/each}
		</ul>
	{/if}
</section>

<ConfirmDialog
	bind:this={confirmBoost}
	title={t('home.boostAll')}
	body={t('home.boostAll.body')}
	confirm={t('home.boostAll')}
	onconfirm={boostAll}
/>

<style>
	.grid {
		display: grid;
		grid-template-columns: repeat(auto-fill, minmax(min(100%, 17.5rem), 1fr));
		gap: 1rem;
		list-style: none;
		margin: 0;
		padding: 0;
	}

	.grid li {
		display: grid;
	}

	.actions {
		gap: 0.75rem;
	}

	.preset select {
		min-width: 9rem;
	}

	fieldset {
		margin: 0;
		padding: 0;
		min-width: 0;
	}
</style>
