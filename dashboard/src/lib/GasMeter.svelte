<script lang="ts">
	import { resolve } from '$app/paths';
	import { act } from './actions.ts';
	import { parseNumber } from './boileroptions.ts';
	import ConfirmDialog from './ConfirmDialog.svelte';
	import { fmt, t } from './i18n.svelte.ts';
	import Icon from './Icon.svelte';
	import { estimateBias, fromLocalInput, localInput, meterRows, readingIssue } from './meter.ts';
	import { api } from './session.ts';
	import type { GasEstimate } from './types.ts';

	interface Props {
		gas: GasEstimate;
	}

	let { gas }: Props = $props();
	const meter = $derived(gas.meter);
	const unit = $derived(gas.unit === 'm3' ? 'm³' : gas.unit);
	const rows = $derived(meter ? meterRows(meter.entries) : []);
	// Without a fuel flow the daemon refuses a reading: the page sends the person to the setup.
	const counting = $derived(gas.reason !== 'noGasFlow');

	let open = $state(false);
	let all = $state(false);
	let busy = $state(false);
	let readingText = $state('');
	let atText = $state('');
	let replaced = $state(false);
	let undoDialog: ConfirmDialog | undefined = $state();

	const reading = $derived(parseNumber(readingText));
	const at = $derived(fromLocalInput(atText));
	const issue = $derived(readingText.trim() === '' ? null : readingIssue(reading, at, meter?.lastReading ?? null, replaced));
	const shown = $derived(all ? rows : rows.slice(0, 5));
	const amount = (v: number, digits = 3) => `${fmt.number(v, digits)} ${unit}`;
	const signed = (v: number) => (v >= 0 ? '+' : '') + amount(v);

	function start() {
		open = !open;
		if (open) {
			readingText = '';
			atText = localInput(new Date());
			replaced = false;
		}
	}

	async function save(event: SubmitEvent) {
		event.preventDefault();
		if (reading === null || reading === undefined || issue !== null) return;
		busy = true;
		const body = { reading, ...(at === null ? {} : { at }), ...(replaced ? { replaced } : {}) };
		const done = await act(() => api.gasReading(body), t('meter.saved'));
		busy = false;
		if (done !== undefined) open = false;
	}

	async function undo() {
		busy = true;
		await act(() => api.undoGasReading(), t('meter.undone'));
		busy = false;
	}
</script>

