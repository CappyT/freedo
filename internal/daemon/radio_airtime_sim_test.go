package daemon

import (
	"context"
	"math/rand"
	"testing"
	"time"

	"freedo/internal/radio"
)

// airtimeSim runs the real syncAirtime and storedAirtime against fwAir, the port of
// the firmware airtime code in radio_airtime_test.go (airPreload: the larger total
// wins). truth is every transmission with its instant, so the airtime of the last
// 60 minutes is known whatever the firmware and the database hold.
type airtimeSim struct {
	t      *testing.T
	store  *stateStore
	d      *daemon
	lr     *storingRadio
	fw     *fwAir
	fwBoot time.Duration // elapsed time at which the firmware clock was zero
	base   time.Time
	skew   time.Duration // wall clock error of the host
	tx     []struct {
		at time.Duration
		us uint32
	}
}

func newAirtimeSim(t *testing.T, fwPhase time.Duration) *airtimeSim {
	store, err := openStateStore(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	lr := &storingRadio{calRadio: newCalRadio(radio.Calibration{}), known: true}
	return &airtimeSim{t: t, store: store, d: &daemon{radio: lr, state: store}, lr: lr, fw: &fwAir{}, fwBoot: -fwPhase, base: time.Unix(1_700_000_000, 0)}
}

func (s *airtimeSim) wall(el time.Duration) time.Time { return s.base.Add(el + s.skew) }

func (s *airtimeSim) clock(el time.Duration) { s.fw.nowUs = uint64((el - s.fwBoot) / time.Microsecond) }

func (s *airtimeSim) send(el time.Duration, frames, n int) {
	s.clock(el)
	for range frames {
		s.fw.count(n)
	}
	s.tx = append(s.tx, struct {
		at time.Duration
		us uint32
	}{el, uint32(frames*(n+11)) * 160})
}

func (s *airtimeSim) train(el time.Duration) { s.send(el, 254, 12); s.send(el, 1, 60) }

// last60 is the airtime of the 60 minutes before el, in ms.
func (s *airtimeSim) last60(el time.Duration) int {
	us := uint64(0)
	for _, e := range s.tx {
		if e.at <= el && el-e.at < 60*time.Minute {
			us += uint64(e.us)
		}
	}
	return int(us / 1000)
}

func (s *airtimeSim) used(el time.Duration) int {
	s.clock(el)
	used, _ := s.fw.query()
	return int(used)
}

func (s *airtimeSim) poll(el time.Duration) {
	s.clock(el)
	used, buckets := s.fw.query()
	s.lr.airtime = radio.Airtime{BudgetMS: 100000, UsedMS: used, Buckets: buckets, At: s.wall(el)}
	s.d.syncAirtime(context.Background(), s.lr)
}

// restart stops the daemon at el and starts it again after down. clean: the radio
// reads the firmware once more and the daemon stores it. reset: the open resets
// the ESP32.
func (s *airtimeSim) restart(el, down time.Duration, clean, reset bool) {
	if clean {
		s.poll(el)
	}
	s.d = &daemon{radio: s.lr, state: s.store}
	el += down
	if reset {
		s.fw = &fwAir{}
		s.fwBoot = el
	}
	s.clock(el)
	s.fw.preload(storedAirtime(s.store, s.wall(el)))
}

// One Router Advertisement burst at 0:30. One hour later the firmware drops that
// minute 47 s before the stored array does, because the two minutes have
// different phases. A second burst goes out in that gap, and the daemon stops
// cleanly 10 s later: the stored array must hold the second burst.
func TestAirtimeCleanStopInThePhaseGapKeepsTheLastBurst(t *testing.T) {
	for _, tc := range []struct {
		name   string
		frames int
		reset  bool
	}{
		{"second burst of 946 ms, the open resets the ESP32", 254, true},
		{"second burst of 946 ms, the ESP32 does not reset", 254, false},
		{"second burst of 920 ms, the ESP32 does not reset", 247, false},
	} {
		s := newAirtimeSim(t, 17*time.Second)
		second := 60*time.Minute + 50*time.Second
		stop := 61 * time.Minute
		for el := time.Duration(0); el <= 70*time.Minute; el += 10 * time.Second {
			if el == 30*time.Second {
				s.train(el)
			}
			if el == second {
				s.send(el, tc.frames, 12)
				s.send(el, 1, 60)
			}
			if el == stop {
				_, at, _, _ := s.store.Airtime(context.Background())
				s.restart(el, 0, true, tc.reset)
				pre := storedAirtime(s.store, s.wall(el))
				t.Logf("%s: clean stop at %s: firmware read %d ms before the stop, stored time %s, the start preloads %d ms, firmware after the preload %d ms",
					tc.name, el, s.lr.airtime.UsedMS, at.Sub(s.base), airtimeTotal(pre[:]), s.used(el))
			}
			s.poll(el)
			if el == stop+2*time.Minute || el == stop+5*time.Minute {
				t.Logf("%s: t=%s firmware count %d ms, airtime of the last 60 min %d ms", tc.name, el, s.used(el), s.last60(el))
			}
		}
		end := 70 * time.Minute
		if got, want := s.used(end), s.last60(end); got < want {
			t.Errorf("%s: at %s the firmware counts %d ms and the radio sent %d ms in the last 60 minutes", tc.name, end, got, want)
		}
	}
}

// Random traffic and random restarts over 24 hours: Enhanced ACKs, one burst of
// 946 ms about every 10 minutes, a clean restart about every 7 minutes that
// takes 0, 10 or 20 s, half of them with an ESP32 reset. The largest distance
// between the firmware count and the airtime of the last 60 minutes, to both sides.
func TestAirtimeRandomRestartsNeverUndercount(t *testing.T) {
	for seed := int64(1); seed <= 8; seed++ {
		rng := rand.New(rand.NewSource(seed))
		s := newAirtimeSim(t, time.Duration(rng.Intn(60))*time.Second)
		under, underAt, over, overAt, restarts := 0, time.Duration(0), 0, time.Duration(0), 0
		downUntil, reset, down := time.Duration(0), false, false
		for el := time.Duration(0); el <= 24*time.Hour; el += 10 * time.Second {
			if down && el >= downUntil {
				s.restart(el, 0, false, reset)
				down = false
			}
			if !down {
				if rng.Intn(2) == 0 {
					s.send(el, 1, 25)
				}
				if rng.Intn(60) == 0 {
					s.train(el)
				}
				s.poll(el)
				if el > 0 && rng.Intn(42) == 0 {
					s.poll(el + time.Second) // the last read of Close and the final store
					down, downUntil, reset = true, el+time.Duration(rng.Intn(3))*10*time.Second, rng.Intn(2) == 0
					restarts++
				}
			}
			if down {
				continue
			}
			used, want := s.used(el), s.last60(el)
			if want-used > under {
				under, underAt = want-used, el
			}
			if used-want > over {
				over, overAt = used-want, el
			}
		}
		t.Logf("seed %d: %d restarts in 24 h: the firmware count is at most %d ms under the airtime of the last 60 min (t=%s) and at most %d ms over it (t=%s)",
			seed, restarts, under, underAt, over, overAt)
		if under > 50 { // rounding of the buckets to milliseconds
			t.Errorf("seed %d: the firmware count is %d ms under the airtime of the last 60 minutes", seed, under)
		}
	}
}

// One busy hour: a burst every 40 s, 85 % of the budget. Then the wall clock of
// the host steps back by 6 hours and the radio sends nothing more. The daemon
// restarts 2, 4 and 5 hours later, with an ESP32 reset.
func TestAirtimeClockStepBackAgesTheStoredCount(t *testing.T) {
	s := newAirtimeSim(t, 17*time.Second)
	step := time.Hour
	for el := time.Duration(0); el <= 9*time.Hour; el += 10 * time.Second {
		if el < step && el%(40*time.Second) == 0 {
			s.train(el)
		}
		if el == step {
			s.skew = -6 * time.Hour
		}
		for _, h := range []time.Duration{3, 5, 6, 8} {
			if el == h*time.Hour {
				before := s.used(el)
				s.restart(el, 0, true, true)
				t.Logf("restart %s after the clock step: the firmware held %d ms, the airtime of the last 60 min is %d ms, the preload gives %d ms (%.0f%% of the budget)",
					el-step, before, s.last60(el), s.used(el), float64(s.used(el))/1000)
				if el < 7*time.Hour && s.used(el) > 80000 && s.last60(el) == 0 {
					t.Errorf("%s after the last transmission a restart preloads %d ms", el-step, s.used(el))
				}
			}
		}
		s.poll(el)
	}
}

// Checks with no finding: a stored array of 60 and of 62 minutes, an ESP32 that
// resets alone in the run, and a forward step of the wall clock.
func TestAirtimeAgeLimitsResetAloneAndForwardStep(t *testing.T) {
	for _, down := range []time.Duration{59 * time.Minute, 60 * time.Minute, 62 * time.Minute} {
		s := newAirtimeSim(t, 17*time.Second)
		s.train(30 * time.Second)
		s.poll(30 * time.Second)
		s.restart(40*time.Second, down, true, true)
		el := 40*time.Second + down
		s.poll(el)
		t.Logf("one burst at 0:30, daemon down for %s, start with reset: preload gives %d ms, airtime of the last 60 min %d ms", down, s.used(el), s.last60(el))
		if s.used(el) < s.last60(el) {
			t.Errorf("down %s: the firmware counts %d ms under the last 60 min", down, s.last60(el)-s.used(el))
		}
	}

	s := newAirtimeSim(t, 17*time.Second)
	for el := time.Duration(0); el <= 20*time.Minute; el += 10 * time.Second {
		if el%(100*time.Second) == 0 {
			s.train(el)
		}
		s.poll(el)
	}
	el := 20*time.Minute + 10*time.Second
	before := s.used(el)
	s.fw, s.fwBoot = &fwAir{}, el // the ESP32 resets alone
	s.poll(el)                    // a reading of zeros
	stored := storedAirtime(s.store, s.wall(el))
	s.restart(el+10*time.Second, 0, true, false)
	t.Logf("ESP32 reset alone: firmware held %d ms, then reads %d ms; the database keeps %d ms; the next start preloads %d ms", before, 0, airtimeTotal(stored[:]), s.used(el+10*time.Second))
	if s.used(el+10*time.Second) < before-946 {
		t.Errorf("the count after the reset is %d ms, %d ms before", s.used(el+10*time.Second), before)
	}

	s = newAirtimeSim(t, 17*time.Second)
	for el := time.Duration(0); el <= 30*time.Minute; el += 10 * time.Second {
		if el%(100*time.Second) == 0 {
			s.train(el)
		}
		if el == 20*time.Minute {
			s.skew = 3 * time.Hour
		}
		s.poll(el)
	}
	el = 30 * time.Minute
	pre := storedAirtime(s.store, s.wall(el))
	t.Logf("forward step of 3 h at 20 min: at 30 min the firmware holds %d ms and a restart preloads %d ms", s.used(el), airtimeTotal(pre[:]))
	if d := s.used(el) - airtimeTotal(pre[:]); d > 946 || d < -946 {
		t.Errorf("forward step: database %d ms, firmware %d ms", airtimeTotal(pre[:]), s.used(el))
	}
}
