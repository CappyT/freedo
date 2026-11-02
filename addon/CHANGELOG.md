# Changelog

## 0.11.0

- A heating room has one room temperature source: a device of the room, or the Home Assistant
  thermometer with freedo as the owner of the room. The owner runs the valve controller with the
  gains of the room on that reading, answers the measure to the members of the room and pushes it
  to them every minute, so the valves follow its demand and no offset on their sensor is needed.
  The wall thermostat takes the demand of such a room from the CoAP observe notifications of the
  coordinator, on one measure path `/z<N>` per room. The registrations and the fresh readings
  survive a restart of the daemon.
- Continuous shift (offset mode `setpoint`) and Valve calibration (`auto_offset`) are gone. The
  first start of this version moves each room in offset mode `setpoint` to `config`, deletes the
  `auto_offset` options, the stored shifts and the stored calibration, and logs one line for each
  room it changes. The first start that transmits writes the valves of those rooms again without
  the shift. A release write that a restart interrupts goes out again as a plain rewrite, so a
  running manual overlay keeps the old shift until it ends. A Home Assistant automation or Better
  Thermostat writes a correction from a sensor through the Offset number entity.
- Daemon: `PATCH /api/install/zones/{n}` sets `owner` (`device` or `coordinator`) and refuses
  `offset_mode` `setpoint`, `auto_offset` and `auto_offset_interval_sec`.
  `POST /api/zones/{n}/temperature` takes a reading only on a room the coordinator owns.
  `GET /api/zones` shows the owner and the observers of an owned room and, in `externalTemp`,
  `inUse` and the last `refused` reading. It drops `autoOffset`, `shiftC`, `wantedC`, `offsetC`
  and `learnInUse`.
- Daemon: the flow limit moves only when the curve value before the rounding is
  `flow_hysteresis_c` from the served one. Before, the default curve moved the limit between 48
  and 49 C every 30 minutes at 13 C outdoor, and each change made the wall thermostat reload its
  configuration and hold its rooms in frost fallback for up to 4 minutes.
- Home Assistant: the select Temperature source of a room lists the devices of the room and the
  Home Assistant sensors. A sensor makes the coordinator the owner of the room, a device gives the
  room back to that device. The sensor Temperature source state says `device` for a room a device
  owns. The select Room temperature correction, the switch Valve calibration and the number
  Calibration are gone: the setup of the integration removes their entities from the registry.
- Dashboard: the room settings show one select, who measures the room: the devices of the room,
  or the Home Assistant thermometer. The note names the wall thermostat when it measures the
  room. The offset and the offset mode sit under Advanced. The room screen shows the room
  temperature from Home Assistant, its sensor, its age and whether freedo uses it.
- Docs: the integration README says, for each temperature source, who regulates the room, what
  the valve sensor does, what freedo writes and what goes on air. The default weather curve of the
  flow limit is checked against eight monthly reports of the tado cloud of one plant.
- Run on a real plant through the 0.11.0 pre-releases: six rooms on the coordinator as owner, on
  the Home Assistant thermometers, one room with the wall thermostat and two rooms with two
  valves. The last pre-release ran three days with no error.

## 0.10.3

- Dashboard: the room report lists the heating periods of the day, the stretches with heat demand, also in a
  room whose program keeps one temperature. A period names the return home or the target rise that started
  it, also a rise of the away temperature. A heat-up of the program shows in its period with the time to
  reach the target.

## 0.10.2

- Dashboard: the outside reading of the Boiler page shows the outdoor temperature that Home Assistant gives
  the daemon when the boiler has no outdoor sensor. It shows `-` again once that reading is stale.

## 0.10.1

- Dashboard: the return, hot water and outside readings of the Boiler page are readable again. A style of
  the statistics tiles had turned them into dark tiles with dark text.
- Daemon: with the burner off, the energy and fuel estimate keeps running at 0 while the wall thermostat
  is silent. Only a silence after a running burner makes it `stale`. With the burner off the thermostat
  stays silent for more than 17 minutes on the plant.
- Dashboard: the notice of a stopped fuel counter on the page Consumption names the real reason, and links
  to the boiler setup only when the fuel flow is missing.
- Dashboard: a device names its radio link warning: a missed configuration answer, which clears by
  itself, or no radio neighbour.
- Statistics run on one plant.

## 0.10.0

- Statistics, off by default: Settings turns them on. The daemon then keeps a sample of each room and of
  the boiler every 5 minutes for 90 days, the fuel, heat and burner time of each day for 13 months, and
  the heat-ups of each room. The data lives in its own file beside the state database, and a backup does
  not include it. Off keeps the data until a delete.
- Dashboard: a room screen links to the day report of the room: the heating time, the average, lowest and
  highest temperature, a chart of the temperature over the target, the heating demand and the humidity,
  the open-window and away periods, a table of the values every hour and the heat-ups of the day.
- Dashboard: the page Consumption, from the Boiler page, shows the fuel, the cost, the heat and the burner
  time per day of a month and per month over 13 months, and takes the price of one fuel unit.
