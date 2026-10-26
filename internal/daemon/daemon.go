package daemon

// daemon: the bridge emulator as a long-running service with an HTTP get/set
// API. One engine goroutine owns the tado.EmuState (which is not concurrent
// safe); every HTTP handler reaches it through do().

import (
	"bytes"
	"cmp"
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"maps"
	"math"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"freedo/cc1101"
	"freedo/internal/buildinfo"
	"freedo/internal/install"
	"freedo/internal/radio"
	"freedo/tado"
)

var (
	errStopped    = radio.ErrStopped
	errNoNode     = errors.New("unknown node")
	errNoInstall  = errors.New("no installation config loaded")
	errTXBlocked  = radio.ErrTXBlocked
	errEngineBusy = errors.New("engine did not answer within the deadline")
	// errNoDHW marks a hot-water request this installation cannot serve.
	errNoDHW = errors.New("no hot-water target")
	// errNoBoiler marks a request for the thermostat wired to the boiler that
	// this installation cannot serve.
	errNoBoiler = errors.New("no boiler thermostat")
)

type immediateBatchRadio interface {
	SendImmediate(actions []tado.Action, autoAck bool) (radio.ImmediateResult, error)
}

type asyncBatchRadio interface {
	SendBatch(frames [][]byte) error
}

type cslCompletionRadio interface {
	CSLCompletions() <-chan radio.CSLCompletion
}

type pairWindowRadio interface {
	SetPairWindow(cc1101.PairWindow) (int, error)
}

type liveRadioStatus interface {
	TXArmed() bool
	RadioError() error
}

// liveRadioRegister reports the frequency-offset register the radio took at
// start. Only a live radio has one, so health omits the field in the other modes.
type liveRadioRegister interface {
	FSCTRL0() byte
}

// liveSerialErrors counts the serial replies the link to the bridge lost. Only a
// live radio has a serial link, so health omits the field in the other modes.
type liveSerialErrors interface {
	SerialErrors() uint64
}

type daemon struct {
	emu *tado.EmuState
	// inst is read on the HTTP goroutines and replaced on the engine one, so it
	// is swapped as a whole validated installation, never edited in place.
	inst             atomic.Pointer[tado.Install]
	radio            radio.Radio
	state            *stateStore
	saveSeq          uint64
	unsaved          bool      // an engine change owed to the database, see saveLearned
	savedAt          time.Time // the last save the engine made itself
	dryRun           bool
	mode             string
	cmds             chan func()
	dispatch         chan struct{}
	done             chan struct{}
	radioLost        chan error    // the error of a live radio loop that ended on its own
	restart          chan struct{} // an import is staged: stop and start again
	beaconEvery      time.Duration
	retryAfter       func(time.Duration) <-chan time.Time
	retryNow         func() time.Time
	retryRand        func() uint16
	retryDue         map[string]uint64
	retryAt          map[string]time.Time
	retryMID         map[string]uint16
	coapRetryBase    map[string]coapRetryState
	dispatchDue      map[string]bool
	dispatchQ        []string
	cslAdmissionWait map[string]cslAdmissionWait
	linkMaint        map[string]linkMaintEntry
	linkMaintOrder   []string
	linkMaintCursor  int
	linkMaintTick    uint16
	linkMaintWait    uint16
	// txResume and midResume are the frame counter and the Message ID the
	// database resumes from, see leaseCounters. They belong to the engine
	// goroutine.
	txResume  uint32
	midResume uint16
	// away is the state of the whole home, home by default. zoneSched holds the
	// block each zone follows now, and schedPushed the block each zone took at
	// the last tick or zone rewrite: a program zone whose two blocks differ owes
	// its members a write of 6200 and 61e0. schedSeeded says whether
	// schedPushed holds a baseline. awayTarget holds the 6200 each zone keeps
	// in away now, on the valve scale, absent for a zone that is off in away,
	// and awayPushed the one each zone took at the last tick or zone rewrite:
	// in away a zone whose two values differ owes its members a write, whatever
	// its block does. awaySeeded says whether awayPushed holds a baseline.
	// programOwed holds the zone rewrites queued and not yet confirmed, see
	// programWrite. schedUnsaved says whether schedPushed, awayPushed,
	// programOwed or early moved since the last save. All nine belong to the
	// engine goroutine.
	away         bool
	zoneSched    map[int]scheduledBlock
	schedPushed  map[int]scheduledBlock
	schedSeeded  bool
	awayTarget   map[int]float64
	awayPushed   map[int]float64
	awaySeeded   bool
	programOwed  map[programWrite]uint64
	schedUnsaved bool
	// presets holds every program preset keyed by id, presetOrder the ids in
	// creation order, activePreset the id the engine follows and serves, and
	// nextPresetID the id of the next storeless create. All four belong to the
	// engine goroutine; see presets.go.
	presets      map[int64]*presetRecord
	presetOrder  []int64
	nextPresetID int64
	activePreset int64
	// progLoc is the wall clock of the programs of progLocOf, see programClock.
	// Both belong to the engine goroutine.
	progLoc   *time.Location
	progLocOf *tado.Install
	// windows holds the open window state of each heating zone and windowOwed
	// the valve writes still owed. Both belong to the engine goroutine; see
	// window.go.
	windows    map[int]zoneWindow
	windowOwed map[windowWrite]*windowOwed
	// roomTemps holds the external room temperature of each zone that has one
	// and roomUnsaved says whether the applied shifts moved since the last save.
	// Both belong to the engine goroutine; see room_temp.go.
	roomTemps   map[int]*roomTemp
	roomUnsaved bool
	// autoOffsets holds the automatic temperature-correction calibration of each
	// zone that states it and autoUnsaved says whether it moved since the last
	// save. Both belong to the engine goroutine; see auto_offset.go.
	autoOffsets map[int]*autoOffset
	autoUnsaved bool
	// zoneOwners holds the controller of each zone the coordinator owns. It
	// belongs to the engine goroutine; see zone_owner.go.
	zoneOwners map[int]*zoneOwner
	// outdoor is the last outdoor temperature pushed, flowChanged when a curve
	// or the adaptation last changed each circuit's flow limit, flowEpoch the
	// engine start and flowAdapt the adaptation of each circuit. All four
	// belong to the engine goroutine; see flow_limit.go and flow_adapt.go.
	outdoor     *outdoorReading
	flowChanged map[int]time.Time
	flowEpoch   time.Time
	flowAdapt   map[int]*flowAdapt
	// thermal holds the heat-up model of each zone, heating the episode each
	// zone runs now, thermalTarget the target each zone followed at the last
	// tick, and early the block each zone started early. All four belong to
	// the engine goroutine; see early_start.go.
	thermal       map[int]*thermalModel
	heating       map[int]*heatEpisode
	thermalTarget map[int]float64
	early         map[int]earlyStart
	// plant holds the plant episode each zone records now. It belongs to the
	// engine goroutine; see plant_episode.go.
	plant map[int]*plantRun
	// energy integrates the boiler heat output. It belongs to the engine
	// goroutine; see boiler_energy.go.
	energy energyMeter
	// history is the open statistics file, nil while they are off. It and the
	// fields after it belong to the engine goroutine; see history.go.
	history     *historyStore
	historyCfg  historySettings
	historyErr  string
	historySlot time.Time
	historyDay  string
	historyPrev historyCounters
	started     time.Time
	// calibrated says whether the database held a passive receiver calibration
	// when the radio opened, and calPort is the serial port health names in the
	// command that stores one.
	calibrated bool
	calPort    string
	// pairBridgeOpen says that the bridge firmware took the open pair window
	// and nobody closed it yet. It belongs to the engine goroutine.
	pairBridgeOpen bool
	// installMu serialises the management mutations: one transaction and one
	// engine reload at a time, so two HTTP writers cannot interleave them.
	installMu sync.Mutex
	// fw holds the firmware transfer of each device, running or last
	// finished, keyed by wire EUI hex; see fw.go.
	fwMu        sync.Mutex
	fw          map[string]*fwTransfer
	fwTiming    fwTiming
	ulog        *tado.UnhandledLogger
	debugAPI    bool
	tapsMu      sync.Mutex
	taps        map[*rxTap]struct{}
	tapStop     chan struct{}
	tapStopOnce sync.Once
}

type cslAdmissionWait uint8

const (
	cslAdmissionQueueFull cslAdmissionWait = iota + 1
	cslAdmissionNeighborFull
)

const (
	// zoneProxySweep is how often expired zone-GET proxies are answered. It only
	// bounds the overshoot past EmuConfig.ZoneProxyTimeout, so it stays coarse.
	zoneProxySweep = time.Second
	// zoneStateSaveEvery bounds the saves a stream of zone cache changes makes.
	zoneStateSaveEvery = 5 * time.Second
	// Keep a submitted non-transactional proactive command out of a racing uplink.
	proactiveDownlinkRetryBackoff = 30 * time.Second
	// A relayed zone datagram retries on the scale of the requester's own CoAP
	// retransmission: a 30 s hold outlives the exchange it answers.
	zoneRelayRetryBackoff = 2 * time.Second
	// A job that found the channel busy put nothing on the air. It retries on
	// the scale of the commercial MAC retransmission, 2 to 6 s.
	cslBusyRetryBackoff = 2 * time.Second
	coapRetryMin        = 0x501 * time.Millisecond
	coapRetrySpread     = 0x280
	// beaconPoll is how often the daemon asks whether a periodic Router
	// Advertisement is due.
	beaconPoll = time.Second
)

// Shutdown budgets. `docker stop` grants 10 s before SIGKILL, so every wait on
// the shutdown path is bounded and their sum stays under it.
const (
	shutdownBudget   = 9 * time.Second // hard backstop, then os.Exit(1)
	httpDrainBudget  = 2 * time.Second // in-flight HTTP handlers
	stateSaveBudget  = 3 * time.Second // engine hop plus the final write
	radioCloseBudget = 2 * time.Second // radio.Close()
	stateCloseBudget = time.Second     // store.Close()
	// healthEngineBudget bounds the engine hop of GET /api/health. The engine
	// goroutine blocks on a live CSL transaction for the radio liveCSLTxTimeout plus
	// liveCSLPollTimeout, so a shorter budget would report a busy engine on every
	// device transaction.
	healthEngineBudget = 3 * time.Second
)

// newDaemon builds the service around one installation. in is what the API
// serves and what the reconciliation compares against; it is the installation
// the caller already gave the emulator.
func newDaemon(in *tado.Install, emu *tado.EmuState, radio radio.Radio, dryRun bool, mode string) *daemon {
	d := &daemon{
		emu: emu, radio: radio, dryRun: dryRun, mode: mode,
		cmds:             make(chan func()),
		dispatch:         make(chan struct{}, 1),
		done:             make(chan struct{}),
		radioLost:        make(chan error, 1),
		restart:          make(chan struct{}, 1),
		retryAfter:       time.After,
		retryNow:         time.Now,
		retryRand:        randomRetryUint16,
		retryDue:         make(map[string]uint64),
		retryAt:          make(map[string]time.Time),
		retryMID:         make(map[string]uint16),
		coapRetryBase:    make(map[string]coapRetryState),
		dispatchDue:      make(map[string]bool),
		cslAdmissionWait: make(map[string]cslAdmissionWait),
		linkMaint:        make(map[string]linkMaintEntry),
		zoneSched:        make(map[int]scheduledBlock),
		schedPushed:      make(map[int]scheduledBlock),
		awayTarget:       make(map[int]float64),
		awayPushed:       make(map[int]float64),
		programOwed:      make(map[programWrite]uint64),
		windows:          make(map[int]zoneWindow),
		windowOwed:       make(map[windowWrite]*windowOwed),
		roomTemps:        make(map[int]*roomTemp),
		autoOffsets:      make(map[int]*autoOffset),
		zoneOwners:       make(map[int]*zoneOwner),
		flowChanged:      make(map[int]time.Time),
		flowAdapt:        make(map[int]*flowAdapt),
		thermal:          make(map[int]*thermalModel),
		heating:          make(map[int]*heatEpisode),
		thermalTarget:    make(map[int]float64),
		early:            make(map[int]earlyStart),
		plant:            make(map[int]*plantRun),
		fw:               make(map[string]*fwTransfer),
		fwTiming:         defaultFwTiming,
		started:          time.Now(),
		taps:             make(map[*rxTap]struct{}),
		tapStop:          make(chan struct{}),
	}
	d.inst.Store(in)
	if emu != nil {
		emu.SetTXArmed(d.txArmed)
	}
	return d
}

// setInstall swaps the installation the daemon serves. It runs on the engine
// goroutine, so an HTTP caller reaches it through d.do. A rejected installation
// leaves the emulator and the daemon exactly as they were.
func (d *daemon) setInstall(in *tado.Install) (added, removed []string, err error) {
	before := d.inst.Load()
	added, removed, err = d.emu.SetInstall(in)
	if err != nil {
		return nil, nil, err
	}
	d.inst.Store(in)
	d.releaseFlowLimits(d.emu, before, in)
	d.reconcile(added, removed)
	if before != nil {
		d.restateShiftedZones(d.emu, before, in)
		d.restateAwayZones(d.emu, before, in)
		d.forgetRemovedZones(before, in)
		d.forgetZonePrograms(before, in)
		d.adoptHotWaterProgram(context.Background())
		// A member that moved zones, or joined one, follows the home program of
		// its zone in the active preset.
		if _, err := d.serveHomePrograms(d.emu, d.requestQueuedDispatch); err != nil {
			log.Printf("program presets: serving the home programs after the installation change: %v", err)
		}
		d.dropRoomTemps(in)
		d.dropAutoOffsets(in)
		d.dropZoneOwners(d.emu, in)
	}
	return added, removed, nil
}

// reconcile enrols the devices a reload added and drops the per-device
// bookkeeping of the ones it removed. The link-maintenance maps are keyed by
// wire EUI hex; the retry, dispatch and CSL-admission maps are keyed by the
// target string a handler addressed the device with, which is that same hex.
// The short address goes too: queueTarget falls back to the action's node
// name, which can be the short address. A CSL job still in flight carries its own
// copy of the target, so its completion finds no entry left and does nothing.
func (d *daemon) reconcile(added, removed []string) {
	for _, eui := range added {
		d.addLinkMaintenance(eui)
	}
	if len(removed) == 0 {
		return
	}
	gone := make(map[string]bool, 2*len(removed))
	for _, eui := range removed {
		gone[eui] = true
		wire, err := hex.DecodeString(eui)
		if err != nil {
			continue
		}
		if short := tado.ShortFromWire(wire); short != "" {
			gone[short] = true
		}
	}
	for key := range gone {
		delete(d.linkMaint, key)
		delete(d.retryDue, key)
		delete(d.retryAt, key)
		delete(d.retryMID, key)
		delete(d.coapRetryBase, key)
		delete(d.dispatchDue, key)
		delete(d.cslAdmissionWait, key)
	}
	order := make([]string, 0, len(d.linkMaintOrder))
	for _, key := range d.linkMaintOrder {
		if !gone[key] {
			order = append(order, key)
		}
	}
	d.linkMaintOrder = order
	if d.linkMaintCursor >= len(order) {
		d.linkMaintCursor = 0
	}
	queue := make([]string, 0, len(d.dispatchQ))
	for _, target := range d.dispatchQ {
		if !gone[target] {
			queue = append(queue, target)
		}
	}
	d.dispatchQ = queue
}

// run is the sole owner of d.emu. It exits only when ctx is cancelled.
func (d *daemon) run(ctx context.Context) {
	frames := d.radio.Frames()
	var cslCompletions <-chan radio.CSLCompletion
	if radio, ok := d.radio.(cslCompletionRadio); ok {
		cslCompletions = radio.CSLCompletions()
	}
	d.initLinkMaintenance()
	d.flowEpoch = d.retryNow()
	maintenance := time.NewTicker(128 * time.Millisecond)
	defer maintenance.Stop()
	// Proxied zone GETs expire on wall time, not on received traffic: the member
	// we asked may have gone silent altogether, so the tick has to fire even
	// when no frame arrives.
	expiry := time.NewTicker(zoneProxySweep)
	defer expiry.Stop()
	var beaconC <-chan time.Time
	var beaconTicker *time.Ticker
	if d.beaconEvery > 0 {
		// The emulator draws each interval between a third of beaconEvery and
		// all of it, so the tick only polls it.
		beaconTicker = time.NewTicker(min(d.beaconEvery, beaconPoll))
		beaconC = beaconTicker.C
		defer beaconTicker.Stop()
	}
	for {
		select {
		case fn := <-d.cmds:
			fn()
		case <-d.dispatch:
			if len(d.dispatchQ) == 0 {
				continue
			}
			target := d.dispatchQ[0]
			d.dispatchQ = d.dispatchQ[1:]
			delete(d.dispatchDue, target)
			d.dispatchQueued(d.emu, target)
			if len(d.dispatchQ) > 0 {
				select {
				case d.dispatch <- struct{}{}:
				default:
				}
			}
		case <-expiry.C:
			d.emu.ExpireFragments()
			for _, a := range d.emu.ExpireZoneProxies() {
				d.tx(a)
			}
			d.tickCounters(d.emu)
			d.tickHomeState(d.emu)
			d.tickRoomTemps(d.emu)
			d.tickAutoOffsets(d.emu)
			d.tickZoneOwners(d.emu)
			d.tickWindows(d.emu)
			d.tickPairWindow(d.emu)
			d.tickEnergy()
			d.tickHistory(d.emu)
			flowChanged := d.tickFlowLimits(d.emu)
			utcChanged := d.tickUTCOffset(d.emu)
			d.tickFwRead(d.emu)
			d.tickSerials(d.emu)
			targets, err := d.emu.ExpireBoosts(d.retryNow())
			if err != nil {
				log.Printf("boost restore: %v", err)
			}
			for _, target := range targets {
				d.requestQueuedDispatch(target)
			}
			if len(targets) > 0 || flowChanged || utcChanged {
				d.saveEngineState(ctx, d.emu, "boost restore, flow limit or utc offset")
			}
			d.flushUnsaved(ctx, d.emu)
		case <-maintenance.C:
			d.linkMaintenanceTick(d.emu)
		case <-beaconC:
			// A dry-run holds the tick: the burst is 255 frames to log.
			if !d.txArmed() {
				continue
			}
			actions, due, err := d.emu.MaybeBeacon()
			if err != nil {
				log.Printf("beacon: %v", err)
				continue
			}
			if due {
				d.txActions(actions)
			}
		case fr, ok := <-frames:
			if !ok {
				// A finished replay must not stop the engine: the API keeps
				// serving the state the capture produced. A live radio that
				// ended on an error stops the process, see Main.
				frames = nil
				d.noteRadioLost()
				continue
			}
			d.handleFrame(fr, true)
		case completion := <-cslCompletions:
			d.handleCSLCompletion(completion)
			for _, frame := range completion.Frames {
				d.handleFrame(frame, false)
			}
			d.chainQueuedDispatch(completion)
		case <-ctx.Done():
			close(d.done)
			return
		}
	}
}

// noteRadioLost hands the error of a live radio loop that ended on its own to
// Main. A replay or a mock has no radio error, so its end changes nothing.
func (d *daemon) noteRadioLost() {
	radio, ok := d.radio.(liveRadioStatus)
	if !ok {
		return
	}
	if err := radio.RadioError(); err != nil {
		select {
		case d.radioLost <- err:
		default:
		}
	}
}

func (d *daemon) handleCSLCompletion(completion radio.CSLCompletion) {
	action := completion.Action
	target := d.queueTarget(action)
	d.releaseCSLAdmissionWait(target)
	d.logCSLCompletionFollow(completion)
	if !action.QueueAttempt || len(action.Bytes) < 11 {
		return
	}
	d.emu.CompleteQueuedDownlinkAttempt(action.Bytes[3:11], action.Bytes[2])
	if completion.Result == nil || completion.Result.Sent == 0 {
		delay := proactiveDownlinkRetryBackoff
		if errors.Is(completion.Err, cc1101.ErrCSLTxBusy) {
			delay = cslBusyRetryBackoff
		}
		retryAt := d.retryNow().Add(delay)
		if d.emu.DeferDownlinkAttempt(action.Bytes[3:11], action.Bytes[2], retryAt) {
			// Sent=0 did not start the CoAP transaction. Replace its admission T0.
			d.replaceQueuedRetryAt(d.emu, target, retryAt)
		}
		return
	}
	if action.CoAPTransaction {
		if retryAt := d.retryAt[target]; !retryAt.IsZero() && !d.retryNow().Before(retryAt) {
			delete(d.retryDue, target)
			delete(d.retryAt, target)
			delete(d.retryMID, target)
			d.requestQueuedDispatch(target)
		}
		return
	}
	if action.ResponseRetry && completion.Err == nil && completion.Result.Ack == nil {
		d.requestQueuedDispatch(target)
		return
	}
	delay := proactiveDownlinkRetryBackoff
	if action.ZoneRelay {
		delay = zoneRelayRetryBackoff
	}
	d.emu.HoldQueuedDownlink(target, delay)
	d.scheduleQueuedRetryAfter(d.emu, target, delay)
}

