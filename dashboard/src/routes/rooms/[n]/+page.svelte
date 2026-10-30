<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { resolve } from '$app/paths';
	import { tick } from 'svelte';
	import { act, errorText } from '#lib/actions.ts';
	import ConfirmDialog from '#lib/ConfirmDialog.svelte';
	import { data } from '#lib/data.svelte.ts';
	import { fmt, t } from '#lib/i18n.svelte.ts';
	import Icon from '#lib/Icon.svelte';
	import { manage } from '#lib/install.svelte.ts';
	import { canJoin, describeEffect, movePlan, moveTargets } from '#lib/install.ts';
	import {
		AWAY_C,
		END_MINUTES,
		WINDOW_MINUTES,
		optionIssues,
		optionsOf,
		optionsPatch,
		parseOffset,
		type RoomOptions
	} from '#lib/roomoptions.ts';
	import { COORDINATOR_SOURCE, electedDevice, readingUnix, sourceNote, sourceValue, sourceWrites } from '#lib/roomtemp.ts';
	import { api } from '#lib/session.ts';
	import { toasts } from '#lib/toast.svelte.ts';
	import type { InstallCircuit, InstallDevice, InstallInfo, InstallZone, Removal } from '#lib/types.ts';
	import { zoneName } from '#lib/zones.ts';

	// The route "none" is the group of the devices in no room. It has members and no settings of a zone.
	const number = $derived(page.params.n === 'none' ? null : Number(page.params.n));

	let zones = $state<InstallZone[]>([]);
	let devices = $state<InstallDevice[]>([]);
	let circuits = $state<InstallCircuit[]>([]);
	let info = $state<InstallInfo | null>(null);
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
	let restarting = $state<InstallDevice | null>(null);
	// The options in the form. They follow the room until the person changes one.
	let options = $state<RoomOptions | null>(null);
	let offsetText = $state('');
	let optionsDirty = $state(false);
	let confirmDelete: ConfirmDialog | undefined = $state();
	let confirmRemove: ConfirmDialog | undefined = $state();
	let confirmRestart: ConfirmDialog | undefined = $state();

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
			[zones, devices, circuits, info] = await Promise.all([api.installZones(), api.installDevices(), api.installCircuits(), api.installInfo()]);
			problem = null;
			if (!typed) name = zone?.name ?? '';
			if (!optionsDirty) showOptions();
		} catch (error) {
			problem = errorText(error);
		}
		loaded = true;
	}

	const issues = $derived(options ? optionIssues(options, offsetText) : []);

	// The room temperature source: a device of the room, or the thermometer of Home Assistant.
	const external = $derived(data.zones?.declared.find((z) => z.number === number)?.externalTemp);
	const selected = $derived(zone ? sourceValue(zone) : '');
	const elected = $derived(zone ? electedDevice(zone) : null);
	const electedName = $derived.by(() => {
		const device = members.find((d) => d.eui === elected);
		return device ? labelOf(device) : null;
	});
	const note = $derived(zone ? sourceNote(zone, external) : 'device');
	const maxAgeMinutes = $derived(Math.round((info?.room_temp_max_age_sec ?? 1800) / 60));
	const readingAge = $derived(external?.ageSec == null ? '' : fmt.ago(readingUnix(external.ageSec)));
	// Home Assistant pushes a room temperature that a device-owned zone drops. The warning stays until the person picks the thermometer.
	const refused = $derived(zone?.owner === 'device' ? external?.refused : null);
	const thermometer = $derived(
		[external?.source ?? external?.refused?.source ?? t('roomSource.ha'), ...(external?.tempC != null && external.ageSec != null ? [fmt.temp(external.tempC), readingAge] : [])].join(' · ')
	);
	const noteText = $derived.by(() => {
		const age = { temp: fmt.temp(external?.tempC), age: readingAge, minutes: maxAgeMinutes };
		if (note === 'fresh') return electedName ? t('roomSource.note.fresh', { ...age, device: electedName }) : t('roomSource.note.fresh.any', age);
		if (note === 'stale') return electedName ? t('roomSource.note.stale', { ...age, device: electedName }) : t('roomSource.note.stale.any', age);
		if (note === 'no_reading') return t('roomSource.note.none');
		return t(note === 'choose' ? 'roomSource.note.choose' : 'roomSource.note.device');
	});

	/** The text of a device option: name, kind and the temperature that the node reports. */
	function deviceOption(device: InstallDevice): string {
		const temp = data.nodes.find((n) => n.Short === device.short)?.TempC;
		return [labelOf(device), t(`kind.${device.kind}`), ...(temp ? [fmt.temp(temp)] : [])].join(' · ');
	}

	/** The select writes at once. The measuring device goes first, so the owner device finds it elected. */
	async function chooseSource(select: HTMLSelectElement) {
		if (!zone) return;
		const room = zone.number;
		const writes = sourceWrites(zone, select.value);
		await choose(select, selected, async () => {
			if (writes.measuring) await api.setRole(room, 'measuring', writes.measuring);
			if (writes.owner) await api.patchZone(room, { owner: writes.owner });
		});
	}

	function showOptions() {
		options = zone?.kind === 'heating' ? optionsOf(zone) : null;
		// No report carries the offset: the valve holds it.
		offsetText = '';
	}

	async function saveOptions(event: SubmitEvent) {
		event.preventDefault();
		if (!zone || !options || issues.length > 0) return;
		const room = zone.number;
		const patch = optionsPatch(zone, options);
		const typedOffset = parseOffset(offsetText);
		const offset = typedOffset === undefined ? null : typedOffset;
		if (Object.keys(patch).length === 0 && offset === null) {
			optionsDirty = false;
			return;
		}
		const done = await run(async () => {
			// The options go first: the offset applies in the mode they leave.
			if (Object.keys(patch).length > 0) await api.patchZone(room, patch);
			if (offset !== null) await api.setOffset(room, offset);
		}, t('rooms.saved'));
		if (done) {
			optionsDirty = false;
			showOptions();
		}
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

	async function askRestart(device: InstallDevice) {
		restarting = device;
		await tick();
		confirmRestart?.show();
	}

	async function restart() {
		const device = restarting;
		if (!device) return;
		busy = true;
		// The answer says that the command waits for the device, not that the device restarted.
		await act(() => api.reboot(device.eui));
		busy = false;
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
								{#if device.kind !== 'bridge'}
									<button class="btn" disabled={busy} onclick={() => askRestart(device)}>
										<Icon name="power" size={18} />{t('devices.restart')}
									</button>
								{/if}
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

		{#if zone?.kind === 'heating' && options}
			<section class="panel" aria-labelledby="options-title">
				<header><h2 id="options-title">{t('opt.title')}</h2></header>
				<form class="options" onsubmit={saveOptions} oninput={() => (optionsDirty = true)} onchange={() => (optionsDirty = true)} data-testid="room-options">
					<fieldset>
						<legend>{t('window.title')}</legend>
						<label class="check"><input type="checkbox" bind:checked={options.windowDetection} />{t('opt.windowDetection')}</label>
						<label class="field">
							<span>{t('opt.windowTime')}</span>
							<input class="input num" type="number" inputmode="numeric" min={WINDOW_MINUTES.min} max={WINDOW_MINUTES.max} step="1" bind:value={options.windowMinutes} disabled={!options.windowDetection} aria-invalid={issues.includes('window')} />
						</label>
						<p class="note">{t('opt.window.hint')}</p>
					</fieldset>

					<fieldset>
						<legend>{t('opt.away')}</legend>
						<label class="field">
							<span>{t('opt.awayC')}</span>
							<input class="input num" type="number" inputmode="decimal" min={AWAY_C.min} max={AWAY_C.max} step="0.1" bind:value={options.awayC} aria-invalid={issues.includes('away')} />
						</label>
						<p class="note">{t('opt.away.hint')}</p>
					</fieldset>

					<fieldset>
						<legend>{t('opt.endRule')}</legend>
						<label class="field">
							<span>{t('ends.legend')}</span>
							<select class="input" bind:value={options.endRule}>
								<option value="until_next">{t('ends.until_next')}</option>
								<option value="timer">{t('ends.timer')}</option>
								<option value="manual">{t('ends.manual')}</option>
							</select>
						</label>
						{#if options.endRule === 'timer'}
							<label class="field">
								<span>{t('opt.endTimer')}</span>
								<input class="input num" type="number" inputmode="numeric" min={END_MINUTES.min} max={END_MINUTES.max} step="1" bind:value={options.endMinutes} aria-invalid={issues.includes('endTimer')} />
							</label>
						{/if}
						<p class="note">{t('opt.endRule.hint')}</p>
					</fieldset>

					<fieldset>
						<legend>{t('opt.earlyStart')}</legend>
						<label class="check"><input type="checkbox" bind:checked={options.earlyStart} />{t('opt.earlyStart')}</label>
						<p class="note">{t('opt.earlyStart.hint')}</p>
					</fieldset>

					<fieldset class="wide">
						<legend>{t('roomTemp.label')}</legend>
						{#if refused}
							<p class="notice tone-warning" data-testid="source-refused">
								<Icon name="warning" />
								<span>{t('roomTemp.refused', { source: refused.source ?? t('roomTemp.source') })}</span>
							</p>
						{/if}
						<label class="field">
							<span>{t('roomSource.field')}</span>
							<!-- The select writes at once, so its events stay out of the form, which saves the options. -->
							<select
								class="input"
								value={selected}
								disabled={busy}
								onchange={(e) => {
									e.stopPropagation();
									void chooseSource(e.currentTarget);
								}}
								oninput={(e) => e.stopPropagation()}
								data-testid="source"
							>
								<optgroup label={t('zone.devices')}>
									{#each members as device (device.eui)}<option value={device.eui}>{deviceOption(device)}</option>{/each}
								</optgroup>
								<optgroup label={t('roomTemp.source')}>
									<option value={COORDINATOR_SOURCE}>{thermometer}</option>
								</optgroup>
							</select>
						</label>
						<p class={note === 'no_reading' ? 'notice tone-warning' : 'note'} data-testid="source-note">
							{#if note === 'no_reading'}<Icon name="warning" />{/if}
							<span>{noteText}</span>
						</p>
						<details>
							<summary>{t('roomSource.advanced')}</summary>
							<div class="pair">
								<label class="field">
									<span>{t('opt.offset')}</span>
									<input class="input num" inputmode="decimal" autocomplete="off" bind:value={offsetText} aria-invalid={issues.includes('offset')} />
								</label>
							</div>
							<p class="note">{t('opt.offset.hint')}</p>
							<div class="pair">
								<label class="field">
									<span>{t('opt.offsetMode')}</span>
									<select class="input" bind:value={options.offsetMode}>
										<option value="config">{t('opt.mode.config')}</option>
										<option value="dbg2">{t('opt.mode.dbg2')}</option>
									</select>
								</label>
							</div>
							<p class="note" data-testid="mode-hint">{t(`opt.mode.${options.offsetMode === 'dbg2' ? 'dbg2' : 'config'}.hint`)}</p>
						</details>
					</fieldset>

					{#if issues.length > 0}
						<ul class="issues small" role="alert">
							{#each issues as issue (issue)}<li>{t(`opt.issue.${issue}`)}</li>{/each}
						</ul>
					{/if}
					<div class="row">
						<button class="btn primary" type="submit" disabled={busy || !optionsDirty || issues.length > 0}>
							<Icon name="check" size={18} />{t('opt.save')}
						</button>
					</div>
				</form>
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
	bind:this={confirmRestart}
	title={t('restart.title', { name: restarting ? labelOf(restarting) : '' })}
	body={t('restart.body')}
	confirm={t('devices.restart')}
	onconfirm={restart}
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

	.options {
		display: grid;
		grid-template-columns: repeat(auto-fit, minmax(min(100%, 18rem), 1fr));
		gap: 1.25rem 1.5rem;
		align-items: start;
	}

	.options fieldset {
		margin: 0;
		padding: 0;
		border: 0;
		min-width: 0;
		display: grid;
		gap: 0.6rem;
		justify-items: start;
	}

	.options legend {
		padding: 0;
		margin-bottom: 0.5rem;
		font-family: var(--font-display);
		font-weight: var(--weight-semibold);
	}

	.options .wide,
	.options .issues,
	.options > .row {
		grid-column: 1 / -1;
	}

	.options .pair {
		display: flex;
		flex-wrap: wrap;
		align-items: flex-end;
		gap: 0.6rem 1.5rem;
	}

	.options select {
		max-width: 100%;
	}

	.options details > :not(summary) {
		margin-top: 0.6rem;
	}

	.num {
		width: 8rem;
	}

	.issues {
		margin: 0;
		padding-left: 1.25rem;
		color: var(--status-fault-text);
	}
</style>
