package tado

import (
	"encoding/binary"
	"fmt"
	"strconv"
	"time"
)

// DefaultUTCPushSpacingSec is options.utc_push_spacing_sec when unset.
const DefaultUTCPushSpacingSec = 60

// LoadTimezone resolves an IANA zone name for options.timezone.
func LoadTimezone(name string) (*time.Location, error) {
	if name == "" || name == "Local" {
		return nil, fmt.Errorf("%q is not an IANA zone name", name)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("%q is not an IANA zone name: %v", name, err)
	}
	return loc, nil
}

// UTCPushSpacing is the shortest time between two automatic 02b2 writes.
func (o Options) UTCPushSpacing() time.Duration {
	return optSeconds(o.UTCPushSpacingSec, DefaultUTCPushSpacingSec)
}

// Location is the zone tag 02b2 follows: options.timezone, else the host's
// zone. The error names a timezone that does not load; the host's zone stands
// in for it.
func (o Options) Location() (*time.Location, error) {
	if o.Timezone == "" {
		return time.Local, nil
	}
	loc, err := LoadTimezone(o.Timezone)
	if err != nil {
		return time.Local, err
	}
	return loc, nil
}

// UTCOffsetAt is tag 02b2 at t: options.utc_offset_min when set, else the
// offset of Location at t.
func (o Options) UTCOffsetAt(t time.Time) int {
	if o.UTCOffsetMin != nil {
		return *o.UTCOffsetMin
	}
	loc, _ := o.Location()
	return zoneOffsetMin(t, loc)
}

// UTCZone is where tag 02b2 comes from at one instant. Fallback is true when
// it is the host's zone, since neither options.utc_offset_min nor a loadable
// options.timezone is set.
type UTCZone struct {
	Name      string `json:"name"`
	OffsetMin int    `json:"offsetMin"`
	Fallback  bool   `json:"fallback"`
}

// UTCZone returns the source of tag 02b2 at t: the fixed offset, the
// installation's zone, or the host's zone.
func (o Options) UTCZone(t time.Time) UTCZone {
	z := UTCZone{OffsetMin: o.UTCOffsetAt(t)}
	if o.UTCOffsetMin != nil {
		z.Name = "fixed offset"
		return z
	}
	if _, err := o.Location(); err == nil && o.Timezone != "" {
		z.Name = o.Timezone
		return z
	}
	z.Name, _ = t.In(time.Local).Zone()
	z.Fallback = true
	return z
}

func zoneOffsetMin(t time.Time, loc *time.Location) int {
	_, sec := t.In(loc).Zone()
	return sec / 60
}

// loadUTCZone resolves the zone tag 02b2 follows once, since the tick reads it
// every second.
func (e *EmuState) loadUTCZone() {
	e.utcZone = time.Local
	if e.inst == nil {
		return
	}
	loc, err := e.inst.Options.Location()
	if err != nil {
		e.log.Log("Config", "self", "", rsnBuildFail+" timezone "+err.Error(), "")
	}
	e.utcZone = loc
}

// resolveUTCOffset is tag 02b2 at t: the fixed EmuConfig.UTCOffsetMin when set,
// else the offset of the installation's zone at t. Without an installation and
// a fixed value there is none.
func (e *EmuState) resolveUTCOffset(t time.Time) *int {
	if o := e.cfg.UTCOffsetMin; o != nil {
		v := *o
		return &v
	}
	if e.inst == nil {
		return nil
	}
	v := zoneOffsetMin(t, e.utcZone)
	return &v
}

// UTCOffset returns tag 02b2 in force, nil when none is served.
func (e *EmuState) UTCOffset() *int {
	if e.utcOffset == nil {
		return nil
	}
	v := *e.utcOffset
	return &v
}

// SyncUTCOffset re-derives tag 02b2 at now and reports whether it changed, which
// is a DST transition of the zone. On a change the wall thermostat's document
// takes the new offset at once: the device drops a written document, so the
// new ETag gets it the re-read trigger (setConfigDocument). The valves take it
// from PushUTCOffset.
func (e *EmuState) SyncUTCOffset(now time.Time) bool {
	next := e.resolveUTCOffset(now)
	if next == nil || (e.utcOffset != nil && *next == *e.utcOffset) {
		return false
	}
	e.utcOffset = next
	e.refreshThermostatDocuments()
	e.log.Log("Config", "self", "", rsnUTCOffsetChange+"="+strconv.Itoa(*next), "")
	return true
}

// PushUTCOffset queues the 02b2 write for the first declared valve whose
// document lags the offset in force, and returns its wire EUI hex, "" when it
// queues nothing. The valve applies 02b2 at runtime from its d/config record
// handler, so one write is enough and no reboot follows. Each write homes the
// valve's motor, so two pushes stay options.utc_push_spacing_sec apart.
//
// One write per device per offset: the document and utcSettled move at
// enqueue, and both persist with the desired state, so a restart neither
// repeats the push nor skips a valve still behind. A valve with no logical id
// waits until it declares or uses one.
func (e *EmuState) PushUTCOffset(now time.Time) string {
	if e.cfg.NoAutoQueue || e.inst == nil || e.utcOffset == nil {
		return ""
	}
	if !e.utcPushedAt.IsZero() && now.Sub(e.utcPushedAt) < e.inst.Options.UTCPushSpacing() {
		return ""
	}
	off := *e.utcOffset
	for i := range e.inst.Devices {
		dev := &e.inst.Devices[i]
		wire := dev.WireEUI()
		if len(wire) != 8 || e.refusesConfigWrite(wire) {
			continue
		}
		key := hexEUI(wire)
		if v, ok := e.utcSettled[key]; ok && v == off {
			continue
		}
		if configUTCOffset(e.cfgDoc[key], off) {
			e.utcSettled[key] = off
			continue
		}
		id, ok := e.deviceID(wire, e.nodes[key])
		if !ok {
			continue
		}
		e.utcPushedAt = now
		v := off
		if err := e.EnqueueConfig(key, id, ConfigWrite{UTCOffsetMin: &v}); err != nil {
			e.log.Log("Data", "self", dev.Ref(), rsnBuildFail+" utc-offset "+err.Error(), "")
			return ""
		}
		e.log.Log("Data", "self", dev.Ref(), rsnUTCOffsetPush+"="+strconv.Itoa(v), "")
		return key
	}
	return ""
}

func configUTCOffset(snap cfgSnapshot, want int) bool {
	for _, tlv := range parseTLVList(snap.body) {
		if tlv.Tag == "02b2" && len(tlv.Val) == 2 {
			return int(int16(binary.BigEndian.Uint16(tlv.Val))) == want
		}
	}
	return false
}
