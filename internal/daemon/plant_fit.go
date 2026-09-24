package daemon

// plant_fit: a first-order-plus-dead-time (FOPDT) model of each heating zone,
// fitted on its stored plant episodes, and the valve PID gains the model
// suggests. It only reads: no gain is served or written to a device.
//
// Heat model, in deviation from the first sample of an episode:
//
//	4060(t) = y0 + K*x(t),  tau*dx/dt = u(t-theta) - x
//
// with u the plant input in percent held between reports: 4160, the demand
// the valve motor follows, or 40a0 in a zone that never reported 4160. For a
// fixed (tau, theta) the model is linear in y0 and K; a grid over tau and
// theta keeps the least squares minimum. One joint fit shares K, tau and theta
// over the usable heat-ups, each with its own y0.
//
// Tuning is SIMC (Skogestad 2003) with tc = max(theta, 10 min):
// Kc = tau/(K*(tc+theta)) and Ti = min(tau, 4*(tc+theta)), and the IMC
// derivative time of a FOPDT, Td = tau*theta/(2*tau+theta). The valve computes
// Kp*e/2000 + Ki*integ/30000 - Kd*(T-Tref)/2000 in centi-degrees, with
// integ += e per 60 s tick and Tref the temperature 600 s earlier, so
// Kp = 20*Kc (Kc in %/C), Ki = 900*Kp/Ti and Kd = Kp*Td/600 (Ti, Td in s).

import (
	"math"
	"net/http"
	"slices"

	"freedo/tado"
)

const (
	// plantFitMinSamples is the fewest samples an episode needs to be fitted.
	plantFitMinSamples = 6
	// plantFitMinR2 is the fit quality an episode needs to be used, and the
	// joint fit to be confident.
	plantFitMinR2 = 0.8
	// plantFitMinEpisodes is the usable heat-ups a confident suggestion needs.
	plantFitMinEpisodes = 3
	// A heat-up is driven by another zone when its own 40a0 stays
	// plantDrivenMarginPct below 4160 for more than plantDrivenMaxShare of the
	// time 4160 is above 0: 4160 is the max 40a0 over the circuit.
	plantDrivenMarginPct = 5
	plantDrivenMaxShare  = 0.25
	// The grid bounds of the heat fit and of the cool-down time constant.
	plantFitMaxDeadSec = 7200.0
	plantFitTauMinSec  = 300.0
	plantFitTauMaxSec  = 48 * 3600.0
	plantLossTauMinSec = 3600.0
	plantLossTauMaxSec = 720 * 3600.0
	// Each gain is a uint16 on the wire; 0 is never suggested.
	plantGainMin        = 1
	plantGainMax        = 0xffff
	plantTuneMinTcSec   = 600.0
	plantValveTickSec   = 60.0
	plantValveDerivSec  = 600.0
	plantGridCoarseStep = 300.0
	plantGridFineStep   = 60.0
)

// plantFOPDT is a fitted heat model: K in C per percent of input, tau and
// theta in seconds, and the fit quality on the episodes it covers.
type plantFOPDT struct {
	GainCPerPct float64 `json:"gainCPerPct"`
	TauSec      float64 `json:"tauSec"`
	DeadTimeSec float64 `json:"deadTimeSec"`
	RMSEC       float64 `json:"rmseC"`
	R2          float64 `json:"r2"`
}

// plantLoss is a fitted cool-down: the decay rate toward the asymptote, the
// outdoor temperature when every sample carries one, else a fitted value.
type plantLoss struct {
	PerHour       float64 `json:"perHour"`
	TauSec        float64 `json:"tauSec"`
	AsymptoteC    float64 `json:"asymptoteC"`
	AsymptoteFrom string  `json:"asymptoteFrom"`
	RMSEC         float64 `json:"rmseC"`
	R2            float64 `json:"r2"`
}

// plantEpisodeFit is the fit of one stored episode, and why it was skipped.
type plantEpisodeFit struct {
	Kind        string      `json:"kind"`
	StartedMs   int64       `json:"startedMs"`
	Used        bool        `json:"used"`
	Skipped     string      `json:"skipped,omitempty"`
	Input       string      `json:"input,omitempty"`
	DrivenShare *float64    `json:"drivenShare,omitempty"`
	Heat        *plantFOPDT `json:"heat,omitempty"`
	Cool        *plantLoss  `json:"cool,omitempty"`
}

