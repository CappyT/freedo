package daemon

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"freedo/cc1101"
	"freedo/internal/radio"
	"freedo/tado"
)

// The synthetic installation the reload tests run on. It is not the capture
// fixture: no frame is replayed here, only the daemon bookkeeping is.
const installReloadYAML = `
version: 1
home_id: 7801
network_key: "0102030405060708090a0b0c0d0e0f10"
coordinator: {eui: "aa016a3107c51b00", serial: IB9000000001}
devices:
  - {kind: thermostat, eui: "03046a3107c51b00", serial: RU9000000001, label: sala}
  - {kind: valve, eui: "01026a3107c51b00", serial: VA9000000001, label: sala}
  - {kind: valve, eui: "11226a3107c51b00", serial: VA9000000002, label: studio}
zones:
  - {number: 1, name: sala, kind: heating, circuit: 1, members: [RU9000000001, VA9000000001], reporter: VA9000000001}
  - {number: 4, name: studio, kind: heating, circuit: 1, members: [VA9000000002], reporter: VA9000000002}
circuits:
  - {id: 1, name: casa, heat_source: RU9000000001}
`

// The lines the reload tests delete, and the wire EUIs they name.
const (
	reloadStudioDevice = "  - {kind: valve, eui: \"11226a3107c51b00\", serial: VA9000000002, label: studio}\n"
	reloadStudioZone   = "  - {number: 4, name: studio, kind: heating, circuit: 1, members: [VA9000000002], reporter: VA9000000002}\n"

	reloadValveEUI    = "01026a3107c51b00" // VA9000000001, zone 1
	reloadStudioEUI   = "11226a3107c51b00" // VA9000000002, zone 4, the one that leaves
	reloadStudioShort = "2211"
)

// reloadInstall edits the synthetic installation and validates the result.
func reloadInstall(t *testing.T, edits ...[2]string) *tado.Install {
	t.Helper()
	doc := installReloadYAML
	for _, edit := range edits {
		if !strings.Contains(doc, edit[0]) {
			t.Fatalf("the reload fixture does not contain %q", edit[0])
		}
		doc = strings.Replace(doc, edit[0], edit[1], 1)
	}
	in, err := tado.Parse([]byte(doc))
	if err != nil {
		t.Fatalf("reload install: %v", err)
	}
	return in
}

// reloadNoStudio is the same installation without the studio valve and its zone.
func reloadNoStudio(t *testing.T) *tado.Install {
	t.Helper()
	return reloadInstall(t, [2]string{reloadStudioDevice, ""}, [2]string{reloadStudioZone, ""})
}

// reloadDaemon builds an armed daemon over in, with link maintenance enrolled
// and no engine goroutine: the tests call the engine-side methods directly.
func reloadDaemon(t *testing.T, in *tado.Install) (*daemon, *immediateTestRadio) {
	t.Helper()
	cfg := in.EmuConfig()
	cfg.TxCounter = 100000
	emu := tado.NewEmu(cfg, tado.NewUnhandledLogger(io.Discard))
	radio := newImmediateTestRadio(true, nil)
	d := newDaemon(in, emu, radio, false, "live")
	d.initLinkMaintenance()
	return d, radio
}

// seedTargetState fills every per-target daemon map for one target.
func seedTargetState(d *daemon, target string) {
	d.retryDue[target] = 3
	d.retryAt[target] = time.Now().Add(time.Minute)
	d.retryMID[target] = 0x1234
	d.coapRetryBase[target] = coapRetryState{base: time.Second, mid: 0x1234}
	d.dispatchDue[target] = true
	d.dispatchQ = append(d.dispatchQ, target)
	d.cslAdmissionWait[target] = cslAdmissionQueueFull
}

