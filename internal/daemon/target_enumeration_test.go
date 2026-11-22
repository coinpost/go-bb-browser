package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/yiplee/go-bb-browser/internal/browser"
	"github.com/yiplee/go-bb-browser/pkg/protocol"
)

// Regressions for stale Target.getTargets snapshots: a full enumeration that was
// requested before a tab was registered must never remove that tab from the
// registry, idle tracking or observation buffers, while a fresh enumeration must
// still remove targets that really disappeared.

const enumTestWait = 10 * time.Second

// enumConn is a concurrency-safe fake browser. PageTargets takes its snapshot
// first and only then runs afterSnapshot, which is where a test parks the reply
// to reproduce "snapshot taken, not yet applied".
type enumConn struct {
	*fakeConn

	mu            sync.Mutex
	pages         []*target.Info
	nextIDs       []target.ID
	created       int
	listErr       error
	afterSnapshot func()
}

func newEnumConn(ids ...target.ID) *enumConn {
	c := &enumConn{fakeConn: &fakeConn{}}
	for _, id := range ids {
		c.pages = append(c.pages, &target.Info{TargetID: id, Type: "page"})
	}
	return c
}

func (c *enumConn) PageTargets() ([]*target.Info, error) {
	c.mu.Lock()
	snapshot := append([]*target.Info(nil), c.pages...)
	err := c.listErr
	hook := c.afterSnapshot
	c.mu.Unlock()
	if hook != nil {
		hook()
	}
	if err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (c *enumConn) CreatePageTarget(initialURL string, _ bool) (target.ID, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.created++
	var id target.ID
	if len(c.nextIDs) > 0 {
		id = c.nextIDs[0]
		c.nextIDs = c.nextIDs[1:]
	} else {
		id = target.ID(fmt.Sprintf("BBBB%028X", c.created))
	}
	c.pages = append(c.pages, &target.Info{TargetID: id, Type: "page", URL: initialURL})
	return id, nil
}

func (c *enumConn) CloseTarget(id target.ID) error {
	c.removeExternally(id)
	return nil
}

// removeExternally drops a page as if the user closed it in Chrome.
func (c *enumConn) removeExternally(id target.ID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	kept := make([]*target.Info, 0, len(c.pages))
	for _, info := range c.pages {
		if info.TargetID != id {
			kept = append(kept, info)
		}
	}
	c.pages = kept
}

func (c *enumConn) has(id target.ID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, info := range c.pages {
		if info.TargetID == id {
			return true
		}
	}
	return false
}

func (c *enumConn) setAfterSnapshot(fn func()) {
	c.mu.Lock()
	c.afterSnapshot = fn
	c.mu.Unlock()
}

func (c *enumConn) setListErr(err error) {
	c.mu.Lock()
	c.listErr = err
	c.mu.Unlock()
}

// holdNextSnapshot parks the next PageTargets call after it took its snapshot.
func (c *enumConn) holdNextSnapshot() (entered <-chan struct{}, release func()) {
	in := make(chan struct{})
	out := make(chan struct{})
	var once, releaseOnce sync.Once
	c.setAfterSnapshot(func() {
		first := false
		once.Do(func() { first = true })
		if first {
			close(in)
			<-out
		}
	})
	return in, func() { releaseOnce.Do(func() { close(out) }) }
}

type enumRPCReply struct {
	Result map[string]any `json:"result"`
	Error  map[string]any `json:"error"`
}

// enumRPC posts one JSON-RPC call. It never touches testing.T so it is safe from
// helper goroutines.
func enumRPC(srv *Server, method string, params map[string]any) (enumRPCReply, error) {
	var out enumRPCReply
	if params == nil {
		params = map[string]any{}
	}
	req := httptest.NewRequest(http.MethodPost, "/v1", bytes.NewBufferString(rpcReq(method, params, 1)))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return out, fmt.Errorf("%s: HTTP %d: %s", method, rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		return out, fmt.Errorf("%s: %w: %s", method, err, rec.Body.String())
	}
	return out, nil
}

func enumMustRPC(t *testing.T, srv *Server, method string, params map[string]any) map[string]any {
	t.Helper()
	reply, err := enumRPC(srv, method, params)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Error != nil {
		t.Fatalf("%s returned error: %v", method, reply.Error)
	}
	return reply.Result
}

func enumTabNew(t *testing.T, srv *Server) string {
	t.Helper()
	short, _ := enumMustRPC(t, srv, protocol.MethodTabNew, nil)["tab"].(string)
	if short == "" {
		t.Fatal("tab_new returned no tab id")
	}
	return short
}

func enumAwait(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(enumTestWait):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func enumPushNetwork(t *testing.T, srv *Server, tid target.ID) {
	t.Helper()
	seq, err := srv.store.NextSeq()
	if err != nil {
		t.Fatal(err)
	}
	// Buffers exist only for registered targets, as after a first query.
	srv.obsStore.RegisterTarget(tid)
	srv.obsStore.PushNetwork(tid, seq, json.RawMessage(`{"url":"https://example.test/"}`))
}

// assertTabIntact checks every daemon-side resource a stale enumeration used to
// destroy for a tab that tab_new reported as created.
func assertTabIntact(t *testing.T, srv *Server, short string, tid target.ID) {
	t.Helper()
	if got, ok := srv.tabs.Lookup(short); !ok || got != tid {
		t.Errorf("tab %q returned by tab_new is no longer registered (got %q, %v)", short, got, ok)
	}
	if _, ok := srv.tabIdle.Snapshot()[tid]; !ok {
		t.Errorf("tab %q lost its managed idle tracking", short)
	}
	if events, _, _ := srv.obsStore.QueryNetwork(tid, 0); len(events) != 1 {
		t.Errorf("tab %q observation buffer has %d events, want 1", short, len(events))
	}
}

func newEnumServer(t *testing.T, conn *enumConn) *Server {
	t.Helper()
	srv := newIdleTestServer(conn.fakeConn, time.Minute)
	srv.tabHook = conn
	return srv
}

const (
	enumOldID = target.ID("AAAA00000000000000000000000A1B2C")
	enumNewID = target.ID("BBBB0000000000000000000000004D5E")
)

// Every RPC entry point that enumerates targets is parked after it obtained a
// snapshot without the new tab; tab_new then completes; the stale snapshot is
// applied last.
func TestStaleEnumerationKeepsTabCreatedMeanwhile(t *testing.T) {
	cases := []struct {
		name   string
		method string
		params func(oldShort string) map[string]any
		// wantErr is true when the enumerating request itself must still fail
		// (unknown tab id), independently of the tab created meanwhile.
		wantErr bool
	}{
		{name: "tab_list", method: protocol.MethodTabList},
		{name: "tab_focus", method: protocol.MethodTabFocus},
		{name: "tab_select", method: protocol.MethodTabSelect, params: func(old string) map[string]any {
			return map[string]any{"tab": old}
		}},
		{name: "resolveTab fallback (goto unknown tab)", method: protocol.MethodGoto, params: func(string) map[string]any {
			return map[string]any{"tab": "feed", "url": "about:blank"}
		}, wantErr: true},
		{name: "resolveTab fallback (eval unknown tab)", method: protocol.MethodEval, params: func(string) map[string]any {
			return map[string]any{"tab": "feed", "script": "1"}
		}, wantErr: true},
		{name: "close fallback (tab_close unknown tab)", method: protocol.MethodTabClose, params: func(string) map[string]any {
			return map[string]any{"tab": "feed"}
		}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := newEnumConn(enumOldID)
			conn.nextIDs = []target.ID{enumNewID}
			srv := newEnumServer(t, conn)

			enumMustRPC(t, srv, protocol.MethodTabList, nil)
			oldShort, ok := srv.tabs.ShortForTarget(enumOldID)
			if !ok {
				t.Fatal("precondition: existing tab not registered")
			}

			entered, release := conn.holdNextSnapshot()
			defer release()
			var params map[string]any
			if tc.params != nil {
				params = tc.params(oldShort)
			}
			type outcome struct {
				reply enumRPCReply
				err   error
			}
			done := make(chan outcome, 1)
			go func() {
				reply, err := enumRPC(srv, tc.method, params)
				done <- outcome{reply, err}
			}()
			enumAwait(t, entered, tc.method+" to take its target snapshot")

			short := enumTabNew(t, srv)
			enumPushNetwork(t, srv, enumNewID)
			assertTabIntact(t, srv, short, enumNewID)
			if t.Failed() {
				t.Fatal("precondition: tab_new did not leave the tab registered and managed")
			}

			release()
			var got outcome
			select {
			case got = <-done:
			case <-time.After(enumTestWait):
				t.Fatalf("%s did not finish", tc.method)
			}
			if got.err != nil {
				t.Fatal(got.err)
			}
			if tc.wantErr != (got.reply.Error != nil) {
				t.Fatalf("%s error = %v, wantErr %v", tc.method, got.reply.Error, tc.wantErr)
			}

			assertTabIntact(t, srv, short, enumNewID)
			if _, ok := srv.tabs.Lookup(oldShort); !ok {
				t.Error("pre-existing tab was removed")
			}
			if !conn.has(enumNewID) {
				t.Error("new tab was closed in the browser")
			}
			// The id handed out by tab_new keeps working without any re-enumeration.
			conn.setListErr(errors.New("enumeration must not be needed"))
			enumMustRPC(t, srv, protocol.MethodEval, map[string]any{"tab": short, "script": "1"})
			conn.setListErr(nil)
			assertTabIntact(t, srv, short, enumNewID)

			// A fresh enumeration lists it and keeps it.
			tabs, _ := enumMustRPC(t, srv, protocol.MethodTabList, nil)["tabs"].([]any)
			if len(tabs) != 2 {
				t.Errorf("fresh tab_list returned %d tabs, want 2: %v", len(tabs), tabs)
			}
			assertTabIntact(t, srv, short, enumNewID)
		})
	}
}

// The protection is per target: a stale snapshot still removes a target that
// had already disappeared before the request, without touching the newer tab.
func TestStaleEnumerationStillRemovesTargetGoneBeforeRequest(t *testing.T) {
	conn := newEnumConn(enumOldID)
	conn.nextIDs = []target.ID{enumNewID}
	srv := newEnumServer(t, conn)
	enumMustRPC(t, srv, protocol.MethodTabList, nil)
	oldShort, _ := srv.tabs.ShortForTarget(enumOldID)
	srv.tabIdle.MarkManaged(enumOldID)
	enumPushNetwork(t, srv, enumOldID)

	conn.removeExternally(enumOldID)
	entered, release := conn.holdNextSnapshot()
	defer release()
	done := make(chan error, 1)
	go func() {
		_, err := enumRPC(srv, protocol.MethodTabList, nil)
		done <- err
	}()
	enumAwait(t, entered, "tab_list to take its target snapshot")
	short := enumTabNew(t, srv)
	enumPushNetwork(t, srv, enumNewID)
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(enumTestWait):
		t.Fatal("tab_list did not finish")
	}

	assertTabIntact(t, srv, short, enumNewID)
	if _, ok := srv.tabs.Lookup(oldShort); ok {
		t.Error("vanished target still registered")
	}
	if _, ok := srv.tabIdle.Snapshot()[enumOldID]; ok {
		t.Error("vanished target still idle-managed")
	}
	if events, _, _ := srv.obsStore.QueryNetwork(enumOldID, 0); len(events) != 0 {
		t.Errorf("vanished target kept %d observation events", len(events))
	}
}

