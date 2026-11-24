package daemon

import (
	"errors"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
)

// Schedules of the FT28 round-9 review. The first group uses the production
// wiring (real Session reader, registry as its observer, root-level discovery
// enabled by the connection itself, the real queue consumer and the HTTP
// handlers) against the CDP stub, which only sends lifecycle notifications
// because the daemon asked for them. A is created once and its id is never
// reused; B shares A's suffix and must stay untouched. A "lost" destroyed
// notification is produced by not sending it.

func announcedAssertNeverRegistered(t *testing.T, s *Server, r enumRPCReply) {
	t.Helper()
	_, idle := s.tabIdle.Snapshot()[ovfA]
	t.Logf("tab_new result=%v error=%v; A known=%v resolves=%v idle=%v selected=%v", r.Result, r.Error, s.tabs.Known(ovfA), s.tabs.Has(ovfA), idle, s.tabs.Selected() == ovfA)
	causalRefusedCreate(t, r)
	if s.tabs.Known(ovfA) || idle || s.tabs.Selected() == ovfA {
		t.Errorf("dead A kept: known=%v idle=%v selected=%v", s.tabs.Known(ovfA), idle, s.tabs.Selected() == ovfA)
	}
}

// P1. The existence of A is proven by its creation notification, read from the
// connection while A is not registered yet (the creation reply is still on its
// way and the queue consumer is busy). A is then closed in the browser, and a
// list requested AFTER the notification was read confirms that only B is left
// — applied while A is still not registered. That list is newer than the
// proof, so A is gone: the creation reply that arrives afterwards must not
// register it, tab_new must not succeed, and no request may ever resolve A,
// whichever of the queued event and the reply the daemon gets to first and
// whether or not it can ask the browser again.
func TestAbsentListAppliedBeforeRegistrationSettlesAnnouncedTarget(t *testing.T) {
	// The reviewer's schedule: the consumer is parked in an enumeration of
	// its own, so created(A) is still queued when the absent list is applied
	// and when the creation reply is delivered.
	t.Run("closed, creation event still queued", func(t *testing.T) {
		s, st, sess, c, ctx, dir := knownTargetEnv(t)
		n := st.getTargetsCalls()
		park := st.holdNextGetTargets()
		defer closeOnce(park)
		knownTargetLoop(t, ctx, s, sess)
		sess.MarkTargetDirty()
		st.emitInfoChanged(ovfB) // wakes the consumer; it then asks for its enumeration
		awaitGetTargets(t, st, n+1)

		newTab := knownTargetCreate(t, ctx, s, st, c, sess) // created(A) read and queued
		st.removePage(ovfA, false)                          // destroyed notification lost
		if s.tabs.Known(ovfA) || len(sess.TargetEvents()) < 1 || s.tabs.PendingAnnounced() != 1 {
			t.Fatalf("precondition: A announced and unregistered, its event queued (known=%v queued=%d announced=%d)",
				s.tabs.Known(ovfA), len(sess.TargetEvents()), s.tabs.PendingAnnounced())
		}
		absent, err := enumRPC(s, "tab_list", nil) // requested after created(A) was read
		if err != nil || absent.Error != nil {
			t.Fatalf("tab_list: %v / %v", err, absent.Error)
		}
		if ovfRowsContain(absent, ovfA) || s.tabs.Known(ovfA) || len(sess.TargetEvents()) < 1 {
			t.Fatal("precondition: the list without A is applied while A is unregistered and its event is still queued")
		}
		if got := s.tabs.PendingAnnounced(); got != 0 {
			t.Errorf("announcements after the list that answered them = %d, want 0", got)
		}
		sent := st.getTargetsCalls()
		closeOnce(c.release)
		r := <-newTab
		// Nothing was parked for a verification: had the commit registered A,
		// tab_new would have asked the browser again before answering.
		if extra := st.getTargetsCalls() - sent; extra != 0 {
			t.Errorf("tab_new sent %d more Target.getTargets: the applied list had already settled A", extra)
		}
		announcedAssertNeverRegistered(t, s, r)
		ovfClearsRefuse(t, s, string(ovfA)) // unknown tab id, from enumerations that succeed

		closeOnce(park) // the consumer applies its old reply and gets to created(A)
		ovfAwait(t, ctx, "queue handled and every window closed", func() bool {
			return len(sess.TargetEvents()) == 0 && s.tabs.OpenWindows() == 0 && s.tabs.PendingAnnounced() == 0
		})
		s.lifeMu.Lock()
		known := s.tabs.Known(ovfA)
		s.lifeMu.Unlock()
		if known {
			t.Error("the queued creation event registered the dead tab")
		}
		ovfClearsRefuse(t, s, string(ovfA))
		if _, ok := s.tabs.Lookup(string(ovfB)); !ok {
			t.Error("B no longer resolves")
		}
		if knownTargetManagedOnDisk(t, s, dir) {
			t.Error("dead A is replayed as a managed tab")
		}
		causalNoClose(t, st, ovfA) // only the rollback of the refused creation
	})

	// The consumer got to created(A) first and could not register A from it
	// (a close of another tab was confirmed after the event was read). The
	// proof must survive that refusal: the list that follows is still newer
	// than it.
	t.Run("closed, creation event handled and refused first", func(t *testing.T) {
		s, st, sess, c, ctx, dir := knownTargetEnv(t)
		st.addPage(ovfMarker, false)
		marker := ovfListed(t, s, ovfMarker)

		newTab := knownTargetCreate(t, ctx, s, st, c, sess)           // created(A) read and queued
		enumMustRPC(t, s, "tab_close", map[string]any{"tab": marker}) // newer than every queued event
		st.removePage(ovfA, false)                                    // destroyed notification lost
		n := st.getTargetsCalls()
		auto := st.holdNextGetTargets()
		defer closeOnce(auto)
		knownTargetLoop(t, ctx, s, sess)
		awaitGetTargets(t, st, n+1) // the enumeration the refused event requested, held
		if len(sess.TargetEvents()) != 0 || s.tabs.Known(ovfA) {
			t.Fatalf("precondition: created(A) handled and refused (queued=%d known=%v)", len(sess.TargetEvents()), s.tabs.Known(ovfA))
		}
		if got := s.tabs.PendingAnnounced(); got != 1 {
			t.Fatalf("announcements after the refusal = %d, want the proof about A kept", got)
		}
		absent := enumMustRPC(t, s, "tab_list", nil)
		if ovfRowsContain(enumRPCReply{Result: absent}, ovfA) || s.tabs.PendingAnnounced() != 0 {
			t.Fatalf("precondition: the list without A answered the announcement (announced=%d)", s.tabs.PendingAnnounced())
		}
		sent := st.getTargetsCalls()
		closeOnce(c.release)
		r := <-newTab
		if extra := st.getTargetsCalls() - sent; extra != 0 {
			t.Errorf("tab_new sent %d more Target.getTargets: the applied list had already settled A", extra)
		}
		announcedAssertNeverRegistered(t, s, r)
		closeOnce(auto)
		ovfAwait(t, ctx, "every window closed", func() bool { return s.tabs.OpenWindows() == 0 })
		ovfClearsRefuse(t, s, string(ovfA))
		if _, ok := s.tabs.Lookup(string(ovfB)); !ok {
			t.Error("B no longer resolves")
		}
		if knownTargetManagedOnDisk(t, s, dir) {
			t.Error("dead A is replayed as a managed tab")
		}
		causalNoClose(t, st, ovfA, ovfMarker)
	})

	// Live control: the list was requested BEFORE A was created, so the
	// notification is newer than its snapshot and proves nothing gone. The
	// creation is committed, verified with the browser, and survives the
	// queued event.
	t.Run("live control: list requested before the tab was created", func(t *testing.T) {
		s, st, sess, c, ctx, dir := knownTargetEnv(t)
		n := st.getTargetsCalls()
		early := st.holdNextGetTargets()
		defer closeOnce(early)
		old := ovfAsyncRPC(s, "tab_list")
		awaitGetTargets(t, st, n+1) // snapshot without A, reply held

		newTab := knownTargetCreate(t, ctx, s, st, c, sess) // created(A) read and queued
		closeOnce(early)
		if r := <-old; r.Error != nil {
			t.Fatal(r.Error)
		} else if ovfRowsContain(r, ovfA) {
			t.Fatal("precondition: the old list must not contain A")
		}
		if s.tabs.Known(ovfA) || s.tabs.PendingAnnounced() != 1 {
			t.Fatalf("precondition: A unregistered, its announcement not answered by the older list (known=%v announced=%d)",
				s.tabs.Known(ovfA), s.tabs.PendingAnnounced())
		}
		sent := st.getTargetsCalls()
		closeOnce(c.release)
		r := <-newTab
		synchronous := st.getTargetsCalls() - sent
		_, idle := s.tabIdle.Snapshot()[ovfA]
		t.Logf("tab_new result=%v error=%v; Target.getTargets sent before the answer=%d; A resolves=%v idle=%v selected=%v",
			r.Result, r.Error, synchronous, s.tabs.Has(ovfA), idle, s.tabs.Selected() == ovfA)
		if r.Error != nil || r.Result["tab"] != string(ovfA) {
			t.Fatalf("the live tab was refused: %v / %v", r.Result, r.Error)
		}
		if synchronous < 1 {
			t.Error("tab_new answered without asking the browser about the contradicting list")
		}
		knownTargetLoop(t, ctx, s, sess)
		ovfAwait(t, ctx, "queue handled", func() bool {
			return len(sess.TargetEvents()) == 0 && s.tabs.OpenWindows() == 0
		})
		_, idle = s.tabIdle.Snapshot()[ovfA]
		if !s.tabs.Has(ovfA) || !idle || s.tabs.Selected() != ovfA || !st.hasPage(ovfA) || s.tabs.PendingAnnounced() != 0 {
			t.Errorf("live A after its queued event: resolves=%v idle=%v selected=%v browser=%v announced=%d",
				s.tabs.Has(ovfA), idle, s.tabs.Selected() == ovfA, st.hasPage(ovfA), s.tabs.PendingAnnounced())
		}
		if !knownTargetManagedOnDisk(t, s, dir) {
			t.Error("live A is not replayed as a managed tab")
		}
		causalNoClose(t, st)
	})
}

