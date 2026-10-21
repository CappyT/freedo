<script lang="ts">
	import { END_RULES, MAX_TIMER_SEC, timerSeconds, validTimer } from './endrule.ts';
	import { t } from './i18n.svelte.ts';
	import type { EndRule } from './types.ts';

	let {
		rule = $bindable(),
		hours = $bindable(),
		minutes = $bindable(),
		name,
		noProgram = false,
		hideLegend = false
	}: { rule: EndRule; hours: number; minutes: number; name: string; noProgram?: boolean; hideLegend?: boolean } = $props();

	const invalid = $derived(rule === 'timer' && !validTimer(timerSeconds(hours, minutes)));
</script>

<fieldset class="ends">
	<legend class:sr-only={hideLegend}>{t('ends.legend')}</legend>
	{#if noProgram}
		<p class="note program-hint" data-testid="no-program">{t('ends.noProgram')}</p>
	{/if}
	{#each END_RULES as value (value)}
		<!-- The title names the rule of the API and of the Home Assistant service set_overlay. -->
		<label class="option" title={value}>
			<input type="radio" {name} {value} bind:group={rule} />
			<span class="text">
				<span class="title">{t(`ends.${value}`)}</span>
				<span class="hint">{t(`ends.${value}.hint`)}</span>
			</span>
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
		gap: 0.125rem;
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

	.program-hint {
		margin: 0 0 0.35rem;
	}

	/* Rows of one list, not cards: the chosen one takes a light tint of the accent. */
	.option {
		display: grid;
		grid-template-columns: auto 1fr;
		align-items: start;
		gap: 0.75rem;
		min-height: 2.75rem;
		padding: 0.6rem 0.625rem;
		border-radius: var(--radius-md);
		cursor: pointer;
	}

	.option:hover {
		background: color-mix(in srgb, var(--panel) 70%, transparent);
	}

	.option:has(input:checked) {
		background: color-mix(in srgb, var(--accent) 10%, transparent);
	}

	.option input {
		width: 1.15rem;
		height: 1.15rem;
		margin: 0.15rem 0 0;
		accent-color: var(--action);
	}

	.text {
		display: grid;
		gap: 0.1rem;
	}

	.title {
		font-weight: var(--weight-semibold);
		font-size: 0.9375rem;
		line-height: 1.3;
	}

	.hint {
		font-size: 0.8125rem;
		line-height: 1.35;
		color: var(--text-muted);
	}

	.duration {
		align-items: end;
		padding: 0.5rem 0 0 2.375rem;
	}

	.duration .field {
		width: 7rem;
	}
</style>
