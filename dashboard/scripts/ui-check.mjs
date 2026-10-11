// Drives the controls of the dashboard in headless Chrome against the real daemon.
//
// The daemon runs with no radio and a seeded synthetic installation. The check rewrites some
// answers in the browser (the Chrome Fetch domain) to fail a command or to change a field, so
// it can test what the page does on a refusal and on the answers of a newer daemon.
//
// - Typing "21.5" into a temperature field of the schedule editor keeps every digit, and the
//   daemon stores the schedule.
// - A failed command puts the presence switch and the child lock back to what the daemon says.
// - A zone with no program starts on "until I end it" and shows a hint. Turn off asks first.
//
// Usage: FREEDO_BIN=./freedo node scripts/ui-check.mjs
import { findChrome, launch } from './browser.mjs';
import { startDaemon } from './harness.mjs';
import { seed } from './seed.mjs';

const bin = process.env.FREEDO_BIN;
if (!bin) {
	console.error('set FREEDO_BIN to a freedo binary that embeds the dashboard');
	process.exit(2);
}
const chrome = findChrome();
if (!chrome) {
	console.log(process.env.REQUIRE_BROWSER === '1' ? 'FAIL no Chrome found' : 'skipped: no Chrome found');
	process.exit(process.env.REQUIRE_BROWSER === '1' ? 1 : 0);
}

const failures = [];
const check = (ok, message) => {
	console.log((ok ? 'ok   ' : 'FAIL ') + message);
	if (!ok) failures.push(message);
};

