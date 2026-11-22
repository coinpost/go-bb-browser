package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/yiplee/go-bb-browser/internal/browser"
	"github.com/yiplee/go-bb-browser/internal/state"
	"github.com/yiplee/go-bb-browser/internal/store"
)

var (
	errTabCloseNoConn    = errors.New("browser session not ready")
	errTabCloseUnknownID = errors.New("unknown tab id")
	errTabLockTimeout    = fmt.Errorf("tab lock timeout: %w", context.DeadlineExceeded)
)

// markTabManaged starts a daemon-created tab's managed lifecycle: in the idle
// tracker and, synchronously, in the persisted managed set under its full
// target id. Persisting here rather than through the asynchronous tab_new audit
// line keeps lifecycle records ordered: a delayed audit line can never register
// a tab after it was retired.
func (s *Server) markTabManaged(tid target.ID, short, openURL string, silent bool) {
	now := time.Now()
	if s.tabIdle != nil {
		s.tabIdle.MarkManagedAt(tid, now)
	}
	if s.store == nil || tid == "" {
		return
	}
	if err := s.store.RegisterManagedTab(short, string(tid), now.UTC()); err != nil {
		s.logManagedPersistFailure("managed tab not persisted; it will not be restored for idle cleanup after a daemon restart", short, tid, err)
	}
}

func (s *Server) touchTabActivity(tid target.ID) {
	if s.tabIdle != nil {
		s.tabIdle.Touch(tid)
	}
}

// forgetTabManaged ends a managed lifecycle once the target is known to be
// gone (closed successfully, destroyed, or absent from a successful
// enumeration). It must not be called for a close that merely failed.
func (s *Server) forgetTabManaged(tid target.ID) {
	if s.tabIdle != nil {
		s.tabIdle.Forget(tid)
	}
	s.retireManagedTarget(tid)
}

// retireManagedTarget removes tid from the persisted managed set. It is a
// no-op for targets the daemon did not create. In-memory state is retired even
// when the write fails; the failure is logged because the entry can then
// reappear (harmlessly: recovery needs the exact target id) after a restart.
func (s *Server) retireManagedTarget(tid target.ID) {
	if s.store == nil || tid == "" {
		return
	}
	if _, err := s.store.RetireManagedTarget(string(tid)); err != nil {
		s.logManagedPersistFailure("managed tab retirement not persisted; the stale entry stays in rpc.jsonl until rotation or the next restart reconciliation", "", tid, err)
	}
}

func (s *Server) logManagedPersistFailure(msg, short string, tid target.ID, err error) {
	if s.logger == nil {
		return
	}
	if errors.Is(err, store.ErrClosed) {
		// Shutdown already closed the log; nothing actionable.
		s.logger.Debug(msg, "tab", short, "target", string(tid), "err", err)
		return
	}
	s.logger.Warn(msg, "tab", short, "target", string(tid), "err", err)
}

// beginTargetEnumeration must run before the browser is asked for its targets;
// the token keeps the (possibly stale) reply from removing tabs registered
// while the request was in flight.
func (s *Server) beginTargetEnumeration() state.EnumerationToken {
	return s.tabs.BeginEnumeration()
}

// enumerateTabs is the single request path for a full Target.getTargets
// alignment: token first, browser round trip second, apply last. On error the
// daemon state is left untouched.
func (s *Server) enumerateTabs(ctx context.Context, conn tabConn) ([]state.TabSnapshot, error) {
	tok := s.beginTargetEnumeration()
	targets, err := pageTargets(ctx, conn)
	if err != nil {
		return nil, err
	}
	return s.syncTabsFromTargets(tok, targets), nil
}

// syncTabsFromTargets applies a target snapshot requested at tok. All
// target-scoped cleanup (idle tracking, observation buffers, keyed locks,
// Session resources) is driven only by the removals the registry reports, so a
// tab protected from a stale snapshot keeps every one of them. Idle tracking is
// deliberately not intersected with the snapshot: that set is as stale as the
// snapshot and would drop a tab the registry just protected.
func (s *Server) syncTabsFromTargets(tok state.EnumerationToken, infos []*target.Info) []state.TabSnapshot {
	s.obsLifeMu.Lock()
	snaps, removed := s.tabs.ApplyEnumeration(tok, infos)
	for _, tab := range removed {
		s.beginTargetReleaseLocked(tab.TargetID)
	}
	s.obsLifeMu.Unlock()
	for _, tab := range removed {
		s.clearRemovedTarget(tab.TargetID)
		s.endTargetRelease(tab.TargetID)
	}
	return snaps
}