- Daemon: `GET` and `PUT /api/history`, `GET /api/history/zones/{n}` and `GET /api/history/energy`.
  Home Assistant ingress reaches them without the token.
- Dashboard: on a phone the profile menu of the Home tile opens from the right edge of the tile, so it
  stays on the screen.
- Dashboard: an amount of fuel always shows two decimals, so the columns of a table line up.
- Run on a daemon with synthetic data in headless Chrome. The statistics did not run on a plant.

## 0.9.0

- Dashboard: the page Boiler takes the gas meter readings: a reading with the time it was read and an
  option for a new or replaced meter, the list of the readings with what the meter and the estimate
  counted between two of them, the learned correction factor, and an undo of the last reading.
- Dashboard: the page Boiler setup, from the button at the top of the page Boiler, writes the fuel,
  the output and the fuel flow of the boiler, the highest flow temperature, the default weather curve,
  a curve per circuit, the adaptation and the timing of the flow limit. Each section says where the
  manual of the boiler lists the values. The page draws the curve with the outdoor temperature of now.
- Dashboard: a room screen judges the air comfort from the temperature and the humidity, with the
  ranges of EN 16798-1, and warns of a mould risk from 70 %.
- Daemon: `PUT /api/install/boiler` writes the boiler and flow limit options alone. Home Assistant
  ingress reaches it and the gas meter routes without the token.
- Daemon: `energy.gas.meter` in `GET /api/boiler` lists the stored readings in `entries`.
- Run on a daemon with synthetic data in headless Chrome. The new pages did not run on a plant. The
  default weather curve is switched on on one plant; its check over a range of outdoor temperatures is
  open.

## 0.8.1

- Dashboard: a tile with no temperature shows a dash in place of the number, like the dial of the
  room screen.
- Dashboard: in a schedule bar the time of now is the only dark line, with a dot in a white ring on
  its top edge. The lines between the blocks are white in both themes.

## 0.8.0

- Dashboard: a new design on the model of the tado app. The home page shows one tile per room, in
  the colour of its state: heating, idle, window open, off, away or fault. A tile shows the
  temperature, the humidity and one line of state. The tiles of the presence, the active profile,
  the hot water and the boiler follow the rooms. The alert strip names the devices.
- Dashboard: a room opens on a screen in the colour of its tile, with the dial, and the tile grows
  into that screen. The screen shows how long a manual change lasts, the schedule of today with the
  next change, the state and the devices of the room. The hot water and the boiler open the same
  way.
- Dashboard: the navigation has four items: Home, Programs, Devices and Settings. The boiler has no
  item: its tile on the home page opens it.
- Dashboard: Programs shows the profiles, home and away, one day at a time and one bar per room. The
  editor opens under the room.
- Dashboard: Rooms and devices shows one line per device, and a tap opens the line with the detail,
  the child lock and Identify. The group No room always shows. Rename, move, restart and removal
  stay in the room settings.
- Dashboard: Settings groups the installation, the appearance, the system, the backup and the
  access. Restore backup opens the file picker. The foot of the page names the version of freedo.
- Dashboard: selects, radios and checkboxes look the same in the light and the dark theme.
- The status colours in `assets/brand/tokens.css` are a tonal set at equal lightness. Text on a
  coloured surface takes the dark step of the same hue.
- Daemon: `GET /api/health` reports `version`.
- Run on a daemon with synthetic data and a replay, in headless Chrome. The new dashboard did not
  run on a plant.

## 0.7.2

- Daemon: with Transmit on, during the wait after an import the daemon serves the web interface.
  `GET /api/health` reports `importWait` with the seconds left, and every other API route answers
  `503` with `Retry-After`. The radio stays closed. The wait still counts 250 seconds from the
  import, and a restart in the middle continues it.
- Dashboard: a countdown shows on every page during that wait. The page takes no command and
  returns to normal by itself.
- While one add-on waits, the other add-on refuses to start.
- Run on one plant with 0.7.1: the return from freedo beta to the add-on freedo with a backup. The
  countdown did not run on a plant.

## 0.7.1

- Breaking: the add-ons copy no state by themselves. The add-on freedo writes no copy to
  `/share/freedo/stable` at start and stop, and freedo beta imports none at its first start.
- Breaking: the option `import_stable_state` of freedo beta is gone. Home Assistant shows a warning
  for a saved value until you save the configuration once.
- Daemon: `GET /api/state/export` downloads the whole state as one SQLite file.
  `POST /api/state/import?confirm=true` checks such a file, keeps the replaced database in
  `backup/before-import-<time>/` (the three newest stay) and restarts the daemon by itself. A file
  of a newer schema is refused. The log names each export and each import.
- Daemon: with Transmit on, a start less than 250 seconds after an import waits for the rest of
  that time. The web interface does not answer during the wait, and the devices keep their last
  target.
- Dashboard: Settings has the section Backup, with Download backup and Restore backup. The backup
  holds the network key: keep the file private.
- In the web interface of the add-on, the backup is for Home Assistant administrators only and
  needs no token. The add-on asks Home Assistant about the user at every request and refuses when
  it gets no answer. Another client needs the API token.
