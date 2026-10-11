<script lang="ts">
	import { act } from './actions.ts';
	import { alertKeys } from './describe.ts';
	import type { DeviceRow } from './devices.ts';
	import { fmt, t } from './i18n.svelte.ts';
	import Icon from './Icon.svelte';
	import { api } from './session.ts';

	let { devices, detailed = false }: { devices: DeviceRow[]; detailed?: boolean } = $props();
</script>

{#if devices.length === 0}
	<p class="muted">{t('devices.empty')}</p>
{:else}
	<ul class="devices" class:detailed>
		{#each devices as device (device.short)}
			<li class="device">
				<div class="id">
					<strong>{device.label}</strong>
					<span class="small muted">
						{t(`kind.${device.kind}` as 'kind.valve')}
						<span class="mono">{device.serial}</span>
					</span>
					{#if detailed && device.zones.length > 0}
						<span class="small muted">{t('devices.zone')}: {device.zones.map((z) => z.name).join(', ')}</span>
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
					<div class="actions row">
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
		align-items: center;
		gap: 0.5rem;
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

	.switch input {
		width: 1.15rem;
		height: 1.15rem;
		accent-color: var(--action);
	}

	@media (max-width: 640px) {
		.device,
		.detailed .device {
			grid-template-columns: 1fr 1fr;
		}

		.id {
			grid-column: 1 / -1;
		}
	}
</style>