// targetStateMaps names the daemon maps that still hold target.
func targetStateMaps(d *daemon, target string) []string {
	var out []string
	if _, ok := d.retryDue[target]; ok {
		out = append(out, "retryDue")
	}
	if _, ok := d.retryAt[target]; ok {
		out = append(out, "retryAt")
	}
	if _, ok := d.retryMID[target]; ok {
		out = append(out, "retryMID")
	}
	if _, ok := d.coapRetryBase[target]; ok {
		out = append(out, "coapRetryBase")
	}
	if _, ok := d.dispatchDue[target]; ok {
		out = append(out, "dispatchDue")
	}
	if _, ok := d.cslAdmissionWait[target]; ok {
		out = append(out, "cslAdmissionWait")
	}
	if _, ok := d.linkMaint[target]; ok {
		out = append(out, "linkMaint")
	}
	for _, queued := range d.dispatchQ {
		if queued == target {
			out = append(out, "dispatchQ")
			break
		}
	}
	return out
}

// daemonStateSnapshot copies every per-target map, for the tests that assert
// nothing moved.
func daemonStateSnapshot(d *daemon) map[string]any {
	return map[string]any{
		"retryDue":         maps(d.retryDue),
		"retryMID":         maps(d.retryMID),
		"coapRetryBase":    maps(d.coapRetryBase),
		"dispatchDue":      maps(d.dispatchDue),
		"cslAdmissionWait": maps(d.cslAdmissionWait),
		"linkMaint":        maps(d.linkMaint),
		"linkMaintOrder":   append([]string(nil), d.linkMaintOrder...),
		"linkMaintCursor":  d.linkMaintCursor,
		"dispatchQ":        append([]string(nil), d.dispatchQ...),
	}
}

