package state

import (
	"sync"
	"testing"
)

// Regressions for evidence that does not travel through the lifecycle event
// queue in order: destroyed notifications recorded when they are read
// (NoteDestroyedRead), also when a full queue drops them, and enumeration
// replies that contradict each other.

// The queue is full: the created and destroyed notifications of A are both
// dropped, the resync that follows never lists A, and only then does the
// creator see its Target.createTarget reply. The registry never held A, so
// nothing but the notification read from the connection says that it is gone.
func TestDestroyedReadConstrainsCreationInFlight(t *testing.T) {
	for _, queued := range []bool{false, true} {
		r := NewTabRegistry()
		r.SyncPageTargets(pageInfos(regIDB))
		create := r.OpenWindow()
		r.NoteDestroyedRead(regIDA, queued)
		r.ApplyEnumeration(r.OpenWindow(), pageInfos(regIDB)) // overflow resync: B only
		if r.CommitCreated(create, regIDA) || r.Has(regIDA) {
			t.Fatalf("queued=%v: late creation reply registered a destroyed target", queued)
		}
		if !r.Has(regIDB) || r.OpenWindows() != 0 {
			t.Fatalf("queued=%v: B=%v windows=%d", queued, r.Has(regIDB), r.OpenWindows())
		}
	}
}

// A creation is only constrained by notifications read while it was in flight.
func TestDestroyedReadBeforeWindowIsNotRemembered(t *testing.T) {
	r := NewTabRegistry()
	r.NoteDestroyedRead(regIDX, false)
	r.NoteDestroyedRead(regIDA, true)
	if r.OpenWindows() != 0 {
		t.Fatal("a notification read with no request in flight left a window behind")
	}
	// Normal concurrent creation: an earlier empty snapshot and the destruction
	// of other targets do not touch it.
	stale := r.OpenWindow()
	create := r.OpenWindow()
	r.NoteDestroyedRead(regIDX, false)
	if !r.CommitCreated(create, regIDB) {
		t.Fatal("creation refused because of an unrelated destroyed notification")
	}
	if _, removed := r.ApplyEnumeration(stale, nil); len(removed) != 0 || !r.Has(regIDB) {
		t.Fatalf("new tab removed by an earlier empty snapshot: %v", removed)
	}
}

// An enumeration whose snapshot still lists A is in flight when A's destroyed
// notification is read: its reply cannot register A, however late the
// notification is handled and even if it is never handled at all.
func TestDestroyedReadConstrainsEnumerationInFlight(t *testing.T) {
	for _, queued := range []bool{false, true} {
		r := NewTabRegistry()
		r.SyncPageTargets(pageInfos(regIDB))
		list := r.OpenWindow()
		r.NoteDestroyedRead(regIDA, queued)
		tabs, removed := r.ApplyEnumeration(list, pageInfos(regIDA, regIDB))
		if hasSnapshot(tabs, regIDA) || r.Has(regIDA) || len(removed) != 0 {
			t.Fatalf("queued=%v: tabs=%v removed=%v, A must not be registered", queued, tabs, removed)
		}
		if !hasSnapshot(tabs, regIDB) {
			t.Fatalf("queued=%v: B missing from %v", queued, tabs)
		}
	}
}

// Reading the notification does not unregister: that is done, together with the
// daemon state of the tab, by whoever applies it — the queue consumer, or for a
// dropped notification the resync requested after it.
func TestDestroyedReadLeavesRegistrationToTheConsumer(t *testing.T) {
	r := NewTabRegistry()
	r.SyncPageTargets(pageInfos(regIDA, regIDB))
	r.Select(regIDA)
	r.NoteDestroyedRead(regIDA, false)
	if !r.Has(regIDA) || r.Selected() != regIDA {
		t.Fatal("the reader unregistered a target")
	}
	_, removed := r.ApplyEnumeration(r.OpenWindow(), pageInfos(regIDB))
	if len(removed) != 1 || removed[0].TargetID != regIDA || removed[0].Contested || r.Has(regIDA) {
		t.Fatalf("removed = %#v, want A removed by the resync", removed)
	}
}

