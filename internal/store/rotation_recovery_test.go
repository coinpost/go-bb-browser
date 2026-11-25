package store

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// A state dir belongs to one Store. These tests cover what that one process
// must survive: the active log's name disappearing under it, a directory sync
// failing after publication, and a rpc.jsonl.1 startup cannot use.

// requireOpenLogIs checks that the handle the store appends to is the file
// named path (the published log was adopted / the old one kept).
func requireOpenLogIs(t *testing.T, s *Store, path string) {
	t.Helper()
	named, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.logFile == nil {
		t.Fatal("store has no open log handle")
	}
	open, err := s.logFile.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(named, open) {
		t.Fatalf("open log handle is not %s", filepath.Base(path))
	}
}

func copyActivity(m map[string]time.Time) map[string]time.Time {
	out := make(map[string]time.Time, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// rotateOnce makes the next append rotate successfully and returns the managed
// set that rotation left recoverable from the named files.
func rotateOnce(t *testing.T, s *Store, want map[string]time.Time) {
	t.Helper()
	setMaxLogBytes(s, 1)
	at := want["cc33"].Add(time.Minute)
	if err := s.AppendRPC(gotoRecord("cc33", at)); err != nil {
		t.Fatalf("rotation: %v", err)
	}
	want["cc33"] = at
	setMaxLogBytes(s, defaultMaxLogBytes)
}

// Review P1: successful rotation, active log unlinked, publication failing.
// The rotation must be refused before any backup moves, so rpc.jsonl.1 — the
// only generation startup can recover from — stays in place.
func TestRotationRefusedWhenActivePathMissingKeepsRecoverableGeneration(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, rpcLogFile)
	s, want := seedRotationState(t, dir)
	rotateOnce(t, s, want)
	recoverable := copyActivity(want)
	prev1 := mustReadFile(t, logPath+".1")
	prev2 := mustReadFile(t, logPath+".2")
	requireAbsent(t, logPath+".3")

	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	var renames, links int
	swap(t, &renamePath, func(from, to string) error { renames++; return syscall.EIO })
	swap(t, &linkPath, func(from, to string) error { links++; return syscall.EIO })
	setMaxLogBytes(s, 1)

	requireUntouched := func() {
		t.Helper()
		if renames != 0 || links != 0 {
			t.Fatalf("refused rotation touched the namespace: %d renames, %d links", renames, links)
		}
		if got := mustReadFile(t, logPath+".1"); !bytes.Equal(got, prev1) {
			t.Fatalf("rpc.jsonl.1 changed:\n%s", got)
		}
		if got := mustReadFile(t, logPath+".2"); !bytes.Equal(got, prev2) {
			t.Fatalf("rpc.jsonl.2 changed:\n%s", got)
		}
		requireAbsent(t, logPath+".3")
		requireAbsent(t, logPath)
		requireAbsent(t, logPath+rotateTmpSuffix)
	}

	at := want["aa11"].Add(time.Hour)
	err := s.AppendRPC(gotoRecord("aa11", at))
	if !errors.Is(err, ErrRotation) || errors.Is(err, ErrDirSync) || !RecordWritten(err) {
		t.Fatalf("append with unlinked log: got %v, want ErrRotation", err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cause not propagated: %v", err)
	}
	want["aa11"] = at
	requireUntouched()

	// The old handle is still open and written to; every append retries.
	s.mu.Lock()
	before := s.logBytes
	s.mu.Unlock()
	if err := appendManaged(s, "dd44", at); !errors.Is(err, ErrRotation) {
		t.Fatalf("lifecycle append: got %v, want ErrRotation", err)
	}
	want["dd44"] = at
	s.mu.Lock()
	fi, serr := s.logFile.Stat()
	after := s.logBytes
	s.mu.Unlock()
	if serr != nil || after <= before || fi.Size() != after {
		t.Fatalf("old handle not written: logBytes %d -> %d, size %v (%v)", before, after, fi, serr)
	}
	requireActivity(t, s, want)
	requireUntouched()

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// What was appended to the unlinked file went with it; the generation the
	// rotation kept is recovered in full, not an empty log.
	renamePath, linkPath = os.Rename, os.Link
	requireRestart(t, dir, recoverable)
	if got := mustReadFile(t, logPath+".1"); !bytes.Equal(got, prev1) {
		t.Fatalf("rpc.jsonl.1 changed by restart:\n%s", got)
	}
}

// The active name restored (same file): the refused rotation is retried by the
// next append and nothing appended meanwhile is lost. A different file under
// the name is refused the same way and left alone.
func TestRotationRetriesAfterActivePathRestored(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, rpcLogFile)
	aside := filepath.Join(dir, "moved-aside")
	s, want := seedRotationState(t, dir)
	rotateOnce(t, s, want)
	prev1 := mustReadFile(t, logPath+".1")

	if err := os.Rename(logPath, aside); err != nil {
		t.Fatal(err)
	}
	setMaxLogBytes(s, 1)
	at := want["aa11"].Add(time.Hour)
	if err := s.AppendRPC(gotoRecord("aa11", at)); !errors.Is(err, ErrRotation) {
		t.Fatalf("append with log moved away: got %v, want ErrRotation", err)
	}
	want["aa11"] = at

	foreign := []byte("{\"action\":\"someone-else\"}\n")
	if err := os.WriteFile(logPath, foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	at = at.Add(time.Minute)
	if err := appendManaged(s, "dd44", at); !errors.Is(err, ErrRotation) {
		t.Fatalf("append with foreign file under the log name: got %v, want ErrRotation", err)
	}
	want["dd44"] = at
	if got := mustReadFile(t, logPath); !bytes.Equal(got, foreign) {
		t.Fatalf("foreign file modified:\n%s", got)
	}
	if got := mustReadFile(t, logPath+".1"); !bytes.Equal(got, prev1) {
		t.Fatalf("rpc.jsonl.1 changed by refused rotations:\n%s", got)
	}
	requireAbsent(t, logPath+".3")
	requireAbsent(t, logPath+rotateTmpSuffix)

	if err := os.Rename(aside, logPath); err != nil {
		t.Fatal(err)
	}
	requireOpenLogIs(t, s, logPath)
	oldLog := mustReadFile(t, logPath)
	at = at.Add(time.Minute)
	rec := gotoRecord("cc33", at)
	if err := s.AppendRPC(rec); err != nil {
		t.Fatalf("retry after the name was restored: %v", err)
	}
	want["cc33"] = at
	oldLog = append(append(oldLog, mustMarshalRecord(t, rec)...), '\n')

	if got := mustReadFile(t, logPath+".1"); !bytes.Equal(got, oldLog) {
		t.Fatalf("rpc.jsonl.1 is not the previous log:\n%s", got)
	}
	if got := mustReadFile(t, logPath+".2"); !bytes.Equal(got, prev1) {
		t.Fatalf("rpc.jsonl.2 is not the generation before:\n%s", got)
	}
	if got := string(mustReadFile(t, logPath+".3")); !strings.Contains(got, "older-generation") {
		t.Fatalf("rpc.jsonl.3: %q", got)
	}
	requireOpenLogIs(t, s, logPath)
	requireLogBytesMatchFile(t, s, logPath)
	requireActivity(t, s, want)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	requireRestart(t, dir, want)
}

// failDirSync makes fsync of a directory fail while *fail is set and counts
// the attempts; file fsyncs are untouched.
func failDirSync(t *testing.T, fail *bool, calls *int) {
	t.Helper()
	swap(t, &syncFile, func(f *os.File) error {
		if fi, err := f.Stat(); err == nil && fi.IsDir() {
			*calls++
			if *fail {
				return syscall.EIO
			}
		}
		return f.Sync()
	})
}

// A directory fsync failing after publication is not a failed rotation: the
// snapshot is the active log and its handle is adopted, the error says so
// (ErrDirSync), and the sync alone is retried by later appends below the size
// limit until it succeeds.
func TestDirSyncFailureAfterPublicationIsRetriedAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directories are not fsynced on Windows")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, rpcLogFile)
	s, want := seedRotationState(t, dir)
	oldLog := mustReadFile(t, logPath)

	fail, calls := true, 0
	failDirSync(t, &fail, &calls)
	setMaxLogBytes(s, 1)
	at := want["cc33"].Add(20 * time.Minute)
	rec := gotoRecord("cc33", at)
	err := s.AppendRPC(rec)
	if !errors.Is(err, ErrDirSync) || errors.Is(err, ErrRotation) || !errors.Is(err, syscall.EIO) || !RecordWritten(err) {
		t.Fatalf("append whose rotation could not sync the dir: got %v, want ErrDirSync(EIO)", err)
	}
	want["cc33"] = at
	if calls != 1 {
		t.Fatalf("dir sync attempts: %d, want 1", calls)
	}
	oldLog = append(append(oldLog, mustMarshalRecord(t, rec)...), '\n')

	// Published and adopted.
	requireOpenLogIs(t, s, logPath)
	requireLogBytesMatchFile(t, s, logPath)
	requireAbsent(t, logPath+rotateTmpSuffix)
	if got := mustReadFile(t, logPath+".1"); !bytes.Equal(got, oldLog) {
		t.Fatalf("rpc.jsonl.1 is not the previous log:\n%s", got)
	}
	snap, invalid := readLogLines(t, logPath)
	if len(snap) != len(want) || invalid != 0 {
		t.Fatalf("snapshot: %d valid / %d invalid lines, want %d / 0", len(snap), invalid, len(want))
	}

	// Below the limit: no rotation, but the owed sync is retried and the
	// record lands in the published log.
	setMaxLogBytes(s, defaultMaxLogBytes)
	at = at.Add(time.Minute)
	err = appendManaged(s, "dd44", at)
	if !errors.Is(err, ErrDirSync) || !RecordWritten(err) {
		t.Fatalf("append below the limit with the sync still failing: got %v, want ErrDirSync", err)
	}
	want["dd44"] = at
	if calls != 2 {
		t.Fatalf("dir sync attempts: %d, want 2", calls)
	}
	if lines, invalid := readLogLines(t, logPath); len(lines) != len(snap)+1 || invalid != 0 {
		t.Fatalf("published log: %d valid / %d invalid lines, want %d / 0", len(lines), invalid, len(snap)+1)
	}
	requireOpenLogIs(t, s, logPath)
	if got := mustReadFile(t, logPath+".1"); !bytes.Equal(got, oldLog) {
		t.Fatal("rotation repeated while only the dir sync was owed")
	}
	if got := string(mustReadFile(t, logPath+".2")); !strings.Contains(got, "older-generation") {
		t.Fatalf("rpc.jsonl.2: %q", got)
	}
	requireAbsent(t, logPath+".3")
	requireRestart(t, dir, want)

	// Fault gone: one more sync settles it, later appends do not sync.
	fail = false
	at = at.Add(time.Minute)
	if err := s.AppendRPC(gotoRecord("aa11", at)); err != nil {
		t.Fatalf("append after the fault cleared: %v", err)
	}
	want["aa11"] = at
	if calls != 3 {
		t.Fatalf("dir sync attempts: %d, want 3", calls)
	}
	at = at.Add(time.Minute)
	if err := s.AppendRPC(gotoRecord("aa11", at)); err != nil {
		t.Fatal(err)
	}
	want["aa11"] = at
	if calls != 3 {
		t.Fatalf("plain append synced the dir (%d attempts)", calls)
	}
	requireLogBytesMatchFile(t, s, logPath)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("Close synced the dir with nothing owed (%d attempts)", calls)
	}
	requireRestart(t, dir, want)
}

