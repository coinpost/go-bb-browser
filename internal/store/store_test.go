package store

import (
	"encoding/json"
	"errors"
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
		Tab:    "ab12",
		Seq:    seq1,
		OK:     true,
		Time:   time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterManagedTab("ab12", "TARGET-AB12", time.Now().UTC()); err != nil {
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
	if managed["ab12"].IsZero() {
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
		{Action: ActionManagedTabOpen, Body: lifecycleBody, Tab: "aa11", Target: "TARGET-AA11", OK: true, Time: t0},
		{Action: protocol.MethodGoto, Body: json.RawMessage(`{"jsonrpc":"2.0","method":"goto","params":{"tab":"aa11","url":"https://ex"},"id":2}`), Tab: "aa11", Target: "TARGET-AA11", OK: true, Time: t0.Add(time.Minute)},
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
	if len(managed) != 1 || !managed["aa11"].Equal(want) {
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
		Action: ActionManagedTabOpen,
		Body:   lifecycleBody,
		Tab:    "aa11",
		Target: "TARGET-AA11",
		OK:     true,
		Time:   t0,
	}
	goodLine, err := json.Marshal(good)
	if err != nil {
		t.Fatal(err)
	}
	later := LogRecord{
		Action: ActionManagedTabOpen,
		Body:   lifecycleBody,
		Tab:    "bb22",
		Target: "TARGET-BB22",
		OK:     true,
		Time:   t0.Add(time.Minute),
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
			if len(managed) != 1 || !managed["aa11"].Equal(t0) {
				t.Fatalf("managed from prefix: %#v", managed)
			}
			if _, ok := managed["bb22"]; ok {
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
				Tab:    "aa11",
				OK:     true,
				Time:   t0.Add(2 * time.Minute),
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

	liveAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	if err := s.RegisterManagedTab("aa11", "TARGET-AA11", liveAt); err != nil {
		t.Fatal(err)
	}
	// A retired tab must NOT survive rotation.
	if err := s.RegisterManagedTab("bb22", "TARGET-BB22", liveAt); err != nil {
		t.Fatal(err)
	}
	if retired, err := s.RetireManagedTarget("TARGET-BB22"); err != nil || !retired {
		t.Fatalf("retire: %v %v", retired, err)
	}

	// Unmanaged-tab traffic fills the log without touching managed activity.
	gotoBody := json.RawMessage(`{"jsonrpc":"2.0","method":"goto","params":{"tab":"cc33","url":"https://example.com/some/long/path"},"id":3}`)
	for i := 0; i < 50; i++ {
		if err := s.AppendRPC(LogRecord{
			Action: protocol.MethodGoto, Body: gotoBody, Tab: "cc33", Target: "TARGET-CC33", OK: true, Time: time.Now().UTC(),
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

	data, err := os.ReadFile(filepath.Join(dir, rpcLogFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "TARGET-BB22") || strings.Contains(string(data), `"bb22"`) {
		t.Fatalf("rotated log reseeded a retired tab:\n%s", data)
	}

	s2, err := Open(OpenConfig{StateDir: dir, MaxLogBytes: 512})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got := s2.ManagedTabs()
	want := []ManagedTab{{Tab: "aa11", TargetID: "TARGET-AA11", LastActivity: liveAt}}
	if len(got) != 1 || got[0].Tab != want[0].Tab || got[0].TargetID != want[0].TargetID || !got[0].LastActivity.Equal(liveAt) {
		t.Fatalf("managed after rotation: %#v want %#v", got, want)
	}
}

func TestApplyManagedUpdate(t *testing.T) {
	t0 := time.Now().UTC().Add(-10 * time.Minute)
	t1 := t0.Add(2 * time.Minute)
	t2 := t1.Add(time.Minute)
	gotoRec := func(tab, targetID string, ok bool, at time.Time) LogRecord {
		return LogRecord{
			Action: protocol.MethodGoto,
			Body:   json.RawMessage(`{"jsonrpc":"2.0","method":"goto","params":{"tab":"` + tab + `"},"id":2}`),
			Tab:    tab, Target: targetID, OK: ok, Time: at,
		}
	}

	managed := make(map[string]managedEntry)
	for _, rec := range []LogRecord{
		{Action: ActionManagedTabOpen, Tab: "aa11", Target: "A", OK: true, Time: t0},
		gotoRec("aa11", "A", true, t1),
		{Action: ActionManagedTabRetire, Tab: "aa11", Target: "A", OK: true, Time: t2},
		{Action: ActionManagedTabOpen, Tab: "bb22", Target: "B", OK: true, Time: t1},
		gotoRec("bb22", "B", false, t2), // failed RPC does not renew
		gotoRec("cc33", "C", true, t2),  // unmanaged target
		gotoRec("bb22", "", true, t2),   // alias only: unverifiable
		gotoRec("bb22", "X", true, t2),  // same alias, different target
		gotoRec("bb22", "B", true, t0),  // out-of-order older line does not rewind
		// Audit lines never change membership, with or without identity.
		{Action: protocol.MethodTabNew, Tab: "dd44", Target: "D", OK: true, Time: t2},
		{Action: protocol.MethodTabNew, Tab: "ee55", OK: true, Time: t2},
		{Action: protocol.MethodTabClose, Tab: "bb22", Target: "B", OK: true, Time: t0},
		{Action: protocol.MethodTabClose, Tab: "bb22", OK: true, Time: t2},
		// Retiring another target that shares the alias leaves B alone.
		{Action: ActionManagedTabRetire, Tab: "bb22", Target: "X", OK: true, Time: t2},
		// Lifecycle records without identity are ignored.
		{Action: ActionManagedTabOpen, Tab: "ff66", OK: true, Time: t2},
		{Action: ActionManagedTabRetire, Tab: "bb22", OK: true, Time: t2},
	} {
		applyManagedUpdate(managed, rec)
	}

	if len(managed) != 1 {
		t.Fatalf("managed: %#v", managed)
	}
	if got := managed["B"]; got.tab != "bb22" || !got.at.Equal(t1) {
		t.Fatalf("B: got %#v want tab bb22 at %v", got, t1)
	}
	applyManagedUpdate(managed, gotoRec("bb22", "B", true, t2))
	if got := managed["B"]; !got.at.Equal(t2) {
		t.Fatalf("B activity not renewed: %v want %v", got.at, t2)
	}
}

// Records written by versions that only stored the short id cannot be verified
// against a live target, so they are not restored as managed.
func TestOpenIgnoresLegacyAliasOnlyRecords(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Now().UTC().Add(-time.Hour)
	var b strings.Builder
	for _, rec := range []LogRecord{
		{Action: protocol.MethodTabNew, Body: json.RawMessage(`{"jsonrpc":"2.0","method":"tab_new","params":{},"id":1}`), Tab: "aa11", OK: true, Time: t0},
		{Action: protocol.MethodGoto, Body: json.RawMessage(`{"jsonrpc":"2.0","method":"goto","params":{"tab":"aa11","url":"https://ex"},"id":2}`), Tab: "aa11", OK: true, Time: t0.Add(time.Minute)},
	} {
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
	if got := s.ManagedTabs(); len(got) != 0 {
		t.Fatalf("legacy records restored as managed: %#v", got)
	}
	if got := s.ReplayManagedTabActivity(); len(got) != 0 {
		t.Fatalf("legacy records restored as managed: %#v", got)
	}
}

func TestManagedLifecycleAcrossAliasReuse(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(OpenConfig{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	if err := s.RegisterManagedTab("1234", "OLD-1234", t0); err != nil {
		t.Fatal(err)
	}
	if retired, err := s.RetireManagedTarget("OLD-1234"); err != nil || !retired {
		t.Fatalf("retire: %v %v", retired, err)
	}
	// Retiring an unmanaged target is a no-op and writes nothing.
	before := s.logBytes
	if retired, err := s.RetireManagedTarget("OLD-1234"); err != nil || retired {
		t.Fatalf("second retire: %v %v", retired, err)
	}
	if retired, err := s.RetireManagedTarget("USER-TAB"); err != nil || retired {
		t.Fatalf("retire unmanaged: %v %v", retired, err)
	}
	if s.logBytes != before {
		t.Fatal("no-op retirement wrote to the log")
	}

	newAt := t0.Add(time.Minute)
	if err := s.RegisterManagedTab("1234", "NEW-1234", newAt); err != nil {
		t.Fatal(err)
	}
	// Delayed audit lines from the old lifecycle arrive after the alias was reused.
	for _, rec := range []LogRecord{
		{Action: protocol.MethodTabNew, Tab: "1234", Target: "OLD-1234", OK: true, Time: t0},
		{Action: protocol.MethodGoto, Tab: "1234", Target: "OLD-1234", OK: true, Time: time.Now().UTC()},
		{Action: protocol.MethodTabClose, Tab: "1234", Target: "OLD-1234", OK: true, Time: time.Now().UTC()},
		{Action: protocol.MethodTabClose, Tab: "1234", OK: true, Time: time.Now().UTC()},
	} {
		rec.Body = json.RawMessage(`{"jsonrpc":"2.0","method":"` + rec.Action + `","params":{"tab":"1234"},"id":1}`)
		if err := s.AppendRPC(rec); err != nil {
			t.Fatal(err)
		}
	}
	check := func(s *Store) {
		t.Helper()
		got := s.ManagedTabs()
		if len(got) != 1 || got[0].TargetID != "NEW-1234" || got[0].Tab != "1234" || !got[0].LastActivity.Equal(newAt) {
			t.Fatalf("managed: %#v", got)
		}
	}
	check(s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(OpenConfig{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	check(s2)
}

func TestAppendRPCRejectsLifecycleActions(t *testing.T) {
	s := openTestStore(t)
	for _, action := range []string{ActionManagedTabOpen, ActionManagedTabRetire} {
		if err := s.AppendRPC(LogRecord{Action: action, Body: lifecycleBody, Tab: "aa11", Target: "A", OK: true}); err == nil {
			t.Fatalf("AppendRPC accepted reserved action %q", action)
		}
	}
	if got := s.ManagedTabs(); len(got) != 0 {
		t.Fatalf("managed: %#v", got)
	}
}

// A retirement that cannot be written is reported to the caller, while the
// in-memory managed set is still retired.
func TestRetireManagedTargetReportsWriteFailure(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(OpenConfig{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterManagedTab("aa11", "A", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterManagedTab("bb22", "B", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	// Break the log file underneath the store.
	if err := s.logFile.Close(); err != nil {
		t.Fatal(err)
	}
	retired, err := s.RetireManagedTarget("A")
	if !retired || err == nil {
		t.Fatalf("retire with broken log: retired=%v err=%v, want true and an error", retired, err)
	}
	if got := s.ManagedTabs(); len(got) != 1 || got[0].TargetID != "B" {
		t.Fatalf("in-memory managed set not retired: %#v", got)
	}
	if err := s.RegisterManagedTab("cc33", "C", time.Now().UTC()); err == nil {
		t.Fatal("register with broken log returned no error")
	}

	_ = s.Close()
	if _, err := s.RetireManagedTarget("B"); !errors.Is(err, ErrClosed) {
		t.Fatalf("retire after Close: %v, want ErrClosed", err)
	}
}

func TestManagedLifecycleInMemory(t *testing.T) {
	s := openTestStore(t)
	if err := s.RegisterManagedTab("aa11", "A", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterManagedTab("", "", time.Now().UTC()); err == nil {
		t.Fatal("empty target id accepted")
	}
	if got := s.ReplayManagedTabActivity(); len(got) != 1 || got["aa11"].IsZero() {
		t.Fatalf("managed: %#v", got)
	}
	if retired, err := s.RetireManagedTarget("A"); err != nil || !retired {
		t.Fatalf("retire: %v %v", retired, err)
	}
	if got := s.ManagedTabs(); len(got) != 0 {
		t.Fatalf("managed: %#v", got)
	}
}
