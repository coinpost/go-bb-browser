package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/yiplee/go-bb-browser/internal/browser"
	"github.com/yiplee/go-bb-browser/pkg/protocol"
)

// Barrier regressions about lifecycle identity, again with a real
// browser.Session, the daemon's RPC entry point and the local CDP protocol stub.
// In each of them an operation that started under one registration of a tab
// finishes after that registration was released and the same full target id (and
// possibly its short id) was registered again. The late operation may only act
// on what its own registration owned.

// obsLifeAliasID shares the last four hex digits with obsLifeTabID and sorts
// before it: registered by the same enumeration it takes "c0de", and
// obsLifeTabID becomes "0c0de".
const obsLifeAliasID = target.ID("AAAB000000000000000000000000C0DE")

var obsLifeIdleSeq atomic.Uint64

// loopIdle is awaitTargetLoopIdle for tests that need it more than once.
func (e *obsLifeEnv) loopIdle() {
	e.t.Helper()
	id := target.ID(fmt.Sprintf("FFFF00000000000000000000%08d", obsLifeIdleSeq.Add(1)))
	e.st.addPage(id, true)
	deadline := time.Now().Add(obsLifeWait)
	for {
		if _, ok := e.srv.tabs.ShortForTarget(id); ok {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatal("target lifecycle loop did not finish the pending removal")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// parkStep parks the first observation query that reaches step for tid.
func (e *obsLifeEnv) parkStep(step string, tid target.ID) (arrived <-chan struct{}, release func()) {
	reached, gate := make(chan struct{}), make(chan struct{})
	var once sync.Once
	e.srv.obsStepHook = func(s string, id target.ID) {
		if s != step || id != tid {
			return
		}
		first := false
		once.Do(func() { first = true })
		if first {
			close(reached)
			<-gate
		}
	}
	return reached, func() { closeOnce(gate) }
}

func (e *obsLifeEnv) emitRequestOn(tid target.ID, url string) {
	e.t.Helper()
	sid, ok := e.st.sessionFor(tid)
	if !ok {
		e.t.Fatalf("no session attached to %s", tid)
	}
	e.st.emitSession(sid, "Network.requestWillBeSent", map[string]any{
		"requestId": "req-" + url, "loaderId": "loader", "documentURL": url,
		"request": map[string]any{
			"url": url, "method": "GET", "headers": map[string]any{},
			"initialPriority": "High", "referrerPolicy": "no-referrer",
		},
		"timestamp": 1, "wallTime": 1, "initiator": map[string]any{"type": "other"},
		"redirectHasExtraInfo": false, "type": "Document",
	})
}

func (e *obsLifeEnv) networkURLsOf(tid target.ID) []string {
	events, _, _ := e.srv.obsStore.QueryNetwork(tid, 0)
	urls := make([]string, 0, len(events))
	for _, ev := range events {
		var d struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		urls = append(urls, d.URL)
	}
	return urls
}

func (e *obsLifeEnv) awaitNetworkURLsOf(tid target.ID, want ...string) {
	e.t.Helper()
	deadline := time.Now().Add(obsLifeWait)
	for {
		got := e.networkURLsOf(tid)
		if slices.Equal(got, want) {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("buffered network events of %s = %v, want %v", tid, got, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// replyEventField lists one data field of every event in an observation reply.
func replyEventField(result map[string]any, field string) []string {
	events, _ := result["events"].([]any)
	out := make([]string, 0, len(events))
	for _, ev := range events {
		m, _ := ev.(map[string]any)
		data, _ := m["data"].(map[string]any)
		v, _ := data[field].(string)
		out = append(out, v)
	}
	return out
}

// queryField runs one observation query through the RPC entry point.
func (e *obsLifeEnv) queryField(method, short, field string) []string {
	e.t.Helper()
	return replyEventField(enumMustRPC(e.t, e.srv, method, map[string]any{"tab": short}), field)
}

func (e *obsLifeEnv) awaitQueryField(method, short, field string, want ...string) {
	e.t.Helper()
	deadline := time.Now().Add(obsLifeWait)
	for {
		got := e.queryField(method, short, field)
		if slices.Equal(got, want) {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("%s of tab %q = %v, want %v", method, short, got, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// replaceTabUnderNewAlias registers obsLifeAliasID and obsLifeTabID together
// after the latter was released: the alias page takes the short id the tab had,
// and the tab's full target id comes back under a longer one.
func (e *obsLifeEnv) replaceTabUnderNewAlias() (newShort string) {
	e.t.Helper()
	if _, ok := e.srv.tabs.Lookup(e.short); ok {
		e.t.Fatal("precondition: the old registration was not released")
	}
	e.st.addPage(obsLifeAliasID, false)
	e.st.addPage(obsLifeTabID, false)
	enumMustRPC(e.t, e.srv, protocol.MethodTabList, nil)
	if tid, _ := e.srv.tabs.Lookup(e.short); tid != obsLifeAliasID {
		e.t.Fatalf("precondition: short id %q names %q, want the other target %q", e.short, tid, obsLifeAliasID)
	}
	newShort, ok := e.srv.tabs.ShortForTarget(obsLifeTabID)
	if !ok || newShort == e.short {
		e.t.Fatalf("precondition: replacement page short id = %q, %v; want a new one", newShort, ok)
	}
	return newShort
}

func assertStaleTabError(t *testing.T, reply enumRPCReply, what string) {
	t.Helper()
	if reply.Error == nil {
		t.Errorf("%s succeeded: tab=%v events=%v", what, reply.Result["tab"], replyEventField(reply.Result, "url"))
		return
	}
	if msg, _ := reply.Error["message"].(string); msg != "unknown tab id" {
		t.Errorf("%s failed with %v, want the stale-tab error (unknown tab id)", what, reply.Error)
	}
}

// Review P1: the first network query of a tab registered the buffers and its
// attach failed. Before it takes the registration back, the tab is released and
// the same full target id is registered again under a new short id, queried and
// receives events. The old query's rollback owns the buffers it created, not
// whatever the target id names by then.
func TestFailedAttachRollbackKeepsReplacementLifecycleBuffers(t *testing.T) {
	e := newObsLifeEnv(t)
	attached, release := e.parkStep(obsStepAttached, obsLifeTabID)
	defer release()
	e.st.failNext("Target.attachToTarget")
	old := e.asyncRPC(protocol.MethodNetwork, map[string]any{"tab": e.short})
	e.await(attached, "the old query's failed attach")
	if n := e.srv.obsStore.TargetCount(); n != 1 {
		t.Fatalf("precondition: old query registered %d bucket(s), want 1", n)
	}

	e.st.destroyPage(obsLifeTabID)
	e.loopIdle()
	newShort := e.replaceTabUnderNewAlias()
	enumMustRPC(t, e.srv, protocol.MethodNetwork, map[string]any{"tab": newShort})
	e.emitRequestOn(obsLifeTabID, "https://new.test/")
	e.awaitNetworkURLsOf(obsLifeTabID, "https://new.test/")

	release()
	got := e.awaitReply(old, "the old query")
	if got.Error == nil {
		t.Errorf("old query succeeded although its attach failed: %v", got.Result)
	}
	if urls := e.networkURLsOf(obsLifeTabID); !slices.Equal(urls, []string{"https://new.test/"}) {
		t.Errorf("replacement lifecycle's buffered events after the old rollback = %v, want [https://new.test/]", urls)
	}
	if got := e.queryField(protocol.MethodNetwork, newShort, "url"); !slices.Equal(got, []string{"https://new.test/"}) {
		t.Errorf("replacement tab's network query = %v, want [https://new.test/]", got)
	}
	// The replacement keeps observing.
	e.emitRequestOn(obsLifeTabID, "https://later.test/")
	e.awaitNetworkURLsOf(obsLifeTabID, "https://new.test/", "https://later.test/")
}

// Review P1: a network query resolved short id c0de and is then held up. The
// page is released, c0de goes to another target and the original full target id
// returns as 0c0de. The old query must not answer with the replacement page's
// events under tab=c0de: its registration is gone, which is a stale tab. The
// same holds when it is held up only after its observer attached.
func TestStaleAliasQueryDoesNotReadReplacementLifecycle(t *testing.T) {
	for _, step := range []string{obsStepResolved, obsStepAttached} {
		t.Run(step, func(t *testing.T) {
			e := newObsLifeEnv(t)
			parked, release := e.parkStep(step, obsLifeTabID)
			defer release()
			old := e.asyncRPC(protocol.MethodNetwork, map[string]any{"tab": e.short})
			e.await(parked, "the old query to pass "+step)

			e.st.destroyPage(obsLifeTabID)
			e.loopIdle()
			newShort := e.replaceTabUnderNewAlias()
			enumMustRPC(t, e.srv, protocol.MethodNetwork, map[string]any{"tab": newShort})
			e.emitRequestOn(obsLifeTabID, "https://replacement.test/")
			e.awaitNetworkURLsOf(obsLifeTabID, "https://replacement.test/")

			release()
			assertStaleTabError(t, e.awaitReply(old, "the old query"), fmt.Sprintf("query of the released tab %q", e.short))

			// Both live pages are observed normally and apart from each other.
			if urls := e.networkURLsOf(obsLifeTabID); !slices.Equal(urls, []string{"https://replacement.test/"}) {
				t.Errorf("replacement page's buffered events = %v, want [https://replacement.test/]", urls)
			}
			if got := e.queryField(protocol.MethodNetwork, e.short, "url"); len(got) != 0 {
				t.Errorf("first query of the target now named %q = %v, want no events", e.short, got)
			}
			e.emitRequestOn(obsLifeAliasID, "https://alias.test/")
			e.awaitQueryField(protocol.MethodNetwork, e.short, "url", "https://alias.test/")
			e.awaitQueryField(protocol.MethodNetwork, newShort, "url", "https://replacement.test/")
			if live := e.st.liveSessions(obsLifeTabID); len(live) != 1 {
				t.Errorf("sessions left attached to the replacement page = %v, want its observer's only", live)
			}
		})
	}
}

// A stale query that attaches its observer only after the release of its tab
// has finished leaves an observer nobody owns on the page that reuses the target
// id. It is retired by the stale query itself, without touching the tab pool
// session the replacement tab is using, and the replacement can still start its
// own observation afterwards.
func TestStaleQueryRetiresOnlyItsOwnLateObserver(t *testing.T) {
	e := newObsLifeEnv(t)
	registered, release := e.parkStep(obsStepRegistered, obsLifeTabID)
	defer release()
	old := e.asyncRPC(protocol.MethodNetwork, map[string]any{"tab": e.short})
	e.await(registered, "the old query to register its buffers")

	e.st.destroyPage(obsLifeTabID)
	e.loopIdle()
	if n := e.srv.obsStore.TargetCount(); n != 0 {
		t.Fatalf("precondition: release left %d bucket(s)", n)
	}
	newShort := e.replaceTabUnderNewAlias()
	enumMustRPC(t, e.srv, protocol.MethodEval, map[string]any{"tab": newShort, "script": "1"})
	pool := e.st.liveSessions(obsLifeTabID)
	if len(pool) != 1 {
		t.Fatalf("precondition: sessions of the replacement tab = %v, want its pool session", pool)
	}

	release()
	assertStaleTabError(t, e.awaitReply(old, "the old query"), "query of the released tab")
	if attached := e.st.sessionCount(obsLifeTabID); attached != 2 {
		t.Fatalf("precondition: sessions ever attached to the target id = %d, want the pool's and the stale observer's", attached)
	}
	if live := e.st.liveSessions(obsLifeTabID); !slices.Equal(live, pool) {
		t.Errorf("sessions left on the replacement tab = %v, want only its pool session %v", live, pool)
	}
	if n := e.srv.obsStore.TargetCount(); n != 0 {
		t.Errorf("stale query left %d bucket(s) registered", n)
	}
	if !e.st.hasPage(obsLifeTabID) || len(e.st.closedTargets()) != 0 {
		t.Error("retiring the stale observer closed a page")
	}
	// The replacement's pool session still works and it can be observed.
	enumMustRPC(t, e.srv, protocol.MethodEval, map[string]any{"tab": newShort, "script": "1"})
	if live := e.st.liveSessions(obsLifeTabID); !slices.Equal(live, pool) {
		t.Errorf("replacement tab had to attach again after the stale query: sessions = %v, want %v", live, pool)
	}
	enumMustRPC(t, e.srv, protocol.MethodNetwork, map[string]any{"tab": newShort})
	e.emitRequestOn(obsLifeTabID, "https://new.test/")
	e.awaitQueryField(protocol.MethodNetwork, newShort, "url", "https://new.test/")
}

// Review P1: Target.targetDestroyed retired the old page while tab_close was
// still waiting for the reply to its own Target.closeTarget, and the same full
// target id was registered again for a replacement page. The late successful
// reply finishes the close of the registration it was sent for; it must not
// remove the replacement's registration or observation buffers. Reproduces with
// and without the short id moving to another target.
func TestStaleTabCloseReplyKeepsReplacementLifecycle(t *testing.T) {
	for _, collision := range []bool{true, false} {
		name := "same_alias"
		if collision {
			name = "alias_reassigned"
		}
		t.Run(name, func(t *testing.T) {
			e := newObsLifeEnv(t)
			e.startObserving("https://old.test/")

			// The browser closes the page at once; only the reply is late.
			closeArrived, releaseClose := e.st.holdNextReply("Target.closeTarget")
			defer releaseClose()
			reply := e.asyncRPC(protocol.MethodTabClose, map[string]any{"tab": e.short})
			e.await(closeArrived, "tab_close to send Target.closeTarget")
			e.st.destroyPage(obsLifeTabID)
			e.loopIdle()
			if n := e.srv.obsStore.TargetCount(); n != 0 {
				t.Fatalf("precondition: Target.targetDestroyed left %d bucket(s)", n)
			}

			newShort := e.short
			if collision {
				newShort = e.replaceTabUnderNewAlias()
				enumMustRPC(t, e.srv, protocol.MethodNetwork, map[string]any{"tab": newShort})
			} else {
				e.st.addPage(obsLifeTabID, false)
				enumMustRPC(t, e.srv, protocol.MethodTabList, nil)
				if tid, _ := e.srv.tabs.Lookup(e.short); tid != obsLifeTabID {
					t.Fatal("precondition: replacement page not registered")
				}
				// tab_close still holds this short id's operation lock, so the
				// replacement's observation is started below the RPC layer.
				if !e.srv.obsStore.RegisterTarget(obsLifeTabID) {
					t.Fatal("precondition: replacement lifecycle found old buffers")
				}
				if err := e.sess.EnsureObservation(context.Background(), obsLifeTabID, browser.ObservationNetwork, e.srv.obsSink, nil); err != nil {
					t.Fatal(err)
				}
			}
			e.emitRequestOn(obsLifeTabID, "https://new.test/")
			e.awaitNetworkURLsOf(obsLifeTabID, "https://new.test/")

			releaseClose()
			if got := e.awaitReply(reply, "tab_close"); got.Error != nil {
				t.Errorf("tab_close of the old page failed: %v", got.Error)
			}
			if !e.st.hasPage(obsLifeTabID) {
				t.Fatal("precondition: the replacement page was closed in the stub")
			}
			if closed := e.st.closedTargets(); len(closed) != 1 {
				t.Errorf("Target.closeTarget calls = %v, want the old page's only", closed)
			}
			if short, ok := e.srv.tabs.ShortForTarget(obsLifeTabID); !ok || short != newShort {
				t.Errorf("replacement page's registration after the old tab_close = %q, %v; want %q", short, ok, newShort)
			}
			if urls := e.networkURLsOf(obsLifeTabID); !slices.Equal(urls, []string{"https://new.test/"}) {
				t.Errorf("replacement page's buffered events after the old tab_close = %v, want [https://new.test/]", urls)
			}
			// The replacement is still a working tab.
			e.emitRequestOn(obsLifeTabID, "https://later.test/")
			e.awaitQueryField(protocol.MethodNetwork, newShort, "url", "https://new.test/", "https://later.test/")
			if collision {
				if tid, _ := e.srv.tabs.Lookup(e.short); tid != obsLifeAliasID {
					t.Errorf("short id %q names %q after the old tab_close, want %q", e.short, tid, obsLifeAliasID)
				}
			}
		})
	}
}

// A live tab is observed in all three domains through the RPC entry point, each
// *_clear empties only its own ring and the tab keeps receiving events.
func TestLiveTabObservationDomainsAndClears(t *testing.T) {
	e := newObsLifeEnv(t)
	for _, m := range []string{protocol.MethodNetwork, protocol.MethodConsole, protocol.MethodErrors} {
		if got := e.queryField(m, e.short, "url"); len(got) != 0 {
			t.Fatalf("first %s query returned events: %v", m, got)
		}
	}
	emitAll := func(tag string) {
		t.Helper()
		sid, _ := e.st.sessionFor(obsLifeTabID)
		e.emitRequestOn(obsLifeTabID, "https://"+tag+".test/")
		e.st.emitSession(sid, "Runtime.consoleAPICalled", map[string]any{
			"type": "log", "args": []any{map[string]any{"type": "string", "value": tag}},
			"executionContextId": 1, "timestamp": 1,
		})
		e.st.emitSession(sid, "Runtime.exceptionThrown", map[string]any{
			"timestamp": 1, "exceptionDetails": map[string]any{
				"exceptionId": 1, "text": tag, "lineNumber": 1, "columnNumber": 1,
			},
		})
	}
	consoleArgs := func() []string {
		t.Helper()
		events, _ := enumMustRPC(t, e.srv, protocol.MethodConsole, map[string]any{"tab": e.short})["events"].([]any)
		var out []string
		for _, ev := range events {
			raw, _ := json.Marshal(ev)
			var d struct {
				Data struct {
					Args []struct {
						Value string `json:"value"`
					} `json:"args"`
				} `json:"data"`
			}
			_ = json.Unmarshal(raw, &d)
			for _, a := range d.Data.Args {
				out = append(out, a.Value)
			}
		}
		return out
	}
	awaitConsole := func(want ...string) {
		t.Helper()
		deadline := time.Now().Add(obsLifeWait)
		for {
			got := consoleArgs()
			if slices.Equal(got, want) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("console events = %v, want %v", got, want)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}

	emitAll("one")
	e.awaitQueryField(protocol.MethodNetwork, e.short, "url", "https://one.test/")
	awaitConsole("one")
	e.awaitQueryField(protocol.MethodErrors, e.short, "text", "one")

	for _, tc := range []struct {
		clear            string
		console, errors_ []string
	}{
		{protocol.MethodNetworkClear, []string{"one"}, []string{"one"}},
		{protocol.MethodConsoleClear, nil, []string{"one"}},
		{protocol.MethodErrorsClear, nil, nil},
	} {
		enumMustRPC(t, e.srv, tc.clear, map[string]any{"tab": e.short})
		if got := e.queryField(protocol.MethodNetwork, e.short, "url"); len(got) != 0 {
			t.Errorf("after %s: network = %v, want none", tc.clear, got)
		}
		if got := consoleArgs(); strings.Join(got, ",") != strings.Join(tc.console, ",") {
			t.Errorf("after %s: console = %v, want %v", tc.clear, got, tc.console)
		}
		if got := e.queryField(protocol.MethodErrors, e.short, "text"); strings.Join(got, ",") != strings.Join(tc.errors_, ",") {
			t.Errorf("after %s: errors = %v, want %v", tc.clear, got, tc.errors_)
		}
	}
	if n := e.srv.obsStore.TargetCount(); n != 1 {
		t.Errorf("target buckets after the clears = %d, want the live tab's 1", n)
	}

	emitAll("two")
	e.awaitQueryField(protocol.MethodNetwork, e.short, "url", "https://two.test/")
	awaitConsole("two")
	e.awaitQueryField(protocol.MethodErrors, e.short, "text", "two")
}
