package daemon

// main@d55fbc5 审计复现（只读 overlay，不进入仓库）。
// 每个测试断言“正确行为”；FAIL 即缺陷证据。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/yiplee/go-bb-browser/internal/browser"
	"github.com/yiplee/go-bb-browser/pkg/protocol"
)

type auditConn struct {
	mu       sync.Mutex
	infos    []*target.Info
	nextIDs  []target.ID
	evalErr  error
	snapErr  error
	pageHook func() // 在 PageTargets 取得快照之后、返回之前调用
}

func (c *auditConn) PageTargets() ([]*target.Info, error) {
	c.mu.Lock()
	out := append([]*target.Info(nil), c.infos...)
	hook := c.pageHook
	c.mu.Unlock()
	if hook != nil {
		hook()
	}
	return out, nil
}

func (c *auditConn) CreatePageTarget(u string, _ bool) (target.ID, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.nextIDs) == 0 {
		return "", fmt.Errorf("auditConn: no ids left")
	}
	id := c.nextIDs[0]
	c.nextIDs = c.nextIDs[1:]
	c.infos = append(c.infos, &target.Info{TargetID: id, Type: "page", URL: u})
	return id, nil
}

func (c *auditConn) CloseTarget(id target.ID) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.infos[:0]
	for _, info := range c.infos {
		if info.TargetID != id {
			out = append(out, info)
		}
	}
	c.infos = out
	return nil
}

func (c *auditConn) has(id target.ID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, info := range c.infos {
		if info.TargetID == id {
			return true
		}
	}
	return false
}

func (c *auditConn) Navigate(target.ID, string) error { return nil }
func (c *auditConn) Reload(target.ID) error           { return nil }
func (c *auditConn) Screenshot(target.ID, string) ([]byte, string, error) {
	return []byte{1}, "image/png", nil
}
func (c *auditConn) Eval(target.ID, string) (json.RawMessage, error) {
	if c.evalErr != nil {
		return nil, c.evalErr
	}
	return json.RawMessage(`null`), nil
}
func (c *auditConn) Click(target.ID, string) error        { return nil }
func (c *auditConn) Fill(target.ID, string, string) error { return nil }
func (c *auditConn) Snapshot(target.ID, browser.SnapshotOpts) (string, string, string, map[string]string, error) {
	if c.snapErr != nil {
		return "", "", "", nil, c.snapErr
	}
	return "", "", "", map[string]string{}, nil
}

func newAuditServer(t *testing.T, stateDir string, conn *auditConn) *Server {
	t.Helper()
	cfg := Config{DebuggerURL: "127.0.0.1:9222", ListenAddr: "127.0.0.1:0", StateDir: stateDir, TabIdleTimeout: 5 * time.Minute}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv.tabHook = conn
	srv.fatalCh = make(chan error, 1)
	return srv
}

func auditRPC(t *testing.T, srv *Server, method string, params any) map[string]json.RawMessage {
	t.Helper()
	body := bytes.NewBufferString(rpcReq(method, params, 1))
	req := httptest.NewRequest(http.MethodPost, "/v1", body)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: HTTP %d %s", method, rec.Code, rec.Body.String())
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	return env
}

func auditTabNew(t *testing.T, srv *Server) string {
	t.Helper()
	env := auditRPC(t, srv, protocol.MethodTabNew, map[string]any{"url": "about:blank"})
	var res protocol.TabNewResult
	if err := json.Unmarshal(env["result"], &res); err != nil || res.Tab == "" {
		t.Fatalf("tab_new: %s err=%v", env, err)
	}
	return res.Tab
}

// 页面 JS 异常文本只要含 websocket / eof 等字样，daemon 就把它当成 CDP 传输断开并触发 fatal 退出。
func TestAudit_PageExceptionTextMustNotKillDaemon(t *testing.T) {
	cases := []struct {
		name   string
		method string
		desc   string
	}{
		{"eval_websocket_state", protocol.MethodEval, "InvalidStateError: Failed to execute 'send' on 'WebSocket': Still in CONNECTING state."},
		{"eval_user_error_eof", protocol.MethodEval, "Error: unexpected EOF while parsing feed"},
		{"snapshot_bad_scope_selector", protocol.MethodSnapshot, "SyntaxError: Failed to execute 'querySelector' on 'Document': 'div[data-kind=websocket' is not a valid selector."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 与 chromedp.Evaluate 在页面抛异常时返回的错误类型一致（*runtime.ExceptionDetails），
			// Session.EvalAwaitContext / SnapshotContext 原样向上返回。
			exc := &runtime.ExceptionDetails{Text: "Uncaught", Exception: &runtime.RemoteObject{Description: tc.desc}}
			conn := &auditConn{infos: []*target.Info{{TargetID: "AAAA00000000000000000000000A1B2C", Type: "page"}}, evalErr: exc, snapErr: exc}
			srv := newAuditServer(t, stateDirDisabled, conn)
			defer srv.closeAudit()

			params := map[string]any{"tab": "1b2c", "script": "x()", "selector_scope": "x"}
			env := auditRPC(t, srv, tc.method, params)
			if env["error"] == nil {
				t.Fatalf("expected a JSON-RPC error for the page exception, got %s", env)
			}
			t.Logf("kind=%v (ErrorTransport=%v) isCDPSessionLost=%v", browser.ErrorKindOf(exc), browser.ErrorTransport, isCDPSessionLost(exc))
			select {
			case err := <-srv.fatalCh:
				t.Fatalf("BUG: page-level JS exception made the daemon declare CDP lost and exit: %v", err)
			default:
			}
			if err := srv.ensureBrowserSession(context.Background()); err != nil {
				t.Fatalf("session should stay usable: %v", err)
			}
		})
	}
}

