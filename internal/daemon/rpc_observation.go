package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/chromedp/cdproto/target"
	"github.com/yiplee/go-bb-browser/internal/browser"
	"github.com/yiplee/go-bb-browser/internal/state"
	"github.com/yiplee/go-bb-browser/pkg/protocol"
)

func obsSince(p *uint64) uint64 {
	if p == nil {
		return 0
	}
	return *p
}

func (s *Server) handleNetwork(ctx context.Context, w http.ResponseWriter, id json.RawMessage, params json.RawMessage) {
	s.handleObsQuery(ctx, w, id, params, protocol.MethodNetwork, browser.ObservationNetwork, state.ObsRingNetwork)
}

func (s *Server) handleConsole(ctx context.Context, w http.ResponseWriter, id json.RawMessage, params json.RawMessage) {
	s.handleObsQuery(ctx, w, id, params, protocol.MethodConsole, browser.ObservationConsole, state.ObsRingConsole)
}

func (s *Server) handleErrors(ctx context.Context, w http.ResponseWriter, id json.RawMessage, params json.RawMessage) {
	s.handleObsQuery(ctx, w, id, params, protocol.MethodErrors, browser.ObservationErrors, state.ObsRingErrors)
}

func mapObsEvents(in []state.ObsEvent) []protocol.ObsEvent {
	if len(in) == 0 {
		return nil
	}
	out := make([]protocol.ObsEvent, len(in))
	for i, e := range in {
		out[i] = protocol.ObsEvent{Seq: e.Seq, Data: e.Data}
	}
	return out
}

// handleObsQuery answers for one registration of the tab: the one its short id
// names when the request resolves it. Registering the buffers, attaching the
// observer and reading the ring each confirm that this registration is still
// current. If the tab was released meanwhile the request is stale, even when
// the same target id or short id is registered again by then; it then gets the
// unknown-tab error instead of another lifecycle's events under its tab id.
func (s *Server) handleObsQuery(ctx context.Context, w http.ResponseWriter, id json.RawMessage, params json.RawMessage, method string, kind browser.ObservationKind, ring state.ObsRing) {
	var p protocol.ObsQueryParams
	if err := json.Unmarshal(params, &p); err != nil {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "invalid params", nil)
		return
	}
	tab := strings.TrimSpace(p.Tab)
	if tab == "" {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "missing tab", &protocol.ErrData{Error: "missing tab", Method: method})
		return
	}
	conn := s.tabConn()
	if conn == nil {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "browser session not ready", nil)
		return
	}
	unlock, ok := s.lockTab(ctx, tab)
	if !ok {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "request timed out", tabLockTimeoutData())
		return
	}
	defer unlock()
	staleTab := func() {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "unknown tab id", &protocol.ErrData{Error: "unknown tab id", Hint: "invalid or stale tab short id", Method: method})
	}
	reg, ok := s.resolveTabRegistration(ctx, conn, tab)
	if !ok {
		staleTab()
		return
	}
	tid := reg.TargetID
	if meta := auditMetaFrom(ctx); meta != nil {
		// The answer is this registration's whatever its short id names later.
		meta.pinned = string(tid)
	}
	s.obsStep(obsStepResolved, tid)
	bucket, err := s.enableObservation(ctx, conn, reg, kind)
	if err != nil {
		if errors.Is(err, errObservationTargetGone) {
			staleTab()
			return
		}
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "failed to enable observation", &protocol.ErrData{Error: "failed to enable observation", Hint: s.cdpHint(err)})
		return
	}
	since := obsSince(p.Since)
	// The observer's counters are read before the ring: a registration found
	// current by the read below was current for both.
	var observerDropped uint64
	if statsConn, ok := conn.(observationStatsConn); ok {
		observerDropped = statsConn.ObservationStats(tid, kind).Dropped
	}
	stored, cursor, dropped, ok := s.readObservation(reg, bucket, ring, since)
	if !ok {
		staleTab()
		return
	}
	s.obsStep(obsStepRead, tid)
	seq, ok := s.nextSeq(ctx, w, id)
	if !ok {
		return
	}
	s.rpcOK(ctx, w, id, protocol.ObsQueryResult{Tab: tab, Seq: seq, Cursor: cursor, Events: mapObsEvents(stored), Dropped: dropped + observerDropped})
}

// readObservation reads one ring of the buffers this request registered, and
// only while reg is still the tab's registration and those buffers are still
// the target's.
func (s *Server) readObservation(reg state.TabRegistration, bucket state.ObsBucket, ring state.ObsRing, since uint64) (events []state.ObsEvent, cursor, dropped uint64, ok bool) {
	s.obsLifeMu.Lock()
	defer s.obsLifeMu.Unlock()
	if !s.tabs.IsCurrent(reg) {
		return nil, since, 0, false
	}
	if s.obsStore == nil {
		return nil, since, 0, true
	}
	return s.obsStore.QueryBucket(bucket, ring, since)
}

