# tanoclo vs freedo: firmware verification

Status: complete, last updated 2026-11-07.

Cross-check of the tanoclo project leads and of freedo's own protocol claims
against the tado device firmware, verified in Ghidra. tanoclo is AI-generated:
every claim is a lead, not a fact. This document records what the firmware code
actually does.

## How to resume this work

Firmware reversing needs a live Ghidra MCP server. It is not part of freedo and
must be reopened each session.

- Ghidra MCP project `Tado-IB` at `http://127.0.0.1:8089` (the user opens it).
- Programs (always pass `program` on every MCP call):
  - `/Valve/valve_nrf52_flash.bin`, `/Valve/valve_nrf52_extflash.bin`
  - `/Thermostat/thermostat_nrf52_flash.bin`, `/Thermostat/thermostat_nrf52_extflash.bin`
  - `/Thermostat/thermostat_stm32l051_flash.bin`
  - `/Bridge/tado_stm32f411.bin`
- Useful tools: `decompile_function`, `batch_decompile` (takes addresses inside a
  function), `disassemble_bytes` (`dry_run=true`, `length` as a decimal number),
  `search_instructions` (operand_pattern `#0x...`), `search_byte_patterns` (for
  literal pools, little-endian), `read_memory`, `get_xrefs_to`,
  `get_function_by_address`, `get_function_callees`.
- tanoclo is cloned under the session scratchpad (ephemeral, re-clone from
  `github.com/tanoclo/tanoclo` if gone). Treat it as untrusted: MIT license, so
  reimplement, never copy. Key files: `docs/coap_tlv.md`, `docs/fine_tuning.md`,
  `docs/coap_debug_endpoints.md`.
- freedo is at `/home/cappyt/Documenti/Repos/freedo`. The freedo fixes from this work are applied (see "freedo fixes").

### Verification constraints (hard rules)

- Ghidra function names and comments are NOT trustworthy: they were written by an
  agent. Verify meaning only from code: constants, literal pools, data flow,
  arithmetic, callers. The same holds for function names in freedo docs: only the
  addresses count.
- Debug resources are analysed statically only. Do not exercise their radio,
  network-key or factory-reset write paths on a plant.

The durable conclusions are also in the hindsight memory bank `cappyt`
(contexts "freedo tado protocol: ...").

## Boiler tags (thermostat STM32L051, `thermostat_stm32l051_flash.bin`)

OT transaction `0x0800bfdc`: request type/id/value at ctx+0xc8/0xc9/0xca, response
type/id/value at ctx+0xcc/0xcd/0xce. Response id must equal request id. Type 0 READ
succeeds on 4 (READ-ACK), type 1 WRITE on 5 (WRITE-ACK), 6/7 = DATA-INVALID /
UNKNOWN-ID -> cfg_attr_reset_default. So r1 = OT data-id. f8.8 -> centi `0x0800beb4`:
`(x>>8)*100 + ((x&0xff)*100>>8) + 1` (the +1 is real). Setter `0x08000ca8(tag,value)`.
Tags read from literal pools, not names.

