<script lang="ts">
	import { data } from './data.svelte.ts';
	import Icon from './Icon.svelte';
	import { t } from './i18n.svelte.ts';
	import { manage } from './install.svelte.ts';
	import { nextCircuitId } from './install.ts';
	import { api } from './session.ts';

	let name = $state('');
	let chosen = $state<string | null>(null);
	let bind = $state(true);
	let busy = $state(false);

	const thermostats = $derived((data.health?.devices ?? []).filter((d) => d.kind === 'thermostat'));
	const source = $derived(chosen ?? thermostats[0]?.short ?? '');
	const id = $derived(nextCircuitId(data.circuits.map((c) => c.ID)));
	const unbound = $derived((data.zones?.declared ?? []).filter((z) => z.kind === 'heating' && z.circuit === undefined));

	async function submit(event: SubmitEvent) {
		event.preventDefault();
		const label = name.trim();
		if (label === '' || source === '' || id === null) return;
		busy = true;
		const rooms = bind ? unbound.map((z) => z.number) : [];
		const done = await manage(async () => {
			await api.createCircuit({ id, name: label, heat_source: source });
			for (const number of rooms) await api.patchZone(number, { circuit: id });
			return true;
		}, t('circuits.created'));
		busy = false;
		if (done) name = '';
	}
</script>

{#if thermostats.length === 0}
	<p class="note">{t('circuits.needThermostat')}</p>
{:else}
	<form class="row fill" onsubmit={submit} data-testid="circuit-form">
		<label class="field grow">
			<span>{t('circuits.name')}</span>
			<input class="input" bind:value={name} placeholder={t('circuits.nameHint')} autocomplete="off" />
		</label>
		<label class="field">
			<span>{t('circuits.heatSource')}</span>
			<select class="input" value={source} onchange={(e) => (chosen = e.currentTarget.value)}>
				{#each thermostats as d (d.short)}<option value={d.short}>{d.label || d.device}</option>{/each}
			</select>
		</label>
		<button class="btn primary" type="submit" disabled={busy || name.trim() === '' || id === null}>
			<Icon name="plus" size={18} />{t('circuits.add')}
		</button>
	</form>
	{#if unbound.length > 0}
		<label class="check small">
			<input type="checkbox" bind:checked={bind} />
			{t('circuits.bindRooms', { n: unbound.length })}
		</label>
	{/if}
{/if}

<style>
	.grow {
		flex: 1 1 12rem;
	}

	.check {
		display: inline-flex;
		align-items: center;
		gap: 0.5rem;
		min-height: 2.75rem;
		cursor: pointer;
	}

	.check input {
		width: 1.1rem;
		height: 1.1rem;
		accent-color: var(--action);
	}
</style>
