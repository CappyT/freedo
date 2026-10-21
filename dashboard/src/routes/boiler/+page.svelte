<script lang="ts">
	import { resolve } from '$app/paths';
	import { act } from '#lib/actions.ts';
	import { boilerFaults, burnerHours, burnerState, canSetFlow, flowDrivers, flowInRange } from '#lib/boiler.ts';
	import { data } from '#lib/data.svelte.ts';
	import { fmt, t, type Key } from '#lib/i18n.svelte.ts';
	import Icon from '#lib/Icon.svelte';
	import { typedNumber } from '#lib/numeric.ts';
	import { api } from '#lib/session.ts';
	import type { FlowCircuit } from '#lib/types.ts';

	const boiler = $derived(data.boiler);
	const faults = $derived(boilerFaults(boiler));
	const burner = $derived(boiler ? burnerState(boiler) : 'unknown');
	// The screen takes the colour of what the boiler does: fault first, then a running burner.
	const fill = $derived(faults.state === 'fault' ? 'fault' : burner === 'on' ? 'heating' : 'off');
	const circuits = $derived(boiler?.flowLimit.circuits ?? []);
	const names = $derived(new Map(data.circuits.map((c) => [c.ID, c.Name])));
	let wanted = $state<Record<number, number | undefined>>({});
	let busy = $state(false);

	const reasonKey = (prefix: string, reason: string | null) => `${prefix}.${reason}` as Key;

	function value(c: FlowCircuit): number {
		return wanted[c.id] ?? c.servedC ?? c.computedC ?? c.maxC;
	}

	function step(c: FlowCircuit, by: number) {
		wanted[c.id] = Math.min(c.maxC, Math.max(c.minC, Math.round(value(c)) + by));
	}

	async function setFlow(c: FlowCircuit) {
		busy = true;
		const done = await act(() => api.flow(value(c), c.id));
		busy = false;
		if (done) wanted[c.id] = undefined;
	}
</script>

