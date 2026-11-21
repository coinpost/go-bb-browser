package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/yiplee/go-bb-browser/pkg/protocol"
)

// These tests are about the requests that decide on a tab from an enumeration:
// tab_select, tab_focus and tab_list. The enumeration and the focus probe take
// time, and while they run the tab can go away and the user's page take over
// its short id "1234". What such a request commits and returns must name the
// tab it looked at, or nothing: never the page that has the short id by then,
// and never a tab that is gone. Names are as in tab_bind_test.go.

// newIdentityServer returns a daemon that knows the given pages from an
// enumeration. Nothing is selected.
func newIdentityServer(t *testing.T, ids ...target.ID) (*Server, *bindConn, string) {
	t.Helper()
	stateDir := t.TempDir()
	conn := &bindConn{fakeConn: &fakeConn{infos: pageInfos(ids...)}}
	srv := newIdleTestServerWithState(conn.fakeConn, time.Minute, stateDir)
	srv.tabHook = conn
	registerHookTabs(t, srv)
	if sel := srv.tabs.Selected(); sel != "" {
		t.Fatalf("test setup: selected = %q", sel)
	}
	return srv, conn, stateDir
}

// takeOverShortID destroys ownTab and lets the user's page get 1234, as
// Target.targetDestroyed and Target.targetCreated would. The browser's target
// list (conn.infos) is left alone: the reply in flight was already read.
func takeOverShortID(t *testing.T, srv *Server) {
	t.Helper()
	srv.removeTargetState(ownTab)
	if short := srv.tabs.RegisterPageTarget(userTab); short != "1234" {
		t.Errorf("test setup: user page short id = %q, want 1234", short)
	}
	trackUserPage(srv)
}

type rpcReply struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string            `json:"message"`
		Data    *protocol.ErrData `json:"data"`
	} `json:"error"`
}

func decodeReply(t *testing.T, body []byte) rpcReply {
	t.Helper()
	var r rpcReply
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("response %s: %v", body, err)
	}
	return r
}

func wantNoPendingEnumeration(t *testing.T, srv *Server) {
	t.Helper()
	if enums, removals := srv.tabs.PendingEnumerations(); enums != 0 || removals != 0 {
		t.Errorf("enumerations in flight = %d, removals remembered = %d; want none", enums, removals)
	}
}

func lastLogRecord(t *testing.T, srv *Server, stateDir string) (action string, ok bool, tid string) {
	t.Helper()
	drainRPCLog(t, srv)
	recs := readRPCLog(t, stateDir)
	if len(recs) == 0 {
		t.Fatal("rpc log is empty")
	}
	last := recs[len(recs)-1]
	return last.Action, last.OK, last.Target
}

// The enumeration's reply still lists the caller's tab. Before it is applied
// the tab is destroyed and the user's page gets 1234. Applying the reply used
// to register the dead tab again, as 91234; tab_select then selected that and
// answered with 1234, which by then was the user's page.
func TestTabSelectAfterStaleEnumerationSelectsNothing(t *testing.T) {
	for name, lateEvent := range map[string]bool{
		"tab destroyed, short id taken over": false,
		// A late targetCreated for the old tab registers it once more, under
		// another short id: the bound target exists, the requested alias is not its.
		"tab registered again under another short id": true,
	} {
		t.Run(name, func(t *testing.T) {
			srv, conn, stateDir := newBindServer(t)
			var once sync.Once
			srv.afterListTargets = func() {
				once.Do(func() {
					takeOverShortID(t, srv)
					if lateEvent {
						srv.tabs.RegisterPageTarget(ownTab)
					}
				})
			}
			rec := postRPC(t, srv, entryReq(protocol.MethodTabSelect, nil))
			wantUnknownTab(t, protocol.MethodTabSelect, rec.Body.Bytes())

			if sel := srv.tabs.Selected(); sel != "" {
				tid, _ := srv.tabs.Lookup(sel)
				t.Errorf("selected = %q (%s), want nothing selected", sel, tid)
			}
			if tid, ok := srv.tabs.Lookup("1234"); !ok || tid != userTab {
				t.Errorf("1234 = %q %v, want the user's page", tid, ok)
			}
			if short, ok := srv.tabs.ShortForTarget(ownTab); ok && !lateEvent {
				t.Errorf("the destroyed tab was registered again as %q", short)
				// What a caller holding that id could then do to a dead tab.
				rec := postRPC(t, srv, rpcReq(protocol.MethodConsoleClear, map[string]any{"tab": short}, 8))
				t.Logf("console_clear %s = %s", short, rec.Body.String())
			}
			if action, ok, tid := lastLogRecord(t, srv, stateDir); action != protocol.MethodTabSelect || ok || tid != "" {
				t.Errorf("log record = %s ok=%v target=%q", action, ok, tid)
			}
			wantNoPendingEnumeration(t, srv)
			checkUserPageUntouched(t, srv, conn, stateDir)
		})
	}
}

