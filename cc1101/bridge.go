// Package cc1101 drives a CC1101 radio over the thin ESP32 serial<->SPI bridge.
// All CC1101 semantics (registers, strobes, FIFO) live here on the host; the
// ESP32 only frames raw SPI transactions. See firmware/cc1101_bridge for the
// firmware.
package cc1101

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"go.bug.st/serial"
)

// Link speeds. Firmware v2.10 and later talks DefaultBaud. Firmware before v2.10
// talks LegacyBaud, and Open still reaches it to read its banner.
const (
	DefaultBaud = 230400
	LegacyBaud  = 921600
)

// ErrLinkFault wraps every error of a request or reply that the serial link
// lost or cut: no answer by the deadline, a reply that stops before its length,
// a reply of the wrong shape. The framing is restored before the next request,
// so the caller may issue the next request. The lost reply stays lost.
var ErrLinkFault = errors.New("serial link fault")

var (
	errSerialDeadline  = fmt.Errorf("%w: deadline exceeded", ErrLinkFault)
	errReplyTruncated  = fmt.Errorf("%w: reply truncated", ErrLinkFault)
	errUnexpectedReply = fmt.Errorf("%w: reply does not answer the request", ErrLinkFault)
	// errRequestDropped is the firmware answer ST_BAD_CRC: the request lost a byte
	// on the way, and the firmware did not run it.
	errRequestDropped = fmt.Errorf("%w: the firmware dropped the request (bad crc)", ErrLinkFault)
)

// ErrUnconfigured is the firmware answer ST_UNCONFIGURED: the ESP32 or the CC1101
// reset, and the chip holds no register set. The firmware refuses every transmit
// command until Configure runs again.
var ErrUnconfigured = errors.New("the bridge chip is not configured")

// ErrAirtimeBudget is the firmware answer ST_AIRTIME: the hourly budget of time
// on air is used up. Nothing went out, so the job or command can run again later.
var ErrAirtimeBudget = errors.New("the hourly airtime budget is used up")

// FirmwareLevel is the oldest bridge firmware this host speaks. Older firmware
// does not check the request crc and does not know CMD_CONFIGURED or CMD_AIRTIME.
const (
	FirmwareMajor = 2
	FirmwareMinor = 11
	FirmwareLevel = "v2.11"
)

// FirmwareError is a bridge firmware that the host cannot work with: a level
// below FirmwareLevel, or a v2.11 build whose AIRTIME contract is older than the
// one the host speaks. Both need a flash.
type FirmwareError struct {
	Found  string // "v2.10", or "unknown" for a banner with no version
	Banner string
	// Contract names the part of the protocol that the firmware does not speak
	// at its level. Empty: the level itself is too old.
	Contract string
}

func (e *FirmwareError) Error() string {
	if e.Contract != "" {
		return fmt.Sprintf("bridge firmware %s %s: flash firmware/cc1101_bridge from the release of this daemon, see docs/OPERATION.md (Bridge firmware level)", e.Found, e.Contract)
	}
	return fmt.Sprintf("bridge firmware %s found, %s or later required: flash firmware/cc1101_bridge, see docs/OPERATION.md (Bridge firmware level)", e.Found, FirmwareLevel)
}

// ParseBanner reads the protocol level from a PING banner of the form
// "CC1101BRIDGE v2.11 (<build>)".
func ParseBanner(banner string) (int, int, bool) {
	fields := strings.Fields(banner)
	if len(fields) < 2 || fields[0] != bannerPrefix || !strings.HasPrefix(fields[1], "v") {
		return 0, 0, false
	}
	maj, min, found := strings.Cut(strings.TrimPrefix(fields[1], "v"), ".")
	if !found {
		return 0, 0, false
	}
	major, err1 := strconv.Atoi(maj)
	minor, err2 := strconv.Atoi(min)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return major, minor, true
}

