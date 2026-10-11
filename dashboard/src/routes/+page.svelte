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

	const zones = $derived(data.models);
	const heating = $derived(data.heating);

	async function setAway(away: boolean) {
		if (away === data.away || busy) return;
		busy = true;
		await act(() => api.setAway(away));
		busy = false;
	}

	const boostAll = () => actMany(heating.map((m) => () => api.boost(m.zone.number, true)));
	const resumeAll = () => actMany(zones.map((m) => () => api.auto(m.zone.number)));
</script>

<section class="page" aria-labelledby="title">
	<div class="page-head">
		<h1 id="title">{t('home.title')}</h1>
		<div class="row actions">
			<fieldset class="segmented" disabled={busy || !data.home}>
				<legend class="sr-only">{t('home.presence')}</legend>
				<label>
					<input type="radio" name="presence" checked={!data.away} onchange={() => setAway(false)} />
					<Icon name="home" size={18} />{t('home.atHome')}
				</label>
				<label>
					<input type="radio" name="presence" checked={data.away} onchange={() => setAway(true)} />
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

	fieldset {
		margin: 0;
		padding: 0;
		min-width: 0;
	}

	@media (max-width: 899px) {
		.actions :global(.btn) {
			flex: 1 1 auto;
		}
	}
</style>
