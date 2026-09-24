package daemon

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"freedo/internal/cli"
)

const testAPIToken = "synthetic-token-0123456789"

// authGet sends GET path with the given Authorization header, or none when it
// is empty.
func authGet(t *testing.T, base, path, authorization string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
}

func TestRequireToken(t *testing.T) {
	srv := httptest.NewServer(requireToken(testAPIToken, false, okHandler()))
	t.Cleanup(srv.Close)
	for _, tc := range []struct {
		name, path, auth string
		want             int
	}{
		{"missing token", "/api/stats", "", http.StatusUnauthorized},
		{"wrong token", "/api/stats", "Bearer " + testAPIToken + "x", http.StatusUnauthorized},
		{"wrong scheme", "/api/stats", "Basic " + testAPIToken, http.StatusUnauthorized},
		{"empty bearer", "/api/stats", "Bearer ", http.StatusUnauthorized},
		{"right token", "/api/stats", "Bearer " + testAPIToken, http.StatusOK},
		{"scheme is case-insensitive", "/api/stats", "bearer " + testAPIToken, http.StatusOK},
		{"health is private by default", "/api/health", "", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := authGet(t, srv.URL, tc.path, tc.auth)
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
			if got := resp.Header.Get("WWW-Authenticate"); tc.want == http.StatusUnauthorized && got != "Bearer" {
				t.Errorf("WWW-Authenticate = %q, want Bearer", got)
			}
		})
	}
}

func TestRequireTokenHealthPublic(t *testing.T) {
	srv := httptest.NewServer(requireToken(testAPIToken, true, okHandler()))
	t.Cleanup(srv.Close)
	if resp := authGet(t, srv.URL, "/api/health", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("GET /api/health without a token = %d, want 200", resp.StatusCode)
	}
	if resp := authGet(t, srv.URL, "/api/stats", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /api/stats without a token = %d, want 401", resp.StatusCode)
	}
	resp, err := http.Post(srv.URL+"/api/health", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("POST /api/health without a token = %d, want 401", resp.StatusCode)
	}
}

func TestRequireTokenEmptyIsOpen(t *testing.T) {
	srv := httptest.NewServer(requireToken("", false, okHandler()))
	t.Cleanup(srv.Close)
	if resp := authGet(t, srv.URL, "/api/stats", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("no token configured: status = %d, want 200", resp.StatusCode)
	}
}

