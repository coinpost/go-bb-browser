package daemonclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coinpost/go-bb-browser/pkg/protocol"
)

// Regressions for what tab_list / tab_focus fan-out may commit: only answers
// completed inside the budget, never after the caller cancelled, and never by
// reading a Client's public configuration from a background goroutine.

// gateCtx is a caller context whose cancellation reaches derived contexts only
// when the test calls release. context.WithTimeout registers through AfterFunc,
// so until then the fan-out's own context stays live. That pins the window in
// which the caller is already cancelled (or past its deadline) while a
// backend's answer is the only thing ready to be received.
type gateCtx struct {
	deadline time.Time // zero: none

	mu   sync.Mutex
	done chan struct{}
	err  error
	held []func()

	// stopped is closed when a derived context deregisters, which the fan-out
	// does on its way out (after its own last look at the caller's context).
	stopped  chan struct{}
	stopOnce sync.Once
}

func newGateCtx(deadline time.Time) *gateCtx {
	return &gateCtx{deadline: deadline, done: make(chan struct{}), stopped: make(chan struct{})}
}

func (c *gateCtx) Deadline() (time.Time, bool) { return c.deadline, !c.deadline.IsZero() }
func (c *gateCtx) Done() <-chan struct{}       { return c.done }
func (c *gateCtx) Value(any) any               { return nil }

func (c *gateCtx) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *gateCtx) AfterFunc(f func()) (stop func() bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.held = append(c.held, f)
	return func() bool {
		c.stopOnce.Do(func() { close(c.stopped) })
		return false
	}
}

// cancel marks the context done without telling derived contexts yet.
func (c *gateCtx) cancel() { c.finish(context.Canceled) }

// expire is cancel for a caller whose deadline passed.
func (c *gateCtx) expire() { c.finish(context.DeadlineExceeded) }

func (c *gateCtx) finish(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil {
		c.err = err
		close(c.done)
	}
}

// release delivers the held notifications (and ends leftover requests).
func (c *gateCtx) release() {
	c.cancel()
	c.mu.Lock()
	held := c.held
	c.held = nil
	c.mu.Unlock()
	for _, f := range held {
		f()
	}
}

// stubRT is an in-process daemon: no sockets, so tests decide exactly when an
// answer completes.
type stubRT struct {
	tab    string
	extra  int                           // additional listed tabs "<tab>-<i>"
	before func(req *http.Request) error // runs before the answer is built

	mu  sync.Mutex
	ids []string
	hdr []http.Header
}

