package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/yiplee/go-bb-browser/internal/browser"
	"github.com/yiplee/go-bb-browser/internal/state"
	"github.com/yiplee/go-bb-browser/pkg/protocol"
)

// These tests are about one rule: a request that names a short tab id is tied
// to the full target id the registry holds for it when the request arrives.
// It never runs on, renews, logs against or reads the buffers of a page that
// got the short id later. ownTab is the tab the caller means; userTab is the
// user's page that takes over the short id "1234".

// registerHookTabs registers the injected connection's pages, as the startup
// enumeration and target events do in a running daemon.
func registerHookTabs(t *testing.T, srv *Server) {
	t.Helper()
	infos, err := srv.tabHook.PageTargets()
	if err != nil {
		t.Fatal(err)
	}
	srv.syncTabsFromTargets(infos)
}

// bindConn records which target every operation is run on.
type bindConn struct {
	*fakeConn
	mu  sync.Mutex
	ops []string // "<op>:<target id>"
	// onOp runs inside an operation, before it returns.
	onOp func(op string)
	// onList runs inside PageTargets, before the list is read.
	onList func()
}

func (c *bindConn) note(op string, tid target.ID) {
	c.mu.Lock()
	c.ops = append(c.ops, op+":"+string(tid))
	hook := c.onOp
	c.mu.Unlock()
	if hook != nil {
		hook(op)
	}
}

// opsOn returns the operations run on tid.
func (c *bindConn) opsOn(tid target.ID) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, op := range c.ops {
		if strings.HasSuffix(op, ":"+string(tid)) {
			out = append(out, op)
		}
	}
	return out
}

func (c *bindConn) PageTargets() ([]*target.Info, error) {
	if c.onList != nil {
		c.onList()
	}
	return c.fakeConn.PageTargets()
}
func (c *bindConn) CloseTarget(id target.ID) error {
	c.note("close", id)
	return c.fakeConn.CloseTarget(id)
}
func (c *bindConn) Navigate(id target.ID, url string) error {
	c.note("navigate", id)
	return nil
}
func (c *bindConn) Reload(id target.ID) error {
	c.note("reload", id)
	return nil
}
func (c *bindConn) Screenshot(id target.ID, f string) ([]byte, string, error) {
	c.note("screenshot", id)
	return c.fakeConn.Screenshot(id, f)
}
func (c *bindConn) Eval(id target.ID, s string) (json.RawMessage, error) {
	c.note("eval", id)
	return c.fakeConn.Eval(id, s)
}
func (c *bindConn) Click(id target.ID, _ string) error {
	c.note("click", id)
	return nil
}
func (c *bindConn) Fill(id target.ID, _, _ string) error {
	c.note("fill", id)
	return nil
}
func (c *bindConn) FetchPage(id target.ID, u, m string, h []byte, b string) (json.RawMessage, error) {
	c.note("fetch", id)
	return c.fakeConn.FetchPage(id, u, m, h, b)
}
func (c *bindConn) Snapshot(id target.ID, o browser.SnapshotOpts) (string, string, string, map[string]string, error) {
	c.note("snapshot", id)
	return c.fakeConn.Snapshot(id, o)
}
func (c *bindConn) AppendNetworkRoute(id target.ID, _ browser.NetworkRouteRule) error {
	c.note("route", id)
	return nil
}
func (c *bindConn) RemoveNetworkRoutes(id target.ID, _ string) error {
	c.note("unroute", id)
	return nil
}
func (c *bindConn) ObservationStats(id target.ID, _ browser.ObservationKind) browser.ObservationStats {
	c.note("stats", id)
	return browser.ObservationStats{}
}
func (c *bindConn) DetectForegroundShort([]state.TabSnapshot) (string, bool) {
	c.note("focus", "")
	return "", false
}

