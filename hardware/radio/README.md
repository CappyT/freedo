# freedo radio rev A

This PCB design is almost completely AI-generated. I have no experience designing a PCB, this work is not tested and may be completely wrong. If you are willing to help, a pull request is accepted.

Documented checks: an electrical review (findings E01 to E20, outcome in section 8.3) and a silkscreen DRC result (section 6.4). No ERC result is documented.

## State of the files

The repository shows no built board, and every bring-up check in section 8.2 is open. `tools/make_fab.py` generates the fabrication package (gerbers, BOM, placement file) on demand. The package is not committed. It comes from an untested design, so have it reviewed before you order it.

## Licence and provenance

The design files in this directory are under the same licence as the rest of the project (Apache License 2.0, see the `LICENSE` file at the root). The footprints and symbols in `lib/` derive from EasyEDA and LCSC libraries and from Espressif libraries. This is provenance only. The terms of those sources apply to those parts.

## 1. Purpose and scope

Rev A replaces the ESP32 devkit and the CC1101 module with one board.
The board carries an ESP32-S3-MINI-1-N8, an Ebyte E07-900MM10S CC1101 module, a W5500 with a
discrete transformer, an optional isolated PoE module and USB-C.
The host talks to the board over USB (native USB-Serial-JTAG) or, after a firmware port, over Ethernet.
JLCPCB makes the 4-layer PCB and assembles all SMD parts on the top side.
The user hand-solders the RJ45 jack, the DP9900LP-5V module, the edge-mount SMA and, when needed, the J203 header.

Conventions in this file:

- "(unverified)" marks a value that no document in hand confirms.
- Class: B = JLC Basic, P = JLC Preferred (Extended without the loading fee), E = Extended.
- Fit: yes = fitted on every board, opt = hand-soldered only on PoE boards, DNP = footprint only.
- Class and stock come from a JLC parts database snapshot of 2026-09-24. Check them again at BOM upload.
- Designators: the RF block uses U1, J1, L1, C1, C2 and 1xx. MCU/USB/power uses 2xx, Ethernet 3xx, PoE 4xx.

## 2. Block diagram

```
 LINE SIDE (PoE)                     ||   SYSTEM SIDE
                                     ||
 J301 RJ45 -- pairs 1/2, 3/6 --[ T301 B1608S ]-- MDI -- U301 W5500 -- SPI GPIO 9-14 ----------------+
   |             | line CTs 15, 10   ||                   | Y301 25 MHz                             |
   | 4/5, 7/8    |                   ||                                                             |
 BR401 MB10S   BR402 MB10S           ||                                                             |
   +------+------+                   ||                                                             |
          | VIN+/VIN-, D401 SMAJ58A  ||                                                             |
     [ U401 DP9900LP-5V, isolated  ]-- +VDC 5.66 V -- D402 SS34 --------------+                     |
 Bob Smith: 4 x (75R + 10 nF)        ||                                       |                     |
 -> C322 -> SHIELD -> C323 -> GND    ||  J201 USB-C -- F201 -- D201 -- D202 --+ 5V_SYS              |
                                     ||       | D+/D- via U202 (no VBUS pin)  |                     |
                                              | VBUS -> R216/R217 -> GPIO17   U203 TLV76133DCYR     |
                                              |                               | 3V3                 |
                                              +-- USB GPIO 19/20 ------> U201 ESP32-S3-MINI-1-N8 <-+
                                                                   J203 ----> | SPI GPIO 36-42
                          +---------------------------------------------------+
   3V3 -- L1 -- 3V3_RF -- U1 E07-900MM10S -- ANT -- R191 0R -- L191 shunt -- J1 SMA (edge)
```

PoE option: only U401 is optional. JLC fits the SMD PoE parts on every board, because one order has one BOM (section 5.4).

## 3. Pin map

Module pads from the ESP32-S3-MINI-1 datasheet v1.7, Table 3-1 (pdf p.11-12).

| Signal | GPIO | Pad | Dir | Circuit |
| --- | --- | --- | --- | --- |
| W5500 RSTn | 9 | 13 | out | R313 10k pull-up to 3V3 |
| W5500 INTn | 10 | 14 | in | R314 10k pull-up to 3V3 |
| W5500 MOSI | 11 | 15 | out | direct |
| W5500 MISO | 12 | 16 | in | direct |
| W5500 SCLK | 13 | 17 | out | R208 33R at the module |
| W5500 SCSn | 14 | 18 | out | W5500 internal pull-up (50-112k) |
| Radio GDO2 (E22 DIO1) | 36 | 32 | in | direct, TP106 |
| Radio GDO0 (E22 DIO3) | 37 | 33 | in | direct, TP105 |
| Radio SCLK | 39 | 35 | out | R209 33R at the module, TP101 |
| Radio CSn (E07 CSN, E22 NSS) | 40 | 36 | out | R210 10k pull-up to 3V3_RF, TP104 |
| Radio SI (MOSI) | 41 | 37 | out | direct, TP102 |
| Radio SO (E07 MISO/GDO1, E22 MISO) | 42 | 38 | in | direct, TP103 |
| E22 NRST (option) | 21 | 25 | out | R101 0R DNP to U1 pad 3 |
| E22 RXEN (option) | 47 | 27 | out | R102 0R DNP to U1 pad 10 |
| E22 BUSY (option) | 48 | 30 | in | R103 0R DNP to U1 pad 11 |
| VBUS_SENSE | 17 | 21 | in | R216 68k from VBUS, R217 100k and C209 100 nF to GND |
| I2C SDA | 15 | 19 | io | J203-2, R219 4.7k to 3V3 DNP |
| I2C SCL | 16 | 20 | io | J203-3, R220 4.7k to 3V3 DNP |
| Header IO1 (ADC1_CH0) | 1 | 5 | io | J203-4 |
| Header IO2 (ADC1_CH1) | 2 | 6 | io | J203-5 |
| Header IO7 (ADC1_CH6) | 7 | 11 | io | J203-6 |
| Header IO8 (ADC1_CH7) | 8 | 12 | io | J203-7 |
| LED TX | 4 | 8 | out | R212 1k, D204 to GND |
| LED RX | 5 | 9 | out | R213 1k, D205 to GND |
| LED STATUS | 6 | 10 | out | R214 1k, D206 to GND |
| USB D- | 19 | 23 | io | R203 22R, U202, J201 A7/B7 |
| USB D+ | 20 | 24 | io | R204 22R, U202, J201 A6/B6 |
| UART0 TX (U0TXD) | 43 | 39 | out | R207 1k at the module, TP211, J203-8 |
| UART0 RX (U0RXD) | 44 | 40 | in | TP212, R218 1k to J203-9 |
| BOOT | 0 | 4 | in | R206 10k pull-up, SW202 to GND |
| EN (CHIP_PU) | - | 45 | in | R205 10k pull-up, C201 1 uF, SW201 to GND |
| 3V3 | - | 3 | pwr | C206 22 uF + C207 100 nF at the pad |
| GND | - | 1, 2, 42, 43, 46-65 | pwr | 46-60 is the antenna-end row, 61 the EPAD, 62-65 the corner pads |
| Not connected | 3, 45, 46, 26 | 7, 41, 44, 26 | - | Straps (3, 45, 46) and PSRAM CS (26) |
| Not connected | 33-35 | 28, 29, 31 | - | Octal flash lines, free on the N8 |
| Spare, not connected | 18, 38 | 22, 34 | - | GPIO38 is driven low in USB-OTG download (see below) |

GPIO allocation rules for the new signals:

- No strapping pin (GPIO0, 3, 45, 46), no USB pin (19, 20), no used pin.
- No flash or PSRAM line: GPIO26-32 are priority 4 (S3 DS v2.2 2.3.5, pdf p.26). GPIO33-37 are priority 3: they
  are the octal lines DQ4-DQ7 and DQS (S3 DS Table 2-14, pdf p.31). The N8 has quad flash (MINI-1 DS Table 1-1), so
  GPIO36 and GPIO37 carry GDO2 and GDO0.
- The free priority-2 pins with a MINI-1 pad are GPIO1, 2, 7, 8, 15-18, 21, 47, 48 (S3 DS 2.3.5 Table 2-9,
  MINI-1 DS Table 3-1). Ten of the eleven are used. GPIO18 stays spare.
- The four header GPIOs are ADC1 pins: GPIO1, 2, 7, 8 = ADC1_CH0, CH1, CH6, CH7 (MINI-1 DS Table 3-1).
- I2C uses GPIO15/16. They show the 60 us low power-up glitch too (HDG 1.3.10 Table 10, XTAL_32K_P/N): a device on
  J203 can see a false START. Send a bus recovery (9 clocks, then STOP) before the first transfer.
- VBUS_SENSE uses GPIO17. Its 60 us power-up low glitch drives into 40 kohm and is harmless.
- The E22 lines use GPIO21, 47 and 48, the free pads nearest to the radio pads 32-38.
- Both SPI buses are planar. The GPIO order follows the pad order of U201, U301 and U1 (section 6.4), so every
  line routes on F.Cu without a crossing. W5500: pads 13-18 against U301 pins 37-32. Radio: pads 32-38 against
  U1 pads 20, 18, 15, 14, 13, 12. Pad 34 (GPIO38) stays spare: its USB-OTG download level (low) would fight a
  radio output.

Boot states of the bus pins. At reset, no pin in the table has a pull or an output (S3 DS Table 2-1, pdf p.16-17).
After reset, each is an input (IE). GPIO39 also gets the weak pull-up while pad JTAG is not disabled (note 7).

| GPIO | Signal | ESP32 side | Power-up glitch | SPI boot | USB-OTG download |
| --- | --- | --- | --- | --- | --- |
| 9 | W5500 RSTn | out | 60 us low | input | input |
| 10 | W5500 INTn | in | 60 us low | input | input |
| 11 | W5500 MOSI | out | 60 us low | input | input |
| 12 | W5500 MISO | in | 60 us low | input | input |
| 13 | W5500 SCLK | out | 60 us low | input | input |
| 14 | W5500 SCSn | out | 60 us low | input | input |
| 36 | radio GDO2 | in | none | input | input |
| 37 | radio GDO0 | in | none | input | input |
| 38 | spare | - | none | input | output low (VMO) |
| 39 | radio SCLK | out | none | input | output low (VPO) |
| 40 | radio CSn | out | none | input | output high (OEN) |
| 41 | radio MOSI | out | none | input | input (VM) |
| 42 | radio MISO | in | none | input | input (VP) |

- Glitch column: HDG 1.3.10 Table 10 (pdf p.20) and S3 DS Table 2-2 (pdf p.18): a 60 us low-level output.
- SPI boot reads the quad flash on the dedicated SPI pins. GPIO33-37 serve only octal flash (S3 DS Table 2-14).
- Joint download boot (GPIO0 low at reset, or forced by esptool over USB) includes USB-OTG download, which is enabled
  by default (TRM v1.8 Table 8.2-1, pdf p.535). The ROM sets the external-PHY pads: GPIO38 low, GPIO39 low, GPIO40
  high, GPIO41, GPIO42 and GPIO21 inputs (HDG 1.3.13 Table 12, pdf p.22). The HDG does not say whether SPI boot sets
  them too. The assignment is safe in both cases. UART download uses GPIO43/44. USB-Serial-JTAG download uses
  GPIO19/20. SPI download boot needs GPIO46 high (TRM Table 8.2-1). GPIO46 is open with its internal pull-down, so
  this board never enters it.
- No radio output lands on a driven pin. GDO0 and GDO2 (outputs from power-up: CLK_XOSC/192 and CHP_RDYn, SWRS061I
  p.71) sit on GPIO37 and GPIO36. SO (3-state while CSn is high, SWRS061I p.61) sits on GPIO42. CSn on GPIO40 is
  high in every mode, so the radio stays deselected while the ROM drives SCLK low.
- The W5500 outputs sit on GPIO10 (INTn) and GPIO12 (MISO). No boot mode drives GPIO9-14. The power-up glitch drives
  them low for 60 us. Every pad next to the W5500 corner has it (GPIO1-18). MISO is high impedance while SCSn is
  high (W5500 DS Table 2, p.10). The glitch holds RSTn (GPIO9) low at the same time. The W5500 DS gives no INTn
  state during reset, so a 60 us contention on INTn is not excluded (unverified).

Pin notes:

- GPIO 9-14 are the SPI2 (FSPI) IO_MUX pins: FSPIHD 9, FSPICS0 10, FSPID 11, FSPICLK 12, FSPIQ 13,
  FSPIWP 14 (MINI-1 DS Table 3-1). The planar order puts MISO on 12, SCLK on 13 and SCSn on 14, so the W5500 bus
  goes through the GPIO matrix (section 7).
- GPIO 33-37 are in the VDD_SPI domain. GPIO45 stays low, so VDD_SPI is 3.3 V (MINI-1 DS Table 4-4).
  Never pull GPIO45 up.
- GPIO47 and GPIO48 are also in the VDD_SPI domain. They run at 1.8 V only on the R8V/R16V chips (S3 DS v2.2
  Table 2-1 note 4, pdf p.18). On the N8 they run at 3.3 V.
- The default function of pads 32 and 33 (GPIO36, GPIO37) depends on an eFuse (MINI-1 DS Table 3-1, note a).
  On the N8 the flash is quad, so both are free (S3 DS v2.2, 2.3.5). The firmware sets them as GPIO inputs.
- GPIO 1-18 give a 60 us low glitch at power-up (HDG 1.3.10 Table 10; GPIO15/16 as XTAL_32K_P/N). On GPIO9 it
  pulses W5500 RSTn. The firmware resets the W5500 after boot, so the glitch is harmless. The header pins GPIO1, 2, 7, 8
  show the same glitch.
- USB-OTG download mode drives GPIO38 low, GPIO39 low and GPIO40 high (HDG 1.3.13 Table 12). GPIO38 is spare,
  GPIO39 and GPIO40 are radio inputs (SCLK, CSn). CSn high keeps the radio deselected. GPIO21 becomes an input.

## 4. Power

### 4.1 Power tree

