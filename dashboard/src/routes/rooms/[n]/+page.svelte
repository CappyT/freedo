<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { resolve } from '$app/paths';
	import { errorText } from '#lib/actions.ts';
	import ConfirmDialog from '#lib/ConfirmDialog.svelte';
	import { t } from '#lib/i18n.svelte.ts';
	import Icon from '#lib/Icon.svelte';
	import { manage } from '#lib/install.svelte.ts';
	import { api } from '#lib/session.ts';
	import type { InstallCircuit, InstallDevice, InstallZone } from '#lib/types.ts';
	import { zoneName } from '#lib/zones.ts';

	const number = $derived(Number(page.params.n));

	let zone = $state<InstallZone | null>(null);
	let devices = $state<InstallDevice[]>([]);
	let circuits = $state<InstallCircuit[]>([]);
	let loaded = $state(false);
	let problem = $state<string | null>(null);
	// The name in the field. It follows the room until the person types.
	let name = $state('');
	let typed = $state(false);
	let joining = $state('');
	let busy = $state(false);
	let confirmDelete: ConfirmDialog | undefined = $state();

	const title = $derived(zone ? zoneName(zone, t('zone.hotWaterName')) : '');
	const labelOf = (device: InstallDevice) => device.label || device.serial || device.eui;
	const members = $derived(
		(zone?.members ?? []).map((eui) => devices.find((d) => d.eui === eui)).filter((d): d is InstallDevice => d !== undefined)
	);
	// Hot water takes the wall thermostat only.
	const candidates = $derived(
		devices.filter((d) => !zone?.members.includes(d.eui) && (zone?.kind === 'heating' ? d.kind !== 'bridge' : d.kind === 'thermostat'))
	);

	async function load() {
		try {
			const [zones, allDevices, allCircuits] = await Promise.all([api.installZones(), api.installDevices(), api.installCircuits()]);
			zone = zones.find((z) => z.number === number) ?? null;
			devices = allDevices;
			circuits = allCircuits;
			problem = null;
			if (!typed) name = zone?.name ?? '';
		} catch (error) {
			problem = errorText(error);
		}
		loaded = true;
	}

	// The first read, and one more when the person opens another room.
	$effect(() => {
		void number;
		void load();
	});

	async function run(command: () => Promise<unknown>, done: string): Promise<boolean> {
		busy = true;
		const ok = (await manage(async () => (await command(), true), done)) === true;
		await load();
		busy = false;
		return ok;
	}

	async function rename(event: SubmitEvent) {
		event.preventDefault();
		const next = name.trim();
		if (next === '' || next === zone?.name) return;
		if (await run(() => api.patchZone(number, { name: next }), t('rooms.renamed'))) typed = false;
	}

	/** A select keeps the choice when the request fails: show what the daemon reports. */
	async function choose(select: HTMLSelectElement, current: string, command: (value: string) => Promise<unknown>) {
		if (!(await run(() => command(select.value), t('rooms.saved')))) select.value = current;
	}

	async function join() {
		if (joining === '') return;
		if (await run(() => api.addMember(number, joining), t('devices.moved'))) joining = '';
	}

	async function remove() {
		if ((await manage(() => api.deleteZone(number), t('rooms.deleted'))) !== undefined) await goto(resolve('/devices'));
	}
</script>

