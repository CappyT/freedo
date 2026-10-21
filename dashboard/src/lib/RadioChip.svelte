<script lang="ts">
	import { resolve } from '$app/paths';
	import { data } from './data.svelte.ts';
	import { t } from './i18n.svelte.ts';
	import type { Tone } from './zones.ts';

	// One word on the radio, the worst first. The banners above the page say the same thing in full.
	const radio = $derived.by((): { tone: Tone; text: string } | null => {
		const h = data.health;
		if (!data.reachable) return { tone: 'fault', text: t('radio.unreachable') };
		if (!h) return null;
		if (h.radioError) return { tone: 'fault', text: t('radio.stopped') };
		if (h.bridgePresent) return { tone: 'fault', text: t('radio.blocked') };
		if (h.mode === 'mock') return { tone: 'off', text: t('mode.mock') };
		if (h.mode === 'replay') return { tone: 'off', text: t('mode.replay') };
		if (!h.tx) return { tone: 'warning', text: t('radio.dryRun') };
		return { tone: 'idle', text: t('mode.live') };
	});
</script>

{#if radio}
	<a class="radio" href={resolve('/settings')} data-testid="radio">
		<span class="dot" style:--dot="var(--status-{radio.tone}-text)"></span>{radio.text}
	</a>
{/if}

<style>
	.radio {
		display: inline-flex;
		align-items: center;
		gap: 0.45rem;
		min-height: 2.25rem;
		padding: 0 0.8rem;
		border-radius: var(--radius-pill);
		background: var(--surface);
		border: 1px solid var(--border);
		color: var(--text);
		font-size: 0.8125rem;
		font-weight: var(--weight-medium);
		text-decoration: none;
		white-space: nowrap;
	}

	.radio:hover {
		color: var(--text);
		border-color: var(--accent);
	}
</style>