// tabEntries lists every RPC that takes a short tab id and the tab operation
// lock. op is the connection call the entry makes on the tab; the clear
// entries only touch the tab's observation buffers.
var tabEntries = []struct {
	method string
	params map[string]any
	op     string
}{
	{protocol.MethodGoto, map[string]any{"url": "https://example.test/"}, "navigate"},
	{protocol.MethodReload, nil, "reload"},
	{protocol.MethodTabClose, nil, "close"},
	{protocol.MethodScreenshot, nil, "screenshot"},
	{protocol.MethodEval, map[string]any{"script": "1"}, "eval"},
	{protocol.MethodClick, map[string]any{"selector": "a"}, "click"},
	{protocol.MethodFill, map[string]any{"selector": "input", "text": "x"}, "fill"},
	{protocol.MethodFetch, map[string]any{"url": "https://example.test/"}, "fetch"},
	{protocol.MethodSnapshot, nil, "snapshot"},
	{protocol.MethodNetworkRoute, map[string]any{"url_pattern": "*"}, "route"},
	{protocol.MethodNetworkUnroute, nil, "unroute"},
	{protocol.MethodNetwork, nil, "stats"},
	{protocol.MethodConsole, nil, "stats"},
	{protocol.MethodErrors, nil, "stats"},
	{protocol.MethodNetworkClear, nil, ""},
	{protocol.MethodConsoleClear, nil, ""},
	{protocol.MethodErrorsClear, nil, ""},
}

func entryReq(method string, params map[string]any) string {
	p := map[string]any{"tab": "1234"}
	for k, v := range params {
		p[k] = v
	}
	return rpcReq(method, p, 7)
}

// userPageActivity is the last-activity time given to the user's page. The
// page is tracked only so that a renewal meant for another tab would show.
var userPageActivity = time.Now().Add(-10 * time.Second)

// trackUserPage gives the user's page state that a misdirected request would
// change: one event in each observation buffer and an idle clock.
func trackUserPage(srv *Server) {
	srv.obsStore.PushNetwork(userTab, 1, json.RawMessage(`{"n":1}`))
	srv.obsStore.PushConsole(userTab, 2, json.RawMessage(`{"c":1}`))
	srv.obsStore.PushError(userTab, 3, json.RawMessage(`{"e":1}`))
	srv.tabIdle.MarkManagedAt(userTab, userPageActivity)
}

