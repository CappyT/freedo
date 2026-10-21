<script lang="ts">
	import { resolve } from '$app/paths';
	import { actMany, act } from '#lib/actions.ts';
	import { boilerFaults, burnerState } from '#lib/boiler.ts';
	import { data } from '#lib/data.svelte.ts';
	import { alertKeys } from '#lib/describe.ts';
	import { fmt, t } from '#lib/i18n.svelte.ts';
	import Icon, { type IconName } from '#lib/Icon.svelte';
	import RadioChip from '#lib/RadioChip.svelte';
	import { api } from '#lib/session.ts';
	import Tile, { type TileChip, type TileFill } from '#lib/Tile.svelte';
	import { stateIcons } from '#lib/today.svelte.ts';
	import { deviceAlerts, zoneName, type StateKey, type ZoneModel } from '#lib/zones.ts';

	let busy = $state(false);
	let confirmBoost = $state(false);
	let menuOpen = $state(false);
	let menuRoot: HTMLDivElement | undefined = $state();
	let menuEl: HTMLDivElement | undefined = $state();
	// The menu opens from the left edge of its tile. A tile in the right column of a phone leaves too
	// little room, so the menu then opens from the right edge.
	let menuEnd = $state(false);

	$effect(() => {
		if (!menuEl || !menuRoot) return;
		const room = document.documentElement.clientWidth - menuRoot.getBoundingClientRect().left;
		menuEnd = menuEl.offsetWidth > room - 16;
	});

	const zones = $derived(data.models);
	const rooms = $derived(data.heating);
	const presets = $derived(data.presets);
	const activePreset = $derived(presets.find((preset) => preset.active));

	// A state that is not about heat carries a large faint icon, so the grid reads without the text.
	const stateMark: Partial<Record<StateKey, IconName>> = {
		away: 'away',
		off: 'power',
		windowOpen: 'window',
		offline: 'offline',
		fault: 'fault'
	};

	function roomLine(m: ZoneModel): string {
		const target = m.zone.target.targetC;
		switch (m.status.state) {
			case 'heating':
				return target === null ? t('state.heating') : t('tile.heatingTo', { temp: fmt.degrees(target) });
			case 'idle':
			case 'on':
			case 'away': {
				const state = t(`state.${m.status.state === 'on' ? 'idle' : m.status.state}`);
				return target === null ? state : t('tile.stateAt', { state, temp: fmt.degrees(target) });
			}
			default:
				return t(`state.${m.status.state}`);
		}
	}

	function roomChips(m: ZoneModel): TileChip[] {
		const chips: TileChip[] = [];
		if (m.humidityPct !== null && m.status.state !== 'offline') chips.push({ icon: 'drop', text: `${fmt.number(m.humidityPct, 0)}%` });
		const overlay = m.zone.target.overlay;
		if (m.zone.target.boost) chips.push({ icon: 'bolt', text: overlay?.endsAt ? fmt.when(overlay.endsAt) : t('mode.boost') });
		else if (overlay?.endsAt) chips.push({ icon: 'clock', text: fmt.when(overlay.endsAt) });
		else if (overlay) chips.push({ icon: 'edit', text: t('tile.manual') });
		return chips;
	}

	function alertText(m: ZoneModel): string | null {
		return m.status.alerts.length === 0 ? null : t('tile.alerts', { list: m.status.alerts.map((a) => t(alertKeys[a])).join(', ') });
	}

	function roomTile(m: ZoneModel) {
		const offline = m.status.state === 'offline';
		const value = offline ? null : m.tempC;
		const line = roomLine(m);
		const alert = alertText(m);
		const parts = [m.zone.name, value === null ? null : fmt.temp(value), line, m.humidityPct === null ? null : `${t('card.humidity')} ${fmt.percent(m.humidityPct)}`, alert];
		return {
			fill: m.status.tone as TileFill,
			name: m.zone.name,
			line,
			lineIcon: stateIcons[m.status.state],
			value,
			chips: roomChips(m),
			mark: stateMark[m.status.state] ?? null,
			alert,
			label: parts.filter(Boolean).join(', '),
			href: resolve('/zones/[n]', { n: String(m.zone.number) })
		};
	}

	const water = $derived.by(() => {
		const m = data.hotWater;
		if (!m) return null;
		const on = m.zone.target.on;
		const state = m.status.state;
		const line = state === 'offline' || state === 'fault' ? t(`state.${state}`) : on ? t('tile.hotWaterOn', { temp: fmt.degrees(m.zone.target.targetC) }) : t('tile.hotWaterOff');
		const value = data.boiler?.DHWC ?? null;
		const name = zoneName(m.zone, t('zone.hotWaterName'));
		return {
			fill: m.status.tone as TileFill,
			name,
			line,
			lineIcon: (on ? 'idle' : 'power') as IconName,
			value,
			alert: alertText(m),
			label: [name, value === null ? null : `${t('hotWater.inBoiler')} ${fmt.temp(value)}`, line].filter(Boolean).join(', ')
		};
	});

	const boiler = $derived.by(() => {
		const b = data.boiler;
		if (!b) return null;
		const faults = boilerFaults(b);
		const burner = burnerState(b);
		const fault = faults.state === 'fault';
		const line = fault
			? `${t('state.fault')} · ${faults.codes.join(', ')}`
			: burner === 'on'
				? t('boiler.burnerOn')
				: burner === 'off'
					? t('boiler.burnerOff')
					: t('boiler.burnerUnknown');
		const fill: TileFill = fault ? 'fault' : burner === 'on' ? 'heating' : 'off';
		return {
			fill,
			line,
			lineIcon: (fault ? 'fault' : burner === 'on' ? 'flame' : 'power') as IconName,
			value: b.FlowC,
			mark: fault ? ('fault' as IconName) : null,
			label: [t('boiler.title'), `${t('boiler.flow')} ${fmt.temp(b.FlowC)}`, line].join(', ')
		};
	});

	// The zones that need a look: unreachable, faulty, or with a warning on a device.
	const problems = $derived(
		zones
			.map((m) => {
				// A device alert names its devices: "Low battery: Hall valve".
				const members = (data.health?.devices ?? []).filter((h) => m.zone.members.includes(h.short));
				const texts = m.status.alerts.map((a) => {
					const raised = deviceAlerts[a];
					const names = raised ? members.filter(raised).map((h) => h.label || h.device) : [];
					return names.length > 0 ? `${t(alertKeys[a])}: ${names.join(', ')}` : t(alertKeys[a]);
				});
				if (m.status.state === 'offline' || m.status.state === 'fault') texts.unshift(t(`state.${m.status.state}`));
				const icon: IconName = m.status.state === 'offline' ? 'offline' : m.status.state === 'fault' ? 'fault' : m.status.alerts.includes('lowBattery') ? 'battery' : 'warning';
				const tone = m.status.state === 'offline' ? 'offline' : m.status.state === 'fault' ? 'fault' : 'warning';
				return { m, texts, icon, tone };
			})
			.filter((p) => p.texts.length > 0)
	);

	async function setAway(value: boolean) {
		if (value === data.away || busy) return;
		busy = true;
		await act(() => api.setAway(value));
		busy = false;
	}

	/** Switches the active profile. The menu shows the profile the daemon reports. */
	async function activate(id: number) {
		menuOpen = false;
		if (id === activePreset?.id || busy) return;
		busy = true;
		await act(() => api.activatePreset(id));
		busy = false;
	}

	function closeOutside(event: MouseEvent) {
		if (menuOpen && menuRoot && !menuRoot.contains(event.target as Node)) menuOpen = false;
	}

	async function boostAll() {
		confirmBoost = false;
		await actMany(rooms.map((m) => () => api.boost(m.zone.number, true)));
	}

	const resumeAll = () => actMany(zones.map((m) => () => api.auto(m.zone.number)));
