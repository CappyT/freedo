package tado

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Coordinator emulator: the receive-side state machine that reproduces the tado
// internet bridge (the 802.15.4-2015 CSL PAN coordinator) with the real bridge
// powered off.
//
// OnFrame is pure: it decodes one received MAC frame, updates state and returns
// the frames the daemon should transmit. It never touches a radio, so the whole
// machine is testable against captured traffic.
//
// Downlink strategy (from firmware analysis):
//
//   1. Piggyback. A sleepy device is only reachable while it is already talking
//      to us, so a queued downlink goes out right after the Enhanced ACK of any
//      frame received from that device. This is the reliable path.
//   2. Cold start. ScheduleDownlink emits a CSL wake-up train plus the Data
//      frame. Best-effort: under ESP32 USB-serial latency (~2.5 ms round trip)
//      hitting the exact rendezvous slot is unlikely. Live hardware test pending.
//   3. Delivery of application writes is confirmed by the device's correlated
//      CoAP response. Unconfirmed downlinks retry on the next rendezvous, up to
//      maxDownlinkTries.
//
// EmuState is not safe for concurrent use; drive it from one goroutine (same
// contract as Reassembler). Only UnhandledLogger is mutex-protected, so a status
// goroutine can read its counters.

const (
	// maxDownlinkTries bounds the piggyback retries of one queued downlink.
	maxDownlinkTries   = 4
	maxDownlinkHistory = 32
	// maxZoneHopsPerTarget bounds the relayed zone datagrams waiting for one
	// sleepy target. A live run grew one backlog to 46 entries, 18 minutes deep.
	maxZoneHopsPerTarget = 8
	// wakeUpTrainLen and the derived step spread the cold-start train over one
	// full CSL period, so the target samples the channel at least once inside it.
	wakeUpTrainLen = 40
	// bcastSecCtrl is the aux-header Security Control of the 0x49e8 broadcast,
	// as read off every captured one.
	bcastSecCtrl = 0x04
)

var (
	ErrBroadcastCounterExhausted = errors.New("tado: broadcast frame counter exhausted")
	ErrHeatingBlockUnavailable   = errors.New("tado: mode/openwindow needs a complete heating block")
	ErrHeatingBlockPending       = errors.New("tado: mode/openwindow waits for the previous heating update")
	ErrTxCounterExhausted        = errors.New("tado: unicast frame counter exhausted")
)

// ActionKind is the class of on-air frame an Action carries.
type ActionKind int

const (
	ActNone ActionKind = iota
	ActEnhAck
	ActData
	ActWakeUp
	ActBroadcast
)

func (k ActionKind) String() string {
	switch k {
	case ActEnhAck:
		return "EnhACK"
	case ActData:
		return "Data"
	case ActWakeUp:
		return "WakeUp"
	case ActBroadcast:
		return "Broadcast"
	}
	return "None"
}

// Action is one frame the daemon should transmit (or, in dry-run, just log).
// Bytes carries no CC1101 CRC; the Enhanced ACK's own 2-byte MAC FCS is included
// because it is part of the frame the device validates.
type Action struct {
	Kind   ActionKind
	Bytes  []byte
	Reason string // human note, e.g. "enh-ack seq=42"
	Node   string // target room/short when known
	// NotBefore holds the action back until this instant. Zero means send it now,
	// which is every action on the normal path: the CSL rendezvous is the only
	// schedule that matters and it is already open when the action is decided.
	// Only the "time" fault injection sets it, to push the answer past the
	// device's 5 s discard deadline on purpose.
	NotBefore time.Time
	CSLTx     bool
	// HasCSLBatch and CSLBatchTag identify RFC 4944 fragments of one datagram.
	// The tag may be zero after the RFC 4944 counter wraps.
	HasCSLBatch    bool
	CSLBatchTag    uint16
	NoCSLBootstrap bool
	QueueAttempt   bool
	QueueTarget    string
	WaitResponse   bool
	QueueTry       int
	QueueMID       uint16
	// CoAPTransaction marks the API commands whose bridge-side CON exchange
	// uses the generic retransmission transaction, not MAC acknowledgement.
	CoAPTransaction bool
	// ResponseRetry marks a response we deliver, a captured NON 2.xx peer
	// relay or an auth answer: it retries as one MAC transmission after a
	// missed Enhanced ACK.
	ResponseRetry bool
	// IdentifyCSLBootstrapRetry keeps the measured no-anchor bootstrap rule
	// specific to d/identify. The firmware path of other CON requests is unknown.
	IdentifyCSLBootstrapRetry bool
	// CSLBootstrapAlways permits the bootstrap train on every attempt. A device
	// asks auth/token on a (re)join, when the bridge may hold a stale anchor for
	// it; the firmware prefers a warm anchor over the train until it misses once.
	CSLBootstrapAlways bool
	// ZoneRelay marks a re-emitted zone datagram. Its requester retransmits
	// within seconds, so a missed Enhanced ACK retries on that scale.
	ZoneRelay bool
}

// NodeState is the coordinator's per-device record.
type NodeState struct {
	WireLE    string // 8-byte wire EUI, hex
	Short     string // 16-bit short address, hex
	Label     string // room or serial from Inventory, else the short address
	RxCounter uint32 // last anti-replay counter seen from the device
	CSLPhase  uint16 // last advertised phase: 320 us units since the device's own CSL sample
	CSLPeriod uint16
	LastSeen  time.Time
	LastSeq   byte
	Telemetry map[string]string // tag label -> latest decoded value

	// Last-known heating block from received TLVs or a CoAP-confirmed write.
	// A mode or open-window downlink resends the complete block.
	LastSetpointC  float64 // 6280
	LastSchedC     float64 // 6200
	LastOverlay    byte    // 6240
	LastOpenWindow bool    // 62e0
	LastID         int     // logical CoAP id seen in an uplink id=/lid= query

	// SessionID is the auth/token session id we served this device; Option2048
	// is the value it actually echoes back as CoAP option 2048 on its uplinks.
	// The device copies the id verbatim, so the two must agree once served.
	SessionID  []byte
	Option2048 []byte
	// SessionAdopted is set once a request after the last served session id
	// carries it as option 2048.
	SessionAdopted bool
	// sessionDrift is the last logged drift of a served session: "missing" or
	// "mismatch", empty while none is logged.
	sessionDrift string
	// authMID and authReply are the last auth answer, reused verbatim for a
	// retransmission of the same request.
	authMID   uint16
	authReply []byte

	// Legionella is the value the device last posted to z/legionella (tag 63c0).
	// Only its first byte is acted on downstream; nil means never posted.
	Legionella []byte

	raw      map[string][]byte // tag hex -> latest raw TLV value
	fallback *fallbackState    // 0182, nil until the device reports it
	// unsupported holds, per boiler bus resource, the tags its last 0481
	// lists. faultCodes holds the 0460 codes of the last hvac/codes report.
	unsupported map[string][]string
	faultCodes  []uint32

	// battery holds the tag 0162 readings of the last BatteryWindow.
	battery    []batteryReading
	lowBattery bool
}

// SetpointParams is a queued "set temperature" downlink.
type SetpointParams struct {
	ID       int // CoAP query id=N (device logical id)
	Setpoint float64
	// Sched is the coordinator's own record of the scheduled setpoint. No
	// plane carries it, so it never leaves the daemon.
	Sched      float64
	Overlay    byte // 0 auto, 2 manual, 3 until_next
	OpenWindow byte
	// RunningOverlay marks a write that updates an overlay the zone already
	// runs, so the block carries no 6440.
	RunningOverlay bool
	// Off writes the room off overlay (6260=0, no 6280) instead of Setpoint.
	Off bool
	// Duration arms the coordinator-side timer of the overlay. The deadline
	// never goes on the air: at expiry the emulator queues the return to
	// schedule itself. Zero leaves the overlay running.
	Duration time.Duration
	// Per-zone commissioning tags, written only when set and unobserved on air.
	// Nothing in the daemon needs them: see SetpointCoAP.
	ZoneFlags  *byte
	Circuit    *byte
	CtrlEnable *byte
}

// EmuConfig configures the emulator. Key and SelfEUI are per-installation and
// have no usable default: build the config with (*Install).EmuConfig and set
// the gates on top of it.
type EmuConfig struct {
	// Install is the installation we coordinate: it resolves a command target
	// to a device and names the nodes in the logs. nil falls back to the legacy
	// Inventory, which is empty outside the tests.
	Install *Install
	Key     []byte // nil => NetworkKey, empty outside the tests
	SelfEUI []byte // nil => BridgeEUI (our 8-byte wire EUI), same
	// TxCounter seeds the frame counter we put in our own downlinks. No device
	// checks it on receive - valve and bridge alike accept a frame on the
	// AES-CCM* MIC alone and keep no last-accepted counter - so any seed is
	// accepted and a low one cannot lock us out. It exists for our own
	// bookkeeping and to keep the CCM* nonce unique per key.
	TxCounter uint32
	// BroadcastCounter seeds the 32-bit counter split across a broadcast frame.
	BroadcastCounter     uint32
	RouterAdvertInterval time.Duration
	// CSLPhase is advertised in the Enhanced ACKs we send: 320 us units elapsed
	// since our own last CSL sample. It only serves to centre the device's ~79 ms
	// wake-up train on that instant, so 0 is safe while our CC1101 listens
	// continuously - there is no sample instant to miss - and becomes mandatory
	// the moment we duty-cycle the radio. Advertising 0 costs an uplink deferral
	// of up to ~1 s, and the device's transmit instant precesses backwards by
	// roughly 40 ms per exchange.
	CSLPhase uint16
	// Now is the wall clock the served "time" resource reads. nil => time.Now.
	Now func() time.Time
	// FragTagSeed supplies a testable initial 6LoWPAN datagram tag.
	FragTagSeed func() uint16
	// Zones declares the cloud's zone map: zone number -> members, each a
	// resolvable target (serial, short address, room or wire EUI). The
	// directory also learns from the lid= traffic it routes, so a declaration
	// is only needed for a zone whose members stay silent while we listen.
	Zones map[int][]string
	// UTCOffsetMin is TLV 02b2, signed minutes east of UTC, as a fixed value.
	// Set, it overrides the installation's timezone. The device keeps no DST
	// rules of its own, so the offset only ever reaches it from whoever serves
	// its config. nil without an installation serves none, which is not the
	// same as 0.
	UTCOffsetMin *int
	// ZoneGetProxy forwards a zone GET addressed to us to another member of the
	// zone instead of answering it from our own cache. Positive sense, unlike
	// the gates below, because the cached answer is what runs today: no capture
	// holds a zone GET addressed to the bridge, so the proxy is unproven on air
	// and a member that stays silent is covered by ZoneProxyTimeout.
	ZoneGetProxy bool
	// ZoneProxyTimeout bounds the wait for a proxied zone GET: past it we answer
	// the requester ourselves from the zone cache. Zero takes
	// defaultZoneProxyTimeout; negative waits forever, which is what the proxy
	// did before the timeout existed.
	ZoneProxyTimeout time.Duration
	// Negative-sense gates so the zero value emulates fully.
	NoEnhAck     bool // never answer a Data frame with a MAC Enhanced ACK
	NoCoAPAck    bool // never answer a CON uplink with a CoAP ACK
	NoZoneFanOut bool // never re-emit a zone PUT addressed to us to the other members
	NoAutoQueue  bool // never queue UTC-offset writes or zone forwarding

	// Fault injection. These exist for the phase C fault tests and break the
	// installation on purpose; the zero value is normal behaviour. They belong to
	// nothing in the installation config and are only reachable from the
	// daemon's --fault-* flags.
	//
	// SuppressTime withholds every answer to the "time" resource. The device then
	// never gets a clock, rt_time_now_get keeps returning -3 and the on-device
	// weekly schedule is never evaluated, while the valve holds its last
	// commanded setpoint indefinitely. Test F12, second half.
	SuppressTime bool
	// TimeDelay holds the answer to "time" back by this long. Past 5 s the device
	// discards it (coap_rx_timed_response_dispatch@0x22748) and asks again, which
	// is what F12 measures. Zero answers synchronously, which is the only
	// behaviour that actually sets the clock.
	TimeDelay time.Duration
}

// EmuStats are running counters for the CLI status view.
type EmuStats struct {
	Frames              int
	Data                int
	EnhAcks             int
	WakeUps             int
	Broadcasts          int
	ICMPv6              int
	RouterSolicitations int
	// RouterSolicitationsAnswered counts the solicitations our advertisement
	// covers: one burst answers every device that asked inside the window.
	RouterSolicitationsAnswered int
	RouterAdvertisements        int
	RouterAdvertisementsSent    int
	NeighborSolicitations       int
	Unknown                     int
	MICFails                    int
	Fragments                   int
	Reassembled                 int
	CoAPAcksSent                int
	DownlinksSent               int
	DownlinksDelivered          int
	DownlinksDropped            int
	ZoneRelays                  int
	ZoneRelaysCollapsed         int
	ZoneFanouts                 int
	ZoneProxies                 int
	ZoneProxyTimeouts           int
	// Fault-injection counters. Non-zero means the run was deliberately broken.
	TimeSuppressed int
	TimeDelayed    int
}

// queuedDownlink is one pending command to a device. The CoAP datagram is built
// once at enqueue and reused verbatim on every attempt, so its MID and token stay
// put and the device can deduplicate at the app layer. Ordinary commands advance
// the MAC sequence and anti-replay counter. Responses (captured relayed NON
// responses, auth answers) retain their MAC sequence while advancing only the
// anti-replay counter.
type queuedDownlink struct {
	kind          string // resource class: setpoint, childlock, identify, pair, ...
	desc          string // human note used in the Action.Reason
	coap          []byte // the fixed CoAP datagram
	key           []byte // MAC key: the network key, or PairingKey for d/pair
	mid           uint16 // for confirm-by-MID
	responseToken []byte // required token of the terminal CoAP response
	responseCode  string // required terminal response code; empty means 2.04
	route         string // zone traffic: origin|resource, so a newer state supersedes it
	relayID       string // zone traffic: origin|type|code|MID of the re-emitted message
	relayAck      bool   // zone traffic: an ACK or RST that closes the target's CON exchange
	fragmented    bool   // RFC 4944 transport; delivery waits for the CoAP ACK
	fragTag       uint16 // RFC 4944 tag of the current fragmented attempt
	requireCoAP   bool   // Enhanced ACK proves reception, not completion
	responseRetry bool   // captured NON 2.xx relay or auth answer: retry as one MAC transmission
	tries         int
	seq           byte // MAC seq of the last attempt
	inFlight      bool
	awaitingCSL   bool
	retryAt       time.Time
	deferred      bool
	heating       *heatingBlock
	dhw           *dhwBlock
}

type activeGet struct {
	resultKey string
	path      []string
	query     []string
	token     []byte
	blockNum  uint32
	blockSZX  byte
	body      []byte
}

type heatingBlock struct {
	zone       int
	setpoint   []byte
	sched      []byte
	overlay    byte
	openWindow byte
	body       []byte
	// windowOnly marks a write built without the node's block: its delivery
	// confirms the 62e0 it carries and nothing else.
	windowOnly bool
}

// boostTimer is one coordinator-side overlay deadline. The boost owns the
// captured case; a setpoint write with a duration arms the same timer.
type boostTimer struct {
	id        int
	expiresAt time.Time
	dhw       bool // the restore is the hot-water block, 6020=2
	boost     bool // the overlay is the boost, not a timed setpoint
}

type dhwBlock struct {
	zone     int
	setpoint []byte
	overlay  byte
	enabled  byte
}

func heatingBlockFor(zone int, setpoint, sched float64, overlay, openWindow byte) *heatingBlock {
	return &heatingBlock{
		zone: zone, setpoint: tempC(setpoint), sched: tempC(sched), overlay: overlay, openWindow: openWindow,
	}
}

// setpointHeatingBlock is the block a delivered room write confirms. The off
// overlay confirms its own body: the zone holds no 6280 and the node keeps the
// setpoint it last had.
func setpointHeatingBlock(c SetpointCoAP, sched float64) *heatingBlock {
	if c.Off {
		if body, err := setpointBody(c); err == nil {
			return &heatingBlock{zone: c.ID, body: body}
		}
	}
	return heatingBlockFor(c.ID, c.Setpoint, sched, c.Overlay, c.OpenWindow)
}

type QueueEntry struct {
	Kind     string `json:"kind"`
	Desc     string `json:"desc"`
	Tries    int    `json:"tries"`
	Seq      byte   `json:"seq"`
	MID      uint16 `json:"mid"`
	InFlight bool   `json:"inFlight"`
}

// DownlinkOutcome records the terminal result and cause of a removed downlink.
type DownlinkOutcome struct {
	Kind      string    `json:"kind"`
	Desc      string    `json:"desc"`
	Tries     int       `json:"tries"`
	Seq       byte      `json:"seq"`
	MID       uint16    `json:"mid"`
	Result    string    `json:"result"`
	Reason    string    `json:"reason"`
	Completed time.Time `json:"completedAt"`
}

// zoneProxyWait is one zone GET we forwarded to a member, held until that member
// answers or the deadline passes. It keeps everything needed to answer the
// requester ourselves, because by then the original frame is long gone.
type zoneProxyWait struct {
	requester []byte // wire EUI of the device that asked us
	asked     []byte // wire EUI of the member we forwarded to
	label     string // requester's room/short name, for logs
	zone      int
	resource  string // normalized, for the zoneState cache lookup
	uri       string
	mid       uint16
	token     []byte
	srcPort   uint16 // requester's port: the destination of our answer
	dstPort   uint16
	deadline  time.Time
}

// defaultZoneProxyTimeout bounds the wait for a proxied zone GET. A device on
// this PAN reports on a CSL rendezvous roughly once a second and its own report
// interval is 112 s by default, so a member that is awake answers well inside
// this; one that does not is asleep or gone, and the requester is better served
// by our cached answer than by silence.
const defaultZoneProxyTimeout = 10 * time.Second
const activeGetRetryBackoff = 30 * time.Second
const BoostDuration = 30 * time.Minute

// EmuState is the bridge emulator.
type EmuState struct {
	cfg     EmuConfig
	inst    *Install
	key     []byte
	selfEUI []byte
	log     *UnhandledLogger
	reasm   *Reassembler
	block2  *block2Reasm
	nodes   map[string]*NodeState        // wire EUI hex -> state
	queue   map[string][]*queuedDownlink // wire EUI hex -> FIFO
	history map[string][]DownlinkOutcome
	// flowC is the CH flow setpoint (deg C) the emulator serves per heating
	// circuit id when the wall thermostat polls "h/<home>/c/<id>/config" (tag
	// 2040); a circuit without one gets options.flow_limit_c. flowFresh marks a
	// value the thermostat has not fetched yet.
	flowC     map[int]float64
	flowFresh map[int]bool
	// pendGet maps the live request MID to one active GET exchange.
	pendGet   map[uint16]*activeGet
	getRes    map[string][]byte
	getCode   map[string]string
	childLock map[string]bool
	tempUnitF map[string]bool
	// offlineSched holds, per device (wire EUI hex), the tag 02b3 an operator
	// wrote through EnqueueConfig. It outranks options.offline_schedule.
	offlineSched map[string]bool
	// cfgDoc holds, per device (wire EUI hex), the last d/config document we
	// pushed and the ETag that stamps it. The bridge validates a device's
	// conditional GET of d/config against this: matching ETag -> 2.03 Valid,
	// otherwise it resends the document with its ETag.
	cfgDoc map[string]cfgSnapshot
	// moduleID holds, per wall thermostat (wire EUI hex), the interface module
	// id (tag 015d) it reported itself, and moduleLearned a value the owner has
	// not persisted yet. moduleIssue holds the last refused or suspicious id.
	// withheldLogged marks the devices whose d/config GET was already logged as
	// answered without a document.
	moduleID       map[string]uint16
	moduleIssue    map[string]InterfaceModuleIssue
	moduleLearned  bool
	withheldLogged map[string]bool
	// sessions holds, per device (wire EUI hex), the session id last served on
	// auth/token and when, and sessionIssued one the owner has not persisted yet.
	// A restart restores it, so a device that echoes the id reads as adopted.
	sessions      map[string]issuedSession
	sessionIssued bool
	// schedDoc holds the weekly program the valve fetches through its zone schedule resource.
	schedDoc map[string]cfgSnapshot
	// zones is our routing table for the zone plane: zone number -> member wire
	// EUIs (hex). zoneState caches the last body published on a zone resource,
	// keyed "<zone>|<resource>", so a GET of it can be answered from here.
	// zoneStateChanged marks a z/s change the owner has not persisted yet.
	zones            map[int]map[string]bool
	zoneState        map[string][]byte
	zoneStateChanged bool
	// zoneProxyPend holds the zone GETs we forwarded to a member on a
	// requester's behalf, keyed by the MID the member echoes back. An entry that
	// outlives its deadline is answered by us instead: see ExpireZoneProxies.
	zoneProxyPend map[uint16]*zoneProxyWait
	// zoneRep and circuitRep hold the merged heat-demand chain, keyed by zone
	// number and by heating-circuit id. obsRoles holds the 015e role records a
	// device published about itself. See demand.go.
	zoneRep    map[int]*demandReport
	circuitRep map[int]*demandReport
	obsRoles   map[string][]NodeTableRec
	// zoneFallback holds the last 6460 of each zone and fallbackEvents the
	// fallback changes the daemon has not taken yet. See fallback.go.
	zoneFallback   map[int]*fallbackState
	fallbackEvents []FallbackEvent
	// utcSettled records, per device (wire EUI hex), the configured UTC offset
	// that device is settled at. Any config write carrying tag 02b2 settles it,
	// an operator's as much as ours, so the automatic push adds nothing until
	// the configured value itself changes.
	utcSettled map[string]int
	// utcZone is the zone tag 02b2 follows, utcOffset the offset in force and
	// utcPushedAt the instant of the last automatic 02b2 write. See utc_offset.go.
	utcZone     *time.Location
	utcOffset   *int
	utcPushedAt time.Time
	boostTimers map[string]boostTimer
	// fwLock holds the devices a firmware upload owns, fwReply the terminal
	// answer of each device's last d/fw exchange, and fwPut and fwGet the last
	// fw/state each device PUT on its own or answered to our GET. fwSeq orders
	// those two. See emu_fw.go.
	fwLock  map[string]bool
	fwReply map[string]FwReply
	fwPut   map[string]FwStateReport
	fwGet   map[string]FwStateReport
	fwSeq   uint64
	// owReports holds the open window reports received since the daemon last
	// took them: the daemon owns the zone window state they drive.
	owReports []OpenWindowReport
	// overlayReports holds the device overlay reports the owner has not taken.
	overlayReports []OverlayReport
	// zoneWrites holds the last delivered write that changed each zone block,
	// and zoneStateReports the z/s member reports the owner has not taken. See
	// zone_echo.go.
	zoneWrites       map[int]deliveredWrite
	zoneStateReports []ZoneStateReport
	// home is the state of the whole home and schedC the setpoint the weekly
	// program holds in each zone. The daemon owns both and pushes them here with
	// SetHomeState; every z/s write the emulator builds stamps them. homeSet
	// reports that the owner pushed them: a tool that never does has no home
	// state to serve on a read.
	home    bool
	homeSet bool
	schedC  map[int]float64
	// schedOff holds the zones whose program block in force is disabled. The
	// daemon owns it and pushes it with SetScheduleOff.
	schedOff map[int]bool
	// windowOpen holds the 62e0 the daemon wants in each zone its window engine
	// drives. The daemon
	// owns it and pushes it with SetOpenWindows.
	windowOpen          map[int]bool
	tx                  uint32 // our anti-replay counter
	seq                 byte   // our MAC sequence number
	bcastCt             uint32
	txArmed             func() bool
	lastRouterAdvert    time.Time
	routerAdvertSent    bool
	routerAdvertPending bool
	fragTag             uint16
	stats               EmuStats
	acceptedInboundIPv6 []byte

	bridgeFrames int    // Data frames seen carrying our own source prefix
	maxBridgeCtr uint32 // highest anti-replay counter seen on those frames
}