// plantSuggestion is the gains the tuning rule gives, and its terms in
// standard form.
type plantSuggestion struct {
	tado.ZonePID
	TcSec     float64 `json:"tcSec"`
	KcPctPerC float64 `json:"kcPctPerC"`
	TiSec     float64 `json:"tiSec"`
	TdSec     float64 `json:"tdSec"`
}

type plantPIDSuggestion struct {
	Zone        int               `json:"zone"`
	MinEpisodes int               `json:"minEpisodes"`
	MinR2       float64           `json:"minR2"`
	HeatUsed    int               `json:"heatUsed"`
	CoolUsed    int               `json:"coolUsed"`
	Confident   bool              `json:"confident"`
	Model       *plantFOPDT       `json:"model"`
	LossPerHour *float64          `json:"lossPerHour"`
	Suggested   *plantSuggestion  `json:"suggested"`
	Current     tado.ZonePID      `json:"current"`
	Episodes    []plantEpisodeFit `json:"episodes"`
}

// plantSeries is an episode on the fit axes: seconds from its first sample,
// input percent and 4060.
type plantSeries struct {
	t, u, y []float64
}

func newPlantSeries(ep *plantEpisode, input func(plantSample) *int) plantSeries {
	var s plantSeries
	for _, smp := range ep.Samples {
		s.t = append(s.t, float64(smp.AtMs-ep.Samples[0].AtMs)/1000)
		s.y = append(s.y, smp.TempC)
		if input != nil {
			u := 0.0
			if p := input(smp); p != nil {
				u = float64(*p)
			}
			s.u = append(s.u, u)
		}
	}
	return s
}

// plantResponse returns x at each sample time of a unit-gain first-order lag
// with time constant tau, driven by u held from each sample and delayed by
// theta, starting from rest.
func plantResponse(t, u []float64, tau, theta float64) []float64 {
	out := make([]float64, len(t))
	x, cur, k := 0.0, 0.0, -1
	step := func(to float64) {
		in := 0.0
		if k >= 0 {
			in = u[k]
		}
		a := math.Exp(-(to - cur) / tau)
		x, cur = x*a+(1-a)*in, to
	}
	for j, tj := range t {
		for k+1 < len(t) && t[k+1]+theta <= tj {
			step(t[k+1] + theta)
			k++
		}
		step(tj)
		out[j] = x
	}
	return out
}

// plantMoments returns the centered second moments of x and y.
func plantMoments(x, y []float64) (sxx, sxy, syy float64) {
	var mx, my float64
	for i := range x {
		mx += x[i]
		my += y[i]
	}
	mx /= float64(len(x))
	my /= float64(len(y))
	for i := range x {
		dx, dy := x[i]-mx, y[i]-my
		sxx += dx * dx
		sxy += dx * dy
		syy += dy * dy
	}
	return sxx, sxy, syy
}

// plantGrid minimizes sse over theta in 0..maxTheta and tau in tauMin..tauMax:
// a coarse grid, then a fine one around its minimum.
func plantGrid(maxTheta, tauMin, tauMax float64, sse func(theta, tau float64) (float64, bool)) (float64, float64, bool) {
	bestTheta, bestTau, best, ok := 0.0, 0.0, math.Inf(1), false
	scan := func(th0, th1, thStep, tau0, tau1, ratio float64) {
		for theta := th0; theta <= th1+plantEps; theta += thStep {
			for tau := tau0; tau <= tau1*(1+plantEps); tau *= ratio {
				if v, fit := sse(theta, tau); fit && v < best {
					bestTheta, bestTau, best, ok = theta, tau, v, true
				}
			}
		}
	}
	const coarse, fine = 1.15, 1.01
	scan(0, maxTheta, plantGridCoarseStep, tauMin, tauMax, coarse)
	if !ok {
		return 0, 0, false
	}
	scan(max(0, bestTheta-plantGridCoarseStep), min(maxTheta, bestTheta+plantGridCoarseStep), plantGridFineStep,
		max(tauMin, bestTau/coarse), min(tauMax, bestTau*coarse), fine)
	return bestTheta, bestTau, true
}

