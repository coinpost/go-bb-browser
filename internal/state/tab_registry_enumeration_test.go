package state

import (
	"testing"

	"github.com/chromedp/cdproto/target"
)

func (r *TabRegistry) removalMarkers() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.removedGen)
}

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
	newShort := r.RegisterPageTarget("BBBB0003")
	if !r.Select(newShort) {
		t.Fatal("select failed")
	}

	snaps, removed := r.ApplyEnumeration(tok, stale)
	if len(snaps) != 1 || snaps[0].TargetID != "AAAA0001" {
		t.Fatalf("snapshots = %#v, want only the enumerated target", snaps)
	}
	if len(removed) != 1 || removed[0].TargetID != "AAAA0002" {
		t.Fatalf("removed = %#v, want only AAAA0002", removed)
	}
	if id, ok := r.Lookup(newShort); !ok || id != "BBBB0003" {
		t.Fatalf("target registered after the token was removed (%q, %v)", id, ok)
	}
	if r.Selected() != newShort {
		t.Fatalf("selection = %q, want %q", r.Selected(), newShort)
	}

	// An enumeration requested after the registration is authoritative for it.
	_, removed = r.ApplyEnumeration(r.BeginEnumeration(), stale)
	if len(removed) != 1 || removed[0].TargetID != "BBBB0003" || removed[0].ShortID != newShort {
		t.Fatalf("removed = %#v, want BBBB0003", removed)
	}
	if _, ok := r.Lookup(newShort); ok {
		t.Fatal("fresh enumeration kept a vanished target")
	}
	if r.Selected() != "" {
		t.Fatalf("selection = %q, want cleared", r.Selected())
	}
}

func TestApplyEnumerationRespectsExplicitReRegistration(t *testing.T) {
	r := NewTabRegistry()
	r.SyncPageTargets(pageInfos("AAAA0001"))
	short, _ := r.ShortForTarget("AAAA0001")

	tok := r.BeginEnumeration()
	if got := r.RegisterPageTarget("AAAA0001"); got != short {
		t.Fatalf("re-registration changed the short id: %q -> %q", short, got)
	}
	if _, removed := r.ApplyEnumeration(tok, nil); len(removed) != 0 {
		t.Fatalf("removed = %#v, want none: target was re-registered after the token", removed)
	}
	if _, ok := r.Lookup(short); !ok {
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
	if _, ok := r.ShortForTarget("AAAA0001"); !ok {
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
		if _, ok := r.ShortForTarget(id); !ok {
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

// A snapshot requested before a target was removed must not register that
// target again; one requested afterwards may.
func TestApplyEnumerationDoesNotReRegisterTargetRemovedAfterToken(t *testing.T) {
	r := NewTabRegistry()
	r.SyncPageTargets(pageInfos("AAAA0001", "AAAA0002"))
	short1, _ := r.ShortForTarget("AAAA0001")

	tok := r.BeginEnumeration()
	stale := pageInfos("AAAA0001", "AAAA0002")
	if _, ok := r.RemoveTarget("AAAA0001"); !ok {
		t.Fatal("precondition: target not removed")
	}

	snaps, removed := r.ApplyEnumeration(tok, stale)
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want none", removed)
	}
	if len(snaps) != 1 || snaps[0].TargetID != "AAAA0002" {
		t.Fatalf("snapshots = %+v, want only AAAA0002", snaps)
	}
	if _, ok := r.ShortForTarget("AAAA0001"); ok {
		t.Fatal("stale snapshot registered the removed target again")
	}
	if _, ok := r.Lookup(short1); ok {
		t.Fatal("stale snapshot gave the removed target its alias back")
	}

	// The enumeration is finished, so its removal marker is no longer needed.
	if n := r.removalMarkers(); n != 0 {
		t.Fatalf("%d removal markers outlive every enumeration", n)
	}
	// A snapshot requested after the removal is newer evidence.
	snaps, _ = r.ApplyEnumeration(r.BeginEnumeration(), stale)
	if len(snaps) != 2 {
		t.Fatalf("newer snapshot = %+v, want both targets", snaps)
	}
}

func TestApplyEnumerationRemovalByNewerSnapshotBlocksOlderOne(t *testing.T) {
	r := NewTabRegistry()
	r.SyncPageTargets(pageInfos("AAAA0001"))
	older := r.BeginEnumeration()
	newer := r.BeginEnumeration()
	if _, removed := r.ApplyEnumeration(newer, nil); len(removed) != 1 {
		t.Fatalf("newer snapshot removed %v, want AAAA0001", removed)
	}
	if snaps, _ := r.ApplyEnumeration(older, pageInfos("AAAA0001")); len(snaps) != 0 {
		t.Fatalf("older snapshot restored %+v", snaps)
	}
	if n := r.removalMarkers(); n != 0 {
		t.Fatalf("%d removal markers outlive every enumeration", n)
	}
}

// Removal markers exist only for enumerations still in flight: none without
// one, and an abandoned (failed) enumeration releases its own.
func TestRemovalMarkersAreBoundedByOpenEnumerations(t *testing.T) {
	r := NewTabRegistry()
	for i := range 100 {
		id := target.ID("BBBB" + string(rune('0'+i%10)) + string(rune('0'+i/10)))
		r.RegisterPageTarget(id)
		r.RemoveTarget(id)
	}
	if n := r.removalMarkers(); n != 0 {
		t.Fatalf("%d removal markers without an enumeration in flight", n)
	}

	older := r.BeginEnumeration()
	r.RegisterPageTarget("AAAA0001")
	r.RemoveTarget("AAAA0001")
	newer := r.BeginEnumeration()
	r.RegisterPageTarget("AAAA0002")
	r.RemoveTarget("AAAA0002")
	if n := r.removalMarkers(); n != 2 {
		t.Fatalf("removal markers = %d, want 2", n)
	}
	r.AbandonEnumeration(older)
	if n := r.removalMarkers(); n != 1 {
		t.Fatalf("removal markers after the older enumeration ended = %d, want 1", n)
	}
	if snaps, _ := r.ApplyEnumeration(newer, pageInfos("AAAA0001", "AAAA0002")); len(snaps) != 1 || snaps[0].TargetID != "AAAA0001" {
		// AAAA0001 was removed before `newer` was requested, AAAA0002 after.
		t.Fatalf("snapshots = %+v, want only AAAA0001", snaps)
	}
	if n := r.removalMarkers(); n != 0 {
		t.Fatalf("removal markers after every enumeration ended = %d, want 0", n)
	}
	// Ending a token twice, or one that was never begun, changes nothing.
	r.AbandonEnumeration(newer)
	r.AbandonEnumeration(EnumerationToken{})
	if n := r.PendingEnumerations(); n != 0 {
		t.Fatalf("pending enumerations = %d, want 0", n)
	}
}
