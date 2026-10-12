<script lang="ts">
	import { act } from '#lib/actions.ts';
	import { burnerHours, canSetFlow, flowDrivers, flowInRange } from '#lib/boiler.ts';
	import { data } from '#lib/data.svelte.ts';
	import { fmt, t, type Key } from '#lib/i18n.svelte.ts';
	import Icon from '#lib/Icon.svelte';
	import { typedNumber } from '#lib/numeric.ts';
	import { api } from '#lib/session.ts';
	import type { FlowCircuit } from '#lib/types.ts';

	const boiler = $derived(data.boiler);
	const circuits = $derived(boiler?.flowLimit.circuits ?? []);
	const names = $derived(new Map(data.circuits.map((c) => [c.ID, c.Name])));
	let wanted = $state<Record<number, number | undefined>>({});
	let busy = $state(false);

	const reasonKey = (prefix: string, reason: string | null) => `${prefix}.${reason}` as Key;

	function value(c: FlowCircuit): number {
		return wanted[c.id] ?? c.servedC ?? c.computedC ?? c.maxC;
	}

	async function setFlow(c: FlowCircuit) {
		busy = true;
		const done = await act(() => api.flow(value(c), c.id));
		busy = false;
		if (done) wanted[c.id] = undefined;
	}
</script>

