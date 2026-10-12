<script lang="ts">
	import { act } from './actions.ts';
	import { alertKeys } from './describe.ts';
	import type { DeviceRow } from './devices.ts';
	import { fmt, t } from './i18n.svelte.ts';
	import Icon from './Icon.svelte';
	import { api } from './session.ts';
	import { zoneName } from './zones.ts';

	let { devices, detailed = false }: { devices: DeviceRow[]; detailed?: boolean } = $props();

	// The shared address of the row whose label is being changed, null when no row is.
	let editing = $state<string | null>(null);
	let draft = $state('');
	let busy = $state(false);

	function start(device: DeviceRow) {
		editing = device.short;
		draft = device.label;
	}

	function cancel() {
		editing = null;
	}

	// A field that opens on a click gets no focus by itself, and the browser ignores autofocus.
	function focus(node: HTMLInputElement) {
		node.focus();
		node.select();
	}

	async function save(device: DeviceRow) {
		const label = draft.trim();
		if (label === '' || label === device.label) {
			editing = null;
			return;
		}
		busy = true;
		const done = await act(() => api.renameDevice(device.short, label), t('devices.renamed'));
		busy = false;
		if (done) editing = null;
	}
</script>

{#if devices.length === 0}
	<p class="muted">{t('devices.empty')}</p>
{:else}
	<ul class="devices" class:detailed>
		{#each devices as device (device.short)}
			<li class="device">
				<div class="id">
					{#if editing === device.short}
						<form
							class="rename row"
							onsubmit={(e) => {
								e.preventDefault();
								void save(device);
							}}
						>
							<input
								class="input"
								bind:value={draft}
								aria-label={t('devices.name')}
								use:focus
								onkeydown={(e) => {
									if (e.key === 'Escape') cancel();
								}}
							/>
							<button class="btn icon" type="submit" disabled={busy} aria-label={t('devices.save')}>
								<Icon name="check" size={18} />
							</button>
							<button class="btn icon" type="button" onclick={cancel} aria-label={t('action.cancel')}>
								<Icon name="close" size={18} />
							</button>
						</form>
					{:else}
						<strong>{device.label}</strong>
					{/if}
					<span class="small muted">
						{t(`kind.${device.kind}` as 'kind.valve')}
						<span class="mono">{device.serial}</span>
					</span>
					{#if detailed && device.zones.length > 0}
						<span class="small muted">{t('devices.zone')}: {device.zones.map((z) => zoneName(z, t('zone.hotWaterName'))).join(', ')}</span>
					{/if}
				</div>

				<div class="conn">
					{#if device.offline}
						<span class="badge tone-offline offline">{t('devices.offline')}</span>
					{:else}
						<span class="badge tone-idle">{t('devices.online')}</span>
					{/if}
					<span class="small muted">{t('devices.lastSeen')}: {fmt.ago(device.lastSeenUnix)}</span>
				</div>

				<div class="battery">
					<Icon name="battery" size={18} />
					{#if device.batteryMV === null}
						<span class="muted">-</span>
					{:else}
						<span class="mono">{fmt.percent(device.batteryPct)}</span>
						<span class="small muted mono">{device.batteryMV} mV</span>
					{/if}
				</div>

				{#if detailed}
					<div class="fw small">
						<span class="muted">{t('devices.firmware')}</span>
						<span class="mono">{device.firmware ?? '-'}</span>
					</div>
				{/if}

				<div class="flags">
					{#each device.alerts as alert (alert)}
						<span class="badge tone-warning">{t(alertKeys[alert])}</span>
					{/each}
				</div>

				{#if detailed}
					<div class="actions row fill">
						<button class="btn" onclick={() => start(device)}>
							<Icon name="edit" size={18} />{t('devices.rename')}
						</button>
						{#if device.canIdentify}
							<button class="btn" onclick={() => act(() => api.identify(device.short))}>
								<Icon name="bell" size={18} />{t('devices.identify')}
							</button>
						{/if}
						{#if device.childLock !== null}
							<label class="switch">
								<input
									type="checkbox"
									checked={device.childLock}
									onchange={async (e) => {
										const box = e.currentTarget;
										const done = await act(() => api.childLock(device.short, box.checked));
										// A checkbox keeps the click when the request fails: show what the daemon reports.
										if (!done) box.checked = device.childLock === true;
									}}
								/>
								<Icon name="lock" size={18} />{t('devices.childLock')}
							</label>
						{/if}
					</div>
				{/if}
			</li>
		{/each}
	</ul>
{/if}

<style>
	.devices {
		list-style: none;
		margin: 0;
		padding: 0;
		display: grid;
	}

	.device {
		display: grid;
		grid-template-columns: minmax(10rem, 1.4fr) minmax(10rem, 1fr) minmax(8rem, 0.8fr);
		gap: 0.5rem 1.25rem;
		align-items: center;
		padding: 0.9rem 0;
		border-top: 1px solid var(--border);
	}

	.detailed .device {
		grid-template-columns: minmax(10rem, 1.4fr) minmax(10rem, 1fr) minmax(7rem, 0.7fr) minmax(5rem, 0.5fr);
	}

	.device:first-child {
		border-top: 0;
	}

	.id,
	.conn,
	.fw {
		display: grid;
		gap: 0.15rem;
		justify-items: start;
	}

	.battery {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: 0.25rem 0.5rem;
	}

	.battery span {
		white-space: nowrap;
	}

	.flags {
		grid-column: 1 / -1;
		display: flex;
		flex-wrap: wrap;
		gap: 0.4rem;
	}

	.flags:empty {
		display: none;
	}

	.actions {
		grid-column: 1 / -1;
	}

	.switch {
		display: inline-flex;
		align-items: center;
		gap: 0.5rem;
		min-height: 2.75rem;
		font-size: var(--text-small-size);
		font-weight: var(--weight-medium);
		cursor: pointer;
	}

	/* A checkbox drawn as a switch. Off is an outline, so the control keeps 3:1 on its ground. */
	.switch input {
		appearance: none;
		position: relative;
		flex: none;
		width: 2.75rem;
		height: 1.5rem;
		margin: 0;
		border: 2px solid var(--text-muted);
		border-radius: var(--radius-pill);
		background: var(--surface);
		cursor: pointer;
	}

	.switch input::before {
		content: '';
		position: absolute;
		top: 2px;
		left: 2px;
		width: 1rem;
		height: 1rem;
		border-radius: 50%;
		background: var(--text-muted);
		transition: translate 0.15s ease-out;
	}

	.switch input:checked {
		border-color: var(--action);
		background: var(--action);
	}

	.switch input:checked::before {
		translate: 1.25rem 0;
		background: var(--action-text);
	}

	.rename {
		width: 100%;
	}

	.rename .input {
		flex: 1 1 6rem;
	}

	@media (max-width: 640px) {
		.device,
		.detailed .device {
			grid-template-columns: 1fr 1fr;
		}

		.id {
			grid-column: 1 / -1;
		}

		.fw {
			display: flex;
			gap: 0.4rem;
		}
	}
</style>
