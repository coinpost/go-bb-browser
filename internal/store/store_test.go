package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
	if !s.InMemory() {
		t.Fatal("expected in-memory mode")
	}
	if got := s.ReplayManagedTabActivity(); got == nil || len(got) != 0 {
		t.Fatalf("fresh store managed: %#v", got)
	}

	t0 := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	newBody := json.RawMessage(`{"jsonrpc":"2.0","method":"tab_new","params":{},"id":1}`)
	gotoBody := json.RawMessage(`{"jsonrpc":"2.0","method":"goto","params":{"tab":"aa11","url":"https://ex"},"id":2}`)
	listBody := json.RawMessage(`{"jsonrpc":"2.0","method":"tab_list","params":{},"id":3}`)
	closeBB := json.RawMessage(`{"jsonrpc":"2.0","method":"tab_close","params":{"tab":"bb22"},"id":4}`)

	appendOK := func(rec LogRecord) {
		t.Helper()
		if err := s.AppendRPC(rec); err != nil {
			t.Fatal(err)
		}
	}

	// Create two managed tabs.
	appendOK(LogRecord{Action: protocol.MethodTabNew, Body: newBody, Tab: "aa11", SenderIP: "127.0.0.1", Seq: 1, OK: true, Time: t0})
	appendOK(LogRecord{Action: protocol.MethodTabNew, Body: newBody, Tab: "bb22", OK: true, Time: t0})
	// Update: tab taken from the request body when rec.Tab is empty.
	appendOK(LogRecord{Action: protocol.MethodGoto, Body: gotoBody, OK: true, Time: t0.Add(time.Minute)})
	// Not updates: tab_list, failed RPCs, and unmanaged tabs.
	appendOK(LogRecord{Action: protocol.MethodTabList, Body: listBody, Tab: "aa11", OK: true, Time: t0.Add(2 * time.Minute)})
	appendOK(LogRecord{Action: protocol.MethodGoto, Body: gotoBody, Tab: "aa11", OK: false, Error: "fail", Time: t0.Add(3 * time.Minute)})
	appendOK(LogRecord{Action: protocol.MethodGoto, Body: gotoBody, Tab: "cc33", OK: true, Time: t0.Add(4 * time.Minute)})
	// Close releases the tab.
	appendOK(LogRecord{Action: protocol.MethodTabClose, Body: closeBB, Tab: "bb22", OK: true, Time: t0.Add(5 * time.Minute)})

	managed := s.ReplayManagedTabActivity()
	if len(managed) != 1 || !managed["aa11"].Equal(t0.Add(time.Minute)) {
		t.Fatalf("managed after create/update/close: %#v", managed)
	}

	// The returned map is a copy: mutating it must not leak into the store.
	managed["aa11"] = t0.Add(24 * time.Hour)
	managed["zz99"] = t0
	delete(managed, "aa11")
	again := s.ReplayManagedTabActivity()
	if len(again) != 1 || !again["aa11"].Equal(t0.Add(time.Minute)) {
		t.Fatalf("store state affected by caller mutation: %#v", again)
	}

	// A zero Time is filled with the current time.
	before := time.Now().UTC()
	appendOK(LogRecord{Action: protocol.MethodGoto, Body: gotoBody, Tab: "aa11", OK: true})
	after := time.Now().UTC()
	got := s.ReplayManagedTabActivity()["aa11"]
	if got.Before(before) || got.After(after) {
		t.Fatalf("zero time not filled with now: got %v, want within [%v, %v]", got, before, after)
	}
}

// Rejected records must keep their existing error semantics and must not touch
// managed-tab state in either storage mode.
func TestAppendRPCRejectedInputLeavesManagedUntouched(t *testing.T) {
	modes := []struct {
		name string
		open func(t *testing.T) *Store
	}{
		{name: "memory", open: openTestStore},
		{name: "disk", open: func(t *testing.T) *Store {
			t.Helper()
			s, err := Open(OpenConfig{StateDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			return s
		}},
	}
	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			s := mode.open(t)
			t0 := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
			if err := s.AppendRPC(LogRecord{
				Action: protocol.MethodTabNew,
				Body:   json.RawMessage(`{"jsonrpc":"2.0","method":"tab_new","params":{},"id":1}`),
				Tab:    "aa11", OK: true, Time: t0,
			}); err != nil {
				t.Fatal(err)
			}

			closeBody := json.RawMessage(`{"jsonrpc":"2.0","method":"tab_close","params":{"tab":"aa11"},"id":2}`)
			cases := []struct {
				name    string
				rec     LogRecord
				wantErr string // "" = any error (encoding/json wording varies by Go version)
			}{
				{name: "empty action", rec: LogRecord{Body: closeBody, Tab: "aa11", OK: true, Time: t0.Add(time.Minute)}, wantErr: "log record action required"},
				{name: "nil body", rec: LogRecord{Action: protocol.MethodTabClose, Tab: "aa11", OK: true, Time: t0.Add(time.Minute)}, wantErr: "log record body required"},
				{name: "empty body", rec: LogRecord{Action: protocol.MethodTabClose, Body: json.RawMessage{}, Tab: "aa11", OK: true, Time: t0.Add(time.Minute)}, wantErr: "log record body required"},
				{name: "invalid JSON body close", rec: LogRecord{Action: protocol.MethodTabClose, Body: json.RawMessage(`{"params":{"tab":"aa11"}`), Tab: "aa11", OK: true, Time: t0.Add(time.Minute)}, wantErr: ""},
				{name: "invalid JSON body new", rec: LogRecord{Action: protocol.MethodTabNew, Body: json.RawMessage(`not json`), Tab: "bb22", OK: true, Time: t0.Add(time.Minute)}, wantErr: ""},
			}
			for _, tc := range cases {
				err := s.AppendRPC(tc.rec)
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("%s: err = %v, want containing %q", tc.name, err, tc.wantErr)
				}
				managed := s.ReplayManagedTabActivity()
				if len(managed) != 1 || !managed["aa11"].Equal(t0) {
					t.Fatalf("%s: managed changed by rejected record: %#v", tc.name, managed)
				}
			}
		})
	}
}