// chainQueuedDispatch sends the next queued downlink of a target right after an
// Enhanced ACK closed the previous one: the anchor is fresh, and waiting for the
// next uplink or the retry timer left relayed answers queued for minutes. A
// d/pair behind the attempt that ended goes at once with or without the ACK:
// it does not wait for the answer or the retry of that attempt.
func (d *daemon) chainQueuedDispatch(completion radio.CSLCompletion) {
	action := completion.Action
	if !action.QueueAttempt || completion.Result == nil {
		return
	}
	target := d.queueTarget(action)
	queue := d.emu.Queue(target)
	if len(queue) == 0 {
		return
	}
	pairWaits := queue[0].Kind != "pair" && slices.ContainsFunc(queue[1:], func(q tado.QueueEntry) bool { return q.Kind == "pair" })
	if (completion.Result.Ack != nil && !queue[0].InFlight) || pairWaits {
		d.requestQueuedDispatch(target)
	}
}

// logCSLCompletionFollow records only frames returned by the scheduler follow window.
func (d *daemon) logCSLCompletionFollow(completion radio.CSLCompletion) {
	if completion.Result == nil || len(completion.Result.Follow) == 0 {
		return
	}
	var key []byte
	if inst := d.inst.Load(); inst != nil {
		key = inst.NetworkKey()
	}
	for _, follow := range completion.Result.Follow {
		if follow == nil {
			continue
		}
		if !follow.CRCOK {
			radio.LogCRCDrop("csl follow", completion.Action.Node, follow)
			continue
		}
		log.Printf("csl follow: %s", formatCSLFollow(completion.Action, key, follow.Payload))
	}
}

func formatCSLFollow(action tado.Action, key, raw []byte) string {
	requestURI := "unavailable"
	var request *tado.CoAPMessage
	if data, err := tado.ParseData(key, action.Bytes); err == nil {
		if app := tado.ParseApp(data.Plaintext); app != nil {
			request = app.CoAP
			if request != nil {
				requestURI = request.URI()
			}
		}
	}
	class := tado.Classify(raw)
	if class != tado.ClassData && class != tado.ClassDataPending {
		return fmt.Sprintf("node=%s request=%s non-coap class=%s", action.Node, requestURI, class)
	}
	data, err := tado.ParseData(key, raw)
	if err != nil {
		return fmt.Sprintf("node=%s request=%s non-coap parse=%v", action.Node, requestURI, err)
	}
	app := tado.ParseApp(data.Plaintext)
	if app == nil || app.CoAP == nil {
		return fmt.Sprintf("node=%s request=%s non-coap app", action.Node, requestURI)
	}
	response := app.CoAP
	responseURI := response.URI()
	if responseURI == "" {
		responseURI = "-"
	}
	midMatch := request != nil && response.MID == request.MID
	tokenMatch := request != nil && bytes.Equal(response.Token, request.Token)
	return fmt.Sprintf("node=%s request=%s response=%s code=%s mid=%04x mid-match=%t token-match=%t tlv=%s",
		action.Node, requestURI, responseURI, response.Code, response.MID, midMatch, tokenMatch, formatCSLFollowTLVs(app.TLVs))
}

func formatCSLFollowTLVs(tlvs []tado.TLV) string {
	if len(tlvs) == 0 {
		return "-"
	}
	values := make([]string, 0, len(tlvs))
	for _, tlv := range tlvs {
		if tlv.Tag == "62a0" && len(tlv.Val) == 1 {
			values = append(values, "62a0="+hex.EncodeToString(tlv.Val))
			continue
		}
		values = append(values, tlv.Tag)
	}
	return strings.Join(values, ",")
}

func (d *daemon) queueTarget(action tado.Action) string {
	if action.QueueTarget != "" {
		return action.QueueTarget
	}
	if inst := d.inst.Load(); inst != nil && len(action.Bytes) >= 11 {
		for _, device := range inst.Devices {
			if bytes.Equal(action.Bytes[3:11], device.WireEUI()) {
				return device.Serial
			}
		}
	}
	return action.Node
}

func (d *daemon) waitCSLAdmission(target string, err error) bool {
	var wait cslAdmissionWait
	switch {
	case errors.Is(err, cc1101.ErrCSLQueueFull):
		wait = cslAdmissionQueueFull
	case errors.Is(err, cc1101.ErrCSLQueueNeighborFull):
		wait = cslAdmissionNeighborFull
	default:
		return false
	}
	d.cslAdmissionWait[target] = wait
	return true
}

func (d *daemon) releaseCSLAdmissionWait(target string) {
	if _, ok := d.cslAdmissionWait[target]; !ok {
		var candidates []string
		for candidate, wait := range d.cslAdmissionWait {
			if wait == cslAdmissionNeighborFull {
				candidates = append(candidates, candidate)
			}
		}
		sort.Strings(candidates)
		if len(candidates) != 0 {
			target = candidates[0]
		}
	}
	if target == "" {
		return
	}
	if _, ok := d.cslAdmissionWait[target]; ok {
		delete(d.cslAdmissionWait, target)
		d.requestQueuedDispatch(target)
	}
}

const (
	linkMaintStartupTicks  = 0x80
	linkMaintIdleTicks     = 0x708
	linkMaintRetryTicks    = 15
	linkMaintPostSendTicks = 3
)

type linkMaintEntry struct {
	state byte
	retry int8
	age   uint16
	wait  uint16
}

func (d *daemon) initLinkMaintenance() {
	inst := d.inst.Load()
	if inst == nil {
		return
	}
	for _, dev := range inst.Devices {
		d.addLinkMaintenance(hex.EncodeToString(dev.WireEUI()))
	}
}

// addLinkMaintenance enrols one wire EUI, keeping whatever state an entry
// already carries: a reload must not restart the cycle of a device that stays.
func (d *daemon) addLinkMaintenance(key string) {
	if _, ok := d.linkMaint[key]; ok {
		return
	}
	d.linkMaint[key] = linkMaintEntry{state: 1, retry: -1}
	d.linkMaintOrder = append(d.linkMaintOrder, key)
}

func (d *daemon) noteLinkMaintenanceInbound() {
	if d.inst.Load() == nil {
		return
	}
	key := hex.EncodeToString(d.emu.TakeAcceptedInboundIPv6())
	entry, ok := d.linkMaint[key]
	if !ok {
		return
	}
	entry.state, entry.retry, entry.age, entry.wait = 1, -1, 0, 0
	d.linkMaint[key] = entry
}

func (d *daemon) linkMaintenanceTick(e *tado.EmuState) {
	if !e.AutoQueueEnabled() {
		return
	}
	if d.linkMaintTick != ^uint16(0) {
		d.linkMaintTick++
	}
	if d.linkMaintTick < linkMaintStartupTicks {
		return
	}
	for target, entry := range d.linkMaint {
		if entry.state == 2 && entry.retry == -1 {
			continue
		}
		if entry.retry == -1 {
			entry.age++
			if entry.age < linkMaintIdleTicks {
				d.linkMaint[target] = entry
				continue
			}
			entry.retry, entry.wait = 0, linkMaintRetryTicks
		}
		if entry.wait > 0 {
			entry.wait--
			if entry.wait == 0 && entry.retry >= 4 {
				entry.state, entry.retry, entry.age = 2, -1, 0
			}
			d.linkMaint[target] = entry
			continue
		}
		if entry.retry >= 4 {
			entry.state, entry.retry, entry.age = 2, -1, 0
			d.linkMaint[target] = entry
			continue
		}
		d.linkMaint[target] = entry
	}
	if d.linkMaintWait > 0 {
		d.linkMaintWait--
		if d.linkMaintWait > 0 {
			return
		}
	}
	for n := range d.linkMaintOrder {
		i := (d.linkMaintCursor + n) % len(d.linkMaintOrder)
		target := d.linkMaintOrder[i]
		entry := d.linkMaint[target]
		if entry.state == 2 || entry.retry < 0 || entry.wait != 0 {
			continue
		}
		d.sendLinkMaintenanceEcho(e, target)
		entry.retry++
		entry.wait = linkMaintPostSendTicks
		d.linkMaint[target] = entry
		d.linkMaintCursor = (i + 1) % len(d.linkMaintOrder)
		d.linkMaintWait = linkMaintPostSendTicks
		break
	}
}

func (d *daemon) sendLinkMaintenanceEcho(e *tado.EmuState, target string) {
	if d.dryRun || e.BridgeSeen() || e.FirmwareLocked(target) {
		return
	}
	batcher, ok := d.radio.(immediateBatchRadio)
	if !ok {
		return
	}
	action, err := e.LinkMaintenanceEcho(target)
	if err != nil {
		log.Printf("link maintenance: %v", err)
		return
	}
	result, err := batcher.SendImmediate([]tado.Action{action}, true)
	if err != nil {
		log.Printf("link maintenance: %v", err)
	}
	for _, ack := range result.Acks {
		d.handleFrame(ack, false)
	}
}

func (d *daemon) txActions(actions []tado.Action) {
	if len(actions) == 0 {
		return
	}
	if !d.dryRun && !d.emu.BridgeSeen() {
		frames := make([][]byte, 0, len(actions))
		for _, a := range actions {
			if !a.NotBefore.IsZero() {
				frames = nil
				break
			}
			frames = append(frames, a.Bytes)
		}
		if frames != nil {
			if batcher, ok := d.radio.(asyncBatchRadio); ok {
				err := batcher.SendBatch(frames)
				d.completeBeacon(err == nil)
				if err != nil {
					log.Printf("async batch tx: %v", err)
				}
				return
			}
		}
	}
	for _, a := range actions {
		d.tx(a)
	}
	d.completeBeacon(!d.dryRun)
}

func (d *daemon) frame(fr []byte) {
	d.handleFrame(fr, true)
}

// handleFrame is the single funnel for received frames: the radio, a CSL
// completion, a re-injected Enhanced ACK and a link-maintenance ACK all land
// here, so it is where the rx taps read.
func (d *daemon) handleFrame(fr []byte, paired bool) {
	d.publishRx(fr)
	actions := d.emu.OnFrame(fr)
	// After the answer: the report takes its 2.05 before the zone writes.
	defer d.takeWindowReports(d.emu)
	defer d.takeOverlayReports(d.emu)
	defer d.logFallbackEvents(d.emu)
	defer d.logZoneStateReports(d.emu)
	defer d.saveLearned(d.emu)
	defer d.noteBoiler(d.emu)
	d.noteLinkMaintenanceInbound()
	batcher, live := d.radio.(immediateBatchRadio)
	if !live || d.dryRun {
		for _, a := range actions {
			d.tx(a)
		}
		d.completeBeacon(!d.dryRun)
		return
	}
	batch := make([]tado.Action, 0, len(actions))
	for _, a := range actions {
		if a.Kind == tado.ActEnhAck {
			continue
		}
		if !a.NotBefore.IsZero() && time.Until(a.NotBefore) > 0 {
			d.tx(a)
			continue
		}
		if d.emu.BridgeSeen() {
			log.Printf("bridge present on-air, refusing to transmit (sole-coordinator assumption): [%s] node=%s reason=%s",
				a.Kind, a.Node, a.Reason)
			continue
		}
		batch = append(batch, a)
	}
	if !paired && len(batch) == 0 && !d.emu.BridgeSeen() {
		return
	}
	result, err := batcher.SendImmediate(batch, !d.emu.BridgeSeen())
	d.revertUnsubmitted(batch, append(append([]tado.Action(nil), result.Submitted...), result.Accepted...))
	markAwaitingCSL(d.emu, result)
	d.completeBeacon(err == nil)
	if err != nil {
		log.Printf("immediate tx: %v", err)
		return
	}
	for _, ack := range result.Acks {
		d.handleFrame(ack, false)
	}
}

// dispatchQueued runs on the daemon goroutine and leaves radio.Live as the sole
// serial-port owner while it sends one explicit queued CSL transaction.
func (d *daemon) dispatchQueued(e *tado.EmuState, target string) {
	if d.dryRun || e.BridgeSeen() {
		return
	}
	batcher, ok := d.radio.(immediateBatchRadio)
	if !ok {
		return
	}
	actions, ok, err := e.NextQueuedDownlink(target)
	if err != nil {
		log.Printf("queued dispatch: %v", err)
		return
	}
	if !ok {
		if e.Pending(target) == 0 {
			delete(d.coapRetryBase, target)
			return
		}
		if e.QueuedDownlinkAwaitingCSL(target) {
			return
		}
		d.scheduleQueuedRetry(e, target)
		return
	}
	before := e.Pending(target)
	coapTransaction := len(actions) != 0 && actions[0].CoAPTransaction
	result, err := batcher.SendImmediate(actions, true)
	d.revertUnsubmitted(actions, append(append([]tado.Action(nil), result.Submitted...), result.Accepted...))
	if err != nil && result.CSLQueue && d.waitCSLAdmission(target, err) {
		log.Printf("queued dispatch: %v", err)
		return
	}
	for _, ack := range result.Acks {
		d.handleFrame(ack, false)
	}
	after := e.Pending(target)
	if after == 0 {
		delete(d.coapRetryBase, target)
		delete(d.retryAt, target)
		delete(d.retryMID, target)
		return
	}
	if after < before {
		d.requestQueuedDispatch(target)
		return
	}
	if len(result.Accepted) > 0 {
		markAwaitingCSL(e, result)
		if coapTransaction {
			retryAt, _ := d.coapRetryDeadline(actions, target)
			e.HoldQueuedDownlink(target, retryAt.Sub(d.retryNow()))
			d.scheduleQueuedRetryAt(e, target, retryAt)
		}
		return
	}
	if len(result.Submitted) > 0 {
		if coapTransaction {
			retryAt, _ := d.coapRetryDeadline(actions, target)
			e.HoldQueuedDownlink(target, retryAt.Sub(d.retryNow()))
			d.scheduleQueuedRetryAt(e, target, retryAt)
		} else {
			e.HoldQueuedDownlink(target, proactiveDownlinkRetryBackoff)
			d.scheduleQueuedRetry(e, target)
		}
	} else if coapTransaction {
		if !result.CSLQueue {
			retryAt, _ := d.coapRetryDeadline(actions, target)
			e.HoldQueuedDownlink(target, retryAt.Sub(d.retryNow()))
			d.scheduleQueuedRetryAt(e, target, retryAt)
		}
	} else {
		d.scheduleQueuedRetry(e, target)
	}
	if err != nil {
		log.Printf("queued dispatch: %v", err)
	}
}

// markAwaitingCSL holds each queued attempt the ESP accepted until its
// completion, so an uplink that races it cannot rebuild the command under a
// new MAC seq and orphan the Enhanced ACK of the attempt on the air.
func markAwaitingCSL(e *tado.EmuState, result radio.ImmediateResult) {
	if !result.CSLQueue {
		return
	}
	for _, accepted := range result.Accepted {
		if accepted.QueueAttempt && len(accepted.Bytes) >= 11 {
			e.MarkQueuedDownlinkAwaitingCSL(accepted.Bytes[3:11], accepted.Bytes[2])
		}
	}
}

func (d *daemon) requestQueuedDispatch(target string) {
	if d.dispatchDue[target] {
		return
	}
	d.dispatchDue[target] = true
	d.dispatchQ = append(d.dispatchQ, target)
	select {
	case d.dispatch <- struct{}{}:
	default:
	}
}

func (d *daemon) scheduleQueuedRetry(e *tado.EmuState, target string) {
	d.scheduleQueuedRetryAfter(e, target, proactiveDownlinkRetryBackoff)
}

func (d *daemon) scheduleQueuedRetryAfter(e *tado.EmuState, target string, delay time.Duration) {
	d.scheduleQueuedRetryAt(e, target, d.retryNow().Add(delay))
}

func (d *daemon) scheduleQueuedRetryAt(e *tado.EmuState, target string, retryAt time.Time) {
	d.scheduleQueuedRetryAtMode(e, target, retryAt, false)
}

func (d *daemon) replaceQueuedRetryAt(e *tado.EmuState, target string, retryAt time.Time) {
	d.scheduleQueuedRetryAtMode(e, target, retryAt, true)
}

func (d *daemon) scheduleQueuedRetryAtMode(e *tado.EmuState, target string, retryAt time.Time, replace bool) {
	if d.dryRun || e.BridgeSeen() || e.Pending(target) == 0 {
		return
	}
	queue := e.Queue(target)
	if len(queue) == 0 {
		return
	}
	mid := queue[0].MID
	if !replace && d.retryDue[target] != 0 && d.retryMID[target] == mid {
		return
	}
	delay := retryAt.Sub(d.retryNow())
	if delay < 0 {
		delay = 0
	}
	d.retryDue[target]++
	generation := d.retryDue[target]
	d.retryAt[target] = retryAt
	d.retryMID[target] = mid
	go func() {
		select {
		case <-d.retryAfter(delay):
		case <-d.done:
			return
		}
		select {
		case d.cmds <- func() {
			if d.retryDue[target] != generation || d.retryMID[target] != mid {
				return
			}
			if d.emu.QueuedDownlinkAwaitingCSL(target) {
				return
			}
			delete(d.retryDue, target)
			delete(d.retryAt, target)
			delete(d.retryMID, target)
			if d.dryRun || d.emu.BridgeSeen() || d.emu.Pending(target) == 0 {
				return
			}
			d.requestQueuedDispatch(target)
		}:
		case <-d.done:
		}
	}()
}

func (d *daemon) coapRetryDeadline(actions []tado.Action, target string) (time.Time, bool) {
	if len(actions) == 0 || !actions[0].CoAPTransaction || actions[0].QueueTry < 1 {
		return time.Time{}, false
	}
	state := d.coapRetryBase[target]
	if state.mid != actions[0].QueueMID {
		state = coapRetryState{}
	}
	if state.base == 0 {
		state = coapRetryState{
			base: coapRetryMin + time.Duration(d.retryRand()%coapRetrySpread)*time.Millisecond,
			mid:  actions[0].QueueMID,
		}
		d.coapRetryBase[target] = state
	}
	delay := state.base << (actions[0].QueueTry - 1)
	return d.retryNow().Add(delay), true
}

type coapRetryState struct {
	base time.Duration
	mid  uint16
}

func randomRetryUint16() uint16 {
	var b [2]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		return 0
	}
	return binary.BigEndian.Uint16(b[:])
}

func (d *daemon) revertUnsubmitted(actions, submitted []tado.Action) {
	type attempt struct {
		action    tado.Action
		submitted bool
	}
	attempts := make(map[string]attempt)
	for i := 0; i < len(actions); i++ {
		action := actions[i]
		if !action.QueueAttempt || len(action.Bytes) < 11 {
			continue
		}
		attempts[string(action.Bytes[3:11])] = attempt{action: action}
	}
	for _, sent := range submitted {
		if !sent.QueueAttempt || len(sent.Bytes) < 11 {
			continue
		}
		key := string(sent.Bytes[3:11])
		entry, ok := attempts[key]
		if !ok {
			continue
		}
		entry.submitted = true
		attempts[key] = entry
	}
	for _, entry := range attempts {
		if !entry.submitted {
			d.emu.RevertDownlinkAttempt(entry.action.Bytes[3:11], entry.action.Bytes[2])
		}
	}
}

func (d *daemon) tx(a tado.Action) {
	// An action the engine held back is a fault injection, nothing else: the CSL
	// rendezvous is already open when a normal action is decided. Sleeping here
	// would stall the state machine, so the wait happens on its own goroutine -
	// but the send itself is posted back to the engine, which owns the radio.
	if wait := time.Until(a.NotBefore); !a.NotBefore.IsZero() && wait > 0 {
		log.Printf("fault-injection: holding [%s] node=%s reason=%s for %s",
			a.Kind, a.Node, a.Reason, wait.Round(time.Millisecond))
		held := a
		held.NotBefore = time.Time{}
		go func() {
			select {
			case <-time.After(wait):
			case <-d.done:
				return
			}
			// Best-effort: a shutdown between the timer and here drops it.
			d.do(func(*tado.EmuState) (any, error) { d.txNow(held); return nil, nil })
		}()
		return
	}
	d.txNow(a)
}