// newBindServer returns a daemon whose tab ownTab was created by tab_new and
// has the short id 1234.
func newBindServer(t *testing.T) (*Server, *bindConn, string) {
	t.Helper()
	stateDir := t.TempDir()
	conn := &bindConn{fakeConn: &fakeConn{createID: ownTab}}
	srv := newIdleTestServerWithState(conn.fakeConn, time.Minute, stateDir)
	srv.tabHook = conn
	rec := postRPC(t, srv, rpcReq(protocol.MethodTabNew, map[string]any{}, 1))
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"tab":"1234"`)) {
		t.Fatalf("test setup: tab_new = %s", rec.Body.String())
	}
	return srv, conn, stateDir
}

// replaceWithUserPage destroys ownTab and lets the user's page take over 1234,
// as Target.targetDestroyed and Target.targetCreated would.
func replaceWithUserPage(t *testing.T, srv *Server, conn *bindConn) {
	t.Helper()
	srv.removeTargetState(ownTab)
	conn.fakeConn.infos = pageInfos(userTab)
	if short := srv.tabs.RegisterPageTarget(userTab); short != "1234" {
		t.Fatalf("test setup: user page short id = %q, want 1234", short)
	}
	trackUserPage(srv)
}

// checkUserPageUntouched fails if anything was run on, logged against, renewed
// for or cleared from the user's page.
func checkUserPageUntouched(t *testing.T, srv *Server, conn *bindConn, stateDir string) {
	t.Helper()
	if ops := conn.opsOn(userTab); len(ops) != 0 {
		t.Errorf("operations ran on the user's page: %v", ops)
	}
	if got := srv.tabIdle.Snapshot()[userTab]; !got.Equal(userPageActivity) {
		t.Errorf("user page idle clock = %v, want it left at %v", got, userPageActivity)
	}
	n, _, _ := srv.obsStore.QueryNetwork(userTab, 0)
	c, _, _ := srv.obsStore.QueryConsole(userTab, 0)
	e, _, _ := srv.obsStore.QueryErrors(userTab, 0)
	if len(n) != 1 || len(c) != 1 || len(e) != 1 {
		t.Errorf("user page buffers network/console/errors = %d/%d/%d events, want 1/1/1", len(n), len(c), len(e))
	}
	drainRPCLog(t, srv)
	for _, rec := range readRPCLog(t, stateDir) {
		if rec.Target == string(userTab) {
			t.Errorf("log record %s (ok=%v) names the user's page", rec.Action, rec.OK)
		}
	}
}

func wantUnknownTab(t *testing.T, method string, body []byte) {
	t.Helper()
	if !bytes.Contains(body, []byte(`"unknown tab id"`)) || bytes.Contains(body, []byte(`"result"`)) {
		t.Errorf("%s = %s, want an unknown tab id error", method, body)
	}
}

// A short id with no registered tab fails at once. The request used to ask the
// browser for its targets and adopt whatever page had the id by then: a page
// opened after the request arrived, which the caller cannot have meant.
func TestUnknownShortIDIsNotResolvedByEnumeration(t *testing.T) {
	methods := []string{protocol.MethodTabSelect}
	params := map[string]map[string]any{}
	for _, e := range tabEntries {
		methods = append(methods, e.method)
		params[e.method] = e.params
	}
	for name, browserPages := range map[string][]target.ID{
		"zero tabs":                           nil,
		"user page opened after the request":  {userTab},
		"tab closed earlier, user page since": {userTab},
	} {
		for _, method := range methods {
			t.Run(name+"/"+method, func(t *testing.T) {
				stateDir := t.TempDir()
				conn := &bindConn{fakeConn: &fakeConn{createID: ownTab}}
				srv := newIdleTestServerWithState(conn.fakeConn, time.Minute, stateDir)
				srv.tabHook = conn
				if strings.HasPrefix(name, "tab closed") {
					// The caller's tab existed and was closed; the caller repeats.
					postRPC(t, srv, rpcReq(protocol.MethodTabNew, map[string]any{}, 1))
					postRPC(t, srv, rpcReq(protocol.MethodTabClose, map[string]any{"tab": "1234"}, 2))
					conn.ops = nil
				}
				// What the browser has now, and only an enumeration would tell.
				conn.fakeConn.infos = pageInfos(browserPages...)
				trackUserPage(srv)
				listed := conn.pageCalls.Load()

				rec := postRPC(t, srv, entryReq(method, params[method]))
				wantUnknownTab(t, method, rec.Body.Bytes())
				if n := conn.pageCalls.Load() - listed; n != 0 {
					t.Errorf("the browser was asked for its targets %d time(s)", n)
				}
				if tid, ok := srv.tabs.Lookup("1234"); ok {
					t.Errorf("short id 1234 got bound to %s", tid)
				}
				if sel := srv.tabs.Selected(); sel != "" {
					t.Errorf("selected = %q", sel)
				}
				if len(conn.ops) != 0 {
					t.Errorf("operations ran: %v", conn.ops)
				}
				checkUserPageUntouched(t, srv, conn, stateDir)
			})
		}
	}
}

// Every entry runs on the registered tab, by full target id, and logs it.
func TestRegisteredTabOperationRunsOnBoundTarget(t *testing.T) {
	for _, e := range tabEntries {
		t.Run(e.method, func(t *testing.T) {
			srv, conn, stateDir := newBindServer(t)
			rec := postRPC(t, srv, entryReq(e.method, e.params))
			if !bytes.Contains(rec.Body.Bytes(), []byte(`"tab":"1234"`)) || bytes.Contains(rec.Body.Bytes(), []byte(`"error"`)) {
				t.Fatalf("%s = %s", e.method, rec.Body.String())
			}
			if e.op != "" && !slices.Equal(conn.ops, []string{e.op + ":" + string(ownTab)}) {
				t.Errorf("operations = %v, want %s on %s", conn.ops, e.op, ownTab)
			}
			drainRPCLog(t, srv)
			recs := readRPCLog(t, stateDir)
			if last := recs[len(recs)-1]; last.Action != e.method || !last.OK || last.Target != string(ownTab) {
				t.Errorf("log record = %s ok=%v target=%q, want %s on %s", last.Action, last.OK, last.Target, e.method, ownTab)
			}
		})
	}
}

// A request waits for the tab's operation lock. Meanwhile its tab is destroyed
// and the user's page gets the short id. The request used to look the short id
// up only once it had the lock, and so ran on the user's page.
func TestQueuedOperationIsNotRedirectedToShortIDSuccessor(t *testing.T) {
	for _, e := range tabEntries {
		t.Run(e.method, func(t *testing.T) {
			srv, conn, stateDir := newBindServer(t)
			unlock, ok := srv.lockTab(context.Background(), "1234") // an operation in flight on the tab
			if !ok {
				t.Fatal("test setup: tab lock")
			}
			queued := make(chan *httptest.ResponseRecorder, 1)
			go func() { queued <- postRPC(t, srv, entryReq(e.method, e.params)) }()
			waitFor(t, "the request to queue for the tab lock", func() bool { return tabLockWaiters(srv, "1234") == 2 })

			replaceWithUserPage(t, srv, conn)
			unlock()
			rec := <-queued
			wantUnknownTab(t, e.method, rec.Body.Bytes())
			if len(conn.ops) != 0 {
				t.Errorf("operations ran: %v", conn.ops)
			}
			if tid, ok := srv.tabs.Lookup("1234"); !ok || tid != userTab {
				t.Errorf("user page registration = %q %v", tid, ok)
			}
			checkUserPageUntouched(t, srv, conn, stateDir)
		})
	}
}

// The operation succeeds on its tab, and before it is recorded the tab is gone
// and the user's page has the short id. Renewal and the log record used to look
// the short id up again and credited the user's page.
func TestSuccessfulOperationIsRecordedForExecutedTarget(t *testing.T) {
	for _, e := range tabEntries {
		if e.op == "" || e.method == protocol.MethodTabClose {
			// The clear entries make no call between binding and recording, and
			// tab_close has its own record (TestQueuedTabCloseDoesNotClose...).
			continue
		}
		t.Run(e.method, func(t *testing.T) {
			srv, conn, stateDir := newBindServer(t)
			var once sync.Once
			conn.onOp = func(op string) {
				if op == e.op {
					once.Do(func() { replaceWithUserPage(t, srv, conn) })
				}
			}
			rec := postRPC(t, srv, entryReq(e.method, e.params))
			if !bytes.Contains(rec.Body.Bytes(), []byte(`"result"`)) {
				t.Fatalf("%s = %s", e.method, rec.Body.String())
			}
			if ops := conn.opsOn(ownTab); len(ops) != 1 {
				t.Errorf("operations on the bound tab = %v", ops)
			}
			checkUserPageUntouched(t, srv, conn, stateDir)
			recs := readRPCLog(t, stateDir)
			if last := recs[len(recs)-1]; last.Action != e.method || !last.OK || last.Target != string(ownTab) {
				t.Errorf("log record = %s ok=%v target=%q, want %s on %s", last.Action, last.OK, last.Target, e.method, ownTab)
			}
		})
	}

	// tab_focus has no tab parameter: its activity belongs to the target its
	// enumeration reported.
	t.Run(protocol.MethodTabFocus, func(t *testing.T) {
		srv, conn, stateDir := newBindServer(t)
		conn.onOp = func(op string) {
			if op == "focus" {
				replaceWithUserPage(t, srv, conn)
			}
		}
		rec := postRPC(t, srv, rpcReq(protocol.MethodTabFocus, map[string]any{}, 7))
		if !bytes.Contains(rec.Body.Bytes(), []byte(`"result"`)) {
			t.Fatalf("tab_focus = %s", rec.Body.String())
		}
		checkUserPageUntouched(t, srv, conn, stateDir)
		recs := readRPCLog(t, stateDir)
		if last := recs[len(recs)-1]; last.Action != protocol.MethodTabFocus || last.Target != string(ownTab) {
			t.Errorf("log record = %s target=%q, want tab_focus on %s", last.Action, last.Target, ownTab)
		}
	})
}

// tab_select checks with the browser that its tab still exists. If the tab is
// gone and the enumeration finds the user's page under the same short id, that
// page must not be selected in its place.
func TestTabSelectDoesNotSelectShortIDSuccessor(t *testing.T) {
	srv, conn, stateDir := newBindServer(t)
	var once sync.Once
	conn.onList = func() { once.Do(func() { replaceWithUserPage(t, srv, conn) }) }
	rec := postRPC(t, srv, entryReq(protocol.MethodTabSelect, nil))
	wantUnknownTab(t, protocol.MethodTabSelect, rec.Body.Bytes())
	// tab_new had selected the caller's tab; with it gone nothing is selected.
	if sel := srv.tabs.Selected(); sel != "" {
		t.Errorf("selected = %q: the user's page was selected", sel)
	}
	checkUserPageUntouched(t, srv, conn, stateDir)
}

func TestTabSelectRegisteredTab(t *testing.T) {
	srv, _, stateDir := newBindServer(t)
	rec := postRPC(t, srv, entryReq(protocol.MethodTabSelect, nil))
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"tab":"1234"`)) || srv.tabs.Selected() != "1234" {
		t.Fatalf("tab_select = %s, selected %q", rec.Body.String(), srv.tabs.Selected())
	}
	drainRPCLog(t, srv)
	recs := readRPCLog(t, stateDir)
	if last := recs[len(recs)-1]; last.Action != protocol.MethodTabSelect || last.Target != string(ownTab) {
		t.Errorf("log record = %s target=%q", last.Action, last.Target)
	}
}

