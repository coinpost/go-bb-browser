package browser

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"

	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// NetworkRouteRule is one URL interception rule for CDP Fetch (per tab).
type NetworkRouteRule struct {
	URLPattern  string // CDP wildcard pattern (see Fetch RequestPattern)
	Abort       bool
	MockBody    string // UTF-8 response body when not aborting
	ContentType string // e.g. application/json
	Status      int    // HTTP status for mock (default 200)
}

// routeCDP is the worker's CDP entry. Tests replace it; the listen callback must not call it.
var routeCDP = runCDP

// listenTarget is chromedp.ListenTarget. Tests replace it to count listeners.
var listenTarget = chromedp.ListenTarget

type routeState struct {
	mu    sync.Mutex
	cdpMu sync.Mutex

	rules   map[target.ID][]NetworkRouteRule
	workers map[target.ID]*routeWorker
	gen     map[target.ID]uint64
}

type routeWorker struct {
	cancel context.CancelFunc
	wake   chan struct{}

	mu           sync.Mutex
	queue        []*fetch.EventRequestPaused
	listening    bool
	listenCancel context.CancelFunc
}

func newRouteState() *routeState {
	return &routeState{
		rules:   make(map[target.ID][]NetworkRouteRule),
		workers: make(map[target.ID]*routeWorker),
		gen:     make(map[target.ID]uint64),
	}
}

func (s *Session) routes() *routeState {
	s.routeMu.RLock()
	defer s.routeMu.RUnlock()
	return s.routeState
}

// routeFetchApply sends a tab's Fetch interception config: Fetch.enable with the
// patterns, or Fetch.disable when there are none. Tests replace it.
var routeFetchApply = func(tabCtx, requestCtx context.Context, patterns []*fetch.RequestPattern) error {
	return runCDPWithContext(tabCtx, requestCtx, chromedp.ActionFunc(func(c context.Context) error {
		if len(patterns) == 0 {
			return fetch.Disable().Do(c)
		}
		return fetch.Enable().WithPatterns(patterns).Do(c)
	}))
}

// SetNetworkRoutes replaces all Fetch interception rules for a tab and syncs CDP.
func (s *Session) SetNetworkRoutes(tabID target.ID, rules []NetworkRouteRule) error {
	return s.SetNetworkRoutesContext(context.Background(), tabID, rules)
}

func (s *Session) SetNetworkRoutesContext(ctx context.Context, tabID target.ID, rules []NetworkRouteRule) error {
	return s.updateRoutes(ctx, tabID, func([]NetworkRouteRule) []NetworkRouteRule {
		return append([]NetworkRouteRule(nil), rules...)
	})
}

// AppendNetworkRoute adds one rule for a tab (existing rules preserved).
func (s *Session) AppendNetworkRoute(tabID target.ID, rule NetworkRouteRule) error {
	return s.AppendNetworkRouteContext(context.Background(), tabID, rule)
}

func (s *Session) AppendNetworkRouteContext(ctx context.Context, tabID target.ID, rule NetworkRouteRule) error {
	return s.updateRoutes(ctx, tabID, func(cur []NetworkRouteRule) []NetworkRouteRule {
		return append(cur, rule)
	})
}

// RemoveNetworkRoutes removes rules whose URLPattern matches pattern; empty pattern clears all rules for the tab.
func (s *Session) RemoveNetworkRoutes(tabID target.ID, urlPattern string) error {
	return s.RemoveNetworkRoutesContext(context.Background(), tabID, urlPattern)
}

func (s *Session) RemoveNetworkRoutesContext(ctx context.Context, tabID target.ID, urlPattern string) error {
	pat := strings.TrimSpace(urlPattern)
	return s.updateRoutes(ctx, tabID, func(cur []NetworkRouteRule) []NetworkRouteRule {
		if pat == "" {
			return nil
		}
		return slices.DeleteFunc(cur, func(r NetworkRouteRule) bool { return r.URLPattern == pat })
	})
}