// txNow puts one action on air, or reports why it did not.
func (d *daemon) txNow(a tado.Action) {
	if d.dryRun {
		log.Printf("dry-run [%s] node=%s reason=%s", a.Kind, a.Node, a.Reason)
		return
	}
	if d.emu.BridgeSeen() {
		log.Printf("bridge present on-air, refusing to transmit (sole-coordinator assumption): [%s] node=%s reason=%s",
			a.Kind, a.Node, a.Reason)
		return
	}
	if err := d.radio.Send(a.Bytes); err != nil {
		log.Printf("tx [%s] node=%s reason=%s: %v", a.Kind, a.Node, a.Reason, err)
	}
}

// do runs fn on the engine goroutine and returns its result. It waits for the
// engine as long as it needs: an API handler uses doReq instead.
func (d *daemon) do(fn func(*tado.EmuState) (any, error)) (any, error) {
	return d.doCtx(context.Background(), 0, fn)
}

// doReq is do for an API handler: it gives up with errEngineBusy when the
// request context ends, so a wedged engine does not hold one goroutine per
// request.
func (d *daemon) doReq(r *http.Request, fn func(*tado.EmuState) (any, error)) (any, error) {
	return d.doCtx(r.Context(), 0, fn)
}

// doWithin is do with a deadline. A zero timeout waits for the engine forever.
func (d *daemon) doWithin(timeout time.Duration, fn func(*tado.EmuState) (any, error)) (any, error) {
	return d.doCtx(context.Background(), timeout, fn)
}

// doCtx gives up when ctx ends or timeout passes, whichever is first. A
// command the engine already took still runs to its end: its reply channel
// has room for the one answer nobody reads.
func (d *daemon) doCtx(ctx context.Context, timeout time.Duration, fn func(*tado.EmuState) (any, error)) (any, error) {
	type res struct {
		v   any
		err error
	}
	var give <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		give = t.C
	}
	reply := make(chan res, 1)
	select {
	case d.cmds <- func() { v, err := fn(d.emu); reply <- res{v, err} }:
	case <-d.done:
		return nil, errStopped
	case <-give:
		return nil, errEngineBusy
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: %w", errEngineBusy, context.Cause(ctx))
	}
	select {
	case r := <-reply:
		return r.v, r.err
	case <-d.done:
		return nil, errStopped
	case <-give:
		return nil, errEngineBusy
	case <-ctx.Done():
		select {
		case r := <-reply:
			return r.v, r.err
		default:
		}
		return nil, fmt.Errorf("%w: %w", errEngineBusy, context.Cause(ctx))
	}
}

type stateSaveError struct{ err error }

func (e *stateSaveError) Error() string { return "persist desired state: " + e.err.Error() }

func (e *stateSaveError) Unwrap() error { return e.err }

// loadDesiredState opens the state database and applies it to emu. The daemon
// starts on an empty desired state when the database holds none, and stops on a
// database it cannot read or cannot validate: a state that loads in part is
// worse than none. The caller keeps the returned store for the saves.
func loadDesiredState(path string, emu *tado.EmuState) (*stateStore, error) {
	if path == "" {
		return nil, nil
	}
	store, err := openStateStore(path)
	if err != nil {
		return nil, err
	}
	if err := restoreDesiredState(store, emu); err != nil {
		store.Close()
		return nil, fmt.Errorf("state database %s: %w", path, err)
	}
	return store, nil
}

// restoreDesiredState applies the stored desired state to emu. Main opens the
// store itself, early, because the installation comes out of the same file.
func restoreDesiredState(store *stateStore, emu *tado.EmuState) error {
	if store == nil {
		return nil
	}
	state, err := store.Load()
	if err != nil {
		return err
	}
	return emu.RestoreDesiredState(state)
}

// closeStore releases the database on an early exit of Main.
func closeStore(store *stateStore) {
	if store != nil {
		closeWithin("the state database", stateCloseBudget, store.Close)
	}
}

// stateSnapshot pairs an export with the sequence number that orders its save.
type stateSnapshot struct {
	state tado.DesiredState
	seq   uint64
}

// saveDesiredState waits for the engine and for the write as long as they need.
// The API handlers use it: their caller already holds a request deadline.
func (d *daemon) saveDesiredState() error {
	if d.state == nil {
		return nil
	}
	snap, err := d.exportForSave(0)
	if err != nil {
		return err
	}
	return d.saveDesiredStateSnapshot(context.Background(), snap.state, snap.seq)
}

// saveDesiredStateBefore spends one deadline on the whole save: the engine hop
// and the write end at the same instant, so a slow engine leaves the write
// exactly what is left of the budget instead of starting a second one.
func (d *daemon) saveDesiredStateBefore(deadline time.Time) error {
	if d.state == nil {
		return nil
	}
	timeout := time.Until(deadline)
	if timeout <= 0 {
		return errEngineBusy
	}
	snap, err := d.exportForSave(timeout)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	return d.saveDesiredStateSnapshot(ctx, snap.state, snap.seq)
}

func (d *daemon) exportForSave(timeout time.Duration) (stateSnapshot, error) {
	v, err := d.doWithin(timeout, func(e *tado.EmuState) (any, error) {
		// On the engine goroutine: the counter needs no atomic, and the snapshot
		// and its sequence are taken together.
		d.saveSeq++
		return stateSnapshot{state: e.ExportDesiredState(), seq: d.saveSeq}, nil
	})
	if err != nil {
		return stateSnapshot{}, err
	}
	return v.(stateSnapshot), nil
}

// attachState gives the daemon its store and continues the store's save
// sequence: a save numbered below the stored one is skipped as stale, so a
// sequence restarted at 0 would drop every save of the new run until it caught up.
func (d *daemon) attachState(store *stateStore) error {
	d.state = store
	if store == nil {
		return nil
	}
	seq, _, err := install.ReadMetaUint(context.Background(), store.db, "save_seq")
	if err != nil {
		return err
	}
	d.saveSeq = seq
	return nil
}

const (
	// counterLease is how far past the frame counter in use the stored resume
	// point runs. The tick stores a new one once less than half is left: a
	// second of traffic never spends 2048 frames, so a restart after a crash
	// resumes above every counter used.
	counterLease = 4096
	// midLease is the same for the CoAP Message ID. It wraps through 0: what
	// matters is that a restart does not repeat a Message ID a device still
	// holds in its deduplication state (EXCHANGE_LIFETIME, 247 s).
	midLease = 4096
)

// Meta keys of the stored resume points. Broadcasts share the unicast frame
// counter, so metaBcastResume is only read: a database written while they
// had a counter of their own keeps it as a floor.
const (
	metaTxResume    = "tx_counter_resume"
	metaBcastResume = "broadcast_counter_resume"
	metaMIDResume   = "coap_mid_resume"
)

// restoreCounters resumes the frame counter and the CoAP Message ID from their
// stored resume points. The configured frame counter seed stays a floor. A
// daemon that transmits on a live radio then stores new resume points before
// its first frame. A dry-run or a replay puts nothing on the air: it reads the
// resume points and stores none.
func (d *daemon) restoreCounters(e *tado.EmuState) error {
	if d.state == nil {
		return nil
	}
	ctx := context.Background()
	stored := make(map[string]uint64, 3)
	for _, key := range []string{metaTxResume, metaBcastResume, metaMIDResume} {
		v, found, err := install.ReadMetaUint(ctx, d.state.db, key)
		if err != nil {
			return err
		}
		if found {
			stored[key] = v
		}
	}
	if v := uint32(max(stored[metaTxResume], stored[metaBcastResume])); v > e.TxCounter() {
		e.SetTxCounter(v)
	}
	if v, ok := stored[metaMIDResume]; ok {
		tado.SeedMID(uint16(v))
	}
	if !d.leasesCounters() {
		return nil
	}
	return d.leaseCounters(e, true)
}

const (
	metaRouterAdvertAt   = "router_advert_at"   // unix seconds of the last burst on air
	metaRouterAdvertNext = "router_advert_next" // seconds drawn for the next burst
)

// completeBeacon ends a router advertisement burst and stores its instant.
func (d *daemon) completeBeacon(sent bool) {
	if !d.emu.CompleteBeacon(sent) || d.state == nil {
		return
	}
	last, next, _ := d.emu.RouterAdvertState()
	if err := d.state.SetCounterResume(context.Background(), map[string]uint64{
		metaRouterAdvertAt: uint64(last.Unix()), metaRouterAdvertNext: uint64(next / time.Second),
	}); err != nil {
		log.Printf("persist router advertisement instant: %v", err)
	}
}

// restoreRouterAdvert continues the interval of the last burst across a
// restart. The coordinator advertises every 200 to 600 s (PROTOCOL.md, Router
// advertisement). A daemon that restarts in a loop would otherwise send a
// 254-frame burst at every start and hold the radio for a second each time.
func (d *daemon) restoreRouterAdvert(e *tado.EmuState) error {
	if d.state == nil {
		return nil
	}
	ctx := context.Background()
	at, foundAt, err := install.ReadMetaUint(ctx, d.state.db, metaRouterAdvertAt)
	if err != nil {
		return err
	}
	next, foundNext, err := install.ReadMetaUint(ctx, d.state.db, metaRouterAdvertNext)
	if err != nil {
		return err
	}
	if foundAt && foundNext && at <= math.MaxInt64 && next <= uint64(math.MaxInt64/time.Second) {
		e.RestoreRouterAdvert(time.Unix(int64(at), 0), time.Duration(next)*time.Second)
	}
	return nil
}

// leasesCounters reports whether the daemon stores resume points: it
// transmits on a live radio.
func (d *daemon) leasesCounters() bool {
	return d.state != nil && !d.dryRun && d.mode == "live"
}

// leaseCounters stores resume points a lease past the counters in use once
// less than half a lease is left, or at once with force.
func (d *daemon) leaseCounters(e *tado.EmuState, force bool) error {
	tx, mid := e.TxCounter(), tado.LastMID()
	if !force && int32(d.txResume-tx) >= counterLease/2 && int16(d.midResume-mid) >= midLease/2 {
		return nil
	}
	txResume, midResume := leaseAbove(tx), mid+midLease
	if err := d.state.SetCounterResume(context.Background(), map[string]uint64{
		metaTxResume: uint64(txResume), metaMIDResume: uint64(midResume),
	}); err != nil {
		return err
	}
	d.txResume, d.midResume = txResume, midResume
	return nil
}

func leaseAbove(v uint32) uint32 {
	return uint32(min(uint64(v)+counterLease, uint64(^uint32(0))))
}

// tickCounters moves the stored resume points ahead on the tick.
func (d *daemon) tickCounters(e *tado.EmuState) {
	if !d.leasesCounters() {
		return
	}
	if err := d.leaseCounters(e, false); err != nil {
		log.Printf("persist frame counter resume points: %v", err)
	}
}

func (d *daemon) saveDesiredStateSnapshot(ctx context.Context, state tado.DesiredState, seq uint64) error {
	if d.state == nil {
		return nil
	}
	return d.state.Save(ctx, state, seq)
}

// saveLearned stores a thermostat's interface module id and a served session id
// as soon as the engine has them: the module id gates the only document the
// thermostat may be served, and the session id lets a restart recognise a
// device that echoes it. A changed z/s block is saved too, at most once per
// zoneStateSaveEvery: after a restart the window engine and the zone rewrites
// restate the overlay from it. So is a delivered child lock or d/dbg2 record,
// which moves the desired state on the device's answer, and a fw/state report,
// so a restart keeps the running version. The tick saves what the bound held
// back.
func (d *daemon) saveLearned(e *tado.EmuState) {
	learned, issued := e.TakeInterfaceModuleLearned(), e.TakeSessionIssued()
	if e.TakeZoneStateChanged() {
		d.unsaved = true
	}
	if e.TakeDeliveredChanged() {
		d.unsaved = true
	}
	if e.TakeFwStateChanged() {
		d.unsaved = true
	}
	if learned || issued {
		d.saveEngineState(context.Background(), e, "interface module or session id")
		return
	}
	d.flushUnsaved(context.Background(), e)
}

// flushUnsaved saves an owed change once zoneStateSaveEvery has passed since
// the last save of the engine.
func (d *daemon) flushUnsaved(ctx context.Context, e *tado.EmuState) {
	if d.unsaved && d.retryNow().Sub(d.savedAt) >= zoneStateSaveEvery {
		d.saveEngineState(ctx, e, "zone state")
	}
}

// saveEngineState saves the whole desired state from the engine goroutine. A
// failed save stays owed, so the tick retries it.
func (d *daemon) saveEngineState(ctx context.Context, e *tado.EmuState, what string) {
	d.saveSeq++
	d.savedAt = d.retryNow()
	d.unsaved = false
	if err := d.saveDesiredStateSnapshot(ctx, e.ExportDesiredState(), d.saveSeq); err != nil {
		d.unsaved = true
		log.Printf("persist %s: %v", what, err)
	}
}

func (d *daemon) persist(err error) error {
	if err != nil || d.state == nil {
		return err
	}
	if err := d.saveDesiredState(); err != nil {
		return &stateSaveError{err: err}
	}
	return nil
}

// --- radios ---

func newLiveRadio(store *stateStore, port string, coordinator []byte, phase uint16, autoAck, tuneDebug bool) (*radio.Live, error) {
	br, err := cc1101.Open(port)
	if err != nil {
		return nil, err
	}
	log.Printf("bridge: %s at %d baud", br.Banner(), br.Baud())
	if br.Baud() != cc1101.DefaultBaud {
		log.Printf("WARNING: this firmware talks %d baud, which loses replies on some USB hosts: flash firmware v2.10 or later (docs/OPERATION.md)", br.Baud())
	}
	if err := br.Configure(); err != nil {
		br.Close()
		return nil, err
	}
	r, err := startSeededRadio(store, br, coordinator, phase, autoAck, tuneDebug)
	if err != nil {
		br.Close()
		return nil, err
	}
	return r, nil
}

// startSeededRadio seeds FSCTRL0 from the store, then starts the receive loop on
// that register value.
func startSeededRadio(store *stateStore, br radio.SeededBridge, coordinator []byte, phase uint16, autoAck, tuneDebug bool) (*radio.Live, error) {
	seedFSCTRL0(store, br)
	return radio.StartSeeded(br, coordinator, phase, autoAck, tuneDebug)
}

// seedFSCTRL0 writes the passive calibration stored by `freedo-dev autotune`
// into FSCTRL0. It is the only FSCTRL0 write of the daemon: as in the Tado
// firmware, the register holds for the whole session and the hardware FOC
// absorbs the offset of each transmitter. A seed the radio refuses is a log
// line, never a reason to refuse to start.
func seedFSCTRL0(store *stateStore, br radio.TunableBridge) {
	if store == nil {
		return
	}
	switch seed, ok, err := store.FSCTRL0(context.Background()); {
	case err != nil:
		fmt.Fprintf(os.Stderr, "stored FSCTRL0: %v\n", err)
	case ok:
		if err := br.SetFSCTRL0(seed); err != nil {
			fmt.Fprintf(os.Stderr, "seed FSCTRL0=%#02x: %v\n", seed, err)
		}
	}
}

// setCalibration records whether the database holds a passive receiver
// calibration, and the port that stores one. It reports the seed the radio took
// at start: a calibration stored while the daemon runs reaches the receiver at
// the next start, so a later read would claim an offset the chip does not hold.
func (d *daemon) setCalibration(store *stateStore, port string) {
	d.calPort = port
	if store == nil {
		return
	}
	_, ok, err := store.FSCTRL0(context.Background())
	d.calibrated = err == nil && ok
}

// calibrationWarning names the command that stores a passive calibration. The
// daemon holds the register it seeds, so a database with no calibration
// leaves the receiver on the register default for the whole session.
func calibrationWarning(port string) string {
	return fmt.Sprintf("no receiver calibration stored: the receiver runs at the FSCTRL0 default. "+
		"run 'freedo-dev autotune -p %s' to store one", port)
}

// --- HTTP ---

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// write answers a read handler; a stopped engine is the only failure mode.
func write(w http.ResponseWriter, v any, err error) {
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// writeHandlerErr maps a handler error to its status: a stopped or busy engine
// is unavailable, a failed save is internal, a rejected request is a bad
// request. It reports whether it wrote an answer.
func writeHandlerErr(w http.ResponseWriter, err error) bool {
	var stateErr *stateSaveError
	switch {
	case errors.Is(err, errStopped), errors.Is(err, errEngineBusy):
		writeErr(w, http.StatusServiceUnavailable, err.Error())
	case errors.As(err, &stateErr):
		writeErr(w, http.StatusInternalServerError, stateErr.Error())
	case err != nil:
		writeErr(w, http.StatusBadRequest, err.Error())
	default:
		return false
	}
	return true
}

// queued answers an enqueue handler. extra merges endpoint-specific caveats.
func queued(w http.ResponseWriter, pending any, err error, extra map[string]any) {
	if writeHandlerErr(w, err) {
		return
	}
	body := map[string]any{"queued": true, "pending": pending}
	for k, v := range extra {
		body[k] = v
	}
	writeJSON(w, http.StatusOK, body)
}

// pathNode resolves the {id} path value against the installation. It returns
// the device and its wire address, which is what the emulator is addressed
// with: a room label shared by two valves must not reach the engine.
func (d *daemon) pathNode(w http.ResponseWriter, r *http.Request) (*tado.Device, string, bool) {
	dev, err := d.resolve(r.PathValue("id"))
	if err != nil {
		writeResolveErr(w, err)
		return nil, "", false
	}
	return dev, hex.EncodeToString(dev.WireEUI()), true
}

func (d *daemon) resolve(ref string) (*tado.Device, error) {
	inst := d.inst.Load()
	if inst == nil {
		return nil, errNoInstall
	}
	return inst.Resolve(ref)
}

// writeResolveErr maps a failed lookup to its status: a shared room label is a
// bad request naming the candidates, not a 404 and never a silent first match.
func writeResolveErr(w http.ResponseWriter, err error) {
	var amb *tado.AmbiguousRefError
	switch {
	case errors.Is(err, errNoInstall):
		writeErr(w, http.StatusServiceUnavailable, err.Error()+": seed the database or pair a device")
	case errors.Is(err, errNoDHW), errors.Is(err, errNoBoiler):
		writeErr(w, http.StatusServiceUnavailable, err.Error())
	case errors.As(err, &amb):
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": amb.Error(), "candidates": amb.Candidates,
		})
	default:
		writeErr(w, http.StatusNotFound, errNoNode.Error())
	}
}

// apiMaxBody bounds the JSON body of an API request. The debug and firmware
// endpoints take larger ones and pass their own cap to decodeBodyLimit.
const apiMaxBody = 64 << 10

// decodeBody decodes the JSON body of r into v, within apiMaxBody. An empty
// body leaves v as it is.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	return decodeBodyLimit(w, r, v, apiMaxBody)
}

// decodeBodyLimit decodes one JSON value of at most limit bytes into v. A field
// v does not declare and anything after the value but white space are errors:
// a misspelt field would otherwise be dropped in silence and the request run
// with its default.
func decodeBodyLimit(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if errors.Is(err, io.EOF) {
		return true
	}
	var tooLarge *http.MaxBytesError
	if err == nil {
		if err = dec.Decode(&json.RawMessage{}); errors.Is(err, io.EOF) {
			return true
		}
		if !errors.As(err, &tooLarge) {
			err = errors.New("the body must hold one JSON value")
		}
	}
	if errors.As(err, &tooLarge) {
		writeErr(w, http.StatusRequestEntityTooLarge,
			"body is longer than "+strconv.FormatInt(tooLarge.Limit, 10)+" bytes")
		return false
	}
	writeErr(w, http.StatusBadRequest, "bad body: "+err.Error())
	return false
}

// apiReadHeaderTimeout bounds how long a client may take to send its request
// headers, so an idle or slow client cannot hold a connection open.
const apiReadHeaderTimeout = 10 * time.Second

// newAPIServer is the HTTP server of the API.
func newAPIServer(addr string, h http.Handler) *http.Server {
	return &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: apiReadHeaderTimeout}
}

