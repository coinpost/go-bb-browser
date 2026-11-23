//go:build realchrome

package daemon

// Real-Chrome acceptance of the tab lifecycle (FT28 round 7). See
// real_chrome_harness_test.go for what runs and how it is isolated. Every
// schedule is "real Chrome + controlled reply/notification timing": it shows
// what the daemon does in that order of events, not how often the order occurs.

import (
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
)

// rcQuiet is how long a schedule leaves the daemon alone (no RPC) before it
// looks: a little over two ticks of the sync loop.
const rcQuiet = 2300 * time.Millisecond

func rcRefused(t *testing.T, r rcReply) {
	t.Helper()
	if r.Error == nil {
		t.Errorf("tab_new succeeded for a tab that was gone before its reply was delivered: %v", r.Result)
		return
	}
	if msg, _ := r.Error["message"].(string); !strings.Contains(msg, "tab closed before tab_new completed") {
		t.Errorf("tab_new error = %v, want the creation-refused error", r.Error)
	}
}

// rcAssertDead checks a tab that is gone from Chrome: unknown to the registry,
// not idle-tracked, refused by the three clears, not persisted; B untouched;
// no Target.closeTarget on the wire except for the dead tab itself. It stops
// the daemon (to replay rpc.jsonl).
func rcAssertDead(t *testing.T, e *rcEnv, a target.ID) {
	t.Helper()
	w := e.proxy.wire()
	known, resolves, idle := e.srv.tabs.Known(a), e.srv.tabs.Has(a), e.idleTracked(a)
	accepted := e.clearsAccept(a)
	pages := e.chrome.pages()
	disk := e.managedOnDisk()
	t.Logf("dead A=%s: registry known=%v resolves=%v, idle tracked=%v, clears accepted=%d/3, managed on disk=%v; Chrome A=%v B=%v; wire closeTarget=%v getTargets=%d rootSetDiscoverTargets=%d rootEvents=%d",
		a, known, resolves, idle, accepted, disk[a], pages[a], pages[e.b], w.closes, w.getTargets, w.discover, w.rootEvents)
	if known || resolves || idle || accepted != 0 || disk[a] {
		t.Error("the dead tab is still known to the daemon")
	}
	if pages[a] {
		t.Error("bad schedule: A is still open in Chrome")
	}
	if !pages[e.b] {
		t.Error("B is gone from Chrome")
	}
	rcOnlyClosed(t, w, a)
}

// The production connection receives the browser's lifecycle notifications
// with no timing control at all: a managed tab the user closes is forgotten —
// registry, idle tracking, disk — without any RPC and without an enumeration.
func TestRealChromeClosedTabIsForgottenWithoutRPC(t *testing.T) {
	e := rcStart(t, true)
	if w := e.proxy.wire(); w.discover != 1 {
		t.Fatalf("the daemon sent %d root-level Target.setDiscoverTargets, want 1", w.discover)
	}
	a := target.ID(e.mustRPC("tab_new", nil)["tab"].(string))
	e.mustRPC("eval", map[string]any{"tab": string(a), "script": "1"})
	if !e.srv.tabs.Has(a) || !e.idleTracked(a) {
		t.Fatal("precondition: A is a registered, managed tab")
	}
	before := e.proxy.wire()
	e.chrome.userClose(a)
	start := time.Now()
	for e.srv.tabs.Known(a) && time.Since(start) < rcQuiet {
		time.Sleep(2 * time.Millisecond)
	}
	took := time.Since(start)
	after := e.proxy.wire()
	t.Logf("user closed A; registry forgot it after %v with no RPC; Target.getTargets sent meanwhile=%d; destroyed notification delivered=%v",
		took.Round(time.Millisecond), after.getTargets-before.getTargets, e.proxy.wasDelivered("Target.targetDestroyed", a))
	if e.srv.tabs.Known(a) {
		t.Error("a tab closed in the browser is still registered")
	}
	if after.getTargets != before.getTargets {
		t.Error("the cleanup needed an enumeration: it must come from the notification")
	}
	if after.rootEvents == 0 {
		t.Error("no root-level lifecycle notification reached the daemon")
	}
	rcAssertDead(t, e, a)
}

