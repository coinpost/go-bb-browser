package daemon

import (
	"fmt"
	"testing"

	"github.com/chromedp/cdproto/target"
	"github.com/yiplee/go-bb-browser/internal/store"
)

// Regressions for the managed reconciliation of an enumeration reply the
// registry did not (fully) accept: "not accepted" is not "gone".

// A managed tab that the browser still lists must not be retired because the
// registry could not verify the reply (overtaken by more removals than it
// remembers): only absence from the reply itself retires, and the caller is
// told to enumerate again.
func TestUnverifiableEnumerationDoesNotRetireListedManagedTab(t *testing.T) {
	const idA = fullIDA
	gone := reconcileGoneID
	dir := t.TempDir()
	seedManagedState(t, dir, idA, gone)
	conn := newEnumConn(idA)
	srv := newReconcileRaceServer(t, conn, dir, 0)

	candidates := srv.beginManagedReconcile()
	tok := srv.beginTargetEnumeration()
	infos, err := conn.PageTargets()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1025; i++ {
		srv.removeTargetState(target.ID(fmt.Sprintf("gone-%d", i)))
	}
	snaps := srv.syncTabsFromTargets(tok, infos)
	if len(snaps) != 0 {
		t.Fatalf("precondition: the overtaken reply must not be accepted, got %v", snaps)
	}
	if srv.reconcileEnumeratedManaged(candidates, infos, snaps) {
		t.Error("reconciliation reported settled although a listed managed tab was not verified")
	}
	persisted := managedTargetSet(srv.store.ManagedTabs())
	if !persisted[idA] {
		t.Error("managed tab still listed by the browser was retired on an unverifiable reply")
	}
	if persisted[gone] {
		t.Error("managed tab absent from the reply was not retired")
	}

	// The retry is a current enumeration: A is recovered for idle tracking.
	candidates = srv.beginManagedReconcile()
	tok = srv.beginTargetEnumeration()
	infos, err = conn.PageTargets()
	if err != nil {
		t.Fatal(err)
	}
	if !srv.reconcileEnumeratedManaged(candidates, infos, srv.syncTabsFromTargets(tok, infos)) {
		t.Error("current enumeration did not settle")
	}
	if _, ok := srv.tabIdle.Snapshot()[idA]; !ok {
		t.Error("managed tab was not recovered for idle tracking")
	}
	drainRPCLog(t, srv)
	if err := srv.closeAudit(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(store.OpenConfig{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if replayed := managedTargetSet(reopened.ManagedTabs()); !replayed[idA] || replayed[gone] {
		t.Errorf("replayed managed set = %v, want only A", replayed)
	}
}

// A candidate the reply lists but the registry refused because it was removed
// after the request is not retired by the reply (the removal did that), and a
// candidate listed as a non-page target counts as absent.
func TestReconcileEnumeratedManagedJudgesAbsenceOnTheReply(t *testing.T) {
	const idA, idB = fullIDA, fullIDB
	dir := t.TempDir()
	seedManagedState(t, dir, idA, idB)
	conn := newEnumConn(idA)
	srv := newReconcileRaceServer(t, conn, dir, 0)

	candidates := srv.beginManagedReconcile()
	tok := srv.beginTargetEnumeration()
	infos := []*target.Info{{TargetID: idA, Type: "page"}, {TargetID: idB, Type: "service_worker"}}
	srv.removeTargetState(idA) // targetDestroyed for a tab this daemon run never registered
	snaps := srv.syncTabsFromTargets(tok, infos)
	if srv.reconcileEnumeratedManaged(candidates, infos, snaps) {
		t.Error("settled although the listed tab A was neither accepted nor retired")
	}
	persisted := managedTargetSet(srv.store.ManagedTabs())
	if persisted[idB] {
		t.Error("candidate listed only as a non-page target was not retired")
	}
	if _, ok := srv.tabIdle.Snapshot()[idA]; ok {
		t.Error("destroyed tab A is idle-tracked")
	}
	// The retry no longer lists A: retired, settled.
	conn.removeExternally(idA)
	reconcileFreshConnSettled(t, srv, conn)
	if managedTargetSet(srv.store.ManagedTabs())[idA] {
		t.Error("destroyed tab A is still persisted after the retry")
	}
}

func reconcileFreshConnSettled(t *testing.T, srv *Server, conn *enumConn) {
	t.Helper()
	candidates := srv.beginManagedReconcile()
	tok := srv.beginTargetEnumeration()
	infos, err := conn.PageTargets()
	if err != nil {
		t.Fatal(err)
	}
	if !srv.reconcileEnumeratedManaged(candidates, infos, srv.syncTabsFromTargets(tok, infos)) {
		t.Error("current enumeration did not settle the managed set")
	}
}
