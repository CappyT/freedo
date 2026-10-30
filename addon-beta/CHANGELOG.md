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

## 0.11.0-beta.4

- Daemon: a zone the coordinator owns elects no demand reporter. The `015e` record of its
  valves carries the plain role and the reload queues the `d/config` write, so a valve stops
  running its own controller and follows the pushed demand. On the plant an elected valve kept
  its own controller and published 100 % on a satisfied room.
- Daemon: the wall thermostat reads the owner's `z/p` with no query every two minutes; the
  daemon answers it with the measure of the owned zone it serves. An empty answer left it on the
  last demand it knew for 25 minutes.

## 0.11.0-beta.5

- Daemon: the coordinator serves its `z/p` as a CoAP observe. The wall thermostat takes the
  demand of a zone from the observe notifications of the owner alone, and parses no answer
  without the Observe option. The answer to its registration carries Observe, and a NON
  notification follows each change of the measure and at least every 10 minutes. A change of the
  zone owner sends the thermostat its re-read trigger, so it reads the new zone document within
  seconds. `GET /api/zones` lists the observers of an owned zone.

## 0.11.0-beta.6

- Daemon: the checks of the zone owner in `PATCH /api/install/zones/{n}` read the values the request
  leaves, so one request moves a zone between the correction methods.
- Dashboard: the room settings hold one Temperature correction select: none, continuous shift, valve
  calibration, or the coordinator measures the room. The offset mode select stays for config and
  dbg2. The calibration hours show only in valve calibration.
- Home Assistant: the select Room temperature correction offers Coordinator measures the room.
- Run on a real plant with 0.11.0-beta.5: after a reboot of the wall thermostat, it registered its
  observe on the coordinator-owned room with a zero-length Observe option, took the notifications,
  and the circuit followed the coordinator demand within a minute. The thermostat did not put any
  room in fallback after that reboot.

## 0.11.0-beta.7

- Daemon: the observe registrations of an owned zone survive a restart of the daemon. The wall
  thermostat keeps its registration across our restart and ignores a notification under an
  unknown token or a lower sequence, so the daemon stores token and sequence and notifies at its
  first step after the start. Before, the thermostat held the last demand it got for up to 30
  minutes after an add-on update.
- Run on a real plant with 0.11.0-beta.6: the room settings and the Home Assistant select move a
  room between the correction methods in one request.

## 0.11.0-beta.8

- Daemon: the room temperature readings survive a restart of the daemon while they are fresh, so
  the controller of an owned zone runs at its first step after the start. On the plant with
  0.11.0-beta.7 the restored observe registration waited 10 minutes for the first valve report,
  because Home Assistant sends a reading again only on a change of the sensor or every 15 minutes.
- Run on a real plant with 0.11.0-beta.7: after a restart of the daemon the wall thermostat kept
  its observe registration, and the first notification under the stored token and the next
  sequence was taken.

## 0.11.0-beta.9

- Daemon: the flow limit moves when the curve value before the rounding is `flow_hysteresis_c`
  from the served one. On the plant the default curve moved the limit between 48 and 49 C every 30
  minutes at 13 C outdoor, and each change made the wall thermostat reload its configuration and
  hold its rooms in frost fallback for up to 4 minutes. A cut of the flow adaptation that ends is
  still served at once.
- Daemon: each room that freedo owns names its own measure path, `/z<N>`, in its zone document.
  The wall thermostat keeps one observe subscription per peer and path, so with one shared `z/p`
  the registrations of several owned rooms replaced each other and the rooms fell back in turn.
- Dashboard: the hint of the temperature correction "freedo owns the room" no longer says that the
  mode is not verified on air.
- Run on a real plant with 0.11.0-beta.8: Living room (two valves) and Hall (wall thermostat and a
  valve) on the coordinator as owner, on the Home Assistant thermometers.

## 0.11.0-beta.10

- Daemon: the ETag of the wall thermostat's `d/config` hashes the measure path of each room it
  serves. The thermostat reloads its observe registrations only after a new `d/config`: with
  0.11.0-beta.9 it read the zone documents with `/z<N>` at its hourly check and stayed on the
  old `z/p`. The first start of this version moves the ETag once, and the thermostat registers on
  the new paths within seconds.
- Run on a real plant with 0.11.0-beta.9: the valves read `/z<N>` and take the measure from
  `GET z<N>?lid=N`. The flow limit did not move on the default curve for 45 minutes at a steady
  outdoor temperature.

## 0.11.0-beta.11

- Home Assistant: a repair issue Room thermometers ignored lists the rooms whose temperature
  sensor freedo ignores, because their Room temperature correction is None. On a real plant two
  rooms had a sensor mapped and the daemon refused every reading, with no sign in the interface.
- Home Assistant: the page of a room in Configure shows the correction outside the closed
  section, with one line for each of the four methods.
- Dashboard: the room screen shows the room temperature from Home Assistant, its sensor, its age
  and the method that uses it. When freedo ignores the sensor, the line says so and opens the
  correction of the room.
- Daemon: `GET /api/zones` shows in `externalTemp.refused` the last reading that a zone refused.
- Docs: the integration README compares the four methods: who regulates the room, what the valve
  sensor does, what freedo writes, radio, and state on air.
- Run on a real plant with 0.11.0-beta.10: six rooms on the coordinator as owner, on the Home
  Assistant thermometers. The wall thermostat holds one observe registration for each room.

## Unreleased

- A heating room has one room temperature source: a device of the room, or the Home Assistant
  thermometer with the coordinator as owner. freedo no longer runs Continuous shift (offset mode
  `setpoint`) and Valve calibration (`auto_offset`). A Home Assistant automation or Better
  Thermostat writes a correction from a sensor through the Offset number entity.
- Daemon: the first start of this version moves each room in offset mode `setpoint` to `config`.
  It deletes the `auto_offset` options, the stored shifts and the stored calibration, and logs one
  line for each room it changes. The first start that transmits writes the valves of those rooms
  again without the shift. An import releases no shift.
- Daemon: `PATCH /api/install/zones/{n}` refuses `offset_mode` `setpoint`, `auto_offset` and
  `auto_offset_interval_sec`. `POST /api/zones/{n}/temperature` takes a reading only on a room the
  coordinator owns. `GET /api/zones` drops `autoOffset` and, in `externalTemp`, `shiftC`,
  `wantedC`, `offsetC` and `learnInUse`: `inUse` says that the controller and the learners read
  the reading.
- Home Assistant: the select Temperature source of a room lists the devices of the room and the
  Home Assistant sensors. A sensor makes the coordinator the owner of the room, a device gives the
  room back to that device. The select Room temperature correction, the switch Valve calibration
  and the number Calibration are gone: delete their entities from the registry. The sensor
  Temperature source state says `device` for a room a device owns.
- Dashboard: the room settings show one select, who measures the room: the devices of the room,
  or the Home Assistant thermometer. The offset and the offset mode sit under Advanced.