func TestTabFocusAnswersFromRegistryAfterProbe(t *testing.T) {
	type want struct {
		tab, focus string
		tid        target.ID
		errData    string // non-empty: the RPC fails with this error data
	}
	for _, tc := range []struct {
		name       string
		pages      []target.ID
		selectOwn  bool   // ownTab is the cached focus when the request arrives
		foreground string // short id the probe reports
		takeOver   bool   // ownTab is replaced by the user's page during the probe
		want       want
	}{
		{name: "probe incomplete, nothing selected: first tab",
			pages: []target.ID{ownTab, otherTab}, want: want{tab: "1234", tid: ownTab}},
		{name: "probe incomplete: cached focus",
			pages: []target.ID{ownTab, otherTab}, selectOwn: true, want: want{tab: "1234", focus: "1234", tid: ownTab}},
		{name: "probe reports a tab",
			pages: []target.ID{ownTab, otherTab}, selectOwn: true, foreground: "abcd", want: want{tab: "abcd", focus: "abcd", tid: otherTab}},

		{name: "only tab replaced, probe incomplete, nothing selected",
			pages: []target.ID{ownTab}, takeOver: true, want: want{errData: "tabs changed"}},
		{name: "only tab replaced, it was the cached focus",
			pages: []target.ID{ownTab}, selectOwn: true, takeOver: true, want: want{errData: "tabs changed"}},
		{name: "only tab replaced, probe reported it",
			pages: []target.ID{ownTab}, foreground: "1234", takeOver: true, want: want{errData: "tabs changed"}},
		{name: "first tab replaced, another listed tab is left",
			pages: []target.ID{ownTab, otherTab}, takeOver: true, want: want{tab: "abcd", tid: otherTab}},
		{name: "reported tab replaced, another listed tab is left",
			pages: []target.ID{ownTab, otherTab}, foreground: "1234", takeOver: true, want: want{tab: "abcd", tid: otherTab}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, conn, stateDir := newIdentityServer(t, tc.pages...)
			if tc.selectOwn {
				srv.tabs.SelectTarget(ownTab)
			}
			conn.foreground = tc.foreground
			conn.onOp = func(op string) {
				if op == "focus" && tc.takeOver {
					takeOverShortID(t, srv)
				}
			}
			rec := postRPC(t, srv, rpcReq(protocol.MethodTabFocus, map[string]any{}, 7))
			reply := decodeReply(t, rec.Body.Bytes())
			action, ok, logged := lastLogRecord(t, srv, stateDir)
			if action != protocol.MethodTabFocus {
				t.Fatalf("last log record is %s", action)
			}

			if tc.want.errData != "" {
				if reply.Error == nil || reply.Error.Message != "no focused tab" || reply.Error.Data == nil || reply.Error.Data.Error != tc.want.errData {
					t.Fatalf("tab_focus = %s, want error no focused tab / %s", rec.Body.String(), tc.want.errData)
				}
				if ok || logged != "" {
					t.Errorf("log record ok=%v target=%q", ok, logged)
				}
				if sel := srv.tabs.Selected(); sel != "" {
					t.Errorf("selected = %q", sel)
				}
			} else {
				var res protocol.TabFocusResult
				if reply.Error != nil || json.Unmarshal(reply.Result, &res) != nil {
					t.Fatalf("tab_focus = %s", rec.Body.String())
				}
				if res.Tab != tc.want.tab || res.Focus != tc.want.focus {
					t.Errorf("tab = %q focus = %q, want %q and %q", res.Tab, res.Focus, tc.want.tab, tc.want.focus)
				}
				// Response, registry and log agree on one full target id.
				if tid, _ := srv.tabs.Lookup(res.Tab); tid != tc.want.tid {
					t.Errorf("returned tab %q is %q in the registry, want %q", res.Tab, tid, tc.want.tid)
				}
				if !ok || logged != string(tc.want.tid) {
					t.Errorf("log record ok=%v target=%q, want %q", ok, logged, tc.want.tid)
				}
				if sel := srv.tabs.Selected(); sel != tc.want.focus {
					t.Errorf("selected = %q, want %q", sel, tc.want.focus)
				}
			}
			wantNoPendingEnumeration(t, srv)
			if tc.takeOver {
				checkUserPageUntouched(t, srv, conn, stateDir)
			}
		})
	}
}

