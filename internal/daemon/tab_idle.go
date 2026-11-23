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

// Lifecycle evidence — replies to Target.getTargets / Target.createTarget /
// Target.closeTarget and lifecycle events — is applied under s.lifeMu: the
// registry first, then everything that hangs off a registered target
// (observation buffers, idle tracking, the persisted managed set). The mutex is
// never held across a browser round trip. Because a removal and a creation
// commit exclude each other, no managed record can outlive the removal of its
// target and nothing has to be re-checked afterwards.
//
// Managed invariant: the idle tracker only holds registered targets, and the
// persisted managed set holds at least those. The two differ only by members a
// previous daemon process persisted that this one has not recovered yet
// (recoverManagedLocked).

// createRollbackTimeout bounds the cleanup of a tab whose creation could not be
// committed. It is independent of the request deadline, which may be used up.
const createRollbackTimeout = 5 * time.Second

type targetDirtyMarker interface {
	MarkTargetDirty()
}

// markTabManagedLocked starts a daemon-created tab's managed lifecycle: in the
// idle tracker and, synchronously, in the persisted managed set under its
// complete target id. Persisting here rather than through the asynchronous
// tab_new audit line keeps lifecycle records ordered: a delayed audit line can
// never register a tab after it was retired. Caller holds s.lifeMu.
func (s *Server) markTabManagedLocked(tid target.ID) {
	now := time.Now()
	if s.tabIdle != nil {
		s.tabIdle.MarkManagedAt(tid, now)
	}
	if s.store == nil || tid == "" {
		return
	}
	if err := s.store.RegisterManagedTab(string(tid), now.UTC()); err != nil {
		s.logManagedPersistFailure("managed tab not persisted; it will not be restored for idle cleanup after a daemon restart", tid, err)
	}
}

// commitCreatedTab makes the target named by a Target.createTarget reply a
// registered, managed tab and reports whether it did. w was opened before the
// creation was requested: a tab reported gone since then (closed, destroyed,
// dropped by an enumeration) is neither registered nor given a managed
// lifecycle, so the late reply cannot bring its id back.
func (s *Server) commitCreatedTab(w *state.Window, tid target.ID) bool {
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()
	if !s.tabs.CommitCreated(w, tid) {
		return false
	}
	s.markTabManagedLocked(tid)
	return true
}

// rollbackCreatedTab closes a tab this daemon just created but could not
// commit, and describes the outcome for the caller. It is one bounded attempt
// on exactly that target id.
func (s *Server) rollbackCreatedTab(ctx context.Context, conn tabConn, tid target.ID) string {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), createRollbackTimeout)
	defer cancel()
	unlock, ok := s.lockTab(rctx, string(tid))
	if !ok {
		return "not closed (tab busy)"
	}
	defer unlock()
	err := closeTarget(rctx, conn, tid)
	switch {
	case err == nil:
		s.removeTargetState(tid)
		return "closed"
	case browser.ErrorKindOf(err) == browser.ErrorTargetGone:
		s.removeTargetState(tid)
		return "already gone"
	default:
		if s.logger != nil {
			s.logger.Warn("tab_new rollback failed; the tab may still be open and is not managed", "tab", string(tid), "err", err)
		}
		return "not closed (" + err.Error() + ")"
	}
}

func (s *Server) touchTabActivity(tid target.ID) {
	if s.tabIdle != nil {
		s.tabIdle.Touch(tid)
	}
}

// forgetTabManagedLocked ends a managed lifecycle once the target is known to
// be gone (closed successfully, reported gone, destroyed, or absent from a
// successful enumeration). It must not be called for a close that merely
// failed. Caller holds s.lifeMu.
func (s *Server) forgetTabManagedLocked(tid target.ID) {
	if s.tabIdle != nil {
		s.tabIdle.Forget(tid)
	}
	if s.store == nil || tid == "" {
		return
	}
	// No-op for targets the daemon did not create. In-memory state is retired
	// even when the write fails; the failure is logged because the entry can
	// then reappear (harmlessly: recovery needs the exact target id) after a
	// restart.
	if _, err := s.store.RetireManagedTarget(string(tid)); err != nil {
		s.logManagedPersistFailure("managed tab retirement not persisted; the stale entry stays in rpc.jsonl until rotation or the next restart reconciliation", tid, err)
	}
}

