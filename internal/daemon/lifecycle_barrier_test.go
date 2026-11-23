package daemon

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/yiplee/go-bb-browser/internal/browser"
	"github.com/yiplee/go-bb-browser/internal/store"
	"github.com/yiplee/go-bb-browser/pkg/protocol"
)

// Barrier regressions for the lifecycle ordering of a complete tab id: each
// test pins one schedule at explicit barriers (a parked Target.createTarget /
// Target.getTargets / Target.closeTarget reply, a full lifecycle event queue)
// and checks that late evidence neither brings a dead id back nor costs a live
// tab its state. A and B share their id suffix on purpose; A is created once
// and its id is never reused.

const (
	barrierA target.ID = "AAAA0000000000000000000000AB1234"
	barrierB target.ID = "BBBB0000000000000000000000AB1234"
)

// barrierConn adds parked replies and a close log to enumConn.
type barrierConn struct {
	*enumConn

	createEntered, createRelease chan struct{} // park the create reply after the page exists
	closeEntered, closeRelease   chan struct{} // park Target.closeTarget before it acts

	bmu      sync.Mutex
	linger   bool                     // closeTarget succeeds, the page stays listed
	closeErr func(id target.ID) error // per-call close failure
	closes   []target.ID              // every Target.closeTarget sent
	once     struct{ create, close sync.Once }
}

func (c *barrierConn) CreatePageTarget(url string, silent bool) (target.ID, error) {
	id, err := c.enumConn.CreatePageTarget(url, silent)
	if c.createEntered != nil {
		first := false
		c.once.create.Do(func() { first = true })
		if first {
			close(c.createEntered)
			<-c.createRelease
		}
	}
	return id, err
}

func (c *barrierConn) CloseTarget(id target.ID) error {
	if c.closeEntered != nil {
		first := false
		c.once.close.Do(func() { first = true })
		if first {
			close(c.closeEntered)
			<-c.closeRelease
		}
	}
	c.bmu.Lock()
	c.closes = append(c.closes, id)
	linger, fail := c.linger, c.closeErr
	c.bmu.Unlock()
	if fail != nil {
		if err := fail(id); err != nil {
			return err
		}
	}
	if !linger {
		c.removeExternally(id)
	}
	return nil
}

func (c *barrierConn) setLinger(v bool) {
	c.bmu.Lock()
	c.linger = v
	c.bmu.Unlock()
}

func (c *barrierConn) closeLog() []target.ID {
	c.bmu.Lock()
	defer c.bmu.Unlock()
	return append([]target.ID(nil), c.closes...)
}

func barrierRequest(srv *Server, method string, params map[string]any) <-chan enumRPCReply {
	done := make(chan enumRPCReply, 1)
	go func() {
		r, err := enumRPC(srv, method, params)
		if err != nil {
			r.Error = map[string]any{"message": err.Error()}
		}
		done <- r
	}()
	return done
}

// barrierListed runs tab_list and returns the tab id it reports for id.
func barrierListed(t *testing.T, srv *Server, id target.ID) string {
	t.Helper()
	if !enumListedTabs(t, enumMustRPC(t, srv, protocol.MethodTabList, nil))[string(id)] {
		t.Fatalf("tab_list does not list %s", id)
	}
	return string(id)
}

// barrierRejects: the three clear methods need no CDP round trip, so they
// depend on the registry alone; all must refuse the stale id.
func barrierRejects(t *testing.T, srv *Server, tab string) {
	t.Helper()
	for _, method := range []string{protocol.MethodNetworkClear, protocol.MethodConsoleClear, protocol.MethodErrorsClear} {
		r, err := enumRPC(srv, method, map[string]any{"tab": tab})
		if err != nil {
			t.Fatal(err)
		}
		if r.Error == nil {
			t.Errorf("%s accepted the stale tab id %q", method, tab)
		} else if msg, _ := r.Error["message"].(string); msg != "unknown tab id" {
			t.Errorf("%s error = %v, want unknown tab id", method, r.Error)
		}
	}
	if _, ok := srv.tabs.Lookup(tab); ok {
		t.Errorf("stale tab id %q resolves", tab)
	}
}

