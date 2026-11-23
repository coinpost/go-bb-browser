package state

import (
	"fmt"
	"sync"
	"testing"

	"github.com/chromedp/cdproto/target"
)

func pageInfos(ids ...target.ID) []*target.Info {
	out := make([]*target.Info, 0, len(ids))
	for _, id := range ids {
		out = append(out, &target.Info{TargetID: id, Type: "page"})
	}
	return out
}

func TestApplyEnumerationKeepsTargetsRegisteredAfterWindowOpened(t *testing.T) {
	r := NewTabRegistry()
	r.SyncPageTargets(pageInfos("AAAA0001", "AAAA0002"))

	tok := r.OpenWindow()
	// Browser snapshot taken here: AAAA0002 is already gone, BBBB0003 not yet created.
	stale := pageInfos("AAAA0001")
	const newID = target.ID("BBBB0003")
	r.RegisterPageTarget(newID)
	if !r.Select(newID) {
		t.Fatal("select failed")
	}

	snaps, removed, _ := r.ApplyEnumeration(tok, stale)
	if len(snaps) != 1 || snaps[0].TargetID != "AAAA0001" {
		t.Fatalf("snapshots = %#v, want only the enumerated target", snaps)
	}
	if len(removed) != 1 || removed[0].TargetID != "AAAA0002" {
		t.Fatalf("removed = %#v, want only AAAA0002", removed)
	}
	if id, ok := r.Lookup(string(newID)); !ok || id != newID {
		t.Fatalf("target registered after the token was removed (%q, %v)", id, ok)
	}
	if r.Selected() != newID {
		t.Fatalf("selection = %q, want %q", r.Selected(), newID)
	}

	// An enumeration requested after the registration is authoritative for it.
	_, removed, _ = r.ApplyEnumeration(r.OpenWindow(), stale)
	if len(removed) != 1 || removed[0].TargetID != "BBBB0003" {
		t.Fatalf("removed = %#v, want BBBB0003", removed)
	}
	if _, ok := r.Lookup(string(newID)); ok {
		t.Fatal("fresh enumeration kept a vanished target")
	}
	if r.Selected() != "" {
		t.Fatalf("selection = %q, want cleared", r.Selected())
	}
}

// A target that is already registered is not made "newer" by later evidence
// that it exists (an event, another listing): it was registered before the
// enumeration was requested, so a reply that does not list it proves it gone.
// Renewing it would let late evidence hide a real absence.
func TestExistenceEvidenceDoesNotRenewRegisteredTarget(t *testing.T) {
	r := NewTabRegistry()
	r.SyncPageTargets(pageInfos("AAAA0001"))

	w := r.OpenWindow()
	r.RegisterPageTarget("AAAA0001")
	if !r.RegisterEvent(r.NoteEventRead("AAAA0001", ""), "AAAA0001") {
		t.Fatal("event for a known target must report it known")
	}
	r.ApplyEnumeration(r.OpenWindow(), pageInfos("AAAA0001")) // listed again meanwhile
	later := r.OpenWindow()
	if _, removed, _ := r.ApplyEnumeration(later, nil); len(removed) != 1 || r.Has("AAAA0001") {
		t.Fatalf("removed = %#v, want AAAA0001: it was registered before the request", removed)
	}
	r.Abandon(w)
}

