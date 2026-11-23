package state

import (
	"sync"
	"testing"
)

// Regressions for the ordering of a Target.createTarget reply against removals
// and enumerations (CommitCreated).

func TestCommitCreatedOrdersReplyAgainstRemoval(t *testing.T) {
	t.Run("no removal", func(t *testing.T) {
		r := NewTabRegistry()
		w := r.OpenWindow()
		if !commitOK(r, w, regIDA) || !r.Has(regIDA) {
			t.Fatalf("registered = %v", r.Has(regIDA))
		}
	})
	t.Run("listed, closed and destroyed before the reply", func(t *testing.T) {
		r := NewTabRegistry()
		r.SyncPageTargets(pageInfos(regIDB))
		w := r.OpenWindow()
		r.ApplyEnumeration(r.OpenWindow(), pageInfos(regIDA, regIDB)) // another caller lists A
		if !r.RemoveTarget(regIDA) {                                  // tab_close confirmed
			t.Fatal("precondition: A was registered by the list")
		}
		r.RemoveEventTarget(regIDA) // targetDestroyed
		if commitOK(r, w, regIDA) || r.Has(regIDA) {
			t.Fatal("the late create reply revived A")
		}
		if !r.Has(regIDB) {
			t.Fatal("B lost")
		}
	})
	t.Run("destroyed before it was ever registered", func(t *testing.T) {
		r := NewTabRegistry()
		w := r.OpenWindow()
		r.RemoveEventTarget(regIDA)
		if commitOK(r, w, regIDA) || r.Has(regIDA) {
			t.Fatal("destroyed target registered")
		}
	})
	t.Run("removed by an enumeration", func(t *testing.T) {
		r := NewTabRegistry()
		w := r.OpenWindow()
		r.RegisterPageTarget(regIDA) // targetCreated event
		if _, removed := r.SyncPageTargetsDetailed(nil); len(removed) != 1 {
			t.Fatalf("removed = %v", removed)
		}
		if commitOK(r, w, regIDA) || r.Has(regIDA) {
			t.Fatal("target dropped by an enumeration registered")
		}
	})
	t.Run("enumeration without the new target is not a removal", func(t *testing.T) {
		r := NewTabRegistry()
		w := r.OpenWindow()
		r.ApplyEnumeration(r.OpenWindow(), pageInfos(regIDB)) // may predate the creation
		if !commitOK(r, w, regIDA) || !r.Has(regIDA) {
			t.Fatal("creation refused because an overlapping enumeration did not list it")
		}
	})
	t.Run("removal older than the request does not count", func(t *testing.T) {
		r := NewTabRegistry()
		r.RemoveTarget(regIDA)
		w := r.OpenWindow()
		if !commitOK(r, w, regIDA) {
			t.Fatal("creation refused for a removal that predates its request")
		}
	})
	t.Run("empty id", func(t *testing.T) {
		r := NewTabRegistry()
		if commitOK(r, r.OpenWindow(), "") || r.Has("") {
			t.Fatal("empty id registered")
		}
		if r.OpenWindows() != 0 {
			t.Fatal("window not released")
		}
	})
}

// Review round 4, schedule 2: the create reply is pending, tab_close(A)
// succeeds, and a current enumeration lists the lingering A again before the
// reply is committed. The removal happened after the creation was requested,
// so the reply is refused although A is registered at that moment.
func TestCommitCreatedRefusedAfterRemovalEvenIfRegisteredAgain(t *testing.T) {
	r := NewTabRegistry()
	r.SyncPageTargets(pageInfos(regIDB))
	create := r.OpenWindow()
	r.ApplyEnumeration(r.OpenWindow(), pageInfos(regIDA, regIDB)) // resolves A for tab_close
	if !r.RemoveTarget(regIDA) {
		t.Fatal("precondition: tab_close removed A")
	}
	// Requested after the close; the browser still lists the closing target.
	if snaps, _, _ := r.ApplyEnumeration(r.OpenWindow(), pageInfos(regIDA, regIDB)); len(snaps) != 2 || !r.Has(regIDA) {
		t.Fatalf("precondition: lingering A listed by a current enumeration: %v", snapshotIDs(snaps))
	}
	if commitOK(r, create, regIDA) {
		t.Fatal("creation committed although the tab was closed after it was requested")
	}
	// The refusal does not touch what the current enumeration registered; the
	// next enumeration without A removes it.
	if _, removed, _ := r.ApplyEnumeration(r.OpenWindow(), pageInfos(regIDB)); len(removed) != 1 || r.Has(regIDA) {
		t.Fatalf("removed = %v, want A", removed)
	}
}