// forgetUnregisteredManaged drops the managed lifecycle of a target that is not
// registered (any more).
func (s *Server) forgetUnregisteredManaged(tid target.ID) {
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()
	if !s.tabs.Has(tid) {
		s.forgetTabManagedLocked(tid)
	}
}

func (s *Server) logManagedPersistFailure(msg string, tid target.ID, err error) {
	if s.logger == nil {
		return
	}
	if errors.Is(err, store.ErrClosed) {
		// Shutdown already closed the log; nothing actionable.
		s.logger.Debug(msg, "tab", string(tid), "err", err)
		return
	}
	s.logger.Warn(msg, "tab", string(tid), "err", err)
}

// enumerateTabs is the single request path for a full Target.getTargets
// alignment: window first, browser round trip second, apply last. On error the
// daemon state is left untouched.
func (s *Server) enumerateTabs(ctx context.Context, conn tabConn) ([]state.TabSnapshot, error) {
	w := s.tabs.OpenWindow()
	targets, err := pageTargets(ctx, conn)
	if err != nil {
		s.tabs.Abandon(w)
		return nil, err
	}
	return s.applyEnumeration(conn, w, targets), nil
}

// applyEnumeration applies the reply (infos) to the enumeration w was opened
// for. All target-scoped cleanup (idle tracking, observation buffers, Session
// resources; keyed locks reclaim themselves) is driven only by the removals the
// registry reports, so a tab protected from a stale snapshot keeps every one of
// them. conn is the connection the reply came from.
func (s *Server) applyEnumeration(conn any, w *state.Window, infos []*target.Info) []state.TabSnapshot {
	s.lifeMu.Lock()
	snaps, removed := s.tabs.ApplyEnumeration(w, infos)
	for _, tab := range removed {
		s.clearTargetStateLocked(tab.TargetID)
	}
	retry := s.recoverManagedLocked(infos)
	s.lifeMu.Unlock()

	if sess, ok := conn.(*browser.Session); ok {
		for _, tab := range removed {
			sess.ForgetTarget(tab.TargetID)
		}
	}
	if retry {
		if m, ok := conn.(targetDirtyMarker); ok {
			m.MarkTargetDirty()
		}
	}
	return snaps
}

// clearTargetStateLocked drops the daemon state of a target that was just
// unregistered. Caller holds s.lifeMu. The keyed operation lock needs no
// cleanup: lockTab reclaims its entry when the last reference exits. Session
// resources are released by the caller outside the mutex (ForgetTarget is
// idempotent per id and ids are not reused).
func (s *Server) clearTargetStateLocked(tid target.ID) {
	if s.obsStore != nil {
		s.obsStore.ClearTarget(tid)
	}
	s.forgetTabManagedLocked(tid)
}

// removeTargetState applies the report, learned from a command reply, that tid
// is gone: Target.closeTarget succeeded or answered that there is no such
// target.
func (s *Server) removeTargetState(tid target.ID) {
	s.dropTarget(s.tabConn(), tid, s.tabs.RemoveTarget)
}

// removeEventTarget applies a lifecycle event reporting that tid is gone
// (destroyed, or not a page).
func (s *Server) removeEventTarget(conn any, tid target.ID) {
	s.dropTarget(conn, tid, s.tabs.RemoveEventTarget)
}

func (s *Server) dropTarget(conn any, tid target.ID, remove func(target.ID) bool) {
	if tid == "" {
		return
	}
	s.lifeMu.Lock()
	registered := remove(tid)
	if registered {
		s.clearTargetStateLocked(tid)
	} else {
		// Nothing hangs off an unregistered target, except that a previous
		// daemon may have persisted it as managed: gone is gone for it too.
		s.forgetTabManagedLocked(tid)
	}
	s.lifeMu.Unlock()
	if !registered {
		return
	}
	if sess, ok := conn.(*browser.Session); ok {
		sess.ForgetTarget(tid)
	}
}

