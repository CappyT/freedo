// A small Chrome DevTools Protocol client for the checks in this directory. It needs a
// Chrome or Chromium binary and no npm package: Node 22 has a WebSocket client.
import { spawn } from 'node:child_process';
import { existsSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const CANDIDATES = ['google-chrome', 'google-chrome-stable', 'chromium', 'chromium-browser', 'chrome'];

/** The Chrome binary: $CHROME, else the first candidate on PATH. Null when there is none. */
export function findChrome() {
	if (process.env.CHROME) return process.env.CHROME;
	for (const dir of (process.env.PATH ?? '').split(':')) {
		for (const name of CANDIDATES) if (existsSync(join(dir, name))) return join(dir, name);
	}
	return null;
}

export async function launch({ chrome = findChrome(), width = 1280, height = 900 } = {}) {
	if (!chrome) throw new Error('no Chrome binary found: set CHROME');
	const profile = mkdtempSync(join(tmpdir(), 'freedo-chrome-'));
	const child = spawn(
		chrome,
		['--headless=new', '--no-sandbox', '--disable-gpu', '--hide-scrollbars', '--remote-debugging-port=0',
			`--user-data-dir=${profile}`, 'about:blank'],
		{ stdio: ['ignore', 'ignore', 'pipe'] }
	);
	const endpoint = await new Promise((resolve, reject) => {
		let log = '';
		const timer = setTimeout(() => reject(new Error('Chrome did not start:\n' + log)), 20_000);
		child.stderr.on('data', (chunk) => {
			log += chunk;
			const match = /DevTools listening on (ws:\/\/\S+)/.exec(log);
			if (match) {
				clearTimeout(timer);
				resolve(match[1]);
			}
		});
		child.on('exit', () => reject(new Error('Chrome exited:\n' + log)));
	});

	const socket = new WebSocket(endpoint);
	await new Promise((resolve, reject) => {
		socket.onopen = resolve;
		socket.onerror = () => reject(new Error('cannot connect to ' + endpoint));
	});
	let nextId = 1;
	const pending = new Map();
	const listeners = [];
	socket.onmessage = ({ data }) => {
		const message = JSON.parse(String(data));
		if (message.id && pending.has(message.id)) {
			const { resolve, reject } = pending.get(message.id);
			pending.delete(message.id);
			if (message.error) reject(new Error(message.error.message));
			else resolve(message.result);
		} else for (const listener of listeners) listener(message);
	};
	const send = (method, params = {}, sessionId) =>
		new Promise((resolve, reject) => {
			const id = nextId++;
			pending.set(id, { resolve, reject });
			socket.send(JSON.stringify({ id, method, params, sessionId }));
		});

	const { targetId } = await send('Target.createTarget', { url: 'about:blank' });
	const { sessionId } = await send('Target.attachToTarget', { targetId, flatten: true });
	const call = (method, params) => send(method, params, sessionId);
	await call('Page.enable');
	await call('Runtime.enable');
	await call('Emulation.setDeviceMetricsOverride', { width, height, deviceScaleFactor: 1, mobile: false });

	const page = {
		call,
		/** Calls handler(params) for each event of this page with that method name. */
		on(method, handler) {
			listeners.push((m) => m.method === method && m.sessionId === sessionId && handler(m.params));
		},
		async navigate(url) {
			const loaded = new Promise((resolve) => {
				const listener = (m) => {
					if (m.method === 'Page.loadEventFired' && m.sessionId === sessionId) {
						listeners.splice(listeners.indexOf(listener), 1);
						resolve();
					}
				};
				listeners.push(listener);
			});
			await call('Page.navigate', { url });
			await loaded;
		},
		async evaluate(expression) {
			const { result, exceptionDetails } = await call('Runtime.evaluate', {
				expression,
				returnByValue: true,
				awaitPromise: true
			});
			if (exceptionDetails) throw new Error(exceptionDetails.exception?.description ?? exceptionDetails.text);
			return result.value;
		},
		/** Polls an expression until it is truthy. */
		async waitFor(expression, timeoutMs = 10_000) {
			const end = Date.now() + timeoutMs;
			for (;;) {
				const value = await page.evaluate(expression).catch(() => undefined);
				if (value) return value;
				if (Date.now() > end) throw new Error('timeout waiting for: ' + expression);
				await new Promise((r) => setTimeout(r, 100));
			}
		},
		async colorScheme(scheme) {
			await call('Emulation.setEmulatedMedia', { features: [{ name: 'prefers-color-scheme', value: scheme }] });
		},
		async viewport(w, h, mobile = false) {
			await call('Emulation.setDeviceMetricsOverride', { width: w, height: h, deviceScaleFactor: 1, mobile });
		},
		async language(tag) {
			await call('Emulation.setUserAgentOverride', { userAgent: '', acceptLanguage: tag });
			await call('Emulation.setLocaleOverride', { locale: tag }).catch(() => undefined);
		},
		/** Registers a script that runs before the scripts of every new document. */
		async init(source) {
			await call('Page.addScriptToEvaluateOnNewDocument', { source });
		},
		async screenshot(file, { fullPage = false } = {}) {
			let clip;
			if (fullPage) {
				const { cssContentSize: size } = await call('Page.getLayoutMetrics');
				clip = { x: 0, y: 0, width: size.width, height: size.height, scale: 1 };
			}
			const { data } = await call('Page.captureScreenshot', { format: 'png', clip, captureBeyondViewport: fullPage });
			writeFileSync(file, Buffer.from(data, 'base64'));
		}
	};

	return {
		page,
		async close() {
			socket.close();
			child.kill('SIGTERM');
			await new Promise((resolve) => {
				child.once('exit', resolve);
				setTimeout(resolve, 3000);
			});
			rmSync(profile, { recursive: true, force: true, maxRetries: 10, retryDelay: 200 });
		}
	};
}
