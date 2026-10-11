<script lang="ts" module>
	// 24 x 24 outline icons drawn for freedo. Each entry lists the paths of one icon.
	const paths = {
		home: ['M3 11 12 4l9 7v9a1 1 0 0 1-1 1h-5v-6H9v6H4a1 1 0 0 1-1-1z'],
		drop: ['M12 3c3.5 4.2 6 7.3 6 10.5a6 6 0 0 1-12 0C6 10.3 8.5 7.2 12 3z'],
		flame: ['M12 3c1 3.5 5 5.5 5 10a5 5 0 0 1-10 0c0-2 1-3.2 2-4.2.2 1.6 1 2.4 2 2.7-.6-3 .2-6 1-8.5z'],
		devices: ['M7 7h10v10H7z', 'M10 3v4M14 3v4M10 17v4M14 17v4M3 10h4M3 14h4M17 10h4M17 14h4'],
		sliders: ['M4 7h9M19 7h1M4 17h1M11 17h9', 'M13 7a3 3 0 1 0 6 0 3 3 0 1 0-6 0', 'M5 17a3 3 0 1 0 6 0 3 3 0 1 0-6 0'],
		idle: ['M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18z', 'M8 12.5l2.7 2.7L16 9.5'],
		away: ['M3 11l9-7 9 7', 'M5 10v10h6', 'M14 15h7M18 12l3 3-3 3'],
		power: ['M12 3v8', 'M7 6.5a7 7 0 1 0 10 0'],
		window: ['M5 4h14v16H5z', 'M12 4v16M5 12h14'],
		offline: ['M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18z', 'M5.6 5.6l12.8 12.8'],
		fault: ['M8 3h8l5 5v8l-5 5H8l-5-5V8z', 'M9.5 9.5l5 5M14.5 9.5l-5 5'],
		warning: ['M12 3 2.5 20h19z', 'M12 10v4.5M12 17.5h.01'],
		battery: ['M3 8h15v8H3z', 'M20 11v2'],
		lock: ['M6 11h12v9H6z', 'M8.5 11V8a3.5 3.5 0 0 1 7 0v3'],
		plus: ['M12 5v14M5 12h14'],
		minus: ['M5 12h14'],
		bolt: ['M13 2 4 14h7l-1 8 9-12h-7z'],
		clock: ['M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18z', 'M12 7v5l3 2'],
		thermo: ['M10 4a2 2 0 1 1 4 0v9.2a4 4 0 1 1-4 0z'],
		check: ['M5 12.5l4.5 4.5L19 7.5'],
		close: ['M6 6l12 12M18 6 6 18'],
		refresh: ['M20 11a8 8 0 1 0-2.3 5.7', 'M20 5v6h-6'],
		bell: ['M6 9a6 6 0 0 1 12 0c0 6 2 7 2 7H4s2-1 2-7', 'M10 19a2 2 0 0 0 4 0'],
		calendar: ['M4 6h16v14H4z', 'M4 10h16M8 3v4M16 3v4'],
		chevron: ['M9 6l6 6-6 6'],
		back: ['M15 6l-6 6 6 6'],
		key: ['M8 15a4 4 0 1 1 3.5-6H21v3h-2v2h-3v-2h-4.5A4 4 0 0 1 8 15z'],
		trash: ['M5 7h14M10 7V4h4v3M7 7l1 13h8l1-13']
	} as const;

	export type IconName = keyof typeof paths;
</script>

<script lang="ts">
	let { name, size = 20 }: { name: IconName; size?: number } = $props();
</script>

<svg
	width={size}
	height={size}
	viewBox="0 0 24 24"
	fill="none"
	stroke="currentColor"
	stroke-width="2"
	stroke-linecap="round"
	stroke-linejoin="round"
	aria-hidden="true"
	focusable="false"
>
	{#each paths[name] as d (d)}
		<path {d} />
	{/each}
</svg>
