package daemon

// program_clock_test: the weekly programs run on the wall clock of the
// installation, across DST transitions.

import (
	"strings"
	"testing"
	"time"

	"freedo/tado"
)

// dayAndMorning is one program for every day: 17 C from midnight, 21 C from
// 07:00.
var dayAndMorning = []tado.ScheduleProgram{{DayMask: 0x7f, Entries: []tado.ScheduleEntry{
	{Minute: 0, SetpointCenti: 1700, Enabled: true},
	{Minute: 420, SetpointCenti: 2100, Enabled: true},
}}}

func mustLoadLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// TestNextScheduleChangeAcrossDST: the 07:00 entry starts at 07:00 on the wall
// clock on the day the clocks move, not seven hours after midnight.
func TestNextScheduleChangeAcrossDST(t *testing.T) {
	rome := mustLoadLocation(t, "Europe/Rome")
	for _, month := range []time.Month{time.March, time.October} {
		day := 29
		if month == time.October {
			day = 25
		}
		at := time.Date(2026, month, day, 1, 0, 0, 0, rome).UTC()
		next, ok := nextScheduleChange(dayAndMorning, at, rome)
		want := time.Date(2026, month, day, 7, 0, 0, 0, rome)
		if !ok || !next.Equal(want) {
			t.Errorf("%s: next change %s, want %s", want.Format(time.DateOnly), next.In(rome), want)
		}
		if block, ok := scheduledBlockAt(dayAndMorning, next, rome); !ok || block.c != 21 {
			t.Errorf("%s: block at the change %+v, want 21 C", want.Format(time.DateOnly), block)
		}
		if block, ok := scheduledBlockAt(dayAndMorning, next.Add(-time.Minute), rome); !ok || block.c != 17 {
			t.Errorf("%s: block before the change %+v, want 17 C", want.Format(time.DateOnly), block)
		}
	}
}

// TestProgramLocation: the programs run on the clock 02b2 gives the devices. A
// fixed utc_offset_min wins over timezone, and an unset pair takes the host's
// zone.
func TestProgramLocation(t *testing.T) {
	at := time.Date(2026, time.July, 1, 5, 30, 0, 0, time.UTC)
	offset := 60
	for _, tc := range []struct {
		name string
		o    tado.Options
		want int
	}{
		{"fixed offset", tado.Options{UTCOffsetMin: &offset, Timezone: "America/New_York"}, 60 * 60},
		{"timezone", tado.Options{Timezone: "America/New_York"}, -4 * 3600},
		{"host", tado.Options{}, func() int { _, s := at.In(time.Local).Zone(); return s }()},
	} {
		if _, got := at.In(programLocation(tc.o)).Zone(); got != tc.want {
			t.Errorf("%s: offset %d s, want %d s", tc.name, got, tc.want)
		}
		if got, want := tc.o.UTCOffsetAt(at)*60, tc.want; got != want {
			t.Errorf("%s: 02b2 %d s disagrees with the program clock %d s", tc.name, got, want)
		}
	}
}

// TestProgramRunsOnInstallClock: the daemon evaluates the programs on the
// installation's clock, not on UTC or on the host's zone. The hot-water week
// runs 45 C to 06:00 and 55 C from it.
func TestProgramRunsOnInstallClock(t *testing.T) {
	for _, tc := range []struct {
		name, option string
		at           time.Time
		want         float64
	}{
		// 05:30 UTC is 06:30 on the devices.
		{"fixed offset", "utc_offset_min: 60\n", time.Date(2026, 1, 5, 5, 30, 0, 0, time.UTC), 55},
		// 06:30 UTC is 01:30 in New York.
		{"timezone", "timezone: America/New_York\n", time.Date(2026, 1, 5, 6, 30, 0, 0, time.UTC), 45},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, err := tado.Parse([]byte(strings.Replace(fixtureInstallYAML, "utc_offset_min: 0\n", tc.option, 1)))
			if err != nil {
				t.Fatal(err)
			}
			saved := defaultInstall
			defaultInstall = in
			t.Cleanup(func() { defaultInstall = saved })
			d, url := newHomeDaemon(t, newTestClock(tc.at))
			putHotWaterWeek(t, url)
			tickHome(t, d)
			v, err := d.do(func(e *tado.EmuState) (any, error) { return d.zoneSched[0], nil })
			if err != nil {
				t.Fatal(err)
			}
			if block := v.(scheduledBlock); block.c != tc.want {
				t.Fatalf("hot-water block %+v, want %g C", block, tc.want)
			}
		})
	}
}
