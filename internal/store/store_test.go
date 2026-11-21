package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yiplee/go-bb-browser/pkg/protocol"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(OpenConfig{StateDir: "-"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestNextSeqMonotonic(t *testing.T) {
	s := openTestStore(t)
	var last uint64
	for i := 0; i < 100; i++ {
		n, err := s.NextSeq()
		if err != nil {
			t.Fatal(err)
		}
		if n <= last {
			t.Fatalf("seq not increasing: got %d after %d", n, last)
		}
		last = n
	}
}

func TestNextSeqConcurrentUnique(t *testing.T) {
	s := openTestStore(t)
	const workers = 16
	const iters = 200
	var wg sync.WaitGroup
	seen := make(map[uint64]struct{})
	var mu sync.Mutex

	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				n, err := s.NextSeq()
				if err != nil {
					t.Errorf("seq: %v", err)
					return
				}
				mu.Lock()
				if _, dup := seen[n]; dup {
					mu.Unlock()
					t.Errorf("duplicate seq %d", n)
					return
				}
				seen[n] = struct{}{}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != workers*iters {
		t.Fatalf("unique seq count: got %d want %d", len(seen), workers*iters)
	}
}

func TestAppendRPCInMemory(t *testing.T) {
	s := openTestStore(t)
	at := time.Now().UTC().Truncate(time.Millisecond)
	body := json.RawMessage(`{"jsonrpc":"2.0","method":"tab_list","params":{},"id":1}`)

	if err := s.AppendRPC(LogRecord{
		Action:   protocol.MethodTabList,
		Body:     body,
		SenderIP: "127.0.0.1",
		Seq:      1,
		OK:       true,
		Time:     at,
	}); err != nil {
		t.Fatal(err)
	}

	s.mu.Lock()
	n := len(s.memLogs)
	s.mu.Unlock()
	if n != 1 {
		t.Fatalf("records: %d", n)
	}
}

