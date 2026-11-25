package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/coinpost/go-bb-browser/internal/browser"
	"github.com/coinpost/go-bb-browser/internal/store"
)

// Graceful shutdown order (ListenAndServe): the HTTP service stops and its
// in-flight RPCs drain while the browser session, the target sync loop and the
// RPC log keep working; then the lifecycle events read so far are applied and
// the enumeration requests still open are answered (bounded); only then do the
// sync loop, the session and the log go.
//
// The background context used to be derived from the caller's: the stop signal
// ended the sync loop at once, and a tab destroyed while an RPC was still
// draining kept its persisted managed record (no retirement written), and an
// enumeration an RPC asked for was never sent.
//
// All of these run the production entry point against the controllable CDP
// stub (loopback only, no Chrome). The managed tab A is created through
// tab_new; B is the page the daemon attached to.

const shutdownTestWait = 10 * time.Second

type shutdownDaemon struct {
	t    *testing.T
	s    *Server
	st   *cdpStub
	dir  string
	addr string
	stop context.CancelFunc
	done chan error
}

// startShutdownDaemon runs NewServer + ListenAndServe against st and waits
// until the service answers. probe is the CDP watchdog timeout, which also
// bounds every automatic enumeration.
func startShutdownDaemon(t *testing.T, st *cdpStub, probe time.Duration, prepare func(*Server)) *shutdownDaemon {
	t.Helper()
	d := &shutdownDaemon{t: t, st: st, dir: t.TempDir(), addr: freeLoopbackAddr(t), done: make(chan error, 1)}
	cfg := Config{
		DebuggerURL:         st.url(),
		ListenAddr:          d.addr,
		StateDir:            d.dir,
		TabIdleTimeout:      time.Minute,
		CDPWatchdogInterval: time.Minute,
		CDPWatchdogTimeout:  probe,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if prepare != nil {
		prepare(s)
	}
	d.s = s
	ctx, cancel := context.WithCancel(context.Background())
	d.stop = cancel
	go func() { d.done <- s.ListenAndServe(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-d.done:
		case <-time.After(shutdownTestWait):
		}
		_ = s.closeAudit()
	})
	deadline := time.Now().Add(shutdownTestWait)
	for !d.accepting() {
		if time.Now().After(deadline) {
			t.Fatal("daemon did not come up")
		}
		select {
		case err := <-d.done:
			t.Fatalf("daemon exited during startup: %v", err)
		case <-time.After(5 * time.Millisecond):
		}
	}
	return d
}

// accepting reports whether the listener takes a new connection.
func (d *shutdownDaemon) accepting() bool {
	c, err := net.DialTimeout("tcp", d.addr, time.Second)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// rpc posts one JSON-RPC call over a connection of its own. Safe from helper
// goroutines.
func (d *shutdownDaemon) rpc(method string, params map[string]any) (enumRPCReply, error) {
	var out enumRPCReply
	if params == nil {
		params = map[string]any{}
	}
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 2 * shutdownTestWait}
	res, err := client.Post("http://"+d.addr+"/v1", "application/json", bytes.NewBufferString(rpcReq(method, params, 1)))
	if err != nil {
		return out, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return out, errors.New(method + ": HTTP " + res.Status)
	}
	return out, json.NewDecoder(res.Body).Decode(&out)
}

// managedTab creates the managed tab A and checks it is registered and
// persisted.
func (d *shutdownDaemon) managedTab() {
	d.t.Helper()
	d.st.queueCreateID(ovfA)
	r, err := d.rpc("tab_new", nil)
	if err != nil || r.Error != nil {
		d.t.Fatalf("tab_new: %v %v", err, r.Error)
	}
	if tab, _ := r.Result["tab"].(string); tab != string(ovfA) {
		d.t.Fatalf("tab_new returned %q", tab)
	}
	if !d.s.tabs.Has(ovfA) || !managedTargetSet(d.s.store.ManagedTabs())[ovfA] {
		d.t.Fatal("precondition: A is a registered, persisted managed tab")
	}
}

