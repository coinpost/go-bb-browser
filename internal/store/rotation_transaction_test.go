//go:build unix

package store

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The name rpc.jsonl.1 is what startup falls back to when rpc.jsonl is gone.
// These tests cover the rotation steps around it: no failure may leave that
// name free or naming less than a complete generation, the active name is
// checked again before it is published over, and errors of independent steps
// (record write, owed directory sync, rotation) are reported independently.

// requireNoRotationLeftovers checks that no private rotation file survived.
func requireNoRotationLeftovers(t *testing.T, logPath string) {
	t.Helper()
	requireAbsent(t, logPath+rotateTmpSuffix)
	requireAbsent(t, logPath+rotateKeepSuffix)
}

// Review P1 (round 2): first rotation succeeds, the second cannot hard link
// (ENOSPC), then the active log disappears. The generation in rpc.jsonl.1 must
// still be there for the restart: nothing may move before the link succeeded.
func TestRotationLinkENOSPCThenActiveRemovedRestartsFromPreviousGeneration(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, rpcLogFile)
	s, want := seedRotationState(t, dir)
	rotateOnce(t, s, want)
	prevGeneration := mustReadFile(t, logPath+".1")
	recoverable := copyActivity(want) // what rpc.jsonl.1 replays to

	links := 0
	swap(t, &linkPath, func(from, to string) error { links++; return syscall.ENOSPC })
	setMaxLogBytes(s, 1)
	at := want["cc33"].Add(time.Minute)
	err := s.AppendRPC(gotoRecord("cc33", at))
	if !errors.Is(err, ErrRotation) || !errors.Is(err, syscall.ENOSPC) || !RecordWritten(err) {
		t.Fatalf("append whose rotation could not link: got %v, want ErrRotation(ENOSPC), record written", err)
	}
	want["cc33"] = at
	if links != 1 {
		t.Fatalf("link attempts: %d, want 1", links)
	}
	requireBackups := func(when string) {
		t.Helper()
		if got := mustReadFile(t, logPath+".1"); !bytes.Equal(got, prevGeneration) {
			t.Fatalf("%s: rpc.jsonl.1 is no longer the previous generation:\n%s", when, got)
		}
		if got := string(mustReadFile(t, logPath+".2")); !strings.Contains(got, "older-generation") {
			t.Fatalf("%s: rpc.jsonl.2: %q", when, got)
		}
		requireAbsent(t, logPath+".3")
		requireNoRotationLeftovers(t, logPath)
	}
	requireBackups("after the failed link")
	requireOpenLogIs(t, s, logPath)
	// With the name still there a restart sees everything.
	requireRestart(t, dir, want)

	// The active name goes away; rotation is now refused before the link.
	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	at = at.Add(time.Minute)
	if err := s.AppendRPC(gotoRecord("aa11", at)); !errors.Is(err, ErrRotation) || !RecordWritten(err) {
		t.Fatalf("append with the log unlinked: got %v, want ErrRotation", err)
	}
	if links != 1 {
		t.Fatalf("rotation went on to link with the active name missing (%d attempts)", links)
	}
	requireBackups("after the refused rotation")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Restart: the previous generation is recovered, not an empty log.
	linkPath = os.Link
	requireRestart(t, dir, recoverable)
	if got := mustReadFile(t, logPath+".1"); !bytes.Equal(got, prevGeneration) {
		t.Fatal("restart changed rpc.jsonl.1")
	}
}