// Real removals must keep working through a fresh enumeration, including the
// empty-list case.
func TestFreshEnumerationRemovesVanishedTargets(t *testing.T) {
	conn := newEnumConn()
	srv := newEnumServer(t, conn)

	shortA := enumTabNew(t, srv)
	shortB := enumTabNew(t, srv)
	tidA, _ := srv.tabs.Lookup(shortA)
	tidB, _ := srv.tabs.Lookup(shortB)
	enumPushNetwork(t, srv, tidA)
	enumPushNetwork(t, srv, tidB)
	unlock, ok := srv.lockTab(context.Background(), shortA)
	if !ok {
		t.Fatal("lock failed")
	}
	unlock()

	conn.removeExternally(tidA)
	enumMustRPC(t, srv, protocol.MethodTabList, nil)
	if _, ok := srv.tabs.Lookup(shortA); ok {
		t.Error("vanished tab still registered after fresh tab_list")
	}
	if _, ok := srv.tabIdle.Snapshot()[tidA]; ok {
		t.Error("vanished tab still idle-managed")
	}
	if events, _, _ := srv.obsStore.QueryNetwork(tidA, 0); len(events) != 0 {
		t.Errorf("vanished tab kept %d observation events", len(events))
	}
	srv.tabMuOps.Lock()
	_, lockRetained := srv.tabCDPLocks[shortA]
	srv.tabMuOps.Unlock()
	if lockRetained {
		t.Error("vanished tab kept its keyed lock")
	}
	assertTabIntact(t, srv, shortB, tidB)

	// The resolveTab fallback is a fresh enumeration too: it removes B, and
	// the now-empty snapshot leaves nothing behind.
	conn.removeExternally(tidB)
	reply, err := enumRPC(srv, protocol.MethodGoto, map[string]any{"tab": "feed", "url": "about:blank"})
	if err != nil {
		t.Fatal(err)
	}
	if reply.Error == nil {
		t.Fatal("goto on an unknown tab must fail")
	}
	if _, ok := srv.tabs.Lookup(shortB); ok {
		t.Error("tab missing from an empty fresh snapshot is still registered")
	}
	if left := srv.tabIdle.Snapshot(); len(left) != 0 {
		t.Errorf("idle tracking retained %v after an empty fresh snapshot", left)
	}
	if events, _, _ := srv.obsStore.QueryNetwork(tidB, 0); len(events) != 0 {
		t.Errorf("removed tab kept %d observation events", len(events))
	}
	tabs, _ := enumMustRPC(t, srv, protocol.MethodTabList, nil)["tabs"].([]any)
	if len(tabs) != 0 {
		t.Errorf("tab_list = %v, want empty", tabs)
	}
}

