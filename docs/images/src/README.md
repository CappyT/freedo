# README images

`screenshots.mjs` makes the three images in `docs/images` from the real dashboard. It seeds a daemon
with the test installation of `tado/testdata`, replays its frames and changes the answers of the API
to a demo state: readable device names, a schedule for every zone, two profiles and a varied state
of the rooms. It captures the pages in English, then renders the compositions of `compose/` at 2x.
The data in the images is invented.

It needs a daemon binary that embeds the current dashboard, and Chrome (`$CHROME`, or the first
Chrome on `PATH`):

```sh
cd dashboard && npm run build && npm run embed && cd ..
go build -o /tmp/freedo ./cmd/freedo
node docs/images/src/screenshots.mjs /tmp/freedo
```

| Composition | Image | Captures |
|---|---|---|
| `compose/hero.html` | `hero.png`, at the top of the README | home and room, light; home, dark |
| `compose/tour.html` | `dashboard-tour.png` | home, room, programs and devices, light |
| `compose/desktop.html` | `dashboard-desktop.png` | home on a wide screen and a room, dark |

The captures go to `raw/`, which git ignores. `compose/phones.js` draws each phone with a status bar
in the top colour of its screen. Update the colours there when a screen changes its top colour.
