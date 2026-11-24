package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/coinpost/go-bb-browser/pkg/protocol"
)

// Rotation publishes a complete snapshot atomically and keeps the previous
// generation; a failure at any step leaves the old log active and writable.
// These tests inject a failure into every step and restart on what is on disk.

var errInjected = errors.New("injected failure")

// swap replaces a package-level file-system step for the duration of the test.
func swap[T any](t *testing.T, target *T, with T) {
	t.Helper()
	orig := *target
	*target = with
	t.Cleanup(func() { *target = orig })
}

func gotoRecord(tab string, at time.Time) LogRecord {
	return LogRecord{
		Action: protocol.MethodGoto,
		Body:   json.RawMessage(`{"jsonrpc":"2.0","method":"goto","params":{"url":"https://example.com"},"id":7}`),
		Target: managedTarget(tab),
		OK:     true,
		Time:   at,
	}
}

func setMaxLogBytes(s *Store, n int64) {
	s.mu.Lock()
	s.maxLogBytes = n
	s.mu.Unlock()
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func requireAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s should not exist (err=%v)", filepath.Base(path), err)
	}
}

// requireActivity checks the complete managed set with last-activity times.
func requireActivity(t *testing.T, s *Store, want map[string]time.Time) {
	t.Helper()
	got := s.ReplayManagedTabActivity()
	if len(got) != len(want) {
		t.Fatalf("managed set: got %v want %v", got, want)
	}
	for tab, at := range want {
		if g, ok := got[managedTarget(tab)]; !ok || !g.Equal(at) {
			t.Fatalf("managed tab %q: got %v (present=%v) want %v; set %v", tab, g, ok, at, got)
		}
	}
}

// requireRestart opens a second store on dir, as a daemon restarted on exactly
// the current on-disk state would, and checks what it recovers.
func requireRestart(t *testing.T, dir string, want map[string]time.Time) {
	t.Helper()
	s, err := Open(OpenConfig{StateDir: dir})
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	defer s.Close()
	requireActivity(t, s, want)
}

