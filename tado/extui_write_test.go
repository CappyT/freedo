package tado

import (
	"encoding/hex"
	"testing"
)

func TestExtuiPZHWriteRequiresValve201(t *testing.T) {
	bedroom := NodeByAny("bedroom")
	bedroomEUI := mustHex(bedroom.WireLE)
	e, _, _ := newTestEmu(t)
	wire, label, err := e.resolve("bedroom")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.EnqueueExtuiPZH("bedroom", "", ""); err != nil {
		t.Fatal(err)
	}
	actions, ok := e.nextDownlink(wire, label)
	if !ok || len(actions) < 2 || !actions[0].WaitResponse {
		t.Fatalf("actions=%+v ok=%t", actions, ok)
	}
	reasm := NewReassembler()
	var request *CoAPMessage
	var lastSeq byte
	for i, action := range actions {
		data, err := ParseData(NetworkKey, action.Bytes)
		if err != nil {
			t.Fatalf("fragment %d: %v", i, err)
		}
		lastSeq = data.Seq
		if msg, done := reasm.Add(data); done {
			request = msg
		}
	}
	if request == nil || request.Code != "0.03" || request.URI() != "z/extui?lid=pzh" {
		t.Fatalf("request=%+v", request)
	}
	if got, want := hex.EncodeToString(request.Payload), "01d51e5b616161613a3a3231623a633530373a666163653a613130395d2f7a2f7001d41e5b616161613a3a3231623a633530373a666163653a613130395d2f7a2f73"; got != want {
		t.Fatalf("payload=%s want=%s", got, want)
	}
	if option, ok := request.Option(7); ok {
		t.Fatalf("Uri-Port=%x", option.Val)
	}
	e.OnFrame(BuildEnhAck(lastSeq, BridgeEUI, bedroomEUI, 0x0101))
	if e.Pending("bedroom") != 1 {
		t.Fatalf("Enhanced ACK completed extui: pending=%d", e.Pending("bedroom"))
	}
	ack, err := BuildCoAPAck("2.01", request.MID, request.Token, nil, bedroomEUI, BridgeEUI, coapPort, bridgeCoAPPort)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := BuildData(NetworkKey, 0x42, BridgeEUI, bedroomEUI[:5], WrapAppPayload(ack, 900, nil))
	if err != nil {
		t.Fatal(err)
	}
	e.OnFrame(frame.Bytes())
	if e.Pending("bedroom") != 0 {
		t.Fatalf("2.01 left extui pending=%d", e.Pending("bedroom"))
	}
	history := e.QueueHistory("bedroom")
	if len(history) != 1 || history[0].Kind != "extui" || history[0].Reason != "coap-ack" {
		t.Fatalf("history=%+v", history)
	}
}
