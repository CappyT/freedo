import adapter from '@sveltejs/adapter-static';
import { sveltekit } from '@sveltejs/kit/vite';
import { defineConfig } from 'vite';

// FREEDO_DEV_API points `vite dev` at a running daemon.
const api = process.env.FREEDO_DEV_API ?? 'http://127.0.0.1:8080';

export default defineConfig({
	plugins: [
		sveltekit({
			adapter: adapter({ pages: 'build', assets: 'build' }),
			// The daemon serves one HTML file. The route lives in the URL fragment, so the file
			// loads from any path prefix, such as the Home Assistant ingress path.
			router: { type: 'hash' },
			// The update check polls a file that the daemon replaces with the whole container.
			version: { pollInterval: 0 }
		})
	],
	server: {
		proxy: { '/api': api },
		// tokens.css and the logos live in assets/brand at the repository root.
		fs: { allow: ['..'] }
	}
});
