<script lang="ts">
	import { resolve } from '$app/paths';
	import { alertKeys, modeLine, targetText } from './describe.ts';
	import Dial from './Dial.svelte';
	import { fmt, t } from './i18n.svelte.ts';
	import Icon from './Icon.svelte';
	import { HEATING_RANGE, HOT_WATER_RANGE } from './schedule.ts';
	import StatusBadge from './StatusBadge.svelte';
	import { zoneName, type ZoneModel } from './zones.ts';

	let { model }: { model: ZoneModel } = $props();

	const hotWater = $derived(model.zone.kind === 'hotwater');
	const range = $derived(hotWater ? HOT_WATER_RANGE : HEATING_RANGE);
	const href = $derived(
		hotWater ? resolve('/hot-water') : resolve('/zones/[n]', { n: String(model.zone.number) })
	);
</script>

<a class="card tone-{model.status.tone}" {href}>
	<header>
		<h2>{zoneName(model.zone, t('zone.hotWaterName'))}</h2>
		<StatusBadge state={model.status.state} tone={model.status.tone} />
	</header>

	<Dial
		current={model.tempC}
		center={hotWater ? model.zone.target.targetC : model.tempC}
		target={model.zone.target.on ? model.zone.target.targetC : null}
		min={range.min}
		max={range.max}
		step={range.step}
		tone={model.status.tone}
		caption={hotWater ? t('card.target') : t('card.now')}
		size="12.5rem"
	/>

	<dl class="meta">
		{#if !hotWater}
			<div>
				<dt>{t('card.target')}</dt>
				<dd class="text">{targetText(model)}</dd>
			</div>
			<div>
				<dt>{t('card.humidity')}</dt>
				<dd class="mono">{fmt.percent(model.humidityPct)}</dd>
			</div>
		{:else if model.zone.target.targetC === null}
			<div>
				<dt>{t('card.target')}</dt>
				<dd class="text">{targetText(model)}</dd>
			</div>
		{/if}
	</dl>
	<p class="mode small muted">{modeLine(model.zone.target)}</p>

	{#if model.status.alerts.length > 0}
		<ul class="alerts">
			{#each model.status.alerts as alert (alert)}
				<li class="badge tone-warning">{t(alertKeys[alert])}</li>
			{/each}
		</ul>
	{/if}
	<span class="go" aria-hidden="true"><Icon name="chevron" size={18} /></span>
</a>

<style>
	.card {
		position: relative;
		display: grid;
		gap: 0.9rem;
		justify-items: center;
		padding: 1.25rem 1.25rem 1.1rem;
		background: var(--surface);
		border: 1px solid var(--border);
		border-radius: var(--radius-card);
		color: var(--text);
		text-decoration: none;
	}

	.card:hover {
		border-color: var(--dot);
		color: var(--text);
	}

	/* Heating is the one warm colour, so a calling zone gets a top edge in it. */
	.card.tone-heating {
		box-shadow: inset 0 4px 0 var(--status-heating);
	}

	header {
		display: flex;
		width: 100%;
		flex-wrap: wrap;
		align-items: center;
		justify-content: space-between;
		gap: 0.5rem;
	}

	.meta {
		display: flex;
		gap: 2rem;
		margin: 0;
	}

	dt {
		font-size: var(--text-small-size);
		color: var(--text-muted);
	}

	dd {
		margin: 0;
		font-weight: var(--weight-medium);
	}

	.mode {
		text-align: center;
		padding-inline: 1.25rem;
	}

	.alerts {
		display: flex;
		flex-wrap: wrap;
		justify-content: center;
		gap: 0.4rem;
		list-style: none;
		margin: 0;
		padding: 0;
	}

	.go {
		position: absolute;
		right: 0.9rem;
		bottom: 0.9rem;
		color: var(--text-muted);
	}
</style>
