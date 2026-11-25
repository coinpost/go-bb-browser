package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/coinpost/go-bb-browser/internal/browser"
	"github.com/coinpost/go-bb-browser/internal/state"
	"github.com/coinpost/go-bb-browser/internal/store"
	"github.com/coinpost/go-bb-browser/internal/timeout"
	"github.com/coinpost/go-bb-browser/pkg/protocol"
)

const (
	readTimeout = 30 * time.Second
	flushMargin = 15 * time.Second
)

// Server is the HTTP API front-end for the daemon (JSON-RPC over POST /v1).
type Server struct {
	cfg    Config
	logger *slog.Logger
	mux    *http.ServeMux

	store    *store.Store
	tabs     *state.TabRegistry
	tabMu    sync.RWMutex
	tabLive  tabConn // set after CDP connect in ListenAndServe
	tabHook  tabConn // optional: tests inject fake CDP
	sessDone func()  // releases browser session references on shutdown (does not close Chrome)

	browserState     sessionState
	browserFailures  int
	browserLastProbe time.Time
	probeTrigger     chan struct{}
	probeInFlight    atomic.Bool

	obsStore *state.TabObsStore
	obsSink  *obsSink
	tabIdle  *state.TabIdleTracker

	// lifeMu serializes the application of lifecycle evidence to the registry
	// and everything keyed by a registered target (see tab_idle.go). Lock
	// order: lifeMu before the registry / idle tracker / store locks.
	lifeMu sync.Mutex
	// managedRecovered is set once the managed tabs persisted by a previous
	// daemon process have been settled. Guarded by lifeMu.
	managedRecovered bool
	// targetReleases has an entry per target id for as long as any release of
	// that id is still running (see beginTargetReleaseLocked). Guarded by lifeMu.
	targetReleases map[target.ID]*targetRelease
	// obsStepHook is nil outside tests. Lifecycle regressions set it to park an
	// observation query between two of its steps (see obsStep*).
	obsStepHook func(step string, tid target.ID)

	// auditCloseHook is nil outside tests. It sees the result of the final
	// store.Close (which has run) and returns what closeAudit reports, so a
	// shutdown whose log could not be confirmed can be exercised.
	auditCloseHook func(error) error

	tabMuOps    sync.Mutex
	tabCDPLocks map[string]*tabLockEntry

	auditMu     sync.Mutex
	auditClosed bool
	auditCh     chan store.LogRecord
	auditDone   chan struct{}
	auditWG     sync.WaitGroup

	// shutdownTimeout bounds http.Server.Shutdown. Zero uses the default.
	// A timed-out shutdown gets one more timeout.Operation to drain handlers.
	shutdownTimeout time.Duration
	// settleTimeout bounds each wait of stopBackground. Zero uses the default
	// (settleBudget).
	settleTimeout time.Duration

	// CDP loss triggers fatal exit (supervisor restart) instead of in-process reconnect.
	fatalOnce sync.Once
	fatalCh   chan error
	// stopRun cancels the context of the background work ListenAndServe owns
	// (bg). That context is not derived from the caller's: the caller's stop
	// signal only stops the HTTP service, see stopBackground.
	stopRun context.CancelFunc
	bg      background

	// SkipBrowserAttach skips CDP connect in ListenAndServe (tests without Chrome).
	SkipBrowserAttach bool
}

// NewServer builds a daemon HTTP server with the given config (must pass Validate).
func NewServer(cfg Config, logger *slog.Logger) (*Server, error) {
	if logger == nil {
		logger = slog.Default()
	}
	cfg.APITokens = normalizeAPITokens(cfg.APITokens)
	obsStore := state.NewTabObsStore()
	s := &Server{
		cfg:          cfg,
		logger:       logger,
		mux:          http.NewServeMux(),
		tabs:         state.NewTabRegistry(),
		obsStore:     obsStore,
		tabIdle:      state.NewTabIdleTracker(),
		probeTrigger: make(chan struct{}, 1),
		auditCh:      make(chan store.LogRecord, 256),
		auditDone:    make(chan struct{}),
	}
	stateDir := effectiveStateDir(cfg)
	if strings.TrimSpace(cfg.StateDir) == stateDirDisabled {
		stateDir = stateDirDisabled
	}
	st, err := store.Open(store.OpenConfig{StateDir: stateDir, Logger: logger, MaxLogBytes: cfg.MaxLogBytes})
	if err != nil {
		return nil, fmt.Errorf("rpc log store: %w", err)
	}
	s.store = st
	s.obsSink = &obsSink{store: st, obs: obsStore, logger: logger}
	go s.runAuditWriter()
	s.routes()
	s.warnUnauthenticatedListen()
	s.warnLoopbackAuthentication()
	return s, nil
}

func (s *Server) routes() {
	s.mux.HandleFunc("/health", s.handleHealthRoute)
	s.mux.HandleFunc("/live", s.handleLiveRoute)
	s.mux.HandleFunc("/ready", s.handleReadyRoute)
	s.mux.HandleFunc("/v1", s.handleV1)
}

