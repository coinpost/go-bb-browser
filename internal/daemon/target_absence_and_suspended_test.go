package daemon

import (
	"context"
	"errors"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/chromedp/cdproto/target"
	"github.com/yiplee/go-bb-browser/internal/state"
	"github.com/yiplee/go-bb-browser/pkg/daemonclient"
	"github.com/yiplee/go-bb-browser/pkg/protocol"
)

// FT28 round-9 regressions:
//   - P1: confirmed target absence from Target.attachToTarget constrains
//     in-flight tab_new and clears stale state, while session-only errors and
//     page JS exceptions do not remove live tabs.
//   - P2-1: resolveTab and tab_select verify a suspended live tab instead of
//     returning unknown tab id, and surface retryable lookup errors when
//     verification fails or remains inconclusive.
//   - P2-2: tab_list / tab_focus verify suspended members before answering so a
//     live tab is not omitted and evicted by daemonclient.Pool.

func TestAttachTargetAbsentConstrainsInFlightCreate(t *testing.T) {
	t.Run("confirmed attach absence constrains in-flight create", func(t *testing.T) {
		s, st, sess, c, ctx, dir := knownTargetEnv(t)
		st.checkAttach = true
		knownTargetLoop(t, ctx, s, sess)

		newTab := knownTargetCreate(t, ctx, s, st, c, sess)
		ovfAwait(t, ctx, "A registered by its creation notification", func() bool { return s.tabs.Has(ovfA) })

		// A is removed in the browser with its destroyed notification withheld;
		// eval triggers Target.attachToTarget(A) -> "No target with given id found".
		st.removePage(ovfA, false)
		r, err := enumRPC(s, protocol.MethodEval, map[string]any{"tab": string(ovfA), "script": "1"})
		if err != nil {
			t.Fatal(err)
		}
		if r.Error == nil {
			t.Fatalf("eval on removed A unexpectedly succeeded: %v", r.Result)
		}
		if s.tabs.Known(ovfA) {
			t.Fatal("confirmed attach absence left dead A known in registry")
		}

		// The late tab_new reply must be refused instead of resurrecting A.
		closeOnce(c.release)
		causalRefusedCreate(t, <-newTab)
		ovfClearsRefuse(t, s, string(ovfA))
		if _, idle := s.tabIdle.Snapshot()[ovfA]; s.tabs.Known(ovfA) || idle {
			t.Fatalf("dead A survived: known=%v idle=%v", s.tabs.Known(ovfA), idle)
		}
		if knownTargetManagedOnDisk(t, s, dir) {
			t.Fatal("dead A is replayed from rpc.jsonl as managed")
		}
		if !s.tabs.Has(ovfB) || !st.hasPage(ovfB) {
			t.Fatalf("live B was affected: resolves=%v browser=%v", s.tabs.Has(ovfB), st.hasPage(ovfB))
		}
	})

	for _, tc := range []struct {
		name  string
		setup func(*cdpStub)
	}{
		{
			name: "live control: JS exception mentioning target absence does not remove target",
			setup: func(st *cdpStub) {
				st.setEvalExcOnce("Error: No target with given id found")
			},
		},
		{
			name: "live control: session-only attach error verifies without removing target",
			setup: func(st *cdpStub) {
				st.setAttachErrOnce("No session with given id")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, st, sess, c, ctx, dir := knownTargetEnv(t)
			st.checkAttach = true
			knownTargetLoop(t, ctx, s, sess)

			newTab := knownTargetCreate(t, ctx, s, st, c, sess)
			ovfAwait(t, ctx, "A registered by its creation notification", func() bool { return s.tabs.Has(ovfA) })
			tc.setup(st)

			r, err := enumRPC(s, protocol.MethodEval, map[string]any{"tab": string(ovfA), "script": "1"})
			if err != nil {
				t.Fatal(err)
			}
			if r.Error == nil {
				t.Fatalf("control eval unexpectedly succeeded: %v", r.Result)
			}
			if !s.tabs.Has(ovfA) {
				t.Fatal("live A was removed by non-target-absence error")
			}

			closeOnce(c.release)
			res := <-newTab
			if res.Error != nil || res.Result["tab"] != string(ovfA) {
				t.Fatalf("tab_new = %v / %v, want success for live A", res.Result, res.Error)
			}
			enumMustRPC(t, s, protocol.MethodEval, map[string]any{"tab": string(ovfA), "script": "1"})
			if _, idle := s.tabIdle.Snapshot()[ovfA]; !idle || !knownTargetManagedOnDisk(t, s, dir) {
				t.Fatalf("live A lost managed/idle state: idle=%v", idle)
			}
		})
	}
}

