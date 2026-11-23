package state

import (
	"testing"

	"github.com/chromedp/cdproto/target"
)

// Regressions for the ordering of enumeration replies against each other: an
// enumeration that was applied confirms the absence of every page target it
// does not list, also of ids that are not registered, and a reply that was
// requested before that point must not register such an id afterwards.

const regIDX = target.ID("CCCC0000000000000000000000AB1234")

func hasSnapshot(snaps []TabSnapshot, id target.ID) bool {
	for _, sn := range snaps {
		if sn.TargetID == id {
			return true
		}
	}
	return false
}

// close(A); an older list still shows A and B; a newer list confirms only B and
// is applied first; the older reply arrives last.
func TestOlderEnumerationDoesNotReviveTargetNewerEnumerationFoundAbsent(t *testing.T) {
	r := NewTabRegistry()
	r.SyncPageTargets(pageInfos(regIDA, regIDB))
	if !r.RemoveTarget(regIDA) {
		t.Fatal("precondition: A was registered")
	}

	older := r.BeginEnumeration() // snapshot still lists A (close confirmed, target lingering)
	newer := r.BeginEnumeration() // snapshot taken after A is gone
	if snaps, _ := r.ApplyEnumeration(newer, pageInfos(regIDB)); len(snaps) != 1 || r.Has(regIDA) {
		t.Fatalf("newer enumeration = %v, want only B", snapshotIDs(snaps))
	}

	snaps, removed := r.ApplyEnumeration(older, pageInfos(regIDA, regIDB))
	if r.Has(regIDA) {
		t.Fatal("older reply re-registered A after a newer enumeration found it absent")
	}
	if _, ok := r.Lookup(string(regIDA)); ok {
		t.Fatal("A resolves again")
	}
	if hasSnapshot(snaps, regIDA) || !hasSnapshot(snaps, regIDB) || len(snaps) != 1 {
		t.Fatalf("older reply snapshots = %v, want only B", snapshotIDs(snaps))
	}
	if len(removed) != 0 || !r.Has(regIDB) {
		t.Fatalf("B was affected: removed=%v", removed)
	}
}

// The same for an id the registry never held: there is no removal to remember,
// only the newer enumeration that did not list it.
func TestOlderEnumerationDoesNotRegisterNeverSeenTargetFoundAbsent(t *testing.T) {
	r := NewTabRegistry()
	r.SyncPageTargets(pageInfos(regIDB))

	older := r.BeginEnumeration()
	r.ApplyEnumeration(r.BeginEnumeration(), pageInfos(regIDB))
	snaps, _ := r.ApplyEnumeration(older, pageInfos(regIDX, regIDB))
	if r.Has(regIDX) || hasSnapshot(snaps, regIDX) {
		t.Fatalf("older reply registered a target a newer enumeration found absent: %v", snapshotIDs(snaps))
	}
	if !r.Has(regIDB) || len(snaps) != 1 {
		t.Fatalf("snapshots = %v, want only B", snapshotIDs(snaps))
	}
}

// Tokens do not say in which order the browser took two overlapping snapshots.
// Whichever reply is applied second, it cannot register what the first one
// found absent while it was in flight.
func TestOverlappingEnumerationsAbsenceWinsInEitherOrder(t *testing.T) {
	r := NewTabRegistry()
	r.SyncPageTargets(pageInfos(regIDA, regIDB))
	r.RemoveTarget(regIDA)

	first := r.BeginEnumeration()
	second := r.BeginEnumeration()
	// The earlier request carries the snapshot without A and is applied first.
	r.ApplyEnumeration(first, pageInfos(regIDB))
	snaps, _ := r.ApplyEnumeration(second, pageInfos(regIDA, regIDB))
	if r.Has(regIDA) || hasSnapshot(snaps, regIDA) {
		t.Fatalf("later-requested reply revived A: %v", snapshotIDs(snaps))
	}
}

