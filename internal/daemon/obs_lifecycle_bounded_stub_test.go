package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/yiplee/go-bb-browser/internal/browser"
	"github.com/yiplee/go-bb-browser/internal/state"
	"github.com/yiplee/go-bb-browser/pkg/protocol"
)

// Regressions for the bounded contract of target release: within one daemon a
// full CDP target id is never used again after its page is gone, while short
// ids are, by pages with other full ids. Every tab below therefore has a full
// target id of its own; where two of them share a short id, one after the
// other, that is said. Real browser.Session, the daemon's RPC entry point and
// the local CDP protocol stub throughout.

// removePageSilently makes a page vanish without Target.targetDestroyed, so
// only a later enumeration can notice.
func (st *cdpStub) removePageSilently(id target.ID) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.pages = slices.DeleteFunc(st.pages, func(p target.ID) bool { return p == id })
}

func (e *obsLifeEnv) emitConsoleOn(tid target.ID, text string) {
	sid, _ := e.st.sessionFor(tid)
	e.st.emitSession(sid, "Runtime.consoleAPICalled", map[string]any{
		"type": "log", "args": []any{map[string]any{"type": "string", "value": text}},
		"executionContextId": 1, "timestamp": 1,
	})
}

func (e *obsLifeEnv) emitExceptionOn(tid target.ID, text string) {
	sid, _ := e.st.sessionFor(tid)
	e.st.emitSession(sid, "Runtime.exceptionThrown", map[string]any{
		"timestamp": 1, "exceptionDetails": map[string]any{
			"exceptionId": 1, "text": text, "lineNumber": 1, "columnNumber": 1,
		},
	})
}

// assertTargetFullyReleased checks everything INV-6 promises for a released
// target id: no events in any of the three rings, no bucket, late events of all
// three kinds rejected, and no CDP session of ours left on it.
func (e *obsLifeEnv) assertTargetFullyReleased(tid target.ID, when string) {
	e.t.Helper()
	for name, query := range map[string]func(target.ID, uint64) ([]state.ObsEvent, uint64, uint64){
		"network": e.srv.obsStore.QueryNetwork,
		"console": e.srv.obsStore.QueryConsole,
		"errors":  e.srv.obsStore.QueryErrors,
	} {
		if events, _, _ := query(tid, 0); len(events) != 0 {
			e.t.Errorf("%s: %d %s event(s) buffered for released target %s (INV-6)", when, len(events), name, tid)
		}
	}
	if e.srv.obsStore.HasTarget(tid) {
		e.t.Errorf("%s: released target %s still owns an observation bucket", when, tid)
	}
	late := json.RawMessage(`{"late":true}`)
	if e.srv.obsStore.PushNetwork(tid, 1, late) || e.srv.obsStore.PushConsole(tid, 1, late) || e.srv.obsStore.PushError(tid, 1, late) {
		e.t.Errorf("%s: late event accepted for released target %s", when, tid)
	}
	if e.srv.obsStore.HasTarget(tid) {
		e.t.Errorf("%s: late events rebuilt the bucket of released target %s", when, tid)
	}
	if live := e.st.liveSessions(tid); len(live) != 0 {
		e.t.Errorf("%s: CDP session(s) %v still attached to released target %s", when, live, tid)
	}
}

