<script lang="ts" module>
	import { SvelteSet } from 'svelte/reactivity';

	// The rows the person opened stay open across data polls and pages.
	const opened = new SvelteSet<string>();
	let reveal: string | null = null;

	/** Opens the row of a device on the page Rooms and devices, which scrolls to it. */
	export function showDevice(short: string) {
		opened.add(short);
		reveal = short;
	}
</script>

<script lang="ts">
	import { resolve } from '$app/paths';
	import { onMount } from 'svelte';
	import { act } from './actions.ts';
	import { alertKeys, conditionText } from './describe.ts';
	import type { DeviceRow } from './devices.ts';
	import { fmt, t } from './i18n.svelte.ts';
	import Icon, { type IconName } from './Icon.svelte';
	import { api } from './session.ts';
	import { zoneName } from './zones.ts';

	interface Props {
		devices: DeviceRow[];
		/** The room of the list. A device that is also in another room names that room. */
		room?: number;
		/** One line per device that leads to its row on the page Rooms and devices: the list of a room screen. */
		compact?: boolean;
		/** The address of the room settings, shown as the last line of a compact list. */
		settings?: string;
	}

	let { devices, room, compact = false, settings }: Props = $props();

	const kindIcon = (kind: string): IconName => (kind === 'thermostat' ? 'thermostat' : kind === 'valve' ? 'valve' : 'devices');

	type Tone = 'idle' | 'offline' | 'warning' | 'fault';

	/** The one state a closed row shows: offline first, then a fault, then a warning. */
	function status(device: DeviceRow): { tone: Tone; text: string } {
		if (device.offline) return { tone: 'offline', text: t('devices.offline') };
		if (device.faults.length > 0) return { tone: 'fault', text: conditionText(device.faults[0]!) };
		const alert = device.alerts.find((a) => a !== 'deviceOffline');
		if (alert) return { tone: alert === 'deviceFault' ? 'fault' : 'warning', text: t(alertKeys[alert]) };
		if (device.warnings.length > 0) return { tone: 'warning', text: conditionText(device.warnings[0]!) };
		const others = device.zones.filter((z) => z.number !== room);
		if (room !== undefined && others.length > 0) {
			return { tone: 'idle', text: t('devices.alsoIn', { rooms: others.map((z) => zoneName(z, t('zone.hotWaterName'))).join(', ') }) };
		}
		return { tone: 'idle', text: t('devices.online') };
	}

	/** The problems an open line lists under the status line, without the one that line shows. */
	function issues(device: DeviceRow): { tone: Tone; text: string }[] {
		const all = [
			...device.faults.map((f) => ({ tone: 'fault' as Tone, text: conditionText(f) })),
			...device.warnings.map((w) => ({ tone: 'warning' as Tone, text: conditionText(w) })),
			// The status line names an unreachable device already.
			...device.alerts.filter((a) => a !== 'deviceOffline').map((a) => ({ tone: 'warning' as Tone, text: t(alertKeys[a]) }))
		];
		const shown = status(device).text;
		return all.filter((issue, i) => issue.text !== shown && all.findIndex((other) => other.text === issue.text) === i);
	}

	function battery(device: DeviceRow): string {
		const volts = device.batteryMV === null ? null : `${fmt.number(device.batteryMV / 1000, 2)} V`;
		if (device.batteryPct === null) return volts ?? '-';
		return volts === null ? fmt.pct(device.batteryPct) : `${fmt.pct(device.batteryPct)} · ${volts}`;
	}

	function toggle(short: string) {
		if (opened.has(short)) opened.delete(short);
		else opened.add(short);
	}

	const settingsOf = (device: DeviceRow) =>
		resolve('/rooms/[n]', { n: String(room ?? device.zones[0]?.number ?? 'none') });

	onMount(() => {
		const short = reveal;
		if (compact || short === null || !devices.some((d) => d.short === short)) return;
		reveal = null;
		document.getElementById(`device-${short}`)?.scrollIntoView({ block: 'center' });
	});
</script>

