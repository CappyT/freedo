<script lang="ts">
	import Icon from './Icon.svelte';
	import { t } from './i18n.svelte.ts';
	import { toasts } from './toast.svelte.ts';
</script>

<div class="toasts" role="status" aria-live="polite">
	{#each toasts.list as toast (toast.id)}
		<div class="toast tone-{toast.kind === 'error' ? 'fault' : 'idle'}">
			<Icon name={toast.kind === 'error' ? 'fault' : 'check'} />
			<span>{toast.text}</span>
			<button class="close" onclick={() => toasts.dismiss(toast.id)} aria-label={t('action.dismiss')}>
				<Icon name="close" size={16} />
			</button>
		</div>
	{/each}
</div>

<style>
	.toasts {
		position: fixed;
		z-index: 30;
		right: 1rem;
		bottom: 1rem;
		display: grid;
		gap: 0.5rem;
		width: min(26rem, calc(100vw - 2rem));
		pointer-events: none;
	}

	.toast {
		pointer-events: auto;
		display: flex;
		align-items: flex-start;
		gap: 0.75rem;
		padding: 0.75rem 0.75rem 0.75rem 1rem;
		border-radius: var(--radius-md);
		background: var(--tint);
		color: var(--ink);
		border: 1px solid var(--dot);
		font-size: var(--text-small-size);
	}

	.toast span {
		flex: 1;
		padding-top: 0.1rem;
	}

	.close {
		border: 0;
		background: transparent;
		color: inherit;
		cursor: pointer;
		padding: 0.15rem;
		border-radius: var(--radius-xs);
	}

	@media (max-width: 899px) {
		.toasts {
			bottom: 5rem;
		}
	}
</style>
