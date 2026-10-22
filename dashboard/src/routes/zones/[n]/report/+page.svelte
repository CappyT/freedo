<script lang="ts">
	import { page } from '$app/state';
	import { resolve } from '$app/paths';
	import { act, errorText } from '#lib/actions.ts';
	import { data } from '#lib/data.svelte.ts';
	import DayChart from '#lib/DayChart.svelte';
	import { localDay, nearest, shiftDay, summarize } from '#lib/history.ts';
	import { fmt, t, type Key } from '#lib/i18n.svelte.ts';
	import Icon from '#lib/Icon.svelte';
	import { api } from '#lib/session.ts';
	import type { HeatUp, HistoryInfo, ZoneHistory } from '#lib/types.ts';

	const number = $derived(Number(page.params.n));
	const model = $derived(data.heating.find((m) => m.zone.number === number));
	const today = localDay(new Date());

	let date = $state(today);
	let info = $state<HistoryInfo | null>(null);
	let day = $state<ZoneHistory | null>(null);
	let error = $state<string | null>(null);
	let loading = $state(true);
	let busy = $state(false);

	async function load(n: number, d: string) {
		loading = true;
		error = null;
		try {
			info = await api.history();
			day = info.enabled ? await api.zoneHistory(n, d) : null;
		} catch (e) {
			error = errorText(e);
		} finally {
			loading = false;
		}
	}

	$effect(() => {
		void load(number, date);
	});

	async function enable() {
		busy = true;
		if (await act(() => api.setHistory({ enabled: true }), t('history.enabled'))) await load(number, date);
		busy = false;
	}

	const summary = $derived(day ? summarize(day.samples, day.intervalSec) : null);
	const fromMs = $derived(day ? Date.parse(day.from) : 0);
	const toMs = $derived(day ? Date.parse(day.to) : 0);
	const canBack = $derived(day?.first !== null && day?.first !== undefined && date > day.first);
	// One row per hour for the table twin of the chart: the sample nearest to the full hour.
	const hours = $derived.by(() => {
		if (!day || day.samples.length === 0) return [];
		const rows = [];
		for (let ms = fromMs; ms < toMs; ms += 3_600_000) {
			const s = nearest(day.samples, ms);
			if (s && Math.abs(s.atMs - ms) <= day.intervalSec * 1000) rows.push(s);
		}
		return rows;
	});

	const endText: Record<string, Key> = {
		'target changed': 'report.end.changed',
		'program block off': 'report.end.changed',
		'no program block': 'report.end.changed',
		'heat-up': 'report.end.changed',
		'offset changed': 'report.end.offset',
		'not settled': 'report.end.notSettled',
		'target not reached': 'report.end.notReached',
		'no zone report': 'report.end.noReport',
		'sample limit': 'report.end.noReport',
		away: 'report.end.away',
		'open window': 'report.end.window',
		overlay: 'report.end.overlay'
	};

	function heatUpLine(ep: HeatUp): string {
		const parts: string[] = [];
		if (ep.crossedMs !== null) {
			parts.push(t('report.reachedIn', { time: fmt.duration((ep.crossedMs - ep.startedMs) / 1000) }));
			if (ep.overshootC !== null && ep.overshootC >= 0.1) parts.push(t('report.overshoot', { temp: fmt.degrees(ep.overshootC) }));
		} else {
			parts.push(t('report.notReached'));
		}
		const why = endText[ep.endReason];
		if (ep.endReason !== 'settled' && why) parts.push(t(why));
		return parts.join(' · ');
	}
</script>

