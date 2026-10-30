import type { InstallZone, ZoneOptionsPatch } from './types.ts';

// The limits of the daemon: docs/API.md, "Zone options".
export const WINDOW_MINUTES = { min: 1, max: 1092 };
export const END_MINUTES = { min: 1, max: 7 * 24 * 60 };
export const AWAY_C = { min: 5, max: 25 };
export const OFFSET_C = { min: -10, max: 10 };
const DEFAULT_END_MINUTES = 60;

export type EndRuleOption = 'manual' | 'until_next' | 'timer';
export type OffsetMode = 'config' | 'dbg2';

/** The options of a heated room as the form holds them: minutes where the daemon holds seconds. */
export interface RoomOptions {
	windowDetection: boolean;
	windowMinutes: number;
	/** The daemon reads 5 C, and no value, as off while the home is away. */
	awayC: number;
	endRule: EndRuleOption;
	endMinutes: number;
	earlyStart: boolean;
	/** How a manual offset reaches the valve. */
	offsetMode: OffsetMode;
}

export function optionsOf(zone: InstallZone): RoomOptions {
	return {
		windowDetection: zone.open_window_detection,
		windowMinutes: Math.round(zone.open_window_duration_sec / 60),
		awayC: zone.away_temperature_c ?? AWAY_C.min,
		endRule: zone.device_overlay_end ?? 'until_next',
		endMinutes: zone.device_overlay_end_sec ? Math.round(zone.device_overlay_end_sec / 60) : DEFAULT_END_MINUTES,
		earlyStart: zone.early_start,
		offsetMode: zone.offset_mode ?? 'config'
	};
}

export type OptionIssue = 'window' | 'away' | 'endTimer' | 'offset';

const whole = (v: number, range: { min: number; max: number }) => Number.isInteger(v) && v >= range.min && v <= range.max;
const twoDecimals = (v: number) => Math.abs(v * 100 - Math.round(v * 100)) < 1e-9;

/**
 * The offset the person typed: a number, null for an empty field (no change), or undefined for a
 * text that is not an offset the daemon takes.
 */
export function parseOffset(text: string): number | null | undefined {
	const clean = text.trim().replace(',', '.');
	if (clean === '') return null;
	const value = Number(clean);
	if (!Number.isFinite(value) || value < OFFSET_C.min || value > OFFSET_C.max || !twoDecimals(value)) return undefined;
	return value;
}

/** What the daemon would refuse in the form. */
export function optionIssues(o: RoomOptions, offset: string): OptionIssue[] {
	const issues: OptionIssue[] = [];
	if (!whole(o.windowMinutes, WINDOW_MINUTES)) issues.push('window');
	if (!(o.awayC >= AWAY_C.min && o.awayC <= AWAY_C.max && twoDecimals(o.awayC))) issues.push('away');
	if (o.endRule === 'timer' && !whole(o.endMinutes, END_MINUTES)) issues.push('endTimer');
	if (parseOffset(offset) === undefined) issues.push('offset');
	return issues;
}

/** The fields of PATCH /api/install/zones/{n} that differ from what the zone holds. Empty when none does. */
export function optionsPatch(zone: InstallZone, o: RoomOptions): ZoneOptionsPatch {
	const now = optionsOf(zone);
	const patch: ZoneOptionsPatch = {};
	if (o.windowDetection !== now.windowDetection) patch.open_window_detection = o.windowDetection;
	if (o.windowMinutes !== now.windowMinutes) patch.open_window_duration_sec = o.windowMinutes * 60;
	if (o.awayC !== now.awayC) patch.away_temperature_c = o.awayC;
	if (o.endRule !== now.endRule || (o.endRule === 'timer' && o.endMinutes !== now.endMinutes)) {
		patch.device_overlay_end = o.endRule;
		if (o.endRule === 'timer') patch.device_overlay_end_sec = o.endMinutes * 60;
	}
	if (o.earlyStart !== now.earlyStart) patch.early_start = o.earlyStart;
	if (o.offsetMode !== now.offsetMode) patch.offset_mode = o.offsetMode;
	return patch;
}