// Two overlapping enumerations that disagree about a target cannot be ordered:
// the one that lists it may be the older snapshot (the target was destroyed in
// between) just as well as the newer one (it was opened in between). A target
// known only from such a listing is therefore not protected from the other
// reply, exactly as when the replies are applied in the opposite order
// (TestOverlappingEnumerationsAbsenceWinsInEitherOrder). But nothing says it is
// gone either: it is suspended — it stops resolving, is not reported removed
// (so nothing that belongs to a possibly live page is released), stays known —
// and the caller is told to ask again.
//
// History of this assertion: first "an older empty snapshot does not remove a
// target registered by a newer enumeration" (kept a destroyed tab resolvable
// when its notification was lost), then "absence wins and the removal is
// reported Contested" (released the browser attachment of a live page, which
// closed it). The creation protection belongs to targets the daemon knows were
// created meanwhile (CommitCreated, lifecycle events), see
// TestApplyEnumerationKeepsTargetsRegisteredAfterWindowOpened and
// TestCommitCreatedProtectsNewTargetFromOverlappingEnumeration.
func TestOverlappingEnumerationSuspendsDiscoveredTarget(t *testing.T) {
	r := NewTabRegistry()
	older := r.OpenWindow()
	newer := r.OpenWindow()

	r.ApplyEnumeration(newer, pageInfos("AAAA0001"))
	r.Select("AAAA0001")
	tabs, removed, verify := r.ApplyEnumeration(older, nil)
	if len(removed) != 0 || !verify || len(tabs) != 0 {
		t.Fatalf("tabs = %v removed = %#v verify = %v, want nothing removed and a fresh enumeration requested", snapshotIDs(tabs), removed, verify)
	}
	if r.Has("AAAA0001") || r.Selected() != "" {
		t.Fatal("a target two overlapping snapshots disagree about still resolves")
	}
	if _, ok := r.Lookup("AAAA0001"); ok || r.Select("AAAA0001") {
		t.Fatal("suspended target can be looked up or selected")
	}
	if !r.Known("AAAA0001") {
		t.Fatal("the contradiction made the registry forget the target")
	}
	// The enumeration requested after the conflict decides: a live target
	// resolves again (and is the focused tab it was).
	tabs, removed, verify = r.ApplyEnumeration(r.OpenWindow(), pageInfos("AAAA0001"))
	if len(tabs) != 1 || len(removed) != 0 || verify || !r.Has("AAAA0001") || r.Selected() != "AAAA0001" {
		t.Fatalf("tabs = %v removed = %#v verify = %v, want AAAA0001 resolving again", snapshotIDs(tabs), removed, verify)
	}
	// Merely being listed again does not renew anything: the next enumeration
	// requested afterwards still removes it, as plainly gone.
	tok := r.OpenWindow()
	r.ApplyEnumeration(r.OpenWindow(), pageInfos("AAAA0001"))
	if _, removed, verify := r.ApplyEnumeration(tok, nil); len(removed) != 1 || verify || r.Known("AAAA0001") {
		t.Fatalf("removed = %#v verify = %v, want AAAA0001 removed for good", removed, verify)
	}
}

// A suspended target that is gone is removed, with its removal reported, by the
// enumeration requested after the conflict — and that removal is recorded in
// the requests still in flight.
func TestEnumerationAfterConflictRemovesSuspendedTargetThatIsGone(t *testing.T) {
	r := NewTabRegistry()
	r.SyncPageTargets(pageInfos(regIDB))
	older := r.OpenWindow()
	create := r.OpenWindow() // a tab_new whose reply is late
	r.ApplyEnumeration(r.OpenWindow(), pageInfos(regIDA, regIDB))
	if _, removed, verify := r.ApplyEnumeration(older, pageInfos(regIDB)); len(removed) != 0 || !verify || r.Has(regIDA) {
		t.Fatalf("removed = %#v verify = %v", removed, verify)
	}
	_, removed, verify := r.ApplyEnumeration(r.OpenWindow(), pageInfos(regIDB))
	if len(removed) != 1 || removed[0].TargetID != regIDA || verify || r.Known(regIDA) {
		t.Fatalf("removed = %#v verify = %v, want A removed by the enumeration requested after the conflict", removed, verify)
	}
	if ok, _ := r.CommitCreated(create, regIDA); ok || r.Known(regIDA) {
		t.Fatal("a creation reply later than the confirmed absence registered the target")
	}
	if !r.Has(regIDB) || r.OpenWindows() != 0 {
		t.Fatalf("B=%v windows=%d", r.Has(regIDB), r.OpenWindows())
	}
}

