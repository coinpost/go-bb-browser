package browser

import (
	"context"
	"encoding/json"
	"fmt"
	goruntime "runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/yiplee/go-bb-browser/internal/state"
)

// barrierRecorder forwards to a real observation store, optionally holding
// every Record call at a barrier after the writer has dequeued its event.
type barrierRecorder struct {
	obs     *state.TabObsStore
	seq     atomic.Uint64
	calls   atomic.Int64
	entered chan struct{}
	release chan struct{}
}

func (r *barrierRecorder) NextSeq() (uint64, bool) { return r.seq.Add(1), true }

func (r *barrierRecorder) hold() {
	r.calls.Add(1)
	if r.entered != nil {
		select {
		case r.entered <- struct{}{}:
		default:
		}
	}
	if r.release != nil {
		<-r.release
	}
}

func (r *barrierRecorder) RecordNetwork(id target.ID, seq uint64, data json.RawMessage) {
	r.hold()
	r.obs.PushNetwork(id, seq, data)
}

func (r *barrierRecorder) RecordConsole(id target.ID, seq uint64, data json.RawMessage) {
	r.hold()
	r.obs.PushConsole(id, seq, data)
}

func (r *barrierRecorder) RecordError(id target.ID, seq uint64, data json.RawMessage) {
	r.hold()
	r.obs.PushError(id, seq, data)
}

func newRetireTestObserver(s *Session, tid target.ID, rec ObsRecorder) *tabObserver {
	ctx, cancel := context.WithCancel(context.Background())
	obs := &tabObserver{
		tid: tid, ctx: ctx, cleanup: &attachCancellation{cancelChild: cancel}, rec: rec,
		queue: make(chan queuedObservation, observationQueueCapacity), ready: make(chan struct{}),
		stop: make(chan struct{}),
	}
	s.obsMu.Lock()
	if s.observers == nil {
		s.observers = make(map[target.ID]*tabObserver)
	}
	s.observers[tid] = obs
	s.obsMu.Unlock()
	return obs
}

func assertObservationStoreEmpty(t *testing.T, obs *state.TabObsStore, tid target.ID) {
	t.Helper()
	for name, query := range map[string]func(target.ID, uint64) ([]state.ObsEvent, uint64, uint64){
		"network": obs.QueryNetwork,
		"console": obs.QueryConsole,
		"errors":  obs.QueryErrors,
	} {
		if events, _, _ := query(tid, 0); len(events) != 0 {
			t.Errorf("%s: %d event(s) present after target release (INV-6)", name, len(events))
		}
	}
	if n := obs.TargetCount(); n != 0 {
		t.Errorf("observation store still holds %d target bucket(s), want 0", n)
	}
}

func runRetireTestWriter(obs *tabObserver) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		obs.runWriter()
	}()
	return done
}