// NewEmu returns an emulator. A nil logger discards.
//
// A missing Key or SelfEUI is not defaulted to anything usable: the caller owns
// the installation and must pass (*Install).EmuConfig. The signature stays
// error-free because the whole test corpus constructs an EmuConfig literal, so
// the failure is reported where the config is built instead.
func NewEmu(cfg EmuConfig, log *UnhandledLogger) *EmuState {
	if cfg.Key == nil {
		cfg.Key = NetworkKey
	}
	if cfg.SelfEUI == nil {
		cfg.SelfEUI = BridgeEUI
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	fragTag := randomFragTagSeed()
	if cfg.FragTagSeed != nil {
		fragTag = cfg.FragTagSeed()
	}
	if log == nil {
		log = NewUnhandledLogger(nil)
	}
	e := &EmuState{
		cfg:            cfg,
		inst:           cfg.Install,
		key:            cfg.Key,
		selfEUI:        cfg.SelfEUI,
		log:            log,
		reasm:          NewReassemblerWithClock(cfg.Now, defaultFragmentReassemblyTimeout),
		block2:         newBlock2Reasm(cfg.Now),
		nodes:          map[string]*NodeState{},
		queue:          map[string][]*queuedDownlink{},
		history:        map[string][]DownlinkOutcome{},
		flowC:          map[int]float64{},
		flowFresh:      map[int]bool{},
		pendGet:        map[uint16]*activeGet{},
		getRes:         map[string][]byte{},
		getCode:        map[string]string{},
		childLock:      map[string]bool{},
		tempUnitF:      map[string]bool{},
		offlineSched:   map[string]bool{},
		cfgDoc:         map[string]cfgSnapshot{},
		moduleID:       map[string]uint16{},
		moduleIssue:    map[string]InterfaceModuleIssue{},
		sessions:       map[string]issuedSession{},
		withheldLogged: map[string]bool{},
		schedDoc:       map[string]cfgSnapshot{},
		zones:          map[int]map[string]bool{},
		zoneState:      map[string][]byte{},
		zoneWrites:     map[int]deliveredWrite{},
		zoneProxyPend:  map[uint16]*zoneProxyWait{},
		zoneRep:        map[int]*demandReport{},
		circuitRep:     map[int]*demandReport{},
		zoneFallback:   map[int]*fallbackState{},
		obsRoles:       map[string][]NodeTableRec{},
		utcSettled:     map[string]int{},
		boostTimers:    map[string]boostTimer{},
		fwLock:         map[string]bool{},
		fwReply:        map[string]FwReply{},
		fwPut:          map[string]FwStateReport{},
		fwGet:          map[string]FwStateReport{},
		schedC:         map[int]float64{},
		tx:             cfg.TxCounter,
		bcastCt:        cfg.BroadcastCounter,
		fragTag:        fragTag,
	}
	for zone, members := range cfg.Zones {
		for _, m := range members {
			if err := e.AddZoneMember(zone, m); err != nil {
				log.Log("Config", m, "", rsnZoneTarget+" zone="+strconv.Itoa(zone), "")
			}
		}
	}
	e.loadUTCZone()
	e.utcOffset = e.resolveUTCOffset(cfg.Now())
	e.initConfigDocuments()
	return e
}

// Stats returns a snapshot of the running counters.
func (e *EmuState) Stats() EmuStats { return e.stats }

// ExpireFragments discards timed-out incomplete 6LoWPAN datagrams.
func (e *EmuState) ExpireFragments() int { return e.reasm.Expire() }

// TakeAcceptedInboundIPv6 returns the source EUI of one accepted unicast IPv6
// datagram and clears the event. The emulator owner calls it after OnFrame.
func (e *EmuState) TakeAcceptedInboundIPv6() []byte {
	src := e.acceptedInboundIPv6
	e.acceptedInboundIPv6 = nil
	return src
}

// BridgeSeen reports whether the real bridge has been heard on air (a Data frame
// carrying our own source prefix). It stays true for the rest of the run: two
// coordinators on one PAN is a state the emulator must not transmit into.
func (e *EmuState) BridgeSeen() bool { return e.bridgeFrames > 0 }

// MaxBridgeCounter returns the highest frame counter observed on a
// bridge-sourced Data frame. No device checks the counter on receive, so this is
// not a floor we have to clear; it is a plausible starting point that keeps our
// downlink stream ordered relative to what the real bridge left behind.
func (e *EmuState) MaxBridgeCounter() uint32 { return e.maxBridgeCtr }

// SetTxCounter reseeds our downlink anti-replay counter.
func (e *EmuState) SetTxCounter(v uint32) { e.tx = v }

// TxCounter returns our current downlink anti-replay counter.
func (e *EmuState) TxCounter() uint32 { return e.tx }

// Nodes returns a snapshot of learned node state, sorted by short address.
func (e *EmuState) Nodes() []NodeState {
	out := make([]NodeState, 0, len(e.nodes))
	for _, n := range e.nodes {
		c := *n
		c.Telemetry = make(map[string]string, len(n.Telemetry))
		for k, v := range n.Telemetry {
			c.Telemetry[k] = v
		}
		c.raw = make(map[string][]byte, len(n.raw))
		for k, v := range n.raw {
			c.raw[k] = append([]byte{}, v...)
		}
		c.unsupported = maps.Clone(n.unsupported)
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Short < out[j].Short })
	return out
}

// OnFrame decodes one received MAC frame (no FCS), updates state and returns the
// actions the daemon should transmit.
func (e *EmuState) OnFrame(raw []byte) []Action {
	e.acceptedInboundIPv6 = nil
	e.stats.Frames++
	switch c := Classify(raw); c {
	case ClassData, ClassDataPending:
		return e.onData(c, raw)
	case ClassEnhAck:
		e.onEnhAck(raw)
	case ClassWakeUp:
		e.onWakeUp(raw)
	case ClassBroadcast:
		return e.onBroadcast(raw)
	default:
		e.stats.Unknown++
		e.log.Log("Unknown", "", "", rsnUnclassified, hex.EncodeToString(raw))
	}
	return nil
}

// --- Data ---

func (e *EmuState) onData(class FrameClass, raw []byte) []Action {
	e.stats.Data++
	f, err := ParseData(e.key, raw)
	if err != nil {
		reason := rsnMalformed
		if errors.Is(err, ErrMICFail) {
			reason = rsnMICFail
			e.stats.MICFails++
		}
		src, dst := "", ""
		if len(raw) >= headerLen {
			dst, src = e.name(raw[3:11]), e.name(append(append([]byte{}, raw[11:16]...), EUISuffix...))
		}
		e.log.Log(class.String(), src, dst, reason, hex.EncodeToString(raw))
		return nil
	}

	srcEUI := f.SrcEUI()
	srcLbl, dstLbl := e.name(srcEUI), e.name(f.DstEUI)

	// Our own source prefix on air means the real bridge is transmitting, which
	// breaks the sole-coordinator assumption. Observe it, never answer it.
	if bytes.Equal(f.SrcPrefix, e.selfEUI[:5]) {
		e.bridgeFrames++
		if len(f.Plaintext) >= 8 {
			if c := binary.LittleEndian.Uint32(f.Plaintext[4:8]); c > e.maxBridgeCtr {
				e.maxBridgeCtr = c
			}
		}
		e.log.Log(class.String(), srcLbl, dstLbl, rsnBridgeSrc, f.Hex())
		return nil
	}

	ns := e.node(srcEUI)
	ns.LastSeen = e.cfg.Now()
	ns.LastSeq = f.Seq

	frag := IsFragment(f.Plaintext)
	app := ParseApp(f.Plaintext)
	_, icmp := UnicastICMPv6Type(f.Plaintext)
	if app == nil && !icmp {
		e.log.Log(class.String(), srcLbl, dstLbl, rsnAppParse, f.Hex())
	} else if app != nil {
		ns.RxCounter = app.Counter
		if !frag {
			// A fragment carries a 6LoWPAN fragment header where the CoAP packet
			// would be, so ParseApp's TLV heuristic yields noise there; the
			// reassembled payload is cached instead.
			e.cacheBody(ns, srcEUI, app.CoAP, app.TLVs, class, srcLbl, dstLbl, f.Hex())
		}
	} else {
		ns.RxCounter = binary.LittleEndian.Uint32(f.Plaintext[4:8])
		e.stats.ICMPv6++
	}

	// A frame addressed elsewhere is passively observed, never acknowledged.
	if !bytes.Equal(f.DstEUI, e.selfEUI) {
		e.log.Log(class.String(), srcLbl, dstLbl, rsnNotForUs, f.Hex())
		return nil
	}

	var actions []Action
	if !e.cfg.NoEnhAck {
		e.stats.EnhAcks++
		actions = append(actions, Action{
			Kind:   ActEnhAck,
			Bytes:  BuildEnhAck(f.Seq, srcEUI, e.selfEUI, e.cfg.CSLPhase),
			Reason: fmt.Sprintf("enh-ack seq=%02x", f.Seq),
			Node:   srcLbl,
		})
	}

	msg := app.coapOrNil()
	if frag {
		e.stats.Fragments++
		m, done := e.reasm.Add(f)
		if !done {
			return actions // more fragments to come; the MAC ACK is all that is due
		}
		e.stats.Reassembled++
		msg = m
		e.cacheBody(ns, srcEUI, m, nil, class, srcLbl, dstLbl, f.Hex())
	}

	if msg != nil {
		if id, ok := queryID(msg.Query); ok {
			ns.LastID = id
		}
		e.learnZone(msg, f)
		e.observeReports(msg, ns)
		e.observeFwState(msg, srcEUI)
		e.observeSession(ns, msg, class, srcLbl, dstLbl, f.Hex())
	}

	if msg == nil {
		if !icmp {
			e.log.Log(class.String(), srcLbl, dstLbl, rsnNoCoAP, f.Hex())
		}
	} else if reply, ok := e.handleCoAP(f, msg, class, srcLbl, dstLbl); ok {
		actions = append(actions, reply...)
	}
	if msg != nil || icmp {
		e.acceptedInboundIPv6 = append(e.acceptedInboundIPv6[:0], srcEUI...)
	}

	// Piggyback: any frame from this device means we are in a live rendezvous.
	if a, ok := e.nextDownlink(srcEUI, srcLbl); ok {
		actions = append(actions, a...)
	}
	return actions
}

// handleCoAP answers a CoAP uplink and logs whatever it cannot fully handle.
func (e *EmuState) handleCoAP(f *Frame, m *CoAPMessage, class FrameClass, srcLbl, dstLbl string) ([]Action, bool) {
	// A zone write teaches us the zone whether we answer it, forward it or only
	// overhear it: the thermostat pushes z/cpe as a NON aimed at the member, and
	// both paths below return before the answer handling.
	e.cacheZoneState(m, f.SrcEUI())
	// Zone traffic addressed to another device reaches us as an L2 hop only: the
	// bridge forwards it, it does not answer it. Answering as ourselves would
	// construct a relay reply for that device. This precedes the ACK handling because a
	// relayed 2.05 is exactly how a zone GET gets answered.
	if m.Contexted() && !e.addressedToSelf(m, f) {
		e.confirmByMID(f.SrcEUI(), m) // the device answered a request we forwarded
		// A member answering a GET we proxied on someone's behalf: its 2.05 goes
		// to the requester through the relay below, so we must not also answer.
		e.resolveZoneProxy(m.MID)
		e.relayZone(f, m, class, srcLbl, dstLbl)
		return nil, false
	}
	// A device ACK confirms the CoAP request we sent, ahead of any MAC evidence,
	// and if it answers a bridge-initiated GET it also carries the queried result.
	if m.Type == "ACK" {
		e.confirmByMID(f.SrcEUI(), m)
		return nil, false
	}
	if m.Type != "CON" || !isRequestCode(m.Code) {
		e.confirmByMID(f.SrcEUI(), m) // a NON 2.xx response to a queued GET, if any
		return nil, false             // NON telemetry: MAC ACK only, by design
	}
	if !knownResource(m.Path) {
		e.log.Log(class.String(), srcLbl, dstLbl, rsnUnknownRes+" path="+m.URI(), f.Hex())
	}
	if !knownMethod(m.Code) {
		e.log.Log(class.String(), srcLbl, dstLbl, rsnUnhandledMethod+" code="+m.Code, f.Hex())
	}
	if e.cfg.NoCoAPAck {
		return nil, false
	}
	// Anything else aimed at another node's address is not ours to answer, and
	// the zone forwarder above cannot carry it: it only speaks context 0.
	if !e.addressedToSelf(m, f) {
		e.log.Log(class.String(), srcLbl, dstLbl, rsnRelayedPath+" uri="+m.URI(), f.Hex())
		return nil, false
	}
	if m.MID == 0 {
		e.log.Log(class.String(), srcLbl, dstLbl, rsnNoMID, f.Hex())
		return nil, false
	}

	code := "2.04" // Changed, the bridge's default answer
	if m.Code == "0.01" {
		code = "2.05" // Content, for a GET
	}
	dst := f.SrcEUI()
	var pkt []byte
	var err error
	switch {
	case isDirectPath(m) && isAuthResource(m):
		e.queueAuthReply(m, dst, class, srcLbl, dstLbl, f.Hex())
		return nil, false
	case isDirectPath(m):
		// A GET of the heating-circuit config carries the served flow setpoint;
		// a resource the bridge holds (the RF key, the join surface, the report
		// endpoints) is served its own answer; every other direct GET/PUT gets
		// the bare 2.04/2.05.
		if cid, ok := servedFlowCircuit(m); ok {
			tempC := e.servedFlowC(cid)
			payload := flowSetpointPayload(tempC)
			if etag, present := m.Option(4); present && bytes.Equal(etag.Val, FlowSetpointETag(payload)) {
				code = "2.03"
				pkt, err = BuildCoAPAck(code, m.MID, m.Token, nil, e.selfEUI, dst, m.DstPort, m.SrcPort)
			} else {
				pkt, err = BuildFlowSetpointCoAP(cid, tempC, m.MID, m.Token, e.selfEUI, dst)
			}
			delete(e.flowFresh, cid)
			break
		}
		if p, sc, handled, serveErr := e.servedZoneConfig(m, dst); handled {
			pkt, code, err = p, sc, serveErr
			break
		}
		if p, sc, handled, serveErr := e.servedConfigGet(m, dst, srcLbl); handled {
			pkt, code, err = p, sc, serveErr
			break
		}
		if p, sc, handled, serveErr := e.servedSchedule(m, dst); handled {
			pkt, code, err = p, sc, serveErr
			break
		}
		if p, sc, handled, serveErr := e.servedDirectZoneState(m, dst); handled {
			pkt, code, err = p, sc, serveErr
			break
		}
		if p, ok := e.servedTime(m, dst); ok {
			pkt = p
			break
		}
		if sc, body, ok := e.servedGet(m, srcLbl, dst); ok {
			code = sc
			pkt, err = BuildCoAPAck(sc, m.MID, m.Token, body, e.selfEUI, dst, m.DstPort, m.SrcPort)
			break
		}
		switch normalizeResource(m.Path) {
		case "z/ow":
			code = "2.05" // the open-window report is answered with Content
			if zone, ok := zoneResourcePath(m.Path, e.inst, "ow"); ok && m.Code == "0.03" {
				e.owReports = append(e.owReports, OpenWindowReport{Zone: zone, Device: hexEUI(dst)})
			}
		case "z/overlay":
			code = "2.05" // the captured answer to a device overlay, with no body
			if zone, ok := zoneResourcePath(m.Path, e.inst, "overlay"); ok && m.Code == "0.03" {
				e.takeDeviceOverlay(zone, dst, m.Payload)
			}
		}
		pkt, err = BuildCoAPAck(code, m.MID, m.Token, nil, e.selfEUI, dst, m.DstPort, m.SrcPort)
	case m.Contexted():
		// A zone write addressed to us updates the zone's cached state and is
		// re-emitted to the zone's other members; a zone read is proxied to a
		// member when that is enabled, else answered from that cache.
		e.fanOutZone(f, m, class, srcLbl, dstLbl)
		if e.proxyZoneGet(f, m, class, srcLbl, dstLbl) {
			return nil, false
		}
		if body, opts, ok := e.servedZoneGet(m); ok {
			pkt, err = BuildContextAckBody(code, m.MID, m.Token, body, e.selfEUI, dst, m.DstPort, m.SrcPort, opts...)
			break
		}
		pkt, err = BuildContextAck(code, m.MID, m.Token, e.selfEUI, dst, m.DstPort, m.SrcPort)
	default:
		e.log.Log(class.String(), srcLbl, dstLbl, rsnUnknownAddrMode+" uri="+m.URI(), f.Hex())
		return nil, false
	}
	if err != nil {
		e.log.Log(class.String(), srcLbl, dstLbl, rsnBuildFail+" "+err.Error(), f.Hex())
		return nil, false
	}
	actions, err := e.coapReplyActions(pkt, dst, code, m.URI(), m.MID, srcLbl)
	if err != nil {
		e.log.Log(class.String(), srcLbl, dstLbl, rsnBuildFail+" "+err.Error(), f.Hex())
		return nil, false
	}
	if d := e.timeAnswerDelay(m); d > 0 {
		for i := range actions {
			actions[i].NotBefore = e.cfg.Now().Add(d)
			actions[i].Reason += fmt.Sprintf(" (fault-injection: held %s)", d)
		}
	}
	return actions, true
}

func (e *EmuState) coapReplyActions(pkt, dst []byte, code, uri string, mid uint16, node string) ([]Action, error) {
	reason := fmt.Sprintf("coap-ack %s %s mid=%04x", code, uri, mid)
	frameLen := headerLen + len(WrapAppPayload(pkt, 0, nil)) + micLen
	if frameLen <= 127 {
		ctr, err := e.nextTx()
		if err != nil {
			return nil, err
		}
		frame, err := BuildData(e.key, e.nextSeq(), dst, e.selfEUI[:5], WrapAppPayload(pkt, ctr, nil))
		if err != nil {
			return nil, err
		}
		e.stats.CoAPAcksSent++
		return []Action{{Kind: ActData, Bytes: frame.Bytes(), Reason: reason, Node: node, CSLTx: true}}, nil
	}

	fragTag := e.nextFragTag()
	fragments, err := Fragment6LoWPANDatagram(pkt, fragTag)
	if err != nil {
		return nil, err
	}
	if err := e.reserveTx(len(fragments)); err != nil {
		return nil, err
	}
	actions := make([]Action, 0, len(fragments))
	for i, fragment := range fragments {
		ctr, err := e.nextTx()
		if err != nil {
			return nil, err
		}
		frame, err := BuildDataPending(e.key, e.nextSeq(), dst, e.selfEUI[:5], WrapAppPayload(fragment, ctr, nil))
		if err != nil {
			return nil, err
		}
		actions = append(actions, Action{
			Kind: ActData, Bytes: frame.Bytes(), Reason: fmt.Sprintf("%s fragment=%d/%d", reason, i+1, len(fragments)), Node: node, CSLTx: true, HasCSLBatch: true, CSLBatchTag: fragTag,
		})
	}
	e.stats.CoAPAcksSent++
	return actions, nil
}

// servedGet returns the body the bridge serves on a link-local request and the
// response code to use. It covers held local state, the join surface, and
// z/legionella. dst is the requesting device's wire EUI.
func (e *EmuState) servedGet(m *CoAPMessage, srcLbl string, dst []byte) (code string, body []byte, ok bool) {
	switch res := normalizeResource(m.Path); res {
	case "d/lock":
		if m.Code == "0.01" {
			return "2.05", TLV{Tag: "0290", Val: []byte{boolByte(e.childLock[hexEUI(dst)])}}.Encode(), true
		}
	case "d/dispsettings":
		if m.Code == "0.01" {
			return "2.05", TLV{Tag: "013f", Val: []byte{boolByte(e.tempUnitF[hexEUI(dst)])}}.Encode(), true
		}
	case "z/legionella":
		return e.servedLegionella(m, srcLbl, dst)
	}
	return "", nil, false
}

func isAuthResource(m *CoAPMessage) bool {
	res := normalizeResource(m.Path)
	return res == "auth/token" || res == "auth/key"
}

// queueAuthReply answers auth/token and auth/key through the downlink queue.
// The device asks on a (re)join, when the bridge holds no CSL anchor for it, so
// the answer needs the bootstrap train and a retry after a missed Enhanced ACK:
// an answer that never lands leaves the device retransmitting the same MID.
// That retransmission gets the answer already built and serves no new session.
//
// auth/key is the mode-1 bootstrap of a device whose keystore id7 is
// unprovisioned: it presents its 12-byte ASCII device id in 0260 and, in 0262,
// AES-128 under its keystore id2 over that id followed by 4 zero bytes
// (join_auth_challenge_build@0x1e6ec). id2 never crosses the wire, so the
// challenge cannot be validated without dumping that device - and need not be:
// the device proves possession of id2 to us and never verifies us in return.
// Both requests therefore get the same session pair, which is what makes the
// device emit CoAP option 2048 on its later uplinks; an empty 2.05 leaves it with
// no session id at all. TLV 0261, the permanent id7 grant, stays absent: see
// BuildAuthTokenBody. Unproven on air for auth/key, no capture has a device with
// an unprovisioned id7.
func (e *EmuState) queueAuthReply(m *CoAPMessage, dst []byte, class FrameClass, srcLbl, dstLbl, hexFrame string) {
	ns := e.node(dst)
	key := hexEUI(dst)
	if ns.authReply != nil && ns.authMID == m.MID {
		for _, d := range e.queue[key] {
			if d.kind == "auth" && d.mid == m.MID {
				// The retransmission proves the device listens now.
				if !d.awaitingCSL {
					d.retryAt = time.Time{}
				}
				return
			}
		}
		e.pushAuthReply(key, m, ns.authReply)
		return
	}
	// The payload is a CoAP body, not an on-air frame: the frame column stays
	// empty so a replay of this log never feeds it back as a frame.
	e.log.Log("Data", srcLbl, "self",
		rsnAuthAccepted+" "+m.URI()+" payload="+hex.EncodeToString(m.Payload), "")
	sid := sessionID(dst)
	body, err := BuildAuthTokenBody(sid)
	if err != nil {
		body = nil // degrade to the empty answer rather than drop the join
	}
	pkt, err := BuildCoAPAck("2.05", m.MID, m.Token, body, e.selfEUI, dst, m.DstPort, m.SrcPort)
	if err != nil {
		e.log.Log(class.String(), srcLbl, dstLbl, rsnBuildFail+" "+err.Error(), hexFrame)
		return
	}
	if body != nil {
		ns.SessionID = sid
		e.sessions[key] = issuedSession{id: append([]byte(nil), sid...), at: e.cfg.Now()}
		e.sessionIssued = true
	}
	ns.SessionAdopted = false
	ns.sessionDrift = ""
	ns.authMID, ns.authReply = m.MID, pkt
	e.stats.CoAPAcksSent++
	e.pushAuthReply(key, m, pkt)
}