// A request cancelled, or out of time, while it waits for the lock reports a
// timeout as before, runs nothing and records no target.
func TestQueuedOperationCancelledWhileWaitingForLock(t *testing.T) {
	for _, e := range tabEntries {
		t.Run(e.method, func(t *testing.T) {
			srv, conn, stateDir := newBindServer(t)
			unlock, ok := srv.lockTab(context.Background(), "1234")
			if !ok {
				t.Fatal("test setup: tab lock")
			}
			defer unlock()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				req := httptest.NewRequest(http.MethodPost, "/v1", strings.NewReader(entryReq(e.method, e.params))).WithContext(ctx)
				rec := httptest.NewRecorder()
				srv.Handler().ServeHTTP(rec, req)
				done <- rec
			}()
			waitFor(t, "the request to queue for the tab lock", func() bool { return tabLockWaiters(srv, "1234") == 2 })
			cancel()
			rec := <-done
			if !bytes.Contains(rec.Body.Bytes(), []byte(`"request timed out"`)) {
				t.Errorf("%s = %s, want request timed out", e.method, rec.Body.String())
			}
			if len(conn.ops) != 0 {
				t.Errorf("operations ran: %v", conn.ops)
			}
			if tid, ok := srv.tabs.Lookup("1234"); !ok || tid != ownTab {
				t.Errorf("tab registration = %q %v", tid, ok)
			}
			drainRPCLog(t, srv)
			recs := readRPCLog(t, stateDir)
			if last := recs[len(recs)-1]; last.Action != e.method || last.OK || last.Target != "" {
				t.Errorf("log record = %s ok=%v target=%q", last.Action, last.OK, last.Target)
			}
		})
	}
}

