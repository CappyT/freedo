---
name: freedo dashboard
description: Local control surface for a tado V3+ heating plant. A mosaic of state-coloured room tiles on a cool blue ground.
colors:
  navy: "#0B1B3A"
  cobalt: "#1B45C6"
  azure: "#2E7BFF"
  sky: "#7CC4FF"
  ice: "#E6F1FF"
  frost: "#F5F9FF"
  slate: "#4A5B78"
  white: "#FFFFFF"
  border-light: "#D2E4FB"
  border-light-strong: "#C9DCF6"
  navy-surface: "#13284F"
  navy-border: "#22365F"
  navy-text: "#EAF2FF"
  navy-text-muted: "#9FB3D6"
  status-heating: "#FEA668"
  status-heating-ink: "#512500"
  status-idle: "#6CD79D"
  status-idle-ink: "#003F22"
  status-warning: "#F8C655"
  status-warning-ink: "#452E00"
  status-off: "#B5BFCB"
  status-off-ink: "#293442"
  status-away: "#C4ACFF"
  status-away-ink: "#362957"
  status-fault: "#FB817A"
  status-fault-ink: "#551F1D"
  status-offline-border: "#3A4660"
  schedule-low: "#FBEBDE"
  schedule-high: "#EC9D53"
typography:
  display:
    fontFamily: "Sora, sans-serif"
    fontSize: "2.875rem"
    fontWeight: 700
    lineHeight: 1
    letterSpacing: "-0.04em"
  headline:
    fontFamily: "Sora, sans-serif"
    fontSize: "2rem"
    fontWeight: 700
    lineHeight: 1.1
    letterSpacing: "-0.025em"
  title:
    fontFamily: "Sora, sans-serif"
    fontSize: "1.375rem"
    fontWeight: 600
    lineHeight: 1.2
    letterSpacing: "-0.01em"
  section:
    fontFamily: "Sora, sans-serif"
    fontSize: "1.09375rem"
    fontWeight: 600
    lineHeight: 1.2
    letterSpacing: "-0.015em"
  body:
    fontFamily: "IBM Plex Sans, Helvetica Neue, sans-serif"
    fontSize: "0.9375rem"
    fontWeight: 400
    lineHeight: 1.5
  label:
    fontFamily: "IBM Plex Sans, Helvetica Neue, sans-serif"
    fontSize: "0.8125rem"
    fontWeight: 600
    lineHeight: 1.25
  data:
    fontFamily: "IBM Plex Mono, monospace"
    fontSize: "0.9375rem"
    fontWeight: 500
    letterSpacing: "0.01em"
rounded:
  xs: "8px"
  md: "14px"
  lg: "20px"
  sheet: "24px"
  pill: "999px"
spacing:
  xs: "4px"
  sm: "8px"
  md: "12px"
  tile: "14px"
  lg: "16px"
  xl: "20px"
  2xl: "24px"
components:
  button-primary:
    backgroundColor: "{colors.cobalt}"
    textColor: "{colors.white}"
    rounded: "{rounded.md}"
    padding: "8px 18px"
    height: "46px"
  button-secondary:
    backgroundColor: "{colors.white}"
    textColor: "{colors.navy}"
    rounded: "{rounded.md}"
    padding: "8px 18px"
    height: "46px"
  input:
    backgroundColor: "{colors.frost}"
    textColor: "{colors.navy}"
    rounded: "{rounded.md}"
    padding: "6px 12px"
    height: "46px"
  tile-heating:
    backgroundColor: "{colors.status-heating}"
    textColor: "{colors.status-heating-ink}"
    rounded: "{rounded.lg}"
    padding: "{spacing.tile}"
    height: "152px"
  tile-idle:
    backgroundColor: "{colors.status-idle}"
    textColor: "{colors.status-idle-ink}"
    rounded: "{rounded.lg}"
    padding: "{spacing.tile}"
    height: "152px"
  tile-neutral:
    backgroundColor: "{colors.white}"
    textColor: "{colors.navy}"
    rounded: "{rounded.lg}"
    padding: "{spacing.tile}"
    height: "152px"
  list-row:
    backgroundColor: "{colors.white}"
    textColor: "{colors.navy}"
    typography: "{typography.body}"
    padding: "10px 14px"
    height: "52px"
  badge:
    rounded: "{rounded.pill}"
    padding: "6px 12px"
    typography: "{typography.label}"
  nav-item-active:
    backgroundColor: "{colors.ice}"
    textColor: "{colors.navy}"
    rounded: "{rounded.md}"
    height: "46px"
