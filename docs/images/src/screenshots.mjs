// Makes the README images in docs/images from the real dashboard. It seeds a daemon with the test
// installation, replays the test frames, sets a demo state, captures the pages in English and then
// renders the compositions of compose/ at 2x.
//
// Usage: node docs/images/src/screenshots.mjs <freedo binary that embeds the dashboard>

import { spawn } from 'node:child_process';
import { mkdirSync, mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { launch } from '../../../dashboard/scripts/browser.mjs';
import { freePort } from '../../../dashboard/scripts/harness.mjs';
import { seed } from '../../../dashboard/scripts/seed.mjs';

const here = (path) => fileURLToPath(new URL(path, import.meta.url));
const BIN = process.argv[2];
if (!BIN) {
	console.error('usage: node docs/images/src/screenshots.mjs <freedo binary>');
	process.exit(2);
}
const RAW = here('./raw/');
const OUT = here('../');
const REPLAY = here('../../../tado/testdata/frames_69ec.txt');
const WORK = mkdtempSync(join(tmpdir(), 'freedo-shots-'));
rmSync(RAW, { recursive: true, force: true });
mkdirSync(RAW, { recursive: true });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function startDaemon(extra = []) {
	const port = await freePort();
	const env = { ...process.env };
	delete env.FREEDO_API_TOKEN;
	const child = spawn(BIN, ['--addr', `127.0.0.1:${port}`, '--state', join(WORK, 'state.db'), '--unhandled', join(WORK, 'unhandled.log'), ...extra], {
		env,
		stdio: 'ignore'
	});
	const base = `http://127.0.0.1:${port}`;
	for (let i = 0; ; i++) {
		try {
			await fetch(base + '/api/health');
			break;
		} catch {
			if (i > 100) throw new Error('the daemon did not start');
			await sleep(100);
		}
	}
	return {
		base,
		async stop() {
			child.kill('SIGTERM');
			await new Promise((done) => {
				child.once('exit', done);
				setTimeout(done, 5000);
			});
		}
	};
}

async function call(base, method, path, body) {
	const response = await fetch(base + path, { method, headers: { 'Content-Type': 'application/json' }, body: body && JSON.stringify(body) });
	if (!response.ok) throw new Error(`${method} ${path}: ${response.status} ${await response.text()}`);
	return response;
}

// The demo installation: readable names, a schedule for every zone, two profiles.
async function prepare(base) {
	await seed(base);
	const labels = {
		'05a2cafa07c51b00': 'Hall thermostat',
		'02a6cefa07c51b00': 'Bedroom valve',
		'04a3defa07c51b00': 'Bath valve',
		'07a5cefa07c51b00': 'Hall valve',
		'0aa7cefa07c51b00': 'Living room window',
		'03a8cafa07c51b00': 'Living room wall',
		'010acefa07c51b00': 'Kitchen left',
		'060acefa07c51b00': 'Kitchen right',
		'08a4defa07c51b00': 'Office valve'
	};
	for (const [eui, label] of Object.entries(labels)) await call(base, 'PUT', `/api/install/devices/${eui}/label`, { label });
	const blocks = (list) => list.map(([minute, setpoint_c, enabled = true]) => ({ minute, setpoint_c, enabled }));
	const week = (weekday, weekend) => ({
		programs: [
			{ days: ['mon', 'tue', 'wed', 'thu', 'fri'], entries: blocks(weekday) },
			{ days: ['sat', 'sun'], entries: blocks(weekend) }
		]
	});
	const schedules = {
		1: week([[0, 17], [390, 20], [1350, 17]], [[0, 17], [450, 20], [1380, 17]]),
		2: week([[0, 18], [360, 22], [480, 19], [1110, 22], [1320, 18]], [[0, 18], [450, 22], [1320, 18]]),
		3: week([[0, 16], [480, 20.5], [1110, 16]], [[0, 16]]),
		5: week([[0, 17], [390, 20], [510, 18], [1020, 21], [1290, 17]], [[0, 17], [480, 20], [1290, 17]]),
		8: week([[0, 18], [390, 21], [510, 19], [1020, 21], [1350, 18]], [[0, 18], [480, 21], [1350, 18]]),
		10: week([[0, 17], [360, 19], [480, 17], [1290, 18.5]], [[0, 17], [480, 19], [1290, 18.5]])
	};
	for (const [zone, body] of Object.entries(schedules)) await call(base, 'PUT', `/api/zones/${zone}/schedule`, body);
	const hotWater = blocks([[0, 50, false], [360, 50], [540, 50, false], [1080, 50], [1350, 50, false]]);
	await call(base, 'PUT', '/api/zones/0/schedule', { programs: [{ days: ['mon', 'tue', 'wed', 'thu', 'fri', 'sat', 'sun'], entries: hotWater }] });
	const { presets } = await (await call(base, 'GET', '/api/presets')).json();
	await call(base, 'PATCH', `/api/presets/${presets[0].id}`, { name: 'Winter' });
	await call(base, 'POST', '/api/presets', { name: 'Holiday' });
}

// The replay holds no live state worth showing, so the answers of the API get a varied one.
const now = Date.now();
const temps = { 1: 19.8, 2: 21.6, 3: 17.1, 5: 19.6, 8: 20.4, 10: 18.9 };
const fromProgram = (targetC) => ({ on: true, targetC, programC: targetC, source: 'program', overlay: null, known: true, hasProgram: true });
const patches = {
	'/api/zones': (json) => {
		for (const z of json.declared) {
			if (z.number === 0) Object.assign(z.target, fromProgram(50));
			if (z.number === 1) Object.assign(z.target, fromProgram(20));
			if (z.number === 8) Object.assign(z.target, fromProgram(21));
			if (z.number === 10) Object.assign(z.target, fromProgram(18.5));
			if (z.number === 2) {
				z.window = { open: true, source: 'valve', since: new Date(now - 6 * 60_000).toISOString(), deadline: new Date(now + 9 * 60_000).toISOString(), undelivered: [] };
				Object.assign(z.target, { on: true, targetC: null, programC: 19, source: 'window', known: true, hasProgram: true });
			}
			if (z.number === 3) {
				Object.assign(z.target, { on: false, targetC: null, source: 'overlay', known: true, hasProgram: true });
				z.target.overlay = { kind: 'manual', rule: 'until_next', on: false, setpointC: null, endsAt: null };
			}
			if (z.number === 5) {
				Object.assign(z.target, { on: true, targetC: 22, programC: 18, source: 'overlay', known: true, hasProgram: true });
				z.target.overlay = { kind: 'manual', rule: 'timer', on: true, setpointC: 22, endsAt: new Date(now + 50 * 60_000).toISOString() };
			}
		}
		for (const demand of json.demand ?? []) {
			if (temps[demand.Number] !== undefined) Object.assign(demand, { TempC: temps[demand.Number], UpdatedUnix: Math.floor(now / 1000) });
			demand.DemandPct = demand.Number === 8 ? 64 : demand.Number === 5 ? 82 : 0;
		}
		return json;
	},
	'/api/health': (json) => {
		Object.assign(json, { mode: 'live', tx: true, txArmed: true, transmitAllowed: true, bridgePresent: false, calibrated: true, alerts: [] });
		delete json.radioError;
		delete json.warning;
		for (const device of json.devices ?? []) Object.assign(device, { offline: false, lowBattery: false });
		return json;
	},
	'/api/nodes': (json) => {
		for (const node of json) node.Telemetry = { ...(node.Telemetry ?? {}), fw_run_version: /^RU/.test(node.Serial ?? '') ? '102.4' : '87.3' };
		return json;
	},
	'/api/boiler': (json) =>
		Object.assign(json, {
			Present: true,
			FlowC: 52.5,
			ReturnC: 41,
			DHWC: 48,
			DHWSetpointC: 50,
			OutsideC: 6,
			ModulationPct: 38,
			Active: true,
			HeatActive: true,
			Mon: { ...(json.Mon ?? {}), boiler_flag: 10 },
			FaultCodes: [],
			FaultCode: 0
		})
};

async function capture(page, base) {
	await page.language('en-US');
	await page.call('Fetch.enable', { patterns: Object.keys(patches).map((path) => ({ urlPattern: '*' + path, requestStage: 'Response' })) });
	page.on('Fetch.requestPaused', async (paused) => {
		const path = new URL(paused.request.url).pathname;
		const patch = Object.entries(patches).find(([suffix]) => path.endsWith(suffix))?.[1];
		try {
			const { body, base64Encoded } = await page.call('Fetch.getResponseBody', { requestId: paused.requestId });
			const json = JSON.parse(base64Encoded ? Buffer.from(body, 'base64').toString() : body);
			await page.call('Fetch.fulfillRequest', {
				requestId: paused.requestId,
				responseCode: 200,
				responseHeaders: [{ name: 'Content-Type', value: 'application/json' }],
				body: Buffer.from(JSON.stringify(patch ? patch(json) : json)).toString('base64')
			});
		} catch {
			await page.call('Fetch.continueRequest', { requestId: paused.requestId }).catch(() => {});
		}
	});

	// file name, viewport, theme, route, and a script that runs after the load.
	const shots = [
		['home-phone-light', 'phone', 'light', '#/'],
		['room-phone-light', 'phone', 'light', '#/zones/8'],
		['programs-phone-light', 'phone', 'light', '#/programs'],
		['devices-phone-light', 'phone', 'light', '#/devices', `document.querySelectorAll('.device .head')[3]?.click()`],
		['home-phone-dark', 'phone', 'dark', '#/'],
		['room-phone-dark', 'phone', 'dark', '#/zones/8'],
		['home-desktop-dark', 'desktop', 'dark', '#/']
	];
	const viewports = {
		phone: { width: 390, height: 844, deviceScaleFactor: 3, mobile: true },
		desktop: { width: 1280, height: 800, deviceScaleFactor: 2, mobile: false }
	};
	for (const [name, viewport, scheme, route, script] of shots) {
		await page.call('Emulation.setDeviceMetricsOverride', viewports[viewport]);
		await page.colorScheme(scheme);
		await page.navigate(`${base}/${route}`);
		await sleep(2500);
		if (script) {
			await page.evaluate(script);
			await sleep(700);
		}
		await page.screenshot(join(RAW, name + '.png'));
		console.log('captured', name);
	}
}

async function render(page) {
	const compositions = [
		['hero', 1200, 600, 'hero'],
		['tour', 1200, 680, 'dashboard-tour'],
		['desktop', 1200, 720, 'dashboard-desktop']
	];
	for (const [source, width, height, name] of compositions) {
		await page.call('Emulation.setDeviceMetricsOverride', { width, height, deviceScaleFactor: 2, mobile: false });
		await page.navigate('file://' + here(`./compose/${source}.html`));
		await page.waitFor(`document.fonts.status === 'loaded' && [...document.images].every((i) => i.complete && i.naturalWidth > 0)`);
		await sleep(400);
		await page.screenshot(join(OUT, name + '.png'));
		console.log('rendered', name);
	}
}

let daemon = await startDaemon();
await prepare(daemon.base);
await daemon.stop();
daemon = await startDaemon(['--replay', REPLAY]);
await sleep(2000);
const browser = await launch({ width: 390, height: 844 });
try {
	await capture(browser.page, daemon.base);
	await render(browser.page);
} finally {
	await browser.close();
	await daemon.stop();
	rmSync(WORK, { recursive: true, force: true });
}
