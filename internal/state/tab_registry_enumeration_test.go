package state

import (
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
