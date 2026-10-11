package tado

import (
	"bytes"
	"encoding/hex"
	"io"
	"reflect"
	"strings"
	"testing"
)

// The fixture lines the reload tests edit. Each one must survive verbatim in
// fixtureInstallYAML, which variantInstall checks.
const (
	bathDevice    = "  - {kind: valve, eui: \"04a3defa07c51b00\", serial: VA1000000007, label: bath}\n"
	officeDevice  = "  - {kind: valve, eui: \"08a4defa07c51b00\", serial: VA1000000002, label: office}\n"
	studyDevice   = "  - {kind: valve, eui: \"11226a3107c51b00\", serial: VA9000000001, label: study}\n"
	zone2         = "  - {number: 2, name: bath, kind: heating, circuit: 1, members: [VA1000000007], reporter: VA1000000007}\n"
	zone3         = "  - {number: 3, name: office, kind: heating, circuit: 1, members: [VA1000000002], reporter: VA1000000002}\n"
	zone3Extended = "  - {number: 3, name: office, kind: heating, circuit: 1, members: [VA1000000002, VA9000000001], reporter: VA1000000002}\n"
	zone5         = "  - {number: 5, name: kitchen, kind: heating, circuit: 1, members: [VA1000000001, VA1000000006], reporter: VA1000000006}\n"
	zone5Reelect  = "  - {number: 5, name: kitchen, kind: heating, circuit: 1, members: [VA1000000001, VA1000000006], reporter: VA1000000001}\n"
	zone10        = "  - {number: 10, name: bedroom, kind: heating, circuit: 1, members: [VA1000000005], reporter: VA1000000005}\n"
	zone5NoWindow = "  - {number: 5, name: kitchen, kind: heating, circuit: 1, members: [VA1000000001, VA1000000006], reporter: VA1000000006, open_window_detection: false}\n"
)

// electedRoleRec is the 015e role byte of an elected demand reporter, plainRoleRec
// that of a plain member.
const (
	electedRoleRec = 0x0d
	plainRoleRec   = 0x05
)

// The wire EUIs the tests name.
const (
	bedroomEUIHex  = "02a6cefa07c51b00" // VA1000000005, zone 10
	bathEUIHex     = "04a3defa07c51b00" // VA1000000007, zone 2
	studyEUIHex    = "11226a3107c51b00" // VA9000000001, added to zone 3
	kitchenAEUIHex = "060acefa07c51b00" // VA1000000001, zone 5
	kitchenBEUIHex = "010acefa07c51b00" // VA1000000006, zone 5 reporter
)

// variantInstall edits the synthetic fixture and validates the result.
func variantInstall(t *testing.T, edits ...[2]string) *Install {
	t.Helper()
	doc := fixtureInstallYAML
	for _, edit := range edits {
		if !strings.Contains(doc, edit[0]) {
			t.Fatalf("the fixture does not contain %q", edit[0])
		}
		doc = strings.Replace(doc, edit[0], edit[1], 1)
	}
	in, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("variant install: %v", err)
	}
	return in
}

func fixtureEmu(t *testing.T) (*EmuState, *Install) {
	t.Helper()
	in := fixtureInstall(t)
	return NewEmu(in.EmuConfig(), NewUnhandledLogger(io.Discard)), in
}

func cfgEtags(e *EmuState) map[string]string {
	out := make(map[string]string, len(e.cfgDoc))
	for key, snap := range e.cfgDoc {
		out[key] = hex.EncodeToString(snap.etag)
	}
	return out
}

func queuedDownlinks(e *EmuState) int {
	n := 0
	for _, q := range e.queue {
		n += len(q)
	}
	return n
}

// zoneRole returns the 015e role byte the served document holds for one zone.
func zoneRole(t *testing.T, e *EmuState, euiHex string, zone int) (byte, bool) {
	t.Helper()
	snap, ok := e.cfgDoc[euiHex]
	if !ok {
		t.Fatalf("no config document for %s", euiHex)
	}
	for _, rec := range nodeTableRecs(parseTLVList(snap.body)) {
		if int(rec.Node) == zone {
			return rec.Role, true
		}
	}
	return 0, false
}