// tab_list removes a tab the browser no longer lists and then clears that
// tab's resources. A page opened and selected in that window is not in the
// tabs of the reply, so it must not be its focus either: the reply used to
// read the focus later and say tabs=[] focus=1234.
func TestTabListFocusAndTabsAreOneSnapshot(t *testing.T) {
	srv, conn, _ := newIdentityServer(t, ownTab)
	conn.fakeConn.infos = nil // closed in the browser, no event yet

	srv.tabMuOps.Lock() // holds the handler in the cleanup after its sync
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- postRPC(t, srv, rpcReq(protocol.MethodTabList, map[string]any{}, 7)) }()
	waitFor(t, "tab_list to apply its enumeration", func() bool { _, ok := srv.tabs.Lookup("1234"); return !ok })
	srv.tabs.RegisterPageTarget(userTab)
	srv.tabs.SelectTarget(userTab)
	srv.tabMuOps.Unlock()

	rec := <-done
	var res protocol.TabListResult
	if reply := decodeReply(t, rec.Body.Bytes()); reply.Error != nil || json.Unmarshal(reply.Result, &res) != nil {
		t.Fatalf("tab_list = %s", rec.Body.String())
	}
	if len(res.Tabs) != 0 || res.Focus != "" || res.Tab != "" {
		t.Errorf("tab_list = %s: focus and tab must be from the listed tabs", rec.Body.String())
	}
	wantNoPendingEnumeration(t, srv)
}

func TestTabListReportsFocusOfItsSnapshot(t *testing.T) {
	srv, _, _ := newIdentityServer(t, ownTab, otherTab)
	list := func() protocol.TabListResult {
		t.Helper()
		rec := postRPC(t, srv, rpcReq(protocol.MethodTabList, map[string]any{}, 7))
		var res protocol.TabListResult
		if reply := decodeReply(t, rec.Body.Bytes()); reply.Error != nil || json.Unmarshal(reply.Result, &res) != nil {
			t.Fatalf("tab_list = %s", rec.Body.String())
		}
		return res
	}
	if res := list(); len(res.Tabs) != 2 || res.Focus != "" || res.Tab != "1234" {
		t.Errorf("nothing selected: %+v", res)
	}
	srv.tabs.SelectTarget(otherTab)
	if res := list(); len(res.Tabs) != 2 || res.Focus != "abcd" || res.Tab != "abcd" {
		t.Errorf("abcd selected: %+v", res)
	}
}

// ctxListConn's enumeration blocks until its request is cancelled.
type ctxListConn struct {
	*bindConn
	entered chan struct{}
}

