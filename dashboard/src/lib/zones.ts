import type { DeclaredZone, Health, HealthDevice, NodeView, ZoneDemand, ZoneFallback } from './types.ts';

/** The colour family of a status token. Each one is shown with an icon and a label. */
export type Tone = 'heating' | 'idle' | 'away' | 'off' | 'warning' | 'fault' | 'offline';

/** What the zone does now. The label key selects the text. */
export type StateKey = 'heating' | 'idle' | 'on' | 'away' | 'off' | 'windowOpen' | 'offline' | 'fault';

export type AlertKey = 'lowBattery' | 'commsWarning' | 'fallback' | 'deviceOffline' | 'deviceFault' | 'windowUndelivered';

export interface ZoneState {
	state: StateKey;
	tone: Tone;
	alerts: AlertKey[];
}

export interface Member {
	short: string;
	label: string;
	kind: string;
	node: NodeView | undefined;
	health: HealthDevice | undefined;
}

export interface ZoneModel {
	zone: DeclaredZone;
	tempC: number | null;
	humidityPct: number | null;
	demandPct: number;
	members: Member[];
	status: ZoneState;
}

export const stateTone: Record<StateKey, Tone> = {
	heating: 'heating',
	idle: 'idle',
	on: 'idle',
	away: 'away',
	off: 'off',
	windowOpen: 'warning',
	offline: 'offline',
	fault: 'fault'
};

/**
 * One state per zone, by priority: unreachable, faulty, window open, off, heating,
 * away, idle. Other problems stay as alerts next to the state.
 */
export function zoneStatus(
	zone: DeclaredZone,
	demand: ZoneDemand | undefined,
	members: Member[],
	away: boolean,
	fallbacks: ZoneFallback[]
): ZoneState {
	const alerts: AlertKey[] = [];
	const healths = members.map((m) => m.health).filter((h): h is HealthDevice => h !== undefined);
	const allOffline = healths.length > 0 && healths.length === members.length && healths.every((h) => h.offline);
	if (!allOffline && healths.some((h) => h.offline)) alerts.push('deviceOffline');
	if (healths.some((h) => h.fault)) alerts.push('deviceFault');
	if (healths.some((h) => h.lowBattery)) alerts.push('lowBattery');
	if (healths.some((h) => (h.commsWarnings?.length ?? 0) > 0)) alerts.push('commsWarning');
	if (fallbacks.some((f) => f.zone === zone.number)) alerts.push('fallback');
	if ((zone.window?.undelivered.length ?? 0) > 0) alerts.push('windowUndelivered');

	const windowOpen = zone.kind === 'heating' && (zone.window?.open === true || zone.target.source === 'window');
	let state: StateKey;
	if (allOffline) state = 'offline';
	else if (alerts.includes('deviceFault')) state = 'fault';
	else if (windowOpen) state = 'windowOpen';
	else if (!zone.target.on) state = 'off';
	else if ((demand?.DemandPct ?? 0) > 0 && zone.kind === 'heating') state = 'heating';
	else if (away && zone.kind === 'heating') state = 'away';
	else state = zone.kind === 'hotwater' ? 'on' : 'idle';
	return { state, tone: stateTone[state], alerts };
}

/** Joins the zone list with the device views and the health lists. */
export function buildZones(
	declared: DeclaredZone[],
	demand: ZoneDemand[],
	nodes: NodeView[],
	health: Health | null,
	away: boolean
): ZoneModel[] {
	const nodeByShort = new Map(nodes.map((n) => [n.Short, n]));
	const healthByShort = new Map((health?.devices ?? []).map((d) => [d.short, d]));
	const fallbacks = health?.zoneFallback ?? [];
	return declared.map((zone) => {
		const members: Member[] = zone.members.map((short) => {
			const node = nodeByShort.get(short);
			const h = healthByShort.get(short);
			return { short, label: h?.label || node?.Room || short, kind: h?.kind ?? '', node, health: h };
		});
		const d = demand.find((z) => z.Number === zone.number);
		return {
			zone,
			tempC: zoneTemperature(zone, d, nodeByShort),
			humidityPct: zoneHumidity(zone, members),
			demandPct: d?.DemandPct ?? 0,
			members,
			status: zoneStatus(zone, d, members, away, fallbacks)
		};
	});
}

/** The zone reports its own temperature. Until it does, the measuring device gives one. */
export function zoneTemperature(
	zone: DeclaredZone,
	demand: ZoneDemand | undefined,
	nodes: Map<string, NodeView>
): number | null {
	if (zone.kind !== 'heating') return null;
	if (demand && demand.UpdatedUnix > 0 && demand.TempC > 0) return demand.TempC;
	const sensor = zone.measuringDevice ? nodes.get(zone.measuringDevice) : undefined;
	return sensor && sensor.TempC > 0 ? sensor.TempC : null;
}

export function zoneHumidity(zone: DeclaredZone, members: Member[]): number | null {
	if (zone.kind !== 'heating') return null;
	const sensor = members.find((m) => m.short === zone.measuringDevice);
	const first = [sensor, ...members].find((m) => (m?.node?.HumidityPct ?? 0) > 0);
	return first?.node?.HumidityPct ?? null;
}

/** Firmware shown for a device: the running version, else the build tag, else null. */
export function firmwareOf(node: NodeView | undefined): string | null {
	const t = node?.Telemetry;
	return t?.fw_run_version || t?.fw_build_hash || null;
}
