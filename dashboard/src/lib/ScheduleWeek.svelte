<script lang="ts">
	import { fmt, t } from './i18n.svelte.ts';
	import { DAYS, formatTime, weekSegments, type Group, type Range } from './schedule.ts';

	let { groups, range }: { groups: Group[]; range: Range } = $props();

	const week = $derived(weekSegments(groups));

	/** The warmer the block, the more of the heating hue it carries. */
	function shade(setpointC: number): string {
		const fraction = Math.min(1, Math.max(0, (setpointC - range.min) / (range.max - range.min)));
		return `color-mix(in srgb, var(--status-heating) ${Math.round(18 + 47 * fraction)}%, var(--surface))`;
	}

	function summary(day: (typeof DAYS)[number]): string {
		const parts = week[day].map((s) => `${formatTime(s.from)} ${s.enabled ? fmt.temp(s.setpointC) : t('schedule.off')}`);
		return `${fmt.day(day, 'long')}: ${parts.join(', ')}`;
	}
</script>

<div class="week">
	{#each DAYS as day (day)}
		<div class="day">
			<span class="name">{fmt.day(day)}</span>
			<div class="bar" role="img" aria-label={summary(day)}>
				{#each week[day] as segment (segment.from)}
					<span
						class="segment"
						class:off={!segment.enabled}
						style:left="{(segment.from / 1440) * 100}%"
						style:width="{((segment.to - segment.from) / 1440) * 100}%"
						style:background={segment.enabled ? shade(segment.setpointC) : undefined}
					>
						{#if segment.to - segment.from >= 150}
							<span class="label mono">{segment.enabled ? fmt.number(segment.setpointC, 1) : t('schedule.off')}</span>
						{/if}
					</span>
				{/each}
			</div>
		</div>
	{/each}
	<div class="day axis" aria-hidden="true">
		<span class="name"></span>
		<div class="ticks mono">
			{#each [0, 6, 12, 18, 24] as hour (hour)}
				<span style:left="{(hour / 24) * 100}%">{hour}</span>
			{/each}
		</div>
	</div>
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