// TestSetInstallUnchangedIsNoOp: the boot-time reload runs on the installation
// the emulator already holds, so it must queue nothing and move no ETag, both
// on the merge path and on the EnqueueConfig path a known logical id takes.
func TestSetInstallUnchangedIsNoOp(t *testing.T) {
	e, in := fixtureEmu(t)
	e.node(mustHex(bedroomEUIHex)).LastID = 10 // takes the EnqueueConfig path

	etags := cfgEtags(e)
	zones := e.ZoneDirectory()
	for pass := 1; pass <= 2; pass++ {
		added, removed, err := e.SetInstall(in)
		if err != nil {
			t.Fatalf("pass %d: SetInstall: %v", pass, err)
		}
		if len(added) != 0 || len(removed) != 0 {
			t.Errorf("pass %d: added = %v, removed = %v, want none", pass, added, removed)
		}
		if got := queuedDownlinks(e); got != 0 {
			t.Errorf("pass %d: %d downlinks queued, want none", pass, got)
		}
		if got := cfgEtags(e); !reflect.DeepEqual(got, etags) {
			t.Errorf("pass %d: config ETags moved:\n got %v\nwant %v", pass, got, etags)
		}
		if got := e.ZoneDirectory(); !reflect.DeepEqual(got, zones) {
			t.Errorf("pass %d: zone directory = %v, want %v", pass, got, zones)
		}
	}
}

// TestSetInstallKeepsRestoredConfigDocument: a restart restores the served
// d/config documents and then reloads the installation over them. The reload
// merges the installation-owned tags back into a document an earlier push has
// extended, so it must leave that document byte for byte: a tag that keeps its
// value but moves mints a new ETag, and the device that cached the old one gets
// the whole document again instead of a 2.03.
func TestSetInstallKeepsRestoredConfigDocument(t *testing.T) {
	pushed, in := fixtureEmu(t)
	fahrenheit := true
	if err := pushed.EnqueueConfig(bedroomEUIHex, 10, ConfigWrite{TempUnitF: &fahrenheit}); err != nil {
		t.Fatalf("EnqueueConfig: %v", err)
	}
	want := pushed.cfgDoc[bedroomEUIHex]
	if len(want.body) == 0 {
		t.Fatal("the push recorded no document")
	}

	e, _ := fixtureEmu(t)
	e.node(mustHex(bedroomEUIHex)).LastID = 10 // takes the EnqueueConfig path
	if err := e.RestoreDesiredState(pushed.ExportDesiredState()); err != nil {
		t.Fatalf("RestoreDesiredState: %v", err)
	}
	if _, _, err := e.SetInstall(in); err != nil {
		t.Fatalf("SetInstall: %v", err)
	}
	got := e.cfgDoc[bedroomEUIHex]
	if !bytes.Equal(got.body, want.body) {
		t.Errorf("reloaded document = %x, want the restored %x", got.body, want.body)
	}
	if !bytes.Equal(got.etag, want.etag) {
		t.Errorf("reloaded ETag = %x, want the restored %x", got.etag, want.etag)
	}
	if n := queuedDownlinks(e); n != 0 {
		t.Errorf("the reload queued %d downlinks, want none", n)
	}
}

