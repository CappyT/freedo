<script lang="ts">
	import { page } from '$app/state';
	import { resolve } from '$app/paths';
	import { act } from '#lib/actions.ts';
	import { alertKeys, modeLine, targetText } from '#lib/describe.ts';
	import Dial from '#lib/Dial.svelte';
	import { buildDevices } from '#lib/devices.ts';
	import DeviceList from '#lib/DeviceList.svelte';
	import { data } from '#lib/data.svelte.ts';
	import EndRuleField from '#lib/EndRuleField.svelte';
	import ConfirmDialog from '#lib/ConfirmDialog.svelte';
	import { defaultEndRule, timerSeconds, validTimer } from '#lib/endrule.ts';
	import { fmt, t } from '#lib/i18n.svelte.ts';
	import Icon from '#lib/Icon.svelte';
	import SchedulePanel from '#lib/SchedulePanel.svelte';
	import { HEATING_RANGE } from '#lib/schedule.ts';
	import { api } from '#lib/session.ts';
	import StatusBadge from '#lib/StatusBadge.svelte';
	import type { EndRule } from '#lib/types.ts';
	import { targetIsTemp, zoneName } from '#lib/zones.ts';

	const range = HEATING_RANGE;
	const number = $derived(Number(page.params.n));
	const model = $derived(data.heating.find((m) => m.zone.number === number));
	const devices = $derived(
		buildDevices(data.nodes, data.health, data.models).filter((d) => model?.zone.members.includes(d.short))
	);

	let draft = $state<number | null>(null);
	let touched = $state(false);
	// The rule the person chose. Until then the page offers the default of the zone.
	let chosen = $state<EndRule | null>(null);
	const rule = $derived(chosen ?? defaultEndRule('heating', model?.zone.target.hasProgram));
	let confirmOff: ConfirmDialog | undefined = $state();
	let hours = $state(1);
	let minutes = $state(0);
	let busy = $state(false);

	const shown = $derived(
		draft ?? model?.zone.target.targetC ?? model?.zone.target.programC ?? model?.tempC ?? 20
	);
	const end = $derived({ mode: rule, durationSec: timerSeconds(hours, minutes) });
	const endOk = $derived(rule !== 'timer' || validTimer(end.durationSec));
	const windowState = $derived(model?.zone.window);
	const offRule = $derived(rule === 'timer' ? `${t('ends.timer')}, ${fmt.duration(end.durationSec)}` : t(`ends.${rule}`));

	// Follow the zone until the user moves the dial.
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
	<a class="back small" href={resolve('/')}><Icon name="back" size={16} />{t('zone.back')}</a>

	{#if !data.loaded}
		<p class="muted">{t('state.loading')}</p>
	{:else if !model}
		<div class="panel">
			<h1>{t('zone.missing.title')}</h1>
			<p>{t('zone.missing.body')}</p>
		</div>
	{:else}
		<div class="page-head titled">
			<h1>{zoneName(model.zone, t('zone.hotWaterName'))}</h1>
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
			<section class="panel control" aria-labelledby="control-title">
				<header>
					<h2 id="control-title">{t('zone.control')}</h2>
					<span class="small muted">{modeLine(model.zone.target)}</span>
				</header>

				<div class="dialbox">
					<Dial
						current={model.tempC}
						target={shown}
						min={range.min}
						max={range.max}
						step={range.step}
						tone={model.status.tone}
						interactive
						label={t('zone.targetLabel')}
						caption={t('card.now')}
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
					name="end-rule"
					noProgram={model.zone.target.hasProgram === false}
				/>

				<div class="commands">
					<button class="btn primary" disabled={busy || !endOk} onclick={() => run(() => api.setpoint(number, shown, end))}>
						<Icon name="check" size={18} />{t('zone.apply', { temp: fmt.temp(shown) })}
					</button>
					<button class="btn" disabled={busy || !endOk} onclick={() => confirmOff?.show()}>
						<Icon name="power" size={18} />{t('zone.off')}
					</button>
					<button class="btn" disabled={busy} onclick={() => run(() => api.auto(number))}>
						<Icon name="calendar" size={18} />{t('zone.auto')}
					</button>
					<button class="btn" disabled={busy} onclick={() => run(() => api.boost(number, !model.zone.target.boost))}>
						<Icon name="bolt" size={18} />{model.zone.target.boost ? t('zone.boostStop') : t('zone.boostStart')}
					</button>
				</div>
			</section>

			<section class="panel" aria-labelledby="state-title">
				<header><h2 id="state-title">{t('zone.state')}</h2></header>
				<dl class="facts">
					<div><dt>{t('card.now')}</dt><dd>{fmt.temp(model.tempC)}</dd></div>
					<div><dt>{t('card.target')}</dt><dd class:text={!targetIsTemp(model.zone.target)}>{targetText(model)}</dd></div>
					<div><dt>{t('card.humidity')}</dt><dd>{fmt.percent(model.humidityPct)}</dd></div>
					<div><dt>{t('zone.demand')}</dt><dd>{fmt.percent(model.demandPct)}</dd></div>
					{#if model.zone.target.hasProgram !== false}
						<div><dt>{t('zone.scheduled')}</dt><dd class:text={model.zone.target.programOff}>{model.zone.target.programOff ? t('schedule.off') : fmt.temp(model.zone.target.programC)}</dd></div>
					{/if}
				</dl>

				<div class="window">
					<div class="row">
						<h3>{t('window.title')}</h3>
						{#if windowState}
							<span class="badge tone-{windowState.open ? 'warning' : 'idle'}">{windowState.open ? t('window.open') : t('window.closed')}</span>
							{#if windowState.open && windowState.deadline}
								<span class="small muted">{t('window.until', { time: fmt.when(windowState.deadline) })}</span>
							{/if}
						{/if}
					</div>
					{#if windowState}
						<div>
							<button class="btn" disabled={busy} onclick={() => run(() => api.window(number, !windowState.open))}>
								<Icon name="window" size={18} />{windowState.open ? t('window.markClosed') : t('window.markOpen')}
							</button>
						</div>
						<p class="note">{t('window.note')}</p>
					{/if}
				</div>
			</section>
		</div>

		<section class="panel" aria-labelledby="devices-title">
			<header><h2 id="devices-title">{t('zone.devices')}</h2></header>
			<DeviceList {devices} />
		</section>

		<SchedulePanel zone={number} {range} />
	{/if}
</section>

<ConfirmDialog
	bind:this={confirmOff}
	title={t('zone.off.confirm.title', { zone: model ? zoneName(model.zone, t('zone.hotWaterName')) : '' })}
	body={t('zone.off.confirm.body', { rule: offRule })}
	confirm={t('zone.off')}
	onconfirm={() => run(() => api.off(number, end))}
/>

<style>
	/* A 44 px target; the negative margin keeps the link close to the title it belongs to. */
	.back {
		display: inline-flex;
		align-items: center;
		gap: 0.25rem;
		width: fit-content;
		min-height: 2.75rem;
		margin-block: -0.5rem -0.75rem;
	}

	.window {
		display: grid;
		gap: 0.75rem;
		padding-top: 1rem;
		border-top: 1px solid var(--border);
	}

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
