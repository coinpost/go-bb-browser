package store

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yiplee/go-bb-browser/pkg/protocol"
)

const (
	storeIDA = "AAAA0000000000000000000000AB1234"
	storeIDB = "BBBB0000000000000000000000AB1234"
)

// Records written by a daemon that named tabs by short alias carry the alias
// under "tab" and no "target_id". They are replayed without error but manage
// nothing, whatever the alias (or the request body) happens to contain.
func TestReplaySkipsRecordsWithoutCompleteTargetID(t *testing.T) {
	dir := t.TempDir()
	at := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	lines := []string{
		// legacy: short alias
		`{"action":"tab_new","body":{"jsonrpc":"2.0","method":"tab_new","params":{},"id":1},"tab":"1234","sender_ip":"","ok":true,"time":"` + at + `"}`,
		// legacy: "tab" that looks like a complete id is still not an identity
		`{"action":"tab_new","body":{"jsonrpc":"2.0","method":"tab_new","params":{},"id":2},"tab":"` + storeIDB + `","sender_ip":"","ok":true,"time":"` + at + `"}`,
		// current format
		`{"action":"tab_new","body":{"jsonrpc":"2.0","method":"tab_new","params":{},"id":3},"target_id":"` + storeIDA + `","sender_ip":"","ok":true,"time":"` + at + `"}`,
		// the tab a request body asks for is not an identity either: it must
		// neither register nor refresh nor release anything
		`{"action":"tab_new","body":{"jsonrpc":"2.0","method":"tab_new","params":{"tab":"` + storeIDB + `"},"id":4},"sender_ip":"","ok":true,"time":"` + at + `"}`,
		`{"action":"tab_close","body":{"jsonrpc":"2.0","method":"tab_close","params":{"tab":"` + storeIDA + `"},"id":5},"tab":"` + storeIDA + `","sender_ip":"","ok":true,"time":"` + at + `"}`,
	}
	if err := os.WriteFile(filepath.Join(dir, rpcLogFile), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	s, err := Open(OpenConfig{StateDir: dir, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	managed := s.ReplayManagedTabActivity()
	if len(managed) != 1 || managed[storeIDA].IsZero() {
		t.Fatalf("managed = %#v, want only %s", managed, storeIDA)
	}
	if !strings.Contains(logs.String(), "without a complete target id") || !strings.Contains(logs.String(), "records=3") {
		t.Fatalf("expected one warning counting the 3 unidentified tab_new records, got %q", logs.String())
	}
}

func TestLogRecordWritesTargetIDNotLegacyTab(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(OpenConfig{StateDir: dir, MaxLogBytes: 600})
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	if err := s.AppendRPC(LogRecord{
		Action: protocol.MethodTabNew, Body: json.RawMessage(`{"jsonrpc":"2.0","method":"tab_new","params":{},"id":1}`),
		TargetID: storeIDA, OK: true, Time: t0,
	}); err != nil {
		t.Fatal(err)
	}
	// An unresolved request (no identity) for the same-suffix id changes nothing.
	evalB := json.RawMessage(`{"jsonrpc":"2.0","method":"eval","params":{"tab":"` + storeIDB + `","script":"1"},"id":2}`)
	for i := 0; i < 20; i++ {
		if err := s.AppendRPC(LogRecord{Action: protocol.MethodEval, Body: evalB, OK: false, Error: "unknown tab id", Time: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, rpcLogFile+".1")); err != nil {
		t.Fatalf("expected a rotation: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// The rotation snapshot names the managed tab by complete id.
	raw, err := os.ReadFile(filepath.Join(dir, rpcLogFile))
	if err != nil {
		t.Fatal(err)
	}
	first, _, _ := bytes.Cut(raw, []byte("\n"))
	var rec map[string]any
	if err := json.Unmarshal(first, &rec); err != nil {
		t.Fatalf("snapshot line %q: %v", first, err)
	}
	if rec["action"] != protocol.MethodTabNew || rec["target_id"] != storeIDA {
		t.Fatalf("snapshot record = %v, want tab_new for %s", rec, storeIDA)
	}
	if _, legacy := rec["tab"]; legacy {
		t.Fatalf("snapshot record carries the legacy tab key: %v", rec)
	}

	s2, err := Open(OpenConfig{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	managed := s2.ReplayManagedTabActivity()
	if len(managed) != 1 || !managed[storeIDA].Equal(t0) {
		t.Fatalf("managed after rotation+reopen = %#v, want %s at %v", managed, storeIDA, t0)
	}
}
