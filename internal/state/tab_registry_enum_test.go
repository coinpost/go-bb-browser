package state

import (
	"testing"

	"github.com/chromedp/cdproto/target"
)

// The registry tests below use two targets whose ids end the same way: old is
// the tab a request means, next the page that gets its short id "1234" once
// old is gone.
const (
	enumOld  = target.ID("BBBB99991234")
	enumNext = target.ID("AAAA00001234")
	enumSide = target.ID("CCCC0000ABCD")
)

func infosOf(ids ...target.ID) []*target.Info {
	out := make([]*target.Info, 0, len(ids))
	for _, id := range ids {
		out = append(out, &target.Info{TargetID: id, Type: "page", Title: "title " + string(id)})
	}
	return out
}

func wantPending(t *testing.T, r *TabRegistry, inFlight, removals int) {
	t.Helper()
	if e, n := r.PendingEnumerations(); e != inFlight || n != removals {
		t.Fatalf("pending enumerations = %d, remembered removals = %d; want %d and %d", e, n, inFlight, removals)
	}
}

func TestSyncKeepsTargetsRegisteredAfterEnumerationBegan(t *testing.T) {
	r := NewTabRegistry()
	r.RegisterPageTarget("AAAA1111")
	enum := r.BeginEnumeration() // enumeration requested here
	r.RegisterPageTarget("BBBB2222")

	// The reply lists neither target: the old one is gone, the new one was
	// created after the browser answered.
	res := r.Sync(nil, enum)
	if len(res.Tabs) != 1 || res.Tabs[0].TargetID != "BBBB2222" || res.Tabs[0].ShortID != "2222" {
		t.Fatalf("tabs = %#v, want the kept target", res.Tabs)
	}
	if len(res.Removed) != 1 || res.Removed[0].TargetID != "AAAA1111" {
		t.Fatalf("removed = %#v", res.Removed)
	}
	if len(res.Kept) != 1 || res.Kept[0] != "BBBB2222" {
		t.Fatalf("kept = %#v", res.Kept)
	}
	if _, ok := r.Lookup("2222"); !ok {
		t.Fatal("target registered after the enumeration began was removed")
	}

	// An enumeration requested after the registration is authoritative.
	res = r.Sync(nil, r.BeginEnumeration())
	if len(res.Removed) != 1 || res.Removed[0].TargetID != "BBBB2222" || len(res.Kept) != 0 {
		t.Fatalf("removed = %#v kept = %#v", res.Removed, res.Kept)
	}
	if _, removed := r.SyncPageTargetsDetailed(nil); len(removed) != 0 {
		t.Fatalf("registry not empty: %#v", removed)
	}
	wantPending(t, r, 0, 0)
}

// A reply that still lists a target removed after the enumeration began must
// not register it again. It used to: the dead tab came back, and under a new
// short id when another page had taken its old one meanwhile.
func TestSyncDoesNotResurrectTargetRemovedAfterEnumerationBegan(t *testing.T) {
	for name, successor := range map[string]bool{"short id free": false, "short id taken over": true} {
		t.Run(name, func(t *testing.T) {
			r := NewTabRegistry()
			r.RegisterPageTarget(enumOld)
			r.SelectTarget(enumOld)
			enum := r.BeginEnumeration()
			r.RemoveTarget(enumOld) // Target.targetDestroyed
			listed := []target.ID{enumOld}
			if successor {
				r.RegisterPageTarget(enumNext) // Target.targetCreated
			}

			res := r.Sync(infosOf(listed...), enum)
			if _, ok := r.ShortForTarget(enumOld); ok {
				t.Fatal("removed target was registered again")
			}
			for _, tab := range res.Tabs {
				if tab.TargetID == enumOld {
					t.Fatalf("removed target is in the snapshot: %#v", res.Tabs)
				}
			}
			if len(res.Removed) != 0 {
				t.Fatalf("removed = %#v", res.Removed)
			}
			if res.Focus != "" || r.Selected() != "" {
				t.Fatalf("focus = %q, selected = %q", res.Focus, r.Selected())
			}
			tid, ok := r.Lookup("1234")
			if successor && (!ok || tid != enumNext || len(res.Tabs) != 1 || res.Tabs[0].TargetID != enumNext) {
				t.Fatalf("successor lost: lookup %q %v, tabs %#v", tid, ok, res.Tabs)
			}
			if !successor && (ok || len(res.Tabs) != 0) {
				t.Fatalf("lookup %q %v, tabs %#v", tid, ok, res.Tabs)
			}
			wantPending(t, r, 0, 0)

			// An enumeration begun after the removal is authoritative again.
			res = r.Sync(infosOf(enumOld), r.BeginEnumeration())
			if _, ok := r.ShortForTarget(enumOld); !ok {
				t.Fatalf("a later enumeration could not register the target: %#v", res.Tabs)
			}
		})
	}
}

