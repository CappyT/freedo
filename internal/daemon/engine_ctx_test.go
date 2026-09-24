package daemon

// engine_ctx_test: an API request gives up on the engine when its context
// ends, so a wedged engine does not hold one goroutine per request.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// serveUntil runs h on req in its own goroutine and returns the recorder once
// h returns, or fails the test when h is still running after limit.
func serveUntil(t *testing.T, h http.HandlerFunc, req *http.Request, limit time.Duration) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h(rec, req)
	}()
	select {
	case <-done:
		return rec
	case <-time.After(limit):
		t.Fatalf("%s %s still waits on the engine %s after its context ended", req.Method, req.URL.Path, limit)
		return nil
	}
}

// TestRequestGivesUpOnAWedgedEngine: no engine goroutine takes the command, so
// only the request context can end the wait. The handler answers 503 and the
// write it carried never reaches the daemon.
func TestRequestGivesUpOnAWedgedEngine(t *testing.T) {
	d, _ := reloadDaemon(t, reloadInstall(t))
	for _, tc := range []struct {
		name string
		h    http.HandlerFunc
		req  *http.Request
	}{
		{"read", d.getNodes, httptest.NewRequest(http.MethodGet, "/api/nodes", nil)},
		{"write", d.patchHome, httptest.NewRequest(http.MethodPatch, "/api/home", strings.NewReader(`{"away":true}`))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			rec := serveUntil(t, tc.h, tc.req.WithContext(ctx), 2*time.Second)
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("code = %d %s, want 503", rec.Code, rec.Body)
			}
		})
	}
	if d.away {
		t.Error("the refused PATCH /api/home switched the home to away")
	}
}

// TestRequestGivesUpOnATakenCommand: the engine took the command but has not
// run it yet. The request still ends on its context, and the command then runs
// to completion without blocking the engine on the abandoned reply.
func TestRequestGivesUpOnATakenCommand(t *testing.T) {
	d, _ := reloadDaemon(t, reloadInstall(t))
	release, ran := make(chan struct{}), make(chan struct{})
	go func() {
		cmd := <-d.cmds
		<-release
		cmd()
		close(ran)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodPatch, "/api/home", strings.NewReader(`{"away":true}`)).WithContext(ctx)
	rec := serveUntil(t, d.patchHome, req, 2*time.Second)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d %s, want 503", rec.Code, rec.Body)
	}
	close(release)
	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("the taken command blocked on its abandoned reply")
	}
	if !d.away {
		t.Error("the taken command did not run to completion")
	}
}
