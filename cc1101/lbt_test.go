package cc1101

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func TestSetLBTSendsTheSwitchAndTheThreshold(t *testing.T) {
	for _, tc := range []struct {
		lbt  LBT
		want []byte
	}{
		{LBT{Enabled: true, ThresholdDBm: -80}, []byte{1, 0xb0}},
		{LBT{Enabled: false, ThresholdDBm: -80}, []byte{0, 0xb0}},
		{LBT{Enabled: true, ThresholdDBm: -120}, []byte{1, 0x88}},
	} {
		p := &fakePort{read: autoAckResponse(0, tc.want)}
		b := &Bridge{port: p, timeout: time.Second}
		if err := b.SetLBT(tc.lbt); err != nil {
			t.Fatalf("%+v: %v", tc.lbt, err)
		}
		if req := withCRC([]byte{reqMagic, cmdLBT, 0, 2, tc.want[0], tc.want[1]}); !bytes.Equal(p.writes[0], req) {
			t.Fatalf("%+v: request %x, want %x", tc.lbt, p.writes[0], req)
		}
	}
}

func TestSetLBTRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		reply []byte
		check func(error) bool
	}{
		"a firmware without the command": {autoAckResponse(statusBadCmd, nil), func(err error) bool { return errors.Is(err, ErrLBTUnsupported) }},
		"a refused length":               {autoAckResponse(statusBadLen, nil), func(err error) bool { return err != nil && !errors.Is(err, ErrLBTUnsupported) }},
		"a reply that does not echo":     {autoAckResponse(0, []byte{1, 0xb1}), func(err error) bool { return err != nil }},
		"an unconfigured chip":           {autoAckResponse(statusUnconfigured, nil), func(err error) bool { return errors.Is(err, ErrUnconfigured) }},
	} {
		b := &Bridge{port: &fakePort{read: tc.reply}, timeout: time.Second}
		if err := b.SetLBT(LBT{Enabled: true, ThresholdDBm: -80}); !tc.check(err) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// The switch goes out after the airtime load and right before CMD_CONFIGURED.
func TestConfigureSendsLBTAfterTheAirtimeLoad(t *testing.T) {
	p := &firmwarePort{}
	b := &Bridge{port: p, timeout: time.Second}
	if err := b.Configure(WithAirtime([AirtimeBuckets]uint16{}), WithLBT(LBT{Enabled: true, ThresholdDBm: -80})); err != nil {
		t.Fatal(err)
	}
	n := len(p.cmds)
	if p.cmds[n-3] != cmdAirtime || p.cmds[n-2] != cmdLBT || p.cmds[n-1] != cmdConfigured {
		t.Fatalf("last commands %x, want AIRTIME, LBT, CONFIGURED", p.cmds[n-3:])
	}
	if req := p.writes[len(p.writes)-2]; !bytes.Equal(req[4:len(req)-1], []byte{1, 0xb0}) {
		t.Fatalf("LBT payload %x", req[4:len(req)-1])
	}
}

// A firmware with no switch never listens, so a daemon that wants no LBT starts
// on it. A daemon that wants LBT needs a flash.
func TestConfigureWithoutFirmwareSupportForLBT(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		p := &noLBTPort{}
		b := &Bridge{port: p, timeout: time.Second, banner: "CC1101BRIDGE v2.11 (0.4.0)"}
		err := b.Configure(WithLBT(LBT{Enabled: enabled, ThresholdDBm: -80}))
		var fw *FirmwareError
		if enabled != errors.As(err, &fw) || (!enabled && err != nil) {
			t.Fatalf("enabled=%t: err = %v", enabled, err)
		}
		if enabled && p.configured {
			t.Fatal("CONFIGURED sent after the refused switch")
		}
	}
}

// noLBTPort is a firmware that answers CMD_LBT with ST_BAD_CMD.
type noLBTPort struct {
	firmwarePort
	configured bool
}

func (p *noLBTPort) Write(data []byte) (int, error) {
	if data[1] != cmdLBT {
		if data[1] == cmdConfigured {
			p.configured = true
		}
		return p.firmwarePort.Write(data)
	}
	p.fakePort.Write(data)
	p.cmds = append(p.cmds, data[1])
	p.read = append(p.read, autoAckResponse(statusBadCmd, nil)...)
	return len(data), nil
}