</script>

<section class="page home" aria-labelledby="title">
	<header class="head">
		<h1 id="title">{t('home.title')}</h1>
		<RadioChip />
	</header>

	{#if !data.loaded}
		<ul class="tiles" aria-busy="true" aria-label={t('state.loading')}>
			{#each [0, 1, 2, 3] as n (n)}<li class="ghost"></li>{/each}
		</ul>
	{:else if zones.length === 0}
		<section class="empty" aria-labelledby="empty-title">
			<div class="ghosts" aria-hidden="true"><span></span><span></span><span></span><span class="plus"><Icon name="plus" size={18} /></span></div>
			<h2 id="empty-title">{t('home.empty.title')}</h2>
			<p>{t('home.empty.body')}</p>
			<a class="btn primary" href={resolve('/setup')}>{t('setup.title')}</a>
			{#if data.problem}<p class="note mono">{data.problem}</p>{/if}
		</section>
	{:else}
		<div class="actions">
			<button class="qa" type="button" onclick={() => (confirmBoost = true)} disabled={rooms.length === 0} aria-expanded={confirmBoost}>
				<span class="qa-icon boost"><Icon name="bolt" size={18} /></span>
				<span class="qa-text"><span class="qa-label">{t('home.boostAll')}</span><span class="qa-sub">{t('home.boostAll.sub')}</span></span>
			</button>
			<button class="qa" type="button" onclick={resumeAll}>
				<span class="qa-icon resume"><Icon name="calendar" size={18} /></span>
				<span class="qa-text"><span class="qa-label">{t('home.resumeAll')}</span><span class="qa-sub">{t('home.resumeAll.sub')}</span></span>
			</button>
		</div>

		{#if confirmBoost}
			<div class="confirm" role="group" aria-label={t('home.boostAll')}>
				<p>{t('home.boostAll.body')}</p>
				<div class="confirm-row">
					<button class="btn" type="button" onclick={() => (confirmBoost = false)}>{t('action.cancel')}</button>
					<button class="btn primary" type="button" onclick={boostAll}><Icon name="bolt" size={18} />{t('home.boostAll')}</button>
				</div>
			</div>
		{/if}

		{#if problems.length > 0}
			<ul class="list problems" aria-label={t('home.alerts')}>
				{#each problems as p (p.m.zone.number)}
					<li>
						<a class="list-row" href={p.m.zone.kind === 'hotwater' ? resolve('/hot-water') : resolve('/zones/[n]', { n: String(p.m.zone.number) })}>
							<span class="problem-icon tone-{p.tone}" class:offline={p.tone === 'offline'}><Icon name={p.icon} size={18} /></span>
							<span class="list-main">
								<span class="list-label">{zoneName(p.m.zone, t('zone.hotWaterName'))}</span>
								<span class="list-sub">{p.texts.join(' · ')}</span>
							</span>
							<span class="chevron"><Icon name="chevron" size={18} /></span>
						</a>
					</li>
				{/each}
			</ul>
		{/if}

		{#if rooms.length > 0}
			<ul class="tiles" aria-label={t('home.rooms')}>
				{#each rooms as m (m.zone.number)}
					<li><Tile {...roomTile(m)} /></li>
				{/each}
			</ul>
		{/if}

		<h2 class="group-title">{t('home.system')}</h2>
		<ul class="tiles system" aria-label={t('home.system')}>
			<li>
				<Tile
					fill="neutral"
					disc={data.away ? 'away' : 'home'}
					discTone={data.away ? 'away' : null}
					word={data.away ? t('home.away') : t('home.atHome')}
					name={t('home.presence')}
					line={data.away ? t('home.presenceReturn') : t('home.presenceLeave')}
					label={`${t('home.presence')}: ${data.away ? t('home.away') : t('home.atHome')}`}
					pressed={data.away}
					disabled={busy || !data.home}
					onclick={() => setAway(!data.away)}
					testid="presence"
				/>
			</li>
			{#if presets.length > 0}
				<li>
					<div class="profile-menu" bind:this={menuRoot}>
						<Tile
							fill="neutral"
							disc="calendar"
							word={activePreset?.name ?? '-'}
							name={t('preset.active')}
							line={presets.length === 1 ? t('home.profileOne') : t('home.profileCount', { n: presets.length })}
							label={`${t('preset.active')}: ${activePreset?.name ?? '-'}. ${t('home.profileMenu')}`}
							expanded={menuOpen}
							disabled={busy}
							onclick={() => (menuOpen = !menuOpen)}
							extraClass="menu-button"
						/>
						{#if menuOpen}
							<div class="menu" class:end={menuEnd} role="menu" aria-label={t('home.profileMenu')} bind:this={menuEl}>
								{#each presets as preset (preset.id)}
									<button class="item" role="menuitemradio" aria-checked={preset.active} onclick={() => activate(preset.id)}>
										<span class="radio" aria-hidden="true"></span>
										<span class="grow">{preset.name}</span>
										{#if preset.active}<span class="badge tone-idle">{t('presets.activeBadge')}</span>{/if}
									</button>
								{/each}
								<hr />
								<a class="item" role="menuitem" href={resolve('/programs?new=1')}><Icon name="plus" size={18} />{t('home.newProfile')}</a>
								<a class="item" role="menuitem" href={resolve('/programs')}><Icon name="edit" size={18} />{t('home.editPrograms')}</a>
							</div>
						{/if}
					</div>
				</li>
			{/if}
			{#if water}
				<li><Tile {...water} mark="drop" href={resolve('/hot-water')} /></li>
			{/if}
			{#if boiler}
				<li>
					<Tile
						{...boiler}
						name={t('boiler.title')}
						chips={[{ text: t('boiler.flow') }]}
						href={resolve('/boiler')}
					/>
				</li>
			{/if}
		</ul>
	{/if}
</section>

<svelte:window onclick={closeOutside} onkeydown={(e) => e.key === 'Escape' && (menuOpen = false)} />

<style>
	.head {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		justify-content: space-between;
		gap: 0.75rem;
		padding-top: 0.25rem;
	}

	.actions {
		display: grid;
		grid-template-columns: repeat(2, minmax(0, 1fr));
		gap: 0.75rem;
	}

	.qa {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		min-height: 3.875rem;
		padding: 0.625rem;
		border: 1px solid light-dark(transparent, var(--border));
		border-radius: 18px;
		background: var(--surface);
		color: var(--text);
		box-shadow:
			0 1px 2px color-mix(in srgb, var(--navy) 6%, transparent),
			0 8px 18px -10px color-mix(in srgb, var(--navy) 22%, transparent);
		text-align: left;
		cursor: pointer;
	}

	.qa:hover:not(:disabled) {
		border-color: var(--accent);
	}

	.qa:disabled {
		opacity: 0.55;
		cursor: not-allowed;
	}

	.qa-icon {
		flex: none;
		display: grid;
		place-items: center;
		width: 2rem;
		height: 2rem;
		border-radius: 50%;
	}

	.qa-icon.boost {
		background: var(--status-heating-tint);
		color: var(--status-heating-text);
	}

	.qa-icon.resume {
		background: light-dark(var(--ice), var(--navy-border));
		color: var(--link);
	}

	.qa-text {
		display: grid;
		gap: 0.05rem;
		min-width: 0;
	}

	.qa-label {
		font-size: 0.9375rem;
		font-weight: var(--weight-semibold);
		line-height: 1.2;
	}

	.qa-sub {
		font-size: 0.75rem;
		line-height: 1.3;
		color: var(--text-muted);
	}

	.confirm {
		display: grid;
		gap: 0.75rem;
		padding: 0.875rem;
		border-radius: 18px;
		background: var(--surface);
		border: 1px solid var(--border);
		font-size: var(--text-small-size);
	}

	.confirm-row {
		display: grid;
		grid-template-columns: repeat(2, minmax(0, 1fr));
		gap: 0.625rem;
	}

	.problem-icon {
		flex: none;
		display: grid;
		place-items: center;
		width: 2rem;
		height: 2rem;
		border-radius: 50%;
		background: var(--tint);
		color: var(--ink);
	}

	.problem-icon.offline {
		border: 1px dashed var(--status-offline-border);
	}

	.tiles {
		display: grid;
		grid-template-columns: repeat(auto-fill, minmax(min(100%, 15rem), 1fr));
		gap: 1rem;
		margin: 0;
		padding: 0;
		list-style: none;
	}

	.tiles > li {
		display: grid;
	}

	/* Four tiles of the home and the plant fit one row on a wide screen. */
	.tiles.system {
		grid-template-columns: repeat(auto-fit, minmax(min(100%, 12rem), 1fr));
	}

	.ghost {
		min-height: 9.5rem;
		border-radius: var(--radius-lg);
		background: var(--panel);
	}

	.group-title {
		margin: 0.5rem 0.25rem 0;
	}

	.profile-menu {
		position: relative;
		display: grid;
	}

	.menu {
		position: absolute;
		z-index: 30;
		top: calc(100% + 0.4rem);
		left: 0;
		min-width: max(100%, 17rem);
		max-width: calc(100vw - 2rem);
		display: grid;
		gap: 0.15rem;
		padding: 0.4rem;
		background: var(--surface);
		border: 1px solid var(--border-strong);
		border-radius: var(--radius-lg);
		box-shadow: 0 0.75rem 2rem color-mix(in srgb, var(--navy) 22%, transparent);
	}

	.menu.end {
		left: auto;
		right: 0;
	}

	.menu hr {
		width: 100%;
		margin: 0.3rem 0;
		border: 0;
		border-top: 1px solid var(--border);
	}

	.item {
		display: flex;
		align-items: center;
		gap: 0.6rem;
		min-height: 2.75rem;
		padding: 0 0.75rem;
		border: 0;
		border-radius: var(--radius-md);
		background: none;
		color: var(--text);
		font-size: var(--text-small-size);
		font-weight: var(--weight-medium);
		text-align: left;
		text-decoration: none;
		cursor: pointer;
	}

	.item:hover {
		background: var(--panel);
		color: var(--text);
	}

	.item .grow {
		flex: 1 1 auto;
	}

	.radio {
		flex: none;
		width: 1rem;
		height: 1rem;
		border-radius: 50%;
		border: 2px solid var(--border-strong);
	}

	.item[aria-checked='true'] .radio {
		border: 5px solid var(--action);
	}

	.empty {
		display: grid;
		justify-items: center;
		gap: 0.75rem;
		padding: 1.75rem 1.25rem;
		text-align: center;
		background: var(--surface);
		border: 1px solid var(--border);
		border-radius: 24px;
	}

	.empty p {
		max-width: 32rem;
		color: var(--text-muted);
		font-size: var(--text-small-size);
	}

	.ghosts {
		display: grid;
		grid-template-columns: repeat(2, 3.25rem);
		gap: 0.5rem;
		margin-bottom: 0.25rem;
	}

	.ghosts span {
		height: 2.75rem;
		border-radius: 12px;
		border: 1.5px dashed var(--border-strong);
	}

	.ghosts .plus {
		display: grid;
		place-items: center;
		color: var(--link);
		border-color: var(--accent);
	}

	/* A phone shows two columns of tiles, like the apps people know. */
	@media (max-width: 559px) {
		.tiles,
		.tiles.system {
			grid-template-columns: repeat(2, minmax(0, 1fr));
			gap: 0.75rem;
		}
	}

	@media (min-width: 900px) {
		.actions {
			grid-template-columns: repeat(2, minmax(0, 16rem));
		}

		/* The rail shows the radio on a wide screen. */
		.head :global(.radio) {
			display: none;
		}
	}
</style>