// In-memory mode has no reader for RPC history, so AppendRPC must not retain
// records or their bodies: live heap after a forced GC has to stay flat while
// the number of appended records grows (managed tab count held constant).
func TestAppendRPCInMemoryDoesNotRetainHistory(t *testing.T) {
	const (
		total     = 100_000
		batch     = 20_000
		maxGrowth = 2 << 20 // bytes; retained history would be tens of MiB
	)
	s := openTestStore(t)
	t0 := time.Now().UTC().Truncate(time.Millisecond)
	tabs := []string{"aa11", "bb22"}
	for _, tab := range tabs {
		if err := s.AppendRPC(LogRecord{
			Action: protocol.MethodTabNew,
			Body:   json.RawMessage(`{"jsonrpc":"2.0","method":"tab_new","params":{},"id":0}`),
			Tab:    tab, OK: true, Time: t0,
		}); err != nil {
			t.Fatal(err)
		}
	}

	liveHeap := func() int64 {
		runtime.GC()
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		return int64(ms.HeapAlloc)
	}

	// Observation polling shape: a fresh body per call (as the HTTP handler
	// produces), short enough that compaction leaves it untouched.
	pad := strings.Repeat("x", 150)
	appendBatch := func(from int) {
		for i := from; i < from+batch; i++ {
			tab := tabs[i%len(tabs)]
			body := fmt.Appendf(nil, `{"jsonrpc":"2.0","method":"network","params":{"tab":%q,"since":%d,"pad":%q},"id":%d}`, tab, i, pad, i)
			if err := s.AppendRPC(LogRecord{
				Action: protocol.MethodNetwork, Body: body, Tab: tab, SenderIP: "127.0.0.1",
				Seq: uint64(i + 1), OK: true, Time: t0.Add(time.Duration(i+1) * time.Millisecond),
			}); err != nil {
				t.Fatal(err)
			}
		}
	}

	// First batch warms up one-time allocations (maps, encoder caches).
	appendBatch(0)
	base := liveHeap()
	for done := batch; done < total; done += batch {
		appendBatch(done)
		if growth := liveHeap() - base; growth > maxGrowth {
			t.Fatalf("live heap grew %d bytes between %d and %d appended records (limit %d): RPC history is retained",
				growth, batch, done+batch, maxGrowth)
		}
	}

	managed := s.ReplayManagedTabActivity()
	if len(managed) != len(tabs) {
		t.Fatalf("managed: %#v", managed)
	}
	for i, tab := range tabs {
		// Last record for tabs[i] is index total-len(tabs)+i.
		want := t0.Add(time.Duration(total-len(tabs)+i+1) * time.Millisecond)
		if !managed[tab].Equal(want) {
			t.Fatalf("%s last activity: got %v want %v", tab, managed[tab], want)
		}
	}
	runtime.KeepAlive(s)
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

	managed := make(map[string]time.Time)
	applyManagedUpdate(managed, LogRecord{
		Action: protocol.MethodTabNew, Tab: "aa11", Seq: 1, OK: true, Time: t0,
	})
	applyManagedUpdate(managed, LogRecord{
		Action: protocol.MethodGoto,
		Body:   json.RawMessage(`{"jsonrpc":"2.0","method":"goto","params":{"tab":"aa11"},"id":2}`),
		Tab:    "aa11", Seq: 2, OK: true, Time: t1,
	})
	applyManagedUpdate(managed, LogRecord{
		Action: protocol.MethodTabClose,
		Body:   json.RawMessage(`{"jsonrpc":"2.0","method":"tab_close","params":{"tab":"aa11"},"id":3}`),
		Tab:    "aa11", Seq: 3, OK: true, Time: t2,
	})
	applyManagedUpdate(managed, LogRecord{
		Action: protocol.MethodTabNew, Tab: "bb22", Seq: 4, OK: true, Time: t1,
	})
	applyManagedUpdate(managed, LogRecord{
		Action: protocol.MethodGoto,
		Body:   json.RawMessage(`{"jsonrpc":"2.0","method":"goto","params":{"tab":"bb22"},"id":5}`),
		Tab:    "bb22", Seq: 5, OK: false, Error: "fail", Time: t2,
	})
	applyManagedUpdate(managed, LogRecord{
		Action: protocol.MethodGoto,
		Body:   json.RawMessage(`{"jsonrpc":"2.0","method":"goto","params":{"tab":"cc33"},"id":6}`),
		Tab:    "cc33", Seq: 6, OK: true, Time: t2,
	})

	if len(managed) != 1 {
		t.Fatalf("managed: %#v", managed)
	}
	if !managed["bb22"].Equal(t1) {
		t.Fatalf("bb22 last activity: got %v want %v", managed["bb22"], t1)
	}
}
