<script lang="ts">
	import { resolve } from '$app/paths';
	import { onMount } from 'svelte';
	import AddDevice from '#lib/AddDevice.svelte';
	import CircuitForm from '#lib/CircuitForm.svelte';
	import ConfirmDialog from '#lib/ConfirmDialog.svelte';
	import { buildDevices } from '#lib/devices.ts';
	import DeviceList from '#lib/DeviceList.svelte';
	import { data } from '#lib/data.svelte.ts';
	import { t } from '#lib/i18n.svelte.ts';
	import Icon from '#lib/Icon.svelte';
	import { install, manage } from '#lib/install.svelte.ts';
	import { shortOf } from '#lib/install.ts';
	import RoomForm from '#lib/RoomForm.svelte';
	import { api } from '#lib/session.ts';
	import type { Circuit } from '#lib/types.ts';
	import { zoneName } from '#lib/zones.ts';

	// The form the person opened. It lives here, so a data poll does not close it.
	let adding = $state<'room' | 'device' | null>(null);
	let doomed = $state<Circuit | null>(null);
	let confirmCircuit: ConfirmDialog | undefined = $state();

	const devices = $derived(buildDevices(data.nodes, data.health, data.models));
	const loose = $derived(devices.filter((d) => d.zones.length === 0));
	const thermostats = $derived(devices.filter((d) => d.kind === 'thermostat'));

	const sourceOf = (circuit: Circuit) => shortOf(data.health?.devices ?? [], circuit.HeatSource);

	onMount(() => void install.load());

	const setHeatSource = (circuit: Circuit, select: HTMLSelectElement) =>
		manage(() => api.patchCircuit(circuit.ID, { heat_source: select.value }), t('circuits.saved')).then((done) => {
			if (!done) select.value = sourceOf(circuit);
		});

	function askDeleteCircuit(circuit: Circuit) {
		doomed = circuit;
		confirmCircuit?.show();
	}
</script>

<section class="page">
	<div class="page-head">
		<h1>{t('devices.title')}</h1>
		<div class="row fill">
			<a class="btn" href={resolve('/setup')}><Icon name="check" size={18} />{t('setup.title')}</a>
			<button class="btn" aria-expanded={adding === 'room'} onclick={() => (adding = adding === 'room' ? null : 'room')}>
				<Icon name="plus" size={18} />{t('rooms.add')}
			</button>
			<button class="btn primary" aria-expanded={adding === 'device'} onclick={() => (adding = adding === 'device' ? null : 'device')}>
				<Icon name="plus" size={18} />{t('pair.title')}
			</button>
		</div>
	</div>

	{#if adding === 'room'}
		<section class="panel" aria-labelledby="add-room-title">
			<header>
				<h2 id="add-room-title">{t('rooms.add')}</h2>
				<button class="btn" onclick={() => (adding = null)}><Icon name="close" size={18} />{t('action.close')}</button>
			</header>
			<RoomForm />
			<p class="note">{t('rooms.add.note')}</p>
		</section>
	{:else if adding === 'device'}
		<section class="panel" aria-labelledby="add-device-title">
			<header>
				<h2 id="add-device-title">{t('pair.title')}</h2>
				<button class="btn" onclick={() => (adding = null)}><Icon name="close" size={18} />{t('action.close')}</button>
			</header>
			<AddDevice />
		</section>
	{/if}

	{#if !data.loaded}
		<p class="muted">{t('state.loading')}</p>
	{:else}
		{#if data.models.length === 0 && devices.length === 0}
			<div class="panel">
				<h2>{t('home.empty.title')}</h2>
				<p>{t('home.empty.body')}</p>
				<div class="row"><a class="btn primary" href={resolve('/setup')}>{t('setup.title')}</a></div>
			</div>
		{/if}

		{#each data.models as model (model.zone.number)}
			{@const zone = model.zone}
			<section class="panel room" aria-label={zoneName(zone, t('zone.hotWaterName'))} data-room={zone.number}>
				<header>
					<div class="row">
						<h2>{zoneName(zone, t('zone.hotWaterName'))}</h2>
						<span class="small muted">{t(`rooms.kind.${zone.kind}`)}</span>
					</div>
					<a class="btn" href={resolve('/rooms/[n]', { n: String(zone.number) })}>
						<Icon name="sliders" size={18} />{t('rooms.settings')}
					</a>
				</header>
				{#if zone.members.length === 0}
					<p class="muted">{t('rooms.empty')}</p>
				{:else}
					<DeviceList devices={devices.filter((d) => zone.members.includes(d.short))} detailed />
				{/if}
			</section>
		{/each}

		{#if loose.length > 0}
			<!-- The devices in no room, shown as the last room. The group has no panel while every device has a room. -->
			<section class="panel room" aria-labelledby="loose-title" data-room="none">
				<header>
					<h2 id="loose-title">{t('devices.noRoom')}</h2>
					<a class="btn" href={resolve('/rooms/[n]', { n: 'none' })}>
						<Icon name="sliders" size={18} />{t('rooms.settings')}
					</a>
				</header>
				<DeviceList devices={loose} detailed />
			</section>
		{/if}

		<section class="panel" aria-labelledby="circuits-title">
			<header><h2 id="circuits-title">{t('circuits.title')}</h2></header>
			<p class="note">{t('circuits.note')}</p>
			{#if data.circuits.length > 0}
				<ul class="circuits">
					{#each data.circuits as circuit (circuit.ID)}
						<li class="row fill">
							<strong class="grow">{circuit.Name || t('boiler.circuit', { id: circuit.ID })}</strong>
							<label class="field inline">
								<span>{t('circuits.heatSource')}</span>
								<select class="input" value={sourceOf(circuit)} onchange={(e) => setHeatSource(circuit, e.currentTarget)}>
									{#each thermostats as d (d.short)}<option value={d.short}>{d.label}</option>{/each}
								</select>
							</label>
							<button class="btn danger" onclick={() => askDeleteCircuit(circuit)}><Icon name="trash" size={18} />{t('circuits.delete')}</button>
						</li>
					{/each}
				</ul>
			{/if}
			<CircuitForm />
		</section>

		{#if (install.info?.warnings.length ?? 0) > 0}
			<section class="panel" aria-labelledby="checks-title">
				<header><h2 id="checks-title">{t('install.checks')}</h2></header>
				<p class="note">{t('install.checks.note')}</p>
				<ul class="checks small">
					{#each install.info?.warnings ?? [] as warning (warning)}<li>{warning}</li>{/each}
				</ul>
			</section>
		{/if}

		<p class="note">{t('devices.note')}</p>
	{/if}
</section>

<ConfirmDialog
	bind:this={confirmCircuit}
	title={t('circuits.delete.title', { name: doomed?.Name ?? '' })}
	body={t('circuits.delete.body')}
	confirm={t('circuits.delete')}
	onconfirm={() => doomed && manage(() => api.deleteCircuit(doomed!.ID), t('circuits.deleted'))}
/>

<style>
	.circuits,
	.checks {
		margin: 0;
		padding: 0;
		list-style: none;
		display: grid;
		gap: 0.75rem;
	}

	.checks {
		padding-left: 1.25rem;
		list-style: disc;
		gap: 0.35rem;
		overflow-wrap: anywhere;
	}

	.grow {
		flex: 1 1 8rem;
	}
</style>