// TestSetInstallKeepsRuntimeConfig: the reload swaps the installation-derived
// fields of EmuConfig and leaves the bootstrap and live ones alone.
func TestSetInstallKeepsRuntimeConfig(t *testing.T) {
	e, _ := fixtureEmu(t)
	e.cfg.CSLPhase = 1234
	e.cfg.ZoneGetProxy = true
	e.tx = 99

	next := variantInstall(t, [2]string{"  utc_offset_min: 60\n", "  utc_offset_min: 120\n"})
	if _, _, err := e.SetInstall(next); err != nil {
		t.Fatalf("SetInstall: %v", err)
	}
	if e.inst != next || e.cfg.Install != next {
		t.Error("the installation was not swapped")
	}
	if e.cfg.UTCOffsetMin == nil || *e.cfg.UTCOffsetMin != 120 {
		t.Errorf("cfg.UTCOffsetMin = %v, want 120", e.cfg.UTCOffsetMin)
	}
	if hex.EncodeToString(e.key) != "0f1e2d3c4b5a69788796a5b4c3d2e1f0" || hex.EncodeToString(e.selfEUI) != "09a1cefa07c51b00" {
		t.Errorf("key or self EUI = %x, %x", e.key, e.selfEUI)
	}
	if e.cfg.CSLPhase != 1234 || !e.cfg.ZoneGetProxy {
		t.Errorf("bootstrap fields changed: cslPhase=%d zoneGetProxy=%v", e.cfg.CSLPhase, e.cfg.ZoneGetProxy)
	}
	if e.tx != 99 {
		t.Errorf("tx counter = %d, want the live 99", e.tx)
	}
}

// TestSetInstallAddsDevice: a device that appears gets its config document and
// its place in the zone routing table.
func TestSetInstallAddsDevice(t *testing.T) {
	e, _ := fixtureEmu(t)
	if _, ok := e.cfgDoc[studyEUIHex]; ok {
		t.Fatal("the new device already has a config document")
	}

	next := variantInstall(t,
		[2]string{officeDevice, officeDevice + studyDevice},
		[2]string{zone3, zone3Extended},
	)
	added, removed, err := e.SetInstall(next)
	if err != nil {
		t.Fatalf("SetInstall: %v", err)
	}
	if !reflect.DeepEqual(added, []string{studyEUIHex}) {
		t.Errorf("added = %v, want [%s]", added, studyEUIHex)
	}
	if len(removed) != 0 {
		t.Errorf("removed = %v, want none", removed)
	}
	if _, ok := e.cfgDoc[studyEUIHex]; !ok {
		t.Fatal("the new device got no config document")
	}
	if role, ok := zoneRole(t, e, studyEUIHex, 3); !ok || role != plainRoleRec {
		t.Errorf("015e role for zone 3 = %#02x (present=%v), want %#02x", role, ok, plainRoleRec)
	}
	if !e.zones[3][studyEUIHex] {
		t.Errorf("zone 3 members = %v, want the new device", e.ZoneMembers(3))
	}
}

// TestSetInstallRemovesDevice: the durable per-device state of a device that
// leaves goes with it, while its NodeState stays: an undeclared node that is
// still on the air has to keep showing.
func TestSetInstallRemovesDevice(t *testing.T) {
	e, _ := fixtureEmu(t)
	e.node(mustHex(bathEUIHex)).LastID = 2
	e.childLock[bathEUIHex] = true
	e.utcSettled[bathEUIHex] = 60
	e.getRes[bathEUIHex+"|d/config"] = []byte{1}

	next := variantInstall(t, [2]string{bathDevice, ""}, [2]string{zone2, ""})
	added, removed, err := e.SetInstall(next)
	if err != nil {
		t.Fatalf("SetInstall: %v", err)
	}
	if len(added) != 0 {
		t.Errorf("added = %v, want none", added)
	}
	if !reflect.DeepEqual(removed, []string{bathEUIHex}) {
		t.Errorf("removed = %v, want [%s]", removed, bathEUIHex)
	}
	if _, ok := e.cfgDoc[bathEUIHex]; ok {
		t.Error("the removed device kept its config document")
	}
	if e.childLock[bathEUIHex] || e.utcSettled[bathEUIHex] != 0 {
		t.Error("the removed device kept its durable state")
	}
	if _, ok := e.getRes[bathEUIHex+"|d/config"]; ok {
		t.Error("the removed device kept a cached GET result")
	}
	if _, ok := e.nodes[bathEUIHex]; !ok {
		t.Error("the removed device lost its NodeState")
	}
}

