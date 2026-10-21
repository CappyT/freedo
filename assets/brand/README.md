# freedo brand

The identity of freedo: the mark, the logo, the colours, the type and the design tokens.
`tokens.css` is the single source of the values. The dashboard imports it.

The mark is a thermostat dial left open at the bottom, with a setpoint knob and an `f` at its
centre.

tado is a trademark of its owner. The identity does not use tado artwork.

## Files

| File | Use |
| --- | --- |
| `svg/icon.svg` | Primary icon. Stores, launchers, docs. |
| `svg/icon-dark.svg` | Dark icon. Dark themes, terminals. |
| `svg/icon-light.svg` | Light icon. Light UIs, print. |
| `svg/icon-mono.svg` | One ink (Navy), no ground. Stickers, CLI. |
| `svg/favicon.svg` | Simplified icon for 32 px and 16 px. |
| `svg/logo.svg` | Primary lockup, horizontal. Use on light grounds. |
| `svg/logo-reversed.svg` | Reversed lockup. Use on Navy. |
| `svg/logo-stacked.svg` | Stacked lockup. |
| `svg/logo-mono.svg` | One-ink lockup. |
| `svg/social-preview.svg` | Source of the social preview. |
| `social-preview.png` | GitHub social preview, 1280 x 640. |
| `web/` | Web app icons for the dashboard. |
| `tokens.css` | Palette, type, radii and status colours as CSS custom properties. |

The SVG files are standalone. The wordmark is converted to outlines, so no font is needed to
show it. Other consumers hold their own PNG copies, rendered from these masters:

- `addon/icon.png` (128 x 128) and `addon/logo.png` (500 x 115): the add-on store.
- `custom_components/freedo/brand/`: Home Assistant brand images. `icon.png` is 256 x 256,
  `logo.png` is 256 px high, the `@2x` files are twice that size, and the `dark_` files are the
  dark icon and the reversed lockup.

Web files in `web/` for the dashboard:

| File | Size | Use |
| --- | --- | --- |
| `favicon.ico` | 16 and 32 | Legacy browsers. Serve it at `/favicon.ico`. |
| `icon-192.png`, `icon-512.png` | 192, 512 | Web app manifest icons. |
| `icon-maskable-512.png` | 512 | Manifest icon with purpose `maskable`. |
| `apple-touch-icon.png` | 180 | iOS home screen. |

The dashboard also serves `svg/favicon.svg` as its SVG icon (`<link rel="icon" type="image/svg+xml">`).
The maskable icon and the Apple icon are the primary icon on a full-bleed square, because
Android and iOS apply their own mask.

Upload `social-preview.png` by hand in the repository settings, under Social preview. GitHub has
no API for it.

## Mark

The master is 512 x 512 with a corner radius of 116.

| Part | Value |
| --- | --- |
| Ground | Gradient `#3D9EFF` to `#1B45C6` at 135 degrees |
| Dial | Radius 150, stroke 40, round caps, 80 degree gap at the bottom, centre (256, 256) |
| Letter `f` | Two strokes (stem and crossbar), width 36, round caps |
| Knob | On the dial at 40 degrees. Radius 30, Navy fill, white stroke 12 |
| Favicon | Dial radius 140, stroke 64. Knob radius 44, stroke 16. No `f`. |

Variants: dark has a Navy ground, an arc gradient `#7CC4FF` to `#2E7BFF` and a white `f`. Light has
a white ground with a 6 px `#C9DCF6` border, an Azure arc and a Navy `f`. Mono uses one ink and
the knob is a ring.

## Logo

- The wordmark is `freedo`, all lowercase, in Sora ExtraBold (800). Tracking is -3 to -4.5
  percent. The lockups use -4 percent.
- `do` takes the text tint `#1F6FF0` on light grounds and Sky on Navy.
- Clear space is half the icon height on every side. Minimum width is 96 px.

## Colours

| Token | Hex | Role |
| --- | --- | --- |
| Navy | `#0B1B3A` | Text, dark surfaces, knob. White on Navy 17:1. |
| Cobalt | `#1B45C6` | Buttons, links, active states. White on Cobalt 7.7:1. |
| Azure | `#2E7BFF` | Brand accent, dial, charts. As text on white: `#1F6FF0`. |
| Sky | `#7CC4FF` | Accent on dark, highlights. Navy on Sky 9.1:1. |
| Ice | `#E6F1FF` | Panels, chips. |
| Frost | `#F5F9FF` | Page ground. |
| Slate | `#4A5B78` | Secondary text. 6.9:1. |
| Border | `#D2E4FB`, `#C9DCF6` | Light borders. The second is the stronger one. |