// Review round 4, schedule 1: the create reply is pending, a list registers A,
// the user closes A, a newer list (only B) is requested and held, the create
// reply is committed, then the list is applied. The commit must not make A's
// registration newer than that list's request, or its absence would be masked.
func TestCommitCreatedDoesNotRenewRegistrationAndMaskAbsence(t *testing.T) {
	r := NewTabRegistry()
	r.SyncPageTargets(pageInfos(regIDB))
	create := r.OpenWindow()
	r.ApplyEnumeration(r.OpenWindow(), pageInfos(regIDA, regIDB)) // A exists: registered
	onlyB := r.OpenWindow()                                       // requested after A was closed in the browser
	if !commitOK(r, create, regIDA) || !r.Has(regIDA) {
		t.Fatal("precondition: nothing reported A gone yet, the creation commits")
	}
	snaps, removed, _ := r.ApplyEnumeration(onlyB, pageInfos(regIDB))
	if len(removed) != 1 || removed[0].TargetID != regIDA || r.Has(regIDA) {
		t.Fatalf("late create commit masked the absence of A: snaps=%v removed=%v", snapshotIDs(snaps), removed)
	}
	if _, ok := r.Lookup(string(regIDA)); ok {
		t.Fatal("A still resolves")
	}
	if !r.Has(regIDB) {
		t.Fatal("B lost")
	}
}

// A creation committed first is still protected from enumerations requested
// before the commit: they may predate the creation.
func TestCommitCreatedProtectsNewTargetFromOverlappingEnumeration(t *testing.T) {
	r := NewTabRegistry()
	r.SyncPageTargets(pageInfos(regIDB))
	stale := r.OpenWindow()
	create := r.OpenWindow()
	if !commitOK(r, create, regIDA) {
		t.Fatal("creation refused")
	}
	if _, removed, _ := r.ApplyEnumeration(stale, pageInfos(regIDB)); len(removed) != 0 || !r.Has(regIDA) {
		t.Fatalf("new tab removed by an overlapping enumeration: %v", removed)
	}
	if _, removed, _ := r.ApplyEnumeration(r.OpenWindow(), pageInfos(regIDB)); len(removed) != 1 {
		t.Fatalf("enumeration requested after the commit removed %v, want A", removed)
	}
}

// Whatever the interleaving of the commit with the removal of the same target,
// a removal reported while the creation was in flight wins.
func TestConcurrentCommitCreatedAndRemoval(t *testing.T) {
	r := NewTabRegistry()
	r.RegisterPageTarget(regIDB)
	for i := 0; i < 2000; i++ {
		w := r.OpenWindow()
		list := r.OpenWindow()
		var wg sync.WaitGroup
		wg.Add(3)
		var committed bool
		go func() { defer wg.Done(); committed = commitOK(r, w, regIDA) }()
		go func() { defer wg.Done(); r.ApplyEnumeration(list, pageInfos(regIDA, regIDB)) }()
		go func() { defer wg.Done(); r.RemoveTarget(regIDA) }()
		wg.Wait()
		_ = committed // true only when the commit ran before the removal
		if r.Has(regIDA) {
			t.Fatalf("iteration %d: removed target is registered", i)
		}
		if r.OpenWindows() != 0 || !r.Has(regIDB) {
			t.Fatalf("iteration %d: windows=%d B=%v", i, r.OpenWindows(), r.Has(regIDB))
		}
	}
}
