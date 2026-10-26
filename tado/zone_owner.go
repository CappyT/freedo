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
	"bytes"
	"encoding/hex"
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
func ZoneMeasureBody(tempC float64, demandPct, humidityDeci int) []byte {
	centi := int(math.Round(tempC * 100))
	h := min(max(humidityDeci, 0), 1000)
	return encodeTLVs([]TLV{
		{Tag: "4060", Val: []byte{byte(centi >> 8), byte(centi)}},
		{Tag: "40a0", Val: []byte{byte(min(max(demandPct, 0), 100))}},
		{Tag: "0135", Val: []byte{byte(h >> 8), byte(h)}},
	})
}

// DeviceHumidityDeci is the last humidity a device reported, in tenths of a
// percent, for the measure of a zone the coordinator owns.
func (e *EmuState) DeviceHumidityDeci(target string) (int, bool) {
	wire, _, err := e.resolve(target)
	if err != nil {
		return 0, false
	}
	ns, known := e.nodes[hexEUI(wire)]
	if !known || len(ns.raw["0135"]) == 0 {
		return 0, false
	}
	return int(be(ns.raw["0135"])), true
}

// BuildZoneMeasureCoAP builds the zone measure push of the owner to one member,
// PUT z/p?lid=N on the zone plane with the Max-Age the captured pushes carry.
// The token is empty, as in the captured pushes of a valve.
func BuildZoneMeasureCoAP(zone int, tempC float64, demandPct, humidityDeci int, mid uint16, srcEUI, dstEUI []byte) ([]byte, error) {
	return BuildContextCoAP(CoAPReq{
		Type:    "CON",
		Code:    "0.03",
		MID:     mid,
		Token:   []byte{},
		Path:    []string{"z", "p"},
		Query:   []string{"lid=" + strconv.Itoa(zone)},
		Options: []CoAPOption{optOctets, optZoneMeasureAge},
		Payload: ZoneMeasureBody(tempC, demandPct, humidityDeci),
		SrcEUI:  srcEUI,
		DstEUI:  dstEUI,
	})
}

// zoneMeasureKind names the owner's measure push in the queue.
const zoneMeasureKind = "measure"

