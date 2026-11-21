package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/yiplee/go-bb-browser/internal/browser"
	"github.com/yiplee/go-bb-browser/internal/state"
	"github.com/yiplee/go-bb-browser/pkg/protocol"
)

type fakeConn struct {
	infos       []*target.Info
	pageErr     error
	createID    target.ID
	createSeq   int
	createErr   error
	closeErr    error
	navigateErr error
	pageCalls   atomic.Int64
	pingCalls   atomic.Int64
	evalCalls   atomic.Int64
	focusCalls  atomic.Int64
	// onClose runs inside CloseTarget, before it returns: the window in which
	// the daemon can already have handled Target.targetDestroyed.
	onClose func(target.ID)
	// onCreate runs inside CreatePageTarget, before it returns: the window in
	// which Chrome's events about the new target can already have been handled.
	onCreate func(target.ID)
	// createGone makes CreatePageTarget report, as browser.Session does, that
	// the target it created was destroyed before the reply.
	createGone bool
	// closed lists the targets CloseTarget was asked to close.
	closed []target.ID
}

func (f *fakeConn) PageTargets() ([]*target.Info, error) {
	f.pageCalls.Add(1)
	if f.pageErr != nil {
		return nil, f.pageErr
	}
	return f.infos, nil
}

func (f *fakeConn) PingBrowser() error {
	f.pingCalls.Add(1)
	if f.pageErr != nil {
		return f.pageErr
	}
	return nil
}

func (f *fakeConn) CreatePageTarget(initialURL string, _ bool) (target.ID, error) {
	if f.createErr != nil {
		return "", f.createErr
	}
	f.createSeq++
	id := f.createID
	if id == "" {
		id = target.ID(fmt.Sprintf("ABCDEF%04d1234", f.createSeq))
	}
	f.infos = append(f.infos, &target.Info{
		TargetID: id,
		Type:     "page",
		Title:    "",
		URL:      initialURL,
	})
	if f.onCreate != nil {
		f.onCreate(id)
	}
	if f.createGone {
		return id, &browser.CDPError{Kind: browser.ErrorTargetGone, Op: "Target.createTarget", Target: id, Err: browser.ErrCreatedTargetGone}
	}
	return id, nil
}

func (f *fakeConn) CloseTarget(id target.ID) error {
	f.closed = append(f.closed, id)
	if f.onClose != nil {
		f.onClose(id)
	}
	if f.closeErr != nil {
		return f.closeErr
	}
	out := f.infos[:0]
	for _, info := range f.infos {
		if info != nil && info.TargetID != id {
			out = append(out, info)
		}
	}
	f.infos = out
	return nil
}

func (f *fakeConn) Navigate(tabID target.ID, url string) error {
	return f.navigateErr
}

func (f *fakeConn) Reload(target.ID) error {
	return nil
}

func (f *fakeConn) Screenshot(target.ID, string) ([]byte, string, error) {
	return []byte{1, 2, 3}, "image/png", nil
}

func (f *fakeConn) Eval(target.ID, string) (json.RawMessage, error) {
	f.evalCalls.Add(1)
	return json.RawMessage(`null`), nil
}

func (f *fakeConn) Click(target.ID, string) error {
	return nil
}

func (f *fakeConn) Fill(target.ID, string, string) error {
	return nil
}

func (f *fakeConn) FetchPage(target.ID, string, string, []byte, string) (json.RawMessage, error) {
	return json.RawMessage(`{"ok":true,"status":200}`), nil
}

func (f *fakeConn) Snapshot(target.ID, browser.SnapshotOpts) (string, string, string, map[string]string, error) {
	return "t", "https://ex", "snap", map[string]string{"1": `[__bb_snap_ref="1"]`}, nil
}

func (f *fakeConn) AppendNetworkRoute(target.ID, browser.NetworkRouteRule) error { return nil }

func (f *fakeConn) RemoveNetworkRoutes(target.ID, string) error { return nil }

func (f *fakeConn) NetworkRouteCount(target.ID) int { return 0 }

func (f *fakeConn) DetectForegroundShort([]state.TabSnapshot) (string, bool) {
	f.focusCalls.Add(1)
	return "", false
}

