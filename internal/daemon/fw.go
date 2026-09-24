package daemon

// fw: the over-the-air firmware transfer of one valve or thermostat. The HTTP
// handler runs the planner's refusals against the device's latest fw/state,
// then one goroutine drives tado.SendOTA through the downlink queue, one block
// in flight, and after the last block waits for the reboot and verifies it.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"freedo/tado"
)

const (
	// fwStateMaxAge bounds the age of the fw/state a transfer plans against.
	fwStateMaxAge = 5 * time.Minute
	// fwUploadMaxBody fits the largest image, base64 in JSON, with room to spare.
	fwUploadMaxBody = 1 << 20
)

// fwTiming holds the transfer's waits. The device keeps its receiver on for
// 60 s after each block and drops the upload after 10 minutes idle, so one
// attempt stays inside the first and every retry of a block inside the second.
type fwTiming struct {
	block   time.Duration // one POST d/fw attempt
	retries int           // extra attempts of one block
	abort   time.Duration // the DELETE d/fw exchange
	reboot  time.Duration // the fw/state PUT after the last block
	read    time.Duration // a confirmed fw/state after that PUT
	reread  time.Duration // between two GETs of d/fw/state
	poll    time.Duration
}

var defaultFwTiming = fwTiming{
	block: 45 * time.Second, retries: 3, abort: 45 * time.Second,
	reboot: 15 * time.Minute, read: 5 * time.Minute, reread: 20 * time.Second,
	poll: 100 * time.Millisecond,
}

var errFwBusy = errors.New("a firmware transfer is already running")

const (
	fwSending    = "sending"
	fwCancelling = "cancelling"
	fwRebooting  = "rebooting"
	fwVerifying  = "verifying"
	fwDone       = "done"
	fwFailed     = "failed"
	fwCancelled  = "cancelled"
)

// fwStatus is what GET /api/nodes/{id}/fw shows under "transfer".
type fwStatus struct {
	Device   string `json:"device"`
	State    string `json:"state"`
	From     string `json:"from"`
	To       string `json:"to"`
	Bytes    int    `json:"bytes"`
	CRC      string `json:"crc"`
	Blocks   int    `json:"blocks"`
	Acked    int    `json:"acked"`
	Resends  int    `json:"resends"`
	LastCode string `json:"lastCode,omitempty"`
	Error    string `json:"error,omitempty"`
	Note     string `json:"note,omitempty"`
	// Reported is the fw/state the device PUT after the reboot, Result the one
	// it answered to the GET that followed.
	Reported *tado.FwStateView `json:"reported,omitempty"`
	Result   *tado.FwStateView `json:"result,omitempty"`
	Started  time.Time         `json:"startedAt"`
	Updated  time.Time         `json:"updatedAt"`
	Finished *time.Time        `json:"finishedAt,omitempty"`
}

type fwTransfer struct {
	target string // wire EUI hex
	want   tado.FwVersion
	blocks [][]byte
	cancel context.CancelFunc
	done   chan struct{}
	status fwStatus // guarded by daemon.fwMu
}

func (t *fwTransfer) active() bool {
	switch t.status.State {
	case fwDone, fwFailed, fwCancelled:
		return false
	}
	return true
}

// postNodeFw starts a transfer. It needs {"confirm":true}, the version M.m the
// header carries, the image kind and the image, base64.
func (d *daemon) postNodeFw(w http.ResponseWriter, r *http.Request) {
	dev, target, ok := d.pathNode(w, r)
	if !ok {
		return
	}
	var b struct {
		Confirm   bool   `json:"confirm"`
		Version   string `json:"version"`
		ImageKind string `json:"imageKind"`
		Image     []byte `json:"image"`
	}
	if !decodeBodyLimit(w, r, &b, fwUploadMaxBody) {
		return
	}
	if !b.Confirm {
		writeErr(w, http.StatusBadRequest, `firmware transfer needs {"confirm":true}`)
		return
	}
	v, err := tado.ParseFwVersion(b.Version)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if !d.txArmed() {
		writeErr(w, http.StatusConflict, "transmit is not armed: a dry-run sends nothing")
		return
	}
	st, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		if e.BridgeSeen() {
			return nil, errTXBlocked
		}
		state, err := e.FreshFwState(target, fwStateMaxAge)
		if err != nil {
			return nil, err
		}
		plan, err := tado.PlanDeviceOTA(dev, b.ImageKind, state, b.Image, v)
		if err != nil {
			return nil, err
		}
		return d.startFw(e, dev, target, plan)
	})
	if errors.Is(err, errFwBusy) || errors.Is(err, errTXBlocked) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if writeHandlerErr(w, err) {
		return
	}
	writeJSON(w, http.StatusAccepted, st)
}

