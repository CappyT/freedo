// Proves that the build works under a path prefix that is unknown at build time.
//
// It starts the real daemon with the dashboard embedded (FREEDO_BIN) and a proxy that
// mounts the daemon under /api/hassio_ingress/<random>/, as Home Assistant ingress does:
// the proxy strips the prefix and sends X-Ingress-Path. The proxy answers 404 and records
// every request outside the prefix, so one absolute URL anywhere fails the check.
//
// 1. Every file that the page and its modules reference loads under the prefix.
// 2. A nested unknown path redirects to the root of the app, relative to the prefix.
// 3. With Chrome (CHROME, or on PATH), the app runs under the prefix: it asks for the
//    token, accepts one, shows the seeded zones and opens each page. The Backup section of Settings
//    downloads a backup and restores it through the prefix.
//
// Usage: FREEDO_BIN=./freedo node scripts/prefix-check.mjs
import { randomBytes } from 'node:crypto';
import http from 'node:http';
import { cancelRestore, confirmRestore, downloadBackup } from './backup-steps.mjs';
import { findChrome, launch } from './browser.mjs';
import { freePort, startDaemon } from './harness.mjs';
import { seed } from './seed.mjs';

const TOKEN = 'check-token-' + randomBytes(8).toString('hex');
const bin = process.env.FREEDO_BIN;
if (!bin) {
	console.error('set FREEDO_BIN to a freedo binary that embeds the dashboard');
	process.exit(2);
}

const failures = [];
const check = (ok, message) => {
	console.log((ok ? 'ok   ' : 'FAIL ') + message);
	if (!ok) failures.push(message);
};

const prefix = '/api/hassio_ingress/' + randomBytes(24).toString('base64url');
const escaped = [];
// The method and the path, without the prefix, of each request the proxy passed on.
const proxied = [];

function startProxy(port, target) {
	const server = http.createServer((req, res) => {
		if (!req.url.startsWith(prefix + '/') && req.url !== prefix) {
			escaped.push(req.method + ' ' + req.url);
			res.writeHead(404, { 'content-type': 'text/plain' });
			res.end('outside the prefix: ' + req.url);
			return;
		}
		proxied.push(req.method + ' ' + (req.url.slice(prefix.length) || '/'));
		const headers = { ...req.headers, host: `127.0.0.1:${target}`, 'x-ingress-path': prefix };
		const upstream = http.request(
			{ host: '127.0.0.1', port: target, method: req.method, path: req.url.slice(prefix.length) || '/', headers },
			(answer) => {
				res.writeHead(answer.statusCode, answer.headers);
				answer.pipe(res);
			}
		);
		upstream.on('error', (error) => {
			res.writeHead(502);
			res.end(String(error));
		});
		req.pipe(upstream);
	});
	return new Promise((resolve) => server.listen(port, '127.0.0.1', () => resolve(server)));
}

const proxyPort = await freePort();
const daemon = await startDaemon(bin, { token: TOKEN });
let proxy;
let browser;

