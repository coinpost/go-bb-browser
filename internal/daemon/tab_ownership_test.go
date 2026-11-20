package daemon

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/yiplee/go-bb-browser/internal/store"
	"github.com/yiplee/go-bb-browser/pkg/protocol"
)

// ownTab and userTab end in the same four hex digits: the first one registered
// gets the short id "1234". userTab sorts first, which is the order a target
// enumeration registers them in.
const (
	ownTab   = target.ID("BBBB99991234")
	userTab  = target.ID("AAAA00001234")
	otherTab = target.ID("CCCC0000ABCD")
)

func pageInfos(ids ...target.ID) []*target.Info {
	out := make([]*target.Info, 0, len(ids))
	for _, id := range ids {
		out = append(out, &target.Info{TargetID: id, Type: "page"})
	}
	return out
}

func hasInfo(infos []*target.Info, id target.ID) bool {
	return slices.ContainsFunc(infos, func(info *target.Info) bool { return info.TargetID == id })
}

// Idle cleanup decides to close a full target id. Between that decision and the
// CDP call the tab can be destroyed and its short id taken by a user's page;
// closing "whatever holds the short id now" closed that page.
func TestIdleCloseClosesExpiredTargetNotShortIDSuccessor(t *testing.T) {
	for name, closeErr := range map[string]error{
		"chrome reports target gone": errors.New("No target with given id found"),
		"close reply is success":     nil,
	} {
		t.Run(name, func(t *testing.T) {
			stateDir := t.TempDir()
			fc := &fakeConn{createID: ownTab}
			srv := newIdleTestServerWithState(fc, time.Minute, stateDir)
			postRPC(t, srv, rpcReq(protocol.MethodTabNew, map[string]any{}, 1))
			srv.tabIdle.MarkManagedAt(ownTab, time.Now().Add(-time.Hour))

			srv.beforeIdleClose = func(tid target.ID) {
				if tid != ownTab {
					t.Errorf("idle close decided for %s, want %s", tid, ownTab)
				}
				srv.removeTargetState(ownTab) // Target.targetDestroyed
				fc.infos = pageInfos(userTab)
				if short := srv.tabs.RegisterPageTarget(userTab); short != "1234" { // Target.targetCreated
					t.Errorf("test setup: user tab short id = %q, want 1234", short)
				}
				fc.closeErr = closeErr
			}
			srv.closeExpiredTabs(context.Background())

			if !slices.Equal(fc.closed, []target.ID{ownTab}) {
				t.Fatalf("CloseTarget called for %v, want only the expired %s", fc.closed, ownTab)
			}
			if !hasInfo(fc.infos, userTab) {
				t.Fatal("user page was closed")
			}
			if tid, ok := srv.tabs.Lookup("1234"); !ok || tid != userTab {
				t.Fatalf("user tab lost its registration: %q %v", tid, ok)
			}
			if err := srv.closeAudit(); err != nil {
				t.Fatal(err)
			}
			recs := readRPCLog(t, stateDir)
			if got := logActions(recs); got != "tab_new,tab_close" {
				t.Fatalf("rpc log = %s, want tab_new,tab_close", got)
			}
			if recs[1].Target != string(ownTab) {
				t.Fatalf("release names target %q, want %q", recs[1].Target, ownTab)
			}
		})
	}
}

// A removal unregisters the tab and then releases it, both under lifeMu. A
// sweeper that finds an expired tab without a registration is looking at the
// middle of that step; if it forgets the tab itself, the step finds nothing to
// release and writes no tab_close, leaving the tab managed on disk.
func TestSweeperLeavesReleaseToLifecycleOwner(t *testing.T) {
	stateDir := t.TempDir()
	fc := &fakeConn{createID: ownTab}
	srv := newIdleTestServerWithState(fc, time.Minute, stateDir)
	postRPC(t, srv, rpcReq(protocol.MethodTabNew, map[string]any{}, 1))
	srv.tabIdle.MarkManagedAt(ownTab, time.Now().Add(-time.Hour))

	// The removal step, stopped between its two halves.
	srv.lifeMu.Lock()
	short, ok := srv.tabs.RemoveTarget(ownTab)
	if !ok {
		t.Fatal("test setup: tab not registered")
	}
	swept := make(chan struct{})
	go func() {
		defer close(swept)
		srv.closeExpiredTabs(context.Background())
	}()
	select {
	case <-swept:
	case <-time.After(5 * time.Second):
		t.Fatal("sweeper blocked on a tab that is being released")
	}
	if !srv.tabIdle.IsManaged(ownTab) {
		t.Error("sweeper forgot a tab whose release is in progress")
	}
	srv.releaseManagedTabLocked(ownTab, short)
	srv.lifeMu.Unlock()

	if len(fc.closed) != 0 {
		t.Fatalf("sweeper closed %v", fc.closed)
	}
	if err := srv.closeAudit(); err != nil {
		t.Fatal(err)
	}
	if got := logActions(readRPCLog(t, stateDir)); got != "tab_new,tab_close" {
		t.Fatalf("rpc log = %s, want tab_new,tab_close", got)
	}
	if got := managedOnDisk(t, stateDir); len(got) != 0 {
		t.Fatalf("managed on disk = %v", got)
	}
}

