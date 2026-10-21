<script lang="ts" module>
	export interface BarItem {
		key: string;
		/** The axis label, empty for none. The chart skips labels that would collide. */
		label: string;
		/** Null where there is no data: the slot stays empty. */
		value: number | null;
	}
</script>

<script lang="ts">
	import { axisTicks } from './history.ts';

	interface Props {
		bars: BarItem[];
		label: string;
		/** An axis value as text. */
		format: (v: number) => string;
		/** The readout of one bar. */
		describe: (index: number) => string;
		/** The readout with no bar under the pointer. */
		hint: string;
	}

	let { bars, label, format, describe, hint }: Props = $props();

	let width = $state(640);
	const W = $derived(Math.max(280, width));
	const LEFT = 40;
	const RIGHT = 6;
	const TOP = 10;
	const PLOT_H = 170;
	const H = PLOT_H + TOP + 22;
	const slot = $derived((W - LEFT - RIGHT) / Math.max(1, bars.length));
	const barW = $derived(Math.max(2, Math.min(24, slot - 2)));
	const ticks = $derived(axisTicks(Math.max(0, ...bars.map((b) => b.value ?? 0))));
	const top = $derived(ticks.at(-1) ?? 1);
	const y = (v: number) => TOP + (1 - v / top) * PLOT_H;
	const cx = (i: number) => LEFT + slot * (i + 0.5);
	// A label needs about 28 px: from the left, a label closer than that to the last one shown is skipped.
	const shown = $derived.by(() => {
		const out = new Set<number>();
		let last = -Infinity;
		bars.forEach((b, i) => {
			if (b.label === '' || cx(i) - last < 28) return;
			out.add(i);
			last = cx(i);
		});
		return out;
	});

	/** A bar with a 4 px rounded top and a square base. */
	function bar(i: number, v: number): string {
		const x0 = cx(i) - barW / 2;
		const y0 = y(v);
		const base = y(0);
		const r = Math.min(4, barW / 2, base - y0);
		return `M${x0},${base}V${y0 + r}Q${x0},${y0} ${x0 + r},${y0}H${x0 + barW - r}Q${x0 + barW},${y0} ${x0 + barW},${y0 + r}V${base}Z`;
	}

	let hover = $state<number | null>(null);

	function move(event: PointerEvent) {
		const box = (event.currentTarget as SVGSVGElement).getBoundingClientRect();
		const px = ((event.clientX - box.left) / box.width) * W;
		const i = Math.floor((px - LEFT) / slot);
		hover = i >= 0 && i < bars.length ? i : null;
	}

	function key(event: KeyboardEvent) {
		if (bars.length === 0) return;
		const last = bars.length - 1;
		if (event.key === 'ArrowLeft' || event.key === 'ArrowRight') {
			const delta = event.key === 'ArrowLeft' ? -1 : 1;
			hover = Math.min(last, Math.max(0, (hover ?? (delta < 0 ? last + 1 : -1)) + delta));
		} else if (event.key === 'Home') hover = 0;
		else if (event.key === 'End') hover = last;
		else if (event.key === 'Escape') hover = null;
		else return;
		event.preventDefault();
	}
</script>

<div class="chart" bind:clientWidth={width}>
	<figure>
		<!-- A slider over the bars: the arrow keys move the readout, which is also its value text. -->
		<svg
			viewBox="0 0 {W} {H}"
			width={W}
			height={H}
			role="slider"
			aria-label={label}
			aria-valuemin={0}
			aria-valuemax={Math.max(0, bars.length - 1)}
			aria-valuenow={hover ?? 0}
			aria-valuetext={hover === null ? hint : describe(hover)}
			tabindex="0"
			onpointermove={move}
			onpointerleave={() => (hover = null)}
			onkeydown={key}
			onblur={() => (hover = null)}
		>
			{#each ticks as v (v)}
				<line class="grid" x1={LEFT} x2={W - RIGHT} y1={y(v)} y2={y(v)} />
				<text class="tick" x={LEFT - 6} y={y(v) + 4} text-anchor="end">{format(v)}</text>
			{/each}
			{#if hover !== null}
				<rect class="slot" x={LEFT + slot * hover} y={TOP} width={slot} height={PLOT_H} />
			{/if}
			{#each bars as b, i (b.key)}
				{#if b.value !== null && b.value > 0}
					<path class="bar" class:on={hover === i} d={bar(i, b.value)} />
				{/if}
				{#if shown.has(i)}
					<text class="tick" x={cx(i)} y={H - 6} text-anchor="middle">{b.label}</text>
				{/if}
			{/each}
		</svg>
		<figcaption class="caption" aria-live="polite">{hover === null ? hint : describe(hover)}</figcaption>
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

	.grid {
		stroke: var(--border);
		stroke-width: 1;
	}

	.tick {
		fill: var(--text-muted);
		font-family: var(--font-mono);
		font-size: 11px;
	}

	.slot {
		fill: color-mix(in srgb, var(--text) 6%, transparent);
	}

	.bar {
		fill: var(--accent);
	}

	.bar.on {
		fill: var(--text);
	}

	.caption {
		min-height: 1.25rem;
		font-size: var(--text-small-size);
		color: var(--text-muted);
	}
</style>
