<script lang="ts">
	import { resolve } from '$app/paths';
	import { onMount } from 'svelte';
	import {
		FLOW_C,
		FUEL_UNITS,
		FALLBACK_FLOW_C,
		badCurves,
		curveFlowAt,
		curveFrom,
		defaultFlowAt,
		energyIssues,
		energyOf,
		energyPatch,
		flowIssues,
		flowOf,
		flowPatch,
		parseNumber,
		type EnergyForm,
		type FlowForm
	} from '#lib/boileroptions.ts';
	import CurveChart from '#lib/CurveChart.svelte';
	import { data } from '#lib/data.svelte.ts';
	import { fmt, t } from '#lib/i18n.svelte.ts';
	import Icon from '#lib/Icon.svelte';
	import { install, manage } from '#lib/install.svelte.ts';
	import { api } from '#lib/session.ts';
	import type { FuelUnit } from '#lib/types.ts';

	const info = $derived(install.info);
	const circuitIds = $derived(data.circuits.map((c) => c.ID));
	const outdoor = $derived(data.boiler?.flowLimit.outdoor ?? null);
	const outdoorNow = $derived(outdoor && !outdoor.stale ? outdoor.tempC : null);

	// The forms follow the daemon until the person changes a field.
	let energy = $state<EnergyForm | null>(null);
	let energyDirty = $state(false);
	let flow = $state<FlowForm | null>(null);
	let flowDirty = $state(false);
	let busy = $state(false);

	$effect(() => {
		if (info && !energyDirty) energy = energyOf(info, fmt.decimal());
		if (info && !flowDirty) flow = flowOf(info, circuitIds);
	});

	onMount(() => void install.load());

	const energyProblems = $derived(energy ? energyIssues(energy) : []);
	const flowProblems = $derived(flow ? flowIssues(flow) : []);
	const unit = (u: FuelUnit) => (u === 'm3' ? 'm³' : u);
	const limit = $derived(flow ? parseNumber(flow.limit) : null);
	const ceiling = $derived(typeof limit === 'number' ? limit : FALLBACK_FLOW_C);
	const circuitName = (id: number) => data.circuits.find((c) => c.ID === id)?.Name || t('boiler.circuit', { id });
	const served = (id: number) => data.boiler?.flowLimit.circuits.find((c) => c.id === id) ?? null;

	// A step from an empty field starts at the limit a curve falls back to.
	function stepLimit(by: number) {
		if (!flow) return;
		const now = parseNumber(flow.limit);
		const base = typeof now === 'number' ? Math.round(now) : FALLBACK_FLOW_C - by;
		flow.limit = String(Math.min(FLOW_C.max, Math.max(FLOW_C.min, base + by)));
		flowDirty = true;
	}

	async function saveEnergy(event: SubmitEvent) {
		event.preventDefault();
		if (!info || !energy || energyProblems.length > 0) return;
		const patch = energyPatch(info, energy, fmt.decimal());
		if (Object.keys(patch).length === 0) return;
		busy = true;
		if ((await manage(() => api.setBoiler(patch), t('boilerSetup.saved'))) !== undefined) energyDirty = false;
		busy = false;
	}

	async function saveFlow(event: SubmitEvent) {
		event.preventDefault();
		if (!info || !flow || flowProblems.length > 0) return;
		const patch = flowPatch(info, flow);
		if (Object.keys(patch).length === 0) return;
		busy = true;
		if ((await manage(() => api.setBoiler(patch), t('boilerSetup.saved'))) !== undefined) flowDirty = false;
		busy = false;
	}
</script>

