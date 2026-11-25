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

	"github.com/coinpost/go-bb-browser/pkg/protocol"
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
		{
			name:   "help bool values keep order",
			tokens: []string{"--help=false", "x.js", "-h=true", "--help=0", "-h=", "--help=maybe"},
			want:   runArgs{script: "x.js", globals: g("help", "false", "help", "true", "help", "0", "help", "", "help", "maybe")},
		},
		{
			name:   "bool help does not consume next token",
			tokens: []string{"x.js", "--help", "false", "-h", "true"},
			want:   runArgs{script: "x.js", positional: []string{"false", "true"}, globals: g("help", "true", "help", "true")},
		},
		{
			name:   "double dash script after separator",
			tokens: []string{"--tab", "t1", "--", "--foo.js", "a", "--count", "20", "--help=true", "-h=true"},
			want: runArgs{script: "--foo.js", positional: []string{"a", "-h=true"},
				named: map[string]string{"count": "20", "help": "true"}, globals: g("tab", "t1")},
		},
		{
			name:   "script after separator is verbatim",
			tokens: []string{"--", "--help=true", "--url", "u"},
			want:   runArgs{script: "--help=true", named: map[string]string{"url": "u"}},
		},
		{name: "reserved name as script after separator", tokens: []string{"--", "--json"}, want: runArgs{script: "--json"}},
		{name: "short help as script after separator", tokens: []string{"--", "-h"}, want: runArgs{script: "-h"}},
		{name: "separator token as script after separator", tokens: []string{"--", "--"}, want: runArgs{script: "--"}},
		{
			name:   "empty script after separator stays the script",
			tokens: []string{"--", "", "x.js"},
			want:   runArgs{script: "", positional: []string{"x.js"}},
		},
		{
			name:    "separator after script does not relax positionals",
			tokens:  []string{"x.js", "--", "--foo.js"},
			wantErr: "flag --foo.js needs a value",
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

// TestRunHelpBool: before the first "--" the help flag follows pflag's bool rules. Whenever it
// ends up true the command prints help and sends nothing, even with no or a missing script.
func TestRunHelpBool(t *testing.T) {
	t.Setenv("BB_BROWSER_API_TOKEN", "help-secret")
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"--help=true after script", []string{"--url", "URL", "run", "SCRIPT", "--help=true"}},
		{"--help=true before script", []string{"--url", "URL", "run", "--help=true", "SCRIPT", "hello"}},
		{"--help=1", []string{"--url", "URL", "run", "SCRIPT", "hello", "--help=1"}},
		{"--help=T", []string{"--url", "URL", "run", "SCRIPT", "--help=T"}},
		{"-h=true after script", []string{"--url", "URL", "run", "SCRIPT", "hello", "-h=true"}},
		{"-h=true before script", []string{"--url", "URL", "run", "-h=true", "SCRIPT"}},
		{"-h=1", []string{"--url", "URL", "run", "SCRIPT", "-h=1"}},
		{"last value true wins", []string{"--url", "URL", "run", "SCRIPT", "--help=false", "-h=false", "--help=true"}},
		{"last value true wins short", []string{"--url", "URL", "run", "--help=false", "SCRIPT", "-h"}},
		{"help without script", []string{"--url", "URL", "run", "--help=true"}},
		{"short help without script", []string{"--url", "URL", "run", "-h=true"}},
		{"help with missing script file", []string{"--url", "URL", "run", "SCRIPT.missing", "--help=true"}},
		{"help before separator and double dash script", []string{"--url", "URL", "run", "-h=true", "--", "--foo.js"}},
		{"help with adapter flag missing value later is still an error-free help", []string{"--url", "URL", "run", "SCRIPT", "--help=true", "--count", "20"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newRunDaemon(t)
			stdout, cobraOut, err := execCLI(t, expand(tc.args, d.URL, runAdapterFile(t))...)
			if err != nil {
				t.Fatal(err)
			}
			out := stdout + cobraOut
			for _, w := range []string{"run SCRIPT.js", "--name value", "--api-token", "--tab"} {
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

// TestRunHelpFalseExecutes: an explicit false is not a help request — the adapter runs, and
// neither the flag nor its value reaches the adapter arguments.
func TestRunHelpFalseExecutes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		wantArgs map[string]string
	}{
		{"--help=false after script", []string{"--url", "URL", "--tab", "t1", "run", "SCRIPT", "hello", "--help=false"},
			map[string]string{"query": "hello", "arg1": "hello"}},
		{"--help=false before script", []string{"--url", "URL", "--tab", "t1", "run", "--help=false", "SCRIPT", "hello", "--count", "20"},
			map[string]string{"query": "hello", "arg1": "hello", "count": "20"}},
		{"--help=0", []string{"--url", "URL", "--tab", "t1", "run", "SCRIPT", "--help=0"}, map[string]string{}},
		{"-h=false after script", []string{"--url", "URL", "--tab", "t1", "run", "SCRIPT", "hello", "-h=false"},
			map[string]string{"query": "hello", "arg1": "hello"}},
		{"-h=false before script", []string{"--url", "URL", "--tab", "t1", "run", "-h=false", "SCRIPT", "hello"},
			map[string]string{"query": "hello", "arg1": "hello"}},
		{"-h=0", []string{"--url", "URL", "--tab", "t1", "run", "SCRIPT", "-h=0"}, map[string]string{}},
		{"last value false wins", []string{"--url", "URL", "--tab", "t1", "run", "--help", "SCRIPT", "-h", "-h=true", "hello", "--help=false"},
			map[string]string{"query": "hello", "arg1": "hello"}},
		{"last value false wins short", []string{"--url", "URL", "--tab", "t1", "run", "SCRIPT", "--help=true", "-h=false"},
			map[string]string{}},
		{"help names after separator belong to the adapter", []string{"--url", "URL", "--tab", "t1", "run", "SCRIPT", "--", "-h", "--help=true", "-h=true"},
			map[string]string{"query": "-h", "arg1": "-h", "arg2": "-h=true", "help": "true"}},
		{"help names after separator and script after it", []string{"--url", "URL", "--tab", "t1", "run", "--", "SCRIPT", "--help", "me", "-h"},
			map[string]string{"query": "-h", "arg1": "-h", "help": "me"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BB_BROWSER_API_TOKEN", "")
			d := newRunDaemon(t)
			stdout, cobraOut, err := execCLI(t, expand(tc.args, d.URL, runAdapterFile(t))...)
			if err != nil {
				t.Fatalf("run failed: %v", err)
			}
			tab, _, args := d.evalCall(t)
			if tab != "t1" {
				t.Errorf("eval tab = %q, want t1", tab)
			}
			if !reflect.DeepEqual(args, tc.wantArgs) {
				t.Errorf("adapter args = %v, want %v", args, tc.wantArgs)
			}
			if n := d.requestCount(); n != 1 {
				t.Errorf("daemon received %d requests, want 1 (eval)", n)
			}
			if strings.TrimSpace(stdout) != "{\n  \"ok\": true\n}" {
				t.Errorf("stdout = %q, want pretty adapter result", stdout)
			}
			if strings.Contains(stdout+cobraOut, "run SCRIPT.js") {
				t.Errorf("help was printed:\n%s%s", stdout, cobraOut)
			}
		})
	}
}

// TestRunHelpInvalidBeforeRequest: an invalid help value, and everything that is still wrong
// once help is explicitly off, fails locally without a request.
func TestRunHelpInvalidBeforeRequest(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"--help=maybe", []string{"--url", "URL", "run", "SCRIPT", "--help=maybe"}, `invalid argument "maybe" for "-h, --help" flag`},
		{"--help= empty", []string{"--url", "URL", "run", "SCRIPT", "--help="}, `invalid argument "" for "-h, --help" flag`},
		{"-h=maybe", []string{"--url", "URL", "run", "SCRIPT", "-h=maybe"}, `invalid argument "maybe" for "-h, --help" flag`},
		{"-h= empty", []string{"--url", "URL", "run", "SCRIPT", "-h="}, `invalid argument "" for "-h, --help" flag`},
		{"invalid value before a valid true", []string{"--url", "URL", "run", "SCRIPT", "--help=maybe", "--help=true"}, `invalid argument "maybe" for "-h, --help" flag`},
		{"invalid value after a valid true", []string{"--url", "URL", "run", "SCRIPT", "-h", "-h=maybe"}, `invalid argument "maybe" for "-h, --help" flag`},
		{"help off without script", []string{"--url", "URL", "run", "--help=false"}, "missing script path"},
		{"short help off without script", []string{"--url", "URL", "run", "-h=false"}, "missing script path"},
		{"help off with blank script", []string{"--url", "URL", "run", "--help=false", "--", " "}, "missing script path"},
		{"help off with missing script file", []string{"--url", "URL", "run", "SCRIPT.missing", "-h=false"}, "SCRIPT.missing"},
		{"help off with adapter flag missing value", []string{"--url", "URL", "run", "SCRIPT", "--help=false", "--count"}, "flag --count needs a value"},
		{"help on with adapter flag missing value", []string{"--url", "URL", "run", "SCRIPT", "--help=true", "--count"}, "flag --count needs a value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BB_BROWSER_API_TOKEN", "")
			d := newRunDaemon(t)
			script := runAdapterFile(t)
			stdout, _, err := execCLI(t, expand(tc.args, d.URL, script)...)
			want := strings.ReplaceAll(tc.wantErr, "SCRIPT", script)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %v, want containing %q", err, want)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
			if n := d.requestCount(); n != 0 {
				t.Fatalf("daemon received %d requests, want 0", n)
			}
		})
	}
}