// Chrome started without any page. Chrome 157 answers the daemon's first
// Target.createTarget with "no browser is open" when it has no window, headless
// or not, so the daemon does not start and this is reported as not verified
// (skip) rather than as covered.
func TestRealChromeStartsWithoutAnyPage(t *testing.T) {
	e := rcStart(t, false)
	w := e.proxy.wire()
	t.Logf("zero-page start: daemon healthy; rootSetDiscoverTargets=%d; Chrome pages after connect=%d", w.discover, len(e.chrome.pages()))
	if w.discover != 1 {
		t.Errorf("the daemon sent %d root-level Target.setDiscoverTargets, want 1", w.discover)
	}
	a := target.ID(e.mustRPC("tab_new", nil)["tab"].(string))
	if !e.listed(a) {
		t.Fatal("tab_new result is not listed")
	}
}

// The page the daemon attached to at startup — the only one — is closed by the
// user: the daemon is told (no RPC), keeps running with zero tabs, and tab_new
// still works (INV-7) when Chrome lets it.
func TestRealChromeLastPageClosedThenTabNew(t *testing.T) {
	e := rcStart(t, true)
	a := target.ID(e.mustRPC("tab_new", nil)["tab"].(string))
	e.chrome.userClose(e.b)
	start := time.Now()
	for e.srv.tabs.Known(e.b) && time.Since(start) < rcQuiet {
		time.Sleep(2 * time.Millisecond)
	}
	if e.srv.tabs.Known(e.b) {
		t.Error("the startup page closed by the user is still registered")
	}
	e.chrome.userClose(a)
	for start = time.Now(); e.srv.tabs.Known(a) && time.Since(start) < rcQuiet; {
		time.Sleep(2 * time.Millisecond)
	}
	if e.srv.tabs.Known(a) || e.idleTracked(a) {
		t.Error("the managed tab closed by the user is still registered")
	}
	pages := len(e.chrome.pages())
	rows, _ := e.mustRPC("tab_list", nil)["tabs"].([]any)
	r := e.rpc("tab_new", nil)
	t.Logf("zero pages while running: Chrome pages=%d, tab_list rows=%d, tab_new result=%v error=%v, wire closeTarget=%v",
		pages, len(rows), r.Result, r.Error, e.proxy.wire().closes)
	if pages != 0 || len(rows) != 0 {
		t.Fatalf("precondition: zero pages (Chrome %d, daemon %d)", pages, len(rows))
	}
	if r.Error != nil {
		if hint, _ := r.Error["data"].(map[string]any)["hint"].(string); strings.Contains(hint, "no browser is open") {
			t.Skipf("NOT VERIFIED: with its last window closed this Chrome refuses Target.createTarget: %v", r.Error)
		}
		t.Fatalf("tab_new with zero tabs: %v", r.Error)
	}
	b := target.ID(r.Result["tab"].(string))
	if !e.listed(b) || !e.chrome.pages()[b] {
		t.Error("the tab created from zero tabs is not listed")
	}
	rcOnlyClosed(t, e.proxy.wire(), a, e.b)
}

// P1-1. An older Target.getTargets reply (B only) is delayed while the user
// opens A and eval attaches the daemon to it. Applying the older reply must
// not close A. "withheld" keeps every notification about A away from the
// daemon, which is what its connection looked like before it enabled
// discovery: then only the two contradicting snapshots are left.
func TestRealChromeOverlappingListDoesNotCloseAttachedTab(t *testing.T) {
	for _, tc := range []struct {
		name                string
		listFirst, withheld bool
	}{
		{"list then eval", true, false},
		{"eval only", false, false},
		{"list then eval, notifications withheld", true, true},
		{"eval only, notifications withheld", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := rcStart(t, true)
			if tc.withheld {
				e.proxy.withholdNotifications(func(_ string, id target.ID) bool { return id != e.b })
			}
			old := e.proxy.holdNext("Target.getTargets")
			list := e.rpcAsync("tab_list", nil)
			e.proxy.awaitHeld(old, "older list")
			a := e.chrome.userOpen()
			if !tc.withheld {
				rcAwait(t, "created notification delivered", func() bool { return e.proxy.wasDelivered("Target.targetCreated", a) })
			}
			if tc.listFirst && !e.listed(a) {
				t.Fatal("precondition: tab_list shows the new tab")
			}
			e.mustRPC("eval", map[string]any{"tab": string(a), "script": "1"})
			if w := e.proxy.wire(); len(w.closes) != 0 {
				t.Fatalf("precondition: no close before the older reply: %v", w.closes)
			}
			old.Release()
			if r := <-list; r.Error != nil {
				t.Fatal(r.Error)
			}
			time.Sleep(rcQuiet) // no RPC
			w := e.proxy.wire()
			resolves := e.srv.tabs.Has(a)
			pages := e.chrome.pages()
			again := e.rpc("eval", map[string]any{"tab": string(a), "script": "1"})
			t.Logf("A=%s: wire closeTarget=%v; Chrome A=%v B=%v; A resolves after %v without RPC=%v; eval again error=%v; getTargets=%d rootSetDiscoverTargets=%d rootEvents=%d",
				a, w.closes, pages[a], pages[e.b], rcQuiet, resolves, again.Error, w.getTargets, w.discover, w.rootEvents)
			if len(w.closes) != 0 {
				t.Errorf("the daemon sent Target.closeTarget: %v", w.closes)
			}
			if !pages[a] || !pages[e.b] {
				t.Error("a live page was closed")
			}
			if !resolves || again.Error != nil {
				t.Error("the live tab did not come back after the conflict")
			}
		})
	}
}