func TestAPITokenSources(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	flagFile := write("flag", "from-flag\n")
	bootFile := write("boot", "  from-bootstrap  \n")
	blank := write("blank", " \n")
	twoWords := write("two", "one two\n")
	env := func(v string) func(string) (string, bool) {
		return func(key string) (string, bool) {
			if key != apiTokenEnv {
				t.Errorf("lookup of %q", key)
			}
			return v, true
		}
	}
	unset := func(string) (string, bool) { return "", false }
	withFile := &cli.Bootstrap{APITokenFile: bootFile}

	for _, tc := range []struct {
		name    string
		flag    string
		b       *cli.Bootstrap
		env     func(string) (string, bool)
		want    string
		wantErr bool
	}{
		{"flag wins", flagFile, withFile, env("from-env"), "from-flag", false},
		{"then the environment", "", withFile, env(" from-env "), "from-env", false},
		{"then the bootstrap", "", withFile, unset, "from-bootstrap", false},
		{"none", "", &cli.Bootstrap{}, unset, "", false},
		{"blank environment", "", withFile, env(""), "", true},
		{"blank file", blank, withFile, unset, "", true},
		{"missing file", filepath.Join(dir, "absent"), withFile, unset, "", true},
		{"whitespace inside", twoWords, withFile, unset, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := apiToken(tc.flag, tc.b, tc.env)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("token = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCheckListen(t *testing.T) {
	for _, tc := range []struct {
		addr     string
		token    bool
		insecure bool
		wantErr  bool
	}{
		{defaultHTTPListen, false, false, false},
		{"127.0.0.1:0", false, false, false},
		{"127.1.2.3:8080", false, false, false},
		{"[::1]:8080", false, false, false},
		{"localhost:8080", false, false, false},
		{":8080", false, false, true},
		{"0.0.0.0:8080", false, false, true},
		{"[::]:8080", false, false, true},
		{"192.168.1.10:8080", false, false, true},
		{"freedo.lan:8080", false, false, true},
		{":8080", true, false, false},
		{":8080", false, true, false},
	} {
		err := checkListen(tc.addr, tc.token, tc.insecure)
		if (err != nil) != tc.wantErr {
			t.Errorf("checkListen(%q, token=%v, insecure=%v) = %v, wantErr %v",
				tc.addr, tc.token, tc.insecure, err, tc.wantErr)
		}
	}
	err := checkListen(":8080", false, false)
	for _, want := range []string{"--api-token-file", apiTokenEnv, "api_token_file", "--insecure-no-auth"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
}

// helperEnv is the environment of a daemon subprocess, without a token the
// developer may have exported.
func helperEnv(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, apiTokenEnv+"=") {
			env = append(env, kv)
		}
	}
	return append(env, extra...)
}

func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

const daemonPublicNoTokenHelper = "FREEDO_TEST_DAEMON_PUBLIC_NO_TOKEN_HELPER"

// A non-loopback address with no token is refused before the database is
// opened, so the refusal leaves nothing behind.
func TestDaemonRefusesPublicAddrWithoutToken(t *testing.T) {
	if os.Getenv(daemonPublicNoTokenHelper) == "1" {
		Main(daemonHelperArgs())
		return
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestDaemonRefusesPublicAddrWithoutToken$")
	cmd.Dir = dir
	cmd.Env = helperEnv(daemonPublicNoTokenHelper+"=1", "FREEDO_TEST_DAEMON_ARGS=--addr\n0.0.0.0:0")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the daemon started on 0.0.0.0 with no token\n%s", out)
	}
	if !strings.Contains(string(out), "--insecure-no-auth") {
		t.Errorf("the refusal does not name --insecure-no-auth:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "freedo-state.db")); err == nil {
		t.Error("the refused start created a database")
	}
}

const daemonTokenHelper = "FREEDO_TEST_DAEMON_TOKEN_HELPER"

// The daemon reads the token file and puts the check in front of the real API.
func TestDaemonServesTokenCLI(t *testing.T) {
	if os.Getenv(daemonTokenHelper) == "1" {
		Main(daemonHelperArgs())
		return
	}
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte(testAPIToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	addr := freeLoopbackAddr(t)
	args := []string{"--addr", addr, "--api-token-file", tokenFile, "--health-public"}
	cmd := exec.Command(os.Args[0], "-test.run=^TestDaemonServesTokenCLI$")
	cmd.Dir = dir
	cmd.Env = helperEnv(daemonTokenHelper+"=1", "FREEDO_TEST_DAEMON_ARGS="+strings.Join(args, "\n"))
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	base := "http://" + addr
	client := &http.Client{Timeout: 100 * time.Millisecond}
	booted := false
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if resp, err := client.Get(base + "/api/health"); err == nil {
			resp.Body.Close()
			booted = resp.StatusCode == http.StatusOK
			break
		}
	}
	if booted {
		if resp := authGet(t, base, "/api/stats", ""); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET /api/stats without a token = %d, want 401", resp.StatusCode)
		}
		if resp := authGet(t, base, "/api/stats", "Bearer "+testAPIToken); resp.StatusCode != http.StatusOK {
			t.Errorf("GET /api/stats with the token = %d, want 200", resp.StatusCode)
		}
	}
	// The signal handler is armed just after the banner.
	time.Sleep(250 * time.Millisecond)
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("the daemon exited with %v\n%s", err, output.String())
		}
	case <-time.After(20 * time.Second):
		cmd.Process.Kill()
		<-done
		t.Fatalf("the daemon did not stop on SIGTERM\n%s", output.String())
	}
	if !booted {
		t.Fatalf("GET /api/health did not answer 200 without a token\n%s", output.String())
	}
	if strings.Contains(output.String(), "no API token") {
		t.Errorf("the daemon warned about a missing token:\n%s", output.String())
	}
}
