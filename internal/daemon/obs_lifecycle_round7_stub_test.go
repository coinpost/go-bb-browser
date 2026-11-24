package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/coinpost/go-bb-browser/internal/browser"
	"github.com/coinpost/go-bb-browser/pkg/protocol"
)

// Concurrent observation query vs user close: when an already-attached query
// loses ownership to a concurrent target release whose writer is still blocked
// inside RecordNetwork, the stale query must wait for writer retirement to
// finish before releasing its bucket handle (strict writer -> bucket order).
func TestConcurrentObservationQueryWaitsForWriterRetirementBeforeClearingBucket(t *testing.T) {
	e := newObsLifeEnv(t)
	sink := &holdingSink{
		obsSink: e.srv.obsSink,
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	releaseRecord := func() { closeOnce(sink.release) }
	defer releaseRecord()

	if err := e.sess.EnsureObservation(context.Background(), obsLifeTabID, browser.ObservationNetwork, sink, nil); err != nil {
		t.Fatal(err)
	}
	e.srv.obsStore.RegisterTarget(obsLifeTabID)

	atAttached, releaseAttached := e.parkStep(obsStepAttached, obsLifeTabID)
	defer releaseAttached()

	query := e.asyncRPC(protocol.MethodNetwork, map[string]any{"tab": e.tab})
	e.await(atAttached, "concurrent observation query to reach obsStepAttached")

	e.emitRequest("https://in-flight.test/")
	e.await(sink.entered, "observer writer to enter RecordNetwork")

	e.st.destroyPage(obsLifeTabID)
	deadline := time.Now().Add(obsLifeWait)
	for {
		known := e.srv.tabs.Known(obsLifeTabID)
		stats := e.sess.ObservationStats(obsLifeTabID, browser.ObservationNetwork)
		if !known && stats.Cursor == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for release to start retiring observer (known=%v cursor=%d)", known, stats.Cursor)
		}
		time.Sleep(time.Millisecond)
	}
	if !e.srv.obsStore.HasTarget(obsLifeTabID) {
		t.Fatal("precondition: bucket cleared before writer retired")
	}

	releaseAttached()
	select {
	case got := <-query:
		t.Fatalf("stale observation query returned before writer retirement completed: %+v", got)
	case <-time.After(150 * time.Millisecond):
	}
	if !e.srv.obsStore.HasTarget(obsLifeTabID) {
		t.Fatal("stale observation query cleared bucket before writer retired")
	}

	releaseRecord()
	got := e.awaitReply(query, "stale observation query after writer retirement")
	if got.Error == nil || got.Error["message"] != "unknown tab id" {
		t.Fatalf("stale observation query = %+v, want unknown tab id", got)
	}
	e.awaitTargetLoopIdle()
	e.assertReleased("after concurrent query and close")

	e.srv.obsSink.RecordNetwork(obsLifeTabID, 999, json.RawMessage(`{"url":"https://late.test/"}`))
	if e.srv.obsStore.HasTarget(obsLifeTabID) {
		t.Fatal("late event rebuilt observation bucket for released target")
	}
}

func setupSuspendedDiscoveryTab(t *testing.T, tid target.ID, method string) (*obsLifeEnv, <-chan enumRPCReply, func()) {
	t.Helper()
	e := newObsLifeEnv(t)
	wOld := e.srv.tabs.OpenWindow()
	e.st.addPage(tid, false)
	enumMustRPC(t, e.srv, protocol.MethodTabList, nil)
	if !e.srv.tabs.Has(tid) {
		t.Fatalf("precondition: discovery target %s not registered", tid)
	}

	atResolved, releaseResolved := e.parkStep(obsStepResolved, tid)
	query := e.asyncRPC(method, map[string]any{"tab": string(tid)})
	e.await(atResolved, "observation query to pass resolveTab")

	e.srv.applyEnumeration(nil, wOld, []*target.Info{
		{TargetID: obsLifeRootID, Type: "page"},
		{TargetID: obsLifeTabID, Type: "page"},
	})
	if !e.srv.tabs.Suspended(tid) || e.srv.tabs.Has(tid) {
		releaseResolved()
		t.Fatalf("precondition: expected %s to be suspended (suspended=%v has=%v)", tid, e.srv.tabs.Suspended(tid), e.srv.tabs.Has(tid))
	}
	return e, query, releaseResolved
}

// A live discovery tab that enters FT28's suspended verification state between
// resolveTab and registerObservation must trigger bounded verification rather
// than failing with unknown tab id across all three observation domains.
func TestSuspendedLiveTabObservationVerifiesInsteadOfUnknownTabID(t *testing.T) {
	t.Run("live_domains", func(t *testing.T) {
		for i, method := range []string{protocol.MethodNetwork, protocol.MethodConsole, protocol.MethodErrors} {
			t.Run(method, func(t *testing.T) {
				tid := target.ID(fmt.Sprintf("EEEE000000000000000000000000000%d", i+1))
				e, query, releaseResolved := setupSuspendedDiscoveryTab(t, tid, method)
				defer releaseResolved()

				releaseResolved()
				got := e.awaitReply(query, "observation query after suspended verification")
				if got.Error != nil {
					t.Fatalf("%s on live suspended tab failed: %+v", method, got.Error)
				}
				if !e.srv.tabs.Has(tid) {
					t.Errorf("live suspended tab %s did not resume after verification", tid)
				}
				if !e.srv.obsStore.HasTarget(tid) {
					t.Errorf("live suspended tab %s did not register observation bucket", tid)
				}
			})
		}
	})

	t.Run("verify_lookup_failure", func(t *testing.T) {
		tid := target.ID("EEEE0000000000000000000000000010")
		e, query, releaseResolved := setupSuspendedDiscoveryTab(t, tid, protocol.MethodNetwork)
		defer releaseResolved()

		e.st.failNext("Target.getTargets")
		releaseResolved()
		got := e.awaitReply(query, "observation query when suspended verification fails")
		if got.Error == nil || got.Error["message"] != "failed to list targets" {
			t.Fatalf("verify lookup failure = %+v, want failed to list targets", got)
		}
		if e.srv.obsStore.HasTarget(tid) {
			t.Errorf("failed verification registered observation bucket for %s", tid)
		}
	})

	t.Run("verify_confirmed_gone", func(t *testing.T) {
		tid := target.ID("EEEE0000000000000000000000000011")
		e, query, releaseResolved := setupSuspendedDiscoveryTab(t, tid, protocol.MethodNetwork)
		defer releaseResolved()

		e.st.dropPage(tid)
		releaseResolved()
		got := e.awaitReply(query, "observation query when suspended tab is gone")
		if got.Error == nil || got.Error["message"] != "unknown tab id" {
			t.Fatalf("verify confirmed gone = %+v, want unknown tab id", got)
		}
		if e.srv.obsStore.HasTarget(tid) {
			t.Errorf("gone target %s registered observation bucket", tid)
		}
		if e.srv.tabs.Known(tid) {
			t.Errorf("gone target %s remained known after verification", tid)
		}
	})
}