// The contradiction is symmetric. Applied in the other order — the snapshot
// without the target first, the one listing it second — the target is known
// but suspended as well, and a fresh enumeration is requested just the same.
func TestOverlappingEnumerationsReverseOrderAlsoRequestsVerification(t *testing.T) {
	r := NewTabRegistry()
	r.SyncPageTargets(pageInfos(regIDB))
	absent := r.OpenWindow()
	listing := r.OpenWindow()
	if _, _, verify := r.ApplyEnumeration(absent, pageInfos(regIDB)); verify {
		t.Fatal("nothing to verify yet")
	}
	tabs, removed, verify := r.ApplyEnumeration(listing, pageInfos(regIDA, regIDB))
	if !verify || len(removed) != 0 || hasSnapshot(tabs, regIDA) || r.Has(regIDA) || !r.Known(regIDA) {
		t.Fatalf("tabs = %v removed = %#v verify = %v known = %v, want A suspended and a fresh enumeration requested",
			snapshotIDs(tabs), removed, verify, r.Known(regIDA))
	}
	if tabs, _, verify := r.ApplyEnumeration(r.OpenWindow(), pageInfos(regIDA, regIDB)); verify || !hasSnapshot(tabs, regIDA) || !r.Has(regIDA) {
		t.Fatalf("tabs = %v verify = %v, want the fresh enumeration to bring a live A back", snapshotIDs(tabs), verify)
	}
}

func TestEndedWindowAppliesNothing(t *testing.T) {
	r := NewTabRegistry()
	r.RegisterPageTarget("AAAA0001")
	for name, end := range map[string]func(*Window){
		"abandoned": func(w *Window) { r.Abandon(w) },
		"applied":   func(w *Window) { r.ApplyEnumeration(w, pageInfos("AAAA0001")) },
		"committed": func(w *Window) { commitOK(r, w, "AAAA0001") },
	} {
		w := r.OpenWindow()
		end(w)
		snaps, removed, _ := r.ApplyEnumeration(w, pageInfos("AAAA0002"))
		if len(removed) != 0 || len(snaps) != 0 || r.Has("AAAA0002") || !r.Has("AAAA0001") {
			t.Fatalf("%s window: snaps=%v removed=%#v", name, snapshotIDs(snaps), removed)
		}
		if commitOK(r, w, "AAAA0003") || r.Has("AAAA0003") {
			t.Fatalf("%s window committed a creation", name)
		}
		r.Abandon(w) // idempotent
	}
	if n := r.OpenWindows(); n != 0 {
		t.Fatalf("%d windows left open", n)
	}
	// A nil window stands for a request made right now.
	if _, removed, _ := r.ApplyEnumeration(nil, pageInfos("AAAA0002")); len(removed) != 1 || !r.Has("AAAA0002") {
		t.Fatalf("nil window: removed=%#v", removed)
	}
}

func TestSyncPageTargetsTreatsListAsCurrent(t *testing.T) {
	r := NewTabRegistry()
	r.RegisterPageTarget("AAAA0001")
	if _, removed := r.SyncPageTargetsDetailed(nil); len(removed) != 1 {
		t.Fatalf("removed = %#v, want AAAA0001", removed)
	}
}

func snapshotIDs(snaps []TabSnapshot) []target.ID {
	out := make([]target.ID, 0, len(snaps))
	for _, sn := range snaps {
		out = append(out, sn.TargetID)
	}
	return out
}

