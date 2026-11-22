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
	s.handleObsQuery(ctx, w, id, params, protocol.MethodNetwork, browser.ObservationNetwork, func(tid target.ID, since uint64) ([]protocol.ObsEvent, uint64, uint64) {
		ev, cur, drop := s.obsStore.QueryNetwork(tid, since)
		return mapObsEvents(ev), cur, drop
	})
}

func (s *Server) handleConsole(ctx context.Context, w http.ResponseWriter, id json.RawMessage, params json.RawMessage) {
	s.handleObsQuery(ctx, w, id, params, protocol.MethodConsole, browser.ObservationConsole, func(tid target.ID, since uint64) ([]protocol.ObsEvent, uint64, uint64) {
		ev, cur, drop := s.obsStore.QueryConsole(tid, since)
		return mapObsEvents(ev), cur, drop
	})
}

func (s *Server) handleErrors(ctx context.Context, w http.ResponseWriter, id json.RawMessage, params json.RawMessage) {
	s.handleObsQuery(ctx, w, id, params, protocol.MethodErrors, browser.ObservationErrors, func(tid target.ID, since uint64) ([]protocol.ObsEvent, uint64, uint64) {
		ev, cur, drop := s.obsStore.QueryErrors(tid, since)
		return mapObsEvents(ev), cur, drop
	})
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

func (s *Server) handleObsQuery(ctx context.Context, w http.ResponseWriter, id json.RawMessage, params json.RawMessage, method string, kind browser.ObservationKind, q func(tid target.ID, since uint64) ([]protocol.ObsEvent, uint64, uint64)) {
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
	tid, ok := s.resolveTab(ctx, conn, tab)
	if !ok {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "unknown tab id", &protocol.ErrData{Error: "unknown tab id", Hint: "invalid or stale tab short id", Method: method})
		return
	}
	if err := s.enableObservation(ctx, conn, tid, kind); err != nil {
		if errors.Is(err, errObservationTargetGone) {
			s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "unknown tab id", &protocol.ErrData{Error: "unknown tab id", Hint: "invalid or stale tab short id", Method: method})
			return
		}
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "failed to enable observation", &protocol.ErrData{Error: "failed to enable observation", Hint: s.cdpHint(err)})
		return
	}
	since := obsSince(p.Since)
	events, cursor, dropped := q(tid, since)
	if statsConn, ok := conn.(observationStatsConn); ok {
		stats := statsConn.ObservationStats(tid, kind)
		dropped += stats.Dropped
	}
	seq, ok := s.nextSeq(ctx, w, id)
	if !ok {
		return
	}
	s.rpcOK(ctx, w, id, protocol.ObsQueryResult{Tab: tab, Seq: seq, Cursor: cursor, Events: events, Dropped: dropped})
}

var errObservationTargetGone = errors.New("target released while enabling observation")

// enableObservation starts the target's observation lifecycle: it registers the
// buffers explicitly (events for unregistered targets are rejected) and then
// attaches the observer. The caller holds the tab's keyed lock, so at most one
// call runs per target.
//
// Target removal does not take the keyed tab lock, so it can run concurrently.
// Registration first waits for every release of the same target id that is
// still running, so a reused id never sees the buffers of its previous
// lifecycle and that release never clears the new ones, and it refuses a target
// that is no longer registered, however long ago it was released. Afterwards the
// lifecycle is confirmed: if the target left the registry, or a release took
// the buffers, while the observer was attaching, whatever this call set up is
// retired again, leaving no buffers or observer behind for a released target.
func (s *Server) enableObservation(ctx context.Context, conn tabConn, tid target.ID, kind browser.ObservationKind) error {
	created, err := s.registerObservation(ctx, tid)
	if err != nil {
		return err
	}
	if sess, ok := conn.(*browser.Session); ok && s.obsSink != nil {
		err = sess.EnsureObservation(ctx, tid, kind, s.obsSink, s.logger)
	}
	s.obsLifeMu.Lock()
	_, live := s.tabs.ShortForTarget(tid)
	if live && s.targetReleases[tid] != nil {
		live = false
	}
	if live && s.obsStore != nil && !s.obsStore.HasTarget(tid) {
		live = false
	}
	switch {
	case !live:
		s.beginTargetReleaseLocked(tid)
	case err != nil && created:
		// The first attach failed: take back the registration this call made.
		// Buffers that existed before belong to a working observation.
		s.obsStore.ClearTarget(tid)
	}
	s.obsLifeMu.Unlock()
	if !live {
		s.retireTargetObservation(tid)
		s.endTargetRelease(tid)
		return errObservationTargetGone
	}
	return err
}

// registerObservation registers tid's buffers once no release of that id is
// running, and reports whether it created them.
//
// The caller resolved tid earlier and without obsLifeMu, so the target may have
// been released, completely, since then. The registry is therefore consulted
// again here: removals and their release announcements happen under obsLifeMu,
// so a target that is registered and has no release running is alive, and
// nothing else may get buffers or an observer.
func (s *Server) registerObservation(ctx context.Context, tid target.ID) (created bool, err error) {
	for {
		s.obsLifeMu.Lock()
		running := s.targetReleases[tid]
		if running == nil {
			if _, live := s.tabs.ShortForTarget(tid); !live {
				s.obsLifeMu.Unlock()
				return false, errObservationTargetGone
			}
			if s.obsStore != nil {
				created = s.obsStore.RegisterTarget(tid)
			}
			s.obsLifeMu.Unlock()
			return created, nil
		}
		s.obsLifeMu.Unlock()
		select {
		case <-running.done:
		case <-ctx.Done():
			return false, fmt.Errorf("waiting for the target's previous release: %w", ctx.Err())
		}
	}
}
