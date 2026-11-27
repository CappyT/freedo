<script lang="ts">
	import { onMount } from 'svelte';
	import { errorText } from './actions.ts';
	import { refusal } from './backup.ts';
	import ConfirmDialog from './ConfirmDialog.svelte';
	import { data } from './data.svelte.ts';
	import { t } from './i18n.svelte.ts';
	import { groupByFamily } from './interfacemodule.ts';
	import { api, setModule } from './session.ts';
	import { toasts } from './toast.svelte.ts';
	import type { InterfaceModule } from './types.ts';

	interface Props {
		/** The wall thermostat. */
		short: string;
	}

	let { short }: Props = $props();

	let view = $state<InterfaceModule | null>(null);
	let problem = $state<string | null>(null);
	let choice = $state('');
	let busy = $state(false);
	let confirmSet: ConfirmDialog | undefined = $state();

	const groups = $derived(groupByFamily(view?.known ?? []));
	const chosen = $derived(view?.known.find((k) => String(k.module) === choice));

	async function load() {
		try {
			view = await api.interfaceModule(short);
			problem = null;
		} catch (error) {
			problem = errorText(error);
		}
	}

	async function set() {
		if (!chosen) return;
		busy = true;
		try {
			view = await setModule(short, chosen.module);
			problem = null;
			choice = '';
			toasts.push('ok', t('devices.module.saved'));
			await data.refresh();
		} catch (error) {
			// A 403 is a Home Assistant user who is not an administrator.
			toasts.push('error', refusal(error) === 'admin' ? t('devices.module.adminOnly') : errorText(error));
		}
		busy = false;
	}

	onMount(load);
</script>

<div class="module">
	{#if problem}<p class="note tone-fault">{problem}</p>{/if}
	{#if view}
		<dl class="data">
			<div>
				<dt>{t('devices.module.title')}</dt>
				<dd class="num">{view.module === null ? '-' : `${view.module} · ${view.family}`}</dd>
			</div>
		</dl>
		{#if view.module === null}
			<form
				class="set"
				onsubmit={(e) => {
					e.preventDefault();
					confirmSet?.show();
				}}
			>
				<p class="note">{t('devices.module.unknown')}</p>
				<label class="field">
					<span>{t('devices.module.choose')}</span>
					<select class="input" bind:value={choice} data-testid="module-select">
						<option value="">{t('devices.module.pick')}</option>
						{#each groups as group (group.family)}
							<optgroup label={group.family}>
								{#each group.ids as id (id)}<option value={String(id)}>{id}</option>{/each}
							</optgroup>
						{/each}
					</select>
				</label>
				<div>
					<button class="btn primary" type="submit" disabled={busy || !chosen}>{t('devices.module.set')}</button>
				</div>
			</form>
		{/if}
	{/if}
</div>

<ConfirmDialog
	bind:this={confirmSet}
	title={t('devices.module.confirm.title')}
	body={t('devices.module.confirm.body')}
	items={chosen ? [`${chosen.module} · ${chosen.family}`] : []}
	confirm={t('devices.module.set')}
	onconfirm={set}
/>

<style>
	.module,
	.set {
		display: grid;
		gap: 0.5rem;
	}

	.note {
		margin: 0;
		color: var(--text-muted);
		font-size: var(--text-small-size);
	}

	.note.tone-fault {
		color: var(--status-fault-text);
	}

	.data {
		display: grid;
		gap: 0.3rem;
		margin: 0;
		font-size: var(--text-small-size);
	}

	.data div {
		display: flex;
		justify-content: space-between;
		gap: 1rem;
	}

	.data dt {
		color: var(--text-muted);
	}

	.data dd {
		margin: 0;
		text-align: right;
	}
</style>
