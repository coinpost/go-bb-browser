package daemon

import (
	"fmt"
	"testing"

	"github.com/chromedp/cdproto/target"
	"github.com/yiplee/go-bb-browser/internal/browser"
	"github.com/yiplee/go-bb-browser/internal/store"
)

// Regressions for the managed recovery of an enumeration reply the registry
// did not (fully) accept: "not accepted" is not "gone".

// A managed tab that the browser still lists must not be retired however many
// removals overtook the enumeration. (The registry used to forget removals
// past a bound and then refuse the whole reply; it now keeps the evidence with
// the request in flight, so the reply stays exact: A is recovered at once.)
func TestOvertakenEnumerationDoesNotRetireListedManagedTab(t *testing.T) {
	const idA = fullIDA
	gone := reconcileGoneID
	dir := t.TempDir()
	seedManagedState(t, dir, idA, gone)
	conn := newEnumConn(idA)
	srv := newReconcileRaceServer(t, conn, dir, 0)

	win := srv.tabs.OpenWindow()
	infos, err := conn.PageTargets()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1025; i++ {
		srv.removeTargetState(target.ID(fmt.Sprintf("gone-%d", i)))
	}
	snaps := srv.applyEnumeration(conn, win, infos)
	if len(snaps) != 1 || snaps[0].TargetID != idA {
		t.Fatalf("overtaken reply = %v, want A (it was not removed meanwhile)", snaps)
	}
	persisted := managedTargetSet(srv.store.ManagedTabs())
	if !persisted[idA] {
		t.Error("managed tab still listed by the browser was retired")
	}
	if persisted[gone] {
		t.Error("managed tab absent from the reply was not retired")
	}
	if _, ok := srv.tabIdle.Snapshot()[idA]; !ok {
		t.Error("managed tab was not recovered for idle tracking")
	}
	if n := srv.tabs.OpenWindows(); n != 0 {
		t.Errorf("%d windows left open", n)
	}
	assertReplayedManaged(t, srv, dir, map[target.ID]bool{idA: true})
}

func assertReplayedManaged(t *testing.T, srv *Server, dir string, want map[target.ID]bool) {
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
	replayed := managedTargetSet(reopened.ManagedTabs())
	if len(replayed) != len(want) {
		t.Errorf("replayed managed set = %v, want %v", replayed, want)
	}
	for id := range want {
		if !replayed[id] {
			t.Errorf("replayed managed set = %v, want %v", replayed, want)
		}
	}
}

// A persisted tab reported gone while the enumeration is in flight (its
// targetDestroyed arrives for a tab this daemon run never registered) is
// retired by that report, not brought back by the reply that still lists it;
// a persisted tab listed only as a non-page target counts as absent.
func TestManagedRecoveryJudgesAbsenceOnTheReply(t *testing.T) {
	const idA, idB = fullIDA, fullIDB
	dir := t.TempDir()
	seedManagedState(t, dir, idA, idB)
	conn := newEnumConn(idA)
	srv := newReconcileRaceServer(t, conn, dir, 0)

	win := srv.tabs.OpenWindow()
	infos := []*target.Info{{TargetID: idA, Type: "page"}, {TargetID: idB, Type: "service_worker"}}
	srv.removeEventTarget(conn, idA) // targetDestroyed
	if snaps := srv.applyEnumeration(conn, win, infos); len(snaps) != 0 {
		t.Fatalf("reply older than the destroyed event registered %v", snaps)
	}
	persisted := managedTargetSet(srv.store.ManagedTabs())
	if persisted[idB] {
		t.Error("tab listed only as a non-page target was not retired")
	}
	if persisted[idA] {
		t.Error("destroyed tab A is still persisted")
	}
	if _, ok := srv.tabIdle.Snapshot()[idA]; ok || srv.tabs.Has(idA) {
		t.Error("destroyed tab A is registered or idle-tracked")
	}
}