// barrierNotManaged: no idle tracking, no persisted membership, and replaying
// rpc.jsonl as a restarted daemon does not restore it. Closes srv's audit log.
func barrierNotManaged(t *testing.T, srv *Server, dir string, id target.ID) {
	t.Helper()
	if _, ok := srv.tabIdle.Snapshot()[id]; ok {
		t.Errorf("%s is idle-tracked", id)
	}
	if managedTargetSet(srv.store.ManagedTabs())[id] {
		t.Errorf("%s is in the managed set", id)
	}
	drainRPCLog(t, srv)
	if err := srv.closeAudit(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(store.OpenConfig{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if managedTargetSet(st.ManagedTabs())[id] {
		t.Errorf("replaying rpc.jsonl restores %s as managed", id)
	}
}

// Review round 4, schedule 1. The create reply is parked; a list registers A;
// the user closes A (no destroyed event delivered); a newer list that only
// holds B is requested and parked; the create reply is committed; the list is
// applied. Committing the late reply must not renew A's registration: the list
// was requested after A was first registered, so its absence there is final.
func TestLateTabNewCommitDoesNotMaskAbsenceInNewerList(t *testing.T) {
	dir := t.TempDir()
	c := &barrierConn{enumConn: newEnumConn(barrierB), createEntered: make(chan struct{}), createRelease: make(chan struct{})}
	c.nextIDs = []target.ID{barrierA}
	srv := newReconcileRaceServer(t, c.enumConn, dir, 0)
	srv.tabHook = c
	enumPushAllObs(t, srv, barrierB)
	wantB := enumObsCounts(srv, barrierB)

	newTab := barrierRequest(srv, protocol.MethodTabNew, nil)
	enumAwait(t, c.createEntered, "A exists in the browser, create reply parked")
	a := barrierListed(t, srv, barrierA)
	c.removeExternally(barrierA) // closed in the browser; its destroyed event is not delivered
	entered, release := c.holdNextSnapshot()
	defer release()
	list := barrierRequest(srv, protocol.MethodTabList, nil)
	enumAwait(t, entered, "newer snapshot holding only B")
	close(c.createRelease)
	created := <-newTab
	release()
	listed := <-list

	if listed.Error != nil {
		t.Fatalf("tab_list failed: %v", listed.Error)
	}
	tabs := enumListedTabs(t, listed.Result)
	if tabs[a] || !tabs[string(barrierB)] || len(tabs) != 1 {
		t.Errorf("tab_list = %v, want only B", tabs)
	}
	if focus, _ := listed.Result["focus"].(string); focus == a {
		t.Errorf("tab_list reports the gone tab A as focus")
	}
	t.Logf("tab_new: result=%v error=%v", created.Result, created.Error)
	barrierRejects(t, srv, a)
	if !c.has(barrierB) || !srv.tabs.Has(barrierB) || enumObsCounts(srv, barrierB) != wantB {
		t.Error("B was closed, unregistered or lost its buffers")
	}
	if n := srv.tabs.OpenWindows(); n != 0 {
		t.Errorf("%d request windows left open", n)
	}
	barrierNotManaged(t, srv, dir, barrierA)
}

// Review round 4, schedule 2. The create reply is parked; tab_close(A)
// succeeds; a current list still shows the closing A and registers it again;
// the create reply is committed. The close happened after the creation was
// requested, so the creation is refused whether or not A is registered at that
// moment, and A gains no managed lifecycle.
func TestLateTabNewRefusedAfterConfirmedCloseDespiteLingeringList(t *testing.T) {
	dir := t.TempDir()
	c := &barrierConn{enumConn: newEnumConn(barrierB), createEntered: make(chan struct{}), createRelease: make(chan struct{})}
	c.setLinger(true)
	c.nextIDs = []target.ID{barrierA}
	srv := newReconcileRaceServer(t, c.enumConn, dir, 0)
	srv.tabHook = c

	newTab := barrierRequest(srv, protocol.MethodTabNew, nil)
	enumAwait(t, c.createEntered, "A exists in the browser, create reply parked")
	a := barrierListed(t, srv, barrierA)
	enumMustRPC(t, srv, protocol.MethodTabClose, map[string]any{"tab": a})
	if _, ok := srv.tabs.Lookup(a); ok {
		t.Fatal("tab_close did not unregister A")
	}
	_ = barrierListed(t, srv, barrierA) // current enumeration, target not destroyed yet
	c.removeExternally(barrierA)
	entered, release := c.holdNextSnapshot()
	defer release()
	absent := barrierRequest(srv, protocol.MethodTabList, nil)
	enumAwait(t, entered, "current snapshot holding only B")
	close(c.createRelease)
	created := <-newTab
	release()
	<-absent

	if created.Error == nil {
		t.Errorf("tab_new reported success (%v) for a tab closed after it was requested", created.Result)
	} else if msg, _ := created.Error["message"].(string); msg != "tab closed before tab_new completed" {
		t.Errorf("tab_new error = %v", created.Error)
	}
	barrierRejects(t, srv, a)
	if !c.has(barrierB) || !srv.tabs.Has(barrierB) {
		t.Error("B was closed or unregistered")
	}
	for _, id := range c.closeLog() {
		if id != barrierA {
			t.Errorf("Target.closeTarget sent for %s", id)
		}
	}
	barrierNotManaged(t, srv, dir, barrierA)
}

// The same refusal with nothing else in flight: the tab the creation could not
// commit is rolled back with one bounded close of exactly that id, and the
// error tells the caller which tab and what became of it.
func TestRefusedTabNewRollsBackCreatedTab(t *testing.T) {
	for _, tc := range []struct {
		name     string
		closeErr error
		want     string
		wantLive bool
	}{
		{name: "closed", want: "cleanup: closed"},
		{name: "already gone", want: "cleanup: already gone",
			closeErr: &browser.CDPError{Kind: browser.ErrorTargetGone, Op: "Target.closeTarget", Target: barrierA, Err: errors.New("No target with given id found")}},
		{name: "close failed", want: "cleanup: not closed (", wantLive: true,
			closeErr: errors.New("Target.closeTarget: browser busy")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			c := &barrierConn{enumConn: newEnumConn(barrierB), createEntered: make(chan struct{}), createRelease: make(chan struct{})}
			c.setLinger(true)
			c.nextIDs = []target.ID{barrierA}
			srv := newReconcileRaceServer(t, c.enumConn, dir, 0)
			srv.tabHook = c

			newTab := barrierRequest(srv, protocol.MethodTabNew, nil)
			enumAwait(t, c.createEntered, "create reply parked")
			a := barrierListed(t, srv, barrierA)
			enumMustRPC(t, srv, protocol.MethodTabClose, map[string]any{"tab": a}) // confirmed; the page lingers
			// From here on only the rollback closes anything.
			c.bmu.Lock()
			c.linger = false
			if tc.closeErr != nil {
				c.closeErr = func(target.ID) error { return tc.closeErr }
			}
			c.bmu.Unlock()
			if tc.name == "already gone" {
				c.removeExternally(barrierA) // the closing page has been destroyed meanwhile
			}
			close(c.createRelease)
			created := <-newTab

			if created.Error == nil {
				t.Fatalf("tab_new succeeded: %v", created.Result)
			}
			data, _ := created.Error["data"].(map[string]any)
			hint, _ := data["hint"].(string)
			if !strings.Contains(hint, string(barrierA)) || !strings.Contains(hint, tc.want) {
				t.Errorf("error hint = %q, want the tab id and %q", hint, tc.want)
			}
			if log := c.closeLog(); len(log) != 2 || log[0] != barrierA || log[1] != barrierA {
				t.Errorf("Target.closeTarget calls = %v, want tab_close(A) then one rollback of A", log)
			}
			if c.has(barrierA) != tc.wantLive {
				t.Errorf("A live in the browser = %v, want %v", c.has(barrierA), tc.wantLive)
			}
			if !c.has(barrierB) {
				t.Error("rollback closed B")
			}
			if srv.tabs.Selected() == barrierA {
				t.Error("refused tab was selected")
			}
			if tc.wantLive {
				// Not silently lost: it is an ordinary unmanaged tab in tab_list.
				if !enumListedTabs(t, enumMustRPC(t, srv, protocol.MethodTabList, nil))[a] {
					t.Error("tab left open by a failed rollback is not listed")
				}
			} else {
				barrierRejects(t, srv, a)
			}
			if n := srv.tabs.OpenWindows(); n != 0 {
				t.Errorf("%d request windows left open", n)
			}
			barrierNotManaged(t, srv, dir, barrierA)
		})
	}
}

// A failed or refused request must not leave its window (and the evidence it
// collects) behind.
func TestFailedRequestsReleaseTheirWindows(t *testing.T) {
	c := newEnumConn(barrierB)
	srv := newEnumServer(t, c)
	defer srv.closeAudit()

	c.setListErr(errors.New("Target.getTargets: timeout"))
	for _, m := range []string{protocol.MethodTabList, protocol.MethodTabFocus} {
		if r, err := enumRPC(srv, m, nil); err != nil || r.Error == nil {
			t.Fatalf("%s: %+v, %v; want an RPC error", m, r, err)
		}
	}
	for _, m := range []string{protocol.MethodTabSelect, protocol.MethodEval, protocol.MethodTabClose, protocol.MethodNetworkClear} {
		if r, err := enumRPC(srv, m, map[string]any{"tab": "nope", "script": "1"}); err != nil || r.Error == nil {
			t.Fatalf("%s: %+v, %v; want an RPC error", m, r, err)
		}
	}
	c.setListErr(nil)

	failing := &failingCreateConn{enumConn: c}
	srv.tabHook = failing
	if r, err := enumRPC(srv, protocol.MethodTabNew, nil); err != nil || r.Error == nil {
		t.Fatalf("tab_new: %+v, %v; want an RPC error", r, err)
	}
	if n := srv.tabs.OpenWindows(); n != 0 {
		t.Fatalf("%d request windows left open", n)
	}
	// With no request in flight the registry keeps no removal history at all:
	// churn does not accumulate.
	srv.tabHook = c
	for i := 0; i < 50; i++ {
		tab := enumTabNew(t, srv)
		enumMustRPC(t, srv, protocol.MethodTabClose, map[string]any{"tab": tab})
	}
	if n := srv.tabs.OpenWindows(); n != 0 {
		t.Fatalf("%d request windows left open after churn", n)
	}
}

type failingCreateConn struct{ *enumConn }

func (c *failingCreateConn) CreatePageTarget(string, bool) (target.ID, error) {
	return "", errors.New("Target.createTarget: timeout")
}

// The lifecycle queue is full, so the destruction of A is dropped and a resync
// is requested. targetInfoChanged(A) events read before A was closed are still
// queued behind it; handled after the close and the resync, they must not
// register A again. Real chromedp session over a loopback CDP stub.
func TestQueuedInfoEventDoesNotReviveTabAfterCloseAndResync(t *testing.T) {
	st := newCDPStub(t, barrierB, barrierA)
	srv := newStubSessionServer(t, st)
	defer srv.closeAudit()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sess, err := browser.Connect(ctx, st.url(), browser.WithTargetObserver(srv.tabs))
	if err != nil {
		t.Fatal(err)
	}
	srv.tabMu.Lock()
	srv.tabLive = sess
	srv.browserState = sessionReady
	srv.tabMu.Unlock()

	a := barrierListed(t, srv, barrierA)
	awaitGetTargets(t, st, 1)
	holdFirst := st.holdNextGetTargets()
	defer closeOnce(holdFirst)
	holdFresh := st.holdNextGetTargets()
	defer closeOnce(holdFresh)
	syncDone := make(chan struct{})
	go func() { defer close(syncDone); srv.runTargetSync(ctx, sess) }()
	defer func() { cancel(); <-syncDone }()
	sess.MarkTargetDirty()
	st.emitInfoChanged(barrierB)
	awaitGetTargets(t, st, 2) // the sync loop is parked in its resync

	n := cap(sess.TargetEvents())
	for i := 0; i < n; i++ {
		if i == n-1 {
			st.emitInfoChanged(barrierA)
		} else {
			st.emitInfoChanged(barrierB)
		}
	}
	if err := sess.PingBrowserContext(ctx); err != nil { // everything above has been read
		t.Fatal(err)
	}
	if len(sess.TargetEvents()) != n {
		t.Fatalf("lifecycle queue holds %d events, want it full (%d)", len(sess.TargetEvents()), n)
	}
	enumMustRPC(t, srv, protocol.MethodTabClose, map[string]any{"tab": a})
	st.emit("Target.targetDestroyed", map[string]any{"targetId": barrierA}) // dropped: queue full
	if err := sess.PingBrowserContext(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := srv.tabs.Lookup(a); ok || st.hasPage(barrierA) {
		t.Fatal("tab_close did not remove A")
	}
	closeOnce(holdFirst)
	awaitGetTargets(t, st, 3) // the resync for the dropped event: lists only B
	closeOnce(holdFresh)

	// A marker announced now is queued behind every earlier event; once it is
	// registered, all of them (including the old event about A) were handled.
	for len(sess.TargetEvents()) == n {
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		runtime.Gosched()
	}
	st.addPage(enumNewID, true)
	for !srv.tabs.Has(enumNewID) {
		if ctx.Err() != nil {
			t.Fatal("marker event was not applied: ", ctx.Err())
		}
		runtime.Gosched()
	}
	barrierRejects(t, srv, a)
	if !srv.tabs.Has(barrierB) || !st.hasPage(barrierB) {
		t.Error("B lost")
	}
}

// Idle cleanup holds A's operation lock inside Target.closeTarget. A tab_select
// for A must wait for that lock: it may not succeed, and may not renew the idle
// clock of a tab that is being closed.
func TestTabSelectWaitsForIdleCloseOfSameTab(t *testing.T) {
	c := &barrierConn{enumConn: newEnumConn(), closeEntered: make(chan struct{}), closeRelease: make(chan struct{})}
	c.nextIDs = []target.ID{barrierA}
	srv := newEnumServer(t, c.enumConn)
	srv.tabHook = c
	defer srv.closeAudit()
	a := enumTabNew(t, srv)
	expired := time.Now().Add(-24 * time.Hour)
	srv.tabIdle.MarkManagedAt(barrierA, expired)

	swept := make(chan struct{})
	go func() { srv.closeExpiredTabs(context.Background()); close(swept) }()
	enumAwait(t, c.closeEntered, "idle cleanup inside Target.closeTarget, holding A's lock")
	selected := barrierRequest(srv, protocol.MethodTabSelect, map[string]any{"tab": a})

	deadline := time.Now().Add(enumTestWait)
	for {
		select {
		case r := <-selected:
			renewed := srv.tabIdle.Snapshot()[barrierA].After(expired)
			close(c.closeRelease)
			enumAwait(t, swept, "idle close")
			t.Fatalf("tab_select returned during the locked idle close: result=%v error=%v renewed=%v", r.Result, r.Error, renewed)
		default:
		}
		srv.tabMuOps.Lock()
		ent := srv.tabCDPLocks[a]
		waiting := ent != nil && ent.refs >= 2
		srv.tabMuOps.Unlock()
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			close(c.closeRelease)
			t.Fatal("tab_select never queued on A's operation lock")
		}
		runtime.Gosched()
	}
	close(c.closeRelease)
	enumAwait(t, swept, "idle close")
	r := <-selected
	if r.Error == nil {
		t.Errorf("tab_select succeeded for a tab closed by idle cleanup: %v", r.Result)
	} else if msg, _ := r.Error["message"].(string); msg != "unknown tab id" {
		t.Errorf("tab_select error = %v, want unknown tab id", r.Error)
	}
	if srv.tabs.Selected() == barrierA || c.has(barrierA) {
		t.Error("closed tab A is selected or still open")
	}
	if _, ok := srv.tabIdle.Snapshot()[barrierA]; ok {
		t.Error("closed tab A is idle-tracked again")
	}
	srv.tabMuOps.Lock()
	left := len(srv.tabCDPLocks)
	srv.tabMuOps.Unlock()
	if left != 0 {
		t.Errorf("%d tab lock entries left", left)
	}
}

// Target.closeTarget answers an explicit tab_close with "no such target": the
// browser has said A is gone. The RPC still returns that error, and A stops
// resolving and loses its buffers and managed membership.
func TestExplicitCloseTargetGoneClearsIdentity(t *testing.T) {
	dir := t.TempDir()
	c := &barrierConn{enumConn: newEnumConn(barrierB)}
	c.nextIDs = []target.ID{barrierA}
	srv := newReconcileRaceServer(t, c.enumConn, dir, 0)
	srv.tabHook = c
	a := enumTabNew(t, srv)
	enumPushAllObs(t, srv, barrierA)
	enumPushAllObs(t, srv, barrierB)
	wantB := enumObsCounts(srv, barrierB)

	// A vanished, but an enumeration would still (briefly) list it: only the
	// close reply says it is gone.
	c.setLinger(true)
	c.bmu.Lock()
	c.closeErr = func(id target.ID) error {
		return &browser.CDPError{Kind: browser.ErrorTargetGone, Op: "Target.closeTarget", Target: id, Err: errors.New("No target with given id found")}
	}
	c.bmu.Unlock()
	r, err := enumRPC(srv, protocol.MethodTabClose, map[string]any{"tab": a})
	if err != nil {
		t.Fatal(err)
	}
	if r.Error == nil {
		t.Fatalf("tab_close succeeded: %v", r.Result)
	} else if msg, _ := r.Error["message"].(string); msg != "failed to close tab" {
		t.Errorf("tab_close error = %v, want the unchanged close failure", r.Error)
	}
	if got := enumObsCounts(srv, barrierA); got != [3]int{} {
		t.Errorf("A buffers = %v, want cleared", got)
	}
	c.removeExternally(barrierA)
	barrierRejects(t, srv, a)
	if !srv.tabs.Has(barrierB) || enumObsCounts(srv, barrierB) != wantB {
		t.Error("B was unregistered or lost its buffers")
	}

	// Any other close failure proves nothing and changes nothing.
	b := string(barrierB)
	c.bmu.Lock()
	c.closeErr = func(target.ID) error { return errors.New("Target.closeTarget: internal error") }
	c.bmu.Unlock()
	if r, _ := enumRPC(srv, protocol.MethodTabClose, map[string]any{"tab": b}); r.Error == nil {
		t.Fatal("failing close of B reported success")
	}
	if !srv.tabs.Has(barrierB) || enumObsCounts(srv, barrierB) != wantB {
		t.Error("a failed close of B removed its state")
	}
	barrierNotManaged(t, srv, dir, barrierA)
}

// Sixteen managed tabs are closed by idle cleanup one after another, then the
// log rotates: every retirement is durable, so the rotation snapshot and a
// replay hold none of them.
func TestIdleRetiredTabsAreNotReseededByRotation(t *testing.T) {
	c := newEnumConn()
	dir := t.TempDir()
	srv := newReconcileRaceServer(t, c, dir, 512)
	for i := 0; i < 16; i++ {
		id := target.ID(fmt.Sprintf("AAAA%024X1234", i))
		c.nextIDs = []target.ID{id}
		enumTabNew(t, srv)
		srv.tabIdle.MarkManagedAt(id, time.Now().Add(-time.Hour))
		srv.closeExpiredTabs(t.Context())
		if c.has(id) {
			t.Fatal("idle close left the target open")
		}
	}
	drainRPCLog(t, srv)
	for i := 0; i < 16; i++ { // passive queries push the log past its limit
		enumMustRPC(t, srv, protocol.MethodTabList, nil)
	}
	drainRPCLog(t, srv)
	if err := srv.closeAudit(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(store.OpenConfig{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if n := len(st.ManagedTabs()); n != 0 {
		t.Errorf("replay restored %d closed tabs", n)
	}
}