| tag | source (address) | value | freedo | tanoclo |
|---|---|---|---|---|
| 0450 | OT1 WRITE TSet 0x0800c564, ctx+0x194 = min(b[0x160], b[0x159]) | int C x100 | control_setpoint_C OK | OK |
| 0452 | OT17 HB 0x0800b554 (pool b828) | int % | boiler_mod OK | OK |
| 0457 | OT0 slave LB bit3 0x0800b4b4 (pool b820) | flame 0/1 | flame_on OK | boiler_active OK |
| 0458 | OT36 0x0800b9cc, centi/10 | flame current 0.1 uA | flame_current_uA (u16, scale 10) OK | "fault flags" WRONG |
| 044c/044d/044e/044f/045a | OT25/28/33/27/26 0x0800c230 pool c47c-c490 | centi C, range -40..127 | OK | OK |
| 045e | OT19 0x0800ba34 (pool bae0), centi/10, u8 table | 0.1 l/min | OK size 1 | OK value, says u16 |
| 0462 | OT18 0x0800b89a (pool bac8), centi/10, u8 table | 0.1 bar | OK | OK value, says s16 |
| 0463 | OT116 raw (pool bacc) | count | OK | OK |
| 0464 | OT119 raw (pool bad0) | DHW burner starts | OK | "CH pump starts" WRONG |
| 0465 | ctx+0x186 - ctx+0x18c = OT116 - OT119 (0x0800adbc) | derived CH starts | OK | OK |
| 0466 | OT120 x60 (0x0800c1b0, pool c22c) | minutes | OK | OK |
| 0467 | (OT120 - OT123) x60 (0x0800ad98, pool b0c0) | minutes | OK | OK |
| 0468 | OT123 x60 (0x0800c1b0 imm 0x468) | minutes | OK | OK |
| 046c | OT3 HB bit3 0x0800b26e | DHW storage tank 0/1 | dhw_storage_tank OK | "DHW supported" WRONG |
| 046d | !OT3 HB bit3 0x0800b27c | instantaneous DHW | dhw_instantaneous OK | "DHW not supported" WRONG |
| 046f | OT57 0x0800c230 pool c498, <=127.00 | MaxTSet centi | max_ch_setpoint_C OK | "DHW setpoint OT57" WRONG (OT57 = MaxTSet) |
| 0471 | OT48 HB x100 0x0800b34a (pool b424), invalid -> 65 | TdhwSet upper bound | dhw_setpoint_max_C OK | dhw_setpoint_max OK |
| 0478 | OT5 LB (OEM fault) <<16 or 0xffff, only if OT0 LB bit0 fault; else 0xffffffff (0x0800b840) | packed | decoded into Fault / FaultCodes | - |
| 0479 | 0xffff0000 or OT115 (0x0800b9f2, pool badc/bad8) | packed | decoded into DiagnosticCode | - |
| 0460 | nRF52 thermostat 0x26ab8: each attr 0478..047f encoded under tag 0460; skips the width sentinels, and 0xffffffff in resource 0x12 | packed u32 | decoded into Fault, FaultCodes, DiagnosticCode | "water pressure mbar" WRONG |

OT49 (MaxTSet bounds) is read (ctx+0x13b/0x13c, clamps 20..30 / <=100, default
30/70) but no attr is set. No tag carries OT49.

Tag `2040` (circuit flow limit): the STM32 writes it to the boiler as OT57 MaxTSet.
There is no OT49 clamp on this path (ctx+0x178 -> ctx+0x159 -> 0x0800b5d8).
`0471` is the OT48 DHW upper bound, not a CH cap.

Tag `0460` (hvac/codes):
- `0478` = (OT5 low byte << 16) | 0xffff.
- `0xff00ffff` = fault with unknown code.
- `0xffffffff` = no fault (skipped).
- `0479` = 0xffff0000 | OT115 diagnostic code. `0xffff0000` is diagnostic code 0, not a fault.

The nRF52 encoder at 0x26ab8 sends 4-byte TLVs.
- It skips 0xffffffff for 0x478..0x47f in resource 0x12.
- It skips the width sentinels everywhere.
- 0x7ffffffe goes into the `0481` unsupported aggregate.
- TLV order: `015d`, then `0478..`, then `0481`.

The nRF52 `2040` handler has no clamp (0x1cde4). DeepSeek and Kimi agree.
The STM32 reads `2040` as s16, divides it by 100 and keeps the quotient byte at ctx+0x178
(0x0800cb14): whole degrees, the fraction is lost. A byte of 0x7f means unset: the OT57 write
is skipped and ctx+0x159 = OT49 max (0x0800b5b2, 0x0800b606), so the control setpoint is capped
at the OT49 maximum (70 C default). freedo now refuses a fractional flow limit.

## Valve tags (`valve_nrf52_flash.bin`)

