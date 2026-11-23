package state

import (
	"fmt"
	"testing"

	"github.com/chromedp/cdproto/target"
)

// Regressions for the ordering of a Target.createTarget reply against removals
// (RegisterCreatedTarget) and for the bound on the applied-enumeration memory.

func TestRegisterCreatedTargetOrdersReplyAgainstRemoval(t *testing.T) {
	t.Run("no removal", func(t *testing.T) {
		r := NewTabRegistry()
		tok := r.BeginEnumeration()
		if got := r.RegisterCreatedTarget(tok, regIDA); got != CreatedRegistered || !r.Has(regIDA) {
			t.Fatalf("outcome = %v, registered = %v", got, r.Has(regIDA))
		}
	})
	t.Run("listed, closed and destroyed before the reply", func(t *testing.T) {
		r := NewTabRegistry()
		r.SyncPageTargets(pageInfos(regIDB))
		tok := r.BeginEnumeration()
		r.ApplyEnumeration(r.BeginEnumeration(), pageInfos(regIDA, regIDB)) // another caller lists A
		if !r.RemoveTarget(regIDA) {                                        // tab_close confirmed
			t.Fatal("precondition: A was registered by the list")
		}
		r.RemoveTarget(regIDA) // targetDestroyed
		if got := r.RegisterCreatedTarget(tok, regIDA); got != CreatedRemoved || r.Has(regIDA) {
			t.Fatalf("outcome = %v, registered = %v; the late create reply revived A", got, r.Has(regIDA))
		}
		if !r.Has(regIDB) {
			t.Fatal("B lost")
		}
	})
	t.Run("destroyed before it was ever registered", func(t *testing.T) {
		r := NewTabRegistry()
		tok := r.BeginEnumeration()
		r.RemoveTarget(regIDA)
		if got := r.RegisterCreatedTarget(tok, regIDA); got != CreatedRemoved || r.Has(regIDA) {
			t.Fatalf("outcome = %v, registered = %v", got, r.Has(regIDA))
		}
	})
	t.Run("removed by an enumeration", func(t *testing.T) {
		r := NewTabRegistry()
		tok := r.BeginEnumeration()
		r.RegisterPageTarget(regIDA) // targetCreated event
		if _, removed := r.SyncPageTargetsDetailed(nil); len(removed) != 1 {
			t.Fatalf("removed = %v", removed)
		}
		if got := r.RegisterCreatedTarget(tok, regIDA); got != CreatedRemoved || r.Has(regIDA) {
			t.Fatalf("outcome = %v, registered = %v", got, r.Has(regIDA))
		}
	})
	t.Run("registered again when the reply arrives", func(t *testing.T) {
		r := NewTabRegistry()
		tok := r.BeginEnumeration()
		r.RegisterPageTarget(regIDA)
		r.RemoveTarget(regIDA)
		r.RegisterPageTarget(regIDA) // a current source says it exists
		stale := r.BeginEnumeration()
		if got := r.RegisterCreatedTarget(tok, regIDA); got != CreatedRegistered {
			t.Fatalf("outcome = %v", got)
		}
		// The registration is refreshed: an enumeration in flight cannot remove it.
		if _, removed := r.ApplyEnumeration(stale, nil); len(removed) != 0 || !r.Has(regIDA) {
			t.Fatalf("created tab removed by a reply requested before it was committed: %v", removed)
		}
	})
	t.Run("enumeration without the new target is not a removal", func(t *testing.T) {
		r := NewTabRegistry()
		tok := r.BeginEnumeration()
		r.ApplyEnumeration(r.BeginEnumeration(), pageInfos(regIDB)) // may predate the creation
		if got := r.RegisterCreatedTarget(tok, regIDA); got != CreatedRegistered || !r.Has(regIDA) {
			t.Fatalf("outcome = %v", got)
		}
	})
	t.Run("removal older than the request does not count", func(t *testing.T) {
		r := NewTabRegistry()
		r.RemoveTarget(regIDA)
		tok := r.BeginEnumeration()
		if got := r.RegisterCreatedTarget(tok, regIDA); got != CreatedRegistered {
			t.Fatalf("outcome = %v", got)
		}
	})
	t.Run("empty id", func(t *testing.T) {
		r := NewTabRegistry()
		if got := r.RegisterCreatedTarget(r.BeginEnumeration(), ""); got == CreatedRegistered || r.Has("") {
			t.Fatalf("outcome = %v", got)
		}
	})
}