---

# Design System: freedo dashboard

## Overview

**Creative North Star: "The Room Mosaic"**

The home page is a grid of room tiles. Each tile is filled edge to edge with the colour of its zone state, so the whole house reads in one glance. A tile opens into its room, and the room screen keeps the same colour as a full-bleed hero around the dial. The interaction model follows the tado app, because its owners must migrate without relearning (see [PRODUCT.md](PRODUCT.md)). The branding stays freedo's own.

Two colour systems share the screen and never mix. The brand blues of [tokens.css](assets/brand/tokens.css) own the ground, the chrome, the actions and the navigation. The tonal status families own every coloured surface. All status bases sit at the same OKLCH lightness, so no state shouts over another, and the ink on a fill is the dark step of the same hue. The result is calm and dense: white grouped lists, soft rounded tiles, big Sora numbers, small Plex labels.

The system rejects the admin-panel default: white cards with small status badges and a dial repeated in every card.

**Key Characteristics:**
- State-coloured room tiles in two columns on a phone, auto-fill columns on a wide screen.
- Monochrome coloured surfaces: one status family per surface (base, top, ink).
- Cool blue chrome; status hues appear nowhere else.
- Big Sora integers with a small degree and decimal beside them.
- Grouped lists with hairline dividers, like the settings screen of a phone.
- Phone-first: bottom navigation bar, sheet over the hero, 44 px touch targets.

## Colors

Cool blue brand chrome around seven tonal status families of equal lightness. Every value lives in [tokens.css](assets/brand/tokens.css); components use its role tokens (`--ground`, `--surface`, `--text`, `--action`, `--status-*`) only.

### Primary
- **Cobalt Action** (cobalt): primary buttons, checked radios and checkboxes, the selected profile mark. It is the one colour that says "act".
- **Azure Accent / Sky** (azure in light, sky in dark): focus rings, hover borders, the accent of the theme. Sky is also the selection highlight.

### Secondary
- **Ice Panel** (ice in light, navy-border in dark): the active navigation item, icon discs, segmented control grounds, row hover.

### Tertiary: status families
- **Heating Apricot** (status-heating): valve open, zone calling for heat. Also the boost quick action icon.
- **Idle Mint** (status-idle): setpoint reached, no demand. Also "Radio attiva".
- **Warning Honey** (status-warning): window open, low battery, weak signal, dry run.
- **Off Pewter** (status-off): zone off or frost protection.
- **Away Lilac** (status-away): away and eco profiles.
- **Fault Coral** (status-fault): device error, failed command, danger buttons.
- **Offline** (status-offline-border): no fill. A dashed outline and a hollow dot.

Each family also has a top step (the light end of a hero gradient), a text step (a label on a neutral surface) and a tint (a badge ground). Those steps change with the theme; [tokens.css](assets/brand/tokens.css) holds them.

### Neutral
- **Frost Ground / Navy Ground** (frost, navy): the page ground in light and dark.
- **White Surface / Navy Surface** (white, navy-surface): lists, panels, neutral tiles, the navigation rail.
- **Navy Ink / Pale Navy Text** (navy, navy-text): body text.
- **Slate / Navy Text Muted** (slate, navy-text-muted): secondary text, field labels, inactive navigation.
- **Hairline** (border-light, navy-border): list dividers and card borders.
- **Schedule Ramp** (schedule-low to schedule-high): schedule blocks, mixed in OKLab by setpoint. Light in both themes, with navy labels.

### Named Rules

**The Two Systems Rule.** Brand blues are for ground, chrome, actions and navigation. Status hues are for state only. A blue never encodes a state, and a status hue never decorates chrome.