<section class="page">
	<a class="back small" href={resolve('/boiler')}><Icon name="back" size={16} />{t('boiler.title')}</a>
	<div class="page-head titled">
		<h1>{t('boilerSetup.title')}</h1>
	</div>
	<p class="muted intro">{t('boilerSetup.intro')}</p>

	{#if !info || !energy || !flow}
		<p class="muted">{t('state.loading')}</p>
	{:else}
		<section class="panel" aria-labelledby="energy-title">
			<header><h2 id="energy-title">{t('boilerSetup.energy')}</h2></header>
			<p class="note">{t('boilerSetup.energy.intro')}</p>
			<form class="options" onsubmit={saveEnergy} oninput={() => (energyDirty = true)} onchange={() => (energyDirty = true)} data-testid="boiler-energy">
				<label class="field">
					<span>{t('boilerSetup.fuel')}</span>
					<select class="input" bind:value={energy.fuel}>
						{#each FUEL_UNITS as u (u)}<option value={u}>{t(`boilerSetup.fuel.${u}`)}</option>{/each}
					</select>
				</label>
				<div class="pair">
					<label class="field">
						<span>{t('boilerSetup.maxKW')}</span>
						<input class="input num" inputmode="decimal" autocomplete="off" bind:value={energy.maxKW} aria-invalid={energyProblems.includes('maxKW')} />
					</label>
					<label class="field">
						<span>{t('boilerSetup.minKW')}</span>
						<input class="input num" inputmode="decimal" autocomplete="off" bind:value={energy.minKW} aria-invalid={energyProblems.includes('minKW') || energyProblems.includes('kwOrder')} />
					</label>
				</div>
				<div class="pair">
					<label class="field">
						<span>{t('boilerSetup.maxFlow', { unit: unit(energy.fuel) })}</span>
						<input class="input num" inputmode="decimal" autocomplete="off" bind:value={energy.maxFlow} aria-invalid={energyProblems.includes('maxFlow') || energyProblems.includes('flowPair')} />
					</label>
					<label class="field">
						<span>{t('boilerSetup.minFlow', { unit: unit(energy.fuel) })}</span>
						<input class="input num" inputmode="decimal" autocomplete="off" bind:value={energy.minFlow} aria-invalid={energyProblems.includes('minFlow') || energyProblems.includes('flowOrder') || energyProblems.includes('flowPair')} />
					</label>
				</div>
				<details class="guide" data-testid="energy-guide">
					<summary>{t('boilerSetup.guide.energy')}</summary>
					<ol>
						<li>{t('boilerSetup.guide.energy.where')}</li>
						<li>{t('boilerSetup.guide.energy.power')}</li>
						<li>{t('boilerSetup.guide.energy.flow')}</li>
						<li>{t('boilerSetup.guide.energy.compute')}</li>
						<li>{t('boilerSetup.guide.energy.example')}</li>
						<li>{t('boilerSetup.guide.energy.meter')}</li>
					</ol>
				</details>
				{#if energyProblems.length > 0}
					<ul class="issues small" role="alert">
						{#each energyProblems as issue (issue)}<li>{t(`boilerSetup.issue.${issue}`)}</li>{/each}
					</ul>
				{/if}
				<div class="row">
					<button class="btn primary" type="submit" disabled={busy || !energyDirty || energyProblems.length > 0}><Icon name="check" size={18} />{t('boilerSetup.save')}</button>
				</div>
			</form>
		</section>

		<section class="panel" aria-labelledby="flow-title">
			<header><h2 id="flow-title">{t('boilerSetup.flow')}</h2></header>
			<p class="note">{t('boilerSetup.flow.intro')}</p>
			<form class="options" onsubmit={saveFlow} oninput={() => (flowDirty = true)} onchange={() => (flowDirty = true)} data-testid="boiler-flow">
				<div class="field">
					<span id="limit-label">{t('boilerSetup.limit')}</span>
					<div class="stepper">
						<button class="btn icon" type="button" aria-label={t('zone.lower')} onclick={() => stepLimit(-1)}><Icon name="minus" size={18} /></button>
						<input
							class="input num"
							inputmode="numeric"
							autocomplete="off"
							aria-labelledby="limit-label"
							placeholder={t('boilerSetup.limit.none')}
							bind:value={flow.limit}
							aria-invalid={flowProblems.includes('limit') || flowProblems.includes('defaultNeedsLimit')}
							data-testid="flow-limit"
						/>
						<button class="btn icon" type="button" aria-label={t('zone.raise')} onclick={() => stepLimit(1)}><Icon name="plus" size={18} /></button>
					</div>
				</div>
				<p class="note">{flow.limit.trim() === '' ? t('boilerSetup.limit.unset') : t('boilerSetup.limit.hint')}</p>
				<details class="guide">
					<summary>{t('boilerSetup.guide.limit')}</summary>
					<ol>
						<li>{t('boilerSetup.guide.limit.radiators')}</li>
						<li>{t('boilerSetup.guide.limit.condensing')}</li>
						<li>{t('boilerSetup.guide.limit.floor')}</li>
						<li>{t('boilerSetup.guide.limit.try')}</li>
					</ol>
				</details>

				<fieldset>
					<legend>{t('boilerSetup.curve')}</legend>
					<p class="note" data-testid="outdoor-state">
						{#if outdoor && !outdoor.stale}
							{t('boilerSetup.outdoor.fresh', { temp: fmt.temp(outdoor.tempC), age: fmt.ago(Math.round(Date.now() / 1000 - outdoor.ageSec)) })}
						{:else if outdoor}
							{t('boilerSetup.outdoor.stale', { temp: fmt.temp(outdoor.tempC) })}
						{:else}
							{t('boilerSetup.outdoor.none')}
						{/if}
					</p>
					<label class="check"><input type="checkbox" bind:checked={flow.defaultCurve} data-testid="default-curve" />{t('boilerSetup.defaultCurve')}</label>
					<p class="note">{t('boilerSetup.defaultCurve.hint')}</p>
					{#if flow.defaultCurve && typeof limit === 'number'}
						<CurveChart at={(c) => defaultFlowAt(limit, c)} outdoorC={outdoorNow} label={t('boilerSetup.curve.label', { limit: fmt.degrees(limit) })} />
					{/if}
				</fieldset>

				{#each circuitIds as id (id)}
					{@const curve = flow.curves[String(id)]}
					{@const now = served(id)}
					{#if curve}
						<fieldset data-testid="circuit-curve">
							<legend>{t('boilerSetup.ownCurve', { circuit: circuitName(id) })}</legend>
							<label class="check"><input type="checkbox" bind:checked={curve.enabled} />{t('boilerSetup.ownCurve.on')}</label>
							<p class="note">{t('boilerSetup.ownCurve.hint')}</p>
							{#if curve.enabled}
								<div class="pair">
									<label class="field">
										<span>{t('boilerSetup.point.cold')}</span>
										<input class="input num" type="number" inputmode="decimal" step="0.5" bind:value={curve.coldOutdoor} />
									</label>
									<label class="field">
										<span>{t('boilerSetup.point.coldFlow')}</span>
										<input class="input num" type="number" inputmode="numeric" step="1" min="30" max="75" bind:value={curve.coldFlow} />
									</label>
								</div>
								<div class="pair">
									<label class="field">
										<span>{t('boilerSetup.point.mild')}</span>
										<input class="input num" type="number" inputmode="decimal" step="0.5" bind:value={curve.mildOutdoor} />
									</label>
									<label class="field">
										<span>{t('boilerSetup.point.mildFlow')}</span>
										<input class="input num" type="number" inputmode="numeric" step="1" min="30" max="75" bind:value={curve.mildFlow} />
									</label>
								</div>
								<div class="pair">
									<label class="field">
										<span>{t('boilerSetup.curve.min')}</span>
										<input class="input num" type="number" inputmode="numeric" step="1" min="30" max="75" bind:value={curve.minC} />
									</label>
									<label class="field">
										<span>{t('boilerSetup.curve.max')}</span>
										<input class="input num" type="number" inputmode="numeric" step="1" min="30" max="75" bind:value={curve.maxC} />
									</label>
								</div>
								{#if !badCurves(flow).includes(String(id))}
									<CurveChart at={(c) => curveFlowAt(curveFrom(curve), c)} outdoorC={outdoorNow} label={t('boilerSetup.ownCurve', { circuit: circuitName(id) })} />
								{/if}
							{/if}
							{#if now}
								<p class="note">{t(now.fetched ? 'boilerSetup.served.read' : 'boilerSetup.served.unread', { served: now.servedC === null ? t('boiler.noLimit') : fmt.degrees(now.servedC) })}</p>
							{/if}
						</fieldset>
					{/if}
				{/each}

				<fieldset>
					<legend>{t('boilerSetup.adapt')}</legend>
					<label class="check"><input type="checkbox" bind:checked={flow.autoAdapt} />{t('boilerSetup.adapt.on')}</label>
					<p class="note">{t('boilerSetup.adapt.hint', { ceiling: fmt.degrees(ceiling) })}</p>
				</fieldset>

				<details class="guide">
					<summary>{t('boilerSetup.advanced')}</summary>
					<div class="pair three">
						<label class="field">
							<span>{t('boilerSetup.outdoorMinutes')}</span>
							<input class="input num" type="number" inputmode="numeric" min="1" step="1" bind:value={flow.outdoorMinutes} aria-invalid={flowProblems.includes('outdoorMinutes')} />
						</label>
						<label class="field">
							<span>{t('boilerSetup.hysteresis')}</span>
							<input class="input num" type="number" inputmode="numeric" min="1" step="1" bind:value={flow.hysteresisC} aria-invalid={flowProblems.includes('hysteresis')} />
						</label>
						<label class="field">
							<span>{t('boilerSetup.interval')}</span>
							<input class="input num" type="number" inputmode="numeric" min="1" step="1" bind:value={flow.intervalMinutes} aria-invalid={flowProblems.includes('interval')} />
						</label>
					</div>
					<p class="note">{t('boilerSetup.advanced.hint')}</p>
				</details>

				{#if flowProblems.length > 0}
					<ul class="issues small" role="alert">
						{#each flowProblems as issue (issue)}<li>{t(`boilerSetup.issue.${issue}`)}</li>{/each}
					</ul>
				{/if}
				<div class="row">
					<button class="btn primary" type="submit" disabled={busy || !flowDirty || flowProblems.length > 0} data-testid="flow-save"><Icon name="check" size={18} />{t('boilerSetup.save')}</button>
				</div>
			</form>
		</section>
	{/if}
</section>

<style>
	.intro {
		max-width: 44rem;
		margin: 0;
	}

	.options {
		display: grid;
		gap: 0.875rem;
	}

	fieldset {
		display: grid;
		gap: 0.5rem;
		margin: 0;
		padding: 0.875rem 0 0;
		border: 0;
		border-top: 1px solid var(--border);
		min-width: 0;
	}

	legend {
		padding: 0;
		font-family: var(--font-display);
		font-weight: var(--weight-semibold);
	}

	.pair {
		display: grid;
		grid-template-columns: repeat(auto-fit, minmax(min(100%, 12rem), 1fr));
		gap: 0.75rem;
	}

	.stepper {
		display: flex;
		align-items: center;
		gap: 0.375rem;
	}

	.stepper .input {
		width: 6rem;
		text-align: center;
	}

	.guide {
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		padding: 0.5rem 0.875rem;
		background: color-mix(in srgb, var(--panel) 50%, transparent);
	}

	.guide summary {
		min-height: 2.25rem;
		display: flex;
		align-items: center;
		color: var(--link);
		font-size: var(--text-small-size);
		font-weight: var(--weight-semibold);
		cursor: pointer;
	}

	.guide ol {
		display: grid;
		gap: 0.5rem;
		margin: 0.25rem 0 0.5rem;
		padding-left: 1.25rem;
		font-size: var(--text-small-size);
		line-height: 1.45;
	}

	.guide .pair {
		margin-top: 0.5rem;
	}

	.issues {
		margin: 0;
		padding-left: 1.25rem;
		color: var(--status-fault-text);
	}
</style>
