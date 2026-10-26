package tado

// zone_owner: the coordinator as the owner of a heating zone. The zone
// document names the owner (tag 63a0) and the measuring endpoint (tag 6040).
// On air the owner runs the zone controller on its own sensor, publishes the
// zone measure on z/p and the other members follow its demand: a second valve
// reports the demand of the first, and a valve follows the wall thermostat
// where that one owns the zone. The commercial wireless temperature sensor
// works this way. With the coordinator as owner the daemon runs the controller
// on an external room temperature and the valves become actuators. No capture
// shows a device with the coordinator as owner: the option is unverified on
// air.

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	// ZoneOwnerDevice is the default: the elected demand reporter owns the zone.
	ZoneOwnerDevice = "device"
	// ZoneOwnerCoordinator makes the daemon the owner of the zone.
	ZoneOwnerCoordinator = "coordinator"
)

// ZoneOwner returns who owns zone: ZoneOwnerDevice or ZoneOwnerCoordinator.
func (o Options) ZoneOwner(zone int) string {
	if v, ok := o.ZoneOwners[zone]; ok && v != "" {
		return v
	}
	return ZoneOwnerDevice
}

// CoordinatorOwns reports whether the daemon owns zone.
func (o Options) CoordinatorOwns(zone int) bool { return o.ZoneOwner(zone) == ZoneOwnerCoordinator }

// CheckZoneOwner reports what is wrong with an owner value, or "".
func CheckZoneOwner(owner string) string {
	switch owner {
	case ZoneOwnerDevice, ZoneOwnerCoordinator:
		return ""
	}
	return "must be " + ZoneOwnerDevice + " or " + ZoneOwnerCoordinator
}

func (in *Install) checkZoneOwners(c *issues) {
	for _, zone := range slices.Sorted(maps.Keys(in.Options.ZoneOwners)) {
		path := "options.zone_owner." + strconv.Itoa(zone)
		owner := in.Options.ZoneOwner(zone)
		if msg := CheckZoneOwner(owner); msg != "" {
			c.errf(path, "%s", msg)
			continue
		}
		if owner != ZoneOwnerCoordinator {
			continue
		}
		switch z, err := in.ZoneByNumber(zone); {
		case err != nil:
			c.errf(path, "no zone %d is declared", zone)
		case z.Kind != ZoneHeating:
			c.errf(path, "zone %d is not a heating zone", zone)
		}
		if in.Options.ZoneOffset(zone).Mode == OffsetModeSetpoint {
			c.errf(path, "zone %d is in offset mode setpoint: the owner regulates on the external reading", zone)
		}
		if in.Options.AutoOffset(zone).Enabled {
			c.errf(path, "zone %d has auto_offset on: the owner regulates on the external reading", zone)
		}
	}
}

// ZoneMeasureBody is the body of a zone measure, tags 4060 (zone temperature)
// and 40a0 (zone demand), in the order a valve pushes them.
func ZoneMeasureBody(tempC float64, demandPct int) []byte {
	centi := int(math.Round(tempC * 100))
	return encodeTLVs([]TLV{
		{Tag: "4060", Val: []byte{byte(centi >> 8), byte(centi)}},
		{Tag: "40a0", Val: []byte{byte(min(max(demandPct, 0), 100))}},
	})
}

// BuildZoneMeasureCoAP builds the zone measure push of the owner to one member,
// PUT z/p?lid=N on the zone plane with the Max-Age the captured pushes carry.
// The token is empty, as in the captured pushes of a valve.
func BuildZoneMeasureCoAP(zone int, tempC float64, demandPct int, mid uint16, srcEUI, dstEUI []byte) ([]byte, error) {
	return BuildContextCoAP(CoAPReq{
		Type:    "CON",
		Code:    "0.03",
		MID:     mid,
		Token:   []byte{},
		Path:    []string{"z", "p"},
		Query:   []string{"lid=" + strconv.Itoa(zone)},
		Options: []CoAPOption{optOctets, optZoneMeasureAge},
		Payload: ZoneMeasureBody(tempC, demandPct),
		SrcEUI:  srcEUI,
		DstEUI:  dstEUI,
	})
}

// zoneMeasureKind names the owner's measure push in the queue.
const zoneMeasureKind = "measure"

