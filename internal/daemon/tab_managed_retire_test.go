package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/yiplee/go-bb-browser/internal/store"
	"github.com/yiplee/go-bb-browser/pkg/protocol"
)

// Two full CDP target ids that derive the same short alias ("1234"): one is
// created by the daemon, the other is a tab the user opened themselves.
const (
	managedTargetA = target.ID("AAAA0000000000000000000000001234")
	userTargetC    = target.ID("CCCC0000000000000000000000001234")
)

func tabNewShort(t *testing.T, srv *Server) string {
	t.Helper()
	rec := postRPC(t, srv, rpcReq(protocol.MethodTabNew, map[string]any{}, 1))
	var env struct {
		Result protocol.TabNewResult `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Result.Tab == "" {
		t.Fatalf("tab_new returned no tab: %s", rec.Body.String())
	}
	return env.Result.Tab
}

// restartIdleServer shuts the old server's store down and brings up a new
// server on the same state dir, running the same startup reconciliation as
// connectBrowserLocked.
func restartIdleServer(t *testing.T, old *Server, fc *fakeConn, stateDir string) *Server {
	t.Helper()
	drainRPCLog(t, old)
	if err := old.closeAudit(); err != nil {
		t.Fatal(err)
	}
	srv := newIdleTestServerWithState(fc, time.Minute, stateDir)
	t.Cleanup(func() { _ = srv.closeAudit() })
	srv.reconcileIdleFromLog(srv.syncTabsFromTargets(srv.beginTargetEnumeration(), fc.infos))
	return srv
}

func hasTarget(fc *fakeConn, id target.ID) bool {
	for _, info := range fc.infos {
		if info != nil && info.TargetID == id {
			return true
		}
	}
	return false
}

// assertUserTabNotAdopted restarts on stateDir with only a user-opened tab that
// shares the retired tab's short alias, and requires that it is neither tracked
// as managed nor closed by the idle sweeper.
func assertUserTabNotAdopted(t *testing.T, old *Server, stateDir string) {
	t.Helper()
	fc := &fakeConn{infos: []*target.Info{{TargetID: userTargetC, Type: "page", URL: "https://mail.example/"}}}
	srv := restartIdleServer(t, old, fc, stateDir)
	if _, ok := srv.tabIdle.Snapshot()[userTargetC]; ok {
		t.Errorf("user-opened tab %s adopted as daemon-managed after restart", userTargetC)
		srv.tabIdle.MarkManagedAt(userTargetC, time.Now().Add(-time.Hour))
	}
	srv.closeExpiredTabs(context.Background())
	if !hasTarget(fc, userTargetC) {
		t.Errorf("idle sweeper closed the user's tab %s", userTargetC)
	}
}

func newManagedTabServer(t *testing.T, stateDir string) (*Server, *fakeConn, string) {
	t.Helper()
	fc := &fakeConn{createID: managedTargetA}
	srv := newIdleTestServerWithState(fc, time.Minute, stateDir)
	t.Cleanup(func() { _ = srv.closeAudit() })
	short := tabNewShort(t, srv)
	drainRPCLog(t, srv)
	if _, ok := srv.store.ReplayManagedTabActivity()[short]; !ok {
		t.Fatalf("precondition: %q should be persisted as managed", short)
	}
	return srv, fc, short
}

func TestIdleCloseRetiresPersistedManagedTab(t *testing.T) {
	dir := t.TempDir()
	srv, fc, short := newManagedTabServer(t, dir)

	srv.tabIdle.MarkManagedAt(managedTargetA, time.Now().Add(-time.Hour))
	srv.closeExpiredTabs(context.Background())
	drainRPCLog(t, srv)
	if hasTarget(fc, managedTargetA) {
		t.Fatal("precondition: idle sweeper should have closed the tab")
	}
	if _, ok := srv.store.ReplayManagedTabActivity()[short]; ok {
		t.Errorf("idle-closed tab %q still in the persisted managed set", short)
	}
	assertUserTabNotAdopted(t, srv, dir)
}

func TestTargetDestroyedRetiresPersistedManagedTab(t *testing.T) {
	dir := t.TempDir()
	srv, fc, short := newManagedTabServer(t, dir)

	// The user closed the tab in Chrome (or it crashed): runTargetSync delivers
	// Target.targetDestroyed through removeTargetState, with no tab_close RPC.
	fc.infos = nil
	srv.removeTargetState(managedTargetA)
	drainRPCLog(t, srv)
	if _, ok := srv.store.ReplayManagedTabActivity()[short]; ok {
		t.Errorf("destroyed tab %q still in the persisted managed set", short)
	}
	assertUserTabNotAdopted(t, srv, dir)
}

func TestEnumerationDisappearanceRetiresPersistedManagedTab(t *testing.T) {
	dir := t.TempDir()
	srv, fc, short := newManagedTabServer(t, dir)

	// The lifecycle event was lost; a later successful enumeration no longer
	// lists the target.
	fc.infos = nil
	srv.reconcileIdleFromLog(srv.syncTabsFromTargets(srv.beginTargetEnumeration(), nil))
	drainRPCLog(t, srv)
	if _, ok := srv.store.ReplayManagedTabActivity()[short]; ok {
		t.Errorf("vanished tab %q still in the persisted managed set", short)
	}
	assertUserTabNotAdopted(t, srv, dir)
}

func TestExplicitTabCloseRetiresPersistedManagedTab(t *testing.T) {
	dir := t.TempDir()
	srv, _, short := newManagedTabServer(t, dir)

	postRPC(t, srv, rpcReq(protocol.MethodTabClose, map[string]any{"tab": short}, 2))
	// Retirement must not depend on the asynchronous audit line having landed.
	if _, ok := srv.store.ReplayManagedTabActivity()[short]; ok {
		t.Errorf("closed tab %q still in the persisted managed set", short)
	}
	assertUserTabNotAdopted(t, srv, dir)
}

// A managed tab that is still live when the daemon stops (no retirement) is
// recovered only for the exact same target, never for another tab that merely
// shares its short alias.
func TestRestartRecoversOnlyExactTarget(t *testing.T) {
	t.Run("same target", func(t *testing.T) {
		dir := t.TempDir()
		srv, _, short := newManagedTabServer(t, dir)
		fc := &fakeConn{infos: []*target.Info{{TargetID: managedTargetA, Type: "page"}}}
		srv2 := restartIdleServer(t, srv, fc, dir)
		if _, ok := srv2.tabIdle.Snapshot()[managedTargetA]; !ok {
			t.Fatal("live daemon-created tab not recovered as managed")
		}
		srv2.tabIdle.MarkManagedAt(managedTargetA, time.Now().Add(-time.Hour))
		srv2.closeExpiredTabs(context.Background())
		if hasTarget(fc, managedTargetA) {
			t.Fatal("recovered managed tab not closed after idle")
		}
		if _, ok := srv2.store.ReplayManagedTabActivity()[short]; ok {
			t.Fatal("recovered tab not retired after idle close")
		}
	})
	t.Run("different target with same alias", func(t *testing.T) {
		dir := t.TempDir()
		srv, _, _ := newManagedTabServer(t, dir)
		// The managed tab vanished while the daemon was down.
		assertUserTabNotAdopted(t, srv, dir)
	})
	t.Run("both present", func(t *testing.T) {
		dir := t.TempDir()
		srv, _, _ := newManagedTabServer(t, dir)
		fc := &fakeConn{infos: []*target.Info{
			{TargetID: managedTargetA, Type: "page"},
			{TargetID: userTargetC, Type: "page"},
		}}
		srv2 := restartIdleServer(t, srv, fc, dir)
		snap := srv2.tabIdle.Snapshot()
		if _, ok := snap[managedTargetA]; !ok {
			t.Error("live daemon-created tab not recovered as managed")
		}
		if _, ok := snap[userTargetC]; ok {
			t.Error("user tab sharing an alias suffix adopted as managed")
		}
	})
}

// Records written before target identity was persisted cannot be verified, so
// they must not turn whatever tab currently owns the alias into a managed tab.
func TestRestartDoesNotAdoptLegacyAliasOnlyRecord(t *testing.T) {
	dir := t.TempDir()
	line, err := json.Marshal(store.LogRecord{
		Action: protocol.MethodTabNew,
		Body:   json.RawMessage(`{"jsonrpc":"2.0","method":"tab_new","params":{},"id":1}`),
		Tab:    "1234",
		OK:     true,
		Time:   time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, store.RPCLogFile()), append(line, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	fc := &fakeConn{infos: []*target.Info{{TargetID: userTargetC, Type: "page"}}}
	srv := newIdleTestServerWithState(fc, time.Minute, dir)
	t.Cleanup(func() { _ = srv.closeAudit() })
	srv.reconcileIdleFromLog(srv.syncTabsFromTargets(srv.beginTargetEnumeration(), fc.infos))
	if got := srv.tabIdle.Snapshot(); len(got) != 0 {
		t.Fatalf("legacy alias-only record adopted a tab: %#v", got)
	}
	if got := srv.store.ReplayManagedTabActivity(); len(got) != 0 {
		t.Fatalf("legacy alias-only record kept as managed: %#v", got)
	}
}

func TestFirstRunEmptyStateAdoptsNothing(t *testing.T) {
	fc := &fakeConn{infos: []*target.Info{{TargetID: userTargetC, Type: "page"}}}
	srv := newIdleTestServerWithState(fc, time.Minute, t.TempDir())
	t.Cleanup(func() { _ = srv.closeAudit() })
	srv.reconcileIdleFromLog(srv.syncTabsFromTargets(srv.beginTargetEnumeration(), fc.infos))
	if got := srv.tabIdle.Snapshot(); len(got) != 0 {
		t.Fatalf("empty state adopted tabs: %#v", got)
	}
}

// A close that fails for a reason other than "target gone" leaves the tab
// alive, so its membership must survive, including across a restart.
func TestTransientCloseFailureKeepsPersistedMembership(t *testing.T) {
	for _, tc := range []struct {
		name  string
		close func(t *testing.T, srv *Server, short string)
	}{
		{"idle sweep", func(t *testing.T, srv *Server, _ string) {
			srv.tabIdle.MarkManagedAt(managedTargetA, time.Now().Add(-time.Hour))
			srv.closeExpiredTabs(context.Background())
		}},
		{"tab_close rpc", func(t *testing.T, srv *Server, short string) {
			postRPC(t, srv, rpcReq(protocol.MethodTabClose, map[string]any{"tab": short}, 2))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			srv, fc, short := newManagedTabServer(t, dir)
			fc.closeErr = errors.New("websocket: close 1006")
			tc.close(t, srv, short)
			drainRPCLog(t, srv)
			if _, ok := srv.store.ReplayManagedTabActivity()[short]; !ok {
				t.Fatal("failed close retired a still-live managed tab")
			}
			if _, ok := srv.tabIdle.Snapshot()[managedTargetA]; !ok {
				t.Fatal("failed close dropped in-memory idle tracking")
			}

			fc2 := &fakeConn{infos: []*target.Info{{TargetID: managedTargetA, Type: "page"}}}
			srv2 := restartIdleServer(t, srv, fc2, dir)
			if _, ok := srv2.tabIdle.Snapshot()[managedTargetA]; !ok {
				t.Fatal("still-live managed tab not recovered after restart")
			}
		})
	}
}

// A failed target enumeration says nothing about which tabs exist, so it must
// not retire anything.
func TestFailedEnumerationKeepsPersistedMembership(t *testing.T) {
	dir := t.TempDir()
	srv, _, short := newManagedTabServer(t, dir)
	srv.syncInitialTargets(noopDirtyMarker{}, srv.beginTargetEnumeration(), nil, errors.New("Target.getTargets: timeout"))
	if _, ok := srv.store.ReplayManagedTabActivity()[short]; !ok {
		t.Fatal("failed enumeration retired a managed tab")
	}
	if _, ok := srv.tabIdle.Snapshot()[managedTargetA]; !ok {
		t.Fatal("failed enumeration dropped in-memory idle tracking")
	}
}

type noopDirtyMarker struct{}

func (noopDirtyMarker) MarkTargetDirty() {}

func TestRotationSnapshotDropsRetiredKeepsLiveIdentity(t *testing.T) {
	dir := t.TempDir()
	liveTarget := target.ID("BBBB0000000000000000000000005678")
	fc := &fakeConn{createID: managedTargetA}
	cfg := Config{
		DebuggerURL:      "127.0.0.1:9222",
		ListenAddr:       "127.0.0.1:0",
		TabIdleTimeout:   time.Minute,
		StateDir:         dir,
		IdleStartupGrace: 30 * time.Millisecond,
		MaxLogBytes:      2048,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.tabHook = fc
	t.Cleanup(func() { _ = srv.closeAudit() })

	retiredShort := tabNewShort(t, srv)
	fc.createID = liveTarget
	liveShort := tabNewShort(t, srv)

	srv.tabIdle.MarkManagedAt(managedTargetA, time.Now().Add(-time.Hour))
	srv.closeExpiredTabs(context.Background())
	if hasTarget(fc, managedTargetA) || !hasTarget(fc, liveTarget) {
		t.Fatalf("precondition: only the expired tab should be closed: %#v", fc.infos)
	}

	logPath := filepath.Join(dir, store.RPCLogFile())
	for i := 0; i < 200; i++ {
		postRPC(t, srv, rpcReq(protocol.MethodGoto, map[string]any{"tab": liveShort, "url": "https://example.com/p"}, i+10))
		drainRPCLog(t, srv)
		if _, err := os.Stat(logPath + ".1"); err == nil {
			break
		}
	}
	if _, err := os.Stat(logPath + ".1"); err != nil {
		t.Fatalf("expected a rotation: %v", err)
	}
	drainRPCLog(t, srv)

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(managedTargetA)) {
		t.Errorf("rotated log still references retired target:\n%s", data)
	}
	if !bytes.Contains(data, []byte(liveTarget)) {
		t.Errorf("rotated log lost the live managed target identity:\n%s", data)
	}
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var rec store.LogRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatalf("bad log line %q: %v", line, err)
		}
		if rec.Tab == retiredShort && rec.Action != protocol.MethodGoto {
			t.Errorf("rotated log seeds retired tab %q: %s", retiredShort, line)
		}
	}

	// Recovery from the rotated file alone: the live tab is recovered by exact
	// identity; a user tab reusing the retired alias is left alone.
	fc2 := &fakeConn{infos: []*target.Info{
		{TargetID: liveTarget, Type: "page"},
		{TargetID: userTargetC, Type: "page"},
	}}
	srv2 := restartIdleServer(t, srv, fc2, dir)
	snap := srv2.tabIdle.Snapshot()
	if _, ok := snap[liveTarget]; !ok {
		t.Error("live managed tab not recovered after rotation")
	}
	if _, ok := snap[userTargetC]; ok {
		t.Error("user tab adopted through a retired alias after rotation")
	}
}

// Audit lines are written asynchronously, so a tab_new / activity / tab_close
// line can reach the store after the tab's lifecycle already ended, and even
// after its alias was reused by a newer daemon-created tab.
func TestDelayedAuditCannotResurrectOrRetireAcrossAliasReuse(t *testing.T) {
	dir := t.TempDir()
	srv, fc, short := newManagedTabServer(t, dir)

	fc.infos = nil
	srv.removeTargetState(managedTargetA)

	// Delayed lines of the old lifecycle, both as this version writes them
	// (naming the old target) and as alias-only lines.
	delayed := func(action string, params map[string]any) []store.LogRecord {
		rec := store.LogRecord{
			Action: action,
			Body:   json.RawMessage(rpcReq(action, params, 99)),
			Tab:    short,
			OK:     true,
			Time:   time.Now().Add(time.Hour).UTC(),
		}
		withTarget := rec
		withTarget.Target = string(managedTargetA)
		return []store.LogRecord{withTarget, rec}
	}
	for _, rec := range append(
		delayed(protocol.MethodTabNew, map[string]any{}),
		delayed(protocol.MethodGoto, map[string]any{"tab": short, "url": "https://example.com"})...,
	) {
		srv.enqueueAudit(rec)
	}
	drainRPCLog(t, srv)
	if _, ok := srv.store.ReplayManagedTabActivity()[short]; ok {
		t.Fatalf("delayed audit lines resurrected retired tab %q", short)
	}

	// The alias is reused by a new daemon-created tab; a delayed tab_close line
	// from the old lifecycle must not retire it.
	newTarget := target.ID("DDDD0000000000000000000000001234")
	fc.createID = newTarget
	if got := tabNewShort(t, srv); got != short {
		t.Fatalf("alias not reused: got %q want %q", got, short)
	}
	registeredAt := srv.store.ReplayManagedTabActivity()[short]
	for _, rec := range append(
		delayed(protocol.MethodTabClose, map[string]any{"tab": short}),
		delayed(protocol.MethodGoto, map[string]any{"tab": short, "url": "https://example.com"})...,
	) {
		srv.enqueueAudit(rec)
	}
	drainRPCLog(t, srv)
	managed := srv.store.ManagedTabs()
	if len(managed) != 1 || managed[0].TargetID != string(newTarget) {
		t.Fatalf("delayed lines from an old lifecycle changed membership: %#v", managed)
	}
	if !managed[0].LastActivity.Equal(registeredAt) {
		t.Fatalf("delayed activity of the old target renewed the new tab: %v want %v", managed[0].LastActivity, registeredAt)
	}

	fc2 := &fakeConn{infos: []*target.Info{{TargetID: newTarget, Type: "page"}}}
	srv2 := restartIdleServer(t, srv, fc2, dir)
	if _, ok := srv2.tabIdle.Snapshot()[newTarget]; !ok {
		t.Fatal("new lifecycle not recovered after restart")
	}

	// And the old target can never be adopted from this log.
	assertUserTabNotAdopted(t, srv2, dir)
}

// Successful tab RPCs persist activity against the full target id, so the idle
// clock survives a restart for the exact tab only.
func TestManagedActivityPersistedByTargetIdentity(t *testing.T) {
	dir := t.TempDir()
	srv, _, short := newManagedTabServer(t, dir)
	registeredAt := srv.store.ManagedTabs()[0].LastActivity

	time.Sleep(2 * time.Millisecond)
	postRPC(t, srv, rpcReq(protocol.MethodGoto, map[string]any{"tab": short, "url": "https://example.com"}, 2))
	drainRPCLog(t, srv)
	managed := srv.store.ManagedTabs()
	if len(managed) != 1 || managed[0].TargetID != string(managedTargetA) || managed[0].Tab != short {
		t.Fatalf("managed: %#v", managed)
	}
	if !managed[0].LastActivity.After(registeredAt) {
		t.Fatalf("activity not persisted: %v, registered %v", managed[0].LastActivity, registeredAt)
	}
}

// lockedConn makes fakeConn's target list safe for concurrent RPCs and hands
// out unique target ids that all collide on the 4-digit short alias.
type lockedConn struct {
	*fakeConn
	mu  sync.Mutex
	seq int
}

func (c *lockedConn) PageTargets() ([]*target.Info, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*target.Info(nil), c.infos...), nil
}

func (c *lockedConn) CreatePageTarget(string, bool) (target.ID, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	id := target.ID(fmt.Sprintf("%028X1234", c.seq))
	c.infos = append(c.infos, &target.Info{TargetID: id, Type: "page"})
	return id, nil
}

func (c *lockedConn) CloseTarget(id target.ID) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fakeConn.CloseTarget(id)
}

// Drives the real asynchronous audit path (scheduleAudit -> auditCh ->
// runAuditWriter) concurrently with every retirement path. Whatever order the
// audit lines land in, no finished lifecycle may remain persisted.
func TestConcurrentLifecyclesLeaveNoPersistedMembership(t *testing.T) {
	dir := t.TempDir()
	conn := &lockedConn{fakeConn: &fakeConn{}}
	cfg := Config{
		DebuggerURL:    "127.0.0.1:9222",
		ListenAddr:     "127.0.0.1:0",
		TabIdleTimeout: time.Minute,
		StateDir:       dir,
		MaxLogBytes:    4096, // force rotations while lifecycles are in flight
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.tabHook = conn
	t.Cleanup(func() { _ = srv.closeAudit() })

	const workers, rounds = 8, 20
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				short := tabNewShort(t, srv)
				tid, ok := srv.tabs.Lookup(short)
				if !ok {
					t.Errorf("tab %q missing from registry", short)
					return
				}
				postRPC(t, srv, rpcReq(protocol.MethodGoto, map[string]any{"tab": short, "url": "https://example.com"}, 2))
				switch (w + i) % 3 {
				case 0:
					postRPC(t, srv, rpcReq(protocol.MethodTabClose, map[string]any{"tab": short}, 3))
				case 1:
					_ = conn.CloseTarget(tid)
					srv.removeTargetState(tid) // Target.targetDestroyed
				default:
					srv.tabIdle.MarkManagedAt(tid, time.Now().Add(-time.Hour))
					srv.closeExpiredTabs(context.Background())
					if _, still := srv.tabIdle.Snapshot()[tid]; still {
						// Another worker's sweep may hold this tab's turn; finish it here.
						_ = srv.closeTabByShort(context.Background(), short)
					}
				}
			}
		}(w)
	}
	wg.Wait()
	drainRPCLog(t, srv)

	if got := srv.store.ManagedTabs(); len(got) != 0 {
		t.Fatalf("finished lifecycles still persisted: %#v", got)
	}
	if err := srv.closeAudit(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(store.OpenConfig{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.ManagedTabs(); len(got) != 0 {
		t.Fatalf("finished lifecycles resurrected from disk: %#v", got)
	}
}
