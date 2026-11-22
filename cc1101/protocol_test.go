package cc1101

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"time"
)

// The check value of CRC-8 with polynomial 0x07, init 0, no reflection and no
// final xor is 0xF4 for "123456789" (the CRC-8/SMBUS catalogue entry).
func TestCRC8CheckValue(t *testing.T) {
	if got := crc8([]byte("123456789")); got != 0xf4 {
		t.Fatalf("crc8 = %#02x, want 0xf4", got)
	}
}

func TestRequestFraming(t *testing.T) {
	got := buildRequest(cmdPing, nil)
	if want := []byte{reqMagic, cmdPing, 0, 0, crc8([]byte{cmdPing, 0, 0})}; !bytes.Equal(got, want) {
		t.Fatalf("PING = %x, want %x", got, want)
	}
	got = buildRequest(cmdXfer, []byte{0x80, 0x00})
	if want := []byte{reqMagic, cmdXfer, 0, 2, 0x80, 0x00, 0xa4}; !bytes.Equal(got, want) {
		t.Fatalf("XFER = %x, want %x", got, want)
	}
}

// crcPort checks the crc of every request it receives, and answers PING, the
// SPI transfers and the strobes like firmware v2.11.
type crcPort struct {
	fakePort
	bad int
}

func (p *crcPort) Write(data []byte) (int, error) {
	p.fakePort.Write(data)
	if len(data) == 1 {
		return 1, nil // the abort byte of a stream
	}
	if len(data) < 5 || data[0] != reqMagic || crc8(data[1:len(data)-1]) != data[len(data)-1] {
		p.bad++
	}
	body := data[4 : len(data)-1]
	switch data[1] {
	case cmdPing:
		p.read = append(p.read, autoAckResponse(0, []byte("CC1101BRIDGE v2.11 (test)"))...)
	case cmdXfer:
		p.read = append(p.read, autoAckResponse(0, make([]byte, len(body)))...)
	case cmdStrobe:
		p.read = append(p.read, autoAckResponse(0, []byte{0x0f})...)
	case cmdStream:
		p.read = append(p.read, rspMagic, streamEnd, 0x00)
	default:
		p.read = append(p.read, autoAckResponse(0, nil)...)
	}
	return len(data), nil
}

// Every request of the bridge carries the crc, also the stream request that
// bypasses req and the PING of a resync.
func TestEveryRequestCarriesTheCRC(t *testing.T) {
	p := &crcPort{}
	b := &Bridge{port: p, timeout: time.Second}
	if _, err := b.Ping(); err != nil {
		t.Fatal(err)
	}
	if err := b.Configure(); err != nil {
		t.Fatal(err)
	}
	if err := b.Stream(10*time.Millisecond, func(*RxFrame) {}); err != nil {
		t.Fatal(err)
	}
	b.owed = append(b.owed, nil)
	if _, err := b.Ping(); err != nil {
		t.Fatal(err)
	}
	if p.bad != 0 || len(p.writes) < 20 {
		t.Fatalf("%d of %d requests carried a wrong crc", p.bad, len(p.writes))
	}
}

// ST_BAD_CRC says the firmware dropped the request. It is a lost request: a
// link fault the caller survives, counted, and the next request works.
func TestBadCRCIsALostRequest(t *testing.T) {
	p := &fakePort{read: append(autoAckResponse(statusBadCRC, nil), autoAckResponse(0, []byte{1, 2})...)}
	b := &Bridge{port: p, timeout: time.Second}
	_, err := b.Xfer([]byte{0x80, 0x00})
	if !errors.Is(err, ErrLinkFault) {
		t.Fatalf("Xfer: %v, want a link fault", err)
	}
	if got := b.SerialErrors(); got != 1 || len(b.owed) != 0 {
		t.Fatalf("serial errors=%d owed=%d, want 1 and 0", got, len(b.owed))
	}
	if d, err := b.Xfer([]byte{0x80, 0x00}); err != nil || !bytes.Equal(d, []byte{1, 2}) {
		t.Fatalf("next Xfer: %x %v", d, err)
	}
}

