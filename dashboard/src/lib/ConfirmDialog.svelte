<script lang="ts">
	import { t } from './i18n.svelte.ts';

	interface Props {
		title: string;
		body: string;
		/** A short list shown under the body, one entry per line. */
		items?: string[];
		confirm: string;
		onconfirm: () => void;
	}

	let { title, body, items = [], confirm, onconfirm }: Props = $props();
	let dialog: HTMLDialogElement | undefined = $state();
	let cancel: HTMLButtonElement | undefined = $state();

	// Cancel takes the focus, so Enter on an opened dialog never confirms by accident.
	export function show() {
		dialog?.showModal();
		cancel?.focus();
	}
</script>

<dialog bind:this={dialog} aria-labelledby="confirm-title">
	<form method="dialog">
		<h2 id="confirm-title">{title}</h2>
		<p>{body}</p>
		{#if items.length > 0}
			<ul class="items">
				{#each items as item (item)}
					<li>{item}</li>
				{/each}
			</ul>
		{/if}
		<div class="row actions">
			<button class="btn" value="cancel" bind:this={cancel}>{t('action.cancel')}</button>
			<button
				class="btn primary"
				value="ok"
				onclick={() => onconfirm()}
			>{confirm}</button>
		</div>
	</form>
</dialog>

<style>
	dialog {
		max-width: min(28rem, calc(100vw - 2rem));
		padding: 1.5rem;
		border: 1px solid var(--border-strong);
		border-radius: var(--radius-lg);
		background: var(--surface);
		color: var(--text);
	}

	dialog::backdrop {
		background: color-mix(in srgb, var(--navy) 55%, transparent);
	}

	form {
		display: grid;
		gap: 1rem;
	}

	.items {
		display: grid;
		gap: 0.35rem;
		margin: -0.25rem 0 0;
		padding-left: 1.25rem;
	}

	.actions {
		justify-content: flex-end;
	}
</style>
