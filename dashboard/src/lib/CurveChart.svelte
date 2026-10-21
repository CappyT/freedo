<script lang="ts">
	import { previewLine } from './boileroptions.ts';
	import { fmt, t } from './i18n.svelte.ts';

	interface Props {
		/** The flow limit at an outdoor temperature. */
		at: (outdoorC: number) => number;
		/** The outdoor temperature now, null while none is fresh. */
		outdoorC: number | null;
		label: string;
	}

	let { at, outdoorC, label }: Props = $props();

	const X = { min: -15, max: 25 };
	const W = 320;
	const PLOT_H = 150;
	const LEFT = 34;
	const RIGHT = 8;
	const BOTTOM = 22;
	const TOP = 10;
	const line = $derived(previewLine(at, X.min, X.max));
	// The scale starts at the 30 C floor of the daemon and ends on a multiple of 5 above the curve.
	const yMax = $derived(Math.max(50, Math.ceil((Math.max(...line.map((p) => p.flowC)) + 2) / 5) * 5));
	const Y = $derived({ min: 30, max: yMax });
	const x = (c: number) => LEFT + ((c - X.min) / (X.max - X.min)) * (W - LEFT - RIGHT);
	const y = (c: number) => TOP + (1 - (c - Y.min) / (Y.max - Y.min)) * (PLOT_H - TOP);
	const path = $derived(line.map((p, i) => `${i === 0 ? 'M' : 'L'}${x(p.outdoorC).toFixed(1)},${y(p.flowC).toFixed(1)}`).join(''));
	const yTicks = $derived(Array.from({ length: Math.floor((Y.max - Y.min) / 10) + 1 }, (_, i) => Y.min + i * 10));
	const xTicks = [-15, -5, 5, 15, 25];
	const table = [-10, -5, 0, 5, 10, 15, 20];
	const now = $derived(outdoorC === null ? null : { outdoorC, flowC: at(Math.min(X.max, Math.max(X.min, outdoorC))) });

	// The hover follows the pointer to the nearest whole degree, so the tooltip reads one value of the curve.
	let hover = $state<number | null>(null);
	function move(event: PointerEvent) {
		const svg = event.currentTarget as SVGSVGElement;
		const box = svg.getBoundingClientRect();
		const px = ((event.clientX - box.left) / box.width) * W;
		const c = Math.round(X.min + ((px - LEFT) / (W - LEFT - RIGHT)) * (X.max - X.min));
		hover = Math.min(X.max, Math.max(X.min, c));
	}
</script>

<div class="chart">
	<figure>
		<svg viewBox="0 0 {W} {PLOT_H + BOTTOM}" role="img" aria-label={label} onpointermove={move} onpointerleave={() => (hover = null)}>
			{#each yTicks as v (v)}
				<line class="grid" x1={LEFT} x2={W - RIGHT} y1={y(v)} y2={y(v)} />
				<text class="tick" x={LEFT - 6} y={y(v) + 4} text-anchor="end">{v}°</text>
			{/each}
			{#each xTicks as c (c)}
				<text class="tick" x={x(c)} y={PLOT_H + 16} text-anchor="middle">{c}°</text>
			{/each}
			<path class="curve" d={path} />
			{#if hover !== null}
				<line class="cross" x1={x(hover)} x2={x(hover)} y1={TOP} y2={PLOT_H} />
				<circle class="mark" cx={x(hover)} cy={y(at(hover))} r="4" />
			{/if}
			{#if now}
				<circle class="now" cx={x(Math.min(X.max, Math.max(X.min, now.outdoorC)))} cy={y(now.flowC)} r="5" />
			{/if}
		</svg>
		<figcaption class="caption">
			{#if hover !== null}
				<span class="num">{t('curve.at', { outdoor: fmt.degrees(hover), flow: fmt.degrees(at(hover)) })}</span>
			{:else if now}
				<span>{t('curve.now', { outdoor: fmt.degrees(now.outdoorC), flow: fmt.degrees(now.flowC) })}</span>
			{:else}
				<span>{t('curve.axes')}</span>
			{/if}
		</figcaption>
	</figure>
	<dl class="table">
		<div class="head"><dt>{t('curve.outdoor')}</dt><dd>{t('curve.flow')}</dd></div>
		{#each table as c (c)}
			<div><dt class="num">{fmt.degrees(c)}</dt><dd class="num">{fmt.degrees(at(c))}</dd></div>
		{/each}
	</dl>
</div>

<style>
	.chart,
	figure {
		display: grid;
		gap: 0.375rem;
		margin: 0;
	}

	svg {
		width: 100%;
		max-width: 28rem;
		height: auto;
		touch-action: pan-y;
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

	.curve {
		fill: none;
		stroke: var(--text);
		stroke-width: 2;
		stroke-linejoin: round;
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

	.now {
		fill: var(--accent);
		stroke: var(--surface);
		stroke-width: 2;
	}

	.caption {
		min-height: 1.25rem;
		font-size: var(--text-small-size);
		color: var(--text-muted);
	}

	.table {
		display: grid;
		grid-template-columns: auto repeat(7, minmax(0, 1fr));
		margin: 0;
		border: 1px solid var(--border);
		border-radius: var(--radius-xs);
		overflow: hidden;
	}

	.table div {
		display: grid;
		justify-items: center;
		padding: 0.3rem 0.1rem;
	}

	.table div + div {
		border-left: 1px solid var(--border);
	}

	.table dt {
		color: var(--text-muted);
		font-size: 0.75rem;
	}

	.table dd {
		margin: 0;
		font-size: 0.8125rem;
	}

	.table .head {
		justify-items: start;
		padding-inline: 0.5rem;
		font-weight: var(--weight-semibold);
	}
</style>