// Chrome can deliver targetCreated and targetDestroyed for a new tab before the
// createTarget reply is handled. The reply must not register the dead target
// again and keep it managed with nothing but a tab_new on disk.
func TestTargetDestroyedWhileCreateInFlight(t *testing.T) {
	for name, events := range map[string]func(srv *Server, id target.ID){
		"created then destroyed": func(srv *Server, id target.ID) {
			srv.tabs.RegisterPageTarget(id) // Target.targetCreated
			srv.removeTargetState(id)       // Target.targetDestroyed
		},
		"destroyed only": func(srv *Server, id target.ID) {
			srv.removeTargetState(id)
		},
		"dropped by an enumeration": func(srv *Server, id target.ID) {
			srv.tabs.RegisterPageTarget(id)
			srv.syncTabsFromTargets(nil)
		},
	} {
		t.Run(name, func(t *testing.T) {
			stateDir := t.TempDir()
			fc := &fakeConn{createID: ownTab}
			srv := newIdleTestServerWithState(fc, time.Minute, stateDir)
			fc.onCreate = func(id target.ID) {
				fc.infos = nil
				events(srv, id)
			}
			rec := postRPC(t, srv, rpcReq(protocol.MethodTabNew, map[string]any{}, 1))
			if !bytes.Contains(rec.Body.Bytes(), []byte(`"tab":"1234"`)) {
				t.Fatalf("tab_new: %s", rec.Body.String())
			}

			if tid, ok := srv.tabs.Lookup("1234"); ok {
				t.Fatalf("destroyed target registered again as %s", tid)
			}
			if got := srv.tabIdle.Snapshot(); len(got) != 0 {
				t.Fatalf("destroyed target is managed: %v", got)
			}
			if n := len(srv.creates.gone) + len(srv.creates.inFlight); n != 0 {
				t.Fatalf("create tracking not released: %+v", srv.creates)
			}
			if err := srv.closeAudit(); err != nil {
				t.Fatal(err)
			}
			recs := readRPCLog(t, stateDir)
			if got := logActions(recs); got != "tab_new,tab_close" {
				t.Fatalf("rpc log = %s, want tab_new,tab_close", got)
			}
			for _, rec := range recs {
				if rec.Target != string(ownTab) || !rec.OK {
					t.Fatalf("record %s: target %q ok %v", rec.Action, rec.Target, rec.OK)
				}
			}
			if got := managedOnDisk(t, stateDir); len(got) != 0 {
				t.Fatalf("managed on disk = %v", got)
			}
		})
	}
}

// Removals are remembered only for creates in flight when they happened.
func TestCreateTrackingIsScopedToInFlightCreates(t *testing.T) {
	srv := newIdleTestServer(&fakeConn{}, time.Minute)
	srv.removeTargetState("GONE0001")
	if len(srv.creates.gone) != 0 {
		t.Fatalf("removal remembered with no create in flight: %v", srv.creates.gone)
	}

	first := srv.beginCreate()
	srv.removeTargetState("GONE0002")
	second := srv.beginCreate()
	srv.removeTargetState("GONE0003")
	if len(srv.creates.gone) != 2 {
		t.Fatalf("remembered = %v, want both removals", srv.creates.gone)
	}
	// The first create ends: what only it could have needed is dropped.
	srv.endCreate(first)
	if _, kept := srv.creates.gone["GONE0002"]; kept || len(srv.creates.gone) != 1 {
		t.Fatalf("remembered after first create ended = %v, want only GONE0003", srv.creates.gone)
	}
	srv.lifeMu.Lock()
	gone := srv.endCreateLocked(second, "GONE0003")
	srv.lifeMu.Unlock()
	if !gone || len(srv.creates.gone) != 0 || len(srv.creates.inFlight) != 0 {
		t.Fatalf("gone = %v, tracking = %+v", gone, srv.creates)
	}
}

