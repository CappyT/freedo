package tado

import (
	"errors"
	"fmt"
	"hash/fnv"
)

const (
	zoneConfigRefreshS   = 900
	zoneConfigEnabled    = 1
	zoneConfigTagKind    = "6020"
	zoneConfigTagOwner   = "63a0"
	zoneConfigTagState   = "63e0"
	zoneConfigTagCircuit = "6060"
	zoneConfigTagKp      = "6080"
	zoneConfigTagKi      = "60a0"
	zoneConfigTagKd      = "60c0"
	zoneConfigTagEnable  = "60e0"
	zoneConfigTagOpen    = "62c0"
	zoneConfigTagRefresh = "6380"
	zoneConfigTagMeasure = "6040"
	zoneConfigTagMembers = "8000"
	zoneConfigTagCPE     = "8200"
	zoneConfigTagReports = "8400"
)

// ZoneConfigDocument is the body and the ETag served at h/<home>/z/N/config.
type ZoneConfigDocument struct {
	Body []byte
	ETag []byte
}

// BuildZoneConfigDocument builds one coordinator-owned zone configuration document.
// It uses the full ASCII IPv6 form the cloud serves for each endpoint.
//
// The capture pins this body: the real coordinator serves h/7654321/z/10/config
// as three RFC 7959 blocks of 128 bytes, the first two 6LoWPAN-fragmented, and
// the 313 reassembled bytes match what this builder emits for zone 10 byte for
// byte. See TestZoneConfigDocumentMatchesCapture. Tag 6060 carries the zone to
// circuit binding, tags 6080, 60a0 and 60c0 the controller gains (ZonePID),
// tags 60e0 and 62c0 the open window detection of the zone.
func BuildZoneConfigDocument(in *Install, zone int) (*ZoneConfigDocument, error) {
	if in == nil {
		return nil, errors.New("tado: zone config: nil install")
	}
	z, err := in.ZoneByNumber(zone)
	if err != nil {
		return nil, err
	}
	members, err := in.ZoneMembers(zone)
	if err != nil {
		return nil, err
	}
	if len(members) == 0 {
		return nil, fmt.Errorf("tado: zone config: zone %d has no members", zone)
	}
	if z.Kind != ZoneHeating && z.Kind != ZoneHotWater {
		return nil, fmt.Errorf("tado: zone config: zone %d has unsupported kind %q", zone, z.Kind)
	}

	owner := zoneConfigOwner(in, z, members)
	endpointMembers := zoneConfigOrderMembers(owner, members)
	// The coordinator as owner: the owner and the measuring endpoint name us,
	// in the same address form the document uses for a device. The members
	// and their report endpoints stay as they are. Unverified on air.
	ownerWire := owner.WireEUI()
	if z.Kind == ZoneHeating && in.Options.CoordinatorOwns(zone) {
		ownerWire = in.CoordinatorEUI()
	}
	membership := endpointMembers
	if z.Kind == ZoneHeating && z.Circuit != nil {
		heatSource, err := in.ZoneHeatSource(zone)
		if err != nil {
			return nil, err
		}
		if !containsDevice(membership, heatSource) {
			membership = append([]*Device{heatSource}, endpointMembers...)
		}
	}

	tls := []TLV{
		{Tag: zoneConfigTagKind, Val: []byte{z.Kind.TLV6020()}},
		{Tag: zoneConfigTagState, Val: []byte{zoneConfigEnabled}},
		{Tag: zoneConfigTagOwner, Val: []byte(zoneConfigEndpointWire(ownerWire, "s"))},
	}

	if z.Kind == ZoneHeating {
		for _, member := range endpointMembers {
			if member.Kind == KindValve {
				tls = append(tls, TLV{Tag: zoneConfigTagCPE, Val: []byte(zoneConfigEndpoint(member, "cpe"))})
			}
		}
	}
	for _, member := range endpointMembers {
		tls = append(tls, TLV{Tag: zoneConfigTagReports, Val: []byte(zoneConfigEndpoint(member, "p"))})
	}
	if z.Kind == ZoneHotWater {
		tls = append(tls, TLV{Tag: zoneConfigTagReports, Val: []byte(zoneConfigEndpoint(owner, "p"))})
	}
	for _, member := range membership {
		tls = append(tls, TLV{Tag: zoneConfigTagMembers, Val: []byte(zoneConfigEndpoint(member, "s"))})
	}

	if z.Kind == ZoneHeating {
		pid := in.Options.ZonePID(zone)
		if z.Circuit != nil { // a zone with no circuit omits 6060 and the heat source: not verified on air
			tls = append(tls, TLV{Tag: zoneConfigTagCircuit, Val: []byte{byte(*z.Circuit)}})
		}
		tls = append(tls,
			TLV{Tag: zoneConfigTagMeasure, Val: []byte(zoneConfigEndpointWire(ownerWire, "p"))},
			zoneConfigU16(zoneConfigTagKp, pid.Kp),
			zoneConfigU16(zoneConfigTagKi, pid.Ki),
			zoneConfigU16(zoneConfigTagKd, pid.Kd),
			TLV{Tag: zoneConfigTagEnable, Val: []byte{boolByte(z.OpenWindowEnabled())}},
			zoneConfigU16(zoneConfigTagOpen, z.OpenWindowSeconds()),
			zoneConfigU16(zoneConfigTagRefresh, zoneConfigRefreshS),
		)
	}

	body := encodeTLVs(tls)
	return &ZoneConfigDocument{Body: body, ETag: zoneConfigETag(body)}, nil
}

// zoneConfigOwner names the endpoint the document publishes as the zone owner.
// The elected demand reporter takes it; a zone that elects none falls back to
// the measuring device, then to the first member, so the document still builds.
func zoneConfigOwner(in *Install, z *Zone, members []*Device) *Device {
	if z.Kind == ZoneHotWater {
		return members[0]
	}
	if d, err := in.DemandReporter(z.Number); err == nil {
		return d
	}
	if d, err := in.MeasuringDevice(z.Number); err == nil {
		return d
	}
	return members[0]
}

func zoneConfigOrderMembers(owner *Device, members []*Device) []*Device {
	out := make([]*Device, 0, len(members))
	if containsDevice(members, owner) {
		out = append(out, owner)
	}
	for _, member := range members {
		if member != owner {
			out = append(out, member)
		}
	}
	return out
}

func zoneConfigEndpoint(d *Device, resource string) string {
	return zoneConfigEndpointWire(d.WireEUI(), resource)
}

// zoneConfigEndpointWire is the endpoint of a node by its wire EUI.
func zoneConfigEndpointWire(wire []byte, resource string) string {
	return fmt.Sprintf("coap://[fe80:0:0:0:%x:%x:%x:%x]/z/%s",
		(uint16(wire[7])<<8|uint16(wire[6]))^0x0200,
		uint16(wire[5])<<8|uint16(wire[4]),
		uint16(wire[3])<<8|uint16(wire[2]),
		uint16(wire[1])<<8|uint16(wire[0]),
		resource)
}

func zoneConfigU16(tag string, value int) TLV {
	return TLV{Tag: tag, Val: []byte{byte(value >> 8), byte(value)}}
}

func zoneConfigETag(body []byte) []byte {
	h := fnv.New64a()
	_, _ = h.Write(body)
	return h.Sum(nil)
}
