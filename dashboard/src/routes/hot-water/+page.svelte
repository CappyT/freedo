<script lang="ts">
	import { act } from '#lib/actions.ts';
	import { alertKeys, modeLine } from '#lib/describe.ts';
	import Dial from '#lib/Dial.svelte';
	import { data } from '#lib/data.svelte.ts';
	import EndRuleField from '#lib/EndRuleField.svelte';
	import { defaultEndRule, timerSeconds, validTimer } from '#lib/endrule.ts';
	import { fmt, t } from '#lib/i18n.svelte.ts';
	import Icon from '#lib/Icon.svelte';
	import SchedulePanel from '#lib/SchedulePanel.svelte';
	import { HOT_WATER_RANGE } from '#lib/schedule.ts';
	import { api } from '#lib/session.ts';
	import StatusBadge from '#lib/StatusBadge.svelte';
	import type { EndRule } from '#lib/types.ts';

	const range = HOT_WATER_RANGE;
	const model = $derived(data.hotWater);

	let draft = $state<number | null>(null);
	let touched = $state(false);
	// The rule the person chose. Until then the page offers the default of the zone.
	let chosen = $state<EndRule | null>(null);
	const rule = $derived(chosen ?? defaultEndRule('hotwater', model?.zone.target.hasProgram));
	let hours = $state(1);
	let minutes = $state(0);
	let busy = $state(false);

	const shown = $derived(draft ?? model?.zone.target.targetC ?? model?.zone.target.programC ?? 50);
	const end = $derived({ mode: rule, durationSec: timerSeconds(hours, minutes) });
	const endOk = $derived(rule !== 'timer' || validTimer(end.durationSec));

	$effect(() => {
		const target = model?.zone.target;
		if (!touched) draft = target?.targetC ?? target?.programC ?? null;
	});

	function move(value: number) {
		draft = value;
		touched = true;
	}

	async function run(command: () => Promise<unknown>) {
		busy = true;
		const result = await act(command);
		busy = false;
		if (result) touched = false;
	}
</script>

<section class="page">
	{#if !data.loaded}
		<p class="muted">{t('state.loading')}</p>
	{:else if !model}
		<div class="panel">
			<h1>{t('hotWater.missing.title')}</h1>
			<p>{t('hotWater.missing.body')}</p>
		</div>
	{:else}
		<div class="page-head">
			<h1>{model.zone.name}</h1>
			<StatusBadge state={model.status.state} tone={model.status.tone} />
		</div>
		{#if model.status.alerts.length > 0}
			<ul class="alerts">
				{#each model.status.alerts as alert (alert)}
					<li class="badge tone-warning">{t(alertKeys[alert])}</li>
				{/each}
			</ul>
		{/if}

		<div class="cols">
			<section class="panel" aria-labelledby="control-title">
				<header>
					<h2 id="control-title">{t('zone.control')}</h2>
					<span class="small muted">{modeLine(model.zone.target)}</span>
				</header>
				<div class="dialbox">
					<Dial
						current={null}
						center={shown}
						target={shown}
						min={range.min}
						max={range.max}
						step={range.step}
						tone={model.status.tone}
						interactive
						label={t('hotWater.targetLabel')}
						caption={t('card.target')}
						onchange={move}
					/>
					<div class="nudge row">
						<button class="btn icon" aria-label={t('zone.lower')} onclick={() => move(Math.max(range.min, shown - range.step))} disabled={shown <= range.min}>
							<Icon name="minus" />
						</button>
						<output class="wanted mono" aria-live="polite">{fmt.temp(shown)}</output>
						<button class="btn icon" aria-label={t('zone.raise')} onclick={() => move(Math.min(range.max, shown + range.step))} disabled={shown >= range.max}>
							<Icon name="plus" />
						</button>
					</div>
				</div>

				<EndRuleField
					bind:rule={() => rule, (value) => (chosen = value)}
					bind:hours
					bind:minutes
					name="dhw-end-rule"
					noProgram={model.zone.target.hasProgram === false}
				/>

				<div class="row">
					<button class="btn primary" disabled={busy || !endOk} onclick={() => run(() => api.setpoint(model.zone.number, shown, end))}>
						<Icon name="power" size={18} />{t('hotWater.on', { temp: fmt.temp(shown) })}
					</button>
					<button class="btn" disabled={busy || !endOk} onclick={() => run(() => api.off(model.zone.number, end))}>
						<Icon name="power" size={18} />{t('hotWater.off')}
					</button>
					<button class="btn" disabled={busy} onclick={() => run(() => api.auto(model.zone.number))}>
						<Icon name="calendar" size={18} />{t('zone.auto')}
					</button>
				</div>
			</section>

			<section class="panel" aria-labelledby="state-title">
				<header><h2 id="state-title">{t('zone.state')}</h2></header>
				<dl class="facts">
					<div><dt>{t('hotWater.power')}</dt><dd class="text">{model.zone.target.on ? t('state.on') : t('state.off')}</dd></div>
					<div><dt>{t('card.target')}</dt><dd class="text">{model.zone.target.on ? fmt.temp(model.zone.target.targetC) : t('target.off')}</dd></div>
					{#if model.zone.target.hasProgram !== false}
						<div><dt>{t('zone.scheduled')}</dt><dd>{model.zone.target.programOff ? t('schedule.off') : fmt.temp(model.zone.target.programC)}</dd></div>
					{/if}
					<div><dt>{t('boiler.dhw')}</dt><dd>{fmt.temp(data.boiler?.DHWC)}</dd></div>
					<div><dt>{t('hotWater.reported')}</dt><dd>{fmt.temp(data.boiler?.DHWSetpointC)}</dd></div>
				</dl>
				<p class="note">{t('hotWater.note')}</p>
			</section>
		</div>

		<SchedulePanel zone={model.zone.number} {range} hotWater />
	{/if}
</section>

<style>
	.alerts {
		display: flex;
		flex-wrap: wrap;
		gap: 0.4rem;
		list-style: none;
		margin: 0;
		padding: 0;
	}

	.cols {
		display: grid;
		grid-template-columns: minmax(0, 1.25fr) minmax(0, 1fr);
		gap: 1.5rem;
		align-items: start;
	}

	.dialbox {
		display: grid;
		justify-items: center;
		gap: 0.75rem;
	}

	.wanted {
		min-width: 6rem;
		text-align: center;
		font-size: 1.25rem;
		font-weight: var(--weight-medium);
	}

	@media (max-width: 899px) {
		.cols {
			grid-template-columns: minmax(0, 1fr);
		}
	}
</style>
