package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/coinpost/go-bb-browser/internal/browser"
	"github.com/coinpost/go-bb-browser/internal/state"
	"github.com/coinpost/go-bb-browser/pkg/protocol"
)

// forgetConn is a fakeConn that also owns observer writers, like browser.Session.
type forgetConn struct {
	*fakeConn
	mu      sync.Mutex
	forgot  []target.ID
	onForgo func(target.ID)
}

func (c *forgetConn) ForgetTarget(id target.ID) {
	c.mu.Lock()
	c.forgot = append(c.forgot, id)
	hook := c.onForgo
	c.mu.Unlock()
	if hook != nil {
		hook(id)
	}
}

func (c *forgetConn) forgotten() []target.ID {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]target.ID(nil), c.forgot...)
}

func recordAllKinds(sink *obsSink, tid target.ID, seq uint64) {
	data := json.RawMessage(`{"kind":"loadingFailed"}`)
	sink.RecordNetwork(tid, seq, data)
	sink.RecordConsole(tid, seq, data)
	sink.RecordError(tid, seq, data)
}

func assertObservationReleased(t *testing.T, srv *Server, tid target.ID) {
	t.Helper()
	for name, query := range map[string]func(target.ID, uint64) ([]state.ObsEvent, uint64, uint64){
		"network": srv.obsStore.QueryNetwork,
		"console": srv.obsStore.QueryConsole,
		"errors":  srv.obsStore.QueryErrors,
	} {
		if events, _, _ := query(tid, 0); len(events) != 0 {
			t.Errorf("%s: %d event(s) for a released target (INV-6)", name, len(events))
		}
	}
	if n := srv.obsStore.TargetCount(); n != 0 {
		t.Errorf("observation store holds %d target bucket(s), want 0", n)
	}
}