// overlayOf maps an API mode name to the on-air overlay byte.
func overlayOf(mode string) (byte, bool) {
	switch mode {
	case "auto":
		return 0, true
	case "manual", "":
		return 2, true
	case "until_next":
		return 3, true
	}
	return 0, false
}

// resolveID picks the CoAP logical id from an explicit body value, learned
// uplink state, the device declaration, or one unambiguous zone membership.
func resolveID(e *tado.EmuState, inst *tado.Install, short string, want *int) (int, error) {
	if want != nil {
		return *want, nil
	}
	for _, n := range e.Nodes() {
		if n.Short == short && n.LastID != 0 {
			return n.LastID, nil
		}
	}
	if inst == nil {
		return 0, errors.New("id required")
	}
	dev, err := inst.Resolve(short)
	if err != nil {
		return 0, err
	}
	if dev.Zone != nil {
		return *dev.Zone, nil
	}
	zones := make([]int, 0, len(inst.Zones))
	for i := range inst.Zones {
		zone := inst.Zones[i]
		members, err := inst.ZoneMembers(zone.Number)
		if err != nil {
			return 0, err
		}
		for _, member := range members {
			if member.Short() == dev.Short() {
				zones = append(zones, zone.Number)
				break
			}
		}
	}
	switch len(zones) {
	case 1:
		return zones[0], nil
	case 0:
		return 0, errors.New("id required")
	}
	ids := make([]string, len(zones))
	for i, zone := range zones {
		ids[i] = strconv.Itoa(zone)
	}
	return 0, fmt.Errorf("id ambiguous: device belongs to zones %s", strings.Join(ids, ", "))
}

type idBody struct {
	ID *int `json:"id"`
}

func (d *daemon) mux() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("GET /api/health", d.getHealth)
	// The debug endpoints decrypt caller-supplied frames and stream live traffic,
	// so they stay off by default.
	if d.debugAPI {
		m.HandleFunc("POST /api/debug/dissect", d.postDebugDissect)
		m.HandleFunc("POST /api/debug/decrypt", d.postDebugDecrypt)
		m.HandleFunc("GET /api/debug/rx", d.getDebugRx)
	}
	m.HandleFunc("GET /api/nodes", d.getNodes)
	m.HandleFunc("GET /api/nodes/{id}", d.getNode)
	m.HandleFunc("GET /api/boiler", d.getBoiler)
	m.HandleFunc("GET /api/circuits", d.getCircuits)
	m.HandleFunc("GET /api/stats", d.getStats)
	m.HandleFunc("GET /api/home", d.getHome)
	m.HandleFunc("PATCH /api/home", d.patchHome)
	m.HandleFunc("GET /api/zones", d.getZones)
	m.HandleFunc("POST /api/zones/{number}/offset", d.postZoneOffset)
	m.HandleFunc("GET /api/zones/{number}/schedule", d.getZoneSchedule)
	m.HandleFunc("PUT /api/zones/{number}/schedule", d.putZoneSchedule)
	m.HandleFunc("DELETE /api/zones/{number}/schedule", d.deleteZoneSchedule)
	m.HandleFunc("GET /api/presets", d.getPresets)
	m.HandleFunc("POST /api/presets", d.postPreset)
	m.HandleFunc("PATCH /api/presets/{id}", d.patchPreset)
	m.HandleFunc("DELETE /api/presets/{id}", d.deletePreset)
	m.HandleFunc("POST /api/presets/{id}/activate", d.activatePreset)
	m.HandleFunc("GET /api/presets/{id}/schedules", d.getPresetSchedules)
	m.HandleFunc("POST /api/zones/{number}/window", d.postZoneWindow)
	m.HandleFunc("POST /api/zones/{number}/temperature", d.postZoneTemperature)
	m.HandleFunc("DELETE /api/zones/{number}/temperature", d.deleteZoneTemperature)
	m.HandleFunc("POST /api/zones/{number}/setpoint", d.postZoneSetpoint)
	m.HandleFunc("POST /api/zones/{number}/auto", d.postZoneAuto)
	m.HandleFunc("POST /api/zones/{number}/off", d.postZoneOff)
	m.HandleFunc("POST /api/zones/{number}/boost", d.postZoneBoost)
	m.HandleFunc("GET /api/early-start", d.getEarlyStarts)
	m.HandleFunc("GET /api/zones/{number}/early-start", d.getEarlyStart)
	m.HandleFunc("PUT /api/zones/{number}/early-start", d.putEarlyStart)
	m.HandleFunc("DELETE /api/zones/{number}/early-start/model", d.deleteEarlyStartModel)
	m.HandleFunc("GET /api/zones/{number}/episodes", d.getPlantEpisodes)
	m.HandleFunc("GET /api/zones/{number}/pid-suggestion", d.getPIDSuggestion)
	m.HandleFunc("GET /api/unhandled", d.getUnhandled)
	m.HandleFunc("GET /api/queue/{id}", d.getQueue)
	m.HandleFunc("DELETE /api/queue/{id}", d.deleteQueue)
	m.HandleFunc("POST /api/nodes/{id}/setpoint", d.postSetpoint)
	m.HandleFunc("POST /api/nodes/{id}/mode", d.postMode)
	m.HandleFunc("POST /api/nodes/{id}/overlay", d.postMode) // alias of mode
	m.HandleFunc("POST /api/nodes/{id}/openwindow", d.postOpenWindow)
	m.HandleFunc("POST /api/nodes/{id}/ow", d.postOpenWindow) // alias of openwindow
	m.HandleFunc("POST /api/nodes/{id}/boost", d.postBoost)
	m.HandleFunc("POST /api/nodes/{id}/childlock", d.postChildLock)
	m.HandleFunc("GET /api/nodes/{id}/display", d.getDisplay)
	m.HandleFunc("PUT /api/nodes/{id}/display", d.putDisplay)
	m.HandleFunc("POST /api/nodes/{id}/identify", d.postIdentify)
	m.HandleFunc("POST /api/nodes/{id}/beep", d.postIdentify) // alias of identify
	m.HandleFunc("POST /api/nodes/{id}/calibrate", d.postCalibrate)
	m.HandleFunc("POST /api/nodes/{id}/reboot", d.postReboot)
	m.HandleFunc("POST /api/nodes/{id}/extui", d.postExtui)
	m.HandleFunc("POST /api/nodes/{id}/config", d.postConfig)
	m.HandleFunc("DELETE /api/nodes/{id}/config/offlineScheduleEnabled", d.deleteOfflineSchedule)
	m.HandleFunc("PUT /api/nodes/{id}/schedule", d.putSchedule)
	m.HandleFunc("POST /api/boiler/dhw", d.postBoilerDHW)
	m.HandleFunc("POST /api/boiler/flow", d.postBoilerFlow)
	m.HandleFunc("POST /api/boiler/gas/meter", d.postGasMeter)
	m.HandleFunc("DELETE /api/boiler/gas/meter", d.deleteGasMeter)
	m.HandleFunc("GET /api/history", d.getHistory)
	m.HandleFunc("PUT /api/history", d.putHistory)
	m.HandleFunc("GET /api/history/zones/{number}", d.getHistoryZone)
	m.HandleFunc("GET /api/history/energy", d.getHistoryEnergy)
	m.HandleFunc("POST /api/outdoor", d.postOutdoor)
	m.HandleFunc("POST /api/pair", d.postPair)
	m.HandleFunc("POST /api/pair/devices/{ref}", d.postPairDevice)
	m.HandleFunc("GET /api/pair/window", d.getPairWindow)
	m.HandleFunc("POST /api/pair/window", d.postPairWindow)
	// Active-GET returns the correlated response, or {"queued":true}.
	m.HandleFunc("GET /api/nodes/{id}/fw", d.getNodeFw)
	m.HandleFunc("POST /api/nodes/{id}/fw", d.postNodeFw)
	m.HandleFunc("DELETE /api/nodes/{id}/fw", d.deleteNodeFw)
	m.HandleFunc("GET /api/nodes/{id}/currentlog", d.getNodeCurrentlog)
	m.HandleFunc("GET /api/nodes/{id}/dbg/mem", d.getNodeMem)
	m.HandleFunc("GET /api/nodes/{id}/resource", d.getNodeResource)
	m.HandleFunc("GET /api/heatsource/capabilities", d.getHeatsourceCaps)
	m.HandleFunc("GET /api/state/export", d.getStateExport)
	m.HandleFunc("POST /api/state/import", d.postStateImport)
	d.installRoutes(m)
	return m
}

// hotWater returns the wall thermostat wired to the boiler and the hot-water
// zone it answers for: the per-installation answer to what used to be two
// constants.
func (d *daemon) hotWater() (*tado.Device, int, error) {
	inst := d.inst.Load()
	if inst == nil {
		return nil, 0, errNoInstall
	}
	z, ok := inst.HotWaterZone()
	if !ok {
		return nil, 0, fmt.Errorf("%w: this installation declares no hot-water zone", errNoDHW)
	}
	dev, err := inst.HotWaterTarget()
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %v", errNoDHW, err)
	}
	return dev, z.Number, nil
}

// boilerThermostat returns the wall thermostat wired to the boiler: the
// hot-water thermostat, or the heat source of the first circuit when the
// installation has no hot-water zone.
func (d *daemon) boilerThermostat() (*tado.Device, error) {
	inst := d.inst.Load()
	if inst == nil {
		return nil, errNoInstall
	}
	dev, err := inst.BoilerTarget()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errNoBoiler, err)
	}
	return dev, nil
}

// targetName is how a device is reported back to the API caller.
func targetName(dev *tado.Device) string {
	if dev.Serial != "" {
		return dev.Serial
	}
	return hex.EncodeToString(dev.WireEUI())
}

// --- read handlers ---

// getHealth answers with the liveness fields it can read on its own, plus the
// engine counters when the engine answers within healthEngineBudget. A stuck
// engine gives 503 and {"ok":false,"engine":"busy"} instead of hanging, so a
// container HEALTHCHECK gets a verdict on every call. A live radio loop that
// ended on an error gives 503 and {"ok":false,"radioError":...}.
func (d *daemon) getHealth(w http.ResponseWriter, r *http.Request) {
	health, txArmed := d.healthBase()
	v, err := d.doWithin(healthEngineBudget, func(e *tado.EmuState) (any, error) {
		return d.healthEngine(e, txArmed), nil
	})
	if errors.Is(err, errEngineBusy) {
		health["ok"] = false
		health["engine"] = "busy"
		writeJSON(w, http.StatusServiceUnavailable, health)
		return
	}
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	for k, value := range v.(map[string]any) {
		health[k] = value
	}
	if _, lost := health["radioError"]; lost {
		health["ok"] = false
		writeJSON(w, http.StatusServiceUnavailable, health)
		return
	}
	writeJSON(w, http.StatusOK, health)
}

// txArmed reports whether a frame decided now reaches the air. A dry-run and a
// radio that is handing its port over are both unarmed.
func (d *daemon) txArmed() bool {
	if d.dryRun {
		return false
	}
	if radio, ok := d.radio.(liveRadioStatus); ok {
		return radio.TXArmed()
	}
	return true
}

// healthBase reads what no goroutine but this one owns. It also returns txArmed,
// which the engine half needs to decide transmitAllowed.
func (d *daemon) healthBase() (map[string]any, bool) {
	txArmed := d.txArmed()
	var radioErr error
	if radio, ok := d.radio.(liveRadioStatus); ok {
		radioErr = radio.RadioError()
	}
	unhandled := 0
	if d.ulog != nil {
		unhandled = d.ulog.Total()
	}
	health := map[string]any{
		"ok": true, "mode": d.mode, "tx": !d.dryRun,
		"version":   buildinfo.Version,
		"txArmed":   txArmed,
		"startedAt": d.started.Format(time.RFC3339),
		"uptimeSec": int(time.Since(d.started).Seconds()),
		"unhandled": unhandled,
		"rxQueue":   len(d.radio.Frames()),
		"taps":      d.tapCount(),
	}
	if radioErr != nil {
		health["radioError"] = radioErr.Error()
	}
	if radio, ok := d.radio.(liveRadioRegister); ok {
		health["fsctrl0"] = fmt.Sprintf("%#02x", radio.FSCTRL0())
	}
	if radio, ok := d.radio.(liveSerialErrors); ok {
		health["serialErrors"] = radio.SerialErrors()
	}
	// calibrated says the register came from a passive measurement. Only a live
	// radio has one.
	if d.mode == "live" {
		health["calibrated"] = d.calibrated
		if !d.calibrated {
			health["warning"] = calibrationWarning(d.calPort)
		}
	}
	return health, txArmed
}

// healthEngine reads the engine counters. It stays off Snapshot(), which copies
// the whole derived state for every call.
func (d *daemon) healthEngine(e *tado.EmuState, txArmed bool) map[string]any {
	stats := e.Stats()
	bridgePresent := e.BridgeSeen()
	pending, awaiting := 0, 0
	if inst := d.inst.Load(); inst != nil {
		for i := range inst.Devices {
			target := hex.EncodeToString(inst.Devices[i].WireEUI())
			pending += e.Pending(target)
			if e.QueuedDownlinkAwaitingCSL(target) {
				awaiting++
			}
		}
	}
	health := map[string]any{
		"away":                        d.away,
		"windows":                     d.windowViews(),
		"bridgePresent":               bridgePresent,
		"transmitAllowed":             txArmed && !bridgePresent,
		"frames":                      stats.Frames,
		"micFails":                    stats.MICFails,
		"routerAdvertsSent":           stats.RouterAdvertisementsSent,
		"routerSolicitationsAnswered": stats.RouterSolicitationsAnswered,
		"downlink": map[string]any{
			"sent":      stats.DownlinksSent,
			"delivered": stats.DownlinksDelivered,
			"dropped":   stats.DownlinksDropped,
		},
		"queue": map[string]any{
			"pending":     pending,
			"awaitingCSL": awaiting,
			"flowPending": e.FlowPending(),
			"zoneProxies": e.PendingZoneProxies(),
		},
	}
	// A thermostat listed here is served its d/config without tag 015d until it reports one.
	if unknown := e.UnknownInterfaceModules(); len(unknown) > 0 {
		health["interfaceModuleUnknown"] = unknown
	}
	// A refused id is not learned; a stub or relay id is learned with a warning.
	if issues := e.InterfaceModuleIssues(); len(issues) > 0 {
		health["interfaceModuleIssues"] = issues
	}
	health["devices"], health["alerts"] = d.deviceHealth(e)
	health["zoneFallback"] = zoneFallbacks(e)
	if inst := d.inst.Load(); inst != nil {
		health["timezone"] = inst.Options.UTCZone(d.retryNow())
	}
	return health
}

// utcZoneWarning says that the devices get an offset of 0 because the host runs
// in UTC and the installation names no zone and no offset. It is empty
// otherwise.
func utcZoneWarning(o tado.Options, now time.Time) string {
	if z := o.UTCZone(now); !z.Fallback || z.OffsetMin != 0 || z.Name != "UTC" {
		return ""
	}
	return "options.timezone is unset and the host zone is UTC: the devices get a UTC offset of 0. " +
		"Set options.timezone (PATCH /api/install) or options.utc_offset_min, or run the daemon with TZ set."
}

// deviceHealth lists every declared device with its alert flags, and the
// devices that raise at least one alert.
func (d *daemon) deviceHealth(e *tado.EmuState) ([]map[string]any, []map[string]any) {
	inst := d.inst.Load()
	if inst == nil {
		return []map[string]any{}, []map[string]any{}
	}
	learned := map[string]tado.ValveView{}
	for _, vv := range e.NodeViews() {
		learned[vv.Short] = vv
	}
	now := d.retryNow()
	devices := make([]map[string]any, 0, len(inst.Devices))
	alerts := []map[string]any{}
	for i := range inst.Devices {
		dev := &inst.Devices[i]
		vv := learned[dev.Short()]
		d.applyAlerts(&vv, dev.Kind, inst.Options, now)
		var lastSeen any
		if vv.LastSeenUnix != 0 {
			lastSeen = time.Unix(vv.LastSeenUnix, 0).UTC().Format(time.RFC3339)
		}
		name := targetName(dev)
		devices = append(devices, map[string]any{
			"device": name, "short": dev.Short(), "kind": string(dev.Kind), "label": dev.Label,
			"batteryMV": vv.BatteryMV, "lowBattery": vv.LowBattery,
			"fault": vv.Fault, "faultWord": vv.FaultWord, "faults": vv.Faults,
			"deviceWarnings": vv.DeviceWarnings, "commsWarnings": vv.CommsWarnings,
			"offline": vv.Offline, "lastSeen": lastSeen,
			"fallbackMode": vv.FallbackMode,
		})
		var raised []string
		if vv.LowBattery {
			raised = append(raised, "lowBattery")
		}
		if vv.Fault {
			raised = append(raised, "fault")
		}
		if vv.Offline {
			raised = append(raised, "offline")
		}
		if len(raised) > 0 {
			alerts = append(alerts, map[string]any{
				"device": name, "short": dev.Short(), "label": dev.Label, "alerts": raised,
			})
		}
	}
	return devices, alerts
}

// applyAlerts sets the fault, warning and offline flags of one device; the
// engine rates the battery. Only the fault names of the fault word set Fault:
// a weak radio link and a valve state such as not mounted are warnings.
// Offline comes from the installation thresholds. A device never heard
// since the start counts its silence from the start, so a restart does not flag
// every device at once. A bridge raises none: it must stay off the air while
// freedo coordinates.
func (d *daemon) applyAlerts(vv *tado.ValveView, kind tado.DeviceKind, opts tado.Options, now time.Time) {
	if kind == tado.KindBridge {
		return
	}
	st := tado.DecodeFaultWord(vv.FaultWord)
	vv.Fault, vv.Faults, vv.DeviceWarnings, vv.CommsWarnings = st.Fault(), st.Faults, st.Warnings, st.CommsWarnings
	seen := d.started
	if vv.LastSeenUnix != 0 {
		seen = time.Unix(vv.LastSeenUnix, 0)
	}
	vv.Offline = now.Sub(seen) > opts.OfflineAfter()
}

// zoneFallbacks lists the zones the wall thermostat reports in fallback.
func zoneFallbacks(e *tado.EmuState) []map[string]any {
	out := []map[string]any{}
	for _, z := range e.ZoneFallbacks() {
		if !tado.FallbackActive(z.Mode) {
			continue
		}
		out = append(out, map[string]any{
			"zone": z.Zone, "mode": z.Mode, "name": tado.FallbackName(z.Mode),
			"source": z.Source, "since": z.Since.UTC().Format(time.RFC3339),
		})
	}
	return out
}

// logZoneStateReports logs the zone state tags of every z/s member report.
func (d *daemon) logZoneStateReports(e *tado.EmuState) {
	for _, r := range e.TakeZoneStateReports() {
		note := ""
		if r.Stale {
			note = " (stale echo of a replaced write, not taken)"
		}
		log.Printf("zone %d z/s from %s: %s%s", r.Zone, r.Device, tado.ZoneStateSummary(r.Body), note)
	}
}

// logFallbackEvents logs each zone or device entering, changing or leaving a
// fallback mode. The device mode of a valve is no fallback: it says whether
// the control of its zone is present.
func (d *daemon) logFallbackEvents(e *tado.EmuState) {
	inst := d.inst.Load()
	for _, ev := range e.TakeFallbackEvents() {
		what := "device " + ev.Device
		if ev.Zone >= 0 {
			what = fmt.Sprintf("zone %d (reported by %s)", ev.Zone, ev.Device)
		} else if inst != nil {
			if dev, err := inst.Resolve(ev.Device); err == nil && dev.Kind == tado.KindValve {
				log.Printf("valve %s reports zone control mode %d (0 present, 1 absent)", ev.Device, ev.To)
				continue
			}
		}
		switch {
		case !tado.FallbackActive(ev.From):
			log.Printf("%s entered fallback %d (%s)", what, ev.To, tado.FallbackName(ev.To))
		case !tado.FallbackActive(ev.To):
			log.Printf("%s left fallback %d (%s)", what, ev.From, tado.FallbackName(ev.From))
		default:
			log.Printf("%s changed fallback %d (%s) -> %d (%s)", what,
				ev.From, tado.FallbackName(ev.From), ev.To, tado.FallbackName(ev.To))
		}
	}
}

