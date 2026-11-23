package state

import (
	"fmt"
	"testing"

	"github.com/chromedp/cdproto/target"
)

// The registration entries judge evidence by the same rules whether or not the
// target is already known when they run (review round 8).

// A creation reply for a target its creation event registered first sees the
// same window evidence as one for an unknown target: an enumeration applied
// while the creation was in flight that did not list it asks for verification.
func TestCommitCreatedChecksWindowAbsenceForKnownTarget(t *testing.T) {
	for _, known := range []bool{false, true} {
		t.Run(fmt.Sprintf("announced first=%v", known), func(t *testing.T) {
			r := NewTabRegistry()
			r.SyncPageTargets(pageInfos(regIDB))
			old := r.OpenWindow()    // B-only list, in flight
			create := r.OpenWindow() // tab_new
			if known {
				if !r.RegisterEvent(r.NoteEventRead(""), regIDA) {
					t.Fatal("creation event refused")
				}
			}
			_, removed, verify := r.ApplyEnumeration(old, pageInfos(regIDB))
			if len(removed) != 0 || verify != known {
				t.Fatalf("old list: removed = %#v verify = %v", removed, verify)
			}
			ok, verify := r.CommitCreated(create, regIDA)
			if !ok || !verify || !r.Has(regIDA) {
				t.Fatalf("CommitCreated = %v, verify = %v: the creation overlapped a list without the tab and must be verified", ok, verify)
			}
			// The verification is requested after the commit: it decides.
			if _, removed, verify := r.ApplyEnumeration(r.OpenWindow(), pageInfos(regIDB)); len(removed) != 1 || removed[0].TargetID != regIDA || verify {
				t.Fatalf("verification: removed = %#v verify = %v, want A removed", removed, verify)
			}
		})
	}

	t.Run("no enumeration applied meanwhile", func(t *testing.T) {
		r := NewTabRegistry()
		create := r.OpenWindow()
		r.RegisterEvent(r.NoteEventRead(""), regIDA)
		if ok, verify := r.CommitCreated(create, regIDA); !ok || verify {
			t.Fatalf("CommitCreated = %v, verify = %v, want a plain commit", ok, verify)
		}
	})
	t.Run("an enumeration applied meanwhile listed it", func(t *testing.T) {
		r := NewTabRegistry()
		create := r.OpenWindow()
		r.RegisterEvent(r.NoteEventRead(""), regIDA)
		r.ApplyEnumeration(r.OpenWindow(), pageInfos(regIDA))
		if ok, verify := r.CommitCreated(create, regIDA); !ok || verify {
			t.Fatalf("CommitCreated = %v, verify = %v, want a plain commit", ok, verify)
		}
	})
	t.Run("known again after it was reported gone", func(t *testing.T) {
		r := NewTabRegistry()
		create := r.OpenWindow()
		r.RegisterEvent(r.NoteEventRead(""), regIDA)
		r.RemoveTarget(regIDA)
		r.ApplyEnumeration(r.OpenWindow(), pageInfos(regIDA)) // Chrome still lists it
		if ok, _ := r.CommitCreated(create, regIDA); ok {
			t.Fatal("a creation reply that predates the removal was committed")
		}
	})
	t.Run("doomed", func(t *testing.T) {
		r := NewTabRegistry()
		r.RegisterEvent(r.NoteEventRead(""), regIDA)
		r.NoteEventRead(regIDA)
		if ok, _ := r.CommitCreated(nil, regIDA); ok || r.Has(regIDA) {
			t.Fatal("a doomed target was committed")
		}
	})
}

