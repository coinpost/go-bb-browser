package daemon

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/yiplee/go-bb-browser/internal/browser"
	"github.com/yiplee/go-bb-browser/internal/state"
	"github.com/yiplee/go-bb-browser/internal/store"
)

var (
	errTabCloseNoConn = errors.New("browser session not ready")
	errUnknownTabID   = errors.New("unknown tab id")
	errTabLockTimeout = fmt.Errorf("tab lock timeout: %w", context.DeadlineExceeded)
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

// forgetTabManagedLocked ends idle tracking of tid and reports whether it was
// managed. Caller holds lifeMu and queues the record that releases the tab on
// disk: every managed tab is registered, and only the lifecycle step that
// unregisters one may forget it. Anything else that forgot a tab would make
// that step skip the record.
func (s *Server) forgetTabManagedLocked(tid target.ID) bool {
	return s.tabIdle != nil && s.tabIdle.Forget(tid)
}

// closingTab is the state of one explicit tab_close RPC in flight. If the
// target's destruction is observed before Target.closeTarget returns, the
// release is parked here so the RPC decides the single log record: its own on
// success, the synthetic one if it fails after all.
type closingTab struct {
	gone      bool
	goneShort string
}

// createWatch remembers targets seen to go away while a Target.createTarget is
// in flight. Chrome can report a new target created and destroyed before the
// createTarget reply is handled; without this the reply would register, and
// keep managed, a tab that no longer exists. Entries live only as long as a
// create that was in flight when they were recorded.
type createWatch struct {
	started  uint64
	inFlight map[uint64]struct{}
	gone     map[target.ID]uint64 // target -> value of started when it went
}

// beginCreate opens the window in which removals are remembered. Every call is
// paired with endCreate or endCreateLocked.
func (s *Server) beginCreate() uint64 {
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()
	c := &s.creates
	if c.inFlight == nil {
		c.inFlight = make(map[uint64]struct{})
		c.gone = make(map[target.ID]uint64)
	}
	c.started++
	c.inFlight[c.started] = struct{}{}
	return c.started
}

// endCreate closes the window of a create that produced no target.
func (s *Server) endCreate(n uint64) {
	s.lifeMu.Lock()
	s.endCreateLocked(n, "")
	s.lifeMu.Unlock()
}

// endCreateLocked closes create n's window and reports whether tid, the target
// it produced, was seen to go away meanwhile. Caller holds lifeMu.
func (s *Server) endCreateLocked(n uint64, tid target.ID) (gone bool) {
	c := &s.creates
	delete(c.inFlight, n)
	if tid != "" {
		_, gone = c.gone[tid]
		delete(c.gone, tid)
	}
	oldest := uint64(math.MaxUint64)
	for m := range c.inFlight {
		oldest = min(oldest, m)
	}
	for id, at := range c.gone {
		if at < oldest {
			delete(c.gone, id)
		}
	}
	return gone
}

// noteTargetGoneLocked records an observed removal of tid, registered or not,
// for the creates in flight. Caller holds lifeMu.
func (s *Server) noteTargetGoneLocked(tid target.ID) {
	if c := &s.creates; len(c.inFlight) > 0 {
		c.gone[tid] = c.started
	}
}

// releaseManagedTabLocked stops idle tracking for a tab that is gone and, if it
// was managed, queues the rpc.jsonl record saying so. Only the tab_close RPC
// writes such a line on its own; every other removal has to, or the tab stays
// managed on disk. Caller holds lifeMu, which is what orders this record after
// the tab's tab_new record, and has just unregistered tid.
func (s *Server) releaseManagedTabLocked(tid target.ID, short string) {
	if !s.idleRecovered {
		s.goneBeforeRecovery[tid] = struct{}{}
	}
	if !s.forgetTabManagedLocked(tid) {
		return
	}
	if c := s.closing[tid]; c != nil {
		c.gone, c.goneShort = true, short
		return
	}
	s.logManagedTabGone(tid, short)
}

// logManagedTabGone queues the synthetic tab_close that releases tid on disk.
func (s *Server) logManagedTabGone(tid target.ID, short string) {
	if s.store == nil || tid == "" {
		return
	}
	s.enqueueAudit(store.ManagedTabGoneRecord(short, string(tid), time.Now().UTC()))
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
		s.noteTargetGoneLocked(tab.TargetID)
		s.releaseManagedTabLocked(tab.TargetID, tab.ShortID)
	}
	s.recoverIdleFromLogLocked()
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
	s.noteTargetGoneLocked(tid)
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
// Later the log lags memory (records are written asynchronously).
//
// Ownership is decided by full target id. Short ids are reassigned on every
// run, in registration order, so the tab that holds a logged short id now need
// not be the tab that held it then. Caller holds lifeMu and has just synced the
// registry.
func (s *Server) recoverIdleFromLogLocked() {
	if s.idleRecovered {
		return
	}
	s.idleRecovered = true
	gone := s.goneBeforeRecovery
	s.goneBeforeRecovery = nil
	if s.tabIdle == nil || s.store == nil {
		return
	}
	tabs, legacy := s.store.StartupManaged()

	now := time.Now()
	timeout := s.cfg.TabIdleTimeout
	grace := s.cfg.IdleStartupGrace
	minLast := now
	if timeout > grace {
		minLast = now.Add(-(timeout - grace))
	}
	adopt := func(tid target.ID, lastAt time.Time) {
		if lastAt.Before(minLast) {
			lastAt = minLast
		}
		s.tabIdle.MarkManagedIfAbsentAt(tid, lastAt)
	}

	for _, tab := range tabs {
		tid := target.ID(tab.Target)
		_, registered := s.tabs.ShortForTarget(tid)
		if _, released := gone[tid]; released || !registered {
			// The previous run's tab is gone: absent from a successful
			// enumeration, or seen to go before it.
			s.logManagedTabGone(tid, tab.Short)
			continue
		}
		adopt(tid, tab.LastActivity)
	}

	// Entries logged by short id only, from before records carried a target
	// id. A short id does not identify a tab: the tab that had it may be gone
	// and a user's page may be the only one left with the same id suffix, so
	// even a single matching tab proves nothing. None is adopted. The lines are
	// retired, and the tabs they meant are no longer idle-closed (they stay
	// open until closed explicitly); that is the price of never guessing.
	shorts := make([]string, 0, len(legacy))
	for short := range legacy {
		shorts = append(shorts, short)
	}
	sort.Strings(shorts)
	for _, short := range shorts {
		s.enqueueAudit(store.ManagedTabGoneRecord(short, "", now.UTC()))
	}
	if len(shorts) > 0 && s.logger != nil {
		s.logger.Warn("managed tabs logged by short id only are not recovered; they will not be idle-closed", "tabs", shorts)
	}
}

// closeTabByShort closes the tab that holds the short id tab when the request
// arrives, and no other: see bindTab.
func (s *Server) closeTabByShort(ctx context.Context, tab string) error {
	conn := s.tabConn()
	if conn == nil {
		return errTabCloseNoConn
	}
	tid, unlock, err := s.bindTab(ctx, tab)
	if err != nil {
		return err
	}
	defer unlock()
	return s.closeTargetLocked(ctx, conn, tid)
}

// closeTargetLocked closes exactly tid and clears the daemon state of tid, and
// of nothing else: a short id resolved again after the CDP call could by then
// belong to a different tab. The caller holds the operation lock of the short
// id tid had when it was resolved.
func (s *Server) closeTargetLocked(ctx context.Context, conn tabConn, tid target.ID) error {
	// A tab_close RPC logs its own record on success. Announce it before the
	// CDP call: targetDestroyed can be handled before closeTarget returns.
	meta := auditMetaFrom(ctx)
	if meta != nil {
		s.lifeMu.Lock()
		s.closing[tid] = &closingTab{}
		s.lifeMu.Unlock()
	}
	err := closeTarget(ctx, conn, tid)

	s.lifeMu.Lock()
	var parked *closingTab
	if meta != nil {
		parked = s.closing[tid]
		delete(s.closing, tid)
	}
	var short string
	var removed bool
	switch {
	case err == nil:
		if meta != nil {
			// The RPC's record names the target and is the release.
			meta.target = tid
			s.forgetTabManagedLocked(tid)
		}
		// Target.closeTarget succeeded, so clear every target-scoped resource
		// even if no later lifecycle event is delivered.
		if short, removed = s.tabs.RemoveTarget(tid); removed {
			s.releaseManagedTabLocked(tid, short)
		}
		// A tab_new whose createTarget reply is still on its way must learn
		// that its target is gone, even if no destroyed event is handled first.
		s.noteTargetGoneLocked(tid)
	case parked != nil && parked.gone:
		// The tab went away, but this RPC reports failure and so logs no
		// successful tab_close: the release needs the synthetic record.
		s.logManagedTabGone(tid, parked.goneShort)
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
	interval := s.idleSweepInterval
	if interval <= 0 {
		interval = max(timeout/6, 10*time.Second)
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
		// The short id only names the operation lock and the log lines here;
		// what gets closed is tid.
		short, ok := s.tabs.ShortForTarget(tid)
		if !ok {
			// A removal of tid is in progress or done. Its lifecycle step
			// forgets the tab and logs the release; doing it here would make
			// that step skip the record.
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
		if s.beforeIdleClose != nil {
			s.beforeIdleClose(tid)
		}
		err := s.closeTargetLocked(ctx, conn, tid)
		if err != nil && browser.ErrorKindOf(err) == browser.ErrorTargetGone {
			// Idle cleanup is complete when Chrome has already removed the target.
			// Keep explicit tab_close RPC errors unchanged.
			s.removeTargetState(tid)
			err = nil
		}
		unlock()
		if err != nil {
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
