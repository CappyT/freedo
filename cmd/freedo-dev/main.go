// Command freedo-dev is the bench and development CLI for the Tado 868 MHz
// link: decrypt/parse captures offline, and drive the ESP32<->CC1101 bridge to
// listen and transmit. The daemon is the separate `freedo` binary.
package main

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"freedo/cc1101"
	"freedo/internal/buildinfo"
	"freedo/tado"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	args := os.Args[2:]
	switch os.Args[1] {
	case "decrypt":
		cmdDecrypt(args)
	case "frame":
		cmdFrame(args)
	case "dissect":
		cmdDissect(args)
	case "nodes":
		cmdNodes(args)
	case "ping":
		cmdPing(args)
	case "config":
		cmdConfig(args)
	case "regs":
		cmdRegs(args)
	case "autotune":
		cmdAutotune(args)
	case "rx":
		cmdRx(args)
	case "tx":
		cmdTx(args)
	case "setpoint":
		cmdSetpoint(args)
	case "emulate":
		cmdEmulate(args)
	case "coldstart":
		cmdColdStart(args)
	case "ota":
		cmdOTA(args)
	case "-h", "--help", "help":
		usage()
	case "-version", "--version", "version":
		fmt.Println(buildinfo.Stamp("freedo-dev"))
	case "daemon":
		fmt.Fprintln(os.Stderr, "the daemon is the freedo binary: run 'freedo' with no arguments")
		os.Exit(2)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `freedo-dev - Tado 868 MHz bench and development tools

offline (no hardware):
  decrypt <file>         decrypt+parse all 0x69ec frames in a capture file
  frame <hex>            decrypt+parse one on-air Data frame (hex)
  dissect <hex>|<file>   dissect any frame class (Data/EnhACK/WakeUp/Broadcast)
  nodes                  print the devices of the installation config

radio (ESP32<->CC1101 bridge, -p /dev/ttyUSB0):
  ping                   ping the bridge firmware
  config                 load the Tado PHY registers, report PART/VERSION
  regs                   dump config + status registers
  autotune               center FSCTRL0 on the live carrier, receive only, and
                         store it as the calibration; -dry prints and stores none
  rx                     listen, print CRC-valid frames, decrypt Data frames
  tx <hex>               transmit one raw MAC frame (no length byte, no FCS)
  setpoint <valve> <C>   build a setpoint command; --ref <hex> to reuse a real
                         downlink's meta+counter, --tx to transmit it

bridge emulator (the real bridge must be OFF; we are the only coordinator):
  emulate                decode, ack and answer live traffic; dry-run unless --tx
  emulate --replay <f>   run a capture file through the emulator, no hardware
                         see "freedo-dev emulate -h" for every flag
  coldstart <device>     deliver one command to a SLEEPING device with a wake-up
                         train and report whether it lands (one attempt per run)

firmware:
  ota <device>           check a firmware image against the device's fw/state and
                         print the Block1 plan; --tx hands it to the daemon API

The long-running service is the separate `+"`freedo`"+` binary: run it with no
arguments to start the daemon and its HTTP get/set API.

Every radio command takes -p <port> (default /dev/ttyUSB0).
--version prints the build stamp of this binary.

Every command that decrypts, names a device or transmits reads the installation
from the freedo database: --state <path>, else the `+"`database`"+` key of the bootstrap
file --config names (default freedo.yaml), else freedo-state.db beside it. The
daemon writes that database; these tools never commission one.
`)
}

// --- offline commands ---

func cmdDecrypt(args []string) {
	fs := flag.NewFlagSet("decrypt", flag.ExitOnError)
	configFlag(fs)
	stateFlag(fs)
	pos := parseFlags(fs, args)
	if len(pos) < 1 {
		die("decrypt needs a capture file")
	}
	in := mustInstall(fs)
	f, err := os.Open(pos[0])
	check(err)
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), 1<<16)
	ok, fail, reasm := 0, 0, 0
	var skip skipCount
	ra := tado.NewReassembler()
	for sc.Scan() {
		line := sc.Text()
		h, take := replayHex(line)
		if !take {
			skip.count(line)
			continue
		}
		if !strings.HasPrefix(h, "69ec") && !strings.HasPrefix(h, "79ec") {
			continue
		}
		fr, err := hex.DecodeString(h)
		if err != nil {
			continue
		}
		// 6LoWPAN fragments: feed the reassembler and print the whole CoAP message
		// once the datagram is complete. The Frame Pending bit does not mark them,
		// so the test is on the plaintext.
		if f, err := tado.ParseData(in.NetworkKey(), fr); err == nil && tado.IsFragment(f.Plaintext) {
			ok++
			if printFragment(ra, f) {
				reasm++
			}
			continue
		}
		if printFrame(in, fr) {
			ok++
		} else {
			fail++
		}
	}
	fmt.Printf("=== %d decrypted, %d MIC-fail, %d reassembled datagrams, %d incomplete ===\n",
		ok, fail, reasm, ra.Incomplete())
	skip.report()
}

