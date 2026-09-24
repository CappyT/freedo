package tado

import (
	"sort"
	"time"
)

// Fallback modes, as the air reports them. The wall thermostat enters a
// per-zone fallback when it loses its observe of the zone owner's z/p, and
// leaves it on the first notification of that observe.
//
//	thermostat  PUT h/<home>/z/N/fallback  6460  zone mode, u8
//	any device  PUT d/<serial>/fallback    0182  device mode, u8; valves send 0
//
// The coordinator records the reports and answers 2.04. It never sends 6400,
// 6460 or 0182.
const (
	FallbackNone  = 0    // no fallback
	FallbackHold  = 1    // last demand, local program
	FallbackFixed = 2    // fixed 80 % demand at 25 C
	FallbackFrost = 3    // frost protection, 5 C
	FallbackUnset = 0x7f // 0182 only: no mode set
)

// FallbackName names a fallback mode.
func FallbackName(mode int) string {
	switch mode {
	case FallbackNone:
		return "none"
	case FallbackHold:
		return "hold"
	case FallbackFixed:
		return "fixed"
	case FallbackFrost:
		return "frost"
	case FallbackUnset:
		return "unset"
	}
	return "unknown"
}

// FallbackActive reports whether mode keeps a zone or a device in fallback.
func FallbackActive(mode int) bool { return mode != FallbackNone && mode != FallbackUnset }

// fallbackState is the last reported mode of one zone or one device.
type fallbackState struct {
	mode   int
	source string    // short address of the reporter
	since  time.Time // when the mode last changed
}

// FallbackEvent is one change of a reported fallback mode.
type FallbackEvent struct {
	Zone     int    // -1 for the device-level mode (0182)
	Device   string // short address of the reporter
	From, To int
}

// observeFallback records a fallback report. Only a write carries one: a GET
// answer holds no path to tell the zone.
func (e *EmuState) observeFallback(m *CoAPMessage, tlvs []TLV, ns *NodeState) {
	if m.Code != "0.03" {
		return
	}
	switch normalizeResource(m.Path) {
	case "z/fallback":
		zone, ok := zoneReportPath(m.Path)
		mode, found := fallbackMode(tlvs, "6460")
		if !ok || !found {
			return
		}
		st := e.zoneFallback[zone]
		if st == nil {
			st = &fallbackState{}
			e.zoneFallback[zone] = st
		}
		e.setFallback(st, zone, mode, ns.Short)
	case "d/fallback":
		mode, found := fallbackMode(tlvs, "0182")
		if !found {
			return
		}
		if ns.fallback == nil {
			ns.fallback = &fallbackState{}
		}
		e.setFallback(ns.fallback, -1, mode, ns.Short)
	}
}

func (e *EmuState) setFallback(st *fallbackState, zone, mode int, source string) {
	prev := st.mode
	st.source = source
	if st.since.IsZero() || prev != mode {
		st.since = e.cfg.Now()
	}
	st.mode = mode
	if prev == mode || (!FallbackActive(prev) && !FallbackActive(mode)) {
		return
	}
	e.fallbackEvents = append(e.fallbackEvents, FallbackEvent{Zone: zone, Device: source, From: prev, To: mode})
}

func fallbackMode(tlvs []TLV, tag string) (int, bool) {
	for _, t := range tlvs {
		if t.Tag == tag && len(t.Val) == 1 {
			return int(t.Val[0]), true
		}
	}
	return 0, false
}

// TakeFallbackEvents returns the fallback changes reported since the last call.
func (e *EmuState) TakeFallbackEvents() []FallbackEvent {
	out := e.fallbackEvents
	e.fallbackEvents = nil
	return out
}

// ZoneFallback is the last fallback mode one zone reported.
type ZoneFallback struct {
	Zone, Mode int
	Source     string    // short address of the reporter
	Since      time.Time // when the mode last changed
}

// ZoneFallbacks returns the zones that reported a fallback mode, by number.
func (e *EmuState) ZoneFallbacks() []ZoneFallback {
	out := make([]ZoneFallback, 0, len(e.zoneFallback))
	for zone, st := range e.zoneFallback {
		out = append(out, ZoneFallback{Zone: zone, Mode: st.mode, Source: st.source, Since: st.since})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Zone < out[j].Zone })
	return out
}
