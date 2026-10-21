<script lang="ts">
	import { resolve } from '$app/paths';
	import icon from '../../../../assets/brand/svg/icon.svg?inline';
	import { act } from '#lib/actions.ts';
	import { auth } from '#lib/auth.svelte.ts';
	import { exportState, importState, refusal, uptimeOf, waitForRestart } from '#lib/backup.ts';
	import ConfirmDialog from '#lib/ConfirmDialog.svelte';
	import { data } from '#lib/data.svelte.ts';
	import { serialLink } from '#lib/health.ts';
	import { fmt, t } from '#lib/i18n.svelte.ts';
	import Icon from '#lib/Icon.svelte';
	import { setupState } from '#lib/install.ts';
	import { api } from '#lib/session.ts';
	import { LOCALES, prefs, type LocalePref, type Theme } from '#lib/prefs.svelte.ts';
	import { toasts } from '#lib/toast.svelte.ts';
	import type { HistoryInfo } from '#lib/types.ts';

	const health = $derived(data.health);
	const link = $derived(serialLink(health));
	let token = $state('');
	let tokenOpen = $state(false);

	const themes: Theme[] = ['system', 'light', 'dark'];
	const languages: Record<string, string> = { en: 'English', it: 'Italiano', de: 'Deutsch', fr: 'Français' };

	const declared = $derived(data.zones?.declared ?? []);
	const steps = $derived(setupState(health, declared));
	const openSteps = $derived(
		[steps.radio, steps.timezone, steps.rooms, steps.transmit, steps.devices, steps.circuits].filter((s) => s !== 'done').length
	);

	// The daemon names a set utc_offset_min "fixed offset", in English.
	const zoneLabel = (name: string) => (name === 'fixed offset' ? t('settings.timezone.fixed') : name);

	/** +120 minutes reads UTC+02:00. */
	function offset(minutes: number): string {
		const sign = minutes < 0 ? '-' : '+';
		const abs = Math.abs(minutes);
		return `UTC${sign}${String(Math.floor(abs / 60)).padStart(2, '0')}:${String(abs % 60).padStart(2, '0')}`;
	}

	const transmitTone = $derived(health?.transmitAllowed ? 'idle' : health?.tx ? 'fault' : 'off');

	function saveToken(event: SubmitEvent) {
		event.preventDefault();
		if (token.trim() === '') return;
		auth.set(token);
		token = '';
		tokenOpen = false;
		toasts.push('ok', t('settings.token.saved'));
		void data.refresh();
	}

	function removeToken() {
		auth.clear();
		toasts.push('ok', t('settings.token.removed'));
	}

	let history = $state<HistoryInfo | null>(null);
	let historyBusy = $state(false);
	let historyOff: ConfirmDialog | undefined = $state();
	let historyDelete: ConfirmDialog | undefined = $state();

	async function loadHistory() {
		try {
			history = await api.history();
		} catch {
			history = null;
		}
	}

	$effect(() => {
		void loadHistory();
	});

	async function setHistory(patch: { enabled: boolean; delete?: boolean }, done: string) {
		historyBusy = true;
		const result = await act(() => api.setHistory(patch), done);
		if (result) history = result;
		historyBusy = false;
	}

	/** On is one tap. Off asks first, and the switch shows the stored state until the answer. */
	function toggleHistory(box: HTMLInputElement) {
		if (box.checked) {
			void setHistory({ enabled: true }, t('history.enabled'));
			return;
		}
		box.checked = true;
		historyOff?.show();
	}

	const backupOptions = { token: () => auth.token };
	// After the wait of a transmitting daemon, the start itself takes a few seconds more.
	const RESTART_MARGIN_MS = 60_000;
	let fileInput: HTMLInputElement | undefined = $state();
	let backupFile = $state<File | null>(null);
	let backupBusy = $state(false);
	let backupState = $state<'idle' | 'restarting' | 'restored' | 'timeout'>('idle');
	let backupWaitSec = $state(0);
	let backupError = $state<string | null>(null);
	let restoreDialog: ConfirmDialog | undefined = $state();

	/** A refusal is a state of this section, not of the page: the other calls keep working. */
	function backupFailed(error: unknown) {
		const why = refusal(error);
		backupError =
			why === 'admin'
				? t('settings.backup.adminOnly')
				: why === 'token'
					? t('settings.backup.needsToken')
					: error instanceof Error
						? error.message
						: String(error);
	}

	async function downloadBackup() {
		backupBusy = true;
		backupError = null;
		try {
			const { name, blob } = await exportState(backupOptions);
			const href = URL.createObjectURL(blob);
			const link = document.createElement('a');
			link.href = href;
			link.download = name;
			document.body.append(link);
			link.click();
			link.remove();
			setTimeout(() => URL.revokeObjectURL(href), 10_000);
			toasts.push('ok', t('settings.backup.downloaded', { name }));
		} catch (error) {
			backupFailed(error);
		} finally {
			backupBusy = false;
		}
	}

	/**
	 * The chosen file opens the confirmation. The input is emptied at once, so the same file picked
	 * again after a cancel is a change too.
	 */
	function picked(input: HTMLInputElement) {
		const file = input.files?.[0];
		input.value = '';
		if (!file) return;
		backupFile = file;
		restoreDialog?.show();
	}

	async function restoreBackup() {
		const file = backupFile;
		if (!file) return;
		backupBusy = true;
		backupError = null;
		backupState = 'idle';
		const before = data.health?.uptimeSec ?? 0;
		try {
			const result = await importState(file, backupOptions);
			backupWaitSec = result.waitSec;
			backupState = 'restarting';
			data.restarting = true;
			const back = await waitForRestart(async () => uptimeOf(await api.health()), {
				before,
				limitMs: result.waitSec * 1000 + RESTART_MARGIN_MS
			});
			backupState = back ? 'restored' : 'timeout';
			backupFile = null;
		} catch (error) {
			backupFailed(error);
		} finally {
			data.restarting = false;
			backupBusy = false;
			void data.refresh();
		}
	}