// A failed enumeration changes nothing, whether or not a tab was created while
// it was in flight.
func TestFailedEnumerationLeavesStateUntouched(t *testing.T) {
	conn := newEnumConn(enumOldID)
	conn.nextIDs = []target.ID{enumNewID}
	srv := newEnumServer(t, conn)
	enumMustRPC(t, srv, protocol.MethodTabList, nil)
	oldShort, _ := srv.tabs.ShortForTarget(enumOldID)

	conn.setListErr(errors.New("getTargets failed"))
	entered, release := conn.holdNextSnapshot()
	defer release()
	done := make(chan enumRPCReply, 1)
	go func() {
		reply, _ := enumRPC(srv, protocol.MethodTabList, nil)
		done <- reply
	}()
	enumAwait(t, entered, "tab_list to reach the browser")
	short := enumTabNew(t, srv)
	enumPushNetwork(t, srv, enumNewID)
	release()
	select {
	case reply := <-done:
		if reply.Error == nil {
			t.Fatal("tab_list must report the enumeration failure")
		}
	case <-time.After(enumTestWait):
		t.Fatal("tab_list did not finish")
	}

	for _, call := range []struct {
		method string
		params map[string]any
	}{
		{protocol.MethodTabFocus, nil},
		{protocol.MethodTabSelect, map[string]any{"tab": short}},
		{protocol.MethodGoto, map[string]any{"tab": "feed", "url": "about:blank"}},
		{protocol.MethodTabClose, map[string]any{"tab": "feed"}},
	} {
		reply, err := enumRPC(srv, call.method, call.params)
		if err != nil {
			t.Fatal(err)
		}
		if reply.Error == nil {
			t.Errorf("%s succeeded although enumeration failed", call.method)
		}
	}
	assertTabIntact(t, srv, short, enumNewID)
	if _, ok := srv.tabs.Lookup(oldShort); !ok {
		t.Error("failed enumeration removed the pre-existing tab")
	}
	if !conn.has(enumNewID) || !conn.has(enumOldID) {
		t.Error("failed enumeration closed a tab in the browser")
	}
}