```
J201 VBUS -- F201 0.75 A PTC --+-- D201 SMF6.0A -- GND
                               +-- C208 100 nF, R215 4.7k bleeder -- GND
                               +-- R216 68k -- VBUS_SENSE (GPIO17) -- R217 100k || C209 100 nF -- GND
                               +-- D202 SS34 ---------------+
U401 +VDC 5.66 V -- C401 100 uF, C402 10 uF -- D402 SS34 ---+-- 5V_SYS: C202 10 uF, C203 100 nF, TP "5V_SYS"
                                                            |
                                                   U203 TLV76133DCYR (SOT-223)
                                                            |
                                    3V3: C204 10 uF, C205 100 nF, TP "3V3"
                                     +-- U201 pad 3: C206 22 uF, C207 100 nF
                                     +-- U301 VDD (C312, C313); FB301 -> 3V3A (C305-C311)
                                     +-- L1 BLM15HG102SN1D -> 3V3_RF (C1 10 uF, C2 100 nF) -> U1 E07-900MM10S
                                     +-- 6 LEDs, J203 pin 1
```

U202 (H5VUT2U) protects D+/D- only. It has no VBUS pin.

### 4.2 Current budget (3V3 = 5V_SYS current, linear LDO)

| Load | Typical | Peak | Source |
| --- | --- | --- | --- |
| ESP32-S3, 240 MHz dual core, Wi-Fi off | 100 mA | 108 mA | MINI-1 DS Table 6-6 |
| ESP32-S3, Wi-Fi TX burst 802.11b 20.5 dBm (replaces the row above) | - | 355 mA | MINI-1 DS Table 6-4 |
| W5500, 100M link / 100M TX | 128 mA | 132 mA | W5500 DS 5.3, 5.4 |
| E07-900MM10S TX (RX 18 mA) | 32 mA | 32 mA | E07 DS 2.2 |
| 6 LEDs at ~1.3 mA | 8 mA | 8 mA | calculated, (3.3 - 2.0) V / 1k (unverified Vf) |
| LDO quiescent | 0.07 mA | 0.1 mA | TLV761 DS 5.5 |
| Total | ~270 mA | ~530 mA | |

- The design point is 0.35 A continuous. The 0.53 A peak is a Wi-Fi TX burst.
- The ESP32-S3 supply must deliver at least 0.5 A (MINI-1 DS Table 6-2, HDG 1.3.2).
- The E22-900MM22S option draws 100 mA in TX at 22 dBm (E22 DS 2.2), 68 mA more than the E07.
- PoE input power: 5.66 V x 0.27 A / 0.83 = 1.8 W typical. The DP9900LP-5V gives 7 W at 70 C (DP9900 DS Table 1).
- F201 holds 0.75 A, above the 0.53 A peak.

### 4.3 5 V ORing

D202 (USB) and D402 (PoE) form a diode-OR into 5V_SYS. The higher source carries the load.

| Sources | Carries the load | 5V_SYS | Reason |
| --- | --- | --- | --- |
| USB only | USB through D202 | 5.0 - 0.1 (PTC) - 0.4 = 4.5 V | D402 blocks back-feed into the DP9900 output |
| PoE only | DP9900 through D402 | 5.66 - 0.4 = 5.26 V | D202 blocks back-feed into VBUS (leakage only) |
| USB and PoE | DP9900 | 5.26 V | 5.66 V is 0.41 V above the 5.25 V VBUS maximum (unverified) |

- The DP9900-5V needs at least 100 mA output load. Below it the module can make audible noise and the
  PSE can drop power (DP9900 DS 3.3, note 2).
- R401 = 0 ohm from ADJ (pin 4) to -VDC (pin 3) sets the output to 5.66 V (DP9900 DS Table 3).
  The next table value, 100k, gives 5.27 V. That does not reliably beat a 5.25 V VBUS (USB 2.0 range
  4.40-5.25 V, unverified). Table 3 has no value between 0 and 100k.
- With PoE present the load is 0.25-0.53 A, so the DP9900 stays above 100 mA. This holds while the
  W5500 is not in power-down and the ESP32 is not in deep sleep. Two corners go below it (review E11, accepted in
  section 8.3).
- The DP9900 datasheet gives no tolerance for the adjusted output. If the +-5% of the 5 V setting also
  applies (unverified), the low corner is 5.38 V. USB then takes a small share of the load and the DP9900
  still carries most of it.
- SS34 forward drop: ~0.4 V at 0.3-0.5 A (MDD SS34 DS Fig 3, typical curve).
- No TVS sits on the DP9900 output: the DP9900 output is isolated and regulated.

### 4.4 LDO: TLV76133DCYR

- TI TLV76133DCYR, LCSC C7527500, Extended, stock 11,418, $0.22. SOT-223 (DCY): pin 1 GND, pin 2 and tab OUT,
  pin 3 IN (TLV761 DS SBVS349D Table 4-1). The land is the JEDEC TO-261AA SOT-223 land.
- Input 2.5-18 V operating, 20 V absolute; 1 A; output accuracy +-1.75 % over line, load and temperature
  (3.24-3.36 V); UVLO 2.4 V max (DS 5.1, 5.3, 5.5).
- Internal soft-start, 500 us typical (DS 1, Fig 7-8).
- Output capacitor 1-220 uF, 0.47 uF effective minimum, ESR 2-500 mohm; input >= 0.47 uF effective (DS 5.3).
- Dropout 1.6 V max at 1 A. It scales with the pass transistor resistance, VDO / 1 A (DS 6.3.1, Fig 5-14):
  0.56 V max at 0.35 A, 0.85 V max at 0.53 A.

3V3 capacitance. FB301 and L1 are low impedance at the loop frequencies, so C311 and C1 count:

| Rail | Capacitors | Nominal | Effective at 3.3 V (typical X5R, unverified) |
| --- | --- | --- | --- |
| +3V3 | C204 10 uF, C206 22 uF, C312 10 uF, C205, C207, C313 100 nF | 42.3 uF | 6 + 10 + 6 + 0.3 = 22.3 uF |
| 3V3A (FB301) | C311 10 uF, C305-C310 100 nF | 10.6 uF | 6.6 uF |
| 3V3_RF (L1) | C1 10 uF, C2 100 nF | 10.1 uF | 6.1 uF |
| Total | | 63.0 uF | ~35 uF |

The total lies inside the 1-220 uF range. The MLCCs sit at their loads and connect through board copper, which adds
series resistance to the 2 mohm ESR floor. Check 3V3 for ringing on a load step at bring-up (section 8.2).

USB attach (USB 2.0 7.2.4.1: at most 10 uF at attach, including the capacitance visible through the regulator):

```
VBUS side, charged at attach through D202:
  C208 0.1 + C202 10 + C203 0.1 = 10.2 uF nominal, ~7.7 uF effective at 5 V (10 uF 25 V 0805 X5R, typical)
  Q = 7.7 uF x 5.25 V = 40 uC   <=   10 uF x 5.25 V = 52.5 uC
C209 sits behind R216 68k (tau 4 ms): 0.3 uC over milliseconds, no attach current.
3V3 side, behind U203:
  U203 starts after VIN passes UVLO (2.4 V max) and ramps OUT over ~500 us (soft-start).
  Q3V3 = 35 uF x 3.3 V = 115 uC  ->  I = 115 uC / 0.5 ms = 0.23 A (0.42 A with the nominal 63 uF)
  The VBUS edge settles in microseconds (review E05 simulation), before the 3V3 ramp starts.
Visible capacitance at attach: ~7.7 uF effective <= 10 uF.
```

The 0.23 A soft-start current then adds to the start-up load. That load is above 100 mA anyway (review E06,
accepted in section 8.3).

VBUS protection:

- D201 SMF6.0A: VRWM 6 V, VBR 6.67-7.37 V, VC 10.3 V at 19.4 A (10/1000 us), IR 400 uA max at 6 V
  (HJC SMF DS p.2). The standoff is above the 5.5 V maximum of a Type-C charger.
- Clamp against the ratings on VBUS and 5V_SYS: U203 IN 20 V (1.9x), F201 16 V (1.6x), C202 25 V, SS34 40 V.
- The hot-plug peak is 7.1 V (review, scenario 4): below the SMF6.0A VBR max, far below every rating.

Dissipation, P = (VIN - 3.3 V) x I:

| Case | VIN | 0.27 A | 0.35 A | 0.53 A burst |
| --- | --- | --- | --- | --- |
| USB only | 5.0 - 0.1 (PTC) - 0.4 = 4.5 V | 0.32 W | 0.42 W | 0.64 W |
| PoE (with or without USB) | 5.66 - 0.4 = 5.26 V | 0.53 W | 0.69 W | 1.04 W |
| PoE, worst (review E10) | 5.66 x 1.05 - 0.27 (SS34 hot) = 5.67 V | 0.64 W | 0.83 W | 1.26 W |

Thermal estimate at the 0.83 W worst case and 50 C ambient:

- RthJA 95.4 C/W on the JEDEC board (TLV761 DS 5.4): Tj = 50 + 0.83 x 95.4 = 129 C, above the 125 C
  recommended maximum (DS 5.3). The minimum land is not enough.
- Requirement: the tab (OUT = 3V3) sits on 4 cm2 or more of F.Cu 3V3 copper. A 3V3 pour of 3 cm2 or more on B.Cu lies
  under it. 8 or more vias of 0.6/0.3 mm join the two.
- Estimate: RthJA ~70-73 C/W. Source: LM1117 DS SNOS412Q Table 9-2, same SOT-223 package on a 1 oz test board:
  0.53 in2 (3.4 cm2) top 75 C/W, 0.76 in2 (4.9 cm2) top 69 C/W, 0.5 + 0.5 in2 top and bottom 70 C/W.
  Tj = 50 + 0.83 x 72 = 110 C. The inner GND planes lower it further (unverified).
- USB only, 0.35 A: 0.42 W, Tj = 50 + 0.42 x 72 = 80 C.
- Bring-up: Tj = TB + 33.4 C/W x P, with TB the board temperature 1 mm from the package (DS 7.1.5, psi_JB DS 5.4).
- Worst USB input: VBUS 4.40 V (unverified) - 0.12 V (PTC) - 0.37 V (SS34) = 3.91 V. U203 needs 3.3 + 0.56 = 3.86 V
  at 0.35 A: it regulates. In a 0.53 A Wi-Fi burst it drops out to ~3.06 V, still above the ESP32 3.0 V minimum.
- D202/D402 dissipate 0.4 V x 0.35 A = 0.14 W each, in SMA packages.

## 5. Circuits and parts

### 5.1 RF: Ebyte E07-900MM10S module

Module (Ebyte E07-M series DS v1.0):

- TI CC1101 with a 26 MHz 10 ppm crystal, match and shield inside (DS 1.1, 2.3, pdf p.4, p.6). The registers are the
  CC1101 registers: the firmware keeps its PHY (section 7).
- 855-925 MHz. TX 9-11 dBm maximum. RX -107 to -109 dBm at 1.2 kbps (DS 2.1, pdf p.5).
- Supply 1.8-3.6 V, 3.3 V or more for full power, above 3.8 V the module dies. TX 32 mA, RX 18 mA (DS 2.2, pdf p.6).
  SPI 0-10 Mb/s (DS 2.3).
- ANT pad 6 is 50 ohm (DS 2.3 and 3.1, pdf p.7).
- 10.0 x 10.0 x 2.5 mm, 20 castellated pads at 1.27 mm. The first pad centre is 2.46 mm from the corner. Module pad
  0.75 x 0.80 mm with a 0.40 mm half hole. No centre pad (DS 3.1, pdf p.7).
- The CE and FCC certificates in the DS cover the 14 x 20 mm E07-900M10S only (DS 6).

PHY that freedo uses (`cc1101/cc1101.go`): 868.324 MHz (channel 26), 2-GFSK, 49.99 kBaud, 25.39 kHz deviation,
135 kHz RX bandwidth, sync D391D391, variable length, CRC-16, FOC +-34 kHz, GDO0 = CRC OK (IOCFG0 0x07),
GDO2 = RX FIFO threshold (IOCFG2 0x00). The module crystal (+-10 ppm = +-8.7 kHz) stays inside the FOC range.

Pad table. The pad drawings are identical: 10.0 x 10.0 mm, 20 pads, 1.27 mm pitch, 2.46 mm, pad 0.75 x 0.80 mm with a
0.40 mm half hole (E07 DS 3.1 pdf p.7-8, tolerance +-0.1 mm; E22-900MM22S DS ch. 3 pdf p.5-6, tolerance +-0.2 mm).
The EasyEDA lands of both parts (C5844212, C5333985) put pad 1 at the same place with the same numbering.

| Pad | E07-900MM10S | E22-900MM22S | Net on this board |
| --- | --- | --- | --- |
| 1 | VCC | VCC | 3V3_RF |
| 2 | GND | GND | GND |
| 3 | NC | NRST | E22_NRST_PAD3, R101 0R DNP to GPIO21 |
| 4 | NC | NC | open |
| 5 | NC | NC | open |
| 6 | ANT | ANT | RF_ANT, R191 0R to J1 |
| 7 | GND | GND | GND |
| 8 | NC | NC | open |
| 9 | NC | TXEN | E22_TXEN_PAD9, R104 0R DNP to pad 19 |
| 10 | NC | RXEN | E22_RXEN_PAD10, R102 0R DNP to GPIO47 |
| 11 | NC | BUSY | E22_BUSY_PAD11, R103 0R DNP to GPIO48 |
| 12 | MISO/GDO1 | MISO | CC_MISO, GPIO42 |
| 13 | MOSI | MOSI | CC_MOSI, GPIO41 |
| 14 | CSN | NSS | CC_CSN, GPIO40, R210 10k to 3V3_RF |
| 15 | SCK | SCK | CC_SCK, R209 33R to GPIO39 |
| 16 | GND | GND | GND |
| 17 | NC | NC | open |
| 18 | GDO0 | DIO3 | CC_GDO0, GPIO37 |
| 19 | NC | DIO2 | E22_DIO2_PAD19, R104 0R DNP to pad 9 |
| 20 | GDO2 | DIO1 | CC_GDO2, GPIO36 |

Every supply and GND pad has the same function on both modules.

E22-900MM22S option (SX1262, LCSC C5333985, not fitted):

- VCC 1.8-3.7 V, 3.7 V absolute. TX 100 mA at up to 22 dBm (E22 DS 2.1, 2.2, pdf p.4).
- The E22-only lines sit on E07 NC pads. Each goes through a DNP 0R. With the E07 fitted and the links open,
  the NC pads touch nothing.
- DIO2 drives TXEN through R104 (E22 DS 4.2, pdf p.7). NRST has an internal 50 kohm pull-up (SX1262 DS Table 8-3),
  so pad 3 needs no external part.
- To fit the E22: R101-R104 0R, 0R (C17168) in place of L1, and an SX1262 driver (section 7).

