import { describe, expect, it } from 'vitest';
import { buildDevices } from '../src/lib/devices.ts';
import type { Health, NodeView } from '../src/lib/types.ts';
import { buildZones } from '../src/lib/zones.ts';
import { demand, device, zone } from './fixtures.ts';

describe('device rows', () => {
	it('join the health list, the node view and the zones', () => {
		const nodes = [
			{ Short: '0201', BatteryPct: 61, LastSeenUnix: 5, ChildLock: true, Telemetry: { fw_run_version: '2.9' } }
		] as unknown as NodeView[];
		const models = buildZones([zone()], [demand()], nodes, null, false);
		const [row] = buildDevices(nodes, { devices: [device({ lowBattery: true })] } as unknown as Health, models);
		expect(row).toMatchObject({
			short: '0201',
			label: 'bath',
			zones: [{ number: 2, name: 'Bath' }],
			batteryPct: 61,
			batteryMV: 2500,
			firmware: '2.9',
			childLock: true,
			canIdentify: true,
			alerts: ['lowBattery']
		});
	});

	it('name the faults, the valve warnings and the link warnings of the fault word', () => {
		const health = {
			devices: [
				device({ deviceWarnings: ['notMounted'] }),
				device({ short: '0202', fault: true, faults: ['stall'] }),
				device({ short: '0203', fault: true }),
				device({ short: '0204', deviceWarnings: ['notMounted'], commsWarnings: ['noConfigAnswer'] })
			]
		} as unknown as Health;
		const rows = buildDevices([], health, []);
		expect(rows[0]).toMatchObject({ faults: [], warnings: ['notMounted'], alerts: [] });
		expect(rows[1]).toMatchObject({ faults: ['stall'], warnings: [], alerts: [] });
		expect(rows[2]).toMatchObject({ faults: [], alerts: ['deviceFault'] });
		expect(rows[3]).toMatchObject({ warnings: ['notMounted', 'noConfigAnswer'], alerts: [] });
	});

	it('let a wall thermostat of a zone identify itself', () => {
		const models = buildZones([zone()], [demand()], [], null, false);
		const rows = buildDevices([], { devices: [device({ kind: 'thermostat', batteryMV: 0 })] } as unknown as Health, models);
		expect(rows[0]).toMatchObject({ canIdentify: true, childLock: null });
	});

	it('leave out what a device cannot do', () => {
		const rows = buildDevices([], { devices: [device({ kind: 'thermostat', batteryMV: 0 })] } as unknown as Health, []);
		expect(rows[0]).toMatchObject({ batteryMV: null, batteryPct: null, childLock: null, canIdentify: false, firmware: null });
	});
});