// The defensive branch: a persisted tab the reply lists but the registry did
// not accept is neither adopted nor retired on that reply; recovery stays
// pending, asks for another enumeration, and that one settles it.
func TestManagedRecoveryKeepsListedButUnacceptedTabAndRetries(t *testing.T) {
	const idA = fullIDA
	gone := reconcileGoneID
	dir := t.TempDir()
	seedManagedState(t, dir, idA, gone)
	conn := newEnumConn(idA)
	srv := newReconcileRaceServer(t, conn, dir, 0)

	win := srv.tabs.OpenWindow()
	infos, err := conn.PageTargets()
	if err != nil {
		t.Fatal(err)
	}
	// Evidence against A reaches the registry alone (no production path does
	// this without retiring the tab in the same critical section).
	srv.tabs.RemoveTarget(idA)
	marker := &countingDirtyMarker{}
	if snaps := srv.applyEnumeration(marker, win, infos); len(snaps) != 0 {
		t.Fatalf("precondition: the reply must not be accepted, got %v", snaps)
	}
	if marker.n != 1 {
		t.Errorf("another enumeration requested %d times, want 1", marker.n)
	}
	persisted := managedTargetSet(srv.store.ManagedTabs())
	if !persisted[idA] {
		t.Error("managed tab still listed by the browser was retired on an unaccepted reply")
	}
	if persisted[gone] {
		t.Error("managed tab absent from the reply was not retired")
	}
	if _, ok := srv.tabIdle.Snapshot()[idA]; ok {
		t.Error("unaccepted tab is idle-tracked")
	}

	// The retry is a current enumeration: A is recovered for idle tracking.
	reconcileFreshConn(t, srv, conn)
	if _, ok := srv.tabIdle.Snapshot()[idA]; !ok {
		t.Error("managed tab was not recovered for idle tracking")
	}
	assertReplayedManaged(t, srv, dir, map[target.ID]bool{idA: true})
}

type countingDirtyMarker struct{ n int }

func (m *countingDirtyMarker) MarkTargetDirty() { m.n++ }

// Recovery is not tied to one particular enumeration. While the startup / dirty
// resync is still in flight, an ordinary tab_list registers the persisted tab
// A; that same step resumes its idle tracking, so A can never end up registered
// and persisted but untracked, whichever reply is applied first.
func TestManagedRecoveryHappensWithWhicheverEnumerationRegistersTheTab(t *testing.T) {
	const idA = fullIDA
	dir := t.TempDir()
	seedManagedState(t, dir, idA, reconcileGoneID)
	conn := newEnumConn(idA)
	srv := newReconcileRaceServer(t, conn, dir, 0)

	resync := srv.tabs.OpenWindow() // the sync loop's request, reply parked
	infos, err := conn.PageTargets()
	if err != nil {
		t.Fatal(err)
	}
	enumMustRPC(t, srv, "tab_list", nil)
	if _, ok := srv.tabIdle.Snapshot()[idA]; !ok || !srv.tabs.Has(idA) {
		t.Fatal("tab_list registered the persisted tab without resuming its idle tracking")
	}
	if managedTargetSet(srv.store.ManagedTabs())[reconcileGoneID] {
		t.Error("persisted tab absent from the tab_list reply was not retired")
	}
	at := srv.tabIdle.Snapshot()[idA]
	srv.applyEnumeration(conn, resync, infos)
	if got, ok := srv.tabIdle.Snapshot()[idA]; !ok || !got.Equal(at) {
		t.Errorf("late resync reply changed idle tracking of A: %v (tracked %v), want %v", got, ok, at)
	}
	assertReplayedManaged(t, srv, dir, map[target.ID]bool{idA: true})
}

// A lifecycle event that had to be refused as older than applied evidence asks
// for a fresh enumeration instead of being lost.
func TestRefusedTargetEventRequestsResync(t *testing.T) {
	conn := newEnumConn(fullIDB)
	srv := newEnumServer(t, conn)
	defer srv.closeAudit()
	enumMustRPC(t, srv, "tab_list", nil)

	marker := &countingDirtyMarker{}
	stamp := srv.tabs.NoteEventRead(fullIDA, "")
	srv.applyTargetEvent(marker, browserInfoEvent(stamp, fullIDB)) // registered: nothing to do
	srv.applyTargetEvent(marker, browserInfoEvent(stamp, fullIDA)) // current: registers
	if marker.n != 0 || !srv.tabs.Has(fullIDA) {
		t.Fatalf("current events: resync requests = %d, A registered = %v", marker.n, srv.tabs.Has(fullIDA))
	}
	srv.removeTargetState(fullIDA)                                 // tab_close confirmed
	srv.applyTargetEvent(marker, browserInfoEvent(stamp, fullIDA)) // read before the close
	if srv.tabs.Has(fullIDA) {
		t.Fatal("queued event revived the closed tab")
	}
	if marker.n != 1 {
		t.Fatalf("resync requests = %d, want 1", marker.n)
	}
	// Non-page and destroyed events are removals and never ask for a resync.
	srv.applyTargetEvent(marker, browser.TargetEvent{Stamp: stamp, Info: &target.Info{TargetID: "WORKER", Type: "service_worker"}})
	srv.applyTargetEvent(marker, browser.TargetEvent{Stamp: stamp, Destroyed: fullIDB})
	if marker.n != 1 || srv.tabs.Has(fullIDB) {
		t.Fatalf("resync requests = %d, B registered = %v", marker.n, srv.tabs.Has(fullIDB))
	}
}

func browserInfoEvent(stamp uint64, id target.ID) browser.TargetEvent {
	return browser.TargetEvent{Stamp: stamp, Info: &target.Info{TargetID: id, Type: "page"}}
}
