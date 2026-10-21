<script lang="ts">
	import { fmt, t } from './i18n.svelte.ts';
	import { DAYS, formatTime, weekSegments, type Day, type Group, type Range } from './schedule.ts';

	interface Props {
		groups: Group[];
		range: Range;
		/** A small week with one row per program and no hour axis, for a list of schedules. */
		compact?: boolean;
		/** One day only, with no day name: the day of a room screen. */
		only?: Day;
		/** The minute of the day to mark on the row of only. */
		now?: number;
		/** The hour axis under the rows. A list of single days draws one axis of its own. */
		axis?: boolean;
	}

	let { groups, range, compact = false, only, now, axis = true }: Props = $props();

	const week = $derived(weekSegments(groups));
	// A compact week draws one row per program: the days that share it carry the same blocks.
	const rows = $derived(
		only
			? [{ key: only, day: only, label: fmt.day(only) }]
			: compact
				? groups.filter((g) => g.days.length > 0).map((g) => ({ key: String(g.key), day: g.days[0]!, label: dayRange(g.days) }))
				: DAYS.map((day) => ({ key: day, day, label: fmt.day(day) }))
	);

	/** mon, tue, wed reads "mon-wed"; days that do not follow each other are listed. */
	function dayRange(days: readonly (typeof DAYS)[number][]): string {
		const index = days.map((d) => DAYS.indexOf(d)).sort((a, b) => a - b);
		const consecutive = index.every((n, i) => i === 0 || n === index[i - 1]! + 1);
		if (consecutive && index.length > 2) return `${fmt.day(DAYS[index[0]!]!)}-${fmt.day(DAYS[index.at(-1)!]!)}`;
		return index.map((n) => fmt.day(DAYS[n]!)).join(', ');
	}

	// The ramp spans the temperatures of this schedule, at least a fifth of the range, so blocks a
	// few degrees apart differ at a glance. The editor and the compact week share it.
	const temps = $derived(groups.flatMap((g) => g.blocks.filter((b) => b.enabled).map((b) => b.setpointC)));
	const low = $derived(temps.length > 0 ? Math.min(...temps) : range.min);
	const span = $derived(Math.max((temps.length > 0 ? Math.max(...temps) : range.max) - low, (range.max - range.min) / 5));

	/** The warmer the block, the further along the warm ramp of the tokens. */
	function shade(setpointC: number): string {
		const fraction = Math.min(1, Math.max(0, (setpointC - low) / span));
		return `color-mix(in oklab, var(--schedule-high) ${(fraction * 100).toFixed(1)}%, var(--schedule-low))`;
	}

	function summary(day: (typeof DAYS)[number], label: string): string {
		const parts = week[day].map(
			(s) => `${formatTime(s.from)}-${formatTime(s.to)} ${s.enabled ? fmt.temp(s.setpointC) : t('schedule.off')}`
		);
		return `${label}: ${parts.join(', ')}`;
	}

	/** The label of the block under the line of now moves to the side away from the line. */
	function side(segment: { from: number; to: number }, day: Day): 'start' | 'end' | null {
		if (now === undefined || only !== day || now < segment.from || now >= segment.to) return null;
		return now - segment.from < (segment.to - segment.from) / 2 ? 'end' : 'start';
	}
</script>

