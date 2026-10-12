package tado

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"unicode"
	"unicode/utf8"
)

// HAAreaMaxLen bounds the id and the name of a Home Assistant area, in bytes.
const HAAreaMaxLen = 255

// HAArea is the Home Assistant area of a zone, as the integration reports it.
// It is a display label for the dashboard: the coordinator never acts on it.
type HAArea struct {
	ID   string `yaml:"id" json:"id"`
	Name string `yaml:"name" json:"name"`
}

// Check returns a problem with a, or "" when a is valid.
func (a HAArea) Check() string {
	for _, f := range []struct{ name, value string }{{"id", a.ID}, {"name", a.Name}} {
		switch {
		case f.value == "":
			return f.name + " is empty"
		case len(f.value) > HAAreaMaxLen:
			return fmt.Sprintf("%s is longer than %d bytes", f.name, HAAreaMaxLen)
		case !utf8.ValidString(f.value):
			return f.name + " is not valid UTF-8"
		case slices.ContainsFunc([]rune(f.value), unicode.IsControl):
			return f.name + " holds a control character"
		}
	}
	return ""
}

func (in *Install) checkHAAreas(c *issues) {
	for _, zone := range slices.Sorted(maps.Keys(in.Options.HAAreas)) {
		path := "options.ha_area." + strconv.Itoa(zone)
		if _, err := in.ZoneByNumber(zone); err != nil {
			c.errf(path, "no zone %d is declared", zone)
		}
		if msg := in.Options.HAAreas[zone].Check(); msg != "" {
			c.errf(path, "%s", msg)
		}
	}
}
