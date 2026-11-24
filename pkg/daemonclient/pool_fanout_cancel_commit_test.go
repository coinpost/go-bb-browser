package daemonclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/coinpost/go-bb-browser/pkg/protocol"
)

// Regressions for the commit step of tab_list / tab_focus: everything between
// the fan-out returning and the bindings / result being handed over (prefixing,
// decoding into the result type, waiting for the affinity lock) must still
// honour a caller cancellation.

// commitOps extends fanoutOps with the remaining generic Call result shapes.
var commitOps = append(fanoutOps[:len(fanoutOps):len(fanoutOps)], []struct {
	name string
	run  func(ctx context.Context, p *Pool) (err error, untouched bool)
}{
	{"Call tab_list map", func(ctx context.Context, p *Pool) (error, bool) {
		got := map[string]any{"keep": "x"}
		err := p.Call(ctx, protocol.MethodTabList, nil, &got)
		return err, len(got) == 1 && got["keep"] == "x"
	}},
	{"Call tab_focus any", func(ctx context.Context, p *Pool) (error, bool) {
		var got any = "keep"
		err := p.Call(ctx, protocol.MethodTabFocus, nil, &got)
		return err, got == "keep"
	}},
	{"Call tab_list nil result", func(ctx context.Context, p *Pool) (error, bool) {
		return p.Call(ctx, protocol.MethodTabList, nil, nil), true
	}},
	{"Call tab_focus nil result", func(ctx context.Context, p *Pool) (error, bool) {
		return p.Call(ctx, protocol.MethodTabFocus, nil, nil), true
	}},
}...)

// commitPool has two healthy in-process backends listing several tabs each and
// an old binding on each backend made before the call under test.
func commitPool(t *testing.T) *Pool {
	t.Helper()
	p := stubPool(t, &stubRT{tab: "ta", extra: 3}, &stubRT{tab: "tb", extra: 3})
	if err := p.bind("0:old", 0); err != nil {
		t.Fatal(err)
	}
	if err := p.bind("1:old", 1); err != nil {
		t.Fatal(err)
	}
	return p
}

var commitPoolTabs = []string{"0:ta", "0:ta-0", "0:ta-2", "1:tb", "1:tb-0", "1:tb-2"}

func mustStillBind(t *testing.T, p *Pool, tab string, idx int) {
	t.Helper()
	if c, ok := p.ClientForTab(tab); !ok || c != p.Clients()[idx] {
		t.Fatalf("tab %s must stay bound to backend %d", tab, idx)
	}
}

// duringCommitLockWait runs call while the test holds the pool's affinity lock,
// waits until the fan-out has returned (so its own cancellation check is
// behind it), runs act, and only then lets the call take the lock.
func duringCommitLockWait(t *testing.T, p *Pool, ctx *gateCtx, act func(), call func()) {
	t.Helper()
	// Both RPCs must have registered their generations before we take mu.
	gates := make([]*gate, p.Len())
	for i, c := range p.Clients() {
		g := newGate()
		defer g.open()
		gates[i] = g
		c.HTTP.Transport.(*stubRT).before = func(*http.Request) error {
			g.wait()
			return nil
		}
	}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		call()
	}()
	for _, g := range gates {
		select {
		case <-g.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("RPC did not reach its gate")
		}
	}
	p.mu.Lock()
	for _, g := range gates {
		g.open()
	}
	select {
	case <-ctx.stopped:
	case <-time.After(5 * time.Second):
		p.mu.Unlock()
		t.Fatal("fan-out did not return")
	}
	act()
	p.mu.Unlock()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("call did not return after the lock was released")
	}
}

// The caller cancels after every backend answered and the fan-out returned,
// while the call is still on its way to (or waiting for) the affinity lock.
func TestPool_fanoutCancelWhileWaitingForCommitLock(t *testing.T) {
	for _, op := range commitOps {
		t.Run(op.name, func(t *testing.T) {
			ctx := newGateCtx(time.Time{})
			defer ctx.release()
			p := commitPool(t)
			var err error
			var untouched bool
			duringCommitLockWait(t, p, ctx, ctx.cancel, func() { err, untouched = op.run(ctx, p) })
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("want context.Canceled, got %v", err)
			}
			if !untouched {
				t.Fatal("cancelled call must not write the caller's result")
			}
			mustNotBind(t, p, commitPoolTabs...)
			mustStillBind(t, p, "0:old", 0)
			mustStillBind(t, p, "1:old", 1)
			waitInFlightZero(t, p)
		})
	}
}