// inFlightEval starts an eval on B and returns once its Runtime.evaluate is
// parked in the stub: the RPC stays in flight until release is called.
func (d *shutdownDaemon) inFlightEval() (reply <-chan enumRPCReply, release func()) {
	d.t.Helper()
	arrived, release := d.st.holdNext("Runtime.evaluate")
	ch := make(chan enumRPCReply, 1)
	go func() {
		r, err := d.rpc("eval", map[string]any{"tab": string(ovfB), "script": "1"})
		if err != nil {
			r.Error = map[string]any{"message": err.Error()}
		}
		ch <- r
	}()
	select {
	case <-arrived:
	case <-time.After(shutdownTestWait):
		d.t.Fatal("the eval never reached the browser")
	}
	return ch, release
}

// beginShutdown sends the stop signal and waits until the HTTP service has
// stopped accepting: the drain of the in-flight RPC has begun.
func (d *shutdownDaemon) beginShutdown() {
	d.t.Helper()
	d.stop()
	deadline := time.Now().Add(shutdownTestWait)
	for d.accepting() {
		if time.Now().After(deadline) {
			d.t.Fatal("the HTTP service kept accepting after the stop signal")
		}
		time.Sleep(2 * time.Millisecond)
	}
	select {
	case err := <-d.done:
		d.t.Fatalf("ListenAndServe returned while an RPC was in flight: %v", err)
	default:
	}
}

func (d *shutdownDaemon) session() *browser.Session {
	d.t.Helper()
	sess, _ := d.s.tabConn().(*browser.Session)
	if sess == nil {
		d.t.Fatal("the browser session was released while an RPC was in flight")
	}
	return sess
}

// finish waits for ListenAndServe to return within limit.
func (d *shutdownDaemon) finish(limit time.Duration) error {
	d.t.Helper()
	select {
	case err := <-d.done:
		d.done <- err // for the cleanup
		return err
	case <-time.After(limit):
		d.t.Fatalf("ListenAndServe did not return within %v", limit)
		return nil
	}
}

// replayedManaged replays the rpc.jsonl the stopped daemon left behind, as the
// next daemon process would.
func (d *shutdownDaemon) replayedManaged() map[target.ID]bool {
	d.t.Helper()
	replay, err := store.Open(store.OpenConfig{StateDir: d.dir})
	if err != nil {
		d.t.Fatal(err)
	}
	defer replay.Close()
	return managedTargetSet(replay.ManagedTabs())
}

func shutdownAssertEvalAnswered(t *testing.T, reply <-chan enumRPCReply) {
	t.Helper()
	select {
	case r := <-reply:
		if r.Error != nil {
			t.Errorf("the RPC in flight during shutdown failed: %v", r.Error)
		}
	case <-time.After(shutdownTestWait):
		t.Fatal("the RPC in flight during shutdown was never answered")
	}
}

// The schedule of the finding: shutdown starts while an RPC is in flight, then
// the managed tab is destroyed in the browser. The destroyed notification must
// be applied — registry, idle tracking and the persisted retirement — while the
// RPC is still draining, the RPC must still be served by the session, and the
// retirement must be on disk for the next process.
func TestShutdownAppliesDestroyedTabWhileRPCDrains(t *testing.T) {
	st := newCDPStub(t, ovfB)
	d := startShutdownDaemon(t, st, time.Minute, nil)
	d.managedTab()
	reply, release := d.inFlightEval()
	defer release()

	d.beginShutdown()
	enumerations := st.getTargetsCalls()
	st.removePage(ovfA, true) // the user closes A; the notification arrives during the drain

	deadline := time.Now().Add(shutdownTestWait)
	for causalKnown(d.s, ovfA) || managedTargetSet(d.s.store.ManagedTabs())[ovfA] {
		if time.Now().After(deadline) {
			t.Fatalf("destroyed notification not applied while the RPC drains: known=%v managed=%v",
				causalKnown(d.s, ovfA), managedTargetSet(d.s.store.ManagedTabs())[ovfA])
		}
		time.Sleep(2 * time.Millisecond)
	}
	if _, idle := d.s.tabIdle.Snapshot()[ovfA]; idle {
		t.Error("destroyed tab is still idle-tracked")
	}
	if got := st.getTargetsCalls(); got != enumerations {
		t.Errorf("%d enumerations sent: the cleanup must come from the notification", got-enumerations)
	}
	d.session() // still attached for the RPC in flight
	select {
	case err := <-d.done:
		t.Fatalf("ListenAndServe returned before the RPC in flight completed: %v", err)
	case r := <-reply:
		t.Fatalf("the held RPC was answered early: %+v", r)
	default:
	}

	release()
	shutdownAssertEvalAnswered(t, reply)
	if err := d.finish(shutdownTestWait); err != nil {
		t.Fatalf("clean shutdown returned %v", err)
	}
	if d.s.tabConn() != nil {
		t.Error("session not released after shutdown")
	}
	if d.replayedManaged()[ovfA] {
		t.Error("the retirement of the destroyed tab is not in rpc.jsonl: a restart would see it as managed")
	}
	causalNoClose(t, st) // shutdown closes no page
}