// skipCount tallies the capture lines replayHex refuses, so a run over an rx log
// says how much of the file it never looked at.
type skipCount struct {
	crcBad  int
	comment int
}

func (s *skipCount) count(line string) {
	switch {
	case strings.Contains(line, "CRC-BAD"):
		s.crcBad++
	case strings.HasPrefix(strings.TrimLeft(line, " \t"), "#"):
		s.comment++
	}
}

func (s *skipCount) report() {
	if s.crcBad == 0 && s.comment == 0 {
		return
	}
	fmt.Printf("skipped: %d crc-bad, %d comment lines\n", s.crcBad, s.comment)
}

// printFragment files one 6LoWPAN fragment and prints it; returns true when this
// fragment completed a datagram.
func printFragment(ra *tado.Reassembler, f *tado.Frame) bool {
	src, dst := tado.ShortFromWire(f.SrcEUI()), tado.ShortFromWire(f.DstEUI)
	rs, ok := ra.AddFrame(f)
	if !ok {
		fmt.Printf("[FRAG DROP] src=%s dst=%s %s\n", src, dst, rs.Fragment)
		return false
	}
	if rs.Message == nil {
		fmt.Printf("[FRAG] src=%s dst=%s %s held=%d/%d frames=%d\n",
			src, dst, rs.Fragment, rs.Held, rs.Fragment.Size, rs.Frames)
		return false
	}
	fmt.Printf("[REASSEMBLED] src=%s dst=%s frames=%d bytes=%d\n    CoAP %s\n",
		src, dst, rs.Frames, rs.Fragment.Size, rs.Message.Summary())
	// A block-wise transfer past block 0 carries a slice of the document, which
	// cuts a TLV in half; only the raw bytes mean anything there.
	if rs.Message.PayloadStartsDocument() {
		ts := tado.ParseTLVs(rs.Message.Payload)
		res := tado.ResourceOf(rs.Message.URI(), ts)
		var labels []string
		for _, t := range ts {
			labels = append(labels, t.TagLabelIn(res))
		}
		if len(labels) > 0 {
			fmt.Printf("    %s\n", strings.Join(labels, "  "))
		}
	}
	fmt.Printf("    payload=%s\n", hex.EncodeToString(rs.Message.Payload))
	return true
}

func cmdFrame(args []string) {
	fs := flag.NewFlagSet("frame", flag.ExitOnError)
	configFlag(fs)
	stateFlag(fs)
	pos := parseFlags(fs, args)
	if len(pos) < 1 {
		die("frame needs a hex frame")
	}
	fr, err := hex.DecodeString(strings.TrimSpace(pos[0]))
	check(err)
	printFrame(mustInstall(fs), fr)
}

// printFrame decrypts and prints one frame; returns false on MIC failure.
func printFrame(in *tado.Install, fr []byte) bool {
	f, err := tado.ParseData(in.NetworkKey(), fr)
	if err != nil {
		fmt.Printf("[MIC FAIL L=%3d] %s...\n", len(fr), hex.EncodeToString(fr[:min(16, len(fr))]))
		return false
	}
	src := hex.EncodeToString(f.SrcEUI())
	dst := hex.EncodeToString(f.DstEUI)
	fmt.Printf("[OK L=%3d] src=%s dst=%s seq=%02x\n", len(fr), src, dst, f.Seq)
	if app := tado.ParseApp(f.Plaintext); app != nil {
		fmt.Printf("    ctr=%d serial=%s id=%s\n", app.Counter, app.Serial, app.ID)
		slice, uri := false, ""
		if app.CoAP != nil {
			c := app.CoAP
			fmt.Printf("    CoAP %s udp %d->%d\n", c.Summary(), c.SrcPort, c.DstPort)
			slice = !c.PayloadStartsDocument()
			uri = c.URI()
		}
		res := tado.ResourceOf(uri, app.TLVs)
		var labels []string
		for _, t := range app.TLVs {
			labels = append(labels, t.TagLabelIn(res))
		}
		// A block-wise transfer past block 0 cuts a TLV in half: print the bytes.
		if slice {
			fmt.Printf("    payload=%s\n", hex.EncodeToString(app.CoAP.Payload))
		} else if len(labels) > 0 {
			fmt.Printf("    %s\n", strings.Join(labels, "  "))
		}
	} else {
		fmt.Printf("    plaintext=%s\n", hex.EncodeToString(f.Plaintext))
	}
	return true
}

