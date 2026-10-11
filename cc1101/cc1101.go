package cc1101

import (
	"encoding/hex"
	"fmt"
	"sort"
	"time"
)

// CC1101 register addresses.
const (
	IOCFG2, IOCFG1, IOCFG0, FIFOTHR             = 0x00, 0x01, 0x02, 0x03
	SYNC1, SYNC0, PKTLEN, PKTCTRL1, PKTCTRL0    = 0x04, 0x05, 0x06, 0x07, 0x08
	ADDR, CHANNR, FSCTRL1, FSCTRL0              = 0x09, 0x0A, 0x0B, 0x0C
	FREQ2, FREQ1, FREQ0                         = 0x0D, 0x0E, 0x0F
	MDMCFG4, MDMCFG3, MDMCFG2, MDMCFG1, MDMCFG0 = 0x10, 0x11, 0x12, 0x13, 0x14
	DEVIATN, MCSM2, MCSM1, MCSM0                = 0x15, 0x16, 0x17, 0x18
	FOCCFG, BSCFG                               = 0x19, 0x1A
	AGCCTRL2, AGCCTRL1, AGCCTRL0                = 0x1B, 0x1C, 0x1D
	FREND1, FREND0                              = 0x21, 0x22
	FSCAL3, FSCAL2, FSCAL1, FSCAL0              = 0x23, 0x24, 0x25, 0x26
	FSTEST, TEST2, TEST1, TEST0                 = 0x29, 0x2C, 0x2D, 0x2E

	// status registers (read with the burst bit)
	PARTNUM, VERSION, FREQEST   = 0x30, 0x31, 0x32
	MARCSTATE, TXBYTES, RXBYTES = 0x35, 0x3A, 0x3B

	PATABLE, TXFIFO, RXFIFO = 0x3E, 0x3F, 0x3F

	// command strobes
	SRES, SCAL, SRX, STX = 0x30, 0x33, 0x34, 0x35
	SIDLE, SFRX, SFTX    = 0x36, 0x3A, 0x3B

	writeBurst, readSingle, readBurst = 0x40, 0x80, 0xC0

	// MARCSTATE values, masked with 0x1F.
	marcIdle, marcTX, marcTXEnd = 0x01, 0x13, 0x14
	marcTXUnderflow             = 0x16
)

// tadoRegs is the Tado 868 MHz PHY register set (26 MHz crystal). Channel 26 =
// 868.324 MHz, 2-GFSK, 25.39 kHz dev, 49.99 kBaud, sync 0xD391, variable length,
// hardware CRC16, no whitening. See docs "RF Protocol e PHY".
var tadoRegs = []struct{ addr, val byte }{
	{IOCFG2, 0x00}, // GDO2: RX FIFO threshold
	{IOCFG0, iocfg0Tado},
	{FIFOTHR, 0x47}, // ADC_RETENTION on (RX BW < 325 kHz), FIFO thr 33/32
	{SYNC1, 0xD3},
	{SYNC0, 0x91},
	{PKTLEN, 0xFF},
	{PKTCTRL1, 0x04}, // append RSSI/LQI/CRC status, no address check
	{PKTCTRL0, 0x05}, // variable length, CRC on, whitening off
	{CHANNR, 0x1A},   // channel 26 (per-install; sweep if RX is silent)
	{FSCTRL1, 0x06},  // 152 kHz IF (Tado firmware value)
	{FSCTRL0, 0x40},  // this module's crystal-offset compensation; the daemon seeds the
	//                   stored autotune calibration at start, then the hardware FOC tracks
	//                   the residual per-transmitter offset. No host retune (Tado approach).
	{FREQ2, 0x21}, // base 863.125 MHz; carrier = base + CHANNR*199.951 kHz
	{FREQ1, 0x32},
	{FREQ0, 0x76},
	{MDMCFG4, 0xAA}, // RX BW 135 kHz (Tado firmware value), DRATE_E=10
	{MDMCFG3, 0xF8}, // DRATE_M=248 -> 49.99 kBaud
	{MDMCFG2, 0x13}, // 2-GFSK, 30/32 sync-word bits
	{MDMCFG1, 0x22}, // 4-byte preamble
	{MDMCFG0, 0xF8}, // 199.951 kHz channel spacing
	{DEVIATN, 0x40}, // 25.39 kHz deviation
	{MCSM1, 0x3F},   // stay in RX after packet and TX (Tado firmware value)
	{MCSM0, 0x08},   // manual calibration only, no autocal on RX entry (Tado firmware value)
	{FOCCFG, 0x16},
	{BSCFG, 0x6C},
	{AGCCTRL2, 0x43},
	{AGCCTRL1, 0x40},
	{AGCCTRL0, 0x91},
	{FREND1, 0x56},
	{FREND0, 0x10},
	{FSCAL3, 0xE9},
	{FSCAL2, 0x2A},
	{FSCAL1, 0x00},
	{FSCAL0, 0x1F},
	{FSTEST, 0x59},
	{TEST2, 0x81},
	{TEST1, 0x35},
	{TEST0, 0x09},
}