func awaitRetireTest(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// The writer has already dequeued an event and is parked just before Record
// when the target is released. Releasing must wait for that in-flight Record,
// and nothing recorded by the old writer may survive or rebuild the buffers.
func TestForgetTargetSynchronizesWithInFlightWriter(t *testing.T) {
	const tid = target.ID("TAB-CLOSED")
	obsStore := state.NewTabObsStore()
	rec := &barrierRecorder{obs: obsStore, entered: make(chan struct{}, 1), release: make(chan struct{})}
	s := newSession(context.Background(), func() {})
	obsStore.RegisterTarget(tid)
	obs := newRetireTestObserver(s, tid, rec)
	for i, kind := range []ObservationKind{ObservationNetwork, ObservationConsole, ObservationErrors, ObservationNetwork} {
		obs.queue <- queuedObservation{kind: kind, seq: uint64(i + 1), payload: map[string]any{"n": i}}
	}
	writerDone := runRetireTestWriter(obs)
	awaitRetireTest(t, rec.entered, "the writer to reach Record")

	cleaned := make(chan struct{})
	go func() {
		defer close(cleaned)
		// Same sequence as daemon clearRemovedTarget.
		s.ForgetTarget(tid)
		obsStore.ClearTarget(tid)
	}()
	awaitRetireTest(t, obs.ctx.Done(), "ForgetTarget to cancel the observer")
	select {
	case <-cleaned:
		t.Error("target release returned while the writer still held a dequeued event before Record")
	case <-time.After(100 * time.Millisecond):
	}
	close(rec.release)
	awaitRetireTest(t, cleaned, "target release after the in-flight Record finished")
	awaitRetireTest(t, writerDone, "the retired writer to exit")
	if got := rec.calls.Load(); got != 1 {
		t.Errorf("Record calls = %d, want only the one already in flight; queued events must be discarded", got)
	}
	assertObservationStoreEmpty(t, obsStore, tid)

	// Releasing again is a no-op.
	s.ForgetTarget(tid)
	obs.retire()
	assertObservationStoreEmpty(t, obsStore, tid)
}

// The same target id is registered again after release. Whatever the old
// observer still produces or holds must not leak into the new lifecycle.
func TestRetiredObserverCannotWriteIntoReregisteredTarget(t *testing.T) {
	const tid = target.ID("TAB-REUSED")
	obsStore := state.NewTabObsStore()
	rec := &barrierRecorder{obs: obsStore}
	s := newSession(context.Background(), func() {})
	obsStore.RegisterTarget(tid)
	old := newRetireTestObserver(s, tid, rec)
	old.active.Store(1<<ObservationNetwork | 1<<ObservationConsole | 1<<ObservationErrors)

	s.ForgetTarget(tid)
	obsStore.ClearTarget(tid)
	obsStore.RegisterTarget(tid)

	seqBefore := rec.seq.Load()
	old.listen(&runtime.EventConsoleAPICalled{Type: runtime.APITypeLog})
	if got := rec.seq.Load(); got != seqBefore {
		t.Errorf("retired listener reserved seq %d", got)
	}
	for i, kind := range []ObservationKind{ObservationNetwork, ObservationConsole, ObservationErrors} {
		item := queuedObservation{kind: kind, seq: uint64(100 + i), payload: map[string]any{"stale": true}}
		old.record(item) // a writer that had dequeued before retirement
		old.queue <- item
	}
	awaitRetireTest(t, runRetireTestWriter(old), "the retired writer to exit")
	if got := rec.calls.Load(); got != 0 {
		t.Errorf("retired observer made %d Record call(s)", got)
	}
	if n := obsStore.TargetCount(); n != 1 {
		t.Fatalf("re-registered target buckets = %d, want 1", n)
	}

	fresh := newRetireTestObserver(s, tid, rec)
	fresh.queue <- queuedObservation{kind: ObservationConsole, seq: 500, payload: map[string]any{"fresh": true}}
	done := runRetireTestWriter(fresh)
	deadline := time.Now().Add(5 * time.Second)
	for {
		events, _, _ := obsStore.QueryConsole(tid, 0)
		if len(events) == 1 && events[0].Seq == 500 {
			break
		}
		if len(events) > 1 || time.Now().After(deadline) {
			t.Fatalf("console events in the new lifecycle = %#v, want only seq 500", events)
		}
		time.Sleep(time.Millisecond)
	}
	for name, query := range map[string]func(target.ID, uint64) ([]state.ObsEvent, uint64, uint64){
		"network": obsStore.QueryNetwork, "errors": obsStore.QueryErrors,
	} {
		if events, _, _ := query(tid, 0); len(events) != 0 {
			t.Errorf("%s: stale events %#v in the new lifecycle", name, events)
		}
	}
	s.ForgetTarget(tid)
	obsStore.ClearTarget(tid)
	awaitRetireTest(t, done, "the fresh writer to exit")
	assertObservationStoreEmpty(t, obsStore, tid)
}

// A target can be released while its observer is still attaching: the writer
// has not started yet and must not record once initialization completes.
func TestObserverRetiredDuringInitializationNeverRecords(t *testing.T) {
	const tid = target.ID("TAB-INIT")
	obsStore := state.NewTabObsStore()
	rec := &barrierRecorder{obs: obsStore}
	s := newSession(context.Background(), func() {})
	obsStore.RegisterTarget(tid)
	obs := newRetireTestObserver(s, tid, rec)
	for i := range 8 {
		obs.queue <- queuedObservation{kind: ObservationNetwork, seq: uint64(i + 1), payload: map[string]any{"n": i}}
	}

	s.ForgetTarget(tid)
	obsStore.ClearTarget(tid)
	awaitRetireTest(t, runRetireTestWriter(obs), "the writer started after retirement to exit")
	if got := rec.calls.Load(); got != 0 {
		t.Errorf("Record calls = %d, want 0", got)
	}
	assertObservationStoreEmpty(t, obsStore, tid)
}

// Session.Close must not cancel tab contexts (that closes live tabs), yet its
// writers have to stop and stay silent.
func TestSessionCloseRetiresWritersWithoutCancellingTabs(t *testing.T) {
	const tid = target.ID("TAB-LIVE")
	obsStore := state.NewTabObsStore()
	rec := &barrierRecorder{obs: obsStore}
	s := newSession(context.Background(), func() {})
	obsStore.RegisterTarget(tid)
	obs := newRetireTestObserver(s, tid, rec)
	done := runRetireTestWriter(obs)

	s.Close()
	awaitRetireTest(t, done, "the writer to exit after Session.Close")
	if obs.ctx.Err() != nil {
		t.Error("Session.Close cancelled a still-open tab context")
	}
	obs.record(queuedObservation{kind: ObservationNetwork, seq: 1, payload: map[string]any{}})
	if got := rec.calls.Load(); got != 0 {
		t.Errorf("Record calls after Close = %d, want 0", got)
	}
	obs.cleanup.cancel()
}

// Tabs with a live event stream are opened and released over and over, with
// the release racing the listener and the writer. Run under -race.
func TestObserverStartStopChurnLeavesNoBuckets(t *testing.T) {
	obsStore := state.NewTabObsStore()
	rec := &barrierRecorder{obs: obsStore}
	s := newSession(context.Background(), func() {})
	events := []any{
		&network.EventLoadingFailed{RequestID: "r", ErrorText: "net::ERR_ABORTED"},
		&runtime.EventConsoleAPICalled{Type: runtime.APITypeLog},
		&runtime.EventExceptionThrown{ExceptionDetails: &runtime.ExceptionDetails{Text: "boom"}},
	}
	const rounds, tabs = 40, 4
	var recorded atomic.Int64
	for round := range rounds {
		var wg sync.WaitGroup
		for tab := range tabs {
			// Ids repeat across rounds, so every round is also a re-registration.
			tid := target.ID(fmt.Sprintf("TAB-%d", tab))
			obsStore.RegisterTarget(tid)
			obs := newRetireTestObserver(s, tid, rec)
			obs.active.Store(1<<ObservationNetwork | 1<<ObservationConsole | 1<<ObservationErrors)
			writerDone := runRetireTestWriter(obs)
			stopFeed := make(chan struct{})
			fed := make(chan struct{})
			wg.Add(2)
			go func() {
				defer wg.Done()
				defer close(fed)
				for i := 0; ; i++ {
					select {
					case <-stopFeed:
						return
					default:
						obs.listen(events[i%len(events)])
					}
				}
			}()
			go func() {
				defer wg.Done()
				for obs.lastSeen[ObservationNetwork].Load() == 0 {
					goruntime.Gosched()
				}
				if round%2 == 0 {
					for calls := rec.calls.Load(); rec.calls.Load() == calls; {
						goruntime.Gosched()
					}
				}
				s.ForgetTarget(tid)
				obsStore.ClearTarget(tid)
				if round%3 == 0 {
					s.ForgetTarget(tid)
					obsStore.ClearTarget(tid)
				}
				// The listener keeps firing after release, like late CDP events.
				for range 64 {
					obs.listen(events[0])
				}
				close(stopFeed)
				<-fed
				<-writerDone
			}()
		}
		wg.Wait()
		recorded.Store(rec.calls.Load())
		if n := obsStore.TargetCount(); n != 0 {
			t.Fatalf("round %d: %d target bucket(s) left after every tab was released", round, n)
		}
	}
	if recorded.Load() == 0 {
		t.Fatal("no event was ever recorded; the churn did not exercise the writer")
	}
	s.obsMu.Lock()
	left := len(s.observers)
	s.obsMu.Unlock()
	if left != 0 {
		t.Fatalf("%d observer(s) left registered", left)
	}
}
