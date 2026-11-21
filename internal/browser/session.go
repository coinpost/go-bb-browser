package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// ErrCreatedTargetGone is wrapped by CreatePageTargetContext when the target it
// created was destroyed before Target.createTarget returned. The created
// target's id is returned alongside the error.
var ErrCreatedTargetGone = errors.New("target destroyed before createTarget returned")

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

// attachCancellation owns every cancellation of an attach context so Target can
// be cleared before chromedp's cancel watcher starts reading it.
type attachCancellation struct {
	mu          sync.Mutex
	cancelled   bool
	cancelChild context.CancelFunc
	stopParent  func() bool
}

func newAttachContext(parent context.Context, id target.ID) (context.Context, *attachCancellation) {
	ctx, cancel := chromedp.NewContext(context.WithoutCancel(parent), chromedp.WithTargetID(id))
	a := &attachCancellation{cancelChild: cancel}
	a.stopParent = context.AfterFunc(parent, func() {
		a.mu.Lock()
		a.cancelled = true
		a.mu.Unlock()
		cancel()
	})
	return ctx, a
}

func (a *attachCancellation) stop() {
	if a.stopParent != nil {
		a.stopParent()
	}
}

func (a *attachCancellation) cancel() {
	a.stop()
	a.mu.Lock()
	a.cancelled = true
	a.mu.Unlock()
	a.cancelChild()
}

// releaseFailedAttach releases a child context without closing the user's tab.
// The caller must own cleanup exclusively: once an entry has been removed by
// ForgetTarget or CloseTarget, their cancel watcher may already be using Target.
func releaseFailedAttach(ctx context.Context, cleanup *attachCancellation, ex cdp.Executor) {
	defer cleanup.cancel()
	cleanup.mu.Lock()
	if cleanup.cancelled {
		// Cancellation has handed Target to chromedp's cancel watcher.
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
			// chromedp's cancel watcher otherwise detaches AND closes this target.
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

	obsMu     sync.Mutex
	observers map[target.ID]*tabObserver

	// tabPool holds one chromedp child context per page/tab target. Cancelling that child
	// (as chromedp.NewContext's cancel does) runs CloseTarget — so we must NOT defer-cancel
	// after each operation. The daemon coordinator drops it only after authoritative removal.
	poolMu  sync.Mutex
	tabPool map[target.ID]*tabPoolEntry

	targetEvents chan TargetEvent
	targetDirty  atomic.Bool

	// Targets destroyed while a Target.createTarget is in flight.
	createMu          sync.Mutex
	creating          int
	destroyedInCreate map[target.ID]struct{}
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
}

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

// DefaultConnectTimeout bounds the connect handshake: resolving the browser
// websocket, dialling it and enabling target discovery.
const DefaultConnectTimeout = 30 * time.Second

// Connect opens a CDP session to an already-running Chrome instance.
// debuggerURL may be host:port, ws(s) URL, or http(s) URL (chromedp resolves to browser websocket).
func Connect(parent context.Context, debuggerURL string) (*Session, error) {
	return ConnectWithTimeout(parent, debuggerURL, DefaultConnectTimeout)
}

// ConnectWithTimeout is Connect with an explicit handshake deadline.
//
// The handshake ends when parent is cancelled or handshakeTimeout passes,
// whichever is first; neither outlives it. Once ConnectWithTimeout has
// returned a session, that session is independent of both: cancelling parent
// (daemon SIGTERM) must not tear chromedp down.
//
// The browser-level context never attaches to a page. chromedp closes the
// target of a context it cancels, and the only pages around at connect time are
// the user's; with no target there is nothing a failed or cancelled handshake
// could close. Pages are attached per tab, on first use (tabChromeCtxContext).
func ConnectWithTimeout(parent context.Context, debuggerURL string, handshakeTimeout time.Duration) (*Session, error) {
	if debuggerURL == "" {
		return nil, fmt.Errorf("debugger URL is empty")
	}
	if handshakeTimeout <= 0 {
		handshakeTimeout = DefaultConnectTimeout
	}
	// The contexts chromedp keeps are not derived from parent or the deadline:
	// they live as long as the session.
	allocCtx, allocCancel := chromedp.NewRemoteAllocator(context.WithoutCancel(parent), remoteAllocatorURL(debuggerURL))
	ctx, ctxCancel := chromedp.NewContext(allocCtx)
	cancel := func() {
		ctxCancel()
		allocCancel()
	}

	limit, stopLimit := context.WithTimeout(parent, handshakeTimeout)
	defer stopLimit()
	// Cancelling ctx is what interrupts every handshake step (HTTP lookup,
	// websocket dial, CDP call) and closes the websocket. It sends nothing to
	// Chrome: this context has no target.
	//
	// chromedp reads Context.Browser once ctx is cancelled, so the handshake
	// may store it only while the abort has not started; abortMu orders the
	// two.
	var (
		abortMu sync.Mutex
		aborted bool
	)
	stopAbort := context.AfterFunc(limit, func() {
		abortMu.Lock()
		aborted = true
		abortMu.Unlock()
		cancel()
	})
	publish := func(b *chromedp.Browser) bool {
		abortMu.Lock()
		defer abortMu.Unlock()
		if aborted {
			return false
		}
		chromedp.FromContext(ctx).Browser = b
		return true
	}

	s, err := handshake(ctx, cancel, publish)
	if !stopAbort() {
		// The abort has fired; whatever the handshake returned, the context
		// under it is gone.
		err = limit.Err()
		if parent.Err() == nil {
			err = fmt.Errorf("handshake did not finish within %s: %w", handshakeTimeout, err)
		}
		return nil, fmt.Errorf("cdp connect: %w%s", err, connectFailureHint(err))
	}
	if err != nil {
		cancel()
		return nil, fmt.Errorf("cdp connect: %w%s", err, connectFailureHint(err))
	}
	return s, nil
}

// handshake allocates the browser connection on ctx and subscribes to target
// lifecycle events on it. publish stores the allocated browser in the chromedp
// context and reports false when the handshake was aborted first.
func handshake(ctx context.Context, cancel context.CancelFunc, publish func(*chromedp.Browser) bool) (*Session, error) {
	c := chromedp.FromContext(ctx)
	// What chromedp.Run does first, minus attaching to (or creating) a page.
	b, err := c.Allocator.Allocate(ctx)
	if err != nil {
		return nil, err
	}
	if !publish(b) {
		// The allocator closes the websocket of a cancelled context itself.
		return nil, context.Canceled
	}

	s := newSession(ctx, cancel)
	// Listen before enabling discovery: Chrome announces the existing targets
	// as soon as it is enabled.
	s.listenTargetEvents()
	// Target events go to the CDP session that enabled discovery. The listener
	// above is on the browser session, so discovery has to be enabled there;
	// chromedp only enables it on the page sessions it attaches.
	if err := target.SetDiscoverTargets(true).Do(cdp.WithExecutor(ctx, b)); err != nil {
		return nil, fmt.Errorf("Target.setDiscoverTargets: %w", err)
	}
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
			s.noteDestroyedDuringCreate(e.TargetID)
		default:
			return
		}
		s.enqueueTargetEvent(out)
	})
}