// targetRelease is the completion barrier of every release of one target id
// that is currently running: done is closed when the last of them has finished.
type targetRelease struct {
	running int
	done    chan struct{}
}

// beginTargetReleaseLocked announces a release of tid. The caller holds
// obsLifeMu and makes the announcement in the same critical section as the
// registry removal that causes it, so there is no moment at which the target is
// gone from the registry while its release is not yet visible. From here until
// endTargetRelease the id cannot start a new observation lifecycle: the old
// buffers stay unreadable to it, and the old release cannot clear new ones.
func (s *Server) beginTargetReleaseLocked(tid target.ID) {
	if s.targetReleases == nil {
		s.targetReleases = make(map[target.ID]*targetRelease)
	}
	r := s.targetReleases[tid]
	if r == nil {
		r = &targetRelease{done: make(chan struct{})}
		s.targetReleases[tid] = r
	}
	r.running++
}

func (s *Server) endTargetRelease(tid target.ID) {
	s.obsLifeMu.Lock()
	defer s.obsLifeMu.Unlock()
	r := s.targetReleases[tid]
	if r == nil {
		return
	}
	if r.running--; r.running == 0 {
		delete(s.targetReleases, tid)
		close(r.done)
	}
}

// clearRemovedTarget drops per-target state. The keyed operation lock needs no
// cleanup here: lockTab reclaims its entry when the last reference exits.
func (s *Server) clearRemovedTarget(tid target.ID) {
	s.forgetTabManaged(tid)
	s.retireTargetObservation(tid)
}

// retireTargetObservation stops the producer before dropping its buffers:
// ForgetTarget returns only once the target's observer writer can no longer
// record, and ClearTarget then unregisters the target so any other late event
// is rejected rather than rebuilding the rings (INV-6).
func (s *Server) retireTargetObservation(tid target.ID) {
	if conn, ok := s.tabConn().(targetForgetConn); ok {
		conn.ForgetTarget(tid)
	}
	if s.obsStore != nil {
		s.obsStore.ClearTarget(tid)
	}
}

// removeTargetState releases everything the daemon holds for tid and returns
// only once that is complete. A missing registry entry does not mean so: the
// release may have been started by another path (a Target.targetDestroyed event
// overtaking the reply to Target.closeTarget) and still be running, in which
// case this call waits for it.
func (s *Server) removeTargetState(tid target.ID) {
	s.obsLifeMu.Lock()
	_, removed := s.tabs.RemoveTarget(tid)
	if removed {
		s.beginTargetReleaseLocked(tid)
	}
	running := s.targetReleases[tid]
	s.obsLifeMu.Unlock()
	if removed {
		s.clearRemovedTarget(tid)
		s.endTargetRelease(tid)
	}
	if running != nil {
		<-running.done
	}
}

// managedRetireCandidates is the set of persisted managed targets one target
// enumeration is allowed to retire. It is fixed BEFORE the browser is asked for
// its targets: every member was registered before the request, so the reply
// must list it unless it is really gone. A managed tab registered later (a
// tab_new overlapping the request, or completing before the reply is
// reconciled) is not a member, and its absence from that reply proves nothing.
// The nil value retires nothing.
type managedRetireCandidates map[target.ID]struct{}

// beginManagedReconcile fixes the retirement candidates for the enumeration the
// caller is about to request (see managedRetireCandidates). Like
// beginTargetEnumeration it must run before the browser round trip.
func (s *Server) beginManagedReconcile() managedRetireCandidates {
	if s.tabIdle == nil || s.store == nil {
		return nil
	}
	managed := s.store.ManagedTabs()
	candidates := make(managedRetireCandidates, len(managed))
	for _, mt := range managed {
		candidates[target.ID(mt.TargetID)] = struct{}{}
	}
	return candidates
}