// previousRun leaves a state dir in which the daemon created ownTab (short id
// 1234) while the user's userTab (01234) was open, and returns its config.
func previousRun(t *testing.T, stateDir string, maxLogBytes int64) {
	t.Helper()
	cfg := Config{DebuggerURL: "127.0.0.1:9222", ListenAddr: "127.0.0.1:0", TabIdleTimeout: time.Minute, StateDir: stateDir, MaxLogBytes: maxLogBytes}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	fc := &fakeConn{createID: ownTab}
	srv.tabHook = fc
	postRPC(t, srv, rpcReq(protocol.MethodTabNew, map[string]any{}, 1))
	fc.infos = append(fc.infos, pageInfos(userTab)...)
	if _, err := srv.listAndSyncTabs(context.Background(), fc); err != nil {
		t.Fatal(err)
	}
	if short, _ := srv.tabs.ShortForTarget(userTab); short != "01234" {
		t.Fatalf("test setup: user tab short id = %q, want 01234", short)
	}
	if maxLogBytes > 0 {
		for i := range 40 {
			postRPC(t, srv, rpcReq(protocol.MethodEval, map[string]any{"tab": "1234", "script": "1"}, i+2))
		}
	}
	if err := srv.closeAudit(); err != nil {
		t.Fatal(err)
	}
}

// restartReordered starts a daemon on stateDir against the same two pages and
// checks that the user's page now holds the short id the daemon's tab had.
func restartReordered(t *testing.T, stateDir string) (*Server, *fakeConn) {
	t.Helper()
	fc := &fakeConn{infos: pageInfos(userTab, ownTab)}
	srv := newIdleTestServerWithState(fc, time.Minute, stateDir)
	t.Cleanup(func() { _ = srv.closeAudit() })
	srv.syncTabsFromTargets(fc.infos)
	if tid, _ := srv.tabs.Lookup("1234"); tid != userTab {
		t.Fatalf("test setup: 1234 is %s, want the user tab to have taken it over", tid)
	}
	return srv, fc
}

// expireAndSweep makes every managed tab idle-expired and runs one sweep.
func expireAndSweep(srv *Server) {
	srv.cfg.TabIdleTimeout = time.Nanosecond
	time.Sleep(time.Millisecond)
	srv.closeExpiredTabs(context.Background())
}

// Nothing changes in the browser across a daemon restart except the order in
// which the two pages are registered. The log's "1234" then names the user's
// page; recovery must follow the full target id instead.
func TestRestartRecoversByFullTargetIDWhenShortIDsReorder(t *testing.T) {
	stateDir := t.TempDir()
	previousRun(t, stateDir, 0)

	srv, fc := restartReordered(t, stateDir)
	if got := srv.tabIdle.Snapshot(); len(got) != 1 || !srv.tabIdle.IsManaged(ownTab) {
		t.Fatalf("managed after restart = %v, want only %s", got, ownTab)
	}
	expireAndSweep(srv)
	if !slices.Equal(fc.closed, []target.ID{ownTab}) {
		t.Fatalf("idle cleanup closed %v, want only %s", fc.closed, ownTab)
	}
	if !hasInfo(fc.infos, userTab) {
		t.Fatal("user page was closed")
	}
}

// Rotation rewrites the managed set into the fresh log. The snapshot has to
// carry the full target id too, or a restart after rotation is back to
// guessing from the short id.
func TestRotationSnapshotKeepsFullTargetIdentity(t *testing.T) {
	stateDir := t.TempDir()
	previousRun(t, stateDir, 2048)
	if _, err := os.Stat(filepath.Join(stateDir, store.RPCLogFile()+".1")); err != nil {
		t.Fatalf("test setup: log did not rotate: %v", err)
	}
	recs := readRPCLog(t, stateDir)
	if len(recs) == 0 || recs[0].Action != protocol.MethodTabNew || recs[0].Target != string(ownTab) || recs[0].Tab != "1234" {
		t.Fatalf("rotated log does not start with the snapshot of %s: %+v", ownTab, recs)
	}

	srv, fc := restartReordered(t, stateDir)
	if got := srv.tabIdle.Snapshot(); len(got) != 1 || !srv.tabIdle.IsManaged(ownTab) {
		t.Fatalf("managed after restart = %v, want only %s", got, ownTab)
	}
	expireAndSweep(srv)
	if !slices.Equal(fc.closed, []target.ID{ownTab}) || !hasInfo(fc.infos, userTab) {
		t.Fatalf("idle cleanup closed %v, want only %s", fc.closed, ownTab)
	}
}

