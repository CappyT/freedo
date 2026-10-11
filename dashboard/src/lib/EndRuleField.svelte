<script lang="ts">
	import { END_RULES, MAX_TIMER_SEC, timerSeconds, validTimer } from './endrule.ts';
	import { t } from './i18n.svelte.ts';
	import type { EndRule } from './types.ts';

	let {
		rule = $bindable(),
		hours = $bindable(),
		minutes = $bindable(),
		name
	}: { rule: EndRule; hours: number; minutes: number; name: string } = $props();

	const invalid = $derived(rule === 'timer' && !validTimer(timerSeconds(hours, minutes)));
</script>

<fieldset class="ends">
	<legend>{t('ends.legend')}</legend>
	{#each END_RULES as value (value)}
		<label class="option">
			<input type="radio" {name} {value} bind:group={rule} />
			<span class="text">
				<span class="title">{t(`ends.${value}`)}</span>
				<span class="hint">{t(`ends.${value}.hint`)}</span>
			</span>
			<code class="mono rule">{value}</code>
		</label>
	{/each}
	{#if rule === 'timer'}
		<div class="duration row">
			<label class="field">
				<span>{t('ends.hours')}</span>
				<input class="input mono" type="number" min="0" max={MAX_TIMER_SEC / 3600} step="1" bind:value={hours} aria-invalid={invalid} />
			</label>
			<label class="field">
				<span>{t('ends.minutes')}</span>
				<input class="input mono" type="number" min="0" max="59" step="5" bind:value={minutes} aria-invalid={invalid} />
			</label>
			{#if invalid}<p class="note" role="alert">{t('ends.timerRange')}</p>{/if}
		</div>
	{/if}
</fieldset>

<style>
	.ends {
		display: grid;
		gap: 0.25rem;
		margin: 0;
		padding: 0;
		border: 0;
		min-width: 0;
	}

	legend {
		padding: 0;
		margin-bottom: 0.35rem;
		font-weight: var(--weight-medium);
		font-size: var(--text-small-size);
		color: var(--text-muted);
	}

	.option {
		display: grid;
		grid-template-columns: auto 1fr auto;
		align-items: center;
		gap: 0.75rem;
		min-height: 2.75rem;
		padding: 0.4rem 0.75rem;
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		cursor: pointer;
	}

	.option:has(input:checked) {
		border-color: var(--accent);
		background: var(--panel);
	}

	.option input {
		width: 1.15rem;
		height: 1.15rem;
		accent-color: var(--action);
	}

	.text {
		display: grid;
	}

	.title {
		font-weight: var(--weight-medium);
	}

	.hint {
		font-size: var(--text-small-size);
		color: var(--text-muted);
	}

	.rule {
		font-size: var(--text-caption-size);
		color: var(--text-muted);
	}

	.duration {
		align-items: end;
		padding-top: 0.5rem;
	}

	.duration .field {
		width: 7rem;
	}

	@media (max-width: 480px) {
		.rule {
			display: none;
		}
	}
</style>