// Steps of an observation query announced to Server.obsStepHook.
const (
	obsStepResolved   = "resolved"   // the short id was resolved, nothing registered yet
	obsStepRegistered = "registered" // the buffers are registered, the attach has not started
	obsStepAttached   = "attached"   // the observer attach returned, outcome not yet committed
	obsStepRead       = "read"       // the ring was read, the reply not yet written
)

func (s *Server) obsStep(step string, tid target.ID) {
	if s.obsStepHook != nil {
		s.obsStepHook(step, tid)
	}
}

var errObservationTargetGone = errors.New("target released while enabling observation")

// enableObservation starts the observation lifecycle of reg's target: it
// registers the buffers explicitly (events for unregistered targets are
// rejected) and then attaches the observer. The caller holds the tab's keyed
// lock, so at most one call runs per short id. It returns the buffers the
// caller may read.
//
// Target removal does not take the keyed tab lock, so it can run concurrently,
// and the target id can be registered again right after, under the same short
// id or another. Everything here is therefore tied to reg, the registration
// the caller resolved, and to the buffers registered for it, never to the
// target id alone:
//
//   - registration first waits for every release of the same target id that is
//     still running, so a reused id never sees the buffers of its previous
//     lifecycle and that release never clears the new ones, and it refuses a
//     registration that is no longer current;
//   - afterwards the lifecycle is confirmed. If reg is still current and still
//     owns its buffers, a failed first attach takes back exactly the buffers
//     this call created. Otherwise the call is stale: it gives up its own
//     buffers if they are still registered and touches no others, since buffers
//     registered for the target id since then belong to a later lifecycle.
//
// A stale call may have installed an observer after the release that made it
// stale had already retired the target's observers. That observer has no owner
// unless a later lifecycle has registered buffers and shares it; when there are
// none it is retired here, as a release of the target id, so a new lifecycle
// waits for it like for any other release.
func (s *Server) enableObservation(ctx context.Context, conn tabConn, reg state.TabRegistration, kind browser.ObservationKind) (state.ObsBucket, error) {
	tid := reg.TargetID
	bucket, created, err := s.registerObservation(ctx, reg)
	if err != nil {
		return state.ObsBucket{}, err
	}
	s.obsStep(obsStepRegistered, tid)
	sess, _ := conn.(*browser.Session)
	if s.obsSink == nil {
		sess = nil
	}
	if sess != nil {
		err = sess.EnsureObservation(ctx, tid, kind, s.obsSink, s.logger)
	}
	s.obsStep(obsStepAttached, tid)

	s.obsLifeMu.Lock()
	owned := s.tabs.IsCurrent(reg) && s.targetReleases[tid] == nil
	if owned && s.obsStore != nil && !s.obsStore.OwnsBucket(bucket) {
		owned = false
	}
	if owned {
		if err != nil && created {
			// The first attach failed: take back the registration this call made.
			// Buffers that existed before belong to a working observation.
			s.obsStore.ReleaseBucket(bucket)
		}
		s.obsLifeMu.Unlock()
		return bucket, err
	}
	orphaned := false
	if s.obsStore != nil {
		s.obsStore.ReleaseBucket(bucket)
		orphaned = !s.obsStore.HasTarget(tid)
	}
	if orphaned {
		s.beginTargetReleaseLocked(tid)
	}
	s.obsLifeMu.Unlock()
	if orphaned {
		if sess != nil {
			sess.ForgetObservation(tid)
		}
		s.endTargetRelease(tid)
	}
	return state.ObsBucket{}, errObservationTargetGone
}

// registerObservation registers the buffers of reg's target once no release of
// that target id is running, and reports whether it created them.
//
// The caller resolved reg earlier and without obsLifeMu, so the tab may have
// been released, completely, since then, and its target id may be registered
// again. The registry is therefore consulted again here: removals and their
// release announcements happen under obsLifeMu, so a registration that is
// current and has no release running is alive, and nothing else may get buffers
// or an observer.
func (s *Server) registerObservation(ctx context.Context, reg state.TabRegistration) (bucket state.ObsBucket, created bool, err error) {
	tid := reg.TargetID
	for {
		s.obsLifeMu.Lock()
		if !s.tabs.IsCurrent(reg) {
			s.obsLifeMu.Unlock()
			return state.ObsBucket{}, false, errObservationTargetGone
		}
		running := s.targetReleases[tid]
		if running == nil {
			if s.obsStore != nil {
				bucket, created = s.obsStore.RegisterBucket(tid)
			}
			s.obsLifeMu.Unlock()
			return bucket, created, nil
		}
		s.obsLifeMu.Unlock()
		select {
		case <-running.done:
		case <-ctx.Done():
			return state.ObsBucket{}, false, fmt.Errorf("waiting for the target's previous release: %w", ctx.Err())
		}
	}
}
