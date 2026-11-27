package daemon

import (
	"net/http"
	"testing"

	"freedo/tado"
)

func TestSensorDaemonKindChecks(t *testing.T) {
	if err := checkDeviceKind("sensor"); err != nil {
		t.Fatalf("checkDeviceKind(sensor) = %v, want nil", err)
	}

	noBoiler := true
	in := &tado.Install{
		Version:       tado.ConfigVersion,
		HomeID:        123456789,
		NetworkKeyHex: "00112233445566778899aabbccddeeff",
		Coordinator:   tado.Coordinator{EUI: "a1b2c37520c51b00"},
		Devices: []tado.Device{
			{Kind: tado.KindSensor, EUI: "1a2b3c3107c51b00", Serial: "SU1000000001", Label: "hallway"},
		},
		Zones: []tado.Zone{
			{
				Number:   11,
				Name:     "hallway",
				Kind:     tado.ZoneHeating,
				Members:  []string{"1a2b3c3107c51b00"},
				Reporter: []string{"1a2b3c3107c51b00"},
			},
		},
		Options: tado.Options{NoBoiler: &noBoiler, Timezone: "UTC"},
	}
	a := newInstallAPIOn(t, in)
	short := tado.ShortFromWire(in.Devices[0].WireEUI())

	// postConfig: calibOffset is served on read with a re-read trigger; utcOffsetMin is refused.
	var res map[string]any
	if code := postJSON(t, http.MethodPost, a.base+"/api/nodes/"+short+"/config",
		map[string]any{"calibOffset": -50}, &res); code != http.StatusOK {
		t.Fatalf("postConfig(calibOffset): code=%d %v", code, res)
	}
	if res["queued"] != false || res["served"] != true || res["reread"] != true {
		t.Errorf("postConfig(calibOffset) = %v, want queued=false served=true reread=true", res)
	}

	if code := postJSON(t, http.MethodPost, a.base+"/api/nodes/"+short+"/config",
		map[string]any{"utcOffsetMin": 90}, &res); code != http.StatusBadRequest {
		t.Errorf("postConfig(utcOffsetMin) on sensor: code=%d %v, want 400", code, res)
	}

	// postConfig with empty body must not auto-populate utcOffsetMin or require interface module id.
	if code := postJSON(t, http.MethodPost, a.base+"/api/nodes/"+short+"/config",
		map[string]any{}, &res); code != http.StatusOK || res["queued"] != false {
		t.Errorf("postConfig(empty) on sensor: code=%d %v, want 200 queued=false", code, res)
	}

	// deleteOfflineSchedule must not require interface module id for a sensor.
	if code := postJSON(t, http.MethodDelete, a.base+"/api/nodes/"+short+"/config/offlineScheduleEnabled",
		nil, &res); code != http.StatusOK || res["queued"] != false {
		t.Errorf("deleteOfflineSchedule on sensor: code=%d %v, want 200 queued=false", code, res)
	}
}

