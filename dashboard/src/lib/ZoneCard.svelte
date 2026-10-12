<script lang="ts">
	import { resolve } from '$app/paths';
	import { data } from './data.svelte.ts';
	import { alertKeys, modeLine, targetText } from './describe.ts';
	import Dial from './Dial.svelte';
	import { fmt, t } from './i18n.svelte.ts';
	import Icon from './Icon.svelte';
	import { HEATING_RANGE, HOT_WATER_RANGE } from './schedule.ts';
	import StatusBadge from './StatusBadge.svelte';
	import { targetIsTemp, zoneName, type ZoneModel } from './zones.ts';

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

	<div class="gauge">
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
	</div>

	<div class="info">
		{#if !hotWater}
			<dl class="meta">
				<div>
					<dt>{t('card.target')}</dt>
					<dd class:mono={targetIsTemp(model.zone.target)}>{targetText(model)}</dd>
				</div>
				<div>
					<dt>{t('card.humidity')}</dt>
					<dd class="mono">{fmt.percent(model.humidityPct)}</dd>
				</div>
			</dl>
		{:else}
			<!-- The dial shows the target of the hot water, so the fact below is the temperature the boiler reads. -->
			<dl class="meta">
				<div>
					<dt>{t('card.now')}</dt>
					<dd class="mono">{fmt.temp(data.boiler?.DHWC)}</dd>
				</div>
			</dl>
		{/if}
		<p class="mode small muted">{modeLine(model.zone.target)}</p>
	</div>

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
		border-color: var(--accent);
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

	.gauge {
		display: grid;
		justify-items: center;
		width: 100%;
	}

	.info {
		display: grid;
		gap: 0.6rem;
		justify-items: center;
	}

	.meta {
		display: flex;
		flex-wrap: wrap;
		justify-content: center;
		gap: 0.25rem 2rem;
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

	/* One column of cards on a phone: the dial sits beside the facts, so a screen shows
	   three zones instead of one and a half. */
	@media (max-width: 559px) {
		.card {
			grid-template-columns: 8rem minmax(0, 1fr);
			gap: 0.75rem 0.875rem;
			align-items: center;
			justify-items: stretch;
			padding: 1rem 1rem 1rem 1.1rem;
		}

		header,
		.alerts {
			grid-column: 1 / -1;
		}

		.alerts {
			justify-content: flex-start;
		}

		.info {
			justify-items: start;
			padding-right: 1rem;
		}

		.meta {
			justify-content: flex-start;
			gap: 0.25rem 1rem;
		}

		.mode {
			text-align: start;
			padding-inline: 0;
		}

		.go {
			right: 0.75rem;
			bottom: auto;
			top: 50%;
			translate: 0 -50%;
		}
	}
</style>