// 200 rounds of concurrent tab_new against every enumerating entry point. Each
// enumeration yields between taking its snapshot and returning it, so snapshots
// routinely predate tabs created in the same round.
func TestConcurrentTabNewAndEnumerationKeepEveryNewTabManaged(t *testing.T) {
	const (
		rounds       = 200
		newPerRound  = 3
		listPerRound = 2
	)
	conn := newEnumConn(enumOldID)
	conn.setAfterSnapshot(func() {
		for i := 0; i < 4; i++ {
			runtime.Gosched()
		}
	})
	srv := newEnumServer(t, conn)
	enumMustRPC(t, srv, protocol.MethodTabList, nil)
	oldShort, _ := srv.tabs.ShortForTarget(enumOldID)

	for round := 0; round < rounds; round++ {
		var (
			wg     sync.WaitGroup
			mu     sync.Mutex
			shorts []string
			errs   []error
		)
		fail := func(err error) {
			mu.Lock()
			errs = append(errs, err)
			mu.Unlock()
		}
		start := make(chan struct{})
		run := func(fn func()) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				fn()
			}()
		}
		for i := 0; i < newPerRound; i++ {
			run(func() {
				reply, err := enumRPC(srv, protocol.MethodTabNew, map[string]any{"silent": true})
				if err == nil && reply.Error != nil {
					err = fmt.Errorf("tab_new: %v", reply.Error)
				}
				if err != nil {
					fail(err)
					return
				}
				short, _ := reply.Result["tab"].(string)
				mu.Lock()
				shorts = append(shorts, short)
				mu.Unlock()
			})
		}
		for i := 0; i < listPerRound; i++ {
			run(func() {
				reply, err := enumRPC(srv, protocol.MethodTabList, nil)
				if err == nil && reply.Error != nil {
					err = fmt.Errorf("tab_list: %v", reply.Error)
				}
				if err != nil {
					fail(err)
				}
			})
		}
		run(func() {
			if _, err := enumRPC(srv, protocol.MethodTabFocus, nil); err != nil {
				fail(err)
			}
		})
		run(func() {
			if _, err := enumRPC(srv, protocol.MethodTabSelect, map[string]any{"tab": oldShort}); err != nil {
				fail(err)
			}
		})
		run(func() {
			// Unknown id: resolveTab falls back to a full enumeration.
			if _, err := enumRPC(srv, protocol.MethodGoto, map[string]any{"tab": "feed", "url": "about:blank"}); err != nil {
				fail(err)
			}
		})
		close(start)
		wg.Wait()

		if len(errs) != 0 {
			t.Fatalf("round %d: %v", round, errs)
		}
		if len(shorts) != newPerRound {
			t.Fatalf("round %d: %d tab_new results, want %d", round, len(shorts), newPerRound)
		}
		managed := srv.tabIdle.Snapshot()
		for _, short := range shorts {
			tid, ok := srv.tabs.Lookup(short)
			if !ok {
				t.Fatalf("round %d: tab %q returned by tab_new is not registered", round, short)
			}
			if _, ok := managed[tid]; !ok {
				t.Fatalf("round %d: tab %q returned by tab_new is not idle-managed", round, short)
			}
		}
		// Closing through the returned id proves it is usable and exercises
		// genuine removal; nothing may linger afterwards.
		for _, short := range shorts {
			enumMustRPC(t, srv, protocol.MethodTabClose, map[string]any{"tab": short})
		}
		tabs, _ := enumMustRPC(t, srv, protocol.MethodTabList, nil)["tabs"].([]any)
		if len(tabs) != 1 {
			t.Fatalf("round %d: %d tabs after closing the round's tabs, want 1", round, len(tabs))
		}
		if left := srv.tabIdle.Snapshot(); len(left) != 0 {
			t.Fatalf("round %d: idle tracking retained closed tabs: %v", round, left)
		}
	}
}