// A queued event proves its target existed when it was READ, also when the
// creation reply registered the target before the consumer got to the event:
// the later commit does not stand in for the earlier proof.
func TestRegisterEventKeepsEarliestProofOfKnownTarget(t *testing.T) {
	// created(A) read; a list requested afterwards (A gone by then); tab_new
	// commits A.
	setup := func(t *testing.T) (r *TabRegistry, stamp uint64, absent *Window) {
		r = NewTabRegistry()
		r.SyncPageTargets(pageInfos(regIDB))
		create := r.OpenWindow()
		stamp = r.NoteEventRead("")
		absent = r.OpenWindow()
		if ok, verify := r.CommitCreated(create, regIDA); !ok || verify {
			t.Fatalf("CommitCreated = %v, %v", ok, verify)
		}
		return r, stamp, absent
	}

	t.Run("event handled before the list is applied", func(t *testing.T) {
		r, stamp, absent := setup(t)
		if !r.RegisterEvent(stamp, regIDA) || !r.Has(regIDA) {
			t.Fatal("the event of a committed target was refused")
		}
		_, removed, verify := r.ApplyEnumeration(absent, pageInfos(regIDB))
		if len(removed) != 1 || removed[0].TargetID != regIDA || verify || r.Known(regIDA) {
			t.Fatalf("removed = %#v verify = %v: the commit masked a proof older than the list", removed, verify)
		}
	})

	t.Run("event handled after the list is applied", func(t *testing.T) {
		r, stamp, absent := setup(t)
		late := r.OpenWindow() // a request in flight: must learn that A is gone
		_, removed, verify := r.ApplyEnumeration(absent, pageInfos(regIDB))
		if len(removed) != 0 || !verify || !r.Has(regIDA) {
			t.Fatalf("removed = %#v verify = %v: nothing orders the list against the commit yet", removed, verify)
		}
		if r.RegisterEvent(stamp, regIDA) {
			t.Fatal("RegisterEvent must ask for an enumeration: the event proves the applied list newer than the tab")
		}
		if r.Has(regIDA) || !r.Known(regIDA) {
			t.Fatalf("resolves = %v known = %v, want gone and awaiting cleanup", r.Has(regIDA), r.Known(regIDA))
		}
		if _, ok := r.Lookup(string(regIDA)); ok || r.Select(regIDA) {
			t.Fatal("the dead target is still usable")
		}
		// Whatever the cleanup enumeration lists, A is reported removed, and
		// nothing requested before can bring it back.
		_, removed, _ = r.ApplyEnumeration(r.OpenWindow(), pageInfos(regIDA, regIDB))
		if len(removed) != 1 || removed[0].TargetID != regIDA || r.Known(regIDA) {
			t.Fatalf("cleanup: removed = %#v known = %v", removed, r.Known(regIDA))
		}
		if _, _, _ = r.ApplyEnumeration(late, pageInfos(regIDA, regIDB)); r.Known(regIDA) {
			t.Fatal("a reply requested before the target was proven gone registered it again")
		}
	})

	t.Run("event read after the list was requested keeps the new tab", func(t *testing.T) {
		r := NewTabRegistry()
		r.SyncPageTargets(pageInfos(regIDB))
		create := r.OpenWindow()
		stale := r.OpenWindow()
		stamp := r.NoteEventRead("")
		if ok, verify := r.CommitCreated(create, regIDA); !ok || verify {
			t.Fatalf("CommitCreated = %v, %v", ok, verify)
		}
		if !r.RegisterEvent(stamp, regIDA) {
			t.Fatal("event refused")
		}
		_, removed, verify := r.ApplyEnumeration(stale, pageInfos(regIDB))
		if len(removed) != 0 || !verify || !r.Has(regIDA) {
			t.Fatalf("removed = %#v verify = %v: a tab created after the request must stay, and be verified", removed, verify)
		}
		// Same in the other handling order.
		q := NewTabRegistry()
		create, stale = q.OpenWindow(), q.OpenWindow()
		stamp = q.NoteEventRead("")
		q.CommitCreated(create, regIDA)
		q.ApplyEnumeration(stale, nil)
		if !q.RegisterEvent(stamp, regIDA) || !q.Has(regIDA) {
			t.Fatal("an event read after the list was requested was held against the new tab")
		}
	})

	t.Run("a later event renews nothing", func(t *testing.T) {
		r := NewTabRegistry()
		r.RegisterEvent(r.NoteEventRead(""), regIDA)
		absent := r.OpenWindow()
		if !r.RegisterEvent(r.NoteEventRead(""), regIDA) { // info changed, read later
			t.Fatal("event refused")
		}
		if _, removed, _ := r.ApplyEnumeration(absent, nil); len(removed) != 1 {
			t.Fatal("a later event renewed the proof of existence")
		}
	})

	t.Run("an earlier event does not resume a suspended discovery", func(t *testing.T) {
		r := NewTabRegistry()
		w1 := r.OpenWindow()
		stamp := r.NoteEventRead("") // read after both requests below could have been sent
		w2 := r.OpenWindow()
		r.ApplyEnumeration(w2, pageInfos(regIDA))
		r.ApplyEnumeration(w1, nil) // cannot be ordered: suspended
		if !r.RegisterEvent(stamp, regIDA) || r.Has(regIDA) || !r.Known(regIDA) {
			t.Fatalf("resolves = %v known = %v, want still suspended", r.Has(regIDA), r.Known(regIDA))
		}
	})
}

