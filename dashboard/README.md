# freedo dashboard

The web interface of the daemon. It is a SvelteKit app that the build turns into static files.
The daemon embeds the files and serves them. No Node process runs at run time.

## Commands

The image and CI use the Node line in `ARG NODE_VERSION` of the `Dockerfile` (24, the current LTS line).
SvelteKit 3 needs Node 22.17 or later. Run the commands in this directory.

| Command | Effect |
|---|---|
| `npm ci` | Install the dependencies from `package-lock.json`. |
| `npm run check` | Type-check the TypeScript and the Svelte files. |
| `npm test` | Run the unit tests (`tests/`). |
| `npm run build` | Build into `build/`. |
| `npm run embed` | Copy `build/` to `../internal/web/dist`, the directory that the daemon embeds. |
| `npm run dev` | Start the development server. `FREEDO_DEV_API` names the daemon (default `http://127.0.0.1:8080`). |
| `npm run prefix-check` | Check the build under a path prefix. See below. |
| `npm run ui-check` | Drive the controls in headless Chrome against the daemon: typing in the schedule editor, a refused command, the first end rule, Turn off, the profiles on the Programs screen, the rooms and devices, the first setup on an empty database, the download and the restore of a backup, the banner and the return to normal of a daemon that waits after an import. `FREEDO_BIN` names a daemon binary that embeds the build. |

To run the daemon with the dashboard:

```sh
npm ci && npm run build && npm run embed
cd .. && go build -o freedo ./cmd/freedo
```

A `go build` without `npm run embed` has no dashboard: `internal/web/dist` holds only `.gitkeep`
and every page request gets `404`. The API works as before.

## Layout

- `src/routes`: the pages. `/` is the home page, then `zones/[n]`, `hot-water`, `programs`, `boiler`,
  `boiler/setup`, `devices`, `rooms/[n]`, `setup` and `settings`. `programs` holds the profiles and the home and away
  schedule of every zone in each profile. The home page shows one tile per room, in the colour of its
  state, and the tiles of the presence, the active profile, the hot water and the boiler. The hot water
  page and the boiler page have no navigation item: their tiles on the home page lead there. A room,
  the hot water and the boiler open on a screen in the colour of their state, and the tile grows into
  that screen through a view transition (`onNavigate` in `+layout.svelte`, the name `hero` in `app.css`).
- `devices` lists the devices room by room, with No room as the last group. A device line opens in place
  on a tap. The page adds a room, pairs a device and manages the heating circuits. `rooms/[n]` holds the settings of one room: the name, the members
  with rename, move and removal, the circuit, the measuring device, the demand reporter, the room options
  and the delete. `src/lib/roomoptions.ts` holds the ranges and builds the patch of the room options.
  `rooms/none` is the same page for the devices in no room. `setup` is the first
  setup: one card for each step, with the state that `GET /api/health` reports. Rooms and devices and Settings link to it,
  and so does the home page while the installation has no room.
- `boiler` shows the live values, the flow limit, the energy and fuel estimate and the gas meter readings
  (`GasMeter.svelte`, rules in `meter.ts`). `boiler/setup` writes the boiler data, the flow limit, the
  weather curves and the adaptation; `boileroptions.ts` holds the ranges, builds the patch and mirrors the
  curves of the daemon for the preview in `CurveChart.svelte`. The room screen judges the air comfort from
  the temperature and the humidity (`AirComfort.svelte`, ranges in `comfort.ts`).
- `src/lib/api.ts`: the client of the HTTP API. `data.svelte.ts` polls the views every 5 seconds.
- `src/lib/install.ts`: the rules of the installation pages: the number of a new zone, the writes of a move,
  the state of each setup step. `install.svelte.ts` runs a change and reads the state again.
- `src/lib/schedule.ts`: the rules and the operations of the schedule editor. `today.svelte.ts` reads the
  program of today on the clock of the installation and names its next change.