// ---- real browser.Session against the local CDP protocol stub ----

func newStubSessionServer(t *testing.T, st *cdpStub) *Server {
	t.Helper()
	cfg := Config{
		DebuggerURL:         st.url(),
		ListenAddr:          "127.0.0.1:0",
		StateDir:            stateDirDisabled,
		TabIdleTimeout:      time.Minute,
		CDPWatchdogInterval: time.Minute,
		// Also bounds the dirty-resync Target.getTargets the tests hold back.
		CDPWatchdogTimeout: time.Minute,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

func awaitGetTargets(t *testing.T, st *cdpStub, want int) {
	t.Helper()
	deadline := time.After(enumTestWait)
	for {
		select {
		case n := <-st.arrived:
			if n >= want {
				return
			}
		case <-deadline:
			t.Fatalf("Target.getTargets call #%d never reached the stub", want)
		}
	}
}

func containsTarget(ids []target.ID, id target.ID) bool {
	for _, got := range ids {
		if got == id {
			return true
		}
	}
	return false
}

// runTargetSync's dirty resync through a real Session: its Target.getTargets
// reply is held back, tab_new completes and attaches a chromedp child context,
// and only then the stale reply is applied.
func TestDirtyResyncWithStaleSnapshotKeepsNewTab(t *testing.T) {
	st := newCDPStub(t, enumOldID)
	srv := newStubSessionServer(t, st)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sess, err := browser.Connect(ctx, st.url())
	if err != nil {
		t.Fatal(err)
	}
	srv.tabMu.Lock()
	srv.tabLive = sess
	srv.browserState = sessionReady
	srv.tabMu.Unlock()

	enumMustRPC(t, srv, protocol.MethodTabList, nil) // Target.getTargets #1
	awaitGetTargets(t, st, 1)
	oldShort, ok := srv.tabs.ShortForTarget(enumOldID)
	if !ok {
		t.Fatal("precondition: existing tab not registered")
	}

	staleGate := st.holdNextGetTargets() // #2: the stale resync
	nextGate := st.holdNextGetTargets()  // #3: proves #2 has been applied
	go srv.runTargetSync(ctx, sess)
	sess.MarkTargetDirty()
	st.emitInfoChanged(enumOldID) // wake the loop without waiting for its ticker
	awaitGetTargets(t, st, 2)

	st.queueCreateID(enumNewID)
	short := enumTabNew(t, srv)
	// Attach the pooled chromedp child: forgetting it would close the tab.
	enumMustRPC(t, srv, protocol.MethodEval, map[string]any{"tab": short, "script": "1"})
	enumPushNetwork(t, srv, enumNewID)
	assertTabIntact(t, srv, short, enumNewID)
	if t.Failed() {
		t.Fatal("precondition: tab_new did not leave the tab registered and managed")
	}

	// Queue a second resync so its request marks the first one as fully applied.
	// The wake-up event is for the old tab, so it cannot re-register the new one.
	sess.MarkTargetDirty()
	st.emitInfoChanged(enumOldID)
	close(staleGate)
	awaitGetTargets(t, st, 3)

	assertTabIntact(t, srv, short, enumNewID)
	if _, ok := srv.tabs.Lookup(oldShort); !ok {
		t.Error("pre-existing tab was removed")
	}

	close(nextGate)
	enumMustRPC(t, srv, protocol.MethodEval, map[string]any{"tab": short, "script": "1"})
	enumMustRPC(t, srv, protocol.MethodTabList, nil)
	assertTabIntact(t, srv, short, enumNewID)
	if closed := st.closedTargets(); containsTarget(closed, enumNewID) {
		t.Errorf("stale resync closed the new tab in the browser: %v", closed)
	}
	if !st.hasPage(enumNewID) {
		t.Error("new tab no longer exists in the browser")
	}

	// Genuine removal still flows through the same resync path.
	st.mu.Lock()
	st.pages = []target.ID{enumOldID}
	st.mu.Unlock()
	sess.MarkTargetDirty()
	st.emitInfoChanged(enumOldID)
	deadline := time.Now().Add(enumTestWait)
	for {
		_, registered := srv.tabs.Lookup(short)
		_, managed := srv.tabIdle.Snapshot()[enumNewID]
		if !registered && !managed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fresh resync kept a vanished tab (registered=%v managed=%v)", registered, managed)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if events, _, _ := srv.obsStore.QueryNetwork(enumNewID, 0); len(events) != 0 {
		t.Errorf("vanished tab kept %d observation events", len(events))
	}
}

// Startup enumeration through the real connect path: a target announced by a
// lifecycle event while the initial Target.getTargets reply is in flight must
// survive that (older) snapshot.
func TestStartupEnumerationKeepsTargetRegisteredMeanwhile(t *testing.T) {
	st := newCDPStub(t, enumOldID)
	srv := newStubSessionServer(t, st)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gate := st.holdNextGetTargets()
	done := make(chan error, 1)
	go func() {
		srv.tabMu.Lock()
		defer srv.tabMu.Unlock()
		done <- srv.connectBrowserLocked(ctx)
	}()
	awaitGetTargets(t, st, 1)

	st.addPage(enumNewID, true) // Target.targetCreated → runTargetSync registers it
	deadline := time.Now().Add(enumTestWait)
	for {
		if _, ok := srv.tabs.ShortForTarget(enumNewID); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("precondition: lifecycle event did not register the new target")
		}
		time.Sleep(time.Millisecond)
	}
	short, _ := srv.tabs.ShortForTarget(enumNewID)
	enumPushNetwork(t, srv, enumNewID)

	close(gate)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(enumTestWait):
		t.Fatal("connect did not finish")
	}

	if got, ok := srv.tabs.Lookup(short); !ok || got != enumNewID {
		t.Errorf("target registered during startup enumeration was removed (got %q, %v)", got, ok)
	}
	if events, _, _ := srv.obsStore.QueryNetwork(enumNewID, 0); len(events) != 1 {
		t.Errorf("observation buffer has %d events, want 1", len(events))
	}
	if _, ok := srv.tabs.ShortForTarget(enumOldID); !ok {
		t.Error("startup enumeration did not register the pre-existing tab")
	}
	if closed := st.closedTargets(); len(closed) != 0 {
		t.Errorf("startup closed tabs in the browser: %v", closed)
	}
}
