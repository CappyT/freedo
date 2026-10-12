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
// - The schedule panel keeps the away program and an open editor across a data poll.
// - A renamed device shows its new name right after the save, also when a poll was in flight.
// - The buttons of a form row sit on the line of the controls, not of the labels.
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
	// While holdHealth is set, the answers of GET /api/health wait in held: a poll stays in flight.
	let holdHealth = false;
	const held = [];
	await tab.call('Fetch.enable', {
		patterns: [
			{ urlPattern: '*/api/*', requestStage: 'Request' },
			{ urlPattern: '*/api/zones', requestStage: 'Response' },
			{ urlPattern: '*/api/health', requestStage: 'Response' }
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
			if (path.endsWith('/api/health')) {
				if (holdHealth) held.push(p.requestId);
				else await tab.call('Fetch.continueResponse', { requestId: p.requestId });
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

	// The field of the daemon itself: Office has no schedule, and Living room got one above.
	const declared = (await (await fetch(daemon.base + '/api/zones')).json()).declared;
	if (declared.every((z) => z.kind === 'hotwater' || 'hasProgram' in z.target)) {
		await zone(() => {});
		zonesPatch = null;
		check((await rules()) === 'manual' && (await hint()), 'the daemon says Office has no program: "until I end it" and the hint');
		await open('#/zones/8', `document.querySelector('main h1')?.textContent === 'Living room'`);
		await tab.waitFor(`document.querySelector('.ends input:checked')`);
		check((await rules()) === 'until_next' && !(await hint()), 'the daemon says Living room has a program: the next block, no hint');
		await open('#/zones/3', `document.querySelector('main h1')?.textContent === 'Office'`);
		await tab.waitFor(`document.querySelector('.ends input:checked')`);
	} else {
		console.log('skipped: this daemon does not send target.hasProgram');
	}

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

	// 6. The active preset from the home page.
	const preset = await (
		await fetch(daemon.base + '/api/presets', {
			method: 'POST',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify({ name: 'Check preset' })
		})
	).json();
	await open('#/', `document.querySelectorAll('.card').length >= 6 && document.querySelector('input[name=presence]')`);
	await tab.waitFor(
		`[...document.querySelectorAll('.actions select.input option')].some((o) => o.textContent === 'Check preset')`,
		8000
	);
	check(true, 'the home page offers the presets');
	await tab.evaluate(`(() => {
		const s = document.querySelector('.actions select.input');
		s.value = '${preset.id}';
		s.dispatchEvent(new Event('change', { bubbles: true }));
	})()`);
	check(await toast('Sent to the daemon'), 'switching the active preset is sent');
	await sleep(500);
	const active = (await (await fetch(daemon.base + '/api/home')).json()).preset;
	check(active?.name === 'Check preset', `the daemon switched the active preset (${active?.name})`);

	// 7. The away program of a zone.
	await open('#/zones/8', `document.querySelector('main h1')?.textContent === 'Living room'`);
	await tab.waitFor(`document.querySelector('[name=schedule-mode-8]')`);
	check(
		(await tab.evaluate(`document.querySelector('.inforce .badge')?.textContent.trim()`)) === 'Home',
		'the panel shows the home program is in force at home'
	);
	await tab.evaluate(`document.querySelectorAll('[name=schedule-mode-8]')[1].click()`);
	await tab.waitFor(`[...document.querySelectorAll('button')].some((b) => b.textContent.includes('Create away program'))`);
	check(true, 'the zone page offers to create the away program');
	await clickText('Create away program');
	await tab.waitFor(`document.querySelector('.block input[type=number]')`);
	await clickText('Save schedule');
	check(await toast('Schedule saved'), 'the away program is saved');
	const away = await (await fetch(daemon.base + '/api/zones/8/schedule?mode=away')).json();
	check(away.programs?.length > 0, 'the daemon stores the away program');

	// 8. The schedule panel keeps the away program and an edit in progress across a data poll. The hot
	// water page passes its zone number from the model that each poll replaces.
	await open('#/hot-water', `document.querySelector('[name=schedule-mode-0]')`);
	await tab.evaluate(`document.querySelectorAll('[name=schedule-mode-0]')[1].click()`);
	await tab.waitFor(`[...document.querySelectorAll('button')].some((b) => b.textContent.includes('Create away program'))`);
	await clickText('Create away program');
	await tab.waitFor(`document.querySelector('.block input[type=number]')`);
	await tab.evaluate(`document.querySelector('.block input[type=number]').focus(); document.querySelector('.block input[type=number]').select()`);
	await type('45');
	await sleep(6500);
	check(
		await tab.evaluate(`document.querySelectorAll('[name=schedule-mode-0]')[1]?.checked === true`),
		'the away program stays selected after a data poll'
	);
	check(
		(await tab.evaluate(`document.querySelector('.block input[type=number]')?.value`)) === '45',
		'an edit in progress survives a data poll'
	);

	// 9. A renamed device shows its new name right after the save. A poll in flight when the daemon
	// takes the new label answers with the old one, so the page must read the state once more after it.
	await open('#/devices', `document.querySelector('.device strong')?.textContent === 'hall thermostat'`);
	await clickText('Rename');
	await tab.waitFor(`document.querySelector('.rename input')`);
	await tab.evaluate(`document.querySelector('.rename input').select()`);
	await type('Hall wall unit');
	holdHealth = true;
	for (let i = 0; held.length === 0; i++) {
		if (i > 120) throw new Error('no poll of /api/health while held');
		await sleep(50);
	}
	await click('.rename button[type=submit]');
	check(await toast('Device renamed'), 'the rename is accepted');
	holdHealth = false;
	for (const requestId of held.splice(0)) await tab.call('Fetch.continueResponse', { requestId });
	check(
		await tab
			.waitFor(`!document.querySelector('.rename') && document.querySelector('.device strong')?.textContent === 'Hall wall unit'`, 1500)
			.then(() => true, () => false),
		'the new name shows right after the save, with a poll in flight'
	);

	// 10. In a row with a labelled field, the buttons sit on the line of the controls, and an inline
	// rename keeps the field, Save and Cancel on one line. The page is 1280 px wide.
	const misaligned = `[...document.querySelectorAll('.row')].filter((row) => row.querySelector(':scope > .field')).flatMap((row) => {
		const control = row.querySelector(':scope > .field .input').getBoundingClientRect();
		return [...row.querySelectorAll(':scope > .btn, :scope > .segmented, :scope > .field .input')]
			.filter((el) => el.getBoundingClientRect().top < control.bottom && el.getBoundingClientRect().bottom > control.top)
			.filter((el) => Math.abs(el.getBoundingClientRect().bottom - control.bottom) > 1)
			.map((el) => el.textContent.trim() || el.className);
	})`;
	await open('#/', `document.querySelector('.actions select.input')`);
	const homeRow = await tab.evaluate(`(() => {
		const tops = [...document.querySelectorAll('.page-head .actions > .btn, .page-head .actions > .segmented, .page-head .actions .field select')].map((el) => Math.round(el.getBoundingClientRect().top));
		return { tops, lines: tops.filter((top, i) => tops.findIndex((other) => Math.abs(other - top) <= 2) === i).length };
	})()`);
	check(homeRow.lines === 1, `the preset select and the buttons of the home page share one line (${homeRow.tops})`);
	await open('#/settings', `document.querySelector('#presets-title')`);
	await tab.evaluate(`[...document.querySelectorAll('.presets button')].find((b) => b.textContent.includes('Rename')).click()`);
	await tab.waitFor(`document.querySelector('.presets form')`);
	const off = await tab.evaluate(misaligned);
	check(off.length === 0, `the buttons of the settings forms sit on the line of their controls (${off})`);
	const renameLine = await tab.evaluate(`new Set([...document.querySelectorAll('.presets form .input, .presets form .btn')].map((el) => Math.round(el.getBoundingClientRect().top))).size`);
	check(renameLine === 1, 'the preset rename keeps the field, Save and Cancel on one line');
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
