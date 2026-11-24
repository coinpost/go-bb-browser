package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/coinpost/go-bb-browser/internal/state"
	"github.com/coinpost/go-bb-browser/pkg/protocol"
)

// Regressions for the bounded contract of target release: within one daemon a
// full CDP target id is never used again after its page is gone. Real
// browser.Session, the daemon's RPC entry point and the local CDP protocol stub
// throughout.

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

// Tabs are opened, observed in all three domains with a live event stream and
// released again, over and over, every one with a full target id of its own.
// Each of the four release paths is used: tab_close, the user closing the tab,
// a target enumeration that no longer lists it, and idle reclaim. After every
// release the old target id owns nothing and the next tab sees none of its
// data. Run under -race.
func TestDistinctTargetChurnReleasesAllObservationState(t *testing.T) {
	e := newObsLifeEnv(t)
	methods := []string{protocol.MethodNetwork, protocol.MethodConsole, protocol.MethodErrors}
	paths := []string{"tab_close", "user_close", "enumeration", "idle_reclaim"}
	const rounds = 12
	for i := range rounds {
		path := paths[i%len(paths)]
		tid := target.ID(fmt.Sprintf("D15C%020d0000C0DE", i))
		e.st.queueCreateID(tid)
		tab := enumTabNew(t, e.srv)
		if tab != string(tid) {
			t.Fatalf("round %d: tab_new returned %q, want %q", i, tab, tid)
		}
		for _, m := range methods {
			if events, _ := enumMustRPC(t, e.srv, m, map[string]any{"tab": tab})["events"].([]any); len(events) != 0 {
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
				events, _ := enumMustRPC(t, e.srv, m, map[string]any{"tab": tab})["events"].([]any)
				return len(events) > 0
			})
		}

		when := fmt.Sprintf("round %d (%s)", i, path)
		switch path {
		case "tab_close":
			enumMustRPC(t, e.srv, protocol.MethodTabClose, map[string]any{"tab": tab})
		case "user_close":
			e.st.destroyPage(tid)
			e.awaitCond(when+": the destroyed tab to leave the registry", func() bool {
				return !e.srv.tabs.Known(tid)
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
		if e.srv.tabs.Known(tid) {
			t.Fatalf("%s: released tab is still registered", when)
		}
		close(stop)
		<-stopped
		reply, err := enumRPC(e.srv, protocol.MethodNetwork, map[string]any{"tab": tab})
		if err != nil {
			t.Fatal(err)
		}
		assertStaleTabError(t, reply, when+": query of the released tab")
		e.assertTargetFullyReleased(tid, when+", after a late query and the rest of the stream")
		if n := e.srv.obsStore.TargetCount(); n != 0 {
			t.Fatalf("%s: a late query or event left %d target bucket(s)", when, n)
		}
	}
	if tid, _ := e.srv.tabs.Lookup(e.tab); tid != obsLifeTabID || !e.st.hasPage(obsLifeTabID) {
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
			reply := e.awaitReply(e.asyncRPC(kind.method, kind.params(e.tab)), "the failing first call")
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

			enumMustRPC(t, e.srv, kind.method, kind.params(e.tab))
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