// Absence is not a tombstone: an enumeration requested after the one that found
// the target absent is current and registers what it lists.
func TestEnumerationRequestedAfterConfirmedAbsenceRegistersTarget(t *testing.T) {
	r := NewTabRegistry()
	r.SyncPageTargets(pageInfos(regIDB))

	older := r.BeginEnumeration()
	r.ApplyEnumeration(r.BeginEnumeration(), pageInfos(regIDB))
	if snaps, _ := r.ApplyEnumeration(older, pageInfos(regIDX, regIDB)); hasSnapshot(snaps, regIDX) {
		t.Fatal("precondition: the overtaken reply must not register X")
	}

	snaps, _ := r.ApplyEnumeration(r.BeginEnumeration(), pageInfos(regIDX, regIDB))
	if !r.Has(regIDX) || !hasSnapshot(snaps, regIDX) {
		t.Fatalf("current enumeration did not register X: %v", snapshotIDs(snaps))
	}
	// SyncPageTargets is current by definition.
	r2 := NewTabRegistry()
	r2.SyncPageTargets(pageInfos(regIDB))
	if snaps := r2.SyncPageTargets(pageInfos(regIDX, regIDB)); !r2.Has(regIDX) || len(snaps) != 2 {
		t.Fatalf("SyncPageTargets = %v, want X and B", snapshotIDs(snaps))
	}
}

// Only an enumeration that did NOT list the target counts. One that listed it
// (even if it could not register it itself) is not evidence of absence, so
// overlapping polls cannot keep a live target out for ever.
func TestEnumerationThatListedTargetIsNotHeldAgainstIt(t *testing.T) {
	r := NewTabRegistry()
	r.SyncPageTargets(pageInfos(regIDB))

	overtaken := r.BeginEnumeration()
	r.ApplyEnumeration(r.BeginEnumeration(), pageInfos(regIDB)) // X absent
	next := r.BeginEnumeration()                                // requested after that absence
	if snaps, _ := r.ApplyEnumeration(overtaken, pageInfos(regIDX, regIDB)); hasSnapshot(snaps, regIDX) {
		t.Fatal("precondition: the overtaken reply must not register X")
	}
	// The only enumeration applied since next was requested listed X.
	snaps, _ := r.ApplyEnumeration(next, pageInfos(regIDX, regIDB))
	if !r.Has(regIDX) || !hasSnapshot(snaps, regIDX) {
		t.Fatalf("X kept out by an enumeration that listed it: %v", snapshotIDs(snaps))
	}
}

// The protection of targets registered while an enumeration is in flight is
// unchanged: neither reply removes the new target, and a registered target is
// returned by an older reply that lists it whatever was applied in between.
func TestConfirmedAbsenceKeepsConcurrentRegistrationProtection(t *testing.T) {
	r := NewTabRegistry()
	r.SyncPageTargets(pageInfos(regIDB))

	older := r.BeginEnumeration()
	newer := r.BeginEnumeration()
	r.RegisterPageTarget(regIDA) // created while both are in flight
	if _, removed := r.ApplyEnumeration(newer, pageInfos(regIDB)); len(removed) != 0 || !r.Has(regIDA) {
		t.Fatalf("newer stale reply removed the new tab: %v", removed)
	}
	// The older reply saw the new tab; the reply applied meanwhile did not.
	snaps, removed := r.ApplyEnumeration(older, pageInfos(regIDA, regIDB))
	if len(removed) != 0 || !r.Has(regIDA) || !hasSnapshot(snaps, regIDA) || len(snaps) != 2 {
		t.Fatalf("registered tab not kept and listed: snaps=%v removed=%v", snapshotIDs(snaps), removed)
	}
	// And a target removed for real by the newer enumeration stays removed.
	r.RegisterPageTarget(regIDX)
	stale := r.BeginEnumeration()
	if _, removed := r.ApplyEnumeration(r.BeginEnumeration(), pageInfos(regIDA, regIDB)); len(removed) != 1 {
		t.Fatalf("current enumeration removed %v, want X", removed)
	}
	if snaps, _ := r.ApplyEnumeration(stale, pageInfos(regIDA, regIDB, regIDX)); r.Has(regIDX) || len(snaps) != 2 {
		t.Fatalf("stale reply revived X: %v", snapshotIDs(snaps))
	}
}