// An enumeration requested during the drain (here: the notification of A's
// destruction was lost and something asked for a resync, as an RPC's
// unordered evidence or a queue overflow does) is answered before the sync
// loop stops, and its result is persisted.
func TestShutdownAnswersOpenEnumerationRequest(t *testing.T) {
	st := newCDPStub(t, ovfB)
	d := startShutdownDaemon(t, st, time.Minute, nil)
	d.managedTab()
	reply, release := d.inFlightEval()
	defer release()

	d.beginShutdown()
	st.removePage(ovfA, false) // gone; the notification is lost
	d.session().MarkTargetDirty()
	release()
	shutdownAssertEvalAnswered(t, reply)
	if err := d.finish(shutdownTestWait); err != nil {
		t.Fatalf("clean shutdown returned %v", err)
	}
	if d.replayedManaged()[ovfA] {
		t.Error("the enumeration requested before shutdown was not settled: A is still persisted as managed")
	}
	causalNoClose(t, st)
}

// Nothing to settle: no event queued, no enumeration requested. Shutdown sends
// the browser nothing and returns promptly.
func TestShutdownWithNothingToSettleAsksTheBrowserNothing(t *testing.T) {
	st := newCDPStub(t, ovfB)
	d := startShutdownDaemon(t, st, time.Minute, nil)
	d.managedTab()
	// Let the events of the creation be consumed, and any enumeration they
	// asked for be answered.
	deadline := time.Now().Add(shutdownTestWait)
	sess := d.session()
	for len(sess.TargetEvents()) != 0 || d.s.tabs.OpenWindows() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("lifecycle queue did not go idle")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if sess.ConsumeTargetDirty() {
		sess.MarkTargetDirty()
		t.Skip("an enumeration is pending for an unrelated reason")
	}
	enumerations := st.getTargetsCalls()
	d.stop()
	if err := d.finish(2 * time.Second); err != nil {
		t.Fatalf("clean shutdown returned %v", err)
	}
	if got := st.getTargetsCalls(); got != enumerations {
		t.Errorf("shutdown sent %d enumerations with nothing to settle", got-enumerations)
	}
	if !d.replayedManaged()[ovfA] {
		t.Error("a live managed tab was retired by shutdown")
	}
	causalNoClose(t, st)
}

