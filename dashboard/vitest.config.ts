import { defineConfig } from 'vitest/config';

// The tests cover plain TypeScript modules, so the SvelteKit plugin is not loaded.
export default defineConfig({
	test: { include: ['tests/**/*.test.ts'] }
});
