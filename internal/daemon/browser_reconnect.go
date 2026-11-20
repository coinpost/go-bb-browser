package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/yiplee/go-bb-browser/internal/browser"
	"github.com/yiplee/go-bb-browser/internal/state"
	"github.com/yiplee/go-bb-browser/internal/timeout"
)

var errBrowserNotConnected = errors.New("browser not connected")

type browserPinger interface {
	PingBrowserContext(context.Context) error
}

type sessionState uint8

const (
	sessionStarting sessionState = iota
	sessionReady
	sessionSuspect
	sessionFailed
)

// isCDPSessionLost reports errors that mean the browser CDP websocket/session is gone.
// context.Canceled is excluded — that is usually request shutdown, not CDP loss.
func isCDPSessionLost(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// ErrorKindOf is the single classifier: it already matches the transport
	// markers on raw errors, and deliberately ignores them inside replies the
	// browser delivered (page JS exceptions, CDP protocol errors).
	return browser.ErrorKindOf(err) == browser.ErrorTransport
}

// connectBrowser attaches a new chromedp session and applies the first target
// enumeration. tabMu is held only to publish the session: enumerating and
// applying the result take the locks they need themselves (cleanup of a removed
// target reads the session back through tabConn).
func (s *Server) connectBrowser(ctx context.Context) error {
	sess, err := browser.Connect(context.WithoutCancel(ctx), s.cfg.DebuggerURL)
	if err != nil {
		return err
	}
	s.tabMu.Lock()
	s.tabLive = sess
	s.sessDone = sess.Close
	s.browserState = sessionReady
	s.browserFailures = 0
	s.browserLastProbe = time.Now()
	s.tabMu.Unlock()
	s.watchCDPSession(sess)
	// The sync loop registers targets from events while the enumeration below
	// is in flight. The epoch from before both keeps a target the loop
	// registered and the (older) reply does not list.
	epoch := s.tabs.Epoch()
	s.bg.Go(func() { s.runTargetSync(ctx, sess) })
	// Parent ctx has no deadline. Keep the old PageTargets 60s cap so a hung
	// Target.getTargets cannot block Listen forever, while SIGTERM still cancels.
	listCtx, cancel := context.WithTimeout(ctx, timeout.Operation)
	targets, terr := sess.PageTargetsContext(listCtx)
	cancel()
	// A successful enumeration also performs the one-time managed-tab recovery.
	// If it failed, the first enumeration that does succeed (runTargetSync's
	// retry, or an RPC) performs it instead: recovering against an empty
	// registry would release every persisted managed tab.
	s.syncInitialTargets(sess, targets, terr, epoch)
	s.bg.Go(func() { s.runCDPWatchdog(ctx, sess) })
	if s.logger != nil {
		s.logger.Info("browser CDP session connected", "debugger", s.cfg.DebuggerURL)
	}
	return nil
}

type targetDirtyMarker interface {
	MarkTargetDirty()
}

// syncInitialTargets applies the enumeration requested when the registry
// epoch was epoch.
func (s *Server) syncInitialTargets(marker targetDirtyMarker, targets []*target.Info, err error, epoch uint64) []state.TabSnapshot {
	if err == nil {
		return s.syncTabsFromTargetsSince(targets, epoch)
	}
	if s.logger != nil {
		s.logger.Warn("list targets after browser connect", "err", err)
	}
	// The session is connected, so treat an initial enumeration failure as
	// transient and let runTargetSync retry it. Without this, persisted managed
	// tabs would never re-enter idle tracking unless another target event arrived.
	marker.MarkTargetDirty()
	return nil
}

func (s *Server) runTargetSync(ctx context.Context, sess *browser.Session) {
	if sess == nil {
		return
	}
	retry := time.NewTicker(time.Second)
	defer retry.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-sess.TargetEvents():
			if ev.Destroyed != "" {
				s.removeTargetState(ev.Destroyed)
			}
			if info := ev.Info; info != nil {
				if info.Type == "page" || info.Type == "tab" {
					s.tabs.RegisterPageTarget(info.TargetID)
				} else {
					s.removeTargetState(info.TargetID)
				}
			}
		case <-retry.C:
		}
		if sess.ConsumeTargetDirty() {
			probe, cancel := context.WithTimeout(ctx, s.cfg.CDPWatchdogTimeout)
			// A resync never re-reads rpc.jsonl: once the startup recovery has run,
			// managed state only changes by full target id (see syncTabsFromTargetsSince).
			_, err := s.listAndSyncTabs(probe, sess)
			cancel()
			if err != nil {
				sess.MarkTargetDirty()
				s.triggerBrowserProbe()
				if s.logger != nil {
					s.logger.Warn("resync targets after lifecycle queue overflow", "err", err)
				}
			}
		}
	}
}