<div class="week" class:compact class:single={only !== undefined}>
	{#each rows as { key, day, label } (key)}
		<div class="day">
			{#if only === undefined}<span class="name">{label}</span>{/if}
			<div class="bar" role="img" aria-label={summary(day, compact ? label : fmt.day(day, 'long'))} title={summary(day, compact ? label : fmt.day(day, 'long'))}>
				{#each week[day] as segment (segment.from)}
					<span
						class="segment {side(segment, day) ?? ''}"
						class:off={!segment.enabled}
						style:left="{(segment.from / 1440) * 100}%"
						style:width="{((segment.to - segment.from) / 1440) * 100}%"
						style:background={segment.enabled ? shade(segment.setpointC) : undefined}
					>
						<span class="label" class:mono={segment.enabled}>{segment.enabled ? fmt.degrees(segment.setpointC) : t('schedule.off')}</span>
					</span>
				{/each}
				{#if now !== undefined && only !== undefined}
					<i class="now" style:left="{(now / 1440) * 100}%"></i>
				{/if}
			</div>
		</div>
	{/each}
	{#if !compact && axis}
		<div class="day axis" aria-hidden="true">
			{#if only === undefined}<span class="name"></span>{/if}
			<div class="ticks mono">
				{#each [0, 6, 12, 18, 24] as hour (hour)}
					<span style:left="{(hour / 24) * 100}%">{String(hour).padStart(2, '0')}</span>
				{/each}
			</div>
		</div>
	{/if}
</div>

<style>
	.week {
		display: grid;
		gap: 0.4rem;
	}

	.day {
		display: grid;
		grid-template-columns: 2.75rem minmax(0, 1fr);
		align-items: center;
		gap: 0.6rem;
	}

	.single .day {
		grid-template-columns: minmax(0, 1fr);
	}

	.name {
		font-size: var(--text-small-size);
		font-weight: var(--weight-medium);
		color: var(--text-muted);
	}

	/* The ramp is light in both themes, so the labels on it are navy. */
	.bar {
		position: relative;
		height: 2rem;
		border-radius: var(--radius-xs);
		background: var(--schedule-low);
	}

	.single .bar {
		height: 2.25rem;
	}

	.segment {
		position: absolute;
		top: 0;
		bottom: 0;
		display: grid;
		place-items: center;
		/* White in both themes: the ramp is light in both, and the navy of now must stay the only dark line. */
		border-left: 2px solid var(--white);
		overflow: hidden;
		/* A block shows its label whenever the label fits in it. */
		container-type: inline-size;
	}

	.segment.start {
		justify-items: start;
		padding-left: 0.375rem;
	}

	.segment.end {
		justify-items: end;
		padding-right: 0.375rem;
	}

	.segment:first-child {
		border-left: 0;
		border-radius: var(--radius-xs) 0 0 var(--radius-xs);
	}

	.segment:last-of-type {
		border-radius: 0 var(--radius-xs) var(--radius-xs) 0;
	}

	.segment.off {
		background: var(--status-off-tint);
	}

	.label {
		font-size: var(--text-caption-size);
		color: var(--navy);
		font-weight: var(--weight-medium);
		white-space: nowrap;
	}

	@container (width < 1.75rem) {
		.label {
			visibility: hidden;
		}
	}

	.off .label {
		color: var(--status-off-text);
	}

	/* The time of now: a navy line across the bar with a pin head on the top edge. The white ring
	   keeps the head visible on the navy card of the dark theme. */
	.now {
		position: absolute;
		top: 0;
		bottom: 0;
		width: 2px;
		margin-left: -1px;
		background: var(--navy);
		pointer-events: none;
	}

	.now::before {
		content: '';
		position: absolute;
		top: -6px;
		left: 50%;
		width: 10px;
		height: 10px;
		margin-left: -5px;
		border-radius: 50%;
		box-shadow: 0 0 0 2px var(--white);
		background: var(--navy);
	}

	.compact {
		gap: 0.2rem;
	}

	.compact .day {
		grid-template-columns: minmax(3.5rem, max-content) minmax(0, 1fr);
		gap: 0.6rem;
	}

	.compact .name {
		font-size: var(--text-caption-size);
	}

	.compact .bar {
		height: 1.25rem;
		border-radius: 5px;
	}

	.compact .label {
		font-size: 0.7rem;
	}

	.ticks {
		position: relative;
		height: 1.1rem;
		font-size: var(--text-caption-size);
		color: var(--text-muted);
	}

	.ticks span {
		position: absolute;
		transform: translateX(-50%);
	}

	.ticks span:first-child {
		transform: none;
	}

	.ticks span:last-child {
		transform: translateX(-100%);
	}
</style>
