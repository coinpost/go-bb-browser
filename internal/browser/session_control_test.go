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

// Lifecycle events are stamped with the consumer's clock at the moment they are
// handed over (connection order), not when they are consumed.
func TestTargetEventsCarryStampFromEnqueueTime(t *testing.T) {
	s := newSession(context.Background(), func() {})
	s.enqueueTargetEvent(TargetEvent{Destroyed: "before-clock"})
	var clock atomic.Uint64
	clock.Store(7)
	s.SetTargetEventStamp(clock.Load)
	s.enqueueTargetEvent(TargetEvent{Info: &target.Info{TargetID: "A", Type: "page"}})
	clock.Store(9)
	s.enqueueTargetEvent(TargetEvent{Destroyed: "B"})
	clock.Store(100) // the consumer falls behind; stamps must not move
	for i, want := range []uint64{0, 7, 9} {
		if ev := <-s.TargetEvents(); ev.Stamp != want {
			t.Fatalf("event %d stamp = %d, want %d", i, ev.Stamp, want)
		}
	}
	s.SetTargetEventStamp(nil)
	s.enqueueTargetEvent(TargetEvent{Destroyed: "C"})
	if ev := <-s.TargetEvents(); ev.Stamp != 0 {
		t.Fatalf("stamp without a clock = %d", ev.Stamp)
	}
}

// A destroyed notification is reported to the hook when it is read, whether it
// was queued or dropped by a full queue; other lifecycle events are not.
func TestDestroyedHookSeesQueuedAndDroppedNotifications(t *testing.T) {
	type seen struct {
		id     target.ID
		queued bool
	}
	var got []seen
	s := &Session{targetEvents: make(chan TargetEvent, 2)}
	s.enqueueTargetEvent(TargetEvent{Destroyed: "before-hook"})
	s.SetTargetDestroyedHook(func(id target.ID, queued bool) { got = append(got, seen{id, queued}) })
	s.enqueueTargetEvent(TargetEvent{Destroyed: "A"})                                  // fills the queue
	s.enqueueTargetEvent(TargetEvent{Info: &target.Info{TargetID: "B", Type: "page"}}) // dropped, not a destruction
	if !s.ConsumeTargetDirty() {
		t.Fatal("precondition: the queue must be full")
	}
	s.enqueueTargetEvent(TargetEvent{Destroyed: "C"}) // dropped
	if !s.ConsumeTargetDirty() {
		t.Fatal("dropped notification did not mark target state dirty")
	}
	want := []seen{{"A", true}, {"C", false}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("hook saw %+v, want %+v", got, want)
	}
	if len(s.TargetEvents()) != 2 {
		t.Fatalf("queue holds %d events, want the 2 that fit", len(s.TargetEvents()))
	}
	s.SetTargetDestroyedHook(nil)
	<-s.TargetEvents()
	s.enqueueTargetEvent(TargetEvent{Destroyed: "D"})
	if len(got) != len(want) {
		t.Fatalf("hook called after it was removed: %+v", got)
	}
}
