package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/yiplee/go-bb-browser/internal/browser"
	"github.com/yiplee/go-bb-browser/internal/state"
)

var (
	errTabCloseNoConn    = errors.New("browser session not ready")
	errTabCloseUnknownID = errors.New("unknown tab id")
	errTabLockTimeout    = fmt.Errorf("tab lock timeout: %w", context.DeadlineExceeded)
)

func (s *Server) markTabManaged(tid target.ID) {
	if s.tabIdle != nil {
		s.tabIdle.MarkManaged(tid)
	}
}

func (s *Server) touchTabActivity(tid target.ID) {
	if s.tabIdle != nil {
		s.tabIdle.Touch(tid)
	}
}

func (s *Server) forgetTabManaged(tid target.ID) {
	if s.tabIdle != nil {
		s.tabIdle.Forget(tid)
	}
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
	snaps, removed := s.tabs.ApplyEnumeration(tok, infos)
	for _, tab := range removed {
		s.clearRemovedTarget(tab.TargetID)
	}
	return snaps
}

// clearRemovedTarget drops per-target state. The keyed operation lock needs no
// cleanup here: lockTab reclaims its entry when the last reference exits.
func (s *Server) clearRemovedTarget(tid target.ID) {
	if s.obsStore != nil {
		s.obsStore.ClearTarget(tid)
	}
	s.forgetTabManaged(tid)
	if sess, ok := s.tabConn().(*browser.Session); ok {
		sess.ForgetTarget(tid)
	}
}

func (s *Server) removeTargetState(tid target.ID) {
	if !s.tabs.RemoveTarget(tid) {
		return
	}
	s.clearRemovedTarget(tid)
}

func (s *Server) reconcileIdleFromLog(snaps []state.TabSnapshot) {
	if s.tabIdle == nil || s.store == nil {
		return
	}
	loaded := s.store.ReplayManagedTabActivity()

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
	// The log names tabs by complete CDP target id only. A logged tab is
	// adopted when a page with exactly that id is still open; records written
	// before that format carry no such identity and were skipped by the store.
	for logged, lastAt := range loaded {
		tid := target.ID(logged)
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
func (s *Server) closeTabLocked(ctx context.Context, conn tabConn, tab string) error {
	tid, ok := s.resolveTab(ctx, conn, tab)
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
		if !s.tabs.Has(tid) {
			s.forgetTabManaged(tid)
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
				s.logger.Warn("tab idle cleanup failed", "tab", tab, "err", err)
			}
			continue
		}
		if s.logger != nil {
			s.logger.Info("tab idle closed", "tab", tab, "idle", timeout)
		}
	}
}
