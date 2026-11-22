package browser

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/target"
)

// routeListeners replaces listenTarget and reports how many listeners are still active.
type routeListeners struct {
	mu   sync.Mutex
	ctxs []context.Context
}

func stubRouteListeners(t *testing.T) *routeListeners {
	t.Helper()
	l := &routeListeners{}
	orig := listenTarget
	listenTarget = func(ctx context.Context, _ func(any)) {
		l.mu.Lock()
		l.ctxs = append(l.ctxs, ctx)
		l.mu.Unlock()
	}
	t.Cleanup(func() { listenTarget = orig })
	return l
}

func (l *routeListeners) active() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, ctx := range l.ctxs {
		if ctx.Err() == nil {
			n++
		}
	}
	return n
}

// newRouteTestSession returns a session whose tab is already "attached" to a
// context that carries no chromedp target: every real Fetch call on it fails
// locally (chromedp.ErrInvalidContext) without touching a browser.
func newRouteTestSession(t *testing.T, tab target.ID) *Session {
	t.Helper()
	s := newSession(context.Background(), func() {})
	tabCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		s.ClearRoutesForTarget(tab)
		cancel()
	})
	ready := make(chan struct{})
	close(ready)
	s.tabPool = map[target.ID]*tabPoolEntry{tab: {ctx: tabCtx, ready: ready}}
	return s
}

func routePatternsOf(s *Session, tab target.ID) []string {
	rules, _ := s.routes().snapshot(tab)
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		out = append(out, r.URLPattern)
	}
	return out
}

func TestNetworkRouteCanceledAppendLeavesNoRule(t *testing.T) {
	const tab target.ID = "tab"
	stubRouteListeners(t)
	s := newRouteTestSession(t, tab)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range 3 {
		if err := s.AppendNetworkRouteContext(ctx, tab, NetworkRouteRule{URLPattern: "*track*", Abort: true}); err == nil {
			t.Fatal("append with a canceled context succeeded")
		}
		if n := s.NetworkRouteCount(tab); n != 0 {
			t.Fatalf("failed append left %d rule(s) registered", n)
		}
	}
}

func TestNetworkRouteFailedSyncKeepsRulesAndListener(t *testing.T) {
	const tab target.ID = "tab"
	a := NetworkRouteRule{URLPattern: "https://a.example/*", Abort: true}
	b := NetworkRouteRule{URLPattern: "https://b.example/*", MockBody: "{}"}
	c := NetworkRouteRule{URLPattern: "https://c.example/*"}
	want := []string{a.URLPattern, b.URLPattern}
	for _, tc := range []struct {
		name string
		call func(*Session) error
	}{
		{"append", func(s *Session) error { return s.AppendNetworkRoute(tab, c) }},
		{"set", func(s *Session) error { return s.SetNetworkRoutes(tab, []NetworkRouteRule{c}) }},
		{"set empty", func(s *Session) error { return s.SetNetworkRoutes(tab, nil) }},
		{"remove one", func(s *Session) error { return s.RemoveNetworkRoutes(tab, a.URLPattern) }},
		{"remove all", func(s *Session) error { return s.RemoveNetworkRoutes(tab, "") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listeners := stubRouteListeners(t)
			s := newRouteTestSession(t, tab)
			rs := s.routes()
			tabCtx, _ := s.tabChromeCtx(tab)
			rs.rules[tab] = []NetworkRouteRule{a, b}
			if !armRoute(rs, tabCtx, tab) || listeners.active() != 1 {
				t.Fatal("precondition: listener not armed")
			}
			for range 2 {
				if err := tc.call(s); err == nil {
					t.Fatal("precondition: expected the Fetch sync to fail")
				}
				if got := routePatternsOf(s, tab); !slices.Equal(got, want) {
					t.Errorf("failed sync changed rules to %v, want %v", got, want)
				}
				if got := listeners.active(); got != 1 {
					t.Fatalf("failed sync left %d active listener(s), want 1", got)
				}
			}
		})
	}
}

// fakeFetch replaces routeFetchApply with an in-memory browser: remote holds
// the patterns of the last call that succeeded (nil after Fetch.disable).
type fakeFetch struct {
	mu     sync.Mutex
	fail   func(patterns []string) error // nil patterns means Fetch.disable
	remote []string
	calls  int
}

func stubRouteFetch(t *testing.T) *fakeFetch {
	t.Helper()
	f := &fakeFetch{}
	orig := routeFetchApply
	routeFetchApply = func(_, _ context.Context, patterns []*fetch.RequestPattern) error {
		var got []string
		for _, p := range patterns {
			got = append(got, p.URLPattern)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls++
		if f.fail != nil {
			if err := f.fail(got); err != nil {
				return err
			}
		}
		f.remote = got
		return nil
	}
	t.Cleanup(func() { routeFetchApply = orig })
	return f
}

func (f *fakeFetch) setFail(fail func([]string) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = fail
}

func (f *fakeFetch) state() ([]string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.remote), f.calls
}

var errFakeFetch = errors.New("fake fetch failure")

