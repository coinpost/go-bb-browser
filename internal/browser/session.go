package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/yiplee/go-bb-browser/internal/state"
	"github.com/yiplee/go-bb-browser/internal/timeout"
)

func errNilSession() error {
	return fmt.Errorf("browser session is nil")
}

func browserExecutor(ctx context.Context) cdp.Executor {
	c := chromedp.FromContext(ctx)
	if c == nil || c.Browser == nil {
		return nil
	}
	return c.Browser
}

// attachTimeout bounds how long callers wait for the first chromedp.Run of an
// attach context; Session.SetAttachTimeout overrides it per session.
var attachTimeout = timeout.Operation

// lateAttachGrace bounds how long an attach that outlived attachTimeout is
// still left running so that the browser's late reply can be cleaned up.
var lateAttachGrace = 5 * time.Minute

// attachCancellation owns every cancellation of an attach context.
//
// chromedp stores Context.Target from inside the first Run and its cancel
// watcher reads the field, unsynchronized, as soon as the child context is
// done. The child is therefore never cancelled while that Run is in flight:
// a cancellation arriving meanwhile ends only the Run (through a context
// derived for it), waits for it to return, and cancels the child afterwards.
//
// The watcher would also close the page by target id. A target id says nothing
// about which lifecycle of the page is meant (the id may already belong to a
// replacement page), and closing pages is never this type's business, so every
// cancellation first blanks the id on the child's Target: the watcher then
// only detaches the CDP session this child attached.
type attachCancellation struct {
	mu          sync.Mutex
	ctx         context.Context // the chromedp child; nil in tests without one
	timeout     time.Duration   // 0 means attachTimeout
	cancelled   bool
	cancelChild context.CancelFunc
	stopParent  func() bool

	// attachDone is non-nil while the first Run is in flight and is closed when
	// it has returned; abortAttach ends that Run without touching the child.
	abortAttach context.CancelFunc
	attachDone  chan struct{}
	// aborted is permanent: an abort that arrives before run keeps it from
	// starting the attach at all.
	aborted bool
}

func newAttachContext(parent context.Context, id target.ID) (context.Context, *attachCancellation) {
	ctx, cancel := chromedp.NewContext(context.WithoutCancel(parent), chromedp.WithTargetID(id))
	a := &attachCancellation{ctx: ctx, cancelChild: cancel}
	a.stopParent = context.AfterFunc(parent, func() { a.cancelWith(cancel) })
	return ctx, a
}

func (a *attachCancellation) stop() {
	if a.stopParent != nil {
		a.stopParent()
	}
}

// cancel detaches the child's CDP session and cancels the child context; it
// never closes the page. If the attach is still in flight, cancel first ends it
// and returns only after the child was cancelled too.
func (a *attachCancellation) cancel() {
	a.stop()
	a.cancelWith(a.cancelChild)
}

func (a *attachCancellation) cancelWith(cancelChild context.CancelFunc) {
	a.mu.Lock()
	a.cancelled = true
	abort, done := a.abortAttach, a.attachDone
	a.mu.Unlock()
	if done != nil {
		abort()
		<-done
	}
	// No attach can be writing Target any more, and the watcher reads it only
	// once the child is done, which happens below.
	a.mu.Lock()
	if a.ctx != nil {
		if c := chromedp.FromContext(a.ctx); c != nil && c.Target != nil && c.Target.TargetID != "" {
			c.Target.TargetID = ""
		}
	}
	a.mu.Unlock()
	cancelChild()
}

// abort ends an attach that is still in flight, or keeps one that has not
// started from ever starting, and leaves the child context alone; the
// initializer then decides what to do with it.
func (a *attachCancellation) abort() {
	a.mu.Lock()
	a.aborted = true
	abort, inFlight := a.abortAttach, a.attachDone != nil
	a.mu.Unlock()
	if inFlight {
		abort()
	}
}

// run performs the first chromedp.Run of ctx, the one that attaches to the
// target, and reports an error whenever the attach was cut short, even if
// chromedp had just completed it.
//
// The wait is bounded, the Run is not: when the bound expires run returns a
// timeout error and leaves the Run in flight, because ending it would drop the
// browser's late reply to Target.attachToTarget and with it the only handle on
// the remote session. The caller then owns that Run and must pass the
// cancellation to releaseFailedAttach, which waits for the late reply and
// detaches the session it announced.
//
// chromedp binds the target's event loop to the context of this Run, so the
// derived context lives as long as ctx unless the attach is aborted.
func (a *attachCancellation) run(ctx context.Context) error {
	runCtx, abort := context.WithCancel(ctx)
	done := make(chan struct{})
	a.mu.Lock()
	if a.cancelled || a.aborted {
		a.mu.Unlock()
		abort()
		return context.Canceled
	}
	a.abortAttach, a.attachDone = abort, done
	limit := a.timeout
	a.mu.Unlock()
	if limit <= 0 {
		limit = attachTimeout
	}

	var err error
	go func() {
		err = chromedp.Run(runCtx)
		a.mu.Lock()
		a.attachDone = nil
		a.mu.Unlock()
		close(done)
	}()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		return fmt.Errorf("attach timed out after %s: %w", limit, context.DeadlineExceeded)
	}

	a.mu.Lock()
	cut := a.cancelled || a.aborted
	a.mu.Unlock()
	if err == nil && cut {
		return context.Canceled
	}
	return err
}