func (c *ctxListConn) PageTargetsContext(ctx context.Context) ([]*target.Info, error) {
	close(c.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}

// An enumeration that fails or is cancelled gives its token back, and with it
// the removals the registry remembered for it.
func TestFailedEnumerationReleasesItsToken(t *testing.T) {
	requests := map[string]string{
		protocol.MethodTabList:   rpcReq(protocol.MethodTabList, map[string]any{}, 7),
		protocol.MethodTabFocus:  rpcReq(protocol.MethodTabFocus, map[string]any{}, 7),
		protocol.MethodTabSelect: entryReq(protocol.MethodTabSelect, nil),
	}
	held := func(t *testing.T, srv *Server) {
		t.Helper()
		if enums, removals := srv.tabs.PendingEnumerations(); enums != 1 || removals != 1 {
			t.Errorf("during the enumeration: in flight = %d, removals remembered = %d; want 1 and 1", enums, removals)
		}
	}
	for method, body := range requests {
		t.Run("error/"+method, func(t *testing.T) {
			srv, conn, _ := newIdentityServer(t, ownTab)
			conn.fakeConn.pageErr = errors.New("enumeration failed (injected)")
			conn.onList = func() {
				srv.removeTargetState(ownTab) // Target.targetDestroyed while in flight
				held(t, srv)
			}
			rec := postRPC(t, srv, body)
			if !bytes.Contains(rec.Body.Bytes(), []byte(`"failed to list targets"`)) {
				t.Fatalf("%s = %s", method, rec.Body.String())
			}
			wantNoPendingEnumeration(t, srv)
		})
		t.Run("cancelled/"+method, func(t *testing.T) {
			srv, conn, _ := newIdentityServer(t, ownTab)
			blocked := &ctxListConn{bindConn: conn, entered: make(chan struct{})}
			srv.tabHook = blocked
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				req := httptest.NewRequest(http.MethodPost, "/v1", strings.NewReader(body)).WithContext(ctx)
				rec := httptest.NewRecorder()
				srv.Handler().ServeHTTP(rec, req)
				done <- rec
			}()
			<-blocked.entered
			srv.removeTargetState(ownTab)
			held(t, srv)
			cancel()
			rec := <-done
			if bytes.Contains(rec.Body.Bytes(), []byte(`"result"`)) {
				t.Fatalf("%s = %s", method, rec.Body.String())
			}
			if sel := srv.tabs.Selected(); sel != "" {
				t.Errorf("selected = %q", sel)
			}
			wantNoPendingEnumeration(t, srv)
		})
	}

	t.Run("startup enumeration", func(t *testing.T) {
		srv, _, _ := newIdentityServer(t, ownTab)
		enum := srv.tabs.BeginEnumeration()
		srv.removeTargetState(ownTab)
		held(t, srv)
		srv.syncInitialTargets(&dirtyMarker{}, nil, errors.New("enumeration failed (injected)"), enum)
		wantNoPendingEnumeration(t, srv)
	})
}

// A stale enumeration must not bring back a managed tab that was closed while
// it was in flight: not in the registry, not in idle tracking, not on disk.
func TestStaleEnumerationDoesNotRestoreClosedManagedTab(t *testing.T) {
	srv, _, _ := newBindServer(t)
	var once sync.Once
	srv.afterListTargets = func() {
		once.Do(func() { srv.removeTargetState(ownTab) }) // closed in Chrome
	}
	rec := postRPC(t, srv, rpcReq(protocol.MethodTabList, map[string]any{}, 7))
	var res protocol.TabListResult
	if reply := decodeReply(t, rec.Body.Bytes()); reply.Error != nil || json.Unmarshal(reply.Result, &res) != nil {
		t.Fatalf("tab_list = %s", rec.Body.String())
	}
	if len(res.Tabs) != 0 || res.Focus != "" || res.Tab != "" {
		t.Errorf("tab_list = %s, want no tabs", rec.Body.String())
	}
	if short, ok := srv.tabs.ShortForTarget(ownTab); ok {
		t.Errorf("closed tab registered again as %q", short)
	}
	rec = postRPC(t, srv, entryReq(protocol.MethodConsoleClear, nil))
	wantUnknownTab(t, protocol.MethodConsoleClear, rec.Body.Bytes())
	if got := srv.tabIdle.Snapshot(); len(got) != 0 {
		t.Errorf("managed tabs = %v", got)
	}
	drainRPCLog(t, srv)
	if got := srv.store.ReplayManagedTabActivity(); len(got) != 0 {
		t.Errorf("log still lists managed tabs: %v", got)
	}
	wantNoPendingEnumeration(t, srv)
}