// Same gate, no cancellation: the staged batch is committed in full.
func TestPool_fanoutCommitsStagedBatchWithoutCancel(t *testing.T) {
	t.Run("TabList", func(t *testing.T) {
		ctx := newGateCtx(time.Time{})
		defer ctx.release()
		p := commitPool(t)
		var list protocol.TabListResult
		var err error
		duringCommitLockWait(t, p, ctx, func() {}, func() { list, err = p.TabList(ctx, protocol.TabListParams{}) })
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"0:ta", "0:ta-0", "0:ta-1", "0:ta-2", "1:tb", "1:tb-0", "1:tb-1", "1:tb-2"}
		if !sameIDs(listIDs(list), want...) || list.Tab != "0:ta" || list.Focus != "0:ta" || list.Seq != 7 {
			t.Fatalf("list %+v", list)
		}
		for _, tab := range commitPoolTabs {
			mustStillBind(t, p, tab, int(tab[0]-'0'))
		}
		mustStillBind(t, p, "1:tb-1", 1)
		mustNotBind(t, p, "0:old", "1:old")
	})
	t.Run("TabFocus", func(t *testing.T) {
		ctx := newGateCtx(time.Time{})
		defer ctx.release()
		p := commitPool(t)
		var focus protocol.TabFocusResult
		var err error
		duringCommitLockWait(t, p, ctx, func() {}, func() { focus, err = p.TabFocus(ctx, protocol.TabFocusParams{}) })
		if err != nil || focus.Tab != "0:ta" || focus.Focus != "0:ta" || focus.Seq != 7 {
			t.Fatalf("focus %+v err %v", focus, err)
		}
		mustStillBind(t, p, "0:ta", 0)
		mustNotBind(t, p, "1:tb")
		mustStillBind(t, p, "0:old", 0)
	})
	t.Run("Call tab_list map", func(t *testing.T) {
		ctx := newGateCtx(time.Time{})
		defer ctx.release()
		p := commitPool(t)
		var got map[string]any
		var err error
		duringCommitLockWait(t, p, ctx, func() {}, func() { err = p.Call(ctx, protocol.MethodTabList, nil, &got) })
		if err != nil || got["tab"] != "0:ta" || len(got["tabs"].([]any)) != 8 {
			t.Fatalf("got %v err %v", got, err)
		}
		mustStillBind(t, p, "1:tb-2", 1)
	})
	t.Run("Call tab_focus nil result", func(t *testing.T) {
		ctx := newGateCtx(time.Time{})
		defer ctx.release()
		p := commitPool(t)
		// Defensive rule kept from bind: an id another backend owns is not taken over.
		if err := p.bind("0:ta", 1); err != nil {
			t.Fatal(err)
		}
		var err error
		duringCommitLockWait(t, p, ctx, func() {}, func() { err = p.Call(ctx, protocol.MethodTabFocus, nil, nil) })
		if err != nil {
			t.Fatal(err)
		}
		mustStillBind(t, p, "0:ta", 1)
	})
}

// The caller's deadline passing during the lock wait is not a cancellation:
// answers decoded inside the budget are still committed.
func TestPool_fanoutDeadlineWhileWaitingForCommitLockKeepsAnswers(t *testing.T) {
	t.Run("TabList", func(t *testing.T) {
		ctx := newGateCtx(time.Now().Add(time.Minute))
		defer ctx.release()
		p := commitPool(t)
		var list protocol.TabListResult
		var err error
		duringCommitLockWait(t, p, ctx, ctx.expire, func() { list, err = p.TabList(ctx, protocol.TabListParams{}) })
		if err != nil || len(list.Tabs) != 8 || list.Tab != "0:ta" {
			t.Fatalf("list %+v err %v", list, err)
		}
		mustStillBind(t, p, "0:ta", 0)
		mustStillBind(t, p, "1:tb-2", 1)
	})
	t.Run("Call tab_focus", func(t *testing.T) {
		ctx := newGateCtx(time.Now().Add(time.Minute))
		defer ctx.release()
		p := commitPool(t)
		var focus protocol.TabFocusResult
		var err error
		duringCommitLockWait(t, p, ctx, ctx.expire, func() { err = p.Call(ctx, protocol.MethodTabFocus, nil, &focus) })
		if err != nil || focus.Tab != "0:ta" {
			t.Fatalf("focus %+v err %v", focus, err)
		}
		mustStillBind(t, p, "0:ta", 0)
	})
}

// decodeProbeHook runs whenever a decodeProbe is decoded (tests here are not
// parallel).
var decodeProbeHook func()

// decodeProbe is a caller result type with its own decoder: a user callback
// that runs while the answer is being prepared for the caller.
type decodeProbe struct {
	Raw string
}

func (d *decodeProbe) UnmarshalJSON(b []byte) error {
	if decodeProbeHook != nil {
		decodeProbeHook()
	}
	d.Raw = string(b)
	return nil
}

