<script lang="ts">
	import { START, SPAN, arcPath, clamp, pointToValue, polar, snap, valueToAngle } from './dial.ts';
	import { fmt } from './i18n.svelte.ts';
	import type { Tone } from './zones.ts';

	interface Props {
		/** The measured temperature, shown in the middle and as a notch on the arc. */
		current: number | null;
		/** The wanted temperature: the knob. Null while the zone follows no value. */
		target: number | null;
		min: number;
		max: number;
		step: number;
		tone?: Tone | null;
		/** A dial that can be set. A read-only dial is decoration for the text next to it. */
		interactive?: boolean;
		label?: string;
		caption?: string;
		/** The number in the middle. The default is the measured temperature. */
		center?: number | null;
		/** Largest width, as a CSS length. */
		size?: string;
		onchange?: (value: number) => void;
	}

	let {
		current,
		target,
		min,
		max,
		step,
		tone = null,
		interactive = false,
		label = '',
		caption = '',
		center = undefined,
		size = '18rem',
		onchange
	}: Props = $props();
	const shown = $derived(center === undefined ? current : center);

	const C = 100;
	const R = 78;
	const track = arcPath(C, C, R, START, START + SPAN);
	const angle = $derived(target === null ? START : valueToAngle(target, min, max));
	const progress = $derived(target === null ? '' : arcPath(C, C, R, START, Math.max(angle, START + 0.1)));
	const knob = $derived(polar(C, C, R, angle));
	const notch = $derived.by(() => {
		if (current === null || current < min || current > max) return null;
		const a = valueToAngle(current, min, max);
		return { from: polar(C, C, R - 15, a), to: polar(C, C, R + 15, a) };
	});

	let svg: SVGSVGElement | undefined = $state();
	let dragging = false;

	function move(event: PointerEvent) {
		if (!svg) return;
		const box = svg.getBoundingClientRect();
		const scale = 200 / box.width;
		const point = { x: (event.clientX - box.left) * scale, y: (event.clientY - box.top) * scale };
		onchange?.(pointToValue(point, { x: C, y: C }, min, max, step));
	}

	function down(event: PointerEvent) {
		if (!interactive) return;
		dragging = true;
		(event.currentTarget as Element).setPointerCapture(event.pointerId);
		move(event);
	}

	function drag(event: PointerEvent) {
		if (dragging) move(event);
	}

	function up() {
		dragging = false;
	}

	function key(event: KeyboardEvent) {
		if (!interactive) return;
		const from = target ?? current ?? min;
		const big = step * 4;
		const next: Record<string, number> = {
			ArrowUp: from + step,
			ArrowRight: from + step,
			ArrowDown: from - step,
			ArrowLeft: from - step,
			PageUp: from + big,
			PageDown: from - big,
			Home: min,
			End: max
		};
		if (!(event.key in next)) return;
		event.preventDefault();
		onchange?.(snap(clamp(next[event.key]!, min, max), min, max, step));
	}
</script>

<div class="dial tone-{tone ?? 'none'}" style:max-width={size}>
	<!-- svelte-ignore a11y_no_noninteractive_tabindex -->
	<svg
		bind:this={svg}
		viewBox="0 0 200 200"
		role={interactive ? 'slider' : undefined}
		tabindex={interactive ? 0 : undefined}
		aria-hidden={interactive ? undefined : 'true'}
		aria-label={interactive ? label : undefined}
		aria-valuemin={interactive ? min : undefined}
		aria-valuemax={interactive ? max : undefined}
		aria-valuenow={interactive ? (target ?? undefined) : undefined}
		aria-valuetext={interactive && target !== null ? fmt.temp(target) : undefined}
		onkeydown={key}
	>
		<path class="track" d={track} />
		{#if progress}<path class="progress" d={progress} />{/if}
		{#if notch}<line class="notch" x1={notch.from.x} y1={notch.from.y} x2={notch.to.x} y2={notch.to.y} />{/if}
		{#if interactive}
			<path class="hit" role="presentation" d={track} onpointerdown={down} onpointermove={drag} onpointerup={up} onpointercancel={up} />
		{/if}
		{#if target !== null}
			<circle
				class="knob"
				role="presentation"
				class:grab={interactive}
				cx={knob.x}
				cy={knob.y}
				r="11"
				onpointerdown={down}
				onpointermove={drag}
				onpointerup={up}
				onpointercancel={up}
			/>
		{/if}
	</svg>
	<div class="center">
		<span class="now">{shown === null ? '-' : fmt.number(shown, 1)}{#if shown !== null}<small>°</small>{/if}</span>
		{#if caption}<span class="caption">{caption}</span>{/if}
	</div>
</div>

<style>
	.dial {
		--arc: var(--accent);
		position: relative;
		width: 100%;
		aspect-ratio: 1;
	}

	.tone-heating {
		--arc: var(--status-heating);
	}

	.tone-away {
		--arc: var(--status-away);
	}

	.tone-off,
	.tone-offline {
		--arc: var(--status-off);
	}

	.tone-warning {
		--arc: var(--status-warning);
	}

	.tone-fault {
		--arc: var(--status-fault);
	}

	.tone-idle {
		--arc: var(--status-idle);
	}

	svg {
		width: 100%;
		height: 100%;
		display: block;
		overflow: visible;
	}

	svg[role='slider'] {
		cursor: pointer;
	}

	path {
		fill: none;
		stroke-linecap: round;
	}

	.track {
		stroke: var(--border-strong);
		stroke-width: 14;
	}

	.progress {
		stroke: var(--arc);
		stroke-width: 14;
	}

	.hit {
		stroke: transparent;
		stroke-width: 40;
		touch-action: none;
	}

	.notch {
		stroke: var(--text);
		stroke-width: 3;
		stroke-linecap: round;
	}

	.knob {
		fill: var(--navy);
		stroke: var(--white);
		stroke-width: 4;
		touch-action: none;
	}

	.knob.grab {
		cursor: grab;
	}

	svg:focus-visible {
		outline: none;
	}

	svg:focus-visible .knob {
		stroke: var(--accent);
		stroke-width: 6;
	}

	.center {
		position: absolute;
		inset: 0;
		display: grid;
		place-content: center;
		justify-items: center;
		gap: 0.1rem;
		pointer-events: none;
		padding-bottom: 0.5rem;
	}

	.now {
		font-family: var(--font-display);
		font-weight: var(--weight-bold);
		font-size: min(3.75rem, 17cqi);
		letter-spacing: -0.03em;
		line-height: 1;
	}

	.now small {
		font-size: 0.5em;
		font-weight: var(--weight-semibold);
		color: var(--text-muted);
		margin-left: 0.05em;
		vertical-align: 0.5em;
	}

	.caption {
		font-size: var(--text-small-size);
		color: var(--text-muted);
	}

	.dial {
		container-type: inline-size;
	}
</style>
