package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/yiplee/go-bb-browser/internal/store"
	"github.com/yiplee/go-bb-browser/pkg/protocol"
)

// Two page targets whose ids end in the same 4 hex digits. collideA is the tab
// the daemon creates first; collideB sorts before it, which is what made a
// registry rebuilt after a restart hand A's short id to B.
const (
	collideA = target.ID("FFFF000000000000000000000000ABCD")
	collideB = target.ID("0000111111111111111111111111ABCD")
)

type rpcOutcome struct {
	Tab   string
	Error *protocol.ResponseError
	Raw   string
}

func callRPC(t *testing.T, srv *Server, method string, params map[string]any) rpcOutcome {
	t.Helper()
	rec := postRPC(t, srv, rpcReq(method, params, 1))
	var env struct {
		Result struct {
			Tab string `json:"tab"`
		} `json:"result"`
		Error *protocol.ResponseError `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("%s: decode %s: %v", method, rec.Body.String(), err)
	}
	return rpcOutcome{Tab: env.Result.Tab, Error: env.Error, Raw: rec.Body.String()}
}

func mustTabNew(t *testing.T, srv *Server, fc *fakeConn, id target.ID) string {
	t.Helper()
	fc.createID = id
	out := callRPC(t, srv, protocol.MethodTabNew, map[string]any{})
	if out.Error != nil || out.Tab == "" {
		t.Fatalf("tab_new %s: %s", id, out.Raw)
	}
	return out.Tab
}

// restartDaemon closes srv's log and brings up a new daemon on the same state
// dir, attached to a browser currently showing ids.
func restartDaemon(t *testing.T, srv *Server, stateDir string, timeout time.Duration, ids ...target.ID) (*Server, *fakeConn) {
	t.Helper()
	drainRPCLog(t, srv)
	if err := srv.store.Close(); err != nil {
		t.Fatal(err)
	}
	fc := &fakeConn{}
	for _, id := range ids {
		fc.infos = append(fc.infos, &target.Info{TargetID: id, Type: "page", URL: "https://ex"})
	}
	next := newIdleTestServerWithState(fc, timeout, stateDir)
	t.Cleanup(func() { _ = next.store.Close() })
	next.reconcileIdleFromLog(next.syncTabsFromTargets(next.beginTargetEnumeration(), fc.infos))
	return next, fc
}

func managedTargets(srv *Server) []target.ID {
	var out []target.ID
	for id := range srv.tabIdle.Snapshot() {
		out = append(out, id)
	}
	return out
}

func liveTargets(fc *fakeConn) []target.ID {
	var out []target.ID
	for _, info := range fc.infos {
		out = append(out, info.TargetID)
	}
	return out
}

func sameTargets(got []target.ID, want ...target.ID) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[target.ID]bool{}
	for _, id := range got {
		seen[id] = true
	}
	for _, id := range want {
		if !seen[id] {
			return false
		}
	}
	return true
}

// The daemon created A (short id abcd). While it was down A went away and the
// user opened B, whose id ends in the same digits. The restarted daemon must
// not treat B as daemon-created: it would idle-close a tab it does not own.
func TestRestartDoesNotAdoptTabReusingShortID(t *testing.T) {
	stateDir := t.TempDir()
	timeout := 40 * time.Millisecond

	fcA := &fakeConn{}
	srvA := newIdleTestServerWithState(fcA, timeout, stateDir)
	if short := mustTabNew(t, srvA, fcA, collideA); short != "abcd" {
		t.Fatalf("short id of A = %q, want abcd", short)
	}

	srvB, fcB := restartDaemon(t, srvA, stateDir, timeout, collideB)
	if got := managedTargets(srvB); len(got) != 0 {
		t.Fatalf("managed after restart = %v; the log only knows A, which is gone", got)
	}
	time.Sleep(timeout + 40*time.Millisecond)
	srvB.closeExpiredTabs(context.Background())
	if got := liveTargets(fcB); !sameTargets(got, collideB) {
		t.Fatalf("live targets after idle sweep = %v; user tab B must survive", got)
	}
}

// A (daemon-created) and B (user tab) coexist with colliding ids. After a
// restart exactly A is managed again, and only A is idle-closed.
func TestRestartRestoresManagedTabDespiteCollision(t *testing.T) {
	stateDir := t.TempDir()
	timeout := 40 * time.Millisecond

	fcA := &fakeConn{}
	srvA := newIdleTestServerWithState(fcA, timeout, stateDir)
	mustTabNew(t, srvA, fcA, collideA)
	// The user opens B: a lifecycle event registers it.
	fcA.infos = append(fcA.infos, &target.Info{TargetID: collideB, Type: "page"})
	srvA.tabs.RegisterPageTarget(collideB)
	// Activity on B, addressed by its own short id, is logged before restart.
	shortB, _ := srvA.tabs.ShortForTarget(collideB)
	if out := callRPC(t, srvA, protocol.MethodEval, map[string]any{"tab": shortB, "script": "1"}); out.Error != nil {
		t.Fatalf("eval on B via %q: %s", shortB, out.Raw)
	}

	srvB, fcB := restartDaemon(t, srvA, stateDir, timeout, collideA, collideB)
	if got := managedTargets(srvB); !sameTargets(got, collideA) {
		t.Fatalf("managed after restart = %v, want exactly A", got)
	}
	time.Sleep(timeout + 40*time.Millisecond)
	srvB.closeExpiredTabs(context.Background())
	if got := liveTargets(fcB); !sameTargets(got, collideB) {
		t.Fatalf("live targets after idle sweep = %v, want only user tab B", got)
	}
}

// A short id that several open tabs share is a hard error (INV-3) for every
// tab-scoped method; it never keeps pointing at whichever tab had it first.
func TestAmbiguousShortIDIsHardError(t *testing.T) {
	fc := &fakeConn{}
	srv := newIdleTestServer(fc, time.Minute)
	if short := mustTabNew(t, srv, fc, collideA); short != "abcd" {
		t.Fatalf("short id of A = %q, want abcd", short)
	}
	if out := callRPC(t, srv, protocol.MethodEval, tabRPCParams(protocol.MethodEval, "abcd")); out.Error != nil {
		t.Fatalf("eval before collision: %s", out.Raw)
	}

	if short := mustTabNew(t, srv, fc, collideB); short != "1abcd" {
		t.Fatalf("short id of B = %q, want 1abcd", short)
	}
	evals := fc.evalCalls.Load()
	for _, method := range append([]string{protocol.MethodTabSelect}, tabLockRPCMethods...) {
		out := callRPC(t, srv, method, tabRPCParams(method, "abcd"))
		if out.Error == nil || out.Error.Code != protocol.CodeInvalidParams || !strings.Contains(out.Error.Message, "unknown tab id") {
			t.Fatalf("%s with ambiguous abcd: %s", method, out.Raw)
		}
		if !strings.Contains(out.Raw, "ambiguous") {
			t.Fatalf("%s: error does not tell the client the id is ambiguous: %s", method, out.Raw)
		}
	}
	if got := fc.evalCalls.Load(); got != evals {
		t.Fatalf("ambiguous short id reached the browser: %d eval calls", got-evals)
	}
	if got := liveTargets(fc); !sameTargets(got, collideA, collideB) {
		t.Fatalf("live targets = %v; an ambiguous tab_close must close nothing", got)
	}

	// tab_list is how a client recovers: it reports the unambiguous ids.
	rec := postRPC(t, srv, rpcReq(protocol.MethodTabList, map[string]any{}, 1))
	var list struct {
		Result protocol.TabListResult `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, item := range list.Result.Tabs {
		listed = append(listed, item.Tab)
	}
	if strings.Join(listed, ",") != "0abcd,1abcd" {
		t.Fatalf("tab_list ids = %v, want [0abcd 1abcd]", listed)
	}
	// tab_new selected B last; the reported focus is B under its current id.
	if list.Result.Focus != "1abcd" {
		t.Fatalf("focus = %q, want 1abcd", list.Result.Focus)
	}
	for short, want := range map[string]target.ID{"0abcd": collideA, "1abcd": collideB} {
		if out := callRPC(t, srv, protocol.MethodEval, tabRPCParams(protocol.MethodEval, short)); out.Error != nil {
			t.Fatalf("eval via %q: %s", short, out.Raw)
		}
		if tid, ok := srv.tabs.Lookup(short); !ok || tid != want {
			t.Fatalf("Lookup(%q) = %q, %v; want %s", short, tid, ok, want)
		}
	}

	// Closing B ends the collision: abcd names A again, never B.
	if out := callRPC(t, srv, protocol.MethodTabClose, map[string]any{"tab": "1abcd"}); out.Error != nil {
		t.Fatalf("tab_close B: %s", out.Raw)
	}
	if got := liveTargets(fc); !sameTargets(got, collideA) {
		t.Fatalf("live targets after closing 1abcd = %v, want only A", got)
	}
	for _, short := range []string{"abcd", "0abcd"} {
		if tid, ok := srv.tabs.Lookup(short); !ok || tid != collideA {
			t.Fatalf("Lookup(%q) after collision cleared = %q, %v; want A", short, tid, ok)
		}
	}
}

func readRPCLog(t *testing.T, stateDir string) []map[string]any {
	t.Helper()
	f, err := os.Open(filepath.Join(stateDir, store.RPCLogFile()))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("rpc log line %q: %v", sc.Text(), err)
		}
		out = append(out, rec)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// Every logged tab operation names the full target it ran against, captured
// while the tab was registered: tab_close removes the tab before the record is
// written, and an alias can mean another tab by the time the log is replayed.
func TestRPCLogRecordsFullTargetID(t *testing.T) {
	stateDir := t.TempDir()
	fc := &fakeConn{}
	srv := newIdleTestServerWithState(fc, time.Minute, stateDir)
	t.Cleanup(func() { _ = srv.store.Close() })

	mustTabNew(t, srv, fc, collideA)
	mustTabNew(t, srv, fc, collideB)
	steps := []struct {
		method string
		tab    string
		want   target.ID
	}{
		{protocol.MethodEval, "0abcd", collideA},
		{protocol.MethodGoto, "ffff000000000000000000000000abcd", collideA}, // any unambiguous suffix
		{protocol.MethodNetwork, "1abcd", collideB},
		{protocol.MethodTabSelect, "0abcd", collideA},
		{protocol.MethodTabClose, "1abcd", collideB},
		{protocol.MethodEval, "abcd", collideA}, // B is gone: abcd is A again
		{protocol.MethodTabClose, "abcd", collideA},
	}
	for _, step := range steps {
		if out := callRPC(t, srv, step.method, tabRPCParams(step.method, step.tab)); out.Error != nil {
			t.Fatalf("%s %q: %s", step.method, step.tab, out.Raw)
		}
	}
	// Failed requests resolve nothing and must not claim a target.
	if out := callRPC(t, srv, protocol.MethodEval, tabRPCParams(protocol.MethodEval, "abcd")); out.Error == nil {
		t.Fatalf("eval on closed tab succeeded: %s", out.Raw)
	}
	drainRPCLog(t, srv)

	recs := readRPCLog(t, stateDir)
	if len(recs) != len(steps)+3 {
		t.Fatalf("log has %d records, want %d: %v", len(recs), len(steps)+3, recs)
	}
	wantNew := []target.ID{collideA, collideB}
	for i, rec := range recs[:2] {
		if rec["action"] != protocol.MethodTabNew || rec["target"] != string(wantNew[i]) {
			t.Fatalf("record %d = %v, want tab_new of %s", i, rec, wantNew[i])
		}
	}
	for i, step := range steps {
		rec := recs[i+2]
		if rec["action"] != step.method || rec["tab"] != step.tab || rec["target"] != string(step.want) {
			t.Fatalf("record for %s %q = %v, want target %s", step.method, step.tab, rec, step.want)
		}
	}
	if last := recs[len(recs)-1]; last["ok"] != false || last["target"] != nil {
		t.Fatalf("failed request logged as %v, want no target", last)
	}
	if left := srv.store.ReplayManagedTabActivity(); len(left) != 0 {
		t.Fatalf("managed after closing both tabs = %v", left)
	}
}

// Log records without a target id (written by daemons before this field
// existed) are not evidence about any tab alive today.
func TestRestartIgnoresLogRecordsWithoutTargetID(t *testing.T) {
	stateDir := t.TempDir()
	line := `{"action":"tab_new","body":{"jsonrpc":"2.0","method":"tab_new","params":{},"id":1},"tab":"abcd","sender_ip":"127.0.0.1","ok":true,"time":"` +
		time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano) + `"}` + "\n"
	if err := os.WriteFile(filepath.Join(stateDir, store.RPCLogFile()), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	fc := &fakeConn{infos: []*target.Info{{TargetID: collideA, Type: "page"}}}
	srv := newIdleTestServerWithState(fc, 40*time.Millisecond, stateDir)
	t.Cleanup(func() { _ = srv.store.Close() })
	srv.reconcileIdleFromLog(srv.syncTabsFromTargets(srv.beginTargetEnumeration(), fc.infos))

	if got := managedTargets(srv); len(got) != 0 {
		t.Fatalf("managed = %v; a record identified only by short id must not be trusted", got)
	}
	time.Sleep(80 * time.Millisecond)
	srv.closeExpiredTabs(context.Background())
	if got := liveTargets(fc); !sameTargets(got, collideA) {
		t.Fatalf("live targets = %v; the tab must not be idle-closed", got)
	}
}

// The log is rotated while a collision is live; the snapshot seeding the new
// file must still let a restart tell A (managed) from B (not managed).
func TestRestartAfterRotationKeepsTargetIdentity(t *testing.T) {
	stateDir := t.TempDir()
	cfg := Config{
		DebuggerURL:      "127.0.0.1:9222",
		ListenAddr:       "127.0.0.1:0",
		TabIdleTimeout:   time.Minute,
		StateDir:         stateDir,
		IdleStartupGrace: 30 * time.Millisecond,
		MaxLogBytes:      2048,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	srvA, err := NewServer(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	fcA := &fakeConn{}
	srvA.tabHook = fcA
	mustTabNew(t, srvA, fcA, collideA)
	fcA.infos = append(fcA.infos, &target.Info{TargetID: collideB, Type: "page"})
	srvA.tabs.RegisterPageTarget(collideB)
	for i := 0; i < 40; i++ {
		if out := callRPC(t, srvA, protocol.MethodEval, tabRPCParams(protocol.MethodEval, "1abcd")); out.Error != nil {
			t.Fatalf("eval on B: %s", out.Raw)
		}
	}
	drainRPCLog(t, srvA)
	if _, err := os.Stat(filepath.Join(stateDir, store.RPCLogFile()+".1")); err != nil {
		t.Fatalf("log was not rotated: %v", err)
	}

	srvB, _ := restartDaemon(t, srvA, stateDir, time.Minute, collideA, collideB)
	if got := managedTargets(srvB); !sameTargets(got, collideA) {
		t.Fatalf("managed after rotation + restart = %v, want exactly A", got)
	}
}

// One tab answers to several aliases. Operations on it must serialize whatever
// spelling each request used, and the extra lock must not leak entries.
func TestAliasesOfOneTabShareTheOperationLock(t *testing.T) {
	srv := newIdleTestServer(&fakeConn{}, time.Minute)
	srv.tabs.RegisterPageTarget(collideA)
	for _, alias := range []string{"abcd", "0abcd"} {
		if tid, ok := srv.tabs.Lookup(alias); !ok || tid != collideA {
			t.Fatalf("Lookup(%q) = %q, %v; want A", alias, tid, ok)
		}
	}

	unlock, ok := srv.lockTab(context.Background(), "abcd")
	if !ok {
		t.Fatal("lock via abcd failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	if unlockOther, ok := srv.lockTab(ctx, "0abcd"); ok {
		unlockOther()
		cancel()
		t.Fatal("0abcd locked the tab while abcd held it: two spellings ran concurrently")
	}
	cancel()

	// A different tab is not blocked.
	srv.tabs.RegisterPageTarget("EEEE0000000000000000000000001111")
	unlockOther, ok := srv.lockTab(context.Background(), "1111")
	if !ok {
		t.Fatal("unrelated tab blocked")
	}
	unlockOther()

	unlock()
	unlock2, ok := srv.lockTab(context.Background(), "0abcd")
	if !ok {
		t.Fatal("lock via 0abcd failed after release")
	}
	unlock2()

	srv.tabMuOps.Lock()
	aliases, targets := len(srv.tabCDPLocks), len(srv.targetCDPLocks)
	srv.tabMuOps.Unlock()
	if aliases != 0 || targets != 0 {
		t.Fatalf("lock entries left: %d alias, %d target", aliases, targets)
	}
}

// Idle cleanup is driven by target id. A short id that became ambiguous (or
// changed) after the tab was created must not redirect or block the close.
func TestIdleCleanupClosesManagedTargetDespiteCollision(t *testing.T) {
	timeout := 40 * time.Millisecond
	fc := &fakeConn{}
	srv := newIdleTestServer(fc, timeout)
	mustTabNew(t, srv, fc, collideA)
	fc.infos = append(fc.infos, &target.Info{TargetID: collideB, Type: "page"})
	srv.tabs.RegisterPageTarget(collideB)

	time.Sleep(timeout + 40*time.Millisecond)
	srv.closeExpiredTabs(context.Background())
	if got := liveTargets(fc); !sameTargets(got, collideB) {
		t.Fatalf("live targets after idle sweep = %v, want only the unmanaged tab B", got)
	}
	if tid, ok := srv.tabs.Lookup("abcd"); !ok || tid != collideB {
		t.Fatalf("Lookup(abcd) once A is closed = %q, %v; want B", tid, ok)
	}
}
