package cc1101

import (
	"errors"
	"strings"
	"testing"
)

// firmwareAt answers PING with banner when the port runs at baud, and nothing
// at any other rate, like a firmware that listens on one rate only.
func firmwareAt(baud int, banner string) *linkPort {
	return &linkPort{reply: func(p *linkPort, request []byte) {
		if request[1] == cmdPing && p.currentBaud() == baud {
			p.feed(autoAckResponse(0, []byte(banner)))
		}
	}}
}

func TestOpenKeepsTheRateThatAnswers(t *testing.T) {
	t.Run("new firmware", func(t *testing.T) {
		t.Parallel()
		b, err := start(firmwareAt(DefaultBaud, "CC1101BRIDGE v2.11 (0.3.1)"), 0)
		if err != nil {
			t.Fatal(err)
		}
		if b.Baud() != DefaultBaud || b.Banner() != "CC1101BRIDGE v2.11 (0.3.1)" || b.SerialErrors() != 0 {
			t.Fatalf("baud=%d banner=%q serial errors=%d", b.Baud(), b.Banner(), b.SerialErrors())
		}
	})
	t.Run("old firmware", func(t *testing.T) {
		t.Parallel()
		p := firmwareAt(LegacyBaud, "CC1101BRIDGE v2.9")
		b, err := start(p, 0)
		if err != nil {
			t.Fatal(err)
		}
		if b.Baud() != LegacyBaud || b.Banner() != "CC1101BRIDGE v2.9" || p.currentBaud() != LegacyBaud {
			t.Fatalf("baud=%d banner=%q port rate=%d", b.Baud(), b.Banner(), p.currentBaud())
		}
		if b.SerialErrors() != 0 || len(b.owed) != 0 {
			t.Fatalf("the failed probe left serial errors=%d owed=%d", b.SerialErrors(), len(b.owed))
		}
		if _, err := b.Ping(); err != nil {
			t.Fatalf("ping after the probe: %v", err)
		}
	})
	t.Run("no firmware", func(t *testing.T) {
		t.Parallel()
		_, err := start(firmwareAt(57600, "CC1101BRIDGE"), 0)
		if err == nil {
			t.Fatal("start succeeded on a bridge that answers at neither rate")
		}
		for _, want := range []string{"230400", "921600", "v2.11"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q does not name %s", err, want)
			}
		}
	})
}

// A link that cuts replies can cut the banner of the right rate. The probe asks
// again at that rate, and does not move on to a rate the firmware does not talk.
func TestProbeAsksAgainWhenTheBannerIsCut(t *testing.T) {
	t.Parallel()
	const banner = "CC1101BRIDGE v2.11 (0.3.1)"
	pings := 0
	p := &linkPort{reply: func(p *linkPort, request []byte) {
		if request[1] != cmdPing || p.currentBaud() != DefaultBaud {
			return
		}
		pings++
		full := autoAckResponse(0, []byte(banner))
		if pings <= 2 {
			p.feed(full[:len(full)-1]) // one byte short, as the CH340 delivered it
			return
		}
		p.feed(full)
	}}
	b, err := start(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	if b.Baud() != DefaultBaud || b.Banner() != banner || pings != 3 {
		t.Fatalf("baud=%d banner=%q pings=%d, want %d, the banner and 3 pings", b.Baud(), b.Banner(), pings, DefaultBaud)
	}
	if b.SerialErrors() != 0 {
		t.Fatalf("serial errors=%d: the cut banners of the probe count as no error", b.SerialErrors())
	}
}

// A bridge with old firmware is closed by the daemon. Its queue and transmitter
// must be stopped first: an old firmware has no host timeout, and a bridge that
// a --tx session left armed would go on transmitting after the port closes.
func TestOldFirmwareGetsAFlushAndAResetBeforeTheClose(t *testing.T) {
	var cmds, subs []byte
	p := &linkPort{reply: func(p *linkPort, request []byte) {
		cmds = append(cmds, request[1])
		sub := byte(0)
		if len(request) > 4 {
			sub = request[4]
		}
		subs = append(subs, sub)
		switch request[1] {
		case cmdPing:
			if p.currentBaud() == DefaultBaud {
				p.feed(autoAckResponse(0, []byte("CC1101BRIDGE v2.10 (0.3.0)")))
			}
		case cmdCSLQueue, cmdReset:
			p.feed(autoAckResponse(0, nil))
		}
	}}
	_, err := attach(p, 0)
	var fw *FirmwareError
	if !errors.As(err, &fw) || fw.Found != "v2.10" {
		t.Fatalf("attach err = %v, want a FirmwareError for v2.10", err)
	}
	n := len(cmds)
	if n < 3 || cmds[n-2] != cmdCSLQueue || subs[n-2] != cslQueueFlush || cmds[n-1] != cmdReset {
		t.Fatalf("commands %x subs %x: want the queue flush and the reset last", cmds, subs)
	}
}

func TestCurrentFirmwareIsNotResetByTheOpen(t *testing.T) {
	var cmds []byte
	p := &linkPort{reply: func(p *linkPort, request []byte) {
		cmds = append(cmds, request[1])
		if request[1] == cmdPing && p.currentBaud() == DefaultBaud {
			p.feed(autoAckResponse(0, []byte("CC1101BRIDGE v2.11 (0.4.0)")))
		}
	}}
	if _, err := attach(p, 0); err != nil {
		t.Fatal(err)
	}
	for _, c := range cmds {
		if c != cmdPing {
			t.Fatalf("commands %x: a current bridge gets the PING only", cmds)
		}
	}
}
