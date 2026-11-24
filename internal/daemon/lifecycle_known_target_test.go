package daemon

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/yiplee/go-bb-browser/internal/browser"
	"github.com/yiplee/go-bb-browser/internal/store"
)

// Schedules of the FT28 round-8 review: the registration entries must reach
// the same verdict whether or not the target is already known when they run.
// All of them use the production wiring (real Session reader, registry as its
// observer, root-level discovery enabled by the connection itself, the real
// queue consumer and the HTTP handlers) against the CDP stub; the stub only
// sends lifecycle notifications because the daemon asked for them. A is
// created once and its id is never reused; B shares A's suffix and must stay
// untouched. A "lost" destroyed notification is produced by not sending it.

// knownTargetEnv is a production-wired server whose Target.createTarget reply
// can be held back from the tab_new handler.
func knownTargetEnv(t *testing.T) (*Server, *cdpStub, *browser.Session, *ovfHeldCreate, context.Context, string) {
	t.Helper()
	dir := t.TempDir()
	st := newCDPStub(t, ovfB)
	s := newReconcileRaceServer(t, newEnumConn(), dir, 0)
	// A held automatic enumeration must stay held, not time out and be retried.
	s.cfg.CDPWatchdogTimeout = 20 * time.Second
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	t.Cleanup(cancel)
	sess := causalConnect(t, ctx, s, st)
	c := &ovfHeldCreate{Session: sess, made: make(chan target.ID, 1), release: make(chan struct{})}
	t.Cleanup(func() { closeOnce(c.release) })
	s.tabHook = c
	ovfListed(t, s, ovfB)
	return s, st, sess, c, ctx, dir
}

// knownTargetCreate starts tab_new, whose reply (A) is held, and has the
// browser announce A: when it returns, created(A) has been read and queued.
func knownTargetCreate(t *testing.T, ctx context.Context, s *Server, st *cdpStub, c *ovfHeldCreate, sess *browser.Session) <-chan enumRPCReply {
	t.Helper()
	st.queueCreateID(ovfA)
	ch := ovfAsyncRPC(s, "tab_new")
	select {
	case id := <-c.made:
		if id != ovfA {
			t.Fatalf("created %s, want %s", id, ovfA)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// The stub does not announce what Target.createTarget makes; this is the
	// one announcement of that target, sent only because discovery is on.
	st.emit("Target.targetCreated", map[string]any{"targetInfo": stubTargetInfo(ovfA)})
	if err := sess.PingBrowserContext(ctx); err != nil {
		t.Fatal(err)
	}
	return ch
}

func knownTargetLoop(t *testing.T, ctx context.Context, s *Server, sess *browser.Session) {
	t.Helper()
	loop, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); s.runTargetSync(loop, sess) }()
	t.Cleanup(func() { stop(); <-done })
}