func maps[V any](in map[string]V) map[string]V {
	out := make(map[string]V, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// TestSetInstallRemovesDeviceState: a device the reload drops leaves every
// daemon map, the link-maintenance cursor stays in range and the next tick
// neither panics nor addresses it.
func TestSetInstallRemovesDeviceState(t *testing.T) {
	d, radio := reloadDaemon(t, reloadInstall(t))
	seedTargetState(d, reloadStudioEUI)
	seedTargetState(d, reloadValveEUI)
	d.cslAdmissionWait[reloadStudioShort] = cslAdmissionNeighborFull
	d.linkMaintCursor = len(d.linkMaintOrder) - 1

	added, removed, err := d.setInstall(reloadNoStudio(t))
	if err != nil {
		t.Fatalf("setInstall: %v", err)
	}
	if len(added) != 0 || !reflect.DeepEqual(removed, []string{reloadStudioEUI}) {
		t.Fatalf("added = %v, removed = %v, want only the studio valve removed", added, removed)
	}
	if held := targetStateMaps(d, reloadStudioEUI); held != nil {
		t.Errorf("the removed device still sits in %v", held)
	}
	if _, ok := d.cslAdmissionWait[reloadStudioShort]; ok {
		t.Error("the removed device still holds a CSL admission wait under its short address")
	}
	if held := targetStateMaps(d, reloadValveEUI); len(held) != 8 {
		t.Errorf("the surviving device kept only %v", held)
	}
	for _, key := range d.linkMaintOrder {
		if key == reloadStudioEUI {
			t.Fatalf("link maintenance order still names the removed device: %v", d.linkMaintOrder)
		}
	}
	if d.linkMaintCursor < 0 || d.linkMaintCursor >= len(d.linkMaintOrder) {
		t.Fatalf("link maintenance cursor = %d, out of range for %d targets", d.linkMaintCursor, len(d.linkMaintOrder))
	}

	// Every surviving target gets its turn; the removed one must get none.
	d.linkMaintTick = linkMaintStartupTicks
	for key := range d.linkMaint {
		d.linkMaint[key] = linkMaintEntry{state: 1, retry: 0}
	}
	for range (linkMaintPostSendTicks + 1) * (len(d.linkMaintOrder) + 1) {
		d.linkMaintenanceTick(d.emu)
	}
	radio.mu.Lock()
	defer radio.mu.Unlock()
	if len(radio.batches) == 0 {
		t.Fatal("link maintenance sent nothing after the reload")
	}
	studio := mustDecodeEUI(t, reloadStudioEUI)
	for _, batch := range radio.batches {
		for _, action := range batch {
			if len(action.Bytes) >= 11 && string(action.Bytes[3:11]) == string(studio) {
				t.Fatalf("link maintenance addressed the removed device: %s", action.Node)
			}
		}
	}
}

// TestSetInstallAddsDeviceState: a device the reload declares joins link
// maintenance from scratch, and the entries already there keep their state.
func TestSetInstallAddsDeviceState(t *testing.T) {
	d, _ := reloadDaemon(t, reloadNoStudio(t))
	kept := linkMaintEntry{state: 2, retry: 3, age: 9, wait: 4}
	d.linkMaint[reloadValveEUI] = kept
	orderBefore := append([]string(nil), d.linkMaintOrder...)

	added, removed, err := d.setInstall(reloadInstall(t))
	if err != nil {
		t.Fatalf("setInstall: %v", err)
	}
	if len(removed) != 0 || !reflect.DeepEqual(added, []string{reloadStudioEUI}) {
		t.Fatalf("added = %v, removed = %v, want only the studio valve added", added, removed)
	}
	entry, ok := d.linkMaint[reloadStudioEUI]
	if !ok || entry != (linkMaintEntry{state: 1, retry: -1}) {
		t.Errorf("added device entry = %+v (present %t), want a fresh one", entry, ok)
	}
	if got := d.linkMaint[reloadValveEUI]; got != kept {
		t.Errorf("surviving entry = %+v, want %+v", got, kept)
	}
	if want := append(orderBefore, reloadStudioEUI); !reflect.DeepEqual(d.linkMaintOrder, want) {
		t.Errorf("link maintenance order = %v, want %v", d.linkMaintOrder, want)
	}
}

// TestSetInstallUnchangedKeepsDaemonState: reloading the same installation is
// the boot-time case, and it must move nothing.
func TestSetInstallUnchangedKeepsDaemonState(t *testing.T) {
	d, _ := reloadDaemon(t, reloadInstall(t))
	seedTargetState(d, reloadStudioEUI)
	before := daemonStateSnapshot(d)

	added, removed, err := d.setInstall(reloadInstall(t))
	if err != nil {
		t.Fatalf("setInstall: %v", err)
	}
	if len(added) != 0 || len(removed) != 0 {
		t.Errorf("added = %v, removed = %v, want none", added, removed)
	}
	if got := daemonStateSnapshot(d); !reflect.DeepEqual(got, before) {
		t.Errorf("daemon state moved:\n got %+v\nwant %+v", got, before)
	}
}

// TestSetInstallRejectedKeepsInstall: an installation the emulator refuses
// leaves both the served pointer and the daemon maps as they were.
func TestSetInstallRejectedKeepsInstall(t *testing.T) {
	d, _ := reloadDaemon(t, reloadInstall(t))
	seedTargetState(d, reloadStudioEUI)
	before := daemonStateSnapshot(d)
	served := d.inst.Load()

	added, removed, err := d.setInstall(&tado.Install{})
	if err == nil {
		t.Fatal("setInstall accepted an installation without a network key")
	}
	if added != nil || removed != nil {
		t.Errorf("added = %v, removed = %v, want neither on the error path", added, removed)
	}
	if d.inst.Load() != served {
		t.Error("the refused installation replaced the served one")
	}
	if got := daemonStateSnapshot(d); !reflect.DeepEqual(got, before) {
		t.Errorf("daemon state moved on the error path:\n got %+v\nwant %+v", got, before)
	}
}

// TestCSLCompletionAfterRemovalSurvives: a CSL job in flight when its device
// left the installation completes against maps that no longer hold it.
func TestCSLCompletionAfterRemovalSurvives(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result *cc1101.CSLTxResult
	}{
		{"nothing sent", nil},
		{"sent", &cc1101.CSLTxResult{Sent: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := reloadDaemon(t, reloadInstall(t))
			seedTargetState(d, reloadStudioEUI)
			if _, _, err := d.setInstall(reloadNoStudio(t)); err != nil {
				t.Fatalf("setInstall: %v", err)
			}
			frame := make([]byte, 11)
			copy(frame[3:], mustDecodeEUI(t, reloadStudioEUI))
			action := tado.Action{
				Node: reloadStudioEUI, Bytes: frame,
				QueueTarget: reloadStudioEUI, QueueAttempt: true,
			}
			d.handleCSLCompletion(radio.CSLCompletion{Action: action, Result: tc.result})
		})
	}
}

// TestSetInstallConcurrentWithAPIReaders: the API reads the installation off
// the HTTP goroutines while the engine swaps it. Every answer must be one whole
// installation, never half of each. The readers run without pause across all
// the reloads, and each reload waits until every reader has done a whole round
// since the previous one, so the overlap does not depend on the host speed.
func TestSetInstallConcurrentWithAPIReaders(t *testing.T) {
	first := reloadInstall(t)
	renamer := strings.NewReplacer("sala", "salotto", "studio", "mansarda")
	second, err := tado.Parse([]byte(renamer.Replace(installReloadYAML)))
	if err != nil {
		t.Fatalf("renamed install: %v", err)
	}

	d, _ := reloadDaemon(t, first)
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)
	srv := httptest.NewServer(d.mux())
	t.Cleanup(func() {
		srv.Close()
		cancel()
	})

	const readers, reloads = 4, 64
	// phase is 2k after k reloads and 2k+1 while reload k+1 runs. Reload i
	// serves first when i is even, second when it is odd.
	var phase atomic.Int64
	servedAt := func(p int64) int {
		if p == 0 {
			return 0
		}
		return int((p/2 - 1) % 2)
	}

	stop := make(chan struct{})
	failed := make(chan struct{})
	var failOnce sync.Once
	progress := make(chan struct{}, 1)
	rounds := make([]atomic.Int64, readers)
	// quiet[r][g] counts the rounds of reader r that ran entirely between two
	// reloads while installation g was served.
	quiet := make([][2]atomic.Int64, readers)
	var straddled atomic.Int64
	var wg sync.WaitGroup
	for r := range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if err := readRound(srv.URL, &phase, servedAt, &quiet[r], &straddled); err != nil {
					t.Error(err)
					failOnce.Do(func() { close(failed) })
					return
				}
				rounds[r].Add(1)
				select {
				case progress <- struct{}{}:
				default:
				}
			}
		}()
	}
	var stopOnce sync.Once
	stopReaders := func() {
		stopOnce.Do(func() {
			close(stop)
			wg.Wait()
		})
	}
	defer stopReaders()

	// awaitFreshRound returns once every reader has completed a round that
	// started after the call: the round in flight at the call may predate it.
	awaitFreshRound := func() {
		want := make([]int64, readers)
		for r := range rounds {
			want[r] = rounds[r].Load() + 2
		}
		for r := range rounds {
			for rounds[r].Load() < want[r] {
				select {
				case <-progress:
				case <-failed:
					t.FailNow()
				}
			}
		}
	}

	awaitFreshRound()
	for i := range reloads {
		next := first
		if i%2 == 1 {
			next = second
		}
		phase.Add(1)
		_, err := d.do(func(*tado.EmuState) (any, error) {
			_, _, err := d.setInstall(next)
			return nil, err
		})
		phase.Add(1)
		if err != nil {
			t.Fatalf("reload %d: %v", i, err)
		}
		awaitFreshRound()
	}
	stopReaders()

	for r := range readers {
		if quiet[r][0].Load() == 0 || quiet[r][1].Load() == 0 {
			t.Errorf("reader %d read the first installation in %d quiet rounds and the second in %d, want both",
				r, quiet[r][0].Load(), quiet[r][1].Load())
		}
	}
	t.Logf("%d reader rounds overlapped a reload", straddled.Load())
}

