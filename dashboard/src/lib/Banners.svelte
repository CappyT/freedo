<script lang="ts">
	import { data } from './data.svelte.ts';
	import Icon, { type IconName } from './Icon.svelte';
	import { t } from './i18n.svelte.ts';
	import type { Tone } from './zones.ts';

	interface Banner {
		tone: Tone;
		icon: IconName;
		text: string;
	}

	// Safety first: say when the daemon cannot reach the devices.
	const banners = $derived.by(() => {
		const list: Banner[] = [];
		const h = data.health;
		if (!data.reachable) list.push({ tone: 'fault', icon: 'offline', text: t('banner.unreachable') });
		if (h?.radioError) list.push({ tone: 'fault', icon: 'fault', text: t('banner.radioError', { error: h.radioError }) });
		if (h?.bridgePresent) list.push({ tone: 'fault', icon: 'warning', text: t('banner.bridge') });
		if (h && h.mode === 'live' && !h.tx) list.push({ tone: 'warning', icon: 'warning', text: t('banner.dryRun') });
		if (h && h.mode !== 'live') list.push({ tone: 'warning', icon: 'warning', text: t('banner.noRadio') });
		return list;
	});
</script>

{#each banners as banner (banner.text)}
	<p class="notice tone-{banner.tone}" role={banner.tone === 'fault' ? 'alert' : 'status'}>
		<Icon name={banner.icon} />
		<span>{banner.text}</span>
	</p>
{/each}