func TestApplyEnumerationDoesNotReviveTargetRemovedAfterToken(t *testing.T) {
	r := NewTabRegistry()
	r.SyncPageTargets(pageInfos(regIDA, regIDB))
	if !r.Select(regIDA) {
		t.Fatal("select failed")
	}

	tok, tok2 := r.OpenWindow(), r.OpenWindow()
	stale := pageInfos(regIDA, regIDB) // browser snapshot taken while A still existed
	if !r.RemoveTarget(regIDA) {
		t.Fatal("precondition: A not removed")
	}

	snaps, removed, _ := r.ApplyEnumeration(tok, stale)
	if ids := snapshotIDs(snaps); len(ids) != 1 || ids[0] != regIDB {
		t.Fatalf("snapshots = %v, want only %s", ids, regIDB)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %#v, want none (A was already removed)", removed)
	}
	if id, ok := r.Lookup(string(regIDA)); ok {
		t.Fatalf("removed target resolves again as %q", id)
	}
	if !r.Has(regIDB) {
		t.Fatal("B lost")
	}
	if r.Select(regIDA) || r.Selected() != "" {
		t.Fatalf("selection = %q, want none", r.Selected())
	}
	// Another reply requested before the removal changes nothing either.
	if snaps, _, _ := r.ApplyEnumeration(tok2, stale); len(snaps) != 1 || r.Has(regIDA) {
		t.Fatalf("second stale apply revived A: %v", snapshotIDs(snaps))
	}

	// An enumeration requested after the removal is current for A: if the
	// browser lists that id then, it is a live target.
	snaps, _, _ = r.ApplyEnumeration(r.OpenWindow(), stale)
	if len(snaps) != 2 || !r.Has(regIDA) {
		t.Fatalf("current enumeration did not register A: %v", snapshotIDs(snaps))
	}
}

func TestApplyEnumerationRemovalByNewerEnumerationWins(t *testing.T) {
	r := NewTabRegistry()
	r.SyncPageTargets(pageInfos(regIDA, regIDB))

	older := r.OpenWindow() // snapshot: A and B
	newer := r.OpenWindow() // snapshot: only B
	if _, removed, _ := r.ApplyEnumeration(newer, pageInfos(regIDB)); len(removed) != 1 || removed[0].TargetID != regIDA {
		t.Fatalf("removed = %#v, want A", removed)
	}
	snaps, removed, _ := r.ApplyEnumeration(older, pageInfos(regIDA, regIDB))
	if len(removed) != 0 || len(snaps) != 1 || snaps[0].TargetID != regIDB || r.Has(regIDA) {
		t.Fatalf("older reply revived A: snaps=%v removed=%#v", snapshotIDs(snaps), removed)
	}
}

func TestApplyEnumerationKeepsTargetReRegisteredAfterRemoval(t *testing.T) {
	r := NewTabRegistry()
	r.SyncPageTargets(pageInfos(regIDA))

	toks := []*Window{r.OpenWindow(), r.OpenWindow()}
	r.RemoveTarget(regIDA)
	r.RegisterPageTarget(regIDA) // announced again: live, newer than the requests

	for i, infos := range [][]*target.Info{pageInfos(regIDA), nil} {
		snaps, removed, _ := r.ApplyEnumeration(toks[i], infos)
		if len(removed) != 0 || !r.Has(regIDA) {
			t.Fatalf("re-registered target removed by an older enumeration (infos=%d): %#v", len(infos), removed)
		}
		if len(snaps) != len(infos) {
			t.Fatalf("snapshots = %v for %d listed targets", snapshotIDs(snaps), len(infos))
		}
	}
	// And a removal after that re-registration is honoured again.
	tok := r.OpenWindow()
	r.RemoveTarget(regIDA)
	if snaps, _, _ := r.ApplyEnumeration(tok, pageInfos(regIDA)); len(snaps) != 0 || r.Has(regIDA) {
		t.Fatal("second removal not honoured")
	}
}

