# Security

## Report a vulnerability

Use GitHub private vulnerability reporting for this repository: open the Security tab and choose
"Report a vulnerability". If that option is not available, open an issue that says you have a
security report and give no details. The maintainer then arranges a private channel.

Do not post a network key, an API token, a serial number or a home id in a public issue.

Only the latest release gets fixes. freedo is experimental.

## What protects what

The API token protects the plant. Anyone who has the token can:

- set temperatures and switch zones, hot water and the whole home;
- change the installation, including devices, zones, circuits and (while no device is
  declared) the network key;
- pair devices and start a firmware transfer;
- read device identifiers from the installation routes.

## Home Assistant ingress

The add-on starts the daemon with `--ingress`. Home Assistant authenticates the user and the Supervisor
forwards the request from the address `172.30.32.2`. Every Home Assistant user can open an ingress
session, also a user who is not an administrator: `panel_admin` hides only the sidebar entry. The daemon
cannot tell the users apart.

For a request from `172.30.32.2`, the daemon skips the token for these method and path pairs only. They
are the calls of the dashboard. The list is `internal/daemon/ingress_routes.json`, and `{n}`, `{id}`
and `{ref}` stand for a zone number, a device and a device reference:

- Read: `GET /api/health`, `/api/zones`, `/api/nodes`, `/api/home`, `/api/boiler`, `/api/circuits`,
  `/api/zones/{n}/schedule`, `/api/presets` and `/api/presets/{id}/schedules`.
- Zone: `POST /api/zones/{n}/setpoint`, `/off`, `/auto`, `/boost`, `/window` and `/offset`, and
  `PUT` and `DELETE /api/zones/{n}/schedule`.
- Preset: `POST /api/presets`, `PATCH` and `DELETE /api/presets/{id}`, and
  `POST /api/presets/{id}/activate`.
- Home: `PATCH /api/home` (home or away).
- Boiler: `POST /api/boiler/flow` (the flow limit).
- Device: `POST /api/nodes/{id}/childlock` and `/identify`, and
  `PUT /api/install/devices/{ref}/label` (the label alone, no other field).
- Installation, read: `GET /api/install`, `/api/install/devices`,
  `/api/install/devices/{ref}/removal`, `/api/install/zones` and `/api/install/circuits`.
- Installation, write: `PUT /api/install/timezone` (the time zone alone), `POST /api/install/devices`,
  `PUT /api/install/devices/{ref}/zone` (the zone claim alone), `DELETE /api/install/devices/{ref}`,
  `POST /api/install/zones`, `PATCH` and `DELETE /api/install/zones/{n}`,
  `POST /api/install/zones/{n}/members`, `DELETE /api/install/zones/{n}/members/{ref}`,
  `PUT` and `DELETE /api/install/zones/{n}/reporter` and `/measuring`, `POST /api/install/circuits`,
  and `PATCH` and `DELETE /api/install/circuits/{id}`.
- Pairing: `GET` and `POST /api/pair/window`, and `POST /api/pair/devices/{ref}` (no body: the network
  key of the installation, never a key of the caller).

So a Home Assistant user, without the token, can read the state of the plant, set temperatures, read,
write and remove the home and the away program of a zone, create, rename, delete and switch the program
presets, switch zones off, switch the home between home and away, set the flow limit, set the temperature
offset of a room, rename a device,
and lock or identify a valve. A preset holds the weekly programs of the zones, so managing the presets is
managing the weekly schedules: it gives no control over the plant that a schedule write does not already
give. Only a preset that is active reaches the devices.

Every Home Assistant user who can open the page can also change the installation and pair devices,
without the token. That user can add, rename and delete rooms, write every option of a room
(`PATCH /api/install/zones/{n}`), add, change and delete heating circuits, declare, move and remove
devices, set the time zone, open the pair window for up to 600 seconds and send the pair command. A
pairing sends the network key over the air under the public pairing key, so a user who can open the page
can cause that. A removed device or a deleted room stops the control of that room. The project accepts
this, because an add-on user cannot read a token that the add-on created and the first setup needs these
calls. Give the right to open the page with care.

These stay behind the token, also from that address: the identity of the network (`home_id`,
`coordinator`) and every other field of `PATCH /api/install`, the network key
(`PUT /api/install/network_key`, and `POST /api/pair` with its `networkKeyHex` field), the kind and the
serial of a device (`PATCH /api/install/devices/{ref}`), reboot, calibration and firmware,
the low-level device writes, the raw device reads and the debug routes. `GET /api/install` shows the
home id and the coordinator EUI. They are identifiers, and no route returns the network key. The daemon compares the method and the exact path. It reads the address of the TCP
connection and no header. It ignores `--ingress`, and logs a warning, unless the process runs under the
Supervisor: the variable `SUPERVISOR_TOKEN` must be set. Outside the Supervisor the address
`172.30.32.2` belongs to some other host.

The API uses plain HTTP. There is no TLS, so the token and all data cross the network in clear
text. Run the daemon on a trusted network, or put a TLS proxy in front. Without a token the daemon
accepts a loopback address only. `--insecure-no-auth` removes that limit: avoid it.

The network key protects the radio traffic. It lives in the database `freedo-state.db` and the
`-wal` and `-shm` files next to it. The daemon sets them to mode `0600`. Anyone who reads
the database can decrypt the traffic and command the devices. Protect the database and its
backups. The add-on stores it in its data folder, which Home Assistant backups include.

The stable add-on also writes a copy of the database to `/share/freedo/stable/`, for the beta add-on.
The copy holds the network key. The folder has mode `0700` and the files have mode `0600`. That stops
no add-on: every add-on that maps the `share` folder runs as root and reads the copy, and a file
sharing add-on gives it to its users. A backup that includes the share folder holds it too. Install
only add-ons you trust, and delete `/share/freedo/stable` when you do not use the beta add-on. The
stable add-on writes it again at each start. The beta add-on keeps the databases that an import
replaced under `/data/backup/` in its data folder.

The file `/share/freedo/owner.json` names the add-on that started last. A process that can write to
the share folder can change it. It can then delay the start of an add-on by up to 250 seconds, or
stop the start while the host it names answers on port 18080.

The pairing key is the same on every valve and is not secret. The network key crosses the air
under the pairing key during a pairing. Keep the pair window short and the band quiet.

The devices do not enforce a frame counter. A frame is accepted when its message integrity code
verifies under the network key. An attacker who records frames and has the key can replay them.

The debug routes (`--debug-api`) decrypt any frame that a caller sends and stream live traffic.
They are off by default. Leave them off.

## The unhandled log

The daemon appends a line to `unhandled_log` for each radio event it cannot handle. The log
hides the secrets it knows about and does not redact everything.

What the log hides: the frame payload stays encrypted, a frame under the public pairing key shows
only its header, and the values of the device token, the serial, the session id and the challenge
keys appear as lengths.

What the log still shows:

- device labels, or serial numbers when a device has no label;
- the clear header of each frame: source and destination EUI and the sequence number;
- the CoAP path of a request, which can hold the serial number and the home id;
- the values of tags that are not secret.

Remove or replace these values before you attach the log to a report. A log written by a build
older than the redaction rule can hold a device token in clear text. Delete such a file. Never
share the log together with the network key.

The routes `GET /api/nodes/{id}/resource`, `.../dbg/mem` and `.../currentlog` return raw device
answers, and `freedo-dev decrypt` and `freedo-dev dissect` print decrypted frames. Their output
can hold credentials. Do not attach it to a report.