// Real HTTP, real browser.Session, loopback CDP. The caller's tab was closed
// and the caller repeats the close. The browser by now has the user's page,
// whose target id ends the same way; the daemon has not been told yet. The
// repeat used to enumerate, bind 1234 to that page and close it.
func TestLiveRepeatedCloseDoesNotAdoptFuturePage(t *testing.T) {
	cdp := newFakeCDP(t, cdpManaged)
	d := startLiveDaemon(t, cdp, t.TempDir(), nil)
	d.waitListening()
	if !d.tabList()["1234"] {
		t.Fatal("test setup: tab 1234 missing")
	}
	d.rpc(protocol.MethodTabClose, map[string]any{"tab": "1234"}, nil)
	if _, ok := d.srv.tabs.Lookup("1234"); ok {
		t.Fatal("test setup: 1234 still registered after tab_close")
	}
	cdp.mu.Lock()
	cdp.targets = append(cdp.targets, cdpUser) // open in the browser, event not delivered
	cdp.mu.Unlock()
	listed := cdp.called("Target.getTargets")

	for _, method := range []string{protocol.MethodTabClose, protocol.MethodEval, protocol.MethodNetwork, protocol.MethodConsoleClear, protocol.MethodTabSelect} {
		_, rpcErr := d.call(method, map[string]any{"tab": "1234", "script": "1"})
		wantUnknownTab(t, method, rpcErr)
	}
	if n := cdp.called("Target.getTargets") - listed; n != 0 {
		t.Errorf("Target.getTargets sent %d time(s) for an unknown short id", n)
	}
	if got := cdp.closedTargets(); !slices.Equal(got, []target.ID{cdpManaged}) {
		t.Errorf("closed targets = %v, want only %s", got, cdpManaged)
	}
	if !cdp.hasTarget(cdpUser) {
		t.Error("the user's page was closed")
	}
	if calls := cdp.callsAbout(cdpUser); len(calls) != 0 {
		t.Errorf("CDP commands addressed the user's page: %v", calls)
	}
	d.stop()
}

