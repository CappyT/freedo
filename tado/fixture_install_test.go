package tado

// The synthetic installation the committed capture corpus (tado/testdata) is
// re-encoded under: every value is fake, the frames keep their structure.
// Every frame in it is encrypted with this key and addressed to these nodes,
// so the regression tests can only run against these values. They are
// test-only on purpose: production code reaches an installation through an
// *Install loaded from a config file.

func init() {
	NetworkKey = mustHex("0f1e2d3c4b5a69788796a5b4c3d2e1f0")
	BridgeEUI = mustHex("09a1cefa07c51b00")
	Inventory = fixtureInventory
}

var fixtureInventory = []Node{
	{"09a1cefa07c51b00", "00:1b:c5:07:fa:ce:a1:09", "a109", "IB1000000001", "bridge"},
	{"05a2cafa07c51b00", "00:1b:c5:07:fa:ca:a2:05", "a205", "RU1000000001", "corridoio"},
	{"02a6cefa07c51b00", "00:1b:c5:07:fa:ce:a6:02", "a602", "VA1000000005", "camera"},
	{"04a3defa07c51b00", "00:1b:c5:07:fa:de:a3:04", "a304", "VA1000000007", "bagno"},
	{"07a5cefa07c51b00", "00:1b:c5:07:fa:ce:a5:07", "a507", "VA1000000003", "corridoio"},
	{"0aa7cefa07c51b00", "00:1b:c5:07:fa:ce:a7:0a", "a70a", "VA1000000004", "soggiorno"},
	{"03a8cafa07c51b00", "00:1b:c5:07:fa:ca:a8:03", "a803", "", "soggiorno"},
	{"010acefa07c51b00", "00:1b:c5:07:fa:ce:0a:01", "0a01", "VA1000000006", "cucina"},
	{"060acefa07c51b00", "00:1b:c5:07:fa:ce:0a:06", "0a06", "VA1000000001", "cucina"},
	{"08a4defa07c51b00", "00:1b:c5:07:fa:de:a4:08", "a408", "VA1000000002", "ufficio"},
}

// fixtureInstall is the same installation expressed as an *Install, for the
// tests that exercise the config-driven path.
func fixtureInstall(t interface{ Fatal(...any) }) *Install {
	in, err := Parse([]byte(fixtureInstallYAML))
	if err != nil {
		t.Fatal(err)
	}
	return in
}

const fixtureInstallYAML = `
version: 1
home_id: 7654321
network_key: "0f1e2d3c4b5a69788796a5b4c3d2e1f0"
coordinator:
  eui: "09a1cefa07c51b00"
  serial: IB1000000001
devices:
  - {kind: thermostat, eui: "05a2cafa07c51b00", serial: RU1000000001, label: corridoio}
  - {kind: valve, eui: "02a6cefa07c51b00", serial: VA1000000005, label: camera}
  - {kind: valve, eui: "04a3defa07c51b00", serial: VA1000000007, label: bagno}
  - {kind: valve, eui: "07a5cefa07c51b00", serial: VA1000000003, label: corridoio}
  - {kind: valve, eui: "0aa7cefa07c51b00", serial: VA1000000004, label: soggiorno}
  - {kind: valve, eui: "03a8cafa07c51b00", label: soggiorno}
  - {kind: valve, eui: "010acefa07c51b00", serial: VA1000000006, label: cucina}
  - {kind: valve, eui: "060acefa07c51b00", serial: VA1000000001, label: cucina}
  - {kind: valve, eui: "08a4defa07c51b00", serial: VA1000000002, label: ufficio}
zones:
  - {number: 0, name: acqua calda, kind: hotwater, members: [RU1000000001]}
  - {number: 1, name: corridoio, kind: heating, circuit: 1, members: [RU1000000001, VA1000000003], reporter: RU1000000001}
  - {number: 2, name: bagno, kind: heating, circuit: 1, members: [VA1000000007], reporter: VA1000000007}
  - {number: 3, name: ufficio, kind: heating, circuit: 1, members: [VA1000000002], reporter: VA1000000002}
  - {number: 5, name: cucina, kind: heating, circuit: 1, members: [VA1000000001, VA1000000006], reporter: VA1000000006}
  - {number: 8, name: soggiorno, kind: heating, circuit: 1, members: [VA1000000004, "a803"], reporter: "a803"}
  - {number: 10, name: camera, kind: heating, circuit: 1, members: [VA1000000005], reporter: VA1000000005}
circuits:
  - {id: 1, name: casa, heat_source: RU1000000001}
options:
  utc_offset_min: 60
`