<section class="page">
	<a class="section-link" href={resolve('/zones/[n]', { n: String(number) })}><Icon name="back" size={16} />{model?.zone.name ?? t('zone.back')}</a>
	<div class="page-head titled">
		<h1>{t('report.title')}</h1>
	</div>

	<div class="day-nav">
		<button class="btn icon" aria-label={t('report.previous')} disabled={!canBack || loading} onclick={() => (date = shiftDay(date, -1))}>
			<Icon name="back" size={20} />
		</button>
		<p class="day-label">{date === today ? t('report.today') : fmt.longDay(date)}</p>
		<button
			class="btn icon next"
			aria-label={t('report.next')}
			disabled={date >= today || loading}
			onclick={() => (date = shiftDay(date, 1))}
		>
			<Icon name="back" size={20} />
		</button>
		{#if date !== today}<button class="btn" onclick={() => (date = today)}>{t('report.toToday')}</button>{/if}
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
	{:else if day && summary}
		<div class="report" class:stale={loading}>
			{#if day.samples.length === 0}
				<p class="notice tone-off"><Icon name="clock" /><span>{t('report.empty')}</span></p>
			{:else}
				<dl class="stat-tiles">
					<div><dt>{t('report.heating')}</dt><dd>{fmt.duration(summary.heatingMin * 60)}</dd></div>
					<div><dt>{t('report.mean')}</dt><dd>{fmt.degrees(summary.meanC)}</dd></div>
					<div><dt>{t('report.range')}</dt><dd>{fmt.degrees(summary.minC)} / {fmt.degrees(summary.maxC)}</dd></div>
					{#if summary.meanHumidity !== null}
						<div><dt>{t('report.humidity')}</dt><dd>{fmt.pct(summary.meanHumidity)}</dd></div>
					{/if}
					{#if summary.windowMin > 0}
						<div><dt>{t('report.window')}</dt><dd>{fmt.duration(summary.windowMin * 60)}</dd></div>
					{/if}
					{#if summary.awayMin > 0}
						<div><dt>{t('state.away')}</dt><dd>{fmt.duration(summary.awayMin * 60)}</dd></div>
					{/if}
				</dl>

				<div class="panel">
					<DayChart
						samples={day.samples}
						{fromMs}
						{toMs}
						intervalSec={day.intervalSec}
						label={t('report.chartLabel', { room: model?.zone.name ?? '', day: fmt.longDay(day.date) })}
					/>
				</div>

				<details class="table-twin">
					<summary>{t('report.table')}</summary>
					<div class="scroll">
						<table>
							<thead>
								<tr>
									<th scope="col">{t('report.time')}</th>
									<th scope="col">{t('report.temp')}</th>
									<th scope="col">{t('report.target')}</th>
									<th scope="col">{t('report.demand')}</th>
									<th scope="col">{t('card.humidity')}</th>
								</tr>
							</thead>
							<tbody>
								{#each hours as s (s.atMs)}
									<tr>
										<th scope="row" class="num">{fmt.clock(s.atMs)}</th>
										<td class="num">{fmt.degrees(s.tempC)}</td>
										<td class="num">{s.on ? fmt.degrees(s.targetC) : t('state.off')}</td>
										<td class="num">{fmt.pct(s.demandPct)}</td>
										<td class="num">{fmt.pct(s.humidityPct)}</td>
									</tr>
								{/each}
							</tbody>
						</table>
					</div>
				</details>
			{/if}

			<section class="section" aria-labelledby="heatups-title">
				<div class="section-head"><h2 id="heatups-title">{t('report.heatUps')}</h2></div>
				{#if day.episodes.length === 0}
					<p class="muted">{t('report.noHeatUps')}</p>
				{:else}
					<ul class="list">
						{#each day.episodes as ep (ep.startedMs)}
							<li class="list-row">
								<span class="disc heat"><Icon name="flame" size={18} /></span>
								<span class="list-main">
									<span class="list-label">
										{t('report.heatUp', { time: fmt.clock(ep.startedMs), from: fmt.degrees(ep.startC), to: fmt.degrees(ep.targetC) })}
									</span>
									<span class="list-sub">{heatUpLine(ep)}</span>
								</span>
							</li>
						{/each}
					</ul>
				{/if}
				<p class="note">{t('report.heatUpNote')}</p>
			</section>
		</div>
	{:else if loading}
		<p class="muted">{t('state.loading')}</p>
	{/if}
</section>

<style>
	.day-nav {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: 0.5rem;
	}

	.day-label {
		min-width: 12rem;
		margin: 0;
		text-align: center;
		font-family: var(--font-display);
		font-weight: var(--weight-semibold);
		font-size: 1.09375rem;
	}

	.next :global(svg) {
		rotate: 180deg;
	}

	/* Below 900 px a row of buttons fills the width. */
	@media (max-width: 899px) {
		.day-label {
			flex: 1;
			min-width: 0;
		}
	}

	.report {
		display: grid;
		gap: 1.25rem;
		transition: opacity 0.2s;
	}

	.report.stale {
		opacity: 0.6;
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

	.disc.heat {
		background: var(--status-heating-tint);
		color: var(--status-heating-text);
	}


	@media (prefers-reduced-motion: reduce) {
		.report {
			transition: none;
		}
	}
</style>