// checkFirmware refuses a bridge below FirmwareLevel.
func (b *Bridge) checkFirmware() error {
	major, minor, ok := ParseBanner(b.banner)
	if ok && (major > FirmwareMajor || (major == FirmwareMajor && minor >= FirmwareMinor)) {
		return nil
	}
	return &FirmwareError{Found: b.bannerLevel(), Banner: b.banner}
}

// bannerLevel is the protocol level of the banner, "v2.11", or "unknown".
func (b *Bridge) bannerLevel() string {
	if major, minor, ok := ParseBanner(b.banner); ok {
		return fmt.Sprintf("v%d.%d", major, minor)
	}
	return "unknown"
}

// oldAirtime is the error for a firmware whose AIRTIME is not the one of this
// host: v2.11 builds from before the 61 buckets answer a query with 8 bytes and
// refuse the preload.
func (b *Bridge) oldAirtime() error {
	return &FirmwareError{Found: b.bannerLevel(), Banner: b.banner, Contract: "has the old AIRTIME command (no per-minute buckets, no preload)"}
}

// requestWindow is the reply wait of a request that has no window of its own. A
// request waits for its reply for b.timeout plus one second, so a reply that the
// link lost holds the host silent for 1.25 s at most. The firmware drops its
// coordinator, queue and pair window after 5 s with no request: the bound leaves
// room for the retry backoff of the live loop. A request with a window of its own
// waits for that window. A command that can arrive behind a transaction waits
// radioQueueWait at least.
const requestWindow = 250 * time.Millisecond

// radioQueueWait is the least reply wait of a command that the live loop can send
// while a CSL transaction runs: PAIRWIN, RABURST, TXTRAIN and the register check
// after an expired job.
// The firmware runs one when the transaction in progress has ended, up to 3.9 s,
// and the requests behind it wait in the same queue. The firmware refreshes its
// host timer when a request arrives and when a command ends, so a wait of 4.5 s
// leaves the 5 s limit in place. A request that no transaction can hold back
// keeps requestWindow: XFER, STROBE and the idle register check, which the loop
// sends only with no job out, so that two lost replies in a row cannot pass the
// 5 s.
const radioQueueWait = 4500 * time.Millisecond

// replyStall bounds a reply once its first byte arrived. The firmware writes a
// reply in one go, at most 1028 bytes, which takes 45 ms at 230400 baud. A reply
// that has not ended after replyStall lost its tail for good.
const replyStall = 250 * time.Millisecond

// replyQuiet is the silence that ends the discard of a lost reply.
const replyQuiet = 30 * time.Millisecond

// probeWait bounds one PING that tells which rate the firmware talks. A rate
// gets probeAttempts of them: on a link that cuts replies, the banner of the
// right rate can be the reply that is lost.
const (
	probeWait     = 500 * time.Millisecond
	probeAttempts = 3
)

// serialStaleMax bounds the replies one request reads past before its own. A
// reply carries no command echo, so a reply of the wrong shape is the late
// reply of an earlier request, and the reply to this request follows it.
const serialStaleMax = 2

// serialDrainLimit bounds a drain: input that never goes quiet is not the tail
// of a stream.
const serialDrainLimit = 3 * time.Second

// bannerPrefix starts the PING banner of every firmware version.
const bannerPrefix = "CC1101BRIDGE"

// replyCheck reports whether a reply has the shape of the answer to a request.
type replyCheck func(status byte, data []byte) bool

// lateReply takes the reply of a request that missed its deadline, once the
// reply arrives. A nil lateReply discards it.
type lateReply func(status byte, data []byte)

