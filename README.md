# freedo

![The freedo dashboard on a phone, in the light and the dark theme](https://raw.githubusercontent.com/CappyT/freedo/main/docs/images/hero.png)

A local coordinator for tado V3+ 868 MHz heating devices, with a Home Assistant integration.

This is heavily AI-assisted work. The code and the documentation were written largely with AI assistance, under the direction of the project owner. Behavior was checked with automated tests, and on a real plant only where the [Status](#status) section says so.

## Contents

- [What it is](#what-it-is)
- [Status](#status)
- [Scope and limits](#scope-and-limits)
  - [Supported devices](#supported-devices)
- [Requirements](#requirements)
- [Install](#install)
  - [Home Assistant: add-on and integration](#home-assistant-add-on-and-integration)
  - [Container](#container)
- [First setup](#first-setup)
- [Dashboard](#dashboard)
- [Features](#features)
- [Safety](#safety)
- [Community](#community)
- [Documentation](#documentation)
- [Development](#development)
- [Licence](#licence)

## What it is

freedo replaces the tado internet bridge for the radiator valves and wall thermostats that it
coordinates. The coordinator is a Go daemon. It talks over a serial link to an ESP32 with a CC1101
radio, and it serves a local HTTP API. A Home Assistant integration uses that API. The daemon
keeps the plant under local control, with no cloud.

freedo is not a way to use the tado app or the tado cloud. See [Scope and limits](#scope-and-limits).

freedo is not affiliated with tado. tado is a trademark of its owner. This project uses the
name only to describe which devices it works with.

## Status

freedo is experimental. Expect bugs, and keep a way to control the heating by hand.

Tested on a real plant (one plant, one ESP32 development board with one CC1101 module):

- Reception and decoding of device reports (temperature, demand, boiler values, battery), and the MAC and CSL
  exchange with the bridge firmware.
- Setpoint, boost and return-to-program writes to a valve.
- Home and away: in use on the plant, and the valves follow.
- A weekly schedule: the rooms follow its blocks, and a schedule edited in the dashboard reaches the valves.
- The child lock of a valve: in use on the plant.
- Hot water on, off and temperature writes to the wall thermostat.
- A fixed flow limit, read back from the boiler through the OpenTherm values the thermostat reports.
- Identify on a valve and on the wall thermostat.
- The display orientation and the change notice of a valve.
- The motor calibration, on one valve.
- The temperature correction in the modes `config` and `dbg2`, on one valve.
- A window state that the coordinator sets, on one valve.
- A window that the wall thermostat detected: its report, the block that reaches the thermostat and the
  valve of the same room, the close after the window time, and a reset that ends the window before that
  time. No window that a valve detected was seen: with one sample each 10 to 16 minutes, a valve did not
  report an open window in 40 minutes.
- The warning for a valve that is not mounted. It raised no fault alert, and it cleared after the valve
  was mounted.
- Pairing one valve through the API: the button press that makes the valve leave, the removal from the
  installation, the pair window of the bridge firmware, the declaration of a device that the installation
  did not hold, the `d/pair` command and the serial that the daemon learns from the device. The valve
  works in its zone again. A remote leave command was tried on that valve and had no effect.
- Pairing the same valve from the dashboard, two times. The last version of the page Add device, with no
  default room and with the end state, did not run on the plant.
- On the page Rooms and devices: a room created, its options saved and the room deleted, a device moved
  from No room to a room, a device removed, and a valve restarted. The valve joined again by itself about
  40 seconds after the command.
- The add-on freedo beta: the move from the add-on freedo, with the copy of the state that the add-ons
  made by themselves up to 0.7.0. Every device talked to the beta add-on in three minutes. The return
  to the add-on freedo did not run with that copy.
- The backup of the dashboard: a download as a Home Assistant administrator with no token, a restore
  of that file in the same add-on, and the return from freedo beta to the add-on freedo with a
  restore there. The daemon kept the replaced state, restarted by itself, waited 250 seconds and
  talked to every device again with no decryption error. The countdown that the dashboard shows
  during that wait did not run on a plant.
- The read of the firmware version of each device.
- freedo runs as the only coordinator of that plant, as a Home Assistant add-on with the integration.
  The devices of that plant were not paired from scratch, apart from one valve.
- The dashboard commands for setpoint, boost, off, home and away, and the programs.

Tested only with automated tests and synthetic fixtures:

- Away temperatures, away programs and the end rule `until_auto_change`.
- A network that starts with no paired device, and the pairing of a wall thermostat.
- A window that a valve detects.
- The Router Advertisement gateway and DNS addresses (`router_gateway`, `router_dns`) and the zone
  configuration of a heating zone with no circuit.
- A schedule document with several day programs, the program profiles and the offline schedule
  (devices run the program alone).
- Firmware transfer to a device. No block was ever sent on air.
- Early start, the weather curves (your own and the default curve), automatic flow adaptation, the
  external room temperature, the automatic correction and the controller gain suggestion.
- The energy and gas estimate against the gas meter of the plant, over 8 days: the meter counted
  7.07 m3 and the estimate 7.38 m3, 4.4 % high. The daemon learned a correction factor of 0.958. The
  meter also counts any other gas user on it. A third reading checks that the factor holds.
- The web dashboard. `npm test` covers the API client and the schedule editor logic. `npm run prefix-check`
  and `npm run ui-check` start the daemon with no radio and a seeded synthetic installation. They open each
  page in headless Chrome under a path prefix, and drive the schedule editor, the presence switch, the
  child lock, Turn off, the profiles, the rename of a device, the page Rooms and devices (a room, a
  pairing, a move, a removal, a restart, a circuit), the options of a room, the boiler fault codes and the
  first setup on an empty database. The page First setup did not run on a plant that starts empty.
- The boiler fault codes in the dashboard and in Home Assistant. No real boiler fault was seen.
- In the integration: the reload after a change of the installation, the service `freedo.pair` and the
  event entity Alert.
- The six blueprints: presence, season profile, alert notification, distance setback, auxiliary heat and
  valve exercise.

No record covers a long run or a long outage of the coordinator.

## Scope and limits

- freedo replaces the tado internet bridge and the tado cloud for the devices it coordinates.
  It does not connect to the tado cloud in place of the bridge. The tado app does not work with a
  plant that freedo runs. This is out of scope.
- Device firmware updates: whether freedo can support them is under evaluation. If it does, you
  supply the firmware images. The project distributes none. The daemon has a transfer route that
  no test on air has used.
- An existing tado plant pairs to freedo from scratch. Each device leaves the commercial bridge and
  joins the network of freedo. The pairing ran on air for one valve. Only automated tests cover a
  network that starts with no paired device. You lose the tado app and the cloud functions for those
  devices. The
  rooms and schedules that you set in the tado app do not move to freedo. You describe the rooms again
  in the dashboard or through the API, and the schedules in the dashboard, Home Assistant or the API. To go back,
  pair the devices to the tado bridge again. That procedure belongs to the tado bridge and is not
  documented or tested here. freedo has no leave command and no remote detach. A device leaves its
  network only when you hold its button.
- A setup import from the tado cloud is planned: rooms, schedules and settings. The devices still pair from
  scratch. It is not available.
- Supported hardware: the valves and the wall thermostats of the V3+ line. Other device types
  (wireless receiver, extension kit, wireless sensor) are not supported, because no capture of
  them exists. See [Supported devices](#supported-devices) for the other tado lines.
- A circuit needs a wall thermostat as its heat source. A heating zone with no circuit gets a zone
  configuration without the circuit tag and without the heat source among the members. No device has
  run this configuration, and the install check warns about such a zone. A plant of valves without a
  wall thermostat is not supported yet.
- The boiler energy estimate needs a boiler that reports its modulation over OpenTherm. A boiler on a
  relay has no estimate yet.
- Geofencing, push notifications and multiple users are not part of the daemon. Home Assistant provides
  them. HomeKit is not implemented.

### Supported devices

freedo replaces the tado Internet Bridge IB01 and pairs each device to its own network. The device
models come from the tado article
[What is a tado° Starter Kit, and what are the differences between versions?](https://support.tado.com/en/articles/3403569-what-is-a-tado-starter-kit-and-what-are-the-differences-between-versions)

| Line | Sold | Devices | tado bridge | freedo |
|---|---|---|---|---|
| V1 | 2012-2014 | Box BX02, temperature sensor TS02 | Gateway GW01 | Not supported: a different system. |
| V2 | 2014-2016 | Smart Thermostat RU01, extension kit BU01 | Bridge GW02 | Not tested. Probably the same radio protocol, see [#25](https://github.com/CappyT/freedo/issues/25). |
| V3 | 2016-2018 | Smart Thermostat RU01, Smart Radiator Thermostat VA01 | Internet Bridge IB01 | Not tested. Probably works: it uses the same bridge as V3+. |
| V3+ | since 2018 | Wired Smart Thermostat RU02, Smart Radiator Thermostat VA02, Wireless Receiver BR02/BP02, Wireless Temperature Sensor SU02 | Internet Bridge IB01 | Valves and wired thermostat supported. Wireless receiver and sensor not supported. |
| tado X | since 2024 | RU04, RU05, VA04, SU04, SU05 and others | Bridge X IB02 | Not supported: Thread and Matter use a different radio. |

V2 is probable because tado documents a swap from the GW02 to the IB01 in which the installed
devices reconnect by themselves, and because tado sold the same RU01 with both bridges. Nobody has
tested a RU01 with freedo yet. The RU01 drives the boiler through the extension kit BU01, while
freedo knows the RU02, which drives the boiler over OpenTherm by itself.

## Requirements

- Radio hardware: an ESP32 with a CC1101 module on a USB port, flashed with the bridge firmware
  in [`firmware/cc1101_bridge`](firmware/cc1101_bridge/README.md). Each release attaches the
  firmware image: see [Flash a released image](firmware/cc1101_bridge/README.md#flash-a-released-image).
  This firmware is almost completely AI-generated. See the [notice](firmware/cc1101_bridge/README.md)
  in its README: it ran on one ESP32 development board with one CC1101 module only.
- A radio board design for the bridge exists in [`hardware/radio`](hardware/radio/README.md). This PCB design is
  almost completely AI-generated and is not tested. See the [notice](hardware/radio/README.md) before you use it.
- A Home Assistant installation (2026.8 or later) or a host that runs a container.
- Go, if you build from source. The version is in `go.mod`.

Use one coordinator per radio network. Switch the commercial bridge off before freedo transmits.

## Install

### Home Assistant: add-on and integration

The repository is an add-on repository and a HACS repository. Home Assistant calls an add-on an
"app" in recent releases.

1. Open the add-on store, the menu, then Repositories. Enter `https://github.com/CappyT/freedo`.
2. Install freedo. On the Configuration tab select the serial port of the ESP32 and save.
3. Start the add-on and leave Protection mode on.
4. In HACS, open the menu, then Custom repositories. Enter `https://github.com/CappyT/freedo` with
   type Integration. Download freedo and restart Home Assistant.
5. Home Assistant lists freedo as discovered. Confirm it. The discovery carries the host, the
   port and the token.

The add-on and the integration follow two channels:

- Stable: tags `vX.Y.Z`. The add-on freedo and HACS offer them by default.
- Experimental: pre-release tags such as `vX.Y.Z-beta.N`, with less testing on a real plant. HACS
  offers them to the integration when you turn on the beta versions of the freedo repository. The
  second add-on of the repository, freedo beta, runs them. It has its own data. To move the plant,
  download a backup on the page Settings of one add-on, stop it, start the other one and restore
  the backup there. The same steps go back. The two add-ons must not run at the same
  time. See [`addon-beta/DOCS.md`](addon-beta/DOCS.md) for the move and the return. The daemon
  images of a pre-release carry the version tag and `beta`, so a container installation can run
  them.

The add-on starts in dry-run. See [`addon/DOCS.md`](addon/DOCS.md) for the options, the data and the
backups. See [`custom_components/freedo/README.md`](custom_components/freedo/README.md) for the
entities, the services and the blueprints.

### Container

The image `ghcr.io/cappyt/freedo` exists for `linux/amd64` and `linux/arm64`.

```sh
cp docker-compose.example.yml docker-compose.yml
openssl rand -hex 32 > freedo_api_token
docker compose up -d
```

Edit the serial device, the group id and the time zone (`TZ`) in the file first. Without `TZ` the
container runs in UTC and the devices get a UTC offset of 0. The container starts in dry-run. For the
volume, the time zone and the build from source, see [`docs/OPERATION.md`](docs/OPERATION.md#container).
Add the integration in Home Assistant from Settings, Devices and services. Enter the host, the port and
the token.

## First setup

The page First setup of the [dashboard](#dashboard) shows these steps with their live state.

1. Start the daemon in dry-run and read its log.
2. Check `GET /api/health`. The field `calibrated` must be `true`. If it is `false`, calibrate the radio once:
   see [Radio calibration](docs/OPERATION.md#radio-calibration). The field `timezone.name` must be the
   time zone of the plant. If `timezone.fallback` is `true`, the daemon uses the zone of its host: set
   the time zone on the page, or see [Time zone](docs/OPERATION.md#time-zone).
3. Add the rooms, on the page or through the API under `/api/install`. The daemon creates a network key
   and a coordinator identity at the first start.
4. Switch the commercial bridge off and add `--tx` (or the add-on option Transmit).
5. Pair the devices, on the page or as described in [`docs/OPERATION.md`](docs/OPERATION.md#pairing-a-device).
6. Add the heating circuit, on the page or through the API.

The calibration, the serial port, `--tx` and the commercial bridge stay outside the page.

## Dashboard

The daemon serves a web interface at its HTTP address. It shows the zones, the hot water, the boiler and
the devices. It sets temperatures with the four end rules, boosts a zone and switches it off. It switches
the home between home and away. It edits the home and away schedules, and it creates, renames, deletes and
activates the program profiles. It sets the flow limit, renames a device, locks a valve and identifies a
device. It resets an open window of a room, also one that a valve detected. It shows the fault codes of
the boiler on the page Boiler and in a banner on every page. It has English, Italian, German and French texts and a light and a dark theme.

![The home page, a room, the programs and the devices on a phone](https://raw.githubusercontent.com/CappyT/freedo/main/docs/images/dashboard-tour.png)

![The home page on a wide screen in the dark theme, with a room on a phone](https://raw.githubusercontent.com/CappyT/freedo/main/docs/images/dashboard-desktop.png)

The screenshots show a test installation with invented data.

- Container: open `http://<host>:8080/` and enter the API token once.
- Home Assistant add-on: select Open Web UI, or the entry freedo in the sidebar. The page needs no token.

The page Rooms and devices manages the installation. It creates, renames and deletes rooms. It adds a
heating circuit, changes its heat source and deletes it. In the settings of a room it renames, moves, restarts and removes a
device of that room, and it sets the circuit, the measuring device and the demand reporter. The devices in no
room form the group No room, with the same settings. It adds a device: it opens the pair window, declares
the device that the window hears and sends the pair command. A removal shows the effects before you confirm. Under Home Assistant ingress this
page and the page First setup need no token, so every Home Assistant user who can open the dashboard can
change the installation and pair a device. The network key, the identity of the network,
the kind and the serial of a device, the controller gains of a room and the name of a circuit stay in
the API. The settings of a heated room hold its options: window detection, away temperature, the end
rule of a change made on a device, early start, offset mode, offset and valve calibration. See
[`docs/OPERATION.md`](docs/OPERATION.md#dashboard) for the access rules and
[`dashboard/README.md`](dashboard/README.md) to build it.

## Features

The table compares the functions of the tado app with freedo. "equal" means the function exists in the
daemon, or in the integration and the blueprints that freedo ships, and has automated tests. The note
says when a function needs Home Assistant. "partial" means a part of the function is missing.
"extra" means the tado app has no such function. "not implemented" means freedo has none. The notes name
what is not verified on air. The routes are in [`docs/API.md`](docs/API.md). The entities are in
[`custom_components/freedo/README.md`](custom_components/freedo/README.md).

| Function | Status | Notes |
|---|---|---|
| Manual change with the four end rules: manual, timer, until the next block, until the next automatic change | equal | `until_auto_change` writes `6240=1`, captured for rooms. No capture holds the end of that overlay. For hot water the value is not verified. |
| Switch a zone off | equal | The form "off until the next block" is captured. The manual form is not. |
| Resume the program, for a zone or for the whole home | equal | |
| Boost for a zone or for all zones | equal | Home Assistant sends one command for each zone. |
| Weekly schedule and copy | equal | In use on one plant: the rooms follow the blocks, and an edit reaches the valves. Several day programs in one document are not verified on air. |
| Home and away | equal | The write shapes come from a capture. In use on one plant: the valves follow. |
| Away temperature for each zone | equal | 5 C means off in away, as in the cloud. Hot water takes no away temperature: it is off in away unless it has an away schedule. Not verified on air. |
| Away schedule for each zone | extra | A zone with an away schedule follows it while the home is away, hot water included. No capture holds this write shape. Not verified on air. |
| Program profiles | extra | A profile holds the home and away schedules of every zone. One profile is active. The dashboard and Home Assistant switch it, and a blueprint switches it by season. Not verified on air. |
| Preheat before arrival | partial | Two blueprints. One ends away when a person enters an approach zone. One sets the away temperature of each room in steps with the distance of the nearest person. Both read a distance and no travel time, and every room gets the same steps. They need Home Assistant. |
| Early start | equal | The daemon learns the heating rate of each zone. No firmware does this. Not verified on air. |
| Geofencing | equal | A blueprint uses the presence of Home Assistant for the whole home, and a second one uses the distance of the nearest person. Needs Home Assistant: the daemon has no geofencing code. |
| Open window detection | equal | The valves detect the window. The detection and its time are room options. A reset from the dashboard or from Home Assistant ends a window of any source. A contact sensor that closes ends only its own hold. On one plant the wall thermostat detected a window and a reset ended it. No detection by a valve was observed. |
| Hot water: on, temperature, program | equal | The daemon keeps the program and writes the thermostat at each block change. |
| Child lock | equal | Valves only. The wall thermostat firmware has no child lock. In use on one plant. |
| Identify a device | equal | Valves and the wall thermostat in a zone. The thermostat shows "Hi!" on its display. |
| Display orientation and change notice | equal | A full `d/config` write homes the valve motor, as with the commercial bridge. |
| Valve calibration | equal | The command comes from firmware and ran on one valve. A `2.xx` answer does not guarantee the stroke. |
| Temperature correction | equal | Three modes. `config` homes the motor. `dbg2` ran on one valve and is in no capture. |
| Automatic correction from an outside sensor | extra | In modes `config` and `dbg2`, at most once per settable interval, with a deadband. Each write in mode `config` homes the motor. Not verified on air. |
| Room temperature from an outside sensor | extra | Mode `setpoint` shifts the setpoints. With the automatic correction the learners read the sensor. Home Assistant chooses for each room a device of the room or the sensor. Filter and limits are design estimates. Not verified on air. |
| Manage rooms and devices | equal | The dashboard creates, renames and deletes rooms, creates and deletes circuits, moves and renames devices, and sets the measuring device, the demand reporter and the room options. Home Assistant has the same room options and maps each room to an area. The controller gains, the network key and the identity of the network are in the API only. No device reports the offset that it holds, so the dashboard shows the offset only in the mode `setpoint`. A device declared without a serial takes it from its first request. Home Assistant follows a change within one poll. The page Rooms and devices is in use on one plant. |
| Interface | equal | A web dashboard on the daemon monitors and controls the zones, the hot water, the boiler and the devices, and manages the schedules, the profiles and the installation. The control pages and the page Rooms and devices are in use on one plant. The page First setup did not run on a plant that starts empty. |
| Pair a device | partial | The dashboard and the API pair a device. Both flows ran on air on one valve. A device can miss the first pair command: send it again. The integration opens the window, lists the waiting devices and sends the pair command to a device that freedo holds (service `freedo.pair`). Missing: the integration cannot add a new device, and its hub device links to the dashboard for that. Not verified on air: a wall thermostat and a network with no paired device. |
| Remove a device | partial | The dashboard and the API report the effects first. The removal from the dashboard ran on one plant. The delete sends no frame. freedo has no remote detach: a device leaves its network only when you hold its button. |
| Firmware version and update | partial | The daemon keeps the version of each device across restarts and reads it once after start from a device that never reported it. A transfer route exists. No block was sent on air. freedo distributes no image. |
| Flow limit | equal | The fixed limit is verified on air through the OpenTherm reading. |
| Weather curve and automatic flow adaptation | partial | The curve has two points. The adaptation is a local controller and not the cloud algorithm. It is off by default. Not verified on air. |
| Default weather curve for the flow limit | partial | A built-in curve under `flow_limit_c`, fitted on one plant and one season of monthly means. It is not the cloud algorithm. Home Assistant must feed the outdoor temperature. It is off by default. The page Boiler setup switches it and draws it. On one plant since 2026-10-21. Checked against the monthly reduction of the tado cloud on the same plant over one season: within 0.6 C from 3.6 to 19 C outdoors, except one month of 1.8 C at the start of the season, and equal to the live cloud value at 8 C. Below 3 C it is an extrapolation. |
| Boiler telemetry and faults | equal | The dashboard shows the boiler fault codes on the page Boiler and in a banner on every page. Home Assistant has the binary sensor Boiler fault with the codes. A code is a number of the boiler maker, and freedo gives it no meaning. Not seen on a real boiler fault. |
| Energy and gas estimate | partial | Estimated from modulation and name-plate data. Not a measurement. The page Boiler setup holds the name-plate data, with a guide to the manual of the boiler. Gas meter readings, from the page Boiler or from Home Assistant, give an estimated meter value and, after two readings, a correction factor: 0.958 on one plant over 7 m3. With the statistics on, the page Consumption shows the fuel, the heat and the cost per day and per month from a price per unit. |
| Air comfort | partial | The room screen judges the room from its temperature and humidity, with the ranges of EN 16798-1 category II, and warns of a mould risk from 70 %. The outdoor air freshness of the cloud needs outdoor data and is missing. |
| History and reports | partial | Optional statistics in the daemon, off by default: a sample of each room and of the boiler every 5 minutes for 90 days, daily totals for 13 months, and the heat-ups of each room. The room report shows one day with the temperature, the target, the heating demand, the humidity and the heating periods, with their cause and, for a rise of the program, the time to reach the target. Runs on one plant with add-on 0.10.0. Home Assistant records the history of every entity as well. |
| Notifications | equal | In Home Assistant, the event entity Alert raises one event when an alert starts, and the blueprint `alert_notification.yaml` sends a notification for a low battery, a device fault, a valve warning, an offline device, a boiler fault and a window that stays open. The texts are English. Needs Home Assistant: the daemon sends no notification. Not run on a real plant. |
| Several users | not implemented | One bearer token. Under Home Assistant ingress every Home Assistant user can use the dashboard. |
| HomeKit | not implemented | |
| Duration of a change made on a valve | equal | The rule is applied by the daemon. No capture holds the thermostat buttons. |
| Offline schedule on the devices | extra | Devices run the program alone after 25 minutes without a coordinator. The 25 minute test on a real plant is open. |
| Auxiliary heat for a room | extra | A blueprint runs another climate entity of the room, for example an air conditioner, while the room is far below its target. It reads the freedo climate entity and never writes to freedo. It needs Home Assistant. Not run on a real plant. |
| Valve exercise | extra | A blueprint moves each valve once a month with the motor calibration, one valve at a time, so that the pin does not stick after the summer. It skips a valve with a low battery, a fault, a warning or no link, and waits while a room heats. Each run costs one full motor stroke per valve, on battery. Not run on a real plant. |
| Controller gains for each zone | extra | The suggestion writes nothing. The check on real episodes is open. |

## Safety

Heating is safety relevant.

- One coordinator per plant. Two coordinators on one network give duplicate answers and an
  inconsistent state. Switch the commercial bridge off before freedo transmits. As a last safeguard the
  daemon stops to transmit for the rest of the run when it hears a data frame with its own coordinator
  source address (`bridgePresent` in `/api/health`).
- The daemon starts in dry-run and sends nothing until you add `--tx`.
- Setpoints stay in range: 5 to 25 C for a room, 30 to 65 C for hot water.
- The wall thermostat enters a zone fallback when it loses the zone owner. The modes are `hold` (last demand,
  local program), `fixed` (80 % demand at 25 C) and `frost` (5 C). That is device firmware behavior. freedo
  reports it (`zoneFallback`, the binary sensor Fallback) and never sends a fallback mode.
- With `offline_schedule` on (the default), a device runs its weekly program alone after 25 minutes without
  an answer from the coordinator. This comes from a reading of the device firmware and is not tested on a plant. A device with no Router Advertisement stops to report after about 20 minutes.
  The daemon sends one at random intervals between 200 and 600 seconds.
- `flow_limit_c` has no default. While it is unset, freedo serves no flow limit to a circuit that no
  curve or adaptation manages. The code notes that the thermostat then applies 70 C. No measurement on
  a boiler confirms this value. Set `flow_limit_c` to a value that suits the boiler. freedo cuts a
  served limit to the highest CH setpoint that the boiler reports.
- After a restart the daemon restores its state and queues again the zone writes that no device confirmed.
- No test on a real plant covered a long outage of the coordinator. How a plant behaves for hours without a
  coordinator depends on the device firmware.
- The database holds the network key. The API token protects the plant. See [`SECURITY.md`](SECURITY.md).

## Community

- Questions and reports from users: the [freedo topic](https://community.home-assistant.io/t/freedo-a-local-coordinator-for-tado-v3-868-mhz-devices-no-cloud-or-internet-needed-open-source/1028536) on the Home Assistant community forum.
- Bugs and feature requests: the [issues](https://github.com/CappyT/freedo/issues) of this repository.

## Documentation

- [`docs/OPERATION.md`](docs/OPERATION.md): configuration, container, dashboard, calibration, pairing.
- [`docs/API.md`](docs/API.md): the HTTP API and the rules behind its routes.
- [`docs/PROTOCOL.md`](docs/PROTOCOL.md): radio protocol facts for developers.
- [`addon/DOCS.md`](addon/DOCS.md) and [`custom_components/freedo/README.md`](custom_components/freedo/README.md): Home Assistant.
- [`dashboard/README.md`](dashboard/README.md): the web dashboard, its build and its tests.
- [`firmware/cc1101_bridge/README.md`](firmware/cc1101_bridge/README.md): bridge firmware.
- [`hardware/radio/README.md`](hardware/radio/README.md): radio board design.
- [`CONTRIBUTING.md`](CONTRIBUTING.md) and [`SECURITY.md`](SECURITY.md).

## Development

```sh
go build ./...
go vet ./...
go test ./...
```

[`CONTRIBUTING.md`](CONTRIBUTING.md) describes the layout, the fixture rule and how to contribute a capture.

## Licence

Apache License 2.0. See [`LICENSE`](LICENSE) and [`NOTICE`](NOTICE).
