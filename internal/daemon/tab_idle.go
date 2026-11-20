package daemon

import (
	"context"
	"errors"
	"fmt"
	"math"
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

func (s *Server) markTabManaged(tid target.ID, short, openURL string, silent bool) {
	if s.tabIdle != nil {
		s.tabIdle.MarkManaged(tid)
	}
}

func (s *Server) touchTabActivity(tid target.ID) {
	if s.tabIdle != nil {
		s.tabIdle.Touch(tid)
	}
}

func (s *Server) forgetTabManaged(tid target.ID) bool {
	return s.tabIdle != nil && s.tabIdle.Forget(tid)
}

// closingTab is the state of one explicit tab_close RPC in flight. If the
// target's destruction is observed before Target.closeTarget returns, the
// release is parked here so the RPC decides the single log record: its own on
// success, the synthetic one if it fails after all.
type closingTab struct {
	goneShort string
}

// releaseManagedTabLocked stops idle tracking for a tab that is gone and, if it
// was managed, queues the rpc.jsonl record saying so. Only the tab_close RPC
// writes such a line on its own; every other removal has to, or the short id
// stays managed on disk. Caller holds lifeMu, which is what orders this record
// after the tab's tab_new record.
func (s *Server) releaseManagedTabLocked(tid target.ID, short string) {
	if !s.idleRecovered && short != "" {
		s.goneBeforeRecovery[short] = struct{}{}
	}
	if !s.forgetTabManaged(tid) {
		return
	}
	if c := s.closing[tid]; c != nil {
		c.goneShort = short
		return
	}
	s.logManagedTabGone(short)
}

func (s *Server) logManagedTabGone(short string) {
	if s.store == nil || short == "" {
		return
	}
	s.enqueueAudit(store.ManagedTabGoneRecord(short, time.Now().UTC()))
}

// syncTabsFromTargets applies an enumeration that is known to be current.
// Request paths that fetch the list themselves use listAndSyncTabs instead.
func (s *Server) syncTabsFromTargets(infos []*target.Info) []state.TabSnapshot {
	return s.syncTabsFromTargetsSince(infos, math.MaxUint64)
}

// listAndSyncTabs enumerates page targets and reconciles daemon state with the
// reply. Target.getTargets can be answered before a concurrent tab_new's
// Target.createTarget and applied after it; the registry epoch captured up
// front keeps such a tab registered and managed instead of treating it as gone.
func (s *Server) listAndSyncTabs(ctx context.Context, conn tabConn) ([]state.TabSnapshot, error) {
	epoch := s.tabs.Epoch()
	targets, err := pageTargets(ctx, conn)
	if err != nil {
		return nil, err
	}
	return s.syncTabsFromTargetsSince(targets, epoch), nil
}

// syncTabsFromTargetsSince is the one place an enumeration changes daemon
// state. Managed tabs are released by full target id, and only those the
// registry removed for this enumeration; nothing is adopted from rpc.jsonl
// except by the very first successful enumeration after startup.
func (s *Server) syncTabsFromTargetsSince(infos []*target.Info, epoch uint64) []state.TabSnapshot {
	s.lifeMu.Lock()
	snaps, removed, _ := s.tabs.SyncPageTargetsSince(infos, epoch)
	for _, tab := range removed {
		s.releaseManagedTabLocked(tab.TargetID, tab.ShortID)
	}
	s.recoverIdleFromLogLocked(snaps)
	s.lifeMu.Unlock()
	for _, tab := range removed {
		s.clearTargetResources(tab.TargetID, tab.ShortID)
	}
	return snaps
}

// clearTargetResources drops the non-lifecycle state of a removed target.
func (s *Server) clearTargetResources(tid target.ID, short string) {
	if s.obsStore != nil {
		s.obsStore.ClearTarget(tid)
	}
	s.forgetTabLock(short)
	if sess, ok := s.tabConn().(*browser.Session); ok {
		sess.ForgetTarget(tid)
	}
}

func (s *Server) removeTargetState(tid target.ID) {
	s.lifeMu.Lock()
	short, ok := s.tabs.RemoveTarget(tid)
	if ok {
		s.releaseManagedTabLocked(tid, short)
	}
	s.lifeMu.Unlock()
	if ok {
		s.clearTargetResources(tid, short)
	}
}

// recoverIdleFromLogLocked adopts the managed tabs the previous run left in
// rpc.jsonl. It runs once, with the first successful enumeration: that is the
// only moment the log says more about the browser's tabs than memory does.
// Later the log lags memory (records are written asynchronously), and replaying
// it would hand a released short id to an unrelated tab that derived the same
// one. Caller holds lifeMu and has just synced the registry from snaps.
func (s *Server) recoverIdleFromLogLocked(snaps []state.TabSnapshot) {
	if s.idleRecovered {
		return
	}
	s.idleRecovered = true
	gone := s.goneBeforeRecovery
	s.goneBeforeRecovery = nil
	if s.tabIdle == nil || s.store == nil {
		return
	}
	loaded := s.store.StartupManagedTabActivity()

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
	for shortID, lastAt := range loaded {
		tid, ok := s.tabs.Lookup(shortID)
		if ok && s.tabIdle.IsManaged(tid) {
			// Created by a tab_new of this run; its own record keeps it managed.
			continue
		}
		_, released := gone[shortID]
		if !ok || released {
			// The previous run's tab is gone: absent from a successful
			// enumeration, or seen to go before it. Release its short id on
			// disk; whichever tab holds it now is not ours.
			s.logManagedTabGone(shortID)
			continue
		}
		if _, ok := present[tid]; !ok {
			continue
		}
		effective := lastAt
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
		if _, err := s.listAndSyncTabs(ctx, conn); err == nil {
			tid, ok = s.tabs.Lookup(tab)
		}
	}
	if !ok {
		return errTabCloseUnknownID
	}
	// A tab_close RPC logs its own record on success. Announce it before the
	// CDP call: targetDestroyed can be handled before closeTarget returns.
	viaRPC := auditMetaFrom(ctx) != nil
	if viaRPC {
		s.lifeMu.Lock()
		s.closing[tid] = &closingTab{}
		s.lifeMu.Unlock()
	}
	err := closeTarget(ctx, conn, tid)

	s.lifeMu.Lock()
	if viaRPC {
		c := s.closing[tid]
		delete(s.closing, tid)
		if err != nil && c != nil && c.goneShort != "" {
			// The tab went away, but this RPC reports failure and so logs no
			// successful tab_close: the release needs the synthetic record.
			s.logManagedTabGone(c.goneShort)
		}
		if err == nil {
			s.forgetTabManaged(tid)
		}
	}
	var short string
	var removed bool
	if err == nil {
		// Target.closeTarget succeeded, so clear every target-scoped resource
		// even if no later lifecycle event is delivered.
		if short, removed = s.tabs.RemoveTarget(tid); removed {
			s.releaseManagedTabLocked(tid, short)
		}
	}
	s.lifeMu.Unlock()
	if removed {
		s.clearTargetResources(tid, short)
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
				s.lifeMu.Lock()
				s.releaseManagedTabLocked(tid, short)
				s.lifeMu.Unlock()
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