<section class="page">
	<div class="page-head titled">
		<h1>{t('boiler.title')}</h1>
		{#if boiler}
			<span class="badge" class:tone-heating={boiler.HeatActive} class:tone-idle={!boiler.HeatActive}>
				{boiler.HeatActive ? t('boiler.burnerOn') : t('boiler.burnerOff')}
			</span>
		{/if}
	</div>

	{#if !data.loaded}
		<p class="muted">{t('state.loading')}</p>
	{:else if !boiler}
		<div class="panel"><p>{t('boiler.unavailable')}</p></div>
	{:else}
		{#if !boiler.Present}
			<p class="notice tone-off" role="status"><Icon name="warning" /><span>{t('boiler.notReported')}</span></p>
		{/if}
		{#if boiler.FaultCode !== 0 || (boiler.FaultCodes?.length ?? 0) > 0}
			<p class="notice tone-fault" role="alert">
				<Icon name="fault" />
				<span>{t('boiler.faults', { codes: (boiler.FaultCodes ?? [boiler.FaultCode]).join(', ') })}</span>
			</p>
		{/if}

		<section class="panel" aria-labelledby="live-title">
			<header><h2 id="live-title">{t('boiler.live')}</h2></header>
			<dl class="facts">
				<div><dt>{t('boiler.flow')}</dt><dd>{fmt.temp(boiler.FlowC)}</dd></div>
				<div><dt>{t('boiler.return')}</dt><dd>{fmt.temp(boiler.ReturnC)}</dd></div>
				<div><dt>{t('boiler.modulation')}</dt><dd>{fmt.percent(boiler.ModulationPct)}</dd></div>
				<div><dt>{t('boiler.dhw')}</dt><dd>{fmt.temp(boiler.DHWC)}</dd></div>
				<div><dt>{t('boiler.outside')}</dt><dd>{fmt.temp(boiler.OutsideC)}</dd></div>
				<div><dt>{t('boiler.controlSetpoint')}</dt><dd>{fmt.temp(boiler.ControlSetpointC)}</dd></div>
				<div><dt>{t('boiler.maxCh')}</dt><dd>{fmt.temp(boiler.MaxChSetpointC)}</dd></div>
				<div><dt>{t('boiler.demand')}</dt><dd>{fmt.percent(boiler.CircuitDemandPct)}</dd></div>
				<div><dt>{t('boiler.pressure')}</dt><dd>{boiler.ChPressureBar === null ? '-' : fmt.number(boiler.ChPressureBar, 1) + ' bar'}</dd></div>
				<div><dt>{t('boiler.starts')}</dt><dd>{fmt.number(boiler.BurnerStarts, 0)}</dd></div>
				<div><dt>{t('boiler.hours')}</dt><dd>{fmt.number(burnerHours(boiler.BurnerHoursMin), 1)}</dd></div>
			</dl>
		</section>

		<section class="panel" aria-labelledby="flow-title">
			<header>
				<h2 id="flow-title">{t('boiler.flowLimit')}</h2>
				{#if boiler.flowLimit.outdoor}
					<span class="small muted">
						{t('boiler.outdoorFed', { temp: fmt.temp(boiler.flowLimit.outdoor.tempC) })}
						{#if boiler.flowLimit.outdoor.stale}<strong>{t('boiler.outdoorStale')}</strong>{/if}
					</span>
				{/if}
			</header>
			{#if circuits.length === 0}
				<p class="muted">{t('boiler.noCircuits')}</p>
			{/if}
			{#each circuits as c (c.id)}
				<div class="circuit">
					<div class="row head">
						<h3>{names.get(c.id) ?? t('boiler.circuit', { id: c.id })}</h3>
						{#each flowDrivers(c) as driver (driver)}
							<span class="badge tone-idle">{t(`driver.${driver}`)}</span>
						{/each}
					</div>
					<dl class="facts">
						<div><dt>{t('boiler.served')}</dt><dd class:text={c.servedC === null}>{c.servedC === null ? t('boiler.noLimit') : fmt.temp(c.servedC)}</dd></div>
						<div><dt>{t('boiler.computed')}</dt><dd>{fmt.temp(c.computedC)}</dd></div>
						<div><dt>{t('boiler.bounds')}</dt><dd>{t('boiler.range', { min: fmt.number(c.minC, 0), max: fmt.number(c.maxC, 0) })}</dd></div>
						<div><dt>{t('boiler.fetched')}</dt><dd class="text">{c.fetched ? t('state.yes') : t('state.no')}</dd></div>
						{#if c.adapt}
							<div><dt>{t('boiler.adapted')}</dt><dd>{fmt.temp(c.adaptedC)}</dd></div>
						{/if}
					</dl>
					{#if canSetFlow(c)}
						<form
							class="row fill"
							onsubmit={(e) => {
								e.preventDefault();
								void setFlow(c);
							}}
						>
							<label class="field">
								<span>{t('boiler.setLimit')}</span>
								<input
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
							</label>
							<button class="btn primary" type="submit" disabled={busy || !flowInRange(c, value(c))}>
								<Icon name="check" size={18} />{t('boiler.apply')}
							</button>
						</form>
						<p class="note">{t('boiler.manualNote')}</p>
					{:else}
						<p class="note">{t('boiler.drivenNote')}</p>
					{/if}
				</div>
			{/each}
		</section>

		<section class="panel" aria-labelledby="energy-title">
			<header><h2 id="energy-title">{t('boiler.energy')}</h2></header>
			<div class="two">
				<div class="estimate">
					<h3>{t('boiler.heat')}</h3>
					<span class="badge" class:tone-idle={boiler.energy.running} class:tone-off={!boiler.energy.running}>
						{boiler.energy.running ? t('boiler.counting') : t('boiler.notCounting')}
					</span>
					{#if boiler.energy.reason}
						<p class="note">{t(reasonKey('reason', boiler.energy.reason))}</p>
					{/if}
					<dl class="facts">
						<div><dt>{t('boiler.power')}</dt><dd>{fmt.number(boiler.energy.powerKW, 2)} kW</dd></div>
						<div><dt>{t('boiler.total')}</dt><dd>{fmt.number(boiler.energy.totalKWh, 1)} kWh</dd></div>
					</dl>
				</div>
				<div class="estimate">
					<h3>{t('boiler.gas')}</h3>
					<span class="badge" class:tone-idle={boiler.energy.gas.running} class:tone-off={!boiler.energy.gas.running}>
						{boiler.energy.gas.running ? t('boiler.counting') : t('boiler.notCounting')}
					</span>
					{#if boiler.energy.gas.reason}
						<p class="note">{t(reasonKey('reason', boiler.energy.gas.reason))}</p>
					{/if}
					<dl class="facts">
						<div><dt>{t('boiler.flowRate')}</dt><dd>{fmt.number(boiler.energy.gas.flowM3H, 2)} {boiler.energy.gas.unit}/h</dd></div>
						<div><dt>{t('boiler.total')}</dt><dd>{fmt.number(boiler.energy.gas.totalM3, 1)} {boiler.energy.gas.unit}</dd></div>
					</dl>
				</div>
			</div>
			<p class="note">{t('boiler.estimateNote')}</p>
		</section>
	{/if}
</section>

<style>
	.circuit {
		display: grid;
		gap: 0.9rem;
		padding-top: 1rem;
		border-top: 1px solid var(--border);
	}

	.circuit:first-of-type {
		border-top: 0;
		padding-top: 0;
	}

	.head {
		gap: 0.75rem;
	}

	.two {
		display: grid;
		grid-template-columns: repeat(auto-fit, minmax(16rem, 1fr));
		gap: 1.5rem;
	}

	.estimate {
		display: grid;
		gap: 0.75rem;
		justify-items: start;
	}

	.estimate .facts {
		width: 100%;
	}

	form .field {
		width: 12rem;
	}
</style>