// pushAuthReply queues an auth answer ahead of every command that is not on the
// air yet: the device's later requests carry the session it establishes. An
// answer to an earlier MID is dropped even in flight: the device has abandoned
// that request and matches responses by MID only.
func (e *EmuState) pushAuthReply(key string, m *CoAPMessage, pkt []byte) {
	q := e.queue[key]
	kept := make([]*queuedDownlink, 0, len(q)+1)
	for _, d := range q {
		if d.kind == "auth" && d.mid != m.MID {
			e.recordDownlinkOutcome(key, d, "superseded", "newer-auth-request")
			continue
		}
		kept = append(kept, d)
	}
	at := 0
	if len(kept) > 0 && kept[0].inFlight {
		at = 1
	}
	d := &queuedDownlink{
		kind: "auth", desc: fmt.Sprintf("coap-ack 2.05 %s mid=%04x", m.URI(), m.MID),
		coap: pkt, key: e.key, mid: m.MID, responseRetry: true,
	}
	e.queue[key] = slices.Insert(kept, at, d)
}

// servedLegionella answers z/legionella. The device firmware implements the
// POST alone - there is no read path in it at all, and no capture ever requests
// one - so every other method is refused. The single record a POST carries is
// filed against the node, and one that names the right zone is answered 2.01
// even where the device itself would refuse the value.
func (e *EmuState) servedLegionella(m *CoAPMessage, srcLbl string, dev []byte) (string, []byte, bool) {
	if m.Code != "0.02" {
		return "4.05", nil, true
	}
	id, ok := queryID(m.Query)
	if !ok || !e.zoneIDMatches(dev, id, srcLbl) {
		return "4.00", nil, true
	}
	recs, err := parseAttrRecords(m.Payload, legionellaMaxValue)
	if err != nil {
		return "4.00", nil, true
	}
	for _, r := range recs {
		if r.Tag == legionellaTag {
			e.node(dev).Legionella = append([]byte{}, r.Val...)
		}
	}
	return "2.01", nil, true
}

// zoneIDMatches reports whether id names the zone the device answers to, which
// is the check the device runs against its own zone byte. A declared zone wins
// over the routing directory. Knowing no zone for the device at all, we accept
// and log instead of refusing: the directory is mostly learned from traffic, and
// rejecting on an empty one would break the exchange that fills it.
func (e *EmuState) zoneIDMatches(dev []byte, id int, srcLbl string) bool {
	if e.inst != nil {
		if d, err := e.inst.Resolve(hexEUI(dev)); err == nil && d.Zone != nil {
			return *d.Zone == id
		}
	}
	member := false
	for zone, members := range e.zones {
		if !members[hexEUI(dev)] {
			continue
		}
		if zone == id {
			return true
		}
		member = true
	}
	if member {
		return false
	}
	e.log.Log("Data", srcLbl, "self", rsnZoneIDUnverified+"="+strconv.Itoa(id), "")
	return true
}

// servedConfigGet answers a device's conditional GET of d/config with CoAP ETag
// validation, the way the bridge does: 2.03 Valid when the presented ETag still
// matches the config we hold for the device, otherwise 2.05 Content carrying the
// document and its ETag (option 4). ok=false when the request is not a d/config
// GET. The exchange follows the valve firmware contract (config_request_build /
// config_report_response_handle). The live logs of 2026-09-24 show valves
// pulling d/config from freedo, answered with 2.05 and with 2.03.
//
// The 2.05 body leaves out tag 015a, as every captured cloud answer does: the
// ETag travels in option 4 only. The valve stores the option's ETag after the
// parse (config_report_response_handle@0x1fb70), so a body 015a would only be
// overwritten, or, during one of its uploads, invalidate the ETag
// (config_doc_tlv_cb@0x1fac0).
//
// A device we hold no document for gets 5.03 with no body. An empty 2.05 is a
// success answer whose missing 015d no capture shows the effect of, and on the
// wall thermostat 015d selects the STM32 image it reflashes. An error answer
// carries no document to apply. How the thermostat retries after it is not
// captured.
func (e *EmuState) servedConfigGet(m *CoAPMessage, dst []byte, srcLbl string) ([]byte, string, bool, error) {
	if m.Code != "0.01" || normalizeResource(m.Path) != "d/config" {
		return nil, "", false, nil
	}
	snap, ok := e.cfgDoc[hexEUI(dst)]
	if !ok {
		e.noteConfigWithheld(dst, srcLbl)
		pkt, err := BuildCoAPAck(configUnavailable, m.MID, m.Token, nil, e.selfEUI, dst, m.DstPort, m.SrcPort)
		return pkt, configUnavailable, true, err
	}
	if et, ok := m.Option(4); ok && bytes.Equal(et.Val, snap.etag) {
		pkt, err := BuildCoAPAck("2.03", m.MID, m.Token, nil, e.selfEUI, dst, m.DstPort, m.SrcPort)
		return pkt, "2.03", true, err
	}
	body := encodeTLVs(withoutConfigETag(parseTLVList(snap.body)))
	pkt, err := e.buildBlock2Response(m, dst, body, snap.etag)
	return pkt, "2.05", true, err
}

// servedSchedule answers the valve's conditional zone schedule GET. The device
// owns the fetch and accepts a full weekly mask only after a 2.05 response.
//
// A device we hold no document for gets 2.03 Valid. The response handler
// (valve 0x2a344, thermostat 0x2934c) parses every 2.xx except 2.03 as the
// document: an empty or partial week writes cfg 0x2b4 = 0 and the stored
// program stops running. 2.03 keeps it and re-arms the fetch at 24 h; a 4.xx
// or 5.xx keeps it too, but re-arms the fetch every 10 min.
func (e *EmuState) servedSchedule(m *CoAPMessage, dst []byte) ([]byte, string, bool, error) {
	if m.Code != "0.01" || !schedulePath(m.Path, e.inst) {
		return nil, "", false, nil
	}
	snap, ok := e.schedDoc[hexEUI(dst)]
	if !ok {
		var opts []CoAPOption
		if et, ok := m.Option(4); ok {
			opts = append(opts, CoAPOption{Num: 4, Val: et.Val})
		}
		pkt, err := BuildCoAPAck("2.03", m.MID, m.Token, nil, e.selfEUI, dst, m.DstPort, m.SrcPort, opts...)
		return pkt, "2.03", true, err
	}
	if et, ok := m.Option(4); ok && bytes.Equal(et.Val, snap.etag) {
		pkt, err := BuildCoAPAck("2.03", m.MID, m.Token, nil, e.selfEUI, dst, m.DstPort, m.SrcPort)
		return pkt, "2.03", true, err
	}
	pkt, err := e.buildBlock2Response(m, dst, snap.body, snap.etag)
	return pkt, "2.05", true, err
}

func schedulePath(path string, in *Install) bool {
	if normalizeResource(path) == "d/sched" {
		return true
	}
	_, ok := zoneResourcePath(path, in, "sched")
	return ok
}

// servedDirectZoneState answers GET h/<home>/z/N/s. The device's response
// handler (valve 0x11984, thermostat 0x116fc) writes each tag of any 2.xx body
// into its zone config and keeps every tag the body leaves out, so an empty
// 2.05 and a 2.03 change nothing. A 4.xx or 5.xx is dropped. The GET carries
// no ETag (app_node_notify_build, valve 0x118e4), so there is none to validate.
func (e *EmuState) servedDirectZoneState(m *CoAPMessage, dst []byte) ([]byte, string, bool, error) {
	zone, ok := zoneResourcePath(m.Path, e.inst, "s")
	if m.Code != "0.01" || !ok {
		return nil, "", false, nil
	}
	body := e.directZoneReadBody(zone)
	pkt, err := BuildCoAPAck("2.05", m.MID, m.Token, body, e.selfEUI, dst, m.DstPort, m.SrcPort)
	return pkt, "2.05", true, err
}

// directZoneReadBody is the block a device reads with GET h/<home>/z/N/s. The
// cloud answers the wall thermostat and a valve alike, in the downlink tag
// order of every captured read (capture-app-20260907), with no 6440:
//
//	6160 6180 6020 61e0 [6200] 6240 [6260 6280] 62e0
//
// The head is the home state and the program block a zone rewrite carries, so
// the read restates them instead of the cached ones, which a later home state
// change leaves stale. 6260 and 6280 follow an overlay only. The cache holds
// the zone-plane order.
//
// A hot-water zone with no cached block still reads 6240=0 and 62e0=0, as
// every cloud read of zone 0 carries them. The thermostat enables DHW from
// zone_state_timer_cb (0x2d70c), which skips a zone whose 6240 is unset, as
// after a restart: without the tag the DHW enable never goes out.
func (e *EmuState) directZoneReadBody(zone int) []byte {
	body := e.zoneState[zoneKey(zone, "z/s")]
	if len(body) == 0 && !(e.homeSet && e.zoneKind(zone) == dhwZoneKind) {
		return e.uncachedZoneReadBody(zone)
	}
	values := make(map[string][]byte)
	for _, tlv := range parseTLVList(body) {
		values[tlv.Tag] = append([]byte(nil), tlv.Val...)
	}
	cached := func(tag string, absent byte) []byte {
		if value := values[tag]; len(value) > 0 {
			return value
		}
		return []byte{absent}
	}
	read := e.zoneReadHead(zone, values["6200"])
	overlay := cached("6240", overlayAuto)
	read = append(read, TLV{Tag: "6240", Val: overlay})
	if overlay[0] != overlayAuto {
		read = append(read, TLV{Tag: "6260", Val: cached("6260", 0x01)})
		// The setpoint is the one field with no captured constant behind it, so a
		// zone we hold no value for reads without it.
		if setpoint := values["6280"]; len(setpoint) > 0 {
			read = append(read, TLV{Tag: "6280", Val: setpoint})
		}
	}
	window := cached("62e0", 0x00)
	if open, driven := e.windowOpen[zone]; driven {
		window = []byte{boolByte(open)}
	}
	return encodeTLVs(append(read, TLV{Tag: "62e0", Val: window}))
}

// zoneReadHead is the zone rewrite head of a read. 6200 comes from the program
// the owner pushed, else from the zone cache, never from the requester: the
// wall thermostat sits in two zones, so its own last 6200 may belong to the
// other one. With no home state pushed the head leaves out 6160, 61e0 and 6200,
// and the device keeps its own.
func (e *EmuState) zoneReadHead(zone int, cachedSchedC []byte) []TLV {
	schedC, known := e.schedC[zone]
	head := zoneWriteHead(zoneWrite{home: e.zoneHome(zone), programOff: e.schedOff[zone], schedC: schedC, kind: e.zoneKind(zone)})
	switch {
	case !e.homeSet:
		return withoutTag(withoutTag(withoutTag(head, "6160"), "61e0"), "6200")
	case known:
		return head
	case len(cachedSchedC) > 0:
		for i := range head {
			if head[i].Tag == "6200" {
				head[i].Val = cachedSchedC
			}
		}
		return head
	}
	return withoutTag(head, "6200")
}

// uncachedZoneReadBody is the read of a heating zone with no cached block: the
// home state and the program block a zone rewrite would carry. The overlay tags
// stay out, and so does a 6200 or a 62e0 the owner does not hold, because the
// device keeps a tag the read omits. With no home state pushed the read stays
// empty.
func (e *EmuState) uncachedZoneReadBody(zone int) []byte {
	if !e.homeSet {
		return nil
	}
	read := e.zoneReadHead(zone, nil)
	if open, driven := e.windowOpen[zone]; driven {
		read = append(read, TLV{Tag: "62e0", Val: []byte{boolByte(open)}})
	}
	return encodeTLVs(read)
}

func (e *EmuState) servedZoneConfig(m *CoAPMessage, dst []byte) ([]byte, string, bool, error) {
	zone, ok := zoneConfigPath(m.Path, e.inst)
	if m.Code != "0.01" || !ok {
		return nil, "", false, nil
	}
	doc, err := BuildZoneConfigDocument(e.inst, zone)
	if err != nil {
		return nil, "", true, err
	}
	if etag, present := m.Option(4); present && bytes.Equal(etag.Val, doc.ETag) {
		// The captured 2.03 Valid echoes the ETag it validates.
		pkt, err := BuildCoAPAck("2.03", m.MID, m.Token, nil, e.selfEUI, dst, m.DstPort, m.SrcPort,
			CoAPOption{Num: 4, Val: doc.ETag})
		return pkt, "2.03", true, err
	}
	pkt, err := e.buildBlock2Response(m, dst, doc.Body, doc.ETag)
	return pkt, "2.05", true, err
}

func (e *EmuState) buildBlock2Response(m *CoAPMessage, dst, body, etag []byte) ([]byte, error) {
	offset, size, szx, err := requestedBlock2(m)
	if err != nil {
		return nil, err
	}
	if offset > len(body) {
		return nil, errors.New("tado: Block2 offset past document")
	}
	end := offset + size
	if end > len(body) {
		end = len(body)
	}
	block := uint32(offset/size)<<4 | uint32(szx)
	if end < len(body) {
		block |= 0x08
	}
	token := m.Token
	if token == nil {
		token = []byte{}
	}
	return BuildCoAP(CoAPReq{
		Type: "ACK", Code: "2.05", MID: m.MID, Token: token,
		Options: []CoAPOption{
			{Num: 4, Val: etag},
			{Num: 12, Val: []byte{42}},
			{Num: 23, Val: encodeCoAPUint(block)},
		},
		Payload: body[offset:end], SrcEUI: e.selfEUI, DstEUI: dst,
		SrcPort: m.DstPort, DstPort: m.SrcPort,
	})
}

func zoneConfigPath(path string, in *Install) (int, bool) {
	return zoneResourcePath(path, in, "config")
}

func zoneResourcePath(path string, in *Install, resource string) (int, bool) {
	if in == nil {
		return 0, false
	}
	segs := strings.Split(path, "/")
	if len(segs) != 5 || segs[0] != "h" || segs[1] != strconv.FormatUint(uint64(in.HomeID), 10) || segs[2] != "z" || segs[4] != resource {
		return 0, false
	}
	zone, err := strconv.Atoi(segs[3])
	return zone, err == nil
}

func requestedBlock2(m *CoAPMessage) (offset, size int, szx byte, err error) {
	size, szx = 128, 3
	o, ok := m.Option(23)
	if !ok {
		return 0, size, szx, nil
	}
	if len(o.Val) > 3 {
		return 0, 0, 0, errors.New("tado: Block2 option is longer than 3 bytes")
	}
	v := decodeCoAPUint(o.Val)
	reqSZX := byte(v & 7)
	if reqSZX == 7 {
		return 0, 0, 0, errors.New("tado: Block2 BERT is invalid over UDP")
	}
	reqSize := 1 << (reqSZX + 4)
	offset = int(v>>4) * reqSize
	if reqSize < size {
		size, szx = reqSize, reqSZX
	}
	return offset, size, szx, nil
}

func decodeCoAPUint(b []byte) uint32 {
	var v uint32
	for _, x := range b {
		v = v<<8 | uint32(x)
	}
	return v
}

func encodeCoAPUint(v uint32) []byte {
	switch {
	case v == 0:
		return nil
	case v <= 0xff:
		return []byte{byte(v)}
	case v <= 0xffff:
		return []byte{byte(v >> 8), byte(v)}
	default:
		return []byte{byte(v >> 16), byte(v >> 8), byte(v)}
	}
}

// servedTime answers a device's GET of "time" with the wall clock. The bridge
// itself does not implement the resource - it relays it to the cloud - so the
// answer reproduces the cloud's wire form: a 2.05 with no options and a 5-byte
// nanopb body. It builds its own datagram because BuildCoAPAck would append a
// Content-Format option to a non-empty payload.
//
// The answer is emitted synchronously with the request on purpose: the device
// discards a "time" response older than 5 s
// (coap_rx_timed_response_dispatch@0x22748), so it can never be queued for a
// later rendezvous. Without it the device clock is never set, rt_time_now_get
// returns -3 and the on-device weekly schedule is never evaluated.
// Two fault-injection knobs override that, and only for test F12: SuppressTime
// drops the answer entirely, TimeDelay holds it past the deadline. Both default
// to off.
func (e *EmuState) servedTime(m *CoAPMessage, dst []byte) ([]byte, bool) {
	if m.Code != "0.01" || normalizeResource(m.Path) != "time" {
		return nil, false
	}
	if e.cfg.SuppressTime {
		e.stats.TimeSuppressed++
		e.log.Log(ClassData.String(), e.name(dst), "", rsnTimeSuppressed+fmt.Sprintf(" mid=%04x", m.MID), "")
		return nil, false
	}
	pkt, err := BuildTimeCoAP(e.cfg.Now(), m.MID, m.Token, e.selfEUI, dst, m.DstPort, m.SrcPort)
	if err != nil {
		return nil, false
	}
	return pkt, true
}

// timeAnswerDelay is how long an answer to "time" is held back before going out.
// Zero on the normal path: the answer has to be synchronous or the device
// discards it. Non-zero only under the F12 fault injection.
func (e *EmuState) timeAnswerDelay(m *CoAPMessage) time.Duration {
	if e.cfg.TimeDelay <= 0 || m.Code != "0.01" || normalizeResource(m.Path) != "time" {
		return 0
	}
	e.stats.TimeDelayed++
	e.log.Log(ClassData.String(), "", "", rsnTimeDelayed+fmt.Sprintf(" mid=%04x by=%s", m.MID, e.cfg.TimeDelay), "")
	return e.cfg.TimeDelay
}

// sessionID derives the session id served on auth/token. We are the authority,
// so the value is ours to pick and only has to be stable: a hash of the device's
// wire EUI keeps it identical across emulator restarts, which is what makes the
// option-2048 echo verifiable. 8 bytes is what the real cloud sends, inside the
// 4..8 the firmware accepts.
func sessionID(wireEUI []byte) []byte {
	h := fnv.New64a()
	h.Write(wireEUI)
	return h.Sum(nil)
}

type issuedSession struct {
	id []byte
	at time.Time
}

// IssuedSession returns when a session id was last served to the device with
// the given wire EUI hex, in this run or a stored earlier one.
func (e *EmuState) IssuedSession(wireHex string) (time.Time, bool) {
	s, ok := e.sessions[wireHex]
	return s.at, ok
}

// TakeSessionIssued reports whether a session id was served since the last
// call, so the owner persists it.
func (e *EmuState) TakeSessionIssued() bool {
	issued := e.sessionIssued
	e.sessionIssued = false
	return issued
}

// observeSession records the option-2048 value a device echoes on its uplinks
// and tracks whether it adopted the session id we served it. The device copies
// the auth/token 025e verbatim into that option on every request, so a request
// without it or with another value means our answer never landed. Responses
// carry no session id. Only a change of state is logged, so a steady session
// costs no log line.
func (e *EmuState) observeSession(ns *NodeState, m *CoAPMessage, class FrameClass, srcLbl, dstLbl, hexFrame string) {
	o, ok := m.Option(coapSessionOption)
	present := ok && len(o.Val) > 0
	changed := present && !bytes.Equal(ns.Option2048, o.Val)
	if changed {
		ns.Option2048 = append([]byte{}, o.Val...)
	}
	if len(ns.SessionID) == 0 || !isRequestCode(m.Code) || isAuthResource(m) {
		return
	}
	switch {
	case !present:
		if ns.sessionDrift != "missing" {
			ns.SessionAdopted, ns.sessionDrift = false, "missing"
			e.log.Log(class.String(), srcLbl, dstLbl, rsnSessionMissing+" uri="+m.URI(), hexFrame)
		}
	case !bytes.Equal(ns.SessionID, o.Val):
		if ns.sessionDrift != "mismatch" || changed {
			ns.SessionAdopted, ns.sessionDrift = false, "mismatch"
			e.log.Log(class.String(), srcLbl, dstLbl, rsnSessionMismatch+"="+hex.EncodeToString(o.Val), hexFrame)
		}
	case !ns.SessionAdopted:
		ns.SessionAdopted, ns.sessionDrift = true, ""
		e.log.Log(class.String(), srcLbl, dstLbl, rsnSessionAdopted+" uri="+m.URI(), "")
	}
}

func (e *EmuState) recordGetResult(srcEUI []byte, m *CoAPMessage) (complete, accepted bool) {
	if (m.Type != "ACK" && m.Type != "NON") || !isResponseCode(m.Code) {
		return false, false
	}
	pending, ok := e.pendGet[m.MID]
	if !ok || !strings.HasPrefix(pending.resultKey, hexEUI(srcEUI)+"|") {
		return false, false
	}
	blockNum, more, szx, hasBlock, err := responseBlock2(m)
	if err != nil {
		return false, false
	}
	if hasBlock {
		if m.Code != "2.05" || blockNum != pending.blockNum || (pending.blockNum > 0 && szx != pending.blockSZX) {
			return false, false
		}
		if more {
			if !e.advanceActiveGet(srcEUI, m.MID, pending, szx, m.Payload) {
				return false, false
			}
			return false, true
		}
	} else if pending.blockNum != 0 {
		return false, false
	}
	pending.body = append(pending.body, m.Payload...)
	e.getRes[pending.resultKey] = append([]byte{}, pending.body...)
	e.getCode[pending.resultKey] = m.Code
	if m.Code == "2.05" && pending.resultKey == hexEUI(srcEUI)+"|"+fwStateResource {
		e.noteFwState(e.fwGet, hexEUI(srcEUI), pending.body)
	}
	delete(e.pendGet, m.MID)
	return true, true
}

func responseBlock2(m *CoAPMessage) (num uint32, more bool, szx byte, present bool, err error) {
	o, present := m.Option(23)
	if !present {
		return 0, false, 0, false, nil
	}
	if len(o.Val) > 3 {
		return 0, false, 0, true, errors.New("tado: Block2 option is longer than 3 bytes")
	}
	v := decodeCoAPUint(o.Val)
	szx = byte(v & 7)
	if szx == 7 {
		return 0, false, 0, true, errors.New("tado: Block2 BERT is invalid over UDP")
	}
	return v >> 4, v&8 != 0, szx, true, nil
}

func (e *EmuState) advanceActiveGet(srcEUI []byte, oldMID uint16, pending *activeGet, szx byte, body []byte) bool {
	key := hexEUI(srcEUI)
	q := e.queue[key]
	if len(q) == 0 || !q[0].inFlight || q[0].kind != "get" || q[0].mid != oldMID {
		return false
	}
	mid, err := randMID()
	if err != nil {
		return false
	}
	block := (pending.blockNum+1)<<4 | uint32(szx)
	pkt, err := BuildGetBlock2CoAP(pending.path, pending.query, block, mid, pending.token, e.selfEUI, srcEUI)
	if err != nil {
		return false
	}
	pending.body = append(pending.body, body...)
	pending.blockNum++
	pending.blockSZX = szx
	delete(e.pendGet, oldMID)
	e.pendGet[mid] = pending
	d := q[0]
	d.coap = pkt
	d.mid = mid
	d.tries = 0
	d.inFlight = false
	d.retryAt = time.Time{}
	d.deferred = false
	return true
}

func isResponseCode(code string) bool {
	return len(code) == 4 && code[0] != '0' && code[1] == '.'
}

