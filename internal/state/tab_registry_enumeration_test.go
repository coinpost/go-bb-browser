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

func TestApplyEnumerationKeepsTargetsRegisteredAfterToken(t *testing.T) {
	r := NewTabRegistry()
	r.SyncPageTargets(pageInfos("AAAA0001", "AAAA0002"))

	tok := r.BeginEnumeration()
	// Browser snapshot taken here: AAAA0002 is already gone, BBBB0003 not yet created.
	stale := pageInfos("AAAA0001")
	const newID = target.ID("BBBB0003")
	r.RegisterPageTarget(newID)
	if !r.Select(newID) {
		t.Fatal("select failed")
	}

	snaps, removed := r.ApplyEnumeration(tok, stale)
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
	_, removed = r.ApplyEnumeration(r.BeginEnumeration(), stale)
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

func TestApplyEnumerationRespectsExplicitReRegistration(t *testing.T) {
	r := NewTabRegistry()
	r.SyncPageTargets(pageInfos("AAAA0001"))

	tok := r.BeginEnumeration()
	r.RegisterPageTarget("AAAA0001")
	if _, removed := r.ApplyEnumeration(tok, nil); len(removed) != 0 {
		t.Fatalf("removed = %#v, want none: target was re-registered after the token", removed)
	}
	if _, ok := r.Lookup("AAAA0001"); !ok {
		t.Fatal("explicitly re-registered target was removed by an older enumeration")
	}
}

func TestApplyEnumerationProtectsTargetsAddedByNewerEnumeration(t *testing.T) {
	r := NewTabRegistry()
	older := r.BeginEnumeration()
	newer := r.BeginEnumeration()

	r.ApplyEnumeration(newer, pageInfos("AAAA0001"))
	if _, removed := r.ApplyEnumeration(older, nil); len(removed) != 0 {
		t.Fatalf("removed = %#v, want none", removed)
	}
	if !r.Has("AAAA0001") {
		t.Fatal("older empty snapshot removed a target registered by a newer one")
	}
	// Merely being listed again does not renew protection: the next
	// enumeration requested afterwards still removes it.
	tok := r.BeginEnumeration()
	r.ApplyEnumeration(r.BeginEnumeration(), pageInfos("AAAA0001"))
	if _, removed := r.ApplyEnumeration(tok, nil); len(removed) != 1 {
		t.Fatalf("removed = %#v, want AAAA0001", removed)
	}
}

func TestApplyEnumerationZeroTokenNeverRemoves(t *testing.T) {
	r := NewTabRegistry()
	r.RegisterPageTarget("AAAA0001")
	snaps, removed := r.ApplyEnumeration(EnumerationToken{}, pageInfos("AAAA0002"))
	if len(removed) != 0 {
		t.Fatalf("removed = %#v, want none", removed)
	}
	if len(snaps) != 1 || snaps[0].TargetID != "AAAA0002" {
		t.Fatalf("snapshots = %#v", snaps)
	}
	for _, id := range []target.ID{"AAAA0001", "AAAA0002"} {
		if !r.Has(id) {
			t.Fatalf("%s not registered", id)
		}
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

	tok := r.BeginEnumeration()
	stale := pageInfos(regIDA, regIDB) // browser snapshot taken while A still existed
	if !r.RemoveTarget(regIDA) {
		t.Fatal("precondition: A not removed")
	}

	snaps, removed := r.ApplyEnumeration(tok, stale)
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
	// Applying the same stale reply again changes nothing either.
	if snaps, _ := r.ApplyEnumeration(tok, stale); len(snaps) != 1 || r.Has(regIDA) {
		t.Fatalf("second stale apply revived A: %v", snapshotIDs(snaps))
	}

	// An enumeration requested after the removal is current for A: if the
	// browser lists that id then, it is a live target.
	snaps, _ = r.ApplyEnumeration(r.BeginEnumeration(), stale)
	if len(snaps) != 2 || !r.Has(regIDA) {
		t.Fatalf("current enumeration did not register A: %v", snapshotIDs(snaps))
	}
}

func TestApplyEnumerationRemovalByNewerEnumerationWins(t *testing.T) {
	r := NewTabRegistry()
	r.SyncPageTargets(pageInfos(regIDA, regIDB))

	older := r.BeginEnumeration() // snapshot: A and B
	newer := r.BeginEnumeration() // snapshot: only B
	if _, removed := r.ApplyEnumeration(newer, pageInfos(regIDB)); len(removed) != 1 || removed[0].TargetID != regIDA {
		t.Fatalf("removed = %#v, want A", removed)
	}
	snaps, removed := r.ApplyEnumeration(older, pageInfos(regIDA, regIDB))
	if len(removed) != 0 || len(snaps) != 1 || snaps[0].TargetID != regIDB || r.Has(regIDA) {
		t.Fatalf("older reply revived A: snaps=%v removed=%#v", snapshotIDs(snaps), removed)
	}
}

func TestApplyEnumerationKeepsTargetReRegisteredAfterRemoval(t *testing.T) {
	r := NewTabRegistry()
	r.SyncPageTargets(pageInfos(regIDA))

	tok := r.BeginEnumeration()
	r.RemoveTarget(regIDA)
	r.RegisterPageTarget(regIDA) // announced again: live, newer than the token

	for _, infos := range [][]*target.Info{pageInfos(regIDA), nil} {
		snaps, removed := r.ApplyEnumeration(tok, infos)
		if len(removed) != 0 || !r.Has(regIDA) {
			t.Fatalf("re-registered target removed by an older enumeration (infos=%d): %#v", len(infos), removed)
		}
		if len(snaps) != len(infos) {
			t.Fatalf("snapshots = %v for %d listed targets", snapshotIDs(snaps), len(infos))
		}
	}
	// And a removal after that re-registration is honoured again.
	tok = r.BeginEnumeration()
	r.RemoveTarget(regIDA)
	if snaps, _ := r.ApplyEnumeration(tok, pageInfos(regIDA)); len(snaps) != 0 || r.Has(regIDA) {
		t.Fatal("second removal not honoured")
	}
}

// A target can be reported gone before the registry ever saw it (its lifecycle
// event overtakes the first enumeration that lists it).
func TestApplyEnumerationSkipsTargetReportedGoneBeforeRegistration(t *testing.T) {
	r := NewTabRegistry()
	tok := r.BeginEnumeration()
	if r.RemoveTarget(regIDA) {
		t.Fatal("unregistered target reported as removed")
	}
	snaps, _ := r.ApplyEnumeration(tok, pageInfos(regIDA, regIDB))
	if len(snaps) != 1 || snaps[0].TargetID != regIDB || r.Has(regIDA) {
		t.Fatalf("snapshots = %v, want only B", snapshotIDs(snaps))
	}
	// Repeated reports for an id that stays unregistered take one slot.
	before := len(r.removedLog)
	for i := 0; i < 10; i++ {
		r.RemoveTarget(regIDA)
		r.RemoveTarget("")
	}
	if len(r.removedLog) != before {
		t.Fatalf("removal memory grew from %d to %d", before, len(r.removedLog))
	}
}

func TestRemovalMemoryIsBounded(t *testing.T) {
	r := NewTabRegistry()
	r.RegisterPageTarget(regIDB)
	ancient := r.BeginEnumeration()

	id := func(i int) target.ID { return target.ID(fmt.Sprintf("CCCC%024X1234", i)) }
	const churn = 3*maxRemovedTargets + 7
	for i := 0; i < churn; i++ {
		r.RegisterPageTarget(id(i))
		if !r.RemoveTarget(id(i)) {
			t.Fatalf("remove %d failed", i)
		}
		// Removing, re-registering and removing the same id again must not
		// leave a stale entry behind when the older one is forgotten.
		if i%5 == 0 {
			r.RegisterPageTarget(id(i))
			r.RemoveTarget(id(i))
		}
	}
	if n := len(r.removedLog); n != maxRemovedTargets {
		t.Fatalf("removal log holds %d entries, want %d", n, maxRemovedTargets)
	}
	if n := len(r.removedGen); n > maxRemovedTargets {
		t.Fatalf("removal index holds %d entries, want at most %d", n, maxRemovedTargets)
	}
	if c := cap(r.removedLog); c > 4*maxRemovedTargets {
		t.Fatalf("removal log capacity grew to %d", c)
	}
	for _, e := range r.removedLog {
		if g := r.removedGen[e.id]; g < e.gen {
			t.Fatalf("index lost %s: has %d, log has %d", e.id, g, e.gen)
		}
	}
	if len(r.targetGen) != 1 {
		t.Fatalf("registered = %d, want only B", len(r.targetGen))
	}

	// An enumeration older than the forgotten removals cannot tell which ids
	// were removed since, so it registers none: neither a forgotten removed id
	// nor one it has never seen. Registered targets are still reported.
	never := target.ID("DDDD0000000000000000000000AB1234")
	snaps, removed := r.ApplyEnumeration(ancient, pageInfos(id(0), id(churn-1), never, regIDB))
	if ids := snapshotIDs(snaps); len(ids) != 1 || ids[0] != regIDB {
		t.Fatalf("ancient enumeration snapshots = %v, want only B", ids)
	}
	if len(removed) != 0 || r.Has(id(0)) || r.Has(id(churn-1)) || r.Has(never) {
		t.Fatal("ancient enumeration registered a target it cannot vouch for")
	}
	// A token inside the remembered window is still exact: only the id removed
	// after it is filtered.
	recent := r.BeginEnumeration()
	r.RegisterPageTarget(id(churn))
	r.RemoveTarget(id(churn))
	snaps, _ = r.ApplyEnumeration(recent, pageInfos(id(churn), never, regIDB))
	if ids := snapshotIDs(snaps); len(ids) != 2 || r.Has(id(churn)) || !r.Has(never) {
		t.Fatalf("recent enumeration snapshots = %v, want B and the never-removed target", ids)
	}
	// A current enumeration is authoritative for everything it lists.
	snaps, _ = r.ApplyEnumeration(r.BeginEnumeration(), pageInfos(id(0), id(churn), never, regIDB))
	if len(snaps) != 4 {
		t.Fatalf("current enumeration snapshots = %v", snapshotIDs(snaps))
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
		tok := r.BeginEnumeration()
		stale := pageInfos(gone, regIDB)

		var wg sync.WaitGroup
		wg.Add(4)
		go func() { defer wg.Done(); r.RemoveTarget(gone) }()
		go func() { defer wg.Done(); r.ApplyEnumeration(tok, stale) }()
		go func() { defer wg.Done(); r.ApplyEnumeration(tok, stale) }()
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
