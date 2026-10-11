// Geometry of the dial. The arc is open at the bottom, like the mark. Angles run
// clockwise from the top, in degrees.

/** The arc starts at the bottom left. */
export const START = 220;
/** The arc covers 280 degrees. The gap at the bottom covers 80. */
export const SPAN = 280;

export interface Point {
	x: number;
	y: number;
}

export const clamp = (v: number, min: number, max: number) => Math.min(max, Math.max(min, v));

export function valueToAngle(value: number, min: number, max: number): number {
	return START + SPAN * clamp((value - min) / (max - min), 0, 1);
}

export function polar(cx: number, cy: number, r: number, deg: number): Point {
	const rad = (deg * Math.PI) / 180;
	return { x: cx + r * Math.sin(rad), y: cy - r * Math.cos(rad) };
}

export function arcPath(cx: number, cy: number, r: number, from: number, to: number): string {
	const a = polar(cx, cy, r, from);
	const b = polar(cx, cy, r, to);
	const large = to - from > 180 ? 1 : 0;
	return `M ${a.x.toFixed(2)} ${a.y.toFixed(2)} A ${r} ${r} 0 ${large} 1 ${b.x.toFixed(2)} ${b.y.toFixed(2)}`;
}

/** The value under a point. A point in the gap snaps to the nearer end of the arc. */
export function pointToValue(
	point: Point,
	center: Point,
	min: number,
	max: number,
	step: number
): number {
	let deg = (Math.atan2(point.x - center.x, -(point.y - center.y)) * 180) / Math.PI;
	if (deg < 0) deg += 360;
	let offset = deg - START;
	if (offset < 0) offset += 360;
	if (offset > SPAN) offset = offset - SPAN < (360 - SPAN) / 2 ? SPAN : 0;
	return snap(min + (offset / SPAN) * (max - min), min, max, step);
}

export function snap(value: number, min: number, max: number, step: number): number {
	const stepped = Math.round((value - min) / step) * step + min;
	// Two decimals remove the float noise of a step such as 0.5.
	return clamp(Math.round(stepped * 100) / 100, min, max);
}
