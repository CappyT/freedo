package tado

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

// zone10DefaultETag is the ETag of the zone 10 document of the fixture with
// the default gains. TestZoneConfigDocumentMatchesCapture pins its body to the
// air; a changed value here means every valve would take a 2.05 and re-init
// its controller.
const zone10DefaultETag = "b8fb9dc0b86042c5"

// TestZonePIDDefaultKeepsTheDocument: a zone with no gains and a zone that
// states the default ones serve the captured tags 6080=03e8, 60a0=01f4 and
// 60c0=05dc under the same ETag as before the gains became a setting.
func TestZonePIDDefaultKeepsTheDocument(t *testing.T) {
	plain, err := BuildZoneConfigDocument(fixtureInstall(t), 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(plain.ETag); got != zone10DefaultETag {
		t.Errorf("default ETag = %s, want %s", got, zone10DefaultETag)
	}
	body := hex.EncodeToString(plain.Body)
	if !strings.Contains(body, "60800203e8"+"60a00201f4"+"60c00205dc") {
		t.Errorf("body = %s, want the default gains", body)
	}

	in := fixtureInstall(t)
	in.Options.ZonePIDs = map[int]ZonePID{10: DefaultZonePID}
	if err := in.Validate(); err != nil {
		t.Fatal(err)
	}
	stated, err := BuildZoneConfigDocument(in, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stated.Body, plain.Body) || !bytes.Equal(stated.ETag, plain.ETag) {
		t.Errorf("stated default = %x %x, want %x %x", stated.ETag, stated.Body, plain.ETag, plain.Body)
	}
}

// TestZonePIDServed: the gains of a zone go into its own document under a new
// ETag, with the same length; the other zones keep their bytes.
func TestZonePIDServed(t *testing.T) {
	base := fixtureInstall(t)
	in := fixtureInstall(t)
	in.Options.ZonePIDs = map[int]ZonePID{10: {Kp: 2000, Ki: 250, Kd: 0}}
	if err := in.Validate(); err != nil {
		t.Fatal(err)
	}
	before, err := BuildZoneConfigDocument(base, 10)
	if err != nil {
		t.Fatal(err)
	}
	after, err := BuildZoneConfigDocument(in, 10)
	if err != nil {
		t.Fatal(err)
	}
	tlvs := ParseTLVs(after.Body)
	for tag, want := range map[string]string{"6080": "07d0", "60a0": "00fa", "60c0": "0000"} {
		if got := tlvValue(tlvs, tag); got != want {
			t.Errorf("%s = %q, want %s", tag, got, want)
		}
	}
	if len(after.Body) != len(before.Body) {
		t.Errorf("body length = %d, want %d", len(after.Body), len(before.Body))
	}
	if bytes.Equal(after.ETag, before.ETag) {
		t.Errorf("ETag = %x, want a value the default document does not have", after.ETag)
	}
	for _, zone := range []int{0, 2, 3, 5} {
		want, err := BuildZoneConfigDocument(base, zone)
		if err != nil {
			t.Fatal(err)
		}
		got, err := BuildZoneConfigDocument(in, zone)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got.Body, want.Body) {
			t.Errorf("zone %d moved with the gains of zone 10", zone)
		}
	}
}

// TestZonePIDReachesTheValveByItsRead: a reload that moves only the gains
// queues no write. The valve's next conditional GET of the document, with the
// ETag it holds, gets a 2.05 with the new gains and the new ETag.
func TestZonePIDReachesTheValveByItsRead(t *testing.T) {
	e, in := fixtureEmu(t)
	held, err := BuildZoneConfigDocument(in, 10)
	if err != nil {
		t.Fatal(err)
	}
	next := fixtureInstall(t)
	next.Options.ZonePIDs = map[int]ZonePID{10: {Kp: 1500, Ki: 500, Kd: 1500}}
	if err := next.Validate(); err != nil {
		t.Fatal(err)
	}
	queued := queuedDownlinks(e)
	if _, _, err := e.SetInstall(next); err != nil {
		t.Fatal(err)
	}
	if n := queuedDownlinks(e); n != queued {
		t.Errorf("queued downlinks = %d after the reload, want %d", n, queued)
	}

	// One 16-byte block keeps the answer inside a single frame.
	raw := deviceUplink(t, mustHex(bedroomEUIHex), CoAPReq{
		Type: "CON", Code: "0.01", MID: 0xd101, Token: []byte{0x0e},
		Path:    []string{"h", "7654321", "z", "10", "config"},
		Options: []CoAPOption{{Num: 4, Val: held.ETag}, {Num: 23, Val: []byte{0x00}}},
	})
	m := servedAnswer(t, e.OnFrame(raw), 0xd101)
	if m.Code != "2.05" {
		t.Fatalf("conditional GET with the old ETag = %s, want 2.05", m.Code)
	}
	etag, _ := m.Option(4)
	if bytes.Equal(etag.Val, held.ETag) {
		t.Errorf("ETag = %x, want a new one", etag.Val)
	}
	want, err := BuildZoneConfigDocument(next, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(etag.Val, want.ETag) {
		t.Errorf("ETag = %x, want %x", etag.Val, want.ETag)
	}
}

func TestZonePIDCheck(t *testing.T) {
	for _, tc := range []struct {
		pid  ZonePID
		want string
	}{
		{DefaultZonePID, ""},
		{ZonePID{Kp: 1000, Ki: 0, Kd: 0}, ""},
		{ZonePID{Kp: 0, Ki: 500, Kd: 0}, ""},
		{ZonePID{Kp: 0xffff, Ki: 0xffff, Kd: 0xffff}, ""},
		{ZonePID{Kp: 0, Ki: 0, Kd: 1500}, "kp and ki cannot both be 0"},
		{ZonePID{Kp: -1, Ki: 500, Kd: 1500}, "kp must be within 0..65535"},
		{ZonePID{Kp: 1000, Ki: 0x10000, Kd: 1500}, "ki must be within 0..65535"},
		{ZonePID{Kp: 1000, Ki: 500, Kd: 0x10000}, "kd must be within 0..65535"},
	} {
		if got := tc.pid.Check(); got != tc.want {
			t.Errorf("%+v: Check() = %q, want %q", tc.pid, got, tc.want)
		}
	}
}

func TestZonePIDValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		pids map[int]ZonePID
		want string
	}{
		{"unknown zone", map[int]ZonePID{4: DefaultZonePID}, "options.zone_pid.4"},
		{"hot water zone", map[int]ZonePID{0: DefaultZonePID}, "not a heating zone"},
		{"bad gain", map[int]ZonePID{10: {Kp: 0, Ki: 0, Kd: 0}}, "kp and ki cannot both be 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := fixtureInstall(t)
			in.Options.ZonePIDs = tc.pids
			err := in.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate() = %v, want an error naming %q", err, tc.want)
			}
		})
	}
}
