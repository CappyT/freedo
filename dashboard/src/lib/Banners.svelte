<script lang="ts">
	import { boilerFaults } from './boiler.ts';
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
		// The wait is the one thing to say: the data of the page is from before the restore.
		if (data.waitSec !== null) return list;
		const h = data.health;
		if (data.restarting) list.push({ tone: 'warning', icon: 'refresh', text: t('banner.restarting') });
		else if (!data.reachable) list.push({ tone: 'fault', icon: 'offline', text: t('banner.unreachable') });
		if (h?.radioError) list.push({ tone: 'fault', icon: 'fault', text: t('banner.radioError', { error: h.radioError }) });
		if (h?.bridgePresent) list.push({ tone: 'fault', icon: 'warning', text: t('banner.bridge') });
		if (h && h.mode === 'live' && !h.tx) list.push({ tone: 'warning', icon: 'warning', text: t('banner.dryRun') });
		if (h && h.mode !== 'live') list.push({ tone: 'warning', icon: 'warning', text: t('banner.noRadio') });
		const faults = boilerFaults(data.boiler);
		if (faults.state === 'fault') list.push({ tone: 'fault', icon: 'fault', text: t('boiler.faults', { codes: faults.codes.join(', ') }) });
		return list;
	});
</script>

{#if data.waitSec !== null}
	<p class="notice tone-warning" role="timer" data-testid="import-wait">
		<Icon name="refresh" />
		<span>{data.waitSec > 0 ? t('banner.importWait', { seconds: data.waitSec }) : t('banner.restarting')}</span>
	</p>
{/if}
{#each banners as banner (banner.text)}
	<p class="notice tone-{banner.tone}" role={banner.tone === 'fault' ? 'alert' : 'status'}>
		<Icon name={banner.icon} />
		<span>{banner.text}</span>
	</p>
{/each}