### 0273 / 027c (ADC voltages)
`0x1d684`: calls `0xf840` (sets P0.17 via `0x13874`, busy delay 0x2710, samples ADC
ch0 with P0.23 high via `0x13068` = clamp(adc,0,4096)*1875/1000, ADC ch7 via
`0x130c2` = adc*586/1000, then clears P0.17), reads u16 at `0x20000240` (ch0),
attr_set(0x273, v), then `0x1d618(v)`. `0x1d618`: v > 0x865 (2149) -> counter 0;
else counter++; at 2 -> attr 0x28f = 1. Report path `0xf0b0..0xf110`: 0x27c <- u16
`0x2000031c` then reset to 0xffff; `0x2000031c` is the running minimum of
(sum/count)*1875/1000 during a motor run (same scale as 0273).
Verdict: 0273 is an ADC voltage (mV x1.875); freedo right, tanoclo "step limit"
WRONG. 027c is a voltage too (min of the averaged reading during a run); freedo
names `motor_load_mV` (0273) and `motor_run_min_mV` (027c) are right. tanoclo "open
limit steps" WRONG.

### 4160 (zone heat-percent divisor)
Zone attr handler `0x2f610`: if byte[0x20008790]==0 and tag==0x4160 ->
byte[0x20008792] = min(v,100), default 100. Scale `0x2f4a4`: out =
min(in*100/byte[0x20008792], 100); passthrough if byte[0x20008790]!=0 or divisor==0.
Caller `0x2f4dc`: in = zone 40a0, scaled only if d/config 0298 != 0. Result ->
published as 4080|c, and 4140|c when byte[0x20008790]!=0. No lower bound 25 in code.
4160 is a zone attr (4160..417f), not d/config.
Verdict: tanoclo arithmetic right, meaning wrong (it scales the circuit-demand
report 4080 in the zone->circuit translator role, not the motor stroke). freedo
correct but does not document the valve-side divisor use. Not worth importing as a
"sensitivity" knob.

### 01a3 (fault word)
`0xa340`: 0fa3 = (byte[0x2000922f] << 3) & 0xf8. `0x10910`: on 0fa3 change, `0x108f8`
puts it in byte 1 of the 01a3 word -> code = (w>>11)&0x1f. Code 13 path: `0x1d618`
sets attr 028f=1 after two 0273 <= 2149 checks; observer `0xa7b8` then sets state
byte to 13 -> 01a3 = 0x6800. freedo OK.

### 0158 (display notify flags)
`0x2507c`: byte[0x200077db] = 1 if bit9, 2 if bit10 (bit10 wins), 0x7fff -> 0.
`0x26012`: 6440 change with value bit0 sets bit2. `0x25844`: byte <= 1 -> adopt new
zone values silently, else on a change fire UI event 0x280. So bit9 = notify on
remote-overlay (6440 bit0) change, bit10 = notify on any zone-value change. freedo
OK, tanoclo "Dazzle/always-on" WRONG.

### 02b2 (UTC offset)
d/config cb `0x1fb30`: v = s16, rt_time_epoch_set(v*60000). Getter `0xbbe4` read only
by the schedule evaluator `0x216d2`: local_s = epoch_ms/1000 + offset_ms/1000.
Verdict: UTC offset in minutes. freedo OK, tanoclo "display timeout" WRONG.

### 015d (interface module)
`0x1002c`: 0 -> 0x70, 0x7fff -> teardown, else res_table_find_by_code `0xa2e0` over
[0x3a43c,0x3a460): one entry, code array `0x3773c` = {0x0070}. freedo OK.

