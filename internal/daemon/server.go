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
	"github.com/yiplee/go-bb-browser/internal/state"
	"github.com/yiplee/go-bb-browser/internal/store"
	"github.com/yiplee/go-bb-browser/internal/timeout"
	"github.com/yiplee/go-bb-browser/pkg/protocol"
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

	tabMuOps    sync.Mutex
	tabCDPLocks map[string]*tabLockEntry

	// lifeMu is the single serial boundary for managed-tab lifecycle: marking a
	// tab managed, releasing it, and queueing the rpc.jsonl records that say so.
	// Whatever order two lifecycle steps take under it is the order of their
	// log lines. It is never held across a CDP call.
	lifeMu sync.Mutex
	// idleRecovered is set by the first successful target enumeration, the only
	// point at which managed tabs are adopted from the previous run's log.
	idleRecovered bool
	// goneBeforeRecovery holds targets seen to go away before that point, so
	// recovery does not adopt one from an enumeration that still listed it.
	goneBeforeRecovery map[target.ID]struct{}
	// closing tracks targets with an explicit tab_close RPC in flight.
	closing map[target.ID]*closingTab
	// creates tracks removals observed while a tab_new's createTarget is in flight.
	creates createWatch

	// bg counts the goroutines that outlive a request and can queue audit
	// records (target sync, watchdog, sweepers). ListenAndServe joins them
	// before the audit queue and the store are closed.
	bg sync.WaitGroup

	// Test seams: sweeper period (zero derives it from the idle timeout), a
	// hook run once an idle close is decided, before its CDP call, and a hook
	// run once a target enumeration's reply is in, before it is applied.
	idleSweepInterval time.Duration
	beforeIdleClose   func(target.ID)
	afterListTargets  func()

	// Audit records form one FIFO: auditPending, appended under auditMu, written
	// in order by whoever holds auditWriteMu (the writer, or an enqueuer once
	// the queue is over auditQueueCap or the writer has stopped).
	auditMu       sync.Mutex
	auditWriteMu  sync.Mutex
	auditPending  []store.LogRecord
	auditQueueCap int
	auditClosed   bool
	auditWake     chan struct{}
	auditStop     chan struct{}
	auditDone     chan struct{}
	auditWG       sync.WaitGroup

	// connectTimeout bounds the initial CDP handshake. Zero uses
	// browser.DefaultConnectTimeout.
	connectTimeout time.Duration

	// shutdownTimeout bounds http.Server.Shutdown. Zero uses the default.
	// A timed-out shutdown gets shutdownDrain more (zero: timeout.Operation) to
	// drain handlers before connections are closed.
	shutdownTimeout time.Duration
	shutdownDrain   time.Duration

	// RPC handlers are audit producers too. rpcGate admits and counts them so
	// shutdown can stop admitting, cancel the ones still running (rpcCtx) and
	// wait for them before the audit queue and the store are closed.
	rpcGate   handlerGate
	rpcCtx    context.Context
	rpcCancel context.CancelFunc

	// CDP loss triggers fatal exit (supervisor restart) instead of in-process reconnect.
	fatalOnce sync.Once
	fatalCh   chan error
	stopRun   context.CancelFunc

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
		closing:      make(map[target.ID]*closingTab),

		goneBeforeRecovery: make(map[target.ID]struct{}),

		auditQueueCap: defaultAuditQueueCap,
		auditWake:     make(chan struct{}, 1),
		auditStop:     make(chan struct{}),
		auditDone:     make(chan struct{}),
	}
	s.rpcCtx, s.rpcCancel = context.WithCancel(context.Background())
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

