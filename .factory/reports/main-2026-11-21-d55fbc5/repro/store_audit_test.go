package store

// main@d55fbc5 审计复现（只读 overlay）。断言正确行为；FAIL 即缺陷证据。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func auditRec(action, tab string, params string) LogRecord {
	return LogRecord{
		Action: action,
		Body:   json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","method":%q,"params":%s,"id":1}`, action, params)),
		Tab:    tab,
		OK:     true,
		Time:   time.Now().UTC(),
	}
}

// 日志尾部若不是以换行结束（进程在写完 JSON 但未写入 '\n' 时崩溃 / 写入被截断在换行前），
// 下一次追加会与上一条粘成一行；再下一次启动 replay 把这一行及其后全部记录截断丢弃。
func TestAudit_AppendAfterUnterminatedTailMustNotLoseRecords(t *testing.T) {
	dir := t.TempDir()
	first, _ := json.Marshal(auditRec("tab_new", "aaaa", `{}`))
	if err := os.WriteFile(filepath.Join(dir, rpcLogFile), first, 0o644); err != nil { // 无结尾换行
		t.Fatal(err)
	}
	s, err := Open(OpenConfig{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.ReplayManagedTabActivity()["aaaa"]; !ok {
		t.Fatal("precondition: first record should replay")
	}
	if err := s.AppendRPC(auditRec("tab_new", "bbbb", `{}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(OpenConfig{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got := s2.ReplayManagedTabActivity()
	raw, _ := os.ReadFile(filepath.Join(dir, rpcLogFile))
	t.Logf("managed after restart = %v; rpc.jsonl size after replay = %d bytes", got, len(raw))
	for _, tab := range []string{"aaaa", "bbbb"} {
		if _, ok := got[tab]; !ok {
			t.Errorf("BUG: managed tab %q lost after restart (records glued onto an unterminated line, then truncated by replay)", tab)
		}
	}
}

// --state-dir - 内存模式下每条 RPC 记录都被永久追加到 memLogs，且生产代码从不读取它。
func TestAudit_InMemoryModeMustNotRetainEveryRecord(t *testing.T) {
	s, err := Open(OpenConfig{StateDir: "-"})
	if err != nil {
		t.Fatal(err)
	}
	const n = 5000
	for i := 0; i < n; i++ {
		if err := s.AppendRPC(auditRec("tab_list", "", `{}`)); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(s.memLogs); got > 1024 {
		t.Fatalf("BUG: in-memory store retains %d of %d records (unbounded growth; nothing reads memLogs)", got, n)
	}
}

// rpc.jsonl 含完整请求体（fill 文本、fetch headers 等），却以 0644 创建、目录 0755。
func TestAudit_RPCLogMustNotBeWorldReadable(t *testing.T) {
	old := setUmask(0o022) // 常见默认 umask
	defer setUmask(old)
	dir := filepath.Join(t.TempDir(), "state")
	s, err := Open(OpenConfig{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.AppendRPC(auditRec("fill", "aaaa", `{"tab":"aaaa","selector":"#password","text":"hunter2-secret"}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendRPC(auditRec("fetch", "aaaa", `{"tab":"aaaa","url":"https://api.example/x","headers":"{\"Authorization\":\"Bearer sk-live-123\"}"}`)); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, rpcLogFile))
	if !strings.Contains(string(raw), "hunter2-secret") || !strings.Contains(string(raw), "sk-live-123") {
		t.Fatalf("precondition: expected request bodies in log, got %s", raw)
	}
	fi, _ := os.Stat(filepath.Join(dir, rpcLogFile))
	di, _ := os.Stat(dir)
	t.Logf("rpc.jsonl mode=%o dir mode=%o; log contains fill text and Authorization header", fi.Mode().Perm(), di.Mode().Perm())
	if fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("BUG: rpc.jsonl holding credentials is created with mode %o (group/other readable)", fi.Mode().Perm())
	}
	if di.Mode().Perm()&0o077 != 0 {
		t.Errorf("BUG: state dir created with mode %o (group/other accessible)", di.Mode().Perm())
	}
}