// Every path that releases a target has to retire its observer writer first
// and clear the buffers second; a writer that already dequeued an event, and
// any event arriving afterwards, must not bring the buffers back.
func TestTargetReleaseRetiresWriterThenClearsObservation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		release func(t *testing.T, srv *Server, fc *fakeConn, tid target.ID, tab string)
	}{
		{"tab_close", func(t *testing.T, srv *Server, _ *fakeConn, _ target.ID, tab string) {
			postRPC(t, srv, rpcReq(protocol.MethodTabClose, map[string]any{"tab": tab}, 20))
		}},
		{"idle_reclaim", func(_ *testing.T, srv *Server, _ *fakeConn, tid target.ID, _ string) {
			srv.tabIdle.MarkManagedAt(tid, time.Now().Add(-time.Hour))
			srv.closeExpiredTabs(context.Background())
		}},
		{"idle_reclaim_target_gone", func(_ *testing.T, srv *Server, fc *fakeConn, tid target.ID, _ string) {
			fc.closeErr = errors.New("No target with given id found")
			srv.tabIdle.MarkManagedAt(tid, time.Now().Add(-time.Hour))
			srv.closeExpiredTabs(context.Background())
		}},
		{"closed_by_user", func(t *testing.T, srv *Server, fc *fakeConn, _ target.ID, _ string) {
			fc.infos = nil
			postRPC(t, srv, rpcReq(protocol.MethodTabList, map[string]any{}, 20))
		}},
		{"removed_twice", func(_ *testing.T, srv *Server, _ *fakeConn, tid target.ID, _ string) {
			srv.removeTargetState(tid)
			srv.removeTargetState(tid)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := &fakeConn{infos: []*target.Info{}}
			srv := newIdleTestServer(fc, time.Minute)
			conn := &forgetConn{fakeConn: fc}
			srv.tabHook = conn

			rec := postRPC(t, srv, rpcReq(protocol.MethodTabNew, map[string]any{}, 1))
			var env struct {
				Result protocol.TabNewResult `json:"result"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatal(err)
			}
			tab := env.Result.Tab
			tid, ok := srv.tabs.Lookup(tab)
			if !ok {
				t.Fatalf("tab %q not registered", tab)
			}

			// Nothing is buffered until a query starts the observation lifecycle.
			recordAllKinds(srv.obsSink, tid, 1)
			assertObservationReleased(t, srv, tid)
			for i, method := range []string{protocol.MethodNetwork, protocol.MethodConsole, protocol.MethodErrors} {
				postRPC(t, srv, rpcReq(method, map[string]any{"tab": tab}, 2+i))
			}
			if n := srv.obsStore.TargetCount(); n != 1 {
				t.Fatalf("buckets after observation queries = %d, want 1", n)
			}
			recordAllKinds(srv.obsSink, tid, 2)

			// The writer has dequeued an event and is parked before Record.
			barrier := make(chan struct{})
			flushed := make(chan struct{})
			go func() {
				defer close(flushed)
				<-barrier
				recordAllKinds(srv.obsSink, tid, 3)
			}()
			var bucketsAtRetire []int
			conn.onForgo = func(id target.ID) {
				if id != tid {
					return
				}
				bucketsAtRetire = append(bucketsAtRetire, srv.obsStore.TargetCount())
				select {
				case <-barrier:
				default:
					close(barrier) // retirement waits for the in-flight Record
				}
				<-flushed
			}

			tc.release(t, srv, fc, tid, tab)

			if got := conn.forgotten(); len(got) == 0 || got[0] != tid {
				t.Fatalf("ForgetTarget calls = %v, want %q first", got, tid)
			}
			if bucketsAtRetire[0] != 1 {
				t.Errorf("buffers were cleared before the writer was retired (buckets at retire = %d)", bucketsAtRetire[0])
			}
			assertObservationReleased(t, srv, tid)
			recordAllKinds(srv.obsSink, tid, 4) // stray event after release
			assertObservationReleased(t, srv, tid)
			if _, present := srv.tabs.Lookup(tab); present {
				t.Error("registry entry retained")
			}
		})
	}
}

// A query whose target is not (or no longer) registered must not set up
// anything for it: there is nothing left that would ever release it again.
func TestEnableObservationForReleasedTargetLeavesNoBuffers(t *testing.T) {
	fc := &fakeConn{}
	srv := newIdleTestServer(fc, time.Minute)
	conn := &forgetConn{fakeConn: fc}
	srv.tabHook = conn
	const tid = target.ID("ABCDEF123456")

	// A target that was released, and one that never existed.
	srv.tabs.RegisterPageTarget(tid)
	srv.removeTargetState(tid)
	forgottenBefore := len(conn.forgotten())
	if _, err := srv.enableObservation(context.Background(), conn, tid, browser.ObservationNetwork); !errors.Is(err, errObservationTargetGone) {
		t.Fatalf("released target: err = %v, want errObservationTargetGone", err)
	}
	if _, err := srv.enableObservation(context.Background(), conn, target.ID("NEVEREXISTED"), browser.ObservationNetwork); !errors.Is(err, errObservationTargetGone) {
		t.Fatalf("unregistered target: err = %v, want errObservationTargetGone", err)
	}
	if got := conn.forgotten(); len(got) != forgottenBefore {
		t.Fatalf("ForgetTarget calls = %v, want none for the stale queries: nothing was set up", got)
	}
	recordAllKinds(srv.obsSink, tid, 1)
	assertObservationReleased(t, srv, tid)

	// When the target id is registered again, its own lifecycle works.
	srv.tabs.RegisterPageTarget(tid)
	if _, err := srv.enableObservation(context.Background(), conn, tid, browser.ObservationNetwork); err != nil {
		t.Fatal(err)
	}
	recordAllKinds(srv.obsSink, tid, 2)
	if events, _, _ := srv.obsStore.QueryNetwork(tid, 0); len(events) != 1 || events[0].Seq != 2 {
		t.Fatalf("live target events = %#v", events)
	}
}

// Single-domain clears keep the target observable.
func TestObservationClearKeepsLiveTargetObservable(t *testing.T) {
	fc := &fakeConn{infos: []*target.Info{{TargetID: "ABCDEF123456", Type: "page"}}}
	srv := newIdleTestServer(fc, time.Minute)
	const tid = target.ID("ABCDEF123456")
	const tab = string(tid)
	for i, d := range []struct {
		query, clear string
		events       func(target.ID, uint64) ([]state.ObsEvent, uint64, uint64)
	}{
		{protocol.MethodNetwork, protocol.MethodNetworkClear, srv.obsStore.QueryNetwork},
		{protocol.MethodConsole, protocol.MethodConsoleClear, srv.obsStore.QueryConsole},
		{protocol.MethodErrors, protocol.MethodErrorsClear, srv.obsStore.QueryErrors},
	} {
		postRPC(t, srv, rpcReq(d.query, map[string]any{"tab": tab}, 1))
		recordAllKinds(srv.obsSink, tid, uint64(10*i+1))
		postRPC(t, srv, rpcReq(d.clear, map[string]any{"tab": tab}, 2))
		if events, _, _ := d.events(tid, 0); len(events) != 0 {
			t.Fatalf("%s: %d event(s) after clear", d.clear, len(events))
		}
		recordAllKinds(srv.obsSink, tid, uint64(10*i+2))
		if events, _, _ := d.events(tid, 0); len(events) != 1 {
			t.Fatalf("%s: live target stopped accepting events after clear: %#v", d.clear, events)
		}
	}
	if n := srv.obsStore.TargetCount(); n != 1 {
		t.Fatalf("buckets = %d, want 1", n)
	}
}

// Queries, events and releases of the same tab race each other. Run under -race.
func TestObservationChurnLeavesNoBuffers(t *testing.T) {
	fc := &fakeConn{}
	srv := newIdleTestServer(fc, time.Minute)
	conn := &forgetConn{fakeConn: fc}
	srv.tabHook = conn
	for round := range 200 {
		tid := target.ID(fmt.Sprintf("CHURN%04d", round%8))
		srv.tabs.RegisterPageTarget(tid)
		var wg sync.WaitGroup
		wg.Add(3)
		go func() {
			defer wg.Done()
			_, _ = srv.enableObservation(context.Background(), conn, tid, browser.ObservationConsole)
		}()
		go func() {
			defer wg.Done()
			for seq := range uint64(50) {
				recordAllKinds(srv.obsSink, tid, seq+1)
			}
		}()
		go func() {
			defer wg.Done()
			srv.removeTargetState(tid)
		}()
		wg.Wait()
		recordAllKinds(srv.obsSink, tid, 99)
		if n := srv.obsStore.TargetCount(); n != 0 {
			t.Fatalf("round %d (%s): %d bucket(s) left after release", round, tid, n)
		}
	}
}
