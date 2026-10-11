import { readStored, writeStored } from './storage.ts';

export const LOCALES = ['en', 'it', 'de', 'fr'] as const;
export type Locale = (typeof LOCALES)[number];
export type LocalePref = Locale | 'auto';
export type Theme = 'system' | 'light' | 'dark';

const THEME_KEY = 'freedo.theme';
const LOCALE_KEY = 'freedo.locale';

/** The first language of the list that the dashboard has, else English. */
export function detectLocale(languages: readonly string[]): Locale {
	for (const language of languages) {
		const base = language.slice(0, 2).toLowerCase();
		const match = LOCALES.find((l) => l === base);
		if (match) return match;
	}
	return 'en';
}

function storedTheme(): Theme {
	const value = readStored(THEME_KEY);
	return value === 'light' || value === 'dark' ? value : 'system';
}

function storedLocale(): LocalePref {
	const value = readStored(LOCALE_KEY);
	return LOCALES.find((l) => l === value) ?? 'auto';
}

class Prefs {
	theme = $state<Theme>(storedTheme());
	localePref = $state<LocalePref>(storedLocale());
	locale = $derived<Locale>(
		this.localePref === 'auto'
			? detectLocale(typeof navigator === 'undefined' ? [] : navigator.languages)
			: this.localePref
	);

	setTheme(theme: Theme) {
		this.theme = theme;
		writeStored(THEME_KEY, theme === 'system' ? null : theme);
	}

	setLocale(pref: LocalePref) {
		this.localePref = pref;
		writeStored(LOCALE_KEY, pref === 'auto' ? null : pref);
	}
}

export const prefs = new Prefs();
