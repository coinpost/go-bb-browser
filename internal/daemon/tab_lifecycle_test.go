package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/yiplee/go-bb-browser/internal/store"
	"github.com/yiplee/go-bb-browser/pkg/protocol"
)

const (
	lifecycleManaged = target.ID("AAAA00001234")
	lifecycleUserTab = target.ID("BBBB99991234") // different target, same short id
)

// hookedWriter runs onWrite just before the response body is written: the
// point at which the response used to be sent but not yet audited.
type hookedWriter struct {
	*httptest.ResponseRecorder
	onWrite func()
}

func (w *hookedWriter) Write(b []byte) (int, error) {
	if w.onWrite != nil {
		w.onWrite()
		w.onWrite = nil
	}
	return w.ResponseRecorder.Write(b)
}

func readRPCLog(t *testing.T, stateDir string) []store.LogRecord {
	t.Helper()
	f, err := os.Open(filepath.Join(stateDir, store.RPCLogFile()))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []store.LogRecord
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var rec store.LogRecord
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

func logActions(recs []store.LogRecord) string {
	names := make([]string, 0, len(recs))
	for _, rec := range recs {
		names = append(names, rec.Action)
	}
	return strings.Join(names, ",")
}

// waitAuditPending waits until n records sit in the audit queue.
func waitAuditPending(t *testing.T, srv *Server, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		srv.auditMu.Lock()
		got := len(srv.auditPending)
		srv.auditMu.Unlock()
		if got == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("audit queue holds %d records, want %d", got, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// Target.targetDestroyed can be handled between a tab_new registering its tab
// and its response being written and audited. The synthetic tab_close must
// still land after the tab_new record, or the short id stays managed on disk
// and the next daemon adopts an unrelated user tab.
func TestTargetDestroyedBeforeTabNewAuditKeepsLogOrder(t *testing.T) {
	stateDir := t.TempDir()
	fc := &fakeConn{createID: lifecycleManaged}
	srv := newIdleTestServerWithState(fc, time.Minute, stateDir)

	w := &hookedWriter{ResponseRecorder: httptest.NewRecorder()}
	w.onWrite = func() { srv.removeTargetState(lifecycleManaged) }
	req := httptest.NewRequest(http.MethodPost, "/v1", strings.NewReader(rpcReq(protocol.MethodTabNew, map[string]any{}, 1)))
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte(`"tab":"1234"`)) {
		t.Fatalf("tab_new: %d %s", w.Code, w.Body.String())
	}
	if err := srv.closeAudit(); err != nil {
		t.Fatal(err)
	}

	if got := logActions(readRPCLog(t, stateDir)); got != "tab_new,tab_close" {
		t.Fatalf("rpc log order = %s, want tab_new,tab_close", got)
	}
	if got := restartWithUserTab(t, stateDir, lifecycleUserTab); len(got) != 0 {
		t.Fatalf("user tab adopted as managed after restart: %v", got)
	}
}

// A full audit queue applies backpressure: the enqueuer writes the backlog
// first and only then queues its own record. A synthetic tab_close written
// directly would overtake the still-queued tab_new of the same tab, and a
// record appended before the wait would let the queue outgrow its bound.
func TestFullAuditQueueKeepsFIFO(t *testing.T) {
	stateDir := t.TempDir()
	fc := &fakeConn{createID: lifecycleManaged}
	srv := newIdleTestServerWithState(fc, time.Minute, stateDir)
	srv.auditQueueCap = 2

	// Stall the writer so records pile up.
	srv.auditWriteMu.Lock()
	postRPC(t, srv, rpcReq(protocol.MethodTabNew, map[string]any{}, 1))
	postRPC(t, srv, rpcReq(protocol.MethodEval, map[string]any{"tab": "1234", "script": "1"}, 2))
	waitAuditPending(t, srv, 2)

	destroyed := make(chan struct{})
	go func() {
		defer close(destroyed)
		srv.removeTargetState(lifecycleManaged) // third record: over the bound
	}()
	select {
	case <-destroyed:
		t.Fatal("enqueue over the bound returned without writing")
	case <-time.After(50 * time.Millisecond):
	}
	waitAuditPending(t, srv, 2) // the bound holds while the writer is stalled
	if data, _ := os.ReadFile(filepath.Join(stateDir, store.RPCLogFile())); len(data) != 0 {
		t.Fatalf("record written past the stalled queue:\n%s", data)
	}
	srv.auditWriteMu.Unlock()
	<-destroyed

	if err := srv.closeAudit(); err != nil {
		t.Fatal(err)
	}
	if got := logActions(readRPCLog(t, stateDir)); got != "tab_new,eval,tab_close" {
		t.Fatalf("rpc log order = %s, want tab_new,eval,tab_close", got)
	}
	if got := restartWithUserTab(t, stateDir, lifecycleUserTab); len(got) != 0 {
		t.Fatalf("user tab adopted as managed after restart: %v", got)
	}
}

// However many producers pile up behind a stalled writer, the queue never
// holds more than its bound.
func TestAuditQueueStaysWithinBound(t *testing.T) {
	srv := newIdleTestServerWithState(&fakeConn{}, time.Minute, t.TempDir())
	srv.auditQueueCap = 2

	srv.auditWriteMu.Lock()
	const producers = 32
	done := make(chan struct{}, producers)
	for range producers {
		go func() {
			srv.enqueueAudit(store.LogRecord{
				Action: protocol.MethodEval,
				Body:   json.RawMessage(`{"jsonrpc":"2.0","method":"eval"}`),
				Time:   time.Now().UTC(),
			})
			done <- struct{}{}
		}()
	}
	waitAuditPending(t, srv, 2)
	time.Sleep(50 * time.Millisecond)
	srv.auditMu.Lock()
	pending := len(srv.auditPending)
	srv.auditMu.Unlock()
	if pending != 2 {
		t.Fatalf("pending with a stalled writer = %d, want the bound 2", pending)
	}
	srv.auditWriteMu.Unlock()
	for range producers {
		<-done
	}
	if err := srv.closeAudit(); err != nil {
		t.Fatal(err)
	}
}

// With a tiny bound, enqueuers and the writer flush concurrently all the time;
// every producer's records must still reach disk in the order it queued them.
func TestAuditQueueOrderUnderContention(t *testing.T) {
	stateDir := t.TempDir()
	srv := newIdleTestServerWithState(&fakeConn{}, time.Minute, stateDir)
	srv.auditQueueCap = 3

	const producers, perProducer = 4, 200
	done := make(chan struct{})
	for p := 0; p < producers; p++ {
		go func(p int) {
			defer func() { done <- struct{}{} }()
			for i := 1; i <= perProducer; i++ {
				srv.enqueueAudit(store.LogRecord{
					Action: protocol.MethodEval,
					Body:   json.RawMessage(`{"jsonrpc":"2.0","method":"eval"}`),
					Tab:    string(rune('a' + p)),
					Seq:    uint64(i),
					Time:   time.Now().UTC(),
				})
			}
		}(p)
	}
	for p := 0; p < producers; p++ {
		<-done
	}
	if err := srv.closeAudit(); err != nil {
		t.Fatal(err)
	}

	recs := readRPCLog(t, stateDir)
	if len(recs) != producers*perProducer {
		t.Fatalf("records = %d, want %d", len(recs), producers*perProducer)
	}
	last := map[string]uint64{}
	for _, rec := range recs {
		if rec.Seq != last[rec.Tab]+1 {
			t.Fatalf("producer %s: record %d written after %d", rec.Tab, rec.Seq, last[rec.Tab])
		}
		last[rec.Tab] = rec.Seq
	}
}

// A runtime resync must not consult rpc.jsonl. While a synthetic tab_close is
// still queued the log says the short id is managed, and replaying it would
// adopt — and later idle-close — a user tab that derived the same short id.
func TestRuntimeResyncDoesNotAdoptFromPendingLog(t *testing.T) {
	stateDir := t.TempDir()
	fc := &fakeConn{createID: lifecycleManaged}
	srv := newIdleTestServerWithState(fc, time.Minute, stateDir)
	srv.syncTabsFromTargets(fc.infos) // startup enumeration: recovery is done
	postRPC(t, srv, rpcReq(protocol.MethodTabNew, map[string]any{}, 1))
	drainRPCLog(t, srv)

	// Stall the writer: the release below stays queued, so the log keeps
	// reporting 1234 as managed for the whole resync.
	srv.auditWriteMu.Lock()
	srv.removeTargetState(lifecycleManaged) // Target.targetDestroyed
	waitAuditPending(t, srv, 1)
	if got := srv.store.ReplayManagedTabActivity(); len(got) != 1 {
		t.Fatalf("test setup: log should still list the tab, got %v", got)
	}
	fc.infos = []*target.Info{{TargetID: lifecycleUserTab, Type: "page", URL: "https://user.example"}}
	if _, err := srv.listAndSyncTabs(context.Background(), fc); err != nil { // runTargetSync resync
		t.Fatal(err)
	}
	if got := srv.tabIdle.Snapshot(); len(got) != 0 {
		t.Fatalf("user tab adopted as managed by resync: %v", got)
	}
	srv.auditWriteMu.Unlock()
	drainRPCLog(t, srv)

	if got := srv.store.ReplayManagedTabActivity(); len(got) != 0 {
		t.Fatalf("log still lists managed tabs: %v", got)
	}
	srv.cfg.TabIdleTimeout = time.Nanosecond
	time.Sleep(time.Millisecond)
	srv.closeExpiredTabs(context.Background())
	if len(fc.infos) != 1 {
		t.Fatal("idle cleanup closed the user tab")
	}
}

// seedManagedLog leaves a previous run's rpc.jsonl with one managed tab.
func seedManagedLog(t *testing.T, stateDir string) {
	t.Helper()
	srv := newIdleTestServerWithState(&fakeConn{createID: lifecycleManaged}, time.Minute, stateDir)
	postRPC(t, srv, rpcReq(protocol.MethodTabNew, map[string]any{}, 1))
	if err := srv.closeAudit(); err != nil {
		t.Fatal(err)
	}
}

// Recovery from rpc.jsonl belongs to the first enumeration that succeeds: a
// failed one must neither adopt nor release anything, the retry adopts the
// previous run's tab, and no later enumeration recovers again.
func TestIdleRecoveryRunsOnceOnFirstSuccessfulEnumeration(t *testing.T) {
	stateDir := t.TempDir()
	seedManagedLog(t, stateDir)

	fc := &fakeConn{infos: []*target.Info{{TargetID: lifecycleManaged, Type: "page"}}}
	srv := newIdleTestServerWithState(fc, time.Minute, stateDir)
	t.Cleanup(func() { _ = srv.closeAudit() })

	marker := &dirtyMarker{}
	srv.syncInitialTargets(marker, nil, errors.New("temporary target enumeration failure"), 0)
	drainRPCLog(t, srv)
	if !marker.marked.Load() {
		t.Fatal("failed initial enumeration did not schedule a retry")
	}
	if got := srv.tabIdle.Snapshot(); len(got) != 0 {
		t.Fatalf("managed after failed enumeration: %v", got)
	}
	if got := srv.store.ReplayManagedTabActivity(); len(got) != 1 {
		t.Fatalf("failed enumeration released persisted tabs: %v", got)
	}

	if _, err := srv.listAndSyncTabs(context.Background(), fc); err != nil { // the retry
		t.Fatal(err)
	}
	if got := srv.tabIdle.Snapshot(); len(got) != 1 {
		t.Fatalf("retry did not recover the managed tab: %v", got)
	}

	// The tab is closed in Chrome and a user tab takes over its short id.
	srv.removeTargetState(lifecycleManaged)
	fc.infos = []*target.Info{{TargetID: lifecycleUserTab, Type: "page"}}
	if _, err := srv.listAndSyncTabs(context.Background(), fc); err != nil {
		t.Fatal(err)
	}
	if got := srv.tabIdle.Snapshot(); len(got) != 0 {
		t.Fatalf("later enumeration recovered again: %v", got)
	}
	drainRPCLog(t, srv)
	if got := srv.store.ReplayManagedTabActivity(); len(got) != 0 {
		t.Fatalf("log still lists managed tabs: %v", got)
	}
}

// Before the first successful enumeration the previous run's tab can already
// be seen (targetCreated) and seen to go (targetDestroyed). A user tab that
// then derives the same short id must not inherit it at recovery.
func TestIdleRecoverySkipsShortIDReleasedBeforeIt(t *testing.T) {
	stateDir := t.TempDir()
	seedManagedLog(t, stateDir)

	fc := &fakeConn{}
	srv := newIdleTestServerWithState(fc, time.Minute, stateDir)
	srv.syncInitialTargets(&dirtyMarker{}, nil, errors.New("temporary target enumeration failure"), 0)
	srv.tabs.RegisterPageTarget(lifecycleManaged) // Target.targetCreated
	srv.removeTargetState(lifecycleManaged)       // Target.targetDestroyed

	fc.infos = []*target.Info{{TargetID: lifecycleUserTab, Type: "page"}}
	if _, err := srv.listAndSyncTabs(context.Background(), fc); err != nil {
		t.Fatal(err)
	}
	if got := srv.tabIdle.Snapshot(); len(got) != 0 {
		t.Fatalf("user tab adopted as managed: %v", got)
	}
	if err := srv.closeAudit(); err != nil {
		t.Fatal(err)
	}
	if got := restartWithUserTab(t, stateDir, lifecycleUserTab); len(got) != 0 {
		t.Fatalf("user tab adopted as managed after restart: %v", got)
	}
}

// A tab_new of this run made before the first successful enumeration keeps its
// own record: recovery releases the previous run's target, by full id, and not
// the new tab that holds the same short id.
func TestIdleRecoveryKeepsTabCreatedBeforeIt(t *testing.T) {
	stateDir := t.TempDir()
	seedManagedLog(t, stateDir) // previous run's 1234 is gone

	fc := &fakeConn{createID: lifecycleUserTab} // derives 1234 again
	srv := newIdleTestServerWithState(fc, time.Minute, stateDir)
	postRPC(t, srv, rpcReq(protocol.MethodTabNew, map[string]any{}, 1))
	if _, err := srv.listAndSyncTabs(context.Background(), fc); err != nil {
		t.Fatal(err)
	}
	if got := srv.tabIdle.Snapshot(); len(got) != 1 {
		t.Fatalf("managed = %v", got)
	}
	if err := srv.closeAudit(); err != nil {
		t.Fatal(err)
	}
	recs := readRPCLog(t, stateDir)
	if got := logActions(recs); got != "tab_new,tab_new,tab_close" {
		t.Fatalf("rpc log = %s, want tab_new,tab_new,tab_close", got)
	}
	if got := recs[2].Target; got != string(lifecycleManaged) {
		t.Fatalf("released target = %q, want the previous run's %q", got, lifecycleManaged)
	}
	if got := managedOnDisk(t, stateDir); len(got) != 1 || got[0] != string(lifecycleUserTab) {
		t.Fatalf("managed on disk = %v, want only this run's tab", got)
	}
}

// managedOnDisk replays the state dir's rpc.jsonl and returns the full target
// ids it leaves managed; it fails if any short-id-only entry is left.
func managedOnDisk(t *testing.T, stateDir string) []string {
	t.Helper()
	st, err := store.Open(store.OpenConfig{StateDir: stateDir})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	tabs, legacy := st.StartupManaged()
	if len(legacy) != 0 {
		t.Fatalf("short-id-only entries left on disk: %v", legacy)
	}
	out := make([]string, 0, len(tabs))
	for _, tab := range tabs {
		out = append(out, tab.Target)
	}
	return out
}

// Target.targetDestroyed is often handled before Target.closeTarget returns.
// The RPC's own record is then the only tab_close; if the RPC fails after all,
// the synthetic record is the one that releases the short id.
func TestTabCloseRPCRacingTargetDestroyedWritesOneRelease(t *testing.T) {
	for name, closeErr := range map[string]error{
		"close succeeds": nil,
		"close fails":    errors.New("closeTarget reply lost"),
	} {
		t.Run(name, func(t *testing.T) {
			stateDir := t.TempDir()
			fc := &fakeConn{createID: lifecycleManaged}
			srv := newIdleTestServerWithState(fc, time.Minute, stateDir)
			postRPC(t, srv, rpcReq(protocol.MethodTabNew, map[string]any{}, 1))

			fc.closeErr = closeErr
			fc.onClose = func(id target.ID) { srv.removeTargetState(id) }
			postRPC(t, srv, rpcReq(protocol.MethodTabClose, map[string]any{"tab": "1234"}, 2))
			if err := srv.closeAudit(); err != nil {
				t.Fatal(err)
			}

			var released int
			for _, rec := range readRPCLog(t, stateDir) {
				if rec.Action == protocol.MethodTabClose && rec.OK {
					released++
				}
			}
			if released != 1 {
				t.Fatalf("successful tab_close records = %d, want 1", released)
			}
			if got := srv.tabIdle.Snapshot(); len(got) != 0 {
				t.Fatalf("still managed: %v", got)
			}
			if got := restartWithUserTab(t, stateDir, lifecycleUserTab); len(got) != 0 {
				t.Fatalf("user tab adopted as managed after restart: %v", got)
			}
		})
	}
}
