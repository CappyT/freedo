package tado

import (
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

const (
	capKitchenOwner  = "010acefa07c51b00" // valve VA1000000006 (0a01), elected owner of zone 5
	capKitchenMember = "060acefa07c51b00" // valve VA1000000001 (0a06), member of zone 5
	capKitchenThird  = "08a4defa07c51b00" // valve VA1000000002 (a408), third member of zone 5 in these tests
)

// threeMemberYAML is the fixture with VA1000000002 moved into zone 5.
func threeMemberYAML() string {
	y := strings.ReplaceAll(fixtureInstallYAML, "members: [VA1000000001, VA1000000006]", "members: [VA1000000001, VA1000000006, VA1000000002]")
	return strings.ReplaceAll(y, "  - {number: 3, name: office, kind: heating, circuit: 1, members: [VA1000000002], reporter: VA1000000002}\n", "")
}

// ownerEmu builds an emulator on yaml with a clock the test moves.
func ownerEmu(t *testing.T, yaml string, now *time.Time) *EmuState {
	t.Helper()
	in, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	cfg := in.EmuConfig()
	cfg.Now = func() time.Time { return *now }
	return NewEmu(cfg, NewUnhandledLogger(io.Discard))
}

// zonePushMID gives each push its own message id: the engine answers a repeated
// one from the same device as a duplicate and does not parse it again.
var zonePushMID uint16 = 0x5000

func zonePush(t *testing.T, e *EmuState, src string, tempCenti uint16, demand byte) {
	t.Helper()
	zonePushMID++
	e.OnFrame(zoneUplink(t, mustHex(src), BridgeEUI, CoAPReq{
		Type: "CON", Code: "0.03", MID: zonePushMID, Token: []byte{0x09},
		Path: []string{"z", "p"}, Query: []string{"lid=5"},
		Payload: concat(
			TLV{Tag: "4060", Val: []byte{byte(tempCenti >> 8), byte(tempCenti)}}.Encode(),
			TLV{Tag: "40a0", Val: []byte{demand}}.Encode(),
		),
	}))
}

// TestZoneMeasureComesFromOwner: members of a zone repeat the owner's copy with
// their own lag, so the zone shows the temperature and the demand of the owner.
func TestZoneMeasureComesFromOwner(t *testing.T) {
	now := time.Date(2026, 1, 5, 6, 0, 0, 0, time.UTC)
	e := ownerEmu(t, threeMemberYAML(), &now)
	if got := e.ZoneOwner(5); got != "0a01" {
		t.Fatalf("zone 5 owner = %q, want the elected 0a01", got)
	}

	zonePush(t, e, capKitchenOwner, 2000, 40)
	zonePush(t, e, capKitchenMember, 1950, 30)
	zonePush(t, e, capKitchenThird, 1980, 20)
	got := zoneOf(t, e, 5)
	if got.TempC != 20.0 || got.DemandPct != 40 || got.Source != "0a01" {
		t.Errorf("zone 5 = %+v, want the owner's 20.00 C and 40 %% from 0a01", got)
	}

	now = now.Add(10 * time.Minute)
	zonePush(t, e, capKitchenMember, 1900, 10)
	if got := zoneOf(t, e, 5); got.TempC != 20.0 || got.DemandPct != 40 {
		t.Errorf("zone 5 = %+v after a member push, want the owner's values", got)
	}

	e.OnFrame(uplinkFrame(t, mustHex(capKitchenMember), "0.03",
		[]string{"h", "7654321", "z", "5", "act"}, nil, TLV{Tag: "40a0", Val: []byte{5}}.Encode()))
	if got := zoneOf(t, e, 5); got.DemandPct != 40 {
		t.Errorf("zone 5 demand = %d after a member act, want the owner's 40", got.DemandPct)
	}

	// Tag 4160 keeps its source.
	e.OnFrame(zoneUplink(t, mustHex(capHall), mustHex(capKitchenMember), CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x3a05, Token: []byte{0x3a},
		Path: []string{"z", "cpe"}, Query: []string{"lid=5"},
		Payload: TLV{Tag: "4160", Val: []byte{55}}.Encode(),
	}))
	if got := zoneOf(t, e, 5); got.HeatPct != 55 || got.TempC != 20.0 {
		t.Errorf("zone 5 = %+v, want heat 55 and the owner's temperature", got)
	}
}

// TestZoneMeasureFillsInWhileOwnerIsSilent: a member value counts only while the
// owner has no fresh value, and the owner takes the zone back when it speaks.
func TestZoneMeasureFillsInWhileOwnerIsSilent(t *testing.T) {
	now := time.Date(2026, 1, 5, 6, 0, 0, 0, time.UTC)
	e := ownerEmu(t, threeMemberYAML(), &now)

	zonePush(t, e, capKitchenMember, 1950, 30)
	if got := zoneOf(t, e, 5); got.TempC != 19.5 || got.DemandPct != 30 {
		t.Fatalf("zone 5 = %+v, want the member's values while the owner has sent none", got)
	}

	zonePush(t, e, capKitchenOwner, 2000, 40)
	now = now.Add(e.inst.Options.OfflineAfter() - time.Second)
	zonePush(t, e, capKitchenMember, 1900, 10)
	if got := zoneOf(t, e, 5); got.TempC != 20.0 || got.DemandPct != 40 {
		t.Fatalf("zone 5 = %+v, want the owner's values inside the freshness window", got)
	}

	now = now.Add(2 * time.Second)
	zonePush(t, e, capKitchenMember, 1900, 10)
	if got := zoneOf(t, e, 5); got.TempC != 19.0 || got.DemandPct != 10 {
		t.Fatalf("zone 5 = %+v, want the member's values once the owner is stale", got)
	}

	zonePush(t, e, capKitchenOwner, 2010, 50)
	zonePush(t, e, capKitchenThird, 1800, 0)
	if got := zoneOf(t, e, 5); got.TempC != 20.1 || got.DemandPct != 50 || got.Source != "0a01" {
		t.Fatalf("zone 5 = %+v, want the owner back in control", got)
	}
}

