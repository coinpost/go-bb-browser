package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"syscall"
	"testing"

	"github.com/chromedp/cdproto"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/yiplee/go-bb-browser/internal/browser"
	"github.com/yiplee/go-bb-browser/pkg/protocol"
)

// failingConn is a fakeConn whose page-evaluating calls fail with a
// configurable error, as Session.Eval/Snapshot/FetchPage would surface it.
type failingConn struct {
	*fakeConn
	mu  sync.Mutex
	err error
}

func (c *failingConn) setErr(err error) {
	c.mu.Lock()
	c.err = err
	c.mu.Unlock()
}

func (c *failingConn) failure() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *failingConn) Eval(tid target.ID, script string) (json.RawMessage, error) {
	if err := c.failure(); err != nil {
		return nil, err
	}
	return c.fakeConn.Eval(tid, script)
}

func (c *failingConn) Snapshot(tid target.ID, opts browser.SnapshotOpts) (string, string, string, map[string]string, error) {
	if err := c.failure(); err != nil {
		return "", "", "", nil, err
	}
	return c.fakeConn.Snapshot(tid, opts)
}

func (c *failingConn) FetchPage(tid target.ID, u, m string, h []byte, b string) (json.RawMessage, error) {
	if err := c.failure(); err != nil {
		return nil, err
	}
	return c.fakeConn.FetchPage(tid, u, m, h, b)
}

func (c *failingConn) AppendNetworkRoute(tid target.ID, r browser.NetworkRouteRule) error {
	if err := c.failure(); err != nil {
		return err
	}
	return c.fakeConn.AppendNetworkRoute(tid, r)
}

const classificationTab = "1b2c"

// newClassificationServer wires the fake as the live session (not tabHook) so
// ensureBrowserSession really consults browserState instead of short-circuiting.
func newClassificationServer(t *testing.T) (*Server, *failingConn) {
	t.Helper()
	srv := newSupervisorTestServer(t, 3)
	conn := &failingConn{fakeConn: &fakeConn{infos: []*target.Info{{TargetID: "AAAA00000000000000000000000A1B2C", Type: "page"}}}}
	srv.tabLive = conn
	if srv.tabHook != nil || srv.SkipBrowserAttach {
		t.Fatal("test server must not bypass ensureBrowserSession")
	}
	return srv, conn
}

type rpcEnvelope struct {
	Result json.RawMessage         `json:"result"`
	Error  *protocol.ResponseError `json:"error"`
}

func classificationRPC(t *testing.T, srv *Server, method string) rpcEnvelope {
	t.Helper()
	params := map[string]any{
		"tab":            classificationTab,
		"script":         "x()",
		"selector_scope": "div[data-kind=websocket",
		"url":            "https://example.test/feed",
		"url_pattern":    "https://example.test/*",
		"abort":          true,
	}
	rec := postRPC(t, srv, rpcReq(method, params, 1))
	var env rpcEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("%s: %v: %s", method, err, rec.Body.String())
	}
	return env
}

func assertSessionHealthy(t *testing.T, srv *Server) {
	t.Helper()
	select {
	case err := <-srv.fatalCh:
		t.Fatalf("request-local failure made the daemon declare CDP lost: %v", err)
	default:
	}
	srv.tabMu.RLock()
	st := srv.browserState
	srv.tabMu.RUnlock()
	if st != sessionReady {
		t.Fatalf("browserState = %v, want sessionReady", st)
	}
	if err := srv.ensureBrowserSession(context.Background()); err != nil {
		t.Fatalf("session should stay usable: %v", err)
	}
}

var evaluatingMethods = []string{
	protocol.MethodEval,
	protocol.MethodSnapshot,
	protocol.MethodFetch,
	protocol.MethodNetworkRoute,
}

func nonFatalCDPErrors() map[string]error {
	exc := func(desc string) error {
		return &runtime.ExceptionDetails{Text: "Uncaught", Exception: &runtime.RemoteObject{Description: desc}}
	}
	out := map[string]error{
		"page exception websocket state": exc("InvalidStateError: Failed to execute 'send' on 'WebSocket': Still in CONNECTING state."),
		"page exception eof":             exc("Error: unexpected EOF while parsing feed"),
		"page exception selector echo":   exc("SyntaxError: Failed to execute 'querySelector' on 'Document': 'div[data-kind=websocket' is not a valid selector."),
		"wrapped page exception":         fmt.Errorf("adapter: %w", exc("Error: websocket EOF")),
		"session wrapped page exception": &browser.CDPError{Kind: browser.ErrorRequest, Op: "Runtime.evaluate", Target: "T", Err: exc("Error: use of closed network connection")},
		"protocol error":                 &cdproto.Error{Code: -32000, Message: "Cannot find context with specified id: websocket eof"},
		"wrapped protocol error":         fmt.Errorf("fetch: %w", &cdproto.Error{Code: -32000, Message: "channel closed"}),
		"protocol target gone":           &cdproto.Error{Code: -32602, Message: "No target with given id found"},
		"typed request":                  &browser.CDPError{Kind: browser.ErrorRequest, Op: "Runtime.evaluate", Err: errors.New("websocket eof broken pipe")},
		"typed timeout":                  &browser.CDPError{Kind: browser.ErrorTimeout, Op: "Runtime.evaluate", Err: errors.New("websocket read timed out: eof")},
		"typed target gone":              &browser.CDPError{Kind: browser.ErrorTargetGone, Op: "Target.attachToTarget", Err: errors.New("target removed while attaching: channel closed")},
		"wrapped typed request":          fmt.Errorf("outer websocket: %w", &browser.CDPError{Kind: browser.ErrorRequest, Err: errors.New("connection reset")}),
		"raw target closed":              errors.New("Target closed"),
		"deadline":                       context.DeadlineExceeded,
		"wrapped deadline":               fmt.Errorf("websocket read: %w", context.DeadlineExceeded),
		"canceled":                       context.Canceled,
		"wrapped canceled":               fmt.Errorf("websocket read: %w", context.Canceled),
	}
	for _, kw := range []string{"eof", "broken pipe", "connection reset", "connection refused", "use of closed network connection", "websocket", "channel closed", "could not dial"} {
		out["page exception keyword "+kw] = exc("Error: " + kw)
	}
	return out
}