// The active name is removed or replaced while a rotation is under way: right
// after the snapshot was fsynced (before the previous generation is kept), or
// after rpc.jsonl.1 was published (before the snapshot is). Either way the
// rotation is refused, a foreign file is neither published over nor adopted,
// and a restart finds a complete generation.
func TestRotationActivePathLostMidRotation(t *testing.T) {
	const foreign = "{\"action\":\"foreign-file\"}\n"
	type disturbance struct {
		name    string
		disturb func(t *testing.T, logPath string)
	}
	disturbances := []disturbance{
		{"removed", func(t *testing.T, logPath string) {
			if err := os.Remove(logPath); err != nil {
				t.Fatal(err)
			}
		}},
		{"replaced", func(t *testing.T, logPath string) {
			if err := os.Rename(logPath, logPath+".moved"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(logPath, []byte(foreign), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, d := range disturbances {
		for _, point := range []string{"after snapshot sync", "after previous generation published"} {
			t.Run(d.name+" "+point, func(t *testing.T) {
				dir := t.TempDir()
				logPath := filepath.Join(dir, rpcLogFile)
				s, want := seedRotationState(t, dir)
				rotateOnce(t, s, want)
				prevGeneration := mustReadFile(t, logPath+".1")
				recoverable := copyActivity(want)

				done := false
				if point == "after snapshot sync" {
					swap(t, &syncFile, func(f *os.File) error {
						err := f.Sync()
						if !done && strings.HasSuffix(f.Name(), rotateTmpSuffix) {
							done = true
							d.disturb(t, logPath)
						}
						return err
					})
				} else {
					swap(t, &renamePath, func(from, to string) error {
						err := os.Rename(from, to)
						if !done && strings.HasSuffix(from, rotateKeepSuffix) {
							done = true
							d.disturb(t, logPath)
						}
						return err
					})
				}

				setMaxLogBytes(s, 1)
				at := want["cc33"].Add(time.Minute)
				err := s.AppendRPC(gotoRecord("cc33", at))
				if !done {
					t.Fatal("rotation did not reach the injection point")
				}
				if !errors.Is(err, ErrRotation) || errors.Is(err, ErrDirSync) || !RecordWritten(err) {
					t.Fatalf("append: got %v, want ErrRotation", err)
				}
				want["cc33"] = at
				requireNoRotationLeftovers(t, logPath)

				// The old handle is still the log and still written to.
				at = at.Add(time.Minute)
				if err := appendManaged(s, "dd44", at); !errors.Is(err, ErrRotation) || !RecordWritten(err) {
					t.Fatalf("append after the refusal: got %v, want ErrRotation", err)
				}
				want["dd44"] = at
				requireActivity(t, s, want)
				requireNoRotationLeftovers(t, logPath)
				if d.name == "replaced" {
					if got := string(mustReadFile(t, logPath)); got != foreign {
						t.Fatalf("foreign file under the log name was written to or replaced: %q", got)
					}
				} else {
					requireAbsent(t, logPath)
				}

				if point == "after snapshot sync" {
					// Nothing had moved: rpc.jsonl.1 is the generation before.
					if got := mustReadFile(t, logPath+".1"); !bytes.Equal(got, prevGeneration) {
						t.Fatalf("rpc.jsonl.1 changed:\n%s", got)
					}
					if got := string(mustReadFile(t, logPath+".2")); !strings.Contains(got, "older-generation") {
						t.Fatalf("rpc.jsonl.2: %q", got)
					}
					requireAbsent(t, logPath+".3")
				} else {
					// rpc.jsonl.1 already is the open log, the generation
					// before it moved to .2, the oldest to .3.
					requireOpenLogIs(t, s, logPath+".1")
					if got := mustReadFile(t, logPath+".2"); !bytes.Equal(got, prevGeneration) {
						t.Fatalf("rpc.jsonl.2 is not the previous generation:\n%s", got)
					}
					if got := string(mustReadFile(t, logPath+".3")); !strings.Contains(got, "older-generation") {
						t.Fatalf("rpc.jsonl.3: %q", got)
					}
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}

				if d.name == "replaced" {
					// Not the store's file: take it away, as an operator would.
					if err := os.Remove(logPath); err != nil {
						t.Fatal(err)
					}
				}
				if point == "after snapshot sync" {
					requireRestart(t, dir, recoverable)
				} else {
					requireRestart(t, dir, want)
				}
			})
		}
	}
}

// All three backup slots in use and every step of keeping the previous
// generation failing three times in a row: at each point rpc.jsonl.1 is a
// complete generation, a restart recovers everything, and once the fault is
// gone exactly one generation (the oldest) has been dropped.
func TestRotationRetriesWithFullBackupsDropOneGeneration(t *testing.T) {
	steps := []struct {
		name   string
		inject func(t *testing.T)
	}{
		{"stage link", func(t *testing.T) {
			swap(t, &linkPath, func(from, to string) error {
				if strings.HasSuffix(to, rotateKeepSuffix) {
					return syscall.ENOSPC
				}
				return os.Link(from, to)
			})
		}},
		{"shift older", func(t *testing.T) {
			swap(t, &renamePath, func(from, to string) error {
				if strings.HasSuffix(from, ".2") {
					return syscall.EIO
				}
				return os.Rename(from, to)
			})
		}},
		{"keep older generation", func(t *testing.T) {
			swap(t, &linkPath, func(from, to string) error {
				if strings.HasSuffix(to, ".2") {
					return syscall.ENOSPC
				}
				return os.Link(from, to)
			})
		}},
		{"publish previous", func(t *testing.T) {
			swap(t, &renamePath, func(from, to string) error {
				if strings.HasSuffix(from, rotateKeepSuffix) {
					return syscall.EIO
				}
				return os.Rename(from, to)
			})
		}},
		{"publish", func(t *testing.T) {
			swap(t, &renamePath, func(from, to string) error {
				if strings.HasSuffix(from, rotateTmpSuffix) {
					return syscall.EIO
				}
				return os.Rename(from, to)
			})
		}},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			dir := t.TempDir()
			logPath := filepath.Join(dir, rpcLogFile)
			s, want := seedRotationState(t, dir)
			gen := func(k int) string { return fmt.Sprintf("{\"action\":\"generation-%d\"}\n", k) }
			for k := 1; k <= maxLogBackups; k++ {
				if err := os.WriteFile(backupPath(dir, k), []byte(gen(k)), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			origLink, origRename := linkPath, renamePath
			step.inject(t)
			setMaxLogBytes(s, 1)
			at := want["cc33"]
			for attempt := 1; attempt <= 3; attempt++ {
				at = at.Add(time.Minute)
				if err := s.AppendRPC(gotoRecord("cc33", at)); !errors.Is(err, ErrRotation) || !RecordWritten(err) {
					t.Fatalf("attempt %d: got %v, want ErrRotation", attempt, err)
				}
				want["cc33"] = at
				requireNoRotationLeftovers(t, logPath)
				requireOpenLogIs(t, s, logPath)

				// rpc.jsonl.1: generation 1, or already the open log.
				first := string(mustReadFile(t, backupPath(dir, 1)))
				if first != gen(1) && first != string(mustReadFile(t, logPath)) {
					t.Fatalf("attempt %d: rpc.jsonl.1 is neither generation 1 nor the open log: %q", attempt, first)
				}
				// Generation 1 never loses its last name, generation 2 is
				// never pushed out by a retry.
				found := map[string]bool{}
				for k := 1; k <= maxLogBackups; k++ {
					if data, err := os.ReadFile(backupPath(dir, k)); err == nil {
						found[string(data)] = true
					}
				}
				if !found[gen(1)] {
					t.Fatalf("attempt %d: generation 1 is gone", attempt)
				}
				if !found[gen(2)] {
					t.Fatalf("attempt %d: a retry pushed generation 2 out", attempt)
				}
				requireRestart(t, dir, want)
			}

			linkPath, renamePath = origLink, origRename
			oldLog := mustReadFile(t, logPath)
			at = at.Add(time.Minute)
			rec := gotoRecord("cc33", at)
			if err := s.AppendRPC(rec); err != nil {
				t.Fatalf("append after the fault cleared: %v", err)
			}
			want["cc33"] = at
			oldLog = append(append(oldLog, mustMarshalRecord(t, rec)...), '\n')
			if got := mustReadFile(t, backupPath(dir, 1)); !bytes.Equal(got, oldLog) {
				t.Fatalf("rpc.jsonl.1 is not the previous log:\n%s", got)
			}
			if got := string(mustReadFile(t, backupPath(dir, 2))); got != gen(1) {
				t.Fatalf("rpc.jsonl.2: %q, want generation 1", got)
			}
			if got := string(mustReadFile(t, backupPath(dir, 3))); got != gen(2) {
				t.Fatalf("rpc.jsonl.3: %q, want generation 2", got)
			}
			requireNoRotationLeftovers(t, logPath)
			requireOpenLogIs(t, s, logPath)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			requireRestart(t, dir, want)
		})
	}
}

// A staging link left by a process that stopped mid-rotation is only another
// name of a log: startup removes it and replays as usual.
func TestRestartRemovesStagingLink(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, rpcLogFile)
	s, want := seedRotationState(t, dir)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(logPath, logPath+rotateKeepSuffix); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	before := mustReadFile(t, logPath)
	requireRestart(t, dir, want)
	requireAbsent(t, logPath+rotateKeepSuffix)
	if got := mustReadFile(t, logPath); !bytes.Equal(got, before) {
		t.Fatal("removing the staging link changed the log")
	}
}

// owedDirSync returns a store whose published rotation still owes its
// directory sync, with *fail controlling further attempts.
func owedDirSync(t *testing.T, dir string, fail *bool, calls *int) (*Store, map[string]time.Time) {
	t.Helper()
	s, want := seedRotationState(t, dir)
	failDirSync(t, fail, calls)
	setMaxLogBytes(s, 1)
	at := want["cc33"].Add(time.Minute)
	err := s.AppendRPC(gotoRecord("cc33", at))
	if !errors.Is(err, ErrDirSync) || errors.Is(err, ErrRotation) || !RecordWritten(err) {
		t.Fatalf("setup: got %v, want ErrDirSync", err)
	}
	want["cc33"] = at
	setMaxLogBytes(s, defaultMaxLogBytes)
	return s, want
}

// An owed directory sync that fails again must stay visible when the rotation
// attempted by the same append fails too: both errors are returned, and the
// record — which was written — is reported as written.
func TestOwedDirSyncNotMaskedByFailedRotation(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, rpcLogFile)
	fail, calls := true, 0
	s, want := owedDirSync(t, dir, &fail, &calls)

	swap(t, &openFile, func(name string, flag int, perm os.FileMode) (*os.File, error) {
		if strings.HasSuffix(name, rotateTmpSuffix) {
			return nil, syscall.ENOSPC
		}
		return os.OpenFile(name, flag, perm)
	})
	setMaxLogBytes(s, 1)
	before := calls
	at := want["cc33"].Add(time.Minute)
	err := s.AppendRPC(gotoRecord("cc33", at))
	for _, target := range []error{ErrRotation, syscall.ENOSPC, ErrDirSync, syscall.EIO} {
		if !errors.Is(err, target) {
			t.Fatalf("append: %v is missing %v", err, target)
		}
	}
	if !RecordWritten(err) {
		t.Fatalf("record was written, RecordWritten(%v) = false", err)
	}
	want["cc33"] = at
	if calls != before+1 {
		t.Fatalf("dir sync attempts: %d, want %d", calls, before+1)
	}
	requireOpenLogIs(t, s, logPath)
	requireLogBytesMatchFile(t, s, logPath)
	requireRestart(t, dir, want)

	// The sync recovers while the rotation still fails: only the rotation is
	// reported, and nothing is owed any more.
	fail = false
	at = at.Add(time.Minute)
	err = s.AppendRPC(gotoRecord("cc33", at))
	if !errors.Is(err, ErrRotation) || errors.Is(err, ErrDirSync) || !RecordWritten(err) {
		t.Fatalf("append with the sync recovered: got %v, want ErrRotation only", err)
	}
	want["cc33"] = at
	before = calls
	at = at.Add(time.Minute)
	if err := s.AppendRPC(gotoRecord("cc33", at)); !errors.Is(err, ErrRotation) {
		t.Fatalf("got %v, want ErrRotation", err)
	}
	want["cc33"] = at
	if calls != before {
		t.Fatalf("dir synced with nothing owed (%d attempts)", calls-before)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close with nothing owed: %v", err)
	}
	requireRestart(t, dir, want)
}

// A sync still failing when the next rotation publishes is superseded by that
// rotation's own sync: its outcome is the answer.
func TestOwedDirSyncSettledByNextPublishedRotation(t *testing.T) {
	dir := t.TempDir()
	fail, calls := true, 0
	s, want := owedDirSync(t, dir, &fail, &calls)

	// Fails for the retry, succeeds for the rotation's own sync.
	n := 0
	swap(t, &syncFile, func(f *os.File) error {
		if fi, err := f.Stat(); err == nil && fi.IsDir() {
			n++
			if n == 1 {
				return syscall.EIO
			}
		}
		return f.Sync()
	})
	setMaxLogBytes(s, 1)
	at := want["cc33"].Add(time.Minute)
	if err := s.AppendRPC(gotoRecord("cc33", at)); err != nil {
		t.Fatalf("append whose rotation published and synced: %v", err)
	}
	want["cc33"] = at
	if n != 2 {
		t.Fatalf("dir sync attempts: %d, want 2", n)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("Close synced with nothing owed (%d attempts)", n)
	}
	requireRestart(t, dir, want)
}

// RecordWritten is about this call's record only. A record that was not
// written is not reported as written because an earlier rotation's sync is
// still owed — and the owed sync is still retried and reported.
func TestRecordWrittenReflectsOnlyThisRecord(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, rpcLogFile)
	fail, calls := true, 0
	s, want := owedDirSync(t, dir, &fail, &calls)
	linesBefore, _ := readLogLines(t, logPath)

	// Write fails outright, sync still owed and failing.
	setWriteLog(s, func(f *os.File, p []byte) (int, error) { return 0, syscall.ENOSPC })
	before := calls
	err := s.AppendRPC(gotoRecord("aa11", want["aa11"].Add(time.Hour)))
	if err == nil || RecordWritten(err) {
		t.Fatalf("failed write reported as written: %v", err)
	}
	if !errors.Is(err, syscall.ENOSPC) || !errors.Is(err, ErrDirSync) || !errors.Is(err, syscall.EIO) || errors.Is(err, ErrRotation) {
		t.Fatalf("got %v, want the write error (ENOSPC) and ErrDirSync(EIO)", err)
	}
	if calls != before+1 {
		t.Fatalf("owed sync skipped because the write failed (%d attempts)", calls-before)
	}

	// Short write: part of the record is in the log, the record is not.
	setWriteLog(s, shortWriteOnce(nil))
	err = s.AppendRPC(gotoRecord("aa11", want["aa11"].Add(2*time.Hour)))
	if err == nil || RecordWritten(err) || !errors.Is(err, ErrDirSync) {
		t.Fatalf("short write: got %v, want not written + ErrDirSync", err)
	}

	// Write fails, nothing owed any more: just the write error.
	fail = false
	setWriteLog(s, nil)
	at := want["cc33"].Add(time.Minute)
	if err := s.AppendRPC(gotoRecord("cc33", at)); err != nil {
		t.Fatalf("append settling the sync: %v", err)
	}
	want["cc33"] = at
	setWriteLog(s, func(f *os.File, p []byte) (int, error) { return 0, syscall.ENOSPC })
	before = calls
	err = s.AppendRPC(gotoRecord("aa11", want["aa11"].Add(3*time.Hour)))
	if !errors.Is(err, syscall.ENOSPC) || RecordWritten(err) || errors.Is(err, ErrDirSync) || errors.Is(err, ErrRotation) {
		t.Fatalf("got %v, want the bare write error", err)
	}
	if calls != before {
		t.Fatal("dir synced with nothing owed")
	}
	// A failed write does not start a rotation either.
	setMaxLogBytes(s, 1)
	if err := s.AppendRPC(gotoRecord("aa11", want["aa11"].Add(4*time.Hour))); RecordWritten(err) || errors.Is(err, ErrRotation) {
		t.Fatalf("got %v, want the bare write error", err)
	}
	setMaxLogBytes(s, defaultMaxLogBytes)

	// Positive cases next to the negative ones.
	if RecordWritten(nil) || RecordWritten(ErrRotation) || RecordWritten(ErrDirSync) || RecordWritten(ErrClosed) {
		t.Fatal("RecordWritten is true for an error no append returned")
	}
	setWriteLog(s, nil)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendRPC(gotoRecord("aa11", time.Now())); !errors.Is(err, ErrClosed) || RecordWritten(err) {
		t.Fatalf("append after Close: %v", err)
	}
	lines, _ := readLogLines(t, logPath)
	if len(lines) != len(linesBefore)+1 {
		t.Fatalf("log gained %d valid records, want 1", len(lines)-len(linesBefore))
	}
	requireRestart(t, dir, want)
}

// Close retries an owed sync and reports it; that error names no record.
func TestCloseErrorIsNotARecordWritten(t *testing.T) {
	dir := t.TempDir()
	fail, calls := true, 0
	s, want := owedDirSync(t, dir, &fail, &calls)
	err := s.Close()
	if !errors.Is(err, ErrDirSync) || !errors.Is(err, syscall.EIO) || RecordWritten(err) {
		t.Fatalf("Close: %v", err)
	}
	requireRestart(t, dir, want)
}

// rpc.jsonl that exists but is not a regular file — a symbolic link to a log,
// a dangling one, a directory — is refused at startup exactly as rotation
// refuses it. Opening it would follow the link (creating a dangling link's
// destination and starting an empty log next to a complete rpc.jsonl.1).
func TestStartupRejectsNonRegularActiveLog(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, dir, logPath, elsewhere string)
	}{
		{"dangling symlink", func(t *testing.T, dir, logPath, elsewhere string) {
			if err := os.Symlink(elsewhere, logPath); err != nil {
				t.Fatal(err)
			}
		}},
		{"relative dangling symlink", func(t *testing.T, dir, logPath, elsewhere string) {
			if err := os.Symlink("missing-target", logPath); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink to a log", func(t *testing.T, dir, logPath, elsewhere string) {
			writeFileMode(t, elsewhere, managedOpenLine(t, "ee55"), 0o644)
			if err := os.Symlink(elsewhere, logPath); err != nil {
				t.Fatal(err)
			}
		}},
		{"directory", func(t *testing.T, dir, logPath, elsewhere string) {
			if err := os.Mkdir(logPath, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, withPrev := range []bool{true, false} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("%s/previous=%v", tc.name, withPrev), func(t *testing.T) {
				dir := filepath.Join(t.TempDir(), "state")
				mkdirMode(t, dir, 0o700)
				logPath := filepath.Join(dir, rpcLogFile)
				elsewhere := filepath.Join(t.TempDir(), "target.jsonl")
				prev := append(mustMarshalRecord(t, LogRecord{Action: ActionManagedTabOpen, Body: lifecycleBody, Target: managedTarget("aa11"), OK: true, Time: time.Now().UTC()}), '\n')
				if withPrev {
					writeFileMode(t, backupPath(dir, 1), prev, 0o600)
				}
				tc.setup(t, dir, logPath, elsewhere)
				before, err := os.Lstat(logPath)
				if err != nil {
					t.Fatal(err)
				}
				var targetBefore []byte
				var targetMode os.FileMode
				hadTarget := false
				if fi, err := os.Stat(elsewhere); err == nil {
					hadTarget, targetMode = true, fi.Mode()
					targetBefore = mustReadFile(t, elsewhere)
				}

				for attempt := 1; attempt <= 2; attempt++ {
					s, err := Open(OpenConfig{StateDir: dir})
					if err == nil {
						_ = s.Close()
						t.Fatalf("attempt %d: Open accepted a non-regular rpc.jsonl", attempt)
					}
					if !strings.Contains(err.Error(), "not a regular file") {
						t.Fatalf("attempt %d: unexpected error: %v", attempt, err)
					}
					after, err := os.Lstat(logPath)
					if err != nil || after.Mode() != before.Mode() || !os.SameFile(before, after) {
						t.Fatalf("attempt %d: rpc.jsonl entry changed (%v)", attempt, err)
					}
					if hadTarget {
						fi, err := os.Stat(elsewhere)
						if err != nil || fi.Mode() != targetMode || !bytes.Equal(mustReadFile(t, elsewhere), targetBefore) {
							t.Fatalf("attempt %d: link destination was touched (%v)", attempt, err)
						}
					} else {
						requireAbsent(t, elsewhere)
					}
					requireAbsent(t, filepath.Join(dir, "missing-target"))
					if withPrev {
						if got := mustReadFile(t, backupPath(dir, 1)); !bytes.Equal(got, prev) {
							t.Fatalf("attempt %d: rpc.jsonl.1 changed: %q", attempt, got)
						}
					} else {
						requireAbsent(t, backupPath(dir, 1))
					}
					requireNoRotationLeftovers(t, logPath)
				}

				// Moving the entry away lets startup proceed: from rpc.jsonl.1
				// when there is one, else as a first start.
				if err := os.Rename(logPath, filepath.Join(dir, "set-aside")); err != nil {
					t.Fatal(err)
				}
				s, err := Open(OpenConfig{StateDir: dir})
				if err != nil {
					t.Fatalf("Open after moving the entry away: %v", err)
				}
				defer s.Close()
				if withPrev {
					requireManaged(t, s, "aa11")
				} else {
					requireManaged(t, s)
				}
			})
		}
	}
}

// A symbolic link put under the active name while the store runs is refused by
// rotation the same way (never linked into the backups, never published over).
func TestRotationRefusesSymlinkedActiveLog(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, rpcLogFile)
	s, want := seedRotationState(t, dir)
	real := filepath.Join(dir, "real.jsonl")
	if err := os.Rename(logPath, real); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, logPath); err != nil {
		t.Fatal(err)
	}
	setMaxLogBytes(s, 1)
	at := want["cc33"].Add(time.Minute)
	if err := s.AppendRPC(gotoRecord("cc33", at)); !errors.Is(err, ErrRotation) || !RecordWritten(err) {
		t.Fatalf("got %v, want ErrRotation", err)
	}
	want["cc33"] = at
	if fi, err := os.Lstat(logPath); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink under the log name was replaced (%v)", err)
	}
	if got := string(mustReadFile(t, logPath+".1")); !strings.Contains(got, "older-generation") {
		t.Fatalf("rpc.jsonl.1: %q", got)
	}
	requireAbsent(t, logPath+".2")
	requireNoRotationLeftovers(t, logPath)

	// The regular file back under its name: the next append rotates.
	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(real, logPath); err != nil {
		t.Fatal(err)
	}
	at = at.Add(time.Minute)
	if err := s.AppendRPC(gotoRecord("cc33", at)); err != nil {
		t.Fatalf("append with the log back: %v", err)
	}
	want["cc33"] = at
	requireOpenLogIs(t, s, logPath)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	requireRestart(t, dir, want)
}

