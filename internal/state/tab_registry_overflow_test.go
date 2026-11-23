package state

import (
	"sync"
	"testing"
)

// Regressions for evidence that does not travel through the lifecycle event
// queue in order: destroyed notifications recorded when they are read
// (NoteEventRead), also when a full queue drops them (EventDropped), and
// enumeration replies that contradict each other.

// The queue is full: the created and destroyed notifications of A are both
// dropped, the resync that follows never lists A, and only then does the
// creator see its Target.createTarget reply. The registry never held A, so
// nothing but the notification read from the connection says that it is gone.
func TestDestroyedReadConstrainsCreationInFlight(t *testing.T) {
	for _, queued := range []bool{false, true} {
		r := NewTabRegistry()
		r.SyncPageTargets(pageInfos(regIDB))
		create := r.OpenWindow()
		readDestroyed(r, regIDA, queued)
		r.ApplyEnumeration(r.OpenWindow(), pageInfos(regIDB)) // overflow resync: B only
		if commitOK(r, create, regIDA) || r.Has(regIDA) {
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
	readDestroyed(r, regIDX, false)
	readDestroyed(r, regIDA, true)
	if r.OpenWindows() != 0 {
		t.Fatal("a notification read with no request in flight left a window behind")
	}
	// Normal concurrent creation: an earlier empty snapshot and the destruction
	// of other targets do not touch it.
	stale := r.OpenWindow()
	create := r.OpenWindow()
	readDestroyed(r, regIDX, false)
	if !commitOK(r, create, regIDB) {
		t.Fatal("creation refused because of an unrelated destroyed notification")
	}
	if _, removed, _ := r.ApplyEnumeration(stale, nil); len(removed) != 0 || !r.Has(regIDB) {
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
		readDestroyed(r, regIDA, queued)
		tabs, removed, _ := r.ApplyEnumeration(list, pageInfos(regIDA, regIDB))
		if hasSnapshot(tabs, regIDA) || r.Has(regIDA) || len(removed) != 0 {
			t.Fatalf("queued=%v: tabs=%v removed=%v, A must not be registered", queued, tabs, removed)
		}
		if !hasSnapshot(tabs, regIDB) {
			t.Fatalf("queued=%v: B missing from %v", queued, tabs)
		}
	}
}

// Reading the notification takes effect at once — the target stops resolving —
// but does not unregister: that is done, together with the daemon state of the
// tab, by whoever applies it — the queue consumer, or ANY enumeration applied
// next, whenever it was requested and whatever it lists.
func TestDestroyedReadStopsResolvingAndLeavesCleanupToTheConsumer(t *testing.T) {
	for name, apply := range map[string]func(r *TabRegistry, inFlight *Window) (removed []RemovedTab){
		"queue consumer": func(r *TabRegistry, _ *Window) []RemovedTab {
			if !r.RemoveEventTarget(regIDA) {
				t.Fatal("consumer was not told the target was known")
			}
			return []RemovedTab{{TargetID: regIDA}}
		},
		"enumeration requested afterwards": func(r *TabRegistry, _ *Window) []RemovedTab {
			_, removed, _ := r.ApplyEnumeration(r.OpenWindow(), pageInfos(regIDB))
			return removed
		},
		"enumeration in flight that still lists it": func(r *TabRegistry, w *Window) []RemovedTab {
			_, removed, _ := r.ApplyEnumeration(w, pageInfos(regIDA, regIDB))
			return removed
		},
	} {
		r := NewTabRegistry()
		r.SyncPageTargets(pageInfos(regIDA, regIDB))
		r.Select(regIDA)
		inFlight := r.OpenWindow()
		readDestroyed(r, regIDA, false)
		if r.Has(regIDA) || r.Selected() == regIDA {
			t.Fatalf("%s: a target whose destroyed notification was read still resolves", name)
		}
		if !r.Known(regIDA) {
			t.Fatalf("%s: the reader unregistered a target (its daemon state would leak)", name)
		}
		removed := apply(r, inFlight)
		if len(removed) != 1 || removed[0].TargetID != regIDA || r.Known(regIDA) || !r.Has(regIDB) {
			t.Fatalf("%s: removed = %#v known = %v", name, removed, r.Known(regIDA))
		}
		if ok, _ := r.CommitCreated(r.OpenWindow(), regIDB); !ok {
			t.Fatalf("%s: B affected", name)
		}
	}
}

// Events still queued when a destroyed notification is dropped were read
// before it. One of them must not register the destroyed target while the
// resync is pending; with the notification queued, the queue itself orders
// them and nothing is refused.
func TestDroppedDestroyedOutdatesQueuedExistenceEvents(t *testing.T) {
	r := NewTabRegistry()
	stamp := r.NoteEventRead("") // targetInfoChanged(A) read here
	readDestroyed(r, regIDA, false)
	if r.RegisterEvent(stamp, regIDA) || r.Has(regIDA) {
		t.Fatal("an event read before a dropped destroyed notification registered the target")
	}
	// An event read afterwards is newer evidence.
	if !r.RegisterEvent(r.NoteEventRead(""), regIDB) {
		t.Fatal("an event read after the drop was refused")
	}

	q := NewTabRegistry()
	stamp = q.NoteEventRead("")
	readDestroyed(q, regIDA, true)
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
// nothing orders them; the listing alone must not keep A resolving, and the
// enumeration requested after the conflict removes it.
func TestDiscoveryDoesNotSurviveContradictingEnumeration(t *testing.T) {
	r := NewTabRegistry()
	r.SyncPageTargets(pageInfos(regIDB))
	list := r.OpenWindow()
	resync := r.OpenWindow()
	if tabs, _, _ := r.ApplyEnumeration(list, pageInfos(regIDA, regIDB)); !hasSnapshot(tabs, regIDA) {
		t.Fatalf("precondition: the list introduces A, got %v", tabs)
	}
	r.Select(regIDA)
	tabs, removed, verify := r.ApplyEnumeration(resync, pageInfos(regIDB))
	if len(removed) != 0 || !verify {
		t.Fatalf("removed = %#v verify = %v, want A suspended and a fresh enumeration requested", removed, verify)
	}
	if r.Has(regIDA) || r.Selected() == regIDA || hasSnapshot(tabs, regIDA) {
		t.Fatal("A still resolves after the contradicting resync")
	}
	if !r.Has(regIDB) {
		t.Fatal("B lost")
	}
	if _, removed, verify := r.ApplyEnumeration(r.OpenWindow(), pageInfos(regIDB)); len(removed) != 1 || removed[0].TargetID != regIDA || verify || r.Known(regIDA) {
		t.Fatalf("removed = %#v verify = %v, want A removed by the enumeration requested after the conflict", removed, verify)
	}
	// A third reply that was in flight across the conflict cannot bring A back
	// either; only an enumeration requested afterwards can.
	r2 := NewTabRegistry()
	list, resync, third := r2.OpenWindow(), r2.OpenWindow(), r2.OpenWindow()
	r2.ApplyEnumeration(list, pageInfos(regIDA))
	r2.ApplyEnumeration(resync, nil)
	if _, _, verify := r2.ApplyEnumeration(third, pageInfos(regIDA)); r2.Has(regIDA) || !verify {
		t.Fatalf("a reply in flight across the conflict made A resolve (verify=%v)", verify)
	}
	if r2.ApplyEnumeration(r2.OpenWindow(), pageInfos(regIDA)); !r2.Has(regIDA) {
		t.Fatal("the enumeration requested after the conflict did not bring a live A back")
	}
}

// A suspended discovery is not a report that the target is gone: the tab_new
// that is creating it still commits, and from then on the daemon knows the
// target was created meanwhile, which protects it like any concurrent tab_new.
// The commit asks for a fresh enumeration, because absence evidence exists that
// it could not order.
func TestContestedDiscoveryIsNotHeldAgainstCreation(t *testing.T) {
	t.Run("commit after the conflict", func(t *testing.T) {
		r := NewTabRegistry()
		stale := r.OpenWindow() // snapshot predates the creation
		create := r.OpenWindow()
		r.ApplyEnumeration(r.OpenWindow(), pageInfos(regIDA)) // another caller lists the new tab
		if _, removed, verify := r.ApplyEnumeration(stale, nil); len(removed) != 0 || !verify || r.Has(regIDA) {
			t.Fatalf("removed = %#v verify = %v, want A suspended", removed, verify)
		}
		ok, verify := r.CommitCreated(create, regIDA)
		if !ok || !verify || !r.Has(regIDA) {
			t.Fatalf("ok = %v verify = %v: tab_new refused, or not verified, because two lists disagreed about its tab", ok, verify)
		}
	})
	t.Run("commit between the two replies", func(t *testing.T) {
		r := NewTabRegistry()
		stale := r.OpenWindow()
		create := r.OpenWindow()
		r.ApplyEnumeration(r.OpenWindow(), pageInfos(regIDA))
		if ok, verify := r.CommitCreated(create, regIDA); !ok || verify {
			t.Fatalf("ok = %v verify = %v, want a plain commit", ok, verify)
		}
		if _, removed, verify := r.ApplyEnumeration(stale, nil); len(removed) != 0 || !verify || !r.Has(regIDA) {
			t.Fatalf("committed tab removed by an earlier empty snapshot: %#v (verify=%v)", removed, verify)
		}
		// The commit did not renew the registration: an enumeration requested
		// after the first listing still removes the tab, as gone.
		if _, removed, _ := r.ApplyEnumeration(r.OpenWindow(), nil); len(removed) != 1 || r.Known(regIDA) {
			t.Fatalf("removed = %#v, want A removed", removed)
		}
	})
	t.Run("commit when an overlapping enumeration did not list it", func(t *testing.T) {
		r := NewTabRegistry()
		create := r.OpenWindow()
		r.ApplyEnumeration(r.OpenWindow(), nil) // may predate the creation
		if ok, verify := r.CommitCreated(create, regIDA); !ok || !verify || !r.Has(regIDA) {
			t.Fatalf("ok = %v verify = %v, want committed and verified", ok, verify)
		}
	})
	t.Run("destroyed meanwhile still wins", func(t *testing.T) {
		r := NewTabRegistry()
		stale := r.OpenWindow()
		create := r.OpenWindow()
		r.ApplyEnumeration(r.OpenWindow(), pageInfos(regIDA))
		readDestroyed(r, regIDA, false)
		r.ApplyEnumeration(stale, nil)
		if commitOK(r, create, regIDA) || r.Has(regIDA) {
			t.Fatal("destroyed tab committed")
		}
	})
}

// A target reported by a lifecycle event while an enumeration was in flight is
// first-hand evidence, ordered by the queue: it keeps its protection.
func TestEventRegisteredTargetKeepsProtection(t *testing.T) {
	r := NewTabRegistry()
	stale := r.OpenWindow()
	if !r.RegisterEvent(r.NoteEventRead(""), regIDA) {
		t.Fatal("event refused")
	}
	r.ApplyEnumeration(r.OpenWindow(), pageInfos(regIDA)) // listed too: still not a mere discovery
	if _, removed, _ := r.ApplyEnumeration(stale, nil); len(removed) != 0 || !r.Has(regIDA) {
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
		go func() { defer wg.Done(); commitOK(r, create, regIDA) }()
		go func() { defer wg.Done(); r.ApplyEnumeration(list, pageInfos(regIDA, regIDB)) }()
		go func() { defer wg.Done(); r.ApplyEnumeration(resync, pageInfos(regIDB)) }()
		go func() { defer wg.Done(); readDestroyed(r, regIDA, false) }()
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

// An event is evidence about the instant it was READ, however late the queue
// consumer gets to it (review round 6, P1-4). Read before an enumeration was
// requested, it cannot shield its target from that enumeration; read after,
// it can, and then the browser has to be asked again.
func TestEventSeenIsItsReadInstantNotItsHandlingInstant(t *testing.T) {
	t.Run("read before the request", func(t *testing.T) {
		r := NewTabRegistry()
		r.SyncPageTargets(pageInfos(regIDB))
		stamp := r.NoteEventRead("") // targetCreated(A) read, queued
		absent := r.OpenWindow()     // a list requested afterwards; A is gone by then
		if !r.RegisterEvent(stamp, regIDA) || !r.Has(regIDA) {
			t.Fatal("precondition: the event, handled while the list is in flight, registers A")
		}
		_, removed, verify := r.ApplyEnumeration(absent, pageInfos(regIDB))
		if len(removed) != 1 || removed[0].TargetID != regIDA || verify || r.Known(regIDA) {
			t.Fatalf("removed = %#v verify = %v: late handling made the event newer than the list", removed, verify)
		}
	})
	t.Run("read after the request", func(t *testing.T) {
		r := NewTabRegistry()
		r.SyncPageTargets(pageInfos(regIDB))
		stale := r.OpenWindow()
		stamp := r.NoteEventRead("")
		if !r.RegisterEvent(stamp, regIDA) {
			t.Fatal("event refused")
		}
		_, removed, verify := r.ApplyEnumeration(stale, pageInfos(regIDB))
		if len(removed) != 0 || !verify || !r.Has(regIDA) {
			t.Fatalf("removed = %#v verify = %v: a tab announced after the request must stay, and be verified", removed, verify)
		}
		// The verification is requested after the event was read: it decides.
		if _, removed, verify := r.ApplyEnumeration(r.OpenWindow(), pageInfos(regIDB)); len(removed) != 1 || verify {
			t.Fatalf("removed = %#v verify = %v, want A removed by the verification", removed, verify)
		}
	})
}

// Window openings and event reads never share an instant, whichever comes
// first: otherwise "read before the request" could not be told from "after".
func TestWindowOpeningsAndEventReadsAreDistinctInstants(t *testing.T) {
	r := NewTabRegistry()
	w1 := r.OpenWindow()
	s1 := r.NoteEventRead("")
	w2 := r.OpenWindow()
	s2 := r.NoteEventRead(regIDX)
	w3 := r.OpenWindow()
	if !(w1.opened < s1 && s1 < w2.opened && w2.opened < s2 && s2 < w3.opened) {
		t.Fatalf("instants not strictly ordered: %d %d %d %d %d", w1.opened, s1, w2.opened, s2, w3.opened)
	}
	for _, w := range []*Window{w1, w2, w3} {
		r.Abandon(w)
	}
}

// Any dropped event, not only a destroyed notification, outdates the "exists"
// events still queued: what was lost is unknown to the queue.
func TestAnyDroppedEventOutdatesQueuedExistenceEvents(t *testing.T) {
	r := NewTabRegistry()
	stamp := r.NoteEventRead("") // targetInfoChanged(A), queued
	r.NoteEventRead("")          // some other event ...
	r.EventDropped()             // ... that found the queue full
	if r.RegisterEvent(stamp, regIDA) || r.Known(regIDA) {
		t.Fatal("an event read before an overflow registered an unknown target")
	}
	if !r.RegisterEvent(r.NoteEventRead(""), regIDA) {
		t.Fatal("an event read after the overflow was refused")
	}
}
