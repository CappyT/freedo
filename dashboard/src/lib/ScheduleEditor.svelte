<script lang="ts">
	import { fmt, t } from './i18n.svelte.ts';
	import Icon from './Icon.svelte';
	import { typedNumber } from './numeric.ts';
	import {
		DAYS,
		addBlock,
		addGroup,
		formatTime,
		parseTime,
		removeBlock,
		removeGroup,
		setBlock,
		toggleDay,
		type Group,
		type Issue,
		type Range
	} from './schedule.ts';

	let { groups = $bindable(), range, issues }: { groups: Group[]; range: Range; issues: Issue[] } = $props();

	const bad = (g: number, b?: number) => issues.some((i) => i.group === g && (b === undefined || i.block === b));

	function issueText(issue: Issue): string {
		return t(`issue.${issue.code}`, {
			group: (issue.group ?? 0) + 1,
			block: (issue.block ?? 0) + 1,
			days: (issue.days ?? []).map((d) => fmt.day(d)).join(', '),
			min: fmt.number(range.min),
			max: fmt.number(range.max)
		});
	}

	function onTime(key: number, index: number, event: Event) {
		const minute = parseTime((event.currentTarget as HTMLInputElement).value);
		if (minute !== null) groups = setBlock(groups, key, index, { minute });
	}

	function onTemp(key: number, index: number, event: Event, leaving: boolean) {
		const stored = groups.find((g) => g.key === key)?.blocks[index]?.setpointC ?? Number.NaN;
		const setpointC = typedNumber((event.currentTarget as HTMLInputElement).valueAsNumber, stored, leaving);
		if (!Object.is(setpointC, stored)) groups = setBlock(groups, key, index, { setpointC });
	}
</script>

<div class="editor">
	{#each groups as group, g (group.key)}
		<fieldset class="group" class:invalid={bad(g)}>
			<legend>{t('editor.group', { n: g + 1 })}</legend>

			<div class="days" role="group" aria-label={t('editor.days')}>
				{#each DAYS as day (day)}
					<label class="chip">
						<input type="checkbox" checked={group.days.includes(day)} onchange={() => (groups = toggleDay(groups, group.key, day))} />
						<span>{fmt.day(day)}</span>
					</label>
				{/each}
			</div>

			<ol class="blocks">
				{#each group.blocks as block, i (i)}
					<li class="block">
						<label class="field">
							<span>{t('editor.from')}</span>
							<input
								class="input mono"
								type="time"
								value={formatTime(block.minute)}
								disabled={i === 0}
								aria-invalid={bad(g, i)}
								onchange={(e) => onTime(group.key, i, e)}
							/>
						</label>
						<label class="field">
							<span>{t('editor.temperature')}</span>
							<input
								class="input mono"
								type="number"
								min={range.min}
								max={range.max}
								step={range.step}
								value={block.setpointC}
								aria-invalid={bad(g, i)}
								oninput={(e) => onTemp(group.key, i, e, false)}
								onchange={(e) => onTemp(group.key, i, e, true)}
							/>
						</label>
						<label class="off">
							<input
								type="checkbox"
								checked={!block.enabled}
								onchange={(e) => (groups = setBlock(groups, group.key, i, { enabled: !e.currentTarget.checked }))}
							/>
							<span>{t('schedule.off')}</span>
						</label>
						<button
							class="btn icon"
							type="button"
							disabled={i === 0}
							aria-label={t('editor.removeBlock', { n: i + 1 })}
							onclick={() => (groups = removeBlock(groups, group.key, i))}
						>
							<Icon name="trash" size={18} />
						</button>
					</li>
				{/each}
			</ol>

			<div class="row">
				<button class="btn" type="button" onclick={() => (groups = addBlock(groups, group.key))}>
					<Icon name="plus" size={18} />{t('editor.addBlock')}
				</button>
				{#if groups.length > 1}
					<button class="btn danger" type="button" onclick={() => (groups = removeGroup(groups, group.key))}>
						<Icon name="trash" size={18} />{t('editor.removeGroup')}
					</button>
				{/if}
			</div>
		</fieldset>
	{/each}

	<div>
		<button class="btn" type="button" onclick={() => (groups = addGroup(groups))}>
			<Icon name="plus" size={18} />{t('editor.addGroup')}
		</button>
	</div>

	{#if issues.length > 0}
		<ul class="issues notice tone-fault" role="alert">
			{#each issues as issue, i (i)}
				<li>{issueText(issue)}</li>
			{/each}
		</ul>
	{/if}
</div>

<style>
	.editor {
		display: grid;
		gap: 1rem;
	}

	.group {
		display: grid;
		gap: 0.9rem;
		margin: 0;
		padding: 1rem;
		border: 1px solid var(--border-strong);
		border-radius: var(--radius-md);
		min-width: 0;
	}

	.group.invalid {
		border-color: var(--status-fault);
	}

	legend {
		padding: 0 0.4rem;
		font-family: var(--font-display);
		font-weight: var(--weight-semibold);
	}

	.days {
		display: flex;
		flex-wrap: wrap;
		gap: 0.4rem;
	}

	.chip {
		position: relative;
		display: inline-grid;
		place-items: center;
		min-width: 3rem;
		min-height: 2.5rem;
		padding: 0 0.7rem;
		border: 1px solid var(--border-strong);
		border-radius: var(--radius-pill);
		font-size: var(--text-small-size);
		font-weight: var(--weight-medium);
		cursor: pointer;
	}

	.chip input {
		position: absolute;
		inset: 0;
		opacity: 0;
		cursor: pointer;
	}

	.chip:has(input:checked) {
		background: var(--action);
		border-color: var(--action);
		color: var(--action-text);
	}

	.chip:has(input:focus-visible) {
		outline: 3px solid var(--accent);
		outline-offset: 2px;
	}

	.blocks {
		list-style: none;
		margin: 0;
		padding: 0;
		display: grid;
		gap: 0.6rem;
	}

	.block {
		display: grid;
		grid-template-columns: minmax(7rem, 9rem) minmax(6rem, 8rem) auto 2.75rem;
		gap: 0.5rem 1rem;
		align-items: end;
	}

	.off {
		display: inline-flex;
		align-items: center;
		gap: 0.5rem;
		min-height: 2.75rem;
		font-size: var(--text-small-size);
		font-weight: var(--weight-medium);
		cursor: pointer;
	}

	.off input {
		width: 1.15rem;
		height: 1.15rem;
		accent-color: var(--action);
	}

	.issues {
		display: grid;
		gap: 0.25rem;
		margin: 0;
		padding-left: 2rem;
		list-style: disc;
	}

	@media (max-width: 520px) {
		.block {
			grid-template-columns: 1fr 1fr;
		}

		.block .off {
			grid-column: 1;
		}
	}
</style>
