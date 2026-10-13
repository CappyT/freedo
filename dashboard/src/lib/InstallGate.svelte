<script lang="ts">
	import Icon from './Icon.svelte';
	import { t } from './i18n.svelte.ts';
	import { install } from './install.svelte.ts';

	let value = $state('');

	function submit(event: SubmitEvent) {
		event.preventDefault();
		if (value.trim() === '') return;
		install.setToken(value);
		value = '';
	}
</script>

{#if install.needsToken}
	<section class="panel" aria-labelledby="install-gate-title" data-testid="install-gate">
		<h2 id="install-gate-title">{t('manage.token.title')}</h2>
		<p>{t('manage.token.body')}</p>
		<form class="row fill" onsubmit={submit}>
			<label class="field grow">
				<span>{t('token.label')}</span>
				<input class="input mono" type="password" autocomplete="off" spellcheck="false" bind:value />
			</label>
			<button class="btn primary" type="submit" disabled={value.trim() === ''}><Icon name="key" size={18} />{t('token.save')}</button>
		</form>
		<p class="note">{t('token.where')}</p>
	</section>
{/if}

<style>
	.grow {
		flex: 1 1 16rem;
	}
</style>
