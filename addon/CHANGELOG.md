# Changelog

## 0.5.0

- Profiles: a profile holds a home schedule and an away schedule for each room and for hot water.
  At the first start the daemon puts the current schedules into the profile `Default` without a
  write to the devices. Choose, create, rename and delete profiles from the home menu of the
  dashboard and from the Programs page, which edits any profile before you activate it. Home
  Assistant gets a Profile select and a blueprint that switches the profile by season.
- Away schedules: while the home is away, a room with an away schedule follows it, and a room
  without one keeps its away temperature. Hot water without an away schedule stays off.
- Rooms in Home Assistant: Configure has two parts, Environment and Rooms. Rooms maps each room to
  a Home Assistant area and to a room sensor, and the valves of a room follow its area. One repair
  issue lists the rooms without an area. The Temperature source select offers the temperature
  sensors of the area, then the devices of the room. One Room temperature correction select
  replaces the offset mode select and the automatic correction switch, which move to the
  diagnostic entities.
- Identify on the wall thermostat: the thermostat shows "Hi!" on its display. The dashboard and
  Home Assistant offer it for a thermostat in a room.
- Dashboard: a renamed device shows its new name at once, the away schedule stays open across a
  data poll, controls line up on desktop and phone, and the texts use one word for each thing.
  Hot water is a card on the home page.
- Home Assistant: shorter entity names that fit a device page, and every text says room and
  freedo.
- The database moves to schema 8. Version 0.4.0 cannot read it: keep the backup that Home
  Assistant offers before the update, and restore it to go back.

## 0.4.0

- Option `channel`: `beta` runs the newest pre-release of the daemon from the GitHub releases of
  the repository, with the same data folder. The add-on copies the database before the first start
  of a beta, and restores that copy when you return to `stable` after a beta changed the schema.
  Every release attaches the daemon binaries and `SHA256SUMS`.
- Automatic correction of the valve offset from an external room sensor, at an interval you set,
  per heating zone. It reads the measuring device, never writes during a heat-up, and stops with a
  reason when a write does not move the difference. Off by default.
- Gas meter: record a meter reading and the daemon learns the correction factor of its gas
  estimate. Home Assistant gets a Gas meter sensor for the Energy panel and the services
  `freedo.calibrate_gas_meter` and `freedo.undo_gas_meter_reading`.
- Home Assistant: a Temperature source select per room (a device of the room, or the external
  sensor), switches for the automatic correction, the device firmware as its software version, and
  a translated name for the hot water zone.
- The daemon keeps the firmware version of each device across restarts, and reads it once from a
  device that has never reported it.
- A device declared without a serial takes it from its first request.
- Dashboard: rename a device, a fixed translated name for hot water, and a layout pass for phones
  and the dark theme. Inside Home Assistant the rename changes only the label.
- The unhandled event counter no longer counts the session id changes of zone requests between
  the devices of a zone.

## 0.3.2

- Web dashboard, served by the daemon and shown in the Home Assistant sidebar. It shows the
  home, the zones with their schedules, hot water, the boiler and the devices, and sends the
  everyday commands. Inside Home Assistant it needs no token for those commands only; every
  other route still needs the token.
- The hot water and room cards always offer the temperature control. On a zone with no target
  the control starts at the lowest temperature, which is not sent until you choose one.
- `GET /api/zones` reports `target.hasProgram`. `GET /api/boiler` reports `DHWSetpointC`, the
  hot water setpoint that the thermostat holds.

## 0.3.1

- A serial reply that the USB link cuts no longer stops the daemon. It discards the reply and
  goes on, counts it in `serialErrors` of `GET /api/health`, and stops only when the link
  answers nothing for 30 s. The radio frame inside a lost reply is lost.
- The daemon talks 230400 baud to the bridge and falls back to 921600 for firmware before
  v2.10. Flash the firmware of this release to stop the lost replies on a Raspberry Pi.
- A restart continues the Router Advertisement interval and sends no burst at every start.
- Each release attaches the bridge firmware image and `SHA256SUMS`.
- A Home Assistant temperature sensor can be mapped to a heating zone, with a select for the
  temperature offset mode of the zone.

## 0.3.0

- Default weather curve for the flow limit, off by default.
- Switches for the flow limit default curve and the flow limit adaptation.
- Italian, German and French translations.

## 0.2.0

- First release of the add-on.
