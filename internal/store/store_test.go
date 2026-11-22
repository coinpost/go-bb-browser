package store

import (
	"encoding/json"
	"errors"
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

	// Only synchronous lifecycle registration creates managed membership.
	for _, tab := range []string{"aa11", "bb22"} {
		if err := s.RegisterManagedTab(tab, "target-"+tab, t0); err != nil {
			t.Fatal(err)
		}
	}
	appendOK(LogRecord{Action: protocol.MethodTabNew, Body: newBody, Tab: "aa11", SenderIP: "127.0.0.1", Seq: 1, OK: true, Time: t0})
	appendOK(LogRecord{Action: protocol.MethodTabNew, Body: newBody, Tab: "bb22", OK: true, Time: t0})
	// Activity matches the full target even when rec.Tab is empty.
	appendOK(LogRecord{Action: protocol.MethodGoto, Body: gotoBody, Target: "target-aa11", OK: true, Time: t0.Add(time.Minute)})
	// Not updates: tab_list, failed RPCs, and unmanaged tabs.
	appendOK(LogRecord{Action: protocol.MethodTabList, Body: listBody, Tab: "aa11", Target: "target-aa11", OK: true, Time: t0.Add(2 * time.Minute)})
	appendOK(LogRecord{Action: protocol.MethodGoto, Body: gotoBody, Tab: "aa11", Target: "target-aa11", OK: false, Error: "fail", Time: t0.Add(3 * time.Minute)})
	appendOK(LogRecord{Action: protocol.MethodGoto, Body: gotoBody, Tab: "cc33", Target: "target-cc33", OK: true, Time: t0.Add(4 * time.Minute)})
	// Audit close cannot retire a tab; only synchronous lifecycle retirement can.
	appendOK(LogRecord{Action: protocol.MethodTabClose, Body: closeBB, Tab: "bb22", Target: "target-bb22", OK: true, Time: t0.Add(5 * time.Minute)})
	if got := s.ManagedTabs(); len(got) != 2 {
		t.Fatalf("audit close changed membership: %#v", got)
	}
	if retired, err := s.RetireManagedTarget("target-bb22"); err != nil || !retired {
		t.Fatalf("retire: %v %v", retired, err)
	}

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
	appendOK(LogRecord{Action: protocol.MethodGoto, Body: gotoBody, Tab: "aa11", Target: "target-aa11", OK: true})
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
			if err := s.RegisterManagedTab("aa11", "target-aa11", t0); err != nil {
				t.Fatal(err)
			}

			closeBody := json.RawMessage(`{"jsonrpc":"2.0","method":"tab_close","params":{"tab":"aa11"},"id":2}`)
			cases := []struct {
				name    string
				rec     LogRecord
				wantErr string // "" = any error (encoding/json wording varies by Go version)
			}{
				{name: "empty action", rec: LogRecord{Body: closeBody, Tab: "aa11", Target: "target-aa11", OK: true, Time: t0.Add(time.Minute)}, wantErr: "log record action required"},
				{name: "nil body", rec: LogRecord{Action: protocol.MethodTabClose, Tab: "aa11", Target: "target-aa11", OK: true, Time: t0.Add(time.Minute)}, wantErr: "log record body required"},
				{name: "empty body", rec: LogRecord{Action: protocol.MethodTabClose, Body: json.RawMessage{}, Tab: "aa11", Target: "target-aa11", OK: true, Time: t0.Add(time.Minute)}, wantErr: "log record body required"},
				{name: "invalid JSON body close", rec: LogRecord{Action: protocol.MethodTabClose, Body: json.RawMessage(`{"params":{"tab":"aa11"}`), Tab: "aa11", Target: "target-aa11", OK: true, Time: t0.Add(time.Minute)}, wantErr: ""},
				{name: "invalid JSON body new", rec: LogRecord{Action: protocol.MethodTabNew, Body: json.RawMessage(`not json`), Tab: "bb22", Target: "target-bb22", OK: true, Time: t0.Add(time.Minute)}, wantErr: ""},
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
		if err := s.RegisterManagedTab(tab, "target-"+tab, t0); err != nil {
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
				Action: protocol.MethodNetwork, Body: body, Tab: tab, Target: "target-" + tab, SenderIP: "127.0.0.1",
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

func TestOpenTruncatesIncompleteFinalLine(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, rpcLogFile)

	t0 := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	goodLine := mustMarshalRecord(t, tabNewRecord("aa11", t0))

	cases := []struct {
		name string
		tail string
	}{
		{name: "truncated final line without newline", tail: `{"action":"tab_new","body":`},
		{name: "truncated final line after blank line", tail: "\n" + `{"action":"tab_new","body":`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(logPath, append(append(append([]byte{}, goodLine...), '\n'), tc.tail...), 0o644); err != nil {
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

			data, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), `"body":`+"\n") || !strings.HasSuffix(string(data), "\n") ||
				strings.TrimRight(string(data), "\n") != string(goodLine) {
				t.Fatalf("log not truncated to good prefix: %q", data)
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
			valid, invalid := readLogLines(t, logPath)
			if len(valid) != 2 || invalid != 0 {
				t.Fatalf("expected 2 intact lines after append, got valid=%d invalid=%d", len(valid), invalid)
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
