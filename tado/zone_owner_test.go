package tado

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

// TestZoneConfigDocumentCoordinatorOwner: with the coordinator as owner the
// document names us in 63a0 and 6040, in the form it uses for a device, and
// keeps the members and their report endpoints.
func TestZoneConfigDocumentCoordinatorOwner(t *testing.T) {
	in := fixtureInstall(t)
	device, err := BuildZoneConfigDocument(in, 3)
	if err != nil {
		t.Fatal(err)
	}
	in.Options.ZoneOwners = map[int]string{3: ZoneOwnerCoordinator}
	doc, err := BuildZoneConfigDocument(in, 3)
	if err != nil {
		t.Fatal(err)
	}
	const valve = "coap://[fe80:0:0:0:21b:c507:fade:a408]/z/"
	const coordinator = "coap://[fe80:0:0:0:21b:c507:face:a109]/z/"
	want := bytes.ReplaceAll(device.Body, []byte(valve+"s"), []byte(coordinator+"s"))
	// 8000 lists the valve as a member: put that one back.
	want = bytes.Replace(want, zoneConfigTestTLV(0x8000, []byte(coordinator+"s")), zoneConfigTestTLV(0x8000, []byte(valve+"s")), 1)
	want = bytes.ReplaceAll(want, zoneConfigTestTLV(0x6040, []byte(valve+"p")), zoneConfigTestTLV(0x6040, []byte(coordinator+"p")))
	if !bytes.Equal(doc.Body, want) {
		t.Errorf("body = %x\nwant   %x", doc.Body, want)
	}
	if bytes.Equal(doc.ETag, device.ETag) {
		t.Error("ETag did not move with the owner")
	}
	if !bytes.Contains(doc.Body, zoneConfigTestTLV(0x8400, []byte(valve+"p"))) {
		t.Error("the valve lost its report endpoint")
	}
	if bytes.Contains(doc.Body, []byte(coordinator+"cpe")) || bytes.Contains(doc.Body, zoneConfigTestTLV(0x8000, []byte(coordinator+"s"))) {
		t.Error("the coordinator appears as a member")
	}
}

// TestCheckZoneOwners: the coordinator owns a heating zone only, and the value
// is one of the two.
func TestCheckZoneOwners(t *testing.T) {
	for _, tc := range []struct {
		zone  int
		owner string
		want  string
	}{
		{3, ZoneOwnerCoordinator, ""},
		{3, ZoneOwnerDevice, ""},
		{0, ZoneOwnerCoordinator, "not a heating zone"},
		{99, ZoneOwnerCoordinator, "no zone 99"},
		{3, "cloud", "must be device or coordinator"},
	} {
		in := fixtureInstall(t)
		in.Options.ZoneOwners = map[int]string{tc.zone: tc.owner}
		err := in.Validate()
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("zone %d owner %q: %v", tc.zone, tc.owner, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("zone %d owner %q: err=%v, want %q", tc.zone, tc.owner, err, tc.want)
		}
	}
}

