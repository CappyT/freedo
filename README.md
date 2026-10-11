<h1 align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/brand/svg/logo-reversed.svg">
    <img src="assets/brand/svg/logo.svg" alt="freedo" height="72">
  </picture>
</h1>

A local coordinator for tado V3+ 868 MHz heating devices, with a Home Assistant integration.

This is heavily AI-assisted work. The code and the documentation were written largely with AI assistance, under the direction of the project owner. Behavior was checked with automated tests, and on a real plant only where the [Status](#status) section says so.

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
- Setpoint and return-to-program writes to valves and to the wall thermostat.
- A fixed flow limit, read back from the boiler through the OpenTherm values the thermostat reports.

Tested only with automated tests and synthetic fixtures:

- Home and away writes, away temperatures and the end rule `until_auto_change`.
- Pairing a device to a freedo network, with the pair window of bridge firmware v2.9.
- The Router Advertisement gateway and DNS addresses (`router_gateway`, `router_dns`) and the zone
  configuration of a heating zone with no circuit.
- Weekly schedules on the devices and the offline schedule (devices run the program alone).
- Firmware transfer to a device. No block was ever sent on air.
- Early start, the weather curves (your own and the default curve), automatic flow adaptation, the
  external room temperature and the controller gain suggestion.
- The energy and gas estimate. No comparison with a gas meter exists.
- The web dashboard. A headless browser drove it against the daemon with a replayed synthetic capture. It
  has not run against a real plant, and the Home Assistant ingress path has not run inside Home Assistant.
- The Home Assistant integration and the presence blueprint. The repository documents automated tests only.

freedo has not run for several days as the only coordinator of a plant.

## Scope and limits

- freedo replaces the tado internet bridge and the tado cloud for the devices it coordinates.
  It does not connect to the tado cloud in place of the bridge. The tado app does not work with a
  plant that freedo runs. This is out of scope.
- Device firmware updates: whether freedo can support them is under evaluation. If it does, you
  supply the firmware images. The project distributes none. The daemon has a transfer route that
  no test on air has used.
- An existing tado plant pairs to freedo from scratch. Each device leaves the commercial bridge and
  joins the network of freedo. You lose the tado app and the cloud functions for those devices. The
  rooms and schedules that you set in the tado app do not move to freedo: you describe them again
  through the API. To go back, pair the devices to the tado bridge again. That procedure belongs to
  the tado bridge and is not documented or tested here. freedo can make a device leave its network
  (`POST /api/nodes/{id}/factoryreset`).
- A setup import from the tado cloud is planned: rooms, schedules and settings. The devices still pair from
  scratch. It is not available.
- Supported hardware: the valves and the wall thermostats of the V3+ line. Other device types
  (wireless receiver, extension kit, wireless sensor) are not supported, because no capture of
  them exists.
- A circuit needs a wall thermostat as its heat source. A heating zone with no circuit gets a zone
  configuration without the circuit tag and without the heat source among the members. No device has
  run this configuration, and the install check warns about such a zone. A plant of valves without a
  wall thermostat is not supported yet.
- The boiler energy estimate needs a boiler that reports its modulation over OpenTherm. A boiler on a
  relay has no estimate yet.
- Geofencing, history reports, push notifications and multiple users are not part of the daemon. Home
  Assistant provides them. HomeKit is not implemented.

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

The add-on starts in dry-run. See [`addon/DOCS.md`](addon/DOCS.md) for the options, the data and the
backups. See [`custom_components/freedo/README.md`](custom_components/freedo/README.md) for the
entities, the services and the presence blueprint.

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

1. Start the daemon in dry-run and read its log.
2. Check `GET /api/health`. The field `calibrated` must be `true`. If it is `false`, calibrate the radio once:
   see [Radio calibration](docs/OPERATION.md#radio-calibration). The field `timezone.name` must be the
   time zone of the plant. If `timezone.fallback` is `true`, the daemon uses the zone of its host: see
   [Time zone](docs/OPERATION.md#time-zone).
3. Describe the installation through the API under `/api/install`: devices, zones and circuits. The
   daemon creates a network key and a coordinator identity at the first start.
4. Switch the commercial bridge off, add `--tx` (or the add-on option Transmit) and run one safe command.
5. Pair the devices as described in [`docs/OPERATION.md`](docs/OPERATION.md#pairing-a-device).

## Dashboard

The daemon serves a web interface at its HTTP address. It shows the zones, the hot water, the boiler and
the devices. It sets temperatures, switches the home between home and away, and edits the weekly
schedules. It has English, Italian, German and French texts and a light and a dark theme.

- Container: open `http://<host>:8080/` and enter the API token once.
- Home Assistant add-on: select Open Web UI, or the entry freedo in the sidebar. No token is needed.

The dashboard monitors and controls. Installation management stays in the API. See
[`docs/OPERATION.md`](docs/OPERATION.md#dashboard) for the access rules and
[`dashboard/README.md`](dashboard/README.md) to build it.

## Features

The table compares the functions of the tado app with freedo. "equal" means the function exists in the
daemon and has automated tests. "partial" means a part is missing in the daemon or in Home Assistant.
"extra" means the tado app has no such function. "not implemented" means freedo has none. The notes name
what is not verified on air. The routes are in [`docs/API.md`](docs/API.md). The entities are in
[`custom_components/freedo/README.md`](custom_components/freedo/README.md).

| Function | Status | Notes |
|---|---|---|
| Manual change with the four end rules: manual, timer, until the next block, until the next automatic change | equal | `until_auto_change` writes `6240=1`, captured for rooms. No capture holds the end of that overlay. For hot water the value is not verified. |
| Switch a zone off | equal | The form "off until the next block" is captured. The manual form is not. |
| Resume the program, for a zone or for the whole home | equal | |
| Boost for a zone or for all zones | equal | Home Assistant sends one command for each zone. |
| Weekly schedule and copy | equal | Several day programs in one document, and the schedule fetch after an edit, are not verified on air. |
| Home and away | equal | The write shapes come from a capture. The transmission is not verified on air. |
| Away temperature for each zone | equal | 5 C means off in away, as in the cloud. Hot water has no field. Not verified on air. |
| Preheat before arrival | partial | A blueprint ends away when a person enters an approach zone. One distance for the whole home. No travel time. |
| Early start | equal | The daemon learns the heating rate of each zone. No firmware does this. Not verified on air. |
| Geofencing | partial | A blueprint uses the presence of Home Assistant for the whole home. The daemon has no geofencing code. |
| Open window detection | partial | A reset ends only a window that Home Assistant set. A window that a valve detected lasts until its time ends. |
| Hot water: on, temperature, program | equal | The daemon keeps the program and writes the thermostat at each block change. |
| Child lock | equal | Valves only. |
| Identify a device | equal | Valves in a zone only. |
| Display orientation and change notice | equal | A full `d/config` write homes the valve motor, as with the commercial bridge. |
| Valve calibration | equal | The command comes from firmware and ran on one valve. A `2.xx` answer does not guarantee the stroke. |
| Temperature correction | equal | Three modes. `config` homes the motor. `dbg2` ran on one valve and is in no capture. |
| Room temperature from an outside sensor | extra | Only in mode `setpoint`. Filter and limits are design estimates. Not verified on air. |
| Manage rooms and devices | partial | Through the API. Home Assistant creates entities at integration start, so reload it after a change. |
| Interface | partial | A web dashboard on the daemon monitors and controls the zones, the hot water, the boiler and the devices. It has no installation management yet: use the API. Not tested on a real plant. |
| Pair a device | partial | The pair window with firmware v2.9 is not verified on air. The integration opens the window and does not send the pair command. |
| Remove a device | partial | The API reports the effects first. The delete sends no frame. The device leaves the network only after `factoryreset`. |
| Firmware version and update | partial | A transfer route exists. No block was sent on air. freedo distributes no image. |
| Flow limit | equal | The fixed limit is verified on air through the OpenTherm reading. |
| Weather curve and automatic flow adaptation | partial | The curve has two points. The adaptation is a local controller and not the cloud algorithm. It is off by default. Not verified on air. |
| Default weather curve for the flow limit | partial | A built-in curve under `flow_limit_c`, fitted on one plant and one season of monthly means. It is not the cloud algorithm. Home Assistant must feed the outdoor temperature. It is off by default. Not verified on air. |
| Boiler telemetry and faults | partial | Boiler fault codes are in the API only. |
| Energy and gas estimate | partial | Estimated from modulation and name-plate data. Not a measurement. Price and cost stay in the Home Assistant Energy panel. |
| History and reports | partial | Home Assistant records the history. The daemon keeps no time series. |
| Notifications | partial | Binary sensors raise the alerts. Push notifications are Home Assistant automations. |
| Several users | not implemented | One bearer token. |
| HomeKit | not implemented | |
| Duration of a change made on a valve | equal | The rule is applied by the daemon. No capture holds the thermostat buttons. |
| Offline schedule on the devices | extra | Devices run the program alone after 25 minutes without a coordinator. The 25 minute test on a real plant is open. |
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