{#snippet state(device: DeviceRow)}
	{@const s = status(device)}
	<span class="status tone-{s.tone}">
		{#if s.tone === 'warning' || s.tone === 'fault'}
			<Icon name={s.tone === 'fault' ? 'fault' : 'warning'} size={14} />
		{:else}
			<span class="dot" class:hollow={s.tone === 'offline'}></span>
		{/if}
		{s.text}
	</span>
{/snippet}

{#if compact}
	<ul class="list">
		{#each devices as device (device.short)}
			{@const s = status(device)}
			<li>
				<a class="list-row" href={resolve('/devices')} onclick={() => showDevice(device.short)}>
					<span class="disc"><Icon name={kindIcon(device.kind)} size={18} /></span>
					<span class="list-main">
						<span class="list-label">{device.label}</span>
						<span class="list-sub">
							{t(`kind.${device.kind}` as 'kind.valve')}{#if device.batteryPct !== null}&nbsp;· {t('devices.battery')}
								<span class="num">{fmt.pct(device.batteryPct)}</span>{/if}
						</span>
					</span>
					<span class="status tone-{s.tone}" title={s.text}>
						{#if s.tone === 'warning' || s.tone === 'fault'}
							<Icon name={s.tone === 'fault' ? 'fault' : 'warning'} size={16} />
						{:else}
							<span class="dot" class:hollow={s.tone === 'offline'}></span>
						{/if}
						<span class="sr-only">{s.text}</span>
					</span>
					<span class="chevron"><Icon name="chevron" size={18} /></span>
				</a>
			</li>
		{:else}
			<li class="none">{t('rooms.empty')}</li>
		{/each}
		{#if settings}
			<li>
				<a class="list-row" href={settings}>
					<span class="disc"><Icon name="sliders" size={18} /></span>
					<span class="list-main"><span class="list-label">{t('rooms.settings')}</span></span>
					<span class="chevron"><Icon name="chevron" size={18} /></span>
				</a>
			</li>
		{/if}
	</ul>
{:else if devices.length === 0}
	<p class="list none">{t('devices.empty')}</p>
{:else}
	<ul class="list devices">
		{#each devices as device (device.short)}
			{@const open = opened.has(device.short)}
			<li class="device" id="device-{device.short}">
				<button class="list-row head" aria-expanded={open} aria-controls="device-{device.short}-more" onclick={() => toggle(device.short)}>
					<span class="disc"><Icon name={kindIcon(device.kind)} size={18} /></span>
					<span class="list-main">
						<strong class="list-label">{device.label}</strong>
						<span class="list-sub">{@render state(device)} · <span class="num">{device.serial}</span></span>
					</span>
					<span class="chevron" class:open><Icon name="chevron" size={18} /></span>
				</button>

				{#if open}
					{@const more = issues(device)}
					<div class="more" id="device-{device.short}-more">
						{#if more.length > 0}
							<div class="flags">
								{#each more as issue (issue.text)}
									<span class="badge tone-{issue.tone}">{issue.text}</span>
								{/each}
							</div>
						{/if}

						<dl class="data">
							<div><dt>{t('rooms.kind')}</dt><dd>{t(`kind.${device.kind}` as 'kind.valve')}</dd></div>
							<div><dt>{t('devices.firmware')}</dt><dd class="num">{device.firmware ?? '-'}</dd></div>
							<div><dt>{t('devices.lastSeen')}</dt><dd>{fmt.ago(device.lastSeenUnix)}</dd></div>
							<div><dt>{t('devices.battery')}</dt><dd class="num">{battery(device)}</dd></div>
						</dl>

						{#if device.childLock !== null}
							<label class="switch">
								<span>{t('devices.childLock')}</span>
								<input
									type="checkbox"
									role="switch"
									checked={device.childLock}
									onchange={async (e) => {
										const box = e.currentTarget;
										const done = await act(() => api.childLock(device.short, box.checked));
										// A checkbox keeps the click when the request fails: show what the daemon reports.
										if (!done) box.checked = device.childLock === true;
									}}
								/>
							</label>
						{/if}

						<div class="actions">
							{#if device.canIdentify}
								<button class="btn" onclick={() => act(() => api.identify(device.short))}>
									<Icon name="bell" size={18} />{t('devices.identify')}
								</button>
							{/if}
							<a class="btn" href={settingsOf(device)}><Icon name="sliders" size={18} />{t('rooms.settings')}</a>
						</div>
					</div>
				{/if}
			</li>
		{/each}
	</ul>
{/if}

<style>
	.none {
		padding: 0.875rem;
		color: var(--text-muted);
		font-size: var(--text-small-size);
	}

	.status {
		display: inline-flex;
		align-items: center;
		gap: 0.3rem;
		font-weight: var(--weight-semibold);
	}

	.status.tone-idle {
		color: var(--status-idle-text);
	}

	.status.tone-offline {
		color: var(--status-offline-text);
	}

	.status.tone-warning {
		color: var(--status-warning-text);
	}

	.status.tone-fault {
		color: var(--status-fault-text);
	}

	/* The tone class sets the fill colour as the dot, too pale on a white row: the dot takes the text colour. */
	.status .dot {
		--dot: currentColor;
	}

	.status .dot.hollow {
		background: transparent;
		border: 1px solid var(--status-offline);
	}

	.chevron {
		transition: rotate 0.2s cubic-bezier(0.2, 0.8, 0.2, 1);
	}

	.chevron.open {
		rotate: 90deg;
	}

	/* The detail of an open row, aligned with the name. */
	.more {
		display: grid;
		gap: 0.75rem;
		padding: 0 0.875rem 0.875rem 3.875rem;
	}

	.flags {
		display: flex;
		flex-wrap: wrap;
		gap: 0.375rem;
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
		overflow-wrap: anywhere;
	}

	.actions {
		display: flex;
		flex-wrap: wrap;
		gap: 0.5rem;
	}

	.switch {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 1rem;
		min-height: 2.75rem;
		font-size: var(--text-small-size);
		font-weight: var(--weight-semibold);
		cursor: pointer;
	}

	/* A checkbox drawn as a switch. Off is an outline, so the control keeps 3:1 on its ground. */
	.switch input {
		appearance: none;
		position: relative;
		flex: none;
		width: 2.875rem;
		height: 1.75rem;
		margin: 0;
		border: 2px solid var(--text-muted);
		border-radius: var(--radius-pill);
		background: var(--surface);
		cursor: pointer;
		transition: background 0.2s;
	}

	.switch input::before {
		content: '';
		position: absolute;
		top: 2px;
		left: 2px;
		width: 1.25rem;
		height: 1.25rem;
		border-radius: 50%;
		background: var(--text-muted);
		transition: translate 0.2s cubic-bezier(0.2, 0.8, 0.2, 1);
	}

	.switch input:checked {
		border-color: var(--action);
		background: var(--action);
	}

	.switch input:checked::before {
		translate: 1.125rem 0;
		background: var(--action-text);
	}

	@media (max-width: 559px) {
		.more {
			padding-left: 0.875rem;
		}
	}

	@media (prefers-reduced-motion: reduce) {
		.chevron,
		.switch input,
		.switch input::before {
			transition: none;
		}
	}
</style>