func (s *stubRT) RoundTrip(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	var in struct {
		ID json.RawMessage `json:"id"`
	}
	_ = json.Unmarshal(body, &in)
	s.mu.Lock()
	s.ids = append(s.ids, string(in.ID))
	s.hdr = append(s.hdr, req.Header.Clone())
	s.mu.Unlock()
	if s.before != nil {
		if err := s.before(req); err != nil {
			return nil, err
		}
	}
	var more strings.Builder
	for i := 0; i < s.extra; i++ {
		fmt.Fprintf(&more, `,{"tab":"%s-%d"}`, s.tab, i)
	}
	out := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"tab":%q,"focus":%q,"seq":7,"tabs":[{"tab":%q}%s]}}`,
		in.ID, s.tab, s.tab, s.tab, more.String())
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(out)),
		Request:    req,
	}, nil
}

func (s *stubRT) seenIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ids...)
}

func stubPool(t *testing.T, stubs ...*stubRT) *Pool {
	t.Helper()
	clients := make([]*Client, len(stubs))
	for i, s := range stubs {
		clients[i] = NewClient(fmt.Sprintf("http://stub-%d.invalid", i))
		clients[i].HTTP = &http.Client{Transport: s}
	}
	p, err := NewPool(clients...)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func mustNotBind(t *testing.T, p *Pool, tabs ...string) {
	t.Helper()
	for _, tab := range tabs {
		if _, ok := p.ClientForTab(tab); ok {
			t.Fatalf("tab %s must not be bound", tab)
		}
	}
}

// fanoutOps runs the same scenario through the typed methods and generic Call.
// Each returns the error and whether the caller's result was left untouched.
var fanoutOps = []struct {
	name string
	run  func(ctx context.Context, p *Pool) (err error, untouched bool)
}{
	{"TabList", func(ctx context.Context, p *Pool) (error, bool) {
		got, err := p.TabList(ctx, protocol.TabListParams{})
		return err, got.Tab == "" && got.Focus == "" && got.Seq == 0 && len(got.Tabs) == 0
	}},
	{"TabFocus", func(ctx context.Context, p *Pool) (error, bool) {
		got, err := p.TabFocus(ctx, protocol.TabFocusParams{})
		return err, got == protocol.TabFocusResult{}
	}},
	{"Call tab_list", func(ctx context.Context, p *Pool) (error, bool) {
		got := protocol.TabListResult{Tab: "keep", Seq: 99}
		err := p.Call(ctx, protocol.MethodTabList, nil, &got)
		return err, got.Tab == "keep" && got.Seq == 99 && got.Focus == "" && got.Tabs == nil
	}},
	{"Call tab_focus", func(ctx context.Context, p *Pool) (error, bool) {
		got := protocol.TabFocusResult{Tab: "keep", Seq: 99}
		err := p.Call(ctx, protocol.MethodTabFocus, nil, &got)
		return err, got == protocol.TabFocusResult{Tab: "keep", Seq: 99}
	}},
}

// The caller cancels, then every backend answers successfully while the
// answers are the only thing ready: the whole batch must still be discarded.
func TestPool_fanoutCancelBeatsReadyAnswers(t *testing.T) {
	for _, op := range fanoutOps {
		t.Run(op.name, func(t *testing.T) {
			ctx := newGateCtx(time.Time{})
			cancelThenAnswer := func(*http.Request) error { ctx.cancel(); return nil }
			p := stubPool(t,
				&stubRT{tab: "ta", before: cancelThenAnswer},
				&stubRT{tab: "tb", before: cancelThenAnswer})
			err, untouched := op.run(ctx, p)
			ctx.release()
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("want context.Canceled, got %v", err)
			}
			if !untouched {
				t.Fatal("cancelled call must not write the caller's result")
			}
			mustNotBind(t, p, "0:ta", "1:tb")
			waitInFlightZero(t, p)
		})
	}
}

// The only successful backend completes after the budget ended, but before the
// fan-out learns that its context is done (normal receive path).
func TestPool_fanoutRejectsSuccessCompletedAfterBudget(t *testing.T) {
	for _, op := range fanoutOps {
		t.Run(op.name, func(t *testing.T) {
			deadline := time.Now().Add(30 * time.Millisecond)
			ctx := newGateCtx(deadline)
			defer ctx.release()
			p := stubPool(t, &stubRT{tab: "ta", before: func(*http.Request) error {
				for !time.Now().After(deadline) {
					time.Sleep(time.Millisecond)
				}
				return nil
			}})
			err, untouched := op.run(ctx, p)
			var all *AllFailedError
			if !errors.As(err, &all) || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("want AllFailedError wrapping DeadlineExceeded, got %v", err)
			}
			if !untouched {
				t.Fatal("over-budget answer must not write the caller's result")
			}
			mustNotBind(t, p, "0:ta")
			waitInFlightZero(t, p)
		})
	}
}

// Same, with a real deadline: the backend ignores cancellation and answers
// successfully only once its request context is done (deadline drain path).
func TestPool_fanoutRejectsSuccessAfterRealDeadline(t *testing.T) {
	for _, op := range fanoutOps {
		t.Run(op.name, func(t *testing.T) {
			for round := 0; round < 20; round++ {
				p := stubPool(t, &stubRT{tab: "ta", before: func(req *http.Request) error {
					<-req.Context().Done()
					return nil
				}})
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
				err, untouched := op.run(ctx, p)
				cancel()
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("round %d: want DeadlineExceeded, got %v", round, err)
				}
				if !untouched {
					t.Fatalf("round %d: late answer must not write the caller's result", round)
				}
				waitInFlightZero(t, p)
				mustNotBind(t, p, "0:ta")
			}
		})
	}
}

// In-budget successes survive next to an over-budget one, in constructor order.
func TestPool_fanoutKeepsInBudgetNextToOverBudget(t *testing.T) {
	// A roomy budget: the in-budget stubs answer in microseconds.
	setup := func() (*gateCtx, func(*http.Request) error) {
		deadline := time.Now().Add(300 * time.Millisecond)
		return newGateCtx(deadline), func(*http.Request) error {
			for !time.Now().After(deadline) {
				time.Sleep(time.Millisecond)
			}
			return nil
		}
	}
	t.Run("TabList", func(t *testing.T) {
		ctx, late := setup()
		defer ctx.release()
		p := stubPool(t, &stubRT{tab: "ta", before: late}, &stubRT{tab: "tb"}, &stubRT{tab: "tc"})
		list, err := p.TabList(ctx, protocol.TabListParams{})
		if err != nil || !sameIDs(listIDs(list), "1:tb", "2:tc") || list.Tab != "1:tb" {
			t.Fatalf("list %+v err %v", list, err)
		}
		mustNotBind(t, p, "0:ta")
		if _, ok := p.ClientForTab("2:tc"); !ok {
			t.Fatal("in-budget tab must be bound")
		}
		waitInFlightZero(t, p)
	})
	t.Run("TabFocus", func(t *testing.T) {
		ctx, late := setup()
		defer ctx.release()
		p := stubPool(t, &stubRT{tab: "ta", before: late}, &stubRT{tab: "tb"})
		focus, err := p.TabFocus(ctx, protocol.TabFocusParams{})
		if err != nil || focus.Tab != "1:tb" {
			t.Fatalf("focus %+v err %v", focus, err)
		}
		mustNotBind(t, p, "0:ta")
		waitInFlightZero(t, p)
	})
}

// TabFocus returns as soon as backend 0 answers; the caller may then
// reconfigure the other backend's Client while its request is still running.
// Run with -race: the background request must not read the public fields.
func TestPool_fanoutDoesNotReadClientConfigAfterReturn(t *testing.T) {
	for round := 0; round < 50; round++ {
		unblock := make(chan struct{})
		slow := &stubRT{tab: "tb", before: func(*http.Request) error {
			<-unblock
			return errors.New("abandoned")
		}}
		p := stubPool(t, &stubRT{tab: "ta"}, slow)
		other := p.Clients()[1]
		other.Headers = http.Header{"X-Probe": {"before"}}

		focus, err := p.TabFocus(context.Background(), protocol.TabFocusParams{})
		if err != nil || focus.Tab != "0:ta" {
			t.Fatalf("focus %+v err %v", focus, err)
		}
		other.Headers.Set("X-Probe", "after")
		other.Headers = nil
		other.BaseURL = "http://reconfigured.invalid"
		other.HTTP = &http.Client{Transport: &stubRT{tab: "tz"}}

		close(unblock)
		waitInFlightZero(t, p)
		slow.mu.Lock()
		for _, h := range slow.hdr {
			if got := h.Get("X-Probe"); got != "before" {
				t.Fatalf("background request saw header %q, want the value at call time", got)
			}
		}
		slow.mu.Unlock()
	}
}

// Fan-out requests draw ids from the Client's own counter, so they never
// collide with direct calls on the same Client.
func TestPool_fanoutRequestIDsShareClientCounter(t *testing.T) {
	stub := &stubRT{tab: "ta"}
	p := stubPool(t, stub, &stubRT{tab: "tb"})
	c := p.Clients()[0]
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			if _, err := c.TabList(ctx, protocol.TabListParams{}); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := p.TabList(ctx, protocol.TabListParams{}); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := p.TabFocus(ctx, protocol.TabFocusParams{}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	waitInFlightZero(t, p)
	ids := stub.seenIDs()
	if len(ids) != 24 {
		t.Fatalf("backend 0 saw %d requests, want 24", len(ids))
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || id == "null" || seen[id] {
			t.Fatalf("request id %q missing or reused: %v", id, ids)
		}
		seen[id] = true
	}
	if next := string(c.nextID()); next != "25" {
		t.Fatalf("client counter at %s after 24 requests, want 25", next)
	}
}

// The snapshot a fan-out request runs from is fixed when it is taken: later
// changes to the Client (including in-place header edits) do not reach it.
func TestClient_rpcTargetSnapshotIsIndependent(t *testing.T) {
	orig := &stubRT{tab: "ta"}
	c := NewClient("http://orig.invalid", WithHeader("X-Probe", "before"))
	c.HTTP = &http.Client{Transport: orig}
	target := c.rpcTarget(true)

	swapped := &stubRT{tab: "tz"}
	c.Headers.Set("X-Probe", "after")
	c.Headers.Add("X-Extra", "1")
	c.BaseURL = "http://swapped.invalid"
	c.HTTP = &http.Client{Transport: swapped}

	var got protocol.TabFocusResult
	if err := target.call(context.Background(), protocol.MethodTabFocus, json.RawMessage("{}"), &got); err != nil {
		t.Fatal(err)
	}
	if got.Tab != "ta" || len(swapped.seenIDs()) != 0 {
		t.Fatalf("request went to the reconfigured transport: %+v", got)
	}
	if ids := orig.seenIDs(); len(ids) != 1 || ids[0] != "1" {
		t.Fatalf("ids %v, want the id drawn at snapshot time", ids)
	}
	h := orig.hdr[0]
	if h.Get("X-Probe") != "before" || h.Get("X-Extra") != "" {
		t.Fatalf("snapshot headers changed: %v", h)
	}
	if next := string(c.nextID()); next != "2" {
		t.Fatalf("next id %s, want 2", next)
	}
}

// With no HTTP client set, the snapshot pins the default one.
func TestClient_rpcTargetDefaultsHTTPClient(t *testing.T) {
	if got := NewClient("http://x.invalid").rpcTarget(true); got.http != http.DefaultClient || got.url != "http://x.invalid/v1" || got.headers != nil {
		t.Fatalf("unexpected snapshot %+v", got)
	}
}