// TestSetInstallRemovesZone: a zone that leaves the installation loses its
// routing entry and every cached body published on it.
func TestSetInstallRemovesZone(t *testing.T) {
	e, _ := fixtureEmu(t)
	e.zoneState[zoneKey(10, "z/s")] = []byte{1, 2, 3}
	e.zoneState[zoneKey(1, "z/s")] = []byte{4, 5, 6}
	e.zoneRep[10] = &demandReport{raw: map[string][]byte{}}
	e.zoneRep[1] = &demandReport{raw: map[string][]byte{}}

	next := variantInstall(t, [2]string{zone10, ""})
	if _, _, err := e.SetInstall(next); err != nil {
		t.Fatalf("SetInstall: %v", err)
	}
	if _, ok := e.zones[10]; ok {
		t.Errorf("zone 10 kept its routing entry %v", e.ZoneMembers(10))
	}
	if _, ok := e.zoneState[zoneKey(10, "z/s")]; ok {
		t.Error("zone 10 kept its cached state")
	}
	if _, ok := e.zoneRep[10]; ok {
		t.Error("zone 10 kept its demand report")
	}
	if _, ok := e.zoneState[zoneKey(1, "z/s")]; !ok {
		t.Error("a surviving zone lost its cached state")
	}
	if _, ok := e.zoneRep[1]; !ok {
		t.Error("a surviving zone lost its demand report")
	}
	if _, ok := e.cfgDoc[bedroomEUIHex]; !ok {
		t.Error("the device of the removed zone lost its config document while still declared")
	}
}

// TestSetInstallKeepsLearnedZoneMember: membership learned from the traffic we
// route is not in any declaration, so a reload of a surviving zone must not
// drop it, while a member the new installation no longer declares goes.
func TestSetInstallKeepsLearnedZoneMember(t *testing.T) {
	e, _ := fixtureEmu(t)
	learned := mustHex("ff016a3107c51b00")
	e.addZoneEUI(1, learned)
	e.addZoneEUI(10, learned)

	next := variantInstall(t, [2]string{zone10, ""})
	if _, _, err := e.SetInstall(next); err != nil {
		t.Fatalf("SetInstall: %v", err)
	}
	if !e.zones[1][hexEUI(learned)] {
		t.Errorf("zone 1 members = %v, want the learned node kept", e.ZoneMembers(1))
	}
	if !e.zones[1]["05a2cafa07c51b00"] {
		t.Errorf("zone 1 members = %v, want the declared thermostat", e.ZoneMembers(1))
	}
	if _, ok := e.zones[10]; ok {
		t.Error("a learned member kept a zone the installation dropped alive")
	}
}

// TestSetInstallReelectsDemandReporter: moving the election changes the 015e
// record of both kitchen valves and reaches each of them as a config write,
// because both know a logical id. A second reload with the same installation
// adds nothing.
func TestSetInstallReelectsDemandReporter(t *testing.T) {
	e, _ := fixtureEmu(t)
	e.node(mustHex(kitchenAEUIHex)).LastID = 5
	e.node(mustHex(kitchenBEUIHex)).LastID = 5

	if role, _ := zoneRole(t, e, kitchenBEUIHex, 5); role != electedRoleRec {
		t.Fatalf("the fixture reporter starts at role %#02x, want %#02x", role, electedRoleRec)
	}

	next := variantInstall(t, [2]string{zone5, zone5Reelect})
	if _, _, err := e.SetInstall(next); err != nil {
		t.Fatalf("SetInstall: %v", err)
	}
	if role, _ := zoneRole(t, e, kitchenAEUIHex, 5); role != electedRoleRec {
		t.Errorf("the new reporter has role %#02x, want %#02x", role, electedRoleRec)
	}
	if role, _ := zoneRole(t, e, kitchenBEUIHex, 5); role != plainRoleRec {
		t.Errorf("the demoted reporter has role %#02x, want %#02x", role, plainRoleRec)
	}
	if got := queuedDownlinks(e); got != 2 {
		t.Fatalf("%d config writes queued, want one per kitchen valve", got)
	}
	etags := cfgEtags(e)
	if _, _, err := e.SetInstall(next); err != nil {
		t.Fatalf("second SetInstall: %v", err)
	}
	if got := queuedDownlinks(e); got != 2 {
		t.Errorf("%d downlinks queued after the second reload, want the same 2", got)
	}
	if got := cfgEtags(e); !reflect.DeepEqual(got, etags) {
		t.Error("the second reload moved a config ETag")
	}
}

