package daemon

import (
	"bytes"
	"context"
	"log"
	"sync"
	"time"

	"freedo/cc1101"
	"freedo/internal/radio"
)

// airtimeStoreEvery is how often the daemon looks for a new airtime reading of
// the live radio, which reads the firmware about every 10 s.
const airtimeStoreEvery = 10 * time.Second

// airtimeSync remembers the last reading that the daemon stored in this run.
type airtimeSync struct {
	mu      sync.Mutex
	seen    bool
	at      time.Time
	buckets [cc1101.AirtimeBuckets]uint16
}

// shiftAirtime moves the buckets of a reading forward by the whole minutes that
// passed since it: the oldest minutes drop out and the new ones count as empty.
// A gap of the full window or more leaves nothing. A clock that went back moves
// nothing, so the count never shrinks by a wrong clock: syncAirtime then gives
// the array the time of the next reading.
func shiftAirtime(buckets []uint16, elapsed time.Duration) [cc1101.AirtimeBuckets]uint16 {
	var out [cc1101.AirtimeBuckets]uint16
	if len(buckets) != len(out) {
		return out
	}
	minutes := max(int(elapsed/time.Minute), 0)
	if minutes < len(out) {
		copy(out[:], buckets[minutes:])
	}
	return out
}

// storedAirtime returns the buckets to preload into the bridge at now: the last
// reading of the previous run, moved forward by the minutes since, or zeros.
func storedAirtime(store *stateStore, now time.Time) [cc1101.AirtimeBuckets]uint16 {
	if store == nil {
		return [cc1101.AirtimeBuckets]uint16{}
	}
	buckets, at, ok, err := store.Airtime(context.Background())
	switch {
	case err != nil:
		log.Printf("stored airtime: %v: the count starts empty", err)
		return [cc1101.AirtimeBuckets]uint16{}
	case !ok:
		return [cc1101.AirtimeBuckets]uint16{}
	}
	return shiftAirtime(buckets, now.Sub(at))
}

// storeAirtime follows the airtime readings of the live radio, so that a restart
// of the bridge keeps the count. It stores the first reading after the open, and
// each later one only when its buckets changed. It ends with ctx. The shutdown
// calls storeAirtimeFinal after the radio closed.
func (d *daemon) storeAirtime(ctx context.Context) {
	lr, ok := d.radio.(liveRadioHealth)
	if !ok || d.state == nil {
		return
	}
	tick := time.NewTicker(airtimeStoreEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if ctx.Err() == nil {
				d.syncAirtime(ctx, lr)
			}
		}
	}
}

// storeAirtimeFinal stores the last reading once more. The radio reads the
// firmware a last time when it closes, so call it after the radio closed.
func (d *daemon) storeAirtimeFinal() {
	if lr, ok := d.radio.(liveRadioHealth); ok && d.state != nil {
		d.syncAirtime(context.Background(), lr)
	}
}

// airtimeTotal is the sum of the buckets in ms.
func airtimeTotal(buckets []uint16) int {
	n := 0
	for _, v := range buckets {
		n += int(v)
	}
	return n
}

// syncAirtime stores the last reading of the radio when it is the first of this
// run, or when its buckets differ from the last one stored. The array with the
// larger total wins, whole: the stored buckets moved forward by the whole minutes
// since they were stored, or the reading. A tie keeps the stored array: a bridge
// that was just loaded from it reads the same total, and a new time would stop it
// from ageing. An ESP32 that reset alone reports zeros, and those must not erase
// the count that the database holds. A mix of the two arrays bucket by bucket
// would copy the busiest minute into every bucket, because the minute of the
// firmware and the minute of the host do not line up.
//
// The two minutes have different phases, so the host drops its oldest bucket up
// to 59 s after the firmware does. Once this run has read the firmware, the
// firmware holds at least what the stored array holds one minute later, and it
// is the truth: a reading is compared with the stored array moved one more
// minute, so the new airtime of a reading in that gap is never left out.
//
// A kept array keeps the sub-minute rest of its time, so a store every 10 to 50 s
// still ages it. A reading that is older than the stored time means that the wall
// clock went back: the kept array gets the time of the reading and ages from
// there.
func (d *daemon) syncAirtime(ctx context.Context, lr liveRadioHealth) {
	a, known := lr.Airtime()
	if !known {
		return
	}
	s := &d.airtimeSync
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen && (a.At.Equal(s.at) || (a.Buckets == s.buckets && !a.At.Before(s.at))) {
		return
	}
	keep, keepAt := a.Buckets, a.At
	switch stored, at, ok, err := d.state.Airtime(ctx); {
	case err != nil:
		log.Printf("stored airtime: %v: the reading replaces it", err)
	case ok:
		elapsed := a.At.Sub(at)
		held := shiftAirtime(stored, elapsed)
		compare := max(elapsed, 0)
		if s.seen {
			compare += time.Minute
		}
		if later := shiftAirtime(stored, compare); airtimeTotal(later[:]) >= airtimeTotal(a.Buckets[:]) {
			keep, keepAt = held, a.At
			if elapsed >= 0 {
				minutes := int(elapsed / time.Minute)
				if minutes == 0 {
					s.seen, s.at, s.buckets = true, a.At, a.Buckets
					return
				}
				keepAt = at.Add(time.Duration(minutes) * time.Minute)
			}
		}
	}
	if err := d.state.SetAirtime(ctx, keep[:], keepAt); err != nil {
		log.Printf("store airtime: %v", err)
		return
	}
	s.seen, s.at, s.buckets = true, a.At, a.Buckets
}

// plantRadio is a live radio that wants to know the devices of the installation.
type plantRadio interface {
	SetPlant(radio.Plant)
}

// installPlant tells the live radio which frame sources are devices of the
// installation that the daemon serves now.
type installPlant struct{ d *daemon }

func (p installPlant) Devices() int {
	if in := p.d.inst.Load(); in != nil {
		return len(in.Devices)
	}
	return 0
}

// HasSource compares a frame source with the 5-byte prefix of each device: the
// source a Data frame carries.
func (p installPlant) HasSource(source string) bool {
	in := p.d.inst.Load()
	if in == nil {
		return false
	}
	for i := range in.Devices {
		if eui := in.Devices[i].WireEUI(); len(eui) >= 5 && bytes.Equal(eui[:5], []byte(source)) {
			return true
		}
	}
	return false
}