// recoverManagedLocked settles the managed tabs a previous daemon process
// persisted, on the first successful enumeration (infos is its reply). Those
// are exactly the persisted members that are not idle-tracked: a tab this
// process creates enters both sets in one step (commitCreatedTab). Each was
// persisted before this process sent any request, so the reply is newer than
// it:
//   - registered now: a live target has exactly the persisted complete target
//     id, and idle tracking resumes from the persisted last activity;
//   - not listed by the reply: gone, retired. This keeps the persisted set
//     bounded by the live managed tabs;
//   - listed but not registered: the registry would not take the reply's word
//     (the target was reported gone, or missing from another enumeration, while
//     the request was in flight). Whatever reported that retired the tab in the
//     same critical section, so this is not expected; the tab stays persisted
//     and the result asks for another enumeration rather than deciding here.
//
// It must only run for a successful enumeration: an empty reply to a failed one
// would retire tabs that are still alive. Caller holds s.lifeMu.
func (s *Server) recoverManagedLocked(infos []*target.Info) (retry bool) {
	if s.managedRecovered || s.tabIdle == nil || s.store == nil {
		return false
	}
	now := time.Now()
	timeout := s.cfg.TabIdleTimeout
	grace := s.cfg.IdleStartupGrace
	minLast := now
	if timeout > grace {
		minLast = now.Add(-(timeout - grace))
	}

	listed := make(map[target.ID]struct{}, len(infos))
	for _, info := range infos {
		// Page targets only, as in the registry: an id listed as anything else
		// is not an open tab.
		if info != nil && info.TargetID != "" && (info.Type == "page" || info.Type == "tab") {
			listed[info.TargetID] = struct{}{}
		}
	}
	tracked := s.tabIdle.Snapshot()
	for _, mt := range s.store.ManagedTabs() {
		tid := target.ID(mt.TargetID)
		if _, ok := tracked[tid]; ok {
			continue
		}
		if s.tabs.Has(tid) {
			effective := mt.LastActivity
			if effective.Before(minLast) {
				effective = minLast
			}
			s.tabIdle.MarkManagedIfAbsentAt(tid, effective)
			continue
		}
		if _, ok := listed[tid]; !ok {
			s.forgetTabManagedLocked(tid)
			continue
		}
		retry = true
	}
	s.managedRecovered = !retry
	return retry
}

func (s *Server) closeTab(ctx context.Context, tab string) error {
	conn := s.tabConn()
	if conn == nil {
		return errTabCloseNoConn
	}
	unlock, ok := s.lockTab(ctx, tab)
	if !ok {
		return errTabLockTimeout
	}
	defer unlock()
	return s.closeTabLocked(ctx, conn, tab)
}

// closeTabLocked closes tab while the caller holds its keyed operation lock.
// The browser's answer is applied either way it says the target is gone:
// Target.closeTarget succeeded, or it replied that there is no such target.
// The error is returned unchanged in the second case; any other failure leaves
// all state alone.
func (s *Server) closeTabLocked(ctx context.Context, conn tabConn, tab string) error {
	tid, ok := s.resolveTab(ctx, conn, tab)
	if !ok {
		return errTabCloseUnknownID
	}
	err := closeTarget(ctx, conn, tid)
	if err == nil || browser.ErrorKindOf(err) == browser.ErrorTargetGone {
		// Clear every target-scoped resource even if no later lifecycle event
		// is delivered.
		s.removeTargetState(tid)
	}
	return err
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
		if !s.tabs.Has(tid) {
			s.forgetUnregisteredManaged(tid)
			continue
		}
		tab := string(tid)
		conn := s.tabConn()
		if conn == nil {
			if s.logger != nil {
				s.logger.Warn("tab idle cleanup failed", "tab", tab, "err", errTabCloseNoConn)
			}
			continue
		}
		unlock, locked := s.lockTab(ctx, tab)
		if !locked {
			if s.logger != nil {
				s.logger.Warn("tab idle cleanup failed", "tab", tab, "err", errTabLockTimeout)
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
		err := s.closeTabLocked(ctx, conn, tab)
		unlock()
		switch {
		case err == nil:
		case browser.ErrorKindOf(err) == browser.ErrorTargetGone:
			// Chrome had already removed the target: cleanup is complete
			// (closeTabLocked cleared its state).
			continue
		case errors.Is(err, errTabCloseUnknownID):
			s.forgetUnregisteredManaged(tid)
			continue
		default:
			if s.logger != nil {
				s.logger.Warn("tab idle cleanup failed", "tab", tab, "err", err)
			}
			continue
		}
		if s.logger != nil {
			s.logger.Info("tab idle closed", "tab", tab, "idle", timeout)
		}
	}
}