// awaitLateAttach returns once no attach Run is in flight. A Run that outlived
// its bound is given lateAttachGrace to hear from the browser and is ended
// after that; a cancellation ends it at once.
func (a *attachCancellation) awaitLateAttach() {
	a.mu.Lock()
	abort, done := a.abortAttach, a.attachDone
	a.mu.Unlock()
	if done == nil {
		return
	}
	grace := time.NewTimer(lateAttachGrace)
	defer grace.Stop()
	select {
	case <-done:
	case <-grace.C:
		abort()
		<-done
	}
}

// releaseFailedAttach releases a child context without closing the user's tab.
// The caller must own cleanup exclusively: once an entry has been removed by
// ForgetTarget or CloseTarget, their cancel watcher may already be using Target.
// An attach that timed out is still in flight here; it is waited for first, so
// a session the browser reports late is detached like any other.
func releaseFailedAttach(ctx context.Context, cleanup *attachCancellation, ex cdp.Executor) {
	defer cleanup.cancel()
	cleanup.awaitLateAttach()
	cleanup.mu.Lock()
	if cleanup.cancelled {
		// Cancellation has handed Target to chromedp's cancel watcher, which
		// detaches the session.
		cleanup.mu.Unlock()
		return
	}
	c := chromedp.FromContext(ctx)
	var id target.SessionID
	if c != nil && c.Target != nil {
		id = c.Target.SessionID
	}
	cleanup.mu.Unlock()
	if c != nil {
		if id != "" && ex != nil {
			// Cleanup is bounded and can finish if the attach context expires.
			cleanupCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
			_ = target.DetachFromTarget().WithSessionID(id).Do(cdp.WithExecutor(cleanupCtx, ex))
			stop()
		}
		cleanup.mu.Lock()
		if !cleanup.cancelled {
			// The session is detached; leave chromedp's cancel watcher nothing to do.
			// If cancellation occurred during detach, the watcher may already be
			// reading Target, so leave it alone in that case.
			chromedp.FromContext(ctx).Target = nil
		}
		cleanup.mu.Unlock()
	}
}

// Session wraps a chromedp remote allocator + browser context for tab-scoped CDP APIs.
// Only NewRemoteAllocator is used — the daemon never launches Chrome.
type Session struct {
	ctx    context.Context
	cancel context.CancelFunc // cancels chromedp context then allocator

	// routeMu protects access to routeState; allocate it only in newSession.
	routeMu    sync.RWMutex
	routeState *routeState

	// closed is set by Close before it empties the tables below; attaches check
	// it under the table lock, so none can be added to a closed session.
	closed atomic.Bool

	obsMu     sync.Mutex
	observers map[target.ID]*tabObserver
	// obsRetiring holds one channel per observer that ForgetTarget or Close has
	// taken out of observers and is still retiring; it is closed when that is
	// complete. Both wait for every retirement they overlap, whoever started it.
	obsRetiring map[target.ID][]chan struct{}

	// attachLimit overrides attachTimeout when positive (nanoseconds).
	attachLimit atomic.Int64

	// tabPool holds one chromedp child context per page/tab target. Cancelling that child
	// (as chromedp.NewContext's cancel does) runs CloseTarget — so we must NOT defer-cancel
	// after each operation. The daemon coordinator drops it only after authoritative removal.
	poolMu  sync.Mutex
	tabPool map[target.ID]*tabPoolEntry

	targetEvents chan TargetEvent
	targetDirty  atomic.Bool
}

func newSession(ctx context.Context, cancel context.CancelFunc) *Session {
	return &Session{
		ctx:          ctx,
		cancel:       cancel,
		routeState:   newRouteState(),
		targetEvents: make(chan TargetEvent, 256),
	}
}

// TargetEvent is the small, non-blocking lifecycle stream consumed by daemon state.
type TargetEvent struct {
	Info      *target.Info
	Destroyed target.ID
}

type tabPoolEntry struct {
	ctx     context.Context
	cleanup *attachCancellation
	ready   chan struct{}
	err     error
	// sessionClosed (guarded by poolMu) hands the child to the initializer.
	sessionClosed bool
}

var errSessionClosed = errors.New("browser session closed")

func runCDP(ctx context.Context, actions ...chromedp.Action) error {
	return runCDPWithContext(ctx, context.Background(), actions...)
}