type hookedIDConn struct {
	*idConn
	hookMu sync.Mutex
	onList func(call int, base func() ([]*target.Info, error)) ([]*target.Info, error)
	calls  int
}

func (c *hookedIDConn) setOnList(fn func(call int, base func() ([]*target.Info, error)) ([]*target.Info, error)) {
	c.hookMu.Lock()
	c.onList = fn
	c.hookMu.Unlock()
}

func (c *hookedIDConn) PageTargets() ([]*target.Info, error) {
	c.hookMu.Lock()
	c.calls++
	call := c.calls
	fn := c.onList
	c.hookMu.Unlock()
	if fn != nil {
		return fn(call, c.idConn.PageTargets)
	}
	return c.idConn.PageTargets()
}

// suspendDiscoveryTarget puts live fullIDA into the suspended state by applying
// an older B-only enumeration after fullIDA was discovered via tab_list.
func suspendDiscoveryTarget(t *testing.T, srv *Server, conn *hookedIDConn) {
	t.Helper()
	// Open an older window while only B is listed, then discover A, then apply
	// the older B-only snapshot so A becomes suspended.
	wOld := srv.tabs.OpenWindow()
	oldSnap := []*target.Info{{TargetID: fullIDB, Type: "page"}}
	conn.mu.Lock()
	if !conn.hasLocked(fullIDA) {
		conn.pages = append(conn.pages, fullIDA)
	}
	conn.mu.Unlock()
	enumMustRPC(t, srv, protocol.MethodTabList, nil)
	if !srv.tabs.Has(fullIDA) {
		t.Fatalf("precondition: %s must resolve after discovery", fullIDA)
	}
	srv.applyEnumeration(conn, wOld, oldSnap)
	if srv.tabs.Has(fullIDA) || !srv.tabs.Suspended(fullIDA) {
		t.Fatalf("precondition: %s must be suspended (has=%v suspended=%v)", fullIDA, srv.tabs.Has(fullIDA), srv.tabs.Suspended(fullIDA))
	}
}

