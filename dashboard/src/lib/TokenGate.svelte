<script lang="ts">
	import { auth } from './auth.svelte.ts';
	import { data } from './data.svelte.ts';
	import Icon from './Icon.svelte';
	import { t } from './i18n.svelte.ts';

	let value = $state('');

	function submit(event: SubmitEvent) {
		event.preventDefault();
		if (value.trim() === '') return;
		auth.set(value);
		value = '';
		void data.refresh();
	}
</script>

<section class="gate panel" data-testid="token-gate">
	<h1>{t('token.title')}</h1>
	{#if auth.status === 'rejected'}
		<p class="notice tone-fault" role="alert">
			<Icon name="fault" />
			<span>{t('token.rejected')}</span>
		</p>
	{:else}
		<p>{t('token.required')}</p>
	{/if}
	<form onsubmit={submit} class="form">
		<label class="field">
			<span>{t('token.label')}</span>
			<input
				class="input mono"
				type="password"
				autocomplete="off"
				spellcheck="false"
				bind:value
				aria-invalid={auth.status === 'rejected'}
			/>
		</label>
		<button class="btn primary" type="submit"><Icon name="key" />{t('token.save')}</button>
	</form>
	<p class="note">{t('token.where')}</p>
</section>

<style>
	.gate {
		max-width: 32rem;
		margin-top: 2rem;
	}

	.form {
		display: grid;
		gap: 1rem;
	}
</style>