// NetworkRouteCount returns how many interception rules are active for a tab.
func (s *Session) NetworkRouteCount(tabID target.ID) int {
	if s == nil {
		return 0
	}
	s.routeMu.RLock()
	defer s.routeMu.RUnlock()
	rs := s.routeState
	if rs == nil {
		return 0
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return len(rs.rules[tabID])
}

// updateRoutes applies one rule change as a transaction: the candidate rules
// are sent to the browser first and stored only once that sync succeeded, so a
// failed call leaves the rules (and the listener serving them) as they were.
// cdpMu serializes whole updates, so concurrent callers build on each other's
// committed result; rs.mu is never held across CDP.
//
// A sync that times out or loses its connection may still have been applied by
// the browser. The local table then keeps the last confirmed rules; the next
// successful update sends the full pattern list and reconciles the browser.
func (s *Session) updateRoutes(ctx context.Context, tabID target.ID, mutate func(cur []NetworkRouteRule) []NetworkRouteRule) error {
	if s == nil {
		return errNilSession()
	}
	rs := s.routes()
	if rs == nil {
		return fmt.Errorf("browser session route state is not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	tabCtx, err := s.tabChromeCtxContext(ctx, tabID)
	if err != nil {
		return wrapCDPError("Fetch.enable", tabID, err)
	}
	rs.cdpMu.Lock()
	defer rs.cdpMu.Unlock()

	cur, gen := rs.snapshot(tabID)
	next := mutate(cur) // cur is a private copy
	if len(next) == 0 {
		if err := ctx.Err(); err != nil {
			return wrapCDPError("Fetch.disable", tabID, err)
		}
		// Paused requests still need the listener until the browser confirms
		// that interception is off.
		if err := routeFetchApply(tabCtx, ctx, nil); err != nil {
			return wrapCDPError("Fetch.disable", tabID, err)
		}
		if rs.commit(tabID, gen, nil) {
			if w := rs.worker(tabID); w != nil {
				w.stopListening()
			}
		}
		return nil
	}
	if err := ctx.Err(); err != nil {
		return wrapCDPError("Fetch.enable", tabID, err)
	}

	// The listener must be in place before Fetch.enable; until commit it keeps
	// answering paused requests from the previously confirmed rules.
	w := rs.ensureWorker(tabID, tabCtx, gen)
	if w == nil || !w.startListening(tabCtx, rs, tabID, gen) {
		return routeTargetGone(tabID)
	}
	if patterns := routePatterns(next); len(patterns) > 0 {
		if err := routeFetchApply(tabCtx, ctx, patterns); err != nil {
			if len(cur) == 0 {
				rs.abandonFirstEnable(tabCtx, tabID, w)
			}
			return wrapCDPError("Fetch.enable", tabID, err)
		}
	}
	if !rs.commit(tabID, gen, next) {
		return routeTargetGone(tabID)
	}
	return nil
}

func routeTargetGone(tabID target.ID) error {
	return &CDPError{Kind: ErrorTargetGone, Op: "Fetch.enable", Target: tabID, Err: fmt.Errorf("target removed while updating routes")}
}

// commit stores rules unless the target was cleared since gen was read, so an
// in-flight update cannot bring a removed target's rules back.
func (rs *routeState) commit(tabID target.ID, gen uint64, rules []NetworkRouteRule) bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.gen[tabID] != gen {
		return false
	}
	if len(rules) == 0 {
		delete(rs.rules, tabID)
	} else {
		rs.rules[tabID] = rules
	}
	return true
}

// abandonFirstEnable releases the worker and listener of a tab whose first
// Fetch.enable failed. The failed call may still have reached the browser, so
// interception is switched off first; if that cannot be confirmed either, the
// listener stays (with no rules it continues every paused request) until a
// later update or target cleanup.
func (rs *routeState) abandonFirstEnable(tabCtx context.Context, tabID target.ID, w *routeWorker) {
	if routeFetchApply(tabCtx, context.Background(), nil) != nil {
		return
	}
	rs.mu.Lock()
	owned := rs.workers[tabID] == w
	if owned {
		delete(rs.workers, tabID)
	}
	rs.mu.Unlock()
	w.stopListening()
	if owned {
		w.cancel()
	}
}

func routePatterns(rules []NetworkRouteRule) []*fetch.RequestPattern {
	patterns := make([]*fetch.RequestPattern, 0, len(rules))
	for _, r := range rules {
		p := strings.TrimSpace(r.URLPattern)
		if p == "" {
			continue
		}
		patterns = append(patterns, &fetch.RequestPattern{URLPattern: p, RequestStage: fetch.RequestStageRequest})
	}
	return patterns
}

func (rs *routeState) snapshot(tabID target.ID) ([]NetworkRouteRule, uint64) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	var out []NetworkRouteRule
	if src := rs.rules[tabID]; len(src) > 0 {
		out = make([]NetworkRouteRule, len(src))
		copy(out, src)
	}
	return out, rs.gen[tabID]
}

func (rs *routeState) generation(tabID target.ID) uint64 {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.gen[tabID]
}

func (rs *routeState) worker(tabID target.ID) *routeWorker {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.workers[tabID]
}

func (rs *routeState) ensureWorker(tabID target.ID, tabCtx context.Context, gen uint64) *routeWorker {
	rs.mu.Lock()
	if rs.gen[tabID] != gen {
		rs.mu.Unlock()
		return nil
	}
	w := rs.workers[tabID]
	if w != nil {
		rs.mu.Unlock()
		return w
	}
	ctx, cancel := context.WithCancel(tabCtx)
	w = &routeWorker{
		cancel: cancel,
		wake:   make(chan struct{}, 1),
	}
	if rs.workers == nil {
		rs.workers = make(map[target.ID]*routeWorker)
	}
	rs.workers[tabID] = w
	rs.mu.Unlock()
	go w.loop(ctx, rs, tabID, tabCtx)
	return w
}

func (w *routeWorker) startListening(tabCtx context.Context, rs *routeState, tabID target.ID, gen uint64) bool {
	live := func() bool { return rs.generation(tabID) == gen }
	if !live() {
		return false
	}
	w.mu.Lock()
	if w.listening {
		w.mu.Unlock()
		if live() {
			return true
		}
		w.stopListening()
		return false
	}
	ctx, cancel := context.WithCancel(tabCtx)
	w.listenCancel = cancel
	w.listening = true
	w.mu.Unlock()
	if !live() {
		w.stopListening()
		return false
	}
	listenTarget(ctx, w.onPaused)
	if !live() {
		w.stopListening()
		return false
	}
	return true
}

func (w *routeWorker) stopListening() {
	w.mu.Lock()
	cancel := w.listenCancel
	w.listenCancel = nil
	w.listening = false
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (w *routeWorker) onPaused(ev any) {
	e, ok := ev.(*fetch.EventRequestPaused)
	if !ok || e == nil || e.Request == nil {
		return
	}
	w.mu.Lock()
	w.queue = append(w.queue, e)
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *routeWorker) pop() *fetch.EventRequestPaused {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.queue) == 0 {
		return nil
	}
	ev := w.queue[0]
	w.queue[0] = nil
	w.queue = w.queue[1:]
	if len(w.queue) == 0 {
		w.queue = nil
	}
	return ev
}

func (w *routeWorker) loop(ctx context.Context, rs *routeState, tabID target.ID, tabCtx context.Context) {
	defer w.stopListening()
	for {
		w.dispatchAll(rs, tabID, tabCtx)
		select {
		case <-ctx.Done():
			w.dispatchAll(rs, tabID, tabCtx)
			return
		case <-w.wake:
		}
	}
}

func (w *routeWorker) dispatchAll(rs *routeState, tabID target.ID, tabCtx context.Context) {
	for {
		ev := w.pop()
		if ev == nil {
			return
		}
		rules, _ := rs.snapshot(tabID)
		applyPaused(tabCtx, rules, ev)
	}
}

func applyPaused(tabCtx context.Context, rules []NetworkRouteRule, e *fetch.EventRequestPaused) {
	if e == nil || e.Request == nil {
		return
	}
	if e.ResponseStatusCode != 0 || e.ResponseErrorReason != "" {
		continuePaused(tabCtx, e.RequestID)
		return
	}
	matched, ok := matchRoute(rules, e.Request.URL)
	if !ok {
		continuePaused(tabCtx, e.RequestID)
		return
	}
	if matched.Abort {
		_ = routeCDP(tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {
			return fetch.FailRequest(e.RequestID, network.ErrorReasonBlockedByClient).Do(ctx)
		}))
		return
	}
	ct := strings.TrimSpace(matched.ContentType)
	if ct == "" {
		ct = "application/json"
	}
	st := matched.Status
	if st <= 0 {
		st = 200
	}
	b64 := base64.StdEncoding.EncodeToString([]byte(matched.MockBody))
	headers := []*fetch.HeaderEntry{{Name: "Content-Type", Value: ct}}
	_ = routeCDP(tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		return fetch.FulfillRequest(e.RequestID, int64(st)).
			WithResponseHeaders(headers).
			WithBody(b64).
			Do(ctx)
	}))
}