// The final enumeration fails or is never answered: shutdown is bounded, ends
// cleanly, and concludes nothing — A (really gone, but nobody could tell) stays
// persisted for the next process to settle. It must not be retired on a failed
// enumeration.
func TestShutdownFinalEnumerationFailureConcludesNothing(t *testing.T) {
	cases := []struct {
		name    string
		probe   time.Duration // bounds one enumeration
		settle  time.Duration // Server.settleTimeout; 0 = default
		prepare func(st *cdpStub)
	}{
		{name: "enumeration refused", probe: time.Minute, prepare: func(st *cdpStub) {
			for range 32 {
				st.failNext("Target.getTargets")
			}
		}},
		// The browser does not answer; the enumeration's own budget ends it.
		{name: "enumeration times out", probe: 150 * time.Millisecond, prepare: func(st *cdpStub) {
			for range 32 {
				st.holdNextGetTargets()
			}
		}},
		// The enumeration would wait a minute; the settlement budget ends it.
		{name: "settlement budget exceeded", probe: time.Minute, settle: 150 * time.Millisecond, prepare: func(st *cdpStub) {
			for range 32 {
				st.holdNextGetTargets()
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newCDPStub(t, ovfB)
			d := startShutdownDaemon(t, st, tc.probe, func(s *Server) { s.settleTimeout = tc.settle })
			d.managedTab()
			reply, release := d.inFlightEval()
			defer release()

			d.beginShutdown()
			tc.prepare(st)
			st.removePage(ovfA, false)
			d.session().MarkTargetDirty()
			release()
			shutdownAssertEvalAnswered(t, reply)
			if err := d.finish(5 * time.Second); err != nil {
				t.Fatalf("shutdown with a failing final enumeration returned %v", err)
			}
			if !d.replayedManaged()[ovfA] {
				t.Error("a tab was retired although no enumeration succeeded")
			}
			causalNoClose(t, st)
		})
	}
}

// Fatal path: the CDP connection is lost. ListenAndServe returns that error
// (bounded, nothing is asked of the dead connection) and everything it owns
// has stopped.
func TestShutdownOnCDPLossIsBoundedAndKeepsTheError(t *testing.T) {
	st := newCDPStub(t, ovfB)
	d := startShutdownDaemon(t, st, time.Minute, nil)
	d.managedTab()
	enumerations := st.getTargetsCalls()

	st.writeMu.Lock()
	_ = st.conn.Close()
	st.writeMu.Unlock()

	err := d.finish(shutdownTestWait)
	if err == nil || !strings.Contains(err.Error(), "cdp session lost") {
		t.Fatalf("CDP loss returned %v", err)
	}
	select {
	case <-d.s.bg.syncDone:
	default:
		t.Error("the target sync loop outlived ListenAndServe")
	}
	if got := st.getTargetsCalls(); got != enumerations {
		t.Errorf("%d enumerations sent after the connection was lost", got-enumerations)
	}
	// Nothing said A is gone: it stays for the next process.
	if !d.replayedManaged()[ovfA] {
		t.Error("a managed tab was retired by a lost connection")
	}
}

// A destroyed notification read from the connection after the sync loop
// stopped (fatal path, or a settlement that ran out of time) is still applied
// before the session and the log go: stopBackground drains the queue once the
// loop has returned, without a browser round trip.
func TestStopBackgroundAppliesEventsReadAfterTheSyncLoopStopped(t *testing.T) {
	dir := t.TempDir()
	st := newCDPStub(t, ovfB)
	s := newReconcileRaceServer(t, newEnumConn(), dir, 0)
	s.tabHook = nil
	s.cfg.DebuggerURL = st.url()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s.stopRun = cancel
	if err := s.connectBrowser(ctx); err != nil {
		t.Fatal(err)
	}
	st.queueCreateID(ovfA)
	if tab := enumTabNew(t, s); tab != string(ovfA) {
		t.Fatalf("tab_new returned %q", tab)
	}
	sess := s.tabConn().(*browser.Session)

	cancel() // as fatalCDPLost does
	select {
	case <-s.bg.syncDone:
	case <-time.After(shutdownTestWait):
		t.Fatal("the sync loop did not stop with its context")
	}
	enumerations := st.getTargetsCalls()
	st.removePage(ovfA, true)
	// A round trip on the same connection: the notification sent before its
	// reply has been read and queued.
	if err := sess.PingBrowserContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(sess.TargetEvents()) == 0 || !managedTargetSet(s.store.ManagedTabs())[ovfA] {
		t.Fatal("precondition: the destroyed notification is queued and not applied")
	}

	s.stopBackground(false)
	if causalKnown(s, ovfA) || managedTargetSet(s.store.ManagedTabs())[ovfA] {
		t.Error("a destroyed notification read before the daemon stopped was not applied")
	}
	if got := st.getTargetsCalls(); got != enumerations {
		t.Errorf("stopping without settlement sent %d enumerations", got-enumerations)
	}
	causalAssertGone(t, s, dir)
}

// Startup failure after the browser was attached (the listen address is
// taken): the service error is returned and the background work this call
// started has stopped.
func TestListenFailureAfterAttachStopsBackgroundWork(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	st := newCDPStub(t, ovfB)
	cfg := Config{DebuggerURL: st.url(), ListenAddr: busy.Addr().String(), StateDir: t.TempDir(), TabIdleTimeout: time.Minute}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.ListenAndServe(context.Background()) }()
	select {
	case err := <-done:
		var opErr *net.OpError
		if !errors.As(err, &opErr) {
			t.Fatalf("listen failure returned %v", err)
		}
	case <-time.After(shutdownTestWait):
		t.Fatal("ListenAndServe did not return")
	}
	select {
	case <-s.bg.syncDone:
	default:
		t.Error("the target sync loop outlived a failed startup")
	}
	if s.tabConn() != nil {
		t.Error("session not released after a failed startup")
	}
	causalNoClose(t, st)
}
