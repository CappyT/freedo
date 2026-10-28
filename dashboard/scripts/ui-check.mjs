// Drives the controls of the dashboard in headless Chrome against the real daemon.
//
// The daemon runs with no radio and a seeded synthetic installation. The check rewrites some
// answers in the browser (the Chrome Fetch domain) to fail a command or to change a field, so
// it can test what the page does on a refusal and on the answers of a newer daemon.
//
// - Typing "21.5" into a temperature field of the schedule editor keeps every digit, and the
//   daemon stores the schedule.
// - A failed command leaves the presence tile and the child lock at what the daemon says.
// - A zone with no program starts on "until I end it" and shows a hint. Turn off asks first.
// - The schedule panel keeps the away program and an open editor across a data poll.
// - A device renamed in the settings of its room shows its new name right after the save, also when
//   a poll was in flight.
// - The buttons of a form row sit on the line of the controls, not of the labels.
// - The Programs screen creates a profile, edits one that is not active and activates it.
// - Rooms and devices adds a room and pairs a device that the pair window heard. A pairing in progress
//   and a pairing that ended each replace the steps, and a device that joined is not offered again. A refused
//   room stops the run before the pair command, and the end follows the room that the daemon holds. The settings of a
//   room restart a device after a confirm, move a device, show the effects of a removal before the delete, offer only free devices,
//   and rename and delete the room. No room is the group of the devices in no room.
// - The settings of a heated room save its options in one request and keep the ranges of the daemon.
// - The room screen shows the room temperature that Home Assistant pushes: the source, the reading, its age and the
//   method. A push that the method None refuses shows as a warning there and in the correction field.
// - The burner badge of the boiler page follows the burner flag, also while no room asks for heat.
//   A boiler fault code shows on the boiler page and as a banner.
// - The boiler setup saves the energy data and the flow options, with a comma as the decimal separator,
//   and refuses the default curve without a highest flow temperature. With a fuel flow set, the boiler
//   page saves a meter reading, refuses a lower one before it is sent and undoes the last after a confirm.
// - The statistics are off by default. Settings turns them on; the room report and the consumption page draw
//   a sample, the price takes a comma, and off asks first and keeps the data until a delete. The room report
//   lists the heating periods of a room with a flat target and names the return home and the target rise.
//   A stopped fuel counter names its reason.
// - The first setup takes a daemon with an empty database to a room, a paired thermostat and a circuit.
//   The device that the pair window hears and the frames of the joined device are put into the
//   answers here: the daemon has no radio. As the daemon does, the answers keep the device in the
//   list after the window closes.
// - The Backup section of Settings tells a user who is not an administrator why the daemon refuses, and
//   downloads a database file. A restore asks first, sends nothing on
//   Cancel, and on a confirm follows the restart of the daemon without an error, to the installation
//   of the backup. After the import the check answers for some seconds as a daemon that waits before
//   its start: the banner and the section count the same seconds, and the section says restored after them.
// - A daemon that waits after an import: every page shows one banner that counts down, keeps its last
//   data, sends no command, and returns to normal by itself. A page that opens in the wait shows the banner alone.
//
// Usage: FREEDO_BIN=./freedo node scripts/ui-check.mjs
import { cancelRestore, confirmRestore, downloadBackup } from './backup-steps.mjs';
import { findChrome, launch } from './browser.mjs';
import { startDaemon, waitingAnswer } from './harness.mjs';
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
// The daemon of the first setup check: an empty database.
let fresh;
try {
	await seed(daemon.base);
	browser = await launch({ chrome });
	const tab = browser.page;
	await tab.language('en-US');

	// Requests the page made, and the rules that fail a request or change an answer.
	const requests = [];
	// The body of each request that carried one, by method and path.
	const bodies = new Map();
	const failing = new Set();
	// The calls that the daemon refuses with 403, as it does under Home Assistant for a user who is no administrator.
	const forbidden = new Set();
	let zonesPatch = null;
	// While holdHealth is set, the answers of GET /api/health wait in held: a poll stays in flight.
	let holdHealth = false;
	const held = [];
	// The device that the pair window reports, as if the radio heard it. Null adds none.
	let joiner = null;
	const hear = (device) => {
		const at = new Date().toISOString();
		joiner = { ...device, label: '', firstSeen: at, lastSeen: at, frames: 3, lastClass: 'BeaconReq' };
	};
	// The short address of a device that the node view reports as joined. As on a plant, the daemon read
	// a frame of it and a request carried the session, and the time of the session is older than the join.
	let joinedShort = null;
	const sessionBefore = Math.floor(Date.now() / 1000) - 3600;
	// Fields put into the answer of GET /api/boiler, as if the boiler reported them. Null changes nothing.
	let boilerPatch = null;
	// The samples put into the answer of GET /api/history/zones/{n}, built from the bounds of the day. Null changes nothing.
	let zoneDayPatch = null;
	// Until waitUntil every answer is the one of a daemon that waits after an import. An import that
	// passes starts such a wait of waitAfterImport milliseconds.
	let waitUntil = 0;
	let waitAfterImport = 0;
	const fulfill = (p, json) =>
		tab.call('Fetch.fulfillRequest', {
			requestId: p.requestId,
			responseCode: p.responseStatusCode,
			responseHeaders: [{ name: 'Content-Type', value: 'application/json' }],
			body: Buffer.from(JSON.stringify(json)).toString('base64')
		});
	const bodyOf = async (p) => {
		const { body, base64Encoded } = await tab.call('Fetch.getResponseBody', { requestId: p.requestId });
		return JSON.parse(base64Encoded ? Buffer.from(body, 'base64').toString() : body);
	};
	await tab.call('Fetch.enable', {
		patterns: [
			{ urlPattern: '*/api/*', requestStage: 'Request' },
			{ urlPattern: '*/api/zones', requestStage: 'Response' },
			{ urlPattern: '*/api/health', requestStage: 'Response' },
			{ urlPattern: '*/api/pair/window', requestStage: 'Response' },
			{ urlPattern: '*/api/nodes', requestStage: 'Response' },
			{ urlPattern: '*/api/boiler', requestStage: 'Response' },
			{ urlPattern: '*/api/history/zones/*', requestStage: 'Response' }
		]
	});
	tab.on('Fetch.requestPaused', async (p) => {
		const { method, url } = p.request;
		const path = new URL(url).pathname;
		try {
			if (p.responseStatusCode === undefined) {
				requests.push(`${method} ${path}`);
				if (p.request.postData !== undefined) bodies.set(`${method} ${path}`, p.request.postData);
				const apiPath = path.slice(path.indexOf('/api/')).replace(/^\/api\/nodes\/[^/]+\//, '/api/nodes/*/');
				const left = Math.ceil((waitUntil - Date.now()) / 1000);
				if (left > 0) {
					const answer = waitingAnswer(path, left);
					await tab.call('Fetch.fulfillRequest', {
						requestId: p.requestId,
						responseCode: answer.status,
						responseHeaders: Object.entries(answer.headers).map(([name, value]) => ({ name, value })),
						body: Buffer.from(answer.body).toString('base64')
					});
					return;
				}
				if (forbidden.has(`${method} ${apiPath}`)) {
					await tab.call('Fetch.fulfillRequest', {
						requestId: p.requestId,
						responseCode: 403,
						responseHeaders: [{ name: 'Content-Type', value: 'application/json' }],
						body: Buffer.from('{"error":"only a Home Assistant administrator can export or import the state"}').toString('base64')
					});
					return;
				}
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
				if (waitAfterImport > 0 && method === 'POST' && path.endsWith('/api/state/import')) waitUntil = Date.now() + waitAfterImport;
				return;
			}
			if (path.endsWith('/api/health')) {
				if (holdHealth) held.push(p.requestId);
				else await tab.call('Fetch.continueResponse', { requestId: p.requestId });
				return;
			}
			if (path.endsWith('/api/boiler')) {
				if (boilerPatch === null) return void (await tab.call('Fetch.continueResponse', { requestId: p.requestId }));
				await fulfill(p, { ...(await bodyOf(p)), ...boilerPatch });
				return;
			}
			if (path.includes('/api/history/zones/')) {
				if (zoneDayPatch === null) return void (await tab.call('Fetch.continueResponse', { requestId: p.requestId }));
				const json = await bodyOf(p);
				await fulfill(p, { ...json, samples: zoneDayPatch(Date.parse(json.from)), episodes: [] });
				return;
			}
			if (path.endsWith('/api/nodes')) {
				if (joinedShort === null) return void (await tab.call('Fetch.continueResponse', { requestId: p.requestId }));
				const nodes = await bodyOf(p);
				for (const node of nodes) {
					if (node.Short !== joinedShort) continue;
					node.RxCounter = 7;
					node.LastSeenUnix = Math.floor(Date.now() / 1000);
					node.SessionIssued = true;
					node.SessionAdopted = true;
					node.SessionIssuedUnix = sessionBefore;
				}
				await fulfill(p, nodes);
				return;
			}
			if (path.endsWith('/api/pair/window')) {
				const json = await bodyOf(p);
				if (joiner) json.joiners = [joiner];
				await fulfill(p, json);
				return;
			}
			if (zonesPatch === null) return void (await tab.call('Fetch.continueResponse', { requestId: p.requestId }));
			const json = await bodyOf(p);
			for (const zone of json.declared) zonesPatch(zone);
			await fulfill(p, json);
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

	// 2. The presence tile after a refused command. The tile is pressed while the home is away.
	const homeReady = `document.querySelectorAll('.tile').length >= 6 && document.querySelector('[data-testid=presence]')`;
	await open('#/', homeReady);
	const presence = () => tab.evaluate(`document.querySelector('[data-testid=presence]').getAttribute('aria-pressed')`);
	check((await presence()) === 'false', 'the presence tile starts at home');
	failing.add('PATCH /api/home');
	await click('[data-testid=presence]');
	check(await toast('refused for the check'), 'the refusal is shown');
	await sleep(300);
	check((await presence()) === 'false', 'the presence tile stays at home after a refused command');
	failing.clear();
	await click('[data-testid=presence]');
	await sleep(1200);
	check((await presence()) === 'true', 'the presence tile shows away after an accepted command');
	check((await (await fetch(daemon.base + '/api/home')).json()).away === true, 'the daemon is away');
	await click('[data-testid=presence]');
	await sleep(800);

	// 3. The child lock after a refused command.
	// A device row opens on a tap: open them all, so the switch of the first valve shows.
	await open('#/devices', `document.querySelector('.device .head')`);
	await tab.evaluate(`document.querySelectorAll('.device .head[aria-expanded=false]').forEach((b) => b.click())`);
	await tab.waitFor(`document.querySelector('.switch input')`);
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
	check(await toast('Messages waiting for the device'), 'Turn off is sent after the person confirms');
	check(requests.filter((r) => r.endsWith('/off')).length === 1, 'the confirmed command is sent once');

	// 6. The active preset from the home page.
	const preset = await (
		await fetch(daemon.base + '/api/presets', {
			method: 'POST',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify({ name: 'Check preset' })
		})
	).json();
	await open('#/', homeReady);
	await click('.profile-menu .menu-button');
	await tab.waitFor(
		`[...document.querySelectorAll('.menu [role=menuitemradio]')].some((o) => o.textContent.includes('Check preset'))`,
		8000
	);
	check(true, 'the home page offers the profiles');
	await tab.evaluate(`[...document.querySelectorAll('.menu [role=menuitemradio]')].find((o) => o.textContent.includes('Check preset')).click()`);
	check(await toast('Messages waiting for the device'), 'switching the active preset is sent');
	await sleep(500);
	const active = (await (await fetch(daemon.base + '/api/home')).json()).preset;
	check(active?.name === 'Check preset', `the daemon switched the active preset (${active?.name})`);

	// On a phone the profile tile sits in the right column, so its menu opens from the right edge.
	await tab.viewport(390, 844, true);
	await open('#/', homeReady);
	await click('.profile-menu .menu-button');
	await tab.waitFor(`document.querySelector('.profile-menu .menu')`, 4000);
	const menuBox = await tab.evaluate(
		`(() => { const r = document.querySelector('.profile-menu .menu').getBoundingClientRect(); return [Math.round(r.left), Math.round(r.right), window.innerWidth, document.documentElement.scrollWidth]; })()`
	);
	check(menuBox[0] >= 0 && menuBox[1] <= menuBox[2] && menuBox[3] <= menuBox[2], `on a phone the profile menu stays on the screen (${menuBox[0]} to ${menuBox[1]} of ${menuBox[2]})`);
	await tab.viewport(1280, 900);

	// 7. The away program of a zone.
	await open('#/zones/8', `document.querySelector('main h1')?.textContent === 'Living room'`);
	await tab.waitFor(`document.querySelector('[name=schedule-mode-8]')`);
	check(
		(await tab.evaluate(`document.querySelector('.inforce .badge')?.textContent.trim()`)) === 'Home',
		'the panel shows the home program is in force at home'
	);
	await tab.evaluate(`document.querySelectorAll('[name=schedule-mode-8]')[1].click()`);
	await tab.waitFor(`[...document.querySelectorAll('button')].some((b) => b.textContent.includes('Create away schedule'))`);
	check(true, 'the zone page offers to create the away program');
	await clickText('Create away schedule');
	await tab.waitFor(`document.querySelector('.block input[type=number]')`);
	await clickText('Save schedule');
	check(await toast('Schedule saved'), 'the away program is saved');
	const away = await (await fetch(daemon.base + '/api/zones/8/schedule?mode=away')).json();
	check(away.programs?.length > 0, 'the daemon stores the away program');

	// 8. The schedule panel keeps the away program and an edit in progress across a data poll. The hot
	// water page passes its zone number from the model that each poll replaces.
	await open('#/hot-water', `document.querySelector('[name=schedule-mode-0]')`);
	await tab.evaluate(`document.querySelectorAll('[name=schedule-mode-0]')[1].click()`);
	await tab.waitFor(`[...document.querySelectorAll('button')].some((b) => b.textContent.includes('Create away schedule'))`);
	await clickText('Create away schedule');
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

	// 9. A device is renamed in the settings of its room. The form keeps its text across a data poll,
	// and the new name shows right after the save, also when a poll is in flight.
	await open('#/rooms/1', `document.querySelector('[data-member="hall thermostat"]')`);
	await tab.evaluate(`[...document.querySelector('[data-member="hall thermostat"]').querySelectorAll('button')].find((b) => b.textContent.trim() === 'Rename').click()`);
	await tab.waitFor(`document.querySelector('.rename input')`);
	await tab.evaluate(`document.querySelector('.rename input').select()`);
	await type('Hall wall unit');
	await sleep(6500);
	check(
		(await tab.evaluate(`document.querySelector('.rename input')?.value`)) === 'Hall wall unit',
		'the rename form of a device keeps its text across a data poll'
	);
	const renameTopsDevice = await tab.evaluate(`[...document.querySelectorAll('.rename .input, .rename .btn')].map((el) => Math.round(el.getBoundingClientRect().top))`);
	check(new Set(renameTopsDevice).size === 1, `the device rename keeps the field, Save and Cancel on one line (${renameTopsDevice})`);
	holdHealth = true;
	for (let i = 0; held.length === 0; i++) {
		if (i > 120) throw new Error('no poll of /api/health while held');
		await sleep(50);
	}
	requests.length = 0;
	await click('.rename button[type=submit]');
	check(await toast('Device renamed'), 'the rename is accepted');
	check(requests.some((r) => /^PUT \/api\/install\/devices\/[^/]+\/label$/.test(r)), 'the rename uses the route that carries the label alone');
	holdHealth = false;
	for (const requestId of held.splice(0)) await tab.call('Fetch.continueResponse', { requestId });
	check(
		await tab
			.waitFor(`!document.querySelector('.rename') && document.querySelector('[data-member="Hall wall unit"] strong')?.textContent === 'Hall wall unit'`, 1500)
			.then(() => true, () => false),
		'the new name shows right after the save, with a poll in flight'
	);
	await open('#/devices', `[...document.querySelectorAll('.device strong')].some((s) => s.textContent === 'Hall wall unit')`);
	check(true, 'the page Rooms and devices shows the new name');
	check(
		await tab.evaluate(`document.querySelectorAll('.device select, .device .btn.danger').length === 0 && ![...document.querySelectorAll('.device button')].some((b) => /Rename|Remove/.test(b.textContent))`),
		'a device row holds no rename, no room selector and no removal'
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
	await open('#/', `document.querySelector('.profile-menu .menu-button')`);
	const homeRow = await tab.evaluate(`(() => {
		const tops = [...document.querySelectorAll('.home .actions > .qa')].map((el) => Math.round(el.getBoundingClientRect().top));
		return { tops, lines: tops.filter((top, i) => tops.findIndex((other) => Math.abs(other - top) <= 2) === i).length };
	})()`);
	check(homeRow.tops.length === 2 && homeRow.lines === 1, `the two quick actions of the home page share one line (${homeRow.tops})`);
	await open('#/settings', `document.querySelector('#token-title')`);
	await click('[data-testid=token-row]');
	await tab.waitFor(`document.querySelector('.token form')`);
	const offSettings = await tab.evaluate(misaligned);
	check(offSettings.length === 0, `the buttons of the settings forms sit on the line of their controls (${offSettings})`);
	await open('#/programs', `document.querySelector('.matrix')`);
	await clickText('Rename');
	await tab.waitFor(`document.querySelector('#form-title')`);
	const off = await tab.evaluate(misaligned);
	check(off.length === 0, `the buttons of the profile form sit on the line of their controls (${off})`);
	const renameTops = await tab.evaluate(`[...document.querySelectorAll('[aria-labelledby=form-title] form .input, [aria-labelledby=form-title] form .btn')].map((el) => Math.round(el.getBoundingClientRect().top))`);
	check(new Set(renameTops).size === 1, `the profile rename keeps the field, Save and Cancel on one line (${renameTops})`);

	// 11. Profiles on the Programs screen: a new profile, an edit of a profile that is not active,
	// and the activation, which names the rooms that change now.
	const typeInto = (selector, value) =>
		tab.evaluate(`(() => { const i = document.querySelector(${JSON.stringify(selector)}); i.value = ${JSON.stringify(value)}; i.dispatchEvent(new Event('input', { bubbles: true })); })()`);
	await open('#/', `document.querySelector('.profile-menu .menu-button')`);
	await click('.profile-menu .menu-button');
	await tab.waitFor(`document.querySelector('.menu a[href*="new"]')`);
	await click('.menu a[href*="new"]');
	await tab.waitFor(`document.querySelector('#form-title')?.textContent === 'New profile'`);
	check(true, 'New profile in the home menu opens the form of the Programs screen');
	await typeInto('main form input', 'Spring');
	await clickText('Create profile');
	check(await toast('Profile created'), 'the new profile is created');
	await tab.waitFor(`document.querySelector('#selected-title')?.textContent === 'Spring' && document.querySelector('.matrix')`);
	check(await tab.evaluate(`document.querySelector('main .notice.tone-away') !== null`), 'the screen says the profile it edits is not active');
	const spring = (await (await fetch(daemon.base + '/api/presets')).json()).presets.find((p) => p.name === 'Spring');
	await tab.evaluate(`[...document.querySelectorAll('.zone')].find((z) => z.textContent.includes('Living room')).querySelector('button').click()`);
	await tab.waitFor(`[...document.querySelectorAll('.editor button')].some((b) => b.textContent.includes('Edit schedule'))`);
	await tab.evaluate(`[...document.querySelectorAll('.editor button')].find((b) => b.textContent.includes('Edit schedule')).click()`);
	await tab.waitFor(`document.querySelector('.editor .block input[type=number]')`);
	// Every block at 19 C, so the block in force of Spring is 19 C at any time of the day.
	const blocks = await tab.evaluate(`document.querySelectorAll('.editor .block input[type=number]').length`);
	for (let i = 0; i < blocks; i++) {
		await tab.evaluate(`document.querySelectorAll('.editor .block input[type=number]')[${i}].focus(); document.querySelectorAll('.editor .block input[type=number]')[${i}].select()`);
		await type('19');
	}
	await tab.evaluate(`[...document.querySelectorAll('.editor button')].find((b) => b.textContent.includes('Save schedule')).click()`);
	check(await toast('Schedule saved'), 'the schedule of the profile that is not active is saved');
	const springLiving = await (await fetch(`${daemon.base}/api/zones/8/schedule?preset=${spring.id}`)).json();
	const activeLiving = await (await fetch(`${daemon.base}/api/zones/8/schedule`)).json();
	check(
		springLiving.programs?.[0]?.entries?.[0]?.setpoint_c === 19 && activeLiving.programs?.[0]?.entries?.[0]?.setpoint_c === 21.5,
		`the edit reaches Spring only (${springLiving.programs?.[0]?.entries?.[0]?.setpoint_c}, active ${activeLiving.programs?.[0]?.entries?.[0]?.setpoint_c})`
	);
	await clickText('Activate');
	await tab.waitFor(`document.querySelector('dialog[open]')`);
	const rooms = await tab.evaluate(`[...document.querySelectorAll('dialog[open] li')].map((li) => li.textContent)`);
	check(rooms.some((r) => /^Living room: [\d.]+ °C → 19 °C$/.test(r)), `the confirmation lists the rooms that change now (${rooms.join(' | ')})`);
	check(await tab.evaluate(`document.activeElement?.textContent.trim() === 'Cancel'`), 'the confirmation focuses Cancel');
	await tab.evaluate(`document.querySelector('dialog[open] button.primary').click()`);
	check(await toast('Messages waiting for the device'), 'the activation is sent');
	await sleep(500);
	const nowActive = (await (await fetch(daemon.base + '/api/home')).json()).preset;
	check(nowActive?.name === 'Spring', `the daemon switched the active profile (${nowActive?.name})`);

	// 12. Rooms and devices on the seeded installation.
	const api = async (base, path) => (await fetch(base + '/api/' + path)).json();
	const clickIn = (scope, text) =>
		tab.evaluate(`(() => { const b = [...document.querySelectorAll(${JSON.stringify(scope + ' button')})].find((x) => x.textContent.trim().startsWith(${JSON.stringify(text)})); if (!b) throw new Error('no button ${text} in ${scope}'); b.click(); })()`);
	const choose = (selector, value) =>
		tab.evaluate(`(() => { const s = document.querySelector(${JSON.stringify(selector)}); s.value = ${JSON.stringify(value)}; s.dispatchEvent(new Event('change', { bubbles: true })); })()`);
	const valueOf = (selector) => tab.evaluate(`document.querySelector(${JSON.stringify(selector)})?.value`);
	const dialogText = () => tab.evaluate(`document.querySelector('dialog[open]')?.innerText ?? ''`);
	const closeDialog = () => tab.evaluate(`[...document.querySelectorAll('dialog[open] button')].find((b) => b.textContent.trim() === 'Cancel').click()`);

	await open('#/devices', `document.querySelector('[data-room="3"] .device')`);
	check(
		(await tab.evaluate(`[...document.querySelectorAll('[data-room] > header h2')].map((h) => h.textContent)`)).join() === 'Hot water,Hall,Bath,Office,Kitchen,Living room,Bedroom,No room',
		'the devices are grouped by room'
	);
	await clickIn('.page > .actions', 'Add room');
	await tab.waitFor(`document.querySelector('[data-testid=room-form]')`);
	await typeInto('[data-testid=room-form] input', 'Attic');
	const offRoom = await tab.evaluate(misaligned);
	check(offRoom.length === 0, `the buttons of the room form sit on the line of their controls (${offRoom})`);
	await sleep(6500);
	check((await valueOf('[data-testid=room-form] input')) === 'Attic', 'the open room form keeps its text across a data poll');
	await clickIn('[data-testid=room-form]', 'Add room');
	check(await toast('Room added'), 'the room is added');
	const attic = (await api(daemon.base, 'install/zones')).zones.find((z) => z.name === 'Attic');
	check(attic?.number === 4 && attic?.circuit === 1, `the daemon stores the room under the lowest free number, on the circuit (${attic?.number}, ${attic?.circuit})`);

	// Add a device: the window, the device it hears, the description and the pair command.
	const valve = { eui: '0ba9cefa07c51b00', short: 'a90b' };
	await clickIn('.page > .actions', 'Add device');
	await tab.waitFor(`document.querySelector('[data-testid=add-device]')`);
	requests.length = 0;
	await clickIn('[data-testid=add-device]', 'Open the pair window');
	check(await toast('Pair window open'), 'the pair window opens');
	const opened = await api(daemon.base, 'pair/window');
	check(opened.open === true && Date.parse(opened.until) - Date.now() > 500_000, 'the daemon reports the window open for 10 minutes');
	const left = await tab.waitFor(`document.querySelector('[data-testid=pair-left]')?.textContent`);
	check(/Open, (10:00|9:\d\d) left/.test(left), `the flow shows the time left (${left})`);
	check(await tab.evaluate(`/Waiting for a device/.test(document.querySelector('[data-testid=add-device]').innerText)`), 'the flow waits for the device');
	hear(valve);
	await tab.waitFor(`document.querySelector('button.joiner')`, 8000);
	await click('button.joiner');
	check((await valueOf('[data-testid=add-device] .eui')) === valve.eui, 'the device that the window heard fills the address');
	await typeInto('[data-testid=add-device] form .field.grow input', 'Attic valve');
	const addDisabled = () => tab.evaluate(`document.querySelector('[data-testid=add-device] form button[type=submit]').disabled`);
	check((await valueOf('[data-testid=pair-room]')) === '' && (await addDisabled()), 'the room has no default: the flow waits for a room or for No room');
	await choose('[data-testid=pair-room]', '4');
	check(!(await addDisabled()), 'a chosen room opens Add and pair');
	const offPair = await tab.evaluate(misaligned);
	check(offPair.length === 0, `the buttons of the device form sit on the line of their controls (${offPair})`);
	await sleep(6500);
	check(
		(await valueOf('[data-testid=add-device] form .field.grow input')) === 'Attic valve' && (await valueOf('[data-testid=add-device] .eui')) === valve.eui,
		'the add device flow keeps its fields across a data poll'
	);
	check(!requests.some((r) => r.startsWith('POST /api/pair/devices/') || r === 'POST /api/install/devices'), 'nothing is declared or paired before the person asks');
	// The daemon refuses the room: the run stops before the pair command, and the next run places the device.
	failing.add('POST /api/install/zones/4/members');
	await clickIn('[data-testid=add-device]', 'Add and pair');
	check(await toast('refused for the check'), 'the refusal of the room is shown');
	await tab.waitFor(`[...document.querySelectorAll('[data-testid=add-device] button')].some((b) => b.textContent.trim() === 'Pair again')`, 8000);
	const declaredValve = (await api(daemon.base, 'install/devices')).devices.find((d) => d.eui === valve.eui);
	check(declaredValve?.label === 'Attic valve' && declaredValve?.kind === 'valve', 'the daemon declares the device');
	check(
		!requests.some((r) => r.startsWith('POST /api/pair/devices/')) && !(await tab.evaluate(`!!document.querySelector('[data-testid=pair-progress], [data-testid=pair-done]')`)),
		'a refused room sends no pair command and starts no pairing'
	);
	check(
		(await valueOf('[data-testid=pair-room]')) === '4' && (await tab.evaluate(`/is in no room/.test(document.querySelector('[data-testid=add-device]').innerText)`)),
		'a declared device in no room keeps the room field and the chosen room'
	);
	failing.clear();
	await clickIn('[data-testid=add-device]', 'Pair again');
	check(await toast('Pair command queued for Attic valve'), 'the device is declared and the pair command is queued');
	check((await api(daemon.base, 'install/zones/4')).zone.members.includes(valve.eui), 'the device is a member of its room before the pairing ends');
	await tab.waitFor(`document.querySelector('[data-testid=pair-progress]')`);
	await sleep(6500);
	check(
		await tab.evaluate(`/Pairing Attic valve/.test(document.querySelector('[data-testid=pair-progress]')?.innerText ?? '') && !document.querySelector('[data-testid=pair-done]')`),
		'a queued pair command shows as in progress, not as a success, also after a data poll'
	);
	check(
		await tab.evaluate(`!document.querySelector('[data-testid=add-device] :is(form, .steps)') && !!document.querySelector('[data-testid=pair-progress] progress')`),
		'a pairing in progress replaces the steps and the form, and shows that it runs'
	);
	await clickIn('[data-testid=add-device]', 'Send again');
	await sleep(800);
	check(requests.filter((r) => r === 'POST /api/pair/devices/' + valve.eui).length === 2, 'Send again sends the pair command once more');
	check(await tab.evaluate(`!!document.querySelector('[data-testid=pair-progress]')`), 'the flow still waits after the second command');
	joinedShort = valve.short;
	const doneText = await tab.waitFor(`document.querySelector('[data-testid=pair-done]')?.innerText`, 8000).catch(() => '');
	check(/Attic valve is paired/.test(doneText) && /Room: Attic/.test(doneText), `the flow ends on the name and the room of the paired device (${doneText})`);
	check(
		(await api(daemon.base, 'install/zones/4')).zone.members.includes(valve.eui) && !(await tab.evaluate(`!!document.querySelector('[data-testid=pair-room-lost]')`)),
		'after the flow the daemon holds the device in the chosen room, and the end shows no warning'
	);
	await sleep(500);
	check((await api(daemon.base, 'pair/window')).open === false, 'the pair window closes when the pairing ends');
	// The room leaves the daemon behind the page: the end follows the daemon and offers the repair.
	await fetch(`${daemon.base}/api/install/zones/4/members/${valve.eui}`, { method: 'DELETE' });
	const lost = await tab.waitFor(`document.querySelector('[data-testid=pair-room-lost]')?.innerText`, 8000).catch(() => '');
	check(
		/You chose the room Attic/.test(lost) && /in no room/.test(await tab.evaluate(`document.querySelector('[data-testid=pair-done-room]').innerText`)),
		`the end says that the device is in no room although a room was chosen (${lost})`
	);
	await clickIn('[data-testid=pair-done]', 'Put it in Attic');
	check(await toast('Device moved'), 'the end puts the device in the chosen room');
	check((await api(daemon.base, 'install/zones/4')).zone.members.includes(valve.eui), 'the daemon holds the device in the room again');
	check(
		await tab.waitFor(`!document.querySelector('[data-testid=pair-room-lost]') && /Room: Attic/.test(document.querySelector('[data-testid=pair-done-room]').innerText)`, 8000).then(() => true, () => false),
		'the end shows the room of the daemon again, with no warning'
	);
	// The daemon keeps the device in the list of the closed window. Wait for a poll that reads it.
	await sleep(6500);
	const ended = await tab.evaluate(`(() => {
		const flow = document.querySelector('[data-testid=add-device]');
		return { done: !!flow.querySelector('[data-testid=pair-done]'), steps: !!flow.querySelector('.steps'), form: !!flow.querySelector('form'), joiner: !!flow.querySelector('.joiner'), text: flow.innerText };
	})()`);
	check(
		ended.done && !ended.steps && !ended.form && !ended.joiner && !/Hold the button|In the installation/.test(ended.text),
		`after the pairing the flow shows the end only: no step, no form, no heard device (${JSON.stringify(ended)})`
	);
	await clickIn('[data-testid=pair-done]', 'Done');
	check(await tab.waitFor(`!document.querySelector('#add-device-title')`, 3000).then(() => true, () => false), 'Done closes the flow');

	// The device that joined stays in the answer of the closed window. The flow does not offer it again.
	await clickIn('.page > .actions', 'Add device');
	await tab.waitFor(`document.querySelector('[data-testid=add-device] .joiner')`, 8000);
	check(
		await tab.evaluate(`!document.querySelector('[data-testid=add-device] button.joiner') && /Paired: Attic valve/.test(document.querySelector('[data-testid=add-device] .joiner').innerText)`),
		'a heard device that joined shows as paired and cannot be selected'
	);
	// A declared device that asks to join again: the daemon did not hear it since.
	joinedShort = null;
	hear({ eui: '04a3defa07c51b00', short: 'a304' });
	await clickIn('[data-testid=add-device]', 'Open the pair window');
	await tab.waitFor(`/In the installation: bath/.test(document.querySelector('[data-testid=add-device] button.joiner')?.innerText ?? '')`, 8000);
	await click('[data-testid=add-device] button.joiner');
	await clickIn('[data-testid=add-device]', 'Pair again');
	check(await toast('Pair command queued for bath'), 'a declared device that is not paired can be paired again');
	check(await tab.waitFor(`/Pairing bath/.test(document.querySelector('[data-testid=pair-progress]')?.innerText ?? '')`, 3000).then(() => true, () => false), 'the second pairing shows as in progress');
	await clickIn('[data-testid=add-device]', 'Stop waiting');
	await clickIn('[data-testid=add-device]', 'Close the window');
	check(await toast('Pair window closed'), 'the pair window closes on request');
	joiner = null;
	await clickIn('#add-device-title ~', 'Close');

	// The options of a room: one form, one save, and the checks of the daemon before the request.
	const opt = (n, rest) => `[data-testid=room-options] fieldset:nth-of-type(${n}) ${rest}`;
	await open('#/rooms/2', `document.querySelector('[data-testid=room-options]')`);
	const saveDisabled = () => tab.evaluate(`[...document.querySelectorAll('[data-testid=room-options] button')].find((b) => b.textContent.trim() === 'Save options').disabled`);
	check(await saveDisabled(), 'the room options cannot be saved before a change');
	check(
		/full motor stroke/.test(await tab.evaluate(`document.querySelector('[data-testid=mode-hint]').textContent`)),
		'the offset mode that homes the valve motor says so'
	);
	await typeInto(opt(1, 'input[type=number]'), '20');
	await typeInto(opt(2, 'input'), '16.5');
	await choose(opt(3, 'select'), 'timer');
	await tab.waitFor(`document.querySelector(${JSON.stringify(opt(3, 'input[type=number]'))})`);
	await typeInto(opt(3, 'input[type=number]'), '90');
	await click(opt(4, 'input[type=checkbox]'));
	await choose('[data-testid=correction]', 'shift');
	check(
		/no motor stroke/.test(await tab.evaluate(`document.querySelector('[data-testid=correction-hint]').textContent`)) &&
			!(await tab.evaluate(`!!document.querySelector('[data-testid=mode-hint]')`)),
		'the continuous shift says it costs no motor stroke and hides the offset mode of the valve'
	);
	await typeInto(opt(5, '.pair .input.num'), '-0.7');
	await sleep(6500);
	check(
		(await valueOf(opt(1, 'input[type=number]'))) === '20' && (await valueOf(opt(2, 'input'))) === '16.5' && (await valueOf(opt(5, '.pair .input.num'))) === '-0.7',
		'the room options keep their values across a data poll'
	);
	await typeInto(opt(1, 'input[type=number]'), '0');
	check(
		(await saveDisabled()) && /Window time: 1 to 1092 minutes/.test(await tab.evaluate(`document.querySelector('[data-testid=room-options] .issues')?.innerText ?? ''`)),
		'a value outside the range of the daemon is named and cannot be saved'
	);
	await typeInto(opt(1, 'input[type=number]'), '20');
	requests.length = 0;
	await clickIn('[data-testid=room-options]', 'Save options');
	check(await toast('Room saved'), 'the room options are saved');
	const bathOptions = (await api(daemon.base, 'install/zones/2')).zone;
	check(
		bathOptions.open_window_duration_sec === 1200 && bathOptions.away_temperature_c === 16.5 && bathOptions.device_overlay_end === 'timer' &&
			bathOptions.device_overlay_end_sec === 5400 && bathOptions.early_start === true && bathOptions.offset_mode === 'setpoint' && bathOptions.setpoint_offset_c === -0.7,
		`the daemon stores the window time, the away temperature, the end rule, the early start, the offset mode and the offset (${JSON.stringify([bathOptions.open_window_duration_sec, bathOptions.away_temperature_c, bathOptions.device_overlay_end, bathOptions.device_overlay_end_sec, bathOptions.early_start, bathOptions.offset_mode, bathOptions.setpoint_offset_c])})`
	);
	check(
		requests.filter((r) => r === 'PATCH /api/install/zones/2').length === 1 && requests.filter((r) => r === 'POST /api/zones/2/offset').length === 1,
		'the options go in one request, and the offset in one more'
	);
	await tab.waitFor(`document.querySelector(${JSON.stringify(opt(5, '.pair .input.num'))})?.value === '-0.7'`, 3000);
	check(await saveDisabled(), 'the saved options show again, with nothing left to save');
	// The room temperature that Home Assistant pushes. Bedroom keeps the method None, so the daemon refuses the push and
	// the room screen and the correction field say why. Bath took the continuous shift above, so its reading is in use.
	const push = (zone, body) =>
		fetch(`${daemon.base}/api/zones/${zone}/temperature`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
	const refusal = 'Test thermometer sends the room temperature, but with the method None freedo ignores it.';
	check((await push(10, { tempC: 20.4, source: 'Test thermometer' })).status === 409, 'the daemon refuses a room temperature for the method None');
	await open('#/zones/10', `document.querySelector('[data-testid=room-temp]')`);
	check(
		(await tab.evaluate(`document.querySelector('[data-testid=room-temp]').dataset.state`)) === 'refused' &&
			(await tab.evaluate(`document.querySelector('[data-testid=room-temp-refused]').textContent.trim()`)) === refusal,
		'the room screen warns that the method None ignores the room temperature that Home Assistant sends'
	);
	await click('[data-testid=room-temp-change]');
	await tab.waitFor(`document.querySelector('[data-testid=correction-refused]')`, 10_000);
	check(
		(await tab.evaluate(`document.querySelector('[data-testid=correction-refused]').textContent.trim()`)) === refusal,
		'the correction field repeats the warning while the method is None'
	);
	check(
		await tab.waitFor(`document.activeElement?.id === 'correction'`, 3000).then(() => true, () => false),
		'the link from the room screen focuses the correction field'
	);
	await choose('[data-testid=correction]', 'shift');
	check(!(await tab.evaluate(`!!document.querySelector('[data-testid=correction-refused]')`)), 'the warning goes when the form picks a method');
	await open('#/zones/1', `document.querySelector('#state-title')`);
	await sleep(1500);
	check(!(await tab.evaluate(`!!document.querySelector('[data-testid=room-temp]')`)), 'a room with the method None and no refused push shows no room temperature line');
	check((await push(2, { tempC: 21.4, source: 'Test thermometer' })).status === 200, 'the daemon takes a room temperature for the continuous shift');
	await open('#/zones/2', `document.querySelector('[data-testid=room-temp][data-state=active]')`);
	const live = await tab.evaluate(`document.querySelector('[data-testid=room-temp]').innerText`);
	check(
		/Room temperature/.test(live) && /Test thermometer/.test(live) && /21\.4°/.test(live) && /ago|now/.test(live) && /Method: Continuous shift/.test(live),
		`the room screen shows the source, the reading, its age and the method (${live.replace(/\s+/g, ' ')})`
	);
	await fetch(`${daemon.base}/api/zones/2/temperature`, { method: 'DELETE' });
	await open('#/zones/2', `document.querySelector('[data-testid=room-temp][data-state=no_reading]')`);
	check(
		/No reading from Home Assistant/.test(await tab.evaluate(`document.querySelector('[data-testid=room-temp]').innerText`)),
		'a method that needs a reading says when none arrived'
	);
	await open('#/rooms/0', `document.querySelector('[data-member]')`);
	check(!(await tab.evaluate(`!!document.querySelector('[data-testid=room-options]')`)), 'the hot water room has no room options');

	// The actions on a device live in the settings of its room.
	const member = (label) => `document.querySelector('[data-member=${JSON.stringify(label)}]')`;
	const memberSelect = (label, value) =>
		tab.evaluate(`(() => { const s = ${member(label)}.querySelector('select'); s.value = ${JSON.stringify(value)}; s.dispatchEvent(new Event('change', { bubbles: true })); })()`);
	const memberButton = (label, text) =>
		tab.evaluate(`[...${member(label)}.querySelectorAll('button')].find((b) => b.textContent.trim() === ${JSON.stringify(text)}).click()`);
	const joinOptions = () => tab.evaluate(`[...document.querySelectorAll('[data-testid=join] option')].map((o) => o.textContent).filter((text) => text !== 'Choose a device').sort().join()`);

	// Move a device, and a refused move.
	await open('#/rooms/3', member('office'));
	check(
		!(await tab.evaluate(`!!document.querySelector('[data-testid=join]')`)),
		'a room offers no device to add while another room holds every device'
	);
	const offMember = await tab.evaluate(misaligned);
	check(offMember.length === 0, `the controls of a member row sit on one line (${offMember})`);
	await memberSelect('office', '4');
	check(await toast('Device moved'), 'the move is accepted');
	const afterMove = (await api(daemon.base, 'install/zones')).zones;
	check(
		afterMove.find((z) => z.number === 3).members.length === 0 && afterMove.find((z) => z.number === 4).members.includes('08a4defa07c51b00'),
		'the daemon moved the device to the other room'
	);
	await open('#/rooms/2', member('bath'));
	failing.add('DELETE /api/install/zones/2/members/04a3defa07c51b00');
	await memberSelect('bath', '3');
	check(await toast('refused for the check'), 'the refusal of a move is shown');
	await sleep(400);
	check((await tab.evaluate(`${member('bath')}.querySelector('select').value`)) === '2', 'the room of the device returns to what the daemon says after a refused move');
	failing.clear();

	// Restart a device: the dialog first, one command after the confirm.
	const reboot = 'POST /api/nodes/04a3defa07c51b00/reboot';
	requests.length = 0;
	await memberButton('bath', 'Restart');
	await tab.waitFor(`document.querySelector('dialog[open]')`);
	const restartText = await dialogText();
	check(
		/Restart bath\?/.test(restartText) && /about 5 seconds/.test(restartText) && /about a minute/.test(restartText) && /keeps its pairing/.test(restartText),
		`the restart dialog names the device and says what happens (${restartText})`
	);
	await closeDialog();
	await sleep(300);
	check(!requests.includes(reboot), 'Cancel sends no restart');
	await memberButton('bath', 'Restart');
	await tab.waitFor(`document.querySelector('dialog[open]')`);
	check(!requests.includes(reboot), 'Restart sends nothing before the person confirms');
	await tab.evaluate(`document.querySelector('dialog[open] button.primary').click()`);
	check(await toast('Messages waiting for the device'), 'the confirmed restart shows as queued, not as done');
	check(
		requests.filter((r) => r === reboot).length === 1 && bodies.get(reboot) === '{}',
		`the confirm sends one restart with an empty body (${requests.filter((r) => r === reboot).length}, ${bodies.get(reboot)})`
	);

	// Remove a device: the effects first.
	await open('#/rooms/1', member('Hall wall unit'));
	requests.length = 0;
	await memberButton('Hall wall unit', 'Remove from the installation');
	await tab.waitFor(`document.querySelector('dialog[open]')`);
	check(/heat source/.test(await dialogText()) && !(await tab.evaluate(`!!document.querySelector('dialog[open] button.primary')`)), 'a heat source cannot be removed, and the dialog says why');
	await closeDialog();
	await open('#/rooms/5', member('kitchen valve left'));
	await memberButton('kitchen valve left', 'Remove from the installation');
	await tab.waitFor(`document.querySelector('dialog[open]')`);
	const effects = await tab.evaluate(`[...document.querySelectorAll('dialog[open] li')].map((li) => li.textContent)`);
	check(effects.some((e) => e === 'Kitchen loses its demand reporter.'), `the removal lists its effects before the confirm (${effects.join(' | ')})`);
	const dialogs = await tab.evaluate(`({ title: document.querySelector('dialog[open] h2').textContent, hidden: [...document.querySelectorAll('dialog:not([open])')].every((d) => getComputedStyle(d).display === 'none') })`);
	check(dialogs.title === 'Remove kitchen valve left?' && dialogs.hidden, `the open dialog names the device, and a closed dialog is not rendered (${JSON.stringify(dialogs)})`);
	await closeDialog();
	await sleep(300);
	check(!requests.some((r) => r.startsWith('DELETE ')), 'Cancel removes nothing');
	await memberButton('kitchen valve left', 'Remove from the installation');
	await tab.waitFor(`document.querySelector('dialog[open]')`);
	await tab.evaluate(`document.querySelector('dialog[open] button.primary').click()`);
	check(await toast('Device removed'), 'the removal is accepted after the confirm');
	check(!(await api(daemon.base, 'install/devices')).devices.some((d) => d.serial === 'VA1000000006'), 'the daemon removed the device');
	check(
		await tab.waitFor(`!${member('kitchen valve left')} && !!${member('kitchen valve right')}`, 3000).then(() => true, () => false),
		'the removed device leaves the list of its room'
	);

	// Room settings: the name, the measuring device and the delete.
	await open('#/devices', `document.querySelector('[data-room="4"] header a')`);
	await click('[data-room="4"] header a');
	await tab.waitFor(`document.querySelector('main h1')?.textContent === 'Attic'`);
	await tab.evaluate(`document.querySelector('main form input').focus(); document.querySelector('main form input').select()`);
	await type('Loft');
	await sleep(6500);
	check((await valueOf('main form input')) === 'Loft', 'a typed room name survives a data poll');
	const offName = await tab.evaluate(misaligned);
	check(offName.length === 0, `the buttons of the room settings sit on the line of their controls (${offName})`);
	await clickIn('main form', 'Save');
	check(await toast('Room renamed'), 'the room is renamed');
	await tab.waitFor(`document.querySelector('main h1')?.textContent === 'Loft'`);
	await choose('.choices label:nth-child(2) select', valve.eui);
	check(await toast('Room saved'), 'the measuring device is saved');
	const loft = (await api(daemon.base, 'install/zones/4')).zone;
	check(loft.name === 'Loft' && loft.measuring_device === valve.eui, `the daemon stores the name and the measuring device (${loft.name}, ${loft.measuring_device})`);
	requests.length = 0;
	await clickIn('main', 'Delete room');
	await tab.waitFor(`document.querySelector('dialog[open]')`);
	const staying = await tab.evaluate(`[...document.querySelectorAll('dialog[open] li')].map((li) => li.textContent)`);
	check(staying.includes('Attic valve') && staying.includes('office'), `the delete names the devices that lose their room (${staying.join(' | ')})`);
	check(!requests.some((r) => r.startsWith('DELETE ')), 'Delete room sends nothing before the person confirms');
	await tab.evaluate(`document.querySelector('dialog[open] button.primary').click()`);
	check(await toast('Room deleted'), 'the room is deleted');
	await tab.waitFor(`location.hash === '#/devices' && document.querySelector('[data-room="none"]')`);
	check(!(await api(daemon.base, 'install/zones')).zones.some((z) => z.number === 4), 'the daemon deleted the room');

	// No room: the last group of the list, with settings of its own.
	const groups = await tab.evaluate(`[...document.querySelectorAll('[data-room] > header h2')].map((h) => h.textContent)`);
	check(groups.at(-1) === 'No room', `the devices in no room form the last group, named No room (${groups.join()})`);
	check(
		(await tab.evaluate(`[...document.querySelectorAll('[data-room="none"] .device strong')].map((s) => s.textContent).sort().join()`)) === 'Attic valve,office',
		'the devices of the deleted room show under No room'
	);
	await open('#/rooms/2', member('bath'));
	check((await joinOptions()) === 'Attic valve,office', `a room offers the devices in no room, and no device of another room (${await joinOptions()})`);
	await open('#/rooms/0', member('Hall wall unit'));
	check((await joinOptions()) === '', `the hot water room offers no valve (${await joinOptions()})`);
	await open('#/devices', `document.querySelector('[data-room="none"] header a')`);
	await click('[data-room="none"] header a');
	await tab.waitFor(`location.hash === '#/rooms/none' && document.querySelector('main h1')?.textContent === 'No room' && ${member('office')}`);
	check(
		await tab.evaluate(`[...${member('office')}.querySelectorAll('button')].map((b) => b.textContent.trim()).join() === 'Rename,Restart,Remove from the installation'`),
		'the settings of No room offer rename, restart and removal for a free device'
	);
	const roomOptions = await tab.evaluate(`[...${member('office')}.querySelectorAll('option')].map((o) => o.textContent).join()`);
	check(roomOptions === 'No room,Hall,Bath,Office,Kitchen,Living room,Bedroom', `a free valve can go to every heated room (${roomOptions})`);
	await memberSelect('office', '3');
	check(await toast('Device moved'), 'a free device is put in a room');
	check((await api(daemon.base, 'install/zones/3')).zone.members.includes('08a4defa07c51b00'), 'the daemon added the device to the room');
	await tab.waitFor(`!${member('office')}`, 3000);
	await memberButton('Attic valve', 'Remove from the installation');
	await tab.waitFor(`document.querySelector('dialog[open]')`);
	check((await tab.evaluate(`document.querySelectorAll('dialog[open] li').length`)) === 0, 'the removal of a device in no room lists no effect');
	await tab.evaluate(`document.querySelector('dialog[open] button.primary').click()`);
	check(await toast('Device removed'), 'a free device is removed from the installation');
	check(!(await api(daemon.base, 'install/devices')).devices.some((d) => d.eui === valve.eui), 'the daemon removed the free device');
	await open('#/devices', `document.querySelector('[data-room="3"] .device')`);
	check(
		await tab.evaluate(`!document.querySelector('[data-room="none"] .device') && /Every device is in a room/.test(document.querySelector('[data-room="none"]')?.innerText ?? '')`),
		'the group No room says that every device has a room'
	);

	// The burner badge follows the burner flag of the boiler, not the call of the heating circuit.
	const burner = async (patch) => {
		boilerPatch = patch;
		await open('#/boiler', `document.querySelector('[data-testid=burner]')`);
		await sleep(300);
		return tab.evaluate(`[document.querySelector('[data-testid=burner]').textContent.trim(), document.querySelector('[data-testid=heat-call]')?.textContent.trim() ?? null]`);
	};
	const hotWaterRun = await burner({ Present: true, Active: true, HeatActive: false, Mon: { boiler_flag: 1 } });
	check(hotWaterRun.join() === 'Burner on,', `a burner that runs while no room asks for heat shows as on (${hotWaterRun})`);
	const heatingRun = await burner({ Present: true, Active: true, HeatActive: true, Mon: { boiler_flag: 1 } });
	check(heatingRun.join() === 'Burner on,Heating requested', `a burner that runs for the heating circuit shows both (${heatingRun})`);
	const waiting = await burner({ Present: true, Active: false, HeatActive: true, Mon: { boiler_flag: 0 } });
	check(waiting.join() === 'Burner off,Heating requested', `a call for heat with the burner off does not show the burner as on (${waiting})`);
	const noFlag = await burner({ Present: true, Active: false, HeatActive: false, Mon: {} });
	check(noFlag.join() === 'Burner state not reported,', `a boiler with no burner flag shows no burner state (${noFlag})`);
	// The fault codes of the boiler: a banner on every page, and the numbers on the boiler page.
	boilerPatch = { Present: true, FaultCodes: [133, 28], FaultCode: 133 };
	await open('#/boiler', `document.querySelector('[data-testid=fault-state]')`);
	await tab.waitFor(`document.querySelector('[data-testid=fault-state]').textContent.trim() === '133, 28'`, 8000);
	check(
		await tab.evaluate(`/maker/.test(document.querySelector('[data-testid=fault-meaning]')?.textContent ?? '')`),
		'the boiler page shows the fault codes as numbers and says that it does not know their meaning'
	);
	await open('#/', `[...document.querySelectorAll('main .notice')].some((n) => /fault codes: 133, 28/.test(n.textContent))`);
	check(true, 'a boiler fault shows as a banner on the home page');
	boilerPatch = { Present: true, FaultCodes: [], FaultCode: 0 };
	await open('#/boiler', `document.querySelector('[data-testid=fault-state]')?.textContent.trim() === 'None'`);
	check(
		!(await tab.evaluate(`[...document.querySelectorAll('main .notice')].some((n) => /fault codes/.test(n.textContent))`)),
		'a boiler with no fault code shows None and no banner'
	);
	// The readings under the flow temperature sit on the colour of the hero, not on a tile of the page.
	check(
		await tab.evaluate(`((cells) => cells.length === 3 && cells.every((d) => getComputedStyle(d).backgroundColor === 'rgba(0, 0, 0, 0)'))([...document.querySelectorAll('.hero dl > div')])`),
		'the return, hot water and outside readings of the boiler page have no tile background'
	);
	boilerPatch = null;

	// The boiler setup writes the energy data and the flow options through PUT /api/install/boiler.
	// Once the fuel flow is set, the boiler page takes a meter reading, refuses a lower one and undoes the last.
	const typeAt = (selector, index, value) =>
		tab.evaluate(
			`(() => { const i = document.querySelectorAll(${JSON.stringify(selector)})[${index}]; i.value = ${JSON.stringify(value)}; i.dispatchEvent(new Event('input', { bubbles: true })); })()`
		);
	await open('#/boiler', `document.querySelector('[data-testid=boiler-setup]')`);
	check(await tab.evaluate(`!!document.querySelector('[data-testid=meter-off]')`), 'without a fuel flow the boiler page sends the meter readings to the setup');
	await click('[data-testid=boiler-setup]');
	await tab.waitFor(`document.querySelectorAll('[data-testid=boiler-energy] .pair .input').length === 4`, 8000);
	for (const [i, v] of ['24', '3,5', '2,63', '0.3'].entries()) await typeAt('[data-testid=boiler-energy] .pair .input', i, v);
	await click('[data-testid=boiler-energy] button[type=submit]');
	check(await toast('Setup saved'), 'the energy data of the boiler are saved');
	const energyOptions = await api(daemon.base, 'install');
	check(
		energyOptions.boiler_max_power_kw === 24 && energyOptions.boiler_min_power_kw === 3.5 && energyOptions.boiler_max_gas_m3h === 2.63 && energyOptions.boiler_min_gas_m3h === 0.3,
		`the daemon holds the energy data, with a comma read as the decimal separator (${energyOptions.boiler_max_power_kw}, ${energyOptions.boiler_min_power_kw}, ${energyOptions.boiler_max_gas_m3h}, ${energyOptions.boiler_min_gas_m3h})`
	);
	await typeAt('[data-testid=flow-limit]', 0, '56');
	await tab.evaluate(`(() => { const c = document.querySelector('[data-testid=default-curve]'); if (!c.checked) c.click(); })()`);
	await tab.waitFor(`document.querySelector('[data-testid=boiler-flow] svg[role=img]')`, 4000).catch(() => {});
	check(await tab.evaluate(`!!document.querySelector('[data-testid=boiler-flow] svg[role=img]')`), 'the default curve shows its preview');
	await typeAt('[data-testid=flow-limit]', 0, '');
	check(
		await tab.evaluate(`document.querySelector('[data-testid=flow-save]').disabled && /needs the highest flow/.test(document.querySelector('[data-testid=boiler-flow] .issues')?.textContent ?? '')`),
		'the default curve without a highest flow temperature cannot be saved and says why'
	);
	await typeAt('[data-testid=flow-limit]', 0, '56');
	// The toast of the first save may still show, so the check waits for the daemon to hold the values.
	await click('[data-testid=flow-save]');
	let flowOptions = {};
	for (let i = 0; i < 50 && !(flowOptions.flow_limit_c === 56 && flowOptions.flow_default_curve === true); i++) {
		await sleep(100);
		flowOptions = await api(daemon.base, 'install');
	}
	check(flowOptions.flow_limit_c === 56 && flowOptions.flow_default_curve === true, `the daemon holds the flow limit and the default curve (${flowOptions.flow_limit_c}, ${flowOptions.flow_default_curve})`);

	await open('#/boiler', `document.querySelector('[data-testid=meter-add]') && !document.querySelector('[data-testid=meter-add]').disabled`);
	await click('[data-testid=meter-add]');
	await tab.waitFor(`document.querySelector('[data-testid=meter-reading]')`, 4000);
	await typeAt('[data-testid=meter-reading]', 0, '100,5');
	await click('[data-testid=gas-meter] form button[type=submit]');
	check(await toast('Reading saved'), 'a meter reading is saved');
	await tab.waitFor(`document.querySelectorAll('[data-testid=meter-readings] .list-row').length === 1`, 8000).catch(() => {});
	const meter = (await api(daemon.base, 'boiler')).energy.gas.meter;
	check(meter?.lastReading === 100.5 && meter?.entries?.length === 1, `the daemon holds the reading (${JSON.stringify(meter?.entries)})`);
	await click('[data-testid=meter-add]');
	await tab.waitFor(`document.querySelector('[data-testid=meter-reading]')`, 4000);
	await typeAt('[data-testid=meter-reading]', 0, '99');
	check(
		await tab.evaluate(`document.querySelector('[data-testid=gas-meter] form button[type=submit]').disabled && /below the last one/.test(document.querySelector('[data-testid=gas-meter] [role=alert]')?.textContent ?? '')`),
		'a reading below the last one is refused before it is sent, and the page names the replaced meter'
	);
	await click('[data-testid=meter-undo]');
	await tab.waitFor(`document.querySelector('dialog[open] button.primary')`, 4000);
	await tab.evaluate(`document.querySelector('dialog[open] button.primary').click()`);
	check(await toast('Reading removed'), 'the last reading is undone after a confirm');
	check((await api(daemon.base, 'boiler')).energy.gas.meter === null, 'the daemon holds no reading after the undo');

	// The statistics: on from Settings, read by the room report and the consumption page, off after a confirm.
	await open('#/settings', `document.querySelector('[data-testid=history-switch]')`);
	check(await tab.evaluate(`!document.querySelector('[data-testid=history-switch]').checked`), 'the statistics are off by default');
	await open('#/zones/8/report', `/Turn on the statistics/.test(document.querySelector('main')?.innerText ?? '')`);
	check(true, 'the room report offers to turn the statistics on while they are off');
	await open('#/settings', `document.querySelector('[data-testid=history-switch]')`);
	await click('[data-testid=history-switch]');
	check(await toast('Statistics on'), 'the switch turns the statistics on');
	check((await api(daemon.base, 'history')).enabled === true, 'the daemon records the statistics');
	// The engine samples the current slot within a second of the switch.
	let zoneDay = { samples: [] };
	for (let i = 0; i < 40 && zoneDay.samples.length === 0; i++) {
		await sleep(100);
		zoneDay = await api(daemon.base, 'history/zones/8');
	}
	check(zoneDay.samples.length === 1, `the daemon holds a sample of the room (${zoneDay.samples.length})`);
	await open('#/zones/8', `document.querySelector('[data-testid=report-link]')`);
	check(await tab.evaluate(`document.querySelector('[data-testid=report-link]').getAttribute('href') === '#/zones/8/report'`), 'a room screen links to its report');
	await open('#/zones/8/report', `document.querySelector('main svg[role=slider]')`);
	check(await tab.evaluate(`document.querySelectorAll('main .stat-tiles > div').length >= 3`), 'the room report shows the stats and the chart of today');
	check(await tab.evaluate(`[...document.querySelectorAll('main .day-nav button')].slice(0, 2).every((b) => b.disabled)`), 'the report of the first day offers no other day');
	await tab.evaluate(`document.querySelector('main svg[role=slider]').focus()`);
	await tab.call('Input.dispatchKeyEvent', { type: 'keyDown', key: 'End', code: 'End', windowsVirtualKeyCode: 35 });
	check(await tab.evaluate(`/target/.test(document.querySelector('main svg[role=slider]').getAttribute('aria-valuetext') ?? '')`), 'the keyboard reads a sample of the chart');
	// A room with a flat target still lists its heating, and names the return home and the target rise.
	zoneDayPatch = (fromMs) =>
		Array.from({ length: 24 }, (_, i) => ({
			atMs: fromMs + i * 300_000,
			tempC: 20,
			targetC: i < 3 ? 16 : i < 14 ? 20 : 21,
			on: true,
			humidityPct: null,
			demandPct: (i >= 3 && i < 9) || i >= 14 ? 30 : 0,
			window: false,
			away: i < 3
		}));
	await open('#/zones/8/report', `document.querySelectorAll('main .list-row').length === 2`);
	const periodRows = await tab.evaluate(`[...document.querySelectorAll('main .list-row .list-sub')].map((r) => r.textContent)`);
	check(/^Back home/.test(periodRows[0]) && /^Target raised to 21/.test(periodRows[1]), `the report lists the heating periods with their cause (${periodRows.join(' | ')})`);
	zoneDayPatch = null;
	await open('#/boiler', `document.querySelector('[data-testid=energy-link]')`);
	check(await tab.evaluate(`document.querySelector('[data-testid=energy-link]').getAttribute('href') === '#/boiler/energy'`), 'the boiler page links to the consumption page');
	await open('#/boiler/energy', `document.querySelector('main svg[role=slider]')`);
	check(true, 'the consumption page shows the chart of the month');
	// The notice of a stopped fuel counter names its reason, and links to the setup only for a missing fuel flow.
	const energyNow = (await api(daemon.base, 'boiler')).energy;
	const gasReason = async (reason) => {
		boilerPatch = { energy: { ...energyNow, gas: { ...energyNow.gas, running: false, reason } } };
		const notice = `[...document.querySelectorAll('main .notice')].find((n) => /fuel counter/.test(n.textContent))`;
		await open('#/boiler/energy', notice);
		return tab.evaluate(`((n) => [n.textContent, !!n.querySelector('a')])(${notice})`);
	};
	const [staleText, staleLink] = await gasReason('stale');
	check(/too old/.test(staleText) && !staleLink, `an old boiler report says so, with no setup link (${staleText})`);
	const [flowText, flowLink] = await gasReason('noGasFlow');
	check(/fuel flow/.test(flowText) && flowLink, `a missing fuel flow links to the boiler setup (${flowText})`);
	boilerPatch = null;
	await typeAt('main form.price input', 0, '1,05');
	await click('main form.price button[type=submit]');
	check(await toast('Price saved'), 'the fuel price is saved');
	const priced = await api(daemon.base, 'history');
	check(priced.fuelPrice === 1.05 && priced.currency === 'EUR', `the daemon holds the price, with a comma read as the decimal separator (${priced.fuelPrice} ${priced.currency})`);

	// On a phone the open table scrolls inside its block instead of widening the page.
	await tab.viewport(390, 844, true);
	await open('#/boiler/energy', `document.querySelector('details.table-twin')`);
	await tab.evaluate(`document.querySelector('details.table-twin').open = true`);
	await tab.waitFor(`document.querySelector('details.table-twin tbody tr')`, 4000);
	check(await tab.evaluate(`document.documentElement.scrollWidth <= window.innerWidth`), 'on a phone the open table of the consumption page keeps the page width');
	await tab.viewport(1280, 900);
	await open('#/settings', `document.querySelector('[data-testid=history-switch]')?.checked`);
	await click('[data-testid=history-switch]');
	await tab.waitFor(`document.querySelector('dialog[open] button.primary')`, 4000);
	check(await tab.evaluate(`document.querySelector('[data-testid=history-switch]').checked`), 'the switch stays on while the page asks');
	await tab.evaluate(`document.querySelector('dialog[open] button.primary').click()`);
	check(await toast('Statistics off'), 'the statistics turn off after a confirm');
	const historyOff = await api(daemon.base, 'history');
	check(historyOff.enabled === false && historyOff.sizeBytes > 0, `off keeps the stored data (${historyOff.sizeBytes} bytes)`);
	await tab.waitFor(`document.querySelector('[data-testid=history-delete]')`, 4000);
	await click('[data-testid=history-delete]');
	await tab.waitFor(`document.querySelector('dialog[open] button.primary')`, 4000);
	await tab.evaluate(`document.querySelector('dialog[open] button.primary').click()`);
	check(await toast('Statistics deleted'), 'the stored statistics are deleted after a confirm');
	check((await api(daemon.base, 'history')).sizeBytes === 0, 'the daemon holds no statistics after the delete');

	// 13. The first setup on a daemon with an empty database.
	fresh = await startDaemon(bin);
	const setupRoot = fresh.base + '/';
	const step = (name) => tab.evaluate(`document.querySelector('[data-step="${name}"]')?.dataset.state`);
	const stepDone = (name) => tab.waitFor(`document.querySelector('[data-step="${name}"]')?.dataset.state === 'done'`, 8000).then(() => true, () => false);
	await tab.navigate(setupRoot);
	await tab.waitFor(`[...document.querySelectorAll('main a')].some((a) => a.textContent.includes('First setup'))`);
	await tab.evaluate(`[...document.querySelectorAll('main a')].find((a) => a.textContent.includes('First setup')).click()`);
	await tab.waitFor(`document.querySelector('[data-step="radio"]')`);
	check(true, 'the empty home page leads to the first setup');
	const states = [];
	for (const name of ['radio', 'timezone', 'rooms', 'transmit', 'devices', 'circuits']) states.push(await step(name));
	check(states.every((v) => v === 'todo'), `every step of an empty daemon with no radio is open (${states})`);
	check(await tab.evaluate(`/no live radio/.test(document.querySelector('[data-step="radio"]').innerText) && /--tx/.test(document.querySelector('[data-step="transmit"]').innerText)`), 'the steps outside the dashboard say what to do');
	const offSetup = await tab.evaluate(misaligned);
	check(offSetup.length === 0, `the buttons of the setup forms sit on the line of their controls (${offSetup})`);

	await typeInto('[data-step="timezone"] input', 'Europe/Rome');
	await clickIn('[data-step="timezone"]', 'Set time zone');
	check(await toast('Time zone set'), 'the time zone is set');
	check(await stepDone('timezone'), 'the time zone step follows the health of the daemon');
	check((await api(fresh.base, 'install')).timezone === 'Europe/Rome', 'the daemon stores the time zone');

	await typeInto('[data-step="rooms"] input', 'Kitchen');
	await sleep(6500);
	check((await valueOf('[data-step="rooms"] input')) === 'Kitchen', 'a setup form keeps its text across a data poll');
	await clickIn('[data-step="rooms"]', 'Add room');
	check(await toast('Room added'), 'the first room is added');
	check(await stepDone('rooms'), 'the rooms step is done');

	const wall = { eui: '05a2cafa07c51b00', short: 'a205' };
	await clickIn('[data-step="devices"]', 'Open the pair window');
	check(await toast('Pair window open'), 'the setup opens the pair window');
	hear(wall);
	await tab.waitFor(`document.querySelector('button.joiner')`, 8000);
	await click('button.joiner');
	await choose('[data-step="devices"] form .field:nth-of-type(2) select', 'thermostat');
	await typeInto('[data-step="devices"] form .field.grow input', 'Wall');
	await choose('[data-step="devices"] form .field:last-of-type select', '1');
	await clickIn('[data-step="devices"]', 'Add and pair');
	check(await toast('Pair command queued for Wall'), 'the setup declares and pairs the thermostat');
	check(await stepDone('devices'), 'the devices step is done');
	joinedShort = wall.short;
	check(
		await tab.waitFor(`/Wall is paired/.test(document.querySelector('[data-testid=pair-done]')?.innerText ?? '') && !document.querySelector('[data-step="devices"] :is(form, .steps)')`, 8000).then(() => true, () => false),
		'the setup ends the pairing on the paired thermostat, with no step and no form'
	);
	joiner = null;
	joinedShort = null;

	await tab.waitFor(`document.querySelector('[data-testid=circuit-form]')`, 8000);
	await typeInto('[data-testid=circuit-form] input', 'House');
	await clickIn('[data-testid=circuit-form]', 'Add circuit');
	check(await toast('Circuit added'), 'the circuit is added');
	check(await stepDone('circuits'), 'the circuits step is done');
	const built = await api(fresh.base, 'install/zones/1');
	check(
		built.zone.name === 'Kitchen' && built.zone.circuit === 1 && built.zone.members.includes(wall.eui),
		`the daemon holds the room, its thermostat and its circuit (${JSON.stringify([built.zone.name, built.zone.circuit, built.zone.members])})`
	);
	check((await api(fresh.base, 'install/circuits')).circuits[0]?.heat_source === wall.eui, 'the thermostat is the heat source of the circuit');

	// 14. The backup: a download from the seeded daemon, then a restore into the daemon of the first setup.
	await open('#/settings', `document.querySelector('[data-testid=backup]')`);
	forbidden.add('GET /api/state/export');
	await clickIn('[data-testid=backup]', 'Download backup');
	check(
		await tab.waitFor(`/Only a Home Assistant administrator can download or restore a backup/.test(document.querySelector('[data-testid=backup-error]')?.innerText ?? '')`, 6000).then(() => true, () => false),
		'a user who is not an administrator reads why the backup is refused'
	);
	check(
		await tab.evaluate(`!document.querySelector('[data-testid="token-gate"]') && Boolean(document.querySelector('#daemon-title'))`),
		'the refusal stays in the section: the page asks for no token'
	);
	forbidden.clear();
	const backup = await downloadBackup(tab, check, 'backup');
	const source = await api(daemon.base, 'install');
	const own = await api(fresh.base, 'install');
	await tab.navigate(fresh.base + '/#/settings');
	await tab.waitFor(`document.querySelector('[data-testid=backup]')`);
	const importSent = () => requests.includes('POST /api/state/import');
	await cancelRestore(tab, check, 'backup', backup, importSent);
	check((await api(fresh.base, 'install')).home_id === own.home_id && own.home_id !== source.home_id, 'a cancelled restore leaves the installation');
	waitAfterImport = 8000;
	await confirmRestore(tab, check, 'backup', backup, () => waitUntil);
	waitAfterImport = 0;
	check(importSent(), 'the restore sends the file to the daemon');
	const restored = await api(fresh.base, 'install');
	check(
		restored.home_id === source.home_id && restored.coordinator?.eui === source.coordinator?.eui,
		`the daemon runs on the installation of the backup (home ${restored.home_id}, want ${source.home_id})`
	);
	check(
		(await api(fresh.base, 'install/zones')).zones.length === (await api(daemon.base, 'install/zones')).zones.length,
		'the daemon holds the rooms of the backup'
	);

	// 15. A daemon that waits after an import: every page counts the seconds, sends no command and comes back by itself.
	const waitBanner = `document.querySelector('main [data-testid=import-wait]')`;
	const waitSeconds = () => tab.evaluate(`Number(/in (\\d+) s/.exec(${waitBanner}?.innerText ?? '')?.[1] ?? NaN)`);
	// A click of the mouse, which an inert element does not take. A click from a script would reach it.
	const mouseClick = async (text) => {
		const at = await tab.evaluate(`(() => {
			const b = [...document.querySelectorAll('main button')].find((x) => x.textContent.trim().startsWith(${JSON.stringify(text)}));
			b.scrollIntoView({ block: 'center' });
			const r = b.getBoundingClientRect();
			return { x: r.x + r.width / 2, y: r.y + r.height / 2 };
		})()`);
		for (const type of ['mousePressed', 'mouseReleased']) {
			await tab.call('Input.dispatchMouseEvent', { type, x: at.x, y: at.y, button: 'left', clickCount: 1 });
		}
	};
	await open('#/', `document.body.innerText.includes('Living room')`);
	waitUntil = Date.now() + 12_000;
	check(
		await tab.waitFor(`/starts again in \\d+ s after the restore of the backup\\. The devices keep their last target/.test(${waitBanner}?.innerText ?? '')`, 8000).then(() => true, () => false),
		'a daemon that waits after an import shows one banner with the seconds left'
	);
	check(
		await tab.evaluate(`document.querySelectorAll('main > .notice').length === 1 && !/No answer from freedo/.test(document.body.innerText) && !document.querySelector('[data-testid="token-gate"]')`),
		'the wait shows no other banner, no "No answer" and no token form'
	);
	const firstLeft = await waitSeconds();
	await sleep(2100);
	const laterLeft = await waitSeconds();
	check(laterLeft < firstLeft && laterLeft > 0, `the banner counts down (${firstLeft} s, then ${laterLeft} s)`);
	check(
		await tab.evaluate(`Boolean(document.querySelector('.content[inert]')) && Boolean(document.querySelector('nav[inert]')) && document.body.innerText.includes('Living room')`),
		'the page keeps its last data and takes no input in the wait'
	);
	requests.length = 0;
	await mouseClick('Resume all');
	await sleep(600);
	check(requests.every((r) => r.startsWith('GET ') && r.endsWith('/api/health')), `the wait sends no command and reads the health alone (${[...new Set(requests)].join(', ') || 'no request'})`);
	await tab.evaluate(`location.hash = '#/devices'; location.reload()`);
	check(
		await tab.waitFor(`${waitBanner} && !document.querySelector('main h1') && !document.body.innerText.includes('Loading')`, 8000).then(() => true, () => false),
		'a page that opens in the wait shows the banner, and no page that loads for ever'
	);
	check(
		await tab.waitFor(`!document.querySelector('[data-testid=import-wait]') && document.querySelector('main h1') && !document.querySelector('.content[inert], nav[inert]')`, 20_000).then(() => true, () => false),
		'the page returns to normal by itself at the end of the wait'
	);
	check(Date.now() >= waitUntil, 'the page returns to normal only after the wait');
	await open('#/', `document.body.innerText.includes('Living room')`);
	requests.length = 0;
	await mouseClick('Resume all');
	check(await toast('Sent to'), 'after the wait the same click sends its command');
	check(requests.some((r) => /^POST .*\/api\/zones\/\d+\/auto$/.test(r)), 'the command reaches the daemon after the wait');
} catch (error) {
	check(false, String(error?.stack ?? error));
} finally {
	for (const cleanup of [() => browser?.close(), () => daemon.stop(), () => fresh?.stop()]) {
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