// Removed and registered again while the enumeration was in flight: the
// registration is the later change and stands, whatever the reply lists.
func TestSyncKeepsTargetRegisteredAgainAfterRemoval(t *testing.T) {
	for name, listed := range map[string][]target.ID{"listed": {enumOld}, "not listed": nil} {
		t.Run(name, func(t *testing.T) {
			r := NewTabRegistry()
			r.RegisterPageTarget(enumOld)
			enum := r.BeginEnumeration()
			r.RemoveTarget(enumOld)
			r.RegisterPageTarget(enumOld)
			res := r.Sync(infosOf(listed...), enum)
			if short, ok := r.ShortForTarget(enumOld); !ok || short != "1234" || len(res.Tabs) != 1 || len(res.Removed) != 0 {
				t.Fatalf("short %q %v, result %#v", short, ok, res)
			}
			wantPending(t, r, 0, 0)
		})
	}
}

// Removals are remembered only for the enumerations in flight, and no longer
// than the oldest of them that began before the removal.
func TestRemovalsAreRememberedOnlyForEnumerationsInFlight(t *testing.T) {
	r := NewTabRegistry()
	for _, id := range []target.ID{"AAAA1111", "BBBB2222", "CCCC3333", "DDDD4444"} {
		r.RegisterPageTarget(id)
	}
	r.RemoveTarget("AAAA1111")
	wantPending(t, r, 0, 0) // nothing in flight: nothing to remember

	first := r.BeginEnumeration()
	r.RemoveTarget("BBBB2222")
	wantPending(t, r, 1, 1)
	second := r.BeginEnumeration()
	r.RemoveTarget("CCCC3333")
	wantPending(t, r, 2, 2)

	// With first gone, only the removal second can be stale about is left.
	first.End()
	wantPending(t, r, 1, 1)
	first.End() // ending twice changes nothing
	wantPending(t, r, 1, 1)

	// second does not bring back what was removed after it began, but it began
	// after BBBB2222 was removed and is authoritative for that one.
	res := r.Sync(infosOf("BBBB2222", "CCCC3333", "DDDD4444"), second)
	if len(res.Tabs) != 2 || res.Tabs[0].TargetID != "BBBB2222" || res.Tabs[1].TargetID != "DDDD4444" {
		t.Fatalf("tabs = %#v", res.Tabs)
	}
	wantPending(t, r, 0, 0)
	second.End()
	var none *Enumeration
	none.End()

	// An enumeration given up (error, cancellation) releases what it held.
	third := r.BeginEnumeration()
	r.RemoveTarget("DDDD4444")
	wantPending(t, r, 1, 1)
	third.End()
	wantPending(t, r, 0, 0)

	// A sync that removes targets itself leaves nothing behind either.
	r.Sync(nil, r.BeginEnumeration())
	wantPending(t, r, 0, 0)
	if _, removed := r.SyncPageTargetsDetailed(nil); len(removed) != 0 {
		t.Fatalf("registry not empty: %#v", removed)
	}
}

// The result of a sync is one snapshot: the focus it reports is one of its tabs.
func TestSyncResultFocusIsFromTheSameSnapshot(t *testing.T) {
	r := NewTabRegistry()
	r.RegisterPageTarget(enumOld)
	r.SelectTarget(enumOld)
	if res := r.Sync(infosOf(enumOld, enumSide), nil); res.Focus != "1234" || len(res.Tabs) != 2 {
		t.Fatalf("result = %#v", res)
	}
	res := r.Sync(infosOf(enumSide), nil)
	if res.Focus != "" || len(res.Tabs) != 1 {
		t.Fatalf("focus %q outlived its tab: %#v", res.Focus, res.Tabs)
	}
}

