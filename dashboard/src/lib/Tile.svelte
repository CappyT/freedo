<script lang="ts" module>
	import type { IconName } from './Icon.svelte';

	export interface TileChip {
		icon?: IconName;
		text: string;
	}

	/** The colour family of a tile: a status, or neutral for a control that has no state colour. */
	export type TileFill = 'heating' | 'idle' | 'away' | 'off' | 'warning' | 'fault' | 'offline' | 'cooling' | 'neutral';

	// The link tile opened last shares its view transition name with the screen it opens, so the tile
	// grows into that screen and shrinks back into the tile on the way home.
	const opened = $state({ href: null as string | null });
</script>

<script lang="ts">
	import { NONE } from './format.ts';
	import { fmt } from './i18n.svelte.ts';
	import Icon from './Icon.svelte';

	interface Props {
		fill: TileFill;
		name: string;
		line: string;
		/** The text a screen reader reads for the whole tile. */
		label: string;
		lineIcon?: IconName;
		/** The large number: the temperature the tile is about. */
		value?: number | null;
		/** A large word in place of the number, for a neutral tile. */
		word?: string;
		chips?: TileChip[];
		/** A large faint icon in the corner, for a state that is not about heat. */
		mark?: IconName | null;
		/** An icon in a disc in the corner, for a neutral tile. */
		disc?: IconName | null;
		discTone?: 'away' | null;
		/** The warnings of the zone. The tile shows a badge and the text goes to its label. */
		alert?: string | null;
		href?: string;
		onclick?: () => void;
		pressed?: boolean;
		expanded?: boolean;
		disabled?: boolean;
		extraClass?: string;
		testid?: string;
	}

	let {
		fill,
		name,
		line,
		label,
		lineIcon,
		value = null,
		word,
		chips = [],
		mark = null,
		disc = null,
		discTone = null,
		alert = null,
		href,
		onclick,
		pressed,
		expanded,
		disabled = false,
		extraClass = '',
		testid
	}: Props = $props();

	const parts = $derived(value === null ? null : fmt.parts(value));
</script>

