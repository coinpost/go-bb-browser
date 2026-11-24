package daemon

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/coinpost/go-bb-browser/pkg/daemonclient"
	"github.com/coinpost/go-bb-browser/pkg/protocol"
)

// These tests drive real daemon servers over HTTP through daemonclient, so the
// complete target id is followed end to end: Client / Pool -> JSON-RPC ->
// registry -> lock -> CDP command -> idle clock.

type idBackend struct {
	conn *idConn
	srv  *Server
	url  string
}

func newIDBackend(t *testing.T) *idBackend {
	t.Helper()
	conn := newIDConn()
	srv := newIDServer(t, conn, stateDirDisabled)
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return &idBackend{conn: conn, srv: srv, url: hs.URL}
}

func isUnknownTabRPCError(err error) bool {
	var rpcErr *daemonclient.RPCError
	return errors.As(err, &rpcErr) && rpcErr.Code == protocol.CodeInvalidParams && strings.Contains(rpcErr.Message, "unknown tab id")
}

func TestClientUsesFullTargetIDEndToEnd(t *testing.T) {
	be := newIDBackend(t)
	be.conn.queueCreate(fullIDA, fullIDB)
	c := daemonclient.NewClient(be.url)
	ctx := context.Background()

	a, err := c.TabNew(ctx, protocol.TabNewParams{URL: "https://example.test/"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.TabNew(ctx, protocol.TabNewParams{URL: "https://example.test/"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Tab != string(fullIDA) || b.Tab != string(fullIDB) {
		t.Fatalf("TabNew tabs = %q, %q; want the complete target ids", a.Tab, b.Tab)
	}
	list, err := c.TabList(ctx, protocol.TabListParams{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Tabs) != 2 || list.Tabs[0].Tab != a.Tab || list.Tabs[1].Tab != b.Tab || list.Focus != b.Tab {
		t.Fatalf("TabList = %+v", list)
	}
	focus, err := c.TabFocus(ctx, protocol.TabFocusParams{})
	if err != nil || focus.Tab != b.Tab {
		t.Fatalf("TabFocus = %+v, %v", focus, err)
	}
	bIdle := be.srv.tabIdle.Snapshot()[fullIDB]

	for _, m := range targetedMethods {
		var res struct {
			Tab string `json:"tab"`
		}
		if err := c.Call(ctx, m.method, m.params(a.Tab), &res); err != nil {
			t.Fatalf("%s with the created id: %v", m.method, err)
		}
		if res.Tab != a.Tab {
			t.Errorf("%s result tab = %q, want %q", m.method, res.Tab, a.Tab)
		}
	}
	if be.conn.has(fullIDA) {
		t.Error("TabClose did not close the tab")
	}

	// A is closed now. Its id and every old alias of it fail, and B is untouched.
	for _, tab := range append(fullIDVariants(fullIDB), a.Tab) {
		for _, m := range targetedMethods {
			if err := c.Call(ctx, m.method, m.params(tab), nil); !isUnknownTabRPCError(err) {
				t.Errorf("%s with tab %q: err = %v, want unknown tab id", m.method, tab, err)
			}
		}
	}
	if ops := be.conn.opsFor(fullIDB); len(ops) != 0 {
		t.Errorf("B received %v", ops)
	}
	if got := be.srv.tabIdle.Snapshot()[fullIDB]; !be.conn.has(fullIDB) || !got.Equal(bIdle) {
		t.Errorf("B was closed or renewed: open %v idle %v -> %v", be.conn.has(fullIDB), bIdle, got)
	}
	if _, err := c.Eval(ctx, protocol.EvalParams{Tab: b.Tab, Script: "1"}); err != nil {
		t.Fatalf("eval on B: %v", err)
	}
}

func TestPoolRoutesFullTargetIDsAcrossBackends(t *testing.T) {
	be := []*idBackend{newIDBackend(t), newIDBackend(t)}
	// Both daemons hand out same-suffix ids; each also has an unmanaged
	// bystander tab that must never be touched.
	be[0].conn.queueCreate(fullIDA)
	be[1].conn.queueCreate(fullIDB)
	pool, err := daemonclient.NewPoolBackends(
		daemonclient.PoolBackend{Key: "west", Client: daemonclient.NewClient(be[0].url)},
		daemonclient.PoolBackend{Key: "east", Client: daemonclient.NewClient(be[1].url)},
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	first, err := pool.TabNew(ctx, protocol.TabNewParams{URL: "https://example.test/"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := pool.TabNew(ctx, protocol.TabNewParams{URL: "https://example.test/"})
	if err != nil {
		t.Fatal(err)
	}
	const tabA, tabB = "west:" + string(fullIDA), "east:" + string(fullIDB)
	if got := map[string]bool{first.Tab: true, second.Tab: true}; !got[tabA] || !got[tabB] {
		t.Fatalf("pool TabNew tabs = %q, %q; want %q and %q", first.Tab, second.Tab, tabA, tabB)
	}

	list, err := pool.TabList(ctx, protocol.TabListParams{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Tabs) != 2 || list.Tabs[0].Tab != tabA || list.Tabs[1].Tab != tabB {
		t.Fatalf("pool TabList = %+v", list.Tabs)
	}

	// References the pool cannot attribute to a backend never reach a daemon.
	cross := "west:" + string(fullIDB) // B's id under A's backend
	idleB := be[1].srv.tabIdle.Snapshot()[fullIDB]
	for _, tab := range []string{
		string(fullIDA),                         // bare target id: no backend named
		"west:1234", "west:ab1234", "east:1234", // old short aliases
		cross,
		"west:" + strings.ToLower(string(fullIDA)),
		"west:" + string(fullIDA[:len(fullIDA)-1]),
		" " + tabA, tabA + " ",
		"WEST:" + string(fullIDA),
		"north:" + string(fullIDA),
	} {
		for _, m := range targetedMethods {
			err := pool.Call(ctx, m.method, m.params(tab), nil)
			var unknown *daemonclient.UnknownTabError
			var invalid *daemonclient.InvalidTabIDError
			if !errors.As(err, &unknown) && !errors.As(err, &invalid) {
				t.Errorf("%s with pool tab %q: err = %v, want UnknownTabError / InvalidTabIDError", m.method, tab, err)
			}
		}
	}
	for i, b := range be {
		if n := b.conn.opCount(); n != 0 {
			t.Errorf("backend %d received %d commands for unroutable ids", i, n)
		}
	}

	// Every targeted method with the id TabNew returned reaches its own
	// backend, addressed by the bare complete target id.
	for _, m := range targetedMethods {
		if m.method == protocol.MethodTabClose {
			continue
		}
		var res struct {
			Tab string `json:"tab"`
		}
		if err := pool.Call(ctx, m.method, m.params(tabA), &res); err != nil {
			t.Fatalf("%s via pool: %v", m.method, err)
		}
		if res.Tab != tabA {
			t.Errorf("%s result tab = %q, want %q", m.method, res.Tab, tabA)
		}
		if m.cdpOp != "" {
			if ops := be[0].conn.opsFor(fullIDA); len(ops) == 0 || ops[len(ops)-1] != m.cdpOp {
				t.Errorf("%s: backend west commands for A = %v", m.method, ops)
			}
		}
	}
	if n := be[1].conn.opCount(); n != 0 {
		t.Errorf("backend east received %d commands meant for west", n)
	}

	// A goes away behind the pool's back. The pool still routes the reference
	// to west only; west reports it unknown; east and B see nothing.
	be[0].conn.drop(fullIDA)
	for _, m := range targetedMethods {
		if err := pool.Call(ctx, m.method, m.params(tabA), nil); !isUnknownTabRPCError(err) {
			t.Errorf("%s on a gone tab via pool: err = %v, want the owner's unknown tab id", m.method, err)
		}
	}
	if n := be[1].conn.opCount(); n != 0 {
		t.Errorf("backend east received %d commands after A went away", n)
	}
	if got := be[1].srv.tabIdle.Snapshot()[fullIDB]; !be[1].conn.has(fullIDB) || !got.Equal(idleB) {
		t.Errorf("B was closed or renewed: open %v idle %v -> %v", be[1].conn.has(fullIDB), idleB, got)
	}

	// B is still fully usable through its own reference, and closing it unbinds it.
	time.Sleep(time.Millisecond)
	if _, err := pool.Eval(ctx, protocol.EvalParams{Tab: tabB, Script: "1"}); err != nil {
		t.Fatalf("eval B via pool: %v", err)
	}
	if got := be[1].srv.tabIdle.Snapshot()[fullIDB]; !got.After(idleB) {
		t.Errorf("eval via pool did not renew B: %v -> %v", idleB, got)
	}
	if _, err := pool.TabClose(ctx, protocol.TabCloseParams{Tab: tabB}); err != nil {
		t.Fatalf("close B via pool: %v", err)
	}
	if be[1].conn.has(target.ID(fullIDB)) {
		t.Error("B still open after pool TabClose")
	}
	var unknown *daemonclient.UnknownTabError
	if _, err := pool.Eval(ctx, protocol.EvalParams{Tab: tabB, Script: "1"}); !errors.As(err, &unknown) {
		t.Errorf("eval on a closed pool tab: err = %v, want UnknownTabError", err)
	}
}

// A tab closed directly on its daemon, behind the pool's back: the pool still
// routes the stale id to the daemon that owns it, and that daemon's explicit
// "unknown tab id" reaches the caller as UnknownTabError (with the RPC error
// kept). No other backend and no other tab is tried.
func TestPoolReportsTabClosedBehindItsBackAsUnknownTab(t *testing.T) {
	west, east := newIDBackend(t), newIDBackend(t)
	west.conn.queueCreate(fullIDA)
	east.conn.queueCreate(fullIDB)
	pool, err := daemonclient.NewPool(daemonclient.NewClient(west.url), daemonclient.NewClient(east.url))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var onWest, onEast string
	for range 2 {
		created, err := pool.TabNew(ctx, protocol.TabNewParams{})
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(created.Tab, "0:") {
			onWest = created.Tab
		} else {
			onEast = created.Tab
		}
	}
	if onWest != "0:"+string(fullIDA) || onEast != "1:"+string(fullIDB) {
		t.Fatalf("pool tabs = %q, %q", onWest, onEast)
	}
	enumMustRPC(t, west.srv, protocol.MethodTabClose, map[string]any{"tab": string(fullIDA)})

	_, err = pool.Eval(ctx, protocol.EvalParams{Tab: onWest, Script: "1"})
	var unknown *daemonclient.UnknownTabError
	if !errors.As(err, &unknown) || unknown.Tab != onWest {
		t.Fatalf("Eval on a tab closed behind the pool: err = %T %v, want errors.As(*UnknownTabError) for %q", err, err, onWest)
	}
	if !isUnknownTabRPCError(err) {
		t.Errorf("the daemon's RPC error is not in the chain: %v", err)
	}
	if !east.conn.has(fullIDB) || !east.srv.tabs.Has(fullIDB) {
		t.Error("the same-suffix tab on the other backend was touched")
	}
	if _, err := pool.Eval(ctx, protocol.EvalParams{Tab: onEast, Script: "1"}); err != nil {
		t.Errorf("live tab on the other backend: %v", err)
	}
}