func TestUnconfiguredBridgeAnswersErrUnconfigured(t *testing.T) {
	b := &Bridge{port: &fakePort{read: autoAckResponse(statusUnconfigured, nil)}, timeout: time.Second}
	if _, err := b.RecvAutoAck(250*time.Millisecond, make([]byte, 8), 0); !errors.Is(err, ErrUnconfigured) {
		t.Fatalf("RecvAutoAck: %v, want ErrUnconfigured", err)
	}
	b = &Bridge{port: &fakePort{read: autoAckResponse(statusUnconfigured, nil)}, timeout: time.Second}
	if err := b.EnqueueCSLBatch([]CSLQueueRequest{{ID: 1, Frame: cslTxData(), DstShort: []byte{0x0a, 0x01}, AckTimeout: 50 * time.Millisecond}}); !errors.Is(err, ErrUnconfigured) {
		t.Fatalf("EnqueueCSLBatch: %v, want ErrUnconfigured", err)
	}
}

// Configure ends with CMD_CONFIGURED, after the register load and SCAL.
func TestConfigureEndsWithCONFIGURED(t *testing.T) {
	p := &firmwarePort{}
	b := &Bridge{port: p, timeout: time.Second}
	if err := b.Configure(); err != nil {
		t.Fatal(err)
	}
	last := len(p.cmds) - 1
	if p.cmds[last] != cmdConfigured {
		t.Fatalf("last command %#02x, want CONFIGURED", p.cmds[last])
	}
	scal := -1
	for i, w := range p.writes {
		if w[1] == cmdStrobe && w[4] == SCAL {
			scal = i
		}
	}
	if scal < 0 || scal > last {
		t.Fatalf("SCAL at %d, CONFIGURED at %d: want SCAL first", scal, last)
	}
	for i, cmd := range p.cmds[:last] {
		if cmd == cmdConfigured {
			t.Fatalf("CONFIGURED sent early, at %d of %d", i, last)
		}
	}
}

// airtimeBody builds an AIRTIME reply: used and budget in ms, then the buckets.
func airtimeBody(used, budget uint32, buckets [AirtimeBuckets]uint16) []byte {
	body := make([]byte, airtimeReplyLen)
	binary.BigEndian.PutUint32(body[:4], used)
	binary.BigEndian.PutUint32(body[4:8], budget)
	for i, v := range buckets {
		binary.BigEndian.PutUint16(body[8+2*i:], v)
	}
	return body
}

func TestAirtimeQueryReadsTheBuckets(t *testing.T) {
	var want [AirtimeBuckets]uint16
	want[0], want[59], want[60] = 1200, 3, 946
	p := &fakePort{read: autoAckResponse(0, airtimeBody(10000, 100000, want))}
	b := &Bridge{port: p, timeout: time.Second}
	got, err := b.Airtime()
	if err != nil || got.UsedMS != 10000 || got.BudgetMS != 100000 || got.Buckets != want {
		t.Fatalf("Airtime = %+v %v, want 10000 100000 and the buckets", got, err)
	}
	if req := withCRC([]byte{reqMagic, cmdAirtime, 0, 0}); !bytes.Equal(p.writes[0], req) {
		t.Fatalf("request %x, want an empty query %x", p.writes[0], req)
	}
	for _, n := range []int{6, 8, airtimeReplyLen - 2} {
		b = &Bridge{port: &fakePort{read: autoAckResponse(0, airtimeBody(1, 2, want)[:n])}, timeout: time.Millisecond}
		if _, err := b.Airtime(); err == nil {
			t.Fatalf("a %d-byte AIRTIME reply was accepted", n)
		}
	}
}