// EnqueueZoneMeasure queues the measure of zone to target. A member answers the
// push with an ACK 2.05. A measure to the same target and zone that has not
// gone out yet is superseded: the measure is a running value.
func (e *EmuState) EnqueueZoneMeasure(target string, zone int, tempC float64, demandPct int) error {
	wire, _, err := e.resolve(target)
	if err != nil {
		return err
	}
	if e.inst == nil || !e.inst.Options.CoordinatorOwns(zone) {
		return fmt.Errorf("tado: zone %d: the coordinator does not own it", zone)
	}
	mid := nextMID()
	pkt, err := BuildZoneMeasureCoAP(zone, tempC, demandPct, mid, e.selfEUI, wire)
	if err != nil {
		return err
	}
	key := hexEUI(wire)
	kept := e.queue[key][:0]
	for _, d := range e.queue[key] {
		if d.kind == zoneMeasureKind && d.zone == zone && !d.inFlight {
			e.recordDownlinkOutcome(key, d, "superseded", "newer-measure")
			continue
		}
		kept = append(kept, d)
	}
	e.queue[key] = kept
	desc := fmt.Sprintf("zone measure %.2f C %d%% lid=%d", tempC, demandPct, zone)
	if err := e.enqueue(target, zoneMeasureKind, desc, pkt, e.key, mid); err != nil {
		return err
	}
	q := e.queue[key]
	q[len(q)-1].requireCoAP = true
	q[len(q)-1].responseToken = []byte{}
	q[len(q)-1].responseCode = "2.05"
	q[len(q)-1].zone = zone
	return nil
}

// SetZoneMeasure makes the daemon serve body to a GET z/p?lid=zone instead of
// the measure a member pushed last. ClearZoneMeasure returns the zone to the
// cache.
func (e *EmuState) SetZoneMeasure(zone int, body []byte) {
	if e.ownedMeasure == nil {
		e.ownedMeasure = map[int][]byte{}
	}
	e.ownedMeasure[zone] = body
}

func (e *EmuState) ClearZoneMeasure(zone int) { delete(e.ownedMeasure, zone) }

// OwnerRequest is one zone-plane request a device addressed to the daemon for a
// zone it owns. The daemon logs them: no capture shows what a member asks its
// owner, and the on-air check needs the record.
type OwnerRequest struct {
	Zone    int
	Device  string // short address of the requester
	Method  string // CoAP code of the request
	URI     string
	Observe bool   // the request carries option 6
	Body    string // ZoneMeasureSummary of the payload, empty when there is none
	At      time.Time
}

// ZoneMeasureSummary renders the TLVs of a zone measure for the log: 4060 as
// a temperature, 40a0 as a percent, every other tag in hex.
func ZoneMeasureSummary(body []byte) string {
	var parts []string
	for _, tlv := range parseTLVList(body) {
		switch {
		case tlv.Tag == "4060" && len(tlv.Val) == 2:
			parts = append(parts, fmt.Sprintf("4060=%.2f", float64(int16(be(tlv.Val)))/100))
		case tlv.Tag == "40a0" && len(tlv.Val) == 1:
			parts = append(parts, fmt.Sprintf("40a0=%d%%", tlv.Val[0]))
		default:
			parts = append(parts, fmt.Sprintf("%s=%x", tlv.Tag, tlv.Val))
		}
	}
	return strings.Join(parts, " ")
}

// ownerRequestsMax bounds the record between two drains.
const ownerRequestsMax = 64

// noteOwnerRequest records a zone-plane request addressed to us for a zone the
// coordinator owns.
func (e *EmuState) noteOwnerRequest(m *CoAPMessage, src []byte) {
	zone, ok := zoneQuery(m.Query)
	if !ok || e.inst == nil || !e.inst.Options.CoordinatorOwns(zone) {
		return
	}
	_, observe := m.Option(6)
	if len(e.ownerSeen) >= ownerRequestsMax {
		e.ownerSeen = e.ownerSeen[1:]
	}
	e.ownerSeen = append(e.ownerSeen, OwnerRequest{
		Zone: zone, Device: ShortFromWire(src), Method: m.Code, URI: m.URI(), Observe: observe,
		Body: ZoneMeasureSummary(m.Payload), At: e.cfg.Now(),
	})
}

// TakeOwnerRequests returns the requests recorded since the last call.
func (e *EmuState) TakeOwnerRequests() []OwnerRequest {
	out := e.ownerSeen
	e.ownerSeen = nil
	return out
}
