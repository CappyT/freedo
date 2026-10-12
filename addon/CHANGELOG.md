# Changelog

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
