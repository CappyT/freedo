<script lang="ts">
	import { onDestroy } from 'svelte';
	import { actMany, errorText } from './actions.ts';
	import { deltaValue, isTicks, lastRead, refusal, statValue, TICK_HZ } from './dbgstats.ts';
	import { fmt, t } from './i18n.svelte.ts';
	import Icon from './Icon.svelte';
	import { api } from './session.ts';
	import type { DebugStat, DebugStats } from './types.ts';

	interface Props {
		/** The valve. */
		short: string;
	}

	let { short }: Props = $props();

	const POLL_MS = 3000;
	// The valve takes a queued message when it next wakes up. Past this the section stops asking; a new read asks again.
	const POLL_LIMIT_MS = 10 * 60_000;

	let states = $state<DebugStats | null>(null);
	let counters = $state<DebugStats | null>(null);
	let problem = $state<string | null>(null);
	let polling = $state(false);
	let loaded = false;
	let destroyed = false;
	let timer: ReturnType<typeof setTimeout> | undefined;
	let pollUntil = 0;

	const waiting = $derived(states?.pending === true || counters?.pending === true);
	const when = $derived(lastRead([states, counters]));
	const refusals = $derived(
		[states, counters].flatMap((view) => {
			const code = refusal(view);
			return view && code ? [{ f: view.f, code }] : [];
		})
	);
	const decodeErrors = $derived([states?.decode_error, counters?.decode_error].filter((e) => e !== undefined));
	const all = $derived([...(states?.stats ?? []), ...(counters?.stats ?? [])]);

	/** The GET reads what the daemon holds: it sends nothing to the valve. */
	async function load() {
		try {
			[states, counters] = await Promise.all([api.debugStats(short, 10), api.debugStats(short, 11)]);
			problem = null;
		} catch (error) {
			problem = errorText(error);
		}
	}

	/** Asks the daemon again while a read is in flight, for POLL_LIMIT_MS from now. */
	function watch() {
		pollUntil = Date.now() + POLL_LIMIT_MS;
		schedule();
	}

	// One timer at a time: the next one starts after the answer, so a slow answer never overlaps the next request.
	function schedule() {
		if (destroyed || timer !== undefined) return;
		if (!waiting || Date.now() >= pollUntil) {
			polling = false;
			return;
		}
		polling = true;
		timer = setTimeout(async () => {
			if (!document.hidden) await load();
			timer = undefined;
			schedule();
		}, POLL_MS);
	}

	function stop() {
		destroyed = true;
		clearTimeout(timer);
		timer = undefined;
	}

	async function read() {
		await actMany([() => api.readDebugStats(short, 10), () => api.readDebugStats(short, 11)]);
		await load();
		watch();
	}

	async function opened(open: boolean) {
		if (!open || loaded) return;
		loaded = true;
		await load();
		watch();
	}

	const count = (name: string) => fmt.number(deltaValue(counters, name), 0);
	const share = (pct: number) => `${fmt.number(pct, 2)} %`;

	function value(stat: DebugStat): string {
		if (isTicks(stat)) return `${fmt.number(stat.value / TICK_HZ, 1)} s`;
		if (stat.name === 'rssi_dBm') return `${stat.value} dBm`;
		return fmt.number(stat.value, 0);
	}

	onDestroy(stop);
</script>

<details class="stats table-twin" ontoggle={(e) => opened(e.currentTarget.open)}>
	<summary>{t('devices.stats.title')}</summary>
	<div class="body">
		<p class="note">{when === null ? t('devices.stats.never') : t('devices.stats.readAt', { when: fmt.ago(when) })}</p>
		{#if problem}<p class="note tone-fault">{problem}</p>{/if}
		{#each refusals as { f, code } (f)}<p class="note tone-fault">{t('devices.stats.answer', { f, code })}</p>{/each}
		{#each decodeErrors as error (error)}<p class="note tone-fault">{error}</p>{/each}

		{#if states?.share || counters?.stats}
			<dl class="data">
				{#if states?.share}
					<div><dt>{t('devices.stats.awake')}</dt><dd class="num">{share(states.share.awake_pct)}</dd></div>
					<div><dt>{t('devices.stats.tx')}</dt><dd class="num">{share(states.share.tx_pct)}</dd></div>
					<div><dt>{t('devices.stats.rx')}</dt><dd class="num">{share(states.share.rx_pct)}</dd></div>
				{/if}
				{#if counters?.delta}
					<div class="span"><dt>{t('devices.stats.since', { duration: fmt.duration(counters.delta.interval_s) })}</dt></div>
					<div><dt>{t('devices.stats.crc')}</dt><dd class="num">{count('rx_crc_fail')}</dd></div>
					<div><dt>{t('devices.stats.noAck')}</dt><dd class="num">{count('tx_no_ack')}</dd></div>
					<div><dt>{t('devices.stats.mic')}</dt><dd class="num">{count('mic_fail')}</dd></div>
				{/if}
				{#if counters?.stats}
					<div>
						<dt>{t('devices.stats.rssi')}</dt>
						<dd class="num">{statValue(counters, 'rssi_dBm') === null ? '-' : `${statValue(counters, 'rssi_dBm')} dBm`}</dd>
					</div>
					<div><dt>{t('devices.stats.lqi')}</dt><dd class="num">{fmt.number(statValue(counters, 'lqi'), 0)}</dd></div>
				{/if}
			</dl>
		{/if}
		{#if states?.stats && !states.share}<p class="note">{t('devices.stats.shareNote')}</p>{/if}
		{#if counters?.stats && !counters.delta}<p class="note">{t('devices.stats.countersNote')}</p>{/if}

		{#if all.length > 0}
			<details class="table-twin">
				<summary>{t('devices.stats.all')}</summary>
				<p class="note">{t('devices.stats.rawNote')}</p>
				<div class="scroll">
					<table>
						<thead><tr><th scope="col">{t('devices.stats.name')}</th><th scope="col">{t('devices.stats.value')}</th></tr></thead>
						<tbody>
							{#each all as stat (stat.tag)}
								<tr><th scope="row" title={stat.tag}>{stat.name ?? stat.tag}</th><td class="num">{value(stat)}</td></tr>
							{/each}
						</tbody>
					</table>
				</div>
			</details>
		{/if}

		<div>
			<button class="btn" disabled={polling} onclick={read}>
				<Icon name="refresh" size={18} />{polling ? t('devices.stats.waiting') : t('devices.stats.read')}
			</button>
		</div>
	</div>
</details>

<style>
	.body {
		display: grid;
		gap: 0.5rem;
		margin-top: 0.5rem;
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

	.data .span dt {
		color: var(--text);
		font-weight: var(--weight-semibold);
	}
</style>
