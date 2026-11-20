package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yiplee/go-bb-browser/pkg/protocol"
)

func TestCLIAPIToken(t *testing.T) {
	for _, tc := range []struct {
		name  string
		env   string
		flags []string
		want  string
	}{
		{"disabled", "", nil, ""},
		{"environment", "env-secret", nil, "Bearer env-secret"},
		{"flag overrides environment", "env-secret", []string{"--api-token", "flag-secret"}, "Bearer flag-secret"},
		{"empty flag disables environment", "env-secret", []string{"--api-token", ""}, ""},
		{"trim whitespace", " env-secret ", nil, "Bearer env-secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BB_BROWSER_API_TOKEN", tc.env)
			var got string
			requests := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Get("Authorization")
				requests++
				if r.Method != http.MethodPost || r.URL.Path != "/v1" {
					t.Error("unexpected request")
				}
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"seq":1,"tabs":[]}}`))
			}))
			defer srv.Close()
			root := newRootCmd()
			root.SetArgs(append([]string{"--url", srv.URL, "tab", "list"}, tc.flags...))
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			if requests != 1 || got != tc.want {
				t.Fatalf("requests=%d Authorization=%q, want %q", requests, got, tc.want)
			}
		})
	}
}

func TestCLIHelpDoesNotExposeAPIToken(t *testing.T) {
	t.Setenv("BB_BROWSER_API_TOKEN", "help-secret")
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "help-secret") || !strings.Contains(out.String(), "--api-token") {
		t.Fatal("help omitted the flag or exposed the token")
	}
}

func TestTabListExitCode(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		exit int
	}{
		{name: "valid result", body: `{"jsonrpc":"2.0","id":1,"result":{"seq":1,"tabs":[]}}`, exit: 0},
		{name: "error null", body: `{"jsonrpc":"2.0","id":1,"result":{"seq":1},"error":null}`, exit: 0},
		{name: "empty body", body: "", exit: 1},
		{name: "non-json", body: "not-json", exit: 1},
		{name: "json-rpc error", body: `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"nope"}}`, exit: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			root := newRootCmd()
			root.SetArgs([]string{"--url", srv.URL, "tab", "list"})
			root.SilenceErrors = true
			root.SilenceUsage = true
			err := root.Execute()
			exit := 0
			if err != nil {
				exit = 1
			}
			if exit != tc.exit {
				t.Fatalf("exit %d, want %d: %v", exit, tc.exit, err)
			}
		})
	}
}

func TestFormatObsLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
		want string
	}{
		{
			name: "log entry with url prints text",
			data: `{"source":"javascript","level":"error","text":"failed to fetch","url":"https://example.com/app.js","lineNumber":12}`,
			want: "failed to fetch",
		},
		{
			name: "exception text",
			data: `{"text":"Uncaught TypeError","lineNumber":3}`,
			want: "Uncaught TypeError",
		},
		{
			name: "empty text falls back to raw json",
			data: `{"text":"","url":"https://example.com/app.js"}`,
			want: `{"text":"","url":"https://example.com/app.js"}`,
		},
		{
			name: "console type",
			data: `{"type":"warning","args":[]}`,
			want: `warning {"type":"warning","args":[]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatObsLine(json.RawMessage(tc.data)); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOpenCurrentStdout(t *testing.T) {
	t.Setenv("BB_BROWSER_API_TOKEN", "")
	const (
		tabID   = "ab12"
		okBody  = `{"jsonrpc":"2.0","id":1,"result":{"tab":"ab12","seq":9}}`
		errBody = `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"nope"}}`
	)
	for _, tc := range []struct {
		name    string
		json    bool
		body    string
		wantOut string
		wantErr bool
	}{
		{name: "text", body: okBody, wantOut: tabID + "\n"},
		{name: "json", json: true, body: okBody, wantOut: okBody + "\n"},
		{name: "text error", body: errBody, wantErr: true},
		{name: "json error", json: true, body: errBody, wantOut: errBody + "\n", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := make(chan struct {
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				var req struct {
					Method string         `json:"method"`
					Params map[string]any `json:"params"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Errorf("decode request: %v", err)
					http.Error(w, "bad request", http.StatusBadRequest)
					return
				}
				requests <- req
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			args := []string{"--url", srv.URL, "--tab", tabID}
			if tc.json {
				args = append(args, "--json")
			}
			args = append(args, "open", "--current", "https://example.com")

			var execErr error
			out := captureStdout(t, func() {
				root := newRootCmd()
				root.SetArgs(args)
				root.SilenceErrors = true
				root.SilenceUsage = true
				execErr = root.Execute()
			})
			if (execErr != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr %v", execErr, tc.wantErr)
			}
			if out != tc.wantOut {
				t.Fatalf("stdout = %q, want %q", out, tc.wantOut)
			}
			if tc.json {
				if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &map[string]any{}); err != nil {
					t.Fatalf("stdout is not one JSON value: %v\n%s", err, out)
				}
			}
			req := <-requests
			if req.Method != protocol.MethodGoto || req.Params["tab"] != tabID || req.Params["url"] != "https://example.com" {
				t.Fatalf("goto request = %+v", req)
			}
		})
	}
}

func TestOpenCurrentSilentRejected(t *testing.T) {
	t.Setenv("BB_BROWSER_API_TOKEN", "")
	requests := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- struct{}{}
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"--url", srv.URL, "open", "--current", "--silent", "https://example.com"})
	root.SilenceErrors = true
	root.SilenceUsage = true
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "--silent") {
		t.Fatalf("error = %v, want --current/--silent rejection", err)
	}
	select {
	case <-requests:
		t.Fatal("rejected flags still called the daemon")
	default:
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	defer func() {
		os.Stdout = orig
		_ = w.Close()
	}()
	fn()
	os.Stdout = orig
	_ = w.Close()
	return <-done
}

// `run` documents free-form adapter flags (`run x.js hello --count 20`). Cobra
// used to reject them as unknown flags before the adapter parser ever ran.
func TestRunPassesAdapterFlagsAndKeepsCLIFlags(t *testing.T) {
	t.Setenv("BB_BROWSER_API_TOKEN", "")
	script := filepath.Join(t.TempDir(), "x.js")
	if err := os.WriteFile(script, []byte("/* @meta {\"name\":\"x\",\"args\":{\"q\":{}}} */\nasync function(args){return args}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		args     []string // %s is the script path
		wantArgs string
		wantErr  string
	}{
		{name: "named flag", args: []string{"--tab", "ab12", "--json", "run", "%s", "hello", "--count", "20"}, wantArgs: `{"arg1":"hello","count":"20","q":"hello"}`},
		{name: "equals form", args: []string{"run", "%s", "--sort=new", "hello", "--json=true", "--tab", "ab12"}, wantArgs: `{"arg1":"hello","q":"hello","sort":"new"}`},
		{name: "cli flags after script", args: []string{"run", "%s", "hello", "--tab", "ab12", "--json", "--count", "20"}, wantArgs: `{"arg1":"hello","count":"20","q":"hello"}`},
		{name: "cli flags before script", args: []string{"run", "--json", "--tab=ab12", "%s", "hello"}, wantArgs: `{"arg1":"hello","q":"hello"}`},
		{name: "cli flags before command", args: []string{"--tab", "ab12", "--json", "run", "%s", "hello"}, wantArgs: `{"arg1":"hello","q":"hello"}`},
		{name: "double dash", args: []string{"run", "--tab", "ab12", "--json", "%s", "--", "--tab", "x"}, wantArgs: `{"tab":"x"}`},
		{name: "negative number", args: []string{"run", "--tab", "ab12", "--json", "%s", "-5"}, wantArgs: `{"arg1":"-5","q":"-5"}`},
		{name: "missing script", args: []string{"run", "--count", "20"}, wantErr: "script path"},
		{name: "cli flag without value", args: []string{"run", "%s", "--tab"}, wantErr: "--tab needs a value"},
		{name: "adapter flag without value", args: []string{"run", "--tab", "ab12", "%s", "--count"}, wantErr: "--count needs a value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotTab, gotScript string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Method string `json:"method"`
					Params struct {
						Tab    string `json:"tab"`
						Script string `json:"script"`
					} `json:"params"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Method != protocol.MethodEval {
					t.Errorf("unexpected request: %v %q", err, req.Method)
				}
				gotTab, gotScript = req.Params.Tab, req.Params.Script
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tab":"ab12","seq":1,"result":{}}}`))
			}))
			defer srv.Close()

			args := []string{"--url", srv.URL}
			for _, a := range tc.args {
				args = append(args, strings.ReplaceAll(a, "%s", script))
			}
			root := newRootCmd()
			root.SetArgs(args)
			root.SilenceUsage, root.SilenceErrors = true, true
			stdout := os.Stdout
			null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			os.Stdout = null
			err = root.Execute()
			os.Stdout = stdout
			null.Close()

			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to mention %q", err, tc.wantErr)
				}
				if gotScript != "" {
					t.Fatal("eval sent despite argument error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if gotTab != "ab12" {
				t.Fatalf("tab = %q, want ab12 (--tab not applied)", gotTab)
			}
			if !jsonOut {
				t.Fatal("--json not applied")
			}
			if want := "})( " + tc.wantArgs + " )"; !strings.HasSuffix(gotScript, want) {
				t.Fatalf("adapter args: script ends %q, want %q", gotScript[max(0, len(gotScript)-80):], want)
			}
		})
	}
}