Supply: 3V3 -> L1 BLM15HG102SN1D -> 3V3_RF -> C2 100 nF at pad 1 and C1 10 uF next to it.

- The E07 DS asks for ceramic filter capacitors at VCC (DS 3.2). The E22 basic circuit uses 10 uF + 10 nF
  (E22 DS 5.1, pdf p.7). The Ebyte EVB feeds VCC from its LDO output with 1 uF (E07 DS 5.1, pdf p.11).
- L1 keeps the ESP32 and W5500 noise off the module supply. It drops 1.1 ohm (max) x 32 mA = 35 mV.
  With 10 uF the bead and the capacitor do not ring: Z0 = sqrt(1.5 uH / 6 uF) = 0.5 ohm, below the DCR
  (bead inductance estimated, unverified).
- R210 10k pulls CSn up to 3V3_RF, so the radio stays deselected while the ESP32 boots.

RF path: U1 pad 6 (RF_ANT) -> R191 0R -> J1 (RF_J1).

- C191 (DNP) is a shunt position on the module side. With R191 it gives a pi or low-pass position if the harmonics
  need a filter (section 8.2).
- L191 56 nH shunt to GND at J1: DC and ESD path for a discharge into the SMA centre pin (review E01).
  LQW15AN56NG00D: +-2 %, Q min 25 at 200 MHz, Q typ 53 at 800 MHz and 55 at 900 MHz, SRF min 2.8 GHz, 200 mA,
  1.17 ohm max (Murata LQW15AN_00 tables, catalog pdf p.188 and p.190).
- At 868 MHz L191 is a 338 ohm shunt (62 nH effective, SRF effect). Insertion loss 0.035-0.05 dB, return loss 22.7 dB
  (calculation, Q 25-55). The E22 basic circuit uses a 330 nH shunt at ANT for the same purpose (E22 DS 5.1).
- 50 ohm grounded coplanar feed from U1 pad 6 to J1 (section 6.1).

| Ref | Function | Value | Package | MPN | LCSC | Class | Mount | Asm | Fit |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| U1 | 868 MHz radio module | E07-900MM10S | LCC-20 10 x 10 mm | E07-900MM10S | C5844212 | E | SMD | JLC | yes |
| L1 | 3V3_RF feed | 1 kohm at 100 MHz, 250 mA | 0402 | BLM15HG102SN1D | C76889 | E | SMD | JLC | yes |
| C1 | 3V3_RF bulk | 10 uF 10 V X5R | 0603 | CL10A106KP8NNNC | C19702 | B | SMD | JLC | yes |
| C2 | U1 pad 1 | 100 nF 50 V X7R | 0603 | CC0603KRX7R9BB104 | C14663 | B | SMD | JLC | yes |
| R210 | CSn pull-up | 10k 1% | 0603 | 0603WAF1002T5E | C25804 | B | SMD | JLC | yes |
| R191 | feed series | 0R | 0402 | 0402WGF0000TCE | C17168 | B | SMD | JLC | yes |
| C191 | feed shunt | - | 0402 | - | - | - | SMD | none | DNP |
| L191 | ESD shunt at J1 | 56 nH 2% wire-wound | 0402 | LQW15AN56NG00D | C167482 | E | SMD | JLC | yes |
| R101-R104 | E22 links | 0R | 0603 | 0603WAF0000T5E | C21189 | B | SMD | none | DNP |
| J1 | antenna | SMA jack, 50 ohm, 0-3 GHz, 1.6 mm board | edge mount | XUNPU SMA-KE-347-H13.5-1.6 | C5723200 | E | edge | hand | yes |

U1 footprint `Ebyte_LCC-20_10x10mm_P1.27mm_E07-MM10S_E22-MM22S`: 20 lands 0.8 x 1.7 mm, 0.8 mm under the module and
0.9 mm outside the body, so an iron reaches every pad. No pad under the module. A 3 x 3 grid of tented GND vias
(2.8 mm pitch) stitches the F.Cu pour under the body to In1.Cu. A rule area keeps tracks out of the module area on all
copper layers. The top GND pour and its stitching vias are allowed (E07 DS 4.1). The 3D model is
the E22 model from easyeda2kicad (same 10.0 x 10.0 x 2.5 mm body, centred, offset 0), because C5844212 has none.

J1 choice: the XUNPU drawing shows an end-launch jack for a 1.60 mm board. The center pin (0.95 mm) lies on
a top pad. The body legs clamp both board faces, so the joints are easy to hand-solder. Stock 1,678.
The land pattern is three 1.20 x 4.20 mm pads with a 5.65 mm span (XUNPU drawing). Repeat the two ground
pads on the bottom side. J1 fixes the board thickness at 1.6 mm.

### 5.2 MCU and USB

- U201 ESP32-S3-MINI-1-N8: ESP32-S3FN8, 8 MB quad flash in package, no PSRAM (MINI-1 DS Table 1-1).
- Land pattern: the KiCad stock symbol `RF_Module:ESP32-S3-MINI-1` points to the footprint
  `RF_Module:ESP32-S2-MINI-1`. Check every pad of the footprint against MINI-1 DS Fig 11-1 before use:
  60 pads 0.8 x 0.4 mm at 0.85 mm pitch, 4 corner pads 0.8 x 0.8 mm, center EPAD. Too much EPAD paste lifts
  the module (MINI-1 DS 9).
- EN: R205 10k to 3V3, C201 1 uF to GND (tau 10 ms), SW201 RESET to GND. Short EN trace
  (MINI-1 DS 9, HDG 1.3.3).
- BOOT: R206 10k to 3V3, SW202 to GND, no capacitor (HDG 1.3.9).
- GPIO46 and GPIO45 stay unconnected (internal pull-downs): SPI boot and VDD_SPI 3.3 V
  (MINI-1 DS Tables 4-1 to 4-4). GPIO3 is ignored with default eFuses (MINI-1 DS Table 4-5).
- USB-C J201: 16P, USB 2.0 only. A6-B6 (D+) and A7-B7 (D-) tied at the connector. CC1 (A5) and CC2 (B5)
  each get their own 5.1k to GND (R201, R202). SBU1/SBU2 unconnected. Shell to GND.
- U202 H5VUT2U at the connector on D+/D-: pin 1 D-, pin 2 D+, pin 3 GND. It has no VBUS pin, so the ESP32 D+
  pull-up has no path to VBUS (review E03). 0.6 pF typ and 0.8 pF max from I/O to GND, VRWM 5 V, VBR 6-9 V,
  IEC 61000-4-2 +-20 kV contact (HJC H5VUT2U DS p.1-2): a small load for full-speed USB.
- C208 100 nF from VBUS to GND at J201.
- R215 4.7k from VBUS to GND: with PoE on and USB unplugged, it holds the D202 reverse leakage down. 75 uA (typ at
  75 C, SS34 DS Fig 2) gives 0.35 V, below the 0.8 V check. It dissipates 6.4 mW at 5.5 V and 10.7 mW at the 7.1 V
  hot-plug peak, on a 0.1 W 0603.
- VBUS sense: R216 68k from VBUS (after F201) to VBUS_SENSE (GPIO17), R217 100k and C209 100 nF to GND.

```
VBUS_SENSE = VBUS x 100k / (68k + 100k) = 0.595 x VBUS   (1 % parts: 0.590-0.600)
VIH min = 0.75 x VDD <= 0.75 x 3.36 V = 2.52 V           (MINI-1 DS Table 6-3; VDD 3.24-3.36 V, section 4.4)
VBUS 4.40 V -> 2.60-2.64 V   reads high (>= 2.52 V)
VBUS 5.50 V -> 3.25-3.30 V   below VIH max = VDD + 0.3 V >= 3.54 V (MINI-1 DS Table 6-3)
7.1 V hot-plug peak (microseconds): tau = (68k || 100k) x 100 nF = 4.0 ms, the pin moves < 15 mV
At power-up the pin rises with tau 4 ms, slower than the 0.5 ms soft-start of 3V3: it stays below VDD + 0.3 V.
Divider current 33 uA at 5.5 V. GPIO leakage 50 nA max (Table 6-3): no effect.
```

- R203, R204 22R in series, close to pads 23/24 (HDG 1.3.13). HDG also suggests optional capacitors to GND.
  Rev A omits them to keep the 90 ohm pair free of stubs.
- No external D+ pull-up. The USB-Serial-JTAG controller drives its own D+ pull-up (HDG Table 9, USB_PU) and
  enables it at reset. The firmware turns it off while VBUS_SENSE is low (section 7, USB 2.0 7.2.1).
- VBUS path: J201 VBUS -> F201 0.75 A PTC -> D201 SMF6.0A to GND -> D202 SS34 -> 5V_SYS.
- R207 1k in series with U0TXD at the module. HDG 1.3.7 asks for a series resistor (~499R) against 80 MHz
  harmonics; 1k also limits the clamp current from a USB-UART adapter (review E02). R218 1k in series with U0RXD
  at J203.
- R208, R209 33R in series with the two SCLK lines at the module (HDG 1.3.8). No capacitor.
- Power LED D203 on 3V3 through R211 1k. TX, RX, STATUS LEDs are active high: GPIO -> 1k -> LED -> GND.
  The power-up low glitch keeps them off.
- All LEDs on the board are red KT-0603R, the only Basic 0603 LED in the database. One part avoids extra fees.

| Ref | Function | Value | Package | MPN | LCSC | Class | Mount | Asm | Fit |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| U201 | MCU module | ESP32-S3-MINI-1-N8 | module | ESP32-S3-MINI-1-N8 | C2913206 | E | SMD | JLC | yes |
| J201 | USB-C receptacle | 16P USB 2.0 | SMD + shell legs | HRO TYPE-C-31-M-12 | C165948 | E | SMD | JLC | yes |
| U202 | USB ESD | 2-line, 0.6 pF, no VBUS pin | SOT-23 | H5VUT2U | C20615824 | P | SMD | JLC | yes |
| F201 | VBUS PTC | 0.75 A hold, 1.5 A trip, 16 V | 0805 | BSMD0805-075-16V | C976303 | E | SMD | JLC | yes |
| D201 | VBUS TVS | VRWM 6 V, VC 10.3 V | SOD-123FL | SMF6.0A | C19077499 | P | SMD | JLC | yes |
| D202 | USB OR diode | 40 V 3 A Schottky | SMA | SS34 | C8678 | B | SMD | JLC | yes |
| U203 | 3.3 V LDO | 1 A, 18 V in, soft-start | SOT-223 | TLV76133DCYR | C7527500 | E | SMD | JLC | yes |
| C202 | 5V_SYS / LDO in | 10 uF 25 V X5R | 0805 | CL21A106KAYNNNE | C15850 | B | SMD | JLC | yes |
| C203, C205, C207, C208, C209 | decoupling, VBUS sense filter | 100 nF 50 V X7R | 0603 | CC0603KRX7R9BB104 | C14663 | B | SMD | JLC | yes |
| C204 | LDO out | 10 uF 10 V X5R | 0603 | CL10A106KP8NNNC | C19702 | B | SMD | JLC | yes |
| C206 | module pad 3 | 22 uF 6.3 V X5R | 0603 | CL10A226MQ8NRNC | C59461 | B | SMD | JLC | yes |
| R201, R202 | CC Rd | 5.1k 1% | 0603 | 0603WAF5101T5E | C23186 | B | SMD | JLC | yes |
| R203, R204 | USB series | 22R 1% | 0603 | 0603WAF220JT5E | C23345 | B | SMD | JLC | yes |
| R205, R206, R210 | EN, GPIO0, radio CSn pull-ups | 10k 1% | 0603 | 0603WAF1002T5E | C25804 | B | SMD | JLC | yes |
| R215 | VBUS bleeder | 4.7k 1% | 0603 | 0603WAF4701T5E | C23162 | B | SMD | JLC | yes |
| R216 | VBUS sense, top | 68k 1% | 0603 | 0603WAF6802T5E | C23231 | B | SMD | JLC | yes |
| R217 | VBUS sense, bottom | 100k 1% | 0603 | 0603WAF1003T5E | C25803 | B | SMD | JLC | yes |
| C201 | EN delay | 1 uF 50 V X5R | 0603 | CL10A105KB8NNNC | C15849 | B | SMD | JLC | yes |
| SW201, SW202 | RESET, BOOT | tactile | SMD 5.1 x 5.1 | TS-1187A-B-A-B | C318884 | B | SMD | JLC | yes |
| R207, R218 | U0TXD, U0RXD series | 1k 1% | 0603 | 0603WAF1001T5E | C21190 | B | SMD | JLC | yes |
| R208, R209 | SCLK series | 33R 1% | 0603 | 0603WAF330JT5E | C23140 | B | SMD | JLC | yes |
| D203-D206 | PWR, TX, RX, STATUS | red LED | 0603 | KT-0603R | C2286 | B | SMD | JLC | yes |
| R211-R214 | LED series | 1k 1% | 0603 | 0603WAF1001T5E | C21190 | B | SMD | JLC | yes |
| J203 | GPIO header | 1x10 2.54 mm | THT | - | - | - | THT | hand | DNP |
| R219, R220 | I2C pull-ups | 4.7k 1% | 0603 | 0603WAF4701T5E | C23162 | B | SMD | none | DNP |

C202 is a 25 V 0805 part: a 0603 22 uF part is rated 6.3 V and loses most of its capacitance at 5 V bias
(unverified). C202 is 10 uF so that the attach capacitance stays at or below 10 uF (section 4.4).

### 5.3 Ethernet: W5500

Circuit (W5500 DS v1.1.0 and WIZnet "W5500-Ref-ExternalMag"):

- MDI, chip side: R304/R305 49.9R 1% from TXP/TXN to 3V3A. R306 10R from T301 pin 2 (TX CT) to 3V3A,
  C314 22 nF from pin 2 to GND. C315/C316 6.8 nF in series between RXP/RXN and T301 pins 6/8.
  R307/R308 82R 1% from RXP/RXN (chip side of the series capacitors) to a node. The node goes to
  T301 pin 7 (RX CT) and to GND through C317 10 nF. R309-R312 0R damping in TXP, TXN, RXP, RXN
  (WIZnet note a: 33R gave the best EMI in WIZnet tests).
- MDI, line side: T301 16 (TX+) -> J301-1, 14 (TX-) -> J301-2, 11 (RX+) -> J301-3, 9 (RX-) -> J301-6.
  Line CTs: pin 15 (pair 1/2), pin 10 (pair 3/6).
