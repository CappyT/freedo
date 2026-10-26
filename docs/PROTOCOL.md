# Protocol facts

This file records what is known about the radio protocol of the supported devices and
how freedo uses it. It is the reference for developers.

Sources, in order of trust: physical captures, device firmware, the code, this file.
When two sources disagree, the higher one wins. A statement here that no capture or
firmware read supports says so.

The captures behind these facts are passive recordings of a commercial coordinator
that drives a real plant. They are not part of the repository, because they hold
device identifiers and keys. The text names them by what they show:

- the heat-demand capture: a long passive recording with heat demand, overlays and offset;
- the app capture: a recording made while a person drove the commercial app;
- the away capture: a recording of home and away switches with away temperatures;
- the overlay capture: a recording of the five overlay end rules set through the cloud API;
- the pairing capture: a recording of two valves that join the network;
- the long watch captures: recordings of many hours with firmware transfers;
- the 15-minute aligned capture: a recording of burner cycles.

Test names in parentheses point to the code that guards a statement. Firmware names and
addresses come from a static reading of the device images. They support a statement
and are not an interface.

### Heat demand chain

Tags `6340` and `6400` never appear on air. The reporter election travels in
`d/config` tag `015e` = `[role bitmask][zone id]`. A reporter reads `0d0a`
(role `0x0d`). A plain member reads `0x05`, so bit 3 marks the elected member.
Tag `6060` is the zone to circuit binding. It lives in `h/<home>/z/N/config`,
not in `z/s`.

The chain:

- The valve computes `40a0`.
- The valve publishes it on `h/<home>/z/N/act`. The zone owner also sends it to
  the wall thermostat in a 75-byte `2.05`: the notification of the thermostat's
  observe of the owner's `z/p`.
- The thermostat aggregates it into `h/<home>/c/1/act` (`4080` circuit demand,
  `2090` heat active).
- The thermostat pushes `z/cpe?lid=N` with `4160` (heat percent, the same
  circuit value for all zones).
- The thermostat drives the boiler and reports `h/<home>/hvac/mon`.

The thermostat takes the demand of a zone from the owner alone. `zone_to_circuit_demand_update`
(thermostat 0x2debc) aggregates the circuit demand from the `40a0` of the owner's `z/p`
observe notifications, and from a member's `PUT z/p` when the thermostat is itself the
owner. It never consumes a member's `h/<home>/z/N/act`. The observe works this way:

- `coap_observe_manager_fsm`@0x28df4 sends a CON GET of the URI in tag `6040` of the zone
  document, with a fresh token of up to 8 bytes and an empty Observe option (option 6, value
  0: register).
- `coap_uri_parse`@0x1d920 takes a path of at most 4 characters (`/z/p`) and a path and
  query of at most 9. A URI with `?lid=N` fails to parse, and the GET never goes out. With
  the coordinator as owner, freedo writes its `z/p` with no query in `6040`.
- `coap_observe_notify_handler`@0x29048 takes a 2.xx that carries the Observe option as a
  notification. It parses the body, sets the deadline of the slot to the Max-Age plus 15 s
  (60 s with no Max-Age), and the zone leaves its fallback.
- A 2.xx with no Observe option ends the observation. The handler does not parse the body,
  the zone goes to its fallback, hold or frost (`zone_mode_validate`@0x31498,
  `zone_demand_apply`@0x316cc), and the thermostat GETs again about every 121 s, with no
  end. On the plant (2026-10-26, coordinator as owner of one zone, plain `2.05` answers) the
  GET came back every two minutes, and the thermostat held 100 % for 25 minutes.
- When the deadline lapses, the thermostat only GETs again under a new token. The demand it
  stored stays until a new notification arrives.
- The thermostat reads `h/<home>/z/N/config` again, a conditional GET with the ETag, after
  each of its `d/config` polls: hourly, or within seconds after a write to its own
  `d/config`. It accepts the coordinator as owner.

As owner, freedo answers the registering GET with an ACK `2.05` that carries Observe (a
sequence number above 0) and Max-Age 1800, and registers the requester and its token. A new
registration of the same requester replaces the old one. After that it sends a NON `2.05`
under that token, with the next sequence number, Max-Age 1800 and the measure, each time the
measure changes and at least every 10 minutes. A GET with no Observe option gets the plain
`2.05` with Max-Age 1800. A change of the owner sends the thermostat that is the heat source
of the zone its re-read trigger (`TestZoneMeasureObserve`, `TestOwnerChangeRereadsHeatSource`).

Tag `2040` never appears in these frames.

`tado/demand.go` models the chain and `tado.Snapshot.Zones` exposes it. The
three phantom tags survive only as opt-in commissioning fields of
`SetpointCoAP`. `tado/config.go` accepts a zone that elects no reporter.

### Setpoint write path

Every `z/s?id=N` write that the coordinator authors is a CoAP CON 0.03. It has
Uri-Port `ffff`, Content-Format 42 and a token of 1 to 8 random bytes. The
token length has no meaning. The body holds the tags in the order
`6160 6180 6020 61e0 [6200] 6240 [6260] [6280] 62e0 [6440]`. The bracketed
tags are optional. Tag `6180` reads 0 in every frame.

Home and away is one state of the whole home. Tag `6160` carries it (1 home, 2
away). Tags `61e0` and `6200` carry the setpoint that the zone follows while
`6240=0`. At home, every write carries `61e0=1` and `6200`, the return to
schedule included. `6200` is the setpoint that the weekly program holds at that
moment (20.00 C on a room zone and 55.00 C on the hot water zone in the
captures). The switch to home writes all seven zones within 23 s. It leaves
the overlay tags as they were.

In away, a room zone follows its away temperature. This is a setting of each
zone: the app writes `awayConfiguration` with `minimumAwayTemperature`, 5..25 C,
default 5 C. Above the default, the zone writes `61e0=1` and the temperature in
`6200`, in the same tag order as at home. At the default, it writes `61e0=0` and
no `6200`: it is off in away. The device needs no away-specific tag. The away
capture (cloud coordinating, passive receive, away temperatures 12, 15 and 18 C
on three zones, the other room zones at the default) shows:

- The switch to away writes one frame per zone: all seven within about 20 s, and
  a sleeping valve later. A room zone with no overlay gets
  `6160=2 6180=0 6020 61e0 [6200] 6240=0 62e0 6440=0001`. The device then
  re-reads `z/s?lid=N` and gets the same head.
- The switch to home writes `6160=1 61e0=1 6200=<program> 6240=0 62e0 6440=0001`
  to the same zones.
- An away temperature change while away writes that zone alone, within 2 s, with
  the same head and the new `6200` (`6440=0001` included). The same change at
  home puts nothing on air.
- The room zone with an away temperature of 18 C carries no `6440` in either
  direction. Its program setpoint also reads 18.00 C, so both writes keep it on
  at 18.00 C. The other five room zones carry `6440`.
- The circuit lead zone target on `h/<home>/c/1/act` reads 20.00 C at home and
  18.00 C in away, the highest away temperature of the capture.

The app capture holds the switch to home of zones that carry a manual overlay at
5.00 C (`6240=2 6260=1 6280=500`, no `6440`). The return restates that slot as it
was. A room zone whose overlay the user cancelled in away reads and takes
`6160=2 61e0=0 6240=0` with no `6260` or `6280`. No capture holds a room overlay
set in away.

Tag `6440=0001` marks the start or the end of an overlay: a manual set, a
cancel, a return to schedule. A write that updates a running overlay omits it.
These writes omit it: a timer arm, the hot water timer, the `62e0` write of an
open window, and the write of a home or away switch to a zone that runs an
overlay.

One rule fits all 16 distinct `z/s?id=N` writes of the away capture (7 to away,
1 away temperature change, 1 open window, 7 to home). The rule applies to a
switch write or an away temperature write to a room zone with no overlay.
`6440=0001` appears when the write changes the effective target of the zone: its
power (`61e0`) or its temperature (`6200`). 11 writes change the target and carry
it. 5 writes keep the target and carry none: the 18 C zone in both directions,
hot water under a manual overlay in both directions, and the open window write of
a zone at 12 C. This rule is inferred from that capture. It is not read from
firmware. The target before the switch to away is observed on one zone only
(20.00 C at home). The other zones take the program setpoint of the return write
5 minutes later.

freedo writes these shapes. The away temperature is an install row of the zone
(`away_temperature_c`). freedo sends it on the valve scale, like every setpoint.
A switch or an away temperature change goes out through `EnqueueZoneSwitch`. This
function adds `6440` to a room zone with no overlay when the new `61e0` and
`6200` differ from the held ones, compared on the wire. The held target is the
head of the newest write queued to the writer, else the head in the zone cache.
An unknown held target counts as a change. A schedule block change goes out
through `EnqueueZoneRewrite` and carries none, because no capture shows a block
boundary. Reads of `z/s` restate the same head. Preheat before arrival has
no tag on this plane.

A timer is `6240=3` (`until_next`) with the setpoint of the overlay. No duration
travels on the air and no `z/activity` exchange appears. The coordinator keeps
the deadline and writes `6240=0` when it expires. A 30 min timer on a room and a
4 h timer on hot water carry the same body.

The `6240` values come from the overlay capture. The app API set five overlays
on zone 10 in a row. Each bridge write on `z/s?id=10` carried `61e0=1`, `6200`
(the program setpoint), `6260=1`, `6280` (the overlay setpoint) and `6440=1`. The
delete carried neither `6260` nor `6280`:

| 6240 | app termination |
|---|---|
| 0 | overlay deleted, back to auto |
| 1 | `TADO_MODE`, until the next automatic change |
| 2 | `MANUAL` |
| 3 | `TIMER` (600 s) and `NEXT_TIME_BLOCK` |

The app API answered a `TADO_MODE` overlay with `termination.type = TADO_MODE`
and a `projectedExpiry` equal to the next schedule block change of the zone. It
answered `NEXT_TIME_BLOCK` with `type = TIMER`. These answers are not in the
radio log. From them, `TADO_MODE` ends at the next block change or at the next
switch between home and away, whichever comes first. A timer and a next block
share 3, because no deadline goes on the air. freedo writes the rule
`until_auto_change` as `6240=1`. It arms a timer for the next block (none
without a program). It ends the overlay at the switch between home and away with
the write of an elapsed timer. The capture holds no end of a `TADO_MODE`
overlay, so that end write is the timer expiry, not a read value. The capture is
a room zone: hot water takes `6240=1` by analogy.

A manual change on a valve (the wheel) is a device overlay. The valve sends
`CON PUT h/<home>/z/N/overlay` with `6280` (the new setpoint), `6240=2` and
`6260=1`, and no `6200`. The coordinator answers `2.05` with no body. About 3 s
later, the valve sends `z/s?lid=N` with the overlay block: `6280` is the overlay
setpoint and `6200` is the scheduled setpoint. The coordinator relays it to the
other members. The body does not change with the end rule. The end rule is a
room setting of the cloud (manual, next block, timer from the PUT time). At
expiry, 1 s after the cloud deadline, the coordinator writes `z/s?id=N` to the
zone writer with `6240=0`, the program `6200`, `61e0 6160 6180 62e0` and no
`6440`. The device answers `2.04` and echoes `z/s?lid=N`. A cancel from the app
writes the same block with `6440=0001`.

freedo keeps the rule per heating zone (`device_overlay_end`, default
`until_next`) and the deadline in the overlay timers. No capture holds the wall
thermostat buttons or the commercial default rule of a new room. A zone write
that is queued before a device overlay and not yet on the air restates the older
block and would undo the overlay. It gives way to one rewrite of the overlay per
node, with the head and the `62e0` of now. A queued write with `6440` or a zone
configuration tag stays (`restateDeviceOverlay`,
`TestDeviceOverlayRestatesQueuedZoneWrites`).

The elected reporter sends `DELETE h/<home>/z/N/overlay` with no body when the
`6240` of the zone goes to 0 on the device. The builder picks DELETE on `6240=0`
and PUT with the block otherwise (valve 0x11d1c, thermostat 0x11a94). The
response handler (valve 0x117d0) clears the flag on any code. No capture holds
the DELETE. freedo answers `2.05`, writes `6240=0` into the zone block and the
reporter's block, drops the overlay timers of the zone, and restates the queued
zone writes in overlay auto (`endDeviceOverlay`,
`TestDeviceOverlayEndRestatesQueuedZoneWrites`, `TestDeviceOverlayEndedOnDevice`).

A room off overlay carries `6260=0` and no `6280`. The capture of the app action
"off until the next time block" holds
`6160=1 6180=0 6020=1 61e0=1 6200=07d0 6240=3 6260=0 62e0=0 6440=0001`. The valve
answers `2.04` and republishes `z/s?lid=N` with `6260=0`. freedo writes the same
shape with `6240=2` for a manual off. No capture holds that variant. Zone
rewrites and `62e0` writes restate `6260=0` from the zone cache.

A rewrite behind a write that the node has not taken yet restates that write.
Behind a queued return to schedule, the rewrite carries `6240=0` and no `6260`,
even while the cache still holds the off overlay (`restatePending`,
`TestRewriteBehindReturnFromOffReachesZone`).

