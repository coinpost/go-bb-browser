package state

import (
	"encoding/json"
	"sync"

	"github.com/chromedp/cdproto/target"
)

// Default observation ring capacities.
const (
	DefaultNetworkCap = 500
	DefaultConsoleCap = 200
	DefaultErrorsCap  = 100
)

// TabObsStore holds per-target network / console / error observation rings (INV-5).
type TabObsStore struct {
	mu sync.Mutex

	netCap  int
	consCap int
	errCap  int

	targets map[target.ID]*targetObsRings
}

type targetObsRings struct {
	net  RingBuffer
	cons RingBuffer
	err  RingBuffer
}

func NewTabObsStore() *TabObsStore {
	return NewTabObsStoreWithCaps(DefaultNetworkCap, DefaultConsoleCap, DefaultErrorsCap)
}

func NewTabObsStoreWithCaps(netCap, consCap, errCap int) *TabObsStore {
	return &TabObsStore{
		netCap:  netCap,
		consCap: consCap,
		errCap:  errCap,
		targets: make(map[target.ID]*targetObsRings),
	}
}

// ObsBucket is a handle on the rings one registration of a target created or
// found. The target id alone does not identify them: after ClearTarget the same
// id can be registered again and then owns different rings. Operations that
// must not cross into such a later lifecycle (reading for a query, taking a
// failed registration back) go through the handle. The zero value owns nothing.
type ObsBucket struct {
	id    target.ID
	rings *targetObsRings
}

// ObsRing names one of a target's three rings.
type ObsRing int

const (
	ObsRingNetwork ObsRing = iota
	ObsRingConsole
	ObsRingErrors
)

// RegisterTarget starts the observation lifecycle of a target. It is the only
// way to create its rings: Push* never creates them, so events arriving before
// registration or after ClearTarget / SyncPresence are rejected instead of
// rebuilding buffers for a released target (INV-6). Registering a target that
// is already registered keeps its buffered events. It reports whether this
// call created the rings, i.e. started a new lifecycle.
func (s *TabObsStore) RegisterTarget(id target.ID) bool {
	_, created := s.RegisterBucket(id)
	return created
}

// RegisterBucket is RegisterTarget returning a handle on the target's rings.
func (s *TabObsStore) RegisterBucket(id target.ID) (bucket ObsBucket, created bool) {
	if id == "" {
		return ObsBucket{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tb := s.targets[id]
	if tb == nil {
		tb = &targetObsRings{
			net:  newRing(s.netCap),
			cons: newRing(s.consCap),
			err:  newRing(s.errCap),
		}
		s.targets[id] = tb
		created = true
	}
	return ObsBucket{id: id, rings: tb}, created
}

// OwnsBucket reports whether b's rings are still the ones registered for its target.
func (s *TabObsStore) OwnsBucket(b ObsBucket) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return b.rings != nil && s.targets[b.id] == b.rings
}

// ReleaseBucket retires b's target like ClearTarget, but only while b's rings
// are still the registered ones. Rings registered for the same target id later
// are left alone; it reports whether anything was removed.
func (s *TabObsStore) ReleaseBucket(b ObsBucket) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b.rings == nil || s.targets[b.id] != b.rings {
		return false
	}
	delete(s.targets, b.id)
	return true
}

// QueryBucket returns the events with seq > since of one ring of b. ok is false,
// with nothing read, when b's rings are no longer the registered ones.
func (s *TabObsStore) QueryBucket(b ObsBucket, ring ObsRing, since uint64) (events []ObsEvent, cursor uint64, dropped uint64, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b.rings == nil || s.targets[b.id] != b.rings {
		return nil, since, 0, false
	}
	var rb *RingBuffer
	switch ring {
	case ObsRingNetwork:
		rb = &b.rings.net
	case ObsRingConsole:
		rb = &b.rings.cons
	case ObsRingErrors:
		rb = &b.rings.err
	default:
		return nil, since, 0, false
	}
	raw, cursor, dropped := rb.QuerySince(since)
	return entriesToObs(raw), cursor, dropped, true
}

// HasTarget reports whether the target currently owns observation rings.
func (s *TabObsStore) HasTarget(id target.ID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.targets[id] != nil
}