func continuePaused(tabCtx context.Context, id fetch.RequestID) {
	_ = routeCDP(tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		return fetch.ContinueRequest(id).Do(ctx)
	}))
}

func matchRoute(rules []NetworkRouteRule, reqURL string) (NetworkRouteRule, bool) {
	for i := range rules {
		if urlMatch(rules[i].URLPattern, reqURL) {
			return rules[i], true
		}
	}
	return NetworkRouteRule{}, false
}

func urlMatch(pattern, reqURL string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return false
	}
	if strings.Contains(pattern, "://") || strings.HasPrefix(pattern, "*") {
		return wildcardMatch(pattern, reqURL)
	}
	u, err := url.Parse(reqURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	path := u.Path
	if path == "" {
		path = "/"
	}
	patLower := strings.ToLower(pattern)
	if strings.Contains(patLower, "/") {
		full := host + path
		return wildcardMatch(strings.ToLower(pattern), strings.ToLower(full))
	}
	return host == patLower || strings.HasSuffix(host, "."+patLower)
}

func wildcardMatch(pattern, s string) bool {
	p := pattern
	str := s
	for len(p) > 0 {
		if p[0] == '*' {
			if len(p) == 1 {
				return true
			}
			for i := 0; i <= len(str); i++ {
				if wildcardMatch(p[1:], str[i:]) {
					return true
				}
			}
			return false
		}
		if len(str) == 0 {
			return false
		}
		if p[0] == '?' {
			p, str = p[1:], str[1:]
			continue
		}
		if p[0] != str[0] {
			return false
		}
		p, str = p[1:], str[1:]
	}
	return len(str) == 0
}

// ClearRoutesForTarget removes routing rules when a tab target disappears.
func (s *Session) ClearRoutesForTarget(tabID target.ID) {
	if s == nil {
		return
	}
	rs := s.routes()
	if rs == nil {
		return
	}
	rs.mu.Lock()
	delete(rs.rules, tabID)
	if rs.gen == nil {
		rs.gen = make(map[target.ID]uint64)
	}
	rs.gen[tabID]++
	w := rs.workers[tabID]
	delete(rs.workers, tabID)
	rs.mu.Unlock()
	if w != nil {
		w.stopListening()
		w.cancel()
	}
}