// A target can be reported gone before the registry ever saw it (its lifecycle
// event overtakes the first enumeration that lists it).
func TestApplyEnumerationSkipsTargetReportedGoneBeforeRegistration(t *testing.T) {
	r := NewTabRegistry()
	tok := r.OpenWindow()
	if r.RemoveTarget(regIDA) {
		t.Fatal("unregistered target reported as removed")
	}
	snaps, _, _ := r.ApplyEnumeration(tok, pageInfos(regIDA, regIDB))
	if len(snaps) != 1 || snaps[0].TargetID != regIDB || r.Has(regIDA) {
		t.Fatalf("snapshots = %v, want only B", snapshotIDs(snaps))
	}
	// Repeated reports for an id that stays unregistered take one slot in a
	// window, and none at all once no request is in flight.
	w := r.OpenWindow()
	for i := 0; i < 10; i++ {
		r.RemoveTarget(regIDA)
		r.RemoveEventTarget(regIDA)
		r.RemoveTarget("")
	}
	if len(w.removed) != 1 {
		t.Fatalf("window holds %d removals for one id", len(w.removed))
	}
	r.Abandon(w)
	if r.OpenWindows() != 0 {
		t.Fatal("window not released")
	}
}

// The registry keeps no history. What it knows about removals lives in the
// windows of the requests in flight: any amount of churn with no request in
// flight leaves nothing behind, and a request in flight sees exactly the
// removals of its own window, however many there were.
func TestRemovalEvidenceLivesAndDiesWithWindows(t *testing.T) {
	r := NewTabRegistry()
	r.RegisterPageTarget(regIDB)

	id := func(i int) target.ID { return target.ID(fmt.Sprintf("CCCC%024X1234", i)) }
	const churn = 5000
	for i := 0; i < churn; i++ {
		r.RegisterPageTarget(id(i))
		if !r.RemoveTarget(id(i)) {
			t.Fatalf("remove %d failed", i)
		}
	}
	if len(r.targets) != 1 || r.OpenWindows() != 0 {
		t.Fatalf("registered = %d, windows = %d; want only B and no window", len(r.targets), r.OpenWindows())
	}

	// A request overtaken by any number of removals is still exact: it
	// registers none of the ids removed since it was sent, and does register a
	// target that was never removed. There is no "too old to verify".
	ancient := r.OpenWindow()
	for i := churn; i < 2*churn; i++ {
		r.RegisterPageTarget(id(i))
		r.RemoveTarget(id(i))
		if i%5 == 0 { // again, after a re-registration
			r.RegisterPageTarget(id(i))
			r.RemoveEventTarget(id(i))
		}
	}
	if len(ancient.removed) != churn {
		t.Fatalf("window recorded %d removals, want %d (one per id)", len(ancient.removed), churn)
	}
	never := target.ID("DDDD0000000000000000000000AB1234")
	snaps, removed, _ := r.ApplyEnumeration(ancient, pageInfos(id(churn), id(2*churn-1), never, regIDB))
	if ids := snapshotIDs(snaps); len(ids) != 2 || !r.Has(never) || !r.Has(regIDB) {
		t.Fatalf("overtaken enumeration snapshots = %v, want B and the never-removed target", ids)
	}
	if len(removed) != 0 || r.Has(id(churn)) || r.Has(id(2*churn-1)) {
		t.Fatal("overtaken enumeration registered a removed target")
	}
	if r.OpenWindows() != 0 {
		t.Fatal("applied window still held")
	}
	// Removals from before a request do not concern it: a current enumeration
	// is authoritative for everything it lists.
	snaps, _, _ = r.ApplyEnumeration(r.OpenWindow(), pageInfos(id(0), id(churn), never, regIDB))
	if len(snaps) != 4 {
		t.Fatalf("current enumeration snapshots = %v", snapshotIDs(snaps))
	}
}