func runCDPWithContext(tabCtx, requestCtx context.Context, actions ...chromedp.Action) error {
	if requestCtx == nil {
		requestCtx = context.Background()
	}
	opCtx, cancel := context.WithTimeout(tabCtx, timeout.Operation)
	stop := context.AfterFunc(requestCtx, cancel)
	defer func() {
		stop()
		cancel()
	}()
	return chromedp.Run(opCtx, actions...)
}

// remoteAllocatorURL adapts debugger endpoints for chromedp.NewRemoteAllocator, which
// only accepts ws(s):// or http(s):// (bare host:port is not a valid URL for json/version).
func remoteAllocatorURL(debuggerURL string) string {
	s := strings.TrimSpace(debuggerURL)
	if s == "" || strings.Contains(s, "://") {
		return s
	}
	return "http://" + s
}

// httpDebuggerBase maps ws/wss debugger URLs to http(s) for the DevTools HTTP API (/json/list).
func httpDebuggerBase(debuggerURL string) string {
	s := remoteAllocatorURL(debuggerURL)
	switch {
	case strings.HasPrefix(s, "ws://"):
		return "http://" + strings.TrimPrefix(s, "ws://")
	case strings.HasPrefix(s, "wss://"):
		return "https://" + strings.TrimPrefix(s, "wss://")
	default:
		return s
	}
}

// firstExistingPageTabID returns the first page-like target id from GET /json/list, or ("", nil)
// if none exist (caller may let chromedp create a blank tab for INV-7).
func firstExistingPageTabID(ctx context.Context, debuggerBase string) (target.ID, error) {
	base := strings.TrimSpace(debuggerBase)
	if base == "" {
		return "", nil
	}
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, strings.TrimRight(base, "/")+"/json/list", nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return "", fmt.Errorf("GET /json/list: HTTP %d", resp.StatusCode)
	}
	var entries []struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return "", err
	}
	for _, e := range entries {
		switch e.Type {
		case "page", "tab":
			if e.ID != "" {
				return target.ID(e.ID), nil
			}
		}
	}
	return "", nil
}

// connectFailureHint adds short troubleshooting text for common attach failures.
func connectFailureHint(err error) string {
	if err == nil {
		return ""
	}
	m := strings.ToLower(err.Error())
	switch {
	case strings.Contains(m, "eof"),
		strings.Contains(m, "connection refused"),
		strings.Contains(m, "connection reset"),
		strings.Contains(m, "no such host"),
		strings.Contains(m, "timeout"),
		strings.Contains(m, "timed out"),
		strings.Contains(m, "wsarefuse"): // Windows WSAECONNREFUSED text
		return " — is anything listening on that port? Quit Chrome completely, start it with --remote-debugging-port=9222, then open http://127.0.0.1:9222/json/version and confirm you see JSON (webSocketDebuggerUrl)."
	default:
		return ""
	}
}

// Connect opens a CDP session to an already-running Chrome instance.
// debuggerURL may be host:port, ws(s) URL, or http(s) URL (chromedp resolves to browser websocket).
func Connect(parent context.Context, debuggerURL string) (*Session, error) {
	if debuggerURL == "" {
		return nil, fmt.Errorf("debugger URL is empty")
	}
	base := remoteAllocatorURL(debuggerURL)
	attachID, err := firstExistingPageTabID(parent, httpDebuggerBase(debuggerURL))
	if err != nil {
		return nil, fmt.Errorf("devtools list targets: %w%s", err, connectFailureHint(err))
	}
	// Do not tie the allocator to parent cancellation (e.g. daemon SIGTERM): cancelling that
	// context makes chromedp.RemoteAllocator run Cancel → CloseTarget on open tabs. The
	// debugging Chrome process should keep running; TCP drops when this process exits.
	allocCtx, allocCancel := chromedp.NewRemoteAllocator(context.WithoutCancel(parent), base)

	// First Run must use the long-lived chromedp ctx, not a child WithTimeout:
	// RemoteAllocator starts a goroutine on Allocate that waits on Run's ctx.Done()
	// then calls chromedp.Cancel — cancelling a timeout child after Connect returns
	// would tear down the browser session (symptom: later CDP ops return "context canceled").
	tryRun := func(targetID target.ID) (context.Context, context.CancelFunc, error) {
		var c context.Context
		var cancel context.CancelFunc
		if targetID != "" {
			// Attach to an existing page/tab; chromedp's default first Run otherwise
			// creates a new about:blank target (Target.createTarget).
			c, cancel = chromedp.NewContext(allocCtx, chromedp.WithTargetID(targetID))
		} else {
			c, cancel = chromedp.NewContext(allocCtx)
		}
		if err := chromedp.Run(c); err != nil {
			cancel()
			return nil, nil, err
		}
		return c, cancel, nil
	}

	ctx, ctxCancel, err := tryRun(attachID)
	if err != nil && attachID != "" && isStaleTargetErr(err) {
		// Some CDP servers (notably Obscura's /json/list) advertise a page-like
		// target that doesn't actually exist in their session map. Fall back to
		// letting chromedp create a fresh target via Target.createTarget so the
		// daemon can still attach.
		ctx, ctxCancel, err = tryRun("")
	}
	if err != nil {
		allocCancel()
		return nil, fmt.Errorf("cdp connect: %w%s", err, connectFailureHint(err))
	}
	cancel := func() {
		ctxCancel()
		allocCancel()
	}
	s := newSession(ctx, cancel)
	s.listenTargetEvents()
	return s, nil
}

