package daemon

// install_removal: what removing a device from the installation does besides
// dropping its row. The tado app asks the cloud for these side effects before it
// removes a device; GET /api/install/devices/{ref}/removal answers the same
// question, and DELETE refuses to release them without ?force=true.

import (
	"net/http"
	"slices"

	"freedo/tado"
)

// The effects, named by the fields of the installation model.
const (
	effectMember          = "member"
	effectHotWater        = "hot_water"
	effectLastMember      = "last_member"
	effectReporter        = "reporter"
	effectMeasuringDevice = "measuring_device"
	effectHeatSource      = "heat_source"
)

// removalEffect is one consequence of removing a device. Zone names the zone an
// effect of the device's membership falls on, and Circuit the circuit it drives.
type removalEffect struct {
	Effect  string `json:"effect"`
	Zone    *int   `json:"zone,omitempty"`
	Circuit *int   `json:"circuit,omitempty"`
}

// removalEffects lists what removing dev from in changes, zone by zone and then
// circuit by circuit: the zone loses the device as a member, the zone is the
// hot-water zone, the zone is left with no member, the zone loses its demand reporter or its measuring device, and a
// circuit loses its heat source. The measuring device is the one the daemon
// reads the zone temperature from: the elected one, else the reporter, else the
// only member.
func removalEffects(in *tado.Install, dev *tado.Device) []removalEffect {
	effects := []removalEffect{}
	for i := range in.Zones {
		z := &in.Zones[i]
		members, err := in.ZoneMembers(z.Number)
		if err != nil || !slices.Contains(members, dev) {
			continue
		}
		zone := z.Number
		onZone := func(effect string) removalEffect { return removalEffect{Effect: effect, Zone: &zone} }
		// Every membership is listed, so an empty list means that DELETE needs no
		// force.
		effects = append(effects, onZone(effectMember))
		if z.Kind == tado.ZoneHotWater {
			effects = append(effects, onZone(effectHotWater))
		}
		if len(members) == 1 {
			effects = append(effects, onZone(effectLastMember))
		}
		if reporter, err := in.DeclaredReporter(z.Number); err == nil && reporter == dev {
			effects = append(effects, onZone(effectReporter))
		}
		if measuring, err := in.MeasuringDevice(z.Number); err == nil && measuring == dev {
			effects = append(effects, onZone(effectMeasuringDevice))
		}
	}
	for _, c := range in.CircuitsFor(dev) {
		id := c.ID
		effects = append(effects, removalEffect{Effect: effectHeatSource, Circuit: &id})
	}
	return effects
}

// getInstallDeviceRemoval reports what DELETE on the same device would do.
// forceable is false while the device is the heat source of a circuit: force
// does not release that, the circuit has to change first.
func (d *daemon) getInstallDeviceRemoval(w http.ResponseWriter, r *http.Request) {
	in, rev, ok := d.installNow(w, r)
	if !ok {
		return
	}
	dev, err := in.Resolve(r.PathValue("ref"))
	if err != nil {
		writeResolveErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"rev":       rev,
		"device":    deviceView(dev),
		"effects":   removalEffects(in, dev),
		"forceable": len(in.CircuitsFor(dev)) == 0,
	})
}