func (s *Server) tabConn() tabConn {
	if s.tabHook != nil {
		return s.tabHook
	}
	s.tabMu.RLock()
	defer s.tabMu.RUnlock()
	return s.tabLive
}

func (s *Server) Handler() http.Handler {
	return http.MaxBytesHandler(s.mux, s.cfg.MaxBodyBytes)
}

func (s *Server) handleHealthRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.handleHealthGet(w, r)
		return
	}
	w.Header().Set("Allow", http.MethodGet)
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func (s *Server) handleHealthGet(w http.ResponseWriter, r *http.Request) {
	result := s.healthResult(false)
	s.writeHealthResult(w, result)
}

func (s *Server) handleReadyRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.writeHealthResult(w, s.healthResult(true))
}

func (s *Server) handleLiveRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	b, err := json.Marshal(protocol.LivenessResult{Status: "ok"})
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

func (s *Server) writeHealthResult(w http.ResponseWriter, result protocol.HealthResult) {
	b, err := json.Marshal(result)
	if err != nil {
		s.logger.Error("health json encode failed", "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	status := http.StatusOK
	if result.Browser == protocol.HealthBrowserDisconnected {
		status = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if _, err := w.Write(b); err != nil {
		s.logger.Error("health response write failed", "err", err)
	}
}

func newHTTPServer(handler http.Handler) *http.Server {
	// net/http arms WriteTimeout when the header read returns, before the
	// handler reads the body and before handleV1 starts timeout.Operation.
	// Cover body reading and CDP work, then allow time to flush the response.
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       readTimeout,
		WriteTimeout:      readTimeout + timeout.Operation + flushMargin,
		IdleTimeout:       120 * time.Second,
	}
}

// background is the work one ListenAndServe call owns besides the HTTP
// service: the target sync loop of the session it attached (the only consumer
// of that session's lifecycle queue) and the periodic tasks (CDP watchdog,
// idle and observer sweepers). All of it runs on one context, cancelled by
// stopRun; stopBackground ends it. Written before the goroutines start and by
// the ListenAndServe goroutine only.
type background struct {
	// sess is the session whose lifecycle queue syncDone's loop consumes; nil
	// when no browser was attached.
	sess *browser.Session
	// settle is closed to ask the sync loop to finish its work and return
	// (settleTargets); nil outside ListenAndServe, where the loop only ends
	// with its context.
	settle chan struct{}
	// syncDone is closed when the sync loop has returned.
	syncDone chan struct{}
	// tasks counts the periodic tasks.
	tasks sync.WaitGroup
}

// goTask runs f as one of the periodic background tasks.
func (s *Server) goTask(f func()) {
	s.bg.tasks.Go(f)
}

// settleBudget bounds each wait of stopBackground: the sync loop's final
// settlement sends at most maxSettleEnumerations enumerations of at most
// CDPWatchdogTimeout each on top of whatever resync it was in.
func (s *Server) settleBudget() time.Duration {
	if s.settleTimeout > 0 {
		return s.settleTimeout
	}
	probe := s.cfg.CDPWatchdogTimeout
	if probe <= 0 {
		probe = DefaultCDPWatchdogTimeout
	}
	return (maxSettleEnumerations+1)*probe + 5*time.Second
}

// stopBackground ends the background work once the HTTP service has stopped,
// before the session is released and the RPC log closed. settle says that the
// browser connection is still to be trusted (not a fatal CDP loss or a failed
// startup).
//
//  1. With settle, the sync loop is asked to finish: it applies every lifecycle
//     event the connection has read and answers the enumeration requests still
//     open (settleTargets), then returns. In-flight RPCs have drained by now,
//     so nothing produces further requests.
//  2. The background context is cancelled and the loop and the periodic tasks
//     are waited for.
//  3. Events read meanwhile and still queued are applied (no browser round
//     trip): a destroyed notification the daemon was told about is not lost
//     because the loop had already stopped — the fatal path ends here.
//
// Every wait is bounded by settleBudget. A loop that does not return in time
// is abandoned with a warning: nothing is concluded for it, the session and
// log are closed regardless, and a write that loses that race gets
// store.ErrClosed. What stays unsettled (a retirement not written) is settled
// by the first successful enumeration of the next daemon process.
func (s *Server) stopBackground(settle bool) {
	budget := s.settleBudget()
	bg := &s.bg
	if settle && bg.syncDone != nil && bg.settle != nil {
		close(bg.settle)
		if !waitClosed(bg.syncDone, budget) && s.logger != nil {
			s.logger.Warn("target lifecycle settlement did not finish in time; stopping it", "budget", budget)
		}
	}
	if s.stopRun != nil {
		s.stopRun()
	}
	if bg.syncDone != nil {
		if waitClosed(bg.syncDone, budget) {
			// The loop has returned: this goroutine is now the only consumer.
			s.drainTargetEvents(bg.sess)
		} else if s.logger != nil {
			s.logger.Warn("target sync did not stop in time; lifecycle events still queued are not applied", "budget", budget)
		}
	}
	tasksDone := make(chan struct{})
	go func() { bg.tasks.Wait(); close(tasksDone) }()
	if !waitClosed(tasksDone, budget) && s.logger != nil {
		s.logger.Warn("background tasks did not stop in time", "budget", budget)
	}
}

// waitClosed waits for done at most d and reports whether it was closed.
func waitClosed(done <-chan struct{}, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}

// ListenAndServe starts the HTTP server until ctx is cancelled, the browser
// connection is lost or the HTTP service fails.
//
// Shutdown order, the same on all three exits: (1) the HTTP server stops
// accepting and in-flight requests drain (shutdownHTTP) while the browser
// session, the target sync loop and the RPC log keep working for them; (2) the
// lifecycle events read so far are applied and the background work stops
// (stopBackground); (3) the session is released; (4) the RPC log is closed.
// Cancelling ctx therefore does not cancel the background work by itself.
//
// The RPC log is closed last. Its close error (a state-dir sync still owed by
// a rotation, or the file close itself) is part of the result: it is joined to
// whatever ended the service, which stays first and intact, and an otherwise
// clean shutdown returns it alone — the daemon must not exit silently with its
// log unconfirmed.
func (s *Server) ListenAndServe(ctx context.Context) (retErr error) {
	runCtx, stopRun := context.WithCancel(context.WithoutCancel(ctx))
	s.stopRun = stopRun
	s.fatalCh = make(chan error, 1)
	s.bg.settle = make(chan struct{})
	// Until the service is up, the caller's stop signal still aborts startup
	// work (the first enumeration); afterwards it only starts the shutdown.
	startupDone := context.AfterFunc(ctx, stopRun)
	defer startupDone()

	defer func() {
		if err := s.closeAudit(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close rpc log (its state on disk is not confirmed): %w", err))
		}
	}()
	defer func() {
		s.tabMu.Lock()
		if s.sessDone != nil {
			s.sessDone()
		}
		s.tabLive = nil
		s.sessDone = nil
		s.tabMu.Unlock()
	}()
	// Runs first: the background work stops before the session and the log go.
	// settle is set once the HTTP service has stopped in an orderly way.
	settle := false
	defer func() { s.stopBackground(settle && runCtx.Err() == nil) }()

	if !s.SkipBrowserAttach && s.tabHook == nil {
		if err := s.connectBrowser(runCtx); err != nil {
			return fmt.Errorf("cdp attach: %w", err)
		}
	}

	ln, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	srv := newHTTPServer(s.Handler())

	errCh := make(chan error, 1)
	go func() {
		s.logger.Info("daemon listening", "addr", ln.Addr().String(), "debugger", s.cfg.DebuggerURL)
		err := srv.Serve(ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- http.ErrServerClosed
	}()

	if s.cfg.TabIdleTimeout > 0 {
		s.goTask(func() { s.runTabIdleSweeper(runCtx) })
	}
	if s.cfg.ObserverIdleTimeout > 0 {
		s.goTask(func() { s.runObserverSweeper(runCtx) })
	}
	startupDone()

	select {
	case <-ctx.Done():
		err := s.shutdownHTTP(srv)
		settle = true
		if err != nil {
			return fmt.Errorf("http shutdown: %w", err)
		}
		<-errCh
		return nil
	case err := <-s.fatalCh:
		// fatalCDPLost cancelled the background context: the connection is not
		// asked anything any more, stopBackground only applies what was read.
		_ = s.shutdownHTTP(srv)
		<-errCh
		return err
	case err := <-errCh:
		// The listener failed; requests already accepted still get to finish
		// before the session goes.
		_ = s.shutdownHTTP(srv)
		settle = true
		return err
	}
}

func (s *Server) shutdownHTTP(srv *http.Server) error {
	budget := s.shutdownTimeout
	if budget <= 0 {
		budget = 15 * time.Second
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), budget)
	err := srv.Shutdown(shutdownCtx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	shutdownCtx, cancel = context.WithTimeout(context.Background(), timeout.Operation)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		_ = srv.Close()
		return err
	}
	return nil
}

// closeAudit rejects further channel sends, drains the writer, and closes the store.
// A handler that loses the race appends synchronously while the file is still open.
func (s *Server) closeAudit() error {
	if s.store == nil {
		return nil
	}
	s.auditMu.Lock()
	if !s.auditClosed {
		s.auditClosed = true
		close(s.auditCh)
	}
	s.auditMu.Unlock()
	<-s.auditDone
	s.auditMu.Lock()
	err := s.store.Close()
	s.auditMu.Unlock()
	if s.auditCloseHook != nil {
		err = s.auditCloseHook(err)
	}
	return err
}

func (s *Server) runAuditWriter() {
	defer close(s.auditDone)
	for rec := range s.auditCh {
		if err := s.store.AppendRPC(rec); err != nil && s.logger != nil {
			s.logger.Warn(auditFailureMessage(err), "err", err, "action", rec.Action)
		}
		s.auditWG.Done()
	}
}
