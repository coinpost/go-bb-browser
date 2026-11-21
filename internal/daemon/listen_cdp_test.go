package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/yiplee/go-bb-browser/internal/store"
	"github.com/yiplee/go-bb-browser/pkg/protocol"
)

// Target ids for the real-ListenAndServe tests. cdpManaged and cdpUser end in
// the same four hex digits, so whichever registers first gets the short id
// "1234"; cdpUser sorts first, which is the order an enumeration registers in.
const (
	cdpExisting = target.ID("CCCC0000AAAA")
	cdpManaged  = target.ID("BBBB99991234")
	cdpUser     = target.ID("AAAA00001234")
)

// liveDaemon is a daemon running the real ListenAndServe against a fakeCDP.
type liveDaemon struct {
	t      *testing.T
	srv    *Server
	url    string
	cancel context.CancelFunc
	done   chan error
}

func startLiveDaemon(t *testing.T, cdp *fakeCDP, stateDir string, mutate func(*Config)) *liveDaemon {
	t.Helper()
	return startLiveDaemonServer(t, cdp, stateDir, mutate, nil)
}

// startLiveDaemonServer also lets the test adjust the Server before it runs.
func startLiveDaemonServer(t *testing.T, cdp *fakeCDP, stateDir string, mutate func(*Config), prepare func(*Server)) *liveDaemon {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	cfg := Config{
		DebuggerURL:      cdp.addr(),
		ListenAddr:       addr,
		StateDir:         stateDir,
		TabIdleTimeout:   time.Minute,
		IdleStartupGrace: 30 * time.Millisecond,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.idleSweepInterval = 10 * time.Millisecond
	if prepare != nil {
		prepare(srv)
	}
	ctx, cancel := context.WithCancel(context.Background())
	d := &liveDaemon{t: t, srv: srv, url: "http://" + addr, cancel: cancel, done: make(chan error, 1)}
	go func() { d.done <- srv.ListenAndServe(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-d.done:
		case <-time.After(10 * time.Second):
		}
	})
	return d
}

