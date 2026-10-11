package tado

import (
	"bytes"
	"net/netip"
	"testing"
)

func beaconRA(t *testing.T, in *Install) *RouterAdvert {
	t.Helper()
	a, err := NewEmu(in.EmuConfig(), nil).Beacon()
	if err != nil {
		t.Fatal(err)
	}
	m, err := ParseBroadcast(in.NetworkKey(), a.Bytes)
	if err != nil || m.RA == nil {
		t.Fatalf("beacon does not decode as an advertisement: %v", err)
	}
	return m.RA
}

// TestBeaconRouterIPv4: the advertisement carries the unspecified address while
// the installation sets no gateway and no DNS, and the configured addresses
// otherwise. Set to the captured pair, the options rebuild the captured message.
func TestBeaconRouterIPv4(t *testing.T) {
	in := fixtureInstall(t)
	ra := beaconRA(t, in)
	if !bytes.Equal(ra.Gateway, []byte{0, 0, 0, 0}) || !bytes.Equal(ra.DNS, []byte{0, 0, 0, 0}) {
		t.Errorf("default gateway=%v dns=%v, want 0.0.0.0 and 0.0.0.0", ra.Gateway, ra.DNS)
	}
	if ra.RouterLifetime != 1800 || ra.MTU != 1500 || ra.Netmask != 0xffffffff {
		t.Errorf("lifetime=%d mtu=%d mask=%08x, want 1800 1500 ffffffff", ra.RouterLifetime, ra.MTU, ra.Netmask)
	}

	in.Options.RouterGateway, in.Options.RouterDNS = "10.9.8.1", "10.9.8.2"
	if err := in.Validate(); err != nil {
		t.Fatal(err)
	}
	ra = beaconRA(t, in)
	if !bytes.Equal(ra.Gateway, []byte{10, 9, 8, 1}) || !bytes.Equal(ra.DNS, []byte{10, 9, 8, 2}) {
		t.Errorf("gateway=%v dns=%v, want 10.9.8.1 and 10.9.8.2", ra.Gateway, ra.DNS)
	}

	captured, err := ParseBroadcast(NetworkKey, mustB(capturedRA))
	if err != nil {
		t.Fatal(err)
	}
	gateway, dns := netip.MustParseAddr("192.168.5.1"), netip.MustParseAddr("8.8.8.8")
	rebuilt, err := BuildRouterAdvert(BridgeEUI, BridgeRouterAdvert(BridgeEUI, gateway, dns))
	if err != nil {
		t.Fatal(err)
	}
	if want := captured.Plaintext[2 : len(captured.Plaintext)-2]; !bytes.Equal(rebuilt, want) {
		t.Errorf("rebuilt advertisement\n got %x\nwant %x", rebuilt, want)
	}
}

func TestRouterIPv4Validate(t *testing.T) {
	for _, bad := range []string{"256.1.1.1", "10.0.0", "::1", "::ffff:10.0.0.1", "router", " 10.0.0.1"} {
		for _, set := range []func(*Options){
			func(o *Options) { o.RouterGateway = bad },
			func(o *Options) { o.RouterDNS = bad },
		} {
			in := synthInstall(t)
			set(&in.Options)
			if err := in.Validate(); err == nil {
				t.Errorf("%q: accepted", bad)
			}
		}
	}
	in := synthInstall(t)
	in.Options.RouterGateway, in.Options.RouterDNS = "0.0.0.0", "1.1.1.1"
	if err := in.Validate(); err != nil {
		t.Errorf("valid addresses refused: %v", err)
	}
}
