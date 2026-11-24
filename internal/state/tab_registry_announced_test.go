package state

import "testing"

// A page event read about a target that is not known is a proof that the
// target existed at that instant. The proof must not depend on how soon the
// queue consumer or the creation reply registers the target: an enumeration
// requested after the read may be applied first (FT28 round-9 review, P1).

// created(A) is read while tab_new's reply is still on its way; a list
// requested afterwards does not show A and is applied while A is unknown.
func TestAbsentListAppliedBeforeRegistrationProvesAnnouncedTargetGone(t *testing.T) {
	setup := func(t *testing.T) (r *TabRegistry, create *Window, stamp uint64) {
		r = NewTabRegistry()
		r.SyncPageTargets(pageInfos(regIDB))
		create = r.OpenWindow()
		stamp = r.NoteEventRead(regIDA, "") // created(A) read, queued
		if r.Known(regIDA) || r.PendingAnnounced() != 1 {
			t.Fatalf("known = %v announced = %d", r.Known(regIDA), r.PendingAnnounced())
		}
		return r, create, stamp
	}
	absentList := func(t *testing.T, r *TabRegistry) {
		t.Helper()
		_, removed, verify := r.ApplyEnumeration(r.OpenWindow(), pageInfos(regIDB))
		// Nothing was registered, so nothing is reported for cleanup.
		if len(removed) != 0 || verify || r.Known(regIDA) || r.PendingAnnounced() != 0 {
			t.Fatalf("removed = %#v verify = %v known = %v announced = %d", removed, verify, r.Known(regIDA), r.PendingAnnounced())
		}
	}
	refused := func(t *testing.T, r *TabRegistry, create *Window) {
		t.Helper()
		if ok, verify := r.CommitCreated(create, regIDA); ok || verify || r.Known(regIDA) {
			t.Fatalf("CommitCreated = %v, %v known = %v: the late creation reply registered a target the list proved gone", ok, verify, r.Known(regIDA))
		}
	}

	t.Run("creation reply before the queued event", func(t *testing.T) {
		r, create, stamp := setup(t)
		absentList(t, r)
		refused(t, r, create)
		if r.RegisterEvent(stamp, regIDA) || r.Known(regIDA) {
			t.Fatal("the queued event registered the target")
		}
	})
	t.Run("queued event before the creation reply", func(t *testing.T) {
		r, create, stamp := setup(t)
		absentList(t, r)
		if r.RegisterEvent(stamp, regIDA) || r.Known(regIDA) {
			t.Fatal("the queued event registered the target")
		}
		refused(t, r, create)
	})
	// The consumer could not register A from the event (something newer than
	// the read was applied); the proof is kept for the enumeration that
	// refusal requests, and any list requested after the read answers it.
	t.Run("event refused before the list", func(t *testing.T) {
		r, create, stamp := setup(t)
		r.RemoveTarget(regIDX) // a close of another tab confirmed after the read
		if r.RegisterEvent(stamp, regIDA) || r.Known(regIDA) {
			t.Fatal("event older than a confirmed removal registered its target")
		}
		if r.PendingAnnounced() != 1 {
			t.Fatalf("announced = %d: the proof was discarded with the refusal", r.PendingAnnounced())
		}
		absentList(t, r)
		refused(t, r, create)
	})
	t.Run("event dropped by a full queue", func(t *testing.T) {
		r, create, _ := setup(t)
		r.EventDropped()
		absentList(t, r)
		refused(t, r, create)
	})
	// Every request in flight learns it, enumerations included.
	t.Run("an older list that still shows the target", func(t *testing.T) {
		r, create, _ := setup(t)
		older := r.OpenWindow()
		absentList(t, r)
		if _, _, _ = r.ApplyEnumeration(older, pageInfos(regIDA, regIDB)); r.Known(regIDA) {
			t.Fatal("a reply requested before the target was proven gone registered it")
		}
		refused(t, r, create)
	})
}