// getNodes lists the installation's devices, each carrying whatever the engine
// has learned about it. A device that has not been heard yet reads as its
// zero values instead of being absent.
func (d *daemon) getNodes(w http.ResponseWriter, r *http.Request) {
	if d.inst.Load() == nil {
		writeResolveErr(w, errNoInstall)
		return
	}
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) { return d.nodeViews(e), nil })
	write(w, v, err)
}

func (d *daemon) nodeViews(e *tado.EmuState) []tado.ValveView {
	learned := map[string]tado.ValveView{}
	for _, vv := range e.NodeViews() {
		learned[vv.Short] = vv
	}
	inst := d.inst.Load()
	if inst == nil {
		inst = &tado.Install{}
	}
	now := d.retryNow()
	out := make([]tado.ValveView, 0, len(inst.Devices))
	for i := range inst.Devices {
		dev := &inst.Devices[i]
		vv, ok := learned[dev.Short()]
		if !ok {
			vv = tado.ValveView{Short: dev.Short()}
			if at, issued := e.IssuedSession(hex.EncodeToString(dev.WireEUI())); issued {
				vv.SessionIssued, vv.SessionIssuedUnix = true, at.Unix()
			}
		}
		vv.Serial, vv.Room = dev.Serial, dev.Label
		if dev.Kind == tado.KindValve {
			if ds, err := e.DisplaySettings(dev.Short()); err == nil {
				vv.Display = &ds
			}
			locked := e.ChildLocked(hex.EncodeToString(dev.WireEUI()))
			vv.ChildLock = &locked
		}
		d.applyAlerts(&vv, dev.Kind, inst.Options, now)
		out = append(out, vv)
		delete(learned, dev.Short())
	}
	// Anything heard that the config does not declare still shows up: an
	// undeclared node on the PAN is exactly what an operator needs to see.
	for _, vv := range learned {
		d.applyAlerts(&vv, "", inst.Options, now)
		out = append(out, vv)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Short < out[j].Short })
	return out
}

func (d *daemon) getNode(w http.ResponseWriter, r *http.Request) {
	n, _, ok := d.pathNode(w, r)
	if !ok {
		return
	}
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		for _, vv := range d.nodeViews(e) {
			if vv.Short == n.Short() {
				return vv, nil
			}
		}
		return nil, errNoNode
	})
	if errors.Is(err, errNoNode) {
		writeErr(w, http.StatusNotFound, errNoNode.Error())
		return
	}
	write(w, v, err)
}

// getBoiler adds the flow limit state and the energy estimate to the boiler view.
func (d *daemon) getBoiler(w http.ResponseWriter, r *http.Request) {
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		return struct {
			tado.BoilerView
			FlowLimit map[string]any `json:"flowLimit"`
			Energy    map[string]any `json:"energy"`
		}{e.Snapshot().Boiler, d.flowLimitView(e), d.energyView(d.retryNow())}, nil
	})
	write(w, v, err)
}

func (d *daemon) getCircuits(w http.ResponseWriter, r *http.Request) {
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) { return e.Snapshot().Circuits, nil })
	write(w, v, err)
}

func (d *daemon) getStats(w http.ResponseWriter, r *http.Request) {
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		return map[string]any{
			"stats":     e.Stats(),
			"nodes":     len(e.Nodes()),
			"unhandled": e.Snapshot().Unhandled,
		}, nil
	})
	write(w, v, err)
}

// getZones returns the zone routing table: zone number -> member short
// addresses. It is declared in the emulator config and learned from the lid=
// traffic, so it is also the way to check what the emulator has picked up. The
// "demand" block is the observed heat-demand chain, one entry per zone.
func (d *daemon) getZones(w http.ResponseWriter, r *http.Request) {
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		return map[string]any{
			"zones":    e.ZoneDirectory(),
			"declared": d.declaredZones(e),
			"demand":   d.correctedZoneViews(e.Snapshot().Zones),
		}, nil
	})
	write(w, v, err)
}

// declaredZones renders the zone map of the config: what the installation says
// a zone is, next to what the engine has learned off the air, and what the zone
// targets now.
func (d *daemon) declaredZones(e *tado.EmuState) []map[string]any {
	inst := d.inst.Load()
	if inst == nil {
		return nil
	}
	out := make([]map[string]any, 0, len(inst.Zones))
	for i := range inst.Zones {
		z := &inst.Zones[i]
		members := []string{}
		if ms, err := inst.ZoneMembers(z.Number); err == nil {
			for _, m := range ms {
				members = append(members, m.Short())
			}
		}
		zone := map[string]any{
			"number": z.Number, "name": z.Name, "kind": string(z.Kind), "members": members,
			"haArea": haAreaView(inst, z.Number),
		}
		if z.Circuit != nil {
			zone["circuit"] = *z.Circuit
		}
		if rep, err := inst.DemandReporter(z.Number); err == nil {
			zone["reporter"] = rep.Short()
		}
		if sensor, err := inst.MeasuringDevice(z.Number); err == nil {
			zone["measuringDevice"] = sensor.Short()
		}
		if z.Kind == tado.ZoneHeating {
			zone["window"] = d.windowView(z.Number)
		}
		if w, err := zoneWriter(inst, z); err == nil {
			zone["writer"] = w.Short()
		}
		zone["target"] = d.zoneTargetView(e, z)
		if z.Kind == tado.ZoneHeating {
			zone["externalTemp"] = d.roomTempView(e, z.Number)
			zone["autoOffset"] = d.autoOffsetView(inst, z.Number)
			zone["owner"] = d.zoneOwnerView(e, inst, z.Number)
		}
		out = append(out, zone)
	}
	return out
}

func (d *daemon) getUnhandled(w http.ResponseWriter, r *http.Request) {
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		return map[string]any{"counts": e.Snapshot().Unhandled}, nil
	})
	write(w, v, err)
}

func (d *daemon) getQueue(w http.ResponseWriter, r *http.Request) {
	_, target, ok := d.pathNode(w, r)
	if !ok {
		return
	}
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		queue := e.Queue(target)
		return map[string]any{"pending": len(queue), "queue": queue, "history": e.QueueHistory(target)}, nil
	})
	write(w, v, err)
}

func (d *daemon) deleteQueue(w http.ResponseWriter, r *http.Request) {
	_, target, ok := d.pathNode(w, r)
	if !ok {
		return
	}
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		return map[string]any{"dropped": e.DropQueue(target)}, nil
	})
	write(w, v, err)
}

// --- home state and weekly schedule ---

// zoneScheduledBlock returns the block zone follows now: the program in force
// of the active preset when the zone holds one, else the last 6200 the zone
// cache holds. program reports the first case.
func (d *daemon) zoneScheduledBlock(e *tado.EmuState, inst *tado.Install, zone int, now time.Time) (block scheduledBlock, program, ok bool) {
	if programs, ok := d.programInForce(zone); ok {
		block, ok := scheduledBlockAt(programs, now, d.programClock(inst))
		return block, true, ok
	}
	c, ok := e.ZoneSchedC(zone)
	return scheduledBlock{c: d.correctedC(inst, zone, c)}, false, ok
}

// nextZoneBlock returns the instant the program in force of zone moves to its
// next entry, which is how long an "until_next" overlay lasts.
func (d *daemon) nextZoneBlock(e *tado.EmuState, inst *tado.Install, zone int, now time.Time) (time.Time, bool) {
	programs, ok := d.programInForce(zone)
	if !ok {
		return time.Time{}, false
	}
	return nextScheduleChange(programs, now, d.programClock(inst))
}

// runningOverlay reports whether the node runs an overlay on zone when a new
// write reaches it: the one a write queued ahead sets, else the one it took
// last. A write that keeps one running updates it, so it carries no 6440.
func runningOverlay(e *tado.EmuState, short string, zone int) bool {
	if block, ok := e.PendingZoneBlock(short, zone); ok {
		return block.Overlay != overlayAutoMode
	}
	for _, n := range e.Nodes() {
		if n.Short == short {
			return n.LastOverlay != 0
		}
	}
	return false
}

// applyHomeState pushes the home flag, the per-zone scheduled blocks and the
// away temperatures into the emulator, so every z/s write it builds stamps the
// current state. It returns the zones that owe a write: a program zone whose
// block moved since it last took one, and in away also every zone whose away
// 6200 moved, which a change of the away program, of its presence or of the
// fallback can do with the block unchanged. In away a hot-water zone without
// an away program takes no block change: its away write carries neither tag.
// Only tickHomeState and the preset activation write them: a command between
// a block boundary and the next tick leaves the change owed to every other
// zone. The first evaluation with no stored baseline only seeds: there is no
// earlier block to move away from. A block read back from the zone cache is no
// program: the zone already holds it, and every member echo carries it back
// after a write drops it from the cache.
func (d *daemon) applyHomeState(e *tado.EmuState) []int {
	blocks := make(map[int]scheduledBlock)
	sched := make(map[int]float64)
	off := make(map[int]bool)
	away := make(map[int]float64)
	programs := make(map[int]bool)
	var zones []int
	if inst := d.inst.Load(); inst != nil {
		now := d.retryNow()
		for i := range inst.Zones {
			zone := inst.Zones[i].Number
			zones = append(zones, zone)
			_, hasAwayProgram := d.awayProgramOf(zone)
			if c, fallback := inst.Options.AwayTemperature(zone); fallback && !hasAwayProgram {
				away[zone] = d.rawSetpointC(inst, zone, c)
			}
			block, program, ok := d.zoneScheduledBlock(e, inst, zone, now)
			if !ok {
				continue
			}
			block = d.earlyBlock(e, inst, zone, block, now)
			blocks[zone], sched[zone], off[zone] = block, d.rawSetpointC(inst, zone, block.c), block.off
			programs[zone] = program
			// In away a zone with an away program follows it: the block in
			// force takes the place of the fixed away temperature, and an off
			// block writes the off of the away fallback.
			if d.away && hasAwayProgram && !block.off {
				away[zone] = d.rawSetpointC(inst, zone, block.c)
			}
		}
	}
	e.SetHomeState(!d.away, sched)
	e.SetScheduleOff(off)
	e.SetAwayTemperatures(away)
	d.zoneSched, d.awayTarget = blocks, away
	if !d.schedSeeded {
		d.schedSeeded, d.awaySeeded = true, true
		d.pushedAll()
		return nil
	}
	if !d.awaySeeded {
		// Blocks stored by a daemon that kept no away 6200: the zones took the
		// away values of today, which nothing could change while it ran.
		d.awaySeeded, d.awayPushed, d.schedUnsaved = true, maps.Clone(away), true
	}
	var changed []int
	for _, zone := range zones {
		moved := false
		if programs[zone] {
			prev, had := d.schedPushed[zone]
			moved = !had || prev != blocks[zone]
		}
		if d.away {
			if _, has := d.awayProgramOf(zone); !has && d.isHotWaterZone(zone) {
				moved = false
			}
			if d.awayPushed[zone] != away[zone] {
				moved = true
			}
		}
		if moved {
			changed = append(changed, zone)
		}
	}
	sort.Ints(changed)
	return changed
}

// pushedAll records every zone as holding the block and the away 6200 it
// follows now.
func (d *daemon) pushedAll() {
	if maps.Equal(d.schedPushed, d.zoneSched) && maps.Equal(d.awayPushed, d.awayTarget) {
		return
	}
	d.schedPushed = maps.Clone(d.zoneSched)
	d.awayPushed = maps.Clone(d.awayTarget)
	d.schedUnsaved = true
}

// markPushed records that zone took the block and the away 6200 it follows
// now: a write went to every zoneWriteTargets member and stamped it.
func (d *daemon) markPushed(zone int) {
	block, ok := d.zoneSched[zone]
	prev, had := d.schedPushed[zone]
	switch {
	case ok == had && prev == block:
	case ok:
		d.schedPushed[zone] = block
		d.schedUnsaved = true
	default:
		delete(d.schedPushed, zone)
		d.schedUnsaved = true
	}
	c, ok := d.awayTarget[zone]
	prevC, had := d.awayPushed[zone]
	switch {
	case ok == had && prevC == c:
	case ok:
		d.awayPushed[zone] = c
		d.schedUnsaved = true
	default:
		delete(d.awayPushed, zone)
		d.schedUnsaved = true
	}
}

// programWrite names one member of a zone that owes a zone rewrite: a program
// block change, the home or away switch, or a setpoint shift. schedPushed moves
// when the write is queued, and the queue is not durable, so the owed write is:
// a restart re-queues it once. The 2.xx of that write, or of any newer zone
// block write to the member, settles it.
type programWrite struct {
	zone   int
	target string
}

// oweProgramWrite records the zone rewrite just queued to target. Its mark
// names the write whose delivery settles it.
func (d *daemon) oweProgramWrite(e *tado.EmuState, zone int, target string) {
	mark, ok := e.ZoneWriteMark(target, zone)
	if !ok {
		return
	}
	w := programWrite{zone: zone, target: target}
	if _, had := d.programOwed[w]; !had {
		d.schedUnsaved = true
	}
	d.programOwed[w] = mark
}

// settleProgramWrites drops every owed write its member confirmed, and the ones
// whose target left the zone. A write restored from the database carries mark 0
// until requeueProgramWrites queues it.
func (d *daemon) settleProgramWrites(e *tado.EmuState) {
	for w, mark := range d.programOwed {
		if !d.zoneMember(w.zone, w.target) || mark != 0 && e.ZoneWriteDelivered(w.target, w.zone, mark) {
			delete(d.programOwed, w)
			d.schedUnsaved = true
		}
	}
}

// requeueProgramWrites queues again each write restored as owed and not queued
// since, once: a member that fails it stays owed until a restart or a newer
// zone write. The write stamps the home state and the block in force now.
func (d *daemon) requeueProgramWrites(e *tado.EmuState) {
	var restored []programWrite
	for w, mark := range d.programOwed {
		if mark == 0 {
			restored = append(restored, w)
		}
	}
	slices.SortFunc(restored, func(a, b programWrite) int {
		return cmp.Or(cmp.Compare(a.zone, b.zone), strings.Compare(a.target, b.target))
	})
	for _, w := range restored {
		if err := e.EnqueueZoneRewrite(w.target, w.zone); err != nil {
			log.Printf("zone %d owed rewrite on %s: %v", w.zone, w.target, err)
			delete(d.programOwed, w)
			d.schedUnsaved = true
			continue
		}
		log.Printf("zone %d owed rewrite on %s queued again", w.zone, w.target)
		d.oweProgramWrite(e, w.zone, w.target)
		d.requestQueuedDispatch(w.target)
	}
}

// saveProgramBlocks stores schedPushed, the owed zone rewrites and the early
// starts once they moved, so a restart knows which block each zone holds and
// which writes it still owes. A failed save stays owed to the next tick.
func (d *daemon) saveProgramBlocks() {
	if !d.schedUnsaved {
		return
	}
	if d.state != nil {
		owed := make([]programWrite, 0, len(d.programOwed))
		for w := range d.programOwed {
			owed = append(owed, w)
		}
		if err := d.state.SetProgramState(context.Background(), d.schedPushed, d.awayPushed, owed, d.early); err != nil {
			log.Printf("persist program blocks: %v", err)
			return
		}
	}
	d.schedUnsaved = false
}

// tickHomeState re-evaluates the weekly programs and pushes the new 6200 and
// 61e0 to the members of every zone that owes a write (applyHomeState). Away,
// a hot-water zone without an away program takes no block change: its away
// write carries neither tag, and the return home rewrites every zone with the
// block then in force. An early start due now
// goes out as a block change of its own, and the heat-up learner then sees the
// targets this tick settled on.
func (d *daemon) tickHomeState(e *tado.EmuState) {
	d.planEarlyStarts(e)
	zones := d.applyHomeState(e)
	if len(zones) > 0 {
		d.rewriteZones(e, zones, d.requestQueuedDispatch, false)
	}
	d.settleProgramWrites(e)
	d.requeueProgramWrites(e)
	d.pushedAll()
	d.saveProgramBlocks()
	d.observeHeatUps(e)
}

// restoreHomeState loads the home state the engine keeps in the database: the
// away flag, the program presets, the windows, the heat-up models, the
// adapted flow limits, the room shifts, the block each zone last took, the
// zone rewrites still owed and the early starts running. With that baseline the
// first tick writes each zone whose program moved while the daemon was down, and
// each owed write, and no other.
func (d *daemon) restoreHomeState(e *tado.EmuState) error {
	if d.state == nil {
		return nil
	}
	ctx := context.Background()
	var err error
	if d.away, err = d.state.Away(ctx); err != nil {
		return err
	}
	if err := d.restorePresets(ctx, e); err != nil {
		return err
	}
	if err := d.restoreWindows(e); err != nil {
		return err
	}
	if d.thermal, err = d.state.ThermalModels(ctx); err != nil {
		return err
	}
	if err := d.restoreFlowAdapt(); err != nil {
		return err
	}
	if err := d.restoreRoomShifts(); err != nil {
		return err
	}
	if err := d.restoreAutoOffsets(); err != nil {
		return err
	}
	blocks, stored, err := d.state.ProgramBlocks(ctx)
	if err != nil {
		return err
	}
	if stored {
		d.schedPushed, d.schedSeeded = blocks, true
	}
	awayPushed, stored, err := d.state.AwayTargets(ctx)
	if err != nil {
		return err
	}
	if stored {
		d.awayPushed, d.awaySeeded = awayPushed, true
	}
	owed, err := d.state.ProgramOwed(ctx)
	if err != nil {
		return err
	}
	for _, w := range owed {
		d.programOwed[w] = 0
	}
	d.early, err = d.state.EarlyStarts(ctx)
	return err
}

// tickUTCOffset serves tag 02b2 at the current instant: a DST transition moves
// the wall thermostat's document, and the valves take the new offset one
// spaced write at a time. It reports whether the durable state moved.
func (d *daemon) tickUTCOffset(e *tado.EmuState) bool {
	now := d.retryNow()
	changed := e.SyncUTCOffset(now)
	target := e.PushUTCOffset(now)
	if target != "" {
		d.requestQueuedDispatch(target)
	}
	return changed || target != ""
}

// tickFwRead reads d/fw/state once from each declared valve and wall thermostat
// that holds no report yet, one spaced read at a time. An unarmed radio queues
// nothing.
func (d *daemon) tickFwRead(e *tado.EmuState) {
	if target := e.ReadFirmwareState(d.retryNow()); target != "" {
		d.requestQueuedDispatch(target)
	}
}

// rewriteZones queues one z/s write per zone, to its zoneWriteTargets. The
// write keeps the overlay and the setpoint the zone already holds. switched
// marks the home or away burst and the restate of an away temperature, whose
// write carries 6440 on a room zone with no overlay when it moves the target of
// the zone; a schedule block change carries none. Each zone then holds the block it follows now.
func (d *daemon) rewriteZones(e *tado.EmuState, zones []int, dispatch func(string), switched bool) int {
	enqueue := e.EnqueueZoneRewrite
	if switched {
		enqueue = e.EnqueueZoneSwitch
	}
	written := 0
	for _, zone := range zones {
		for _, target := range d.zoneWriteTargets(e, zone) {
			if err := enqueue(target, zone); err != nil {
				log.Printf("zone %d rewrite: %v", zone, err)
				continue
			}
			if q := e.Queue(target); len(q) > 0 {
				log.Printf("zone %d queued %s", zone, q[len(q)-1].Desc)
			}
			d.oweProgramWrite(e, zone, target)
			written++
			dispatch(target)
		}
		d.markPushed(zone)
	}
	return written
}

// allZones lists the declared zone numbers in order.
func (d *daemon) allZones() []int {
	inst := d.inst.Load()
	if inst == nil {
		return nil
	}
	out := make([]int, 0, len(inst.Zones))
	for i := range inst.Zones {
		out = append(out, inst.Zones[i].Number)
	}
	sort.Ints(out)
	return out
}

// getHome returns the state of the whole home and the active preset.
func (d *daemon) getHome(w http.ResponseWriter, r *http.Request) {
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		d.ensurePresets()
		scheduled, off := d.scheduledView()
		out := map[string]any{"away": d.away, "scheduled": scheduled, "scheduledOff": off}
		if p := d.presets[d.activePreset]; p != nil {
			out["preset"] = map[string]any{"id": p.id, "name": p.name}
		}
		return out, nil
	})
	write(w, v, err)
}

