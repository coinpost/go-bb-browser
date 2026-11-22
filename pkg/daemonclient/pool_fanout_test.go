package daemonclient

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/yiplee/go-bb-browser/pkg/protocol"
)

// Regressions for tab_list / tab_focus fan-out: a backend that accepts the
// request and never answers must not stop healthy backends from being asked.

// hangRPC makes the daemon accept /v1 requests without ever answering.
func (st *fakeDaemon) hangRPC(t *testing.T) {
	t.Helper()
	st.hang = make(chan struct{})
	t.Cleanup(func() { close(st.hang) }) // runs before srv.Close
}

// within fails the test instead of letting a hung pool call block it forever.
func within[R any](t *testing.T, limit time.Duration, fn func() (R, error)) (R, error) {
	t.Helper()
	type out struct {
		val R
		err error
	}
	ch := make(chan out, 1)
	go func() {
		v, err := fn()
		ch <- out{v, err}
	}()
	select {
	case o := <-ch:
		return o.val, o.err
	case <-time.After(limit):
		t.Fatalf("pool call still blocked after %v", limit)
		panic("unreachable")
	}
}

func waitInFlightZero(t *testing.T, p *Pool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		busy := false
		for i := range p.inFlight {
			if p.inFlight[i].Load() != 0 {
				busy = true
			}
		}
		if !busy {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("in-flight counters did not return to zero")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func listIDs(list protocol.TabListResult) []string {
	ids := make([]string, 0, len(list.Tabs))
	for _, item := range list.Tabs {
		ids = append(ids, item.Tab)
	}
	return ids
}

func sameIDs(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// hungAndHealthy returns a pool whose backend 0 never answers /v1 and whose
// backend 1 lists/focuses the daemon-native tab "tb".
func hungAndHealthy(t *testing.T) (*Pool, *fakeDaemon, *fakeDaemon) {
	t.Helper()
	a, sa := newFakeDaemon(t, "a")
	b, sb := newFakeDaemon(t, "b")
	sa.hangRPC(t)
	sa.listTab = "ta"
	sb.listTab = "tb"
	p, err := NewPool(NewClient(a.URL), NewClient(b.URL))
	if err != nil {
		t.Fatal(err)
	}
	return p, sa, sb
}

func TestPool_tabListHungBackendDoesNotBlockHealthy(t *testing.T) {
	p, _, sb := hungAndHealthy(t)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	list, err := within(t, 5*time.Second, func() (protocol.TabListResult, error) {
		return p.TabList(ctx, protocol.TabListParams{})
	})
	if err != nil {
		t.Fatalf("TabList with one hung backend: %v (healthy backend got %d tab_list calls)",
			err, sb.methodCount(protocol.MethodTabList))
	}
	if !sameIDs(listIDs(list), "1:tb") {
		t.Fatalf("want only backend 1 tabs, got %v", list.Tabs)
	}
	if list.Tab != "1:tb" || list.Focus != "1:tb" || list.Seq == 0 {
		t.Fatalf("tab %q focus %q seq %d", list.Tab, list.Focus, list.Seq)
	}
	if n := sb.methodCount(protocol.MethodTabList); n != 1 {
		t.Fatalf("healthy backend tab_list calls = %d, want 1", n)
	}
	if _, ok := p.ClientForTab("1:tb"); !ok {
		t.Fatal("listed tab must be bound")
	}
	if _, ok := p.ClientForTab("0:ta"); ok {
		t.Fatal("hung backend must not contribute bindings")
	}
	waitInFlightZero(t, p)
}

func TestPool_tabFocusHungBackendDoesNotBlockHealthy(t *testing.T) {
	p, _, sb := hungAndHealthy(t)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	focus, err := within(t, 5*time.Second, func() (protocol.TabFocusResult, error) {
		return p.TabFocus(ctx, protocol.TabFocusParams{})
	})
	if err != nil {
		t.Fatalf("TabFocus with one hung backend: %v (healthy backend got %d tab_focus calls)",
			err, sb.methodCount(protocol.MethodTabFocus))
	}
	if focus.Tab != "1:tb" || focus.Focus != "1:tb" {
		t.Fatalf("want backend 1 focus, got tab=%q focus=%q", focus.Tab, focus.Focus)
	}
	if _, ok := p.ClientForTab("1:tb"); !ok {
		t.Fatal("focused tab must be bound")
	}
	waitInFlightZero(t, p)
}

// Without a caller deadline the fan-out is still bounded by the pool's own
// timeout (seconds, not forever).
func TestPool_fanoutNoDeadlineIsBounded(t *testing.T) {
	t.Run("tab_list", func(t *testing.T) {
		t.Parallel()
		p, _, _ := hungAndHealthy(t)
		list, err := within(t, 6*time.Second, func() (protocol.TabListResult, error) {
			return p.TabList(context.Background(), protocol.TabListParams{})
		})
		if err != nil || !sameIDs(listIDs(list), "1:tb") {
			t.Fatalf("list %v err %v", list.Tabs, err)
		}
		waitInFlightZero(t, p)
	})
	t.Run("tab_focus", func(t *testing.T) {
		t.Parallel()
		p, _, _ := hungAndHealthy(t)
		focus, err := within(t, 6*time.Second, func() (protocol.TabFocusResult, error) {
			return p.TabFocus(context.Background(), protocol.TabFocusParams{})
		})
		if err != nil || focus.Tab != "1:tb" {
			t.Fatalf("focus %q err %v", focus.Tab, err)
		}
		waitInFlightZero(t, p)
	})
	t.Run("all hung", func(t *testing.T) {
		t.Parallel()
		a, sa := newFakeDaemon(t, "a")
		b, sb := newFakeDaemon(t, "b")
		sa.hangRPC(t)
		sb.hangRPC(t)
		p, err := NewPool(NewClient(a.URL), NewClient(b.URL))
		if err != nil {
			t.Fatal(err)
		}
		_, err = within(t, 6*time.Second, func() (protocol.TabListResult, error) {
			return p.TabList(context.Background(), protocol.TabListParams{})
		})
		var af *AllFailedError
		if !errors.As(err, &af) || af.Op != protocol.MethodTabList || len(af.Errs) != 2 {
			t.Fatalf("want AllFailedError tab_list with 2 errs, got %v", err)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("timed-out backends must report a deadline error, got %v", err)
		}
		waitInFlightZero(t, p)
	})
}

func TestPool_fanoutAllHungCallerDeadline(t *testing.T) {
	a, sa := newFakeDaemon(t, "a")
	b, sb := newFakeDaemon(t, "b")
	sa.hangRPC(t)
	sb.hangRPC(t)
	p, err := NewPool(NewClient(a.URL), NewClient(b.URL))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err = within(t, 5*time.Second, func() (protocol.TabListResult, error) {
		return p.TabList(ctx, protocol.TabListParams{})
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("TabList: want deadline error, got %v", err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel2()
	_, err = within(t, 5*time.Second, func() (protocol.TabFocusResult, error) {
		return p.TabFocus(ctx2, protocol.TabFocusParams{})
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("TabFocus: want deadline error, got %v", err)
	}
	waitInFlightZero(t, p)
}

func TestPool_fanoutPreCancelledSendsNothing(t *testing.T) {
	a, sa := newFakeDaemon(t, "a")
	b, sb := newFakeDaemon(t, "b")
	p, err := NewPool(NewClient(a.URL), NewClient(b.URL))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.TabList(ctx, protocol.TabListParams{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("TabList: want context.Canceled, got %v", err)
	}
	if _, err := p.TabFocus(ctx, protocol.TabFocusParams{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("TabFocus: want context.Canceled, got %v", err)
	}
	for _, st := range []*fakeDaemon{sa, sb} {
		if n := st.methodCount(protocol.MethodTabList) + st.methodCount(protocol.MethodTabFocus); n != 0 {
			t.Fatalf("pre-cancelled call sent %d requests", n)
		}
	}
	waitInFlightZero(t, p)
}

// Explicit cancellation aborts the whole call: it returns the caller's
// context error promptly and binds nothing, even though backend 1 had
// already answered.
func TestPool_fanoutCancelMidCall(t *testing.T) {
	p, _, _ := hungAndHealthy(t)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(150*time.Millisecond, cancel)
	start := time.Now()
	list, err := within(t, 5*time.Second, func() (protocol.TabListResult, error) {
		return p.TabList(ctx, protocol.TabListParams{})
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("TabList: want context.Canceled, got %v (list %v)", err, list.Tabs)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("cancel took %v to return", d)
	}
	if len(list.Tabs) != 0 || list.Tab != "" {
		t.Fatalf("cancelled call must not return data, got %+v", list)
	}
	if _, ok := p.ClientForTab("1:tb"); ok {
		t.Fatal("cancelled call must not bind tabs")
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	time.AfterFunc(150*time.Millisecond, cancel2)
	focus, err := within(t, 5*time.Second, func() (protocol.TabFocusResult, error) {
		return p.TabFocus(ctx2, protocol.TabFocusParams{})
	})
	if !errors.Is(err, context.Canceled) || focus.Tab != "" {
		t.Fatalf("TabFocus: want context.Canceled and no data, got %v / %q", err, focus.Tab)
	}
	if _, ok := p.ClientForTab("1:tb"); ok {
		t.Fatal("cancelled call must not bind tabs")
	}
	waitInFlightZero(t, p)
}

// Backends answer in reverse order; results still follow constructor order.
func TestPool_fanoutKeepsConstructorOrder(t *testing.T) {
	a, sa := newFakeDaemon(t, "a")
	b, sb := newFakeDaemon(t, "b")
	c, sc := newFakeDaemon(t, "c")
	sa.delay = 200 * time.Millisecond
	sb.delay = 100 * time.Millisecond
	sa.listTab, sb.listTab, sc.listTab = "ta", "tb", "tc"
	p, err := NewPool(NewClient(a.URL), NewClient(b.URL), NewClient(c.URL))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	start := time.Now()
	list, err := p.TabList(ctx, protocol.TabListParams{})
	if err != nil {
		t.Fatal(err)
	}
	if !sameIDs(listIDs(list), "0:ta", "1:tb", "2:tc") {
		t.Fatalf("merge order %v", list.Tabs)
	}
	if list.Tab != "0:ta" || list.Focus != "0:ta" {
		t.Fatalf("tab %q focus %q: want first backend in constructor order", list.Tab, list.Focus)
	}
	focus, err := p.TabFocus(ctx, protocol.TabFocusParams{})
	if err != nil {
		t.Fatal(err)
	}
	if focus.Tab != "0:ta" {
		t.Fatalf("TabFocus %q: want first backend in constructor order, not the fastest", focus.Tab)
	}
	// Everything answered: no waiting for the fan-out timeout.
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Fatalf("calls took %v", d)
	}
	waitInFlightZero(t, p)
}

func TestPool_fanoutPartialFailureAndHang(t *testing.T) {
	a, sa := newFakeDaemon(t, "a")
	b, sb := newFakeDaemon(t, "b")
	c, sc := newFakeDaemon(t, "c")
	sa.failRPC.Store(true)
	sb.hangRPC(t)
	sc.listTab = "tc"
	p, err := NewPool(NewClient(a.URL), NewClient(b.URL), NewClient(c.URL))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	list, err := within(t, 5*time.Second, func() (protocol.TabListResult, error) {
		return p.TabList(ctx, protocol.TabListParams{})
	})
	if err != nil || !sameIDs(listIDs(list), "2:tc") {
		t.Fatalf("list %v err %v", list.Tabs, err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel2()
	focus, err := within(t, 5*time.Second, func() (protocol.TabFocusResult, error) {
		return p.TabFocus(ctx2, protocol.TabFocusParams{})
	})
	if err != nil || focus.Tab != "2:tc" {
		t.Fatalf("focus %q err %v", focus.Tab, err)
	}
	waitInFlightZero(t, p)
}

// tab_focus is decided once every backend ahead of the first success has
// answered; a hung backend behind it costs nothing.
func TestPool_tabFocusDoesNotWaitForLaterBackends(t *testing.T) {
	a, sa := newFakeDaemon(t, "a")
	b, sb := newFakeDaemon(t, "b")
	c, sc := newFakeDaemon(t, "c")
	sa.failRPC.Store(true)
	sb.listTab = "tb"
	sc.hangRPC(t)
	p, err := NewPool(NewClient(a.URL), NewClient(b.URL), NewClient(c.URL))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	focus, err := within(t, 5*time.Second, func() (protocol.TabFocusResult, error) {
		return p.TabFocus(context.Background(), protocol.TabFocusParams{})
	})
	if err != nil || focus.Tab != "1:tb" {
		t.Fatalf("focus %q err %v", focus.Tab, err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("TabFocus waited %v for a backend it did not need", d)
	}
	waitInFlightZero(t, p)
}

// An empty list is a success, not a failed backend.
func TestPool_tabListEmptyIsSuccess(t *testing.T) {
	p, _, sb := hungAndHealthy(t)
	sb.listTab = ""
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	list, err := within(t, 5*time.Second, func() (protocol.TabListResult, error) {
		return p.TabList(ctx, protocol.TabListParams{})
	})
	if err != nil {
		t.Fatalf("empty list must succeed, got %v", err)
	}
	if list.Tabs == nil || len(list.Tabs) != 0 || list.Seq == 0 {
		t.Fatalf("want empty non-nil tabs with seq, got %+v", list)
	}
}

func TestPool_genericCallFanout(t *testing.T) {
	p, _, _ := hungAndHealthy(t)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	var list map[string]any
	if _, err := within(t, 5*time.Second, func() (struct{}, error) {
		return struct{}{}, p.Call(ctx, protocol.MethodTabList, nil, &list)
	}); err != nil {
		t.Fatalf("Call tab_list: %v", err)
	}
	tabs, _ := list["tabs"].([]any)
	if len(tabs) != 1 || tabs[0].(map[string]any)["tab"] != "1:tb" {
		t.Fatalf("tabs %v", list["tabs"])
	}
	p.ForgetTab("1:tb")

	// nil result still binds what was observed.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel2()
	if _, err := within(t, 5*time.Second, func() (struct{}, error) {
		return struct{}{}, p.Call(ctx2, protocol.MethodTabFocus, nil, nil)
	}); err != nil {
		t.Fatalf("Call tab_focus: %v", err)
	}
	if _, ok := p.ClientForTab("1:tb"); !ok {
		t.Fatal("Call(tab_focus, nil result) must bind the focused tab")
	}
	waitInFlightZero(t, p)
}

// A backend that answers after the call returned must not change the
// caller's result or the pool's bindings.
func TestPool_fanoutLateAnswerHasNoEffect(t *testing.T) {
	a, sa := newFakeDaemon(t, "a")
	b, sb := newFakeDaemon(t, "b")
	sa.delay = 400 * time.Millisecond // ignores cancellation, answers late
	sa.listTab = "ta"
	sb.listTab = "tb"
	p, err := NewPool(NewClient(a.URL), NewClient(b.URL))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	list, err := p.TabList(ctx, protocol.TabListParams{})
	if err != nil || !sameIDs(listIDs(list), "1:tb") {
		t.Fatalf("list %v err %v", list.Tabs, err)
	}
	time.Sleep(500 * time.Millisecond) // backend 0 has answered by now
	if !sameIDs(listIDs(list), "1:tb") || list.Tab != "1:tb" || list.Focus != "1:tb" {
		t.Fatalf("result changed after return: %+v", list)
	}
	if _, ok := p.ClientForTab("0:ta"); ok {
		t.Fatal("late answer must not bind tabs")
	}
	waitInFlightZero(t, p)
}

func TestPool_fanoutConcurrentCalls(t *testing.T) {
	p, _, _ := hungAndHealthy(t)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			list, err := p.TabList(ctx, protocol.TabListParams{})
			if err == nil && !sameIDs(listIDs(list), "1:tb") {
				err = errors.New("unexpected tab_list result")
			}
			errs <- err
		}()
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			focus, err := p.TabFocus(ctx, protocol.TabFocusParams{})
			if err == nil && focus.Tab != "1:tb" {
				err = errors.New("unexpected tab_focus result")
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	waitInFlightZero(t, p)
}
