<script lang="ts">
	import { resolve } from '$app/paths';
	import { act, errorText } from '#lib/actions.ts';
	import {
		ADVICE_WINDOWS,
		COMFORT_MAX_C,
		DEFAULT_ADVICE_WINDOW,
		MODEL_DAYS,
		adviceKeys,
		adviceLink,
		adviceParams,
		confidenceTone,
		rangeText,
		splitRows,
		type AdviceWindow
	} from '#lib/advice.ts';
	import BarChart, { type BarItem } from '#lib/BarChart.svelte';
	import { data } from '#lib/data.svelte.ts';
	import { cost, localDay, monthBars, shiftMonth, total, yearBars, type Bar } from '#lib/history.ts';
	import { fmt, t, type Key } from '#lib/i18n.svelte.ts';
	import { prefs } from '#lib/prefs.svelte.ts';
	import Icon from '#lib/Icon.svelte';
	import { api } from '#lib/session.ts';
	import type { Advice, EnergyHistory, HistoryAdvice, HistoryInfo } from '#lib/types.ts';

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
	let comfort = $state<number | null>(null);
	let adviceDays = $state<AdviceWindow>(DEFAULT_ADVICE_WINDOW);
	let advice = $state<HistoryAdvice | null>(null);
	let adviceError = $state<string | null>(null);
	let adviceLoading = $state(false);
	let roomNames = $state<Record<number, string>>({});
	let adviceAsked = 0;

	async function load() {
		loading = true;
		error = null;
		try {
			info = await api.history();
			energy = info.enabled ? await api.energyHistory() : null;
			price = info.fuelPrice === null ? '' : String(info.fuelPrice).replace('.', fmt.decimal());
			currency = info.currency;
			comfort = info.comfortMaxC ?? null;
		} catch (e) {
			error = errorText(e);
		} finally {
			loading = false;
		}
	}

	$effect(() => {
		void load();
	});

	// A late answer must not overwrite the answer of a newer window.
	async function loadAdvice(days: AdviceWindow) {
		const asked = ++adviceAsked;
		adviceLoading = true;
		try {
			const [result, zones] = await Promise.all([api.historyAdvice(days), api.installZones()]);
			if (asked !== adviceAsked) return;
			advice = result;
			roomNames = Object.fromEntries(zones.map((z) => [z.number, z.name]));
			adviceError = null;
		} catch (e) {
			if (asked !== adviceAsked) return;
			advice = null;
			adviceError = errorText(e);
		} finally {
			if (asked === adviceAsked) adviceLoading = false;
		}
	}

	// Saving the price or the comfort limit reads the info again, and the advice with it.
	$effect(() => {
		if (info?.enabled) void loadAdvice(adviceDays);
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

	const comfortOk = $derived(comfort === null || (Number.isFinite(comfort) && comfort >= COMFORT_MAX_C.min && comfort <= COMFORT_MAX_C.max));

	async function saveComfort(event: SubmitEvent) {
		event.preventDefault();
		if (!comfortOk) return;
		busy = true;
		if (await act(() => api.setHistory({ comfortMaxC: comfort }), t('energy.comfortSaved'))) await load();
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

	const adviceUnit = $derived(advice?.unit ?? unit);
	const adviceMoney = (v: number) => fmt.money(v, advice?.currency ?? currency);
	const rows = $derived(advice ? splitRows(advice.split) : []);
	const roomName = (n: number) => roomNames[n] ?? `#${n}`;
	const textContext = {
		name: (n: number) => roomName(n),
		list: (names: string[]) => new Intl.ListFormat(prefs.locale, { style: 'long', type: 'conjunction' }).format(names),
		number: (v: number, digits: number) => fmt.number(v, digits),
		temp: (v: number) => fmt.temp(v)
	};
	const rowName = (r: (typeof rows)[number]) =>
		r.kind === 'zone' ? roomName(r.zone ?? 0) : t(r.kind === 'hotWater' ? 'advice.hotWater' : 'advice.unattributed');
	// Fuel the change would have saved, against fuel that the event spent.
	const saves = (a: Advice) => a.kind === 'setpoint' || a.kind === 'setback';
	const fuelRange = (a: Advice) => (a.fuel ? `${rangeText(a.fuel, (v) => fmt.number(v, 2, 2))} ${fmt.fuelUnit(adviceUnit)}` : null);
	const shareText = (r: (typeof rows)[number], price: number | null) =>
		[fmt.fuel(r.fuel, adviceUnit), ...(price === null ? [] : [adviceMoney(r.fuel * price)]), fmt.percent(r.fraction * 100)].join(' · ');
	const hrefOf = (a: Advice) => {
		const link = adviceLink(a);
		return link.route === '/rooms/[n]' ? resolve(link.route, link.params) : resolve(link.route);
	};
	const costRange = (a: Advice) => (a.cost ? rangeText(a.cost, adviceMoney) : null);

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

		<section class="section" aria-labelledby="split-title">
			<div class="section-head">
				<h2 id="split-title">{t('advice.split')}</h2>
				<fieldset class="segmented" data-testid="advice-window">
					<legend class="sr-only">{t('advice.period')}</legend>
					{#each ADVICE_WINDOWS as d (d)}
						<label><input type="radio" name="advice-days" value={d} bind:group={adviceDays} />{t('advice.days', { n: d })}</label>
					{/each}
				</fieldset>
			</div>
			{#if adviceError}
				<p class="notice tone-fault" role="alert"><Icon name="fault" /><span>{adviceError}</span></p>
			{:else if advice}
				<div class="panel split" class:stale={adviceLoading} data-testid="advice-split">
					{#if rows.length === 0}
						<p class="muted">{t('advice.splitEmpty')}</p>
					{:else}
						<ul class="shares">
							{#each rows as r (r.kind + (r.zone ?? ''))}
								<li data-kind={r.kind}>
									<div class="share-head">
										<span class="share-name">{rowName(r)}</span>
										<span class="num">{shareText(r, advice.fuelPrice)}</span>
									</div>
									<div class="track" aria-hidden="true"><div class="fill {r.kind}" style:width="{r.fraction * 100}%"></div></div>
								</li>
							{/each}
						</ul>
					{/if}
					{#if !advice.split.weighted && advice.split.zones.length > 0}
						<p class="note" data-testid="advice-unweighted">
							{t('advice.unweighted')} <a href={resolve('/devices')}>{t('rooms.settings')}</a>
						</p>
					{/if}
				</div>
			{:else}
				<p class="muted">{t('state.loading')}</p>
			{/if}
		</section>

		<section class="section" aria-labelledby="suggestions-title">
			<div class="section-head"><h2 id="suggestions-title">{t('advice.suggestions')}</h2></div>
			{#if advice}
				{#if !advice.model.usable}
					<p class="notice tone-idle" data-testid="advice-not-ready">
						<Icon name="warning" /><span>{t('advice.notReady', { needed: MODEL_DAYS, validDays: advice.model.validDays })}</span>
					</p>
				{/if}
				{#if advice.advice.length === 0}
					<p class="muted" data-testid="advice-none">{t('advice.none')}</p>
				{:else}
					<ul class="list" class:stale={adviceLoading} data-testid="advice-list">
						{#each advice.advice as a (a.id)}
							{@const keys = adviceKeys(a)}
							{@const fuelText = fuelRange(a)}
							{@const costText = costRange(a)}
							<li>
								<a class="list-row advice" href={hrefOf(a)} data-kind={a.kind}>
									<span class="list-main">
										<span class="list-label">{t(keys.title)}</span>
										<span class="list-sub">{t(keys.body, adviceParams(a, textContext))}</span>
										{#if fuelText}
											<span class="list-sub num" data-testid="advice-range">
												{t(saves(a) ? 'advice.saves' : 'advice.spent', { range: fuelText })}{costText ? ` · ${costText}` : ''}
											</span>
										{/if}
									</span>
									<span class="badge tone-{confidenceTone[a.confidence]}">{t(`advice.confidence.${a.confidence}`)}</span>
									<span class="chevron"><Icon name="chevron" size={18} /></span>
								</a>
							</li>
						{/each}
					</ul>
				{/if}
			{:else if !adviceError}
				<p class="muted">{t('state.loading')}</p>
			{/if}
		</section>

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
			<form class="panel price" onsubmit={saveComfort}>
				<label class="field">
					<span>{t('energy.comfortMax')}</span>
					<input
						class="input num"
						class:error={!comfortOk}
						type="number"
						inputmode="decimal"
						min={COMFORT_MAX_C.min}
						max={COMFORT_MAX_C.max}
						step="0.5"
						autocomplete="off"
						name="comfortMaxC"
						bind:value={comfort}
					/>
				</label>
				<button class="btn primary" type="submit" disabled={busy || !comfortOk}><Icon name="check" size={18} />{t('energy.saveComfort')}</button>
				<p class="note">{t('energy.comfortMax.hint')}</p>
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

	.shares {
		display: grid;
		gap: 0.75rem;
		margin: 0;
		padding: 0;
		list-style: none;
	}

	.share-head {
		display: flex;
		flex-wrap: wrap;
		justify-content: space-between;
		gap: 0.25rem 1rem;
		font-size: var(--text-small-size);
	}

	.share-name {
		font-weight: var(--weight-semibold);
	}

	.track {
		height: 0.5rem;
		margin-top: 0.3rem;
		border-radius: 0.25rem;
		background: var(--border);
		overflow: hidden;
	}

	.fill {
		height: 100%;
		border-radius: 0.25rem;
		background: var(--status-heating);
	}

	.fill.hotWater {
		background: var(--status-away);
	}

	.fill.unattributed {
		background: var(--status-off);
	}

	.split.stale,
	.list.stale {
		opacity: 0.6;
	}

	.advice .badge {
		flex: none;
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
