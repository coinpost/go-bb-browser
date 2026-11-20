package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/yiplee/go-bb-browser/internal/browser"
	"github.com/yiplee/go-bb-browser/internal/state"
	"github.com/yiplee/go-bb-browser/internal/store"
	"github.com/yiplee/go-bb-browser/pkg/protocol"
)

func newIdleTestServer(fc *fakeConn, timeout time.Duration) *Server {
	return newIdleTestServerWithState(fc, timeout, stateDirDisabled)
}

func newIdleTestServerWithState(fc *fakeConn, timeout time.Duration, stateDir string) *Server {
	cfg := Config{
		DebuggerURL:      "127.0.0.1:9222",
		ListenAddr:       "127.0.0.1:0",
		TabIdleTimeout:   timeout,
		StateDir:         stateDir,
		IdleStartupGrace: 30 * time.Millisecond,
	}
	if err := cfg.Validate(); err != nil {
		panic(err)
	}
	srv, err := NewServer(cfg, nil)
	if err != nil {
		panic(err)
	}
	srv.tabHook = fc
	return srv
}

func postRPC(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	return rec
}

func drainRPCLog(t *testing.T, srv *Server) {
	t.Helper()
	srv.auditWG.Wait()
}

func TestExternalTargetRemovalClearsAllDaemonState(t *testing.T) {
	srv := newIdleTestServer(&fakeConn{}, time.Minute)
	tid := target.ID("ABCDEF123456")
	short := srv.tabs.RegisterPageTarget(tid)
	srv.tabIdle.MarkManaged(tid)
	seq, err := srv.store.NextSeq()
	if err != nil {
		t.Fatal(err)
	}
	srv.obsStore.PushNetwork(tid, seq, json.RawMessage(`{"url":"https://example.com"}`))
	unlock, ok := srv.lockTab(context.Background(), short)
	if !ok {
		t.Fatal("lock failed")
	}
	unlock()

	srv.removeTargetState(tid)
	if _, ok := srv.tabs.Lookup(short); ok {
		t.Fatal("registry entry retained")
	}
	if _, ok := srv.tabIdle.Snapshot()[tid]; ok {
		t.Fatal("idle state retained")
	}
	if events, _, _ := srv.obsStore.QueryNetwork(tid, 0); len(events) != 0 {
		t.Fatalf("observation events retained: %#v", events)
	}
	srv.tabMuOps.Lock()
	_, lockRetained := srv.tabCDPLocks[short]
	srv.tabMuOps.Unlock()
	if lockRetained {
		t.Fatal("keyed tab lock retained")
	}
}

func TestTabIdlePreExistingTabNotClosed(t *testing.T) {
	fc := &fakeConn{infos: []*target.Info{
		{TargetID: "ABCDEF123456", Type: "page", Title: "t", URL: "https://ex"},
	}}
	srv := newIdleTestServer(fc, 50*time.Millisecond)

	postRPC(t, srv, rpcReq(protocol.MethodTabList, map[string]any{}, 1))
	time.Sleep(60 * time.Millisecond)
	srv.closeExpiredTabs(context.Background())

	if len(fc.infos) != 1 {
		t.Fatalf("pre-existing tab closed: %#v", fc.infos)
	}
}

func TestTabIdleClosesManagedTab(t *testing.T) {
	fc := &fakeConn{infos: []*target.Info{}}
	srv := newIdleTestServer(fc, 50*time.Millisecond)

	postRPC(t, srv, rpcReq(protocol.MethodTabNew, map[string]any{}, 1))
	time.Sleep(60 * time.Millisecond)
	srv.closeExpiredTabs(context.Background())

	if len(fc.infos) != 0 {
		t.Fatalf("managed tab not closed: %#v", fc.infos)
	}
}