// startFw runs on the engine goroutine: the busy check, the queue lock and the
// registration happen together.
func (d *daemon) startFw(e *tado.EmuState, dev *tado.Device, target string, plan *tado.OTAPlan) (fwStatus, error) {
	d.fwMu.Lock()
	defer d.fwMu.Unlock()
	for _, t := range d.fw {
		if t.active() {
			return fwStatus{}, fmt.Errorf("%w: %s", errFwBusy, t.status.Device)
		}
	}
	if err := e.LockFirmware(target); err != nil {
		return fwStatus{}, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	now := time.Now()
	img := plan.Image
	t := &fwTransfer{
		target: target, want: img.Version, blocks: img.Blocks(), cancel: cancel, done: make(chan struct{}),
		status: fwStatus{
			Device: dev.Ref(), State: fwSending, From: plan.Running.String(), To: img.Version.String(),
			Bytes: len(img.Image), CRC: fmt.Sprintf("%#04x", img.CRC), Blocks: plan.Blocks,
			Started: now, Updated: now,
		},
	}
	d.fw[target] = t
	go d.runFw(ctx, t)
	return t.status, nil
}

// deleteNodeFw cancels a running transfer. While blocks are going out the
// upload ends with DELETE d/fw; after the last block it only stops watching.
func (d *daemon) deleteNodeFw(w http.ResponseWriter, r *http.Request) {
	_, target, ok := d.pathNode(w, r)
	if !ok {
		return
	}
	d.fwMu.Lock()
	t := d.fw[target]
	if t == nil || !t.active() {
		d.fwMu.Unlock()
		writeErr(w, http.StatusNotFound, "no firmware transfer running")
		return
	}
	if t.status.State == fwSending {
		t.status.State = fwCancelling
		t.status.Updated = time.Now()
	}
	st := t.status
	d.fwMu.Unlock()
	t.cancel()
	writeJSON(w, http.StatusAccepted, st)
}

// fwView adds the transfer of target, running or last finished, to a GET.
func (d *daemon) fwView(target string, out map[string]any) {
	d.fwMu.Lock()
	defer d.fwMu.Unlock()
	if t := d.fw[target]; t != nil {
		out["transfer"] = t.status
	}
}

func (d *daemon) fwSet(t *fwTransfer, fn func(*fwStatus)) {
	d.fwMu.Lock()
	defer d.fwMu.Unlock()
	fn(&t.status)
	t.status.Updated = time.Now()
}

func (d *daemon) fwFinish(t *fwTransfer, state string, err error) {
	d.fwSet(t, func(s *fwStatus) {
		s.State = state
		if err != nil {
			s.Error = err.Error()
		}
		now := time.Now()
		s.Finished = &now
	})
}

func (d *daemon) runFw(ctx context.Context, t *fwTransfer) {
	defer close(t.done)
	defer t.cancel()
	tr := &fwTransport{d: d, t: t, last: -1}
	err := tado.SendOTA(ctx, tr, t.blocks, d.fwTiming.retries, func(num int) {
		d.fwSet(t, func(s *fwStatus) { s.Acked = num + 1 })
	})
	// The held commands go out now, and the verification GET needs the queue.
	if _, unlockErr := d.do(func(e *tado.EmuState) (any, error) {
		if e.UnlockFirmware(t.target) {
			d.requestQueuedDispatch(t.target)
		}
		return nil, nil
	}); unlockErr != nil {
		d.fwFinish(t, fwFailed, unlockErr)
		return
	}
	switch {
	case ctx.Err() != nil:
		d.fwFinish(t, fwCancelled, err)
		return
	case errors.Is(err, tado.ErrOTAUnverified):
		d.fwSet(t, func(s *fwStatus) { s.Note = err.Error() })
	case err != nil:
		d.fwFinish(t, fwFailed, err)
		return
	}
	d.fwSet(t, func(s *fwStatus) { s.State = fwRebooting })
	put, err := d.awaitFwReboot(ctx, t, tr.putSeq)
	if err == nil {
		d.fwSet(t, func(s *fwStatus) { s.State = fwVerifying })
		err = d.verifyFw(ctx, t, put.Seq)
	}
	switch {
	case ctx.Err() != nil:
		d.fwFinish(t, fwCancelled, nil)
	case err != nil:
		d.fwFinish(t, fwFailed, err)
	default:
		d.fwFinish(t, fwDone, nil)
	}
}

// awaitFwReboot waits for the fw/state PUT of the new image. The device also
// PUTs on a slot change, which still names the old running image.
func (d *daemon) awaitFwReboot(ctx context.Context, t *fwTransfer, after uint64) (tado.FwStateReport, error) {
	var put tado.FwStateReport
	err := d.fwPoll(ctx, d.fwTiming.reboot, func(e *tado.EmuState) bool {
		p, _ := e.FwStateReports(t.target)
		if p.Seq <= after {
			return false
		}
		after = p.Seq
		state, err := tado.ParseFwState(p.Body)
		if err != nil {
			return false
		}
		view := state.View()
		d.fwSet(t, func(s *fwStatus) { s.Reported = &view })
		if state.Running.Version != t.want {
			return false
		}
		put = p
		return true
	})
	if err != nil {
		return put, fmt.Errorf("no fw/state PUT running %s: %w", t.want, err)
	}
	return put, nil
}

// verifyFw reads d/fw/state until it shows the new image confirmed. The
// device confirms on the first 2.xx answer to its PUT, which the engine gave.
func (d *daemon) verifyFw(ctx context.Context, t *fwTransfer, after uint64) error {
	var verr error
	var askedAt time.Time
	err := d.fwPoll(ctx, d.fwTiming.read, func(e *tado.EmuState) bool {
		_, g := e.FwStateReports(t.target)
		if g.Seq > after {
			after = g.Seq
			state, err := tado.ParseFwState(g.Body)
			if err != nil {
				verr = err
				return false
			}
			view := state.View()
			d.fwSet(t, func(s *fwStatus) { s.Result = &view })
			if verr = tado.VerifyOTA(state, t.want); verr == nil {
				return true
			}
			if state.Running.Version != t.want {
				return true
			}
		}
		if now := time.Now(); now.Sub(askedAt) >= d.fwTiming.reread {
			askedAt = now
			if err := e.EnqueueGet(t.target, "d/fw/state", []string{"d", "fw", "state"}, nil); err != nil {
				verr = err
				return false
			}
			d.dispatchQueued(e, t.target)
		}
		return false
	})
	if verr != nil {
		return verr
	}
	return err
}

// fwPoll calls check on the engine goroutine until it reports true.
func (d *daemon) fwPoll(ctx context.Context, timeout time.Duration, check func(*tado.EmuState) bool) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(d.fwTiming.poll)
	defer tick.Stop()
	for {
		v, err := d.do(func(e *tado.EmuState) (any, error) { return check(e), nil })
		if err != nil {
			return err
		}
		if v.(bool) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-d.done:
			return errStopped
		case <-deadline.C:
			return fmt.Errorf("nothing within %s", timeout)
		case <-tick.C:
		}
	}
}

