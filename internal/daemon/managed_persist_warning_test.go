package daemon

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chromedp/cdproto/target"
	"github.com/coinpost/go-bb-browser/internal/store"
)

// A lifecycle write that returns store.ErrRotation wrote its record: only the
// rotation it triggered failed. The warning must not claim the registration or
// retirement was lost — a restart on the same state dir proves it was not —
// and must not claim it is persisted either (see the unlinked-log test below).
func TestManagedLifecycleWarningsDistinguishWrittenRecords(t *testing.T) {
	dir := t.TempDir()
	// Every rotation fails before publication: its private snapshot path is
	// occupied by a non-empty directory the store cannot clear.
	blocker := filepath.Join(dir, store.RPCLogFile()+".tmp")
	if err := os.MkdirAll(filepath.Join(blocker, "keep"), 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(store.OpenConfig{StateDir: dir, MaxLogBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	var logs bytes.Buffer
	srv := &Server{store: st, logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	tid := target.ID("0123456789ABCDEF0123456789ABCDEF")

	reopenedManaged := func() int {
		t.Helper()
		again, err := store.Open(store.OpenConfig{StateDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		defer again.Close()
		return len(again.ManagedTabs())
	}
	requireWarn := func(want string, not ...string) {
		t.Helper()
		got := logs.String()
		logs.Reset()
		if strings.Count(got, "level=WARN") != 1 || !strings.Contains(got, want) || !strings.Contains(got, string(tid)) {
			t.Fatalf("want one warning containing %q, got:\n%s", want, got)
		}
		for _, n := range not {
			if strings.Contains(got, n) {
				t.Fatalf("warning claims %q:\n%s", n, got)
			}
		}
	}

	srv.lifeMu.Lock()
	srv.markTabManagedLocked(tid)
	srv.lifeMu.Unlock()
	requireWarn(msgManagedOpenWritten, "persisted", "will not be restored")
	if n := reopenedManaged(); n != 1 {
		t.Fatalf("registration reported as written, but a restart recovers %d managed tabs", n)
	}

	srv.lifeMu.Lock()
	srv.forgetTabManagedLocked(tid)
	srv.lifeMu.Unlock()
	requireWarn(msgManagedRetireWritten, "persisted", "stale entry")
	if n := reopenedManaged(); n != 0 {
		t.Fatalf("retirement reported as written, but a restart recovers %d managed tabs", n)
	}

	// A write that did not reach the log keeps the original wording, and the
	// audit path names the same distinction.
	failed := errors.New("write rpc.jsonl: input/output error")
	srv.logManagedPersistFailure(msgManagedOpenNotPersisted, msgManagedOpenWritten, tid, failed)
	requireWarn(msgManagedOpenNotPersisted, "maintenance")
	srv.logManagedPersistFailure(msgManagedRetireNotPersisted, msgManagedRetireWritten, tid, failed)
	requireWarn(msgManagedRetireNotPersisted, "maintenance")
	if got := auditFailureMessage(failed); got != "append rpc log failed" {
		t.Fatalf("audit message for a failed write: %q", got)
	}
	// A bare sentinel proves nothing about a record; only an error an append
	// returned for a written record does.
	for _, bare := range []error{store.ErrRotation, store.ErrDirSync} {
		if got := auditFailureMessage(bare); got != "append rpc log failed" {
			t.Fatalf("audit message for bare %v: %q", bare, got)
		}
	}
	written := st.AppendRPC(store.LogRecord{Action: "goto", Body: []byte(`{"jsonrpc":"2.0","method":"goto","id":1}`), Target: string(tid), OK: true})
	if !errors.Is(written, store.ErrRotation) || !store.RecordWritten(written) {
		t.Fatalf("append with rotation blocked: %v", written)
	}
	if got := auditFailureMessage(written); !strings.Contains(got, "record written") || strings.Contains(got, "persisted") || !strings.Contains(got, "not confirmed") {
		t.Fatalf("audit message for a written record: %q", got)
	}

	// Shutdown: nothing actionable, not a warning.
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	srv.lifeMu.Lock()
	srv.markTabManagedLocked(tid)
	srv.lifeMu.Unlock()
	if got := logs.String(); strings.Contains(got, "level=WARN") || !strings.Contains(got, "level=DEBUG") {
		t.Fatalf("closed store should log at debug only:\n%s", got)
	}
}

// Review P2: with the name rpc.jsonl gone the record still reaches the open
// (now nameless) handle and store.RecordWritten is true, but a restart cannot
// read it. The warning must therefore not say the tab is persisted or will be
// restored — the restart below shows it is not.
func TestManagedLifecycleWarningDoesNotPromiseRecoveryForUnlinkedLog(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(store.OpenConfig{StateDir: dir, MaxLogBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := os.Remove(filepath.Join(dir, store.RPCLogFile())); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	srv := &Server{store: st, logger: slog.New(slog.NewTextHandler(&logs, nil))}
	tid := target.ID("0123456789ABCDEF0123456789ABCDEF")
	srv.lifeMu.Lock()
	srv.markTabManagedLocked(tid)
	srv.lifeMu.Unlock()

	got := logs.String()
	if strings.Count(got, "level=WARN") != 1 || !strings.Contains(got, msgManagedOpenWritten) || !strings.Contains(got, string(tid)) {
		t.Fatalf("want one conservative warning, got:\n%s", got)
	}
	// The message itself, without the wrapped error text.
	for _, claim := range []string{"persisted", "will be restored", "durable"} {
		if strings.Contains(msgManagedOpenWritten, claim) || strings.Contains(msgManagedRetireWritten, claim) {
			t.Fatalf("written-record warning claims %q", claim)
		}
	}
	if !strings.Contains(got, "not confirmed") || !strings.Contains(got, "no longer names the open log") && !strings.Contains(got, "path unusable") {
		t.Fatalf("warning does not say what is unconfirmed and why:\n%s", got)
	}
	if len(st.ManagedTabs()) != 1 {
		t.Fatal("in-memory managed set must still hold the tab")
	}

	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := store.Open(store.OpenConfig{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if n := len(again.ManagedTabs()); n != 0 {
		t.Fatalf("restart recovered %d managed tabs from an unlinked log; the scenario no longer shows what the warning must not promise", n)
	}
}
