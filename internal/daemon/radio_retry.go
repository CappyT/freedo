package daemon

import (
	"context"
	"errors"
	"log"
	"time"

	"freedo/cc1101"
)

// radioRetryEvery is the wait between two tries to open and configure a bridge
// that did not start. Tests shorten it.
var radioRetryEvery = time.Minute

// fault is why the radio did not start, or "".
func (d *daemon) fault() string {
	d.faultMu.Lock()
	defer d.faultMu.Unlock()
	return d.radioFault
}

func (d *daemon) setFault(fault string) {
	d.faultMu.Lock()
	defer d.faultMu.Unlock()
	d.radioFault = fault
}

// probeBridge opens the bridge and runs the whole start check as the start does:
// the register load and the calibration seed with its synthesizer lock. Then it
// closes the bridge. A nil result says that the real start can work now.
func probeBridge(store *stateStore, port string, set bridgeSettings) error {
	br, _, err := openConfiguredBridge(store, port, set)
	if err != nil {
		return err
	}
	return br.Close()
}

// retryRadio tries to open and configure the bridge every radioRetryEvery while
// the daemon runs on a mock radio. A try that works asks main to restart: the
// start then runs the normal path (the register load, the airtime preload,
// the calibration seed and the --tx setting) on one owner of the serial port, and no
// radio swaps under the engine loop. A bridge with old firmware stops the retry:
// it needs a flash. It ends with ctx.
func (d *daemon) retryRadio(ctx context.Context, probe func() error) {
	tick := time.NewTicker(radioRetryEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		err := probe()
		var fw *cc1101.FirmwareError
		switch {
		case err == nil:
			log.Printf("radio: the bridge opens and configures")
			select {
			case d.radioReady <- struct{}{}:
			default:
			}
			return
		case errors.As(err, &fw):
			d.setFault(err.Error())
			log.Printf("radio: %v: no more tries", err)
			return
		}
		d.setFault(retryFault(err))
		log.Printf("radio: the bridge does not start: %v", err)
	}
}
