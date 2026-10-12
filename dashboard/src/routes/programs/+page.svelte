<script lang="ts">
	import { page } from '$app/state';
	import { untrack } from 'svelte';
	import { act, errorText } from '#lib/actions.ts';
	import ConfirmDialog from '#lib/ConfirmDialog.svelte';
	import { data } from '#lib/data.svelte.ts';
	import { fmt, t } from '#lib/i18n.svelte.ts';
	import Icon from '#lib/Icon.svelte';
	import { changedZones } from '#lib/programs.ts';
	import SchedulePanel from '#lib/SchedulePanel.svelte';
	import ScheduleWeek from '#lib/ScheduleWeek.svelte';
	import { fromPrograms, HEATING_RANGE, HOT_WATER_RANGE } from '#lib/schedule.ts';
	import { api } from '#lib/session.ts';
	import type { PresetSchedules, PresetZoneSchedules } from '#lib/types.ts';
	import { zoneName } from '#lib/zones.ts';

	const profiles = $derived(data.presets);
	const active = $derived(profiles.find((p) => p.active));
	// The route lives in the fragment (#/programs?new=1), so the query is read from there.
	const query = new URLSearchParams(page.url.hash.split('?')[1] ?? '');
	// The profile the person picked. Until then the page shows the one the link names, else the active one.
	let chosen = $state<number | null>(Number(query.get('profile')) || null);
	const selectedId = $derived(chosen ?? active?.id ?? null);
	const selected = $derived(profiles.find((p) => p.id === selectedId));

	let view = $state<PresetSchedules | null>(null);
	let loading = $state(false);
	let failure = $state<string | null>(null);
	let editingZone = $state<number | null>(null);
	let busy = $state(false);

	let form = $state<'new' | 'rename' | null>(query.has('new') ? 'new' : null);
	let name = $state('');
	// Empty means the active profile, the source the daemon copies by default.
	let copyFrom = $state('');
	let confirmActivate: ConfirmDialog | undefined = $state();
	let confirmDelete: ConfirmDialog | undefined = $state();
	let activateBody = $state('');

	async function load(id: number) {
		loading = true;
		failure = null;
		try {
			view = await api.presetSchedules(id);
		} catch (error) {
			failure = errorText(error);
		}
		loading = false;
	}

	// The data poll replaces the profile list every 5 seconds. selectedId is a number, so the
	// effect runs again only when the selection changes.
	$effect(() => {
		const id = selectedId;
		editingZone = null;
		if (id !== null) void untrack(() => load(id));
	});

	const reload = () => (selectedId !== null ? load(selectedId) : undefined);

	function choose(id: number) {
		chosen = id;
		form = null;
	}

	function openNew(source?: { id: number; name: string }) {
		form = 'new';
		name = source ? t('programs.copyName', { name: source.name }) : '';
		copyFrom = source ? String(source.id) : '';
	}

	function openRename() {
		if (!selected) return;
		form = 'rename';
		name = selected.name;
	}

	async function submit(event: SubmitEvent) {
		event.preventDefault();
		const clean = name.trim();
		if (clean === '' || busy) return;
		busy = true;
		if (form === 'new') {
			const source = copyFrom === '' ? undefined : Number(copyFrom);
			const created = await act(() => api.createPreset(clean, source), t('presets.created'));
			if (created) {
				form = null;
				chosen = created.id;
			}
		} else if (form === 'rename' && selected) {
			const id = selected.id;
			if (await act(() => api.renamePreset(id, clean), t('presets.renamed'))) {
				form = null;
				await reload();
			}
		}
		busy = false;
	}

	function blockText(block: PresetZoneSchedules['inForce']): string {
		if (block === null) return '-';
		return block.off ? t('schedule.off') : fmt.temp(block.setpointC);
	}

	/** Reads both profiles again, so the confirmation names the rooms that change at this moment. */
	async function askActivate() {
		if (!selected || !active || busy) return;
		busy = true;
		try {
			const [now, next] = await Promise.all([api.presetSchedules(active.id), api.presetSchedules(selected.id)]);
			const changes = changedZones(now, next);
			activateBody =
				changes.length === 0
					? t('programs.activate.none')
					: t('programs.activate.changes', {
							rooms: changes
								.map((c) => `${zoneName(c, t('zone.hotWaterName'))} ${blockText(c.from)} -> ${blockText(c.to)}`)
								.join(', ')
						});
			confirmActivate?.show();
		} catch (error) {
			failure = errorText(error);
		}
		busy = false;
	}

	async function activate() {
		if (!selected) return;
		const id = selected.id;
		busy = true;
		if (await act(() => api.activatePreset(id), t('programs.activated'))) await reload();
		busy = false;
	}

	async function remove() {
		if (!selected) return;
		const id = selected.id;
		if (await act(() => api.deletePreset(id), t('presets.deleted'))) chosen = null;
	}

	const rangeOf = (z: PresetZoneSchedules) => (z.kind === 'hotwater' ? HOT_WATER_RANGE : HEATING_RANGE);