**The Monochrome Surface Rule.** A coloured surface uses one status family: its base, its top and its ink. Text and icons on a fill take the ink, never navy or white. This keeps a tile or a hero readable (ink on base is at least 5.3:1) and calm.

**The Paired Signal Rule.** A status colour always comes with an icon or a word: the tile line, the hero state pill, the badge label, the radio chip word. Colour alone never carries state, because PRODUCT.md requires it for accessibility.

**The Pale Dot Rule.** On a white or navy surface, a status dot takes the text step of its family, not the base. The base is too pale on white to read as a signal.

**The State Not Temperature Rule.** A tile colour encodes the zone state, never the temperature. Only the schedule ramp maps temperature to colour.

## Typography

**Display Font:** Sora (with sans-serif)
**Body Font:** IBM Plex Sans (with Helvetica Neue, sans-serif)
**Label/Mono Font:** IBM Plex Mono (with monospace)

**Character:** Sora is round and confident for numbers and titles. Plex Sans is plain and compact for labels and body copy. Plex Mono marks values that come from the plant. All three are self-hosted, because a local controller never calls a font CDN.

### Hierarchy
- **Display** (Sora 700, 2.875rem, line-height 1, -0.04em): the integer of a tile temperature. The degree sign and the decimal sit beside it in Sora 600 at about 1.2rem, stacked: degree at the top, decimal at the bottom. The dial centre uses the same face at min(3rem, 17cqi) with a superscript degree.
- **Headline** (Sora 700, 2rem, 1.1, -0.025em): the page title, such as "Casa".
- **Title** (Sora 600, 1.375rem, -0.01em): the room name in the hero bar. The hero target value uses Sora 700 at 1.875rem.
- **Section** (Sora 600, 1.09375rem): a section heading above a grouped list. Tile names use Sora 600 at 1rem.
- **Body** (Plex Sans 400, 0.9375rem, 1.5): running text. A dashboard reads closer than the brand pages, so body is 15 px, not the brand 18 px.
- **Label** (Plex Sans 600, 0.8125rem, 1.25): tile lines, hero commands, state pills. Small text is 14 px (`--text-small-size`); tile chips and phone navigation labels are 12 px.
- **Data** (Plex Mono 500, +0.01em): numbers, units and identifiers inside text, key and value facts, the dial end labels, the schedule hour axis.

### Named Rules

**The Numbers Speak Sora Rule.** A big temperature is a Sora integer with a small degree and decimal beside it. A number inside a sentence or a list takes Plex Mono. Words stay in Plex Sans.

**The No Shouting Rule.** No uppercase labels and no wide tracking. Hierarchy comes from size and weight only.

## Layout

The layout is phone-first, because the main scene is the Home Assistant companion app.

- **Phone (below 900 px):** one column, 16 px page padding. A sticky bottom bar holds four items (Casa, Programmi, Dispositivi, Impostazioni), each an icon in a pill over a 12 px label. A label may wrap to two lines; the bar grows instead of clipping. The logo is hidden.
- **Wide (900 px and up):** a 15rem rail on the left with the logo, the items and the radio chip at the bottom. Content padding is 2rem top and clamp(1rem, 3vw, 2.5rem) at the sides. Pages cap at 70rem.
- **Tiles:** auto-fill columns of at least 15rem with a 16 px gap. Below 560 px, always two columns with a 12 px gap. Home and plant tiles use a 12rem minimum so four fit one row.
- **Room screen:** on a phone the hero runs edge to edge and the sections slide over its bottom edge as a sheet with a 24 px top radius. From 900 px the hero becomes a sticky card (20rem to 26rem) beside the sections.
- **Rhythm:** 4, 8, 12, 16, 20 and 24 px steps. Pages stack sections at 20 px; a section heading sits 8 px above its list.
- **Commands:** two columns of buttons in a card, one column below 420 px; an odd last button takes the full row. A row of buttons fills the width below 900 px.

## Elevation & Depth

The system is flat and tonal at rest. Depth comes from the ground and surface steps (frost under white, navy under navy-surface) and from hairline borders. Shadows are soft, navy-tinted and wide; they mark a lifted or floating element, never a structure.