// iocfg0Tado makes GDO0 assert on a CRC-OK packet. The firmware sets IOCFG0 to
// 0x2F after its own reset, and a CC1101 power-on reset leaves 0x3F, so the
// register also tells whether the chip still holds the Tado register set.
const iocfg0Tado = 0x07

const paTable0 = 0xC0 // ~+11 dBm at 868 MHz

// --- register access ---

func (b *Bridge) Strobe(addr byte) (byte, error) {
	_, d, err := b.req(cmdStrobe, []byte{addr}, okLen(1))
	if err != nil {
		return 0, err
	}
	return d[0], nil
}

func (b *Bridge) WriteReg(addr, val byte) error {
	_, err := b.Xfer([]byte{addr, val})
	return err
}

func (b *Bridge) ReadReg(addr byte) (byte, error) {
	d, err := b.Xfer([]byte{addr | readSingle, 0x00})
	if err != nil {
		return 0, err
	}
	return d[1], nil
}

// ReadStatus reads a status register (burst bit set; needed for 0x30-0x3D).
func (b *Bridge) ReadStatus(addr byte) (byte, error) {
	d, err := b.Xfer([]byte{addr | readBurst, 0x00})
	if err != nil {
		return 0, err
	}
	return d[1], nil
}

func (b *Bridge) WriteBurst(addr byte, data []byte) error {
	_, err := b.Xfer(append([]byte{addr | writeBurst}, data...))
	return err
}

func (b *Bridge) ReadBurst(addr byte, n int) ([]byte, error) {
	d, err := b.Xfer(append([]byte{addr | readBurst}, make([]byte, n)...))
	if err != nil {
		return nil, err
	}
	return d[1:], nil
}

// Configure resets the chip, loads the Tado PHY registers, and calibrates. It
// first flushes the CSL queue: jobs a crashed session left in the bridge would
// otherwise transmit on a chip that is half configured. Firmware v2.5 clears
// the queue on the reset as well.
func (b *Bridge) Configure() error {
	if err := b.FlushCSLQueue(); err != nil {
		return err
	}
	if err := b.Reset(); err != nil {
		return err
	}
	for _, r := range tadoRegs {
		if err := b.WriteReg(r.addr, r.val); err != nil {
			return err
		}
	}
	// FOCCFG stays at the Tado firmware value 0x16 loaded from tadoRegs above
	// (FOC_PRE_K = 3K, FOC_POST_K = K/2, FOC_LIMIT = +/-BW/4): the hardware
	// demodulator tracks each transmitter's crystal offset per packet, so no host
	// FSCTRL0 chasing.
	if err := b.WriteBurst(PATABLE, []byte{paTable0}); err != nil {
		return err
	}
	b.curFSCTRL0 = 0x40 // matches the tadoRegs FSCTRL0 default (drift-range center)
	b.Strobe(SIDLE)
	b.Strobe(SCAL)
	time.Sleep(5 * time.Millisecond)
	return nil
}