// The read of a destroyed notification is remembered per target only while the
// notification is in the queue, and only against events read before it.
func TestDestroyedReadIsRememberedOnlyWhileQueued(t *testing.T) {
	id := func(i int) target.ID { return target.ID(fmt.Sprintf("DDDD%028d", i)) }

	t.Run("released by the consumer", func(t *testing.T) {
		r := NewTabRegistry()
		const n = 64
		stamps := make([]uint64, n)
		for i := range n {
			stamps[i] = readDestroyed(r, id(i), true)
		}
		if got := r.PendingDestroyed(); got != n {
			t.Fatalf("remembered reads = %d, want %d", got, n)
		}
		for i := range n {
			r.RemoveDestroyedTarget(stamps[i], id(i))
		}
		if got := r.PendingDestroyed(); got != 0 {
			t.Fatalf("remembered reads after the queue was handled = %d", got)
		}
	})

	for name, newer := range map[string]func(*TabRegistry){
		"enumeration applied":   func(r *TabRegistry) { r.ApplyEnumeration(r.OpenWindow(), pageInfos(regIDB)) },
		"command reply removal": func(r *TabRegistry) { r.RemoveTarget(regIDX) },
		"queue overflow":        func(r *TabRegistry) { readDestroyed(r, regIDX, false) },
	} {
		t.Run("released by newer absence: "+name, func(t *testing.T) {
			r := NewTabRegistry()
			old := r.NoteEventRead("") // created(A)
			readDestroyed(r, regIDA, true)
			newer(r)
			if got := r.PendingDestroyed(); got != 0 {
				t.Fatalf("remembered reads = %d, want 0", got)
			}
			// The release loses nothing: the older event is outdated anyway
			// (and now has to be settled by an enumeration).
			if r.RegisterEvent(old, regIDA) || r.Known(regIDA) {
				t.Fatal("the event read before the destroyed notification registered the target")
			}
		})
	}

	t.Run("kept across an earlier event about the same target", func(t *testing.T) {
		r := NewTabRegistry()
		created := r.NoteEventRead("") // created(A)
		r.NoteEventRead("")            // info changed(A): not a page any more
		destroyed := readDestroyed(r, regIDA, true)
		if r.RegisterEvent(created, regIDA); r.Known(regIDA) {
			t.Fatal("created registered a destroyed target")
		}
		r.RemoveEventTarget(regIDA) // the consumer handles the info event
		if r.RegisterEvent(created, regIDA); r.PendingDestroyed() != 1 || r.Known(regIDA) {
			t.Fatal("an older queued event released the destroyed read")
		}
		// A second notification about the same id keeps its own, newer read.
		second := readDestroyed(r, regIDA, true)
		r.RemoveDestroyedTarget(destroyed, regIDA)
		if r.PendingDestroyed() != 1 {
			t.Fatal("handling the first notification released the read of the second")
		}
		r.RemoveDestroyedTarget(second, regIDA)
		if r.PendingDestroyed() != 0 {
			t.Fatal("read not released")
		}
	})

	t.Run("a doomed target needs no enumeration for its older events", func(t *testing.T) {
		r := NewTabRegistry()
		r.SyncPageTargets(pageInfos(regIDA))
		info := r.NoteEventRead("") // info changed(A), queued
		readDestroyed(r, regIDA, true)
		if !r.RegisterEvent(info, regIDA) || r.Has(regIDA) {
			t.Fatal("the older event of a doomed target must be a no-op")
		}
	})

	t.Run("not held against events read afterwards or other targets", func(t *testing.T) {
		r := NewTabRegistry()
		before := r.NoteEventRead("")
		readDestroyed(r, regIDA, true)
		if !r.RegisterEvent(before, regIDB) || !r.Has(regIDB) {
			t.Fatal("the destroyed read of A refused an event about B")
		}
		if !r.RegisterEvent(r.NoteEventRead(""), regIDX) || !r.Has(regIDX) {
			t.Fatal("an event read after the destroyed notification was refused")
		}
	})
}