func cmdDissect(args []string) {
	fs := flag.NewFlagSet("dissect", flag.ExitOnError)
	configFlag(fs)
	stateFlag(fs)
	pos := parseFlags(fs, args)
	if len(pos) < 1 {
		die("dissect needs a hex frame or a capture file")
	}
	in := mustInstall(fs)
	arg := strings.TrimSpace(pos[0])
	// A capture holds fragments, so the file path uses the stateful dissector.
	dis := in.NewDissector()
	// If it's a readable file, dissect every frame in it; else treat as one hex frame.
	if f, err := os.Open(arg); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 4096), 1<<16)
		counts := map[string]int{}
		var skip skipCount
		for sc.Scan() {
			line := sc.Text()
			h, take := replayHex(line)
			if !take {
				skip.count(line)
				continue
			}
			fr, err := hex.DecodeString(h)
			if err != nil || len(fr) < 3 {
				continue
			}
			counts[tado.Classify(fr).String()]++
			fmt.Println(dis.Dissect(fr))
		}
		fmt.Printf("=== classes: %v ===\n", counts)
		skip.report()
		return
	}
	fr, err := hex.DecodeString(arg)
	check(err)
	fmt.Println(dis.Dissect(fr))
}

func cmdNodes(args []string) {
	fs := flag.NewFlagSet("nodes", flag.ExitOnError)
	configFlag(fs)
	stateFlag(fs)
	parseFlags(fs, args)
	in := mustInstall(fs)

	fmt.Printf("%-16s %-24s %-6s %-11s %-14s %-14s %s\n",
		"wire(LE)", "EUI-64", "short", "kind", "serial", "label", "zones")
	fmt.Printf("%-16s %-24s %-6s %-11s %-14s %-14s %s\n",
		hex.EncodeToString(in.CoordinatorEUI()), eui64(in.CoordinatorEUI()),
		tado.ShortFromWire(in.CoordinatorEUI()), "coordinator", in.Coordinator.Serial, "(us)", "")
	for i := range in.Devices {
		d := &in.Devices[i]
		fmt.Printf("%-16s %-24s %-6s %-11s %-14s %-14s %s\n",
			hex.EncodeToString(d.WireEUI()), eui64(d.WireEUI()), d.Short(), d.Kind,
			d.Serial, d.Label, zonesOf(in, d))
	}
}

// eui64 renders a wire (little-endian) address as the canonical big-endian
// EUI-64 printed on the device.
func eui64(wire []byte) string {
	if len(wire) != 8 {
		return ""
	}
	parts := make([]string, 8)
	for i, b := range wire {
		parts[7-i] = fmt.Sprintf("%02x", b)
	}
	return strings.Join(parts, ":")
}

func zonesOf(in *tado.Install, d *tado.Device) string {
	var out []string
	for i := range in.Zones {
		z := &in.Zones[i]
		for _, ref := range z.Members {
			if m, err := in.Resolve(ref); err == nil && m == d {
				out = append(out, strconv.Itoa(z.Number))
				break
			}
		}
	}
	return strings.Join(out, ",")
}

// --- radio commands ---

func openBridge(fs *flag.FlagSet) *cc1101.Bridge {
	port := fs.Lookup("p").Value.String()
	br, err := cc1101.Open(port)
	if err != nil {
		die("open %s: %v", port, err)
	}
	return br
}

func portFlag(fs *flag.FlagSet) {
	p := fs.String("p", "/dev/ttyUSB0", "serial port of the ESP32 bridge")
	fs.StringVar(p, "port", "/dev/ttyUSB0", "alias for -p")
}

func cmdPing(args []string) {
	fs := flag.NewFlagSet("ping", flag.ExitOnError)
	portFlag(fs)
	fs.Parse(args)
	br := openBridge(fs)
	defer br.Close()
	banner, err := br.Ping()
	check(err)
	fmt.Printf("bridge: %q\n", banner)
}

func cmdConfig(args []string) {
	fs := flag.NewFlagSet("config", flag.ExitOnError)
	portFlag(fs)
	fs.Parse(args)
	br := openBridge(fs)
	defer br.Close()
	check(br.Configure())
	part, ver, err := br.PartVersion()
	check(err)
	status := "check wiring"
	if ver == 0x04 || ver == 0x14 {
		status = "CC1101 ok"
	}
	fmt.Printf("configured. PARTNUM=%#02x VERSION=%#02x (%s)\n", part, ver, status)
}

