<script lang="ts">
	import { resolve } from '$app/paths';
	import { auth } from '#lib/auth.svelte.ts';
	import { exportState, importState, refusal, uptimeOf, waitForRestart } from '#lib/backup.ts';
	import ConfirmDialog from '#lib/ConfirmDialog.svelte';
	import { data } from '#lib/data.svelte.ts';
	import { serialLink } from '#lib/health.ts';
	import { fmt, t } from '#lib/i18n.svelte.ts';
	import Icon from '#lib/Icon.svelte';
	import { api } from '#lib/session.ts';
	import { LOCALES, prefs, type LocalePref, type Theme } from '#lib/prefs.svelte.ts';
	import { toasts } from '#lib/toast.svelte.ts';

	const health = $derived(data.health);
	const link = $derived(serialLink(health));
	let token = $state('');

	const themes: Theme[] = ['system', 'light', 'dark'];
	const languages: Record<string, string> = { en: 'English', it: 'Italiano', de: 'Deutsch', fr: 'Français' };

	// The daemon names a set utc_offset_min "fixed offset", in English.
	const zoneLabel = (name: string) => (name === 'fixed offset' ? t('settings.timezone.fixed') : name);

	/** +120 minutes reads UTC+02:00. */
	function offset(minutes: number): string {
		const sign = minutes < 0 ? '-' : '+';
		const abs = Math.abs(minutes);
		return `UTC${sign}${String(Math.floor(abs / 60)).padStart(2, '0')}:${String(abs % 60).padStart(2, '0')}`;
	}

	function saveToken(event: SubmitEvent) {
		event.preventDefault();
		if (token.trim() === '') return;
		auth.set(token);
		token = '';
		toasts.push('ok', t('settings.token.saved'));
		void data.refresh();
	}

	function removeToken() {
		auth.clear();
		toasts.push('ok', t('settings.token.removed'));
	}

	const backupOptions = { token: () => auth.token };
	// After the wait of a transmitting daemon, the start itself takes a few seconds more.
	const RESTART_MARGIN_MS = 60_000;
	let backupFiles = $state<FileList | undefined>();
	const backupFile = $derived(backupFiles?.[0]);
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
			backupFiles = undefined;
		} catch (error) {
			backupFailed(error);
		} finally {
			data.restarting = false;
			backupBusy = false;
			void data.refresh();
		}
	}

</script>

