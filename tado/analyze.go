package tado

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Offline capture analysis. EmuState is a coordinator state machine and ignores
// every frame the real bridge sent (it cannot answer them without breaking the
// sole-coordinator assumption). The Analyzer has no such constraint: it decodes
// BOTH directions, so the bridge-sourced downlinks - the commands the cloud
// actually issued - become readable.
//
// Read-only: it never builds a frame and never decides an answer. Not safe for
// concurrent use; drive it from one goroutine (same contract as Reassembler).

const (
	maxDLVariants = 8   // distinct payload renderings printed per downlink group
	maxULVariants = 3   // ... per uplink group (telemetry varies every frame)
	maxDatagrams  = 20  // reassembled datagrams listed in full
	maxPrevBytes  = 160 // bytes of a non-TLV payload preview
)

// AnalyzerStats are the per-capture counters.
type AnalyzerStats struct {
	Frames       int // every frame fed to Add
	Data         int // Data + Data-pending
	Downlinks    int // Data frames with the bridge source prefix
	Uplinks      int // Data frames from a device
	Fragments    int // 6LoWPAN fragment frames, both directions
	Reassembled  int // completed datagrams
	MICFails     int
	Malformed    int
	AppParseFail int
	EnhAcks      int
	WakeUps      int
	Broadcasts   int
	Unknown      int
}

// Other returns the frames that are counted but not decoded.
func (s AnalyzerStats) Other() int { return s.EnhAcks + s.WakeUps + s.Broadcasts + s.Unknown }

// tlvVariant is one distinct payload rendering seen inside a group.
type tlvVariant struct {
	text string
	n    int
}

// msgGroup aggregates every message of one resource exchanged with one device.
type msgGroup struct {
	res      string
	peer     string
	count    int
	reasm    int
	codes    map[string]int
	variants []tlvVariant
}

func (g *msgGroup) addVariant(text string) {
	for i := range g.variants {
		if g.variants[i].text == text {
			g.variants[i].n++
			return
		}
	}
	g.variants = append(g.variants, tlvVariant{text: text, n: 1})
}

// datagram is one completed 6LoWPAN reassembly.
type datagram struct {
	dir  string
	src  string
	dst  string
	uri  string
	code string
	size int
	body string
}

// devRecord is the per-device view of the capture.
type devRecord struct {
	key       string
	short     string
	name      string
	up        int
	down      int
	telemetry map[string]string
	upRes     map[string]int
	downRes   map[string]int
}

// Analyzer decodes a capture in both directions and reports what it saw.
type Analyzer struct {
	inst   *Install
	key    []byte
	self   []byte // our own coordinator EUI: what makes a frame a downlink
	log    *UnhandledLogger
	reasm  *Reassembler
	stats  AnalyzerStats
	dl     map[string]*msgGroup // resource+device -> bridge-sourced commands
	ul     map[string]*msgGroup // resource+device -> device-sourced traffic
	devs   map[string]*devRecord
	dgrams []datagram
}

// NewAnalyzer returns an analyzer with no installation: the key and the
// coordinator address come from the legacy globals, empty outside the tests.
func NewAnalyzer(log *UnhandledLogger) *Analyzer {
	return newAnalyzer(nil, NetworkKey, BridgeEUI, log)
}

// NewAnalyzerFor returns an analyzer over one installation's key, naming its
// nodes and telling a downlink apart by its coordinator address.
func NewAnalyzerFor(in *Install, log *UnhandledLogger) *Analyzer {
	return newAnalyzer(in, in.NetworkKey(), in.CoordinatorEUI(), log)
}

func newAnalyzer(in *Install, key, self []byte, log *UnhandledLogger) *Analyzer {
	if log == nil {
		log = NewUnhandledLogger(nil)
	}
	return &Analyzer{
		inst:  in,
		key:   key,
		self:  self,
		log:   log,
		reasm: NewReassembler(),
		dl:    map[string]*msgGroup{},
		ul:    map[string]*msgGroup{},
		devs:  map[string]*devRecord{},
	}
}

// Stats returns a snapshot of the counters.
func (a *Analyzer) Stats() AnalyzerStats { return a.stats }

func (a *Analyzer) name(wireEUI []byte) string { return nameOf(a.inst, wireEUI) }

