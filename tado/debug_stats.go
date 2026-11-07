package tado

import (
	"encoding/binary"
	"errors"
	"math"
	"strconv"
	"time"
)

// Debug statistics come from the read-only functions 10 and 11 of the valve
// resource d/dbg/m (POST d/dbg/m?f=N, docs/PROTOCOL.md "Debug resources").
const (
	DebugStatsStateTime = 10 // RTC2 ticks spent in each firmware state
	DebugStatsCounters  = 11 // radio and MAC counters
)

// DebugStatsTickHz is the RTC2 rate of the state-time accumulators.
const DebugStatsTickHz = 32768

// debugStatsWrap is how long the valve takes to wrap a u32 state-time
// accumulator at DebugStatsTickHz: two readings further apart than this give no
// usable delta.
const debugStatsWrap = (1 << 32) / DebugStatsTickHz * time.Second

// debugStatsSlack is the fixed part of how far the ticks of two readings may
// stray from the clock of the bridge (DebugStatsShareBetween).
const debugStatsSlack = 10 * time.Second

// DebugStatsMoreBlocks is the code freedo records for an answer that announces
// more Block2 blocks. A continuation would need a new POST, which would run the
// function again, so freedo ends the exchange there.
const DebugStatsMoreBlocks = "2.05+more"

// debugStatNames names the tags of functions 10 and 11. A tag the firmware
// never writes (0241, 023f) keeps its raw form.
var debugStatNames = map[string]string{
	// function 10
	"0223": "awake_ticks", "0224": "sleep_ticks", "0226": "tx_ticks", "0227": "rx_ticks",
	"0228": "flash_read_ticks", "0229": "flash_write_ticks", "0232": "report_enqueue_ticks",
	"0245": "crypto_ticks", "022a": "adc_ticks", "022b": "i2c_power_ticks",
	"0231": "radio_isr_ticks", "0251": "node_handler_ticks",
	// function 11
	"0230": "rx_ok", "023b": "rx_crc_fail", "0249": "rx_timeout", "022f": "tx",
	"0233": "tx_no_ack", "0234": "tx_ack", "0235": "mac_error", "0236": "rx_duplicate",
	"0237": "rx_dispatched", "023c": "mic_fail", "0238": "csl_wakeup_seen",
	"0239": "csl_wakeup_accepted", "023a": "csl_activity", "0252": "activity_no_frame",
	"022e": "lowpan_rx", "022d": "lowpan_frag_tx", "024f": "csl_long_wait",
	"0250": "csl_short_wait", "024c": "timer_due_next_tick", "024d": "timer_sleep_armed",
	"024e": "timer_overdue", "0253": "timer_phase_adjust", "023d": "rssi_dBm", "023e": "lqi",
}

// DebugStat is one decoded value of a debug statistics answer.
type DebugStat struct {
	Tag   string `json:"tag"`
	Name  string `json:"name,omitempty"`
	Value int64  `json:"value"`
}

// DebugStatsSample is one 2.05 answer of a debug statistics read.
type DebugStatsSample struct {
	At   time.Time
	Body []byte
}

// DebugStatsShare is the part of the valve's time in each state between two
// function 10 readings, in percent.
type DebugStatsShare struct {
	IntervalS float64 `json:"interval_s"`
	AwakePct  float64 `json:"awake_pct"`
	TxPct     float64 `json:"tx_pct"`
	RxPct     float64 `json:"rx_pct"`
}

// DecodeDebugStats decodes the TLV body of a function 10 or 11 answer. Values
// are big-endian and unsigned, except the RSSI, which is a signed 16-bit dBm.
func DecodeDebugStats(body []byte) ([]DebugStat, error) {
	tlvs := parseTLVList(body)
	read := 0
	for _, t := range tlvs {
		read += 3 + len(t.Val)
	}
	if read != len(body) {
		return nil, errors.New("tado: debug statistics body is not a whole TLV stream")
	}
	out := make([]DebugStat, 0, len(tlvs))
	for _, t := range tlvs {
		var v int64
		switch len(t.Val) {
		case 2:
			if t.Tag == "023d" {
				v = int64(int16(binary.BigEndian.Uint16(t.Val)))
			} else {
				v = int64(binary.BigEndian.Uint16(t.Val))
			}
		case 4:
			v = int64(binary.BigEndian.Uint32(t.Val))
		default:
			return nil, errors.New("tado: debug statistic " + t.Tag + " has " + strconv.Itoa(len(t.Val)) + " bytes, want 2 or 4")
		}
		out = append(out, DebugStat{Tag: t.Tag, Name: debugStatNames[t.Tag], Value: v})
	}
	return out, nil
}