// runCDPWatchdog owns all periodic Browser.getVersion probes. Business RPCs only
// consult the cached state and may request a coalesced, rate-limited early probe.
func (s *Server) runCDPWatchdog(ctx context.Context, sess *browser.Session) {
	interval := s.cfg.CDPWatchdogInterval
	if interval <= 0 {
		interval = DefaultCDPWatchdogInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.probeBrowserSession(ctx, sess)
		case <-s.probeTrigger:
			s.tabMu.RLock()
			last := s.browserLastProbe
			s.tabMu.RUnlock()
			if time.Since(last) >= interval {
				s.probeBrowserSession(ctx, sess)
			}
		}
	}
}

func (s *Server) probeBrowserSession(parent context.Context, pinger browserPinger) {
	if pinger == nil || !s.probeInFlight.CompareAndSwap(false, true) {
		return
	}
	defer s.probeInFlight.Store(false)
	timeout := s.cfg.CDPWatchdogTimeout
	if timeout <= 0 {
		timeout = DefaultCDPWatchdogTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	err := pinger.PingBrowserContext(ctx)
	cancel()

	now := time.Now()
	var fatal bool
	var failures int
	s.tabMu.Lock()
	s.browserLastProbe = now
	if err == nil {
		s.browserState = sessionReady
		s.browserFailures = 0
	} else {
		s.browserFailures++
		failures = s.browserFailures
		limit := s.cfg.CDPWatchdogFailures
		if limit <= 0 {
			limit = DefaultCDPWatchdogFailures
		}
		if failures >= limit {
			s.browserState = sessionFailed
			fatal = true
		} else {
			s.browserState = sessionSuspect
		}
	}
	s.tabMu.Unlock()

	if err != nil && s.logger != nil {
		s.logger.Warn("browser CDP watchdog probe failed", "err", err, "consecutive_failures", failures)
	}
	if fatal {
		s.fatalCDPLost(fmt.Errorf("watchdog failed %d consecutive probes: %w", failures, err))
	}
}

func (s *Server) triggerBrowserProbe() {
	if s == nil || s.probeTrigger == nil {
		return
	}
	select {
	case s.probeTrigger <- struct{}{}:
	default:
	}
}

func (s *Server) runObserverSweeper(ctx context.Context) {
	idle := s.cfg.ObserverIdleTimeout
	if idle <= 0 {
		return
	}
	interval := idle / 4
	if interval > 30*time.Second {
		interval = 30 * time.Second
	}
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if sess, ok := s.tabConn().(*browser.Session); ok {
				sess.SweepObservers(now, idle)
			}
		}
	}
}

// watchCDPSession exits the daemon when chromedp tears down the browser websocket
// (Chrome quit, debug port gone, etc.) so systemd/docker compose can restart us.
func (s *Server) watchCDPSession(sess *browser.Session) {
	if sess == nil || s.SkipBrowserAttach || s.tabHook != nil {
		return
	}
	go func() {
		<-sess.Context().Done()
		err := sess.Context().Err()
		if err == nil {
			err = errors.New("cdp session ended")
		}
		s.fatalCDPLost(err)
	}()
}

// fatalCDPLost stops the daemon with a non-zero exit so a supervisor reconnects cleanly.
func (s *Server) fatalCDPLost(err error) {
	if err == nil {
		err = errors.New("cdp session lost")
	}
	s.fatalOnce.Do(func() {
		s.tabMu.Lock()
		s.browserState = sessionFailed
		s.tabMu.Unlock()
		wrapped := fmt.Errorf("cdp session lost: %w", err)
		if s.logger != nil {
			s.logger.Error("cdp disconnected, exiting for supervisor restart", "err", err)
		}
		if s.stopRun != nil {
			s.stopRun()
		}
		select {
		case s.fatalCh <- wrapped:
		default:
		}
	})
}

// ensureBrowserSession is an O(1) cached-state check. It never sends a CDP command.
func (s *Server) ensureBrowserSession(ctx context.Context) error {
	_ = ctx
	if s.SkipBrowserAttach || s.tabHook != nil {
		return nil
	}

	s.tabMu.RLock()
	live := s.tabLive
	state := s.browserState
	s.tabMu.RUnlock()

	if live == nil {
		return errBrowserNotConnected
	}
	if state == sessionFailed {
		return errBrowserNotConnected
	}
	return nil
}