// Once the removal of the created target may have been forgotten, the registry
// says so instead of registering it.
func TestRegisterCreatedTargetAfterRemovalOverflowIsUnverifiable(t *testing.T) {
	r := NewTabRegistry()
	tok := r.BeginEnumeration()
	r.RemoveTarget(regIDA)
	for i := 0; i < maxRemovedTargets; i++ {
		r.RemoveTarget(target.ID(fmt.Sprintf("gone-%d", i)))
	}
	if _, ok := r.removedGen[regIDA]; ok {
		t.Fatal("precondition: the removal of A must have been forgotten")
	}
	if got := r.RegisterCreatedTarget(tok, regIDA); got != CreatedUnverifiable || r.Has(regIDA) {
		t.Fatalf("outcome = %v, registered = %v", got, r.Has(regIDA))
	}
	// A request sent now is newer than everything forgotten.
	if got := r.RegisterCreatedTarget(r.BeginEnumeration(), regIDB); got != CreatedRegistered {
		t.Fatalf("fresh creation outcome = %v", got)
	}
}

func appliedID(i int) target.ID { return target.ID(fmt.Sprintf("DDDD%024X1234", i)) }

// Polling an unchanged browser costs one remembered snapshot, and any number of
// changing ones at most maxAppliedEnumerations.
func TestAppliedEnumerationMemoryIsBounded(t *testing.T) {
	r := NewTabRegistry()
	for i := 0; i < 5*maxAppliedEnumerations; i++ {
		r.ApplyEnumeration(r.BeginEnumeration(), pageInfos(regIDA, regIDB))
	}
	if n := len(r.appliedLog); n != 1 {
		t.Fatalf("identical enumerations hold %d entries, want 1", n)
	}
	if r.appliedFloor != 0 {
		t.Fatalf("appliedFloor = %d without any eviction", r.appliedFloor)
	}
	for i := 0; i < 5*maxAppliedEnumerations; i++ {
		r.ApplyEnumeration(r.BeginEnumeration(), pageInfos(regIDB, appliedID(i)))
	}
	if n := len(r.appliedLog); n != maxAppliedEnumerations {
		t.Fatalf("applied log holds %d entries, want %d", n, maxAppliedEnumerations)
	}
	if c := cap(r.appliedLog); c > 4*maxAppliedEnumerations {
		t.Fatalf("applied log capacity grew to %d", c)
	}
	for i := 1; i < len(r.appliedLog); i++ {
		if r.appliedLog[i-1].gen >= r.appliedLog[i].gen {
			t.Fatalf("applied log out of generation order at %d", i)
		}
	}
	if r.appliedFloor == 0 || r.appliedFloor >= r.appliedLog[0].gen {
		t.Fatalf("appliedFloor = %d, oldest kept = %d", r.appliedFloor, r.appliedLog[0].gen)
	}
	// Bounded by the listed targets too: nothing but the last snapshot's ids
	// stays registered.
	if n := len(r.targetGen); n != 2 {
		t.Fatalf("%d targets registered, want 2", n)
	}
}

// A reply overtaken by more target-set changes than are remembered cannot be
// checked against the forgotten ones: it registers nothing it cannot vouch
// for, keeps returning what is registered, and a current enumeration recovers.
func TestAppliedEnumerationOverflowIsConservative(t *testing.T) {
	r := NewTabRegistry()
	r.SyncPageTargets(pageInfos(regIDB))

	// All requested before the enumeration that finds X absent.
	old := r.BeginEnumeration()
	r.ApplyEnumeration(r.BeginEnumeration(), pageInfos(regIDB)) // X absent
	// Overtaken replies that all list X (and a different extra id each): each
	// is refused, and together they push the absence out of the memory.
	for i := 0; i <= maxAppliedEnumerations; i++ {
		if snaps, _ := r.ApplyEnumeration(old, pageInfos(regIDX, regIDB, appliedID(i))); len(snaps) != 1 {
			t.Fatalf("overtaken reply %d registered %v", i, snapshotIDs(snaps))
		}
	}
	if r.appliedFloor <= old.gen {
		t.Fatal("precondition: the enumeration that found X absent must have been forgotten")
	}
	for _, e := range r.appliedLog {
		if _, ok := e.present[regIDX]; !ok {
			t.Fatal("precondition: every remembered enumeration must list X")
		}
	}
	snaps, removed := r.ApplyEnumeration(old, pageInfos(regIDX, regIDB))
	if r.Has(regIDX) || len(snaps) != 1 || !hasSnapshot(snaps, regIDB) || len(removed) != 0 {
		t.Fatalf("unverifiable reply: snaps=%v removed=%v", snapshotIDs(snaps), removed)
	}
	if snaps, _ := r.ApplyEnumeration(r.BeginEnumeration(), pageInfos(regIDX, regIDB)); !r.Has(regIDX) || len(snaps) != 2 {
		t.Fatalf("current enumeration after overflow = %v, want X and B", snapshotIDs(snaps))
	}
}
