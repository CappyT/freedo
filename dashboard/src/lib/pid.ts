import { duration } from './format.ts';
import type { Pid, PidSuggestion } from './types.ts';

// The limits of the daemon: docs/API.md, "Zone options".
export const PID_MAX = 65535;
export const DEFAULT_PID: Pid = { kp: 1000, ki: 500, kd: 1500 };

export const samePid = (a: Pid, b: Pid) => a.kp === b.kp && a.ki === b.ki && a.kd === b.kd;

/** A zone without stored gains serves the defaults. */
export const gainsOf = (pid: Pid | null | undefined): Pid => pid ?? DEFAULT_PID;

export type PidIssue = 'range' | 'zero';

/** The gains in the three fields, or the reason why the daemon would refuse them. */
export function parsePid(text: { kp: string; ki: string; kd: string }): { pid: Pid } | { issue: PidIssue } {
	const values = [text.kp, text.ki, text.kd].map((v) => (/^\s*\d+\s*$/.test(v) ? Number(v) : NaN));
	if (values.some((v) => !Number.isInteger(v) || v > PID_MAX)) return { issue: 'range' };
	const [kp, ki, kd] = values as [number, number, number];
	return kp === 0 && ki === 0 ? { issue: 'zero' } : { pid: { kp, ki, kd } };
}

/** 7800 gives "2 h 10 min"; under a minute the result is in seconds. */
export function spanText(seconds: number): string {
	return seconds < 60 ? `${Math.round(seconds)} s` : duration(seconds);
}

export type SuggestionView =
	| { state: 'none'; heatUsed: number; minEpisodes: number; noEpisodes: boolean }
	| { state: 'weak' | 'confident'; gains: Pid; heatUsed: number; minEpisodes: number; r2: number | null; reason: string | null };

const clamp = (v: number) => Math.min(PID_MAX, Math.max(0, Math.round(v)));

/** The three states of the suggestion: none yet, a weak estimate, a confident one. The gains are whole numbers. */
export function suggestionView(s: PidSuggestion): SuggestionView {
	const { heatUsed, minEpisodes } = s;
	if (s.suggested === null) return { state: 'none', heatUsed, minEpisodes, noEpisodes: !s.episodes.some((e) => e.kind === 'heat') };
	const gains = { kp: clamp(s.suggested.kp), ki: clamp(s.suggested.ki), kd: clamp(s.suggested.kd) };
	// A reason from the daemon (a device-owned zone whose controller period is not verified) makes any estimate weak.
	const reason = s.reason || null;
	return { state: s.confident && !reason ? 'confident' : 'weak', gains, heatUsed, minEpisodes, r2: s.model?.r2 ?? null, reason };
}
