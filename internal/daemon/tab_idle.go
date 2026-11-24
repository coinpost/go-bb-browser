package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/coinpost/go-bb-browser/internal/browser"
	"github.com/coinpost/go-bb-browser/internal/state"
	"github.com/coinpost/go-bb-browser/internal/store"
)

var (
	errTabCloseNoConn    = errors.New("browser session not ready")
	errTabCloseUnknownID = errors.New("unknown tab id")
	// errTabLookup wraps the failure of the enumeration a request needed to
	// resolve its tab id: nothing is known about the id, in particular not
	// that it is invalid.
	errTabLookup      = errors.New("list targets to resolve tab")
	errTabLockTimeout = fmt.Errorf("tab lock timeout: %w", context.DeadlineExceeded)
)

// Lifecycle evidence — replies to Target.getTargets / Target.createTarget /
// Target.closeTarget and lifecycle events — is applied under s.lifeMu: the
// registry first, then everything that hangs off a registered target
// (observation buffers, idle tracking, the persisted managed set). The mutex is
// never held across a browser round trip. Because a removal and a creation
// commit exclude each other, no managed record can outlive the removal of its
// target and nothing has to be re-checked afterwards.
//
// Managed invariant: the idle tracker only holds targets the registry knows
// (resolving or, for the moment, not), and the
// persisted managed set holds at least those. The two differ only by members a
// previous daemon process persisted that this one has not recovered yet
// (recoverManagedLocked).

// createRollbackTimeout bounds the cleanup of a tab whose creation could not be
// committed. It is independent of the request deadline, which may be used up.
const createRollbackTimeout = 5 * time.Second

type targetDirtyMarker interface {
	MarkTargetDirty()
}

