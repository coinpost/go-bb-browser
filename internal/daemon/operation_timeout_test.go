package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/yiplee/go-bb-browser/internal/timeout"
	"github.com/yiplee/go-bb-browser/pkg/protocol"
)

// budgetEvalConn is an Eval that never finishes on its own: it only ends with
// its context. With wait unset it reports the deadline it was given instead of
// sleeping until it, so the per-RPC budget is observed without a real 60s wait.
type budgetEvalConn struct {
	*fakeConn
	wait    bool
	started chan struct{}

	mu       sync.Mutex
	deadline time.Time
	bounded  bool
	ctxErr   error
}

func (c *budgetEvalConn) EvalContext(ctx context.Context, _ target.ID, _ string) (json.RawMessage, error) {
	dl, ok := ctx.Deadline()
	c.mu.Lock()
	c.deadline, c.bounded = dl, ok
	c.mu.Unlock()
	if !c.wait {
		return nil, fmt.Errorf("Runtime.evaluate: %w", context.DeadlineExceeded)
	}
	close(c.started)
	<-ctx.Done()
	c.mu.Lock()
	c.ctxErr = ctx.Err()
	c.mu.Unlock()
	return nil, fmt.Errorf("Runtime.evaluate: %w", ctx.Err())
}

func newBudgetServer(t *testing.T, conn *budgetEvalConn) (*Server, string) {
	t.Helper()
	cfg := Config{DebuggerURL: "127.0.0.1:9222", ListenAddr: "127.0.0.1:0", StateDir: stateDirDisabled}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	tid := target.ID("ABCDEF123456")
	conn.fakeConn = &fakeConn{infos: []*target.Info{{TargetID: tid, Type: "page"}}}
	srv.tabHook = conn
	return srv, srv.tabs.RegisterPageTarget(tid)
}

func decodeRPCError(t *testing.T, rec *httptest.ResponseRecorder) (string, protocol.ErrData) {
	t.Helper()
	var env struct {
		Error *struct {
			Message string           `json:"message"`
			Data    protocol.ErrData `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || env.Error == nil {
		t.Fatalf("want JSON-RPC error, got %s (%v)", rec.Body.String(), err)
	}
	return env.Error.Message, env.Error.Data
}

func operationLimitText() string { return fmt.Sprintf("%ds", timeout.OperationSeconds) }

// An eval that outlives the budget is cut at timeout.Operation, and the error
// tells the caller which limit it hit.
func TestEvalDeadlineIsOperationBudget(t *testing.T) {
	conn := &budgetEvalConn{}
	srv, short := newBudgetServer(t, conn)

	before := time.Now()
	rec := postRPC(t, srv, rpcReq(protocol.MethodEval, map[string]any{"tab": short, "script": "new Promise(() => {})"}, 1))
	after := time.Now()

	conn.mu.Lock()
	deadline, bounded := conn.deadline, conn.bounded
	conn.mu.Unlock()
	if !bounded {
		t.Fatal("eval ran without a deadline")
	}
	if deadline.Before(before.Add(timeout.Operation)) || deadline.After(after.Add(timeout.Operation)) {
		t.Fatalf("eval budget = %v, want %v", deadline.Sub(before).Round(time.Second), timeout.Operation)
	}
	msg, data := decodeRPCError(t, rec)
	if msg != "eval failed" || data.Error != "eval failed" {
		t.Fatalf("error = %q / %q, want eval failed", msg, data.Error)
	}
	if !strings.Contains(data.Hint, context.DeadlineExceeded.Error()) {
		t.Fatalf("hint %q lost the underlying cause", data.Hint)
	}
	if !strings.Contains(data.Hint, operationLimitText()) {
		t.Fatalf("hint %q does not name the %s per-RPC limit", data.Hint, operationLimitText())
	}
}

// A caller that goes away earlier ends the eval at once, and is not told it
// hit the per-RPC limit.
func TestEvalCallerCancelBeatsOperationBudget(t *testing.T) {
	conn := &budgetEvalConn{wait: true, started: make(chan struct{})}
	srv, short := newBudgetServer(t, conn)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-conn.started
		cancel()
	}()
	body := rpcReq(protocol.MethodEval, map[string]any{"tab": short, "script": "new Promise(() => {})"}, 1)
	req := httptest.NewRequest(http.MethodPost, "/v1", bytes.NewBufferString(body)).WithContext(ctx)
	rec := httptest.NewRecorder()
	begin := time.Now()
	srv.Handler().ServeHTTP(rec, req)
	if elapsed := time.Since(begin); elapsed > 10*time.Second {
		t.Fatalf("cancelled eval returned after %v", elapsed)
	}

	conn.mu.Lock()
	ctxErr, deadline := conn.ctxErr, conn.deadline
	conn.mu.Unlock()
	if ctxErr != context.Canceled {
		t.Fatalf("eval context ended with %v, want context.Canceled", ctxErr)
	}
	if !time.Now().Before(deadline) {
		t.Fatal("eval was not cancelled before its deadline")
	}
	if rec.Body.Len() > 0 {
		if _, data := decodeRPCError(t, rec); strings.Contains(data.Hint, operationLimitText()) {
			t.Fatalf("cancellation reported as the per-RPC limit: %q", data.Hint)
		}
	}
}

// The HTTP server must outlast the operation budget so the timeout error can
// still be written.
func TestWriteTimeoutLeavesFlushMarginAfterOperation(t *testing.T) {
	srv := newHTTPServer(http.NotFoundHandler())
	if got, want := srv.WriteTimeout, srv.ReadTimeout+timeout.Operation+flushMargin; got != want {
		t.Fatalf("WriteTimeout = %v, want %v", got, want)
	}
	if flushMargin <= 0 {
		t.Fatalf("flushMargin = %v, want > 0", flushMargin)
	}
}