func (s *Session) listenTargetEvents() {
	if s == nil {
		return
	}
	chromedp.ListenBrowser(s.ctx, func(ev any) {
		var out TargetEvent
		switch e := ev.(type) {
		case *target.EventTargetCreated:
			out.Info = e.TargetInfo
		case *target.EventTargetInfoChanged:
			out.Info = e.TargetInfo
		case *target.EventTargetDestroyed:
			out.Destroyed = e.TargetID
		default:
			return
		}
		s.enqueueTargetEvent(out)
	})
}

func (s *Session) enqueueTargetEvent(event TargetEvent) {
	select {
	case s.targetEvents <- event:
	default:
		s.targetDirty.Store(true)
	}
}

func (s *Session) TargetEvents() <-chan TargetEvent {
	if s == nil {
		return nil
	}
	return s.targetEvents
}

func (s *Session) ConsumeTargetDirty() bool {
	return s != nil && s.targetDirty.Swap(false)
}

func (s *Session) MarkTargetDirty() {
	if s != nil {
		s.targetDirty.Store(true)
	}
}

// isStaleTargetErr matches CDP errors raised when attaching to a target id that the
// remote DevTools host claims exists but its session bookkeeping cannot resolve.
// Obscura reports "No page" / "No page for session" via its catch-all -32601 wrapper;
// real Chrome reports variants like "No target with given id" / "Target closed".
func isStaleTargetErr(err error) bool {
	if err == nil {
		return false
	}
	m := strings.ToLower(err.Error())
	switch {
	case strings.Contains(m, "no page"),
		strings.Contains(m, "no target"),
		strings.Contains(m, "target not found"),
		strings.Contains(m, "target closed"),
		strings.Contains(m, "no session with given id"),
		strings.Contains(m, "no such target"):
		return true
	default:
		return false
	}
}

// Context returns the browser-level chromedp context (lifetime matches the session).
func (s *Session) Context() context.Context {
	if s == nil {
		return context.Background()
	}
	return s.ctx
}

// SetAttachTimeout bounds how long this session's callers wait for a tab or
// observer attach that the browser does not answer; d <= 0 restores the default.
func (s *Session) SetAttachTimeout(d time.Duration) {
	if s != nil {
		s.attachLimit.Store(int64(max(d, 0)))
	}
}

func (s *Session) newAttachContext(id target.ID) (context.Context, *attachCancellation) {
	ctx, cleanup := newAttachContext(s.ctx, id)
	cleanup.timeout = time.Duration(s.attachLimit.Load())
	return ctx, cleanup
}

// beginObserverRetirementLocked publishes the completion barrier of one
// observer that is leaving the table. The caller holds obsMu and removes the
// observer in the same critical section.
func (s *Session) beginObserverRetirementLocked(id target.ID) chan struct{} {
	done := make(chan struct{})
	if s.obsRetiring == nil {
		s.obsRetiring = make(map[target.ID][]chan struct{})
	}
	s.obsRetiring[id] = append(s.obsRetiring[id], done)
	return done
}

func (s *Session) endObserverRetirement(id target.ID, done chan struct{}) {
	s.obsMu.Lock()
	left := slices.DeleteFunc(s.obsRetiring[id], func(ch chan struct{}) bool { return ch == done })
	if len(left) == 0 {
		delete(s.obsRetiring, id)
	} else {
		s.obsRetiring[id] = left
	}
	s.obsMu.Unlock()
	close(done)
}