func cmdRegs(args []string) {
	fs := flag.NewFlagSet("regs", flag.ExitOnError)
	portFlag(fs)
	fs.Parse(args)
	br := openBridge(fs)
	defer br.Close()
	part, ver, err := br.PartVersion()
	check(err)
	fmt.Printf("PARTNUM=%#02x VERSION=%#02x\n", part, ver)
	for base := 0x00; base < 0x2F; base += 8 {
		fmt.Printf("  %02x:", base)
		for a := base; a < base+8 && a < 0x2F; a++ {
			v, err := br.ReadReg(byte(a))
			check(err)
			fmt.Printf(" %02x", v)
		}
		fmt.Println()
	}
	m, err := br.ReadStatus(cc1101.MARCSTATE)
	check(err)
	fmt.Printf("MARCSTATE=%#02x\n", m&0x1F)
}

func cmdAutotune(args []string) {
	fs := flag.NewFlagSet("autotune", flag.ExitOnError)
	portFlag(fs)
	configFlag(fs)
	stateFlag(fs)
	dry := fs.Bool("dry", false, "measure and print the register, store nothing")
	fs.Parse(args)
	// The database is resolved before the radio opens, so a wrong path fails
	// now instead of after the receive window.
	path := ""
	if !*dry {
		path = mustDatabase(fs)
	}
	br := openBridge(fs)
	defer br.Close()
	check(br.Configure())
	f, ok, err := br.Autotune()
	check(err)
	switch {
	case !ok:
		fmt.Printf("untuned, FSCTRL0=%#02x: nothing stored\n", br.FSCTRL0())
	case *dry:
		fmt.Printf("tuned FSCTRL0=%#02x, nothing stored (-dry)\n", f)
	default:
		storeCalibration(path, f)
		fmt.Printf("tuned FSCTRL0=%#02x stored\n", f)
	}
}

