<script lang="ts">
	import { page } from '$app/state';
	import { resolve } from '$app/paths';
	import { onMount } from 'svelte';
	import logo from '../../../assets/brand/svg/logo.svg?inline';
	import logoReversed from '../../../assets/brand/svg/logo-reversed.svg?inline';
	import '../app.css';
	import { auth } from '#lib/auth.svelte.ts';
	import Banners from '#lib/Banners.svelte';
	import { data } from '#lib/data.svelte.ts';
	import Icon, { type IconName } from '#lib/Icon.svelte';
	import { t, type Key } from '#lib/i18n.svelte.ts';
	import { prefs } from '#lib/prefs.svelte.ts';
	import Toasts from '#lib/Toasts.svelte';
	import TokenGate from '#lib/TokenGate.svelte';

	let { children } = $props();

	interface Item {
		href: string;
		label: Key;
		icon: IconName;
		active: boolean;
	}

	const route = $derived(page.route.id ?? '/');
	const items = $derived.by<Item[]>(() => {
		// Hot water has no item: its card on the home page leads to its page. First setup has none
		// either: the home page, Rooms and devices and Settings lead to it, so no item is active there.
		return [
			{ href: resolve('/'), label: 'nav.home', icon: 'home', active: route === '/' || route.startsWith('/zones') || route === '/hot-water' },
			{ href: resolve('/programs'), label: 'nav.programs', icon: 'calendar', active: route === '/programs' },
			{ href: resolve('/boiler'), label: 'nav.boiler', icon: 'flame', active: route === '/boiler' },
			{ href: resolve('/devices'), label: 'devices.title', icon: 'devices', active: route === '/devices' || route.startsWith('/rooms') },
			{ href: resolve('/settings'), label: 'nav.settings', icon: 'sliders', active: route === '/settings' }
		];
	});

	onMount(() => {
		data.start();
		return () => data.stop();
	});

	$effect(() => {
		document.documentElement.lang = prefs.locale;
		if (prefs.theme === 'system') delete document.documentElement.dataset.theme;
		else document.documentElement.dataset.theme = prefs.theme;
	});
</script>

<svelte:head>
	<title>freedo</title>
</svelte:head>

<a class="skip" href="#main-content" onclick={(e) => { e.preventDefault(); document.getElementById('main-content')?.focus(); }}>{t('nav.skip')}</a>

<div class="shell">
	<div class="brand">
		<a href={resolve('/')} aria-label="freedo">
			<img class="logo light" src={logo} alt="" width="120" height="28" />
			<img class="logo reversed" src={logoReversed} alt="" width="120" height="28" />
		</a>
	</div>
	<nav aria-label={t('nav.label')}>
		<ul>
			{#each items as item (item.href)}
				<li>
					<a href={item.href} aria-current={item.active ? 'page' : undefined} class:active={item.active}>
						<Icon name={item.icon} size={22} />
						<span>{t(item.label)}</span>
					</a>
				</li>
			{/each}
		</ul>
	</nav>
	<main id="main-content" tabindex="-1">
		{#if auth.blocked}
			<TokenGate />
		{:else}
			<Banners />
			{@render children()}
		{/if}
	</main>
</div>

<Toasts />

<style>
	.skip {
		position: absolute;
		left: 0.5rem;
		top: -4rem;
		z-index: 40;
		padding: 0.6rem 1rem;
		background: var(--action);
		color: var(--action-text);
		border-radius: var(--radius-md);
	}

	.skip:focus {
		top: 0.5rem;
	}

	.shell {
		display: grid;
		grid-template-columns: 15rem minmax(0, 1fr);
		grid-template-rows: auto 1fr;
		min-height: 100dvh;
	}

	.brand {
		grid-column: 1;
		padding: 1.5rem 1.25rem 1rem;
	}

	.logo {
		display: block;
		height: 1.75rem;
		width: auto;
	}

	.logo.reversed {
		display: none;
	}

	nav {
		grid-column: 1;
		padding: 0 0.75rem 1.5rem;
		position: sticky;
		top: 0;
		align-self: start;
	}

	nav ul {
		list-style: none;
		margin: 0;
		padding: 0;
		display: grid;
		gap: 0.25rem;
	}

	nav a {
		position: relative;
		display: flex;
		align-items: center;
		gap: 0.75rem;
		min-height: 2.75rem;
		padding: 0.5rem 0.9rem;
		border-radius: var(--radius-md);
		color: var(--text-muted);
		text-decoration: none;
		font-weight: var(--weight-medium);
		font-size: var(--text-small-size);
	}

	nav a:hover {
		color: var(--text);
		background: var(--panel);
	}

	nav a.active {
		color: var(--text);
		background: var(--panel);
	}

	nav a.active::before {
		content: '';
		position: absolute;
		left: 0;
		top: 0.65rem;
		bottom: 0.65rem;
		width: 3px;
		border-radius: var(--radius-pill);
		background: var(--accent);
	}

	main {
		grid-column: 2;
		grid-row: 1 / span 2;
		padding: 2rem clamp(1rem, 3vw, 2.5rem) 3rem;
		display: grid;
		gap: 1.25rem;
		align-content: start;
		min-width: 0;
		outline: none;
	}

	/* Phone and tablet: the logo sits on top and the navigation moves to the bottom edge. */
	@media (max-width: 899px) {
		.shell {
			grid-template-columns: minmax(0, 1fr);
			grid-template-rows: auto 1fr auto;
		}

		.brand {
			padding: 1rem 1rem 0;
		}

		main {
			grid-column: 1;
			grid-row: 2;
			padding: 1rem 1rem 2rem;
		}

		/* The bar is a row of the page that sticks to the bottom edge, so a label on two lines makes the bar higher and hides no content. */
		nav {
			grid-column: 1;
			grid-row: 3;
			position: sticky;
			z-index: 20;
			top: auto;
			bottom: 0;
			align-self: end;
			padding: 0;
			background: var(--surface);
			border-top: 1px solid var(--border);
		}

		nav ul {
			grid-auto-flow: column;
			grid-auto-columns: minmax(0, 1fr);
			gap: 0;
		}

		/* Every item takes the height of the highest one. */
		nav li {
			display: grid;
		}

		nav a {
			flex-direction: column;
			justify-content: center;
			gap: 0.15rem;
			min-height: 3.75rem;
			padding: 0.4rem 0.15rem;
			border-radius: 0;
			font-size: 0.75rem;
			line-height: 1.2;
			text-align: center;
		}

		nav a.active::before {
			left: 28%;
			right: 28%;
			top: 0;
			bottom: auto;
			width: auto;
			height: 3px;
		}
	}

	@media (prefers-color-scheme: dark) {
		:global(:root:not([data-theme='light'])) .logo.light {
			display: none;
		}

		:global(:root:not([data-theme='light'])) .logo.reversed {
			display: block;
		}
	}

	:global(:root[data-theme='dark']) .logo.light {
		display: none;
	}

	:global(:root[data-theme='dark']) .logo.reversed {
		display: block;
	}
</style>