// TestZoneMeasureServedForOwnedZone: a GET z/p?lid=N of an owned zone gets the
// measure the daemon set, not the one a member pushed, and the request is
// recorded.
func TestZoneMeasureServedForOwnedZone(t *testing.T) {
	in := synthInstall(t)
	in.Options.ZoneOwners = map[int]string{4: ZoneOwnerCoordinator}
	e := NewEmu(in.EmuConfig(), nil)
	va, err := in.Resolve("VA0000000001")
	if err != nil {
		t.Fatal(err)
	}
	e.OnFrame(synthZoneUplink(t, in, va.WireEUI(), CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x7501, Token: []byte{},
		Path: []string{"z", "p"}, Query: []string{"lid=4"},
		Options: []CoAPOption{optOctets, optZoneMeasureAge},
		Payload: ZoneMeasureBody(21.5, 0, 500),
	}))
	own := ZoneMeasureBody(18.25, 63, 634)
	e.SetZoneMeasure(4, own)
	get := synthZoneUplink(t, in, va.WireEUI(), CoAPReq{
		Type: "CON", Code: "0.01", MID: 0x7502, Token: []byte{0x23},
		Path: []string{"z", "p"}, Query: []string{"lid=4"},
		Options: []CoAPOption{{Num: 6, Val: []byte{}}},
	})
	m := synthAnswer(t, in, e.OnFrame(get), 0x7502)
	if m.Code != "2.05" || !bytes.Equal(m.Payload, own) {
		t.Errorf("answer=%s body=%x, want 2.05 %x", m.Code, m.Payload, own)
	}
	if age, ok := m.Option(14); !ok || decodeCoAPUint(age.Val) != 1800 {
		t.Errorf("Max-Age=%x present=%v, want 1800", age.Val, ok)
	}
	reqs := e.TakeOwnerRequests()
	if len(reqs) != 2 || reqs[1].Method != "0.01" || !reqs[1].Observe || reqs[1].Zone != 4 || reqs[1].URI != "z/p?lid=4" {
		t.Errorf("owner requests = %+v, want the push and the observe GET of zone 4", reqs)
	}
	if len(e.TakeOwnerRequests()) != 0 {
		t.Error("the requests were not drained")
	}
	e.ClearZoneMeasure(4)
	m = synthAnswer(t, in, e.OnFrame(synthZoneUplink(t, in, va.WireEUI(), CoAPReq{
		Type: "CON", Code: "0.01", MID: 0x7503, Token: []byte{0x24},
		Path: []string{"z", "p"}, Query: []string{"lid=4"},
	})), 0x7503)
	if !bytes.Equal(m.Payload, ZoneMeasureBody(21.5, 0, 500)) {
		t.Errorf("after the clear the answer is %x, want the member's push", m.Payload)
	}
}

// TestEnqueueZoneMeasure: the push is a CON PUT z/p?lid=N on the zone plane
// with the measure and the Max-Age, a member's ACK 2.05 closes it, and a newer
// measure replaces one that has not gone out.
func TestEnqueueZoneMeasure(t *testing.T) {
	in := synthInstall(t)
	in.Options.ZoneOwners = map[int]string{4: ZoneOwnerCoordinator}
	e := NewEmu(in.EmuConfig(), nil)
	if err := e.EnqueueZoneMeasure("VA0000000001", 0, 18, 10, 500); err == nil {
		t.Error("a zone the coordinator does not own took a measure")
	}
	if err := e.EnqueueZoneMeasure("VA0000000001", 4, 18, 10, 500); err != nil {
		t.Fatal(err)
	}
	if err := e.EnqueueZoneMeasure("VA0000000001", 4, 18.25, 63, 634); err != nil {
		t.Fatal(err)
	}
	if n := e.Pending("VA0000000001"); n != 1 {
		t.Fatalf("pending = %d, want the newer measure alone", n)
	}
	actions, ok, err := e.NextQueuedDownlink("VA0000000001")
	if err != nil || !ok {
		t.Fatalf("dispatch ok=%t err=%v", ok, err)
	}
	var m *CoAPMessage
	for _, a := range actions {
		if a.Kind != ActData {
			continue
		}
		df, err := ParseData(in.NetworkKey(), a.Bytes)
		if err != nil {
			continue
		}
		if app := ParseApp(df.Plaintext); app != nil && app.CoAP != nil {
			m = app.CoAP
		}
	}
	if m == nil {
		t.Fatal("no CoAP message went out")
	}
	if m.Type != "CON" || m.Code != "0.03" || m.URI() != "z/p?lid=4" || !m.Contexted() || len(m.Token) != 0 {
		t.Errorf("push = %s %s %s contexted=%t token=%x, want CON PUT z/p?lid=4 on the zone plane with an empty token",
			m.Type, m.Code, m.URI(), m.Contexted(), m.Token)
	}
	if want := ZoneMeasureBody(18.25, 63, 634); !bytes.Equal(m.Payload, want) {
		t.Errorf("payload = %x, want %x", m.Payload, want)
	}
	if hex.EncodeToString(m.Payload) != "4060020721"+"40a0013f"+"013502027a" {
		t.Errorf("payload = %x, want 4060=0721 40a0=3f 0135=027a", m.Payload)
	}
	if age, ok := m.Option(14); !ok || decodeCoAPUint(age.Val) != 1800 {
		t.Errorf("Max-Age=%x present=%v, want 1800", age.Val, ok)
	}
	va, _ := in.Resolve("VA0000000001")
	e.OnFrame(synthZoneUplink(t, in, va.WireEUI(), CoAPReq{
		Type: "ACK", Code: "2.05", MID: m.MID, Token: []byte{},
		Options: []CoAPOption{optOctets}, Payload: TLV{Tag: "62a0", Val: []byte{0}}.Encode(),
	}))
	if n := e.Pending("VA0000000001"); n != 0 {
		t.Errorf("pending = %d after the member's 2.05, want 0", n)
	}
}

