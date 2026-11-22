package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/yiplee/go-bb-browser/pkg/protocol"
)

// Complete CDP target ids sharing a suffix: "1234" used to be a valid alias.
const (
	cliTabA = "AAAA0000000000000000000000AB1234"
	cliTabB = "BBBB0000000000000000000000AB1234"
)

type cliRPC struct {
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}

// cliDaemon is a strict stand-in for bb-daemon: it knows complete tab ids only
// and answers anything else with "unknown tab id", like the real daemon.
type cliDaemon struct {
	mu    sync.Mutex
	tabs  []string
	focus string
	next  string
	calls []cliRPC
}

func (d *cliDaemon) requests() []cliRPC {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]cliRPC(nil), d.calls...)
}

func (d *cliDaemon) serve(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req cliRPC
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		d.calls = append(d.calls, req)
		result := map[string]any{"seq": len(d.calls)}
		switch req.Method {
		case protocol.MethodTabList:
			items := []map[string]string{}
			for _, tab := range d.tabs {
				items = append(items, map[string]string{"tab": tab, "title": "t", "url": "https://example.test/"})
			}
			result["tabs"], result["tab"], result["focus"] = items, d.focus, d.focus
		case protocol.MethodTabNew:
			d.tabs = append(d.tabs, d.next)
			result["tab"] = d.next
		default:
			tab, _ := req.Params["tab"].(string)
			known := false
			for _, have := range d.tabs {
				known = known || have == tab
			}
			if !known {
				_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"unknown tab id"}}`)
				return
			}
			result["tab"] = tab
			if req.Method == protocol.MethodEval {
				result["result"] = json.RawMessage(`1`)
			}
		}
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
		_, _ = w.Write(raw)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func runCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	t.Setenv("BB_BROWSER_API_TOKEN", "")
	var err error
	out := captureStdout(t, func() {
		root := newRootCmd()
		root.SetArgs(args)
		root.SilenceErrors = true
		root.SilenceUsage = true
		err = root.Execute()
	})
	return out, err
}

func TestCLIPrintsAndSendsCompleteTabID(t *testing.T) {
	d := &cliDaemon{tabs: []string{cliTabB}, focus: cliTabB, next: cliTabA}
	url := d.serve(t)

	out, err := runCLI(t, "--url", url, "open", "https://example.test/")
	if err != nil || out != cliTabA+"\n" {
		t.Fatalf("open printed %q (err %v), want the complete tab id %q", out, err, cliTabA)
	}
	if out, err := runCLI(t, "--url", url, "tab", "list"); err != nil || !strings.Contains(out, cliTabA) || !strings.Contains(out, cliTabB) {
		t.Fatalf("tab list = %q (err %v), want both complete ids", out, err)
	}

	script := filepath.Join(t.TempDir(), "adapter.js")
	if err := os.WriteFile(script, []byte("async function(args) { return 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Every tab-scoped command forwards the id it was given, byte for byte.
	for _, tc := range []struct {
		method string
		args   []string
	}{
		{protocol.MethodEval, []string{"--tab", cliTabA, "eval", "1"}},
		{protocol.MethodEval, []string{"eval", "1", "--tab=" + cliTabA}},
		{protocol.MethodGoto, []string{"--tab", cliTabA, "goto", "https://example.test/"}},
		{protocol.MethodGoto, []string{"--tab", cliTabA, "open", "--current", "https://example.test/"}},
		{protocol.MethodReload, []string{"--tab", cliTabA, "reload"}},
		{protocol.MethodSnapshot, []string{"--tab", cliTabA, "snapshot"}},
		{protocol.MethodClick, []string{"--tab", cliTabA, "click", "#a"}},
		{protocol.MethodFill, []string{"--tab", cliTabA, "fill", "#a", "x"}},
		{protocol.MethodNetwork, []string{"--tab", cliTabA, "network", "requests"}},
		{protocol.MethodNetworkClear, []string{"--tab", cliTabA, "network", "clear"}},
		{protocol.MethodNetworkRoute, []string{"--tab", cliTabA, "network", "route", "*", "--abort"}},
		{protocol.MethodNetworkUnroute, []string{"--tab", cliTabA, "network", "unroute"}},
		{protocol.MethodConsole, []string{"--tab", cliTabA, "console"}},
		{protocol.MethodErrors, []string{"--tab", cliTabA, "errors"}},
		{protocol.MethodEval, []string{"run", script, "--tab", cliTabA}},
		{protocol.MethodTabSelect, []string{"tab", "select", "--id", cliTabA}},
		{protocol.MethodTabClose, []string{"tab", "close", "--id", cliTabA}},
	} {
		before := len(d.requests())
		if _, err := runCLI(t, append([]string{"--url", url}, tc.args...)...); err != nil {
			t.Errorf("%v: %v", tc.args, err)
			continue
		}
		reqs := d.requests()[before:]
		if len(reqs) != 1 || reqs[0].Method != tc.method || reqs[0].Params["tab"] != cliTabA {
			t.Errorf("%v sent %+v, want one %s for tab %q (no tab_list lookup)", tc.args, reqs, tc.method, cliTabA)
		}
	}
}

// An explicit id that is not a complete known id fails; the CLI never swaps in
// the focused tab (B here, which shares the suffix) or any other tab.
func TestCLIExplicitTabNeverFallsBack(t *testing.T) {
	d := &cliDaemon{tabs: []string{cliTabA, cliTabB}, focus: cliTabB}
	url := d.serve(t)

	for _, tab := range []string{
		"1234", "ab1234", cliTabA[1:], cliTabA[:len(cliTabA)-1],
		strings.ToLower(cliTabA), " " + cliTabA, cliTabA + " ", "0:" + cliTabA,
	} {
		for _, args := range [][]string{
			{"--tab", tab, "eval", "1"},
			{"--tab", tab, "close"},
			{"tab", "close", "--id", tab},
			{"tab", "select", "--id", tab},
		} {
			before := len(d.requests())
			_, err := runCLI(t, append([]string{"--url", url}, args...)...)
			if err == nil || !strings.Contains(err.Error(), "unknown tab id") {
				t.Errorf("%q: err = %v, want unknown tab id", args, err)
			}
			for _, req := range d.requests()[before:] {
				if req.Method == protocol.MethodTabList || req.Params["tab"] != tab {
					t.Errorf("%q: sent %+v; the id must be forwarded verbatim with no focus lookup", args, req)
				}
			}
		}
	}

	// An explicitly empty id is rejected locally instead of meaning "focused tab".
	for _, args := range [][]string{
		{"--tab", "", "eval", "1"},
		{"--tab", "  ", "close"},
		{"eval", "1", "--tab="},
		{"run", "missing.js", "--tab", ""},
		{"tab", "close", "--id", ""},
		{"tab", "select", "--id", " "},
	} {
		before := len(d.requests())
		if _, err := runCLI(t, append([]string{"--url", url}, args...)...); err == nil || !strings.Contains(err.Error(), "empty") {
			t.Errorf("%q: err = %v, want an empty-id error", args, err)
		}
		if got := d.requests()[before:]; len(got) != 0 {
			t.Errorf("%q still called the daemon: %+v", args, got)
		}
	}

	// Without --tab the focused tab is still used, by its complete id.
	before := len(d.requests())
	if _, err := runCLI(t, "--url", url, "eval", "1"); err != nil {
		t.Fatal(err)
	}
	reqs := d.requests()[before:]
	if len(reqs) != 2 || reqs[0].Method != protocol.MethodTabList || reqs[1].Method != protocol.MethodEval || reqs[1].Params["tab"] != cliTabB {
		t.Fatalf("eval without --tab sent %+v, want tab_list then eval on the focused %q", reqs, cliTabB)
	}
}
