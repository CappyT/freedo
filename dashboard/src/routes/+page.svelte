<script lang="ts">
	import { resolve } from '$app/paths';
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
	let menuOpen = $state(false);
	let menuRoot: HTMLDivElement | undefined = $state();

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

	/** Switches the active profile. The menu shows the profile the daemon reports. */
	async function activate(id: number) {
		menuOpen = false;
		if (id === activePreset?.id || busy) return;
		busy = true;
		await act(() => api.activatePreset(id));
		busy = false;
	}

	function closeOutside(event: MouseEvent) {
		if (menuOpen && menuRoot && !menuRoot.contains(event.target as Node)) menuOpen = false;
	}

	const boostAll = () => actMany(heating.map((m) => () => api.boost(m.zone.number, true)));
	const resumeAll = () => actMany(zones.map((m) => () => api.auto(m.zone.number)));
</script>

<section class="page" aria-labelledby="title">
	<div class="page-head">
		<h1 id="title">{t('home.title')}</h1>
		<div class="row actions fill">
			{#if presets.length > 0}
				<div class="profile-menu" bind:this={menuRoot}>
					<button class="btn menu-button" aria-haspopup="menu" aria-expanded={menuOpen} disabled={busy} onclick={() => (menuOpen = !menuOpen)}>
						<Icon name="calendar" size={18} />
						<span class="muted">{t('preset.active')}</span>
						<strong>{activePreset?.name ?? '-'}</strong>
						<span class="caret"><Icon name="chevron" size={16} /></span>
					</button>
					{#if menuOpen}
						<div class="menu" role="menu" aria-label={t('home.profileMenu')}>
							{#each presets as preset (preset.id)}
								<button class="item" role="menuitemradio" aria-checked={preset.active} onclick={() => activate(preset.id)}>
									<span class="radio" aria-hidden="true"></span>
									<span class="grow">{preset.name}</span>
									{#if preset.active}<span class="badge tone-heating">{t('presets.activeBadge')}</span>{/if}
								</button>
							{/each}
							<hr />
							<a class="item" role="menuitem" href={resolve('/programs?new=1')}><Icon name="plus" size={18} />{t('home.newProfile')}</a>
							<a class="item" role="menuitem" href={resolve('/programs')}><Icon name="edit" size={18} />{t('home.editPrograms')}</a>
						</div>
					{/if}
				</div>
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
			<div class="row"><a class="btn primary" href={resolve('/setup')}>{t('setup.start')}</a></div>
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

<svelte:window onclick={closeOutside} onkeydown={(e) => e.key === 'Escape' && (menuOpen = false)} />

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

	.profile-menu {
		position: relative;
	}

	.menu-button strong {
		font-weight: var(--weight-semibold);
	}

	.caret {
		display: inline-flex;
		rotate: 90deg;
	}

	.menu {
		position: absolute;
		z-index: 30;
		top: calc(100% + 0.4rem);
		left: 0;
		min-width: max(100%, 17rem);
		display: grid;
		gap: 0.15rem;
		padding: 0.4rem;
		background: var(--surface);
		border: 1px solid var(--border-strong);
		border-radius: var(--radius-lg);
		box-shadow: 0 0.75rem 2rem color-mix(in srgb, var(--navy) 22%, transparent);
	}

	.menu hr {
		width: 100%;
		margin: 0.3rem 0;
		border: 0;
		border-top: 1px solid var(--border);
	}

	.item {
		display: flex;
		align-items: center;
		gap: 0.6rem;
		min-height: 2.75rem;
		padding: 0 0.75rem;
		border: 0;
		border-radius: var(--radius-md);
		background: none;
		color: var(--text);
		font-size: var(--text-small-size);
		font-weight: var(--weight-medium);
		text-align: left;
		text-decoration: none;
		cursor: pointer;
	}

	.item:hover {
		background: var(--panel);
	}

	.item .grow {
		flex: 1 1 auto;
	}

	.radio {
		flex: none;
		width: 1rem;
		height: 1rem;
		border-radius: 50%;
		border: 2px solid var(--border-strong);
	}

	.item[aria-checked='true'] .radio {
		border: 5px solid var(--action);
	}

	/* On a phone the header controls fill the width, like the buttons of a .row.fill. */
	@media (max-width: 899px) {
		.actions .segmented {
			flex: 1 1 auto;
		}

		.actions .segmented label {
			flex: 1 1 0;
			justify-content: center;
		}
	}

	@media (max-width: 559px) {
		.profile-menu,
		.actions .segmented {
			flex: 1 1 100%;
		}

		.menu-button {
			width: 100%;
		}

		.menu-button strong {
			flex: 1 1 auto;
			text-align: left;
		}
	}

	fieldset {
		margin: 0;
		padding: 0;
		min-width: 0;
	}
</style>
