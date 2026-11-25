//go:build unix

package store

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Ordinary local-disk failures around the checks rotation and startup make: an
// inspection that fails is not an answer (it is returned, nothing moves on a
// guess), a published rotation stays published whatever closing the replaced
// handle says, the writability probe really writes, and an existing log that
// cannot be opened refuses startup before staging files and backups change.

// failLstat makes the inspection of every path ending in suffix fail with
// errno while *fail is set.
func failLstat(t *testing.T, suffix string, errno syscall.Errno, fail *bool) {
	t.Helper()
	swap(t, &lstatPath, func(name string) (os.FileInfo, error) {
		if *fail && strings.HasSuffix(name, suffix) {
			return nil, &os.PathError{Op: "lstat", Path: name, Err: errno}
		}
		return os.Lstat(name)
	})
}

// seedFullBackups puts a distinct generation into every backup slot.
func seedFullBackups(t *testing.T, dir string) (gen func(int) string) {
	t.Helper()
	gen = func(k int) string { return fmt.Sprintf("{\"action\":\"generation-%d\"}\n", k) }
	for k := 1; k <= maxLogBackups; k++ {
		if err := os.WriteFile(backupPath(dir, k), []byte(gen(k)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return gen
}

func requireBackupContents(t *testing.T, dir, when string, want ...string) {
	t.Helper()
	for i, w := range want {
		if got := string(mustReadFile(t, backupPath(dir, i+1))); got != w {
			t.Fatalf("%s: rpc.jsonl.%d = %q, want %q", when, i+1, got, w)
		}
	}
}

// Review P2 (08:52): publishing the previous generation fails with ENOSPC
// after rpc.jsonl.1 already got its second name rpc.jsonl.2. On the retry the
// inspection of rpc.jsonl.2 fails (EIO). That is not "an earlier attempt did
// not get this far": shifting again would leave rpc.jsonl.2 and .3 the same
// generation and drop an older one, with the append reporting success.
func TestRotationRetryStopsOnBackupInspectionError(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, rpcLogFile)
	s, want := seedRotationState(t, dir)
	gen := seedFullBackups(t, dir)

	failPublish := true
	swap(t, &renamePath, func(from, to string) error {
		if failPublish && strings.HasSuffix(from, rotateKeepSuffix) {
			return syscall.ENOSPC
		}
		return os.Rename(from, to)
	})
	setMaxLogBytes(s, 1)
	at := want["cc33"].Add(time.Minute)
	err := s.AppendRPC(gotoRecord("cc33", at))
	if !errors.Is(err, ErrRotation) || !errors.Is(err, syscall.ENOSPC) || !RecordWritten(err) {
		t.Fatalf("append whose rotation could not publish rpc.jsonl.1: got %v, want ErrRotation(ENOSPC), record written", err)
	}
	want["cc33"] = at
	// The failed attempt dropped the oldest generation, once.
	requireBackupContents(t, dir, "after ENOSPC", gen(1), gen(1), gen(2))

	// The disk has room again, but rpc.jsonl.2 cannot be inspected.
	failPublish = false
	failInspect := true
	failLstat(t, ".2", syscall.EIO, &failInspect)
	for attempt := 1; attempt <= 3; attempt++ {
		at = at.Add(time.Minute)
		err = s.AppendRPC(gotoRecord("cc33", at))
		if !errors.Is(err, ErrRotation) || !errors.Is(err, syscall.EIO) || !RecordWritten(err) {
			t.Fatalf("retry %d with rpc.jsonl.2 not inspectable: got %v, want ErrRotation(EIO), record written", attempt, err)
		}
		want["cc33"] = at
		requireBackupContents(t, dir, fmt.Sprintf("retry %d", attempt), gen(1), gen(1), gen(2))
		requireNoRotationLeftovers(t, logPath)
		requireOpenLogIs(t, s, logPath)
		requireRestart(t, dir, want)
	}

	// Inspectable again: the retry completes without dropping another one.
	failInspect = false
	oldLog := mustReadFile(t, logPath)
	at = at.Add(time.Minute)
	rec := gotoRecord("cc33", at)
	if err := s.AppendRPC(rec); err != nil {
		t.Fatalf("append after the fault cleared: %v", err)
	}
	want["cc33"] = at
	oldLog = append(append(oldLog, mustMarshalRecord(t, rec)...), '\n')
	requireBackupContents(t, dir, "after the completed rotation", string(oldLog), gen(1), gen(2))
	requireNoRotationLeftovers(t, logPath)
	requireOpenLogIs(t, s, logPath)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	requireRestart(t, dir, want)
}

// A slot that cannot be inspected is not a free slot either: on a first
// attempt (nothing shifted yet) no backup moves, whichever slot it is.
func TestRotationRefusedWhileBackupSlotCannotBeInspected(t *testing.T) {
	for _, slot := range []string{".1", ".2", ".3"} {
		t.Run("rpc.jsonl"+slot, func(t *testing.T) {
			dir := t.TempDir()
			logPath := filepath.Join(dir, rpcLogFile)
			s, want := seedRotationState(t, dir)
			gen := seedFullBackups(t, dir)

			renames := 0
			swap(t, &renamePath, func(from, to string) error { renames++; return os.Rename(from, to) })
			failInspect := true
			failLstat(t, slot, syscall.EIO, &failInspect)
			setMaxLogBytes(s, 1)
			at := want["cc33"].Add(time.Minute)
			err := s.AppendRPC(gotoRecord("cc33", at))
			if !errors.Is(err, ErrRotation) || !errors.Is(err, syscall.EIO) || !RecordWritten(err) {
				t.Fatalf("got %v, want ErrRotation(EIO), record written", err)
			}
			want["cc33"] = at
			if renames != 0 {
				t.Fatalf("%d renames with a backup slot that could not be inspected", renames)
			}
			requireBackupContents(t, dir, "after the refusal", gen(1), gen(2), gen(3))
			requireNoRotationLeftovers(t, logPath)
			requireOpenLogIs(t, s, logPath)
			requireRestart(t, dir, want)

			failInspect = false
			at = at.Add(time.Minute)
			if err := s.AppendRPC(gotoRecord("cc33", at)); err != nil {
				t.Fatalf("append after the fault cleared: %v", err)
			}
			want["cc33"] = at
			if got := string(mustReadFile(t, backupPath(dir, 2))); got != gen(1) {
				t.Fatalf("rpc.jsonl.2 = %q, want generation 1", got)
			}
			if got := string(mustReadFile(t, backupPath(dir, 3))); got != gen(2) {
				t.Fatalf("rpc.jsonl.3 = %q, want generation 2", got)
			}
			requireRestart(t, dir, want)
		})
	}
}

// Review P3 (08:52): the staging link was created but cannot be inspected.
// The cause is reported as it is — not as "the path was replaced", which a
// failed inspection cannot know — and no backup has moved.
func TestRotationStagingLinkInspectionErrorKeepsCause(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, rpcLogFile)
	s, want := seedRotationState(t, dir)
	gen := seedFullBackups(t, dir)

	failInspect := true
	failLstat(t, rotateKeepSuffix, syscall.EIO, &failInspect)
	setMaxLogBytes(s, 1)
	at := want["cc33"].Add(time.Minute)
	err := s.AppendRPC(gotoRecord("cc33", at))
	if !errors.Is(err, ErrRotation) || !errors.Is(err, syscall.EIO) || !RecordWritten(err) {
		t.Fatalf("got %v, want ErrRotation(EIO), record written", err)
	}
	if strings.Contains(err.Error(), "no longer names the open log") {
		t.Fatalf("an inspection error was reported as a replaced path: %v", err)
	}
	want["cc33"] = at
	requireBackupContents(t, dir, "after the refusal", gen(1), gen(2), gen(3))
	requireNoRotationLeftovers(t, logPath)
	requireOpenLogIs(t, s, logPath)
	requireRestart(t, dir, want)

	failInspect = false
	at = at.Add(time.Minute)
	if err := s.AppendRPC(gotoRecord("cc33", at)); err != nil {
		t.Fatalf("append after the fault cleared: %v", err)
	}
	want["cc33"] = at
	requireBackupContents(t, dir, "after the completed rotation", string(mustReadFile(t, backupPath(dir, 1))), gen(1), gen(2))
	requireRestart(t, dir, want)
}

// Review P2 (08:52): the snapshot is published and the close of the replaced
// handle fails. The rotation is not undone or repeated — the published handle
// is the log — and the append reports the close error (with the directory sync
// error when that fails too) as a maintenance error: record written, neither
// a failed rotation nor, on its own, a failed directory sync.
func TestRotationPublishedDespitePreviousHandleCloseError(t *testing.T) {
	for _, syncFails := range []bool{false, true} {
		name := "close fails"
		if syncFails {
			name = "close and directory sync fail"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			logPath := filepath.Join(dir, rpcLogFile)
			s, want := seedRotationState(t, dir)

			failSync, syncCalls := syncFails, 0
			failDirSync(t, &failSync, &syncCalls)
			// The replaced handle is the one opened as rpc.jsonl; the published
			// one was opened as rpc.jsonl.tmp.
			closes := 0
			swap(t, &closeFile, func(f *os.File) error {
				if f.Name() != logPath {
					return f.Close()
				}
				closes++
				_ = f.Close()
				return syscall.EIO
			})

			setMaxLogBytes(s, 1)
			oldLog := mustReadFile(t, logPath)
			at := want["cc33"].Add(time.Minute)
			rec := gotoRecord("cc33", at)
			err := s.AppendRPC(rec)
			want["cc33"] = at
			if err == nil || !RecordWritten(err) || !errors.Is(err, syscall.EIO) {
				t.Fatalf("append whose rotation could not close the previous handle: got %v, want a maintenance error wrapping EIO", err)
			}
			if errors.Is(err, ErrRotation) {
				t.Fatalf("a published rotation was reported as a failed one: %v", err)
			}
			if !strings.Contains(err.Error(), "closing the previous log") {
				t.Fatalf("close error not reported: %v", err)
			}
			if got := errors.Is(err, ErrDirSync); got != syncFails {
				t.Fatalf("errors.Is(err, ErrDirSync) = %v, want %v: %v", got, syncFails, err)
			}
			if closes != 1 {
				t.Fatalf("previous handle closed %d times, want 1", closes)
			}

			// Published and adopted: rpc.jsonl is the snapshot and the open
			// handle, rpc.jsonl.1 the complete previous log.
			requireOpenLogIs(t, s, logPath)
			requireNoRotationLeftovers(t, logPath)
			oldLog = append(append(oldLog, mustMarshalRecord(t, rec)...), '\n')
			if got := mustReadFile(t, backupPath(dir, 1)); !bytes.Equal(got, oldLog) {
				t.Fatalf("rpc.jsonl.1 is not the previous log:\n%s", got)
			}
			if records, invalid := readLogLines(t, logPath); invalid != 0 || len(records) != len(want) {
				t.Fatalf("rpc.jsonl is not the snapshot: %d records, %d invalid", len(records), invalid)
			}
			requireActivity(t, s, want)

			// The new handle keeps taking records; the rotation is not rerun and
			// the close error (its handle is gone) is not reported again.
			setMaxLogBytes(s, defaultMaxLogBytes)
			at = want["aa11"].Add(time.Minute)
			err = s.AppendRPC(gotoRecord("aa11", at))
			want["aa11"] = at
			if syncFails {
				if !errors.Is(err, ErrDirSync) || !RecordWritten(err) || errors.Is(err, ErrRotation) || strings.Contains(err.Error(), "closing the previous log") {
					t.Fatalf("append with the directory sync still owed: got %v, want ErrDirSync only", err)
				}
				failSync = false
				at = at.Add(time.Minute)
				err = s.AppendRPC(gotoRecord("aa11", at))
				want["aa11"] = at
			}
			if err != nil {
				t.Fatalf("append to the published log: %v", err)
			}
			if records, _ := readLogLines(t, logPath); records[len(records)-1].Target != managedTarget("aa11") {
				t.Fatalf("later record did not reach rpc.jsonl: %+v", records[len(records)-1])
			}
			if closes != 1 {
				t.Fatalf("previous handle closed %d times, want 1", closes)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			requireRestart(t, dir, want)
		})
	}
}

// Review P2 (08:52): creating an empty file proves nothing about writing. The
// probe writes one byte to its own file and reports a failed write, a short
// write and a failed close; whatever failed, it removes its own file and
// nothing else, and startup stops before a log is created.
func TestWritabilityProbeWritesAndCleansUp(t *testing.T) {
	isProbe := func(f *os.File) bool { return strings.HasPrefix(filepath.Base(f.Name()), ".write-test-") }
	failWrite := func(n int, err error) func(t *testing.T) {
		return func(t *testing.T) {
			swap(t, &probeWrite, func(f *os.File, p []byte) (int, error) {
				if !isProbe(f) || len(p) != 1 {
					t.Errorf("probe wrote %d bytes to %s", len(p), f.Name())
				}
				return n, err
			})
		}
	}
	failClose := func(t *testing.T) {
		swap(t, &closeFile, func(f *os.File) error {
			if !isProbe(f) {
				return f.Close()
			}
			_ = f.Close()
			return syscall.EIO
		})
	}
	cases := []struct {
		name   string
		inject func(t *testing.T)
		want   []error
		text   []string
	}{
		{"write error", failWrite(0, syscall.EFBIG), []error{syscall.EFBIG}, []string{"write probe file"}},
		{"short write", failWrite(0, nil), []error{io.ErrShortWrite}, []string{"write probe file"}},
		{"close error", failClose, []error{syscall.EIO}, []string{"close probe file"}},
		{"write and close error", func(t *testing.T) { failWrite(0, syscall.EFBIG)(t); failClose(t) },
			[]error{syscall.EFBIG, syscall.EIO}, []string{"write probe file", "close probe file"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			operator := filepath.Join(dir, ".write-test")
			older := filepath.Join(dir, ".write-test-operator")
			for _, path := range []string{operator, older} {
				writeFileMode(t, path, []byte("operator data\n"), 0o644)
			}
			tc.inject(t)

			s, err := Open(OpenConfig{StateDir: dir})
			if err == nil {
				_ = s.Close()
				t.Fatal("Open succeeded although the probe could not write")
			}
			if !strings.Contains(err.Error(), "not writable") {
				t.Fatalf("unexpected error: %v", err)
			}
			for _, want := range tc.want {
				if !errors.Is(err, want) {
					t.Fatalf("error %v does not wrap %v", err, want)
				}
			}
			for _, want := range tc.text {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %v does not say %q", err, want)
				}
			}

			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 2 {
				t.Fatalf("state dir after the failed probe: %v", entries)
			}
			for _, path := range []string{operator, older} {
				if got := string(mustReadFile(t, path)); got != "operator data\n" {
					t.Fatalf("%s changed: %q", filepath.Base(path), got)
				}
				assertPerm(t, path, 0o644)
			}
		})
	}

	// Without a fault the byte really is written (and the file removed).
	t.Run("writes one byte", func(t *testing.T) {
		dir := t.TempDir()
		var size int64 = -1
		swap(t, &closeFile, func(f *os.File) error {
			if fi, err := f.Stat(); err == nil && isProbe(f) {
				size = fi.Size()
			}
			return f.Close()
		})
		if err := probeDirWritable(dir); err != nil {
			t.Fatal(err)
		}
		if size != 1 {
			t.Fatalf("probe file size at close: %d, want 1", size)
		}
		if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
			t.Fatalf("probe left files behind: %v %v", entries, err)
		}
	})
}