On a zone that the window engine does not drive, every write keeps the `62e0` of
the newest write queued ahead of it, an open-window write included. Without a
queued write, it keeps the value that the node holds. A driven zone takes the
value of the engine (`zoneOpenWindow`, `TestZoneWritesKeepOpenWindow`).

The daemon restates a zone, after a change of its correction, from the block a
queued write sets before the one the zone holds. A new overlay behind a queued
return to schedule carries `6440` (`PendingZoneBlock`,
`TestRestateKeepsQueuedReturn`, `TestSetpointBehindReturnStartsOverlay`).

Hot water is zone 0. The coordinator writes it to the wall thermostat with the
same block: `6020=2`, `6200=5500`, overlays 0, 2 and 3, and `6260=1` with `6280`
when an overlay is set.

Hot water follows home and away like a room zone. In away, a hot-water zone with
no overlay reads and takes `6160=2 61e0=0 6240=0`. The thermostat takes the DHW
enable from `61e0` and turns hot water off.

A manual overlay keeps `6240=2 6260=1 6280` under the away head. The cloud away
read of zone 0 shows this (`6160=2 61e0=0 6240=2 6260=1 6280=48.00`, heat-demand
capture). The enable comes from `6260`, so hot water stays on. The cloud write of
the same state in the away capture reads
`6160=2 61e0=1 6200=38.00 6240=2 6260=1 6280=48.00` with no `6440`.

A hot-water zone without an away program takes `61e0=0` and no `6200` from
freedo in away. The away burst restates the overlay. A block change of its home
program waits for the return home. The return home writes the block in force with
the overlay (`TestHotWaterFollowsAway`, `TestHotWaterOverlayHoldsThroughAway`).

A hot-water zone with an away program takes the block of that program in away:
`6160=2 61e0=1` and the block setpoint in `6200`, with no overlay, and `61e0=0`
with no `6200` for an off block. Each block change of the away program writes the
zone (`TestAwayProgramHotWater`). No capture shows this shape: the cloud writes
`61e0=1` with `6200` on hot water in away only under a manual overlay, as above.
It is not verified that the thermostat keeps hot water on at the `6200` of a write
with `61e0=1` and no overlay.

The thermostat reports the hot water setpoint as tag `045b` on `h/<home>/hvac/mon/dhw`, in
centi-degrees. The capture of the 53.00 C write shows `14b4` 19 s later. freedo shows the
value as `DHWSetpointC` of `GET /api/boiler` and never writes it.

A valve that is resyncing its configuration answers `5.03` to a `z/s` write, and
the coordinator retries. `5.03` is a device-busy answer. The answer closes the
CoAP exchange. So freedo retries every write after a 5.xx under a new MID and
token. A resend under the same MID could read the answer back from the
deduplication state of the device (`remintDownlink`,
`TestServerErrorRetryMintsNewIDs`). The retry never goes out in the rendezvous
that carried the 5.xx. It waits for the retry deadline of the daemon on an
explicit dispatch, else 2 s (the CoAP `ACK_TIMEOUT`). Each 5.xx costs one try
(`TestServerErrorWaitsForTheRetry`).

Every coordinator `z/s?id=N` write restates the whole block. A newer write to the
same zone on the same device replaces a queued write that is not on the air. So a
device that is off the air takes the newest state only. A queued write with
`6440` or a zone configuration tag (`6060`, `6340`, `6400`) stays when the newer
write lacks it, because those tags mark an event (`supersedeZoneWrites`,
`TestSilentDeviceKeepsOneZoneWrite`). The same holds for `d/config`: the newest
document replaces the queued ones, because each write homes the motor of the
valve. Child lock, calibration, `d/dbg2/tlvs` and firmware writes all go out in
order.

The byte-exact fixtures hold the away shape of a zone at the default alone:
`6160=02`, `61e0=00`, no `6200`, and a return-to-schedule downlink (`6240=0`)
that omits both `6260` and `6280`. `BuildSetpointCoAP` and `tado/command.go`
reproduce those two bodies byte for byte (`TestBuildSetpointCoAPMatchesCaptureBlock`
in `tado/build_coap_test.go`). A delivered write stores in the zone cache the
head that it carried, home or away, with `6200` after `61e0`
(`TestZoneReadsRestateTheHomeState`).

freedo builds both states and keeps the timers. `SetHomeState` gives the
emulator the home flag and the `6200` of each zone. The daemon evaluates the
weekly programs on its own clock and pushes `6200` at every block change. A write
with a duration arms the coordinator-side deadline that queues the return to
schedule.

The deadline belongs to one zone on one node (`<wire EUI>|<zone>` in the stored
state). The wall thermostat holds the hot-water deadline and the room deadline
apart. A write that ends one overlay leaves the other deadline in place
(`TestOverlayTimersPerZone`). A stored deadline keyed by the node alone restores
on the zone that it names. A deadline ends when the node takes the write that
ends its overlay: the return to schedule, a mode, or an overlay with no duration.
Until then the node still runs the overlay. So a tick that finds a write to the
zone queued adds none. A return that the retry limit or a 4.xx drops leaves the
deadline due for the next tick (`TestDroppedReturnKeepsTimer`,
`TestModeEndsTimer`). A reload that removes a zone drops its deadlines.

The node cache holds one heating block per node and records the zone that it came
from. A write to another zone of the same node restates the zone cache instead.
The wall thermostat reports only `z/s?lid=1` in every capture, so the guard
changes nothing on the captured traffic (`TestThermostatBlockKeepsItsZone`).

Only the 1 s tick writes a block change. A command before the tick leaves the
change owed to every other zone. The block that each zone took is stored in meta
`program_blocks`. The first tick after a restart writes once each zone whose
program moved while the daemon was down
(`TestHandlerInBoundaryGapKeepsBlockChange`,
`TestRestartAcrossBoundaryRewritesDueZones`).

The baseline moves when a rewrite is queued, and the queue is not durable. So
every zone rewrite (`rewriteZones`, `restateShiftedZones`) also records its member
in meta `program_owed`. The emulator numbers each zone block write that it queues
(`ZoneWriteMark`). A 2.xx to that write, or to a newer zone block write to the
same member and zone, settles it (`ZoneWriteDelivered`). The first tick after a
restart queues each owed write again, once, with the state then in force. A
restart with nothing owed writes nothing (`TestRestartBeforeDeliveryKeepsBlockChange`,
`TestRestartBeforeDeliveryKeepsAwaySwitch`). A running early start is stored in
meta `early_starts`, so a restart before the boundary keeps the early block
(`TestRestartDuringEarlyStartKeepsBlock`). The three keys are saved in one
transaction.

Every write takes `6200` from the program that the daemon pushed for the zone,
else from the zone cache, never from the node, because the wall thermostat sits in
two zones. With neither, a home write leaves `6200` out and the device keeps its
own (`homeBlock` in `tado/emu.go`). A zone with no program has no block change: freedo
writes it at a switch between home and away and at a command, never at a block change or at
the start (`TestZoneWithoutProgramWrites`). A delivered write records in the node and in
the zone cache the `6200` that it carried. A write without `6200` leaves both as
they were (`sentSchedC`, `keepZoneSchedC`). An open-window write to a node with no
known block waits for a `6200`.

### Session id

CoAP option 2048 carries a session id of 8 bytes. The `025e` TLV of the
`auth/token` or `auth/key` 2.05 answer assigns it. A device stamps the value that
it received on every message that it authors, until the next session
initialisation. The `auth/token` answer carries no option at all.

The coordinator forwards a zone-plane message byte for byte and never adds option
2048. In the app capture and two long watch captures, all 1444 relayed pairs (623
requests, 821 answers) are identical on both hops. A relayed request carries
option 2048 only when its author stamped it (75 of 623). No relayed request gains
it. The same holds for a relayed empty ACK or RST: RFC 7252 allows no option in an
empty message, and the bridge relays it as the same 4 bytes. freedo forwards
`CoAPMessage.Raw` unchanged (`buildZoneHop`).

A request that the coordinator itself authors carries no option 2048 either: every
`z/s?id=N` downlink in `tado/testdata/frames_69ec.txt` holds options 7 and 12
alone. The one captured `d/pair` is the exception. It holds a single-use value
that matches no session id.

Option 2048 is above every standard option, so it always encodes last, right
before the payload marker.

The cloud serves a new `025e` on each `auth/token` exchange. In the pairing
capture, one valve gets four different ids in four exchanges. Each retransmission
of one exchange gets the id of that exchange. freedo derives the id from the wire
EUI of the device, so a device always gets the same id.

freedo stores the session id that it served, and when, in `meta.sessions`. After a
restart, a device whose requests echo that id reads as adopted, without a new
`auth/token`.

### Weekly schedule

The document is nanopb: a `Schedule` of `DayProgram` of `Entry`. A program
carries the day mask on field 1 and its entries on field 2. An entry carries the
minute of day on field 1, the setpoint in centi-degrees as a signed varint on
field 2, and the enabled flag on field 3. The device copies field 3 verbatim to
zone tag `61e0` (power on). A value of 0 selects the 5 C fallback. The API
`enabled` value is field 3. `EncodeSchedulePrograms` writes the document and
`DecodeSchedule` reads it back. freedo serves the document on
`h/<home>/z/N/sched` and `d/<serial>/sched`, with Block2 of 128 bytes and an
ETag derived from the document.

`EmuState.SetSchedule` takes one or more day programs. These rules apply:

- Every mask selects at least one day.
- Two masks share no day.
- The masks together cover the seven days.
- Every program starts at minute 0, and its minutes increase strictly up to 1439.
- Each program encodes to at most 255 bytes.
- The whole body stays within 0x719 bytes.

The device re-encodes each day program into one config slot per day
(`0x29a`+day, `sched_program_write`) and rejects a blob over 255 bytes. The
check counts the day mask field, which the device may not keep in the slot.
`PUT /api/zones/N/schedule` validates the programs and writes the same document
to every zone member.

A device fetches the weekly program only when its `d/config` tag `02b3` is 1. It
then GETs `h/<home>/z/N/sched` (Block2 128, ETag) at boot, 24 h after a 2.05 or
2.03, and 10 min after an error. Each 2.05 of `z/N/config` (event `0x16d`) also
zeroes the held ETag and arms a full fetch. The request build (valve `0x2a1f0`)
runs only while the byte at `0x200082f8`+`0x38` is 1. The init at `0x2a52c`
loads it from cfg `0x2b3`.

The device serves `d/sched` with POST only. Its dispatch node (init image at
valve `0x3ad8c`, thermostat `0x5caa4`) holds a POST handler and no other, so GET,
PUT and DELETE get 4.05 (`coap_resource_dispatch`, valve `0x2a8a4`). The POST
handler (valve `0x2a1e8`, thermostat `0x291f0`) ignores the payload and runs the
fetch timer callback (`sched_fetch_timer_cb`, valve `0x2a1c4`, thermostat
`0x291cc`). The callback arms the fetch, resets the Block2 state and keeps the
held ETag. The handler sets no response code, so the answer is the server default
2.05.

`EmuState.SetSchedule` queues one empty `POST d/sched` (`BuildSchedFetchCoAP`)
when the served ETag moves and the resolved `02b3` of the device is 1. A trigger
that waits in the queue covers later documents. A trigger on the air does not.
The 2.05 of the device closes it (`TestScheduleEditQueuesOfflineFetch`,
`TestAPIScheduleEditQueuesFetchTrigger`). No capture shows the POST on the air.

The response handler (valve `0x2a344`, thermostat `0x2934c`) parses every 2.xx
except 2.03 as the document. A body that does not cover all seven days, an empty
body included, writes cfg `0x2b4` = 0, and the stored program stops. A 4.xx or
5.xx keeps the program. A device that freedo holds no document for gets 2.03 with
the presented ETag. After 25 min without coordinator answers, the device applies
the program to its heating zone (`6020` = 1). With `02b3` = 0 it never fetches.

The commercial coordinator keeps `02b3` at 0, so the captures hold no `sched`
exchange. The program reaches a zone as `6200` in the `z/s` writes, which the
coordinator pushes at every block change. freedo does the same and also serves the
program for the offline case. Bit 0 of the day mask is Monday and bit 6 is Sunday:
the valve evaluates the mask with (tm_wday+6)%7
(`sched_eval_setpoint_and_next`@0x216c0). The served shape and the acceptance of
several day programs stay unverified on the air.

### d/config write

Every accepted `d/config` write is a CoAP CON 0.03 to the bare path
`d/config`, with Uri-Port `ffff`, Content-Format 42 and no query. The device
reads the same document back from `d/<serial>/config`.

The write carries the whole document, never the changed tags alone, in this
order:

`0143 0140 015d 015c 02b3 021a 0149 015e.. 0158 015a`

Tag `015a` closes the document. Tag `021a` is the constant `0000`. Tags `013f`
and `02b2` appear in no capture. freedo emits `013f` only when a caller sets it,
and `02b2` in every document of an installation. Both go between `02b3` and
`021a`. A field that the coordinator never saw goes out with the captured
default: `0143`=0, `0149`=0, `0158`=`0200`.

