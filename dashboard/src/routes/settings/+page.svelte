<script lang="ts">
	import { resolve } from '$app/paths';
	import { auth } from '#lib/auth.svelte.ts';
	import { data } from '#lib/data.svelte.ts';
	import { serialLink } from '#lib/health.ts';
	import { fmt, t } from '#lib/i18n.svelte.ts';
	import Icon from '#lib/Icon.svelte';
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

	<section class="panel" aria-labelledby="install-title">
		<header><h2 id="install-title">{t('settings.install')}</h2></header>
		<p class="note">{t('settings.install.note')}</p>
		<div class="row fill">
			<a class="btn" href={resolve('/setup')}><Icon name="check" size={18} />{t('setup.link')}</a>
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
