package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

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

// handleObsQuery resolves the target, registers its observation buffers,
// attaches the observer and reads the requested ring. If the target is
// released meanwhile the query gets the unknown-tab error without recreating
// buffers for the released target.
func (s *Server) handleObsQuery(ctx context.Context, w http.ResponseWriter, id json.RawMessage, params json.RawMessage, method string, kind browser.ObservationKind, ring state.ObsRing) {
	var p protocol.ObsQueryParams
	if err := json.Unmarshal(params, &p); err != nil {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "invalid params", nil)
		return
	}
	tab := p.Tab
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
	tid, err := s.resolveTab(ctx, conn, tab)
	if err != nil {
		s.rpcResolveErr(ctx, w, id, method, err)
		return
	}
	s.obsStep(obsStepResolved, tid)
	bucket, err := s.enableObservation(ctx, conn, tid, kind)
	if err != nil {
		if errors.Is(err, errObservationTargetGone) {
			s.rpcResolveErr(ctx, w, id, method, errTabCloseUnknownID)
			return
		}
		s.observeTargetError(conn, tid, err)
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "failed to enable observation", &protocol.ErrData{Error: "failed to enable observation", Hint: s.cdpHint(err)})
		return
	}
	since := obsSince(p.Since)
	// The observer's counters are read before the ring: a target still known by
	// the read below was known for both.
	var observerDropped uint64
	if statsConn, ok := conn.(observationStatsConn); ok {
		observerDropped = statsConn.ObservationStats(tid, kind).Dropped
	}
	stored, cursor, dropped, ok := s.readObservation(tid, bucket, ring, since)
	if !ok {
		s.rpcResolveErr(ctx, w, id, method, errTabCloseUnknownID)
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
// only while tid is still known to the registry and those buffers still belong
// to it.
func (s *Server) readObservation(tid target.ID, bucket state.ObsBucket, ring state.ObsRing, since uint64) (events []state.ObsEvent, cursor, dropped uint64, ok bool) {
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()
	if !s.tabs.Known(tid) {
		return nil, since, 0, false
	}
	if s.obsStore == nil {
		return nil, since, 0, true
	}
	return s.obsStore.QueryBucket(bucket, ring, since)
}

// Steps of an observation query announced to Server.obsStepHook.
const (
	obsStepResolved   = "resolved"   // the target id was resolved, nothing registered yet
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

// enableObservation starts the observation lifecycle of tid: it registers the
// buffers explicitly (events for unregistered targets are rejected) and then
// attaches the observer. The caller holds the tab's keyed lock.
//
// Target removal does not take the keyed tab lock, so it can run concurrently.
// Registration first waits for any release of tid that is still running; after
// EnsureObservation returns, s.lifeMu confirms tid is still known and still owns
// bucket. A failed first attach takes back only the bucket this call created.
// A stale call (tid was released while attaching) gives up its own bucket and
// retires any orphaned observer it installed via ForgetObservation (leaving tab
// pool entries and routes alone).
func (s *Server) enableObservation(ctx context.Context, conn tabConn, tid target.ID, kind browser.ObservationKind) (state.ObsBucket, error) {
	bucket, created, err := s.registerObservation(ctx, tid)
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

	s.lifeMu.Lock()
	owned := s.tabs.Known(tid) && s.targetReleases[tid] == nil
	if owned && s.obsStore != nil && !s.obsStore.OwnsBucket(bucket) {
		owned = false
	}
	if owned {
		if err != nil && created && s.obsStore != nil {
			// The first attach failed: take back the registration this call made.
			// Buffers that existed before belong to a working observation.
			s.obsStore.ReleaseBucket(bucket)
		}
		s.lifeMu.Unlock()
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
	s.lifeMu.Unlock()
	if orphaned {
		if sess != nil {
			sess.ForgetObservation(tid)
		}
		s.endTargetRelease(tid)
	}
	return state.ObsBucket{}, errObservationTargetGone
}

// registerObservation registers the buffers of tid once no release of that
// target id is running, and reports whether it created them.
func (s *Server) registerObservation(ctx context.Context, tid target.ID) (bucket state.ObsBucket, created bool, err error) {
	for {
		s.lifeMu.Lock()
		if !s.tabs.Has(tid) {
			s.lifeMu.Unlock()
			return state.ObsBucket{}, false, errObservationTargetGone
		}
		running := s.targetReleases[tid]
		if running == nil {
			if s.obsStore != nil {
				bucket, created = s.obsStore.RegisterBucket(tid)
			}
			s.lifeMu.Unlock()
			return bucket, created, nil
		}
		s.lifeMu.Unlock()
		select {
		case <-running.done:
		case <-ctx.Done():
			return state.ObsBucket{}, false, fmt.Errorf("waiting for the target's previous release: %w", ctx.Err())
		}
	}
}