// Serial framing (matches firmware/cc1101_bridge/cc1101_bridge.ino, 8N1):
//
//	request : 0xA5 cmd lenHi lenLo payload crc
//	response: 0x5A status lenHi lenLo payload
//
// crc is CRC-8 (polynomial 0x07, init 0, no reflection, no final xor) over cmd,
// lenHi, lenLo and the payload.
const (
	reqMagic = 0xA5
	rspMagic = 0x5A

	cmdPing     = 0x01
	cmdXfer     = 0x02 // full-duplex, CS-framed
	cmdGDO      = 0x03
	cmdReset    = 0x04
	cmdStrobe   = 0x05
	cmdRecv     = 0x06 // receive one packet, drained on-chip
	cmdStream   = 0x07 // continuous RX; streams frames until a duration elapses
	cmdTxTrain  = 0x08 // back-to-back TX burst, inter-frame gap set on-device
	cmdRespond  = 0x09 // continuous RX with on-device auto-response to a match
	cmdAutoAck  = 0x0A
	cmdRABurst  = 0x0B
	cmdCSLTx    = 0x0C
	cmdCSLQueue = 0x0D
	// cmdPairWindow is 0x0E, in pair_window.go.
	cmdConfigured = 0x0F // the register set is loaded
	cmdAirtime    = 0x10 // time on air over the window, the budget and the minute buckets
	cmdLBT        = 0x11 // listen before talk: enable, threshold in dBm

	statusUnconfigured = 0x1c
	statusAirtime      = 0x1d
	statusBadLen       = 0xe1
	statusBadArgs      = 0xe3
	statusBadPayload   = 0xe2
	statusBadCRC       = 0xe4

	streamFrame = 0xF1 // 5A F1 <n> <n bytes> <freqest> <xor8>
	streamEnd   = 0xF2 // 5A F2 00
	streamAlive = 0xF3 // 5A F3 00 heartbeat: link live but quiet (keeps read timer fed)
)

// Bridge is a connection to the ESP32<->CC1101 bridge.
type Bridge struct {
	port       serial.Port
	timeout    time.Duration
	curFSCTRL0 byte // current frequency-offset register
	// owed has one entry per request that missed its deadline before its reply
	// arrived, in request order. The next request reads those replies first.
	owed []lateReply
	// late holds the CSL queue events that arrived as the reply of a poll that
	// missed its deadline. The next polls return them first.
	late []*CSLQueueEvent
	// serialErrors counts the requests whose reply the link lost, since Open.
	serialErrors atomic.Uint64
	baud         int
	banner       string
}

// SerialErrors is the number of requests whose reply did not arrive whole.
func (b *Bridge) SerialErrors() uint64 { return b.serialErrors.Load() }

// Baud is the rate the firmware answered at when the port opened.
func (b *Bridge) Baud() int { return b.baud }

// Banner is the PING banner the firmware answered with when the port opened.
func (b *Bridge) Banner() string { return b.banner }

// Open connects to the ESP32 bridge on the given serial port. The ESP32
// auto-resets when the port opens, so this waits for it to boot. It asks the
// firmware for its banner at DefaultBaud and then at LegacyBaud, and keeps the
// rate that answered. A firmware below FirmwareLevel is closed again and
// reported as a *FirmwareError.
func Open(portName string) (*Bridge, error) {
	p, err := serial.Open(portName, &serial.Mode{BaudRate: DefaultBaud})
	if err != nil {
		return nil, err
	}
	return attach(p, 2*time.Second)
}

// attach starts the bridge on an open port and checks its firmware. A bridge
// below FirmwareLevel is stopped before the port closes: see quiesce.
func attach(p serial.Port, boot time.Duration) (*Bridge, error) {
	b, err := start(p, boot)
	if err != nil {
		return nil, err
	}
	if err := b.checkFirmware(); err != nil {
		b.quiesce()
		b.Close()
		return nil, err
	}
	return b, nil
}

// quiesce stops a bridge that the daemon refuses to run. An old firmware has no
// host timeout, so a bridge that a --tx session left armed would keep its CSL
// queue and its transmitter after the port closes, when the DTR reset does not
// fire. The flush and the reset go out best effort: an old firmware reads the
// request and ignores the trailing CRC byte, and a firmware that does not know
// a command answers with an error that is of no use here.
func (b *Bridge) quiesce() {
	_ = b.FlushCSLQueue()
	_ = b.Reset()
}

