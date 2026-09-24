package tado

import (
	"maps"
	"slices"
	"strconv"
	"time"
)

const (
	DefaultEarlyStartMaxLeadSec  = 2 * 60 * 60
	DefaultEarlyStartMinEpisodes = 3
	// EarlyStartEpisodesKept is how many heat-up episodes the model of a zone
	// keeps, so it bounds options.early_start_min_episodes too.
	EarlyStartEpisodesKept = 10
)

// EarlyStart reports whether zone starts its warmer program blocks early.
func (o Options) EarlyStart(zone int) bool { return o.EarlyStartZones[zone] }

func (o Options) EarlyStartMaxLead() time.Duration {
	return optSeconds(o.EarlyStartMaxLeadSec, DefaultEarlyStartMaxLeadSec)
}

func (o Options) EarlyStartMinEpisodeCount() int {
	if o.EarlyStartMinEpisodes == nil {
		return DefaultEarlyStartMinEpisodes
	}
	return *o.EarlyStartMinEpisodes
}

func (in *Install) checkEarlyStartOptions(c *issues) {
	if o := in.Options.EarlyStartMaxLeadSec; o != nil && *o <= 0 {
		c.errf("options.early_start_max_lead_sec", "%d is not a positive number of seconds", *o)
	}
	if o := in.Options.EarlyStartMinEpisodes; o != nil && (*o < 1 || *o > EarlyStartEpisodesKept) {
		c.errf("options.early_start_min_episodes", "%d is out of range 1..%d", *o, EarlyStartEpisodesKept)
	}
	for _, zone := range slices.Sorted(maps.Keys(in.Options.EarlyStartZones)) {
		if !in.Options.EarlyStartZones[zone] {
			continue
		}
		path := "options.early_start_zones." + strconv.Itoa(zone)
		switch z, err := in.ZoneByNumber(zone); {
		case err != nil:
			c.errf(path, "no zone %d is declared", zone)
		case z.Kind != ZoneHeating:
			c.errf(path, "zone %d is not a heating zone", zone)
		}
	}
}