### 015e (membership roles)
d/config cb `0x1faee`: per record i: 6300|i = byte1 (zone), 6340|i = byte0 (role).
Role bits (6020|i: 1 heating, 2 DHW): bit3 + type1 -> elected member (observe target /
membership index, 0x2a5a8, 0x2b5b0); (role&0xc)==4 -> plain member subscribing to
reports (0x30bd0); bit1 -> controller/report roles (0x11320, 0x24e70, 0x2f360);
bit0 -> marker flag (0x24fc0).
Thermostat role bits (freedo):
- bit3 = acts on the zone (`/z/N/s` observe, `/overlay`, `/ow`, `/sched`).
- bit1 without bit3 = fallback member.
- (role & 0xc) == 4 = subscribes to reports.
- bit0 = the device's local zone.
No role makes the thermostat a sensor only (DeepSeek agent; I checked 0x31d78). It copies
`012d` into `4060|<zone>` only for the zone it controls (0x31d78). 0x2a380 picks that zone:
a stored index other than 0x7f wins, else the scan keeps the last zone with type 1
(`6020|zone`) and bit3 in `6340|zone` (DeepSeek said "first": wrong). The report builder
0x202a0 carries `012d`/`012e` (also 0160-0162, 01c8, 0135-0137) and reads no role bit. I
traced it myself: the URI is `/d/` (0x5b05c) + serial + table entry 4 (0x5b088 -> `/sen`,
built at 0x2b734). Its only call site 0xec96 sits in a chain of report producers
(0xeb82-0xecc8) inside the dispatcher 0xe6dc. The chain reads the zone count (`017d`) and
loops the per-zone producers, but no role bit. The entry state of the chain is not traced.
tanoclo role `0x09` has bit3, so it acts on the zone: it is not a "wireless sensor" role.
Verdict: tanoclo role bytes 0x02/0x03/0x09/0x0b fit this bit layout; their names
(RU, wireless sensor) are not provable from the valve.

### Climate report block (build `0x21a60`, observer `0x21c74`, cache 0x20006d08)
Observer registered at `0x21e60` for 11 attrs (list `0x3831e`): 01c8 012d 012e 0136
0135 0162 0161 0160 0169 0137 0291. Report emits each attr whose cache != sentinel
(0x7fff / 0x7f). 0137 cache reset to 0x7f after send; 027a is read live at report
time from `0x2d1ac`. Dirty thresholds: 012d/012e > byte attr 0291, 0135 > 50,
0136 > 10, 01c8 > 30, 0162 > 500, 0160 any change, 0137 always.

### 0160 (reset reason)
Valve `0x2db50` reads POWER->RESETREAS (0x40000400), table `0x38d4c` {mask,code}:
RESETPIN->3, DOG->0, SREQ->2, LOCKUP/OFF/LPCOMP/DIF/NFC->5, none->1 (POR); only
caller is the WDT crash dump `0x1d7f0`. No code sets attr 0160 by constant on the
valve or the thermostat nRF52, so it stays at sentinel 0x7f and is not reported. The
IB (`0x25334`) reads RCC_CSR into a bitmask (bit0 PIN, bit1 POR, bit2 LPWR, bit3
WWDG, bit4 SFT, bit5 IWDG) but sets its own attr 0160 to constant 2 at init
(`0x2ea1c`).
Verdict: tanoclo's bit order (2 SW, 3 IWDG, 4 WWDG, 5 LPWR) is WRONG even for the IB
word, and that word is not what 0160 carries. Do not import.

### 0136 (ambient light)
`0x2d628`: device "AMBIENT_LIGHT_0", channel 17, u8. Driver `0x12fa8`: SAADC ch1, 16
samples, x*100/3550, cap 100. (The Ghidra comment on 0x12fa8 says humidity: wrong.)
Verdict: 0..100 %. tanoclo TRUE. freedo name `ambient_light_pct`.

### 01c8 (aux temperature)
`0x2d628`: device "SENSOR_TEMP_1", channel 13, centi C, no user offset. Driver
`0x1316c`: excitation pin, SAADC ch2, 16 samples, 10k divider + NTC lookup `0x29ae8`.
Verdict: NTC thermistor, centi C. freedo temp_aux_C OK. tanoclo "stem NTC" fits;
physical location not provable.

### 0137 / 027a (dial)
`0x2d088`: GPIO_0 pins 25 and 26, handler `0x2d224` = quadrature decoder (2-bit state
from 0x2d1ac: bit0 = P0.25 low, bit1 = P0.26 low; debounce > 5; table `0x38cd3`;
signed acc, at |acc| >= 4 and state 0 calls cb with -1 / +1). cb `0x254ac` increments
unsigned counter byte 0x20007862. `0x25038` (activity end) reports 0137 = counter,
then 0. 027a = live pin state at report time.
Verdict: 0137 = detent clicks in the last UI session (unsigned count); tanoclo
"127 +/- delta" WRONG. 027a = raw encoder pin state (0 at a detent); tanoclo
"3 = manual override committed" WRONG. freedo names `dial_clicks` (0137) and `dial_pins` (027a).