// Every way a request can end releases its window, so the evidence kept is
// bounded by the requests actually in flight.
func TestEveryRequestOutcomeReleasesItsWindow(t *testing.T) {
	r := NewTabRegistry()
	var ws []*Window
	for i := 0; i < 6; i++ {
		ws = append(ws, r.OpenWindow())
	}
	if r.OpenWindows() != 6 {
		t.Fatalf("open = %d", r.OpenWindows())
	}
	r.RemoveTarget(regIDA)
	r.ApplyEnumeration(ws[0], pageInfos(regIDB))
	for _, w := range ws[1:] {
		if _, ok := w.removed[regIDA]; !ok || !w.narrowed || len(w.listed) != 1 {
			t.Fatalf("window in flight missed evidence: removed=%v narrowed=%v listed=%v", w.removed, w.narrowed, w.listed)
		}
	}
	r.ApplyEnumeration(ws[1], nil)
	commitOK(r, ws[2], regIDA) // refused: removed in its window
	commitOK(r, ws[3], "CCCC")
	r.Abandon(ws[4])
	r.Abandon(ws[5])
	r.Abandon(ws[5])
	if r.OpenWindows() != 0 {
		t.Fatalf("open = %d, want 0", r.OpenWindows())
	}
	// Ended windows are no longer fed.
	r.RemoveTarget("CCCC")
	if _, ok := ws[4].removed["CCCC"]; ok {
		t.Fatal("ended window still collects evidence")
	}
}

// Whatever the interleaving of a removal with the apply of a snapshot that was
// requested before it, the removed id must not be registered afterwards, and a
// target registered after the request must survive.
func TestConcurrentRemovalAndStaleApply(t *testing.T) {
	r := NewTabRegistry()
	r.RegisterPageTarget(regIDB)
	for i := 0; i < 2000; i++ {
		gone := target.ID(fmt.Sprintf("AAAA%024X1234", i))
		fresh := target.ID(fmt.Sprintf("EEEE%024X1234", i))
		r.RegisterPageTarget(gone)
		tok, tok2 := r.OpenWindow(), r.OpenWindow()
		stale := pageInfos(gone, regIDB)

		var wg sync.WaitGroup
		wg.Add(4)
		go func() { defer wg.Done(); r.RemoveTarget(gone) }()
		go func() { defer wg.Done(); r.ApplyEnumeration(tok, stale) }()
		go func() { defer wg.Done(); r.ApplyEnumeration(tok2, stale) }()
		go func() { defer wg.Done(); r.RegisterPageTarget(fresh) }()
		wg.Wait()

		if r.Has(gone) {
			t.Fatalf("iteration %d: removed target is registered", i)
		}
		if !r.Has(fresh) || !r.Has(regIDB) {
			t.Fatalf("iteration %d: live target lost", i)
		}
		r.RemoveTarget(fresh)
	}
}

// A removal report for a target that is already unregistered (tab_close removed
// it, then Target.targetDestroyed arrives) is still a removal: an enumeration
// requested between the two predates the second one and must not register it.
func TestRepeatedRemovalRefreshesRemovalGeneration(t *testing.T) {
	r := NewTabRegistry()
	r.SyncPageTargets(pageInfos(regIDA, regIDB))
	if !r.RemoveTarget(regIDA) {
		t.Fatal("first removal of a registered target must report true")
	}

	toks := []*Window{r.OpenWindow(), r.OpenWindow()}
	stale := pageInfos(regIDA, regIDB) // the browser had not destroyed A yet
	if r.RemoveEventTarget(regIDA) {
		t.Fatal("repeated removal of an unregistered target must report false")
	}
	if tok2 := r.OpenWindow(); tok2.opened <= toks[0].opened {
		t.Fatalf("repeated removal did not advance the clock (%d -> %d)", toks[0].opened, tok2.opened)
	} else {
		r.Abandon(tok2)
	}

	for i := 0; i < 2; i++ {
		snaps, removed, _ := r.ApplyEnumeration(toks[i], stale)
		if ids := snapshotIDs(snaps); len(ids) != 1 || ids[0] != regIDB {
			t.Fatalf("apply %d: snapshots = %v, want only %s", i, ids, regIDB)
		}
		if len(removed) != 0 || r.Has(regIDA) || !r.Has(regIDB) {
			t.Fatalf("apply %d: A registered=%v B registered=%v removed=%#v", i, r.Has(regIDA), r.Has(regIDB), removed)
		}
	}
	if _, ok := r.Lookup(string(regIDA)); ok {
		t.Fatal("removed target resolves again")
	}

	// An enumeration requested after the last removal is current for A.
	if snaps, _, _ := r.ApplyEnumeration(r.OpenWindow(), stale); len(snaps) != 2 || !r.Has(regIDA) {
		t.Fatalf("current enumeration did not register A: %v", snapshotIDs(snaps))
	}
}

