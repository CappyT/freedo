package tado

import "testing"

// TestBoilerTarget: the thermostat wired to the boiler is the hot-water
// thermostat, and without a hot-water zone the heat source of the first
// circuit as declared.
func TestBoilerTarget(t *testing.T) {
	dev, err := synthInstall(t).BoilerTarget()
	if err != nil || dev.Serial != "RU0000000001" {
		t.Fatalf("with a hot-water zone: %v %v, want RU0000000001", dev, err)
	}

	in, err := Parse([]byte(`
version: 1
home_id: 7654321
network_key: "000102030405060708090a0b0c0d0e0f"
coordinator:
  eui: "1000000000c51b00"
devices:
  - {kind: thermostat, eui: "2000000000c51b00", serial: RU0000000001, label: hall}
  - {kind: thermostat, eui: "2100000000c51b00", serial: RU0000000002, label: loft}
  - {kind: valve, eui: "3000000000c51b00", serial: VA0000000001, label: study}
  - {kind: valve, eui: "3100000000c51b00", serial: VA0000000002, label: attic}
zones:
  - {number: 4, name: study, kind: heating, circuit: 1, members: [VA0000000001], reporter: VA0000000001}
  - {number: 5, name: attic, kind: heating, circuit: 2, members: [VA0000000002], reporter: VA0000000002}
circuits:
  - {id: 2, name: upper, heat_source: RU0000000002}
  - {id: 1, name: lower, heat_source: RU0000000001}
`))
	if err != nil {
		t.Fatal(err)
	}
	if dev, err := in.BoilerTarget(); err != nil || dev.Serial != "RU0000000002" {
		t.Errorf("without a hot-water zone: %v %v, want RU0000000002", dev, err)
	}
	in.Circuits = nil
	if _, err := in.BoilerTarget(); err == nil {
		t.Error("an installation with no hot-water zone and no circuit named a target")
	}
}
