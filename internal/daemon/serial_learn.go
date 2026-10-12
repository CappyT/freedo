package daemon

import (
	"context"
	"log"

	"freedo/tado"
)

// tickSerials writes into the installation the serial that a device declared
// without one names in its own requests. The write runs off the engine
// goroutine, because the new installation reaches the engine through d.do.
func (d *daemon) tickSerials(e *tado.EmuState) {
	learned := e.TakeLearnedSerials()
	if len(learned) == 0 || d.state == nil {
		return
	}
	go d.adoptSerials(learned)
}

// adoptSerials patches each learned serial into its device row, unless an
// installation write gave the device a serial in the meantime.
func (d *daemon) adoptSerials(learned map[string]string) {
	d.installMu.Lock()
	defer d.installMu.Unlock()
	for key, serial := range learned {
		in := d.inst.Load()
		if in == nil {
			return
		}
		dev, err := in.Resolve(key)
		if err != nil || dev.Serial != "" {
			continue
		}
		ref := dev.Ref()
		res, _, err := d.state.PatchDevice(context.Background(), key, devicePatch{Serial: &serial})
		if err != nil {
			log.Printf("device %s: serial %s from its requests not stored: %v", ref, serial, err)
			continue
		}
		if err := d.applyInstall(res.Install); err != nil {
			log.Printf("device %s: serial %s stored in revision %d, the running daemon did not take it: %v", ref, serial, res.Rev, err)
			continue
		}
		log.Printf("device %s: serial %s learned from its requests", ref, serial)
	}
}