func start(p serial.Port, boot time.Duration) (*Bridge, error) {
	if err := p.SetReadTimeout(100 * time.Millisecond); err != nil {
		p.Close()
		return nil, err
	}
	time.Sleep(boot) // ESP32 boot if DTR reset fired
	b := &Bridge{port: p, timeout: requestWindow}
	if err := b.probe(); err != nil {
		p.Close()
		return nil, err
	}
	return b, nil
}

// probe finds the rate of the firmware. The DTR reset is unreliable on the
// CH340, so a stream left running by a previous (killed) session may still be
// flooding the port. Send a byte to abort any running stream (firmware breaks on
// Serial.available()), then drain everything until the port goes quiet, so the
// PING starts from a clean command loop.
func (b *Bridge) probe() error {
	for _, baud := range []int{DefaultBaud, LegacyBaud} {
		if err := b.port.SetMode(&serial.Mode{BaudRate: baud}); err != nil {
			return err
		}
		b.port.Write([]byte{0x00})
		if err := b.drain(400*time.Millisecond, serialDrainLimit); err != nil {
			return err
		}
		for range probeAttempts {
			_, banner, err := b.reqUntil(cmdPing, nil, time.Now().Add(probeWait), isBanner, nil)
			b.owed = nil
			if err == nil {
				b.baud, b.banner = baud, string(banner)
				b.serialErrors.Store(0)
				return nil
			}
			if !errors.Is(err, ErrLinkFault) {
				return err
			}
		}
	}
	return fmt.Errorf("the bridge does not answer at %d or %d baud: check the port, then flash firmware %s or later from firmware/cc1101_bridge", DefaultBaud, LegacyBaud, FirmwareLevel)
}

// Close releases the serial port.
func (b *Bridge) Close() error { return b.port.Close() }

// drain discards all input until the port is quiet for `quiet`: a leftover
// stream at startup, or the tail of an aborted stream. Input that still
// arrives after limit is an error.
func (b *Bridge) drain(quiet, limit time.Duration) error {
	b.port.SetReadTimeout(quiet)
	defer b.port.SetReadTimeout(100 * time.Millisecond)
	buf := make([]byte, 512)
	deadline := time.Now().Add(limit)
	for {
		n, err := b.port.Read(buf)
		if err != nil || n == 0 {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("serial input still busy after %s", limit)
		}
	}
}

func (b *Bridge) readExact(n int) ([]byte, error) {
	return b.readExactUntil(n, time.Now().Add(b.timeout+time.Second))
}

func (b *Bridge) readExactUntil(n int, deadline time.Time) ([]byte, error) {
	out := make([]byte, 0, n)
	buf := make([]byte, n)
	for len(out) < n {
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("%w after %d/%d bytes", errSerialDeadline, len(out), n)
		}
		m, err := b.port.Read(buf[:n-len(out)])
		if err != nil {
			return nil, err
		}
		if m > 0 {
			out = append(out, buf[:m]...)
			continue
		}
	}
	return out, nil
}

// req sends a request and returns (status, payload). It resyncs on the response
// magic byte, tolerating stray boot/noise bytes, and reads past a reply that
// valid rejects.
func (b *Bridge) req(cmd byte, payload []byte, valid replyCheck) (byte, []byte, error) {
	return b.reqUntil(cmd, payload, time.Now().Add(b.timeout+time.Second), valid, nil)
}

// reqRadio is req for a command that can wait behind a transaction: see
// radioQueueWait. It never waits less than req.
func (b *Bridge) reqRadio(cmd byte, payload []byte, valid replyCheck) (byte, []byte, error) {
	return b.reqUntil(cmd, payload, time.Now().Add(max(b.timeout+time.Second, radioQueueWait)), valid, nil)
}

