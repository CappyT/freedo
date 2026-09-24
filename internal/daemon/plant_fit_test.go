package daemon

// plant_fit_test: the FOPDT fit and the PID suggestion. Every episode is
// synthetic, generated from a known model with seeded noise.

import (
	"context"
	"math"
	"math/rand/v2"
	"net/http"
	"testing"

	"freedo/tado"
)

// Model of the synthetic heat-ups: +8 C at 100 % input, a 3 h lag and 15 min
// of dead time.
const (
	synthK     = 0.08
	synthTau   = 10800.0
	synthTheta = 900.0
)

// synthHeat is a 4 h heat-up of that model sampled every 5 min from 18 C: two
// samples at rest, then a proportional input toward 21 C held between
// samples. driven sets the zone's own 40a0 to a third of 4160, the demand of
// another zone on the circuit.
func synthHeat(startMs int64, seed uint64, driven bool) plantEpisode {
	const step, n = 300, 49
	rng := rand.New(rand.NewPCG(seed, 0))
	type change struct{ at, u float64 }
	var changes []change
	x, temp := 0.0, 18.0
	ep := plantEpisode{Kind: plantHeat, StartedMs: startMs}
	for i := range n {
		now := float64(i * step)
		if i > 0 {
			// Integrate the true plant second by second from the last sample.
			for sec := now - step; sec < now; sec++ {
				in := 0.0
				for _, c := range changes {
					if c.at+synthTheta <= sec {
						in = c.u
					}
				}
				a := math.Exp(-1 / synthTau)
				x = x*a + (1-a)*in
			}
		}
		temp = 18 + synthK*x + rng.NormFloat64()*0.02
		temp = math.Round(temp*100) / 100
		u := 0
		if i >= 2 {
			u = int(min(100, max(0, math.Round(60*(21-temp)))))
		}
		changes = append(changes, change{now, float64(u)})
		heat, demand := u, u
		if driven {
			demand = u / 3
		}
		ep.Samples = append(ep.Samples, plantSample{
			AtMs: startMs + int64(now)*1000, TempC: temp, DemandPct: &demand, HeatPct: &heat,
		})
	}
	ep.EndedMs = ep.Samples[n-1].AtMs
	if sec, from, ok := plantDeadTime(ep.Samples); ok {
		ep.DeadTimeSec, ep.DeadTimeFrom = &sec, from
	}
	return ep
}

// synthCool is an 8 h cool-down from 20 C toward 5 C with a 40 h time
// constant, sampled every 10 min, with the outdoor temperature when outdoor.
func synthCool(startMs int64, seed uint64, outdoor bool) plantEpisode {
	rng := rand.New(rand.NewPCG(seed, 1))
	ep := plantEpisode{Kind: plantCool, StartedMs: startMs}
	for i := range 49 {
		t := float64(i * 600)
		temp := 5 + 15*math.Exp(-t/(40*3600)) + rng.NormFloat64()*0.02
		zero := 0
		s := plantSample{AtMs: startMs + int64(t)*1000, TempC: math.Round(temp*100) / 100, DemandPct: &zero}
		if outdoor {
			c := 5.0
			s.OutdoorC = &c
		}
		ep.Samples = append(ep.Samples, s)
	}
	return ep
}

func within(got, want, tol float64) bool { return math.Abs(got-want) <= tol }

func TestPlantFitRecoversFOPDT(t *testing.T) {
	eps := []plantEpisode{synthHeat(0, 1, false), synthHeat(1e8, 2, false), synthHeat(2e8, 3, false)}
	got := suggestPlantPID(3, eps, tado.DefaultZonePID)
	if got.HeatUsed != 3 || !got.Confident {
		t.Fatalf("heat used=%d confident=%t episodes=%+v, want 3 and confident", got.HeatUsed, got.Confident, got.Episodes)
	}
	m := got.Model
	if !within(m.GainCPerPct, synthK, 0.05*synthK) || !within(m.TauSec, synthTau, 0.1*synthTau) ||
		!within(m.DeadTimeSec, synthTheta, 120) {
		t.Errorf("model = %+v, want K=%g tau=%g theta=%g", *m, synthK, synthTau, synthTheta)
	}
	if m.R2 < 0.99 || m.RMSEC > 0.05 {
		t.Errorf("fit quality r2=%g rmse=%g, want r2 >= 0.99 and rmse <= 0.05 C", m.R2, m.RMSEC)
	}
	for _, f := range got.Episodes {
		if f.Heat == nil || !within(f.Heat.GainCPerPct, synthK, 0.2*synthK) {
			t.Errorf("episode %d fit = %+v, want K near %g", f.StartedMs, f.Heat, synthK)
		}
	}
	if got.Suggested == nil || got.Suggested.Check() != "" {
		t.Errorf("suggested = %+v, want valid gains", got.Suggested)
	}
}

func TestPlantFitSkipsDrivenEpisodes(t *testing.T) {
	driven := synthHeat(3e8, 4, true)
	eps := []plantEpisode{synthHeat(0, 1, false), synthHeat(1e8, 2, false), synthHeat(2e8, 3, false), driven}
	got := suggestPlantPID(3, eps, tado.DefaultZonePID)
	if got.HeatUsed != 3 {
		t.Errorf("heat used = %d, want 3", got.HeatUsed)
	}
	f := got.Episodes[3]
	if f.Used || f.Skipped != "driven by another zone" || f.DrivenShare == nil || *f.DrivenShare <= plantDrivenMaxShare {
		t.Errorf("driven episode = %+v, want skipped as driven by another zone", f)
	}
	if f := got.Episodes[0]; f.DrivenShare == nil || *f.DrivenShare != 0 {
		t.Errorf("own episode driven share = %v, want 0", f.DrivenShare)
	}
}

