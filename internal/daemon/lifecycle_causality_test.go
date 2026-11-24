package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/yiplee/go-bb-browser/internal/store"
)

// Regressions for the causal ordering of lifecycle evidence (review of round
// 6: four P1 and one P2, plus the event wiring gap found on real Chrome).
//
// Unless stated otherwise they run the real chromedp connection reader against
// the loopback CDP stub, connected the way the production daemon connects
// (causalConnect). The stub delivers target lifecycle notifications only
// because that connection asked for them at the root level, and "lost"
// notifications are produced by not sending them (the tab was closed in the
// browser while nobody was told), never by handing the daemon something its
// production connection would not get.
//
// A is opened once, its id is never reused; B shares A's suffix and must stay
// untouched throughout.

// causalNoClose fails the test if the daemon sent Target.closeTarget for a
// target other than those listed in allowed, or if B is gone.
func causalNoClose(t *testing.T, st *cdpStub, allowed ...target.ID) {
	t.Helper()
	for _, id := range st.closedTargets() {
		if !containsTarget(allowed, id) {
			t.Errorf("the daemon sent Target.closeTarget(%s); all closes on the wire: %v", id, st.closedTargets())
		}
	}
	if !st.hasPage(ovfB) {
		t.Error("B is gone from the browser")
	}
}

