# Fonts

The dashboard serves its fonts itself. It never requests a font from an external host.

All four files are the `latin` subset in WOFF2, taken unchanged from the npm packages of the
Fontsource project at version 5.3.0. The upstream fonts are licensed under the SIL Open Font License 1.1.
The full text of each licence is in the `LICENSE-*.txt` file next to the font.

| File | Family | Source package | Upstream project | Licence |
| --- | --- | --- | --- | --- |
| `sora-latin-wght-normal.woff2` | Sora, variable weight 100 to 800 | `@fontsource-variable/sora` | https://github.com/sora-xor/sora-font | OFL 1.1, `LICENSE-sora.txt` |
| `ibm-plex-sans-latin-wght-normal.woff2` | IBM Plex Sans, variable weight | `@fontsource-variable/ibm-plex-sans` | https://github.com/IBM/plex | OFL 1.1, `LICENSE-ibm-plex-sans.txt` |
| `ibm-plex-mono-latin-400-normal.woff2` | IBM Plex Mono 400 | `@fontsource/ibm-plex-mono` | https://github.com/IBM/plex | OFL 1.1, `LICENSE-ibm-plex-mono.txt` |
| `ibm-plex-mono-latin-500-normal.woff2` | IBM Plex Mono 500 | `@fontsource/ibm-plex-mono` | https://github.com/IBM/plex | OFL 1.1, `LICENSE-ibm-plex-mono.txt` |

The `latin` subset covers English, Italian, German and French.

The licence asks that its text accompany every copy of a font. The daemon embeds the fonts, so the
build copies the `LICENSE-*.txt` files to `licenses/` in the bundle (`scripts/brand.mjs`). The daemon
serves them at `licenses/LICENSE-sora.txt`, `licenses/LICENSE-ibm-plex-sans.txt` and
`licenses/LICENSE-ibm-plex-mono.txt`, next to `index.html`. `npm run prefix-check` checks it.