</script>

<section class="page" aria-labelledby="programs-title">
	<div class="page-head">
		<h1 id="programs-title">{t('programs.title')}</h1>
		<button class="btn" onclick={() => openNew()}><Icon name="plus" size={18} />{t('programs.new')}</button>
	</div>

	{#if !data.loaded}
		<p class="muted">{t('state.loading')}</p>
	{:else}
		<div class="layout">
			<section class="panel list" aria-labelledby="profiles-title">
				<h2 id="profiles-title">{t('programs.profiles')}</h2>
				<ul class="profiles">
					{#each profiles as profile (profile.id)}
						<li>
							<button class="profile" class:selected={profile.id === selectedId} aria-pressed={profile.id === selectedId} onclick={() => choose(profile.id)}>
								<span class="name">{profile.name}</span>
								{#if profile.active}<span class="badge tone-heating">{t('presets.activeBadge')}</span>{/if}
							</button>
						</li>
					{/each}
				</ul>
				<p class="note">{t('presets.note')}</p>
			</section>

			<div class="main">
				{#if form}
					<section class="panel" aria-labelledby="form-title">
						<h2 id="form-title">{form === 'new' ? t('programs.new') : t('presets.rename')}</h2>
						<form class="row fill" onsubmit={submit}>
							<label class="field grow">
								<span>{t('presets.name')}</span>
								<input class="input" bind:value={name} placeholder={t('presets.nameHint')} autocomplete="off" spellcheck="false" />
							</label>
							{#if form === 'new'}
								<label class="field">
									<span>{t('presets.copyFrom')}</span>
									<select class="input" bind:value={copyFrom}>
										<option value="">{t('presets.copyActive')}</option>
										{#each profiles as profile (profile.id)}<option value={String(profile.id)}>{profile.name}</option>{/each}
									</select>
								</label>
							{/if}
							<button class="btn primary" type="submit" disabled={busy || name.trim() === ''}>
								<Icon name="check" size={18} />{form === 'new' ? t('presets.create') : t('presets.save')}
							</button>
							<button class="btn" type="button" onclick={() => (form = null)}>{t('action.cancel')}</button>
						</form>
					</section>
				{/if}

				{#if selected}
					<section class="panel" aria-labelledby="selected-title">
						<header>
							<div class="row">
								<h2 id="selected-title">{selected.name}</h2>
								<span class="badge" class:tone-heating={selected.active} class:tone-off={!selected.active}>
									{selected.active ? t('presets.activeBadge') : t('programs.inactive')}
								</span>
							</div>
						</header>
						<div class="commands">
							{#if !selected.active}
								<button class="btn primary" disabled={busy} onclick={askActivate}><Icon name="check" size={18} />{t('programs.activate')}</button>
							{/if}
							<button class="btn" onclick={openRename}><Icon name="edit" size={18} />{t('presets.rename')}</button>
							<button class="btn" onclick={() => selected && openNew(selected)}><Icon name="plus" size={18} />{t('programs.duplicate')}</button>
							{#if !selected.active}
								<button class="btn danger" onclick={() => confirmDelete?.show()}><Icon name="trash" size={18} />{t('presets.delete')}</button>
							{/if}
						</div>
						{#if !selected.active && active}
							<p class="notice tone-away"><Icon name="calendar" /><span>{t('programs.editing', { name: selected.name, active: active.name })}</span></p>
						{/if}

						{#if loading && !view}
							<p class="muted">{t('state.loading')}</p>
						{:else if failure}
							<p class="notice tone-fault" role="alert"><Icon name="fault" /><span>{failure}</span></p>
						{:else if view}
							<div class="matrix" role="table" aria-label={selected.name}>
								<div class="head" role="row">
									<span role="columnheader">{t('programs.room')}</span>
									<span role="columnheader">{t('programs.homeSchedule')}</span>
									<span role="columnheader">{t('programs.awaySchedule')}</span>
								</div>
								{#each view.zones as z (z.zone)}
									<div class="zone" role="row">
										<div class="room" role="cell">
											<strong>{zoneName(z, t('zone.hotWaterName'))}</strong>
											<button class="btn ghost" aria-expanded={editingZone === z.zone} onclick={() => (editingZone = editingZone === z.zone ? null : z.zone)}>
												<Icon name={editingZone === z.zone ? 'close' : 'edit'} size={16} />{editingZone === z.zone ? t('programs.close') : t('programs.edit')}
											</button>
										</div>
										<div role="cell">
											<span class="label">{t('programs.homeSchedule')}</span>
											{#if z.home}
												<ScheduleWeek groups={fromPrograms(z.home)} range={rangeOf(z)} compact />
											{:else}
												<span class="small muted">{t('programs.noSchedule')}</span>
											{/if}
										</div>
										<div role="cell">
											<span class="label">{t('programs.awaySchedule')}</span>
											{#if z.away}
												<ScheduleWeek groups={fromPrograms(z.away)} range={rangeOf(z)} compact />
											{:else}
												<span class="small muted">{z.kind === 'hotwater' ? t('programs.awayOffHotWater') : t('programs.awayFallback')}</span>
											{/if}
										</div>
									</div>
									{#if editingZone === z.zone && selectedId !== null}
										<div class="editor">
											<SchedulePanel zone={z.zone} range={rangeOf(z)} hotWater={z.kind === 'hotwater'} preset={selectedId} onsaved={reload} />
										</div>
									{/if}
								{/each}
							</div>
						{/if}
					</section>
				{/if}
			</div>
		</div>
	{/if}
</section>

<ConfirmDialog
	bind:this={confirmActivate}
	title={t('programs.activate.title', { name: selected?.name ?? '' })}
	body={activateBody}
	confirm={t('programs.activate')}
	onconfirm={activate}
/>

<ConfirmDialog
	bind:this={confirmDelete}
	title={t('presets.delete.confirm.title', { name: selected?.name ?? '' })}
	body={t('presets.delete.confirm.body')}
	confirm={t('presets.delete')}
	onconfirm={remove}
/>

<style>
	.layout {
		display: grid;
		grid-template-columns: 15rem minmax(0, 1fr);
		gap: 1.25rem;
		align-items: start;
	}

	.main {
		display: grid;
		gap: 1.25rem;
		min-width: 0;
	}

	.profiles {
		list-style: none;
		margin: 0;
		padding: 0;
		display: grid;
		gap: 0.25rem;
	}

	.profile {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		width: 100%;
		min-height: 2.75rem;
		padding: 0.4rem 0.75rem;
		border: 1px solid transparent;
		border-radius: var(--radius-md);
		background: none;
		text-align: left;
		cursor: pointer;
	}

	.profile:hover {
		background: var(--panel);
	}

	.profile.selected {
		background: var(--panel);
		border-color: var(--accent);
	}

	.profile .name {
		flex: 1 1 auto;
		font-weight: var(--weight-medium);
	}

	.grow {
		flex: 1 1 14rem;
	}

	.matrix {
		display: grid;
	}

	.head,
	.zone {
		display: grid;
		grid-template-columns: minmax(9rem, 0.8fr) minmax(0, 1.5fr) minmax(0, 1.5fr);
		gap: 1rem;
		align-items: start;
	}

	.head {
		padding-bottom: 0.5rem;
		font-size: var(--text-small-size);
		font-weight: var(--weight-medium);
		color: var(--text-muted);
	}

	.zone {
		padding: 0.85rem 0;
		border-top: 1px solid var(--border);
	}

	.zone > div {
		display: grid;
		gap: 0.35rem;
		min-width: 0;
	}

	.room {
		justify-items: start;
	}

	.label {
		display: none;
		font-size: var(--text-small-size);
		color: var(--text-muted);
	}

	.btn.ghost {
		min-height: 2.25rem;
		padding: 0.25rem 0.5rem;
		border-color: transparent;
		background: none;
		color: var(--link);
	}

	.editor {
		padding-bottom: 1rem;
	}

	@media (max-width: 899px) {
		.layout {
			grid-template-columns: minmax(0, 1fr);
		}

		.profiles {
			display: flex;
			flex-wrap: wrap;
		}

		.profile {
			width: auto;
			border-color: var(--border-strong);
		}
	}

	@media (max-width: 639px) {
		.head {
			display: none;
		}

		.zone {
			grid-template-columns: minmax(0, 1fr);
			gap: 0.6rem;
		}

		.zone > .room {
			display: flex;
			justify-content: space-between;
			align-items: center;
		}

		.label {
			display: block;
		}
	}
</style>
