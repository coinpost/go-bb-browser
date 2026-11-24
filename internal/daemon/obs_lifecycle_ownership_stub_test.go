package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/coinpost/go-bb-browser/internal/browser"
	"github.com/coinpost/go-bb-browser/pkg/protocol"
)

// More barrier regressions with a real browser.Session, the daemon and the
// local CDP protocol stub. These are about who owns what at the edges of a
// lifecycle: the CDP session of a child context, a retirement shared by Close
// and ForgetTarget, a target id resolved before its release, and an attach the
// browser answers late.

// attachKindsRPC is the first RPC that makes the session attach a child context
// to the tab: the observer's, or the tab pool's.
var attachKindsRPC = []struct {
	name   string
	method string
	params func(tab string) map[string]any
}{
	{"observer", protocol.MethodNetwork, func(tab string) map[string]any { return map[string]any{"tab": tab} }},
	{"pool", protocol.MethodEval, func(tab string) map[string]any { return map[string]any{"tab": tab, "script": "1"} }},
}

// Review P1: the old lifecycle's release is parked on the reply to
// Target.detachFromTarget while the same target id comes back as a replacement
// page. Finishing the release may only detach the old CDP session: a
// Target.closeTarget by id would close the replacement.
func TestReleaseOfReusedTargetIDKeepsReplacementPage(t *testing.T) {
	for _, kind := range attachKindsRPC {
		t.Run(kind.name, func(t *testing.T) {
			e := newObsLifeEnv(t)
			enumMustRPC(t, e.srv, kind.method, kind.params(e.tab))
			old := e.st.liveSessions(obsLifeTabID)
			if len(old) != 1 {
				t.Fatalf("precondition: sessions attached to the tab = %v, want one", old)
			}

			detachArrived, releaseDetach := e.st.holdNext("Target.detachFromTarget")
			defer releaseDetach()
			e.st.destroyPage(obsLifeTabID)
			e.await(detachArrived, "the old lifecycle's release to reach Target.detachFromTarget")

			e.st.addPage(obsLifeTabID, false)
			enumMustRPC(t, e.srv, protocol.MethodTabList, nil)
			if _, ok := e.srv.tabs.Lookup(e.tab); !ok {
				t.Fatal("precondition: reused target id not registered again")
			}

			releaseDetach()
			e.awaitTargetLoopIdle() // the old release, child cancellation included, is over
			if closed := e.st.closedTargets(); len(closed) != 0 {
				t.Errorf("releasing the old lifecycle sent Target.closeTarget for %v", closed)
			}
			if !e.st.hasPage(obsLifeTabID) {
				t.Error("replacement page no longer exists after the old lifecycle's release")
			}
			if live := e.st.liveSessions(obsLifeTabID); len(live) != 0 {
				t.Errorf("old lifecycle's session(s) %v were not detached", live)
			}
			if _, ok := e.srv.tabs.Lookup(e.tab); !ok {
				t.Error("replacement page lost its registration")
			}
		})
	}
}

// Review P1: Session.Close and ForgetTarget retire the same writer. Whichever
// takes the observer out of the table first, the other one must not return
// while the old writer is still inside Record: its caller goes on to clear and
// re-register the buffers, and the old event would land in the new lifecycle.
func TestSessionCloseAndForgetTargetShareRetirementBarrier(t *testing.T) {
	for _, tc := range []struct {
		name          string
		first, second func(*browser.Session)
	}{
		{"close_then_forget", (*browser.Session).Close, func(s *browser.Session) { s.ForgetTarget(obsLifeTabID) }},
		{"forget_then_close", func(s *browser.Session) { s.ForgetTarget(obsLifeTabID) }, (*browser.Session).Close},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			if err := e.sess.EnsureObservation(context.Background(), obsLifeTabID, browser.ObservationNetwork, sink, nil); err != nil {
				t.Fatal(err)
			}
			e.srv.obsStore.RegisterTarget(obsLifeTabID)
			e.emitRequest("https://old.test/")
			e.await(sink.entered, "the writer to reach Record")

			first := make(chan struct{})
			go func() {
				defer close(first)
				tc.first(e.sess)
			}()
			deadline := time.Now().Add(obsLifeWait)
			for e.sess.ObservationStats(obsLifeTabID, browser.ObservationNetwork).Cursor != 0 {
				if time.Now().After(deadline) {
					t.Fatal("the first retirement did not take the observer out of the table")
				}
				time.Sleep(time.Millisecond)
			}

			second := make(chan struct{})
			go func() {
				defer close(second)
				tc.second(e.sess)
				// What the caller does once the writer is said to be retired.
				e.srv.obsStore.ClearTarget(obsLifeTabID)
				e.srv.obsStore.RegisterTarget(obsLifeTabID)
			}()
			select {
			case <-second:
				t.Error("the second retirement returned while the old writer was still inside Record")
			case <-time.After(200 * time.Millisecond):
			}
			select {
			case <-first:
				t.Error("the first retirement returned while the old writer was still inside Record")
			default:
			}
			releaseRecord()
			e.await(first, "the first retirement")
			e.await(second, "the second retirement")
			if urls := e.networkURLs(); len(urls) != 0 {
				t.Errorf("new lifecycle received the old writer's events: %v", urls)
			}
		})
	}
}