// The same holds for an id the registry never registered: every report counts,
// not only the first one.
func TestRepeatedRemovalOfNeverRegisteredTargetRefreshes(t *testing.T) {
	r := NewTabRegistry()
	r.RegisterPageTarget(regIDB)
	if r.RemoveTarget(regIDA) {
		t.Fatal("unregistered target reported as removed")
	}
	tok := r.OpenWindow()
	if r.RemoveTarget(regIDA) {
		t.Fatal("unregistered target reported as removed")
	}
	snaps, _, _ := r.ApplyEnumeration(tok, pageInfos(regIDA, regIDB))
	if ids := snapshotIDs(snaps); len(ids) != 1 || ids[0] != regIDB || r.Has(regIDA) {
		t.Fatalf("snapshots = %v, want only B", ids)
	}
}

// Repeated reports cost no memory: however often one id is reported, it holds
// one slot per window in flight, and other removals stay recorded.
func TestRepeatedRemovalStaysBounded(t *testing.T) {
	r := NewTabRegistry()
	r.SyncPageTargets(pageInfos(regIDA, regIDB))
	other := target.ID("CCCC0000000000000000000000AB1234")
	never := target.ID("DDDD0000000000000000000000AB1234")
	r.RegisterPageTarget(other)

	old := r.OpenWindow()
	if !r.RemoveTarget(other) || !r.RemoveTarget(regIDA) {
		t.Fatal("precondition: removals failed")
	}
	var tok *Window
	const repeats = 3079
	for i := 0; i < repeats; i++ {
		if i == repeats-1 {
			tok = r.OpenWindow()
		}
		if r.RemoveEventTarget(regIDA) {
			t.Fatalf("repeat %d reported a registered target", i)
		}
	}
	if len(old.removed) != 2 || len(tok.removed) != 1 {
		t.Fatalf("window evidence: old=%d tok=%d, want 2 and 1", len(old.removed), len(tok.removed))
	}

	// The older removal was not crowded out, and the old request stays exact:
	// it filters both removed ids and still registers a target never removed.
	snaps, _, _ := r.ApplyEnumeration(old, pageInfos(regIDA, other, never, regIDB))
	if ids := snapshotIDs(snaps); len(ids) != 2 || r.Has(regIDA) || r.Has(other) || !r.Has(never) {
		t.Fatalf("old enumeration snapshots = %v, want B and the never-removed target", ids)
	}
	// The enumeration requested before the very last report is still stale for A.
	if snaps, _, _ := r.ApplyEnumeration(tok, pageInfos(regIDA, regIDB)); len(snaps) != 1 || r.Has(regIDA) {
		t.Fatalf("snapshots = %v, want only B", snapshotIDs(snaps))
	}
	if r.OpenWindows() != 0 {
		t.Fatal("windows left open")
	}
}

// A removal reported again and again stays in force for a request in flight
// whatever else is removed meanwhile.
func TestRepeatedRemovalSurvivesChurn(t *testing.T) {
	r := NewTabRegistry()
	r.RegisterPageTarget(regIDA)
	r.RemoveTarget(regIDA)
	id := func(i int) target.ID { return target.ID(fmt.Sprintf("CCCC%024X1234", i)) }
	var tok *Window
	const n = 2048
	for i := 0; i < n; i++ {
		r.RegisterPageTarget(id(i))
		r.RemoveTarget(id(i))
		if i == n-3 {
			tok = r.OpenWindow()
		}
		r.RemoveEventTarget(regIDA) // reported again and again
	}
	if snaps, _, _ := r.ApplyEnumeration(tok, pageInfos(regIDA)); len(snaps) != 0 || r.Has(regIDA) {
		t.Fatal("refreshed removal forgotten")
	}
}