// cacheBody caches the TLVs a received message carries. A block-wise response
// reaches the TLV parser only once, whole: a block boundary falls anywhere, so a
// single block can end in the middle of a TLV. fallback carries the TLV-suffix
// heuristic of a plaintext that holds no CoAP packet.
func (e *EmuState) cacheBody(ns *NodeState, srcEUI []byte, m *CoAPMessage, fallback []TLV, class FrameClass, srcLbl, dstLbl, hexFrame string) {
	if m != nil && e.staleZoneEcho(m) {
		return
	}
	if m == nil {
		e.cacheTLVs(ns, fallback, "", class, srcLbl, dstLbl, hexFrame)
		return
	}
	num, more, szx, present, err := responseBlock2(m)
	if err != nil || !present || !isResponseCode(m.Code) {
		tlvs := parseTLVList(m.Payload)
		e.cacheTLVs(ns, tlvs, m.URI(), class, srcLbl, dstLbl, hexFrame)
		e.observeModuleID(srcEUI, m, tlvs)
		return
	}
	body, done, err := e.block2.Add(block2Key(srcEUI, m), num, more, szx, m.Payload)
	if err != nil {
		e.log.Log(class.String(), srcLbl, dstLbl, rsnBlock2Dropped+"="+err.Error(), hexFrame)
		return
	}
	if !done {
		return
	}
	tlvs := parseTLVList(body)
	e.cacheTLVs(ns, tlvs, m.URI(), class, srcLbl, dstLbl, hexFrame)
	e.observeModuleID(srcEUI, m, tlvs)
}

func (e *EmuState) cacheTLVs(ns *NodeState, tlvs []TLV, uri string, class FrameClass, srcLbl, dstLbl, hexFrame string) {
	res := ResourceOf(uri, tlvs)
	for _, t := range tlvs {
		ns.raw[t.Tag] = append([]byte{}, t.Val...)
		if t.Tag == "0162" {
			e.noteBattery(ns, rawInt(t.Val))
		}
		info, ok := tagFor(t.Tag, res)
		if !ok {
			ns.Telemetry[t.Tag] = hex.EncodeToString(t.Val)
			e.log.Log(class.String(), srcLbl, dstLbl, rsnUnknownTag+"="+t.Tag, hexFrame)
			continue
		}
		ns.Telemetry[info.name] = info.decode(t.Val)
	}
	ns.noteHVAC(tlvs, uri)
	ns.refreshLastKnown()
}

// refreshLastKnown re-reads the typed heating block from the raw TLV cache.
func (ns *NodeState) refreshLastKnown() {
	if v := ns.raw["6280"]; len(v) > 0 {
		ns.LastSetpointC = float64(be(v)) / 100.0
	}
	if v := ns.raw["6200"]; len(v) > 0 {
		ns.LastSchedC = float64(be(v)) / 100.0
	}
	if v := ns.raw["6240"]; len(v) > 0 {
		ns.LastOverlay = v[0]
	}
	if v := ns.raw["62e0"]; len(v) > 0 {
		ns.LastOpenWindow = be(v) != 0
	}
}

// --- zone plane ---
//
// Zone traffic is peer-to-peer at the application layer and the bridge is its
// router. A device compresses both endpoints against 6LoWPAN context 0, puts the
// zone peer's own IPv6 address in the destination and sends the frame to us at
// L2; we forward it to that peer. Membership itself is cloud state, so the
// directory below is either declared (EmuConfig.Zones) or learned from the
// traffic we route.

// zoneResources are the resources whose body is a zone's running state. The
// bridge fans those out and can answer a GET of them from its cache.
var zoneResources = map[string]bool{"z/s": true, "z/p": true, "z/cpe": true}

// AddZoneMember declares target (serial, short address, room or wire EUI) a
// member of zone.
func (e *EmuState) AddZoneMember(zone int, target string) error {
	wire, _, err := e.resolve(target)
	if err != nil {
		return err
	}
	e.addZoneEUI(zone, wire)
	return nil
}

// ZoneDirectory returns the whole routing table: zone -> member labels, sorted.
func (e *EmuState) ZoneDirectory() map[int][]string {
	out := make(map[int][]string, len(e.zones))
	for zone := range e.zones {
		out[zone] = e.ZoneMembers(zone)
	}
	return out
}

// ZoneMembers returns the members of zone as short addresses, sorted. The short
// address rather than the room name: two devices of one room share the name.
func (e *EmuState) ZoneMembers(zone int) []string {
	out := make([]string, 0, len(e.zones[zone]))
	for k := range e.zones[zone] {
		b, err := hex.DecodeString(k)
		if err != nil {
			continue
		}
		out = append(out, ShortFromWire(b))
	}
	sort.Strings(out)
	return out
}

func (e *EmuState) addZoneEUI(zone int, wire []byte) {
	if len(wire) != 8 || bytes.Equal(wire, e.selfEUI) {
		return
	}
	if e.zones[zone] == nil {
		e.zones[zone] = map[string]bool{}
	}
	e.zones[zone][hexEUI(wire)] = true
}

// learnZone files the endpoints of a "lid=N" datagram as members of zone N. Zone
// numbers are cloud-allocated, so in a live deployment this is how the directory
// is built: whoever sends or answers on a zone belongs to it, and so does the
// peer the datagram is addressed to.
func (e *EmuState) learnZone(m *CoAPMessage, f *Frame) {
	zone, ok := zoneQuery(m.Query)
	if !ok {
		return
	}
	e.addZoneEUI(zone, f.SrcEUI())
	if w := wireFromIID(m.Dst.IID); w != nil {
		e.addZoneEUI(zone, w)
	}
}

// zoneQuery extracts N from a "lid=N" query. The "id=N" form is the bridge's own
// command plane - one physical device, link-local - and is not a zone.
func zoneQuery(query string) (int, bool) {
	for _, item := range strings.Split(query, "&") {
		if !strings.HasPrefix(item, "lid=") {
			continue
		}
		if n, err := strconv.Atoi(item[len("lid="):]); err == nil {
			return n, true
		}
	}
	return 0, false
}

// wireFromIID inverts modifiedEUI64: the interface identifier an IPHC header
// carries, back to the wire (little-endian) address it names. It reports nil for
// an identifier outside this PAN, so a stray destination is never forwarded to.
func wireFromIID(iid []byte) []byte {
	if len(iid) != 8 {
		return nil
	}
	wire := make([]byte, 8)
	for i := range wire {
		wire[i] = iid[7-i]
	}
	wire[7] ^= 0x02
	if !bytes.Equal(wire[5:], EUISuffix) {
		return nil
	}
	return wire
}

// relayZone forwards one zone datagram to the node its IPv6 destination names.
// The datagram goes out verbatim - same MID, token, options and payload, source
// address preserved, hop limit decremented - which is what the real bridge does:
// the second hop of every relayed pair in the capture is byte-identical to what
// BuildZoneRelay emits, checksum included. The target is a sleepy node, so the
// frame is queued and rides out at its next rendezvous instead of being
// transmitted into the void.
func (e *EmuState) relayZone(f *Frame, m *CoAPMessage, class FrameClass, srcLbl, dstLbl string) {
	if e.cfg.NoAutoQueue {
		return
	}
	dst := wireFromIID(m.Dst.IID)
	if dst == nil || bytes.Equal(dst, e.selfEUI) {
		e.log.Log(class.String(), srcLbl, dstLbl, rsnZoneUnroutable+" uri="+m.URI(), f.Hex())
		return
	}
	srcIID := m.Src.IID
	if srcIID == nil {
		srcIID = modifiedEUI64(f.SrcEUI())
	}
	pkt, ok := e.buildZoneHop(f, m, srcIID, dst, class, srcLbl, dstLbl)
	if !ok {
		return
	}
	e.stats.ZoneRelays++
	e.queueZoneHop(dst, m, pkt, fmt.Sprintf("zone-relay %s %s mid=%04x %s->%s",
		m.Type, m.Code, m.MID, srcLbl, e.name(dst)), f.SrcEUI())
}

// fanOutZone re-emits a zone PUT addressed to us to the zone's other members.
//
// The captured bridge never needs this: a device addresses its zone peer itself
// and we forward that (relayZone), while the copy a device sends to the bridge
// is the one meant for the cloud, answered with a bare 2.04. We fan it out
// anyway, because it is the only path that reaches a member the sender does not
// address directly - and the zone's demand has to reach the wall thermostat for
// the boiler to fire. A member that also got the sender's own copy sees the same
// TLVs twice under two MIDs, which is idempotent state. EmuConfig.NoZoneFanOut
// turns it off.
func (e *EmuState) fanOutZone(f *Frame, m *CoAPMessage, class FrameClass, srcLbl, dstLbl string) {
	if e.cfg.NoZoneFanOut || e.cfg.NoAutoQueue || m.Code != "0.03" {
		return
	}
	zone, ok := zoneQuery(m.Query)
	if !ok || !zoneResources[normalizeResource(m.Path)] {
		return
	}
	src := f.SrcEUI()
	srcIID := modifiedEUI64(src)
	for _, member := range sortedKeys(e.zones[zone]) {
		dst, err := hex.DecodeString(member)
		if err != nil || bytes.Equal(dst, src) {
			continue
		}
		pkt, ok := e.buildZoneHop(f, m, srcIID, dst, class, srcLbl, dstLbl)
		if !ok {
			return
		}
		e.stats.ZoneFanouts++
		e.queueZoneHop(dst, m, pkt, fmt.Sprintf("zone-fanout %s mid=%04x %s->%s",
			m.URI(), m.MID, srcLbl, e.name(dst)), src)
	}
}

// buildZoneHop serializes one re-emission of m towards dst, keeping srcIID as
// the IPv6 source. It refuses a datagram whose inline checksum we cannot
// reproduce: a reassembled packet keeps only its first fragment in Raw, and
// forwarding that would put a truncated body under a checksum that no longer
// covers it.
func (e *EmuState) buildZoneHop(f *Frame, m *CoAPMessage, srcIID, dst []byte, class FrameClass, srcLbl, dstLbl string) ([]byte, bool) {
	if ck, ok := m.RecomputeChecksum(f.SrcEUI(), f.DstEUI); !ok || ck != m.Checksum {
		e.log.Log(class.String(), srcLbl, dstLbl, rsnZoneIncomplete+" uri="+m.URI(), f.Hex())
		return nil, false
	}
	msg, err := StampSessionID(m.Raw, e.relaySessionID(m, srcIID))
	if err != nil {
		e.log.Log(class.String(), srcLbl, dstLbl, rsnBuildFail+" "+err.Error(), f.Hex())
		return nil, false
	}
	pkt, err := BuildZoneRelay(msg, srcIID, dst, m.SrcPort, m.DstPort)
	if err != nil {
		e.log.Log(class.String(), srcLbl, dstLbl, rsnBuildFail+" "+err.Error(), f.Hex())
		return nil, false
	}
	return pkt, true
}

// relaySessionID names the author of a message we forward. A request carries the
// originating device's session id: the one it echoes to us, or the 025e we
// served it when it has echoed none yet. An answer carries no session id at all,
// which is what the capture shows on every relayed 2.xx.
func (e *EmuState) relaySessionID(m *CoAPMessage, srcIID []byte) []byte {
	if isResponseCode(m.Code) {
		return nil
	}
	author := wireFromIID(srcIID)
	if author == nil {
		return nil
	}
	if ns, ok := e.nodes[hexEUI(author)]; ok && len(ns.Option2048) > 0 {
		return ns.Option2048
	}
	return sessionID(author)
}

// queueZoneHop queues one re-emitted zone datagram against its target, keyed by
// the wire EUI of its origin: two devices can share a label.
//
// A copy of a message still queued (same origin, type, code and MID) is
// collapsed into it: it is the origin's CoAP retransmission, or the peer's
// repeated answer to one, and the target deduplicates by MID anyway. A newer
// datagram on the same route supersedes a queued one that has not gone out yet:
// zone state is a running value, so a sleepy target must not wake up to a
// backlog of stale updates. A request's route is its resource; a NON observe
// notification's route is its token. An ACK or RST goes ahead of the queued
// datagrams that are not on the air: until it lands, the target retransmits
// its CON and every retransmission costs a relay round trip.
func (e *EmuState) queueZoneHop(dst []byte, m *CoAPMessage, pkt []byte, desc string, origin []byte) {
	src := hexEUI(origin)
	id := fmt.Sprintf("%s|%s|%s|%04x", src, m.Type, m.Code, m.MID)
	key := hexEUI(dst)
	q := e.queue[key]
	for _, d := range q {
		if d.kind == "zone" && d.relayID == id {
			e.stats.ZoneRelaysCollapsed++
			return
		}
	}
	route := zoneHopRoute(src, m)
	kept := q[:0]
	for _, d := range q {
		if d.kind == "zone" && route != "" && d.route == route && !d.inFlight {
			e.recordDownlinkOutcome(key, d, "superseded", "newer-zone-state")
			continue
		}
		kept = append(kept, d)
	}
	hop := &queuedDownlink{
		kind: "zone", desc: desc, coap: pkt, key: e.key, mid: m.MID, route: route,
		relayID: id, relayAck: m.Type == "ACK" || m.Type == "RST",
		responseRetry: m.Type == "NON" && strings.HasPrefix(m.Code, "2."),
	}
	at := len(kept)
	if hop.relayAck {
		at = 0
		for at < len(kept) && (kept[at].inFlight || kept[at].relayAck) {
			at++
		}
	}
	e.queue[key] = e.trimZoneHops(key, slices.Insert(kept, at, hop))
}

// zoneHopRoute names what a queued zone datagram carries, so a newer one on
// the same route replaces it. The method is part of a request's route: a
// proxied GET of a resource carries no state that replaces a queued write of
// the same one. Only a NON notification supersedes by token: a CON one is
// retransmitted under its own MID, and that copy must not replace a newer one.
func zoneHopRoute(origin string, m *CoAPMessage) string {
	if m.Path != "" {
		return origin + "|" + m.Code + "|" + m.URI()
	}
	if _, observe := m.Option(6); observe && m.Type == "NON" && isResponseCode(m.Code) && len(m.Token) > 0 {
		return origin + "|notify|" + hex.EncodeToString(m.Token)
	}
	return ""
}

// trimZoneHops drops the oldest waiting zone datagram that is not an ACK while
// the target holds more than maxZoneHopsPerTarget of them.
func (e *EmuState) trimZoneHops(key string, q []*queuedDownlink) []*queuedDownlink {
	n := 0
	for _, d := range q {
		if d.kind == "zone" {
			n++
		}
	}
	for i := 0; n > maxZoneHopsPerTarget && i < len(q); {
		d := q[i]
		if d.kind != "zone" || d.inFlight || d.relayAck {
			i++
			continue
		}
		e.stats.DownlinksDropped++
		e.recordDownlinkOutcome(key, d, "dropped", "zone-backlog")
		q = slices.Delete(q, i, i+1)
		n--
	}
	return q
}

// cacheZoneState merges a zone PUT into the held resource state. Zone reports
// carry changed attributes, so replacing the body would discard older fields.
func (e *EmuState) cacheZoneState(m *CoAPMessage, src []byte) {
	zone, ok := zoneQuery(m.Query)
	res := normalizeResource(m.Path)
	if !ok || m.Code != "0.03" || len(m.Payload) == 0 || !zoneResources[res] {
		return
	}
	updates := parseTLVList(m.Payload)
	if len(updates) == 0 {
		return
	}
	if res == "z/s" {
		stale := e.staleZoneEcho(m)
		e.noteZoneStateReport(zone, src, m.Payload, stale)
		if stale {
			return
		}
		e.adoptZoneReport(zone, updates)
	}
	e.storeZoneState(zone, res, encodeTLVs(mergeConfigTLVs(parseTLVList(e.zoneState[zoneKey(zone, res)]), updates)))
}

// storeZoneState replaces a cached zone body. A changed z/s block is marked
// for the owner to persist: after a restart the window engine and the zone
// rewrites restate the overlay from it.
func (e *EmuState) storeZoneState(zone int, res string, body []byte) {
	key := zoneKey(zone, res)
	if res == "z/s" && !bytes.Equal(e.zoneState[key], body) {
		e.zoneStateChanged = true
	}
	e.zoneState[key] = body
}

// TakeZoneStateChanged reports whether a cached z/s block changed since the
// last call, so the owner persists it.
func (e *EmuState) TakeZoneStateChanged() bool {
	changed := e.zoneStateChanged
	e.zoneStateChanged = false
	return changed
}

// proxyZoneGet forwards a zone GET addressed to us to one of the zone's other
// members, keeping the requester as the IPv6 source so the member answers that
// address directly; its 2.05 comes back through relayZone under the original
// MID. It reports whether the request was proxied, and a proxied request must
// not be answered by us as well: both replies would carry the same MID, the
// requester keeps whichever lands first and ours - built synchronously - would
// win, which is exactly the outcome the proxy exists to avoid.
//
// Off by default (EmuConfig.ZoneGetProxy). Knowing no live member we fall back
// to answering, rather than leaving the requester with nothing. A member that
// takes the request and never replies is covered by ExpireZoneProxies, which
// answers the requester itself once ZoneProxyTimeout has passed.
func (e *EmuState) proxyZoneGet(f *Frame, m *CoAPMessage, class FrameClass, srcLbl, dstLbl string) bool {
	if !e.cfg.ZoneGetProxy || e.cfg.NoAutoQueue || m.Code != "0.01" {
		return false
	}
	zone, ok := zoneQuery(m.Query)
	if !ok || !zoneResources[normalizeResource(m.Path)] {
		return false
	}
	src := f.SrcEUI()
	dst := e.zonePeer(zone, src)
	if dst == nil {
		return false
	}
	pkt, ok := e.buildZoneHop(f, m, modifiedEUI64(src), dst, class, srcLbl, dstLbl)
	if !ok {
		return false
	}
	e.stats.ZoneProxies++
	e.queueZoneHop(dst, m, pkt, fmt.Sprintf("zone-proxy %s mid=%04x %s->%s",
		m.URI(), m.MID, srcLbl, e.name(dst)), src)
	e.trackZoneProxy(f, m, zone, src, dst, srcLbl)
	return true
}

// zonePeer picks the member of zone to ask on behalf of asking, or nil when the
// zone has no other member we believe is alive.
//
// Freshest first: the member heard from most recently is the one most likely to
// answer, and a member never heard from at all is skipped entirely rather than
// asked into the void. Ties and the all-unheard case fall back to the lowest
// wire EUI, so the choice stays stable across runs - nothing in the captures
// says which member the real bridge asks.
func (e *EmuState) zonePeer(zone int, asking []byte) []byte {
	var best []byte
	var bestSeen time.Time
	for _, member := range sortedKeys(e.zones[zone]) {
		b, err := hex.DecodeString(member)
		if err != nil || len(b) != 8 || bytes.Equal(b, asking) {
			continue
		}
		ns, known := e.nodes[member]
		if !known || ns.LastSeen.IsZero() {
			continue
		}
		if best == nil || ns.LastSeen.After(bestSeen) {
			best, bestSeen = b, ns.LastSeen
		}
	}
	if best != nil {
		return best
	}
	// Nobody has been heard from yet. On a cold start that is every member, and
	// asking the lowest EUI beats answering from an empty cache.
	for _, member := range sortedKeys(e.zones[zone]) {
		b, err := hex.DecodeString(member)
		if err != nil || len(b) != 8 || bytes.Equal(b, asking) {
			continue
		}
		return b
	}
	return nil
}

// trackZoneProxy records a proxied GET so ExpireZoneProxies can answer the
// requester if the member we asked stays silent. Keyed by MID, which is what the
// member's relayed 2.05 carries back.
func (e *EmuState) trackZoneProxy(f *Frame, m *CoAPMessage, zone int, src, dst []byte, srcLbl string) {
	if e.zoneProxyTimeout() <= 0 {
		return
	}
	e.zoneProxyPend[m.MID] = &zoneProxyWait{
		requester: append([]byte{}, src...),
		asked:     append([]byte{}, dst...),
		label:     srcLbl,
		zone:      zone,
		resource:  normalizeResource(m.Path),
		uri:       m.URI(),
		mid:       m.MID,
		token:     append([]byte{}, m.Token...),
		srcPort:   m.SrcPort,
		dstPort:   m.DstPort,
		deadline:  e.cfg.Now().Add(e.zoneProxyTimeout()),
	}
}

// zoneProxyTimeout is how long a proxied zone GET waits for the member's answer
// before we answer it ourselves. Zero or negative disables the expiry path,
// which restores the original behaviour of waiting forever.
func (e *EmuState) zoneProxyTimeout() time.Duration {
	if e.cfg.ZoneProxyTimeout == 0 {
		return defaultZoneProxyTimeout
	}
	return e.cfg.ZoneProxyTimeout
}

// resolveZoneProxy drops the pending record for mid, if any: the member answered
// and its 2.05 is on its way to the requester, so we must not answer too.
func (e *EmuState) resolveZoneProxy(mid uint16) {
	delete(e.zoneProxyPend, mid)
}

// ExpireZoneProxies answers every proxied zone GET whose member did not reply in
// time, and returns the frames to send. The answer is what the emulator would
// have sent had the proxy been off: the zone's cached state, or a bare 2.05.
// That keeps the requester's CoAP transaction from hanging forever on a member
// that is asleep, out of battery or gone.
//
// The caller drives it: the daemon calls it on its own timer, so the engine
// stays free of background goroutines and a replay run is deterministic.
func (e *EmuState) ExpireZoneProxies() []Action {
	if len(e.zoneProxyPend) == 0 {
		return nil
	}
	now := e.cfg.Now()
	var due []*zoneProxyWait
	for mid, w := range e.zoneProxyPend {
		if now.Before(w.deadline) {
			continue
		}
		due = append(due, w)
		delete(e.zoneProxyPend, mid)
	}
	sort.Slice(due, func(i, j int) bool { return due[i].mid < due[j].mid })

	var actions []Action
	for _, w := range due {
		e.stats.ZoneProxyTimeouts++
		e.log.Log(ClassData.String(), e.name(w.asked), w.label,
			rsnZoneProxyTimeout+" uri="+w.uri+fmt.Sprintf(" mid=%04x", w.mid), "")
		a, ok := e.answerExpiredProxy(w)
		if ok {
			actions = append(actions, a)
		}
	}
	return actions
}

// answerExpiredProxy builds the fallback answer for one timed-out proxied GET.
func (e *EmuState) answerExpiredProxy(w *zoneProxyWait) (Action, bool) {
	var pkt []byte
	var err error
	if body, ok := e.zoneState[zoneKey(w.zone, w.resource)]; ok && len(body) > 0 {
		pkt, err = BuildContextAckBody("2.05", w.mid, w.token, body, e.selfEUI, w.requester, w.dstPort, w.srcPort,
			zoneReadOptions(w.resource)...)
	} else {
		pkt, err = BuildContextAck("2.05", w.mid, w.token, e.selfEUI, w.requester, w.dstPort, w.srcPort)
	}
	if err != nil {
		e.log.Log(ClassData.String(), "", w.label, rsnBuildFail+" "+err.Error(), "")
		return Action{}, false
	}
	ctr, err := e.nextTx()
	if err != nil {
		e.log.Log(ClassData.String(), "", w.label, rsnBuildFail+" "+err.Error(), "")
		return Action{}, false
	}
	frame, err := BuildData(e.key, e.nextSeq(), w.requester, e.selfEUI[:5], WrapAppPayload(pkt, ctr, nil))
	if err != nil {
		e.log.Log(ClassData.String(), "", w.label, rsnBuildFail+" "+err.Error(), "")
		return Action{}, false
	}
	e.stats.CoAPAcksSent++
	return Action{
		Kind: ActData, Bytes: frame.Bytes(), Reason: fmt.Sprintf("zone-proxy-timeout 2.05 %s mid=%04x", w.uri, w.mid), Node: w.label, CSLTx: true,
	}, true
}

// PendingZoneProxies is how many proxied zone GETs are still waiting for their
// member's answer. For the API status view.
func (e *EmuState) PendingZoneProxies() int { return len(e.zoneProxyPend) }