func TestOpenDiskMode(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(OpenConfig{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if s.InMemory() {
		t.Fatal("expected disk mode")
	}
	seq1, err := s.NextSeq()
	if err != nil {
		t.Fatal(err)
	}
	body := json.RawMessage(`{"jsonrpc":"2.0","method":"tab_new","params":{},"id":1}`)
	if err := s.AppendRPC(LogRecord{
		Action: protocol.MethodTabNew,
		Body:   body,
		Tab:    "ab12", Target: "TARGET-ab12",
		Seq:  seq1,
		OK:   true,
		Time: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	logPath := filepath.Join(dir, rpcLogFile)
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("expected rpc.jsonl: %v", err)
	}

	s2, err := Open(OpenConfig{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	seq2, err := s2.NextSeq()
	if err != nil {
		t.Fatal(err)
	}
	if seq2 <= seq1 {
		t.Fatalf("seq not persisted: %d after %d", seq2, seq1)
	}
	managed := s2.ReplayManagedTabActivity()
	if managed["TARGET-ab12"].IsZero() {
		t.Fatalf("managed tab not restored: %#v", managed)
	}
}

func TestOpenRebuildsManagedFromFullLog(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, rpcLogFile)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	t0 := time.Now().UTC().Add(-time.Hour)
	records := []LogRecord{
		{Action: protocol.MethodTabNew, Body: json.RawMessage(`{"jsonrpc":"2.0","method":"tab_new","params":{},"id":1}`), Tab: "aa11", Target: "TARGET-aa11", OK: true, Time: t0},
		{Action: protocol.MethodGoto, Body: json.RawMessage(`{"jsonrpc":"2.0","method":"goto","params":{"tab":"aa11","url":"https://ex"},"id":2}`), Tab: "aa11", Target: "TARGET-aa11", OK: true, Time: t0.Add(time.Minute)},
	}
	var b strings.Builder
	for _, rec := range records {
		line, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(logPath, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(OpenConfig{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	managed := s.ReplayManagedTabActivity()
	want := t0.Add(time.Minute)
	if len(managed) != 1 || !managed["TARGET-aa11"].Equal(want) {
		t.Fatalf("managed: %#v", managed)
	}
	if got := s.seq.Load(); got == 0 {
		t.Fatal("seq should be seeded from wall clock, got 0")
	}
}

func TestOpenAbandonsCorruptRPCLog(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, rpcLogFile)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	t0 := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	good := LogRecord{
		Action: protocol.MethodTabNew,
		Body:   json.RawMessage(`{"jsonrpc":"2.0","method":"tab_new","params":{},"id":1}`),
		Tab:    "aa11", Target: "TARGET-aa11",
		OK:   true,
		Time: t0,
	}
	goodLine, err := json.Marshal(good)
	if err != nil {
		t.Fatal(err)
	}
	later := LogRecord{
		Action: protocol.MethodTabNew,
		Body:   json.RawMessage(`{"jsonrpc":"2.0","method":"tab_new","params":{},"id":2}`),
		Tab:    "bb22", Target: "TARGET-bb22",
		OK:   true,
		Time: t0.Add(time.Minute),
	}
	laterLine, err := json.Marshal(later)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		tail string
	}{
		{name: "corrupt line with newline", tail: "{\"action\":\"tab_new\",\"body\":\n" + string(laterLine) + "\n"},
		{name: "truncated final line without newline", tail: `{"action":"tab_new","body":`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(logPath, append(append(goodLine, '\n'), tc.tail...), 0o644); err != nil {
				t.Fatal(err)
			}

			s, err := Open(OpenConfig{StateDir: dir})
			if err != nil {
				t.Fatalf("Open should tolerate corrupt rpc log: %v", err)
			}
			defer s.Close()

			managed := s.ReplayManagedTabActivity()
			if len(managed) != 1 || !managed["TARGET-aa11"].Equal(t0) {
				t.Fatalf("managed from prefix: %#v", managed)
			}
			if _, ok := managed["TARGET-bb22"]; ok {
				t.Fatalf("records after corrupt line should be abandoned: %#v", managed)
			}

			data, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			want := append(append([]byte{}, goodLine...), '\n')
			if string(data) != string(want) {
				t.Fatalf("log not truncated to good prefix:\n got: %q\nwant: %q", data, want)
			}

			// Append after recovery must not glue onto leftover corrupt bytes.
			if err := s.AppendRPC(LogRecord{
				Action: protocol.MethodGoto,
				Body:   json.RawMessage(`{"jsonrpc":"2.0","method":"goto","params":{"tab":"aa11","url":"https://ex"},"id":3}`),
				Tab:    "aa11", Target: "TARGET-aa11",
				OK:   true,
				Time: t0.Add(2 * time.Minute),
			}); err != nil {
				t.Fatal(err)
			}
			data, err = os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
			if len(lines) != 2 {
				t.Fatalf("expected 2 intact lines after append, got %d: %q", len(lines), data)
			}
			for i, line := range lines {
				if !json.Valid([]byte(line)) {
					t.Fatalf("line %d not valid JSON: %q", i, line)
				}
			}
		})
	}
}

func TestLogRotationPreservesManaged(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(OpenConfig{StateDir: dir, MaxLogBytes: 512})
	if err != nil {
		t.Fatal(err)
	}

	newBody := json.RawMessage(`{"jsonrpc":"2.0","method":"tab_new","params":{},"id":1}`)
	if err := s.AppendRPC(LogRecord{
		Action: protocol.MethodTabNew, Body: newBody, Tab: "aa11", Target: "TARGET-aa11", OK: true, Time: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	// A tab that gets closed must NOT survive rotation.
	if err := s.AppendRPC(LogRecord{
		Action: protocol.MethodTabNew, Body: newBody, Tab: "bb22", Target: "TARGET-bb22", OK: true, Time: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendRPC(LogRecord{
		Action: protocol.MethodTabClose,
		Body:   json.RawMessage(`{"jsonrpc":"2.0","method":"tab_close","params":{"tab":"bb22"},"id":2}`),
		Tab:    "bb22", Target: "TARGET-bb22", OK: true, Time: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	gotoBody := json.RawMessage(`{"jsonrpc":"2.0","method":"goto","params":{"tab":"aa11","url":"https://example.com/some/long/path"},"id":3}`)
	for i := 0; i < 50; i++ {
		if err := s.AppendRPC(LogRecord{
			Action: protocol.MethodGoto, Body: gotoBody, Tab: "aa11", Target: "TARGET-aa11", OK: true, Time: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := os.Stat(filepath.Join(dir, rpcLogFile+".1")); err != nil {
		t.Fatalf("expected rotated backup rpc.jsonl.1: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(OpenConfig{StateDir: dir, MaxLogBytes: 512})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	managed := s2.ReplayManagedTabActivity()
	if _, ok := managed["TARGET-aa11"]; !ok {
		t.Fatalf("managed tab lost across rotation: %#v", managed)
	}
	if _, ok := managed["TARGET-bb22"]; ok {
		t.Fatalf("closed tab resurrected by snapshot: %#v", managed)
	}
}

func TestReplayManagedTabActivity(t *testing.T) {
	t0 := time.Now().UTC().Add(-10 * time.Minute)
	t1 := t0.Add(2 * time.Minute)
	t2 := t1.Add(time.Minute)

	managed := make(map[string]managedTab)
	applyManagedUpdate(managed, LogRecord{
		Action: protocol.MethodTabNew, Tab: "aa11", Target: "TARGET-aa11", Seq: 1, OK: true, Time: t0,
	})
	applyManagedUpdate(managed, LogRecord{
		Action: protocol.MethodGoto,
		Body:   json.RawMessage(`{"jsonrpc":"2.0","method":"goto","params":{"tab":"aa11"},"id":2}`),
		Tab:    "aa11", Target: "TARGET-aa11", Seq: 2, OK: true, Time: t1,
	})
	applyManagedUpdate(managed, LogRecord{
		Action: protocol.MethodTabClose,
		Body:   json.RawMessage(`{"jsonrpc":"2.0","method":"tab_close","params":{"tab":"aa11"},"id":3}`),
		Tab:    "aa11", Target: "TARGET-aa11", Seq: 3, OK: true, Time: t2,
	})
	applyManagedUpdate(managed, LogRecord{
		Action: protocol.MethodTabNew, Tab: "bb22", Target: "TARGET-bb22", Seq: 4, OK: true, Time: t1,
	})
	applyManagedUpdate(managed, LogRecord{
		Action: protocol.MethodGoto,
		Body:   json.RawMessage(`{"jsonrpc":"2.0","method":"goto","params":{"tab":"bb22"},"id":5}`),
		Tab:    "bb22", Target: "TARGET-bb22", Seq: 5, OK: false, Error: "fail", Time: t2,
	})
	applyManagedUpdate(managed, LogRecord{
		Action: protocol.MethodGoto,
		Body:   json.RawMessage(`{"jsonrpc":"2.0","method":"goto","params":{"tab":"cc33"},"id":6}`),
		Tab:    "cc33", Target: "TARGET-cc33", Seq: 6, OK: true, Time: t2,
	})

	if len(managed) != 1 {
		t.Fatalf("managed: %#v", managed)
	}
	if !managed["TARGET-bb22"].at.Equal(t1) {
		t.Fatalf("bb22 last activity: got %v want %v", managed["TARGET-bb22"].at, t1)
	}
}

// A short id is only an alias. Records are attributed by the full target id
// they carry, so a short id reused by another tab cannot inherit, refresh or
// revoke the managed state of the tab that had it before.
func TestReplayAttributesByTargetNotShortID(t *testing.T) {
	t0 := time.Now().UTC().Add(-10 * time.Minute)
	gotoBody := json.RawMessage(`{"jsonrpc":"2.0","method":"goto","params":{"tab":"abcd"},"id":2}`)
	closeBody := json.RawMessage(`{"jsonrpc":"2.0","method":"tab_close","params":{"tab":"abcd"},"id":3}`)

	managed := make(map[string]managedTab)
	applyManagedUpdate(managed, LogRecord{Action: protocol.MethodTabNew, Tab: "abcd", Target: "A", OK: true, Time: t0})
	// The same short id now names target B, which the daemon did not create.
	applyManagedUpdate(managed, LogRecord{Action: protocol.MethodGoto, Body: gotoBody, Tab: "abcd", Target: "B", OK: true, Time: t0.Add(time.Minute)})
	applyManagedUpdate(managed, LogRecord{Action: protocol.MethodTabClose, Body: closeBody, Tab: "abcd", Target: "B", OK: true, Time: t0.Add(2 * time.Minute)})
	if len(managed) != 1 || !managed["A"].at.Equal(t0) {
		t.Fatalf("records about B changed A: %#v", managed)
	}

	// Activity on A under a different alias still counts for A.
	applyManagedUpdate(managed, LogRecord{Action: protocol.MethodGoto, Body: gotoBody, Tab: "0abcd", Target: "A", OK: true, Time: t0.Add(3 * time.Minute)})
	if got := managed["A"]; !got.at.Equal(t0.Add(3*time.Minute)) || got.short != "0abcd" {
		t.Fatalf("A after activity under its longer alias: %#v", got)
	}
	applyManagedUpdate(managed, LogRecord{Action: protocol.MethodTabClose, Body: closeBody, Tab: "0abcd", Target: "A", OK: true, Time: t0.Add(4 * time.Minute)})
	if len(managed) != 0 {
		t.Fatalf("tab_close of A left it managed: %#v", managed)
	}
}

// Records written before the log carried target ids cannot prove which tab
// they were about: they neither grant, refresh nor revoke managed status.
func TestReplaySkipsRecordsWithoutTargetIdentity(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	newBody := json.RawMessage(`{"jsonrpc":"2.0","method":"tab_new","params":{},"id":1}`)
	gotoBody := json.RawMessage(`{"jsonrpc":"2.0","method":"goto","params":{"tab":"abcd","url":"https://ex"},"id":2}`)
	closeBody := json.RawMessage(`{"jsonrpc":"2.0","method":"tab_close","params":{"tab":"abcd"},"id":3}`)
	records := []LogRecord{
		{Action: protocol.MethodTabNew, Body: newBody, Tab: "ef01", OK: true, Time: t0}, // legacy
		{Action: protocol.MethodTabNew, Body: newBody, Tab: "abcd", Target: "A", OK: true, Time: t0},
		{Action: protocol.MethodGoto, Body: gotoBody, Tab: "abcd", OK: true, Time: t0.Add(time.Minute)},          // legacy
		{Action: protocol.MethodTabClose, Body: closeBody, Tab: "abcd", OK: true, Time: t0.Add(2 * time.Minute)}, // legacy
	}
	var b strings.Builder
	for _, rec := range records {
		line, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dir, rpcLogFile), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(OpenConfig{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	managed := s.ReplayManagedTabActivity()
	if len(managed) != 1 || !managed["A"].Equal(t0) {
		t.Fatalf("managed = %#v, want only target A at its tab_new time", managed)
	}
}

// The snapshot seeding a rotated log must carry the full target id, otherwise
// a restart after rotation would fall back to trusting short ids.
func TestLogRotationSnapshotKeepsTargetIdentity(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(OpenConfig{StateDir: dir, MaxLogBytes: 512})
	if err != nil {
		t.Fatal(err)
	}
	const (
		targetA = "FFFF000000000000000000000000ABCD"
		targetB = "0000111111111111111111111111ABCD"
	)
	newBody := json.RawMessage(`{"jsonrpc":"2.0","method":"tab_new","params":{},"id":1}`)
	createdA := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	if err := s.AppendRPC(LogRecord{Action: protocol.MethodTabNew, Body: newBody, Tab: "abcd", Target: targetA, OK: true, Time: createdA}); err != nil {
		t.Fatal(err)
	}
	// Activity on the unmanaged colliding tab B is what fills the log.
	evalBody := json.RawMessage(`{"jsonrpc":"2.0","method":"eval","params":{"tab":"1abcd","script":"document.title"},"id":2}`)
	for i := 0; i < 50; i++ {
		if err := s.AppendRPC(LogRecord{Action: protocol.MethodEval, Body: evalBody, Tab: "1abcd", Target: targetB, OK: true, Time: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, rpcLogFile+".1")); err != nil {
		t.Fatalf("expected rotated backup rpc.jsonl.1: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(dir, rpcLogFile))
	if err != nil {
		t.Fatal(err)
	}
	var first LogRecord
	if err := json.Unmarshal([]byte(strings.SplitN(string(data), "\n", 2)[0]), &first); err != nil {
		t.Fatal(err)
	}
	if first.Action != protocol.MethodTabNew || first.Target != targetA || first.Tab != "abcd" || !first.Time.Equal(createdA) {
		t.Fatalf("snapshot record = %+v, want synthetic tab_new for target A", first)
	}

	s2, err := Open(OpenConfig{StateDir: dir, MaxLogBytes: 512})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	managed := s2.ReplayManagedTabActivity()
	if len(managed) != 1 || !managed[targetA].Equal(createdA) {
		t.Fatalf("managed after rotation + restart = %#v, want only target A", managed)
	}
}