func (e *obsLifeEnv) awaitCond(what string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(obsLifeWait)
	for !cond() {
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// The old tab's writer has dequeued an event and is parked inside Record when
// the user closes the tab; its release is therefore still waiting. Meanwhile a
// page with a DIFFERENT full target id takes over the old tab's short id and is
// observed. The new tab must not wait for, read or lose anything to the old
// lifecycle, and once the old Record is let go the old target id owns nothing:
// three empty rings, no bucket, late events rejected.
func TestShortIDTakenOverByOtherTargetDuringReleaseIsolatesObservation(t *testing.T) {
	e := newObsLifeEnv(t)
	sink := &holdingSink{obsSink: e.srv.obsSink, entered: make(chan struct{}, 1), release: make(chan struct{})}
	releaseRecord := func() { closeOnce(sink.release) }
	defer releaseRecord()
	// The observer keeps the recorder it was created with.
	for _, kind := range []browser.ObservationKind{browser.ObservationNetwork, browser.ObservationConsole, browser.ObservationErrors} {
		if err := e.sess.EnsureObservation(context.Background(), obsLifeTabID, kind, sink, nil); err != nil {
			t.Fatal(err)
		}
	}
	e.srv.obsStore.RegisterTarget(obsLifeTabID)
	recordAllKinds(e.srv.obsSink, obsLifeTabID, 1) // all three rings hold old data
	e.emitRequest("https://old-late.test/")
	e.await(sink.entered, "the old writer to reach Record")

	// The user closes the old tab; its release blocks on the in-flight Record.
	e.st.destroyPage(obsLifeTabID)
	e.awaitCond("the old registration to leave the registry", func() bool {
		_, ok := e.srv.tabs.Lookup(e.short)
		return !ok
	})

	// Another page takes the short id. The lifecycle loop is busy with the old
	// release, so the page is found by an enumeration.
	e.st.addPage(obsLifeAliasID, false)
	enumMustRPC(t, e.srv, protocol.MethodTabList, nil)
	if tid, _ := e.srv.tabs.Lookup(e.short); tid != obsLifeAliasID {
		t.Fatalf("precondition: short id %q names %q, want the other target %q", e.short, tid, obsLifeAliasID)
	}
	for _, m := range []string{protocol.MethodNetwork, protocol.MethodConsole, protocol.MethodErrors} {
		reply := e.awaitReply(e.asyncRPC(m, map[string]any{"tab": e.short}), "the new tab's first "+m+" query (it must not wait for the old release)")
		if reply.Error != nil {
			t.Fatalf("new tab's first %s query failed: %v", m, reply.Error)
		}
		if events, _ := reply.Result["events"].([]any); len(events) != 0 {
			t.Errorf("new tab's first %s query returned the old tab's events: %v", m, events)
		}
	}
	if !e.srv.obsStore.HasTarget(obsLifeTabID) {
		t.Fatal("precondition: the old release finished although its Record is still parked")
	}
	e.emitRequestOn(obsLifeAliasID, "https://new.test/")
	e.emitConsoleOn(obsLifeAliasID, "new")
	e.emitExceptionOn(obsLifeAliasID, "new")
	e.awaitQueryField(protocol.MethodNetwork, e.short, "url", "https://new.test/")
	e.awaitQueryField(protocol.MethodErrors, e.short, "text", "new")

	// The old Record goes through; the old release completes.
	releaseRecord()
	e.awaitCond("the old target's release to complete", func() bool { return !e.srv.obsStore.HasTarget(obsLifeTabID) })
	e.loopIdle()
	e.assertTargetFullyReleased(obsLifeTabID, "after the old release")
	if n := e.srv.obsStore.TargetCount(); n != 1 {
		t.Errorf("target buckets = %d, want only the new tab's 1", n)
	}
	if got := e.queryField(protocol.MethodNetwork, e.short, "url"); !slices.Equal(got, []string{"https://new.test/"}) {
		t.Errorf("new tab's network events = %v, want only its own", got)
	}
	if got := e.queryField(protocol.MethodErrors, e.short, "text"); !slices.Equal(got, []string{"new"}) {
		t.Errorf("new tab's error events = %v, want only its own", got)
	}
	if events, _ := enumMustRPC(t, e.srv, protocol.MethodConsole, map[string]any{"tab": e.short})["events"].([]any); len(events) != 1 {
		t.Errorf("new tab's console events = %v, want only its own one", events)
	}
	if slices.Contains(e.st.closedTargets(), obsLifeAliasID) || !e.st.hasPage(obsLifeAliasID) {
		t.Error("releasing the old tab closed the page that took over its short id")
	}
	if live := e.st.liveSessions(obsLifeAliasID); len(live) != 1 {
		t.Errorf("sessions attached to the new tab = %v, want its observer's one", live)
	}
}

// Tabs are opened, observed in all three domains with a live event stream and
// released again, over and over, every one with a full target id of its own and
// (one after the other) the same short id. Each of the four release paths is
// used: tab_close, the user closing the tab, a target enumeration that no
// longer lists it, and idle reclaim. After every release the old target id owns
// nothing and the next tab sees none of its data. Run under -race.
func TestDistinctTargetChurnReleasesAllObservationState(t *testing.T) {
	e := newObsLifeEnv(t)
	methods := []string{protocol.MethodNetwork, protocol.MethodConsole, protocol.MethodErrors}
	paths := []string{"tab_close", "user_close", "enumeration", "idle_reclaim"}
	const rounds = 12
	var shorts []string
	for i := range rounds {
		path := paths[i%len(paths)]
		// Same last hex digits as the env's own tab, a different full id each time.
		tid := target.ID(fmt.Sprintf("D15C%020d0000C0DE", i))
		e.st.queueCreateID(tid)
		short := enumTabNew(t, e.srv)
		if got, _ := e.srv.tabs.Lookup(short); got != tid {
			t.Fatalf("round %d: tab_new short id %q names %q, want %q", i, short, got, tid)
		}
		shorts = append(shorts, short)
		for _, m := range methods {
			if events, _ := enumMustRPC(t, e.srv, m, map[string]any{"tab": short})["events"].([]any); len(events) != 0 {
				t.Fatalf("round %d: first %s query of a new tab returned events %v", i, m, events)
			}
		}

		stop, stopped := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(stopped)
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				tag := fmt.Sprintf("r%d-%d", i, n)
				e.emitRequestOn(tid, "https://"+tag+".test/")
				e.emitConsoleOn(tid, tag)
				e.emitExceptionOn(tid, tag)
				time.Sleep(200 * time.Microsecond)
			}
		}()
		for _, m := range methods {
			e.awaitCond(fmt.Sprintf("round %d: %s events of the live tab", i, m), func() bool {
				events, _ := enumMustRPC(t, e.srv, m, map[string]any{"tab": short})["events"].([]any)
				return len(events) > 0
			})
		}

		when := fmt.Sprintf("round %d (%s)", i, path)
		switch path {
		case "tab_close":
			enumMustRPC(t, e.srv, protocol.MethodTabClose, map[string]any{"tab": short})
		case "user_close":
			e.st.destroyPage(tid)
			e.awaitCond(when+": the destroyed tab to leave the registry", func() bool {
				_, ok := e.srv.tabs.ShortForTarget(tid)
				return !ok
			})
			e.loopIdle()
		case "enumeration":
			e.st.removePageSilently(tid)
			enumMustRPC(t, e.srv, protocol.MethodTabList, nil)
		case "idle_reclaim":
			e.srv.tabIdle.MarkManagedAt(tid, time.Now().Add(-time.Hour))
			e.srv.closeExpiredTabs(context.Background())
		}
		// tab_close, the enumeration and idle reclaim are synchronous: the release
		// is complete when they return, with the event stream still running.
		e.assertTargetFullyReleased(tid, when)
		if n := e.srv.obsStore.TargetCount(); n != 0 {
			t.Fatalf("%s: observation store holds %d target bucket(s), want 0", when, n)
		}
		if _, ok := e.srv.tabs.ShortForTarget(tid); ok {
			t.Fatalf("%s: released tab is still registered", when)
		}
		close(stop)
		<-stopped
		reply, err := enumRPC(e.srv, protocol.MethodNetwork, map[string]any{"tab": short})
		if err != nil {
			t.Fatal(err)
		}
		assertStaleTabError(t, reply, when+": query of the released tab")
		e.assertTargetFullyReleased(tid, when+", after a late query and the rest of the stream")
		if n := e.srv.obsStore.TargetCount(); n != 0 {
			t.Fatalf("%s: a late query or event left %d target bucket(s)", when, n)
		}
	}
	if len(slices.Compact(slices.Clone(shorts))) != 1 {
		t.Errorf("short ids = %v, want every round to take over the same one", shorts)
	}
	if tid, _ := e.srv.tabs.Lookup(e.short); tid != obsLifeTabID || !e.st.hasPage(obsLifeTabID) {
		t.Error("the churn removed or closed the tab that was never released")
	}

	// A tab that stays is still observable afterwards.
	e.startObserving("https://live.test/")
	if n := e.srv.obsStore.TargetCount(); n != 1 {
		t.Errorf("target buckets = %d, want the live tab's 1", n)
	}
}