// Configured reports whether the chip still holds the register set Configure
// loaded. An ESP32 that rebooted with the USB link up, or a CC1101 that browned
// out, leaves the chip on reset defaults: every request still gets an answer,
// but the radio hears nothing.
func (b *Bridge) Configured() (bool, error) {
	v, err := b.ReadReg(IOCFG0)
	if err != nil {
		return false, err
	}
	return v == iocfg0Tado, nil
}

// PartVersion returns PARTNUM and VERSION (VERSION 0x04/0x14 = a live CC1101).
func (b *Bridge) PartVersion() (part, ver byte, err error) {
	if part, err = b.ReadStatus(PARTNUM); err != nil {
		return
	}
	ver, err = b.ReadStatus(VERSION)
	return
}

// s8 interprets a byte as a signed 8-bit value.
func s8(x byte) int {
	if x >= 128 {
		return int(x) - 256
	}
	return int(x)
}

// setFSCTRL0 writes the frequency-offset register and recalibrates the synth so
// the new offset takes effect.
func (b *Bridge) setFSCTRL0(v byte) error {
	if err := b.WriteReg(FSCTRL0, v); err != nil {
		return err
	}
	b.Strobe(SIDLE)
	b.Strobe(SCAL)
	time.Sleep(8 * time.Millisecond)
	b.curFSCTRL0 = v
	return nil
}

// FSCTRL0 returns the current frequency-offset register value.
func (b *Bridge) FSCTRL0() byte { return b.curFSCTRL0 }

// SetFSCTRL0 seeds the frequency-offset register. The daemon calls it once, at
// start, with the passive calibration stored by `freedo-dev autotune`.
func (b *Bridge) SetFSCTRL0(v byte) error { return b.setFSCTRL0(v) }

// medianInt returns the median of v (0 if empty). Median, not mean, so a single
// outlier FREQEST (a stray weak frame) cannot drag the correction off center. An
// even count averages the two middle values, truncated toward zero: the upper
// middle alone biases two equal populations onto the higher one.
func medianInt(v []int) int {
	if len(v) == 0 {
		return 0
	}
	s := append([]int(nil), v...)
	sort.Ints(s)
	mid := len(s) / 2
	if len(s)%2 == 1 {
		return s[mid]
	}
	return (s[mid-1] + s[mid]) / 2
}

// freqSource labels the transmitter of a frame. A wake-up frame carries no
// source address, but only the coordinator sends one, so it gets a fixed label.
func freqSource(payload []byte) (string, bool) {
	if len(payload) >= 16 && (payload[0] == 0x69 || payload[0] == 0x79) && payload[1] == 0xec {
		return string(payload[11:16]), true
	}
	if len(payload) >= 13 && payload[0] == 0x49 && payload[1] == 0xe8 {
		return string(payload[5:13]), true
	}
	if len(payload) >= 12 && payload[0] == 0x25 {
		return tuneCoordinator, true
	}
	return "", false
}

// Tune bounds. An isolated frame reads the module offset within one unit, but a
// coordinator frame that follows another frame within about 25 ms scatters by
// about 7 units and pins at the FOC limit (BW/4 = 21 units). A tune therefore
// rests on a population of frames and drops the clipped readings.
const (
	TuneWant     = 20 // usable frames one evaluation asks for; enough to outvote the scatter
	TuneMin      = 5  // fewest usable frames that still move FSCTRL0
	TuneDeadband = 4  // per-packet jitter; below it the correction is noise, not drift
)

const (
	autotuneRounds      = 3
	autotuneRoundBudget = 60 * time.Second
	autotuneClip        = 20 // one below the FOC limit; the drop bounds a measured correction to +/-19
)

// autotuneUsable rejects a reading pinned to the FOC limit: it is clipped, not
// measured, so it would pull the median toward the limit.
func autotuneUsable(freqEst int) bool {
	return freqEst > -autotuneClip && freqEst < autotuneClip
}

