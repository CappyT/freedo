// Makes build/index.html independent of where the page is mounted.
//
// SvelteKit writes the single-page entry with root-relative URLs ("/_app/..."), and its
// paths.relative option does not apply to that file. The daemon serves the page at its
// root, and Home Assistant ingress serves it under /api/hassio_ingress/<token>/. The
// prefix is not known at build time, so every URL must be relative to the document.
import { readFileSync, writeFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

const file = fileURLToPath(new URL('../build/index.html', import.meta.url));
const html = readFileSync(file, 'utf8').replaceAll('"/_app/', '"./_app/');

// A reference that starts with a single slash would escape the prefix.
const rooted = html.match(/(?:href|src)="\/(?!\/)[^"]*"|import\("\/[^"]*"\)/g);
if (rooted) {
	console.error('index.html still has root-relative URLs:\n  ' + rooted.join('\n  '));
	process.exit(1);
}
if (!html.includes('"./_app/')) {
	console.error('index.html has no asset reference: the build output changed shape');
	process.exit(1);
}
writeFileSync(file, html);