// causalAssertGone asserts that the dead tab A is refused everywhere a tab id
// is accepted, is neither idle-tracked nor persisted as managed (replaying the
// rpc.jsonl in dir), and that B still resolves. It closes the audit log.
func causalAssertGone(t *testing.T, s *Server, dir string) {
	t.Helper()
	ovfClearsRefuse(t, s, string(ovfA))
	if s.tabs.Has(ovfA) || causalKnown(s, ovfA) || s.tabs.Selected() == ovfA {
		t.Errorf("dead A is still in the registry (resolves=%v)", s.tabs.Has(ovfA))
	}
	if _, ok := s.tabs.Lookup(string(ovfB)); !ok {
		t.Error("B no longer resolves")
	}
	_, idle := s.tabIdle.Snapshot()[ovfA]
	if err := s.closeAudit(); err != nil {
		t.Fatal(err)
	}
	replay, err := store.Open(store.OpenConfig{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Close()
	if managed := managedTargetSet(replay.ManagedTabs())[ovfA]; idle || managed {
		t.Errorf("dead A kept as a managed tab: idle tracking=%v, replayed rpc.jsonl=%v", idle, managed)
	}
}

func causalRefusedCreate(t *testing.T, r enumRPCReply) {
	t.Helper()
	if r.Error == nil {
		t.Errorf("tab_new succeeded for a tab that was gone before its reply was delivered: %v", r.Result)
		return
	}
	if msg, _ := r.Error["message"].(string); !strings.Contains(msg, "tab closed before tab_new completed") {
		t.Errorf("tab_new error = %v, want the creation-refused error", r.Error)
	}
}

// P1-1. Two read-only requests disagree about a tab the user just opened: an
// older Target.getTargets reply (B only) is still in flight when another
// request discovers A and eval attaches a long-lived chromedp context to it.
// Releasing the browser attachment of A — which is what applying the older
// reply as a removal did — makes chromedp send Target.closeTarget(A): a
// read-only conflict closed a live user page.
//
// The conflict may only stop A from resolving until the browser is asked
// again; nothing of A may be released and no close may reach the wire.
func TestOverlappingListDoesNotCloseAttachedLivePage(t *testing.T) {
	for _, tc := range []struct {
		name string
		// announce: the browser reports the new tab (targetCreated).
		announce bool
		// listFirst: a tab_list discovers A before eval; otherwise eval's own
		// enumeration does (one tab_list in flight plus one eval is enough).
		listFirst bool
		// loop: the production sync loop is running; the tab must come back
		// without any further RPC.
		loop bool
	}{
		{name: "two lists and eval, notification queued", announce: true, listFirst: true},
		{name: "two lists and eval, no notification", listFirst: true},
		{name: "one list and eval, no notification, sync loop recovers", loop: true},
		{name: "one list and eval, sync loop running", announce: true, loop: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newCDPStub(t, ovfB)
			s := newStubSessionServer(t, st)
			defer s.closeAudit()
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			sess := causalConnect(t, ctx, s, st)
			s.tabHook = sess
			ovfListed(t, s, ovfB)
			if tc.loop {
				done := make(chan struct{})
				go func() { defer close(done); s.runTargetSync(ctx, sess) }()
				defer func() { cancel(); <-done }()
			}

			n := st.getTargetsCalls()
			gate := st.holdNextGetTargets()
			defer closeOnce(gate)
			old := ovfAsyncRPC(s, "tab_list")
			awaitGetTargets(t, st, n+1) // snapshot: B only, held
			st.addPage(ovfA, tc.announce)
			if err := sess.PingBrowserContext(ctx); err != nil {
				t.Fatal(err)
			}
			if tc.listFirst {
				ovfListed(t, s, ovfA)
			}
			// Attaches a pooled chromedp context to A.
			enumMustRPC(t, s, "eval", map[string]any{"tab": string(ovfA), "script": "1"})
			if !st.hasPage(ovfA) || len(st.closedTargets()) != 0 {
				t.Fatal("precondition: nothing closed before the older reply is applied")
			}

			closeOnce(gate)
			if r := <-old; r.Error != nil {
				t.Fatal(r.Error)
			}
			if tc.loop {
				// No RPC: the daemon asks the browser again by itself.
				ovfAwait(t, ctx, "A resolves again without any RPC", func() bool { return s.tabs.Has(ovfA) })
			} else if !ovfRowsContain(enumRPCReply{Result: enumMustRPC(t, s, "tab_list", nil)}, ovfA) {
				t.Error("the enumeration requested after the conflict does not list the live A")
			}
			// The same attachment still works.
			if r, err := enumRPC(s, "eval", map[string]any{"tab": string(ovfA), "script": "1"}); err != nil || r.Error != nil {
				t.Errorf("eval on the live A after the conflict: %v %v", r.Error, err)
			}
			if !st.hasPage(ovfA) {
				t.Error("an overlapping read-only request closed a live user page")
			}
			causalNoClose(t, st)
		})
	}
}

// causalParkedCreator delays the delivery of a successful creation reply of
// the fake browser.
type causalParkedCreator struct {
	*enumConn
	made, deliver chan struct{}
}

func (c *causalParkedCreator) CreatePageTarget(url string, silent bool) (target.ID, error) {
	id, err := c.enumConn.CreatePageTarget(url, silent)
	close(c.made)
	<-c.deliver
	return id, err
}

// P1-2 (fake browser, no sync loop). tab_new(A) and an early B-only list are
// in flight. Another list discovers A. A is closed in the browser, the early
// list is applied (the two snapshots contradict each other), and then:
//
//   - "confirmed first": an independent NEW list confirms that A is gone, and
//     only then is the creation reply delivered;
//   - "reply first": the creation reply is delivered right away, before
//     anything asked the browser again.
//
// No destroyed notification ever arrives. Either way tab_new must not hand out
// the dead tab: not in the registry, not in idle tracking, not on disk.
func TestLateCreateAfterContradictedDiscoveryIsRefused(t *testing.T) {
	for _, confirmFirst := range []bool{true, false} {
		name := "reply first"
		if confirmFirst {
			name = "confirmed first"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			c := &causalParkedCreator{newEnumConn(ovfB), make(chan struct{}), make(chan struct{})}
			c.nextIDs = []target.ID{ovfA}
			s := newReconcileRaceServer(t, c.enumConn, dir, 0)
			s.tabHook = c
			ovfListed(t, s, ovfB)
			entered, release := c.holdNextSnapshot()
			defer release()
			old := ovfAsyncRPC(s, "tab_list")
			enumAwait(t, entered, "early B-only snapshot")
			created := ovfAsyncRPC(s, "tab_new")
			defer closeOnce(c.deliver)
			enumAwait(t, c.made, "A created, reply parked")
			ovfListed(t, s, ovfA) // another caller discovers A
			c.removeExternally(ovfA)
			release()
			if r := <-old; r.Error != nil {
				t.Fatal(r.Error)
			}
			if confirmFirst {
				fresh := enumMustRPC(t, s, "tab_list", nil)
				if rows := fresh["tabs"].([]any); len(rows) != 1 || c.has(ovfA) || !c.has(ovfB) {
					t.Fatalf("precondition: the independent list confirms B only, got %v", rows)
				}
			}
			closeOnce(c.deliver)
			causalRefusedCreate(t, <-created)
			if !c.has(ovfB) {
				t.Error("B lost")
			}
			causalAssertGone(t, s, dir)
		})
	}
}