try {
	await seed(daemon.base, TOKEN);
	proxy = await startProxy(proxyPort, daemon.port);
	const origin = `http://127.0.0.1:${proxyPort}`;
	const root = origin + prefix + '/';

	// 1. The files of the page.
	const page = await fetch(root);
	const html = await page.text();
	check(page.status === 200 && /text\/html/.test(page.headers.get('content-type') ?? ''), 'the page loads under the prefix');
	check(page.headers.get('cache-control') === 'no-cache', 'the HTML is not cached');
	check(!/(?:href|src)="\/(?!\/)|import\("\//.test(html), 'index.html has no root-relative URL');

	const seen = new Set();
	const queue = [];
	// A fragment such as url(#id) and a data: URI name no file.
	const refs = (text, base, pattern) =>
		[...text.matchAll(pattern)].filter((m) => !/^(#|data:)/.test(m[1])).map((m) => new URL(m[1], base).href);
	queue.push(...refs(html, root, /(?:href|src)="([^"#]+)"/g), ...refs(html, root, /import\("([^"]+)"\)/g));
	while (queue.length > 0) {
		const url = queue.shift();
		if (seen.has(url)) continue;
		seen.add(url);
		const answer = await fetch(url);
		const type = answer.headers.get('content-type') ?? '';
		const wanted = url.endsWith('.js') ? /javascript/ : url.endsWith('.css') ? /text\/css/ : /./;
		check(answer.status === 200 && wanted.test(type), `${url.slice(origin.length)} -> ${answer.status} ${type}`);
		if (/\.(js|css)$/.test(url)) {
			const text = await answer.text();
			queue.push(
				...refs(text, url, /["'](\.{1,2}\/[^"'\s]+\.(?:js|css|woff2|svg|png))["']/g),
				...refs(text, url, /url\(([^)"']+)\)/g)
			);
		} else await answer.arrayBuffer();
	}
	check(seen.size > 8, `the page references ${seen.size} files`);
	const immutable = await fetch([...seen].find((u) => u.includes('/_app/immutable/') && u.endsWith('.js')));
	check(/immutable/.test(immutable.headers.get('cache-control') ?? ''), 'hashed files are cached as immutable');

	// The fonts travel with their licence texts.
	for (const name of ['sora', 'ibm-plex-sans', 'ibm-plex-mono']) {
		const answer = await fetch(`${root}licenses/LICENSE-${name}.txt`);
		const text = await answer.text();
		check(
			answer.status === 200 && /^text\/plain/.test(answer.headers.get('content-type') ?? '') && /SIL OPEN FONT LICENSE/i.test(text),
			`the licence of ${name} is served with the bundle`
		);
	}

	// 2. Nested unknown paths go back to the root, whatever the prefix is.
	const nested = await fetch(root + 'zones/1', { redirect: 'manual' });
	const location = nested.headers.get('location') ?? '';
	check(
		nested.status === 302 && new URL(location, root + 'zones/1').href === root,
		`a nested path redirects to the app root (${nested.status} ${location})`
	);

	// The API keeps its rules behind the prefix.
	check((await fetch(root + 'api/zones')).status === 401, 'the API refuses a request without a token');
	check(
		(await fetch(root + 'api/zones', { headers: { Authorization: 'Bearer ' + TOKEN } })).status === 200,
		'the API answers a request with the token'
	);

	// 3. The running app.
	const chrome = findChrome();
	if (!chrome) {
		check(process.env.REQUIRE_BROWSER !== '1', 'no Chrome found: the browser check is skipped');
	} else {
		browser = await launch({ chrome });
		const tab = browser.page;
		await tab.language('en-US');
		const errors = [];
		await tab.call('Log.enable');
		tab.on('Runtime.exceptionThrown', (p) => errors.push(p.exceptionDetails.exception?.description ?? p.exceptionDetails.text));
		// Expected: the 401 answers before the token is entered, and the 404 of a zone with no schedule.
		tab.on('Log.entryAdded', (p) => p.entry.level === 'error' && !/401|\/schedule/.test(`${p.entry.text} ${p.entry.url ?? ''}`) && errors.push(`${p.entry.text} ${p.entry.url ?? ''}`));
		await tab.navigate(root);
		check(
			Boolean(await tab.waitFor(`document.querySelector('[data-testid="token-gate"]') !== null`)),
			'the app asks for the token when the daemon refuses the request'
		);
		const enter = (value) =>
			tab.evaluate(`(() => {
				const input = document.querySelector('[data-testid="token-gate"] input');
				input.value = ${JSON.stringify(value)};
				input.dispatchEvent(new Event('input', { bubbles: true }));
				document.querySelector('[data-testid="token-gate"] form').requestSubmit();
			})()`);
		await enter('not-the-token');
		check(
			Boolean(await tab.waitFor(`document.querySelector('[data-testid="token-gate"] [role="alert"]') !== null`)),
			'the app says that the daemon refused a wrong token'
		);
		await enter(TOKEN);
		check(
			Boolean(await tab.waitFor(`document.body.innerText.includes('Living room')`)),
			'the zones load through the prefix after the token is entered'
		);
		for (const route of ['#/zones/8', '#/hot-water', '#/boiler', '#/devices', '#/rooms/8', '#/setup', '#/settings', '#/']) {
			await tab.evaluate(`location.hash = ${JSON.stringify(route)}`);
			const ok = await tab
				.waitFor(`document.querySelector('main h1') !== null && !document.body.innerText.includes('Loading')`, 8000)
				.then(() => true, () => false);
			check(ok, `the page ${route} renders under the prefix`);
		}
		check(await tab.evaluate(`localStorage.getItem('freedo.token') === ${JSON.stringify(TOKEN)}`), 'the token is kept in browser storage');
		await tab.navigate(root + '#/zones/8');
		check(
			Boolean(await tab.waitFor(`document.querySelector('main h1')?.textContent === 'Living room'`)),
			'a reload on a zone page keeps the route and the token'
		);
		check(errors.length === 0, `the browser logged no error${errors.length ? ': ' + errors.join(' | ') : ''}`);

		// 4. The backup under the prefix: the download and the upload are relative to the page too.
		// The daemon restarts, so the proxy answers 502 for a moment and the browser logs it.
		const install = () => fetch(daemon.base + '/api/install', { headers: { Authorization: 'Bearer ' + TOKEN } }).then((r) => r.json());
		const before = await install();
		await tab.navigate(root + '#/settings');
		const backup = await downloadBackup(tab, check, 'under the prefix');
		const imports = () => proxied.filter((r) => r.startsWith('POST /api/state/import')).length;
		await cancelRestore(tab, check, 'under the prefix', backup, () => imports() > 0);
		await confirmRestore(tab, check, 'under the prefix', backup);
		check(imports() === 1 && proxied.includes('GET /api/state/export'), 'the backup calls go through the prefix');
		const after = await install();
		check(after.home_id === before.home_id && after.rev === before.rev, 'the daemon runs on the restored installation');
	}

	check(escaped.length === 0, `no request left the prefix${escaped.length ? ': ' + escaped.join(', ') : ''}`);
} catch (error) {
	check(false, String(error?.stack ?? error));
} finally {
	const cleanups = [
		() => browser?.close(),
		() => proxy?.close(),
		() => daemon.stop()
	];
	for (const cleanup of cleanups) {
		try {
			await cleanup();
		} catch (error) {
			console.error('cleanup failed:', error);
		}
	}
}

if (failures.length > 0) {
	console.error(`\n${failures.length} check(s) failed`);
	process.exit(1);
}
console.log('\nall checks passed');
process.exit(0);
