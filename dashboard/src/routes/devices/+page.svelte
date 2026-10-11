<script lang="ts">
	import { buildDevices } from '#lib/devices.ts';
	import DeviceList from '#lib/DeviceList.svelte';
	import { data } from '#lib/data.svelte.ts';
	import { t } from '#lib/i18n.svelte.ts';

	const devices = $derived(buildDevices(data.nodes, data.health, data.models));
</script>

<section class="page">
	<div class="page-head">
		<h1>{t('devices.title')}</h1>
	</div>

	{#if !data.loaded}
		<p class="muted">{t('state.loading')}</p>
	{:else}
		<section class="panel" aria-label={t('devices.title')}>
			<DeviceList {devices} detailed />
		</section>
		<p class="note">{t('devices.note')}</p>
	{/if}
</section>