// reqUntil sends a request and reads its reply by deadline. The firmware
// answers every request once and in request order, so a reply that valid
// rejects is the late reply of an earlier request, and the reply to this
// request follows it. A request that misses its deadline leaves its reply
// owed: the next request resyncs first and hands that reply to late. A reply
// that starts and stops short is lost, and nothing is owed for it.
func (b *Bridge) reqUntil(cmd byte, payload []byte, deadline time.Time, valid replyCheck, late lateReply) (status byte, data []byte, err error) {
	defer func() {
		if errors.Is(err, ErrLinkFault) {
			b.serialErrors.Add(1)
		}
	}()
	if err := b.resync(deadline); err != nil {
		return 0, nil, err
	}
	if err := b.send(cmd, payload); err != nil {
		return 0, nil, err
	}
	for skipped := 0; ; skipped++ {
		status, data, err := b.readReply(deadline)
		if err != nil {
			if !errors.Is(err, errReplyTruncated) {
				b.owed = append(b.owed, late)
			}
			return 0, nil, err
		}
		switch status {
		case statusBadCRC, statusBadPayload:
			// A request without its crc byte is ST_BAD_PAYLOAD, a wrong crc is
			// ST_BAD_CRC. The firmware ran neither.
			return 0, nil, errRequestDropped
		case statusUnconfigured:
			return 0, nil, ErrUnconfigured
		}
		if parserReject(status) || valid(status, data) {
			return status, data, nil
		}
		if skipped == serialStaleMax {
			b.owed = append(b.owed, late)
			return 0, nil, fmt.Errorf("%w: status 0x%02x with %d bytes", errUnexpectedReply, status, len(data))
		}
	}
}

// resync reads the replies still owed to requests that missed their deadline.
// A PING goes out behind them. Every reply before its banner is a late reply,
// in request order, and the banner shows that nothing else is in flight. A
// bridge that rebooted lost the owed replies, and the banner comes first.
func (b *Bridge) resync(deadline time.Time) error {
	if len(b.owed) == 0 {
		return nil
	}
	if err := b.send(cmdPing, nil); err != nil {
		return err
	}
	b.owed = append(b.owed, nil)
	for extra := 0; ; {
		status, data, err := b.readReply(deadline)
		if err != nil {
			return fmt.Errorf("serial resync: %w", err)
		}
		if isBanner(status, data) {
			b.owed = nil
			return nil
		}
		if len(b.owed) > 1 {
			late := b.owed[0]
			b.owed = b.owed[1:]
			if late != nil {
				late(status, data)
			}
			continue
		}
		if extra++; extra > serialStaleMax {
			return fmt.Errorf("serial resync: %w: status 0x%02x with %d bytes", errUnexpectedReply, status, len(data))
		}
	}
}

func (b *Bridge) send(cmd byte, payload []byte) error {
	_, err := b.port.Write(buildRequest(cmd, payload))
	return err
}

// buildRequest frames one request, crc included.
func buildRequest(cmd byte, payload []byte) []byte {
	n := len(payload)
	req := make([]byte, 0, 5+n)
	req = append(req, reqMagic, cmd, byte(n>>8), byte(n))
	req = append(req, payload...)
	return append(req, crc8(req[1:]))
}