### 019d / 019e
Reserved ids with no use. Both sit in the u16 persistent-record id whitelist (valve
0x37818/0x3781a, thermostat nRF52 0x5a100, IB 0x57644). The only readers of that list are
id-validity gates (valve 0xcb46, thermostat 0xd546, IB 0x201d4). No setter, getter or
d/config handler uses either id. The thermostat STM32 has no occurrence. The two
immediate constants are log codes (thermostat nRF52 0x20436 assert 0x19e, IB 0x4c53e fault
log 0x19d). Source: GLM agent; I checked the valve table bytes.
Verdict: tanoclo "display contrast/brightness" is unsupported (the valve and the IB have no
display). Do not import.

### d/config ETag
Request `0x1fd08`: ETag option = cfg blob 0x15a (8 B) when present. Response handler
`0x1fc60`: on block 0 of a 2.xx (not 2.03) copies CoAP option 4 (`0x24542`) and
stores it as attr 0x15a. 2.03 only re-arms; 4.xx/5.xx invalidate.
Verdict: the device echoes the server ETag, computes nothing. freedo FNV-64a (
`tado/emu.go:configEtag`) is a valid local choice; tanoclo MD5 slice is its own
server choice. Nothing to import on either side.

### Debug endpoints
The valve registers CoAP receivers `d/dbg/st`, `d/dbg2/tlvs`, `d/dbg/m` (strings at
`0x38110`). The diag/stats table is a 43-entry u16 attr-key array
(`coap_report_stats_table_init` `0x20480`, keys at `0x380be`: 01ac, 62e0..62ff, 0289,
03ed, 024b, 01c6, 01fa, 0fa3, 028b, 0294, 016d, 0290) matching tanoclo's d/dbg/st
table.

`d/dbg/m` has a memory-read GET and a POST dispatcher with 12 functions. Several
POST functions write radio registers or change device state. `d/dbg2/tlvs` has no
endpoint-level tag whitelist. Its 0x13 limit applies to each value, not to the
record count. Its 2.04 response does not prove that a backend applied a write.
The complete map, including tags `0003` and `016c`, is in
[Debug resources](PROTOCOL.md#debug-resources).

## Stale downlink drop

tanoclo drops zone writes that are older than 2 hours. freedo does not need this.
`supersedeZoneWrites` in `tado/emu.go` keeps only the newest zone state.

## freedo fixes

Applied:

1. `0471` is the OT48 DHW upper bound. The cap is removed from the flow bounds:
   `FlowBounds` is gone and the limits are 30..75 C.
2. `0460` is decoded into `Fault` (bool), `FaultCodes` (known codes, no duplicates)
   and `DiagnosticCode`. `FaultCode` is removed (API break).
3. Renames: `0457` flame_on, `0458` flame_current_uA (scale 10), `046c`
   dhw_storage_tank, `046d` dhw_instantaneous, `0471` dhw_setpoint_max_C, `0136`
   ambient_light_pct, `0273` motor_load_mV, `027c` motor_run_min_mV, `0137`
   dial_clicks, `027a` dial_pins.
4. `docs/PROTOCOL.md` has the 015e roles, the 4160 valve divisor, a Boiler link tags
   table and an hvac/codes paragraph.
5. `docs/PROTOCOL.md` documents the debug resources and their write paths.
6. `BuildDbg2TLVWriteCoAP` writes only `0140`, `0149` and `0158` (`dbg2Tags`).
   `/api/nodes/{id}/dbg/mem` reads only inside mapped regions
   (`debugMemoryReadable`), and the generic resource route refuses `d/dbg/m`.

Nothing else from tanoclo is worth importing: every other differing lead is wrong
where it conflicts with freedo. The leads that the firmware does not prove are in
"Open items".

## Open items

None.