// TestZoneOwnerElectsNoMember: a heating zone the coordinator owns elects no
// demand reporter, so the valve's 015e record drops the elected bit; the
// declared reporter still names the measuring device.
func TestZoneOwnerElectsNoMember(t *testing.T) {
	in := synthInstall(t)
	va, err := in.Resolve("VA0000000001")
	if err != nil {
		t.Fatal(err)
	}
	if recs := initialNodeTable(in, va); len(recs) != 1 || recs[0].Role != 0x0d || recs[0].Node != 4 {
		t.Fatalf("node table under the device = %+v, want role 0d for zone 4", recs)
	}
	in.Options.ZoneOwners = map[int]string{4: ZoneOwnerCoordinator}
	if _, err := in.DemandReporter(4); err == nil {
		t.Error("an owned zone elects a demand reporter")
	}
	if d, err := in.DeclaredReporter(4); err != nil || d != va {
		t.Errorf("declared reporter = %v %v, want the valve", d, err)
	}
	if d, err := in.MeasuringDevice(4); err != nil || d != va {
		t.Errorf("measuring device = %v %v, want the valve", d, err)
	}
	if recs := initialNodeTable(in, va); len(recs) != 1 || recs[0].Role != 0x05 || recs[0].Node != 4 {
		t.Errorf("node table under the coordinator = %+v, want role 05 for zone 4", recs)
	}
}

// TestZoneMeasureReadWithoutZoneID: the wall thermostat reads the owner's z/p
// with no query. With the coordinator as owner the answer is the served
// measure of the owned zone the thermostat serves; a zone it does not own
// answers the bare 2.05 as before.
func TestZoneMeasureReadWithoutZoneID(t *testing.T) {
	in := synthInstall(t)
	in.Options.ZoneOwners = map[int]string{4: ZoneOwnerCoordinator}
	e := NewEmu(in.EmuConfig(), nil)
	ru, err := in.Resolve("RU0000000001")
	if err != nil {
		t.Fatal(err)
	}
	body := ZoneMeasureBody(19.5, 42, 500)
	e.SetZoneMeasure(4, body)
	read := func(mid uint16) *CoAPMessage {
		get := synthZoneUplink(t, in, ru.WireEUI(), CoAPReq{
			Type: "CON", Code: "0.01", MID: mid, Token: []byte{0x24},
			Path: []string{"z", "p"},
		})
		return synthAnswer(t, in, e.OnFrame(get), mid)
	}
	m := read(0x7410)
	if m.Code != "2.05" || !bytes.Equal(m.Payload, body) {
		t.Errorf("answer = %s %x, want 2.05 with the served measure %x", m.Code, m.Payload, body)
	}
	if age, ok := m.Option(14); !ok || decodeCoAPUint(age.Val) != 1800 {
		t.Errorf("Max-Age=%x present=%v, want 1800", age.Val, ok)
	}
	reqs := e.TakeOwnerRequests()
	if len(reqs) != 1 || reqs[0].Zone != 4 || reqs[0].Device != ru.Short() {
		t.Errorf("owner requests = %+v, want one for zone 4 from the thermostat", reqs)
	}
	// Back to the device: nothing served, the bare answer.
	e.ClearZoneMeasure(4)
	in.Options.ZoneOwners = nil
	m = read(0x7411)
	if m.Code != "2.05" || len(m.Payload) != 0 {
		t.Errorf("answer under the device = %s %x, want a bare 2.05", m.Code, m.Payload)
	}
}