// crc8 is CRC-8 with polynomial 0x07, init 0, no reflection and no final xor.
func crc8(data []byte) byte {
	var crc byte
	for _, b := range data {
		crc ^= b
		for range 8 {
			if crc&0x80 != 0 {
				crc = crc<<1 ^ 0x07
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

func (b *Bridge) readReply(deadline time.Time) (byte, []byte, error) {
	for {
		m, err := b.readExactUntil(1, deadline)
		if err != nil {
			return 0, nil, err
		}
		if m[0] == rspMagic {
			break
		}
	}
	end := time.Now().Add(replyStall)
	if end.After(deadline) {
		end = deadline
	}
	head, err := b.readExactUntil(3, end)
	if err != nil {
		return 0, nil, b.lostReply(err)
	}
	status := head[0]
	rlen := int(head[1])<<8 | int(head[2])
	if err := responsePayloadLength(rlen); err != nil {
		return 0, nil, b.lostReply(err)
	}
	var data []byte
	if rlen > 0 {
		if data, err = b.readExactUntil(rlen, end); err != nil {
			return 0, nil, b.lostReply(err)
		}
	}
	return status, data, nil
}

// lostReply turns a read error inside a reply into errReplyTruncated and
// discards what is left of the reply, so the next request reads from a quiet
// line. An error of the port itself comes back as it is.
func (b *Bridge) lostReply(cause error) error {
	if !errors.Is(cause, ErrLinkFault) {
		return cause
	}
	err := fmt.Errorf("%w: %s", errReplyTruncated, strings.TrimPrefix(cause.Error(), ErrLinkFault.Error()+": "))
	if derr := b.drain(replyQuiet, serialDrainLimit); derr != nil {
		return fmt.Errorf("%w; %v", err, derr)
	}
	return err
}

// parserReject reports the status the firmware parser answers a request it
// could not read with, ST_BAD_CMD to ST_BAD_ARGS. It answers any request.
func parserReject(status byte) bool {
	return status >= 0xe0 && status <= 0xe3
}

func isBanner(status byte, data []byte) bool {
	return status == 0 && strings.HasPrefix(string(data), bannerPrefix)
}

// okLen accepts a reply with status 0 and n payload bytes.
func okLen(n int) replyCheck {
	return func(status byte, data []byte) bool { return status == 0 && len(data) == n }
}

func responsePayloadLength(n int) error {
	if n > maxReqPayload {
		return fmt.Errorf("%w: length %d exceeds %d", ErrLinkFault, n, maxReqPayload)
	}
	return nil
}

// --- primitives ---

// Ping returns the firmware banner.
func (b *Bridge) Ping() (string, error) {
	_, d, err := b.req(cmdPing, nil, isBanner)
	return string(d), err
}

// markConfigured tells the firmware that the register set is loaded. Until it
// arrives, the firmware refuses every transmit command.
func (b *Bridge) markConfigured() error {
	_, _, err := b.req(cmdConfigured, nil, okLen(0))
	return err
}

// AirtimeBuckets is the number of one-minute buckets of the firmware count: the
// current minute and the 60 before it, so the window never undercounts an hour.
const AirtimeBuckets = 61

// The hourly budgets of the airtime limit, in ms: the Tcum_on of polite spectrum
// access in ETSI EN 300 220-2 clause 4.6, 1 % without a listen before talk and
// 100 s with it.
const (
	AirtimeBudgetNoLBT uint32 = 36000
	AirtimeBudgetLBT   uint32 = 100000
)

// AirtimeBudget is the budget of the airtime limit for a listen-before-talk setting.
func AirtimeBudget(l LBT) uint32 {
	if l.Enabled {
		return AirtimeBudgetLBT
	}
	return AirtimeBudgetNoLBT
}

// AirtimeReading is the firmware count of the time on air. Buckets holds the
// milliseconds on air of each minute, oldest first, the current minute last.
// BudgetMS is the budget in force, and 0 means that the firmware refuses nothing.
type AirtimeReading struct {
	UsedMS   uint32
	BudgetMS uint32
	Buckets  [AirtimeBuckets]uint16
}

const (
	airtimeReplyLen    = 8 + 2*AirtimeBuckets
	airtimeOldReplyLen = 8 // used and budget, no buckets
)

// Airtime reads the time the firmware had the transmitter on over its window,
// the budget of that window and the per-minute buckets, all in milliseconds.
func (b *Bridge) Airtime() (AirtimeReading, error) {
	status, d, err := b.req(cmdAirtime, nil, airtimeReply)
	if err != nil {
		return AirtimeReading{}, err
	}
	return b.parseAirtime(status, d)
}

// PreloadAirtime loads the buckets into the firmware, aligned to its current
// minute, and returns the count as a query does. The firmware takes the load
// only when its total is larger than the total of its own window, so a load never
// lowers a count. The daemon sends it after the register load and before
// CMD_CONFIGURED.
func (b *Bridge) PreloadAirtime(buckets [AirtimeBuckets]uint16) (AirtimeReading, error) {
	return b.loadAirtime(buckets, nil)
}

// LoadAirtime is PreloadAirtime and sets the budget of the firmware to budgetMS.
// The firmware refuses a transmission above the budget with ST_AIRTIME. It has
// no budget after boot and after a reset, so the host sends it at every start.
func (b *Bridge) LoadAirtime(buckets [AirtimeBuckets]uint16, budgetMS uint32) (AirtimeReading, error) {
	return b.loadAirtime(buckets, &budgetMS)
}

func (b *Bridge) loadAirtime(buckets [AirtimeBuckets]uint16, budgetMS *uint32) (AirtimeReading, error) {
	payload := make([]byte, 2*AirtimeBuckets, 2*AirtimeBuckets+4)
	for i, v := range buckets {
		binary.BigEndian.PutUint16(payload[2*i:], v)
	}
	if budgetMS != nil {
		payload = binary.BigEndian.AppendUint32(payload, *budgetMS)
	}
	status, d, err := b.req(cmdAirtime, payload, airtimeReply)
	if err == nil && status == statusBadLen && budgetMS != nil {
		// The parser of the firmware also answers E1 when a bit of the length field
		// flipped on the way. One E1 is a link fault, and the load goes out again.
		// A second one says that the firmware takes only 122 bytes.
		status, d, err = b.req(cmdAirtime, payload, airtimeReply)
	}
	if err != nil {
		return AirtimeReading{}, err
	}
	switch {
	case status == statusBadLen && budgetMS != nil:
		return AirtimeReading{}, &FirmwareError{Found: b.bannerLevel(), Banner: b.banner, Contract: "has no airtime budget (AIRTIME with 126 bytes)"}
	case status == statusBadArgs:
		return AirtimeReading{}, b.oldAirtime()
	case parserReject(status):
		// The firmware of this contract takes the 122 bytes. A reject of the
		// parser means that the request lost bytes on the way, which a retry
		// fixes.
		return AirtimeReading{}, fmt.Errorf("%w: AIRTIME preload status 0x%02x", ErrLinkFault, status)
	}
	return b.parseAirtime(status, d)
}

// airtimeReply accepts an AIRTIME answer, or the refusal of a payload length.
// The 8-byte answer of the old firmware is accepted so that parseAirtime can
// name it.
func airtimeReply(status byte, data []byte) bool {
	return (status == 0 && (len(data) == airtimeReplyLen || len(data) == airtimeOldReplyLen)) || status == statusBadLen
}

func (b *Bridge) parseAirtime(status byte, d []byte) (AirtimeReading, error) {
	if status == 0 && len(d) == airtimeOldReplyLen {
		return AirtimeReading{}, b.oldAirtime()
	}
	if status != 0 || len(d) != airtimeReplyLen {
		return AirtimeReading{}, fmt.Errorf("AIRTIME status 0x%02x with %d bytes", status, len(d))
	}
	r := AirtimeReading{UsedMS: binary.BigEndian.Uint32(d[:4]), BudgetMS: binary.BigEndian.Uint32(d[4:8])}
	for i := range r.Buckets {
		r.Buckets[i] = binary.BigEndian.Uint16(d[8+2*i:])
	}
	return r, nil
}

// Reset issues a manual CC1101 power-on reset.
func (b *Bridge) Reset() error { _, _, err := b.req(cmdReset, nil, okLen(0)); return err }

// Xfer does a full-duplex SPI transfer, returning the MISO bytes.
func (b *Bridge) Xfer(data []byte) ([]byte, error) {
	_, d, err := b.req(cmdXfer, data, okLen(len(data)))
	return d, err
}