{#snippet body()}
	{#if mark && !alert}<span class="mark" aria-hidden="true"><Icon name={mark} size={46} /></span>{/if}
	{#if disc}<span class="corner-disc" class:away={discTone === 'away'} aria-hidden="true"><Icon name={disc} size={18} /></span>{/if}
	{#if alert}<span class="badge-alert" title={alert} aria-hidden="true"><Icon name="warning" size={16} /></span>{/if}
	<span class="top" aria-hidden="true">
		{#each chips as chip (chip.text)}
			<span class="chip">{#if chip.icon}<Icon name={chip.icon} size={14} />{/if}{chip.text}</span>
		{/each}
	</span>
	{#if parts}
		<span class="value" aria-hidden="true">
			<span class="whole">{parts.whole}</span>
			<span class="side"><span class="deg">°</span><span class="fraction">{parts.fraction}</span></span>
		</span>
	{:else if word}
		<span class="word" aria-hidden="true">{word}</span>
	{:else}
		<!-- No reading yet: the dash of the dial, so the tile keeps its number line. -->
		<span class="value" aria-hidden="true"><span class="whole">{NONE}</span></span>
	{/if}
	<span class="name" aria-hidden="true">{name}</span>
	<span class="line" aria-hidden="true">{#if lineIcon}<Icon name={lineIcon} size={14} />{/if}{line}</span>
{/snippet}

{#if href}
	<a
		class="tile fill-{fill} {extraClass}"
		class:offline={fill === 'offline'}
		class:neutral={fill === 'neutral'}
		style:view-transition-name={opened.href === href ? 'hero' : undefined}
		{href}
		onclick={() => (opened.href = href ?? null)}
		aria-label={label}
		data-testid={testid}
	>
		{@render body()}
	</a>
{:else}
	<button
		class="tile fill-{fill} {extraClass}"
		class:offline={fill === 'offline'}
		class:neutral={fill === 'neutral'}
		type="button"
		{onclick}
		{disabled}
		aria-label={label}
		aria-pressed={pressed}
		aria-haspopup={expanded === undefined ? undefined : 'menu'}
		aria-expanded={expanded}
		data-testid={testid}
	>
		{@render body()}
	</button>
{/if}

<style>
	.tile {
		position: relative;
		display: flex;
		flex-direction: column;
		align-items: flex-start;
		gap: 0.25rem;
		width: 100%;
		min-height: 9.5rem;
		padding: 0.875rem;
		border: 0;
		border-radius: var(--radius-lg);
		background: var(--fill);
		color: var(--fill-ink);
		font: inherit;
		text-align: left;
		text-decoration: none;
		overflow: hidden;
		cursor: pointer;
		transition:
			translate 0.2s cubic-bezier(0.2, 0.8, 0.2, 1),
			box-shadow 0.2s cubic-bezier(0.2, 0.8, 0.2, 1);
	}

	.tile:hover:not(:disabled) {
		color: var(--fill-ink);
		translate: 0 -2px;
		box-shadow: 0 12px 24px -14px color-mix(in srgb, var(--navy) 60%, transparent);
	}

	.tile:active:not(:disabled) {
		translate: 0 0;
	}

	.tile:disabled {
		cursor: not-allowed;
		opacity: 0.6;
	}

	.tile:focus-visible {
		outline: 3px solid var(--accent);
		outline-offset: 3px;
		border-radius: var(--radius-lg);
	}

	/* Unreachable: no colour to show, only the outline of where it would be. */
	.tile.offline {
		--fill: transparent;
		--fill-ink: var(--text);
		border: 1.5px dashed var(--status-offline-border);
	}

	.tile.neutral {
		--fill: var(--surface);
		--fill-ink: var(--text);
		border: 1px solid var(--border);
	}

	.mark {
		position: absolute;
		top: 0.625rem;
		right: 0.625rem;
		opacity: 0.3;
		display: flex;
	}

	.mark :global(svg) {
		stroke-width: 1.3;
	}

	.corner-disc,
	.badge-alert {
		position: absolute;
		top: 0.75rem;
		right: 0.75rem;
		display: grid;
		place-items: center;
		border-radius: 50%;
	}

	.corner-disc {
		width: 2.125rem;
		height: 2.125rem;
		background: light-dark(var(--ice), var(--navy-border));
		color: var(--link);
	}

	.corner-disc.away {
		background: var(--status-away-tint);
		color: var(--status-away-text);
	}

	.badge-alert {
		width: 1.875rem;
		height: 1.875rem;
		background: var(--surface);
		color: var(--status-warning-text);
		box-shadow: 0 2px 6px color-mix(in srgb, var(--navy) 25%, transparent);
	}

	.top {
		display: flex;
		flex-wrap: wrap;
		gap: 0.3rem;
		min-height: 1.5rem;
		padding-right: 2.5rem;
	}

	.chip {
		display: inline-flex;
		align-items: center;
		gap: 0.25rem;
		height: 1.5rem;
		padding: 0 0.55rem 0 0.45rem;
		border-radius: var(--radius-pill);
		background: color-mix(in srgb, var(--fill-ink) 12%, transparent);
		font-size: 0.75rem;
		font-weight: var(--weight-semibold);
		font-variant-numeric: tabular-nums;
		white-space: nowrap;
	}

	.value {
		display: flex;
		align-items: flex-start;
		margin-top: auto;
		font-family: var(--font-display);
		line-height: 1;
	}

	.whole {
		font-size: 2.875rem;
		font-weight: var(--weight-bold);
		letter-spacing: -0.04em;
	}

	.side {
		display: flex;
		flex-direction: column;
		justify-content: space-between;
		height: 2.375rem;
		margin-left: 0.2rem;
		padding-top: 0.15rem;
		font-weight: var(--weight-semibold);
	}

	.deg {
		font-size: 1.25rem;
	}

	.fraction {
		font-size: 1.125rem;
		letter-spacing: -0.02em;
	}

	.word {
		margin-top: auto;
		font-family: var(--font-display);
		font-size: 1.5rem;
		font-weight: var(--weight-bold);
		line-height: 1.05;
		letter-spacing: -0.02em;
		overflow-wrap: anywhere;
	}

	.name {
		margin-top: 0.125rem;
		font-family: var(--font-display);
		font-size: 1rem;
		font-weight: var(--weight-semibold);
		line-height: 1.2;
		letter-spacing: -0.01em;
		overflow-wrap: anywhere;
	}

	.line {
		display: flex;
		align-items: center;
		gap: 0.3rem;
		font-size: 0.8125rem;
		font-weight: var(--weight-medium);
		line-height: 1.25;
	}

	.line :global(svg) {
		flex: none;
	}
</style>
