package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/yiplee/go-bb-browser/internal/browser"
	"github.com/yiplee/go-bb-browser/internal/store"
	"github.com/yiplee/go-bb-browser/pkg/protocol"
)

// Regressions for the managed reconciliation race: a target enumeration may
// only retire persisted managed tabs that were already persisted when the
// enumeration was requested. A tab_new that overlaps the request, or completes
// after its reply was applied to the registry but before the managed set is
// reconciled, is older than nothing in that reply and must stay managed, in
// memory and on disk.

const (
	// Persisted by a previous daemon run and gone from the browser since.
	reconcileGoneID = target.ID("DEAD0000000000000000000000009F9F")
	// Persisted by a previous daemon run and still open.
	reconcileLiveID = enumOldID
	// Created by tab_new while an enumeration is being processed.
	reconcileNewID = enumNewID
)

// seedManagedState writes the lifecycle records a previous daemon run would
// have left behind for ids.
func seedManagedState(t *testing.T, dir string, ids ...target.ID) {
	t.Helper()
	st, err := store.Open(store.OpenConfig{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-time.Second)
	for _, id := range ids {
		if err := st.RegisterManagedTab(string(id), at); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

func managedTargetSet(tabs []store.ManagedTab) map[target.ID]bool {
	out := make(map[target.ID]bool, len(tabs))
	for _, mt := range tabs {
		out[target.ID(mt.TargetID)] = true
	}
	return out
}

// assertReconcileOutcome checks the three roles after the raced enumeration:
// the new tab stays managed, the still-open persisted tab is recovered, and the
// persisted tab that really vanished is retired.
func assertReconcileOutcome(t *testing.T, srv *Server) {
	t.Helper()
	persisted := managedTargetSet(srv.store.ManagedTabs())
	idle := srv.tabIdle.Snapshot()
	if !persisted[reconcileNewID] {
		t.Errorf("tab created by tab_new was retired from the persisted managed set: %v", persisted)
	}
	if _, ok := idle[reconcileNewID]; !ok {
		t.Errorf("tab created by tab_new lost idle tracking: %v", idle)
	}
	if !persisted[reconcileLiveID] {
		t.Errorf("still-open persisted tab was retired: %v", persisted)
	}
	if _, ok := idle[reconcileLiveID]; !ok {
		t.Errorf("still-open persisted tab was not recovered for idle tracking: %v", idle)
	}
	if persisted[reconcileGoneID] {
		t.Errorf("persisted tab absent from the enumeration was not retired: %v", persisted)
	}
	if _, ok := idle[reconcileGoneID]; ok {
		t.Errorf("vanished tab is idle-tracked: %v", idle)
	}
}

// assertPersistedAfterReopen closes srv and replays its rpc.jsonl: the outcome
// must be durable, not just in memory.
func assertPersistedAfterReopen(t *testing.T, srv *Server, dir string) {
	t.Helper()
	drainRPCLog(t, srv)
	if err := srv.closeAudit(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(store.OpenConfig{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	persisted := managedTargetSet(reopened.ManagedTabs())
	if !persisted[reconcileNewID] || !persisted[reconcileLiveID] || persisted[reconcileGoneID] {
		t.Errorf("replayed managed set = %v, want {%s, %s}", persisted, reconcileNewID, reconcileLiveID)
	}
}

func newReconcileRaceServer(t *testing.T, conn *enumConn, dir string, maxLogBytes int64) *Server {
	t.Helper()
	cfg := Config{
		DebuggerURL:      "127.0.0.1:9222",
		ListenAddr:       "127.0.0.1:0",
		TabIdleTimeout:   time.Minute,
		StateDir:         dir,
		IdleStartupGrace: 30 * time.Millisecond,
		MaxLogBytes:      maxLogBytes,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.tabHook = conn
	t.Cleanup(func() { _ = srv.closeAudit() })
	return srv
}

// The enumeration steps of connectBrowserLocked / runTargetSync, with tab_new
// completing at each point where it can interleave with them. The state dir
// starts as a restarted daemon finds it, so the startup reconciliation and the
// dirty retry (same steps) are both covered.
func TestReconcileDoesNotRetireTabCreatedAfterEnumerationWasRequested(t *testing.T) {
	const (
		whileInFlight = iota // request sent, reply not yet applied
		afterSync            // reply applied to the registry, managed set not yet reconciled
	)
	for _, tc := range []struct {
		name  string
		point int
	}{
		{"tab_new while enumeration in flight", whileInFlight},
		{"tab_new after registry sync before reconcile", afterSync},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			seedManagedState(t, dir, reconcileGoneID, reconcileLiveID)
			conn := newEnumConn(reconcileLiveID)
			conn.nextIDs = []target.ID{reconcileNewID}
			srv := newReconcileRaceServer(t, conn, dir, 0)

			candidates := srv.beginManagedReconcile()
			tok := srv.beginTargetEnumeration()
			infos, err := conn.PageTargets() // the browser's snapshot, without the new tab
			if err != nil {
				t.Fatal(err)
			}
			if tc.point == whileInFlight {
				enumTabNew(t, srv)
			}
			snaps := srv.syncTabsFromTargets(tok, infos)
			if tc.point == afterSync {
				enumTabNew(t, srv)
			}
			if !conn.has(reconcileNewID) {
				t.Fatal("precondition: tab_new did not create the tab in the browser")
			}
			srv.reconcileIdleFromLog(candidates, snaps)

			assertReconcileOutcome(t, srv)
			if !srv.tabs.Has(reconcileNewID) {
				t.Error("tab created by tab_new is no longer registered")
			}

			// A later enumeration that does list it changes nothing, and once the
			// tab is really gone the same path retires it.
			reconcileFreshConn(t, srv, conn)
			assertReconcileOutcome(t, srv)
			assertPersistedAfterReopen(t, srv, dir)

			// Restart on the same state dir: the tab is recovered by identity.
			srv2 := newReconcileRaceServer(t, conn, dir, 0)
			reconcileFreshConn(t, srv2, conn)
			if _, ok := srv2.tabIdle.Snapshot()[reconcileNewID]; !ok {
				t.Error("tab created during the raced enumeration not recovered after restart")
			}
			conn.removeExternally(reconcileNewID)
			reconcileFreshConn(t, srv2, conn)
			if managedTargetSet(srv2.store.ManagedTabs())[reconcileNewID] {
				t.Error("tab that vanished afterwards was not retired by a later enumeration")
			}
		})
	}
}

func reconcileFreshConn(t *testing.T, srv *Server, conn *enumConn) {
	t.Helper()
	candidates := srv.beginManagedReconcile()
	tok := srv.beginTargetEnumeration()
	infos, err := conn.PageTargets()
	if err != nil {
		t.Fatal(err)
	}
	srv.reconcileIdleFromLog(candidates, srv.syncTabsFromTargets(tok, infos))
}

// The surviving membership must also survive a log rotation (the snapshot is
// written from the in-memory set the raced reconciliation used to corrupt).
func TestReconcileRaceSurvivorKeptAcrossRotation(t *testing.T) {
	dir := t.TempDir()
	seedManagedState(t, dir, reconcileGoneID, reconcileLiveID)
	conn := newEnumConn(reconcileLiveID)
	conn.nextIDs = []target.ID{reconcileNewID}
	srv := newReconcileRaceServer(t, conn, dir, 2048)

	candidates := srv.beginManagedReconcile()
	tok := srv.beginTargetEnumeration()
	infos, err := conn.PageTargets()
	if err != nil {
		t.Fatal(err)
	}
	snaps := srv.syncTabsFromTargets(tok, infos)
	short := enumTabNew(t, srv)
	srv.reconcileIdleFromLog(candidates, snaps)

	logPath := filepath.Join(dir, store.RPCLogFile())
	for i := 0; i < 200; i++ {
		enumMustRPC(t, srv, protocol.MethodGoto, map[string]any{"tab": short, "url": "https://example.com/p"})
		drainRPCLog(t, srv)
		if _, err := os.Stat(logPath + ".1"); err == nil {
			break
		}
	}
	if _, err := os.Stat(logPath + ".1"); err != nil {
		t.Fatalf("expected a rotation: %v", err)
	}
	assertReconcileOutcome(t, srv)
	assertPersistedAfterReopen(t, srv, dir)
}

// A failed enumeration never reaches reconciliation, and candidates alone (no
// successful reply) retire nothing; without candidates nothing is retirable.
func TestReconcileRetiresOnlyCandidatesOfASuccessfulEnumeration(t *testing.T) {
	dir := t.TempDir()
	seedManagedState(t, dir, reconcileGoneID, reconcileLiveID)
	conn := newEnumConn(reconcileLiveID)
	srv := newReconcileRaceServer(t, conn, dir, 0)

	candidates := srv.beginManagedReconcile()
	if len(candidates) != 2 {
		t.Fatalf("candidates = %v, want the two persisted targets", candidates)
	}
	conn.setListErr(errors.New("Target.getTargets: timeout"))
	tok := srv.beginTargetEnumeration()
	infos, err := conn.PageTargets()
	if snaps := srv.syncInitialTargets(noopDirtyMarker{}, tok, infos, err); snaps != nil {
		t.Fatalf("failed enumeration produced snapshots: %v", snaps)
	}
	if got := managedTargetSet(srv.store.ManagedTabs()); !got[reconcileGoneID] || !got[reconcileLiveID] {
		t.Fatalf("failed enumeration retired persisted tabs: %v", got)
	}

	// No candidate set: an (empty) enumeration result proves nothing.
	srv.reconcileIdleFromLog(nil, nil)
	if got := managedTargetSet(srv.store.ManagedTabs()); !got[reconcileGoneID] || !got[reconcileLiveID] {
		t.Fatalf("reconcile without candidates retired persisted tabs: %v", got)
	}

	// The successful retry retires exactly the absent candidate.
	conn.setListErr(nil)
	reconcileFreshConn(t, srv, conn)
	got := managedTargetSet(srv.store.ManagedTabs())
	if got[reconcileGoneID] || !got[reconcileLiveID] {
		t.Fatalf("successful enumeration: managed = %v, want only %s", got, reconcileLiveID)
	}
	if _, ok := srv.tabIdle.Snapshot()[reconcileLiveID]; !ok {
		t.Fatal("still-open persisted tab not recovered")
	}
}

// First run: an empty state dir yields no candidates, and a tab created while
// that first enumeration is processed stays managed.
func TestReconcileFirstRunKeepsTabCreatedMeanwhile(t *testing.T) {
	dir := t.TempDir()
	conn := newEnumConn()
	conn.nextIDs = []target.ID{reconcileNewID}
	srv := newReconcileRaceServer(t, conn, dir, 0)

	candidates := srv.beginManagedReconcile()
	if len(candidates) != 0 {
		t.Fatalf("empty state produced candidates: %v", candidates)
	}
	tok := srv.beginTargetEnumeration()
	infos, err := conn.PageTargets()
	if err != nil {
		t.Fatal(err)
	}
	snaps := srv.syncTabsFromTargets(tok, infos)
	enumTabNew(t, srv)
	srv.reconcileIdleFromLog(candidates, snaps)

	if !managedTargetSet(srv.store.ManagedTabs())[reconcileNewID] {
		t.Error("first-run enumeration retired the tab created meanwhile")
	}
	if _, ok := srv.tabIdle.Snapshot()[reconcileNewID]; !ok {
		t.Error("first-run enumeration dropped idle tracking of the tab created meanwhile")
	}
}

// The production wiring: runTargetSync's dirty resync through a real
// browser.Session. Its Target.getTargets reply is held back, tab_new completes,
// and only then the (older) reply is applied and reconciled.
func TestDirtyResyncReplyOlderThanTabNewKeepsPersistedMembership(t *testing.T) {
	dir := t.TempDir()
	seedManagedState(t, dir, reconcileGoneID, reconcileLiveID)

	st := newCDPStub(t, reconcileLiveID)
	cfg := Config{
		DebuggerURL:         st.url(),
		ListenAddr:          "127.0.0.1:0",
		StateDir:            dir,
		TabIdleTimeout:      time.Minute,
		CDPWatchdogInterval: time.Minute,
		CDPWatchdogTimeout:  time.Minute, // bounds the held-back Target.getTargets
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.closeAudit() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sess, err := browser.Connect(ctx, st.url())
	if err != nil {
		t.Fatal(err)
	}
	srv.tabMu.Lock()
	srv.tabLive = sess
	srv.browserState = sessionReady
	srv.tabMu.Unlock()

	staleGate := st.holdNextGetTargets() // #1: the resync that predates tab_new
	nextGate := st.holdNextGetTargets()  // #2: its request proves #1 was reconciled
	defer closeOnce(nextGate)
	defer closeOnce(staleGate)
	syncDone := make(chan struct{})
	go func() {
		defer close(syncDone)
		srv.runTargetSync(ctx, sess)
	}()
	sess.MarkTargetDirty()
	st.emitInfoChanged(reconcileLiveID) // wake the loop without waiting for its ticker
	awaitGetTargets(t, st, 1)

	st.queueCreateID(reconcileNewID)
	short := enumTabNew(t, srv)
	if !managedTargetSet(srv.store.ManagedTabs())[reconcileNewID] {
		t.Fatal("precondition: tab_new did not persist the new tab as managed")
	}

	// runTargetSync reconciles #1 before it loops, so the arrival of the next
	// resync request means #1 has been fully applied.
	sess.MarkTargetDirty()
	st.emitInfoChanged(reconcileLiveID)
	close(staleGate)
	awaitGetTargets(t, st, 2)

	assertReconcileOutcome(t, srv)
	if got, ok := srv.tabs.Lookup(short); !ok || got != reconcileNewID {
		t.Errorf("tab %q is no longer registered (got %q, %v)", short, got, ok)
	}
	if !st.hasPage(reconcileNewID) {
		t.Error("new tab no longer exists in the browser")
	}

	close(nextGate)
	cancel()
	select {
	case <-syncDone:
	case <-time.After(enumTestWait):
		t.Fatal("runTargetSync did not stop")
	}
	// Whatever the second resync got to do, the durable outcome is the same.
	assertPersistedAfterReopen(t, srv, dir)
}