// Events still queued when a destroyed notification is dropped were read
// before it. One of them must not register the destroyed target while the
// resync is pending; with the notification queued, the queue itself orders
// them and nothing is refused.
func TestDroppedDestroyedOutdatesQueuedExistenceEvents(t *testing.T) {
	r := NewTabRegistry()
	stamp := r.EventStamp() // targetInfoChanged(A) read here
	r.NoteDestroyedRead(regIDA, false)
	if r.RegisterEvent(stamp, regIDA) || r.Has(regIDA) {
		t.Fatal("an event read before a dropped destroyed notification registered the target")
	}
	// An event read afterwards is newer evidence.
	if !r.RegisterEvent(r.EventStamp(), regIDB) {
		t.Fatal("an event read after the drop was refused")
	}

	q := NewTabRegistry()
	stamp = q.EventStamp()
	q.NoteDestroyedRead(regIDA, true)
	if !q.RegisterEvent(stamp, regIDA) {
		t.Fatal("a queued notification must leave ordering to the queue")
	}
	q.RemoveEventTarget(regIDA) // the destroyed event, next in the queue
	if q.Has(regIDA) {
		t.Fatal("destroyed event did not remove the target")
	}
}

// The schedule without any creation: an ordinary list snapshots A, A is
// destroyed (notification lost), the overflow resync snapshots B only. The
// list is applied first, the resync second. Both were in flight together, so
// nothing orders them; the listing alone must not shield A from the resync.
func TestDiscoveryDoesNotSurviveContradictingEnumeration(t *testing.T) {
	r := NewTabRegistry()
	r.SyncPageTargets(pageInfos(regIDB))
	list := r.OpenWindow()
	resync := r.OpenWindow()
	if tabs, _ := r.ApplyEnumeration(list, pageInfos(regIDA, regIDB)); !hasSnapshot(tabs, regIDA) {
		t.Fatalf("precondition: the list introduces A, got %v", tabs)
	}
	r.Select(regIDA)
	tabs, removed := r.ApplyEnumeration(resync, pageInfos(regIDB))
	if len(removed) != 1 || removed[0].TargetID != regIDA || !removed[0].Contested {
		t.Fatalf("removed = %#v, want A as contested", removed)
	}
	if r.Has(regIDA) || r.Selected() == regIDA || hasSnapshot(tabs, regIDA) {
		t.Fatal("A still resolves after the contradicting resync")
	}
	if !r.Has(regIDB) {
		t.Fatal("B lost")
	}
	// A third reply that was in flight across the conflict cannot bring A back
	// either; only an enumeration requested afterwards can.
	r2 := NewTabRegistry()
	list, resync, third := r2.OpenWindow(), r2.OpenWindow(), r2.OpenWindow()
	r2.ApplyEnumeration(list, pageInfos(regIDA))
	r2.ApplyEnumeration(resync, nil)
	if r2.ApplyEnumeration(third, pageInfos(regIDA)); r2.Has(regIDA) {
		t.Fatal("a reply in flight across the conflict registered A")
	}
	if r2.ApplyEnumeration(r2.OpenWindow(), pageInfos(regIDA)); !r2.Has(regIDA) {
		t.Fatal("the enumeration requested after the conflict did not register a live A")
	}
}

