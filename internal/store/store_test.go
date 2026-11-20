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
		Tab:    "ab12",
		Seq:    seq1,
		OK:     true,
		Time:   time.Now().UTC(),
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
		{Action: protocol.MethodTabNew, Body: json.RawMessage(`{"jsonrpc":"2.0","method":"tab_new","params":{},"id":1}`), Tab: "aa11", OK: true, Time: t0},
		{Action: protocol.MethodGoto, Body: json.RawMessage(`{"jsonrpc":"2.0","method":"goto","params":{"tab":"aa11","url":"https://ex"},"id":2}`), Tab: "aa11", OK: true, Time: t0.Add(time.Minute)},
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
		Action: protocol.MethodTabNew,
		Body:   json.RawMessage(`{"jsonrpc":"2.0","method":"tab_new","params":{},"id":1}`),
		Tab:    "aa11",
		OK:     true,
		Time:   t0,
	}
	goodLine, err := json.Marshal(good)
	if err != nil {
		t.Fatal(err)
	}
	later := LogRecord{
		Action: protocol.MethodTabNew,
		Body:   json.RawMessage(`{"jsonrpc":"2.0","method":"tab_new","params":{},"id":2}`),
		Tab:    "bb22",
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

	newBody := json.RawMessage(`{"jsonrpc":"2.0","method":"tab_new","params":{},"id":1}`)
	if err := s.AppendRPC(LogRecord{
		Action: protocol.MethodTabNew, Body: newBody, Tab: "aa11", OK: true, Time: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	// A tab that gets closed must NOT survive rotation.
	if err := s.AppendRPC(LogRecord{
		Action: protocol.MethodTabNew, Body: newBody, Tab: "bb22", OK: true, Time: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendRPC(LogRecord{
		Action: protocol.MethodTabClose,
		Body:   json.RawMessage(`{"jsonrpc":"2.0","method":"tab_close","params":{"tab":"bb22"},"id":2}`),
		Tab:    "bb22", OK: true, Time: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	gotoBody := json.RawMessage(`{"jsonrpc":"2.0","method":"goto","params":{"tab":"aa11","url":"https://example.com/some/long/path"},"id":3}`)
	for i := 0; i < 50; i++ {
		if err := s.AppendRPC(LogRecord{
			Action: protocol.MethodGoto, Body: gotoBody, Tab: "aa11", OK: true, Time: time.Now().UTC(),
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
	if _, ok := managed["aa11"]; !ok {
		t.Fatalf("managed tab lost across rotation: %#v", managed)
	}
	if _, ok := managed["bb22"]; ok {
		t.Fatalf("closed tab resurrected by snapshot: %#v", managed)
	}
}

func TestReplayManagedTabActivity(t *testing.T) {
	t0 := time.Now().UTC().Add(-10 * time.Minute)
	t1 := t0.Add(2 * time.Minute)
	t2 := t1.Add(time.Minute)

	log := newManagedLog()
	log.apply(LogRecord{
		Action: protocol.MethodTabNew, Tab: "aa11", Seq: 1, OK: true, Time: t0,
	})
	log.apply(LogRecord{
		Action: protocol.MethodGoto,
		Body:   json.RawMessage(`{"jsonrpc":"2.0","method":"goto","params":{"tab":"aa11"},"id":2}`),
		Tab:    "aa11", Seq: 2, OK: true, Time: t1,
	})
	log.apply(LogRecord{
		Action: protocol.MethodTabClose,
		Body:   json.RawMessage(`{"jsonrpc":"2.0","method":"tab_close","params":{"tab":"aa11"},"id":3}`),
		Tab:    "aa11", Seq: 3, OK: true, Time: t2,
	})
	log.apply(LogRecord{
		Action: protocol.MethodTabNew, Tab: "bb22", Seq: 4, OK: true, Time: t1,
	})
	log.apply(LogRecord{
		Action: protocol.MethodGoto,
		Body:   json.RawMessage(`{"jsonrpc":"2.0","method":"goto","params":{"tab":"bb22"},"id":5}`),
		Tab:    "bb22", Seq: 5, OK: false, Error: "fail", Time: t2,
	})
	log.apply(LogRecord{
		Action: protocol.MethodGoto,
		Body:   json.RawMessage(`{"jsonrpc":"2.0","method":"goto","params":{"tab":"cc33"},"id":6}`),
		Tab:    "cc33", Seq: 6, OK: true, Time: t2,
	})

	managed := log.shorts()
	if len(managed) != 1 {
		t.Fatalf("managed: %#v", managed)
	}
	if !managed["bb22"].Equal(t1) {
		t.Fatalf("bb22 last activity: got %v want %v", managed["bb22"], t1)
	}
}

func TestManagedTabGoneRecordReleasesTabOnReplay(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(OpenConfig{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AppendRPC(LogRecord{
		Action: protocol.MethodTabNew,
		Body:   json.RawMessage(`{"jsonrpc":"2.0","method":"tab_new","params":{},"id":1}`),
		Tab:    `a"b`,
		OK:     true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendRPC(ManagedTabGoneRecord(`a"b`, "", time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	if got := st.ReplayManagedTabActivity(); len(got) != 0 {
		t.Fatalf("managed after gone record = %v", got)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(OpenConfig{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if got := st.ReplayManagedTabActivity(); len(got) != 0 {
		t.Fatalf("managed after replay = %v", got)
	}
}

func tabRec(action, tab, target string, at time.Time) LogRecord {
	return LogRecord{
		Action: action,
		Body:   json.RawMessage(`{"jsonrpc":"2.0","method":"` + action + `","params":{"tab":"` + tab + `"},"id":1}`),
		Tab:    tab, Target: target, OK: true, Time: at,
	}
}

// Records that name a target touch that target's entry only, in any order
// relative to another tab that held the same short id.
func TestReplayKeysManagedTabsByFullTargetID(t *testing.T) {
	t0 := time.Now().UTC().Add(-time.Hour)
	log := newManagedLog()
	log.apply(tabRec(protocol.MethodTabNew, "1234", "AAAA1234", t0))
	log.apply(tabRec(protocol.MethodTabNew, "1234", "BBBB1234", t0.Add(time.Minute)))
	// Late records about the first tab: its activity, then its close.
	log.apply(tabRec(protocol.MethodGoto, "1234", "AAAA1234", t0.Add(2*time.Minute)))
	log.apply(tabRec(protocol.MethodTabClose, "1234", "AAAA1234", t0.Add(3*time.Minute)))
	// A record without a target says nothing about a full-id entry.
	log.apply(tabRec(protocol.MethodGoto, "1234", "", t0.Add(4*time.Minute)))
	log.apply(tabRec(protocol.MethodTabClose, "1234", "", t0.Add(5*time.Minute)))

	tabs := log.tabs()
	if len(tabs) != 1 || tabs[0].Target != "BBBB1234" || tabs[0].Short != "1234" {
		t.Fatalf("managed = %+v, want only BBBB1234", tabs)
	}
	if !tabs[0].LastActivity.Equal(t0.Add(time.Minute)) {
		t.Fatalf("last activity = %v, want its own tab_new time", tabs[0].LastActivity)
	}
	log.apply(tabRec(protocol.MethodGoto, "1234", "BBBB1234", t0.Add(6*time.Minute)))
	if got := log.tabs()[0].LastActivity; !got.Equal(t0.Add(6 * time.Minute)) {
		t.Fatalf("last activity = %v, want the goto time", got)
	}
}

// Lines written before records carried a target id stay a separate, short-id
// keyed set: readable, releasable by an old-style tab_close, and never merged
// into the full-id entries.
func TestReplayKeepsLegacyShortIDRecordsApart(t *testing.T) {
	t0 := time.Now().UTC().Add(-time.Hour)
	log := newManagedLog()
	log.apply(tabRec(protocol.MethodTabNew, "aa11", "", t0))
	log.apply(tabRec(protocol.MethodTabNew, "bb22", "", t0))
	log.apply(tabRec(protocol.MethodGoto, "aa11", "", t0.Add(time.Minute)))
	log.apply(tabRec(protocol.MethodTabClose, "bb22", "", t0.Add(time.Minute)))
	// A full-id release of some tab that had the short id aa11 is not about it.
	log.apply(tabRec(protocol.MethodTabClose, "aa11", "CCCCaa11", t0.Add(2*time.Minute)))

	if len(log.byTarget) != 0 {
		t.Fatalf("full-id entries = %v", log.byTarget)
	}
	if len(log.legacy) != 1 || !log.legacy["aa11"].Equal(t0.Add(time.Minute)) {
		t.Fatalf("legacy = %v, want aa11 at its goto time", log.legacy)
	}
}

// Rotation must not reduce a managed tab to its short id: two targets that
// held the same short id at different times stay distinguishable in the fresh
// log, and a legacy entry stays legacy.
func TestLogRotationKeepsFullTargetIDs(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(OpenConfig{StateDir: dir, MaxLogBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	for _, rec := range []LogRecord{
		tabRec(protocol.MethodTabNew, "1234", "AAAA1234", t0),
		tabRec(protocol.MethodTabClose, "1234", "AAAA1234", t0),
		tabRec(protocol.MethodTabNew, "1234", "BBBB1234", t0.Add(time.Minute)),
		tabRec(protocol.MethodTabNew, "91234", "CCCC91234", t0.Add(2*time.Minute)),
		tabRec(protocol.MethodTabNew, "ee55", "", t0.Add(3*time.Minute)),
	} {
		if err := s.AppendRPC(rec); err != nil {
			t.Fatal(err)
		}
	}
	for range 40 {
		if err := s.AppendRPC(tabRec(protocol.MethodTabList, "", "", time.Now().UTC())); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, rpcLogFile+".1")); err != nil {
		t.Fatalf("expected rotated backup rpc.jsonl.1: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(OpenConfig{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	tabs, legacy := s2.StartupManaged()
	want := []ManagedTab{
		{Target: "BBBB1234", Short: "1234", LastActivity: t0.Add(time.Minute)},
		{Target: "CCCC91234", Short: "91234", LastActivity: t0.Add(2 * time.Minute)},
	}
	if len(tabs) != len(want) {
		t.Fatalf("managed after rotation = %+v, want %+v", tabs, want)
	}
	for i := range want {
		if tabs[i].Target != want[i].Target || tabs[i].Short != want[i].Short || !tabs[i].LastActivity.Equal(want[i].LastActivity) {
			t.Fatalf("managed[%d] = %+v, want %+v", i, tabs[i], want[i])
		}
	}
	if len(legacy) != 1 || !legacy["ee55"].Equal(t0.Add(3*time.Minute)) {
		t.Fatalf("legacy after rotation = %v, want ee55", legacy)
	}
}
