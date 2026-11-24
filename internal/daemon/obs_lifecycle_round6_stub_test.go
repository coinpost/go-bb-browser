package daemon

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/coinpost/go-bb-browser/pkg/protocol"
)

// Startup and stale-enumeration regressions. Every page here has its own full
// target id; none is ever reused.

// The first enumeration of a starting daemon finds that targets the lifecycle
// loop registered before the request are gone and releases them. The release
// looks up the published session, so startup must not hold the connection lock
// across it: the listener has to come up and cancellation has to end startup.
// The targets are registered before ListenAndServe, which is the state the
// lifecycle loop produces when it handles Target.targetCreated before the
// enumeration token is taken.
func TestStartupEnumerationReleasesVanishedTargetsWithoutDeadlock(t *testing.T) {
	st := newCDPStub(t, obsLifeRootID)
	srv := newStubSessionServer(t, st)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	srv.cfg.ListenAddr = addr

	gone := []target.ID{obsLifeTabID, obsLifeOtherID}
	for _, id := range gone {
		srv.tabs.RegisterPageTarget(id)
		srv.obsStore.RegisterTarget(id)
		srv.obsSink.RecordNetwork(id, 1, json.RawMessage(`{"url":"https://before.test/"}`))
		if events, _, _ := srv.obsStore.QueryNetwork(id, 0); len(events) != 1 {
			t.Fatalf("precondition: %s has %d buffered events, want 1", id, len(events))
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe(ctx) }()

	stuck := func(what string) {
		t.Helper()
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		for _, g := range strings.Split(string(buf[:n]), "\n\n") {
			if strings.Contains(g, "ListenAndServe") && strings.Contains(g, "applyEnumeration") {
				t.Fatalf("%s: startup is blocked inside its first enumeration:\n%s", what, g)
			}
		}
		t.Fatal(what)
	}

	deadline := time.Now().Add(obsLifeWait)
	for {
		res, err := http.Get("http://" + addr + "/health")
		if err == nil {
			code := res.StatusCode
			_ = res.Body.Close()
			if code == http.StatusOK {
				break
			}
		}
		select {
		case err := <-done:
			t.Fatalf("ListenAndServe returned before serving: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			stuck("HTTP listener did not come up")
		}
		time.Sleep(5 * time.Millisecond)
	}

	for _, id := range gone {
		if srv.tabs.Known(id) {
			t.Errorf("%s is still registered after the startup enumeration", id)
		}
		if srv.obsStore.HasTarget(id) {
			t.Errorf("%s kept its observation buffers", id)
		}
		srv.obsSink.RecordNetwork(id, 2, json.RawMessage(`{"url":"https://late.test/"}`))
		if srv.obsStore.HasTarget(id) {
			t.Errorf("a late event rebuilt the buffers of %s", id)
		}
	}
	if n := srv.obsStore.TargetCount(); n != 0 {
		t.Errorf("observation store holds %d targets, want 0", n)
	}
	if !srv.tabs.Has(obsLifeRootID) {
		t.Error("the live page was not registered by the startup enumeration")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ListenAndServe: %v", err)
		}
	case <-time.After(obsLifeWait):
		stuck("ListenAndServe did not return after cancel")
	}
}

// Cancelling a daemon whose startup enumeration released targets must end
// ListenAndServe even when nothing ever connects to the listener.
func TestStartupEnumerationReleaseIsCancellable(t *testing.T) {
	st := newCDPStub(t, obsLifeRootID)
	srv := newStubSessionServer(t, st)
	srv.tabs.RegisterPageTarget(obsLifeTabID)
	srv.tabs.RegisterPageTarget(obsLifeOtherID)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe(ctx) }()
	awaitGetTargets(t, st, 1)
	deadline := time.Now().Add(obsLifeWait)
	for {
		a := srv.tabs.Known(obsLifeTabID)
		b := srv.tabs.Known(obsLifeOtherID)
		if !a && !b {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("startup enumeration did not remove the vanished targets")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ListenAndServe: %v", err)
		}
	case <-time.After(obsLifeWait):
		t.Fatal("ListenAndServe did not return after cancel: startup is blocked releasing a vanished target")
	}
}