// TuneSample returns the source of a frame a tune may read: any CRC-OK frame
// that carries a FREQEST reading and that freqSource labels. Isolated device
// uplinks and the wake-up train read the offset within one unit. Coordinator
// frames that follow another frame scatter and clip at the FOC limit, and the
// per-source median absorbs that. An Enhanced ACK stays out because freqSource
// does not know it: it always follows a device uplink, so it is one of the
// frames that scatter.
func TuneSample(f *RxFrame) (source string, ok bool) {
	if !f.CRCOK || !f.HasFreqEst {
		return "", false
	}
	return freqSource(f.Payload)
}

// TuneCorrection returns the FSCTRL0 correction of one population of readings,
// the number of readings it rests on and whether the population clips on one
// side. One median per source, then the median across sources: a talkative
// transmitter must not move the coordinator carrier onto its own crystal offset.
// A single source is enough. Sign convention: FSCTRL0 += correction.
func TuneCorrection(samples map[string][]int) (correction int, usable int, clipped bool) {
	perSource := make([]int, 0, len(samples))
	total, low, high := 0, 0, 0
	for _, values := range samples {
		kept := make([]int, 0, len(values))
		for _, v := range values {
			switch {
			case autotuneUsable(v):
				kept = append(kept, v)
			case v < 0:
				low++
			default:
				high++
			}
		}
		total += len(values)
		if len(kept) == 0 {
			continue
		}
		usable += len(kept)
		perSource = append(perSource, medianInt(kept))
	}
	if side, ok := tuneClipSide(total, low, high); ok {
		return side, usable, true
	}
	if len(perSource) == 0 {
		return 0, 0, false
	}
	return medianInt(perSource), usable, false
}

// tuneClipSide returns the correction of a population that clips on one side.
// A register far past the FOC limit pins a large part of the readings at the
// limit with one sign and leaves the survivors near zero, so the drop removes
// the readings that carry the offset and the correction is the clip value
// itself. Under a third of the readings, or with both signs present, the
// clipping is the usual scatter of a frame that follows another frame.
func tuneClipSide(total, low, high int) (int, bool) {
	if low > 0 && high > 0 {
		return 0, false
	}
	clipped := low + high
	if clipped == 0 || clipped*3 < total {
		return 0, false
	}
	if low > 0 {
		return -autotuneClip, true
	}
	return autotuneClip, true
}

// tuneCoordinator is the freqSource label of the wake-up train, the one source
// that carries no address of its own.
const tuneCoordinator = "coordinator"

// TuneLabel renders a TuneSample source for a log line. Every source but the
// wake-up train is a raw address slice, so it goes out as hex.
func TuneLabel(source string) string {
	if source == tuneCoordinator {
		return tuneCoordinator
	}
	return hex.EncodeToString([]byte(source))
}

// Autotune centers FSCTRL0 on the live Tado carrier from a dedicated stream
// window. `freedo-dev autotune` runs it once per chip or installation and stores
// the result, which the daemon seeds at start. It reads the residual frequency
// error the demodulator reports per frame (FREQEST) and folds the median back
// into FSCTRL0, one round at a time, starting from the current register value.
// A round streams up to autotuneRoundBudget for TuneWant usable
// frames. It stops when the correction falls inside TuneDeadband, or after
// autotuneRounds rounds. A round with fewer than TuneMin usable frames leaves
// FSCTRL0 where it is and returns ok=false, unless its readings clip on one
// side: that round moves the register by the clip value. The only error is a
// serial fault.
func (b *Bridge) Autotune() (fsctrl0 byte, ok bool, err error) {
	for round := 0; round < autotuneRounds; round++ {
		samples, serr := b.autotuneRound(autotuneRoundBudget, TuneWant)
		if serr != nil {
			return b.curFSCTRL0, false, serr
		}
		correction, usable, clipped := TuneCorrection(samples)
		if usable < TuneMin && !clipped {
			return b.curFSCTRL0, false, nil
		}
		if correction >= -TuneDeadband && correction <= TuneDeadband {
			return b.curFSCTRL0, true, nil
		}
		if err := b.setFSCTRL0(byte(int(b.curFSCTRL0) + correction)); err != nil {
			return b.curFSCTRL0, false, err
		}
	}
	return b.curFSCTRL0, true, nil
}