// fitPlantHeat fits one FOPDT shared by the series, each with its own y0.
func fitPlantHeat(series []plantSeries) (*plantFOPDT, bool) {
	moments := func(theta, tau float64) (sxx, sxy, syy float64, n int) {
		for _, s := range series {
			xx, xy, yy := plantMoments(plantResponse(s.t, s.u, tau, theta), s.y)
			sxx, sxy, syy, n = sxx+xx, sxy+xy, syy+yy, n+len(s.t)
		}
		return
	}
	theta, tau, ok := plantGrid(plantFitMaxDeadSec, plantFitTauMinSec, plantFitTauMaxSec, func(theta, tau float64) (float64, bool) {
		sxx, sxy, syy, _ := moments(theta, tau)
		if sxx <= plantEps || sxy <= 0 {
			return 0, false
		}
		return syy - sxy*sxy/sxx, true
	})
	if !ok {
		return nil, false
	}
	sxx, sxy, syy, n := moments(theta, tau)
	if syy <= plantEps {
		return nil, false
	}
	sse := max(0, syy-sxy*sxy/sxx)
	return &plantFOPDT{
		GainCPerPct: sxy / sxx, TauSec: tau, DeadTimeSec: theta,
		RMSEC: math.Sqrt(sse / float64(n)), R2: 1 - sse/syy,
	}, true
}

// fitPlantCool fits 4060 = a + c*exp(-t/tau) on a cool-down, with a the mean
// outdoor temperature when outdoorC is not nil, else a fitted asymptote.
func fitPlantCool(s plantSeries, outdoorC *float64) (*plantLoss, bool) {
	fit := func(tau float64) (c, a, sse float64, ok bool) {
		e := make([]float64, len(s.t))
		for i, t := range s.t {
			e[i] = math.Exp(-t / tau)
		}
		if outdoorC != nil {
			var ee, ez, zz float64
			for i := range e {
				z := s.y[i] - *outdoorC
				ee, ez, zz = ee+e[i]*e[i], ez+e[i]*z, zz+z*z
			}
			c = ez / ee
			return c, *outdoorC, max(0, zz-c*ez), c > 0
		}
		see, sey, syy := plantMoments(e, s.y)
		if see <= plantEps {
			return 0, 0, 0, false
		}
		c = sey / see
		var me, my float64
		for i := range e {
			me += e[i]
			my += s.y[i]
		}
		a = (my - c*me) / float64(len(e))
		return c, a, max(0, syy-c*sey), c > 0
	}
	_, tau, ok := plantGrid(0, plantLossTauMinSec, plantLossTauMaxSec, func(_, tau float64) (float64, bool) {
		_, _, sse, ok := fit(tau)
		return sse, ok
	})
	if !ok {
		return nil, false
	}
	_, a, sse, _ := fit(tau)
	_, _, syy := plantMoments(s.y, s.y)
	if syy <= plantEps {
		return nil, false
	}
	from := "fitted"
	if outdoorC != nil {
		from = "outdoor"
	}
	return &plantLoss{
		PerHour: 3600 / tau, TauSec: tau, AsymptoteC: a, AsymptoteFrom: from,
		RMSEC: math.Sqrt(sse / float64(len(s.t))), R2: 1 - sse/syy,
	}, true
}

// plantHeatInput picks the plant input of a heat-up like plantDeadTime does:
// 4160 when the zone reported it, else 40a0.
func plantHeatInput(samples []plantSample) (string, func(plantSample) *int) {
	if slices.ContainsFunc(samples, func(s plantSample) bool { return s.HeatPct != nil }) {
		return "4160", func(s plantSample) *int { return s.HeatPct }
	}
	return "40a0", func(s plantSample) *int { return s.DemandPct }
}

// plantDrivenShare is the share of the time 4160 is above 0 that the zone's
// own 40a0 stays plantDrivenMarginPct below it.
func plantDrivenShare(samples []plantSample) float64 {
	var on, driven float64
	for i := 0; i+1 < len(samples); i++ {
		s := samples[i]
		if s.HeatPct == nil || *s.HeatPct <= 0 {
			continue
		}
		dt := float64(samples[i+1].AtMs - s.AtMs)
		on += dt
		if s.DemandPct != nil && *s.DemandPct < *s.HeatPct-plantDrivenMarginPct {
			driven += dt
		}
	}
	if on == 0 {
		return 0
	}
	return driven / on
}