// Real HTTP, real browser.Session, loopback CDP. A tab_close holds the lock of
// 1234 while requests of every kind queue behind it. The tab goes away and the
// user's page gets 1234 (target events). None of the queued requests may send
// a command to that page.
func TestLiveQueuedOperationsAreNotSentToShortIDSuccessor(t *testing.T) {
	cdp := newFakeCDP(t, cdpManaged)
	hold := newHandshakeHold()
	cdp.onCloseTarget = func(target.ID) { hold.wait() }
	d := startLiveDaemon(t, cdp, t.TempDir(), nil)
	defer hold.release()
	d.waitListening()

	closeDone := make(chan struct{})
	go func() {
		defer close(closeDone)
		d.call(protocol.MethodTabClose, map[string]any{"tab": "1234"})
	}()
	hold.awaitEntered(t)

	queued := map[string]map[string]any{
		protocol.MethodEval:         {"script": "1"},
		protocol.MethodGoto:         {"url": "https://example.test/"},
		protocol.MethodClick:        {"selector": "a"},
		protocol.MethodFill:         {"selector": "input", "text": "x"},
		protocol.MethodFetch:        {"url": "https://example.test/"},
		protocol.MethodNetwork:      {},
		protocol.MethodConsoleClear: {},
		protocol.MethodTabClose:     {},
	}
	type reply struct {
		method string
		rpcErr json.RawMessage
	}
	replies := make(chan reply, len(queued))
	for method, params := range queued {
		params["tab"] = "1234"
		go func() {
			_, rpcErr := d.call(method, params)
			replies <- reply{method, rpcErr}
		}()
	}
	waitFor(t, "every request to queue for the tab lock", func() bool { return tabLockWaiters(d.srv, "1234") == 1+len(queued) })

	cdp.destroyTarget(cdpManaged)
	waitFor(t, "the tab to be unregistered", func() bool { _, ok := d.srv.tabs.Lookup("1234"); return !ok })
	cdp.openTarget(cdpUser)
	waitFor(t, "the user's page to get 1234", func() bool { tid, ok := d.srv.tabs.Lookup("1234"); return ok && tid == cdpUser })
	d.srv.obsStore.PushConsole(cdpUser, 1, json.RawMessage(`{"c":1}`))

	hold.release()
	<-closeDone
	for range queued {
		r := <-replies
		wantUnknownTab(t, r.method, r.rpcErr)
	}
	if calls := cdp.callsAbout(cdpUser); len(calls) != 0 {
		t.Errorf("CDP commands addressed the user's page: %v", calls)
	}
	if !cdp.hasTarget(cdpUser) {
		t.Error("the user's page was closed")
	}
	if ev, _, _ := d.srv.obsStore.QueryConsole(cdpUser, 0); len(ev) != 1 {
		t.Errorf("user page console buffer has %d events, want 1", len(ev))
	}
	d.stop()
	for _, rec := range readRPCLog(t, d.srv.cfg.StateDir) {
		if rec.Target == string(cdpUser) {
			t.Errorf("log record %s (ok=%v) names the user's page", rec.Action, rec.OK)
		}
	}
}
