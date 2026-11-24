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
// retirement was lost — a restart on the same state dir proves it was not.
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
	requireWarn(msgManagedOpenWritten, "not persisted", "will not be restored")
	if n := reopenedManaged(); n != 1 {
		t.Fatalf("registration reported as written, but a restart recovers %d managed tabs", n)
	}

	srv.lifeMu.Lock()
	srv.forgetTabManagedLocked(tid)
	srv.lifeMu.Unlock()
	requireWarn(msgManagedRetireWritten, "not persisted", "stale entry")
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
	for _, written := range []error{store.ErrRotation, store.ErrDirSync} {
		if got := auditFailureMessage(written); !strings.Contains(got, "record written") {
			t.Fatalf("audit message for %v: %q", written, got)
		}
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