// ListenAndServe starts the HTTP server until ctx is cancelled or Listen fails.
func (s *Server) ListenAndServe(ctx context.Context) error {
	runCtx, stopRun := context.WithCancel(ctx)
	s.stopRun = stopRun
	s.fatalCh = make(chan error, 1)

	// Exit order, on every path: shutdownHTTP drains handlers within its
	// budget; then no further RPC is admitted, the ones still running are
	// cancelled and joined; then the background goroutines are cancelled and
	// joined; and only then are the session, the audit queue and the store
	// closed. Whoever has changed a tab in Chrome must still get to log it, or
	// the next run would read a log that disagrees with the browser.
	defer func() { _ = s.closeAudit() }()
	defer func() {
		s.tabMu.Lock()
		if s.sessDone != nil {
			s.sessDone()
		}
		s.tabLive = nil
		s.sessDone = nil
		s.tabMu.Unlock()
	}()
	defer func() {
		stopRun()
		s.bg.Wait()
	}()
	defer s.stopRPCHandlers()

	if !s.SkipBrowserAttach && s.tabHook == nil {
		if err := s.connectBrowser(runCtx); err != nil {
			if ctx.Err() != nil {
				// Asked to stop while still connecting: same as a stop later on.
				return nil
			}
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
		s.bg.Go(func() { s.runTabIdleSweeper(runCtx) })
	}
	if s.cfg.ObserverIdleTimeout > 0 {
		s.bg.Go(func() { s.runObserverSweeper(runCtx) })
	}

	select {
	case <-ctx.Done():
		if err := s.shutdownHTTP(srv); err != nil {
			return fmt.Errorf("http shutdown: %w", err)
		}
		<-errCh
		return nil
	case err := <-s.fatalCh:
		_ = s.shutdownHTTP(srv)
		<-errCh
		return err
	case err := <-errCh:
		// Serve failed on its own; requests already accepted still run.
		_ = s.shutdownHTTP(srv)
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
	drain := s.shutdownDrain
	if drain <= 0 {
		drain = timeout.Operation
	}
	shutdownCtx, cancel = context.WithTimeout(context.Background(), drain)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		_ = srv.Close()
		return err
	}
	return nil
}

// handlerGate admits RPC handlers until it is closed and lets the closer wait
// for the admitted ones. Admission and closing take the same lock, so a handler
// is either counted before the wait starts or turned away.
type handlerGate struct {
	mu     sync.Mutex
	closed bool
	active int
	idle   chan struct{} // closed when the last admitted handler leaves a closed gate
}

func (g *handlerGate) enter() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return false
	}
	g.active++
	return true
}

func (g *handlerGate) leave() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.active--
	if g.closed && g.active == 0 {
		close(g.idle)
	}
}

// close stops admission and returns a channel that is closed once no admitted
// handler is left.
func (g *handlerGate) close() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.closed {
		g.closed = true
		g.idle = make(chan struct{})
		if g.active == 0 {
			close(g.idle)
		}
	}
	return g.idle
}

// stopRPCHandlers ends RPC handling for good: nothing new is admitted, the
// context of every handler still running is cancelled, and it returns when
// they have all returned. After a graceful HTTP shutdown none is left and this
// is immediate. After a forced one the wait lasts as long as the handlers take
// to notice the cancellation; a CDP call that ignores its context holds it up,
// which is the price of never closing the store under a handler that may
// still record what it did. No lock is held while waiting.
func (s *Server) stopRPCHandlers() {
	idle := s.rpcGate.close()
	s.rpcCancel()
	<-idle
}

// defaultAuditQueueCap bounds how many audit records wait for the writer.
const defaultAuditQueueCap = 256

// closeAudit stops the writer, drains the queue, and closes the store.
// A handler that loses the race appends synchronously, in queue order.
func (s *Server) closeAudit() error {
	if s.store == nil {
		return nil
	}
	s.stopAuditWriter()
	s.auditWriteMu.Lock()
	defer s.auditWriteMu.Unlock()
	return s.store.Close()
}

// stopAuditWriter ends the writer goroutine after it has drained the queue.
// Later records are written by their enqueuer.
func (s *Server) stopAuditWriter() {
	s.auditMu.Lock()
	if !s.auditClosed {
		s.auditClosed = true
		close(s.auditStop)
	}
	s.auditMu.Unlock()
	<-s.auditDone
}

func (s *Server) runAuditWriter() {
	defer close(s.auditDone)
	for {
		select {
		case <-s.auditWake:
			s.flushAudit()
		case <-s.auditStop:
			s.flushAudit()
			return
		}
	}
}

// flushAudit writes every queued record in queue order. Batches are taken and
// written under auditWriteMu, so concurrent flushers cannot interleave them.
func (s *Server) flushAudit() {
	s.auditWriteMu.Lock()
	defer s.auditWriteMu.Unlock()
	s.auditMu.Lock()
	batch := s.auditPending
	s.auditPending = nil
	s.auditMu.Unlock()
	for _, rec := range batch {
		if err := s.store.AppendRPC(rec); err != nil && s.logger != nil {
			s.logger.Warn("append rpc log failed", "err", err, "action", rec.Action)
		}
		s.auditWG.Done()
	}
}
