package daemon

import (
	"context"
	"testing"
	"time"

	"freedo/internal/radio"
)

// fwAir is a port of the airtime code of firmware/cc1101_bridge/cc1101_bridge.ino
// (airAdvance, airCount, airPreload, the reply loop of CMD_AIRTIME).
type fwAir struct {
	bucket [61]uint32
	minute uint64
	sumUs  uint32
	nowUs  uint64
}

func (f *fwAir) advance() {
	minute := f.nowUs / 60_000_000
	if minute <= f.minute {
		return
	}
	steps := minute - f.minute
	if steps >= 61 {
		f.bucket = [61]uint32{}
		f.sumUs = 0
	} else {
		for i := uint64(1); i <= steps; i++ {
			b := &f.bucket[(f.minute+i)%61]
			f.sumUs -= *b
			*b = 0
		}
	}
	f.minute = minute
}

func (f *fwAir) count(n int) {
	f.advance()
	us := uint32(n+11) * 160
	f.bucket[f.minute%61] += us
	f.sumUs += us
}

func (f *fwAir) train() {
	for range 254 {
		f.count(12)
	}
	f.count(60)
}

func (f *fwAir) preload(p [61]uint16) {
	f.advance()
	var sum uint32
	for k := range 61 {
		sum += uint32(min(p[k], 60000)) * 1000
	}
	if sum <= f.sumUs {
		return
	}
	for k := range 61 {
		age := 60 - k
		f.bucket[(int(f.minute%61)+61-age)%61] = uint32(min(p[k], 60000)) * 1000
	}
	f.sumUs = sum
}

func (f *fwAir) query() (uint32, [61]uint16) {
	f.advance()
	var out [61]uint16
	for k := range 61 {
		age := 60 - k
		out[k] = uint16(f.bucket[(int(f.minute%61)+61-age)%61] / 1000)
	}
	return f.sumUs / 1000, out
}

// One steady run with no restart. The firmware counts one bootstrap train at
// t=30 s and one Enhanced ACK every ackEvery. The radio reads the firmware every
// 10 s and the daemon runs syncAirtime on each reading, as its ticker does. The
// database, which is what the next start preloads, must track the firmware within
// one bucket (the train of 946 ms) at every reading, and a restart must never
// preload more than the budget.
func TestStoredAirtimeAgesWhileTheRadioTransmits(t *testing.T) {
	for _, ackEvery := range []time.Duration{0, 20 * time.Second, 50 * time.Second, 90 * time.Second, 5 * time.Minute} {
		d, lr, store := airtimeTestDaemon(t)
		ctx := context.Background()
		base := time.Unix(1_700_000_000, 0)
		fw := &fwAir{}
		const fwPhase = 17 * time.Second // the firmware clock is not aligned to the wall minute
		lr.known = true
		writes := 0
		var lastAt time.Time
		worst, worstAt, worstFw := 0, time.Duration(0), 0
		gap := 0 // largest distance between the database and the firmware
		trainDone := false
		for el := time.Duration(0); el <= 180*time.Minute; el += 10 * time.Second {
			fw.nowUs = uint64((el + fwPhase) / time.Microsecond)
			if !trainDone && el >= 30*time.Second {
				fw.train()
				trainDone = true
			}
			if ackEvery > 0 && el%ackEvery == 0 {
				fw.count(25)
			}
			used, buckets := fw.query()
			lr.airtime = radio.Airtime{BudgetMS: 100000, UsedMS: used, Buckets: buckets, At: base.Add(el)}
			d.syncAirtime(ctx, lr)
			if _, at, ok, _ := store.Airtime(ctx); ok && !at.Equal(lastAt) {
				writes++
				lastAt = at
			}
			preload := storedAirtime(store, base.Add(el))
			pre := airtimeTotal(preload[:])
			gap = max(gap, max(pre-int(used), int(used)-pre))
			if pre-int(used) > worst-worstFw {
				worst, worstAt, worstFw = pre, el, int(used)
			}
			if el == 59*time.Minute || el == 90*time.Minute || el == 180*time.Minute {
				t.Logf("ack every %-5s t=%-7s firmware used=%5d ms, a restart now preloads %6d ms (%.0f%% of the budget)", ackEvery, el, used, pre, float64(pre)/1000)
			}
		}
		t.Logf("ack every %-5s worst: t=%s firmware %d ms, database %d ms; database writes in 180 min: %d", ackEvery, worstAt, worstFw, worst, writes)
		t.Logf("ack every %-5s largest gap between the database and the firmware: %d ms", ackEvery, gap)
		if gap > 946+30 {
			t.Errorf("ack every %s: the database is %d ms away from the firmware, want within one bucket (946 ms)", ackEvery, gap)
		}
		if worst >= 100000 {
			t.Errorf("ack every %s: a restart at %s preloads %d ms, the firmware holds %d ms", ackEvery, worstAt, worst, worstFw)
		}
	}
}

// A daemon that restarts every restartEvery on a bridge that does not reset (the
// port open does not reset the ESP32) or that resets. One train at t=30 s, nothing
// else. Each start preloads the stored array, each stop reads and stores.
func TestRestartLoopDoesNotInflateTheAirtimeCount(t *testing.T) {
	for _, reset := range []bool{false, true} {
		for _, restartEvery := range []time.Duration{50 * time.Second, 5 * time.Minute} {
			store, err := openStateStore(t.TempDir() + "/state.db")
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			base := time.Unix(1_700_000_000, 0)
			fw := &fwAir{}
			fwBoot := -17 * time.Second // wall offset of the firmware clock zero
			lr := &storingRadio{calRadio: newCalRadio(radio.Calibration{}), known: true}
			d := &daemon{radio: lr, state: store}
			trainDone := false
			peak, peakAt := 0, time.Duration(0)
			gone := time.Duration(-1)
			for el := time.Duration(0); el <= 200*time.Minute; el += 10 * time.Second {
				fw.nowUs = uint64((el - fwBoot) / time.Microsecond)
				if el > 0 && el%restartEvery == 0 {
					// stop: the final read and the final store, then a new process
					used, buckets := fw.query()
					lr.airtime = radio.Airtime{BudgetMS: 100000, UsedMS: used, Buckets: buckets, At: base.Add(el)}
					d.syncAirtime(ctx, lr)
					d = &daemon{radio: lr, state: store}
					if reset {
						fw = &fwAir{}
						fwBoot = el
						fw.nowUs = 0
					}
					fw.preload(storedAirtime(store, base.Add(el)))
				}
				if !trainDone && el >= 30*time.Second {
					fw.train()
					trainDone = true
				}
				used, buckets := fw.query()
				lr.airtime = radio.Airtime{BudgetMS: 100000, UsedMS: used, Buckets: buckets, At: base.Add(el)}
				d.syncAirtime(ctx, lr)
				if int(used) > peak {
					peak, peakAt = int(used), el
				}
				if trainDone && used == 0 && gone < 0 {
					gone = el
				}
			}
			store.Close()
			t.Logf("reset=%-5v restart every %-5s one train of 946 ms at 0:30: the firmware count peaks at %d ms (t=%s) and reaches zero at t=%s (true: 946 ms, gone at about 61 min)", reset, restartEvery, peak, peakAt, gone)
			if peak > 946+30 || gone < 0 || gone > 63*time.Minute {
				t.Errorf("reset=%v, restart every %s: peak %d ms, gone at %s", reset, restartEvery, peak, gone)
			}
		}
	}
}
