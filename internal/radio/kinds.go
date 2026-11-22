package radio

import (
	"sync"

	"freedo/cc1101"
	"freedo/tado"
)

// KindCount is what the radio put on air for one kind of downlink since the
// start: the jobs that sent at least one frame, and their time on air. The time
// is an estimate from the frame counts and sizes, as the firmware counts a frame
// (preamble, sync, length, payload and CRC at 160 us a byte). The count of the
// firmware, in Airtime, is the total that counts.
type KindCount struct {
	Attempts  uint64
	AirtimeMS float64
}

const (
	// airByteUS and airFrameOverhead are the airtime model of the firmware.
	airByteUS        = 160
	airFrameOverhead = 4 + 4 + 1 + 2
	// cslWakeFrameLen is the length of a CSL wake-up frame.
	cslWakeFrameLen = 12
)

// airFrameMS is the time on air of one frame of n bytes, in ms.
func airFrameMS(n int) float64 { return float64(n+airFrameOverhead) * airByteUS / 1000 }

// jobAirtimeMS estimates the time on air of a CSL job: Sent counts the wake-up
// frames and the Data frame.
func jobAirtimeMS(dataLen, sent int) float64 {
	return float64(max(sent-1, 0))*airFrameMS(cslWakeFrameLen) + airFrameMS(dataLen)
}

// countJob adds a CSL job to the counter of its kind. A job that sent no frame
// has no time on air and counts for nothing.
func (r *Live) countJob(action tado.Action, result *cc1101.CSLTxResult) {
	if result == nil || result.Sent == 0 {
		return
	}
	r.countKind(action.JobKind(), jobAirtimeMS(len(action.Bytes), result.Sent))
}

func (r *Live) countKind(kind string, ms float64) {
	r.kindMu.Lock()
	defer r.kindMu.Unlock()
	if r.kinds == nil {
		r.kinds = make(map[string]KindCount)
	}
	c := r.kinds[kind]
	c.Attempts++
	c.AirtimeMS += ms
	r.kinds[kind] = c
}

// DownlinkKinds returns the counters by kind of downlink, a copy.
func (r *Live) DownlinkKinds() map[string]KindCount {
	r.kindMu.Lock()
	defer r.kindMu.Unlock()
	out := make(map[string]KindCount, len(r.kinds))
	for k, v := range r.kinds {
		out[k] = v
	}
	return out
}

// kindCounters is embedded in Live.
type kindCounters struct {
	kindMu sync.Mutex
	kinds  map[string]KindCount
}