// readRound reads the three API views once. Each view must be one whole
// installation. A round that ran entirely between two reloads must also show
// exactly the installation the last reload served, in every view.
func readRound(base string, phase *atomic.Int64, servedAt func(int64) int, quiet *[2]atomic.Int64, straddled *atomic.Int64) error {
	start := phase.Load()
	zones, err := checkZonesView(base)
	if err != nil {
		return err
	}
	nodes, err := checkNodesView(base)
	if err != nil {
		return err
	}
	// This one resolves the reference on the HTTP goroutine, which is the read
	// the atomic pointer is there for.
	node, err := checkNodeView(base, "VA9000000002")
	if err != nil {
		return err
	}
	if end := phase.Load(); end != start || start%2 == 1 {
		straddled.Add(1)
		return nil
	}
	want := servedAt(start)
	if zones != want || nodes != want || node != want {
		return fmt.Errorf("after %d reloads the views show installation %d, %d, %d, want %d",
			start/2, zones, nodes, node, want)
	}
	quiet[want].Add(1)
	return nil
}

// The two names each renamed object carries, first installation then second.
var (
	reloadZoneNames  = map[int][2]string{1: {"sala", "salotto"}, 4: {"studio", "mansarda"}}
	reloadNodeRooms  = map[string][2]string{"VA9000000001": {"sala", "salotto"}, "VA9000000002": {"studio", "mansarda"}}
	errMixedInstalls = "mixes the two installations"
)