- T301 CND-tek B1608S: pinout TD+ 1, TCT 2, TD- 3, RD+ 6, RCT 7, RD- 8, RX- 9, RXCT 10, RX+ 11,
  TX- 14, TXCT 15, TX+ 16, same as H1102NL. 350 uH OCL at 8 mA, 1500 Vrms hipot, 720 mA per PoE port
  (B1608S DS). Second choice: YDS 11FB-05NL (C97692, 350 mA PoE). H1102NL has no PoE rating.
- Straps and analog pins: EXRES1 (10) R303 12.4k 1% to AGND. TOCAP (20) C303 4.7 uF, short trace.
  1V2O (22) C304 10 nF. VBG (18) floating. RSVD 23 to GND. RSVD 38-42, NC 12/13/46/47, DNC 7,
  SPDLED 24, DUPLED 26 unconnected. PMODE2-0 (43-45) open: internal pull-ups give 111, all capable with
  auto-negotiation (W5500 DS Table 2).
- RSTn (37) R313 10k and INTn (36) R314 10k pull-ups to 3V3.
- Supply: 3V3 = 3V3D to VDD (28) with C312 10 uF + C313 100 nF. FB301 120 ohm bead from 3V3D to 3V3A.
  3V3A: C305-C310 100 nF, one at each AVDD pin (4, 8, 11, 15, 17, 21), plus C311 10 uF.
  WIZnet uses a 120 ohm 0603 bead; 100-2000 ohm at 100 MHz is adequate (WIZnet note g).
- Crystal Y301 YXC X322525MOB4SI, 25 MHz, CL 12 pF, +-10 ppm. R301 1M across XI/XO, R302 0R in series on
  XO (W5500 DS Figure 3). The W5500 DS asks for +-30 ppm and a CL 18 pF crystal (DS 5.5.3). The load
  capacitors set the effective load to the crystal CL:
  C301 = C302 = 2 x (CL - Cstray) = 2 x (12 - 3) = 18 pF C0G. Cstray 3 pF is unverified: the W5500 DS does not
  give the XI/XO pin capacitance. LilyGO T-ETH-Lite-S3 uses the same crystal class with 18 pF.
- LEDs on the board, because J301 has none: LINKLED (25) and ACTLED (27) are active low.
  3V3 -> LED -> 1k -> pin (D301/R315 link, D302/R316 activity). LINK is on when the link is up.
- J301 Hirose TM24RSG-5A-88: withstands 1500 V AC for 1 minute from the terminals to the shield, 500 V AC between
  contacts (TM24R catalog p.2). This meets the 802.3 isolation test between the MDI and the shield (review E09).
  Tab up, contact 1 on the right seen from the mouth. 4 outer shell holes and 2 separate ground spring holes, all on
  SHIELD (catalog p.3). Stock 65, $4.16.
- W5500 has no auto-MDIX (W5500 DS 5.5.6). The MDI pinout is the NIC pinout: a straight cable to a switch
  works. A link to another MDI device needs a crossover cable or an auto-MDIX peer.
- SPI: mode 0 or 3. 33.3 MHz guaranteed, 80 MHz theoretical (W5500 DS 5.5.4).
- Bob Smith termination (always fitted, PoE-aware, see the resolution table): R317-R320 75R 1% in series with
  C318-C321 10 nF 100 V from T301-15, T301-10, J301 4+5 and J301 7+8 to node BS. C322 1 nF 2 kV from BS
  to SHIELD. C323 1 nF 2 kV from SHIELD to GND. J301 shield pins on SHIELD. No DC path from SHIELD to GND.

WIZnet reference contradictions:

| Item | Conflict | Resolution | Source |
| --- | --- | --- | --- |
| RX termination | ExternalMag ref: R6/R7 82R. RJ45WithMag ref: 49.9R. | 82R. This board uses a discrete transformer, and ExternalMag is the WIZnet reference for that topology. The W5500 DS gives no MDI values. No document explains the difference; LilyGO T-ETH-Lite-S3 also uses 82R with H1102NLT. | W5500-Ref-ExternalMag, W5500-Ref-RJ45WithMag (both 2015-09-25) |
| Pin 23 (RSVD) | DS: "It must be tied to GND." ExternalMag ref: no-connect marker. | Tie to GND. The datasheet is the part specification. The pin has an internal pull-down (48-174k), so the open pin of the ref also reads low. | W5500 DS v1.1.0 Table 2 p.9, 5.3 p.59 |
| Bob Smith with PoE | WIZnet ref: 75R from each line CT to one node, 49.9R networks on 4/5 and 7/8, 150R from RX+/RX- to the node. With PoE the two 75R put 150 ohm across the PSE output (CT to CT); 48 V / 150 ohm = 320 mA. | 75R in series with 10 nF on each of the four power lines, to a common node, then 1 nF 2 kV to SHIELD. Delete the 49.9R and 150R networks. The WIZnet documents do not cover PoE. | TI SLUA454 1.4 and Figure 1-1 (0.01 uF 100 V, 1000 pF 2 kV 1808); TI SLVA233A p.12 (2 kV line-to-earth, 200 V for the 10 nF) |
| TX CT bias | Both WIZnet refs: R3 10R. LilyGO: 49.9R. | 10R. Both WIZnet references agree. | both WIZnet reference schematics |

The 10 nF DC blocks use 100 V parts, as in SLUA454. SLVA233A calls 200 V a safe choice for surge. This is an
open item if the board must pass a surge test.

| Ref | Function | Value | Package | MPN | LCSC | Class | Mount | Asm | Fit |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| U301 | Ethernet controller | W5500 | LQFP-48 7x7 | W5500 | C32843 | E | SMD | JLC | yes |
| T301 | LAN transformer | 1:1, 350 uH, PoE+ 720 mA | SMD-16 12.7x7 | CND-tek B1608S | C507596 | E | SMD | JLC | yes |
| J301 | RJ45, shielded, no LEDs, 1500 V AC contacts to shield | 8P8C | THT | Hirose TM24RSG-5A-88 | C6285250 | E | THT | hand | yes |
| Y301 | crystal | 25 MHz CL 12 pF +-10 ppm | 3225-4P | X322525MOB4SI | C9006 | B | SMD | JLC | yes |
| C301, C302 | crystal load | 18 pF 5% C0G | 0603 | CL10C180JB8NNNC | C1647 | B | SMD | JLC | yes |
| R301 | XI-XO | 1M 1% | 0603 | 0603WAF1004T5E | C22935 | B | SMD | JLC | yes |
| R302, R309-R312 | XO series, MDI damping | 0R | 0603 | 0603WAF0000T5E | C21189 | B | SMD | JLC | yes |
| R303 | EXRES1 | 12.4k 1% | 0603 | 0603WAF1242T5E | C22865 | E | SMD | JLC | yes |
| C303 | TOCAP | 4.7 uF 16 V X5R | 0603 | CL10A475KO8NNNC | C19666 | B | SMD | JLC | yes |
| C304, C317 | 1V2O, RX CT | 10 nF 50 V X7R | 0603 | 0603B103K500NT | C57112 | B | SMD | JLC | yes |
| FB301 | 3V3D -> 3V3A | 120 ohm at 100 MHz, 2 A | 0603 | BLM18PG121SN1D | C14709 | B | SMD | JLC | yes |
| C305-C310, C313 | AVDD, VDD decoupling | 100 nF 50 V X7R | 0603 | CC0603KRX7R9BB104 | C14663 | B | SMD | JLC | yes |
| C311, C312 | 3V3A, VDD bulk | 10 uF 10 V X5R | 0603 | CL10A106KP8NNNC | C19702 | B | SMD | JLC | yes |
| R304, R305 | TX pull-ups | 49.9R 1% | 0603 | 0603WAF499JT5E | C23185 | B | SMD | JLC | yes |
| R306 | TX CT bias | 10R 1% | 0603 | 0603WAF100JT5E | C22859 | B | SMD | JLC | yes |
| C314 | TX CT | 22 nF 50 V X7R | 0603 | CL10B223KB8NNNC | C21122 | B | SMD | JLC | yes |
| C315, C316 | RX series | 6.8 nF 50 V X7R | 0603 | 0603B682K500NT | C1631 | B | SMD | JLC | yes |
| R307, R308 | RX termination | 82R 1% | 0603 | 0603WAF820JT5E | C23255 | P | SMD | JLC | yes |
| R313, R314 | RSTn, INTn pull-ups | 10k 1% | 0603 | 0603WAF1002T5E | C25804 | B | SMD | JLC | yes |
| D301, D302 | LINK, ACT | red LED | 0603 | KT-0603R | C2286 | B | SMD | JLC | yes |
| R315, R316 | LED series | 1k 1% | 0603 | 0603WAF1001T5E | C21190 | B | SMD | JLC | yes |
| R317-R320 | Bob Smith | 75R 1% | 0603 | 0603WAF750JT5E | C4275 | B | SMD | JLC | yes |
| C318-C321 | Bob Smith DC block | 10nF/100V X7R | 0603 | 0603B103K101NT | C43253 | E | SMD | JLC | yes |
| C322, C323 | BS -> SHIELD, SHIELD -> GND | 1nF/2kV X7R | 1808 | 1808B102K202NT | C81332 | E | SMD | JLC | yes |

The schematic Value of C318-C323 carries the voltage (`10nF/100V`, `1nF/2kV`). The BOM groups by Value and
Footprint, so these parts cannot merge with the 10 nF 50 V C304 and C317.

C322/C323 are 1808 because they cross the isolation barrier. A standard 1206 land has a ~1.8 mm pad gap
(unverified), below the 2 mm rule. The 1206 part C9196 is Basic and would save one Extended fee.

### 5.4 PoE (option)

Circuit (DP9900 DS Figure 3, TI SLUA454 1.4):

- BR401 MB10S: AC inputs J301 4+5 and J301 7+8 (pins tied per pair). BR402 MB10S: AC inputs T301-15 and
  T301-10. Both "+" to VIN+ (U401 pins 5, 6), both "-" to VIN- (pins 7, 8). MB10S is the 1 kV member of the
  MB6S family in the DP9900 DS.
- D401 SMAJ58A across VIN+ (cathode) and VIN- (anode).
- No other capacitance across VIN+/VIN-: the total after the bridges must stay <= 0.12 uF for detection
  (SLUA454 1.4). The DP9900 DS shows no input capacitor.
- U401 DP9900LP-5V: 36-57 V in, 60 V absolute, 80 V for 1 ms. 1.4 A typ output, 7 W at 70 C, 4.5 W at 85 C.
  1500 V isolation (impulse test, DS 3.3 line 9). The DS does not state the IEEE class (unverified).
- Output: C401 100 uF 25 V and C402 10 uF 25 V across +VDC/-VDC, as DP9900 DS Figure 3.
- C401 Panasonic EEH-ZA1E101XP, hybrid aluminium: 105 C 10000 h, 30 mohm ESR, 2 A ripple, 6.3 x 7.7 mm
  (Panasonic ZA catalog p.1-2). A 24/7 board needs the long life (review E13). Place it away from U203 and U401.
  The DP9900 DS gives no limit for the output capacitance or ESR (unverified); C402 is a ceramic anyway.
  -VDC is system GND.
- RB (bleeder in Figure 3) is R402, 56R 2512 from +VDC to GND. The footprint is present, the part is not
  fitted: the board load is above 100 mA in normal operation (section 4.3). Fit R402 if the PSE drops power at
  low load: 5.66 V / 56R = 101 mA, 0.57 W, so a 1 W part.
- R401 0R from ADJ (4) to -VDC (3): 5.66 V (DP9900 DS Table 3). The same position takes 100k (5.27 V) or
  470k (5.08 V). Remove R401 for 5.00 V.
- D402 SS34 from +VDC to 5V_SYS.
- Mechanics (DP9900 DS 4.2): body 21.2 x 14.2 mm, 8.0 mm high, 1.5 mm standoff. Pins 0.64 mm, 3.00 mm long.
  Pin centers along the long axis: 1.27, 3.81, 17.38, 19.92 mm from the body end. Rows 11.38 mm apart.
  Top view: pin 1 bottom-left, pin 4 bottom-right, pin 5 top-right, pin 8 top-left. Output row 1-4,
  input row 5-8. Solder the module directly; hole size for the 0.64 mm square pin: 1.1 mm (unverified).
- JLC fits BR401, BR402, D401, R401, C401, C402 and D402 on every board: one order has one BOM, and these parts cost
  less than $1 per board. A board without PoE leaves U401 off. With U401 absent the line-side parts carry no current.
  The Bob Smith network, T301 and J301 stay on every board.

| Ref | Function | Value | Package | MPN | LCSC | Class | Mount | Asm | Fit |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| BR401, BR402 | bridges | 1 kV 1 A | MBS | MB10S | C2488 | B | SMD | JLC | yes |
| D401 | PD input TVS | 58 V VRWM, 93.6 V VC | SMA | SMAJ58A (MDD) | C123816 | E | SMD | JLC | yes |
| U401 | PD module | 5 V isolated, 802.3af | 8-pin THT module | SDAPO DP9900LP-5V | C5380108 | E | THT | hand | opt |
| R401 | output adjust | 0R | 0603 | 0603WAF0000T5E | C21189 | B | SMD | JLC | yes |
| C401 | output bulk | 100 uF 25 V hybrid aluminium, 105 C 10000 h | SMD 6.3 x 7.7 | Panasonic EEHZA1E101XP | C264047 | E | SMD | JLC | yes |
| C402 | output ceramic | 10 uF 25 V X5R | 0805 | CL21A106KAYNNNE | C15850 | B | SMD | JLC | yes |
| D402 | PoE OR diode | 40 V 3 A Schottky | SMA | SS34 | C8678 | B | SMD | JLC | yes |
| R402 | RB bleeder | 56R 5% 1 W | 2512 | ROHM MCR100JZHJ560 | C308356 | E | SMD | none | DNP |

### 5.5 Test pads and GPIO header

- Labelled round 1.5 mm test pads, probed from the top: 3V3, 5V_SYS, GND (at least 3, one next to each group),
  radio SCLK, SI, SO, CSn, GDO0, GDO2 (TP101-TP107), UART0 TX and RX (TP211, TP212, TP213 GND).
  TP211/TP212 give the ROM log without a header fitted.
- TP101-TP107 are plated pads with a 0.7 mm hole (`TestPoint_THTPad_D1.5mm_Drill0.7mm`) beside the radio bus. A
  B.Cu stub from one tap via on the bus reaches the pad directly, so no stub needs a second via (section 6.4).
  The other test pads are SMD.
