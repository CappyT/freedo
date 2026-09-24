package tado

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

const DesiredStateVersion = 1

// DesiredConfigDocument is the complete d/config reply remembered for a node.
type DesiredConfigDocument struct {
	Body []byte `json:"body"`
	ETag []byte `json:"etag"`
}

// DesiredScheduleDocument is the complete d/sched reply remembered for a node.
type DesiredScheduleDocument struct {
	Body []byte `json:"body"`
	ETag []byte `json:"etag"`
}

// DesiredSession is the auth/token session id served to a device, and when.
type DesiredSession struct {
	ID       []byte    `json:"id"`
	IssuedAt time.Time `json:"issuedAt"`
}

// DesiredBoostTimer is the deadline of the overlay zone ID runs on one node.
// Its key is "<wire EUI>|<zone>". A bare wire EUI is the key of a store that
// kept one timer per node: the restore moves it to the zone ID names.
type DesiredBoostTimer struct {
	ID        int       `json:"id"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// DesiredState is the versioned, durable state served by the emulator.
type DesiredState struct {
	Version    int                                `json:"version"`
	ChildLock  map[string]bool                    `json:"childLock"`
	TempUnitF  map[string]bool                    `json:"tempUnitF"`
	FlowC      map[string]float64                 `json:"flowC"`
	Config     map[string]DesiredConfigDocument   `json:"config"`
	Schedule   map[string]DesiredScheduleDocument `json:"schedule"`
	ZoneState  map[string][]byte                  `json:"zoneState"`
	UTCSettled map[string]int                     `json:"utcSettled"`
	Boost      map[string]DesiredBoostTimer       `json:"boost,omitempty"`
	// OfflineSchedule holds the per-device tag 02b3 an operator wrote.
	OfflineSchedule map[string]bool `json:"offlineSchedule,omitempty"`
	// InterfaceModule holds the tag 015d each wall thermostat reported.
	InterfaceModule map[string]uint16 `json:"interfaceModule,omitempty"`
	// Session holds the last session id served to each device on auth/token.
	Session map[string]DesiredSession `json:"session,omitempty"`
}

// ExportDesiredState returns a detached snapshot safe for persistence.
func (e *EmuState) ExportDesiredState() DesiredState {
	out := DesiredState{
		Version:         DesiredStateVersion,
		ChildLock:       make(map[string]bool, len(e.childLock)),
		TempUnitF:       make(map[string]bool, len(e.tempUnitF)),
		FlowC:           make(map[string]float64, len(e.flowC)),
		Config:          make(map[string]DesiredConfigDocument, len(e.cfgDoc)),
		Schedule:        make(map[string]DesiredScheduleDocument, len(e.schedDoc)),
		ZoneState:       make(map[string][]byte, len(e.zoneState)),
		UTCSettled:      make(map[string]int, len(e.utcSettled)),
		Boost:           make(map[string]DesiredBoostTimer, len(e.boostTimers)),
		OfflineSchedule: cloneBoolMap(e.offlineSched),
		InterfaceModule: make(map[string]uint16, len(e.moduleID)),
		Session:         make(map[string]DesiredSession, len(e.sessions)),
	}
	for k, v := range e.sessions {
		out.Session[k] = DesiredSession{ID: append([]byte(nil), v.id...), IssuedAt: v.at}
	}
	for k, v := range e.moduleID {
		out.InterfaceModule[k] = v
	}
	for k, v := range e.childLock {
		out.ChildLock[k] = v
	}
	for k, v := range e.tempUnitF {
		out.TempUnitF[k] = v
	}
	for id, v := range e.flowC {
		out.FlowC[strconv.Itoa(id)] = v
	}
	for k, v := range e.cfgDoc {
		out.Config[k] = DesiredConfigDocument{Body: append([]byte(nil), v.body...), ETag: append([]byte(nil), v.etag...)}
	}
	for k, v := range e.schedDoc {
		out.Schedule[k] = DesiredScheduleDocument{Body: append([]byte(nil), v.body...), ETag: append([]byte(nil), v.etag...)}
	}
	for k, v := range e.zoneState {
		out.ZoneState[k] = append([]byte(nil), v...)
	}
	for k, v := range e.utcSettled {
		out.UTCSettled[k] = v
	}
	for k, v := range e.boostTimers {
		out.Boost[k] = DesiredBoostTimer{ID: v.id, ExpiresAt: v.expiresAt}
	}
	return out
}

// RestoreDesiredState validates and replaces the durable state from a snapshot.
func (e *EmuState) RestoreDesiredState(in DesiredState) error {
	if err := validateDesiredState(in); err != nil {
		return err
	}
	childLock := cloneBoolMap(in.ChildLock)
	tempUnitF := cloneBoolMap(in.TempUnitF)
	offlineSched := cloneBoolMap(in.OfflineSchedule)
	moduleID := make(map[string]uint16, len(in.InterfaceModule))
	dropped := map[string]InterfaceModuleIssue{}
	for key, value := range in.InterfaceModule {
		if err := validThermostatModule(value); err != nil {
			dropped[key] = InterfaceModuleIssue{Module: value, Family: InterfaceModuleFamily(value), Refused: true,
				Reason: "dropped from the stored state: " + err.Error()}
			continue
		}
		moduleID[key] = value
	}
	sessions := make(map[string]issuedSession, len(in.Session))
	for key, value := range in.Session {
		sessions[key] = issuedSession{id: append([]byte(nil), value.ID...), at: value.IssuedAt}
	}
	flowC := make(map[int]float64, len(in.FlowC))
	flowFresh := make(map[int]bool, len(in.FlowC))
	for key, value := range in.FlowC {
		id, _ := strconv.Atoi(key)
		flowC[id] = value
		flowFresh[id] = true
	}
	cfgDoc := make(map[string]cfgSnapshot, len(e.cfgDoc)+len(in.Config))
	for key, value := range e.cfgDoc {
		cfgDoc[key] = cfgSnapshot{body: append([]byte(nil), value.body...), etag: append([]byte(nil), value.etag...)}
	}
	for key, value := range in.Config {
		cfgDoc[key] = cfgSnapshot{body: append([]byte(nil), value.Body...), etag: append([]byte(nil), value.ETag...)}
	}
	schedDoc := make(map[string]cfgSnapshot, len(in.Schedule))
	for key, value := range in.Schedule {
		schedDoc[key] = cfgSnapshot{body: append([]byte(nil), value.Body...), etag: append([]byte(nil), value.ETag...)}
	}
	zoneState := make(map[string][]byte, len(in.ZoneState))
	for key, value := range in.ZoneState {
		zoneState[key] = append([]byte(nil), value...)
	}
	utcSettled := make(map[string]int, len(in.UTCSettled))
	for key, value := range in.UTCSettled {
		utcSettled[key] = value
	}
	e.childLock = childLock
	e.tempUnitF = tempUnitF
	e.offlineSched = offlineSched
	e.flowC = flowC
	e.flowFresh = flowFresh
	e.cfgDoc = cfgDoc
	e.schedDoc = schedDoc
	e.zoneState = zoneState
	e.utcSettled = utcSettled
	e.boostTimers = make(map[string]boostTimer, len(in.Boost))
	for key, value := range in.Boost {
		if strings.Contains(key, "|") {
			e.boostTimers[key] = e.restoredTimer(value)
		}
	}
	for key, value := range in.Boost {
		if strings.Contains(key, "|") {
			continue
		}
		zoned := overlayTimerKey(key, value.ID)
		if _, ok := e.boostTimers[zoned]; ok {
			e.log.Log("Config", key, "", rsnTimerDropped+"="+strconv.Itoa(value.ID), "")
			continue
		}
		e.boostTimers[zoned] = e.restoredTimer(value)
	}
	e.moduleID = moduleID
	for key, issue := range dropped {
		e.moduleIssue[key] = issue
		e.log.Log("Config", key, "", rsnModuleDropped+"="+strconv.Itoa(int(issue.Module)), "")
	}
	e.sessions = sessions
	for key, ns := range e.nodes {
		if s, ok := sessions[key]; ok {
			ns.SessionID = append([]byte(nil), s.id...)
		}
	}
	e.withheldLogged = map[string]bool{}
	e.refreshThermostatDocuments()
	return nil
}

func validateDesiredState(in DesiredState) error {
	if in.Version != DesiredStateVersion {
		return fmt.Errorf("tado: desired state version %d is unsupported", in.Version)
	}
	for _, entries := range []map[string]bool{in.ChildLock, in.TempUnitF, in.OfflineSchedule} {
		for key := range entries {
			if err := validateStateEUI(key); err != nil {
				return err
			}
		}
	}
	for key, value := range in.FlowC {
		id, err := strconv.Atoi(key)
		if err != nil || strconv.Itoa(id) != key || id < 0 {
			return fmt.Errorf("tado: desired state has invalid flow circuit %q", key)
		}
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("tado: desired state has invalid flow temperature for circuit %d", id)
		}
		if err := flowInRange(value); err != nil {
			return err
		}
	}
	for key, doc := range in.Config {
		if err := validateStateEUI(key); err != nil {
			return err
		}
		if err := validateDesiredConfig(doc); err != nil {
			return fmt.Errorf("tado: desired state config %s: %w", key, err)
		}
	}
	for key, doc := range in.Schedule {
		if err := validateStateEUI(key); err != nil {
			return err
		}
		if err := validateDesiredSchedule(doc); err != nil {
			return fmt.Errorf("tado: desired state schedule %s: %w", key, err)
		}
	}
	for key := range in.UTCSettled {
		if err := validateStateEUI(key); err != nil {
			return err
		}
	}
	for key := range in.InterfaceModule {
		if err := validateStateEUI(key); err != nil {
			return err
		}
	}
	for key, session := range in.Session {
		if err := validateStateEUI(key); err != nil {
			return err
		}
		if len(session.ID) < 4 || len(session.ID) > 8 || session.IssuedAt.IsZero() {
			return fmt.Errorf("tado: desired state has invalid session for %s", key)
		}
	}
	for key, timer := range in.Boost {
		eui, zone, zoned := strings.Cut(key, "|")
		if err := validateStateEUI(eui); err != nil {
			return err
		}
		if timer.ID < 0 || timer.ExpiresAt.IsZero() || zoned && zone != strconv.Itoa(timer.ID) {
			return fmt.Errorf("tado: desired state has invalid boost timer for %s", key)
		}
	}
	for key := range in.ZoneState {
		zone, resource, ok := strings.Cut(key, "|")
		id, err := strconv.Atoi(zone)
		if !ok || err != nil || strconv.Itoa(id) != zone || id < 0 || resource == "" {
			return fmt.Errorf("tado: desired state has invalid zone resource %q", key)
		}
	}
	return nil
}

func validateStateEUI(value string) error {
	b, err := hex.DecodeString(value)
	if err != nil || len(b) != 8 || hex.EncodeToString(b) != value {
		return fmt.Errorf("tado: desired state has invalid wire EUI %q", value)
	}
	return nil
}

func validateDesiredConfig(doc DesiredConfigDocument) error {
	if len(doc.ETag) == 0 || len(doc.ETag) > 8 {
		return errors.New("invalid ETag")
	}
	var tags int
	for i := 0; i < len(doc.Body); {
		if i+3 > len(doc.Body) {
			return errors.New("truncated TLV")
		}
		end := i + 3 + int(doc.Body[i+2])
		if end > len(doc.Body) {
			return errors.New("truncated TLV value")
		}
		if hex.EncodeToString(doc.Body[i:i+2]) == configEtagTag {
			tags++
			if !bytes.Equal(doc.Body[i+3:end], doc.ETag) {
				return errors.New("ETag does not match document")
			}
		}
		i = end
	}
	if tags != 1 {
		return errors.New("document must contain one ETag")
	}
	return nil
}

func validateDesiredSchedule(doc DesiredScheduleDocument) error {
	if len(doc.Body) == 0 || len(doc.Body) > 0x719 {
		return errors.New("invalid document length")
	}
	if len(doc.ETag) != 8 || !bytes.Equal(doc.ETag, configEtag(doc.Body)) {
		return errors.New("invalid ETag")
	}
	return nil
}

func cloneBoolMap(in map[string]bool) map[string]bool {
	out := make(map[string]bool, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