// patchHome switches the whole home between home and away. The switch queues
// the captured burst: one write per zone, each keeping its own overlay.
func (d *daemon) patchHome(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Away *bool `json:"away"`
	}
	if !decodeBody(w, r, &b) {
		return
	}
	if b.Away == nil {
		writeErr(w, http.StatusBadRequest, "away is required")
		return
	}
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		// Stored on the engine goroutine, so racing requests reach the database
		// in the order the engine took them, and before the engine moves, so a
		// failed save changes nothing.
		if d.state != nil {
			if err := d.state.SetAway(context.Background(), *b.Away); err != nil {
				return nil, &stateSaveError{err: err}
			}
		}
		changed := d.away != *b.Away
		d.away = *b.Away
		d.applyHomeState(e)
		queued := 0
		if changed {
			zones := d.allZones()
			ended := d.endHomeChangeOverlays(e)
			zones = slices.DeleteFunc(zones, func(zone int) bool { return slices.Contains(ended, zone) })
			queued = d.rewriteZones(e, zones, func(target string) { d.dispatchQueued(e, target) }, true)
			// The end of an overlay already wrote the new home state.
			for _, zone := range ended {
				d.markPushed(zone)
			}
			// The owed writes reach the database before the flag: a restart
			// between the two rewrites the zones once more, never none.
			d.saveProgramBlocks()
		}
		scheduled, off := d.scheduledView()
		return map[string]any{"away": d.away, "queued": queued, "scheduled": scheduled, "scheduledOff": off}, nil
	})
	if writeHandlerErr(w, d.persist(err)) {
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// endHomeChangeOverlays ends the overlays whose rule is the next switch between
// home and away. They take the write an elapsed timer gets, built with the home
// state applyHomeState set, so their zones need no switch write. It returns
// those zones.
func (d *daemon) endHomeChangeOverlays(e *tado.EmuState) []int {
	now := d.retryNow()
	zones := e.EndHomeChangeOverlays(now)
	targets, err := e.ExpireBoosts(now)
	if err != nil {
		log.Printf("home change overlay end: %v", err)
	}
	for _, target := range targets {
		d.dispatchQueued(e, target)
	}
	return zones
}

// scheduledView renders the per-zone scheduled setpoint the writes carry, and
// per zone whether the block in force is disabled: its writes carry 61e0=0.
func (d *daemon) scheduledView() (map[string]float64, map[string]bool) {
	setpoints := make(map[string]float64, len(d.zoneSched))
	off := make(map[string]bool, len(d.zoneSched))
	for zone, block := range d.zoneSched {
		setpoints[strconv.Itoa(zone)], off[strconv.Itoa(zone)] = block.c, block.off
	}
	return setpoints, off
}

// --- write handlers ---

func (d *daemon) postSetpoint(w http.ResponseWriter, r *http.Request) {
	n, target, ok := d.pathNode(w, r)
	if !ok {
		return
	}
	var b struct {
		TempC       *float64  `json:"tempC"`
		Mode        string    `json:"mode"`
		Sched       *float64  `json:"sched"`
		OpenWindow  bool      `json:"openWindow"`
		DurationSec *int      `json:"durationSec"`
		Zone        *zoneBody `json:"zone"`
		ID          *int      `json:"id"`
	}
	if !decodeBody(w, r, &b) {
		return
	}
	overlay, ok := overlayOf(b.Mode)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad mode "+b.Mode)
		return
	}
	if b.TempC == nil {
		writeErr(w, http.StatusBadRequest, "tempC is required")
		return
	}
	sched := *b.TempC
	if b.Sched != nil {
		sched = *b.Sched
	}
	if err := errors.Join(roomSetpointErr("tempC", *b.TempC), roomSetpointErr("sched", sched)); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var ow byte
	if b.OpenWindow {
		ow = 1
	}
	sp := tado.SetpointParams{
		Setpoint:   *b.TempC,
		Sched:      sched,
		Overlay:    overlay,
		OpenWindow: ow,
	}
	if b.DurationSec != nil {
		d, err := overlaySeconds(*b.DurationSec)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		sp.Duration, sp.EndRule = d, tado.OverlayEndTimer
	}
	if err := b.Zone.apply(&sp); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	inst := d.inst.Load()
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		id, err := resolveID(e, inst, n.Short(), b.ID)
		if err != nil {
			return nil, err
		}
		sp.ID = id
		d.applyHomeState(e)
		now := d.retryNow()
		// "until_next" with no duration lasts to the next block of the program.
		if overlay == 3 && sp.Duration == 0 && inst != nil {
			if next, ok := d.nextZoneBlock(e, inst, id, now); ok && next.After(now) {
				sp.Duration, sp.EndRule = next.Sub(now), tado.OverlayEndUntilNext
			}
		}
		sp.RunningOverlay = overlay != overlayAutoMode && runningOverlay(e, n.Short(), id)
		if err := e.EnqueueSetpoint(target, sp); err != nil {
			return nil, err
		}
		d.dispatchQueued(e, target)
		return e.Pending(target), nil
	})
	queued(w, v, d.persist(err), nil)
}

// overlayAutoMode is the 6240 value of a zone that follows its program.
const overlayAutoMode = 0

// zoneFlagsBody names the bits of the zone-flags byte (6340) an operator may
// set. The circuit-binding bit is absent on purpose: it is derived from
// zone.circuit, since the firmware reads the circuit id only when that bit is
// set. The whole block is a commissioning aid and stays out of every downlink
// the daemon builds on its own; see tado.SetpointCoAP.
type zoneFlagsBody struct {
	Active  bool `json:"active"`  // the zone takes part in active-zone selection
	Primary bool `json:"primary"` // this device publishes the zone's demand
	Member  bool `json:"member"`  // plain membership, no demand reporting
}

// zoneBody is the optional "zone" block of POST /api/nodes/{id}/setpoint. Each
// member is omit-means-untouched, and the whole block is absent from a plain
// temperature set, which keeps that downlink the block the capture pins.
type zoneBody struct {
	Circuit           *int           `json:"circuit"`           // 6060, heating-circuit id
	Flags             *zoneFlagsBody `json:"flags"`             // 6340
	ControllerEnabled *bool          `json:"controllerEnabled"` // 6400
}

// apply renders the block into the three optional z/s zone bytes.
func (z *zoneBody) apply(sp *tado.SetpointParams) error {
	if z == nil {
		return nil
	}
	if z.Circuit != nil && z.Flags == nil {
		return errors.New("zone.circuit needs zone.flags: the binding bit lives in the same byte, " +
			"and a circuit id without it leaves the zone on circuit 9")
	}
	if z.Flags != nil {
		if z.Flags.Primary && z.Flags.Member {
			return errors.New("zone.flags: primary and member are exclusive, " +
				"the election skips a device with the membership bit set")
		}
		f := byte(0)
		if z.Flags.Active {
			f |= tado.ZoneFlagActive
		}
		if z.Flags.Primary {
			f |= tado.ZoneFlagPrimary
		}
		if z.Flags.Member {
			f |= tado.ZoneFlagMember
		}
		if z.Circuit != nil {
			f |= tado.ZoneFlagCircuit
		}
		sp.ZoneFlags = &f
	}
	if z.Circuit != nil {
		if *z.Circuit < 0 || *z.Circuit > 0xff {
			return errors.New("zone.circuit out of range 0..255")
		}
		c := byte(*z.Circuit)
		sp.Circuit = &c
	}
	if z.ControllerEnabled != nil {
		v := byte(1) // the device enables the zone controller on 0
		if *z.ControllerEnabled {
			v = 0
		}
		sp.CtrlEnable = &v
	}
	return nil
}

// postBoilerDHW queues the hot-water setpoint on the wall thermostat. The
// target is fixed: only that node drives the boiler.
func (d *daemon) postBoilerDHW(w http.ResponseWriter, r *http.Request) {
	var b struct {
		TempC       *float64 `json:"tempC"`
		ID          *int     `json:"id"`
		On          *bool    `json:"on"`
		DurationSec *int     `json:"durationSec"`
	}
	if !decodeBody(w, r, &b) {
		return
	}
	dev, zone, err := d.hotWater()
	if err != nil {
		writeResolveErr(w, err)
		return
	}
	if b.ID != nil && *b.ID != zone {
		writeErr(w, http.StatusBadRequest, "id must match hot-water zone "+strconv.Itoa(zone))
		return
	}
	id := zone
	on := true
	if b.On != nil {
		on = *b.On
	}
	if on && b.TempC == nil {
		writeErr(w, http.StatusBadRequest, "tempC is required when on is true")
		return
	}
	tempC := 0.0
	if b.TempC != nil {
		tempC = *b.TempC
	}
	duration, rule := time.Duration(0), ""
	if b.DurationSec != nil {
		if duration, err = overlaySeconds(*b.DurationSec); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		rule = tado.OverlayEndTimer
	}
	target := hex.EncodeToString(dev.WireEUI())
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		d.applyHomeState(e)
		if err := e.EnqueueDHWOverlay(target, id, tempC, on, duration, rule); err != nil {
			return nil, err
		}
		d.dispatchQueued(e, target)
		return e.Pending(target), nil
	})
	queued(w, v, d.persist(err), map[string]any{"target": targetName(dev), "id": id, "on": on})
}

// postBoilerFlow registers the CH flow setpoint the
// emulator serves on the wall thermostat's circuit-config poll. Delivery is a
// pull: the value reaches the boiler when the thermostat next reads
// "h/<home>/c/<id>/config" and applies it as the OpenTherm CH setpoint.
func (d *daemon) postBoilerFlow(w http.ResponseWriter, r *http.Request) {
	var b struct {
		TempC  float64 `json:"tempC"`
		ID     *int    `json:"id"`
		Target string  `json:"target"`
	}
	if !decodeBody(w, r, &b) {
		return
	}
	var dev *tado.Device
	var err error
	if b.Target == "" {
		dev, err = d.boilerThermostat()
	} else {
		dev, err = d.resolve(b.Target)
	}
	if err != nil {
		writeResolveErr(w, err)
		return
	}
	id, err := d.flowCircuit(dev, b.ID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, ok := d.inst.Load().Options.EnabledFlowCurve(id); ok {
		writeErr(w, http.StatusConflict, fmt.Sprintf("circuit %d follows its weather curve: disable options.flow_curves.%d first", id, id))
		return
	}
	if d.inst.Load().Options.FlowAutoAdaptEnabled() {
		writeErr(w, http.StatusConflict, fmt.Sprintf("circuit %d adapts its flow limit: set flow_limit_c, or disable flow_auto_adapt first", id))
		return
	}
	if flowDefaultCurve(d.inst.Load().Options, id) {
		writeErr(w, http.StatusConflict, fmt.Sprintf("circuit %d follows the default weather curve: set flow_limit_c, or disable flow_default_curve first", id))
		return
	}
	target := hex.EncodeToString(dev.WireEUI())
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		if err := e.EnqueueFlowSetpoint(target, id, b.TempC); err != nil {
			return nil, err
		}
		return e.FlowPending(), nil
	})
	queued(w, v, d.persist(err), map[string]any{
		"target": targetName(dev), "id": id,
		"note": "served on the wall thermostat's next c/<id>/config poll",
	})
}

// flowCircuit picks the heating circuit of a CH flow setpoint: the body value,
// which the installation must declare, else the single circuit this thermostat
// is the heat source of.
func (d *daemon) flowCircuit(dev *tado.Device, want *int) (int, error) {
	inst := d.inst.Load()
	if inst == nil {
		return 0, errNoInstall
	}
	if want != nil {
		if _, err := inst.CircuitByID(*want); err != nil {
			return 0, fmt.Errorf("the installation declares no circuit %d", *want)
		}
		return *want, nil
	}
	cs := inst.CircuitsFor(dev)
	switch len(cs) {
	case 1:
		return cs[0].ID, nil
	case 0:
		return 0, fmt.Errorf("%s is the heat source of no circuit: pass \"id\"", dev.Ref())
	}
	ids := make([]string, len(cs))
	for i, c := range cs {
		ids[i] = strconv.Itoa(c.ID)
	}
	return 0, fmt.Errorf("%s drives circuits %s: pass \"id\"", dev.Ref(), strings.Join(ids, ", "))
}

func (d *daemon) postMode(w http.ResponseWriter, r *http.Request) {
	n, target, ok := d.pathNode(w, r)
	if !ok {
		return
	}
	var b struct {
		Mode string `json:"mode"`
		ID   *int   `json:"id"`
	}
	if !decodeBody(w, r, &b) {
		return
	}
	overlay, ok := overlayOf(b.Mode)
	if !ok || b.Mode == "" {
		writeErr(w, http.StatusBadRequest, "bad mode "+b.Mode)
		return
	}
	inst := d.inst.Load()
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		id, err := resolveID(e, inst, n.Short(), b.ID)
		if err != nil {
			return nil, err
		}
		d.applyHomeState(e)
		if err := e.EnqueueMode(target, id, overlay); err != nil {
			return nil, err
		}
		d.dispatchQueued(e, target)
		return e.Pending(target), nil
	})
	queued(w, v, d.persist(err), nil)
}

func (d *daemon) postOpenWindow(w http.ResponseWriter, r *http.Request) {
	n, target, ok := d.pathNode(w, r)
	if !ok {
		return
	}
	var b struct {
		Open bool `json:"open"`
		ID   *int `json:"id"`
	}
	if !decodeBody(w, r, &b) {
		return
	}
	inst := d.inst.Load()
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		id, err := resolveID(e, inst, n.Short(), b.ID)
		if err != nil {
			return nil, err
		}
		d.applyHomeState(e)
		if err := e.EnqueueOpenWindow(target, id, b.Open); err != nil {
			return nil, err
		}
		d.dispatchQueued(e, target)
		return e.Pending(target), nil
	})
	queued(w, v, d.persist(err), nil)
}

func (d *daemon) postBoost(w http.ResponseWriter, r *http.Request) {
	n, target, ok := d.pathNode(w, r)
	if !ok {
		return
	}
	var b struct {
		On bool `json:"on"`
		ID *int `json:"id"`
	}
	if !decodeBody(w, r, &b) {
		return
	}
	inst := d.inst.Load()
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		id, err := resolveID(e, inst, n.Short(), b.ID)
		if err != nil {
			return nil, err
		}
		zone, err := inst.ZoneByNumber(id)
		if err != nil {
			return nil, err
		}
		if zone.Kind != tado.ZoneHeating {
			return nil, errors.New("boost requires a heating zone")
		}
		d.applyHomeState(e)
		if err := e.EnqueueBoost(target, id, b.On); err != nil {
			return nil, err
		}
		d.dispatchQueued(e, target)
		return e.Pending(target), nil
	})
	queued(w, v, d.persist(err), nil)
}

func (d *daemon) postChildLock(w http.ResponseWriter, r *http.Request) {
	n, target, ok := d.pathNode(w, r)
	if !ok {
		return
	}
	if n.Kind != tado.KindValve {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("%s is a %s: only a valve has a child lock (d/lock)", n.Ref(), n.Kind))
		return
	}
	var b struct {
		On bool `json:"on"`
		ID *int `json:"id"`
	}
	if !decodeBody(w, r, &b) {
		return
	}
	inst := d.inst.Load()
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		id, err := resolveID(e, inst, n.Short(), b.ID)
		if err != nil {
			return nil, err
		}
		if err := e.EnqueueChildLock(target, id, b.On); err != nil {
			return nil, err
		}
		d.dispatchQueued(e, target)
		return e.Pending(target), nil
	})
	queued(w, v, d.persist(err), nil)
}

// getDisplay reads a valve's display settings as its served d/config carries them.
func (d *daemon) getDisplay(w http.ResponseWriter, r *http.Request) {
	_, target, ok := d.pathNode(w, r)
	if !ok {
		return
	}
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) { return e.DisplaySettings(target) })
	if writeHandlerErr(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// putDisplay changes a valve's display settings. An omitted field keeps its
// value. The write goes through the whole d/config write, as the commercial
// bridge does, unless "via" asks for "dbg2".
func (d *daemon) putDisplay(w http.ResponseWriter, r *http.Request) {
	n, target, ok := d.pathNode(w, r)
	if !ok {
		return
	}
	var b struct {
		Orientation  *string `json:"orientation"`
		Notification *bool   `json:"notification"`
		Via          string  `json:"via"`
		ID           *int    `json:"id"`
	}
	if !decodeBody(w, r, &b) {
		return
	}
	inst := d.inst.Load()
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		id, err := resolveID(e, inst, n.Short(), b.ID)
		if err != nil {
			return nil, err
		}
		dw := tado.DisplaySettingsWrite{Orientation: b.Orientation, Notification: b.Notification, Via: b.Via}
		if err := e.EnqueueDisplaySettings(target, id, dw); err != nil {
			return nil, err
		}
		d.dispatchQueued(e, target)
		display, err := e.DisplaySettings(target)
		if err != nil {
			return nil, err
		}
		return map[string]any{"queued": true, "pending": e.Pending(target), "display": display}, nil
	})
	if writeHandlerErr(w, d.persist(err)) {
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (d *daemon) postIdentify(w http.ResponseWriter, r *http.Request) {
	n, target, ok := d.pathNode(w, r)
	if !ok {
		return
	}
	var b idBody
	if !decodeBody(w, r, &b) {
		return
	}
	inst := d.inst.Load()
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		id, err := resolveID(e, inst, n.Short(), b.ID)
		if err != nil {
			return nil, err
		}
		if err := e.EnqueueIdentify(target, id); err != nil {
			return nil, err
		}
		d.dispatchQueued(e, target)
		return e.Pending(target), nil
	})
	queued(w, v, d.persist(err), nil)
}

// postCalibrate starts the motor calibration of one valve. Only the valve
// firmware serves d/calibrate.
func (d *daemon) postCalibrate(w http.ResponseWriter, r *http.Request) {
	n, target, ok := d.pathNode(w, r)
	if !ok {
		return
	}
	if n.Kind != tado.KindValve {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("%s is a %s: only a valve calibrates", n.Ref(), n.Kind))
		return
	}
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		if err := e.EnqueueCalibrate(target); err != nil {
			return nil, err
		}
		d.dispatchQueued(e, target)
		return e.Pending(target), nil
	})
	queued(w, v, d.persist(err), nil)
}

func (d *daemon) postReboot(w http.ResponseWriter, r *http.Request) {
	n, target, ok := d.pathNode(w, r)
	if !ok {
		return
	}
	var b idBody
	if !decodeBody(w, r, &b) {
		return
	}
	inst := d.inst.Load()
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		id, err := resolveID(e, inst, n.Short(), b.ID)
		if err != nil {
			return nil, err
		}
		if err := e.EnqueueReboot(target, id); err != nil {
			return nil, err
		}
		d.dispatchQueued(e, target)
		return e.Pending(target), nil
	})
	queued(w, v, d.persist(err), nil)
}

func (d *daemon) postExtui(w http.ResponseWriter, r *http.Request) {
	_, target, ok := d.pathNode(w, r)
	if !ok {
		return
	}
	var b struct {
		StateEndpoint string `json:"stateEndpoint"`
		MeasEndpoint  string `json:"measEndpoint"`
	}
	if !decodeBody(w, r, &b) {
		return
	}
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		if err := e.EnqueueExtuiPZH(target, b.StateEndpoint, b.MeasEndpoint); err != nil {
			return nil, err
		}
		d.dispatchQueued(e, target)
		return e.Pending(target), nil
	})
	queued(w, v, d.persist(err), map[string]any{"lid": "pzh"})
}

// putSchedule writes the document of one device. A member of a heating zone
// takes it as the home program of its first heating zone in the active preset,
// which every member of that zone is then served (setZoneProgram): the zone
// has one program, so a document of one member alone would leave the devices
// on a program the engine does not follow. A device in no heating zone keeps
// a document of its own.
func (d *daemon) putSchedule(w http.ResponseWriter, r *http.Request) {
	n, target, ok := d.pathNode(w, r)
	if !ok {
		return
	}
	var b struct {
		DayMask int                 `json:"dayMask"`
		Entries []scheduleEntryBody `json:"entries"`
		ID      *int                `json:"id"`
	}
	if !decodeBody(w, r, &b) {
		return
	}
	entries, err := decodeScheduleEntries(b.Entries)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	inst := d.inst.Load()
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		if _, err := resolveID(e, inst, n.Short(), b.ID); err != nil {
			return nil, err
		}
		if b.DayMask != 0x7f {
			return nil, errors.New("schedule requires dayMask 127")
		}
		programs := []tado.ScheduleProgram{{DayMask: byte(b.DayMask), Entries: entries}}
		if zone, ok := heatingZoneOf(inst, n); ok {
			if err := tado.ValidateSchedule(programs); err != nil {
				return nil, err
			}
			d.ensurePresets()
			return true, d.setZoneProgram(e, d.activePreset, zone, false, programs, true)
		}
		if err := e.SetSchedule(target, programs); err != nil {
			return nil, err
		}
		d.dispatchQueued(e, target)
		return true, nil
	})
	queued(w, v, d.persist(err), map[string]any{"note": "served on the valve's next zone schedule GET"})
}