// Real HTTP, real browser.Session, loopback CDP. tab_select 1234 enumerates;
// the reply lists the caller's tab. Chrome then destroys that tab and the user
// opens a page that gets 1234, both before the daemon applies the reply. The
// daemon used to register the dead tab again (as 91234), select it and answer
// "tab":"1234": a caller closing the tab it was told it selected closed the
// user's page.
func TestLiveTabSelectDuringShortIDTakeover(t *testing.T) {
	for _, order := range []string{
		"events sent before the reply",        // the reply is held back in the browser
		"events sent after the reply is read", // the handler is held with the reply in hand
	} {
		t.Run(order, func(t *testing.T) {
			cdp := newFakeCDP(t, cdpManaged)
			hold := newHandshakeHold()
			defer hold.release()
			var armed atomic.Bool
			wait := func() {
				if armed.Load() {
					hold.wait()
				}
			}
			d := startLiveDaemonServer(t, cdp, t.TempDir(), nil, func(s *Server) {
				if strings.HasPrefix(order, "events sent after") {
					s.afterListTargets = wait
				}
			})
			d.waitListening()
			if !d.tabList()["1234"] {
				t.Fatal("test setup: tab 1234 missing")
			}
			if strings.HasPrefix(order, "events sent before") {
				cdp.mu.Lock()
				cdp.onGetTargets = wait
				cdp.mu.Unlock()
			}
			armed.Store(true)

			type reply struct{ result, rpcErr json.RawMessage }
			done := make(chan reply, 1)
			go func() {
				result, rpcErr := d.call(protocol.MethodTabSelect, map[string]any{"tab": "1234"})
				done <- reply{result, rpcErr}
			}()
			hold.awaitEntered(t)
			cdp.destroyTarget(cdpManaged)
			waitFor(t, "the tab to be unregistered", func() bool { _, ok := d.srv.tabs.Lookup("1234"); return !ok })
			cdp.openTarget(cdpUser)
			waitFor(t, "the user's page to get 1234", func() bool { tid, ok := d.srv.tabs.Lookup("1234"); return ok && tid == cdpUser })
			select {
			case r := <-done:
				t.Fatalf("test setup: tab_select returned before the takeover: %s %s", r.result, r.rpcErr)
			default:
			}
			armed.Store(false)
			hold.release()

			r := <-done
			if len(r.result) != 0 {
				t.Errorf("tab_select succeeded: %s", r.result)
			}
			wantUnknownTab(t, protocol.MethodTabSelect, r.rpcErr)
			if sel := d.srv.tabs.Selected(); sel != "" {
				tid, _ := d.srv.tabs.Lookup(sel)
				t.Errorf("selected = %q (%s), want nothing selected", sel, tid)
			}
			if short, ok := d.srv.tabs.ShortForTarget(cdpManaged); ok {
				t.Errorf("the destroyed tab was registered again as %q", short)
			}
			if tabs := d.tabList(); len(tabs) != 1 || !tabs["1234"] {
				t.Errorf("tab_list = %v, want only the user's page as 1234", tabs)
			}
			if tid, ok := d.srv.tabs.Lookup("1234"); !ok || tid != cdpUser {
				t.Errorf("1234 = %q %v, want the user's page", tid, ok)
			}
			wantNoPendingEnumeration(t, d.srv)
			checkLiveUserPageUntouched(t, d, cdp)
		})
	}
}