// seedRotationState fills dir with an older backup generation and a log holding
// a managed tab with later activity (aa11), a retired tab (bb22) and a second
// managed tab (cc33). It returns the store and the expected managed set.
func seedRotationState(t *testing.T, dir string) (*Store, map[string]time.Time) {
	t.Helper()
	t0 := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	s := mustOpenDisk(t, dir)
	// Written after Open: a lone rpc.jsonl.1 would be restored as the log.
	if err := os.WriteFile(filepath.Join(dir, rpcLogFile+".1"), []byte("{\"action\":\"older-generation\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tab := range []string{"aa11", "bb22", "cc33"} {
		if err := appendManaged(s, tab, t0); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AppendRPC(gotoRecord("aa11", t0.Add(10*time.Minute))); err != nil {
		t.Fatal(err)
	}
	if retired, err := s.RetireManagedTarget(managedTarget("bb22")); err != nil || !retired {
		t.Fatalf("retire: %v %v", retired, err)
	}
	return s, map[string]time.Time{"aa11": t0.Add(10 * time.Minute), "cc33": t0}
}

func TestRotationFailureKeepsLogWritableAndRecoverable(t *testing.T) {
	isTmp := func(name string) bool { return strings.HasSuffix(name, rotateTmpSuffix) }
	cases := []struct {
		name   string
		inject func(t *testing.T, s *Store)
	}{
		{"snapshot create", func(t *testing.T, s *Store) {
			swap(t, &openFile, func(name string, flag int, perm os.FileMode) (*os.File, error) {
				if isTmp(name) {
					return nil, errInjected
				}
				return os.OpenFile(name, flag, perm)
			})
		}},
		{"snapshot write error", func(t *testing.T, s *Store) {
			setWriteLog(s, func(f *os.File, p []byte) (int, error) {
				if isTmp(f.Name()) {
					return 0, errInjected
				}
				return f.Write(p)
			})
		}},
		{"snapshot partial write", func(t *testing.T, s *Store) {
			setWriteLog(s, func(f *os.File, p []byte) (int, error) {
				if isTmp(f.Name()) {
					n, _ := f.Write(p[:len(p)/2])
					return n, errInjected
				}
				return f.Write(p)
			})
		}},
		{"snapshot short write without error", func(t *testing.T, s *Store) {
			setWriteLog(s, func(f *os.File, p []byte) (int, error) {
				if isTmp(f.Name()) {
					return f.Write(p[:len(p)-1])
				}
				return f.Write(p)
			})
		}},
		{"snapshot sync", func(t *testing.T, s *Store) {
			swap(t, &syncFile, func(f *os.File) error {
				if isTmp(f.Name()) {
					return errInjected
				}
				return f.Sync()
			})
		}},
		{"backup shift", func(t *testing.T, s *Store) {
			swap(t, &renamePath, func(from, to string) error {
				if strings.HasSuffix(from, ".1") {
					return errInjected
				}
				return os.Rename(from, to)
			})
		}},
		{"keep previous", func(t *testing.T, s *Store) {
			swap(t, &linkPath, func(from, to string) error { return errInjected })
		}},
		{"publish", func(t *testing.T, s *Store) {
			swap(t, &renamePath, func(from, to string) error {
				if isTmp(from) {
					return errInjected
				}
				return os.Rename(from, to)
			})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			logPath := filepath.Join(dir, rpcLogFile)
			s, want := seedRotationState(t, dir)
			linesBefore, _ := readLogLines(t, logPath)

			origOpen, origLink, origRename, origSync := openFile, linkPath, renamePath, syncFile
			tc.inject(t, s)
			setMaxLogBytes(s, 1)

			// The triggering append is written, and told that rotation failed.
			at := want["cc33"].Add(20 * time.Minute)
			err := s.AppendRPC(gotoRecord("cc33", at))
			if !errors.Is(err, ErrRotation) {
				t.Fatalf("append with failing rotation: got %v, want ErrRotation", err)
			}
			if tc.name != "snapshot short write without error" && !errors.Is(err, errInjected) {
				t.Fatalf("cause not propagated: %v", err)
			}
			want["cc33"] = at

			// While the fault lasts every append still lands in the old log
			// (the handle survived) and keeps reporting the failed retry.
			at = at.Add(time.Minute)
			if err := s.AppendRPC(gotoRecord("aa11", at)); !errors.Is(err, ErrRotation) {
				t.Fatalf("second append: got %v, want ErrRotation", err)
			}
			want["aa11"] = at
			if err := appendManaged(s, "dd44", at); !errors.Is(err, ErrRotation) {
				t.Fatalf("lifecycle append: got %v, want ErrRotation", err)
			}
			want["dd44"] = at

			lines, invalid := readLogLines(t, logPath)
			if len(lines) != len(linesBefore)+3 || invalid != 0 {
				t.Fatalf("old log: %d valid (+%d) / %d invalid lines, want +3 / 0", len(lines), len(lines)-len(linesBefore), invalid)
			}
			requireLogBytesMatchFile(t, s, logPath)
			requireAbsent(t, logPath+rotateTmpSuffix)
			requireActivity(t, s, want)
			// A daemon restarted on this state recovers everything.
			requireRestart(t, dir, want)

			// Fault gone: the next append rotates.
			openFile, linkPath, renamePath, syncFile = origOpen, origLink, origRename, origSync
			setWriteLog(s, nil)
			oldLog := mustReadFile(t, logPath)
			at = at.Add(time.Minute)
			rec := gotoRecord("cc33", at)
			if err := s.AppendRPC(rec); err != nil {
				t.Fatalf("append after fault cleared: %v", err)
			}
			want["cc33"] = at
			oldLog = append(append(oldLog, mustMarshalRecord(t, rec)...), '\n')

			// Previous generation complete, older one not pushed out twice by
			// the failed attempts, snapshot only in the new log.
			if got := mustReadFile(t, logPath+".1"); !bytes.Equal(got, oldLog) {
				t.Fatalf("rpc.jsonl.1 is not the previous log:\n%s\nwant:\n%s", got, oldLog)
			}
			if got := string(mustReadFile(t, logPath+".2")); !strings.Contains(got, "older-generation") {
				t.Fatalf("rpc.jsonl.2 lost the older generation: %q", got)
			}
			requireAbsent(t, logPath+".3")
			requireAbsent(t, logPath+rotateTmpSuffix)
			snap, invalid := readLogLines(t, logPath)
			if len(snap) != len(want) || invalid != 0 {
				t.Fatalf("snapshot: %d valid / %d invalid lines, want %d / 0", len(snap), invalid, len(want))
			}
			for _, r := range snap {
				if r.Action != ActionManagedTabOpen || r.Target == managedTarget("bb22") {
					t.Fatalf("unexpected snapshot record %+v", r)
				}
			}
			requireLogBytesMatchFile(t, s, logPath)

			// The published log is the one being appended to.
			setMaxLogBytes(s, defaultMaxLogBytes)
			at = at.Add(time.Minute)
			if err := s.AppendRPC(gotoRecord("dd44", at)); err != nil {
				t.Fatal(err)
			}
			want["dd44"] = at
			requireLogBytesMatchFile(t, s, logPath)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			requireRestart(t, dir, want)
		})
	}
}

// With nothing managed the rotation publishes an empty log; the previous
// generation is kept and a restart recovers the empty set.
func TestRotationWithEmptyManagedSetPublishesEmptyLog(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, rpcLogFile)
	s := mustOpenDisk(t, dir)
	t0 := time.Now().UTC().Truncate(time.Second)
	if err := appendManaged(s, "aa11", t0); err != nil {
		t.Fatal(err)
	}
	setMaxLogBytes(s, 1)
	if retired, err := s.RetireManagedTarget(managedTarget("aa11")); err != nil || !retired {
		t.Fatalf("retire: %v %v", retired, err)
	}
	if got := mustReadFile(t, logPath); len(got) != 0 {
		t.Fatalf("log after rotating an empty set: %q", got)
	}
	if prev, invalid := readLogLines(t, logPath+".1"); len(prev) != 2 || invalid != 0 {
		t.Fatalf("previous generation: %d valid / %d invalid lines", len(prev), invalid)
	}
	requireLogBytesMatchFile(t, s, logPath)
	requireRestart(t, dir, map[string]time.Time{})

	setMaxLogBytes(s, defaultMaxLogBytes)
	if err := appendManaged(s, "bb22", t0); err != nil {
		t.Fatal(err)
	}
	requireRestart(t, dir, map[string]time.Time{"bb22": t0})
}

// A process stopped while writing the snapshot leaves a fragment next to the
// intact log. It is never replayed, and is removed.
func TestRestartWithUnpublishedSnapshotFragment(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, rpcLogFile)
	s, want := seedRotationState(t, dir)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	fragment := append(mustMarshalRecord(t, tabNewRecord("zz99", time.Now().UTC())), "\n{\"action\":\"managed_tab_op"...)
	if err := os.WriteFile(logPath+rotateTmpSuffix, fragment, 0o600); err != nil {
		t.Fatal(err)
	}
	before := mustReadFile(t, logPath)

	requireRestart(t, dir, want)
	requireAbsent(t, logPath+rotateTmpSuffix)
	if got := mustReadFile(t, logPath); !bytes.Equal(got, before) {
		t.Fatalf("log changed by restart:\n%s", got)
	}
}

