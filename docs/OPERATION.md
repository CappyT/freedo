# Operation guide

How to configure, start and calibrate the freedo daemon, and how to pair a device. For the
HTTP API see [API.md](API.md). For the install paths see the [README](../README.md).

## Start modes

The daemon `freedo` has no subcommands. It starts in dry-run: it listens, decides and
sends nothing.

| Flag | Effect |
|---|---|
| `--port <device>` | Use a live radio (the serial port of the bridge). Without it there is no radio. |
| `--replay <file>` | Feed a recorded capture instead of a radio. The API keeps serving after the replay ends. |
| `--tx` | Transmit the frames the daemon decides. Without it the daemon sends nothing. |
| `--config <file>` | Name the bootstrap file. |
| `--state <file>` | Name the database. |
| `--addr`, `--api-token-file`, `--health-public`, `--insecure-no-auth` | See [API.md](API.md#address-and-authentication). |
| `--beacon <duration>` | Longest interval of the Router Advertisement. Default 10 minutes. `0` disables it. |
| `--debug-api` | Enable the debug routes. |
| `--version` | Print the build stamp. |

A flag on the command line wins over the bootstrap file. Run `freedo -h` for the full list.

### Router advertisement

The daemon broadcasts a Router Advertisement at random intervals between a third of
`--beacon` and the full value (200 to 600 s by default), as the commercial coordinator does.
A device that gets none stops to report after about 20 minutes. The daemon answers a Router
Solicitation at once, unless the last burst ended less than 3 seconds before. It answers an
Echo Request with an Echo Reply and a Neighbor Solicitation for its address with a unicast
Neighbor Advertisement. The advertisements need `--tx`. In dry-run the daemon logs the
solicitation in the unhandled log.

The advertisement carries an IPv4 gateway and an IPv4 DNS address. The installation options
`router_gateway` and `router_dns` set them. Both default to `0.0.0.0`. Write them with
`PATCH /api/install` (see [API.md](API.md#installation-options)). Not verified on a device.

## Bootstrap file

The bootstrap file holds process preferences only. Copy `freedo.example.yaml` to
`freedo.yaml` and edit the keys you need. The daemon starts without the file, because every
key has a default. A file that `--config` names and that is missing stops the start.

The file declares `version: 2` and these keys:

| Key | Default | Meaning |
|---|---|---|
| `serial_port` | empty | Serial port of the bridge. Empty means no radio. |
| `http_listen` | `127.0.0.1:8080` | Listen address of the API. |
| `api_token_file` | empty | File with the bearer token. |
| `database` | `freedo-state.db` beside the file | Database of the installation and the desired state. |
| `unhandled_log` | `freedo-unhandled.log` | File of the unhandled log. |
| `debug_api` | `false` | Debug routes. |
| `csl_phase` | `0` | CSL phase in the Enhanced ACKs. |
| `tx_counter` | `0` | Floor of the downlink frame counter. The daemon resumes from a higher stored value. |
| `zone_get_proxy` | `false` | Forward a zone GET to the member heard most recently. |
| `zone_proxy_timeout_sec` | `10` | Wait for the member answer. |
| `tune_debug` | `false` | Log one line for each frame with a frequency estimate. |
| `beacon_sec` | `600` | Longest Router Advertisement interval. `0` disables it. |

A relative path resolves against the directory of the file. The daemon accepts and ignores
the key `radio_tune`, with a warning. A file with version 1, or with a key of the
installation, is refused with the list of moved keys. The daemon migrates nothing by itself.

### Installation and database

The installation lives in the SQLite database: network key, coordinator identity, devices,
zones, circuits and the UTC offset. The database holds the 16-byte network key. The daemon
sets the permissions of the database and of the `-wal` and `-shm` files to `0600` and warns
when it cannot. Keep these files out of version control and out of backups you share.

On the first start with an empty database, the daemon creates the network key, the
coordinator EUI and a home id, and waits for commissioning. The start banner reports an empty
installation and never prints the key. Manage the installation with the routes under
`/api/install`.

The tools that read the installation read it from the database, not from the bootstrap file.
The tools never create an identity. Only the daemon does.

## Container

The image `ghcr.io/cappyt/freedo` exists for `linux/amd64` and `linux/arm64`, with the
version tag (for example `0.2.0`) and `latest`. The `Dockerfile` builds a static binary into an
Alpine image. The process runs as user 10001. The image holds the daemon only. The second target `addon` builds the Home Assistant add-on image. Its start
command reads the add-on options, so use it only inside Home Assistant.

```sh
docker pull ghcr.io/cappyt/freedo:latest
docker build -t freedo:local .
```

- The working directory is the volume `/data`. It holds `freedo.yaml` (optional),
  `freedo-state.db` and the unhandled log.
- The default command is `--addr :8080`, so the container needs a token: `--api-token-file` or
  `FREEDO_API_TOKEN`. A flag passed to the container wins over `freedo.yaml`.
- Map the serial device with `--device` and pass `--port`. The container user must belong to the
  group that owns the device on the host (`group_add` with the numeric gid).
- The container time zone is UTC. Set `TZ` to the IANA zone of the plant, or set the
  installation option `timezone`. See [Time zone](#time-zone). The binary embeds the zone
  database, so the image needs no `tzdata`.
- `docker-compose.example.yml` shows a complete start: token from a Docker secret, serial
  device, volume and a health check with `--health-public`. The secret file must be readable by
  user 10001.

```sh
cp docker-compose.example.yml docker-compose.yml
openssl rand -hex 32 > freedo_api_token
docker compose up -d
```

The container starts in dry-run. To use an image built locally, replace the `image:` line with
`build: .` and run `docker compose up -d --build`.

## Time zone

The devices keep no time zone. The daemon serves them a UTC offset (tag `02b2`) and runs the
weekly programs, the early start and `until_next` on the same clock. The offset comes from the
first of these sources that is set:

1. `utc_offset_min` of the installation: a fixed offset in minutes east of UTC.
2. `timezone` of the installation: an IANA name such as `Europe/Rome`. The offset follows the
   clock changes of the zone.
3. The zone of the host. The daemon takes it from the variable `TZ`, else from the system.

Write the first two with `PATCH /api/install`:

```sh
curl -X PATCH -H "Authorization: Bearer $(cat freedo_api_token)" \
  -H "Content-Type: application/json" -d '{"timezone": "Europe/Rome"}' \
  http://127.0.0.1:8080/api/install
```

`GET /api/health` shows the source in `timezone`: `name`, `offsetMin` and `fallback`.
`fallback` is `true` when the daemon uses the zone of the host. A container has no system
zone, so its host zone is UTC while `TZ` is unset. The daemon then serves an offset of 0 and
logs a warning at start: `options.timezone is unset and the host zone is UTC`. To end the
warning, set `TZ`, set `timezone`, or set `utc_offset_min`. A plant that runs on UTC sets
`timezone` to `UTC`.

## First radio session

Do these steps in order. Stop at the first step that fails.

1. Run `go test ./...`.
2. Replay a fixture: `freedo --replay <file>`. The daemon sends nothing in a replay.
3. Start the radio in receive only and check the serial port, the frequency, the identity and
   the counters.
4. Start the daemon in dry-run. Check that it knows the expected nodes.
5. Switch the commercial coordinator off. Add `--tx`. Run one reversible command on one zone,
   at a safe temperature and for a short time. Check the reception, the answer, the
   correlation in `GET /api/queue/{id}` and the persistence.
6. Return the plant to its normal state before you end the session.

The session is good when: the receive loop never stops, no answer is duplicated, the counters
only increase after a restart, the API state agrees with the received messages, and the CSL
queue stays bounded and can be emptied.

## Radio calibration

The receiver needs a frequency calibration once for each chip or installation. `GET /api/health`
reports `calibrated`. While it is `false`, the daemon runs on the default register value, and
`warning` names the calibration command. The daemon writes the calibrated register once at start
and never while it runs.

The calibration runs in receive only, with the daemon stopped. The tool is a developer tool: see
[CONTRIBUTING.md](../CONTRIBUTING.md#radio-calibration). A passive measurement is enough, because
the hardware frequency offset compensation absorbs up to 21 units of residual offset. The
measurements are in [PROTOCOL.md](PROTOCOL.md#freqest-and-autotune).

## Pairing a device

Pairing moves a device from the network of the commercial coordinator to the network of
freedo. The network key travels in clear text inside the `d/pair` frame. A pairing key protects
the frame, and every valve shares that key. Anyone who listens to the band at that moment can
read the network key. Keep the window short and the band quiet: no other coordinator on, no
test in parallel.

This procedure has not been verified on air with the current bridge firmware.

1. Switch the commercial bridge off.
2. Start freedo with `--tx` and the beacon on (`--beacon 10m`, the default), on bridge firmware
   v2.9 or later.
3. Make the device leave its previous network: `POST /api/nodes/{id}/factoryreset` with
   `{"confirm": true}`. The command sends network command 1007. The device leaves the network
   and keeps its token.
4. Open the pair window: `POST /api/pair/window` with `{"seconds": 120}`. The maximum is
   600 seconds. Check that the answer has `"responder": true`. The bridge then answers the
   Enhanced Beacon Requests by itself, and each Router Advertisement goes out twice. With
   `false` the firmware is older than v2.9. At the close, the log reports how many Enhanced
   Beacons the bridge sent.
5. Hold the button of the device to open its commissioning window. It lasts two minutes.
6. Check with `GET /api/pair/window` that the device appears in `joiners`. freedo registers a
   device when it receives an encrypted frame under the pairing key from it: an Enhanced
   Beacon Request or a Router Solicitation.
7. Call `POST /api/pair` with `{}` inside both windows. freedo picks the single device in
   `joiners`. With several devices, or without an open window, pass `{"target": "<short>"}`.

Without an open window, `d/pair` always uses the pairing key. With an open window it uses the
network key only for a device that the window heard under the network key after its last
pairing-key frame. That device is already in the network and answers `2.03`. freedo picks the
key for each attempt when it builds the frame.

`d/pair` goes ahead of every queued downlink for the device, as in the commercial coordinator.
An attempt that the radio is sending finishes first. A command that waits for its CoAP answer
goes behind `d/pair` and restarts with the same message id. Each `d/pair` attempt can use the
long wake-up train, because a rebooted device samples at a new phase. The bridge firmware tries
a fresh anchor first and uses the long train after one miss.

The device answers `2.04` and continues by itself: it sends a Router Solicitation, takes the
Router Advertisement, authenticates with `auth/token`, reads the time and the configuration
and starts to report. Then set a safe first setpoint, for example `POST /api/nodes/{id}/setpoint`
with `{"tempC": 18, "mode": "manual"}`.
