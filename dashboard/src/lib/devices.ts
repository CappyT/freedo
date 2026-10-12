import type { Health, NodeView, ZoneKind } from './types.ts';
import { firmwareOf, type AlertKey, type ZoneModel } from './zones.ts';

export interface DeviceRow {
	short: string;
	label: string;
	serial: string;
	kind: string;
	zones: { number: number; name: string; kind: ZoneKind }[];
	batteryPct: number | null;
	batteryMV: number | null;
	lastSeenUnix: number;
	firmware: string | null;
	offline: boolean;
	alerts: AlertKey[];
	/** Null on a device with no child lock. */
	childLock: boolean | null;
	canIdentify: boolean;
}

/** One row for each device the installation declares. */
export function buildDevices(nodes: NodeView[], health: Health | null, models: ZoneModel[]): DeviceRow[] {
	const nodeByShort = new Map(nodes.map((n) => [n.Short, n]));
	return (health?.devices ?? []).map((h) => {
		const node = nodeByShort.get(h.short);
		const zones = models
			.filter((m) => m.zone.members.includes(h.short))
			.map((m) => ({ number: m.zone.number, name: m.zone.name, kind: m.zone.kind }));
		const alerts: AlertKey[] = [];
		if (h.lowBattery) alerts.push('lowBattery');
		if (h.fault) alerts.push('deviceFault');
		if ((h.commsWarnings?.length ?? 0) > 0) alerts.push('commsWarning');
		if (h.offline) alerts.push('deviceOffline');
		return {
			short: h.short,
			label: h.label || h.device,
			serial: h.device,
			kind: h.kind,
			zones,
			batteryPct: h.batteryMV > 0 ? (node?.BatteryPct ?? null) : null,
			batteryMV: h.batteryMV > 0 ? h.batteryMV : null,
			lastSeenUnix: node?.LastSeenUnix ?? 0,
			firmware: firmwareOf(node),
			offline: h.offline,
			alerts,
			childLock: node?.ChildLock ?? null,
			canIdentify: h.kind === 'valve' && zones.length > 0
		};
	});
}