// A process stopped after linking the log to rpc.jsonl.1 and before publishing
// leaves the old log under both names. Restart recovers from it, and the next
// rotation does not store that generation twice.
func TestRestartBetweenKeepPreviousAndPublish(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, rpcLogFile)
	s, want := seedRotationState(t, dir)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(logPath+".1", logPath+".2"); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(logPath, logPath+".1"); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}

	s2 := mustOpenDisk(t, dir)
	requireActivity(t, s2, want)
	oldLog := mustReadFile(t, logPath)
	setMaxLogBytes(s2, 1)
	at := want["aa11"].Add(time.Hour)
	rec := gotoRecord("aa11", at)
	if err := s2.AppendRPC(rec); err != nil {
		t.Fatal(err)
	}
	want["aa11"] = at
	oldLog = append(append(oldLog, mustMarshalRecord(t, rec)...), '\n')

	if got := mustReadFile(t, logPath+".1"); !bytes.Equal(got, oldLog) {
		t.Fatalf("rpc.jsonl.1 is not the previous log:\n%s", got)
	}
	if got := string(mustReadFile(t, logPath+".2")); !strings.Contains(got, "older-generation") {
		t.Fatalf("older generation displaced: %q", got)
	}
	requireAbsent(t, logPath+".3")
	requireRestart(t, dir, want)
}

