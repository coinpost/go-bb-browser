package browser

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/yiplee/go-bb-browser/internal/state"
)

type targetsExecutor struct {
	calls int
}

func TestDetectForegroundHungTabDoesNotSerializeProbes(t *testing.T) {
	snaps := []state.TabSnapshot{
		{TargetID: "A"},
		{TargetID: "B"},
		{TargetID: "C"},
	}
	var calls atomic.Int64
	start := time.Now()
	got, ok := detectForegroundConcurrent(context.Background(), snaps, 3, 30*time.Millisecond, 200*time.Millisecond,
		func(ctx context.Context, sn state.TabSnapshot) (bool, error) {
			calls.Add(1)
			if sn.TargetID == "A" {
				<-ctx.Done()
				return false, ctx.Err()
			}
			return sn.TargetID == "B", nil
		})
	if ok || got != "" {
		t.Fatalf("got (%q, %v), want incomplete result", got, ok)
	}
	if calls.Load() != 3 {
		t.Fatalf("probe calls = %d, want 3", calls.Load())
	}
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Fatalf("hung tab serialized probes: %s", elapsed)
	}
}

func (e *targetsExecutor) Execute(_ context.Context, method string, _ any, result any) error {
	e.calls++
	if method != "Target.getTargets" {
		return fmt.Errorf("unexpected method %q", method)
	}
	out, ok := result.(*target.GetTargetsReturns)
	if !ok {
		return fmt.Errorf("unexpected result type %T", result)
	}
	out.TargetInfos = []*target.Info{
		{TargetID: "PAGE", Type: "page"},
		{TargetID: "WORKER", Type: "service_worker"},
	}
	return nil
}

func TestPageTargetsUsesBrowserExecutorDirectly(t *testing.T) {
	ex := &targetsExecutor{}
	// A plain context with only a CDP executor proves this does not require a
	// chromedp target context or attach to any page.
	infos, err := pageTargetsWithExecutor(context.Background(), ex)
	if err != nil {
		t.Fatal(err)
	}
	if ex.calls != 1 {
		t.Fatalf("calls = %d, want 1", ex.calls)
	}
	if len(infos) != 1 || infos[0].TargetID != "PAGE" {
		t.Fatalf("targets = %#v", infos)
	}
}

func TestTargetEventQueueOverflowMarksDirty(t *testing.T) {
	s := &Session{targetEvents: make(chan TargetEvent, 1)}
	s.enqueueTargetEvent(TargetEvent{Destroyed: "A"})
	s.enqueueTargetEvent(TargetEvent{Destroyed: "B"})
	if !s.ConsumeTargetDirty() {
		t.Fatal("queue overflow did not mark target state dirty")
	}
	if s.ConsumeTargetDirty() {
		t.Fatal("dirty flag was not consumed")
	}
}

// recordingObserver is a TargetObserver that records what the reader told it
// and what the session looked like at that moment.
type recordingObserver struct {
	s     *Session
	clock uint64
	calls []string
	// dirtyAtDrop is whether the overflow was already visible to the consumer
	// when EventDropped ran.
	dirtyAtDrop []bool
	// queuedAtRead is the queue length when NoteEventRead ran.
	queuedAtRead []int
}

func (o *recordingObserver) NoteEventRead(destroyed target.ID) uint64 {
	o.clock++
	o.calls = append(o.calls, "read:"+string(destroyed))
	o.queuedAtRead = append(o.queuedAtRead, len(o.s.targetEvents))
	return o.clock
}

func (o *recordingObserver) EventDropped() {
	o.calls = append(o.calls, "dropped")
	o.dirtyAtDrop = append(o.dirtyAtDrop, o.s.targetDirty.Load())
}

// Lifecycle events are stamped by the observer at the moment they are handed
// over (connection order), not when they are consumed.
func TestTargetEventsCarryStampFromEnqueueTime(t *testing.T) {
	s := newSession(context.Background(), func() {})
	s.enqueueTargetEvent(TargetEvent{Destroyed: "before-observer"})
	obs := &recordingObserver{s: s, clock: 6}
	s.SetTargetObserver(obs)
	s.enqueueTargetEvent(TargetEvent{Info: &target.Info{TargetID: "A", Type: "page"}})
	obs.clock = 8
	s.enqueueTargetEvent(TargetEvent{Destroyed: "B"})
	obs.clock = 100 // the consumer falls behind; stamps must not move
	for i, want := range []uint64{0, 7, 9} {
		if ev := <-s.TargetEvents(); ev.Stamp != want {
			t.Fatalf("event %d stamp = %d, want %d", i, ev.Stamp, want)
		}
	}
	s.SetTargetObserver(nil)
	s.enqueueTargetEvent(TargetEvent{Destroyed: "C"})
	if ev := <-s.TargetEvents(); ev.Stamp != 0 {
		t.Fatalf("stamp without an observer = %d", ev.Stamp)
	}
}

// The reader's order is a contract (review round 6, P1-3): the observer learns
// about an event — in particular which target a destroyed notification names —
// BEFORE the event is queued, a dropped event is reported to the observer
// next, and only then does the overflow become visible to the consumer. A
// consumer that sees the dirty flag therefore requests its enumeration after
// everything the dropped event meant has been recorded.
func TestObserverIsToldBeforeQueueAndBeforeDirtyIsPublished(t *testing.T) {
	s := &Session{targetEvents: make(chan TargetEvent, 2)}
	obs := &recordingObserver{s: s}
	s.enqueueTargetEvent(TargetEvent{Destroyed: "before-observer"}) // queued, unobserved
	s.SetTargetObserver(obs)
	s.enqueueTargetEvent(TargetEvent{Destroyed: "A"})                                  // fills the queue
	s.enqueueTargetEvent(TargetEvent{Info: &target.Info{TargetID: "B", Type: "page"}}) // dropped
	if !s.ConsumeTargetDirty() {
		t.Fatal("precondition: the queue must be full")
	}
	s.enqueueTargetEvent(TargetEvent{Destroyed: "C"}) // dropped
	if !s.ConsumeTargetDirty() {
		t.Fatal("dropped notification did not mark target state dirty")
	}
	want := []string{"read:A", "read:", "dropped", "read:C", "dropped"}
	if len(obs.calls) != len(want) {
		t.Fatalf("observer calls = %v, want %v", obs.calls, want)
	}
	for i := range want {
		if obs.calls[i] != want[i] {
			t.Fatalf("observer calls = %v, want %v", obs.calls, want)
		}
	}
	if obs.queuedAtRead[0] != 1 {
		t.Fatalf("queue held %d events when A was observed: observed after it was queued", obs.queuedAtRead[0])
	}
	for i, dirty := range obs.dirtyAtDrop {
		if dirty {
			t.Fatalf("drop %d: the overflow was published before the observer was told", i)
		}
	}
	if len(s.TargetEvents()) != 2 {
		t.Fatalf("queue holds %d events, want the 2 that fit", len(s.TargetEvents()))
	}
	s.SetTargetObserver(nil)
	<-s.TargetEvents()
	s.enqueueTargetEvent(TargetEvent{Destroyed: "D"})
	if len(obs.calls) != len(want) {
		t.Fatalf("observer called after it was removed: %v", obs.calls)
	}
}