- The add-ons have the permission `homeassistant_api`, to ask Home Assistant whether a user is an
  administrator. With it an add-on can call the whole API of Home Assistant. freedo uses the list
  of the users only.
- The move to freedo beta and the return are now: download a backup in one add-on, stop it, start
  the other one, restore the backup there.
- The folder `/share/freedo/stable` of older releases holds the network key and nothing reads it.
  The add-on does not delete it: delete it by hand. The log names it at each start.
- Run on one plant with freedo beta: a download and a restore in the same add-on. A restore in the
  other add-on did not run.

## 0.7.0

- Daemon: a user reset ends an open window of any source, also one that a device detected. A window
  sensor that closes ends only the hold that Home Assistant set.
- Dashboard: the room settings hold the room options: open window detection and its time, the away
  temperature, the end rule of a change made on a device, early start, the offset mode, the offset
  and the valve calibration. The page names the options that cost a motor stroke.
- Dashboard: the room settings have a Restart action for a valve and for the wall thermostat, with
  a confirmation. The restart command is open to Home Assistant ingress: every Home Assistant user
  who can open the page can restart a device.
- Dashboard: Add device has no default room. The page declares the device, adds it to the room and
  then pairs it. It shows only the progress while the pairing runs, and it ends with one state that
  names the paired device and the room that the daemon holds.
- Dashboard: the Boiler page and a banner show the fault codes of the boiler, as numbers.
- Dashboard and Home Assistant use the same words for the end rules, the alerts and the heating
  demand. No entity id and no state value changed.
- Home Assistant: binary sensor Boiler fault, event entity Alert, service `freedo.pair` and switch
  Pairing mode. The integration reloads by itself after a change of the installation.
- Blueprints: notifications for alerts, an open window and a warmer outside temperature; a setback
  in steps with the distance from home; auxiliary heat for a room from another appliance; a monthly
  valve exercise.
- Run on one plant with the pre-releases of freedo beta: the room options, a room created and
  deleted, a device moved, removed, paired and restarted from the dashboard, and a window that the
  wall thermostat detected with its reset.
- The add-on freedo does not read the state of freedo beta. After a stay on freedo beta, the add-on
  freedo starts from its own state: a room, a device or a schedule that changed on freedo beta is
  not in it.

## 0.6.1

- Home Assistant: the list of rooms in Configure follows the language of the user. A table shows
  the area, the temperature source and the number of window sensors of each room, and the choices
  are the room names. A window sensor that another room uses shows that room in square brackets.
- Home Assistant: a sensor that is the only temperature sensor of its device shows the name of
  the device. The options of the select Temperature source change their text for such sensors.

## 0.6.0

- Add-on freedo beta: a second add-on of the repository runs the pre-releases. It has its own data
  and imports a copy of the state of the add-on freedo at its first start. The two add-ons must
  not run at the same time. See the documentation of freedo beta for the move and the return.
- The option `channel` is gone. The add-on freedo runs stable releases only. Home Assistant shows
  a warning for a saved `channel` value until you save the configuration once.
- The add-on freedo writes a copy of its state to `/share/freedo/stable` at each start and stop.
  The copy holds the network key.
- Home Assistant: Configure has one page for each room, with its area, its temperature source, its
  window and door sensors and its correction. A list shows what each room uses. A window sensor
  that another room uses moves to the room that selects it. The select Temperature source offers
  the same choices as the page.
- Home Assistant: a room stays off while one of its window sensors is open, also past the time
  limit of the daemon. A reset of the window by service does not hold while a sensor is open.
- Dashboard: the settings of a room rename, move and remove its devices. The devices in no room
  form the group No room. The device rows keep Identify and Child lock.
- Dashboard: the burner badge follows the burner of the boiler, also while it heats hot water. A
  second badge shows a heating request.
- Dashboard: one name for each page in the navigation, the title and the links.

## 0.5.2

- Breaking: the routes `POST /api/nodes/{id}/factoryreset` and `POST /api/nodes/{id}/netcmd` are
  gone. No device resource receives the network commands they sent, so they had no effect. A
  device leaves its network only when its button is held.

## 0.5.1

- Dashboard: the page First setup shows the six steps of a new installation with their live state.
  The page Rooms and devices creates, renames and deletes rooms, pairs a device and places it in a
  room, moves and removes devices, and sets the heating circuit, the measuring device and the
  demand reporter of a room. Both pages work from Home Assistant with no token. Every Home
  Assistant user who can open the page can change the installation and pair devices.
- Pairing: the dashboard opens the pair window for ten minutes, waits for the device after you hold
  its button, and reports success only when the device joined. A new pairing ran on a real plant
  with one valve.
- Device warnings: a valve that is not mounted on a radiator, has a short travel, calibrates again
  or has a weak supply for the motor reports a warning and no fault. Home Assistant gets the
  binary sensor Valve warning, and the Fault sensor lists the names of the faults. The dashboard
  shows each warning and fault by name.
- The removal preview of a device lists the rooms it leaves.
- `PATCH /api/install` with `"flow_limit_c": null` clears a stored flow limit.

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