- J203 (DNP, Asm hand): 1x10 2.54 mm THT header at a board edge. One row at the edge gives every pin its own silk
  label and takes Dupont jumpers. A 2x5 hides the inner-row labels.

| J203 pin | Signal | GPIO | Circuit |
| --- | --- | --- | --- |
| 1 | 3V3 (output) | - | +3V3 |
| 2 | I2C SDA | 15 | R219 4.7k to 3V3, DNP |
| 3 | I2C SCL | 16 | R220 4.7k to 3V3, DNP |
| 4 | IO1 | 1 | ADC1_CH0 |
| 5 | IO2 | 2 | ADC1_CH1 |
| 6 | IO7 | 7 | ADC1_CH6 |
| 7 | IO8 | 8 | ADC1_CH7 |
| 8 | UART0 TX | 43 | R207 1k at U201 |
| 9 | UART0 RX | 44 | R218 1k at J203 |
| 10 | GND | - | GND |

- Pin 1 is a 3V3 output for small sensors. Never feed 3V3 into it: U203 has no reverse-current protection
  (TLV761 DS 7.1.3).
- The 1k resistors limit the current into the U0TXD/U0RXD pad clamps from an adapter on an unpowered board to
  <= 3 mA (review E02). EN is not on the header: download mode uses USB, or BOOT and RESET.

### 5.6 JLC Extended part count

Unique Extended parts that JLC assembles:

| # | Part | LCSC | Block |
| --- | --- | --- | --- |
| 1 | ESP32-S3-MINI-1-N8 | C2913206 | MCU |
| 2 | HRO TYPE-C-31-M-12 | C165948 | USB |
| 3 | BSMD0805-075-16V | C976303 | USB |
| 4 | TLV76133DCYR | C7527500 | power |
| 5 | E07-900MM10S | C5844212 | RF |
| 6 | LQW15AN56NG00D | C167482 | RF |
| 7 | BLM15HG102SN1D | C76889 | RF |
| 8 | W5500 | C32843 | Ethernet |
| 9 | B1608S | C507596 | Ethernet |
| 10 | 12.4k 1% 0603 | C22865 | Ethernet |
| 11 | 10 nF 100 V 0603 | C43253 | Ethernet |
| 12 | 1 nF 2 kV 1808 | C81332 | Ethernet |
| 13 | SMAJ58A | C123816 | PoE |
| 14 | EEHZA1E101XP 100 uF 25 V | C264047 | PoE |

- Total: 14 on every board. JLC charges the fee once per unique part per order
  (about $3, JLC FAQ, unverified).
- Not counted: Preferred parts (SMF6.0A, H5VUT2U, C23255), DNP R402, R101-R104, R219, R220, and the hand-soldered
  J1, J301, J203, U401.
- Possible saving: C9196 1206 instead of C81332 1808, at the cost of the 2 mm rule.
- Low-stock items (parts database snapshot): J301 TM24RSG-5A-88 65, DP9900LP-5V 521, E07-900MM10S 649,
  SMA C5723200 1,678, EEHZA1E101XP 2,321, B1608S 4,015, ESP32-S3-MINI-1-N8 7,392.

## 6. Layout rules

### 6.1 RF checklist

The module carries its own match and shield, so the board RF rules cover the ground, the supply and the 50 ohm
feed only (rules RF-M1 to RF-M5; E07 DS 4.1).

- [x] RF-M1: solid In1.Cu GND under U1 and under the feed. In1.Cu has no track and one void only, under the J1
  center pad (rule area `RF_J1_IN1_VOID`).
- [x] RF-M2: F.Cu GND pour under the module body, tented, stitched to In1.Cu with a 3 x 3 via grid. No tracks under U1
  on F.Cu, In1.Cu or In2.Cu: the footprint rule area `RF_MODULE_NO_TRACKS` enforces it. The E22 DIO2 link is the only
  track near U1: B.Cu under the GDO and SPI pads, 7.5 mm from pad 6 (E07 DS 4.1).
- [x] RF-M3: U1 pad 6 -> C191 -> R191 -> L191 -> J1, 0.30 mm grounded coplanar on F.Cu over In1.Cu, straight at
  y 43.54, 7.78 mm of track (3.34 + 4.44 mm). GND via fence on both sides: 8 vias north (largest pitch 1.50 mm),
  11 south (1.55 mm). C191 and L191 sit on the line, each with its own GND via under the GND pad. The only non-GND
  copper within 3 mm of the feed is U1 pads 5 and 8 (NC). The feed tracks are locked.
- [x] Measured gap from the feed to F.Cu GND: 0.45 mm at the U1 land, 0.20-0.38 mm beside the C191, R191 and L191
  pads (their pads widen the line), 0.20 mm elsewhere. In1.Cu lies under the whole run except the last 2.6 mm over
  the J1 void. The F.Cu pour sits 0.20 mm from the J1 centre pad. The run is 0.04 wavelength at 868 MHz: the gap
  steps have no measurable effect.
- [x] RF-M4: C2 at pad 1 (1.68 mm pad gap), C1 2.2 mm from it. L1 sits between the 3V3 branch and C1.
- [x] RF-M5: the SPI and GDO lines leave U1 on the top and left sides, away from pad 6, as one six-line bus on F.Cu.
  R101-R104 sit next to their U1 pads.
- [x] U203 is 32 mm from U1. The +3V3 trunk (B.Cu, y 57.2) stays 10.7 mm from U1 and 13.2 mm from the feed, 5V_SYS
  22.5 mm and VBUS 29 mm from the feed. Only the 7 mm 3V3 branch into L1 comes closer (4.9 mm from U1): RF-M4 needs it.
- [x] Iron ring: every part pad keeps 1.5 mm or more from the U1 lands (nearest R103, 1.52 mm), except C191, which sits
  on the feed by RF-M3 (0.79 mm).
- [x] J1 ground pads: four GND vias in a row outside each ground leg, plus the fence rows between the legs.
- [x] In1.Cu void under the J1 center pad: 2.4 mm x 4.75 mm (pad + 0.6 mm per side, to the board edge), In2.Cu GND
  below. Estimate: the 1.2 x 4.2 mm pad is 0.93 pF and ~23 ohm over In1.Cu, 0.16 pF and ~58 ohm over In2.Cu with the
  F.Cu pour 0.6 mm away (closed form, unverified). The SMA pin lowers it toward 50 ohm.
- [x] Open top areas carry the F.Cu GND pour, stitched to In1.Cu, In2.Cu and B.Cu (section 6.4, GND stitching).

### 6.2 Isolation barrier

| Boundary | Minimum, every layer | Target | Parts that cross | Measured F.Cu / In1.Cu / In2.Cu / B.Cu |
| --- | --- | --- | --- | --- |
| Line side <-> system | 2.0 mm | 3 mm | T301, U401 | 5.08 / 6.29 / 6.29 / 6.29 mm |
| Line side <-> SHIELD | 2.0 mm | 3 mm | C322 | 2.00 / 3.05 / 3.05 / 2.00 mm |
| SHIELD <-> system | 2.0 mm | 3 mm | C323 | 2.88 / 5.10 / 5.10 / 3.20 mm |

- Measured after the zone fill, copper edge to copper edge (pads, tracks, vias and zone fills exported from pcbnew,
  distances with shapely), pads of the crossing parts excluded (they are exempt from each other). The line side is
  the POE_LINE class plus T301 pins 9-16 and U401 pins 5-8.
- The barrier strip x 22-25 has no copper on any layer. Only the C323 pads reach into it (2.6 mm pad gap).
- The SHIELD pour fills up to the 2.0 mm rule, so line side <-> SHIELD sits at the minimum on B.Cu (PAIR_45 track)
  and F.Cu (BS track), below the 3 mm target.
- Rule check: a GND via placed 1.0 mm from T301 pin 11 on a copy of the board raises `iso_line_side` (1.90 mm to the
  CT_36 track).
- The M3 screws are chassis. The SHIELD pour ends 0.25 mm from the H3 hole wall, and the system GND pour ends 0.25 mm
  from H2 and H4. So a metal screw head bonds SHIELD and GND to a metal enclosure and bypasses C323. This is the
  intended bond for a metal enclosure. Where the chassis must float, use a plastic enclosure or insulating washers.

- Line side: J301 contacts, T301 pins 9-16, BR401, BR402, D401, R317-R320, C318-C321, node BS, U401 pins 5-8 and
  the traces between them.
- System side: GND, 3V3, 5V_SYS, T301 pins 1-8, U301, U401 pins 1-4 and everything else.
- SHIELD: J301 shield pins, C322 pad 2, C323 pad 1 and the SHIELD pour on F.Cu and B.Cu (outline in section 6.4),
  stitched with 18 vias of 0.6/0.3 mm (SLUA454 1.4). The pour gives the cable shield a low-inductance path to C323.
  The iso rules set its clearance, so the fill stops 2.0 mm from line-side copper.
- Only T301, U401, C322 and C323 cross the barrier.
- The barrier strip is x 22-25 mm over the full board height (rule area `ISOLATION_BARRIER`, section 6.4). T301 and
  U401 center on x 23.5. The C322/C323 1808 pad gap is 2.6 mm.
- `freedo-radio.kicad_dru` enforces the 2.0 mm minimum on every layer: `iso_line_side` (net class POE_LINE to
  every other class, SHIELD included), `iso_shield_to_system` (SHIELD to the system classes) and
  `iso_line_to_mounting_holes` (3.9 mm from the H1-H4 hole wall, because the screws are chassis). The pads of
  T301, U401, C322, C323 and J301 are exempt from each other.
- Inner planes stop at the barrier. No power or ground plane under T301 (WIZnet note c). The T301 land rows are
  6.4 mm apart inside (B1608S land pattern: 10.40 mm outer span, 2.00 mm pads). The U401 pin rows are
  11.38 mm apart.
- Rule source: 2 mm for 1500 Vrms on FR4 (Microchip AN3468 7). Others: 1.5 mm (TI SLUA454, AN3533),
  3 mm (Silvertel).
- J301 withstands 1500 V AC for 1 minute from the terminals to the shield (TM24R catalog p.2). Its maker land keeps
  1.56 mm from the shield pads 11-14 to the contact pads 1 and 8, with 0.15 mm rings on those pads. That is below the
  2.0 mm board rule and above the 1.5 mm of TI SLUA454. `iso_line_side` exempts the J301 pads from each other.

### 6.3 Stackup, impedance and routing

Stackup: JLCPCB JLC04161H-7628, 4 layers, 1.6 mm, 1 oz outer, 0.5 oz inner, ENIG finish for the fine-pitch parts.
It is the JLC standard stackup: no stackup fee, and it allows Economic PCBA. KiCad holds it in Board Setup > Physical
Stackup.

| Layer | Material | Thickness (mm) | Use |
| --- | --- | --- | --- |
| F.Cu (L1) | 1 oz | 0.035 | parts, RF feed, pairs, GND pour |
| prepreg | 7628 x1, er 4.4 | 0.2104 | |
| In1.Cu (L2) | 0.5 oz | 0.0152 | solid GND. One void under the J1 center pad (section 6.1). Stops at the barrier. |
| core | FR4, er 4.6 | 1.065 | |
| In2.Cu (L3) | 0.5 oz | 0.0152 | solid GND, one zone over the whole system side. Stops at the barrier. |
| prepreg | 7628 x1, er 4.4 | 0.2104 | |
| B.Cu (L4) | 1 oz | 0.035 | routing, GND pour |

Copper and dielectric add up to 1.586 mm, 1.616 mm with the 0.015 mm mask layers. JLC gives 1.59 mm +-10% finished.

Reason for the stackup (JLC quote page, impedance page and PCBA price page, read 2026-09-24):

- The JLC standard stackup has no stackup fee and allows Economic PCBA. Every thicker L1-L2 stackup checked
  is "special" (7628A, 7628B, 7628C, 7628E, 7628F, 7628G, 2116B): no Economic PCBA, about $70 more per order.
- The 0.21 mm prepreg keeps the 50 ohm feed and the 90/100 ohm pairs narrow enough for the 0.5 mm pitch W5500 and
  USB-C pads. On 7628F they would be 0.66 mm (50 ohm), 0.56 mm (90 ohm) and 0.41 mm (100 ohm).
- U1 carries its own match, so the RF path needs only the 50 ohm feed.

Impedance geometry, all on F.Cu over In1.Cu GND (0.21 mm):

| Line | Net class | Track | Gap | JLC calculator | Closed form, no mask |
| --- | --- | --- | --- | --- | --- |
| 50 ohm grounded coplanar, U1 pad 6 -> R191 -> J1 | RF_50 | 0.30 mm | 0.20 mm to the F.Cu GND pour | 0.306 mm for 50 ohm | 47-57 ohm |
| USB D+/D-, 90 ohm differential | USB_90 | 0.29 mm | 0.20 mm | 0.291 mm for 90 ohm | 92-98 ohm |
| Ethernet MDI, 100 ohm differential, both sides of T301 | ETH_100; line side POE_LINE | 0.23 mm | 0.20 mm | 0.226 mm for 100 ohm | 103-109 ohm |

- JLC calculator: jlcpcb.com/pcb-impedance-calculator, JLC04161H-7628, 1 oz outer, solder mask included. It is the
  fab's own model, so its widths are the design values.
- Closed form: Wadell grounded coplanar (with and without the copper thickness correction), IPC-2141 and
  Garg-Bahl coupled microstrip, er 4.4, no solder mask. The mask lowers the impedance by a few percent. The spread
  is within +-10% of the JLC values.
- B.Cu over In2.Cu has the same 0.21 mm dielectric and In2.Cu is solid GND, so a B.Cu track has the same impedance.
  The RF feed and the USB pair stay on F.Cu. The chip-side TX pair runs on B.Cu over In2.Cu for 14.4 mm (TXP) and
  15.3 mm (TXN), coupled over its whole run, with a GND return via 0.8-1.4 mm from each of its four signal vias.

Net classes (`freedo-radio.kicad_pro`) and custom rules (`freedo-radio.kicad_dru`):