// SelectAlias commits a selection prepared earlier: only if the short id still
// names the target it was resolved to.
func TestSelectAliasChecksAliasAndTargetTogether(t *testing.T) {
	r := NewTabRegistry()
	r.RegisterPageTarget(enumOld)
	r.RegisterPageTarget(enumSide)
	if !r.SelectAlias("1234", enumOld) || r.Selected() != "1234" {
		t.Fatalf("selected = %q", r.Selected())
	}
	r.SelectTarget(enumSide)

	// The tab is gone and its short id belongs to another page.
	r.RemoveTarget(enumOld)
	r.RegisterPageTarget(enumNext)
	if r.SelectAlias("1234", enumOld) {
		t.Fatal("selected through a short id that names another target")
	}
	// The target is registered again, under another short id.
	if short := r.RegisterPageTarget(enumOld); short == "1234" {
		t.Fatalf("test setup: short = %q", short)
	}
	if r.SelectAlias("1234", enumOld) {
		t.Fatal("selected a target that no longer has the requested short id")
	}
	if r.Selected() != "abcd" {
		t.Fatalf("a refused selection changed the focus to %q", r.Selected())
	}
}

func TestCommitFocus(t *testing.T) {
	// The snapshot a probe ran on: old (1234) and side (abcd), in that order.
	setup := func() (*TabRegistry, []TabSnapshot) {
		r := NewTabRegistry()
		return r, r.Sync(infosOf(enumOld, enumSide), nil).Tabs
	}
	replaceOld := func(r *TabRegistry) {
		r.RemoveTarget(enumOld)
		if short := r.RegisterPageTarget(enumNext); short != "1234" {
			panic("test setup: successor short id " + short)
		}
	}

	t.Run("detected tab becomes the focus", func(t *testing.T) {
		r, tabs := setup()
		tab, focus, ok := r.CommitFocus(tabs, enumSide)
		if !ok || tab.TargetID != enumSide || tab.ShortID != "abcd" || focus != "abcd" || tab.Title != "title "+string(enumSide) {
			t.Fatalf("tab %#v focus %q ok %v", tab, focus, ok)
		}
		if r.Selected() != "abcd" {
			t.Fatalf("selected = %q", r.Selected())
		}
	})
	t.Run("nothing detected, nothing selected: first tab", func(t *testing.T) {
		r, tabs := setup()
		tab, focus, ok := r.CommitFocus(tabs, "")
		if !ok || tab.TargetID != enumOld || tab.ShortID != "1234" || focus != "" || r.Selected() != "" {
			t.Fatalf("tab %#v focus %q ok %v selected %q", tab, focus, ok, r.Selected())
		}
	})
	t.Run("nothing detected: the selection stands", func(t *testing.T) {
		r, tabs := setup()
		r.SelectTarget(enumSide)
		tab, focus, ok := r.CommitFocus(tabs, "")
		if !ok || tab.TargetID != enumSide || focus != "abcd" || tab.Title == "" {
			t.Fatalf("tab %#v focus %q ok %v", tab, focus, ok)
		}
	})
	t.Run("first tab replaced under its short id: skipped", func(t *testing.T) {
		r, tabs := setup()
		replaceOld(r)
		tab, focus, ok := r.CommitFocus(tabs, "")
		if !ok || tab.TargetID != enumSide || tab.ShortID != "abcd" || focus != "" {
			t.Fatalf("tab %#v focus %q ok %v", tab, focus, ok)
		}
	})
	t.Run("detected tab replaced: not selected", func(t *testing.T) {
		r, tabs := setup()
		replaceOld(r)
		tab, focus, ok := r.CommitFocus(tabs, enumOld)
		if !ok || tab.TargetID != enumSide || focus != "" || r.Selected() != "" {
			t.Fatalf("tab %#v focus %q ok %v selected %q", tab, focus, ok, r.Selected())
		}
	})
	t.Run("no tab of the snapshot left", func(t *testing.T) {
		r, tabs := setup()
		replaceOld(r)
		r.RemoveTarget(enumSide)
		tab, focus, ok := r.CommitFocus(tabs, enumOld)
		if ok || tab != (TabSnapshot{}) || focus != "" || r.Selected() != "" {
			t.Fatalf("tab %#v focus %q ok %v selected %q", tab, focus, ok, r.Selected())
		}
	})
	t.Run("selected tab is newer than the snapshot", func(t *testing.T) {
		r, tabs := setup()
		replaceOld(r)
		r.SelectTarget(enumNext) // e.g. a tab_new while the probe ran
		tab, focus, ok := r.CommitFocus(tabs, "")
		if !ok || tab.TargetID != enumNext || tab.ShortID != "1234" || focus != "1234" || tab.Title != "" {
			t.Fatalf("tab %#v focus %q ok %v", tab, focus, ok)
		}
	})
}