// The check*View helpers return the installation the view shows: 0 for the
// first, 1 for the second.

func checkZonesView(base string) (int, error) {
	var body struct {
		Declared []struct {
			Number int    `json:"number"`
			Name   string `json:"name"`
		} `json:"declared"`
	}
	if err := getInto(base+"/api/zones", &body); err != nil {
		return -1, err
	}
	if len(body.Declared) != len(reloadZoneNames) {
		return -1, fmt.Errorf("GET /api/zones returned %d zones, want %d", len(body.Declared), len(reloadZoneNames))
	}
	generation := -1
	for _, zone := range body.Declared {
		names, ok := reloadZoneNames[zone.Number]
		if !ok {
			return -1, fmt.Errorf("GET /api/zones returned unknown zone %d", zone.Number)
		}
		seen := -1
		for i, name := range names {
			if zone.Name == name {
				seen = i
			}
		}
		if seen < 0 {
			return -1, fmt.Errorf("zone %d is named %q, which is neither installation", zone.Number, zone.Name)
		}
		if generation >= 0 && seen != generation {
			return -1, fmt.Errorf("GET /api/zones %s: %+v", errMixedInstalls, body.Declared)
		}
		generation = seen
	}
	return generation, nil
}

func checkNodesView(base string) (int, error) {
	var nodes []struct {
		Serial string
		Room   string
	}
	if err := getInto(base+"/api/nodes", &nodes); err != nil {
		return -1, err
	}
	generation := -1
	for _, node := range nodes {
		rooms, ok := reloadNodeRooms[node.Serial]
		if !ok {
			continue
		}
		seen := -1
		for i, room := range rooms {
			if node.Room == room {
				seen = i
			}
		}
		if seen < 0 {
			return -1, fmt.Errorf("node %s is in room %q, which is neither installation", node.Serial, node.Room)
		}
		if generation >= 0 && seen != generation {
			return -1, fmt.Errorf("GET /api/nodes %s: %+v", errMixedInstalls, nodes)
		}
		generation = seen
	}
	if generation < 0 {
		return -1, fmt.Errorf("GET /api/nodes returned no declared device: %+v", nodes)
	}
	return generation, nil
}

func checkNodeView(base, serial string) (int, error) {
	var node struct {
		Serial string
		Room   string
	}
	if err := getInto(base+"/api/nodes/"+serial, &node); err != nil {
		return -1, err
	}
	rooms := reloadNodeRooms[serial]
	for i, room := range rooms {
		if node.Room == room {
			return i, nil
		}
	}
	return -1, fmt.Errorf("node %s is in room %q, which is neither installation", serial, node.Room)
}

func getInto(url string, v any) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func mustDecodeEUI(t *testing.T, euiHex string) []byte {
	t.Helper()
	wire, err := hex.DecodeString(euiHex)
	if err != nil {
		t.Fatalf("bad EUI %q: %v", euiHex, err)
	}
	return wire
}