// noteDestroyedDuringCreate runs on chromedp's read loop, so a destruction that
// precedes a Target.createTarget reply on the wire is recorded before
// CreatePageTargetContext sees that reply.
func (s *Session) noteDestroyedDuringCreate(id target.ID) {
	s.createMu.Lock()
	if s.creating > 0 {
		if s.destroyedInCreate == nil {
			s.destroyedInCreate = make(map[target.ID]struct{})
		}
		s.destroyedInCreate[id] = struct{}{}
	}
	s.createMu.Unlock()
}

func (s *Session) beginCreate() {
	s.createMu.Lock()
	s.creating++
	s.createMu.Unlock()
}

// endCreate reports whether id, the target a create returned ("" if none), was
// destroyed while creates were in flight.
func (s *Session) endCreate(id target.ID) (destroyed bool) {
	s.createMu.Lock()
	defer s.createMu.Unlock()
	s.creating--
	if id != "" {
		_, destroyed = s.destroyedInCreate[id]
		delete(s.destroyedInCreate, id)
	}
	if s.creating == 0 {
		s.destroyedInCreate = nil
	}
	return destroyed
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

// Context returns the browser-level chromedp context (lifetime matches the session).
func (s *Session) Context() context.Context {
	if s == nil {
		return context.Background()
	}
	return s.ctx
}

// Close drops references to CDP observers and the chromedp cancel func without invoking
// chromedp shutdown: chromedp.Cancel / child cancels run Target.CloseTarget on still-open
// tabs. The remote Chrome process should stay up; the OS closes the CDP TCP connection when
// this process exits.
func (s *Session) Close() {
	if s == nil {
		return
	}
	s.obsMu.Lock()
	for _, obs := range s.observers {
		obs.cleanup.stop()
	}
	s.observers = nil
	s.obsMu.Unlock()

	s.poolMu.Lock()
	for _, ent := range s.tabPool {
		ent.cleanup.stop()
	}
	s.tabPool = nil
	s.poolMu.Unlock()

	if s.cancel != nil {
		s.cancel = nil
	}
}

// ForgetTarget drops daemon-owned resources after Chrome has already removed a target.
// Cancelling a live chromedp child closes its tab, so this must only be called from an
// authoritative target-removal path.
func (s *Session) ForgetTarget(id target.ID) {
	if s == nil || id == "" {
		return
	}
	var cancel context.CancelFunc
	s.poolMu.Lock()
	if ent := s.tabPool[id]; ent != nil {
		delete(s.tabPool, id)
		cancel = ent.cleanup.cancel
	}
	s.poolMu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.obsMu.Lock()
	obs := s.observers[id]
	delete(s.observers, id)
	s.obsMu.Unlock()
	if obs != nil && obs.cleanup != nil {
		obs.cleanup.cancel()
	}
	s.ClearRoutesForTarget(id)
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
	ctx, cleanup := newAttachContext(s.ctx, tabID)
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
	err := chromedp.Run(ent.ctx)
	s.poolMu.Lock()
	release := false
	if s.tabPool[tabID] != ent {
		ent.err = &CDPError{Kind: ErrorTargetGone, Op: "Target.attachToTarget", Target: tabID, Err: fmt.Errorf("target removed while attaching")}
	} else if err != nil {
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
	s.beginCreate()
	id, err := params.Do(cdp.WithExecutor(opCtx, ex))
	destroyed := s.endCreate(id)
	if err != nil {
		return "", wrapCDPError("Target.createTarget", "", err)
	}
	if destroyed {
		// Chrome reported the new target destroyed before it answered. The id
		// is returned with the error so the caller can account for the target.
		return id, &CDPError{Kind: ErrorTargetGone, Op: "Target.createTarget", Target: id, Err: ErrCreatedTargetGone}
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
		// The target is already gone; chromedp's cleanup may repeat detach/close but
		// cannot accidentally close a different live tab.
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
		return nil, err
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