// EnqueueZoneMeasure queues the measure of zone to target. A member answers the
// push with an ACK 2.05. A measure to the same target and zone that has not
// gone out yet is superseded: the measure is a running value.
func (e *EmuState) EnqueueZoneMeasure(target string, zone int, tempC float64, demandPct, humidityDeci int) error {
	wire, _, err := e.resolve(target)
	if err != nil {
		return err
	}
	if e.inst == nil || !e.inst.Options.CoordinatorOwns(zone) {
		return fmt.Errorf("tado: zone %d: the coordinator does not own it", zone)
	}
	mid := nextMID()
	pkt, err := BuildZoneMeasureCoAP(zone, tempC, demandPct, humidityDeci, mid, e.selfEUI, wire)
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
	Options string // every option of the request, as Label renders it
	Body    string // ZoneMeasureSummary of the payload, empty when there is none
	At      time.Time
	// Registered names an observe registration of the zone measure, and
	// Token its token in hex.
	Registered bool
	Token      string
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
// coordinator owns. A GET of z/p with Observe 0 registers the requester while
// the daemon serves a measure of the zone; servedZoneGet then answers it as the
// first notification.
func (e *EmuState) noteOwnerRequest(m *CoAPMessage, src []byte) {
	zone, ok := e.ownerRequestZone(m, src)
	if !ok {
		return
	}
	obs, observe := m.Option(6)
	registered := observe && obs.Uint() == 0 && m.Code == "0.01" &&
		normalizeResource(m.Path) == "z/p" && len(e.ownedMeasure[zone]) > 0
	if registered {
		e.observeZone(zone, src, m.Token)
	}
	if len(e.ownerSeen) >= ownerRequestsMax {
		e.ownerSeen = e.ownerSeen[1:]
	}
	e.ownerSeen = append(e.ownerSeen, OwnerRequest{
		Zone: zone, Device: ShortFromWire(src), Method: m.Code, URI: m.URI(), Observe: observe,
		Body: ZoneMeasureSummary(m.Payload), At: e.cfg.Now(),
		Registered: registered, Token: hex.EncodeToString(m.Token),
		Options: optionLabels(m.Options),
	})
}

// zoneNotifyEvery bounds the time between two notifications of an unchanged
// measure. The thermostat holds a notification for its Max-Age plus 15 s, then
// GETs again under a new token.
const zoneNotifyEvery = 10 * time.Minute

// zoneObserver is one observe registration of the z/p of an owned zone.
type zoneObserver struct {
	requester []byte
	token     []byte
	seq       uint32    // the Observe value last sent
	body      []byte    // the measure last sent
	at        time.Time // when it was sent
}

// ZoneObserver is an observe registration of the measure of an owned zone,
// for the API and the log.
type ZoneObserver struct {
	Zone       int
	Device     string // short address of the requester
	Target     string // wire EUI hex of the requester
	Token      string // hex
	Seq        uint32
	NotifiedAt time.Time
}

func (s *zoneObserver) view(zone int) ZoneObserver {
	return ZoneObserver{
		Zone: zone, Device: ShortFromWire(s.requester), Target: hexEUI(s.requester),
		Token: hex.EncodeToString(s.token), Seq: s.seq, NotifiedAt: s.at,
	}
}

// observeOption is the Observe option with the sequence number seq.
func observeOption(seq uint32) CoAPOption { return CoAPOption{Num: 6, Val: encodeCoAPUint(seq)} }

// nextObserveSeq is the Observe value after seq, in 24 bits. It skips 0, the
// value of a registration.
func nextObserveSeq(seq uint32) uint32 { return max((seq+1)&0xffffff, 1) }

// observeZone registers src as an observer of the measure of zone under token.
// A new registration of the same requester for the same zone replaces the old
// one, and its sequence numbers keep rising.
func (e *EmuState) observeZone(zone int, src, token []byte) {
	if e.zoneObservers == nil {
		e.zoneObservers = map[int]map[string]*zoneObserver{}
	}
	subs := e.zoneObservers[zone]
	if subs == nil {
		subs = map[string]*zoneObserver{}
		e.zoneObservers[zone] = subs
	}
	s := subs[hexEUI(src)]
	if s == nil {
		s = &zoneObserver{requester: slices.Clone(src)}
		subs[hexEUI(src)] = s
	}
	s.token = append([]byte{}, token...)
	s.seq = nextObserveSeq(s.seq)
	s.body = slices.Clone(e.ownedMeasure[zone])
	s.at = e.cfg.Now()
}

// RestoreZoneObserver adds a registration the daemon stored before a restart,
// and reports whether it took it: the coordinator must still own zone and the
// requester must still belong to it. The registration holds no body, so the
// next NotifyZoneObservers sends the served measure under the next sequence.
func (e *EmuState) RestoreZoneObserver(zone int, requester, token []byte, seq uint32, at time.Time) bool {
	if e.inst == nil || !e.inst.Options.CoordinatorOwns(zone) || !e.zoneInvolves(zone, requester) {
		return false
	}
	if e.zoneObservers == nil {
		e.zoneObservers = map[int]map[string]*zoneObserver{}
	}
	if e.zoneObservers[zone] == nil {
		e.zoneObservers[zone] = map[string]*zoneObserver{}
	}
	e.zoneObservers[zone][hexEUI(requester)] = &zoneObserver{
		requester: slices.Clone(requester), token: slices.Clone(token), seq: seq, at: at,
	}
	return true
}

// ZoneObservers lists the observe registrations of the measure of zone, by
// requester.
func (e *EmuState) ZoneObservers(zone int) []ZoneObserver {
	subs := e.zoneObservers[zone]
	out := make([]ZoneObserver, 0, len(subs))
	for _, key := range slices.Sorted(maps.Keys(subs)) {
		out = append(out, subs[key].view(zone))
	}
	return out
}

// NotifyZoneObservers queues a notification of the served measure of zone to
// each observer whose last one carried another measure or is zoneNotifyEvery
// old, and returns those observers. The notification is a NON 2.05 under the
// token of the registration, so a newer one supersedes it in the queue
// (queueZoneHop).
func (e *EmuState) NotifyZoneObservers(zone int) []ZoneObserver {
	body := e.ownedMeasure[zone]
	if len(body) == 0 {
		return nil
	}
	now := e.cfg.Now()
	var out []ZoneObserver
	for _, key := range slices.Sorted(maps.Keys(e.zoneObservers[zone])) {
		s := e.zoneObservers[zone][key]
		if bytes.Equal(s.body, body) && now.Sub(s.at) < zoneNotifyEvery {
			continue
		}
		seq := nextObserveSeq(s.seq)
		pkt, err := BuildContextCoAP(CoAPReq{
			Type: "NON", Code: "2.05", MID: nextMID(), Token: s.token,
			Options: []CoAPOption{optOctets, observeOption(seq), optZoneMeasureAge},
			Payload: body, SrcEUI: e.selfEUI, DstEUI: s.requester,
		})
		var m *CoAPMessage
		if err == nil {
			m, err = parseCoAPCore(pkt)
		}
		if err != nil {
			e.log.Log(ClassData.String(), "", e.name(s.requester), rsnBuildFail+" z/p notify "+err.Error(), "")
			continue
		}
		e.queueZoneHop(s.requester, m, pkt, fmt.Sprintf("zone notify z/p lid=%d obs=%d tok=%x", zone, seq, s.token), e.selfEUI)
		s.seq, s.body, s.at = seq, slices.Clone(body), now
		out = append(out, s.view(zone))
	}
	return out
}

// pruneZoneObservers drops the registrations of a zone the coordinator no
// longer owns, and those of a requester that is no longer a member or the heat
// source of the zone.
func (e *EmuState) pruneZoneObservers() {
	for zone, subs := range e.zoneObservers {
		for key, s := range subs {
			if !e.inst.Options.CoordinatorOwns(zone) || !e.zoneInvolves(zone, s.requester) {
				delete(subs, key)
			}
		}
		if len(subs) == 0 {
			delete(e.zoneObservers, zone)
		}
	}
}

// TakeOwnerRequests returns the requests recorded since the last call.
func (e *EmuState) TakeOwnerRequests() []OwnerRequest {
	out := e.ownerSeen
	e.ownerSeen = nil
	return out
}

// ownerRequestZone names the zone a zone-plane request to the coordinator is
// about, when the coordinator owns it. A member names the zone in the query.
// The wall thermostat reads the owner's z/p with no query, as a valve owner
// answers for its one zone: the request is then about the owned zone the
// requester serves or belongs to, the one with the highest served demand when
// there are several, since the thermostat takes the maximum anyway.
func (e *EmuState) ownerRequestZone(m *CoAPMessage, src []byte) (int, bool) {
	if e.inst == nil {
		return 0, false
	}
	if zone, ok := zoneQuery(m.Query); ok {
		return zone, e.inst.Options.CoordinatorOwns(zone)
	}
	if normalizeResource(m.Path) != "z/p" {
		return 0, false
	}
	best, bestDemand, found := 0, -1, false
	for i := range e.inst.Zones {
		z := &e.inst.Zones[i]
		if z.Kind != ZoneHeating || !e.inst.Options.CoordinatorOwns(z.Number) {
			continue
		}
		if !e.zoneInvolves(z.Number, src) {
			continue
		}
		demand := 0
		for _, t := range parseTLVList(e.ownedMeasure[z.Number]) {
			if t.Tag == "40a0" && len(t.Val) == 1 {
				demand = int(t.Val[0])
			}
		}
		if demand > bestDemand {
			best, bestDemand, found = z.Number, demand, true
		}
	}
	return best, found
}

// zoneInvolves reports whether the device with wire EUI src is a member or the
// heat source of zone.
func (e *EmuState) zoneInvolves(zone int, src []byte) bool {
	members, err := e.inst.ZoneMembers(zone)
	if err != nil {
		return false
	}
	for _, d := range members {
		if bytes.Equal(d.WireEUI(), src) {
			return true
		}
	}
	if hs, err := e.inst.ZoneHeatSource(zone); err == nil && bytes.Equal(hs.WireEUI(), src) {
		return true
	}
	return false
}

// optionLabels renders every option of a request for the log, so an on-air
// record shows a zero-length Observe or an option the parser has no name for.
func optionLabels(opts []CoAPOption) string {
	out := make([]string, 0, len(opts))
	for _, o := range opts {
		out = append(out, o.Label())
	}
	return strings.Join(out, ",")
}