// heatingZoneOf returns the first heating zone, in declaration order, that
// lists dev as a member.
func heatingZoneOf(inst *tado.Install, dev *tado.Device) (int, bool) {
	for i := range inst.Zones {
		z := &inst.Zones[i]
		if z.Kind != tado.ZoneHeating {
			continue
		}
		members, err := inst.ZoneMembers(z.Number)
		if err != nil {
			continue
		}
		for _, m := range members {
			if m == dev || bytes.Equal(m.WireEUI(), dev.WireEUI()) {
				return z.Number, true
			}
		}
	}
	return 0, false
}

type scheduleEntryBody struct {
	StartMin int     `json:"startMin"`
	TempC    float64 `json:"tempC"`
	Enabled  *bool   `json:"enabled"`
}

func decodeScheduleEntries(in []scheduleEntryBody) ([]tado.ScheduleEntry, error) {
	out := make([]tado.ScheduleEntry, len(in))
	for i, entry := range in {
		centi, err := centiDegrees(entry.TempC)
		if err != nil {
			return nil, fmt.Errorf("entries[%d].tempC must have at most two decimal places", i)
		}
		enabled := true
		if entry.Enabled != nil {
			enabled = *entry.Enabled
		}
		if err := roomSetpointErr(fmt.Sprintf("entries[%d].tempC", i), entry.TempC); err != nil {
			return nil, err
		}
		out[i] = tado.ScheduleEntry{Minute: entry.StartMin, SetpointCenti: centi, Enabled: enabled}
	}
	return out, nil
}

// centiDegrees converts a temperature to the centi-degrees the wire carries.
func centiDegrees(c float64) (int, error) {
	centi := c * 100
	rounded := math.Round(centi)
	if math.IsNaN(centi) || math.IsInf(centi, 0) || math.Abs(centi-rounded) > 1e-9 || rounded < -2147483648 || rounded > 2147483647 {
		return 0, errors.New("temperature must have at most two decimal places")
	}
	return int(rounded), nil
}

// nodeTableBody is one 015e node-table record of POST /api/nodes/{id}/config.
type nodeTableBody struct {
	Role uint8 `json:"role"` // role bitmask, selected on & 0x0d
	Node uint8 `json:"node"` // node/zone id, 127 = none
}

// errModuleIDField answers a config write that carries the retired
// reportIvlSec field.
const errModuleIDField = "reportIvlSec is not writable: tag 015d is the device's interface module id, " +
	"freedo holds it at 112 on every valve and serves the wall thermostat only the id it reported"

// postConfig queues a d/config store write to a valve. Every field is
// omit-means-untouched. The body is flat because the store is a flat tag space:
// the fields share nothing but the resource, so a nesting level would name a
// grouping the device does not have. The one exception is nodeTable, an array
// because tag 015e repeats and its order is what the device indexes by.
func (d *daemon) postConfig(w http.ResponseWriter, r *http.Request) {
	n, target, ok := d.pathNode(w, r)
	if !ok {
		return
	}
	var b struct {
		TempUnitF              *bool           `json:"tempUnitF"`              // 013f
		CalibOffset            *int            `json:"calibOffset"`            // 0140
		TraceEnable            *uint8          `json:"traceEnable"`            // 0143
		Orientation            *uint8          `json:"lcdVariant"`             // 0149
		ChangeNotify           *uint16         `json:"stateBits"`              // 0158
		EtagHex                string          `json:"etagHex"`                // 015a
		HomeID                 *uint32         `json:"homeId"`                 // 015c
		NodeTable              []nodeTableBody `json:"nodeTable"`              // 015e
		UTCOffsetMin           *int            `json:"utcOffsetMin"`           // 02b2
		OfflineScheduleEnabled *bool           `json:"offlineScheduleEnabled"` // 02b3
		ID                     *int            `json:"id"`
		// ModuleID only detects the retired field: tag 015d is not writable.
		ModuleID json.RawMessage `json:"reportIvlSec"`
	}
	if !decodeBody(w, r, &b) {
		return
	}
	if b.ModuleID != nil {
		writeErr(w, http.StatusBadRequest, errModuleIDField)
		return
	}
	cw := tado.ConfigWrite{
		TempUnitF:              b.TempUnitF,
		CalibOffset:            b.CalibOffset,
		TraceEnable:            b.TraceEnable,
		Orientation:            b.Orientation,
		ChangeNotify:           b.ChangeNotify,
		HomeID:                 b.HomeID,
		UTCOffsetMin:           b.UTCOffsetMin,
		OfflineScheduleEnabled: b.OfflineScheduleEnabled,
	}
	if b.EtagHex != "" {
		etag, err := hex.DecodeString(b.EtagHex)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad etagHex: "+err.Error())
			return
		}
		cw.Etag = etag
	}
	// A body that omits utcOffsetMin inherits the installation's offset now: the
	// device keeps no DST rules, so that is what it has to be told. An explicit
	// value in the body wins. The wall thermostat's served document already
	// follows the offset in force (SyncUTCOffset).
	inst := d.inst.Load()
	if cw.UTCOffsetMin == nil && inst != nil && n.Kind != tado.KindThermostat {
		off := inst.Options.UTCOffsetAt(d.retryNow())
		cw.UTCOffsetMin = &off
	}
	for _, rec := range b.NodeTable {
		cw.NodeTable = append(cw.NodeTable, tado.NodeTableRec{Role: rec.Role, Node: rec.Node})
	}
	// Validated here and not only inside the enqueue, so that a bad value
	// answers with its own message instead of whatever the id lookup says first.
	if err := cw.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	d.configWrite(w, r, target, nil, func(e *tado.EmuState) error {
		// The wall thermostat takes no write, so the id of its two zones
		// matters to nobody.
		id, err := resolveID(e, inst, n.Short(), b.ID)
		if err != nil && n.Kind != tado.KindThermostat {
			return err
		}
		return e.EnqueueConfig(target, id, cw)
	})
}

// configWrite runs a d/config write on the engine and answers how it reaches
// the device: queued, or served on the device's own next read when the device
// drops the written document (the wall thermostat). reread tells whether a
// trigger that makes that read come at once waits in the queue. extra merges
// endpoint fields.
func (d *daemon) configWrite(w http.ResponseWriter, r *http.Request, target string, extra map[string]any,
	write func(*tado.EmuState) error) {
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		if err := write(e); err != nil {
			return nil, err
		}
		body := map[string]any{"queued": true}
		if e.ServesConfigOnRead(target) {
			body["queued"], body["served"] = false, true
			body["reread"] = e.ConfigRereadQueued(target)
		}
		d.dispatchQueued(e, target)
		body["pending"] = e.Pending(target)
		for k, v := range extra {
			body[k] = v
		}
		return body, nil
	})
	if writeHandlerErr(w, d.persist(err)) {
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// deleteOfflineSchedule drops the per-device offlineScheduleEnabled (02b3), so
// the device follows options.offline_schedule again. It queues one d/config
// write when that moves the device's value and none when it does not: each
// write makes a valve home its motor.
func (d *daemon) deleteOfflineSchedule(w http.ResponseWriter, r *http.Request) {
	n, target, ok := d.pathNode(w, r)
	if !ok {
		return
	}
	inst := d.inst.Load()
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		// The wall thermostat takes no write, so the id of its two zones matters
		// to nobody.
		id, err := resolveID(e, inst, n.Short(), nil)
		if err != nil && n.Kind != tado.KindThermostat {
			return nil, err
		}
		written, err := e.ClearOfflineSchedule(target, id)
		if err != nil {
			return nil, err
		}
		if written {
			d.dispatchQueued(e, target)
		}
		return map[string]any{"queued": written, "pending": e.Pending(target)}, nil
	})
	if writeHandlerErr(w, d.persist(err)) {
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// postZoneOffset applies the room-level temperature correction to the zone's
// elected measuring device. The low-level device config endpoint remains
// available for diagnostics.
func (d *daemon) postZoneOffset(w http.ResponseWriter, r *http.Request) {
	inst := d.inst.Load()
	if inst == nil {
		writeResolveErr(w, errNoInstall)
		return
	}
	zoneNumber, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid zone number")
		return
	}
	zone, err := inst.ZoneByNumber(zoneNumber)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if zone.Kind != tado.ZoneHeating {
		writeErr(w, http.StatusBadRequest, "temperature offset requires a heating zone")
		return
	}
	var b struct {
		OffsetC *float64 `json:"offsetC"`
	}
	if !decodeBody(w, r, &b) {
		return
	}
	if b.OffsetC == nil {
		writeErr(w, http.StatusBadRequest, "offsetC is required")
		return
	}
	centi := *b.OffsetC * 100
	rounded := math.Round(centi)
	if math.IsNaN(centi) || math.IsInf(centi, 0) || math.Abs(centi-rounded) > 1e-9 {
		writeErr(w, http.StatusBadRequest, "offsetC must have at most two decimal places")
		return
	}
	offset := int(rounded)
	cw := tado.ConfigWrite{CalibOffset: &offset}
	if err := cw.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	mode := inst.Options.ZoneOffset(zoneNumber).Mode
	if mode == tado.OffsetModeSetpoint {
		d.postSetpointOffset(w, r, zoneNumber, offset)
		return
	}
	targetDevice, err := inst.MeasuringDevice(zoneNumber)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	target := hex.EncodeToString(targetDevice.WireEUI())
	extra := map[string]any{"zone": zoneNumber, "target": targetName(targetDevice), "offsetC": *b.OffsetC, "mode": mode}
	d.configWrite(w, r, target, extra, func(e *tado.EmuState) error {
		// A manual offset moves 4060 as an automatic one does: reject the
		// episodes that span it, unless the learners read the external reading.
		d.noteOffsetChange(inst, zoneNumber, offset)
		if mode == tado.OffsetModeDbg2 {
			return e.EnqueueCalibOffsetDbg2(target, zoneNumber, offset)
		}
		return e.EnqueueConfig(target, zoneNumber, cw)
	})
}

// activeNodeGet enqueues a bridge-initiated GET and returns its correlated response.
func (d *daemon) activeNodeGet(w http.ResponseWriter, r *http.Request, resource string, path []string, idScoped, refresh bool, extraQuery []string) {
	d.activeNodeGetDecoded(w, r, resource, path, idScoped, refresh, extraQuery, nil, nil)
}

// activeNodeGetDecoded is activeNodeGet that also adds a "decoded" view of a
// 2.05 answer, or a "decode_error" when the body does not decode. with, when
// set, adds its own fields to the answer.
func (d *daemon) activeNodeGetDecoded(w http.ResponseWriter, r *http.Request, resource string, path []string, idScoped, refresh bool, extraQuery []string, decode func([]byte) (any, error), with func(target string, out map[string]any)) {
	n, target, ok := d.pathNode(w, r)
	if !ok {
		return
	}
	inst := d.inst.Load()
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		query := append([]string(nil), extraQuery...)
		if idScoped {
			if id, err := resolveID(e, inst, n.Short(), nil); err == nil {
				query = append(query, "id="+strconv.Itoa(id))
			}
		}
		if !refresh {
			if code, body, ok := e.ActiveGetResponseFor(target, resource, query); ok {
				out := map[string]any{"resource": resource, "code": code, "result": hex.EncodeToString(body)}
				if decode != nil && code == "2.05" {
					if v, err := decode(body); err != nil {
						out["decode_error"] = err.Error()
					} else {
						out["decoded"] = v
					}
				}
				if with != nil {
					with(target, out)
				}
				return out, nil
			}
		}
		if err := e.EnqueueGet(target, resource, path, query); err != nil {
			return nil, err
		}
		d.dispatchQueued(e, target)
		out := map[string]any{"resource": resource, "queued": true, "pending": e.Pending(target)}
		if with != nil {
			with(target, out)
		}
		return out, nil
	})
	write(w, v, err)
}

// getNodeFw reads d/fw/state and shows the firmware transfer of the device,
// running or last finished, under "transfer".
func (d *daemon) getNodeFw(w http.ResponseWriter, r *http.Request) {
	d.activeNodeGetDecoded(w, r, "d/fw/state", []string{"d", "fw", "state"}, false, false, nil, decodeFwState, d.fwView)
}

func decodeFwState(body []byte) (any, error) {
	s, err := tado.ParseFwState(body)
	if err != nil {
		return nil, err
	}
	return s.View(), nil
}

func (d *daemon) getNodeCurrentlog(w http.ResponseWriter, r *http.Request) {
	d.activeNodeGet(w, r, "d/currentlog", []string{"d", "currentlog"}, false, false, nil)
}

// getNodeMem reads device memory: d/dbg/m?adr=&len=.
func (d *daemon) getNodeMem(w http.ResponseWriter, r *http.Request) {
	query, refresh, err := debugMemoryQuery(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	d.activeNodeGet(w, r, "d/dbg/m", []string{"d", "dbg", "m"}, false, refresh, query)
}

func debugMemoryQuery(r *http.Request) ([]string, bool, error) {
	q := r.URL.Query()
	for name, values := range q {
		if (name != "adr" && name != "len" && name != "refresh") || len(values) != 1 {
			return nil, false, errors.New("invalid dbg/mem query")
		}
	}
	adrValues, lenValues := q["adr"], q["len"]
	if len(adrValues) != 1 || len(lenValues) != 1 {
		return nil, false, errors.New("adr and len query params required")
	}
	adr := adrValues[0]
	if len(adr) == 0 || len(adr) > 8 || strings.ContainsAny(adr, "^+-") {
		return nil, false, errors.New("adr must be 1-8 hexadecimal digits without 0x")
	}
	for _, c := range adr {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return nil, false, errors.New("adr must be 1-8 hexadecimal digits without 0x")
		}
	}
	address, err := strconv.ParseUint(adr, 16, 32)
	if err != nil {
		return nil, false, errors.New("invalid adr")
	}
	ln := lenValues[0]
	if ln == "" || strings.Trim(ln, "0123456789") != "" {
		return nil, false, errors.New("len must be decimal 1-255")
	}
	length, err := strconv.ParseUint(ln, 10, 8)
	if err != nil || length == 0 {
		return nil, false, errors.New("len must be decimal 1-255")
	}
	refresh := false
	if values, ok := q["refresh"]; ok {
		if values[0] != "1" {
			return nil, false, errors.New("refresh must be 1")
		}
		refresh = true
	}
	return []string{"adr=" + strconv.FormatUint(address, 10), "len=" + strconv.FormatUint(length, 10)}, refresh, nil
}

func (d *daemon) getNodeResource(w http.ResponseWriter, r *http.Request) {
	resource, path, query, err := activeResource(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	d.activeNodeGet(w, r, resource, path, false, false, query)
}

func activeResource(r *http.Request) (string, []string, []string, error) {
	path := strings.TrimSpace(r.URL.Query().Get("path"))
	if path == "" {
		return "", nil, nil, errors.New("path query param required")
	}
	if strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/") || strings.ContainsAny(path, "?#\r\n") {
		return "", nil, nil, errors.New("bad resource path")
	}
	parts := strings.Split(path, "/")
	for _, part := range parts {
		if part == "" || strings.ContainsAny(part, "&=\x00") {
			return "", nil, nil, errors.New("bad resource path")
		}
	}
	queryText := r.URL.Query().Get("query")
	var query []string
	if queryText != "" {
		query = strings.Split(queryText, "&")
		for _, item := range query {
			if item == "" || strings.ContainsAny(item, "?#\r\n\x00") {
				return "", nil, nil, errors.New("bad resource query")
			}
		}
	}
	if path == "z/extui" && (len(query) != 1 || (query[0] != "lid=pzh" && query[0] != "lid=pzw")) {
		return "", nil, nil, errors.New("z/extui requires query lid=pzh or lid=pzw")
	}
	resource := path
	if queryText != "" {
		resource += "?" + queryText
	}
	return resource, parts, query, nil
}

// getHeatsourceCaps queries the wall thermostat's heat-source capabilities. The
// target is the boiler thermostat: only the node wired to the boiler knows
// them.
func (d *daemon) getHeatsourceCaps(w http.ResponseWriter, r *http.Request) {
	const resource = "heatsource/capabilities"
	dev, err := d.boilerThermostat()
	if err != nil {
		writeResolveErr(w, err)
		return
	}
	target := hex.EncodeToString(dev.WireEUI())
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		if code, body, ok := e.ActiveGetResponse(target, resource); ok {
			return map[string]any{"resource": resource, "code": code, "result": hex.EncodeToString(body)}, nil
		}
		if err := e.EnqueueGet(target, resource, []string{"heatsource", "capabilities"}, nil); err != nil {
			return nil, err
		}
		d.dispatchQueued(e, target)
		return map[string]any{
			"resource": resource, "target": targetName(dev), "queued": true, "pending": e.Pending(target),
		}, nil
	})
	write(w, v, err)
}

// postPair queues d/pair. An empty target names the one device the open pair
// window heard under the pairing key; a target outside the installation may
// name a device of that window by wire EUI or short address.
func (d *daemon) postPair(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Target        string `json:"target"`
		NetworkKeyHex string `json:"networkKeyHex"`
	}
	if !decodeBody(w, r, &b) {
		return
	}
	d.queuePair(w, r, b.Target, b.NetworkKeyHex)
}

// postPairDevice queues d/pair for the device the path names, under the
// network key of the installation. It takes no body, so the ingress route that
// reaches it cannot send another key.
func (d *daemon) postPairDevice(w http.ResponseWriter, r *http.Request) {
	d.queuePair(w, r, r.PathValue("ref"), "")
}

// queuePair is the shared half of the two pair routes. An empty keyHex takes
// the network key of the installation.
func (d *daemon) queuePair(w http.ResponseWriter, r *http.Request, ref, keyHex string) {
	target := ""
	if ref != "" {
		dev, err := d.resolve(ref)
		if err == nil {
			target = hex.EncodeToString(dev.WireEUI())
		} else {
			v, jerr := d.doReq(r, func(e *tado.EmuState) (any, error) {
				t, _ := e.PairJoinerTarget(ref)
				return t, nil
			})
			t, _ := v.(string)
			if jerr != nil || t == "" {
				writeResolveErr(w, err)
				return
			}
			target = t
		}
	}
	key := d.inst.Load().NetworkKey()
	if keyHex != "" {
		k, err := hex.DecodeString(keyHex)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad networkKeyHex: "+err.Error())
			return
		}
		key = k
	}
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		if target == "" {
			t, err := e.PairAutoTarget()
			if err != nil {
				return nil, err
			}
			target = t
		}
		if err := e.EnqueuePair(target, key); err != nil {
			return nil, err
		}
		d.dispatchQueued(e, target)
		return e.Pending(target), nil
	})
	queued(w, v, d.persist(err), map[string]any{
		"target": target,
		"note":   "press the valve button to open its commissioning window first; unvalidated on-air",
	})
}

func (d *daemon) getPairWindow(w http.ResponseWriter, r *http.Request) {
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) { return e.PairWindow(), nil })
	write(w, v, err)
}

// postPairWindow opens the pair window for "seconds"; 0 closes it.
func (d *daemon) postPairWindow(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Seconds int `json:"seconds"`
	}
	if !decodeBody(w, r, &b) {
		return
	}
	window := time.Duration(b.Seconds) * time.Second
	if b.Seconds < 0 || window > tado.MaxPairWindow {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("seconds must be between 0 and %d", int(tado.MaxPairWindow/time.Second)))
		return
	}
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		e.OpenPairWindow(window)
		d.armPairResponder(e, window)
		return e.PairWindow(), nil
	})
	write(w, v, err)
}

// pairBeaconBlock is the number of Enhanced Beacons the bridge may send in one
// window. A joiner asks every 8 s until it gets one, so 64 cover several
// joiners in the longest window.
const pairBeaconBlock = 64