const daemon = await startDaemon(bin);
let browser;
try {
	await seed(daemon.base);
	browser = await launch({ chrome });
	const tab = browser.page;
	await tab.language('en-US');

	// Requests the page made, and the rules that fail a request or change an answer.
	const requests = [];
	const failing = new Set();
	let zonesPatch = null;
	await tab.call('Fetch.enable', {
		patterns: [
			{ urlPattern: '*/api/*', requestStage: 'Request' },
			{ urlPattern: '*/api/zones', requestStage: 'Response' }
		]
	});
	tab.on('Fetch.requestPaused', async (p) => {
		const { method, url } = p.request;
		const path = new URL(url).pathname;
		try {
			if (p.responseStatusCode === undefined) {
				requests.push(`${method} ${path}`);
				const apiPath = path.slice(path.indexOf('/api/')).replace(/^\/api\/nodes\/[^/]+\//, '/api/nodes/*/');
				if (failing.has(`${method} ${apiPath}`)) {
					await tab.call('Fetch.fulfillRequest', {
						requestId: p.requestId,
						responseCode: 500,
						responseHeaders: [{ name: 'Content-Type', value: 'application/json' }],
						body: Buffer.from('{"error":"refused for the check"}').toString('base64')
					});
					return;
				}
				await tab.call('Fetch.continueRequest', { requestId: p.requestId });
				return;
			}
			if (zonesPatch === null) return void (await tab.call('Fetch.continueResponse', { requestId: p.requestId }));
			const { body, base64Encoded } = await tab.call('Fetch.getResponseBody', { requestId: p.requestId });
			const json = JSON.parse(base64Encoded ? Buffer.from(body, 'base64').toString() : body);
			for (const zone of json.declared) zonesPatch(zone);
			await tab.call('Fetch.fulfillRequest', {
				requestId: p.requestId,
				responseCode: p.responseStatusCode,
				responseHeaders: [{ name: 'Content-Type', value: 'application/json' }],
				body: Buffer.from(JSON.stringify(json)).toString('base64')
			});
		} catch {
			await tab.call('Fetch.continueRequest', { requestId: p.requestId }).catch(() => {});
		}
	});

	const root = daemon.base + '/';
	const click = (selector) => tab.evaluate(`document.querySelector(${JSON.stringify(selector)}).click()`);
	const clickText = (text) =>
		tab.evaluate(`(() => { const b = [...document.querySelectorAll('button')].find((x) => x.textContent.trim().startsWith(${JSON.stringify(text)})); if (!b) throw new Error('no button ${text}'); b.click(); })()`);
	const toast = (re) => tab.waitFor(`/${re}/.test(document.querySelector('.toasts')?.innerText || '')`, 6000).then(() => true, () => false);
	const open = async (route, ready) => {
		await tab.navigate(root + route);
		await tab.waitFor(ready, 10_000);
	};
	const type = async (text) => {
		for (const key of text) {
			await tab.call('Input.dispatchKeyEvent', { type: 'keyDown', key, text: key });
			await tab.call('Input.dispatchKeyEvent', { type: 'keyUp', key });
		}
	};
	const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

	// 1. The digits of a number typed into the schedule editor.
	await open('#/zones/8', `document.querySelector('main h1')?.textContent === 'Living room'`);
	await tab.waitFor(`[...document.querySelectorAll('button')].some((b) => b.textContent.includes('Create schedule'))`);
	await clickText('Create schedule');
	await tab.waitFor(`document.querySelector('.block input[type=number]')`);
	await tab.evaluate(`document.querySelector('.block input[type=number]').focus(); document.querySelector('.block input[type=number]').select()`);
	await type('21.5');
	const typed = await tab.evaluate(`document.querySelector('.block input[type=number]').value`);
	check(typed === '21.5', `typing 2, 1, ".", 5 leaves 21.5 in the field (${typed})`);
	check(
		await tab.evaluate(`![...document.querySelectorAll('button')].find((b) => b.textContent.includes('Save schedule')).disabled`),
		'the draft with 21.5 passes validation'
	);
	await clickText('Save schedule');
	check(await toast('Schedule saved'), 'the schedule is saved');
	const saved = await (await fetch(daemon.base + '/api/zones/8/schedule')).json();
	check(saved.programs?.[0]?.entries?.[0]?.setpoint_c === 21.5, `the daemon stores 21.5 (${JSON.stringify(saved.programs?.[0]?.entries?.[0])})`);

	// 2. The presence switch after a refused command.
	await open('#/', `document.querySelectorAll('.card').length >= 6 && document.querySelector('input[name=presence]')`);
	const presence = () => tab.evaluate(`[...document.querySelectorAll('input[name=presence]')].map((r) => r.checked)`);
	check(JSON.stringify(await presence()) === '[true,false]', 'the switch starts at home');
	failing.add('PATCH /api/home');
	await tab.evaluate(`document.querySelectorAll('input[name=presence]')[1].click()`);
	check(await toast('refused for the check'), 'the refusal is shown');
	await sleep(300);
	check(JSON.stringify(await presence()) === '[true,false]', 'the switch returns to home after a refused command');
	failing.clear();
	await tab.evaluate(`document.querySelectorAll('input[name=presence]')[1].click()`);
	await sleep(1200);
	check(JSON.stringify(await presence()) === '[false,true]', 'the switch keeps away after an accepted command');
	check((await (await fetch(daemon.base + '/api/home')).json()).away === true, 'the daemon is away');
	await tab.evaluate(`document.querySelectorAll('input[name=presence]')[0].click()`);
	await sleep(800);

	// 3. The child lock after a refused command.
	await open('#/devices', `document.querySelector('.switch input')`);
	failing.add('POST /api/nodes/*/childlock');
	await tab.evaluate(`document.querySelector('.switch input').click()`);
	check(await toast('refused for the check'), 'the refusal of the child lock is shown');
	await sleep(300);
	check(!(await tab.evaluate(`document.querySelector('.switch input').checked`)), 'the child lock returns to off after a refused command');
	failing.clear();

	// 4. The rule offered first, from target.hasProgram.
	const rules = () => tab.evaluate(`document.querySelector('.ends input:checked')?.value`);
	const hint = () => tab.evaluate(`document.querySelector('[data-testid="no-program"]') !== null`);
	const zone = async (patch) => {
		zonesPatch = patch;
		await open('#/zones/3', `document.querySelector('main h1')?.textContent === 'Office'`);
		await tab.waitFor(`document.querySelector('.ends input:checked')`);
		await sleep(400);
	};
	await zone((z) => { z.target.hasProgram = false; });
	check((await rules()) === 'manual' && (await hint()), 'a zone with no program starts on "until I end it" and shows the hint');
	await zone((z) => { z.target.hasProgram = true; });
	check((await rules()) === 'until_next' && !(await hint()), 'a zone with a program starts on the next block, with no hint');
	await zone((z) => { delete z.target.hasProgram; });
	check((await rules()) === 'until_next' && !(await hint()), 'a daemon that does not send the field behaves as before');
	await zone((z) => { z.target.hasProgram = false; });
	await click('.ends input[value="timer"]');
	await sleep(2200);
	check((await rules()) === 'timer', 'a rule that the person chose stays after the next poll');
	zonesPatch = null;

	// 5. Turn off asks first.
	requests.length = 0;
	await click('.ends input[value="manual"]');
	await clickText('Turn off');
	await tab.waitFor(`document.querySelector('dialog[open]')`);
	check(!requests.some((r) => r.endsWith('/off')), 'Turn off sends nothing before the person confirms');
	await tab.evaluate(`[...document.querySelectorAll('dialog[open] button')].find((b) => b.textContent.trim() === 'Cancel').click()`);
	await sleep(300);
	check(!requests.some((r) => r.endsWith('/off')), 'Cancel sends nothing');
	await clickText('Turn off');
	await tab.waitFor(`document.querySelector('dialog[open]')`);
	await tab.evaluate(`document.querySelector('dialog[open] button.primary').click()`);
	check(await toast('Sent to the daemon'), 'Turn off is sent after the person confirms');
	check(requests.filter((r) => r.endsWith('/off')).length === 1, 'the confirmed command is sent once');
} catch (error) {
	check(false, String(error?.stack ?? error));
} finally {
	for (const cleanup of [() => browser?.close(), () => daemon.stop()]) {
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