// A contested discovery is not a report that the target is gone: the tab_new
// that is creating it still commits, and from then on the daemon knows the
// target was created meanwhile, which protects it like any concurrent tab_new.
func TestContestedDiscoveryIsNotHeldAgainstCreation(t *testing.T) {
	t.Run("commit after the conflict", func(t *testing.T) {
		r := NewTabRegistry()
		stale := r.OpenWindow() // snapshot predates the creation
		create := r.OpenWindow()
		r.ApplyEnumeration(r.OpenWindow(), pageInfos(regIDA)) // another caller lists the new tab
		if _, removed := r.ApplyEnumeration(stale, nil); len(removed) != 1 || !removed[0].Contested {
			t.Fatalf("removed = %#v, want A contested", removed)
		}
		if !r.CommitCreated(create, regIDA) || !r.Has(regIDA) {
			t.Fatal("tab_new refused because two lists disagreed about its tab")
		}
	})
	t.Run("commit between the two replies", func(t *testing.T) {
		r := NewTabRegistry()
		stale := r.OpenWindow()
		create := r.OpenWindow()
		r.ApplyEnumeration(r.OpenWindow(), pageInfos(regIDA))
		if !r.CommitCreated(create, regIDA) {
			t.Fatal("creation refused")
		}
		if _, removed := r.ApplyEnumeration(stale, nil); len(removed) != 0 || !r.Has(regIDA) {
			t.Fatalf("committed tab removed by an earlier empty snapshot: %#v", removed)
		}
		// The commit did not renew the registration: an enumeration requested
		// after the first listing still removes the tab, as gone.
		if _, removed := r.ApplyEnumeration(r.OpenWindow(), nil); len(removed) != 1 || removed[0].Contested {
			t.Fatalf("removed = %#v, want A removed", removed)
		}
	})
	t.Run("destroyed meanwhile still wins", func(t *testing.T) {
		r := NewTabRegistry()
		stale := r.OpenWindow()
		create := r.OpenWindow()
		r.ApplyEnumeration(r.OpenWindow(), pageInfos(regIDA))
		r.NoteDestroyedRead(regIDA, false)
		r.ApplyEnumeration(stale, nil)
		if r.CommitCreated(create, regIDA) || r.Has(regIDA) {
			t.Fatal("destroyed tab committed")
		}
	})
}

// A target reported by a lifecycle event while an enumeration was in flight is
// first-hand evidence, ordered by the queue: it keeps its protection.
func TestEventRegisteredTargetKeepsProtection(t *testing.T) {
	r := NewTabRegistry()
	stale := r.OpenWindow()
	if !r.RegisterEvent(r.EventStamp(), regIDA) {
		t.Fatal("event refused")
	}
	r.ApplyEnumeration(r.OpenWindow(), pageInfos(regIDA)) // listed too: still not a mere discovery
	if _, removed := r.ApplyEnumeration(stale, nil); len(removed) != 0 || !r.Has(regIDA) {
		t.Fatalf("removed = %#v, want none", removed)
	}
}

// Readers, the queue consumer, enumerations and creations interleave freely;
// whatever the order, a target whose destroyed notification was read while its
// creation was in flight ends up unregistered and no window leaks.
func TestConcurrentDestroyedReadAndCommit(t *testing.T) {
	r := NewTabRegistry()
	r.RegisterPageTarget(regIDB)
	for i := 0; i < 2000; i++ {
		create := r.OpenWindow()
		list := r.OpenWindow()
		resync := r.OpenWindow()
		var wg sync.WaitGroup
		wg.Add(4)
		go func() { defer wg.Done(); r.CommitCreated(create, regIDA) }()
		go func() { defer wg.Done(); r.ApplyEnumeration(list, pageInfos(regIDA, regIDB)) }()
		go func() { defer wg.Done(); r.ApplyEnumeration(resync, pageInfos(regIDB)) }()
		go func() { defer wg.Done(); r.NoteDestroyedRead(regIDA, false) }()
		wg.Wait()
		// The resync the drop requests is sent after the notification was read.
		r.ApplyEnumeration(r.OpenWindow(), pageInfos(regIDB))
		if r.Has(regIDA) {
			t.Fatalf("iteration %d: destroyed target is registered", i)
		}
		if r.OpenWindows() != 0 || !r.Has(regIDB) {
			t.Fatalf("iteration %d: windows=%d B=%v", i, r.OpenWindows(), r.Has(regIDB))
		}
	}
}
