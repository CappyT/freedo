---
version: 1
slug: "dashboard-src-routes-page-svelte"
primary_target: "dashboard/src/routes/+page.svelte"
related_targets: ["dashboard/src/routes/+layout.svelte"]
---

# Dashboard surface brief

## Scope and mode

The freedo web dashboard: home, room, programs, boiler, rooms and devices, settings. Mode: Operate.
Phone-first (Home Assistant companion app through ingress), adapted to desktop.

## Audience, job, constraints

- The owner of a tado V3+ plant run by freedo. Job: see at a glance which rooms heat, rest or need
  attention, then change one room in a few taps.
- Brand blues of `assets/brand/tokens.css` stay. Status colours move to a tonal set (user decision after the first mock clashed). Status colour always with an icon or a label.
- The room screen keeps the circular dial (5 to 25 C, current temperature in the middle, target as knob).
- Tile colour encodes the zone state, never the temperature.
- Copy in the four languages; Italian and German lengths must fit.

## Direction contract

THESIS: The home is a mosaic of room tiles, each filled with its state colour, so the whole house reads in one glance. A tile opens into its room, which keeps the same colour full-bleed around the dial. It refuses the admin-panel default: white cards with small badges and a dial repeated in every card.

OWN-WORLD: Brand blues (navy, cobalt, sky, ice, frost) for ground, chrome, actions and navigation only. Status colours are one tonal family each, all at the same lightness: heating #FEA668, idle #6CD79D, warning #F8C655, off #B5BFCB, away #C4ACFF, fault #FB817A; the ink on a fill is the dark step of the same hue, so a tile or a room screen is monochrome. Offline is a dashed hollow tile. Presence and profile tiles are neutral surfaces with a blue icon disk. Setpoint blocks use a warm ramp from #FBEBDE to #EC9D53. Sora for big numbers (large integer, small superscript degree and decimal) and titles, Plex Sans body, Plex Mono data. White grouped lists, 20 px tile radius, 24 px sheet radius.

STORY: The owner opens freedo in the Home Assistant app, sees which rooms heat, rest or need attention, taps one, turns the dial and applies the change with an end rule.

FIRST VIEWPORT: Phone. Title Casa with a radio status chip on the right; two quick actions (Boost a tutte, Riprendi tutte); an alert strip when a problem exists; two columns of room tiles fill the rest. The primary action is a tap on a tile.

FORM: the tado app model, pinned by the user in their own words ("fai un nuovo design della dashboard basandoti sul design di tado"); position 1. Signature move: the state-coloured tile that continues as the room hero, with a view transition that expands the tile into the hero (exponential ease-out, about 280 ms). The user pinned the direction and approved the proposal before any roll, so the roll was skipped; a roll afterwards would decide nothing. The user accepted this pinned, no-roll FORM in their own words: "ok, mi va bene così". seed: none (pinned by the user)

FINISH: unreviewed and undocumented is unfinished; this build ends with the finish review, the verdict, DESIGN.md, and every shipping raster carrying its provenance

## Decisions confirmed by the user

- Boiler leaves the navigation and is reached from its home tile.
- The navigation item is "Dispositivi".
- New copy ships in it, en, de and fr.
- Rename, move, restart and removal of a device stay in the room settings. The device row on Rooms and devices links there.
