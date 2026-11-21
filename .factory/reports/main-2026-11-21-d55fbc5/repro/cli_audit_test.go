package main

// main@d55fbc5 审计复现（只读 overlay）。断言正确行为；FAIL 即缺陷证据。

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type auditDaemon struct {
	mu         sync.Mutex
	evalResult string
	scripts    []string
}

func newAuditDaemon(t *testing.T, evalResult string) (*httptest.Server, *auditDaemon) {
	st := &auditDaemon{evalResult: evalResult}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req struct {
			Method string `json:"method"`
			Params struct {
				Script string `json:"script"`
			} `json:"params"`
		}
		_ = json.Unmarshal(b, &req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "eval":
			st.mu.Lock()
			st.scripts = append(st.scripts, req.Params.Script)
			st.mu.Unlock()
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tab":"t1","seq":1,"result":` + st.evalResult + `}}`))
		case "screenshot":
			jpeg := base64.StdEncoding.EncodeToString([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0x10, 'J', 'F', 'I', 'F'})
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tab":"t1","seq":1,"data":"` + jpeg + `","mime":"image/jpeg"}}`))
		default:
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tab":"t1","seq":1}}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, st
}

func auditAdapterFile(t *testing.T) string {
	p := filepath.Join(t.TempDir(), "demo.js")
	src := `/* @meta {"name":"demo","args":{"query":{"required":true}}} */
async function(args) { return { q: args.query, count: args.count }; }
`
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// README / skills：bb-browser run ./x.js "pos" --name value。cobra 先把 --name 当未知 flag 拒绝。
func TestAudit_RunAcceptsDocumentedNamedArgs(t *testing.T) {
	for _, form := range [][]string{{"hello", "--count", "20"}, {"hello", "--count=20"}} {
		srv, st := newAuditDaemon(t, `{"q":"hello"}`)
		root := newRootCmd()
		root.SetOut(io.Discard)
		root.SetErr(io.Discard)
		root.SetArgs(append([]string{"--url", srv.URL, "--tab", "t1", "run", auditAdapterFile(t)}, form...))
		err := root.Execute()
		if err != nil {
			t.Errorf("BUG: documented form `run x.js %s` rejected: %v", strings.Join(form, " "), err)
			continue
		}
		st.mu.Lock()
		ok := len(st.scripts) == 1 && strings.Contains(st.scripts[0], `"count":"20"`)
		st.mu.Unlock()
		if !ok {
			t.Errorf("named arg not forwarded to adapter for %v", form)
		}
	}
}

// adapter 返回 { error } 时 CLI 只在 stderr 打印 [error]，Execute 返回 nil → 进程退出码 0。
func TestAudit_RunAdapterErrorMustFailTheCommand(t *testing.T) {
	srv, _ := newAuditDaemon(t, `{"error":"HTTP 401","hint":"please log in"}`)
	root := newRootCmd()
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"--url", srv.URL, "--tab", "t1", "run", auditAdapterFile(t), "hello"})
	if err := root.Execute(); err == nil {
		t.Fatal("BUG: adapter returned {error:\"HTTP 401\"} but the command succeeded (main would exit 0)")
	}
}

// screenshot --format jpeg 未给路径时，JPEG 数据被写进 screenshot-<ts>.png。
func TestAudit_ScreenshotJPEGDefaultNameMustNotBePNG(t *testing.T) {
	srv, _ := newAuditDaemon(t, `null`)
	dir := t.TempDir()
	t.Chdir(dir)
	root := newRootCmd()
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"--url", srv.URL, "--tab", "t1", "screenshot", "--format", "jpeg"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("expected one output file, got %d", len(entries))
	}
	name := entries[0].Name()
	raw, _ := os.ReadFile(filepath.Join(dir, name))
	isJPEG := len(raw) > 2 && raw[0] == 0xFF && raw[1] == 0xD8
	t.Logf("wrote %s (jpeg magic=%v)", name, isJPEG)
	if isJPEG && strings.HasSuffix(name, ".png") {
		t.Fatalf("BUG: JPEG screenshot saved under a .png name: %s", name)
	}
}