The document of the wall thermostat is shorter:
`0143 0140 015d 015c 02b3 015e.. 0158`, with no `021a`, no `0149` and
`0158`=`0000` (app capture and long watch capture). freedo serves that shape to
the thermostat (`thermostatConfigDocDefaults`). A stored thermostat document in
the valve shape changes its ETag once at the next start, and the thermostat reads
it again. The thermostat has no motor, so the read homes nothing.

Tag `015d` is the id of the interface module of the device. It has no default.
Every valve document carries 112 (0 reads as 112). Any other value leaves the
valve without its controller after the next restart, and `7fff` tears it down.
On the wall thermostat, the id selects the STM32 firmware overlay that the nRF52
reflashes. The installed id is 71. The id 112 matches no image and stops the
boiler interface. The commercial bridge serves 71 to the thermostat and 112 to the
valves. The thermostat reports its own id in `h/<home>/hvac/codes`.

freedo learns the id from that report, or from a `d/config` document held for the
thermostat. It refuses 0, 112, `7fff` and every id outside the family table of
`tado/interface_module.go`. It stores the id (table `interface_module`). It learns
202 (stub, no bus) and the relay ids 1, 5, 6, 7, 9 after another family, with a
warning in `/api/health` `interfaceModuleIssues`. A refused id in the stored state
is dropped at restore, logged as `interface-module-dropped` and listed there as
refused. The boot goes on. Until the id is known, freedo holds no document for the
thermostat, and `/api/health` names it in `interfaceModuleUnknown`. Its
`d/config` GET gets `2.05` with the installation document without `015d`
(`withheldModuleDocument`), which freedo does not hold.

The thermostat gates every uplink after `d/config` on a `2.05` or `2.03` of it.
The report pump (0xe6dc) skips the rest of the cycle while 0x1e6dc reports the
gate closed. The response handler (0x1e4b4) opens the gate only on `2.03` or a
complete `2.05`. A 4.xx or 5.xx leaves it closed. The requests before the gate are
`device_auth_post`, `factory_reset_put`, `fw_state_put` and `time_get`. None of
them carries `015d`, so an error answer would keep `hvac/codes` from ever
reporting the id. The tag handler of the document (0x1e2e8) writes only the tags
that a body carries, so a body without `015d` keeps the id that the thermostat
holds. Once the id is known, the served document carries it under a new ETag
(`TestThermostatWithoutModuleServedWithout015d`), with the re-read trigger. No API
writes the tag.

Tag `02b3` is the offline schedule enable. freedo sets it in every document that
it serves or writes. The value is the one that a per-device write through
`POST /api/nodes/{id}/config` (`offlineScheduleEnabled`) set, else the
`options.offline_schedule` of the installation, 1 by default. The per-device value
stays until the device leaves the installation.

Tag `02b2` is the UTC offset in signed minutes. freedo derives it from
`options.utc_offset_min` when set, else from `options.timezone` (IANA name,
default the zone of the host) at the current instant. Valve and thermostat
firmware apply it at runtime: the per-record callback of the `d/config` response
(valve `0x1fb30`, thermostat `0x1e474`) stores it, calls `rt_time_epoch_set`
(offset x 60000 ms) and kicks the schedule engine (`sched_engine_kick`). The boot
path (`app_net_provision_init`) reads the stored value too. One write is enough,
with no reboot.

At a DST transition or an option change, the daemon serves the new offset to the
thermostat at once (new ETag, re-read trigger). It writes the new offset to one
valve every `options.utc_push_spacing_sec` (default 60), because each write homes
the motor. The document and `utc_settled` move at enqueue and persist. A push
while an operator `d/config` write is still queued replaces that write with one
document that carries both (`TestConfigWritesCoalesce`). The write that replaces a
queued one counts as queued for a clear of `02b3` and for a reload
(`TestConfigWriteReportsCoalescedWrite`). The valve also re-reads `d/config`
hourly with a conditional GET (`config_sync_due_check`@0x1fd98, ETag from
`config_request_build`@0x1fd08).

The daemon evaluates the weekly programs, the early start and `until_next` on the
same clock that the devices run (`programLocation`): a fixed zone of
`options.utc_offset_min` when set, else the zone of `options.timezone`, else the
zone of the host. An entry minute is a wall-clock time, so on a DST day a 07:00
entry starts at 07:00 local (`time.Date`), not seven hours after midnight.

Tag `0140` has no default: a write of 0 would clear an offset that the operator
set on the device. The signed centi-degree value of -70 is `ffba`.

A `PUT d/dbg2/tlvs` record reaches the served document, under the ETag that the
valve holds, only on the 2.04 of the valve. A 4.xx answer, or a write that the
retry limit drops, leaves the document as it was. If the daemon patched the
document earlier, the conditional GET would earn 2.03 and the valve would never
get the value (`TestDbg2PatchesTheDocumentOnDelivery`). A `d/config` write queued
behind it carries the record too.

For a valve with one `015e` record and an 8-byte `015a`, the document is 55 bytes
and the app plaintext is 99 bytes, with the captured 6-byte CoAP token.
`TestConfigDocumentMatchesCapture` pins both documents byte for byte.

The wall thermostat takes its configuration on the read path only. Its
`PUT d/config` handler (thermostat 0x1e230) ignores the payload. The handler
writes ff..ff to `015a` and flags `config_request_build`@0x1e64c to GET `d/config`
at once, instead of on the hourly timer. The handler answers 2.04 and does not echo
Block1, so `coap_server_request_process`@0x21150 turns the last block of a Block1
upload into `5.01`. The 64-byte Block1 PUT of the commercial coordinator gets 2.31
then `5.01`, and the thermostat GETs `d/config` about 9 s later (pairing capture).
An empty PUT without Block1 gets 2.04, and the thermostat GETs `d/config` within
about 2 s (observed on air).

Every change that moves the ETag of the served document of the thermostat queues
one empty `PUT d/config` without Block1 as the re-read trigger
(`setConfigDocument`, the one writer of `cfgDoc`). These changes do it: a `0140`
write, a flow limit (the ETag hashes the served `2040` values), an installation
reload (`015e`, election, zones, `02b3`, `02b2`), a DST transition, a cleared
per-device `02b3`, a restore and a learned `015d`. A change that keeps the ETag
queues nothing. A trigger that waits in the queue, not on the air yet, covers
later changes (`TestThermostatRereadOnEveryETagChange`). The GET after it earns a
2.05, and after a 2.05 the thermostat GETs `c/<N>/config`.

`EnqueueConfig` to the thermostat queues no document write (`serveConfig`). A
write that moves only `0140` goes into the served document under a new ETag and is
stored with the desired state. `config_report_response_handle`@0x1e4b4 parses the
body through the per-tag callback at 0x1e2e8. The callback hands `0140` to
`cfg_set_and_report` (0x1e346), then stores the ETag of the option in `015a`
(`TestThermostatCalibOffsetServed`). A write that moves any other tag stays
refused. The callback takes `02b2`, `02b3` and the rest too, but the installation
owns them. `d/dbg2/tlvs` is a valve resource, so mode `dbg2` stays refused on the
thermostat. No capture shows the thermostat apply a served `0140`.

### d/lock and d/identify write

Both take the shape of the `d/config` write: a CoAP CON 0.03 to the bare path,
with Uri-Port `ffff`, Content-Format 42 and no query. `d/lock` carries `0290`:
`01` to lock and `00` to unlock. `d/identify` carries an empty payload. The device
answers `2.04` to both. The wall thermostat has no `d/lock` resource: its firmware
holds no `d/lock` path and it answers `4.04`. So `EnqueueChildLock` refuses it, and
the API answers 400 for any device that is not a valve. The thermostat has no child
lock in any form: its image holds no `0290`, its `d/config` record callback drops
unknown tags, and its `/lock` report suffix has no caller. It registers `d/identify`
the same way a valve does, and it takes the write: on 2026-10-12 a wall thermostat RU02
with firmware 215.2 showed "Hi!" on its display. The node view reads the
served lock state under `ChildLock`, null on a device that is not a valve.

A valve GETs `d/<serial>/lock` with Block2 0/0/128. The captured answer is a
`2.05` with ETag, Block2 0/0/128 and `0290`=`00`. Every valve with the lock off
reads the ETag `31cfbb11e5533758`. freedo serves the same options with the ETag
that `configEtag` derives from the body.

### Pairing

The pairing frame to a joining device is a Data frame under the fixed pairing key
`tado.PairingKey`, not under the network key. It carries a CoAP CON POST (code
`0.02`) from UDP 5683 to 5683 to the bare path `d/pair`, with an empty token,
Content-Format 42 and option 2048 with an 8-byte single-use value. It has no
Uri-Port and no query. The 8 bytes are new on each request and nothing keeps
them. The pairing capture holds three `d/pair`, all from port 5683.

The payload is one TLV with a one-byte tag and a one-byte length. Two forms
exist. Tag `0x12`, length 16, holds the network key in plaintext. The coordinator
also uses tag `0x07`, which wraps the key with a per-device KEK that only the
cloud holds. freedo owns no KEK, so it sends tag `0x12`.

The valve answers `2.04` under the new network key within 30 ms. It keeps its
persistent token and resumes with `auth/token`. It then sends a Router
Solicitation, takes the advertisement, reads `time` and `d/<serial>/config`, and
starts to report. A `d/pair` to a valve that already talks the network key goes
under the network key, and the valve answers `2.03`.

A joining valve talks the pairing key before the `d/pair`: it broadcasts Enhanced
Beacon Requests and Router Solicitations under it. The three MAC command frames of
the captures are all Enhanced Beacon Requests. No capture holds an Association
Request. `tado/beacon.go` rebuilds the three captured request and answer pairs of
the pairing capture byte for byte.

- Request: 27 bytes. It holds FCF `0be8`, the seq of the valve, PAN and address
  `ffff`, the valve EUI, security control `04`, then (sealed) the frame counter of
  the valve, the MAC command `07` and the MAC FCS, and then the MIC. The command id
  sits in the ciphertext.
- Answer: an Enhanced Beacon of 32 bytes. It holds FCF `18ec` (frame type 0, Frame
  Pending set), the next seq of the bridge, PAN `abcd`, the valve EUI, the bridge
  EUI, security control `04`, the next frame counter of the bridge (12905, between
  the downlinks 12903 and 12906), the MAC FCS and the MIC. It has no IE and no
  payload. As in a Data frame, the nonce is `frame[0:13]` and the aad is
  `frame[0:16]`.
- Timing: there is no wake-up train. The answer ends 14, 10 and 9 ms after the
  request ends, so it starts 2.4 to 7.4 ms after it at 50 kBaud. The Enhanced ACK
  of a Data frame ends 6 to 7 ms after it (0 to 1 ms gap). The spread matches the
  commercial listen before transmit (3.97 to 7.81 ms). A request that the sniffer
  received with a bad CRC gets no answer: a wake-up train starts 4 ms later.
- The valve repeats the request every 8.000 s on this channel. Its frame counter
  moves by 7 between two requests, so it sends other frames that the capture does
  not hold. After an answer, its next frame is a Router Solicitation under the
  pairing key, 2.8 s and 1.5 s later. After one answer, it asked once more. The
  reason is open.

During the window, the bridge sends each Router Advertisement twice. Each copy
goes behind its own 254-frame wake-up train. The first copy is under the network
key with Frame Pending set (FCF `59e8`). The second copy is under the pairing key
(`49e8`). The two copies have consecutive seq and frame counter values and the
same payload (four pairs in the pairing capture). The second train starts about
6 ms after the first advertisement ends. The first pair comes before any
pairing-key frame of the valve, so the doubling follows the window, not the
joiner. A Router Solicitation under the pairing key gets the pair within 1.4 s
(three cases). A solicitation 2.8 s after a pair ended gets none, as the 3 s rule
of the Router advertisement section says. One solicitation 3.9 s after a pair gets
none, for a reason that the capture does not show.

The bridge sends `d/pair` ahead of the frames that it holds for the joiner. In the
first window, a `d/config` gets its Enhanced ACK and no CoAP answer. `d/pair` goes
on 2.9 s later. That `d/config` never comes back: a new `d/config` with a new MID
goes on later. In the second window, a Neighbor Advertisement retries under one MAC
seq until its Enhanced ACK, and `d/pair` goes on after it. The capture does not
show when the cloud asked for `d/pair`. So it cannot tell whether `d/pair` waited
for that advertisement.

The joiner restarted its frame counter before both windows (counter 1 after a
higher value). Both commercial `d/pair` frames go on a 21-frame train, on the
anchor of an Enhanced ACK that the joiner sent after that restart.

The bridge firmware learns the joining device in its pair window. While flag
`0x20006e14` is set, `csl_rx_frame_dispatch@0x36088` retries a frame that fails the
network key. The retry uses the key that `pair_target_arm@0x32c50` stored at
`0x20006e04` (default "tado pairing key" at `0x5a884`). A frame that verifies there
tags its sender with key index 2, and `csl_tx_statemachine` secures the frames to
that sender with the pairing key.

freedo does the same while a pair window opened with `POST /api/pair/window` lasts
(at most 10 minutes). A Data frame, a broadcast or an Enhanced Beacon Request that
fails the network key and verifies under the pairing key records its sender in
`GET /api/pair/window`. `POST /api/pair` with no target names the one device of the
open window. While the window is open, `d/pair` goes under the network key only to a
device that the window heard under the network key after its last pairing-key frame.
freedo picks the key when it builds each attempt.

