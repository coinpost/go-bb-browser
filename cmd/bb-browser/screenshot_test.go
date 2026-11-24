package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/coinpost/go-bb-browser/pkg/protocol"
)

// screenshotDaemon is a fake daemon answering screenshot with the given result
// JSON (or a full envelope when it already carries "jsonrpc"). It records the
// requested format.
func screenshotDaemon(t *testing.T, body string, gotFormat *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string                    `json:"method"`
			Params protocol.ScreenshotParams `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1" || req.Method != protocol.MethodScreenshot || req.Params.Tab != "t1" {
			t.Errorf("unexpected request %s %s method=%q tab=%q", r.Method, r.URL.Path, req.Method, req.Params.Tab)
		}
		if gotFormat != nil {
			*gotFormat = req.Params.Format
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func screenshotEnvelope(t *testing.T, result map[string]any) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// runScreenshot runs the CLI in a fresh working directory and returns the
// execution error plus the files left behind (name -> content).
func runScreenshot(t *testing.T, url string, args ...string) (error, map[string][]byte) {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	root := newRootCmd()
	root.SetArgs(append([]string{"--url", url, "--tab", "t1", "screenshot"}, args...))
	root.SilenceErrors = true
	root.SilenceUsage = true
	err := root.Execute()
	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	files := map[string][]byte{}
	for _, e := range entries {
		b, rerr := os.ReadFile(e.Name())
		if rerr != nil {
			t.Fatal(rerr)
		}
		files[e.Name()] = b
	}
	return err, files
}

func TestScreenshotDefaultNameFollowsResponseMIME(t *testing.T) {
	payload := []byte("\xff\xd8\xff not really an image \x00\x01")
	data := base64.StdEncoding.EncodeToString(payload)
	for _, tc := range []struct {
		name    string
		args    []string
		mime    any // nil omits the field
		wantFmt string
		wantExt string
	}{
		{"jpeg requested", []string{"--format", "jpeg"}, "image/jpeg", "jpeg", ".jpg"},
		{"jpg requested", []string{"--format", "jpg"}, "image/jpeg", "jpg", ".jpg"},
		{"png requested", []string{"--format", "png"}, "image/png", "png", ".png"},
		{"default format", nil, "image/png", "png", ".png"},
		{"png requested but jpeg returned", []string{"--format", "png"}, "image/jpeg", "png", ".jpg"},
		{"jpeg requested but png returned", []string{"--format", "jpeg"}, "image/png", "jpeg", ".png"},
		{"jpeg requested but mime missing", []string{"--format", "jpeg"}, nil, "jpeg", ".png"},
		{"jpeg requested but mime empty", []string{"--format", "jpeg"}, "", "jpeg", ".png"},
		{"jpeg requested but mime unknown", []string{"--format", "jpeg"}, "image/webp", "jpeg", ".png"},
		{"empty output flag uses auto name", []string{"--format", "jpeg", "--output", ""}, "image/jpeg", "jpeg", ".jpg"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := map[string]any{"tab": "t1", "seq": 7, "data": data}
			if tc.mime != nil {
				result["mime"] = tc.mime
			}
			var gotFmt string
			srv := screenshotDaemon(t, screenshotEnvelope(t, result), &gotFmt)
			err, files := runScreenshot(t, srv.URL, tc.args...)
			if err != nil {
				t.Fatal(err)
			}
			if gotFmt != tc.wantFmt {
				t.Errorf("requested format %q, want %q", gotFmt, tc.wantFmt)
			}
			if len(files) != 1 {
				t.Fatalf("got %d files, want 1: %v", len(files), fileNames(files))
			}
			want := regexp.MustCompile(`^screenshot-\d+` + regexp.QuoteMeta(tc.wantExt) + `$`)
			for name, got := range files {
				if !want.MatchString(name) {
					t.Errorf("auto name %q, want match %s", name, want)
				}
				if !bytes.Equal(got, payload) {
					t.Errorf("%s content differs from decoded response data", name)
				}
			}
		})
	}
}

func TestScreenshotExplicitPathNotRenamed(t *testing.T) {
	payload := []byte("\xff\xd8\xff jpeg bytes \x00\x01")
	body := screenshotEnvelope(t, map[string]any{
		"tab": "t1", "seq": 7, "mime": "image/jpeg",
		"data": base64.StdEncoding.EncodeToString(payload),
	})
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"positional png path", []string{"--format", "jpeg", "shot.png"}, "shot.png"},
		{"positional without extension", []string{"--format", "jpeg", "shot"}, "shot"},
		{"output flag png path", []string{"--format", "jpeg", "--output", "out.png"}, "out.png"},
		{"positional wins over output flag", []string{"--format", "jpeg", "--output", "out.png", "pos.png"}, "pos.png"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := screenshotDaemon(t, body, nil)
			err, files := runScreenshot(t, srv.URL, tc.args...)
			if err != nil {
				t.Fatal(err)
			}
			if len(files) != 1 || !bytes.Equal(files[tc.want], payload) {
				t.Fatalf("files %v, want only %q with unchanged bytes", fileNames(files), tc.want)
			}
		})
	}
}

func TestScreenshotWritesNoFileOnFailureOrJSON(t *testing.T) {
	jpeg := screenshotEnvelope(t, map[string]any{
		"tab": "t1", "seq": 7, "mime": "image/jpeg",
		"data": base64.StdEncoding.EncodeToString([]byte("jpeg bytes")),
	})
	for _, tc := range []struct {
		name    string
		body    string
		args    []string
		wantErr bool
	}{
		{"json-rpc error", `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"nope"}}`, []string{"--format", "jpeg"}, true},
		{"invalid base64", screenshotEnvelope(t, map[string]any{"tab": "t1", "seq": 7, "mime": "image/jpeg", "data": "!!not base64!!"}), []string{"--format", "jpeg"}, true},
		{"non-json body", "not-json", []string{"--format", "jpeg"}, true},
		{"json output", jpeg, []string{"--format", "jpeg", "--json"}, false},
		{"json output ignores explicit path", jpeg, []string{"--format", "jpeg", "--json", "shot.jpg"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := screenshotDaemon(t, tc.body, nil)
			var err error
			var files map[string][]byte
			out := captureStdout(t, func() {
				err, files = runScreenshot(t, srv.URL, tc.args...)
			})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if len(files) != 0 {
				t.Fatalf("unexpected files written: %v", fileNames(files))
			}
			if !tc.wantErr && strings.TrimSpace(out) != tc.body {
				t.Fatalf("--json stdout = %q, want raw envelope %q", out, tc.body)
			}
		})
	}
}

func fileNames(files map[string][]byte) []string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	return names
}
