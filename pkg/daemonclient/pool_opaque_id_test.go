package daemonclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/yiplee/go-bb-browser/pkg/protocol"
)

// strictDaemon answers like bb-daemon for tab identity: it knows complete tab
// ids only, byte for byte, and answers "unknown tab id" for anything else.
type strictDaemon struct {
	mu    sync.Mutex
	tabs  []string
	next  string
	evals []string
	fail  *protocol.ResponseError // answer for every targeted call, when set
}

func (d *strictDaemon) drop(tab string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	kept := d.tabs[:0]
	for _, have := range d.tabs {
		if have != tab {
			kept = append(kept, have)
		}
	}
	d.tabs = kept
}

func (d *strictDaemon) evalTabs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.evals...)
}

func (d *strictDaemon) serve(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Method string         `json:"method"`
			ID     any            `json:"id"`
			Params map[string]any `json:"params"`
		}
		_ = json.Unmarshal(body, &req)
		d.mu.Lock()
		defer d.mu.Unlock()
		result := map[string]any{"seq": 1}
		var rpcErr *protocol.ResponseError
		switch req.Method {
		case protocol.MethodTabList:
			items := []map[string]string{}
			for _, tab := range d.tabs {
				items = append(items, map[string]string{"tab": tab})
			}
			result["tabs"] = items
			if len(d.tabs) > 0 {
				result["tab"], result["focus"] = d.tabs[0], d.tabs[0]
			}
		case protocol.MethodTabFocus:
			if len(d.tabs) == 0 {
				rpcErr = &protocol.ResponseError{Code: protocol.CodeInvalidParams, Message: "no focused tab"}
				break
			}
			result["tab"], result["focus"] = d.tabs[0], d.tabs[0]
		case protocol.MethodTabNew:
			d.tabs = append(d.tabs, d.next)
			result["tab"] = d.next
		default:
			tab, _ := req.Params["tab"].(string)
			known := false
			for _, have := range d.tabs {
				known = known || have == tab
			}
			switch {
			case d.fail != nil:
				rpcErr = d.fail
			case !known:
				rpcErr = &protocol.ResponseError{Code: protocol.CodeInvalidParams, Message: "unknown tab id",
					Data: json.RawMessage(`{"error":"unknown tab id","hint":"tab must be the complete CDP target id"}`)}
			default:
				result["tab"] = tab
				if req.Method == protocol.MethodEval {
					d.evals = append(d.evals, tab)
					result["result"] = 1
				}
			}
		}
		out := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if rpcErr != nil {
			out["error"] = rpcErr
		} else {
			out["result"] = result
		}
		raw, _ := json.Marshal(out)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// A tab id is opaque. What TabList / TabFocus return is what the pool binds:
// an id with surrounding whitespace must be usable exactly as returned, and
// its trimmed spelling must not be bound in its place.
func TestPool_observedTabIDIsBoundVerbatim(t *testing.T) {
	const bare = "AAAA0000000000000000000000AB1234"
	const native = bare + " "
	for _, observe := range []string{"list", "focus"} {
		t.Run(observe, func(t *testing.T) {
			d := &strictDaemon{tabs: []string{native}}
			p, err := NewPool(NewClient(d.serve(t)))
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			var id string
			if observe == "list" {
				r, err := p.TabList(ctx, protocol.TabListParams{})
				if err != nil {
					t.Fatal(err)
				}
				id = r.Tabs[0].Tab
			} else {
				r, err := p.TabFocus(ctx, protocol.TabFocusParams{})
				if err != nil {
					t.Fatal(err)
				}
				id = r.Tab
			}
			if id != "0:"+native {
				t.Fatalf("returned id = %q, want %q", id, "0:"+native)
			}
			if _, ok := p.ClientForTab(id); !ok {
				t.Errorf("the id returned by the pool is not bound")
			}
			if _, ok := p.ClientForTab("0:" + bare); ok {
				t.Errorf("a trimmed spelling of the id is bound")
			}
			if _, err := p.Eval(ctx, protocol.EvalParams{Tab: id, Script: "1"}); err != nil {
				t.Errorf("Eval with the returned id: %v", err)
			}
			if got := d.evalTabs(); len(got) != 1 || got[0] != native {
				t.Errorf("daemon saw eval tabs %q, want exactly %q", got, native)
			}
			var unknown *UnknownTabError
			if _, err := p.Eval(ctx, protocol.EvalParams{Tab: "0:" + bare, Script: "1"}); !errors.As(err, &unknown) {
				t.Errorf("Eval with the trimmed id: err = %v, want UnknownTabError", err)
			}
		})
	}
}

