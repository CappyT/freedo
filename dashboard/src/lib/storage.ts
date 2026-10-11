// Browser storage can be missing or throw: in a private window, with site data blocked,
// or in a frame without access. The dashboard works without it and loses the saved values.

export function readStored(key: string): string | null {
	try {
		return globalThis.localStorage?.getItem(key) ?? null;
	} catch {
		return null;
	}
}

export function writeStored(key: string, value: string | null): void {
	try {
		if (value === null) globalThis.localStorage?.removeItem(key);
		else globalThis.localStorage?.setItem(key, value);
	} catch {
		// The value stays in memory for this page.
	}
}