// P1-2. tab_new(A) with a delayed Target.createTarget reply, an older B-only
// list delayed as well, another list that shows A, and the user closing A.
// The creation reply is delivered either right after the older list or only
// after the daemon had time to enumerate again by itself (the timing that
// persisted). "withheld" as above: no notification about A reaches the daemon.
func TestRealChromeLateCreateIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		afterResync, withheld bool
	}{
		{"reply right after the conflict", false, false},
		{"reply after the automatic re-enumeration", true, false},
		{"reply right after the conflict, notifications withheld", false, true},
		{"reply after the automatic re-enumeration, notifications withheld", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := rcStart(t, true)
			if tc.withheld {
				e.proxy.withholdNotifications(func(_ string, id target.ID) bool { return id != e.b })
			}
			old := e.proxy.holdNext("Target.getTargets")
			list := e.rpcAsync("tab_list", nil)
			e.proxy.awaitHeld(old, "older list")
			create := e.proxy.holdNext("Target.createTarget")
			newTab := e.rpcAsync("tab_new", nil)
			e.proxy.awaitHeld(create, "creation")
			a := rcCreatedID(t, create)
			if !e.listed(a) {
				t.Fatal("precondition: another tab_list shows A while its creation reply is delayed")
			}
			e.chrome.userClose(a)
			if !tc.withheld {
				rcAwait(t, "destroyed notification delivered", func() bool { return e.proxy.wasDelivered("Target.targetDestroyed", a) })
			}
			sent := e.proxy.wire().getTargets
			old.Release()
			if r := <-list; r.Error != nil {
				t.Fatal(r.Error)
			}
			if tc.afterResync {
				if tc.withheld {
					rcAwait(t, "the daemon enumerated again by itself", func() bool { return e.proxy.wire().getTargets > sent })
				}
				time.Sleep(rcQuiet)
			}
			create.Release()
			r := <-newTab
			time.Sleep(rcQuiet) // no RPC: nothing may heal a wrong commit behind our back
			t.Logf("late tab_new: result=%v error=%v; Target.getTargets sent by the daemon on its own since the conflict=%d",
				r.Result, r.Error, e.proxy.wire().getTargets-sent)
			rcRefused(t, r)
			rcAssertDead(t, e, a)
		})
	}
}

// P1-3 on the production wiring. The lifecycle consumer is parked, the queue
// is filled with real notifications, tab_new(A) is in flight with its reply
// delayed, and the user closes A: both notifications about A are dropped by
// the full queue. Nothing is injected; discovery is the daemon's own.
func TestRealChromeDroppedDestroyDuringCreate(t *testing.T) {
	e := rcStartPatient(t)
	parked := e.park()
	sess := e.session()
	full := func() bool { return len(sess.TargetEvents()) == cap(sess.TargetEvents()) }
	e.chrome.flood(full)
	create := e.proxy.holdNext("Target.createTarget")
	newTab := e.rpcAsync("tab_new", nil)
	e.proxy.awaitHeld(create, "creation")
	a := rcCreatedID(t, create)
	e.chrome.userClose(a)
	// Forwarded to the daemon before the delayed creation reply is: the reader
	// handles it first.
	rcAwait(t, "destroyed notification forwarded", func() bool { return e.proxy.wasDelivered("Target.targetDestroyed", a) })
	if !full() {
		t.Fatal("precondition: the queue must still be full, so the notifications about A were dropped")
	}
	create.Release()
	r := <-newTab
	parked.Release()
	time.Sleep(rcQuiet)
	t.Logf("late tab_new with dropped notifications: result=%v error=%v", r.Result, r.Error)
	rcRefused(t, r)
	rcAssertDead(t, e, a)
}