</script>

{#snippet chevron()}
	<span class="chevron"><Icon name="chevron" size={18} /></span>
{/snippet}

<section class="page">
	<h1>{t('settings.title')}</h1>

	<div class="columns">
		<section class="section" aria-labelledby="install-title">
			<div class="section-head"><h2 id="install-title">{t('settings.install')}</h2></div>
			<ul class="list">
				<li>
					<a class="list-row" href={resolve('/devices')}>
						<span class="disc"><Icon name="devices" size={18} /></span>
						<span class="list-main">
							<span class="list-label">{t('devices.title')}</span>
							<span class="list-sub">{t('settings.install.count', { rooms: declared.length, devices: health?.devices.length ?? 0 })}</span>
						</span>
						{@render chevron()}
					</a>
				</li>
				<li>
					<a class="list-row" href={resolve('/setup')}>
						<span class="disc"><Icon name="check" size={18} /></span>
						<span class="list-main">
							<span class="list-label">{t('setup.title')}</span>
							<span class="list-sub">{openSteps === 0 ? t('settings.setup.done') : t('settings.setup.open', { n: openSteps })}</span>
						</span>
						{@render chevron()}
					</a>
				</li>
				<li>
					<a class="list-row" href={resolve('/setup')}>
						<span class="disc"><Icon name="clock" size={18} /></span>
						<span class="list-main">
							<span class="list-label">{t('settings.timezone')}</span>
							{#if health?.timezone}<span class="list-sub num">{offset(health.timezone.offsetMin)}</span>{/if}
						</span>
						<span class="list-value value">{health?.timezone ? zoneLabel(health.timezone.name) : '-'}</span>
						{@render chevron()}
					</a>
				</li>
			</ul>
			{#if health?.timezone?.fallback}
				<p class="notice tone-warning" role="status"><Icon name="warning" /><span>{t('settings.timezoneFallback')}</span></p>
			{/if}
		</section>

		<section class="section" aria-labelledby="look-title">
			<div class="section-head"><h2 id="look-title">{t('settings.look')}</h2></div>
			<ul class="list">
				<li>
					<fieldset class="theme">
						<legend class="sr-only">{t('settings.theme')}</legend>
						<div class="list-row" aria-hidden="true">
							<span class="disc"><Icon name="sliders" size={18} /></span>
							<span class="list-label">{t('settings.theme')}</span>
						</div>
						<div class="segmented">
							{#each themes as theme (theme)}
								<label>
									<input type="radio" name="theme" checked={prefs.theme === theme} onchange={() => prefs.setTheme(theme)} />
									{t(`theme.${theme}`)}
								</label>
							{/each}
						</div>
					</fieldset>
				</li>
				<li>
					<!-- The select covers the row, so the whole row opens the list of the system. -->
					<label class="list-row language">
						<span class="disc"><Icon name="globe" size={18} /></span>
						<span class="list-main">
							<span class="list-label">{t('settings.language')}</span>
							{#if prefs.localePref === 'auto'}<span class="list-sub">{t('settings.language.auto')}</span>{/if}
						</span>
						<span class="list-value value">{languages[prefs.locale]}</span>
						{@render chevron()}
						<select value={prefs.localePref} onchange={(e) => prefs.setLocale(e.currentTarget.value as LocalePref)}>
							<option value="auto">{t('settings.language.auto')}</option>
							{#each LOCALES as locale (locale)}
								<option value={locale}>{languages[locale]}</option>
							{/each}
						</select>
					</label>
				</li>
			</ul>
		</section>

		<section class="section" aria-labelledby="daemon-title">
			<div class="section-head"><h2 id="daemon-title">{t('settings.daemon')}</h2></div>
			{#if !health}
				<p class="muted">{t('state.loading')}</p>
			{:else}
				{#if health.warning}
					<p class="notice tone-warning" role="status"><Icon name="warning" /><span>{health.warning}</span></p>
				{/if}
				{#if link === 'warning'}
					<p class="notice tone-warning" role="status">
						<Icon name="warning" /><span>{t('settings.serialErrors.notice', { n: fmt.number(health.serialErrors, 0) })}</span>
					</p>
				{/if}
				<ul class="list">
					<li class="list-row">
						<span class="disc"><Icon name="radio" size={18} /></span>
						<span class="list-main"><span class="list-label">{t('settings.mode')}</span></span>
						<span class="list-value value"><span class="dot tone-{health.mode === 'live' ? 'idle' : 'off'}"></span>{t(`mode.${health.mode}`)}</span>
					</li>
					<li class="list-row">
						<span class="disc"><Icon name="bolt" size={18} /></span>
						<span class="list-main"><span class="list-label">{t('settings.transmit')}</span></span>
						<span class="list-value value">
							<span class="dot tone-{transmitTone}"></span>
							{health.transmitAllowed ? t('settings.transmit.on') : health.tx ? t('settings.transmit.blocked') : t('settings.transmit.dryRun')}
						</span>
					</li>
					<!-- A live radio reports its calibration, with the radio error and the serial link under it. -->
					<li class="list-row">
						<span class="disc"><Icon name="check" size={18} /></span>
						{#if health.calibrated !== undefined}
							<span class="list-main">
								<span class="list-label">{t('settings.calibrated')}</span>
								<span class="list-sub">
									{t('settings.radio')}: {health.radioError ?? t('settings.radio.ok')}{#if link !== 'none'}&nbsp;· {t('settings.serialErrors')}:
										<span class="num">{fmt.number(health.serialErrors, 0)}</span>{/if}
								</span>
							</span>
							<span class="list-value value">{health.calibrated ? t('state.yes') : t('state.no')}</span>
						{:else}
							<span class="list-main"><span class="list-label">{t('settings.radio')}</span></span>
							<span class="list-value value">{health.radioError ?? t('settings.radio.ok')}</span>
						{/if}
					</li>
					<li class="list-row">
						<span class="disc"><Icon name="pulse" size={18} /></span>
						<span class="list-main">
							<span class="list-label">{t('settings.frames')}</span>
							<span class="list-sub">{t('settings.uptime')}: <span class="num">{fmt.uptime(health.uptimeSec)}</span></span>
						</span>
						<span class="list-value num">{fmt.number(health.frames, 0)}</span>
					</li>
					<li>
						<details>
							<summary class="list-row">
								<span class="list-main"><span class="list-label more">{t('settings.details')}</span></span>
								{@render chevron()}
							</summary>
							<dl class="facts-list">
								<div><dt>{t('settings.bridge')}</dt><dd>{health.bridgePresent ? t('state.yes') : t('state.no')}</dd></div>
								{#if health.fsctrl0 !== undefined}<div><dt>FSCTRL0</dt><dd class="num">{health.fsctrl0}</dd></div>{/if}
								<div><dt>{t('settings.micFails')}</dt><dd class="num">{fmt.number(health.micFails, 0)}</dd></div>
								<div><dt>{t('settings.queue')}</dt><dd class="num">{fmt.number(health.queue.pending, 0)}</dd></div>
								<div><dt>{t('settings.downlink')}</dt><dd class="num">{health.downlink.delivered} / {health.downlink.sent} / {health.downlink.dropped}</dd></div>
								<div><dt>{t('settings.unhandled')}</dt><dd class="num">{fmt.number(health.unhandled, 0)}</dd></div>
							</dl>
						</details>
					</li>
				</ul>
			{/if}
		</section>

		<section class="section" aria-labelledby="history-title" data-testid="history">
			<div class="section-head"><h2 id="history-title">{t('history.title')}</h2></div>
			{#if !history}
				<p class="muted">{t('state.loading')}</p>
			{:else if !history.available}
				<p class="notice tone-off"><Icon name="warning" /><span>{t('history.noDatabase')}</span></p>
			{:else}
				{#if history.error}
					<p class="notice tone-fault" role="alert"><Icon name="fault" /><span>{history.error}</span></p>
				{/if}
				<ul class="list">
					<li>
						<label class="list-row">
							<span class="disc"><Icon name="pulse" size={18} /></span>
							<span class="list-main">
								<span class="list-label">{t('history.record')}</span>
								<span class="list-sub">{t('history.what', { days: history.sampleDays, months: history.dayMonths })}</span>
							</span>
							<input
								type="checkbox"
								role="switch"
								data-testid="history-switch"
								checked={history.enabled}
								disabled={historyBusy}
								onchange={(e) => toggleHistory(e.currentTarget)}
							/>
						</label>
					</li>
					{#if history.enabled || history.sizeBytes > 0}
						<li class="list-row">
							<span class="disc"><Icon name="clock" size={18} /></span>
							<span class="list-main">
								<span class="list-label">{t('history.stored')}</span>
								{#if history.since}<span class="list-sub">{t('history.since', { date: fmt.dateTime(history.since) })}</span>{/if}
							</span>
							<span class="list-value num">{fmt.size(history.sizeBytes)}</span>
						</li>
					{/if}
					{#if history.enabled}
						<li>
							<a class="list-row" href={resolve('/boiler/energy')}>
								<span class="disc"><Icon name="flame" size={18} /></span>
								<span class="list-main">
									<span class="list-label">{t('energy.title')}</span>
									<span class="list-sub">{t('history.energySub')}</span>
								</span>
								{@render chevron()}
							</a>
						</li>
					{:else if history.sizeBytes > 0}
						<li>
							<button class="list-row" type="button" data-testid="history-delete" disabled={historyBusy} onclick={() => historyDelete?.show()}>
								<span class="disc"><Icon name="trash" size={18} /></span>
								<span class="list-main">
									<span class="list-label danger">{t('history.delete')}</span>
									<span class="list-sub">{t('history.deleteSub')}</span>
								</span>
							</button>
						</li>
					{/if}
				</ul>
				<p class="note">{t('history.local')}</p>
			{/if}
		</section>

		<section class="section" aria-labelledby="backup-title" data-testid="backup">
			<div class="section-head"><h2 id="backup-title">{t('settings.backup')}</h2></div>
			{#if backupError}
				<p class="notice tone-fault" role="alert" data-testid="backup-error"><Icon name="fault" /><span>{backupError}</span></p>
			{/if}
			{#if backupState === 'restarting'}
				<p class="notice tone-warning" role="status" data-testid="backup-restarting">
					<Icon name="refresh" />
					<span>
						{data.waitSec !== null
							? data.waitSec > 0
								? t('banner.importWait', { seconds: data.waitSec })
								: t('banner.restarting')
							: backupWaitSec > 0
								? t('settings.backup.waiting', { seconds: backupWaitSec })
								: t('settings.backup.restarting')}
					</span>
				</p>
			{:else if backupState === 'restored'}
				<p class="notice tone-idle" role="status" data-testid="backup-restored"><Icon name="check" /><span>{t('settings.backup.restored')}</span></p>
			{:else if backupState === 'timeout'}
				<p class="notice tone-fault" role="alert" data-testid="backup-timeout"><Icon name="fault" /><span>{t('settings.backup.timeout')}</span></p>
			{/if}
			<ul class="list">
				<li>
					<button class="list-row" type="button" onclick={downloadBackup} disabled={backupBusy}>
						<span class="disc"><Icon name="download" size={18} /></span>
						<span class="list-main">
							<span class="list-label">{t('settings.backup.download')}</span>
							<span class="list-sub">{t('settings.backup.download.sub')}</span>
						</span>
						{@render chevron()}
					</button>
				</li>
				<li>
					<button class="list-row" type="button" onclick={() => fileInput?.click()} disabled={backupBusy}>
						<span class="disc"><Icon name="upload" size={18} /></span>
						<span class="list-main">
							<span class="list-label">{t('settings.backup.restore')}</span>
							<span class="list-sub">{t('settings.backup.restore.sub')}</span>
						</span>
						{@render chevron()}
					</button>
				</li>
			</ul>
			<input
				class="sr-only"
				type="file"
				tabindex="-1"
				aria-label={t('settings.backup.file')}
				accept=".db,application/vnd.sqlite3,application/octet-stream"
				bind:this={fileInput}
				onchange={(e) => picked(e.currentTarget)}
			/>
			<p class="note secret" data-testid="backup-secret"><Icon name="lock" size={14} /><span>{t('settings.backup.secret')}</span></p>
		</section>

		<section class="section" aria-labelledby="token-title">
			<div class="section-head"><h2 id="token-title">{t('settings.access')}</h2></div>
			<ul class="list">
				<li>
					<button class="list-row" aria-expanded={tokenOpen} data-testid="token-row" onclick={() => (tokenOpen = !tokenOpen)}>
						<span class="disc"><Icon name="key" size={18} /></span>
						<span class="list-main">
							<span class="list-label">{t('settings.token')}</span>
							<span class="list-sub">{t('settings.token.sub')}</span>
						</span>
						<span class="list-value value">{auth.token ? t('settings.token.set') : t('settings.token.none')}</span>
						{@render chevron()}
					</button>
				</li>
				{#if tokenOpen}
					<li class="token">
						<form class="row fill" onsubmit={saveToken}>
							<label class="field grow">
								<span>{t('token.label')}</span>
								<input class="input mono" type="password" autocomplete="off" spellcheck="false" bind:value={token} />
							</label>
							<button class="btn primary" type="submit" disabled={token.trim() === ''}><Icon name="key" size={18} />{t('token.save')}</button>
							<button class="btn danger" type="button" onclick={removeToken} disabled={!auth.token}><Icon name="trash" size={18} />{t('settings.token.remove')}</button>
						</form>
						<p class="note">{t('settings.token.note')}</p>
					</li>
				{/if}
			</ul>
		</section>
	</div>

	{#if health?.version}
		<footer class="version"><img src={icon} alt="" width="28" height="28" />freedo <span class="num">{health.version}</span></footer>
	{/if}
</section>

<ConfirmDialog
	bind:this={historyOff}
	title={t('history.off.title')}
	body={t('history.off.body')}
	confirm={t('history.off.confirm')}
	onconfirm={() => void setHistory({ enabled: false }, t('history.disabled'))}
/>

<ConfirmDialog
	bind:this={historyDelete}
	title={t('history.delete.title')}
	body={t('history.delete.body')}
	confirm={t('history.delete')}
	onconfirm={() => void setHistory({ enabled: false, delete: true }, t('history.deleted'))}
/>

<ConfirmDialog
	bind:this={restoreDialog}
	title={t('settings.backup.confirm.title')}
	body={t('settings.backup.confirm.body', { name: backupFile?.name ?? '' })}
	items={[
		t('settings.backup.replaces.install'),
		t('settings.backup.replaces.programs'),
		t('settings.backup.replaces.keys'),
		t('settings.backup.replaces.counters'),
		t('settings.backup.replaces.learned')
	]}
	confirm={t('settings.backup.restore')}
	onconfirm={() => void restoreBackup()}
/>

<style>
	.columns {
		display: grid;
		grid-template-columns: repeat(auto-fill, minmax(min(100%, 28rem), 1fr));
		gap: 1.5rem 1.25rem;
		align-items: start;
	}

	.value {
		color: var(--text-muted);
		font-weight: var(--weight-regular);
	}

	.chevron {
		flex: none;
		color: var(--text-muted);
	}

	.danger {
		color: var(--status-fault-text);
	}

	.theme {
		display: grid;
		margin: 0;
		padding: 0 0 0.875rem;
		border: 0;
	}

	.theme .segmented {
		display: grid;
		margin-inline: 0.875rem;
		grid-template-columns: repeat(3, minmax(0, 1fr));
		justify-self: stretch;
	}

	.language {
		position: relative;
		cursor: pointer;
	}

	.language select {
		position: absolute;
		inset: 0;
		width: 100%;
		opacity: 0;
		cursor: pointer;
	}

	.language:has(select:focus-visible) {
		outline: 3px solid var(--accent);
		outline-offset: -3px;
	}

	.dot.tone-idle {
		--dot: var(--status-idle-text);
	}

	.dot.tone-off {
		--dot: var(--status-off-text);
	}

	.dot.tone-fault {
		--dot: var(--status-fault-text);
	}

	details summary {
		list-style: none;
		cursor: pointer;
	}

	details summary::-webkit-details-marker {
		display: none;
	}

	details[open] .chevron {
		rotate: 90deg;
	}

	.more {
		color: var(--link);
	}

	.facts-list {
		display: grid;
		gap: 0.4rem;
		margin: 0;
		padding: 0 0.875rem 0.875rem;
		font-size: var(--text-small-size);
	}

	.facts-list div {
		display: flex;
		justify-content: space-between;
		gap: 1rem;
	}

	.facts-list dt {
		color: var(--text-muted);
	}

	.facts-list dd {
		margin: 0;
		text-align: right;
		overflow-wrap: anywhere;
	}

	.secret {
		display: flex;
		align-items: flex-start;
		gap: 0.4rem;
		margin: 0 0.25rem;
	}

	.secret :global(svg) {
		flex: none;
		margin-top: 0.2rem;
	}

	.token {
		display: grid;
		gap: 0.5rem;
		padding: 0.875rem;
	}

	.grow {
		flex: 1 1 14rem;
	}

	.version {
		display: flex;
		align-items: center;
		justify-content: center;
		gap: 0.5rem;
		color: var(--text-muted);
		font-size: var(--text-small-size);
	}

	.version img {
		border-radius: 7px;
	}

	.version .num {
		color: var(--text);
	}
</style>