func TestPlantFitNeedsEpisodes(t *testing.T) {
	short := synthHeat(2e8, 3, false)
	short.Samples = short.Samples[:plantFitMinSamples-1]
	noOnset := synthHeat(3e8, 4, false)
	noOnset.Samples = noOnset.Samples[2:]
	got := suggestPlantPID(3, []plantEpisode{synthHeat(0, 1, false), synthHeat(1e8, 2, false), short, noOnset},
		tado.DefaultZonePID)
	if got.HeatUsed != 2 || got.Confident || got.Model == nil || got.Suggested == nil {
		t.Errorf("two usable heat-ups: used=%d confident=%t model=%v suggested=%v, want 2, not confident, a fit",
			got.HeatUsed, got.Confident, got.Model, got.Suggested)
	}
	if got.Episodes[2].Skipped != "too few samples" || got.Episodes[3].Skipped != "no demand onset" {
		t.Errorf("skipped = %q, %q, want too few samples and no demand onset",
			got.Episodes[2].Skipped, got.Episodes[3].Skipped)
	}

	got = suggestPlantPID(3, nil, tado.DefaultZonePID)
	if got.Confident || got.Model != nil || got.Suggested != nil || got.LossPerHour != nil || len(got.Episodes) != 0 {
		t.Errorf("no episodes = %+v, want no fit", got)
	}
}

func TestPlantFitCoolDown(t *testing.T) {
	got := suggestPlantPID(3, []plantEpisode{synthCool(0, 1, true), synthCool(1e8, 2, false)}, tado.DefaultZonePID)
	outdoor, fitted := got.Episodes[0].Cool, got.Episodes[1].Cool
	if outdoor == nil || outdoor.AsymptoteFrom != "outdoor" || outdoor.AsymptoteC != 5 ||
		!within(outdoor.TauSec, 40*3600, 0.1*40*3600) || outdoor.R2 < 0.99 {
		t.Errorf("cool-down toward outdoor = %+v, want tau near 40 h", outdoor)
	}
	if fitted == nil || fitted.AsymptoteFrom != "fitted" || fitted.R2 < 0.99 {
		t.Errorf("cool-down without outdoor = %+v, want a fitted asymptote", fitted)
	}
	if got.CoolUsed != 2 || got.LossPerHour == nil || got.Model != nil {
		t.Errorf("used=%d loss=%v model=%v, want 2 cool-downs, a loss and no heat model",
			got.CoolUsed, got.LossPerHour, got.Model)
	}
}

func TestPlantTune(t *testing.T) {
	cases := []struct {
		name          string
		k, tau, theta float64
		want          tado.ZonePID
	}{
		// tc = 900: Kc = 60 %/C, Ti = 7200 s, Td = 432 s.
		{"lag dominant", 0.1, 10800, 900, tado.ZonePID{Kp: 1200, Ki: 150, Kd: 864}},
		// tc floors at 600 s: Kc = 60 %/C, Ti = 2400 s, Td = 0 -> kd 1.
		{"no dead time", 0.1, 3600, 0, tado.ZonePID{Kp: 1200, Ki: 450, Kd: 1}},
		{"clamped high", 0.001, 172800, 600, tado.ZonePID{Kp: 65535, Ki: 12288, Kd: 32711}},
		{"clamped low", 10, 600, 600, tado.ZonePID{Kp: 1, Ki: 2, Kd: 1}},
	}
	for _, tc := range cases {
		if got := plantTune(tc.k, tc.tau, tc.theta).ZonePID; got != tc.want {
			t.Errorf("%s: gains = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestPlantPIDSuggestionAPI(t *testing.T) {
	_, url, _, _ := newEarlyDaemon(t, false, nil, nil)
	var res map[string]any
	if code := getJSON(t, url+"/api/zones/3/pid-suggestion", &res); code != http.StatusServiceUnavailable {
		t.Errorf("without a store: code=%d %v, want 503", code, res)
	}
	store := newTestStore(t, t.TempDir()+"/state.db")
	d, url, _, _ := newEarlyDaemon(t, false, store, nil)
	if code := getJSON(t, url+"/api/zones/0/pid-suggestion", &res); code != http.StatusBadRequest {
		t.Errorf("hot water zone: code=%d %v, want 400", code, res)
	}
	if code := getJSON(t, url+"/api/zones/4/pid-suggestion", &res); code != http.StatusNotFound {
		t.Errorf("unknown zone: code=%d %v, want 404", code, res)
	}
	for i := range 3 {
		ep := synthHeat(int64(i)*1e8, uint64(i+1), false)
		if err := store.AddPlantEpisode(context.Background(), 3, &ep); err != nil {
			t.Fatal(err)
		}
	}
	var got plantPIDSuggestion
	if code := getJSON(t, url+"/api/zones/3/pid-suggestion", &got); code != http.StatusOK {
		t.Fatalf("zone 3: code=%d", code)
	}
	if got.Zone != 3 || !got.Confident || got.HeatUsed != 3 || got.Suggested == nil ||
		got.Current != tado.DefaultZonePID || len(got.Episodes) != 3 {
		t.Errorf("zone 3 = %+v, want a confident suggestion next to the default gains", got)
	}
	if pid := d.inst.Load().Options.ZonePID(3); pid != tado.DefaultZonePID {
		t.Errorf("served gains = %+v, want them unchanged", pid)
	}
}