// Review P3 (08:52): a regular rpc.jsonl this process may not open for
// writing (0400) fails startup at the open — before the staging files of an
// interrupted rotation are removed and before a backup's mode is tightened.
// Once the log is usable the usual cleanup happens.
func TestStartupWithUnopenableActiveLogLeavesStagingAndBackups(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("mode bits do not stop root from opening the log")
	}
	dir := filepath.Join(t.TempDir(), "state")
	mkdirMode(t, dir, 0o700)
	// Files created below must not inherit entries that grant write access
	// despite mode 0400.
	dropInheritedACL(t, dir)
	logPath := filepath.Join(dir, rpcLogFile)
	active := managedOpenLine(t, "aa11")
	writeFileMode(t, logPath, active, 0o400)
	if f, err := os.OpenFile(logPath, os.O_RDWR, 0); err == nil {
		_ = f.Close()
		t.Fatal("mode 0400 does not stop this user from opening the log read-write; the fixture proves nothing")
	}
	staged := map[string][]byte{
		logPath + rotateTmpSuffix:  managedOpenLine(t, "bb22"),
		logPath + rotateKeepSuffix: managedOpenLine(t, "cc33"),
	}
	for path, data := range staged {
		writeFileMode(t, path, data, 0o600)
	}
	previous := managedOpenLine(t, "dd44")
	writeFileMode(t, backupPath(dir, 1), previous, 0o640)
	older := managedOpenLine(t, "ee55")
	writeFileMode(t, backupPath(dir, 2), older, 0o644)

	for attempt := 1; attempt <= 2; attempt++ {
		s, err := Open(OpenConfig{StateDir: dir})
		if err == nil {
			_ = s.Close()
			t.Fatalf("attempt %d: Open succeeded on a log it cannot write", attempt)
		}
		if !errors.Is(err, os.ErrPermission) || !strings.Contains(err.Error(), "open rpc log") {
			t.Fatalf("attempt %d: unexpected error: %v", attempt, err)
		}
		for path, data := range staged {
			if got := mustReadFile(t, path); !bytes.Equal(got, data) {
				t.Fatalf("attempt %d: %s changed: %q", attempt, filepath.Base(path), got)
			}
		}
		assertPerm(t, backupPath(dir, 1), 0o640)
		assertPerm(t, backupPath(dir, 2), 0o644)
		assertPerm(t, logPath, 0o400)
		if !bytes.Equal(mustReadFile(t, backupPath(dir, 1)), previous) || !bytes.Equal(mustReadFile(t, backupPath(dir, 2)), older) ||
			!bytes.Equal(mustReadFile(t, logPath), active) {
			t.Fatalf("attempt %d: a log file's content changed", attempt)
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 5 {
			t.Fatalf("attempt %d: state dir entries: %v (%v)", attempt, entries, err)
		}
	}

	if err := os.Chmod(logPath, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(OpenConfig{StateDir: dir})
	if err != nil {
		t.Fatalf("Open with a usable log: %v", err)
	}
	defer s.Close()
	if got := s.ReplayManagedTabActivity(); len(got) != 1 || got["target-aa11"].IsZero() {
		t.Fatalf("managed set after the start: %v", got)
	}
	requireNoRotationLeftovers(t, logPath)
	assertPerm(t, backupPath(dir, 1), 0o600)
	assertPerm(t, backupPath(dir, 2), 0o600)
}