// waitListening fails the test if the daemon does not serve /health in time:
// that is what a startup deadlock looks like from outside.
func (d *liveDaemon) waitListening() {
	d.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		res, err := http.Get(d.url + "/health")
		if err == nil {
			_ = res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case err := <-d.done:
			d.t.Fatalf("ListenAndServe returned during startup: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			d.t.Fatal("daemon did not start listening: startup is stuck before net.Listen")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (d *liveDaemon) rpc(method string, params any, out any) {
	d.t.Helper()
	result, rpcErr := d.call(method, params)
	if len(rpcErr) != 0 {
		d.t.Fatalf("%s: error %s", method, rpcErr)
	}
	if out != nil {
		if err := json.Unmarshal(result, out); err != nil {
			d.t.Fatal(err)
		}
	}
}

// call sends one RPC and returns its result or its JSON-RPC error object.
func (d *liveDaemon) call(method string, params any) (result, rpcErr json.RawMessage) {
	d.t.Helper()
	res, err := http.Post(d.url+"/v1", "application/json", bytes.NewBufferString(rpcReq(method, params, 1)))
	if err != nil {
		d.t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		d.t.Fatalf("%s: %s (%v)", method, body, err)
	}
	return env.Result, env.Error
}

// exited waits up to limit for ListenAndServe to return.
func (d *liveDaemon) exited(limit time.Duration) (err error, ok bool) {
	select {
	case err := <-d.done:
		d.done <- err
		return err, true
	case <-time.After(limit):
		return nil, false
	}
}

func (d *liveDaemon) tabList() map[string]bool {
	d.t.Helper()
	var res protocol.TabListResult
	d.rpc(protocol.MethodTabList, map[string]any{}, &res)
	out := make(map[string]bool, len(res.Tabs))
	for _, tab := range res.Tabs {
		out[tab.Tab] = true
	}
	return out
}

// stop cancels the daemon and waits for ListenAndServe to return.
func (d *liveDaemon) stop() {
	d.t.Helper()
	d.cancel()
	select {
	case err := <-d.done:
		d.done <- err
		if err != nil {
			d.t.Fatalf("ListenAndServe: %v", err)
		}
	case <-time.After(10 * time.Second):
		d.t.Fatal("shutdown timed out")
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The startup enumeration is answered with a list older than what the target
// sync loop has registered meanwhile. Startup used to hold the tabMu write lock
// across it, treat the newer target as removed, and then block on its own read
// lock while cleaning it up, before ever listening.
func TestStartupWithStaleInitialEnumerationListensAndKeepsNewTarget(t *testing.T) {
	cdp := newFakeCDP(t, cdpExisting)
	var d *liveDaemon
	var ready sync.WaitGroup
	ready.Add(1)
	var once sync.Once
	cdp.onGetTargets = func() {
		once.Do(func() {
			ready.Wait()
			cdp.openTarget(cdpUser) // not in the reply being built
			waitFor(t, "sync loop to register the new target", func() bool {
				_, ok := d.srv.tabs.ShortForTarget(cdpUser)
				return ok
			})
		})
	}
	d = startLiveDaemon(t, cdp, t.TempDir(), nil)
	ready.Done()

	d.waitListening()
	tabs := d.tabList()
	if !tabs["aaaa"] || !tabs["1234"] || len(tabs) != 2 {
		t.Fatalf("tab_list = %v, want aaaa and 1234", tabs)
	}
	d.stop()
}

// An empty browser reply and a failing first enumeration must both leave the
// daemon serving; the retry then performs the one-time recovery.
func TestStartupFirstEnumerationFailureStillListensAndRecoversOnRetry(t *testing.T) {
	stateDir := t.TempDir()
	st, err := store.Open(store.OpenConfig{StateDir: stateDir})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AppendRPC(store.ManagedTabRecord("1234", string(cdpManaged), time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	cdp := newFakeCDP(t, cdpExisting, cdpManaged)
	cdp.failGetTargets = 1
	d := startLiveDaemon(t, cdp, stateDir, nil)
	d.waitListening()
	waitFor(t, "retry to recover the managed tab", func() bool { return d.srv.tabIdle.IsManaged(cdpManaged) })
	if got := d.srv.tabIdle.Snapshot(); len(got) != 1 {
		t.Fatalf("managed = %v, want only %s", got, cdpManaged)
	}
	d.stop()
	if got := managedOnDisk(t, stateDir); !slices.Equal(got, []string{string(cdpManaged)}) {
		t.Fatalf("managed on disk = %v", got)
	}
}

// Same browser, same two pages, before and after a daemon restart; only the
// order in which they are registered differs, which moves the short id "1234"
// from the daemon's tab to the user's. Recovery by short id adopted the user's
// page and idle cleanup closed it.
func TestRestartWithReorderedShortIDsClosesOnlyOwnTab(t *testing.T) {
	stateDir := t.TempDir()

	cdp1 := newFakeCDP(t, cdpExisting)
	cdp1.nextCreate = []target.ID{cdpManaged}
	d1 := startLiveDaemon(t, cdp1, stateDir, nil)
	d1.waitListening()
	var created protocol.TabNewResult
	d1.rpc(protocol.MethodTabNew, map[string]any{"silent": true}, &created)
	if created.Tab != "1234" {
		t.Fatalf("daemon tab = %q, want 1234", created.Tab)
	}
	cdp1.openTarget(cdpUser)
	waitFor(t, "user tab to be listed", func() bool { return d1.tabList()["01234"] })
	d1.stop()

	// Restart against the unchanged browser.
	cdp2 := newFakeCDP(t, cdpExisting, cdpUser, cdpManaged)
	d2 := startLiveDaemon(t, cdp2, stateDir, func(cfg *Config) { cfg.TabIdleTimeout = 150 * time.Millisecond })
	d2.waitListening()
	if short, _ := d2.srv.tabs.ShortForTarget(cdpUser); short != "1234" {
		t.Fatalf("test setup: user tab short id = %q, want it to have taken over 1234", short)
	}
	if d2.srv.tabIdle.IsManaged(cdpUser) {
		t.Fatal("user page adopted as managed after restart")
	}

	waitFor(t, "idle cleanup to close the daemon's tab", func() bool { return len(cdp2.closedTargets()) > 0 })
	time.Sleep(300 * time.Millisecond) // several more sweeps
	if got := cdp2.closedTargets(); !slices.Equal(got, []target.ID{cdpManaged}) {
		t.Fatalf("closed targets = %v, want only %s", got, cdpManaged)
	}
	if !cdp2.hasTarget(cdpUser) {
		t.Fatal("user page was closed")
	}
	d2.stop()
	if got := managedOnDisk(t, stateDir); len(got) != 0 {
		t.Fatalf("managed on disk after idle close = %v", got)
	}
}

// Shutdown begins while the idle sweeper has closed a tab in Chrome and has not
// yet recorded it. ListenAndServe must wait for that record: returning first
// closed the store under the sweeper, the release was lost, and the next run
// found the tab still managed.
func TestShutdownWaitsForIdleCloseInFlight(t *testing.T) {
	stateDir := t.TempDir()
	cdp := newFakeCDP(t, cdpExisting)
	cdp.nextCreate = []target.ID{cdpManaged}
	entered := make(chan struct{})
	gate := make(chan struct{})
	var once sync.Once
	cdp.onCloseTarget = func(target.ID) {
		once.Do(func() { close(entered) })
		<-gate
	}
	d := startLiveDaemon(t, cdp, stateDir, func(cfg *Config) { cfg.TabIdleTimeout = 100 * time.Millisecond })
	d.waitListening()
	d.rpc(protocol.MethodTabNew, map[string]any{"silent": true}, nil)

	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("idle sweeper never closed the tab")
	}
	// Hold the lifecycle lock: the sweeper gets its closeTarget reply and then
	// stops right before recording the release.
	d.srv.lifeMu.Lock()
	close(gate)
	<-cdp.closeReplied
	time.Sleep(200 * time.Millisecond) // let the sweeper take the reply and reach the lock

	d.cancel()
	select {
	case err := <-d.done:
		d.srv.lifeMu.Unlock()
		t.Fatalf("ListenAndServe returned (%v) while the idle close was unrecorded", err)
	case <-time.After(300 * time.Millisecond):
	}
	d.srv.lifeMu.Unlock()
	d.stop()

	var releases int
	for _, rec := range readRPCLog(t, stateDir) {
		if rec.Action == protocol.MethodTabClose && rec.OK && rec.Target == string(cdpManaged) {
			releases++
		}
	}
	if releases != 1 {
		t.Fatalf("release records for the idle-closed tab = %d, want 1", releases)
	}
	if got := managedOnDisk(t, stateDir); len(got) != 0 {
		t.Fatalf("managed on disk after shutdown = %v", got)
	}
}

// Every way out of ListenAndServe joins the background goroutines, including a
// listen failure after the browser session is up.
func TestListenFailureStopsBackgroundGoroutines(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()

	cdp := newFakeCDP(t, cdpExisting)
	cfg := Config{DebuggerURL: cdp.addr(), ListenAddr: busy.Addr().String(), StateDir: t.TempDir(), TabIdleTimeout: time.Minute}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe(context.Background()) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a listen error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ListenAndServe did not return on listen failure")
	}
	joined := make(chan struct{})
	go func() { srv.bg.Wait(); close(joined) }()
	select {
	case <-joined:
	case <-time.After(time.Second):
		t.Fatal("background goroutines still running after ListenAndServe returned")
	}
}

// Two tab_close RPCs name the same daemon tab. While the first is in flight the
// second queues for the tab's operation lock, and the user opens a page whose
// target id gives it the same short id. The second close must report a stale id.
// When it resolved the short id only after getting the lock, it enumerated the
// browser, found the user's page under that id, and closed it.
func TestConcurrentTabCloseOverCDPLeavesShortIDSuccessorOpen(t *testing.T) {
	cdp := newFakeCDP(t, cdpExisting)
	cdp.nextCreate = []target.ID{cdpManaged}
	d := startLiveDaemon(t, cdp, t.TempDir(), nil)
	d.waitListening()
	var created protocol.TabNewResult
	d.rpc(protocol.MethodTabNew, map[string]any{"silent": true}, &created)
	if created.Tab != "1234" {
		t.Fatalf("tab_new = %+v, want tab 1234", created)
	}

	type reply struct{ result, rpcErr json.RawMessage }
	second := make(chan reply, 1)
	var once sync.Once
	cdp.mu.Lock()
	cdp.onCloseTarget = func(target.ID) {
		once.Do(func() {
			go func() {
				result, rpcErr := d.call(protocol.MethodTabClose, map[string]any{"tab": "1234"})
				second <- reply{result, rpcErr}
			}()
			waitFor(t, "second tab_close to queue for the tab lock", func() bool { return tabLockWaiters(d.srv, "1234") == 2 })
			// The user's page exists in Chrome before the first close is
			// answered; its targetCreated event has not been delivered.
			cdp.mu.Lock()
			cdp.targets = append(cdp.targets, cdpUser)
			cdp.mu.Unlock()
		})
	}
	cdp.mu.Unlock()

	if result, rpcErr := d.call(protocol.MethodTabClose, map[string]any{"tab": "1234"}); len(rpcErr) != 0 {
		t.Fatalf("first tab_close: %s", rpcErr)
	} else if len(result) == 0 {
		t.Fatal("first tab_close returned no result")
	}
	got := <-second
	if !bytes.Contains(got.rpcErr, []byte("unknown tab id")) {
		t.Fatalf("queued tab_close = result %s error %s, want a stale tab id error", got.result, got.rpcErr)
	}
	d.stop()
	if closed := cdp.closedTargets(); !slices.Equal(closed, []target.ID{cdpManaged}) {
		t.Fatalf("Target.closeTarget sent for %v, want only %s", closed, cdpManaged)
	}
	if !cdp.hasTarget(cdpUser) {
		t.Fatal("user page was closed")
	}
}
