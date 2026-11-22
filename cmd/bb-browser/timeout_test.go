package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yiplee/go-bb-browser/internal/timeout"
)

// deadlineTransport stands in for bb-daemon and records the context deadline of
// every request the CLI sends, so budgets are checked without waiting for them.
type deadlineTransport struct {
	respond func(*http.Request) (*http.Response, error)

	mu        sync.Mutex
	deadlines []time.Time
	missing   int
}

func (d *deadlineTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	d.mu.Lock()
	if dl, ok := r.Context().Deadline(); ok {
		d.deadlines = append(d.deadlines, dl)
	} else {
		d.missing++
	}
	d.mu.Unlock()
	if d.respond != nil {
		return d.respond(r)
	}
	body := `{"jsonrpc":"2.0","id":1,"result":{"tab":"t1","seq":1,"focus":"t1","tabs":[],"result":{"ok":true},"status":"ok","browser":"connected"}}`
	if strings.HasSuffix(r.URL.Path, "/health") {
		body = `{"status":"ok","browser":"connected"}`
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
	}, nil
}

func useTransport(t *testing.T, rt http.RoundTripper) {
	t.Helper()
	old := http.DefaultClient.Transport
	http.DefaultClient.Transport = rt
	t.Cleanup(func() { http.DefaultClient.Transport = old })
}

// The daemon ends every RPC at timeout.Operation, so no CLI command may wait
// (or promise to wait) longer than that for it.
func TestCommandBudgetMatchesDaemonOperation(t *testing.T) {
	script := runAdapterFile(t)
	cases := map[string][]string{
		"run":             {"run", script, "hello", "--tab", "t1"},
		"run focus tab":   {"run", script, "hello"},
		"eval":            {"eval", "1+1", "--tab", "t1", "--json"},
		"snapshot":        {"snapshot", "--tab", "t1", "--json"},
		"fetch":           {"fetch", "https://example.com/api", "--tab", "t1", "--json"},
		"screenshot":      {"screenshot", "--tab", "t1", "--json"},
		"html":            {"html", "--tab", "t1", "--json"},
		"goto":            {"goto", "https://example.com", "--tab", "t1", "--json"},
		"open":            {"open", "https://example.com", "--json"},
		"open current":    {"open", "https://example.com", "--current", "--tab", "t1", "--json"},
		"click":           {"click", "#a", "--tab", "t1", "--json"},
		"fill":            {"fill", "#a", "x", "--tab", "t1", "--json"},
		"reload":          {"reload", "--tab", "t1", "--json"},
		"tab list":        {"tab", "list", "--json"},
		"tab new":         {"tab", "new", "--json"},
		"console":         {"console", "--tab", "t1", "--json"},
		"network":         {"network", "requests", "--tab", "t1", "--json"},
		"health":          {"health", "--json"},
		"network clear":   {"network", "clear", "--tab", "t1", "--json"},
		"network route":   {"network", "route", "*x*", "--abort", "--tab", "t1", "--json"},
		"network unroute": {"network", "unroute", "--tab", "t1", "--json"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			rt := &deadlineTransport{}
			useTransport(t, rt)
			start := time.Now()
			if _, out, err := execCLI(t, append([]string{"--url", "http://daemon.invalid"}, args...)...); err != nil {
				t.Fatalf("command failed: %v\n%s", err, out)
			}
			limit := time.Now().Add(timeout.Operation)
			rt.mu.Lock()
			defer rt.mu.Unlock()
			if rt.missing != 0 || len(rt.deadlines) == 0 {
				t.Fatalf("requests with deadline = %d, without = %d; want every request bounded", len(rt.deadlines), rt.missing)
			}
			for i, dl := range rt.deadlines {
				if dl.After(limit) {
					t.Errorf("request %d may wait %v, longer than the daemon's %v per-RPC limit",
						i, dl.Sub(start).Round(time.Second), timeout.Operation)
				}
			}
		})
	}
}

// Long-running commands get the whole daemon budget, not a shorter one.
func TestLongCommandBudgetEqualsDaemonOperation(t *testing.T) {
	script := runAdapterFile(t)
	for name, args := range map[string][]string{
		"run":        {"run", script, "hello", "--tab", "t1"},
		"eval":       {"eval", "1+1", "--tab", "t1", "--json"},
		"snapshot":   {"snapshot", "--tab", "t1", "--json"},
		"fetch":      {"fetch", "https://example.com/api", "--tab", "t1", "--json"},
		"screenshot": {"screenshot", "--tab", "t1", "--json"},
		"goto":       {"goto", "https://example.com", "--tab", "t1", "--json"},
	} {
		t.Run(name, func(t *testing.T) {
			rt := &deadlineTransport{}
			useTransport(t, rt)
			start := time.Now()
			if _, out, err := execCLI(t, append([]string{"--url", "http://daemon.invalid"}, args...)...); err != nil {
				t.Fatalf("command failed: %v\n%s", err, out)
			}
			end := time.Now()
			rt.mu.Lock()
			defer rt.mu.Unlock()
			if len(rt.deadlines) != 1 {
				t.Fatalf("requests = %d, want 1", len(rt.deadlines))
			}
			dl := rt.deadlines[0]
			if dl.Before(start.Add(timeout.Operation)) || dl.After(end.Add(timeout.Operation)) {
				t.Fatalf("budget = %v, want %v", dl.Sub(start).Round(time.Second), timeout.Operation)
			}
		})
	}
}

func TestCommandTimeoutErrorNamesLimit(t *testing.T) {
	script := runAdapterFile(t)
	want := fmt.Sprintf("%ds", timeout.OperationSeconds)
	for name, args := range map[string][]string{
		"run":  {"run", script, "hello", "--tab", "t1"},
		"eval": {"eval", "1+1", "--tab", "t1"},
	} {
		t.Run(name, func(t *testing.T) {
			useTransport(t, &deadlineTransport{respond: func(*http.Request) (*http.Response, error) {
				return nil, context.DeadlineExceeded
			}})
			_, _, err := execCLI(t, append([]string{"--url", "http://daemon.invalid"}, args...)...)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("err = %v, want context.DeadlineExceeded", err)
			}
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("err = %q, want it to name the %s limit", err, want)
			}
		})
	}
}

// A caller that cancels earlier wins: the command returns at once and is not
// reported as having hit the time limit.
func TestCommandCancelBeatsBudget(t *testing.T) {
	started := make(chan struct{})
	useTransport(t, &deadlineTransport{respond: func(r *http.Request) (*http.Response, error) {
		close(started)
		<-r.Context().Done()
		return nil, r.Context().Err()
	}})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	root := newRootCmd()
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SilenceErrors = true
	root.SilenceUsage = true
	root.SetArgs([]string{"--url", "http://daemon.invalid", "eval", "1+1", "--tab", "t1"})
	begin := time.Now()
	err := root.ExecuteContext(ctx)
	if elapsed := time.Since(begin); elapsed > 10*time.Second {
		t.Fatalf("cancelled command returned after %v", elapsed)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if strings.Contains(err.Error(), fmt.Sprintf("%ds", timeout.OperationSeconds)) {
		t.Fatalf("cancellation reported as a timeout: %q", err)
	}
}