`EnqueuePair` puts `d/pair` ahead of every downlink that is not on the air. When
the daemon builds the next attempt (`pairFirst`), an attempt that the radio holds
(awaitingCSL) finishes first, because the host cannot recall it. Any other downlink
in flight waits behind `d/pair` with its tries, MID and MAC seq. Its answer no
longer closes it, and its next attempt asks again under the same MID. The daemon
dispatches `d/pair` as soon as the radio hands the attempt back, with or without an
Enhanced ACK. A firmware upload owns the queue, so `d/pair` waits for it.

Every `d/pair` attempt carries `CSLBootstrapAlways`. The bridge firmware takes an
anchor that has not missed, and falls back to the full train after one miss
(`anchorForJob`). The host cannot force the full train over a warm anchor without a
firmware change. So a joiner that rebooted after its last Enhanced ACK costs one
attempt.

The answer to the Beacon Request lives in the bridge firmware. A request reaches the
daemon one serial reply after it ends, and the beacon needs one more request. Each
takes about 2.5 ms of USB latency, plus the engine and the receive window. The
commercial gap is 2.4 to 7.4 ms.

With an armed transmitter, the window hands firmware v2.9 the pairing key, the
coordinator EUI and a block of 64 seq and frame counter values (`CMD_PAIRWIN`), and
`GET /api/pair/window` reports `responder: true`. Then every advertisement of the
window goes twice as above, and a Router Solicitation under the pairing key gets
that pair, with the 3 s rule of the network-key solicitations. A bridge before v2.9
answers `0xE0`. The host then sends the beacon itself through the immediate path,
late, and the advertisement stays single. This is not validated on air. The bridge
stops answering at the deadline on its own and keeps its beacon count until the next
`CMD_PAIRWIN`. The daemon closes the window on the bridge within a second of the
deadline and logs that count, as it does on an explicit close.

The key crosses the air in the clear, under a key that every valve shares. So the
commissioning window must stay short and the band must stay quiet.

### Network commands and leaving the network

A "network command" is a value `t` in 1000 to 1008. The valve firmware carries a
handler for them, `coap_res_netcmd_write`@0x224e0: it reads the URI-query `t`
(`coap_msg_uriquery_find_param`, key `t`@0x38be0), converts it to an integer and
branches through a jump table at 0x2258e (disassembly, confirmed with Ghidra
pseudocode).

| `t` | Effect in the firmware | Address |
|---|---|---|
| 1000 | queue a reboot: write reboot reason attr `0148` = `0x20` | 0x225c6 |
| 1001 | mark the running OTA image bits (`ota_image_state_bits_update(1)`) | 0x22642 |
| 1002 | clear the OTA image bits (`ota_image_state_bits_update(0)`) | 0x225ea |
| 1003 | send the OTA image-state report | 0x225f2 |
| 1004 | store the payload length byte, no device change | 0x22646 |
| 1005 | `rxwindow`: `FUN_14fc8(0)` then a radio-table callback | 0x225f8 |
| 1006 | tear the node down, then the 1005 action and enable subsystem `0f` | 0x22608 |
| 1007 | erase the network key: `FUN_14fc8(0x80)`, disable subsystem `0f`, call the key-erase helper @0xbe0c, then mark the OTA image bits | 0x2262c |
| 1008 | `devinfo`: send the device-info report (attr `0260`) | 0x22626 |

The key-erase helper @0xbe0c overwrites provisioning field `0x12`, the 16-byte
network key, with 16 bytes `0xFF` and commits it (`prov_mount_page(1)`,
`prov_field_write(0x12)`, `prov_store_commit`, disassembly). So `t=1007` erases the
stored key. It does not reboot and it does not open the pairing window. A valve that
runs `1007` keeps the key in RAM until the next reset; after the next reset it has no
key and no open window.

`coap_res_netcmd_write` is present but not wired to any received resource in this
image (valve 215.1). Its only reference is one data word at a resource descriptor
0x3acb4, and nothing references that descriptor (Ghidra: no code reference to the
handler, no reference to the descriptor; the constant 0x224e1 appears once in the
image). The path `d/test/v1`@0x38360 is used once only, to enqueue a boot report
(`dispatch_node_enqueue`@0x22068), not to register a receiver. The 19 resources that
the valve registers as CoAP receivers (`coap_dispatch_node_register`) do not include
`d/test/v1`: `d/pair`, `d/config`, `d/reboot`, `d/identify`, `d/lock`, `d/sched`,
`d/fw`, `d/fw/state`, `d/fw/rq`, `d/currentlog`, `d/dbg/st`, `d/dbg/m`, `d/dbg2/tlvs`,
`z/config`, `z/s`, `z/cpe`, `z/extui`, `z/legionella` and the device self node. So a
write to `d/test/v1` matches no receiver and runs no handler.

This matches the plant (2026-10-13): a write to `d/test/v1?t=1007` takes the MAC ack,
gets no CoAP answer, and leaves the key in place; the valve keeps reporting and, after
a `d/reboot`, rejoins with the stored key. On this firmware a coordinator cannot run
network command 1007. freedo sends no network command.

The resources that change pairing, key or factory state:

- `d/reboot` (POST): schedules a reset about 5 s later with reboot reason attr
  `0148` = `0x20` (`coap_reboot_res_init`@0x21358, handler @0x2133c, disassembly). The
  boot path reloads the stored key, so the valve rejoins. No key change. On the plant the valve
  reset about 5 s after the request and was back on the network about 40 s later (2026-10-13).
- The physical button long-press runs `pair_factory_reset`@0x30a9c (disassembly plus
  Ghidra): it wipes the device config records `0xf2`, `0xf3` and `0xf4` to `0xfb`
  (`cfg_wipe`@0xc738), writes `015d`=0 and `015c`=0, sets attr `0270`=1, erases the
  network key (@0xbe0c), then forces a system reset. It also enters commissioning
  through `pair_enter_commissioning`@0xbff0, which installs the shared pairing key and
  opens the window.
- `pair_factory_reset`@0x30a9c and `pair_enter_commissioning`@0xbff0 are reached only
  from the button UI state machine (callers 0x26c6e, 0x27636 and 0x26f26, 0x278ac;
  Ghidra cross-references). No CoAP resource, network command, attribute write or
  reboot reason reaches them. There is no remote way to enter commissioning.

So there is no frame a coordinator can send to detach a valve from the network. The
button is required. A hypothetical working `1007` would make this worse, not better:
it erases the key without a reboot and without a window, so the valve would be deaf
after its next reset until someone holds the button. After the button and the new
pairing the valve reports fault state code 8 (`01a3` = `0x4000`) until it is mounted
again, because the reset clears its calibration (plant, 2026-10-13).

The wall thermostat carries the same resources (`d/test/v1`, `d/reboot`, `d/pair`,
`d/dbg2/tlvs`) and the same network-command jump table (`t - 0x3e8`@0x20de2). Its
`t=1007` branch calls the same key-erase helper (@0xc7fc: field `0x12` = 16 bytes
`0xFF`, commit), and its button factory reset calls that helper too (caller 0x31428,
disassembly). The thermostat mirrors the valve; its network-command receive wiring is
not traced separately.

### Echo and neighbor discovery

A valve sends the bridge an ICMPv6 Echo Request with identifier and sequence
(pairing capture, twice, after pairing). It also broadcasts a Neighbor Solicitation
for the bridge address to its solicited-node group (once). The bridge queues an Echo
Reply with the same identifier, sequence and data. It also queues a unicast Neighbor
Advertisement with flags R, S and O, the bridge address as target and the bridge
EUI-64 as target link-layer address, hop limit 255. It retries each under the same
MAC seq with a new frame counter. The capture holds nine attempts of the
advertisement. freedo builds both answers byte for byte and queues them the same
way.

The engine does not cache the body of an ICMPv6 frame as telemetry
(`TestICMPv6BodyIsNotTelemetry`). `ParseApp` falls back to a TLV-suffix heuristic
when the body is no CoAP packet, and it accepts some ICMPv6 bytes. The EUI-64 of
the source link-layer option in a Neighbor Solicitation starts `00 1b c5 07`. From
its second byte, it reads as tag `1bc5` with length 7 and a value of the 4
remaining EUI bytes and 3 padding zeros, followed by an empty tag `0000`. The next
header, type and checksum of an Echo Request read as `3a80` and a checksum tag.

### Zone read path

`GET h/<home>/z/N/s` gets a `2.05` with an ETag and Block2 0/0/128, and no
Content-Format. This holds for all 21 answers of the app capture. The GET asks for
Block2 0/0/128. The ETag follows the body alone: `107917f7351475a6` answers zones 8,
5, 2 and 1 with one body. freedo answers the same options, with the ETag that
`configEtag` derives from the body.

The body holds the downlink tag order `6160 6180 6020 61e0 [6200] 6240 [6260 6280] 62e0`,
with no `6440` in any captured read. The head is the home state of the moment:

- At home: `6160=1`, `61e0=1` and the program `6200` (app capture: 20.00 C on a room
  zone, 55.00 C on hot water).
- In away, `6160=2`. A zone at the default away temperature gets `61e0=0` and no
  `6200` (heat-demand capture, and the first reads of the app capture). Another zone
  gets `61e0=1` and the away temperature as `6200`.

`6260` and `6280` follow an overlay only: a read with `6240=0` carries neither. `6020`
is 1 on a room zone and 2 on the hot water zone. The cloud answers a valve with the
same shape.

`directZoneReadBody` rebuilds that block for the wall thermostat and for a valve. It
takes the head from `zoneWriteHead` with the pushed home state and program block. It
takes the overlay tags from the zone cache, whose order is the zone-plane one. `6200`
comes from the pushed program, else from the zone cache, never from the requester,
because the thermostat sits in two zones. The rebuilt block matches all 12 valve read
answers of the app capture. The cache echo matches 1.

The thermostat applies a read through `zone_state_read_tlv_cb` (`0x11f38`), which
stores each tag in its zone config. A change of `61e0`, `6200`, `6260` or `6280`
schedules `zone_state_timer_cb` (`0x2d70c`, via `zone_state_on_resource_change`
`0x2d948`). With `6240=0` and `61e0=0`, the callback publishes the zone target `4020`
as 5.00 C on a room zone and 30.00 C on hot water, and `40e0=0`, `4120=0`. The
callback reads the enable from `61e0` when `6240=0`, else from `6260`, and publishes
it on `40e0` and `4120`. It skips a zone whose `6240` is unset (`0x7f`) and publishes
nothing for it. A set `62e0` forces the enable to 0.

`zone_to_circuit_translator_handler` (`0x2df2a`) takes `4020` as the own target of the
thermostat or as the zone setpoint of the circuit lead zone (`4000`). On the hot-water
zone, it stores `4020`, `40e0` and `4120` in cfg `0x199`, `0x19a` and `0x19b`. `40e0`
is the DHW enable. No target path reads `6160`. This comes from static analysis only.
No capture shows the effect.

The device sends the GET with no ETag (`app_node_notify_build`, valve `0x118e4`). The
response handler (valve `0x11984`, thermostat `0x116fc`) writes each tag of a 2.xx
body into its zone config (`zone_state_read_tlv_cb`, valve `0x121c0`). It keeps every
tag that the body omits. So an empty 2.05 and a 2.03 change nothing, and the device
drops a 4.xx or 5.xx.

A zone with no cached block reads the home state and the program block that a zone
rewrite carries (`6160 6180 6020 61e0 [6200]`), then `6240=0` and `62e0=0`, or the
`62e0` that the daemon drives. Every cloud read answer of the app capture, valve and
thermostat, carries `6240` and `62e0` and no `6440`. A thermostat that restarted
holds no `6240`, and `zone_state_timer_cb` skips the zone until one arrives: no zone
target and, on hot water, no DHW enable. Before the daemon pushes a home state, the
read stays an empty 2.05. A zone-plane read of `z/s?lid=N` answered from the cache
takes the same rebuilt block. So does the answer to a proxied read that timed out. The
cache keeps the head of the last write or report, which a later home state leaves
stale.

A write of `h/<home>/z/N/ow` gets a `2.05` with no body. It is the one write resource
that is not answered with `2.04`. The valve sends the report as a CON PUT with no body
when its own detector sees the window open. In the app capture, the report of zone 3
(`62c0` = 900) takes its `2.05` after 1 s. After 2 s, the coordinator writes
`z/s?id=3` with `62e0=1` to the one valve of the zone, with no `6440`. It writes
`62e0=0` 905 s after that write. In an earlier sniff, the wall thermostat reports zone
1 and takes the `z/s?id=1` write with `62e0=1`. The valve of zone 1 gets it only from
the relayed `z/s?lid=1` echo of the thermostat.

A member sends `DELETE h/<home>/z/N/ow` when the `62e0` of its zone drops to 0 from a
source other than the coordinator (valve 0x11bb8, thermostat 0x11930).
`app_node_apply_write` (valve 0x11fc0) sets the report flag only on a write that does
not come from the `z/s` tag handler (0x121c0). The builder sends PUT while the detector
holds the window, DELETE once `62e0` is 0, and nothing while `62e0` is 1. No capture
holds the DELETE. The response handler (valve 0x117f0) clears the flag on any code, so
freedo keeps the `2.05`. The daemon drops the device hold of the zone at once. When the
zone closes, it owes `62e0=0` to the zone writers (`TestWindowDeviceCloseEndsHold`).

