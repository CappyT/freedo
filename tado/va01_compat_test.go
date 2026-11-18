package tado

import "testing"

func TestVA01SetpointResponseCompatibility(t *testing.T) {
	cases := []struct {
		name     string
		firmware string
		response string
		want     string
	}{
		{"VA01_54_20_Changed", "54.20", "2.04", "delivered"},
		{"VA01_54_20_Content", "54.20", "2.05", "delivered"},
		{"VA02_215_1_Changed", "215.1", "2.04", "delivered"},
		{"VA02_215_1_Content", "215.1", "2.05", "delivered"},
		{"Unknown_Changed", "", "2.04", "delivered"},
		{"Unknown_Content", "", "2.05", "delivered"},
		{"VA01_54_21_Content", "54.21", "2.05", "delivered"},
		{"VA01_OtherSuccess", "54.20", "2.01", "pending"},
		{"VA01_ClientError", "54.20", "4.05", "dropped"},
		{"VA01_ServerError", "54.20", "5.03", "pending"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, _, _ := newTestEmu(t)

			wire, label, err := e.resolve("bedroom")
			if err != nil {
				t.Fatal(err)
			}

			// Simulate firmware information from d/fw/state.
			e.node(wire).Telemetry["fw_run_version"] = tc.firmware

			err = e.EnqueueSetpoint("bedroom", SetpointParams{
				ID:       4,
				Setpoint: 21,
				Overlay:  2,
			})
			if err != nil {
				t.Fatal(err)
			}

			actions, ok := e.nextDownlink(wire, label)
			if !ok || len(actions) != 1 {
				t.Fatalf("no downlink: ok=%v actions=%+v", ok, actions)
			}

			q := e.queue[hexEUI(wire)]
			if len(q) != 1 || !q[0].inFlight {
				t.Fatalf("unexpected queue: %+v", q)
			}

			// Simulate a matching CoAP ACK from the valve.
			e.confirmByMID(wire, &CoAPMessage{
				Type:  "ACK",
				Code:  tc.response,
				MID:   q[0].mid,
				Token: append([]byte(nil), q[0].responseToken...),
			})

			pending := e.Pending("bedroom")

			switch tc.want {
			case "delivered":
				if pending != 0 {
					t.Fatalf("expected delivery, pending=%d", pending)
				}
				history := e.QueueHistory("bedroom")
				if len(history) != 1 || history[0].Result != "delivered" {
					t.Fatalf("unexpected history: %+v", history)
				}

			case "dropped":
				if pending != 0 {
					t.Fatalf("expected drop, pending=%d", pending)
				}
				history := e.QueueHistory("bedroom")
				if len(history) != 1 || history[0].Result != "dropped" {
					t.Fatalf("unexpected history: %+v", history)
				}

			case "pending":
				if pending != 1 {
					t.Fatalf("expected pending command, got %d", pending)
				}
			}
		})
	}
}

func TestVA01IdentifyResponseCompatibility(t *testing.T) {
	for _, tc := range []struct {
		response string
		pending  int
	}{{"2.04", 0}, {"2.05", 0}, {"2.01", 1}} {
		t.Run(tc.response, func(t *testing.T) {
			e, _, _ := newTestEmu(t)
			wire, label, err := e.resolve("bedroom")
			if err != nil {
				t.Fatal(err)
			}
			if err := e.EnqueueIdentify("bedroom", 1); err != nil {
				t.Fatal(err)
			}
			if actions, ok := e.nextDownlink(wire, label); !ok || len(actions) != 1 {
				t.Fatalf("no downlink: ok=%v actions=%+v", ok, actions)
			}
			q := e.queue[hexEUI(wire)]
			e.confirmByMID(wire, &CoAPMessage{
				Type: "ACK", Code: tc.response, MID: q[0].mid,
				Token: append([]byte(nil), q[0].responseToken...),
			})
			if got := e.Pending("bedroom"); got != tc.pending {
				t.Fatalf("pending after %s = %d, want %d", tc.response, got, tc.pending)
			}
		})
	}
}
