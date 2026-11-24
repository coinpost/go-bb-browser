package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coinpost/go-bb-browser/pkg/protocol"
)

// tabNewRecord is the lifecycle record that registers tab as managed under the
// full target id managedTarget(tab). (RPC tab_new audit lines no longer add
// managed membership, so the boundary tests drive it through lifecycle records.)
func tabNewRecord(tab string, at time.Time) LogRecord {
	return LogRecord{
		Action: ActionManagedTabOpen,
		Body:   lifecycleBody,
		Target: managedTarget(tab),
		OK:     true,
		Time:   at,
	}
}

func managedTarget(tab string) string { return "TARGET-" + tab }

// appendManaged writes the same record as tabNewRecord through the store.
func appendManaged(s *Store, tab string, at time.Time) error {
	return s.RegisterManagedTab(managedTarget(tab), at)
}

func mustMarshalRecord(t *testing.T, rec LogRecord) []byte {
	t.Helper()
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// readLogLines splits the log on physical newlines and returns the records of
// lines that parse on their own plus the number of non-blank lines that do not.
func readLogLines(t *testing.T, path string) (valid []LogRecord, invalid int) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec LogRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			invalid++
			continue
		}
		valid = append(valid, rec)
	}
	return valid, invalid
}

func mustOpenDisk(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(OpenConfig{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func requireManaged(t *testing.T, s *Store, tabs ...string) {
	t.Helper()
	managed := s.ReplayManagedTabActivity()
	for _, tab := range tabs {
		if _, ok := managed[managedTarget(tab)]; !ok {
			t.Fatalf("managed tab %q missing: %#v", tab, managed)
		}
	}
	if len(managed) != len(tabs) {
		t.Fatalf("managed tabs: got %#v want %v", managed, tabs)
	}
}

func requireLogBytesMatchFile(t *testing.T, s *Store, path string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	got := s.logBytes
	s.mu.Unlock()
	if got != fi.Size() {
		t.Fatalf("logBytes %d != file size %d", got, fi.Size())
	}
}

func setWriteLog(s *Store, fn func(f *os.File, p []byte) (int, error)) {
	s.mu.Lock()
	s.writeLog = fn
	s.mu.Unlock()
}

// shortWriteOnce really writes only the first half of the next payload and
// reports err; later writes pass through.
func shortWriteOnce(err error) func(f *os.File, p []byte) (int, error) {
	done := false
	return func(f *os.File, p []byte) (int, error) {
		if done {
			return f.Write(p)
		}
		done = true
		n, werr := f.Write(p[:len(p)/2])
		if werr != nil {
			return n, werr
		}
		return n, err
	}
}

// A complete JSON record whose trailing newline never reached disk must be
// replayed, and later appends must start on a fresh line instead of gluing
// onto it (which used to make the next startup truncate everything).
func TestAppendAfterUnterminatedValidTailKeepsRecords(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, rpcLogFile)
	t0 := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)

	if err := os.WriteFile(logPath, mustMarshalRecord(t, tabNewRecord("aa11", t0)), 0o644); err != nil {
		t.Fatal(err)
	}

	// Reopening without appending must neither lose nor corrupt the tail.
	for i := 0; i < 2; i++ {
		s := mustOpenDisk(t, dir)
		requireManaged(t, s, "aa11")
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}

	s := mustOpenDisk(t, dir)
	requireManaged(t, s, "aa11")
	if err := appendManaged(s, "bb22", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	requireLogBytesMatchFile(t, s, logPath)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2 := mustOpenDisk(t, dir)
	requireManaged(t, s2, "aa11", "bb22")
	if err := appendManaged(s2, "cc33", t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}

	s3 := mustOpenDisk(t, dir)
	requireManaged(t, s3, "aa11", "bb22", "cc33")

	valid, invalid := readLogLines(t, logPath)
	if len(valid) != 3 || invalid != 0 {
		t.Fatalf("every line must parse on its own: valid=%d invalid=%d", len(valid), invalid)
	}
}

// A corrupt line in the middle only costs that line: independent valid records
// after it are replayed and stay on disk. Glued records are not guessed apart.
func TestOpenSkipsCorruptMiddleLines(t *testing.T) {
	t0 := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	first := mustMarshalRecord(t, tabNewRecord("aa11", t0))
	later := mustMarshalRecord(t, tabNewRecord("bb22", t0.Add(time.Minute)))

	cases := []struct {
		name string
		bad  string
	}{
		{name: "truncated record", bad: `{"action":"tab_new","body":`},
		{name: "glued records", bad: string(mustMarshalRecord(t, tabNewRecord("xx99", t0))) + string(mustMarshalRecord(t, tabNewRecord("yy88", t0)))},
		{name: "garbage", bad: "\x00\x00not json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			logPath := filepath.Join(dir, rpcLogFile)
			content := string(first) + "\n" + tc.bad + "\n" + string(later) + "\n"
			if err := os.WriteFile(logPath, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}

			s := mustOpenDisk(t, dir)
			requireManaged(t, s, "aa11", "bb22")
			if err := appendManaged(s, "cc33", t0.Add(2*time.Minute)); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}

			data, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(string(data), content) {
				t.Fatalf("existing log content must be preserved:\n got: %q\nwant prefix: %q", data, content)
			}
			valid, invalid := readLogLines(t, logPath)
			if len(valid) != 3 || invalid != 1 {
				t.Fatalf("lines: valid=%d invalid=%d", len(valid), invalid)
			}

			s2 := mustOpenDisk(t, dir)
			requireManaged(t, s2, "aa11", "bb22", "cc33")
		})
	}
}