// A sync still owed at Close is tried once more and reported.
func TestCloseReportsPendingDirSync(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directories are not fsynced on Windows")
	}
	dir := t.TempDir()
	s, want := seedRotationState(t, dir)
	fail, calls := true, 0
	failDirSync(t, &fail, &calls)
	setMaxLogBytes(s, 1)
	at := want["cc33"].Add(time.Minute)
	if err := s.AppendRPC(gotoRecord("cc33", at)); !errors.Is(err, ErrDirSync) {
		t.Fatalf("got %v, want ErrDirSync", err)
	}
	want["cc33"] = at
	if err := s.Close(); !errors.Is(err, ErrDirSync) || calls != 2 {
		t.Fatalf("Close: %v after %d sync attempts, want ErrDirSync after 2", err, calls)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	requireRestart(t, dir, want)
}

// rpc.jsonl missing and a rpc.jsonl.1 that exists but is not a regular file:
// it may be the only recoverable generation (an older daemon moved a symlinked
// log there), so startup fails instead of beginning an empty log.
func TestStartupRejectsUnsupportedPreviousGeneration(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}
	cases := []struct {
		name string
		make func(t *testing.T, dir, prev string)
	}{
		{"symlink to a log with managed records", func(t *testing.T, dir, prev string) {
			real := filepath.Join(dir, "elsewhere.jsonl")
			line := append(mustMarshalRecord(t, LogRecord{Action: ActionManagedTabOpen, Body: lifecycleBody, Target: managedTarget("aa11"), OK: true, Time: time.Now().UTC()}), '\n')
			if err := os.WriteFile(real, line, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, prev); err != nil {
				t.Fatal(err)
			}
		}},
		{"dangling symlink", func(t *testing.T, dir, prev string) {
			if err := os.Symlink(filepath.Join(dir, "gone.jsonl"), prev); err != nil {
				t.Fatal(err)
			}
		}},
		{"directory", func(t *testing.T, dir, prev string) {
			if err := os.Mkdir(prev, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			logPath := filepath.Join(dir, rpcLogFile)
			prev := logPath + ".1"
			tc.make(t, dir, prev)
			before, err := os.Lstat(prev)
			if err != nil {
				t.Fatal(err)
			}

			s, err := Open(OpenConfig{StateDir: dir})
			if err == nil {
				_ = s.Close()
				t.Fatalf("Open succeeded over an unusable rpc.jsonl.1 (managed %v)", s.ReplayManagedTabActivity())
			}
			if !strings.Contains(err.Error(), "not a regular file") || !strings.Contains(err.Error(), rpcLogFile+".1") {
				t.Fatalf("error does not name the problem: %v", err)
			}
			requireAbsent(t, logPath)
			after, err := os.Lstat(prev)
			if err != nil || after.Mode() != before.Mode() {
				t.Fatalf("rpc.jsonl.1 changed: %v (%v)", after, err)
			}

			// With the active log present the same entry is not needed for
			// recovery and does not block startup.
			if err := os.WriteFile(logPath, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			requireRestart(t, dir, map[string]time.Time{})
		})
	}
}

// Ordinary absence of both files is a first start even when unrelated files
// are present; an older backup alone is not used to invent a current log.
func TestStartupWithoutPreviousGenerationStartsEmpty(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, rpcLogFile)
	older := []byte("{\"action\":\"managed_tab_open\",\"body\":{},\"target\":\"TARGET-old\",\"ok\":true,\"time\":\"2026-01-01T00:00:00Z\"}\n")
	if err := os.WriteFile(logPath+".2", older, 0o600); err != nil {
		t.Fatal(err)
	}
	s := mustOpenDisk(t, dir)
	requireActivity(t, s, map[string]time.Time{})
	if got := mustReadFile(t, logPath); len(got) != 0 {
		t.Fatalf("first log not empty: %q", got)
	}
	if got := mustReadFile(t, logPath+".2"); !bytes.Equal(got, older) {
		t.Fatalf("rpc.jsonl.2 changed:\n%s", got)
	}
	requireAbsent(t, logPath+".1")
}

