<script lang="ts">
	import { t } from './i18n.svelte.ts';

	interface Props {
		title: string;
		body: string;
		confirm: string;
		onconfirm: () => void;
	}

	let { title, body, confirm, onconfirm }: Props = $props();
	let dialog: HTMLDialogElement | undefined = $state();

	export function show() {
		dialog?.showModal();
	}
</script>

<dialog bind:this={dialog} aria-labelledby="confirm-title">
	<form method="dialog">
		<h2 id="confirm-title">{title}</h2>
		<p>{body}</p>
		<div class="row actions">
			<button class="btn" value="cancel">{t('action.cancel')}</button>
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

	.actions {
		justify-content: flex-end;
	}
</style>
