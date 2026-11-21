package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/yiplee/go-bb-browser/pkg/protocol"
)

// runDaemon is a fake bb-daemon that records what `bb-browser run` sends.
type runDaemon struct {
	URL string

	mu       sync.Mutex
	requests int
	auth     []string
	evalTabs []string
	scripts  []string
}

const runEvalBody = `{"jsonrpc":"2.0","id":1,"result":{"tab":"t1","seq":7,"result":{"ok":true}}}`

func newRunDaemon(t *testing.T) *runDaemon {
	t.Helper()
	d := &runDaemon{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
			Params struct {
				Tab    string `json:"tab"`
				Script string `json:"script"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		d.mu.Lock()
		d.requests++
		d.auth = append(d.auth, r.Header.Get("Authorization"))
		if req.Method == protocol.MethodEval {
			d.evalTabs = append(d.evalTabs, req.Params.Tab)
			d.scripts = append(d.scripts, req.Params.Script)
		}
		d.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case protocol.MethodEval:
			_, _ = io.WriteString(w, runEvalBody)
		case protocol.MethodTabList:
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"seq":1,"tab":"focus1","focus":"focus1","tabs":[]}}`)
		default:
			t.Errorf("unexpected method %q", req.Method)
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"unexpected"}}`)
		}
	}))
	t.Cleanup(srv.Close)
	d.URL = srv.URL
	return d
}

func (d *runDaemon) requestCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.requests
}

// evalCall returns the tab, Authorization header and adapter args object of the single eval request.
func (d *runDaemon) evalCall(t *testing.T) (tab, auth string, args map[string]string) {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.scripts) != 1 {
		t.Fatalf("eval requests = %d, want 1", len(d.scripts))
	}
	script := d.scripts[0]
	const open, closing = "\n})( ", " )"
	i := strings.LastIndex(script, open)
	if i < 0 || !strings.HasSuffix(script, closing) {
		t.Fatalf("eval script has no args object: %q", script)
	}
	raw := strings.TrimSuffix(script[i+len(open):], closing)
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		t.Fatalf("args JSON %q: %v", raw, err)
	}
	return d.evalTabs[0], d.auth[len(d.auth)-1], args
}

func runAdapterFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "demo.js")
	src := `/* @meta {"name":"demo","args":{"query":{"required":true}}} */
