<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { resolve } from '$app/paths';
	import { tick } from 'svelte';
	import { errorText } from '#lib/actions.ts';
	import ConfirmDialog from '#lib/ConfirmDialog.svelte';
	import { t } from '#lib/i18n.svelte.ts';
	import Icon from '#lib/Icon.svelte';
	import { manage } from '#lib/install.svelte.ts';
	import { canJoin, describeEffect, movePlan, moveTargets } from '#lib/install.ts';
	import { api } from '#lib/session.ts';
	import { toasts } from '#lib/toast.svelte.ts';
	import type { InstallCircuit, InstallDevice, InstallZone, Removal } from '#lib/types.ts';
	import { zoneName } from '#lib/zones.ts';

	// The route "none" is the group of the devices in no room. It has members and no settings of a zone.
	const number = $derived(page.params.n === 'none' ? null : Number(page.params.n));

	let zones = $state<InstallZone[]>([]);
	let devices = $state<InstallDevice[]>([]);
	let circuits = $state<InstallCircuit[]>([]);
	let loaded = $state(false);
	let problem = $state<string | null>(null);
	// The name in the field. It follows the room until the person types.
	let name = $state('');
	let typed = $state(false);
	let joining = $state('');
	let busy = $state(false);
	// The EUI of the member whose name is being changed, null when none is.
	let editing = $state<string | null>(null);
	let draft = $state('');
	let removal = $state<Removal | null>(null);
	let removing = $state<InstallDevice | null>(null);
	let confirmDelete: ConfirmDialog | undefined = $state();
	let confirmRemove: ConfirmDialog | undefined = $state();

	const zone = $derived(zones.find((z) => z.number === number) ?? null);
	const hotWater = $derived(t('zone.hotWaterName'));
	const title = $derived(number === null ? t('devices.noRoom') : zone ? zoneName(zone, hotWater) : '');
	const labelOf = (device: InstallDevice) => device.label || device.serial || device.eui;
	const roomsOf = (device: InstallDevice) => zones.filter((z) => z.members.includes(device.eui));
	const members = $derived(
		number === null
			? devices.filter((d) => roomsOf(d).length === 0)
			: (zone?.members ?? []).map((eui) => devices.find((d) => d.eui === eui)).filter((d): d is InstallDevice => d !== undefined)
	);
	const candidates = $derived(zone ? devices.filter((d) => canJoin(d.kind, roomsOf(d), zone)) : []);
	const targets = (device: InstallDevice) => moveTargets(device.kind, roomsOf(device), number, zones);
	const effects = $derived(
		(removal?.effects ?? []).map((effect) => {
			const { key, name: on } = describeEffect(
				effect,
				(n) => {
					const z = zones.find((x) => x.number === n);
					return z ? zoneName(z, hotWater) : String(n);
				},
				(id) => circuits.find((c) => c.id === id)?.name || t('boiler.circuit', { id })
			);
			return t(key, { name: on });
		})
	);

	async function load() {
		try {
			[zones, devices, circuits] = await Promise.all([api.installZones(), api.installDevices(), api.installCircuits()]);
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

	async function renameRoom(event: SubmitEvent) {
		event.preventDefault();
		const next = name.trim();
		if (number === null || next === '' || next === zone?.name) return;
		if (await run(() => api.patchZone(number, { name: next }), t('rooms.renamed'))) typed = false;
	}

	/** A select keeps the choice when the request fails: show what the daemon reports. */
	async function choose(select: HTMLSelectElement, current: string, command: (value: string) => Promise<unknown>) {
		if (!(await run(() => command(select.value), t('rooms.saved')))) select.value = current;
	}

	async function join() {
		if (number === null || joining === '') return;
		if (await run(() => api.addMember(number, joining), t('devices.moved'))) joining = '';
	}

	function startRename(device: InstallDevice) {
		editing = device.eui;
		draft = labelOf(device);
	}

	// A field that opens on a click gets no focus by itself, and the browser ignores autofocus.
	function focus(node: HTMLInputElement) {
		node.focus();
		node.select();
	}

	async function renameDevice(device: InstallDevice) {
		const label = draft.trim();
		if (label === '' || label === device.label) {
			editing = null;
			return;
		}
		if (await run(() => api.renameDevice(device.eui, label), t('devices.renamed'))) editing = null;
	}

	async function move(device: InstallDevice, select: HTMLSelectElement) {
		const to = select.value === '' ? null : Number(select.value);
		const done = await run(async () => {
			for (const step of movePlan(device.zone, number, to)) {
				if (step.op === 'claim') await api.claimZone(device.eui, step.zone);
				else if (step.op === 'leave') await api.removeMember(step.zone, device.eui);
				else await api.addMember(step.zone, device.eui);
			}
		}, t('devices.moved'));
		if (!done) select.value = number === null ? '' : String(number);
	}

	/** Asks the daemon what the removal changes, and shows it before the person confirms. */
	async function askRemove(device: InstallDevice) {
		let answer: Removal;
		try {
			answer = await api.removal(device.eui);
		} catch (error) {
			toasts.push('error', errorText(error));
			return;
		}
		removing = device;
		removal = answer;
		await tick();
		confirmRemove?.show();
	}

	function removeDevice() {
		const device = removing;
		if (!device) return;
		// The person saw the effects, so the delete releases the zone memberships. It never releases a heat source.
		void run(() => api.deleteDevice(device.eui, true), t('devices.removed'));
	}

	async function deleteRoom() {
		if (number === null) return;
		if ((await manage(() => api.deleteZone(number), t('rooms.deleted'))) !== undefined) await goto(resolve('/devices'));
	}
</script>

<section class="page">
	<a class="back small" href={resolve('/devices')}><Icon name="back" size={16} />{t('devices.title')}</a>

	{#if !loaded}
		<p class="muted">{t('state.loading')}</p>
	{:else if problem}
		<p class="notice tone-fault" role="alert"><Icon name="fault" /><span>{problem}</span></p>
	{:else if number !== null && !zone}
		<div class="panel">
			<h1>{t('rooms.missing.title')}</h1>
			<p>{t('rooms.missing.body')}</p>
		</div>
	{:else}
		<div class="page-head titled">
			<h1>{title}</h1>
			{#if zone}<span class="small muted">{t(`rooms.kind.${zone.kind}`)}</span>{/if}
		</div>

		{#if zone?.kind === 'heating'}
			<section class="panel" aria-label={t('rooms.name')}>
				<form class="row fill" onsubmit={renameRoom}>
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
			<header><h2 id="members-title">{zone ? t('zone.devices') : t('setup.devices')}</h2></header>
			{#if number === null}<p class="note">{t('rooms.none.note')}</p>{/if}
			{#if members.length === 0}
				<p class="muted">{zone ? t('rooms.empty') : t('rooms.none.empty')}</p>
			{:else}
				<ul class="members">
					{#each members as device (device.eui)}
						<li class="member" data-member={labelOf(device)}>
							<div class="id">
								{#if editing === device.eui}
									<form
										class="rename row"
										onsubmit={(e) => {
											e.preventDefault();
											void renameDevice(device);
										}}
									>
										<input
											class="input"
											bind:value={draft}
											aria-label={t('devices.name')}
											use:focus
											onkeydown={(e) => {
												if (e.key === 'Escape') editing = null;
											}}
										/>
										<button class="btn icon" type="submit" disabled={busy} aria-label={t('devices.save')}>
											<Icon name="check" size={18} />
										</button>
										<button class="btn icon" type="button" onclick={() => (editing = null)} aria-label={t('action.cancel')}>
											<Icon name="close" size={18} />
										</button>
									</form>
								{:else}
									<strong>{labelOf(device)}</strong>
								{/if}
								<span class="small muted">{t(`kind.${device.kind}`)} <span class="mono">{device.serial || device.eui}</span></span>
							</div>
							<div class="actions row fill">
								<button class="btn" disabled={busy} onclick={() => startRename(device)}>
									<Icon name="edit" size={18} />{t('devices.rename')}
								</button>
								<label class="field inline">
									<span>{t('devices.zone')}</span>
									<select class="input" value={number === null ? '' : String(number)} disabled={busy} onchange={(e) => move(device, e.currentTarget)}>
										<option value="">{t('devices.noRoom')}</option>
										{#if zone}<option value={String(zone.number)}>{title}</option>{/if}
										{#each targets(device) as z (z.number)}<option value={String(z.number)}>{zoneName(z, hotWater)}</option>{/each}
									</select>
								</label>
								<button class="btn danger" disabled={busy} onclick={() => askRemove(device)}>
									<Icon name="trash" size={18} />{t('devices.remove')}
								</button>
							</div>
						</li>
					{/each}
				</ul>
			{/if}
			{#if zone}
				{#if candidates.length > 0}
					<div class="row fill" data-testid="join">
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
			{/if}
		</section>

		{#if zone?.kind === 'heating'}
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
									api.patchZone(zone.number, { circuit: v === '' ? null : Number(v) })
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
								choose(e.currentTarget, zone?.measuring_device ?? '', (v) => api.setRole(zone.number, 'measuring', v === '' ? null : v))}
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
								choose(e.currentTarget, zone?.reporter ?? '', (v) => api.setRole(zone.number, 'reporter', v === '' ? null : v))}
						>
							<option value="">{t('rooms.role.none')}</option>
							{#each members as device (device.eui)}<option value={device.eui}>{labelOf(device)}</option>{/each}
						</select>
						<span class="note">{t('rooms.reporter.hint')}</span>
					</label>
				</div>
			</section>
		{/if}

		{#if zone}
			<section class="panel" aria-labelledby="delete-title">
				<header><h2 id="delete-title">{t('rooms.delete')}</h2></header>
				<p class="note">{t('rooms.delete.note')}</p>
				<div class="row">
					<button class="btn danger" disabled={busy} onclick={() => confirmDelete?.show()}><Icon name="trash" size={18} />{t('rooms.delete')}</button>
				</div>
			</section>
		{/if}
	{/if}
</section>

<ConfirmDialog
	bind:this={confirmDelete}
	title={t('rooms.delete.title', { name: title })}
	body={members.length > 0 ? t('rooms.delete.body.members') : t('rooms.delete.body')}
	items={members.map(labelOf)}
	confirm={t('rooms.delete')}
	onconfirm={deleteRoom}
/>

<ConfirmDialog
	bind:this={confirmRemove}
	title={t('remove.title', { name: removing ? labelOf(removing) : '' })}
	body={removal?.forceable === false ? t('remove.blocked') : effects.length > 0 ? t('remove.body.effects') : t('remove.body')}
	items={effects}
	confirm={removal?.forceable === false ? undefined : t('devices.remove')}
	onconfirm={removeDevice}
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

	.member {
		display: grid;
		gap: 0.6rem;
		padding: 0.9rem 0;
		border-top: 1px solid var(--border);
	}

	.member:first-child {
		border-top: 0;
		padding-top: 0;
	}

	.id {
		display: grid;
		gap: 0.15rem;
		justify-items: start;
	}

	.rename {
		width: 100%;
		max-width: 28rem;
	}

	.rename .input {
		flex: 1 1 6rem;
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
