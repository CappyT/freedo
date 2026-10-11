package radio

import (
	"encoding/hex"

	"freedo/tado"
)

// The synthetic installation the committed capture corpus (tado/testdata) is
// re-encoded under: every value is fake, the frames keep their structure.
// The live radio tests build their frames with this key and these addresses.
// Test-only: the radio itself takes the coordinator from its caller.

// defaultInstall is the installation the tests build their frames on.
var defaultInstall *tado.Install

func init() {
	in, err := tado.Parse([]byte(fixtureInstallYAML))
	if err != nil {
		panic("internal/radio: bad test install: " + err.Error())
	}
	defaultInstall = in

	tado.NetworkKey = in.NetworkKey()
	tado.BridgeEUI = in.CoordinatorEUI()
	tado.Inventory = []tado.Node{
		{WireLE: "09a1cefa07c51b00", EUI: "00:1b:c5:07:fa:ce:a1:09", Short: "a109", Serial: "IB1000000001", Room: "bridge"},
	}
	for i := range in.Devices {
		d := &in.Devices[i]
		tado.Inventory = append(tado.Inventory, tado.Node{
			WireLE: hex.EncodeToString(d.WireEUI()), Short: d.Short(), Serial: d.Serial, Room: d.Label,
		})
	}
}

const fixtureInstallYAML = `
version: 1
home_id: 7654321
network_key: "0f1e2d3c4b5a69788796a5b4c3d2e1f0"
coordinator:
  eui: "09a1cefa07c51b00"
  serial: IB1000000001
devices:
  - {kind: thermostat, eui: "05a2cafa07c51b00", serial: RU1000000001, label: hall}
  - {kind: valve, eui: "02a6cefa07c51b00", serial: VA1000000005, label: bedroom}
  - {kind: valve, eui: "04a3defa07c51b00", serial: VA1000000007, label: bath}
  - {kind: valve, eui: "07a5cefa07c51b00", serial: VA1000000003, label: hall}
  - {kind: valve, eui: "0aa7cefa07c51b00", serial: VA1000000004, label: living}
  - {kind: valve, eui: "03a8cafa07c51b00", label: living}
  - {kind: valve, eui: "010acefa07c51b00", serial: VA1000000006, label: kitchen}
  - {kind: valve, eui: "060acefa07c51b00", serial: VA1000000001, label: kitchen}
  - {kind: valve, eui: "08a4defa07c51b00", serial: VA1000000002, label: office}
zones:
  - {number: 0, name: hot water, kind: hotwater, members: [RU1000000001]}
  - {number: 1, name: hall, kind: heating, circuit: 1, members: [RU1000000001, VA1000000003], reporter: RU1000000001}
  - {number: 2, name: bath, kind: heating, circuit: 1, members: [VA1000000007], reporter: VA1000000007}
  - {number: 3, name: office, kind: heating, circuit: 1, members: [VA1000000002], reporter: VA1000000002}
  - {number: 5, name: kitchen, kind: heating, circuit: 1, members: [VA1000000001, VA1000000006], reporter: VA1000000006}
  - {number: 8, name: living, kind: heating, circuit: 1, members: [VA1000000004, "a803"], reporter: "a803"}
  - {number: 10, name: bedroom, kind: heating, circuit: 1, members: [VA1000000005], reporter: VA1000000005}
circuits:
  - {id: 1, name: house, heat_source: RU1000000001}
options:
  utc_offset_min: 60
`