### Shadow Vocabulary
- **Tile lift** (`box-shadow: 0 12px 24px -14px color-mix(in srgb, var(--navy) 60%, transparent)`): a tile on hover, together with a 2 px lift.
- **Quick action** (`box-shadow: 0 1px 2px color-mix(in srgb, var(--navy) 6%, transparent), 0 8px 18px -10px color-mix(in srgb, var(--navy) 22%, transparent)`): the whole-home quick actions in light mode. In dark mode a border replaces it.
- **Floating card** (`box-shadow: 0 14px 30px -16px color-mix(in srgb, var(--navy) 70%, transparent)`): the pending-change card on a hero.
- **Menu** (`box-shadow: 0 0.75rem 2rem color-mix(in srgb, var(--navy) 22%, transparent)`): popover menus.
- **Nudge** (`box-shadow: 0 6px 14px -8px color-mix(in srgb, var(--fill-ink) 60%, transparent)`): the white plus and minus buttons on a hero, tinted with the ink of the state.

### Named Rules

**The Flat At Rest Rule.** Tiles, lists and panels carry no shadow at rest. A shadow appears on hover, on a popover, or on a card that floats over a coloured hero.

## Shapes

Soft, generous corners and circles. Radii come from [tokens.css](assets/brand/tokens.css).

- **Tiles, lists, panels, menus** (20 px, `--radius-lg`).
- **Buttons, inputs, navigation items, notices** (14 px, `--radius-md`).
- **Schedule bars, focus outline** (8 px, `--radius-xs`).
- **Sheet** (24 px top corners): the sections over a hero on a phone.
- **Pills** (999 px): badges, chips, the hero state, the radio chip.
- **Circles:** icon discs (36 px), round hero buttons (44 to 54 px), dots (8 px), the dial knob.
- **Dashed outline:** the one sign of offline, on tiles (1.5 px), badges and dots (1 px). Dashes mean "unreachable" and nothing else.

## Components

### Buttons
Solid, compact, 46 px high, so a field and its buttons line up.
- **Shape:** gently rounded (14 px).
- **Primary:** cobalt fill, white text, Plex Sans 600 at 15 px. Hover mixes 20% of the text colour into cobalt: darker on light, lighter on dark.
- **Secondary:** surface fill, strong border, text colour. Hover turns the border to the accent.
- **Danger:** surface fill, fault border, fault text.
- **Icon:** 44 px square.
- **Disabled:** 55% opacity.
- **Focus:** a 3 px accent outline at 2 px offset, everywhere.

### Chips
- **Status badge:** a pill with an 8 px dot and a label, on the tint of its family with the text step as ink. Offline is a dashed hollow badge.
- **Tile chip:** a 24 px pill of 12% ink on the fill, with an icon and a short value (humidity, mode, timer).
- **Radio chip:** a 36 px surface pill with a dot and one word for the radio state. It sits beside the page title on a phone and at the foot of the rail on a wide screen.
- **Segmented control:** radios as a row of buttons on an ice ground; the checked option is a surface pill with a small shadow.

### Cards / Containers
- **Grouped list:** white surface, 1 px hairline border, 20 px radius, rows of at least 52 px, a hairline between rows inset 14 px from the left. A row leads with a 36 px icon disc and ends with a value or a chevron.
- **Panel:** surface, hairline border, 20 px radius, 20 px padding.
- **Quick action:** 62 px high, 18 px radius, a 32 px icon disc, a label and a sub-line. Boost takes the heating tint; resume takes the ice disc.
- **Notice / alert strip:** the tint and text of its family, an icon, 14 px radius.

### Inputs / Fields
- **Style:** the ground colour inside, a strong border, 14 px radius, 46 px high, 16 px text so a phone does not zoom on focus. The label sits above in muted Plex Sans 500.
- **Select:** a chevron of the icon set in the muted text colour.
- **Radio and checkbox:** an outline that fills with cobalt, the same in both themes.
- **Error:** a fault border.

### Navigation
- **Wide:** a rail of 46 px items, muted text, a 14 px radius. Hover shows the panel colour. The active item takes the ice ground, semibold text and a link-coloured icon.
- **Phone:** a bottom bar of four equal items. The active icon sits in an ice pill (58 x 30 px).