<section class="page">
	<div class="page-head"><h1>{t('settings.title')}</h1></div>

	<section class="panel" aria-labelledby="look-title">
		<header><h2 id="look-title">{t('settings.look')}</h2></header>
		<fieldset class="group">
			<legend class="small muted">{t('settings.theme')}</legend>
			<div class="segmented">
				{#each themes as theme (theme)}
					<label>
						<input type="radio" name="theme" checked={prefs.theme === theme} onchange={() => prefs.setTheme(theme)} />
						{t(`theme.${theme}`)}
					</label>
				{/each}
			</div>
		</fieldset>
		<label class="field select">
			<span>{t('settings.language')}</span>
			<select class="input" value={prefs.localePref} onchange={(e) => prefs.setLocale(e.currentTarget.value as LocalePref)}>
				<option value="auto">{t('settings.language.auto')}</option>
				{#each LOCALES as locale (locale)}
					<option value={locale}>{languages[locale]}</option>
				{/each}
			</select>
		</label>
	</section>

	<section class="panel" aria-labelledby="token-title">
		<header>
			<h2 id="token-title">{t('settings.token')}</h2>
			<span class="badge" class:tone-idle={auth.token} class:tone-off={!auth.token}>
				{auth.token ? t('settings.token.set') : t('settings.token.none')}
			</span>
		</header>
		<p class="note">{t('settings.token.note')}</p>
		<form class="row fill" onsubmit={saveToken}>
			<label class="field grow">
				<span>{t('token.label')}</span>
				<input class="input mono" type="password" autocomplete="off" spellcheck="false" bind:value={token} />
			</label>
			<button class="btn primary" type="submit" disabled={token.trim() === ''}><Icon name="key" size={18} />{t('token.save')}</button>
			<button class="btn danger" type="button" onclick={removeToken} disabled={!auth.token}><Icon name="trash" size={18} />{t('settings.token.remove')}</button>
		</form>
	</section>

	<section class="panel" aria-labelledby="backup-title" data-testid="backup">
		<header><h2 id="backup-title">{t('settings.backup')}</h2></header>
		<p class="note">{t('settings.backup.note')}</p>
		<p class="notice tone-warning" role="note"><Icon name="key" /><span>{t('settings.backup.secret')}</span></p>
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
		<div class="row fill">
			<button class="btn" type="button" onclick={downloadBackup} disabled={backupBusy}><Icon name="key" size={18} />{t('settings.backup.download')}</button>
		</div>
		<div class="row fill">
			<label class="field grow">
				<span>{t('settings.backup.file')}</span>
				<input class="input" type="file" accept=".db,application/vnd.sqlite3,application/octet-stream" bind:files={backupFiles} disabled={backupBusy} />
			</label>
			<button class="btn danger" type="button" onclick={() => restoreDialog?.show()} disabled={backupBusy || !backupFile}>
				<Icon name="refresh" size={18} />{t('settings.backup.restore')}
			</button>
		</div>
	</section>

	<section class="panel" aria-labelledby="install-title">
		<header><h2 id="install-title">{t('settings.install')}</h2></header>
		<p class="note">{t('settings.install.note')}</p>
		<div class="row fill">
			<a class="btn" href={resolve('/setup')}><Icon name="check" size={18} />{t('setup.title')}</a>
			<a class="btn" href={resolve('/devices')}><Icon name="devices" size={18} />{t('devices.title')}</a>
		</div>
	</section>

	<section class="panel" aria-labelledby="daemon-title">
		<header><h2 id="daemon-title">{t('settings.daemon')}</h2></header>
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
			{#if health.timezone?.fallback}
				<p class="notice tone-warning" role="status"><Icon name="warning" /><span>{t('settings.timezoneFallback')}</span></p>
			{/if}
			<dl class="facts">
				<div><dt>{t('settings.mode')}</dt><dd class="text">{t(`mode.${health.mode}`)}</dd></div>
				<div><dt>{t('settings.transmit')}</dt><dd class="text">{health.transmitAllowed ? t('settings.transmit.on') : health.tx ? t('settings.transmit.blocked') : t('settings.transmit.dryRun')}</dd></div>
				<div><dt>{t('settings.radio')}</dt><dd class="text">{health.radioError ?? t('settings.radio.ok')}</dd></div>
				{#if link !== 'none'}
					<div>
						<dt>{t('settings.serialErrors')}</dt>
						<dd class="text">
							<span class="mono">{fmt.number(health.serialErrors, 0)}</span>
							<span class="badge tone-{link === 'warning' ? 'warning' : 'idle'}">
								{link === 'warning' ? t('settings.serialErrors.warn') : t('settings.serialErrors.ok')}
							</span>
						</dd>
					</div>
				{/if}
				<div><dt>{t('settings.bridge')}</dt><dd class="text">{health.bridgePresent ? t('state.yes') : t('state.no')}</dd></div>
				{#if health.calibrated !== undefined}
					<div><dt>{t('settings.calibrated')}</dt><dd class="text">{health.calibrated ? t('state.yes') : t('state.no')} ({health.fsctrl0})</dd></div>
				{/if}
				<div>
					<dt>{t('settings.timezone')}</dt>
					<dd class="text">{health.timezone ? `${zoneLabel(health.timezone.name)}, ${offset(health.timezone.offsetMin)}` : '-'}</dd>
				</div>
				<div><dt>{t('settings.uptime')}</dt><dd>{fmt.uptime(health.uptimeSec)}</dd></div>
				<div><dt>{t('settings.frames')}</dt><dd>{fmt.number(health.frames, 0)}</dd></div>
				<div><dt>{t('settings.micFails')}</dt><dd>{fmt.number(health.micFails, 0)}</dd></div>
				<div><dt>{t('settings.queue')}</dt><dd>{fmt.number(health.queue.pending, 0)}</dd></div>
				<div><dt>{t('settings.downlink')}</dt><dd>{health.downlink.delivered} / {health.downlink.sent} / {health.downlink.dropped}</dd></div>
				<div><dt>{t('settings.unhandled')}</dt><dd>{fmt.number(health.unhandled, 0)}</dd></div>
			</dl>
		{/if}
	</section>
</section>

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
	.group {
		margin: 0;
		padding: 0;
		border: 0;
		display: grid;
		gap: 0.4rem;
	}

	.group legend {
		padding: 0;
		margin-bottom: 0.35rem;
		font-weight: var(--weight-medium);
	}

	.select {
		max-width: 20rem;
	}

	.grow {
		flex: 1 1 16rem;
	}
</style>