func fatalCDPErrors() map[string]error {
	return map[string]error{
		"io.EOF":                 io.EOF,
		"wrapped io.EOF":         fmt.Errorf("read message: %w", io.EOF),
		"net.ErrClosed":          &net.OpError{Op: "write", Net: "tcp", Err: net.ErrClosed},
		"closed connection text": errors.New("write tcp 127.0.0.1:1->127.0.0.1:9222: use of closed network connection"),
		"broken pipe":            &net.OpError{Op: "write", Net: "tcp", Err: syscall.EPIPE},
		"websocket close":        errors.New("websocket: close 1006 (abnormal closure)"),
		"channel closed":         errors.New("channel closed"),
		"typed transport":        &browser.CDPError{Kind: browser.ErrorTransport, Op: "Runtime.evaluate", Err: errors.New("socket gone")},
		"wrapped typed transport": fmt.Errorf("outer: %w",
			&browser.CDPError{Kind: browser.ErrorTransport, Op: "Runtime.evaluate", Err: io.EOF}),
	}
}

func TestIsCDPSessionLostTrustsTypedClassification(t *testing.T) {
	if isCDPSessionLost(nil) {
		t.Fatal("nil error reported as lost session")
	}
	for name, err := range nonFatalCDPErrors() {
		t.Run("not lost/"+name, func(t *testing.T) {
			if isCDPSessionLost(err) {
				t.Fatalf("isCDPSessionLost(%v) = true, want false", err)
			}
		})
	}
	for name, err := range fatalCDPErrors() {
		t.Run("lost/"+name, func(t *testing.T) {
			if !isCDPSessionLost(err) {
				t.Fatalf("isCDPSessionLost(%v) = false, want true", err)
			}
		})
	}
}

// A page-level exception or protocol reply fails only the current RPC: the
// daemon keeps its CDP session and serves the next request.
func TestRequestLocalCDPFailureDoesNotKillDaemon(t *testing.T) {
	for name, cdpErr := range nonFatalCDPErrors() {
		for _, method := range evaluatingMethods {
			t.Run(method+"/"+name, func(t *testing.T) {
				srv, conn := newClassificationServer(t)
				defer srv.closeAudit()
				conn.setErr(cdpErr)

				env := classificationRPC(t, srv, method)
				if env.Error == nil || env.Error.Code != protocol.CodeServerError {
					t.Fatalf("expected a JSON-RPC server error, got result=%s error=%+v", env.Result, env.Error)
				}
				assertSessionHealthy(t, srv)

				conn.setErr(nil)
				next := classificationRPC(t, srv, method)
				if next.Error != nil {
					t.Fatalf("next %s after request-local failure: %+v", method, next.Error)
				}
				assertSessionHealthy(t, srv)
			})
		}
	}
}

// Real transport loss must stay fatal so the supervisor restarts the daemon.
func TestTransportCDPFailureStaysFatal(t *testing.T) {
	for name, cdpErr := range fatalCDPErrors() {
		for _, method := range evaluatingMethods {
			t.Run(method+"/"+name, func(t *testing.T) {
				srv, conn := newClassificationServer(t)
				defer srv.closeAudit()
				conn.setErr(cdpErr)

				env := classificationRPC(t, srv, method)
				if env.Error == nil {
					t.Fatalf("expected a JSON-RPC error, got %s", env.Result)
				}
				select {
				case err := <-srv.fatalCh:
					if !errors.Is(err, cdpErr) {
						t.Fatalf("fatal error %v does not wrap %v", err, cdpErr)
					}
				default:
					t.Fatal("transport loss did not signal fatal exit")
				}
				if err := srv.ensureBrowserSession(context.Background()); !errors.Is(err, errBrowserNotConnected) {
					t.Fatalf("ensureBrowserSession = %v, want errBrowserNotConnected", err)
				}
				conn.setErr(nil)
				calls := conn.pageCalls.Load()
				if next := classificationRPC(t, srv, method); next.Error == nil {
					t.Fatalf("failed session still served %s: %s", method, next.Result)
				}
				if got := conn.pageCalls.Load(); got != calls {
					t.Fatalf("failed session still sent %d CDP calls", got-calls)
				}
			})
		}
	}
}