// A corrupt middle line followed by an incomplete final line: the middle line
// is skipped, the final fragment is still truncated (not disguised by a newline).
func TestOpenSkipsCorruptMiddleAndTruncatesIncompleteTail(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, rpcLogFile)
	t0 := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	kept := string(mustMarshalRecord(t, tabNewRecord("aa11", t0))) + "\nnot json\n" +
		string(mustMarshalRecord(t, tabNewRecord("bb22", t0.Add(time.Minute)))) + "\n"
	if err := os.WriteFile(logPath, []byte(kept+`{"action":"tab_new","tab":"zz00","ok":true,"bo`), 0o644); err != nil {
		t.Fatal(err)
	}

	s := mustOpenDisk(t, dir)
	requireManaged(t, s, "aa11", "bb22")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != kept {
		t.Fatalf("incomplete tail must be truncated:\n got: %q\nwant: %q", data, kept)
	}
	requireLogBytesMatchFile(t, s, logPath)
}

// A short write (e.g. disk full) leaves a fragment; the error must surface and
// the next append must not glue onto the fragment.
func TestAppendAfterShortWriteDoesNotGlue(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, rpcLogFile)
	t0 := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)

	s := mustOpenDisk(t, dir)
	if err := appendManaged(s, "aa11", t0); err != nil {
		t.Fatal(err)
	}

	setWriteLog(s, shortWriteOnce(syscall.ENOSPC))
	if err := appendManaged(s, "lost", t0.Add(time.Minute)); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("short write error must propagate, got %v", err)
	}
	requireLogBytesMatchFile(t, s, logPath)

	if err := appendManaged(s, "bb22", t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := appendManaged(s, "cc33", t0.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	requireLogBytesMatchFile(t, s, logPath)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	valid, invalid := readLogLines(t, logPath)
	if len(valid) != 3 || invalid != 1 {
		t.Fatalf("lines: valid=%d invalid=%d", len(valid), invalid)
	}
	s2 := mustOpenDisk(t, dir)
	requireManaged(t, s2, "aa11", "bb22", "cc33")
}

// A writer that reports fewer bytes without an error is still a failed append.
func TestAppendShortWriteWithoutErrorIsReported(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, rpcLogFile)
	t0 := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)

	s := mustOpenDisk(t, dir)
	setWriteLog(s, shortWriteOnce(nil))
	if err := appendManaged(s, "lost", t0); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("want io.ErrShortWrite, got %v", err)
	}
	if err := appendManaged(s, "aa11", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	requireLogBytesMatchFile(t, s, logPath)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2 := mustOpenDisk(t, dir)
	requireManaged(t, s2, "aa11")
}

