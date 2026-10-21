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

	// The form and the circuit the person opened. They live here, so a data poll does not close them.
	let adding = $state<'room' | 'device' | 'circuit' | null>(null);
	let openCircuit = $state<number | null>(null);
	let doomed = $state<Circuit | null>(null);
	let confirmCircuit: ConfirmDialog | undefined = $state();

	const devices = $derived(buildDevices(data.nodes, data.health, data.models));
	const loose = $derived(devices.filter((d) => d.zones.length === 0));
	const thermostats = $derived(devices.filter((d) => d.kind === 'thermostat'));

	const sourceOf = (circuit: Circuit) => shortOf(data.health?.devices ?? [], circuit.HeatSource);
	const sourceName = (circuit: Circuit) => thermostats.find((d) => d.short === sourceOf(circuit))?.label ?? '-';
	const roomsOn = (circuit: Circuit) => (data.zones?.declared ?? []).filter((z) => z.circuit === circuit.ID).length;

	onMount(() => void install.load());

	const toggle = (form: 'room' | 'device' | 'circuit') => (adding = adding === form ? null : form);

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
	<h1>{t('devices.title')}</h1>

	<div class="actions">
		<button class="btn primary" aria-expanded={adding === 'device'} onclick={() => toggle('device')}>
			<Icon name="plus" size={18} />{t('pair.title')}
		</button>
		<button class="btn" aria-expanded={adding === 'room'} onclick={() => toggle('room')}>{t('rooms.add')}</button>
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
			<AddDevice onclose={() => (adding = null)} />
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

		<div class="rooms">
			{#each data.models as model (model.zone.number)}
				{@const zone = model.zone}
				{@const name = zoneName(zone, t('zone.hotWaterName'))}
				<section class="section" aria-label={name} data-room={zone.number}>
					<header class="section-head">
						<h2>{name}</h2>
						<a class="section-link" href={resolve('/rooms/[n]', { n: String(zone.number) })} aria-label="{t('rooms.settings')}: {name}">
							{t('nav.settings')}<Icon name="chevron" size={16} />
						</a>
					</header>
					{#if zone.members.length === 0}
						<p class="list none">{t('rooms.empty')}</p>
					{:else}
						<DeviceList devices={devices.filter((d) => zone.members.includes(d.short))} room={zone.number} />
					{/if}
				</section>
			{/each}

			<section class="section" aria-labelledby="loose-title" data-room="none">
				<header class="section-head">
					<h2 id="loose-title">{t('devices.noRoom')}</h2>
					<a class="section-link" href={resolve('/rooms/[n]', { n: 'none' })} aria-label="{t('rooms.settings')}: {t('devices.noRoom')}">
						{t('nav.settings')}<Icon name="chevron" size={16} />
					</a>
				</header>
				{#if loose.length === 0}
					<p class="list none">{t('rooms.none.empty')}</p>
				{:else}
					<DeviceList devices={loose} />
				{/if}
			</section>

			<section class="section" aria-labelledby="circuits-title">
				<header class="section-head">
					<h2 id="circuits-title">{t('circuits.title')}</h2>
					<button class="section-link" aria-expanded={adding === 'circuit'} onclick={() => toggle('circuit')}>
						{adding === 'circuit' ? t('action.close') : t('circuits.add')}
					</button>
				</header>
				<ul class="list">
					{#each data.circuits as circuit (circuit.ID)}
						{@const open = openCircuit === circuit.ID}
						<li>
							<button class="list-row" aria-expanded={open} onclick={() => (openCircuit = open ? null : circuit.ID)}>
								<span class="disc"><Icon name="flame" size={18} /></span>
								<span class="list-main">
									<span class="list-label">{circuit.Name || t('boiler.circuit', { id: circuit.ID })}</span>
									<span class="list-sub">{t('circuits.summary', { source: sourceName(circuit), n: roomsOn(circuit) })}</span>
								</span>
								<span class="chevron" class:open><Icon name="chevron" size={18} /></span>
							</button>
							{#if open}
								<div class="circuit row fill">
									<label class="field inline grow">
										<span>{t('circuits.heatSource')}</span>
										<select class="input" value={sourceOf(circuit)} onchange={(e) => setHeatSource(circuit, e.currentTarget)}>
											{#each thermostats as d (d.short)}<option value={d.short}>{d.label}</option>{/each}
										</select>
									</label>
									<button class="btn danger" onclick={() => askDeleteCircuit(circuit)}><Icon name="trash" size={18} />{t('circuits.delete')}</button>
								</div>
							{/if}
						</li>
					{/each}
					{#if adding === 'circuit' || data.circuits.length === 0}
						<li class="add"><CircuitForm /></li>
					{/if}
				</ul>
				<p class="note">{t('circuits.note')}</p>
			</section>
		</div>

		{#if (install.info?.warnings.length ?? 0) > 0}
			<section class="section" aria-labelledby="checks-title">
				<div class="section-head"><h2 id="checks-title">{t('install.checks')}</h2></div>
				<ul class="list checks small">
					{#each install.info?.warnings ?? [] as warning (warning)}<li class="list-row"><Icon name="warning" size={18} />{warning}</li>{/each}
				</ul>
				<p class="note">{t('install.checks.note')}</p>
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
	/* Two commands side by side; on a phone too narrow for both, the second takes a line of its own. */
	.actions {
		display: flex;
		flex-wrap: wrap;
		gap: 0.625rem;
	}

	.actions .btn {
		flex: 1 1 auto;
		padding-inline: 0.75rem;
		white-space: nowrap;
	}

	/* One column on every width: an opened device line grows its room, and a grid row would leave a
	   hole beside it. */
	.page {
		max-width: 48rem;
	}

	.rooms {
		display: grid;
		gap: 1.375rem;
	}

	.none {
		padding: 0.875rem;
		color: var(--text-muted);
		font-size: var(--text-small-size);
	}

	button.section-link {
		padding: 0;
		border: 0;
		background: none;
		cursor: pointer;
	}

	.chevron {
		transition: rotate 0.2s cubic-bezier(0.2, 0.8, 0.2, 1);
	}

	.chevron.open {
		rotate: 90deg;
	}

	.circuit {
		padding: 0 0.875rem 0.875rem 3.875rem;
	}

	.add {
		padding: 0.875rem;
	}

	.checks .list-row {
		align-items: flex-start;
		overflow-wrap: anywhere;
		color: var(--status-warning-text);
	}

	.grow {
		flex: 1 1 12rem;
	}

	@media (min-width: 900px) {
		.actions .btn {
			flex: 0 0 auto;
		}
	}

	@media (max-width: 559px) {
		.circuit {
			padding-left: 0.875rem;
		}
	}

	@media (prefers-reduced-motion: reduce) {
		.chevron {
			transition: none;
		}
	}
</style>