func TestSuspendedLiveTabVerifiesInsteadOfUnknownTabID(t *testing.T) {
	newEnv := func(t *testing.T) (*hookedIDConn, *Server, *daemonclient.Pool) {
		t.Helper()
		conn := &hookedIDConn{idConn: newIDConn(fullIDB)}
		srv := newIDServer(t, conn.idConn, stateDirDisabled)
		srv.tabHook = conn
		t.Cleanup(func() { _ = srv.closeAudit() })
		hs := httptest.NewServer(srv.Handler())
		t.Cleanup(hs.Close)
		pool, err := daemonclient.NewPool(daemonclient.NewClient(hs.URL))
		if err != nil {
			t.Fatal(err)
		}
		return conn, srv, pool
	}

	t.Run("resolveTab and tab_select recover suspended live tab", func(t *testing.T) {
		conn, srv, _ := newEnv(t)
		suspendDiscoveryTarget(t, srv, conn)
		enumMustRPC(t, srv, protocol.MethodEval, map[string]any{"tab": string(fullIDA), "script": "1"})
		if !srv.tabs.Has(fullIDA) {
			t.Fatal("A did not resume after eval verification")
		}

		conn2, srv2, _ := newEnv(t)
		suspendDiscoveryTarget(t, srv2, conn2)
		enumMustRPC(t, srv2, protocol.MethodTabSelect, map[string]any{"tab": string(fullIDA)})
		if srv2.tabs.Selected() != fullIDA {
			t.Fatalf("selected = %q, want %q", srv2.tabs.Selected(), fullIDA)
		}
	})

	t.Run("verification failure returns retryable error not unknown tab id", func(t *testing.T) {
		conn, srv, pool := newEnv(t)
		ctx := context.Background()
		list, err := pool.TabList(ctx, protocol.TabListParams{})
		if err != nil || len(list.Tabs) != 1 {
			t.Fatalf("initial TabList = %+v, %v", list, err)
		}
		// Open wOld before A is discovered so applying it after pool.TabList
		// binds A puts A into the suspended state.
		wOld := srv.tabs.OpenWindow()
		conn.mu.Lock()
		conn.pages = append(conn.pages, fullIDA)
		conn.mu.Unlock()
		if _, err = pool.TabList(ctx, protocol.TabListParams{}); err != nil {
			t.Fatalf("bind TabList: %v", err)
		}
		srv.applyEnumeration(conn, wOld, []*target.Info{{TargetID: fullIDB, Type: "page"}})
		if !srv.tabs.Suspended(fullIDA) {
			t.Fatalf("precondition: %s must be suspended", fullIDA)
		}
		conn.setListErr(errors.New("temporary list outage"))
		for _, m := range []string{protocol.MethodEval, protocol.MethodNetworkClear, protocol.MethodTabSelect} {
			params := map[string]any{"tab": string(fullIDA)}
			if m == protocol.MethodEval {
				params["script"] = "1"
			}
			r, err := enumRPC(srv, m, params)
			if err != nil {
				t.Fatal(err)
			}
			if r.Error == nil || r.Error["message"] != "failed to list targets" {
				t.Fatalf("%s error = %v, want failed to list targets", m, r.Error)
			}
		}

		_, err = pool.Eval(ctx, protocol.EvalParams{Tab: "0:" + string(fullIDA), Script: "1"})
		var unknown *daemonclient.UnknownTabError
		var rpcErr *daemonclient.RPCError
		if err == nil || errors.As(err, &unknown) || isUnknownTabRPCError(err) {
			t.Fatalf("Pool.Eval returned unknown tab error while verification failed: %v", err)
		}
		if !errors.As(err, &rpcErr) || rpcErr.Message != "failed to list targets" {
			t.Fatalf("Pool.Eval error = %v, want failed to list targets", err)
		}

		conn.setListErr(nil)
		if _, err := pool.Eval(ctx, protocol.EvalParams{Tab: "0:" + string(fullIDA), Script: "1"}); err != nil {
			t.Fatalf("Pool.Eval after verification recovered: %v", err)
		}
	})

	t.Run("persistent conflict past retry limit returns retryable error", func(t *testing.T) {
		conn, srv, _ := newEnv(t)
		// Open older windows BEFORE fullIDA is discovered so each applied B-only
		// snapshot suspends A (rather than removing it) and narrows the in-flight
		// verification window.
		wOlds := make([]*state.Window, 10)
		for i := range wOlds {
			wOlds[i] = srv.tabs.OpenWindow()
		}
		t.Cleanup(func() {
			for _, w := range wOlds {
				srv.tabs.Abandon(w)
			}
		})
		conn.mu.Lock()
		conn.pages = append(conn.pages, fullIDA)
		conn.mu.Unlock()
		enumMustRPC(t, srv, protocol.MethodTabList, nil)
		srv.applyEnumeration(conn, wOlds[0], []*target.Info{{TargetID: fullIDB, Type: "page"}})
		if !srv.tabs.Suspended(fullIDA) {
			t.Fatalf("precondition: %s must be suspended", fullIDA)
		}

		var idx atomic.Int32
		conn.setOnList(func(_ int, base func() ([]*target.Info, error)) ([]*target.Info, error) {
			i := int(idx.Add(1))
			if i < len(wOlds) {
				srv.applyEnumeration(conn, wOlds[i], []*target.Info{{TargetID: fullIDB, Type: "page"}})
			}
			return base()
		})
		for _, m := range []string{protocol.MethodEval, protocol.MethodNetworkClear, protocol.MethodTabSelect} {
			params := map[string]any{"tab": string(fullIDA)}
			if m == protocol.MethodEval {
				params["script"] = "1"
			}
			r, err := enumRPC(srv, m, params)
			if err != nil {
				t.Fatal(err)
			}
			if r.Error == nil || r.Error["message"] != "failed to list targets" {
				t.Fatalf("%s error = %v, want failed to list targets when still suspended", m, r.Error)
			}
			if !srv.tabs.Known(fullIDA) {
				t.Fatalf("%s dropped live suspended A", m)
			}
		}

		conn.setOnList(nil)
		enumMustRPC(t, srv, protocol.MethodEval, map[string]any{"tab": string(fullIDA), "script": "1"})
		enumMustRPC(t, srv, protocol.MethodTabSelect, map[string]any{"tab": string(fullIDA)})
	})
}

