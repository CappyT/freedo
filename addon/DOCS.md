# freedo

freedo is a local coordinator for tado 868 MHz heating devices. This add-on runs the freedo
daemon. Home Assistant calls an add-on an "app" in recent releases.

The project is experimental. Read [Status](https://github.com/CappyT/freedo#status) and
[Scope and limits](https://github.com/CappyT/freedo#scope-and-limits) in the project README
before you use it.

## Requirements

- An ESP32 with a CC1101 radio, flashed with the bridge firmware of the freedo project, on a USB
  port of the Home Assistant host.
- One coordinator on the radio network. Switch the commercial tado bridge off before you turn on
  Transmit.

## Installation

1. Open the add-on store, then the menu, then Repositories. Enter
   `https://github.com/CappyT/freedo`.
2. Install freedo and open its Configuration tab.
3. Select the serial port of the radio bridge and save.
4. Start the add-on. Leave Protection mode on.
5. Install the freedo integration. HACS installs it from the same repository. Home Assistant then
   lists freedo as discovered: select it and confirm.

## Options

| Option | Default | Meaning |
|---|---|---|
| `serial_port` | empty | The serial port of the radio bridge. Empty means no radio: the daemon only serves its API. |
| `api_token` | empty | The bearer token of the API. Empty means the add-on creates one on the first start. |
| `transmit` | off | Send frames on air. Off is dry-run: the daemon listens and decides, and sends nothing. |

The add-on starts in dry-run. Turn Transmit on only after the
[first radio session](https://github.com/CappyT/freedo/blob/main/docs/OPERATION.md#first-radio-session)
checks.

## API and token

The HTTP API listens on port 18080. The Network tab changes the host port. Clear the host port to
keep the API inside Home Assistant: the integration reaches the add-on by its host name and does
not need the host port. Every request needs `Authorization: Bearer <token>`.

With an empty `api_token`, the add-on creates a random token on the first start and keeps it in its
data folder. The add-on gives that token to Home Assistant through discovery, so the integration
needs no address and no token. To call the API from another client, set `api_token` to a value you
choose.

## Installation data and network key

The add-on has no option for the network key, the devices or the zones. They live in the daemon
database. On the first start, the daemon creates a network key and a coordinator identity. You
manage the installation through the API under `/api/install`. The
[API reference](https://github.com/CappyT/freedo/blob/main/docs/API.md) describes it.

## Data and backups

The add-on keeps everything under its data folder: the database `freedo-state.db`, the generated
`api_token` and the log `freedo-unhandled.log`. Home Assistant backups include this folder. The
database holds the network key, so protect the backups.

The backup is cold: Home Assistant stops the add-on, copies the folder and starts the add-on again.
A cold backup keeps the SQLite files consistent.

The log `freedo-unhandled.log` grows without a limit. Delete it from time to time. It can hold
device labels and serial numbers: remove them before you share it.

## Behavior

- The add-on logs to the add-on log.
- Stop and restart send SIGTERM. The daemon saves its state and exits in less than 10 seconds.
- After a radio error the daemon exits. Turn on the Watchdog switch of the add-on so that Home
  Assistant starts it again.
- The daemon follows the time zone of its container (variable `TZ`) for the device clock and the
  program times. The Supervisor sets `TZ` from the Home Assistant time zone. The add-on has no
  time zone option. Check that `timezone.fallback` in `GET /api/health` is `false`. If it is
  `true`, the daemon logs a warning at start. Set the zone with `PATCH /api/install` and
  `{"timezone": "<IANA name>"}`, for example `Europe/Rome`. The
  [operation guide](https://github.com/CappyT/freedo/blob/main/docs/OPERATION.md#time-zone) has the
  command.

## Support

Report issues at https://github.com/CappyT/freedo/issues.
