package browser

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// addRouteTestTab attaches one more fake tab to a route test session and
// returns its tab context, which identifies the tab inside a Fetch stub.
func addRouteTestTab(t *testing.T, s *Session, tab target.ID) context.Context {
	t.Helper()
	tabCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		s.ClearRoutesForTarget(tab)
		cancel()
	})
	ready := make(chan struct{})
	close(ready)
	s.poolMu.Lock()
	s.tabPool[tab] = &tabPoolEntry{ctx: tabCtx, ready: ready}
	s.poolMu.Unlock()
	return tabCtx
}

// stubRouteFetchFunc replaces routeFetchApply with fn (nil patterns means Fetch.disable).
func stubRouteFetchFunc(t *testing.T, fn func(tabCtx, requestCtx context.Context, patterns []string) error) {
	t.Helper()
	orig := routeFetchApply
	routeFetchApply = func(tabCtx, requestCtx context.Context, patterns []*fetch.RequestPattern) error {
		var got []string
		for _, p := range patterns {
			got = append(got, p.URLPattern)
		}
		return fn(tabCtx, requestCtx, got)
	}
	t.Cleanup(func() { routeFetchApply = orig })
}

// A tab whose first Fetch.enable failed and whose compensating Fetch.disable
// hangs must not delay route updates of another, healthy tab.
func TestNetworkRouteBlockedCompensationDoesNotDelayOtherTab(t *testing.T) {
	const tabA, tabB target.ID = "tab-a", "tab-b"
	rule := NetworkRouteRule{URLPattern: "https://example.com/*", Abort: true}
	stubRouteListeners(t)
	s := newRouteTestSession(t, tabA)
	aCtx, _ := s.tabChromeCtx(tabA)
	addRouteTestTab(t, s, tabB)

	compensating := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	stubRouteFetchFunc(t, func(tabCtx, _ context.Context, patterns []string) error {
		if tabCtx != aCtx {
			return nil
		}
		if len(patterns) > 0 {
			return errFakeFetch
		}
		once.Do(func() { close(compensating) })
		<-release // an unresponsive tab: ignores every deadline
		return errFakeFetch
	})

	aDone := make(chan error, 1)
	go func() { aDone <- s.AppendNetworkRoute(tabA, rule) }()
	t.Cleanup(func() {
		close(release)
		<-aDone
	})
	select {
	case <-compensating:
	case <-time.After(2 * time.Second):
		t.Fatal("tab A did not reach the compensating Fetch.disable")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	bDone := make(chan error, 1)
	go func() { bDone <- s.AppendNetworkRouteContext(ctx, tabB, rule) }()
	select {
	case err := <-bDone:
		if err != nil {
			t.Fatalf("healthy tab B update failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("healthy tab B update waited behind tab A's blocked compensation")
	}
	if n := s.NetworkRouteCount(tabB); n != 1 {
		t.Fatalf("tab B has %d rule(s), want 1", n)
	}
	if n := s.NetworkRouteCount(tabA); n != 0 {
		t.Fatalf("tab A has %d rule(s), want 0", n)
	}
}

// Waiting behind another update of the same tab must end when the caller's
// context does, without touching the rules.
func TestNetworkRouteQueuedUpdateHonorsCancellation(t *testing.T) {
	const tab target.ID = "tab"
	stubRouteListeners(t)
	s := newRouteTestSession(t, tab)

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	stubRouteFetchFunc(t, func(context.Context, context.Context, []string) error {
		once.Do(func() {
			close(entered)
			<-release
		})
		return nil
	})

	firstDone := make(chan error, 1)
	go func() { firstDone <- s.AppendNetworkRoute(tab, NetworkRouteRule{URLPattern: "first.example"}) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first update did not reach the Fetch sync")
	}

	ctx, cancel := context.WithCancel(context.Background())
	queuedDone := make(chan error, 1)
	go func() {
		queuedDone <- s.AppendNetworkRouteContext(ctx, tab, NetworkRouteRule{URLPattern: "queued.example"})
	}()
	time.Sleep(50 * time.Millisecond) // let it queue; canceling earlier is equally valid
	cancel()
	queuedReturned := false
	select {
	case err := <-queuedDone:
		queuedReturned = true
		if !errors.Is(err, context.Canceled) {
			t.Errorf("queued update returned %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Error("queued update ignored cancellation while waiting for the tab's update lock")
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first update: %v", err)
	}
	if !queuedReturned {
		<-queuedDone
	}
	if got := routePatternsOf(s, tab); !slices.Equal(got, []string{"first.example"}) {
		t.Fatalf("rules = %v, want only the first update", got)
	}
}

// The compensating Fetch.disable has its own short deadline: a tab that never
// answers it cannot hold the caller (and the tab's update lock) for a full
// operation timeout.
func TestNetworkRouteCompensationHasShortDeadline(t *testing.T) {
	const tab target.ID = "tab"
	listeners := stubRouteListeners(t)
	s := newRouteTestSession(t, tab)

	release := make(chan struct{})
	stubRouteFetchFunc(t, func(_, requestCtx context.Context, patterns []string) error {
		if len(patterns) > 0 {
			return errFakeFetch
		}
		select {
		case <-requestCtx.Done():
			return requestCtx.Err()
		case <-release:
			return errFakeFetch
		}
	})

	start := time.Now()
	done := make(chan error, 1)
	go func() {
		done <- s.AppendNetworkRoute(tab, NetworkRouteRule{URLPattern: "https://example.com/*"})
	}()
	select {
	case err := <-done:
		if !errors.Is(err, errFakeFetch) {
			t.Errorf("append error = %v, want the Fetch.enable failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("compensating Fetch.disable had no deadline of its own")
		close(release)
		<-done
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("failed first enable took %v, want about the 1s compensation deadline", d)
	}
	if n := s.NetworkRouteCount(tab); n != 0 {
		t.Fatalf("failed append left %d rule(s)", n)
	}
	// Unconfirmed Fetch state: the listener stays to continue paused requests.
	if listeners.active() != 1 {
		t.Fatalf("active listeners = %d, want 1 while Fetch state is unconfirmed", listeners.active())
	}
}

// doneSignalCtx reports the first Done call, which is how a caller starts to
// wait (here: for the tab attach).
type doneSignalCtx struct {
	context.Context
	once    sync.Once
	waiting chan struct{}
}

func (c *doneSignalCtx) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

// An update that started before its target was cleared must fail as target
// gone even when the clear happens while it still waits for the tab attach: it
// may not adopt the post-clear generation and start a worker on the dead tab.
func TestNetworkRouteClearWhileAttachingDoesNotReviveTarget(t *testing.T) {
	const tab target.ID = "tab"
	rule := NetworkRouteRule{URLPattern: "https://example.com/*"}
	for _, tc := range []struct {
		name string
		fail bool
	}{
		{"fetch succeeds", false},
		{"fetch fails", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listeners := stubRouteListeners(t)
			remote := stubRouteFetch(t)
			if tc.fail {
				remote.setFail(func([]string) error { return errFakeFetch })
			}
			s := newSession(context.Background(), func() {})
			tabCtx, cancelTab := context.WithCancel(context.Background())
			t.Cleanup(func() {
				s.ClearRoutesForTarget(tab)
				cancelTab()
			})
			ready := make(chan struct{})
			s.tabPool = map[target.ID]*tabPoolEntry{tab: {ctx: tabCtx, ready: ready}}

			ctx := &doneSignalCtx{Context: context.Background(), waiting: make(chan struct{})}
			done := make(chan error, 1)
			go func() { done <- s.AppendNetworkRouteContext(ctx, tab, rule) }()
			select {
			case <-ctx.waiting:
			case <-time.After(2 * time.Second):
				t.Fatal("update did not start waiting for the attach")
			}
			// Target goes away: its tab context dies and its routes are cleared.
			cancelTab()
			s.ClearRoutesForTarget(tab)
			close(ready)

			var gone *CDPError
			if err := <-done; !errors.As(err, &gone) || gone.Kind != ErrorTargetGone {
				t.Errorf("update across a target clear returned %v, want target gone", err)
			}
			if _, calls := remote.state(); calls != 0 {
				t.Errorf("stale update sent %d Fetch call(s) to a cleared target", calls)
			}
			if n := s.NetworkRouteCount(tab); n != 0 {
				t.Errorf("stale update revived %d rule(s)", n)
			}
			if listeners.active() != 0 || s.routes().worker(tab) != nil {
				t.Errorf("stale update left listener/worker: active=%d worker=%v", listeners.active(), s.routes().worker(tab) != nil)
			}
		})
	}
}

// Clearing the last rules races with target cleanup: the update must report
// the lost target like the non-empty path does, not success.
func TestNetworkRouteClearAllOnClearedTargetReportsTargetGone(t *testing.T) {
	const tab target.ID = "tab"
	for _, tc := range []struct {
		name string
		call func(*Session) error
	}{
		{"remove all", func(s *Session) error { return s.RemoveNetworkRoutes(tab, "") }},
		{"remove last", func(s *Session) error { return s.RemoveNetworkRoutes(tab, "https://old.example/*") }},
		{"set empty", func(s *Session) error { return s.SetNetworkRoutes(tab, nil) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listeners := stubRouteListeners(t)
			remote := stubRouteFetch(t)
			s := newRouteTestSession(t, tab)
			if err := s.AppendNetworkRoute(tab, NetworkRouteRule{URLPattern: "https://old.example/*"}); err != nil {
				t.Fatal(err)
			}
			entered := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			remote.setFail(func([]string) error {
				once.Do(func() {
					close(entered)
					<-release
				})
				return nil
			})
			done := make(chan error, 1)
			go func() { done <- tc.call(s) }()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("update did not reach Fetch.disable")
			}
			s.ClearRoutesForTarget(tab)
			close(release)
			var gone *CDPError
			if err := <-done; !errors.As(err, &gone) || gone.Kind != ErrorTargetGone || gone.Op != "Fetch.disable" {
				t.Fatalf("clearing rules of a cleared target returned %v, want target gone from Fetch.disable", err)
			}
			if s.NetworkRouteCount(tab) != 0 || listeners.active() != 0 || s.routes().worker(tab) != nil {
				t.Fatal("cleared target kept rules, listener or worker")
			}
		})
	}
}

// routeCommands replaces routeCDP and records the CDP methods the route worker sends.
type routeCommands struct {
	mu      sync.Mutex
	methods []string
	changed chan struct{}
}

func (r *routeCommands) Execute(_ context.Context, method string, _, _ any) error {
	r.mu.Lock()
	r.methods = append(r.methods, method)
	r.mu.Unlock()
	select {
	case r.changed <- struct{}{}:
	default:
	}
	return nil
}

func stubRouteCommands(t *testing.T) *routeCommands {
	t.Helper()
	r := &routeCommands{changed: make(chan struct{}, 1)}
	orig := routeCDP
	routeCDP = func(ctx context.Context, actions ...chromedp.Action) error {
		ctx = cdp.WithExecutor(ctx, r)
		for _, a := range actions {
			if err := a.Do(ctx); err != nil {
				return err
			}
		}
		return nil
	}
	t.Cleanup(func() { routeCDP = orig })
	return r
}

func (r *routeCommands) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.methods)
}

// wait returns the recorded methods once there are at least n, or what was
// recorded when d elapsed.
func (r *routeCommands) wait(n int, d time.Duration) []string {
	deadline := time.After(d)
	for {
		if got := r.snapshot(); len(got) >= n {
			return got
		}
		select {
		case <-r.changed:
		case <-deadline:
			return r.snapshot()
		}
	}
}

// stubRouteCallbacks replaces listenTarget and hands out the registered callbacks.
func stubRouteCallbacks(t *testing.T) func() func(any) {
	t.Helper()
	var mu sync.Mutex
	var fns []func(any)
	orig := listenTarget
	listenTarget = func(_ context.Context, fn func(any)) {
		mu.Lock()
		fns = append(fns, fn)
		mu.Unlock()
	}
	t.Cleanup(func() { listenTarget = orig })
	return func() func(any) {
		mu.Lock()
		defer mu.Unlock()
		if len(fns) == 0 {
			return nil
		}
		return fns[len(fns)-1]
	}
}

// A request paused while Fetch.enable is in flight was paused by the candidate
// patterns. It must be answered from the rules the update ends with: the
// candidate once confirmed, the previous rules if the update fails. Answering
// it early from the old table would let a to-be-blocked request through.
func TestNetworkRoutePausedDuringUpdateUsesCommittedRules(t *testing.T) {
	const tab target.ID = "tab"
	const url = "https://example.com/a"
	abort := NetworkRouteRule{URLPattern: "https://example.com/*", Abort: true}
	mock := NetworkRouteRule{URLPattern: "https://example.com/*", MockBody: "{}"}
	for _, tc := range []struct {
		name   string
		seed   []NetworkRouteRule
		call   func(*Session) error
		fail   bool
		expect string
	}{
		{"first rule committed", nil, func(s *Session) error { return s.AppendNetworkRoute(tab, abort) }, false, "Fetch.failRequest"},
		{"replacement committed", []NetworkRouteRule{mock}, func(s *Session) error { return s.SetNetworkRoutes(tab, []NetworkRouteRule{abort}) }, false, "Fetch.failRequest"},
		{"replacement failed", []NetworkRouteRule{abort}, func(s *Session) error { return s.SetNetworkRoutes(tab, []NetworkRouteRule{mock}) }, true, "Fetch.failRequest"},
		{"clear failed", []NetworkRouteRule{abort}, func(s *Session) error { return s.RemoveNetworkRoutes(tab, "") }, true, "Fetch.failRequest"},
		{"clear committed", []NetworkRouteRule{abort}, func(s *Session) error { return s.RemoveNetworkRoutes(tab, "") }, false, "Fetch.continueRequest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			callback := stubRouteCallbacks(t)
			commands := stubRouteCommands(t)
			s := newRouteTestSession(t, tab)

			var inject bool
			var early []string
			stubRouteFetchFunc(t, func(context.Context, context.Context, []string) error {
				if !inject {
					return nil
				}
				fn := callback()
				if fn == nil {
					t.Error("no paused-request listener registered before the Fetch sync")
					return errFakeFetch
				}
				fn(&fetch.EventRequestPaused{RequestID: "req-1", Request: &network.Request{URL: url}})
				// Give a worker that does not wait for the commit time to answer.
				early = commands.wait(1, 150*time.Millisecond)
				if tc.fail {
					return errFakeFetch
				}
				return nil
			})
			if len(tc.seed) > 0 {
				if err := s.SetNetworkRoutes(tab, tc.seed); err != nil {
					t.Fatal(err)
				}
			}
			inject = true
			if err := tc.call(s); (err != nil) != tc.fail {
				t.Fatalf("update error = %v, want failure=%v", err, tc.fail)
			}
			if len(early) != 0 {
				t.Errorf("paused request answered with %v before the update finished", early)
			}
			if got := commands.wait(1, 2*time.Second); !slices.Equal(got, []string{tc.expect}) {
				t.Fatalf("paused request answered with %v, want [%s]", got, tc.expect)
			}
		})
	}
}

// Dispatch is paused per tab: while tab A's update is in flight, tab B's
// worker keeps answering paused requests.
func TestNetworkRouteUpdateDoesNotPauseOtherTabDispatch(t *testing.T) {
	const tabA, tabB target.ID = "tab-a", "tab-b"
	abort := NetworkRouteRule{URLPattern: "https://example.com/*", Abort: true}
	callback := stubRouteCallbacks(t)
	commands := stubRouteCommands(t)
	s := newRouteTestSession(t, tabA)
	aCtx, _ := s.tabChromeCtx(tabA)
	addRouteTestTab(t, s, tabB)

	entered := make(chan struct{})
	release := make(chan struct{})
	stubRouteFetchFunc(t, func(tabCtx, _ context.Context, _ []string) error {
		if tabCtx == aCtx {
			close(entered)
			<-release
		}
		return nil
	})
	if err := s.AppendNetworkRoute(tabB, abort); err != nil {
		t.Fatal(err)
	}
	bPaused := callback()
	aDone := make(chan error, 1)
	go func() { aDone <- s.AppendNetworkRoute(tabA, abort) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("tab A update did not reach the Fetch sync")
	}
	bPaused(&fetch.EventRequestPaused{RequestID: "req-b", Request: &network.Request{URL: "https://example.com/b"}})
	if got := commands.wait(1, 2*time.Second); !slices.Equal(got, []string{"Fetch.failRequest"}) {
		t.Errorf("tab B answered with %v during tab A's update, want [Fetch.failRequest]", got)
	}
	close(release)
	if err := <-aDone; err != nil {
		t.Fatalf("tab A update: %v", err)
	}
}

// Per-target update locks are reclaimed once nobody holds or waits for them,
// whether the update succeeded, failed or gave up waiting.
func TestNetworkRouteUpdateLocksAreReclaimed(t *testing.T) {
	const tab target.ID = "tab"
	stubRouteListeners(t)
	remote := stubRouteFetch(t)
	s := newRouteTestSession(t, tab)
	rule := NetworkRouteRule{URLPattern: "https://example.com/*"}

	if err := s.AppendNetworkRoute(tab, rule); err != nil {
		t.Fatal(err)
	}
	remote.setFail(func([]string) error { return errFakeFetch })
	if err := s.AppendNetworkRoute(tab, rule); err == nil {
		t.Fatal("append succeeded")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.AppendNetworkRouteContext(canceled, tab, rule); err == nil {
		t.Fatal("append with a canceled context succeeded")
	}
	s.ClearRoutesForTarget(tab)
	if err := s.RemoveNetworkRoutes(tab, ""); err == nil {
		t.Fatal("remove succeeded")
	}
	rs := s.routes()
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if n := len(rs.updates); n != 0 {
		t.Fatalf("%d update lock(s) left behind", n)
	}
}
