<script lang="ts">
	import { t } from './i18n.svelte.ts';
	import type { StateKey, Tone } from './zones.ts';

	let { state, tone }: { state: StateKey; tone: Tone } = $props();
</script>

<span class="badge tone-{tone}" class:offline={tone === 'offline'}>{t(`state.${state}`)}</span>