// suggestPlantPID fits the episodes of zone and maps the heat model to gains.
func suggestPlantPID(zone int, episodes []plantEpisode, current tado.ZonePID) plantPIDSuggestion {
	out := plantPIDSuggestion{
		Zone: zone, MinEpisodes: plantFitMinEpisodes, MinR2: plantFitMinR2, Current: current,
		Episodes: []plantEpisodeFit{},
	}
	var heat []plantSeries
	var losses []float64
	for i := range episodes {
		ep := &episodes[i]
		f := plantEpisodeFit{Kind: ep.Kind, StartedMs: ep.StartedMs}
		switch ep.Kind {
		case plantHeat:
			s := fitPlantHeatEpisode(ep, &f)
			if f.Used {
				heat = append(heat, s)
			}
		case plantCool:
			fitPlantCoolEpisode(ep, &f)
			if f.Used {
				losses = append(losses, f.Cool.PerHour)
			}
		default:
			continue
		}
		out.Episodes = append(out.Episodes, f)
	}
	out.HeatUsed, out.CoolUsed = len(heat), len(losses)
	if len(losses) > 0 {
		slices.Sort(losses)
		mid := len(losses) / 2
		loss := losses[mid]
		if len(losses)%2 == 0 {
			loss = (losses[mid-1] + losses[mid]) / 2
		}
		out.LossPerHour = &loss
	}
	if len(heat) > 0 {
		if m, ok := fitPlantHeat(heat); ok {
			out.Model = m
			sug := plantTune(m.GainCPerPct, m.TauSec, m.DeadTimeSec)
			out.Suggested = &sug
			out.Confident = len(heat) >= plantFitMinEpisodes && m.R2 >= plantFitMinR2
		}
	}
	return out
}

// fitPlantHeatEpisode screens and fits one heat-up into f, and returns its
// series for the joint fit.
func fitPlantHeatEpisode(ep *plantEpisode, f *plantEpisodeFit) plantSeries {
	from, input := plantHeatInput(ep.Samples)
	f.Input = from
	s := newPlantSeries(ep, input)
	if from == "4160" {
		share := plantDrivenShare(ep.Samples)
		f.DrivenShare = &share
	}
	switch {
	case len(ep.Samples) < plantFitMinSamples:
		f.Skipped = "too few samples"
	case s.u[0] > 0 || !slices.ContainsFunc(s.u, func(u float64) bool { return u > 0 }):
		f.Skipped = "no demand onset"
	case f.DrivenShare != nil && *f.DrivenShare > plantDrivenMaxShare:
		f.Skipped = "driven by another zone"
	}
	if f.Skipped != "" {
		return s
	}
	m, ok := fitPlantHeat([]plantSeries{s})
	switch {
	case !ok:
		f.Skipped = "no fit"
	case m.R2 < plantFitMinR2:
		f.Heat, f.Skipped = m, "poor fit"
	default:
		f.Heat, f.Used = m, true
	}
	return s
}

// fitPlantCoolEpisode screens and fits one cool-down into f.
func fitPlantCoolEpisode(ep *plantEpisode, f *plantEpisodeFit) {
	if len(ep.Samples) < plantFitMinSamples {
		f.Skipped = "too few samples"
		return
	}
	var outdoor *float64
	if !slices.ContainsFunc(ep.Samples, func(s plantSample) bool { return s.OutdoorC == nil }) {
		var sum float64
		for _, s := range ep.Samples {
			sum += *s.OutdoorC
		}
		mean := sum / float64(len(ep.Samples))
		outdoor = &mean
	}
	m, ok := fitPlantCool(newPlantSeries(ep, nil), outdoor)
	switch {
	case !ok:
		f.Skipped = "no fit"
	case m.R2 < plantFitMinR2:
		f.Cool, f.Skipped = m, "poor fit"
	default:
		f.Cool, f.Used = m, true
	}
}

// plantTune maps a FOPDT (K in C/%, tau and theta in s) to valve gains.
func plantTune(k, tau, theta float64) plantSuggestion {
	tc := max(theta, plantTuneMinTcSec)
	kc := tau / (k * (tc + theta))
	ti := min(tau, 4*(tc+theta))
	td := tau * theta / (2*tau + theta)
	gain := func(v float64) int {
		return int(min(max(math.Round(v), plantGainMin), plantGainMax))
	}
	kp := gain(20 * kc)
	return plantSuggestion{
		ZonePID: tado.ZonePID{
			Kp: kp,
			Ki: gain(30000 * plantValveTickSec / 2000 * float64(kp) / ti),
			Kd: gain(float64(kp) * td / plantValveDerivSec),
		},
		TcSec: tc, KcPctPerC: kc, TiSec: ti, TdSec: td,
	}
}

// getPIDSuggestion returns the plant fit of a heating zone and the gains it
// suggests, next to the gains the zone serves. It writes nothing.
func (d *daemon) getPIDSuggestion(w http.ResponseWriter, r *http.Request) {
	inst, zone, ok := d.earlyStartZone(w, r)
	if !ok {
		return
	}
	store, ok := d.installStore(w)
	if !ok {
		return
	}
	episodes, err := store.PlantEpisodes(r.Context(), zone)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, suggestPlantPID(zone, episodes, inst.Options.ZonePID(zone)))
}
