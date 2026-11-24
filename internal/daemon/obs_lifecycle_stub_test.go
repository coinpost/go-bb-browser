package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/coinpost/go-bb-browser/internal/browser"
	"github.com/coinpost/go-bb-browser/pkg/protocol"
)

// These regressions drive a real browser.Session and the daemon against the
// local CDP protocol stub. Each one parks the browser or the observer writer at
// a barrier inside a target release and then checks what the rest of the
// daemon is allowed to see meanwhile.

const (
	obsLifeRootID  = target.ID("AAAA0000000000000000000000000001")
	obsLifeTabID   = target.ID("BBBB000000000000000000000000C0DE")
	obsLifeOtherID = target.ID("CCCC000000000000000000000000BEEF")
	obsLifeWait    = 10 * time.Second
)

type obsLifeEnv struct {
	t    *testing.T
	st   *cdpStub
	srv  *Server
	sess *browser.Session
	tab  string
}

// newObsLifeEnv connects a real Session to the stub, runs the daemon's target
// lifecycle loop and registers obsLifeTabID. The session's own root context is
// attached to a different page, so the tab under test has no child context yet.
func newObsLifeEnv(t *testing.T) *obsLifeEnv {
	t.Helper()
	st := newCDPStub(t, obsLifeRootID, obsLifeTabID)
	srv := newStubSessionServer(t, st)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sess, err := browser.Connect(ctx, st.url(), browser.WithTargetObserver(srv.tabs))
	if err != nil {
		t.Fatal(err)
	}
	srv.tabMu.Lock()
	srv.tabLive = sess
	srv.browserState = sessionReady
	srv.tabMu.Unlock()
	go srv.runTargetSync(ctx, sess)
	enumMustRPC(t, srv, protocol.MethodTabList, nil)
	if !srv.tabs.Has(obsLifeTabID) {
		t.Fatal("precondition: tab not registered")
	}
	return &obsLifeEnv{t: t, st: st, srv: srv, sess: sess, tab: string(obsLifeTabID)}
}

func (e *obsLifeEnv) await(ch <-chan struct{}, what string) {
	e.t.Helper()
	select {
	case <-ch:
	case <-time.After(obsLifeWait):
		e.t.Fatalf("timed out waiting for %s", what)
	}
}