// Concurrent appends, lifecycle changes and readers while rotations are
// refused (name missing), succeed, and owe a directory sync. Run with -race.
func TestConcurrentAppendsAcrossRotationOutcomes(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, rpcLogFile)
	aside := filepath.Join(dir, "moved-aside")
	s := mustOpenDisk(t, dir)
	setMaxLogBytes(s, 512)

	var mu sync.Mutex
	failSync := false
	swap(t, &syncFile, func(f *os.File) error {
		mu.Lock()
		fail := failSync
		mu.Unlock()
		if fi, err := f.Stat(); fail && err == nil && fi.IsDir() {
			return syscall.EIO
		}
		return f.Sync()
	})

	t0 := time.Now().UTC().Truncate(time.Second)
	tabs := []string{"aa11", "bb22", "cc33", "dd44"}
	const rounds = 60
	var wg sync.WaitGroup
	for _, tab := range tabs {
		wg.Add(1)
		go func(tab string) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				at := t0.Add(time.Duration(i) * time.Second)
				if err := appendManaged(s, tab, at); err != nil && !RecordWritten(err) {
					t.Errorf("register %s: %v", tab, err)
				}
				if err := s.AppendRPC(gotoRecord(tab, at)); err != nil && !RecordWritten(err) {
					t.Errorf("append %s: %v", tab, err)
				}
				_ = s.ManagedTabs()
				if i%7 == 3 {
					if _, err := s.RetireManagedTarget(managedTarget(tab)); err != nil && !RecordWritten(err) {
						t.Errorf("retire %s: %v", tab, err)
					}
				}
			}
			if err := appendManaged(s, tab, t0.Add(time.Hour)); err != nil && !RecordWritten(err) {
				t.Errorf("final register %s: %v", tab, err)
			}
		}(tab)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			// The log's name goes away and comes back (same file, or the log a
			// rotation published in between is displaced by the returning one).
			if err := os.Rename(logPath, aside); err == nil {
				_ = os.Rename(aside, logPath)
			}
			mu.Lock()
			failSync = i%3 == 0
			mu.Unlock()
		}
		mu.Lock()
		failSync = false
		mu.Unlock()
	}()
	wg.Wait()

	want := map[string]time.Time{}
	for _, tab := range tabs {
		want[tab] = t0.Add(time.Hour)
	}
	requireActivity(t, s, want)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}
