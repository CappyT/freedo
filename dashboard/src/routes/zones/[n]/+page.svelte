<script lang="ts">
	import { page } from '$app/state';
	import { resolve } from '$app/paths';
	import { act } from '#lib/actions.ts';
	import AirComfort from '#lib/AirComfort.svelte';
	import { alertKeys, modeLine } from '#lib/describe.ts';
	import Dial from '#lib/Dial.svelte';
	import { buildDevices } from '#lib/devices.ts';
	import DeviceList from '#lib/DeviceList.svelte';
	import { data } from '#lib/data.svelte.ts';
	import EndRuleField from '#lib/EndRuleField.svelte';
	import ConfirmDialog from '#lib/ConfirmDialog.svelte';
	import { defaultEndRule, timerSeconds, validTimer } from '#lib/endrule.ts';
	import { fmt, t } from '#lib/i18n.svelte.ts';
	import Icon, { type IconName } from '#lib/Icon.svelte';
	import SchedulePanel from '#lib/SchedulePanel.svelte';
	import ScheduleWeek from '#lib/ScheduleWeek.svelte';
	import { HEATING_RANGE, programInForce, type Group, type Mode } from '#lib/schedule.ts';
	import { api } from '#lib/session.ts';
	import { stateIcons, nextText, todayOf } from '#lib/today.svelte.ts';
	import type { EndRule } from '#lib/types.ts';
	import { zoneName } from '#lib/zones.ts';

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
	let programs = $state<Record<Mode, Group[] | null>>({ home: null, away: null });

	const shown = $derived(
		draft ?? model?.zone.target.targetC ?? model?.zone.target.programC ?? model?.tempC ?? 20
	);
	const end = $derived({ mode: rule, durationSec: timerSeconds(hours, minutes) });
	const endOk = $derived(rule !== 'timer' || validTimer(end.durationSec));
	const windowState = $derived(model?.zone.window);
	const ruleText = $derived(rule === 'timer' ? `${t('ends.timer')}, ${fmt.duration(end.durationSec)}` : t(`ends.${rule}`));
	// A change waits for the person to apply it. A zone that is off offers to switch on at the value shown.
	const pending = $derived(model !== undefined && ((touched && draft !== model.zone.target.targetC) || !model.zone.target.on));

	const inForce = $derived(programs[programInForce(data.away, programs.away !== null)]);
	const today = $derived(todayOf(inForce));
	const following = $derived(
		model !== undefined && model.zone.target.source === 'program' && model.zone.target.overlay === null && model.zone.target.on
	);
	const mode = $derived.by(() => {
		if (!model) return '';
		const line = modeLine(model.zone.target);
		const next = following ? nextText(today?.next ?? null) : null;
		return next ? `${line} · ${next}` : line;
	});

	// Follow the zone until the user moves the dial.
	$effect(() => {
		const target = model?.zone.target;
		if (!touched) draft = target?.targetC ?? target?.programC ?? null;
	});

	function move(value: number) {
		draft = value;
		touched = true;
	}

	function cancel() {
		touched = false;
		draft = model?.zone.target.targetC ?? model?.zone.target.programC ?? null;
	}

	async function run(command: () => Promise<unknown>) {
		busy = true;
		const result = await act(command);
		busy = false;
		if (result) touched = false;
	}

	const commands = $derived<{ icon: IconName; label: string; run: () => void; disabled: boolean }[]>(
		model
			? [
					{ icon: 'power', label: t('zone.off'), run: () => confirmOff?.show(), disabled: busy || !endOk },
					{
						icon: 'bolt',
						label: model.zone.target.boost ? t('zone.boostStop') : t('zone.boostStart'),
						run: () => run(() => api.boost(number, !model.zone.target.boost)),
						disabled: busy
					},
					{ icon: 'calendar', label: t('zone.auto'), run: () => run(() => api.auto(number)), disabled: busy }
				]
			: []
	);
