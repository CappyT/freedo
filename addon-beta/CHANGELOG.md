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

## 0.7.1-beta.1

- Breaking: the add-ons copy no state by themselves. freedo beta imports no copy of the state of the
  add-on freedo at its first start.
- Breaking: the option `import_stable_state` of freedo beta is gone. Home Assistant shows a warning
  for a saved value until you save the configuration once.
- Daemon: `GET /api/state/export` downloads the whole state as one SQLite file.
  `POST /api/state/import?confirm=true` checks such a file, keeps the replaced database in
  `backup/before-import-<time>/` (the three newest stay) and restarts the daemon by itself. A file
  of a newer schema is refused.
- Daemon: with Transmit on, a start less than 250 seconds after an import waits for the rest of
  that time.
- Dashboard: Settings has the section Backup, with Download backup and Restore backup. The backup
  holds the network key.
- In the web interface of the add-on, the backup is for Home Assistant administrators only and
  needs no token. The add-on asks Home Assistant about the user at every request and refuses when
  it gets no answer. Another client needs the API token.
- freedo beta has the permission `homeassistant_api`, to ask Home Assistant whether a user is an
  administrator. With it an add-on can call the whole API of Home Assistant. freedo uses the list
  of the users only.
- The folder `/share/freedo/stable` of older releases holds the network key and nothing reads it.
  The add-on does not delete it: delete it by hand. The log names it at each start.
- The backup did not run on a plant yet: this pre-release is the first run.

## 0.11.0-beta.1

- Daemon: the zone option `owner` (`device` or `coordinator`). With `coordinator`, freedo is the
  owner and the measuring point of the zone: it runs the valve controller with the gains of the
  zone on the outside temperature reading of the zone, answers the measure to its members and
  pushes it to them. The valves follow that demand and no offset on their sensor is needed.
  `PATCH /api/install/zones/{n}` sets it; the install view and `GET /api/zones` show it.
- Daemon: the option is refused on hot water, with `offset_mode setpoint` or with `auto_offset`
  on, and those two are refused on an owned zone.
- Daemon: the log names each zone-plane request that a member of an owned zone sends to the
  coordinator, with the method, the URI and the observe flag.
- Docs: the default weather curve of the flow limit is checked against eight monthly reports of
  the tado cloud of one plant and the value it serves today.
- The zone owner did not run on a plant yet: this pre-release is the first run. The daemon sends
  no observe notification: the first run tells whether the members ask for them.

## 0.11.0-beta.2

- Daemon: the controller of an owned zone resets when its temperature source changes, so the
  gap between the two sensors is no derivative. On the plant the switch from the valve sensor to
  the room thermometer gave a demand of 78 percent for 10 minutes.
- Daemon: the owner pushes its measure to the members every minute. On the plant a valve went
  back to its own sensor between pushes 3 to 5 minutes apart.
- Daemon: the log line of a member request to the owner shows the body of a zone measure.
- Home Assistant: the integration feeds the room thermometer reading to an owned zone, and sets
  no correction when it maps a sensor to one.
- Run on a real plant with 0.11.0-beta.1, one valve zone: the valve read the zone document
  within minutes, sends its own measure to the coordinator about once a minute with no observe,
  answers the pushed measure with 2.05, and took the pushed temperature into its controller.

## 0.11.0-beta.3

- Daemon: the zone measure of the owner carries the humidity tag `0135` of the measuring device,
  as every captured measure does. On the plant the wall thermostat asked the coordinator for the
  measure of the owned zone after its restart and stayed in the frost fallback on an answer
  without the tag, while the answer of a valve ended the fallback within seconds.
- Run on a real plant with 0.11.0-beta.2, one valve zone: with a push every minute the valve
  reports the pushed demand as its own and keeps the pushed temperature.