// Controls: an announcement proves nothing gone unless the list was requested
// after it was read, and it is answered — never accumulated.
func TestAnnouncementIsOnlyOrderedAgainstLaterRequests(t *testing.T) {
	t.Run("list requested before the event was read", func(t *testing.T) {
		r := NewTabRegistry()
		r.SyncPageTargets(pageInfos(regIDB))
		create := r.OpenWindow()
		stale := r.OpenWindow()
		stamp := r.NoteEventRead(regIDA, "")
		if _, removed, _ := r.ApplyEnumeration(stale, pageInfos(regIDB)); len(removed) != 0 || r.PendingAnnounced() != 1 {
			t.Fatalf("removed = %#v announced = %d: a snapshot that may predate the tab answered its announcement", removed, r.PendingAnnounced())
		}
		ok, verify := r.CommitCreated(create, regIDA)
		if !ok || !verify || !r.Has(regIDA) || r.PendingAnnounced() != 0 {
			t.Fatalf("CommitCreated = %v, %v resolves = %v announced = %d, want a commit that must be verified", ok, verify, r.Has(regIDA), r.PendingAnnounced())
		}
		if !r.RegisterEvent(stamp, regIDA) || !r.Has(regIDA) {
			t.Fatal("the queued event was held against the new tab")
		}
		// The commit took the read as seen: the verification, requested
		// afterwards, removes the tab if it does not list it.
		if _, removed, _ := r.ApplyEnumeration(r.OpenWindow(), pageInfos(regIDB)); len(removed) != 1 || r.Known(regIDA) {
			t.Fatalf("removed = %#v: the verification did not settle the tab", removed)
		}
	})
	t.Run("list requested after the read shows the target", func(t *testing.T) {
		r := NewTabRegistry()
		stamp := r.NoteEventRead(regIDA, "")
		r.EventDropped() // the event never reaches the consumer
		later := r.OpenWindow()
		if tabs, _, verify := r.ApplyEnumeration(r.OpenWindow(), pageInfos(regIDA)); len(tabs) != 1 || verify || !r.Has(regIDA) || r.PendingAnnounced() != 0 {
			t.Fatalf("tabs = %#v verify = %v announced = %d", tabs, verify, r.PendingAnnounced())
		}
		// Registered with the read as its proof: a list requested after the
		// read, though before the registration, removes it.
		if _, removed, verify := r.ApplyEnumeration(later, nil); len(removed) != 1 || verify || r.Known(regIDA) {
			t.Fatalf("removed = %#v verify = %v: the registration renewed a proof older than the list (stamp %d)", removed, verify, stamp)
		}
	})
	t.Run("released by every report that the target is gone", func(t *testing.T) {
		for name, gone := range map[string]func(r *TabRegistry){
			"destroyed read":  func(r *TabRegistry) { r.NoteEventRead("", regIDA) },
			"close confirmed": func(r *TabRegistry) { r.RemoveTarget(regIDA) },
			"not a page":      func(r *TabRegistry) { r.RemoveEventTarget(regIDA) },
		} {
			r := NewTabRegistry()
			r.NoteEventRead(regIDA, "")
			gone(r)
			if r.PendingAnnounced() != 0 {
				t.Errorf("%s: announced = %d", name, r.PendingAnnounced())
			}
		}
	})
	t.Run("not kept for known targets, earliest read wins", func(t *testing.T) {
		r := NewTabRegistry()
		r.SyncPageTargets(pageInfos(regIDB))
		r.NoteEventRead(regIDB, "")
		if r.PendingAnnounced() != 0 {
			t.Fatalf("announced = %d for a known target", r.PendingAnnounced())
		}
		first := r.NoteEventRead(regIDA, "")
		between := r.OpenWindow()
		second := r.NoteEventRead(regIDA, "")
		if r.PendingAnnounced() != 1 {
			t.Fatalf("announced = %d", r.PendingAnnounced())
		}
		if !r.RegisterEvent(second, regIDA) {
			t.Fatal("event refused")
		}
		// seen is the first read: the list requested between the two reads
		// is newer than the proof.
		if _, removed, _ := r.ApplyEnumeration(between, nil); len(removed) != 2 || r.Known(regIDA) {
			t.Fatalf("removed = %#v (first read %d, second %d)", removed, first, second)
		}
	})
}

// ReportedGone is what a caller that keeps a window open across its own
// request uses to learn that a target was reported gone meanwhile, by anyone.
func TestWindowReportsWhatWasGoneWhileItWasOpen(t *testing.T) {
	r := NewTabRegistry()
	r.SyncPageTargets(pageInfos(regIDA, regIDB))
	before := r.OpenWindow()
	r.Abandon(before)
	w := r.OpenWindow()
	if r.ReportedGone(w, regIDA) || r.ReportedGone(nil, regIDA) {
		t.Fatal("nothing was reported yet")
	}
	r.RemoveTarget(regIDA)
	r.ApplyEnumeration(r.OpenWindow(), pageInfos(regIDA, regIDB)) // still listed: known again
	if !r.Has(regIDA) || !r.ReportedGone(w, regIDA) || r.ReportedGone(w, regIDB) || r.ReportedGone(before, regIDA) {
		t.Fatalf("resolves = %v gone(A) = %v gone(B) = %v ended-before = %v", r.Has(regIDA), r.ReportedGone(w, regIDA), r.ReportedGone(w, regIDB), r.ReportedGone(before, regIDA))
	}
	// The enumeration of the window itself counts, and the record outlives
	// the window.
	own := r.OpenWindow()
	if _, removed, _ := r.ApplyEnumeration(own, pageInfos(regIDA)); len(removed) != 1 {
		t.Fatalf("removed = %#v", removed)
	}
	if !r.ReportedGone(own, regIDB) || r.ReportedGone(own, regIDA) {
		t.Fatal("the window's own enumeration was not recorded in it")
	}
	r.Abandon(w)
	if !r.ReportedGone(w, regIDA) || r.OpenWindows() != 0 {
		t.Fatal("the record was lost when the window ended")
	}
}
