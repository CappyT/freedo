<script lang="ts">
	import { data } from './data.svelte.ts';
	import Icon from './Icon.svelte';
	import { t } from './i18n.svelte.ts';
	import { manage } from './install.svelte.ts';
	import { nextZoneNumber } from './install.ts';
	import { install } from './session.ts';
	import type { ZoneKind } from './types.ts';

	let name = $state('');
	let kind = $state<ZoneKind>('heating');
	// The circuit the person chose. Until then the form offers the first circuit.
	let chosen = $state<string | null>(null);
	let busy = $state(false);

	const zones = $derived(data.zones?.declared ?? []);
	const hasHotWater = $derived(zones.some((z) => z.kind === 'hotwater'));
	const circuit = $derived(chosen ?? (data.circuits[0] ? String(data.circuits[0].ID) : ''));
	const number = $derived(nextZoneNumber(zones.map((z) => z.number), kind));

	async function submit(event: SubmitEvent) {
		event.preventDefault();
		const label = name.trim();
		if (label === '' || number === null) return;
		busy = true;
		const done = await manage(
			() =>
				install.createZone({
					number,
					name: label,
					kind,
					...(kind === 'heating' && circuit !== '' ? { circuit: Number(circuit) } : {})
				}),
			t('rooms.created')
		);
		busy = false;
		if (done) {
			name = '';
			kind = 'heating';
		}
	}
</script>

<form class="row fill" onsubmit={submit} data-testid="room-form">
	<label class="field grow">
		<span>{t('rooms.name')}</span>
		<input class="input" bind:value={name} placeholder={t('rooms.nameHint')} autocomplete="off" />
	</label>
	<label class="field">
		<span>{t('rooms.kind')}</span>
		<select class="input" bind:value={kind}>
			<option value="heating">{t('rooms.kind.heating')}</option>
			{#if !hasHotWater}<option value="hotwater">{t('rooms.kind.hotwater')}</option>{/if}
		</select>
	</label>
	{#if kind === 'heating' && data.circuits.length > 0}
		<label class="field">
			<span>{t('rooms.circuit')}</span>
			<select class="input" value={circuit} onchange={(e) => (chosen = e.currentTarget.value)}>
				<option value="">{t('rooms.circuit.none')}</option>
				{#each data.circuits as c (c.ID)}<option value={String(c.ID)}>{c.Name || t('boiler.circuit', { id: c.ID })}</option>{/each}
			</select>
		</label>
	{/if}
	<button class="btn primary" type="submit" disabled={busy || name.trim() === '' || number === null}>
		<Icon name="plus" size={18} />{t('rooms.add')}
	</button>
</form>
{#if number === null}<p class="note">{t('rooms.full')}</p>{/if}

<style>
	.grow {
		flex: 1 1 12rem;
	}
</style>
