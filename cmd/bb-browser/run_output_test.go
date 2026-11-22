package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const loginHintLine = "  (Log in to the site in Chrome for this profile, then retry.)\n"

// runEvalEnvelope is the JSON-RPC success envelope a daemon returns for eval
// when the adapter's return value is adapterResult.
func runEvalEnvelope(adapterResult string) string {
	return `{"jsonrpc":"2.0","id":1,"result":{"tab":"t1","seq":7,"result":` + adapterResult + `}}`
}

// newRunOutputDaemon serves body for every eval call and fails the test on anything else.
func newRunOutputDaemon(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if r.Method != http.MethodPost || r.URL.Path != "/v1" || req.Method != "eval" {
			t.Errorf("unexpected request %s %s method=%q", r.Method, r.URL.Path, req.Method)
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func writeRunAdapter(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "adapter.js")
	if err := os.WriteFile(path, []byte("async function(args) { return 1; }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type runOutput struct {
	stdout string // process stdout
	stderr string // process stderr
	cobra  string // what Cobra itself printed (its "Error: …" line and usage)
	err    error
}

// executeRun runs `bb-browser run` in-process against a fake daemon that
// answers eval with body.
func executeRun(t *testing.T, body string, extraArgs ...string) runOutput {
	t.Helper()
	srv := newRunOutputDaemon(t, body)
	root := newRootCmd()
	var cobraOut bytes.Buffer
	root.SetOut(&cobraOut)
	root.SetErr(&cobraOut)
	root.SetArgs(append([]string{"--url", srv.URL, "--tab", "t1", "run", writeRunAdapter(t)}, extraArgs...))

	var out runOutput
	out.stderr = captureFile(t, &os.Stderr, func() {
		out.stdout = captureFile(t, &os.Stdout, func() {
			out.err = root.Execute()
		})
	})
	out.cobra = cobraOut.String()
	return out
}

func captureFile(t *testing.T, target **os.File, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := *target
	*target = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	defer func() {
		*target = orig
		_ = w.Close()
	}()
	fn()
	*target = orig
	_ = w.Close()
	return <-done
}

func TestRunAdapterErrorFailsCommand(t *testing.T) {
	for _, tc := range []struct {
		name       string
		result     string
		wantStderr string
	}{
		{
			name:       "login error with hint",
			result:     `{"error":"HTTP 401","hint":"please log in"}`,
			wantStderr: "[error] HTTP 401\n  Hint: please log in\n" + loginHintLine,
		},
		{
			name:       "no hint, not a login error",
			result:     `{"error":"Missing argument: query"}`,
			wantStderr: "[error] Missing argument: query\n",
		},
		{
			name:       "login wording only in hint",
			result:     `{"error":"CSRF token not found","hint":"Not logged in?"}`,
			wantStderr: "[error] CSRF token not found\n  Hint: Not logged in?\n" + loginHintLine,
		},
		{
			name:       "error alongside other fields",
			result:     `{"error":"rate limited","items":[1,2]}`,
			wantStderr: "[error] rate limited\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			envelope := runEvalEnvelope(tc.result)
			for _, mode := range []struct {
				name       string
				args       []string
				wantStdout string
			}{
				{name: "human", wantStdout: ""},
				{name: "json", args: []string{"--json"}, wantStdout: envelope + "\n"},
			} {
				t.Run(mode.name, func(t *testing.T) {
					out := executeRun(t, envelope, mode.args...)
					if out.err == nil {
						t.Error("Execute returned nil; adapter error must fail the command")
					}
					if out.stdout != mode.wantStdout {
						t.Errorf("stdout = %q, want %q", out.stdout, mode.wantStdout)
					}
					if out.stderr != tc.wantStderr {
						t.Errorf("stderr = %q, want %q", out.stderr, tc.wantStderr)
					}
					if out.cobra != "" {
						t.Errorf("Cobra printed usage or a duplicate error for an adapter failure: %q", out.cobra)
					}
				})
			}
		})
	}
}

func TestRunAdapterValueSucceeds(t *testing.T) {
	for _, tc := range []struct {
		name       string
		result     string
		wantStdout string
	}{
		{name: "object", result: `{"title":"hi","n":1}`, wantStdout: "{\n  \"n\": 1,\n  \"title\": \"hi\"\n}\n"},
		{name: "array", result: `[1,"a"]`, wantStdout: "[\n  1,\n  \"a\"\n]\n"},
		{name: "string", result: `"ok"`, wantStdout: "\"ok\"\n"},
		{name: "number", result: `42`, wantStdout: "42\n"},
		{name: "bool", result: `false`, wantStdout: "false\n"},
		{name: "null", result: `null`, wantStdout: "null\n"},
		{name: "empty string error", result: `{"error":""}`, wantStdout: "{\n  \"error\": \"\"\n}\n"},
		{name: "null error", result: `{"error":null,"data":1}`, wantStdout: "{\n  \"data\": 1,\n  \"error\": null\n}\n"},
		{name: "boolean error", result: `{"error":false}`, wantStdout: "{\n  \"error\": false\n}\n"},
		{name: "object error", result: `{"error":{"code":401}}`, wantStdout: "{\n  \"error\": {\n    \"code\": 401\n  }\n}\n"},
		{name: "error inside array", result: `[{"error":"HTTP 401"}]`, wantStdout: "[\n  {\n    \"error\": \"HTTP 401\"\n  }\n]\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			envelope := runEvalEnvelope(tc.result)
			for _, mode := range []struct {
				name       string
				args       []string
				wantStdout string
			}{
				{name: "human", wantStdout: tc.wantStdout},
				{name: "json", args: []string{"--json"}, wantStdout: envelope + "\n"},
			} {
				t.Run(mode.name, func(t *testing.T) {
					out := executeRun(t, envelope, mode.args...)
					if out.err != nil {
						t.Errorf("Execute returned %v, want nil", out.err)
					}
					if out.stdout != mode.wantStdout {
						t.Errorf("stdout = %q, want %q", out.stdout, mode.wantStdout)
					}
					if out.stderr != "" || out.cobra != "" {
						t.Errorf("unexpected diagnostics: stderr=%q cobra=%q", out.stderr, out.cobra)
					}
				})
			}
		})
	}
}

// A JSON-RPC level failure is not an adapter failure: it keeps the generic
// error path (returned to main, reported by Cobra) and never prints [error].
func TestRunRPCErrorStillFails(t *testing.T) {
	const envelope = `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"nope"}}`
	for _, mode := range []struct {
		name       string
		args       []string
		wantStdout string
	}{
		{name: "human", wantStdout: ""},
		{name: "json", args: []string{"--json"}, wantStdout: envelope + "\n"},
	} {
		t.Run(mode.name, func(t *testing.T) {
			out := executeRun(t, envelope, mode.args...)
			if out.err == nil || !strings.Contains(out.err.Error(), "json-rpc error") {
				t.Errorf("Execute returned %v, want a json-rpc error", out.err)
			}
			var adapterErr *adapterError
			if errors.As(out.err, &adapterErr) {
				t.Errorf("RPC failure was classified as an adapter error: %v", out.err)
			}
			if out.stdout != mode.wantStdout {
				t.Errorf("stdout = %q, want %q", out.stdout, mode.wantStdout)
			}
			if out.stderr != "" {
				t.Errorf("stderr = %q, want nothing printed by run itself", out.stderr)
			}
			if !strings.Contains(out.cobra, "json-rpc error") {
				t.Errorf("Cobra output = %q, want the generic error report", out.cobra)
			}
		})
	}
}

// TestRunMainHelper is not a test: TestRunExitStatus re-executes the test
// binary with BB_BROWSER_RUN_MAIN_ARGS set so that the real main() — including
// its error printing and os.Exit — runs in a subprocess.
func TestRunMainHelper(t *testing.T) {
	raw := os.Getenv("BB_BROWSER_RUN_MAIN_ARGS")
	if raw == "" {
		t.Skip("subprocess helper")
	}
	var args []string
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(3)
	}
	os.Args = append([]string{"bb-browser"}, args...)
	main()
	os.Exit(0)
}