Every captured `z/s?id=N` write goes to the one member that publishes `z/N/act`, also
for home or away and block changes. The other members get the block from its relayed
echo. freedo writes the demand reporter alone. It writes every member when the zone has
no reporter or the reporter is offline. freedo keeps the window of each zone in the
daemon: see `internal/daemon/window.go`.

`GET z/p?lid=N` gets the last measures that the zone member pushed, tags `4060`,
`40a0` and `0135` in that order, with a `Max-Age` of 1800 s. The push carries the same
option. `GET z/cpe?lid=N` gets the last heat percent `4160` that the wall thermostat
pushed for that zone, with no `Max-Age`. The thermostat sends the `z/cpe` push as a NON
aimed at the zone member. So the coordinator caches a zone write on the forwarding path
too, not only when the write is addressed to it. The captures hold no `GET z/cpe`: the
served shape follows the push.

A `GET h/<home>/z/N/config` that carries the ETag of the served document gets a `2.03`
with no body, the same ETag and the `Block2` option of the request echoed back. The
`2.03` to `d/config` has the same shape (67 `z/N/config` and 39 `d/config` 2.03 in the
two long watch captures). freedo answers every conditional GET that it validates this
way (`validAnswer`), `c/N/config` and `d/lock` included. The captures do not show those
two as conditional. A GET of the config of a zone that the installation does not hold
gets `4.04` with the `Block2` of the request and no other option (pairing capture).

No captured answer to a direct GET carries Content-Format: `d/config`,
`h/<home>/z/N/config`, `h/<home>/c/N/config`, `h/<home>/z/N/s`, `d/lock`,
`auth/token` and `time`. The 2.05 carries ETag and Block2, except `time` and
`auth/token`, which carry no option. freedo adds no Content-Format to them. The
zone-plane answers (`z/s?lid=N`, `z/p?lid=N`) keep Content-Format 42, as
captured.

### Delta reports

Every report resource (`hvac/mon`, `d/*/sen`, `d/*/act`) sends only the
attributes that changed. Frame-length deltas prove it. The emulator must not
assume full snapshots.

### Flow limit

Tag `2040` is the heating flow limit of a circuit: a big-endian uint16 in centi-degrees
(`12c0` = 48.00 C in every capture). The wall thermostat GETs it from
`h/<home>/c/<N>/config`. At HVAC init it writes the value as the OpenTherm MaxTSet (id 57), for
the circuit that its `015e` role selects. The read-back of the applied value arrives as tag
`046f` in `hvac/config`. The boiler reports the highest CH setpoint it accepts as tag `0471` in
`hvac/config` (OpenTherm id 49, centi-degrees, 60.00 C in the reference installation). Tag
`046f` is not that bound.

freedo serves no `2040` to a circuit while `flow_limit_c` is unset and nothing manages the
circuit. The code notes that the thermostat then applies a MaxTSet of 70 C when its boiler
interface starts. No capture or measurement on a boiler confirms this value. freedo serves the
lower of the limit and the `0471` bound, when the bound is from 30 C up to below 75 C, the
window of the daemon.

### Boiler energy

The energy estimate (`internal/daemon/boiler_energy.go`) reads two `hvac/mon`
values and the time of the last `hvac/mon` frame.

- `0452` is the relative modulation in percent (`0x0035` = 53). `0457` reads 1
  while the burner runs and 0 after, and no other value appears. The 15-minute
  aligned capture holds three burns with the hot-water temperature (`045a`)
  falling from 48 to 34 C, and `0457` stays 0 or 1 there too. No captured tag
  tells heating from hot water, so each counter is a total.
- The modulation spans the output from the boiler minimum to its maximum. The
  burner stays on (`0457` = 1) with `0452` at 0 for 32 s in the 15-minute aligned
  capture, and at 3 % for 120 s in the heat-demand capture.
- The heat-demand capture holds 17 `hvac/mon` frames in 223 s while the burner
  ran. The gaps are 4..48 s, and `0458` is in 11 of the frames. The modulation
  alone stays unreported for up to 120 s. The 15-minute aligned capture holds gaps
  of at most 29 s inside a burn. Between burns the frames stop (94 s, then 280 s).
- No catalogue tag is decoded as the boiler capacity (OpenTherm id 15). `046e` and
  `0470` are the only 2-byte `hvac/config` tags without a name, and the captures
  hold neither. The maximum output is a configuration value.
- The reference boiler lists `044d`, `044f`, `045e` and, in `hvac/maint`, at least
  `0464`, `0466` and `0468` as unsupported (the decoded log cuts the first byte of
  the `hvac/maint` list). Its `0465` and `0467` read 0. `GET /api/boiler` shows
  `BurnerStarts` and `BurnerHoursMin`, null on such a boiler.

### Zone config document

The coordinator serves `h/<home>/z/10/config` as three RFC 7959 blocks of 128 bytes
under one ETag `6ddcb9dc8da3b865`. Blocks 0 and 1 travel 6LoWPAN fragmented over two
Data frames each. Block 2 fits one frame. The 313 reassembled bytes match what
`BuildZoneConfigDocument` emits for zone 10 (`TestZoneConfigDocumentMatchesCapture`).
Member order changes the bytes, so the installation store must keep it.

| Tag | Name | Type | Meaning |
|---|---|---|---|
| 6020 | `zone_kind` | uint8 | zone kind: 1 heating, 2 hot water |
| 63e0 | `zone_cfg_state` | uint8 | the document always carries 1 |
| 63a0 | `zone_owner_uri_s` | string | owner endpoint, the elected demand reporter |
| 8000 | `zone_member_uri_s` | string | one entry per zone member |
| 8400 | `zone_report_uri_p` | string | where a member reports its zone state |
| 8200 | `zone_cpe_uri` | string | where a valve takes the heat percent `4160` |
| 6040 | `zone_measure_uri_p` | string | the measuring endpoint of the zone |
| 6060 | `zone_circuit` | uint8 | zone to circuit binding, `01` in the capture. Absent for a heating zone with no circuit |
| 6080 | `zone_cfg_6080` | uint16 | controller gain Kp, 1000 by default |
| 60a0 | `zone_cfg_60a0` | uint16 | controller gain Ki, 500 by default |
| 60c0 | `zone_cfg_60c0` | uint16 | controller gain Kd, 1500 by default |
| 60e0 | `zone_cfg_enable` | uint8 | open window detection: 0 off, 1 on |
| 62c0 | `zone_open_ivl_sec` | uint16 | open window duration in seconds, 900 by default |
| 6380 | `zone_refresh_ivl_sec` | uint16 | 900 seconds, the open window report timer |

A heating zone with no circuit gets a document without `6060` and without the heat source
among the `8000` members. No device has run this document. The install check states that such
a zone falls back to circuit 9, whose demand nobody publishes.

Every URI tag holds the ASCII form `coap://[<ipv6>]/z/<resource>` of one
member endpoint. The 0x8xxx tags are indexed containers: one TLV per member,
in membership order.

Tags 6080, 60a0 and 60c0 are the gains of the zone controller. The valve
(`zone_ctrl_pid_init`@0x2b5f8) reads them with no clamp. `ctrl_pct_update`@0x31970
gives the heat percent `40a0`, clamped to 0..100:

`Kp*(SP-T)/2000 + Ki*integ/30000 - Kd*(T-Tref)/2000`

The formula works in centi-degrees. `integ` is clamped to `0..3000000/Ki` (0 when Ki
is 0). `Tref` is the zone temperature about 10 minutes earlier. The init zeroes the
controller state, integrator included. The valve runs it after each 2.05 of this
document (event `0x16d` from the response handler at 0x10ea8), not after a 2.03.

The valve re-reads this document after each `d/config` check. A 2.03 of the hourly
conditional GET clears the fetch flag (`app_node_view_reset`@0x29780), and the next
GET carries the ETag of the document that the valve holds (`0x6000|zone`). The
heat-demand capture shows the pair, `d/config` 2.03 then `h/<home>/z/N/config` 2.03,
one second apart. A new ETag therefore reaches the valve within an hour with no
write. The motor state machine subscribes to no event of this path (list at
0x37724), so the document alone should not home the motor. No capture confirms it.

The wall thermostat holds the same controller (`zone_ctrl_pid_init`@0x2a44c). It
reads the gains only when its zone control is switched (`6400|zone`).
`app_report_interval_get`@0x1189c reads tag 6380 as the period of an open window
report timer (default 600), not as a document refresh. freedo keeps the gains per
zone in `options.zone_pid` (`PATCH /api/install/zones/{n}` field `pid`).

Open window detection needs no `z/s` write. A change of the setting rewrites this
document under a new ETag. `60e0` carries the enable. `62c0` carries the duration in
seconds: 900 by default, with 1200 and 600 observed. freedo keeps the two values per
zone, in the `zone` row and on the `/api/install` zone. The coordinator then PUTs the
unchanged `d/config` document to the valve. This makes the valve read
`d/<serial>/config` and `h/<home>/z/N/config` again.

The window-open state of the zone plane is a different tag, `62e0`. Tag `62c0`
appears in this document alone, but `tado/tlv.go` names it `open_window_zone` outside
the document. `tado.ResourceOf` reads the resource from the request URI. For a
response, which repeats no Uri-Path, it reads the resource from the marker tags
`63a0`, `63e0`, `6040` and `6380`.

### Open window detection in the device

The valve runs its own detector, `open-window-detector`@0x38a54. It watches two
attributes, the raw sensor temperature `012e` (centi-degrees) and the humidity `0135`
(deci-percent), through a report subscriber (`config_evt_apply`@0x2a738, attr list
`012e 0135`@0x38a6a, disassembly). It keeps a short ring of recent samples, 3 deep for
temperature and 2 deep for humidity, and pushes a new sample each time the device
reports one. A 900 s timer (`0x1c200` ticks at 1/128 s) re-arms the node and samples
again, so the ring advances at the report cadence and at least every 900 s.

The decision runs on each new sample (the state machine at 0x2a5f0, disassembly). It
compares the newest raw temperature to the previous sample and to the one before, and
the humidity the same way:

- temperature fall to the previous sample of more than `0x14` = 20 centi-degrees
  (0.20 C);
- temperature fall to the sample before that of more than `0x1e` = 30 centi-degrees
  (0.30 C);
- a humidity step of `0x13` = 19 deci-percent (1.9%).

Any one of these sets attribute `021c` = 1 (`open_window_detected`, write @0x2a6fe).
The decision reads no setpoint and no heat-demand flag, so detection does not need the
valve to be heating at that moment (disassembly).

The enable is zone tag `60e0` (`h/<home>/z/N/config`, 1 on, 0 off). The duration is
zone tag `62c0`, 900 s by default (`docs` above, and `tado.ZoneOpenWindowDurationSec`).
freedo serves `60e0` = 1 by default. A change of either tag rewrites `d/config` under
a new ETag, which costs one homing stroke per valve, so the setting must not be toggled
right before a test.

Reproduction on a plant: with detection enabled, drop the valve's own sensor by about
0.3 C across a few consecutive reports. Cold air or an open window aimed at the valve
head, enough to move its reading 0.3 C within a few minutes, is the reliable trigger. A
slow whole-room cooling where each sample step stays under 0.20 C may never trigger,
because the test is sample to sample, not cumulative.

On a trigger the valve sends a CON PUT `h/<home>/z/N/ow` with no body (valve 0x11bb8
builder, disassembly). The coordinator answers `2.05` and, as the app capture shows,
writes `z/s?id=N` with `62e0=1` to the one member that reports the zone, with no
`6440`. `62e0=1` makes the zone target publish its enable as 0 (`zone_state_timer_cb`,
"a set `62e0` forces the enable to 0" above), so the valve stops calling for heat and
closes. The other members of the zone do not follow by themselves: they get `62e0`
only from the coordinator, or from the relayed `z/s?lid=N` echo of the member that
took the write. freedo drives this in `internal/daemon/window.go`
(`takeWindowReports`): a device report opens a `windowDevice` hold for the `62c0`
seconds of the zone and owes the reporter a `62e0=1` write.

Ending the window early from the coordinator:

- The frame that ends it is the same `z/s?id=N` zone write with `62e0=0` and no
  `6440`, sent to the member that holds the zone. It re-enables the zone target, so
  the valve calls for heat again at once. freedo already builds this frame
  (`tado/build_coap.go:347`, the `62e0` byte; the engine writes it on a close).
- The valve does not fight a coordinator `62e0=0`: `app_node_apply_write`
  (valve 0x11fc0) sets the report flag only on a write that does not come from the
  `z/s` tag handler, so a coordinator `z/s` write makes the valve neither re-report nor
  reopen (disassembly). It reopens only if a fresh temperature fall fires the detector
  again, so the early close holds once the temperature has recovered.
