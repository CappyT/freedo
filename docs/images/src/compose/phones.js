// Builds each phone from data-shot: the status bar in the top colour of that screen, then the shot.
const tops = {
	light: ['#F5F9FF', '#0B1B3A'],
	dark: ['#0B1B3A', '#FFFFFF'],
	room: ['#FDC6A3', '#512500'],
	boiler: ['#FDC6A3', '#512500'],
	water: ['#A8E3C0', '#003F22']
};
const icons =
	'<svg viewBox="0 0 64 12"><rect x="0" y="8" width="3" height="4" rx="1"/><rect x="5" y="5.5" width="3" height="6.5" rx="1"/><rect x="10" y="3" width="3" height="9" rx="1"/><rect x="15" y="0" width="3" height="12" rx="1"/>' +
	'<path d="M30 3.2a10 10 0 0 1 13 0l-1.4 1.5a8 8 0 0 0-10.2 0zM32.6 6a6 6 0 0 1 7.8 0L39 7.5a4 4 0 0 0-5 0zM35.2 8.8a2 2 0 0 1 2.6 0L36.5 10.2z"/>' +
	'<rect x="47" y="1" width="14" height="10" rx="2.6" fill="none" stroke="currentColor" stroke-width="1.2" opacity=".45"/><rect x="48.6" y="2.6" width="10.8" height="6.8" rx="1.6"/><rect x="62" y="4" width="1.6" height="4" rx=".8" opacity=".45"/></svg>';
for (const phone of document.querySelectorAll('.phone[data-shot]')) {
	const shot = phone.dataset.shot;
	const kind = ['room', 'boiler', 'water'].find((k) => shot.startsWith(k)) ?? (shot.endsWith('dark') ? 'dark' : 'light');
	const [top, ink] = tops[kind];
	phone.innerHTML = `<div class="screen" style="--top:${top};--ink:${ink}"><div class="status"><span>9:41</span><i class="island"></i>${icons}</div><img src="../raw/${shot}.png" alt="" /></div>`;
}
