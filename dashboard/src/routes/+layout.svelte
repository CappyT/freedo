<script lang="ts">
	import { onNavigate } from '$app/navigation';
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
	import RadioChip from '#lib/RadioChip.svelte';
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
		// Hot water and the boiler have no item: their tiles on the home page lead to their pages, so
		// Home stays active there. First setup has none either: the home page, Rooms and devices and
		// Settings lead to it, so no item is active there.
		return [
			{
				href: resolve('/'),
				label: 'nav.home',
				icon: 'home',
				active: route === '/' || route.startsWith('/zones') || route === '/hot-water' || route === '/boiler'
			},
			{ href: resolve('/programs'), label: 'nav.programs', icon: 'calendar', active: route === '/programs' },
			{ href: resolve('/devices'), label: 'nav.devices', icon: 'devices', active: route === '/devices' || route.startsWith('/rooms') },
			{ href: resolve('/settings'), label: 'nav.settings', icon: 'sliders', active: route === '/settings' }
		];
	});

	const waiting = $derived(data.waitSec !== null);

	onMount(() => {
		data.start();
		return () => data.stop();
	});

	// A view transition on every navigation: the tile and the screen it opens share the name hero
	// (app.css), and the rest of the page crossfades.
	onNavigate((navigation) => {
		if (!document.startViewTransition || matchMedia('(prefers-reduced-motion: reduce)').matches) return;
		return new Promise((done) => {
			document.startViewTransition(async () => {
				done();
				await navigation.complete;
			});
		});
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
	<nav aria-label={t('nav.label')} inert={waiting}>
		<ul>
			{#each items as item (item.href)}
				<li>
					<a href={item.href} aria-current={item.active ? 'page' : undefined} class:active={item.active}>
						<span class="pill"><Icon name={item.icon} size={22} /></span>
						<span>{t(item.label)}</span>
					</a>
				</li>
			{/each}
		</ul>
		<div class="radio"><RadioChip /></div>
	</nav>
	<main id="main-content" tabindex="-1">
		{#if auth.blocked}
			<TokenGate />
		{:else}
			<Banners />
			<!-- A daemon that waits after an import refuses every command, and a page that opens in the wait reads nothing. -->
			{#if !waiting || data.loaded}
				<div class="content" inert={waiting}>{@render children()}</div>
			{/if}
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

	/* Wide screen: a rail on the left with the logo, the items and the state of the radio. */
	.shell {
		display: grid;
		grid-template-columns: 15rem minmax(0, 1fr);
		grid-template-rows: auto 1fr;
		min-height: 100dvh;
	}

	.brand {
		grid-column: 1;
		grid-row: 1;
		padding: 1.5rem 1.25rem 1.25rem;
		background: var(--surface);
		border-right: 1px solid var(--border);
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
		grid-row: 2;
		display: flex;
		flex-direction: column;
		gap: 1rem;
		padding: 0 0.75rem 1.5rem;
		background: var(--surface);
		border-right: 1px solid var(--border);
	}

	nav ul {
		list-style: none;
		margin: 0;
		padding: 0;
		display: grid;
		gap: 0.25rem;
		position: sticky;
		top: 1rem;
	}

	nav a {
		display: flex;
		align-items: center;
		gap: 0.75rem;
		min-height: 2.875rem;
		padding: 0 0.75rem;
		border-radius: var(--radius-md);
		color: var(--text-muted);
		text-decoration: none;
		font-weight: var(--weight-medium);
		font-size: 0.9375rem;
	}

	nav a:hover {
		color: var(--text);
		background: var(--panel);
	}

	nav a.active {
		color: var(--text);
		font-weight: var(--weight-semibold);
		background: light-dark(var(--ice), var(--navy-border));
	}

	nav a.active .pill {
		color: var(--link);
	}

	.pill {
		display: grid;
		place-items: center;
	}

	.radio {
		margin-top: auto;
		padding: 0 0.25rem;
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

	.content {
		display: grid;
		min-width: 0;
	}

	.content[inert],
	nav[inert] {
		opacity: 0.5;
	}

	/* Phone and tablet: no logo, and four items at the bottom edge, like the apps people know. */
	@media (max-width: 899px) {
		.shell {
			grid-template-columns: minmax(0, 1fr);
			grid-template-rows: 1fr auto;
		}

		.brand {
			display: none;
		}

		main {
			grid-column: 1;
			grid-row: 1;
			padding: 1rem 1rem 2rem;
		}

		/* The bar is a row of the page that sticks to the bottom edge, so a label on two lines makes the bar higher and hides no content. */
		nav {
			grid-column: 1;
			grid-row: 2;
			position: sticky;
			z-index: 20;
			bottom: 0;
			align-self: end;
			padding: 0.375rem 0.375rem calc(0.5rem + env(safe-area-inset-bottom));
			border-right: 0;
			border-top: 1px solid var(--border);
		}

		nav ul {
			position: static;
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
			gap: 0.2rem;
			min-height: 3.5rem;
			padding: 0.25rem 0.15rem;
			border-radius: 0;
			font-size: 0.75rem;
			line-height: 1.2;
			text-align: center;
		}

		nav a:hover,
		nav a.active {
			background: none;
		}

		.pill {
			width: 3.625rem;
			height: 1.875rem;
			border-radius: var(--radius-pill);
		}

		nav a.active .pill {
			background: light-dark(var(--ice), var(--navy-border));
		}

		.radio {
			display: none;
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
