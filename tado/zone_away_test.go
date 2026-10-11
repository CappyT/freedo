package tado

// zone_away_test: the away temperature of a heating zone on the wire. The
// shapes are the ones of capture-away-20261011; the installation and the
// values are synthetic.

import (
	"encoding/hex"
	"io"
	"strings"
	"testing"
)

const awayTarget = "VA0000000001"

// awayEmu is an emulator on the synthetic installation with the home state
// pushed. Zone 4 is the room and zone 0 the hot water.
func awayEmu(t *testing.T, home bool, away map[int]float64) *EmuState {
	t.Helper()
	e := NewEmu(synthInstall(t).EmuConfig(), NewUnhandledLogger(io.Discard))
	e.SetHomeState(home, map[int]float64{0: 55, 4: 20.5})
	e.SetAwayTemperatures(away)
	return e
}

// awayValve is the wire EUI of the valve of awayTarget.
func awayValve() []byte { return mustHex("3000000000c51b00") }

func firstQueued(t *testing.T, e *EmuState) map[string]string {
	t.Helper()
	got := queuedTags(t, e, hexEUI(awayValve()))
	if len(got) == 0 {
		t.Fatal("nothing queued")
	}
	return got[0]
}

func wantAwayTags(t *testing.T, got, want map[string]string, absent ...string) {
	t.Helper()
	for tag, value := range want {
		if got[tag] != value {
			t.Errorf("%s=%q, want %q (write %v)", tag, got[tag], value, got)
		}
	}
	for _, tag := range absent {
		if value, ok := got[tag]; ok {
			t.Errorf("%s=%q is present (write %v)", tag, value, got)
		}
	}
}

// TestAwaySwitchWriteShapes: the write of a home or away switch to a room zone
// with no overlay, per capture-away-20261011. Away, a zone above the default
// carries its temperature in 6200 with 61e0=1; a zone at the default carries
// 61e0=0 and no 6200. Both carry 6180=0, 6240=0 and 6440. The return home
// restores the program setpoint.
func TestAwaySwitchWriteShapes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		home   bool
		away   map[int]float64
		want   map[string]string
		absent []string
	}{{
		name: "away, 12 C",
		away: map[int]float64{4: 12},
		want: map[string]string{
			"6160": "02", "6180": "00", "6020": "01", "61e0": "01", "6200": "04b0", "6240": "00", "62e0": "00", "6440": "0001",
		},
		absent: []string{"6260", "6280"},
	}, {
		name: "away, zone at the default",
		away: map[int]float64{},
		want: map[string]string{
			"6160": "02", "6180": "00", "6020": "01", "61e0": "00", "6240": "00", "62e0": "00", "6440": "0001",
		},
		absent: []string{"6200", "6260", "6280"},
	}, {
		name: "return home",
		home: true,
		away: map[int]float64{4: 12},
		want: map[string]string{
			"6160": "01", "6180": "00", "6020": "01", "61e0": "01", "6200": "0802", "6240": "00", "62e0": "00", "6440": "0001",
		},
		absent: []string{"6260", "6280"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			e := awayEmu(t, tc.home, tc.away)
			if err := e.EnqueueZoneSwitch(awayTarget, 4); err != nil {
				t.Fatal(err)
			}
			wantAwayTags(t, firstQueued(t, e), tc.want, tc.absent...)
		})
	}
}

// TestAwayRewriteCarriesTheAwayHead: a schedule block change in away restates
// the same away head as the switch, and carries no 6440. An overlay the zone
// runs stays under that head and carries no 6440 on a switch either: the
// overlay tags decide the target while it runs, and the away head is the one
// the zone returns to.
func TestAwayRewriteCarriesTheAwayHead(t *testing.T) {
	away := map[int]float64{4: 12}
	head := map[string]string{"6160": "02", "61e0": "01", "6200": "04b0"}

	e := awayEmu(t, false, away)
	if err := e.EnqueueZoneRewrite(awayTarget, 4); err != nil {
		t.Fatal(err)
	}
	wantAwayTags(t, firstQueued(t, e), head, "6440")

	for _, tc := range []struct {
		name  string
		queue func(*EmuState) error
	}{
		{"switch", func(e *EmuState) error { return e.EnqueueZoneSwitch(awayTarget, 4) }},
		{"rewrite", func(e *EmuState) error { return e.EnqueueZoneRewrite(awayTarget, 4) }},
	} {
		t.Run("overlay, "+tc.name, func(t *testing.T) {
			e := awayEmu(t, false, away)
			e.node(awayValve()).raw = map[string][]byte{"6280": {0x08, 0x34}, "6240": {0x02}, "62e0": {0x00}}
			e.node(awayValve()).refreshLastKnown()
			if err := tc.queue(e); err != nil {
				t.Fatal(err)
			}
			wantAwayTags(t, firstQueued(t, e), map[string]string{
				"6160": "02", "61e0": "01", "6200": "04b0", "6240": "02", "6260": "01", "6280": "0834",
			}, "6440")
		})
	}

	t.Run("new overlay", func(t *testing.T) {
		e := awayEmu(t, false, away)
		if err := e.EnqueueSetpoint(awayTarget, SetpointParams{ID: 4, Setpoint: 22, Overlay: 2}); err != nil {
			t.Fatal(err)
		}
		wantAwayTags(t, firstQueued(t, e), map[string]string{
			"6160": "02", "61e0": "01", "6200": "04b0", "6240": "02", "6260": "01", "6280": "0898", "6440": "0001",
		})
	})

	t.Run("hot water", func(t *testing.T) {
		e := awayEmu(t, false, map[int]float64{0: 38, 4: 12})
		if err := e.EnqueueZoneSwitch("RU0000000001", 0); err != nil {
			t.Fatal(err)
		}
		got := queuedTags(t, e, hexEUI(mustHex("2000000000c51b00")))[0]
		wantAwayTags(t, got, map[string]string{"6160": "02", "61e0": "00", "6240": "00"}, "6200", "6440")
	})
}

