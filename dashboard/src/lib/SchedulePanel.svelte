<script lang="ts">
	import { act, errorText } from './actions.ts';
	import { t } from './i18n.svelte.ts';
	import Icon from './Icon.svelte';
	import ScheduleEditor from './ScheduleEditor.svelte';
	import ScheduleWeek from './ScheduleWeek.svelte';
	import {
		defaultDraft,
		fromView,
		sameSchedule,
		toPrograms,
		validate,
		type Group,
		type Range
	} from './schedule.ts';
	import { api } from './session.ts';

	let { zone, range, hotWater = false }: { zone: number; range: Range; hotWater?: boolean } = $props();

	let stored = $state<Group[] | null>(null);
	let loading = $state(true);
	let failure = $state<string | null>(null);
	let editing = $state(false);
	let saving = $state(false);
	let draft = $state<Group[]>([]);

	const issues = $derived(validate(draft, range));
	const changed = $derived(stored === null || !sameSchedule(draft, stored));

	async function load() {
		loading = true;
		failure = null;
		try {
			const view = await api.schedule(zone);
			stored = view ? fromView(view) : null;
		} catch (error) {
			failure = errorText(error);
		}
		loading = false;
	}

	$effect(() => {
		void zone;
		editing = false;
		void load();
	});

	function edit() {
		draft = stored ? structuredClone($state.snapshot(stored)) : defaultDraft(range, hotWater);
		editing = true;
	}

	async function save() {
		saving = true;
		const view = await act(() => api.putSchedule(zone, toPrograms(draft)), t('schedule.saved'));
		saving = false;
		if (view) {
			stored = fromView(view);
			editing = false;
		}
	}
</script>

<section class="panel" aria-labelledby="schedule-title">
	<header>
		<h2 id="schedule-title">{t('schedule.title')}</h2>
		{#if !editing && !loading && !failure}
			<button class="btn" onclick={edit}>
				<Icon name="calendar" size={18} />{stored ? t('schedule.edit') : t('schedule.create')}
			</button>
		{/if}
	</header>

	{#if loading}
		<p class="muted">{t('state.loading')}</p>
	{:else if failure}
		<p class="notice tone-fault" role="alert"><Icon name="fault" /><span>{failure}</span></p>
		<div><button class="btn" onclick={load}><Icon name="refresh" size={18} />{t('action.retry')}</button></div>
	{:else if editing}
		<ScheduleEditor bind:groups={draft} {range} {issues} />
		<ScheduleWeek groups={draft} {range} />
		<div class="row">
			<button class="btn primary" onclick={save} disabled={saving || issues.length > 0 || !changed}>
				<Icon name="check" size={18} />{t('schedule.save')}
			</button>
			<button class="btn" onclick={() => (editing = false)} disabled={saving}>{t('action.cancel')}</button>
		</div>
	{:else if stored}
		<ScheduleWeek groups={stored} {range} />
	{:else}
		<p class="muted">{t('schedule.none')}</p>
	{/if}
</section>
