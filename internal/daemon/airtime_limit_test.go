package daemon

import (
	"testing"

	"freedo/cc1101"
)

// configRecorder is a bridge that keeps the options that Configure got.
type configRecorder struct {
	*configBridge
	opts []cc1101.ConfigureOption
}

func (b *configRecorder) Configure(opts ...cc1101.ConfigureOption) error {
	b.opts = opts
	return b.configBridge.Configure(opts...)
}

// The airtime limit reaches both ends from the start: Configure gets the budget
// of the listen setting (36 s without it, 100 s with it) and no budget while the
// limit is off, and the radio that the daemon starts paces the trains only with
// the limit on.
func TestAirtimeLimitReachesTheBridgeAndTheRadio(t *testing.T) {
	for _, tc := range []struct {
		name   string
		set    bridgeSettings
		budget uint32
		limit  int
	}{
		{"off", bridgeSettings{}, 0, 0},
		{"off with listen", bridgeSettings{lbt: cc1101.LBT{Enabled: true, ThresholdDBm: -80}}, 0, 0},
		{"on", bridgeSettings{airtimeLimit: true}, 36000, 4},
		{"on with listen", bridgeSettings{lbt: cc1101.LBT{Enabled: true, ThresholdDBm: -80}, airtimeLimit: true}, 100000, 4},
	} {
		br := &configRecorder{configBridge: &configBridge{tunableLiveBridge: newTunableBridge(cc1101.TuneDefault)}}
		stubBridge(t, br)
		r, err := newLiveRadio(nil, "/dev/null", make([]byte, 8), 0, false, false, tc.set)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		wantSteps := 2
		if tc.budget != 0 {
			wantSteps = 3
		}
		if len(br.opts) != wantSteps {
			t.Errorf("%s: Configure got %d steps, want %d (the budget step only with the limit on)", tc.name, len(br.opts), wantSteps)
		}
		budget, limited := airtimeBudgetFor(tc.set)
		if limited != (tc.budget != 0) || budget != tc.budget {
			t.Errorf("%s: budget %d (limited=%t), want %d", tc.name, budget, limited, tc.budget)
		}
		if got := r.BootstrapLimit(); got != tc.limit {
			t.Errorf("%s: the radio paces %d trains a minute, want %d", tc.name, got, tc.limit)
		}
		r.Close()
	}
}