// TargetCount reports how many targets currently own observation rings.
func (s *TabObsStore) TargetCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.targets)
}

// SyncPresence removes observation state for targets that are no longer present (INV-6).
func (s *TabObsStore) SyncPresence(infos []*target.Info) {
	present := make(map[target.ID]struct{})
	for _, info := range infos {
		if info == nil {
			continue
		}
		switch info.Type {
		case "page", "tab":
			present[info.TargetID] = struct{}{}
		default:
			continue
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.targets {
		if _, ok := present[id]; !ok {
			delete(s.targets, id)
		}
	}
}

// ClearTarget retires one target: its rings are removed and later Push* calls
// are rejected until the target is registered again (INV-6).
func (s *TabObsStore) ClearTarget(id target.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.targets, id)
}

// ClearNetworkOnly clears only the network ring for a target (keep console/errors).
func (s *TabObsStore) ClearNetworkOnly(id target.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tb := s.targets[id]
	if tb == nil {
		return
	}
	tb.net = newRing(s.netCap)
}

// ClearConsoleOnly clears only the console ring for a target.
func (s *TabObsStore) ClearConsoleOnly(id target.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tb := s.targets[id]
	if tb == nil {
		return
	}
	tb.cons = newRing(s.consCap)
}

// ClearErrorsOnly clears only the errors ring for a target.
func (s *TabObsStore) ClearErrorsOnly(id target.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tb := s.targets[id]
	if tb == nil {
		return
	}
	tb.err = newRing(s.errCap)
}

// PushNetwork records a network observation with the given global seq.
// It reports false, storing nothing, when the target is not registered.
func (s *TabObsStore) PushNetwork(id target.ID, seq uint64, data json.RawMessage) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	tb := s.targets[id]
	if tb == nil {
		return false
	}
	tb.net.Push(seq, data)
	return true
}

// PushConsole records a console observation.
// It reports false, storing nothing, when the target is not registered.
func (s *TabObsStore) PushConsole(id target.ID, seq uint64, data json.RawMessage) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	tb := s.targets[id]
	if tb == nil {
		return false
	}
	tb.cons.Push(seq, data)
	return true
}

// PushError records an error / exception observation.
// It reports false, storing nothing, when the target is not registered.
func (s *TabObsStore) PushError(id target.ID, seq uint64, data json.RawMessage) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	tb := s.targets[id]
	if tb == nil {
		return false
	}
	tb.err.Push(seq, data)
	return true
}

// QueryNetwork returns events with seq > since for the given CDP target id.
func (s *TabObsStore) QueryNetwork(id target.ID, since uint64) (events []ObsEvent, cursor uint64, dropped uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tb := s.targets[id]
	if tb == nil {
		return nil, since, 0
	}
	raw, cursor, dropped := tb.net.QuerySince(since)
	return entriesToObs(raw), cursor, dropped
}

// QueryConsole returns console events with seq > since.
func (s *TabObsStore) QueryConsole(id target.ID, since uint64) (events []ObsEvent, cursor uint64, dropped uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tb := s.targets[id]
	if tb == nil {
		return nil, since, 0
	}
	raw, cursor, dropped := tb.cons.QuerySince(since)
	return entriesToObs(raw), cursor, dropped
}

// QueryErrors returns error events with seq > since.
func (s *TabObsStore) QueryErrors(id target.ID, since uint64) (events []ObsEvent, cursor uint64, dropped uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tb := s.targets[id]
	if tb == nil {
		return nil, since, 0
	}
	raw, cursor, dropped := tb.err.QuerySince(since)
	return entriesToObs(raw), cursor, dropped
}

// ObsEvent is one buffered observation for JSON APIs.
type ObsEvent struct {
	Seq  uint64          `json:"seq"`
	Data json.RawMessage `json:"data"`
}

func entriesToObs(in []obsEntry) []ObsEvent {
	if len(in) == 0 {
		return nil
	}
	out := make([]ObsEvent, 0, len(in))
	for _, e := range in {
		out = append(out, ObsEvent{Seq: e.Seq, Data: e.Data})
	}
	return out
}
