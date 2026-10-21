<script lang="ts">
	import { nearest, runs, spans, tempRange, type Point } from './history.ts';
	import { fmt, t } from './i18n.svelte.ts';
	import type { HistorySample } from './types.ts';

	interface Props {
		samples: HistorySample[];
		fromMs: number;
		toMs: number;
		intervalSec: number;
		label: string;
	}

	let { samples, fromMs, toMs, intervalSec, label }: Props = $props();

	// The plot is drawn at the width of its box, so the text keeps its size on every screen.
	let width = $state(640);
	const W = $derived(Math.max(280, width));
	const LEFT = 34;
	const RIGHT = 10;
	const TEMP = { top: 8, h: 168 };
	const DEMAND = $derived({ top: TEMP.top + TEMP.h + 30, h: 40 });
	const HUMID = $derived({ top: DEMAND.top + DEMAND.h + 30, h: 60 });
	const step = $derived(intervalSec * 1000);
	const hasHumidity = $derived(samples.some((s) => s.humidityPct !== null));
	const bottom = $derived(hasHumidity ? HUMID.top + HUMID.h : DEMAND.top + DEMAND.h);
	const H = $derived(bottom + 22);

	const x = (ms: number) => LEFT + ((ms - fromMs) / (toMs - fromMs)) * (W - LEFT - RIGHT);
	const temps = $derived(samples.flatMap((s) => [s.tempC, s.targetC].filter((v): v is number => v !== null)));
	const range = $derived(tempRange(temps));
	const yT = (c: number) => TEMP.top + (1 - (c - range.min) / (range.max - range.min)) * TEMP.h;
	const tempTicks = $derived.by(() => {
		const every = range.max - range.min > 8 ? 4 : 2;
		const out: number[] = [];
		for (let v = Math.ceil(range.min / every) * every; v <= range.max; v += every) out.push(v);
		return out;
	});
	const yD = (pct: number) => DEMAND.top + (1 - pct / 100) * DEMAND.h;
	const humid = $derived.by(() => {
		const values = samples.flatMap((s) => (s.humidityPct === null ? [] : [s.humidityPct]));
		if (values.length === 0) return { min: 0, max: 100 };
		let min = Math.max(0, Math.floor(Math.min(...values) / 10) * 10);
		let max = Math.min(100, Math.ceil(Math.max(...values) / 10) * 10);
		if (max - min < 20) {
			max = Math.min(100, min + 20);
			min = Math.max(0, max - 20);
		}
		return { min, max };
	});
	const yH = (pct: number) => HUMID.top + (1 - (pct - humid.min) / (humid.max - humid.min)) * HUMID.h;

	const line = (points: Point[], y: (v: number) => number) =>
		points.map((p, i) => `${i === 0 ? 'M' : 'L'}${x(p.atMs).toFixed(1)},${y(p.v).toFixed(1)}`).join('');
	// A target holds from its sample to the next one, so it draws as steps.
	const stepLine = (points: Point[], y: (v: number) => number) =>
		points
			.map((p, i) => (i === 0 ? `M${x(p.atMs).toFixed(1)},${y(p.v).toFixed(1)}` : `H${x(p.atMs).toFixed(1)}V${y(p.v).toFixed(1)}`))
			.join('') + `H${x((points.at(-1)?.atMs ?? fromMs) + step).toFixed(1)}`;
	// The area under a stepped value, down to base: the heating demand, and the target as a soft band.
	const stepArea = (points: Point[], y: (v: number) => number, base: number) =>
		`M${x(points[0]?.atMs ?? fromMs).toFixed(1)},${base}` +
		points.map((p) => `V${y(p.v).toFixed(1)}H${x(p.atMs + step).toFixed(1)}`).join('') +
		`V${base}Z`;

	const tempLines = $derived(runs(samples, (s) => s.tempC, step).map((r) => line(r, yT)));
	const targetRuns = $derived(runs(samples, (s) => (s.on ? s.targetC : null), step));
	const targetLines = $derived(targetRuns.map((r) => stepLine(r, yT)));
	const targetAreas = $derived(targetRuns.map((r) => stepArea(r, yT, TEMP.top + TEMP.h)));
	const demandAreas = $derived(runs(samples, (s) => s.demandPct, step).map((r) => stepArea(r, yD, yD(0))));
	const humidLines = $derived(runs(samples, (s) => s.humidityPct, step).map((r) => line(r, yH)));
	const windows = $derived(spans(samples, (s) => s.window, step));
	const aways = $derived(spans(samples, (s) => s.away, step));
	const hourTicks = $derived([0, 6, 12, 18, 24].map((h) => fromMs + h * 3_600_000).filter((ms) => ms <= toMs));

	let hover = $state<number | null>(null);
	const hovered = $derived(hover === null ? null : (samples[hover] ?? null));

	function move(event: PointerEvent) {
		const box = (event.currentTarget as SVGSVGElement).getBoundingClientRect();
		const px = ((event.clientX - box.left) / box.width) * W;
		const ms = fromMs + ((px - LEFT) / (W - LEFT - RIGHT)) * (toMs - fromMs);
		const s = nearest(samples, ms);
		hover = s === null ? null : samples.indexOf(s);
	}

	function key(event: KeyboardEvent) {
		if (samples.length === 0) return;
		const last = samples.length - 1;
		const moves: Record<string, number> = { ArrowLeft: -1, ArrowRight: 1, PageUp: -12, PageDown: 12 };
		const by = moves[event.key];
		if (by !== undefined) hover = Math.min(last, Math.max(0, (hover ?? last) + by));
		else if (event.key === 'Home') hover = 0;
		else if (event.key === 'End') hover = last;
		else if (event.key === 'Escape') hover = null;
		else return;
		event.preventDefault();
	}

	const readout = $derived.by(() => {
		if (!hovered) return null;
		const parts = [fmt.clock(hovered.atMs), fmt.degrees(hovered.tempC)];
		parts.push(hovered.on ? t('report.targetAt', { temp: fmt.degrees(hovered.targetC) }) : t('state.off'));
		if (hovered.demandPct !== null) parts.push(t('report.demandAt', { pct: fmt.pct(hovered.demandPct) }));
		if (hovered.humidityPct !== null) parts.push(t('report.humidityAt', { pct: fmt.pct(hovered.humidityPct) }));
		if (hovered.window) parts.push(t('report.window'));
		if (hovered.away) parts.push(t('state.away'));
		return parts.join(' · ');
	});
