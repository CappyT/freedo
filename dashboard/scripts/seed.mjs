// Seeds a daemon with the synthetic installation of tado/testdata through /api/install.
// The network key, the EUIs and the serial numbers are invented fixture values. A daemon
// that replays tado/testdata/frames_69ec.txt then shows the readings of this installation.
//
// Usage: node scripts/seed.mjs <http://127.0.0.1:PORT> [token]

import { fileURLToPath } from 'node:url';

const KEY = '0f1e2d3c4b5a69788796a5b4c3d2e1f0';

const devices = [
	{ kind: 'thermostat', eui: '05a2cafa07c51b00', serial: 'RU1000000001', label: 'hall thermostat' },
	{ kind: 'valve', eui: '02a6cefa07c51b00', serial: 'VA1000000005', label: 'bedroom' },
	{ kind: 'valve', eui: '04a3defa07c51b00', serial: 'VA1000000007', label: 'bath' },
	{ kind: 'valve', eui: '07a5cefa07c51b00', serial: 'VA1000000003', label: 'hall valve' },
	{ kind: 'valve', eui: '0aa7cefa07c51b00', serial: 'VA1000000004', label: 'living valve' },
	{ kind: 'valve', eui: '03a8cafa07c51b00', label: 'living sensor valve' },
	{ kind: 'valve', eui: '010acefa07c51b00', serial: 'VA1000000006', label: 'kitchen valve left' },
	{ kind: 'valve', eui: '060acefa07c51b00', serial: 'VA1000000001', label: 'kitchen valve right' },
	{ kind: 'valve', eui: '08a4defa07c51b00', serial: 'VA1000000002', label: 'office' }
];

const zones = [
	{ number: 0, name: 'Hot water', kind: 'hotwater', members: ['RU1000000001'] },
	{ number: 1, name: 'Hall', kind: 'heating', members: ['RU1000000001', 'VA1000000003'], reporter: 'RU1000000001' },
	{ number: 2, name: 'Bath', kind: 'heating', members: ['VA1000000007'], reporter: 'VA1000000007' },
	{ number: 3, name: 'Office', kind: 'heating', members: ['VA1000000002'], reporter: 'VA1000000002' },
	{ number: 5, name: 'Kitchen', kind: 'heating', members: ['VA1000000001', 'VA1000000006'], reporter: 'VA1000000006' },
	{ number: 8, name: 'Living room', kind: 'heating', members: ['VA1000000004', '03a8cafa07c51b00'], reporter: '03a8cafa07c51b00' },
	{ number: 10, name: 'Bedroom', kind: 'heating', members: ['VA1000000005'], reporter: 'VA1000000005' }
];

export async function seed(base, token) {
	const headers = { 'Content-Type': 'application/json' };
	if (token) headers.Authorization = 'Bearer ' + token;
	async function call(method, path, body) {
		const response = await fetch(new URL(path, base), { method, headers, body: JSON.stringify(body) });
		if (!response.ok) throw new Error(`${method} ${path}: ${response.status} ${await response.text()}`);
	}

	await call('PATCH', '/api/install', {
		home_id: 7654321,
		coordinator: { eui: '09a1cefa07c51b00', serial: 'IB1000000001' }
	});
	await call('PUT', '/api/install/network_key', { key: KEY });
	for (const device of devices) await call('POST', '/api/install/devices', device);
	for (const { number, name, kind } of zones) await call('POST', '/api/install/zones', { number, name, kind });
	for (const zone of zones) {
		await call('PUT', `/api/install/zones/${zone.number}/members`, { members: zone.members });
		if (zone.reporter) await call('PUT', `/api/install/zones/${zone.number}/reporter`, { ref: zone.reporter });
	}
	await call('POST', '/api/install/circuits', { id: 1, name: 'House', heat_source: 'RU1000000001' });
	for (const zone of zones.filter((z) => z.kind === 'heating')) {
		await call('PATCH', `/api/install/zones/${zone.number}`, { circuit: 1 });
	}
	await call('PATCH', '/api/install', { utc_offset_min: 60, flow_limit_c: 55 });
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
	const [base, token] = process.argv.slice(2);
	if (!base) {
		console.error('usage: node scripts/seed.mjs <http://127.0.0.1:PORT> [token]');
		process.exit(2);
	}
	await seed(base, token);
	console.log('seeded', base);
}