// Close drops references to CDP observers and the chromedp cancel func without invoking
// chromedp shutdown: cancelling the browser context would close tabs that are still open.
// The remote Chrome process should stay up; the OS closes the CDP TCP connection when
// this process exits.
//
// When Close returns, no observer of this session can record any more: it
// waits for every writer it retires and for every retirement ForgetTarget had
// already started, so the caller may clear or hand over the buffers.
func (s *Session) Close() {
	if s == nil {
		return
	}
	s.closed.Store(true)
	type retirement struct {
		id   target.ID
		obs  *tabObserver
		done chan struct{}
	}
	s.obsMu.Lock()
	// Retirements already under way belong to ForgetTarget (or another Close);
	// a missing table entry does not mean their writers have stopped.
	var pending []chan struct{}
	for _, list := range s.obsRetiring {
		pending = append(pending, list...)
	}
	retiring := make([]retirement, 0, len(s.observers))
	for id, obs := range s.observers {
		obs.sessionClosed = true
		retiring = append(retiring, retirement{id: id, obs: obs, done: s.beginObserverRetirementLocked(id)})
	}
	s.observers = nil
	s.obsMu.Unlock()
	for _, r := range retiring {
		r.obs.cleanup.stop()
		// An attach still in flight ends now and one not yet started never
		// starts; its initializer keeps ownership of the child and releases it
		// without closing the tab.
		r.obs.cleanup.abort()
		// Stop the writer too: an observer of a closed session must not keep
		// writing into buffers that a later session may own.
		r.obs.retire()
		s.endObserverRetirement(r.id, r.done)
	}
	for _, ch := range pending {
		<-ch
	}

	s.poolMu.Lock()
	entries := make([]*tabPoolEntry, 0, len(s.tabPool))
	for _, ent := range s.tabPool {
		ent.sessionClosed = true
		entries = append(entries, ent)
	}
	s.tabPool = nil
	s.poolMu.Unlock()
	for _, ent := range entries {
		ent.cleanup.stop()
		ent.cleanup.abort()
	}

	if s.cancel != nil {
		s.cancel = nil
	}
}

// ForgetTarget drops daemon-owned resources after Chrome has already removed a target.
// When it returns, every observer the target ever had on this session has
// stopped recording for good and its child context is released; a call that
// overlaps another release of the same target, or a Close retiring it, waits
// for that one as well.
// The children's CDP sessions are detached; the page itself is never closed
// from here, since the target id may already name a replacement page.
func (s *Session) ForgetTarget(id target.ID) {
	if s == nil || id == "" {
		return
	}
	s.poolMu.Lock()
	ent := s.tabPool[id]
	delete(s.tabPool, id)
	s.poolMu.Unlock()

	// Writes are disabled first; this also waits out an in-flight Record, so
	// the caller can clear the target's observation buffers afterwards.
	obs, done, pending := s.retireObserver(id)
	if ent != nil {
		ent.cleanup.cancel()
	}
	s.finishObserverRetirement(id, obs, done, pending)
	s.ClearRoutesForTarget(id)
}

// ForgetObservation is the observer half of ForgetTarget: it retires the
// target's observer under the same barriers and leaves the tab pool entry and
// the network routes alone. It is for an observer that turned out to have no
// owner while the target id itself may be in use.
func (s *Session) ForgetObservation(id target.ID) {
	if s == nil || id == "" {
		return
	}
	obs, done, pending := s.retireObserver(id)
	s.finishObserverRetirement(id, obs, done, pending)
}

// retireObserver takes id's observer out of the table, publishes its
// retirement and stops its writer, in-flight Record included. The caller must
// pass the results to finishObserverRetirement.
func (s *Session) retireObserver(id target.ID) (obs *tabObserver, done chan struct{}, pending []chan struct{}) {
	s.obsMu.Lock()
	obs = s.observers[id]
	delete(s.observers, id)
	// Retirements already under way are not finished just because the table
	// entry is gone: finishObserverRetirement waits for them, outside the lock.
	pending = slices.Clone(s.obsRetiring[id])
	if obs != nil {
		done = s.beginObserverRetirementLocked(id)
	}
	s.obsMu.Unlock()
	if obs != nil {
		obs.retire()
	}
	return obs, done, pending
}

func (s *Session) finishObserverRetirement(id target.ID, obs *tabObserver, done chan struct{}, pending []chan struct{}) {
	if obs != nil {
		if obs.cleanup != nil {
			// Safe during initialization: cancel ends a pending attach and takes
			// the child over only once the initializer's Run has returned.
			obs.cleanup.cancel()
		}
		s.endObserverRetirement(id, done)
	}
	for _, ch := range pending {
		<-ch
	}
}

// PingBrowser checks CDP connectivity with a lightweight Browser.getVersion call.
func (s *Session) PingBrowser() error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout.Operation)
	defer cancel()
	return s.PingBrowserContext(ctx)
}

// PingBrowserContext checks CDP connectivity and honors the caller's deadline.
func (s *Session) PingBrowserContext(ctx context.Context) error {
	if s == nil {
		return errNilSession()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ex := browserExecutor(s.ctx)
	if ex == nil {
		return fmt.Errorf("browser not available in context")
	}
	_, _, _, _, _, err := browser.GetVersion().Do(cdp.WithExecutor(ctx, ex))
	return err
}

// PageTargets returns Target API targets filtered to page-like tabs.
func (s *Session) PageTargets() ([]*target.Info, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout.Operation)
	defer cancel()
	return s.PageTargetsContext(ctx)
}