// A tab closed behind the pool's back: the op still goes to the backend that
// owns the id (no other backend, no other tab), and the daemon's explicit
// "unknown tab id" surfaces as UnknownTabError with the RPC error preserved.
func TestPool_backendUnknownTabIsUnknownTabError(t *testing.T) {
	const idA, idB = "AAAA0000000000000000000000AB1234", "BBBB0000000000000000000000AB1234"
	west := &strictDaemon{next: idA}
	east := &strictDaemon{tabs: []string{idB}, next: "CCCC0000000000000000000000AB1234"}
	p, err := NewPool(NewClient(west.serve(t)), NewClient(east.serve(t)))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	created, err := p.TabNew(ctx, protocol.TabNewParams{})
	if err != nil {
		t.Fatal(err)
	}
	if created.Tab != "0:"+idA {
		t.Fatalf("created %q, want 0:%s", created.Tab, idA)
	}
	west.drop(idA) // closed directly on the daemon / by idle cleanup

	check := func(what string, err error) {
		t.Helper()
		var unknown *UnknownTabError
		if !errors.As(err, &unknown) {
			t.Fatalf("%s: err = %T %v, want errors.As(*UnknownTabError)", what, err, err)
		}
		if unknown.Tab != created.Tab {
			t.Errorf("%s: UnknownTabError.Tab = %q, want the pool id %q", what, unknown.Tab, created.Tab)
		}
		var rpcErr *RPCError
		if !errors.As(err, &rpcErr) || rpcErr.Code != protocol.CodeInvalidParams || rpcErr.Message != "unknown tab id" {
			t.Errorf("%s: underlying RPC error lost: %v", what, err)
		}
		var data protocol.ErrData
		if rpcErr == nil || rpcErr.UnmarshalData(&data) != nil || data.Error != "unknown tab id" {
			t.Errorf("%s: RPC error data lost", what)
		}
	}
	_, err = p.Eval(ctx, protocol.EvalParams{Tab: created.Tab, Script: "1"})
	check("Eval", err)
	check("Call without result", p.Call(ctx, protocol.MethodReload, protocol.ReloadParams{Tab: created.Tab}, nil))
	_, err = p.TabClose(ctx, protocol.TabCloseParams{Tab: created.Tab})
	check("TabClose", err)
	if len(east.evalTabs()) != 0 || len(west.evalTabs()) != 0 {
		t.Errorf("stale id reached a tab: west %v east %v", west.evalTabs(), east.evalTabs())
	}

	// Only that explicit answer is translated: any other error for a bound tab
	// is returned as it is.
	west.mu.Lock()
	west.tabs = []string{idA}
	west.fail = &protocol.ResponseError{Code: protocol.CodeServerError, Message: "eval failed"}
	west.mu.Unlock()
	_, err = p.Eval(ctx, protocol.EvalParams{Tab: created.Tab, Script: "1"})
	var unknown *UnknownTabError
	var rpcErr *RPCError
	if errors.As(err, &unknown) || !errors.As(err, &rpcErr) || rpcErr.Message != "eval failed" {
		t.Errorf("other RPC error = %T %v, want the plain RPCError", err, err)
	}
	west.mu.Lock()
	west.fail = &protocol.ResponseError{Code: protocol.CodeInvalidParams, Message: "missing script"}
	west.mu.Unlock()
	if _, err = p.Eval(ctx, protocol.EvalParams{Tab: created.Tab, Script: "1"}); errors.As(err, &unknown) {
		t.Errorf("an invalid-params error that is not about the tab became UnknownTabError: %v", err)
	}
}

// The pool-local UnknownTabError (id never bound) carries no backend error.
func TestUnknownTabErrorUnwrap(t *testing.T) {
	local := &UnknownTabError{Tab: "0:x"}
	if errors.Unwrap(local) != nil {
		t.Fatal("local UnknownTabError unwraps to something")
	}
	var nilErr *UnknownTabError
	if nilErr.Unwrap() != nil || nilErr.Error() == "" {
		t.Fatal("nil receiver")
	}
}