<section class="page">
	{#if !data.loaded}
		<h1>{t('boiler.title')}</h1>
		<p class="muted">{t('state.loading')}</p>
	{:else if !boiler}
		<a class="section-link" href={resolve('/')}><Icon name="back" size={16} />{t('nav.home')}</a>
		<h1>{t('boiler.title')}</h1>
		<div class="panel"><p>{t('boiler.unavailable')}</p></div>
	{:else}
		<div class="screen">
			<section class="hero fill-{fill}" aria-labelledby="boiler-title">
				<div class="hero-bar">
					<a class="round" href={resolve('/')} aria-label={t('nav.home')}><Icon name="back" size={22} /></a>
					<h1 id="boiler-title">{t('boiler.title')}</h1>
					<span></span>
				</div>
				<div class="pills">
					<span class="hero-state" data-testid="burner">
						<Icon name={burner === 'on' ? 'flame' : 'power'} size={16} />
						{burner === 'on' ? t('boiler.burnerOn') : burner === 'off' ? t('boiler.burnerOff') : t('boiler.burnerUnknown')}
					</span>
					<!-- The burner flag does not say what the burner works for. The call of the heating circuit is the one sure cause. -->
					{#if boiler.HeatActive}<span class="hero-state" data-testid="heat-call">{t('boiler.heatCall')}</span>{/if}
				</div>
				<div class="big">
					{#if boiler.FlowC === null}
						<span class="value">-</span>
					{:else}
						<span class="value">{fmt.number(boiler.FlowC, 1)}<small>°</small></span>
					{/if}
					<span class="label">{t('boiler.flow')}</span>
				</div>
				{#if boiler.ModulationPct !== null}
					<div class="modulation">
						<div class="mod-row"><span>{t('boiler.modulation')}</span><b>{fmt.pct(boiler.ModulationPct)}</b></div>
						<div class="bar" role="meter" aria-label={t('boiler.modulation')} aria-valuemin="0" aria-valuemax="100" aria-valuenow={boiler.ModulationPct}>
							<span style:width="{Math.min(100, Math.max(0, boiler.ModulationPct))}%"></span>
						</div>
					</div>
				{/if}
				<dl class="stats">
					<div><dt>{t('boiler.return')}</dt><dd>{fmt.degrees(boiler.ReturnC)}</dd></div>
					<div><dt>{t('boiler.dhw')}</dt><dd>{fmt.degrees(boiler.DHWC)}</dd></div>
					<div><dt>{t('boiler.outside')}</dt><dd>{fmt.degrees(boiler.OutsideC)}</dd></div>
				</dl>
			</section>

			<div class="sheet">
				{#if !boiler.Present}
					<p class="notice tone-off" role="status"><Icon name="warning" /><span>{t('boiler.notReported')}</span></p>
				{/if}
				<!-- The banner of every page names the fault codes. Here the page says what a code is. -->
				{#if faults.state === 'fault'}
					<p class="notice tone-fault" data-testid="fault-meaning"><Icon name="fault" /><span>{t('boiler.faults.meaning')}</span></p>
				{/if}

				<section class="section" aria-labelledby="live-title">
					<div class="section-head"><h2 id="live-title">{t('boiler.live')}</h2></div>
					<ul class="list">
						<li class="list-row"><span class="list-main list-label">{t('boiler.controlSetpoint')}</span><span class="list-value num">{fmt.degrees(boiler.ControlSetpointC)}</span></li>
						<li class="list-row"><span class="list-main list-label">{t('boiler.maxCh')}</span><span class="list-value num">{fmt.degrees(boiler.MaxChSetpointC)}</span></li>
						<li class="list-row"><span class="list-main list-label">{t('boiler.demand')}</span><span class="list-value num">{fmt.pct(boiler.CircuitDemandPct)}</span></li>
						<li class="list-row">
							<span class="list-main list-label">{t('boiler.pressure')}</span>
							<span class="list-value num">{boiler.ChPressureBar === null ? '-' : fmt.number(boiler.ChPressureBar, 1) + ' bar'}</span>
						</li>
						<li class="list-row">
							<span class="list-main list-label">{t('boiler.faultState')}</span>
							<span class="list-value" class:num={faults.state === 'fault'} data-testid="fault-state">
								{faults.state === 'fault' ? faults.codes.join(', ') : faults.state === 'none' ? t('boiler.faults.none') : t('boiler.faults.unknown')}
							</span>
						</li>
						<li class="list-row"><span class="list-main list-label">{t('boiler.starts')}</span><span class="list-value num">{fmt.number(boiler.BurnerStarts, 0)}</span></li>
						<li class="list-row"><span class="list-main list-label">{t('boiler.hours')}</span><span class="list-value num">{fmt.number(burnerHours(boiler.BurnerHoursMin), 1)}</span></li>
					</ul>
				</section>

				<section class="section" aria-labelledby="flow-title">
					<div class="section-head"><h2 id="flow-title">{t('boiler.flowLimit')}</h2></div>
					{#if boiler.flowLimit.outdoor}
						<p class="note">
							{t('boiler.outdoorFed', { temp: fmt.temp(boiler.flowLimit.outdoor.tempC) })}
							{#if boiler.flowLimit.outdoor.stale}<strong>{t('boiler.outdoorStale')}</strong>{/if}
						</p>
					{/if}
					{#if circuits.length === 0}
						<p class="list none muted">{t('boiler.noCircuits')}</p>
					{/if}
					{#each circuits as c (c.id)}
						<ul class="list">
							<li class="list-row head">
								<span class="list-main list-label">{names.get(c.id) ?? t('boiler.circuit', { id: c.id })}</span>
								{#each flowDrivers(c) as driver (driver)}
									<span class="list-value">{t(`driver.${driver}`)}</span>
								{/each}
							</li>
							<li class="list-row">
								<span class="list-main list-label">{t('boiler.served')}</span>
								<span class="list-value" class:num={c.servedC !== null}>{c.servedC === null ? t('boiler.noLimit') : fmt.degrees(c.servedC)}</span>
							</li>
							<li class="list-row"><span class="list-main list-label">{t('boiler.computed')}</span><span class="list-value num">{fmt.degrees(c.computedC)}</span></li>
							<li class="list-row">
								<span class="list-main list-label">{t('boiler.bounds')}</span>
								<span class="list-value num">{t('boiler.range', { min: fmt.number(c.minC, 0), max: fmt.number(c.maxC, 0) })}</span>
							</li>
							<li class="list-row"><span class="list-main list-label">{t('boiler.fetched')}</span><span class="list-value">{c.fetched ? t('state.yes') : t('state.no')}</span></li>
							{#if c.adapt}
								<li class="list-row"><span class="list-main list-label">{t('boiler.adapted')}</span><span class="list-value num">{fmt.degrees(c.adaptedC)}</span></li>
							{/if}
							{#if canSetFlow(c)}
								<li class="set">
									<form
										onsubmit={(e) => {
											e.preventDefault();
											void setFlow(c);
										}}
									>
										<label class="list-label" for="flow-{c.id}">{t('boiler.setLimit')}</label>
										<div class="stepper">
											<button class="btn icon" type="button" aria-label={t('zone.lower')} onclick={() => step(c, -1)} disabled={value(c) <= c.minC}>
												<Icon name="minus" size={18} />
											</button>
											<input
												id="flow-{c.id}"
												class="input mono"
												type="number"
												min={c.minC}
												max={c.maxC}
												step="1"
												value={value(c)}
												oninput={(e) => (wanted[c.id] = typedNumber(e.currentTarget.valueAsNumber, value(c), false))}
												onchange={(e) => (wanted[c.id] = typedNumber(e.currentTarget.valueAsNumber, value(c), true))}
												aria-invalid={!flowInRange(c, value(c))}
											/>
											<button class="btn icon" type="button" aria-label={t('zone.raise')} onclick={() => step(c, 1)} disabled={value(c) >= c.maxC}>
												<Icon name="plus" size={18} />
											</button>
										</div>
										<button class="btn primary" type="submit" disabled={busy || !flowInRange(c, value(c))}>
											<Icon name="check" size={18} />{t('boiler.apply')}
										</button>
									</form>
								</li>
							{/if}
						</ul>
						<p class="note">{canSetFlow(c) ? t('boiler.manualNote') : t('boiler.drivenNote')}</p>
					{/each}
				</section>

				<section class="section" aria-labelledby="energy-title">
					<div class="section-head"><h2 id="energy-title">{t('boiler.energy')}</h2></div>
					<ul class="list">
						<li class="list-row">
							<span class="list-main"><span class="list-label">{t('boiler.heat')}</span>{#if boiler.energy.reason}<span class="list-sub">{t(reasonKey('reason', boiler.energy.reason))}</span>{/if}</span>
							<span class="list-value"><span class="dot" style:--dot={boiler.energy.running ? 'var(--status-idle-text)' : 'var(--status-off-text)'}></span>{boiler.energy.running ? t('boiler.counting') : t('boiler.notCounting')}</span>
						</li>
						<li class="list-row"><span class="list-main list-label">{t('boiler.power')}</span><span class="list-value num">{fmt.number(boiler.energy.powerKW, 2)} kW</span></li>
						<li class="list-row"><span class="list-main list-label">{t('boiler.total')}</span><span class="list-value num">{fmt.number(boiler.energy.totalKWh, 1)} kWh</span></li>
					</ul>
					<ul class="list">
						<li class="list-row">
							<span class="list-main"><span class="list-label">{t('boiler.gas')}</span>{#if boiler.energy.gas.reason}<span class="list-sub">{t(reasonKey('reason', boiler.energy.gas.reason))}</span>{/if}</span>
							<span class="list-value"><span class="dot" style:--dot={boiler.energy.gas.running ? 'var(--status-idle-text)' : 'var(--status-off-text)'}></span>{boiler.energy.gas.running ? t('boiler.counting') : t('boiler.notCounting')}</span>
						</li>
						<li class="list-row"><span class="list-main list-label">{t('boiler.flowRate')}</span><span class="list-value num">{fmt.number(boiler.energy.gas.flowM3H, 2)} {boiler.energy.gas.unit}/h</span></li>
						<li class="list-row"><span class="list-main list-label">{t('boiler.total')}</span><span class="list-value num">{fmt.number(boiler.energy.gas.totalM3, 1)} {boiler.energy.gas.unit}</span></li>
					</ul>
					<p class="note">{t('boiler.estimateNote')}</p>
				</section>
			</div>
		</div>
	{/if}
</section>

<style>
	.pills {
		display: flex;
		flex-wrap: wrap;
		justify-content: center;
		gap: 0.375rem;
	}

	.big {
		display: grid;
		justify-items: center;
		margin-top: 0.5rem;
	}

	.value {
		font-family: var(--font-display);
		font-size: 4.5rem;
		font-weight: var(--weight-bold);
		line-height: 1;
		letter-spacing: -0.04em;
	}

	.value small {
		font-size: 0.42em;
		font-weight: var(--weight-semibold);
		vertical-align: 1.2em;
		margin-left: 0.05em;
	}

	.label {
		font-size: 0.9375rem;
		font-weight: var(--weight-semibold);
	}

	.modulation {
		display: grid;
		gap: 0.375rem;
		width: 100%;
		max-width: 19rem;
	}

	.mod-row {
		display: flex;
		justify-content: space-between;
		font-size: var(--text-small-size);
		font-weight: var(--weight-semibold);
	}

	.mod-row b {
		font-family: var(--font-mono);
		font-weight: var(--weight-medium);
	}

	.bar {
		height: 0.625rem;
		border-radius: var(--radius-pill);
		background: color-mix(in srgb, var(--fill-ink) 16%, transparent);
		overflow: hidden;
	}

	.bar span {
		display: block;
		height: 100%;
		border-radius: var(--radius-pill);
		background: var(--fill-ink);
	}

	.stats {
		display: grid;
		grid-template-columns: repeat(3, minmax(0, 1fr));
		width: 100%;
		max-width: 24rem;
		margin: 0.25rem 0 0;
	}

	.stats div {
		display: grid;
		justify-items: center;
		gap: 0.1rem;
		padding: 0.25rem 0.5rem;
	}

	.stats div + div {
		border-left: 1px solid color-mix(in srgb, var(--fill-ink) 22%, transparent);
	}

	.stats dt {
		order: 2;
		font-size: 0.8125rem;
		font-weight: var(--weight-semibold);
	}

	.stats dd {
		margin: 0;
		font-family: var(--font-display);
		font-size: 1.375rem;
		font-weight: var(--weight-bold);
		letter-spacing: -0.02em;
	}

	.head {
		background: light-dark(var(--ice), var(--navy-border));
	}

	/* The label, the stepper and the command on one line; on a phone the command takes a line of its own. */
	.set form {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: 0.625rem 0.75rem;
		padding: 0.625rem 0.875rem;
	}

	.set .list-label {
		flex: 1 1 8rem;
	}

	.stepper {
		display: flex;
		align-items: center;
		gap: 0.375rem;
	}

	.stepper .input {
		width: 4.5rem;
		text-align: center;
		appearance: textfield;
	}

	.stepper .input::-webkit-inner-spin-button,
	.stepper .input::-webkit-outer-spin-button {
		-webkit-appearance: none;
		margin: 0;
	}

	@media (max-width: 559px) {
		.set .btn.primary {
			flex: 1 1 100%;
		}
	}

	.none {
		padding: 0.875rem;
		font-size: var(--text-small-size);
	}
</style>
