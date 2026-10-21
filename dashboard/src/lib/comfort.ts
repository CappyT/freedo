// The air comfort of a room from its temperature and relative humidity. The ranges follow the
// heating-season values of EN 16798-1 category II, the band the norm sets for a normal level
// of expectation: 20 to 24 C, and 30 to 60 % humidity. Above 70 % for long, mould grows on
// cold walls, so that band has its own verdict.

export const COMFORT = { coldBelowC: 20, warmAboveC: 24, dryBelowPct: 30, humidAbovePct: 60, mouldFromPct: 70 };

export type TempVerdict = 'cold' | 'ok' | 'warm';
export type HumidityVerdict = 'dry' | 'ok' | 'humid' | 'mould';
/** One word for the room, the most urgent finding first. */
export type ComfortVerdict = 'pleasant' | 'cold' | 'warm' | 'dry' | 'humid' | 'mould';

export interface Comfort {
	temp: TempVerdict | null;
	humidity: HumidityVerdict | null;
	verdict: ComfortVerdict;
}

export function tempVerdict(tempC: number): TempVerdict {
	if (tempC < COMFORT.coldBelowC) return 'cold';
	return tempC > COMFORT.warmAboveC ? 'warm' : 'ok';
}

export function humidityVerdict(pct: number): HumidityVerdict {
	if (pct < COMFORT.dryBelowPct) return 'dry';
	if (pct >= COMFORT.mouldFromPct) return 'mould';
	return pct > COMFORT.humidAbovePct ? 'humid' : 'ok';
}

/** Null while the room reports neither value. Mould comes first, then humidity, then temperature. */
export function comfortOf(tempC: number | null, humidityPct: number | null): Comfort | null {
	const temp = tempC === null ? null : tempVerdict(tempC);
	const humidity = humidityPct === null ? null : humidityVerdict(humidityPct);
	if (temp === null && humidity === null) return null;
	const verdict: ComfortVerdict =
		humidity === 'mould' || humidity === 'humid' || humidity === 'dry' ? humidity : temp === 'cold' || temp === 'warm' ? temp : 'pleasant';
	return { temp, humidity, verdict };
}