// P1-2 in the timing that persisted on a production daemon: the creation reply
// arrives after the enumeration the conflict made the sync loop request by
// itself. Real session, production sync loop, no RPC between the conflict and
// the late reply. Before the fix nothing recorded that A had been seen and
// found gone, so the late reply registered the dead tab for good.
func TestLateCreateAfterAutomaticReenumerationIsRefused(t *testing.T) {
	dir := t.TempDir()
	st := newCDPStub(t, ovfB)
	s := newReconcileRaceServer(t, newEnumConn(), dir, 0)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	sess := causalConnect(t, ctx, s, st)
	c := &ovfHeldCreate{Session: sess, made: make(chan target.ID, 1), release: make(chan struct{})}
	defer closeOnce(c.release)
	s.tabHook = c
	s.cfg.CDPWatchdogTimeout = 10 * time.Second
	ovfListed(t, s, ovfB)
	done := make(chan struct{})
	go func() { defer close(done); s.runTargetSync(ctx, sess) }()
	defer func() { cancel(); <-done }()

	n := st.getTargetsCalls()
	gate := st.holdNextGetTargets()
	defer closeOnce(gate)
	old := ovfAsyncRPC(s, "tab_list")
	awaitGetTargets(t, st, n+1) // early snapshot: B only, held
	st.queueCreateID(ovfA)
	created := ovfAsyncRPC(s, "tab_new")
	select {
	case id := <-c.made:
		if id != ovfA {
			t.Fatalf("created %q, want %q", id, ovfA)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	ovfListed(t, s, ovfA)      // #n+2: another caller discovers A
	st.removePage(ovfA, false) // closed in the browser; nobody is told
	closeOnce(gate)            // the early list contradicts the discovery
	if r := <-old; r.Error != nil {
		t.Fatal(r.Error)
	}
	// The sync loop asks again on its own (#n+3) and finds A gone.
	awaitGetTargets(t, st, n+3)
	ovfAwait(t, ctx, "the automatic enumeration forgot A", func() bool { return !causalKnown(s, ovfA) })

	closeOnce(c.release)
	causalRefusedCreate(t, <-created)
	causalNoClose(t, st, ovfA) // at most the bounded rollback close of exactly A
	causalAssertGone(t, s, dir)
}

// P1-3. The lifecycle queue is full while tab_new(A) is in flight, A is closed
// in the browser and its destroyed notification is dropped. The reader used to
// publish the overflow first and record what the notification said afterwards:
// the sync loop could open its compensating enumeration in between, the
// creation committed, and the only compensating reply then kept the dead A as
// "created while I was in flight".
//
// The reader is parked exactly while it handles the dropped notification. At
// that point the creation in flight must already be constrained, and the
// overflow must still lead to an enumeration afterwards.
func TestDroppedDestroyIsRecordedBeforeOverflowIsPublished(t *testing.T) {
	dir := t.TempDir()
	st := newCDPStub(t, ovfB, ovfMarker)
	s := newReconcileRaceServer(t, newEnumConn(), dir, 0)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	sess, enter, resume := causalConnectWatchingDrop(t, ctx, s, st, ovfA)
	defer closeOnce(resume)
	c := &ovfHeldCreate{Session: sess, made: make(chan target.ID, 1), release: make(chan struct{})}
	defer closeOnce(c.release)
	s.tabHook = c
	s.cfg.CDPWatchdogTimeout = 10 * time.Second
	ovfListed(t, s, ovfMarker)
	// The consumer is not running: the queue is exactly full of events about
	// B, nothing has been lost so far.
	ovfFillQueue(t, ctx, st, sess)

	st.queueCreateID(ovfA)
	created := ovfAsyncRPC(s, "tab_new")
	select {
	case id := <-c.made:
		if id != ovfA {
			t.Fatalf("created %q, want %q", id, ovfA)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	n := st.getTargetsCalls()
	gate := st.holdNextGetTargets()
	defer closeOnce(gate)
	st.removePage(ovfA, true) // closed in the browser; the notification finds the queue full
	select {
	case <-enter:
	case <-ctx.Done():
		t.Fatal("the reader never reported the dropped destroyed notification")
	}

	done := make(chan struct{})
	go func() { defer close(done); s.runTargetSync(ctx, sess) }()
	defer func() { closeOnce(resume); closeOnce(gate); cancel(); <-done }()
	// Give the loop every chance to act on an overflow published too early: it
	// drains the queue and looks at the dirty flag right after.
	ovfAwait(t, ctx, "queue drained", func() bool { return len(sess.TargetEvents()) == 0 })
	for end := time.Now().Add(300 * time.Millisecond); time.Now().Before(end) && s.tabs.OpenWindows() < 2; {
		time.Sleep(time.Millisecond)
	}

	// Deliver the creation reply while the reader is still parked, and let the
	// reader go on only once the commit has been decided (the creation window
	// ended); the reply to tab_new itself needs the reader (rollback).
	open := s.tabs.OpenWindows()
	closeOnce(c.release)
	ovfAwait(t, ctx, "creation commit decided", func() bool { return s.tabs.OpenWindows() < open })
	closeOnce(resume)
	r := <-created
	// Every dropped event leaves an enumeration behind it.
	awaitGetTargets(t, st, n+1)
	// Witness that the compensating reply was applied: the marker is closed
	// behind it in the queue and forgotten only after it.
	st.removePage(ovfMarker, true)
	if err := sess.PingBrowserContext(ctx); err != nil {
		t.Fatal(err)
	}
	closeOnce(gate)
	ovfAwait(t, ctx, "marker forgotten", func() bool { return !causalKnown(s, ovfMarker) })

	if st.hasPage(ovfA) {
		t.Fatal("bad stub state: A still open")
	}
	causalRefusedCreate(t, r)
	causalNoClose(t, st, ovfA)
	causalAssertGone(t, s, dir)
}

// P1-4. targetCreated(A) is read from the connection strictly BEFORE the
// request of a list that will not contain A (A was closed in between, its
// destroyed notification has not arrived), but the queue consumer handles the
// event only while that list is in flight. Handling time used to give A a
// registration newer than the request, which shielded it from the absence.
// The event is evidence about the instant it was read: the list is newer.
func TestEventReadBeforeAbsentListDoesNotOutliveIt(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	st := newCDPStub(t, ovfB, ovfMarker)
	s := newStubSessionServer(t, st)
	defer s.closeAudit()
	sess := causalConnect(t, ctx, s, st)
	s.tabHook = sess
	ovfListed(t, s, ovfMarker)

	st.addPage(ovfA, true) // targetCreated(A): read and queued, not handled
	if err := sess.PingBrowserContext(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sess.TargetEvents()) == 0 {
		t.Fatal("precondition: the creation event must be queued")
	}
	st.removePage(ovfA, false) // closed by the user; the notification is not delivered
	n := st.getTargetsCalls()
	gate := st.holdNextGetTargets()
	defer closeOnce(gate)
	absent := ovfAsyncRPC(s, "tab_list")
	awaitGetTargets(t, st, n+1) // requested after the event was read; B + marker, held
	// The marker's destruction is queued behind A's event: once the marker is
	// forgotten, A's event has been handled.
	st.removePage(ovfMarker, true)
	done := make(chan struct{})
	go func() { defer close(done); s.runTargetSync(ctx, sess) }()
	defer func() { cancel(); <-done }()
	ovfAwait(t, ctx, "marker forgotten", func() bool { return !causalKnown(s, ovfMarker) })
	t.Logf("old created(A) handled while the list is in flight: A resolves=%v", s.tabs.Has(ovfA))

	closeOnce(gate)
	if r := <-absent; r.Error != nil {
		t.Fatal(r.Error)
	} else if ovfRowsContain(r, ovfA) {
		t.Error("the list that did not contain A returned it")
	}
	if s.tabs.Has(ovfA) || causalKnown(s, ovfA) {
		t.Error("A outlived a list requested after the only evidence of its existence was read")
	}
	ovfClearsRefuse(t, s, string(ovfA))
	if _, ok := s.tabs.Lookup(string(ovfB)); !ok {
		t.Error("B no longer resolves")
	}
	causalNoClose(t, st)
}

// The protection that must survive P1-4: an event read AFTER the list was
// requested is not older than it. The tab stays, and the browser is asked
// again because the two cannot be ordered.
func TestEventReadAfterListRequestKeepsNewTab(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	st := newCDPStub(t, ovfB)
	s := newStubSessionServer(t, st)
	defer s.closeAudit()
	sess := causalConnect(t, ctx, s, st)
	s.tabHook = sess
	ovfListed(t, s, ovfB)
	done := make(chan struct{})
	go func() { defer close(done); s.runTargetSync(ctx, sess) }()
	defer func() { cancel(); <-done }()

	n := st.getTargetsCalls()
	gate := st.holdNextGetTargets()
	defer closeOnce(gate)
	stale := ovfAsyncRPC(s, "tab_list")
	awaitGetTargets(t, st, n+1) // snapshot: B only, held
	st.addPage(ovfA, true)      // opened afterwards; the event registers it
	ovfAwait(t, ctx, "the event registered A", func() bool { return s.tabs.Has(ovfA) })
	closeOnce(gate)
	if r := <-stale; r.Error != nil {
		t.Fatal(r.Error)
	}
	if !s.tabs.Has(ovfA) {
		t.Fatal("a tab opened after the list was requested was removed by its older snapshot")
	}
	awaitGetTargets(t, st, n+2) // the unordered absence is settled by asking again
	ovfAwait(t, ctx, "A still resolves after the verification", func() bool {
		return st.getTargetsCalls() >= n+2 && s.tabs.OpenWindows() == 0 && s.tabs.Has(ovfA)
	})
	causalNoClose(t, st)
}

// P2. The contradiction in the other application order: a B-only list is in
// flight, the only notification about a new live tab A is dropped by the full
// queue, and the compensating enumeration (which lists A) is applied AFTER the
// B-only reply. The daemon refused A — it cannot order the two snapshots — but
// asked nobody again: A stayed unknown until some later RPC happened to
// enumerate. Both application orders must request the verification themselves.
func TestReverseSnapshotConflictRecoversWithoutRPC(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	st := newCDPStub(t, ovfB)
	s := newStubSessionServer(t, st)
	defer s.closeAudit()
	sess := causalConnect(t, ctx, s, st)
	s.tabHook = sess
	ovfListed(t, s, ovfB)
	n := st.getTargetsCalls()
	first, second := st.holdNextGetTargets(), st.holdNextGetTargets()
	defer closeOnce(first)
	defer closeOnce(second)
	absent := ovfAsyncRPC(s, "tab_list")
	awaitGetTargets(t, st, n+1) // snapshot: B only, held
	ovfFillQueue(t, ctx, st, sess)
	st.addPage(ovfA, true) // the only notification about A: dropped
	if err := sess.PingBrowserContext(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sess.TargetEvents()) != cap(sess.TargetEvents()) {
		t.Fatal("precondition: the queue must be full")
	}
	done := make(chan struct{})
	go func() { defer close(done); s.runTargetSync(ctx, sess) }()
	defer func() { closeOnce(second); cancel(); <-done }()
	awaitGetTargets(t, st, n+2) // compensating snapshot: A + B, held

	closeOnce(first)
	if r := <-absent; r.Error != nil {
		t.Fatal(r.Error)
	}
	closeOnce(second)
	// No RPC from here on. Two sync ticks are allowed, as in the diagnostic.
	end := time.Now().Add(2150 * time.Millisecond)
	for !s.tabs.Has(ovfA) && time.Now().Before(end) {
		time.Sleep(5 * time.Millisecond)
	}
	t.Logf("after the reverse conflict: browser A=%v, A resolves=%v, Target.getTargets sent=%d (was %d)",
		st.hasPage(ovfA), s.tabs.Has(ovfA), st.getTargetsCalls(), n)
	if !s.tabs.Has(ovfA) {
		t.Error("the live A did not come back after the conflict without an RPC")
	}
	if st.getTargetsCalls() < n+3 {
		t.Error("no enumeration was requested after the conflict")
	}
	if !st.hasPage(ovfA) {
		t.Error("A was closed")
	}
	causalNoClose(t, st)
}

// Production wiring. The daemon's own connection must turn the browser's
// target lifecycle notifications on: chromedp attaches the daemon to one page
// and only enables discovery on that page session, whose notifications never
// reach the browser listener. Without a root-level Target.setDiscoverTargets
// the daemon is told nothing, and a tab the user closes stays registered,
// idle-tracked and persisted until some RPC happens to enumerate.
//
// connectBrowser is the production connect path; the stub only sends
// lifecycle notifications to a client that asked for them at the root level.
func TestProductionConnectionLearnsOfClosedTabWithoutRPC(t *testing.T) {
	dir := t.TempDir()
	st := newCDPStub(t, ovfB)
	s := newReconcileRaceServer(t, newEnumConn(), dir, 0)
	s.tabHook = nil
	s.cfg.DebuggerURL = st.url()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err := s.connectBrowser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n := st.rootDiscoverRequests(); n != 1 {
		t.Fatalf("the production connection sent %d root-level Target.setDiscoverTargets, want 1", n)
	}

	st.queueCreateID(ovfA)
	if tab := enumTabNew(t, s); tab != string(ovfA) {
		t.Fatalf("tab_new returned %q", tab)
	}
	enumMustRPC(t, s, "eval", map[string]any{"tab": string(ovfA), "script": "1"})
	if _, idle := s.tabIdle.Snapshot()[ovfA]; !idle || !s.tabs.Has(ovfA) {
		t.Fatal("precondition: A is a registered, managed tab")
	}
	n := st.getTargetsCalls()

	st.removePage(ovfA, true) // the user closes the tab; no RPC follows
	deadline := time.Now().Add(3 * time.Second)
	for causalKnown(s, ovfA) && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if causalKnown(s, ovfA) {
		t.Error("a tab closed in the browser is still registered: the connection receives no lifecycle notifications")
	}
	if got := st.getTargetsCalls(); got != n {
		t.Errorf("Target.getTargets sent %d times after the close: the cleanup must come from the notification", got-n)
	}
	causalNoClose(t, st, ovfA) // releasing the attachment of the gone tab may repeat its close
	causalAssertGone(t, s, dir)
}