// A tab_list reply that was snapshotted before the user closed a tab arrives
// after that tab's release has completed. It must not bring the dead target
// back: a following network query is an unknown tab and creates no buffers,
// and a late event of the old writer is still rejected.
func TestStaleTabListDoesNotResurrectReleasedTarget(t *testing.T) {
	e := newObsLifeEnv(t)
	e.startObserving("https://first.test/")

	snapshot, send := e.st.holdNextReply("Target.getTargets")
	defer send()
	list := e.asyncRPC(protocol.MethodTabList, nil)
	e.await(snapshot, "tab_list snapshot containing the tab")

	e.st.destroyPage(obsLifeTabID)
	e.awaitTargetLoopIdle()
	if e.srv.obsStore.HasTarget(obsLifeTabID) {
		t.Fatal("precondition: release left the observation buffers")
	}
	if _, ok := e.srv.tabs.Lookup(e.tab); ok {
		t.Fatal("precondition: release left the registry entry")
	}

	send()
	reply := e.awaitReply(list, "stale tab_list")
	if reply.Error != nil {
		t.Fatalf("tab_list: %v", reply.Error)
	}
	tabs, _ := reply.Result["tabs"].([]any)
	for _, item := range tabs {
		if m, _ := item.(map[string]any); m["tab"] == e.tab {
			t.Errorf("stale tab_list reported the released tab %q", e.tab)
		}
	}
	if tid, ok := e.srv.tabs.Lookup(e.tab); ok {
		t.Errorf("stale tab_list registered the released target again: %s -> %s", e.tab, tid)
	}
	if e.srv.tabs.Known(obsLifeTabID) {
		t.Error("released target is back in the registry")
	}
	if !e.srv.tabs.Has(obsLifeOtherID) {
		t.Error("stale tab_list dropped a live tab that was registered meanwhile")
	}

	// Chrome refuses to attach to a target that is gone. Hold that refusal so
	// the store can be inspected while the query would be initializing.
	e.st.failNext("Target.attachToTarget")
	attach, refuse := e.st.holdNextReply("Target.attachToTarget")
	defer refuse()
	query := e.asyncRPC(protocol.MethodNetwork, map[string]any{"tab": e.tab})
	var got enumRPCReply
	select {
	case <-attach:
		if e.srv.obsStore.HasTarget(obsLifeTabID) {
			t.Error("a query after the stale tab_list rebuilt the released target's buffers before the attach answered")
			e.srv.obsSink.RecordNetwork(obsLifeTabID, 900, json.RawMessage(`{"url":"https://late.test/"}`))
			if events, _, _ := e.srv.obsStore.QueryNetwork(obsLifeTabID, 0); len(events) != 0 {
				t.Errorf("the rebuilt buffers accepted %d late events", len(events))
			}
		}
		refuse()
		got = e.awaitReply(query, "network query on the released tab")
	case got = <-query:
	case <-time.After(obsLifeWait):
		t.Fatal("network query on the released tab did not answer")
	}
	if got.Error == nil || got.Error["message"] != "unknown tab id" {
		t.Errorf("network on the released tab = %+v, want unknown tab id", got)
	}
	if e.srv.obsStore.HasTarget(obsLifeTabID) {
		t.Error("released target owns observation buffers again")
	}
	e.srv.obsSink.RecordNetwork(obsLifeTabID, 901, json.RawMessage(`{"url":"https://late.test/"}`))
	if e.srv.obsStore.HasTarget(obsLifeTabID) {
		t.Error("a late event rebuilt the released target's buffers")
	}
	if n := e.st.sessionCount(obsLifeTabID); n != 1 {
		t.Errorf("released target has %d attached sessions in total, want only the original observer", n)
	}

	// A fresh enumeration still serves the live tab.
	fresh := enumMustRPC(t, e.srv, protocol.MethodTabList, nil)
	if tabs, _ := fresh["tabs"].([]any); len(tabs) != 2 {
		t.Errorf("fresh tab_list has %d tabs, want the root page and the other live tab: %v", len(tabs), tabs)
	}

	// A failed enumeration ends its window too, so open windows stay bounded.
	e.st.failNext("Target.getTargets")
	if failed, err := enumRPC(e.srv, protocol.MethodTabList, nil); err != nil || failed.Error == nil {
		t.Fatalf("tab_list with a refused Target.getTargets = %+v, %v; want an RPC error", failed, err)
	}
	if n := e.srv.tabs.OpenWindows(); n != 0 {
		t.Errorf("%d windows are still open with none in flight", n)
	}
}