// TestAwayReadCarriesTheAwayTemperature: the valve reads its zone block after
// a write and gets the same head, so a read in away restates 61e0=1 and the
// away 6200 and not the program setpoint.
func TestAwayReadCarriesTheAwayTemperature(t *testing.T) {
	in := synthInstall(t)
	for _, tc := range []struct {
		name string
		away map[int]float64
		want string
	}{{
		name: "12 C",
		away: map[int]float64{4: 12},
		want: "61600102" + "61800100" + "60200101" + "61e00101" + "62000204b0" + "62400100" + "62e00100",
	}, {
		name: "default",
		away: map[int]float64{},
		want: "61600102" + "61800100" + "60200101" + "61e00100" + "62400100" + "62e00100",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			e := awayEmu(t, false, tc.away)
			raw := synthDeviceUplink(t, in, awayValve(), CoAPReq{
				Type: "CON", Code: "0.01", MID: 0x7131, Token: []byte{0x07},
				Path: homePath(in, "z", "4", "s"),
			})
			m := synthAnswer(t, in, e.OnFrame(raw), 0x7131)
			if got := hex.EncodeToString(m.Payload); m.Code != "2.05" || got != tc.want {
				t.Errorf("answer=%s body=%s, want 2.05 %s", m.Code, got, tc.want)
			}
		})
	}
}

// TestAwayWriteUpdatesTheZoneCache: a delivered away write leaves its head in
// the zone cache, with the away temperature in 6200 and 61e0=1.
func TestAwayWriteUpdatesTheZoneCache(t *testing.T) {
	e := awayEmu(t, false, map[int]float64{4: 12})
	e.node(awayValve()).raw = map[string][]byte{"6280": {0x08, 0x34}, "6240": {0x00}, "62e0": {0x00}}
	e.node(awayValve()).refreshLastKnown()
	if err := e.EnqueueZoneSwitch(awayTarget, 4); err != nil {
		t.Fatal(err)
	}
	answerQueued(t, e, awayValve(), "2.04")
	cached := parseTLVList(e.zoneState[zoneKey(4, "z/s")])
	for tag, want := range map[string]string{"6160": "02", "61e0": "01", "6200": "04b0"} {
		if got := tlvValue(cached, tag); got != want {
			t.Errorf("cache %s=%q, want %q", tag, got, want)
		}
	}
}

// TestAwayTemperatureGate: the installation gate keeps an away temperature to
// a declared heating zone inside the room setpoint range. The room minimum
// reads as off in away.
func TestAwayTemperatureGate(t *testing.T) {
	for _, tc := range []struct {
		name string
		temp map[int]float64
		want string
	}{
		{"in range", map[int]float64{4: 12.5}, ""},
		{"room minimum", map[int]float64{4: 5}, ""},
		{"room maximum", map[int]float64{4: 25}, ""},
		{"below the range", map[int]float64{4: 4.5}, "out of range"},
		{"above the range", map[int]float64{4: 25.5}, "out of range"},
		{"hot water zone", map[int]float64{0: 38}, "not a heating zone"},
		{"undeclared zone", map[int]float64{9: 12}, "no zone 9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := synthInstall(t)
			in.Options.AwayTemperatures = tc.temp
			err := in.Validate()
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("Validate() = %v, want nil", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("Validate() = %v, want %q", err, tc.want)
			}
		})
	}

	var o Options
	o.AwayTemperatures = map[int]float64{1: 12, 2: 5}
	if c, ok := o.AwayTemperature(1); !ok || c != 12 {
		t.Errorf("AwayTemperature(1) = %g, %t, want 12, true", c, ok)
	}
	for _, zone := range []int{2, 3} {
		if c, ok := o.AwayTemperature(zone); ok {
			t.Errorf("AwayTemperature(%d) = %g, true, want off", zone, c)
		}
	}
}