func TestNetworkRouteFirstEnableFailureReleasesListener(t *testing.T) {
	const tab target.ID = "tab"
	rule := NetworkRouteRule{URLPattern: "https://example.com/*", Abort: true}
	listeners := stubRouteListeners(t)
	remote := stubRouteFetch(t)
	s := newRouteTestSession(t, tab)

	remote.setFail(func(patterns []string) error {
		if len(patterns) > 0 {
			return errFakeFetch
		}
		return nil
	})
	for range 2 {
		if err := s.AppendNetworkRoute(tab, rule); !errors.Is(err, errFakeFetch) {
			t.Fatalf("append error = %v, want the Fetch.enable failure", err)
		}
		if n := s.NetworkRouteCount(tab); n != 0 {
			t.Fatalf("failed first enable left %d rule(s)", n)
		}
		if listeners.active() != 0 || s.routes().worker(tab) != nil {
			t.Fatalf("failed first enable leaked listener/worker: active=%d worker=%v", listeners.active(), s.routes().worker(tab) != nil)
		}
	}

	remote.setFail(nil)
	if err := s.AppendNetworkRoute(tab, rule); err != nil {
		t.Fatalf("retry after failure: %v", err)
	}
	if got := routePatternsOf(s, tab); !slices.Equal(got, []string{rule.URLPattern}) {
		t.Fatalf("rules after retry = %v, want exactly one", got)
	}
	if got, _ := remote.state(); !slices.Equal(got, []string{rule.URLPattern}) {
		t.Fatalf("browser patterns after retry = %v", got)
	}
	if listeners.active() != 1 {
		t.Fatalf("active listeners after retry = %d, want 1", listeners.active())
	}

	if err := s.RemoveNetworkRoutes(tab, ""); err != nil {
		t.Fatalf("remove all: %v", err)
	}
	if got, _ := remote.state(); s.NetworkRouteCount(tab) != 0 || len(got) != 0 || listeners.active() != 0 {
		t.Fatalf("after remove all: rules=%d browser=%v active=%d", s.NetworkRouteCount(tab), got, listeners.active())
	}
}

// When neither Fetch.enable nor the follow-up Fetch.disable can be confirmed,
// interception may be on in the browser: the listener must stay so paused
// requests are continued, and target cleanup still releases everything.
func TestNetworkRouteUnconfirmedFirstEnableKeepsListener(t *testing.T) {
	const tab target.ID = "tab"
	listeners := stubRouteListeners(t)
	remote := stubRouteFetch(t)
	s := newRouteTestSession(t, tab)
	remote.setFail(func([]string) error { return errFakeFetch })

	if err := s.AppendNetworkRoute(tab, NetworkRouteRule{URLPattern: "https://example.com/*"}); err == nil {
		t.Fatal("append succeeded")
	}
	if n := s.NetworkRouteCount(tab); n != 0 {
		t.Fatalf("failed append left %d rule(s)", n)
	}
	if listeners.active() != 1 {
		t.Fatalf("active listeners = %d, want 1 while Fetch state is unconfirmed", listeners.active())
	}
	s.ClearRoutesForTarget(tab)
	if listeners.active() != 0 || s.routes().worker(tab) != nil {
		t.Fatal("target cleanup did not release the listener/worker")
	}
}

func TestNetworkRouteConcurrentUpdatesKeepEverySuccess(t *testing.T) {
	const tab target.ID = "tab"
	const n = 24
	listeners := stubRouteListeners(t)
	remote := stubRouteFetch(t)
	s := newRouteTestSession(t, tab)
	// The appended rule is the last pattern; odd ones are rejected by the browser.
	remote.setFail(func(patterns []string) error {
		if len(patterns) > 0 && strings.HasPrefix(patterns[len(patterns)-1], "fail-") {
			return errFakeFetch
		}
		return nil
	})

	var want []string
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		pattern := fmt.Sprintf("ok-%02d.example", i)
		if i%2 == 1 {
			pattern = fmt.Sprintf("fail-%02d.example", i)
		} else {
			want = append(want, pattern)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := s.AppendNetworkRoute(tab, NetworkRouteRule{URLPattern: pattern})
			if failed := strings.HasPrefix(pattern, "fail-"); failed != (err != nil) {
				t.Errorf("append %s: err = %v", pattern, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	got := routePatternsOf(s, tab)
	browser, _ := remote.state()
	if !slices.Equal(got, browser) {
		t.Fatalf("local rules %v differ from browser patterns %v", got, browser)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("rules = %v, want every successful append exactly once: %v", got, want)
	}
	if listeners.active() != 1 {
		t.Fatalf("active listeners = %d, want 1", listeners.active())
	}
}

func TestNetworkRouteInFlightUpdateDoesNotReviveClearedTarget(t *testing.T) {
	const tab target.ID = "tab"
	old := NetworkRouteRule{URLPattern: "https://old.example/*"}
	fresh := NetworkRouteRule{URLPattern: "https://new.example/*"}
	for _, tc := range []struct {
		name   string
		seeded bool
		call   func(*Session) error
	}{
		{"append first rule", false, func(s *Session) error { return s.AppendNetworkRoute(tab, fresh) }},
		{"append", true, func(s *Session) error { return s.AppendNetworkRoute(tab, fresh) }},
		{"set", true, func(s *Session) error { return s.SetNetworkRoutes(tab, []NetworkRouteRule{fresh}) }},
		{"remove", true, func(s *Session) error { return s.RemoveNetworkRoutes(tab, fresh.URLPattern) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listeners := stubRouteListeners(t)
			remote := stubRouteFetch(t)
			s := newRouteTestSession(t, tab)
			if tc.seeded {
				if err := s.AppendNetworkRoute(tab, old); err != nil {
					t.Fatal(err)
				}
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
			case <-time.After(time.Second):
				t.Fatal("update did not reach the Fetch sync")
			}
			s.ClearRoutesForTarget(tab)
			close(release)
			var gone *CDPError
			if err := <-done; !errors.As(err, &gone) || gone.Kind != ErrorTargetGone {
				t.Fatalf("update on a cleared target returned %v, want target gone", err)
			}
			if n := s.NetworkRouteCount(tab); n != 0 {
				t.Fatalf("in-flight update revived %d rule(s) on a cleared target", n)
			}
			if listeners.active() != 0 || s.routes().worker(tab) != nil {
				t.Fatal("in-flight update revived the listener/worker of a cleared target")
			}
		})
	}
}
