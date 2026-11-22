package cc1101

import (
	"encoding/binary"
	"errors"
	"testing"
	"time"
)

// The budget follows the listen setting: 1 % of an hour without a listen, 100 s
// with it.
func TestAirtimeBudgetFollowsTheListenSetting(t *testing.T) {
	if got := AirtimeBudget(LBT{}); got != 36000 {
		t.Fatalf("budget without listen = %d, want 36000", got)
	}
	if got := AirtimeBudget(LBT{Enabled: true, ThresholdDBm: -80}); got != 100000 {
		t.Fatalf("budget with listen = %d, want 100000", got)
	}
}

// With a budget, the load is 126 bytes: the buckets and the budget as uint32 big
// endian. It goes out in the same place as the plain load. Without a budget the
// load stays at 122 bytes, so that the firmware keeps its default of no cap.
func TestConfigureSendsTheAirtimeBudgetWithTheLoad(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []ConfigureOption
		want int
	}{
		{"no budget", []ConfigureOption{WithAirtime([AirtimeBuckets]uint16{})}, 2 * AirtimeBuckets},
		{"budget", []ConfigureOption{WithAirtime([AirtimeBuckets]uint16{}), WithAirtimeBudget(36000)}, 2*AirtimeBuckets + 4},
		{"budget without buckets", []ConfigureOption{WithAirtimeBudget(100000)}, 2*AirtimeBuckets + 4},
	} {
		p := &firmwarePort{}
		b := &Bridge{port: p, timeout: time.Second}
		if err := b.Configure(tc.opts...); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		var payload []byte
		for _, w := range p.writes {
			if w[1] == cmdAirtime {
				payload = w[4 : len(w)-1]
			}
		}
		if len(payload) != tc.want {
			t.Fatalf("%s: AIRTIME payload of %d bytes, want %d", tc.name, len(payload), tc.want)
		}
		if tc.want > 2*AirtimeBuckets {
			got := binary.BigEndian.Uint32(payload[2*AirtimeBuckets:])
			if want := map[string]uint32{"budget": 36000, "budget without buckets": 100000}[tc.name]; got != want {
				t.Fatalf("%s: budget %d, want %d", tc.name, got, want)
			}
		}
	}
}

// A firmware that answers the 126-byte load with a bad length twice has no
// budget: the daemon needs a flash, and the error names the firmware.
func TestLoadAirtimeBudgetOnFirmwareWithoutIt(t *testing.T) {
	p := &fakePort{read: append(autoAckResponse(statusBadLen, nil), autoAckResponse(statusBadLen, nil)...)}
	b := &Bridge{port: p, timeout: time.Second, banner: "CC1101BRIDGE v2.11 (0.4.0)"}
	_, err := b.LoadAirtime([AirtimeBuckets]uint16{}, 36000)
	var fw *FirmwareError
	if !errors.As(err, &fw) {
		t.Fatalf("err = %v, want a firmware error", err)
	}
	if len(p.writes) != 2 {
		t.Fatalf("%d requests, want the load and one retry", len(p.writes))
	}
}

// One E1 on the 126-byte load is a lost bit on the way, not a firmware without
// the budget: the load goes out again and works.
func TestLoadAirtimeBudgetRetriesOneBadLength(t *testing.T) {
	read := append(autoAckResponse(statusBadLen, nil), autoAckResponse(0, make([]byte, airtimeReplyLen))...)
	p := &fakePort{read: read}
	b := &Bridge{port: p, timeout: time.Second, banner: "CC1101BRIDGE v2.11 (0.4.0)"}
	if _, err := b.LoadAirtime([AirtimeBuckets]uint16{}, 36000); err != nil {
		t.Fatalf("err = %v, want the retry to work", err)
	}
	if len(p.writes) != 2 {
		t.Fatalf("%d requests, want 2", len(p.writes))
	}
}
