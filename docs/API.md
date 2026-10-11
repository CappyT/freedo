# HTTP API reference

The freedo daemon serves a JSON API over HTTP. The Home Assistant integration uses it.
Any other client can use it too.

This file lists every route that `internal/daemon` registers and the rules that apply to
the routes. For the radio facts behind a rule, see [PROTOCOL.md](PROTOCOL.md).

## Conventions

### Address and authentication

- The daemon listens on `http_listen` (default `127.0.0.1:8080`). The flag `--addr` overrides it.
- Every request under `/api` carries `Authorization: Bearer <token>`. The daemon serves the
  [dashboard](OPERATION.md#dashboard) at every other path, without a token.
- The daemon reads the token from one source, in this order: the file of `--api-token-file`,
  the variable `FREEDO_API_TOKEN`, the file of the key `api_token_file`.
- The daemon removes spaces at both ends of the token. An empty token stops the start.
- The daemon compares tokens in constant time. A missing or wrong token gets `401` with
  `WWW-Authenticate: Bearer`.
- `--health-public` makes `GET /api/health` readable without a token, for a container
  health check. The default is off.
- `--ingress` makes the daemon accept a request from the connection address `172.30.32.2`, the Home
  Assistant ingress gateway, without a token. Only the add-on start script sets it. Every other source
  needs the token. See [Access](OPERATION.md#access).
- With no token, the daemon accepts a loopback address only and warns at start. An address
  that is not loopback stops the start. `--insecure-no-auth` removes this limit. With it,
  every host that reaches the port controls the plant.

To create a token:

```sh
openssl rand -hex 32 > freedo_api_token
chmod 600 freedo_api_token
curl -H "Authorization: Bearer $(cat freedo_api_token)" http://127.0.0.1:8080/api/stats
```

### Requests and answers

- A JSON body holds one value and no field that the route does not know. An unknown field
  or a second value gets `400`. A body over 64 KiB gets `413`. The limit is 1 MiB for
  firmware upload and for the debug routes.
- The server closes a connection that sends no complete header in 10 seconds.
- When a client closes a request, the daemon stops to wait for the engine and answers `503`.
  A command that the engine already took still runs.
- A command that cannot save its state in the database gets `500`.
  `PATCH /api/home`, `PUT /api/zones/{n}/schedule` on the hot water zone and
  `POST /api/zones/{n}/window` save first and change the engine state after. After a `500`
  the engine state is unchanged.
- A write that queues a radio message answers with the queue state of the target device.
  `GET /api/queue/{id}` shows the queue and the history, with the result of each message.
  A newer write to the same zone replaces a queued one that is not yet on air. The history
  shows the replaced message as `superseded`.
- After a `5.xx` answer from a device, the daemon repeats the write with a new message id
  and token.
- The daemon exits with code 1 when the live radio loop ends on an error. The service
  manager or the container restart policy then starts it again. The API keeps serving
  after a replay ends.

### Installation writes

Every write under `/api/install` answers `{"rev": N, "warnings": [...], <object>}`. Each
write accepts `rev` as an optimistic concurrency token. The label of a device is unique:
a write that repeats the label of another device gets `409` and names that device.
A delete that breaks a reference gets `409` and names the row that holds the reference.

### Credentials in answers

No route outside `/api/debug/*` returns a credential. The credentials are the device token,
the session id, the challenge keys and the network key.

- `Telemetry` of `/api/nodes` omits `auth_token`, `serial_id`, `cloud_session_id`,
  `challenge_key_rotate` and `0262`.
- `SessionIssued`, `SessionAdopted` and `SessionIssuedUnix` tell whether and when the daemon
  served a session. They do not carry the id.
- `GET /api/install` reports `network_key.present`. `PUT /api/install/network_key` and the
  field `networkKeyHex` of `POST /api/pair` receive the key and never return it.
- `GET /api/nodes/{id}/resource`, `/dbg/mem` and `/currentlog` return the raw device answer
  in hexadecimal, without a filter.

Identifiers are not credentials. The serial number, the EUI, the home id and the
coordinator EUI appear in `/api/install/*`, in `Serial` of `/api/nodes` and in the start
banner.

## Routes

`{id}` names a device by label, serial number or short address. `{n}` is a zone number.
`{ref}` names a device the same way.

### Read

| Route | Purpose |
|---|---|
| `GET /api/health` | State of the daemon, the radio and the devices, with alerts. |
| `GET /api/nodes`, `GET /api/nodes/{id}` | Devices with telemetry, battery, faults and settings. |
| `GET /api/zones` | Zones with members, demand, target, window state and external temperature. |
| `GET /api/circuits` | Heating circuits as the engine sees them. |
| `GET /api/boiler` | Boiler values, flow limit state, energy and gas estimate. |
| `GET /api/home` | Home and away state and the scheduled setpoint of each zone. |
| `GET /api/stats` | Frame counters, node count and unhandled counts. |
| `GET /api/unhandled` | Counts of unhandled radio events by reason. |
| `GET /api/queue/{id}` | Queue and history of a device. |
| `DELETE /api/queue/{id}` | Drop the queue of a device. |
| `GET /api/early-start` | Early start state of all zones. |
| `GET /api/heatsource/capabilities` | Reads the heat source capabilities from the [boiler thermostat](#boiler-thermostat). |

### Zone commands

| Route | Purpose |
|---|---|
| `POST /api/zones/{n}/setpoint` | Set a manual overlay. |
| `POST /api/zones/{n}/off` | Switch the zone off with an overlay. |
| `POST /api/zones/{n}/auto` | Return the zone to its program. |
| `POST /api/zones/{n}/boost` | Start or stop the boost of a room. |
| `POST /api/zones/{n}/offset` | Set the temperature correction. |
| `POST /api/zones/{n}/window` | Open or close the window state from an outside source. |
| `POST /api/zones/{n}/temperature`, `DELETE ...` | Give or remove an external room temperature. |
| `GET`, `PUT /api/zones/{n}/schedule` | Read or write the weekly schedule. |
| `GET`, `PUT /api/zones/{n}/early-start` | Read or switch the early start. |
| `DELETE /api/zones/{n}/early-start/model` | Delete the learned heat-up model. |
| `GET /api/zones/{n}/episodes` | Recorded heating and cooling episodes. |
| `GET /api/zones/{n}/pid-suggestion` | Suggested controller gains from the episodes. |
| `PATCH /api/home` | Switch the whole home between home and away. |

### Device commands

| Route | Purpose |
|---|---|
| `POST /api/nodes/{id}/setpoint` | Low-level setpoint write to one device. |
| `POST /api/nodes/{id}/mode`, `.../overlay` | Low-level overlay mode (`overlay` is an alias). |
| `POST /api/nodes/{id}/openwindow`, `.../ow` | Low-level window write (`ow` is an alias). |
| `POST /api/nodes/{id}/boost` | Low-level boost. |
| `POST /api/nodes/{id}/childlock` | Lock or unlock the controls of a valve. |
| `GET`, `PUT /api/nodes/{id}/display` | Read or write the display settings of a valve. |
| `POST /api/nodes/{id}/identify`, `.../beep` | Make a valve signal itself (`beep` is an alias). |
| `POST /api/nodes/{id}/calibrate` | Start the motor calibration of a valve. |
| `POST /api/nodes/{id}/reboot` | Restart a device. |
| `POST /api/nodes/{id}/extui` | Write the external interface endpoints. |
| `POST /api/nodes/{id}/netcmd` | Send a non-destructive network command. |
| `POST /api/nodes/{id}/config` | Write chosen `d/config` tags. |
| `DELETE /api/nodes/{id}/config/offlineScheduleEnabled` | Remove the per-device offline schedule value. |
| `POST /api/nodes/{id}/factoryreset` | Make a device leave the network. |
| `PUT /api/nodes/{id}/schedule` | Write the schedule document to one device. |
| `GET`, `POST`, `DELETE /api/nodes/{id}/fw` | Read the firmware state, start or cancel a transfer. |
| `GET /api/nodes/{id}/currentlog` | Read the device log. |
| `GET /api/nodes/{id}/dbg/mem?adr=&len=` | Read device memory (`refresh=1` forces a new read). |
| `GET /api/nodes/{id}/resource?path=&query=` | Read any resource of a device. |

### Boiler, environment and pairing

| Route | Purpose |
|---|---|
| `POST /api/boiler/dhw` | Set the hot water temperature overlay. |
| `POST /api/boiler/flow` | Set the flow limit of a circuit (the [boiler thermostat](#boiler-thermostat) by default). |
| `POST /api/outdoor` | Give the outdoor temperature. |
| `GET`, `POST /api/pair/window` | Read, open or close the pair window. |
| `POST /api/pair` | Send the `d/pair` command to a device. |

### Installation

| Route | Purpose |
|---|---|
| `GET`, `PATCH /api/install` | Read or change the installation and its options. |
| `PUT /api/install/network_key` | Write the network key (only while no device is declared). |
| `GET`, `POST /api/install/devices` | List or declare devices. |
| `GET`, `PATCH`, `DELETE /api/install/devices/{ref}` | Read, change or delete a device. |
| `GET /api/install/devices/{ref}/removal` | Show what a removal changes. |
| `GET`, `POST /api/install/zones` | List or declare zones. |
| `GET`, `PATCH`, `DELETE /api/install/zones/{number}` | Read, change or delete a zone. |
| `PUT`, `POST /api/install/zones/{number}/members` | Replace the members, or add one. |
| `DELETE /api/install/zones/{number}/members/{ref}` | Remove one member. |
| `PUT`, `DELETE /api/install/zones/{number}/reporter` | Set or clear the demand reporter. |
| `PUT`, `DELETE /api/install/zones/{number}/measuring` | Set or clear the measuring device. |
| `GET`, `POST /api/install/circuits` | List or declare circuits. |
| `GET`, `PATCH`, `DELETE /api/install/circuits/{id}` | Read, change or delete a circuit. |

### Debug

`--debug-api` or `debug_api: true` enables these routes. They decrypt any frame that a
caller sends and stream live traffic. Leave them off in normal use.

| Route | Purpose |
|---|---|
| `POST /api/debug/dissect` | Interpret a frame. |
| `POST /api/debug/decrypt` | Decrypt a frame. |
| `GET /api/debug/rx` | Stream received frames. |

## Installation model

A device is a physical node. A zone is a room or the hot water service. A circuit joins
one or more zones to a heat source. The three have separate identifiers.

- A heating zone holds valves. It can hold a wall thermostat as the measuring device.
- The hot water zone holds the wall thermostat.
- `measuring_device` selects the measurement the zone uses. `reporter` selects the member
  that publishes the aggregated demand. The loader rejects a missing or ambiguous reference.
- A zone number can be sparse. A valve answers to one logical zone number. The `zone` field
  of the device states it. The circuit number does not depend on the zone number.
- A circuit needs a heat source, and the heat source must be a wall thermostat. A heating
  zone should have a circuit. Without one, the zone configuration for its valves has no
  circuit tag `6060` and no heat source among the members, and the install check warns about
  the zone. No device has run a zone configuration of this kind.
- The order of the members matters: the zone configuration document has other bytes with
  another order.

The installation is stored in the SQLite database. The tables are `install` (key-value
options), `net_key`, `device`, `circuit`, `zone` and `zone_member`. Each write is one
transaction that applies the rows, rebuilds the installation, validates it and raises
`install_rev`. The engine gets the installation after the commit.

The database holds more than the installation. It also keeps the desired state of the
devices and the learned state. These are the away state, the hot water program, the program
blocks of each zone, the early start models, the adapted flow limits, the setpoint shifts
from an outside temperature, the boiler counters, the resume points of the frame counters
and the radio calibration. The node state, the learned zone members and the command queue
are caches. Device reports rebuild them, so a restart repairs them.

The database uses the pure Go driver `modernc.org/sqlite`, WAL mode and
`synchronous=FULL`. It migrates forward only. A damaged, foreign or newer database stops
the start with an error. `SIGTERM` and `SIGINT` run the same orderly stop. It ends in
less than nine seconds, below the ten seconds of `docker stop`.

On the first start with an empty database, the daemon creates the 16-byte network key, the
coordinator EUI and a home id. It never prints the key. A network key that differs from
the one of an existing network makes every frame of that network fail the MIC check.

### Installation options

`PATCH /api/install` writes these options. A `null` removes the value where the option
allows it.

| Option | Default | Meaning |
|---|---|---|
| `home_id`, `coordinator` | generated | Identity of the network. Change only while no device is declared. |
| `utc_offset_min`, `timezone` | host zone | Clock of the devices (tag `02b2`) and of the programs. A set `utc_offset_min` wins over `timezone`, and a loadable `timezone` (IANA name) wins over the host zone. `GET /api/health` shows the source in `timezone`. |
| `utc_push_spacing_sec` | 60 | Spacing of the offset writes to the valves. Each write homes the motor. |
| `offline_schedule` | `true` | Tag `02b3` for every device without its own value. |
| `external_window_max_sec` | 7200 | Cap of a window state from an outside source. |
| `room_temp_max_age_sec` | 1800 | Age at which an external room temperature expires. |
| `low_battery_cell_mv` | 1100 | Battery alarm threshold per cell. |
| `offline_after_sec` | 3600 | Silence after which a device is `offline`. |
| `flow_limit_c` | none | Flow limit of every circuit that no manual value, curve or adaptation sets, 30 to 75 C. It is also the manual maximum that the default curve reduces. While it is unset, such a circuit is served no limit: see [Flow limit](#flow-limit). A `null` does not clear a set value. |
| `outdoor_max_age_sec` | 3600 | Age at which the outdoor temperature expires. |
| `flow_hysteresis_c`, `flow_min_interval_sec` | 1, 1800 | Smallest change and shortest interval of the served flow limit. |
| `flow_curves`, `flow_auto_adapt` | none, `false` | Weather curve per circuit, and the automatic adaptation. |
| `flow_default_curve` | `false` | Built-in weather curve for every circuit without a curve of its own. It needs `flow_limit_c`: see [Flow limit](#flow-limit). |
| `early_start_min_episodes`, `early_start_max_lead_sec` | 3, 7200 | Early start thresholds. |
| `boiler_max_power_kw`, `boiler_min_power_kw` | none, 0 | Boiler output at 100 % and 0 % modulation. |
| `boiler_max_gas_m3h`, `boiler_min_gas_m3h` | none | Fuel flow at 100 % and 0 % modulation, in `fuel_unit` per hour. |
| `fuel_unit` | `m3` | Unit of the fuel flow and counter: `m3`, `l` or `kg`. `null` takes `m3`. |
| `router_gateway`, `router_dns` | `0.0.0.0` | IPv4 gateway and DNS address that the Router Advertisement carries, as dotted quads. `null` or an empty string takes `0.0.0.0`. Not verified on a device. |

### Zone options

`PATCH /api/install/zones/{number}` writes these options.

| Option | Default | Meaning |
|---|---|---|
| `open_window_detection`, `open_window_duration_sec` | `true`, 900 | Window detection of the valves and the time the zone stays open. |
| `early_start` | `false` | Early start for the zone. |
| `device_overlay_end`, `device_overlay_end_sec` | `until_next` | End rule of a change made on a device: `manual`, `until_next` or `timer` with a duration. |
| `offset_mode` | `config` | How the temperature correction reaches the zone: `config`, `dbg2` or `setpoint`. |
| `pid` | 1000, 500, 1500 | Controller gains `kp`, `ki`, `kd` of the valve, each 0 to 65535. `kp` and `ki` cannot both be 0. `null` restores the defaults. |
| `away_temperature_c` | `null` | Temperature in away, 5 to 25 C with at most two decimals. `null` and 5 mean off in away. The hot water zone refuses it. |

A `pid` change gives the zone document a new ETag and writes nothing to the valve. The
valve rereads the document after its hourly check, applies the gains and resets the
integrator. A change of `away_temperature_c` during away rewrites that zone at once. A
change at home writes nothing and applies from the next away.

## Zones

### Zone commands

The daemon chooses the device to write. The commercial coordinator writes `z/s?id=N` only
to the reporter of the zone, even when the reporter is the wall thermostat. The other
members take the block from the echo of the reporter. A zone without a reporter and with
one member writes that member. With more members, `setpoint`, `off`, `auto` and `boost`
write every member. The answer then holds the members in `target`, separated by commas
(serial number, or EUI when a device has none), and the sum of their pending downlinks in
`pending`. The hot water zone writes the wall thermostat.

- `POST /api/zones/{n}/setpoint` with `{"setpointC": 21, "mode": "manual"}` sets an overlay.
  A room accepts 5 to 25 C. Hot water accepts 30 to 65 C.
  - `mode` is `manual`, `until_next`, `timer` (with `durationSec`) or `until_auto_change`.
  - `until_next` lasts until the next block of the program. Without a program it does not end.
  - `until_auto_change` lasts until the next program block or the next switch between home
    and away, whichever comes first. On air it is `6240=1`. A capture shows it for rooms.
    For hot water the value is not verified. The daemon keeps the rule and the deadline in
    the database. `device_overlay_end` does not accept it.
  - `timer` needs `durationSec`, 1 to 604800 (7 days). The other modes refuse it.
- `POST /api/zones/{n}/auto` returns the zone to its program.
- `POST /api/zones/{n}/off` with `{"mode": ...}` switches the zone off with an overlay
  (`6260=0`, no `6280`). `mode` takes the values of `setpoint`.
- `POST /api/zones/{n}/boost` with `{"on": true}` sets a room to 25 C for 30 minutes, then
  returns it to the program.
- `POST /api/nodes/{id}/setpoint` is the low-level form. It needs `tempC`. `tempC` and
  `sched` stay in 5 to 25 C, because a value outside wraps in the 16 bits of the tag.

### Overlays made on a device

A person who turns the wheel of a valve sends `PUT h/<home>/z/N/overlay`. The daemon answers
`2.05` and writes nothing. The end rule does not travel on air, so the daemon applies the
`device_overlay_end` rule of the zone:

- `manual` does not end.
- `until_next` ends at the next program block. Without a program it does not end.
- `timer` ends after `device_overlay_end_sec`.

At the end, the daemon writes `6240=0` and the program setpoint to the writer of the zone.
`POST /api/zones/{n}/auto` cancels the overlay. The deadline survives a restart.

The elected reporter sends `DELETE h/<home>/z/N/overlay` when `6240` returns to 0 on the
device. The daemon answers `2.05`, returns the zone to automatic, drops the deadline and
replaces queued writes that repeat the old overlay.

### Zone target in `GET /api/zones`

`declared[].writer` names the device that receives commands. `declared[].target` holds
`targetC`, `source` (`program`, `overlay`, `boost`, `window` or `away`), `overlay`
(`kind`, `rule`, `on`, `setpointC`, `endsAt`), `programC`, `programOff`, `hasProgram`,
`boost`, `known` and `on`.

- `targetC` is `null` when the device follows a value of its own: open window, away without
  a temperature, an off overlay or a program block that is off. It is also `null` on a zone
  that has no program and whose zone cache holds no `6200`.
- `hasProgram` is `true` while the daemon holds a weekly program for the zone: the hot water
  program, or a schedule document that a member of a heating zone is served. It is `false`
  for a zone with no program. `source` is `program` in both cases, because it only says that
  no overlay, window or away state applies. Read `hasProgram` to tell a zone that follows a
  program from a zone that has none.
- Without a program, `programC` is the `6200` that the zone cache holds (the last write or
  member report that carried it), or `null` when the cache holds none. A zone with no
  program and no `6200` has `targetC` and `programC` at `null`. The daemon writes such a zone
  only at a switch between home and away and at a command, never at a block change or at the
  start, and its writes at home carry no `6200`.
- `overlay.kind` follows `6240`: `manual`, `until_next` or `until_auto_change`. A timer
  appears as `until_next`, because the air has no timer.
- `overlay.rule` is the rule the overlay runs under: `manual`, `timer`, `until_next` or
  `until_auto_change`. `endsAt` is the deadline the daemon keeps, `null` without one. The
  remaining time is `endsAt` minus now.
- `GET /api/zones` reports the mode of each zone in `demand`.

### Home and away

`GET /api/home` returns the state and the scheduled setpoints. `scheduledOff` lists the
zones whose current block is off. `PATCH /api/home` with `{"away": true}` switches the
whole home and writes each zone once. The state survives a restart and appears in
`/api/health`.

- In away, a heating zone with `away_temperature_c` above 5 receives `6160=2`, `61e0=1` and
  the temperature in `6200`. Without the field, or at 5, it receives `61e0=0` and no `6200`.
- A write to a zone without an overlay carries `6440` when the effective target changes.
  A running overlay keeps its block, and the write carries no `6440`. The return to home
  writes the program block again.
- An overlay with the rule `until_auto_change` ends at each switch between home and away.
  The zone then receives the return to program with the new home state.
- Hot water follows away like a room. On its program it switches off in away. A permanent
  manual overlay (for example 48 C) stays on in away.

### Weekly schedule

`PUT /api/zones/{n}/schedule` writes one or more day programs with a day mask and switch
points. `GET` returns the stored programs, or `404`. Each member of the zone receives the
same document with its next GET.

- A member fetches the program only with `offline_schedule` on. When the document ETag of
  a member changes and its tag `02b3` is 1, the daemon queues an empty `POST d/sched`. The
  device then fetches at its next report cycle. With `02b3` at 0 the daemon queues nothing.
- A device that has no coordinator answer for 25 minutes applies the program by itself
  (needs `02b3` at 1).
- The daemon checks these rules. Every mask selects at least one day. Masks share no day and
  cover seven days. Every program starts at minute 0, and its minutes increase up to 1439. Each
  encoded program stays within 255 bytes. A heating setpoint stays in 5 to 25 C, also in an off
  block.
- The daemon evaluates programs, early start and `until_next` on the clock of the devices:
  UTC plus the served `02b2`. The clock follows `utc_offset_min`, else `timezone`, else the
  zone of the host. A program minute is a local time, also on a day with a clock change.
- The hot water program stays in the daemon. No device downloads it. The setpoint stays in
  30 to 65 C. At each block change the daemon writes the wall thermostat with `6200` and
  `61e0=1`, or `61e0=0` for an off block.
- The daemon stores the block each zone received. After a restart it writes once each zone
  whose program moved while the daemon was down. It also stores zone rewrites that are queued
  and not confirmed, and queues them again once after a restart.
- A device that moves to another heating zone receives the document of the new zone, taken
  from a member that stays there. Without one the device gets `2.03` and keeps its program.
- At start the daemon removes the stored `sched` document of the hot water thermostat when
  no heating program explains it, and logs this.

`PUT /api/nodes/{id}/schedule` writes the document to one device.

### Window

`POST /api/zones/{n}/window` with `{"open": true}` or `{"open": false}` sets the window
state from an outside source, for example a Home Assistant sensor.

- The daemon writes `62e0` to the demand reporter when the zone state changes. The other
  members take the block from the echo of the reporter. Without a reporter, or with an
  offline one, the daemon writes every member. Each write carries the home state, the program
  `6200` and the active overlay.
- A member that does not accept the write is retried every 30 seconds, 40 times. The daemon
  then lists it in `undelivered`. The list is not stored: after a restart the daemon retries
  the writes that are due.
- The valve detector opens the same window with the report `z/ow` when the zone has
  `open_window_detection`. The window closes after `open_window_duration_sec`.
- A member that clears its own `62e0` sends `DELETE h/<home>/z/N/ow`. The daemon answers
  `2.05`, drops that source and, when the zone closes, writes `62e0=0` to the zone writers.
- The zone stays open while one source keeps it open. An outside window without a close ends
  after `external_window_max_sec`.
- `open`, `source`, `since` and `deadline` are stored and appear in `GET /api/zones` and, for
  each zone, in `/api/health`. The hot water zone has no window.

### Temperature correction

`POST /api/zones/{n}/offset` with `{"offsetC": -0.7}` sets the correction. The zone option
`offset_mode` selects how it applies.

- `config` writes tag `0140` in the whole `PUT d/config`, as the commercial bridge does. The
  valve rereads the configuration and homes its motor on each change. The wall thermostat
  ignores the document of a `d/config` write. The daemon puts `0140` in the served document
  under a new ETag, and the thermostat rereads it. The answer carries `"queued": false`,
  `"served": true` and `"reread"`.
- `dbg2` writes the single record `0140` with `PUT d/dbg2/tlvs`. On the `2.04` of the valve
  the daemon puts it in the served `d/config` under the same ETag. A `4.xx` answer or a lost
  write leaves the document unchanged. The valve does not home. No capture shows this on
  air. The wall thermostat has no `d/dbg2/tlvs`, so its zone refuses this mode.
- `setpoint` leaves `0140` unchanged. The daemon stores `setpoint_offset_c`, subtracts it
  from every setpoint it writes to the zone and adds it to the temperature and target of the
  zone in `GET /api/zones`. Node views keep the valve scale.

### External room temperature

`POST /api/zones/{n}/temperature` with `{"tempC": 20.4}` gives the zone the room temperature
that an outside sensor measures. `DELETE` removes it. `tempC` stays in -10 to 50 and the
daemon rounds it to 0.01 C. A zone must be a heating zone in mode `setpoint`. Hot water gets
`400`, an unknown zone `404`, and the modes `config` and `dbg2` get `409`.

The valve controls on its own sensor, and the daemon cannot change that value. The daemon
shifts the setpoints it writes instead. The shift is the external temperature minus the zone
temperature `4060`. A zone with several valves has one shift, because the write goes to the
writer and the other members take the block from its echo.

The shift moves slowly, because a valve near a radiator swings by several degrees:

- A first-order filter with a 2 hour time constant smooths the difference.
- The difference stays within 5 C of the manual correction.
- The applied shift changes by 0.2 C at a time, and only when it is more than 0.15 C from
  the filtered value.
- Two shifts of one zone are at least 15 minutes apart. The interval also starts at daemon
  start.
- A shift that does not change the setpoint writes nothing.

A correction of 2 C needs about 4 hours. These constants are design estimates. No test on air
verifies them.

While the reading is fresh, the shift replaces the manual correction. An older reading than
`room_temp_max_age_sec`, or a `DELETE`, ends it. The shift then returns to the manual
correction at the same pace. A zone that leaves the mode `setpoint` gets setpoints without a
shift and forgets the reading. The reading is not stored. The applied shift is stored in
`meta.room_shift`.

`declared[].externalTemp` in `GET /api/zones` shows `tempC`, `at`, `ageSec`, `stale`,
`inUse`, `deviceC`, `shiftC`, `offsetC` and `wantedC`. While `inUse` is true,
`demand[].TempC` is `tempC`. Early start, flow adaptation and the plant episodes read the
same temperature.

### Early start

The daemon learns the heat-up rate (C/h) of each heating zone. No device firmware starts a
block early.

- An episode starts when the zone target rises at least 0.5 C above `4060`. It ends when a
  report reaches the target. The daemon discards an episode that has any of these: an open
  window, an overlay, away, an off block, a target change, no report for `offline_after_sec`,
  no demand `40a0`, a length over 6 hours, or a rate outside 0.1 to 10 C/h.
- The model keeps the last 10 episodes and uses the median. With a fresh outdoor temperature
  it prefers the episodes nearest to that temperature. It is stored in the `meta` table.
- With at least `early_start_min_episodes` episodes, the daemon sends the `6200` of a warmer
  block `(target - temperature) / rate` before it starts, at most `early_start_max_lead_sec`
  before. It acts only at home, without an overlay and without an open window, and never on
  the hot water zone. It stores the running early start.
- `PUT /api/zones/{n}/early-start` with `{"enabled": true}` switches it on. It is off by
  default. `PATCH /api/install/zones/{n}` writes the same `early_start`.
- `GET /api/zones/{n}/early-start` and `GET /api/early-start` show the rate, the episodes,
  the last update and the next early start. `DELETE .../early-start/model` clears the model.

### Plant episodes and controller suggestion

The daemon records plant episodes for an offline fit of the valve controller. Each zone
report adds a sample with `4060`, `40a0`, `4160` and the fresh outdoor temperature.
`GET /api/zones/{n}/episodes` returns the last 20 episodes per zone and kind.

- A `heat` episode starts with the heating episode and continues after the target. It ends
  when `4060` stays within 0.2 C of the target for 30 minutes, or 2 hours after the crossing.
  It also ends on an open window, an overlay, away, a target change, no report for
  `offline_after_sec`, 6 hours without reaching the target, or 1440 samples.
- A `cool` episode covers a stretch of at least 30 minutes (at most 12 hours) with `40a0`
  at 0 and `4160` absent or 0.
- `GET /api/zones/{n}/pid-suggestion` fits a first-order model with dead time and suggests
  `kp`, `ki` and `kd` with the SIMC rule. `confident` is true with at least 3 `heat`
  episodes used and R^2 of at least 0.8. The route writes nothing. Gains change only with
  `PATCH /api/install/zones/{n}`.

## Devices

### Faults, battery and alerts

`GET /api/health` lists `devices` with `lowBattery`, `fault` (with `faultWord`),
`commsWarnings` and `offline`. `alerts` lists the devices with at least one alarm.
`/api/nodes` shows the same data as `LowBattery`, `Fault`, `FaultWord`, `CommsWarnings` and
`Offline`.

- The fault word `01a3` has two bits for the radio link: `0x02` (no IPv6 neighbour) and
  `0x80` (no answer to the `d/config` GET for 90 minutes, or since start). They appear in
  `commsWarnings` (`noNeighbour`, `noConfigAnswer`), not in `fault` or `alerts`. Any other
  bit sets `fault`.
- `low_battery_cell_mv` is the threshold per cell: 2200 mV for a valve (two cells) and
  3300 mV for the wall thermostat (three cells). The level is the highest `0162` reading of
  the last three hours. `lowBattery` turns on below the threshold and off above the threshold
  plus 50 mV per cell. `BatteryPct` is linear per cell, 0 % at the threshold and 100 % at
  1500 mV.
- A device is `offline` after `offline_after_sec` without a frame.
- `bridgePresent` turns true when the daemon hears a data frame with its own coordinator
  source address. The daemon then stops to transmit for the rest of the run, and
  `transmitAllowed` is false. This is a last safeguard. It does not replace switching the
  commercial bridge off.
- `zoneFallback` lists the zones that the wall thermostat reports in fallback (`6460`). The
  modes are `hold`, `fixed` and `frost`. The daemon records the report and never sends a
  fallback mode.
- `timezone` in `GET /api/health` names where tag `02b2` comes from: `name`, `offsetMin`
  and `fallback`. `name` is `fixed offset` for a set `utc_offset_min`, the IANA name of
  `timezone`, or the name of the zone of the host. `fallback` is `true` for the zone of the
  host. A container without `TZ` runs in UTC, so the devices get an offset of 0. The daemon
  then logs `options.timezone is unset and the host zone is UTC` at start. Set `timezone`
  or `utc_offset_min` with `PATCH /api/install`, or start the daemon with `TZ` set. The
  field exists while the daemon holds an installation.
- `GET /api/health` answers within three seconds. It answers `503` with `"ok": false` when
  the engine is busy (`"engine": "busy"`) or when the live radio loop ended (`radioError`).
  It reports `fsctrl0` and `calibrated` in live mode, and a `warning` that names the
  calibration command when none is stored.
- `serialErrors` in `GET /api/health` is the number of serial replies that the link to the
  bridge lost since the daemon started. It exists in live mode. A reply that stops short is
  discarded and the daemon reads the next one, so the radio frame inside it is lost. The live
  radio loop ends only when one kind of exchange, the queue poll or the receive window, failed
  every time for 30 s. A count that keeps growing points to the USB link: see
  [Serial link faults](OPERATION.md#serial-link-faults).

### Child lock, identify and calibration

- `POST /api/nodes/{id}/childlock` is for valves only. Another device kind gets `400`,
  because the wall thermostat has no `d/lock`. `ChildLock` in `/api/nodes` is `false` until
  the valve confirms a write, and `null` on a device that is not a valve.
- `POST /api/nodes/{id}/identify` makes a valve signal itself. It needs a valve that
  belongs to a zone.
- `POST /api/nodes/{id}/calibrate` starts the motor calibration of a valve. The daemon sends
  `PUT d/calibrate` and waits for the `2.xx` answer. A `2.xx` does not guarantee the stroke,
  because the valve drops the request in silence in some states. The result arrives in the
  `d/<serial>/mnt` report: `Telemetry` shows `calib_state` (1 started, 2 done, 3 failed),
  `calib_error`, `ctrl_1b5` and `ctrl_1b6`.
- `POST /api/nodes/{id}/factoryreset` with `{"confirm": true}` queues the network command 1007.
  The device leaves the network. It keeps its token. The physical button wipes its storage.
  The call changes nothing in the installation.

### Display

`GET /api/nodes/{id}/display` returns `orientation` (`horizontal` or `vertical`, tag `0149`)
and `notification` (tag `0158`, the notice of a remote change). `PUT` with
`{"orientation": "vertical"}`, `{"notification": false}` or both changes them. A missing
field stays as it is. The defaults are the captured values: horizontal, notice on
(`0158=0200`).

- The daemon writes the whole `d/config` under a new ETag `015a`. The valve answers `2.04`,
  rereads `d/config` and `z/N/config`, and homes its motor (a full stroke), as with the
  commercial bridge.
- With `"via": "dbg2"` the daemon writes only the changed records `0149` and `0158` in one
  `PUT d/dbg2/tlvs`. The valve does not reread and does not home. On the `2.04` the served
  document takes the new values under the ETag the valve knows. `"via": "config"` or no
  `via` selects the full write.
- `notification` changes only bits 9 and 10 of `0158`. A change that leaves the document
  unchanged writes nothing.
- The wall thermostat has no display settings and gets `400`.

### Node configuration

`POST /api/nodes/{id}/config` writes chosen tags of `d/config`: `tempUnitF`, `calibOffset`,
`traceEnable`, `lcdVariant`, `stateBits`, `etagHex`, `homeId`, `nodeTable`, `utcOffsetMin`
and `offlineScheduleEnabled`. A body with `reportIvlSec` gets `400`: the daemon never writes
tag `015d`.

`offlineScheduleEnabled` sets the value for one valve. It outranks `offline_schedule`.
`DELETE /api/nodes/{id}/config/offlineScheduleEnabled` removes it. The wall thermostat
accepts no value of its own.

### Interface module id

Tag `015d` of `d/config` is the interface module id of a device. Every valve gets 112.
The id of the wall thermostat selects the firmware image that the radio chip reflashes, so
freedo never invents it. The daemon learns the id from the `hvac/codes` report of the
thermostat or from a `d/config` document of the thermostat, and stores it.

- The daemon refuses 0, 112, `0x7fff` and every id that no known firmware family has. It
  learns the ids 202 and 1, 5, 6, 7, 9 with a warning, when another family reported before.
- At start the daemon drops a refused id from the database and continues. The thermostat has
  no id until it reports again.
- `/api/health` lists refused and dropped ids and warnings in `interfaceModuleIssues`, and
  the thermostats without an id in `interfaceModuleUnknown`.
- Until the id is known, the `d/config` GET of the thermostat gets `2.05` with the
  installation document without `015d`, and the daemon does not keep that document.
- `GET /api/boiler` reports the bus family in `BusType` and the id in `InterfaceModule`.

### Firmware state and transfer

- `GET /api/nodes/{id}/fw` queues a GET of `d/fw/state` for the next CSL contact. A `2.05`
  answer appears in `result` and, decoded, in `decoded`. The version is `major<<6 | minor`.
  The status nibble is: bit 0 pending, bit 1 image, bit 2 active, bit 3 confirmed. Valves and
  thermostat both report type 8, so the type does not tell the hardware.
- `POST /api/nodes/{id}/fw` starts the transfer of an image to one device. The body needs
  `confirm: true`, `version` (M.m), `imageKind` and `image` in base64. The daemon refuses the
  request in dry-run, after it heard the commercial bridge on air, and when the last
  `d/fw/state` is older than 5 minutes.
- The daemon sends one block at a time (128 bytes, `POST d/fw` with Block1) and repeats a
  block up to 3 times. Other commands for the device stay queued meanwhile. After the last
  block it waits for the `PUT d/<serial>/fw/state` of the new image, answers `2.04`, reads
  `d/fw/state` and checks the confirmation. `GET` shows the progress under `transfer`.
  `DELETE` cancels it.
- The project does not distribute images. No transfer ran on air.

## Boiler

### Hot water

`POST /api/boiler/dhw` sets a hot water temperature overlay with the same fields as
`POST /api/zones/{n}/setpoint` (`durationSec` included). It writes the wall thermostat. It
answers `503` while the installation has no hot water zone.

`GET /api/boiler` shows the hot water temperature as `DHWC` and the hot water setpoint as
`DHWSetpointC` (tag `045b` of `hvac/mon/dhw`). `DHWSetpointC` is the value that the wall
thermostat reports, in C. The daemon does not set it and it is not a target: the target of
the hot water zone is `declared[].target` in `GET /api/zones`. It is `null` until the
thermostat reports it, and while the boiler lists it as unsupported. One capture shows
53.00 C 19 s after a write of 53.00 C. No capture shows the value of a zone that follows a
program, so it is not verified that it follows the program.

### Boiler thermostat

The boiler thermostat is the wall thermostat wired to the boiler: the thermostat of the hot
water zone, or the heat source of the circuit with the lowest id while the installation has no
hot water zone. `GET /api/heatsource/capabilities` reads it, and `POST /api/boiler/flow`
writes it when the body has no `target`. Both answer `503` when the daemon finds no such
thermostat.

### Flow limit

The wall thermostat reads the flow limit (tag `2040`) from `h/<home>/c/<N>/config` and
applies it as the OpenTherm MaxTSet, in whole degrees, only when it opens the boiler
session again. Each application closes and opens the session.

- `flow_limit_c` has no default. A circuit gets `flow_limit_c` when it is set, unless
  `POST /api/boiler/flow` set a value, or a curve, the default curve or the adaptation manages
  the circuit. While `flow_limit_c` is unset, a circuit that nothing manages is served no
  `2040`. The code notes that the thermostat then applies a MaxTSet of 70 C when its boiler
  interface starts. No measurement on a boiler confirms this value. Set `flow_limit_c` to a
  value that suits the boiler.
- A circuit with an enabled curve, or with the adaptation on, falls back to 48 C while
  `flow_limit_c` is unset. The fallback applies to a stale outdoor reading and to the start of
  the adaptation.
- The window is 30 to 75 C, and `POST /api/boiler/flow` accepts the whole window. The daemon
  serves the lower of the requested limit and the highest CH setpoint that the boiler
  reports (tag `0471`, OpenTherm id 49). It applies the bound only when the bound is from
  30 C up to below 75 C. A new bound that changes the served limit also moves the ETag.
- The thermostat rereads `c/<N>/config` only after a `2.05` of `d/config`. For this the
  ETag of its `d/config` hashes the document and the served flow limits. A new limit
  changes the ETag and queues one empty `PUT d/config`. The thermostat rereads within a few
  seconds.
- `flow_curves` holds one weather curve per circuit: two points (outdoor temperature to
  flow limit), `min_c`, `max_c` and `enabled`. It is off by default. `POST /api/outdoor` with
  `{"tempC": n}` gives the outdoor temperature. A reading older than `outdoor_max_age_sec`
  returns the circuit to `flow_limit_c` (48 C while that option is unset).
- The served limit changes by at least `flow_hysteresis_c` and at most once per
  `flow_min_interval_sec`. The interval also starts at daemon start.
- A circuit with an active curve of its own refuses `POST /api/boiler/flow` with `409`. An `id` that the
  installation does not declare gets `400`.
- `GET /api/boiler` shows under `flowLimit` the outdoor temperature and its age, `defaultC`
  (`null` while `flow_limit_c` is unset) and, for each circuit, the computed and the served
  limit, the lowest and the highest limit it accepts (`minC`, `maxC`), the time of the last
  change and whether the thermostat read it. `servedC` is `null` while the circuit is served
  no limit.

`flow_default_curve` (default `false`) gives every circuit without an enabled curve of its own a
built-in weather curve, so that you do not define two points. The curve is a fit on the monthly
figures of one heating season on one plant with radiators and a condensing boiler. It is not the
algorithm of the tado cloud, which is not public.

- The curve is relative. The limit is `flow_limit_c` minus a reduction. The reduction is
  `3.2 + 0.33 x outdoor` (outdoor temperature in C), rounded to whole degrees and clamped to 0 to
  8 C. The limit never goes above `flow_limit_c` and never below 30 C.
- The line is the least-squares fit through seven monthly means, from 3 to 21 C outdoors. A monthly
  mean hides the daily swing, so the true curve can be steeper than the line. Below 3 C the line is
  an extrapolation. It gives a reduction of 0 from about -9.7 C down, so a cold day gets
  `flow_limit_c`. The cap of 8 C applies from about 14.5 C: the figures show 8 C at 19 to 21 C and
  8 to 9 C from 13.6 C.
- For `flow_limit_c` 56 C, the limit is 54 C at -5 C outdoors, 52 C at 3 C, 49 C at 10 C and 48 C
  at 16 C and above.
- The option needs `flow_limit_c`, the reference of the reduction. A `PATCH /api/install` that
  switches it on while `flow_limit_c` is unset gets `400`. Send both in one request, or set
  `flow_limit_c` first.
- A circuit with an enabled curve of its own follows that curve. The default curve does not apply
  to it.
- The outdoor temperature comes from `POST /api/outdoor`. Home Assistant must feed it: the
  integration pushes the sensor that you select in its options. With no reading, or a reading older
  than `outdoor_max_age_sec`, the circuit returns to `flow_limit_c`, as with a curve of your own.
- The curve uses the same rules as the other curves: `flow_hysteresis_c`, `flow_min_interval_sec`,
  the cut to `maxC` and the single writer of tag `2040`. A new `flow_limit_c` reaches a circuit at
  its next allowed change.
- With `flow_auto_adapt` on, the curve value is the ceiling and the adaptation trims below it.
- `POST /api/boiler/flow` answers `409` for a circuit that follows the default curve. Switching the
  option off returns the circuit to `flow_limit_c`.
- Each item of `flowLimit.circuits` also shows `defaultCurve` (`true` while the circuit follows the
  default curve) and `reductionC` (the reduction in force now, `null` while the outdoor reading is
  missing or stale or the circuit does not follow the curve). `computedC` is the limit that results.
- The fit is not verified on a plant. No test on air covers the curve.

`flow_auto_adapt` (default `false`) switches on an automatic adaptation of the limit of each
circuit. It is a local controller. It is not the algorithm of the tado cloud, which is not
public.

- The served limit is `min(adapted, ceiling)`. The ceiling is the limit the daemon serves
  without adaptation: `flow_limit_c` (48 C while it is unset), or the value of the curve or of
  the default curve while the outdoor reading is fresh.
- The adapted limit starts at the served limit, or at the ceiling while the circuit is served
  none.
- It moves down by 2 C (minimum 40 C) when 12 hours passed since the last move, the circuit
  called for heat at least 2 hours in that time, and no room is behind.
- It moves up by 4 C (up to the ceiling) when a room with at least 90 % demand stays at least
  0.5 C under its target and gains less than 0.2 C in one hour of reports. A new rise needs
  another hour of reports.
- A zone report older than `offline_after_sec`, or a zone without a target, stops the descent.
  The controller reads only `4060`, `40a0` and the zone target. It does not use boiler values.
- The adapted limit survives a restart (`meta.flow_adapt`). The collected evidence restarts,
  so the first descent after a restart waits 12 hours again.
- With the adaptation on, `POST /api/boiler/flow` answers `409`. Switching it off returns the
  circuits without a curve to `flow_limit_c`, or to no limit while it is unset.
- Each item of `flowLimit.circuits` also shows `adapt`, `adaptedC`, `adaptedAt` and
  `adaptReason` (`start`, `satisfied`, `short`).
- The constants are design estimates. No test on air covers the adaptation.

### Energy and gas estimate

The daemon estimates the heat that the boiler delivers. Home Assistant holds the gas price.

- The estimate needs a boiler that reports its modulation over OpenTherm. A relay boiler has
  no estimate yet.
- The wall thermostat reports the relative modulation (`0452`) and the burner flag (`0457`) in
  `hvac/mon`. With the burner on, the power is
  `min + (max - min) x modulation / 100`. With the burner off it is 0.
- The estimate follows one thermostat: the heat source of the circuit with the lowest id.
  Without a circuit it follows the only thermostat that reported `hvac/mon`, and it follows
  none while several thermostats report. The samples of two boilers never mix. `reason` is
  `noReport` until that thermostat reports.
- `boiler_max_power_kw` and `boiler_min_power_kw` hold the output at 100 % and 0 % modulation.
  Without the maximum the estimate stays off.
- The thermostat sends only changed attributes. The daemon holds the two values until the
  next report. Each interval between two reports counts at the power of the first. An
  interval over 300 s adds no energy, because a longer silence means a lost link or a stopped
  thermostat.
- The counter is in kWh and never decreases. It is stored in `meta.boiler_energy_kwh`, at
  most once per minute and at shutdown. Downtime adds no energy.
- The counter is a total. The thermostat does not report whether the burner heats rooms or
  hot water.
- The estimate has three error sources. The modulation is relative to the working range of
  the boiler and does not measure power. The efficiency is not modeled, so the counter is
  delivered heat and the burned gas is higher. The samples are sparse, so a short cycle is
  approximate.

`GET /api/boiler` reports it in `energy`: `running`, `reason` (`null` while running, else
`noMaxPower`, `noReport`, `stale`, `noBurnerFlag` or `noModulation`), `powerKW`, `totalKWh`,
`maxPowerKW`, `minPowerKW`, `holdSec` (300), `reportedAt`, `updatedAt` and `gas`.

The fuel counter in `energy.gas` counts the burned fuel in `fuel_unit` and needs no
efficiency. `fuel_unit` is `m3` (natural gas, the default), `l` (heating oil) or `kg` (LPG or
pellets). The `M3` in the names of the fields stays for every unit: `boiler_max_gas_m3h` and
`boiler_min_gas_m3h` hold the fuel flow at 100 % and at 0 % modulation with the burner on, in
`fuel_unit` per hour. `flowM3H`, `totalM3`, `maxFlowM3H` and `minFlowM3H` of `energy.gas` hold
flows and totals in the same unit, and `energy.gas.unit` names it. The daemon does not convert
a stored counter when `fuel_unit` changes. The data sheet of the boiler lists the flows. The
daemon has no default. The limit is 60 in each unit per hour, so a value in a smaller unit is
refused (500 kW burns about 53 m3/h of natural gas, 50 l/h of heating oil or 40 kg/h of
propane). The maximum is above 0 and the minimum is from 0 up to below the maximum. The count
needs both values and does not depend on the power values. It uses the same cycle, the same
300 s limit and the same sample order as the heat counter, and it is stored in
`meta.boiler_gas_m3`.

Example, not a default: a 24 kW condensing boiler on natural gas lists 2.63 and 0.30 m3/h, so
`{"boiler_max_gas_m3h": 2.63, "boiler_min_gas_m3h": 0.3}`.

It is not verified which maximum the relative modulation refers to (heating or hot water),
and the thermostat does not say which service the burner provides. If the hot water maximum
is higher than the heating maximum, a hot water draw can read up to about 15 % low.

`energy.gas` has `running`, `reason` (`noGasFlow` replaces `noMaxPower`), `flowM3H`,
`totalM3`, `maxFlowM3H`, `minFlowM3H` and `unit`. `GET /api/boiler` also shows the boiler counters
`BurnerStarts`, `BurnerHoursMin` and the group `Maintenance`. A value that the boiler
declares unsupported (tag `0481`) is `null` and listed in `Unsupported`.

## Pairing

`POST /api/pair/window` with `{"seconds": 120}` opens the pair window. The maximum is 600.
`{"seconds": 0}` closes it. `GET /api/pair/window` shows `open`, `opened`, `until`,
`responder` and `joiners`. `POST /api/pair` with `{}` pairs the single device in `joiners`. With several
devices, or without an open window, pass `{"target": "<short>"}`. A target outside the
installation must appear in `joiners`. `networkKeyHex` replaces the network key of the
installation for this call.

The full procedure is in [OPERATION.md](OPERATION.md#pairing-a-device). Pairing has not been
verified on air with the current firmware.

## Unhandled log

The daemon appends one line to `unhandled_log` for each radio event it does not fully
handle. A line has six tab-separated columns: time, frame class, source, destination, reason
and frame. `GET /api/unhandled` returns the counts by reason.

- The frame column holds the bytes on air in hexadecimal. The network key encrypts them and
  stays in the database. A data frame that opens with the public pairing key shows only the
  16 header bytes, because the `d/pair` command carries the network key.
- The reason of an authentication request lists the payload tags. A secret tag shows only its
  length: `0007` (device token), `0260` (serial), `025e` (session id), `0261` and `0262`
  (challenge keys).
- The log still holds device labels or serial numbers, the clear header of each frame (source
  and destination EUI, sequence number), the CoAP path of a request (which can hold the serial
  and the home id) and the value of non-secret tags.

Remove or replace serial numbers and the home id before you attach the log to a report. The
file only grows. Rotate or delete it from time to time.