// requestEnumeration asks the target sync loop of conn for a fresh
// Target.getTargets. It is how every piece of lifecycle evidence that could
// not be ordered gets settled: the new enumeration is requested after it.
func requestEnumeration(conn any) {
	if m, ok := conn.(targetDirtyMarker); ok {
		m.MarkTargetDirty()
	}
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
// registered, managed tab and reports whether it is one when the call returns.
// w was opened before the creation was requested: a tab reported gone since
// then (closed, destroyed, dropped by an enumeration) is neither registered nor
// given a managed lifecycle, so the late reply cannot bring its id back.
//
// When absence evidence exists that could not be ordered against the creation
// (an enumeration applied meanwhile did not list the tab), the tab is committed
// all the same — the snapshot may simply predate it — and the browser is asked
// once more before tab_new answers: that enumeration is requested after the
// commit, so a tab it does not list is gone, and it removes the tab together
// with its managed record. If the question cannot be asked, the answer is left
// to the sync loop of conn and the commit stands.
//
// The creation is reported as a success only if nothing reported the tab gone
// between the commit and the end of that round trip, whether it succeeded or
// failed: the window of the verification is opened in the critical section of
// the commit and records every such report (another caller's tab_close, a
// destroyed notification, an enumeration). That the id resolves afterwards is
// not enough — an enumeration requested after a confirmed close registers a
// target Chrome still lists as a discovery, which is not the managed tab this
// call created: its managed lifecycle ended with the close. Nothing is
// concluded from the failed enumeration itself.
func (s *Server) commitCreatedTab(ctx context.Context, conn tabConn, w *state.Window, tid target.ID) bool {
	s.lifeMu.Lock()
	ok, verify := s.tabs.CommitCreated(w, tid)
	var check *state.Window
	if ok {
		s.markTabManagedLocked(tid)
		if verify {
			check = s.tabs.OpenWindow()
		}
	}
	s.lifeMu.Unlock()
	if check == nil {
		return ok
	}
	if infos, err := pageTargets(ctx, conn); err != nil {
		s.tabs.Abandon(check)
		requestEnumeration(conn)
	} else {
		s.applyEnumeration(conn, check, infos)
	}
	return !s.tabs.ReportedGone(check, tid) && s.tabs.Has(tid)
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

// forgetUnregisteredManaged drops the managed lifecycle of a target the
// registry does not know (any more). A target that is known but does not
// resolve at the moment keeps it.
func (s *Server) forgetUnregisteredManaged(tid target.ID) {
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()
	if !s.tabs.Known(tid) {
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

// maxSuspendedVerifyAttempts bounds synchronous re-enumeration when an
// authoritative listing or lookup encounters a target still suspended by an
// overlapping snapshot conflict.
const maxSuspendedVerifyAttempts = 2

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

// enumerateAuthoritativeTabs enumerates targets for calls (tab_list, tab_focus)
// whose reply clients treat as authoritative for which tabs currently exist.
// If the browser listed a target that stayed suspended due to an overlapping
// snapshot conflict, it verifies with a fresh enumeration instead of omitting a
// potentially live tab from a successful response.
func (s *Server) enumerateAuthoritativeTabs(ctx context.Context, conn tabConn) ([]state.TabSnapshot, error) {
	for attempt := 0; ; attempt++ {
		w := s.tabs.OpenWindow()
		targets, err := pageTargets(ctx, conn)
		if err != nil {
			s.tabs.Abandon(w)
			return nil, err
		}
		snaps, omittedSuspended := s.applyEnumerationChecked(conn, w, targets)
		if !omittedSuspended {
			return snaps, nil
		}
		if attempt >= maxSuspendedVerifyAttempts {
			return nil, errors.New("target list has members pending verification")
		}
	}
}

// applyEnumeration applies the reply (infos) to the enumeration w was opened
// for. All target-scoped cleanup (idle tracking, observation buffers, Session
// resources; keyed locks reclaim themselves) is driven only by the removals the
// registry reports, and it reports only targets the browser is known to have
// dropped. A tab the reply merely does not list while its existence cannot be
// ordered against the snapshot keeps every one of them — in particular its
// browser attachment, whose release would close a page that is still open —
// and a fresh enumeration is requested instead. conn is the connection the
// reply came from.
func (s *Server) applyEnumeration(conn any, w *state.Window, infos []*target.Info) []state.TabSnapshot {
	snaps, _ := s.applyEnumerationChecked(conn, w, infos)
	return snaps
}

// targetRelease is the completion barrier of every release of one target id
// that is currently running: done is closed when the last of them has finished.
type targetRelease struct {
	running int
	done    chan struct{}
}

// beginTargetReleaseLocked announces a release of tid. The caller holds
// lifeMu and makes the announcement in the same critical section as the
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
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()
	r := s.targetReleases[tid]
	if r == nil {
		return
	}
	if r.running--; r.running == 0 {
		delete(s.targetReleases, tid)
		close(r.done)
	}
}

// retireTargetObservation stops the producer before dropping its buffers:
// ForgetTarget returns only once the target's observer writer can no longer
// record, and ClearTarget then unregisters the target so any other late event
// is rejected rather than rebuilding the rings (INV-6).
func (s *Server) retireTargetObservation(conn any, tid target.ID) {
	if conn == nil {
		conn = s.tabConn()
	}
	if fc, ok := conn.(targetForgetConn); ok {
		fc.ForgetTarget(tid)
	}
	if s.obsStore != nil {
		s.obsStore.ClearTarget(tid)
	}
}

func (s *Server) applyEnumerationChecked(conn any, w *state.Window, infos []*target.Info) ([]state.TabSnapshot, bool) {
	s.lifeMu.Lock()
	snaps, removed, verify, omittedSuspended := s.tabs.ApplyEnumerationChecked(w, infos)
	for _, tab := range removed {
		s.forgetTabManagedLocked(tab.TargetID)
		s.beginTargetReleaseLocked(tab.TargetID)
	}
	retry := s.recoverManagedLocked(infos)
	s.lifeMu.Unlock()

	for _, tab := range removed {
		s.retireTargetObservation(conn, tab.TargetID)
		s.endTargetRelease(tab.TargetID)
	}
	if verify || retry {
		requestEnumeration(conn)
	}
	return snaps, omittedSuspended
}

// removeTargetState applies the report, learned from a command reply, that tid
// is gone: Target.closeTarget succeeded or answered that there is no such
// target.
func (s *Server) removeTargetState(tid target.ID) {
	s.dropTarget(s.tabConn(), tid, s.tabs.RemoveTarget)
}

// observeTargetError feeds explicit target-level absence reported by a
// target-addressed CDP failure (such as Target.attachToTarget answering that no
// target exists with that id) back into the lifecycle state so in-flight
// creations and local caches cannot keep a dead target alive. Session-only or
// unconfirmed target-gone errors only schedule an enumeration to verify.
func (s *Server) observeTargetError(conn any, tid target.ID, err error) {
	if err == nil || tid == "" {
		return
	}
	if absentID, ok := browser.ConfirmedTargetAbsent(err); ok && absentID == tid {
		s.removeTargetState(tid)
		return
	}
	if browser.ErrorKindOf(err) == browser.ErrorTargetGone {
		requestEnumeration(conn)
	}
}

// removeEventTarget applies a queued lifecycle event reporting that tid is not
// a page (a destroyed notification goes through RemoveDestroyedTarget, see
// applyTargetEvent).
func (s *Server) removeEventTarget(conn any, tid target.ID) {
	s.dropTarget(conn, tid, s.tabs.RemoveEventTarget)
}

func (s *Server) dropTarget(conn any, tid target.ID, remove func(target.ID) bool) {
	if tid == "" {
		return
	}
	s.lifeMu.Lock()
	known := remove(tid)
	// Even an unknown target may have been persisted as managed by a previous
	// daemon: gone is gone for it too.
	s.forgetTabManagedLocked(tid)
	if known {
		s.beginTargetReleaseLocked(tid)
	}
	running := s.targetReleases[tid]
	s.lifeMu.Unlock()
	if known {
		s.retireTargetObservation(conn, tid)
		s.endTargetRelease(tid)
	}
	if running != nil {
		<-running.done
	}
}

// recoverManagedLocked settles the managed tabs a previous daemon process
// persisted, on the first successful enumeration (infos is its reply). Those
// are exactly the persisted members that are not idle-tracked: a tab this
// process creates enters both sets in one step (commitCreatedTab). Each was
// persisted before this process sent any request, so the reply is newer than
// it:
//   - known to the registry now: a live target has exactly the persisted
//     complete target id, and idle tracking resumes from the persisted last
//     activity (also when the target is suspended for the moment: it is
//     settled, together with its managed lifecycle, by the enumeration the
//     suspension requested);
//   - not listed by the reply: gone, retired. This keeps the persisted set
//     bounded by the live managed tabs;
//   - listed but not known: the registry would not take the reply's word
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
		if s.tabs.Known(tid) {
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
// all state alone. errTabCloseUnknownID is returned only when the browser was
// asked and does not have the tab; a failed lookup returns errTabLookup.
func (s *Server) closeTabLocked(ctx context.Context, conn tabConn, tab string) error {
	tid, err := s.resolveTab(ctx, conn, tab)
	if err != nil {
		return err
	}
	err = closeTarget(ctx, conn, tid)
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
		if !s.tabs.Known(tid) {
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
