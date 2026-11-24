package state

import (
	"encoding/json"
	"testing"

	"github.com/chromedp/cdproto/target"
)

func pushAllKinds(s *TabObsStore, id target.ID, seq uint64) {
	data := json.RawMessage(`{"late":true}`)
	s.PushNetwork(id, seq, data)
	s.PushConsole(id, seq, data)
	s.PushError(id, seq, data)
}

func assertNoObsBucket(t *testing.T, s *TabObsStore, id target.ID) {
	t.Helper()
	s.mu.Lock()
	_, bucket := s.targets[id]
	total := len(s.targets)
	s.mu.Unlock()
	if bucket || total != 0 {
		t.Errorf("bucket for %q present = %v, targets map size = %d, want none", id, bucket, total)
	}
	for name, query := range map[string]func(target.ID, uint64) ([]ObsEvent, uint64, uint64){
		"network": s.QueryNetwork, "console": s.QueryConsole, "errors": s.QueryErrors,
	} {
		if events, _, _ := query(id, 0); len(events) != 0 {
			t.Errorf("%s: %d event(s), want 0", name, len(events))
		}
	}
}

func TestObsStoreRejectsPushWithoutRegistration(t *testing.T) {
	s := NewTabObsStore()
	pushAllKinds(s, "NEVER-REGISTERED", 1)
	assertNoObsBucket(t, s, "NEVER-REGISTERED")
}

func TestObsStoreRejectsLatePushAfterRelease(t *testing.T) {
	for name, release := range map[string]func(*TabObsStore, target.ID){
		"clear_target":  func(s *TabObsStore, id target.ID) { s.ClearTarget(id) },
		"sync_presence": func(s *TabObsStore, _ target.ID) { s.SyncPresence(nil) },
		"clear_twice": func(s *TabObsStore, id target.ID) {
			s.ClearTarget(id)
			s.ClearTarget(id)
		},
	} {
		t.Run(name, func(t *testing.T) {
			const id = target.ID("TAB-A")
			s := NewTabObsStore()
			s.RegisterTarget(id)
			pushAllKinds(s, id, 1)
			release(s, id)
			pushAllKinds(s, id, 2)
			for _, clearOne := range []func(target.ID){s.ClearNetworkOnly, s.ClearConsoleOnly, s.ClearErrorsOnly} {
				clearOne(id) // must not resurrect the bucket either
			}
			assertNoObsBucket(t, s, id)
			if s.PushNetwork(id, 3, nil) || s.PushConsole(id, 3, nil) || s.PushError(id, 3, nil) {
				t.Error("push for a released target reported success")
			}
		})
	}
}

func TestObsStoreRegistrationLifecycle(t *testing.T) {
	const id = target.ID("TAB-A")
	s := NewTabObsStore()
	s.RegisterTarget("")
	if n := s.TargetCount(); n != 0 {
		t.Fatalf("empty target id registered: %d bucket(s)", n)
	}
	s.RegisterTarget(id)
	pushAllKinds(s, id, 1)
	s.RegisterTarget(id) // repeated registration keeps buffered events
	if events, cursor, _ := s.QueryNetwork(id, 0); len(events) != 1 || cursor != 1 {
		t.Fatalf("events after re-register = %#v cursor %d", events, cursor)
	}

	// A new lifecycle of the same id starts empty.
	s.ClearTarget(id)
	s.RegisterTarget(id)
	if n := s.TargetCount(); n != 1 {
		t.Fatalf("buckets = %d, want 1", n)
	}
	for name, query := range map[string]func(target.ID, uint64) ([]ObsEvent, uint64, uint64){
		"network": s.QueryNetwork, "console": s.QueryConsole, "errors": s.QueryErrors,
	} {
		if events, _, _ := query(id, 0); len(events) != 0 {
			t.Errorf("%s: %d event(s) carried over into the new lifecycle", name, len(events))
		}
	}
	// Other targets are isolated from the registration (INV-5).
	pushAllKinds(s, "TAB-B", 5)
	if n := s.TargetCount(); n != 1 {
		t.Fatalf("push to an unregistered sibling created a bucket: %d", n)
	}
}

func TestObsStoreSingleDomainClearKeepsLiveTarget(t *testing.T) {
	const id = target.ID("TAB-A")
	type domain struct {
		clear func(target.ID)
		push  func(target.ID, uint64, json.RawMessage) bool
		query func(target.ID, uint64) ([]ObsEvent, uint64, uint64)
	}
	s := NewTabObsStore()
	domains := map[string]domain{
		"network": {s.ClearNetworkOnly, s.PushNetwork, s.QueryNetwork},
		"console": {s.ClearConsoleOnly, s.PushConsole, s.QueryConsole},
		"errors":  {s.ClearErrorsOnly, s.PushError, s.QueryErrors},
	}
	s.RegisterTarget(id)
	for name, cleared := range domains {
		pushAllKinds(s, id, 1)
		cleared.clear(id)
		if events, _, _ := cleared.query(id, 0); len(events) != 0 {
			t.Fatalf("%s: %d event(s) after single-domain clear", name, len(events))
		}
		for other, d := range domains {
			if other == name {
				continue
			}
			if events, _, _ := d.query(id, 0); len(events) == 0 {
				t.Fatalf("clearing %s also emptied %s", name, other)
			}
		}
		if !cleared.push(id, 2, json.RawMessage(`{"after":"clear"}`)) {
			t.Fatalf("%s: live target rejected a push after single-domain clear", name)
		}
		if events, _, _ := cleared.query(id, 0); len(events) != 1 || events[0].Seq != 2 {
			t.Fatalf("%s: events after clear = %#v", name, events)
		}
	}
	if n := s.TargetCount(); n != 1 {
		t.Fatalf("buckets = %d, want 1", n)
	}
}

