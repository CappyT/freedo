<script lang="ts">
	import { resolve } from '$app/paths';
	import { act, errorText } from '#lib/actions.ts';
	import BarChart, { type BarItem } from '#lib/BarChart.svelte';
	import { data } from '#lib/data.svelte.ts';
	import { cost, localDay, monthBars, shiftMonth, total, yearBars, type Bar } from '#lib/history.ts';
	import { fmt, t, type Key } from '#lib/i18n.svelte.ts';
	import Icon from '#lib/Icon.svelte';
	import { api } from '#lib/session.ts';
	import type { EnergyHistory, HistoryInfo } from '#lib/types.ts';

	const CURRENCIES = ['EUR', 'CHF', 'GBP', 'USD', 'SEK', 'NOK', 'DKK', 'PLN', 'CZK', 'HUF', 'RON'];
	const thisMonth = localDay(new Date()).slice(0, 7);

	let info = $state<HistoryInfo | null>(null);
	let energy = $state<EnergyHistory | null>(null);
	let error = $state<string | null>(null);
	let loading = $state(true);
	let busy = $state(false);
	let view = $state<'days' | 'months'>('days');
	let month = $state(thisMonth);
	let price = $state('');
	let currency = $state('EUR');

	async function load() {
		loading = true;
		error = null;
		try {
			info = await api.history();
			energy = info.enabled ? await api.energyHistory() : null;
			price = info.fuelPrice === null ? '' : String(info.fuelPrice).replace('.', fmt.decimal());
			currency = info.currency;
		} catch (e) {
			error = errorText(e);
		} finally {
			loading = false;
		}
	}

	$effect(() => {
		void load();
	});

	async function enable() {
		busy = true;
		if (await act(() => api.setHistory({ enabled: true }), t('history.enabled'))) await load();
		busy = false;
	}

	const priceValue = $derived(price.trim() === '' ? null : Number(price.replace(',', '.')));
	const priceOk = $derived(priceValue === null || (Number.isFinite(priceValue) && priceValue >= 0 && priceValue <= 1000));

	async function savePrice(event: SubmitEvent) {
		event.preventDefault();
		if (!priceOk) return;
		busy = true;
		if (await act(() => api.setHistory({ fuelPrice: priceValue, currency }), t('energy.priceSaved'))) await load();
		busy = false;
	}

	// Day 1 and every fifth day, like the scale of a calendar.
	const dayLabel = (n: number) => (n === 1 || n % 5 === 0 ? String(n) : '');
	const unit = $derived(energy?.unit ?? 'm3');
	const firstMonth = $derived(shiftMonth(thisMonth, -((info?.dayMonths ?? 13) - 1)));
	const bars = $derived<Bar[]>(
		!energy ? [] : view === 'days' ? monthBars(month, energy.days) : yearBars(thisMonth, info?.dayMonths ?? 13, energy.days)
	);
	const items = $derived<BarItem[]>(
		bars.map((b) => ({
			key: b.key,
			label: view === 'days' ? dayLabel(Number(b.key.slice(8))) : fmt.month(b.key, 'short'),
			value: b.total?.fuel ?? null
		}))
	);
	const sum = $derived(total(energy ? energy.days.filter((d) => (view === 'days' ? d.day.startsWith(month) : true)) : []));
	const fuelPrice = $derived(energy?.fuelPrice ?? null);
	const money = (fuel: number) => fmt.money(cost(fuel, fuelPrice), energy?.currency ?? currency);
	const barName = (b: Bar) => (view === 'days' ? fmt.shortDay(b.key) : fmt.month(b.key));

	function describe(i: number): string {
		const b = bars[i];
		if (!b) return '';
		if (!b.total) return `${barName(b)}: ${t('energy.noData')}`;
		const parts = [fmt.fuel(b.total.fuel, unit)];
		if (fuelPrice !== null) parts.push(money(b.total.fuel));
		parts.push(`${fmt.number(b.total.heatKWh, 1)} kWh`);
		return `${barName(b)}: ${parts.join(' · ')}`;
	}

	const fuelOff = $derived(data.boiler?.energy.gas.reason ?? null);
