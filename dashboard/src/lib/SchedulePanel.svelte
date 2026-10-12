<script lang="ts">
	import { resolve } from '$app/paths';
	import { untrack } from 'svelte';
	import { act, errorText } from './actions.ts';
	import ConfirmDialog from './ConfirmDialog.svelte';
	import { data } from './data.svelte.ts';
	import { t } from './i18n.svelte.ts';
	import Icon from './Icon.svelte';
	import ScheduleEditor from './ScheduleEditor.svelte';
	import ScheduleWeek from './ScheduleWeek.svelte';
	import {
		defaultDraft,
		fromView,
		programInForce,
		sameSchedule,
		toPrograms,
		validate,
		type Group,
		type Mode,
		type Range
	} from './schedule.ts';
	import { api } from './session.ts';

	interface Props {
		zone: number;
		range: Range;
		hotWater?: boolean;
		/** The profile the panel reads and writes. Absent, the active one. */
		preset?: number;
		/** The program the panel opens on. */
		initialMode?: Mode;
		/** Called after a save or a delete, so a parent can read the profile again. */
		onsaved?: () => void;
	}

	let { zone, range, hotWater = false, preset, initialMode = 'home', onsaved }: Props = $props();

	/** The home program and the away program of the profile. Null while the zone holds none. */
	let stored = $state<Record<Mode, Group[] | null>>({ home: null, away: null });
	let mode = $state<Mode>('home');
	let loading = $state(true);
	let failure = $state<string | null>(null);
	let editing = $state(false);
	let saving = $state(false);
	let draft = $state<Group[]>([]);
	let confirmDelete: ConfirmDialog | undefined = $state();

	const current = $derived(stored[mode]);
	const issues = $derived(validate(draft, range));
	const changed = $derived(!current || !sameSchedule(draft, current));
	const inForce = $derived(programInForce(data.away, stored.away !== null));
	const profile = $derived(data.presets.find((p) => (preset === undefined ? p.active : p.id === preset)));
	const query = $derived({ preset });

	async function load() {
		loading = true;
		failure = null;
		try {
			const [home, away] = await Promise.all([
				api.schedule(zone, { preset }),
				api.schedule(zone, { mode: 'away', preset })
			]);
			stored = { home: home ? fromView(home) : null, away: away ? fromView(away) : null };
		} catch (error) {
			failure = errorText(error);
		}
		loading = false;
	}

	// The parent passes zone as a getter, so reading the prop tracks everything the parent expression
	// reads, such as the zone model that each poll replaces. The derived value changes only with the number.
	const key = $derived(`${zone}:${preset ?? ''}`);

	$effect(() => {
		void key;
		editing = false;
		mode = untrack(() => initialMode);
		// load reads the API token before its first await: untrack keeps a token change from reloading.
		void untrack(load);
	});

	function switchTo(next: Mode) {
		mode = next;
		editing = false;
	}

	/** Opens the editor. seed is the away program created as a copy of the home one. */
	function edit(seed?: Group[]) {
		const base = seed ?? stored[mode] ?? defaultDraft(range, hotWater);
		draft = structuredClone($state.snapshot(base));
		editing = true;
	}

	async function save() {
		saving = true;
		const view = await act(() => api.putSchedule(zone, toPrograms(draft), { ...query, mode }), t('schedule.saved'));
		saving = false;
		if (view) {
			stored = { ...stored, [mode]: fromView(view) };
			editing = false;
			onsaved?.();
		}
	}

	async function removeAway() {
		const view = await act(() => api.deleteSchedule(zone, { ...query, mode: 'away' }), t('schedule.awayDeleted'));
		if (view) {
			stored = { ...stored, away: null };
			onsaved?.();
		}
	}
</script>

<section class="panel" aria-labelledby="schedule-title">
	<header>
		<h2 id="schedule-title">{t('schedule.title')}</h2>
		{#if !loading && !failure && profile?.active}
			<span class="inforce row">
				<span class="small muted">{t('schedule.inForce')}</span>
				<span class="badge" class:tone-away={inForce === 'away'} class:tone-idle={inForce === 'home'}>
					{t(`schedule.${inForce}`)}
				</span>
			</span>
		{/if}
	</header>

	{#if profile}
		<p class="profile row small">
			<span class="badge" class:tone-heating={profile.active} class:tone-off={!profile.active}>
				{t('schedule.profile', { name: profile.name })}{profile.active ? `, ${t('schedule.profileActive')}` : ''}
			</span>
			{#if preset === undefined}
				<a href={resolve('/programs')}>{t('schedule.otherProfiles')}</a>
			{/if}
		</p>
	{/if}

	{#if loading}
		<p class="muted">{t('state.loading')}</p>
	{:else if failure}
		<p class="notice tone-fault" role="alert"><Icon name="fault" /><span>{failure}</span></p>
		<div><button class="btn" onclick={load}><Icon name="refresh" size={18} />{t('action.retry')}</button></div>
	{:else}
		<fieldset class="segmented" disabled={saving}>
			<legend class="sr-only">{t('schedule.mode')}</legend>
			<label>
				<input type="radio" name={`schedule-mode-${zone}`} checked={mode === 'home'} onchange={() => switchTo('home')} />
				{t('schedule.home')}
			</label>
			<label>
				<input type="radio" name={`schedule-mode-${zone}`} checked={mode === 'away'} onchange={() => switchTo('away')} />
				{t('schedule.away')}
			</label>
		</fieldset>

		{#if editing}
			<ScheduleEditor bind:groups={draft} {range} {issues} />
			<ScheduleWeek groups={draft} {range} />
			<div class="row">
				<button class="btn primary" onclick={save} disabled={saving || issues.length > 0 || !changed}>
					<Icon name="check" size={18} />{t('schedule.save')}
				</button>
				<button class="btn" onclick={() => (editing = false)} disabled={saving}>{t('action.cancel')}</button>
			</div>
		{:else if mode === 'away' && !stored.away}
			<p class="muted">{t(hotWater ? 'schedule.noAwayHotWater' : 'schedule.noAway')}</p>
			<div class="row">
				<button class="btn primary" onclick={() => edit()}>
					<Icon name="plus" size={18} />{t('schedule.createAway')}
				</button>
				{#if stored.home}
					<button class="btn" onclick={() => edit(stored.home ?? undefined)}>
						<Icon name="calendar" size={18} />{t('schedule.copyHome')}
					</button>
				{/if}
			</div>
		{:else}
			<div class="row">
				<button class="btn" onclick={() => edit()}>
					<Icon name="calendar" size={18} />{current ? t('schedule.edit') : t('schedule.create')}
				</button>
				{#if mode === 'away' && stored.away}
					<button class="btn danger" onclick={() => confirmDelete?.show()}>
						<Icon name="trash" size={18} />{t('schedule.deleteAway')}
					</button>
				{/if}
			</div>
			{#if current}
				<ScheduleWeek groups={current} {range} />
			{:else}
				<p class="muted">{t('schedule.none')}</p>
			{/if}
		{/if}
	{/if}
</section>

<ConfirmDialog
	bind:this={confirmDelete}
	title={t('schedule.deleteAway.confirm.title')}
	body={t(hotWater ? 'schedule.deleteAway.confirm.bodyHotWater' : 'schedule.deleteAway.confirm.body')}
	confirm={t('schedule.deleteAway')}
	onconfirm={removeAway}
/>

<style>
	.inforce {
		align-items: center;
		gap: 0.5rem;
	}

	.segmented {
		margin-bottom: 0.25rem;
	}

	.profile {
		margin-top: -0.25rem;
	}
</style>