// Review P1: a query resolved its target id, and then the target was destroyed
// and its release ran to completion. The late registration must not bring the
// buffers back, not even for the time the doomed attach takes to fail.
func TestStaleResolvedTargetCannotRegisterAfterRelease(t *testing.T) {
	e := newObsLifeEnv(t)
	ctx := context.Background()
	// The first half of handleObsQuery ...
	tid, err := e.srv.resolveTab(ctx, e.sess, e.tab)
	if err != nil || tid != obsLifeTabID {
		t.Fatalf("precondition: resolveTab = %q, %v", tid, err)
	}
	// ... the complete release in between ...
	e.st.destroyPage(obsLifeTabID)
	e.awaitTargetLoopIdle()
	if _, ok := e.srv.tabs.Lookup(e.tab); ok {
		t.Fatal("precondition: destroyed target is still registered")
	}

	// ... and its second half, with the attach it would start held back.
	attachArrived, releaseAttach := e.st.holdNext("Target.attachToTarget")
	defer releaseAttach()
	result := make(chan error, 1)
	go func() {
		_, err := e.srv.enableObservation(ctx, e.sess, tid, browser.ObservationNetwork)
		result <- err
	}()
	select {
	case err = <-result:
	case <-attachArrived:
		t.Error("stale query started an observer attach for a released target")
		if n := e.srv.obsStore.TargetCount(); n != 0 {
			t.Errorf("stale query registered %d target bucket(s) for a released target", n)
		}
		releaseAttach()
		err = <-result
	case <-time.After(obsLifeWait):
		t.Fatal("timed out waiting for the stale query")
	}
	if !errors.Is(err, errObservationTargetGone) {
		t.Errorf("stale query returned %v, want errObservationTargetGone", err)
	}
	e.assertReleased("after the stale query")
	if e.srv.obsStore.PushNetwork(obsLifeTabID, 1, json.RawMessage(`{}`)) {
		t.Error("late event accepted for the released target")
	}

	// The same request through the RPC entry point is an unknown tab.
	reply := e.awaitReply(e.asyncRPC(protocol.MethodNetwork, map[string]any{"tab": e.tab}), "the query of the released tab")
	if reply.Error == nil {
		t.Errorf("query of a released tab succeeded: %v", reply.Result)
	}
	e.assertReleased("after querying the released tab")
}

// Review P1: the bounded attach gives up while Target.attachToTarget is still
// unanswered, and the browser answers afterwards. The session announced by that
// late reply still belongs to the attach that asked for it and has to be
// detached; otherwise every retry leaves one more session on the page.
func TestTimedOutAttachDetachesLateSession(t *testing.T) {
	for _, kind := range attachKindsRPC {
		t.Run(kind.name, func(t *testing.T) {
			e := newObsLifeEnv(t)
			e.sess.SetAttachTimeout(100 * time.Millisecond)
			attachArrived, releaseAttach := e.st.holdNext("Target.attachToTarget")
			defer releaseAttach()
			reply := e.awaitReply(e.asyncRPC(kind.method, kind.params(e.tab)), "the attach to time out")
			if reply.Error == nil {
				t.Fatalf("call succeeded although the attach was never answered: %v", reply.Result)
			}
			e.await(attachArrived, "Target.attachToTarget")
			if n := e.st.sessionCount(obsLifeTabID); n != 0 {
				t.Fatalf("precondition: %d session(s) attached before the browser answered", n)
			}
			e.sess.SetAttachTimeout(0)

			// The late reply: the browser did attach a session.
			releaseAttach()
			deadline := time.Now().Add(obsLifeWait)
			for e.st.sessionCount(obsLifeTabID) != 1 {
				if time.Now().After(deadline) {
					t.Fatal("stub did not attach the late session")
				}
				time.Sleep(time.Millisecond)
			}
			late, _ := e.st.sessionFor(obsLifeTabID)

			// The retry attaches afresh and works.
			enumMustRPC(t, e.srv, kind.method, kind.params(e.tab))
			current, _ := e.st.sessionFor(obsLifeTabID)
			if current == late {
				t.Fatal("precondition: the retry did not attach a session of its own")
			}
			for {
				live := e.st.liveSessions(obsLifeTabID)
				if len(live) == 1 && live[0] == current {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("sessions left attached to the tab = %v, want only the retry's %q (late session %q must be detached)", live, current, late)
				}
				time.Sleep(2 * time.Millisecond)
			}
			if slices.Contains(e.st.closedTargets(), obsLifeTabID) || !e.st.hasPage(obsLifeTabID) {
				t.Error("cleaning up the late session closed the page")
			}
			if kind.method == protocol.MethodNetwork {
				e.emitRequest("https://retry.test/")
				e.awaitNetworkURLs("https://retry.test/")
			}
		})
	}
}

// Review P1: the observer's attach has succeeded and the first query is in its
// domain initialization (Network.disable pending) when Session.Close retires
// the writer. The query must not report a working observation with a cursor,
// and the registration it made is taken back.
func TestSessionCloseDuringDomainInitializationFailsFirstQuery(t *testing.T) {
	e := newObsLifeEnv(t)
	disableArrived, releaseDisable := e.st.holdNext("Network.disable")
	defer releaseDisable()
	reply := e.asyncRPC(protocol.MethodNetwork, map[string]any{"tab": e.tab})
	e.await(disableArrived, "the first query to reach its domain initialization")

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		e.sess.Close()
	}()
	e.await(closed, "Session.Close")
	releaseDisable()

	got := e.awaitReply(reply, "the first query")
	if got.Error == nil {
		t.Errorf("first query succeeded on a closed session: %v", got.Result)
	}
	if n := e.srv.obsStore.TargetCount(); n != 0 {
		t.Errorf("failed first query left %d target bucket(s) registered", n)
	}
	if e.srv.obsStore.PushNetwork(obsLifeTabID, 1, json.RawMessage(`{}`)) {
		t.Error("late event accepted for a target whose observation never started")
	}
	if _, ok := e.srv.tabs.Lookup(e.tab); !ok {
		t.Error("closing the session removed the tab")
	}
}