func knownTargetManagedOnDisk(t *testing.T, s *Server, dir string) bool {
	t.Helper()
	if err := s.closeAudit(); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(store.OpenConfig{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	return managedTargetSet(db.ManagedTabs())[ovfA]
}

// P1 (CommitCreated, known target). An older B-only list is in flight when
// tab_new creates A; A's creation notification registers A before the creation
// reply is delivered; the old list is applied (it cannot be ordered against A,
// so A stays and the sync loop asks again — that reply is held). The creation
// reply must not be answered on the strength of the registration alone: the
// window of the creation saw an enumeration that did not list A, exactly as it
// would have for a target not yet known, and the question is settled before
// tab_new answers.
func TestLateCreateOfAnnouncedTargetSettlesWindowAbsence(t *testing.T) {
	for _, closed := range []bool{true, false} {
		name := "closed in the browser"
		if !closed {
			name = "live control"
		}
		t.Run(name, func(t *testing.T) {
			s, st, sess, c, ctx, dir := knownTargetEnv(t)
			n := st.getTargetsCalls()
			early := st.holdNextGetTargets()
			defer closeOnce(early)
			old := ovfAsyncRPC(s, "tab_list")
			awaitGetTargets(t, st, n+1)
			newTab := knownTargetCreate(t, ctx, s, st, c, sess)
			knownTargetLoop(t, ctx, s, sess)
			ovfAwait(t, ctx, "A registered by its creation notification", func() bool { return s.tabs.Has(ovfA) })
			if closed {
				st.removePage(ovfA, false) // destroyed notification lost
			}
			auto := st.holdNextGetTargets()
			defer closeOnce(auto)
			closeOnce(early)
			if r := <-old; r.Error != nil {
				t.Fatal(r.Error)
			} else if ovfRowsContain(r, ovfA) {
				t.Fatal("precondition: the old list must not contain A")
			}
			awaitGetTargets(t, st, n+2) // the automatic recheck, held
			atCommit := st.getTargetsCalls()
			closeOnce(c.release)
			r := <-newTab
			synchronous := st.getTargetsCalls() - atCommit
			_, idle := s.tabIdle.Snapshot()[ovfA]
			t.Logf("closed=%v: tab_new result=%v error=%v; Target.getTargets sent before the answer=%d (automatic recheck still held); browser A=%v B=%v; A resolves=%v idle=%v",
				closed, r.Result, r.Error, synchronous, st.hasPage(ovfA), st.hasPage(ovfB), s.tabs.Has(ovfA), idle)
			if synchronous < 1 {
				t.Error("tab_new answered without asking the browser again, although an enumeration applied during the creation did not list the tab")
			}
			if closed {
				causalRefusedCreate(t, r)
				ovfClearsRefuse(t, s, string(ovfA))
				if s.tabs.Has(ovfA) || s.tabs.Known(ovfA) || idle {
					t.Errorf("dead A kept: resolves=%v known=%v idle=%v", s.tabs.Has(ovfA), s.tabs.Known(ovfA), idle)
				}
				if knownTargetManagedOnDisk(t, s, dir) {
					t.Error("dead A is replayed from rpc.jsonl as a managed tab")
				}
				causalNoClose(t, st, ovfA) // only the rollback of A itself
				return
			}
			if r.Error != nil || r.Result["tab"] != string(ovfA) {
				t.Fatalf("live control: tab_new = %v / %v", r.Result, r.Error)
			}
			if !s.tabs.Has(ovfA) || !idle || !st.hasPage(ovfA) {
				t.Errorf("live control lost its tab: resolves=%v idle=%v browser=%v", s.tabs.Has(ovfA), idle, st.hasPage(ovfA))
			}
			closeOnce(auto)
			if !knownTargetManagedOnDisk(t, s, dir) {
				t.Error("live control: A is not persisted as managed")
			}
			causalNoClose(t, st)
		})
	}
}

// P1 (RegisterEvent, known target). created(A) is read and queued; A is closed
// in the browser (notification lost); a list is requested after that read and
// its B-only reply is held; the creation reply commits A; the consumer handles
// the queued event. The event proves A existed before the list was requested,
// so the list proves A gone — whichever of the two the daemon gets to first.
// The later commit must not stand in for the earlier proof.
func TestQueuedEventOfCommittedTargetKeepsItsReadInstant(t *testing.T) {
	setup := func(t *testing.T) (*Server, *cdpStub, *browser.Session, context.Context, chan struct{}, <-chan enumRPCReply) {
		s, st, sess, c, ctx, _ := knownTargetEnv(t)
		newTab := knownTargetCreate(t, ctx, s, st, c, sess)
		if len(sess.TargetEvents()) != 1 {
			t.Fatal("precondition: created(A) must be queued")
		}
		st.removePage(ovfA, false)
		n := st.getTargetsCalls()
		gate := st.holdNextGetTargets()
		t.Cleanup(func() { closeOnce(gate) })
		list := ovfAsyncRPC(s, "tab_list")
		awaitGetTargets(t, st, n+1)
		closeOnce(c.release)
		if r := <-newTab; r.Error != nil {
			t.Fatal(r.Error)
		}
		if !s.tabs.Has(ovfA) {
			t.Fatal("precondition: tab_new committed A")
		}
		// Announced behind created(A): once the marker resolves, the consumer
		// has handled created(A).
		st.addPage(ovfMarker, true)
		if err := sess.PingBrowserContext(ctx); err != nil {
			t.Fatal(err)
		}
		return s, st, sess, ctx, gate, list
	}
	absentList := func(t *testing.T, list <-chan enumRPCReply) {
		t.Helper()
		if r := <-list; r.Error != nil || ovfRowsContain(r, ovfA) {
			t.Fatalf("list = %+v, want one without A", r)
		}
	}
	gone := func(t *testing.T, s *Server, st *cdpStub, when string) {
		t.Helper()
		t.Logf("%s: browser A=%v, A resolves=%v", when, st.hasPage(ovfA), s.tabs.Has(ovfA))
		if s.tabs.Has(ovfA) || s.tabs.Selected() == ovfA {
			t.Errorf("%s: A still resolves although a list requested after created(A) was read does not contain it", when)
		}
	}

	t.Run("event handled before the list is applied", func(t *testing.T) {
		s, st, sess, ctx, gate, list := setup(t)
		knownTargetLoop(t, ctx, s, sess)
		ovfAwait(t, ctx, "consumer handled created(A)", func() bool { return s.tabs.Has(ovfMarker) })
		auto := st.holdNextGetTargets()
		defer closeOnce(auto)
		closeOnce(gate)
		absentList(t, list)
		gone(t, s, st, "list applied after the event was handled, next automatic reply held")
		// The list alone removed A, so nothing asked for an automatic recheck
		// and the gate is still pending. Resolving an unknown id enumerates
		// once: left in place, the gate would hold that request until the
		// operation deadline and the timeout would pass for a refusal.
		closeOnce(auto)
		ovfClearsRefuse(t, s, string(ovfA))
		ovfAwait(t, ctx, "A forgotten", func() bool { return !s.tabs.Known(ovfA) })
		_, idle := s.tabIdle.Snapshot()[ovfA]
		if idle {
			t.Error("dead A is still idle-tracked")
		}
		causalNoClose(t, st)
	})

	t.Run("event handled after the list is applied", func(t *testing.T) {
		s, st, sess, ctx, gate, list := setup(t)
		auto := st.holdNextGetTargets()
		defer closeOnce(auto)
		closeOnce(gate)
		absentList(t, list)
		// Nothing orders the list against the commit yet: the event that does is
		// still queued. The consumer handles the whole queue before it sends the
		// enumeration the conflict asked for (held): once that is on the wire,
		// created(A) has been handled.
		n := st.getTargetsCalls()
		knownTargetLoop(t, ctx, s, sess)
		awaitGetTargets(t, st, n+1)
		gone(t, s, st, "event handled after the list was applied, automatic reply held")
		// The automatic recheck took the gate: the clears' own enumeration is
		// answered at once while that reply stays held.
		ovfClearsRefuse(t, s, string(ovfA))
		closeOnce(auto)
		ovfAwait(t, ctx, "A forgotten", func() bool { return !s.tabs.Known(ovfA) })
		_, idle := s.tabIdle.Snapshot()[ovfA]
		if idle {
			t.Error("dead A is still idle-tracked")
		}
		causalNoClose(t, st)
	})
}

// Control for the test above: the list was requested BEFORE created(A) was
// read. Nothing orders its B-only snapshot against A, so the live new tab
// keeps resolving through it, also after its queued event was handled.
func TestQueuedEventReadAfterListRequestKeepsCommittedTab(t *testing.T) {
	s, st, sess, c, ctx, _ := knownTargetEnv(t)
	n := st.getTargetsCalls()
	gate := st.holdNextGetTargets()
	defer closeOnce(gate)
	list := ovfAsyncRPC(s, "tab_list")
	awaitGetTargets(t, st, n+1) // B-only snapshot taken, reply held
	newTab := knownTargetCreate(t, ctx, s, st, c, sess)
	closeOnce(c.release)
	// The creation window has not seen the list applied yet: no verification.
	if r := <-newTab; r.Error != nil || r.Result["tab"] != string(ovfA) {
		t.Fatalf("tab_new = %v / %v", r.Result, r.Error)
	}
	st.addPage(ovfMarker, true)
	knownTargetLoop(t, ctx, s, sess)
	ovfAwait(t, ctx, "consumer handled created(A)", func() bool { return s.tabs.Has(ovfMarker) })
	auto := st.holdNextGetTargets()
	defer closeOnce(auto)
	closeOnce(gate)
	if r := <-list; r.Error != nil || ovfRowsContain(r, ovfA) {
		t.Fatalf("list = %+v, want the older one without A", r)
	}
	awaitGetTargets(t, st, n+2) // the conflict is rechecked automatically
	if !s.tabs.Has(ovfA) {
		t.Error("the new live tab stopped resolving because of a snapshot requested before it was announced")
	}
	for _, method := range []string{"network_clear", "console_clear", "errors_clear"} {
		if r, err := enumRPC(s, method, map[string]any{"tab": string(ovfA)}); err != nil || r.Error != nil {
			t.Errorf("%s on the live new tab: %v %v", method, err, r.Error)
		}
	}
	closeOnce(auto)
	ovfAwait(t, ctx, "automatic recheck applied", func() bool { return s.tabs.OpenWindows() == 0 })
	if _, idle := s.tabIdle.Snapshot()[ovfA]; !s.tabs.Has(ovfA) || !idle || !st.hasPage(ovfA) {
		t.Errorf("live A lost after the recheck: resolves=%v idle=%v browser=%v", s.tabs.Has(ovfA), idle, st.hasPage(ovfA))
	}
	causalNoClose(t, st)
}

// P2 (unknown target). created(A) and destroyed(A) are both read before the
// consumer runs, and the consumer cannot finish the destruction because
// another lifecycle writer holds the mutex. The older creation event must not
// make the dead tab resolve in between.
func TestQueuedCreationOfUnknownTargetDoesNotOutliveItsDestroyedRead(t *testing.T) {
	st := newCDPStub(t, ovfB)
	s := newStubSessionServer(t, st)
	defer s.closeAudit()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	sess := causalConnect(t, ctx, s, st)
	s.tabHook = sess
	ovfListed(t, s, ovfB)
	st.addPage(ovfA, true)
	st.removePage(ovfA, true)
	if err := sess.PingBrowserContext(ctx); err != nil {
		t.Fatal(err)
	}
	if got := len(sess.TargetEvents()); got != 2 || s.tabs.Known(ovfA) {
		t.Fatalf("precondition: created and destroyed queued for an unknown A, got %d queued, known=%v", got, s.tabs.Known(ovfA))
	}

	s.lifeMu.Lock()
	done := make(chan struct{})
	go func() { defer close(done); s.runTargetSync(ctx, sess) }()
	defer func() { cancel(); <-done }()
	func() {
		defer s.lifeMu.Unlock()
		ovfAwait(t, ctx, "both notifications dequeued", func() bool { return len(sess.TargetEvents()) == 0 })
		resolves := s.tabs.Has(ovfA)
		t.Logf("consumer blocked on the destruction: browser A=%v, A resolves=%v, B=%v", st.hasPage(ovfA), resolves, st.hasPage(ovfB))
		if resolves {
			t.Error("a creation event read before the destroyed notification registered the dead tab")
			// Resolved from the registry alone; an id that does not resolve would
			// make the RPC enumerate, which needs the mutex held here.
			ovfClearsRefuse(t, s, string(ovfA))
		}
	}()
	ovfAwait(t, ctx, "cleanup finished", func() bool { return !s.tabs.Known(ovfA) && s.tabs.PendingDestroyed() == 0 })
	ovfClearsRefuse(t, s, string(ovfA))
	if _, ok := s.tabs.Lookup(string(ovfB)); !ok {
		t.Error("B no longer resolves")
	}
	causalNoClose(t, st)
}

// verifyConn is a fake browser whose Target.createTarget reply can be held and
// which counts the enumerations requested from its sync loop.
type verifyConn struct {
	*enumConn
	made, release chan struct{}
	dirty         atomic.Int32
}

func (c *verifyConn) CreatePageTarget(url string, silent bool) (target.ID, error) {
	id, err := c.enumConn.CreatePageTarget(url, silent)
	close(c.made)
	<-c.release
	return id, err
}

func (c *verifyConn) MarkTargetDirty() { c.dirty.Add(1) }

// P2 (commitCreatedTab). The enumeration that verifies a contested creation
// fails. A tab that another caller closed successfully during that
// verification is not a created tab any more; a tab that is still registered
// is, and the question is handed to the sync loop.
func TestFailedCreateVerificationRechecksTheCommittedTab(t *testing.T) {
	for _, closed := range []bool{true, false} {
		name := "closed during verification"
		if !closed {
			name = "live control"
		}
		t.Run(name, func(t *testing.T) {
			c := &verifyConn{enumConn: newEnumConn(ovfB), made: make(chan struct{}), release: make(chan struct{})}
			c.nextIDs = []target.ID{ovfA}
			s := newEnumServer(t, c.enumConn)
			defer s.closeAudit()
			s.tabHook = c
			started, finish := c.holdNextSnapshot()
			defer finish()
			earlier := ovfAsyncRPC(s, "tab_list")
			enumAwait(t, started, "B-only snapshot")
			newTab := ovfAsyncRPC(s, "tab_new")
			enumAwait(t, c.made, "creation took effect")
			finish()
			if r := <-earlier; r.Error != nil {
				t.Fatal(r.Error)
			}
			c.setListErr(errors.New("transient getTargets failure"))
			verifying, fail := c.holdNextSnapshot()
			defer fail()
			close(c.release)
			select {
			case early := <-newTab:
				t.Fatalf("tab_new answered before verifying the contested creation: %v / %v", early.Result, early.Error)
			case <-verifying:
			case <-time.After(5 * time.Second):
				t.Fatal("no verification and no answer")
			}
			if closed {
				enumMustRPC(t, s, "tab_close", map[string]any{"tab": string(ovfA)})
				if c.has(ovfA) || !c.has(ovfB) || s.tabs.Known(ovfA) {
					t.Fatal("precondition: tab_close(A) completed, B untouched")
				}
			}
			before := c.dirty.Load()
			fail()
			r := <-newTab
			_, idle := s.tabIdle.Snapshot()[ovfA]
			t.Logf("closed=%v: browser A=%v B=%v; tab_new result=%v error=%v; A resolves=%v idle=%v; enumerations requested from the sync loop=%d",
				closed, c.has(ovfA), c.has(ovfB), r.Result, r.Error, s.tabs.Has(ovfA), idle, c.dirty.Load()-before)
			if c.dirty.Load()-before < 1 {
				t.Error("the failed verification was not handed to the sync loop")
			}
			if closed {
				causalRefusedCreate(t, r)
				// A refusal is "unknown tab id" from an enumeration that
				// succeeded; while the lookup itself fails nothing is said
				// about the id (TestTabLookupFailureIsNotAnUnknownTabID).
				c.setListErr(nil)
				ovfClearsRefuse(t, s, string(ovfA))
				if s.tabs.Known(ovfA) || idle || s.tabs.Selected() == ovfA {
					t.Errorf("closed A kept: known=%v idle=%v selected=%v", s.tabs.Known(ovfA), idle, s.tabs.Selected() == ovfA)
				}
				if !c.has(ovfB) {
					t.Error("B is gone")
				}
				return
			}
			if r.Error != nil || r.Result["tab"] != string(ovfA) {
				t.Fatalf("live control lost its tab: %v / %v", r.Result, r.Error)
			}
			if !s.tabs.Has(ovfA) || !idle || !c.has(ovfA) || s.tabs.Selected() != ovfA {
				t.Errorf("live control: resolves=%v idle=%v browser=%v selected=%v", s.tabs.Has(ovfA), idle, c.has(ovfA), s.tabs.Selected() == ovfA)
			}
		})
	}
}
