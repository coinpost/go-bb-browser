package state

import (
	"strings"
	"testing"

	"github.com/chromedp/cdproto/target"
)

const (
	regIDA = target.ID("AAAA0000000000000000000000AB1234")
	regIDB = target.ID("BBBB0000000000000000000000AB1234")
)

func TestSnapshotsCarryCompleteTargetID(t *testing.T) {
	r := NewTabRegistry()
	snap := r.SyncPageTargets([]*target.Info{
		{TargetID: regIDB, Type: "page", Title: "b", URL: "https://b"},
		{TargetID: regIDA, Type: "page", Title: "a", URL: "https://a"},
		{TargetID: "WORKER", Type: "service_worker"},
	})
	if len(snap) != 2 || snap[0].TargetID != regIDA || snap[1].TargetID != regIDB {
		t.Fatalf("snapshots = %#v, want the two page targets ordered by id", snap)
	}
	if snap[0].Title != "a" || snap[0].URL != "https://a" {
		t.Fatalf("snapshot = %#v", snap[0])
	}
	if _, ok := r.Lookup("WORKER"); ok {
		t.Fatal("a non-page target must not resolve")
	}
}

// Lookup takes the complete target id verbatim. In particular the suffix
// aliases the registry used to derive ("1234", lengthened on collision) and
// every other partial or normalized spelling resolve to nothing.
func TestLookupMatchesCompleteIDExactly(t *testing.T) {
	r := NewTabRegistry()
	r.SyncPageTargets(pageInfos(regIDA, regIDB))

	for _, id := range []target.ID{regIDA, regIDB} {
		if got, ok := r.Lookup(string(id)); !ok || got != id {
			t.Fatalf("Lookup(%q) = %q, %v", id, got, ok)
		}
	}
	a := string(regIDA)
	for _, in := range []string{
		"", "1234", "ab1234", "AB1234", a[len(a)-8:], a[1:], a[:4], a[:len(a)-1],
		strings.ToLower(a), " " + a, a + " ", a + "\n", a + "0",
	} {
		if got, ok := r.Lookup(in); ok {
			t.Errorf("Lookup(%q) resolved to %q", in, got)
		}
		if r.Select(target.ID(in)) {
			t.Errorf("Select(%q) succeeded", in)
		}
	}
}

func TestLookupInvalidTab(t *testing.T) {
	r := NewTabRegistry()
	_, ok := r.Lookup("nope")
	if ok {
		t.Fatal("expected miss")
	}
}

func TestSelectUnknownTab(t *testing.T) {
	r := NewTabRegistry()
	if r.Select("abcd") {
		t.Fatal("select should fail for unknown tab")
	}
}

// When A goes away and a same-suffix B appears, A's id stops resolving and
// never comes to mean B, whatever order the two were registered in and also
// in a registry rebuilt from scratch.
func TestGoneTargetIDNeverResolvesToSameSuffixTarget(t *testing.T) {
	orders := map[string][]target.ID{"A first": {regIDA, regIDB}, "B first": {regIDB, regIDA}}
	for name, order := range orders {
		t.Run(name, func(t *testing.T) {
			r := NewTabRegistry()
			for _, id := range order {
				r.RegisterPageTarget(id)
			}
			if !r.Select(regIDA) {
				t.Fatal("select A")
			}
			_, removed := r.SyncPageTargetsDetailed(pageInfos(regIDB))
			if len(removed) != 1 || removed[0].TargetID != regIDA {
				t.Fatalf("removed = %#v, want A", removed)
			}
			if got, ok := r.Lookup(string(regIDA)); ok {
				t.Fatalf("gone A resolved to %q", got)
			}
			if r.Selected() != "" {
				t.Fatalf("selection = %q, want cleared with A (never moved to B)", r.Selected())
			}
			if got, ok := r.Lookup(string(regIDB)); !ok || got != regIDB {
				t.Fatalf("live B = %q, %v", got, ok)
			}

			rebuilt := NewTabRegistry()
			rebuilt.SyncPageTargets(pageInfos(regIDB))
			if _, ok := rebuilt.Lookup(string(regIDA)); ok {
				t.Fatal("rebuilt registry resolved the gone A")
			}
			if got, ok := rebuilt.Lookup(string(regIDB)); !ok || got != regIDB {
				t.Fatalf("rebuilt registry: B = %q, %v", got, ok)
			}
		})
	}
}

func TestRemoveTargetReportsRegistration(t *testing.T) {
	r := NewTabRegistry()
	r.RegisterPageTarget(regIDA)
	if !r.RemoveTarget(regIDA) {
		t.Fatal("RemoveTarget of a registered target = false")
	}
	if r.RemoveTarget(regIDA) || r.Has(regIDA) {
		t.Fatal("target still registered after removal")
	}
}

// commitOK is CommitCreated for tests that only care whether it registered.
func commitOK(r *TabRegistry, w *Window, id target.ID) bool {
	ok, _ := r.CommitCreated(w, id)
	return ok
}

// readDestroyed plays the connection reader for one Target.targetDestroyed
// notification: read (NoteEventRead), then either queued or dropped by a full
// queue (EventDropped), in the order Session.enqueueTargetEvent uses.
func readDestroyed(r *TabRegistry, id target.ID, queued bool) {
	r.NoteEventRead(id)
	if !queued {
		r.EventDropped()
	}
}