func TestTabIdleCloseTargetErrors(t *testing.T) {
	missing := errors.New("No target with given id found")
	for _, tc := range []struct {
		name string
		err  error
		gone bool
	}{
		{"missing target", missing, true},
		{"wrapped missing target", fmt.Errorf("close target: %w", missing), true},
		{"wrapped typed target gone", fmt.Errorf("close target: %w", &browser.CDPError{
			Kind: browser.ErrorTargetGone, Op: "Target.closeTarget", Err: errors.New("target removed while attaching"),
		}), true},
		{"target closed", errors.New("Target closed"), true},
		{"no such target", errors.New("No such target"), true},
		{"other CDP error", errors.New("Internal error (-32603)"), false},
		{"transport error", errors.New("websocket: close 1006"), false},
		{"wrapped timeout", fmt.Errorf("close target: %w", context.DeadlineExceeded), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := &fakeConn{closeErr: tc.err}
			srv := newIdleTestServer(fc, time.Minute)
			var logs bytes.Buffer
			srv.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
			tid := target.ID("ABCDEF123456")
			short := srv.tabs.RegisterPageTarget(tid)
			srv.tabIdle.MarkManagedAt(tid, time.Now().Add(-2*time.Minute))
			seq, err := srv.store.NextSeq()
			if err != nil {
				t.Fatal(err)
			}
			data := json.RawMessage(`{"message":"recorded before cleanup"}`)
			srv.obsStore.PushNetwork(tid, seq, data)
			srv.obsStore.PushConsole(tid, seq, data)
			srv.obsStore.PushError(tid, seq, data)

			for round := 1; round <= 2; round++ {
				srv.closeExpiredTabs(context.Background())
				wantWarnings, wantEvents := round, 1
				if tc.gone {
					wantWarnings, wantEvents = 0, 0
				}
				if got := bytes.Count(logs.Bytes(), []byte("level=WARN")); got != wantWarnings {
					t.Fatalf("round %d: warnings = %d, want %d; logs: %s", round, got, wantWarnings, &logs)
				}
				if _, present := srv.tabs.Lookup(short); present == tc.gone {
					t.Fatalf("round %d: registry present = %v, target gone = %v", round, present, tc.gone)
				}
				expired := srv.tabIdle.Expired(time.Now(), time.Minute)
				if tc.gone {
					if _, present := srv.tabIdle.Snapshot()[tid]; present || len(expired) != 0 {
						t.Fatalf("round %d: removed target still tracked or expired: %v", round, expired)
					}
					srv.tabMuOps.Lock()
					_, retained := srv.tabCDPLocks[short]
					srv.tabMuOps.Unlock()
					if retained {
						t.Fatal("removed target's keyed lock retained")
					}
				} else if len(expired) != 1 || expired[0] != tid {
					t.Fatalf("round %d: failed target not retained for retry: %v", round, expired)
				}
				for name, query := range map[string]func(target.ID, uint64) ([]state.ObsEvent, uint64, uint64){
					"network": srv.obsStore.QueryNetwork,
					"console": srv.obsStore.QueryConsole,
					"errors":  srv.obsStore.QueryErrors,
				} {
					if events, _, _ := query(tid, 0); len(events) != wantEvents {
						t.Fatalf("round %d: %s events = %d, want %d", round, name, len(events), wantEvents)
					}
				}
			}
			if !tc.gone {
				fc.closeErr = nil
				srv.closeExpiredTabs(context.Background())
				if _, present := srv.tabIdle.Snapshot()[tid]; present {
					t.Fatal("successful retry did not forget target")
				}
			}
		})
	}
}

func TestTabCloseRPCErrorSemanticsUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name       string
		registered bool
		code       int
		message    string
	}{
		{"unknown short id", false, protocol.CodeInvalidParams, "unknown tab id"},
		{"registered target gone", true, protocol.CodeServerError, "failed to close tab"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newIdleTestServer(&fakeConn{closeErr: errors.New("No target with given id found")}, time.Minute)
			tab := "unknown"
			if tc.registered {
				tab = srv.tabs.RegisterPageTarget("ABCDEF123456")
			}
			rec := postRPC(t, srv, rpcReq(protocol.MethodTabClose, map[string]any{"tab": tab}, 1))
			var env protocol.Response
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatal(err)
			}
			if env.Error == nil || env.Error.Code != tc.code || env.Error.Message != tc.message || len(env.Result) != 0 {
				t.Fatalf("unexpected tab_close response: %s", rec.Body.String())
			}
		})
	}
}