// writeLegacyLog leaves a log as written before records carried a target id:
// one managed tab known by short id only.
func writeLegacyLog(t *testing.T, stateDir, short string) {
	t.Helper()
	line := `{"action":"tab_new","body":{"jsonrpc":"2.0","method":"tab_new","params":{},"id":1},"tab":"` + short +
		`","sender_ip":"127.0.0.1","seq":7,"ok":true,"time":"` + time.Now().UTC().Format(time.RFC3339Nano) + `"}` + "\n"
	if err := os.WriteFile(filepath.Join(stateDir, store.RPCLogFile()), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A short-id-only record cannot say which tab it meant. It is adopted only
// when exactly one open tab can have had that short id; otherwise no tab is
// adopted. Either way the next run no longer depends on the old line.
func TestLegacyShortIDRecordIsAdoptedOnlyWhenUnambiguous(t *testing.T) {
	for name, tc := range map[string]struct {
		present []target.ID
		adopted []target.ID
	}{
		"two tabs can have had the short id": {present: []target.ID{userTab, ownTab, otherTab}},
		"no tab can have had the short id":   {present: []target.ID{otherTab}},
		"exactly one tab":                    {present: []target.ID{ownTab, otherTab}, adopted: []target.ID{ownTab}},
	} {
		t.Run(name, func(t *testing.T) {
			stateDir := t.TempDir()
			writeLegacyLog(t, stateDir, "1234")

			fc := &fakeConn{infos: pageInfos(tc.present...)}
			srv := newIdleTestServerWithState(fc, time.Minute, stateDir)
			srv.syncTabsFromTargets(fc.infos)
			managed := srv.tabIdle.Snapshot()
			if len(managed) != len(tc.adopted) {
				t.Fatalf("managed = %v, want %v", managed, tc.adopted)
			}
			for _, tid := range tc.adopted {
				if !srv.tabIdle.IsManaged(tid) {
					t.Fatalf("managed = %v, want %v", managed, tc.adopted)
				}
			}
			expireAndSweep(srv)
			if !slices.Equal(fc.closed, tc.adopted) {
				t.Fatalf("idle cleanup closed %v, want %v", fc.closed, tc.adopted)
			}
			if err := srv.closeAudit(); err != nil {
				t.Fatal(err)
			}
			// The legacy line is retired; nothing is left to guess from.
			if got := managedOnDisk(t, stateDir); len(got) != 0 {
				t.Fatalf("managed on disk = %v", got)
			}
		})
	}
}

// An adopted legacy entry is written again under its full target id, so the
// run after that recovers it without guessing.
func TestLegacyShortIDRecordIsRewrittenWithFullTargetID(t *testing.T) {
	stateDir := t.TempDir()
	writeLegacyLog(t, stateDir, "1234")

	fc := &fakeConn{infos: pageInfos(ownTab)}
	srv := newIdleTestServerWithState(fc, time.Minute, stateDir)
	srv.syncTabsFromTargets(fc.infos)
	if err := srv.closeAudit(); err != nil {
		t.Fatal(err)
	}
	if got := managedOnDisk(t, stateDir); !slices.Equal(got, []string{string(ownTab)}) {
		t.Fatalf("managed on disk = %v, want %s under its full id", got, ownTab)
	}

	srv2, _ := restartReordered(t, stateDir)
	if got := srv2.tabIdle.Snapshot(); len(got) != 1 || !srv2.tabIdle.IsManaged(ownTab) {
		t.Fatalf("managed after second restart = %v, want only %s", got, ownTab)
	}
}

// An explicit tab_close is audited after its response. A new tab can take the
// short id in between and be logged first; the close record names its target,
// so replay still keeps the new tab managed.
func TestTabCloseRecordAfterShortIDReuseKeepsNewTabManaged(t *testing.T) {
	stateDir := t.TempDir()
	fc := &fakeConn{createID: userTab}
	srv := newIdleTestServerWithState(fc, time.Minute, stateDir)
	postRPC(t, srv, rpcReq(protocol.MethodTabNew, map[string]any{}, 1))

	w := &hookedWriter{ResponseRecorder: httptest.NewRecorder()}
	w.onWrite = func() {
		fc.createID = ownTab
		rec := postRPC(t, srv, rpcReq(protocol.MethodTabNew, map[string]any{}, 2))
		if !bytes.Contains(rec.Body.Bytes(), []byte(`"tab":"1234"`)) {
			t.Errorf("test setup: second tab did not reuse 1234: %s", rec.Body.String())
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/v1", strings.NewReader(rpcReq(protocol.MethodTabClose, map[string]any{"tab": "1234"}, 3)))
	srv.Handler().ServeHTTP(w, req)
	if err := srv.closeAudit(); err != nil {
		t.Fatal(err)
	}
	recs := readRPCLog(t, stateDir)
	if got := logActions(recs); got != "tab_new,tab_new,tab_close" {
		t.Fatalf("rpc log = %s, want tab_new,tab_new,tab_close", got)
	}
	if recs[2].Target != string(userTab) {
		t.Fatalf("tab_close names target %q, want the closed %q", recs[2].Target, userTab)
	}
	if got := managedOnDisk(t, stateDir); !slices.Equal(got, []string{string(ownTab)}) {
		t.Fatalf("managed on disk = %v, want the new tab %s", got, ownTab)
	}
}