// While the line boundary cannot be restored, nothing may be appended after
// the fragment; once writes work again the log continues on a fresh line.
func TestAppendRefusedWhileBoundaryRepairFails(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, rpcLogFile)
	t0 := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)

	s := mustOpenDisk(t, dir)
	if err := appendManaged(s, "aa11", t0); err != nil {
		t.Fatal(err)
	}
	setWriteLog(s, shortWriteOnce(syscall.ENOSPC))
	if err := appendManaged(s, "lost", t0.Add(time.Minute)); err == nil {
		t.Fatal("short write must fail")
	}
	before, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}

	// Every write fails: the separator cannot be written, so no record may follow.
	var attempts [][]byte
	setWriteLog(s, func(f *os.File, p []byte) (int, error) {
		attempts = append(attempts, append([]byte{}, p...))
		return 0, syscall.EIO
	})
	for i := 0; i < 2; i++ {
		if err := appendManaged(s, "dd44", t0.Add(2*time.Minute)); !errors.Is(err, syscall.EIO) {
			t.Fatalf("boundary repair failure must propagate, got %v", err)
		}
	}
	for _, p := range attempts {
		if !bytes.Equal(p, []byte("\n")) {
			t.Fatalf("record write attempted before the line boundary was restored: %q", p)
		}
	}
	after, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("log changed while boundary repair failed:\n before: %q\n after: %q", before, after)
	}

	// The separator lands but the record itself fails with nothing written:
	// the boundary is clean again and the next append still must not glue.
	setWriteLog(s, func(f *os.File, p []byte) (int, error) {
		if len(p) > 1 {
			return 0, syscall.EIO
		}
		return f.Write(p)
	})
	if err := appendManaged(s, "ee55", t0.Add(3*time.Minute)); !errors.Is(err, syscall.EIO) {
		t.Fatalf("record write failure must propagate, got %v", err)
	}
	requireLogBytesMatchFile(t, s, logPath)

	setWriteLog(s, nil)
	if err := appendManaged(s, "bb22", t0.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	requireLogBytesMatchFile(t, s, logPath)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	valid, invalid := readLogLines(t, logPath)
	if len(valid) != 2 || invalid != 1 {
		t.Fatalf("lines: valid=%d invalid=%d", len(valid), invalid)
	}
	s2 := mustOpenDisk(t, dir)
	requireManaged(t, s2, "aa11", "bb22")
}

// The rotation snapshot shares the write path: a short snapshot write must not
// make the following append glue onto the snapshot fragment.
func TestAppendAfterShortSnapshotWriteDoesNotGlue(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, rpcLogFile)
	t0 := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)

	s := mustOpenDisk(t, dir)
	if err := appendManaged(s, "aa11", t0); err != nil {
		t.Fatal(err)
	}
	if err := appendManaged(s, "bb22", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	// Force rotation on the next append and cut the two-line snapshot inside
	// its second line (aa11 complete, bb22 fragment).
	s.mu.Lock()
	s.maxLogBytes = 1
	s.writeLog = func(f *os.File, p []byte) (int, error) {
		if bytes.Count(p, []byte("\n")) < 2 {
			return f.Write(p)
		}
		n, err := f.Write(p[:len(p)-10])
		if err != nil {
			return n, err
		}
		return n, syscall.ENOSPC
	}
	s.mu.Unlock()
	if err := s.AppendRPC(LogRecord{
		Action: protocol.MethodGoto,
		Body:   json.RawMessage(`{"jsonrpc":"2.0","method":"goto","params":{"tab":"aa11","url":"https://ex"},"id":2}`),
		OK:     true,
		Time:   t0.Add(2 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(logPath + ".1"); err != nil {
		t.Fatalf("expected rotation: %v", err)
	}
	requireLogBytesMatchFile(t, s, logPath)

	s.mu.Lock()
	s.maxLogBytes = defaultMaxLogBytes
	s.writeLog = nil
	s.mu.Unlock()
	if err := appendManaged(s, "cc33", t0.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	requireLogBytesMatchFile(t, s, logPath)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	valid, invalid := readLogLines(t, logPath)
	if len(valid) != 2 || invalid != 1 {
		t.Fatalf("lines: valid=%d invalid=%d", len(valid), invalid)
	}
	s2, err := Open(OpenConfig{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	managed := s2.ReplayManagedTabActivity()
	for _, tab := range []string{"aa11", "cc33"} {
		if _, ok := managed[managedTarget(tab)]; !ok {
			t.Fatalf("managed tab %q lost after short snapshot write: %#v", tab, managed)
		}
	}
}