func TestTabIdleTouchRenews(t *testing.T) {
	fc := &fakeConn{infos: []*target.Info{}}
	srv := newIdleTestServer(fc, 50*time.Millisecond)

	rec := postRPC(t, srv, rpcReq(protocol.MethodTabNew, map[string]any{}, 1))
	var env struct {
		Result protocol.TabNewResult `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	tab := env.Result.Tab

	time.Sleep(30 * time.Millisecond)
	postRPC(t, srv, rpcReq(protocol.MethodGoto, map[string]any{
		"tab": tab,
		"url": "https://example.com",
	}, 2))
	time.Sleep(30 * time.Millisecond)
	srv.closeExpiredTabs(context.Background())

	if len(fc.infos) != 1 {
		t.Fatalf("touched tab closed: %#v", fc.infos)
	}
}

func TestFailedTabRPCDoesNotRenewIdle(t *testing.T) {
	fc := &fakeConn{infos: []*target.Info{}}
	srv := newIdleTestServer(fc, time.Minute)
	rec := postRPC(t, srv, rpcReq(protocol.MethodTabNew, map[string]any{}, 1))
	var env struct {
		Result protocol.TabNewResult `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	tid, ok := srv.tabs.Lookup(env.Result.Tab)
	if !ok {
		t.Fatal("created tab missing from registry")
	}
	old := time.Now().Add(-time.Hour)
	srv.tabIdle.MarkManagedAt(tid, old)
	fc.navigateErr = errors.New("navigation failed")

	postRPC(t, srv, rpcReq(protocol.MethodGoto, map[string]any{
		"tab": env.Result.Tab,
		"url": "https://example.com",
	}, 2))

	if got := srv.tabIdle.Snapshot()[tid]; !got.Equal(old) {
		t.Fatalf("failed RPC renewed idle time: got %v want %v", got, old)
	}
}

func TestSuccessfulTabRPCRenewsIdle(t *testing.T) {
	fc := &fakeConn{infos: []*target.Info{}}
	srv := newIdleTestServer(fc, time.Minute)
	rec := postRPC(t, srv, rpcReq(protocol.MethodTabNew, map[string]any{}, 1))
	var env struct {
		Result protocol.TabNewResult `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	tid, ok := srv.tabs.Lookup(env.Result.Tab)
	if !ok {
		t.Fatal("created tab missing from registry")
	}
	old := time.Now().Add(-time.Hour)
	srv.tabIdle.MarkManagedAt(tid, old)

	postRPC(t, srv, rpcReq(protocol.MethodGoto, map[string]any{
		"tab": env.Result.Tab,
		"url": "https://example.com",
	}, 2))

	if got := srv.tabIdle.Snapshot()[tid]; !got.After(old) {
		t.Fatalf("successful RPC did not renew idle time: got %v want after %v", got, old)
	}
}

func TestTabIdleRechecksExpiryAfterAcquiringTabLock(t *testing.T) {
	tid := target.ID("ABCDEF123456")
	fc := &fakeConn{infos: []*target.Info{
		{TargetID: tid, Type: "page", Title: "t", URL: "https://ex"},
	}}
	srv := newIdleTestServer(fc, time.Minute)
	short := srv.tabs.RegisterPageTarget(tid)
	srv.tabIdle.MarkManagedAt(tid, time.Now().Add(-2*time.Minute))

	unlock, ok := srv.lockTab(context.Background(), short)
	if !ok {
		t.Fatal("failed to hold tab lock")
	}
	done := make(chan struct{})
	go func() {
		srv.closeExpiredTabs(context.Background())
		close(done)
	}()

	deadline := time.Now().Add(time.Second)
	for {
		srv.tabMuOps.Lock()
		refs := 0
		if ent := srv.tabCDPLocks[short]; ent != nil {
			refs = ent.refs
		}
		srv.tabMuOps.Unlock()
		if refs == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("idle cleanup did not wait for tab lock")
		}
		time.Sleep(time.Millisecond)
	}

	srv.touchTabActivity(tid)
	unlock()
	<-done
	if len(fc.infos) != 1 {
		t.Fatalf("tab closed after activity raced with cleanup: %#v", fc.infos)
	}
}

func TestTabIdleDisabled(t *testing.T) {
	fc := &fakeConn{infos: []*target.Info{}}
	srv := newIdleTestServer(fc, 0)

	postRPC(t, srv, rpcReq(protocol.MethodTabNew, map[string]any{}, 1))
	time.Sleep(60 * time.Millisecond)
	srv.closeExpiredTabs(context.Background())

	if len(fc.infos) != 1 {
		t.Fatalf("tab closed while disabled: %#v", fc.infos)
	}
}

func TestTabIdleRepeatedReconcileDoesNotRenew(t *testing.T) {
	stateDir := t.TempDir()
	timeout := 60 * time.Millisecond
	fc := &fakeConn{infos: []*target.Info{}}
	srv := newIdleTestServerWithState(fc, timeout, stateDir)

	postRPC(t, srv, rpcReq(protocol.MethodTabNew, map[string]any{}, 1))
	if len(fc.infos) != 1 {
		t.Fatalf("tab_new: %#v", fc.infos)
	}

	// Simulate repeated target syncs (e.g. tab_list polling) while the tab sits
	// idle. Syncing must not reset the already-tracked idle timer.
	deadline := time.Now().Add(timeout + 40*time.Millisecond)
	for time.Now().Before(deadline) {
		srv.syncTabsFromTargets(fc.infos)
		time.Sleep(15 * time.Millisecond)
	}

	srv.closeExpiredTabs(context.Background())
	if len(fc.infos) != 0 {
		t.Fatalf("idle tab kept alive by repeated reconcile: %#v", fc.infos)
	}
}

func TestConfigValidateTabIdleTimeoutNegative(t *testing.T) {
	cfg := Config{
		DebuggerURL:    "127.0.0.1:9222",
		TabIdleTimeout: -1,
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for negative tab idle timeout")
	}
}

func TestTabIdleRestartRestoresManagedTab(t *testing.T) {
	stateDir := t.TempDir()
	timeout := 50 * time.Millisecond

	fcA := &fakeConn{infos: []*target.Info{}}
	srvA := newIdleTestServerWithState(fcA, timeout, stateDir)
	rec := postRPC(t, srvA, rpcReq(protocol.MethodTabNew, map[string]any{}, 1))
	var env struct {
		Result protocol.TabNewResult `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if len(fcA.infos) != 1 {
		t.Fatalf("tab_new: %#v", fcA.infos)
	}
	drainRPCLog(t, srvA)
	logPath := filepath.Join(stateDir, store.RPCLogFile())
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("expected rpc.jsonl: %v", err)
	}
	if err := srvA.store.Close(); err != nil {
		t.Fatal(err)
	}

	fcB := &fakeConn{infos: []*target.Info{
		{
			TargetID: fcA.infos[0].TargetID,
			Type:     "page",
			Title:    "t",
			URL:      "https://ex",
		},
	}}
	srvB := newIdleTestServerWithState(fcB, timeout, stateDir)
	srvB.syncTabsFromTargets(fcB.infos)

	time.Sleep(60 * time.Millisecond)
	srvB.closeExpiredTabs(context.Background())
	if len(fcB.infos) != 0 {
		t.Fatalf("restored managed tab not closed after idle: %#v", fcB.infos)
	}
}

func TestTabIdleRestartGraceDelaysClose(t *testing.T) {
	stateDir := t.TempDir()
	timeout := 80 * time.Millisecond
	grace := 40 * time.Millisecond
	tabID := target.ID("ABCDEF999999")
	expiredAt := time.Now().Add(-timeout - time.Millisecond)

	logPath := filepath.Join(stateDir, store.RPCLogFile())
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(store.LogRecord{
		Action: protocol.MethodTabNew,
		Body:   json.RawMessage(`{"jsonrpc":"2.0","method":"tab_new","params":{},"id":1}`),
		Tab:    "9999",
		OK:     true,
		Time:   expiredAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	logData := append(line, '\n')
	if err := os.WriteFile(logPath, logData, 0o644); err != nil {
		t.Fatal(err)
	}

	fc := &fakeConn{infos: []*target.Info{
		{TargetID: tabID, Type: "page", Title: "t", URL: "https://ex"},
	}}
	cfg := Config{
		DebuggerURL:      "127.0.0.1:9222",
		ListenAddr:       "127.0.0.1:0",
		TabIdleTimeout:   timeout,
		IdleStartupGrace: grace,
		StateDir:         stateDir,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.tabHook = fc
	srv.syncTabsFromTargets(fc.infos)

	srv.closeExpiredTabs(context.Background())
	if len(fc.infos) != 1 {
		t.Fatalf("tab closed during grace: %#v", fc.infos)
	}

	time.Sleep(grace + timeout/2)
	srv.closeExpiredTabs(context.Background())
	if len(fc.infos) != 0 {
		t.Fatalf("tab not closed after grace+timeout: %#v", fc.infos)
	}
}

func TestUnwritableStateDirErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permission bits")
	}
	readOnlyDir := t.TempDir()
	if err := os.Chmod(readOnlyDir, 0o555); err != nil {
		t.Skip("cannot chmod read-only:", err)
	}
	t.Cleanup(func() { _ = os.Chmod(readOnlyDir, 0o755) })

	cfg := Config{
		DebuggerURL:    "127.0.0.1:9222",
		ListenAddr:     "127.0.0.1:0",
		TabIdleTimeout: 50 * time.Millisecond,
		StateDir:       readOnlyDir,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	if _, err := NewServer(cfg, nil); err == nil {
		t.Fatal("expected error for unwritable state dir")
	}
}

// restartWithUserTab reopens the state dir against a browser whose only tab is
// a user-opened one, and returns the tabs the daemon adopted as managed.
func restartWithUserTab(t *testing.T, stateDir string, userTab target.ID) map[target.ID]time.Time {
	t.Helper()
	fc := &fakeConn{infos: []*target.Info{{TargetID: userTab, Type: "page", URL: "https://user.example"}}}
	srv := newIdleTestServerWithState(fc, time.Minute, stateDir)
	t.Cleanup(func() { _ = srv.closeAudit() })
	srv.syncTabsFromTargets(fc.infos)
	return srv.tabIdle.Snapshot()
}

// A managed tab that goes away without a tab_close RPC must be released in
// rpc.jsonl. Short ids are the trailing hex digits of the CDP target id, so a
// stale entry would make the next daemon adopt — and idle-close — an unrelated
// user tab that happens to derive the same short id.
func TestManagedTabGoneWithoutRPCIsReleasedInLog(t *testing.T) {
	const managed = target.ID("AAAA00001234")
	const userTab = target.ID("BBBB99991234") // different target, same short id

	for name, vanish := range map[string]func(t *testing.T, srv *Server, fc *fakeConn){
		"idle cleanup": func(t *testing.T, srv *Server, fc *fakeConn) {
			srv.tabIdle.MarkManagedAt(managed, time.Now().Add(-time.Hour))
			srv.closeExpiredTabs(context.Background())
			if len(fc.infos) != 0 {
				t.Fatalf("idle tab not closed: %#v", fc.infos)
			}
		},
		"closed in Chrome": func(t *testing.T, srv *Server, fc *fakeConn) {
			srv.removeTargetState(managed) // Target.targetDestroyed
		},
		"dropped from enumeration": func(t *testing.T, srv *Server, fc *fakeConn) {
			fc.infos = nil
			srv.syncTabsFromTargets(fc.infos)
		},
	} {
		t.Run(name, func(t *testing.T) {
			stateDir := t.TempDir()
			fc := &fakeConn{createID: managed}
			srv := newIdleTestServerWithState(fc, time.Minute, stateDir)
			postRPC(t, srv, rpcReq(protocol.MethodTabNew, map[string]any{}, 1))
			drainRPCLog(t, srv)
			if got := srv.store.ReplayManagedTabActivity(); len(got) != 1 {
				t.Fatalf("managed after tab_new = %v", got)
			}

			vanish(t, srv, fc)
			if err := srv.closeAudit(); err != nil {
				t.Fatal(err)
			}

			if got := restartWithUserTab(t, stateDir, userTab); len(got) != 0 {
				t.Fatalf("user tab adopted as managed after restart: %v", got)
			}
		})
	}
}

// Tabs that vanished while the daemon was down (Chrome restart) are released on
// the first successful reconciliation, not kept until a short id collides.
func TestReconcileReleasesManagedTabsGoneAcrossRestart(t *testing.T) {
	stateDir := t.TempDir()
	fcA := &fakeConn{createID: "AAAA00001234"}
	srvA := newIdleTestServerWithState(fcA, time.Minute, stateDir)
	postRPC(t, srvA, rpcReq(protocol.MethodTabNew, map[string]any{}, 1))
	if err := srvA.closeAudit(); err != nil {
		t.Fatal(err)
	}

	// Chrome restarted: the managed tab is gone when the daemon comes back.
	srvB := newIdleTestServerWithState(&fakeConn{}, time.Minute, stateDir)
	srvB.syncTabsFromTargets(nil)
	if err := srvB.closeAudit(); err != nil {
		t.Fatal(err)
	}

	if got := restartWithUserTab(t, stateDir, "BBBB99991234"); len(got) != 0 {
		t.Fatalf("user tab adopted as managed after restart: %v", got)
	}
}

// An explicit tab_close already logs its own record; no synthetic duplicate.
func TestTabCloseRPCWritesSingleCloseRecord(t *testing.T) {
	stateDir := t.TempDir()
	fc := &fakeConn{createID: "AAAA00001234"}
	srv := newIdleTestServerWithState(fc, time.Minute, stateDir)
	postRPC(t, srv, rpcReq(protocol.MethodTabNew, map[string]any{}, 1))
	postRPC(t, srv, rpcReq(protocol.MethodTabClose, map[string]any{"tab": "1234"}, 2))
	if err := srv.closeAudit(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(stateDir, store.RPCLogFile()))
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(data, []byte(`"action":"tab_close"`)); n != 1 {
		t.Fatalf("tab_close records = %d, want 1:\n%s", n, data)
	}
}