{#if !counting && !meter}
	<p class="list none" data-testid="meter-off">
		<span>{t('meter.needsFlow')}</span>
		<a class="section-link" href={resolve('/boiler/setup')}>{t('meter.toSetup')}<Icon name="chevron" size={16} /></a>
	</p>
{:else}
	<ul class="list" data-testid="gas-meter">
		{#if meter}
			<li class="list-row">
				<span class="list-main">
					<span class="list-label">{t('meter.value')}</span>
					<span class="list-sub">{t('meter.last', { reading: amount(meter.lastReading), when: fmt.dateTime(meter.lastReadingAt) })}</span>
				</span>
				<span class="list-value num" data-testid="meter-value">{amount(meter.value)}</span>
			</li>
			<li class="list-row">
				<span class="list-main">
					<span class="list-label">{t('meter.factor')}</span>
					<span class="list-sub">
						{#if meter.factorReadings === 0}
							{t('meter.factor.none', { unit })}
						{:else if estimateBias(meter.factor) >= 0}
							{t('meter.factor.high', { pct: fmt.number(estimateBias(meter.factor), 1) })}
						{:else}
							{t('meter.factor.low', { pct: fmt.number(-estimateBias(meter.factor), 1) })}
						{/if}
					</span>
				</span>
				<span class="list-value" class:num={meter.factorReadings > 0}>
					{meter.factorReadings > 0 ? fmt.number(meter.factor, 3) : t('meter.factor.notYet')}
				</span>
			</li>
		{/if}
		<li>
			<button class="list-row" aria-expanded={open} disabled={!counting} data-testid="meter-add" onclick={start}>
				<span class="disc"><Icon name="plus" size={18} /></span>
				<span class="list-main">
					<span class="list-label action">{t('meter.add')}</span>
					<span class="list-sub">{t('meter.add.sub')}</span>
				</span>
			</button>
		</li>
		{#if open}
			<li class="form">
				<form onsubmit={save}>
					<div class="row fill">
						<label class="field grow">
							<span>{t('meter.reading', { unit })}</span>
							<input class="input num" inputmode="decimal" autocomplete="off" bind:value={readingText} aria-invalid={issue === 'number' || issue === 'below'} data-testid="meter-reading" />
						</label>
						<label class="field grow">
							<span>{t('meter.at')}</span>
							<input class="input num" type="datetime-local" bind:value={atText} max={localInput(new Date())} aria-invalid={issue === 'future'} />
						</label>
					</div>
					<label class="check"><input type="checkbox" bind:checked={replaced} />{t('meter.replaced')}</label>
					{#if issue}
						<p class="note issue" role="alert">{t(`meter.issue.${issue}`, { last: meter ? amount(meter.lastReading) : '' })}</p>
					{:else}
						<p class="note">{replaced ? t('meter.replaced.note') : t('meter.reading.note')}</p>
					{/if}
					<div class="row fill">
						<button class="btn primary" type="submit" disabled={busy || reading === null || issue !== null}><Icon name="check" size={18} />{t('meter.save')}</button>
						<button class="btn" type="button" onclick={() => (open = false)}>{t('action.cancel')}</button>
					</div>
				</form>
			</li>
		{/if}
	</ul>

	{#if rows.length > 0}
		<ul class="list" data-testid="meter-readings">
			{#each shown as row, i (row.at + row.reading)}
				<li class="list-row">
					<span class="list-main">
						<span class="list-label">{fmt.dateTime(row.at)}</span>
						{#if row.meterDelta !== null && row.estimateDelta !== null}
							<span class="list-sub">{t('meter.delta', { meter: signed(row.meterDelta), estimate: signed(row.estimateDelta) })}</span>
						{:else}
							<span class="list-sub">{t('meter.first')}</span>
						{/if}
					</span>
					<span class="list-value num">{amount(row.reading)}</span>
					{#if i === 0}
						<button class="btn icon undo" type="button" aria-label={t('meter.undo')} title={t('meter.undo')} disabled={busy} onclick={() => undoDialog?.show()} data-testid="meter-undo">
							<Icon name="trash" size={18} />
						</button>
					{/if}
				</li>
			{/each}
			{#if rows.length > shown.length || all}
				<li>
					<button class="list-row more" onclick={() => (all = !all)}>
						<span class="list-main list-label action">{all ? t('meter.fewer') : t('meter.all', { n: rows.length })}</span>
					</button>
				</li>
			{/if}
		</ul>
	{/if}
{/if}

<ConfirmDialog
	bind:this={undoDialog}
	title={t('meter.undo.title')}
	body={meter ? t('meter.undo.body', { reading: amount(meter.lastReading), when: fmt.dateTime(meter.lastReadingAt) }) : ''}
	confirm={t('meter.undo')}
	onconfirm={() => void undo()}
/>

<style>
	.none {
		display: grid;
		gap: 0.25rem;
		padding: 0.875rem;
		font-size: var(--text-small-size);
	}

	.none .section-link {
		margin-block: 0;
	}

	.form form {
		display: grid;
		gap: 0.625rem;
		padding: 0.875rem;
	}

	.grow {
		flex: 1 1 11rem;
	}

	.issue {
		color: var(--status-fault-text);
	}

	.undo {
		min-height: 2.75rem;
		border-color: transparent;
		color: var(--text-muted);
	}

	.undo:hover:not(:disabled) {
		color: var(--status-fault-text);
	}

	.action {
		color: var(--link);
	}
</style>
