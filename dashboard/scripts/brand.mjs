// Copies the web icons from assets/brand into static/, so the brand files stay the only source.
import { copyFileSync, mkdirSync } from 'node:fs';
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