| Class | Nets | Track | Clearance | Via |
| --- | --- | --- | --- | --- |
| Default | all others | 0.20 mm | 0.20 mm | 0.45/0.2 mm |
| RF_50 | /rf/RF_ANT (U1 pad 6 to R191), /rf/RF_J1 (R191 to J1) | 0.30 mm, no vias | 0.20 mm | - |
| USB_90 | USB_DP/USB_DN, USB_MCU_DP/USB_MCU_DN | 0.29 mm, gap 0.20 mm | 0.20 mm, pour 0.5 mm | 0.45/0.2 mm |
| ETH_100 | TXP/TXN, TD_P/TD_N, RXP/RXN, RX_TERM_P/RX_TERM_N, RD_P/RD_N | 0.23 mm, gap 0.20 mm | 0.20 mm, pour 0.5 mm | 0.45/0.2 mm |
| POWER | VBUS (both sides of F201), 5V_SYS, +3V3, +VDC, 3V3_RF, 3V3A | 0.65 mm | 0.20 mm | 0.6/0.3 mm |
| POE_LINE | MDI_TX_P/N, MDI_RX_P/N, PAIR_45, PAIR_78, CT_12, CT_36, Bob Smith nets, BS, VIN+, VIN-, T301 pins 12/13 | 0.50 mm | 2.0 mm to other classes | 0.6/0.3 mm |
| SHIELD | SHIELD | 0.50 mm | 2.0 mm to system classes | 0.6/0.3 mm |

- The line-side MDI pairs are POE_LINE nets. The rule `eth_100_pair` gives them the 100 ohm geometry.
- The rules `usb_90_gap` and `eth_100_gap` hold the 0.20 mm gap on the coupled runs. The pairs split only where they
  fan out. The USB pair splits inside the R203, R204, U202 and J201 courtyards and on U201 -> R203/R204. The
  Ethernet pairs split inside the rule areas `ETH_FANOUT`. These areas cover only J301 pins 3/6 with the RX_P length
  bump, the T301 line pins, the R309-R312 pads with the TX vias and the U301 pins 1-6. So the rule checks the runs
  between them. TD, RD and RX_TERM join series parts that sit 1.9-5.7 mm apart and cannot couple, so `eth_100_gap`
  skips them.
- Coupled share at the 0.18-0.22 mm gap, per line: MDI RX 59-60 %, MDI TX 51-52 %, TXP/TXN 75-78 %, RXP/RXN 61-67 %,
  USB 80 %. The rest is the pin and part fan-out.
- The rule `power_rail_width` keeps VBUS, 5V_SYS, +3V3 and +VDC at 0.5 mm or more, except necks into U201, U301 and
  J201 pins inside their courtyards (the J201 VBUS pads sit between the NPTH pegs and the CC/SBU pads). 3V3_RF and
  3V3A neck down to the U1 and W5500 pins.
- POWER and the Bob Smith patterns use the auto-generated names `Net-(F201-Pad1)` and
  `Net-(C318-Pad1)`-`Net-(C321-Pad1)`. A change of these designators breaks the patterns. The RF_50 nets carry
  labels (`RF_ANT`, `RF_J1`).
- Board minimums (Board Setup > Constraints, JLC multilayer): 0.09 mm track and clearance, via 0.45/0.2 mm (0.125 mm
  ring), 0.2 mm via hole to hole, 0.3 mm copper to edge, silk text 1.0 mm tall with a 0.15 mm stroke (JLC legend
  minimum). The custom rules add 0.15 mm ring on plated component holes, 0.45 mm pad hole to hole, 0.5 mm NPTH,
  0.25 mm hole to other-net copper (0.2 mm inside the J201 land) and `via_not_in_smd_pad`: 0.075 mm from a via to
  any SMD pad of any net, so no via reaches into a pad or its 0.05 mm mask opening (0.071 mm at a square corner).
  A via in the C401 pad, and a via 0.04 mm outside it, each raise the rule on a copy of the board.
- `no_paste_on_dnp` gives the pads of every DNP footprint a -10 mm paste margin. KiCad clamps the aperture to zero
  size and does not plot it, so the stencil has no opening on C191, R101-R104, R219, R220 and R402. The rule reads
  the footprint DNP attribute, which comes from the schematic.

Routing:

- USB D+/D-: 90 ohm +-10% differential, parallel, equal length. Continuous ground reference. Fewest vias, with a
  ground via pair at each transition. Ground on both sides of the pair (HDG 1.4.8), 0.5 mm from the pair.
  As routed: F.Cu only, no via, 52.49 mm per line from U201 pads 23/24 to J201, intra-pair skew 0.00 mm. In1.Cu
  under the pair is solid.
- Ethernet MDI: 100 ohm differential, equal length inside each pair, W5500 <-> T301 and T301 <-> J301
  (WIZnet note d). Keep the W5500 close to T301. As routed, the intra-pair skew from pad centre to pad centre is:
  chip side RX 0.02 mm, TX 0.01 mm; line side RX 0.06 mm, TX 0.00 mm (limit 0.5 mm). A meander on TD_N, a bump on
  MDI_RX_P in the J301 pin field and a dip of MDI_TX_P into pin 16 carry the length. All sit outside the coupled runs.
- TOCAP and EXRES1 traces short. Y301 next to U301.
- 3V3 main trace >= 25 mil (HDG 1.4.2): POWER is 0.65 mm. U203 tab copper as in section 4.4.

### 6.4 Placement

Board: 100 x 75 mm, 1 mm corner radius, 4 x M3 NPTH (3.2 mm, H1-H4, board-only footprints) 4 mm in from each corner.
Coordinates: origin at the top-left corner of the outline, x to the right, y down, mm. The KiCad grid origin and
drill/place origin sit on that corner. The regions, the U203 copper areas and the planned routing channels are drawn on
User.Comments. The generated placement maps show them.

Size reason:

- Short side 75 mm: the extra 5 mm gives the header and LED region above the Ethernet row its height.
- Assembly route: Economic PCBA needs no rails. Standard PCBA needs edge rails and fiducials (JLC "How to Add Edge
  Rails and Fiducials"). The board carries three global fiducials (1.0 mm copper, 2.0 mm mask opening, no silk):
  FID1 (10.5, 3.5), FID2 (94, 20.5), FID3 (90.5, 71.5). The rails are an order-time choice. The top edge cannot take
  a rail under the antenna overhang, so rails go on the two 75 mm edges. Nearest SMD courtyard per edge, the
  overhanging U201, J1 and J201 aside: top 0.83 mm (C201), bottom 0.83 mm (R402, DNP; C323 1.06 mm), left 3.82 mm
  (R319, C320), right 4.69 mm (L191).
- Long side 100 mm: line side, barrier, T301, the MDI parts, U301, U201, the radio and the feed sit in one row with
  pad gaps of 1.0 mm or more between passives.
- Courtyards cover ~2,570 mm2 of the 7,500 mm2 board (~34%). The rest holds the barrier, the U203 copper, the RF
  stitching and the routing channels.

| Edge | Part | Position |
| --- | --- | --- |
| Top | U201 antenna, overhanging | module center (65, 5.25), rotation 0. The board edge is the module line: the antenna (15.4 x 5.0 mm) is off the board, pads 46-60 end 0.4 mm inside. |
| Top | SW201 RESET, SW202 BOOT | side by side right of U201, rotation 180, centers (80.6, 5.1) and (88.7, 5.1): the bodies end 2.55 mm from the top edge (JLC conveyor edge 2.5 mm). The EN and BOOT pads form the north row, the GND pads the south row, 1.1 mm pad gap between the switches. The silk labels RESET and BOOT sit above the switches. SW201 is next to pad 45, so EN stays short (HDG 1.3.3). The switches sit on the board, outside the U201 antenna keep-out (y < 0). SW202 ends 0.1 mm from the H2 screw-head courtyard. |
| Top | J203 GPIO header | rotation -90, pin 1 (3V3) at (49.8, 2.9), pin 10 (GND) at (26.94, 2.9). Its courtyard ends 0.14 mm from the barrier strip, 26 mm from the RF feed. |
| Right | J1 edge SMA | center (97.85, 43.54), rotation 90. The pads end 0.05 mm inside the Edge.Cuts center line: `j1_edge_launch` allows 0 mm, and DRC counts the 0.05 mm edge line. The flange sits on the edge, the body is off the board. JLC asks for 0.2 mm copper to a routed edge: confirm the J1 pads at the DFM query (hand-soldered edge launch). |
| Right | TP211-TP213 | UART group at x 97.4, 2.54 mm pitch, top to bottom TP212 RX, TP211 TX, TP213 GND (y 9.5-14.58). The reference sits next to each pad, the net name beyond it after a 1 mm gap. |
| Inner | TP101-TP107 | radio group beside the bus fan-out, 2.54 mm pitch: TP106 GDO2, TP105 GDO0, TP101 SCK, TP104 CSN at x 73.3 (y 25.28-32.9), west of the GDO lines; TP107 GND, TP102 MOSI, TP103 MISO at x 89.0 (y 26.92-32.0), east of E22_BUSY. Stubs 4.7-9.4 mm, one via or none. |
| Left | J301 RJ45, mouth outward | center (7.06, 26), rotation -90. The board edge is 2.6 mm in front of the front pin row; the jack face overhangs the edge by 2.4 mm (TM24R catalog p.3; 2.34 mm in the 3D model). Keep the two catalog no-trace areas at the front shell holes (footprint keep-outs). |
| Bottom | J201 USB-C, mouth outward | center (65.425, 70.23), rotation 0: its D+/D- pads sit on the vertical line through U201 pads 23/24. The shell front face is 0.30 mm past the edge (front shell legs at y 72.70, face 2.60 mm ahead of them per the HRO drawing), so a plug overmold seats on the shell, not on the board edge. |
| Bottom | TP250, TP251, TP252, TP301 | y 72.5, x 77.0-84.62, 2.54 mm pitch: 5V_SYS, 3V3, GND, GND. |

| Region | x | y | Contents |
| --- | --- | --- | --- |
| Line side | 0-22 | 0-75 | J301 contacts; T301 pins 9-16 (x 19.3); Bob Smith columns at x 4.6-13.6, 3.0 mm pitch, in the B.Cu lane order PAIR_45, PAIR_78, CT_12, CT_36 (R319/C320, R320/C321, R317/C318, R318/C319), resistors at y 39.8 above the capacitors at y 43.4; BR402 (13, 51.5), D401 (12.5, 58.5) and BR401 (13, 64.8) beside U401 pins 5-8 (x 17.81); C322 (6, 63.8) with pad 2 toward the SHIELD strip. The J301 line-side pairs meet T301 in the same order (RX above TX), so they need no layer change. |
| SHIELD | 0-2, 0-22 | 31.6-75, 71.4-75 | J301 bottom flank (pins 10, 13, 14), a 2 mm strip along the left edge and a strip along the bottom edge to C323 (23.5, 72.5), with a finger to C322 pad 2. The J301 top flank (pins 9, 11, 12, y 11-20) is a second SHIELD area, joined through the jack shell. Pour on F.Cu and B.Cu. |
| Barrier | 22-25 | 0-75 | rule area `ISOLATION_BARRIER`: no track, via or pour on any copper layer. T301 (23.5, 26), U401 (23.5, 53) and C323 straddle it: pins 1-8 of T301 and 1-4 of U401 face +x. |
| Header, LEDs | 25-57 | 0-17 | J203 on the top edge. R212, R213, R214, R211 (y 8, signal pad up) above D204, D205, D206, D203 (y 11.8, anode up) at x 40-49, 3.0 mm pitch: the power LED D203 at the east end. R218 (29.48, 7.1), under J203 pin 9 with room for the pin label. R220 and R219 (46.4) on y 15.8. C207 and C206 (x 55.6 and 53, y 2.6) at U201 pad 3. |
| Ethernet, system side | 25-61 | 17-41 | U301 (46.5, 26), rotation -90: the MDI pins face T301, the SPI pins, XI/XO and VDD face U201. MDI parts between T301 and U301: the RX parts in columns x 31.2 and 35.0 from y 18.6 to 25.3 (C316/R312 at y 21.35, 1.0 mm pad gap to C315/R311); the TX ladder R305/R310 (y 28.7) and R304/R309 (y 32.8) at x 34.15/36.1, with R306 (32.2, 30) and C314 (30.1, 30). U301 pin shunts: C305 (40.35, 23.4, pin 4), C306 (39, 26.6, pin 8), R303 (39, 28.6, pin 10), C307 (39, 30.6, pin 11). Decoupling fan under the bottom pins, 1.95 mm pitch from x 42.9, y 32.98-33.95: C308 (pin 15), C309 (17), C303 (TOCAP 20), C310 (21), C304 (1V2O 22). C311 (3V3A bulk) at (39.2, 33). R313 (48.4, 19.5) on the RST lane, R314 (52.9, 22.1) on the INT lane, R208 (59.9, 17.6) in the SCLK lane. Right of U301: C313 (53, 28) at pin 28 with C312 (55, 29.3), R301 (57.2, 30.7, upright between XO and XI, 1.25 mm from C312) and R302 on XI/XO, C302 and Y301 (59, 34.3), C301 (55.9, 37.2). FB301 (37, 38.4) and the LINK/ACT row D301, R315, R316, D302 (y 40.3, x 46.4-57.6) under U301. |
| MCU | 57-73 | 0-16 | U201. The W5500 pads 13-18 sit at the bottom-left corner, the USB pads 23/24 at the bottom center, the E22 pads 25/27/30 at the bottom right, the radio pads 33-38 in the lower right column. |
| EN, RESET, BOOT, UART | 73-94 | 0-13 | C201/R205 (x 74.9, y 1.6 and 3.8) at pad 45, SW201 RESET (80.6, 5.1), SW202 BOOT (88.7, 5.1), R206 (88.7, 8.45) under SW202, R207 (75.3, 6.95) at pad 39, R209 at pad 35. |
| Radio | 70-96 | 25-55 | U1 (84, 41), rotation 90: SPI pads on top, GDO pads on the left, ANT pad 6 on the right side facing J1. Feed: pad 6 -> C191 (91.0) -> R191 (92.9) -> L191 (94.8) -> J1, straight at y 43.54, 5.85 mm from pad 6 to the J1 pad. C2 and C1 under pad 1 (pad gaps 1.7 and 2.1 mm), L1 next to C1, R101 under pad 3, R102 (93, 35.6) and R104 (93, 38) right of pads 10 and 9, R103 (86.54, 32.35) above pad 11, R210 (75.1, 34.5) on the CSN lane west of the bus. The radio test pads TP101-TP107 flank the bus fan-out (see the edge table). A 1.5 mm ring around the U1 lands stays clear. |
| Power, USB | 25-70 | 41-75 | U203 (45, 56.2), rotation -90: tab up into the 3V3 copper area, pins down. C204 and C205 at the tab, C203 and C202 under the IN pin. U401 pins 1-4 with R401, C402, D402 (38.6, 62.4), C401 (43.5, 70.2, away from U203 and U401) and R402 (31, 72.2). R203/R204 (x 64.175 and 66.175, y 19.6, 1.05 mm pad gap) at pads 23/24, U202 (64.925, 61) on the pair, R201/R202 beside U202, D202 (57.8, 60), the VBUS row F201, D201, C208, R215 (y 70.8), R216 (62.4, 55.6) and C209 (61.6, 22.0), R217 (61.6, 24.0) on the VBUS_SENSE line from pad 21. |

U203 copper (section 4.4): zones `U203_3V3_F` (x 32.6-58.8, y 41-57.6) and `U203_3V3_B` (x 34-56, y 42.4-57.4).
Filled copper: 4.26 cm2 on F.Cu and 3.28 cm2 on B.Cu, joined by 16 +3V3 vias of 0.6/0.3 mm around the tab. The tab
and the OUT pin connect solid. Only U203, C204, C205 and their GND vias sit inside. U203 is 32 mm from U1. The zones and
their vias are locked.

Decoupling at the pins: every capacitor sits on F.Cu next to its pin. A 100 nF or smaller part keeps at most
2.0 mm pad to pad and has room for its own GND via. A bulk part (1 uF or more) keeps at most 5 mm. The pad gaps come
from the board, pad edge to pad edge. The W5500 pin groups follow W5500-Ref-ExternalMag (C1-C6 at the AVDD pins,
C7 10 uF on 3V3A, C11/C12 at VDD, C17 at 1V2O, C18 at TOCAP).

| IC | Pin | Net | Capacitor | Gap mm |
| --- | --- | --- | --- | --- |
| U301 | 4 AVDD | 3V3A | C305 100 nF | 0.73 |
| U301 | 8 AVDD | 3V3A | C306 100 nF | 1.28 |
| U301 | 11 AVDD | 3V3A | C307 100 nF | 2.30 |
| U301 | 15 AVDD | 3V3A | C308 100 nF | 1.87 |
| U301 | 17 AVDD | 3V3A | C309 100 nF | 1.33 |
| U301 | 21 AVDD | 3V3A | C310 100 nF | 1.85 |
| U301 | 11 AVDD (group) | 3V3A | C311 10 uF | 4.36 |
| U301 | 20 TOCAP | TOCAP | C303 4.7 uF | 0.77 |
| U301 | 22 1V2O | 1V2O | C304 10 nF | 2.67 |
| U301 | 28 VDD | +3V3 | C313 100 nF, C312 10 uF | 1.03, 3.18 |
| U201 | 3 | +3V3 | C207 100 nF, C206 22 uF | 1.53, 4.13 |
| U1 | 1 VCC | 3V3_RF | C2 100 nF, C1 10 uF | 1.68, 2.20 |
| U203 | 3 IN | 5V_SYS | C203 100 nF, C202 10 uF | 1.31, 1.99 |
| U203 | 2/4 OUT | +3V3 | C205 100 nF, C204 10 uF | 1.76, 1.83 |
| U401 | 1/2 +VDC | +VDC | C402 10 uF, C401 100 uF | 2.39, 10.98 |

- U202 (H5VUT2U) has no supply pin.
- C401 stays 10.9 mm from U401 pin 1, away from U401 and U203 for its life (review E13, section 5.4). C402 10 uF
  decouples the pin at 2.4 mm.
- C307 (2.30 mm) and C304 (2.67 mm) exceed the 2.0 mm target: the 1.0 mm rework gaps to R303 and C310 set their
  positions in the 0.5 mm pitch fan-out. Each keeps its own GND via and a straight 45 degree track to its pin.
- Every capacitor has its own GND via within 1.6 mm of its GND pad.
- U301 AVDD: the supply reaches each AVDD pin through its capacitor pad. FB301 -> C311 -> C307 -> pin 11 and
  C311 -> C308 -> pin 15 on F.Cu; from C311 a 3V3A trunk on B.Cu feeds one via beside C305, C306, C309 and C310, each
  via -> capacitor pad -> pin. No 3V3A copper sits inside the pin ring: the U301 interior is GND pour.
- Between passives every pad gap is 1.0 mm or more, except the RF feed (C191, R191, L191, J1: 0.63-0.81 mm, section
  6.1).
- The U201 lands end 0.3 mm inside the module body (LGA). An iron reaches no U201 pad: U201 rework is hot air only.

Routing (the channels on User.Comments, layer in each label):

- USB pair: straight and vertical, centred on x 64.925, from pads 23/24 through R203/R204 and U202 to J201, F.Cu, no
  via, 52.5 mm. At J201 the B7 pad joins A7 in a loop south of the pads and A6 joins B6 over them, 45 degree corners.
- W5500 SPI: six lanes on F.Cu, no crossing, 14-16 mm, from pads 13-18 to U301 pins 37-32. From the outer side: RST,
  INT, MOSI, MISO, SCLK, CS. RST passes R313 pad 2, INT passes R314 pad 2, SCLK runs through R208. R313 and R314 reach
  3V3 through a via.
- Radio SPI/GDO: six lanes on F.Cu, no crossing, 32-39 mm, from pads 32-38 right to x 76-83.5, then down. From the
  west: GDO2 and GDO0 to the U1 left pads 20 and 18, SCLK (through R209), CSn (R210 pad 2), MOSI and MISO to the top
  pads 15-12. R210 pad 1 (3V3_RF) takes a via.
- E22 links: BUSY and RXEN from pads 30/27 cross the radio bus on B.Cu (y 16.9 and 16.1), then run on F.Cu to R103
  and, along the right edge, to R102. DIO2 runs on B.Cu from U1 pad 19 to R104. NRST from pad 25 runs on F.Cu west of
  the bus and cuts the corner above L1 to R101 (49 mm, GPIO21 stays an input).
- EN: pad 45 -> C201, R205 -> the SW201 north pads, F.Cu, ~10 mm. GPIO0: via at pad 4, B.Cu under U201 at y 3.65,
  0.3 mm from the +3V3 trunk so no pour sliver forms between them, to a via between SW201 and SW202, F.Cu to the
  SW202 north pads and under the SW202 body to R206.
- MDI: RX pair on F.Cu from T301 pins 6/8 to U301 pins 5/6; TX pair on B.Cu from U301 pins 1/2 to T301 pins 1/3,
  because TX is above RX on U301 and below RX on T301. Line-side pairs on F.Cu from J301 to T301: the RX pair
  leaves the J301 pin field coupled through the gap between pins 5 and 4, 2 mm clear of the shield pins. The CT and pair
  lines run down the line side on B.Cu to the Bob Smith columns and the bridges.
- 5V_SYS: D402 and D202 to the U203 IN pin along the bottom.
- 3V3: the F.Cu pour from U203 up to FB301 and U301; a B.Cu trunk north to U201 pad 3; a B.Cu trunk east under the
  USB pair to L1.
- VBUS_SENSE: x 61.4, from R216 up to R217/C209.
- Header: IO1/IO2/IO7/IO8 on F.Cu in the header region, IO7/IO8 around the LED block. SDA/SCL to J203 on B.Cu.
  UART TX/RX to J203 on B.Cu: under U201, then along the top edge at y 1.55 and 1.1 with a GND via row at y 0.525
  between them and the edge. UART to TP211/TP212 on F.Cu under the SW201 body, then down between the switches.
- Radio test pads: GDO2 takes an F.Cu stub (6.7 mm, no via) from the west side of its lane. GDO0, SCK, CSN, MOSI and
  MISO take one tap via on the bus fan-out and a B.Cu stub of 4.7-9.4 mm to their plated pads.

Layer use as routed (hand-placed geometry, 0/45/90 degree segments; no autorouter output):

- F.Cu: all buses, the RF feed, the USB and Ethernet pairs, the power trunks and the GND pour. 562 segments, 1541 mm.
  Both SPI buses run as one highway each: 0.2 mm tracks at one pitch, 45 degree bends.
- In1.Cu: solid GND, no tracks. It is the reference for every F.Cu signal.
- In2.Cu: solid GND, no tracks.
- B.Cu: GND pour with short crossings. 130 segments, 601.5 mm: 164 mm on the line side (CT and pair lines, no GND
  pour there) and 437.5 mm on the system side. The longest system-side crossing, and so the longest slot in the B.Cu
  pour, is UART0_TX: 50.6 mm (x 32.0-76.3, y 1.5-7.0), with UART0_RX (47.1 mm) beside it. No B.Cu track runs under a B.Cu signal, so no slot sits under a
  fast signal: those run on F.Cu over In1.Cu, or on B.Cu over In2.Cu (TX pair). The B.Cu GND pour stays one fragment.
- Vias: 648, all tented. No via comes within 0.075 mm of an SMD pad, so none sits in a pad or its mask opening
  (`via_not_in_smd_pad`). The nearest are the eight U201 EPAD vias in the gaps between the paste squares (MINI-1 DS
  Fig 11-1), 0.093 mm from the square corners. 592 of 0.45/0.2 mm, 56 of 0.6/0.3 mm (power), 562 on GND.
- GND stitching: rows at 2 mm pitch along the board edges and along the system side of the barrier (x 25.7), a
  2.5 mm grid in open areas. Every F.Cu GND fragment (10) has 2 vias or more to In1.Cu/In2.Cu; the B.Cu, In1.Cu and
  In2.Cu GND pours are one fragment each. The long pour fingers carry their own vias: the strip between the UART
  test-pad lines (3), the LED columns (one more each) and the TD_N meander (2). GND pads use thermal reliefs,
  except the U1, C191, L191 and J1 grounds (solid, RF). The U203 tab is +3V3 and connects solid.
- Locked: the RF feed, the USB pair, the Ethernet pairs on both sides of T301, the U203 zones and their vias.

Silkscreen:

- Every text is 1.0 mm tall or more with a 0.15 mm stroke, the JLC legend minimum (169 texts, 1.0-1.5 mm).
- Every silk line, arc, circle and polygon has a 0.15 mm stroke or more, the JLC minimum line width. Filled marks
  keep their printed size: the stroke grows and the shape shrinks by the same amount.
- Every reference designator is visible, off every pad, mask opening and via, and off the bodies of other parts. It
  sits nearer its own part than any other. Where no free spot next to the part exists, a 0.15 mm leader line of
  0.3-2.2 mm points to the part: C201, C307, C308, C309, C315, D205, D206, H2, R303, R306, R307, R310, R312, SW202.
- Labels: "freedo radio", "rev A 2026-09", RESET and BOOT above the switches, USB, "ETH / PoE", "868 MHz", the J203
  pin names under the pins (3V3, SDA, SCL, IO1, IO2, IO7, IO8, TX, RX, GND) and a net label next to each test point.
- Pin-1 and polarity marks come from the footprints (ICs, modules, diodes, LEDs, bridges, C401, J203, T301, U401).
- U401 marks its orientation, because the DP9900 pins are 180 degree symmetric (DS 4.2: rows 1.46/1.36 mm from the
  body edges, well inside the hole play). The outline has a chamfered pin-1 corner. An arrow points at pin 1. The DS
  pin names (+VDC, -VDC, ADJ, VIN+, VIN-) stand next to the pins. CHECK ORIENTATION runs along the body. If the
  module is reversed, it puts 36-57 V on +VDC and 5V_SYS (section 8.2).
- DRC: 0 silk over copper, 0 silk overlap. The 8 silk-to-edge warnings are the intended overhangs of the U201
  antenna, J1, J201 and J301 outlines.

- The antenna overhangs the edge (HDG 1.4.7 Fig 24/25). The module keep-out areas lie off the board.
  Dense ground vias near the antenna end of the module. 15 mm clearance to the housing in all directions (HDG 1.4.7).
- U201 sits on the top edge, not in a corner (HDG 1.4.7 Fig 25). Accepted: Wi-Fi is not in the transport plan (USB
  and Ethernet). J203 pin 1 sits 7.5 mm from the module side when the header is fitted. Move the module to a corner
  if a later revision uses Wi-Fi.
- The MINI-1 puts the USB pads between the W5500 pads and the radio pads, on the side away from the antenna. So the
  blocks run Ethernet/PoE -> MCU -> radio along x, with USB and power under the MCU. J201 sits on the edge opposite
  the antenna, so the pair is straight but 53 mm long.
- The radio sits at the far end from U401 (62 mm), U203 (42 mm) and Y301 (28 mm). U1 is 40 mm from U301.
- Both SPI buses are planar (section 3): the lanes on User.Comments keep 0.2 mm from every other-net pad at 0.2 mm
  track width.
- The 1808 pads of C323 reach 0.2 mm into the 3 mm strip (2.6 mm pad gap, above the 2.0 mm minimum).
- The antenna overhangs the board edge by 5.0 mm. Check at order time whether JLC asks for rails because of it
  (unverified).

## 7. Firmware and driver changes

1. Pins: radio MISO 42, MOSI 41, SCLK 39, CSn 40, GDO0 37, GDO2 36 (today 21, 23, 18, 5, 25, 26 in
   `firmware/cc1101_bridge/cc1101_bridge.ino`). LEDs 4, 5, 6. W5500 RST 9, INT 10, MOSI 11, MISO 12, SCLK 13, CS 14. GPIO38 is spare:
   leave it an input.
   VBUS_SENSE 17 (input). J203: I2C SDA 15, SCL 16, IO1, IO2, IO7, IO8. E22 option: NRST 21, RXEN 47, BUSY 48;
   keep them as inputs while the E07 is fitted.
2. SPI hosts are a firmware choice. Both buses go through the GPIO matrix: the W5500 pins (MISO 12, SCLK 13,
   CS 14) are not the SPI2 IO_MUX set (CS0 10, SCLK 12, MISO 13). The design allows both host orders:
   - W5500 on SPI2 (FSPI), CC1101 on `SPIClass(HSPI)` (SPI3).
   - Or the CC1101 keeps the global `SPI` (FSPI, SPI2) and the W5500 gets `SPI3_HOST` through
     `ETH.begin(ETH_PHY_W5500, ...)`.
   - SPI clock limit: at 40 MHz or lower, SPI pins routed through the GPIO matrix behave as IO_MUX pins (ESP-IDF v6.1
     spi_master for the ESP32-S3, section "GPIO Matrix and IO_MUX"), so full duplex works up to 40 MHz on both buses.
     The W5500 is guaranteed to 33.3 MHz. 80 MHz is its theoretical design speed (W5500 DS 5.5.4, p.61, note 5).
     The S3 SPI clock is 80 MHz / n, so set the W5500 to 26.67 MHz or less (`SPI_MASTER_FREQ_26M`, 80 / 3).
     arduino-esp32 defaults to 20 MHz. CC1101 runs at 1/6 MHz.
3. Configure GPIO36 and GPIO37 as GPIO inputs explicitly (eFuse-dependent default, section 3).
4. USB: Arduino "USB Mode: Hardware CDC and JTAG", "USB CDC On Boot: Enabled". `Serial` is the HWCDC. The
   baud rate has no effect. Debug logs go to `Serial0` (UART0, GPIO43/44), never to the protocol port.
   Writes time out when no host reads. After a reset, the ROM prints to USB. The host drain removes it.
5. Host `cc1101/bridge.go`: remove the 2 s wait after open. Opening the port does not reset the S3
   (Linux cdc-acm sets DTR and RTS together; TRM v1.8 Table 33.3-2: RTS=1 DTR=1 is no action). Keep the 0x00
   write and the drain. Never set RTS=1 with DTR=0: that resets the chip. Use the `/dev/serial/by-id/` path.
   Not tested on hardware.
6. PATABLE: `cc1101.go` writes `paTable0 = 0xC0`. The E07 gives 9-11 dBm at its maximum setting (E07 DS 2.1);
   its match is inside the module. The limit in 868.0-868.6 MHz is 25 mW e.r.p. = +14 dBm (EN 300 220-2 V3.2.1
   Annex B Table B.1, review E12). The antenna gain, net of cable loss, must stay at or below
   14 - 11 + 2.15 = 5.15 dBi at the maximum setting. Measure the conducted power per PATABLE value at bring-up.
   Duty cycle (1% in bands L and M) stays a firmware item.
7. FSCTRL0: `tadoRegs` sets 0x40 and `curFSCTRL0` starts at 0x40. That is the crystal offset of the old module.
   The E07 crystal is +-10 ppm (E07 DS 2.3). Re-run `freedo-dev autotune` and store a new calibration.
8. W5500 transport: bring up the W5500 with `ETH_PHY_W5500` (arduino-esp32 >= 3.0.0), wire INTn, and serve the
   bridge byte protocol over TCP. The host driver needs a TCP transport next to the serial one.
9. W5500 reset: RSTn low >= 500 us, then wait for PLL lock (<= 1 ms) before SPI access (W5500 DS 5.5.1).
10. PoE boards: no ESP32 deep sleep and no W5500 power-down. The DP9900 needs >= 100 mA load.
11. D+ pull-up (review E03): the USB-Serial-JTAG controller enables the D+ pull-up at reset (HDG Table 9). Early in
    `setup()`, set USB_SERIAL_JTAG_PAD_PULL_OVERRIDE = 1 and DP_PULLUP = 0 while VBUS_SENSE (GPIO17) is low.
    Release the override when VBUS_SENSE goes high, and apply it again when it goes low. USB 2.0 7.2.1: a device
    must not power the D+ pull-up without VBUS. Test on a PoE-only board with a Type-C host cabled.
12. IOCFG0 early (review E17): write IOCFG0 (0x07) right after reset. Until then GDO0 drives CLK_XOSC/192 into
    GPIO37 (SWRS061I 19.1).
13. E22-900MM22S option: an SX1262 backend in `cc1101/` (command interface with BUSY, 256-byte buffer, CRC
    polynomial 0x8005 seed 0xFFFF). The SX1262 has no GFSK frequency-error readout for the autotune. Measure the
    CSL and ACK timing again. Limit the power to +14 dBm e.r.p.

## 8. Open items and bring-up checks

### 8.1 Open items

1. E07 RF: the DS gives no harmonic, spur or 50 kBaud sensitivity figures and no certificate for the MM10S. Measure
   them on the first board (section 8.2). If the harmonics are high, fit C191 and replace R191 with a series
   inductor as a low-pass.
2. 3V3 capacitance: 63 uF nominal lies inside the TLV761 1-220 uF range. The 2 mohm ESR floor depends on the board
   copper between the MLCCs. Check 3V3 for ringing on a load step.
3. USB attach: the VBUS side is ~7.7 uF effective (section 4.4, typical DC bias, unverified). Measure the attach
   current with a current probe if a hub port droops.
4. USB-only peak: 0.53 A with a Wi-Fi TX burst is above 500 mA (USB 2.0 port, unverified). Limit Wi-Fi TX power
   or keep Wi-Fi off on USB-only boards if a host port trips.
5. DP9900 adjusted output tolerance and IEEE class: not in the datasheet.
6. E22-900MM22S option: not tested. It needs R101-R104, 0R in place of L1 and an SX1262 driver.
7. Bob Smith capacitor rating: 100 V (SLUA454) vs 200 V (SLVA233A) if a surge test applies.
8. W5500 RX termination: 82R follows the ExternalMag reference. No document explains the 49.9R of the magjack
   reference.
9. Y301 crystal stray (3 pF) is an estimate. The bring-up tunes it.

### 8.2 Bring-up checks

- [ ] Before the first power-up with PoE: check the U401 orientation. Pin 1 (+VDC) sits at the silk arrow and the
  chamfered corner, the VIN+/VIN- pins face the line side. A reversed module puts 36-57 V on +VDC and 5V_SYS.
- [ ] Power, USB only: 5V_SYS, 3V3, U203 temperature.
- [ ] Power, PoE only: 5V_SYS ~5.26 V, no audible noise from U401, the PSE keeps power for 1 h.
- [ ] Power, PoE and USB: PoE carries the load (compare the D402 and D202 forward drops). Unplug USB, then plug
  it back, then unplug PoE: no ESP32 reset (check the reset reason).
- [ ] PoE power-up: measure the 3V3 and EN ramps. If 3V3 rises slower than the EN RC (10 ms), add a supervisor
  on EN (Espressif HDG 1.3.3).
- [ ] USB unplugged with PoE on: VBUS at J201 stays below 0.8 V, VBUS_SENSE reads low, D+ stays low (item 11 of
  section 7). Repeat cold and warm.
- [ ] VBUS_SENSE from a lab supply on J201: high at 4.40 V, pin below 3.4 V at 5.5 V.
- [ ] U203 temperature on PoE at steady state: Tj = TB + 33.4 C/W x P (section 4.4).
- [ ] 3V3 ringing on a load step and on a Wi-Fi TX burst (open item 2).
- [ ] DTR/RTS: open and close the port with the Go host. `dmesg` shows no re-enumeration, the uptime keeps
  counting.
- [ ] RF: conducted TX power and spectrum through an attenuator into an SDR, per PATABLE value (0xC0, 0xC5 and
  lower). Check the 2nd and 3rd harmonics and the spurs.
- [ ] RSSI, packet loss and sensitivity against the old module: same devices, same positions, side by side for
  several hours.
- [ ] `freedo-dev autotune` in receive only. Store the new FSCTRL0 calibration.
- [ ] Ethernet: 100M full-duplex auto-negotiation with a switch, link and activity LEDs, ping loss over a long
  cable. J301 pin 1 to pin 1 with a patch cable.

### 8.3 Accepted review items

The electrical review lists findings E01-E20. The review file is not in the repository. The design fixes E01-E05, E07, E09, E10
and E13. E08 (699 MHz spur of the chip-down CC1101) does not apply to the module: section 8.2 measures its spurs.
E12 and E17 are firmware items (section 7). These stay as accepted:

| ID | Finding | Reason to accept |
| --- | --- | --- |
| E06 | The board draws ~0.27 A from power-up and 0.53 A in Wi-Fi bursts; USB 2.0 allows 100 mA before configuration. | Same class as the ESP32-S3-DevKitC-1. Wi-Fi stays off on USB-only boards. PoE boards do not load VBUS. |
| E11 | The DP9900 minimum load (100 mA) can fail with USB and PoE together at a low DP9900 corner, or with EN held low. | No damage: audible noise and possible PoE cycling only. Fit R402 on boards that run on USB and PoE together. |
| E14 | F201 hold current derates to 0.41 A at 70 C and 0.34 A at 85 C. | Without Wi-Fi the board draws 0.27-0.29 A: 17 % margin at 85 C local. Keep F201 away from U203. |

## 9. Sources

The datasheets are not stored in the repository. Fetch them from the links below.

- Ebyte E07-M series DS v1.0 (E07-900MM10S): https://www.lcsc.com/datasheet/lcsc_datasheet_2410091512_Chengdu-Ebyte-Elec-Tech-E07-900MM10S_C5844212.pdf
- Ebyte E22-900MM22S user manual: https://wmsc.lcsc.com/wmsc/upload/file/pdf/v2/lcsc/2212192008_Chengdu-Ebyte-Elec-Tech-E22-900MM22S_C5333985.pdf
- Semtech SX1262 datasheet (NRESET pull-up, Table 8-3)
- TI SWRS061I, CC1101 datasheet: https://www.ti.com/lit/ds/symlink/cc1101.pdf
- Murata LQW15AN_00 (LQW15AN56NG00D): https://www.lcsc.com/datasheet/lcsc_datasheet_2304140030_Murata-Electronics-LQW15AN56NG00D_C167482.pdf
- TI SWRA122D, DN005 sensitivity vs frequency offset: https://www.ti.com/lit/pdf/swra122
- TI SWRZ020E, CC1101 errata: https://www.ti.com/lit/pdf/swrz020
- YXC X322525MOB4SI: https://www.lcsc.com/datasheet/lcsc_datasheet_2403291504_YXC-Crystal-Oscillators-X322525MOB4SI_C9006.pdf
- XUNPU SMA-KE-347-H13.5-1.6: https://wmsc.lcsc.com/wmsc/upload/file/pdf/v2/lcsc/2304281213_XUNPU-SMA-KE-347-H13-5-1-6_C5723200.pdf
- Espressif ESP32-S3-MINI-1 & MINI-1U datasheet v1.7: https://www.espressif.com/sites/default/files/documentation/esp32-s3-mini-1_mini-1u_datasheet_en.pdf
- Espressif ESP32-S3 series datasheet v2.2: https://www.espressif.com/sites/default/files/documentation/esp32-s3_datasheet_en.pdf
- Espressif ESP32-S3 hardware design guidelines: https://docs.espressif.com/projects/esp-hardware-design-guidelines/en/latest/esp32s3/
- Espressif ESP32-S3 technical reference manual v1.8, chapter 33: https://www.espressif.com/sites/default/files/documentation/esp32-s3_technical_reference_manual_en.pdf
- ESP-IDF SPI master, GPIO matrix and IO_MUX: https://docs.espressif.com/projects/esp-idf/en/latest/esp32s3/api-reference/peripherals/spi_master.html
- ESP32-S3-DevKitC-1 v1.1 schematic: https://dl.espressif.com/dl/schematics/SCH_ESP32-S3-DevKitC-1_V1.1_20221130.pdf
- WIZnet W5500 datasheet v1.1.0: https://docs.wiznet.io/assets/files/W5500_ds_v110e-226ffec190c588b69f88d629789585e1.pdf
- WIZnet W5500-Ref-ExternalMag schematic: https://docs.wiznet.io/assets/files/w5500_sch_v110_use_trans_-18b3297138bdf20fafd0f95e4c19358d.pdf
- WIZnet W5500-Ref-RJ45WithMag schematic (docs.wiznet.io, W5500 reference schematics)
- CND-tek B1608S: https://wmsc.lcsc.com/wmsc/upload/file/pdf/v2/lcsc/2405301629_CND-tek-B1608S_C507596.pdf
- Hirose TM24R series catalog (TM24RSG-5A-88): https://www.lcsc.com/datasheet/lcsc_datasheet_2407051755_HRS-Hirose-TM24RSG-5A-88_C6285250.pdf
- Panasonic ZA series (EEH-ZA1E101XP): https://www.lcsc.com/datasheet/lcsc_datasheet_1809121629_PANASONIC-EEHZA1E101XP_C264047.pdf
- SDAPO DP9900 series: https://wmsc.lcsc.com/wmsc/upload/file/pdf/v2/lcsc/2305061824_SDAPO-DP9900LP-5V_C5380108.pdf
- TI SLUA454, EMI-compliant PoE PD with non-isolated DC/DC: https://www.ti.com/lit/pdf/slua454
- TI SLUA469, EMI-compliant PoE PD with isolated flyback: https://www.ti.com/lit/pdf/slua469
- TI SLVA233A, circuit protection for PoE: https://www.ti.com/lit/pdf/slva233
- TI SLUA736, PoE PD surge: https://www.ti.com/lit/pdf/slua736
- Microchip AN3468, PoE PD design (section 7, spacing)
- Silvertel ANX-POE-Isolation-Barrier v2.0
- TI TLV761 datasheet SBVS349D: https://www.ti.com/lit/ds/symlink/tlv761.pdf
- TI LM1117 datasheet SNOS412Q (Table 9-2, SOT-223 RthJA vs copper area): https://www.ti.com/lit/ds/symlink/lm1117.pdf
- MDD SS34: https://www.lcsc.com/datasheet/lcsc_datasheet_2407101107_MDD-Microdiode-Semiconductor-SS34_C8678.pdf
- HJC SMF6.0A: https://wmsc.lcsc.com/wmsc/upload/file/pdf/v2/lcsc/2312011957_hongjiacheng-SMF6-0A_C19077499.pdf
- HJC H5VUT2U: https://wmsc.lcsc.com/wmsc/upload/file/pdf/v2/lcsc/2401261525_hongjiacheng-H5VUT2U_C20615824.pdf
- USB 2.0 specification 7.2.1, 7.2.4.1 (usb.org `usb_20_20190524.zip`); ETSI EN 300 220-2 V3.2.1 Annex B
- BHFUSE BSMD0805-075-16V: https://www.lcsc.com/datasheet/lcsc_datasheet_2305121424_BHFUSE-BSMD0805-075-16V_C976303.pdf
- LilyGO T-ETH-Lite-S3 and PoE shield schematics: https://github.com/Xinyuan-LilyGO/LilyGO-T-ETH-Series
- JLCPCB stackups: https://jlcpcb.com/impedance
- JLCPCB impedance calculator: https://jlcpcb.com/pcb-impedance-calculator
- JLCPCB quote page (stackup options, "special stackup" notes, via price rule): https://cart.jlcpcb.com/quote
- JLCPCB PCB assembly FAQ (Basic, Preferred, Extended fees): https://jlcpcb.com/help/article/pcb-assembly-faqs
- JLCPCB PCBA price (Economic and Standard PCBA): https://jlcpcb.com/help/article/pcb-assembly-price
- JLCPCB PCBA capabilities (Economic PCBA: standard stackups only): https://jlcpcb.com/capabilities/pcb-assembly-capabilities