// DebugStatsShareBetween gives the state shares between two function 10
// readings, or false when they do not allow it. Function 10 omits an
// accumulator that is 0, so a missing tag reads 0. The accumulators wrap, so
// only the delta of two readings less than one wrap apart is meaningful.
//
// The CPU is either awake or in WFI, so the awake and sleep ticks together
// cover the time between the readings. A restart of the valve zeroes the
// accumulators: their delta then disagrees with the clock of the bridge, and
// the readings are refused. The tolerance absorbs the radio latency of each
// answer and the fold of a running state, which lags by up to 2 s.
func DebugStatsShareBetween(prev, cur DebugStatsSample) (DebugStatsShare, bool) {
	wall := cur.At.Sub(prev.At)
	if wall <= 0 || wall >= debugStatsWrap {
		return DebugStatsShare{}, false
	}
	a, errA := DecodeDebugStats(prev.Body)
	b, errB := DecodeDebugStats(cur.Body)
	if errA != nil || errB != nil {
		return DebugStatsShare{}, false
	}
	delta := func(tag string) float64 {
		var x, y uint32
		for _, s := range a {
			if s.Tag == tag {
				x = uint32(s.Value)
			}
		}
		for _, s := range b {
			if s.Tag == tag {
				y = uint32(s.Value)
			}
		}
		return float64(y - x)
	}
	awake := delta("0223")
	total := awake + delta("0224")
	if total == 0 || math.Abs(total/DebugStatsTickHz-wall.Seconds()) > debugStatsSlack.Seconds()+wall.Seconds()/10 {
		return DebugStatsShare{}, false
	}
	return DebugStatsShare{
		IntervalS: total / DebugStatsTickHz,
		AwakePct:  awake / total * 100,
		TxPct:     delta("0226") / total * 100,
		RxPct:     delta("0227") / total * 100,
	}, true
}

func debugStatsResource(fn int) string { return "d/dbg/m?f=" + strconv.Itoa(fn) }

// EnqueueDebugStats queues POST d/dbg/m?f=fn to a valve, fn 10 or 11. Both
// functions only read; the other functions of the resource write radio
// registers or device state and are not reachable here. The wall thermostat
// registers d/dbg/m too, but its function table is not traced, so it is
// refused.
func (e *EmuState) EnqueueDebugStats(target string, fn int) error {
	if fn != DebugStatsStateTime && fn != DebugStatsCounters {
		return errors.New("tado: debug statistics function " + strconv.Itoa(fn) + ": want 10 or 11")
	}
	wire, _, err := e.resolve(target)
	if err != nil {
		return err
	}
	if e.inst == nil {
		return errors.New("tado: debug statistics need an installation to know the device kind")
	}
	if dev, err := e.inst.Resolve(hexEUI(wire)); err != nil || dev.Kind != KindValve {
		return errors.New("tado: debug statistics are read from valves only")
	}
	resource := debugStatsResource(fn)
	if e.hasPendingGet(wire, resource, nil) {
		return nil
	}
	mid, token, err := downlinkIDs()
	if err != nil {
		return err
	}
	pkt, err := BuildCoAP(CoAPReq{
		Type: "CON", Code: "0.02", MID: mid, Token: token,
		Path: []string{"d", "dbg", "m"}, Query: []string{"f=" + strconv.Itoa(fn)},
		Options: []CoAPOption{optUriPort},
		SrcEUI:  e.selfEUI, DstEUI: wire, SrcPort: bridgeCoAPPort, DstPort: coapPort,
	})
	if err != nil {
		return err
	}
	e.pendGet[midKey{hexEUI(wire), mid}] = &activeGet{
		resultKey: activeGetResultKey(wire, resource, nil),
		path:      []string{"d", "dbg", "m"},
		query:     []string{"f=" + strconv.Itoa(fn)},
		token:     append([]byte(nil), token...),
		post:      true,
	}
	if err := e.enqueue(target, "get", "post "+resource, pkt, e.key, mid); err != nil {
		delete(e.pendGet, midKey{hexEUI(wire), mid})
		return err
	}
	return nil
}

// DebugStats returns the last two 2.05 answers of function fn from target,
// newest last, and whether a read is in flight.
func (e *EmuState) DebugStats(target string, fn int) (samples []DebugStatsSample, pending bool, err error) {
	wire, _, err := e.resolve(target)
	if err != nil {
		return nil, false, err
	}
	key := activeGetResultKey(wire, debugStatsResource(fn), nil)
	for _, s := range e.dbgStats[key] {
		samples = append(samples, DebugStatsSample{At: s.At, Body: append([]byte(nil), s.Body...)})
	}
	return samples, e.hasPendingGet(wire, debugStatsResource(fn), nil), nil
}

// recordDebugStats keeps the last two 2.05 answers of a debug statistics read.
func (e *EmuState) recordDebugStats(key string, body []byte) {
	s := append(e.dbgStats[key], DebugStatsSample{At: e.cfg.Now(), Body: append([]byte(nil), body...)})
	if len(s) > 2 {
		s = s[len(s)-2:]
	}
	e.dbgStats[key] = s
}
