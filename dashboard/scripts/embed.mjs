// Copies build/ into internal/web/dist, the directory the daemon embeds. Only .gitkeep stays.
import { cpSync, existsSync, readdirSync, rmSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

const build = fileURLToPath(new URL('../build/', import.meta.url));
const dist = fileURLToPath(new URL('../../internal/web/dist/', import.meta.url));

if (!existsSync(build + 'index.html')) {
	console.error('build/index.html is missing: run npm run build first');
	process.exit(1);
}
for (const entry of readdirSync(dist)) {
	if (entry !== '.gitkeep') rmSync(dist + entry, { recursive: true, force: true });
}
cpSync(build, dist, { recursive: true });
