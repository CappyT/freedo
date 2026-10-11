import { fmt, t, type Key } from './i18n.svelte.ts';
import type { Overlay, Target } from './types.ts';
import type { AlertKey, ZoneModel } from './zones.ts';

/** One line that says what the zone follows now and until when. */
export function modeLine(target: Target): string {
	const o = target.overlay;
	if (o) return overlayLine(o, target.boost);
	if (target.source === 'window') return t('mode.window');
	if (target.source === 'away') return t('mode.away');
	// source is "program" also for a zone with no program: hasProgram tells the two apart.
	if (target.known && target.hasProgram === false) return t('mode.noProgram');
	return target.known ? t('mode.schedule') : t('mode.waiting');
}

export function overlayLine(o: Overlay, boost: boolean): string {
	if (boost) return o.endsAt ? t('mode.boostUntil', { time: fmt.when(o.endsAt) }) : t('mode.boost');
	if (o.endsAt) return t('mode.until', { time: fmt.when(o.endsAt) });
	switch (o.rule) {
		case 'until_next':
			return t('mode.untilNext');
		case 'until_auto_change':
			return t('mode.untilAuto');
		default:
			return t('mode.manual');
	}
}

/** The wanted temperature as text: a number, or the reason there is none. */
export function targetText(model: ZoneModel): string {
	const target = model.zone.target;
	if (!target.on) return t('target.off');
	if (target.source === 'window') return t('target.window');
	if (target.targetC === null) return target.known ? t('target.none') : '-';
	return fmt.temp(target.targetC);
}

export const alertKeys: Record<AlertKey, Key> = {
	lowBattery: 'alert.lowBattery',
	commsWarning: 'alert.commsWarning',
	fallback: 'alert.fallback',
	deviceOffline: 'alert.deviceOffline',
	deviceFault: 'alert.deviceFault',
	windowUndelivered: 'alert.windowUndelivered'
};
