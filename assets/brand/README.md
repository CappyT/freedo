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

Each status has three values. Base is for dots, icons and charts. Text is for labels. Tint is the
badge ground. The badge is a pill (radius 999 px) with an 8 px dot.

| Status | Meaning | Light base / text / tint | Dark base / text / tint |
| --- | --- | --- | --- |
| Heating | Valve open, zone calling for heat | `#F2671F` `#B4430B` `#FFEDE3` | `#FF7A33` `#FFA26B` `#413038` |
| Cooling | AC running towards the setpoint | `#14B8D4` `#0B7487` `#E0F7FB` | `#2BD2EE` `#6FE3F5` `#11405E` |
| Idle | On, setpoint reached, no demand | `#22A06B` `#146C48` `#E3F6EC` | `#3DD68C` `#7BE8B0` `#15404A` |
| Away, Eco | Nobody home, reduced setpoint | `#7C5CFA` `#5B3FD6` `#EFEBFF` | `#9B85FF` `#BDB0FF` `#2B3265` |
| Off | Zone off or frost protection | `#A7B3C8` `#4A5B78` `#EEF2F8` | `#5A6B8A` `#B4C2DA` `#1C2C4A` |
| Warning | Low battery, weak signal, window open, rate limit | `#F5A623` `#8A5A00` `#FFF4DB` | `#FFB938` `#FFCB66` `#3D3527` |
| Fault | Device error, failed command, auth failure | `#D9363E` `#B4232A` `#FDECEC` | `#FF5A61` `#FF8A8F` `#412943` |
| Offline | Bridge or device unreachable | `#3A4660` `#3A4660` `#E4E8F0` | `#C9D3E6` `#C9D3E6` none |

Offline has a dashed border and a hollow dot. Its border is `#3A4660` on light and `#8A9BBB` on
dark. freedo has no cooling zone today. The Cooling colours stay in the tokens.

The dark values use the same hues as light, raised in lightness. Each dark tint is its hue mixed
about 20 percent into Navy. Each dark text value has a contrast of at least 5.7:1 on Surface
and on its own tint.

### Rules

- The brand colours are cool blues only. Status colours are the only other hues. Heating is the
  only warm hue, so it stands out against the blue brand.
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