// Add decodes one received MAC frame (no FCS) into the analysis.
func (a *Analyzer) Add(raw []byte) {
	a.stats.Frames++
	class := Classify(raw)
	switch class {
	case ClassData, ClassDataPending:
	case ClassEnhAck:
		a.stats.EnhAcks++
		return
	case ClassWakeUp:
		a.stats.WakeUps++
		return
	case ClassBroadcast:
		a.stats.Broadcasts++
		return
	default:
		a.stats.Unknown++
		return
	}

	a.stats.Data++
	f, err := ParseData(a.key, raw)
	if err != nil {
		reason := rsnMalformed
		if errors.Is(err, ErrMICFail) {
			reason = rsnMICFail
			a.stats.MICFails++
		} else {
			a.stats.Malformed++
		}
		a.log.Log(class.String(), "", "", reason, loggedFrameHex(raw))
		return
	}

	down := len(a.self) == 8 && bytes.Equal(f.SrcPrefix, a.self[:5])
	peer := f.SrcEUI()
	if down {
		a.stats.Downlinks++
		peer = f.DstEUI
	} else {
		a.stats.Uplinks++
	}
	dev := a.dev(peer)
	if down {
		dev.down++
	} else {
		dev.up++
	}

	if IsFragment(f.Plaintext) {
		a.stats.Fragments++
		m, done := a.reasm.Add(f)
		if !done {
			return
		}
		a.stats.Reassembled++
		a.record(f, class, down, dev, m, parseTLVList(m.Payload), true)
		return
	}

	app := ParseApp(f.Plaintext)
	if app == nil {
		a.stats.AppParseFail++
		a.log.Log(class.String(), a.name(f.SrcEUI()), a.name(f.DstEUI), rsnAppParse, f.Hex())
		return
	}
	a.record(f, class, down, dev, app.CoAP, app.TLVs, false)
}

// record files one decoded message under its resource group and its device.
func (a *Analyzer) record(f *Frame, class FrameClass, down bool, dev *devRecord, m *CoAPMessage, tlvs []TLV, reasm bool) {
	res, code := "(no-coap)", ""
	var payload []byte
	if m != nil {
		if res = m.URI(); res == "" {
			res = "(no-uri)"
		}
		code = m.Type + " " + m.Code
		payload = m.Payload
	}
	srcLbl, dstLbl := a.name(f.SrcEUI()), a.name(f.DstEUI)
	tagRes := ResourceOf(res, tlvs)

	var body string
	switch {
	case m == nil:
		body = renderTLVs(tagRes, tlvs) // heuristic suffix TLVs: no CoAP payload to measure against
	case len(payload) == 0:
		body = "(no payload)"
	case tlvBytes(tlvs)*10 >= len(payload)*9:
		body = renderTLVs(tagRes, tlvs)
	default:
		// TLV parsing left most of the payload unconsumed, so it is not a TLV list
		// (a coap:// resource listing, say). Decoding it yields noise and a flood
		// of unknown tags.
		body, tlvs = "raw:"+preview(payload), nil
	}

	groups := a.ul
	if down {
		groups = a.dl
	}
	g := a.group(groups, res, dev)
	g.count++
	if reasm {
		g.reasm++
	}
	if code != "" {
		g.codes[code]++
	}
	g.addVariant(body)

	if reasm && len(a.dgrams) < maxDatagrams {
		a.dgrams = append(a.dgrams, datagram{
			dir: dirName(down), src: srcLbl, dst: dstLbl,
			uri: res, code: code, size: len(payload), body: body,
		})
	}

	if down {
		dev.downRes[res]++
	} else {
		dev.upRes[res]++
	}

	if m != nil && m.Path != "" && !knownResource(m.Path) {
		a.log.Log(class.String(), srcLbl, dstLbl, rsnUnknownRes+" path="+res, f.Hex())
	}
	for _, t := range tlvs {
		info, ok := tagFor(t.Tag, tagRes)
		if !ok {
			a.log.Log(class.String(), srcLbl, dstLbl, rsnUnknownTag+"="+t.Tag, f.Hex())
			continue
		}
		if !down {
			dev.telemetry[info.name] = info.decode(t.Val)
		}
	}
}

