# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Users

The owner of a tado V3+ heating plant who replaced the tado bridge and cloud with freedo. Usually a
Home Assistant user who self-hosts. The main scene is a phone: the dashboard opens through Home
Assistant ingress inside the companion app, to check the rooms or change a temperature. Desktop use
is secondary: installation, pairing, schedules and backup.

## Product Purpose

freedo is a local coordinator for tado V3+ 868 MHz valves and wall thermostats. A Go daemon talks to
the devices through an ESP32 with a CC1101 radio and serves a local HTTP API. The dashboard is the web
interface of that daemon: it shows the rooms, the hot water, the boiler and the devices, and it
controls them. Success: the owner reads the state of every room at a glance and changes it in a few
taps, with no cloud.

## Positioning

The only local, cloud-free control surface for tado V3+ hardware. It keeps the room model that tado
owners know (one tile per room, quick actions for the whole home) and adds what the plant shows only
locally: boiler telemetry over OpenTherm, radio health, device faults and the install itself.

## Operating Context

- Served by the daemon as static files. Under Home Assistant ingress the page needs no token; in a
  container it asks for the API token once.
- One build works at the root and under the ingress prefix (hash router, relative URLs).
- The page polls the views every 5 seconds. A daemon that waits after a backup import refuses
  commands; the page shows a countdown banner and goes inert.
- Home Assistant provides geofencing, history, notifications and multiple users. The dashboard does
  not.

## Capabilities and Constraints

- Pages: home (rooms grid, presence, active profile, boost all, resume all), room (dial, four end
  rules, off, boost, back to schedule, window, devices, weekly schedule), hot water, programs and
  profiles, boiler, rooms and devices (install, pairing, circuits), first setup, settings (backup,
  language, theme, flow limit).
- Setpoint limits: 5 to 25 C for a room, 30 to 65 C for hot water. Never weaken them.
- Zone states, by priority: offline, fault, window open, off, heating, away, idle. Other problems stay
  as alerts next to the state.
- Texts in English (reference), Italian, German and French. Light and dark themes, system by default.
- Fonts are self-hosted. A local controller never calls a font CDN.

## Brand Commitments

- The brand blues in `assets/brand/tokens.css` are fixed. The status colours are the only other hues,
  and a status colour always comes with an icon or a label.
- The status colours are a tonal set at equal lightness, with the dark step of each hue as its ink,
  because saturated fills with navy ink clashed.
- Type: Sora for display and numbers, IBM Plex Sans for body, IBM Plex Mono for data.
- freedo is not affiliated with tado. The dashboard can follow the interaction model of the tado app,
  but it never uses the tado name, logo or visual assets as branding.

## Evidence on Hand

- Brand assets: `assets/brand/` (logo SVGs, web icons, tokens).
- Synthetic installation for screenshots and checks: `dashboard/scripts/seed.mjs`.
- No testimonials, usage numbers or benchmarks exist. Do not invent them.

## Product Principles

1. State before control: the home page answers "is every room fine?" before it offers a command.
2. Safety is visible: faults, offline devices, open windows and dry-run mode are never hidden behind a tap.
3. Familiar to tado owners: one tile per room and whole-home quick actions, so a migration costs no relearning.
4. Local and honest: show what the plant reports, label estimates, never fake a value.

## Accessibility & Inclusion

- Status colour is never the only signal: every state has an icon and a label.
- Touch targets of at least 44 px. Full keyboard use on desktop.
- Four languages: labels must survive German text length.