// rpc.jsonl.1 without rpc.jsonl is what a daemon before the atomic rotation
// left when it stopped between moving the log away and creating its successor.
// Startup must not begin an empty log over the recoverable generation.
func TestRestartWithLogMovedAwayRestoresPreviousGeneration(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, rpcLogFile)
	s, want := seedRotationState(t, dir)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// The old sequence: backups shifted, log renamed to .1, then the stop.
	if err := os.Rename(logPath+".1", logPath+".2"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(logPath, logPath+".1"); err != nil {
		t.Fatal(err)
	}
	moved := mustReadFile(t, logPath+".1")

	t.Run("restore failure fails startup without creating a log", func(t *testing.T) {
		swap(t, &linkPath, func(from, to string) error { return errInjected })
		if s, err := Open(OpenConfig{StateDir: dir}); !errors.Is(err, errInjected) {
			if err == nil {
				_ = s.Close()
			}
			t.Fatalf("Open: got %v, want the link failure", err)
		}
		requireAbsent(t, logPath)
		if got := mustReadFile(t, logPath+".1"); !bytes.Equal(got, moved) {
			t.Fatalf("previous generation changed:\n%s", got)
		}
	})

	s2 := mustOpenDisk(t, dir)
	requireActivity(t, s2, want)
	if got := mustReadFile(t, logPath+".1"); !bytes.Equal(got, moved) {
		t.Fatalf("previous generation changed by restore:\n%s", got)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(logPath)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != logFilePerm {
			t.Fatalf("restored log mode %v", fi.Mode().Perm())
		}
	}

	// Appends and the next rotation work on the restored log; the restored
	// generation is kept once.
	setMaxLogBytes(s2, 1)
	at := want["cc33"].Add(time.Hour)
	rec := gotoRecord("cc33", at)
	if err := s2.AppendRPC(rec); err != nil {
		t.Fatal(err)
	}
	want["cc33"] = at
	wantPrev := append(append(moved, mustMarshalRecord(t, rec)...), '\n')
	if got := mustReadFile(t, logPath+".1"); !bytes.Equal(got, wantPrev) {
		t.Fatalf("rpc.jsonl.1 after rotation:\n%s", got)
	}
	if got := string(mustReadFile(t, logPath+".2")); !strings.Contains(got, "older-generation") {
		t.Fatalf("older generation displaced: %q", got)
	}
	requireAbsent(t, logPath+".3")
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	requireRestart(t, dir, want)
}

// First start (no log, no backups) still begins with an empty log.
func TestFirstStartCreatesEmptyLog(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s := mustOpenDisk(t, dir)
	requireActivity(t, s, map[string]time.Time{})
	if got := mustReadFile(t, filepath.Join(dir, rpcLogFile)); len(got) != 0 {
		t.Fatalf("first log not empty: %q", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("state dir after first start: %v", entries)
	}
}

// The startup writability probe must not touch a file the operator (or an
// older daemon) left under the name the probe used to overwrite and delete.
func TestOpenLeavesExistingWriteTestFileAlone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".write-test")
	content := []byte("operator data\n")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	mtime := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		s, err := Open(OpenConfig{StateDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}

	after, err := os.Lstat(path)
	if err != nil {
		t.Fatalf(".write-test removed: %v", err)
	}
	if got := mustReadFile(t, path); !bytes.Equal(got, content) {
		t.Fatalf(".write-test content changed: %q", got)
	}
	if after.Mode() != before.Mode() || !after.ModTime().Equal(before.ModTime()) || !os.SameFile(before, after) {
		t.Fatalf(".write-test changed: mode %v→%v mtime %v→%v", before.Mode(), after.Mode(), before.ModTime(), after.ModTime())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != ".write-test" && e.Name() != rpcLogFile {
			t.Fatalf("startup left %q in the state dir", e.Name())
		}
	}
}

func TestProbeDirWritable(t *testing.T) {
	dir := t.TempDir()
	if err := probeDirWritable(dir); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("probe left files behind: %v %v", entries, err)
	}
	if err := probeDirWritable(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("probe succeeded in a directory that does not exist")
	}
}

// A state dir files cannot be created in still fails startup.
func TestOpenFailsWhenStateDirNotWritable(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions and a non-root user")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if f, err := os.CreateTemp(dir, "check-*"); err == nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		t.Skip("mode 0500 does not stop file creation here (ACL or privileged user)")
	}
	s, err := Open(OpenConfig{StateDir: dir})
	if err == nil {
		_ = s.Close()
		t.Fatal("Open succeeded on a read-only state dir")
	}
	if !strings.Contains(err.Error(), "not writable") {
		t.Fatalf("unexpected error: %v", err)
	}
}
