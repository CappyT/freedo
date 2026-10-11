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

To run the daemon with the dashboard:

```sh
npm ci && npm run build && npm run embed
cd .. && go build -o freedo ./cmd/freedo
```

A `go build` without `npm run embed` has no dashboard: `internal/web/dist` holds only `.gitkeep`
and every page request gets `404`. The API works as before.

## Layout

- `src/routes`: the pages. `/` is the home page, then `zones/[n]`, `hot-water`, `boiler`,
  `devices` and `settings`.
- `src/lib/api.ts`: the client of the HTTP API. `data.svelte.ts` polls the views every 5 seconds.
- `src/lib/schedule.ts`: the rules and the operations of the schedule editor.
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
`CHROME` names it), it runs the app under the prefix and opens each page.

## Access

The static files need no token. The API keeps its rules. In a standalone container the page asks for
the API token once and keeps it in the browser storage. Under Home Assistant ingress the daemon trusts
the gateway, so the page asks for nothing. See [`docs/OPERATION.md`](../docs/OPERATION.md#dashboard).