// servedZoneGet answers a GET of a zone resource from the cached state, with the
// options the captured answer for that resource carries. With nothing cached the
// answer is the bare 2.05 the bridge sent before.
func (e *EmuState) servedZoneGet(m *CoAPMessage) ([]byte, []CoAPOption, bool) {
	zone, ok := zoneQuery(m.Query)
	if !ok || m.Code != "0.01" {
		return nil, nil, false
	}
	resource := normalizeResource(m.Path)
	body, ok := e.zoneState[zoneKey(zone, resource)]
	if !ok || len(body) == 0 {
		return nil, nil, false
	}
	return body, zoneReadOptions(resource), true
}

// zoneReadOptions are the options beyond Content-Format that the captured answer
// of a zone resource carries.
func zoneReadOptions(resource string) []CoAPOption {
	if resource == "z/p" {
		return []CoAPOption{optZoneMeasureAge}
	}
	return nil
}

func zoneKey(zone int, res string) string { return strconv.Itoa(zone) + "|" + res }

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// queryID extracts the logical id of an uplink from its "id=N"/"lid=N" query.
func queryID(query string) (int, bool) {
	m := reID.FindStringSubmatch(query)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

// --- Enhanced ACK ---

func (e *EmuState) onEnhAck(raw []byte) {
	e.stats.EnhAcks++
	a, err := ParseEnhAck(raw)
	if err != nil {
		e.log.Log("EnhACK", "", "", rsnMalformed, hex.EncodeToString(raw))
		return
	}
	if bytes.Equal(a.SrcEUI, e.selfEUI) {
		return // our own ACK looped back
	}
	key := hexEUI(a.SrcEUI)
	ns, known := e.nodes[key]
	if !known {
		if !e.known(key) {
			e.log.Log("EnhACK", e.name(a.SrcEUI), e.name(a.DstEUI), rsnUnknownEnhAck, hex.EncodeToString(raw))
		}
		ns = e.node(a.SrcEUI)
	}
	ns.CSLPhase, ns.CSLPeriod = a.CSLPhase, a.CSLPeriod
	ns.LastSeen = e.cfg.Now()
	e.confirmBySeq(a.SrcEUI, a.Seq)
}

// --- wake-up ---

func (e *EmuState) onWakeUp(raw []byte) {
	e.stats.WakeUps++
	w, err := ParseWakeUp(raw)
	if err != nil {
		e.log.Log("WakeUp", "", "", rsnMalformed, hex.EncodeToString(raw))
		return
	}
	if w.PANID != PANID {
		e.log.Log("WakeUp", "", w.DstShort, fmt.Sprintf("%s pan=%04x", rsnForeignPAN, w.PANID), hex.EncodeToString(raw))
		return
	}
	if w.DstShort == "ffff" {
		return // broadcast wake-up
	}
	if !e.known(w.DstShort) {
		e.log.Log("WakeUp", "", w.DstShort, rsnUnknownWakeTarget+" short="+w.DstShort, hex.EncodeToString(raw))
	}
	// A wake-up aimed at us means a device wants the channel; informational only,
	// the frame carries no source address to attribute it to.
}

// --- broadcast ---

func (e *EmuState) onBroadcast(raw []byte) []Action {
	e.stats.Broadcasts++
	src := ""
	bridgeSource := false
	if len(raw) >= 13 {
		src = e.name(raw[5:13])
		bridgeSource = bytes.Equal(raw[5:10], e.selfEUI[:5])
		if bridgeSource {
			e.bridgeFrames++
		}
	}
	b, err := ParseBroadcast(e.key, raw)
	if err != nil {
		reason := rsnMalformed
		if errors.Is(err, ErrMICFail) {
			reason = rsnMICFail
			e.stats.MICFails++
		}
		e.log.Log("Broadcast", src, "ffff", reason, hex.EncodeToString(raw))
		return nil
	}

	reason := rsnForeignBroadcast
	switch b.ICMPType {
	case icmpRouterSolicit:
		e.stats.ICMPv6++
		e.stats.RouterSolicitations++
		reason = rsnRouterSolicit
	case icmpRouterAdvert:
		e.stats.ICMPv6++
		e.stats.RouterAdvertisements++
		reason = rsnRouterAdvert
	case icmpNeighborSolicit:
		e.stats.ICMPv6++
		e.stats.NeighborSolicitations++
		reason = rsnNeighborSolicit
	}
	if b.ICMPType != icmpRouterSolicit || bridgeSource || e.BridgeSeen() {
		e.log.Log("Broadcast", src, "ffff", reason, hex.EncodeToString(raw))
		return nil
	}
	if !e.armed() {
		e.log.Log("Broadcast", src, "ffff", rsnRouterSolicit+" suppressed=tx-not-armed", hex.EncodeToString(raw))
		return nil
	}
	actions, due, err := e.beaconIfDue(routerSolicitAnswerWindow)
	if err != nil {
		e.log.Log("Broadcast", src, "ffff", rsnBuildFail, hex.EncodeToString(raw))
		return nil
	}
	// The advertisement is a broadcast, so a solicitation that arrives inside
	// the window is answered by the burst already on air.
	e.stats.RouterSolicitationsAnswered++
	if !due {
		return nil
	}
	return actions
}

// --- downlink queue ---

// enqueue appends one built command to the target's FIFO.
func (e *EmuState) enqueue(target, kind, desc string, coap, key []byte, mid uint16) error {
	wire, _, err := e.resolve(target)
	if err != nil {
		return err
	}
	frameLen := headerLen + len(WrapAppPayload(coap, 0, nil)) + micLen
	fragmented := false
	if frameLen > 127 {
		if kind != "config" && kind != "extui" && kind != fwKind {
			return fmt.Errorf("tado: %s downlink is %d bytes, exceeds PHY limit 127", kind, frameLen)
		}
		if _, err := Fragment6LoWPANDatagram(coap, 0); err != nil {
			return fmt.Errorf("tado: %s downlink is %d bytes and cannot be fragmented: %w", kind, frameLen, err)
		}
		fragmented = true
	}
	if frameLen > 127 && !fragmented {
		return fmt.Errorf("tado: %s downlink is %d bytes, exceeds PHY limit 127", kind, frameLen)
	}
	q := hexEUI(wire)
	e.queue[q] = append(e.queue[q], &queuedDownlink{
		kind: kind, desc: desc, coap: coap, key: key, mid: mid, fragmented: fragmented,
	})
	return nil
}

// downlinkIDs mints the CoAP MID and token of one queued command.
func downlinkIDs() (uint16, []byte, error) {
	mid, err := randMID()
	if err != nil {
		return 0, nil, err
	}
	token, err := randBytes(4)
	if err != nil {
		return 0, nil, err
	}
	return mid, token, nil
}

// EnqueueSetpoint queues a setpoint downlink for the next rendezvous with target
// (serial, short address, wire EUI or room name).
func (e *EmuState) EnqueueSetpoint(target string, sp SetpointParams) error {
	wire, _, err := e.resolve(target)
	if err != nil {
		return err
	}
	mid, token, err := downlinkIDs()
	if err != nil {
		return err
	}
	home, schedC := e.homeBlock(sp.ID, wire, sp.Sched)
	openWindow := sp.OpenWindow
	if open, ok := e.windowOpen[sp.ID]; ok {
		openWindow = boolByte(open)
	}
	c := SetpointCoAP{
		ID:         sp.ID,
		Setpoint:   sp.Setpoint,
		Overlay:    sp.Overlay,
		OpenWindow: openWindow,
		Home:       home,
		SchedC:     schedC,
		ProgramOff: e.schedOff[sp.ID],
		ZoneFlags:  sp.ZoneFlags,
		Circuit:    sp.Circuit,
		CtrlEnable: sp.CtrlEnable,
		NewOverlay: !sp.RunningOverlay,
		Off:        sp.Off,
		MID:        mid,
		Token:      token,
		SrcEUI:     e.selfEUI,
		DstEUI:     wire,
	}
	pkt, err := BuildSetpointCoAP(c)
	if err != nil {
		return err
	}
	desc := fmt.Sprintf("setpoint %.2fC id=%d", sp.Setpoint, sp.ID)
	if sp.Off {
		desc = fmt.Sprintf("off id=%d overlay=%d", sp.ID, sp.Overlay)
	}
	if err := e.enqueue(target, "setpoint", desc, pkt, e.key, mid); err != nil {
		return err
	}
	q := e.queue[hexEUI(wire)]
	q[len(q)-1].heating = setpointHeatingBlock(c, sp.Sched)
	q[len(q)-1].requireCoAP = true
	q[len(q)-1].responseToken = append([]byte(nil), token...)
	q[len(q)-1].responseCode = "2.04"
	e.armOverlayTimer(wire, sp.ID, sp.Duration, false, false)
	return nil
}

// armOverlayTimer starts, replaces or clears the coordinator-side deadline of
// an overlay on target. A zero duration clears it, which is what a return to
// schedule does.
func (e *EmuState) armOverlayTimer(wire []byte, id int, d time.Duration, dhw, boost bool) {
	key := hexEUI(wire)
	if d <= 0 {
		delete(e.boostTimers, key)
		return
	}
	e.boostTimers[key] = boostTimer{id: id, expiresAt: e.cfg.Now().UTC().Add(d), dhw: dhw, boost: boost}
}

// EnqueueZoneRewrite queues the per-zone write of a home or away switch, or of
// a schedule block change: the zone keeps the overlay and the setpoint it
// already holds, so the block carries no 6440.
func (e *EmuState) EnqueueZoneRewrite(target string, id int) error {
	wire, _, err := e.resolve(target)
	if err != nil {
		return err
	}
	mid, token, err := downlinkIDs()
	if err != nil {
		return err
	}
	c, sched := e.lastSetpointBlock(wire, id, mid, token)
	c.NewOverlay = false
	hotWater := e.zoneKind(id) == dhwZoneKind
	var pkt []byte
	var dhw *dhwBlock
	var body []byte
	if hotWater {
		dhw = e.lastDHWBlock(wire, id)
		pkt, err = buildDHWRewrite(c, dhw)
	} else {
		if pending := e.pendingHeating(wire, id); pending != nil {
			restatePending(&c, pending)
		}
		if body, err = setpointBody(c); err == nil {
			pkt, err = BuildSetpointCoAP(c)
		}
	}
	if err != nil {
		return err
	}
	desc := fmt.Sprintf("rewrite id=%d home=%t", id, c.Home)
	if body != nil {
		desc += " " + ZoneStateSummary(body)
	}
	if err := e.enqueue(target, "rewrite", desc, pkt, e.key, mid); err != nil {
		return err
	}
	q := e.queue[hexEUI(wire)]
	switch {
	case hotWater:
		q[len(q)-1].dhw = dhw
	case e.hasLastSetpointBlock(wire):
		q[len(q)-1].heating = setpointHeatingBlock(c, sched)
	}
	q[len(q)-1].requireCoAP = true
	q[len(q)-1].responseToken = append([]byte(nil), token...)
	q[len(q)-1].responseCode = "2.04"
	return nil
}

// buildDHWRewrite renders a zone rewrite on the hot-water block and fixes
// block to what it restates. The overlay is the one the hot-water zone runs,
// not the one the thermostat's heating zone runs. A setpoint the thermostat
// would clamp away leaves the zone on its program, which is the only state the
// coordinator can restate without inventing one.
func buildDHWRewrite(c SetpointCoAP, block *dhwBlock) ([]byte, error) {
	w := DHWSetpointCoAP{
		ID: c.ID, Overlay: block.overlay, Home: c.Home, SchedC: c.SchedC, ProgramOff: c.ProgramOff,
		MID: c.MID, Token: c.Token, SrcEUI: c.SrcEUI, DstEUI: c.DstEUI,
	}
	if block.overlay != overlayAuto {
		enabled := block.enabled != 0
		setpoint := float64(be(block.setpoint)) / 100
		switch {
		case !enabled:
			w.Enabled = &enabled
			block.setpoint = nil
		case len(block.setpoint) == 2 && setpoint >= DHWMinC && setpoint <= DHWMaxC:
			w.Setpoint = &setpoint
		default:
			w.Overlay = overlayAuto
		}
	}
	if w.Overlay == overlayAuto {
		*block = dhwBlock{zone: block.zone, overlay: overlayAuto}
	}
	return BuildDHWSetpointCoAP(w)
}

// lastDHWBlock returns the hot-water block zone id holds on wire: the last one
// queued to it, else the one the zone cache holds, else overlay auto.
func (e *EmuState) lastDHWBlock(wire []byte, id int) *dhwBlock {
	q := e.queue[hexEUI(wire)]
	for i := len(q) - 1; i >= 0; i-- {
		if d := q[i].dhw; d != nil && d.zone == id {
			return &dhwBlock{zone: id, overlay: d.overlay, enabled: d.enabled, setpoint: append([]byte(nil), d.setpoint...)}
		}
	}
	out := &dhwBlock{zone: id, overlay: overlayAuto}
	for _, tlv := range parseTLVList(e.zoneState[zoneKey(id, "z/s")]) {
		switch {
		case tlv.Tag == "6240" && len(tlv.Val) == 1:
			out.overlay = tlv.Val[0]
		case tlv.Tag == "6260" && len(tlv.Val) == 1:
			out.enabled = tlv.Val[0]
		case tlv.Tag == "6280" && len(tlv.Val) == 2:
			out.setpoint = append([]byte(nil), tlv.Val...)
		}
	}
	return out
}

// EnqueueDHWSetpoint queues the domestic-hot-water setpoint write for the wall
// thermostat: the z/s?id=N block on the hot-water zone, which the device turns
// into an OpenTherm TdhwSet write. id is the commissioned hot-water zone (0 in
// every capture). An enabled command carries tempC within DHWMinC..DHWMaxC. A
// disabled command omits the setpoint.
func (e *EmuState) EnqueueDHWSetpoint(target string, id int, tempC float64) error {
	return e.EnqueueDHWControl(target, id, tempC, true)
}

func (e *EmuState) EnqueueDHWControl(target string, id int, temperatureC float64, enabled bool) error {
	return e.EnqueueDHWOverlay(target, id, temperatureC, enabled, 0)
}

// EnqueueDHWOverlay is EnqueueDHWControl with a coordinator-side deadline. A
// non-zero duration arms the timer that queues the return to schedule; the
// captured 4 h hot-water timer carries nothing on the air.
func (e *EmuState) EnqueueDHWOverlay(target string, id int, temperatureC float64, enabled bool, d time.Duration) error {
	wire, _, err := e.resolve(target)
	if err != nil {
		return err
	}
	mid, token, err := downlinkIDs()
	if err != nil {
		return err
	}
	var setpoint *float64
	if enabled {
		setpoint = &temperatureC
	}
	overlay := byte(2)
	if d > 0 {
		overlay = 3
	}
	home, schedC := e.homeBlock(id, wire, 0)
	pkt, err := BuildDHWSetpointCoAP(DHWSetpointCoAP{
		ID:         id,
		Setpoint:   setpoint,
		Enabled:    &enabled,
		Overlay:    overlay,
		Home:       home,
		SchedC:     schedC,
		ProgramOff: e.schedOff[id],
		NewOverlay: true,
		MID:        mid,
		Token:      token,
		SrcEUI:     e.selfEUI,
		DstEUI:     wire,
	})
	if err != nil {
		return err
	}
	desc := fmt.Sprintf("dhw enabled=%t id=%d", enabled, id)
	if enabled {
		desc = fmt.Sprintf("dhw enabled=true setpoint %.2fC id=%d", temperatureC, id)
	}
	if err := e.enqueue(target, "dhw", desc, pkt, e.key, mid); err != nil {
		return err
	}
	q := e.queue[hexEUI(wire)]
	dhw := &dhwBlock{zone: id, overlay: overlay}
	if enabled {
		dhw.enabled = 1
		dhw.setpoint = tempC(temperatureC)
	}
	q[len(q)-1].dhw = dhw
	q[len(q)-1].requireCoAP = true
	q[len(q)-1].responseToken = append([]byte(nil), token...)
	q[len(q)-1].responseCode = "2.04"
	e.armOverlayTimer(wire, id, d, true, false)
	return nil
}

// EnqueueDHWSchedule queues the hot-water return to schedule: 6240=0 with
// neither 6260 nor 6280, and 6440 to mark the end of the overlay.
func (e *EmuState) EnqueueDHWSchedule(target string, id int) error {
	wire, _, err := e.resolve(target)
	if err != nil {
		return err
	}
	mid, token, err := downlinkIDs()
	if err != nil {
		return err
	}
	home, schedC := e.homeBlock(id, wire, 0)
	pkt, err := BuildDHWSetpointCoAP(DHWSetpointCoAP{
		ID:         id,
		Overlay:    overlayAuto,
		Home:       home,
		SchedC:     schedC,
		ProgramOff: e.schedOff[id],
		NewOverlay: true,
		MID:        mid,
		Token:      token,
		SrcEUI:     e.selfEUI,
		DstEUI:     wire,
	})
	if err != nil {
		return err
	}
	if err := e.enqueue(target, "dhw", fmt.Sprintf("dhw schedule id=%d", id), pkt, e.key, mid); err != nil {
		return err
	}
	q := e.queue[hexEUI(wire)]
	q[len(q)-1].dhw = &dhwBlock{zone: id, overlay: overlayAuto}
	q[len(q)-1].requireCoAP = true
	q[len(q)-1].responseToken = append([]byte(nil), token...)
	q[len(q)-1].responseCode = "2.04"
	delete(e.boostTimers, hexEUI(wire))
	return nil
}

// EnqueueFlowSetpoint registers the CH flow setpoint ("temperatura di mandata")
// the emulator serves for heating circuit id when the wall thermostat next polls
// "h/<home>/c/<id>/config" (tag 2040). Unlike a room or DHW setpoint this is not a
// queued downlink: the circuit config is cloud-hosted and the device pulls it, so
// the value is delivered as the answer to that GET, not as a bridge PUT. target is
// validated (the wall thermostat, the OpenTherm master); tempC must be within
// FlowMinC..FlowMaxC.
func (e *EmuState) EnqueueFlowSetpoint(target string, id int, tempC float64) error {
	if _, _, err := e.resolve(target); err != nil {
		return err
	}
	if err := flowInRange(tempC); err != nil {
		return err
	}
	e.flowC[id] = tempC
	e.flowFresh[id] = true
	e.refreshThermostatDocuments()
	return nil
}

// ClearFlowSetpoint drops the flow setpoint of circuit id, which then gets
// options.flow_limit_c.
func (e *EmuState) ClearFlowSetpoint(id int) {
	old, ok := e.flowC[id]
	if !ok {
		return
	}
	delete(e.flowC, id)
	if old != e.servedFlowC(id) {
		e.flowFresh[id] = true
	}
	e.refreshThermostatDocuments()
}

// ServedFlow returns the flow limit circuit id is served and whether the wall
// thermostat has fetched it.
func (e *EmuState) ServedFlow(id int) (tempC float64, fetched bool) {
	return e.servedFlowC(id), !e.flowFresh[id]
}

func (e *EmuState) servedFlowC(id int) float64 {
	if v, ok := e.flowC[id]; ok {
		return v
	}
	return float64(e.defaultFlowC())
}

func (e *EmuState) defaultFlowC() int {
	if e.inst == nil {
		return DefaultFlowLimitC
	}
	return e.inst.Options.FlowLimit()
}

// flowETagInput is what a wall thermostat's d/config ETag hashes beside the
// document. The thermostat GETs c/<N>/config only after a d/config 2.05, and
// its hourly d/config GET is conditional, so a served flow limit reaches it
// only when this ETag moves. The input is the served flow limits: the default
// and each circuit value that differs from it.
func (e *EmuState) flowETagInput() []byte {
	def := e.defaultFlowC()
	out := binary.BigEndian.AppendUint16([]byte(flowTag), uint16(def*100))
	for _, id := range slices.Sorted(maps.Keys(e.flowC)) {
		if v := e.flowC[id]; v != float64(def) {
			out = binary.BigEndian.AppendUint16(out, uint16(id))
			out = binary.BigEndian.AppendUint16(out, uint16(v*100+0.5))
		}
	}
	return out
}

// FlowPending reports how many heating circuits carry a flow setpoint the wall
// thermostat has not fetched yet.
func (e *EmuState) FlowPending() int {
	n := 0
	for _, fresh := range e.flowFresh {
		if fresh {
			n++
		}
	}
	return n
}

// EnqueueChildLock queues the d/lock child-lock write. The write goes to the
// bare path: id only labels the queue entry.
func (e *EmuState) EnqueueChildLock(target string, id int, on bool) error {
	wire, _, err := e.resolve(target)
	if err != nil {
		return err
	}
	mid, token, err := downlinkIDs()
	if err != nil {
		return err
	}
	pkt, err := BuildChildLockCoAP(on, mid, token, e.selfEUI, wire)
	if err != nil {
		return err
	}
	if err := e.enqueue(target, "childlock", fmt.Sprintf("childlock id=%d on=%t", id, on), pkt, e.key, mid); err != nil {
		return err
	}
	e.childLock[hexEUI(wire)] = on
	return nil
}

func (e *EmuState) EnqueueBoost(target string, id int, on bool) error {
	return e.enqueueBoostBlock(target, id, on, true, "boost", fmt.Sprintf("boost id=%d on=%t", id, on))
}

// EnqueueSchedule queues the return to schedule of a room zone: the captured
// block of a boost cancel, 6240=0 with neither 6260 nor 6280 and with 6440. It
// needs no earlier block of the node, unlike EnqueueMode.
func (e *EmuState) EnqueueSchedule(target string, id int) error {
	return e.enqueueBoostBlock(target, id, false, true, "mode", fmt.Sprintf("schedule id=%d", id))
}

func (e *EmuState) enqueueBoostBlock(target string, id int, on, newOverlay bool, kind, desc string) error {
	wire, _, err := e.resolve(target)
	if err != nil {
		return err
	}
	mid, token, err := downlinkIDs()
	if err != nil {
		return err
	}
	home, schedC := e.homeBlock(id, wire, 0)
	c := BoostCoAP{
		ID: id, Enabled: on, Home: home, SchedC: schedC, ProgramOff: e.schedOff[id],
		OpenWindow: e.zoneOpenWindow(id, wire), NewOverlay: newOverlay,
		MID: mid, Token: token, SrcEUI: e.selfEUI, DstEUI: wire,
	}
	pkt, err := BuildBoostCoAP(c)
	if err != nil {
		return err
	}
	if err := e.enqueue(target, kind, desc, pkt, e.key, mid); err != nil {
		return err
	}
	q := e.queue[hexEUI(wire)]
	q[len(q)-1].heating = &heatingBlock{zone: id, body: boostHeatingBody(c)}
	q[len(q)-1].requireCoAP = true
	q[len(q)-1].responseToken = append([]byte(nil), token...)
	q[len(q)-1].responseCode = "2.04"
	d := time.Duration(0)
	if on {
		d = BoostDuration
	}
	e.armOverlayTimer(wire, id, d, false, on)
	return nil
}

// zoneOpenWindow is the 62e0 a write to zone on node wire carries: the one the
// daemon drives, else the one the node last held.
func (e *EmuState) zoneOpenWindow(zone int, wire []byte) byte {
	if open, ok := e.windowOpen[zone]; ok {
		return boolByte(open)
	}
	if ns, ok := e.nodes[hexEUI(wire)]; ok && ns.LastOpenWindow {
		return 1
	}
	return 0
}

// ExpireBoosts queues the return to schedule for every elapsed coordinator
// timer: the boost restore and the deadline of any other overlay take the same
// path. The captured room expiry carries no 6440, unlike the return to schedule
// a user asks for. It returns the targets whose queue changed.
func (e *EmuState) ExpireBoosts(now time.Time) ([]string, error) {
	due := make([]string, 0)
	for target, timer := range e.boostTimers {
		if !now.Before(timer.expiresAt) {
			due = append(due, target)
		}
	}
	sort.Strings(due)
	for i, target := range due {
		timer := e.boostTimers[target]
		var err error
		if timer.dhw {
			err = e.EnqueueDHWSchedule(target, timer.id)
		} else {
			err = e.enqueueBoostBlock(target, timer.id, false, false, "boost", fmt.Sprintf("boost id=%d on=false", timer.id))
		}
		if err != nil {
			return due[:i], err
		}
	}
	return due, nil
}

// EnqueueIdentify queues the d/identify blink request. The write goes to the
// bare path: id only labels the queue entry.
func (e *EmuState) EnqueueIdentify(target string, id int) error {
	wire, _, err := e.resolve(target)
	if err != nil {
		return err
	}
	mid, token, err := downlinkIDs()
	if err != nil {
		return err
	}
	pkt, err := BuildIdentifyCoAP(mid, token, e.selfEUI, wire)
	if err != nil {
		return err
	}
	if err := e.enqueue(target, "identify", fmt.Sprintf("identify id=%d", id), pkt, e.key, mid); err != nil {
		return err
	}
	q := e.queue[hexEUI(wire)]
	q[len(q)-1].requireCoAP = true
	q[len(q)-1].responseToken = append([]byte(nil), token...)
	return nil
}

// EnqueueCalibrate queues the d/calibrate request of a valve. The valve
// answers with the server default 2.05 before the stroke starts; the outcome
// arrives later in its d/<serial>/mnt report (01fa, 01fb, 01b5, 01b6).
func (e *EmuState) EnqueueCalibrate(target string) error {
	wire, _, err := e.resolve(target)
	if err != nil {
		return err
	}
	mid, token, err := downlinkIDs()
	if err != nil {
		return err
	}
	pkt, err := BuildCalibrateCoAP(mid, token, e.selfEUI, wire)
	if err != nil {
		return err
	}
	if err := e.enqueue(target, "calibrate", "calibrate", pkt, e.key, mid); err != nil {
		return err
	}
	q := e.queue[hexEUI(wire)]
	q[len(q)-1].requireCoAP = true
	q[len(q)-1].responseToken = append([]byte(nil), token...)
	q[len(q)-1].responseCode = "2.05"
	return nil
}

// EnqueueReboot queues the d/reboot?id=N request.
func (e *EmuState) EnqueueReboot(target string, id int) error {
	wire, _, err := e.resolve(target)
	if err != nil {
		return err
	}
	mid, token, err := downlinkIDs()
	if err != nil {
		return err
	}
	pkt, err := BuildRebootCoAP(id, mid, token, e.selfEUI, wire)
	if err != nil {
		return err
	}
	if err := e.enqueue(target, "reboot", fmt.Sprintf("reboot id=%d", id), pkt, e.key, mid); err != nil {
		return err
	}
	q := e.queue[hexEUI(wire)]
	q[len(q)-1].requireCoAP = true
	q[len(q)-1].responseToken = append([]byte(nil), token...)
	return nil
}

// EnqueueNetcmd queues netcmd?t=T.
func (e *EmuState) EnqueueNetcmd(target string, t int) error {
	wire, _, err := e.resolve(target)
	if err != nil {
		return err
	}
	mid, token, err := downlinkIDs()
	if err != nil {
		return err
	}
	pkt, err := BuildNetcmdCoAP(t, mid, token, e.selfEUI, wire)
	if err != nil {
		return err
	}
	return e.enqueue(target, "netcmd", fmt.Sprintf("netcmd t=%d", t), pkt, e.key, mid)
}

// EnqueueMode queues a z/s write that changes only the overlay (6240): the rest
// of the block is resent from the node's last-known values.
func (e *EmuState) EnqueueMode(target string, id int, overlay byte) error {
	wire, _, err := e.resolve(target)
	if err != nil {
		return err
	}
	if !e.hasLastSetpointBlock(wire) {
		return ErrHeatingBlockUnavailable
	}
	if e.hasPendingHeatingBlock(wire) {
		return ErrHeatingBlockPending
	}
	mid, token, err := downlinkIDs()
	if err != nil {
		return err
	}
	c, sched := e.lastSetpointBlock(wire, id, mid, token)
	c.Overlay = overlay
	if overlay == overlayAuto {
		// The return to schedule ends the overlay, so its deadline goes with it.
		delete(e.boostTimers, hexEUI(wire))
		c.Off = false
	}
	pkt, err := BuildSetpointCoAP(c)
	if err != nil {
		return err
	}
	if err := e.enqueue(target, "mode", fmt.Sprintf("mode id=%d overlay=%d", id, overlay), pkt, e.key, mid); err != nil {
		return err
	}
	q := e.queue[hexEUI(wire)]
	q[len(q)-1].heating = setpointHeatingBlock(c, sched)
	q[len(q)-1].requireCoAP = true
	q[len(q)-1].responseToken = append([]byte(nil), token...)
	q[len(q)-1].responseCode = "2.04"
	return nil
}

// EnqueueOpenWindow queues a z/s write that changes only the open-window flag
// (62e0): the rest of the block is resent from the node's last-known values.
//
// This is the window-open state of the zone plane, not the open window
// detection setting: that one is tags 60e0 and 62c0 of the zone configuration
// document and needs no z/s write at all.
//
// The write keeps the overlay the zone runs, so it carries no 6440: the
// captured 62e0 writes that answer an open window report hold none.
//
// A node whose block is unknown, as after a restart, takes the block a zone
// rewrite restates. In the home state that block carries 6200, so it needs the
// program of the zone or the value the node was last given.
func (e *EmuState) EnqueueOpenWindow(target string, id int, open bool) error {
	wire, _, err := e.resolve(target)
	if err != nil {
		return err
	}
	cached := e.hasLastSetpointBlock(wire)
	if !cached && !e.knowsSchedC(id, wire) {
		return ErrHeatingBlockUnavailable
	}
	if e.hasPendingHeatingBlock(wire) {
		return ErrHeatingBlockPending
	}
	mid, token, err := downlinkIDs()
	if err != nil {
		return err
	}
	c, sched := e.lastSetpointBlock(wire, id, mid, token)
	c.NewOverlay = false
	c.OpenWindow = 0
	if open {
		c.OpenWindow = 1
	}
	pkt, err := BuildSetpointCoAP(c)
	if err != nil {
		return err
	}
	if err := e.enqueue(target, "openwindow", fmt.Sprintf("openwindow id=%d open=%t", id, open), pkt, e.key, mid); err != nil {
		return err
	}
	q := e.queue[hexEUI(wire)]
	block := &heatingBlock{zone: id, openWindow: c.OpenWindow, windowOnly: true}
	if cached {
		block = setpointHeatingBlock(c, sched)
	}
	q[len(q)-1].heating = block
	q[len(q)-1].requireCoAP = true
	q[len(q)-1].responseToken = append([]byte(nil), token...)
	q[len(q)-1].responseCode = "2.04"
	return nil
}

// OpenWindowReport is one PUT h/<home>/z/<Zone>/ow from a device: its own
// detector saw the window open. The captured report carries no body.
type OpenWindowReport struct {
	Zone   int
	Device string // wire EUI hex of the reporter
}

// TakeOpenWindowReports returns the reports received since the last call.
func (e *EmuState) TakeOpenWindowReports() []OpenWindowReport {
	out := e.owReports
	e.owReports = nil
	return out
}

// EnqueuePair queues the d/pair commission write that hands networkKey (16 bytes,
// plaintext) to a device. Its Data frame is encrypted under PairingKey, so the
// device accepts it only while its commissioning window is open. The request
// carries an empty token, so the terminal 2.04 echoes an empty token too.
func (e *EmuState) EnqueuePair(target string, networkKey []byte) error {
	wire, _, err := e.resolve(target)
	if err != nil {
		return err
	}
	mid, err := randMID()
	if err != nil {
		return err
	}
	pkt, err := BuildPairCoAP(networkKey, mid, e.selfEUI, wire)
	if err != nil {
		return err
	}
	if err := e.enqueue(target, "pair", "pair", pkt, PairingKey, mid); err != nil {
		return err
	}
	q := e.queue[hexEUI(wire)]
	q[len(q)-1].requireCoAP = true
	q[len(q)-1].responseToken = []byte{}
	q[len(q)-1].responseCode = "2.04"
	return nil
}

// SetSchedule stores the weekly document served at the valve's next schedule
// GET. The programs cover the week together: the firmware enables its schedule
// only after all seven days.
func (e *EmuState) SetSchedule(target string, programs []ScheduleProgram) error {
	wire, _, err := e.resolve(target)
	if err != nil {
		return err
	}
	if err := ValidateSchedule(programs); err != nil {
		return err
	}
	body := EncodeSchedulePrograms(programs)
	if len(body) > 0x719 {
		return errors.New("tado: schedule document exceeds valve buffer")
	}
	e.schedDoc[hexEUI(wire)] = cfgSnapshot{body: body, etag: configEtag(body)}
	return nil
}

// DropHotWaterScheduleLeftovers removes the sched document of each wall
// thermostat of the hot-water zone that can only hold a hot-water program from
// PUT /api/zones/<n>/schedule, before the daemon ran that program itself. The
// thermostat's offline engine would run it on its heating zone. The document
// names no zone, so it counts as a hot-water program only when no heating
// schedule explains it: the thermostat belongs to no heating zone, or no other
// member of its heating zones holds the same document and every entry sits in
// DHWMinC..DHWMaxC. It returns the serials whose document it dropped.
func (e *EmuState) DropHotWaterScheduleLeftovers() []string {
	if e.inst == nil {
		return nil
	}
	hw, ok := e.inst.HotWaterZone()
	if !ok {
		return nil
	}
	var dropped []string
	for _, ref := range hw.Members {
		d, err := e.inst.Resolve(ref)
		if err != nil || d.Kind != KindThermostat {
			continue
		}
		snap, ok := e.schedDoc[d.wireHex()]
		if !ok || !e.hotWaterLeftover(d, snap.body) {
			continue
		}
		delete(e.schedDoc, d.wireHex())
		dropped = append(dropped, d.Serial)
	}
	return dropped
}

func (e *EmuState) hotWaterLeftover(d *Device, body []byte) bool {
	heating := false
	for i := range e.inst.Zones {
		z := &e.inst.Zones[i]
		if z.Kind != ZoneHeating {
			continue
		}
		member, shared := false, false
		for _, ref := range z.Members {
			m, err := e.inst.Resolve(ref)
			if err != nil {
				continue
			}
			if m.wireHex() == d.wireHex() {
				member = true
			} else if other, ok := e.schedDoc[m.wireHex()]; ok && bytes.Equal(other.body, body) {
				shared = true
			}
		}
		if !member {
			continue
		}
		if shared {
			return false
		}
		heating = true
	}
	if !heating {
		return true
	}
	programs, err := DecodeSchedule(body)
	if err != nil {
		return false
	}
	for _, program := range programs {
		for _, entry := range program.Entries {
			if c := float64(entry.SetpointCenti) / 100; c < DHWMinC || c > DHWMaxC {
				return false
			}
		}
	}
	return true
}

// Schedule returns the day programs of the document served to target, and
// whether one is stored.
func (e *EmuState) Schedule(target string) ([]ScheduleProgram, bool, error) {
	wire, _, err := e.resolve(target)
	if err != nil {
		return nil, false, err
	}
	snap, ok := e.schedDoc[hexEUI(wire)]
	if !ok {
		return nil, false, nil
	}
	programs, err := DecodeSchedule(snap.body)
	if err != nil {
		return nil, false, err
	}
	return programs, true, nil
}

// ValidateSchedule checks a weekly document against the rules the device
// firmware enforces.
func ValidateSchedule(programs []ScheduleProgram) error {
	if len(programs) == 0 {
		return errors.New("tado: schedule requires at least one day program")
	}
	var week byte
	for _, program := range programs {
		if program.DayMask == 0 {
			return errors.New("tado: schedule day program selects no day")
		}
		if week&program.DayMask != 0 {
			return errors.New("tado: schedule day programs must not share a day")
		}
		week |= program.DayMask
		if err := validateScheduleEntries(program.Entries); err != nil {
			return err
		}
	}
	if week != 0x7f {
		return errors.New("tado: schedule requires all seven day bits")
	}
	return nil
}

// scheduleDayBlobMax bounds one day program as the device stores it: re-encoded
// into one config slot per day (0x29a+day, sched_program_write), which rejects a
// blob longer than this.
const scheduleDayBlobMax = 255

func validateScheduleEntries(entries []ScheduleEntry) error {
	if len(entries) == 0 || entries[0].Minute != 0 {
		return errors.New("tado: schedule first entry must start at minute 0")
	}
	previous := -1
	for _, entry := range entries {
		if entry.Minute < 0 || entry.Minute > 0x59f || entry.Minute <= previous {
			return errors.New("tado: schedule entries must be strictly increasing minutes in one day")
		}
		previous = entry.Minute
	}
	// The count includes the two-byte day mask field: exact if the device keeps
	// the mask in its per-day blob, two bytes strict if it does not.
	if n := len(EncodeScheduleDayProgram(0x01, entries...)); n > scheduleDayBlobMax {
		return fmt.Errorf("tado: schedule day program encodes to %d bytes, the device stores at most %d per day",
			n, scheduleDayBlobMax)
	}
	return nil
}

// EnqueueGet queues a bridge-initiated CoAP GET (active query) for a device
// resource. The device answers with a 2.05 whose payload the emulator files under
// resource, readable with ActiveGetResult. It returns the pending queue depth.
// In replay or mock there is no live device, so the result stays empty; the
// queue-and-correlate mechanism is what this exercises.
func (e *EmuState) EnqueueGet(target, resource string, path, query []string) error {
	wire, _, err := e.resolve(target)
	if err != nil {
		return err
	}
	if e.hasPendingGet(wire, resource, query) {
		return nil
	}
	mid, token, err := downlinkIDs()
	if err != nil {
		return err
	}
	pkt, err := BuildGetCoAP(path, query, mid, token, e.selfEUI, wire)
	if err != nil {
		return err
	}
	e.pendGet[mid] = &activeGet{
		resultKey: activeGetResultKey(wire, resource, query),
		path:      append([]string(nil), path...),
		query:     append([]string(nil), query...),
		token:     append([]byte(nil), token...),
	}
	if err := e.enqueue(target, "get", "get "+resource, pkt, e.key, mid); err != nil {
		delete(e.pendGet, mid)
		return err
	}
	return nil
}

func activeGetResultKey(wire []byte, resource string, query []string) string {
	key := hexEUI(wire) + "|" + resource
	if len(query) == 0 || strings.Contains(resource, "?") {
		return key
	}
	return key + "?" + strings.Join(query, "&")
}

func (e *EmuState) hasPendingGet(wire []byte, resource string, query []string) bool {
	key := hexEUI(wire)
	resultKey := activeGetResultKey(wire, resource, query)
	for mid, pending := range e.pendGet {
		if pending.resultKey != resultKey {
			continue
		}
		for _, d := range e.queue[key] {
			if d.kind == "get" && d.mid == mid {
				return true
			}
		}
		delete(e.pendGet, mid)
	}
	return false
}

func (e *EmuState) clearPendingGet(key string, d *queuedDownlink) {
	if d.kind != "get" {
		return
	}
	if pending, ok := e.pendGet[d.mid]; ok && strings.HasPrefix(pending.resultKey, key+"|") {
		delete(e.pendGet, d.mid)
	}
}

func (e *EmuState) ActiveGetResponse(target, resource string) (string, []byte, bool) {
	return e.ActiveGetResponseFor(target, resource, nil)
}

// ActiveGetResponseFor returns the last payload for one exact active GET URI.
func (e *EmuState) ActiveGetResponseFor(target, resource string, query []string) (string, []byte, bool) {
	wire, _, err := e.resolve(target)
	if err != nil {
		return "", nil, false
	}
	key := activeGetResultKey(wire, resource, query)
	body, ok := e.getRes[key]
	if !ok {
		return "", nil, false
	}
	return e.getCode[key], append([]byte{}, body...), true
}

// ActiveGetResult returns the last payload a device returned for a resource
// queried with EnqueueGet, and whether one has arrived yet.
func (e *EmuState) ActiveGetResult(target, resource string) ([]byte, bool) {
	_, body, ok := e.ActiveGetResponse(target, resource)
	return body, ok
}

// configEtagTag is the d/config document ETag (valve attr 0x15a, 8B). The device
// stores it and presents it on a conditional GET of d/config; the bridge answers
// 2.03 Valid when it still matches.
const configEtagTag = "015a"

// configUnavailable answers a d/config GET from a device we hold no document for.
const configUnavailable = "5.03"

// cfgSnapshot is the last d/config document pushed to a device and its ETag.
type cfgSnapshot struct {
	body []byte // the encoded store TLVs, ETag included
	etag []byte // 8-byte ETag stamping this document
}

// configEtag derives the 8-byte d/config ETag from the document body. The value
// is opaque to the device, which only compares it for equality, so any function
// that changes iff the body changes works; a hash keeps it deterministic across
// restarts. The real cloud assigns its own opaque ETags.
func configEtag(body []byte) []byte {
	h := fnv.New64a()
	h.Write(body)
	return h.Sum(nil)
}

// mergeConfigDocument merges a write into the device's last document and returns
// the TLVs to put on the wire and the document to remember. They are the same
// list: the bridge writes the whole document, never the changed tags alone.
// etagInput joins the document in the ETag hash.
func mergeConfigDocument(previous cfgSnapshot, w ConfigWrite, etagInput []byte) ([]TLV, cfgSnapshot, error) {
	if err := w.Validate(); err != nil {
		return nil, cfgSnapshot{}, err
	}
	updates := withoutConfigETag(w.TLVs())
	if len(updates) == 0 && len(w.Etag) == 0 && !w.Resend {
		return nil, cfgSnapshot{}, errors.New("tado: config write has no fields")
	}

	base := withoutConfigETag(parseTLVList(previous.body))
	merged := configDocument(mergeConfigTLVs(base, updates))
	etag := append([]byte(nil), w.Etag...)
	switch {
	case len(etag) > 0:
	case etagInput == nil && len(previous.etag) > 0 && bytes.Equal(encodeTLVs(merged), encodeTLVs(base)):
		// An unchanged document keeps its ETag: after a d/dbg2 patch
		// (patchConfigDocument) the ETag is no longer the hash of the body, and
		// a new one would earn the device a 2.05 and a full config apply.
		etag = append(etag, previous.etag...)
	default:
		etag = configEtag(append(encodeTLVs(merged), etagInput...))
	}
	document := append(merged, TLV{Tag: configEtagTag, Val: etag})
	return document, cfgSnapshot{body: encodeTLVs(document), etag: etag}, nil
}

func withoutConfigETag(in []TLV) []TLV {
	out := make([]TLV, 0, len(in))
	for _, tlv := range in {
		if tlv.Tag != configEtagTag {
			out = append(out, tlv)
		}
	}
	return out
}

// mergeConfigTLVs keeps the base order: an updated tag takes the place the base
// gave it, and only a tag the base does not hold goes to the end. The ETag is a
// hash of the encoded document, so moving a tag that keeps its value mints a new
// ETag and costs a restarted device the 2.03 its cached one earns. One tag can
// repeat (015e), so the whole update group lands on the first base record of
// that tag and the later ones drop.
func mergeConfigTLVs(base, updates []TLV) []TLV {
	groups := make(map[string][]TLV, len(updates))
	order := make([]string, 0, len(updates))
	for _, tlv := range updates {
		if _, ok := groups[tlv.Tag]; !ok {
			order = append(order, tlv.Tag)
		}
		groups[tlv.Tag] = append(groups[tlv.Tag], tlv)
	}
	placed := make(map[string]bool, len(groups))
	out := make([]TLV, 0, len(base)+len(updates))
	for _, tlv := range base {
		group, ok := groups[tlv.Tag]
		if !ok {
			out = append(out, tlv)
			continue
		}
		if !placed[tlv.Tag] {
			placed[tlv.Tag] = true
			out = append(out, group...)
		}
	}
	for _, tag := range order {
		if !placed[tag] {
			out = append(out, groups[tag]...)
		}
	}
	return out
}

// offlineSchedule resolves tag 02b3 for one device: the value the write
// carries, else the value an operator last wrote to that device, else the
// installation's options.offline_schedule.
func (e *EmuState) offlineSchedule(key string, explicit *bool) *bool {
	v, ok := e.offlineSched[key]
	switch {
	case explicit != nil:
		v = *explicit
	case !ok:
		v = e.inst == nil || e.inst.Options.OfflineScheduleEnabled()
	}
	return &v
}

// mergeDeviceConfig merges a write into the device's document with tag 02b3
// resolved, so every document freedo serves or writes carries it. A write with
// no field stays empty, so mergeConfigDocument still refuses it.
func (e *EmuState) mergeDeviceConfig(key string, w ConfigWrite) ([]TLV, cfgSnapshot, error) {
	if len(w.TLVs()) > 0 || w.Resend {
		w.OfflineScheduleEnabled = e.offlineSchedule(key, w.OfflineScheduleEnabled)
	}
	var etagInput []byte
	if e.isThermostat(key) {
		etagInput = e.flowETagInput()
	}
	return mergeConfigDocument(e.cfgDoc[key], w, etagInput)
}

func (e *EmuState) isThermostat(key string) bool {
	if e.inst == nil {
		return false
	}
	dev, err := e.inst.Resolve(key)
	return err == nil && dev.Kind == KindThermostat
}

func (e *EmuState) initConfigDocuments() {
	if e.inst == nil {
		return
	}
	for i := range e.inst.Devices {
		dev := &e.inst.Devices[i]
		key := hexEUI(dev.WireEUI())
		w, ok := e.installConfigWrite(dev)
		if !ok {
			continue
		}
		_, snap, err := e.mergeDeviceConfig(key, w)
		if err != nil {
			e.log.Log("Config", dev.Ref(), "", rsnBuildFail+" initial d/config "+err.Error(), "")
			continue
		}
		e.cfgDoc[key] = snap
	}
}

// initialNodeTable derives a device's 015e records. On a heating zone, bit 0x02
// and bit 0x04 are alternatives: the thermostat STM32 (cfg_attr_list_validate
// @0x08000548, run once by hvac_app_init) takes the circuit from 6060 on 0x02
// and from the fixed id 9 on 0x04, then reads 2040|circuit for OpenTherm
// MaxTSet. The heat source of its own zone therefore carries 0x0b, as captured
// (015e = 0b01), not 0x0d: with 0x0d it never writes the served flow setpoint.
func initialNodeTable(in *Install, dev *Device) []NodeTableRec {
	var out []NodeTableRec
	for i := range in.Zones {
		zone := &in.Zones[i]
		role := byte(0)
		members, _ := in.ZoneMembers(zone.Number)
		heatSource, err := in.ZoneHeatSource(zone.Number)
		isHeatSource := zone.Kind == ZoneHeating && err == nil && heatSource == dev
		if containsDevice(members, dev) {
			role = 0x05
			if isHeatSource {
				role = 0x03
			}
			if zone.Kind == ZoneHotWater {
				role |= roleElected
			} else if reporter, err := in.DemandReporter(zone.Number); err == nil && reporter == dev {
				role |= roleElected
			}
		} else if isHeatSource {
			role = 0x02
		}
		if role != 0 {
			out = append(out, NodeTableRec{Role: role, Node: uint8(zone.Number)})
		}
	}
	return out
}

// EnqueueConfig queues a d/config store write with the set fields merged into
// the device's document. At least one field must be set, unless the write asks
// for a resend of the unchanged document. A set OfflineScheduleEnabled stays
// the device's tag 02b3 over options.offline_schedule until the device leaves
// the installation. The whole document goes on the wire, as the bridge writes
// it, stamped with its ETag (tag 015a) unless the caller supplied one; the ETag that went on the wire is remembered so a later
// conditional GET can be validated. On-air acceptance requires the
// matching final 2.04 response from the device.
//
// The id names the write in the log and in the queue: the path carries no query.
func (e *EmuState) EnqueueConfig(target string, id int, w ConfigWrite) error {
	wire, _, err := e.resolve(target)
	if err != nil {
		return err
	}
	if e.refusesConfigWrite(wire) {
		return errors.New("tado: the wall thermostat refuses every d/config write with 5.01")
	}
	if w.InterfaceModuleID != nil && *w.InterfaceModuleID != valveInterfaceModule {
		return errors.New("tado: only a valve accepts a d/config write, and its interface module id is 112")
	}
	fields := len(w.TLVs())
	key := hexEUI(wire)
	explicit := w.OfflineScheduleEnabled
	tlvs, snap, err := e.mergeDeviceConfig(key, w)
	if err != nil {
		return err
	}
	if previous := e.cfgDoc[key]; !w.Resend &&
		bytes.Equal(previous.body, snap.body) && bytes.Equal(previous.etag, snap.etag) {
		if explicit != nil {
			e.offlineSched[key] = *explicit
		}
		return nil
	}
	mid, token, err := downlinkIDs()
	if err != nil {
		return err
	}
	pkt, err := BuildConfigWriteCoAP(tlvs, mid, token, e.selfEUI, wire)
	if err != nil {
		return err
	}
	desc := fmt.Sprintf("config write id=%d (%d fields)", id, fields)
	if fields == 0 && w.Resend {
		desc = fmt.Sprintf("config write id=%d (unchanged document)", id)
	}
	if err := e.enqueue(target, "config", desc, pkt, e.key, mid); err != nil {
		return err
	}
	q := e.queue[key]
	q[len(q)-1].requireCoAP = true
	q[len(q)-1].responseToken = append([]byte{}, token...)
	q[len(q)-1].responseCode = "2.04"
	e.cfgDoc[key] = snap
	if explicit != nil {
		e.offlineSched[key] = *explicit
	}
	if w.TempUnitF != nil {
		e.tempUnitF[hexEUI(wire)] = *w.TempUnitF
	}
	if w.UTCOffsetMin != nil && e.utcOffset != nil {
		// The device has now been told an offset, so the automatic push has
		// nothing left to add: an operator's explicit value is not ours to
		// correct back to the one in force.
		e.utcSettled[hexEUI(wire)] = *e.utcOffset
	}
	return nil
}

// ClearOfflineSchedule drops the tag 02b3 an operator wrote to target, so the
// device follows options.offline_schedule again. Each d/config write makes a
// valve home its motor, so a write is queued only when the device's document
// moves: when the dropped value differs from the installation option. The id
// names the write, as in EnqueueConfig. A device that refuses the write gets
// the document served on its next read. It reports whether a write was queued.
func (e *EmuState) ClearOfflineSchedule(target string, id int) (bool, error) {
	wire, _, err := e.resolve(target)
	if err != nil {
		return false, err
	}
	key := hexEUI(wire)
	previous, ok := e.offlineSched[key]
	if !ok {
		return false, nil
	}
	delete(e.offlineSched, key)
	if _, ok := e.cfgDoc[key]; !ok {
		return false, nil
	}
	w := ConfigWrite{OfflineScheduleEnabled: e.offlineSchedule(key, nil)}
	if !e.refusesConfigWrite(wire) {
		queued := len(e.queue[key])
		err := e.EnqueueConfig(key, id, w)
		// EnqueueConfig records an explicit value as the device's own.
		delete(e.offlineSched, key)
		if err != nil {
			e.offlineSched[key] = previous
			return false, err
		}
		return len(e.queue[key]) > queued, nil
	}
	_, snap, err := e.mergeDeviceConfig(key, w)
	if err != nil {
		e.offlineSched[key] = previous
		return false, err
	}
	e.cfgDoc[key] = snap
	return false, nil
}

// refusesConfigWrite reports whether the device answers a d/config write 5.01.
// The wall thermostat does, so the coordinator writes its configuration nowhere
// and serves it on the read path only. A device the installation does not
// declare has no known kind, and no capture refuses a write to one.
func (e *EmuState) refusesConfigWrite(wire []byte) bool {
	if e.inst == nil {
		return false
	}
	dev, err := e.inst.Resolve(hexEUI(wire))
	return err == nil && dev.Kind == KindThermostat
}

// deviceID is the logical id a device's own "?id=N" resources answer to: the one
// the installation declares for it, else the last one it used itself.
func (e *EmuState) deviceID(wire []byte, ns *NodeState) (int, bool) {
	if e.inst != nil {
		if d, err := e.inst.Resolve(hexEUI(wire)); err == nil && d.Zone != nil {
			return *d.Zone, true
		}
	}
	if ns != nil && ns.LastID != 0 {
		return ns.LastID, true
	}
	return 0, false
}

// SetHomeState records the state of the whole home and the setpoint the weekly
// program holds in each zone. Every z/s write the emulator builds after it
// carries that pair: 6160 and 61e0, plus 6200 in the home state. The hot-water
// zone always takes the home state (zoneHome). The daemon owns the values and
// replaces the map as a whole.
func (e *EmuState) SetHomeState(home bool, schedC map[int]float64) {
	e.home, e.homeSet = home, true
	e.schedC = make(map[int]float64, len(schedC))
	for zone, c := range schedC {
		e.schedC[zone] = c
	}
}

// SetScheduleOff gives the emulator the zones whose program block in force is
// disabled. Every home-state z/s write to such a zone carries 61e0=0, so the
// device targets its fallback: 5 C in a room, 30 C on hot water. The app
// capture of a room block set to off holds 61e0=0 and no 6200; the firmware's
// offline engine copies entry field 3 into 61e0 the same way. The daemon
// replaces the map as a whole.
func (e *EmuState) SetScheduleOff(zones map[int]bool) {
	e.schedOff = make(map[int]bool, len(zones))
	for zone, off := range zones {
		if off {
			e.schedOff[zone] = true
		}
	}
}

// SetOpenWindows gives the emulator the 62e0 the daemon wants in the zones its
// window engine drives. Every z/s write to such a zone carries that value, so a
// setpoint or a zone rewrite cannot undo the window behind the daemon's back
// and a member the engine could not reach converges on its next write. A zone
// outside the map keeps the 62e0 its node last held.
func (e *EmuState) SetOpenWindows(zones map[int]bool) {
	e.windowOpen = make(map[int]bool, len(zones))
	for zone, open := range zones {
		e.windowOpen[zone] = open
	}
}

// ConfirmedOpenWindow reports the 62e0 target holds for sure: the value it
// last reported or took, with no heating write to it in flight.
func (e *EmuState) ConfirmedOpenWindow(target string) (open, confirmed bool) {
	wire, _, err := e.resolve(target)
	if err != nil || e.hasPendingHeatingBlock(wire) {
		return false, false
	}
	ns, ok := e.nodes[hexEUI(wire)]
	if !ok || len(ns.raw["62e0"]) != 1 {
		return false, false
	}
	return ns.LastOpenWindow, true
}

// HomeState reports the state the emulator stamps on its z/s writes.
func (e *EmuState) HomeState() bool { return e.home }

// zoneHome reports whether the z/s blocks of zone carry the home head. The
// hot-water zone ignores away: on the program, 6160=2 with 61e0=0 makes the
// thermostat's zone_state_timer_cb publish DHW off at 30 C, and no target path
// reads 6160, so the home head keeps hot water on.
func (e *EmuState) zoneHome(zone int) bool {
	return e.home || e.zoneKind(zone) == dhwZoneKind
}

// ZoneSchedC returns the scheduled setpoint (6200) the zone cache holds: the
// value of the last write or member report z/s?lid=N that carried it. The valve
// reports the 6200 of the last home-state write it took.
func (e *EmuState) ZoneSchedC(zone int) (float64, bool) {
	for _, tlv := range parseTLVList(e.zoneState[zoneKey(zone, "z/s")]) {
		if tlv.Tag == "6200" && len(tlv.Val) == 2 {
			return float64(be(tlv.Val)) / 100.0, true
		}
	}
	return 0, false
}

// homeBlock returns the home flag and the 6200 value of a write to zone on
// node wire. A home write always carries 6200, so a zone the daemon holds no
// program for falls back to the last value the node was given, then to what the
// caller records for it.
func (e *EmuState) homeBlock(zone int, wire []byte, fallback float64) (bool, float64) {
	if !e.zoneHome(zone) {
		return false, 0
	}
	if c, ok := e.schedC[zone]; ok {
		return true, c
	}
	if ns, ok := e.nodes[hexEUI(wire)]; ok && ns.LastSchedC != 0 {
		return true, ns.LastSchedC
	}
	return true, fallback
}

// knowsSchedC reports whether a write to zone on node wire can restate its
// 6200 without a fallback: away the write carries none.
func (e *EmuState) knowsSchedC(zone int, wire []byte) bool {
	if !e.zoneHome(zone) {
		return true
	}
	if _, ok := e.schedC[zone]; ok {
		return true
	}
	ns, ok := e.nodes[hexEUI(wire)]
	return ok && ns.LastSchedC != 0
}

// zoneKind is the 6020 value of a zone: hot water reads 2, a room 1.
func (e *EmuState) zoneKind(id int) byte {
	if e.inst != nil {
		if z, err := e.inst.ZoneByNumber(id); err == nil {
			return z.Kind.TLV6020()
		}
	}
	return roomZoneKind
}

// lastSetpointBlock rebuilds the downlink from the node's last-known values. It
// also returns the scheduled setpoint (6200) the node last reported or took.
func (e *EmuState) lastSetpointBlock(wire []byte, id int, mid uint16, token []byte) (SetpointCoAP, float64) {
	c := SetpointCoAP{ID: id, ProgramOff: e.schedOff[id], NewOverlay: true, MID: mid, Token: token, SrcEUI: e.selfEUI, DstEUI: wire}
	open, driven := e.windowOpen[id]
	c.OpenWindow = boolByte(open)
	var sched float64
	if ns, ok := e.nodes[hexEUI(wire)]; ok {
		c.Setpoint, c.Overlay = ns.LastSetpointC, ns.LastOverlay
		if !driven && ns.LastOpenWindow {
			c.OpenWindow = 1
		}
		sched = ns.LastSchedC
	}
	c.Home, c.SchedC = e.homeBlock(id, wire, sched)
	overlay, setpoint, off, ok := e.cachedZoneOverlay(id)
	switch {
	case !e.hasLastSetpointBlock(wire):
		if ok {
			c.Overlay, c.Setpoint, c.Off = overlay, setpoint, off
		}
	case ok && overlay != overlayAuto && c.Overlay != overlayAuto:
		// The node cache mixes every zone a node reports, so the zone cache
		// alone says whether the room runs the off overlay.
		c.Off = off
	}
	return c, sched
}

// cachedZoneOverlay returns the overlay of the zone cache, with its setpoint
// when the overlay carries one. The cache outlives a restart, so a node whose
// block is unknown keeps its overlay. off reports the room off overlay
// (6260=0), which carries no setpoint.
func (e *EmuState) cachedZoneOverlay(zone int) (overlay byte, setpoint float64, off, ok bool) {
	if e.zoneKind(zone) == dhwZoneKind {
		return 0, 0, false, false
	}
	var mode, enabled, sp []byte
	for _, tlv := range parseTLVList(e.zoneState[zoneKey(zone, "z/s")]) {
		switch tlv.Tag {
		case "6240":
			mode = tlv.Val
		case "6260":
			enabled = tlv.Val
		case "6280":
			sp = tlv.Val
		}
	}
	switch {
	case len(mode) != 1:
		return 0, 0, false, false
	case mode[0] == overlayAuto:
		return overlayAuto, 0, false, true
	case len(enabled) == 1 && enabled[0] == 0x00:
		return mode[0], 0, true, true
	case len(enabled) != 1 || enabled[0] != 0x01 || len(sp) != 2:
		return 0, 0, false, false
	}
	return mode[0], float64(be(sp)) / 100, false, true
}

func (e *EmuState) hasLastSetpointBlock(wire []byte) bool {
	ns, ok := e.nodes[hexEUI(wire)]
	return ok && len(ns.raw["6280"]) == 2 && len(ns.raw["6200"]) == 2 &&
		len(ns.raw["6240"]) == 1 && len(ns.raw["62e0"]) == 1
}

func (e *EmuState) hasPendingHeatingBlock(wire []byte) bool {
	for _, d := range e.queue[hexEUI(wire)] {
		if d.heating != nil {
			return true
		}
	}
	return false
}

func (e *EmuState) promoteHeatingBlock(wire []byte, block *heatingBlock) {
	if block == nil {
		return
	}
	ns := e.node(wire)
	if block.windowOnly {
		ns.raw["62e0"] = []byte{block.openWindow}
		ns.refreshLastKnown()
		if cached := e.zoneState[zoneKey(block.zone, "z/s")]; len(cached) > 0 {
			e.storeZoneState(block.zone, "z/s", encodeTLVs(mergeConfigTLVs(parseTLVList(cached),
				[]TLV{{Tag: "62e0", Val: []byte{block.openWindow}}})))
		}
		return
	}
	defer e.noteZoneWrite(block.zone, e.zoneHeating(block.zone))
	if block.body != nil {
		for _, tlv := range parseTLVList(block.body) {
			switch tlv.Tag {
			case "6200", "6240", "6280", "62e0":
				ns.raw[tlv.Tag] = append([]byte(nil), tlv.Val...)
			}
		}
		ns.refreshLastKnown()
		e.storeZoneState(block.zone, "z/s", append([]byte(nil), block.body...))
		return
	}
	ns.raw["6280"] = append([]byte(nil), block.setpoint...)
	ns.raw["6200"] = append([]byte(nil), block.sched...)
	ns.raw["6240"] = []byte{block.overlay}
	ns.raw["62e0"] = []byte{block.openWindow}
	ns.refreshLastKnown()
	key := zoneKey(block.zone, "z/s")
	// The served block keeps 6280 even under overlay auto: the capture shows the
	// device still reports the last setpoint after a return to schedule.
	updates := []TLV{
		{Tag: "6160", Val: []byte{zoneCtlAway}},
		{Tag: "6180", Val: []byte{0x00}},
		{Tag: "6020", Val: []byte{roomZoneKind}},
		{Tag: "61e0", Val: []byte{0x00}},
		{Tag: "6240", Val: []byte{block.overlay}},
		{Tag: "6260", Val: []byte{0x01}},
		{Tag: "6280", Val: append([]byte(nil), block.setpoint...)},
		{Tag: "62e0", Val: []byte{block.openWindow}},
	}
	e.storeZoneState(block.zone, "z/s", encodeTLVs(mergeConfigTLVs(withoutTag(parseTLVList(e.zoneState[key]), "6200"), updates)))
}

// withoutTag drops one tag from a cached block. A dropped 6200 comes back with
// the next member report z/s?lid=N, which carries the scheduled setpoint.
func withoutTag(list []TLV, tag string) []TLV {
	out := make([]TLV, 0, len(list))
	for _, tlv := range list {
		if tlv.Tag != tag {
			out = append(out, tlv)
		}
	}
	return out
}

func (e *EmuState) promoteDHWBlock(block *dhwBlock) {
	if block == nil {
		return
	}
	updates := []TLV{
		{Tag: "6160", Val: []byte{0x02}},
		{Tag: "6180", Val: []byte{0x00}},
		{Tag: "6020", Val: []byte{0x02}},
		{Tag: "61e0", Val: []byte{0x00}},
		{Tag: "6240", Val: []byte{block.overlay}},
		{Tag: "6260", Val: []byte{block.enabled}},
	}
	if len(block.setpoint) > 0 {
		updates = append(updates, TLV{Tag: "6280", Val: append([]byte(nil), block.setpoint...)})
	}
	updates = append(updates, TLV{Tag: "62e0", Val: []byte{0x00}})
	e.storeZoneState(block.zone, "z/s", encodeTLVs(updates))
}

func (e *EmuState) invalidateHeatingBlock(wire []byte) {
	ns, ok := e.nodes[hexEUI(wire)]
	if !ok {
		return
	}
	delete(ns.raw, "6280")
	delete(ns.raw, "6200")
	delete(ns.raw, "6240")
	delete(ns.raw, "62e0")
	ns.LastSetpointC = 0
	ns.LastSchedC = 0
	ns.LastOverlay = 0
	ns.LastOpenWindow = false
}

// Pending returns the number of queued downlinks for target.
func (e *EmuState) Pending(target string) int {
	wire, _, err := e.resolve(target)
	if err != nil {
		return 0
	}
	return len(e.queue[hexEUI(wire)])
}

func (e *EmuState) Queue(target string) []QueueEntry {
	wire, _, err := e.resolve(target)
	if err != nil {
		return nil
	}
	queued := e.queue[hexEUI(wire)]
	out := make([]QueueEntry, len(queued))
	for i, d := range queued {
		out[i] = QueueEntry{
			Kind: d.kind, Desc: d.desc, Tries: d.tries, Seq: d.seq,
			MID: d.mid, InFlight: d.inFlight,
		}
	}
	return out
}

// QueueHistory returns the newest terminal downlink results for target.
func (e *EmuState) QueueHistory(target string) []DownlinkOutcome {
	wire, _, err := e.resolve(target)
	if err != nil {
		return nil
	}
	history := e.history[hexEUI(wire)]
	return append([]DownlinkOutcome(nil), history...)
}

func (e *EmuState) recordDownlinkOutcome(key string, d *queuedDownlink, result, reason string) {
	if d.kind == fwKind && result != "delivered" {
		e.fwReply[key] = FwReply{MID: d.mid, Block1: -1, Result: result, Reason: reason}
	}
	history := e.history[key]
	outcome := DownlinkOutcome{
		Kind: d.kind, Desc: d.desc, Tries: d.tries, Seq: d.seq, MID: d.mid,
		Result: result, Reason: reason, Completed: e.cfg.Now().UTC(),
	}
	history = append([]DownlinkOutcome{outcome}, history...)
	if len(history) > maxDownlinkHistory {
		history = history[:maxDownlinkHistory]
	}
	e.history[key] = history
}

// DropQueue discards every downlink still queued for target and returns how many
// went. The delivery counters are left alone: nothing was delivered and nothing
// was given up on by the retry logic, the operator simply withdrew the commands.
//
// It exists for the cold-start harness, which has to guarantee that an attempt
// that did not land does not linger and ride out as a piggyback during the next
// one - that would credit the cold path with a delivery the rendezvous made.
func (e *EmuState) DropQueue(target string) int {
	wire, _, err := e.resolve(target)
	if err != nil {
		return 0
	}
	key := hexEUI(wire)
	queued := e.queue[key]
	n := len(queued)
	for _, d := range queued {
		e.clearPendingGet(key, d)
		e.recordDownlinkOutcome(key, d, "withdrawn", "operator")
	}
	delete(e.queue, key)
	return n
}

// NextQueuedDownlink builds the next attempt for an explicit coordinator
// dispatch. It uses the same FIFO and accounting path as an uplink rendezvous.
func (e *EmuState) NextQueuedDownlink(target string) ([]Action, bool, error) {
	wire, label, err := e.resolve(target)
	if err != nil {
		return nil, false, err
	}
	actions, ok := e.nextDownlink(wire, label)
	for i := range actions {
		actions[i].QueueTarget = target
	}
	return actions, ok, nil
}

func (e *EmuState) LinkMaintenanceEcho(target string) (Action, error) {
	wire, label, err := e.resolve(target)
	if err != nil {
		return Action{}, err
	}
	pkt, err := BuildICMPv6EchoRequest(e.selfEUI, wire)
	if err != nil {
		return Action{}, err
	}
	ctr, err := e.nextTx()
	if err != nil {
		return Action{}, err
	}
	frame, err := BuildData(e.key, e.nextSeq(), wire, e.selfEUI[:5], WrapAppPayload(pkt, ctr, nil))
	if err != nil {
		return Action{}, err
	}
	return Action{Kind: ActData, Bytes: frame.Bytes(), Reason: "link-maintenance echo", Node: label, CSLTx: true, NoCSLBootstrap: true, WaitResponse: true}, nil
}

func (e *EmuState) AutoQueueEnabled() bool { return !e.cfg.NoAutoQueue }

// HoldQueuedDownlink prevents an in-flight explicit dispatch from being
// retransmitted by an uplink that arrives before its acknowledgement.
func (e *EmuState) HoldQueuedDownlink(target string, delay time.Duration) {
	wire, _, err := e.resolve(target)
	if err != nil || delay <= 0 {
		return
	}
	q := e.queue[hexEUI(wire)]
	if len(q) == 0 || !q[0].inFlight {
		return
	}
	q[0].retryAt = e.cfg.Now().Add(delay)
}

// MarkQueuedDownlinkAwaitingCSL prevents a queued ESP job from being rebuilt
// while the firmware still owns its completion.
func (e *EmuState) MarkQueuedDownlinkAwaitingCSL(dstEUI []byte, seq byte) bool {
	q := e.queue[hexEUI(dstEUI)]
	if len(q) == 0 || !q[0].inFlight || q[0].seq != seq {
		return false
	}
	q[0].awaitingCSL = true
	return true
}

// CompleteQueuedDownlinkAttempt releases the host-side admission hold.
func (e *EmuState) CompleteQueuedDownlinkAttempt(dstEUI []byte, seq byte) bool {
	q := e.queue[hexEUI(dstEUI)]
	if len(q) == 0 || !q[0].inFlight || q[0].seq != seq {
		return false
	}
	q[0].awaitingCSL = false
	return true
}

// QueuedDownlinkAwaitingCSL reports an accepted ESP job without a completion.
func (e *EmuState) QueuedDownlinkAwaitingCSL(target string) bool {
	wire, _, err := e.resolve(target)
	if err != nil {
		return false
	}
	q := e.queue[hexEUI(wire)]
	return len(q) != 0 && q[0].inFlight && q[0].awaitingCSL
}

// nextDownlink builds the next attempt for a device we are in rendezvous with.
func (e *EmuState) nextDownlink(dstEUI []byte, label string) ([]Action, bool) {
	key := hexEUI(dstEUI)
	for len(e.queue[key]) > 0 {
		if e.fwLock[key] && !e.fwAtHead(key) {
			return nil, false
		}
		d := e.queue[key][0]
		if d.inFlight && d.awaitingCSL {
			return nil, false
		}
		if d.inFlight && !d.retryAt.IsZero() && e.cfg.Now().Before(d.retryAt) {
			return nil, false
		}
		if d.inFlight && d.tries >= maxDownlinkTries {
			e.queue[key] = e.queue[key][1:]
			e.stats.DownlinksDropped++
			e.clearPendingGet(key, d)
			e.recordDownlinkOutcome(key, d, "dropped", "retry-limit")
			e.log.Log("Data", "self", label,
				fmt.Sprintf("%s %s after %d tries", rsnDownlinkLost, d.desc, d.tries), "")
			continue
		}
		frames, seq, err := e.buildFrames(dstEUI, d)
		if err != nil {
			e.queue[key] = e.queue[key][1:]
			e.stats.DownlinksDropped++
			e.clearPendingGet(key, d)
			e.recordDownlinkOutcome(key, d, "dropped", "build-failed")
			e.log.Log("Data", "self", label, rsnBuildFail+" "+err.Error(), "")
			continue
		}
		d.tries++
		d.seq = seq
		d.inFlight = true
		d.deferred = false
		if d.kind == "get" {
			d.retryAt = e.cfg.Now().Add(activeGetRetryBackoff)
		}
		e.stats.DownlinksSent++
		actions := make([]Action, 0, len(frames))
		for i, frame := range frames {
			reason := "downlink " + d.desc + fmt.Sprintf(" seq=%02x try=%d", seq, d.tries)
			if len(frames) > 1 {
				reason += fmt.Sprintf(" fragment=%d/%d", i+1, len(frames))
			}
			actions = append(actions, Action{Kind: ActData, Bytes: frame, Reason: reason, Node: label, CSLTx: true, HasCSLBatch: d.fragmented, CSLBatchTag: d.fragTag, QueueAttempt: true, WaitResponse: d.kind == "get" || d.requireCoAP || d.fragmented, QueueTry: d.tries, QueueMID: d.mid, CoAPTransaction: d.requireCoAP || d.fragmented, ResponseRetry: d.responseRetry, IdentifyCSLBootstrapRetry: d.kind == "identify", CSLBootstrapAlways: d.kind == "auth", ZoneRelay: d.kind == "zone"})
		}
		return actions, true
	}
	return nil, false
}

// buildFrames frames one CoAP datagram for one more attempt. A fragmented
// config write stays one CoAP exchange and uses a fresh RFC 4944 tag per try.
func (e *EmuState) buildFrames(dstEUI []byte, d *queuedDownlink) ([][]byte, byte, error) {
	if !d.fragmented {
		ctr, err := e.nextTx()
		if err != nil {
			return nil, 0, err
		}
		seq := d.seq
		if !d.responseRetry || d.tries == 0 {
			seq = e.nextSeq()
		}
		f, err := BuildData(d.key, seq, dstEUI, e.selfEUI[:5], WrapAppPayload(d.coap, ctr, nil))
		if err != nil {
			return nil, 0, err
		}
		return [][]byte{f.Bytes()}, seq, nil
	}
	d.fragTag = e.nextFragTag()
	fragments, err := Fragment6LoWPANDatagram(d.coap, d.fragTag)
	if err != nil {
		return nil, 0, err
	}
	if err := e.reserveTx(len(fragments)); err != nil {
		return nil, 0, err
	}
	frames := make([][]byte, 0, len(fragments))
	var lastSeq byte
	for _, fragment := range fragments {
		ctr, err := e.nextTx()
		if err != nil {
			return nil, 0, err
		}
		lastSeq = e.nextSeq()
		f, err := BuildDataPending(d.key, lastSeq, dstEUI, e.selfEUI[:5], WrapAppPayload(fragment, ctr, nil))
		if err != nil {
			return nil, 0, err
		}
		frames = append(frames, f.Bytes())
	}
	return frames, lastSeq, nil
}

func (e *EmuState) nextFragTag() uint16 {
	e.fragTag++
	return e.fragTag
}

func randomFragTagSeed() uint16 {
	b, err := randBytes(2)
	if err == nil {
		return binary.BigEndian.Uint16(b)
	}
	n := time.Now().UnixNano()
	return uint16(n) ^ uint16(n>>16) ^ uint16(n>>32)
}

// confirmBySeq closes the in-flight downlink acknowledged at the MAC layer.
func (e *EmuState) confirmBySeq(dstEUI []byte, seq byte) {
	key := hexEUI(dstEUI)
	q := e.queue[key]
	if len(q) == 0 || !q[0].inFlight || q[0].fragmented || q[0].kind == "get" || q[0].requireCoAP || q[0].seq != seq {
		return
	}
	e.promoteHeatingBlock(dstEUI, q[0].heating)
	e.queue[key] = q[1:]
	e.stats.DownlinksDelivered++
	e.recordDownlinkOutcome(key, q[0], "delivered", "enhanced-ack")
}

func (e *EmuState) RevertDownlinkAttempt(dstEUI []byte, seq byte) bool {
	key := hexEUI(dstEUI)
	q := e.queue[key]
	if len(q) == 0 || !q[0].inFlight || q[0].seq != seq || q[0].tries == 0 {
		return false
	}
	q[0].tries--
	q[0].retryAt = time.Time{}
	q[0].deferred = false
	q[0].awaitingCSL = false
	if q[0].tries == 0 {
		q[0].inFlight = false
	}
	e.stats.DownlinksSent--
	return true
}

// DeferDownlinkAttempt restores accounting and keeps the logical command held
// until retryAt, so an unrelated uplink cannot build a new frame early.
func (e *EmuState) DeferDownlinkAttempt(dstEUI []byte, seq byte, retryAt time.Time) bool {
	key := hexEUI(dstEUI)
	q := e.queue[key]
	if retryAt.IsZero() || len(q) == 0 || !q[0].inFlight || q[0].seq != seq || q[0].tries == 0 || q[0].deferred {
		return false
	}
	q[0].tries--
	q[0].inFlight = true
	q[0].retryAt = retryAt
	q[0].deferred = true
	q[0].awaitingCSL = false
	e.stats.DownlinksSent--
	return true
}

// confirmByMID closes an in-flight downlink after its CoAP response.
func (e *EmuState) confirmByMID(srcEUI []byte, m *CoAPMessage) {
	key := hexEUI(srcEUI)
	q := e.queue[key]
	if len(q) == 0 || !q[0].inFlight || q[0].mid != m.MID {
		return
	}
	if q[0].kind == fwKind {
		e.confirmFw(key, q[0], m)
		return
	}
	if q[0].requireCoAP {
		d := q[0]
		if m.Type != "ACK" || !bytes.Equal(m.Token, d.responseToken) {
			return
		}
		code := d.responseCode
		if code == "" {
			code = "2.04"
		}
		// A retransmitted d/calibrate queues a second stroke, so any 2.xx closes
		// it: the 2.05 comes from the firmware, not from a capture.
		calibrated := d.kind == "calibrate" && strings.HasPrefix(m.Code, "2.")
		if m.Code != code && !calibrated {
			if strings.HasPrefix(m.Code, "4.") {
				if d.heating != nil {
					e.invalidateHeatingBlock(srcEUI)
				}
				e.queue[key] = q[1:]
				e.stats.DownlinksDropped++
				e.recordDownlinkOutcome(key, d, "dropped", "coap-"+m.Code)
			}
			return
		}
	} else if q[0].kind == "get" {
		complete, accepted := e.recordGetResult(srcEUI, m)
		if !accepted || !complete {
			return
		}
	} else if m.Type != "ACK" {
		return
	}
	if q[0].heating != nil {
		if strings.HasPrefix(m.Code, "2.") {
			e.promoteHeatingBlock(srcEUI, q[0].heating)
		} else {
			e.invalidateHeatingBlock(srcEUI)
		}
	}
	if q[0].dhw != nil && strings.HasPrefix(m.Code, "2.") {
		e.promoteDHWBlock(q[0].dhw)
	}
	e.queue[key] = q[1:]
	e.stats.DownlinksDelivered++
	reason := "coap-ack"
	if q[0].kind == "calibrate" {
		reason = "coap-" + m.Code
	}
	e.recordDownlinkOutcome(key, q[0], "delivered", reason)
}

// ScheduleDownlink returns the cold-start action list for target: a CSL wake-up
// train followed by the queued Data frame at the rendezvous. Best-effort, see
// the timing caveat in the package comment.
func (e *EmuState) ScheduleDownlink(target string) ([]Action, error) {
	wire, label, err := e.resolve(target)
	if err != nil {
		return nil, err
	}
	key := hexEUI(wire)
	if len(e.queue[key]) == 0 {
		return nil, errors.New("tado: no downlink queued for " + target)
	}
	d := e.queue[key][0]
	frames, seq, err := e.buildFrames(wire, d)
	if err != nil {
		return nil, err
	}
	if len(frames) != 1 {
		return nil, errors.New("tado: cold-start does not support fragmented downlinks")
	}
	d.tries++
	d.seq = seq
	d.inFlight = true
	e.stats.DownlinksSent++

	// The train shares the Data frame's seq: one transaction. Unverified, no
	// capture pairs a wake-up train with the Data frame that follows it.
	short := []byte{wire[1], wire[0]}
	step := uint16(CSLPeriodUnits / (wakeUpTrainLen - 1))
	train := BuildWakeUpTrain(seq, short, wakeUpTrainLen, CSLPeriodUnits, step)
	actions := make([]Action, 0, len(train)+1)
	for i, w := range train {
		actions = append(actions, Action{
			Kind:   ActWakeUp,
			Bytes:  w,
			Reason: fmt.Sprintf("wake-up %d/%d seq=%02x", i+1, len(train), seq),
			Node:   label,
		})
	}
	return append(actions, Action{
		Kind:   ActData,
		Bytes:  frames[0],
		Reason: "downlink " + d.desc + fmt.Sprintf(" seq=%02x try=%d (cold start)", seq, d.tries),
		Node:   label,
	}), nil
}

// Beacon builds the periodic 0x49e8 broadcast: the ICMPv6 Router Advertisement
// the real bridge sends, rebuilt field by field. Only the 2-byte app trailer
// differs from a captured RA, its content being unexplained.
func (e *EmuState) Beacon() (Action, error) {
	pkt, err := BuildRouterAdvert(e.selfEUI, BridgeRouterAdvert(e.selfEUI))
	if err != nil {
		return Action{}, err
	}
	if e.bcastCt == ^uint32(0) {
		return Action{}, ErrBroadcastCounterExhausted
	}
	e.bcastCt++
	frame, err := BuildBroadcast(e.key, e.nextSeq(), e.selfEUI, uint16(e.bcastCt), bcastSecCtrl,
		WrapBroadcastPayload(pkt, uint16(e.bcastCt>>16)))
	if err != nil {
		return Action{}, err
	}
	return Action{
		Kind:   ActBroadcast,
		Bytes:  frame,
		Reason: fmt.Sprintf("beacon router-advert ctr=%d", e.bcastCt),
		Node:   "ffff",
	}, nil
}

func (e *EmuState) BeaconBurst() ([]Action, error) {
	a, err := e.Beacon()
	if err != nil {
		return nil, err
	}
	train := BuildRouterAdvertWakeUpBurst(a.Bytes[2])
	e.routerAdvertPending = true
	actions := make([]Action, 0, len(train)+1)
	for i, frame := range train {
		actions = append(actions, Action{
			Kind:   ActWakeUp,
			Bytes:  frame,
			Reason: fmt.Sprintf("beacon wake-up %d/%d seq=%02x", i+1, len(train), a.Bytes[2]),
			Node:   "ffff",
		})
	}
	return append(actions, a), nil
}

func (e *EmuState) CompleteBeacon(sent bool) {
	if !e.routerAdvertPending {
		return
	}
	e.routerAdvertPending = false
	if !sent {
		return
	}
	e.lastRouterAdvert = e.cfg.Now()
	e.routerAdvertSent = true
	e.stats.RouterAdvertisementsSent++
}

// routerSolicitAnswerWindow bounds the answer to one burst. Devices that lose
// the router solicit together: a capture holds 180 solicitations in 75 s.
const routerSolicitAnswerWindow = 5 * time.Second

// MaybeBeacon returns the periodic burst when the configured interval is due.
func (e *EmuState) MaybeBeacon() ([]Action, bool, error) {
	if e.cfg.RouterAdvertInterval <= 0 {
		return nil, false, nil
	}
	return e.beaconIfDue(e.cfg.RouterAdvertInterval)
}

// beaconIfDue returns the burst when the last advertisement on air is older
// than within. A burst still in flight and a bridge on air both hold it back.
func (e *EmuState) beaconIfDue(within time.Duration) ([]Action, bool, error) {
	if e.BridgeSeen() || e.routerAdvertPending {
		return nil, false, nil
	}
	now := e.cfg.Now()
	if e.routerAdvertSent && now.Before(e.lastRouterAdvert.Add(within)) {
		return nil, false, nil
	}
	actions, err := e.BeaconBurst()
	if err != nil {
		return nil, false, err
	}
	return actions, true, nil
}

// SetTXArmed installs the gate that says whether a frame decided now reaches
// the air. It guards the answer to a Router Solicitation, which a dry-run logs
// instead. nil is not armed, which is what the offline tools want.
func (e *EmuState) SetTXArmed(fn func() bool) { e.txArmed = fn }

func (e *EmuState) armed() bool { return e.txArmed != nil && e.txArmed() }

// --- helpers ---

func (e *EmuState) node(wireEUI []byte) *NodeState {
	key := hexEUI(wireEUI)
	if ns, ok := e.nodes[key]; ok {
		return ns
	}
	ns := &NodeState{
		WireLE:    key,
		Short:     ShortFromWire(wireEUI),
		Label:     e.name(wireEUI),
		Telemetry: map[string]string{},
		raw:       map[string][]byte{},
	}
	if s, ok := e.sessions[key]; ok {
		ns.SessionID = append([]byte(nil), s.id...)
	}
	e.nodes[key] = ns
	return ns
}

// resolve maps a command target (serial, short address, room label or wire EUI)
// to its address. An ambiguous room label is returned as *AmbiguousRefError so
// the caller can list the candidates instead of guessing one.
func (e *EmuState) resolve(target string) (wire []byte, label string, err error) {
	if e.inst != nil {
		d, err := e.inst.Resolve(target)
		if err == nil {
			b := d.WireEUI()
			if len(b) != 8 {
				return nil, "", errors.New("tado: unusable EUI for " + target)
			}
			return b, deviceLabel(d), nil
		}
		var amb *AmbiguousRefError
		if errors.As(err, &amb) {
			return nil, "", err
		}
	}
	if n := NodeByAny(target); n != nil {
		b, derr := hex.DecodeString(n.WireLE)
		if derr != nil || len(b) != 8 {
			return nil, "", errors.New("tado: bad inventory entry for " + target)
		}
		return b, labelOf(n), nil
	}
	b, derr := hex.DecodeString(strings.TrimSpace(target))
	if derr != nil || len(b) != 8 {
		return nil, "", errors.New("tado: unknown target " + target)
	}
	return b, e.name(b), nil
}

// name labels a node for a log line, through the installation when there is one.
func (e *EmuState) name(wireEUI []byte) string { return nameOf(e.inst, wireEUI) }

// known reports whether an address or short address belongs to the installation.
func (e *EmuState) known(ref string) bool {
	if e.inst != nil {
		_, err := e.inst.Resolve(ref)
		return err == nil
	}
	return NodeByAny(ref) != nil
}

func (e *EmuState) nextSeq() byte { e.seq++; return e.seq }

func (e *EmuState) reserveTx(n int) error {
	if n < 1 || uint64(e.tx)+uint64(n) > uint64(^uint32(0)) {
		return ErrTxCounterExhausted
	}
	return nil
}

func (e *EmuState) nextTx() (uint32, error) {
	if err := e.reserveTx(1); err != nil {
		return 0, err
	}
	e.tx++
	return e.tx, nil
}

// coapOrNil is nil-safe access to the decoded CoAP message of a plaintext.
func (p *AppPayload) coapOrNil() *CoAPMessage {
	if p == nil {
		return nil
	}
	return p.CoAP
}

// isDirectPath reports whether a message travels the plain link-local (IPHC
// 7e33) path, which BuildCoAPAck answers. The zone path compresses both
// endpoints against 6LoWPAN context 0 instead and BuildContextAck answers it.
func isDirectPath(m *CoAPMessage) bool { return m.LinkLocal() }

// servedFlowCircuit returns the heating-circuit id of a GET on
// "h/<home>/c/<id>/config", the resource the wall thermostat polls for its CH
// flow setpoint (tag 2040). It reports false for anything else, so only a GET of
// that resource is answered with a served flow value.
func servedFlowCircuit(m *CoAPMessage) (int, bool) {
	if m.Code != "0.01" { // GET only
		return 0, false
	}
	segs := strings.Split(m.Path, "/")
	for i := 0; i+2 < len(segs); i++ {
		if segs[i] == "c" && segs[i+2] == "config" {
			if n, err := strconv.Atoi(segs[i+1]); err == nil {
				return n, true
			}
		}
	}
	return 0, false
}

// addressedToSelf reports whether the packet's IPv6 destination is our own
// address. The L2 destination is already us here, so this separates a request
// made to the bridge from one merely relayed through it: the two differ only in
// the destination IID the 6LoWPAN header carries.
func (e *EmuState) addressedToSelf(m *CoAPMessage, f *Frame) bool {
	dst := m.Dst.IPv6(f.DstEUI)
	return dst != nil && bytes.Equal(dst[8:], modifiedEUI64(e.selfEUI))
}

func isRequestCode(code string) bool { return strings.HasPrefix(code, "0.") && code != "0.00" }

func knownMethod(code string) bool {
	switch code {
	case "0.01", "0.02", "0.03", "0.04": // GET, POST, PUT, DELETE
		return true
	}
	return false
}

// knownResources is the CoAP resource catalog, matched after normalizeResource
// strips the instance segments. The second group is what the captures actually
// carry; extend either group as new resources turn up in the logs.
var knownResources = map[string]bool{
	"z/s": true, "z/p": true, "z/config": true, "z/activity": true, "z/cpe": true,
	"z/extui": true, "z/legionella": true, "z/sched": true,
	"d/config": true, "d/lock": true, "d/sched": true, "d/calibrate": true, "d/currentlog": true,
	"d/dispsettings": true, "d/fw": true, "d/fw/state": true, "d/fw/rq": true,
	"d/err": true, "d/info": true, "d/identify": true, "d/reboot": true,
	"d/test/v1": true, "d/dbg/st": true, "d/dbg/m": true, "d/dbg2/tlvs": true,
	"d/hvac/testinterface": true, "d/hvac/trigger": true,
	"auth/key": true, "auth/token": true,
	"netcmd": true, "sen": true, "act": true, "fallback": true,

	"hvac/mon": true, "hvac/mon/dhw": true, "z/ow": true,
	"hvac/config": true, "hvac/maint": true, "hvac/codes": true,
	"c/config":     true, // heating-circuit config (carries the flow setpoint, tag 2040)
	"hvac/det/bus": true, "hvac/mon/hp": true, "hvac/mon/hc": true,
	"heatsource/schedule": true, "heatsource/capabilities": true,

	// Remaining catalog fragments seen in the captures or the firmware map. Each
	// is a distinctive leaf, so the trailing-segment match in knownResource
	// covers both the h/<home>/<zone> and the d/<serial> prefixes without
	// over-matching an unrelated path.
	"d/pair": true, "time": true, "mnt": true,
	"neighbors": true, "selftest/result": true, "overlay": true,
	"drv": true, "beep": true, "fw/request": true,
}

// normalizeResource drops the instance segments of a path so the catalog can be
// written once: the h/<home> family prefix, numeric ids, device serials.
func normalizeResource(path string) string {
	if path == "" {
		return ""
	}
	segs := strings.Split(path, "/")
	out := make([]string, 0, len(segs))
	for i, s := range segs {
		switch {
		case s == "":
		case i == 0 && s == "h":
		case isAllDigits(s):
		case reSerial.MatchString(s):
		default:
			out = append(out, s)
		}
	}
	return strings.Join(out, "/")
}

// knownResource matches the normalized path, then each of its trailing segment
// runs, so a catalog entry like "act" covers "c/act" and "z/act".
func knownResource(path string) bool {
	n := normalizeResource(path)
	if n == "" {
		return false
	}
	if knownResources[n] {
		return true
	}
	segs := strings.Split(n, "/")
	for i := 1; i < len(segs); i++ {
		if knownResources[strings.Join(segs[i:], "/")] {
			return true
		}
	}
	return false
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// --- unhandled logger ---

// Reason kinds. A reason may carry a detail suffix, so Counts keys give
// per-tag / per-resource totals.
const (
	rsnMICFail           = "mic-fail"
	rsnMalformed         = "malformed"
	rsnUnclassified      = "unclassified-fcf"
	rsnNotForUs          = "not-addressed-to-us"
	rsnBridgeSrc         = "bridge-sourced-data"
	rsnAppParse          = "app-parse-fail"
	rsnNoCoAP            = "no-coap-in-plaintext"
	rsnUnknownTag        = "unknown-tlv-tag"
	rsnUnknownRes        = "unknown-coap-resource"
	rsnUnhandledMethod   = "unhandled-coap-method"
	rsnRelayedPath       = "relayed-not-addressed-to-us"
	rsnZoneUnroutable    = "zone-destination-not-on-pan"
	rsnZoneIncomplete    = "zone-datagram-incomplete"
	rsnZoneTarget        = "zone-config-unknown-target"
	rsnZoneProxyTimeout  = "zone-proxy-timeout"
	rsnTimeSuppressed    = "fault-time-suppressed"
	rsnTimeDelayed       = "fault-time-delayed"
	rsnZoneIDUnverified  = "zone-id-unverified"
	rsnUnknownAddrMode   = "unsupported-6lowpan-addressing"
	rsnNoMID             = "coap-con-without-mid"
	rsnForeignPAN        = "foreign-pan"
	rsnUnknownWakeTarget = "unknown-wakeup-target"
	rsnForeignBroadcast  = "foreign-broadcast"
	rsnRouterSolicit     = "icmpv6-router-solicitation"
	rsnRouterAdvert      = "icmpv6-router-advertisement"
	rsnNeighborSolicit   = "icmpv6-neighbor-solicitation"
	rsnUnknownEnhAck     = "enh-ack-from-unknown-node"
	rsnDownlinkLost      = "downlink-undelivered"
	rsnBuildFail         = "build-fail"
	rsnAuthAccepted      = "auth-accepted"
	rsnSessionMismatch   = "session-id-mismatch"
	rsnSessionMissing    = "session-id-missing"
	rsnSessionAdopted    = "session-id-adopted"
	rsnUTCOffsetPush     = "utc-offset-pushed"
	rsnUTCOffsetChange   = "utc-offset-changed"
	rsnBlock2Dropped     = "coap-block2-dropped"
	rsnConfigWithheld    = "config-withheld"
	rsnModuleLearned     = "interface-module-learned"
	rsnModuleRefused     = "interface-module-refused"
	rsnModuleWarning     = "interface-module-warning"
	rsnModuleDropped     = "interface-module-dropped"
)

// UnhandledLogger is the structured sink for everything the emulator cannot
// fully handle. It is safe for concurrent use.
type UnhandledLogger struct {
	mu     sync.Mutex
	w      io.Writer
	counts map[string]int
}

// NewUnhandledLogger writes one line per event to w. A nil w discards.
func NewUnhandledLogger(w io.Writer) *UnhandledLogger {
	if w == nil {
		w = io.Discard
	}
	return &UnhandledLogger{w: w, counts: map[string]int{}}
}

// Log records one event: RFC3339 timestamp, frame class, src, dst, reason, hex.
func (l *UnhandledLogger) Log(class, src, dst, reason, hexFrame string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.counts[reason]++
	fmt.Fprintf(l.w, "%s\t%s\t%s\t%s\t%s\t%s\n",
		time.Now().Format(time.RFC3339), class, dash(src), dash(dst), reason, dash(hexFrame))
}

// Counts returns a copy of the per-reason event counts.
func (l *UnhandledLogger) Counts() map[string]int {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]int, len(l.counts))
	for k, v := range l.counts {
		out[k] = v
	}
	return out
}

// Total returns the number of logged events.
func (l *UnhandledLogger) Total() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, v := range l.counts {
		n += v
	}
	return n
}

// Summary returns the running "reason: N" list, sorted by reason.
func (l *UnhandledLogger) Summary() string {
	c := l.Counts()
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s: %d\n", k, c[k])
	}
	return b.String()
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