<section class="page">
	<a class="back small" href={resolve('/devices')}><Icon name="back" size={16} />{t('devices.title')}</a>

	{#if !loaded}
		<p class="muted">{t('state.loading')}</p>
	{:else if problem}
		<p class="notice tone-fault" role="alert"><Icon name="fault" /><span>{problem}</span></p>
	{:else if !zone}
		<div class="panel">
			<h1>{t('rooms.missing.title')}</h1>
			<p>{t('rooms.missing.body')}</p>
		</div>
	{:else}
		<div class="page-head titled">
			<h1>{title}</h1>
			<span class="small muted">{t(`rooms.kind.${zone.kind}`)}</span>
		</div>

		{#if zone.kind === 'heating'}
		<section class="panel" aria-label={t('rooms.name')}>
			<form class="row fill" onsubmit={rename}>
				<label class="field grow">
					<span>{t('rooms.name')}</span>
					<input class="input" bind:value={name} oninput={() => (typed = true)} autocomplete="off" />
				</label>
				<button class="btn primary" type="submit" disabled={busy || name.trim() === '' || name.trim() === zone.name}>
					<Icon name="check" size={18} />{t('rooms.save')}
				</button>
			</form>
		</section>
		{/if}

		<section class="panel" aria-labelledby="members-title">
			<header><h2 id="members-title">{t('zone.devices')}</h2></header>
			{#if members.length === 0}
				<p class="muted">{t('rooms.empty')}</p>
			{:else}
				<ul class="members">
					{#each members as device (device.eui)}
						<li>
							<strong>{labelOf(device)}</strong>
							<span class="small muted">{t(`kind.${device.kind}`)} <span class="mono">{device.serial || device.eui}</span></span>
						</li>
					{/each}
				</ul>
			{/if}
			{#if candidates.length > 0}
				<div class="row fill">
					<label class="field grow">
						<span>{t('rooms.join')}</span>
						<select class="input" bind:value={joining}>
							<option value="">{t('rooms.join.pick')}</option>
							{#each candidates as device (device.eui)}<option value={device.eui}>{labelOf(device)}</option>{/each}
						</select>
					</label>
					<button class="btn" disabled={busy || joining === ''} onclick={join}><Icon name="plus" size={18} />{t('rooms.join.add')}</button>
				</div>
			{/if}
			<p class="note">{t('rooms.members.note')}</p>
		</section>

		{#if zone.kind === 'heating'}
			<section class="panel" aria-labelledby="heating-title">
				<header><h2 id="heating-title">{t('rooms.heating')}</h2></header>
				<div class="choices">
					<label class="field">
						<span>{t('rooms.circuit')}</span>
						<select
							class="input"
							value={zone.circuit === null ? '' : String(zone.circuit)}
							disabled={busy}
							onchange={(e) =>
								choose(e.currentTarget, zone?.circuit == null ? '' : String(zone.circuit), (v) =>
									api.patchZone(number, { circuit: v === '' ? null : Number(v) })
								)}
						>
							<option value="">{t('rooms.circuit.none')}</option>
							{#each circuits as c (c.id)}<option value={String(c.id)}>{c.name || t('boiler.circuit', { id: c.id })}</option>{/each}
						</select>
						<span class="note">{t('rooms.circuit.hint')}</span>
					</label>
					<label class="field">
						<span>{t('rooms.measuring')}</span>
						<select
							class="input"
							value={zone.measuring_device ?? ''}
							disabled={busy}
							onchange={(e) =>
								choose(e.currentTarget, zone?.measuring_device ?? '', (v) => api.setRole(number, 'measuring', v === '' ? null : v))}
						>
							<option value="">{t('rooms.role.auto')}</option>
							{#each members as device (device.eui)}<option value={device.eui}>{labelOf(device)}</option>{/each}
						</select>
						<span class="note">{t('rooms.measuring.hint')}</span>
					</label>
					<label class="field">
						<span>{t('rooms.reporter')}</span>
						<select
							class="input"
							value={zone.reporter ?? ''}
							disabled={busy}
							onchange={(e) =>
								choose(e.currentTarget, zone?.reporter ?? '', (v) => api.setRole(number, 'reporter', v === '' ? null : v))}
						>
							<option value="">{t('rooms.role.none')}</option>
							{#each members as device (device.eui)}<option value={device.eui}>{labelOf(device)}</option>{/each}
						</select>
						<span class="note">{t('rooms.reporter.hint')}</span>
					</label>
				</div>
			</section>
		{/if}

		<section class="panel" aria-labelledby="delete-title">
			<header><h2 id="delete-title">{t('rooms.delete')}</h2></header>
			<p class="note">{t('rooms.delete.note')}</p>
			<div class="row">
				<button class="btn danger" disabled={busy} onclick={() => confirmDelete?.show()}><Icon name="trash" size={18} />{t('rooms.delete')}</button>
			</div>
		</section>
	{/if}
</section>

<ConfirmDialog
	bind:this={confirmDelete}
	title={t('rooms.delete.title', { name: title })}
	body={members.length > 0 ? t('rooms.delete.body.members') : t('rooms.delete.body')}
	items={members.map(labelOf)}
	confirm={t('rooms.delete')}
	onconfirm={remove}
/>

<style>
	.back {
		display: inline-flex;
		align-items: center;
		gap: 0.25rem;
		justify-self: start;
		text-decoration: none;
	}

	.members {
		list-style: none;
		margin: 0;
		padding: 0;
		display: grid;
	}

	.members li {
		display: grid;
		gap: 0.15rem;
		padding: 0.7rem 0;
		border-top: 1px solid var(--border);
	}

	.members li:first-child {
		border-top: 0;
		padding-top: 0;
	}

	.choices {
		display: grid;
		grid-template-columns: repeat(auto-fit, minmax(min(100%, 16rem), 1fr));
		gap: 1rem 1.5rem;
		align-items: start;
	}

	.grow {
		flex: 1 1 14rem;
	}
</style>
