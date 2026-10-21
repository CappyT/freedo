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
	import { DAYS, fromPrograms, HEATING_RANGE, HOT_WATER_RANGE, type Day, type Mode } from '#lib/schedule.ts';
	import { api } from '#lib/session.ts';
	import { clockNow } from '#lib/today.svelte.ts';
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
	// The program and the day the rooms show. The day starts on today of the installation.
	let mode = $state<Mode>('home');
	let pickedDay = $state<Day | null>(null);
	const now = $derived(clockNow());
	const day = $derived(pickedDay ?? now.day);

	let form = $state<'new' | 'rename' | null>(query.has('new') ? 'new' : null);
	let name = $state('');
	// Empty means the active profile, the source the daemon copies by default.
	let copyFrom = $state('');
	let confirmActivate: ConfirmDialog | undefined = $state();
	let confirmDelete: ConfirmDialog | undefined = $state();
	let activateBody = $state('');
	let activateItems = $state<string[]>([]);

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
			activateBody = changes.length === 0 ? t('programs.activate.none') : t('programs.activate.changes');
			activateItems = changes.map((c) => `${zoneName(c, t('zone.hotWaterName'))}: ${blockText(c.from)} → ${blockText(c.to)}`);
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
	<h1 id="programs-title">{t('programs.title')}</h1>

	{#if !data.loaded}
		<p class="muted">{t('state.loading')}</p>
	{:else}
		<div class="chips" role="group" aria-label={t('programs.profiles')}>
			{#each profiles as profile (profile.id)}
				<button class="chip" class:selected={profile.id === selectedId} aria-pressed={profile.id === selectedId} onclick={() => choose(profile.id)}>
					{profile.name}
					{#if profile.active}<span class="active"><Icon name="check" size={14} />{t('presets.activeBadge')}</span>{/if}
				</button>
			{/each}
			<button class="chip add" onclick={() => openNew()}><Icon name="plus" size={16} />{t('programs.new')}</button>
		</div>
		<p class="note">{t('presets.note')}</p>

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
			{#if !selected.active && active}
				<div class="notice tone-away editing">
					<Icon name="calendar" />
					<div class="editing-body">
						<span>{t('programs.editing', { name: selected.name, active: active.name })}</span>
						<div class="profile-actions">
							<button class="btn primary" disabled={busy} onclick={askActivate}><Icon name="check" size={18} />{t('programs.activate')}</button>
							<button class="btn" onclick={openRename}><Icon name="edit" size={18} />{t('presets.rename')}</button>
							<button class="btn" onclick={() => selected && openNew(selected)}><Icon name="plus" size={18} />{t('programs.duplicate')}</button>
							<button class="btn danger" onclick={() => confirmDelete?.show()}><Icon name="trash" size={18} />{t('presets.delete')}</button>
						</div>
					</div>
				</div>
			{:else}
				<div class="links">
					<button class="link" onclick={openRename}><Icon name="edit" size={16} />{t('presets.rename')}</button>
					<button class="link" onclick={() => selected && openNew(selected)}><Icon name="plus" size={16} />{t('programs.duplicate')}</button>
				</div>
			{/if}

			<fieldset class="segmented wide">
				<legend class="sr-only">{t('schedule.mode')}</legend>
				<label><input type="radio" name="programs-mode" checked={mode === 'home'} onchange={() => (mode = 'home')} /><Icon name="home" size={16} />{t('schedule.home')}</label>
				<label><input type="radio" name="programs-mode" checked={mode === 'away'} onchange={() => (mode = 'away')} /><Icon name="away" size={16} />{t('schedule.away')}</label>
			</fieldset>

			<div class="days" role="group" aria-label={t('programs.day')}>
				{#each DAYS as d (d)}
					<button class="day" class:today={d === now.day} aria-pressed={d === day} onclick={() => (pickedDay = d)}>
						<span aria-hidden="true">{fmt.day(d).slice(0, 1).toUpperCase()}</span>
						<span class="sr-only">{fmt.day(d, 'long')}{d === now.day ? `, ${t('programs.today')}` : ''}</span>
					</button>
				{/each}
			</div>

			<section class="section" aria-labelledby="selected-title">
				<div class="section-head">
					<h2 id="selected-title">{selected.name}</h2>
					<span class="badge" class:tone-idle={selected.active} class:tone-off={!selected.active}>
						{selected.active ? t('presets.activeBadge') : t('programs.inactive')}
					</span>
				</div>
				{#if loading && !view}
					<p class="muted">{t('state.loading')}</p>
				{:else if failure}
					<p class="notice tone-fault" role="alert"><Icon name="fault" /><span>{failure}</span></p>
				{:else if view}
					<div class="matrix list" aria-label={selected.name}>
						<div class="axis" aria-hidden="true"><span>00</span><span>06</span><span>12</span><span>18</span><span>24</span></div>
						{#each view.zones as z (z.zone)}
							{@const program = mode === 'home' ? z.home : z.away}
							<div class="zone">
								<div class="room">
									<strong>{zoneName(z, t('zone.hotWaterName'))}</strong>
									<button class="link" aria-expanded={editingZone === z.zone} onclick={() => (editingZone = editingZone === z.zone ? null : z.zone)}>
										<Icon name={editingZone === z.zone ? 'close' : 'edit'} size={16} />{editingZone === z.zone ? t('programs.close') : t('programs.edit')}
									</button>
								</div>
								{#if program}
									<ScheduleWeek groups={fromPrograms(program)} range={rangeOf(z)} only={day} now={day === now.day ? now.minute : undefined} axis={false} />
								{:else}
									<p class="small muted">
										{mode === 'home' ? t('programs.noSchedule') : z.kind === 'hotwater' ? t('programs.awayOffHotWater') : t('programs.awayFallback')}
									</p>
								{/if}
								{#if editingZone === z.zone && selectedId !== null}
									<div class="editor">
										<SchedulePanel zone={z.zone} range={rangeOf(z)} hotWater={z.kind === 'hotwater'} preset={selectedId} initialMode={mode} onsaved={reload} />
									</div>
								{/if}
							</div>
						{/each}
					</div>
				{/if}
			</section>
		{/if}
	{/if}
</section>

<ConfirmDialog
	bind:this={confirmActivate}
	title={t('programs.activate.title', { name: selected?.name ?? '' })}
	body={activateBody}
	items={activateItems}
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
	.chips {
		display: flex;
		flex-wrap: wrap;
		gap: 0.5rem;
	}

	.chip {
		display: inline-flex;
		align-items: center;
		gap: 0.4rem;
		min-height: 2.75rem;
		padding: 0 1rem;
		border: 1px solid var(--border-strong);
		border-radius: var(--radius-pill);
		background: var(--surface);
		color: var(--text);
		font-size: var(--text-small-size);
		font-weight: var(--weight-semibold);
		cursor: pointer;
	}

	.chip:hover {
		border-color: var(--accent);
	}

	.chip.selected {
		background: var(--action);
		border-color: var(--action);
		color: var(--action-text);
	}

	.chip.add {
		border-style: dashed;
		color: var(--link);
	}

	.active {
		display: inline-flex;
		align-items: center;
		gap: 0.2rem;
		height: 1.375rem;
		padding: 0 0.5rem;
		border-radius: var(--radius-pill);
		background: var(--status-idle-tint);
		color: var(--status-idle-text);
		font-size: 0.75rem;
	}

	.selected .active {
		background: color-mix(in srgb, var(--white) 22%, transparent);
		color: var(--white);
	}

	.page > .note {
		margin-top: -0.5rem;
	}

	.grow {
		flex: 1 1 14rem;
	}

	.editing {
		align-items: flex-start;
	}

	.editing-body {
		display: grid;
		gap: 0.75rem;
	}

	.profile-actions {
		display: flex;
		flex-wrap: wrap;
		gap: 0.5rem;
	}

	.links {
		display: flex;
		flex-wrap: wrap;
		gap: 0.25rem;
		margin: -0.5rem 0 -0.25rem;
	}

	.link {
		display: inline-flex;
		align-items: center;
		gap: 0.35rem;
		min-height: 2.75rem;
		padding: 0 0.5rem;
		border: 0;
		border-radius: var(--radius-md);
		background: none;
		color: var(--link);
		font-size: var(--text-small-size);
		font-weight: var(--weight-semibold);
		cursor: pointer;
	}

	.link:hover {
		background: var(--panel);
	}

	.segmented.wide {
		display: grid;
		grid-template-columns: repeat(2, minmax(0, 1fr));
		width: 100%;
		max-width: 24rem;
	}

	.days {
		display: grid;
		grid-template-columns: repeat(7, minmax(0, 2.75rem));
		justify-content: space-between;
		gap: 0.25rem;
		max-width: 24rem;
	}

	.day {
		position: relative;
		display: grid;
		place-items: center;
		height: 2.75rem;
		border: 0;
		border-radius: 50%;
		background: none;
		color: var(--text);
		font-family: var(--font-display);
		font-size: 0.9375rem;
		font-weight: var(--weight-semibold);
		cursor: pointer;
	}

	.day:hover {
		background: var(--panel);
	}

	.day[aria-pressed='true'] {
		background: var(--text);
		color: var(--ground);
	}

	.day.today::after {
		content: '';
		position: absolute;
		bottom: 3px;
		width: 5px;
		height: 5px;
		border-radius: 50%;
		background: var(--accent);
	}

	.matrix {
		padding-top: 0.5rem;
	}

	.axis {
		display: flex;
		justify-content: space-between;
		padding: 0 0.875rem;
		font-family: var(--font-mono);
		font-size: var(--text-caption-size);
		color: var(--text-muted);
	}

	.matrix > .axis::before {
		display: none;
	}

	.zone {
		display: grid;
		gap: 0.5rem;
		padding: 0.625rem 0.875rem 0.875rem;
	}

	.room {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 0.75rem;
	}

	.room strong {
		font-family: var(--font-display);
		font-weight: var(--weight-semibold);
	}

	.room .link {
		margin: -0.5rem -0.5rem -0.5rem 0;
	}

	.editor {
		padding-top: 0.5rem;
	}

	@media (max-width: 639px) {
		/* The fields take whole lines, so the two buttons of the form share the last one. */
		form .field {
			flex: 1 1 100%;
		}
	}
</style>