// A first attach fails after the browser attached a session, and the browser
// then refuses the Target.detachFromTarget that cleans it up, once. The session
// id must survive that refusal so the cleanup can try again; otherwise the
// session stays on the live page and every retry adds one more.
func TestFailedAttachCleanupRetriesRefusedDetach(t *testing.T) {
	for _, kind := range attachKindsRPC {
		t.Run(kind.name, func(t *testing.T) {
			e := newObsLifeEnv(t)
			e.st.failNext("Inspector.enable") // part of chromedp's attach, after Target.attachToTarget
			e.st.failNext("Target.detachFromTarget")
			reply := e.awaitReply(e.asyncRPC(kind.method, kind.params(e.short)), "the failing first call")
			if reply.Error == nil {
				t.Fatalf("call succeeded although the attach failed: %v", reply.Result)
			}
			failed, ok := e.st.sessionFor(obsLifeTabID)
			if !ok {
				t.Fatal("precondition: the failed attach never attached a session")
			}
			deadline := time.Now().Add(obsLifeWait)
			for len(e.st.liveSessions(obsLifeTabID)) != 0 {
				if time.Now().After(deadline) {
					t.Fatalf("session %q of the failed attach is still attached after one refused detach", failed)
				}
				time.Sleep(2 * time.Millisecond)
			}

			enumMustRPC(t, e.srv, kind.method, kind.params(e.short))
			current, _ := e.st.sessionFor(obsLifeTabID)
			if live := e.st.liveSessions(obsLifeTabID); len(live) != 1 || live[0] != current || current == failed {
				t.Errorf("sessions attached after the retry = %v, want only the retry's own (failed attach: %q)", live, failed)
			}
			if slices.Contains(e.st.closedTargets(), obsLifeTabID) || !e.st.hasPage(obsLifeTabID) {
				t.Error("cleaning up the failed attach closed the page")
			}
			if kind.method == protocol.MethodNetwork {
				if n := e.srv.obsStore.TargetCount(); n != 1 {
					t.Errorf("target buckets = %d, want the working observation's 1", n)
				}
			}
		})
	}
}

