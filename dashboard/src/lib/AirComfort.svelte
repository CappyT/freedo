<script lang="ts">
	import { COMFORT, comfortOf, type ComfortVerdict } from './comfort.ts';
	import { fmt, t } from './i18n.svelte.ts';

	interface Props {
		tempC: number | null;
		humidityPct: number | null;
	}

	let { tempC, humidityPct }: Props = $props();
	const comfort = $derived(comfortOf(tempC, humidityPct));

	const tone: Record<ComfortVerdict, string> = {
		pleasant: 'var(--status-idle-text)',
		cold: 'var(--status-warning-text)',
		warm: 'var(--status-warning-text)',
		dry: 'var(--status-warning-text)',
		humid: 'var(--status-warning-text)',
		mould: 'var(--status-fault-text)'
	};

	// The map spans the temperatures and humidities a heated room meets. A reading outside sits on the edge.
	const T = { min: 14, max: 30 };
	const H = { min: 10, max: 90 };
	const W = 300;
	const PLOT_H = 132;
	const LEFT = 30;
	const BOTTOM = 22;
	const x = (c: number) => LEFT + ((Math.min(T.max, Math.max(T.min, c)) - T.min) / (T.max - T.min)) * (W - LEFT - 16);
	const y = (pct: number) => 6 + (1 - (Math.min(H.max, Math.max(H.min, pct)) - H.min) / (H.max - H.min)) * (PLOT_H - 6);
	const tTicks = [14, 18, 22, 26, 30];
	const hTicks = [30, 60, 90];
</script>

{#if comfort}
	<ul class="list" data-testid="air-comfort">
		<li class="list-row">
			<span class="list-main">
				<span class="list-label">{t(`comfort.${comfort.verdict}`)}</span>
				<span class="list-sub">{t(`comfort.${comfort.verdict}.hint`)}</span>
			</span>
			<span class="list-value"><span class="dot" style:--dot={tone[comfort.verdict]}></span><span class="num">{[tempC === null ? null : fmt.degrees(tempC), humidityPct === null ? null : fmt.pct(humidityPct)].filter(Boolean).join(' · ')}</span></span>
		</li>
		{#if tempC !== null && humidityPct !== null}
			<li class="map">
				<svg viewBox="0 0 {W} {PLOT_H + BOTTOM}" role="img" aria-label={t('comfort.map', { temp: fmt.degrees(tempC), humidity: fmt.pct(humidityPct) })}>
					{#each hTicks as h (h)}
						<line class="grid" x1={LEFT} x2={W - 16} y1={y(h)} y2={y(h)} />
						<text class="tick" x={LEFT - 6} y={y(h) + 4} text-anchor="end">{h}%</text>
					{/each}
					{#each tTicks as c (c)}
						<text class="tick" x={x(c)} y={PLOT_H + 16} text-anchor="middle">{c}°</text>
					{/each}
					<rect
						class="zone"
						x={x(COMFORT.coldBelowC)}
						y={y(COMFORT.humidAbovePct)}
						width={x(COMFORT.warmAboveC) - x(COMFORT.coldBelowC)}
						height={y(COMFORT.dryBelowPct) - y(COMFORT.humidAbovePct)}
						rx="8"
					/>
					<line class="mould" x1={LEFT} x2={W - 16} y1={y(COMFORT.mouldFromPct)} y2={y(COMFORT.mouldFromPct)} />
					<circle class="now" cx={x(tempC)} cy={y(humidityPct)} r="5" />
				</svg>
				<p class="note">{t('comfort.legend')}</p>
			</li>
		{/if}
	</ul>
{/if}

<style>
	.map {
		display: grid;
		gap: 0.25rem;
		padding: 0.75rem 0.875rem 0.875rem;
	}

	svg {
		width: 100%;
		max-width: 26rem;
		height: auto;
		justify-self: center;
	}

	.grid {
		stroke: var(--border);
		stroke-width: 1;
	}

	.tick {
		fill: var(--text-muted);
		font-family: var(--font-mono);
		font-size: 11px;
	}

	.zone {
		fill: var(--status-idle-tint);
		stroke: var(--status-idle-text);
		stroke-width: 1;
	}

	.mould {
		stroke: var(--status-fault-text);
		stroke-width: 1;
		opacity: 0.6;
	}

	/* The surface ring keeps the dot readable where it crosses the zone edge. */
	.now {
		fill: var(--text);
		stroke: var(--surface);
		stroke-width: 2;
	}
</style>