// autotuneRound collects the FREQEST readings of one round. Streaming in short
// windows lets the round span the CSL quiet periods between bursts.
func (b *Bridge) autotuneRound(budget time.Duration, want int) (map[string][]int, error) {
	samples := make(map[string][]int)
	usable := 0
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) && usable < want {
		if err := b.Stream(1500*time.Millisecond, func(f *RxFrame) {
			source, valid := TuneSample(f)
			if !valid {
				return
			}
			samples[source] = append(samples[source], f.FreqEst)
			if autotuneUsable(f.FreqEst) {
				usable++
			}
		}); err != nil {
			return samples, err
		}
	}
	return samples, nil
}

// --- TX ---

// Send transmits one variable-length frame (payload without the length byte).
// It streams the FIFO for frames longer than 64 bytes. Returns false if the
// frame did not leave the chip within timeout, and false with an error if the
// TX FIFO underflowed.
func (b *Bridge) Send(payload []byte, timeout time.Duration) (bool, error) {
	data := append([]byte{byte(len(payload))}, payload...)
	b.Strobe(SIDLE)
	b.Strobe(SFTX)
	first := data
	if len(first) > 64 {
		first = data[:64]
	}
	if err := b.WriteBurst(TXFIFO, first); err != nil {
		return false, err
	}
	idx := len(first)
	b.Strobe(STX)
	for idx < len(data) {
		txb, err := b.ReadStatus(TXBYTES)
		if err != nil {
			return false, err
		}
		if txb&0x80 != 0 { // TX FIFO underflow
			b.Strobe(SFTX)
			return false, fmt.Errorf("TX FIFO underflow")
		}
		space := 64 - int(txb&0x7F)
		if space > 0 {
			end := idx + space
			if end > len(data) {
				end = len(data)
			}
			if err := b.WriteBurst(TXFIFO, data[idx:end]); err != nil {
				return false, err
			}
			idx = end
		} else {
			time.Sleep(time.Millisecond)
		}
	}
	// The Tado register set has MCSM1 TXOFF_MODE=RX, so the chip lands in RX and
	// never shows IDLE. The frame is out once the state machine left TX/TX_END
	// and the TX FIFO drained.
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		m, err := b.ReadStatus(MARCSTATE)
		if err != nil {
			return false, err
		}
		txb, err := b.ReadStatus(TXBYTES)
		if err != nil {
			return false, err
		}
		// An underflow leaves the count at zero with bit 0x80 set, which the
		// drained-FIFO test below reads as a frame that went out.
		if m&0x1F == marcTXUnderflow || txb&0x80 != 0 {
			b.Strobe(SFTX)
			return false, fmt.Errorf("TX FIFO underflow: MARCSTATE %#x TXBYTES %#x", m&0x1F, txb)
		}
		if s := m & 0x1F; s != marcTX && s != marcTXEnd {
			if txb&0x7F == 0 {
				return true, nil
			}
		}
		time.Sleep(time.Millisecond)
	}
	return false, nil
}

// Firmware limits (firmware/cc1101_bridge/cc1101_bridge.ino):
// MAX_TX_FRAME, MAX_TRAIN, MAX_MATCH and the request buffer MAX_PAYLOAD.
const (
	maxTrainFrame  = 140
	maxTrainFrames = 48
	maxMatch       = 64
	maxReqPayload  = 1024
)

// packTrain serializes frames as [len][bytes] sub-frames.
func packTrain(dst []byte, frames [][]byte) ([]byte, error) {
	for _, f := range frames {
		if len(f) == 0 || len(f) > maxTrainFrame {
			return nil, fmt.Errorf("frame length %d out of range", len(f))
		}
		dst = append(dst, byte(len(f)))
		dst = append(dst, f...)
	}
	return dst, nil
}