// emitRequest delivers one Network.requestWillBeSent on the newest session
// attached to the tab, which is its current observer.
func (e *obsLifeEnv) emitRequest(url string) {
	e.t.Helper()
	sid, ok := e.st.sessionFor(obsLifeTabID)
	if !ok {
		e.t.Fatal("no session attached to the tab")
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

func (e *obsLifeEnv) networkURLs() []string {
	events, _, _ := e.srv.obsStore.QueryNetwork(obsLifeTabID, 0)
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

func (e *obsLifeEnv) awaitNetworkURLs(want ...string) {
	e.t.Helper()
	deadline := time.Now().Add(obsLifeWait)
	for {
		got := e.networkURLs()
		if strings.Join(got, ",") == strings.Join(want, ",") {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("buffered network events = %v, want %v", got, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// startObserving runs the first network query (attach + register) and buffers
// one event of the tab's current lifecycle.
func (e *obsLifeEnv) startObserving(url string) {
	e.t.Helper()
	enumMustRPC(e.t, e.srv, protocol.MethodNetwork, map[string]any{"tab": e.tab})
	e.emitRequest(url)
	e.awaitNetworkURLs(url)
}

// awaitTargetLoopIdle proves the lifecycle loop has finished every removal it
// was handed: the loop is sequential, so it can only register a page announced
// now after returning from them.
func (e *obsLifeEnv) awaitTargetLoopIdle() {
	e.t.Helper()
	e.st.addPage(obsLifeOtherID, true)
	deadline := time.Now().Add(obsLifeWait)
	for {
		if e.srv.tabs.Has(obsLifeOtherID) {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatal("target lifecycle loop did not finish the pending removal")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (e *obsLifeEnv) asyncRPC(method string, params map[string]any) <-chan enumRPCReply {
	out := make(chan enumRPCReply, 1)
	go func() {
		reply, err := enumRPC(e.srv, method, params)
		if err != nil {
			reply.Error = map[string]any{"message": err.Error()}
		}
		out <- reply
	}()
	return out
}

func (e *obsLifeEnv) awaitReply(ch <-chan enumRPCReply, what string) enumRPCReply {
	e.t.Helper()
	select {
	case reply := <-ch:
		return reply
	case <-time.After(obsLifeWait):
		e.t.Fatalf("timed out waiting for %s", what)
		return enumRPCReply{}
	}
}

func (e *obsLifeEnv) assertReleased(when string) {
	e.t.Helper()
	if urls := e.networkURLs(); len(urls) != 0 {
		e.t.Errorf("%s: network events %v still buffered for the released target (INV-6)", when, urls)
	}
	if n := e.srv.obsStore.TargetCount(); n != 0 {
		e.t.Errorf("%s: observation store holds %d target bucket(s), want 0", when, n)
	}
}

// Review P1: Target.targetDestroyed arrives while the observer's chromedp child
// is still attaching (attachToTarget answered, the next command pending). The
// release must not cancel that child underneath the attach: chromedp's cancel
// watcher reads Context.Target while the attach writes it. Run under -race.
func TestTargetDestroyedDuringObserverAttach(t *testing.T) {
	e := newObsLifeEnv(t)
	arrived, release := e.st.holdNext("Runtime.enable")
	defer release()
	reply := e.asyncRPC(protocol.MethodNetwork, map[string]any{"tab": e.tab})
	e.await(arrived, "the observer attach to reach its first session command")
	if n := e.st.sessionCount(obsLifeTabID); n != 1 {
		t.Fatalf("sessions attached to the tab = %d, want the observer's only", n)
	}

	e.st.destroyPage(obsLifeTabID)
	// The reply stays parked: release has to abort the attach by itself.
	got := e.awaitReply(reply, "the query racing the target removal")
	if got.Error == nil {
		t.Errorf("query of a target destroyed during attach succeeded: %v", got.Result)
	}
	e.awaitTargetLoopIdle()
	if _, ok := e.srv.tabs.Lookup(e.tab); ok {
		t.Error("destroyed target is still registered")
	}
	e.assertReleased("after the removal finished")
	if stats := e.sess.ObservationStats(obsLifeTabID, browser.ObservationNetwork); stats != (browser.ObservationStats{}) {
		t.Errorf("observer of the destroyed target is still installed: %+v", stats)
	}
}

// Same race, unparked: the attachToTarget reply and Target.targetDestroyed are
// let go together, so the release lands anywhere inside the attach, including
// the window in which chromedp stores Context.Target. Run under -race.
func TestTargetDestroyedRacingObserverAttachReply(t *testing.T) {
	e := newObsLifeEnv(t)
	for round := range 40 {
		tid := target.ID(fmt.Sprintf("DDDD00000000000000000000000%05d", round))
		e.st.addPage(tid, false)
		enumMustRPC(t, e.srv, protocol.MethodTabList, nil)
		if !e.srv.tabs.Has(tid) {
			t.Fatalf("round %d: tab not registered", round)
		}
		arrived, release := e.st.holdNext("Target.attachToTarget")
		reply := e.asyncRPC(protocol.MethodNetwork, map[string]any{"tab": string(tid)})
		e.await(arrived, "the observer attach to start")
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			release()
		}()
		go func() {
			defer wg.Done()
			<-start
			e.st.destroyPage(tid)
		}()
		close(start)
		wg.Wait()
		e.awaitReply(reply, "the query racing the target removal")
		deadline := time.Now().Add(obsLifeWait)
		for {
			if !e.srv.tabs.Has(tid) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("round %d: destroyed target is still registered", round)
			}
			time.Sleep(time.Millisecond)
		}
	}
	e.awaitTargetLoopIdle()
	if n := e.srv.obsStore.TargetCount(); n != 0 {
		t.Errorf("observation store holds %d target bucket(s) after every target was destroyed", n)
	}
}

// holdingSink records through the daemon's real sink but parks every Record
// after the writer dequeued its event.
type holdingSink struct {
	*obsSink
	entered chan struct{}
	release chan struct{}
}

func (h *holdingSink) RecordNetwork(id target.ID, seq uint64, data json.RawMessage) {
	select {
	case h.entered <- struct{}{}:
	default:
	}
	<-h.release
	h.obsSink.RecordNetwork(id, seq, data)
}

// Review P1: the first ForgetTarget has taken the observer out of the table and
// is waiting for an in-flight Record. A second release of the same target has
// to wait at the same barrier; otherwise its caller clears and re-registers the
// buffers and the old Record then lands in the new lifecycle.
func TestRepeatedForgetTargetWaitsForInFlightRecord(t *testing.T) {
	e := newObsLifeEnv(t)
	sink := &holdingSink{obsSink: e.srv.obsSink, entered: make(chan struct{}, 1), release: make(chan struct{})}
	released := false
	releaseRecord := func() {
		if !released {
			released = true
			close(sink.release)
		}
	}
	defer releaseRecord()
	// The observer keeps the recorder it was created with.
	if err := e.sess.EnsureObservation(context.Background(), obsLifeTabID, browser.ObservationNetwork, sink, nil); err != nil {
		t.Fatal(err)
	}
	e.srv.obsStore.RegisterTarget(obsLifeTabID)
	e.emitRequest("https://old.test/")
	e.await(sink.entered, "the writer to reach Record")

	first := make(chan struct{})
	go func() {
		defer close(first)
		e.sess.ForgetTarget(obsLifeTabID)
	}()
	deadline := time.Now().Add(obsLifeWait)
	for e.sess.ObservationStats(obsLifeTabID, browser.ObservationNetwork).Cursor != 0 {
		if time.Now().After(deadline) {
			t.Fatal("first ForgetTarget did not take the observer out of the table")
		}
		time.Sleep(time.Millisecond)
	}

	second := make(chan struct{})
	go func() {
		defer close(second)
		// The daemon's release sequence, followed by a new lifecycle of the id.
		e.sess.ForgetTarget(obsLifeTabID)
		e.srv.obsStore.ClearTarget(obsLifeTabID)
		e.srv.obsStore.RegisterTarget(obsLifeTabID)
	}()
	select {
	case <-second:
		t.Error("second ForgetTarget returned while the old writer was still inside Record")
	case <-time.After(200 * time.Millisecond):
	}
	releaseRecord()
	e.await(first, "the first ForgetTarget")
	e.await(second, "the second ForgetTarget")
	if urls := e.networkURLs(); len(urls) != 0 {
		t.Errorf("new lifecycle received the old writer's events: %v", urls)
	}
}

// Review P1: the old lifecycle's release is parked on the browser's reply to
// Target.detachFromTarget when the same target id is registered and queried
// again. The new lifecycle must neither read the old buffers nor lose its own
// buffers to the old release finishing later.
func TestReusedTargetIDWaitsForOldLifecycleRelease(t *testing.T) {
	e := newObsLifeEnv(t)
	e.startObserving("https://old.test/")

	detachArrived, releaseDetach := e.st.holdNext("Target.detachFromTarget")
	defer releaseDetach()
	e.st.destroyPage(obsLifeTabID)
	e.await(detachArrived, "the old lifecycle's release to reach Target.detachFromTarget")

	// The id comes back and is registered again by a full enumeration.
	e.st.addPage(obsLifeTabID, false)
	enumMustRPC(t, e.srv, protocol.MethodTabList, nil)
	if _, ok := e.srv.tabs.Lookup(e.tab); !ok {
		t.Fatal("precondition: reused target id not registered again")
	}

	reply := e.asyncRPC(protocol.MethodNetwork, map[string]any{"tab": e.tab})
	var got enumRPCReply
	answered := false
	select {
	case got = <-reply:
		answered = true
	case <-time.After(300 * time.Millisecond):
	}
	releaseDetach()
	if !answered {
		got = e.awaitReply(reply, "the new lifecycle's query")
	}
	if got.Error != nil {
		t.Fatalf("query of the new lifecycle failed: %v", got.Error)
	}
	if events, _ := got.Result["events"].([]any); len(events) != 0 {
		t.Errorf("new lifecycle read %d event(s) buffered by the old one: %v", len(events), events)
	}

	// The old release is over. Whatever it cleared, the new lifecycle's buffers
	// have to be in place for its own observer.
	e.awaitTargetLoopIdle()
	e.emitRequest("https://new.test/")
	e.awaitNetworkURLs("https://new.test/")
}

// Review P1: Target.targetDestroyed overtakes the reply to the RPC's own
// Target.closeTarget. The event handler removes the registry entry and is still
// retiring the target when tab_close resumes; tab_close may only report success
// once that release has finished, not because the entry is already gone.
func TestTabCloseWaitsForReleaseStartedByTargetDestroyed(t *testing.T) {
	e := newObsLifeEnv(t)
	e.startObserving("https://old.test/")

	closeArrived, releaseClose := e.st.holdNext("Target.closeTarget")
	defer releaseClose()
	detachArrived, releaseDetach := e.st.holdNext("Target.detachFromTarget")
	defer releaseDetach()

	reply := e.asyncRPC(protocol.MethodTabClose, map[string]any{"tab": e.tab})
	e.await(closeArrived, "tab_close to send Target.closeTarget")
	e.st.destroyPage(obsLifeTabID)
	e.await(detachArrived, "the event handler's release to reach Target.detachFromTarget")
	if _, ok := e.srv.tabs.Lookup(e.tab); ok {
		t.Fatal("precondition: event handler has not removed the registry entry")
	}

	releaseClose()
	unpark := time.AfterFunc(150*time.Millisecond, releaseDetach)
	defer unpark.Stop()
	got := e.awaitReply(reply, "tab_close")
	if got.Error != nil {
		t.Fatalf("tab_close failed: %v", got.Error)
	}
	e.assertReleased("when tab_close reported success")
}

// Review note: a first attach that fails must take back the registration the
// query made, while a failure on a target that is already being observed must
// leave its buffered data alone.
func TestFailedObservationAttachRollsBackOnlyItsOwnRegistration(t *testing.T) {
	e := newObsLifeEnv(t)
	e.st.failNext("Target.attachToTarget")
	reply := e.awaitReply(e.asyncRPC(protocol.MethodNetwork, map[string]any{"tab": e.tab}), "the failing query")
	if reply.Error == nil {
		t.Fatalf("query succeeded although the attach was refused: %v", reply.Result)
	}
	if _, ok := e.srv.tabs.Lookup(e.tab); !ok {
		t.Fatal("a failed attach removed the tab")
	}
	if n := e.srv.obsStore.TargetCount(); n != 0 {
		t.Errorf("failed first attach left %d target bucket(s) registered", n)
	}
	if e.srv.obsStore.PushNetwork(obsLifeTabID, 1, json.RawMessage(`{}`)) {
		t.Error("late event accepted for a target whose observation never started")
	}

	e.startObserving("https://kept.test/")
	e.st.failNext("Runtime.enable") // console activation on the live observer
	reply = e.awaitReply(e.asyncRPC(protocol.MethodConsole, map[string]any{"tab": e.tab}), "the failing console query")
	if reply.Error == nil {
		t.Fatalf("console query succeeded although Runtime.enable was refused: %v", reply.Result)
	}
	e.awaitNetworkURLs("https://kept.test/")
	if n := e.srv.obsStore.TargetCount(); n != 1 {
		t.Errorf("target buckets = %d, want the working observation's 1", n)
	}
}
