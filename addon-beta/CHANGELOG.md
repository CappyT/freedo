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

## 0.7.0-beta.2

- Dashboard: Add device ends with one state that names the paired device and its room. While the
  pairing runs, the page shows only the progress. A device that is already paired shows as paired
  in the list of heard devices and cannot be selected.
- Run on a real plant with 0.7.0-beta.1: the move from the add-on freedo to freedo beta, the
  room options and a room created and deleted from the dashboard, a window that the wall
  thermostat detected and its reset, and a valve paired from the dashboard.

## 0.7.0-beta.3

- Dashboard: the room settings have a Restart action for a valve and for the wall thermostat, with
  a confirmation. The restart command is open to Home Assistant ingress: every Home Assistant
  user who can open the page can restart a device.
- Dashboard: Add device has no default room. The page declares the device, adds it to the room and
  then pairs it, and it sends no pair command when the room is refused. The end state shows the
  room that the daemon holds, and it offers the repair when the chosen room is missing.
- Dashboard: the page reads the join of a device from the frames that verified under the network
  key, or from the learned serial.
- Run on a real plant with 0.7.0-beta.2: a valve paired from the dashboard, and the warning for a
  valve that is not mounted, with no fault alert.
