package radio

import "testing"

// jobConfigBridge counts which register check the loop picks.
type jobConfigBridge struct {
	*queuedLiveBridge
	idle, behindJob int
}

func (b *jobConfigBridge) Configured() (bool, error) {
	b.idle++
	return true, nil
}

func (b *jobConfigBridge) ConfiguredBehindJob() (bool, error) {
	b.behindJob++
	return true, nil
}

// The register check after an expired job can queue behind a transaction and
// waits long. The idle check has no job out and keeps the short wait.
func TestRegisterCheckWaitsLongOnlyAfterAnExpiredJob(t *testing.T) {
	br := &jobConfigBridge{queuedLiveBridge: &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}}}
	r := &Live{br: br}
	var errs int
	if err := r.checkConfigured(&errs, false); err != nil {
		t.Fatal(err)
	}
	if br.idle != 1 || br.behindJob != 0 {
		t.Fatalf("idle check: Configured %d times, ConfiguredBehindJob %d, want 1 and 0", br.idle, br.behindJob)
	}
	if err := r.checkConfigured(&errs, true); err != nil {
		t.Fatal(err)
	}
	if br.idle != 1 || br.behindJob != 1 {
		t.Fatalf("check after a job: Configured %d times, ConfiguredBehindJob %d, want 1 and 1", br.idle, br.behindJob)
	}
}
