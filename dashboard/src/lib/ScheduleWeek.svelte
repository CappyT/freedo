<script lang="ts">
	import { fmt, t } from './i18n.svelte.ts';
	import { DAYS, formatTime, weekSegments, type Group, type Range } from './schedule.ts';

	// compact draws a small week with one row per program and no hour axis, for a list of schedules.
	let { groups, range, compact = false }: { groups: Group[]; range: Range; compact?: boolean } = $props();

	const week = $derived(weekSegments(groups));
	// A compact week draws one row per program: the days that share it carry the same blocks.
	const rows = $derived(
		compact
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

	/** The warmer the block, the more of the heating hue it carries. The theme sets the ends of the ramp. */
	function shade(setpointC: number): string {
		const fraction = Math.min(1, Math.max(0, (setpointC - low) / span));
		return `color-mix(in srgb, var(--status-heating) calc(var(--ramp-low) + var(--ramp-span) * ${fraction.toFixed(3)}), var(--surface))`;
	}

	function summary(day: (typeof DAYS)[number], label: string): string {
		const parts = week[day].map(
			(s) => `${formatTime(s.from)}-${formatTime(s.to)} ${s.enabled ? fmt.temp(s.setpointC) : t('schedule.off')}`
		);
		return `${label}: ${parts.join(', ')}`;
	}

	// The shortest block that fits its temperature label.
	const labelMin = $derived(compact ? 240 : 150);
</script>

<div class="week" class:compact>
	{#each rows as { key, day, label } (key)}
		<div class="day">
			<span class="name">{label}</span>
			<div class="bar" role="img" aria-label={summary(day, compact ? label : fmt.day(day, 'long'))} title={summary(day, compact ? label : fmt.day(day, 'long'))}>
				{#each week[day] as segment (segment.from)}
					<span
						class="segment"
						class:off={!segment.enabled}
						style:left="{(segment.from / 1440) * 100}%"
						style:width="{((segment.to - segment.from) / 1440) * 100}%"
						style:background={segment.enabled ? shade(segment.setpointC) : undefined}
					>
						{#if segment.to - segment.from >= labelMin}
							<span class="label" class:mono={segment.enabled}>{segment.enabled ? fmt.number(segment.setpointC, 1) : t('schedule.off')}</span>
						{/if}
					</span>
				{/each}
			</div>
		</div>
	{/each}
	{#if !compact}
		<div class="day axis" aria-hidden="true">
			<span class="name"></span>
			<div class="ticks mono">
				{#each [0, 6, 12, 18, 24] as hour (hour)}
					<span style:left="{(hour / 24) * 100}%">{hour}</span>
				{/each}
			</div>
		</div>
	{/if}
</div>

<style>
	/* Above 56 percent of the hue the label on the dark theme falls under 4.5:1. */
	.week {
		--ramp-low: 14%;
		--ramp-span: 58%;
		display: grid;
		gap: 0.4rem;
	}

	@media (prefers-color-scheme: dark) {
		:global(:root:not([data-theme='light'])) .week {
			--ramp-low: 20%;
			--ramp-span: 36%;
		}
	}

	:global(:root[data-theme='dark']) .week {
		--ramp-low: 20%;
		--ramp-span: 36%;
	}

	.day {
		display: grid;
		grid-template-columns: 2.75rem minmax(0, 1fr);
		align-items: center;
		gap: 0.6rem;
	}

	.name {
		font-size: var(--text-small-size);
		font-weight: var(--weight-medium);
		color: var(--text-muted);
	}

	.bar {
		position: relative;
		height: 2rem;
		border-radius: var(--radius-xs);
		border: 1px solid var(--border);
		overflow: hidden;
		background: var(--panel);
	}

	.segment {
		position: absolute;
		top: 0;
		bottom: 0;
		display: grid;
		place-items: center;
		border-left: 1px solid var(--surface);
	}

	.segment.off {
		background: repeating-linear-gradient(
			135deg,
			var(--status-off-tint) 0 6px,
			var(--border-strong) 6px 8px
		);
	}

	.label {
		font-size: var(--text-caption-size);
		color: var(--text);
		font-weight: var(--weight-medium);
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