// TestSetInstallRejectsUnusableInstall: SetInstall trusts the caller for the
// full validation but still refuses an installation it cannot address the PAN
// with, because that would swap in a key or an address of the wrong size.
func TestSetInstallRejectsUnusableInstall(t *testing.T) {
	e, in := fixtureEmu(t)
	if _, _, err := e.SetInstall(nil); err == nil {
		t.Error("SetInstall(nil) = nil error")
	}
	unvalidated := &Install{Version: ConfigVersion, HomeID: 1, NetworkKeyHex: in.NetworkKeyHex, Coordinator: in.Coordinator}
	if _, _, err := e.SetInstall(unvalidated); err == nil {
		t.Error("SetInstall on an install with no resolved key = nil error")
	}
	if err := unvalidated.ValidateCommissionable(true); err != nil {
		t.Fatalf("ValidateCommissionable: %v", err)
	}
	unvalidated.Coordinator.EUI = "aa01"
	if _, _, err := e.SetInstall(unvalidated); err == nil {
		t.Error("SetInstall on an install with an unusable coordinator EUI = nil error")
	}
	if e.inst != in {
		t.Error("a refused SetInstall replaced the installation")
	}
}

// TestSetInstallPokesZoneOnOpenWindowChange: the open window detection of a
// zone rewrites the zone configuration document, which no z/s write carries.
// Every member gets its own d/config document back unchanged, and that poke is
// what makes the device read h/<home>/z/N/config again.
func TestSetInstallPokesZoneOnOpenWindowChange(t *testing.T) {
	e, in := fixtureEmu(t)
	members := []string{kitchenAEUIHex, kitchenBEUIHex}
	for _, eui := range members {
		e.node(mustHex(eui)).LastID = 5
	}
	before, err := BuildZoneConfigDocument(in, 5)
	if err != nil {
		t.Fatal(err)
	}
	documents := map[string]cfgSnapshot{}
	for _, eui := range members {
		documents[eui] = e.cfgDoc[eui]
	}

	next := variantInstall(t, [2]string{zone5, zone5NoWindow})
	if _, _, err := e.SetInstall(next); err != nil {
		t.Fatalf("SetInstall: %v", err)
	}

	after, err := BuildZoneConfigDocument(next, 5)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(before.ETag, after.ETag) {
		t.Errorf("zone document ETag = %x, want one the changed document does not share", after.ETag)
	}
	if got := queuedDownlinks(e); got != len(members) {
		t.Errorf("%d downlinks queued, want one per member (%d)", got, len(members))
	}
	for _, eui := range members {
		q := e.queue[eui]
		if len(q) != 1 {
			t.Errorf("%s holds %d downlinks, want one", eui, len(q))
			continue
		}
		if q[0].kind != "config" {
			t.Errorf("%s got a %q downlink, want a config write", eui, q[0].kind)
		}
		got, want := e.cfgDoc[eui], documents[eui]
		if !bytes.Equal(got.body, want.body) || !bytes.Equal(got.etag, want.etag) {
			t.Errorf("%s document = %x/%x, want the unchanged %x/%x", eui, got.body, got.etag, want.body, want.etag)
		}
	}

	// The zone the poke did not name keeps its members quiet.
	if q := e.queue[bedroomEUIHex]; len(q) != 0 {
		t.Errorf("zone 10 got %d downlinks, want none", len(q))
	}
	// A second reload on the same installation changes nothing, so it pokes nobody.
	if _, _, err := e.SetInstall(next); err != nil {
		t.Fatalf("second SetInstall: %v", err)
	}
	if got := queuedDownlinks(e); got != len(members) {
		t.Errorf("%d downlinks after the second reload, want the %d of the first", got, len(members))
	}
}