// The caller cancels while a large answer is being decoded into its result
// type. That happens before anything is committed and outside the affinity
// lock (the callback uses the pool), so the whole batch is still dropped.
func TestPool_fanoutCancelDuringResultPreparation(t *testing.T) {
	for _, method := range []string{protocol.MethodTabList, protocol.MethodTabFocus} {
		t.Run(method, func(t *testing.T) {
			ctx := newGateCtx(time.Time{})
			defer ctx.release()
			p := stubPool(t, &stubRT{tab: "ta", extra: 5000}, &stubRT{tab: "tb", extra: 5000})
			if err := p.bind("0:old", 0); err != nil {
				t.Fatal(err)
			}
			boundEarly, calls := false, 0
			decodeProbeHook = func() {
				calls++
				_, boundEarly = p.ClientForTab("0:ta") // deadlocks if the lock were held
				ctx.cancel()
			}
			defer func() { decodeProbeHook = nil }()

			got := decodeProbe{Raw: "keep"}
			_, err := within(t, 5*time.Second, func() (struct{}, error) {
				return struct{}{}, p.Call(ctx, method, nil, &got)
			})
			if calls != 1 {
				t.Fatalf("result decoder ran %d times, want 1", calls)
			}
			if boundEarly {
				t.Fatal("tabs were bound before the result was prepared and the cancellation checked")
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("want context.Canceled, got %v", err)
			}
			if got.Raw != "keep" {
				t.Fatal("cancelled call must not write the caller's result")
			}
			mustNotBind(t, p, "0:ta", "0:ta-0", "0:ta-4999", "1:tb", "1:tb-4999")
			mustStillBind(t, p, "0:old", 0)
			waitInFlightZero(t, p)
		})
	}
}

// Without a cancellation the same result type receives the decoded answer.
func TestPool_fanoutCustomResultDecoderSuccess(t *testing.T) {
	p := stubPool(t, &stubRT{tab: "ta"}, &stubRT{tab: "tb"})
	got := decodeProbe{Raw: "keep"}
	if err := p.Call(context.Background(), protocol.MethodTabFocus, nil, &got); err != nil {
		t.Fatal(err)
	}
	var focus protocol.TabFocusResult
	if err := json.Unmarshal([]byte(got.Raw), &focus); err != nil || focus.Tab != "0:ta" {
		t.Fatalf("raw %q err %v", got.Raw, err)
	}
	mustStillBind(t, p, "0:ta", 0)
}

// A result the answer cannot be decoded into is reported and left untouched;
// as before, the observed tabs are still bound.
func TestPool_fanoutUndecodableResult(t *testing.T) {
	for name, result := range map[string]any{
		"wrong type":  new(int),
		"non-pointer": protocol.TabFocusResult{},
		"nil pointer": (*protocol.TabFocusResult)(nil),
	} {
		t.Run(name, func(t *testing.T) {
			p := stubPool(t, &stubRT{tab: "ta"})
			err := p.Call(context.Background(), protocol.MethodTabFocus, nil, result)
			if err == nil || errors.Is(err, context.Canceled) {
				t.Fatalf("want a decode error, got %v", err)
			}
			if n, ok := result.(*int); ok && n != nil && *n != 0 {
				t.Fatalf("result written despite the error: %d", *n)
			}
			mustStillBind(t, p, "0:ta", 0)
		})
	}
}

// A selected reply still needs its removal marks during result staging. An
// abandoned transport may keep running after return, without pinning marks
// or changing bindings when it eventually produces a successful reply.
func TestPool_fanoutObservationLifetime(t *testing.T) {
	for _, method := range []string{protocol.MethodTabList, protocol.MethodTabFocus} {
		t.Run(method, func(t *testing.T) {
			slow := newGate()
			defer slow.open()
			fast := &stubRT{tab: "ta"}
			p := stubPool(t, fast, &stubRT{tab: "tb", before: func(*http.Request) error {
				slow.wait()
				return nil
			}})
			fast.before = func(*http.Request) error {
				<-slow.entered
				p.ForgetTab("1:tb")
				return nil
			}
			if err := p.bind("1:old", 1); err != nil {
				t.Fatal(err)
			}
			p.fanoutTimeout = 500 * time.Millisecond
			decodeProbeHook = func() { p.ForgetTab("0:ta") }
			defer func() { decodeProbeHook = nil }()
			var result decodeProbe
			if err := p.Call(context.Background(), method, nil, &result); err != nil {
				t.Fatal(err)
			}
			if tabFromRawResult(json.RawMessage(result.Raw)) != "0:ta" {
				t.Fatalf("staged result: %s", result.Raw)
			}
			mustNotBind(t, p, "0:ta", "1:tb")
			mustStillBind(t, p, "1:old", 1)
			for i := range p.clients {
				if len(p.observing[i]) != 0 || len(p.removed[i]) != 0 {
					t.Fatalf("backend %d retained observations or removal marks after return", i)
				}
			}
			slow.open()
			waitInFlightZero(t, p)
			mustNotBind(t, p, "1:tb")
			mustStillBind(t, p, "1:old", 1)
		})
	}
}
