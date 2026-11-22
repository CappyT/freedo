# cc1101_bridge

This firmware is almost completely AI-generated. I have no experience writing radio firmware. It was tested on one ESP32 development board with one CC1101 module only, and it may be completely wrong. If you are willing to help, a pull request is accepted.

The firmware has not run on the radio board design in [`hardware/radio`](../../hardware/radio/README.md), whose pin map differs, or on any other module.

Firmware for the ESP32 + CC1101 radio bridge. The host keeps all CC1101
semantics. The bridge frames SPI transactions and runs the timed operations that
a USB serial round trip is too slow for.

All three receive paths report FREQEST. The host driver in `cc1101/` reads that
byte only when it is there.

## Serial protocol

The contract with the host driver:

- Request frame `A5 cmd lenHi lenLo payload crc`, response frame
  `5A status lenHi lenLo payload`, 230400 8N1 (921600 before v2.10, see
  [Serial rate](#serial-rate)), payload limit 1024 bytes.
- The request CRC is CRC-8 with polynomial `0x07`, init `0x00`, no reflection and
  no final xor, over `cmd`, `lenHi`, `lenLo` and the payload. A request with a bad
  CRC is dropped and gets `5A E4 00 00`, so the host still reads one reply per
  request.
- Commands `01` PING to `0D` CSLQUEUE, with the same request and response layout.
  `0E` PAIRWIN is new in v2.9, see below. `0B` RABURST also takes a frame with
  FCF `59 E8`.
- `0F` CONFIGURED has an empty payload and answers `00`. The host sends it when its
  register load ends.
- `10` AIRTIME with an empty payload answers `00` with 8 + 2 x 61 bytes: the
  milliseconds on air over the window and the budget in force (0 means no cap), both as uint32 big
  endian, then 61 uint16 big endian per-minute values in ms, oldest first, the
  current partial minute last. A payload of 122 bytes (61 uint16 big endian, same
  order) loads the buckets, so the last value is the current minute. The array
  with the larger total wins, whole: when the loaded total is larger than the
  window total, the window takes the loaded array, also when the window holds
  airtime that the loaded array lacks. Otherwise the window keeps its own count
  unchanged. It never mixes the two arrays bucket by bucket. The host loads the
  last array that it read from the bridge, so the airtime that the bridge sent
  after that read is the only airtime that a load can erase. A payload of 126 bytes
  loads the buckets and then sets the budget in ms as uint32 big endian, 0 for no cap. It answers like a
  query. Any other length answers `E1`. The budget is 0 after boot and after RESET, so the host
  sends it at every start. The command is allowed while the flag is false: the host loads the
  buckets and the budget before CONFIGURED.
- `11` LBT takes 2 bytes, enable (0 or 1) and the threshold as int8 dBm. It answers `00`
  with the same 2 bytes. Another length answers `E1`, an enable byte above 1 answers
  `E3`. It is off with -80 dBm after boot and after RESET. The command is allowed while
  the flag is false: the host sends it before CONFIGURED. See Listen before talk in Design.
- Status bytes `00`, `10` to `1D` and `E0` to `E4`. `1B` BUSY, `1C` UNCONFIGURED and `1D`
  AIRTIME are new in v2.11, see Listen before talk, Configured flag and Airtime in Design. `1D`
  comes only when the host set a budget.
- Stream markers `5A F1` frame with FREQEST, `5A F2 00` end, `5A F3 00`
  heartbeat every 500 ms.
- CSLQUEUE ENQUEUE, POLL and FLUSH records and completion records. The inbound
  record keeps its layout and adds the FREQEST byte at the end. The RECV and
  AUTOACK responses add the same byte at the end.
- Neighbour table of 16 entries and a per neighbour depth of 32 jobs, so one
  fragmented datagram still goes out as one batch.
- Wake burst cadence, rendezvous countdown payload, 500 ms follow window and
  ACK timeout semantics. An anchored job can report up to 255 frames, because
  its guard grows with the age of the anchor.
- The PING banner keeps the `CC1101BRIDGE` prefix and reads
  `CC1101BRIDGE v2.11 (<build>)`. The build is the release version of a released
  image, for example `0.3.1`, and `dev` for a local build. The host tests only the
  prefix. A reply carries no command echo, so after a
  request misses its deadline the host sends a PING and reads every reply
  before the banner as a late reply, in request order.

## Design

**FREQEST on every receive path.** A receive blob is
`[len][payload][rssi][lqi/crc][freqest]`: the `CMD_RECV` response, the AUTOACK
response and the CSLQUEUE inbound record append the FREQEST register after the
LQI/CRC byte. The stream frame carries the register in its own marker. The
firmware reads the register once, in the radio task, at the instant the FIFO pump
completes the frame. The assembler holds that byte next to the frame buffer, and
every emitter copies it. The read comes before the strobe or the transmission
that follows the frame: the Enhanced ACK takes the demodulator off the packet and
scatters the reading to a standard deviation of about 8 units. The host folds the
value back into FSCTRL0, so it tunes on the frames it receives anyway instead of
on a dedicated stream window. An older host reads the blob by its length byte and
ignores the trailing byte.

**Request CRC.** `serialTask` reads one CRC byte after the payload and compares it
with the CRC it computed. A byte lost inside a request no longer turns later bytes
into a command. A bad request goes to no queue and answers `E4`. A request
longer than 1024 bytes still answers `E1` with no CRC check.

**Configured flag.** The flag is false at boot, after `CMD_RESET` and after the
bridge finds the chip reset. `CMD_CONFIGURED` sets it. While it is false,
every command except PING, XFER, GDO, RESET, STROBE, CONFIGURED, AIRTIME and LBT
answers `1C`, the frame send refuses, the queue starts no job and the idle drain
answers and stores nothing. A chip with reset values sits at 800 MHz with no
calibration, so it must not transmit. The idle tick reads IOCFG0 every 8 ms,
except in the 500 ms after a host XFER or STROBE and while a transaction or a
command runs. Every frame send also reads IOCFG0 before it transmits. The value
`0x3F` is the reset value, a 135 kHz clock on GDO0. A read of `0x3F` is read again.
When both reads give `0x3F`, the bridge writes `0x2F` to IOCFG0 and IOCFG2, which
quiets the pins, clears the flag and, in a frame send, refuses the frame.

**Airtime.** Every frame that leaves the chip adds its time on air to a sliding
hour: preamble 4, sync 4, length 1, payload and CRC 2 bytes, 160 us per byte. The
window is 61 buckets of one minute, so it is 60 to 61 minutes long and covers a full
real hour. The firmware always counts, and it has no budget after boot and after RESET: nothing is
refused. The host can set one with `CMD_AIRTIME` (the airtime limit option, alpha, not verified on a
plant: [issue 42](https://github.com/CappyT/freedo/issues/42)). The budget follows the Tcum_on of
polite spectrum access in ETSI EN 300 220-2 (clause 4.6, band M, per 200 kHz): 36000 ms with no
listen before talk and 100000 ms with it. An Enhanced ACK is always sent and always counted. With
a budget, any other transmission refuses to start while the sum is at or above the
budget: `CMD_CSLTX`, a CSLQUEUE ENQUEUE, `CMD_TXTRAIN`, `CMD_RESPOND` with a
response train and `CMD_RABURST` answer `1D`, a queued job completes with `1D`,
and a pair beacon is not sent. A job that started sends all its frames, so one
job can cross the budget. A frame that the host sends with `CMD_XFER` or
`CMD_STROBE` is not counted.

**Host timeout.** With no valid request for 5000 ms the bridge forgets the
coordinator, closes the pair window and clears the queue, the completion ring and
the inbound ring. The host sends a request at least every 1.25 s. A command that
ends counts as a request, so a long `CMD_RECV` or `CMD_STREAM` does not trigger
the timeout.

**Two tasks.** `serialTask` on core 0 parses requests and writes every response
byte. `radioTask` on core 1 owns SPI and all radio state. No other code touches
SPI. The serial port is never left unread while the radio works, so a 1024-byte
request cannot overrun the UART buffer. The receive buffer is 2048 bytes.

**One state machine.** The states are IDLE, RX, WAKE, ACK and FOLLOW. The queue, the
synchronous `CMD_CSLTX` and the `CMD_RABURST` wake burst all run as a job on it.

**Arbitration.** A command that needs the radio waits until the machine is idle, so a
host `SIDLE` strobe cannot land in the middle of a transaction. PING, GDO, CSLQUEUE,
CONFIGURED, AIRTIME and LBT answer at once, so the host keeps polling the queue while a
transaction is in flight.

**SPI profile.** Every access runs at 6 MHz with 1 us between CS low and the first
clock. The datasheet gives 150 us only for a chip in power-down, and the bridge
never sends SPWD or SXOFF. The slow profile (1 MHz, 150 us) runs only inside the
power-on reset sequence. The computed delay from the end of an uplink to the start of
its Enhanced ACK is about 0.4 ms. It is not measured on air.

**Interrupt driven receive.** GDO0 gives the end of frame instant in the
interrupt handler, with a 64-bit timestamp. GDO2 gives the RX FIFO threshold,
which drains a frame longer than the 64-byte FIFO. The firmware does not poll
RXBYTES in a loop with nothing to read. A tick every 8 ms pumps the
FIFO anyway and finds an overflow, which raises no interrupt. The FIFO overflows
about 11 ms after the frame starts, so one lost GDO2 edge costs latency and not
the frame.

**Idle receive.** Between transactions the chip stays in RX, because MCSM1 has
TXOFF_MODE=RX and RXOFF_MODE=RX. `CMD_RECV` and `CMD_AUTOACK` also end in RX,
with a frame that already started left in the FIFO. The idle drain removes those
frames, which keeps the FIFO clean. A frame with a bad CRC needs no flush: it is
out of the FIFO already.

**No strobe in RX.** `CMD_RECV`, `CMD_AUTOACK`, a CSLQUEUE FLUSH, the end of every job
and the start of every job except a Router Advertisement read MARCSTATE and RXBYTES. They
send SIDLE, SFRX and SRX only when the chip is not in RX, or when the FIFO
overflowed. SIDLE and SFRX on a chip in RX drop a frame that is on the air or in
the FIFO, for example the second fragment of a datagram that follows an Enhanced
ACK. The assembler keeps its state. The
drain pauses for 500 ms after `CMD_XFER`, `CMD_STROBE` or `CMD_RESET`, because
the host drives the radio itself in that case. While the bridge knows a
coordinator, the drain answers a frame addressed to it with the Enhanced ACK and
puts every CRC-OK frame except a wake frame in the inbound ring. A full ring
of 32 frames gets no Enhanced ACK, so the device retransmits the uplink
instead of losing it. The host polls `CSLQUEUE` before
each receive window, so a stored frame reaches it within one window. `CMD_AUTOACK`,
`CMD_CSLTX` and a CSLQUEUE ENQUEUE set the coordinator. `CMD_RECV`, `CMD_STREAM`,
`CMD_RESPOND`, `CMD_RESET` and a CSLQUEUE FLUSH clear it, so a host that stops
transmitting leaves a drain that answers nothing and stores nothing.

**Reset clears the queue.** `CMD_RESET` also cancels the queued jobs, the
completion ring and the inbound ring, as a CSLQUEUE FLUSH does, and clears the
configured flag. The host loads its register set next, and a job a crashed session
left behind must not transmit on a chip that is half configured. The anchors stay.

**Stable status reads.** A helper reads a status register until two reads agree.
TI errata: RXBYTES, TXBYTES and MARCSTATE read while the radio is active can
return a value in transition.

**Clean FIFO before every transmission.** Each frame the bridge sends starts
with SIDLE, SFRX and SFTX in one SPI access, and resets the receive assembler.
SIDLE keeps the bytes of a frame cut off mid-air in the RX FIFO. A flush after
the TX arrives too late: MCSM1 TXOFF_MODE=RX re-enters RX the instant the TX
ends, and an Enhanced ACK follows about 192 us later. A failed TX re-arms RX
with an empty RX FIFO.

**Frames in the ACK window.** The bridge sends no auto-ACK in the ACK window,
because that TX would cover the Enhanced ACK of the target. A CRC-OK frame of
another node goes to the inbound ring without an answer, and that node
retransmits. Only a frame that can be a damaged Enhanced ACK turns NOACK into
BADACK: a frame with a bad CRC, or a frame with the Enhanced ACK frame control
`42 EE`. A frame with a bad CRC in the follow window takes no follow slot,
counts in the follow `dropped` byte and goes to no inbound record: its payload
fails the MIC on the host.

**Outcome before watchdog.** The machine looks at the outcome of a transaction
before it looks at the transaction guard, so an OK outcome that waits for a free
completion slot is not reported as TXFAIL. The guard comes from the parameters of
the job: whole wake window, plus ACK timeout, plus follow window, plus 400 ms of
margin. The legitimate worst case is about 2.6 s.

**Listen before talk.** `CMD_LBT` switches it on. It is off after boot and after
`CMD_RESET`, with a threshold of -80 dBm. It follows the polite spectrum access of
ETSI EN 300 220-2 (clause 4.6).

The listen runs before the first frame of a CSL wake-up train (a queued job and
`CMD_CSLTX`), of `CMD_RABURST`, of `CMD_TXTRAIN` and of a pair beacon. It does not
run before an Enhanced ACK. The ACK leaves about 1 ms after the uplink, and the
listen needs 250 us for the RSSI and 160 us for the sample, so the window cannot hold
it. The commercial bridge also sends its Enhanced ACK without a listen. The listen
does not run before the Data frame that follows a train, because the train and the
Data frame are one dialogue. It does not run before the response train of
`CMD_RESPOND`, which answers a frame that just arrived.

One listen puts the chip in RX when it is not, waits until the RSSI is valid, then
reads RSSI and PKTSTATUS over SPI for at least 160 us. The RSSI updates every 59 us,
so that is three updates or more. Both GDO pins are in use, so the bridge polls. The
RSSI is valid 250 us after the chip enters RX. That value is calculated from datasheet
17.3 and DN505 and is not measured. A chip that has been in RX for longer needs no
wait. RSSI_dBm = signed(raw) / 2 - 74 (datasheet table 31). The channel is busy when
one sample is at or above the threshold, or when PKTSTATUS shows a sync word.

A busy channel gives a random backoff of 0 to 6 x 640 us, as in the commercial
bridge, then a new listen, up to 3 listens. After the third busy listen the job
completes with `1B`. A queued job reports `1B` in its completion record.
`CMD_CSLTX`, `CMD_RABURST` and `CMD_TXTRAIN` answer `1B` with 0 frames sent. A pair
beacon is not sent and keeps its seq and counter. The backoff of a job does not block
the state machine, so an uplink that arrives meanwhile is answered. The backoff of
`CMD_TXTRAIN` and of a pair beacon blocks the radio task, for about 9 ms at most.

A clear channel is followed by the first frame, sent with STX from RX. MCSM1
CCA_MODE=3 (RSSI below the chip threshold and no packet in reception) is a second
gate. When the chip does not leave RX, or a frame sits in the RX FIFO, the bridge
sends nothing and the job ends with `1B` at once. The carrier sense point of the chip
comes from the AGC registers: about -90 dBm by estimate, not measured at 50 kBaud.
So this gate can be stricter than the threshold of the host.

An anchored train starts at the same instant as without the listen. The listen
starts 500 us before the start instant (`LBT_LEAD_US`: 250 us for the RSSI, 160 us
for the sample, and the SPI reads). A listen that ends early waits for the start
instant. A busy listen and its backoff move the first frame later. The train still
ends at the same instant, so it is shorter. A train without an anchor (bootstrap and
Router Advertisement) starts when the listen ends and keeps its full length. In a
pair window the listen runs inside the 3968 us gap, from 250 us after the request
ends, and the beacon leaves at the same instant as without the listen.

It is off by default because an earlier build, with a listen of 3.97 to 7.81 ms that
ended where the train starts, got 43.9% of the CSL jobs answered against 81.7% in a
15 minute bisection on air, with the same host code and plant. Only one job found the
channel busy. The loss is on the 80 ms anchored train: the 1 s train kept its rate
(27/33 against 22/25). The cause is open, see [PROTOCOL.md](../../docs/PROTOCOL.md).
The effect of this listen on air is not measured.

**Gap before the Data frame.** The Data frame starts 12 ms after the end of the last
wake frame. The commercial coordinator leaves 9 to 24 ms, median 12 ms. The last wake
frame announces rendezvous 0, as in the commercial trains, so the field counts down to
the start of that frame and not to the start of the Data frame. The gap is a constant
and is not tuned on air.

**Anchor table.** An anchor has no age limit: 4 missed Enhanced ACKs in a row
drop it, as in the commercial bridge. Eviction picks the least recently
refreshed entry. The samples follow the nominal period of 1 s. The
guard starts at 40 ms on each side of the sample and grows by 30 ppm of the
time since the last refresh, up to half a period: past that the train covers
every instant, as the bootstrap train does. The short train spans about 79 ms
after a fresh refresh. A job targets the first sample that leaves the whole
guard before it. A closer sample waits one more CSL period.

The bridge measures no drift. The commercial bridge divides the error of two
samples at least 60 periods apart by the number of periods, with samples
stamped to 32 us. Here a sample comes from the GDO0 edge of the Enhanced ACK,
and a stamp that is a few ms off, divided by 60 periods and projected over a
few hundred, moves the train off the sample. The device clocks measured in the commercial
captures run within -6 to +18 ppm of the capture host, which the 30 ppm guard
covers without an estimate.

A refresh takes the GDO0 stamp only when it is later than the Data frame. An
earlier edge belongs to another frame, and an anchor placed on it misses by up
to a period: the refresh then takes the instant the ACK is read.

A host simulation compiles these functions from the lines of the sketch. It
drives them with the device clocks and the refresh intervals of the commercial
captures (median 27 s, 13% above 300 s) and peers from -25 to +25 ppm. With
stamps within 0.3 ms, or 30% of them up to 20 ms late, every anchored train
covers the sample, also after 1 h with no refresh, and the worst margin is
never below the one of the fixed 40 ms guard.

**Train length.** The bootstrap train and the Router Advertisement burst cover one
whole CSL period, as the commercial bridge does: the first frame announces 3125
units and the rendezvous counts down to 0 over the period (254 frames over 1.003 s
in the captures, a step of 4.0 ms). The window is 1 s. The frame that ends after
the window and the last wake frame add up to 8 ms, so the train lasts about 1.003
to 1.01 s on air (not measured here). That is over the 1 s of EN 300 220 Ton_max by
a few ms. The decision is to accept it, as the commercial bridge does: a shorter
train leaves a gap in the period, a device that samples in the gap misses the train,
and another 1 s train follows. The guard of an anchored job stops at 500 ms on each
side, so its train never passes the same length. `CMD_TXTRAIN` and `CMD_RESPOND`
stop before a frame that would end more than 980 ms after the first frame, and the
reply gives the frames sent. A train of 48 frames of 140 bytes would last 1.16 s, so
the cap cuts it to 40 frames. The Data frame follows 12 ms after the last wake
frame. It is a second transmission of the same dialogue.

**Enhanced ACK phase.** The CSL phase of the Enhanced ACK comes from the host:
the phase field of `CMD_AUTOACK`, of a `CMD_CSLTX` with the follow flag and of
each CSLQUEUE record. A CSLTX without the follow flag keeps the last phase known
for the coordinator. The commercial bridge sends its timer in units of 320 us,
truncated to 16 bits (`csl_enhack_build_send` at 0x367b8). The bridge does not
copy it. The valve stores this phase as the coordinator anchor
(`csl_nbr_period_update` at 0x2fed4, called from `csl_transmit_frame` at
0x15f94) and times its uplinks on it (`mac_periodic_next_wake` at 0x2fe00). On
air, the build with the free-running phase got 43% of the anchored trains
answered, against 77% with the host phase. That build also listened before the
train, and the listen alone gives the same drop, so the run does not tell
whether the free-running phase is harmful. The host phase stays until a capture
shows the free-running one is harmless.

**Stale anchor after a reboot.** A device that reboots and rejoins samples at a
new CSL phase. Its uplinks carry no CSL IE, so only its next Enhanced ACK shows
the new phase. A job with the bootstrap flag skips an anchor that has missed at
least once and sends the bootstrap train instead. The Enhanced ACK of the train
replaces the anchor. A missed ACK counts against the anchor also after a train,
so an anchor that keeps missing expires after 4 misses. A job without the
bootstrap flag keeps the anchor until then.

**Pair window.** A joining valve sends an Enhanced Beacon Request under the
pairing key and listens for the answer right after it: the commercial bridge
answers 2.4 to 7.4 ms after the request, with no wake train. A round trip
through the host is slower, so the bridge answers itself while the host keeps a
pair window open. The request is `A5 0E 00 20` with the window in seconds
(`durHi durLo`, at most 600), the key (16 bytes), the coordinator EUI (8 bytes,
wire order), the first MAC seq, the first frame counter (4 bytes LE) and the
number of beacons it may send. `A5 0E 00 02 00 00` closes the window. The reply
carries one byte: the beacons sent in the window the request ends. A bad
request gets `0x14`, a firmware before v2.9 answers `0xE0`.

In the window the idle drain and `CMD_AUTOACK` take a 27-byte frame with FCF
`0B E8`, decrypt it under the key, check the MIC, the MAC FCS and the command
`07`, and send the 32-byte Enhanced Beacon (FCF `18 EC`, PAN `ABCD`, the
requester, the coordinator, security control `04`, the frame counter, the MAC
FCS, sealed as `tado/beacon.go` does). The beacon leaves 3968 us after the end
of the request, the listen time of the commercial bridge, and takes the next
seq and frame counter of the block. Only a drain that knows a coordinator
answers, as for the Enhanced ACK. The CSL state machine never answers: a
request that lands in a transaction goes to the inbound ring, and the valve
asks again 8 s later. `CMD_RESET`, a CSLQUEUE FLUSH and the end of the window
close it. Outside a window no path changes.

A pair window also sends each Router Advertisement twice: under the network key
with Frame Pending set (FCF `59 E8`), then under the pairing key. `CMD_RABURST`
takes both frames.

**Shared job pool.** The firmware keeps 128 jobs in one pool and links them into per
neighbour lists. Static RAM is 64460 bytes. The pool holds four full batches at once. An enqueue
that finds the pool short answers `0x19`, the same status as a full neighbour
queue.

**Watchdog.** Both tasks subscribe to `esp_task_wdt` with a 2 s timeout, so a
hang reboots the bridge. The idle task subscription is off, because the radio
task busy-waits on core 1 during a wake burst. A host that stops reading does not
trip the watchdog: the serial task writes in chunks that fit the free space of
the TX buffer.

**Defensive limits.** Every FIFO read clamps to 64 bytes. A request longer than
1024 bytes is answered `0xE1` and dropped. A payload read has an inter-byte
timeout and a total budget. An assembly that stops in the middle of a frame is
thrown away after 60 ms. `Serial.flush()` is gone from the radio path, because
the radio task never writes to the serial port.

## Serial rate

Firmware v2.10 talks 230400 baud. Firmware before v2.10 talked 921600 baud. The host
driver asks for the PING banner at 230400 and then at 921600, and keeps the rate that
answers, so a bridge with old firmware keeps working. The daemon logs the banner and
the rate at start, and warns when the rate is 921600.

The reason is a loss of bytes. A CH340 adapter on a USB 2.0 full-speed bus, shared with
another full-speed device, lost bytes of replies of about 80 to 120 bytes at 921600
baud. The board has no flow control line, and the CH340 datasheet advises flow control
or a lower rate from 500 kbaud in both directions. The point of loss inside the USB
stack is not measured.

The firmware clocks the UART from the 80 MHz APB clock. The ESP32 core clocks a UART at
250000 baud or less from a 1 MHz reference tick, and at 230400 baud that clock gave a
divisor of 4.3: a CH340 adapter garbled 28 of 100 PING replies, and the same rate was
clean from APB. 115200 baud was clean from either clock. The core uses APB above 250000
baud.

A byte takes 10 bit times (start, 8 data, stop). Time on the wire:

| Rate (baud) | Bytes per ms | 110 bytes (a reply with one frame) | 1028 bytes (the largest request or reply) |
| --- | --- | --- | --- |
| 921600 | 92.2 | 1.2 ms | 11.2 ms |
| 460800 | 46.1 | 2.4 ms | 22.3 ms |
| 230400 | 23.0 | 4.8 ms | 44.6 ms |
| 115200 | 11.5 | 9.5 ms | 89.2 ms |

The serial rate sets no radio timing. The Enhanced ACK, the CSL state machine, the wake
burst, the pair window answer and the idle receive run on the radio task, and none of
them waits for the serial port. The Enhanced ACK of `CMD_AUTOACK` leaves before the
reply is built. The rate adds latency only to the exchange with the host. An enqueue
of 1028 bytes arrives 33 ms later than at 921600, and a reply with a frame arrives 4 ms
later. Both are small against the 2 s poll timeout, the 500 ms follow window and the
3 s budget that the firmware gives one request. The radio task meets the serial port
at the output queue, which holds four replies. Only the stream of the developer tools
fills it: a frame of at most 150 bytes leaves every 23 ms at the earliest, which is 6.5
bytes per ms against 23 bytes per ms.

A host that is older than daemon 0.3.1 opens 921600 and does not talk to firmware
v2.10. Update the daemon and the firmware together.

## Build and flash

Install `arduino-cli` and the ESP32 core. CI and the release images use core 3.3.11:

```
arduino-cli core install esp32:esp32@3.3.11 \
  --additional-urls https://espressif.github.io/arduino-esp32/package_esp32_index.json
```

The board is the ESP32 Dev Module, `esp32:esp32:esp32`. It matches the ESP32 DevKit
(WROOM-32, 30-pin) of the wiring below. CI compiles the sketch for this board on every
change. Compile:

```
arduino-cli compile --fqbn esp32:esp32:esp32 firmware/cc1101_bridge
```

A release build sets the version that the PING banner shows, with an unquoted value:

```
arduino-cli compile --fqbn esp32:esp32:esp32 \
  --build-property 'compiler.cpp.extra_flags=-DFIRMWARE_BUILD=0.3.1' \
  firmware/cc1101_bridge
```

Flash, with the port of the bridge:

```
arduino-cli upload --fqbn esp32:esp32:esp32 -p /dev/ttyUSB0 \
  firmware/cc1101_bridge
```

Stop the daemon before you flash. One coordinator unit transmits at a time.

### Flash a released image

Each release has the files `freedo-cc1101-bridge-<version>.bin` (the application),
`freedo-cc1101-bridge-<version>-merged.bin` (bootloader, partition table and
application in one image) and `SHA256SUMS`. Check the image against `SHA256SUMS`, then
write the merged image at offset 0 with esptool 5:

```
esptool --chip esp32 --port /dev/ttyUSB0 write-flash 0x0 \
  freedo-cc1101-bridge-0.3.1-merged.bin
```

Use the application image only at offset `0x10000` of a board that already has the
bootloader and the partition table of the same core.

### Check the bridge

Check the bridge after the flash:

```
freedo-dev ping -p /dev/ttyUSB0
freedo-dev config -p /dev/ttyUSB0
```

`freedo-dev ping` prints `CC1101BRIDGE v2.11 (<build>)`. `freedo-dev config` prints
VERSION `0x04` or `0x14` for a live CC1101.

## Wiring

| CC1101 | ESP32 devkit |
| --- | --- |
| Vcc | 3V3 |
| GND | GND |
| SCLK | GPIO18 |
| MOSI | GPIO23 |
| MISO | GPIO21 |
| CSN | GPIO5 |
| GDO0 | GPIO25 |
| GDO2 | GPIO26 |

GDO2 is an interrupt input. The host register set puts IOCFG2 at `0x00`, the RX FIFO threshold,
and IOCFG0 at `0x07`, the CRC-OK packet flag. After each reset the firmware
sets both registers to `0x2F`, which drives the pin low, until the host loads
its register set. The reset value of IOCFG0 is `0x3F`, a 135 kHz clock that
would fire the GDO0 interrupt 270000 times a second. An ESP32 that reboots with
the USB link up, or a CC1101 that browns out, leaves the chip on these reset
values. The firmware clears the configured flag in both cases, so a command that
needs the radio answers `0x1C` until the host loads its register set again. For a
CC1101 that resets alone, the idle tick sets both pins low again. With GDO2 wired, a frame longer
than 64 bytes leaves the FIFO as soon as the threshold trips. Without it the 8 ms
tick does the same work with more latency.
