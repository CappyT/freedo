<script lang="ts">
	import { resolve } from '$app/paths';
	import { act } from '#lib/actions.ts';
	import { alertKeys, modeLine } from '#lib/describe.ts';
	import Dial from '#lib/Dial.svelte';
	import { data } from '#lib/data.svelte.ts';
	import EndRuleField from '#lib/EndRuleField.svelte';
	import { defaultEndRule, timerSeconds, validTimer } from '#lib/endrule.ts';
	import { fmt, t } from '#lib/i18n.svelte.ts';
	import Icon from '#lib/Icon.svelte';
	import SchedulePanel from '#lib/SchedulePanel.svelte';
	import ScheduleWeek from '#lib/ScheduleWeek.svelte';
	import { HOT_WATER_RANGE, programInForce, type Group, type Mode } from '#lib/schedule.ts';
	import { api } from '#lib/session.ts';
	import { nextText, stateIcons, todayOf } from '#lib/today.svelte.ts';
	import type { EndRule } from '#lib/types.ts';
	import { zoneName } from '#lib/zones.ts';

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
	let programs = $state<Record<Mode, Group[] | null>>({ home: null, away: null });

	const shown = $derived(draft ?? model?.zone.target.targetC ?? model?.zone.target.programC ?? 50);
	const end = $derived({ mode: rule, durationSec: timerSeconds(hours, minutes) });
	const endOk = $derived(rule !== 'timer' || validTimer(end.durationSec));
	const ruleText = $derived(rule === 'timer' ? `${t('ends.timer')}, ${fmt.duration(end.durationSec)}` : t(`ends.${rule}`));
	const pending = $derived(model !== undefined && ((touched && draft !== model.zone.target.targetC) || !model.zone.target.on));
	const today = $derived(todayOf(programs[programInForce(data.away, programs.away !== null)]));
	const mode = $derived.by(() => {
		if (!model) return '';
		const target = model.zone.target;
		const line = modeLine(target);
		const next = target.source === 'program' && target.overlay === null ? nextText(today?.next ?? null) : null;
		return next ? `${line} · ${next}` : line;
	});

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
</script>

<section class="page">
	{#if !data.loaded}
		<p class="muted">{t('state.loading')}</p>
	{:else if !model}
		<a class="section-link" href={resolve('/')}><Icon name="back" size={16} />{t('nav.home')}</a>
		<div class="panel">
			<h1>{t('hotWater.missing.title')}</h1>
			<p>{t('hotWater.missing.body')}</p>
		</div>
	{:else}
		<div class="screen">
			<section class="hero fill-{model.status.tone}" aria-labelledby="water-title">
				<div class="hero-bar">
					<a class="round" href={resolve('/')} aria-label={t('nav.home')}><Icon name="back" size={22} /></a>
					<h1 id="water-title">{zoneName(model.zone, t('zone.hotWaterName'))}</h1>
					<a class="round" href="#schedule-title" aria-label={t('schedule.title')}><Icon name="calendar" size={20} /></a>
				</div>
				<span class="hero-state">
					<Icon name={stateIcons[model.status.state]} size={16} />
					{model.zone.target.on ? t('tile.hotWaterOn', { temp: fmt.degrees(model.zone.target.targetC) }) : t('tile.hotWaterOff')}
				</span>
				<p class="hero-facts">
					<span>{t('hotWater.reported')} <b>{fmt.degrees(data.boiler?.DHWSetpointC)}</b></span>
				</p>
				<!-- The dial reads like the one of a room: what the boiler measures in the middle, the target on the arc. -->
				<Dial
					current={data.boiler?.DHWC ?? null}
					target={shown}
					min={range.min}
					max={range.max}
					step={range.step}
					interactive
					label={t('hotWater.targetLabel')}
					caption={t('hotWater.inBoiler')}
					onchange={move}
					size="17rem"
				/>
				<div class="hero-nudge">
					<button class="round" aria-label={t('zone.lower')} onclick={() => move(Math.max(range.min, shown - range.step))} disabled={shown <= range.min}>
						<Icon name="minus" size={22} />
					</button>
					<div class="hero-target">
						<output aria-live="polite">{fmt.number(shown, 0)}°</output>
						<span>{t('card.target')}</span>
					</div>
					<button class="round" aria-label={t('zone.raise')} onclick={() => move(Math.min(range.max, shown + range.step))} disabled={shown >= range.max}>
						<Icon name="plus" size={22} />
					</button>
				</div>
				<p class="hero-mode"><Icon name={model.zone.target.overlay ? 'edit' : 'calendar'} size={16} />{mode}</p>
				{#if pending}
					<div class="hero-apply">
						<button class="btn primary" disabled={busy || !endOk} onclick={() => run(() => api.setpoint(model.zone.number, shown, end))}>
							<Icon name="power" size={18} />{t('hotWater.on', { temp: fmt.temp(shown) })}
						</button>
						<p class="note">{t('zone.applyRule', { rule: ruleText })} · <a href="#duration">{t('programs.edit')}</a></p>
						{#if touched}<button class="btn" onclick={cancel}>{t('action.cancel')}</button>{/if}
					</div>
				{/if}
				<div class="hero-commands">
					<button class="hero-command" disabled={busy || !endOk || !model.zone.target.on} onclick={() => run(() => api.off(model.zone.number, end))}>
						<span class="round"><Icon name="power" size={22} /></span>{t('hotWater.off')}
					</button>
					<button class="hero-command" disabled={busy} onclick={() => run(() => api.auto(model.zone.number))}>
						<span class="round"><Icon name="calendar" size={22} /></span>{t('zone.auto')}
					</button>
				</div>
			</section>

			<div class="sheet">
				{#each model.status.alerts as alert (alert)}
					<p class="notice tone-warning"><Icon name="warning" /><span>{t(alertKeys[alert])}</span></p>
				{/each}

				<section class="section" id="duration" aria-labelledby="duration-title">
					<div class="section-head"><h2 id="duration-title">{t('ends.legend')}</h2></div>
					<div class="card">
						<EndRuleField
							bind:rule={() => rule, (value) => (chosen = value)}
							bind:hours
							bind:minutes
							name="dhw-end-rule"
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
						<li class="list-row">
							<span class="list-main"><span class="list-label">{t('hotWater.power')}</span></span>
							<span class="list-value">
								<span class="dot" style:--dot={model.zone.target.on ? 'var(--status-idle-text)' : 'var(--status-off-text)'}></span>
								{model.zone.target.on ? t('tile.hotWaterOn', { temp: fmt.degrees(model.zone.target.targetC) }) : t('tile.hotWaterOff')}
							</span>
						</li>
						{#if model.zone.target.hasProgram !== false}
							<li class="list-row">
								<span class="list-main"><span class="list-label">{t('zone.scheduled')}</span></span>
								<span class="list-value" class:num={!model.zone.target.programOff}>
									{model.zone.target.programOff ? t('schedule.off') : fmt.degrees(model.zone.target.programC)}
								</span>
							</li>
						{/if}
						<li class="list-row">
							<span class="list-main"><span class="list-label">{t('boiler.dhw')}</span></span>
							<span class="list-value num">{fmt.degrees(data.boiler?.DHWC)}</span>
						</li>
					</ul>
					<p class="note">{t('hotWater.note')}</p>
				</section>

				<SchedulePanel zone={model.zone.number} {range} hotWater onprograms={(p) => (programs = p)} />
			</div>
		</div>
	{/if}
</section>

<style>
	.card {
		padding: 0.875rem;
		background: var(--surface);
		border: 1px solid var(--border);
		border-radius: var(--radius-lg);
	}
</style>