</script>

<section class="page">
	<a class="section-link" href={resolve('/boiler')}><Icon name="back" size={16} />{t('boiler.title')}</a>
	<div class="page-head titled">
		<h1>{t('energy.title')}</h1>
	</div>

	{#if error}
		<p class="notice tone-fault" role="alert"><Icon name="fault" /><span>{error}</span></p>
	{:else if info && !info.available}
		<p class="notice tone-off"><Icon name="warning" /><span>{t('history.noDatabase')}</span></p>
	{:else if info && !info.enabled}
		<div class="panel off">
			<h2>{t('history.offTitle')}</h2>
			<p>{t('history.what', { days: info.sampleDays, months: info.dayMonths })}</p>
			<button class="btn primary" disabled={busy} onclick={enable}><Icon name="check" size={18} />{t('history.enable')}</button>
		</div>
	{:else if energy}
		{#if fuelOff}
			<p class="notice tone-warning">
				<Icon name="warning" /><span
					>{t('energy.fuelOff')} {t(`reason.${fuelOff}` as Key)}
					{#if fuelOff === 'noGasFlow'}<a href={resolve('/boiler/setup')}>{t('boilerSetup.title')}</a>{/if}</span
				>
			</p>
		{/if}

		<div class="controls">
			<fieldset class="segmented">
				<legend class="sr-only">{t('energy.view')}</legend>
				<label><input type="radio" name="energy-view" value="days" bind:group={view} />{t('energy.days')}</label>
				<label><input type="radio" name="energy-view" value="months" bind:group={view} />{t('energy.months')}</label>
			</fieldset>
			{#if view === 'days'}
				<div class="month-nav">
					<button class="btn icon" aria-label={t('energy.previous')} disabled={month <= firstMonth} onclick={() => (month = shiftMonth(month, -1))}>
						<Icon name="back" size={20} />
					</button>
					<p class="period">{fmt.month(month)}</p>
					<button class="btn icon next" aria-label={t('energy.next')} disabled={month >= thisMonth} onclick={() => (month = shiftMonth(month, 1))}>
						<Icon name="back" size={20} />
					</button>
				</div>
			{:else}
				<p class="period">{t('energy.lastMonths', { n: info?.dayMonths ?? 13 })}</p>
			{/if}
		</div>

		<dl class="stat-tiles">
			<div><dt>{t('energy.fuel')}</dt><dd>{fmt.fuel(sum.fuel, unit)}</dd></div>
			<div>
				<dt>{t('energy.cost')}</dt>
				<dd>{#if fuelPrice !== null}{money(sum.fuel)}{:else}<a class="set" href="#price-title">{t('energy.setPrice')}</a>{/if}</dd>
			</div>
			<div><dt>{t('energy.heat')}</dt><dd>{fmt.number(sum.heatKWh, 0)} kWh</dd></div>
			<div><dt>{t('energy.burner')}</dt><dd>{fmt.duration(sum.burnerMin * 60)}</dd></div>
		</dl>

		<div class="panel chart" class:stale={loading}>
			<p class="chart-title">{t('energy.chartTitle', { unit: fmt.fuelUnit(unit) })}</p>
			<BarChart
				bars={items}
				label={view === 'days' ? t('energy.chartDays', { month: fmt.month(month) }) : t('energy.chartMonths')}
				format={(v) => fmt.number(v, v > 0 && v < 10 ? 1 : 0)}
				{describe}
				hint={sum.days === 0 ? t('energy.empty') : t('report.hint')}
			/>
		</div>

		<details class="table-twin">
			<summary>{view === 'days' ? t('energy.tableDays') : t('energy.tableMonths')}</summary>
			<div class="scroll">
				<table>
					<thead>
						<tr>
							<th scope="col">{view === 'days' ? t('energy.day') : t('energy.month')}</th>
							<th scope="col">{t('energy.fuel')}</th>
							{#if fuelPrice !== null}<th scope="col">{t('energy.cost')}</th>{/if}
							<th scope="col">{t('energy.heat')}</th>
							<th scope="col">{t('energy.burner')}</th>
						</tr>
					</thead>
					<tbody>
						{#each bars.filter((b) => b.total !== null) as b (b.key)}
							<tr>
								<th scope="row">{barName(b)}</th>
								<td class="num">{fmt.fuel(b.total?.fuel ?? 0, unit)}</td>
								{#if fuelPrice !== null}<td class="num">{money(b.total?.fuel ?? 0)}</td>{/if}
								<td class="num">{fmt.number(b.total?.heatKWh ?? 0, 1, 1)} kWh</td>
								<td class="num">{fmt.duration((b.total?.burnerMin ?? 0) * 60)}</td>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
		</details>

		<p class="note">{t('energy.note')}</p>

		<section class="section" aria-labelledby="price-title">
			<div class="section-head"><h2 id="price-title">{t('energy.price')}</h2></div>
			<form class="panel price" onsubmit={savePrice}>
				<label class="field">
					<span>{t('energy.pricePer', { unit: fmt.fuelUnit(unit) })}</span>
					<input class="input num" class:error={!priceOk} inputmode="decimal" autocomplete="off" bind:value={price} placeholder="1,05" />
				</label>
				<label class="field">
					<span>{t('energy.currency')}</span>
					<select class="input" bind:value={currency}>
						{#each CURRENCIES.includes(currency) ? CURRENCIES : [currency, ...CURRENCIES] as c (c)}
							<option value={c}>{c}</option>
						{/each}
					</select>
				</label>
				<button class="btn primary" type="submit" disabled={busy || !priceOk}><Icon name="check" size={18} />{t('energy.savePrice')}</button>
				<p class="note">{t('energy.priceNote')}</p>
			</form>
		</section>
	{:else if loading}
		<p class="muted">{t('state.loading')}</p>
	{/if}
</section>

<style>
	.controls {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: 0.75rem 1.25rem;
	}

	.month-nav {
		display: flex;
		align-items: center;
		gap: 0.5rem;
	}

	.period {
		min-width: 9rem;
		margin: 0;
		text-align: center;
		font-family: var(--font-display);
		font-weight: var(--weight-semibold);
		font-size: 1.09375rem;
	}

	.next :global(svg) {
		rotate: 180deg;
	}


	.stat-tiles .set {
		font-family: var(--font-body);
		font-size: var(--text-small-size);
		color: var(--link);
	}

	.chart {
		gap: 0.5rem;
		transition: opacity 0.2s;
	}

	.chart.stale {
		opacity: 0.6;
	}

	.chart-title {
		margin: 0;
		font-weight: var(--weight-semibold);
		font-size: var(--text-small-size);
	}

	.panel.off {
		display: grid;
		gap: 0.75rem;
		justify-items: start;
	}

	.panel.off h2,
	.panel.off p {
		margin: 0;
	}

	.price {
		display: flex;
		flex-wrap: wrap;
		align-items: end;
		gap: 0.75rem 1rem;
	}

	.price .field {
		flex: 1 1 9rem;
	}

	.price .note {
		flex-basis: 100%;
		margin: 0;
	}


	/* Below 900 px a row of controls fills the width. */
	@media (max-width: 899px) {
		.controls > *,
		.month-nav {
			flex: 1 1 100%;
		}

		.segmented label,
		.period {
			flex: 1;
			min-width: 0;
		}
	}

	@media (prefers-reduced-motion: reduce) {
		.chart {
			transition: none;
		}
	}
</style>