- `src/lib/backup.ts`: the download and the upload of a backup, and the wait for the restart of the
  daemon. The section Backup of `settings` uses it.
- `src/lib/importwait.ts`: the countdown of a daemon that waits after an import. Such a daemon
  answers `GET /api/health` with `importWait` and every other call with `503`. `data.svelte.ts` then
  reads the health alone, `Banners.svelte` shows the seconds that are left, and the layout makes the
  page and the navigation inert, so no page sends a command. A page that opens in the wait shows the
  banner alone.
- `src/lib/i18n`: the texts. `en.ts` is the reference. `it.ts`, `de.ts` and `fr.ts` must define the
  same keys, and the type checker enforces it. The page picks the language from the browser.
- `src/lib/fonts`: the fonts, with their licences. See the README in that directory.
- `../assets/brand/tokens.css`: the colours, the type and the themes. The dashboard imports it and
  defines none of them. `scripts/brand.mjs` copies the web icons from `../assets/brand` into `static/`.

## One build for any path prefix

The daemon serves the dashboard at its root. Home Assistant ingress serves it under
`/api/hassio_ingress/<token>/`, a prefix that is not known at build time. Three rules make one build
work in both places:

- The router type is `hash`. The route is in the URL fragment (`#/zones/2`), so the document is always
  the one file `index.html` at the root of the app.
- Every URL is relative to the document. The API client resolves `api/...` against `document.baseURI`.
  The static files use `./`.
- SvelteKit writes the URLs of the entry page as `/_app/...` and ignores `paths.relative` for that
  file. `scripts/relocate.mjs` rewrites them to `./_app/...` after the build and fails when a
  root-relative URL is left.

The daemon redirects a nested path that no file matches to the root of the app, with a relative
`Location`, so a mistyped URL does not load the page with broken paths.

`npm run prefix-check` proves it. It starts the daemon given by `FREEDO_BIN` (a binary that embeds the
build) behind a proxy that strips a random `/api/hassio_ingress/...` prefix and answers 404 to every
other path. It loads every file that the page references, and, when Chrome is on the machine (or
`CHROME` names it), it runs the app under the prefix, opens each page, and downloads and restores a
backup.

## Access

The static files need no token. The API keeps its rules. In a standalone container the page asks for
the API token once and keeps it in the browser storage. Under Home Assistant ingress the daemon trusts
the gateway for the calls that `src/lib/api.ts` makes, so the page asks for nothing. The calls are listed in
`../internal/daemon/ingress_routes.json`, and `tests/ingress.test.ts` fails when the client calls a
pair that is not in the list. The test also holds the exact allowed set, and a list of pairs that must
stay behind the token: the identity of the network, the network key, `POST /api/pair` with its key field,
calibration, firmware and debug. A new client call needs a line in the JSON file and in the test, and every
line widens what a Home Assistant user can do without the token. The list holds the installation and
pairing calls, so the pages Rooms and devices and First setup work under the add-on. Where the wider route also
writes a field that ingress must not reach, the client calls a route that carries one field:
`PUT /api/install/timezone`, `PUT /api/install/boiler`, `PUT /api/install/devices/{ref}/zone` and
`POST /api/pair/devices/{ref}`.

The backup is a class of its own. `src/lib/backup.ts` calls `GET /api/state/export` and
`POST /api/state/import`, which are not in that list and are not methods of the client in `api.ts`. They
are the two pairs of `../internal/daemon/ingress_admin_routes.json`: the file holds the network key, so
under the add-on the daemon serves them to a Home Assistant administrator only, and elsewhere they need
the token. `tests/backup.test.ts` pins the two pairs and checks that they are in no other list. The
section Backup of Settings shows a `403` as "only a Home Assistant administrator" and a `401` as a
missing token, and does not open the token form of the whole page.
See [`docs/OPERATION.md`](../docs/OPERATION.md#dashboard).