// fwTransport carries SendOTA's exchanges through the downlink queue.
type fwTransport struct {
	d    *daemon
	t    *fwTransfer
	last int
	// putSeq is the device's last fw/state PUT before the last block went out.
	putSeq uint64
}

func (tr *fwTransport) PostBlock(ctx context.Context, num int, more bool, data []byte) (tado.OTAReply, error) {
	d, target := tr.d, tr.t.target
	resend := num == tr.last
	tr.last = num
	if resend {
		d.fwSet(tr.t, func(s *fwStatus) { s.Resends++ })
	}
	v, err := d.do(func(e *tado.EmuState) (any, error) {
		if !more && !resend {
			put, _ := e.FwStateReports(target)
			tr.putSeq = put.Seq
		}
		mid, err := e.EnqueueFwBlock(target, num, more, data)
		if err != nil {
			return nil, err
		}
		d.dispatchQueued(e, target)
		return mid, nil
	})
	if err != nil {
		return tado.OTAReply{}, err
	}
	return tr.await(ctx, v.(uint16), d.fwTiming.block)
}

func (tr *fwTransport) Abort() error {
	d, target := tr.d, tr.t.target
	v, err := d.do(func(e *tado.EmuState) (any, error) {
		mid, err := e.EnqueueFwAbort(target)
		if err != nil {
			return nil, err
		}
		d.dispatchQueued(e, target)
		return mid, nil
	})
	if err != nil {
		return err
	}
	reply, err := tr.await(context.Background(), v.(uint16), d.fwTiming.abort)
	if err != nil {
		return err
	}
	if reply.Code != "2.02" {
		return fmt.Errorf("DELETE d/fw answered %s, want 2.02", reply.Code)
	}
	return nil
}

// await waits for the answer to the exchange mid. On timeout or cancellation
// it withdraws the exchange, unless the answer arrived meanwhile.
func (tr *fwTransport) await(ctx context.Context, mid uint16, timeout time.Duration) (tado.OTAReply, error) {
	d, target := tr.d, tr.t.target
	var reply tado.FwReply
	var answered bool
	err := d.fwPoll(ctx, timeout, func(e *tado.EmuState) bool {
		reply, answered = e.FwReply(target, mid)
		return answered
	})
	if err != nil && !errors.Is(err, errStopped) {
		v, _ := d.do(func(e *tado.EmuState) (any, error) {
			if r, ok := e.FwReply(target, mid); ok {
				return r, nil
			}
			e.WithdrawFw(target, mid)
			return nil, nil
		})
		reply, answered = v.(tado.FwReply)
		if ctx.Err() != nil {
			return tado.OTAReply{}, ctx.Err()
		}
	}
	if !answered {
		return tado.OTAReply{}, fmt.Errorf("d/fw exchange %04x: %w", mid, err)
	}
	if reply.Code == "" {
		return tado.OTAReply{}, fmt.Errorf("d/fw exchange %04x: no answer: %s (%s)", mid, reply.Result, reply.Reason)
	}
	d.fwSet(tr.t, func(s *fwStatus) { s.LastCode = reply.Code })
	return tado.OTAReply{Code: reply.Code, Block1: reply.Block1}, nil
}