// P2 (commitCreatedTab). While tab_new verifies a contested creation, another
// caller closes A successfully: the daemon retires A. A list requested after
// that close still shows the closing target, so A is registered again — as a
// discovery, not as the managed tab this tab_new created — and then A is
// really gone. Whether the verification succeeds or fails, the creation was
// reported gone before it ended and tab_new must not answer with A just
// because the id resolves again.
func TestCreateVerificationRemembersCloseDespiteRediscovery(t *testing.T) {
	for _, tc := range []struct {
		name           string
		failed, closed bool
	}{
		{"closed, verification succeeds", false, true},
		{"closed, verification fails", true, true},
		{"live control, verification succeeds", false, false},
		{"live control, verification fails", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &verifyConn{enumConn: newEnumConn(ovfB), made: make(chan struct{}), release: make(chan struct{})}
			c.nextIDs = []target.ID{ovfA}
			c.closeLingers = true
			s := newEnumServer(t, c.enumConn)
			defer s.closeAudit()
			s.tabHook = c
			started, release := c.holdNextSnapshot()
			defer release()
			old := ovfAsyncRPC(s, "tab_list")
			enumAwait(t, started, "B-only snapshot")
			newTab := ovfAsyncRPC(s, "tab_new")
			enumAwait(t, c.made, "A created once")
			release()
			if r := <-old; r.Error != nil {
				t.Fatal(r.Error)
			}
			if tc.failed {
				c.setListErr(errors.New("transient verification failure"))
			}
			verifying, finish := c.holdNextSnapshot()
			defer finish()
			close(c.release)
			select {
			case early := <-newTab:
				t.Fatalf("tab_new answered before verifying the contested creation: %v / %v", early.Result, early.Error)
			case <-verifying:
			case <-time.After(enumTestWait):
				t.Fatal("no verification and no answer")
			}
			c.setListErr(nil)
			if tc.closed {
				enumMustRPC(t, s, "tab_close", map[string]any{"tab": string(ovfA)})
				if s.tabs.Known(ovfA) {
					t.Fatal("precondition: the confirmed close retired A")
				}
			}
			// Requested after the close: Chrome still lists the closing target.
			if rows := enumMustRPC(t, s, "tab_list", nil); !ovfRowsContain(enumRPCReply{Result: rows}, ovfA) || !s.tabs.Has(ovfA) {
				t.Fatal("precondition: a fresh list shows A and A resolves")
			}
			if tc.closed {
				c.removeExternally(ovfA) // the destruction takes effect
			}
			finish()
			r := <-newTab
			_, idle := s.tabIdle.Snapshot()[ovfA]
			managed := managedTargetSet(s.store.ManagedTabs())[ovfA]
			t.Logf("closed=%v failed=%v: browser A=%v B=%v; tab_new result=%v error=%v; A known=%v idle=%v managed=%v selected=%v",
				tc.closed, tc.failed, c.has(ovfA), c.has(ovfB), r.Result, r.Error, s.tabs.Known(ovfA), idle, managed, s.tabs.Selected() == ovfA)
			if !c.has(ovfB) {
				t.Error("B is gone")
			}
			if !tc.closed {
				if r.Error != nil || r.Result["tab"] != string(ovfA) {
					t.Fatalf("live control lost its tab: %v / %v", r.Result, r.Error)
				}
				if !s.tabs.Has(ovfA) || !idle || !managed || s.tabs.Selected() != ovfA || !c.has(ovfA) {
					t.Errorf("live control: resolves=%v idle=%v managed=%v selected=%v browser=%v",
						s.tabs.Has(ovfA), idle, managed, s.tabs.Selected() == ovfA, c.has(ovfA))
				}
				return
			}
			causalRefusedCreate(t, r)
			if s.tabs.Known(ovfA) || idle || managed || s.tabs.Selected() == ovfA {
				t.Errorf("retired A kept: known=%v idle=%v managed=%v selected=%v", s.tabs.Known(ovfA), idle, managed, s.tabs.Selected() == ovfA)
			}
			ovfClearsRefuse(t, s, string(ovfA))
		})
	}
}