// TestRunDoubleDashScriptAfterSeparator: the first "--" ends the CLI flags, so the token after
// it is the script path even when the file name itself starts with "--".
func TestRunDoubleDashScriptAfterSeparator(t *testing.T) {
	dir := t.TempDir()
	src := `/* @meta {"name":"dd","args":{"query":{"required":true}}} */
async function(args) { return args; }
`
	for _, name := range []string{"--foo.js", "--help", "--json=true.js", "-h"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)

	for _, tc := range []struct {
		name     string
		args     []string
		wantArgs map[string]string
	}{
		{"script only", []string{"--url", "URL", "--tab", "t1", "run", "--", "--foo.js"}, map[string]string{}},
		{"with adapter arguments", []string{"--url", "URL", "run", "--tab", "t1", "--", "--foo.js", "hello", "--count", "20", "--tab=x"},
			map[string]string{"query": "hello", "arg1": "hello", "count": "20", "tab": "x"}},
		{"adapter flag before the separator, script after it", []string{"--url", "URL", "--tab", "t1", "run", "--foo.js=x", "--", "--foo.js"},
			map[string]string{"foo.js": "x"}},
		{"file named like the help flag", []string{"--url", "URL", "--tab", "t1", "run", "--", "--help", "hello"},
			map[string]string{"query": "hello", "arg1": "hello"}},
		{"file named like a bool flag with value", []string{"--url", "URL", "--tab", "t1", "run", "--", "--json=true.js"}, map[string]string{}},
		{"file named like the short help flag", []string{"--url", "URL", "--tab", "t1", "run", "--", "-h", "--help", "me"},
			map[string]string{"help": "me"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BB_BROWSER_API_TOKEN", "")
			d := newRunDaemon(t)
			stdout, cobraOut, err := execCLI(t, expand(tc.args, d.URL, "")...)
			if err != nil {
				t.Fatalf("run failed: %v", err)
			}
			tab, _, args := d.evalCall(t)
			if tab != "t1" {
				t.Errorf("eval tab = %q, want t1", tab)
			}
			if !reflect.DeepEqual(args, tc.wantArgs) {
				t.Errorf("adapter args = %v, want %v", args, tc.wantArgs)
			}
			if strings.TrimSpace(stdout) != "{\n  \"ok\": true\n}" {
				t.Errorf("stdout = %q, want pretty adapter result (not raw JSON, not help)", stdout)
			}
			if cobraOut != "" {
				t.Errorf("unexpected Cobra output:\n%s", cobraOut)
			}
		})
	}

	for _, tc := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"missing double dash file", []string{"--url", "URL", "run", "--", "--absent.js"}, "--absent.js"},
		{"without separator it is an adapter flag", []string{"--url", "URL", "run", "--foo.js"}, "flag --foo.js needs a value"},
		{"separator after the script keeps the adapter rules", []string{"--url", "URL", "run", "./--foo.js", "--", "--foo.js"}, "flag --foo.js needs a value"},
		{"second double dash token is not a positional", []string{"--url", "URL", "run", "--", "--foo.js", "--foo.js"}, "flag --foo.js needs a value"},
		{"separator pair names no script file", []string{"--url", "URL", "run", "--", "--"}, "--"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BB_BROWSER_API_TOKEN", "")
			d := newRunDaemon(t)
			_, _, err := execCLI(t, expand(tc.args, d.URL, "")...)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tc.wantErr)
			}
			if n := d.requestCount(); n != 0 {
				t.Fatalf("daemon received %d requests, want 0", n)
			}
		})
	}
}