func TestRunExitStatus(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	const (
		errorResult = `{"error":"HTTP 401","hint":"please log in"}`
		errorStderr = "[error] HTTP 401\n  Hint: please log in\n" + loginHintLine
	)
	for _, tc := range []struct {
		name       string
		result     string
		args       []string
		wantExit   int
		wantStdout func(envelope string) string
		wantStderr string
	}{
		{
			name: "adapter error", result: errorResult, wantExit: 1,
			wantStdout: func(string) string { return "" }, wantStderr: errorStderr,
		},
		{
			name: "adapter error json", result: errorResult, args: []string{"--json"}, wantExit: 1,
			wantStdout: func(envelope string) string { return envelope + "\n" }, wantStderr: errorStderr,
		},
		{
			name: "value", result: `{"ok":true}`, wantExit: 0,
			wantStdout: func(string) string { return "{\n  \"ok\": true\n}\n" },
		},
		{
			name: "value json", result: `{"ok":true}`, args: []string{"--json"}, wantExit: 0,
			wantStdout: func(envelope string) string { return envelope + "\n" },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			envelope := runEvalEnvelope(tc.result)
			srv := newRunOutputDaemon(t, envelope)
			args, err := json.Marshal(append([]string{"--url", srv.URL, "--tab", "t1", "run", writeRunAdapter(t)}, tc.args...))
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(exe, "-test.run=^TestRunMainHelper$")
			cmd.Env = append(os.Environ(), "BB_BROWSER_RUN_MAIN_ARGS="+string(args), "BB_BROWSER_API_TOKEN=", "BB_BROWSER_URL=")
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			exit := 0
			if err := cmd.Run(); err != nil {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) {
					t.Fatal(err)
				}
				exit = exitErr.ExitCode()
			}
			if exit != tc.wantExit {
				t.Errorf("exit status = %d, want %d", exit, tc.wantExit)
			}
			if want := tc.wantStdout(envelope); stdout.String() != want {
				t.Errorf("stdout = %q, want %q", stdout.String(), want)
			}
			if stderr.String() != tc.wantStderr {
				t.Errorf("stderr = %q, want %q", stderr.String(), tc.wantStderr)
			}
		})
	}
}