func TestTabListDoesNotOmitSuspendedLiveMemberForPool(t *testing.T) {
	newPoolEnv := func(t *testing.T) (*hookedIDConn, *Server, *daemonclient.Pool, *state.Window) {
		t.Helper()
		conn := &hookedIDConn{idConn: newIDConn(fullIDB)}
		srv := newIDServer(t, conn.idConn, stateDirDisabled)
		srv.tabHook = conn
		t.Cleanup(func() { _ = srv.closeAudit() })
		hs := httptest.NewServer(srv.Handler())
		t.Cleanup(hs.Close)
		pool, err := daemonclient.NewPool(daemonclient.NewClient(hs.URL))
		if err != nil {
			t.Fatal(err)
		}
		// Open wOverlap before A is discovered so applying it later suspends A
		// and contradicts an overlapping tab_list window.
		wOverlap := srv.tabs.OpenWindow()
		t.Cleanup(func() { srv.tabs.Abandon(wOverlap) })
		conn.mu.Lock()
		conn.pages = append(conn.pages, fullIDA)
		conn.mu.Unlock()
		res, err := pool.TabList(context.Background(), protocol.TabListParams{})
		if err != nil || len(res.Tabs) != 2 {
			t.Fatalf("initial TabList = %+v, %v", res, err)
		}
		return conn, srv, pool, wOverlap
	}

	t.Run("suspended live member triggers fresh verification and keeps Pool binding", func(t *testing.T) {
		conn, srv, pool, wOverlap := newPoolEnv(t)
		ctx := context.Background()

		var conflicted atomic.Bool
		conn.setOnList(func(_ int, base func() ([]*target.Info, error)) ([]*target.Info, error) {
			if conflicted.CompareAndSwap(false, true) {
				// While tab_list's window is open, an older B-only snapshot applies,
				// causing A in this reply to become suspended.
				srv.applyEnumeration(conn, wOverlap, []*target.Info{{TargetID: fullIDB, Type: "page"}})
			}
			return base()
		})

		res, err := pool.TabList(ctx, protocol.TabListParams{})
		if err != nil {
			t.Fatalf("Pool.TabList = %v", err)
		}
		if len(res.Tabs) != 2 {
			t.Fatalf("Pool.TabList tabs = %+v, want both A and B", res.Tabs)
		}
		if _, err := pool.Eval(ctx, protocol.EvalParams{Tab: "0:" + string(fullIDA), Script: "1"}); err != nil {
			t.Fatalf("Pool.Eval(A) after TabList = %v", err)
		}
	})

	t.Run("failed verification fails tab_list instead of omitting A and evicting Pool binding", func(t *testing.T) {
		conn, srv, pool, wOverlap := newPoolEnv(t)
		ctx := context.Background()

		var stage atomic.Int32
		conn.setOnList(func(_ int, base func() ([]*target.Info, error)) ([]*target.Info, error) {
			switch stage.Add(1) {
			case 1:
				srv.applyEnumeration(conn, wOverlap, []*target.Info{{TargetID: fullIDB, Type: "page"}})
				return base()
			case 2:
				return nil, errors.New("verification list failed")
			default:
				return base()
			}
		})

		if _, err := pool.TabList(ctx, protocol.TabListParams{}); err == nil {
			t.Fatal("Pool.TabList unexpectedly succeeded with partial list when verification failed")
		}
		beforeOps := len(conn.opsFor(fullIDA))
		if _, err := pool.Eval(ctx, protocol.EvalParams{Tab: "0:" + string(fullIDA), Script: "1"}); err != nil {
			t.Fatalf("Pool.Eval(A) failed after failed TabList verification (evicted binding?): %v", err)
		}
		if len(conn.opsFor(fullIDA)) != beforeOps+1 {
			t.Fatalf("Pool.Eval did not forward HTTP request to backend for live A")
		}
	})

	t.Run("genuine target removal still returns remaining tabs and retires Pool binding", func(t *testing.T) {
		conn, _, pool, _ := newPoolEnv(t)
		ctx := context.Background()
		conn.drop(fullIDA)

		res, err := pool.TabList(ctx, protocol.TabListParams{})
		if err != nil {
			t.Fatalf("Pool.TabList = %v", err)
		}
		if len(res.Tabs) != 1 || res.Tabs[0].Tab != "0:"+string(fullIDB) {
			t.Fatalf("Pool.TabList tabs = %+v, want only B", res.Tabs)
		}
		_, err = pool.Eval(ctx, protocol.EvalParams{Tab: "0:" + string(fullIDA), Script: "1"})
		var unknown *daemonclient.UnknownTabError
		if !errors.As(err, &unknown) {
			t.Fatalf("Pool.Eval(closed A) = %v, want *UnknownTabError", err)
		}
	})
}