func rpcReq(method string, params any, id any) string {
	m := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"id":      id,
	}
	if params != nil {
		m["params"] = params
	}
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestLockTabHonorsContextDeadline(t *testing.T) {
	cfg := Config{DebuggerURL: "127.0.0.1:9222", ListenAddr: "127.0.0.1:0", StateDir: stateDirDisabled}
	srv, err := NewServer(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	unlock, ok := srv.lockTab(context.Background(), "tab")
	if !ok {
		t.Fatal("initial lock failed")
	}
	defer unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, ok := srv.lockTab(ctx, "tab"); ok {
		t.Fatal("lock acquired after context deadline")
	}
}

func TestV1TabListWithoutTabParam(t *testing.T) {
	cfg := Config{DebuggerURL: "127.0.0.1:9222", ListenAddr: "127.0.0.1:0", StateDir: stateDirDisabled}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.tabHook = &fakeConn{infos: []*target.Info{
		{TargetID: "ABCDEF123456", Type: "page", Title: "t", URL: "https://ex"},
	}}

	body := bytes.NewBufferString(rpcReq(protocol.MethodTabList, map[string]any{}, 1))
	req := httptest.NewRequest(http.MethodPost, "/v1", body)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Result protocol.TabListResult `json:"result"`
		ID     int                    `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.ID != 1 {
		t.Fatalf("id %d want 1", env.ID)
	}
	if env.Result.Seq == 0 {
		t.Fatalf("seq %d want > 0", env.Result.Seq)
	}
	if env.Result.Tab != "3456" {
		t.Fatalf("tab %q want 3456 (first when no focus)", env.Result.Tab)
	}
	if len(env.Result.Tabs) != 1 || env.Result.Tabs[0].Tab != "3456" {
		t.Fatalf("tabs %#v", env.Result.Tabs)
	}
}

func TestV1TabListUsesOneBrowserEnumerationOnly(t *testing.T) {
	for _, count := range []int{1, 10, 100} {
		t.Run(fmt.Sprintf("targets_%d", count), func(t *testing.T) {
			infos := make([]*target.Info, 0, count)
			for i := range count {
				infos = append(infos, &target.Info{TargetID: target.ID(fmt.Sprintf("ABCDEF%08d", i)), Type: "page"})
			}
			cfg := Config{DebuggerURL: "127.0.0.1:9222", ListenAddr: "127.0.0.1:0", StateDir: stateDirDisabled}
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			srv, err := NewServer(cfg, nil)
			if err != nil {
				t.Fatal(err)
			}
			fc := &fakeConn{infos: infos}
			srv.tabHook = fc
			postRPC(t, srv, rpcReq(protocol.MethodTabList, map[string]any{}, 1))
			if got := fc.pageCalls.Load(); got != 1 {
				t.Fatalf("GetTargets calls = %d, want 1", got)
			}
			if got := fc.focusCalls.Load(); got != 0 {
				t.Fatalf("focus probes = %d, want 0", got)
			}
			if got := fc.evalCalls.Load(); got != 0 {
				t.Fatalf("eval calls = %d, want 0", got)
			}
		})
	}
}

func TestV1TabNewWithZeroTabsDoesNotEnumerate(t *testing.T) {
	cfg := Config{DebuggerURL: "127.0.0.1:9222", ListenAddr: "127.0.0.1:0", StateDir: stateDirDisabled}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	fc := &fakeConn{}
	srv.tabHook = fc
	postRPC(t, srv, rpcReq(protocol.MethodTabNew, map[string]any{}, 1))
	if got := fc.pageCalls.Load(); got != 0 {
		t.Fatalf("GetTargets calls = %d, want 0", got)
	}
}

func TestV1TabFocus(t *testing.T) {
	cfg := Config{DebuggerURL: "127.0.0.1:9222", ListenAddr: "127.0.0.1:0", StateDir: stateDirDisabled}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.tabHook = &fakeConn{infos: []*target.Info{
		{TargetID: "ABCDEF123456", Type: "page", Title: "t", URL: "https://ex"},
	}}

	body := bytes.NewBufferString(rpcReq(protocol.MethodTabFocus, map[string]any{}, 1))
	req := httptest.NewRequest(http.MethodPost, "/v1", body)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Result protocol.TabFocusResult `json:"result"`
		ID     int                     `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.ID != 1 {
		t.Fatalf("id %d want 1", env.ID)
	}
	if env.Result.Seq == 0 {
		t.Fatalf("seq %d want > 0", env.Result.Seq)
	}
	if env.Result.Tab != "3456" {
		t.Fatalf("tab %q want 3456", env.Result.Tab)
	}
	if env.Result.Title != "t" || env.Result.URL != "https://ex" {
		t.Fatalf("meta title=%q url=%q", env.Result.Title, env.Result.URL)
	}
}

func TestV1TabFocusNoTabs(t *testing.T) {
	cfg := Config{DebuggerURL: "127.0.0.1:9222", ListenAddr: "127.0.0.1:0", StateDir: stateDirDisabled}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.tabHook = &fakeConn{infos: []*target.Info{}}

	body := bytes.NewBufferString(rpcReq(protocol.MethodTabFocus, map[string]any{}, 1))
	req := httptest.NewRequest(http.MethodPost, "/v1", body)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var env struct {
		Error *protocol.ResponseError `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Error == nil || env.Error.Code != protocol.CodeInvalidParams {
		t.Fatalf("got %#v", env.Error)
	}
}

func TestV1TabSelectUnknownTab(t *testing.T) {
	cfg := Config{DebuggerURL: "127.0.0.1:9222", ListenAddr: "127.0.0.1:0", StateDir: stateDirDisabled}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.tabHook = &fakeConn{infos: []*target.Info{
		{TargetID: "ABCDEF123456", Type: "page"},
	}}

	body := bytes.NewBufferString(rpcReq(protocol.MethodTabSelect, map[string]string{"tab": "9999"}, 1))
	req := httptest.NewRequest(http.MethodPost, "/v1", body)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var env struct {
		Error *protocol.ResponseError `json:"error"`
		ID    int                     `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Error == nil || env.Error.Message == "" {
		t.Fatal("expected error object")
	}
	if env.Error.Code != protocol.CodeInvalidParams {
		t.Fatalf("code %d", env.Error.Code)
	}
}

func TestV1TabNewSilentPreservesFocus(t *testing.T) {
	cfg := Config{DebuggerURL: "127.0.0.1:9222", ListenAddr: "127.0.0.1:0", StateDir: stateDirDisabled}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.tabHook = &fakeConn{infos: []*target.Info{
		{TargetID: "ABCDEF123456", Type: "page", Title: "first", URL: "https://a"},
	}, createID: "CAFEBABE0001"}

	post := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1", bytes.NewBufferString(body)))
		return rec
	}

	rList := post(rpcReq(protocol.MethodTabList, map[string]any{}, 1))
	if rList.Code != http.StatusOK {
		t.Fatalf("tab_list %d %s", rList.Code, rList.Body.String())
	}
	var listEnv struct {
		Result protocol.TabListResult `json:"result"`
	}
	if err := json.Unmarshal(rList.Body.Bytes(), &listEnv); err != nil {
		t.Fatal(err)
	}
	first := listEnv.Result.Tab
	if first == "" {
		t.Fatal("empty first tab")
	}

	rSel := post(rpcReq(protocol.MethodTabSelect, map[string]string{"tab": first}, 2))
	if rSel.Code != http.StatusOK {
		t.Fatalf("tab_select %d %s", rSel.Code, rSel.Body.String())
	}

	rNew := post(rpcReq(protocol.MethodTabNew, map[string]any{"url": "about:blank", "silent": true}, 3))
	if rNew.Code != http.StatusOK {
		t.Fatalf("tab_new %d %s", rNew.Code, rNew.Body.String())
	}
	var newEnv struct {
		Result protocol.TabNewResult `json:"result"`
	}
	if err := json.Unmarshal(rNew.Body.Bytes(), &newEnv); err != nil {
		t.Fatal(err)
	}
	if newEnv.Result.Tab == "" {
		t.Fatal("empty tab from tab_new")
	}
	if newEnv.Result.Tab == first {
		t.Fatalf("silent tab_new returned same tab %q", first)
	}

	rList2 := post(rpcReq(protocol.MethodTabList, map[string]any{}, 4))
	if rList2.Code != http.StatusOK {
		t.Fatalf("tab_list2 %d %s", rList2.Code, rList2.Body.String())
	}
	var list2 struct {
		Result protocol.TabListResult `json:"result"`
	}
	if err := json.Unmarshal(rList2.Body.Bytes(), &list2); err != nil {
		t.Fatal(err)
	}
	if list2.Result.Focus != first {
		t.Fatalf("focus %q want %q after silent tab_new", list2.Result.Focus, first)
	}
}

func TestV1WorkflowTabNewGotoClose(t *testing.T) {
	cfg := Config{DebuggerURL: "127.0.0.1:9222", ListenAddr: "127.0.0.1:0", StateDir: stateDirDisabled}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.tabHook = &fakeConn{
		infos:    []*target.Info{},
		createID: "CAFEBABE0001",
	}

	post := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1", bytes.NewBufferString(body)))
		return rec
	}

	rNew := post(rpcReq(protocol.MethodTabNew, map[string]string{"url": "about:blank"}, 1))
	if rNew.Code != http.StatusOK {
		t.Fatalf("tab_new %d %s", rNew.Code, rNew.Body.String())
	}
	var newEnv struct {
		Result protocol.TabNewResult `json:"result"`
	}
	if err := json.Unmarshal(rNew.Body.Bytes(), &newEnv); err != nil {
		t.Fatal(err)
	}
	if newEnv.Result.Tab == "" {
		t.Fatal("empty tab from tab_new")
	}

	rGoto := post(rpcReq(protocol.MethodGoto, map[string]string{
		"tab": newEnv.Result.Tab,
		"url": "https://example.com",
	}, 2))
	if rGoto.Code != http.StatusOK {
		t.Fatalf("goto %d %s", rGoto.Code, rGoto.Body.String())
	}

	rReload := post(rpcReq(protocol.MethodReload, map[string]string{"tab": newEnv.Result.Tab}, 3))
	if rReload.Code != http.StatusOK {
		t.Fatalf("reload %d %s", rReload.Code, rReload.Body.String())
	}

	rClose := post(rpcReq(protocol.MethodTabClose, map[string]string{"tab": newEnv.Result.Tab}, 4))
	if rClose.Code != http.StatusOK {
		t.Fatalf("tab_close %d %s", rClose.Code, rClose.Body.String())
	}
}