// SendTrain transmits frames back-to-back from the ESP32 (CMD_TXTRAIN): the
// inter-frame gap is SPI plus state-machine time instead of the ~2.5 ms USB
// serial round-trip a host-driven burst pays, which is what a CSL cold-start
// wake-up train needs. Returns how many frames the firmware put on air.
func (b *Bridge) SendTrain(frames [][]byte, timeout time.Duration) (int, error) {
	if len(frames) > 255 {
		return 0, fmt.Errorf("too many frames: %d", len(frames))
	}
	pkt, err := packTrain([]byte{byte(len(frames))}, frames)
	if err != nil {
		return 0, err
	}
	if len(pkt) > maxReqPayload {
		return 0, fmt.Errorf("train too large: %d bytes", len(pkt))
	}
	saved := b.timeout
	if timeout > saved {
		b.timeout = timeout
	}
	status, d, err := b.req(cmdTxTrain, pkt, func(status byte, d []byte) bool {
		return status == 0 && len(d) == 1 && int(d[0]) <= len(frames)
	})
	b.timeout = saved
	if err != nil {
		return 0, err
	}
	if status != 0x00 || len(d) < 1 {
		return 0, fmt.Errorf("TXTRAIN status 0x%02X", status)
	}
	return int(d[0]), nil
}

// --- RX ---

// RxFrame is a received link-layer frame (FCS already validated by the chip).
type RxFrame struct {
	Payload    []byte
	RSSIdBm    float64
	LQI        byte
	CRCOK      bool
	FreqEst    int  // residual carrier offset (FREQEST) in FSCTRL0 units, signed
	HasFreqEst bool // false when the receive path reports no FREQEST
}

// parseRxBlob turns a raw on-chip blob ([length][payload][rssi][lqi/crc]) into an
// RxFrame, or nil if malformed. Firmware v2.1 appends FREQEST after the LQI/CRC
// byte on the RECV, AUTOACK and CSL inbound blobs; an older firmware sends neither
// the byte nor a tune reading.
func parseRxBlob(data []byte) *RxFrame {
	if len(data) < 3 {
		return nil
	}
	length := int(data[0])
	if len(data) < 1+length+2 {
		return nil
	}
	rssiRaw := data[1+length]
	lqiCRC := data[1+length+1]
	rssi := float64(rssiRaw)/2 - 74
	if rssiRaw >= 128 {
		rssi = (float64(rssiRaw)-256)/2 - 74
	}
	frame := &RxFrame{
		Payload: append([]byte{}, data[1:1+length]...),
		RSSIdBm: rssi,
		LQI:     lqiCRC & 0x7F,
		CRCOK:   lqiCRC&0x80 != 0,
	}
	if len(data) >= 1+length+3 {
		frame.FreqEst = s8(data[1+length+2])
		frame.HasFreqEst = true
	}
	return frame
}

// RecvFW receives one packet with the FIFO drain done on the ESP32 (firmware
// CMD_RECV). Host-side draining over USB serial (~2.5 ms/round-trip) is too slow
// and corrupts frames > ~54 bytes; on-chip it is reliable. Returns nil on timeout.
func (b *Bridge) RecvFW(timeout time.Duration) (*RxFrame, error) {
	ms := timeout.Milliseconds()
	if ms > 0xFFFF {
		ms = 0xFFFF
	}
	status, data, err := b.req(cmdRecv, []byte{byte(ms >> 8), byte(ms)}, func(status byte, data []byte) bool {
		return (status == autoAckNoFrame && len(data) == 0) || (status == 0 && parseRxBlobExact(data) != nil)
	})
	if err != nil {
		return nil, err
	}
	if status != 0x00 {
		return nil, nil // timeout / no packet
	}
	return parseRxBlob(data), nil
}

// Stream keeps the radio armed and pushes every received frame for ~dur, invoking
// cb per frame. RX is never disarmed between frames (only re-armed on FIFO
// overflow), so back-to-back frames a one-packet RecvFW would miss are caught. The
// firmware emits a heartbeat during quiet periods, so a read timeout here signals a
// real fault, not just a silent CSL gap. On any fault - or if the window's
// STREAM_END never arrives - Stream aborts and resyncs the firmware to a clean
// stream end before returning, so the next Stream re-arms in phase instead of one
// stream lifecycle behind (the desync that used to wedge rx). The per-frame XOR
// check keeps the reader self-resynchronizing.
func (b *Bridge) Stream(dur time.Duration, cb func(*RxFrame)) error {
	return b.StreamUntil(dur, func(f *RxFrame) bool { cb(f); return false })
}