// PageTargetsContext lists page targets and honors the caller's deadline.
func (s *Session) PageTargetsContext(ctx context.Context) ([]*target.Info, error) {
	if s == nil {
		return nil, fmt.Errorf("browser session is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ex := browserExecutor(s.ctx)
	if ex == nil {
		return nil, fmt.Errorf("browser not available in context")
	}
	return pageTargetsWithExecutor(ctx, ex)
}

func pageTargetsWithExecutor(ctx context.Context, ex cdp.Executor) ([]*target.Info, error) {
	all, err := target.GetTargets().Do(cdp.WithExecutor(ctx, ex))
	if err != nil {
		return nil, wrapCDPError("Target.getTargets", "", err)
	}
	var pages []*target.Info
	for _, info := range all {
		if info == nil {
			continue
		}
		switch info.Type {
		case "page", "tab":
			pages = append(pages, info)
		default:
			continue
		}
	}
	return pages, nil
}

// DetectForegroundShort returns the short id of the unique page target whose
// document.visibilityState is "visible". If zero or more than one target
// qualifies (typical with multiple visible browser windows), it returns "", false.
func (s *Session) DetectForegroundShort(snaps []state.TabSnapshot) (string, bool) {
	return s.DetectForegroundShortContext(context.Background(), snaps)
}

func (s *Session) DetectForegroundShortContext(ctx context.Context, snaps []state.TabSnapshot) (string, bool) {
	if s == nil || len(snaps) == 0 {
		return "", false
	}
	return detectForegroundConcurrent(ctx, snaps, 16, 500*time.Millisecond, 5*time.Second, func(probe context.Context, sn state.TabSnapshot) (bool, error) {
		tabCtx, err := s.tabChromeCtxContext(probe, sn.TargetID)
		if err != nil {
			return false, err
		}
		var vis string
		err = runCDPWithContext(tabCtx, probe, chromedp.Evaluate(`document.visibilityState`, &vis))
		return vis == "visible", err
	})
}

type visibilityProbe func(context.Context, state.TabSnapshot) (bool, error)

func detectForegroundConcurrent(ctx context.Context, snaps []state.TabSnapshot, concurrency int, perTab, total time.Duration, probe visibilityProbe) (string, bool) {
	if len(snaps) == 0 || probe == nil {
		return "", false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if concurrency <= 0 {
		concurrency = 1
	}
	overall, cancel := context.WithTimeout(ctx, total)
	defer cancel()
	type result struct {
		short   string
		visible bool
		ok      bool
	}
	results := make(chan result, len(snaps))
	sem := make(chan struct{}, concurrency)
	for _, sn := range snaps {
		sn := sn
		if sn.TargetID == "" {
			results <- result{}
			continue
		}
		go func() {
			select {
			case sem <- struct{}{}:
			case <-overall.Done():
				results <- result{}
				return
			}
			defer func() { <-sem }()
			probeCtx, stop := context.WithTimeout(overall, perTab)
			defer stop()
			visible, err := probe(probeCtx, sn)
			if err != nil {
				results <- result{}
				return
			}
			results <- result{short: sn.ShortID, visible: visible, ok: true}
		}()
	}
	var picked string
	complete := 0
	for range len(snaps) {
		var r result
		select {
		case r = <-results:
		case <-overall.Done():
			return "", false
		}
		if r.ok {
			complete++
		}
		if !r.visible {
			continue
		}
		if picked != "" {
			return "", false
		}
		picked = r.short
	}
	if complete != len(snaps) || picked == "" {
		return "", false
	}
	return picked, true
}

// tabChromeCtx returns a chromedp context attached to tabID, creating and caching it on first use.
func (s *Session) tabChromeCtx(tabID target.ID) (context.Context, error) {
	return s.tabChromeCtxContext(context.Background(), tabID)
}

func (s *Session) tabChromeCtxContext(requestCtx context.Context, tabID target.ID) (context.Context, error) {
	if s == nil {
		return nil, fmt.Errorf("browser session is nil")
	}
	if tabID == "" {
		return nil, fmt.Errorf("empty target id")
	}
	if requestCtx == nil {
		requestCtx = context.Background()
	}
	s.poolMu.Lock()
	if s.closed.Load() {
		s.poolMu.Unlock()
		return nil, wrapCDPError("Target.attachToTarget", tabID, errSessionClosed)
	}
	if s.tabPool == nil {
		s.tabPool = make(map[target.ID]*tabPoolEntry)
	}
	if ent, ok := s.tabPool[tabID]; ok {
		s.poolMu.Unlock()
		select {
		case <-ent.ready:
			return ent.ctx, ent.err
		case <-requestCtx.Done():
			return nil, wrapCDPError("Target.attachToTarget", tabID, requestCtx.Err())
		}
	}
	ent := &tabPoolEntry{ready: make(chan struct{})}
	ctx, cleanup := s.newAttachContext(tabID)
	ent.ctx = ctx
	ent.cleanup = cleanup
	s.tabPool[tabID] = ent
	s.poolMu.Unlock()
	go s.initializeTabPoolEntry(tabID, ent)
	select {
	case <-ent.ready:
		return ent.ctx, ent.err
	case <-requestCtx.Done():
		return nil, wrapCDPError("Target.attachToTarget", tabID, requestCtx.Err())
	}
}

// The first chromedp.Run must use the long-lived target context itself. Running it
// on a request timeout child would bind Target.run to that child and kill the cached
// executor as soon as the request returned.
func (s *Session) initializeTabPoolEntry(tabID target.ID, ent *tabPoolEntry) {
	err := ent.cleanup.run(ent.ctx)
	s.poolMu.Lock()
	release := false
	switch {
	case ent.sessionClosed:
		// Close dropped the entry without cancelling it (that would close the
		// tab), so the child is still this initializer's to release.
		ent.err = wrapCDPError("Target.attachToTarget", tabID, errSessionClosed)
		release = true
	case s.tabPool[tabID] != ent:
		// Removed by ForgetTarget / CloseTarget, whose cancel owns the child.
		ent.err = &CDPError{Kind: ErrorTargetGone, Op: "Target.attachToTarget", Target: tabID, Err: fmt.Errorf("target removed while attaching")}
	case err != nil:
		delete(s.tabPool, tabID) // the next call can retry with a fresh context
		ent.err = wrapCDPError("Target.attachToTarget", tabID, err)
		release = true
	}
	s.poolMu.Unlock()
	close(ent.ready)
	if release {
		releaseFailedAttach(ent.ctx, ent.cleanup, browserExecutor(ent.ctx))
	}
}

// CreatePageTarget opens a new page target (INV-7: works when zero tabs exist).
// When silent is true, the tab is created in the background without focusing the browser window.
func (s *Session) CreatePageTarget(initialURL string, silent bool) (target.ID, error) {
	return s.CreatePageTargetContext(context.Background(), initialURL, silent)
}

func (s *Session) CreatePageTargetContext(ctx context.Context, initialURL string, silent bool) (target.ID, error) {
	if s == nil {
		return "", fmt.Errorf("browser session is nil")
	}
	ex := browserExecutor(s.ctx)
	if ex == nil {
		return "", fmt.Errorf("browser not available in context")
	}
	params := target.CreateTarget(initialURL)
	if silent {
		params = params.WithBackground(true).WithFocus(false)
	}
	opCtx, cancel := context.WithTimeout(s.ctx, timeout.Operation)
	stop := context.AfterFunc(ctx, cancel)
	defer cancel()
	defer stop()
	id, err := params.Do(cdp.WithExecutor(opCtx, ex))
	if err != nil {
		return "", wrapCDPError("Target.createTarget", "", err)
	}
	return id, nil
}

// CloseTarget closes a page target by CDP id.
func (s *Session) CloseTarget(id target.ID) error {
	return s.CloseTargetContext(context.Background(), id)
}

func (s *Session) CloseTargetContext(ctx context.Context, id target.ID) error {
	if s == nil {
		return fmt.Errorf("browser session is nil")
	}
	ex := browserExecutor(s.ctx)
	if ex == nil {
		return fmt.Errorf("browser not available in context")
	}
	opCtx, cancel := context.WithTimeout(s.ctx, timeout.Operation)
	stop := context.AfterFunc(ctx, cancel)
	defer cancel()
	defer stop()
	if err := target.CloseTarget(id).Do(cdp.WithExecutor(opCtx, ex)); err != nil {
		return wrapCDPError("Target.closeTarget", id, err)
	}
	var poolCancel context.CancelFunc
	s.poolMu.Lock()
	if ent, ok := s.tabPool[id]; ok {
		delete(s.tabPool, id)
		poolCancel = ent.cleanup.cancel
	}
	s.poolMu.Unlock()
	if poolCancel != nil {
		// The target is already gone; the child only detaches its own session.
		poolCancel()
	}
	s.ClearRoutesForTarget(id)
	return nil
}

// Navigate navigates the given page target to url.
func (s *Session) Navigate(tabID target.ID, url string) error {
	return s.NavigateContext(context.Background(), tabID, url)
}

func (s *Session) NavigateContext(ctx context.Context, tabID target.ID, url string) error {
	if s == nil {
		return fmt.Errorf("browser session is nil")
	}
	tabCtx, err := s.tabChromeCtxContext(ctx, tabID)
	if err != nil {
		return err
	}
	return wrapCDPError("Page.navigate", tabID, runCDPWithContext(tabCtx, ctx, chromedp.Navigate(url)))
}

// Reload performs a full navigation reload for the page target.
func (s *Session) Reload(tabID target.ID) error {
	return s.ReloadContext(context.Background(), tabID)
}

func (s *Session) ReloadContext(ctx context.Context, tabID target.ID) error {
	if s == nil {
		return fmt.Errorf("browser session is nil")
	}
	tabCtx, err := s.tabChromeCtxContext(ctx, tabID)
	if err != nil {
		return err
	}
	return wrapCDPError("Page.reload", tabID, runCDPWithContext(tabCtx, ctx, chromedp.Reload()))
}

// Screenshot captures the viewport; format is "png" (default) or "jpeg".
func (s *Session) Screenshot(tabID target.ID, format string) ([]byte, string, error) {
	return s.ScreenshotContext(context.Background(), tabID, format)
}

func (s *Session) ScreenshotContext(ctx context.Context, tabID target.ID, format string) ([]byte, string, error) {
	if s == nil {
		return nil, "", fmt.Errorf("browser session is nil")
	}
	tabCtx, err := s.tabChromeCtxContext(ctx, tabID)
	if err != nil {
		return nil, "", err
	}
	format = strings.ToLower(strings.TrimSpace(format))
	if format == "jpeg" || format == "jpg" {
		var buf []byte
		err := runCDPWithContext(tabCtx, ctx, chromedp.ActionFunc(func(ctx context.Context) error {
			var err error
			buf, err = page.CaptureScreenshot().
				WithFormat(page.CaptureScreenshotFormatJpeg).
				WithQuality(85).
				Do(ctx)
			return err
		}))
		return buf, "image/jpeg", wrapCDPError("Page.captureScreenshot", tabID, err)
	}
	var buf []byte
	err = runCDPWithContext(tabCtx, ctx, chromedp.CaptureScreenshot(&buf))
	return buf, "image/png", wrapCDPError("Page.captureScreenshot", tabID, err)
}

// Eval runs script in the page and returns JSON-marshalable result as RawMessage.
// awaitPromise is true so async expressions (including site adapters) resolve to
// JSON-serializable values instead of a Promise handle.
func (s *Session) Eval(tabID target.ID, script string) (json.RawMessage, error) {
	return s.EvalAwaitContext(context.Background(), tabID, script, true)
}

func (s *Session) EvalContext(ctx context.Context, tabID target.ID, script string) (json.RawMessage, error) {
	return s.EvalAwaitContext(ctx, tabID, script, true)
}

// EvalAwait runs script; when awaitPromise is true, resolves Promises (same as bb-browser fetch).
func (s *Session) EvalAwait(tabID target.ID, script string, awaitPromise bool) (json.RawMessage, error) {
	return s.EvalAwaitContext(context.Background(), tabID, script, awaitPromise)
}

func (s *Session) EvalAwaitContext(ctx context.Context, tabID target.ID, script string, awaitPromise bool) (json.RawMessage, error) {
	if s == nil {
		return nil, errNilSession()
	}
	tabCtx, err := s.tabChromeCtxContext(ctx, tabID)
	if err != nil {
		return nil, wrapCDPError("Runtime.evaluate", tabID, err)
	}
	var raw []byte
	err = runCDPWithContext(tabCtx, ctx, chromedp.Evaluate(script, &raw, func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
		return p.WithAwaitPromise(awaitPromise).WithReturnByValue(true)
	}))
	if err != nil {
		return nil, wrapCDPError("Runtime.evaluate", tabID, err)
	}
	return json.RawMessage(raw), nil
}

// Click performs a click on the first node matching selector.
func (s *Session) Click(tabID target.ID, selector string) error {
	return s.ClickContext(context.Background(), tabID, selector)
}

func (s *Session) ClickContext(ctx context.Context, tabID target.ID, selector string) error {
	if s == nil {
		return fmt.Errorf("browser session is nil")
	}
	tabCtx, err := s.tabChromeCtxContext(ctx, tabID)
	if err != nil {
		return err
	}
	return wrapCDPError("DOM.click", tabID, runCDPWithContext(tabCtx, ctx, chromedp.Click(selector, chromedp.ByQuery)))
}

// Fill sets the value of an input/textarea (see chromedp.SetValue).
func (s *Session) Fill(tabID target.ID, selector, text string) error {
	return s.FillContext(context.Background(), tabID, selector, text)
}

func (s *Session) FillContext(ctx context.Context, tabID target.ID, selector, text string) error {
	if s == nil {
		return fmt.Errorf("browser session is nil")
	}
	tabCtx, err := s.tabChromeCtxContext(ctx, tabID)
	if err != nil {
		return err
	}
	return wrapCDPError("DOM.fill", tabID, runCDPWithContext(tabCtx, ctx, chromedp.SetValue(selector, text, chromedp.ByQuery)))
}