// RegisterTarget tells a new lifecycle from an existing one, so a caller can
// take back only the registration it made.
func TestRegisterTargetReportsNewLifecycle(t *testing.T) {
	s := NewTabObsStore()
	const id = target.ID("TAB")
	if s.HasTarget(id) || s.RegisterTarget("") || s.TargetCount() != 0 {
		t.Fatal("empty store reports a registered target")
	}
	if !s.RegisterTarget(id) || !s.HasTarget(id) {
		t.Fatal("first registration did not create the target")
	}
	s.PushNetwork(id, 1, json.RawMessage(`{}`))
	if s.RegisterTarget(id) {
		t.Error("registering a live target reported a new lifecycle")
	}
	if events, _, _ := s.QueryNetwork(id, 0); len(events) != 1 {
		t.Errorf("re-registration dropped buffered events: %d left", len(events))
	}
	s.ClearTarget(id)
	if s.HasTarget(id) || !s.RegisterTarget(id) {
		t.Error("cleared target did not start a new lifecycle")
	}
	if events, _, _ := s.QueryNetwork(id, 0); len(events) != 0 {
		t.Errorf("new lifecycle sees %d old event(s)", len(events))
	}
}

// A bucket handle stands for the rings one registration got. After the target
// is cleared and registered again the old handle owns nothing: it cannot read
// the new rings or release them.
func TestObsBucketHandleIsBoundToItsLifecycle(t *testing.T) {
	s := NewTabObsStore()
	const id = target.ID("T1")
	old, created := s.RegisterBucket(id)
	if !created || !s.OwnsBucket(old) {
		t.Fatalf("first registration: created = %v, owns = %v", created, s.OwnsBucket(old))
	}
	again, created := s.RegisterBucket(id)
	if created || !s.OwnsBucket(again) || !s.OwnsBucket(old) {
		t.Fatal("registering a registered target must hand out the same rings")
	}
	s.PushNetwork(id, 1, json.RawMessage(`{"n":1}`))
	s.ClearNetworkOnly(id)
	if !s.OwnsBucket(old) {
		t.Fatal("a single-domain clear ended the lifecycle")
	}
	s.PushConsole(id, 2, json.RawMessage(`{"n":2}`))
	if ev, cursor, _, ok := s.QueryBucket(old, ObsRingConsole, 0); !ok || len(ev) != 1 || cursor != 2 {
		t.Fatalf("QueryBucket(console) = %v, %d, %v", ev, cursor, ok)
	}
	if ev, _, _, ok := s.QueryBucket(old, ObsRingNetwork, 0); !ok || len(ev) != 0 {
		t.Fatalf("QueryBucket(network) after its clear = %v, %v", ev, ok)
	}

	s.ClearTarget(id)
	next, created := s.RegisterBucket(id)
	if !created {
		t.Fatal("registration after ClearTarget did not create new rings")
	}
	s.PushNetwork(id, 3, json.RawMessage(`{"n":3}`))
	s.PushError(id, 4, json.RawMessage(`{"n":4}`))
	if s.OwnsBucket(old) {
		t.Fatal("old handle owns the rings of the new lifecycle")
	}
	for _, ring := range []ObsRing{ObsRingNetwork, ObsRingConsole, ObsRingErrors} {
		if ev, cursor, _, ok := s.QueryBucket(old, ring, 7); ok || len(ev) != 0 || cursor != 7 {
			t.Fatalf("old handle read ring %d of the new lifecycle: %v, %d, %v", ring, ev, cursor, ok)
		}
	}
	if s.ReleaseBucket(old) {
		t.Fatal("old handle released the rings of the new lifecycle")
	}
	if ev, _, _, ok := s.QueryBucket(next, ObsRingNetwork, 0); !ok || len(ev) != 1 || ev[0].Seq != 3 {
		t.Fatalf("new lifecycle's events = %v, %v", ev, ok)
	}
	if ev, _, _, ok := s.QueryBucket(next, ObsRingErrors, 0); !ok || len(ev) != 1 || ev[0].Seq != 4 {
		t.Fatalf("new lifecycle's errors = %v, %v", ev, ok)
	}
	if !s.ReleaseBucket(next) || s.TargetCount() != 0 || s.ReleaseBucket(next) {
		t.Fatal("ReleaseBucket by the owner must retire the target exactly once")
	}
	if s.PushNetwork(id, 5, json.RawMessage(`{}`)) {
		t.Fatal("event accepted after the owner released the bucket")
	}
	if s.OwnsBucket(ObsBucket{}) || s.ReleaseBucket(ObsBucket{}) {
		t.Fatal("zero handle owns something")
	}
}