func TestAirtimePreloadSendsOldestFirstAndReadsTheReply(t *testing.T) {
	var load [AirtimeBuckets]uint16
	load[0], load[1], load[60] = 0x0102, 0x0304, 0x0506
	p := &fakePort{read: autoAckResponse(0, airtimeBody(5000, 100000, load))}
	b := &Bridge{port: p, timeout: time.Second}
	got, err := b.PreloadAirtime(load)
	if err != nil || got.UsedMS != 5000 || got.Buckets != load {
		t.Fatalf("PreloadAirtime = %+v %v", got, err)
	}
	req := p.writes[0]
	payload := req[4 : len(req)-1]
	if req[1] != cmdAirtime || len(payload) != 2*AirtimeBuckets {
		t.Fatalf("request cmd %#02x with %d payload bytes, want AIRTIME with %d", req[1], len(payload), 2*AirtimeBuckets)
	}
	if !bytes.Equal(payload[:4], []byte{1, 2, 3, 4}) || !bytes.Equal(payload[120:], []byte{5, 6}) {
		t.Fatalf("payload %x: want 61 big endian uint16, oldest first", payload)
	}
}

func TestAirtimePreloadRefusedLengthIsAnError(t *testing.T) {
	b := &Bridge{port: &fakePort{read: autoAckResponse(statusBadLen, nil)}, timeout: time.Second}
	if _, err := b.PreloadAirtime([AirtimeBuckets]uint16{}); err == nil {
		t.Fatal("ST_BAD_LEN was accepted as a reading")
	}
}

// A build of v2.11 from before the per-minute buckets answers a query with
// 8 bytes and refuses the preload. The error says to flash the bridge, and it is
// a *FirmwareError, so the daemon stops its retry: only a flash helps.
func TestOldAirtimeFirmwareNamesTheFlash(t *testing.T) {
	banner := "CC1101BRIDGE v2.11 (0.4.0)"
	for name, call := range map[string]struct {
		reply []byte
		do    func(*Bridge) error
	}{
		"query with the 8-byte shape": {autoAckResponse(0, airtimeBody(10000, 100000, [AirtimeBuckets]uint16{})[:8]), func(b *Bridge) error { _, err := b.Airtime(); return err }},
		"preload refused":             {autoAckResponse(0xe3, nil), func(b *Bridge) error { _, err := b.PreloadAirtime([AirtimeBuckets]uint16{}); return err }},
	} {
		t.Run(name, func(t *testing.T) {
			b := &Bridge{port: &fakePort{read: call.reply}, timeout: time.Second, banner: banner}
			err := call.do(b)
			var fw *FirmwareError
			if !errors.As(err, &fw) {
				t.Fatalf("err = %v, want a *FirmwareError", err)
			}
			for _, want := range []string{"v2.11", "AIRTIME", "flash firmware/cc1101_bridge"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q lacks %q", err, want)
				}
			}
		})
	}
}

// The parser of the current firmware refuses a preload whose header lost a byte
// with ST_BAD_LEN, ST_BAD_CMD or ST_BAD_PAYLOAD. That is a link fault that a retry
// fixes, not a firmware that needs a flash.
func TestAirtimePreloadParserRejectIsALinkFaultNotOldFirmware(t *testing.T) {
	for _, status := range []byte{0xe0, statusBadLen} {
		b := &Bridge{port: &fakePort{read: autoAckResponse(status, nil)}, timeout: time.Second, banner: "CC1101BRIDGE v2.11 (0.4.0)"}
		_, err := b.PreloadAirtime([AirtimeBuckets]uint16{})
		var fw *FirmwareError
		if !errors.Is(err, ErrLinkFault) || errors.As(err, &fw) {
			t.Fatalf("status %#02x: err = %v, want a link fault and no *FirmwareError", status, err)
		}
	}
}