// Chrome answers an explicit tab_close with its definitive target-gone error
// and never delivers Target.targetDestroyed. The RPC keeps reporting the
// failure, but the tab is released like after any other close: registry entry,
// observer writer and all buffers go, and a late event does not rebuild them.
// A close refused for another reason leaves the live tab and its events alone.
func TestExplicitCloseOfGoneTargetReleasesItsObservation(t *testing.T) {
	e := newObsLifeEnv(t)
	e.startObserving("https://first.test/")

	e.st.failNext("Target.closeTarget")
	refused, err := enumRPC(e.srv, protocol.MethodTabClose, map[string]any{"tab": e.tab})
	if err != nil {
		t.Fatal(err)
	}
	if refused.Error == nil || refused.Error["message"] != "failed to close tab" {
		t.Fatalf("refused tab_close = %+v, want failed to close tab", refused)
	}
	if tid, ok := e.srv.tabs.Lookup(e.tab); !ok || tid != obsLifeTabID {
		t.Fatalf("a refused close released the live tab (%q, %v)", tid, ok)
	}
	e.emitRequest("https://second.test/")
	e.awaitNetworkURLs("https://first.test/", "https://second.test/")

	sid, _ := e.st.sessionFor(obsLifeTabID)
	e.st.dropPage(obsLifeTabID)
	e.st.failNextWith("Target.closeTarget", "No target with given id found")
	gone, err := enumRPC(e.srv, protocol.MethodTabClose, map[string]any{"tab": e.tab})
	if err != nil {
		t.Fatal(err)
	}
	if gone.Error == nil || gone.Error["message"] != "failed to close tab" {
		t.Fatalf("tab_close of a gone target = %+v, want the failed to close tab error", gone)
	}
	if tid, ok := e.srv.tabs.Lookup(e.tab); ok {
		t.Errorf("gone target kept its registration: %s -> %s", e.tab, tid)
	}
	if e.srv.obsStore.HasTarget(obsLifeTabID) {
		t.Errorf("gone target kept its observation buffers: %v", e.networkURLs())
	}
	if n := e.srv.obsStore.TargetCount(); n != 0 {
		t.Errorf("observation store holds %d targets, want 0", n)
	}
	// The retired writer no longer records what the old session still emits.
	e.st.emitSession(sid, "Network.loadingFailed", map[string]any{
		"requestId": "req-late", "timestamp": 2, "type": "Document", "errorText": "net::ERR_ABORTED", "canceled": true,
	})
	e.srv.obsSink.RecordNetwork(obsLifeTabID, 902, json.RawMessage(`{"url":"https://late.test/"}`))
	e.awaitTargetLoopIdle()
	if e.srv.obsStore.HasTarget(obsLifeTabID) {
		t.Error("a late event rebuilt the gone target's buffers")
	}
	again, err := enumRPC(e.srv, protocol.MethodNetwork, map[string]any{"tab": e.tab})
	if err != nil {
		t.Fatal(err)
	}
	if again.Error == nil || again.Error["message"] != "unknown tab id" {
		t.Errorf("network after the release = %+v, want unknown tab id", again)
	}
}
