# Changelog

This add-on follows every release of freedo. A stable release has its notes in the
[changelog of the stable add-on](https://github.com/CappyT/freedo/blob/main/addon/CHANGELOG.md). A
pre-release has its notes here.

## 0.7.0-beta.1

- Daemon: a user reset ends an open window of any source, also one that a valve detected. A window
  sensor that closes ends only the hold that Home Assistant set.
- Dashboard: the room settings hold the room options: open window detection and its time, the away
  temperature, the end rule of a change made on a device, early start, the offset mode, the offset
  and the valve calibration. The page names the options that cost a motor stroke.
- Dashboard: the Boiler page and a banner show the fault codes of the boiler, as numbers.
- Dashboard and Home Assistant use the same words for the end rules, the alerts and the heating
  demand. No entity id and no state value changed.
- Home Assistant: binary sensor Boiler fault, event entity Alert, service `freedo.pair` and switch
  Pairing mode. The integration reloads by itself after a change of the installation.
- Blueprints: notifications for alerts, an open window and a warmer outside temperature; a setback
  in steps with the distance from home; auxiliary heat for a room from another appliance; a monthly
  valve exercise.
- Not run on a real plant yet: this pre-release is the first run.