// A startup that is refused changes nothing in the state dir: whether the dir
// can be started from is decided before the staging files of an interrupted
// rotation are removed and before backup modes are tightened. Once the entry
// that made startup refuse is moved away, the usual cleanup happens.
func TestRefusedStartupLeavesStateDirAsFound(t *testing.T) {
	cases := []struct {
		name    string
		blocker func(logPath string) string // the entry startup refuses
		wantErr string
	}{
		{"rpc.jsonl is a directory", func(logPath string) string { return logPath }, "not a regular file"},
		{"rpc.jsonl missing and rpc.jsonl.1 is a directory", func(logPath string) string { return logPath + ".1" }, "previous generation"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "state")
			mkdirMode(t, dir, 0o700)
			logPath := filepath.Join(dir, rpcLogFile)
			blocker := tc.blocker(logPath)
			if err := os.Mkdir(blocker, 0o700); err != nil {
				t.Fatal(err)
			}
			staged := map[string][]byte{
				logPath + rotateTmpSuffix:  managedOpenLine(t, "aa11"),
				logPath + rotateKeepSuffix: managedOpenLine(t, "bb22"),
			}
			for path, data := range staged {
				writeFileMode(t, path, data, 0o600)
			}
			older := managedOpenLine(t, "cc33")
			writeFileMode(t, backupPath(dir, 2), older, 0o644)

			for attempt := 1; attempt <= 2; attempt++ {
				s, err := Open(OpenConfig{StateDir: dir})
				if err == nil {
					_ = s.Close()
					t.Fatalf("attempt %d: Open succeeded", attempt)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("attempt %d: unexpected error: %v", attempt, err)
				}
				for path, data := range staged {
					if got := mustReadFile(t, path); !bytes.Equal(got, data) {
						t.Fatalf("attempt %d: %s changed: %q", attempt, filepath.Base(path), got)
					}
				}
				fi, err := os.Stat(backupPath(dir, 2))
				if err != nil || fi.Mode().Perm() != 0o644 || !bytes.Equal(mustReadFile(t, backupPath(dir, 2)), older) {
					t.Fatalf("attempt %d: rpc.jsonl.2 was touched (mode %v, err %v)", attempt, fi.Mode().Perm(), err)
				}
				if fi, err := os.Lstat(blocker); err != nil || !fi.IsDir() {
					t.Fatalf("attempt %d: refused entry changed (%v)", attempt, err)
				}
			}

			if err := os.Rename(blocker, filepath.Join(dir, "set-aside")); err != nil {
				t.Fatal(err)
			}
			s, err := Open(OpenConfig{StateDir: dir})
			if err != nil {
				t.Fatalf("Open after moving the entry away: %v", err)
			}
			defer s.Close()
			requireManaged(t, s)
			requireNoRotationLeftovers(t, logPath)
			if fi, err := os.Stat(backupPath(dir, 2)); err != nil || fi.Mode().Perm() != 0o600 {
				t.Fatalf("rpc.jsonl.2 not tightened after a successful start (%v)", err)
			}
		})
	}
}
