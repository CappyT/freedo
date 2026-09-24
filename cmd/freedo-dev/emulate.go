package main

// emulate: run the bridge emulator (tado.EmuState) either against the live radio
// or offline against a capture file.

import (
	"bufio"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"freedo/cc1101"
	"freedo/tado"
)

// setFlag collects the repeatable --set values.
type setFlag []string

func (s *setFlag) String() string     { return strings.Join(*s, ",") }
func (s *setFlag) Set(v string) error { *s = append(*s, v); return nil }

func cmdEmulate(args []string) {
	fs := flag.NewFlagSet("emulate", flag.ExitOnError)
	portFlag(fs)
	configFlag(fs)
	stateFlag(fs)
	replay := fs.String("replay", "", "offline: replay this capture file through the emulator, no radio")
	analyze := fs.Bool("analyze", false, "offline: with --replay, print a capture-analysis report (decode ALL frames both directions, do not run the state machine)")
	logfile := fs.String("logfile", "", "append a per-frame decode log to this file (default: off)")
	unhandledPath := fs.String("unhandled", "-", `unhandled-frame log destination ("-" = stderr)`)
	beacon := fs.Duration("beacon", 0, "interval of the 0x49e8 router advertisement (0 = off)")
	ack := fs.Bool("ack", true, "answer uplinks with a MAC Enhanced ACK and a CoAP ACK")
	doTx := fs.Bool("tx", false, "transmit the decided frames after the capture window, host-driven and with no wake train: too late for any ACK window (default: dry-run, transmit nothing)")
	duration := fs.Duration("duration", 0, "stop the live daemon after this long (0 = until Ctrl-C)")
	txctr := fs.Uint("txctr", 0, "initial frame counter for downlinks (overrides tx_counter)")
	cslphase := fs.Uint("cslphase", 0, "CSL phase in our Enhanced ACKs: 320 us units elapsed since our own last CSL sample; 0 is safe while our receiver listens continuously (overrides csl_phase)")
	id := fs.Int("id", -1, "CoAP device id (the z/s?id=N logical id) used by --set")
	sched := fs.Float64("sched", -1, "scheduled setpoint C for --set (default: the --set temperature)")
	overlay := fs.Int("overlay", 2, "overlay for --set: 0=auto, 2=manual, 3=until_next")
	var sets setFlag
	fs.Var(&sets, "set", "queue a setpoint downlink: <node>=<tempC>[@<id>] (repeatable)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `freedo-dev emulate - run the local tado coordinator emulator

Decodes every frame, decides the answers (Enhanced ACK, CoAP ACK, queued
downlinks) and logs whatever it cannot fully handle. The real bridge must be
powered OFF: the emulator assumes it is the only PAN coordinator on air.

The default is a dry-run. Nothing is transmitted unless --tx is given.
--tx sends each action from the host once the stream stops, 10-25 ms after
the frame that decided it, as a plain frame with no CSL wake train. That misses
every ACK window and every sleeping device: use the daemon for real traffic.
--replay runs the same state machine offline, with no hardware at all.
--replay --analyze skips the state machine and reports the capture instead,
decoding both directions (the bridge-sourced downlink commands included).
Ctrl-C stops the live daemon at the end of the current 15s capture window.

flags:
`)
		fs.PrintDefaults()
	}
	if pos := parseFlags(fs, args); len(pos) > 0 {
		die("emulate takes no positional arguments (got %q)", pos[0])
	}
	b := mustBootstrap(fs)
	in := mustInstall(fs)
	given := setFlags(fs)

	var uw io.Writer = os.Stderr
	if p := *unhandledPath; p != "" && p != "-" {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		check(err)
		defer f.Close()
		uw = f
	}
	ulog := tado.NewUnhandledLogger(uw)

	// Analysis is a pure offline view: no emulator, so no downlink is queued.
	if *analyze {
		if *replay == "" {
			die("--analyze requires --replay")
		}
		emulateAnalyze(in, *replay, ulog)
		return
	}
	if *beacon > 0 && *doTx {
		die("emulate --beacon --tx is unsupported; use daemon")
	}

	var flog *bufio.Writer
	if *logfile != "" {
		f, err := os.OpenFile(*logfile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		check(err)
		defer f.Close()
		flog = bufio.NewWriter(f)
		defer flog.Flush()
	}

	cfg := in.EmuConfig()
	cfg.TxCounter, cfg.CSLPhase = b.TxCounterSeed(), b.CSLPhase
	cfg.ZoneGetProxy = b.ZoneGetProxy
	cfg.ZoneProxyTimeout = b.ZoneProxyTimeout()
	if given["txctr"] {
		cfg.TxCounter = uint32(*txctr)
	}
	if given["cslphase"] {
		cfg.CSLPhase = uint16(*cslphase)
	}
	cfg.RouterAdvertInterval = *beacon
	cfg.NoEnhAck, cfg.NoCoAPAck = !*ack, !*ack
	emu := tado.NewEmu(cfg, ulog)

	for _, s := range sets {
		node, temp, sid := parseSetSpec(s, *id)
		if sid < 0 {
			die("--set %s needs a CoAP device id: pass --id <N>, or write --set <node>=<C>@<id>", s)
		}
		schedC := temp
		if *sched >= 0 {
			schedC = *sched
		}
		if err := emu.EnqueueSetpoint(node, tado.SetpointParams{
			ID:       sid,
			Setpoint: temp,
			Sched:    schedC,
			Overlay:  byte(*overlay),
		}); err != nil {
			die("--set %s: %v", s, err)
		}
		fmt.Printf("queued setpoint %s = %.2fC (sched %.2fC, overlay %d, id %d)\n", node, temp, schedC, *overlay, sid)
	}

	if *replay != "" {
		emulateReplay(in, *replay, emu, ulog, flog, *doTx)
		return
	}
	emulateLive(fs, in, emu, ulog, flog, *doTx, *beacon, *duration)
}

// parseSetSpec splits "<node>=<tempC>" or "<node>=<tempC>@<id>".
func parseSetSpec(s string, defID int) (node string, temp float64, id int) {
	id = defID
	if i := strings.LastIndex(s, "@"); i >= 0 {
		if _, err := fmt.Sscanf(s[i+1:], "%d", &id); err != nil {
			die("bad id in --set %q", s)
		}
		s = s[:i]
	}
	i := strings.Index(s, "=")
	if i < 0 {
		die("--set wants <node>=<tempC>, got %q", s)
	}
	return s[:i], parseFloat(s[i+1:]), id
}

// --- offline replay ---

func emulateReplay(in *tado.Install, path string, emu *tado.EmuState, ulog *tado.UnhandledLogger, flog *bufio.Writer, doTx bool) {
	f, err := os.Open(path)
	check(err)
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), 1<<16)
	dis := in.NewDissector()
	n := 0
	for sc.Scan() {
		h, ok := replayHex(sc.Text())
		if !ok {
			continue
		}
		fr, err := hex.DecodeString(h)
		if err != nil || len(fr) < 2 {
			continue
		}
		n++
		d := dis.Dissect(fr)
		logDecode(flog, fr, d)
		fmt.Printf("[%04d] %s\n", n, d)
		for _, a := range emu.OnFrame(fr) {
			fmt.Printf("       -> [%s] node=%s reason=%s tx=%s\n", a.Kind, a.Node, a.Reason, txField(a, doTx))
		}
	}
	check(sc.Err())
	if flog != nil {
		flog.Flush()
	}
	fmt.Printf("=== replayed %d frames from %s ===\n", n, path)
	printEmuReport(emu, ulog)
}

// emulateAnalyze decodes a capture in both directions and prints the report. The
// emulator is not involved: nothing is answered, nothing is transmitted.
func emulateAnalyze(in *tado.Install, path string, ulog *tado.UnhandledLogger) {
	f, err := os.Open(path)
	check(err)
	defer f.Close()
	an := tado.NewAnalyzerFor(in, ulog)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), 1<<16)
	n := 0
	for sc.Scan() {
		h, ok := replayHex(sc.Text())
		if !ok {
			continue
		}
		fr, err := hex.DecodeString(h)
		if err != nil || len(fr) < 2 {
			continue
		}
		n++
		an.Add(fr)
	}
	check(sc.Err())
	fmt.Printf("=== analyzed %d frames from %s ===\n", n, path)
	fmt.Print(an.Report())
}

// --- live daemon ---

func emulateLive(fs *flag.FlagSet, in *tado.Install, emu *tado.EmuState, ulog *tado.UnhandledLogger, flog *bufio.Writer, doTx bool, beacon, duration time.Duration) {
	br := openBridge(fs)
	defer br.Close()
	check(br.Configure())
	if f, ok, _ := br.Autotune(); ok {
		fmt.Printf("autotuned FSCTRL0=%#02x\n", f)
	} else {
		fmt.Printf("autotune: untuned, FSCTRL0=%#02x\n", br.FSCTRL0())
	}
	mode := "dry-run, transmitting nothing"
	if doTx {
		mode = "TX ARMED"
	}
	fmt.Printf("emulating the bridge (%s); the real bridge must be off. Ctrl-C to stop.\n", mode)

	stop := make(chan os.Signal, 1)
	// SIGTERM must end the loop as well, so the deferred bridge close runs.
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	var deadline, beaconAt time.Time
	if duration > 0 {
		deadline = time.Now().Add(duration)
	}
	if beacon > 0 {
		beaconAt = time.Now().Add(beacon)
	}

	dis := in.NewDissector()
	errs := 0
loop:
	for {
		select {
		case <-stop:
			fmt.Println("interrupted")
			break loop
		default:
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			break
		}
		// The beacon fires between capture windows, so its period is quantized to
		// the 15s window.
		if beacon > 0 && !time.Now().Before(beaconAt) {
			beaconAt = time.Now().Add(beacon)
			if actions, due, err := emu.MaybeBeacon(); err != nil {
				fmt.Fprintf(os.Stderr, "beacon: %v\n", err)
			} else if due {
				for _, a := range actions {
					emitLive(br, a, doTx)
				}
				completeDryRunBeacon(emu, doTx)
			}
		}
		window := 15 * time.Second
		if !deadline.IsZero() {
			if d := time.Until(deadline); d < window {
				window = d
			}
		}
		if window <= 0 {
			break
		}
		if err := emulateWindow(br, window, emu, dis, flog, doTx); err != nil {
			errs++
			fmt.Fprintf(os.Stderr, "emulate: %v\n", err)
			if errs > 10 {
				die("too many consecutive RX errors")
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		errs = 0
		if flog != nil {
			flog.Flush()
		}
		fmt.Printf("--- %s nodes=%d unhandled=%d\n", statsLine(emu.Stats()), len(emu.Nodes()), ulog.Total())
	}
	printEmuReport(emu, ulog)
}

// liveLink is the bridge the live emulator streams on and transmits through.
type liveLink interface {
	StreamUntil(d time.Duration, cb func(*cc1101.RxFrame) bool) error
	Send(payload []byte, timeout time.Duration) (bool, error)
}

// emulateWindow runs one capture window: the same 15s streaming window as rx,
// so the radio stays armed inside it and no frame is lost between calls. A
// CRC-bad frame is garbage, never decoded. An armed TX never sends inside the
// stream: the first byte of a request would end the firmware stream and read
// its STREAM_END as the response. The frame that decides an action ends the
// window, and the actions go out once the port is back at the command loop.
func emulateWindow(br liveLink, window time.Duration, emu *tado.EmuState, dis *tado.Dissector, flog *bufio.Writer, doTx bool) error {
	var owed []tado.Action
	err := br.StreamUntil(window, func(f *cc1101.RxFrame) bool {
		line := dis.Dissect(f.Payload)
		printRxLine(f, line)
		if !f.CRCOK {
			return false
		}
		logDecode(flog, f.Payload, line)
		for _, a := range emu.OnFrame(f.Payload) {
			if doTx {
				owed = append(owed, a)
			} else {
				emitLive(br, a, false)
			}
		}
		completeDryRunBeacon(emu, doTx)
		return len(owed) > 0
	})
	for _, a := range owed {
		emitLive(br, a, doTx)
	}
	return err
}

func completeDryRunBeacon(emu *tado.EmuState, doTx bool) {
	if !doTx {
		emu.CompleteBeacon(false)
	}
}

// emitLive transmits one decided action, or prints it in dry-run. It runs only
// outside a firmware stream; see emulateWindow. The host-driven Send has no
// wake train and comes too late for an ACK window: it is a bench tool, and the
// daemon sends real traffic through the firmware CSL queue.
func emitLive(br liveLink, a tado.Action, doTx bool) {
	if !doTx {
		fmt.Printf("       (dry-run) would send [%s] node=%s reason=%s tx=%s\n",
			a.Kind, a.Node, a.Reason, hex.EncodeToString(a.Bytes))
		return
	}
	ok, err := br.Send(a.Bytes, time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tx [%s] %s: %v\n", a.Kind, a.Reason, err)
		return
	}
	fmt.Printf("       -> sent [%s] node=%s reason=%s %s\n", a.Kind, a.Node, a.Reason,
		map[bool]string{true: "ok", false: "TX did not return to IDLE"}[ok])
}

// --- reporting ---

func txField(a tado.Action, doTx bool) string {
	if !doTx {
		return "(dry-run)"
	}
	return hex.EncodeToString(a.Bytes)
}

func logDecode(w *bufio.Writer, frame []byte, dissect string) {
	if w == nil {
		return
	}
	fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", time.Now().Format(time.RFC3339),
		tado.Classify(frame), hex.EncodeToString(frame), dissect)
}

func statsLine(s tado.EmuStats) string {
	return fmt.Sprintf("frames=%d data=%d enh-acks=%d wakeups=%d bcast=%d unknown=%d mic-fail=%d frag=%d reasm=%d coap-acks=%d dl-sent=%d dl-ok=%d dl-drop=%d",
		s.Frames, s.Data, s.EnhAcks, s.WakeUps, s.Broadcasts, s.Unknown, s.MICFails,
		s.Fragments, s.Reassembled, s.CoAPAcksSent, s.DownlinksSent, s.DownlinksDelivered, s.DownlinksDropped)
}

func printEmuReport(emu *tado.EmuState, ulog *tado.UnhandledLogger) {
	fmt.Printf("stats: %s\n", statsLine(emu.Stats()))
	nodes := emu.Nodes()
	fmt.Printf("nodes: %d\n", len(nodes))
	if len(nodes) > 0 {
		fmt.Printf("  %-16s %-6s %-14s %-8s %-6s %-7s %s\n",
			"wire(LE)", "short", "label", "rxctr", "phase", "period", "telemetry")
		for _, n := range nodes {
			fmt.Printf("  %-16s %-6s %-14s %-8d %-6d %-7d %s\n",
				n.WireLE, n.Short, n.Label, n.RxCounter, n.CSLPhase, n.CSLPeriod, telemetryLine(n.Telemetry))
		}
	}
	fmt.Printf("unhandled: %d events\n", ulog.Total())
	for _, line := range strings.Split(strings.TrimRight(ulog.Summary(), "\n"), "\n") {
		if line != "" {
			fmt.Printf("  %s\n", line)
		}
	}
}

func telemetryLine(t map[string]string) string {
	keys := make([]string, 0, len(t))
	for k := range t {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+t[k])
	}
	return strings.Join(parts, " ")
}