// Real HTTP, real browser.Session, loopback CDP. tab_focus enumerates and
// probes the pages' visibility. During the probe the first tab (1234) is
// destroyed and the user's page gets 1234. The probe is incomplete and nothing
// is selected, and the answer used to fall back to the first row of the old
// snapshot: "tab":"1234", by then the user's page.
func TestLiveTabFocusDuringShortIDTakeover(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pages   []target.ID
		held    target.ID // the probe of this page is held
		wantTab string    // "": the RPC fails
	}{
		{"another listed tab is left", []target.ID{cdpManaged, cdpExisting}, cdpExisting, "aaaa"},
		{"no listed tab is left", []target.ID{cdpManaged}, cdpManaged, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cdp := newFakeCDP(t, tc.pages...)
			d := startLiveDaemon(t, cdp, t.TempDir(), nil)
			d.waitListening()
			if !d.tabList()["1234"] {
				t.Fatal("test setup: tab 1234 missing")
			}
			hold := newHandshakeHold()
			defer hold.release()
			cdp.mu.Lock()
			cdp.onCommand = func(method, sessionID string) {
				if method == "Page.getFrameTree" && sessionID == "session-"+string(tc.held) {
					hold.wait()
				}
			}
			cdp.mu.Unlock()

			type reply struct{ result, rpcErr json.RawMessage }
			done := make(chan reply, 1)
			go func() {
				result, rpcErr := d.call(protocol.MethodTabFocus, map[string]any{})
				done <- reply{result, rpcErr}
			}()
			hold.awaitEntered(t)
			cdp.destroyTarget(cdpManaged)
			waitFor(t, "the tab to be unregistered", func() bool { _, ok := d.srv.tabs.Lookup("1234"); return !ok })
			cdp.openTarget(cdpUser)
			waitFor(t, "the user's page to get 1234", func() bool { tid, ok := d.srv.tabs.Lookup("1234"); return ok && tid == cdpUser })
			hold.release()

			r := <-done
			if tc.wantTab == "" {
				if len(r.result) != 0 || !bytes.Contains(r.rpcErr, []byte(`"no focused tab"`)) || !bytes.Contains(r.rpcErr, []byte(`"tabs changed"`)) {
					t.Errorf("tab_focus = %s %s, want error no focused tab / tabs changed", r.result, r.rpcErr)
				}
			} else {
				var res protocol.TabFocusResult
				if len(r.rpcErr) != 0 || json.Unmarshal(r.result, &res) != nil {
					t.Fatalf("tab_focus = %s %s", r.result, r.rpcErr)
				}
				if res.Tab != tc.wantTab || res.Focus != "" {
					t.Errorf("tab_focus = %s, want tab %q and no focus", r.result, tc.wantTab)
				}
				if tid, _ := d.srv.tabs.Lookup(res.Tab); tid != cdpExisting {
					t.Errorf("returned tab %q is %q in the registry, want %q", res.Tab, tid, cdpExisting)
				}
				// What the caller does with the answer reaches the tab it names.
				d.rpc(protocol.MethodTabClose, map[string]any{"tab": res.Tab}, nil)
				if cdp.hasTarget(cdpExisting) {
					t.Errorf("closing the returned tab %q did not close %s", res.Tab, cdpExisting)
				}
			}
			if sel := d.srv.tabs.Selected(); sel != "" {
				t.Errorf("selected = %q", sel)
			}
			wantNoPendingEnumeration(t, d.srv)
			stateDir := d.srv.cfg.StateDir
			checkLiveUserPageUntouched(t, d, cdp)
			if tc.wantTab != "" {
				var focus string
				for _, rec := range readRPCLog(t, stateDir) {
					if rec.Action == protocol.MethodTabFocus && rec.OK {
						focus = rec.Target
					}
				}
				if focus != string(cdpExisting) {
					t.Errorf("tab_focus log record names %q, want %s", focus, cdpExisting)
				}
			}
		})
	}
}

// checkLiveUserPageUntouched stops the daemon and fails if the user's page was
// closed, sent a CDP command or named in the log.
func checkLiveUserPageUntouched(t *testing.T, d *liveDaemon, cdp *fakeCDP) {
	t.Helper()
	if !cdp.hasTarget(cdpUser) {
		t.Error("the user's page was closed")
	}
	for _, id := range cdp.closedTargets() {
		if id == cdpUser {
			t.Error("Target.closeTarget was sent for the user's page")
		}
	}
	if calls := cdp.callsAbout(cdpUser); len(calls) != 0 {
		t.Errorf("CDP commands addressed the user's page: %v", calls)
	}
	d.stop()
	for _, rec := range readRPCLog(t, d.srv.cfg.StateDir) {
		if rec.Target == string(cdpUser) {
			t.Errorf("log record %s (ok=%v) names the user's page", rec.Action, rec.OK)
		}
	}
}
