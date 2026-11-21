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