</script>

<section class="page">
	{#if !data.loaded}
		<p class="muted">{t('state.loading')}</p>
	{:else if !model}
		<a class="section-link" href={resolve('/')}><Icon name="back" size={16} />{t('zone.back')}</a>
		<div class="panel">
			<h1>{t('zone.missing.title')}</h1>
			<p>{t('zone.missing.body')}</p>
		</div>
	{:else}
		<div class="screen">
			<section class="hero fill-{model.status.tone}" aria-labelledby="room-title">
				<div class="hero-bar">
					<a class="round" href={resolve('/')} aria-label={t('zone.back')}><Icon name="back" size={22} /></a>
					<h1 id="room-title">{model.zone.name}</h1>
					<a class="round" href="#schedule-title" aria-label={t('schedule.title')}><Icon name="calendar" size={20} /></a>
				</div>
				<span class="hero-state"><Icon name={stateIcons[model.status.state]} size={16} />{t(`state.${model.status.state}`)}</span>
				<p class="hero-facts">
					<span>{t('card.humidity')} <b>{fmt.pct(model.humidityPct)}</b></span>
					<span>{t('zone.demandShort')} <b>{fmt.pct(model.demandPct)}</b></span>
				</p>
				<Dial
					current={model.tempC}
					target={shown}
					min={range.min}
					max={range.max}
					step={range.step}
					interactive
					label={t('zone.targetLabel')}
					caption={t('card.now')}
					onchange={move}
					size="17rem"
				/>
				<div class="hero-nudge">
					<button class="round" aria-label={t('zone.lower')} onclick={() => move(Math.max(range.min, shown - range.step))} disabled={shown <= range.min}>
						<Icon name="minus" size={22} />
					</button>
					<div class="hero-target">
						<!-- Always one decimal, like the proposal: the steps are half degrees. -->
						<output aria-live="polite">{fmt.parts(shown).whole}{fmt.parts(shown).fraction}°</output>
						<span>{t('card.target')}</span>
					</div>
					<button class="round" aria-label={t('zone.raise')} onclick={() => move(Math.min(range.max, shown + range.step))} disabled={shown >= range.max}>
						<Icon name="plus" size={22} />
					</button>
				</div>
				<p class="hero-mode"><Icon name={model.zone.target.overlay ? 'edit' : 'calendar'} size={16} />{mode}</p>
				{#if pending}
					<div class="hero-apply">
						<button class="btn primary" disabled={busy || !endOk} onclick={() => run(() => api.setpoint(number, shown, end))}>
							<Icon name="check" size={18} />{t('zone.apply', { temp: fmt.temp(shown) })}
						</button>
						<p class="note">{t('zone.applyRule', { rule: ruleText })} · <a href="#duration">{t('programs.edit')}</a></p>
						{#if touched}<button class="btn" onclick={cancel}>{t('action.cancel')}</button>{/if}
					</div>
				{/if}
				<div class="hero-commands">
					{#each commands as c (c.icon)}
						<button class="hero-command" onclick={c.run} disabled={c.disabled}>
							<span class="round"><Icon name={c.icon} size={22} /></span>{c.label}
						</button>
					{/each}
				</div>
			</section>

			<div class="sheet">
				{#if model.status.alerts.length > 0}
					{#each model.status.alerts as alert (alert)}
						<p class="notice tone-warning"><Icon name="warning" /><span>{t(alertKeys[alert])}</span></p>
					{/each}
				{/if}

				<section class="section" id="duration" aria-labelledby="duration-title">
					<div class="section-head"><h2 id="duration-title">{t('ends.legend')}</h2></div>
					<div class="card">
						<EndRuleField
							bind:rule={() => rule, (value) => (chosen = value)}
							bind:hours
							bind:minutes
							name="end-rule"
							noProgram={model.zone.target.hasProgram === false}
							hideLegend
						/>
					</div>
				</section>

				{#if today}
					<section class="section" aria-labelledby="today-title">
						<div class="section-head">
							<h2 id="today-title">{t('zone.today')}</h2>
							<a class="section-link" href="#schedule-title">{t('programs.edit')}</a>
						</div>
						<div class="card">
							<ScheduleWeek groups={today.groups} {range} only={today.day} now={today.minute} />
						</div>
					</section>
				{/if}

				<section class="section" aria-labelledby="state-title">
					<div class="section-head"><h2 id="state-title">{t('zone.state')}</h2></div>
					<ul class="list">
						{#if model.zone.target.hasProgram !== false}
							<li class="list-row">
								<span class="list-main"><span class="list-label">{t('zone.scheduled')}</span></span>
								<span class="list-value" class:num={!model.zone.target.programOff}>
									{model.zone.target.programOff ? t('schedule.off') : fmt.degrees(model.zone.target.programC)}
								</span>
							</li>
						{/if}
						{#if windowState}
							<li class="list-row">
								<span class="list-main">
									<span class="list-label">{t('window.title')}</span>
									{#if windowState.open && windowState.deadline}
										<span class="list-sub">{t('window.until', { time: fmt.when(windowState.deadline) })}</span>
									{/if}
								</span>
								<span class="list-value">
									<span class="dot" style:--dot={windowState.open ? 'var(--status-warning-text)' : 'var(--status-idle-text)'}></span>
									{windowState.open ? t('window.open') : t('window.closed')}
								</span>
							</li>
							<li>
								<button class="list-row" disabled={busy} onclick={() => run(() => api.window(number, !windowState.open))}>
									<span class="list-main">
										<span class="list-label action">{windowState.open ? t('window.markClosed') : t('window.markOpen')}</span>
										<span class="list-sub">{t('window.note')}</span>
									</span>
									<span class="action"><Icon name="window" size={18} /></span>
								</button>
							</li>
						{/if}
					</ul>
				</section>

				{#if model.tempC !== null || model.humidityPct !== null}
					<section class="section" aria-labelledby="comfort-title">
						<div class="section-head"><h2 id="comfort-title">{t('comfort.title')}</h2></div>
						<AirComfort tempC={model.tempC} humidityPct={model.humidityPct} />
					</section>
				{/if}

				<section class="section" aria-labelledby="report-title">
					<div class="section-head"><h2 id="report-title">{t('report.title')}</h2></div>
					<ul class="list">
						<li>
							<a class="list-row" href={resolve('/zones/[n]/report', { n: String(number) })} data-testid="report-link">
								<span class="disc"><Icon name="pulse" size={18} /></span>
								<span class="list-main">
									<span class="list-label">{t('report.open')}</span>
									<span class="list-sub">{t('report.openSub')}</span>
								</span>
								<span class="chevron"><Icon name="chevron" size={18} /></span>
							</a>
						</li>
					</ul>
				</section>

				<section class="section" aria-labelledby="devices-title">
					<div class="section-head"><h2 id="devices-title">{t('zone.devices')}</h2></div>
					<DeviceList {devices} room={number} compact settings={resolve('/rooms/[n]', { n: String(number) })} />
				</section>

				<SchedulePanel zone={number} {range} onprograms={(p) => (programs = p)} />
			</div>
		</div>
	{/if}
</section>

<ConfirmDialog
	bind:this={confirmOff}
	title={t('zone.off.confirm.title', { zone: model ? zoneName(model.zone, t('zone.hotWaterName')) : '' })}
	body={t('zone.off.confirm.body', { rule: ruleText })}
	confirm={t('zone.off')}
	onconfirm={() => run(() => api.off(number, end))}
/>

<style>
	.card {
		padding: 0.875rem;
		background: var(--surface);
		border: 1px solid var(--border);
		border-radius: var(--radius-lg);
	}

	.action {
		color: var(--link);
	}
</style>