// reconcileIdleFromLog restores idle tracking for persisted managed tabs after
// a successful target enumeration (snaps). A persisted tab is recovered only
// when a live target has exactly the same full CDP target id; a tab that merely
// shares its short id is never adopted. Persisted tabs whose target is absent
// from the enumeration are confirmed gone and retired, which keeps the
// persisted set bounded by the live managed tabs.
//
// Only targets in candidates, fixed by beginManagedReconcile before snaps was
// requested, can be retired here. The managed set is read again now, so it may
// hold tabs registered after the request; snaps is older than those and must
// not be taken as proof that they vanished.
//
// Callers must not invoke it when enumeration failed: an empty snaps would then
// retire tabs that are still alive.
func (s *Server) reconcileIdleFromLog(candidates managedRetireCandidates, snaps []state.TabSnapshot) {
	if s.tabIdle == nil || s.store == nil {
		return
	}
	loaded := s.store.ManagedTabs()
	if len(loaded) == 0 {
		return
	}

	now := time.Now()
	timeout := s.cfg.TabIdleTimeout
	grace := s.cfg.IdleStartupGrace
	minLast := now
	if timeout > grace {
		minLast = now.Add(-(timeout - grace))
	}

	present := make(map[target.ID]struct{}, len(snaps))
	for _, sn := range snaps {
		if sn.TargetID == "" {
			continue
		}
		present[sn.TargetID] = struct{}{}
	}
	for _, mt := range loaded {
		tid := target.ID(mt.TargetID)
		if _, ok := present[tid]; !ok {
			if _, retirable := candidates[tid]; retirable {
				s.forgetTabManaged(tid)
			}
			continue
		}
		effective := mt.LastActivity
		if effective.Before(minLast) {
			effective = minLast
		}
		s.tabIdle.MarkManagedIfAbsentAt(tid, effective)
	}
}

func (s *Server) closeTabByShort(ctx context.Context, tab string) error {
	conn := s.tabConn()
	if conn == nil {
		return errTabCloseNoConn
	}
	unlock, ok := s.lockTab(ctx, tab)
	if !ok {
		return errTabLockTimeout
	}
	defer unlock()
	return s.closeTabByShortLocked(ctx, conn, tab)
}

// closeTabByShortLocked closes tab while the caller holds its keyed operation lock.
func (s *Server) closeTabByShortLocked(ctx context.Context, conn tabConn, tab string) error {
	tid, ok := s.tabs.Lookup(tab)
	if !ok {
		if _, err := s.enumerateTabs(ctx, conn); err == nil {
			tid, ok = s.tabs.Lookup(tab)
		}
	}
	if !ok {
		return errTabCloseUnknownID
	}
	if err := closeTarget(ctx, conn, tid); err != nil {
		return err
	}
	// Target.closeTarget succeeded, so clear every target-scoped resource even if
	// no later lifecycle event is delivered.
	s.removeTargetState(tid)
	return nil
}

func (s *Server) runTabIdleSweeper(ctx context.Context) {
	timeout := s.cfg.TabIdleTimeout
	if timeout <= 0 || s.tabIdle == nil {
		return
	}
	interval := timeout / 6
	if interval < 10*time.Second {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.closeExpiredTabs(ctx)
		}
	}
}

func (s *Server) closeExpiredTabs(ctx context.Context) {
	timeout := s.cfg.TabIdleTimeout
	if timeout <= 0 || s.tabIdle == nil {
		return
	}
	for _, tid := range s.tabIdle.Expired(time.Now(), timeout) {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if err := s.ensureBrowserSession(ctx); err != nil {
			if s.logger != nil {
				s.logger.Warn("tab idle cleanup skipped: browser supervisor not ready", "err", err)
			}
			return
		}
		short, ok := s.tabs.ShortForTarget(tid)
		if !ok {
			s.forgetTabManaged(tid)
			continue
		}
		conn := s.tabConn()
		if conn == nil {
			if s.logger != nil {
				s.logger.Warn("tab idle cleanup failed", "tab", short, "err", errTabCloseNoConn)
			}
			continue
		}
		unlock, locked := s.lockTab(ctx, short)
		if !locked {
			if s.logger != nil {
				s.logger.Warn("tab idle cleanup failed", "tab", short, "err", errTabLockTimeout)
			}
			continue
		}
		// Expired returned a snapshot. Activity may have completed while cleanup
		// waited for this lock, so make the close decision again while serialized
		// with tab operations.
		if !s.tabIdle.IsExpired(tid, time.Now(), timeout) {
			unlock()
			continue
		}
		err := s.closeTabByShortLocked(ctx, conn, short)
		if err != nil && browser.ErrorKindOf(err) == browser.ErrorTargetGone {
			// Idle cleanup is complete when Chrome has already removed the target.
			// Keep explicit tab_close RPC errors unchanged.
			s.removeTargetState(tid)
			s.forgetTabManaged(tid)
			unlock()
			continue
		}
		unlock()
		if err != nil {
			if errors.Is(err, errTabCloseUnknownID) {
				s.forgetTabManaged(tid)
				continue
			}
			if s.logger != nil {
				s.logger.Warn("tab idle cleanup failed", "tab", short, "err", err)
			}
			continue
		}
		if s.logger != nil {
			s.logger.Info("tab idle closed", "tab", short, "idle", timeout)
		}
	}
}
