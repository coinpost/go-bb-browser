package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yiplee/go-bb-browser/internal/timeout"
	"github.com/yiplee/go-bb-browser/pkg/daemonclient"
	"github.com/yiplee/go-bb-browser/pkg/protocol"
)

// FT28 round-9 review, P2 (resolveTab). A request names a live tab the daemon
// has not registered yet, and the Target.getTargets it needs to resolve the id
// fails. Nothing is known about the id then: the answer must be the failure of
// the lookup with its cause — never "unknown tab id", which tells the caller
// (and the Pool) that the identity is invalid.

func expectLookupFailure(t *testing.T, srv *Server, m targetedMethod, tab string, wantHint ...string) {
	t.Helper()
	reply, err := enumRPC(srv, m.method, m.params(tab))
	if err != nil {
		t.Fatal(err)
	}
	if reply.Error == nil {
		t.Errorf("%s succeeded although the tab could not be looked up: %v", m.method, reply.Result)
		return
	}
	msg, _ := reply.Error["message"].(string)
	code, _ := reply.Error["code"].(float64)
	data, _ := reply.Error["data"].(map[string]any)
	hint, _ := data["hint"].(string)
	if strings.Contains(fmt.Sprint(reply.Error), "unknown tab id") {
		t.Errorf("%s: a failed lookup was reported as an invalid id: %v", m.method, reply.Error)
	}
	if int(code) != protocol.CodeServerError || msg != "failed to list targets" {
		t.Errorf("%s: error %v %q, want %d failed to list targets", m.method, code, msg, protocol.CodeServerError)
	}
	for _, want := range wantHint {
		if !strings.Contains(hint, want) {
			t.Errorf("%s: hint %q does not name %q", m.method, hint, want)
		}
	}
}

func TestTabLookupFailureIsNotAnUnknownTabID(t *testing.T) {
	conn := newIDConn(fullIDA, fullIDB) // live pages this daemon has not listed yet
	srv := newIDServer(t, conn, stateDirDisabled)
	tab := string(fullIDA)

	conn.setListErr(context.DeadlineExceeded)
	budget := fmt.Sprintf("limited to %ds", timeout.OperationSeconds)
	for _, m := range targetedMethods {
		expectLookupFailure(t, srv, m, tab, context.DeadlineExceeded.Error(), budget)
	}
	conn.setListErr(errors.New("websocket: transient failure"))
	for _, m := range targetedMethods {
		expectLookupFailure(t, srv, m, tab, "websocket: transient failure")
	}
	if n := conn.opCount(); n != 0 || !conn.has(fullIDA) || !conn.has(fullIDB) {
		t.Fatalf("commands sent while the lookup failed = %d (A open %v, B open %v)", n, conn.has(fullIDA), conn.has(fullIDB))
	}
	if srv.tabs.OpenWindows() != 0 {
		t.Errorf("windows left open by failed lookups: %d", srv.tabs.OpenWindows())
	}
	if n := tabLockEntryCount(srv); n != 0 {
		t.Errorf("lock entries = %d, want 0", n)
	}

	// The browser answers again: the very same id works for every method
	// (tab_close last), and only an id the browser really does not have is
	// reported as unknown.
	conn.setListErr(nil)
	for _, m := range targetedMethods {
		expectUnknownTab(t, srv, m, "CCCC0000000000000000000000AB1234")
	}
	for _, m := range targetedMethods {
		if res := enumMustRPC(t, srv, m.method, m.params(tab)); res["tab"] != tab {
			t.Errorf("%s result tab = %v, want %q", m.method, res["tab"], tab)
		}
	}
	if conn.has(fullIDA) || !conn.has(fullIDB) {
		t.Errorf("after tab_close(A): A open %v, B open %v", conn.has(fullIDA), conn.has(fullIDB))
	}
}

// The Pool turns the daemon's explicit "unknown tab id" into UnknownTabError.
// A daemon that was restarted (its registry is empty) and cannot list targets
// at the moment must not make the Pool report a live, bound tab as unknown.
func TestPoolDoesNotReportLookupFailureAsUnknownTab(t *testing.T) {
	conn := newIDConn(fullIDA, fullIDB)
	var current atomic.Pointer[Server]
	current.Store(newIDServer(t, conn, stateDirDisabled))
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current.Load().Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(hs.Close)
	pool, err := daemonclient.NewPool(daemonclient.NewClient(hs.URL))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	list, err := pool.TabList(ctx, protocol.TabListParams{})
	if err != nil || len(list.Tabs) != 2 {
		t.Fatalf("TabList = %+v, %v", list, err)
	}
	tab := "0:" + string(fullIDA)

	current.Store(newIDServer(t, conn, stateDirDisabled)) // the daemon restarted
	conn.setListErr(context.DeadlineExceeded)
	_, err = pool.Eval(ctx, protocol.EvalParams{Tab: tab, Script: "1"})
	var unknown *daemonclient.UnknownTabError
	var rpcErr *daemonclient.RPCError
	if err == nil || errors.As(err, &unknown) || isUnknownTabRPCError(err) {
		t.Fatalf("Eval while the daemon cannot list targets: %v, want the lookup failure, not an unknown tab", err)
	}
	if !errors.As(err, &rpcErr) || rpcErr.Code != protocol.CodeServerError || rpcErr.Message != "failed to list targets" {
		t.Fatalf("Eval error = %v, want the daemon's failed to list targets", err)
	}
	conn.setListErr(nil)
	if _, err := pool.Eval(ctx, protocol.EvalParams{Tab: tab, Script: "1"}); err != nil {
		t.Fatalf("Eval with the same Pool id once the daemon can list again: %v", err)
	}
	if ops := conn.opsFor(fullIDB); len(ops) != 0 {
		t.Errorf("B received %v", ops)
	}
}
