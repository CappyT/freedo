package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"freedo/internal/install"
)

// waitAnswer is one answer of a daemon, with the body as JSON.
type waitAnswer struct {
	code       int
	retryAfter string
	body       map[string]any
}

func waitCall(t *testing.T, method, url, token string, body io.Reader) (waitAnswer, error) {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: time.Second}).Do(req)
	if err != nil {
		return waitAnswer{}, err
	}
	defer resp.Body.Close()
	out := waitAnswer{code: resp.StatusCode, retryAfter: resp.Header.Get("Retry-After")}
	_ = json.NewDecoder(resp.Body).Decode(&out.body)
	return out, nil
}

// leftSec is the importWait.leftSec of a health answer, or -1 without one.
func (a waitAnswer) leftSec() int {
	wait, ok := a.body["importWait"].(map[string]any)
	if !ok {
		return -1
	}
	left, _ := wait["leftSec"].(float64)
	return int(left)
}

// refused checks a 503 of the wait: the header and the reason.
func (a waitAnswer) refused(t *testing.T, what string, maxLeft int, words ...string) {
	t.Helper()
	retry, err := strconv.Atoi(a.retryAfter)
	if a.code != http.StatusServiceUnavailable || err != nil || retry < 1 || retry > maxLeft {
		t.Errorf("%s: status %d, Retry-After %q, want 503 and 1 to %d", what, a.code, a.retryAfter, maxLeft)
	}
	msg, _ := a.body["error"].(string)
	for _, word := range append(words, "waits after the import") {
		if !strings.Contains(msg, word) {
			t.Errorf("%s: the message %q does not carry %q", what, msg, word)
		}
	}
}

// The waiting API keeps the token rule, says how long the wait lasts, and then
// leaves the address to the API of the daemon.
func TestImportWaitHandsTheAddressOver(t *testing.T) {
	addr := freeLoopbackAddr(t)
	base := "http://" + addr
	until := time.Now().Add(1500 * time.Millisecond)
	served := make(chan error, 1)
	go func() {
		served <- serveImportWait(addr, serverHandler("secret", true, false, importWaitHandler(until)), until)
	}()
	var health waitAnswer
	for deadline := time.Now().Add(time.Second); ; time.Sleep(10 * time.Millisecond) {
		var err error
		if health, err = waitCall(t, http.MethodGet, base+"/api/health", "", nil); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the waiting API does not answer: %v", err)
		}
	}
	if health.code != http.StatusOK || health.body["ok"] != true || health.body["transmitAllowed"] != false || health.leftSec() < 1 || health.leftSec() > 2 {
		t.Errorf("health in the wait: %d %v", health.code, health.body)
	}
	if got, _ := waitCall(t, http.MethodGet, base+"/api/zones", "", nil); got.code != http.StatusUnauthorized {
		t.Errorf("a read without the token: %d, want 401", got.code)
	}
	if got, _ := waitCall(t, http.MethodPost, base+"/api/zones/10/setpoint", "wrong", strings.NewReader(`{"setpointC":21}`)); got.code != http.StatusUnauthorized {
		t.Errorf("a write with a wrong token: %d, want 401", got.code)
	}
	for _, call := range [][2]string{
		{http.MethodGet, "/api/zones"}, {http.MethodPost, "/api/zones/10/setpoint"}, {http.MethodPatch, "/api/home"},
		{http.MethodPut, "/api/install/timezone"}, {http.MethodPost, "/api/pair/window"}, {http.MethodGet, "/api/no-such-route"},
	} {
		got, _ := waitCall(t, call[0], base+call[1], "secret", strings.NewReader(`{}`))
		got.refused(t, call[0]+" "+call[1], 2)
	}
	for _, call := range [][2]string{{http.MethodGet, "/api/state/export"}, {http.MethodPost, "/api/state/import?confirm=true"}} {
		got, _ := waitCall(t, call[0], base+call[1], "secret", strings.NewReader("SQLite format 3\x00"))
		got.refused(t, call[0]+" "+call[1], 2, "The export and the import are refused")
	}
	// The files of the dashboard are outside the API: 404 is a build without them.
	if got, _ := waitCall(t, http.MethodGet, base+"/", "", nil); got.code != http.StatusOK && got.code != http.StatusNotFound {
		t.Errorf("the page in the wait: %d", got.code)
	}

	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("serveImportWait: %v", err)
		}
		if early := time.Until(until); early > 0 {
			t.Errorf("the wait ended %s early", early)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the wait did not end")
	}
	a := newInstallAPIOn(t, defaultInstall)
	srv := newAPIServer(addr, serverHandler("secret", true, false, a.d.mux()))
	go srv.ListenAndServe()
	defer srv.Close()
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		got, err := waitCall(t, http.MethodGet, base+"/api/health", "", nil)
		if err == nil {
			if got.code != http.StatusOK || got.leftSec() != -1 || got.body["frames"] == nil {
				t.Errorf("health after the wait: %d %v", got.code, got.body)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the API does not serve on %s after the wait: %v", addr, err)
		}
	}
	if got, _ := waitCall(t, http.MethodGet, base+"/api/zones", "secret", nil); got.code != http.StatusOK {
		t.Errorf("a read after the wait: %d %v", got.code, got.body)
	}
}

func TestServeImportWaitReportsABusyAddress(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := serveImportWait(ln.Addr().String(), http.NotFoundHandler(), time.Now().Add(time.Minute)); err == nil {
		t.Fatal("serveImportWait took an address that is in use")
	}
}