// StreamUntil is Stream that cb can end early by returning true. The host then
// aborts the firmware stream and reads up to its STREAM_END, so the port is back
// at the command loop and the next request reads its own response. A request
// sent inside the stream instead aborts it with its first byte and reads the
// STREAM_END as its response. A frame that arrives before STREAM_END still
// reaches cb.
func (b *Bridge) StreamUntil(dur time.Duration, cb func(*RxFrame) bool) error {
	ms := durMS(dur)
	if err := b.resync(time.Now().Add(b.timeout + time.Second)); err != nil {
		return err
	}
	if _, err := b.port.Write([]byte{reqMagic, cmdStream, 0x00, 0x02, byte(ms >> 8), byte(ms)}); err != nil {
		return err
	}
	return b.readStream(dur, cb)
}

// durMS clamps a duration to the 2-byte millisecond field the firmware reads.
func durMS(d time.Duration) int64 {
	ms := d.Milliseconds()
	if ms > 0xFFFF {
		ms = 0xFFFF
	}
	return ms
}

// readStream consumes the firmware frame stream (5A F1/F2/F3) of an already-sent
// STREAM request, invoking cb per frame. The first true from cb aborts the
// stream: any host byte ends the firmware window, which then sends STREAM_END.
func (b *Bridge) readStream(dur time.Duration, cb func(*RxFrame) bool) error {
	deadline := time.Now().Add(dur + 3*time.Second)
	stopping := false
	for time.Now().Before(deadline) {
		m, err := b.readExact(1)
		if err != nil {
			return b.resyncStream(err)
		}
		if m[0] != rspMagic {
			continue // scan for the marker
		}
		t, err := b.readExact(1)
		if err != nil {
			return b.resyncStream(err)
		}
		switch t[0] {
		case streamEnd:
			b.readExact(1) // trailing 0x00
			return nil
		case streamAlive:
			b.readExact(1) // trailing 0x00; liveness only, no frame
		case streamFrame:
			ln, err := b.readExact(1)
			if err != nil {
				return b.resyncStream(err)
			}
			data, err := b.readExact(int(ln[0]))
			if err != nil {
				return b.resyncStream(err)
			}
			fe, err := b.readExact(1) // residual FREQEST measured for this frame
			if err != nil {
				return b.resyncStream(err)
			}
			x, err := b.readExact(1)
			if err != nil {
				return b.resyncStream(err)
			}
			xor := fe[0]
			for _, c := range data {
				xor ^= c
			}
			if xor != x[0] {
				continue // desync: resync on the next marker
			}
			if f := parseRxBlob(data); f != nil {
				f.FreqEst, f.HasFreqEst = s8(fe[0]), true
				if cb(f) && !stopping {
					stopping = true
					if _, err := b.port.Write([]byte{0x00}); err != nil {
						return b.resyncStream(err)
					}
				}
			}
		default:
			// not a stream marker; keep scanning
		}
	}
	// The firmware ends every window with STREAM_END; reaching here means it never
	// arrived, so resync before the caller re-arms on top of a still-live stream.
	return b.resyncStream(fmt.Errorf("stream did not end within %v", dur+3*time.Second))
}

// resyncStream aborts a running firmware stream and drains until the port is quiet,
// so a following Stream re-arms from a clean command loop. Any byte aborts ccStream
// (it breaks on Serial.available()); the firmware then flushes STREAM_END and idles.
// It returns the original cause so the caller still sees why the stream failed.
func (b *Bridge) resyncStream(cause error) error {
	b.port.Write([]byte{0x00})
	if err := b.drain(300*time.Millisecond, serialDrainLimit); err != nil {
		return fmt.Errorf("%w; %v", cause, err)
	}
	return cause
}