// P1-4 on the production wiring. targetCreated(A) is read while the consumer
// is parked; the user closes A and the proxy delays the destroyed
// notification; a list that does not contain A is requested afterwards; the
// consumer then handles the old event. A must not outlive that list.
func TestRealChromeQueuedCreatedEventDoesNotOutliveAbsentList(t *testing.T) {
	e := rcStartPatient(t)
	parked := e.park()
	sess := e.session()
	e.proxy.withholdNotifications(func(method string, _ target.ID) bool { return method == "Target.targetDestroyed" })
	queued := len(sess.TargetEvents())
	a := e.chrome.userOpen()
	rcAwait(t, "created notification read and queued", func() bool {
		return e.proxy.wasDelivered("Target.targetCreated", a) && len(sess.TargetEvents()) > queued
	})
	e.chrome.userClose(a) // its destroyed notification is delayed
	absent := e.proxy.holdNext("Target.getTargets")
	list := e.rpcAsync("tab_list", nil)
	e.proxy.awaitHeld(absent, "absent list")
	parked.Release() // the consumer gets to the old event now
	rcAwait(t, "queue handled", func() bool { return len(sess.TargetEvents()) == 0 })
	mid := e.srv.tabs.Has(a)
	absent.Release()
	r := <-list
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	rows, _ := r.Result["tabs"].([]any)
	inList := false
	for _, row := range rows {
		inList = inList || row.(map[string]any)["tab"] == string(a)
	}
	after := e.srv.tabs.Has(a)
	t.Logf("old created(A) handled while the absent list was in flight: A resolved then=%v; in the list=%v; resolves after the list=%v", mid, inList, after)
	if inList || after {
		t.Error("A outlived a list requested after the only evidence of its existence was read")
	}
	e.proxy.deliverWithheld()
	rcAwait(t, "A forgotten", func() bool { return !e.srv.tabs.Known(a) })
	rcAssertDead(t, e, a)
}

// P2 on the production wiring. A B-only list is delayed, the notifications
// about a new live tab A are dropped by the full queue, and the daemon's
// compensating enumeration (which shows A) is applied after the B-only reply.
// The live tab must come back without any RPC, and nothing may be closed.
func TestRealChromeReverseConflictRecoversWithoutRPC(t *testing.T) {
	e := rcStartPatient(t)
	parked := e.park()
	sess := e.session()
	absent := e.proxy.holdNext("Target.getTargets")
	list := e.rpcAsync("tab_list", nil)
	e.proxy.awaitHeld(absent, "absent list")
	full := func() bool { return len(sess.TargetEvents()) == cap(sess.TargetEvents()) }
	e.chrome.flood(full)
	a := e.chrome.userOpen()
	rcAwait(t, "created notification forwarded", func() bool { return e.proxy.wasDelivered("Target.targetCreated", a) })
	if !full() {
		t.Fatal("precondition: the queue must still be full, so the notification about A was dropped")
	}
	comp := e.proxy.holdNext("Target.getTargets")
	parked.Release() // the consumer drains, sees the overflow and enumerates
	e.proxy.awaitHeld(comp, "compensating enumeration")
	sent := e.proxy.wire().getTargets
	absent.Release()
	if r := <-list; r.Error != nil {
		t.Fatal(r.Error)
	}
	comp.Release()
	start := time.Now()
	for !e.srv.tabs.Has(a) && time.Since(start) < rcQuiet {
		time.Sleep(5 * time.Millisecond)
	}
	took := time.Since(start)
	w := e.proxy.wire()
	pages := e.chrome.pages()
	again := e.rpc("eval", map[string]any{"tab": string(a), "script": "1"})
	t.Logf("A=%s resolves=%v after %v without RPC; Target.getTargets sent by the daemon since the conflict=%d; Chrome A=%v B=%v; wire closeTarget=%v; eval error=%v",
		a, e.srv.tabs.Has(a), took.Round(time.Millisecond), w.getTargets-sent, pages[a], pages[e.b], w.closes, again.Error)
	if !e.srv.tabs.Has(a) || again.Error != nil {
		t.Error("the live tab did not come back after the conflict without an RPC")
	}
	if !pages[a] || !pages[e.b] || len(w.closes) != 0 {
		t.Error("something was closed")
	}
}