// An observation query has read its tab's ring and has not answered yet when
// the tab is closed and a page with a DIFFERENT full target id takes over its
// short id. The answer is still the old tab's, so the success must be credited
// to the old tab: neither the idle clock nor the persisted last-activity of the
// tab that now carries the short id may be renewed by it.
func TestQueryAnsweredAfterShortIDMovedDoesNotRenewOtherTab(t *testing.T) {
	e := newObsLifeEnv(t)
	e.startObserving("https://old.test/")
	arrived, release := e.parkStep(obsStepRead, obsLifeTabID)
	defer release()
	reply := e.asyncRPC(protocol.MethodNetwork, map[string]any{"tab": e.short})
	e.await(arrived, "the query to read its ring")

	e.st.destroyPage(obsLifeTabID)
	e.awaitCond("the old tab's release", func() bool {
		_, ok := e.srv.tabs.Lookup(e.short)
		return !ok && !e.srv.obsStore.HasTarget(obsLifeTabID)
	})
	e.st.addPage(obsLifeAliasID, true)
	e.awaitCond("the other page to take over the short id", func() bool {
		tid, _ := e.srv.tabs.Lookup(e.short)
		return tid == obsLifeAliasID
	})
	idleSince := time.Now().Add(-time.Hour)
	e.srv.tabIdle.MarkManagedAt(obsLifeAliasID, idleSince)
	if err := e.srv.store.RegisterManagedTab(e.short, string(obsLifeAliasID), idleSince.UTC()); err != nil {
		t.Fatal(err)
	}

	release()
	got := e.awaitReply(reply, "the parked query")
	if got.Error != nil {
		t.Fatalf("query that had already read its ring failed: %v", got.Error)
	}
	if urls := replyEventField(got.Result, "url"); !slices.Equal(urls, []string{"https://old.test/"}) {
		t.Fatalf("precondition: the query answered %v, want the old tab's events", urls)
	}
	e.srv.auditWG.Wait()

	if !e.srv.tabIdle.IsExpired(obsLifeAliasID, time.Now(), time.Minute) {
		t.Error("the old tab's query renewed the idle clock of the tab that took over its short id")
	}
	for _, mt := range e.srv.store.ManagedTabs() {
		if mt.TargetID == string(obsLifeAliasID) && mt.LastActivity.After(idleSince.Add(time.Second)) {
			t.Errorf("the old tab's audit line renewed the persisted last-activity of the other tab: %v", mt.LastActivity)
		}
	}
	e.assertTargetFullyReleased(obsLifeTabID, "after the late answer")
}
