// The steps of the Backup section of the page Settings, for ui-check and prefix-check. The page of
// tab must show Settings.
import { mkdtempSync, readdirSync, readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const section = '[data-testid=backup]';
const button = (text) =>
	`[...document.querySelectorAll('${section} button')].find((b) => b.textContent.trim().startsWith(${JSON.stringify(text)}))`;
const dialogButton = (value) => `document.querySelector('dialog[open] button[value=${value}]')`;

/** Clicks Download backup and returns the path of the file that Chrome saved, or null. */
export async function downloadBackup(tab, check, where) {
	const dir = mkdtempSync(join(tmpdir(), 'freedo-download-'));
	await tab.call('Page.setDownloadBehavior', { behavior: 'allow', downloadPath: dir });
	await tab.waitFor(`document.querySelector('${section}') && !${button('Download backup')}.disabled`);
	check(
		await tab.evaluate(`/network key/.test(document.querySelector('${section} .notice.tone-warning')?.innerText ?? '')`),
		`${where}: the section warns that the backup holds the network key`
	);
	await tab.evaluate(`${button('Download backup')}.click()`);
	let file = null;
	for (let i = 0; i < 100 && file === null; i++) {
		const name = readdirSync(dir).find((n) => /^freedo-state-.+\.db$/.test(n));
		if (name) file = join(dir, name);
		else await sleep(100);
	}
	const head = file === null ? '' : readFileSync(file).subarray(0, 15).toString();
	check(head === 'SQLite format 3', `${where}: the download arrives as a database file (${file ?? 'no file'})`);
	check(
		await tab.waitFor(`/Backup downloaded: freedo-state-/.test(document.querySelector('.toasts')?.innerText || '')`, 6000).then(() => true, () => false),
		`${where}: the page names the downloaded file`
	);
	return file;
}

async function pick(tab, file) {
	const { result } = await tab.call('Runtime.evaluate', { expression: `document.querySelector('${section} input[type=file]')` });
	await tab.call('DOM.setFileInputFiles', { files: [file], objectId: result.objectId });
	await tab.waitFor(`!${button('Restore backup')}.disabled`);
	await tab.evaluate(`${button('Restore backup')}.click()`);
	await tab.waitFor(dialogButton('ok'));
}

/** Picks the file, opens the dialog and cancels. sent() says whether the page sent the import. */
export async function cancelRestore(tab, check, where, file, sent) {
	await pick(tab, file);
	const text = await tab.evaluate(`document.querySelector('dialog[open]').innerText`);
	check(
		/Rooms, devices/.test(text) && /network key/.test(text) && /Frame counters/.test(text) && /learned values/.test(text) &&
			/keeps its current state as a copy/.test(text) && /restarts/.test(text),
		`${where}: the dialog says what the backup replaces, that the state is kept and that freedo restarts`
	);
	await tab.evaluate(`${dialogButton('cancel')}.click()`);
	await sleep(500);
	check(!sent() && !(await tab.evaluate(`Boolean(document.querySelector('dialog[open]'))`)), `${where}: Cancel sends nothing`);
}

const waitText = /starts again in \d+ s after the restore of the backup\. The devices keep their last target/;

/**
 * Picks the file, confirms, and follows the restart of the daemon to the end. waitUntil() is the time
 * until which the check answers as a daemon that waits after the import: 0 is a daemon that does not wait.
 */
export async function confirmRestore(tab, check, where, file, waitUntil = () => 0) {
	await pick(tab, file);
	await tab.evaluate(`${dialogButton('ok')}.click()`);
	const restarting = await tab.waitFor(`document.querySelector('[data-testid=backup-restarting]')`, 8000).then(() => true, () => false);
	check(restarting, `${where}: the section says that freedo restarts`);
	// The page must take the missing daemon as expected for the whole restart.
	let scary = false;
	let restored = false;
	let counted = false;
	let early = false;
	for (let i = 0; i < 300 && !restored; i++) {
		const state = await tab.evaluate(`({
			restored: Boolean(document.querySelector('[data-testid=backup-restored]')),
			wait: document.querySelector('main [data-testid=import-wait]')?.innerText ?? '',
			section: document.querySelector('[data-testid=backup-restarting]')?.innerText ?? '',
			failed: Boolean(document.querySelector('[data-testid=backup-error], [data-testid=backup-timeout]')),
			banner: [...document.querySelectorAll('main .notice, .toasts *')].some((n) => /No answer from freedo/.test(n.textContent)),
			gate: Boolean(document.querySelector('[data-testid="token-gate"]'))
		})`);
		if (state.banner || state.gate || state.failed) scary = true;
		restored = state.restored;
		if (waitText.test(state.wait) && state.section.trim() === state.wait.trim()) counted = true;
		if (restored && Date.now() < waitUntil()) early = true;
		if (!restored) await sleep(100);
	}
	check(restored, `${where}: the section says that the backup is restored`);
	if (waitUntil() > 0) {
		check(counted, `${where}: the banner and the section count the seconds of the wait after the restore`);
		check(!early, `${where}: the section says restored only after the wait`);
		check(
			await tab.waitFor(`!document.querySelector('[data-testid=import-wait]') && !document.querySelector('.content[inert], nav[inert]')`, 8000).then(() => true, () => false),
			`${where}: the page returns to normal by itself after the wait`
		);
	}
	check(!scary, `${where}: the restart shows no error, no "No answer" banner and no token form`);
}
