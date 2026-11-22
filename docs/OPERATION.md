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
| `--lbt` | Listen before the first frame of a CSL job and defer the job while the channel is busy. Default off. Needs bridge firmware with `CMD_LBT`. See [Listen before talk](#listen-before-talk). |
| `--lbt-threshold <dBm>` | Level above which the channel counts as busy with `--lbt`, from -120 to -20. Default -80. |
| `--airtime-limit` | Alpha, not verified on a plant ([status](https://github.com/CappyT/freedo/issues/42)). Keep the hourly airtime under the ETSI budget, 36 s, or 100 s with `--lbt`. Default off. Needs bridge firmware that takes the budget. See [Airtime budget](#airtime-budget). |
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
install a newer version, or download a [backup](#backup) while it runs. An older daemon does not
import a backup of a newer schema. In Home Assistant, the pre-releases run in the beta add-on on
its own database, and the database of the stable add-on keeps its schema: see
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

The [statistics](API.md#statistics) file `freedo-state-history.db` has a schema of its own, and
the daemon migrates it forward too. A daemon before 0.12.0-beta.5 refuses a file of schema 2:
the statistics stay off with `schema "2", this daemon reads "1"` in `GET /api/history`, and
nothing else changes. Upgrade again, or delete the file to start the statistics over.

### Backup

The daemon exports its whole state as one file and imports such a file: see
[State export and import](API.md#state-export-and-import). The page Settings of the
[dashboard](#dashboard) has both actions in the section Backup. The file holds the network key.
Anyone who has it can decrypt the radio traffic and command the devices: keep it private.

An import keeps the replaced database in `backup/before-import-<UTC time>/` next to the database,
the three newest only, and restarts the daemon by itself. What restarts the daemon:

- Home Assistant add-on: the daemon replaces its own process, and the start script keeps waiting on
  the same process id. The add-on does not stop, and the Supervisor does nothing.
- Container image: the daemon is the first process of the container and replaces itself. The
  container does not stop, so no restart policy is needed.
- Plain binary: the same. Under a service manager the process id stays.

When the replacement of the process fails, the daemon exits with status 1 and the imported file
stays staged. The next start, by a restart policy or by hand, applies it.

With `--tx` on a live radio, the start waits until 250 seconds have passed since the import. The log
says how long. During that time the radio is closed and the daemon serves the dashboard,
`GET /api/health` with `importWait`, and `503` with `Retry-After` on every other route: see
[API.md](API.md#state-export-and-import). The time counts from the import, so a restart in the wait does not shorten
it. A container health check on `GET /api/health` passes during the wait. To go back to the replaced state, stop
the daemon and copy the files of the newest `before-import` folder over the database.

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
Settings shows the health summary of the daemon: radio mode, transmit, calibration and frames, with
every other counter behind All counters. It marks `serialErrors` above zero with a warning: see
[Serial link faults](#serial-link-faults). The foot of the page names the version of the daemon.

Settings also has the section Backup. Download backup saves the [state export](#backup) as a file,
and the section warns that the file holds the network key. Restore backup opens the file picker,
asks first with the list of what the chosen backup replaces, and sends it. The page then shows that freedo restarts
and reconnects by itself: a missing answer during the restart is not shown as a fault. While a
transmitting daemon waits its 250 seconds, every page shows one banner with the seconds that are
left, keeps its last data and takes no command. A page that opens during the wait shows the banner
alone. Under the Home Assistant add-on both actions
work for an administrator with no token, and another user reads in the section that only a Home
Assistant administrator can download or restore a backup. Outside the add-on both actions need the
API token of the page. A refusal stays in the section, and every other page keeps working.

The page Rooms and devices manages the installation. It lists the devices room by room, one line per
device with its state and serial number. A tap opens the line in place: the type, the firmware, the
last contact, the battery, the child lock, Identify and a link to the room settings. The devices in no
room form the last group, No room. The page and the settings of a room do these tasks:

- Add a room, rename it, delete it. A delete leaves the devices of the room in the installation, in No
  room.
- Add a device: it opens the pair window for 600 seconds and shows the time left, lists the devices the
  window hears, declares the chosen one with a type, a name and a room, and sends the pair command. The
  room has no default: choose a room, or No room. The page puts the device in the room before it sends the
  pair command, so a refused room stops the run. A declared device that is in no room gets the room field
  again. The page then shows the pairing as in progress, and as done only when the daemon read a frame of
  the device under the network key after the command: the frame counter of the device moved, or the daemon
  learned its serial number. Send again repeats the pair command while the window is open: a device can
  miss the first one. The page closes the window when the device joined. It then shows only the name of
  the paired device and the room that the daemon reports, with Done and Add another device. When the
  daemon does not hold the device in the chosen room, the page says so and offers to put it there. The
  list marks a heard device as paired when the daemon heard it after its last request to join and a
  request of it carried the session, and does not offer it again. See
  [Pairing a device](#pairing-a-device).
- Add a heating circuit, change its heat source, delete it.
- In the settings of a room, for each device of the room: rename it, move it to another room or to No
  room, restart it, and remove it from the installation. The settings of No room do the same for the
  devices in no room.
- A restart asks first. It queues `POST /api/nodes/{id}/reboot` for a valve or the wall thermostat, and
  the page shows the messages that wait for the device. The device restarts about 5 seconds after it
  receives the command, does not answer for about a minute, and joins again with its stored key. The
  daemon needs a zone id for the command: a device in no room that never named one gets `id required`.
- A removal shows the effects that the daemon reports and asks before the delete. A device that is the
  heat source of a circuit cannot be removed. The delete sends no frame, and the device keeps talking on
  the network.
- In the settings of a room: choose the circuit and the demand reporter, and add a device that the
  installation already holds. A room takes a device that is in no room. The wall
  thermostat is the exception: it can be in the hot water room and in one heated room.
- In the settings of a heated room, under Room temperature: one list chooses who measures the room,
  a device of the room or the Home Assistant thermometer. A device becomes the measuring device and
  the device owns the zone. The thermometer gives the zone to the coordinator, which regulates on the
  reading and falls back to the measuring device while the reading is stale or missing. The list
  saves at once. The note under the list gives the last reading and its age. The closed section
  Advanced under the list holds the offset and the offset mode.
- In the settings of a heated room, under Room options: the open window detection and its time, the
  away temperature, the end rule of a change made on a device and the early start. One save sends the changed options in one
  `PATCH /api/install/zones/{n}`, and a typed offset in one `POST /api/zones/{n}/offset`. The page keeps
  the ranges of the daemon. In the offset mode Valve configuration each offset write makes the valve
  do one full motor stroke; the page says so at that field. The valve holds the offset and does not
  report it, so the page does not show it. The hot water room has none of these options.

The page Boiler shows the fault codes that the boiler reports, as numbers, and every page shows them in
a banner while there is one. A code is a number of the boiler maker: freedo gives it no meaning.

The page Boiler also takes the gas meter readings: Add a reading sends the value and the time it was
read, with an option for a new or replaced meter. The page lists the stored readings with what the
meter and the estimate counted between two of them, shows the learned factor, and removes the last
reading after a confirm. See [Gas meter](API.md#gas-meter).

The page Boiler setup, from the button at the top of the Boiler page, writes the boiler options in one
`PUT /api/install/boiler`: the fuel, the output and the fuel flow at full and at no modulation, the
highest flow temperature, the default weather curve, a curve per circuit, the adaptation and the
timing of the flow limit. Each section says where the manual of the boiler lists the values. The page
draws the curve that the daemon serves, with the outdoor temperature of now.

A room screen judges the air comfort from the temperature and the humidity of the room. The ranges are
the heating-season values of EN 16798-1 category II: 20 to 24 C and 30 to 60 % humidity. From 70 % the
page warns of a mould risk.

The [statistics](API.md#statistics) are off by default. Settings turns them on in the section
Statistics, which says what they store, since when and how much space they take. Off asks first and
keeps the data; a separate row then deletes it after a confirm. With the statistics on:

- A room screen links to the report of the room. The report shows one local day: the heating time,
  the average, lowest and highest temperature, the average humidity and the time with the window open
  or away; a chart of the temperature over the target, the heating demand and the humidity; a table of
  the values every hour; and the heating periods of the day. A period is a stretch with heat demand of
  15 minutes at least, also with a target that never moves. The return home or a target rise, also a
  rise of the away temperature, starts a new period and names it. A heat-up of the program shows in
  its period with the time to reach the target. Arrows move to
  the day before or after, back to the first day with a sample.
- The page Consumption, from the Boiler page and from Settings, shows the fuel, the cost, the heat and
  the burner time per day of a month and per month over 13 months, with a table of the same values. It
  writes the price of one fuel unit and the currency.

The statistics live in `freedo-state-history.db` beside the state database. A backup does not include
them, and a restore keeps them. To remove them by hand, stop the daemon and delete the file and its
`-wal` and `-shm` files.

The dashboard chooses the number of a new room: 0 for hot water, else the lowest free number from 1. It
declares a device without a serial number, and the daemon takes the serial from the first request of the
device. The controller gains of a room and every installation option other than the time zone and the
boiler options stay in the API: see [Installation options](API.md#installation-options) and
[Zone options](API.md#zone-options).

The page First setup takes a new daemon to a working plant. Rooms and devices and Settings link to it, and the home
page does while the installation has no room. It shows six steps with their live state:

| Step | Where | Done when |
|---|---|---|
| Radio | Outside: connect the bridge, pass `--port`. The radio [calibrates itself](#radio-calibration) | `mode` is `live`. `calibration` shows the state of the receiver calibration |
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
  `172.30.32.2` without a token. Only the 59 method and path pairs of
  [`internal/daemon/ingress_routes.json`](../internal/daemon/ingress_routes.json) qualify. They include
  the installation and pairing calls of the pages Rooms and devices and First setup, so those pages need no token
  under the add-on. Every other request from that address needs the token. [SECURITY.md](../SECURITY.md#home-assistant-ingress) lists
  the pairs and what a user can do with them.
- The export and the import of the state and `POST /api/radio/calibrate` are not among them. They
  are the 3 pairs of
  [`internal/daemon/ingress_admin_routes.json`](../internal/daemon/ingress_admin_routes.json): from the
  gateway address the daemon serves them without a token only to a Home Assistant administrator. It
  reads the user from `X-Remote-User-Id`, which the Supervisor sets, and asks Home Assistant through
  `ws://supervisor/core/websocket` with the command `config/auth/list`. The add-ons declare
  `homeassistant_api: true` for that. No answer means no access.
- Only `addon/run.sh` sets the flag. The bootstrap file and the environment have no such setting, and
  the container image does not set it. The daemon ignores the flag and logs a warning unless the
  variable `SUPERVISOR_TOKEN` is set, which only the Supervisor does.
- Every other source still needs the token: other apps, the host, the LAN, and the published port 18080.
- The daemon compares the method and the exact path, and reads the address of the TCP connection. A
  header such as `X-Forwarded-For` or `X-Ingress-Path` has no effect. `X-Remote-User-Id` counts for the
  2 backup pairs only, and only on a connection from the gateway address.

For the 59 pairs the daemon does not tell the Home Assistant users apart. Every user who can
open the panel can use those calls, and the project accepts this. Home Assistant lets every user open an ingress session, so the
add-on option `panel_admin` (default `true`) hides only the sidebar entry. The Supervisor adds the
headers `X-Remote-User-Id`, `X-Remote-User-Name` and `X-Remote-User-Display-Name`, and none of them
says whether the user is an administrator. Do not set `--ingress` in
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

The receiver needs a frequency calibration (register FSCTRL0) once for each chip. The receiver
calibrates itself: you do not need a shell or a stopped daemon.

Without a stored calibration, the daemon starts the receiver on FSCTRL0 0x34, the register this
radio module needs. The chip configuration loads 0x34 too, so the developer tools start on it. The hardware frequency offset compensation absorbs up to 21 units of residual
offset, so the receiver hears the devices before the calibration. Nothing blocks pairing or
transmit while `calibrated` is `false`, with one exception: while the radio holds a register other
than the home value, a scan step or a candidate, it defers the Router Advertisement bursts and the
CSL bootstrap trains. An empty plant, or a new one, stays on 0x34 until a device is paired.

When the database holds no calibration, the receiver calibrates itself on the frames of the paired
devices. It reads the frequency error of each uplink that it receives in a plain receive window.
A pairing request gives no reading, and the radio ignores the frames that answer its own
transmission, because their reading is not a measurement. On an empty plant the readings therefore
start when the first device is paired and sends its own uplinks. After 20 readings the radio
computes the correction. A correction inside +-8 units, half the compensation margin, ends the
measurement on the current register. Outside that band the radio moves the register by the
correction and measures one more round, with at most three writes. A round that ends on a write
or on a clipped reading is never a result: only a round that finds the register inside the band
ends the measurement, and only then does the daemon store it. When the round after the third
write is still outside the band, `calibration.state` is `unsettled`, the radio keeps the register
and the daemon stores nothing. The band is wide because the measurement on air reads a few units off the bench
value: see [PROTOCOL.md](PROTOCOL.md#freqest-and-autotune). The log shows one line for each register
write, `receiver calibration: round 1: correction -13 on 20 frames of 4 sources, FSCTRL0 0x34 ->
0x27`, and one line for the result: `receiver calibration: FSCTRL0 0x27 measured on 20 frames of 4
sources, stored`. At the next start the daemon writes the stored register once and the radio
measures nothing. A session that hears too few frames stays in `measuring`, and the next start
measures again.

A receiver that is far from the carrier decodes no frame, so no reading can move its register.
The register scans in this case only. The scan starts when the installation has at least one
device and no frame from a device of the installation arrived for 10 minutes. A frame of another
network, or of the commercial coordinator, does not count. The radio steps the register by 30
units around its home value, alternately up and down across the range of the register, 3 minutes
for each step. The home value is the stored calibration, or 0x34 when none is stored. The first
step that decodes a frame of a device ends the scan and becomes the candidate: the register stays
on it, and the normal rounds measure from there. The log shows `receiver calibration: scan step 1
of 8`.

The daemon keeps and stores the candidate only when a round settles inside +-8 units. When the
rounds end outside the band, or no round settles within 30 minutes, the radio writes the home value
back and holds the scan for 1 hour, as after a pass that decoded nothing. The Router Advertisement
bursts and the CSL bootstrap trains go out on the home value during the hold. A write that the link
loses leaves the register unknown: the radio tries the home value again every 5 s until it works,
without comparing it to the value that the host last wrote, and a scan step whose write failed ends
the pass. The write of a correction in a measurement round is the same: the radio writes that
correction again every 5 s, collects nothing until it works, and the daemon never stores a register
that the chip may not hold. While a register is unknown, `radioWarnings` has a line that says so and
the long transmissions wait. `calibration.candidate` in
`GET /api/health` is `true` while a candidate runs, and `radioWarnings` has a line that names the
candidate register, the home value and the deadline. A plant with sparse uplinks can need more
than 30 minutes for the 20 readings of a round. It then goes back home and scans again after
the hold.

A pass that decodes no frame from a device restores the home value and waits 1 hour before the
next pass. Opening a pair window stops the scan too, restores the home value before the window
opens when no CSL job is out, and holds the scan back until 10 minutes after the window. A pair window ends a candidate too. The transmit carrier
moves with the receive register. While a scan step, a candidate, a restore that did not work yet or
the reset write of `POST /api/radio/calibrate` that waits for the loop holds another register than
the home value, the daemon defers the Router Advertisement bursts and
the CSL bootstrap trains. They go out again when the
register is the home value, or when the daemon stores the candidate. `radioWarnings` in
`GET /api/health` has a line while the radio scans, while it measures a candidate and after a pass
that decoded nothing: check the antenna, the module and that a device transmits.

After a calibration, the radio keeps the readings of the frames it hears. When enough of them
arrive, `calibration.residual` in `GET /api/health` is the offset they still read. A residual
beyond +-8 units adds a line to `radioWarnings`: the module drifted. Press Calibrate again.

The radio also runs the calibration of the frequency synthesizer (SCAL) once every hour, and
reads the lock back. A chip that does not lock holds every transmission: see
[Synthesizer lock](#synthesizer-lock).

Read the state in `GET /api/health`: `calibrated` and `calibration.state`, which is `stored`,
`measuring`, `measured` or `unsettled`. In `measuring`, `calibration.readings` of
`calibration.want` shows the progress. See [Receiver calibration](API.md#receiver-calibration). The radio step of the page First
setup shows the same count. The count stays at 0 until a device is paired and talks. If it stays at
0 after that, the bridge hears nothing: check the board and the antenna.

To measure again, for example after you change the bridge, press Calibrate again on the page First
setup, or call `POST /api/radio/calibrate`. Under the add-on, only a Home Assistant administrator
can do this without the token. The daemon forgets the stored value, the radio sets the register back
to 0x34, and then it measures. The log shows `receiver calibration: stored value forgotten, FSCTRL0
0x34 (default), measuring again`.

For a bench without a running daemon, the developer tool `freedo-dev autotune` measures and stores
the same value: see [CONTRIBUTING.md](../CONTRIBUTING.md#radio-calibration). The measurements are in
[PROTOCOL.md](PROTOCOL.md#freqest-and-autotune).

## Bridge firmware level

The daemon needs bridge firmware v2.11 or later. The firmware prints its level in the banner,
for example `CC1101BRIDGE v2.11 (0.4.0)`, and the daemon logs it at start.

Firmware v2.11 adds three things that the daemon relies on:

- Every serial request ends with a CRC-8 byte. The firmware drops a request with a wrong CRC and
  answers `ST_BAD_CRC`. The daemon treats it as a lost request and counts it in `serialErrors`.
- The firmware refuses every transmit command until the daemon sends `CMD_CONFIGURED`, which comes
  after the register load. A chip that the ESP32 or the CC1101 reset has no register set, and the
  firmware then answers `ST_UNCONFIGURED`. The daemon ends the radio loop at once, and the
  supervisor restarts it and configures the chip.
- The firmware counts the time on air of the last hour. It refuses a transmission above a budget
  only when the airtime limit is on, and it is off by default: see [Airtime budget](#airtime-budget).

The firmware also drops its coordinator, its pair window and its queue after 5 s with no request.
The idle loop sends a request at least every 1.25 s: a receive window of 250 ms waits for its reply
for 1.25 s at most, and so does every request with no window of its own. A lost reply stretches
the gap to the reply wait of that request, then 100 ms of backoff follow. A queue poll waits 2 s.
PAIRWIN, a train, a Router Advertisement burst and the register check after an expired CSL job
wait 4.5 s: the loop can send them while a CSL job is out, and the firmware runs them when the CSL
transaction in progress has ended, up to 3.9 s. The idle register check, the idle register writes
and the synthesizer calibration (XFER and STROBE) keep the 1.25 s, because no transaction can hold
them back: two lost replies in a row stay under the 5 s of the firmware, which counts from the
arrival of its last request and the end of its last command.

A bridge below v2.11 does not start the radio. Before the daemon closes the port, it sends the
bridge a queue flush and a chip reset, so that a bridge that a session left armed stops its
transmitter. A firmware that predates the CRC byte reads those requests and ignores the last byte. The
daemon keeps running as with no bridge, in dry-run, and `GET /api/health` answers `503` with `radioError`, for example:

```text
bridge firmware v2.10 found, v2.11 or later required: flash firmware/cc1101_bridge, see docs/OPERATION.md (Bridge firmware level)
```

Fix: flash firmware v2.11 or later, see the
[firmware README](../firmware/cc1101_bridge/README.md#flash-a-released-image), and restart the
daemon. A bridge that answers at neither 230400 nor 921600 baud stops the start with a message
that names both rates.

A v2.11 build from before the per-minute airtime buckets reports the level v2.11, but it answers
`CMD_AIRTIME` with 8 bytes and refuses the load. The daemon then runs as with no bridge, like for an
old level, and `radioError` says that the firmware has the old AIRTIME command and that you flash
the firmware of the release of the daemon. The daemon does not try this bridge again.

The developer tools that transmit through raw SPI (the `freedo-dev` commands that call `Send`)
are not in the airtime count of the firmware. Do not use them for a long session.

## Airtime budget

The firmware adds up the time on air of every frame it sends, over a sliding hour. The count is
always on and `GET /api/health` reports it as `airtime`. By default nothing refuses or defers a
transmission for it: the firmware has no budget (`airtime.budgetMs` is 0) and the daemon holds
nothing back. A plant of 9 devices needs about 100 to 120 s an hour, and a cap would throttle it.

`--airtime-limit` (add-on option `airtime_limit`) turns the limit on. Alpha, not verified on a plant: [status and help wanted](https://github.com/CappyT/freedo/issues/42). It is off by
default. The budget follows the cumulative on-time limit of the 868.0 to 868.6 MHz band under polite
spectrum access (EN 300 220-2, clause 4.6, per 200 kHz channel): 36 s an hour (1 %) without
`--lbt` and 100 s an hour with it. The daemon sends the budget to the firmware in `Configure`,
with the airtime load, and at every start: the firmware has no budget after a reset. With the
limit on:

- The firmware refuses a transmission that starts above the budget with `ST_AIRTIME` (`1D`). A job
  that started sends all its frames, so one job can cross the budget. An Enhanced ACK is always
  sent.
- From 90 % of the budget the daemon holds back the Router Advertisement bursts and the CSL
  bootstrap trains, the two long transmissions. It sends them again under 80 %. The log shows a
  warning at each change.
- A job or a command that the firmware refuses is deferred, not failed: the daemon tries it again
  after one minute. The daemon does not build a Router Advertisement burst while the radio defers
  it: the log says so once for each period of deferral.
- The bootstrap trains are paced: see [Bootstrap pace](#bootstrap-pace).
- Known limits of the alpha:
  - A deferral holds the Router Advertisement bursts and the bootstrap trains for any origin,
    also for a user command. A user command to a device that has no anchor goes out with no train
    and ends with no anchor, and the daemon asks again.
  - A job that always asks for a train is refused while the radio defers, whatever the anchor:
    pair, auth, the link-maintenance echo and a fragmented datagram (a firmware block, a large
    config write). The echo keeps its probe and goes out when the deferral ends.
  - A one-frame job to a device that has an anchor goes out while the radio defers.
  - A job to a device with a known anchor is not held by the pace. If the anchors of several
    devices miss, the firmware sends a train for each of those jobs, and more than 4 trains can
    start in a minute. The completion counts the train, so the pace holds the next train of an
    unknown device.
- `airtime` in `GET /api/health` has `limit` `true`, `budgetMs`, `deferring` and `overBudget`.
  `radioWarnings` has a line while the hour is above the budget or the radio defers.

With the limit off the bridge must run the firmware of this release. An older v2.11 build has a
fixed cap of 100 s an hour and refuses above it. The log says so once, and `GET /api/health` shows
`airtime.budgetMs` above 0 with `limit` `false`: flash the bridge. The host starts no deferral for
that cap, and it holds an echo that the bridge refuses for 60 s.

The cost: with a plant that needs more than the budget, the radio defers often, the bootstrap
trains wait and commands are slower. The channel-busy refusal of `--lbt` is separate and retries
after 2 s.

### Bootstrap pace

A CSL bootstrap train is 252 frames and about 1 s on air. After a restart of the daemon no device
has a sample anchor in the bridge, so each queued job asks for a train. The 9 devices of the bench
sent 15 trains in the 879 s of a log after a restart. The daemon starts the trains at least 2 s
apart, except the echo and the fragmented jobs, which skip that cooldown. With the airtime limit on
it also starts at most 4 trains in any minute, for all devices together (a job to a known anchor
is the exception below). With the limit off (the default) there is no limit of trains in a minute.
With the limit off a queued job that asks inside the 2 s after the last train waits for the rest
of them, where before it went out with no train. With the limit off the train that an echo sends
on a known anchor starts no cooldown. A train that a
job needs and cannot start is deferred: the job stays in the queue of its device, keeps its tries,
and asks again. A job over the limit asks again when the oldest train of the minute leaves the
window. A job that asks inside the 2 s after the last train waits for the rest of them in the same
way, and does not go out with no train. A deferral does not drop a job. The log shows
`CSL bootstrap train deferred: ... paced`, once a minute at most.

With the airtime limit on, a job that asks for a train books a slot when it starts. The bridge sends a train only when it
has no anchor for the device. A job that ran on an anchor sent about 22 wake frames and no train,
and its slot returns when the job completes. The daemon holds the anchor of a device as known for 10
minutes after the last Enhanced ACK of any job to it, with the bootstrap flag or without it. A
train of one period that the device did not answer ends the known anchor at once (limit on only:
with the limit off the next job uses the anchor). A job to a device with a known anchor, such as a
firmware transfer or the link-maintenance echo, is never held by the pace and books no slot. If the
firmware sends a train for it anyway (the anchor missed), the completion counts that train in the
window of the minute and starts the 2 s cooldown (limit on only). The first frame of a batch
decides, and the retry cooldown of the device does not start. So a burst of jobs to known anchors
that all miss can exceed 4 trains in a minute: see the limits of the alpha. A job that sent a train
keeps its slot for the minute. The
link-maintenance echo needs its train for a device with no known anchor, so the pace holds it like
a queued job: it keeps its probe and goes out at a later tick, and it never goes out with no train.
A held echo does not stop the echoes of the other devices, and it is not asked again at each tick:
it waits the retry that the pace named, and at least 2 s. While the radio itself holds long
transmissions (synthesizer, scan, airtime deferral) it waits until the hold ends, and 60 s at most. A fragmented datagram books one slot for
all its frames, and its first frame decides. A fragmented response that the pace refuses goes out
whole on the anchor, with no train for any fragment. A job that the firmware turned away before
its first frame returns its slot. While jobs that have not completed hold the four slots, a job
that waits asks again after 2 s.

The pace serves a job that carries a user command before a refresh. A user command is a write that
the API or Home Assistant asked for: setpoint, mode, overlay, boost, child lock, open window,
identify, calibrate, reboot, display, pair, the home and away switch and the config write of the
API. A refresh is traffic that the daemon starts: zone measure and notify, config pushes of the
installation, time, reads, schedule block changes and the firmware transfer. A device whose queue
holds a user command counts as a user command for the whole queue, because the train that opens
the link serves every job behind it. A refresh gives way while a user command waits for a slot,
with the limit on or off. Jobs on an anchor have no order by origin. A
waiting job that does not ask again within 5 s of its retry time stops holding the refreshes back.
A response to a device request, such as the CoAP acknowledgement of a report, has no retry of its
own: over the limit it goes out on the anchor the bridge holds, with no train, and the device asks
again. `GET /api/health` shows the devices that wait in `bootstrap.waiting` and the limit of trains in a
minute in `bootstrap.perMinute` (0 with the airtime limit off).

The firmware keeps the count in 61 buckets of one minute and loses it at each reset of the ESP32,
and opening the serial port can reset the ESP32. The daemon therefore stores the buckets and their
time in its database. It stores the first reading after it opens the bridge. It stores each later
reading only when its buckets changed, so a radio that sends nothing writes the database once. It
stores the last reading again at a clean shutdown, after the radio read the firmware a last time.
The stored buckets move forward by the whole minutes since they were stored, and the sub-minute
rest of that time stays, so a store every 10 to 50 s still ages them. The array with the larger
total wins, whole: the moved stored buckets or the reading. A tie keeps the stored array. So an
ESP32 that reset alone and reports zeros does not erase the count that the database holds, and no
busy minute is copied into other buckets.

The minute of the firmware and the minute of the daemon have different phases: the daemon drops a
bucket up to 59 s after the firmware does. After the first reading of a run, the daemon therefore
compares a reading with the stored buckets moved one minute more. A reading that holds new airtime
wins in that gap, and the count never lacks the airtime of the last minute at a stop. A kill with no
final store can lose the airtime that the bridge sent after the last reading, up to 10 s of it, when
the stored buckets hold a minute that the firmware already dropped. A
reading with an earlier time than the stored one means that the wall clock went back: the daemon
keeps the buckets and gives them the time of the reading, so they age from there.

After the daemon opens the bridge and loads the registers, and before it sends `CMD_CONFIGURED`, it
moves the stored buckets forward by the whole minutes that passed, drops the buckets older than
61 minutes, and loads them into the firmware with `CMD_AIRTIME` and a 122-byte payload (126 bytes with the
airtime limit on: the buckets and the budget in ms as `uint32` big endian). A new
database loads zeros. The firmware compares the total of the load with the total of its own
window. When the load total is larger, the window takes the load. Otherwise the window keeps its
own count. So a load never lowers the count, also when the open did not reset the ESP32.
`CMD_AIRTIME` with no payload reads the count: `used` and the budget in force (0 means no cap) in milliseconds as `uint32` big endian, then the 61
buckets as `uint16` big endian in milliseconds, oldest first.

## Listen before talk

`--lbt` is experimental and the add-ons do not offer it. On a 9-device plant with no other 868 MHz
traffic, a 30 minute run at -80 dBm found the channel busy about 40 times a minute, on every device.
The cause is not measured yet: the threshold, the RSSI offset or the moment of the reading.

`--lbt` makes the firmware listen before the first frame of a CSL job. The daemon sends the
setting in `Configure`, after the register load and the airtime load and before `CMD_CONFIGURED`,
as `CMD_LBT` with 2 bytes: the switch (0 or 1) and the threshold in dBm as a signed byte. The
firmware echoes the 2 bytes. The daemon sends the setting at every start, also with the switch off,
so that a bridge that did not reset does not keep the setting of an earlier run. A firmware without
`CMD_LBT` starts with the switch off. With `--lbt` it is a firmware that needs a flash, and the
daemon runs as with no bridge.

A job that finds the channel busy is deferred, not failed: nothing went on air, the job keeps its
tries and the daemon sends it again after 2 s. `GET /api/health` shows `lbt` with `enabled`,
`thresholdDbm` and `busy`, the number of jobs and bursts that found the channel busy since the
daemon started. The reason for the default and for the Enhanced ACK, which has no listen, is in
[Channel access](PROTOCOL.md#channel-access).

## Synthesizer lock

The CC1101 cannot hold the carrier while its synthesizer is not locked. After every SCAL the
daemon reads FSCAL1 back: the value 0x3F means no lock. One calibration makes three tries.

At start, a bridge that fails its configuration does not stop the daemon. The cause can be a
synthesizer that does not lock, or any other error of the register load. The daemon then runs as
with no bridge: on a mock radio, in dry-run. `GET /api/health` answers `503` with `radioError`
that names the cause, for example:

```text
configure the bridge: configure: the synthesizer did not lock after SCAL: 3 attempts: the daemon tries again every 1m0s
```

The start loads the registers and the airtime, sends `CMD_CONFIGURED`, and then writes the
calibration seed into FSCTRL0, which runs SCAL again. A seed write that fails for any reason, a
synthesizer that does not lock or a reply that the link lost, is a failed start: the register can
hold the seed with no lock result.

The daemon opens the bridge again every 60 s and runs this whole check: the register load and the
seed write with its lock. When a try works, the real start works too, so the daemon restarts itself
and runs the normal start: the register load, the airtime load, `CMD_CONFIGURED`, the stored receiver
calibration and the `--tx` setting of the command line. A seed that does not lock keeps the daemon on the mock
radio and does not restart it. A bridge with firmware below v2.11
is not tried again: it needs a flash, see [Bridge firmware level](#bridge-firmware-level).

While the daemon runs, a synthesizer that is not locked at the hourly calibration, or after a
register write, holds the radio:

- The radio transmits nothing: no Enhanced ACK, no Router Advertisement burst, no CSL job and no
  pair window beacon. A pair window that is open closes, and a request to open one is refused. The
  refusal also covers a lock lost by the register write that the open sends before the window.
  The receiver stays on and hears frames.
- The radio calibrates the synthesizer again every minute. The warning in `radioWarnings` says so.
  A calibration that locks lifts the hold, and the radio sends again.
- A job that the hold refuses is deferred: the daemon tries it again after one minute.
- After 3 failed retries in a row the radio loop ends, 3 minutes after the first failed
  calibration. The daemon stops, and the supervisor starts
  it. The start opens the bridge and configures it as above.

## Serial link faults

Symptom: the log shows lines like these, and `serialErrors` in `GET /api/health` grows.

```text
rx autoack: serial link fault: reply truncated: deadline exceeded after 89/113 bytes
CSL queue poll: serial link fault: reply truncated: deadline exceeded after 98/119 bytes
```

Before version 0.3.1 the daemon ended with `radio loop ended: live radio owner ended`
within minutes, and the supervisor restarted it.

Cause: the CH340 adapter of the ESP32 board lost bytes of replies of about 80 to 120 bytes at
921600 baud. The board has no flow control line, and the CH340 datasheet advises flow control
or a lower rate from 500 kbaud in both directions. It showed on a Raspberry Pi 5 with a
second full-speed USB device on the same bus, and not on an x86 host. The point of loss inside
the USB stack is not measured. Bridge firmware v2.10 and later talks 230400 baud.

Fix: flash firmware v2.11 or later: see [Bridge firmware level](#bridge-firmware-level). The
daemon logs the banner and the rate at start. A request that loses a byte fails its CRC, and the
firmware drops it instead of running a wrong command.

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
