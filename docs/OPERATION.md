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

A restart continues the interval of the last burst. The daemon stores the instant of each
burst and the interval it drew, and sends no burst at start until that interval ends. A daemon
that restarts in a loop sends one burst for each interval, as the commercial coordinator does,
and not one at every start. A start after a longer stop sends the burst at once.

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

### Schema upgrade and rollback

The daemon migrates the database forward at start, and only forward. An older daemon refuses
a database of a newer schema and stops with `schema version "8" is unsupported`. Schema 8
adds the [program presets](API.md#program-presets).

To go back to a daemon before schema 8, stop the daemon and restore a copy of the database
that you made before the upgrade. Copy `freedo-state.db` while the daemon is stopped, before you
install a newer version. In Home Assistant, the pre-releases run in the beta add-on on a copy of
the database, and the database of the stable add-on keeps its schema: see
[addon-beta/DOCS.md](../addon-beta/DOCS.md).

The restored copy holds the state at the time of the copy. You lose every change made after
it, and in particular:

- every preset other than `Default`, and the choice of the active preset;
- every away program, of the heating zones and of the hot water zone;
- every change of a home program, the hot water program included.

The restored copy does not tell the devices that their documents changed. A device keeps the
program it fetched last until a schedule write changes its document. Check the programs after
the restore, and write again each one that differs from the one you want.

Do not change `schema_version` by hand. An older daemon then ignores the presets and the
away programs, and it runs without the hot water program once schema 8 moved it into the
presets.

## Container

The image `ghcr.io/cappyt/freedo` exists for `linux/amd64` and `linux/arm64`, with the
version tag (for example `0.2.0`), `latest` for the newest stable release and `beta` for the
newest pre-release. The `Dockerfile` builds a static binary into an
Alpine image. The process runs as user 10001. The image holds the daemon with the [dashboard](#dashboard)
embedded. The second target `addon` builds the Home Assistant add-on image
`ghcr.io/cappyt/freedo-addon`, with the same tags. The stable and the beta add-on both run it: the
Supervisor pulls the tag that the `version` of the add-on names. Its start command reads the add-on
options, so use it only inside Home Assistant.

Every release also attaches the daemon binary of each platform, `freedo-linux-amd64` and
`freedo-linux-arm64`, taken from the image of the same release, with `SHA256SUMS`.

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

## Dashboard

The daemon serves a web interface at the root of its HTTP address. It shows the zones, the hot
water, the boiler and the devices. It sets temperatures with the four end rules, switches a zone off,
returns it to its program, switches the home between home and away, edits the weekly schedules of the
home and away programs, and chooses and manages the program presets. It
calls the routes of [API.md](API.md) and no other.
Settings shows the health summary of the daemon. It marks `serialErrors` above zero with a warning:
see [Serial link faults](#serial-link-faults).

The page Rooms and devices manages the installation. It lists the devices room by room, with the state
of each device, identify and the child lock. The devices in no room form a last group, No room, while
there is one. The page and the settings of a room do these tasks:

- Add a room, rename it, delete it. A delete leaves the devices of the room in the installation, in No
  room.
- Add a device: it opens the pair window for 600 seconds and shows the time left, lists the devices the
  window hears, declares the chosen one with a type, a name and a room, and sends the pair command. It then
  shows the pairing as in progress, and as done only when the daemon served the device a session or learned
  its serial number. Send again repeats the pair command while the window is open: a device can miss the
  first one. The page closes the window when the device joined. See [Pairing a device](#pairing-a-device).
- Add a heating circuit, change its heat source, delete it.
- In the settings of a room, for each device of the room: rename it, move it to another room or to No
  room, and remove it from the installation. The settings of No room do the same for the devices in no
  room.
- A removal shows the effects that the daemon reports and asks before the delete. A device that is the
  heat source of a circuit cannot be removed. The delete sends no frame, and the device keeps talking on
  the network.
- In the settings of a room: choose the circuit, the measuring device and the demand reporter, and add a
  device that the installation already holds. A room takes a device that is in no room. The wall
  thermostat is the exception: it can be in the hot water room and in one heated room.
- In the settings of a heated room, under Room options: the open window detection and its time, the
  away temperature, the end rule of a change made on a device, the early start, the offset mode, the
  offset and the valve calibration. One save sends the changed options in one
  `PATCH /api/install/zones/{n}`, and a typed offset in one `POST /api/zones/{n}/offset`. The page keeps
  the ranges of the daemon. In the offset mode Valve configuration each offset write, by hand or by the
  valve calibration, makes the valve do one full motor stroke; the page says so at that field. The
  valve holds the offset in the modes Valve configuration and Valve debug record and does not report
  it, so the page shows the offset only in the mode Setpoint shift. The hot water room has none of
  these options.

The page Boiler shows the fault codes that the boiler reports, as numbers, and every page shows them in
a banner while there is one. A code is a number of the boiler maker: freedo gives it no meaning.

The dashboard chooses the number of a new room: 0 for hot water, else the lowest free number from 1. It
declares a device without a serial number, and the daemon takes the serial from the first request of the
device. The controller gains of a room and every installation option other than the time zone stay in
the API: see [Installation options](API.md#installation-options) and [Zone options](API.md#zone-options).

The page First setup takes a new daemon to a working plant. Rooms and devices and Settings link to it, and the home
page does while the installation has no room. It shows six steps with their live state:

| Step | Where | Done when |
|---|---|---|
| Radio | Outside: connect the bridge, pass `--port`, [calibrate](#radio-calibration) | `mode` is `live` and `calibrated` is not `false` |
| Time zone | On the page: it writes `timezone` | `timezone.fallback` is `false` |
| Rooms | On the page | The installation has a heating zone |
| Transmit | Outside: switch the commercial bridge off, pass `--tx` | `transmitAllowed` is `true` and `bridgePresent` is `false` |
| Devices | On the page: the same flow as Add a device | The installation has a device |
| Heating circuits | On the page | Every heating zone has a circuit |

A `go build` on a machine without Node has no dashboard. The daemon then answers every page request
with `404`, and the API works as before. To build the dashboard, see
[`dashboard/README.md`](../dashboard/README.md). The container images include it.

### Open it

- Container: open `http://<host>:8080/`. The page asks for the API token once. Enter the content of
  `freedo_api_token`. The browser keeps the token in its storage, and the page sends it as the bearer
  token. A wrong token shows a message and the page asks again. Settings removes the stored token.
- Home Assistant add-on: select Open Web UI on the add-on page, or the entry freedo in the sidebar. The
  page asks for no token.

The page works under any path prefix, as long as the proxy removes the prefix before it forwards the
request, as Home Assistant ingress does. The route is in the URL fragment (`#/zones/2`).

### Access

The files of the dashboard need no token, because they hold no data. Every path under `/api` keeps
its rules from [API.md](API.md#address-and-authentication).

Home Assistant ingress shows the page inside Home Assistant, under `/api/hassio_ingress/<token>/`.
Home Assistant authenticates the user. The Supervisor then forwards the request to the add-on from the
fixed address `172.30.32.2`. The developer documentation for apps tells an app to accept ingress
connections from that address only and to treat the user as authenticated. The daemon follows it:

- The flag `--ingress` makes the daemon accept the calls of the dashboard from the connection address
  `172.30.32.2` without a token. Only the 50 method and path pairs of
  [`internal/daemon/ingress_routes.json`](../internal/daemon/ingress_routes.json) qualify. They include
  the installation and pairing calls of the pages Rooms and devices and First setup, so those pages need no token
  under the add-on. Every other request from that address needs the token. [SECURITY.md](../SECURITY.md#home-assistant-ingress) lists
  the pairs and what a user can do with them.
- Only `addon/run.sh` sets the flag. The bootstrap file and the environment have no such setting, and
  the container image does not set it. The daemon ignores the flag and logs a warning unless the
  variable `SUPERVISOR_TOKEN` is set, which only the Supervisor does.
- Every other source still needs the token: other apps, the host, the LAN, and the published port 18080.
- The daemon compares the method and the exact path, and reads the address of the TCP connection. A
  header such as `X-Forwarded-For` or `X-Ingress-Path` has no effect.

The daemon cannot tell the Home Assistant users apart. Every user who can open the panel can use those
calls, and the project accepts this. Home Assistant lets every user open an ingress session, so the
add-on option `panel_admin` (default `true`) hides only the sidebar entry. Do not set `--ingress` in
another setup. A reverse proxy on the host shows the daemon its own address, not the address of the
client.

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

## Serial link faults

Symptom: the log shows lines like these, and `serialErrors` in `GET /api/health` grows.

```text
rx autoack: serial link fault: reply truncated: deadline exceeded after 89/113 bytes
CSL queue poll: serial link fault: reply truncated: deadline exceeded after 98/119 bytes
bridge: CC1101BRIDGE v2.9 at 921600 baud
WARNING: this firmware talks 921600 baud, which loses replies on some USB hosts
```

Before version 0.3.1 the daemon ended with `radio loop ended: live radio owner ended`
within minutes, and the supervisor restarted it.

Cause: the CH340 adapter of the ESP32 board lost bytes of replies of about 80 to 120 bytes at
921600 baud. The board has no flow control line, and the CH340 datasheet advises flow control
or a lower rate from 500 kbaud in both directions. It showed on a Raspberry Pi 5 with a
second full-speed USB device on the same bus, and not on an x86 host. The point of loss inside
the USB stack is not measured. Bridge firmware v2.10 and later talks 230400 baud.

Fix: flash firmware v2.10 or later, see the
[firmware README](../firmware/cc1101_bridge/README.md#flash-a-released-image). The daemon
tries 230400 and then 921600, so an old bridge keeps working until you flash it. The daemon
logs the banner and the rate at start. A bridge that answers at neither rate stops the start
with a message that names both rates.

What the daemon does with a lost reply:

- It discards the reply, restores the framing and goes on. It reads the next reply within
  about 0.3 s, and does not wait for the deadline.
- The frame in the lost reply is lost. A lost queue event is a completion or an uplink that the
  bridge already acknowledged. A lost completion makes the job fail at its deadline, and the
  daemon sends it again. A lost uplink is not sent again.
- It counts the loss in `serialErrors`.
- It ends the radio loop only when one kind of exchange, the queue poll or the receive window,
  failed every time for 30 s. A failed port ends the loop at once. The supervisor then
  restarts the daemon.

## Pairing a device

Pairing moves a device from the network of the commercial coordinator to the network of
freedo. The network key travels in clear text inside the `d/pair` frame. A pairing key protects
the frame, and every valve shares that key. Anyone who listens to the band at that moment can
read the network key. Keep the window short and the band quiet: no other coordinator on, no
test in parallel.

The steps below ran on air once, on one valve, through the API. The valve was in the installation
before: it was removed, then paired as a device that the installation did not hold. Not verified on air:
a network with no paired device, a wall thermostat, and the same flow from the dashboard.

The page Rooms and devices of the [dashboard](#dashboard) does steps 4 and 6 to 8. The steps below do the same
through the API.

1. Switch the commercial bridge off.
2. Start freedo with `--tx` and the beacon on (`--beacon 10m`, the default), on bridge firmware
   v2.9 or later.
3. A device that moves out of an installation of freedo: remove it from its zone and delete it from the
   installation. See [Installation writes](API.md#installation-writes).
4. Open the pair window: `POST /api/pair/window` with `{"seconds": 600}`, the maximum. Check that
   the answer has `"responder": true`. The bridge then answers the
   Enhanced Beacon Requests by itself, and each Router Advertisement goes out twice. With
   `false` the firmware is older than v2.9. At the close, the log reports how many Enhanced
   Beacons the bridge sent.
5. Hold the button of the device. The device leaves its previous network and opens its
   commissioning window, which lasts two minutes. freedo has no command that does this: see
   [Network commands and leaving the network](PROTOCOL.md#network-commands-and-leaving-the-network).
6. Check with `GET /api/pair/window` that the device appears in `joiners`. freedo registers a
   device when it receives an encrypted frame under the pairing key from it: an Enhanced
   Beacon Request or a Router Solicitation. In the test the valve appeared about one minute after the
   window opened.
7. Declare the device: `POST /api/install/devices` with the `eui` from `joiners`, the `kind`, a
   `label` and no `serial`. A body without `eui` gets `400`, so the declaration comes after step 6.
   Then add the device to its zone: `POST /api/install/zones/{number}/members`.
8. Call `POST /api/pair` with `{}` inside both windows. freedo picks the single device in
   `joiners`. With several devices, or without an open window, pass `{"target": "<short>"}`. When no
   `2.04` arrives, call it again while both windows are open. In the test the first call got no answer
   in four attempts, and a second call about 90 seconds later got `2.04` at the first attempt. The
   cause is not measured.

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
and starts to report. The daemon takes the serial of a device declared without one from the first
requests of the device. A valve that is not mounted on a radiator reports a fault until it is mounted.
Then set a safe first setpoint, for example `POST /api/nodes/{id}/setpoint`
with `{"tempC": 18, "mode": "manual"}`.