// tab_list（或任何 resolveTab 回退）拿到的过期 Target.getTargets 快照，在 tab_new 完成后才被应用，
// 会把刚创建的受管 tab 从注册表 / 空闲跟踪中删掉。
func TestAudit_StaleTargetListMustNotUnmanageNewTab(t *testing.T) {
	newID := target.ID("BBBB0000000000000000000000004D5E")
	conn := &auditConn{
		infos:   []*target.Info{{TargetID: "AAAA00000000000000000000000A1B2C", Type: "page"}},
		nextIDs: []target.ID{newID},
	}
	srv := newAuditServer(t, stateDirDisabled, conn)
	defer srv.closeAudit()

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	conn.mu.Lock()
	conn.pageHook = func() {
		first := false
		once.Do(func() { first = true })
		if first {
			close(entered)
			<-release
		}
	}
	conn.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		auditRPC(t, srv, protocol.MethodTabList, map[string]any{}) // 快照里没有 newID
	}()
	<-entered
	short := auditTabNew(t, srv) // Target.createTarget 完成并登记为受管
	if _, ok := srv.tabIdle.Snapshot()[newID]; !ok {
		t.Fatal("precondition: tab_new should mark the tab managed")
	}
	close(release)
	<-done

	if _, ok := srv.tabs.Lookup(short); !ok {
		t.Errorf("BUG: tab %q returned by tab_new was dropped from the registry by a stale target list", short)
	}
	if _, ok := srv.tabIdle.Snapshot()[newID]; !ok {
		t.Errorf("BUG: daemon-created tab %q lost its managed flag (will never be idle-closed in this process)", short)
	}
	if !conn.has(newID) {
		t.Errorf("tab unexpectedly closed in browser")
	}
}

// 未知 / 过期 tab id 的请求在 tabCDPLocks 中留下永不回收的条目（key 为客户端可控字符串）。
func TestAudit_UnknownTabRequestsMustNotLeakLockEntries(t *testing.T) {
	conn := &auditConn{infos: []*target.Info{{TargetID: "AAAA00000000000000000000000A1B2C", Type: "page"}}}
	srv := newAuditServer(t, stateDirDisabled, conn)
	defer srv.closeAudit()

	const n = 300
	for i := 0; i < n; i++ {
		env := auditRPC(t, srv, protocol.MethodGoto, map[string]any{"tab": fmt.Sprintf("ghost-%d", i), "url": "https://example.com"})
		if env["error"] == nil {
			t.Fatalf("expected unknown tab error, got %s", env)
		}
	}
	srv.tabMuOps.Lock()
	got := len(srv.tabCDPLocks)
	srv.tabMuOps.Unlock()
	if got != 0 {
		t.Fatalf("BUG: %d lock entries retained after %d requests for unknown tab ids (want 0)", got, n)
	}
}

// 空闲回收（以及浏览器侧关闭）不会把 tab 从持久化 managed 集合移除；重启后同短 id 的用户 tab
// 被当成 daemon 创建的 tab 并被空闲回收关闭。
func TestAudit_IdleClosedTabMustLeavePersistedManagedSet(t *testing.T) {
	dir := t.TempDir()
	managedID := target.ID("AAAA0000000000000000000000001234")
	conn := &auditConn{nextIDs: []target.ID{managedID}}
	srv := newAuditServer(t, dir, conn)

	short := auditTabNew(t, srv)
	srv.auditWG.Wait()
	if _, ok := srv.store.ReplayManagedTabActivity()[short]; !ok {
		t.Fatalf("precondition: %q should be persisted as managed", short)
	}

	srv.tabIdle.MarkManagedAt(managedID, time.Now().Add(-time.Hour))
	srv.closeExpiredTabs(context.Background())
	srv.auditWG.Wait()
	if conn.has(managedID) {
		t.Fatal("precondition: idle sweeper should have closed the tab")
	}
	if _, ok := srv.store.ReplayManagedTabActivity()[short]; ok {
		t.Errorf("BUG: idle-closed tab %q is still in the persisted managed set (never pruned; re-snapshotted on every rotation)", short)
	}
	if err := srv.closeAudit(); err != nil {
		t.Fatal(err)
	}

	// 重启：浏览器里只有一个用户自己打开的 tab，CDP target id 后 4 位相同 → 相同短 id。
	userID := target.ID("CCCC0000000000000000000000001234")
	conn2 := &auditConn{infos: []*target.Info{{TargetID: userID, Type: "page", URL: "https://mail.example/"}}}
	srv2 := newAuditServer(t, dir, conn2)
	defer srv2.closeAudit()
	infos, _ := conn2.PageTargets()
	snaps := srv2.syncTabsFromTargets(infos) // 与 connectBrowserLocked 相同的启动序列
	srv2.reconcileIdleFromLog(snaps)

	if _, ok := srv2.tabIdle.Snapshot()[userID]; ok {
		t.Errorf("BUG: after restart a user-opened tab (short %q) is tracked as daemon-managed because of a stale persisted entry", short)
		srv2.tabIdle.MarkManagedAt(userID, time.Now().Add(-time.Hour))
		srv2.closeExpiredTabs(context.Background())
		if !conn2.has(userID) {
			t.Errorf("BUG(consequence): idle sweeper closed the user's tab %s", userID)
		}
	}
}