### Room Tile (signature)
A button of at least 152 px filled with its state base, 20 px radius, 14 px padding, ink text. Top left: tile chips. Top right: a large outline icon of the state at 30% opacity, a corner disc on neutral tiles, or a white disc with a warning icon when the zone has an alert. Bottom: the Sora temperature, the room name in Sora 600, and a line with an icon and the state word. Variants: neutral (surface fill, hairline border, blue icon disc) for presence and profile; offline (no fill, 1.5 px dashed border, text colour); a word in place of the number when no temperature exists.

### Room Hero (signature)
The tile grown into a screen. A vertical gradient from the top step to the base at 70%, all content in the ink. A bar with two 44 px round buttons and the title; a state pill; facts in Plex Mono; the dial; the target with white round nudge buttons; round commands with labels; a white floating card for the pending change. The tile expands into the hero through a view transition of 280 ms with an exponential ease-out (`cubic-bezier(0.16, 1, 0.3, 1)`).

### Dial
A 270 degree arc from 5 to 25 C, drawn in the ink of the state: a 14-unit track at 16% ink, the progress in full ink, a notch at the current temperature, and a knob in ink with a white stroke at the target. The current temperature sits in the centre in Sora with a caption. The end values sit under the arc in Plex Mono.

### Schedule Week
One bar per day, 32 px high, 8 px radius. Blocks are separated by a 2 px white line in both themes and coloured on the schedule ramp by setpoint; off blocks take the off tint. Labels are navy at 13 px and hide when a block is too narrow. The time of now is the only dark line on the bar: a 2 px navy line across it, with a 10 px navy dot on the top edge in a 2 px white ring, so the dot also reads on the navy card of the dark theme.

### Curve Preview
A weather curve drawn as one 2 px line in the text colour over hairline grid lines every 10 C: the outdoor temperature from -15 to 25 C across, the flow limit from 30 C up. The outdoor temperature of now is an accent dot in a 2 px surface ring, and the caption under the plot names its value. A pointer over the plot shows a crosshair and the value at that degree. A row of seven values in Plex Mono under the plot is the table twin.

### Comfort Map
Temperature from 14 to 30 C across, humidity from 10 to 90 % up. The comfort zone (20 to 24 C, 30 to 60 %) is the idle tint with an idle-text hairline and an 8 px radius. The 70 % mould line is a fault-text hairline. The room is a text-colour dot in a 2 px surface ring. The row above pairs a status dot with the verdict word and a hint.

### Guide
A `<details>` block inside a panel: the panel tint at half strength, a hairline border, a 14 px radius. The summary is a link-coloured semibold line; the body is a numbered list at 13.5 px. A guide explains where a value comes from; it never holds a control.

## Do's and Don'ts

### Do:
- **Do** fill a tile or a hero with one status family and set all text and icons in its ink.
- **Do** pair every status colour with an icon or a word.
- **Do** use the text step of a status family for dots and labels on a white or navy surface.
- **Do** keep touch targets at 44 px or more and buttons, inputs and segmented controls at 46 px.
- **Do** set big temperatures as a Sora integer with a small degree and decimal beside it, and inline numbers in Plex Mono.
- **Do** group settings and devices into white lists with hairline dividers and a heading outside the list.
- **Do** use the 3 px accent focus outline on every interactive element.
- **Do** let German and Italian labels wrap instead of truncating them.
- **Do** honour reduced motion: transitions and the view transition collapse to near zero.

### Don't:
- **Don't** use a brand blue to show a state or a status hue to decorate chrome.
- **Don't** put navy or white text on a status fill.
- **Don't** colour a tile by temperature.
- **Don't** give offline a fill; it is a dashed hollow outline.
- **Don't** repeat the dial in a tile or a card; it lives on the room screen only.
- **Don't** add uppercase labels, eyebrows or wide tracking.
- **Don't** use the icon gradient anywhere but the app icon.
- **Don't** use the tado name, logo or visual assets.
- **Don't** load fonts from a CDN.