// Lifecycle events wait in a queue. One that says a target exists is applied
// only if no enumeration, and no removal learned from a command reply, was
// applied after the event was read from the connection.
func TestRegisterEventOrdersQueuedEventAgainstNewerEvidence(t *testing.T) {
	t.Run("fresh event registers", func(t *testing.T) {
		r := NewTabRegistry()
		r.SyncPageTargets(pageInfos(regIDB))
		if !r.RegisterEvent(r.NoteEventRead(regIDA, ""), regIDA) || !r.Has(regIDA) {
			t.Fatal("current event did not register its target")
		}
		if r.RegisterEvent(r.NoteEventRead("", ""), "") || r.Has("") {
			t.Fatal("empty id registered")
		}
	})
	t.Run("queued across tab_close", func(t *testing.T) {
		r := NewTabRegistry()
		r.SyncPageTargets(pageInfos(regIDA, regIDB))
		stamp := r.NoteEventRead(regIDA, "") // targetInfoChanged(A) read, then queued
		r.RemoveTarget(regIDA)               // Target.closeTarget(A) confirmed
		if r.RegisterEvent(stamp, regIDA) || r.Has(regIDA) {
			t.Fatal("queued event revived a closed tab")
		}
		if !r.RegisterEvent(stamp, regIDB) {
			t.Fatal("registered target not reported")
		}
	})
	t.Run("queued across a resync", func(t *testing.T) {
		r := NewTabRegistry()
		r.SyncPageTargets(pageInfos(regIDA, regIDB))
		stamp := r.NoteEventRead(regIDA, "")
		w := r.OpenWindow() // requested after the event was read
		if _, removed, _ := r.ApplyEnumeration(w, pageInfos(regIDB)); len(removed) != 1 {
			t.Fatalf("removed = %v", removed)
		}
		if r.RegisterEvent(stamp, regIDA) || r.Has(regIDA) {
			t.Fatal("queued event revived a target the resync found gone")
		}
		// An event read after the resync is newer than it.
		if !r.RegisterEvent(r.NoteEventRead(regIDX, ""), regIDX) {
			t.Fatal("event newer than the resync refused")
		}
	})
	t.Run("zero stamp is older than any evidence", func(t *testing.T) {
		r := NewTabRegistry()
		if !r.RegisterEvent(0, regIDA) {
			t.Fatal("zero stamp refused although nothing was ever applied")
		}
		r.SyncPageTargets(pageInfos(regIDA))
		if r.RegisterEvent(0, regIDX) {
			t.Fatal("unstamped event registered after an enumeration")
		}
	})
	t.Run("removals reported by events are ordered by the queue", func(t *testing.T) {
		r := NewTabRegistry()
		r.SyncPageTargets(pageInfos(regIDB))
		stamp := r.NoteEventRead(regIDA, "") // targetCreated(A) queued behind unrelated events
		r.RemoveEventTarget("WORKER-1")
		r.RemoveEventTarget(regIDX)
		if !r.RegisterEvent(stamp, regIDA) {
			t.Fatal("unrelated queued removals invalidated a page event")
		}
		// ...and still shield requests in flight.
		w := r.OpenWindow()
		r.RemoveEventTarget(regIDA) // targetDestroyed(A)
		if snaps, _, _ := r.ApplyEnumeration(w, pageInfos(regIDA, regIDB)); len(snaps) != 1 || r.Has(regIDA) {
			t.Fatalf("destroyed target revived by a reply in flight: %v", snapshotIDs(snaps))
		}
	})
}
