package daemon

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/yiplee/go-bb-browser/pkg/protocol"
)

// Helpers and barrier regressions for observation bucket ownership and live
// domain clears with a real browser.Session, the daemon's RPC entry point and
// the local CDP protocol stub.

var obsLifeIdleSeq atomic.Uint64

// loopIdle is awaitTargetLoopIdle for tests that need it more than once.
func (e *obsLifeEnv) loopIdle() {
	e.t.Helper()
	id := target.ID(fmt.Sprintf("FFFF00000000000000000000%08d", obsLifeIdleSeq.Add(1)))
	e.st.addPage(id, true)
	deadline := time.Now().Add(obsLifeWait)
	for {
		if e.srv.tabs.Has(id) {
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
func (e *obsLifeEnv) queryField(method, tab, field string) []string {
	e.t.Helper()
	return replyEventField(enumMustRPC(e.t, e.srv, method, map[string]any{"tab": tab}), field)
}

func (e *obsLifeEnv) awaitQueryField(method, tab, field string, want ...string) {
	e.t.Helper()
	deadline := time.Now().Add(obsLifeWait)
	for {
		got := e.queryField(method, tab, field)
		if slices.Equal(got, want) {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("%s of tab %q = %v, want %v", method, tab, got, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
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

// A stale query that attaches its observer only after the release of its tab
// has finished leaves an observer nobody owns. It is retired by the stale query
// itself via ForgetObservation (leaving any tab pool session alone), and a
// live tab can still be observed normally.
func TestStaleQueryRetiresOnlyItsOwnLateObserver(t *testing.T) {
	e := newObsLifeEnv(t)
	registered, release := e.parkStep(obsStepRegistered, obsLifeTabID)
	defer release()
	old := e.asyncRPC(protocol.MethodNetwork, map[string]any{"tab": e.tab})
	e.await(registered, "the old query to register its buffers")

	e.st.destroyPage(obsLifeTabID)
	e.loopIdle()
	if n := e.srv.obsStore.TargetCount(); n != 0 {
		t.Fatalf("precondition: release left %d bucket(s)", n)
	}
	e.st.addPage(obsLifeTabID, false)
	enumMustRPC(t, e.srv, protocol.MethodTabList, nil)
	// old still holds the keyed lock for e.tab, so install the pool session
	// directly on the Session to verify ForgetObservation leaves tabPool alone.
	if _, err := e.sess.Eval(obsLifeTabID, "1"); err != nil {
		t.Fatalf("precondition: session Eval failed: %v", err)
	}
	pool := e.st.liveSessions(obsLifeTabID)
	if len(pool) != 1 {
		t.Fatalf("precondition: sessions = %v, want its pool session", pool)
	}

	release()
	assertStaleTabError(t, e.awaitReply(old, "the old query"), "query of the released tab")
	if attached := e.st.sessionCount(obsLifeTabID); attached != 2 {
		t.Fatalf("precondition: sessions ever attached to the target id = %d, want the pool's and the stale observer's", attached)
	}
	if live := e.st.liveSessions(obsLifeTabID); !slices.Equal(live, pool) {
		t.Errorf("sessions left = %v, want only the pool session %v", live, pool)
	}
	if n := e.srv.obsStore.TargetCount(); n != 0 {
		t.Errorf("stale query left %d bucket(s) registered", n)
	}
	if !e.st.hasPage(obsLifeTabID) || len(e.st.closedTargets()) != 0 {
		t.Error("retiring the stale observer closed a page")
	}
	enumMustRPC(t, e.srv, protocol.MethodEval, map[string]any{"tab": e.tab, "script": "1"})
	if live := e.st.liveSessions(obsLifeTabID); !slices.Equal(live, pool) {
		t.Errorf("tab had to attach again after the stale query: sessions = %v, want %v", live, pool)
	}
	enumMustRPC(t, e.srv, protocol.MethodNetwork, map[string]any{"tab": e.tab})
	e.emitRequestOn(obsLifeTabID, "https://new.test/")
	e.awaitQueryField(protocol.MethodNetwork, e.tab, "url", "https://new.test/")
}

// A live tab is observed in all three domains through the RPC entry point, each
// *_clear empties only its own ring and the tab keeps receiving events.
func TestLiveTabObservationDomainsAndClears(t *testing.T) {
	e := newObsLifeEnv(t)
	for _, m := range []string{protocol.MethodNetwork, protocol.MethodConsole, protocol.MethodErrors} {
		if got := e.queryField(m, e.tab, "url"); len(got) != 0 {
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
		events, _ := enumMustRPC(t, e.srv, protocol.MethodConsole, map[string]any{"tab": e.tab})["events"].([]any)
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
	e.awaitQueryField(protocol.MethodNetwork, e.tab, "url", "https://one.test/")
	awaitConsole("one")
	e.awaitQueryField(protocol.MethodErrors, e.tab, "text", "one")

	for _, tc := range []struct {
		clear            string
		console, errors_ []string
	}{
		{protocol.MethodNetworkClear, []string{"one"}, []string{"one"}},
		{protocol.MethodConsoleClear, nil, []string{"one"}},
		{protocol.MethodErrorsClear, nil, nil},
	} {
		enumMustRPC(t, e.srv, tc.clear, map[string]any{"tab": e.tab})
		if got := e.queryField(protocol.MethodNetwork, e.tab, "url"); len(got) != 0 {
			t.Errorf("after %s: network = %v, want none", tc.clear, got)
		}
		if got := consoleArgs(); strings.Join(got, ",") != strings.Join(tc.console, ",") {
			t.Errorf("after %s: console = %v, want %v", tc.clear, got, tc.console)
		}
		if got := e.queryField(protocol.MethodErrors, e.tab, "text"); strings.Join(got, ",") != strings.Join(tc.errors_, ",") {
			t.Errorf("after %s: errors = %v, want %v", tc.clear, got, tc.errors_)
		}
	}
	if n := e.srv.obsStore.TargetCount(); n != 1 {
		t.Errorf("target buckets after the clears = %d, want the live tab's 1", n)
	}

	emitAll("two")
	e.awaitQueryField(protocol.MethodNetwork, e.tab, "url", "https://two.test/")
	awaitConsole("two")
	e.awaitQueryField(protocol.MethodErrors, e.tab, "text", "two")
}
