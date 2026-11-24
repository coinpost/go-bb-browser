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
//
// browser.ErrorKindOf is the single authority: it trusts typed errors (CDPError,
// JS exceptions, protocol replies) before any message text. Do not match on
// err.Error() here — page-controlled text reaches this error chain.
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
	return browser.ErrorKindOf(err) == browser.ErrorTransport
}

// connectBrowserLocked attaches a new chromedp session. Caller must hold s.tabMu.
func (s *Server) connectBrowserLocked(ctx context.Context) error {
	// The registry observes the connection reader from the first message on:
	// lifecycle events are stamped, and destroyed notifications recorded, in
	// connection order before any reply that names targets can be applied.
	sess, err := browser.Connect(context.WithoutCancel(ctx), s.cfg.DebuggerURL, browser.WithTargetObserver(s.tabs))
	if err != nil {
		return err
	}
	if derr := sess.TargetDiscoveryErr(); derr != nil && s.logger != nil {
		s.logger.Warn("browser target discovery not enabled: tabs closed in the browser are only noticed by the next target enumeration", "err", derr)
	}
	s.tabLive = sess
	s.sessDone = sess.Close
	s.browserState = sessionReady
	s.browserFailures = 0
	s.browserLastProbe = time.Now()
	s.watchCDPSession(sess)
	go s.runTargetSync(ctx, sess)
	// Parent ctx has no deadline. Keep the old PageTargets 60s cap so a hung
	// Target.getTargets cannot block Listen forever, while SIGTERM still cancels.
	listCtx, cancel := context.WithTimeout(ctx, timeout.Operation)
	// runTargetSync is already registering lifecycle events: open the window
	// before the request so this snapshot cannot remove what they register.
	w := s.tabs.OpenWindow()
	targets, terr := sess.PageTargetsContext(listCtx)
	cancel()
	// A successful reply also performs the initial managed-tab recovery. If the
	// enumeration failed, runTargetSync retries it; recovery must not run on a
	// failed enumeration, which would retire still-live tabs.
	s.syncInitialTargets(sess, w, targets, terr)
	go s.runCDPWatchdog(ctx, sess)
	if s.logger != nil {
		s.logger.Info("browser CDP session connected", "debugger", s.cfg.DebuggerURL)
	}
	return nil
}

// syncInitialTargets applies the reply to the enumeration sent right after the
// browser connected. conn is the session it was sent on.
func (s *Server) syncInitialTargets(conn targetDirtyMarker, w *state.Window, targets []*target.Info, err error) []state.TabSnapshot {
	if err == nil {
		return s.applyEnumeration(conn, w, targets)
	}
	s.tabs.Abandon(w)
	if s.logger != nil {
		s.logger.Warn("list targets after browser connect", "err", err)
	}
	// The session is connected, so treat an initial enumeration failure as
	// transient and let runTargetSync retry it. Without this, persisted managed
	// tabs would never re-enter idle tracking unless another target event arrived.
	conn.MarkTargetDirty()
	return nil
}

// runTargetSync is the only consumer of the session's lifecycle event queue.
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
			s.applyTargetEvent(sess, ev)
			// Handle what is already queued before looking at the dirty flag,
			// so one resync answers a whole burst of events.
		drain:
			for range cap(sess.TargetEvents()) {
				select {
				case ev := <-sess.TargetEvents():
					s.applyTargetEvent(sess, ev)
				default:
					break drain
				}
			}
		case <-retry.C:
		}
		if sess.ConsumeTargetDirty() {
			probe, cancel := context.WithTimeout(ctx, s.cfg.CDPWatchdogTimeout)
			// Window before the request: RPCs keep registering and removing tabs
			// while this resync is in flight.
			w := s.tabs.OpenWindow()
			infos, err := sess.PageTargetsContext(probe)
			cancel()
			if err == nil {
				// Also settles managed tabs persisted by a previous process when
				// the startup enumeration failed.
				s.applyEnumeration(sess, w, infos)
			} else {
				s.tabs.Abandon(w)
				sess.MarkTargetDirty()
				s.triggerBrowserProbe()
				if s.logger != nil {
					s.logger.Warn("resync targets after lifecycle queue overflow", "err", err)
				}
			}
		}
	}
}

// applyTargetEvent applies one queued lifecycle event. "Gone" is always
// applied: it stays true however late it is handled (and the registry already
// stopped resolving the target when the notification was read). "Exists" is
// evidence about the instant the event was read (TabRegistry.RegisterEvent):
// it is applied with that instant, and not at all when something newer was
// applied meanwhile, so an event that waited in the queue across a tab_close,
// a resync or the read of the target's own destroyed notification cannot bring
// a closed tab back, and a target registered meanwhile by something later
// keeps the earlier instant. An event that did not settle the matter asks for
// a fresh enumeration instead, which registers the target if it is really
// there and cleans it up if the event proved it gone.
func (s *Server) applyTargetEvent(conn any, ev browser.TargetEvent) {
	if ev.Destroyed != "" {
		s.dropTarget(conn, ev.Destroyed, func(id target.ID) bool { return s.tabs.RemoveDestroyedTarget(ev.Stamp, id) })
	}
	if info := ev.Info; info != nil && info.TargetID != "" {
		if info.Type != "page" && info.Type != "tab" {
			s.removeEventTarget(conn, info.TargetID)
		} else if !s.tabs.RegisterEvent(ev.Stamp, info.TargetID) {
			requestEnumeration(conn)
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