async function(args) { return { q: args.query, count: args.count }; }
`
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// execCLI runs the root command through SetArgs (never os.Args) and returns stdout,
// Cobra's own output (help/usage), and the Execute error.
func execCLI(t *testing.T, args ...string) (stdout, cobraOut string, err error) {
	t.Helper()
	var buf bytes.Buffer
	stdout = captureStdout(t, func() {
		root := newRootCmd()
		root.SetOut(&buf)
		root.SetErr(&buf)
		root.SetArgs(args)
		root.SilenceErrors = true
		root.SilenceUsage = true
		err = root.Execute()
	})
	return stdout, buf.String(), err
}

// expand replaces the URL and SCRIPT placeholders in a test argv.
func expand(args []string, url, script string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		a = strings.ReplaceAll(a, "URL", url)
		out[i] = strings.ReplaceAll(a, "SCRIPT", script)
	}
	return out
}

func TestRunArguments(t *testing.T) {
	for _, tc := range []struct {
		name     string
		env      string // BB_BROWSER_API_TOKEN
		args     []string
		wantArgs map[string]string
		wantTab  string
		wantAuth string
		wantJSON bool
	}{
		{
			name:     "named with space",
			args:     []string{"--url", "URL", "--tab", "t1", "run", "SCRIPT", "hello", "--count", "20"},
			wantArgs: map[string]string{"query": "hello", "arg1": "hello", "count": "20"},
			wantTab:  "t1",
		},
		{
			name:     "named with equals",
			args:     []string{"--url", "URL", "--tab", "t1", "run", "SCRIPT", "hello", "--count=20"},
			wantArgs: map[string]string{"query": "hello", "arg1": "hello", "count": "20"},
			wantTab:  "t1",
		},
		{
			name:     "positional only keep order",
			args:     []string{"--url", "URL", "--tab", "t1", "run", "SCRIPT", "b", "a", "c"},
			wantArgs: map[string]string{"query": "b", "arg1": "b", "arg2": "a", "arg3": "c"},
			wantTab:  "t1",
		},
		{
			name:     "named before positional",
			args:     []string{"--url", "URL", "--tab", "t1", "run", "SCRIPT", "--count", "20", "hello"},
			wantArgs: map[string]string{"query": "hello", "arg1": "hello", "count": "20"},
			wantTab:  "t1",
		},
		{
			name:     "no adapter args",
			args:     []string{"--url", "URL", "run", "SCRIPT"},
			wantArgs: map[string]string{},
			wantTab:  "focus1",
		},
		{
			name:     "globals after run",
			args:     []string{"run", "SCRIPT", "hello", "--url", "URL", "--tab", "t9", "--json", "--api-token", "tok", "--count", "20"},
			wantArgs: map[string]string{"query": "hello", "arg1": "hello", "count": "20"},
			wantTab:  "t9",
			wantAuth: "Bearer tok",
			wantJSON: true,
		},
		{
			name:     "globals interleaved and equals forms",
			args:     []string{"--url=URL", "run", "--tab=t2", "SCRIPT", "--json", "hello", "--count", "20", "--api-token=tok2", "world"},
			wantArgs: map[string]string{"query": "hello", "arg1": "hello", "arg2": "world", "count": "20"},
			wantTab:  "t2",
			wantAuth: "Bearer tok2",
			wantJSON: true,
		},
		{
			name:     "json before script takes no value",
			args:     []string{"--url", "URL", "--tab", "t1", "run", "--json", "SCRIPT", "hello"},
			wantArgs: map[string]string{"query": "hello", "arg1": "hello"},
			wantTab:  "t1",
			wantJSON: true,
		},
		{
			name:     "json=false after run overrides earlier json",
			args:     []string{"--url", "URL", "--json", "run", "SCRIPT", "hello", "--json=false"},
			wantArgs: map[string]string{"query": "hello", "arg1": "hello"},
			wantTab:  "focus1",
		},
		{
			name:     "token from environment",
			env:      "env-secret",
			args:     []string{"--url", "URL", "--tab", "t1", "run", "SCRIPT", "hello", "--count", "20"},
			wantArgs: map[string]string{"query": "hello", "arg1": "hello", "count": "20"},
			wantTab:  "t1",
			wantAuth: "Bearer env-secret",
		},
		{
			name:     "token flag after run overrides environment",
			env:      "env-secret",
			args:     []string{"--url", "URL", "--tab", "t1", "run", "SCRIPT", "hello", "--api-token", "flag-secret"},
			wantArgs: map[string]string{"query": "hello", "arg1": "hello"},
			wantTab:  "t1",
			wantAuth: "Bearer flag-secret",
		},
		{
			name:     "empty token flag disables environment",
			env:      "env-secret",
			args:     []string{"--url", "URL", "--tab", "t1", "run", "SCRIPT", "hello", "--api-token="},
			wantArgs: map[string]string{"query": "hello", "arg1": "hello"},
			wantTab:  "t1",
		},
		{
			name: "separator passes reserved names to the adapter",
			env:  "env-secret",
			args: []string{"--url", "URL", "--tab", "t1", "run", "SCRIPT", "--", "hello",
				"--tab", "x", "--json", "true", "--url=http://adapter.invalid", "--api-token", "z", "--help", "me"},
			wantArgs: map[string]string{"query": "hello", "arg1": "hello",
				"tab": "x", "json": "true", "url": "http://adapter.invalid", "api-token": "z", "help": "me"},
			wantTab:  "t1",
			wantAuth: "Bearer env-secret",
		},
		{
			name:     "script after separator",
			args:     []string{"--url", "URL", "--tab", "t1", "run", "--", "SCRIPT", "hello", "--count", "20"},
			wantArgs: map[string]string{"query": "hello", "arg1": "hello", "count": "20"},
			wantTab:  "t1",
		},
		{
			name: "values are passed through verbatim",
			args: []string{"--url", "URL", "--tab", "t1", "run", "SCRIPT", "-5", "", "--offset", "-3", "--q=a=b",
				"--empty=", "--blank", "", "--pad", " x ", "--n", "1", "--n=2", "--dash=--x"},
			wantArgs: map[string]string{"query": "-5", "arg1": "-5", "arg2": "", "offset": "-3", "q": "a=b",
				"empty": "", "blank": "", "pad": " x ", "n": "2", "dash": "--x"},
			wantTab: "t1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BB_BROWSER_API_TOKEN", tc.env)
			t.Setenv("BB_BROWSER_URL", "http://127.0.0.1:1") // --url must win wherever it appears
			d := newRunDaemon(t)
			stdout, _, err := execCLI(t, expand(tc.args, d.URL, runAdapterFile(t))...)
			if err != nil {
				t.Fatalf("run failed: %v", err)
			}
			tab, auth, args := d.evalCall(t)
			if !reflect.DeepEqual(args, tc.wantArgs) {
				t.Errorf("adapter args = %v, want %v", args, tc.wantArgs)
			}
			if tab != tc.wantTab {
				t.Errorf("eval tab = %q, want %q", tab, tc.wantTab)
			}
			if auth != tc.wantAuth {
				t.Errorf("Authorization = %q, want %q", auth, tc.wantAuth)
			}
			if isJSON := stdout == runEvalBody+"\n"; isJSON != tc.wantJSON {
				t.Errorf("raw JSON output = %v, want %v; stdout:\n%s", isJSON, tc.wantJSON, stdout)
			}
			if !tc.wantJSON && strings.TrimSpace(stdout) != "{\n  \"ok\": true\n}" {
				t.Errorf("stdout = %q, want pretty adapter result", stdout)
			}
		})
	}
}

func TestRunArgumentErrorsBeforeRequest(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"named flag without value at end", []string{"--url", "URL", "run", "SCRIPT", "hello", "--count"}, "flag --count needs a value"},
		{"named flag followed by another flag", []string{"--url", "URL", "run", "SCRIPT", "--count", "--limit", "5"}, "flag --count needs a value"},
		{"named flag followed by global flag", []string{"--url", "URL", "run", "SCRIPT", "--count", "--json"}, "flag --count needs a value"},
		{"named flag without value after separator", []string{"--url", "URL", "run", "SCRIPT", "--", "--tab"}, "flag --tab needs a value"},
		{"empty flag name", []string{"--url", "URL", "run", "SCRIPT", "--=x"}, "flag needs a name"},
		{"missing script", []string{"--url", "URL", "run"}, "script"},
		{"missing script with only globals", []string{"--url", "URL", "run", "--tab", "t1", "--json"}, "script"},
		{"missing script after separator", []string{"--url", "URL", "run", "--"}, "script"},
		{"invalid bool global", []string{"--url", "URL", "run", "SCRIPT", "hello", "--json=maybe"}, `invalid argument "maybe" for "--json" flag`},
		{"global missing value", []string{"--url", "URL", "run", "SCRIPT", "hello", "--tab"}, "flag needs an argument: --tab"},
		{"unreadable script", []string{"--url", "URL", "run", "SCRIPT.missing", "--count", "20"}, "SCRIPT.missing"},
		{"unknown flag on other subcommand", []string{"--url", "URL", "tab", "list", "--count", "20"}, "unknown flag: --count"},
		{"unknown flag on eval", []string{"--url", "URL", "--tab", "t1", "eval", "1", "--count=20"}, "unknown flag: --count"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BB_BROWSER_API_TOKEN", "")
			d := newRunDaemon(t)
			script := runAdapterFile(t)
			_, _, err := execCLI(t, expand(tc.args, d.URL, script)...)
			want := strings.ReplaceAll(tc.wantErr, "SCRIPT", script)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %v, want containing %q", err, want)
			}
			if n := d.requestCount(); n != 0 {
				t.Fatalf("daemon received %d requests, want 0", n)
			}
		})
	}
}

func TestRunHelpAndVersion(t *testing.T) {
	t.Setenv("BB_BROWSER_API_TOKEN", "help-secret")
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"run --help", []string{"--url", "URL", "run", "--help"}, []string{"run SCRIPT.js", "--name value", "--api-token", "--tab"}},
		{"run -h", []string{"--url", "URL", "run", "-h"}, []string{"run SCRIPT.js", "--name value"}},
		{"run script --help", []string{"--url", "URL", "run", "SCRIPT", "hello", "--help"}, []string{"run SCRIPT.js", "--name value"}},
		{"help run", []string{"--url", "URL", "help", "run"}, []string{"run SCRIPT.js", "--name value", "--api-token", "--tab"}},
		{"root help lists run", []string{"--help"}, []string{"run ", "--api-token"}},
		{"root --version", []string{"--version"}, []string{"bb-browser dev"}},
		{"root -v", []string{"-v"}, []string{"bb-browser dev"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newRunDaemon(t)
			stdout, cobraOut, err := execCLI(t, expand(tc.args, d.URL, runAdapterFile(t))...)
			if err != nil {
				t.Fatal(err)
			}
			out := stdout + cobraOut
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("output missing %q:\n%s", w, out)
				}
			}
			if strings.Contains(out, "help-secret") {
				t.Error("help exposed the API token")
			}
			if n := d.requestCount(); n != 0 {
				t.Fatalf("daemon received %d requests, want 0", n)
			}
		})
	}
}

func TestParseRunArgs(t *testing.T) {
	kind := func(name string) (bool, string) {
		switch name {
		case "json", "help":
			return true, "true"
		case "url", "tab", "api-token":
			return true, ""
		}
		return false, ""
	}
	g := func(kv ...string) []runGlobal {
		var out []runGlobal
		for i := 0; i < len(kv); i += 2 {
			out = append(out, runGlobal{kv[i], kv[i+1]})
		}
		return out
	}
	for _, tc := range []struct {
		name    string
		tokens  []string
		want    runArgs
		wantErr string
	}{
		{name: "empty", want: runArgs{}},
		{name: "script only", tokens: []string{"x.js"}, want: runArgs{script: "x.js"}},
		{
			name:   "positional order",
			tokens: []string{"x.js", "b", "a", "b"},
			want:   runArgs{script: "x.js", positional: []string{"b", "a", "b"}},
		},
		{
			name:   "named forms",
			tokens: []string{"x.js", "hello", "--count", "20", "--sort=new"},
			want:   runArgs{script: "x.js", positional: []string{"hello"}, named: map[string]string{"count": "20", "sort": "new"}},
		},
		{
			name:   "named before script",
			tokens: []string{"--count", "20", "x.js", "hello"},
			want:   runArgs{script: "x.js", positional: []string{"hello"}, named: map[string]string{"count": "20"}},
		},
		{
			name:   "globals anywhere keep order",
			tokens: []string{"--url", "u1", "x.js", "--json", "a", "--tab=t1", "--url=u2", "--api-token", "", "--json=false"},
			want: runArgs{script: "x.js", positional: []string{"a"},
				globals: g("url", "u1", "json", "true", "tab", "t1", "url", "u2", "api-token", "", "json", "false")},
		},
		{
			name:   "bool global does not consume next token",
			tokens: []string{"--json", "x.js", "--json", "true"},
			want:   runArgs{script: "x.js", positional: []string{"true"}, globals: g("json", "true", "json", "true")},
		},
		{
			name:   "value global consumes any next token",
			tokens: []string{"x.js", "--tab", "--json", "--url", "--"},
			want:   runArgs{script: "x.js", globals: g("tab", "--json", "url", "--")},
		},
		{
			name:   "help forms",
			tokens: []string{"-h", "x.js", "--help"},
			want:   runArgs{script: "x.js", globals: g("help", "true", "help", "true")},
		},
		{
			name:   "separator releases reserved names",
			tokens: []string{"--tab", "t1", "x.js", "--", "a", "--tab", "x", "--json=1", "--help", "me", "-h"},
			want: runArgs{script: "x.js", positional: []string{"a", "-h"},
				named: map[string]string{"tab": "x", "json": "1", "help": "me"}, globals: g("tab", "t1")},
		},
		{
			name:   "script after separator",
			tokens: []string{"--", "x.js", "a"},
			want:   runArgs{script: "x.js", positional: []string{"a"}},
		},
		{
			name:   "verbatim values",
			tokens: []string{"x.js", "-5", "", "-", " a ", "--offset", "-3", "--q=a=b", "--empty=", "--blank", "", "--pad", " x ", "--dash=--x"},
			want: runArgs{script: "x.js", positional: []string{"-5", "", "-", " a "},
				named: map[string]string{"offset": "-3", "q": "a=b", "empty": "", "blank": "", "pad": " x ", "dash": "--x"}},
		},
		{
			name:   "repeated name keeps last",
			tokens: []string{"x.js", "--n", "1", "--n=2", "--n", "3"},
			want:   runArgs{script: "x.js", named: map[string]string{"n": "3"}},
		},
		{name: "named missing value at end", tokens: []string{"x.js", "--count"}, wantErr: "flag --count needs a value"},
		{name: "named followed by flag", tokens: []string{"x.js", "--count", "--n", "1"}, wantErr: "flag --count needs a value"},
		{name: "named followed by separator", tokens: []string{"x.js", "--count", "--", "1"}, wantErr: "flag --count needs a value"},
		{name: "reserved name missing value after separator", tokens: []string{"x.js", "--", "--json"}, wantErr: "flag --json needs a value"},
		{name: "global missing value", tokens: []string{"x.js", "--url"}, wantErr: "flag needs an argument: --url"},
		{name: "empty name with value", tokens: []string{"x.js", "--=x"}, wantErr: "flag needs a name: --=x"},
		{name: "second separator", tokens: []string{"x.js", "--", "a", "--"}, wantErr: "flag needs a name: --"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRunArgs(tc.tokens, kind)
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.want.named == nil {
				tc.want.named = map[string]string{}
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseRunArgs(%q)\n got %+v\nwant %+v", tc.tokens, got, tc.want)
			}
		})
	}
}