const daemonImportWaitHelper = "FREEDO_TEST_DAEMON_IMPORT_WAIT_HELPER"

// A start that transmits serves the wait before it opens the radio, changes
// nothing, and a restart in the middle goes on from the import time. A start
// that does not transmit has no wait. The serial port does not exist: the
// start fails when it opens the radio, which marks that moment.
func TestImportWaitInTheStart(t *testing.T) {
	if os.Getenv(daemonImportWaitHelper) == "1" {
		Main(daemonHelperArgs())
		return
	}
	dir := t.TempDir()
	state := filepath.Join(dir, "state.db")
	if err := install.WriteInstall(state, defaultInstall); err != nil {
		t.Fatal(err)
	}
	importedAt := func(at time.Time) {
		t.Helper()
		store, err := openStateStore(state)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		if err := store.SetCounterResume(context.Background(), map[string]uint64{metaImportedAt: uint64(at.Unix())}); err != nil {
			t.Fatal(err)
		}
	}
	addr := freeLoopbackAddr(t)
	base := "http://" + addr
	port := filepath.Join(dir, "no-such-port")
	var output bytes.Buffer
	start := func(args ...string) (*exec.Cmd, chan error) {
		t.Helper()
		args = append([]string{"--addr", addr, "--state", state}, args...)
		cmd := exec.Command(os.Args[0], "-test.run=^TestImportWaitInTheStart$")
		cmd.Dir = dir
		cmd.Env = helperEnv(daemonImportWaitHelper+"=1", "FREEDO_TEST_DAEMON_ARGS="+strings.Join(args, "\n"))
		cmd.Stdout, cmd.Stderr = &output, &output
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		t.Cleanup(func() { cmd.Process.Kill() })
		return cmd, done
	}
	health := func() waitAnswer {
		t.Helper()
		for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			got, err := waitCall(t, http.MethodGet, base+"/api/health", "", nil)
			if err == nil {
				return got
			}
			if time.Now().After(deadline) {
				t.Fatalf("the daemon does not answer: %v\n%s", err, output.String())
			}
		}
	}

	importedAt(time.Now())
	cmd, done := start()
	if got := health(); got.code != http.StatusOK || got.leftSec() != -1 || got.body["tx"] != false {
		t.Fatalf("health of a start that does not transmit: %d %v", got.code, got.body)
	}
	if got, _ := waitCall(t, http.MethodGet, base+"/api/zones", "", nil); got.code != http.StatusOK {
		t.Fatalf("a read of a start that does not transmit: %d %v", got.code, got.body)
	}
	// The signal handler is armed just after the banner.
	time.Sleep(250 * time.Millisecond)
	cmd.Process.Signal(syscall.SIGTERM)
	if err := <-done; err != nil {
		t.Fatalf("the daemon exited with %v\n%s", err, output.String())
	}

	const total = 6
	at := time.Now().Add(-midWait + total*time.Second)
	end := time.Unix(at.Unix(), 0).Add(midWait)
	importedAt(at)
	cmd, done = start("--tx", "--port", port)
	first := health()
	if first.code != http.StatusOK || first.leftSec() < 1 || first.leftSec() > total {
		t.Fatalf("health in the wait: %d %v\n%s", first.code, first.body, output.String())
	}
	before := dumpFile(t, state)
	got, _ := waitCall(t, http.MethodPost, base+"/api/zones/10/setpoint", "", strings.NewReader(`{"setpointC":21}`))
	got.refused(t, "setpoint", total)
	got, _ = waitCall(t, http.MethodGet, base+"/api/state/export", "", nil)
	got.refused(t, "export", total, "The export and the import are refused")
	export, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	got, _ = waitCall(t, http.MethodPost, base+"/api/state/import?confirm=true", "", bytes.NewReader(export))
	got.refused(t, "import", total, "The export and the import are refused")

	cmd.Process.Kill()
	<-done
	cmd, done = start("--tx", "--port", port)
	second := health()
	if want := time.Until(end).Seconds(); second.leftSec() < 1 || second.leftSec() > first.leftSec() ||
		float64(second.leftSec()) < want-1 || float64(second.leftSec()) > want+1 {
		t.Fatalf("health after a restart in the wait: %v, first %d s, %.1f s to the end\n%s", second.body, first.leftSec(), want, output.String())
	}
	select {
	case err := <-done:
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("the daemon exited with %v, want the failure of the serial port\n%s", err, output.String())
		}
		if early := time.Until(end); early > 0 {
			t.Errorf("the start went on %s before the end of the wait\n%s", early, output.String())
		}
	case <-time.After(total*time.Second + 15*time.Second):
		t.Fatalf("the wait did not end\n%s", output.String())
	}
	log := output.String()
	for _, want := range []string{"before the start, so that no device still holds a Message ID of the replaced state", "error: no such file or directory"} {
		if !strings.Contains(log, want) {
			t.Errorf("the log does not carry %q:\n%s", want, log)
		}
	}
	if after := dumpFile(t, state); !reflect.DeepEqual(after, before) {
		t.Errorf("the database changed in the wait\n got: %v\nwant: %v", after, before)
	}
	if _, err := os.Stat(state + stagedImportSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refused import left a staged file: %v", err)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("the address is not free after the daemon: %v", err)
	}
	ln.Close()
}