// ST_AIRTIME refuses a job or a command: nothing went out, the caller defers it.
func TestAirtimeRefusalMapsToErrAirtimeBudget(t *testing.T) {
	t.Run("enqueue", func(t *testing.T) {
		b := &Bridge{port: &fakePort{read: autoAckResponse(statusAirtime, nil)}, timeout: time.Second}
		err := b.EnqueueCSLBatch([]CSLQueueRequest{{ID: 1, Frame: cslTxData(), DstShort: []byte{0x0a, 0x01}, AckTimeout: 50 * time.Millisecond}})
		if !errors.Is(err, ErrAirtimeBudget) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("csltx", func(t *testing.T) {
		b := &Bridge{port: &fakePort{read: autoAckResponse(statusAirtime, []byte{0})}, timeout: time.Second}
		_, err := b.SendCSLData(cslTxData(), []byte{0x0a, 0x01}, 50*time.Millisecond, time.Second, true, false, 0)
		if !errors.Is(err, ErrAirtimeBudget) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("router advertisement", func(t *testing.T) {
		b := &Bridge{port: &fakePort{read: autoAckResponse(statusAirtime, []byte{0})}, timeout: time.Second}
		_, err := b.SendRouterAdvertBurst([]byte{0x49, 0xe8, 0x42}, time.Second)
		if !errors.Is(err, ErrAirtimeBudget) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("train", func(t *testing.T) {
		b := &Bridge{port: &fakePort{read: autoAckResponse(statusAirtime, nil)}, timeout: time.Second}
		_, err := b.SendTrain([][]byte{{0x25, 0x01}}, time.Second)
		if !errors.Is(err, ErrAirtimeBudget) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("queued completion", func(t *testing.T) {
		body := []byte{0, 0x00, 0x2c, cslTxFlagBootstrap, 0}
		b := &Bridge{port: &fakePort{read: autoAckResponse(statusAirtime, body)}, timeout: time.Second}
		event, err := b.PollCSLQueue()
		if err != nil || event == nil || event.Completion == nil {
			t.Fatalf("event=%+v err=%v", event, err)
		}
		if c := event.Completion; c.ID != 0x2c || !errors.Is(c.Err, ErrAirtimeBudget) || c.Result == nil || c.Result.Sent != 0 {
			t.Fatalf("completion = %+v", c)
		}
	})
}

func TestParseBanner(t *testing.T) {
	for _, tc := range []struct {
		banner       string
		major, minor int
		ok           bool
	}{
		{"CC1101BRIDGE v2.11 (0.4.0)", 2, 11, true},
		{"CC1101BRIDGE v2.9", 2, 9, true},
		{"CC1101BRIDGE v3.0 (x)", 3, 0, true},
		{"CC1101BRIDGE", 0, 0, false},
		{"CC1101BRIDGE vX.1", 0, 0, false},
		{"OTHER v2.11", 0, 0, false},
	} {
		major, minor, ok := ParseBanner(tc.banner)
		if major != tc.major || minor != tc.minor || ok != tc.ok {
			t.Errorf("ParseBanner(%q) = %d %d %t, want %d %d %t", tc.banner, major, minor, ok, tc.major, tc.minor, tc.ok)
		}
	}
}

// Firmware below v2.11 does not check the crc: Open reports it, and the error
// names the level found, the level required and where the flash steps are.
func TestFirmwareBelowTheLevelIsRefused(t *testing.T) {
	for _, tc := range []struct {
		banner string
		accept bool
		found  string
	}{
		{"CC1101BRIDGE v2.10 (0.3.1)", false, "v2.10"},
		{"CC1101BRIDGE v2.9", false, "v2.9"},
		{"CC1101BRIDGE", false, "unknown"},
		{"CC1101BRIDGE v2.11 (0.4.0)", true, ""},
		{"CC1101BRIDGE v2.12 (0.5.0)", true, ""},
		{"CC1101BRIDGE v3.0", true, ""},
	} {
		b := &Bridge{banner: tc.banner}
		err := b.checkFirmware()
		var fw *FirmwareError
		switch {
		case tc.accept && err != nil:
			t.Errorf("%q refused: %v", tc.banner, err)
		case !tc.accept && !errors.As(err, &fw):
			t.Errorf("%q: err = %v, want a FirmwareError", tc.banner, err)
		case !tc.accept:
			for _, want := range []string{tc.found + " found", "v2.11 or later", "docs/OPERATION.md"} {
				if !bytes.Contains([]byte(err.Error()), []byte(want)) {
					t.Errorf("%q: error %q lacks %q", tc.banner, err, want)
				}
			}
		}
	}
}

// The probe works on firmware that does not know the crc: it skips the trailing
// byte, and the banner comes back.
func TestProbeReadsTheBannerOfOldFirmware(t *testing.T) {
	var seen []byte
	p := &linkPort{reply: func(p *linkPort, request []byte) {
		seen = append([]byte(nil), request...)
		if p.currentBaud() == LegacyBaud && request[1] == cmdPing {
			p.feed(autoAckResponse(0, []byte("CC1101BRIDGE v2.9")))
		}
	}}
	b, err := start(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	if b.Banner() != "CC1101BRIDGE v2.9" || len(seen) != 5 {
		t.Fatalf("banner=%q request=%x", b.Banner(), seen)
	}
	var fw *FirmwareError
	if err := b.checkFirmware(); !errors.As(err, &fw) || fw.Found != "v2.9" {
		t.Fatalf("checkFirmware = %v", err)
	}
}

// ST_BAD_PAYLOAD answers a request that lacks the crc byte. Like ST_BAD_CRC it
// means the firmware dropped the request.
func TestBadPayloadIsALostRequest(t *testing.T) {
	b := &Bridge{port: &fakePort{read: autoAckResponse(statusBadPayload, nil)}, timeout: time.Second}
	if _, err := b.Xfer([]byte{0x80, 0x00}); !errors.Is(err, ErrLinkFault) {
		t.Fatalf("Xfer: %v, want a link fault", err)
	}
}

// A chip that has not been configured answers the flush ahead of the reset with
// ST_UNCONFIGURED. Configure goes on: the reset clears the queue.
type unconfiguredFlushPort struct{ firmwarePort }

func (p *unconfiguredFlushPort) Write(data []byte) (int, error) {
	if data[1] == cmdCSLQueue {
		p.fakePort.Write(data)
		p.cmds, p.subs = append(p.cmds, data[1]), append(p.subs, data[4])
		p.read = append(p.read, autoAckResponse(statusUnconfigured, nil)...)
		return len(data), nil
	}
	return p.firmwarePort.Write(data)
}

func TestConfigureSurvivesAnUnconfiguredFlush(t *testing.T) {
	p := &unconfiguredFlushPort{}
	b := &Bridge{port: p, timeout: time.Second}
	if err := b.Configure(); err != nil {
		t.Fatal(err)
	}
	if p.cmds[0] != cmdCSLQueue || p.cmds[1] != cmdReset || p.cmds[len(p.cmds)-1] != cmdConfigured {
		t.Fatalf("commands %x", p.cmds)
	}
}

// A STREAM request that the firmware refuses with ST_UNCONFIGURED ends the
// stream with ErrUnconfigured, instead of scanning for frames until the deadline.
func TestStreamOnAnUnconfiguredChip(t *testing.T) {
	p := &fakePort{read: []byte{rspMagic, statusUnconfigured, 0x00, 0x00}}
	b := &Bridge{port: p, timeout: time.Second}
	started := time.Now()
	if err := b.Stream(time.Second, func(*RxFrame) {}); !errors.Is(err, ErrUnconfigured) {
		t.Fatalf("Stream: %v, want ErrUnconfigured", err)
	}
	if time.Since(started) > 500*time.Millisecond {
		t.Fatalf("Stream took %s", time.Since(started))
	}
}