func TestV1SeqMonotonicAcrossCalls(t *testing.T) {
	cfg := Config{DebuggerURL: "127.0.0.1:9222", ListenAddr: "127.0.0.1:0", StateDir: stateDirDisabled}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.tabHook = &fakeConn{
		infos: []*target.Info{
			{TargetID: "ABCDEF123456", Type: "page"},
		},
	}

	do := func(body string) uint64 {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1", bytes.NewBufferString(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d %s", rec.Code, rec.Body.String())
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		var inner struct {
			Seq uint64 `json:"seq"`
		}
		if err := json.Unmarshal(m["result"], &inner); err != nil {
			t.Fatal(err)
		}
		return inner.Seq
	}

	a := do(rpcReq(protocol.MethodTabList, map[string]any{}, 1))
	b := do(rpcReq(protocol.MethodTabSelect, map[string]string{"tab": "3456"}, 2))
	c := do(rpcReq(protocol.MethodTabList, map[string]any{}, 3))
	if !(a < b && b < c) {
		t.Fatalf("seq not increasing: %d %d %d", a, b, c)
	}
}

func TestV1UnknownMethod(t *testing.T) {
	cfg := Config{DebuggerURL: "127.0.0.1:9222", ListenAddr: "127.0.0.1:0", StateDir: stateDirDisabled}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.tabHook = &fakeConn{infos: []*target.Info{}}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1",
		bytes.NewBufferString(`{"jsonrpc":"2.0","method":"nope","id":1}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var env struct {
		Error *protocol.ResponseError `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Error == nil || env.Error.Code != protocol.CodeMethodNotFound {
		t.Fatalf("got %#v", env.Error)
	}
}

func TestV1MissingID(t *testing.T) {
	cfg := Config{DebuggerURL: "127.0.0.1:9222", ListenAddr: "127.0.0.1:0", StateDir: stateDirDisabled}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1",
		bytes.NewBufferString(`{"jsonrpc":"2.0","method":"tab_list"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
}

// staleTargetsConn answers the first enumeration with the target list as it was
// when the call started, but only after release — the shape of a Target.getTargets
// reply that Chrome produced before a concurrent Target.createTarget.
type staleTargetsConn struct {
	*fakeConn
	once     sync.Once
	captured chan struct{}
	release  chan struct{}
}

func (c *staleTargetsConn) PageTargetsContext(context.Context) ([]*target.Info, error) {
	infos := append([]*target.Info(nil), c.fakeConn.infos...)
	c.once.Do(func() {
		close(c.captured)
		<-c.release
	})
	return infos, nil
}

// A sync from an enumeration that predates tab_new must not unregister the new
// tab: that silently dropped it from idle tracking, so it was never auto-closed.
func TestStaleEnumerationDoesNotDropConcurrentTabNew(t *testing.T) {
	conn := &staleTargetsConn{fakeConn: &fakeConn{}, captured: make(chan struct{}), release: make(chan struct{})}
	srv := newIdleTestServer(conn.fakeConn, time.Minute)
	srv.tabHook = conn

	listDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1", bytes.NewBufferString(rpcReq(protocol.MethodTabList, map[string]any{}, 1)))
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		listDone <- rec
	}()
	<-conn.captured

	rec := postRPC(t, srv, rpcReq(protocol.MethodTabNew, map[string]any{}, 2))
	var created struct {
		Result protocol.TabNewResult `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.Result.Tab == "" {
		t.Fatalf("tab_new: %v %s", err, rec.Body.String())
	}
	tid, ok := srv.tabs.Lookup(created.Result.Tab)
	if !ok {
		t.Fatal("new tab not registered")
	}

	close(conn.release)
	listRec := <-listDone
	if listRec.Code != http.StatusOK {
		t.Fatalf("tab_list status %d", listRec.Code)
	}
	// tabs, tab and focus describe one snapshot: the focused new tab is listed.
	var listed struct {
		Result protocol.TabListResult `json:"result"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	res := listed.Result
	if res.Focus != created.Result.Tab || res.Tab != res.Focus || len(res.Tabs) != 1 || res.Tabs[0].Tab != res.Focus {
		t.Fatalf("tab_list = %s, want tabs, tab and focus all naming %s", listRec.Body.String(), created.Result.Tab)
	}

	if got, ok := srv.tabs.Lookup(created.Result.Tab); !ok || got != tid {
		t.Fatal("stale enumeration unregistered the tab created meanwhile")
	}
	if _, ok := srv.tabIdle.Snapshot()[tid]; !ok {
		t.Fatal("stale enumeration dropped the new tab from idle tracking")
	}

	// The next, current enumeration is authoritative again.
	conn.fakeConn.infos = nil
	postRPC(t, srv, rpcReq(protocol.MethodTabList, map[string]any{}, 3))
	if _, ok := srv.tabs.Lookup(created.Result.Tab); ok {
		t.Fatal("tab missing from a current enumeration was kept")
	}
	if len(srv.tabIdle.Snapshot()) != 0 {
		t.Fatal("idle tracking kept a tab missing from a current enumeration")
	}
}

func TestSnapRefSelectorEscapesRef(t *testing.T) {
	for ref, want := range map[string]string{
		"3":  `[__bb_snap_ref="3"]`,
		"@3": `[__bb_snap_ref="3"]`,
		// A trailing backslash used to escape the closing quote.
		`3\`:  `[__bb_snap_ref="3\\"]`,
		`a"b`: `[__bb_snap_ref="a\"b"]`,
		// `\"` used to end the string early, turning the rest into selectors
		// of its own (here: `,body,`).
		`x\"],body,[y="`: `[__bb_snap_ref="x\\\"],body,[y=\""]`,
	} {
		if got := snapRefSelector(ref); got != want {
			t.Errorf("snapRefSelector(%q) = %s, want %s", ref, got, want)
		}
	}
}

// click must hand the escaped selector to the browser, not just build it.
type clickRecorder struct {
	*fakeConn
	selector string
}

func (c *clickRecorder) Click(_ target.ID, selector string) error {
	c.selector = selector
	return nil
}

func TestClickRefIsEscapedInSelector(t *testing.T) {
	conn := &clickRecorder{fakeConn: &fakeConn{infos: []*target.Info{{TargetID: "AAAA00001234", Type: "page"}}}}
	srv := newIdleTestServer(conn.fakeConn, 0)
	srv.tabHook = conn
	postRPC(t, srv, rpcReq(protocol.MethodClick, map[string]any{"tab": "1234", "ref": `x\"],body,[y="`}, 1))
	if want := `[__bb_snap_ref="x\\\"],body,[y=\""]`; conn.selector != want {
		t.Fatalf("selector = %s, want %s", conn.selector, want)
	}
}