</script>

<div class="chart" bind:clientWidth={width}>
	<ul class="legend" aria-hidden="true">
		<li><span class="key line"></span>{t('report.temp')}</li>
		<li><span class="key line target"></span>{t('report.target')}</li>
		<li><span class="key box heat"></span>{t('report.demand')}</li>
		{#if windows.length > 0}<li><span class="key box window"></span>{t('report.window')}</li>{/if}
		{#if aways.length > 0}<li><span class="key box away"></span>{t('state.away')}</li>{/if}
	</ul>
	<figure>
		<!-- A slider over the samples: the arrow keys move the readout, which is also its value text. -->
		<svg
			viewBox="0 0 {W} {H}"
			width={W}
			height={H}
			role="slider"
			aria-label={label}
			aria-valuemin={0}
			aria-valuemax={Math.max(0, samples.length - 1)}
			aria-valuenow={hover ?? Math.max(0, samples.length - 1)}
			aria-valuetext={readout ?? t('report.hint')}
			tabindex="0"
			onpointermove={move}
			onpointerleave={() => (hover = null)}
			onkeydown={key}
			onblur={() => (hover = null)}
		>
			{#each windows as s (s.startMs)}
				<rect class="band window" x={x(s.startMs)} y={TEMP.top} width={x(s.endMs) - x(s.startMs)} height={TEMP.h} />
			{/each}
			{#each aways as s (s.startMs)}
				<rect class="band away" x={x(s.startMs)} y={TEMP.top} width={x(s.endMs) - x(s.startMs)} height={TEMP.h} />
			{/each}
			{#each tempTicks as v (v)}
				<line class="grid" x1={LEFT} x2={W - RIGHT} y1={yT(v)} y2={yT(v)} />
				<text class="tick" x={LEFT - 6} y={yT(v) + 4} text-anchor="end">{v}°</text>
			{/each}
			{#each targetAreas as d, i (i)}<path class="target-area" {d} />{/each}
			{#each targetLines as d, i (i)}<path class="target" {d} />{/each}
			{#each tempLines as d, i (i)}<path class="temp" {d} />{/each}

			<text class="panel" x={LEFT} y={DEMAND.top - 6}>{t('report.demand')}</text>
			<line class="grid" x1={LEFT} x2={W - RIGHT} y1={yD(0)} y2={yD(0)} />
			<line class="grid" x1={LEFT} x2={W - RIGHT} y1={yD(100)} y2={yD(100)} />
			<text class="tick" x={LEFT - 6} y={yD(100) + 4} text-anchor="end">100%</text>
			<text class="tick" x={LEFT - 6} y={yD(0) + 4} text-anchor="end">0%</text>
			{#each demandAreas as d, i (i)}<path class="heat" {d} />{/each}

			{#if hasHumidity}
				<text class="panel" x={LEFT} y={HUMID.top - 6}>{t('card.humidity')}</text>
				<line class="grid" x1={LEFT} x2={W - RIGHT} y1={yH(humid.max)} y2={yH(humid.max)} />
				<line class="grid" x1={LEFT} x2={W - RIGHT} y1={yH(humid.min)} y2={yH(humid.min)} />
				<text class="tick" x={LEFT - 6} y={yH(humid.max) + 4} text-anchor="end">{humid.max}%</text>
				<text class="tick" x={LEFT - 6} y={yH(humid.min) + 4} text-anchor="end">{humid.min}%</text>
				{#each humidLines as d, i (i)}<path class="humid" {d} />{/each}
			{/if}

			{#each hourTicks as ms, i (ms)}
				<text class="tick" x={x(ms)} y={H - 6} text-anchor={i === 0 ? 'start' : i === hourTicks.length - 1 ? 'end' : 'middle'}>
					{String(i * 6).padStart(2, '0')}
				</text>
			{/each}

			{#if hovered}
				<line class="cross" x1={x(hovered.atMs)} x2={x(hovered.atMs)} y1={TEMP.top} y2={bottom} />
				{#if hovered.tempC !== null}<circle class="mark" cx={x(hovered.atMs)} cy={yT(hovered.tempC)} r="4" />{/if}
				{#if hasHumidity && hovered.humidityPct !== null}
					<circle class="mark" cx={x(hovered.atMs)} cy={yH(hovered.humidityPct)} r="4" />
				{/if}
			{/if}
		</svg>
		<figcaption class="caption" aria-live="polite">{readout ?? t('report.hint')}</figcaption>
	</figure>
</div>

<style>
	.chart,
	figure {
		display: grid;
		gap: 0.5rem;
		min-width: 0;
		margin: 0;
	}

	svg {
		display: block;
		max-width: 100%;
		height: auto;
		touch-action: pan-y;
		border-radius: var(--radius-xs);
	}

	svg:focus-visible {
		outline: 3px solid var(--accent);
		outline-offset: 2px;
	}

	.legend {
		display: flex;
		flex-wrap: wrap;
		gap: 0.25rem 1rem;
		margin: 0;
		padding: 0;
		list-style: none;
		font-size: var(--text-small-size);
		color: var(--text-muted);
	}

	.legend li {
		display: inline-flex;
		align-items: center;
		gap: 0.375rem;
	}

	.key {
		display: inline-block;
	}

	.key.line {
		width: 1rem;
		height: 2px;
		background: var(--text);
	}

	.key.line.target {
		background: repeating-linear-gradient(90deg, var(--text-muted) 0 4px, transparent 4px 7px);
	}

	.key.box {
		width: 0.75rem;
		height: 0.75rem;
		border-radius: 3px;
	}

	.key.heat {
		background: var(--status-heating);
	}

	.key.window {
		background: var(--status-warning-tint);
		box-shadow: inset 0 0 0 1px var(--status-warning-text);
	}

	.key.away {
		background: var(--status-away-tint);
		box-shadow: inset 0 0 0 1px var(--status-away-text);
	}

	.grid {
		stroke: var(--border);
		stroke-width: 1;
	}

	.tick,
	.panel {
		fill: var(--text-muted);
		font-size: 11px;
	}

	.tick {
		font-family: var(--font-mono);
	}

	.panel {
		font-weight: var(--weight-semibold);
	}

	.band.window {
		fill: var(--status-warning-tint);
	}

	.band.away {
		fill: var(--status-away-tint);
	}

	.temp,
	.target,
	.humid {
		fill: none;
		stroke-width: 2;
		stroke-linejoin: round;
		stroke-linecap: round;
	}

	.temp,
	.humid {
		stroke: var(--text);
	}

	.target {
		stroke: var(--text-muted);
		stroke-width: 1.5;
		stroke-dasharray: 4 3;
	}

	.target-area {
		fill: color-mix(in srgb, var(--text) 5%, transparent);
	}

	.heat {
		fill: var(--status-heating);
	}

	.cross {
		stroke: var(--text-muted);
		stroke-width: 1;
	}

	.mark {
		fill: var(--text);
		stroke: var(--surface);
		stroke-width: 2;
	}

	.caption {
		min-height: 1.25rem;
		font-size: var(--text-small-size);
		color: var(--text-muted);
	}
</style>