Muted text is `#9FB3D6` on Navy and `#DCE7FF` on Cobalt. The icon gradient `#3D9EFF` to `#1B45C6` is for
the icon only.

### Dark theme

| Role | Hex |
| --- | --- |
| Ground | `#0B1B3A` |
| Surface | `#13284F` |
| Border | `#22365F` (stronger: `#34507F`, used on ground-coloured swatches) |
| Text | `#EAF2FF` |
| Text secondary | `#9FB3D6` |
| Link | `#7CC4FF`, hover `#FFFFFF` |

### Status

Each status is one tonal family. Every base has the same OKLCH lightness (0.80, warning 0.85, fault
0.74), so a grid of rooms in different states reads as one set and no state shouts over another.

- Base: the fill of a room tile and of a room screen, and dots, icons and charts.
- Top: the light end of the gradient of a room screen.
- Ink: text and icons on the base or the top. It is the dark step of the same hue, never navy, so a
  coloured surface stays monochrome. Contrast at least 5.3:1 on the base and 7:1 on the top.
- Text: labels on a neutral surface. Contrast at least 6.3:1 on white and on the tint.
- Tint: the badge ground. The badge is a pill (radius 999 px) with an 8 px dot.

Base, top and ink are the same in both themes. Text and tint change with the theme.

| Status | Meaning | Base / top / ink | Light text / tint | Dark text / tint |
| --- | --- | --- | --- | --- |
| Heating | Valve open, zone calling for heat | `#FEA668` `#FDC7A4` `#512500` | `#8C4400` `#FFEBDA` | `#FEA668` `#462C19` |
| Cooling | AC running towards the setpoint | `#58D1E5` `#A1E1EC` `#003C46` | `#00697A` `#DAF6FB` | `#58D1E5` `#11393F` |
| Idle | On, setpoint reached, no demand | `#6CD79D` `#A9E4C1` `#003F22` | `#006E40` `#DEF8E7` | `#6CD79D` `#1A3B29` |
| Away, Eco | Nobody home, reduced setpoint | `#C4ACFF` `#D7CBFF` `#362957` | `#614A96` `#F1ECFF` | `#C4ACFF` `#352E4A` |
| Off | Zone off or frost protection | `#B5BFCB` `#CFD5DC` `#293442` | `#4C5C71` `#ECF1F7` | `#B5BFCB` `#2C333D` |
| Warning | Low battery, weak signal, window open, rate limit | `#F8C655` `#FEE1A4` `#452E00` | `#795300` `#FBEFD6` | `#F8C655` `#3F3112` |
| Fault | Device error, failed command, auth failure | `#FB817A` `#F6AAA4` `#551F1D` | `#923A37` `#FFE8E4` | `#FB817A` `#492826` |
| Offline | Bridge or device unreachable | none | `#3A4660` `#E4E8F0` | `#C9D3E6` none |

Offline has no fill: a dashed border and a hollow dot. Its border is `#3A4660` on light and
`#8A9BBB` on dark. freedo has no cooling zone today. The Cooling colours stay in the tokens.

Schedule blocks use a warm ramp from `#FBEBDE` to `#EC9D53`, light in both themes, with navy labels.

### Rules

- The brand colours are cool blues only, for the ground, actions and navigation. Status colours are
  the only other hues.
- A coloured surface uses one status family only: its base, its top and its ink.
- Always pair a status colour with an icon or a label.
- The gradient is for the icon only.
- The wordmark is lowercase.

## Type

| Role | Font | Style |
| --- | --- | --- |
| Display | Sora 800 | 64 / 1.0, tracking -0.035em |
| Heading | Sora 600 | 32 / 1.2, tracking -0.015em |
| Body | IBM Plex Sans 400, 500 | 18 / 1.55 |
| Data and code | IBM Plex Mono 400, 500 | 18, weight 500, tracking 0.01em |

The identity also uses Sora 400 and 700. The fonts are licensed under the SIL Open Font License
1.1. They are on Google Fonts (fonts.google.com).

The dashboard must self-host the fonts. A local controller must not call a font CDN.
