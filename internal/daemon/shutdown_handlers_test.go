package daemon

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/yiplee/go-bb-browser/pkg/protocol"
)

// stubbornTargets ignores its context: PageTargetsContext returns only when
// released, like an injected CDP hook that does not honor cancellation.
type stubbornTargets struct {
	*fakeConn
	entered chan struct{}
	release chan struct{}
}

func (c *stubbornTargets) PageTargetsContext(context.Context) ([]*target.Info, error) {
	close(c.entered)
	<-c.release
	return c.PageTargets()
}

// forcedShutdownDaemon runs ListenAndServe with shutdown budgets of a few
// milliseconds, so a handler still in a CDP call forces the close path.
type forcedShutdownDaemon struct {
	srv           *Server
	stateDir      string
	cancel        context.CancelFunc
	done          chan error
	sessionClosed chan struct{}
	clientDone    chan struct{}
}

func startForcedShutdownDaemon(t *testing.T, conn tabConn, entered <-chan struct{}) *forcedShutdownDaemon {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	d := &forcedShutdownDaemon{stateDir: t.TempDir(), done: make(chan error, 1), sessionClosed: make(chan struct{}), clientDone: make(chan struct{})}
	cfg := Config{DebuggerURL: "127.0.0.1:9222", ListenAddr: addr, StateDir: d.stateDir}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.SkipBrowserAttach = true
	srv.shutdownTimeout = 20 * time.Millisecond
	srv.shutdownDrain = 20 * time.Millisecond
	srv.tabLive = conn
	srv.sessDone = func() { close(d.sessionClosed) }
	d.srv = srv

	ctx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel
	t.Cleanup(cancel)
	go func() { d.done <- srv.ListenAndServe(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		res, err := http.Get("http://" + addr + "/live")
		if err == nil {
			_ = res.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	go func() {
		defer close(d.clientDone)
		res, err := http.Post("http://"+addr+"/v1", "application/json", strings.NewReader(rpcReq(protocol.MethodTabList, map[string]any{}, 1)))
		if err == nil {
			_ = res.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not reach CDP")
	}
	return d
}

func (d *forcedShutdownDaemon) auditedTabList(t *testing.T) bool {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(d.stateDir, "rpc.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Contains(data, []byte(`"action":"tab_list"`))
}

// The HTTP shutdown budget expires while a handler is still in a CDP call that
// ignores cancellation. ListenAndServe used to return then, closing the session
// and the store under the handler, whose audit record was lost. It must keep
// everything open until the handler is done, and admit no new RPC meanwhile.
func TestForcedShutdownWaitsForHandlerStillInFlight(t *testing.T) {
	conn := &stubbornTargets{fakeConn: &fakeConn{}, entered: make(chan struct{}), release: make(chan struct{})}
	d := startForcedShutdownDaemon(t, conn, conn.entered)

	d.cancel()
	time.Sleep(300 * time.Millisecond) // far past both shutdown budgets
	select {
	case err := <-d.done:
		close(conn.release)
		t.Fatalf("ListenAndServe returned (%v) while a handler was still in flight", err)
	default:
	}
	select {
	case <-d.sessionClosed:
		close(conn.release)
		t.Fatal("session closed while a handler was still in flight")
	default:
	}
	if d.auditedTabList(t) {
		t.Fatal("test setup: handler already audited")
	}
	// Admission is closed: a request arriving now is refused and records nothing.
	rec := httptest.NewRecorder()
	d.srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1", strings.NewReader(rpcReq(protocol.MethodTabNew, map[string]any{}, 2))))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("RPC during shutdown: status %d, want 503", rec.Code)
	}

	close(conn.release)
	select {
	case err := <-d.done:
		if err == nil {
			t.Fatal("forced shutdown should report the expired budget")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ListenAndServe did not return after the handler finished")
	}
	select {
	case <-d.sessionClosed:
	default:
		t.Fatal("session not closed after shutdown")
	}
	if !d.auditedTabList(t) {
		t.Fatal("audit record of the in-flight handler was lost")
	}
	data, _ := os.ReadFile(filepath.Join(d.stateDir, "rpc.jsonl"))
	if bytes.Contains(data, []byte(`"action":"tab_new"`)) {
		t.Fatalf("refused request was audited:\n%s", data)
	}
	<-d.clientDone
}

// When the in-flight CDP call honors its context, a forced shutdown cancels it
// and stays bounded: the handler fails fast, its record is written, and only
// then is the store closed.
func TestForcedShutdownCancelsHandlerAndKeepsItsAudit(t *testing.T) {
	entered := make(chan struct{})
	conn := &blockingTargets{fakeConn: &fakeConn{}, entered: entered, release: make(chan struct{})} // never released
	d := startForcedShutdownDaemon(t, conn, entered)

	d.cancel()
	select {
	case err := <-d.done:
		if err == nil {
			t.Fatal("forced shutdown should report the expired budget")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("forced shutdown did not cancel the in-flight handler")
	}
	if !d.auditedTabList(t) {
		t.Fatal("audit record of the cancelled handler was lost")
	}
	<-d.clientDone
}

// The gate counts exactly the handlers it admitted and turns the rest away.
func TestHandlerGate(t *testing.T) {
	var g handlerGate
	if !g.enter() || !g.enter() {
		t.Fatal("open gate refused a handler")
	}
	idle := g.close()
	if g.enter() {
		t.Fatal("closed gate admitted a handler")
	}
	g.leave()
	select {
	case <-idle:
		t.Fatal("idle with a handler still active")
	default:
	}
	g.leave()
	select {
	case <-idle:
	case <-time.After(time.Second):
		t.Fatal("not idle after the last handler left")
	}
	select {
	case <-g.close(): // closing again is harmless
	default:
		t.Fatal("second close is not idle")
	}
}

// Stopping the handlers cancels their contexts itself; it does not rely on the
// HTTP server closing the connection. Here the request's own context never ends.
func TestStopRPCHandlersCancelsActiveRPCContext(t *testing.T) {
	stateDir := t.TempDir()
	entered := make(chan struct{})
	srv := newIdleTestServerWithState(&fakeConn{}, time.Minute, stateDir)
	srv.tabHook = &blockingTargets{fakeConn: &fakeConn{}, entered: entered, release: make(chan struct{})} // never released

	handled := make(chan struct{})
	go func() {
		defer close(handled)
		req := httptest.NewRequest(http.MethodPost, "/v1", strings.NewReader(rpcReq(protocol.MethodTabList, map[string]any{}, 1)))
		srv.Handler().ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-entered

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		srv.stopRPCHandlers()
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("stopRPCHandlers did not cancel the in-flight RPC")
	}
	select {
	case <-handled:
	default:
		t.Fatal("stopRPCHandlers returned before the handler did")
	}
	if err := srv.closeAudit(); err != nil {
		t.Fatal(err)
	}
	if got := logActions(readRPCLog(t, stateDir)); got != "tab_list" {
		t.Fatalf("rpc log = %q, want the cancelled tab_list", got)
	}
}
