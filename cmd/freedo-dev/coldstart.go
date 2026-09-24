package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"freedo/cc1101"
	"freedo/internal/radio"
	"freedo/tado"
)

const coldStartPreflightMin = 75 * time.Second

func cmdColdStart(args []string) {
	fs := flag.NewFlagSet("coldstart", flag.ExitOnError)
	portFlag(fs)
	configFlag(fs)
	stateFlag(fs)
	tempC := fs.Float64("temp", 5.0, "setpoint C to deliver; below ambient by default, so a delivery does not call for heat")
	id := fs.Int("id", -1, "CoAP device id of the target (its zone number), as in z/s?id=N")
	overlay := fs.Int("overlay", 2, "overlay: 0=auto, 2=manual, 3=until_next")
	attempts := fs.Int("attempts", 1, "number of explicit CSL attempts; only 1 is currently supported")
	preflight := fs.Duration("preflight", coldStartPreflightMin, "passive bridge-detection window before planning or transmitting")
	quiet := fs.Duration("quiet", 30*time.Second, "require the target to have been silent this long after preflight (0 = do not wait)")
	listen := fs.Duration("listen", 5*time.Second, "how long to await the on-chip CSL completion")
	doTx := fs.Bool("tx", false, "actually transmit; without it the frames are only printed (dry-run)")
	unhandledPath := fs.String("unhandled", "-", `unhandled-frame log destination ("-" = stderr)`)
	txctr := fs.Uint("txctr", 0, "initial downlink frame counter (overrides tx_counter)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `freedo-dev coldstart - deliver a command to a sleeping device, and measure how often it lands

Passively listens for 75 seconds before it plans a setpoint. With --tx it then
submits one bootstrap CSL transaction to the on-chip scheduler and waits for its
completion. The scheduler owns the wake-up train, ACK and response window.

This is phase C test H14. The real bridge must be powered OFF: like the daemon,
this refuses to transmit once it hears a bridge-sourced frame.

usage:
  freedo-dev coldstart <device> --id <N> [--tx]

flags:
`)
		fs.PrintDefaults()
	}
	pos := parseFlags(fs, args)
	if len(pos) != 1 {
		die("coldstart wants exactly one device (serial, short address, wire EUI or room)")
	}
	target := pos[0]
	if *id < 0 {
		die("coldstart needs the target's CoAP device id: --id <N>")
	}
	if *attempts != 1 {
		die("--attempts must be 1: the CSL bootstrap retry cooldown requires a new run")
	}
	if err := validateColdStartPreflight(*preflight); err != nil {
		die("%v", err)
	}
	b := mustBootstrap(fs)
	in := mustInstall(fs)
	given := setFlags(fs)

	uw, closeLog := openUnhandled(*unhandledPath)
	defer closeLog()
	ulog := tado.NewUnhandledLogger(uw)

	cfg := in.EmuConfig()
	cfg.TxCounter, cfg.CSLPhase = b.TxCounterSeed(), b.CSLPhase
	cfg.NoAutoQueue = true
	if given["txctr"] {
		cfg.TxCounter = uint32(*txctr)
	}
	emu := tado.NewEmu(cfg, ulog)

	br := openBridge(fs)
	check(br.Configure())

	mode := "DRY-RUN (nothing goes on air)"
	if *doTx {
		mode = "TX ARMED"
	}
	fmt.Printf("coldstart %s id=%d %.2fC: %d attempts, %s\n", target, *id, *tempC, *attempts, mode)
	fmt.Println("the real bridge must be off; this refuses to transmit while it hears one")

	stop := make(chan os.Signal, 1)
	// SIGTERM must end the attempt loop as well, so the deferred radio close runs.
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	r := &coldStartRun{emu: emu, passive: br, in: in, doTx: *doTx, target: target, listen: *listen, quiet: *quiet}
	if err := r.preflight(*preflight); err != nil {
		br.Close()
		die("passive preflight: %v", err)
	}
	if emu.BridgeSeen() {
		if *doTx {
			br.Close()
			die("bridge present on-air, refusing to arm CSL transmission")
		}
		fmt.Println("bridge detected during preflight; dry-run remains offline")
	}
	if err := r.waitQuiet(); err != nil {
		br.Close()
		die("target silence: %v", err)
	}
	if emu.BridgeSeen() {
		if *doTx {
			br.Close()
			die("bridge present on-air, refusing to arm CSL transmission")
		}
		fmt.Println("bridge detected while waiting for target silence; dry-run remains offline")
	}
	r.prepared = true
	if *doTx {
		live, err := radio.StartLive(br, in.CoordinatorEUI(), cfg.CSLPhase, false)
		check(err)
		defer live.Close()
		r.tx, r.completions = live, live.CSLCompletions()
		r.frames = live.Frames()
	} else {
		defer br.Close()
	}
	ok, done := 0, 0
	for att := 1; att <= *attempts; att++ {
		select {
		case <-stop:
			fmt.Println("interrupted")
			att = *attempts + 1
			continue
		default:
		}
		if err := emu.EnqueueSetpoint(target, tado.SetpointParams{
			ID: *id, Setpoint: *tempC, Sched: *tempC,
			Overlay: byte(*overlay),
		}); err != nil {
			die("queue setpoint for %s: %v", target, err)
		}
		done++
		delivered, err := r.attempt(att)
		if err != nil {
			fmt.Printf("[%02d/%d] error: %v\n", att, *attempts, err)
			continue
		}
		if delivered {
			ok++
		}
		fmt.Printf("[%02d/%d] %s  (running %d/%d = %.0f%%)\n", att, *attempts,
			map[bool]string{true: "DELIVERED", false: "no acknowledgement"}[delivered],
			ok, done, 100*float64(ok)/float64(done))
	}

	fmt.Printf("\n=== cold-start delivery: %d/%d = %.1f%% ===\n", ok, done, 100*float64(ok)/float64(done))
	printEmuReport(emu, ulog)
}

func validateColdStartPreflight(window time.Duration) error {
	if window < coldStartPreflightMin {
		return fmt.Errorf("--preflight must be at least %s", coldStartPreflightMin)
	}
	return nil
}

type passiveRadio interface {
	Stream(d time.Duration, cb func(*cc1101.RxFrame)) error
}

type immediateBatchRadio interface {
	SendImmediate(actions []tado.Action, autoAck bool) (radio.ImmediateResult, error)
}

// coldStartRun carries the per-run state of one measurement campaign.
type coldStartRun struct {
	emu         *tado.EmuState
	passive     passiveRadio
	tx          immediateBatchRadio
	completions <-chan radio.CSLCompletion
	frames      <-chan []byte
	in          *tado.Install
	doTx        bool
	target      string
	listen      time.Duration
	quiet       time.Duration
	prepared    bool
}

func (r *coldStartRun) attempt(n int) (bool, error) {
	if !r.prepared {
		if err := r.waitQuiet(); err != nil {
			return false, err
		}
		r.prepared = true
	}
	r.drainFrames()
	if r.emu.BridgeSeen() {
		return false, fmt.Errorf("bridge present on-air, refusing to transmit (sole-coordinator assumption)")
	}
	before := r.emu.Stats().DownlinksDelivered
	actions, ok, err := r.emu.NextQueuedDownlink(r.target)
	if err != nil {
		return false, err
	}
	if !ok || len(actions) != 1 || !actions[0].CSLTx || !actions[0].QueueAttempt {
		return false, fmt.Errorf("cold-start needs one queued CSL Data action")
	}
	defer r.flush()
	action := actions[0]
	fmt.Printf("[%02d] bootstrap CSL Data frame\n", n)
	if !r.doTx {
		fmt.Printf("     (dry-run) [CSLQueue bootstrap=true follow=%t] %s\n", action.WaitResponse, hex.EncodeToString(action.Bytes))
		return false, nil
	}
	if r.tx == nil || r.completions == nil {
		return false, fmt.Errorf("CSL scheduler is unavailable")
	}
	if r.emu.BridgeSeen() {
		return false, fmt.Errorf("bridge present on-air, refusing to transmit (sole-coordinator assumption)")
	}
	result, err := r.tx.SendImmediate(actions, true)
	if err != nil {
		return false, fmt.Errorf("submit CSL queue: %w", err)
	}
	if !result.CSLQueue || len(result.Accepted) != 1 {
		return false, fmt.Errorf("CSL scheduler did not accept the queued action")
	}
	r.emu.MarkQueuedDownlinkAwaitingCSL(action.Bytes[3:11], action.Bytes[2])
	for _, ack := range result.Acks {
		r.emu.OnFrame(ack)
	}
	if r.emu.Stats().DownlinksDelivered > before {
		return true, nil
	}
	if err := r.awaitCompletion(action, before); err != nil {
		return false, err
	}
	delivered := r.emu.Stats().DownlinksDelivered > before
	return delivered, nil
}

func (r *coldStartRun) drainFrames() {
	for r.frames != nil {
		select {
		case frame, ok := <-r.frames:
			if !ok {
				r.frames = nil
				return
			}
			r.emu.OnFrame(frame)
		default:
			return
		}
	}
}

func (r *coldStartRun) awaitCompletion(action tado.Action, before int) error {
	deadline := time.NewTimer(r.listen)
	defer deadline.Stop()
	for {
		select {
		case completion, ok := <-r.completions:
			if !ok {
				return fmt.Errorf("CSL scheduler stopped before completion")
			}
			if !completion.Action.QueueAttempt || len(completion.Action.Bytes) < 11 || completion.Action.Bytes[2] != action.Bytes[2] || string(completion.Action.Bytes[3:11]) != string(action.Bytes[3:11]) {
				continue
			}
			r.emu.CompleteQueuedDownlinkAttempt(action.Bytes[3:11], action.Bytes[2])
			for _, frame := range completion.Frames {
				r.emu.OnFrame(frame)
			}
			if completion.Err != nil {
				return fmt.Errorf("CSL completion: %w", completion.Err)
			}
			if completion.Result == nil || completion.Result.Sent == 0 {
				return fmt.Errorf("CSL completion has no transmitted frame")
			}
			fmt.Printf("     scheduler sent=%d ack=%t follow=%d\n", completion.Result.Sent, completion.Result.Ack != nil, len(completion.Result.Follow))
			return nil
		case <-deadline.C:
			return fmt.Errorf("CSL scheduler did not complete within %s", r.listen)
		}
	}
}

func (r *coldStartRun) waitQuiet() error {
	if r.quiet <= 0 {
		return nil
	}
	deadline := time.Now().Add(2 * r.quiet) // give up rather than block forever
	for {
		if last, ok := r.lastSeen(); !ok || time.Since(last) >= r.quiet {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%s never went quiet for %s", r.target, r.quiet)
		}
		if err := r.passive.Stream(time.Second, func(f *cc1101.RxFrame) {
			if f.CRCOK {
				r.emu.OnFrame(f.Payload)
			}
		}); err != nil {
			return err
		}
	}
}

func (r *coldStartRun) preflight(window time.Duration) error {
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		wait := time.Until(deadline)
		if wait > time.Second {
			wait = time.Second
		}
		if err := r.passive.Stream(wait, func(f *cc1101.RxFrame) {
			if f.CRCOK {
				r.emu.OnFrame(f.Payload)
			}
		}); err != nil {
			return err
		}
	}
	return nil
}

// lastSeen is when the target last spoke, and whether it has spoken at all.
func (r *coldStartRun) lastSeen() (time.Time, bool) {
	want, err := r.in.Resolve(r.target)
	if err != nil {
		return time.Time{}, false
	}
	wire := hex.EncodeToString(want.WireEUI())
	for _, ns := range r.emu.Nodes() {
		if ns.WireLE == wire && !ns.LastSeen.IsZero() {
			return ns.LastSeen, true
		}
	}
	return time.Time{}, false
}

// flush drops whatever is still queued for the target, so an undelivered attempt
// does not linger and land as a piggyback during the next one.
func (r *coldStartRun) flush() {
	r.emu.DropQueue(r.target)
}

// openUnhandled resolves the --unhandled destination to a writer and its closer.
func openUnhandled(path string) (*os.File, func()) {
	if path == "" || path == "-" {
		return os.Stderr, func() {}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	check(err)
	return f, func() { f.Close() }
}