// TestZoneMeasureWithoutOwnerTakesEverySender: a zone with no owner keeps the
// value of the last sender.
func TestZoneMeasureWithoutOwnerTakesEverySender(t *testing.T) {
	now := time.Date(2026, 1, 5, 6, 0, 0, 0, time.UTC)
	y := strings.ReplaceAll(threeMemberYAML(), "VA1000000002], reporter: VA1000000006}", "VA1000000002]}")
	e := ownerEmu(t, y, &now)
	if got := e.ZoneOwner(5); got != "" {
		t.Fatalf("zone 5 owner = %q, want none", got)
	}
	zonePush(t, e, capKitchenOwner, 2000, 40)
	zonePush(t, e, capKitchenMember, 1950, 30)
	if got := zoneOf(t, e, 5); got.TempC != 19.5 || got.DemandPct != 30 || got.Source != "0a06" {
		t.Errorf("zone 5 = %+v, want the last sender's values", got)
	}
}

// TestFallbackEventNamesZonesOfOtherOwners: a member that reports 0182 = 1 while
// another device owns its zone raises an event that carries the zone.
func TestFallbackEventNamesZonesOfOtherOwners(t *testing.T) {
	now := time.Date(2026, 1, 5, 6, 0, 0, 0, time.UTC)
	e := fallbackEmu(t, &now)

	e.OnFrame(deviceFallbackFrame(t, mustHex(capKitchenMember), "VA1000000001", FallbackHold))
	want := []FallbackEvent{{Zone: -1, Device: "0a06", From: FallbackNone, To: FallbackHold, MemberOf: []int{5}}}
	if ev := e.TakeFallbackEvents(); !reflect.DeepEqual(ev, want) {
		t.Errorf("member events = %v, want %v", ev, want)
	}

	e.OnFrame(deviceFallbackFrame(t, mustHex(capKitchenOwner), "VA1000000006", FallbackHold))
	want = []FallbackEvent{{Zone: -1, Device: "0a01", From: FallbackNone, To: FallbackHold}}
	if ev := e.TakeFallbackEvents(); !reflect.DeepEqual(ev, want) {
		t.Errorf("owner events = %v, want %v", ev, want)
	}
}

// TestFallbackEventNamesCoordinatorOwnedZone: in a zone the coordinator owns,
// the measuring device is a member like the others, so its 0182 = 1 names the
// zone.
func TestFallbackEventNamesCoordinatorOwnedZone(t *testing.T) {
	now := time.Date(2026, 1, 5, 6, 0, 0, 0, time.UTC)
	y := strings.Replace(fixtureInstallYAML, "  utc_offset_min: 60\n", "  utc_offset_min: 60\n  zone_owner: {5: coordinator}\n", 1)
	e := ownerEmu(t, y, &now)

	e.OnFrame(deviceFallbackFrame(t, mustHex(capKitchenOwner), "VA1000000006", FallbackHold))
	want := []FallbackEvent{{Zone: -1, Device: "0a01", From: FallbackNone, To: FallbackHold, MemberOf: []int{5}}}
	if ev := e.TakeFallbackEvents(); !reflect.DeepEqual(ev, want) {
		t.Errorf("events = %v, want %v", ev, want)
	}
}

// TestZoneOwnerFollowsElectionNotObservedRole: after a re-election the demoted
// device may still publish its old 015e record. The owner stays the reporter
// the installation elects, and the zone takes its measure.
func TestZoneOwnerFollowsElectionNotObservedRole(t *testing.T) {
	now := time.Date(2026, 1, 5, 6, 0, 0, 0, time.UTC)
	y := strings.Replace(fixtureInstallYAML,
		"members: [VA1000000001, VA1000000006], reporter: VA1000000006}",
		"members: [VA1000000001, VA1000000006], reporter: VA1000000001}", 1)
	e := ownerEmu(t, y, &now)

	// The demoted valve 0a01 publishes the elected role it held before.
	e.OnFrame(uplinkFrame(t, mustHex(capKitchenOwner), "0.03",
		[]string{"d", "VA1000000006", "config"}, nil,
		concat(
			TLV{Tag: "015c", Val: []byte{0x00, 0x74, 0xcb, 0xb1}}.Encode(),
			TLV{Tag: "015e", Val: []byte{0x0d, 0x05}}.Encode(),
		)))
	if got := e.ZoneOwner(5); got != "0a06" {
		t.Fatalf("zone 5 owner = %q, want the elected 0a06", got)
	}

	zonePush(t, e, capKitchenMember, 2000, 40)
	zonePush(t, e, capKitchenOwner, 1900, 10)
	if got := zoneOf(t, e, 5); got.TempC != 20.0 || got.DemandPct != 40 || got.Source != "0a06" {
		t.Errorf("zone 5 = %+v, want the elected reporter's values", got)
	}
}