func cmdRx(args []string) {
	fs := flag.NewFlagSet("rx", flag.ExitOnError)
	portFlag(fs)
	configFlag(fs)
	stateFlag(fs)
	tune := fs.Bool("autotune", true, "center FSCTRL0 on the live carrier first")
	fs.Parse(args)
	in := mustInstall(fs)
	br := openBridge(fs)
	defer br.Close()
	check(br.Configure())
	if *tune {
		if f, ok, _ := br.Autotune(); ok {
			fmt.Printf("autotuned FSCTRL0=%#02x\n", f)
		} else {
			fmt.Printf("autotune: untuned, FSCTRL0=%#02x\n", br.FSCTRL0())
		}
	}
	fmt.Println("listening (Ctrl-C to stop)...")
	dis := in.NewDissector()
	errs := 0
	for {
		// Continuous streaming in 15s windows; the radio stays armed inside each
		// window so no frame is missed between calls.
		var freqEst []int
		cb := func(f *cc1101.RxFrame) {
			printRx(dis, f)
			if f.CRCOK {
				freqEst = append(freqEst, f.FreqEst)
			}
		}
		if err := br.Stream(15*time.Second, cb); err != nil {
			errs++
			fmt.Fprintf(os.Stderr, "rx: %v\n", err)
			if errs > 10 {
				die("too many consecutive RX errors")
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		errs = 0
		// No host retune: autotune centers FSCTRL0 once for this module's crystal and
		// the hardware FOC (Tado firmware config) tracks per-transmitter offset, so we
		// never chase it. freqEst stays collected only for the per-frame fe display.
		_ = freqEst
	}
}

func printRx(dis *tado.Dissector, f *cc1101.RxFrame) {
	printRxLine(f, dis.Dissect(f.Payload))
}

// printRxLine prints one received frame with a dissection the caller already
// holds. A Dissector files every fragment it reads, so each frame must reach it
// exactly once.
func printRxLine(f *cc1101.RxFrame, line string) {
	flag := "CRC-OK"
	if !f.CRCOK {
		flag = "CRC-BAD"
	}
	// timestamp + raw hex + dissection: raw hex lets an external tool re-decrypt.
	fmt.Printf("%.3f [%s rssi=%.0f fe=%+d] %s | %s\n",
		float64(time.Now().UnixMilli())/1000, flag, f.RSSIdBm, f.FreqEst,
		hex.EncodeToString(f.Payload), line)
}

func cmdTx(args []string) {
	fs := flag.NewFlagSet("tx", flag.ExitOnError)
	portFlag(fs)
	configFlag(fs)
	stateFlag(fs)
	pos := parseFlags(fs, args)
	if len(pos) < 1 {
		die("tx needs a hex MAC frame (no length byte)")
	}
	payload, err := hex.DecodeString(pos[0])
	check(err)
	fmt.Printf("sending  %s\n", mustInstall(fs).Dissect(payload))
	br := openBridge(fs)
	defer br.Close()
	check(br.Configure())
	ok, err := br.Send(payload, time.Second)
	check(err)
	fmt.Printf("sent %d B, %s\n", len(payload), map[bool]string{true: "ok", false: "TX did not return to IDLE"}[ok])
}

func cmdSetpoint(args []string) {
	fs := flag.NewFlagSet("setpoint", flag.ExitOnError)
	portFlag(fs)
	configFlag(fs)
	stateFlag(fs)
	ref := fs.String("ref", "", "a real captured downlink to the SAME valve; reuse its meta and counter+1")
	metaHex := fs.String("meta", "", "raw meta block (hex) if not using --ref")
	ctr := fs.Int("ctr", -1, "frame counter; default = ref.counter+1")
	seq := fs.Int("seq", 0x10, "MAC sequence number")
	overlay := fs.Int("overlay", -1, "overlay: 0=auto, 2=manual, 3=until_next; default = inherit --ref or 2")
	doTx := fs.Bool("tx", false, "transmit the built frame via the bridge")
	pos := parseFlags(fs, args)
	if len(pos) < 2 {
		die("setpoint needs <valve> <tempC>")
	}
	in := mustInstall(fs)
	node, err := in.Resolve(pos[0])
	if err != nil {
		die("%v (try: freedo-dev nodes)", err)
	}
	temp := parseFloat(pos[1])
	dst := node.WireEUI()

	var meta []byte
	counter := uint32(*ctr)
	overlayVal := 2 // default: manual
	switch {
	case *ref != "":
		refFr, err := hex.DecodeString(strings.TrimSpace(*ref))
		check(err)
		rp, err := tado.ParseData(in.NetworkKey(), refFr)
		if err != nil {
			die("--ref does not decrypt: %v", err)
		}
		if !bytes.Equal(rp.DstEUI, dst) {
			die("--ref dst %s is not valve %s", hex.EncodeToString(rp.DstEUI), hex.EncodeToString(dst))
		}
		rapp := tado.ParseApp(rp.Plaintext)
		if rapp == nil {
			die("--ref plaintext did not parse (cannot extract meta)")
		}
		meta = rapp.Meta
		if *ctr < 0 {
			counter = rapp.Counter + 1
		}
		// Inherit the overlay from the real command: change only the setpoint.
		if t, ok := rapp.TLV("6240"); ok && len(t.Val) == 1 {
			overlayVal = int(t.Val[0])
		}
	case *metaHex != "":
		var err error
		meta, err = hex.DecodeString(*metaHex)
		check(err)
		if *ctr < 0 {
			die("--ctr is required with --meta")
		}
	default:
		die("provide --ref <captured downlink> (recommended) or --meta <hex> + --ctr")
	}

	if *overlay >= 0 {
		overlayVal = *overlay
	}
	f, pt, err := tado.BuildSetpoint(in.NetworkKey(), tado.SetpointCmd{
		DstEUI:    dst,
		SrcPrefix: in.CoordinatorEUI()[:5],
		Seq:       byte(*seq),
		Counter:   counter,
		Setpoint:  temp,
		Meta:      meta,
		Overlay:   byte(overlayVal),
	})
	check(err)

	fmt.Printf("valve   %s (%s, %s)\n", node.Serial, node.Short(), node.Label)
	fmt.Printf("counter %d  setpoint %.2fC  overlay %d\n", counter, temp, overlayVal)
	fmt.Printf("plaintext %s\n", hex.EncodeToString(pt))
	fmt.Printf("frame     %s\n", f.Hex())

	if !*doTx {
		fmt.Println("(dry run; add --tx to transmit)")
		return
	}
	br := openBridge(fs)
	defer br.Close()
	check(br.Configure())
	ok, err := br.Send(f.Bytes(), time.Second)
	check(err)
	fmt.Printf("transmitted, %s\n", map[bool]string{true: "ok", false: "TX did not return to IDLE"}[ok])
}

// --- helpers ---

func parseFloat(s string) float64 {
	var v float64
	if _, err := fmt.Sscanf(s, "%g", &v); err != nil {
		die("bad number %q", s)
	}
	return v
}
