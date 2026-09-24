package tado

// EnqueueExtuiPZH queues the bridge's external-UI binding write for heating.
func (e *EmuState) EnqueueExtuiPZH(target, stateEndpoint, measEndpoint string) error {
	wire, _, err := e.resolve(target)
	if err != nil {
		return err
	}
	mid, token, err := downlinkIDs()
	if err != nil {
		return err
	}
	pkt, err := BuildExtuiWriteCoAP(ExtuiWriteCoAP{
		StateEndpoint: stateEndpoint,
		MeasEndpoint:  measEndpoint,
		MID:           mid,
		Token:         token,
		SrcEUI:        e.selfEUI,
		DstEUI:        wire,
	})
	if err != nil {
		return err
	}
	if err := e.enqueue(target, "extui", "extui lid=pzh", pkt, e.key, mid); err != nil {
		return err
	}
	q := e.queue[hexEUI(wire)]
	q[len(q)-1].requireCoAP = true
	q[len(q)-1].responseCode = "2.01"
	q[len(q)-1].responseToken = append([]byte(nil), token...)
	return nil
}