// Report renders the grouped capture analysis.
func (a *Analyzer) Report() string {
	var b strings.Builder
	s := a.stats
	fmt.Fprintf(&b, "capture analysis: %d frames  data=%d downlink=%d uplink=%d  frag=%d reasm=%d  mic-fail=%d malformed=%d app-parse-fail=%d  other=%d (enh-ack=%d wakeup=%d bcast=%d unknown=%d)\n",
		s.Frames, s.Data, s.Downlinks, s.Uplinks, s.Fragments, s.Reassembled,
		s.MICFails, s.Malformed, s.AppParseFail, s.Other(),
		s.EnhAcks, s.WakeUps, s.Broadcasts, s.Unknown)

	writeGroups(&b, "downlink commands (bridge -> device)", a.dl, maxDLVariants)
	writeGroups(&b, "uplink traffic (device -> bridge)", a.ul, maxULVariants)

	fmt.Fprintf(&b, "\nreassembled datagrams: %d\n", s.Reassembled)
	for _, d := range a.dgrams {
		fmt.Fprintf(&b, "  %s %s -> %s  %s %s  %d bytes\n", d.dir, d.src, d.dst, d.code, d.uri, d.size)
		fmt.Fprintf(&b, "        %s\n", d.body)
	}
	if s.Reassembled > len(a.dgrams) {
		fmt.Fprintf(&b, "  ... %d more (see the downlink/uplink groups)\n", s.Reassembled-len(a.dgrams))
	}

	devs := make([]*devRecord, 0, len(a.devs))
	for _, d := range a.devs {
		devs = append(devs, d)
	}
	sort.Slice(devs, func(i, j int) bool { return devs[i].short < devs[j].short })
	fmt.Fprintf(&b, "\ndevices: %d\n", len(devs))
	for _, d := range devs {
		fmt.Fprintf(&b, "  %-6s %-12s up=%-4d down=%-4d %s\n", d.short, d.name, d.up, d.down, sortedKV(d.telemetry))
		if len(d.downRes) > 0 {
			fmt.Fprintf(&b, "        downlink: %s\n", resList(d.downRes))
		}
		if len(d.upRes) > 0 {
			fmt.Fprintf(&b, "        uplink:   %s\n", resList(d.upRes))
		}
	}

	fmt.Fprintf(&b, "\nunhandled: %d events\n", a.log.Total())
	for _, line := range strings.Split(strings.TrimRight(a.log.Summary(), "\n"), "\n") {
		if line != "" {
			fmt.Fprintf(&b, "  %s\n", line)
		}
	}
	return b.String()
}

// --- helpers ---

func (a *Analyzer) dev(wireEUI []byte) *devRecord {
	key := hexEUI(wireEUI)
	if d, ok := a.devs[key]; ok {
		return d
	}
	d := &devRecord{
		key:       key,
		short:     ShortFromWire(wireEUI),
		name:      a.name(wireEUI),
		telemetry: map[string]string{},
		upRes:     map[string]int{},
		downRes:   map[string]int{},
	}
	a.devs[key] = d
	return d
}

func (a *Analyzer) group(m map[string]*msgGroup, res string, dev *devRecord) *msgGroup {
	key := res + "\x00" + dev.key
	if g, ok := m[key]; ok {
		return g
	}
	g := &msgGroup{res: res, peer: dev.name + "/" + dev.short, codes: map[string]int{}}
	m[key] = g
	return g
}

func writeGroups(b *strings.Builder, title string, m map[string]*msgGroup, maxVar int) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Fprintf(b, "\n%s: %d groups\n", title, len(keys))
	for _, k := range keys {
		g := m[k]
		note := ""
		if g.reasm > 0 {
			note = fmt.Sprintf("  reassembled=%d", g.reasm)
		}
		fmt.Fprintf(b, "  %-30s %-18s %4d msg  %s%s\n", g.res, g.peer, g.count, codeList(g.codes), note)
		vs := append([]tlvVariant{}, g.variants...)
		sort.SliceStable(vs, func(i, j int) bool {
			if vs[i].n != vs[j].n {
				return vs[i].n > vs[j].n
			}
			return vs[i].text < vs[j].text
		})
		for i, v := range vs {
			if i == maxVar {
				fmt.Fprintf(b, "        ... %d more distinct payloads\n", len(vs)-maxVar)
				break
			}
			fmt.Fprintf(b, "        x%-4d %s\n", v.n, v.text)
		}
	}
}

func renderTLVs(res TLVResource, ts []TLV) string {
	if len(ts) == 0 {
		return "(no payload)"
	}
	parts := make([]string, 0, len(ts))
	for _, t := range ts {
		parts = append(parts, t.TagLabelIn(res))
	}
	return strings.Join(parts, " ")
}

func codeList(c map[string]int) string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s x%d", k, c[k]))
	}
	return strings.Join(parts, ", ")
}

func resList(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s(%d)", k, m[k]))
	}
	return strings.Join(parts, " ")
}

func sortedKV(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, " ")
}

func dirName(down bool) string {
	if down {
		return "DL"
	}
	return "UL"
}

// tlvBytes is the payload length the parsed TLVs account for.
func tlvBytes(ts []TLV) int {
	n := 0
	for _, t := range ts {
		n += 3 + len(t.Val)
	}
	return n
}

// preview renders a payload that is not a TLV list: ASCII when it reads as text
// (a coap:// resource listing), hex otherwise.
func preview(b []byte) string {
	if len(b) > maxPrevBytes {
		b = b[:maxPrevBytes]
	}
	n := 0
	for _, c := range b {
		if c >= 0x20 && c < 0x7f {
			n++
		}
	}
	if n*2 < len(b) { // half printable: a fragment tail still shows its URIs
		return hex.EncodeToString(b)
	}
	out := make([]byte, len(b))
	for i, c := range b {
		out[i] = '.'
		if c >= 0x20 && c < 0x7f {
			out[i] = c
		}
	}
	return string(out)
}
