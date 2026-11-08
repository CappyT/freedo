package tado

import (
	"maps"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// SharedSpaceMaxLen is the longest accepted shared space name, in characters.
const SharedSpaceMaxLen = 32

// SharedSpace returns the shared space name of zone, trimmed, and false when the
// zone states none. Zones with the same name share one air volume.
func (o Options) SharedSpace(zone int) (string, bool) {
	name, ok := o.SharedSpaces[zone]
	if !ok {
		return "", false
	}
	return strings.TrimSpace(name), true
}

// SharedSpaceNameErr returns why name is not a valid shared space name, or "".
func SharedSpaceNameErr(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "is empty"
	}
	if utf8.RuneCountInString(name) > SharedSpaceMaxLen {
		return "is longer than " + strconv.Itoa(SharedSpaceMaxLen) + " characters"
	}
	for _, r := range name {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != ' ' && r != '-' && r != '_' {
			return "holds a character other than a letter, a digit, a space, - or _"
		}
	}
	return ""
}

func (in *Install) checkSharedSpaces(c *issues) {
	for _, zone := range slices.Sorted(maps.Keys(in.Options.SharedSpaces)) {
		path := "options.shared_space." + strconv.Itoa(zone)
		switch z, err := in.ZoneByNumber(zone); {
		case err != nil:
			c.errf(path, "no zone %d is declared", zone)
		case z.Kind != ZoneHeating:
			c.errf(path, "zone %d is not a heating zone", zone)
		}
		if msg := SharedSpaceNameErr(in.Options.SharedSpaces[zone]); msg != "" {
			c.errf(path, "the name %s", msg)
		}
	}
}