- The `windowDevice` hold ends at the `62c0` deadline (`tickWindows`,
  `internal/daemon/window.go`), on a device close report (`takeWindowReports`), or on a
  reset. `POST /api/zones/{n}/window` with `{"open": false, "reset": true}` drops every
  source of the zone, the `windowDevice` hold included, and writes `62e0=0`
  (`TestWindowResetEndsDeviceHold`). Without `reset`, a close ends the `windowExternal`
  source only and leaves a device-detected window open. See [API.md](API.md#window). No
  reset of a detected window ran on a plant.

Test risk: a `z/s` write that carries `62e0` carries no `6440` and does not restate the
program, so it takes no new ETag and rewrites no `d/config`: it costs no homing stroke.
The only motor movement is the valve closing when `62e0=1` suppresses heat and opening
again on `62e0=0`, which is ordinary setpoint following, not a calibration. The test
needs no `d/reboot`. The one expensive step to avoid is toggling the `60e0` or `62c0`
setting, which rewrites `d/config` and homes the valve.

The wall thermostat carries the same detector (`open-window-detector`@0x5affc) and
reports the window the same way. The app capture shows the thermostat reporting zone 1
open and the coordinator writing `z/s?id=1` with `62e0=1`; the valve of that zone gets
it only from the relayed `z/s?lid=1` echo of the thermostat. The thermostat detector
internals are not traced separately.

### Offset and telemetry

- Offset: `PUT d/config` tag `0140`, signed centi-degrees. `0140 = 012d -
  012e` (calibrated minus raw). App offset and Home Assistant offset are
  byte-identical on the wire and both trigger a full valve stroke. A
  setpoint change (overlay `z/s`) does not stroke.

The catalogue names the motor telemetry of `d/*/act`, the fault report of `d/*/err`
and the three constants that carry no confirmed meaning. A name with a tag suffix
marks a role that the capture shows but does not resolve.

| Tag | Name | Type | Fact |
|---|---|---|---|
| 0265 | `valve_travel_0265` | uint16 | travel counter, paired with `0294` |
| 0294 | `valve_travel_0294` | uint16 | travel counter, paired with `0265` |
| 028d | `valve_travel_delta` | uint16 | `0265 - 0294` in every frame that carries it |
| 0273 | `valve_ctr_0273` | uint16 | 2278..2347, moves with `027c` |
| 027c | `valve_ctr_027c` | uint16 | 2300..2480, always above `0273` |
| 0280 | `valve_const_0280` | uint16 | 1786..1794, near constant |
| 01a3 | `device_fault_word` | uint32 | `d/*/err` fault word, `0` = no fault; bits below |
| 0161 | `sen_0161` | uint16 | one sample of 6205 in a thermostat `d/*/sen` |
| 021a | `dcfg_const_021a` | uint16 | `d/config`, always `0000`, no store backend |

Tag `01a3` is one RAM word in the valve (0x20003850) and in the wall thermostat
(0x20002090). Both images hold the same code for it. A set helper (valve @0x108d8,
thermostat @0x106e0) and a clear helper (@0x1094c, @0x10754) change single bits, and
each change sends `d/*/err`. Byte 1 of the word holds a state code from the interface
module of the device, and no flags. The constants in `tado/tlv.go` name the flags, the
mask and the state codes, and `tado.DecodeFaultWord` splits a word into communication
warnings, device warnings and faults. Addresses in the table are valve first, thermostat
second.

| Bit | Constant | Set when |
|---|---|---|
| `0x02` | `FaultNoNeighbour` | the 60 s check (@0x2e188) finds no IPv6 source address or no neighbour |
| `0x04` | `FaultStoreWrite` | the attribute store refuses a write in the common attribute set path (@0xd902, @0xe116). No code clears the bit, so it stays until a reboot |
| `0x08` | `FaultConfigWrite` | a write of a persistent configuration record fails 3 times, or the write of a default record fails (@0xcc94 and @0xccdc, @0xd694 and @0xd6dc). No code clears the bit |
| `0x80` | `FaultNoConfigAnswer` | the valve got no answer to its own `d/config` GET for 90 min (timer 5400 s, cleared by any answer code in `config_report_response_handle`@0x1fb70), and from boot until the first answer |
| `0xf800` | `FaultStateMask` | state code of the interface module, `(word >> 11) & 0x1f`. The module writes attribute `0fa3` = code `<< 3`, and an observer copies it into byte 1 (@0x108f8, @0x10700). Bits `0x0700` always read 0 |
| `0x100000` | `FaultStackLow` | the stack check finds 32 units or less of free stack (@0x2e2d8, @0x2cc2a). No code clears the bit |
| `0x200000` | `FaultIgnored` | no code sets the bit. The `d/*/err` serializer clears it after a report that carries it (@0x20712, @0x1ee66) |

Bits `0x02` and `0x80` describe the radio link. A missed hourly GET on a weak link sets
`0x80` and the next `2.05` clears it. The daemon reports them as
`commsWarnings` (`noNeighbour`, `noConfigAnswer`) in `/api/health` and
`/api/nodes`, and keeps `fault` false for them. The flags `0x04`, `0x08` and `0x100000`
are the faults `storeWrite`, `configWrite` and `stackLow`. The daemon ignores `0x200000`.
A bit outside this table is the fault `unknown`. `faultWord` keeps the raw word.

The state code of the valve comes from the motor state machine
(`hvac_therm_interface_valve`, state byte at 0x2000922f, published @0xa340). The code
is one value: `0x4000` is code 8. Each motor move and each calibration
writes the code again, so a later successful move or a calibration start sets it to 0.
After a failed move the valve waits `0276` seconds (default 600) and tries again.

| Code | Word | Set when |
|---|---|---|
| 0 | `0x0000` | a move ends at its target, or a calibration starts or ends without a remark |
| 1 | `0x0800` | calibration fails with error 1 after the last attempt: the search finds no contact with the valve pin |
| 2 | `0x1000` | calibration fails with error 2: the closing move finds no end stop |
| 3 | `0x1800` | calibration fails with error 3: the travel between pin contact and end stop is below 300 steps, or the motor stops in an early phase |
| 4 | `0x2000` | calibration ends and is accepted with a travel of 300 to 899 steps, after the last attempt |
| 5 | `0x2800` | the low-voltage sequence of code 13 ended its reference move |
| 6 | `0x3000` | calibration fails with error 6: the voltage check refuses the start, or the motor driver is not ready |
| 7 | `0x3800` | the voltage check refuses or stops a move (supply at 1699 or less during the move, or code 13 is latched), or the motor driver is not ready |
| 8 | `0x4000` | a move to the closed end runs 200 steps past the calibrated closed position and finds no stall. The pin gives no resistance: the valve is not on a radiator, or the calibration does not fit the radiator |
| 9 | `0x4800` | a move to a position between the ends stops on a stall |
| 10 | `0x5000` | a move is requested while the motor has no reference position, or the target is above 3500 steps. The valve starts a calibration |
| 11 | `0x5800` | a move or a calibration returns a result that the state machine does not know |
| 12 | `0x6000` | move result 8. No code path that produces this result is found |
| 13 | `0x6800` | the voltage under motor load (tag `0273`) reads 2149 or less on two checks in sequence (@0x1d618). The valve then makes one last move and refuses the next ones |
| 14 | `0x7000` | a move to the closed end stalls more than 100 steps before the calibrated closed position |
| 15 | `0x7800` | a move reaches its target after the driver saw a load event |

A plant measurement agrees with code 8. A valve that is off its radiator reports
`01a3 = 0x4000`. After the valve is mounted it sends `d/*/mnt` and `01a3 = 0`.

The daemon names each state code and rates it. A warning keeps `fault` false: the valve
works, or it recovers with a new calibration, new batteries or a mount on a radiator.

| Code | Name | Rated as |
|---|---|---|
| 1 | `calibrationNoPinContact` | warning |
| 2 | `calibrationNoEndStop` | fault |
| 3 | `calibrationTravel` | fault |
| 4 | `shortTravel` | warning |
| 5, 7, 13 | `lowVoltage` | warning |
| 6 | `calibrationPower` | fault |
| 8 | `notMounted` | warning |
| 9 | `stall` | fault |
| 10 | `recalibrating` | warning |
| 11 | `unknownResult` | fault |
| 12, 15 | `state12`, `state15` | fault |
| 14 | `earlyStall` | fault |

Only code 8 is measured on a plant. The other names come from the firmware analysis
alone. Codes 6 and 7 also cover a motor driver that is not ready, which the names
`calibrationPower` and `lowVoltage` do not say. Codes 12 and 15 have no resolved cause,
so their names carry the number only.

The wall thermostat sets the same bits `0x02`, `0x04`, `0x08`, `0x80` and `0x100000`
from the same conditions. Its two interface module records (0x5bd78, 0x5bd9c) hold no
status function, so the thermostat never writes `0fa3` and its state code stays 0. The
thermostat has no other fault word. A boiler fault travels in the `hvac/*` resources
and is not part of `01a3`.

`d/*/err` carries `01a3`, `1010`, `1011` to `1015` and `02a9`. The other tags hold
diagnostic values and have no confirmed meaning.

Calibration of the valve reports in `d/*/mnt`. Tag `01fa` (`calib_state`) is the
progress and tag `01fb` (`calib_error`) is the result code of the motor
(`valve_app_fsm_event`@0xab96, result from @0xeb38). The valve repeats a failed
calibration up to `0277` times (default 3) before it reports the failure. Errors 6 and
11 are not repeated.

| `01fa` | Meaning |
|---|---|
| 0 | idle, the valve waits for the mount signal (`01b8` = 1) |
| 1 | started |
| 2 | done, `01b5` and `01b6` hold the open and the closed position |
| 3 | failed, `01b5` and `01b6` read 0 except for error 6 |

| `01fb` | Meaning |
|---|---|
| 0 | no error |
| 1 | no contact with the valve pin |
| 2 | no end stop on the closing move |
| 3 | travel below 300 steps, or a stop in an early phase, or a move that an abort or low voltage ends |
| 6 | the voltage check refuses the start, or the motor driver is not ready |

Tag `0182` of `d/*/fallback` is the device fallback mode.

| Device | `0182` | Meaning |
|---|---|---|
| valve | 0 | the control of its zone is present (zone flag `6400` is not 0) |
| valve | 1 | the control of its zone is absent (`6400` = 0). The valve feeds its own temperature to the zone (`coap_zone_ctrl_attr_write`@0x30ad8) |
| thermostat | 0 to 3 | the mode of its fallback manager (@0x31664 refuses a value above 3). 0 is no fallback and 1 is the entry mode. `tado/fallback.go` names the modes |

A valve has no mode 2 or 3. A plant measurement shows `0182 = 1` on a valve directly
after pairing and 0 some minutes later.

The `z/s` control block names `6020` `zone_kind` (1 heating, 2 hot water) and
`6260` `zone_channel_enable`. The flags `6160` and `61e0` carry the home and away
state of the whole home: home reads `6160=1`, away reads `6160=2`. Flag `61e0`
reads 1 at home and 0 in away, except in a zone with an away temperature, which
reads 1 in away too. Flag `6180` reads 0 in every frame and keeps its tag suffix.

### Frame counter

The frame counter is an application field in the plaintext (`tado/frame.go`). It is
not part of the CCM nonce: the nonce is the first 13 header bytes. Tests on air show
that the devices accept lower counters, so there is no anti-replay enforcement. The
Router Advertisement takes the same counter as the unicast downlinks: in every
capture, a commercial broadcast carries the last unicast counter plus one. The
broadcast counter is not in the CCM nonce either (`BuildBroadcast`, nonce =
header[0:13]).

The daemon keeps the counter monotonic across restarts for its own bookkeeping. With
`--tx` on a live radio, it stores meta `tx_counter_resume` 4096 ahead of the counter
in use before the first frame. It moves that value on the tick once less than half is
left, and resumes from it on start. A stored `broadcast_counter_resume` is read as one
more floor and is not written. The `--txctr`/`--bcastctr` seeds stay as floors. A
dry-run or a replay reads them and stores nothing.

### Enhanced ACK FCS

The last 2 bytes of the 29-byte Enhanced ACK are the software MAC FCS over the 27
bytes before it (`macFCS`, CRC-16/CCITT reflected, init 0, little-endian). The CC1101
strips only the PHY CRC. Of 6978 distinct Enhanced ACKs in the capture logs, from the
coordinator and from every device, 6865 carry a valid FCS. The 113 others show bit
errors. `ParseEnhAck` refuses a bad FCS. An Enhanced ACK closes a downlink only when
it is addressed to us, because an overheard one carries a sequence number of another
node. The FCS is no authentication. So an Enhanced ACK updates the CSL phase and period
only of a node from the installation or of one that sent an authenticated Data frame.
It creates no node state for any other EUI.

### MAC retransmission

A device that misses our Enhanced ACK sends the frame again with the same MAC seq and
the same datagram under the next frame counter, 1.5 to 3.6 s later (app and pairing
captures). A CoAP retransmission is a new frame with a new seq. The commercial
coordinator does the same: a downlink that misses its Enhanced ACK goes out again
under the same seq and the next frame counter, 2 to 6 s later (long watch capture).
freedo retries a downlink that the Enhanced ACK delivers (`confirmBySeq`) under its
first seq. A downlink that waits for a CoAP answer (`requireCoAP`, fragmented, active
GET) retries as a new frame with a new seq.

Frame Pending (FCF `79ec`) says that another frame for the same device follows. The
commercial coordinator sets it on each fragment but the last. It sets it on a last or
single frame only while another frame for the device waits in its queue. freedo sets
it on the same frames: the queue of the device holds one more downlink behind the one
that freedo builds, or a CoAP reply finds a queued downlink.

freedo takes a frame as a MAC retransmission when it has the seq and the datagram of
the last frame from that source, inside 10 s (`tado/mac_duplicate.go`). Such a frame
gets the Enhanced ACK again, a CON gets the answer it was served, and nothing else runs
again. Other bytes under the same seq are a new frame. The 10 s bound covers a seq
restart after a reboot.

### CoAP message layer

- Message IDs. The commercial coordinator takes the MID of its own requests from one
  counter shared by every device. In the app capture, the MIDs advance from `0x0efc`
  to `0x608f`. freedo does the same (`nextMID`, random start in a process with no
  stored value). The daemon stores meta `coap_mid_resume` 4096 ahead of the last MID
  with the frame counters, and seeds `nextMID` from it on start (`SeedMID`). So a
  restart does not repeat a MID that a device still holds for deduplication. MID 0 is
  valid: freedo answers a CON with MID 0 and sends a request with MID 0.
  `CoAPReq.GenerateMID` asks for a new MID. `MID` is sent as given.
- Deduplication. freedo keeps the answer to each CON that it serves itself, per device
  and MID, for `EXCHANGE_LIFETIME` (247 s), at most 8 per device. A byte-identical
  retransmission gets the same answer and is not processed again. The same MID with
  other bytes is a new request. Relayed and proxied exchanges and `time` are not
  cached. The active GETs and the proxied zone GETs are keyed by device and MID.
- MID spaces. An answer that a device addresses to us carries a MID of our counter, so
  it closes only a downlink that we authored (`confirmByMID`). A relayed ACK or RST
  carries a MID of the counter of its originator, so it closes only a queued zone relay
  (`confirmRelayByMID`).
- Reset. An empty RST that names the MID of our in-flight CON ends that exchange. The
  downlink is dropped with reason `coap-rst` and the log line `downlink-reset`, and the
  next queued downlink goes out. A retry under the same MID would read the same RST
  from the deduplication state of the device. A CON empty message (CoAP ping) addressed
  to us gets the empty RST of its MID.
- Options. The parser keeps every option value as opaque bytes, whatever its
  length. A relayed message goes out as its raw bytes. A Block2 longer than 3
  bytes is an error.
- Block2 reassembly. The daemon ignores a response block that repeats the latest block
  of an open transfer, as a MAC retransmission delivers it. A transfer lives 60 s after
  its latest block.
- Block2 serving. Block 0 fixes the document of the transfer per device and
  URI. The later blocks come from that document, with its ETag, until the last
  block or 247 s after the latest block. A block past 0 with no transfer gets
  an empty 5.03, so the device restarts at block 0.
- 6LoWPAN. The reassembler holds the raw fragments and parses CoAP once, over
  the whole datagram. A zone relay does not fragment: a hop over 127 bytes on
  air is refused with `zone-relay-too-large`.

### Router advertisement

The coordinator broadcasts an ICMPv6 Router Advertisement every 200 to 600 s: the
captured intervals run from 217 s to 592 s. This is the Contiki default of RFC 4861,
MaxRtrAdvInterval 600 s and MinRtrAdvInterval a third of it. A device that receives no
advertisement stops reporting after about 20 minutes. It then broadcasts ICMPv6 Router
Solicitations: a capture holds 180 solicitations from nine devices in 75 s. An
advertisement brings the devices back. They re-associate in four minutes.

This is the commercial answer to a solicitation, measured in the pairing capture. It is
the only capture of the commercial bridge that holds solicitations, all from the joining
valve.

- The bridge sends advertisements on the tick of a 1 s timer. In the eight captures, the
  wake-up trains start on one 1 s grid per capture. Three trains start 12 to 22 ms after
  a CSL exchange ends: the tick comes while the radio is busy. The answer train starts
  0.02 to 0.89 s after the solicitation. In one case, the train starts 22 ms after a
  `d/config` exchange.
- A solicitation less than 3 s after the last advertisement ended gets none, now or
  later (five cases, 0.4 to 2.8 s after). This is MIN_DELAY_BETWEEN_RAS of RFC 4861. The
  closest two trains start 4.0 s apart. A solicitation 4.2 s after gets its train.
- Some solicitations more than 3 s after get none, for a reason that the capture does not
  show (3.4 to 19 s after the last advertisement). A replay check explains some of them:
  the joiner counters sit below the counters that the bridge has already seen. It does
  not explain two of them. The sniffer RSSI does not separate the answered from the
  unanswered solicitations either: -88 and -87 dBm against -93, -80 and -80 dBm. One
  unanswered solicitation comes 255 ms after a Neighbor Solicitation.
- A solicited advertisement restarts the periodic interval. In the pairing capture, the
  last train that follows no visible solicitation comes 619 s before the capture ends. A
  periodic timer that kept running from it would send one more within 600 s. The four
  advertisements in that span each follow a solicitation within 0.9 s, and none comes in
  the last 308 s.

The advertisement carries option 25 with the IPv4 netmask 255.255.255.255, a gateway and a
DNS address. The two addresses travel as IPv4-mapped IPv6 addresses. The commercial bridge
advertises the gateway and the DNS address of its LAN. freedo sends `options.router_gateway`
and `options.router_dns`, and `0.0.0.0` while they are unset. This is not verified on a
device.

freedo answers a solicitation with one burst at once, and with none when the last burst
ended less than 3 s before. The advertisement is a broadcast, so the burst on air answers
every device that asked. freedo also answers the solicitations that the commercial bridge
leaves unanswered for no visible reason, because a device without an advertisement stops
reporting. The periodic beacon draws each interval between a third of
`--beacon`/`beacon_sec` and all of it, 200 to 600 s by default. An answer to a
solicitation starts a new interval. Both the beacon and the answer need an armed
transmitter. A dry-run logs the solicitation instead.

### Link layer, planes and report timing

These facts come from a long passive recording of a commercial coordinator at work (the
heat-demand capture).

MAC layer:

- Only the coordinator sends wake-up trains. No train targets the bridge, because the
  coordinator receiver stays on. A device sends its uplink without a train. A train
  carries the sequence number and the short destination of the Data frame that follows.
  The Data frame starts 9 to 24 ms after the last wake-up frame (median 12 ms).
- A train has one of two shapes: rendezvous 248 to 0 in 21 frames over 79 ms, or rendezvous
  3125 to 0 in 254 frames over 1.003 s. The frame step is 12.5 rendezvous units, which is
  4.0 ms. A broadcast (`0x49e8`) goes behind a full 3125-unit train to `ffff`.
- An Enhanced ACK follows a Data frame 5.0 to 8.0 ms after it (median 6.0 ms).
- A retransmission keeps the MAC sequence number and increments the application counter. The
  gap is exactly 2.0 s or 3.0 s, with up to three attempts.
- The application counter increments once per transmitted frame and per source, with no reuse.
- Frame Pending (`0x79ec`) means that more frames are queued on the same link, not that the
  frame is fragmented. Every frame with it was followed by another frame within 5 s. For plain
  `0x69ec` frames this was true in 43 % of the cases.

Addressing planes:

| Plane | Query form | 6LoWPAN | Use |
|---|---|---|---|
| device | `?id=N`, or none | link local, both interface ids from L2 | the bridge commands a device |
| home | `h/<home>/...` | link local | a device reports to the bridge and the cloud |
| zone | `?lid=N` | context 0 | a device talks to a device, the bridge relays at L2 |

The bridge relay adds exactly 1 byte to the frame: the in-line hop limit, because the relayed
datagram can no longer compress 63 into the IPHC HLIM field. The relay latency is 0.15 to
2.7 s, which the 1 s CSL period bounds.

Heat demand chain, detail:

- A valve publishes `40a0` on two paths. One is `PUT h/<home>/z/N/act` with `40a0` alone.
  The other is a `2.05` message of 75 bytes with `4060` (zone temperature), `40a0` and
  `0135` (humidity): the `z/p` observe notification of the owner. The bridge relays that
  message to the wall thermostat as 76 bytes.
  Every zone device does this.
- The wall thermostat publishes `PUT h/<home>/c/N/act` with `4040`, `4000`, `4080` (circuit
  demand) and `2090` (heat active). `4000` and `4040` are the target and the temperature of the
  circuit lead zone (`LeadZoneTargetC` and `LeadZoneTempC` in the API).
- Timing after a setpoint write that raises demand: the valve answers `2.05` with `40a0=100`
  after 1.25 s and the relay reaches the thermostat at 2.2 s. The thermostat sends `z/cpe`
  with `4160=100` at 3.3 s, `c/N/act` with `4080=100` at 4.2 s, `z/N/act` with `40a0=100` at
  12 s, `c/N/act` with `2090=1` at 16 s, `hvac/mon` with `0450=40.00 C` at 41 s, and
  `0452=53` with `0457=1` (burner on) at 45 s.
- Timing after demand falls to 0: `z/cpe` with `4160=0` at 5.8 s, `c/N/act` with `4080=0` at
  8.4 s, `z/N/act` with `40a0=0` at 22 s, `hvac/mon` with `0450=0.00 C` at 51 s, `2090=0` at
  52 s and `0452=0`, `0457=0` at 60 s. Demand at 100 % lasted 205 s, `2090` stayed 1 for
  44 s after the demand reached 0, and the burner ran 219 s.
- The fan-out of `z/cpe` to six zones takes 41 s on the rising transition and 50 s on the
  falling one. The thermostat sends one `z/cpe` per zone and the bridge relays it to each
  device of that zone.

Boiler link: `hvac/mon` frames carry only changed attributes. `0450` is the control
setpoint and ramps (40, 46, 48 C), then falls to 0 when the burn ends. `044c` reads
39.10 C and then 46.40 C during the burn and `0458` changes in most frames. The meaning
of these two is not resolved. `2040` never appears in these frames.

Write bodies (decrypted, byte-exact). The home was in the state `6160=2` at capture. Tag
`6200` is absent in both:

```text
manual overlay, 38 bytes:
61600102 61800100 60200101 61e00100 62400102 62600101 62800209c4 62e00100 6440020001
return to schedule, 29 bytes:
61600102 61800100 60200101 61e00100 62400100 62e00100 6440020001
```

The two downlinks carry tokens of different lengths (2 and 8 bytes), which explains why the
frame is 3 bytes shorter and the body 9 bytes shorter.

The zone-plane echo of a block (`PUT z/s?lid=N`) uses another tag order, with no `6440`:
`6280 6240 6260 61e0 6160 6180 6020 62e0`. After a return to schedule the valve still reports
the last `6280` together with `6240=0`. The applied schedule setpoint appears about 5 s later
in `c/N/act` tag `4000`.

After a `PUT d/config`, the valve repeats one sequence:

- `2.04` and `GET d/<serial>/config` after 1.8 s.
- The `2.05` answer after 3.0 s.
- A zone push to the thermostat after 4.1 s.
- Five GETs of `h/<home>/z/N/config` with fragmented answers from 4.2 s.
- Then, within 22 s: `PUT z/p?lid=N`, `d/<serial>/sen`, `d/<serial>/act`,
  `h/<home>/z/N/act`, `d/<serial>/err` and `d/<serial>/fallback`.

The `2.05` answer to the device read carries the nine first tags of the document and
omits `015a`. The offset in `0140` is the difference of `012d` and `012e`. A residual of
0.01 to 0.03 C comes from rounding, because the device applies the offset to a value with
more resolution than `012e` reports.

Open questions on tags:

- `z/p?lid=N` carries `0135` with a value that differs from the `0135` of `d/<serial>/sen` for
  the same device (62.70 % constant over 594 s against 64.20 to 64.80 %). `4060` in the same
  payload matches `012d` exactly.
- `0273` and `027c` stay 150 to 250 units below the battery voltage `0162` reports (in mV). The
  data do not choose between a loaded supply voltage and a stroke time in milliseconds.
- `0265` and `0294` pair with the valve travel. The data do not separate an absolute position
  from the length of the last movement.

### Firmware state and transfer

The `d/fw/state` document holds the device type (`01a0`), the running internal slot
(`003a`/`003b`), the external slots 0x10 (`0035`/`0036`) and 0x11 (`0039`/`003c`), the
build hash (`0210`) and `0285`. A version is `major<<6 | minor`. The status nibble holds
four bits: bit 0 pending, bit 1 image, bit 2 active, bit 3 confirmed. Valves and the wall
thermostat both report type 8, so the type does not tell the hardware.

The coordinator sends `POST d/fw` with Block1 and blocks of 128 bytes, one block at a
time. Block 0 opens with a 20-byte header: `01a8` size (4 bytes, without length bytes),
`01a9` unused, `01aa` CRC16, `01ab` version. The CRC is CRC-16/CCITT-FALSE over the whole
slot window of 0x73000 bytes, padded with 0xFF. The image is the application linked at
0xa000. The device refuses to start a transfer when its running image is not confirmed.
The bootloader activates a pending image only with a version equal to or above the running
one. The device confirms the new image at the first `2.xx` answer to its
`PUT d/<serial>/fw/state`. Nothing on the device checks that an image is for its hardware.

No transfer has run on air. The tests use synthetic data only.

## Firmware bridge v2

Architecture and build: [`firmware/cc1101_bridge/README.md`](../firmware/cc1101_bridge/README.md).

Hardware validation:

- Passive RX: 1027 CRC-OK frames in 4 min, with 20 frames of 64..113 bytes intact.
- Live coordinator session: 8 warm CSL transmissions, every one `sent=22` (bound 3..25),
  zero TXFAIL and zero missing-anchor errors. The setpoint and return-to-schedule downlinks
  arrived with matching CoAP mid and token.

`Bridge.Send` (Go): with `MCSM1=0x3F` (TXOFF=RX), the chip never returns to IDLE. So TX
completion is "MARCSTATE out of 0x13/0x14 and `TXBYTES&0x7F == 0`", polled at 1 ms.

Idle drain: frames that the bridge catches between transactions get the Enhanced ACK and
go to the CSL inbound ring, while the bridge knows a coordinator. The daemon polls CSLQUEUE
before every receive window, so a stored frame reaches the emulator within one window. This
is validated on air with firmware v2.3: no CSL queue poll stall in an 80 minute live
session. A double ACK on air is not measurable without a second receiver.

Serial resync (`cc1101/bridge.go`): a reply carries no command echo, and the firmware
answers every request once and in request order. Each request checks the shape of its
reply. A reply of the wrong shape is the late reply of an earlier request, so the request
reads past it, at most 2 times, to its own reply. A request that misses its deadline leaves
its reply owed. The next request first sends a PING. It reads every reply before the
`CC1101BRIDGE` banner as an owed reply, in request order. A late poll reply goes to a queue
that the next polls return first, so no completion and no auto-ACKed uplink is lost. A
drain that stays busy for 3 s is an error.

Bridge reboot: an ESP32 reset (watchdog, brownout) with the USB link up leaves the CC1101
on reset values, IOCFG0 `0x2F`. Every request still gets an answer, with empty polls and
empty receive windows. The live owner reads IOCFG0 back after a lost CSL completion, and
after 40 receive windows (about 10 s) with no CRC-OK frame. Any value but `0x07` ends the
owner. The daemon then exits 1, so its supervisor restarts it and configures the chip.
`Configure` flushes the CSL queue before the reset.

Firmware v2.5: `CMD_RESET` clears the CSL queue, its completions and its
inbound ring. `CMD_RECV` and `CMD_AUTOACK` leave the radio in RX, so the idle
drain answers an uplink between two host windows. A full inbound ring gets no
Enhanced ACK.

Firmware v2.8 follows the CSL transmitter of the commercial coordinator
(`csl_tx_statemachine` at 0x36948, `csl_compute_rendezvous` at 0x47574 and
`csl_enhack_build_send` at 0x367b8 of `tado_stm32f411.bin`, timer ticks of
32 us), except for the listen before transmit, the drift estimate and the
Enhanced ACK phase.

- Listen before transmit: left out, because it broke delivery on air. The commercial
  coordinator waits a random 0 to 6 x 20 ticks, listens 124 ticks and gives up with a
  collision status when a packet is on the air. On-air bisection, 15 min per build, same
  host code, same plant: v2.5 plus that listen alone (3.97 to 7.81 ms, ending where the
  train starts, BUSY `0x1b` on a packet) got CSL ACK 43.9%, against 81.7% for v2.5 and
  79.6% for v2.5 plus the anchor changes below. Jobs without the bootstrap flag: 33/102
  against 78/100 and 49/61. Jobs with it: 35/53 against 65/75 and 41/52. One job ended BUSY.
  The 1 s train kept its rate (27/33 against 22/25). The anchored 80 ms train lost it
  (41/109 against 121/147), also right after a refresh: an anchored job within 1 s of an
  answered anchored job got 6/17 against 50/52. 23 of 33 unanswered anchored CoAP ACKs came
  back as device retransmissions, against 3 of 12 with v2.5, so the devices did not take the
  Data frame. The root cause is open. The host keeps `cc1101.ErrCSLTxBusy` for status
  `0x1b`, which v2.8 never sends.
- Anchor life. The commercial coordinator keeps a neighbour until 4 missed
  Enhanced ACKs. The bridge has no age limit and keeps the anchor until 4
  misses in a row.
- Drift. The bridge measures none and follows the nominal period of 1 s. The commercial rule
  (two samples at least 60 periods apart, error above 480 us per period discarded) divides
  the error of samples that are stamped to 32 us. The bridge stamps a sample on the GDO0 edge
  of the Enhanced ACK. A host simulation (refresh intervals of the commercial captures, 90 s
  refreshes, 30% of stamps up to 20 ms late) gives 92% of anchored trains on the sample with
  the estimate and 100% without it. The device clocks run within -6 to +18 ppm of the capture
  host over 15 h of long watch captures, each on one continuous grid of 1 s. The guard covers
  this without an estimate.
- Guard. The commercial train is a fixed 79.4 ms around the sample. The bridge
  starts at the same 40 ms on each side and adds 30 ppm of the time since the
  last refresh, up to half a period, where the train covers the whole period
  as the bootstrap train does.
- ACK stamp. A refresh takes the GDO0 stamp only when it is later than the Data
  frame, else the instant the ACK is read. An earlier edge belongs to another
  frame and puts the anchor up to one period off.
- Enhanced ACK phase. The bridge sends the phase of the host request, as v2.5 does, and keeps
  the last one known for a CSLTX without the follow flag. The commercial phase is free
  running: the timer in units of 320 us, truncated to 16 bits. In both capture sets, the phase
  minus the capture time in those units stays constant within 10 units. The valve stores the
  phase as the coordinator anchor, `rx - phase x 10` ticks, with the same drift rule and
  4-miss eviction (`csl_nbr_period_update@0x2fed4`, from `csl_transmit_frame@0x15f94`).
  `mac_periodic_next_wake@0x2fe00` projects that anchor to the next coordinator sample.
  `csl_tx_sample_offset_get@0x2ffd0` defers an uplink past 15.6 ms (488 ticks), and
  `csl_tx_wait_sample_window@0x2ffe8` busy-waits for a closer one. The static reading found no
  read of that entry in the own sample schedule of the valve (`csl_period_timer_update@0x15878`,
  `csl_sched_restart@0x158e8`). Device uplinks in the commercial captures fall uniformly over
  the advertised grid, so the deferral is not seen on air. Open: whether a phase that jumps
  between ACKs moves valve uplinks onto the valve's own sample.
- On air, v2.6 (drift estimate, free-running phase, a listen that pushed a close sample one
  period later) against v2.5, in the first 15 minutes after a restart: CSL ACK 45.7% (122/267)
  against 75.2% (179/238), and 21 against 8 "no target sample anchor". Anchored jobs of v2.6:
  71 ACK, 95 sent with no ACK, 25 busy (`sent=0`). So 43% of the anchored trains on air got an
  ACK, against 77% (112/146) with v2.5. The trains went out and were not answered. The
  simulation does not reproduce the drop with any stamp error below 20 ms. v2.6 also listened
  before the train, and the listen alone gives the same drop. So this run does not tell whether
  the free-running phase is harmful. The bridge keeps the host phase.

Firmware v2.9 adds the pair window (`CMD_PAIRWIN`, see Pairing). It also takes a Frame
Pending advertisement (`59e8`) in `CMD_RABURST`. The beacon answer runs only in the idle
drain and in `CMD_AUTOACK`, never in the CSL state machine, and only while the host keeps a
window open.

A host harness compiled from the sketch lines answers the three captured requests with the
captured beacons byte for byte. Compare a v2.8 build with the same toolchain. Every function
of the CSL and Enhanced ACK paths compiles to the same instructions (`spiXfer`, `ccSendFrame`,
`rxPump`, `ccAutoAck`, `cslStart`, `cslSendWake`, `cslSendData`, `anchorNextSample`,
`cslDrainWindow`, `cslStoreInbound`, `cslDeliver`, `rxWaitFrame`). Only literal and call
addresses move. `radioTask` inlines the idle drain and the CSL steps. It makes the same calls
plus the `pairAnswer` call of the drain, with one block placed elsewhere. The beacon code has
its own copy of the MAC FCS and reads the GDO0 stamp itself. A build that shared `macFCS` lost
the constant length 27 in the ACK code and gained a `memw` barrier in `spiXfer`.

Link-maintenance echo. The commercial coordinator sends the ICMPv6 echo to a device that it
holds no sample for behind a train of one period (253 wake frames, first rendezvous 3125, in
a long watch capture). It sends the echo to a synced device behind the short train. freedo
sends the echo with `CSLBootstrapAlways`: the bridge uses the anchor when it has one with no
miss, else the train of one period. The echo does not end as "no target sample anchor".

### FREQEST and autotune

This module has a stable offset of -13 FSCTRL0 units of 1.587 kHz each. The measurement ran at
FSCTRL0 0x40 and FOCCFG 0x16 over 5 minutes of receive with the commercial coordinator on:

| frames | n | mean | sd | clipped at -21 |
|---|---|---|---|---|
| WakeUp train, coordinator, 12 bytes | 2215 | -13.0 | 0.6 | 0 |
| Data, isolated valve and thermostat uplinks | 17 | -13.0 | 0.0 | 0 |
| Data, coordinator, 60 bytes or more | 14 | -11.7 | 6.3 | 14% |
| Data, coordinator, under 60 bytes | 13 | -11.1 | 1.4 | 0 |
| Enhanced ACK, coordinator | 23 | -14.5 | 7.0 | 17% |

FSCTRL0 0x33 leaves a residual of 0. Of 823 WakeUp frames, 710 read exactly 0, with sd 0.8.
Long Data frames read mean +1.0 with sd 1.6. Frame length is not the discriminator, because
the WakeUp frames are the shortest and the cleanest. An isolated frame reads the offset
exactly. A coordinator frame that follows another frame within about 25 ms scatters and clips
at the FOC limit (BW/4 = 21 units). Two cases: a Data frame after the wake-up train, and an
Enhanced ACK after a device uplink.

FOCCFG stays 0x16. The value 0x36 (FOC_BS_CS_GATE=1) makes the WakeUp sd bimodal at 1.4,
disables the estimate on the Enhanced ACK (which then reads a constant 0), and loses 46% of
the WakeUp frames. A tune misses this module in three cases: it rejects a correction above 12
units; it wants 2 sources within 15 s while Data frames arrive at about 10 per minute; or it
probes FSCTRL0 0x50, where the residual of -29 is past the FOC limit.

`cc1101.TuneSample` reads a frame that the tune may use: a CRC-OK frame that carries a FREQEST
byte and that `freqSource` labels. `freqSource` labels the Data frame (first byte 0x69 or 0x79)
with its source address and the Broadcast frame (0x49 0xe8) with its source address. It labels
the WakeUp frame (0x25), which carries no source address, with the fixed label `coordinator`.
The Enhanced ACK (0x42 0xee) stays out.

`cc1101.TuneCorrection` drops the readings with `|FreqEst| >= 20`, takes one median per source,
and returns the median across sources with the number of readings it kept. When at least a third
of the readings clip on one side, the drop removes the readings that carry the offset. In that
case it returns the clip value as the correction, and the caller writes the register. `TuneWant`
is 20, `TuneMin` is 5, `TuneDeadband` is 4.

Firmware v2.2 reports FREQEST on all four receive paths. The stream frame keeps
its own marker byte. The CMD_RECV response, the AUTOACK response and the CSLQUEUE
inbound record append the register after the LQI/CRC byte, so the blob reads
`[len][payload][rssi][lqi][freqest]`. The firmware reads the register once, at
the instant the FIFO pump completes the frame, and every path copies that byte.
`RxFrame.HasFreqEst` says whether a reading is there, so the host runs on v2 as
well.

The daemon writes FSCTRL0 once, at start, from the `meta` table under `fsctrl0`, and never while
it runs. The Tado bridge, valve and thermostat firmware do the same: one constant register table
with FSCTRL0 0, and the hardware FOC (FOCCFG 0x16) absorbs the offset of each device. This module
sits about +81 kHz off (FSCTRL0 0x33), outside the FOC range, so it needs a static trim.

The trim is a passive calibration. Run it once per new chip or installation: `freedo-dev autotune`
runs `Bridge.Autotune` in receive only and stores the result under `fsctrl0`. `GET /api/health`
reports `fsctrl0` and `calibrated` in live mode and, without a stored value, a `warning` that names
the command. The bootstrap key `radio_tune` is accepted and ignored, with one warning at start.

FREQEST is telemetry only. `tune_debug` (`--tune-debug`) logs one line per received frame that
`TuneSample` labels, on the CMD_RECV, AUTOACK and CSL inbound paths. The line shows the time since
the previous frame of that source and since our own last transmission. A CSL inbound frame arrives
right after our own transmission, before the FOC loop converges. With TX armed, those frames spread
the per-source medians from -14 to +15 units. The same devices read 0 in passive receive at the same
FSCTRL0.
