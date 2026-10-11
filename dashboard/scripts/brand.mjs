// Copies the web icons from assets/brand and the font licences from src/lib/fonts into
// static/, so the sources stay the only copies in the repository.
import { copyFileSync, mkdirSync, readdirSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

const brand = fileURLToPath(new URL('../../assets/brand/', import.meta.url));
const out = fileURLToPath(new URL('../static/', import.meta.url));

const files = [
	['svg/favicon.svg', 'favicon.svg'],
	['web/favicon.ico', 'favicon.ico'],
	['web/apple-touch-icon.png', 'apple-touch-icon.png'],
	['web/icon-192.png', 'icon-192.png'],
	['web/icon-512.png', 'icon-512.png'],
	['web/icon-maskable-512.png', 'icon-maskable-512.png']
];

mkdirSync(out, { recursive: true });
for (const [from, to] of files) copyFileSync(brand + from, out + to);

// The Open Font Licence asks that the licence text accompany every copy of a font. The
// bundle holds the fonts, so it holds their licences, at licenses/ next to index.html.
const fonts = fileURLToPath(new URL('../src/lib/fonts/', import.meta.url));
mkdirSync(out + 'licenses', { recursive: true });
for (const name of readdirSync(fonts).filter((n) => n.startsWith('LICENSE-'))) {
	copyFileSync(fonts + name, out + 'licenses/' + name);
}