// armPairResponder opens or closes the pair window of the bridge firmware.
// The Enhanced Beacon has to follow the Beacon Request within a few
// milliseconds, faster than a round trip through the host, so the bridge
// answers it from the pairing key, the coordinator and a block of seq and
// frame counter. A bridge without the command leaves the answer to the host.
func (d *daemon) armPairResponder(e *tado.EmuState, window time.Duration) {
	pr, ok := d.radio.(pairWindowRadio)
	if !ok || !d.txArmed() {
		return
	}
	w := cc1101.PairWindow{Duration: window}
	if window > 0 {
		seq, ctr, err := e.ReservePairBeacons(pairBeaconBlock)
		if err != nil {
			log.Printf("pair window: %v", err)
			return
		}
		w = cc1101.PairWindow{
			Duration: window, Key: tado.PairingKey, Coordinator: d.inst.Load().CoordinatorEUI(),
			Seq: seq, Counter: ctr, Count: pairBeaconBlock,
		}
	}
	sent, err := pr.SetPairWindow(w)
	d.pairBridgeOpen = window > 0 && err == nil
	switch {
	case err != nil && window > 0:
		log.Printf("pair window: bridge responder: %v; the host answers Enhanced Beacon Requests", err)
	case err != nil:
		log.Printf("pair window: close on the bridge: %v", err)
	case window > 0:
		e.SetPairResponder(true)
		log.Printf("pair window: bridge answers Enhanced Beacon Requests for %v", window)
	default:
		log.Printf("pair window: closed, the bridge sent %d Enhanced Beacons", sent)
	}
}

// tickPairWindow reports a pair window that ran out with the bridge still
// holding it. The bridge stops answering at the deadline on its own and keeps
// the beacon count until the next pair window command, so a close reads it.
func (d *daemon) tickPairWindow(e *tado.EmuState) {
	if !d.pairBridgeOpen || e.PairWindow().Open {
		return
	}
	d.pairBridgeOpen = false
	pr, ok := d.radio.(pairWindowRadio)
	if !ok {
		return
	}
	sent, err := pr.SetPairWindow(cc1101.PairWindow{})
	if err != nil {
		log.Printf("pair window: expired, the bridge count is unknown: %v", err)
		return
	}
	log.Printf("pair window: expired, the bridge sent %d Enhanced Beacons", sent)
}

// --- entry point ---

// movedTools are the subcommands the freedo binary used to carry. They now
// live in freedo-dev, so a stale command line gets told where they went.
var movedTools = map[string]bool{
	"decrypt": true, "frame": true, "dissect": true, "nodes": true,
	"ping": true, "config": true, "regs": true, "autotune": true,
	"rx": true, "tx": true, "setpoint": true, "emulate": true, "coldstart": true,
}

// redirectMovedTool answers a command line that opens with a subcommand this
// binary no longer carries, and returns for every other word.
func redirectMovedTool(name string) {
	switch {
	case name == "daemon":
		die("the freedo binary is the daemon; remove the 'daemon' word")
	case movedTools[name]:
		die("'%s' moved to the freedo-dev binary: run 'freedo-dev %s'", name, name)
	}
}

// checkStateStatus is the exit status of --check-state, for the add-on start
// script: 0 when the state database is absent or this binary reads it, 3 when
// the stored schema is newer than this binary, 1 on any other error. It prints
// one line saying which.
func checkStateStatus(path string) int {
	status, version, err := install.InspectSchema(path)
	switch {
	case err != nil:
		fmt.Fprintf(os.Stderr, "check-state: %s: %v\n", path, err)
		return 1
	case status == install.SchemaMissing:
		fmt.Printf("check-state: %s: no state database\n", path)
	case status == install.SchemaNewer:
		fmt.Printf("check-state: %s: schema version %s is newer than this binary reads (%s)\n",
			path, version, install.SchemaVersion)
		return 3
	default:
		fmt.Printf("check-state: %s: schema version %s is readable\n", path, version)
	}
	return 0
}

func Main(args []string) {
	fs := flag.NewFlagSet("freedo", flag.ExitOnError)
	configFlag(fs)
	stateFlag(fs)
	addr := fs.String("addr", defaultHTTPListen, "HTTP listen address of the get/set API; with no API token only a loopback address is accepted (overrides http_listen)")
	tokenFile := fs.String("api-token-file", "", "file holding the bearer token of the API, surrounding whitespace trimmed (overrides "+apiTokenEnv+" and api_token_file)")
	healthPublic := fs.Bool("health-public", false, "serve GET /api/health without the API token, for a container health check")
	insecureNoAuth := fs.Bool("insecure-no-auth", false, "serve the API with no token on a non-loopback --addr: every host that reaches the port controls the installation")
	ingress := fs.Bool("ingress", false, "serve the calls of the dashboard without a token to the Home Assistant ingress gateway (172.30.32.2), and the export and the import of the state to a Home Assistant administrator; every other request keeps the token. Only the add-on start script sets it, and it has no effect unless "+supervisorTokenEnv+" is set")
	port := fs.String("port", "", "serial port of the ESP32 bridge (overrides serial_port; empty: no live radio)")
	replay := fs.String("replay", "", "feed this capture file instead of a radio")
	doTx := fs.Bool("tx", false, "transmit the decided frames (default: dry-run, transmit nothing)")
	txctr := fs.Uint64("txctr", 0, "initial 32-bit frame counter for downlinks (overrides tx_counter)")
	bcastctr := fs.Uint64("bcastctr", 0, "floor of the frame counter, which broadcasts share with downlinks (same as --txctr)")
	cslphase := fs.Uint("cslphase", 0, "CSL phase in our Enhanced ACKs: 320 us units elapsed since our own last CSL sample; 0 is safe while our receiver listens continuously (overrides csl_phase)")
	unhandled := fs.String("unhandled", "freedo-unhandled.log", "append the unhandled-frame log to this file (overrides unhandled_log)")
	duration := fs.Duration("duration", 0, "stop the daemon after this long (0 = until Ctrl-C)")
	beacon := fs.Duration("beacon", defaultBeaconInterval, "longest interval of the 0x49e8 router advertisement, each one random from a third of it, transmitted with --tx alone (0 = disabled; overrides beacon_sec)")
	noAutoQueue := fs.Bool("no-auto-queue", false, "never queue automatic writes: UTC-offset, zone forwarding, firmware reads")
	tuneDebug := fs.Bool("tune-debug", false, "log the FREQEST reading of every received frame (overrides tune_debug)")
	debugAPI := fs.Bool("debug-api", false, "serve /api/debug/dissect, /api/debug/decrypt and /api/debug/rx; they decrypt any frame a caller sends and stream live traffic (overrides debug_api)")
	faultTimeDelay := fs.Duration("fault-time-delay", 0, "FAULT INJECTION (test F12): hold our answer to the \"time\" resource back by this long; past 5 s the device discards it and asks again")
	faultNoTime := fs.Bool("fault-no-time", false, "FAULT INJECTION (test F12): never answer the \"time\" resource, so the device never gets a clock and its weekly schedule never runs")
	showVersion := fs.Bool("version", false, "print the build stamp and exit")
	checkState := fs.String("check-state", "", "check the schema version of the state database at this path and exit: status 0 when the file is absent or this binary reads it, 3 when the schema is newer, 1 on any other error")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `freedo - the local tado coordinator daemon

Runs the emulator state machine against the live radio (--port), a capture file
(--replay) or nothing at all, and exposes it over HTTP: GET the derived state,
POST a command to queue a downlink. The default is a dry-run: nothing reaches
the air unless --tx is given. The API keeps serving after a replay ends.

Run it with no arguments to start the daemon on the built-in defaults.

--config names the bootstrap file: listen address, serial port, database path.
The installation - network key, coordinator identity, devices, zones and
circuits - lives in the SQLite database and is managed through /api/install. A
database without one gets a fresh key and coordinator identity on the first
start. A flag given on the command line wins over the bootstrap file; what the
command line leaves alone takes the bootstrap value, then the built-in default.

Every request under /api carries "Authorization: Bearer <token>". The files of
the dashboard, served at every other path, need no token. The token comes from
--api-token-file, then the FREEDO_API_TOKEN environment variable, then the
bootstrap api_token_file. With no token the API is open and binds a loopback
address alone; --insecure-no-auth lifts that limit. --health-public leaves
GET /api/health open for a container health check. --ingress, set by the Home
Assistant add-on, lets the gateway of Home Assistant make the calls of the
dashboard without a token, and the export and the import of the state for a
Home Assistant administrator.

The bench and development tools are the freedo-dev binary.

flags:
`)
		fs.PrintDefaults()
	}
	// The flag set exits on the first unknown flag, so a stale `freedo rx -p ...`
	// would print the daemon usage instead of the redirect. Answer the leading
	// subcommand before the parse.
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		redirectMovedTool(args[0])
	}
	if pos := parseFlags(fs, args); len(pos) > 0 {
		name := pos[0]
		if name == "help" {
			fs.Usage()
			os.Exit(0)
		}
		redirectMovedTool(name)
		die("daemon takes no positional arguments (got %q)", name)
	}
	if *showVersion {
		fmt.Println(buildinfo.Stamp("freedo"))
		return
	}
	if *checkState != "" {
		os.Exit(checkStateStatus(*checkState))
	}
	b := mustBootstrap(fs)
	for _, w := range b.Warnings() {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}
	given := setFlags(fs)
	if !given["addr"] {
		*addr = b.HTTPListen
	}
	if !given["port"] {
		*port = b.SerialPort
	}
	if !given["unhandled"] {
		*unhandled = b.UnhandledLog
	}
	if !given["txctr"] {
		*txctr = uint64(b.TxCounterSeed())
	}
	if !given["cslphase"] {
		*cslphase = uint(b.CSLPhase)
	}
	if !given["debug-api"] {
		*debugAPI = b.DebugAPI
	}
	if !given["tune-debug"] {
		*tuneDebug = b.TuneDebug
	}
	if !given["beacon"] {
		*beacon = b.BeaconInterval()
	}
	token, err := apiToken(*tokenFile, b, os.LookupEnv)
	if err != nil {
		die("%v", err)
	}
	if err := checkListen(*addr, token != "", *insecureNoAuth); err != nil {
		die("%v", err)
	}
	if token == "" {
		fmt.Fprintf(os.Stderr, "WARNING: no API token is configured: the API on %s has no authentication\n", *addr)
	}
	txSeed, err := daemonCounterSeed(*txctr, *bcastctr)
	if err != nil {
		die("counter seed: %v", err)
	}
	dbPath := resolveDatabase(fs, b)
	if info, err := os.Stat(dbPath); err == nil && info.IsDir() {
		die("the state database %s is a directory", dbPath)
	}
	if dir := filepath.Dir(dbPath); dir != "" {
		if _, err := os.Stat(dir); err != nil {
			die("state directory %s does not exist", dir)
		}
	}
	// A capture file is a source of its own: it must not be shadowed by the
	// serial port the bootstrap names for live use.
	if *replay != "" && !given["port"] {
		*port = ""
	}

	imported, err := applyStagedImport(dbPath, time.Now())
	if err != nil {
		die("import into %s: %v", dbPath, err)
	}
	store, err := openStateStore(dbPath)
	check(err)
	if imported {
		fmt.Printf("import: the daemon starts on the imported state in %s\n", dbPath)
		if err := store.SetCounterResume(context.Background(), map[string]uint64{metaImportedAt: uint64(time.Now().Unix())}); err != nil {
			closeStore(store)
			die("state database %s: %v", dbPath, err)
		}
	}
	// A database with no identity gets one here, before anything reads it.
	commissioned, err := store.EnsureIdentity(context.Background())
	if err != nil {
		closeStore(store)
		die("state database %s: %v", dbPath, err)
	}
	in, _, err := store.LoadInstall(context.Background())
	if err != nil {
		closeStore(store)
		die("state database %s: %v", dbPath, err)
	}
	if err := in.ValidateCommissionable(true); err != nil {
		closeStore(store)
		die("installation in %s: %v", dbPath, err)
	}
	for _, w := range in.Warnings() {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}
	if w := utcZoneWarning(in.Options, time.Now()); w != "" {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}

	var uw io.Writer = os.Stderr
	if f, err := os.OpenFile(*unhandled, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		defer f.Close()
		uw = f
	} else {
		fmt.Fprintf(os.Stderr, "unhandled log %s: %v (using stderr)\n", *unhandled, err)
	}
	cfg := in.EmuConfig()
	cfg.TxCounter, cfg.CSLPhase = txSeed, uint16(*cslphase)
	cfg.ZoneGetProxy = b.ZoneGetProxy
	cfg.ZoneProxyTimeout = b.ZoneProxyTimeout()
	cfg.RouterAdvertInterval = *beacon
	cfg.NoZoneFanOut = true
	cfg.NoAutoQueue = *noAutoQueue
	cfg.TimeDelay, cfg.SuppressTime = *faultTimeDelay, *faultNoTime
	if cfg.SuppressTime || cfg.TimeDelay > 0 {
		fmt.Fprintf(os.Stderr, "FAULT INJECTION ACTIVE: time suppressed=%v delayed=%s - "+
			"the devices will not keep a correct clock and their on-device schedules will not run\n",
			cfg.SuppressTime, cfg.TimeDelay)
	}
	ulog := tado.NewUnhandledLogger(uw)
	emu := tado.NewEmu(cfg, ulog)
	if err := restoreDesiredState(store, emu); err != nil {
		closeStore(store)
		die("state database %s: %v", dbPath, err)
	}
	// RestoreDesiredState brings back the stored d/config documents, so the
	// installation has to land after it: the database is the source of truth and
	// the desired state is the device-facing cache.
	if _, _, err := emu.SetInstall(in); err != nil {
		closeStore(store)
		die("installation in %s: %v", dbPath, err)
	}

	var link radio.Radio
	mode := "mock"
	if *port != "" && *doTx {
		at, _, err := install.ReadMetaUint(context.Background(), store.db, metaImportedAt)
		if err != nil {
			closeStore(store)
			die("state database %s: %v", dbPath, err)
		}
		if left := importWaitLeft(at, time.Now()); left > 0 {
			fmt.Printf("import: the state was imported less than %s ago: waiting %s before the start, so that no device still holds a Message ID of the replaced state. Until then the API on %s answers 503\n",
				midWait, left.Round(time.Second), *addr)
			until := time.Now().Add(left)
			if err := serveImportWait(*addr, serverHandler(token, *healthPublic, *ingress, importWaitHandler(until)), until); err != nil {
				closeStore(store)
				die("http: %v", err)
			}
		}
	}
	switch {
	case *port != "":
		r, err := newLiveRadio(store, *port, in.CoordinatorEUI(), cfg.CSLPhase, *doTx, *tuneDebug)
		if err != nil {
			closeStore(store)
			die("%v", err)
		}
		link, mode = r, "live"
	case *replay != "":
		r, err := radio.NewReplay(*replay)
		if err != nil {
			closeStore(store)
			die("%v", err)
		}
		link, mode = r, "replay"
	default:
		link = radio.NewMock(nil)
	}

	d := newDaemon(in, emu, link, !*doTx, mode)
	if mode == "live" {
		d.setCalibration(store, *port)
	}
	d.debugAPI = *debugAPI
	if err := d.attachState(store); err != nil {
		closeStore(store)
		die("%v", err)
	}
	if err := d.purgeHotWaterSchedules(emu); err != nil {
		closeStore(store)
		die("state database %s: %v", dbPath, err)
	}
	d.ulog = ulog
	d.beaconEvery = *beacon
	if err := d.restoreHomeState(emu); err != nil {
		closeStore(store)
		die("%v", err)
	}
	if err := d.restoreCounters(emu); err != nil {
		closeStore(store)
		die("frame counters: %v", err)
	}
	if err := d.restoreRouterAdvert(emu); err != nil {
		closeStore(store)
		die("router advertisement: %v", err)
	}
	if err := d.restoreEnergy(); err != nil {
		closeStore(store)
		die("boiler energy: %v", err)
	}
	if err := d.restoreHistory(); err != nil {
		closeStore(store)
		die("statistics: %v", err)
	}
	d.applyHomeState(emu)
	unknownModules := emu.UnknownInterfaceModules()
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)

	srv := newAPIServer(*addr, serverHandler(token, *healthPublic, *ingress, d.mux()))
	// An open rx tap would otherwise hold the whole HTTP drain budget.
	srv.RegisterOnShutdown(d.closeTaps)
	srvErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			srvErr <- err
		}
	}()

	txState := "dry-run"
	if *doTx {
		txState = "ARMED"
	}
	fmt.Printf("daemon: mode=%s tx=%s addr=%s home=%d devices=%d zones=%d coordinator=%s short=%s\n",
		mode, txState, *addr, in.HomeID, len(in.Devices), len(in.Zones),
		hex.EncodeToString(in.CoordinatorEUI()), tado.ShortFromWire(in.CoordinatorEUI()))
	if mode == "live" && !d.calibrated {
		fmt.Println(calibrationWarning(*port))
	}
	if len(unknownModules) > 0 {
		fmt.Printf("WARNING: no interface module id (tag 015d) known for %s: it is served its d/config without 015d until it reports one\n",
			strings.Join(unknownModules, ", "))
	}
	if commissioned {
		fmt.Println("the installation is empty: a new network key and a new coordinator identity were generated.")
		fmt.Println("every frame from an existing network fails the MIC until the devices are paired or the database is seeded.")
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	var timeout <-chan time.Time
	if *duration > 0 {
		t := time.NewTimer(*duration)
		defer t.Stop()
		timeout = t.C
	}
	var httpErr, radioErr error
	restart := false
	select {
	case <-d.restart:
		restart = true
		fmt.Println("import: a state is staged, restarting")
	case s := <-sig:
		fmt.Printf("interrupted by %s\n", s)
		// A second signal leaves at once, whatever the shutdown is waiting for.
		go func() {
			<-sig
			os.Exit(1)
		}()
	case <-timeout:
	case httpErr = <-srvErr:
	case radioErr = <-d.radioLost:
		// A supervisor (systemd, a container restart policy) starts a new
		// process, which opens the bridge again.
		fmt.Fprintf(os.Stderr, "radio loop ended: %v\n", radioErr)
	}

	backstop := time.AfterFunc(shutdownBudget, func() {
		fmt.Fprintf(os.Stderr, "WARNING: shutdown did not finish within %s, leaving now\n", shutdownBudget)
		os.Exit(1)
	})
	defer backstop.Stop()

	// Order matters: the handlers drain while the engine still answers, so the
	// final save is the last writer of the state database.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), httpDrainBudget)
	defer shutdownCancel()
	_ = srv.Shutdown(shutdownCtx)
	if err := d.saveDesiredStateBefore(time.Now().Add(stateSaveBudget)); err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: persist desired state on shutdown: %v\n", err)
	}
	if err := d.saveEnergyBefore(time.Now().Add(stateSaveBudget)); err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: persist boiler energy on shutdown: %v\n", err)
	}
	if err := d.closeHistoryBefore(time.Now().Add(stateCloseBudget)); err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: close the statistics on shutdown: %v\n", err)
	}
	cancel()
	closeWithin("the radio", radioCloseBudget, link.Close)
	if store != nil {
		closeWithin("the state database", stateCloseBudget, store.Close)
	}
	fmt.Println("daemon stopped")
	if httpErr != nil {
		die("http: %v", httpErr)
	}
	if radioErr != nil {
		die("radio: %v", radioErr)
	}
	if restart {
		// A service manager or a restart policy starts the process when this fails,
		// and that start applies the staged file too.
		die("restart after the import: %v", reexec())
	}
}

// closeWithin runs close on its own goroutine and gives up after budget. A
// serial port or a database that does not answer must not hold the shutdown.
func closeWithin(what string, budget time.Duration, close func() error) {
	closeWithinTo(os.Stderr, what, budget, close)
}

func closeWithinTo(warn io.Writer, what string, budget time.Duration, close func() error) {
	done := make(chan error, 1)
	go func() { done <- close() }()
	select {
	case err := <-done:
		if err != nil {
			fmt.Fprintf(warn, "WARNING: close %s: %v\n", what, err)
		}
	case <-time.After(budget):
		fmt.Fprintf(warn, "WARNING: close %s did not finish within %s\n", what, budget)
	}
}

// daemonCounterSeed is the floor of the frame counter. Broadcasts share it,
// so --bcastctr is one more floor.
func daemonCounterSeed(tx, broadcast uint64) (uint32, error) {
	if tx > uint64(^uint32(0)) {
		return 0, errors.New("--txctr must fit in uint32")
	}
	if broadcast > uint64(^uint32(0)) {
		return 0, errors.New("--bcastctr must fit in uint32")
	}
	return uint32(max(tx, broadcast)), nil
}
